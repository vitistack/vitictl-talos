package cluster

import (
	"testing"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"
)

// The three sources spell versions differently and only one of them is
// reality, so each parser is pinned to the shape it actually meets.
func TestVersionParsers(t *testing.T) {
	for _, tc := range []struct {
		name  string
		parse func(string) string
		in    string
		want  string
	}{{
		name:  "declared, from a factory image URL",
		parse: VersionFromImageID,
		in:    "https://factory.talos.dev/image/b0f2a8b5/v1.13.9/nocloud-amd64.iso",
		want:  "1.13.9",
	}, {
		name:  "declared, prerelease",
		parse: VersionFromImageID,
		in:    "https://factory.talos.dev/image/b0f2a8b5/v1.14.0-beta.1/nocloud-amd64.iso",
		want:  "1.14.0-beta.1",
	}, {
		name:  "running, from a Node's osImage",
		parse: VersionFromOSImage,
		in:    "Talos (v1.13.7)",
		want:  "1.13.7",
	}, {
		// Not a Talos node: reporting a version here would put a foreign OS
		// into a Talos upgrade plan.
		name:  "running, non-Talos node",
		parse: VersionFromOSImage,
		in:    "Ubuntu 24.04.1 LTS",
		want:  "",
	}, {
		name:  "operator-verified, whole cluster",
		parse: VersionFromEnforcement,
		in:    "All nodes run Talos v1.12.7",
		want:  "1.12.7",
	}, {
		// Mid-upgrade the condition says something else entirely, and guessing
		// a version out of it would report a rolling cluster as settled.
		name:  "operator-verified, mid-upgrade",
		parse: VersionFromEnforcement,
		in:    "Upgrading nodes to Talos v1.13.8 (2/5 complete)",
		want:  "",
	}, {
		name:  "operator-verified, no condition message",
		parse: VersionFromEnforcement,
		in:    "",
		want:  "",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.parse(tc.in); got != tc.want {
				t.Errorf("parsed %q as %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// EnforcedVersion is read on error paths as much as on the happy path, so a
// cluster with no CR must not panic it.
func TestEnforcedVersionToleratesAnEmptyCluster(t *testing.T) {
	if got := EnforcedVersion(Cluster{}); got != "" {
		t.Errorf("EnforcedVersion of a zero Cluster = %q, want %q", got, "")
	}
}

// The condition type is a string match against what the operator publishes, so
// it is worth pinning: a typo here reads as "no cluster has ever been verified"
// rather than as a bug.
func TestEnforcedVersionReadsTheCondition(t *testing.T) {
	withConditions := func(conds ...vitiv1alpha1.KubernetesClusterCondition) Cluster {
		kc := &vitiv1alpha1.KubernetesCluster{}
		kc.Status.Conditions = conds
		return Cluster{KC: kc}
	}

	got := EnforcedVersion(withConditions(
		vitiv1alpha1.KubernetesClusterCondition{Type: "ClusterReady", Message: "All nodes run Talos v9.9.9"},
		vitiv1alpha1.KubernetesClusterCondition{
			Type: TalosVersionEnforcementCondition, Message: "All nodes run Talos v1.13.7"},
	))
	if got != "1.13.7" {
		t.Errorf("EnforcedVersion = %q, want %q — and it must not read another condition's message", got, "1.13.7")
	}

	// d-stackops-1010's shape: no such condition, which is why "viti kc list"
	// marks that cluster's version uncertain.
	if got := EnforcedVersion(withConditions(
		vitiv1alpha1.KubernetesClusterCondition{Type: "ClusterReady", Message: "ok"},
	)); got != "" {
		t.Errorf("EnforcedVersion with no enforcement condition = %q, want %q", got, "")
	}
}
