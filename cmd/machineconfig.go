package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/vitistack/vitictl-talos/internal/cluster"
	"github.com/vitistack/vitictl-talos/internal/talosctl"
)

// applyModes are talosctl's config-apply modes, in the order its own help
// lists them.
//
// They are worth knowing before running anything that writes:
//
//	auto       apply now, and reboot only if the change requires it
//	no-reboot  apply only what can take effect without a reboot; fail otherwise
//	reboot     apply and reboot unconditionally
//	staged     write the config to disk, to take effect on the next reboot
//	try        apply with a timeout and roll back automatically
var applyModes = []string{"auto", "no-reboot", "reboot", "staged", "try"}

// modeReboots reports whether a mode can take a node down. Those are the modes
// a fleet command must sequence one node at a time rather than fan out.
func modeReboots(mode string) bool {
	switch mode {
	case "no-reboot", "staged":
		return false
	default:
		// auto, reboot and try can all end in a restart — auto because it
		// decides per-change, try because a rollback reboots too.
		return true
	}
}

func parseMode(mode string) (string, error) {
	m := strings.ToLower(strings.TrimSpace(mode))
	if m == "" {
		return "auto", nil
	}
	for _, valid := range applyModes {
		if m == valid {
			return m, nil
		}
	}
	return "", fmt.Errorf("unknown apply mode %q (valid: %s)", mode, strings.Join(applyModes, ", "))
}

func newEditCmd(s *scope) *cobra.Command {
	n := &nodeSelector{}
	var mode string
	var dryRun bool

	cmd := &cobra.Command{
		Use:     "edit [cluster] [-- talosctl flags]",
		Aliases: []string{"edit-machineconfig", "editmc"},
		Short:   "📝 Edit one node's Talos machine configuration",
		Long: `Open one node's machine configuration in your editor and apply what you
save, through "talosctl edit machineconfig".

This addresses exactly one node — editing is a per-node conversation, and an
editor cannot be opened once for many. To change the same thing across a
cluster, or across the whole fleet, use "viti talos patch": it takes a patch
rather than an editor session, so it is reviewable, repeatable, and safe to
run against a hundred nodes.

The editor is talosctl's own choice: $TALOS_EDITOR, then $EDITOR, then vi.
Leave [cluster] out to pick one from an interactive list, and --node out to
pick the node from one too.

--mode decides when the change takes effect:
  auto       apply now, rebooting only if the change requires it (default)
  no-reboot  apply only what can take effect without a reboot; fail otherwise
  reboot     apply and reboot unconditionally
  staged     write to disk, to take effect on the next reboot
  try        apply with a timeout, rolling back automatically`,
		Example: `  viti talos edit
  viti talos edit my-cluster -N my-cluster-ctp0
  viti talos edit my-cluster -N my-cluster-wrk0 --mode staged
  viti talos edit my-cluster --dry-run`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			name, passthrough, err := splitArgs(cmd, args)
			if err != nil {
				return err
			}
			applyMode, err := parseMode(mode)
			if err != nil {
				return err
			}
			sess, node, err := resolveSingleNode(cmd, s, n, name)
			if err != nil {
				return err
			}
			defer sess.Close()

			target := talosctl.Target{
				Talosconfig: sess.Talosconfig,
				Endpoints:   sess.Endpoints(),
				Nodes:       []string{node.IP},
			}
			extra := []string{"--mode", applyMode}
			if dryRun {
				extra = append(extra, "--dry-run")
			}
			extra = append(extra, passthrough...)

			echo(cmd, fmt.Sprintf("%s — editing machine config of %s (%s), mode %s%s",
				sess.Cluster().Describe(), node.Name, node.IP, applyMode, dryRunSuffix(dryRun)))
			return talosctl.Run(contextOrBackground(cmd), streams(cmd), target, "edit machineconfig", extra...)
		},
	}
	n.register(cmd)
	cmd.Flags().StringVarP(&mode, "mode", "m", "auto",
		fmt.Sprintf("when the change takes effect: %s", strings.Join(applyModes, ", ")))
	cmd.Flags().BoolVar(&dryRun, "dry-run", false,
		"print the change summary instead of applying what you saved")
	return cmd
}

func newShowCmd(s *scope) *cobra.Command {
	n := &nodeSelector{}

	cmd := &cobra.Command{
		Use:     "show [cluster] [-- talosctl flags]",
		Aliases: []string{"machineconfig", "mc", "get-config"},
		Short:   "🔍 Print the machine configuration a node is running",
		Long: `Print the machine configuration currently in effect on a node, as
"talosctl get machineconfig -o yaml" reports it.

This is the other half of "patch": run it before to see what you are changing,
and after to confirm the change landed. Unlike the config stored in the
cluster's Secret, this is what the node itself is actually running, which is
the only version that settles an argument.

Every node is printed unless --node or --role narrows it, so a whole cluster's
configs can be diffed against each other in one go.`,
		Example: `  viti talos show my-cluster -N my-cluster-ctp0
  viti talos show my-cluster --role controlplane
  viti talos show my-cluster -N my-cluster-ctp0 > before.yaml`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			name, passthrough, err := splitArgs(cmd, args)
			if err != nil {
				return err
			}
			sess, nodes, err := resolveNodes(cmd, s, n, name)
			if err != nil {
				return err
			}
			defer sess.Close()

			target := talosctl.Target{
				Talosconfig: sess.Talosconfig,
				Endpoints:   sess.Endpoints(),
				Nodes:       nodeAddresses(nodes),
			}
			extra := append([]string{"-o", "yaml"}, passthrough...)
			echo(cmd, fmt.Sprintf("%s — machine config of %s",
				sess.Cluster().Describe(), strings.Join(nodeNames(nodes), ", ")))
			return talosctl.Run(contextOrBackground(cmd), streams(cmd), target, "get machineconfig", extra...)
		},
	}
	n.register(cmd)
	return cmd
}

// resolveSingleNode resolves the one node a command addresses, picking it
// interactively when --node was not given.
func resolveSingleNode(cmd *cobra.Command, s *scope, n *nodeSelector, name string) (*cluster.Session, cluster.Node, error) {
	role, err := parseRole(n.role)
	if err != nil {
		return nil, cluster.Node{}, err
	}
	c, err := selectCluster(cmd, s, name)
	if err != nil {
		return nil, cluster.Node{}, err
	}
	sess, err := openSession(cmd, s, c)
	if err != nil {
		return nil, cluster.Node{}, err
	}
	if len(n.nodes) == 0 {
		node, err := pickNode(cmd, sess.Topology)
		if err != nil {
			sess.Close()
			return nil, cluster.Node{}, err
		}
		return sess, node, nil
	}
	nodes, err := sess.Topology.Select(n.nodes, role)
	if err != nil {
		sess.Close()
		return nil, cluster.Node{}, err
	}
	if len(nodes) > 1 {
		sess.Close()
		return nil, cluster.Node{}, fmt.Errorf(
			"this command addresses one node, but %d were given (%s)",
			len(nodes), strings.Join(nodeNames(nodes), ", "))
	}
	return sess, nodes[0], nil
}

func dryRunSuffix(dryRun bool) string {
	if dryRun {
		return " (dry run)"
	}
	return ""
}
