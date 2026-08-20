package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/vitistack/vitictl-talos/internal/cluster"
	"github.com/vitistack/vitictl-talos/internal/talosctl"
)

func newUpgradeNodeCmd(s *scope) *cobra.Command {
	n := &nodeSelector{}
	var (
		to      string
		image   string
		yes     bool
		stage   bool
		noDrain bool
	)

	cmd := &cobra.Command{
		Use:     "upgrade-node [cluster] [-- talosctl flags]",
		Aliases: []string{"upgrade-talos", "node-upgrade"},
		Short:   "⬆️  Upgrade Talos on a cluster's nodes",
		Long: `Upgrade Talos itself on a cluster's nodes, through "talosctl upgrade".

Each node is upgraded one at a time and fully back before the next one starts.
That is not a tunable: a Talos upgrade reboots the node, and upgrading two
control planes at once costs the cluster its etcd quorum.

Which installer image is used matters more than anything else here, and this
command reads it from the node rather than constructing one. A Talos installer
image carries the Image Factory schematic — the cluster's system extensions —
and a platform variant baked into its name (nocloud-installer,
metal-installer, …). Upgrading onto a generic ghcr.io/siderolabs/installer
drops the extensions, which Talos rejects with a confusing "file exists"
error; upgrading onto the wrong platform variant silently changes the node's
talos.platform, and the node stays Ready while its config source quietly
breaks. So --to swaps only the version tag into each node's own current image,
and the resolved image is printed before anything happens.

--image overrides that outright, for the cases where the node's own image is
not the one you want. It is your responsibility to have matched the schematic
and the platform.

Note that the talos-operator also enforces a cluster's Talos version. Running
this by hand is for the cases the operator cannot resolve — an upgrade that
needs stepping by hand, or a node it has given up on.`,
		Example: `  # Upgrade every node of a cluster to v1.13.9, keeping each node's schematic.
  viti talos upgrade-node my-cluster --to v1.13.9

  # One node only, staged so it takes effect on the next reboot.
  viti talos upgrade-node my-cluster -N my-cluster-wrk0 --to v1.13.9 --stage

  # Workers first, with an explicit installer image.
  viti talos upgrade-node my-cluster --role worker --image factory.talos.dev/nocloud-installer/<schematic>:v1.13.9`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			name, passthrough, err := splitArgs(cmd, args)
			if err != nil {
				return err
			}
			if to == "" && image == "" {
				return fmt.Errorf("pass --to <version> (recommended) or --image <installer image>")
			}
			if to != "" && image != "" {
				return fmt.Errorf("--to and --image are mutually exclusive: --to rewrites each node's own image, --image replaces it")
			}

			sess, nodes, err := resolveNodes(cmd, s, n, name)
			if err != nil {
				return err
			}
			defer sess.Close()

			plan, err := planNodeUpgrades(cmd, sess, nodes, to, image)
			if err != nil {
				return err
			}
			printUpgradePlan(cmd, sess.Cluster(), plan, stage)

			if !yes {
				ok, err := confirm(cmd, fmt.Sprintf("Upgrade %d node(s) of %s, one at a time?",
					len(plan), sess.Cluster().Name()))
				if err != nil {
					return err
				}
				if !ok {
					_, _ = fmt.Fprintln(cmd.OutOrStdout(), "aborted")
					return errCancelled
				}
			}

			for _, step := range plan {
				target := talosctl.Target{
					Talosconfig: sess.Talosconfig,
					Endpoints:   sess.Endpoints(),
					Nodes:       []string{step.node.IP},
				}
				extra := []string{"--image", step.image}
				if stage {
					extra = append(extra, "--stage")
				}
				if noDrain {
					extra = append(extra, "--drain=false")
				}
				extra = append(extra, passthrough...)

				echo(cmd, fmt.Sprintf("upgrading %s (%s) → %s", step.node.Name, step.node.IP, step.image))
				if err := talosctl.Run(contextOrBackground(cmd), streams(cmd), target, "upgrade", extra...); err != nil {
					return fmt.Errorf("node %s: %w — remaining nodes were not touched", step.node.Name, err)
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "✅ %s upgraded\n", step.node.Name)
			}
			return nil
		},
	}
	n.register(cmd)
	cmd.Flags().StringVar(&to, "to", "",
		"Talos version to upgrade to; swapped into each node's own installer image")
	cmd.Flags().StringVar(&image, "image", "",
		"installer image to upgrade onto, replacing the node's own (must match its schematic and platform)")
	cmd.Flags().BoolVar(&stage, "stage", false,
		"stage the upgrade to run on the next reboot instead of rebooting now")
	cmd.Flags().BoolVar(&noDrain, "no-drain", false,
		"skip cordoning and draining the Kubernetes node before rebooting it")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	return cmd
}

// upgradeStep is one node and the image it will be upgraded onto.
type upgradeStep struct {
	node  cluster.Node
	image string
}

// planNodeUpgrades resolves the installer image for every node before any node
// is touched.
//
// Resolving up front is the point: a fleet of nodes that turn out to disagree
// about their schematic should be visible in the plan, not discovered halfway
// through a rolling reboot.
func planNodeUpgrades(cmd *cobra.Command, sess *cluster.Session, nodes []cluster.Node, to, image string) ([]upgradeStep, error) {
	ctx := contextOrBackground(cmd)
	steps := make([]upgradeStep, 0, len(nodes))
	for _, node := range nodes {
		if image != "" {
			steps = append(steps, upgradeStep{node: node, image: image})
			continue
		}
		current, err := talosctl.InstallImage(ctx, talosctl.Target{
			Talosconfig: sess.Talosconfig,
			Endpoints:   sess.Endpoints(),
			Nodes:       []string{node.IP},
		})
		if err != nil {
			return nil, err
		}
		steps = append(steps, upgradeStep{
			node:  node,
			image: talosctl.SwapImageTag(current, talosctl.NormalizeVersion(to)),
		})
	}
	return steps, nil
}

func printUpgradePlan(cmd *cobra.Command, c cluster.Cluster, plan []upgradeStep, stage bool) {
	w := cmd.ErrOrStderr()
	effect := "each node reboots into the new version as it is upgraded"
	if stage {
		effect = "staged only; each node takes the upgrade on its next reboot"
	}
	_, _ = fmt.Fprintf(w, "\n⬆️  Upgrading %d node(s) of %s, one at a time\n", len(plan), c.Describe())
	_, _ = fmt.Fprintf(w, "   effect: %s\n", effect)
	for _, step := range plan {
		from := dash(step.node.TalosVersion)
		_, _ = fmt.Fprintf(w, "   • %-28s %-16s declared v%s → %s\n",
			step.node.Name, step.node.Role, from, step.image)
	}
	_, _ = fmt.Fprintln(w)
}
