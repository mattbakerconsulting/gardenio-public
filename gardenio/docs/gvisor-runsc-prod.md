# gVisor (runsc) sandboxed runtime in prod — runbook

**Epic:** ka-0000007417 (Phase 4: Productionize gVisor agent freeze/thaw) ·
**Tasks:** ka-0000007200 (substrate), ka-0000003688 (lifecycle wiring),
ka-0000004620 (feature flag + rollout/rollback) ·
**Substrate escalation:** ka-0000004674 ·
**PoC report:** `gardenio/docs/gvisor-runsc-cr/report.md`

## What this is

A `runsc` (gVisor) RuntimeClass and an optional Karpenter node pool, shipped in
the prod Helm chart (`gardenio/helm/gardenio/`), **flag-gated and default OFF**.
This is the substrate half of Phase 4: it makes the sandboxed runtime *available*
so pods can opt into it via `runtimeClassName: runsc`. It does **not** route any
agent onto runsc by itself, and it does not invoke Freeze/Thaw — those are
separate Phase 4 children.

The Freeze/Thaw verbs themselves already exist as merged-but-dormant library
code (`gardenio/controller/internal/podmanager/freezethaw.go`), and the
`RuntimeClassName` podspec plumbing is in place (`controller/cmd/controller/
podspec.go` lines 165-169/417, resolved from role/project/agent `runtime_class`).
What was missing in prod — and what this task adds — is the RuntimeClass object
in the chart so a `runtime_class: runsc` opt-in can actually schedule.

## Values

```yaml
gvisor:
  enabled: false            # master flag; OFF emits nothing
  runtimeClass:
    name: runsc
    handler: runsc
    scheduling:             # auto-pins runsc pods onto the gVisor pool
      nodeSelector: {gvisor: "enabled"}
      tolerations:
        - {key: gvisor, operator: Equal, value: enabled, effect: NoSchedule}
    overhead: {}            # optional sentry cpu/memory accounting
  nodePool:
    create: false           # reference the operator-provisioned pool by default
    # ... see values.yaml for the full chart-managed Karpenter NodePool/EC2NodeClass
```

### Two deployment modes

1. **RuntimeClass-only (recommended, default when enabled).**
   `gvisor.enabled=true`, `gvisor.nodePool.create=false`. The chart creates only
   the `runsc` RuntimeClass and references the **existing** operator-provisioned
   pool (`gvisor-poc` / `gvisor-runsc`, label/taint `gvisor=enabled`). This is the
   safest path: the node pool, its IAM role, subnets, security groups, and — most
   importantly — the **nodeadm-registered runsc containerd handler** are already
   verified end-to-end (escalation ka-0000004674). The RuntimeClass `scheduling`
   block carries the matching nodeSelector + toleration, so a caller only sets
   `runtimeClassName: runsc` and Kubernetes pins the pod onto the pool.

2. **Chart-managed node pool (advanced).**
   `gvisor.nodePool.create=true`. The chart also templates the Karpenter
   `EC2NodeClass` + `NodePool`. Requires the Karpenter CRDs installed on the
   cluster and cluster-specific values: `nodeClass.role` (node IAM role),
   `nodeClass.subnetSelectorTerms`, `nodeClass.securityGroupSelectorTerms` (the
   template fails fast if any are missing), and a `nodeClass.userData` that
   registers runsc in **nodeadm NodeConfig.spec.containerd** — see the durability
   note below.

## Node sizing: fat-node memory floor for freeze/thaw (ka-0000002348)

**The gVisor pool must only provision nodes with enough RAM to clean-rehydrate a
heavy checkpoint, or freeze/thaw silently degrades to cold-start — or worse,
OOM-kills the restore.** This is the durable fix for node-fit blocker
**ka-0000006507**.

- A context-loaded agent freezes at **~2.947 GiB** `pages.img` (measured, PoC
  **ka-0000002587**, bundle `/bundle/ka-0000002092` — matches canary-#12).
- **Two** pre-thaw checks a heavy restore must clear, and BOTH grew after the
  first `6000` (m6a.large) floor was chosen:
  - **NODE mem-gate** (`freezethaw.go` `preThawMemoryGate` → `modeledRestoreNeed`,
    **ka-0000007579**) — fail-close + non-retryable: refuses a restore
    (cold-starts instead) unless the freeze node's live free RAM ≥
    `pages*1.5 + 768Mi*containers + 512Mi` ≈ **6.42 GiB** for a heavy 2-container
    agent. (The original doc'd `pages.img + 512Mi ≈ 3.46 GiB` model was the
    canary-#14 UNDER-model that OOM-SIGKILLed the agent-sub restore.)
  - **Pod B cgroup LIMIT** (`modeledRestorePeakLimit`, canary #15
    **ka-0000000413**): the placeholder's container `memory.max` is sized to
    `pages*3.0 + 768Mi*containers + 512Mi` ≈ **10.84 GiB**, inside which
    `runsc restore` peaks **>2.71×** the image. A node whose allocatable RAM is
    below this limit cgroup-OOM-SIGKILLs the restore mid-flight.
- **`m6a.large` (8192 MiB total, ~7.1 GiB allocatable) now FAILS** the cgroup
  limit (10.84 GiB) — it schedules the pod but OOM-kills every heavy rehydrate,
  strictly worse than a clean cold-start. **`m6a.xlarge` (16384 MiB total,
  ~15.1 GiB allocatable) clears BOTH** the 6.42 GiB node-gate and the 10.84 GiB
  cgroup limit with headroom. `c6a.large` (4096 MiB) never had a chance.

**The requirement that enforces this** (chart-managed pool, `values.yaml`
`gvisor.nodePool.requirements`; it **mirrors the live out-of-band `gvisor-poc`
NodePool**, which runs this exact floor on prod-gardenio-eks / acct 123456789012
today):

```yaml
- key: "karpenter.k8s.aws/instance-memory"   # Karpenter reports MiB
  operator: "Gt"
  values: ["12000"]                           # excludes 8 GiB *.large, admits 16 GiB *.xlarge
```

`Gt 12000` cleanly splits the 8192 MiB m-family `*.large` from the 16384 MiB
`*.xlarge`, so Karpenter's cheapest fit under `instance-category [c,m]` becomes
`m6a.xlarge` and it can **never** provision an unfittable node again. **Cost
delta:** `m6a.xlarge` on-demand (us-east-1) ≈ $0.1728/hr vs `c6a.large`
≈ $0.0765/hr — the price of a pool that actually delivers the warm-resume the epic
promises instead of silently OOM-killing or cold-starting every heavy agent.

The controller carries the matching **interim** analogue (MR !5116, const
`FatNodeMemoryFloorMiB`): a *soft* node-affinity preferring the same
`instance-memory Gt 12000` for freeze-eligible agents, so a heavy freeze lands on
an m6a.xlarge and can clean-rehydrate — reversible and zero-cost. With the live
NodePool already carrying the hard floor, the soft preference is redundant
belt-and-suspenders.

> **Live status (as of ka-0000002348 re-scope):** the physical provisioning is
> **done** — `kubectl get nodepool gvisor-poc` shows `instance-memory Gt 12000`
> and 3× `m6a.xlarge` nodes are live. This chart/controller/doc change reconciles
> the in-repo source-of-truth (which had drifted to the stale, now-unsafe `6000`)
> so a future `nodePool.create=true` rebuild can never regress the live floor.

## Durability note: runsc must be registered via nodeadm NodeConfig

(From escalation ka-0000004674, verified the hard way during the PoC.)

The naive approach — boot the node, run `runsc install`, restart containerd —
does **NOT** survive on AL2023: nodeadm regenerates `/etc/containerd/config.toml`
after `scripts-user`, wiping the handler. The block that actually persists must
be merged via NodeConfig before containerd starts:

```toml
[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.runsc]
  runtime_type = 'io.containerd.runsc.v1'
```

Put this in the node's nodeadm NodeConfig userdata (or the chart-managed
`gvisor.nodePool.nodeClass.userData`), alongside installing `runsc` and
`containerd-shim-runsc-v1`. Confirmed working: runsc `release-20260525.0` on
containerd 2.2.3.

## Overlay2 medium: relocate the rootfs-overlay filestore for freeze/thaw

(Plan `gardenio/plans/gvisor-thaw-overlay-dir-medium.md`, bug ka-0000008217,
operator decision ka-0000003129, node-substrate task ka-0000007329.)

Freeze/thaw on a gVisor node **requires** the runsc rootfs-overlay to be backed
by a **host-visible directory** rather than the default `self` medium. By default
runsc runs with `--overlay2=root:self`, which stores the agent rootfs-overlay
**upper layer** in a sentry-internal filestore file (`.gvisor.filestore.*`) *inside
the container rootfs*. The freeze relocation (`freezethaw_node.go` step [3]) copies
only the gofer view (empty image WORKDIR) and then **deletes that filestore** —
destroying every runtime write under `/home/agent/workspace`. Restore then 9p-walks
the now-empty workspace and fails closed at `restore rc=128`.

Setting `--overlay2=root:dir=/var/lib/gardenio/overlay` moves the upper's filestore
to a stable host path **outside** the rootfs, so the freeze strip no longer touches
it and freeze can deterministically preserve it into the bundle (controller half:
ka-0000007239).

**The medium is a node-level containerd runtimeArg, not a per-pod knob.** gVisor's
`overrideAllowlist` only admits the `memory`/`self` mediums through the
`dev.gvisor.flag.overlay2` OCI annotation — **not `dir`** — so `root:dir` must be
set on the node's runsc containerd runtime handler. It is wired in two parts, both
in the nodeadm `NodeConfig.spec.containerd.config` block (the only block that
survives `config.toml` regeneration):

```toml
# Register runsc AND point it at a shim config file.
[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.runsc]
runtime_type = "io.containerd.runsc.v1"
[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.runsc.options]
TypeUrl = "io.containerd.runsc.v1.options"
ConfigPath = "/etc/containerd/runsc.toml"
```

…and the shim config `/etc/containerd/runsc.toml` (written by the same userData
shellscript that installs the runsc binaries; the `[runsc_config]` key is the
runsc flag name minus the leading `--`, per the gVisor containerd-config guide):

```toml
[runsc_config]
  overlay2 = "root:dir=/var/lib/gardenio/overlay"
```

The shellscript must also `install -d -m 0711 /var/lib/gardenio/overlay` so the
filestore dir exists before the first runsc sandbox starts. See the worked
`gvisor.nodePool.nodeClass.userData` example in `values.yaml`.

**C/R must match.** A checkpoint taken under `dir` can only be restored under the
same medium, so `runsc restore` must be passed `--overlay2=root:dir=…` to match. The
snapshot self-describes its medium (plan Step 4) so thaw replays the right one and
non-`dir` (`self`) nodes are untouched. **Rollback:** drop the runtimeArg from the
nodeadm userData ⇒ runsc reverts to `root:self` (default); old `self`-medium
snapshots stay consistent because thaw reads the medium from the snapshot.

This was applied to BOTH the live operator-provisioned EC2NodeClass `gvisor-runsc`
userData AND the chart-managed `gvisor.nodePool.nodeClass.userData` example, so a
chart-managed pool matches the operator pool.

### Controller freeze/thaw handling of the dir medium (ka-0000007239)

On-node substrate revalidation (ka-0000007329) established the load-bearing fact
that shapes the controller half: under `root:dir`, runsc creates a **single**
filestore file `/var/lib/gardenio/overlay/runsc-filestore-<rand>` and **immediately
unlinks it**, holding only an open fd — the dir is **empty on disk**. The
rootfs-overlay upper is therefore serialized **into `checkpoint.img`** by `runsc
checkpoint`, NOT carried as a host file. A hand-rolled `runsc checkpoint
--leave-running` → `runsc restore --overlay2=root:dir=…` cycle on a dir-medium node
restored a runtime marker written under the overlay upper (`rc=0`), with the restore
debug log confirming `Overlay: root:dir=…` and `--gofer-filestore-fds` rebuilt at
boot.

So the controller does **not** copy the overlay dir at freeze. The change is minimal
and snapshot-self-describing:

- **Freeze** probes the live medium off the sandbox boot args (`--overlay2=…`) and
  emits it as `FT_OVERLAY_MEDIUM`; the controller persists it on the snapshot
  (`Snapshot.OverlayMedium`).
- **Thaw** renders `--overlay2=<medium>` (via `overlayRestoreFlag`) into the
  `OVERLAY_FLAG` shell var and replays it on **every** `runsc restore` (background
  sandbox root + each sub). A `self`/empty medium renders `OVERLAY_FLAG=''`, leaving
  the restore lines **byte-identical** to the pre-fix script — zero blast radius on
  non-`dir` nodes and on every snapshot taken before this change.

The existing freeze `rm -f $LOWER/.gvisor.filestore.*` strip stays as a
belt-and-suspenders no-op (the dir-medium filestore was never in `$rootfs`).

## Opting an agent/role onto runsc

Once `gvisor.enabled=true`, a workload runs sandboxed only if it sets
`runtime_class: runsc` at the agent, role, or project level (resolution order in
`podspec.go`). Nothing is opted in by this chart change. Example role field:

```
runtime_class: runsc
```

The podspec plumbing copies this to `Pod.spec.runtimeClassName`, the RuntimeClass
`scheduling` block adds the nodeSelector/toleration, and the pod lands on the
gVisor pool. Until then, every agent keeps today's default-runtime cold-start
lifecycle.

## Freeze/thaw lifecycle wiring (task ka-0000003688)

The substrate above makes runsc *available*. The freeze/thaw lifecycle makes the
controller actually **use** it: checkpoint a runsc agent on graceful stop and
restore it on the next spawn, instead of cold-starting. This is a **second,
independent flag** — `gvisor.freezeThaw.enabled` — wired to the controller env
var `FREEZE_THAW_ENABLED` (`controller/internal/config` → `cmd/controller/
freezethaw_lifecycle.go`).

```yaml
gvisor:
  enabled: true              # substrate: RuntimeClass + node pool must exist
  freezeThaw:
    enabled: true            # controller: invoke Freeze/Thaw for runsc agents
```

**What it does when on:**

- **Dehydrate (Freeze).** On an `AgentStop` event (graceful, intentional
  despawn) for a pod pinned to `runtimeClassName: runsc`, the controller calls
  `FreezeThawManager.Freeze` — `runsc checkpoint --leave-running` plus bundle/
  warm-`/work` preservation — and persists the snapshot before the normal pod
  delete. `AgentDone` (work complete) and `AgentKill` (forced failure) are **not**
  frozen: a snapshot of a finished/failed agent has no wake to serve.
- **Wake (Thaw).** On the next `AgentSpawn` for a runsc agent that has a
  persisted snapshot, the controller calls `.Thaw` — create placeholder Pod B
  (kubelet re-provisions a fresh netns / emptyDir / projected token), then drive
  the count-gated barrier restore on the node — instead of `CreateAgentPod`. The
  first spawn of an agent has no snapshot, so it cold-starts as usual.

**Gating and fallback (all conservative, because this is opt-in):**

- Default OFF. With `freezeThaw.enabled=false` the manager is never constructed
  and every agent uses today's create/delete lifecycle untouched.
- Opt-in per agent. Even with the flag on, only agents pinned to the runsc
  RuntimeClass are frozen/thawed. Non-runsc agents never touch the path.
- Graceful degradation. A freeze failure falls through to the normal pod delete;
  a thaw failure falls through to a cold `CreateAgentPod`. A flaky node or a
  flipped flag never wedges an agent — worst case is a cold start (today's
  behaviour).
- Single cycle. The path-b PoC is node-local and single-shot; one freeze→thaw
  cycle per agent is in scope. Re-freezing a thawed Pod B (and off-node snapshot
  transport) is the snapshot-transport follow-up, not this task.

**Rehydration hand-off (C3, ka-0000004017 — now wired).** Thaw no longer leaves
the restored agent living in the past. After the process tree is verified, the
controller fires the `NodeRehydrationTrigger` (constructed in
`newFreezeThawLifecycle`): it writes a monotonic **thaw-epoch marker** onto the
agent's node-side state dir and **gates the thaw on the agent-side acks**,
fail-closed under a deadline. See the dedicated section below.

## Credential & NATS rehydration on thaw (C3, task ka-0000004017)

A thawed agent's process RAM is a byte-for-byte copy of the freeze instant: its
in-RAM IRSA/SA tokens are stale and bound to **Pod A's** identity, and its NATS
TCP 4-tuples are dead after the IP change. Resuming work without rehydrating
would either fail the next authenticated call or — worse — act under Pod A's
identity inside Pod B. The rehydration contract (design ka-0000001515, C1–C5)
makes thaw a **warm boot**: poison the in-RAM Pod-A creds, re-mint identity from
the kubelet-fresh on-disk tokens, reconnect all NATS transports + the AVA
bridge, re-announce presence, **fail-closed** under a deadline.

**How the controller drives it.** `FreezeThawManager.Thaw`, after verifying the
restore, calls `NodeRehydrationTrigger.Trigger`
(`controller/internal/podmanager/freezethaw_rehydrate.go`):

1. **Stage 0 — marker.** Writes a monotonic `thaw-epoch.json` marker into the
   agent's node-side thaw-state dir (`/var/lib/gardenio/thaw/<agent>`) over the
   same privileged-probe `NodeExecutor` the verbs use. The marker carries the
   epoch, **Pod B's kubelet UID**, the node, the freeze wall-clock (`frozen_at`),
   and the deadline. The write is base64-payload + chown-to-agent-UID + atomic
   temp-then-mv, so the in-pod watchers (running as UID 1000) read a complete
   marker and can write their acks beside it. Epoch monotonicity means a
   re-frozen-then-thawed agent never replays an old epoch.
2. **Stage 5 — gate.** Polls the node for the `gb` and `coop` consumer acks.
   Releases iff **both** ack OK **for the current epoch** within the deadline.
   If either consumer never acks, acks `ok:false`, or acks a stale epoch, the
   trigger returns an error → `Thaw` returns `(result, error)` and the agent is
   **not** marked live on a half-rehydrated identity.

**The on-disk rendezvous.** The marker/ack files live on a per-agent
**hostPath** that only runsc pods carry: volume `thaw-state` sourced from
`/var/lib/gardenio/thaw/<agent>` on the node, mounted into the pod at
`/run/gardenio-thaw` and exported as `COOP_THAW_STATE_DIR`. The controller
addresses that node directory directly via the probe chroot; the in-pod
`gb` supervisor and `coop` watchers address the same inode through the mount —
so the marker the controller drops and the acks the agent writes are the same
bytes. The placeholder Pod B inherits this volume via the snapshot's
`PodTemplate` deep-copy. Non-runsc pods get **no** hostPath (the wiring is gated
on `RuntimeClassName == runsc` in `buildVolumes`/`buildVolumeMounts`/
`buildEnvVars`).

The mount path is **canonical on `/run`, not `/var/run`** (bug
[ka-0000003421]). The restore barrier normalizes every checkpointed mount
destination `/var/run/*` → `/run/*`, so the thaw-state hostPath always re-lands
at `/run/gardenio-thaw` in the restored sandbox. A live (pre-freeze) Pod A had
`/var/run` → `/run` as a symlink, so a `/var/run`-rooted `COOP_THAW_STATE_DIR`
resolved to the same inode — but the **restored** sandbox materializes
`/var/run` as a *real* directory (no symlink), so a `/var/run`-rooted env would
point the watchers at an empty `/var/run/gardenio-thaw` while the marker sat at
the real mount `/run/gardenio-thaw`, fail-closing the C3 gate. Pinning the mount
+ env on `/run` makes `normalize(MountThawState) == MountThawState`, so the
poll path and the mount destination coincide in both the live and restored
sandbox with no symlink dependency.

**Partial-success teardown.** When `Thaw` returns `(result != nil, error)` —
Pod B is live but rehydration fail-closed — the controller's wake path deletes
Pod B **before** cold-starting, so the cold start does not orphan a running pod
(`cmd/controller/main.go` thaw branch; per the ka-0000003688 heads-up).

**Tuning the deadline.** `THAW_REHYDRATE_DEADLINE` (controller env;
`config.ThawRehydrateDeadline`) bounds the whole hand-off. Default is the
manager's built-in `DefaultRehydrateDeadline`. Rationale (design §5): thaw is
sub-300 ms, STS AssumeRoleWithWebIdentity <1 s, NATS reconnect <1 s; the rest is
retry headroom on a freshly-networked pod. Keep it comfortably under the
15-minute presence reaper.

> **`FT_SENTRY_EGRESS` single-target verdict is expected** (ka-0000005005, MR
> !5044 review). The [6c-2] in-sentry dial (ka-0000002907) resolves NATS and
> beads to ClusterIP literals in Go and passes each into the script; an
> unresolvable host yields `""` and that side SKIPS (the [6c-1] route check
> stays the floor). When only ONE of nats/beads resolves, `FT_SENTRY_EGRESS` is
> driven by the single resolved target — that single-target verdict is CORRECT
> and fail-safe, NOT a bug.

### Per-attempt thaw connect clamp (escape hatch + gb/coop lockstep)

Inside that overall deadline, **each individual reconnect attempt** is clamped
so a single black-holed dial cannot eat the whole budget. On a freshly-grafted
post-restore CNI netns, kube-proxy may not have programmed the DNAT rule for the
cross-namespace NATS / beads ClusterIP yet, so a TCP SYN to that VIP is
**black-holed** (no RST) and blocks for the underlying client's full handshake
timeout (coop async-nats default 5 s; the gb beadsapi `http.Client` carries
~30 s). At 30 s/attempt the gb consumer gets only ~2 probes against a 60 s
rehydrate deadline; clamping each attempt to ~2 s yields ~25 probes, so the
bounded retry re-probes often and lands the instant the route is programmed
(bug ka-0000001637 RANK4, ka-0000001492). The clamp **never extends** the overall
deadline — `WithTimeout` on an already-deadlined parent honors the earlier of the
two.

**Two sides, one deadline.** Both the `gb` supervisor and `coop` clamp their
per-attempt connect, and the C3 gate releases only when **both** consumers ack
within `THAW_REHYDRATE_DEADLINE`. They gate the *same* rehydrate-ack deadline, so
the **slower / un-widened side is the one that fail-closes the gate**. The two
defaults are therefore deliberately identical (**2 s**) and **MUST be kept in
lockstep** — if you change one, change the other:

| Side | Const | Source | Env override |
|---|---|---|---|
| `gb` | `thawConnectTimeoutDefault` (2 s) | `gardenio/controller/cmd/gb/agent_start_thaw.go` | `GB_THAW_CONNECT_TIMEOUT_MS` |
| `coop` | `THAW_CONNECT_TIMEOUT` (2000 ms) | `coop/crates/cli/src/transport/nats_supervisor.rs` | `COOP_THAW_NATS_CONNECT_TIMEOUT_MS` |

A CI canary keeps the two defaults honest: `TestThawConnectTimeoutLockstepWithCoop`
(`cmd/gb/agent_start_thaw_lockstep_test.go`) reads coop's `THAW_CONNECT_TIMEOUT`
out of source and fails the gb build if it ever drifts from
`thawConnectTimeoutDefault`. (The test *skips*, rather than fails, in a
controller-only checkout where the coop tree is absent, so it can only catch a
real value drift — never break an unrelated build.) Bump **both** consts in the
same change and the canary stays green.

**Operator escape hatch — widen or disable without a rebuild.** Both clamps are
overridable by env var on the agent pod, so an operator can react to a
pathological network without cutting a release. **The two knobs are NOT symmetric
on `0` — read carefully:**

- **`GB_THAW_CONNECT_TIMEOUT_MS`** (gb): a positive integer of milliseconds sets
  the per-attempt clamp; **`0` *disables* the clamp** — each attempt gets the
  full remaining deadline, i.e. the pre-fix one-budget-per-attempt behavior;
  **unset / empty / unparseable / negative** falls back to the 2 s default.
- **`COOP_THAW_NATS_CONNECT_TIMEOUT_MS`** (coop): a positive integer of
  milliseconds sets the per-attempt clamp; **`0` / empty / unparseable falls back
  to the 2 s default** — coop has **no disable sentinel** (`Some(ms) if ms > 0`),
  so `0` does *not* disable on the coop side. To approximate "disable" on coop,
  set a value at or above the rehydrate deadline (e.g. `60000`) so the clamp can
  never fire before the overall deadline does.

Because of that asymmetry, **to truly disable the clamp on both sides** you would
set `GB_THAW_CONNECT_TIMEOUT_MS=0` *and* `COOP_THAW_NATS_CONNECT_TIMEOUT_MS` to a
large value (≥ the rehydrate deadline) — `0` on coop leaves the 2 s clamp in
place. To **widen** both symmetrically (the common case), set the same
millisecond value on both, e.g. `GB_THAW_CONNECT_TIMEOUT_MS=5000` and
`COOP_THAW_NATS_CONNECT_TIMEOUT_MS=5000`. Widening only one side is usually a
mistake: the gate still waits on the narrower side.

**PSA / hostPath note for prod.** The thaw-state volume is a `hostPath`
(`DirectoryOrCreate`). hostPath volumes are forbidden under the PSA `restricted`
profile and require `baseline` (or a targeted exception) on the gVisor node
pool's namespace — the same privileged-substrate posture the probe/DaemonSet
already needs for `runsc checkpoint|restore`. Validate the namespace PSA label
and that kubelet can provision `/var/lib/gardenio/thaw/<agent>` (chowned to UID
1000) on a real cluster before enabling freeze/thaw with rehydration in that
environment. The marker dir holds only thaw-epoch JSON (no secrets); tokens are
never written there — rehydration re-reads them from their own kubelet-owned
mounts.

## The gVisor substrate namespace (fix-substrate; bug ka-0000004301)

The main release namespace (`gardenio`) enforces PodSecurity **`baseline`**, which
forbids BOTH things a runsc freeze/thaw agent needs:

- the per-agent **thaw-state hostPath** every runsc pod carries
  (`podmanager/volumes.go`, gated on `RuntimeClassName == runsc`), and
- the privileged **hostPID/hostPath gvisor-probe** the controller exec's into to
  run `runsc checkpoint|restore`.

Managed EKS does **not** expose the apiserver PodSecurity `AdmissionConfiguration`,
so a cluster-level `exemptions.runtimeClasses: [runsc]` (which would let the runsc
thaw-state hostPath through in `gardenio`) is unavailable. Relaxing the `gardenio`
namespace from `baseline` to `privileged` would weaken the posture of every other
agent. So runsc freeze/thaw agents run in a **dedicated PSA-privileged namespace**,
and a probe DaemonSet runs there pinned to the gVisor node. The first canary
attempt FAILED exactly here — `violates PodSecurity baseline:latest: hostPath
volumes (volume thaw-state)` — before this substrate existed.

### Values

```yaml
gvisor:
  enabled: true
  namespace:
    name: "gardenio-gvisor"   # maps to controller GVISOR_NAMESPACE; "" disables the split
    create: true             # render the PSA=privileged Namespace; false to reference an existing one
    # Mirror EVERY shared Secret a runsc agent references. The controller's
    # verifySecretsExist gate defers pod creation until ALL referenced Secrets
    # exist in the agent's namespace (now the gVisor ns), so an incomplete set
    # wedges the canary in "deferring pod create — referenced secrets not yet
    # synced" forever. Derive the full list from a live agent pod:
    #   kubectl -n gardenio get pod <agent> -o json | jq '[.spec.containers[].env[]
    #     | select(.valueFrom.secretKeyRef) | .valueFrom.secretKeyRef.name] +
    #     [.spec.volumes[]?.projected.sources[]?.secret.name] | unique'
    # For a project=gardenio agent (perAgentTokens off) the set is: the Claude
    # OAuth secret, kd-beads-token (COOP + BEADS tokens — shared, not per-agent
    # minted), and the project's git/github/gitlab/rwx/mezmo secrets. SM keys are
    # the doubled `gardenio/gardenio-*` path (kd-beads-token is `gardenio/kd-beads-token`);
    # verify each with `kubectl -n gardenio get externalsecret <name> -o
    # jsonpath='{.spec.data[0].remoteRef.key}'`. dataFrom.extract pulls the JSON
    # properties, so git-credentials' username+token both flow.
    secretMirrors:
      - name: gardenio-claude-oauth-gardenio
        remoteRef: gardenio/gardenio-claude-oauth-gardenio
        refreshInterval: 15m
      - name: kd-beads-token
        remoteRef: gardenio/kd-beads-token
        refreshInterval: 15m
      - name: gardenio-git-credentials
        remoteRef: gardenio/gardenio-git-credentials
        refreshInterval: 15m
      - name: gardenio-github-token
        remoteRef: gardenio/gardenio-github-token
        refreshInterval: 15m
      - name: gardenio-gitlab-token
        remoteRef: gardenio/gardenio-gitlab-token
        refreshInterval: 15m
      - name: gardenio-rwx-access-token
        remoteRef: gardenio/gardenio-rwx-access-token
        refreshInterval: 15m
      - name: gardenio-mezmo-service-key
        remoteRef: gardenio/gardenio-mezmo-service-key
        refreshInterval: 15m
  probe:
    create: true             # render the gvisor-probe DaemonSet; false to reference an operator-managed probe
    image: "public.ecr.aws/docker/library/ubuntu:22.04"
    # nodeSelector/tolerations default to the gvisor=enabled pool
  freezeThaw:
    enabled: true
```

### What the chart renders (only when `gvisor.enabled` AND `gvisor.namespace.name`)

- A **Namespace** labelled `pod-security.kubernetes.io/{enforce,audit,warn}=privileged`
  (`helm.sh/resource-policy: keep`).
- The **agent ServiceAccount** (`gardenio.coop.serviceAccountName`, IRSA-annotated)
  in the gVisor namespace. A runsc agent pod sets `serviceAccountName` to the same
  name as in the main namespace, and K8s requires that SA to exist in the pod's own
  namespace — with `agents.needsK8sAPI` the pod automounts a SA token, so a missing
  SA wedges the pod. The `eks.amazonaws.com/role-arn` annotation is mirrored from
  `agents.agentServiceAccount.annotations`. **IRSA caveat:** S3 session archival
  (`agents.s3.bucket`) works from the gVisor namespace only if the IAM role's trust
  policy admits `system:serviceaccount:<gVisor-ns>:<sa>`. The role
  (`gardenio-agents-s3`) is operator-managed out-of-band (not in this repo's
  terraform); if its trust condition is scoped only to the main namespace, widen it
  before relying on S3 archival from runsc agents. Freeze/thaw itself does NOT use
  S3 (it checkpoints to the `/work` PVC via `runsc checkpoint`), so a
  namespace-scoped trust policy degrades archival but does not block the cycle.
- A **gvisor-probe DaemonSet** (`app=gvisor-probe`, privileged, hostPID, hostPath
  `/`), pinned to the pool via nodeSelector+toleration — one probe per gVisor node.
- A controller cross-namespace **Role + RoleBinding** (`pods`, `pods/exec`,
  `persistentvolumeclaims`, `secrets`) bound to the agents ServiceAccount, so the
  controller can create/delete runsc agent pods, exec the probe, and mint
  per-agent token Secrets + PVCs in that namespace.
- **ExternalSecret** mirrors for each `secretMirrors` entry.

### What the controller does (`GVISOR_NAMESPACE`)

When the env is set: `buildAgentPodSpec` routes `runtime_class: runsc` agents into
that namespace; the freeze/thaw `ProbeNodeExecutor` targets it; and the
status-reporter pod sweep includes it (`SetExtraNamespaces`) so the runsc agent's
bead still receives phase/`coop_url` updates. Non-runsc agents are untouched, and
an explicit event `namespace` still wins.

> **Follow-up (ka-0000008076):** the reconciler, stale-reaper, and PVC-GC are
> still scoped to the main namespace — a runsc agent in the gVisor namespace is
> not yet reconciled/reaped/GC'd. Safe for one operator-driven canary; required
> before any widening.

## Production-readiness gate: go/no-go before the FreezeThawEnabled flip (ka-0000002867)

Before you flip `gvisor.freezeThaw.enabled=true` for a real role (step 3 of the
rollout below), run the **readiness gate** — the single hard-fail signal that the
gVisor freeze/thaw lifecycle actually works end-to-end on a real substrate.

**Why the gate exists.** Every dedicated gVisor E2E module
(`test-gvisor-freeze-thaw`, `-armed-guard`, `-rehydration`, `-rehydrate-ack`,
`test-resume-latency`, `test-userresponse-latency`, `test-gb-stop-suspend`)
self-**SKIPs** (`exit 0`) when the `gvisor-poc` substrate is absent — and the
substrate is frequently torn down (Karpenter cold-start; the controller lacks
`gardenio-gvisor` RBAC at rest — see `fast-interruptible-freeze.md` §C3). That
default is deliberate: a torn-down pool must never red a release pipeline
(ka-0000000847 / ka-0000003662). But it means a normal green E2E run proves
**nothing** about freeze/thaw if the substrate happened to be down. A
production-readiness go/no-go cannot rest on tests that silently skipped.

**The `REQUIRE_SUBSTRATE` regime.** Every gVisor module reads a shared decision
(`gvisor_require_substrate_guard` in `tests/e2e/scripts/lib-gvisor-node.sh`):

| `REQUIRE_SUBSTRATE` | substrate absent | use |
|---|---|---|
| unset / `0` (default) | clean **SKIP** (`exit 0`) | per-feature dispatches, golden gate — a torn-down pool is tolerated |
| `1` (the gate) | **NO-GO hard fail** (`exit 1`) | the readiness go/no-go — an absent/cold substrate is itself a NO-GO |

**Running the gate.**

- **CI (operator):** click the manual `rwx-gvisor-readiness` GitLab job. It
  dispatches `gardenio-gvisor-readiness` (`.rwx/e2e.yml`), which runs the
  `gvisor-substrate-ready` preflight (provokes Karpenter to stand up a
  `gvisor=poc` node, then a warm-node gate) **first**, then every gVisor
  lifecycle module with `REQUIRE_SUBSTRATE=1`. The GitLab job status **is** the
  go/no-go verdict: green = GO, red = NO-GO (substrate could not stand up, or a
  module regressed). It is `needs: []` so a NO-GO blocks the *flip decision*, not
  unrelated prod releases.
- **CLI / ad hoc:** `rwx dispatch gardenio-gvisor-readiness --param commit-sha=<sha>`,
  or locally `GVISOR_E2E=1 REQUIRE_SUBSTRATE=1 ONLY=<module> run-shard.sh`.

A GO here means the substrate stood up and every lifecycle module asserted on it
— no silent skips. It is the precondition for step 3 below. (Flipping
`freezeThaw.enabled` for real agents remains a *separate* operator go/no-go per
the epic non-goals; the gate proves the capability, it does not authorize the
fleet-wide flip.)

## Gradual opt-in rollout plan (one role first)

This is an **opt-in** capability (epic non-goal: fleet-wide default-on). Roll it
out one role at a time, smallest blast radius first, with a verified rollback at
every step. The two flags compose, so the rollout has two independent dials:
`gvisor.enabled` (substrate exists) and `gvisor.freezeThaw.enabled` (controller
uses it). Bring them up in this order:

1. **Substrate only — no agents on runsc.** Ship `gvisor.enabled=true`,
   `gvisor.freezeThaw.enabled=false`, set `gvisor.namespace.name` so the
   PSA-privileged namespace + probe DaemonSet exist (see "The gVisor substrate
   namespace" above), and have no role carrying `runtime_class: runsc`.
   This creates the RuntimeClass and references the gVisor pool but routes
   nothing onto it. Confirm a plain `helm upgrade` changes no running agent
   (blast radius is zero — see below). This is the safe landing zone.

2. **One role onto runsc, still cold-start.** Pick a single low-stakes,
   non-prod-blast-radius role (the epic's success criterion 4 slice — e.g. a
   throwaway demo/echo role, **not** gardener or a release-owner role) and set
   `runtime_class: runsc` on that one role bead. Leave `freezeThaw.enabled=false`.
   Agents in that role now schedule on the gVisor pool and run sandboxed, but
   still use today's create/delete lifecycle. Verify they spawn, do work, and
   despawn normally on runsc before touching freeze/thaw.

3. **Enable freeze/thaw for that one role.** First run the readiness gate
   (`rwx-gvisor-readiness`, see "Production-readiness gate" above) and confirm a
   GO — that proves the freeze/thaw lifecycle works end-to-end on a real
   substrate with no silent skips. Then flip `gvisor.freezeThaw.enabled=true`.
   Only the runsc-pinned role from step 2 is now frozen on graceful stop and
   thawed on next spawn; every other agent is untouched (non-runsc agents never
   enter the path). Watch one full freeze→thaw cycle end-to-end: graceful stop →
   snapshot persisted → next spawn restores through Pod B with `/work` state
   intact. The E2E rehydration suite (success criterion 3) gates this step.
   The rehydration hand-off (ka-0000001515 / ka-0000004017) is now wired, so the
   thawed agent re-mints credentials and reconnects NATS before resuming — but
   confirm on this role that the gb+coop acks land within `THAW_REHYDRATE_DEADLINE`
   (a fail-closed thaw cleanly cold-starts; see the rehydration section above).

4. **Widen one role at a time.** Only after a role is stable across several
   freeze→thaw cycles, repeat steps 2–3 for the next role. There is no
   "enable for everyone" switch by design — each role is an explicit
   `runtime_class: runsc` opt-in. A separate operator go/no-go decision gates any
   move toward broader adoption.

At every step the rollback below is available and lossless; never advance a step
until you have confirmed you can fall back from it.

## Rollback

**No data is lost on rollback.** Every fallback path lands an agent on today's
cold-start lifecycle — the worst case is a cold start, never a wedged or
corrupted agent. Agent durable state lives on the `/work` PVC and in beads, not
in the frozen snapshot: a snapshot that is never thawed (because the flag flipped
off) is simply ignored, and the agent cold-starts against the same PVC and bead
state it always would. There is no migration, no irreversible cutover, and no
state that exists only inside a checkpoint.

There are three rollback scopes, narrowest first:

1. **Roll back one role** (preferred): remove `runtime_class: runsc` from that
   role bead. Its agents immediately revert to the default runtime + cold-start
   lifecycle on the next spawn; all other roles and the substrate are untouched.
   This is the per-role kill switch and needs no `helm upgrade`.

2. **Roll back the lifecycle fleet-wide, keep the substrate**: set
   `gvisor.freezeThaw.enabled=false` and `helm upgrade`. The controller drops
   `FREEZE_THAW_ENABLED`; the manager is never constructed and **every** agent
   reverts to the cold-start create/delete lifecycle, while the runsc
   RuntimeClass and pool stay in place (runsc agents still run sandboxed, just
   cold-started). This is the narrower, safer rollback for the freeze/thaw
   feature alone (epic success criterion 5).

3. **Roll back the whole substrate**: set `gvisor.enabled=false` and
   `helm upgrade`. The RuntimeClass (and any chart-managed node pool) is removed;
   agents fall back to the cluster default runtime's cold-start lifecycle.
   **Keep any role/agent `runtime_class: runsc` opt-in in sync** — a runsc pod
   cannot schedule once the RuntimeClass is gone, so remove the opt-in (scope 1)
   before (or together with) disabling the flag.

The RuntimeClass carries `helm.sh/resource-policy: keep` so a stray uninstall or
a single release flipping the flag off does not yank the cluster-scoped object
out from under other releases/pods still referencing it. To truly remove it,
delete it explicitly after confirming no `runtimeClassName: runsc` pods remain.

## Blast radius

Zero by default. The node pool is tainted `gvisor=enabled:NoSchedule`, so only
pods that tolerate it (i.e. those routed through the `runsc` RuntimeClass) ever
land there — the general node pool is untouched. The flag defaults OFF, so a
plain `helm upgrade` changes nothing until an operator explicitly opts in.

## Cold-pull spawn latency: prewarm the image

A cold runsc agent spawn is ~4–5 min (vs ~135 s runc), dominated by the first
agent-image pull into a freshly-scaled gVisor node. To collapse that pull cost,
enable the per-pool **agent-image prewarm DaemonSet**
(`agents.prewarmImage.enabled`, default OFF) — it caches the agent image on every
gVisor (and standard agent) pool node before an agent is scheduled there. See
[`agent-image-prewarm.md`](agent-image-prewarm.md).

## Keeping a warm gVisor node always Ready (WS1, ka-0000000239)

The warm-B placeholder pool (`gvisor.warmPlaceholderPool`) has two fill modes
that together keep both *concurrent-thaw headroom* and *a warm node floor*:

- **Per-topology snapshot fill** (`minSize`): for each persisted snapshot's
  `(node, topology)` the pool keeps `minSize` Ready placeholders built from the
  snapshot template, so a thaw grafts onto a mount-compatible, repo-warm pod
  (~321 ms R4 vs ~2.4 s R3). These are node-pinned (a checkpoint image is
  node-local) and inherit the snapshot's full agent resource envelope.
- **Per-node warm floor** (`nodeFloor`): a small number of cheap legacy (blind
  bare-`/work`, 100m) placeholders on **every** gVisor node, *unioned* with the
  snapshot buckets. Without it, the per-topology fill only warms nodes that
  already hold a snapshot — a gVisor node with no snapshot drops to zero warm
  pods, trips the `gvisor-poc` NodePool's `WhenEmpty` consolidation, and is
  reclaimed. The next thaw then pays cold Karpenter node provisioning (the >10 s
  resume path). A `nodeFloor` of 1 keeps every node non-empty so Karpenter never
  scales the warm substrate to zero. Rollback: `nodeFloor: 0`.

> The `gvisor-poc` NodePool is operator-applied (`nodePool.create=false`) with
> `consolidationPolicy: WhenEmpty` + `consolidateAfter: 10m` and no min-nodes
> floor, so the only in-repo lever to keep a node alive is to keep it non-empty.
> `nodeFloor` is that lever — a non-DS warm pod blocks `WhenEmpty` consolidation.

### Placeholder leak / OutOfcpu storm guard

Two reconciler defenses prevent a structurally-unfittable bucket from leaking
dead pods (the ka-0000000239 finding: 399 `OutOfcpu`-rejected placeholders piled
on one node):

- **Terminal reap**: `Failed`/`Succeeded` placeholders (e.g. a kubelet `OutOfcpu`
  rejection of a node-pinned full-envelope placeholder that cannot fit alongside
  the live agent on its node) are now *deleted*, not merely uncounted. Previously
  they lingered forever and left the bucket permanently below target.
- **Admission-failure backoff**: when every placeholder a bucket mints is
  immediately rejected, minting pauses on an exponential cooldown (1m, 2m, 4m …
  capped at 30m) instead of re-storming a doomed batch every 30 s reconcile. The
  backoff clears the instant the bucket reaches its Ready target (e.g. the live
  agent exits and frees node CPU).

## WS3: widening freeze/thaw-on-interrupt to the plan-gardener + thread roles (ka-0000000138)

Epic **ka-0000004429** ("stop agent pods being interrupted mid-turn"). WS0–WS2/WS4
made every interruption *graceful and rare*; **WS3 is the only path that makes an
interrupted turn *invisible***, by widening the canary-proven freeze/thaw lifecycle
(p50 ~321 ms / p95 ~366 ms warm restore, ka-0000000069) from the single `gvisor-poc`
canary to the roles that actually get interrupted mid-turn — **plan-gardener** and
**thread**. Today those roles cold-recycle on every interruption; freeze/thaw makes
an interrupted turn resume sub-second and invisible.

### The problem this section's flag solves: freeze-eligibility has no per-role dial

Freeze-eligibility is otherwise a **global** predicate — `FreezeThawEnabled` AND the
agent pinned to `runtime_class: runsc` (`isFreezeEligible` in
`cmd/controller/podspec.go`, re-checked live by `shouldFreeze` in
`cmd/controller/freezethaw_lifecycle.go`). **Prod already runs
`freezeThaw.enabled=true`** (for the `gvisor-poc` canary). So the instant a second,
already-populated role — `plan-gardener` or `thread` — is opted onto runsc, **every
instance of it would start freezing at once**. There is no "route this role onto
runsc but keep it cold-start first" step (rollout step 2 above is impossible for a
new role once the lifecycle flag is globally on). That is exactly the
widen-one-role-at-a-time footgun this workstream must avoid.

### The dial: `gvisor.freezeThaw.roleAllowlist` (env `FREEZE_THAW_ROLE_ALLOWLIST`)

A controller feature flag that narrows freeze-eligibility to a **canary set of roles**:

```yaml
gvisor:
  freezeThaw:
    enabled: true
    # [] (default): NO role filter — any runsc agent freezes (pre-WS3 behaviour).
    # Non-empty: freezing is narrowed to ONLY these roles; a runsc agent whose
    # role is absent schedules on the gVisor pool but is never checkpointed.
    roleAllowlist: ["gvisor-poc", "plan-gardener"]
```

- **Empty (default)** = no filter = the pre-WS3 behaviour verbatim. This is why
  merging the flag is a **zero-blast-radius no-op**: nothing changes until an
  operator sets a non-empty list.
- **Non-empty** narrows freezing to exactly the listed roles *even if other runsc
  roles exist*. Applied at **both** freeze seams — the spawn-time
  `BOAT_FREEZE_ELIGIBLE` signal that `gb stop` reads, and the controller's own
  `shouldFreeze` gate (via the pod's `gardenio.io/role` label) — so the suspend
  decision and the actual checkpoint never drift. Comma/dash sanitization is
  tolerated so an entry authored either way (`crew,thread` or `crew-thread`) matches
  at both seams.
- **Rollback** = shrink or clear the list (an env change via `helm upgrade`, no code
  redeploy); the removed role reverts to cold-start on its next graceful stop.
- **Prod setting (since 2026-07-19, bug ka-0000002174)**:
  `roleAllowlist: ["gvisor-poc", "thread", "plan-assistant"]` in the prod overlay
  (`terraform/composition/canonical-prod/gvisor-values.yaml`). `plan-gardener` is
  deliberately **excluded**: its trigger-engine gardener loops mint a fresh agent
  name per run, so their snapshots could never be thawed (`HasSnapshot(name)`
  never matched — pure single-shot checkpoint waste, ~50 freezes/day × ~3 GB).
  The overlay comment carries the full rationale and the known trade-off (the
  name-stable `/plans` viewer assistants also run under `plan-gardener` and lose
  the sub-second thaw; they cold-respawn with `coop --resume`, keeping the
  conversation).

### Secrets: mirror the gardener/thread credential set into the gVisor namespace

A runsc agent runs in the PSA-privileged `gardenio-gvisor` namespace and can only
reference Secrets that exist **there** — the controller's `verifySecretsExist` gate
defers pod creation until every referenced Secret is present, so an incomplete mirror
set wedges the agent forever ("referenced secrets not yet synced"). `plan-gardener`
and `thread` agents are project=gardenio agents and reference the **same** shared
secret set as any other gardenio agent (they carry no extra per-role secrets — verify
with the `kubectl … jq` recipe in the "gVisor substrate namespace" section against a
live gardener/thread pod). Reuse the canary mirror list already in the deploy overlay:

```yaml
gvisor:
  namespace:
    name: "gardenio-gvisor"
    secretMirrors:
      - {name: gardenio-claude-oauth-gardenio, remoteRef: gardenio/gardenio-claude-oauth-gardenio, refreshInterval: 15m}
      - {name: kd-beads-token,               remoteRef: gardenio/kd-beads-token,               refreshInterval: 15m}  # plain-string → add secretKey: token
      - {name: gardenio-git-credentials,      remoteRef: gardenio/gardenio-git-credentials,      refreshInterval: 15m}
      - {name: gardenio-github-token,         remoteRef: gardenio/gardenio-github-token,         refreshInterval: 15m}
      - {name: gardenio-gitlab-token,         remoteRef: gardenio/gardenio-gitlab-token,         refreshInterval: 15m}
      - {name: gardenio-rwx-access-token,     remoteRef: gardenio/gardenio-rwx-access-token,     refreshInterval: 15m}
      - {name: gardenio-mezmo-service-key,    remoteRef: gardenio/gardenio-mezmo-service-key,    refreshInterval: 15m}
```

**Account caveat (learned on the canary, ka-0000003266):** the OAuth secret that
must be mirrored is the one for the **account the role actually uses**. `plan-gardener`
runs `model: claude-opus-4-8-oauth`; confirm which account bead backs it and mirror
*that* account's OAuth secret (a mismatch fails the credential guard closed and the
pod never schedules — the exact failure the canary hit when its account was repointed
to one whose OAuth secret was absent from `gardenio-gvisor`).

### Warm-pool sizing for gardener/thread concurrency

`gvisor.warmPlaceholderPool.minSize` is **per (gVisor node, snapshot topology)** — a
checkpoint image is node-local, so a warm placeholder only serves a thaw whose
snapshot lives on the same node. Size it for the number of *concurrent* frozen agents
of the canary role you expect to thaw at once, not the total. For a single-gardener
canary, `minSize: 1–2` is enough; a busy thread channel that runs several thread
agents concurrently wants `minSize` ≈ peak concurrent threads. Keep `nodeFloor ≥ 1`
so a gVisor node with no snapshot stays non-empty and Karpenter never reclaims the
warm substrate out from under the next thaw (see "Keeping a warm gVisor node always
Ready"). gVisor pods are budgeted separately from the per-project cap via
`agents.coopMaxPodsGVisor` (env `COOP_MAX_PODS_GVISOR`) — set it positive so a runsc
warm sub-pool + live gardener/thread agents can schedule without starving against the
saturated gardenio per-project cap (ka-0000004152).

### Rollout — one role at a time, each behind the operator go-step

Prereq: the substrate is already up in prod (`gvisor.enabled=true`,
`freezeThaw.enabled=true`, `gardenio-gvisor` namespace + probe + canary mirrors). This
MR ships the `roleAllowlist` dial **off (empty)** — so the code is merge-safe and
per-role enablement is a deliberate operator go-step, not a merge side effect.

1. **Pin `roleAllowlist` to the proven canary only.** Set
   `roleAllowlist: ["gvisor-poc"]` and `helm upgrade`. This changes *nothing*
   behaviourally (gvisor-poc already freezes) but proves the flag is wired and gives
   you a known-good baseline to widen from.
2. **Opt the first real role onto runsc, still cold-start.** Set
   `runtime_class: runsc` on the **plan-gardener** role bead (`ka-0000003712`) — but do
   **not** add it to `roleAllowlist` yet. Gardeners now schedule on the gVisor pool and
   run sandboxed, cold-starting as today. Confirm a gardener spawns in `gardenio-gvisor`,
   does work, and despawns normally on runsc before enabling freezing. (Mirror the
   gardener's account OAuth secret first — see the account caveat above.)
3. **Enable freezing for that one role.** Run the readiness gate
   (`rwx-gvisor-readiness`, GO/NO-GO) then add the role:
   `roleAllowlist: ["gvisor-poc", "plan-gardener"]` and `helm upgrade`. Only gardeners
   now freeze on graceful stop and thaw on next spawn; threads and every other runsc
   role stay cold. Watch one full interrupt→freeze→thaw cycle: the interrupted turn
   resumes sub-second with `/work` + conversation intact, gb+coop acks land within
   `THAW_REHYDRATE_DEADLINE`, and a fail-closed thaw cleanly cold-starts. WS4's
   `AgentNodeReclaimDeaths` sentinel + resume-latency dashboard (merged via !5585)
   make any regression visible.
4. **Widen to `thread`.** Only after plan-gardener is stable across several cycles,
   repeat steps 2–3 for the thread role: pin threads onto runsc (a persistent role
   bead, or per-channel via `channel_runtime_class` on the project bead to scope it to
   one Slack channel first), then append `"thread"` to `roleAllowlist`. Size
   `warmPlaceholderPool.minSize` for peak concurrent threads before flipping.

**Rollback at every step is the narrowest of:** drop the role from `roleAllowlist`
(env change, reverts that role to cold-start on next stop, no other role touched) →
remove `runtime_class: runsc` from the role bead (per-role kill switch, no
`helm upgrade`) → `freezeThaw.enabled=false` (fleet-wide, keeps substrate) →
`gvisor.enabled=false` (whole substrate). All are lossless — durable state is the
`/work` PVC + beads, never the snapshot.
