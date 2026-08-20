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

// realTalosctlOutput mirrors what `talosctl get machineconfig -o yaml` actually
// prints: the spec is a YAML block scalar holding the config as text, and that
// text is itself multi-document. An earlier fixture here invented a nested
// mapping instead, which no Talos version emits — so the parser passed its
// tests and failed against every real node. The shape is the whole point of
// this fixture; the values are dummies.
const realTalosctlOutput = `node: 10.0.0.3
metadata:
    namespace: config
    type: MachineConfigs.config.talos.dev
    id: v1alpha1
    version: 1
    owner:
    phase: running
    annotations:
        talos.dev/yaml-spec: 1
spec: |
    version: v1alpha1
    debug: false
    persist: true
    machine:
        type: controlplane
        token: dummy.token
        network: {}
        install:
            disk: /dev/vda
            image: factory.talos.dev/nocloud-installer/b0f2a8b5:v1.12.7
            wipe: false
    cluster:
        id: dummy
    ---
    apiVersion: v1alpha1
    kind: DHCPv4Config
    name: net0
    clientIdentifier: mac
`

// The installer image is read from the node rather than constructed, because
// it carries both the factory schematic and the platform variant.
func TestInstallImageFromRealTalosctlOutput(t *testing.T) {
	got, err := installImageFromConfig(realTalosctlOutput)
	if err != nil {
		t.Fatalf("installImageFromConfig() error: %v", err)
	}
	if got != "factory.talos.dev/nocloud-installer/b0f2a8b5:v1.12.7" {
		t.Errorf("installImageFromConfig() = %q", got)
	}
}

// The nested-mapping shape is still accepted, so a talosctl that renders the
// spec structurally does not regress.
func TestInstallImageFromNestedSpec(t *testing.T) {
	raw := `node: 10.0.0.3
metadata:
    id: v1alpha1
spec:
    version: v1alpha1
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

// A bare config with no resource envelope at all.
func TestInstallImageFromBareConfig(t *testing.T) {
	raw := "version: v1alpha1\nmachine:\n    install:\n        image: ghcr.io/siderolabs/installer:v1.13.9\n"
	got, err := installImageFromConfig(raw)
	if err != nil {
		t.Fatalf("installImageFromConfig() error: %v", err)
	}
	if got != "ghcr.io/siderolabs/installer:v1.13.9" {
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

// The bug this guards: talosctl writes "WARNING: <ip>: server version X is
// older than client version Y" to stderr on every call to an older node.
// Merged into the document it is not valid YAML, and the parser then reports
// machine.install.image as missing from a config that plainly has it — which
// is what happened against every real node until the streams were separated.
func TestInstallImageSurvivesAPrependedAdvisory(t *testing.T) {
	raw := "WARNING: 10.0.0.3: server version 1.13.4 is older than client version 1.13.9\n" +
		realTalosctlOutput
	got, err := installImageFromConfig(raw)
	if err != nil {
		t.Fatalf("a prepended advisory defeated the parser: %v", err)
	}
	if got != "factory.talos.dev/nocloud-installer/b0f2a8b5:v1.12.7" {
		t.Errorf("installImageFromConfig() = %q", got)
	}
}

func TestStripAdvisoriesKeepsTheDocument(t *testing.T) {
	// Only leading advisories go; a WARNING inside the payload is data.
	in := "WARNING: a\nNOTICE: b\nnode: 1.2.3.4\nspec: |\n    WARNING: not an advisory\n"
	got := stripAdvisories(in)
	if strings.HasPrefix(got, "WARNING") {
		t.Errorf("leading advisories not stripped: %q", got)
	}
	if !strings.Contains(got, "WARNING: not an advisory") {
		t.Errorf("stripAdvisories ate part of the document: %q", got)
	}
	if unchanged := stripAdvisories("node: 1.2.3.4\n"); unchanged != "node: 1.2.3.4\n" {
		t.Errorf("stripAdvisories changed a clean document: %q", unchanged)
	}
}

func TestRewriteImageChangesOnlyTheNamedParts(t *testing.T) {
	const (
		oldSchematic = "b0f2a8b575460a3dcb1234cc081c73c88e795aaef36eda9b88a6f4dddbd49365"
		newSchematic = "1111111111111111111111111111111111111111111111111111111111111111"
		ref          = "factory.talos.dev/nocloud-installer/" + oldSchematic + ":v1.13.2"
	)
	cases := []struct {
		name string
		edit ImageEdit
		want string
	}{
		{
			name: "version only",
			edit: ImageEdit{Version: "1.13.9"},
			want: "factory.talos.dev/nocloud-installer/" + oldSchematic + ":v1.13.9",
		},
		{
			name: "schematic only",
			edit: ImageEdit{Schematic: newSchematic},
			want: "factory.talos.dev/nocloud-installer/" + newSchematic + ":v1.13.2",
		},
		{
			name: "platform only",
			edit: ImageEdit{Platform: "hcloud"},
			want: "factory.talos.dev/hcloud-installer/" + oldSchematic + ":v1.13.2",
		},
		{
			name: "registry only",
			edit: ImageEdit{Registry: "registry.internal:5000/talos"},
			want: "registry.internal:5000/talos/nocloud-installer/" + oldSchematic + ":v1.13.2",
		},
		{
			name: "all four at once",
			edit: ImageEdit{Registry: "mirror.example.com", Platform: "metal", Schematic: newSchematic, Version: "v1.14.0"},
			want: "mirror.example.com/metal-installer/" + newSchematic + ":v1.14.0",
		},
		{
			name: "nothing",
			edit: ImageEdit{},
			want: ref,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := RewriteImage(ref, tc.edit)
			if err != nil {
				t.Fatalf("RewriteImage() error: %v", err)
			}
			if got != tc.want {
				t.Errorf("RewriteImage() = %q\nwant %q", got, tc.want)
			}
		})
	}
}

// A platform is named either way round; the reference shows the variant, so a
// user reading it will reasonably type either.
func TestRewriteImageAcceptsPlatformOrVariant(t *testing.T) {
	ref := "factory.talos.dev/metal-installer/" + strings.Repeat("a", 64) + ":v1.13.2"
	for _, p := range []string{"hcloud", "hcloud-installer"} {
		got, err := RewriteImage(ref, ImageEdit{Platform: p})
		if err != nil {
			t.Fatalf("RewriteImage(platform=%q) error: %v", p, err)
		}
		if !strings.Contains(got, "/hcloud-installer/") {
			t.Errorf("RewriteImage(platform=%q) = %q", p, got)
		}
	}
	// The generic installer is a variant in its own right.
	got, err := RewriteImage(ref, ImageEdit{Platform: "installer"})
	if err != nil {
		t.Fatalf("RewriteImage() error: %v", err)
	}
	if !strings.Contains(got, "/installer/") {
		t.Errorf("RewriteImage() = %q", got)
	}
}

// A version-only edit is positionless, so it works on any reference. The other
// three are positional and must refuse anything that is not a factory image —
// rewriting a segment of ghcr.io/siderolabs/installer yields an image that does
// not exist, which fails as a pull error mid-rollout.
func TestRewriteImageOnNonFactoryReferences(t *testing.T) {
	const ref = "ghcr.io/siderolabs/installer:v1.13.2"

	got, err := RewriteImage(ref, ImageEdit{Version: "v1.13.9"})
	if err != nil {
		t.Fatalf("a version-only edit was refused: %v", err)
	}
	if got != "ghcr.io/siderolabs/installer:v1.13.9" {
		t.Errorf("RewriteImage() = %q", got)
	}

	for name, edit := range map[string]ImageEdit{
		"platform":  {Platform: "hcloud"},
		"schematic": {Schematic: strings.Repeat("a", 64)},
		"registry":  {Registry: "mirror.example.com"},
	} {
		if got, err := RewriteImage(ref, edit); err == nil {
			t.Errorf("a %s edit rewrote a non-factory image to %q", name, got)
		}
	}
}

func TestRewriteImageValidatesItsInputs(t *testing.T) {
	ref := "factory.talos.dev/nocloud-installer/" + strings.Repeat("a", 64) + ":v1.13.2"
	for name, edit := range map[string]ImageEdit{
		"short schematic":     {Schematic: strings.Repeat("a", 63)},
		"non-hex schematic":   {Schematic: strings.Repeat("z", 64)},
		"platform with slash": {Platform: "foo/bar"},
		"platform with space": {Platform: "foo bar"},
		"registry with space": {Registry: "not a registry"},
	} {
		if got, err := RewriteImage(ref, edit); err == nil {
			t.Errorf("%s was accepted, producing %q", name, got)
		}
	}
}

func TestPlatformOf(t *testing.T) {
	cases := map[string]string{
		"factory.talos.dev/nocloud-installer/" + strings.Repeat("a", 64) + ":v1.13.2": "nocloud",
		"factory.talos.dev/hcloud-installer/" + strings.Repeat("a", 64) + ":v1.13.2":  "hcloud",
		// The generic installer names no platform.
		"factory.talos.dev/installer/" + strings.Repeat("a", 64) + ":v1.13.2": "",
		"ghcr.io/siderolabs/installer:v1.13.2":                                "",
	}
	for ref, want := range cases {
		if got := PlatformOf(ref); got != want {
			t.Errorf("PlatformOf(%q) = %q, want %q", ref, got, want)
		}
	}
}
