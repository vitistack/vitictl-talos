# `viti talos tunnel` — reaching the Talos API through a pod

Status: approved 2026-09-10. Prototype; expected to need correction in use.

## Problem

From this workstation no Talos node answers on tcp/50000 — not the tenant
clusters, not the three KubeVirt hypervisor clusters, not the three management
clusters. Verified 2026-09-09 and again 2026-09-10:

    10.204.146.227  (pos1-mgmt-001-ctp01)  unreachable
    10.122.6.174    (bgo-mgmt-ctpl01)      unreachable
    10.204.145.11   (ptr1-mgmt-ctpl01)     unreachable
    100.64.0.4      (pos1-kv-cl01-ctp01)   unreachable

The Kubernetes API of every one of them *is* reachable. So the plugin can list
174 clusters and operate none of them from here.

The manual workaround, used successfully on all three hypervisor clusters: a
socat pod inside the target Kubernetes cluster forwards to one control-plane
node's Talos API, `kubectl port-forward` brings it to localhost, and
`talosctl -e 127.0.0.1 -n <node-ip>` works with normal TLS verification —
apid on the endpoint proxies to any `-n` node.

This spec makes that one command.

## Scope

In scope: the three management clusters (`admin@{pos1,bgo,ptr1}-mgmt-001`) and
the three KubeVirt hypervisor clusters (`admin@pos1-kv-cl01`,
`admin@bgo-virt-001`, `admin@ptr1-kv-cl01`).

Out of scope, deliberately:

- Tenant/guest clusters. They are operator-managed and already work through
  `KubernetesCluster` resolution.
- Any change to the existing commands. No `--via-pod` flag, no auto-fallback.
  `tunnel` is additive; with it absent, nothing behaves differently.
- Making the six clusters appear in `clusters`, `nodes`, or the picker.

## What the investigation established

Three findings shaped the design; two of them changed the question the task
was posed with.

**Topology needs no configuration.** Every node of all six clusters is fully
described by the Kubernetes API already in reach: name, InternalIP,
control-plane role, readiness, and the Talos version in
`status.nodeInfo.osImage` (`"Talos (v1.13.6)"`). The control-plane addresses
discovered this way match the hand-collected list exactly, including
ptr1-kv-cl01's three default-hostname control planes (`talos-np6-59s`
100.64.0.53, `talos-q4w-1q4` 100.64.0.4, `talos-stc-49s` 100.64.0.33). No
address list is maintained anywhere in this design.

**The management clusters are already availability zones.**
`~/.vitistack/ctl.config.yaml` lists all three, and the plugin connects to
them on every command. What is missing is not access — it is a Talos identity
for the zone itself.

**Credentials are the only real gap.** `~/.talos/config` holds 143 contexts;
none of the six is among them. No talosconfig Secret exists in-cluster either.
The three hand-fetched files cover the KubeVirt clusters only — **there is no
talosconfig for any management cluster on this machine.** The design therefore
treats mgmt-cluster support as a data problem: they get a config entry the day
their talosconfig exists, with no code change.

Preconditions confirmed on all six: namespace `viti-center` exists, and the
current user has `create pods`, `delete pods` and `create pods/portforward`
in it.

## The config file, and the position it reverses

`cmd/config.go:18` and `internal/config/config.go:4` currently state that this
plugin has no configuration file of its own, deliberately, because "a second
config file would only give the two CLIs something to drift on."

That reasoning was about **duplicating** what vitictl already knows —
availability zones and their kubeconfigs. This file holds only what vitictl
has **no concept of**: Talos credentials for clusters that are not
`KubernetesCluster` resources. Zones still come from `ctl.config.yaml`;
nothing is mirrored, so there is nothing to drift. Both doc comments must be
rewritten to say that, rather than left contradicting the code.

`~/.vitistack/talos.yaml` — resolved as `talos.yaml` in the same directory as
the resolved `ctl.config.yaml`, so `$VITI_CONFIG` pointing elsewhere keeps the
two together. `$VITI_TALOS_CONFIG` overrides the whole path.

```yaml
clusters:
  - name: pos1-kv-cl01                  # optional; defaults to context
    context: admin@pos1-kv-cl01         # required — the cluster's identity
    kubeconfig: ~/.kube/config          # optional; $KUBECONFIG, ~/.kube/config
    talosconfig: ~/.talos/kvosltalos    # required
    namespace: viti-center              # optional; default viti-center
```

Rules: `context` and `talosconfig` are required; `name` defaults to `context`;
`namespace` defaults to `viti-center`; `~` and `$HOME` are expanded in both
path fields; duplicate names are an error naming both entries. A missing file
is not an error until a tunnel is actually asked for, and then the message
names the path and shows the shape above.

## Components

### `internal/config/tunnels.go`

```go
type TunnelCluster struct {
    Name        string `yaml:"name,omitempty"`
    Context     string `yaml:"context"`
    Kubeconfig  string `yaml:"kubeconfig,omitempty"`
    Talosconfig string `yaml:"talosconfig"`
    Namespace   string `yaml:"namespace,omitempty"`
}

func TalosConfigPath() (string, error)
func TunnelClusters() ([]TunnelCluster, error)   // parsed, defaulted, validated
```

Pure and file-driven; tested against fixtures.

### `internal/tunnel/topology.go`

Reuses `cluster.Node` rather than introducing a parallel type — it already
carries Name, IP, Role, Phase and TalosVersion, and `cluster.Topology.Select`
already implements node selection by name or address with the error messages
this command wants. Nodes are wrapped as `&cluster.Topology{Nodes: …}` purely
to reach `Select`; the zero `Cluster` field is never read.

```go
func NodesFrom(list *corev1.NodeList, includeIPv6 bool) []cluster.Node
func SelectVia(nodes []cluster.Node, override string) (cluster.Node, error)
```

`NodesFrom` maps, per node: first usable `InternalIP` in the requested family;
`Role` from the `node-role.kubernetes.io/control-plane` label; `Phase` from
the `Ready` condition (`"Ready"` / `"NotReady"`); `TalosVersion` parsed from
`osImage` matching `^Talos \((.+)\)$`, empty when it does not match. Control
planes sort first, matching `cluster.Resolve`.

`SelectVia` returns the node the socat pod points at: the override when given
(matched on name or IP, erroring when it has no usable address), otherwise the
first Ready control plane. With no Ready control plane it errors naming what
it did find, because "connection refused" three layers down is not a
diagnosis.

Both are pure functions over an in-memory list — the table-tested part.

`internal/cluster` gains one small export so the IP-family logic is not
duplicated:

```go
func PickNodeIP(addrs []string, includeIPv6 bool) string  // wraps filterIPFamily + isUsableNodeIP
```

### `internal/tunnel/pod.go`

```go
func Pod(name, namespace, image, targetIP, forCluster string, deadline time.Duration) *corev1.Pod
```

Pure constructor, asserted field by field in tests. Satisfies the restricted
PodSecurity profile, which is what the namespaces enforce:

- pod `securityContext`: `runAsNonRoot: true`, `runAsUser: 65534`,
  `runAsGroup: 65534`, `seccompProfile: RuntimeDefault`
- container `securityContext`: `allowPrivilegeEscalation: false`,
  `capabilities.drop: [ALL]`
- `restartPolicy: Never`
- args `["TCP-LISTEN:50000,fork,reuseaddr", "TCP:<targetIP>:50000"]`
- `activeDeadlineSeconds` from `deadline` — the backstop for an orphan
- resource requests/limits (10m/16Mi, 200m/64Mi), so a namespace with a
  LimitRange or quota does not reject the pod

Labels: `app.kubernetes.io/name=talos-tunnel`,
`app.kubernetes.io/managed-by=viti-talos`, `vitistack.io/tunnel-for=<name>`.
Cluster names are contexts like `admin@pos1-kv-cl01`, and `@` is not legal in
a label value — the label carries a sanitised form and an annotation of the
same key carries the name verbatim. Pod name is `talos-tunnel-<8 hex>` —
unique per run, so two engineers, or two of your own windows, never collide.

### `internal/tunnel/tunnel.go`

```go
type Options struct {
    Cluster     config.TunnelCluster
    Image       string          // default alpine/socat
    LocalPort   int             // 0 = ephemeral
    Via         string          // --via-node
    Timeout     time.Duration   // pod readiness, default 60s
    Deadline    time.Duration   // activeDeadlineSeconds, default 8h
    IncludeIPv6 bool
    Out, Err    io.Writer
}

type Tunnel struct {
    LocalAddr string        // "127.0.0.1:54417"
    Via       cluster.Node
    Nodes     []cluster.Node
    PodName   string
    Namespace string
}

func Open(ctx context.Context, o Options) (*Tunnel, error)
func (t *Tunnel) Close()   // idempotent
```

`Open`, unwinding cleanly at every step that can fail:

1. `kube.RESTConfig(o.Cluster.Kubeconfig, o.Cluster.Context)`
2. `kubernetes.NewForConfig` — a typed clientset, new to this repo; the
   existing controller-runtime client cannot produce the `pods/portforward`
   subresource URL
3. list nodes → `NodesFrom` → `SelectVia`
4. warn about pre-existing pods in this namespace carrying the managed-by
   label — **warn, not delete**: a colleague may be holding one open
5. create the pod
6. watch until Ready, bounded by `Timeout`; on timeout report the pod's own
   phase and any container waiting-reason, since `ImagePullBackOff` on
   `alpine/socat` is the likely first failure in an air-gapped zone
7. `spdy.RoundTripperFor(rest)` → `spdy.NewDialer(upgrader, client, "POST",
   clientset.CoreV1().RESTClient().Post().Resource("pods").
   Namespace(ns).Name(pod).SubResource("portforward").URL())`
8. `portforward.New(dialer, []string{fmt.Sprintf("%d:50000", o.LocalPort)},
   stop, ready, out, err)`; `"0:50000"` selects a free local port
9. `go pf.ForwardPorts()`; wait on `ready` or the goroutine's error
10. `pf.GetPorts()` for the port actually bound

`Close` closes the stop channel, then deletes the pod with
`GracePeriodSeconds: 0` on **`context.WithoutCancel(ctx)`** — the cancelled
context that triggered teardown must not also kill the delete. Idempotent, in
the shape of `Session.Close`.

### `internal/cluster/credentials.go`

`WriteTempTalosconfig` currently takes a `*corev1.Secret`. Extract the
byte-level half so the file-backed path reuses it instead of duplicating the
context-renaming and endpoint-override logic:

```go
func WriteTempTalosconfigBytes(raw []byte, contextName string, endpoints []string) (string, func(), error)
```

`WriteTempTalosconfig` becomes a two-line wrapper. Behaviour is unchanged.

`writeTalosconfigFile`'s "talosconfig has no usable context" also gets the
explanation the failure needs, because this is a gotcha already hit in
practice: a talosconfig exported as a bare context body — `endpoints:`, `ca:`,
`crt:`, `key:` at the top level with no `context:`/`contexts:` keys — parses
into an empty struct and fails here. The caller wraps it with the file path.

### `cmd/tunnel.go`

```
viti talos tunnel [cluster] [-- talosctl args]      # alias: portfwd
```

Named `tunnel` because it creates a pod, not merely a port-forward; `portfwd`
is aliased for the kubectl muscle memory.

No cluster named → picker over the configured clusters, matching every other
single-cluster command here. Columns: NAME, CONTEXT, NAMESPACE, TALOSCONFIG.
Non-interactive with no name → the usual "pass one, or run in a terminal"
error.

Args are split on `--` with the existing `splitArgs`.

**With passthrough** — run and tear down:

```
viti talos tunnel pos1-kv-cl01 -- get members
```

`talosctl.Run` with `Target{Talosconfig: <temp>, Endpoints: [127.0.0.1:<port>],
Nodes: <selected>}`. Endpoints are passed explicitly even though the temp
config also carries them, per this repo's rule that a target is never left to
ambient state. A failed run exits non-zero, as every other command here does
(`main.go:24` exits 1 on any error — talosctl's own exit code is not
propagated by this plugin today, and this command does not change that).

**Without** — hold open until Ctrl-C:

```
▶ pos1-kv-cl01 — tunnel via viti-center/talos-tunnel-3f9a2c1d → 100.64.0.4:50000
▶ ready on 127.0.0.1:54417

  export TALOSCONFIG=/var/folders/…/viti-talos-8kd/talosconfig
  talosctl -n 100.64.0.4,100.64.0.5,100.64.0.6 get members

(Ctrl-C to tear down)
```

The temp talosconfig carries `127.0.0.1:<port>` as its endpoint, so `-e` drops
out of the printed line — the reader pastes node addresses and nothing else.
The `-n` list is the *selected* nodes, so `--role controlplane` or `-N` narrows
what is printed exactly as it narrows what the passthrough form acts on. All of
this goes to stderr via `echo`, keeping stdout pipeable.

Flags:

| flag | default | why |
|---|---|---|
| `--via-node` | first Ready control plane | pin the socat target |
| `--tunnel-namespace` | from config, else `viti-center` | **not** `-n`; that is the root's "limit to this namespace" |
| `--image` | `alpine/socat` | air-gapped zones need a mirror |
| `--local-port` | `0` (ephemeral) | 50000 is often already bound |
| `--timeout` | `60s` | pod readiness |
| `--deadline` | `8h` | `activeDeadlineSeconds` backstop |
| `-N`, `--node` | all | reuses `nodeSelector` |
| `--role` | all | reuses `nodeSelector` |

Ephemeral local port is the default because 50000 is frequently already bound
— by a talosctl of your own, or a second tunnel — and pinning it turns a
second window into a confusing failure. The bound port is always printed.

## Cleanup

The requirement the manual workaround kept getting wrong, so it is layered:

1. `defer t.Close()` on the normal path.
2. `signal.NotifyContext(ctx, SIGINT, SIGTERM)` in the command, so Ctrl-C
   unwinds through that same `defer` instead of killing the process.
3. The delete runs on `context.WithoutCancel(ctx)`, so the cancellation that
   caused teardown cannot cancel the teardown.
4. `activeDeadlineSeconds` on the pod, so an orphan from `kill -9` — the one
   path no handler can catch — expires on its own.

Plus the label, so an orphan is findable:

```sh
kubectl -n viti-center delete pod -l app.kubernetes.io/managed-by=viti-talos
```

which is the line the start-up warning prints when it finds one.

## Errors worth naming

| symptom | message |
|---|---|
| `x509: certificate signed by unknown authority` | a hint after any failed passthrough run, naming the configured file and the context it may not belong to. talosctl streams straight to the terminal, so its output cannot be matched on from here — only an exit code comes back |
| talosconfig with no `contexts:` key | names the file and explains a bare context body will not parse |
| no Ready control plane | lists what was found, with roles and readiness |
| pod not Ready before timeout | the pod's phase and container waiting-reason, not just "timed out" |
| no config file | names the path and shows the expected shape |

## Testing

Table-driven and pure, in the style of `topology_test.go` and
`credentials_test.go`. Nothing in the test suite talks to a cluster.

- `NodesFrom`: control-plane detection, IPv4-only default vs `--ipv6`, Ready
  and NotReady, `osImage` parsing including a non-Talos value, a node with no
  InternalIP, control-plane-first ordering
- `SelectVia`: override by name, override by IP, override not found, override
  with no address, no Ready control plane, normal pick
- `Pod`: every restricted-PSA field, socat args, deadline, labels, resources
- `TunnelClusters`: defaulting, `~` expansion, missing required fields,
  duplicate names, missing file
- `WriteTempTalosconfigBytes`: unchanged behaviour for the Secret path, plus
  the bare-context-body failure
- `cmd`: flag registration and the rendered talosctl line

## Documentation

A `## Reaching a cluster the Talos API cannot see` section in README.md, after
`### Which address gets used` (line 79) — it is the same question, answered
for the case where the answer is "none of them". It covers the config file,
the two modes, and cleanup including the orphan-sweep line.

`cmd/config.go`'s long help and `internal/config/config.go`'s package doc are
corrected as described above. `config path` gains the talos.yaml path;
`config test` reports how many tunnel clusters are configured and whether each
one's talosconfig is readable.

## Risks

- **`alpine/socat` must be pullable.** Confirmed working on all three
  hypervisor clusters; untested on the management clusters. `--image` is the
  escape hatch, and the readiness timeout reports `ImagePullBackOff` plainly.
- **No management-cluster talosconfig exists yet.** Those three entries cannot
  be exercised until someone obtains one. The KubeVirt clusters are the
  testable path on day one.
- **A typed clientset is new to this repo.** It is the same client-go already
  depended on, and pulls in `github.com/moby/spdystream` transitively.
- **Prototype.** Accepted as such; correction in use is expected.
