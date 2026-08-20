# vitictl-talos

🐢 Talos Linux commands for the [viti](https://github.com/vitistack/vitictl) CLI.

`viti-talos` is a viti **plugin**: viti discovers any `viti-*` binary on `PATH`
and exposes it as a subcommand, so this binary is reachable as
`viti talos ...`, and as `viti t ...` through a `viti-t` link beside it. It
also runs standalone as `viti-talos ...`.

The `t` alias comes from the `aliases:` field in viti's plugin index, so
`viti plugin install talos` creates it; `make install` does the same for a
source build. An older viti that predates index aliases installs only
`viti-talos` — add the link by hand if you want the shortcut:

```sh
ln -sf viti-talos "$(dirname "$(command -v viti-talos)")/viti-t"
```

A single letter is the expensive kind of alias, and the likeliest name a future
viti built-in claims. It degrades safely if that happens: built-ins always win
dispatch, `viti plugin install` refuses a colliding alias and `viti plugin list`
re-checks existing ones, so a clash surfaces as a message rather than as a
command that quietly does the wrong thing. `viti talos` keeps working either way.

It finds the Talos `KubernetesCluster`s across viti's availability zones, works
out each one's control-plane endpoints and node addresses, mints a throwaway
`talosconfig` from that cluster's own credentials `Secret`, and runs `talosctl`
against it — for one cluster, or for a hundred at once.

## Install

```sh
viti plugin install talos
```

Or from source:

```sh
make install    # installs viti-talos + the viti-t symlink onto GOBIN
```

Requires [`talosctl`](https://www.talos.dev/latest/talos-guides/install/talosctl/)
on `PATH`. Check the whole setup with:

```sh
viti talos config test
```

```
✅ talosctl (/opt/homebrew/bin/talosctl)
✅ vitistack availability zones (5/5 reachable)
✅ talos clusters (173 found)
```

## No login, no config file

There is nothing to configure. The plugin reads viti's own availability zones
(`viti config add`) and takes everything else from the management cluster:

```
KubernetesCluster  spec.data.clusterId
  ├─► Secret <clusterId>            (or labelled vitistack.io/clusterid)
  │     └─► talosconfig ──────────► CA + admin cert/key
  ├─► ControlPlaneVirtualSharedIP   status.poolMembers ──► --endpoints
  └─► Machine <clusterId>-ctp*/-wrk*  node IPs ──────────► --nodes
```

Two consequences worth knowing:

- **No `viti kc login` first.** Credentials are read per command.
- **Your `~/.talos/config` is never touched.** The generated config is
  owner-only, lives in a temp directory, and is deleted when the command exits.
  A command that spans ten clusters must not leave your `talosctl` pointing at
  whichever one ran last.

When viti dispatches the plugin it forwards `VITI_CONFIG` and
`VITI_AVAILABILITYZONE`, so `viti -z prod talos ...` stays scoped to `prod`.

### Which address gets used

This is the thing that goes wrong, so it is worth stating plainly. Talos issues
its certificates for the node's own addresses, so `talosctl` has to be pointed
at one of those. `viti talos nodes` shows exactly what was resolved, and
`--probe` also dials each one on the Talos API port:

```sh
$ viti talos nodes cephtest-036 --probe
🐢 test-south-az1/cephtest5/cephtest-036 — endpoints 100.64.2.23 (from controlplanevirtualsharedip)
NAME                     ADDRESS       ROLE           PHASE     TALOS    TALOS API
cephtest-036-ab36-ctp0   100.64.2.23   controlplane   Running   1.13.6   ✅ open
cephtest-036-ab36-wrk0   100.64.2.35   worker         Running   1.13.6   ✅ open
cephtest-036-ab36-wrk1   100.64.2.34   worker         Running   1.13.6   ✅ open
```

`❌ no answer` on every node means a network path problem — a VPN or tunnel
that is down — not a broken cluster. Without the probe, that surfaces from
`talosctl` as a gRPC dial timeout, which reads like the cluster's fault.

Machine addresses are filtered before use: CNI bridges, `cilium_*`, per-pod
veths and the interfaces a KubeVirt guest agent reports but KubeVirt did not
attach are all discarded. Picking one of those is what produces
`x509: certificate is valid for X, not Y`. IPv6 is off by default (`--ipv6` to
include it) because one unreachable v6 entry hangs `talosctl` until its dial
timeout.

## Commands

| Command | What it does |
| ------- | ------------ |
| `clusters` | List the Talos clusters viti knows about |
| `nodes` | List a cluster's nodes, their addresses, roles and Talos versions |
| `dmesg` / `netstat` / `memory` | Read from a cluster's nodes |
| `dashboard` | Open talosctl's full-screen dashboard |
| `show` | Print the machine config a node is actually running |
| `edit` | Edit one node's machine config in your editor |
| **`patch`** | **Apply a machine-config patch across many clusters at once** |
| `upgrade-node` | Upgrade Talos itself, one node at a time |
| `upgrade-k8s` | Upgrade Kubernetes on one cluster |
| `config` / `version` / `upgrade` | The plugin's own housekeeping |

Leave the cluster name out of any single-cluster command and a fuzzy-searchable
picker opens. Every column is matched, so `prod`, a region, or a clusterId
narrows the list as readily as a name does.

Anything after a `--` is handed to `talosctl` unchanged:

```sh
viti talos dmesg my-cluster -- --follow --tail
viti talos netstat my-cluster -- --listening --programs
```

## Changing machine configs across the fleet

This is what the plugin is for. Editing nodes by hand does not scale past a
handful and cannot be reviewed; a patch is one file, applied identically
everywhere, and re-running it on a node that already matches is a no-op.

**1. Write the patch.** A strategic-merge fragment:

```yaml
# patch.yaml
machine:
  kubelet:
    extraArgs:
      rotate-server-certificates: "true"
```

or an RFC 6902 JSON patch list — whatever `talosctl` accepts.

**2. Preview it everywhere. Nothing is written.**

```sh
viti talos patch --all -p @patch.yaml --dry-run
```

Each node prints the diff it would get, per cluster:

```
🩹 Previewing (dry run) 83 node(s) across 26 cluster(s)
   mode:      staged (written to disk, takes effect on next reboot; nothing restarts now)
   ordering:  all nodes together within a cluster, 4 cluster(s) at a time
   patch:     @patch.yaml
   • test-south-az1/cephtest5/cephtest-036    3 node(s): …-ctp0, …-wrk0, …-wrk1
   …

✅ test-south-az1/cephtest5/cephtest-036
   Config diff:
   --- a
   +++ b
   @@ -30,6 +30,7 @@
        nodeLabels:
            node.kubernetes.io/exclude-from-external-load-balancers: ""
   +        viti.io/managed-by: vitictl-talos
```

**3. Apply it.**

```sh
viti talos patch --all -p @patch.yaml --mode staged --yes
```

**4. Confirm it landed.**

```sh
viti talos show my-cluster --role controlplane | grep -A3 kubelet
```

Choose the clusters with `--cluster` (repeatable), with `--all`, or by leaving
both out to mark them interactively — filter, `Ctrl-A` to mark everything
shown, `Enter` to confirm. There is deliberately **no** "neither flag means the
whole fleet" shortcut: a forgotten argument must not become a fleet-wide write.

The diffs go to stderr and the summary to stdout, so the run stays pipeable:

```sh
$ viti talos -z test-south-az1 patch --all -p @patch.yaml --dry-run 2>/dev/null
AZ               NAMESPACE   CLUSTER        NODES   RESULT      DETAIL
test-south-az1   cephtest5   cephtest-036   3       ✅ patched   -
test-south-az1   cephtest5   cephtest-038   3       ✅ patched   -
…
```

`-o json` gives the same summary with every node's captured output, for a CI
job that needs to record what it did.

### Removing a config document

A Talos machine config is multi-document. Deleting a whole document — a
`DHCPv4Config`, a `LinkConfig` — is a strategic-merge patch with
`$patch: delete`:

```yaml
# drop-dhcp.yaml
apiVersion: v1alpha1
kind: DHCPv4Config
name: net0
$patch: delete
```

That patch is **not idempotent**: talosctl (v1.13.9, at least) errors out when
the document is already gone, so a second pass over a half-finished fleet
fails for exactly the clusters that succeeded the first time. `--if-match`
guards against that by reading each node's current config and patching only
the nodes that match:

```sh
viti talos patch --all -p @drop-dhcp.yaml --if-match 'kind: DHCPv4Config' --mode no-reboot
```

Nodes already done are reported as `➖ nothing to do` rather than failing, so
the command is safe to re-run and a run interrupted halfway picks up where it
left off. With `--dry-run` the same command doubles as a fleet-wide survey of
which clusters still carry the thing you are removing.

Note that documents delivered by the talos-operator's tenant `ConfigMap`
(`talos-tenant-config` in `vitistack`) are re-applied to **new** nodes. The
operator does not re-apply config to already-configured running ones, so a
node patch sticks — but a scale-up or a rebuild brings the document back
unless the ConfigMap is changed too.

### Starting small, then widening

`--if-match` is what makes a careful rollout cheap: widening the scope never
re-touches a node that is already done, so every rung is the *same command*,
just less narrow.

```sh
P="-p @drop-dhcp.yaml --if-match 'kind: DHCPv4Config' --mode no-reboot"

viti talos patch -c my-cluster -N my-cluster-wrk0 $P --dry-run   # one machine
viti talos patch -c my-cluster -N my-cluster-wrk0 $P --yes       # …for real

viti talos patch -c my-cluster --role worker       $P --yes      # rest of the workers
viti talos patch -c my-cluster --role controlplane $P --yes      # then the control planes
viti talos patch -c my-cluster                     $P --yes      # whole cluster; done nodes skipped

viti talos -z my-zone patch --all $P --dry-run                   # one availability zone
viti talos patch --all            $P --dry-run                   # the fleet
```

Check a rung landed before climbing:

```sh
viti talos show my-cluster -N my-cluster-wrk0 | grep -c DHCPv4Config   # 1 before, 0 after
```

The narrowing flags:

| Flag | Scope |
| ---- | ----- |
| `-N`, `--node` | One machine, by name or address. Repeatable. |
| `--role` | `worker` or `controlplane` — workers first is the natural first rung inside a cluster. |
| `-c`, `--cluster` | One cluster, by name or clusterId. Repeatable, so two or three is a valid middle step. |
| `-z`, `--availabilityzone` | Scopes `--all` to one zone. |

The guard filters **per node, not per cluster**. Targeting a whole 3-node
cluster when only one node still matches:

```
🔎 --if-match ruled out 2 of 3 node(s)
   • test-south-az1/cephtest5/cephtest-036    1 node(s): cephtest-036-ab36-wrk0
```

That is the same mechanism that skips already-patched nodes when you widen, so
a partially-completed cluster resumes rather than failing on the nodes you
already did.

### Picking a mode

`--mode` is the safety-critical choice.

| Mode | Effect |
| ---- | ------ |
| `staged` | Written to disk, takes effect on next reboot. **Nothing restarts.** |
| `no-reboot` | Applied live; fails rather than rebooting. |
| `auto` (default) | Applied now, rebooting only the nodes whose change requires it. |
| `reboot` | Applied, and every targeted node reboots. |
| `try` | Applied with a timeout, rolled back automatically. |

For a fleet-wide change, `--mode staged` is usually the right answer: stage
everything in one pass, then sequence the reboots separately and deliberately.

### What it does to protect you

- **Everything is resolved before anything is written.** All clusters are
  opened, all nodes selected, and every endpoint dialled up front. A run that
  would die on the eighth cluster refuses to start instead.
- **Unreachable clusters stop the run.** "Patch everything except the four I
  could not reach" is a fleet that has silently drifted apart. `--no-probe`
  overrides this.
- **`@file` patches are checked up front** — otherwise a mistyped path produces
  the same error once per cluster, after the run has started.
- **`--if-match` makes a non-idempotent patch re-runnable**, and a node that
  cannot be read fails the run rather than being quietly excluded from a
  fleet-wide change.
- **Nodes are patched one at a time whenever the mode can reboot**, including
  `auto`, which decides per change. Fanning a rebooting change across every
  control plane at once costs the cluster its etcd quorum. `--batch` overrides
  it; `--serial` forces it on for the modes that would otherwise batch.
- **A serial run stops at the first failing node.** Continuing past a node that
  would not take the config means patching the next one while the cluster is
  already in an unknown state.
- **Clusters run concurrently** (`--parallel`, default 4) because they are
  independent of each other. 26 clusters / 83 nodes preview in about 8 seconds.
- **Confirmation is required** unless `--dry-run` or `--yes`, and a
  non-terminal stdin is refused rather than assumed either way.

## Upgrades

```sh
viti talos upgrade-node my-cluster --to v1.13.9      # Talos itself
viti talos upgrade-k8s  my-cluster --to 1.34.4 --dry-run   # Kubernetes
viti talos upgrade                                    # this plugin
```

Nodes are upgraded **one at a time**, fully back before the next starts. That
is not tunable: a Talos upgrade reboots the node, and two control planes at
once costs the cluster its quorum.

### Why `--to` and not `--image`

A Talos installer image carries more than a version. It encodes the Image
Factory **schematic** — the cluster's system extensions — and a **platform
variant** baked into its name (`nocloud-installer`, `metal-installer`, …).

- Upgrading onto a generic `ghcr.io/siderolabs/installer` drops the extensions.
  Talos rejects that with a confusing `file exists` validation error that
  strands the upgrade.
- Upgrading onto the *wrong platform variant* silently changes the node's
  `talos.platform`. The node stays `Ready` while its config source quietly
  breaks — which surfaces days later, at the next reboot.

So `--to` reads each node's current `machine.install.image` live and swaps only
the version tag into it. Every resolved image is printed before anything runs:

```
⬆️  Upgrading 3 node(s) of test-south-az1/cephtest5/cephtest-036, one at a time
   effect: each node reboots into the new version as it is upgraded
   • cephtest-036-ab36-ctp0     controlplane   declared v1.13.6 → factory.talos.dev/nocloud-installer/b0f2…:v1.13.9
```

`--image` overrides that outright. Matching the schematic and the platform is
then yours to get right.

Note that the talos-operator also enforces a cluster's Talos version. Running
`upgrade-node` by hand is for the cases it cannot resolve.

`upgrade-k8s` is deliberately one cluster at a time: `talosctl` drives the
whole upgrade from one control-plane node — API server, controller manager,
scheduler, proxy and every kubelet — rewriting each node's machine config as it
goes. Run it with `--dry-run` first; `talosctl` then prints the full plan and
changes nothing. Afterwards, remember that the `KubernetesCluster`'s own
`spec.topology.version` is what the operators reconcile from; the command warns
when the two disagree.

## Why shell out to talosctl

Everything this plugin does to a node — reading dmesg, opening a dashboard,
patching a config, upgrading Talos — is something `talosctl` already does
correctly, including the parts that are easy to get subtly wrong: the gRPC
streaming protocol, the config-apply modes, upgrade progress tracking, and the
etcd-aware sequencing behind a node reboot.

The plugin's job is working out **what to point it at** across a fleet. So
every invocation is a thin, explicit argv — no shell, no string interpolation,
and never anything inherited from the ambient environment. In particular
`--talosconfig` and `--endpoints` are always passed explicitly, because letting
`talosctl` fall back to `$TALOSCONFIG` is exactly the "act on whatever cluster
the shell was pointing at" accident this plugin exists to prevent.

## Development

```sh
make build        # build into bin/
make test         # go test ./...
make lint         # golangci-lint
make gosec        # security analysis
make govulncheck  # known vulnerabilities in dependencies
make install      # onto GOBIN, with the viti-t symlink
```
