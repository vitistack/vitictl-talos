# `viti talos tunnel` — deferred findings

Every finding raised during review that was **not** fixed before merge, with the
triage that let it ship. Nothing here blocks the feature; it is the honest list
of what was seen and consciously left.

Delete this file once the items are ticketed elsewhere.

## Live-run verification still owed

The feature cannot be exercised against a cluster from a workstation that
cannot reach any Talos API — which is the whole reason it exists. The plan's
manual-verification checklist covers the happy paths; these are the specific
claims only a live run can settle.

- **`alpine/socat` is pullable in the three management clusters.** Proven on
  all three KubeVirt clusters by the manual workaround; never tried on the
  management clusters. `--image` is the escape hatch and a failed pull surfaces
  as `ImagePullBackOff` in the readiness error.
- **The restricted-PSA field set satisfies the admission controllers actually
  deployed.** The pod constructor is asserted field by field, but only against
  the spec's list — not against a real admission decision.
- **The operator's context has cluster-wide `list nodes`.** The spec confirmed
  `create pods`, `delete pods` and `create pods/portforward` in `viti-center`
  on all six clusters, but the design's central claim — that topology needs no
  configuration — rests on a cluster-scoped node read that was never in the
  confirmed set. A 403 is diagnosable (the error names the context), but this
  belongs in the checklist.
- **`apid` on the chosen control plane proxies `-n` to workers on their
  InternalIPs.** Assumed throughout; demonstrated for control planes only.
- **A dying tunnel is reported.** `Tunnel.Done()` by construction only fires
  when a live pod dies under a live forwarder.

## Known behaviour, accepted

- **A pod `Create` that times out after the API server persisted the object
  leaks a pod.** Bounded by `activeDeadlineSeconds` (8h, `--deadline`) and
  surfaced by the orphan warning on the next run.
- **A successful passthrough that completes in the same instant as Ctrl-C
  exits 1 silently instead of 0.** The translation triggers on `ctx.Err()`
  alone. A genuine non-cancellation failure racing the signal is likewise
  flattened. Both windows are microseconds wide and only occur when the user
  did press Ctrl-C.
- **The passthrough branch's `tun.Err()` check is best-effort.** If the
  forwarder goroutine has not recorded the loss by the time talosctl's error
  propagates, the operator gets the x509 credential hint rather than "the
  tunnel stopped". Strictly better than before; not fixable without a wait that
  branch should not take.
- **A present-but-empty `clusters: []` is now a fatal `config test` failure**
  rather than "none configured". Deliberate.
- **`--endpoint`, `-z`, `-n` and `--all-providers` are accepted on `tunnel` and
  silently do nothing.** They drive the management-cluster listing path this
  command deliberately bypasses. `viti talos tunnel kv -n viti-center` looks
  like it sets the pod namespace and does not — `--tunnel-namespace` does.
  Worth a line in the long help, or an explicit error.

## Diagnostics that could be better

- **`NotReadyReason` reports only `Waiting` container states.** A socat
  container that starts and immediately exits — bad args, wrong entrypoint in a
  mirrored image — yields a bare `phase Failed` with no exit code. This is the
  second-most-likely first failure after an image pull.
- **`warnAboutOrphans` swallows its list error**, so an RBAC denial on
  `pods list` produces no output and the operator never learns the orphan check
  did not run.
- **The talosconfig is parsed only after a pod exists and is forwarded.** The
  bare-context-body failure the spec calls out — the one that has already cost
  an afternoon — surfaces about a minute in, then tears everything down again.
  A parse before `tunnel.Open` would fail in a second.

## Code hygiene

- **`Close()` writes `t.stop` and `t.clientset` outside the mutex**, relying on
  `closeOnce` for exclusion. Correct as written, but the file now has a mutex
  and these two fields are not under it — a future reader may assume they are.
- **`cmd/config.go` discards `TalosConfigPath()`'s error.** Provably safe, but
  only because of a non-local invariant about which error `TunnelClusters`
  returns first.
- **`Tunnel`'s doc comment overstates its callers** — it describes a signal
  handler calling `Close` on its own goroutine. `cmd/tunnel.go` uses
  `signal.NotifyContext`, so every `Close` is a defer on one goroutine. The
  `sync.Once` is still right, and becomes load-bearing if a second caller ever
  appears; the justification should describe the code that exists.
- **`&cluster.Topology{Nodes: tun.Nodes}` in `cmd/tunnel.go`** leaks a
  zero-value contract into the command. A `func (t *Tunnel) Select(...)` would
  keep the invariant in the package that owns it.
- **`internal/tunnel/pod.go`**: the container `Ports:` entry is informational
  rather than required; `PodName()`'s 32-bit entropy budget is undocumented;
  `LabelValue`'s truncate-then-trim is only exercised by an all-alphanumeric
  case.
- **`cmd/tunnel_test.go`** asserts flag registration but not `DefValue`, so a
  wrong default would pass; the `-n` anti-collision assertion checks the long
  name but not the shorthand.
- **`internal/tunnel/topology.go`**: `describeNodes` and `SelectVia`'s
  unknown-override error both build a `(have: %s)` join.
- **`TestTalosVersionFromOSImage`** uses a map-based table, so iteration order
  is nondeterministic. Harmless — each case asserts independently.
- **README example fences.** The new section uses one ```console block
  combining prompt and output; the rest of the README pairs a ```sh block with
  a separate output block.

## Future-proofing

- **The port-forward is plain SPDY, with no websocket fallback.** Kubernetes is
  migrating `pods/portforward` off SPDY, and `kubectl` has dialled websockets
  first with a fallback since 1.31. client-go 0.37 already ships
  `portforward.NewSPDYOverWebsocketDialer` and `NewFallbackDialer`, and the
  transitive dependencies are already in `go.mod` because of them — so adopting
  the fallback costs no new dependency. SPDY works against every current API
  server, so this is future-proofing rather than a fix.
