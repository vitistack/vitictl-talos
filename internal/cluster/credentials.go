package cluster

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

const (
	// ClusterIDLabel links a cluster's artifacts back to it, and is the
	// fallback lookup when the Secret is not named after the clusterId.
	ClusterIDLabel = "vitistack.io/clusterid"

	// KeyTalosconfig is the Secret entry holding the cluster's talosconfig:
	// the CA, the admin certificate and key, and the endpoints. It is what
	// makes every talosctl call in this plugin possible without a prior
	// "viti kc login".
	KeyTalosconfig = "talosconfig"

	// KeyKubeConfig is the Secret entry holding the guest cluster's
	// kubeconfig.
	KeyKubeConfig = "kube.config"
)

// FindSecret locates the Secret holding a cluster's config artifacts.
//
// It first tries a Secret named exactly <clusterId> in the cluster's
// namespace, then falls back to a label-selector search on
// vitistack.io/clusterid=<clusterId>, which covers the Talos case where a
// SECRET_PREFIX may have been configured on the operator.
func FindSecret(ctx context.Context, c Cluster) (*corev1.Secret, error) {
	id := c.ID()
	if id == "" {
		return nil, fmt.Errorf("cluster %s has no spec.data.clusterId", c.Describe())
	}

	var direct corev1.Secret
	err := c.AZ.Ctrl.Get(ctx, ctrlclient.ObjectKey{Namespace: c.Namespace(), Name: id}, &direct)
	if err == nil {
		return &direct, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("reading secret %s/%s: %w", c.Namespace(), id, err)
	}

	var list corev1.SecretList
	sel := labels.SelectorFromSet(labels.Set{ClusterIDLabel: id})
	if err := c.AZ.Ctrl.List(ctx, &list,
		ctrlclient.InNamespace(c.Namespace()),
		ctrlclient.MatchingLabelsSelector{Selector: sel}); err != nil {
		return nil, fmt.Errorf("listing secrets by label: %w", err)
	}
	switch len(list.Items) {
	case 0:
		return nil, fmt.Errorf(
			"no credentials secret found for clusterId %s in namespace %s (tried name=%s and label %s=%s)",
			id, c.Namespace(), id, ClusterIDLabel, id)
	case 1:
		return &list.Items[0], nil
	default:
		names := make([]string, 0, len(list.Items))
		for i := range list.Items {
			names = append(names, list.Items[i].Name)
		}
		sort.Strings(names)
		return nil, fmt.Errorf("multiple secrets match clusterId %s in namespace %s: %s",
			id, c.Namespace(), strings.Join(names, ", "))
	}
}

// WriteTempTalosconfig turns a cluster credentials secret into a
// self-contained talosconfig on disk, sized for a single talosctl invocation.
//
// It is deliberately not merged into the user's ~/.talos/config. This plugin
// drives ten or more clusters in one command; writing a context per cluster
// into the user's persistent config would leave their talosctl pointing at
// whichever one happened to run last.
//
// The returned cleanup removes the temp directory and the config within, and
// must be called once the child process has exited.
func WriteTempTalosconfig(secret *corev1.Secret, contextName string, endpoints []string) (path string, cleanup func(), err error) {
	raw, ok := secret.Data[KeyTalosconfig]
	if !ok || len(raw) == 0 {
		return "", nil, fmt.Errorf("secret %s/%s has no %q entry (not a Talos cluster?)",
			secret.Namespace, secret.Name, KeyTalosconfig)
	}

	dir, err := os.MkdirTemp("", "viti-talos-*")
	if err != nil {
		return "", nil, fmt.Errorf("creating temp dir: %w", err)
	}
	cleanup = func() { _ = os.RemoveAll(dir) }

	path = filepath.Join(dir, "talosconfig")
	if err := writeTalosconfigFile(raw, contextName, endpoints, path); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("writing temp talosconfig: %w", err)
	}
	return path, cleanup, nil
}

// talosConfig mirrors the subset of the talosconfig YAML structure that has to
// be understood to rename a context and replace its endpoints. Everything else
// is carried through verbatim.
type talosConfig struct {
	Context  string                    `json:"context,omitempty"`
	Contexts map[string]*talosCtxEntry `json:"contexts,omitempty"`
}

type talosCtxEntry struct {
	Endpoints []string `json:"endpoints,omitempty"`
	Nodes     []string `json:"nodes,omitempty"`
	CA        string   `json:"ca,omitempty"`
	Crt       string   `json:"crt,omitempty"`
	Key       string   `json:"key,omitempty"`
}

// writeTalosconfigFile renames the incoming talosconfig's sole context to
// contextName, overrides its endpoints when any are given, and writes the
// result to path with owner-only permissions — it holds an admin certificate
// and key for the cluster.
func writeTalosconfigFile(data []byte, contextName string, endpoints []string, path string) error {
	if len(data) == 0 {
		return fmt.Errorf("empty talosconfig")
	}
	if contextName == "" {
		return fmt.Errorf("context name is required")
	}
	var cfg talosConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("parsing talosconfig: %w", err)
	}
	src := sourceContext(&cfg)
	if src == nil {
		return fmt.Errorf("talosconfig has no usable context")
	}
	if len(endpoints) > 0 {
		src.Endpoints = dedupeKeepOrder(endpoints)
	}

	out := talosConfig{
		Context:  contextName,
		Contexts: map[string]*talosCtxEntry{contextName: src},
	}
	encoded, err := yaml.Marshal(out)
	if err != nil {
		return fmt.Errorf("marshalling talosconfig: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("creating output dir: %w", err)
	}
	return os.WriteFile(path, encoded, 0o600)
}

// sourceContext picks the context to carry over: the one the config names as
// current, or the first alphabetically so the choice is stable across runs.
func sourceContext(cfg *talosConfig) *talosCtxEntry {
	if cfg == nil || len(cfg.Contexts) == 0 {
		return nil
	}
	name := cfg.Context
	if name == "" || cfg.Contexts[name] == nil {
		names := make([]string, 0, len(cfg.Contexts))
		for k := range cfg.Contexts {
			names = append(names, k)
		}
		sort.Strings(names)
		name = names[0]
	}
	return cfg.Contexts[name]
}

// KeyInstallImage is the Secret entry holding the cluster's pinned Talos
// installer image.
//
// It is the desired state the talos-operator upgrades from: its resolution
// order is this pinned value first, a live-fetch from a running control plane
// second, and a provider default last. Changing a node's installer image
// without changing this therefore does not stick — the operator's next version
// enforcement resolves from the pin, swaps only the version tag, and puts the
// old schematic back.
const KeyInstallImage = "install_image" // #nosec G101 -- a Secret key name, not a credential

// PinnedInstallImage returns the installer image pinned in the cluster's
// credentials Secret, or "" when none is set.
func PinnedInstallImage(ctx context.Context, c Cluster) (string, error) {
	secret, err := FindSecret(ctx, c)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(secret.Data[KeyInstallImage])), nil
}

// PinInstallImage writes image into the cluster's credentials Secret, making it
// the installer the operator will drive future upgrades from.
//
// This is written before the nodes are touched, not after. The operator's job
// is to converge on desired state, so recording the intent first makes it an
// ally: an upgrade interrupted halfway leaves the operator finishing the rest
// rather than undoing what was done.
func PinInstallImage(ctx context.Context, c Cluster, image string) error {
	secret, err := FindSecret(ctx, c)
	if err != nil {
		return err
	}
	patched := secret.DeepCopy()
	if patched.Data == nil {
		patched.Data = map[string][]byte{}
	}
	patched.Data[KeyInstallImage] = []byte(image)
	if err := c.AZ.Ctrl.Update(ctx, patched); err != nil {
		return fmt.Errorf("pinning install image in secret %s/%s: %w",
			secret.Namespace, secret.Name, err)
	}
	return nil
}
