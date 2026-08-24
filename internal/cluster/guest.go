package cluster

import (
	"context"
	"fmt"
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

// guestReadTimeout bounds the guest-cluster read.
//
// Without a bound this read defeats its own purpose. It is best-effort because
// a cluster whose Kubernetes API is unreachable is a large part of why anyone
// runs this command by hand — and an unbounded List against an unreachable API
// does not degrade, it hangs, turning a weakened column into a stalled upgrade.
// Fifteen seconds matches what vitictl allows its own version read.
const guestReadTimeout = 15 * time.Second

// RunningVersions returns the Talos version each of a cluster's nodes actually
// runs, keyed by the node's name.
//
// The source is the guest cluster's own Node objects —
// status.nodeInfo.osImage, "Talos (v1.13.7)", what the kubelet reports — which
// is the only per-node account of reality available from the management side.
// Everything else the management cluster holds is desired state: see the note
// on the parsers in version.go.
//
// It costs one List against the guest cluster, reached with the kubeconfig
// already sitting in the same Secret this package mints talosconfigs from. No
// Talos API call and no second credential.
//
// A node the guest cluster does not know about is absent from the result
// rather than present with an empty version, and the two must not be conflated
// by callers: "not upgraded" and "cannot tell" lead to opposite decisions.
func RunningVersions(ctx context.Context, c Cluster) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(ctx, guestReadTimeout)
	defer cancel()

	secret, err := FindSecret(ctx, c)
	if err != nil {
		return nil, err
	}
	raw, ok := secret.Data[KeyKubeConfig]
	if !ok || len(raw) == 0 {
		return nil, fmt.Errorf("secret %s/%s has no %q entry, so the nodes' running versions cannot be read",
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
	out := make(map[string]string, len(nodes.Items))
	for i := range nodes.Items {
		n := &nodes.Items[i]
		if v := VersionFromOSImage(n.Status.NodeInfo.OSImage); v != "" {
			out[n.Name] = v
		}
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
