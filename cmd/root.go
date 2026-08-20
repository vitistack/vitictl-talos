// Package cmd wires the viti-talos plugin's cobra command tree.
//
// The binary is named viti-talos so that vitictl's plugin dispatcher exposes
// it as "viti talos ..."; it also runs standalone as "viti-talos ...".
// Dispatch is by binary name, so the "t" shorthand is a viti-t symlink
// alongside it rather than a cobra alias — see the Makefile.
package cmd

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"
)

// version is set by main from the -ldflags-injected build version.
var version = "dev"

const rootLong = `🐢 viti-talos adds Talos Linux commands to the viti CLI.

It finds the Talos KubernetesClusters in viti's availability zones, works out
each one's control-plane endpoints and node addresses, mints a throwaway
talosconfig from the cluster's own credentials Secret, and runs talosctl
against it. No "viti kc login" first, and nothing written to your
~/.talos/config — a command that spans ten clusters must not leave your
talosctl pointing at whichever one ran last.

Single-cluster commands (dmesg, netstat, memory, dashboard, edit) take a
cluster name, or open an interactive fuzzy-searchable picker when you leave it
out. The fleet command — patch — takes many, and its picker marks them.

Installed as a viti plugin (a viti-talos binary on PATH) it is invoked as
"viti talos ..." — or "viti t ..." via the viti-t symlink.

Requires talosctl on PATH:
  https://www.talos.dev/latest/talos-guides/install/talosctl/`

// NewRootCmd builds a fresh command tree. Tests construct their own instance
// so flag state is never shared between runs.
func NewRootCmd() *cobra.Command {
	s := &scope{}

	root := &cobra.Command{
		Use:           "viti-talos",
		Short:         "🐢 Talos Linux commands for viti",
		Long:          rootLong,
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetVersionTemplate("viti-talos version {{.Version}}\n")
	s.register(root)

	root.AddCommand(
		newClustersCmd(s),
		newNodesCmd(s),
		newDashboardCmd(s),
		newEditCmd(s),
		newShowCmd(s),
		newPatchCmd(s),
		newUpgradeNodeCmd(s),
		newUpgradeK8sCmd(s),
		newConfigCmd(s),
		newVersionCmd(),
		newUpgradeCmd(),
	)
	for _, r := range readOnlyCommands() {
		root.AddCommand(newReadOnlyCmd(s, r))
	}
	return root
}

// SetVersion wires the build version in before the tree is constructed.
func SetVersion(v string) {
	if v != "" {
		version = v
	}
}

// Execute runs the plugin, printing errors in viti's style.
func Execute() error {
	root := NewRootCmd()
	if err := root.Execute(); err != nil {
		if errors.Is(err, errCancelled) {
			return err
		}
		_, _ = fmt.Fprintln(root.ErrOrStderr(), "❌ Error:", err)
		return err
	}
	return nil
}

// errCancelled unwinds a declined confirmation or a dismissed picker without
// printing an error: cancelling is a decision, not a failure.
var errCancelled = errors.New("cancelled")

// warn reports a non-fatal problem on stderr, so structured stdout stays
// pipeable even when one availability zone is unreachable.
func warn(cmd *cobra.Command, err error) {
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "⚠️  %v\n", err)
}

// echo reports a resolved choice on stderr, so stdout stays pipeable.
func echo(cmd *cobra.Command, what string) {
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "▶ %s\n", what)
}

// contextOrBackground keeps helpers usable from tests that build a bare
// command without calling Execute.
func contextOrBackground(cmd *cobra.Command) context.Context {
	if ctx := cmd.Context(); ctx != nil {
		return ctx
	}
	return context.Background()
}

// firstArg returns the cluster name a command was given, or "" when it was
// left out so one can be picked interactively.
func firstArg(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}
