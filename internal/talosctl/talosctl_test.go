package talosctl

import (
	"strings"
	"testing"
)

// The whole point of this package is that a fleet-wide command is pointed
// exactly where it claims to be. These assert the argv, since that is the only
// thing that decides which cluster gets written to.
func TestBuildAlwaysPassesTalosconfigAndNodes(t *testing.T) {
	got, err := build(Target{
		Talosconfig: "/tmp/tc",
		Endpoints:   []string{"10.0.0.1", "10.0.0.2"},
		Nodes:       []string{"10.0.0.3"},
	}, "dmesg")
	if err != nil {
		t.Fatalf("build() error: %v", err)
	}
	want := []string{"dmesg", "--talosconfig", "/tmp/tc", "--nodes", "10.0.0.3", "--endpoints", "10.0.0.1,10.0.0.2"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("build() = %v\nwant %v", got, want)
	}
}

func TestBuildSplitsMultiWordSubcommands(t *testing.T) {
	got, err := build(Target{Talosconfig: "/tmp/tc", Nodes: []string{"n"}}, "patch machineconfig", "-p", "@p.yaml")
	if err != nil {
		t.Fatalf("build() error: %v", err)
	}
	if got[0] != "patch" || got[1] != "machineconfig" {
		t.Errorf("build() did not split the subcommand: %v", got)
	}
	if got[len(got)-2] != "-p" || got[len(got)-1] != "@p.yaml" {
		t.Errorf("build() dropped the extra args: %v", got)
	}
}

// Falling back to the ambient TALOSCONFIG is exactly the "act on whatever
// cluster the shell was pointing at" accident this plugin exists to prevent,
// so an empty target is an error rather than a default.
func TestBuildRefusesAnUnpointedTarget(t *testing.T) {
	if _, err := build(Target{Nodes: []string{"n"}}, "dmesg"); err == nil {
		t.Error("build() accepted a target with no talosconfig")
	}
	if _, err := build(Target{Talosconfig: "/tmp/tc"}, "dmesg"); err == nil {
		t.Error("build() accepted a target with no nodes")
	}
	if _, err := build(Target{Talosconfig: "/tmp/tc", Nodes: []string{"n"}}, "  "); err == nil {
		t.Error("build() accepted an empty subcommand")
	}
}

func TestCommandLineIsPasteable(t *testing.T) {
	got := CommandLine(Target{Talosconfig: "/tmp/tc", Nodes: []string{"10.0.0.3"}}, "upgrade", "--image", "x")
	want := "talosctl upgrade --talosconfig /tmp/tc --nodes 10.0.0.3 --image x"
	if got != want {
		t.Errorf("CommandLine() = %q\nwant %q", got, want)
	}
}

func TestSwapImageTag(t *testing.T) {
	cases := []struct{ ref, tag, want string }{
		{"factory.talos.dev/nocloud-installer/abc123:v1.13.7", "v1.13.9",
			"factory.talos.dev/nocloud-installer/abc123:v1.13.9"},
		{"ghcr.io/siderolabs/installer", "v1.13.9", "ghcr.io/siderolabs/installer:v1.13.9"},
		// A registry port must not be mistaken for a tag separator.
		{"localhost:5000/installer", "v1.13.9", "localhost:5000/installer:v1.13.9"},
		{"localhost:5000/installer:v1.0.0", "v1.13.9", "localhost:5000/installer:v1.13.9"},
	}
	for _, c := range cases {
		if got := SwapImageTag(c.ref, c.tag); got != c.want {
			t.Errorf("SwapImageTag(%q, %q) = %q, want %q", c.ref, c.tag, got, c.want)
		}
	}
}

func TestNormalizeVersion(t *testing.T) {
	cases := map[string]string{"1.13.9": "v1.13.9", "v1.13.9": "v1.13.9", " 1.13.9 ": "v1.13.9", "": ""}
	for in, want := range cases {
		if got := NormalizeVersion(in); got != want {
			t.Errorf("NormalizeVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

// The installer image is read from the node rather than constructed, because
// it carries both the factory schematic and the platform variant.
func TestInstallImageFromConfig(t *testing.T) {
	raw := `node: 10.0.0.3
metadata:
    namespace: config
    type: MachineConfigs.config.talos.dev
    id: v1alpha1
spec:
    version: v1alpha1
    machine:
        type: controlplane
        install:
            disk: /dev/vda
            image: factory.talos.dev/nocloud-installer/b0f2a8b5:v1.13.9
    cluster:
        id: abc
`
	got, err := installImageFromConfig(raw)
	if err != nil {
		t.Fatalf("installImageFromConfig() error: %v", err)
	}
	if got != "factory.talos.dev/nocloud-installer/b0f2a8b5:v1.13.9" {
		t.Errorf("installImageFromConfig() = %q", got)
	}
}

func TestInstallImageFromConfigSkipsDocumentsWithoutOne(t *testing.T) {
	raw := `node: 10.0.0.3
spec:
    machine:
        type: worker
---
node: 10.0.0.4
spec:
    machine:
        install:
            image: factory.talos.dev/metal-installer/abc:v1.13.9
`
	got, err := installImageFromConfig(raw)
	if err != nil {
		t.Fatalf("installImageFromConfig() error: %v", err)
	}
	if got != "factory.talos.dev/metal-installer/abc:v1.13.9" {
		t.Errorf("installImageFromConfig() = %q", got)
	}
}

// Guessing an installer image is worse than refusing: the wrong one strands
// the upgrade or silently flips the node's platform.
func TestInstallImageFromConfigRefusesToGuess(t *testing.T) {
	_, err := installImageFromConfig("node: 10.0.0.3\nspec:\n    machine:\n        type: worker\n")
	if err == nil {
		t.Fatal("installImageFromConfig() invented an image for a config that declares none")
	}
	if !strings.Contains(err.Error(), "--image") {
		t.Errorf("error does not point at the way out: %v", err)
	}
}

func TestSplitYAMLDocuments(t *testing.T) {
	got := splitYAMLDocuments("a: 1\n---\nb: 2\n")
	if len(got) != 2 {
		t.Fatalf("splitYAMLDocuments() = %d docs, want 2: %q", len(got), got)
	}
	if !strings.Contains(got[0], "a: 1") || !strings.Contains(got[1], "b: 2") {
		t.Errorf("splitYAMLDocuments() = %q", got)
	}
}
