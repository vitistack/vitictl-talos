package cluster

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/clientcmd"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/vitistack/vitictl-talos/internal/kube"
)

// TalosVersionEnforcementCondition is the KubernetesCluster condition through
// which the talos-operator publishes the version it has verified every node
// runs.
const TalosVersionEnforcementCondition = "TalosVersionEnforcement"

// SchematicAnnotation is the Node annotation through which Talos publishes the
// Image Factory schematic a node is actually built from.
const SchematicAnnotation = "extensions.talos.dev/schematic"

// guestReadTimeout bounds the guest-cluster read.
//
// Without a bound this read defeats its own purpose. It is best-effort because
// a cluster whose Kubernetes API is unreachable is a large part of why anyone
// runs this command by hand — and an unbounded List against an unreachable API
// does not degrade, it hangs, turning a weakened column into a stalled upgrade.
// Fifteen seconds matches what vitictl allows its own version read.
const guestReadTimeout = 15 * time.Second

// NodeState is what a node reports about itself, as opposed to what anything
// in the management cluster wishes it were running.
type NodeState struct {
	// Version is the Talos version the kubelet reports, without its leading v.
	Version string
	// Schematic is the Image Factory schematic id the node is actually built
	// from, "" when the node does not publish one.
	Schematic string
}

// RunningState returns what each of a cluster's nodes actually runs, keyed by
// node name.
//
// Both halves come from the guest cluster's own Node objects, which is the only
// per-node account of reality available from the management side — everything
// the management cluster holds is desired state, and this cluster proved how
// far that can drift: two upgrades after v1.13.5, with the nodes on v1.13.9 and
// the pin on v1.13.9, machine.install.image still read v1.13.5.
//
//   - Version from status.nodeInfo.osImage, "Talos (v1.13.9)".
//   - Schematic from the extensions.talos.dev/schematic annotation. Talos lists
//     it in talos.dev/owned-annotations, so the node maintains it rather than it
//     being stamped once at provisioning time. Note it is an annotation: the
//     labels carry the individual extensions (iscsi-tools, qemu-guest-agent, …)
//     and never the schematic id, so a label lookup finds nothing and concludes
//     the wrong thing.
//
// One List, reached with the kubeconfig already sitting in the same Secret this
// package mints talosconfigs from. No Talos API call and no second credential.
//
// A node the guest cluster does not know about is absent from the result rather
// than present and empty, and callers must not conflate the two: "cannot tell"
// and "not upgraded" lead to opposite decisions.
func RunningState(ctx context.Context, c Cluster) (map[string]NodeState, error) {
	ctx, cancel := context.WithTimeout(ctx, guestReadTimeout)
	defer cancel()

	secret, err := FindSecret(ctx, c)
	if err != nil {
		return nil, err
	}
	raw, ok := secret.Data[KeyKubeConfig]
	if !ok || len(raw) == 0 {
		return nil, fmt.Errorf("secret %s/%s has no %q entry, so the nodes' running state cannot be read",
			secret.Namespace, secret.Name, KeyKubeConfig)
	}
	cfg, err := clientcmd.RESTConfigFromKubeConfig(raw)
	if err != nil {
		return nil, fmt.Errorf("parsing %s from secret %s/%s: %w",
			KeyKubeConfig, secret.Namespace, secret.Name, err)
	}
	// The context bounds the calls this function makes; cfg.Timeout also bounds
	// the discovery controller-runtime performs lazily inside the client, which
	// is not always reached through the context passed to List.
	cfg.Timeout = guestReadTimeout

	sch, err := kube.Scheme()
	if err != nil {
		return nil, err
	}
	cl, err := ctrlclient.New(cfg, ctrlclient.Options{Scheme: sch})
	if err != nil {
		return nil, fmt.Errorf("connecting to cluster %s: %w", c.Describe(), err)
	}

	var nodes corev1.NodeList
	if err := cl.List(ctx, &nodes); err != nil {
		return nil, fmt.Errorf("listing nodes of cluster %s: %w", c.Describe(), err)
	}
	out := make(map[string]NodeState, len(nodes.Items))
	for i := range nodes.Items {
		n := &nodes.Items[i]
		state := NodeState{
			Version:   VersionFromOSImage(n.Status.NodeInfo.OSImage),
			Schematic: strings.TrimSpace(n.Annotations[SchematicAnnotation]),
		}
		// A node reporting neither is not a Talos node this command can reason
		// about, and recording it as known-but-empty would let a caller read it
		// as "runs nothing".
		if state.Version == "" && state.Schematic == "" {
			continue
		}
		out[n.Name] = state
	}
	return out, nil
}

// EnforcedVersion returns the Talos version the operator has verified every
// node of the cluster runs, or "" when it has not said.
//
// Free — the condition is on the KubernetesCluster already in hand. It is the
// fallback for when the guest cluster cannot be reached, and a cross-check
// otherwise. Being cluster-wide, it can only ever say that all nodes agree; it
// says nothing during a rolling upgrade, which is exactly when they do not.
func EnforcedVersion(c Cluster) string {
	if c.KC == nil {
		return ""
	}
	for _, cond := range c.KC.Status.Conditions {
		if cond.Type == TalosVersionEnforcementCondition {
			return VersionFromEnforcement(cond.Message)
		}
	}
	return ""
}
