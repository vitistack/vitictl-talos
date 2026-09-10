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
| `tunnel` | Reach a Talos API that is not routable from here, through a pod |
| `config` / `version` / `upgrade` | The plugin's own housekeeping |

Leave the cluster name out of any single-cluster command and a fuzzy-searchable
picker opens. Every column is matched, so `prod`, a region, or a clusterId
narrows the list as readily as a name does.

Anything after a `--` is handed to `talosctl` unchanged:

```sh
viti talos dmesg my-cluster -- --follow --tail
viti talos netstat my-cluster -- --listening --programs
```

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

Define the unchanging half once, as a shell **function** — the arguments
contain spaces and quotes, so a `P="..."` variable does not survive expansion
in either bash or zsh:

```sh
drop() { viti talos patch -p @drop-dhcp.yaml --if-match 'kind: DHCPv4Config' --mode no-reboot "$@"; }
```

Then climb, widening the scope one rung at a time:

```sh
drop -c my-cluster -N my-cluster-wrk0 --dry-run   # one machine
drop -c my-cluster -N my-cluster-wrk0 --yes       # …for real

drop -c my-cluster --role worker       --yes      # rest of the workers
drop -c my-cluster --role controlplane --yes      # then the control planes
drop -c my-cluster                     --yes      # whole cluster; done nodes skipped

drop --all --dry-run                              # the fleet
```

Scoping `--all` to one availability zone takes viti's own `-z`, which belongs
to `viti` rather than to the plugin, so it goes before the subcommand:

```sh
viti talos -z my-zone patch --all -p @drop-dhcp.yaml --if-match 'kind: DHCPv4Config' --mode no-reboot --dry-run
```

Substitute your own cluster for `my-cluster` — a placeholder left in place
fails at cluster resolution, after the patch file has already been read.

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
   image:  factory.talos.dev/nocloud-installer/b0f2…9365
   • cephtest-036-ab36-ctp0     controlplane   v1.13.6 → v1.13.9
   • cephtest-036-ab36-wrk0     worker         v1.13.6 → v1.13.9
   • cephtest-036-ab36-wrk1     worker         v1.13.6 → v1.13.9
   pin:    install_image v1.13.6 → v1.13.9
```

When every node moves within one installer lineage, which is what a `--to` bump
does, the reference is printed once and the rows carry only the part that
changes. Nodes on different lineages, or a change to more than the version,
print the full reference on both sides instead.

### Three versions disagree, and only one is real

A Talos cluster reports its version from three places:

| Source | What it is |
| ------ | ---------- |
| `Machine spec.os.imageID` | **Declared.** The operator bumps it to the newest *available* image ahead of any upgrade — observed 112 days ahead of reality |
| `machine.install.image` on the node | **Installed.** What the config said to install *at provisioning time*. No upgrade writes it back — `d-stackops-1010` sat two upgrades past v1.13.5 with its nodes on v1.13.9, its pin on v1.13.9, and this field still reading v1.13.5 |
| `Node status.nodeInfo.osImage` in the guest cluster | **Running.** `Talos (v1.13.7)`, what the kubelet reports. The only one that is not a wish |

The plan reads the third. It comes from the guest cluster's own Node objects,
reached with the `kube.config` already in the same Secret the talosconfig is
minted from — one API call, no Talos round trip, no second credential. The
column says so, because a column that silently mixes the three reads as fact:

```
   current: what each node runs now, from the guest cluster's own Node objects
   • d-stackops-1010-qjxq-ctp0    controlplane     v1.13.7 → v1.13.9
```

Whichever of the other two disagrees is named rather than left to stand in for
reality — a fleet declaring v1.13.9 while running v1.13.7 makes an upgrade to
v1.13.8 read as a *downgrade*:

```
   note:   nodes run v1.13.7, but machines declare v1.13.9 in spec.os.imageID.
           Desired state runs ahead of the nodes; the rows above show what the nodes actually run.
```

The read is best-effort: upgrading a cluster whose Kubernetes API is unreachable
is a large part of why this command exists, so a guest cluster that cannot be
reached weakens the column instead of blocking the run. It falls back to the
operator-verified `TalosVersionEnforcement` condition — free, on the CR already
in hand, but cluster-wide, so it only speaks when every node agrees — and then
to the installed version, each time saying which:

```
   current: what each node runs now, except 1 of 4 falling back to desired state
   current: desired state for all 4 — no running version could be read
```

This is the same distinction `viti kc list` draws with its `~` prefix:
`~1.13.9` means *unverified — machine image target only, runtime unknown*.

### Nodes already on the target are left alone

An upgrade to where a node already is costs a reboot and buys nothing. So a node
running exactly what the upgrade would install is skipped:

```
✅ All 4 targeted node(s) of admin@pos1-mgmt-001/vitistack-stackops/d-stackops-1010 already run the target
   image:  factory.talos.dev/nocloud-installer/b0f2…9365
   current: what each node runs now, from the guest cluster's own Node objects
   ✓ d-stackops-1010-qjxq-ctp0    controlplane     v1.13.9 → v1.13.9   already on target, not touched
   ✓ d-stackops-1010-qjxq-wrk0    worker           v1.13.9 → v1.13.9   already on target, not touched
   ✓ d-stackops-1010-qjxq-wrk1    worker           v1.13.9 → v1.13.9   already on target, not touched
   ✓ d-stackops-1010-qjxq-wrk2    worker           v1.13.9 → v1.13.9   already on target, not touched
   pin:    install_image already v1.13.9
```

**Both the version and the schematic must match**, and both come from the node
itself — `status.nodeInfo.osImage` and the `extensions.talos.dev/schematic`
annotation, which Talos lists in `talos.dev/owned-annotations` and so keeps
current. Version alone is not enough: `--schematic <new-id>` changes the
extensions while leaving the version alone, so `v1.13.9 → v1.13.9` can be real
work.

Note the schematic is an *annotation*. The **labels** carry the individual
extensions (`extensions.talos.dev/iscsi-tools`, `qemu-guest-agent`, …) and never
the schematic id, so a label lookup finds nothing and concludes the wrong thing.

**A node that cannot be verified is always upgraded.** Both halves must be
known and both must match; anything else means *cannot tell*, and the answer to
that is never "skip". The node a half-finished run left behind is precisely the
one that fails to answer — it is `NotReady`, which is why it did not — and
reading its silence as "already done" would strand the only node the command
was run for.

Skipping is off entirely for anything a Node object cannot report, and the plan
says which:

```
   skip:   off — --platform changes where Talos reads its config on the next boot, which a node does not report — no node is skipped
```

| Flag | Why skipping is off |
| ---- | ------------------- |
| `--platform` | No counterpart on the node, and a wrong skip does not fail loudly — the node stays `Ready` with its config source gone |
| `--registry` | Cannot be checked against what the nodes report |
| `--image` | Replaces the whole reference, so its registry and platform are whatever was typed |
| `--force` | Asked for: upgrade every node regardless, for reinstalling over one that reports the right version but is not behaving like it |

When every targeted node has arrived, the pin is still reconciled — the nodes
are there, and leaving the operator's desired state behind them would have it
undo their arrival — and the command exits without touching anything.

### Transitions Talos will not make are refused

Talos upgrades **one minor version at a time** and **does not go backwards**.
Neither rule is visible in an installer reference, so a downgrade and a
three-minor jump render exactly like the patch bump beside them:

```
   • d-stackops-1010-qjxq-ctp0    controlplane     v1.13.9 → v1.13.7
   ⚠️  d-stackops-1010-qjxq-ctp0: v1.13.9 → v1.13.7 is a downgrade
       Talos does not go backwards: the installed system would be newer than the installer
       writing over it.
       Pass --allow-unsupported-version to do it anyway.
```

The run is refused **before the pin moves**. The pin is written ahead of any
node precisely so an interrupted run gets finished rather than undone — which
means a pin recording a version Talos will not upgrade to would have the
operator keep being driven toward it.

The comparison is against **what each node runs**, which is what makes the
guard usable. `spec.os.imageID` runs ahead of the nodes and
`machine.install.image` lags behind them, so a guard built on either would
refuse legitimate upgrades: the first real run of this command was
`v1.12.7 → v1.13.8` — a legal one-minor step — while the machines declared
`v1.13.9`. Checked against the declared version, that reads as a downgrade and
would have been blocked.

**A version that cannot be read is never a refusal.** A cluster whose running
version is unavailable is one of the clusters this command exists to rescue.
The check says it could not run rather than blocking:

```
   note:   version check skipped for 1 of 4 node(s) — needs both a running version and a
           version-tagged target to compare
```

That line appears only for a *partial* gap. When no node reported a version at
all, the `current:` line has already said the plan is showing desired state, and
repeating it would train the reader past the case that means something.

`--image` overrides that outright. Matching the schematic and the platform is
then yours to get right.

### Changing the image — all four parts, or the whole thing

An Image Factory reference has four independently editable parts. Anything you
do not name is carried over from the node:

```
<registry>          / <platform>-installer / <schematic> : <version>
--registry            --platform             --schematic   --to
```

| Flag | Changes | Why you would |
| ---- | ------- | ------------- |
| `--registry` | Where the image is pulled from | A local mirror, an air-gapped copy |
| `--platform` | Which platform variant | The cluster moved provider — `metal` → `hcloud` |
| `--schematic` | System extensions and kernel arguments | Adding or dropping extensions |
| `--to` | The Talos release | An upgrade |
| `--image` | The whole reference | When the parts are not what you want to think in |

They combine, so a provider move, an extension change and a version bump are
one rolling pass rather than three:

```sh
viti talos upgrade-node my-cluster --platform hcloud --schematic <id> --to v1.13.9 --dry-run
```

Only `--to` works on any reference. The other three are *positional* parts of a
factory reference, so they need one — a plain `ghcr.io/siderolabs/installer` is
refused rather than rewritten, since rewriting one of its segments would
produce an image that does not exist and fail as a pull error mid-rollout.

#### `--platform` is the sharp one

The platform baked into the installer decides where Talos reads its machine
config and network settings on the next boot. A wrong one **does not fail
loudly** — the node comes back up `Ready` with its config source silently gone.

So each node's actually-running platform is read from its own
`PlatformMetadata` and shown against the target:

```
   • cephtest-036-ab36-ctp0   controlplane
       from  factory.talos.dev/nocloud-installer/b0f2…9365:v1.13.6
       to    factory.talos.dev/hcloud-installer/b0f2…9365:v1.13.9
   ⚠️  platform: nodes report nocloud, installer targets hcloud
       The platform decides where Talos reads its config and network settings on the next
       boot. A wrong one does not fail loudly — the node comes back Ready with its config
       source gone. Make sure this matches where the cluster actually runs.
```

A mismatch is *expected* when this is the change you intend — a node
provisioned from a metal installer reports `metal` even when it is a Hetzner
VM, and that is the reason to switch. It is shown so the switch is deliberate.

#### Getting a schematic id

Build the new schematic at the factory first — what you pass is the id it
returns, not a file:

```sh
curl -X POST --data-binary @schematic.yaml https://factory.talos.dev/schematics
# {"id":"<64 hex characters>"}

viti talos upgrade-node my-cluster --schematic <id> --dry-run   # extensions only
viti talos upgrade-node my-cluster --schematic <id> --to v1.13.9   # and a version bump
```

**Any of these also update the cluster's pinned `install_image`**, in the credentials
Secret, before any node is touched:

```
   • cephtest-036-ab36-ctp0   controlplane
       from  factory.talos.dev/nocloud-installer/b0f2…9365:v1.13.6
       to    factory.talos.dev/nocloud-installer/1111…1111:v1.13.9
   pin:    install_image factory.talos.dev/nocloud-installer/b0f2…9365:v1.13.2 → factory.talos.dev/nocloud-installer/1111…1111:v1.13.9
```

That pin is the talos-operator's desired state and it **wins over what the
nodes report** — its resolution order is the pin first, a live-fetch from a
running control plane second, a provider default last. Change the image on the
nodes without changing the pin and the operator's next version enforcement
resolves from the old pin, swaps only the version tag, and puts the old
schematic back. Recording the intent first also means an upgrade interrupted
halfway leaves the operator finishing the job rather than undoing it.

`--no-pin` opts out and says so:

```
   ⚠️  install_image stays factory.talos.dev/…:v1.13.2 (--no-pin) — the operator will revert this
```

If the targeted nodes would end up on different images, there is no single
image to pin and the command refuses rather than picking one — pinning one of
several would quietly converge the rest onto it.

Note that the talos-operator also enforces a cluster's Talos version. Running
`upgrade-node` by hand is for the cases it cannot resolve.

### When a node fails mid-roll

Nodes are upgraded one at a time, so a failure stops the run with some nodes
done and the rest untouched. Two consequences of that are easy to miss, so both
are reported rather than left to be discovered:

```
❌ cephtest-036-ab36-wrk1 was not upgraded — 3 of 5 node(s) are done.
   cephtest-036-ab36-wrk1 may be left cordoned. The upgrade cordons and drains a node before rebooting it
   and uncordons it only once the upgrade succeeds, so check it before anything else:
     kubectl get node cephtest-036-ab36-wrk1
     kubectl uncordon cephtest-036-ab36-wrk1   # only if it is SchedulingDisabled and not coming back
   install_image is already pinned to the target, so resuming changes nothing about the
   operator's desired state:
     factory.talos.dev/nocloud-installer/b0f2…9365:v1.13.9
   Resume on the 2 node(s) left rather than re-running the cluster, which would reboot the
   3 that are done:
     viti talos upgrade-node cephtest-036 -N cephtest-036-ab36-wrk1 -N cephtest-036-ab36-wrk2 --to=v1.13.9
```

`talosctl` cordons and drains a node before rebooting it and uncordons it only
once the upgrade succeeds, so a failed node is left `SchedulingDisabled` — a
worker the cluster will not schedule onto, with nothing else on screen saying
so. The warning is omitted under `--stage` and `--no-drain`, where nothing was
drained in the first place.

The resume command is rebuilt from the flags the run was actually given, so it
carries `--stage`, `--schematic`, `--endpoint` and the rest; only `--node` is
replaced, with the nodes that are left. It names itself the way you reached it
— `viti talos`, or `viti t` through the alias — falling back to `viti-talos`
when the binary is installed standalone with no `viti` beside it to dispatch
through. Re-running the whole cluster also works
— the pin already holds the target, so nothing fights the operator — but it
reboots every node that already succeeded.

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
