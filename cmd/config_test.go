package cmd

import (
	"bytes"
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

// The long help said, in as many words, that this plugin has no config file of
// its own. It now has one, and the help is where that is discovered.
func TestConfigHelpDescribesTheTunnelConfig(t *testing.T) {
	cmd := find(t, NewRootCmd(), "config")
	if !strings.Contains(cmd.Long, "talos.yaml") {
		t.Errorf("config --help does not mention talos.yaml:\n%s", cmd.Long)
	}
}
