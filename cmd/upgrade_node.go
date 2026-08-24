package cmd

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

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
		force     bool
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

A node that already runs exactly what the upgrade would install is left alone.
That comparison is against what the node reports — its Talos version and its
Image Factory schematic, both read from the guest cluster's own Node object —
and never against machine.install.image, which no upgrade writes back and which
has been observed two upgrades stale. Both halves must be known and both must
match: a node that cannot be read is upgraded, because the node a half-finished
run left behind is precisely the one that fails to answer, and reading silence
as "already done" would strand it.

Skipping is off entirely for anything a Node object cannot report. --platform
and --registry are positional parts of the reference with no counterpart on the
node, and --image replaces the reference outright, so under any of those every
targeted node is upgraded and the plan says so. --force does the same on
demand, for reinstalling over a node that reports the right version but is not
behaving like it.

Note that the talos-operator also enforces a cluster's Talos version. Running
this by hand is for the cases the operator cannot resolve — an upgrade that
needs stepping by hand, or a node it has given up on.`,
		Example: `  # Upgrade every node of a cluster to v1.13.9, keeping each node's schematic.
  viti talos upgrade-node my-cluster --to v1.13.9

  # One node only, staged so it takes effect on the next reboot.
  viti talos upgrade-node my-cluster -N my-cluster-wrk0 --to v1.13.9 --stage

  # See exactly which image each node would get, changing nothing. Nodes already
  # on the target are shown with a tick and would not be touched.
  viti talos upgrade-node my-cluster --to v1.13.9 --dry-run

  # Reinstall over a node that reports the target but is not behaving like it.
  viti talos upgrade-node my-cluster -N my-cluster-wrk0 --to v1.13.9 --force

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

			policy := newSkipPolicy(edit, image, force)
			plan, err := planNodeUpgrades(cmd, sess, nodes, edit, image, policy)
			if err != nil {
				return err
			}
			// The pin is resolved from the whole plan, not just the work: it is
			// the cluster's desired state, and a run where every node already
			// arrived must still leave that state correct.
			pin, err := pinTarget(cmd, sess.Cluster(), plan, noPin)
			if err != nil {
				return err
			}
			printUpgradePlan(cmd, sess.Cluster(), plan, stage, pin, policy)
			work := todo(plan)
			if dryRun {
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(),
					"🔎 dry run — nothing was upgraded and nothing was pinned. Every image above was "+
						"derived from that node's own machine.install.image.")
				return nil
			}

			// Nothing to do is a success, and it is still worth reconciling the
			// pin: the nodes have arrived, and leaving the operator's desired
			// state behind them would have it undo their arrival.
			if len(work) == 0 {
				if pin.needed() {
					if err := cluster.PinInstallImage(contextOrBackground(cmd), sess.Cluster(), pin.want); err != nil {
						return err
					}
					echo(cmd, "pinned install_image = "+pin.want)
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(),
					"✅ every targeted node already runs the target — nothing to upgrade\n")
				return nil
			}

			if !yes {
				ok, err := confirm(cmd, fmt.Sprintf("Upgrade %d node(s) of %s, one at a time?",
					len(work), sess.Cluster().Name()))
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

			for i, step := range work {
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
					printUpgradeFailure(cmd, upgradeFailure{
						remaining:   work[i:],
						done:        i,
						total:       len(work),
						stage:       stage,
						noDrain:     noDrain,
						pin:         pin,
						clusterArg:  resumeClusterArg(name, sess.Cluster()),
						passthrough: passthrough,
					})
					return fmt.Errorf("node %s: %w — %d of %d node(s) upgraded, the rest were not touched",
						step.node.Name, err, i, len(work))
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
	cmd.Flags().BoolVar(&force, "force", false,
		"upgrade every targeted node, including any already running the target image")
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
	// state is what the node reports running. A zero value means "cannot
	// tell", never "not upgraded" — the two lead to opposite decisions, and
	// treating a node that could not be read as already-upgraded would skip
	// exactly the node a half-finished run left behind.
	state cluster.NodeState
	// already records that the node runs precisely what this upgrade would put
	// on it, so there is nothing to do and no reason to reboot it.
	already bool
}

// running is the Talos version the node reports, "" when it could not be read.
func (s upgradeStep) running() string { return s.state.Version }

// versionSource names where a row's current version came from, and describes
// it in the terms the reader needs: whether they are looking at reality or at
// something that wants to be.
type versionSource int

const (
	sourceNone versionSource = iota
	sourceRunning
	sourceInstalled
	sourceDeclared
)

func (v versionSource) String() string {
	switch v {
	case sourceRunning:
		return "what each node runs now, from the guest cluster's own Node objects"
	case sourceInstalled:
		return "what each node's config installs — desired state, not what booted"
	case sourceDeclared:
		return "what each machine declares in spec.os.imageID — desired state, not what booted"
	}
	return ""
}

// currentVersion is what a row shows as the node's version now, and where that
// came from.
//
// The fallbacks are display fallbacks only: both are desired state, so neither
// says anything about whether the node needs the upgrade. That is exactly why
// the source is returned alongside the version rather than left to look the
// same as the real thing.
func (s upgradeStep) currentVersion() (string, versionSource) {
	if s.running() != "" {
		return talosctl.NormalizeVersion(s.running()), sourceRunning
	}
	if tag := talosctl.TagOf(s.from); tag != "" {
		return tag, sourceInstalled
	}
	if v := talosctl.NormalizeVersion(s.node.TalosVersion); v != "" {
		return v, sourceDeclared
	}
	return "", sourceNone
}

// planNodeUpgrades resolves the installer image for every node before any node
// is touched.
//
// Resolving up front is the point: a fleet of nodes that turn out to disagree
// about their schematic should be visible in the plan, not discovered halfway
// through a rolling reboot.
func planNodeUpgrades(cmd *cobra.Command, sess *cluster.Session, nodes []cluster.Node, edit talosctl.ImageEdit, image string, policy skipPolicy) ([]upgradeStep, error) {
	ctx := contextOrBackground(cmd)
	running := runningState(cmd, sess.Cluster(), nodes)
	steps := make([]upgradeStep, 0, len(nodes))
	for _, node := range nodes {
		target := talosctl.Target{
			Talosconfig: sess.Talosconfig,
			Endpoints:   sess.Endpoints(),
			Nodes:       []string{node.IP},
		}
		step := upgradeStep{node: node, state: running[node.Name]}
		// Only read the running platform when it is about to change: it costs
		// a round trip per node and says nothing useful otherwise.
		if edit.Platform != "" || image != "" {
			step.platform = talosctl.NodePlatform(ctx, target)
		}

		if image != "" {
			step.image = image
			step.already = policy.alreadyOnTarget(step)
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
		step.already = policy.alreadyOnTarget(step)
		steps = append(steps, step)
	}
	return steps, nil
}

// runningState resolves what each node actually runs, keyed by node name.
//
// Best effort by design: this is the truth the plan wants, but it lives in the
// guest cluster and a run must not be blocked by a guest cluster that cannot
// be reached — upgrading a broken cluster is a large part of why this command
// exists. What cannot be read is reported and left empty, and the plan says
// which nodes it had to fall back on rather than quietly showing desired state
// as though it were reality.
//
// The operator-verified TalosVersionEnforcement condition is the fallback. It
// is free and truthful but cluster-wide, so it can only speak when every node
// agrees — which is never the case mid-upgrade, exactly when the per-node read
// matters most. It also carries no schematic, so a run resolved from it can
// report versions but can never skip a node.
func runningState(cmd *cobra.Command, c cluster.Cluster, nodes []cluster.Node) map[string]cluster.NodeState {
	byNode, err := cluster.RunningState(contextOrBackground(cmd), c)
	if err == nil {
		return byNode
	}
	warn(cmd, fmt.Errorf("reading the nodes' running Talos state: %w", err))

	enforced := cluster.EnforcedVersion(c)
	if enforced == "" {
		return nil
	}
	echo(cmd, fmt.Sprintf(
		"falling back to the operator-verified version for the whole cluster: v%s", enforced))
	out := make(map[string]cluster.NodeState, len(nodes))
	for _, n := range nodes {
		out[n.Name] = cluster.NodeState{Version: enforced}
	}
	return out
}

// skipPolicy decides whether a node already running the target can be left
// alone, and is deliberately narrow about when it will say so.
//
// Skipping is only safe when every part of the target reference can be checked
// against what the node reports, and a Node object reports exactly two things:
// its Talos version and its schematic. The registry and the platform it cannot
// report at all — so an edit that moves either is never skipped, even if the
// version and schematic happen to match. That is not conservatism for its own
// sake: --platform decides where Talos reads its config on the next boot, and
// wrongly skipping it leaves a node Ready with its config source silently gone.
//
// --image is excluded for the same reason: it replaces the whole reference, so
// its registry and platform are whatever the caller typed and neither can be
// verified against the node.
type skipPolicy struct {
	// enabled is false when something unverifiable is being changed, or when
	// the caller asked for every node to be upgraded regardless.
	enabled bool
	// reason says why skipping is off, for the plan to state plainly rather
	// than leaving a fleet of no-op reboots looking intentional.
	reason string
}

func newSkipPolicy(edit talosctl.ImageEdit, image string, force bool) skipPolicy {
	switch {
	case force:
		return skipPolicy{reason: "--force: every node is upgraded, including any already on the target"}
	case image != "":
		return skipPolicy{reason: "--image replaces the whole reference, so its registry and platform " +
			"cannot be checked against what the nodes report"}
	case edit.Platform != "":
		return skipPolicy{reason: "--platform changes where Talos reads its config on the next boot, " +
			"which a node does not report — no node is skipped"}
	case edit.Registry != "":
		return skipPolicy{reason: "--registry cannot be checked against what the nodes report"}
	}
	return skipPolicy{enabled: true}
}

// alreadyOnTarget reports whether the node runs precisely what the upgrade
// would install, so upgrading it would reboot it to arrive where it already is.
//
// Both halves must be known and both must match. An unread version or an
// absent schematic annotation means "cannot tell", and the answer to that is
// always to upgrade: a node that a half-finished run left behind is exactly the
// node that fails to answer, and skipping it would strand the one node that
// needed the command.
func (p skipPolicy) alreadyOnTarget(s upgradeStep) bool {
	if !p.enabled {
		return false
	}
	running, schematic := s.state.Version, s.state.Schematic
	if running == "" || schematic == "" {
		return false
	}
	if talosctl.NormalizeVersion(running) != talosctl.TagOf(s.image) {
		return false
	}
	return schematic == talosctl.SchematicOf(s.image)
}

// todo returns the steps that still have work, in plan order.
func todo(plan []upgradeStep) []upgradeStep {
	out := make([]upgradeStep, 0, len(plan))
	for _, step := range plan {
		if !step.already {
			out = append(out, step)
		}
	}
	return out
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

func printUpgradePlan(cmd *cobra.Command, c cluster.Cluster, plan []upgradeStep, stage bool, p pin, policy skipPolicy) {
	w := cmd.ErrOrStderr()
	effect := "each node reboots into the new version as it is upgraded"
	if stage {
		effect = "staged only; each node takes the upgrade on its next reboot"
	}
	// The headline counts the work, not the targets: "upgrading 4 nodes" when
	// three of them are already there is the claim this whole change exists to
	// stop making.
	work := len(todo(plan))
	skipped := len(plan) - work
	switch {
	case work == 0:
		_, _ = fmt.Fprintf(w, "\n✅ All %d targeted node(s) of %s already run the target\n",
			len(plan), c.Describe())
	case skipped > 0:
		_, _ = fmt.Fprintf(w, "\n⬆️  Upgrading %d of %d node(s) of %s, one at a time\n",
			work, len(plan), c.Describe())
	default:
		_, _ = fmt.Fprintf(w, "\n⬆️  Upgrading %d node(s) of %s, one at a time\n", len(plan), c.Describe())
	}
	if work > 0 {
		_, _ = fmt.Fprintf(w, "   effect: %s\n", effect)
	}
	// Why nothing is being skipped is worth a line whenever a node might
	// otherwise have been: a fleet of no-op reboots should never look
	// deliberate by omission.
	if !policy.enabled && policy.reason != "" {
		_, _ = fmt.Fprintf(w, "   skip:   off — %s\n", policy.reason)
	}

	// The lineage every node shares, when they share one. Computed once: it
	// decides both how the rows read and whether the pin can be stated as a
	// version rather than as two more full references.
	lineage := planLineage(plan)
	printUpgradeRows(w, plan, lineage)

	// The declared version is not the installed one and is routinely ahead of
	// it, so the disagreement is named rather than left to look like a
	// downgrade.
	printDeclaredDrift(w, plan)

	// A platform change is the one edit that can leave a node Ready while
	// quietly breaking it, so it is called out rather than left for the reader
	// to spot inside two long image references.
	printPlatformChange(w, plan)

	// The pin is the operator's desired state, so what happens to it decides
	// whether the change survives the next reconcile. Say so either way.
	have, want := pinVersions(p, lineage)
	switch {
	case p.skipped:
		_, _ = fmt.Fprintf(w, "   ⚠️  install_image stays %s (--no-pin) — the operator will revert this\n",
			dash(have))
	case p.needed():
		_, _ = fmt.Fprintf(w, "   pin:    install_image %s → %s\n", dash(have), want)
	case p.want != "":
		_, _ = fmt.Fprintf(w, "   pin:    install_image already %s\n", want)
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

// printUpgradeRows renders one line per node, showing what it is on now and
// what it is being moved to.
//
// Which form that takes depends on what actually differs. --to only swaps the
// tag, so in the ordinary case every node moves within one installer lineage
// and the hundred-character reference is identical on both sides of every
// arrow: printed once with the rows carrying only the version, the change is
// legible. When the nodes disagree, or when more than the tag moves, the full
// references are the information and are printed as such.
func printUpgradeRows(w io.Writer, plan []upgradeStep, lineage string) {
	if lineage != "" {
		_, _ = fmt.Fprintf(w, "   image:  %s\n", lineage)
		printCurrentColumn(w, plan)
		for _, step := range plan {
			current, _ := step.currentVersion()
			_, _ = fmt.Fprintf(w, "   %s %-28s %-16s %s → %s%s\n", bullet(step), step.node.Name,
				step.node.Role, dash(current), dash(talosctl.TagOf(step.image)), skipNote(step))
		}
		return
	}
	// --image replaces the reference outright and so never reads the node's
	// own. Saying so is the honest rendering: there is no "from" to show, and
	// leaving the column blank would read as "on nothing".
	if replacement := planReplacement(plan); replacement != "" {
		_, _ = fmt.Fprintf(w, "   image:  %s\n", replacement)
		_, _ = fmt.Fprintln(w, "           replaces each node's own installer, which --image does not read")
		printCurrentColumn(w, plan)
		for _, step := range plan {
			current, _ := step.currentVersion()
			_, _ = fmt.Fprintf(w, "   %s %-28s %-16s %s%s\n", bullet(step), step.node.Name,
				step.node.Role, dash(current), skipNote(step))
		}
		return
	}
	for _, step := range plan {
		_, _ = fmt.Fprintf(w, "   %s %-28s %s%s\n", bullet(step), step.node.Name, step.node.Role, skipNote(step))
		// The running version is not in either reference — an installer image
		// records what was asked for, not what booted — so it gets its own line
		// rather than being inferred from the "from" tag.
		if step.running() != "" {
			_, _ = fmt.Fprintf(w, "       runs  %s\n", talosctl.NormalizeVersion(step.running()))
		}
		if step.from != "" {
			_, _ = fmt.Fprintf(w, "       from  %s\n", step.from)
		}
		_, _ = fmt.Fprintf(w, "       to    %s\n", step.image)
	}
}

// bullet marks a row by whether it is work. A skipped node reads as done
// because it is, and giving it the same bullet as the rest would make a plan
// that touches one node look like a plan that touches five.
func bullet(s upgradeStep) string {
	if s.already {
		return "✓"
	}
	return "•"
}

// skipNote says why a row is not being acted on, on the row itself: a reader
// scanning a long plan for what it will do should not have to hold a legend in
// their head.
func skipNote(s upgradeStep) string {
	if s.already {
		return "   already on target, not touched"
	}
	return ""
}

// planLineage returns the installer reference every node shares on both sides
// of the upgrade, and "" when they do not share one.
//
// "" means the full references have to be printed: the nodes are on different
// lineages, or the edit moves the registry, platform or schematic rather than
// only the version, or a node's current installer was never read.
func planLineage(plan []upgradeStep) string {
	lineage := ""
	for _, step := range plan {
		if step.from == "" {
			return ""
		}
		fromBase, _ := talosctl.SplitImageTag(step.from)
		toBase, _ := talosctl.SplitImageTag(step.image)
		if fromBase != toBase {
			return ""
		}
		if lineage != "" && lineage != toBase {
			return ""
		}
		lineage = toBase
	}
	return lineage
}

// pinVersions renders a pin as compactly as it can be stated truthfully.
//
// When both sides of the pin sit on the installer lineage the rows above
// already named, the versions are the whole change and the references are two
// hundred characters of agreement. When either side does not — a pin on a
// different schematic than the nodes carry, which is the case worth catching —
// the full references are the point and are returned unchanged.
func pinVersions(p pin, lineage string) (have, want string) {
	haveBase, haveTag := talosctl.SplitImageTag(p.have)
	wantBase, wantTag := talosctl.SplitImageTag(p.want)
	if lineage == "" || haveBase != lineage || wantBase != lineage || haveTag == "" || wantTag == "" {
		return p.have, p.want
	}
	return haveTag, wantTag
}

// planReplacement returns the single image every node is being moved onto when
// none of their current ones was read — the --image path — and "" otherwise.
func planReplacement(plan []upgradeStep) string {
	image := ""
	for _, step := range plan {
		if step.from != "" {
			return ""
		}
		if image != "" && image != step.image {
			return ""
		}
		image = step.image
	}
	return image
}

// printCurrentColumn writes the column's provenance, and writes nothing when
// there is no version to be provenant about.
func printCurrentColumn(w io.Writer, plan []upgradeStep) {
	if note := currentColumn(plan); note != "" {
		_, _ = fmt.Fprintf(w, "   %s\n", note)
	}
}

// currentColumn says what the plan's current-version column actually holds.
//
// Three sources disagree about a Talos cluster's version and only one of them
// is reality (see internal/cluster/version.go). A column that silently mixes
// them is worse than no column: it reads as fact. So the column names its own
// provenance, and names it per run rather than in the help text, because which
// source was available is decided at run time.
func currentColumn(plan []upgradeStep) string {
	counts := map[versionSource]int{}
	for _, step := range plan {
		_, source := step.currentVersion()
		counts[source]++
	}
	if counts[sourceNone] == len(plan) {
		return ""
	}
	// The ordinary case: every row came from the same place, so it is named
	// once and the reader is done.
	for _, source := range []versionSource{sourceRunning, sourceInstalled, sourceDeclared} {
		if counts[source] == len(plan) {
			return "current: " + source.String()
		}
	}
	// Mixed. Naming the majority and counting the exceptions beats listing
	// every source: the number is the part that decides whether to trust the
	// column, and a fleet where some nodes fell back is itself worth noticing.
	fellBack := len(plan) - counts[sourceRunning]
	if counts[sourceRunning] > 0 {
		return fmt.Sprintf("current: %s, except %d of %d falling back to desired state",
			sourceRunning.String(), fellBack, len(plan))
	}
	return fmt.Sprintf("current: desired state for all %d — no running version could be read", len(plan))
}

// printDeclaredDrift names the version sources that disagree with the one the
// rows above are showing.
//
// Both of the others are desired state and both routinely run ahead:
// spec.os.imageID because the operator bumps it to the newest available image
// before any upgrade happens, and machine.install.image because it is
// reconciled from the cluster's pin, which this very command moves before it
// touches a node. Left unnamed, either standing in for the running version is
// actively misleading — a fleet declaring v1.13.9 while running v1.12.7 makes
// an upgrade to v1.13.8 read as a downgrade, and a version guard built on it
// would refuse that upgrade for the same reason.
//
// The comparison is against whatever the rows show, so it still works when the
// guest cluster could not be reached: with no running version the baseline is
// what the configs install, and a declared version ahead of that is the case
// this note exists for.
func printDeclaredDrift(w io.Writer, plan []upgradeStep) {
	runs, declared, installed := map[string]struct{}{}, map[string]struct{}{}, map[string]struct{}{}
	for _, step := range plan {
		if step.running() != "" {
			runs[talosctl.NormalizeVersion(step.running())] = struct{}{}
		}
		if v := talosctl.NormalizeVersion(step.node.TalosVersion); v != "" {
			declared[v] = struct{}{}
		}
		if tag := talosctl.TagOf(step.from); tag != "" {
			installed[tag] = struct{}{}
		}
	}

	// Which sources are candidates for disagreeing depends on which one is the
	// baseline: comparing the baseline against itself would report every
	// cluster as drifting.
	type source struct{ format, version string }
	candidates := []source{
		{"machines declare %s in spec.os.imageID", joinSet(declared)},
		{"configs install %s", joinSet(installed)},
	}
	baseline, label, tail := joinSet(runs), "nodes run", "what the nodes actually run"
	if baseline == "" {
		baseline, label, tail = joinSet(installed), "configs install", "the installed ones"
		candidates = candidates[:1]
	}
	if baseline == "" {
		return
	}

	var disagree []string
	for _, c := range candidates {
		if c.version != "" && c.version != baseline {
			disagree = append(disagree, fmt.Sprintf(c.format, c.version))
		}
	}
	if len(disagree) == 0 {
		return
	}
	// No pronoun back to the disagreeing sources: there may be one or two of
	// them, and "that is"/"those are" would have to agree with a count that is
	// decided at run time.
	_, _ = fmt.Fprintf(w, "   note:   %s %s, but %s.\n", label, baseline, strings.Join(disagree, " and "))
	// Not "runs ahead": spec.os.imageID does, but machine.install.image is a
	// fossil of the original install that no upgrade writes back, so it lags —
	// d-stackops-1010 sat two upgrades past v1.13.5 with its configs still
	// saying v1.13.5. Direction-neutral is the only wording true of both.
	_, _ = fmt.Fprintf(w,
		"           Desired state does not track what booted; the rows above show %s.\n", tail)
}

// upgradeFailure is what a failed node left behind, and what is needed to
// describe picking the run back up.
type upgradeFailure struct {
	// remaining is the unfinished tail of the plan, the failed node first.
	remaining []upgradeStep
	done      int
	total     int
	stage     bool
	noDrain   bool
	pin       pin
	// clusterArg is the cluster as it should be named on the command line.
	clusterArg  string
	passthrough []string
}

func (f upgradeFailure) failed() upgradeStep { return f.remaining[0] }

// printUpgradeFailure reports what a failed node leaves behind and how to
// resume, because two consequences of failing here are both easy to miss and
// expensive to miss.
//
// The upgrade cordons and drains a node before rebooting it and uncordons it
// only once the upgrade succeeds, so a failure leaves a node
// SchedulingDisabled with nothing on screen saying so — it surfaces later as a
// cluster that will not schedule onto one of its workers.
//
// And the pin was deliberately moved to the target before any node was
// touched, so the operator's desired state is already correct and the run only
// needs its remaining nodes. Re-running the whole cluster instead works, but
// reboots every node that already succeeded, so the resume command names the
// nodes left rather than leaving that to be assembled by hand.
func printUpgradeFailure(cmd *cobra.Command, f upgradeFailure) {
	w := cmd.ErrOrStderr()
	failed := f.failed().node.Name

	_, _ = fmt.Fprintf(w, "\n❌ %s was not upgraded — %d of %d node(s) are done.\n",
		failed, f.done, f.total)

	// Only when draining was in play: --stage does not reboot, and --no-drain
	// asked talosctl not to cordon in the first place.
	if !f.stage && !f.noDrain {
		_, _ = fmt.Fprintf(w,
			"   %s may be left cordoned. The upgrade cordons and drains a node before rebooting it\n"+
				"   and uncordons it only once the upgrade succeeds, so check it before anything else:\n"+
				"     kubectl get node %s\n"+
				"     kubectl uncordon %s   # only if it is SchedulingDisabled and not coming back\n",
			failed, failed, failed)
	}

	// An installer reference is too long to sit mid-sentence, so the prose
	// says what the pin means and the reference goes on its own line.
	switch {
	case f.pin.skipped:
		_, _ = fmt.Fprintln(w,
			"   install_image was not pinned (--no-pin), so the operator still wants the old image and\n"+
				"   will revert the nodes that did succeed:")
		_, _ = fmt.Fprintf(w, "     %s\n", dash(f.pin.have))
	case f.pin.want != "":
		_, _ = fmt.Fprintln(w,
			"   install_image is already pinned to the target, so resuming changes nothing about the\n"+
				"   operator's desired state:")
		_, _ = fmt.Fprintf(w, "     %s\n", f.pin.want)
	}

	_, _ = fmt.Fprintf(w,
		"   Resume on the %d node(s) left rather than re-running the cluster, which would reboot the\n"+
			"   %d that are done:\n     %s\n",
		len(f.remaining), f.done, resumeCommand(cmd, f))
}

// resumeClusterArg picks how to name the cluster on a resume command line: as
// the user named it when they named it at all, and otherwise by the name the
// interactive picker resolved to.
func resumeClusterArg(typed string, c cluster.Cluster) string {
	if strings.TrimSpace(typed) != "" {
		return typed
	}
	return c.Name()
}

// resumeCommand renders the invocation that picks the run back up on the nodes
// it did not reach.
//
// It is rebuilt from the flags the command was actually given rather than from
// the handful this file happens to know about, so a resume carries --stage,
// --schematic, --endpoint and anything else the original run used. Only --node
// is dropped: that is the one flag the resume has to disagree with.
func resumeCommand(cmd *cobra.Command, f upgradeFailure) string {
	parts := []string{commandName(cmd)}
	if f.clusterArg != "" {
		parts = append(parts, shellQuote(f.clusterArg))
	}
	for _, step := range f.remaining {
		parts = append(parts, "-N", shellQuote(step.node.Name))
	}
	cmd.Flags().Visit(func(fl *pflag.Flag) {
		if fl.Name == "node" {
			return
		}
		parts = append(parts, flagArgs(fl)...)
	})
	if len(f.passthrough) > 0 {
		parts = append(parts, "--")
		for _, a := range f.passthrough {
			parts = append(parts, shellQuote(a))
		}
	}
	return strings.Join(parts, " ")
}

// flagArgs renders one flag that was set back into command-line arguments.
func flagArgs(f *pflag.Flag) []string {
	// A repeatable flag holds several values, and its String() renders them as
	// "[a,b]" — pasted back, that is one value with brackets in it.
	if sv, ok := f.Value.(pflag.SliceValue); ok {
		out := make([]string, 0, 1)
		for _, v := range sv.GetSlice() {
			out = append(out, "--"+f.Name+"="+shellQuote(v))
		}
		return out
	}
	if f.Value.Type() == "bool" {
		if f.Value.String() == "true" {
			return []string{"--" + f.Name}
		}
		return []string{"--" + f.Name + "=false"}
	}
	return []string{"--" + f.Name + "=" + shellQuote(f.Value.String())}
}

// shellQuote quotes a value that would not survive being pasted into a shell.
// The command it builds is meant to be run, not just read, so a value the
// shell would resplit has to come back quoted.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	safe := func(r rune) bool {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
			return true
		}
		return strings.ContainsRune("-_./:@=+,", r)
	}
	for _, r := range s {
		if !safe(r) {
			return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
		}
	}
	return s
}
