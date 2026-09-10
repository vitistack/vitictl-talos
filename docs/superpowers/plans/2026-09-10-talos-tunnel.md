# `viti talos tunnel` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `viti talos tunnel <cluster>`, which reaches a Talos API that is
unroutable from this workstation by running a socat pod inside the cluster's
own Kubernetes cluster and port-forwarding to it.

**Architecture:** A new config file (`~/.vitistack/talos.yaml`) names clusters
that are not `KubernetesCluster` resources, pairing a kube context with a
talosconfig path. Node topology is discovered live from the Kubernetes API
rather than configured. A new `internal/tunnel` package creates the socat pod
and drives client-go's SPDY port-forwarder; `cmd/tunnel.go` wires it to the
existing talosctl runner. Nothing existing changes behaviour.

**Tech Stack:** Go 1.27, cobra, client-go v0.37.0
(`kubernetes` typed clientset, `tools/portforward`, `transport/spdy`),
controller-runtime (existing), `go.yaml.in/yaml/v3`, stdlib `testing`.

**Spec:** `docs/superpowers/specs/2026-09-10-talos-tunnel-design.md`

## Global Constraints

- Go module `github.com/vitistack/vitictl-talos`, Go 1.27.0.
- Tests use the **standard library `testing` package only** — no testify. Match
  the style of `internal/cluster/topology_test.go`: helper constructors,
  `t.Errorf`/`t.Fatalf`, and a doc comment on each test saying *why* the
  behaviour matters.
- No test may contact a cluster, a network, or the real `~/.talos` /
  `~/.vitistack`. Use `t.TempDir()` and `t.Setenv`.
- Verify with `make test` (runs `go fmt`, `go vet`, `go test ./... -coverprofile cover.out`)
  and `make lint` (golangci-lint, repo defaults — no `.golangci.yml`).
- `gosec` runs in CI (`make gosec`). Any `os.ReadFile` on a variable path needs
  a `// #nosec G304 -- <reason>` comment, matching `internal/config/config.go:71`.
- The Talos API port is `50000`, already defined as `cluster.APIPort`.
- Default tunnel namespace is `viti-center`; default image is `alpine/socat`.
- **The user handles all git commits and pushes.** Commit steps below say what
  to stage and what message to use; run them only if the user asks.
- Comments explain *why*, not *what* — this repo's prevailing style. Match the
  density of the surrounding file.

---

### Task 1: Tunnel cluster configuration

Adds the config file that names the six clusters. Also corrects the two doc
comments that currently claim this plugin has no config file of its own.

**Files:**
- Create: `internal/config/tunnels.go`
- Create: `internal/config/tunnels_test.go`
- Modify: `internal/config/config.go:1-9` (package doc)

**Interfaces:**
- Consumes: `config.VitistackConfigPath()` (existing, `internal/config/config.go:52`)
- Produces:
  ```go
  const EnvTalosConfig = "VITI_TALOS_CONFIG"
  const TalosConfigFileName = "talos.yaml"
  const DefaultTunnelNamespace = "viti-center"

  type TunnelCluster struct {
      Name        string `yaml:"name,omitempty"`
      Context     string `yaml:"context"`
      Kubeconfig  string `yaml:"kubeconfig,omitempty"`
      Talosconfig string `yaml:"talosconfig"`
      Namespace   string `yaml:"namespace,omitempty"`
  }

  func TalosConfigPath() (string, error)
  func TunnelClusters() ([]TunnelCluster, error)
  func FindTunnelCluster(in []TunnelCluster, name string) (TunnelCluster, bool)
  ```

- [ ] **Step 1: Write the failing tests**

Create `internal/config/tunnels_test.go`:

```go
package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sampleTalosYAML = `clusters:
    - context: admin@pos1-kv-cl01
      talosconfig: ~/.talos/kvosltalos
    - name: bgo-virt-001
      context: admin@bgo-virt-001
      kubeconfig: ~/.kube/config
      talosconfig: /etc/talos/bgo
      namespace: viti-tunnels
`

func writeTalosConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "talos.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvTalosConfig, path)
	return path
}

// The file sits beside vitictl's own config rather than at a fixed absolute
// path, so a VITI_CONFIG pointing at a scratch directory keeps the two
// together instead of silently reading the user's real credentials.
func TestTalosConfigPathSitsBesideTheVitistackConfig(t *testing.T) {
	t.Setenv(EnvTalosConfig, "")
	t.Setenv(EnvVitiConfig, "/somewhere/else/ctl.config.yaml")
	got, err := TalosConfigPath()
	if err != nil {
		t.Fatalf("TalosConfigPath() error: %v", err)
	}
	if got != "/somewhere/else/talos.yaml" {
		t.Errorf("TalosConfigPath() = %q, want it beside ctl.config.yaml", got)
	}
}

func TestTalosConfigPathHonoursItsOwnOverride(t *testing.T) {
	t.Setenv(EnvTalosConfig, "/tmp/custom.yaml")
	got, err := TalosConfigPath()
	if err != nil {
		t.Fatalf("TalosConfigPath() error: %v", err)
	}
	if got != "/tmp/custom.yaml" {
		t.Errorf("TalosConfigPath() = %q, want the override", got)
	}
}

// An entry naming only a context is the common case — repeating the context
// as the name for all six clusters would be noise nobody maintains.
func TestTunnelClustersDefaultsNameAndNamespace(t *testing.T) {
	writeTalosConfig(t, sampleTalosYAML)
	got, err := TunnelClusters()
	if err != nil {
		t.Fatalf("TunnelClusters() error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("TunnelClusters() returned %d entries, want 2", len(got))
	}
	if got[0].Name != "admin@pos1-kv-cl01" {
		t.Errorf("Name = %q, want it defaulted to the context", got[0].Name)
	}
	if got[0].Namespace != DefaultTunnelNamespace {
		t.Errorf("Namespace = %q, want %q", got[0].Namespace, DefaultTunnelNamespace)
	}
	if got[1].Name != "bgo-virt-001" {
		t.Errorf("Name = %q, want the explicit name kept", got[1].Name)
	}
	if got[1].Namespace != "viti-tunnels" {
		t.Errorf("Namespace = %q, want the explicit namespace kept", got[1].Namespace)
	}
}

// The three credential files really do live under ~, and a literal "~" passed
// to os.ReadFile fails with a message that blames the file rather than the
// path.
func TestTunnelClustersExpandsHomePaths(t *testing.T) {
	writeTalosConfig(t, sampleTalosYAML)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory in this environment")
	}
	got, err := TunnelClusters()
	if err != nil {
		t.Fatalf("TunnelClusters() error: %v", err)
	}
	want := filepath.Join(home, ".talos/kvosltalos")
	if got[0].Talosconfig != want {
		t.Errorf("Talosconfig = %q, want %q", got[0].Talosconfig, want)
	}
	wantKube := filepath.Join(home, ".kube/config")
	if got[1].Kubeconfig != wantKube {
		t.Errorf("Kubeconfig = %q, want %q", got[1].Kubeconfig, wantKube)
	}
}

func TestTunnelClustersRejectsIncompleteEntries(t *testing.T) {
	for name, content := range map[string]string{
		"no context":     "clusters:\n    - talosconfig: /a\n",
		"no talosconfig": "clusters:\n    - context: admin@a\n",
	} {
		t.Run(name, func(t *testing.T) {
			writeTalosConfig(t, content)
			_, err := TunnelClusters()
			if err == nil {
				t.Fatal("TunnelClusters() succeeded, want an error")
			}
			if !strings.Contains(err.Error(), "entry 1") {
				t.Errorf("error %q does not say which entry is wrong", err)
			}
		})
	}
}

// Two entries with the same name make "viti talos tunnel X" ambiguous, and
// silently picking one would act on the wrong cluster.
func TestTunnelClustersRejectsDuplicateNames(t *testing.T) {
	writeTalosConfig(t, "clusters:\n    - context: admin@a\n    - context: admin@a\n")
	_, err := TunnelClusters()
	if err == nil {
		t.Fatal("TunnelClusters() succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "admin@a") {
		t.Errorf("error %q does not name the duplicate", err)
	}
}

// Nobody has this file until they need a tunnel, so the error has to teach
// the format rather than just report a missing path.
func TestTunnelClustersExplainsAMissingFile(t *testing.T) {
	t.Setenv(EnvTalosConfig, filepath.Join(t.TempDir(), "absent.yaml"))
	_, err := TunnelClusters()
	if err == nil {
		t.Fatal("TunnelClusters() succeeded, want an error")
	}
	for _, want := range []string{"absent.yaml", "clusters:", "talosconfig:"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestFindTunnelClusterMatchesNameOrContext(t *testing.T) {
	in := []TunnelCluster{{Name: "kv", Context: "admin@kv"}}
	for _, q := range []string{"kv", "admin@kv"} {
		if _, ok := FindTunnelCluster(in, q); !ok {
			t.Errorf("FindTunnelCluster(%q) did not match", q)
		}
	}
	if _, ok := FindTunnelCluster(in, "nope"); ok {
		t.Error("FindTunnelCluster(\"nope\") matched, want no match")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/config/ -run 'Tunnel|TalosConfig' -v`
Expected: FAIL — `undefined: EnvTalosConfig`, `undefined: TunnelClusters`, etc.

- [ ] **Step 3: Write the implementation**

Create `internal/config/tunnels.go`:

```go
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	yaml "go.yaml.in/yaml/v3"
)

const (
	// EnvTalosConfig overrides the path to this plugin's own configuration,
	// for the same reason VITI_CONFIG exists: a scratch config must be
	// reachable without touching the real one.
	EnvTalosConfig = "VITI_TALOS_CONFIG"

	// TalosConfigFileName sits beside vitictl's ctl.config.yaml rather than
	// at a fixed absolute path, so the two move together.
	TalosConfigFileName = "talos.yaml"

	// DefaultTunnelNamespace is where a tunnel pod is created when an entry
	// does not say otherwise. It exists on every Vitistack cluster.
	DefaultTunnelNamespace = "viti-center"
)

// TunnelCluster is one Talos cluster this plugin can reach only through a pod
// in its own Kubernetes cluster.
//
// These are the clusters that are not KubernetesCluster resources — the
// Vitistack management clusters and the KubeVirt hypervisor clusters — so
// nothing in the management cluster can be asked where they are or how to
// authenticate to them. The kube context is their identity; their topology is
// still discovered live rather than configured.
type TunnelCluster struct {
	// Name is what "viti talos tunnel <name>" takes. It defaults to Context,
	// which is already unique and already what the operator types elsewhere.
	Name string `yaml:"name,omitempty"`
	// Context is the kubeconfig context to reach the cluster's Kubernetes API
	// through. Required — it is the cluster's identity here.
	Context string `yaml:"context"`
	// Kubeconfig is the file holding that context, defaulting to $KUBECONFIG
	// and ~/.kube/config the way kubectl does.
	Kubeconfig string `yaml:"kubeconfig,omitempty"`
	// Talosconfig is the file holding the cluster's Talos admin credentials.
	// Required, and the whole reason this file exists: it is the one thing
	// that cannot be discovered from anything already in reach.
	Talosconfig string `yaml:"talosconfig"`
	// Namespace is where the tunnel pod is created.
	Namespace string `yaml:"namespace,omitempty"`
}

// talosConfigFile is this plugin's configuration file.
type talosConfigFile struct {
	Clusters []TunnelCluster `yaml:"clusters"`
}

// exampleTalosConfig is appended to the not-found error. The format is not
// guessable and this file is met for the first time at the moment it is
// missing, so the error teaches it rather than merely reporting a path.
const exampleTalosConfig = `clusters:
  - context: admin@pos1-kv-cl01
    talosconfig: ~/.talos/kvosltalos`

// TalosConfigPath returns this plugin's own configuration file.
func TalosConfigPath() (string, error) {
	if p := os.Getenv(EnvTalosConfig); p != "" {
		return p, nil
	}
	base, err := VitistackConfigPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(base), TalosConfigFileName), nil
}

// TunnelClusters returns the configured tunnel clusters, defaulted and
// validated.
func TunnelClusters() ([]TunnelCluster, error) {
	path, err := TalosConfigPath()
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path) // #nosec G304 -- path is this plugin's own config location
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf(
				"no tunnel clusters configured — create %s:\n\n%s\n",
				path, exampleTalosConfig)
		}
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var file talosConfigFile
	if err := yaml.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if len(file.Clusters) == 0 {
		return nil, fmt.Errorf("no clusters listed in %s:\n\n%s\n", path, exampleTalosConfig)
	}

	seen := make(map[string]int, len(file.Clusters))
	out := make([]TunnelCluster, 0, len(file.Clusters))
	for i, c := range file.Clusters {
		c.Context = strings.TrimSpace(c.Context)
		if c.Context == "" {
			return nil, fmt.Errorf("%s: entry %d has no context", path, i+1)
		}
		c.Talosconfig = expandPath(c.Talosconfig)
		if c.Talosconfig == "" {
			return nil, fmt.Errorf("%s: entry %d (%s) has no talosconfig", path, i+1, c.Context)
		}
		c.Kubeconfig = expandPath(c.Kubeconfig)
		if strings.TrimSpace(c.Name) == "" {
			c.Name = c.Context
		}
		if strings.TrimSpace(c.Namespace) == "" {
			c.Namespace = DefaultTunnelNamespace
		}
		if first, dup := seen[c.Name]; dup {
			return nil, fmt.Errorf("%s: entries %d and %d are both named %q", path, first, i+1, c.Name)
		}
		seen[c.Name] = i + 1
		out = append(out, c)
	}
	return out, nil
}

// FindTunnelCluster narrows the configured clusters to the one named, matching
// either the entry's name or its context — the two are the same for a default
// entry, and insisting on one of them would be a way to be wrong half the time.
func FindTunnelCluster(in []TunnelCluster, name string) (TunnelCluster, bool) {
	for _, c := range in {
		if c.Name == name || c.Context == name {
			return c, true
		}
	}
	return TunnelCluster{}, false
}

// expandPath resolves the leading ~ and any environment variables in a
// configured path. The credential files really do live under ~, and a literal
// "~" reaching os.ReadFile fails with a message that blames the file rather
// than the path.
func expandPath(p string) string {
	p = os.ExpandEnv(strings.TrimSpace(p))
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(home, strings.TrimPrefix(p, "~"))
}
```

- [ ] **Step 4: Correct the package doc that now contradicts the code**

In `internal/config/config.go`, replace lines 1-9 with:

```go
// Package config reads the Vitistack configuration this plugin operates
// against.
//
// Almost everything it needs is already discoverable: the availability zones
// come from vitictl's ctl.config.yaml, and a Talos cluster's credentials come
// from the Secret its own management cluster holds. Duplicating either here
// would only give the two CLIs something to drift on.
//
// The exception is talos.yaml (see tunnels.go), which holds what vitictl has
// no concept of: Talos credentials for clusters that are not
// KubernetesCluster resources — the management clusters and the KubeVirt
// hypervisor clusters. Nothing in it is mirrored from vitictl, so there is
// nothing for the two to disagree about.
package config
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/config/ -v`
Expected: PASS, including the pre-existing `config_test.go` tests.

- [ ] **Step 6: Lint**

Run: `make lint`
Expected: 0 issues.

- [ ] **Step 7: Commit** *(only if the user asks — they handle git)*

```bash
git add internal/config/tunnels.go internal/config/tunnels_test.go internal/config/config.go
git commit -m "feat: read tunnel clusters from talos.yaml"
```

---

### Task 2: Node topology from the Kubernetes API

Turns a `corev1.NodeList` into the `cluster.Node` values the rest of the
plugin already speaks, and picks the control plane the socat pod points at.
This is the finding that removed the need for any configured address list.

**Files:**
- Create: `internal/tunnel/topology.go`
- Create: `internal/tunnel/topology_test.go`
- Modify: `internal/cluster/topology.go` (add `PickNodeIP`, add
  `Topology.Describe`, change the `Select` error at line 123)

**Interfaces:**
- Consumes: `cluster.Node`, `cluster.RoleControlPlane`, `cluster.RoleWorker`
  (existing, `internal/cluster/cluster.go:32-35`, `internal/cluster/topology.go:29`)
- Produces:
  ```go
  const ControlPlaneLabel = "node-role.kubernetes.io/control-plane"
  const PhaseReady    = "Ready"
  const PhaseNotReady = "NotReady"

  func NodesFrom(list *corev1.NodeList, includeIPv6 bool) []cluster.Node
  func TalosVersionFromOSImage(osImage string) string
  func SelectVia(nodes []cluster.Node, override string) (cluster.Node, error)

  // in package cluster:
  func PickNodeIP(addrs []string, includeIPv6 bool) string
  func (t *Topology) Describe() string
  ```

- [ ] **Step 1: Write the failing tests**

Create `internal/tunnel/topology_test.go`:

```go
package tunnel

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vitistack/vitictl-talos/internal/cluster"
)

func node(name string, controlPlane bool, ready bool, osImage string, ips ...string) corev1.Node {
	n := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if controlPlane {
		n.Labels = map[string]string{ControlPlaneLabel: ""}
	}
	for _, ip := range ips {
		n.Status.Addresses = append(n.Status.Addresses,
			corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: ip})
	}
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: status}}
	n.Status.NodeInfo.OSImage = osImage
	return n
}

func list(nodes ...corev1.Node) *corev1.NodeList { return &corev1.NodeList{Items: nodes} }

// ptr1-kv-cl01's control planes carry Talos' default hostnames
// (talos-np6-59s), so the role cannot be inferred from the name the way the
// Vitistack -ctp<N> scheme allows. The label is the only reliable signal.
func TestNodesFromReadsRoleFromTheLabel(t *testing.T) {
	got := NodesFrom(list(
		node("talos-np6-59s", true, true, "Talos (v1.13.6)", "100.64.0.53"),
		node("ptr1-kv-cl01-wrk01", false, true, "Talos (v1.13.6)", "100.64.0.121"),
	), false)
	if len(got) != 2 {
		t.Fatalf("NodesFrom() returned %d nodes, want 2", len(got))
	}
	if got[0].Role != cluster.RoleControlPlane {
		t.Errorf("Role = %q, want %q", got[0].Role, cluster.RoleControlPlane)
	}
	if got[1].Role != cluster.RoleWorker {
		t.Errorf("Role = %q, want %q", got[1].Role, cluster.RoleWorker)
	}
}

// Control planes first, matching how cluster.Resolve orders a topology, so a
// listing reads the same however the cluster was resolved.
func TestNodesFromPutsControlPlanesFirst(t *testing.T) {
	got := NodesFrom(list(
		node("wrk02", false, true, "Talos (v1.13.6)", "10.0.0.4"),
		node("ctp01", true, true, "Talos (v1.13.6)", "10.0.0.1"),
		node("wrk01", false, true, "Talos (v1.13.6)", "10.0.0.3"),
	), false)
	want := []string{"ctp01", "wrk01", "wrk02"}
	for i, w := range want {
		if got[i].Name != w {
			t.Errorf("node %d = %q, want %q", i, got[i].Name, w)
		}
	}
}

// Operator workstations often cannot reach v6 endpoints, and a single
// unreachable v6 address hangs talosctl until its dial timeout fires — the
// same reason cluster.Resolve defaults to v4.
func TestNodesFromFiltersIPv6ByDefault(t *testing.T) {
	n := list(node("ctp01", true, true, "Talos (v1.13.6)",
		"2a05:ec0:1018:301:be24:11ff:fe6f:1fef", "10.122.6.174"))

	if got := NodesFrom(n, false); got[0].IP != "10.122.6.174" {
		t.Errorf("IP = %q, want the v4 address", got[0].IP)
	}
	if got := NodesFrom(n, true); got[0].IP != "2a05:ec0:1018:301:be24:11ff:fe6f:1fef" {
		t.Errorf("IP = %q, want the first address with --ipv6", got[0].IP)
	}
}

func TestNodesFromRecordsReadiness(t *testing.T) {
	got := NodesFrom(list(
		node("ctp01", true, true, "Talos (v1.13.6)", "10.0.0.1"),
		node("ctp02", true, false, "Talos (v1.13.6)", "10.0.0.2"),
	), false)
	if got[0].Phase != PhaseReady {
		t.Errorf("Phase = %q, want %q", got[0].Phase, PhaseReady)
	}
	if got[1].Phase != PhaseNotReady {
		t.Errorf("Phase = %q, want %q", got[1].Phase, PhaseNotReady)
	}
}

// A node with no InternalIP is kept in the listing — it is real, and saying
// so is more useful than hiding it — but with no address to dial.
func TestNodesFromKeepsNodesWithNoAddress(t *testing.T) {
	got := NodesFrom(list(node("ctp01", true, true, "Talos (v1.13.6)")), false)
	if len(got) != 1 {
		t.Fatalf("NodesFrom() returned %d nodes, want 1", len(got))
	}
	if got[0].IP != "" {
		t.Errorf("IP = %q, want empty", got[0].IP)
	}
}

func TestTalosVersionFromOSImage(t *testing.T) {
	for in, want := range map[string]string{
		"Talos (v1.13.6)":              "v1.13.6",
		"  Talos (v1.13.6)  ":          "v1.13.6",
		"Ubuntu 24.04.1 LTS":           "",
		"":                             "",
		"Talos v1.13.6":                "",
	} {
		if got := TalosVersionFromOSImage(in); got != want {
			t.Errorf("TalosVersionFromOSImage(%q) = %q, want %q", in, got, want)
		}
	}
}

// apid on the endpoint proxies to any -n node, so one healthy control plane is
// all the tunnel needs — but it must be a healthy one, or the failure surfaces
// as a connection refused three layers down.
func TestSelectViaPicksTheFirstReadyControlPlane(t *testing.T) {
	nodes := NodesFrom(list(
		node("ctp01", true, false, "Talos (v1.13.6)", "10.0.0.1"),
		node("ctp02", true, true, "Talos (v1.13.6)", "10.0.0.2"),
		node("wrk01", false, true, "Talos (v1.13.6)", "10.0.0.3"),
	), false)
	got, err := SelectVia(nodes, "")
	if err != nil {
		t.Fatalf("SelectVia() error: %v", err)
	}
	if got.Name != "ctp02" {
		t.Errorf("SelectVia() = %q, want ctp02", got.Name)
	}
}

func TestSelectViaHonoursTheOverrideByNameOrAddress(t *testing.T) {
	nodes := NodesFrom(list(
		node("ctp01", true, true, "Talos (v1.13.6)", "10.0.0.1"),
		node("wrk01", false, true, "Talos (v1.13.6)", "10.0.0.3"),
	), false)
	for _, q := range []string{"wrk01", "10.0.0.3"} {
		got, err := SelectVia(nodes, q)
		if err != nil {
			t.Fatalf("SelectVia(%q) error: %v", q, err)
		}
		if got.Name != "wrk01" {
			t.Errorf("SelectVia(%q) = %q, want wrk01", q, got.Name)
		}
	}
}

// "no Ready control plane" has to say what it did find, because the next
// question is always "then what is there".
func TestSelectViaExplainsWhenNothingIsEligible(t *testing.T) {
	nodes := NodesFrom(list(
		node("ctp01", true, false, "Talos (v1.13.6)", "10.0.0.1"),
	), false)
	_, err := SelectVia(nodes, "")
	if err == nil {
		t.Fatal("SelectVia() succeeded, want an error")
	}
	for _, want := range []string{"ctp01", "NotReady", "--via-node"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestSelectViaRejectsAnUnknownOverride(t *testing.T) {
	nodes := NodesFrom(list(node("ctp01", true, true, "Talos (v1.13.6)", "10.0.0.1")), false)
	if _, err := SelectVia(nodes, "nope"); err == nil {
		t.Fatal("SelectVia(\"nope\") succeeded, want an error")
	}
}

// Forwarding to a node with no address would build "TCP::50000" and fail
// inside the pod, where nobody is looking.
func TestSelectViaRejectsAnOverrideWithNoAddress(t *testing.T) {
	nodes := NodesFrom(list(node("ctp01", true, true, "Talos (v1.13.6)")), false)
	if _, err := SelectVia(nodes, "ctp01"); err == nil {
		t.Fatal("SelectVia() succeeded on an addressless node, want an error")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/tunnel/ -v`
Expected: FAIL — the package does not exist yet.

- [ ] **Step 3: Export the shared IP helper and fix the Select message in `internal/cluster`**

Append to `internal/cluster/topology.go`:

```go
// PickNodeIP returns the first address worth dialling, honouring the IP-family
// preference.
//
// It is exported for callers that resolve a cluster's nodes from somewhere
// other than the management cluster — the tunnel command reads them from the
// Kubernetes API — so which addresses count as usable stays decided in one
// place rather than drifting between two.
func PickNodeIP(addrs []string, includeIPv6 bool) string {
	for _, a := range filterIPFamily(addrs, includeIPv6) {
		if isUsableNodeIP(a) {
			return a
		}
	}
	return ""
}
```

Add a `Describe` method next to `Topology` (after `NodeIPs`, around
`internal/cluster/topology.go:88`):

```go
// Describe names the cluster this topology belongs to, for error messages.
//
// A topology is not always built from a KubernetesCluster: the tunnel command
// assembles one from the Kubernetes API, where there is no management-cluster
// resource to name. Falling back to a neutral phrase keeps those errors
// readable instead of "cluster <unresolved cluster> has no node X".
func (t *Topology) Describe() string {
	if t.Cluster.KC == nil {
		return "this cluster"
	}
	return "cluster " + t.Cluster.Describe()
}
```

Then change `internal/cluster/topology.go:123-124` from:

```go
		return nil, fmt.Errorf("cluster %s has no node %s (have: %s)",
			t.Cluster.Describe(), strings.Join(missing, ", "), strings.Join(nodeNames(byRole), ", "))
```

to:

```go
		return nil, fmt.Errorf("%s has no node %s (have: %s)",
			t.Describe(), strings.Join(missing, ", "), strings.Join(nodeNames(byRole), ", "))
```

The rendered message for a resolved cluster is byte-identical; no existing
test asserts on it.

- [ ] **Step 4: Write `internal/tunnel/topology.go`**

```go
// Package tunnel reaches a Talos API that is not routable from this machine.
//
// It runs a socat pod inside the cluster's own Kubernetes cluster, forwarding
// to one control-plane node's Talos API, and brings that to localhost with a
// port-forward. apid on the endpoint proxies to any node named with -n, so one
// control plane is enough to reach all of them.
//
// The clusters this exists for — the Vitistack management clusters and the
// KubeVirt hypervisor clusters — are not KubernetesCluster resources, so
// nothing in the management cluster knows where they are. Their credentials
// are configured (see internal/config/tunnels.go); their topology is not, and
// is read from the Kubernetes API that is reachable.
package tunnel

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/vitistack/vitictl-talos/internal/cluster"
)

// ControlPlaneLabel marks a control-plane node.
//
// This is the only reliable signal here: ptr1-kv-cl01's control planes carry
// Talos' default hostnames (talos-np6-59s), so the -ctp<N> name convention
// cluster.Resolve can lean on does not hold.
const ControlPlaneLabel = "node-role.kubernetes.io/control-plane"

// Phases a node is reported in. These land in cluster.Node.Phase, which for a
// management-cluster-resolved node holds the Machine's phase instead — both
// answer "is this node going to respond".
const (
	PhaseReady    = "Ready"
	PhaseNotReady = "NotReady"
)

// talosOSImage matches what a Talos node reports as its OS image, e.g.
// "Talos (v1.13.6)". It is the only place a node's running Talos version is
// available over the Kubernetes API.
var talosOSImage = regexp.MustCompile(`^Talos \((.+)\)$`)

// NodesFrom turns a Kubernetes node listing into the nodes talosctl addresses,
// control planes first.
func NodesFrom(l *corev1.NodeList, includeIPv6 bool) []cluster.Node {
	if l == nil {
		return nil
	}
	out := make([]cluster.Node, 0, len(l.Items))
	for i := range l.Items {
		n := &l.Items[i]
		out = append(out, cluster.Node{
			Name:         n.Name,
			IP:           cluster.PickNodeIP(internalIPs(n), includeIPv6),
			Role:         roleOf(n),
			Phase:        readiness(n),
			TalosVersion: TalosVersionFromOSImage(n.Status.NodeInfo.OSImage),
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].IsControlPlane() != out[j].IsControlPlane() {
			return out[i].IsControlPlane()
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// TalosVersionFromOSImage extracts the version from a node's reported OS
// image, returning "" for anything that is not Talos.
func TalosVersionFromOSImage(osImage string) string {
	m := talosOSImage.FindStringSubmatch(strings.TrimSpace(osImage))
	if m == nil {
		return ""
	}
	return m[1]
}

// SelectVia picks the node the tunnel pod forwards to: the override when one
// is given, otherwise the first Ready control plane.
//
// It insists on Ready because the alternative failure is a connection refused
// inside a pod inside a port-forward, which says nothing about which of the
// three layers broke.
func SelectVia(nodes []cluster.Node, override string) (cluster.Node, error) {
	if override = strings.TrimSpace(override); override != "" {
		for _, n := range nodes {
			if n.Name != override && n.IP != override {
				continue
			}
			if n.IP == "" {
				return cluster.Node{}, fmt.Errorf(
					"node %s reports no usable address — the tunnel pod would have nothing to forward to", n.Name)
			}
			return n, nil
		}
		return cluster.Node{}, fmt.Errorf("no node %q here (have: %s)",
			override, strings.Join(describeNodes(nodes), ", "))
	}
	for _, n := range nodes {
		if n.IsControlPlane() && n.Phase == PhaseReady && n.IP != "" {
			return n, nil
		}
	}
	return cluster.Node{}, fmt.Errorf(
		"no Ready control-plane node with a usable address to forward through (have: %s) — "+
			"pass --via-node to use one anyway", strings.Join(describeNodes(nodes), ", "))
}

func internalIPs(n *corev1.Node) []string {
	out := make([]string, 0, len(n.Status.Addresses))
	for _, a := range n.Status.Addresses {
		if a.Type == corev1.NodeInternalIP {
			out = append(out, a.Address)
		}
	}
	return out
}

func roleOf(n *corev1.Node) string {
	if _, ok := n.Labels[ControlPlaneLabel]; ok {
		return cluster.RoleControlPlane
	}
	return cluster.RoleWorker
}

func readiness(n *corev1.Node) string {
	for _, c := range n.Status.Conditions {
		if c.Type != corev1.NodeReady {
			continue
		}
		if c.Status == corev1.ConditionTrue {
			return PhaseReady
		}
		return PhaseNotReady
	}
	return PhaseNotReady
}

// describeNodes renders the candidates for an error that has to answer "then
// what is there".
func describeNodes(nodes []cluster.Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		ip := n.IP
		if ip == "" {
			ip = "no address"
		}
		out = append(out, fmt.Sprintf("%s (%s, %s, %s)", n.Name, ip, n.Role, n.Phase))
	}
	return out
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/tunnel/ ./internal/cluster/ -v`
Expected: PASS, including every pre-existing `internal/cluster` test.

- [ ] **Step 6: Lint**

Run: `make lint`
Expected: 0 issues.

- [ ] **Step 7: Commit** *(only if the user asks)*

```bash
git add internal/tunnel/topology.go internal/tunnel/topology_test.go internal/cluster/topology.go
git commit -m "feat: resolve tunnel-cluster topology from the Kubernetes API"
```

---

### Task 3: The socat pod

The pod spec, as a pure constructor so the restricted-PodSecurity fields are
asserted rather than discovered by a rejected pod.

**Files:**
- Create: `internal/tunnel/pod.go`
- Create: `internal/tunnel/pod_test.go`

**Interfaces:**
- Consumes: `cluster.APIPort` (existing, `internal/cluster/reachability.go:12`)
- Produces:
  ```go
  const DefaultImage        = "alpine/socat"
  const PodNamePrefix       = "talos-tunnel-"
  const NameLabel           = "app.kubernetes.io/name"
  const NameLabelValue      = "talos-tunnel"
  const ManagedByLabel      = "app.kubernetes.io/managed-by"
  const ManagedByLabelValue = "viti-talos"
  const TunnelForKey        = "vitistack.io/tunnel-for"
  const ManagedBySelector   = ManagedByLabel + "=" + ManagedByLabelValue

  func PodName() (string, error)
  func Pod(name, namespace, image, targetIP, forCluster string, deadline time.Duration) *corev1.Pod
  func LabelValue(s string) string
  ```

- [ ] **Step 1: Write the failing tests**

Create `internal/tunnel/pod_test.go`:

```go
package tunnel

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
)

func testPod() *corev1.Pod {
	return Pod("talos-tunnel-3f9a2c1d", "viti-center", DefaultImage,
		"100.64.0.4", "admin@pos1-kv-cl01", 8*time.Hour)
}

// The namespaces this runs in enforce the restricted PodSecurity profile. A
// pod missing any one of these fields is rejected at admission, which is a
// slow and confusing way to learn about a typo.
func TestPodSatisfiesRestrictedPodSecurity(t *testing.T) {
	p := testPod()

	sc := p.Spec.SecurityContext
	if sc == nil {
		t.Fatal("pod has no securityContext")
	}
	if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		t.Error("runAsNonRoot is not true")
	}
	if sc.RunAsUser == nil || *sc.RunAsUser != 65534 {
		t.Errorf("runAsUser = %v, want 65534", sc.RunAsUser)
	}
	if sc.RunAsGroup == nil || *sc.RunAsGroup != 65534 {
		t.Errorf("runAsGroup = %v, want 65534", sc.RunAsGroup)
	}
	if sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Error("seccompProfile is not RuntimeDefault")
	}

	if len(p.Spec.Containers) != 1 {
		t.Fatalf("pod has %d containers, want 1", len(p.Spec.Containers))
	}
	csc := p.Spec.Containers[0].SecurityContext
	if csc == nil {
		t.Fatal("container has no securityContext")
	}
	if csc.AllowPrivilegeEscalation == nil || *csc.AllowPrivilegeEscalation {
		t.Error("allowPrivilegeEscalation is not false")
	}
	if csc.Capabilities == nil || len(csc.Capabilities.Drop) != 1 || csc.Capabilities.Drop[0] != "ALL" {
		t.Errorf("capabilities.drop = %v, want [ALL]", csc.Capabilities)
	}
}

func TestPodForwardsToTheTargetNode(t *testing.T) {
	want := []string{"TCP-LISTEN:50000,fork,reuseaddr", "TCP:100.64.0.4:50000"}
	got := testPod().Spec.Containers[0].Args
	if len(got) != len(want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("args[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// A kill -9 leaves no handler to run, so the pod has to be able to expire on
// its own or it forwards to a control plane forever.
func TestPodExpiresOnItsOwn(t *testing.T) {
	p := testPod()
	if p.Spec.ActiveDeadlineSeconds == nil {
		t.Fatal("pod has no activeDeadlineSeconds")
	}
	if *p.Spec.ActiveDeadlineSeconds != 28800 {
		t.Errorf("activeDeadlineSeconds = %d, want 28800", *p.Spec.ActiveDeadlineSeconds)
	}
	if p.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restartPolicy = %q, want Never", p.Spec.RestartPolicy)
	}
}

// The label is how an orphaned pod is found again, so it has to be on there
// even when the cluster name cannot be a label value.
func TestPodIsLabelledForCleanup(t *testing.T) {
	p := testPod()
	if p.Labels[ManagedByLabel] != ManagedByLabelValue {
		t.Errorf("%s = %q, want %q", ManagedByLabel, p.Labels[ManagedByLabel], ManagedByLabelValue)
	}
	if p.Labels[NameLabel] != NameLabelValue {
		t.Errorf("%s = %q, want %q", NameLabel, p.Labels[NameLabel], NameLabelValue)
	}
	if p.Labels[TunnelForKey] != "admin-pos1-kv-cl01" {
		t.Errorf("%s = %q, want the sanitised cluster name", TunnelForKey, p.Labels[TunnelForKey])
	}
	if p.Annotations[TunnelForKey] != "admin@pos1-kv-cl01" {
		t.Errorf("annotation %s = %q, want the name verbatim", TunnelForKey, p.Annotations[TunnelForKey])
	}
}

// A namespace with a LimitRange or a quota rejects a pod that asks for
// nothing, and socat needs almost nothing.
func TestPodDeclaresResources(t *testing.T) {
	r := testPod().Spec.Containers[0].Resources
	if r.Requests.Cpu().IsZero() || r.Requests.Memory().IsZero() {
		t.Errorf("requests = %v, want cpu and memory set", r.Requests)
	}
	if r.Limits.Cpu().IsZero() || r.Limits.Memory().IsZero() {
		t.Errorf("limits = %v, want cpu and memory set", r.Limits)
	}
}

// Cluster names here are kube contexts like admin@pos1-kv-cl01, and "@" is not
// legal in a label value — the pod would be rejected outright.
func TestLabelValueSanitises(t *testing.T) {
	for in, want := range map[string]string{
		"admin@pos1-kv-cl01": "admin-pos1-kv-cl01",
		"bgo-virt-001":       "bgo-virt-001",
		"-leading":           "leading",
		"trailing-":          "trailing",
		"":                   "unnamed",
		"@@@":                "unnamed",
	} {
		if got := LabelValue(in); got != want {
			t.Errorf("LabelValue(%q) = %q, want %q", in, got, want)
		}
	}
	long := strings.Repeat("a", 100)
	if got := LabelValue(long); len(got) != 63 {
		t.Errorf("LabelValue(<100 chars>) is %d chars, want 63", len(got))
	}
}

// Two windows, or two engineers, must not collide on one pod name.
func TestPodNameIsUniquePerCall(t *testing.T) {
	a, err := PodName()
	if err != nil {
		t.Fatalf("PodName() error: %v", err)
	}
	b, err := PodName()
	if err != nil {
		t.Fatalf("PodName() error: %v", err)
	}
	if a == b {
		t.Errorf("PodName() returned %q twice", a)
	}
	if !strings.HasPrefix(a, PodNamePrefix) {
		t.Errorf("PodName() = %q, want the %q prefix", a, PodNamePrefix)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/tunnel/ -run 'Pod|LabelValue' -v`
Expected: FAIL — `undefined: Pod`, `undefined: LabelValue`, `undefined: PodName`.

- [ ] **Step 3: Write the implementation**

Create `internal/tunnel/pod.go`:

```go
package tunnel

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vitistack/vitictl-talos/internal/cluster"
)

const (
	// DefaultImage is the socat build the manual workaround was proven with.
	// Overridable because an air-gapped zone needs its own mirror.
	DefaultImage = "alpine/socat"

	// PodNamePrefix leads a per-run random suffix, so two windows — or two
	// engineers — never collide on one pod.
	PodNamePrefix = "talos-tunnel-"

	// containerName is what a kubectl logs/describe on the pod refers to.
	containerName = "tunnel"

	// nobodyUID is the uid and gid the container runs as. Any non-root value
	// satisfies the restricted profile; socat needs no identity of its own.
	nobodyUID = int64(65534)
)

// Labels and annotations a tunnel pod carries, so an orphan is findable:
//
//	kubectl -n viti-center delete pod -l app.kubernetes.io/managed-by=viti-talos
const (
	NameLabel           = "app.kubernetes.io/name"
	NameLabelValue      = "talos-tunnel"
	ManagedByLabel      = "app.kubernetes.io/managed-by"
	ManagedByLabelValue = "viti-talos"

	// TunnelForKey names the cluster the pod was opened for. It is both a
	// label and an annotation: the label is sanitised so it is selectable,
	// the annotation carries the name verbatim so it is readable.
	TunnelForKey = "vitistack.io/tunnel-for"
)

// ManagedBySelector finds every tunnel pod this plugin created.
const ManagedBySelector = ManagedByLabel + "=" + ManagedByLabelValue

// illegalLabelChars matches everything a Kubernetes label value may not hold.
var illegalLabelChars = regexp.MustCompile(`[^A-Za-z0-9_.-]`)

// PodName returns a fresh, unique tunnel pod name.
func PodName() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating a tunnel pod name: %w", err)
	}
	return PodNamePrefix + hex.EncodeToString(b[:]), nil
}

// Pod builds the socat pod that forwards to targetIP's Talos API.
//
// Every security field here is load-bearing: the namespaces this runs in
// enforce the restricted PodSecurity profile, which rejects a pod missing any
// one of them at admission — a slow and unhelpful way to find a typo. Hence
// the pure constructor, asserted field by field in the tests.
func Pod(name, namespace, image, targetIP, forCluster string, deadline time.Duration) *corev1.Pod {
	nonRoot := true
	noEscalation := false
	uid := nobodyUID
	gid := nobodyUID
	deadlineSeconds := int64(deadline.Seconds())

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				NameLabel:      NameLabelValue,
				ManagedByLabel: ManagedByLabelValue,
				TunnelForKey:   LabelValue(forCluster),
			},
			Annotations: map[string]string{TunnelForKey: forCluster},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:         corev1.RestartPolicyNever,
			ActiveDeadlineSeconds: &deadlineSeconds,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   &nonRoot,
				RunAsUser:      &uid,
				RunAsGroup:     &gid,
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name:  containerName,
				Image: image,
				Args: []string{
					fmt.Sprintf("TCP-LISTEN:%d,fork,reuseaddr", cluster.APIPort),
					fmt.Sprintf("TCP:%s:%d", targetIP, cluster.APIPort),
				},
				Ports: []corev1.ContainerPort{{ContainerPort: cluster.APIPort}},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: &noEscalation,
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
				// Declared because a namespace with a LimitRange or a quota
				// rejects a pod that asks for nothing. socat needs almost none
				// of it.
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("10m"),
						corev1.ResourceMemory: resource.MustParse("16Mi"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("200m"),
						corev1.ResourceMemory: resource.MustParse("64Mi"),
					},
				},
			}},
		},
	}
}

// LabelValue makes s usable as a Kubernetes label value.
//
// Cluster names here are kube contexts — admin@pos1-kv-cl01 — and "@" is not
// legal in a label value, so the unsanitised name would have the pod rejected
// outright. The verbatim name survives as an annotation.
func LabelValue(s string) string {
	s = illegalLabelChars.ReplaceAllString(s, "-")
	if len(s) > 63 {
		s = s[:63]
	}
	s = strings.Trim(s, "-_.")
	if s == "" {
		return "unnamed"
	}
	return s
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/tunnel/ -v`
Expected: PASS.

- [ ] **Step 5: Lint**

Run: `make lint`
Expected: 0 issues.

- [ ] **Step 6: Commit** *(only if the user asks)*

```bash
git add internal/tunnel/pod.go internal/tunnel/pod_test.go
git commit -m "feat: build a restricted-PSA socat pod for the Talos tunnel"
```

---

### Task 4: A talosconfig from bytes

`WriteTempTalosconfig` takes a Secret; the tunnel has a file. Extract the
byte-level half rather than duplicating the context-renaming logic, and give
the "no usable context" failure the explanation it has already needed once.

**Files:**
- Modify: `internal/cluster/credentials.go:80-109` (split `WriteTempTalosconfig`),
  `internal/cluster/credentials.go:144` (the error message)
- Modify: `internal/cluster/credentials_test.go` (add two tests)

**Interfaces:**
- Produces:
  ```go
  func WriteTempTalosconfigBytes(raw []byte, contextName string, endpoints []string) (path string, cleanup func(), err error)
  ```
  `WriteTempTalosconfig(secret, contextName, endpoints)` keeps its exact
  signature and behaviour.

- [ ] **Step 1: Write the failing tests**

Append to `internal/cluster/credentials_test.go`:

```go
// The tunnel command has a talosconfig file rather than a Secret, and must
// rename its context and override its endpoints exactly as the Secret path
// does — duplicating that logic is how the two drift.
func TestWriteTempTalosconfigBytesRenamesAndOverrides(t *testing.T) {
	path, cleanup, err := WriteTempTalosconfigBytes(
		[]byte(sampleTalosconfig), "pos1-kv-cl01", []string{"127.0.0.1:54417"})
	if err != nil {
		t.Fatalf("WriteTempTalosconfigBytes() error: %v", err)
	}
	defer cleanup()

	raw, err := os.ReadFile(path) // #nosec G304 -- a path this test just created
	if err != nil {
		t.Fatal(err)
	}
	var got talosConfig
	if err := yaml.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Context != "pos1-kv-cl01" {
		t.Errorf("context = %q, want pos1-kv-cl01", got.Context)
	}
	entry := got.Contexts["pos1-kv-cl01"]
	if entry == nil {
		t.Fatalf("no context named pos1-kv-cl01 in %v", got.Contexts)
	}
	if len(entry.Endpoints) != 1 || entry.Endpoints[0] != "127.0.0.1:54417" {
		t.Errorf("endpoints = %v, want [127.0.0.1:54417]", entry.Endpoints)
	}
	if entry.CA != "Y2E=" {
		t.Errorf("ca = %q, want it carried through", entry.CA)
	}
}

// A talosconfig exported as a bare context body — endpoints/ca/crt/key at the
// top level with no context:/contexts: keys — parses into an empty struct.
// This has already cost an afternoon once, so the message has to name the
// shape rather than say "no usable context".
func TestWriteTempTalosconfigBytesExplainsABareContextBody(t *testing.T) {
	bare := "endpoints:\n    - 1.1.1.1\nca: Y2E=\ncrt: Y3J0\nkey: a2V5\n"
	_, _, err := WriteTempTalosconfigBytes([]byte(bare), "kv", nil)
	if err == nil {
		t.Fatal("WriteTempTalosconfigBytes() succeeded on a bare context body, want an error")
	}
	if !strings.Contains(err.Error(), "contexts:") {
		t.Errorf("error %q does not explain the expected shape", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/cluster/ -run WriteTempTalosconfigBytes -v`
Expected: FAIL — `undefined: WriteTempTalosconfigBytes`.

- [ ] **Step 3: Split the function**

In `internal/cluster/credentials.go`, replace the body of
`WriteTempTalosconfig` (lines 90-109) and add the new function:

```go
func WriteTempTalosconfig(secret *corev1.Secret, contextName string, endpoints []string) (path string, cleanup func(), err error) {
	raw, ok := secret.Data[KeyTalosconfig]
	if !ok || len(raw) == 0 {
		return "", nil, fmt.Errorf("secret %s/%s has no %q entry (not a Talos cluster?)",
			secret.Namespace, secret.Name, KeyTalosconfig)
	}
	return WriteTempTalosconfigBytes(raw, contextName, endpoints)
}

// WriteTempTalosconfigBytes is WriteTempTalosconfig for credentials that did
// not come from a Secret.
//
// The tunnel command reads a talosconfig from disk — the clusters it reaches
// are not KubernetesCluster resources, so no management cluster holds their
// credentials — but needs exactly the same treatment: one renamed context,
// endpoints replaced, owner-only permissions, cleaned up afterwards.
func WriteTempTalosconfigBytes(raw []byte, contextName string, endpoints []string) (path string, cleanup func(), err error) {
	dir, err := os.MkdirTemp("", "viti-talos-*")
	if err != nil {
		return "", nil, fmt.Errorf("creating temp dir: %w", err)
	}
	cleanup = func() { _ = os.RemoveAll(dir) }

	path = filepath.Join(dir, "talosconfig")
	if err := writeTalosconfigFile(raw, contextName, endpoints, path); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("writing temp talosconfig: %w", err)
	}
	return path, cleanup, nil
}
```

Keep the existing doc comment on `WriteTempTalosconfig` where it is.

- [ ] **Step 4: Improve the "no usable context" message**

In `internal/cluster/credentials.go`, replace line 144:

```go
		return fmt.Errorf("talosconfig has no usable context")
```

with:

```go
		return fmt.Errorf(
			"talosconfig has no usable context — it needs a top-level %q map; " +
				"a context exported as a bare body (endpoints/ca/crt/key at the top level) will not parse",
			"contexts:")
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/cluster/ -v`
Expected: PASS, including every pre-existing credentials test — the Secret
path's behaviour is unchanged.

- [ ] **Step 6: Lint**

Run: `make lint`
Expected: 0 issues.

- [ ] **Step 7: Commit** *(only if the user asks)*

```bash
git add internal/cluster/credentials.go internal/cluster/credentials_test.go
git commit -m "refactor: mint a temp talosconfig from bytes, not only a Secret"
```

---

### Task 5: The tunnel lifecycle

Creates the pod, waits for it, port-forwards to it, and tears all of it down on
every path out. Most of this is I/O; the three decisions that are not are
extracted and tested.

**Files:**
- Create: `internal/tunnel/tunnel.go`
- Create: `internal/tunnel/tunnel_test.go`
- Modify: `go.mod`, `go.sum` (via `go mod tidy` — adds
  `github.com/moby/spdystream` transitively)

**Interfaces:**
- Consumes: `config.TunnelCluster` (Task 1), `NodesFrom`/`SelectVia` (Task 2),
  `Pod`/`PodName`/`ManagedBySelector`/`DefaultImage` (Task 3),
  `kube.RESTConfig` (existing, `internal/kube/kube.go:85`)
- Produces:
  ```go
  type Options struct {
      Cluster     config.TunnelCluster
      Image       string
      LocalPort   int
      Via         string
      Timeout     time.Duration
      Deadline    time.Duration
      IncludeIPv6 bool
      Warn        func(error)
      Err         io.Writer
  }

  type Tunnel struct {
      LocalAddr string
      Via       cluster.Node
      Nodes     []cluster.Node
      PodName   string
      Namespace string
      // unexported fields
  }

  func Open(ctx context.Context, o Options) (*Tunnel, error)
  func (t *Tunnel) Close()
  func PortSpec(local int) string
  func NotReadyReason(p *corev1.Pod) string

  const DefaultTimeout  = 60 * time.Second
  const DefaultDeadline = 8 * time.Hour
  ```

- [ ] **Step 1: Write the failing tests**

Create `internal/tunnel/tunnel_test.go`:

```go
package tunnel

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// 50000 is frequently already bound — by a talosctl of your own, or a second
// tunnel — so the default asks the kernel for a free port instead of turning a
// second window into a confusing failure.
func TestPortSpecDefaultsToAnEphemeralLocalPort(t *testing.T) {
	if got := PortSpec(0); got != "0:50000" {
		t.Errorf("PortSpec(0) = %q, want 0:50000", got)
	}
	if got := PortSpec(50000); got != "50000:50000" {
		t.Errorf("PortSpec(50000) = %q, want 50000:50000", got)
	}
}

// "timed out waiting for the pod" is not a diagnosis. In an air-gapped zone
// the first failure will be an image pull, and the pod already knows that.
func TestNotReadyReasonReportsTheContainerState(t *testing.T) {
	p := &corev1.Pod{}
	p.Status.Phase = corev1.PodPending
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: containerName,
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
			Reason:  "ImagePullBackOff",
			Message: "Back-off pulling image \"alpine/socat\"",
		}},
	}}
	got := NotReadyReason(p)
	for _, want := range []string{"Pending", "ImagePullBackOff", "Back-off pulling image"} {
		if !strings.Contains(got, want) {
			t.Errorf("NotReadyReason() = %q, does not mention %q", got, want)
		}
	}
}

func TestNotReadyReasonHandlesAPodThatWasNeverFetched(t *testing.T) {
	if got := NotReadyReason(nil); got == "" {
		t.Error("NotReadyReason(nil) = \"\", want something readable")
	}
}

// Close runs from a defer, from a signal handler, and again from the deferred
// call after the signal handler — it must survive all three.
func TestCloseIsIdempotentOnAnUnopenedTunnel(t *testing.T) {
	var tun *Tunnel
	tun.Close()

	tun = &Tunnel{}
	tun.Close()
	tun.Close()
}

func TestOptionsApplyDefaults(t *testing.T) {
	var o Options
	o.applyDefaults()
	if o.Image != DefaultImage {
		t.Errorf("Image = %q, want %q", o.Image, DefaultImage)
	}
	if o.Timeout != DefaultTimeout {
		t.Errorf("Timeout = %v, want %v", o.Timeout, DefaultTimeout)
	}
	if o.Deadline != DefaultDeadline {
		t.Errorf("Deadline = %v, want %v", o.Deadline, DefaultDeadline)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/tunnel/ -run 'PortSpec|NotReadyReason|Close|Options' -v`
Expected: FAIL — `undefined: PortSpec`, `undefined: NotReadyReason`,
`undefined: Tunnel`, `undefined: Options`.

- [ ] **Step 3: Write the implementation**

Create `internal/tunnel/tunnel.go`:

```go
package tunnel

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"

	"github.com/vitistack/vitictl-talos/internal/cluster"
	"github.com/vitistack/vitictl-talos/internal/config"
	"github.com/vitistack/vitictl-talos/internal/kube"
)

const (
	// DefaultTimeout bounds how long the tunnel pod has to become ready.
	// Generous enough for a cold image pull, short enough that a wedged pod
	// is reported rather than waited on.
	DefaultTimeout = 60 * time.Second

	// DefaultDeadline is the pod's own activeDeadlineSeconds: the backstop for
	// the one exit path no handler can catch, a kill -9.
	DefaultDeadline = 8 * time.Hour

	// deleteTimeout bounds the teardown delete, which runs on a context
	// deliberately detached from the cancelled one.
	deleteTimeout = 30 * time.Second

	// pollInterval is how often the pod is checked while waiting for Ready.
	pollInterval = 500 * time.Millisecond
)

// Options configures one tunnel.
type Options struct {
	Cluster     config.TunnelCluster
	Image       string
	LocalPort   int
	Via         string
	Timeout     time.Duration
	Deadline    time.Duration
	IncludeIPv6 bool
	// Warn reports non-fatal problems — a pre-existing tunnel pod, a delete
	// that did not land — without failing the command.
	Warn func(error)
	// Err receives the port-forwarder's own error output.
	Err io.Writer
}

func (o *Options) applyDefaults() {
	if strings.TrimSpace(o.Image) == "" {
		o.Image = DefaultImage
	}
	if o.Timeout <= 0 {
		o.Timeout = DefaultTimeout
	}
	if o.Deadline <= 0 {
		o.Deadline = DefaultDeadline
	}
	if o.Warn == nil {
		o.Warn = func(error) {}
	}
	if o.Err == nil {
		o.Err = io.Discard
	}
}

// Tunnel is an open path to a cluster's Talos API: a socat pod inside the
// cluster and a port-forward to it from here.
//
// It mirrors cluster.Session: opened once, torn down on every path out
// including the ones that fail, and Close is idempotent. The difference is
// that a Session leaves only a temp file behind, while an un-closed Tunnel
// leaves a pod running in somebody's cluster.
type Tunnel struct {
	// LocalAddr is what talosctl connects to, e.g. "127.0.0.1:54417".
	LocalAddr string
	// Via is the control-plane node the pod forwards to.
	Via cluster.Node
	// Nodes is every node of the cluster, as discovered from its Kubernetes
	// API — control planes first.
	Nodes []cluster.Node

	PodName   string
	Namespace string

	clientset kubernetes.Interface
	stop      chan struct{}
	warn      func(error)
}

// Open creates the tunnel pod and brings its Talos API to localhost.
//
// Every step that can fail tears down what the steps before it created, so a
// failure never leaves a pod behind.
func Open(ctx context.Context, o Options) (*Tunnel, error) {
	o.applyDefaults()

	rc, err := kube.RESTConfig(o.Cluster.Kubeconfig, o.Cluster.Context)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", o.Cluster.Name, err)
	}
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return nil, fmt.Errorf("%s: building a kubernetes client: %w", o.Cluster.Name, err)
	}

	nodeList, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("%s: listing nodes through context %s: %w",
			o.Cluster.Name, o.Cluster.Context, err)
	}
	nodes := NodesFrom(nodeList, o.IncludeIPv6)
	via, err := SelectVia(nodes, o.Via)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", o.Cluster.Name, err)
	}

	warnAboutOrphans(ctx, cs, o)

	name, err := PodName()
	if err != nil {
		return nil, err
	}
	pod := Pod(name, o.Cluster.Namespace, o.Image, via.IP, o.Cluster.Name, o.Deadline)
	if _, err := cs.CoreV1().Pods(o.Cluster.Namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		return nil, fmt.Errorf("%s: creating tunnel pod %s/%s: %w",
			o.Cluster.Name, o.Cluster.Namespace, name, err)
	}

	t := &Tunnel{
		Via:       via,
		Nodes:     nodes,
		PodName:   name,
		Namespace: o.Cluster.Namespace,
		clientset: cs,
		stop:      make(chan struct{}),
		warn:      o.Warn,
	}
	if err := t.waitReady(ctx, o.Timeout); err != nil {
		t.Close()
		return nil, err
	}
	if err := t.forward(ctx, rc, cs, o); err != nil {
		t.Close()
		return nil, err
	}
	return t, nil
}

// Close stops the port-forward and deletes the pod. It is idempotent, and safe
// on a nil receiver, because it is reached from a defer, from a signal, and
// from the defer that runs after the signal.
func (t *Tunnel) Close() {
	if t == nil {
		return
	}
	if t.stop != nil {
		close(t.stop)
		t.stop = nil
	}
	if t.clientset == nil || t.PodName == "" {
		return
	}
	cs := t.clientset
	t.clientset = nil

	// A fresh context on purpose, not the caller's. Teardown is usually
	// triggered *by* the caller's context being cancelled — Ctrl-C — and
	// inheriting it would cancel the delete along with everything else,
	// leaving the pod running: the one outcome this function exists to
	// prevent. Nothing the delete needs travels in a context; the credentials
	// are in the clientset.
	ctx, cancel := context.WithTimeout(context.Background(), deleteTimeout)
	defer cancel()

	grace := int64(0)
	err := cs.CoreV1().Pods(t.Namespace).Delete(ctx, t.PodName,
		metav1.DeleteOptions{GracePeriodSeconds: &grace})
	if err != nil && !apierrors.IsNotFound(err) && t.warn != nil {
		t.warn(fmt.Errorf(
			"tunnel pod %s/%s was not deleted (%v) — remove it with: kubectl -n %s delete pod %s",
			t.Namespace, t.PodName, err, t.Namespace, t.PodName))
	}
}

// waitReady blocks until the tunnel pod can accept a connection.
func (t *Tunnel) waitReady(ctx context.Context, timeout time.Duration) error {
	var last *corev1.Pod
	err := wait.PollUntilContextTimeout(ctx, pollInterval, timeout, true,
		func(ctx context.Context) (bool, error) {
			p, err := t.clientset.CoreV1().Pods(t.Namespace).Get(ctx, t.PodName, metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			last = p
			return podReady(p), nil
		})
	if err == nil {
		return nil
	}
	if last == nil {
		return fmt.Errorf("waiting for tunnel pod %s/%s: %w", t.Namespace, t.PodName, err)
	}
	return fmt.Errorf("tunnel pod %s/%s did not become ready within %s: %s",
		t.Namespace, t.PodName, timeout, NotReadyReason(last))
}

// forward brings the pod's Talos API port to localhost.
func (t *Tunnel) forward(ctx context.Context, rc *rest.Config, cs *kubernetes.Clientset, o Options) error {
	rt, upgrader, err := spdy.RoundTripperFor(rc)
	if err != nil {
		return fmt.Errorf("building the port-forward transport: %w", err)
	}
	u := cs.CoreV1().RESTClient().Post().
		Resource("pods").Namespace(t.Namespace).Name(t.PodName).
		SubResource("portforward").URL()
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: rt}, http.MethodPost, u)

	ready := make(chan struct{})
	// The forwarder's own stdout is discarded: it prints its own
	// "Forwarding from …" line, and this command prints a better one.
	pf, err := portforward.New(dialer, []string{PortSpec(o.LocalPort)}, t.stop, ready, io.Discard, o.Err)
	if err != nil {
		return fmt.Errorf("preparing the port-forward: %w", err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- pf.ForwardPorts() }()

	select {
	case <-ready:
	case err := <-errCh:
		return fmt.Errorf("port-forward to %s/%s failed: %w", t.Namespace, t.PodName, err)
	case <-ctx.Done():
		return ctx.Err()
	}

	ports, err := pf.GetPorts()
	if err != nil {
		return fmt.Errorf("reading the forwarded port: %w", err)
	}
	if len(ports) == 0 {
		return fmt.Errorf("port-forward to %s/%s bound no port", t.Namespace, t.PodName)
	}
	t.LocalAddr = net.JoinHostPort("127.0.0.1", strconv.Itoa(int(ports[0].Local)))
	return nil
}

// PortSpec renders the port-forward mapping. A local port of 0 asks the kernel
// for a free one, which is the default because 50000 is so often taken.
func PortSpec(local int) string {
	return fmt.Sprintf("%d:%d", local, cluster.APIPort)
}

// podReady reports whether the pod will accept a connection.
func podReady(p *corev1.Pod) bool {
	if p == nil || p.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// NotReadyReason explains why a pod is not ready yet, in the pod's own words.
//
// "timed out waiting for the pod" is not a diagnosis, and the first failure in
// a zone without a mirror for alpine/socat will be an image pull — which the
// pod's container status already says plainly.
func NotReadyReason(p *corev1.Pod) string {
	if p == nil {
		return "the pod could not be read back after it was created"
	}
	parts := []string{"phase " + string(p.Status.Phase)}
	for _, cs := range p.Status.ContainerStatuses {
		w := cs.State.Waiting
		if w == nil {
			continue
		}
		detail := w.Reason
		if w.Message != "" {
			detail += ": " + w.Message
		}
		parts = append(parts, fmt.Sprintf("container %s %s", cs.Name, detail))
	}
	return strings.Join(parts, ", ")
}

// warnAboutOrphans reports tunnel pods already in the namespace.
//
// Reported, not deleted: a colleague may be holding one open right now, and
// deleting it would drop their session with no warning. Reporting is enough —
// the label makes the cleanup a one-liner, which the message prints.
func warnAboutOrphans(ctx context.Context, cs kubernetes.Interface, o Options) {
	list, err := cs.CoreV1().Pods(o.Cluster.Namespace).List(ctx,
		metav1.ListOptions{LabelSelector: ManagedBySelector})
	if err != nil || len(list.Items) == 0 {
		return
	}
	names := make([]string, 0, len(list.Items))
	for i := range list.Items {
		names = append(names, list.Items[i].Name)
	}
	o.Warn(fmt.Errorf(
		"%d tunnel pod(s) already in %s/%s (%s) — someone may be using them; "+
			"clean up with: kubectl -n %s delete pod -l %s",
		len(names), o.Cluster.Name, o.Cluster.Namespace, strings.Join(names, ", "),
		o.Cluster.Namespace, ManagedBySelector))
}
```

- [ ] **Step 4: Resolve the new dependency**

Run: `go mod tidy`
Expected: `go.mod` gains `github.com/moby/spdystream` and
`github.com/gorilla/websocket` as indirect requirements.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/tunnel/ -v && go build ./...`
Expected: PASS, and a clean build.

- [ ] **Step 6: Lint and scan**

Run: `make lint && make gosec`
Expected: 0 issues from both.

- [ ] **Step 7: Commit** *(only if the user asks)*

```bash
git add internal/tunnel/tunnel.go internal/tunnel/tunnel_test.go go.mod go.sum
git commit -m "feat: open and tear down a Talos API tunnel pod"
```

---

### Task 6: The `tunnel` command

**Files:**
- Create: `cmd/tunnel.go`
- Create: `cmd/tunnel_test.go`
- Modify: `cmd/root.go:93-103` (add to the command tree)
- Modify: `cmd/root_test.go:16-20` (add to the asserted tree)

**Interfaces:**
- Consumes: `config.TunnelClusters`/`FindTunnelCluster` (Task 1),
  `tunnel.Open`/`tunnel.Options` (Task 5),
  `cluster.WriteTempTalosconfigBytes` (Task 4), and the existing
  `splitArgs`/`nodeSelector`/`parseRole`/`nodeAddresses`/`streams`/`echo`/`warn`
  helpers in package `cmd`
- Produces: `func newTunnelCmd(s *scope) *cobra.Command`

- [ ] **Step 1: Write the failing tests**

Create `cmd/tunnel_test.go`:

```go
package cmd

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/vitistack/vitictl-talos/internal/cluster"
	"github.com/vitistack/vitictl-talos/internal/config"
)

// find locates a command in the tree. Task 7's cmd/config_test.go reuses it.
func find(t *testing.T, root *cobra.Command, name string) *cobra.Command {
	t.Helper()
	for _, c := range root.Commands() {
		if c.Name() == name {
			return c
		}
	}
	t.Fatalf("command %q is not in the tree", name)
	return nil
}

// -n is already the root's "limit to this namespace". Defining a second
// --namespace here would give one flag two meanings on one command, so the
// tunnel's own namespace flag has to be a different name.
func TestTunnelDoesNotRedefineNamespace(t *testing.T) {
	tun := find(t, NewRootCmd(), "tunnel")
	if tun.Flags().Lookup("tunnel-namespace") == nil {
		t.Error("--tunnel-namespace is not registered")
	}
	// LocalFlags excludes inherited persistent flags, so a hit here means the
	// command declared its own and shadowed the root's.
	if tun.LocalNonPersistentFlags().Lookup("namespace") != nil {
		t.Error("tunnel declares its own --namespace, shadowing the root's")
	}
}

func TestTunnelRegistersItsFlags(t *testing.T) {
	tun := find(t, NewRootCmd(), "tunnel")
	for _, name := range []string{"via-node", "tunnel-namespace", "image", "local-port", "timeout", "deadline", "node", "role"} {
		if tun.Flags().Lookup(name) == nil {
			t.Errorf("--%s is not registered", name)
		}
	}
}

// "portfwd" is what the muscle memory reaches for after a week of kubectl.
func TestTunnelIsAliasedAsPortfwd(t *testing.T) {
	tun := find(t, NewRootCmd(), "tunnel")
	for _, a := range tun.Aliases {
		if a == "portfwd" {
			return
		}
	}
	t.Errorf("aliases = %v, want portfwd among them", tun.Aliases)
}

// Everything after -- goes to talosctl as a subcommand plus its flags. Leading
// with a flag would build "talosctl --follow --talosconfig …", which fails
// with a message about the flag rather than about the mistake.
func TestTunnelRejectsPassthroughStartingWithAFlag(t *testing.T) {
	err := checkPassthrough([]string{"--follow"})
	if err == nil {
		t.Fatal("checkPassthrough() accepted a leading flag, want an error")
	}
	if !strings.Contains(err.Error(), "subcommand") {
		t.Errorf("error %q does not say what was expected", err)
	}
	if err := checkPassthrough([]string{"dmesg", "--follow"}); err != nil {
		t.Errorf("checkPassthrough() rejected a valid passthrough: %v", err)
	}
	if err := checkPassthrough(nil); err != nil {
		t.Errorf("checkPassthrough(nil) = %v, want nil", err)
	}
}

// The wrong talosconfig for the right cluster fails as "x509: certificate
// signed by unknown authority", which reads as a broken cluster. talosctl's
// output does not come back through this process, so the hint has to name
// both halves of the mismatch itself.
func TestCredentialHintNamesTheFileAndTheCluster(t *testing.T) {
	got := credentialHint(config.TunnelCluster{Talosconfig: "/a/b", Context: "admin@kv"})
	for _, want := range []string{"x509", "/a/b", "admin@kv"} {
		if !strings.Contains(got, want) {
			t.Errorf("credentialHint() = %q, does not mention %q", got, want)
		}
	}
}

// The printed line is meant to be pasted, so -e must be absent (the temp
// config carries it) and the node list must be the selected nodes.
func TestTunnelUsageLineIsPasteable(t *testing.T) {
	got := tunnelUsage("/tmp/x/talosconfig", []cluster.Node{
		{Name: "ctp01", IP: "100.64.0.4"},
		{Name: "ctp02", IP: "100.64.0.5"},
	})
	if !strings.Contains(got, "export TALOSCONFIG=/tmp/x/talosconfig") {
		t.Errorf("usage %q does not export TALOSCONFIG", got)
	}
	if !strings.Contains(got, "talosctl -n 100.64.0.4,100.64.0.5") {
		t.Errorf("usage %q does not list the selected nodes", got)
	}
	if strings.Contains(got, "-e ") {
		t.Errorf("usage %q still passes -e; the temp config carries the endpoint", got)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./cmd/ -run Tunnel -v`
Expected: FAIL — `command "tunnel" is not in the tree`, `undefined: checkPassthrough`,
`undefined: tunnelUsage`.

- [ ] **Step 3: Write the command**

Create `cmd/tunnel.go`:

```go
package cmd

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/vitistack/vitictl-talos/internal/cluster"
	"github.com/vitistack/vitictl-talos/internal/config"
	"github.com/vitistack/vitictl-talos/internal/talosctl"
	"github.com/vitistack/vitictl-talos/internal/tunnel"
	"github.com/vitistack/vitictl/pkg/plugin/picker"
)

const tunnelLong = `Reach a cluster's Talos API through a pod inside its Kubernetes cluster.

Some Talos clusters answer on the Kubernetes API but not on tcp/50000 from
where you are sitting — the Vitistack management clusters and the KubeVirt
hypervisor clusters are the usual case. This runs a small socat pod inside the
cluster, forwards one control-plane node's Talos API to it, and brings that to
localhost. apid on that endpoint proxies to every other node, so one control
plane reaches all of them.

These clusters are not KubernetesCluster resources, so nothing in the
management cluster knows where they are or how to authenticate to them. Name
them in talos.yaml beside vitictl's own config ("viti talos config path"):

  clusters:
    - context: admin@pos1-kv-cl01
      talosconfig: ~/.talos/kvosltalos

Their nodes are not configured — they are read from the Kubernetes API each
time, so a rebuilt cluster needs no edit here.

With a -- , a talosctl command is run through the tunnel and everything is torn
down afterwards. Without one, the tunnel is held open and the talosctl line to
paste is printed, until Ctrl-C.

The pod is deleted on every exit path, and carries an activeDeadlineSeconds so
that even a killed process cannot leave one running forever.`

const tunnelExample = `  viti talos tunnel pos1-kv-cl01
  viti talos tunnel pos1-kv-cl01 -- get members
  viti talos tunnel pos1-kv-cl01 --role controlplane -- dmesg
  viti talos tunnel pos1-kv-cl01 --local-port 50000`

// tunnelOpts holds the flags specific to opening a tunnel.
type tunnelOpts struct {
	via       string
	namespace string
	image     string
	localPort int
	timeout   time.Duration
	deadline  time.Duration
}

func (o *tunnelOpts) register(cmd *cobra.Command) {
	cmd.Flags().StringVar(&o.via, "via-node", "",
		"control-plane node to forward through, by name or address (default: the first Ready one)")
	// Deliberately not -n/--namespace: that is the root's "limit to this
	// namespace", and one flag with two meanings on one command is a trap.
	cmd.Flags().StringVar(&o.namespace, "tunnel-namespace", "",
		"namespace to create the tunnel pod in (default: from talos.yaml, else "+config.DefaultTunnelNamespace+")")
	cmd.Flags().StringVar(&o.image, "image", tunnel.DefaultImage,
		"socat image for the tunnel pod")
	cmd.Flags().IntVar(&o.localPort, "local-port", 0,
		"local port to bind (default: a free one, printed once bound)")
	cmd.Flags().DurationVar(&o.timeout, "timeout", tunnel.DefaultTimeout,
		"how long to wait for the tunnel pod to become ready")
	cmd.Flags().DurationVar(&o.deadline, "deadline", tunnel.DefaultDeadline,
		"pod lifetime cap, so a killed process cannot leave one running")
}

func newTunnelCmd(s *scope) *cobra.Command {
	n := &nodeSelector{}
	o := &tunnelOpts{}

	cmd := &cobra.Command{
		Use:     "tunnel [cluster] [-- talosctl command]",
		Aliases: []string{"portfwd"},
		Short:   "🛣️  Reach a Talos API that is not routable from here",
		Long:    tunnelLong,
		Example: tunnelExample,
		Args:    cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTunnel(cmd, s, n, o, args)
		},
	}
	n.register(cmd)
	o.register(cmd)
	return cmd
}

func runTunnel(cmd *cobra.Command, s *scope, n *nodeSelector, o *tunnelOpts, args []string) error {
	name, passthrough, err := splitArgs(cmd, args)
	if err != nil {
		return err
	}
	if err := checkPassthrough(passthrough); err != nil {
		return err
	}
	role, err := parseRole(n.role)
	if err != nil {
		return err
	}
	tc, err := selectTunnelCluster(cmd, name)
	if err != nil {
		return err
	}
	if o.namespace != "" {
		tc.Namespace = o.namespace
	}
	raw, err := os.ReadFile(tc.Talosconfig) // #nosec G304 -- the path is this plugin's own config value
	if err != nil {
		return fmt.Errorf("reading the talosconfig configured for %s: %w", tc.Name, err)
	}

	// Ctrl-C must unwind through the deferred Close rather than killing the
	// process, which would leave the pod running in someone's cluster.
	ctx, stopSignals := signal.NotifyContext(contextOrBackground(cmd), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	tun, err := tunnel.Open(ctx, tunnel.Options{
		Cluster:     tc,
		Image:       o.image,
		LocalPort:   o.localPort,
		Via:         o.via,
		Timeout:     o.timeout,
		Deadline:    o.deadline,
		IncludeIPv6: s.ipv6,
		Warn:        func(e error) { warn(cmd, e) },
		Err:         cmd.ErrOrStderr(),
	})
	if err != nil {
		return err
	}
	defer tun.Close()

	nodes, err := (&cluster.Topology{Nodes: tun.Nodes}).Select(n.nodes, role)
	if err != nil {
		return fmt.Errorf("%s: %w", tc.Name, err)
	}

	path, cleanup, err := cluster.WriteTempTalosconfigBytes(raw, tc.Name, []string{tun.LocalAddr})
	if err != nil {
		return fmt.Errorf("talosconfig %s: %w", tc.Talosconfig, err)
	}
	defer cleanup()

	echo(cmd, fmt.Sprintf("%s — tunnel via %s/%s → %s:%d",
		tc.Name, tun.Namespace, tun.PodName, tun.Via.IP, cluster.APIPort))
	echo(cmd, "ready on "+tun.LocalAddr)

	target := talosctl.Target{
		Talosconfig: path,
		// Passed explicitly even though the temp config carries them, per this
		// package's rule that a target is never left to ambient state.
		Endpoints: []string{tun.LocalAddr},
		Nodes:     nodeAddresses(nodes),
	}
	if len(passthrough) > 0 {
		if runErr := talosctl.Run(ctx, streams(cmd), target, passthrough[0], passthrough[1:]...); runErr != nil {
			return fmt.Errorf("%w\n%s", runErr, credentialHint(tc))
		}
		return nil
	}

	_, _ = fmt.Fprint(cmd.ErrOrStderr(), tunnelUsage(path, nodes))
	<-ctx.Done()
	echo(cmd, "tearing down")
	return nil
}

// checkPassthrough rejects a talosctl invocation that leads with a flag.
//
// build() puts the subcommand first, so "-- --follow" would produce
// "talosctl --follow --talosconfig …" and fail with a message about the flag
// rather than about the mistake.
func checkPassthrough(passthrough []string) error {
	if len(passthrough) == 0 || !strings.HasPrefix(passthrough[0], "-") {
		return nil
	}
	return fmt.Errorf(
		"the first word after -- must be a talosctl subcommand, got %q "+
			"(e.g. 'viti talos tunnel my-cluster -- dmesg --follow')", passthrough[0])
}

// credentialHint follows a failed talosctl run through a tunnel.
//
// talosctl's output streams straight to the terminal rather than through this
// process, so an exit code is all that comes back here — there is nothing to
// pattern-match on. The one failure worth naming in advance is a talosconfig
// for the wrong cluster: it surfaces as "x509: certificate signed by unknown
// authority", which reads as a broken cluster rather than as a mismatched
// file, and the tunnel makes it likelier by decoupling the credentials from
// the cluster they were fetched for.
func credentialHint(tc config.TunnelCluster) string {
	return fmt.Sprintf(
		"   if that was \"x509: certificate signed by unknown authority\", then %s holds "+
			"credentials for a different cluster than %s", tc.Talosconfig, tc.Context)
}

// tunnelUsage renders the block printed when the tunnel is held open.
//
// No -e: the temp talosconfig already names the local endpoint, so what is
// printed is the shortest line that actually works.
func tunnelUsage(talosconfig string, nodes []cluster.Node) string {
	var b strings.Builder
	b.WriteString("\n  export TALOSCONFIG=" + talosconfig + "\n")
	b.WriteString("  talosctl -n " + strings.Join(nodeAddresses(nodes), ",") + " <command>\n")
	b.WriteString("\n(Ctrl-C to tear down)\n")
	return b.String()
}

// selectTunnelCluster resolves the configured cluster to open a tunnel to.
func selectTunnelCluster(cmd *cobra.Command, name string) (config.TunnelCluster, error) {
	clusters, err := config.TunnelClusters()
	if err != nil {
		return config.TunnelCluster{}, err
	}
	if name != "" {
		found, ok := config.FindTunnelCluster(clusters, name)
		if !ok {
			return config.TunnelCluster{}, fmt.Errorf(
				"no tunnel cluster named %q — configured: %s",
				name, strings.Join(tunnelClusterNames(clusters), ", "))
		}
		return found, nil
	}
	if len(clusters) == 1 {
		return clusters[0], nil
	}
	if !picker.Interactive() {
		return config.TunnelCluster{}, fmt.Errorf(
			"no cluster given — pass one (configured: %s), or run in a terminal to pick one interactively",
			strings.Join(tunnelClusterNames(clusters), ", "))
	}
	return pickTunnelCluster(cmd, clusters)
}

func pickTunnelCluster(cmd *cobra.Command, clusters []config.TunnelCluster) (config.TunnelCluster, error) {
	items := make([]picker.Item, 0, len(clusters))
	for _, c := range clusters {
		columns := []string{c.Name, c.Context, c.Namespace, c.Talosconfig}
		items = append(items, picker.Item{
			Label:   strings.Join(columns, " "),
			Columns: columns,
			Value:   c,
		})
	}
	chosen, err := picker.Select(" Select a cluster to tunnel to ",
		[]string{"NAME", "CONTEXT", "NAMESPACE", "TALOSCONFIG"}, items)
	if err != nil {
		if errors.Is(err, picker.ErrCancelled) {
			return config.TunnelCluster{}, errCancelled
		}
		return config.TunnelCluster{}, err
	}
	got, ok := chosen.Value.(config.TunnelCluster)
	if !ok {
		return config.TunnelCluster{}, fmt.Errorf("picker returned an unexpected item %T", chosen.Value)
	}
	echo(cmd, got.Name)
	return got, nil
}

func tunnelClusterNames(in []config.TunnelCluster) []string {
	out := make([]string, 0, len(in))
	for _, c := range in {
		out = append(out, c.Name)
	}
	return out
}
```

- [ ] **Step 4: Wire it into the tree**

In `cmd/root.go`, add `newTunnelCmd(s),` to the `root.AddCommand(...)` call
(after `newUpgradeK8sCmd(s),`, before `newConfigCmd(s),`).

In `cmd/root_test.go`, add `"tunnel"` to the `want` slice in
`TestRootCommandTree`.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./cmd/ -v`
Expected: PASS, including the pre-existing tree and help-text tests.

- [ ] **Step 6: Verify the help renders**

Run: `go run . tunnel --help`
Expected: the long help above, with all eight flags listed and `portfwd` shown
as an alias.

- [ ] **Step 7: Lint and scan**

Run: `make lint && make gosec`
Expected: 0 issues.

- [ ] **Step 8: Commit** *(only if the user asks)*

```bash
git add cmd/tunnel.go cmd/tunnel_test.go cmd/root.go cmd/root_test.go
git commit -m "feat: add 'viti talos tunnel' for unroutable Talos APIs"
```

---

### Task 7: Diagnostics and documentation

`config path` and `config test` are where someone goes when a tunnel does not
work, so they have to know about the new file. And the README needs the
section.

**Files:**
- Modify: `cmd/config.go:13-48` (long help and `config path`), `cmd/config.go:50-110`
  (`config test`)
- Modify: `cmd/config_test.go` — create if absent
- Modify: `README.md` (new section after line 105, and the command table at
  line 106)

**Interfaces:**
- Consumes: `config.TalosConfigPath`, `config.TunnelClusters` (Task 1)
- Produces: nothing new

- [ ] **Step 1: Write the failing test**

Create `cmd/config_test.go`:

```go
package cmd

import (
	"bytes"
	"strings"
	"testing"
)

// "viti talos config path" is where someone goes when a tunnel cannot find its
// credentials, so it has to name the file that holds them.
func TestConfigPathNamesTheTalosConfigFile(t *testing.T) {
	root := NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"config", "path"})
	if err := root.Execute(); err != nil {
		t.Fatalf("config path: %v", err)
	}
	if !strings.Contains(out.String(), "talos.yaml") {
		t.Errorf("config path output does not mention talos.yaml:\n%s", out.String())
	}
}

// The long help said, in as many words, that this plugin has no config file of
// its own. It now has one, and the help is where that is discovered.
func TestConfigHelpDescribesTheTunnelConfig(t *testing.T) {
	cmd := find(t, NewRootCmd(), "config")
	if !strings.Contains(cmd.Long, "talos.yaml") {
		t.Errorf("config --help does not mention talos.yaml:\n%s", cmd.Long)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./cmd/ -run Config -v`
Expected: FAIL — the output does not mention `talos.yaml`.

- [ ] **Step 3: Update `cmd/config.go`**

Replace the `Long` on `newConfigCmd` (lines 18-26) with:

```go
		Long: `viti-talos discovers almost everything it needs.

The availability zones come from vitictl's own ctl.config.yaml ("viti config
add"), and each Talos cluster's credentials come from the Secret its
management cluster holds — duplicating either here would only give the two
CLIs something to drift on.

The exception is talos.yaml, beside that file, which names the clusters that
are not KubernetesCluster resources — the management clusters and the KubeVirt
hypervisor clusters. Nothing in the management cluster knows their Talos
credentials, so "viti talos tunnel" is the one thing that has to be told. Its
nodes are still discovered rather than configured.

These subcommands are for finding out what that all resolves to when something
does not work.`,
```

Replace the body of `newConfigPathCmd`'s `RunE` (lines 37-46) with:

```go
		RunE: func(cmd *cobra.Command, _ []string) error {
			path, err := config.VitistackConfigPath()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(out, "vitistack (viti):  %s\n", path)

			tpath, err := config.TalosConfigPath()
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(out, "tunnel clusters:   %s%s\n", tpath, existsSuffix(tpath))
			_, err = fmt.Fprintf(out, "talosconfig:       minted per command from each cluster's Secret (never written to ~/.talos/config)\n")
			return err
		},
```

and add, at the bottom of `cmd/config.go`:

```go
// existsSuffix marks a configured path that is not there, since "the file you
// are looking at is absent" is the answer to most questions that reach here.
func existsSuffix(path string) string {
	if _, err := os.Stat(path); err != nil {
		return " (not present)"
	}
	return ""
}
```

adding `"os"` to that file's imports.

Then, in `newConfigTestCmd`'s `RunE`, insert this immediately before the final
`if failed {` block:

```go
			// Reported but never fatal: most people never open a tunnel, and a
			// missing talos.yaml is the normal state for them.
			switch tunnels, err := config.TunnelClusters(); {
			case err != nil:
				_, _ = fmt.Fprintf(out, "➖ tunnel clusters: none configured (%s)\n",
					firstLine(err.Error()))
			default:
				for _, tc := range tunnels {
					if _, statErr := os.Stat(tc.Talosconfig); statErr != nil {
						report("tunnel cluster "+tc.Name,
							fmt.Errorf("talosconfig %s is not readable: %w", tc.Talosconfig, statErr))
						continue
					}
					report(fmt.Sprintf("tunnel cluster %s (%s)", tc.Name, tc.Context), nil)
				}
			}
```

and add:

```go
// firstLine trims a multi-line error down for a one-line report. The
// missing-config error deliberately carries an example of the file format,
// which is right in isolation and too much inside a checklist.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
```

adding `"strings"` to the imports.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./cmd/ -v`
Expected: PASS.

- [ ] **Step 5: Add the README section**

In `README.md`, add this row to the command table immediately after the
`upgrade-k8s` row (line 118):

```markdown
| `tunnel` | Reach a Talos API that is not routable from here, through a pod |
```

Then insert this section immediately before
`## Changing machine configs across the fleet` (line 132):

````markdown
## Reaching a cluster the Talos API cannot see

Some clusters answer on the Kubernetes API but not on tcp/50000 from where you
are sitting. The Vitistack management clusters and the KubeVirt hypervisor
clusters are the usual case: `viti talos nodes <cluster> --probe` shows ❌ on
every node, while `kubectl` against the same cluster works fine.

`viti talos tunnel` closes that gap. It runs a small socat pod inside the
cluster's own Kubernetes cluster, pointed at one control-plane node's Talos
API, and port-forwards it to localhost. apid on that endpoint proxies to every
other node, so one control plane reaches all of them — and TLS verification is
unaffected, because the certificate is checked against the cluster's CA rather
than against the address you dialled.

These clusters are not `KubernetesCluster` resources, so nothing in the
management cluster knows their Talos credentials. That one fact is what has to
be configured, in `talos.yaml` beside vitictl's own config — `viti talos
config path` prints where:

```yaml
clusters:
  - context: admin@pos1-kv-cl01
    talosconfig: ~/.talos/kvosltalos
  - context: admin@bgo-virt-001
    talosconfig: ~/.talos/kvbgotalos
  - context: admin@ptr1-kv-cl01
    talosconfig: ~/.talos/ktrdtalos
```

`name` defaults to the context, `namespace` to `viti-center`, and `kubeconfig`
to `$KUBECONFIG` / `~/.kube/config`. Nodes are **not** configured: they are
read from the Kubernetes API on every run, so a rebuilt cluster needs no edit
here.

Run one command through the tunnel and tear it down:

```console
$ viti talos tunnel pos1-kv-cl01 -- get members
▶ pos1-kv-cl01 — tunnel via viti-center/talos-tunnel-3f9a2c1d → 100.64.0.4:50000
▶ ready on 127.0.0.1:54417
NODE          NAMESPACE   TYPE     ID                    VERSION   HOSTNAME
100.64.0.4    cluster     Member   pos1-kv-cl01-ctp01    1         pos1-kv-cl01-ctp01
...
```

Or hold it open and drive `talosctl` yourself:

```console
$ viti talos tunnel pos1-kv-cl01
▶ pos1-kv-cl01 — tunnel via viti-center/talos-tunnel-3f9a2c1d → 100.64.0.4:50000
▶ ready on 127.0.0.1:54417

  export TALOSCONFIG=/var/folders/…/viti-talos-8kd/talosconfig
  talosctl -n 100.64.0.4,100.64.0.5,100.64.0.6 <command>

(Ctrl-C to tear down)
```

No `-e` in that line: the temporary talosconfig already names the local
endpoint. The `-n` list is whatever `--node` and `--role` selected, so
`--role controlplane` narrows what is printed as well as what is run.

The local port is a free one by default, because 50000 is so often already
bound — by a `talosctl` of your own, or by a second tunnel. `--local-port
50000` pins it when you want the familiar number.

### Cleaning up

The pod is deleted when the command exits, including on Ctrl-C and including
when a step fails partway through. It also carries an `activeDeadlineSeconds`
(8 hours, `--deadline`) so that even a `kill -9` — the one exit no handler can
catch — cannot leave one running indefinitely.

If you do find an orphan, every tunnel pod is labelled:

```sh
kubectl -n viti-center delete pod -l app.kubernetes.io/managed-by=viti-talos
```

`viti talos tunnel` prints that line itself when it finds a tunnel pod already
in the namespace. It does not delete them for you — a colleague may be holding
one open.
````

- [ ] **Step 6: Full verification**

Run: `make test && make lint && make gosec && go build ./...`
Expected: all pass, 0 lint issues, 0 gosec issues.

- [ ] **Step 7: Commit** *(only if the user asks)*

```bash
git add cmd/config.go cmd/config_test.go README.md
git commit -m "docs: document the Talos API tunnel and report it in config"
```

---

## Manual verification

Automated tests never touch a cluster, so the tunnel itself is proven by hand.
Run these against `admin@pos1-kv-cl01`, which has a talosconfig today.

- [ ] Write `~/.vitistack/talos.yaml` with the three KubeVirt entries from the
      README section above.
- [ ] `viti talos config path` — names `talos.yaml`, without `(not present)`.
- [ ] `viti talos config test` — reports all three tunnel clusters ✅.
- [ ] `viti talos tunnel pos1-kv-cl01 -- get members` — lists the members, then
      exits. Confirm the pod is gone:
      `kubectl --context admin@pos1-kv-cl01 -n viti-center get pods -l app.kubernetes.io/managed-by=viti-talos`
      returns nothing.
- [ ] `viti talos tunnel pos1-kv-cl01` — holds open, prints the paste line. In
      another terminal, run that exact `talosctl` line and confirm it works.
      Ctrl-C, then confirm the pod is gone.
- [ ] `viti talos tunnel pos1-kv-cl01` in two terminals at once — the second
      warns about the first's pod and still opens on a different local port.
- [ ] `viti talos tunnel pos1-kv-cl01 --local-port 50000` — binds 50000.
- [ ] `viti talos tunnel pos1-kv-cl01 --via-node nope` — fails naming the nodes
      it did find, and creates no pod.
- [ ] Point an entry at the wrong cluster's talosconfig (e.g. `kvbgotalos` for
      `pos1-kv-cl01`) and run `-- get members` — confirm the x509 failure is
      readable, and that the pod is still cleaned up.
- [ ] `kill -9` the process while a tunnel is held open, and confirm the
      orphaned pod is found by the labelled `kubectl delete` line.
