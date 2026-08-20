// Package talosctl runs the Talos CLI.
//
// Everything this plugin does to a node — reading dmesg, opening a dashboard,
// patching a machine config, upgrading Talos — is something talosctl already
// does correctly, including the parts that are easy to get subtly wrong: the
// gRPC streaming protocol, the config-apply modes, the upgrade progress
// tracking, and the etcd-aware sequencing behind a node reboot.
//
// This plugin's job is to work out *what* to point it at across a fleet, not
// to reimplement it. So every command here is a thin, explicit argv: no shell,
// no string interpolation, and nothing inherited from the ambient environment —
// in particular never the user's ~/.talos/config, which is exactly the "act on
// whatever cluster the shell was pointing at" accident this plugin exists to
// prevent.
package talosctl

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// BinaryName is the expected name of the Talos CLI on PATH.
const BinaryName = "talosctl"

// InstallHint is appended to the not-found error, since the fix is always the
// same and is never obvious to someone meeting Talos for the first time.
const InstallHint = "install it from https://www.talos.dev/latest/talos-guides/install/talosctl/"

// Available reports whether talosctl is discoverable on PATH.
func Available() bool {
	_, err := exec.LookPath(BinaryName)
	return err == nil
}

// Path resolves the talosctl binary, with an actionable error when it is
// missing.
func Path() (string, error) {
	p, err := exec.LookPath(BinaryName)
	if err != nil {
		return "", fmt.Errorf("%s not found on PATH — %s", BinaryName, InstallHint)
	}
	return p, nil
}

// Target is the cluster-and-nodes half of every invocation: which config to
// authenticate with, which addresses to connect through, and which nodes the
// API calls are directed at.
//
// Talosconfig and Endpoints are always passed explicitly. Leaving either to
// talosctl's own defaults would fall back to the ambient TALOSCONFIG, which is
// how a fleet-wide command ends up acting on one unrelated cluster.
type Target struct {
	Talosconfig string
	Endpoints   []string
	Nodes       []string
}

// args renders the target's flags.
func (t Target) args() ([]string, error) {
	if t.Talosconfig == "" {
		return nil, fmt.Errorf("talosconfig path is required")
	}
	if len(t.Nodes) == 0 {
		return nil, fmt.Errorf("at least one node is required")
	}
	out := []string{"--talosconfig", t.Talosconfig, "--nodes", strings.Join(t.Nodes, ",")}
	if len(t.Endpoints) > 0 {
		out = append(out, "--endpoints", strings.Join(t.Endpoints, ","))
	}
	return out, nil
}

// Streams are the caller's I/O for an interactive run.
type Streams struct {
	In  io.Reader
	Out io.Writer
	Err io.Writer
}

// Run executes talosctl attached to the caller's streams, so full-screen and
// long-running commands (dashboard, edit, upgrade progress) behave exactly as
// they would when invoked by hand.
func Run(ctx context.Context, s Streams, t Target, verb string, extra ...string) error {
	argv, err := build(t, verb, extra...)
	if err != nil {
		return err
	}
	bin, err := Path()
	if err != nil {
		return err
	}
	// #nosec G204 -- bin is resolved from PATH; argv is a fixed subcommand plus
	// values that came from the cluster's own API and from typed flags, never
	// a shell string.
	c := exec.CommandContext(ctx, bin, argv...)
	c.Stdin = s.In
	c.Stdout = s.Out
	c.Stderr = s.Err
	if c.Stdin == nil {
		c.Stdin = os.Stdin
	}
	// The editor talosctl opens for "edit machineconfig" is chosen from the
	// environment, so the environment is inherited rather than cleared.
	c.Env = os.Environ()
	return c.Run()
}

// Capture executes talosctl and returns its combined output.
//
// Fleet-wide commands use this rather than Run: with several clusters in
// flight at once, interleaved streaming output is unreadable, and a per-target
// block printed on completion is both readable and attributable. The output is
// returned even when the command fails — a talosctl error message is the most
// useful thing a failed target can report.
//
// Merging the streams is right for output meant to be read and wrong for
// output meant to be parsed; see CaptureStreams.
func Capture(ctx context.Context, t Target, verb string, extra ...string) (string, error) {
	stdout, stderr, err := CaptureStreams(ctx, t, verb, extra...)
	return stdout + stderr, err
}

// CaptureStreams executes talosctl and returns stdout and stderr separately.
//
// Anything that parses talosctl's output must use this rather than Capture.
// talosctl writes advisories to stderr — "WARNING: 10.0.0.1: server version
// 1.13.2 is older than client version 1.13.9" is emitted on every call to a
// node running an older Talos — and that line is not valid YAML. Merged into
// the document it silently defeats the parser, which then reports the field as
// missing from a config that plainly contains it.
func CaptureStreams(ctx context.Context, t Target, verb string, extra ...string) (stdout, stderr string, err error) {
	argv, err := build(t, verb, extra...)
	if err != nil {
		return "", "", err
	}
	bin, err := Path()
	if err != nil {
		return "", "", err
	}
	var outBuf, errBuf bytes.Buffer
	// #nosec G204 -- see Run.
	c := exec.CommandContext(ctx, bin, argv...)
	c.Stdout = &outBuf
	c.Stderr = &errBuf
	c.Env = os.Environ()
	err = c.Run()
	return outBuf.String(), errBuf.String(), err
}

// build assembles the full argument list. Split out so tests can assert on it
// without executing anything, which is the only way to check that a fleet-wide
// command is pointed where it claims to be.
func build(t Target, verb string, extra ...string) ([]string, error) {
	if strings.TrimSpace(verb) == "" {
		return nil, fmt.Errorf("a talosctl subcommand is required")
	}
	targetArgs, err := t.args()
	if err != nil {
		return nil, err
	}
	// The subcommand leads, matching how talosctl is written by hand, so an
	// echoed command line can be pasted straight into a shell.
	argv := append([]string{}, strings.Fields(verb)...)
	argv = append(argv, targetArgs...)
	argv = append(argv, extra...)
	return argv, nil
}

// CommandLine renders an invocation the way a human would type it, for the
// "▶ talosctl …" echo that precedes anything long-running or destructive.
func CommandLine(t Target, verb string, extra ...string) string {
	argv, err := build(t, verb, extra...)
	if err != nil {
		return BinaryName + " " + verb
	}
	return BinaryName + " " + strings.Join(argv, " ")
}
