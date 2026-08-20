package cmd

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/vitistack/vitictl-talos/internal/cluster"
	"github.com/vitistack/vitictl-talos/internal/talosctl"
)

func newUpgradeNodeCmd(s *scope) *cobra.Command {
	n := &nodeSelector{}
	var (
		to        string
		image     string
		yes       bool
		stage     bool
		noDrain   bool
		dryRun    bool
		schematic string
		platform  string
		registry  string
		noPin     bool
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

Only the version tag is replaced. The registry host, the repository path, the
platform variant and the schematic digest all come from the node and are
carried over untouched, so a private mirror, a custom factory schematic and a
platform-specific installer all survive the upgrade.

An Image Factory reference has four independently editable parts, and each has
its own flag. Anything you do not name is carried over from the node:

  <registry>          / <platform>-installer / <schematic> : <version>
  --registry            --platform             --schematic   --to

  --registry   where the image is pulled from — a local mirror, an air-gapped copy
  --platform   which platform variant, e.g. metal, nocloud, hcloud
  --schematic  which system extensions and kernel arguments are baked in
  --to         which Talos release

They combine, so moving a cluster to a different provider, adding extensions
and bumping the version is one rolling pass rather than three. --image still
replaces the whole reference when the parts are not what you want to think in.

Build a new schematic at the Image Factory first — what you pass is the id it
hands back, not a file:

  curl -X POST --data-binary @schematic.yaml https://factory.talos.dev/schematics

--platform is the one to be careful with. The platform baked into the installer
decides where Talos reads its machine config and network settings on the next
boot, and a wrong one does not fail loudly: the node comes back up Ready with
its config source silently gone. Each node's currently-running platform is read
and shown against the target whenever this changes, so the switch is visible
before it happens rather than days later.

The registry, platform and schematic are positional parts of a factory
reference, so editing them requires one. A plain ghcr.io/siderolabs/installer
is refused rather than rewritten — rewriting one of its segments would produce
an image that does not exist, failing as a pull error part way through a
rolling upgrade instead of as a mistake in the command. Only --to works on any
reference.

Either way the cluster's pinned install_image is updated to match, in the
credentials Secret, before any node is touched. That pin is the talos-operator's
desired state and it wins over what the nodes report: change an image without
it and the operator's next version enforcement resolves from the old pin, swaps
only the version tag, and puts the old schematic back. Recording the intent
first also means an upgrade interrupted halfway leaves the operator finishing
the job rather than undoing it. --no-pin opts out, and says so in the plan.

Because the image is resolved per node, a cluster whose nodes disagree — part
way through an earlier upgrade, say — keeps each node on its own lineage
rather than being forced onto one image. Run with --dry-run first to see every
node's resolved image before anything is touched.

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

  # See exactly which image each node would get, changing nothing.
  viti talos upgrade-node my-cluster --to v1.13.9 --dry-run

  # Add system extensions by moving to a new schematic, same Talos version.
  viti talos upgrade-node my-cluster --schematic <64-hex-id> --dry-run

  # Change extensions and version in one pass.
  viti talos upgrade-node my-cluster --schematic <64-hex-id> --to v1.13.9

  # Workers first, with an explicit installer image.
  viti talos upgrade-node my-cluster --role worker --image factory.talos.dev/nocloud-installer/<schematic>:v1.13.9`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			name, passthrough, err := splitArgs(cmd, args)
			if err != nil {
				return err
			}
			edit := talosctl.ImageEdit{
				Registry:  registry,
				Platform:  platform,
				Schematic: schematic,
				Version:   to,
			}
			if edit.Empty() && image == "" {
				return fmt.Errorf(
					"pass --to, --schematic, --platform or --registry to change part of each node's own " +
						"installer image, or --image to replace it outright")
			}
			if image != "" && !edit.Empty() {
				return fmt.Errorf(
					"--image replaces the installer outright, so it cannot be combined with " +
						"--to, --schematic, --platform or --registry")
			}

			sess, nodes, err := resolveNodes(cmd, s, n, name)
			if err != nil {
				return err
			}
			defer sess.Close()

			plan, err := planNodeUpgrades(cmd, sess, nodes, edit, image)
			if err != nil {
				return err
			}
			pin, err := pinTarget(cmd, sess.Cluster(), plan, noPin)
			if err != nil {
				return err
			}
			printUpgradePlan(cmd, sess.Cluster(), plan, stage, pin)
			if dryRun {
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(),
					"🔎 dry run — nothing was upgraded and nothing was pinned. Every image above was "+
						"derived from that node's own machine.install.image.")
				return nil
			}

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

			// Desired state is recorded before the nodes are touched, so an
			// upgrade interrupted halfway leaves the operator finishing the
			// job rather than undoing it.
			if pin.needed() {
				if err := cluster.PinInstallImage(contextOrBackground(cmd), sess.Cluster(), pin.want); err != nil {
					return fmt.Errorf("%w — no node was upgraded", err)
				}
				echo(cmd, "pinned install_image = "+pin.want)
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
	cmd.Flags().StringVar(&schematic, "schematic", "",
		"Image Factory schematic id to switch to (the system extensions and kernel arguments)")
	cmd.Flags().StringVar(&platform, "platform", "",
		"platform variant to switch to, e.g. hcloud or metal — changes where Talos reads its config on the next boot")
	cmd.Flags().StringVar(&registry, "registry", "",
		"registry to pull the installer from, e.g. a local mirror")
	cmd.Flags().BoolVar(&noPin, "no-pin", false,
		"do not update the cluster's pinned install_image (the operator will revert the change)")
	cmd.Flags().BoolVar(&stage, "stage", false,
		"stage the upgrade to run on the next reboot instead of rebooting now")
	cmd.Flags().BoolVar(&noDrain, "no-drain", false,
		"skip cordoning and draining the Kubernetes node before rebooting it")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false,
		"resolve and print the per-node installer images, then stop without upgrading")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	return cmd
}

// upgradeStep is one node, the image it will be upgraded onto, and the image
// it is on now.
type upgradeStep struct {
	node  cluster.Node
	image string
	from  string
	// platform is what the node reports running as, read only when the
	// platform is about to change.
	platform string
}

// planNodeUpgrades resolves the installer image for every node before any node
// is touched.
//
// Resolving up front is the point: a fleet of nodes that turn out to disagree
// about their schematic should be visible in the plan, not discovered halfway
// through a rolling reboot.
func planNodeUpgrades(cmd *cobra.Command, sess *cluster.Session, nodes []cluster.Node, edit talosctl.ImageEdit, image string) ([]upgradeStep, error) {
	ctx := contextOrBackground(cmd)
	steps := make([]upgradeStep, 0, len(nodes))
	for _, node := range nodes {
		target := talosctl.Target{
			Talosconfig: sess.Talosconfig,
			Endpoints:   sess.Endpoints(),
			Nodes:       []string{node.IP},
		}
		step := upgradeStep{node: node}
		// Only read the running platform when it is about to change: it costs
		// a round trip per node and says nothing useful otherwise.
		if edit.Platform != "" || image != "" {
			step.platform = talosctl.NodePlatform(ctx, target)
		}

		if image != "" {
			step.image = image
			steps = append(steps, step)
			continue
		}
		current, err := talosctl.InstallImage(ctx, target)
		if err != nil {
			return nil, err
		}
		next, err := talosctl.RewriteImage(current, edit)
		if err != nil {
			return nil, fmt.Errorf("node %s: %w", node.Name, err)
		}
		step.from, step.image = current, next
		steps = append(steps, step)
	}
	return steps, nil
}

// pin is what the cluster's pinned install_image should become, and what it is
// now.
type pin struct {
	have string
	want string
	// skipped records that pinning was suppressed, so the plan can say the
	// operator will revert the change rather than staying silent about it.
	skipped bool
}

func (p pin) needed() bool { return !p.skipped && p.want != "" && p.want != p.have }

// pinTarget works out what the cluster's pinned installer image should become.
//
// The pin is the operator's desired state and it wins over what any node
// reports, so an upgrade that changes the image without changing the pin is
// undone by the next reconcile. Refusing to guess when the nodes disagree
// matters for the same reason: pinning one of several images would quietly
// converge the rest onto it.
func pinTarget(cmd *cobra.Command, c cluster.Cluster, plan []upgradeStep, noPin bool) (pin, error) {
	p := pin{skipped: noPin}

	images := map[string]struct{}{}
	for _, step := range plan {
		images[step.image] = struct{}{}
	}
	if len(images) == 1 {
		for img := range images {
			p.want = img
		}
	}

	have, err := cluster.PinnedInstallImage(contextOrBackground(cmd), c)
	if err != nil {
		return pin{}, err
	}
	p.have = have

	if !noPin && p.want == "" && len(images) > 1 {
		return pin{}, fmt.Errorf(
			"the targeted nodes would end up on %d different installer images, so there is no single "+
				"image to pin for the cluster — narrow the run, or pass --no-pin and set install_image yourself",
			len(images))
	}
	return p, nil
}

func printUpgradePlan(cmd *cobra.Command, c cluster.Cluster, plan []upgradeStep, stage bool, p pin) {
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

	// A platform change is the one edit that can leave a node Ready while
	// quietly breaking it, so it is called out rather than left for the reader
	// to spot inside two long image references.
	printPlatformChange(w, plan)

	// The pin is the operator's desired state, so what happens to it decides
	// whether the change survives the next reconcile. Say so either way.
	switch {
	case p.skipped:
		_, _ = fmt.Fprintf(w, "   ⚠️  install_image stays %s (--no-pin) — the operator will revert this\n",
			dash(p.have))
	case p.needed():
		_, _ = fmt.Fprintf(w, "   pin:    install_image %s → %s\n", dash(p.have), p.want)
	case p.want != "":
		_, _ = fmt.Fprintf(w, "   pin:    install_image already %s\n", p.want)
	}
	_, _ = fmt.Fprintln(w)
}

// printPlatformChange reports a platform switch, in the terms that matter.
//
// The installer image bakes the platform in: it decides where Talos looks for
// its machine config and network configuration on the next boot. Getting it
// wrong does not fail loudly — the node comes back up and stays Ready while its
// config source is silently gone — so the before and after are stated plainly,
// alongside what the nodes currently report running as.
func printPlatformChange(w io.Writer, plan []upgradeStep) {
	running := map[string]struct{}{}
	target := map[string]struct{}{}
	for _, step := range plan {
		if step.platform != "" {
			running[step.platform] = struct{}{}
		}
		if p := talosctl.PlatformOf(step.image); p != "" {
			target[p] = struct{}{}
		}
	}
	if len(target) == 0 {
		return
	}
	from, to := joinSet(running), joinSet(target)
	if from == to || from == "" {
		return
	}
	_, _ = fmt.Fprintf(w, "   ⚠️  platform: nodes report %s, installer targets %s\n", from, to)
	_, _ = fmt.Fprintln(w,
		"       The platform decides where Talos reads its config and network settings on the next")
	_, _ = fmt.Fprintln(w,
		"       boot. A wrong one does not fail loudly — the node comes back Ready with its config")
	_, _ = fmt.Fprintln(w,
		"       source gone. Make sure this matches where the cluster actually runs.")
}

// joinSet renders a set of values compactly and deterministically.
func joinSet(set map[string]struct{}) string {
	out := make([]string, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}
