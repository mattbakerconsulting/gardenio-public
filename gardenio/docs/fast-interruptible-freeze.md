# Fast + interruptible agent freeze (Phase 6)

Epic ka-0000008235 · parent Phase 4 productionize ka-0000007417 · operator: Matt Baker.
Plan-of-record for making the **freeze** (checkpoint / put-to-sleep) side of the
gVisor freeze/thaw lifecycle fast *and* interruptible, defending an end-to-end
**< 20 s user-message response** SLO whether the target agent is cold, resuming,
or mid-freeze.

## TL;DR

Phase 5 (ka-0000002590, **closed, goal reached**) proved the **thaw** (wake) side:
real `DRIVER=node` `bench-resume.sh` R4 thaw **p50 = 321–337 ms / p95 = 366–376 ms**,
repo-present (41 files survive thaw), ~7.5–12× faster than R3 fresh-Pod-B, over a
**stable trailing window** — not a single lucky run (MR 3783, benchmark bead
ka-0000003577). Wake is solved.

This epic closes the two remaining gaps on the **freeze** side:

1. **Freeze speed is unmeasured & unproductionized.** `FreezeThawManager.Freeze`
   (`freezethaw.go`) writes a ~3.0 G checkpoint image
   (`checkpoint.img`/`pages.img`/`pages_meta.img`) to the node and has **zero
   production callers**. We have no real-node number for how long a freeze takes.
2. **There is no story for a user message arriving mid-freeze.** If a freeze is
   slow, a user could send a message while the agent is being checkpointed.

**Terminal SLO:** a user message gets a response in **< 20 s** in all three states:
COLD (no pod → thaw/cold-start), RESUMING (thaw from snapshot — already < 1 s), and
MID-FREEZE (interrupt the checkpoint, serve the still-live agent). The < 1 s thaw is
the happy path; the < 20 s SLO is the user-visible guarantee this epic defends.

## Why interruption is *feasible*, not hypothetical

`Freeze` runs `runsc checkpoint --leave-running <sandbox-cid>` (see
`freezethaw.go` §`buildFreezeScript`, and the post-state assertion at L540 that
the sandbox MUST still be `running` after the checkpoint). **Pod A stays alive and
serving for the entire checkpoint.** The checkpoint produces an image *copy* of
process state; it does not stop the live agent. So an inbound message during a
freeze does not race a dead process — it races a live one whose checkpoint we can
simply **abort and discard**. The agent keeps running; we lose only the in-flight
snapshot, which we recompute on the next idle window.

This is the key asymmetry vs. thaw: thaw had to be made fast because there is no
fallback (the agent doesn't exist yet). Freeze has a built-in fallback — the live
agent — so "make it fast" and "make it safely abortable" are two independent,
both-valuable levers, and the < 20 s SLO is defendable even if freeze never gets
below a few seconds.

## Where the time goes today (hypotheses to measure, not assume)

`Freeze` (`freezethaw.go`, L483) does, in order:

| Step | What | Suspected cost |
|---|---|---|
| 0 | get pod, resolve node, pin image/bundle paths | ms (control-plane) |
| 1 | node exec: `runsc checkpoint --leave-running` → write 3.0 G image | **seconds — suspected dominant cost (page dump + disk write)** |
| 2 | post-state assertions (checkpoint rc, image present, sandbox still running, container_count, bundle-OK) | ms |
| 3 | image-relocation guard (3.0 G image moved to controller-owned safe path, ka-0000003702) | **seconds — 3.0 G move/copy if cross-device** |
| 4 | persist `Snapshot` to the SnapshotStore | ms |

**Do not pre-commit to which step dominates.** C1 (benchmark) exists precisely to
turn these hypotheses into numbers. The 3.0 G image is the obvious suspect, but
the relocation guard (Step 3) and the runsc page-dump (Step 1) are separable and
must be attributed independently before we optimize.

## Workstreams (children of ka-0000008235)

### C1 — Freeze-latency benchmark + histogram (mirror Phase-5 thaw proof) — FIRST
Build `bench-freeze.sh` (mirroring `bench-resume.sh`; both PoC harness scripts now live
only in the archive tag `archive/clutter-pre-delete-2026-10-28`, see [`gvisor-runsc-cr/README.md`](gvisor-runsc-cr/README.md))
and a `gardenio_freeze_latency_seconds` Prometheus histogram emitted from the
controller freeze path (mirroring `gardenio_resume_latency_seconds`, ka-0000003577).

- **Metric:** freeze latency = `t(checkpoint image durably persisted at
  snap.ImagePath + Snapshot recorded)` − `t(freeze requested)`. Break out the
  per-step attribution (runsc-checkpoint vs. image-relocation vs. persist) as
  histogram labels or separate sub-timers so we know *where* the seconds are.
- **Driver parity:** `DRIVER=node` against the live `gvisor-poc` substrate with a
  **repo-present** agent (`REQUIRE_REPO=1`) — same hard scope rule that Phase 5
  enforced (C6 ka-0000007582). A fast freeze of an empty workspace does not count.
- **Convergence gate:** report a converged freeze p50/p95 over a **stable trailing
  window** (per-run CSV+JSONL row, rolling percentiles + trend), not a single run.
  This run *measures*; it does not assert a target until C1 lands a real baseline.
- Blocks C2's interrupt-timeout tuning (need the real distribution first).

#### C1 baseline — CONVERGED (ka-0000005147, `DRIVER=node`, gvisor-poc, repo-present)

Real `runsc checkpoint --leave-running` + Step-3 relocation copy on the live
`gvisor-poc` node, `REQUIRE_REPO=1`, 40 freezes, **BASELINE-STABLE** over a full
30-freeze trailing window (total p50 moved 0 % < 10 % across 3 consecutive
batches; run `node-baseline-20260607T162334Z`):

| step | p50 | p95 | p99 | share of total p50 |
|---|---|---|---|---|
| **total** | **751 ms** | **812 ms** | 814 ms | 100 % |
| relocation | 662 ms | 722 ms | 733 ms | **~88 %** |
| runsc-checkpoint | 80 ms | 106 ms | 126 ms | ~11 % |
| persist | 11 ms | 12 ms | 13 ms | ~1 % |

**Headline finding (overturns the pre-measurement hypothesis):** the dominant
freeze cost is the **Step-3 relocation copy** (`cp -a` of the merged rootfs into
the GC-safe lowerdir), **not** the runsc page-dump. On this substrate the
checkpoint image is only ~7.8 M (179 K `checkpoint.img` + 7.9 M `pages.img` →
80 ms page-dump), while the relocated rootfs is ~129 M (→ 662 ms copy). The
"3.0 G image write is the suspect" hypothesis (Step 1 above) is the **wrong cost
model**: relocation scales with **rootfs size**, checkpoint with **resident page
count**, and the rootfs copy dominates. A heavier agent makes this *worse*, not
better — a multi-GB agent rootfs would push the relocation copy into multi-second
territory while the page-dump stays comparatively cheap.

**This decides C3's lever:** attack the relocation copy first (checkpoint
directly to the safe path / `cp --reflink` on the overlay upperdir / eliminate
the move entirely), not image compression. Image-shrink (the other C3 candidate)
is a distant second on this evidence. Artifacts: per-run CSV+JSONL + Prometheus
textfile under the PoC harness's `bench-out/` (buckets match
`internal/metrics/freeze_latency.go`).

### C2 — Interruptible freeze: abort-on-inbound-message — IMPLEMENTED (ka-0000005834)
Detect a user message targeting an agent that is **mid-freeze** and abort the
checkpoint, leaving Pod A running and routing the message to the live agent.

**Landed (gated behind `FREEZE_INTERRUPT_ENABLED`, default off):**
- **Registry + abort.** `FreezeThawManager` keeps an in-flight registry
  (`agent → {cancelFunc, aborted}`) populated only when `InterruptEnabled`.
  `Freeze` runs the node-checkpoint exec under a derived ctx; `AbortFreeze(agent)`
  flips `aborted` under a lock and cancels that ctx, so the blocking
  `ProbeNodeExecutor.Exec` (`StreamWithContext`) returns promptly (the node
  `runsc` is also `timeout`-bounded as a backstop). `IsFreezing(agent)` exposes
  registry state.
- **Race rule (TOCTOU).** `Freeze`'s finalize defer reads the `aborted` verdict
  under the registry lock. If an abort fired at **any** point — including
  microseconds after a successful `Put` — `Freeze` returns `ErrFreezeAborted`,
  the just-persisted `Snapshot` is `Delete`d and its image best-effort reaped
  (`discardAbortedFreeze`), and the agent is left live. A mid-flight abort
  persists no snapshot (the partial image was never `FT_IMAGE_OK`-relocated).
  Never a dead agent without a thaw target: `--leave-running` keeps Pod A alive
  and the event handler only tears down on freeze **success**.
- **Event handler.** On `ErrFreezeAborted` the `AgentStop` path abandons the
  graceful stop entirely — no `DeleteAgentPod`, no token revoke, no "Stopped"
  status, no session-close metrics — and records `freeze_interrupts_total`.
- **Ingress seam.** `POST /api/v1/freeze/interrupt {"agent":"…"}` on the
  controller health server → `ft.abortFreeze`; the bridge `bot_mentions.go`
  calls it (`maybeInterruptFreeze`) right before nudging the resolved agent, so a
  mid-freeze Pod A is kept live before the message is forwarded. 404
  (`aborted:false`) when nothing is in flight — caller proceeds unchanged.
- **Config / helm.** `cfg.FreezeInterruptEnabled` ← `FREEZE_INTERRUPT_ENABLED`;
  helm `gvisor.freezeThaw.interruptEnabled` (default false) renders the env.
- **Tests.** `freezethaw_interrupt_test.go` (abort mid-checkpoint, abort-races-
  completion discard, disabled rollback, no-inflight), lifecycle flag plumbing +
  nil-safety, and the HTTP endpoint. The live-substrate E2E below remains.

- **Detect:** the controller freeze path tracks in-flight freezes (agent → freeze
  context). The message ingress (NATS / bridge `bot_mentions.go`) checks that set
  before deciding cold-start vs. route-to-live.
- **Abort:** cancel the freeze `ctx` (the node `runsc` invocation is already
  `timeout`-bounded — kd carries the RUNSC_TIMEOUT wrapper), discard the partial
  image at `freezeImagePath(agent)`, and DO NOT persist a `Snapshot`. Because the
  checkpoint is `--leave-running`, Pod A is untouched — the abort is a clean no-op
  on the live agent.
- **Route:** hand the message to the still-warm live agent (no cold start, no
  thaw). This is the < 20 s safety net.
- **Idempotency / races:** define the ordering when abort races
  checkpoint-completion (freeze finished microseconds before the abort): prefer
  "snapshot exists, agent also still live → keep agent live, drop or lazily-reap
  the snapshot." Never end up with a dead agent and no thaw target.
- **Test:** E2E on `gvisor-poc` — start a freeze, inject a message mid-checkpoint,
  assert (a) checkpoint aborted, (b) Pod A still Ready, (c) message answered by
  the live agent, (d) no orphaned 3.0 G image left on the node.

### C3 — Freeze-speed optimization (data-driven, gated on C1) — ka-0000002239
**Gated on C1, now landed.** The C1 attribution (above) picked the lever for us:
the **Step-3 relocation copy dominates (~88 %, p50 662 ms)**, so we attack the
copy, not the image. Image-shrink/compression is a distant second on the evidence
and is **not** pursued.

#### Chosen optimization: `cp --reflink=auto` on the relocation copy

The relocation copies the **live merged rootfs** into the controller-owned GC-safe
lowerdir (`$BUNDLE/<c>/lower`) so it survives Pod A teardown — `freezethaw_node.go`
§`[3/8]` and the `bench-freeze-node.sh` mirror. The copy was a byte-for-byte
`cp -a "$T/$cid/rootfs/." "$LOWER/"`. We change it to:

```
cp -a --reflink=auto "$T/$cid/rootfs/." "$LOWER/"
```

- **Why it's the right lever:** on a reflink-capable backing fs (overlayfs upperdir
  on **XFS** with `reflink=1` — the modern `mkfs.xfs` default and the EKS node fs
  signature — or Btrfs/bcachefs), the clone becomes an **O(metadata) copy-on-write**
  operation instead of moving ~129 M (or multi-GB for a heavy agent) of data. That
  directly collapses the C1-dominant cost.
- **Why it's strictly safe (zero regression risk):** `--reflink=auto` **falls back
  transparently to a full byte copy** on any fs that cannot reflink (ext4, 9p,
  tmpfs), so a non-CoW node is exactly as fast as before — never slower. And the
  GC-safety invariant (ka-0000003702) is **preserved**: a reflink shares extents but
  is an **independent inode** that survives source deletion identically to a full
  copy, so the relocated lower still survives Pod A teardown.
- **No new flag.** The change is a strict Pareto improvement, so it ships on the
  existing `FreezeThawEnabled` path — there is no behavior to gate off.

#### How the speedup is proven (substrate run still required)

The realized speedup shows up **directly in `FT_RELOCATION_MS`** (a CoW clone
collapses that timer), which already feeds the `gardenio_freeze_latency_seconds`
relocation sub-histogram and the bench CSV/JSONL — no new measurement plumbing
needed. To make the *cause* legible we additionally emit a `FT_RELOC_FSTYPE_<c>`
signal (the LOWER target's filesystem type) from both the prod node script and the
bench, so a substrate run can tell at a glance whether the node fs was
reflink-capable or whether `--reflink=auto` necessarily fell back to a full copy.

**Status:** code + tests landed (this MR). The **converged real-node re-baseline is
still pending a live `gvisor-poc` substrate** — at authoring time the substrate is
torn down (no `gvisor-poc` pods, controller lacks `gardenio-gvisor` RBAC), so the
post-optimization relocation p50 has NOT yet been captured on a real node. The
acceptance bar is identical to C1: re-run `DRIVER=node REQUIRE_REPO=1
bench-freeze.sh` over a stable trailing window and record the new relocation p50/p95
+ `RELOC_FSTYPE` here, alongside the C1 baseline, to confirm the CoW path engaged.
Tracked as the C3 follow-up; until that run lands, the speedup is **expected, not
yet measured**.

#### C3 follow-up — node fs reflink-capability CONFIRMED (ka-0000006167, 2026-06-07)

The converged `DRIVER=node` re-baseline remains substrate-blocked (no live
`gvisor-poc` pods; the controller SA has no `gardenio-gvisor` RBAC, and the canary
that would stand the substrate up — ka-0000001355 — is itself yielded waiting on a
release reaching prod). But the **single load-bearing unverified premise** of the
C3 optimization — *"will `--reflink=auto` actually engage CoW on the EKS node fs, or
silently fall back to a full byte copy?"* — is **substrate-independent** and was
verified directly on a live cluster node:

- The EKS nodes run **Amazon Linux 2023** (AMI `2023.11.*`, kernel `6.12.*`,
  `m6a`/Nitro), containerd 2.2. The primary EBS data volume `/dev/nvme0n1p1` (which
  backs `/var/lib/containerd` → the overlay upperdirs the relocation copies) is
  **`xfs`** — `df -T` reports `/dev/nvme0n1p1 xfs`, mounted `inode64` (the modern
  `mkfs.xfs reflink=1` default signature).
- A live **`cp --reflink=always`** on that XFS volume **succeeded** (a forced reflink
  errors out on a non-reflink fs), producing an **independent inode** (`src` and
  `clone` inode numbers differ) whose contents **survived deletion of the source
  byte-for-byte** (sha256 identical before/after `rm src`). That is exactly the
  C3 + GC-safety (ka-0000003702) invariant: a reflink shares extents but is an
  independent inode that outlives the source.

**Conclusion:** on this node fs, `--reflink=auto` takes the **CoW path**, not the
full-copy fallback — so the C1-dominant ~662 ms / ~88 % relocation copy is expected
to collapse to an O(metadata) clone once measured. The risk that the optimization is
a silent no-op on the production substrate is **retired**. What still requires a live
substrate is the *quantified* post-optimization relocation p50/p95 (the real
`runsc checkpoint` + relocation timing); the bench harness is validated end-to-end in
sim mode (converges, emits CSV/JSONL/prom/summary, percentile + stability-gate math
green) and is ready to capture `RELOC_FSTYPE` + the converged numbers the moment a
repo-present `gvisor-poc` substrate exists.

If freeze is *already* comfortably inside the < 20 s SLO with the interrupt safety
net (C2) — and at p50 751 ms it is — C3 is a margin-of-safety win, not a blocker:
the SLO, not raw freeze speed, is the bar.

#### C3 follow-up — converged `DRIVER=node` re-baseline: CoW does NOT engage on the bench relocation (ka-0000006167, 2026-08-27)

The quantified re-baseline the acceptance asked for finally ran on a **live
`gardenio-gvisor` substrate** (node `ip-10-50-65-160`, AL2023 kernel `6.1.180`, node
root fs `/dev/nvme0n1p1` **xfs**, reflink-capable). Command:
`DRIVER=node REQUIRE_REPO=1 NS=gardenio-gvisor PROBE=gvisor-probe-9hlcn
bench-freeze.sh`, 40 freezes, **BASELINE STABLE** over the full 30-freeze trailing
window (total p50 moved 0 % across 3 batches).

| step             | C1 baseline p50 | **C3 re-baseline p50** | p95   |
|------------------|-----------------|------------------------|-------|
| total            | 751 ms          | **1174 ms**            | 1186 ms |
| relocation       | 662 ms          | **1104 ms**            | 1115 ms |
| runsc-checkpoint | —               | 48 ms                  | 51 ms  |
| persist          | —               | 21 ms                  | 23 ms  |
| `RELOC_FSTYPE`   | —               | **xfs**                | —      |

**Verdict: the CoW path did NOT engage — `--reflink=auto` silently fell back to a
full byte copy.** Relocation p50 did **not** drop vs C1; it is *higher* (1104 ms vs
662 ms — noise-band + a failed per-file `FICLONE` ioctl before the copy fallback),
i.e. the opposite of the expected O(metadata) collapse. `RELOC_FSTYPE=xfs` here is
**false reassurance**: it reports the *destination* fstype, not whether a clone
actually happened (exactly the trap bug ka-0000007174 named).

**Mechanism, proven directly on the node** (not inferred):

- The relocation SOURCE `$BDIR/rootfs` (bench-freeze-node.sh:161) is an **overlayfs
  merged mount** (`grep /proc/mounts` → `overlay`), just like the prod path.
- `cp --reflink=always` reading *through* that overlay source into bare xfs
  **fails**: `failed to clone …: Invalid cross-device link` (EXDEV) — `FICLONE`
  cannot clone an overlayfs inode into a different fs.
- The same `cp --reflink=always` **plain-xfs → xfs succeeds**, confirming the fs
  supports reflink and the *destination* is fine. The block is the overlay source,
  not the fs.

This **empirically reproduces canary #13** (2026-07-03, prod 2026.184.9) at bench
scale and closes out the C3 headline question with a definitive **negative** for the
bench relocation path.

**Important scope note — the bench does not yet exercise the shipped fix.** The
controller relocation was fixed by **MR !5150** (bug ka-0000007174): it stopped
reading through the overlay merged mount and now copies the **bare-xfs constituent
layer dirs** (upperdir + image lowerdirs), where `FICLONE` *does* reflink. The bench
harness (`bench-freeze-node.sh`) still copies through the overlay merged mount, so
these numbers measure the **old no-op path**, not the post-!5150 controller path.
Consequently this bench cannot demonstrate the controller's CoW win — it can only
confirm (as it does) that copying through an overlay source never reflinks.

**Next lever** to actually quantify the post-!5150 win: either (a) update
`bench-freeze-node.sh` to relocate the bare-xfs layer dirs the way the controller now
does (mirror the MR !5150 copy plan) and re-baseline, or (b) measure the controller's
real freeze path directly via a live canary carrying the `FT_RELOC` reflink
hit/fallback counter (the smoking-gun `hits>0` the secondary fix in ka-0000007174
added). Until one of those runs, the *controller's* relocation CoW win remains
structurally expected (xfs + bare-layer source) but not yet bench-quantified; the
freeze SLO is unaffected — total freeze here is p50 1174 ms, an order of magnitude
inside the < 20 s bar.

### C4 — End-to-end < 20 s SLO regression gate — IMPLEMENTED (ka-0000000107)
A standing timed test (mirror C7 ka-0000006324 `test-resume-latency.sh`) that
exercises the **user-visible** path for all three states and asserts < 20 s:
- COLD: no pod → spawn/thaw → first response.
- RESUMING: snapshot present → thaw → first response (reuses the proven < 1 s gate).
- MID-FREEZE: freeze in flight → message → interrupt → live response.
Wired into release gating the same way the resume-latency gate was operationalized.

- **Module.** `gardenio/tests/e2e/scripts/test-userresponse-latency.sh` — the
  user-visible companion to the C7 resume gate. Same opt-in / LOUD-skip discipline
  (`GVISOR_E2E=1` or `USERRESPONSE_LATENCY_E2E=1`, gvisor-poc substrate required),
  same proven node scripts, same `p50 < GATE_P50_MS` (default 10 000 ms) gate —
  applied **per state**. Emits `userresponse_latency_ms state=…` per sample and a
  `gardenio_userresponse_latency_seconds` histogram textfile (sibling buckets to
  `gardenio_resume_latency_seconds`).
- **Three legs.** COLD = fresh-B (create→Ready→restore wall-clock). RESUMING =
  warm-B (restore-only, the proven sub-second path; reuses
  `single-container-thaw-into-b-node.sh`). MID-FREEZE = the C2 leg: a new
  `LEAVE_RUNNING_ONLY=1` mode of `single-container-resume-node.sh` takes a
  `runsc checkpoint --leave-running`, asserts the sandbox is still `running`
  (the L540 invariant the interrupt path depends on), and reports the in-flight
  checkpoint window (`LR_FREEZE_MS`) — the bound on how long a mid-freeze message
  waits before the still-live agent serves it with **no thaw**.
- **Release gating.** Pinned (not hashed) in `run-shard.sh` `shard_for` like the
  C7 gate; gated block runs it under `GVISOR_E2E=1` / `USERRESPONSE_LATENCY_E2E=1`.
  Dedicated `gardenio-userresponse-latency` dispatch + `userresponse-latency` task
  in `.rwx/e2e.yml`. Proven green on the live gvisor-poc substrate: COLD p50 ≈ 8 s,
  RESUMING p50 ≈ 0.3 s, MID-FREEZE p50 ≈ 0.1 s (all < 20 s).

## Config / flags (reuse, default-off)
Reuse `FreezeThawEnabled` and the existing freeze tunables in `freezethaw.go`.
Add (if needed) a `FreezeInterruptEnabled` flag (default off) so the abort path is
opt-in and instantly reversible. Default-off keeps blast radius zero; flip-on is
the rollout, flip-off the rollback.

## Non-goals
Fleet-wide default-on; removing today's cold-start path; resuming Claude session
RAM state on thaw (`session_resume=false` stays fine and desirable). Multi-node
freeze migration. Opt-in until a separate go/no-go.

## Constraints carried from Phases 2–5
- `runsc` C/R can hang on process exit — every node-side `runsc` stays wrapped in
  `timeout` + Go-side post-state assertions (`freezethaw.go` already does this).
- `runsc checkpoint --leave-running` MUST leave the sandbox `running` (post-state
  assertion at L540) — the interrupt path depends on this invariant holding.
- Credentials (IRSA STS, projected SA token) are re-minted on thaw from
  kubelet-fresh on-disk tokens, never restored from frozen RAM — unchanged.
- All real-node proofs must be over a **stable trailing window** with
  `REQUIRE_REPO=1`, not a single lucky run (the Phase-5 evidentiary bar).

## Checkpoint-consistent 9p-gofer snapshot (ka-0000004656, decision ka-0000003069)

**Defect.** `/tmp`, `/work`, and every other runtime-populated 9p-gofer
EmptyDir/subPath mount is preserved into the snapshot by a host-side warm copy
(`$BUNDLE/.tmp`, `.work`, `.gofer/<n>`), because those host-side trees never ride
the checkpoint image's VFS state (the kubelet re-provisions a fresh empty dir for
Pod B). The first cut took those copies **after** a bare
`runsc checkpoint --leave-running`, which had **already resumed the sentry** — so a
live Claude session kept mutating the gofer trees (it creates then unlinks
`/tmp/claude-1000/<session>/tasks/<id>.output` once per tool call) in the gap
between the checkpoint's VFS capture and the `cp`. The checkpoint then referenced a
transient dentry the later copy no longer saw, so thaw's `vfs.CompleteRestore()` 9p
walk `ENOENT`ed it → agent sub `rc=128`. A **quiescent** canary masked it (no churn
⇒ copy == dump ⇒ false-positive `rc=0`); an **active-Claude** canary reproduced it.

**Fix (dir A — consistent snapshot).** Bracket **both** the gofer warm copies **and**
the checkpoint save inside one external `runsc pause`..`runsc resume` window:

```
runsc pause $SB            # external stop — guest quiesced, in-flight 9p ops drained
  cp .work / .tmp / .gofer # warm copies of the SAME VFS the checkpoint will dump
  runsc checkpoint --leave-running $SB
runsc resume $SB           # release the external stop
```

The guest executes **zero instructions** from the pause through the resume, so the
warm copy and the checkpoint dentry set are byte-identical — the TOCTOU is gone.
Source-grounded against the deployed gVisor `release-20260525.0`:

- `Container.Checkpoint` accepts a **Paused** input (`requireStatus(Created,
  Running, Paused)`), so checkpointing inside the window is legal.
- `Kernel.Pause`/`Unpause` are **reference-counted** (`BeginExternalStop` /
  `EndExternalStop`, "nest and require an equal number of calls to Unpause"). Our
  external pause **nests** with the save path's own balanced `k.Pause()` /
  `defer k.Unpause()` (`pkg/sentry/state.Save`), and `--leave-running`'s post-save
  `BeforeResume` does **not** clear our external stop — so the sentry stays paused
  until our explicit `runsc resume`. The post-checkpoint `FT_SANDBOX_STATE=running`
  invariant therefore still holds (asserted Go-side).
- `Kernel.Pause` drains `runningGoroutines`/`aioGoroutines` before returning, so
  the host-side gofer tree is fully quiescent before the copy.

**Exit-safety.** A `trap 'resume_sb' EXIT INT TERM` resumes the sentry on **any**
exit path (checkpoint failure, error, an aborted exec's SIGTERM) so a paused sandbox
is never left hung; `resume_sb` is idempotent (guarded on `PAUSED=1`). SIGKILL can't
be trapped, but with no thaw target persisted the lifecycle cold-starts and deletes
Pod A (destroying the sandbox), so an unresumed pause is reaped — never eternal.

**Degrade-safe.** If `runsc pause` itself fails (`FT_PAUSE_RC != 0`) the script logs
and proceeds **unpaused** — the pre-fix non-atomic copy — rather than aborting; a
lost freeze is strictly worse than a copy that is no worse than before this fix.

**Scope.** Only the cheap gofer trees ride inside the window; the 3.0 G rootfs
relocation stays **after** resume (it is an overlay source, not a churning 9p gofer
mount, and its restore does no per-dentry 9p walk) so agent downtime is not extended
by the multi-second copy ("…before relocation", per the decision).
