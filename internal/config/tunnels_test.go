package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sampleTalosYAML = `clusters:
    - context: admin@pos1-kv-cl01
      talosconfig: ~/.talos/kvosltalos
    - name: bgo-virt-001
      context: admin@bgo-virt-001
      kubeconfig: ~/.kube/config
      talosconfig: /etc/talos/bgo
      namespace: viti-tunnels
`

func writeTalosConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "talos.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvTalosConfig, path)
	return path
}

// The file sits beside vitictl's own config rather than at a fixed absolute
// path, so a VITI_CONFIG pointing at a scratch directory keeps the two
// together instead of silently reading the user's real credentials.
func TestTalosConfigPathSitsBesideTheVitistackConfig(t *testing.T) {
	t.Setenv(EnvTalosConfig, "")
	t.Setenv(EnvVitiConfig, "/somewhere/else/ctl.config.yaml")
	got, err := TalosConfigPath()
	if err != nil {
		t.Fatalf("TalosConfigPath() error: %v", err)
	}
	if got != "/somewhere/else/talos.yaml" {
		t.Errorf("TalosConfigPath() = %q, want it beside ctl.config.yaml", got)
	}
}

func TestTalosConfigPathHonoursItsOwnOverride(t *testing.T) {
	t.Setenv(EnvTalosConfig, "/tmp/custom.yaml")
	got, err := TalosConfigPath()
	if err != nil {
		t.Fatalf("TalosConfigPath() error: %v", err)
	}
	if got != "/tmp/custom.yaml" {
		t.Errorf("TalosConfigPath() = %q, want the override", got)
	}
}

// An entry naming only a context is the common case — repeating the context
// as the name for all six clusters would be noise nobody maintains.
func TestTunnelClustersDefaultsNameAndNamespace(t *testing.T) {
	writeTalosConfig(t, sampleTalosYAML)
	got, err := TunnelClusters()
	if err != nil {
		t.Fatalf("TunnelClusters() error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("TunnelClusters() returned %d entries, want 2", len(got))
	}
	if got[0].Name != "admin@pos1-kv-cl01" {
		t.Errorf("Name = %q, want it defaulted to the context", got[0].Name)
	}
	if got[0].Namespace != DefaultTunnelNamespace {
		t.Errorf("Namespace = %q, want %q", got[0].Namespace, DefaultTunnelNamespace)
	}
	if got[1].Name != "bgo-virt-001" {
		t.Errorf("Name = %q, want the explicit name kept", got[1].Name)
	}
	if got[1].Namespace != "viti-tunnels" {
		t.Errorf("Namespace = %q, want the explicit namespace kept", got[1].Namespace)
	}
}

// The three credential files really do live under ~, and a literal "~" passed
// to os.ReadFile fails with a message that blames the file rather than the
// path.
func TestTunnelClustersExpandsHomePaths(t *testing.T) {
	writeTalosConfig(t, sampleTalosYAML)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory in this environment")
	}
	got, err := TunnelClusters()
	if err != nil {
		t.Fatalf("TunnelClusters() error: %v", err)
	}
	want := filepath.Join(home, ".talos/kvosltalos")
	if got[0].Talosconfig != want {
		t.Errorf("Talosconfig = %q, want %q", got[0].Talosconfig, want)
	}
	wantKube := filepath.Join(home, ".kube/config")
	if got[1].Kubeconfig != wantKube {
		t.Errorf("Kubeconfig = %q, want %q", got[1].Kubeconfig, wantKube)
	}
}

func TestTunnelClustersRejectsIncompleteEntries(t *testing.T) {
	for name, content := range map[string]string{
		"no context":     "clusters:\n    - talosconfig: /a\n",
		"no talosconfig": "clusters:\n    - context: admin@a\n",
	} {
		t.Run(name, func(t *testing.T) {
			writeTalosConfig(t, content)
			_, err := TunnelClusters()
			if err == nil {
				t.Fatal("TunnelClusters() succeeded, want an error")
			}
			if !strings.Contains(err.Error(), "entry 1") {
				t.Errorf("error %q does not say which entry is wrong", err)
			}
		})
	}
}

// Two entries with the same name make "viti talos tunnel X" ambiguous, and
// silently picking one would act on the wrong cluster.
func TestTunnelClustersRejectsDuplicateNames(t *testing.T) {
	writeTalosConfig(t, "clusters:\n    - context: admin@a\n    - context: admin@a\n")
	_, err := TunnelClusters()
	if err == nil {
		t.Fatal("TunnelClusters() succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "admin@a") {
		t.Errorf("error %q does not name the duplicate", err)
	}
}

// Nobody has this file until they need a tunnel, so the error has to teach
// the format rather than just report a missing path.
func TestTunnelClustersExplainsAMissingFile(t *testing.T) {
	t.Setenv(EnvTalosConfig, filepath.Join(t.TempDir(), "absent.yaml"))
	_, err := TunnelClusters()
	if err == nil {
		t.Fatal("TunnelClusters() succeeded, want an error")
	}
	for _, want := range []string{"absent.yaml", "clusters:", "talosconfig:"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestFindTunnelClusterMatchesNameOrContext(t *testing.T) {
	in := []TunnelCluster{{Name: "kv", Context: "admin@kv"}}
	for _, q := range []string{"kv", "admin@kv"} {
		if _, ok := FindTunnelCluster(in, q); !ok {
			t.Errorf("FindTunnelCluster(%q) did not match", q)
		}
	}
	if _, ok := FindTunnelCluster(in, "nope"); ok {
		t.Error("FindTunnelCluster(\"nope\") matched, want no match")
	}
}
