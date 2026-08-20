package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/spf13/cobra"

	"github.com/vitistack/vitictl-talos/internal/cluster"
	"github.com/vitistack/vitictl-talos/internal/output"
	"github.com/vitistack/vitictl-talos/internal/talosctl"
)

// patchOptions is the fleet patch's full configuration, kept together so the
// plan that is printed and the work that is done are built from one value.
type patchOptions struct {
	clusters []string
	all      bool
	patches  []string
	mode     string
	dryRun   bool
	parallel int
	serial   bool
	batch    bool
	yes      bool
	failFast bool
	noProbe  bool
	ifMatch  string
	output   string
}

func newPatchCmd(s *scope) *cobra.Command {
	n := &nodeSelector{}
	o := &patchOptions{}

	cmd := &cobra.Command{
		Use:     "patch [-- talosctl flags]",
		Aliases: []string{"patch-machineconfig", "patchmc"},
		Short:   "🩹 Apply a machine-config patch across many clusters at once",
		Long: `Apply the same machine-configuration patch to every node of many Talos
clusters, through "talosctl patch machineconfig".

This is the command for a fleet-wide configuration change. Editing each node
by hand does not scale past a handful and cannot be reviewed; a patch is one
file, applied identically everywhere, and re-running it on a node that already
matches is a no-op.

The workflow it is built around:

  1. Write the patch once — a strategic-merge fragment, or an RFC 6902 JSON
     patch list, exactly as talosctl accepts them.
  2. Preview it everywhere:      viti talos patch --all -p @patch.yaml --dry-run
     Nothing is written; each node prints the diff the patch would make.
  3. Apply it:                   viti talos patch --all -p @patch.yaml --mode staged
  4. Confirm it landed:          viti talos show <cluster> --role controlplane

Choose the clusters with --cluster (repeatable), with --all, or by leaving
both out to mark them in an interactive picker: filter, press Ctrl-A to mark
everything shown, Enter to confirm. There is deliberately no "neither flag
means the whole fleet" shortcut — a forgotten argument must not turn into a
fleet-wide write.

--mode decides when the change takes effect, and it is the safety-critical
choice here:

  staged     write to disk, to take effect at the next reboot. Nothing
             restarts, so a whole fleet can be staged in one pass and the
             reboots sequenced separately.
  no-reboot  apply what can take effect live, and fail rather than reboot.
  auto       apply now, rebooting only if the change requires it (default).
  reboot     apply and reboot unconditionally.
  try        apply with a timeout, rolling back automatically.

--if-match reads each node's current config first and patches only the nodes
that match the given regular expression. Use it whenever a patch is not
idempotent — deleting a config document with "$patch: delete" is the common
case, since talosctl errors out if the document is already gone. Guarded that
way, the same command can be re-run safely: nodes already done are reported as
skipped rather than failing, so a run interrupted halfway just picks up where
it left off. With --dry-run it doubles as a fleet-wide survey of which
clusters still carry the thing you are removing.

Clusters are patched concurrently (--parallel, default 4) because they are
independent of each other. Nodes within one cluster are not: whenever the mode
can reboot a node, they are patched one at a time so a cluster never loses
quorum to a fan-out. --batch overrides that, and --serial forces it on for the
modes that would otherwise batch.

Anything after a -- is handed to talosctl unchanged.`,
		Example: `  # Preview across the whole fleet, changing nothing.
  viti talos patch --all -p @patch.yaml --dry-run

  # Stage it everywhere; nothing reboots.
  viti talos patch --all -p @patch.yaml --mode staged --yes

  # Two named clusters, control planes only, applied live.
  viti talos patch -c prod-a -c prod-b --role controlplane -p @patch.yaml --mode no-reboot

  # Pick the clusters interactively, inline patch.
  viti talos patch -p '[{"op":"add","path":"/machine/kubelet/extraArgs","value":{"rotate-server-certificates":"true"}}]'

  # Remove a config document fleet-wide, guarded so the run is re-runnable.
  viti talos patch --all -p @drop-dhcp.yaml --if-match 'kind: DHCPv4Config' --mode no-reboot`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if at := cmd.ArgsLenAtDash(); at > 0 {
				return fmt.Errorf(
					"patch takes no positional arguments — name clusters with --cluster, "+
						"and put talosctl flags after a -- (got %s)", strings.Join(args[:at], ", "))
			}
			var passthrough []string
			if at := cmd.ArgsLenAtDash(); at >= 0 {
				passthrough = args[at:]
			} else if len(args) > 0 {
				return fmt.Errorf(
					"patch takes no positional arguments — name clusters with --cluster (got %s)",
					strings.Join(args, ", "))
			}
			return runPatch(cmd, s, n, o, passthrough)
		},
	}

	n.register(cmd)
	cmd.Flags().StringArrayVarP(&o.clusters, "cluster", "c", nil,
		"cluster to patch, by name or clusterId (repeatable)")
	cmd.Flags().BoolVar(&o.all, "all", false,
		"patch every Talos cluster in scope")
	cmd.Flags().StringArrayVarP(&o.patches, "patch", "p", nil,
		"patch to apply: inline YAML/JSON, or @path to read one from a file (repeatable)")
	cmd.Flags().StringVarP(&o.mode, "mode", "m", "auto",
		fmt.Sprintf("when the change takes effect: %s", strings.Join(applyModes, ", ")))
	cmd.Flags().BoolVar(&o.dryRun, "dry-run", false,
		"print the diff each node would get and change nothing")
	cmd.Flags().IntVar(&o.parallel, "parallel", 4,
		"how many clusters to patch at once")
	cmd.Flags().BoolVar(&o.serial, "serial", false,
		"patch nodes one at a time even in modes that would batch them")
	cmd.Flags().BoolVar(&o.batch, "batch", false,
		"patch all of a cluster's nodes in one call even in modes that can reboot")
	cmd.Flags().BoolVarP(&o.yes, "yes", "y", false,
		"skip the confirmation prompt")
	cmd.Flags().BoolVar(&o.failFast, "fail-fast", false,
		"stop the remaining clusters after the first failure")
	cmd.Flags().BoolVar(&o.noProbe, "no-probe", false,
		"skip the pre-flight check that every cluster's Talos API answers")
	cmd.Flags().StringVar(&o.ifMatch, "if-match", "",
		"only patch nodes whose current machine config matches this regular expression")
	cmd.Flags().StringVarP(&o.output, "output", "o", "",
		"summary format: table, json, yaml")
	return cmd
}

// nodeResult is one talosctl invocation's outcome.
type nodeResult struct {
	Nodes  []string `json:"nodes"`
	Output string   `json:"output,omitempty"`
	Error  string   `json:"error,omitempty"`
}

// clusterResult is one cluster's outcome, holding every invocation made
// against it so a partially-patched cluster reports exactly how far it got.
type clusterResult struct {
	AvailabilityZone string       `json:"availabilityZone"`
	Namespace        string       `json:"namespace"`
	Cluster          string       `json:"cluster"`
	ClusterID        string       `json:"clusterId,omitempty"`
	Nodes            []string     `json:"targetedNodes"`
	Steps            []nodeResult `json:"steps,omitempty"`
	Error            string       `json:"error,omitempty"`
	// Skipped marks a cluster that was never attempted, which --fail-fast
	// produces. Reporting it is the difference between "this one is fine" and
	// "this one is unknown".
	Skipped bool `json:"skipped,omitempty"`
	// NoMatch marks a cluster where --if-match ruled every node out. That is
	// a success, not a skip: the cluster was inspected and had nothing left to
	// change. Conflating it with Skipped would make a completed fleet look
	// half-done on the second run.
	NoMatch bool `json:"nothingToDo,omitempty"`
}

// OK reports whether everything attempted against the cluster succeeded.
func (r clusterResult) OK() bool {
	if r.NoMatch {
		return true
	}
	if r.Error != "" || r.Skipped {
		return false
	}
	for _, s := range r.Steps {
		if s.Error != "" {
			return false
		}
	}
	return true
}

func runPatch(cmd *cobra.Command, s *scope, n *nodeSelector, o *patchOptions, passthrough []string) error {
	format, err := output.Parse(o.output)
	if err != nil {
		return err
	}
	if format == output.FormatWide || format == output.FormatName {
		return fmt.Errorf("unsupported summary format %q for patch (valid: table, json, yaml)", o.output)
	}
	mode, err := parseMode(o.mode)
	if err != nil {
		return err
	}
	if o.serial && o.batch {
		return errors.New("--serial and --batch are mutually exclusive")
	}
	if o.parallel < 1 {
		return fmt.Errorf("--parallel must be at least 1 (got %d)", o.parallel)
	}
	role, err := parseRole(n.role)
	if err != nil {
		return err
	}
	patchArgs, err := patchFlags(o.patches)
	if err != nil {
		return err
	}

	targets, err := selectClusters(cmd, s, o.clusters, o.all)
	if err != nil {
		return err
	}

	// Resolve every cluster before writing anything to any of them. A patch
	// run that dies halfway through because the eighth cluster has no
	// credentials secret is far worse than one that refuses to start.
	sessions, err := openAll(cmd, s, targets, n.nodes, role)
	if err != nil {
		return err
	}
	defer func() {
		for _, p := range sessions {
			p.session.Close()
		}
	}()

	if !o.noProbe {
		if err := probeReachable(cmd, sessions); err != nil {
			return err
		}
	}

	var noMatch []planned
	if o.ifMatch != "" {
		re, err := regexp.Compile(o.ifMatch)
		if err != nil {
			return fmt.Errorf("--if-match is not a valid regular expression: %w", err)
		}
		sessions, noMatch, err = filterByConfig(cmd, sessions, re)
		if err != nil {
			return err
		}
		if len(sessions) == 0 {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
				"\n✅ nothing to do — no node in %d cluster(s) matches %q\n", len(noMatch), o.ifMatch)
			return renderPatchResults(cmd, nothingToDoResults(noMatch), format)
		}
	}

	// Serial within a cluster whenever a node might restart. auto is included:
	// it decides per-change, so a patch that turns out to need a reboot would
	// otherwise take every control plane down at once.
	serial := o.serial || (modeReboots(mode) && !o.batch)

	printPlan(cmd, sessions, mode, serial, o, patchArgs)
	if !o.dryRun && !o.yes {
		ok, err := confirm(cmd, fmt.Sprintf("Patch %d node(s) across %d cluster(s) with mode %q?",
			countNodes(sessions), len(sessions), mode))
		if err != nil {
			return err
		}
		if !ok {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "aborted")
			return errCancelled
		}
	}

	results := append(execute(cmd, sessions, patchArgs, mode, serial, o, passthrough),
		nothingToDoResults(noMatch)...)
	if err := renderPatchResults(cmd, results, format); err != nil {
		return err
	}
	failed := 0
	for _, r := range results {
		if !r.OK() {
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d cluster(s) did not complete", failed, len(results))
	}
	return nil
}

// planned is one cluster resolved and ready to patch.
type planned struct {
	session *cluster.Session
	nodes   []cluster.Node
}

// openAll resolves and opens every target, failing on the first one that
// cannot be reached.
func openAll(cmd *cobra.Command, s *scope, targets []cluster.Cluster, wantNodes []string, role string) ([]planned, error) {
	out := make([]planned, 0, len(targets))
	closeAll := func() {
		for _, p := range out {
			p.session.Close()
		}
	}
	for _, c := range targets {
		sess, err := openSession(cmd, s, c)
		if err != nil {
			closeAll()
			return nil, fmt.Errorf("cluster %s: %w", c.Describe(), err)
		}
		nodes, err := sess.Topology.Select(wantNodes, role)
		if err != nil {
			sess.Close()
			closeAll()
			return nil, fmt.Errorf("cluster %s: %w", c.Describe(), err)
		}
		out = append(out, planned{session: sess, nodes: nodes})
	}
	if len(out) == 0 {
		return nil, errors.New("no clusters to patch")
	}
	return out, nil
}

// probeReachable checks that every cluster's Talos API actually answers from
// this machine before any of them is written to.
//
// It is here because the failure it catches is both common and misleading. A
// cluster whose addresses are resolvable but not routable — a VPN or tunnel
// that is down, which is the usual cause — fails inside talosctl as a gRPC
// dial timeout, once per node, several minutes into a fleet-wide run. A TCP
// dial on the same port answers the same question in two seconds, for every
// cluster at once, before anything has changed.
//
// Any unreachable cluster stops the run rather than being skipped: "patch
// everything except the four I could not reach" is a fleet that has silently
// drifted apart, which is exactly what a fleet-wide patch exists to prevent.
func probeReachable(cmd *cobra.Command, sessions []planned) error {
	type miss struct {
		cluster   string
		endpoints []string
	}
	var unreachable []miss

	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := range sessions {
		wg.Add(1)
		go func(p planned) {
			defer wg.Done()
			if len(cluster.Reachable(p.session.Endpoints(), cluster.DefaultProbeTimeout)) > 0 {
				return
			}
			mu.Lock()
			unreachable = append(unreachable, miss{
				cluster:   p.session.Cluster().Describe(),
				endpoints: p.session.Endpoints(),
			})
			mu.Unlock()
		}(sessions[i])
	}
	wg.Wait()

	if len(unreachable) == 0 {
		return nil
	}
	sort.Slice(unreachable, func(i, j int) bool { return unreachable[i].cluster < unreachable[j].cluster })

	var b strings.Builder
	fmt.Fprintf(&b, "%d of %d cluster(s) do not answer on the Talos API (tcp/%d) from this machine:",
		len(unreachable), len(sessions), cluster.APIPort)
	for _, m := range unreachable {
		fmt.Fprintf(&b, "\n   • %s — tried %s", m.cluster, strings.Join(m.endpoints, ", "))
	}
	b.WriteString("\n\nThis is a network path problem, not a cluster one — check your VPN or tunnel. " +
		"Narrow the run to the clusters you can reach, pass --endpoint to name a reachable address, " +
		"or --no-probe to try anyway.")
	return errors.New(b.String())
}

// filterByConfig drops the nodes whose current machine config does not match
// re, returning the clusters that still have work and those that have none.
//
// This is what makes a non-idempotent patch re-runnable. Deleting a config
// document is the case that needs it: talosctl fails outright when the
// document is already gone, so without a guard, a second pass over a
// half-finished fleet reports failures for exactly the clusters that
// succeeded first time round.
//
// Each node's config is read individually — the documents this matches against
// exist only on the node — so the reads are fanned out per cluster.
func filterByConfig(cmd *cobra.Command, sessions []planned, re *regexp.Regexp) (todo, none []planned, err error) {
	ctx := contextOrBackground(cmd)
	kept := make([][]cluster.Node, len(sessions))
	errs := make([]error, len(sessions))

	var wg sync.WaitGroup
	for i := range sessions {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := sessions[i]
			for _, node := range p.nodes {
				raw, rerr := talosctl.MachineConfigYAML(ctx, talosctl.Target{
					Talosconfig: p.session.Talosconfig,
					Endpoints:   p.session.Endpoints(),
					Nodes:       []string{node.IP},
				})
				if rerr != nil {
					// Reading is a precondition, not the work itself: a node
					// that cannot be read must not be silently excluded from a
					// fleet-wide change.
					errs[i] = fmt.Errorf("cluster %s: %w", p.session.Cluster().Describe(), rerr)
					return
				}
				if re.MatchString(raw) {
					kept[i] = append(kept[i], node)
				}
			}
		}(i)
	}
	wg.Wait()

	for i := range sessions {
		if errs[i] != nil {
			return nil, nil, errs[i]
		}
		if len(kept[i]) == 0 {
			none = append(none, sessions[i])
			continue
		}
		todo = append(todo, planned{session: sessions[i].session, nodes: kept[i]})
	}

	// Two different counts, and conflating them reads as a bug: nodes ruled
	// out is the whole filtered set, while `none` is only the clusters left
	// with no work at all. A cluster that keeps some of its nodes contributes
	// to the first and not the second.
	skipped := 0
	for i := range sessions {
		skipped += len(sessions[i].nodes) - len(kept[i])
	}
	if skipped > 0 {
		msg := fmt.Sprintf("🔎 --if-match ruled out %d of %d node(s)",
			skipped, countNodes(sessions))
		if len(none) > 0 {
			msg += fmt.Sprintf("; %d cluster(s) have nothing left to do", len(none))
		}
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), msg)
	}
	return todo, none, nil
}

// nothingToDoResults renders the clusters --if-match ruled out entirely.
func nothingToDoResults(none []planned) []clusterResult {
	out := make([]clusterResult, 0, len(none))
	for _, p := range none {
		out = append(out, clusterResult{
			AvailabilityZone: p.session.Cluster().Zone(),
			Namespace:        p.session.Cluster().Namespace(),
			Cluster:          p.session.Cluster().Name(),
			ClusterID:        p.session.Cluster().ID(),
			Nodes:            nodeNames(p.nodes),
			NoMatch:          true,
		})
	}
	return out
}

// printPlan shows what is about to happen, in full, before anything does.
//
// It goes to stderr so a --output json summary stays pipeable, and it is
// printed even under --yes: an unattended run's log should still say what it
// did to which clusters.
func printPlan(cmd *cobra.Command, sessions []planned, mode string, serial bool, o *patchOptions, patchArgs []string) {
	err := cmd.ErrOrStderr()
	verb := "Patching"
	if o.dryRun {
		verb = "Previewing (dry run)"
	}
	_, _ = fmt.Fprintf(err, "\n🩹 %s %d node(s) across %d cluster(s)\n",
		verb, countNodes(sessions), len(sessions))
	_, _ = fmt.Fprintf(err, "   mode:      %s (%s)\n", mode, modeEffect(mode))
	_, _ = fmt.Fprintf(err, "   ordering:  %s within a cluster, %d cluster(s) at a time\n",
		orderingLabel(serial), o.parallel)
	for i := 0; i < len(patchArgs); i += 2 {
		_, _ = fmt.Fprintf(err, "   patch:     %s\n", truncate(patchArgs[i+1], 120))
	}
	for _, p := range sessions {
		_, _ = fmt.Fprintf(err, "   • %-40s %d node(s): %s\n",
			p.session.Cluster().Describe(), len(p.nodes),
			strings.Join(nodeNames(p.nodes), ", "))
	}
	if !o.dryRun {
		_, _ = fmt.Fprintln(err, "   (run the same command with --dry-run first to see each node's diff)")
	}
	_, _ = fmt.Fprintln(err)
}

// execute patches every cluster, up to --parallel at a time.
//
// Clusters are independent, so they run concurrently; nodes within one are
// sequenced by serial. Output is captured rather than streamed: with several
// clusters in flight, interleaved talosctl output is unreadable, and a block
// printed per cluster is both readable and attributable.
func execute(cmd *cobra.Command, sessions []planned, patchArgs []string, mode string, serial bool, o *patchOptions, passthrough []string) []clusterResult {
	ctx, cancel := context.WithCancel(contextOrBackground(cmd))
	defer cancel()

	results := make([]clusterResult, len(sessions))
	sem := make(chan struct{}, o.parallel)
	var wg sync.WaitGroup
	// failed is checked before starting each cluster so --fail-fast stops
	// scheduling new work; clusters already running are left to finish rather
	// than killed halfway through a config apply.
	var mu sync.Mutex
	failed := false

	for i := range sessions {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			p := sessions[i]
			base := clusterResult{
				AvailabilityZone: p.session.Cluster().Zone(),
				Namespace:        p.session.Cluster().Namespace(),
				Cluster:          p.session.Cluster().Name(),
				ClusterID:        p.session.Cluster().ID(),
				Nodes:            nodeNames(p.nodes),
			}
			if o.failFast {
				mu.Lock()
				stop := failed
				mu.Unlock()
				if stop {
					base.Skipped = true
					results[i] = base
					return
				}
			}

			base.Steps = patchCluster(ctx, p, patchArgs, mode, serial, o.dryRun, passthrough)
			results[i] = base
			if !base.OK() && o.failFast {
				mu.Lock()
				failed = true
				mu.Unlock()
			}
			reportCluster(cmd, base)
		}(i)
	}
	wg.Wait()
	return results
}

// patchCluster runs the patch against one cluster, either as a single call
// covering every node or one call per node.
//
// A serial run stops at the first failing node. Continuing past a node that
// would not take the config means the next one is patched while the cluster is
// already in an unknown state, which is precisely when a control plane loses
// quorum.
func patchCluster(ctx context.Context, p planned, patchArgs []string, mode string, serial bool, dryRun bool, passthrough []string) []nodeResult {
	extra := append([]string{"--mode", mode}, patchArgs...)
	if dryRun {
		extra = append(extra, "--dry-run")
	}
	extra = append(extra, passthrough...)

	run := func(nodes []cluster.Node) nodeResult {
		target := talosctl.Target{
			Talosconfig: p.session.Talosconfig,
			Endpoints:   p.session.Endpoints(),
			Nodes:       nodeAddresses(nodes),
		}
		out, err := talosctl.Capture(ctx, target, "patch machineconfig", extra...)
		res := nodeResult{Nodes: nodeNames(nodes), Output: strings.TrimSpace(out)}
		if err != nil {
			res.Error = err.Error()
		}
		return res
	}

	if !serial {
		return []nodeResult{run(p.nodes)}
	}
	steps := make([]nodeResult, 0, len(p.nodes))
	for i := range p.nodes {
		step := run(p.nodes[i : i+1])
		steps = append(steps, step)
		if step.Error != "" {
			break
		}
	}
	return steps
}

// reportCluster prints one cluster's captured output as a block, as soon as
// that cluster finishes.
func reportCluster(cmd *cobra.Command, r clusterResult) {
	w := cmd.ErrOrStderr()
	marker := "✅"
	if !r.OK() {
		marker = "❌"
	}
	_, _ = fmt.Fprintf(w, "%s %s/%s/%s\n", marker, r.AvailabilityZone, r.Namespace, r.Cluster)
	for _, s := range r.Steps {
		if body := strings.TrimSpace(s.Output); body != "" {
			_, _ = fmt.Fprintln(w, indent(body, "   "))
		}
		if s.Error != "" {
			_, _ = fmt.Fprintf(w, "   ⚠️  %s: %s\n", strings.Join(s.Nodes, ","), s.Error)
		}
	}
}

func renderPatchResults(cmd *cobra.Command, results []clusterResult, format output.Format) error {
	out := cmd.OutOrStdout()
	sorted := append([]clusterResult(nil), results...)
	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if a.AvailabilityZone != b.AvailabilityZone {
			return a.AvailabilityZone < b.AvailabilityZone
		}
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Cluster < b.Cluster
	})

	switch format {
	case output.FormatJSON:
		return output.WriteJSON(out, sorted)
	case output.FormatYAML:
		return output.WriteYAML(out, sorted)
	}

	header := "AZ\tNAMESPACE\tCLUSTER\tNODES\tRESULT\tDETAIL"
	rows := make([]string, 0, len(sorted))
	for _, r := range sorted {
		result, detail := "✅ patched", ""
		switch {
		case r.NoMatch:
			result, detail = "➖ nothing to do", "no node matched --if-match"
		case r.Skipped:
			result, detail = "⏭️  skipped", "not attempted (--fail-fast)"
		case r.Error != "":
			result, detail = "❌ failed", firstLine(r.Error)
		default:
			for _, s := range r.Steps {
				if s.Error != "" {
					result = "❌ failed"
					detail = strings.Join(s.Nodes, ",") + ": " + firstLine(s.Error)
					break
				}
			}
		}
		rows = append(rows, strings.Join([]string{
			r.AvailabilityZone, r.Namespace, r.Cluster,
			fmt.Sprintf("%d", len(r.Nodes)), result, dash(detail),
		}, "\t"))
	}
	return output.WriteTable(out, header, rows)
}

// patchFlags turns each --patch value into the "-p <value>" pair talosctl
// expects, validating @file references up front.
//
// The file check is the point: without it, a mistyped path produces the same
// error once per cluster, after the run has already started.
func patchFlags(patches []string) ([]string, error) {
	if len(patches) == 0 {
		return nil, errors.New(
			"no patch given — pass -p with an inline patch, or -p @path to read one from a file")
	}
	out := make([]string, 0, len(patches)*2)
	for _, p := range patches {
		p = strings.TrimSpace(p)
		if p == "" {
			return nil, errors.New("empty --patch value")
		}
		if strings.HasPrefix(p, "@") {
			path := strings.TrimPrefix(p, "@")
			info, err := os.Stat(path)
			if err != nil {
				if hint := glueHint(path); hint != "" {
					return nil, fmt.Errorf("patch file %q is not readable — %s", path, hint)
				}
				return nil, fmt.Errorf("patch file %q is not readable: %w", path, err)
			}
			if info.IsDir() {
				return nil, fmt.Errorf("patch file %q is a directory", path)
			}
			if info.Size() == 0 {
				return nil, fmt.Errorf("patch file %q is empty", path)
			}
		}
		out = append(out, "-p", p)
	}
	return out, nil
}

// glueHint recognises a whole command line that arrived as one flag value and
// explains it, returning "" for an ordinary missing file.
//
// It catches a specific and easily-made mistake: collecting the fixed flags in
// a variable — P="-p @patch.yaml --mode staged" — and passing $P unquoted.
// Neither shell does what that looks like. zsh does not word-split an
// unquoted expansion at all, so the whole string arrives as one token and the
// -p shorthand swallows the rest of it as its value; bash does split, but on
// every space, which tears an embedded 'kind: X' argument in half. The
// resulting error otherwise reads as a missing file with an absurd name,
// which sends the reader looking at their filesystem rather than their shell.
func glueHint(path string) string {
	if !strings.Contains(path, " -") {
		return ""
	}
	return "that is a whole command line, not a filename. A shell variable of " +
		"flags does not survive expansion in bash or zsh; put the fixed flags in a " +
		"function instead:\n" +
		"  drop() { viti talos patch -p @patch.yaml --mode staged \"$@\"; }\n" +
		"  drop -c my-cluster --dry-run"
}

func countNodes(sessions []planned) int {
	n := 0
	for _, p := range sessions {
		n += len(p.nodes)
	}
	return n
}

func orderingLabel(serial bool) string {
	if serial {
		return "one node at a time"
	}
	return "all nodes together"
}

// modeEffect renders the one-line consequence of an apply mode, so the plan
// says what will happen rather than only what was typed.
func modeEffect(mode string) string {
	switch mode {
	case "staged":
		return "written to disk, takes effect on next reboot; nothing restarts now"
	case "no-reboot":
		return "applied live; fails rather than rebooting"
	case "reboot":
		return "applied and every targeted node reboots"
	case "try":
		return "applied with a timeout, rolled back automatically"
	default:
		return "applied now, rebooting only the nodes whose change requires it"
	}
}

func indent(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if len(s) <= n {
		return s
	}
	if n <= 3 {
		return s[:n]
	}
	return s[:n-3] + "..."
}
