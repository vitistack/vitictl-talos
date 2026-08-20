package cluster

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

const sampleTalosconfig = `context: original-name
contexts:
    original-name:
        endpoints:
            - 1.1.1.1
        nodes:
            - 2.2.2.2
        ca: Y2E=
        crt: Y3J0
        key: a2V5
`

func secretWith(data map[string][]byte) *corev1.Secret {
	s := &corev1.Secret{Data: data}
	s.Namespace = "vitistack"
	s.Name = "creds"
	return s
}

func TestWriteTempTalosconfigRenamesContextAndReplacesEndpoints(t *testing.T) {
	path, cleanup, err := WriteTempTalosconfig(
		secretWith(map[string][]byte{KeyTalosconfig: []byte(sampleTalosconfig)}),
		"my-cluster-id", []string{"10.0.0.1", "10.0.0.2", "10.0.0.1"})
	if err != nil {
		t.Fatalf("WriteTempTalosconfig() error: %v", err)
	}
	defer cleanup()

	raw, err := os.ReadFile(path) // #nosec G304 -- path is the temp file just written
	if err != nil {
		t.Fatalf("reading written config: %v", err)
	}
	var cfg talosConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("written config does not parse: %v", err)
	}
	if cfg.Context != "my-cluster-id" {
		t.Errorf("context = %q, want my-cluster-id", cfg.Context)
	}
	entry := cfg.Contexts["my-cluster-id"]
	if entry == nil {
		t.Fatalf("context was not renamed: %v", cfg.Contexts)
	}
	if len(entry.Endpoints) != 2 {
		t.Errorf("endpoints = %v, want the two given, deduped", entry.Endpoints)
	}
	// The PKI must survive the rewrite or nothing can authenticate.
	if entry.CA == "" || entry.Crt == "" || entry.Key == "" {
		t.Errorf("credentials were lost in the rewrite: %+v", entry)
	}
}

// The file holds an admin certificate and key for the whole cluster.
func TestWriteTempTalosconfigIsOwnerOnly(t *testing.T) {
	path, cleanup, err := WriteTempTalosconfig(
		secretWith(map[string][]byte{KeyTalosconfig: []byte(sampleTalosconfig)}), "c", nil)
	if err != nil {
		t.Fatalf("WriteTempTalosconfig() error: %v", err)
	}
	defer cleanup()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("talosconfig mode = %o, want 600", perm)
	}
}

func TestWriteTempTalosconfigCleanupRemovesEverything(t *testing.T) {
	path, cleanup, err := WriteTempTalosconfig(
		secretWith(map[string][]byte{KeyTalosconfig: []byte(sampleTalosconfig)}), "c", nil)
	if err != nil {
		t.Fatalf("WriteTempTalosconfig() error: %v", err)
	}
	cleanup()
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Errorf("temp dir survived cleanup: %v", err)
	}
}

// Keeping the original endpoints when none are given matters: it is the
// fallback when neither a CPVIP nor a machine address could be resolved.
func TestWriteTempTalosconfigKeepsEndpointsWhenNoneGiven(t *testing.T) {
	path, cleanup, err := WriteTempTalosconfig(
		secretWith(map[string][]byte{KeyTalosconfig: []byte(sampleTalosconfig)}), "c", nil)
	if err != nil {
		t.Fatalf("WriteTempTalosconfig() error: %v", err)
	}
	defer cleanup()

	raw, _ := os.ReadFile(path) // #nosec G304 -- path is the temp file just written
	var cfg talosConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("written config does not parse: %v", err)
	}
	if got := cfg.Contexts["c"].Endpoints; len(got) != 1 || got[0] != "1.1.1.1" {
		t.Errorf("endpoints = %v, want the original 1.1.1.1", got)
	}
}

func TestWriteTempTalosconfigRejectsANonTalosSecret(t *testing.T) {
	_, _, err := WriteTempTalosconfig(secretWith(map[string][]byte{KeyKubeConfig: []byte("x")}), "c", nil)
	if err == nil {
		t.Fatal("WriteTempTalosconfig() accepted a secret with no talosconfig")
	}
	if !strings.Contains(err.Error(), KeyTalosconfig) {
		t.Errorf("error does not name the missing key: %v", err)
	}
}

// A talosconfig whose current-context is missing still has to be usable; the
// first context alphabetically keeps the choice stable between runs.
func TestSourceContextFallsBackDeterministically(t *testing.T) {
	cfg := &talosConfig{Contexts: map[string]*talosCtxEntry{
		"zeta":  {CA: "z"},
		"alpha": {CA: "a"},
	}}
	got := sourceContext(cfg)
	if got == nil || got.CA != "a" {
		t.Errorf("sourceContext() = %+v, want the alpha context", got)
	}
	if sourceContext(&talosConfig{}) != nil {
		t.Error("sourceContext() invented a context for an empty config")
	}
}
