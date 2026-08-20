// Package cluster resolves what a Talos command needs to act on: which
// KubernetesClusters exist across the configured availability zones, which
// nodes each one is made of, and the credentials to reach them over the Talos
// API.
//
// Everything here reads the Vitistack management cluster. Nothing in this
// package speaks the Talos API itself — that is talosctl's job, and this
// package's output is exactly its input: a talosconfig path, a set of
// endpoints, and a set of node addresses.
package cluster

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"k8s.io/apimachinery/pkg/api/meta"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"

	"github.com/vitistack/vitictl-talos/internal/kube"
)

// ControlPlaneNameSuffix is the Machine-name fragment identifying a control
// plane node in the Vitistack naming scheme: <clusterId>-ctp<N>.
const ControlPlaneNameSuffix = "-ctp"

// Roles a node can hold. These are the values --role accepts.
const (
	RoleControlPlane = "controlplane"
	RoleWorker       = "worker"
)

// Cluster is one KubernetesCluster together with the zone it was found in.
type Cluster struct {
	AZ *kube.Client
	KC *vitiv1alpha1.KubernetesCluster
}

// The accessors below tolerate a zero-value Cluster. They are read from error
// messages as much as from the happy path, and an error path that panics
// replaces a diagnosable failure with an unreadable one.

// ID is the cluster's clusterId, which names both its Machines and the Secret
// holding its credentials.
func (c Cluster) ID() string {
	if c.KC == nil {
		return ""
	}
	return c.KC.Spec.Cluster.ClusterId
}

// Namespace is where the cluster and its Machines live.
func (c Cluster) Namespace() string {
	if c.KC == nil {
		return ""
	}
	return c.KC.Namespace
}

// Name is the KubernetesCluster's own name.
func (c Cluster) Name() string {
	if c.KC == nil {
		return ""
	}
	return c.KC.Name
}

// Zone is the availability zone the cluster was found in.
func (c Cluster) Zone() string {
	if c.AZ == nil {
		return ""
	}
	return c.AZ.AZ.Name
}

// IsTalos reports whether the cluster is provisioned by the Talos provider.
// Everything this plugin does goes through the Talos API, so a cluster that is
// not Talos is not addressable here at all.
func (c Cluster) IsTalos() bool {
	return c.KC != nil && c.KC.Spec.Cluster.Provider == vitiv1alpha1.KubernetesProviderTypeTalos
}

// Describe renders the cluster the way prompts, echoes and errors name it.
func (c Cluster) Describe() string {
	if c.KC == nil {
		return "<unresolved cluster>"
	}
	return c.Zone() + "/" + c.Namespace() + "/" + c.Name()
}

// List returns the clusters across the given zones, optionally narrowed to a
// namespace and to Talos clusters only.
//
// Zones are queried concurrently — wall clock is the slowest zone, not the sum
// — and a zone that cannot be listed is reported to warn and skipped, so one
// unreachable zone never hides the clusters in the others. A zone with no
// Vitistack CRDs at all is skipped silently; see isVitistackNotInstalled.
func List(ctx context.Context, clients []*kube.Client, namespace string, talosOnly bool, warn func(error)) []Cluster {
	perClient := make([][]Cluster, len(clients))
	errs := make([]error, len(clients))

	var wg sync.WaitGroup
	for idx, c := range clients {
		wg.Add(1)
		go func(idx int, c *kube.Client) {
			defer wg.Done()
			var list vitiv1alpha1.KubernetesClusterList
			var opts []ctrlclient.ListOption
			if namespace != "" {
				opts = append(opts, ctrlclient.InNamespace(namespace))
			}
			if err := c.Ctrl.List(ctx, &list, opts...); err != nil {
				errs[idx] = fmt.Errorf("availability zone %q: listing kubernetesclusters: %w", c.AZ.Name, err)
				return
			}
			hits := make([]Cluster, 0, len(list.Items))
			for i := range list.Items {
				hit := Cluster{AZ: c, KC: &list.Items[i]}
				if talosOnly && !hit.IsTalos() {
					continue
				}
				hits = append(hits, hit)
			}
			perClient[idx] = hits
		}(idx, c)
	}
	wg.Wait()

	// Warnings are emitted after the fan-in: warn writes to a shared stream
	// and is not assumed goroutine-safe.
	var out []Cluster
	for idx := range clients {
		if err := errs[idx]; err != nil {
			if warn != nil && !isVitistackNotInstalled(err) {
				warn(err)
			}
			continue
		}
		out = append(out, perClient[idx]...)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Describe() < out[j].Describe() })
	return out
}

// Find narrows a listing to the clusters matching name, which may be either
// the KubernetesCluster's name or its clusterId — the two differ often enough
// that insisting on one of them is just a way to be wrong half the time.
func Find(clusters []Cluster, name string) []Cluster {
	var out []Cluster
	for _, c := range clusters {
		if c.Name() == name || c.ID() == name {
			out = append(out, c)
		}
	}
	return out
}

// Ambiguous explains a name that matches several clusters, for when there is
// no terminal to resolve it interactively.
func Ambiguous(name string, matches []Cluster) error {
	where := make([]string, 0, len(matches))
	for _, c := range matches {
		where = append(where, c.Describe())
	}
	return fmt.Errorf(
		"%q is ambiguous — it matches %s; narrow it with --namespace or --availabilityzone, "+
			"or run in a terminal to pick one interactively",
		name, strings.Join(where, ", "))
}

// isVitistackNotInstalled reports whether a listing failed only because the
// cluster has no Vitistack CRDs.
//
// Clusters are configured as availability zones before the Vitistack operators
// land on them — a freshly bootstrapped management cluster is the usual case —
// and such a zone holds no KubernetesClusters by definition. Warning about it
// on every listing trains the reader to ignore the warnings that do matter, so
// it is skipped in silence.
func isVitistackNotInstalled(err error) bool {
	return meta.IsNoMatchError(err)
}
