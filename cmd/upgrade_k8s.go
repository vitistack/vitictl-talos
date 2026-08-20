package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/vitistack/vitictl-talos/internal/cluster"
	"github.com/vitistack/vitictl-talos/internal/talosctl"
)

func newUpgradeK8sCmd(s *scope) *cobra.Command {
	n := &nodeSelector{}
	var (
		to     string
		from   string
		dryRun bool
		yes    bool
	)

	cmd := &cobra.Command{
		Use:     "upgrade-k8s [cluster] [-- talosctl flags]",
		Aliases: []string{"upgrade-kubernetes", "k8s-upgrade"},
		Short:   "⬆️  Upgrade Kubernetes on a Talos cluster",
		Long: `Upgrade the Kubernetes control plane and kubelets of one Talos cluster,
through "talosctl upgrade-k8s".

Unlike upgrade-node, this is a single cluster-wide operation rather than a
per-node one: talosctl is pointed at one control-plane node and drives the
whole upgrade from there — API server, controller manager, scheduler, proxy
and every kubelet — patching each node's machine config as it goes. That is
why this command targets one node and not a set.

It is deliberately a one-cluster-at-a-time command. A Kubernetes upgrade takes
minutes, rewrites every node's machine config, and is the kind of change worth
watching; "patch" is the command for doing one thing to a whole fleet.

Always run it once with --dry-run first. talosctl then prints the full upgrade
plan — every component, every node, the versions on each side — and changes
nothing.`,
		Example: `  # See the plan without changing anything.
  viti talos upgrade-k8s my-cluster --to 1.34.4 --dry-run

  # Run it.
  viti talos upgrade-k8s my-cluster --to 1.34.4

  # Pick the cluster and the control-plane node interactively.
  viti talos upgrade-k8s --to 1.34.4`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			name, passthrough, err := splitArgs(cmd, args)
			if err != nil {
				return err
			}
			if to == "" {
				return fmt.Errorf("pass --to <kubernetes version>, e.g. --to 1.34.4")
			}
			// Kubernetes upgrades are driven from a control plane: it is the
			// only node holding the static pod manifests being replaced.
			if n.role == "" {
				n.role = cluster.RoleControlPlane
			}
			sess, node, err := resolveSingleNode(cmd, s, n, name)
			if err != nil {
				return err
			}
			defer sess.Close()
			if !node.IsControlPlane() {
				return fmt.Errorf(
					"node %s is a worker — a Kubernetes upgrade must be driven from a control plane",
					node.Name)
			}

			target := talosctl.Target{
				Talosconfig: sess.Talosconfig,
				Endpoints:   sess.Endpoints(),
				Nodes:       []string{node.IP},
			}
			extra := []string{"--to", to}
			if from != "" {
				extra = append(extra, "--from", from)
			}
			if dryRun {
				extra = append(extra, "--dry-run")
			}
			extra = append(extra, passthrough...)

			declared := dash(sess.Cluster().KC.Spec.Topology.Version)
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
				"\n⬆️  Upgrading Kubernetes on %s to %s%s\n   driven from: %s (%s)\n   KubernetesCluster declares: %s\n\n",
				sess.Cluster().Describe(), to, dryRunSuffix(dryRun), node.Name, node.IP, declared)

			if !dryRun && !yes {
				ok, err := confirm(cmd, fmt.Sprintf("Upgrade Kubernetes on %s to %s?",
					sess.Cluster().Name(), to))
				if err != nil {
					return err
				}
				if !ok {
					_, _ = fmt.Fprintln(cmd.OutOrStdout(), "aborted")
					return errCancelled
				}
			}
			if err := talosctl.Run(contextOrBackground(cmd), streams(cmd), target, "upgrade-k8s", extra...); err != nil {
				return err
			}
			if dryRun {
				return nil
			}
			// The KubernetesCluster's own topology.version is what the
			// operators reconcile from, so an upgrade done here leaves the two
			// disagreeing until someone updates it. Saying so is cheaper than
			// the confusion of finding out later.
			if declared != "-" && declared != to {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
					"⚠️  %s still declares spec.topology.version %s — update it so the operators agree with the cluster\n",
					sess.Cluster().Name(), declared)
			}
			return nil
		},
	}
	n.register(cmd)
	cmd.Flags().StringVar(&to, "to", "", "Kubernetes version to upgrade to, e.g. 1.34.4")
	cmd.Flags().StringVar(&from, "from", "",
		"Kubernetes version to upgrade from (default: detected by talosctl)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false,
		"print the upgrade plan and change nothing")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	return cmd
}
