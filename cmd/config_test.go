package cmd

import (
	"bytes"
	"strings"
	"testing"
)

// "viti talos config path" is where someone goes when a tunnel cannot find its
// credentials, so it has to name the file that holds them.
func TestConfigPathNamesTheTalosConfigFile(t *testing.T) {
	root := NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"config", "path"})
	if err := root.Execute(); err != nil {
		t.Fatalf("config path: %v", err)
	}
	if !strings.Contains(out.String(), "talos.yaml") {
		t.Errorf("config path output does not mention talos.yaml:\n%s", out.String())
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
