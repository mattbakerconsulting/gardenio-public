# Sub-second agent resume: prewarmed pods × gVisor thaw

Task ka-0000006446 · epic ka-0000002590 (Phase 5) · operator: Matt Baker.
Plan-of-record for combining the warm-pool and freeze/thaw components into a
single **<1s agent resume** path, plus the benchmark that proves it.

## TL;DR

We have three latency mechanisms, each already built but wired independently:

1. **Node-image prewarm DaemonSet** (ka-0000000861) — eliminates the cold
   image-pull penalty (~4–5 min on a freshly-scaled runsc node).
2. **Warm agent-POD pool** (`poolmanager.Manager`) — Claude is already booted and
   idle in standby; wake = bead flip + coop nudge (~60–190 ms).
3. **gVisor freeze/thaw** (Phase 4, `podmanager.FreezeThawManager`) — restores a
   checkpointed agent's full RAM/FD/cwd/FS state.

The PoC proved raw `runsc restore` is **sub-300 ms** (report.md §2). But the
**productionized** `Thaw` is NOT sub-second: its dominant cost is **creating
placeholder Pod B and waiting for it to reach Ready** (`waitPodReady`, default
180 s budget — in practice seconds while kubelet provisions the CNI netns,
emptyDir volumes, and the projected SA token). The page-restore is cheap; the
*kubelet pod-admission* is what costs seconds.

**The combine:** keep a small pool of **pre-Ready runsc placeholder pods** and
have `Thaw` graft the checkpoint onto a warm placeholder, instead of
`Create + waitReady` on the hot path. That removes the only multi-second step,
landing total resume at restore-cost (~300 ms) + rehydrate. Node-image prewarm
is the prerequisite that lets the placeholder pool refill quickly after a scale-up.

## Where the seconds actually go today

`FreezeThawManager.Thaw` (freezethaw.go) does, in order:

| Step | What | Cost |
|---|---|---|
| 0 | `waitPodGone` — Pod A teardown barrier | 0–seconds (only when a fast re-spawn races teardown) |
| 1 | **Create placeholder Pod B + `waitPodReady`** | **seconds (kubelet admit + netns/volume/token provision)** ← bottleneck |
| 2–5 | repoint cids/uid/netns + count-gated `runsc restore` on the node | ~50–300 ms (PoC-measured) |
| 6 | rehydration hand-off (creds/NATS), bounded by `RehydrateDeadline` | ~ms–60 s deadline; warm-boot is fast |
| 7 | reap consumed checkpoint image | best-effort, off hot path |

Step 1 is the entire gap between "sub-300 ms capability" and "sub-second
product." Everything else is already fast or off the critical path.

## Design: the placeholder-pod pool ("warm B pool")

Generalize the existing per-(project,role) prewarmed pool so it can hold
**runsc placeholder pods** — pods that are already Ready (kubelet has provisioned
the netns/volume/token) and parked on a sleep placeholder, exactly the shape
`buildPlaceholderPodB` produces today, but created *ahead of time* rather than on
the thaw hot path.

```
                 ┌─────────────────────────────────────────┐
   refill loop   │  warm-B pool (per node, runsc, Ready)     │
  (poolmanager)  │   pod-b-1 [sleep]  pod-b-2 [sleep] ...    │
                 └───────────────┬─────────────────────────┘
                                 │  claim oldest Ready B on snapshot.Node
   spawn(runsc, has-snapshot) ───┤
                                 ▼
                   Thaw.graftOnto(podB):  repoint + runsc restore  (~300ms)
                                 ▼
                   rehydrate (creds/NATS, session_resume=false)    (~ms)
                                 ▼
                   live agent  ── total ~sub-second
```

Key properties:

- **Node affinity is load-bearing.** A snapshot's checkpoint image is node-local
  (`Snapshot.Node`, `/var/lib/gardenio/ckpt/<agent>`). A warm B can only serve a
  thaw whose snapshot lives on the *same node*. So the pool is keyed by
  `(project, role, node)` — or, more practically, the pool keeps N Ready
  placeholders per gVisor node and the thaw claims one co-resident with the
  snapshot. Cross-node thaw needs snapshot transport (out of scope; see Phase 4
  "remaining work").
- **`session_resume=false` is fine and desired** (per the epic). We are NOT
  restoring Claude's conversation; the checkpoint restores the *process tree +
  creds rehydrate*. For a FRESH agent the snapshot is a generic
  "booted-Claude-on-runsc" template, not a specific prior session.
- **A generic warm B is also usable without any snapshot** — i.e. a Ready runsc
  placeholder onto which we cold-launch the agent command. That already beats
  cold start (no kubelet admit wait), and is the fallback when no snapshot
  exists. This unifies "warm pool" and "thaw target" into one pool.

## Integration changes (smallest viable seams)

### C1 — `Thaw` accepts a pre-supplied placeholder pod
Add `ThawRequest.PlaceholderPod string` (an existing Ready Pod B name). When set,
`Thaw` skips Step 1's Create + `waitPodReady` and proceeds straight to the
repoint + barrier restore against that pod. When empty, today's behavior (create
+ wait) is preserved — pure additive change, zero blast radius when unused.

### C2 — `freezeThawLifecycle.thaw` claims from the warm-B pool
`shouldThaw` already gates on (runsc ∧ snapshot-exists). Extend the wake path in
main.go (~L988) to first try `claimWarmPlaceholder(snapshot.Node)`; on hit, call
`Thaw` with `PlaceholderPod` set; on miss, fall back to today's
create-then-restore (still correct, just not sub-second).

### C3 — poolmanager grows a runsc-placeholder pool type
Reuse `PrewarmedRolePool` with a new `Runtime: "runsc"` (or `placeholder: true`)
field. The reconciler creates Ready placeholder pods (sleep shell, runsc
RuntimeClass, restricted mount shape) per gVisor node up to MinSize, and refills
after each claim. This is the same loop as the existing pool, with the pod shape
swapped to `buildPlaceholderPodB`'s template and a per-node target.

### C4 — role/runtime-aware fast path (Phase 5 headline gap ka-0000002590)
`tryPoolAssign` (bot_mentions.go:996) must pass `role` (resolved from
`channel_roles`) into `AssignRequest` BEFORE the pool lookup, so a gvisor-poc
channel gets a runsc warm pod, not a generic one. `AssignPrewarmed` already
filters by role (poolmanager.go:322) — the gap is purely that the caller never
sends it. One-line-ish plumb + a fall-through to cold start on pool miss.

### C5 — config flags
Reuse `FreezeThawEnabled` + `ThawRehydrateDeadline`. Add
`WARM_PLACEHOLDER_POOL_ENABLED` (default off) and a per-node MinSize. Default-off
keeps blast radius zero; flip-on is the rollout, flip-off the rollback.

## Benchmark plan

Goal: a reproducible measurement of **spawn-trigger → agent-responsive** across
the four regimes, isolating where each mechanism buys its time.

### Regimes (cumulative)
| # | Regime | Expected dominant cost |
|---|---|---|
| R0 | cold runsc spawn, cold node | image pull (~4–5 min) |
| R1 | + node-image prewarm | kubelet admit + Claude boot (~60–120 s) |
| R2 | + warm agent-pod pool (generic) | bead flip + coop nudge (~60–190 ms) — but NOT runtime-aware today |
| R3 | + thaw onto **fresh-created** Pod B | Pod B Ready wait (seconds) + restore (~300 ms) |
| R4 | + thaw onto **warm placeholder** Pod B (this plan) | restore (~300 ms) + rehydrate → **target <1s** |

### Metric & instrumentation
Define **resume latency** = `t(agent emits first NATS presence heartbeat or
coop-ready) − t(spawn event accepted by controller)`. Both endpoints are already
observable:
- start: controller logs "spawn event" / `RecordLifecycleEvent(LifecycleSpawned)`.
- end: the thaw rehydration fires one immediate heartbeat (agent_start_thaw.go
  N6) — timestamp that, or the bead `agent_state→working` transition.

Emit a structured `resume_latency_ms` metric (Prometheus histogram, labels
`{regime, role, runtime, path=cold|pool|thaw-fresh|thaw-warm}`) from the
controller wake path. A benchmark harness script triggers N spawns per regime,
scrapes the histogram, reports p50/p95/p99.

### Harness
`bench-resume.sh` (new; PoC harness, now only in the archive tag `archive/clutter-pre-delete-2026-10-28`, see
[`gvisor-runsc-cr/README.md`](gvisor-runsc-cr/README.md)): drives the gVisor pool,
fires synthetic spawn events (or real @-mentions in a bench channel), records the
two timestamps per run, and tabulates per-regime p50/p95/p99. Run on the
gvisor-poc substrate (the same node pool the Phase 4 PoC used). 20+ runs/regime
for stable tails.

### Success criteria
- R4 p50 **< 1 s**, p95 **< 1.5 s**, for a runsc agent wake.
- R4 strictly faster than R3 (proves the placeholder-pool removes the Pod B Ready
  wait).
- Node-image prewarm verified to keep pool refill < (assignment interval) so the
  pool does not starve under sustained load.

## A new gardener (per Matt's note)

Spawn a **Phase 5 gardener** (type=trigger, role=gardener) to drive this epic to
green — sibling to the existing Phase 4 gardener (ka-0000007409). It owns:
- decomposing this plan into child tasks under ka-0000002590,
- spawning crew on the C1–C5 changes + the benchmark harness,
- gating on the R4<1s success criterion before declaring done.

Gardener self-block (per advice ka-0000000974): `run_budget` + a progress-aware
`loop_end` keyed on the epic's open-child count reaching zero AND the benchmark
bead reporting R4<1s. Per advice ka-0000003852, a `gb decision` operator
checkpoint precedes actually spawning the gardener.

## Open questions / risks
- **Per-node pool accounting.** The existing pool keys on (project,role); adding
  `node` means the reconciler must know which gVisor nodes exist and target each.
  Simplest: target the gVisor nodeSelector set, MinSize placeholders per node.
- **Warm-B staleness.** A placeholder pod holds a projected SA token; long-lived
  placeholders must tolerate token rotation. The rehydration path already
  re-mints creds on thaw (C3 NodeRehydrationTrigger), so a stale placeholder
  token is repaired at graft time — but verify the placeholder itself stays
  Ready across rotation.
- **Snapshot ↔ placeholder mount-shape match.** `buildPlaceholderPodB` derives
  the mount shape from the snapshot's `PodTemplate`. A pre-created generic
  placeholder must match the snapshot's mount destinations or the restore's
  RestoreValidateSpec fails. Either (a) make placeholders from a canonical
  template all runsc agents share, or (b) accept that placeholders only serve
  snapshots whose template matches. Needs a spike.
- **Eviction/cost.** Warm placeholders reserve node capacity. Keep MinSize small
  (1–2/node) and lean on node-image prewarm for fast cold-node refill instead of
  large standing pools.

## The standing <1s regression gate (C7 — ka-0000006324)

The overnight convergence benchmark (ka-0000003577, `bench-resume.sh`) **proves
the number once**. The standing gate **keeps it <1s forever**. Per the operator
mandate (Matt Baker, thread 1780804147.560279), the sub-second resume result is
guarded by a permanent timed e2e module that runs on every gVisor e2e dispatch
and FAILS the suite on regression.

**Module:** `gardenio/tests/e2e/scripts/test-resume-latency.sh`
(wired into `run-shard.sh` as the `resume-latency` module; dedicated RWX dispatch
`gardenio-resume-latency` in `.rwx/e2e.yml`).

### The SLO it enforces

Over **N≥5** in-test resumes (default `N_RESUMES=5`), all three must hold or the
module exits non-zero (red suite):

| Gate | Assertion | Why |
|---|---|---|
| **G1 — sub-second** | `p50(R4 warm-B) < GATE_P50_MS` (default **1000ms**) | the headline <1s goal |
| **G2 — pool wins** | `p50(R4 warm-B)` **strictly <** `p50(R3 fresh-B)` | proves the warm pool removed the Pod-B Ready wait, not just a fast node |
| **G3 — repo present** | (when `REQUIRE_REPO=1`) every warm-B resume reports `repo_present=1` | C6 (ka-0000007582): a sub-1s number on an empty workspace does not count |

`resume_latency` is measured exactly as the benchmark defines it:
`t(agent responsive) − t(wake request)`. The **R4 warm-B** number is the
`runsc restore` (THAW_MS) into an *already-Ready* placeholder Pod B — the kubelet
admit happened off the hot path when the pool was pre-warmed. The **R3 fresh-B**
control number is the full `create → waitReady → restore` wall-clock, so it
includes the multi-second pod-admission cost the warm-B pool exists to remove.

The module reuses the **proven node scripts** the benchmark already validated —
`single-container-resume-node.sh` (builds the ROOT-agent bundle + checkpoint, fix
path (c), no mounts-fd handoff) and `single-container-thaw-into-b-node.sh`
(restore into B's kubelet-owned netns) — so the gate tracks the same code path
the number was proven against.

### Timed emission (feeds the histogram + timing report)

- One line per resume to the task log:
  `resume_latency_ms regime=R4 path=thaw-warm run=<i> value=<ms> repo_present=<0|1>`
  (and `regime=R3 path=thaw-fresh` for the control). The e2e flake/timing report
  ingests these.
- A Prometheus textfile (`$PROM_TEXTFILE`, default under `$PROM_TEXTFILE_DIR`) in
  the **same** `gardenio_resume_latency_seconds` histogram name + buckets + labels
  (`{regime,role,runtime,path,env}`) as
  `controller/internal/metrics/resume_latency.go` and `bench-resume.sh`, so the
  ka-0000003577 dashboard ingests standing-CI samples (`env="e2e"`) alongside
  harness (`env="bench"`) and production samples.

### Opt-in + never-silently-pass gating

The module targets the dedicated **gvisor-poc** namespace + runsc node pool, not
the per-instance gardenio-rwx namespace. It is therefore opt-in, exactly like the
sibling `gvisor-freeze-thaw` / `gvisor-rehydration` modules:

- **opt-in flag unset** (`GVISOR_E2E` / `RESUME_LATENCY_E2E` ≠ 1) → SKIP quietly
  (exit 0). Inert in the standard gardenio-rwx shards (no gVisor there).
- **opt-in flag set but substrate absent** (no runsc RuntimeClass / no
  create-pods in gvisor-poc) → **SKIP LOUD** with a red banner, still exit 0 so an
  absent node pool does not red the suite — but it must **never look like it
  asserted when it did not** (scope item 3 of ka-0000006324).
- **opt-in flag set + substrate present** → assert all three gates; a real
  regression or substrate fault blocks promote/deploy-prod.

### How to run it

```sh
# Dedicated RWX dispatch (the standing CI path):
rwx dispatch gardenio-resume-latency --param commit-sha=<sha>

# Locally / ad-hoc against the gvisor-poc substrate:
GVISOR_E2E=1 ./gardenio/tests/e2e/scripts/test-resume-latency.sh

# Tighter tails / debug (keep the pods for inspection):
GVISOR_E2E=1 N_RESUMES=8 GATE_P50_MS=1000 KEEP_PODS=1 \
  ./gardenio/tests/e2e/scripts/test-resume-latency.sh

# Post-C6 (repo on the placeholder): also gate repo-present.
GVISOR_E2E=1 REQUIRE_REPO=1 ./gardenio/tests/e2e/scripts/test-resume-latency.sh
```

Key env knobs: `N_RESUMES` (resumes per path, default 5), `GATE_P50_MS` (SLO,
default 1000), `REQUIRE_REPO` (C6 gate, default 0 until ka-0000007582 lands),
`GVISOR_NS` (default gvisor-poc), `POD_READY_TIMEOUT` (default 180s),
`PROM_TEXTFILE` / `PROM_TEXTFILE_DIR` (histogram output), `KEEP_PODS` (debug).

## Channel-scoped opt-in: `channel_runtime_class` (gb-turbo)

Freeze/thaw is gated to `runtime_class=runsc` agents. To opt a **single Slack
channel's thread agents** onto runsc — and only that channel — set a
`channel_runtime_class` map on the project bead, mirroring `channel_roles` /
`channel_modes`:

```sh
# Pin thread agents born in the gb-turbo channel (C0XXXXXXX) to runsc.
kd update <project-bead-id> --field channel_runtime_class='{"C0XXXXXXX":"runsc"}'
```

Operator: Matt Baker (Slack thread 1781385375-987669, *"agents responding to
threads in gb-turbo channel use runsc but limit it to that for now"*). Epic
ka-0000003080.

**What it does** (`beadsapi.ProjectInfo.ChannelRuntimeClass`,
`bridge.runtimeClassFromChannel`):

- `handleThreadSpawn` (first @mention) and `respawnThreadAgent` (every follow-up)
  resolve the channel's RuntimeClass override. When set, they **skip the runc
  warm-prewarmed pool** — a pooled pod is runc, and grafting onto it would
  forfeit freeze/thaw — and stamp `runtime_class=<value>` on the cold-mint agent
  bead fields.
- `podspec.go routeGVisorNamespace` then routes the runsc pod into the gVisor
  namespace (`GVISOR_NAMESPACE`) with the release-namespace DNS search fixup,
  the same proven canary path.

**Rollback:** remove the channel from the map (or clear the field). New agents in
that channel revert to the default runc warm-pool path immediately — no code
redeploy. Channels without the override are entirely unaffected.

## `gb stop` suspends freeze-eligible agents (ka-0000005754)

Pinning to runsc makes an agent freeze-*eligible*; this is the change that makes
`gb stop` actually *freeze* one instead of terminally closing it. Freeze fires
only on a transition to `agent_state=stopping` → `AgentStop` (`subscriber/sse.go`);
`AgentDone` (bead closed to `done`) is deliberately NOT checkpointed ("a snapshot
of a finished/failed agent has no wake to serve", `main.go`). So `gb stop` now
forks:

- **Freeze-eligible → SUSPEND.** `gb stop` sets `agent_state=stopping` and does
  **not** close the bead, set `stop_requested`, or shut coop down. The stopping
  transition is the sole `AgentStop` trigger; the controller runs `runsc
  checkpoint --leave-running` on the live sandbox, persists a thaw-able snapshot,
  and tears down Pod A. The bead stays open in a frozen-awaiting-thaw state a
  later `AgentSpawn` thaws from (the reconciler's snapshot exemptions already
  keep it from being cold-restarted or leaked-closed). coop stays live through
  the checkpoint so the restored process is a true hot resume.
- **Not eligible (runc), or `--done` / `--clean` → terminal DONE.** Unchanged:
  `stop_requested=true` + `CloseBead(agent_state=done)` + coop shutdown →
  `AgentDone` → no freeze. `--done` forces discard for a freeze-eligible agent;
  `--clean` (fresh next-spawn conversation) is incompatible with thawing the
  frozen process, so it also forces done.

**The eligibility signal.** The controller is the source of truth. At spawn,
`podspec.go markFreezeEligible` injects `BOAT_FREEZE_ELIGIBLE=true` into the pod
env iff `FreezeThawEnabled` AND the spec resolved to `runtime_class=runsc` —
mirroring the controller's own `shouldFreeze` gate, and wired into **both** spec
builders (`buildAgentPodSpec` events path + `BuildSpecFromBeadInfo` reconciler
path) so they never drift. `gb stop` reads that env var (`freezeEligible()`).
Because the controller's `shouldFreeze` independently re-checks the LIVE pod's
RuntimeClass before checkpointing, a stale or missing var only ever costs a cold
start — never a bad freeze.

**Why no `stop_requested` on suspend.** `stop_requested` drives the entrypoint
restart loop to `CloseBead(agent_state=done)` (`agent_start_k8s.go`), which would
overwrite the `stopping` state and defeat the freeze. On the suspend path the
controller's Pod A teardown — after the `--leave-running` checkpoint — ends the
pod's life, so the entrypoint close-to-done path is never reached.

## Warm-pool coop-resume graft (ka-0000000983)

The freeze/thaw path above is the *true* "sub-second **and** real transcript"
answer — CRIU restores the live process with the conversation in RAM. But it is
gated behind the runsc HARD-HOLD (ka-0000001283): widening runsc past the
gvisor-poc canary needs a fresh operator go/no-go, so ordinary Slack threads on
the runc warm pool cannot use it yet.

This graft closes the gap for the **runc warm pool** without touching freeze/thaw.
The problem it solves: a follow-up reply served warm (`respawnThreadAgent` →
`tryPoolAssign`) grabs a *differently-named* pool pod that already booted coop
under its **own empty** S3 prefix — so the operator's follow-up gets a fresh
Claude session with **zero prior transcript**, only the Slack thread text
re-injected by `gb prime`. coop restores a transcript **only at process launch**,
keyed by `COOP_S3_SESSION_KEY`; there is no runtime session-load API.

**The graft:** on a warm follow-up assignment, hand the warm pod the prior thread
agent's S3 key and relaunch coop keyed on it, so it downloads and resumes the
**real** conversation — reusing all the warm infrastructure (no cold pod spawn,
image pull, or PVC provision).

```text
respawnThreadAgent (follow-up)
  └─ resolveWarmResumeS3Key(priorBead)      # prior s3_session_key, else prior bead id
        │  (only when WARM_POOL_RESUME_ENABLED)
        ▼
  tryPoolAssign(..., resumeS3Key)           # → AssignRequest.ResumeS3Key
        ▼
  AssignPrewarmed: stamp fields.warm_resume_s3_key  # atomic with agent_state flip
        ▼
  pod standbyWait returns → maybeAdoptWarmResumeKey  # canonicalS3Key = prior key,
        │                                            #   stamp s3_session_key on own bead
        ▼
  supervisor loop relaunches coop ONCE (uncounted restart)
        ▼
  buildCoopEnv wires COOP_S3_SESSION_KEY=<prior transcript prefix>
        ▼
  coop --resume restores the real conversation → injectPrompt(follow-up)
```

**Seams (all additive, default-off):**

- `poolmanager.AssignRequest.ResumeS3Key` → stamped as `fields.warm_resume_s3_key`
  in the same atomic PATCH as the `agent_state` flip (mirrors `operated_by` /
  `thread_id`).
- `bridge.tryPoolAssign` grows a `resumeS3Key` arg; the first-mention path passes
  `""`, the respawn path passes `resolveWarmResumeS3Key(priorBead)`.
- `resolveWarmResumeS3Key` gates on `WARM_POOL_RESUME_ENABLED` and resolves the
  prior transcript prefix the same way `currentS3SessionKeyForTask` does (prefer
  the persisted `s3_session_key`, else the prior bead id — the prefix coop keyed
  its uploads on).
- Pod side: `maybeAdoptWarmResumeKey` (init_s3_restore.go) reads
  `warm_resume_s3_key` off the assigned bead, adopts it into `canonicalS3Key`
  (the same memo cold resume uses), stamps `s3_session_key` on its own bead for
  chain continuity, and signals the supervisor loop to relaunch coop once.

**Honest latency note.** This is *not* literally sub-second: it pays one extra
coop + Claude boot (a few seconds) on top of the warm-assign, because coop must
re-launch to pick up the resume key. It is far cheaper than a full cold resume
(no image pull, no pod admission, no PVC provision) and it restores the **real**
transcript — the thing warm-pool alone cannot do. Genuine sub-second **with**
transcript remains freeze/thaw's domain (the CRIU live-process restore), which
this does not attempt to replace. When runsc widens past the HARD-HOLD, a
gb-turbo thread already rides freeze/thaw and skips this graft entirely (the
runc warm pool is where this graft earns its keep).

**Config / rollback.** `WARM_POOL_RESUME_ENABLED` on the bridge Deployment env is
the single kill switch (any `strconv.ParseBool`-true value; unset/false ⇒ OFF).
Default-off means the warm path is byte-for-byte unchanged until an operator
opts in: with the flag off, `resolveWarmResumeS3Key` returns `""`,
`AssignPrewarmed` skips the field stamp, and `maybeAdoptWarmResumeKey` never
fires — a fresh warm session, exactly as before. Flip the env off to roll back
instantly, no code redeploy.
