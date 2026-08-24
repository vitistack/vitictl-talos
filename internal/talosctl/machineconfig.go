package talosctl

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"sigs.k8s.io/yaml"
)

// InstallImage returns the installer image a node is currently running, read
// live from its own machine configuration.
//
// This is the only safe base for an upgrade. A Talos installer image encodes
// two things beyond the version: the Image Factory schematic, which carries
// the cluster's system extensions, and the platform variant baked into the
// image name (nocloud-installer, metal-installer, …). Upgrading onto a generic
// ghcr.io/siderolabs/installer drops the extensions — Talos rejects that with a
// confusing "file exists" validation error that strands the upgrade — and
// upgrading onto the wrong platform variant silently changes the node's
// talos.platform, which breaks how it reads its config on the next boot
// without ever failing loudly.
//
// So the version is swapped into the node's own image rather than an image
// being built from scratch.
func InstallImage(ctx context.Context, t Target) (string, error) {
	out, err := MachineConfigYAML(ctx, t)
	if err != nil {
		return "", err
	}
	image, err := installImageFromConfig(out)
	if err != nil {
		return "", fmt.Errorf("node %s: %w", t.Nodes[0], err)
	}
	return image, nil
}

// MachineConfigYAML returns one node's machine configuration as talosctl
// renders it — the full multi-document config, envelope included.
//
// It is the "what is actually on this node right now" read that both the
// upgrade planner and the patch guard are built on. Talos machine configs are
// multi-document, and the documents beyond v1alpha1 (DHCPv4Config,
// LinkConfig, ResolverConfig, …) only exist here; nothing in the management
// cluster's own resources describes them.
func MachineConfigYAML(ctx context.Context, t Target) (string, error) {
	if len(t.Nodes) != 1 {
		return "", fmt.Errorf("reading a machine config addresses exactly one node, got %d", len(t.Nodes))
	}
	// Stdout only: talosctl's stderr advisories are not YAML, and merged into
	// the document they defeat the parser.
	stdout, stderr, err := CaptureStreams(ctx, t, "get machineconfig", "-o", "yaml")
	if err != nil {
		return "", fmt.Errorf("reading machine config from %s: %w: %s",
			t.Nodes[0], err, strings.TrimSpace(stderr+stdout))
	}
	return stdout, nil
}

// machineConfigEnvelope is the resource wrapper talosctl prints around a
// machine config. Its spec is deliberately left raw: talosctl renders it as a
// YAML block scalar — "spec: |" followed by the indented config text — rather
// than as a nested mapping, so the shape has to be decided after the fact.
type machineConfigEnvelope struct {
	Spec json.RawMessage `json:"spec"`
}

// configSpec is the sliver of a machine config that matters here. Everything
// else is ignored on purpose: the config is a large, version-dependent
// structure, and decoding all of it would break on every Talos release.
type configSpec struct {
	Machine struct {
		Install struct {
			Image string `json:"image"`
		} `json:"install"`
	} `json:"machine"`
}

// installImageFromConfig extracts machine.install.image from talosctl's
// output, which is one resource document per node.
func installImageFromConfig(raw string) (string, error) {
	for _, doc := range splitYAMLDocuments(stripAdvisories(raw)) {
		if img := imageFromDocument(doc); img != "" {
			return img, nil
		}
	}
	return "", fmt.Errorf(
		"machine config reports no machine.install.image — pass --image to name the installer explicitly")
}

// imageFromDocument reads the image out of one resource document, coping with
// every shape talosctl has been observed to print.
//
// The spec arrives as a block scalar — a string holding the config as text —
// so it has to be unwrapped before it can be read as a config. Treating it as
// a nested mapping silently yields nothing, which surfaces much later as
// "reports no machine.install.image" against a node whose config plainly has
// one. Both shapes are therefore tried, plus a bare config with no envelope at
// all.
func imageFromDocument(doc string) string {
	var env machineConfigEnvelope
	if err := yaml.Unmarshal([]byte(doc), &env); err == nil && len(env.Spec) > 0 {
		var nested string
		if err := json.Unmarshal(env.Spec, &nested); err == nil {
			// spec: | — the config is the string's contents.
			if img := imageFromConfigBody(nested); img != "" {
				return img
			}
		}
		var spec configSpec
		if err := json.Unmarshal(env.Spec, &spec); err == nil {
			if img := strings.TrimSpace(spec.Machine.Install.Image); img != "" {
				return img
			}
		}
	}
	return imageFromConfigBody(doc)
}

// imageFromConfigBody reads machine.install.image from a machine config, which
// is itself multi-document: the v1alpha1 config comes first, followed by the
// network and other documents.
func imageFromConfigBody(body string) string {
	for _, doc := range splitYAMLDocuments(body) {
		var spec configSpec
		if err := yaml.Unmarshal([]byte(doc), &spec); err != nil {
			continue
		}
		if img := strings.TrimSpace(spec.Machine.Install.Image); img != "" {
			return img
		}
	}
	return ""
}

// advisoryPrefixes are the line prefixes talosctl uses for messages that are
// not part of its output proper.
var advisoryPrefixes = []string{"WARNING:", "ERROR:", "NOTICE:"}

// stripAdvisories drops leading advisory lines from talosctl's output.
//
// Belt and braces: these belong on stderr and CaptureStreams keeps them there,
// so nothing should reach the parser. But a single "WARNING: 10.0.0.1: server
// version …" line prepended to a document is not valid YAML and silently
// defeats the whole parse, reporting a field as missing from a config that
// plainly contains it. That failure is expensive enough to be worth guarding
// twice, and cheap enough to guard.
func stripAdvisories(raw string) string {
	lines := strings.Split(raw, "\n")
	cut := 0
	for _, l := range lines {
		trimmed := strings.TrimSpace(l)
		if trimmed == "" {
			cut++
			continue
		}
		isAdvisory := false
		for _, prefix := range advisoryPrefixes {
			if strings.HasPrefix(trimmed, prefix) {
				isAdvisory = true
				break
			}
		}
		if !isAdvisory {
			break
		}
		cut++
	}
	return strings.Join(lines[cut:], "\n")
}

// splitYAMLDocuments splits a multi-document YAML stream on its --- separators.
func splitYAMLDocuments(raw string) []string {
	lines := strings.Split(raw, "\n")
	var docs []string
	var current []string
	for _, l := range lines {
		if strings.TrimRight(l, " \t") == "---" {
			docs = append(docs, strings.Join(current, "\n"))
			current = nil
			continue
		}
		current = append(current, l)
	}
	return append(docs, strings.Join(current, "\n"))
}

// SplitImageTag splits an image reference into everything before its tag and
// the tag itself, which is "" when the reference carries none.
//
// The split is anchored after the last "/" so a registry port —
// "localhost:5000/installer" — is not mistaken for a tag separator.
func SplitImageTag(ref string) (base, tag string) {
	slash := strings.LastIndexByte(ref, '/')
	colon := strings.LastIndexByte(ref, ':')
	if colon > slash {
		return ref[:colon], ref[colon+1:]
	}
	return ref, ""
}

// TagOf returns the tag an image reference carries, or "" when it has none.
//
// It exists so a plan can show the part of a reference that is actually
// changing. An installer reference is around a hundred characters of registry,
// platform and schematic digest, and printed in full on both sides of an arrow
// the version is the hardest thing on the line to find.
func TagOf(ref string) string {
	_, tag := SplitImageTag(ref)
	return tag
}

// SwapImageTag returns ref with its tag replaced by tag, appending one when
// ref carries none.
func SwapImageTag(ref, tag string) string {
	base, _ := SplitImageTag(ref)
	return base + ":" + tag
}

// schematicPattern matches an Image Factory schematic digest: the 64-character
// hex id identifying a set of system extensions and kernel arguments.
var schematicPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// pathSegmentPattern matches a value that can stand as one path segment of an
// image reference. It exists to reject a "platform" or "registry" that would
// silently restructure the whole reference.
var pathSegmentPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*$`)

// installerVariantSuffix is how the Image Factory names its per-platform
// installers: metal-installer, nocloud-installer, hcloud-installer, and the
// bare "installer" for the generic one.
const installerVariantSuffix = "installer"

// ImageEdit names the parts of an installer reference to replace. An empty
// field leaves that part as it is.
//
// The four parts are independent and each carries different consequences:
//
//	Registry   where the image is pulled from — a mirror, an air-gapped copy
//	Platform   which platform variant, and so where Talos reads its config
//	           from on the next boot
//	Schematic  which system extensions and kernel arguments are baked in
//	Version    which Talos release
type ImageEdit struct {
	Registry  string
	Platform  string
	Schematic string
	Version   string
}

// Empty reports whether the edit would change nothing.
func (e ImageEdit) Empty() bool {
	return e.Registry == "" && e.Platform == "" && e.Schematic == "" && e.Version == ""
}

// installerRef is an Image Factory installer reference split into its parts:
// <registry>/<platform>-installer/<schematic>:<tag>
type installerRef struct {
	registry  string
	variant   string
	schematic string
	tag       string
}

func (r installerRef) String() string {
	out := r.registry + "/" + r.variant + "/" + r.schematic
	if r.tag != "" {
		out += ":" + r.tag
	}
	return out
}

// Platform returns the platform a variant names — "nocloud" for
// "nocloud-installer" — and "" for the generic installer.
func (r installerRef) platform() string {
	if r.variant == installerVariantSuffix {
		return ""
	}
	return strings.TrimSuffix(r.variant, "-"+installerVariantSuffix)
}

// PlatformOf returns the platform an installer reference targets, or "" when
// the reference is not a factory image or names the generic installer.
func PlatformOf(ref string) string {
	parsed, ok := parseInstallerRef(ref)
	if !ok {
		return ""
	}
	return parsed.platform()
}

// SchematicOf returns the Image Factory schematic an installer reference is
// built from, or "" when the reference is not a factory image.
//
// It is what makes an upgrade's target comparable to what a node reports
// running: a version match alone is not enough, since --schematic changes the
// extensions while leaving the version alone.
func SchematicOf(ref string) string {
	parsed, ok := parseInstallerRef(ref)
	if !ok {
		return ""
	}
	return parsed.schematic
}

// parseInstallerRef splits a factory installer reference into its parts,
// reporting false for anything that is not one.
func parseInstallerRef(ref string) (installerRef, bool) {
	base, tag := ref, ""
	if slash, colon := strings.LastIndexByte(ref, '/'), strings.LastIndexByte(ref, ':'); colon > slash {
		base, tag = ref[:colon], ref[colon+1:]
	}
	segments := strings.Split(base, "/")
	if len(segments) < 3 {
		return installerRef{}, false
	}
	schematic := segments[len(segments)-1]
	variant := segments[len(segments)-2]
	if !schematicPattern.MatchString(schematic) {
		return installerRef{}, false
	}
	if variant != installerVariantSuffix && !strings.HasSuffix(variant, "-"+installerVariantSuffix) {
		return installerRef{}, false
	}
	return installerRef{
		registry:  strings.Join(segments[:len(segments)-2], "/"),
		variant:   variant,
		schematic: schematic,
		tag:       tag,
	}, true
}

// RewriteImage applies edit to an installer reference, leaving every part the
// edit does not name exactly as the node reported it.
//
// A version-only edit works on any reference, including a plain
// ghcr.io/siderolabs/installer. Changing the registry, platform or schematic
// requires a recognisable Image Factory reference, because those parts are
// positional: rewriting the last path segment of a non-factory reference would
// produce an image that does not exist, and that surfaces as a pull failure
// part way through a rolling upgrade rather than as a mistake in the command.
func RewriteImage(ref string, edit ImageEdit) (string, error) {
	if edit.Empty() {
		return ref, nil
	}
	if edit.Registry == "" && edit.Platform == "" && edit.Schematic == "" {
		return SwapImageTag(ref, NormalizeVersion(edit.Version)), nil
	}

	parsed, ok := parseInstallerRef(ref)
	if !ok {
		return "", fmt.Errorf(
			"%q is not an Image Factory reference (expected <registry>/<platform>-installer/<64-hex schematic>), "+
				"so its registry, platform and schematic cannot be edited individually — pass --image to set the "+
				"installer outright", ref)
	}

	if edit.Registry != "" {
		for _, segment := range strings.Split(edit.Registry, "/") {
			if !pathSegmentPattern.MatchString(segment) {
				return "", fmt.Errorf("%q is not a usable registry", edit.Registry)
			}
		}
		parsed.registry = strings.TrimSuffix(edit.Registry, "/")
	}
	if edit.Platform != "" {
		variant, err := installerVariant(edit.Platform)
		if err != nil {
			return "", err
		}
		parsed.variant = variant
	}
	if edit.Schematic != "" {
		if !schematicPattern.MatchString(edit.Schematic) {
			return "", fmt.Errorf(
				"%q is not a schematic id — expected 64 hex characters, as returned by the Image Factory",
				edit.Schematic)
		}
		parsed.schematic = edit.Schematic
	}
	if edit.Version != "" {
		parsed.tag = NormalizeVersion(edit.Version)
	}
	return parsed.String(), nil
}

// installerVariant renders a platform name as the factory's installer variant.
// Both "hcloud" and "hcloud-installer" are accepted, since the reference shows
// the latter and a user reading it will reasonably type either.
func installerVariant(platform string) (string, error) {
	p := strings.TrimSpace(platform)
	if p == "" {
		return "", fmt.Errorf("empty platform")
	}
	if !pathSegmentPattern.MatchString(p) {
		return "", fmt.Errorf("%q is not a usable platform name", platform)
	}
	if p == installerVariantSuffix || strings.HasSuffix(p, "-"+installerVariantSuffix) {
		return p, nil
	}
	return p + "-" + installerVariantSuffix, nil
}

// NormalizeVersion renders a Talos version the way an image tag spells it:
// with the leading v, which users habitually leave off.
func NormalizeVersion(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if strings.HasPrefix(v, "v") {
		return v
	}
	return "v" + v
}

// NodePlatform returns the platform a node is actually running as, from its own
// PlatformMetadata resource.
//
// This is what the installed image baked in, not what the hardware is — which
// is precisely why it is worth showing before a platform change. A node
// provisioned from a metal-installer reports "metal" even when it is a Hetzner
// VM, and that mismatch is the reason to switch variants in the first place.
// Best effort: a node that cannot answer costs the plan a column, not the run.
func NodePlatform(ctx context.Context, t Target) string {
	stdout, _, err := CaptureStreams(ctx, t, "get platformmetadata", "-o", "yaml")
	if err != nil {
		return ""
	}
	for _, doc := range splitYAMLDocuments(stripAdvisories(stdout)) {
		var parsed struct {
			Spec struct {
				Platform string `json:"platform"`
			} `json:"spec"`
		}
		if err := yaml.Unmarshal([]byte(doc), &parsed); err != nil {
			continue
		}
		if p := strings.TrimSpace(parsed.Spec.Platform); p != "" {
			return p
		}
	}
	return ""
}
