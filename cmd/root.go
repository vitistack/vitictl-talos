// Package cmd wires the viti-talos plugin's cobra command tree.
//
// The binary is named viti-talos so that vitictl's plugin dispatcher exposes
// it as "viti talos ..."; it also runs standalone as "viti-talos ...".
// Dispatch is by binary name, so the "t" shorthand is a viti-t symlink
// alongside it rather than a cobra alias — see the Makefile.
package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/vitistack/vitictl/pkg/plugin/selfupgrade"
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

Single-cluster commands (dmesg, netstat, memory, get, dashboard, edit) take a
cluster name, or open an interactive fuzzy-searchable picker when you leave it
out. The fleet command — patch — takes many, and its picker marks them.

Installed as a viti plugin (a viti-talos binary on PATH) it is invoked as
"viti talos ..." — or "viti t ..." via the viti-t symlink.

Requires talosctl on PATH:
  https://www.talos.dev/latest/talos-guides/install/talosctl/`

// versionLong, upgradeShort and upgradeLong are the pre-migration wording from
// this repo's own cmd/version.go and cmd/upgrade.go (see git show
// 65469c7:cmd/version.go and 65469c7:cmd/upgrade.go), restored verbatim over
// the selfupgrade kit's generic text: with upgrade-node and upgrade-k8s also
// in the tree, "upgrade" needs to say plainly that it means the plugin, not
// Talos or Kubernetes, and "version" needs to point elsewhere for the Talos
// version a cluster is actually running.
const versionLong = `Print the installed viti-talos version.

With --check, also ask GitHub for the latest published release and report
whether this build is current. Being offline is not a failure of "version",
so a check that cannot complete says so and exits zero.

This is the plugin's own version. For the Talos version a cluster runs, see
"viti talos nodes <cluster>", and for what the nodes actually booted rather
than what their images declare, "viti talos dashboard <cluster>".`

const upgradeShort = "⬆️  Check for a newer viti-talos release and upgrade the plugin"

const upgradeLong = `Upgrades this plugin — not Talos, and not Kubernetes. Those are
"viti talos upgrade-node" and "viti talos upgrade-k8s".

Checks GitHub for the latest released version of the viti-talos plugin and, if
a newer release is available, prints the command that upgrades it.

viti-talos ships no installer of its own. It is a viti plugin, so upgrades go
through "viti plugin upgrade talos", which downloads the release, verifies its
SHA-256 checksum and (when cosign is installed) its Sigstore signature, and
replaces the binary atomically. Pass --run to have this command invoke that
for you.`

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
	)
	o := selfupgrade.Options{
		Name:    "talos",
		Repo:    "vitistack/vitictl-talos",
		Version: version,
	}
	ver := selfupgrade.NewVersionCmd(o)
	up := selfupgrade.NewUpgradeCmd(o)
	// The kit's generic wording loses the disambiguation this repo needs —
	// three upgrade* commands sit side by side in root --help.
	ver.Long = versionLong
	up.Short = upgradeShort
	up.Long = upgradeLong
	root.AddCommand(ver)
	root.AddCommand(up)
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

// confirm asks for a yes/no answer on the command's stdin.
//
// Non-interactive stdin is refused rather than assumed either way, so a piped
// or CI invocation never patches a fleet or replaces a binary without having
// been told to. --yes is the documented way through.
func confirm(cmd *cobra.Command, prompt string) (bool, error) {
	// Ask the terminal directly rather than inferring from the file mode:
	// /dev/null is a character device but is nobody's terminal, so a
	// mode-based check waves `... < /dev/null` through and then fails on the
	// read with a bare "EOF".
	if in, ok := cmd.InOrStdin().(*os.File); ok && !term.IsTerminal(int(in.Fd())) {
		return false, fmt.Errorf("stdin is not a terminal; re-run with --yes to confirm non-interactively")
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s [y/N]: ", prompt)

	// A final line without a trailing newline still counts as an answer.
	line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil && line == "" {
		return false, err
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}
