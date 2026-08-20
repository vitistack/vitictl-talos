package cluster

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"

	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"
)

// EndpointSource names where a set of Talos endpoints came from. It is shown
// in the CLI output so an operator can tell at a glance which source was used
// and decide whether to override it.
type EndpointSource string

const (
	SourceCPVIP    EndpointSource = "controlplanevirtualsharedip"
	SourceMachines EndpointSource = "machines"
	SourceOverride EndpointSource = "override"
	SourceSecret   EndpointSource = "talosconfig (from secret)" // #nosec G101 -- display label, not a credential
	SourceNone     EndpointSource = "none"
)

// Node is one Talos machine of a cluster, as the Talos API addresses it.
type Node struct {
	// Name is the Machine's name (<clusterId>-ctp0, <clusterId>-wrk3, …),
	// which is what an operator types and reads.
	Name string
	// IP is the address talosctl is pointed at. Talos certificates carry the
	// node addresses in their SANs, so this must be an address the cluster
	// itself knows about — not a NAT or a proxy in front of it.
	IP string
	// Role is RoleControlPlane or RoleWorker.
	Role string
	// Phase is the Machine's own phase, useful for spotting a node that is
	// not going to answer before pointing talosctl at it.
	Phase string
	// TalosVersion is the declared version from the machine's factory image
	// URL, empty when it carries none. It is what the operator is driving
	// toward, which can run ahead of what the node actually boots.
	TalosVersion string
}

// IsControlPlane reports whether the node holds a control-plane role.
func (n Node) IsControlPlane() bool { return n.Role == RoleControlPlane }

// Topology is a cluster resolved into everything talosctl needs.
type Topology struct {
	Cluster Cluster
	// Endpoints are the Talos API addresses to connect through, always
	// control-plane addresses.
	Endpoints []string
	// Source records where Endpoints came from, for the "which address did
	// you actually use" question that follows any certificate error.
	Source EndpointSource
	// Nodes are every machine of the cluster, control planes first.
	Nodes []Node
	// Warnings are non-fatal problems worth surfacing: a CPVIP with no
	// status yet, a machine with no usable address, and so on.
	Warnings []string
}

// NodeIPs returns the addresses of the nodes matching role, which may be
// RoleControlPlane, RoleWorker, or "" for all of them.
func (t *Topology) NodeIPs(role string) []string {
	out := make([]string, 0, len(t.Nodes))
	for _, n := range t.Nodes {
		if role != "" && n.Role != role {
			continue
		}
		if n.IP == "" {
			continue
		}
		out = append(out, n.IP)
	}
	return out
}

// Select resolves the nodes a command should act on: those named in wanted
// (matched on machine name or address) filtered by role, or — when wanted is
// empty — every node of that role.
//
// Matching on either name or address matters because the two audiences differ:
// a human types "t-x-ctp0" from a listing, while a script pastes back the
// address it was given in JSON. Refusing one of them would make the flag
// useless to half its callers.
func (t *Topology) Select(wanted []string, role string) ([]Node, error) {
	byRole := make([]Node, 0, len(t.Nodes))
	for _, n := range t.Nodes {
		if role != "" && n.Role != role {
			continue
		}
		byRole = append(byRole, n)
	}
	if len(wanted) == 0 {
		return withAddress(byRole)
	}

	out := make([]Node, 0, len(wanted))
	var missing []string
	for _, w := range wanted {
		w = strings.TrimSpace(w)
		if w == "" {
			continue
		}
		found := false
		for _, n := range byRole {
			if n.Name == w || n.IP == w {
				out = append(out, n)
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, w)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("cluster %s has no node %s (have: %s)",
			t.Cluster.Describe(), strings.Join(missing, ", "), strings.Join(nodeNames(byRole), ", "))
	}
	return withAddress(out)
}

// withAddress drops nodes with no resolved address and fails when that leaves
// nothing — pointing talosctl at an empty node list silently targets whatever
// the talosconfig's own nodes field says, which is not what was asked for.
func withAddress(in []Node) ([]Node, error) {
	out := make([]Node, 0, len(in))
	var noAddr []string
	for _, n := range in {
		if n.IP == "" {
			noAddr = append(noAddr, n.Name)
			continue
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		if len(noAddr) > 0 {
			return nil, fmt.Errorf("no node has a usable address (%s have none reported)",
				strings.Join(noAddr, ", "))
		}
		return nil, fmt.Errorf("no nodes matched")
	}
	return out, nil
}

func nodeNames(nodes []Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.Name)
	}
	return out
}

// Resolve reads the cluster's machines and control-plane addresses, producing
// everything talosctl needs to be pointed at it.
//
// includeIPv6 defaults off in every caller because operator workstations often
// cannot reach v6 endpoints (no v6 transit, NAT64 not in path) and a single
// unreachable v6 entry hangs talosctl until its dial timeout fires.
func Resolve(ctx context.Context, c Cluster, includeIPv6 bool) (*Topology, error) {
	if c.ID() == "" {
		return nil, fmt.Errorf("cluster %s has no spec.data.clusterId", c.Describe())
	}

	var machines vitiv1alpha1.MachineList
	if err := c.AZ.Ctrl.List(ctx, &machines, ctrlclient.InNamespace(c.Namespace())); err != nil {
		return nil, fmt.Errorf("zone %q: listing machines: %w", c.Zone(), err)
	}

	t := &Topology{Cluster: c}
	prefix := c.ID() + "-"
	cpPrefix := c.ID() + ControlPlaneNameSuffix
	for i := range machines.Items {
		m := &machines.Items[i]
		if !strings.HasPrefix(m.Name, prefix) {
			continue
		}
		ips := filterIPFamily(MachineNodeIPs(m), includeIPv6)
		node := Node{
			Name:         m.Name,
			Role:         RoleWorker,
			Phase:        m.Status.Phase,
			TalosVersion: VersionFromImageID(m.Spec.OS.ImageID),
		}
		if strings.HasPrefix(m.Name, cpPrefix) {
			node.Role = RoleControlPlane
		}
		if len(ips) > 0 {
			node.IP = ips[0]
		} else {
			t.Warnings = append(t.Warnings,
				fmt.Sprintf("machine %s has no usable node IP", m.Name))
		}
		t.Nodes = append(t.Nodes, node)
	}
	if len(t.Nodes) == 0 {
		return nil, fmt.Errorf("no machines matched %s* in namespace %s — nothing to address",
			prefix, c.Namespace())
	}
	// Control planes first, then by name: a listing reads as the cluster's
	// shape, and any command that has to pick one node picks a control plane.
	sort.SliceStable(t.Nodes, func(i, j int) bool {
		if t.Nodes[i].IsControlPlane() != t.Nodes[j].IsControlPlane() {
			return t.Nodes[i].IsControlPlane()
		}
		return t.Nodes[i].Name < t.Nodes[j].Name
	})

	t.Endpoints, t.Source = resolveEndpoints(ctx, c, t, includeIPv6)
	return t, nil
}

// resolveEndpoints picks the Talos API addresses to connect through: the
// cluster's CPVIP pool members when it publishes one, otherwise the
// control-plane machines' own addresses.
//
// The VIP itself is deliberately not used. It adds a routing hop and is
// frequently unreachable from networks where the node addresses are, and a
// command that has already resolved the real control planes gains nothing by
// going through a load balancer to reach them.
func resolveEndpoints(ctx context.Context, c Cluster, t *Topology, includeIPv6 bool) ([]string, EndpointSource) {
	var list vitiv1alpha1.ControlPlaneVirtualSharedIPList
	if err := c.AZ.Ctrl.List(ctx, &list, ctrlclient.InNamespace(c.Namespace())); err != nil {
		t.Warnings = append(t.Warnings, fmt.Sprintf("CPVIP lookup: %v", err))
	} else {
		for i := range list.Items {
			cpvip := &list.Items[i]
			if cpvip.Spec.ClusterIdentifier != c.ID() {
				continue
			}
			if addrs := filterIPFamily(cpvip.Status.PoolMembers, includeIPv6); len(addrs) > 0 {
				return dedupeKeepOrder(addrs), SourceCPVIP
			}
			// Status empty — fall back to spec.poolMembers (desired state) so
			// a newly provisioned cluster is not unusable just because the
			// CPVIP controller has not reconciled yet.
			if addrs := filterIPFamily(cpvip.Spec.PoolMembers, includeIPv6); len(addrs) > 0 {
				t.Warnings = append(t.Warnings,
					fmt.Sprintf("CPVIP %s has no status addresses yet; using spec.poolMembers", cpvip.Name))
				return dedupeKeepOrder(addrs), SourceCPVIP
			}
			t.Warnings = append(t.Warnings, fmt.Sprintf("CPVIP %s has no addresses populated", cpvip.Name))
			break
		}
	}

	if addrs := t.NodeIPs(RoleControlPlane); len(addrs) > 0 {
		return dedupeKeepOrder(addrs), SourceMachines
	}
	return nil, SourceNone
}

// MachineNodeIPs returns the deduped node-level IP addresses of m.
//
// "Node-level" means addresses belonging to a real NIC — not pod CNI bridges,
// kube-proxy dummy interfaces, per-pod veth pairs, or libvirt/ovs bridges.
// Getting this wrong is not cosmetic: pointing talosctl at a cilium_host /32
// from the pod CIDR fails with "x509: certificate is valid for X, not Y",
// which reads as a broken PKI rather than as the wrong address.
//
// Resolution:
//  1. Status.NetworkInterfaces when populated, in one of two filtering modes:
//     a. KubeVirt: when at least one interface's Type carries the KubeVirt
//     "attached" marker (`domain` or `multus-status`, copied verbatim from
//     the VMI's infoSource), only those interfaces are kept. The others are
//     guest-agent reports of interfaces the guest OS happens to expose and
//     have no Name set, so the name-based filter cannot catch them.
//     b. Otherwise: drop interfaces whose name matches a known virtual
//     pattern and keep the rest.
//  2. Otherwise Status.IPAddresses, then Private, then Public.
//
// Loopback, link-local and unspecified addresses are dropped regardless of
// source. Same-order semantics: callers index [0] to pick a primary address.
func MachineNodeIPs(m *vitiv1alpha1.Machine) []string {
	if m == nil {
		return nil
	}
	var raw []string
	if len(m.Status.NetworkInterfaces) > 0 {
		kubevirtMode := hasKubevirtAttachedInterface(m.Status.NetworkInterfaces)
		for _, iface := range m.Status.NetworkInterfaces {
			if kubevirtMode {
				if !isKubevirtAttachedInterface(iface) {
					continue
				}
			} else if isVirtualInterfaceName(iface.Name) {
				continue
			}
			raw = append(raw, iface.IPAddresses...)
			raw = append(raw, iface.IPv6Addresses...)
		}
	}
	if len(raw) == 0 {
		switch {
		case len(m.Status.IPAddresses) > 0:
			raw = append(raw, m.Status.IPAddresses...)
		case len(m.Status.PrivateIPAddresses) > 0:
			raw = append(raw, m.Status.PrivateIPAddresses...)
		case len(m.Status.PublicIPAddresses) > 0:
			raw = append(raw, m.Status.PublicIPAddresses...)
		}
	}
	out := make([]string, 0, len(raw))
	for _, s := range raw {
		if !isUsableNodeIP(s) {
			continue
		}
		out = append(out, s)
	}
	return dedupeKeepOrder(out)
}

// isUsableNodeIP returns true for parseable, non-loopback, non-link-local,
// non-unspecified IP literals — the addresses worth dialling.
func isUsableNodeIP(s string) bool {
	ip := net.ParseIP(s)
	if ip == nil {
		return false
	}
	return !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !ip.IsUnspecified()
}

// filterIPFamily returns only the IPv4 entries from in when includeIPv6 is
// false, and in unchanged otherwise.
func filterIPFamily(in []string, includeIPv6 bool) []string {
	if includeIPv6 {
		return in
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		ip := net.ParseIP(s)
		if ip == nil || ip.To4() == nil {
			continue
		}
		out = append(out, s)
	}
	return out
}

// hasKubevirtAttachedInterface reports whether ifaces contains at least one
// entry carrying the KubeVirt "attached" marker.
func hasKubevirtAttachedInterface(ifaces []vitiv1alpha1.NetworkInterfaceStatus) bool {
	for i := range ifaces {
		if isKubevirtAttachedInterface(ifaces[i]) {
			return true
		}
	}
	return false
}

// isKubevirtAttachedInterface reports whether iface was actually attached by
// KubeVirt, as opposed to merely being visible to the qemu-guest-agent inside
// the guest.
//
// The kubevirt-operator copies vmi.Status.Interfaces[].InfoSource verbatim into
// NetworkInterfaceStatus.Type. `domain` means KubeVirt defined the NIC in the
// libvirt domain XML and `multus-status` means Multus reported a CNI
// attachment; `guest-agent`-only entries are the guest's own internal
// interfaces. Other providers set neither marker (proxmox-operator sets
// `ethernet`), so this filter only applies when at least one interface carries
// one of them.
func isKubevirtAttachedInterface(iface vitiv1alpha1.NetworkInterfaceStatus) bool {
	t := strings.ToLower(iface.Type)
	return strings.Contains(t, "domain") || strings.Contains(t, "multus-status")
}

// isVirtualInterfaceName matches well-known interfaces that never carry a
// node's primary address. Unmatched names are kept: including an unknown NIC
// is recoverable, dropping a real one is not.
func isVirtualInterfaceName(name string) bool {
	if name == "" {
		return false
	}
	n := strings.ToLower(name)
	switch n {
	case "lo", "cni0", "docker0", "kube-bridge", "kube-ipvs0", "kube-dummy-if",
		"tunl0", "ip6tnl0", "sit0", "gretap0", "erspan0":
		return true
	}
	for _, prefix := range []string{
		"flannel.",   // flannel.1, flannel.2 …
		"cilium_",    // cilium_host, cilium_net, cilium_vxlan
		"cilium-",    // cilium-health, cilium-tap
		"lxc",        // cilium per-pod lxcXXX
		"calico",     // calico*, calixxx
		"vxlan.cali", // calico vxlan device
		"weave",      // weave, weave-bridge
		"veth",       // per-pod veth pairs
		"br-",        // docker / containerd bridges
		"dummy",      // dummyN
		"ovs-",       // open vswitch
		"vnet",       // libvirt/QEMU host-side tap names
	} {
		if strings.HasPrefix(n, prefix) {
			return true
		}
	}
	return false
}

func dedupeKeepOrder(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
