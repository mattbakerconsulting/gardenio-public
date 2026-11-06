# Fix gVisor thaw rc=128: relocate the rootfs-overlay filestore to a host-visible medium (`overlay-dir-medium`)

**Bug:** ka-0000008217 · **Decision:** ka-0000003129 (operator chose `overlay-dir-medium`) ·
**Epic:** ka-0000007417 (Phase 4) · **Node-substrate escalation:** ka-0000004674 (original grant) ·
**Operator:** Matthew Baker (slack:U0OPERATOR1)

## TL;DR

Prod gVisor thaw fails closed at `restore rc=128`: `vfs.CompleteRestore` 9p-walks
`home/agent/workspace` in mount `agent:/` and ENOENTs. Root cause (proven on-node by crew
f4m, canary ka-0000000298): runsc runs with the **default `--overlay2=root:self`**, which
stores the agent rootfs-overlay **upper layer** in a sentry-internal **filestore file**
(`.gvisor.filestore.*`) living *inside the container rootfs*. The freeze relocation
(`freezethaw_node.go` step [3]) does `cp -a $rootfs/. $LOWER && rm -f $LOWER/.gvisor.filestore.*`
— it copies only the **gofer view** (the empty image WORKDIR) and then **deletes the filestore**,
where every runtime write under `/home/agent/workspace` (the agent's CWD, where it clones repos)
actually lives. The preserved lower therefore carries an *empty* workspace; restore 9p-walk ENOENTs.

The operator-chosen direction is to **move the rootfs-overlay filestore out of the rootfs onto a
host-visible, GC-safe path** so it is no longer stripped at freeze and can be carried across C/R.

> **✅ VERIFIED ON-NODE (ka-0000007329, 2026-06-10) — supersedes the "must explicitly copy
> the filestore" claim in correction 1 below.** A live `runsc checkpoint --leave-running`
> → `runsc restore --overlay2=root:dir=…` cycle on a dir-medium gVisor node restored a
> runtime marker written under the rootfs-overlay upper (`rc=0`). Crucially, under
> `root:dir` runsc creates the filestore file (`/var/lib/gardenio/overlay/runsc-filestore-<rand>`)
> and **immediately unlinks it**, holding only an open fd — the dir is **empty on disk**.
> The upper is therefore serialized **into `checkpoint.img`** by `runsc checkpoint`, **not**
> carried as a host file. So freeze does **NOT** copy the external filestore (there is nothing
> to copy); the controller half (ka-0000007239) reduces to **probing the live medium at freeze,
> self-describing it on the snapshot, and replaying `--overlay2=root:dir=…` to every `runsc
> restore`** so the restored sentry rebuilds the upper from the image under the matching medium.
> Steps 2/3 below are simplified accordingly: the `FT_BUNDLE_OK` upper-preserve assertion and
> the `overlay-upper/` bundle copy are unnecessary.

## ⚠ Two load-bearing corrections to the option's literal premise

The decision option A label says: *"switch runsc rootfs overlay to a host-visible medium
(`--overlay2=root:dir`) so the rootfs-overlay upper is a normal host dir the freeze `cp -a`
already captures."* Two parts of that are imprecise, and shipping them naively would be the
**third no-op** on this bead (two prior crews refused exactly this trap):

1. **`--overlay2=root:dir=/path` is NOT a browsable directory tree.** Per gVisor primary sources
   ([filesystem guide](https://gvisor.dev/docs/user_guide/filesystem/),
   [flags.go](https://github.com/google/gvisor/blob/master/runsc/config/flags.go)), the `dir`
   medium creates a **single opaque filestore file** at `/path` ("…`dir=/abs/dir/path` in which
   filestore will be created"). It is the *same* tmpfs-backing filestore as `self`, only stored at
   a chosen host path instead of inside the rootfs. So the existing `cp -a $rootfs/.` does **not**
   automatically capture it — the freeze code must **explicitly copy that external filestore file**
   into the bundle, and thaw must **restore it and point `runsc restore` at the same medium**.

2. **The medium cannot be set per-pod via OCI annotation.** `flagOverlay2` *is* in gVisor's
   `overrideAllowlist`, but its `checkOverlay2` validator only permits the `memory`/`self` mediums
   through a `dev.gvisor.flag.overlay2` annotation — **not `dir`** (and `none` is not in the
   allowlist at all). So `root:dir` must be a **node-level containerd runtimeArg** (or requires a
   node-wide `--allow-flag-override`, which we do not want for a fleet runtime). This confirms the
   option's own note: it is a node/nodeadm change owned by escalation ka-0000004674, plus a
   re-validation of checkpoint/restore under the new medium.

### Why this is still the right direction (not a no-op)

With the filestore at a **stable host path outside the rootfs** (e.g.
`/var/lib/gardenio/overlay/<sandbox-or-cid>/`):

- The in-rootfs `rm -f $LOWER/.gvisor.filestore.*` strip no longer touches it (it isn't in the
  rootfs anymore), so freeze stops destroying the runtime upper.
- Freeze can **deterministically copy the external filestore file** into the bundle alongside the
  lower, with the same GC-safe + reflink machinery already used for the lower.
- Thaw reconstructs the overlay and runs `runsc restore --overlay2=root:dir=<restored-path>` so the
  restored sentry's filestore matches the checkpoint's expectation — sidestepping the
  fresh-empty-upper / `.gvisor.filestore` collision that the `self`-medium strip was originally
  added to dodge (PoC bug #4), which is the exact hazard option B (`preserve-filestore` under
  `self`) would have had to solve.

## Alternative considered & why dir over none

`--overlay2=none` (overlay OFF) makes rootfs writes **pass through the gofer to the host
snapshot** ("If you need to propagate rootfs changes to the host filesystem, disable it with
`--overlay2=none`") — at which point the *existing* `cp -a $rootfs/.` genuinely would capture
`/home/agent/workspace`, with **zero freeze/thaw code change**. It is the literal-cp-captures
path the option author was reaching for. We do **not** choose it because:

- It removes the rootfs overlay entirely, regressing the performance win gVisor ships on by
  default (the [2023 rootfs-overlay work](https://gvisor.dev/blog/2023/05/08/rootfs-overlay/)) and
  routing every rootfs write through the 9p gofer on the fleet hot path.
- It changes write-visibility semantics fleet-wide for any runsc agent, a larger behavioural blast
  radius than relocating the filestore.

`root:dir` keeps the overlay performance characteristics and confines the change to *where the
upper is stored*, which is the minimal change that makes the upper survivable across C/R. (If the
freeze/thaw copy of the external filestore proves fiddly during canary, `none` remains the
documented fallback that needs no freeze/thaw code — note it on the canary bead.)

## Implementation

Default-OFF, fail-closed, runsc-only — same discipline as every prior freeze/thaw increment.

### Step 1 — node substrate (escalation, operator/cluster-admin owned)

Add the overlay2 runtimeArg to the `runsc` containerd runtime handler on the gVisor node pool, via
**nodeadm `NodeConfig.spec.containerd.config`** (the only block that survives `config.toml`
regeneration — escalation ka-0000004674 durability note). The EC2NodeClass `gvisor-runsc` userData
already carries:

```toml
[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.runsc]
runtime_type = "io.containerd.runsc.v1"
```

Add the runtime options so runsc is invoked with the dir medium for every pod on the pool:

```toml
[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.runsc]
runtime_type = "io.containerd.runsc.v1"
[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.runsc.options]
TypeUrl = "io.containerd.runsc.v1.options"
ConfigPath = "/etc/containerd/runsc.toml"
```

with `/etc/containerd/runsc.toml` (written by the same userData shellscript that installs the
runsc binaries):

```toml
[runsc_config]
  overlay2 = "root:dir=/var/lib/gardenio/overlay"
```

(`overlay2` under `[runsc_config]` is how the containerd-shim-runsc passes a runsc flag; confirm
the exact key name against the installed `containerd-shim-runsc-v1` release on first canary.)

This must be applied to the **chart-managed** path too: `gvisor.nodePool.nodeClass.userData` in
`values.yaml` (documented in `gvisor.yaml`), so a chart-managed pool matches the operator pool.

**Re-validate C/R under the new medium on a throwaway gVisor node before any fleet exposure** —
checkpoint/restore behaviour with a `dir`-backed filestore is the unknown that gates everything.

### Step 2 — freeze (controller, `freezethaw_node.go` freezeTmpl step [3])

- **Stop stripping the filestore from a path that no longer holds it,** and instead **preserve the
  external filestore.** With `root:dir=/var/lib/gardenio/overlay`, the agent rootfs upper's filestore
  lives under that host dir (keyed per sandbox/cid), *not* in `$rootfs`. So:
  - The merged-rootfs `cp -a $T/$cid/rootfs/. $LOWER` still captures the **lower** (image layers) —
    unchanged.
  - Add a sibling copy of the **external filestore dir** for this cid into the bundle (e.g.
    `$BUNDLE/$dstn/overlay-upper/`), reusing the same `HMNT` + `cp -a --reflink=auto` + GC-safe
    `$BUNDLE` pattern as the lower relocation. Record its path in `.overlay-opts` (or a new
    `.overlay-upper-path` sentinel) so thaw knows where to put it back.
  - The in-rootfs `rm -f $LOWER/.gvisor.filestore.*` becomes a no-op (nothing there) but stays as a
    belt-and-suspenders guard.
- Extend the `FT_BUNDLE_OK` completeness guard ([3b]) to assert the preserved upper is present +
  non-empty, so freeze fail-closes (cold-start) rather than persist an un-thawable snapshot.

### Step 3 — thaw (controller, `freezethaw_node.go` thawTmpl step [2] `remount_rootfs` + step [5] restore)

- Materialize the preserved external filestore back to its host path (`/var/lib/gardenio/overlay/<cid>/`)
  before restore, from `$BUNDLE/$dstn/overlay-upper/`, in the **host ns** (`HMNT`) per the
  ka-0000002536 mount-ns invariant.
- Pass the medium to restore: `runsc restore --image-path=$IMG --bundle $BUNDLE/<c>
  --overlay2=root:dir=/var/lib/gardenio/overlay --detach <cid>` (both the backgrounded root sandbox
  restore and each sub restore). The medium **must match** the checkpoint's, or restore will not
  find the upper.
- The existing **POST-MOUNT CWD-dentry assertion** (added in MR !3974, the diagnostic guard) now
  becomes a *positive* check: after the filestore is restored, `home/agent/workspace` must resolve
  with its runtime contents. Keep it — it converts any residual gap into a by-name FATAL.

### Step 4 — config / flag plumbing (controller)

The medium is a **node** property, not a controller knob, so the controller does not *set* it. But
freeze/thaw must **know** the medium to pass `--overlay2` to restore and to locate the external
filestore. Options, simplest first:

- **A node-probe read** at freeze: parse the live agent sub-container's overlay medium from
  `/proc/mounts` or the runsc state (the freeze script already inspects live overlay opts at [3]),
  persist it in the snapshot, and replay it at thaw. This keeps the controller agnostic and is the
  most robust (the snapshot self-describes its medium).
- A single controller env/value (`gvisor.overlayDir`, mapped to e.g. `GVISOR_OVERLAY_DIR`) as a
  fallback when the probe can't resolve it. Default empty ⇒ today's behaviour (assume `self`, no
  external upper) so non-`dir` nodes are untouched.

Prefer the snapshot-self-describes approach; add the value only if needed.

### Step 5 — docs + values

- `gardenio/docs/gvisor-runsc-prod.md`: document the overlay2 medium requirement, the
  `/var/lib/gardenio/overlay` host path, the nodeadm runtimeArg block, and the C/R-must-match rule.
- `values.yaml` `gvisor.nodePool.nodeClass.userData` example: include the runsc.toml + runtime
  options block.

## Test plan

- **Unit (controller, `go test ./...`):** extend `freezethaw_node.go` template tests
  (`freezethaw_test.go`, `freezethaw_ncontainer_test.go`) — assert the freeze script preserves the
  external upper + emits the completeness sentinel; assert the thaw script materializes it and
  passes `--overlay2=root:dir=…` to every `runsc restore`; assert default-empty medium leaves the
  script byte-identical to today (zero blast radius on `self` nodes). `bash -n` the rendered
  scripts. Mirror the existing `TestThawScript_AssertsCWDDentryAfterRemount` pattern.
- **Substrate re-validation (throwaway node):** a single freeze→thaw of a hand-rolled runsc pod on
  a node carrying the new medium, confirming C/R succeeds with a `dir`-backed filestore and that a
  runtime marker written under `/home/agent/workspace` survives the cycle.
- **Driven-thaw canary (gvisor-poc role, on-node probe):** the decisive proof — freeze a real agent
  canary that has cloned a repo into `/home/agent/workspace`, drive a thaw (status→in_progress,
  placeholder Pod B / R3), and confirm `restore rc=0` with the workspace contents intact. This is
  the gate for the epic. **Note** the separate latent PodB-init-clone-on-cold-netns bug
  (kd-…, filed by f4m) can fail-close the thaw *before* restore on a cold netns — re-drive or use a
  warm placeholder so the barrier restore actually runs.

## Rollback

Every layer is independently reversible:
- Node: drop the runtimeArg from nodeadm userData ⇒ runsc reverts to `root:self` (default). Old
  snapshots taken under `self` remain consistent because thaw reads the medium from the snapshot.
- Controller: `gvisor.freezeThaw.enabled=false` ⇒ no freeze/thaw at all (cold start), the
  ultimate kill switch (epic success criterion 5).

## Files touched (estimate)

- `gardenio/controller/internal/podmanager/freezethaw_node.go` — freezeTmpl [3] + thawTmpl [2]/[5]
- `gardenio/controller/internal/podmanager/freezethaw.go` — snapshot field for the medium (if
  snapshot-self-describes)
- `gardenio/controller/internal/podmanager/freezethaw_test.go`,
  `freezethaw_ncontainer_test.go` — template assertions
- `gardenio/helm/gardenio/values.yaml` + `templates/gvisor.yaml` — userData example + docs
- `gardenio/docs/gvisor-runsc-prod.md` — medium runbook
- **Node substrate (escalation):** EC2NodeClass `gvisor-runsc` userData / nodeadm NodeConfig
