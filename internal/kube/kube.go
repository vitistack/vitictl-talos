// Package kube builds Kubernetes clients for the Vitistack availability zones
// this plugin reads from.
//
// Only one kind of cluster is contacted through the Kubernetes API: the
// management cluster holding KubernetesCluster, Machine and the credentials
// Secret. The Talos clusters themselves are reached over the Talos API, by
// talosctl, using a talosconfig minted from that Secret.
package kube

import (
	"context"
	"fmt"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"

	"github.com/vitistack/vitictl-talos/internal/config"
)

// Client is a connection to one Vitistack availability zone.
type Client struct {
	AZ   config.AvailabilityZone
	Ctrl ctrlclient.Client
}

// ConnectAll builds clients for the given availability zones. A zone that
// cannot be reached is reported to warn and skipped, so one unreachable zone
// does not hide the clusters in every other one.
func ConnectAll(_ context.Context, zones []config.AvailabilityZone, warn func(error)) ([]*Client, error) {
	sch, err := Scheme()
	if err != nil {
		return nil, err
	}

	out := make([]*Client, 0, len(zones))
	var firstErr error
	for _, z := range zones {
		cfg, err := RESTConfig(z.Kubeconfig, z.Context)
		if err == nil {
			var c ctrlclient.Client
			c, err = ctrlclient.New(cfg, ctrlclient.Options{Scheme: sch})
			if err == nil {
				out = append(out, &Client{AZ: z, Ctrl: c})
				continue
			}
		}
		if firstErr == nil {
			firstErr = err
		}
		if warn != nil {
			warn(fmt.Errorf("availability zone %q unreachable: %w", z.Name, err))
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf(
			"cannot reach any Vitistack availability zone (0/%d). "+
				"This is a kubeconfig or network problem, not a Talos one. First error: %v",
			len(zones), firstErr)
	}
	return out, nil
}

// Scheme returns a scheme carrying the core types plus Vitistack's, which is
// everything the management-cluster reads need: KubernetesCluster, Machine,
// ControlPlaneVirtualSharedIP and the credentials Secret.
func Scheme() (*runtime.Scheme, error) {
	sch := runtime.NewScheme()
	if err := scheme.AddToScheme(sch); err != nil {
		return nil, fmt.Errorf("adding core scheme: %w", err)
	}
	if err := vitiv1alpha1.AddToScheme(sch); err != nil {
		return nil, fmt.Errorf("adding vitistack scheme: %w", err)
	}
	return sch, nil
}

// RESTConfig loads a kubeconfig, honouring an explicit path and context and
// otherwise falling back to $KUBECONFIG and ~/.kube/config.
func RESTConfig(kubeconfig, kubecontext string) (*rest.Config, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		if _, err := os.Stat(kubeconfig); err != nil {
			return nil, fmt.Errorf("kubeconfig %q is not readable: %w", kubeconfig, err)
		}
		rules.ExplicitPath = kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{}
	if kubecontext != "" {
		overrides.CurrentContext = kubecontext
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("loading kube client config: %w", err)
	}
	return cfg, nil
}
