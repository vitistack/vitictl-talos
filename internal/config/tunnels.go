package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	yaml "go.yaml.in/yaml/v3"
)

const (
	// EnvTalosConfig overrides the path to this plugin's own configuration,
	// for the same reason VITI_CONFIG exists: a scratch config must be
	// reachable without touching the real one.
	EnvTalosConfig = "VITI_TALOS_CONFIG"

	// TalosConfigFileName sits beside vitictl's ctl.config.yaml rather than
	// at a fixed absolute path, so the two move together.
	TalosConfigFileName = "talos.yaml"

	// DefaultTunnelNamespace is where a tunnel pod is created when an entry
	// does not say otherwise. It exists on every Vitistack cluster.
	DefaultTunnelNamespace = "viti-center"
)

// TunnelCluster is one Talos cluster this plugin can reach only through a pod
// in its own Kubernetes cluster.
//
// These are the clusters that are not KubernetesCluster resources — the
// Vitistack management clusters and the KubeVirt hypervisor clusters — so
// nothing in the management cluster can be asked where they are or how to
// authenticate to them. The kube context is their identity; their topology is
// still discovered live rather than configured.
type TunnelCluster struct {
	// Name is what "viti talos tunnel <name>" takes. It defaults to Context,
	// which is already unique and already what the operator types elsewhere.
	Name string `yaml:"name,omitempty"`
	// Context is the kubeconfig context to reach the cluster's Kubernetes API
	// through. Required — it is the cluster's identity here.
	Context string `yaml:"context"`
	// Kubeconfig is the file holding that context, defaulting to $KUBECONFIG
	// and ~/.kube/config the way kubectl does.
	Kubeconfig string `yaml:"kubeconfig,omitempty"`
	// Talosconfig is the file holding the cluster's Talos admin credentials.
	// Required, and the whole reason this file exists: it is the one thing
	// that cannot be discovered from anything already in reach.
	Talosconfig string `yaml:"talosconfig"`
	// Namespace is where the tunnel pod is created.
	Namespace string `yaml:"namespace,omitempty"`
}

// talosConfigFile is this plugin's configuration file.
type talosConfigFile struct {
	Clusters []TunnelCluster `yaml:"clusters"`
}

// exampleTalosConfig is appended to the not-found error. The format is not
// guessable and this file is met for the first time at the moment it is
// missing, so the error teaches it rather than merely reporting a path.
const exampleTalosConfig = `clusters:
  - context: admin@pos1-kv-cl01
    talosconfig: ~/.talos/kvosltalos`

// TalosConfigPath returns this plugin's own configuration file.
func TalosConfigPath() (string, error) {
	if p := os.Getenv(EnvTalosConfig); p != "" {
		return p, nil
	}
	base, err := VitistackConfigPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(base), TalosConfigFileName), nil
}

// TunnelClusters returns the configured tunnel clusters, defaulted and
// validated.
func TunnelClusters() ([]TunnelCluster, error) {
	path, err := TalosConfigPath()
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path) // #nosec G304 -- path is this plugin's own config location
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf(
				"no tunnel clusters configured — create %s:\n\n%s",
				path, exampleTalosConfig)
		}
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var file talosConfigFile
	if err := yaml.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if len(file.Clusters) == 0 {
		return nil, fmt.Errorf("no clusters listed in %s:\n\n%s", path, exampleTalosConfig)
	}

	seen := make(map[string]int, len(file.Clusters))
	out := make([]TunnelCluster, 0, len(file.Clusters))
	for i, c := range file.Clusters {
		c.Context = strings.TrimSpace(c.Context)
		if c.Context == "" {
			return nil, fmt.Errorf("%s: entry %d has no context", path, i+1)
		}
		c.Talosconfig = expandPath(c.Talosconfig)
		if c.Talosconfig == "" {
			return nil, fmt.Errorf("%s: entry %d (%s) has no talosconfig", path, i+1, c.Context)
		}
		c.Kubeconfig = expandPath(c.Kubeconfig)
		if strings.TrimSpace(c.Name) == "" {
			c.Name = c.Context
		}
		if strings.TrimSpace(c.Namespace) == "" {
			c.Namespace = DefaultTunnelNamespace
		}
		if first, dup := seen[c.Name]; dup {
			return nil, fmt.Errorf("%s: entries %d and %d are both named %q", path, first, i+1, c.Name)
		}
		seen[c.Name] = i + 1
		out = append(out, c)
	}
	return out, nil
}

// FindTunnelCluster narrows the configured clusters to the one named, matching
// either the entry's name or its context — the two are the same for a default
// entry, and insisting on one of them would be a way to be wrong half the time.
func FindTunnelCluster(in []TunnelCluster, name string) (TunnelCluster, bool) {
	for _, c := range in {
		if c.Name == name || c.Context == name {
			return c, true
		}
	}
	return TunnelCluster{}, false
}

// expandPath resolves the leading ~ and any environment variables in a
// configured path. The credential files really do live under ~, and a literal
// "~" reaching os.ReadFile fails with a message that blames the file rather
// than the path.
func expandPath(p string) string {
	p = os.ExpandEnv(strings.TrimSpace(p))
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(home, strings.TrimPrefix(p, "~"))
}
