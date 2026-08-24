package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/vitistack/vitictl-talos/internal/cluster"
	"github.com/vitistack/vitictl-talos/internal/talosctl"
)

const (
	factoryBase = "factory.talos.dev/nocloud-installer/" +
		"b0f2a8b575460a3dcb1234cc081c73c88e795aaef36eda9b88a6f4dddbd49365"
	otherBase = "factory.talos.dev/metal-installer/" +
		"a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90"
)

// step builds one plan entry the way planNodeUpgrades does.
func step(name, role, declared, from, to string) upgradeStep {
	return upgradeStep{
		node:  cluster.Node{Name: name, Role: role, IP: "10.0.0.1", TalosVersion: declared},
		from:  from,
		image: to,
	}
}

// running marks a step with the version its node actually reports.
func running(s upgradeStep, version string) upgradeStep {
	s.state.Version = version
	return s
}

// onNode marks a step with the full state its node reports — the version and
// the schematic, which is what the skip decision needs both halves of.
func onNode(s upgradeStep, version, schematic string) upgradeStep {
	s.state = cluster.NodeState{Version: version, Schematic: schematic}
	return s
}

// The plan's job is to show what is about to change. The version installed now
// is that; the version spec.os.imageID declares is not, and standing in for it
// made a 1.12.7 → 1.13.8 upgrade read as a downgrade from 1.13.9.
func TestPlanShowsInstalledVersionNotDeclared(t *testing.T) {
	plan := []upgradeStep{
		step("t-x-ctp0", cluster.RoleControlPlane, "1.13.9", factoryBase+":v1.12.7", factoryBase+":v1.13.8"),
		step("t-x-wrk0", cluster.RoleWorker, "1.13.9", factoryBase+":v1.12.7", factoryBase+":v1.13.8"),
	}
	out := renderPlan(t, plan, pin{have: factoryBase + ":v1.12.7", want: factoryBase + ":v1.13.8"})

	if !strings.Contains(out, "v1.12.7 → v1.13.8") {
		t.Errorf("plan does not show the installed version moving to the target:\n%s", out)
	}
	// The lineage is identical on both sides of every arrow and of the pin, so
	// it belongs on one line rather than six times down the plan.
	if got := strings.Count(out, factoryBase); got != 1 {
		t.Errorf("expected the lineage named exactly once, got %d:\n%s", got, out)
	}
	if !strings.Contains(out, "pin:    install_image v1.12.7 → v1.13.8") {
		t.Errorf("pin line does not collapse onto the lineage above it:\n%s", out)
	}
	// With no running version the baseline is what the configs install, and a
	// declared version ahead of it is exactly what this note exists for.
	if !strings.Contains(out, "configs install v1.12.7, but machines declare v1.13.9 in spec.os.imageID") ||
		!strings.Contains(out, "does not track what booted") {
		t.Errorf("plan does not name the declared/installed disagreement:\n%s", out)
	}
	if !strings.Contains(out, "current: what each node's config installs") {
		t.Errorf("plan does not say the column is desired state, not what booted:\n%s", out)
	}
	if strings.Contains(out, "declared v1.13.9 → ") {
		t.Errorf("plan still presents the declared version as the one being upgraded from:\n%s", out)
	}
}

// A declared version that agrees with what is installed is not news, and
// saying it every run trains the reader to skip the line that matters.
func TestPlanStaysQuietWhenDeclaredAgrees(t *testing.T) {
	plan := []upgradeStep{
		step("t-x-ctp0", cluster.RoleControlPlane, "1.12.7", factoryBase+":v1.12.7", factoryBase+":v1.13.8"),
	}
	if out := renderPlan(t, plan, pin{}); strings.Contains(out, "note:") {
		t.Errorf("plan notes a disagreement that does not exist:\n%s", out)
	}
}

// Nodes on different lineages are exactly the case the per-node resolution
// exists for, so the full references have to be visible rather than collapsed
// into a version column that hides which schematic each node keeps.
func TestPlanPrintsFullRefsWhenNodesDisagree(t *testing.T) {
	plan := []upgradeStep{
		step("t-x-ctp0", cluster.RoleControlPlane, "", factoryBase+":v1.12.7", factoryBase+":v1.13.8"),
		step("t-x-wrk0", cluster.RoleWorker, "", otherBase+":v1.12.7", otherBase+":v1.13.8"),
	}
	out := renderPlan(t, plan, pin{})
	for _, want := range []string{factoryBase + ":v1.12.7", factoryBase + ":v1.13.8", otherBase + ":v1.13.8"} {
		if !strings.Contains(out, want) {
			t.Errorf("plan omits %s:\n%s", want, out)
		}
	}
}

// --image never reads the node's own installer, so the plan must not imply it
// knows what each node is on.
func TestPlanSaysCurrentInstallerWasNotReadUnderImage(t *testing.T) {
	plan := []upgradeStep{
		step("t-x-ctp0", cluster.RoleControlPlane, "1.13.9", "", factoryBase+":v1.13.8"),
		step("t-x-wrk0", cluster.RoleWorker, "1.13.9", "", factoryBase+":v1.13.8"),
	}
	out := renderPlan(t, plan, pin{})
	if !strings.Contains(out, "--image does not read") {
		t.Errorf("plan does not say the current installer is unknown:\n%s", out)
	}
	// The declared version is the only one left, so it is shown — and named as
	// what it is, since --image read no config and the nodes were not asked.
	if !strings.Contains(out, "v1.13.9") ||
		!strings.Contains(out, "current: what each machine declares in spec.os.imageID") {
		t.Errorf("plan drops or mislabels the only version it does know:\n%s", out)
	}
	if strings.Contains(out, "note:") {
		t.Errorf("plan compares against an installed version it never read:\n%s", out)
	}
}

func renderPlan(t *testing.T, plan []upgradeStep, p pin) string {
	t.Helper()
	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetErr(&buf)
	printUpgradePlan(cmd, cluster.Cluster{}, plan, false, p, skipPolicy{enabled: true})
	return buf.String()
}

// Rendering the operator's real scenario, kept as a golden-ish eyeball test.
func TestPlanRendersRealScenario(t *testing.T) {
	names := []string{"t-jraviti-123-vexr-ctp0", "t-jraviti-123-vexr-wrk0",
		"t-jraviti-123-vexr-wrk1", "t-jraviti-123-vexr-wrk2", "t-jraviti-123-vexr-wrk3"}
	plan := make([]upgradeStep, 0, len(names))
	for i, n := range names {
		role := cluster.RoleWorker
		if i == 0 {
			role = cluster.RoleControlPlane
		}
		plan = append(plan, step(n, role, "1.13.9", factoryBase+":v1.12.7", factoryBase+":v1.13.8"))
	}
	t.Log("\n" + renderPlan(t, plan, pin{have: factoryBase + ":v1.12.7", want: factoryBase + ":v1.13.8"}))
}

// A pin sitting on a different schematic than the nodes carry is the case the
// pin line exists to catch — collapsing it to a version would hide exactly the
// disagreement worth seeing.
func TestPinPrintedInFullWhenOffTheLineage(t *testing.T) {
	plan := []upgradeStep{
		step("t-x-ctp0", cluster.RoleControlPlane, "", factoryBase+":v1.12.7", factoryBase+":v1.13.8"),
	}
	out := renderPlan(t, plan, pin{have: otherBase + ":v1.12.7", want: factoryBase + ":v1.13.8"})
	if !strings.Contains(out, "install_image "+otherBase+":v1.12.7 → "+factoryBase+":v1.13.8") {
		t.Errorf("pin line hides that the pin is on another lineage:\n%s", out)
	}
}

// A failure mid-roll leaves a node cordoned and drained, because talosctl
// uncordons only on success. Nothing else on screen says so.
func TestUpgradeFailureWarnsAboutTheCordon(t *testing.T) {
	out, _ := renderFailure(t, []string{"--to", "v1.13.8"}, upgradeFailure{
		stage: false, noDrain: false,
	})
	if !strings.Contains(out, "may be left cordoned") {
		t.Errorf("failure does not mention the cordon:\n%s", out)
	}
	if !strings.Contains(out, "kubectl uncordon t-jraviti-123-vexr-wrk3") {
		t.Errorf("failure does not say how to clear the cordon:\n%s", out)
	}
}

// --stage does not reboot and --no-drain asked talosctl not to cordon, so
// warning about a cordon in either case is noise that teaches the reader to
// ignore the warning when it is real.
func TestUpgradeFailureStaysQuietWhenNothingWasDrained(t *testing.T) {
	for _, f := range []upgradeFailure{{stage: true}, {noDrain: true}} {
		out, _ := renderFailure(t, []string{"--to", "v1.13.8"}, f)
		if strings.Contains(out, "cordoned") {
			t.Errorf("failure warns about a cordon that was never taken:\n%s", out)
		}
	}
}

// Re-running the whole cluster to reach one node reboots every node that
// already succeeded, so the resume command must name only what is left.
func TestResumeCommandNamesOnlyTheRemainingNodes(t *testing.T) {
	out, resume := renderFailure(t, []string{"--to", "v1.13.8", "--stage"}, upgradeFailure{stage: true})

	if !strings.Contains(resume, "-N t-jraviti-123-vexr-wrk3 -N t-jraviti-123-vexr-wrk4") {
		t.Errorf("resume does not name the remaining nodes: %q", resume)
	}
	if strings.Contains(resume, "ctp0") {
		t.Errorf("resume would reboot a node that already succeeded: %q", resume)
	}
	// Flags the run was given have to survive, or the resume upgrades onto a
	// different image than the run it is continuing.
	if !strings.Contains(resume, "--to=v1.13.8") || !strings.Contains(resume, "--stage") {
		t.Errorf("resume drops flags the original run used: %q", resume)
	}
	if !strings.Contains(out, resume) {
		t.Errorf("resume command was not printed:\n%s", out)
	}
}

// The pin moved to the target before any node was touched, so the reader needs
// to know resuming will not fight the operator.
func TestUpgradeFailureReportsThePinState(t *testing.T) {
	out, _ := renderFailure(t, []string{"--to", "v1.13.8"}, upgradeFailure{
		pin: pin{have: factoryBase + ":v1.12.7", want: factoryBase + ":v1.13.8"},
	})
	if !strings.Contains(out, "already pinned to the target") ||
		!strings.Contains(out, factoryBase+":v1.13.8") {
		t.Errorf("failure does not say the pin already holds the target:\n%s", out)
	}

	out, _ = renderFailure(t, []string{"--to", "v1.13.8"}, upgradeFailure{
		pin: pin{have: factoryBase + ":v1.12.7", skipped: true},
	})
	if !strings.Contains(out, "--no-pin") || !strings.Contains(out, "will revert") {
		t.Errorf("failure does not warn that --no-pin loses the nodes that succeeded:\n%s", out)
	}
}

// A cluster name or node name the shell would resplit has to come back quoted,
// since the command is meant to be pasted and run.
func TestResumeCommandQuotesAwkwardValues(t *testing.T) {
	cmd := newUpgradeNodeCmd(&scope{})
	cmd.SetErr(&bytes.Buffer{})
	resume := resumeCommand(cmd, upgradeFailure{
		clusterArg: "my cluster",
		remaining:  []upgradeStep{step("t-x wrk0", cluster.RoleWorker, "", "", "")},
	})
	if !strings.Contains(resume, "'my cluster'") || !strings.Contains(resume, "'t-x wrk0'") {
		t.Errorf("resume leaves values the shell would resplit unquoted: %q", resume)
	}
}

// renderFailure runs printUpgradeFailure over a five-node plan that failed on
// its fourth node, returning the output and the resume command inside it.
func renderFailure(t *testing.T, argv []string, f upgradeFailure) (out, resume string) {
	t.Helper()
	var buf bytes.Buffer
	cmd := newUpgradeNodeCmd(&scope{})
	cmd.SetErr(&buf)
	cmd.SetOut(&buf)
	if err := cmd.ParseFlags(argv); err != nil {
		t.Fatalf("parsing %v: %v", argv, err)
	}

	f.remaining = []upgradeStep{
		step("t-jraviti-123-vexr-wrk3", cluster.RoleWorker, "", factoryBase+":v1.12.7", factoryBase+":v1.13.8"),
		step("t-jraviti-123-vexr-wrk4", cluster.RoleWorker, "", factoryBase+":v1.12.7", factoryBase+":v1.13.8"),
	}
	f.done, f.total = 3, 5
	f.clusterArg = "t-jraviti-123-vexr"

	printUpgradeFailure(cmd, f)
	return buf.String(), resumeCommand(cmd, f)
}

// The operator's real failure, kept as an eyeball test.
func TestUpgradeFailureRendersRealScenario(t *testing.T) {
	out, _ := renderFailure(t, []string{"--to", "v1.13.8"}, upgradeFailure{
		pin: pin{have: factoryBase + ":v1.12.7", want: factoryBase + ":v1.13.8"},
	})
	t.Log("\n" + out)
}

// The running version is the only one of the three that is not a wish, so when
// it is available it is what the rows show and what everything is compared
// against.
func TestPlanPrefersTheRunningVersion(t *testing.T) {
	// d-stackops-1010: four nodes on v1.13.7, machines declaring v1.13.9.
	plan := []upgradeStep{
		running(step("d-stackops-1010-qjxq-ctp0", cluster.RoleControlPlane, "1.13.9",
			factoryBase+":v1.13.9", factoryBase+":v1.13.9"), "1.13.7"),
		running(step("d-stackops-1010-qjxq-wrk0", cluster.RoleWorker, "1.13.9",
			factoryBase+":v1.13.9", factoryBase+":v1.13.9"), "1.13.7"),
	}
	out := renderPlan(t, plan, pin{})

	if !strings.Contains(out, "v1.13.7 → v1.13.9") {
		t.Errorf("rows do not start from what the nodes run:\n%s", out)
	}
	if !strings.Contains(out, "current: what each node runs now, from the guest cluster") {
		t.Errorf("plan does not say the column is the running version:\n%s", out)
	}
	// Both other sources say v1.13.9 while the nodes run v1.13.7. Naming both
	// is the point: either one alone would make this look like a no-op.
	if !strings.Contains(out, "nodes run v1.13.7, but machines declare v1.13.9 in spec.os.imageID and configs install v1.13.9") {
		t.Errorf("plan does not name both disagreeing sources:\n%s", out)
	}
}

// A guest cluster that answered for some nodes and not others must not have the
// gap smoothed over — the count is what decides whether to trust the column.
func TestPlanCountsNodesThatFellBack(t *testing.T) {
	plan := []upgradeStep{
		running(step("t-x-ctp0", cluster.RoleControlPlane, "", factoryBase+":v1.12.7", factoryBase+":v1.13.8"), "1.12.7"),
		step("t-x-wrk0", cluster.RoleWorker, "", factoryBase+":v1.12.7", factoryBase+":v1.13.8"),
	}
	if out := renderPlan(t, plan, pin{}); !strings.Contains(out, "except 1 of 2 falling back to desired state") {
		t.Errorf("plan hides that one node's running version is unknown:\n%s", out)
	}
}

// An installer reference records what was asked for, not what booted, so the
// running version needs its own line in the full-reference form.
func TestPlanShowsRunningVersionAlongsideFullRefs(t *testing.T) {
	plan := []upgradeStep{
		running(step("t-x-ctp0", cluster.RoleControlPlane, "", factoryBase+":v1.12.7", otherBase+":v1.13.8"), "1.12.7"),
	}
	if out := renderPlan(t, plan, pin{}); !strings.Contains(out, "runs  v1.12.7") {
		t.Errorf("full-reference rows drop the running version:\n%s", out)
	}
}

// Everything in agreement is not news, and a note printed every run trains the
// reader to skip the one that matters.
func TestPlanStaysQuietWhenEverySourceAgrees(t *testing.T) {
	plan := []upgradeStep{
		running(step("t-x-ctp0", cluster.RoleControlPlane, "1.12.7",
			factoryBase+":v1.12.7", factoryBase+":v1.13.8"), "1.12.7"),
	}
	if out := renderPlan(t, plan, pin{}); strings.Contains(out, "note:") {
		t.Errorf("plan notes a disagreement that does not exist:\n%s", out)
	}
}

// d-stackops-1010 rendered end to end, kept as an eyeball test.
func TestPlanRendersStackopsScenario(t *testing.T) {
	names := []string{"d-stackops-1010-qjxq-ctp0", "d-stackops-1010-qjxq-wrk0",
		"d-stackops-1010-qjxq-wrk1", "d-stackops-1010-qjxq-wrk2"}
	plan := make([]upgradeStep, 0, len(names))
	for i, n := range names {
		role := cluster.RoleWorker
		if i == 0 {
			role = cluster.RoleControlPlane
		}
		plan = append(plan, running(step(n, role, "1.13.9",
			factoryBase+":v1.13.7", factoryBase+":v1.13.9"), "1.13.7"))
	}
	t.Log("\n" + renderPlan(t, plan, pin{have: factoryBase + ":v1.13.7", want: factoryBase + ":v1.13.9"}))
}

const schematicB = "b0f2a8b575460a3dcb1234cc081c73c88e795aaef36eda9b88a6f4dddbd49365"

// A node running exactly what the upgrade would install has nothing to gain
// from a reboot. This is the d-stackops-1010 case: four healthy nodes on
// v1.13.9, a plan reading "v1.13.9 → v1.13.9", and every one of them rebooted.
func TestSkipsNodeAlreadyOnTarget(t *testing.T) {
	p := newSkipPolicy(talosctl.ImageEdit{Version: "v1.13.9"}, "", false)
	s := onNode(step("t-x-ctp0", cluster.RoleControlPlane, "", factoryBase+":v1.13.5",
		factoryBase+":v1.13.9"), "1.13.9", schematicB)

	if !p.alreadyOnTarget(s) {
		t.Error("node running the target version and schematic was not recognised as already there")
	}
}

// The safety property this whole design turns on. A node that could not be read
// is the node a half-finished run left behind — it is NotReady, which is exactly
// why it did not answer. Reading that silence as "already upgraded" would strand
// the one node the command was run for.
func TestNeverSkipsWhatItCannotVerify(t *testing.T) {
	p := newSkipPolicy(talosctl.ImageEdit{Version: "v1.13.9"}, "", false)
	target := factoryBase + ":v1.13.9"

	for _, tc := range []struct {
		name               string
		version, schematic string
	}{
		{"node did not answer at all", "", ""},
		{"version known, schematic absent", "1.13.9", ""},
		{"schematic known, version absent", "", schematicB},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := onNode(step("t-x-wrk3", cluster.RoleWorker, "", factoryBase+":v1.13.5", target),
				tc.version, tc.schematic)
			if p.alreadyOnTarget(s) {
				t.Error("skipped a node whose state could not be fully verified")
			}
		})
	}
}

// Matching the version is not enough: --schematic changes the extensions while
// leaving the version alone, so "v1.13.9 → v1.13.9" can be real work.
func TestDoesNotSkipASchematicChange(t *testing.T) {
	other := "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90"
	p := newSkipPolicy(talosctl.ImageEdit{Schematic: other}, "", false)
	s := onNode(step("t-x-ctp0", cluster.RoleControlPlane, "", factoryBase+":v1.13.9",
		"factory.talos.dev/nocloud-installer/"+other+":v1.13.9"), "1.13.9", schematicB)

	if p.alreadyOnTarget(s) {
		t.Error("skipped a node whose schematic is being changed — same version, different extensions")
	}
}

// A Node object reports its version and its schematic and nothing else. An edit
// that moves a part the node cannot report is unverifiable, and --platform is
// the one where a wrong skip does not fail loudly: the node stays Ready with its
// config source silently gone.
func TestSkippingIsOffForUnverifiableEdits(t *testing.T) {
	for _, tc := range []struct {
		name  string
		edit  talosctl.ImageEdit
		image string
		force bool
		want  string
	}{
		{name: "platform", edit: talosctl.ImageEdit{Platform: "hcloud"}, want: "--platform"},
		{name: "registry", edit: talosctl.ImageEdit{Registry: "mirror.local"}, want: "--registry"},
		{name: "whole image replaced", image: factoryBase + ":v1.13.9", want: "--image"},
		{name: "force", edit: talosctl.ImageEdit{Version: "v1.13.9"}, force: true, want: "--force"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newSkipPolicy(tc.edit, tc.image, tc.force)
			if p.enabled {
				t.Fatalf("skipping is enabled for %s, which cannot be checked against a Node object", tc.name)
			}
			if !strings.Contains(p.reason, tc.want) {
				t.Errorf("reason %q does not name %s", p.reason, tc.want)
			}
			// Even a node that plainly matches must not be skipped here.
			s := onNode(step("t-x-ctp0", cluster.RoleControlPlane, "", factoryBase+":v1.13.9",
				factoryBase+":v1.13.9"), "1.13.9", schematicB)
			if p.alreadyOnTarget(s) {
				t.Error("a disabled policy skipped a node anyway")
			}
		})
	}
}

// The plan must not claim to be upgrading nodes it will not touch, and must say
// which ones those are.
func TestPlanSeparatesWorkFromNodesAlreadyThere(t *testing.T) {
	done := onNode(step("t-x-ctp0", cluster.RoleControlPlane, "", factoryBase+":v1.13.5",
		factoryBase+":v1.13.9"), "1.13.9", schematicB)
	done.already = true
	pending := onNode(step("t-x-wrk0", cluster.RoleWorker, "", factoryBase+":v1.13.5",
		factoryBase+":v1.13.9"), "1.13.7", schematicB)

	plan := []upgradeStep{done, pending}
	if got := len(todo(plan)); got != 1 {
		t.Fatalf("todo() returned %d steps, want 1", got)
	}

	out := renderPlan(t, plan, pin{})
	if !strings.Contains(out, "Upgrading 1 of 2 node(s)") {
		t.Errorf("plan claims to upgrade nodes it will not touch:\n%s", out)
	}
	if !strings.Contains(out, "✓ t-x-ctp0") || !strings.Contains(out, "already on target, not touched") {
		t.Errorf("plan does not mark the node it is skipping:\n%s", out)
	}
	if !strings.Contains(out, "• t-x-wrk0") {
		t.Errorf("plan does not mark the node it will act on:\n%s", out)
	}
}

// A whole cluster already there should say so plainly rather than announcing an
// upgrade of zero nodes.
func TestPlanSaysWhenThereIsNothingToDo(t *testing.T) {
	s := onNode(step("t-x-ctp0", cluster.RoleControlPlane, "", factoryBase+":v1.13.5",
		factoryBase+":v1.13.9"), "1.13.9", schematicB)
	s.already = true

	out := renderPlan(t, []upgradeStep{s}, pin{})
	if !strings.Contains(out, "All 1 targeted node(s)") || !strings.Contains(out, "already run the target") {
		t.Errorf("plan does not say the cluster has arrived:\n%s", out)
	}
	if strings.Contains(out, "effect:") {
		t.Errorf("plan describes the effect of an upgrade that will not happen:\n%s", out)
	}
}

// Skipping being off is a fact about the run, and a fleet of no-op reboots must
// never look deliberate by omission.
func TestPlanStatesWhySkippingIsOff(t *testing.T) {
	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetErr(&buf)
	plan := []upgradeStep{onNode(step("t-x-ctp0", cluster.RoleControlPlane, "",
		factoryBase+":v1.13.9", factoryBase+":v1.13.9"), "1.13.9", schematicB)}

	printUpgradePlan(cmd, cluster.Cluster{}, plan, false, pin{},
		newSkipPolicy(talosctl.ImageEdit{Platform: "hcloud"}, "", false))

	if out := buf.String(); !strings.Contains(out, "skip:   off — --platform") {
		t.Errorf("plan does not say why nothing is being skipped:\n%s", out)
	}
}

// d-stackops-1010 the second time round: the plan that started this change.
func TestPlanRendersStackopsAlreadyThere(t *testing.T) {
	names := []string{"d-stackops-1010-qjxq-ctp0", "d-stackops-1010-qjxq-wrk0",
		"d-stackops-1010-qjxq-wrk1", "d-stackops-1010-qjxq-wrk2"}
	policy := newSkipPolicy(talosctl.ImageEdit{Version: "v1.13.9"}, "", false)
	plan := make([]upgradeStep, 0, len(names))
	for i, n := range names {
		role := cluster.RoleWorker
		if i == 0 {
			role = cluster.RoleControlPlane
		}
		s := onNode(step(n, role, "1.13.9", factoryBase+":v1.13.5", factoryBase+":v1.13.9"),
			"1.13.9", schematicB)
		s.already = policy.alreadyOnTarget(s)
		plan = append(plan, s)
	}
	if got := len(todo(plan)); got != 0 {
		t.Fatalf("%d node(s) would still be rebooted", got)
	}
	t.Log("\n" + renderPlan(t, plan, pin{have: factoryBase + ":v1.13.9", want: factoryBase + ":v1.13.9"}))
}
