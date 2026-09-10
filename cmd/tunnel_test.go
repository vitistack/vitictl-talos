package cmd

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/vitistack/vitictl-talos/internal/cluster"
	"github.com/vitistack/vitictl-talos/internal/config"
)

// find locates a command in the tree. Task 7's cmd/config_test.go reuses it.
func find(t *testing.T, root *cobra.Command, name string) *cobra.Command {
	t.Helper()
	for _, c := range root.Commands() {
		if c.Name() == name {
			return c
		}
	}
	t.Fatalf("command %q is not in the tree", name)
	return nil
}

// -n is already the root's "limit to this namespace". Defining a second
// --namespace here would give one flag two meanings on one command, so the
// tunnel's own namespace flag has to be a different name.
func TestTunnelDoesNotRedefineNamespace(t *testing.T) {
	tun := find(t, NewRootCmd(), "tunnel")
	if tun.Flags().Lookup("tunnel-namespace") == nil {
		t.Error("--tunnel-namespace is not registered")
	}
	// LocalFlags excludes inherited persistent flags, so a hit here means the
	// command declared its own and shadowed the root's.
	if tun.LocalNonPersistentFlags().Lookup("namespace") != nil {
		t.Error("tunnel declares its own --namespace, shadowing the root's")
	}
}

func TestTunnelRegistersItsFlags(t *testing.T) {
	tun := find(t, NewRootCmd(), "tunnel")
	for _, name := range []string{"via-node", "tunnel-namespace", "image", "local-port", "timeout", "deadline", "node", "role"} {
		if tun.Flags().Lookup(name) == nil {
			t.Errorf("--%s is not registered", name)
		}
	}
}

// "portfwd" is what the muscle memory reaches for after a week of kubectl.
func TestTunnelIsAliasedAsPortfwd(t *testing.T) {
	tun := find(t, NewRootCmd(), "tunnel")
	for _, a := range tun.Aliases {
		if a == "portfwd" {
			return
		}
	}
	t.Errorf("aliases = %v, want portfwd among them", tun.Aliases)
}

// Everything after -- goes to talosctl as a subcommand plus its flags. Leading
// with a flag would build "talosctl --follow --talosconfig …", which fails
// with a message about the flag rather than about the mistake.
func TestTunnelRejectsPassthroughStartingWithAFlag(t *testing.T) {
	err := checkPassthrough([]string{"--follow"})
	if err == nil {
		t.Fatal("checkPassthrough() accepted a leading flag, want an error")
	}
	if !strings.Contains(err.Error(), "subcommand") {
		t.Errorf("error %q does not say what was expected", err)
	}
	if err := checkPassthrough([]string{"dmesg", "--follow"}); err != nil {
		t.Errorf("checkPassthrough() rejected a valid passthrough: %v", err)
	}
	if err := checkPassthrough(nil); err != nil {
		t.Errorf("checkPassthrough(nil) = %v, want nil", err)
	}
}

// The wrong talosconfig for the right cluster fails as "x509: certificate
// signed by unknown authority", which reads as a broken cluster. talosctl's
// output does not come back through this process, so the hint has to name
// both halves of the mismatch itself.
func TestCredentialHintNamesTheFileAndTheCluster(t *testing.T) {
	got := credentialHint(config.TunnelCluster{Talosconfig: "/a/b", Context: "admin@kv"})
	for _, want := range []string{"x509", "/a/b", "admin@kv"} {
		if !strings.Contains(got, want) {
			t.Errorf("credentialHint() = %q, does not mention %q", got, want)
		}
	}
}

// The printed line is meant to be pasted, so -e must be absent (the temp
// config carries it) and the node list must be the selected nodes.
func TestTunnelUsageLineIsPasteable(t *testing.T) {
	got := tunnelUsage("/tmp/x/talosconfig", []cluster.Node{
		{Name: "ctp01", IP: "100.64.0.4"},
		{Name: "ctp02", IP: "100.64.0.5"},
	})
	if !strings.Contains(got, "export TALOSCONFIG=/tmp/x/talosconfig") {
		t.Errorf("usage %q does not export TALOSCONFIG", got)
	}
	if !strings.Contains(got, "talosctl -n 100.64.0.4,100.64.0.5") {
		t.Errorf("usage %q does not list the selected nodes", got)
	}
	if strings.Contains(got, "-e ") {
		t.Errorf("usage %q still passes -e; the temp config carries the endpoint", got)
	}
}
