// Package tunnel reaches a Talos API that is not routable from this machine.
//
// It runs a socat pod inside the cluster's own Kubernetes cluster, forwarding
// to one control-plane node's Talos API, and brings that to localhost with a
// port-forward. apid on the endpoint proxies to any node named with -n, so one
// control plane is enough to reach all of them.
//
// The clusters this exists for — the Vitistack management clusters and the
// KubeVirt hypervisor clusters — are not KubernetesCluster resources, so
// nothing in the management cluster knows where they are. Their credentials
// are configured (see internal/config/tunnels.go); their topology is not, and
// is read from the Kubernetes API that is reachable.
package tunnel

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/vitistack/vitictl-talos/internal/cluster"
)

// ControlPlaneLabel marks a control-plane node.
//
// This is the only reliable signal here: ptr1-kv-cl01's control planes carry
// Talos' default hostnames (talos-np6-59s), so the -ctp<N> name convention
// cluster.Resolve can lean on does not hold.
const ControlPlaneLabel = "node-role.kubernetes.io/control-plane"

// Phases a node is reported in. These land in cluster.Node.Phase, which for a
// management-cluster-resolved node holds the Machine's phase instead — both
// answer "is this node going to respond".
const (
	PhaseReady    = "Ready"
	PhaseNotReady = "NotReady"
)

// talosOSImage matches what a Talos node reports as its OS image, e.g.
// "Talos (v1.13.6)". It is the only place a node's running Talos version is
// available over the Kubernetes API.
var talosOSImage = regexp.MustCompile(`^Talos \((.+)\)$`)

// NodesFrom turns a Kubernetes node listing into the nodes talosctl addresses,
// control planes first.
func NodesFrom(l *corev1.NodeList, includeIPv6 bool) []cluster.Node {
	if l == nil {
		return nil
	}
	out := make([]cluster.Node, 0, len(l.Items))
	for i := range l.Items {
		n := &l.Items[i]
		out = append(out, cluster.Node{
			Name:         n.Name,
			IP:           cluster.PickNodeIP(internalIPs(n), includeIPv6),
			Role:         roleOf(n),
			Phase:        readiness(n),
			TalosVersion: TalosVersionFromOSImage(n.Status.NodeInfo.OSImage),
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].IsControlPlane() != out[j].IsControlPlane() {
			return out[i].IsControlPlane()
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// TalosVersionFromOSImage extracts the version from a node's reported OS
// image, returning "" for anything that is not Talos.
func TalosVersionFromOSImage(osImage string) string {
	m := talosOSImage.FindStringSubmatch(strings.TrimSpace(osImage))
	if m == nil {
		return ""
	}
	return m[1]
}

// SelectVia picks the node the tunnel pod forwards to: the override when one
// is given, otherwise the first Ready control plane.
//
// It insists on Ready because the alternative failure is a connection refused
// inside a pod inside a port-forward, which says nothing about which of the
// three layers broke.
func SelectVia(nodes []cluster.Node, override string) (cluster.Node, error) {
	if override = strings.TrimSpace(override); override != "" {
		for _, n := range nodes {
			if n.Name != override && n.IP != override {
				continue
			}
			if n.IP == "" {
				return cluster.Node{}, fmt.Errorf(
					"node %s reports no usable address — the tunnel pod would have nothing to forward to", n.Name)
			}
			return n, nil
		}
		return cluster.Node{}, fmt.Errorf("no node %q here (have: %s)",
			override, strings.Join(describeNodes(nodes), ", "))
	}
	for _, n := range nodes {
		if n.IsControlPlane() && n.Phase == PhaseReady && n.IP != "" {
			return n, nil
		}
	}
	return cluster.Node{}, fmt.Errorf(
		"no Ready control-plane node with a usable address to forward through (have: %s) — "+
			"pass --via-node to use one anyway", strings.Join(describeNodes(nodes), ", "))
}

func internalIPs(n *corev1.Node) []string {
	out := make([]string, 0, len(n.Status.Addresses))
	for _, a := range n.Status.Addresses {
		if a.Type == corev1.NodeInternalIP {
			out = append(out, a.Address)
		}
	}
	return out
}

func roleOf(n *corev1.Node) string {
	if _, ok := n.Labels[ControlPlaneLabel]; ok {
		return cluster.RoleControlPlane
	}
	return cluster.RoleWorker
}

func readiness(n *corev1.Node) string {
	for _, c := range n.Status.Conditions {
		if c.Type != corev1.NodeReady {
			continue
		}
		if c.Status == corev1.ConditionTrue {
			return PhaseReady
		}
		return PhaseNotReady
	}
	return PhaseNotReady
}

// describeNodes renders the candidates for an error that has to answer "then
// what is there".
func describeNodes(nodes []cluster.Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		ip := n.IP
		if ip == "" {
			ip = "no address"
		}
		out = append(out, fmt.Sprintf("%s (%s, %s, %s)", n.Name, ip, n.Role, n.Phase))
	}
	return out
}
