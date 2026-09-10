package cmd

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// The command tree is what viti's users actually meet, so its shape is worth
// asserting: a renamed or dropped command is a breaking change.
func TestRootCommandTree(t *testing.T) {
	root := NewRootCmd()
	want := []string{
		"clusters", "nodes", "dashboard", "dmesg", "netstat", "memory", "get",
		"edit", "show", "patch", "upgrade-node", "upgrade-k8s", "tunnel",
		"config", "version", "upgrade",
	}
	have := map[string]bool{}
	for _, c := range root.Commands() {
		have[c.Name()] = true
	}
	for _, w := range want {
		if !have[w] {
			t.Errorf("command %q is missing from the tree", w)
		}
	}
}

// "upgrade" upgrades the plugin while "upgrade-node"/"upgrade-k8s" upgrade the
// cluster. That is easy to conflate, so each must say which it is.
func TestUpgradeCommandsDistinguishThemselves(t *testing.T) {
	root := NewRootCmd()
	for name, must := range map[string]string{
		"upgrade":      "plugin",
		"upgrade-node": "Talos",
		"upgrade-k8s":  "Kubernetes",
	} {
		cmd, _, err := root.Find([]string{name})
		if err != nil {
			t.Fatalf("Find(%q): %v", name, err)
		}
		if !strings.Contains(cmd.Short, must) {
			t.Errorf("%q short help %q does not say it upgrades the %s", name, cmd.Short, must)
		}
	}
}

func TestVersionIsReported(t *testing.T) {
	SetVersion("v1.2.3")
	t.Cleanup(func() { SetVersion("dev") })

	root := NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"version"})
	if err := root.Execute(); err != nil {
		t.Fatalf("version: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "viti-talos version v1.2.3") {
		t.Errorf("version printed %q", got)
	}
}

// SetVersion must not clear a real version when handed an empty string.
func TestSetVersionIgnoresEmpty(t *testing.T) {
	SetVersion("v9.9.9")
	t.Cleanup(func() { SetVersion("dev") })
	SetVersion("")
	if version != "v9.9.9" {
		t.Errorf("version = %q after SetVersion(\"\"), want it unchanged", version)
	}
}

func TestParseRole(t *testing.T) {
	cases := map[string]string{
		"": "", "controlplane": "controlplane", "CP": "controlplane",
		"control-plane": "controlplane", "worker": "worker", "workers": "worker",
	}
	for in, want := range cases {
		got, err := parseRole(in)
		if err != nil {
			t.Errorf("parseRole(%q) error: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("parseRole(%q) = %q, want %q", in, got, want)
		}
	}
	if _, err := parseRole("etcd"); err == nil {
		t.Error("parseRole() accepted an unknown role")
	}
}

func TestParseMode(t *testing.T) {
	if got, err := parseMode(""); err != nil || got != "auto" {
		t.Errorf("parseMode(\"\") = %q, %v; want auto", got, err)
	}
	for _, m := range applyModes {
		if got, err := parseMode(strings.ToUpper(m)); err != nil || got != m {
			t.Errorf("parseMode(%q) = %q, %v", m, got, err)
		}
	}
	if _, err := parseMode("yolo"); err == nil {
		t.Error("parseMode() accepted an unknown mode")
	}
}

// This decides whether a cluster's nodes are patched one at a time. Getting
// "auto" wrong would fan a rebooting change out across every control plane at
// once, which costs the cluster its etcd quorum.
func TestModeRebootsTreatsAutoAsRebooting(t *testing.T) {
	for mode, want := range map[string]bool{
		"auto": true, "reboot": true, "try": true,
		"no-reboot": false, "staged": false,
	} {
		if got := modeReboots(mode); got != want {
			t.Errorf("modeReboots(%q) = %v, want %v", mode, got, want)
		}
	}
}

func TestSplitArgs(t *testing.T) {
	cases := []struct {
		name           string
		argv           []string
		wantCluster    string
		wantPassthough []string
		wantErr        bool
	}{
		{name: "nothing", argv: nil},
		{name: "cluster only", argv: []string{"c1"}, wantCluster: "c1"},
		{
			name: "cluster and passthrough", argv: []string{"c1", "--", "--follow"},
			wantCluster: "c1", wantPassthough: []string{"--follow"},
		},
		{
			name: "passthrough only", argv: []string{"--", "--tail"},
			wantPassthough: []string{"--tail"},
		},
		{name: "two clusters", argv: []string{"c1", "c2"}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := &cobra.Command{Use: "x", Args: cobra.ArbitraryArgs, RunE: func(*cobra.Command, []string) error { return nil }}
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})

			var gotCluster string
			var gotPass []string
			var gotErr error
			cmd.RunE = func(c *cobra.Command, args []string) error {
				gotCluster, gotPass, gotErr = splitArgs(c, args)
				return nil
			}
			cmd.SetArgs(tc.argv)
			if err := cmd.Execute(); err != nil {
				t.Fatalf("execute: %v", err)
			}
			if tc.wantErr {
				if gotErr == nil {
					t.Fatalf("splitArgs(%v) accepted two cluster names", tc.argv)
				}
				return
			}
			if gotErr != nil {
				t.Fatalf("splitArgs(%v) error: %v", tc.argv, gotErr)
			}
			if gotCluster != tc.wantCluster {
				t.Errorf("cluster = %q, want %q", gotCluster, tc.wantCluster)
			}
			if strings.Join(gotPass, " ") != strings.Join(tc.wantPassthough, " ") {
				t.Errorf("passthrough = %v, want %v", gotPass, tc.wantPassthough)
			}
		})
	}
}

// A mistyped patch path would otherwise produce the same error once per
// cluster, after the run has already started.
func TestPatchFlagsValidatesFilesUpFront(t *testing.T) {
	if _, err := patchFlags(nil); err == nil {
		t.Error("patchFlags(nil) accepted a patch run with no patch")
	}
	if _, err := patchFlags([]string{"@/definitely/not/here.yaml"}); err == nil {
		t.Error("patchFlags() accepted a missing patch file")
	}
	if _, err := patchFlags([]string{"  "}); err == nil {
		t.Error("patchFlags() accepted an empty patch")
	}

	dir := t.TempDir()
	empty := dir + "/empty.yaml"
	if err := writeFile(empty, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := patchFlags([]string{"@" + empty}); err == nil {
		t.Error("patchFlags() accepted an empty patch file")
	}
	if _, err := patchFlags([]string{"@" + dir}); err == nil {
		t.Error("patchFlags() accepted a directory as a patch file")
	}

	good := dir + "/patch.yaml"
	if err := writeFile(good, "machine:\n  kubelet: {}\n"); err != nil {
		t.Fatal(err)
	}
	got, err := patchFlags([]string{"@" + good, "machine: {}"})
	if err != nil {
		t.Fatalf("patchFlags() error: %v", err)
	}
	want := []string{"-p", "@" + good, "-p", "machine: {}"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("patchFlags() = %v, want %v", got, want)
	}
}

// The plan is what an operator reads before authorising a fleet-wide write, so
// it must state the consequence of the mode rather than only its name.
func TestModeEffectIsStatedForEveryMode(t *testing.T) {
	for _, m := range applyModes {
		if strings.TrimSpace(modeEffect(m)) == "" {
			t.Errorf("modeEffect(%q) is empty", m)
		}
	}
	if !strings.Contains(modeEffect("staged"), "next reboot") {
		t.Errorf("modeEffect(staged) = %q, want it to say nothing restarts now", modeEffect("staged"))
	}
}

func TestClusterResultOK(t *testing.T) {
	ok := clusterResult{Steps: []nodeResult{{Nodes: []string{"n"}}}}
	if !ok.OK() {
		t.Error("a clean result reported not OK")
	}
	if (clusterResult{Skipped: true}).OK() {
		t.Error("a skipped cluster reported OK — it was never attempted")
	}
	if (clusterResult{Error: "boom"}).OK() {
		t.Error("a failed cluster reported OK")
	}
	partial := clusterResult{Steps: []nodeResult{{Nodes: []string{"a"}}, {Nodes: []string{"b"}, Error: "boom"}}}
	if partial.OK() {
		t.Error("a partially-patched cluster reported OK")
	}
}

func TestFirstLineAndTruncate(t *testing.T) {
	if got := firstLine("one\ntwo"); got != "one" {
		t.Errorf("firstLine() = %q", got)
	}
	if got := truncate("a\nb", 10); got != "a b" {
		t.Errorf("truncate() did not flatten newlines: %q", got)
	}
	if got := truncate(strings.Repeat("x", 20), 10); len(got) != 10 || !strings.HasSuffix(got, "...") {
		t.Errorf("truncate() = %q", got)
	}
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}

// --if-match exists to make a non-idempotent patch re-runnable, so "inspected
// and had nothing left to change" must not read as a failure — otherwise a
// completed fleet looks half-done on the second pass.
func TestNothingToDoIsSuccessButNotAttemptedIsNot(t *testing.T) {
	if !(clusterResult{NoMatch: true}).OK() {
		t.Error("a cluster where --if-match matched nothing reported not OK")
	}
	if (clusterResult{Skipped: true}).OK() {
		t.Error("a cluster skipped by --fail-fast reported OK — it was never inspected")
	}
	// A cluster that matched and then failed is still a failure.
	if (clusterResult{NoMatch: false, Steps: []nodeResult{{Error: "boom"}}}).OK() {
		t.Error("a failed cluster reported OK")
	}
}

// A flags-in-a-variable mistake arrives as a filename containing a command
// line. Diagnosing it beats sending the reader to look at their filesystem.
func TestGlueHintFiresOnlyOnAGluedCommandLine(t *testing.T) {
	if got := glueHint("patch.yaml"); got != "" {
		t.Errorf("glueHint() fired on an ordinary missing file: %q", got)
	}
	if got := glueHint("my-patches/net-0.yaml"); got != "" {
		t.Errorf("glueHint() fired on a path containing a dash: %q", got)
	}
	got := glueHint("drop.yaml --if-match 'kind: DHCPv4Config' --mode no-reboot")
	if got == "" {
		t.Fatal("glueHint() did not recognise a glued command line")
	}
	if !strings.Contains(got, "function") {
		t.Errorf("hint does not point at the fix: %q", got)
	}
}

// The pin is the operator's desired state; pinning one of several images would
// quietly converge the rest onto it.
func TestPinNeededOnlyWhenItWouldChangeSomething(t *testing.T) {
	if (pin{have: "a", want: "a"}).needed() {
		t.Error("pin reported needed when it already matches")
	}
	if !(pin{have: "a", want: "b"}).needed() {
		t.Error("pin not reported needed when it differs")
	}
	if (pin{have: "a", want: "b", skipped: true}).needed() {
		t.Error("--no-pin still wrote the pin")
	}
	if (pin{have: "a", want: ""}).needed() {
		t.Error("pin reported needed with no target image")
	}
}

// The resume command this tool prints is meant to be pasted, so it has to name
// itself the way its user reaches it — "viti talos", which is what every
// Example block and the whole README say — while still naming itself runnably
// when there is no viti to reach it through.
func TestInvocationName(t *testing.T) {
	const path, root = "viti-talos upgrade-node", "viti-talos"

	for _, tc := range []struct {
		name       string
		argv0      string
		vitiOnPath bool
		want       string
	}{{
		name:       "dispatched through viti",
		argv0:      "/Users/x/.local/bin/viti-talos",
		vitiOnPath: true,
		want:       "viti talos upgrade-node",
	}, {
		// The alias is its own symlink, so a user who typed the shorthand gets
		// the shorthand back rather than having it spelled out for them.
		name:       "dispatched through the viti-t alias",
		argv0:      "/Users/x/.local/bin/viti-t",
		vitiOnPath: true,
		want:       "viti t upgrade-node",
	}, {
		// The one case where inferring would hand over a command that does not
		// run: standalone, with no vitictl installed beside it.
		name:       "standalone with no viti on PATH",
		argv0:      "/Users/x/.local/bin/viti-talos",
		vitiOnPath: false,
		want:       "viti-talos upgrade-node",
	}, {
		name:       "renamed binary",
		argv0:      "/Users/x/bin/talos-tool",
		vitiOnPath: true,
		want:       "viti-talos upgrade-node",
	}, {
		name:       "windows",
		argv0:      `C:\Users\x\bin\viti-talos.exe`,
		vitiOnPath: true,
		want:       "viti talos upgrade-node",
	}, {
		// The test binary's own argv0, which is why every other test in this
		// package sees cobra's spelling and not the inferred one.
		name:       "a test binary",
		argv0:      "/tmp/go-build123/b001/cmd.test",
		vitiOnPath: true,
		want:       "viti-talos upgrade-node",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			if got := invocationName(tc.argv0, path, root, tc.vitiOnPath); got != tc.want {
				t.Errorf("invocationName(%q, viti on PATH=%v) = %q, want %q",
					tc.argv0, tc.vitiOnPath, got, tc.want)
			}
		})
	}
}

// Naming the root itself must not repeat the name it just derived.
func TestInvocationNameOfTheRootCommand(t *testing.T) {
	got := invocationName("/usr/bin/viti-talos", "viti-talos", "viti-talos", true)
	if got != "viti talos" {
		t.Errorf("invocationName of the root = %q, want %q", got, "viti talos")
	}
}
