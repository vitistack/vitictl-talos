package talosctl

import (
	"context"
	"fmt"
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
	out, err := Capture(ctx, t, "get machineconfig", "-o", "yaml")
	if err != nil {
		return "", fmt.Errorf("reading machine config from %s: %w: %s",
			t.Nodes[0], err, strings.TrimSpace(out))
	}
	return out, nil
}

// machineConfigDoc is the sliver of `talosctl get machineconfig -o yaml` that
// matters here. Everything else in the document is ignored on purpose: the
// machine config is a large, version-dependent structure, and decoding all of
// it would break on every Talos release.
type machineConfigDoc struct {
	Spec struct {
		Machine struct {
			Install struct {
				Image string `json:"image"`
			} `json:"install"`
		} `json:"machine"`
	} `json:"spec"`
}

// installImageFromConfig extracts machine.install.image from talosctl's
// output, which is one YAML document per node.
func installImageFromConfig(raw string) (string, error) {
	for _, doc := range splitYAMLDocuments(raw) {
		var parsed machineConfigDoc
		if err := yaml.Unmarshal([]byte(doc), &parsed); err != nil {
			// A document that does not parse is not fatal on its own:
			// talosctl prefixes warnings to its output often enough that
			// insisting on every document would be brittle.
			continue
		}
		if img := strings.TrimSpace(parsed.Spec.Machine.Install.Image); img != "" {
			return img, nil
		}
	}
	return "", fmt.Errorf(
		"machine config reports no machine.install.image — pass --image to name the installer explicitly")
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

// SwapImageTag returns ref with its tag replaced by tag, appending one when
// ref carries none.
//
// The search is anchored after the last "/" so a registry port —
// "localhost:5000/installer" — is not mistaken for a tag separator.
func SwapImageTag(ref, tag string) string {
	slash := strings.LastIndexByte(ref, '/')
	colon := strings.LastIndexByte(ref, ':')
	if colon > slash {
		return ref[:colon] + ":" + tag
	}
	return ref + ":" + tag
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
