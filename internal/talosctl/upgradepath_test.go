package talosctl

import "testing"

// The two transitions Talos refuses are invisible in an installer reference, so
// the classification is the only thing standing between an operator and a
// rolling reboot that cannot succeed.
func TestClassifyUpgrade(t *testing.T) {
	for _, tc := range []struct {
		name      string
		from, to  string
		want      UpgradeDirection
		supported bool
	}{{
		// The operator's first run: 1.12.7 → 1.13.8, which the old plan
		// rendered as "declared v1.13.9 → v1.13.8" and looked like a downgrade.
		name: "one minor forward", from: "1.12.7", to: "v1.13.8",
		want: DirectionMinor, supported: true,
	}, {
		name: "patch forward", from: "1.13.5", to: "v1.13.9",
		want: DirectionPatch, supported: true,
	}, {
		name: "already there", from: "1.13.9", to: "v1.13.9",
		want: DirectionSame, supported: true,
	}, {
		name: "patch downgrade", from: "1.13.9", to: "v1.13.8",
		want: DirectionDowngrade, supported: false,
	}, {
		name: "minor downgrade", from: "1.13.9", to: "v1.12.7",
		want: DirectionDowngrade, supported: false,
	}, {
		name: "two minors forward", from: "1.11.2", to: "v1.13.8",
		want: DirectionSkip, supported: false,
	}, {
		name: "across a major", from: "1.13.9", to: "v2.0.0",
		want: DirectionSkip, supported: false,
	}, {
		name: "into a prerelease one minor on", from: "1.13.9", to: "v1.14.0-beta.1",
		want: DirectionMinor, supported: true,
	}, {
		name: "out of a prerelease to its release", from: "1.14.0-beta.1", to: "v1.14.0",
		want: DirectionPatch, supported: true,
	}, {
		// Backwards out of a release into its own prerelease is still backwards.
		name: "release back to prerelease", from: "1.14.0", to: "v1.14.0-beta.1",
		want: DirectionDowngrade, supported: false,
	}, {
		// The running version was never read. Refusing here would block the
		// clusters this command exists for.
		name: "running version unknown", from: "", to: "v1.13.9",
		want: DirectionUnknown, supported: true,
	}, {
		name: "target unreadable", from: "1.13.7", to: "",
		want: DirectionUnknown, supported: true,
	}, {
		name: "garbage", from: "latest", to: "v1.13.9",
		want: DirectionUnknown, supported: true,
	}, {
		// semver would fill the missing tail in as v1.0.0 and call this a
		// thirteen-minor skip. An under-specified version is not a legible
		// mistake, so the guard declines to read one rather than refusing on it.
		name: "no minor component", from: "v1", to: "v1.13.9",
		want: DirectionUnknown, supported: true,
	}, {
		name: "no patch component", from: "v1.13", to: "v1.14.0",
		want: DirectionUnknown, supported: true,
	}, {
		name: "build metadata is not a missing patch", from: "1.13.5+abc", to: "v1.13.9",
		want: DirectionPatch, supported: true,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyUpgrade(tc.from, tc.to)
			if got != tc.want {
				t.Errorf("ClassifyUpgrade(%q, %q) = %v (%s), want %v (%s)",
					tc.from, tc.to, got, got, tc.want, tc.want)
			}
			if got.Supported() != tc.supported {
				t.Errorf("%v.Supported() = %v, want %v", got, got.Supported(), tc.supported)
			}
		})
	}
}
