package talosctl

import (
	"strconv"
	"strings"

	"golang.org/x/mod/semver"
)

// UpgradeDirection classifies a Talos version transition against what Talos
// supports upgrading between.
//
// Two rules, both load-bearing. Talos upgrades one minor version at a time: a
// skipped minor misses the migrations the intervening release performs, and the
// upgrade either fails part way through a rolling reboot or leaves a node that
// boots but was never migrated. And Talos does not support going backwards,
// where the installed system is newer than the installer writing over it.
//
// Neither shows up in an installer reference, so without this a downgrade and a
// three-minor jump render exactly like the patch bump beside them.
type UpgradeDirection int

const (
	// DirectionUnknown means a version could not be read, so nothing can be
	// claimed about the transition. It is never a refusal: refusing on "cannot
	// tell" would block every cluster whose running version is unavailable,
	// which is the same mistake as skipping a node that would not answer.
	DirectionUnknown UpgradeDirection = iota
	// DirectionSame is a node already on the target.
	DirectionSame
	// DirectionPatch is forward within one minor.
	DirectionPatch
	// DirectionMinor is forward exactly one minor — the largest step Talos
	// takes in one go.
	DirectionMinor
	// DirectionSkip is forward by more than one minor, or across a major.
	DirectionSkip
	// DirectionDowngrade is backwards, by any amount.
	DirectionDowngrade
)

// Supported reports whether Talos upgrades between the two versions directly.
//
// DirectionUnknown counts as supported. An unreadable version is a reason to
// stay quiet, not a reason to block: the guard exists to catch a mistake that
// is legible, and turning it into a gate on data this command cannot always
// obtain would make it refuse the runs it is most needed for.
func (d UpgradeDirection) Supported() bool {
	switch d {
	case DirectionDowngrade, DirectionSkip:
		return false
	}
	return true
}

// String names the transition as a noun phrase, so it reads after an "is" —
// which is how every message about it is written. A verb phrase here renders
// as "v1.11.2 → v1.13.8 is skips a minor version".
func (d UpgradeDirection) String() string {
	switch d {
	case DirectionSame:
		return "already there"
	case DirectionPatch:
		return "a patch upgrade"
	case DirectionMinor:
		return "a one-minor upgrade"
	case DirectionSkip:
		return "a minor-version skip"
	case DirectionDowngrade:
		return "a downgrade"
	}
	return "not determined"
}

// ClassifyUpgrade compares the version a node runs against the version it
// would be upgraded onto.
//
// from must be what the node actually runs. Classifying against
// machine.install.image instead would be worse than not classifying at all:
// that field is a fossil of the original install which no upgrade writes back,
// so a cluster running v1.13.9 with configs reading v1.13.5 would have a
// genuine v1.13.9 → v1.13.9 no-op reported as an upgrade, and the reverse case
// would raise a downgrade that is not happening.
func ClassifyUpgrade(from, to string) UpgradeDirection {
	f, t := NormalizeVersion(from), NormalizeVersion(to)
	if !semver.IsValid(f) || !semver.IsValid(t) {
		return DirectionUnknown
	}
	// Both must name a patch. An under-specified version is not a legible
	// mistake, and semver resolves it in a direction this guard should not
	// inherit: MajorMinor("v1") is "v1.0", so "v1" against "v1.13.9" would be
	// refused as a thirteen-minor skip on the strength of a field nobody wrote.
	// Talos versions are always three-component, so requiring it costs nothing
	// and keeps the guard from refusing what it cannot actually read.
	if !namesAPatch(f) || !namesAPatch(t) {
		return DirectionUnknown
	}
	switch semver.Compare(f, t) {
	case 1:
		return DirectionDowngrade
	case 0:
		return DirectionSame
	}
	// Forward. A major change is a jump no Talos release has asked anyone to
	// make in one step, so it is treated as one.
	if semver.Major(f) != semver.Major(t) {
		return DirectionSkip
	}
	fromMinor, fromOK := minorOf(f)
	toMinor, toOK := minorOf(t)
	if !fromOK || !toOK {
		return DirectionUnknown
	}
	switch toMinor - fromMinor {
	case 0:
		return DirectionPatch
	case 1:
		return DirectionMinor
	}
	return DirectionSkip
}

// namesAPatch reports whether a version spells out major.minor.patch, as
// opposed to leaving the tail for semver to fill in.
func namesAPatch(v string) bool {
	core := v
	if i := strings.IndexAny(core, "-+"); i >= 0 {
		core = core[:i]
	}
	return strings.Count(core, ".") == 2
}

// minorOf pulls the minor number out of a version semver has already accepted.
func minorOf(v string) (int, bool) {
	_, minor, ok := strings.Cut(semver.MajorMinor(v), ".")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(minor)
	if err != nil {
		return 0, false
	}
	return n, true
}
