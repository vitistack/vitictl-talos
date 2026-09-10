package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vitistack/vitictl-talos/internal/config"
)

// "viti talos config path" is where someone goes when a tunnel cannot find its
// credentials, so it has to name the file that holds them.
//
// Both of this plugin's own config paths are overridden to a scratch
// directory rather than left to resolve against the real ~/.vitistack: the
// plan's own constraints forbid a test touching the operator's real config,
// and leaving it unset would only pass by luck, whichever way the stat on the
// real file happens to go.
func TestConfigPathNamesTheTalosConfigFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(config.EnvVitiConfig, filepath.Join(dir, "ctl.config.yaml"))
	talosPath := filepath.Join(dir, "talos.yaml")
	t.Setenv(config.EnvTalosConfig, talosPath)

	root := NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"config", "path"})
	if err := root.Execute(); err != nil {
		t.Fatalf("config path: %v", err)
	}
	want := "tunnel clusters:   " + talosPath + " (not present)\n"
	if !strings.Contains(out.String(), want) {
		t.Errorf("config path output = %q, want it to contain %q", out.String(), want)
	}
}

// A missing talos.yaml is the normal state for the many people who never open
// a tunnel: it is reported, it names the path, and it must not make the
// command fail.
func TestConfigTestTreatsAMissingTalosConfigAsNoneConfigured(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(config.EnvVitiConfig, filepath.Join(dir, "ctl.config.yaml"))
	talosPath := filepath.Join(dir, "talos.yaml")
	t.Setenv(config.EnvTalosConfig, talosPath)

	var out bytes.Buffer
	var failures []string
	checkTunnelClusters(&out, func(label string, err error) {
		if err != nil {
			failures = append(failures, label+": "+err.Error())
		}
	})

	if len(failures) != 0 {
		t.Errorf("a missing talos.yaml was reported as a failure: %v", failures)
	}
	want := "➖ tunnel clusters: none configured (" + talosPath + ")\n"
	if out.String() != want {
		t.Errorf("output = %q, want %q", out.String(), want)
	}
}

// The opposite case, and the reason the two are told apart: this is the
// command someone runs when a tunnel will not open. Calling a broken file
// "none configured" tells them to create a file they have just broken, and
// exits 0 while doing it.
func TestConfigTestFailsOnATalosConfigThatIsThereAndWrong(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(config.EnvVitiConfig, filepath.Join(dir, "ctl.config.yaml"))
	talosPath := filepath.Join(dir, "talos.yaml")
	t.Setenv(config.EnvTalosConfig, talosPath)
	if err := os.WriteFile(talosPath, []byte("clusters:\n  - context: admin@a\n   talosconfig: /a\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	var failures []string
	checkTunnelClusters(&out, func(label string, err error) {
		if err != nil {
			failures = append(failures, label+": "+err.Error())
		}
	})

	if len(failures) != 1 {
		t.Fatalf("a broken talos.yaml produced %d failures, want 1 (output: %q)", len(failures), out.String())
	}
	if !strings.Contains(failures[0], talosPath) {
		t.Errorf("failure %q does not name the file", failures[0])
	}
	if strings.Contains(out.String(), "none configured") {
		t.Errorf("a broken talos.yaml was rendered as %q", out.String())
	}
}

// The long help said, in as many words, that this plugin has no config file of
// its own. It now has one, and the help is where that is discovered.
func TestConfigHelpDescribesTheTunnelConfig(t *testing.T) {
	cmd := find(t, NewRootCmd(), "config")
	if !strings.Contains(cmd.Long, "talos.yaml") {
		t.Errorf("config --help does not mention talos.yaml:\n%s", cmd.Long)
	}
}
