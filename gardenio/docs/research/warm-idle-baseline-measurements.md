# Warm-idle thread agents — baseline measurements (spike)

**Task:** ka-0000001429 · **Epic:** ka-0000006987 · **Operator:** Matt Baker (@matthew.baker)
**Measured:** 2026-06-02, prod `gardenio` namespace (america-e2e-eks), agent image `2026.153.10`
**Method:** controller/slack-bridge structured logs + `kubectl` pod phase/event timeline. Measurement only — no code changes.

This is the **status-quo KPI baseline** the epic improves against. It answers the three
questions in the task: (1) warm-idle lifetime today, (2) cold-respawn end-to-end latency,
(3) warm-nudge latency — plus the delta that motivates the idle-TTL keepalive work.

---

## TL;DR

| KPI | Today (baseline) | Source |
|---|---|---|
| **Warm-idle lifetime** of a thread pod after a turn | **~0 (none by design)** — Claude exits at turn end, bead closes, pod despawns | advice ka-0000006028; `stop:` turn-complete + bead-close logs |
| **Cold-respawn** latency (follow-up → first agent hook) | **median ~126 s** (range 107–146 s, N=6) | `reaction-lifecycle` spawning→working deltas |
| ↳ bridge respawn (trigger → bead recreated) | **~1.5–1.8 s** | "no longer active" → "respawned thread agent" |
| ↳ pod schedule → containers ready | **~151 s** (init-clone 92 s + coop/claude-resume 59 s) | pod `conditions` + `Events` |
| **Warm-nudge** injection (bridge → live coop) | **~1–15 ms** (median ~12 ms) | "nudging assignee" → "nudge delivered via coop" |
| **Delta (cold − warm), controller-measurable** | **~126 s vs ~0.01 s ⇒ ~126 s, ~4 orders of magnitude** | above |

> The Claude **turn duration** (model thinking + tool calls) is the same in both paths and is
> *not* part of the delta — both warm and cold pay it once. The delta is purely the cold
> respawn's pod-boot + session-rehydration tax. That tax is what the epic's idle-TTL keepalive
> removes for follow-ups inside the warm window.

---

## (1) Warm-idle lifetime today — effectively zero

**There is no warm-idle window in production today.** Two findings establish this:

1. **Policy.** The active thread-agent role advice **ka-0000006028** ("Thread agent lifecycle:
   respond then gb done") explicitly instructs:
   > *"Thread agents are ephemeral per turn — one response per spawn. … Call `gb done` once
   > you've responded. Don't linger. You'll be re-spawned when needed."*

   So a healthy thread agent **deliberately despawns after a single turn**. The next message in
   the same thread therefore *always* hits the cold-respawn path — never a live pod.

2. **Observed behavior.** In the bridge logs, every thread follow-up resolves through
   `getAgentByThread` → `FindAgentBead` **fails** → `"thread-bound agent no longer active,
   respawning with session resume"` (`bot_mentions.go:206` → `respawnThreadAgent`
   `bot_agent_kill.go:91`). Across the sampled window, **6 of 6** thread follow-ups cold-respawned;
   zero hit a warm live pod via `nudgeAgentForMention`.

   (The `stop:thread-…` task_updates in the logs are **turn-complete markers**
   `HandleStopEvent` / `agentstream.go:737`, *not* despawns — they fire at the end of every
   Claude turn. Don't mistake them for pod lifetime. Measured turn durations were 56–250 s,
   which is model work, not warm-idle.)

### What *would* end a warm pod, if one stayed alive (for the implementation task)

Today nothing keeps a thread pod alive past its turn, but if the idle-TTL work keeps it warm,
these are the incidental despawn levers it must coexist with (all confirmed in code):

- **`gb done` / `gb stop`** — current explicit despawn (the dominant one today).
- **Stale reaper** — `STALE_AGENT_TIMEOUT` default **2 h** (`config.go:673`), reap pass every
  **5 min** (`main.go:~688`). For a genuinely idle live pod, `SyncAll` now **skips the
  `updated_at` bump when coop reports `idle`** (`reporter.go:459`, ka-0000003615), so `updated_at`
  *does* age and a warm-but-idle pod **would** be reaped at ~2 h. ⚠️ The older comment in
  `stalereaper.go` ("SyncAll bumps updated_at every cycle … staleness never trips") is **stale**
  and contradicted by that fix — worth a doc/comment cleanup.
- **Upgrade drain** — on image/spec drift the reconciler drains then deletes; `DrainReady`
  waits for idle or `COOP_UPGRADE_DRAIN_TIMEOUT` default **5 min** (`config.go:665`,
  `reconciler.go:430`). A warm thread pod is drained on the next release.
- **Crash / max_restarts** — `init_agent_loop.go`.
- **Orphan close** — reconciler closes leaked beads in terminal state with no pod
  (`reconciler.go:316`); reaper closes `working` beads whose source task already closed.

**Implication for the epic:** the lever is *not* "make the pod survive longer against the
reaper" (2 h is already plenty). The lever is **stop telling thread agents to `gb done` after
one turn**, and instead keep them parked-idle until `now − last_message_ts > idle_ttl`
(~90 m), then deliberately `gb stop`. The reaper/drain provide a safe upper backstop.

---

## (2) Cold-respawn latency — end-to-end ~126 s (median)

End-to-end = operator follow-up after pod gone → first agent hook fires (card flips to
`working`/⏳). Measured as the `reaction-lifecycle` **spawning → working** delta per respawned
thread agent:

```text
thread-1780410121-147219   107 s
thread-1780410055-057729   118 s
thread-1780409149-576449   123 s
thread-1780409027-641439   126 s
thread-1780077640-006269   138 s
thread-1780372970-393089   146 s
                           N=6  min 107  median 126  max 146  mean 126
```

### Phase breakdown (representative pod `thread-1780352456-856459`)

From `kubectl` pod conditions + events, relative to pod creation:

```text
PodScheduled        +0 s     (scheduler is instant — capacity available)
init-clone pull     +0 s     alpine/git:v2.45.2 already on node
Initialized        +92 s     ← init-clone (git clone of workspace into PVC) dominates
agent image pull    339 ms   agent:2026.153.10 already cached on node (2.6 GB)
ContainersReady   +151 s     ← coop boot + claude --resume + first hook ≈ 59 s after init
Ready             +151 s
```

And the bridge-side head of the path is cheap:

```
14:19:27.8  mention: thread-bound agent no longer active, respawning with session resume
14:19:29.2  agent bead created (kd-…)
14:19:29.3  respawned thread agent with session resume      ← ~1.5 s of bridge work
…pod schedules, init-clone, coop boot, claude --resume…
14:21:56.0  reaction-lifecycle → working (first hook)       ← ~148 s after trigger
```

**Where the ~126 s goes:**
- **~1.5–1.8 s** bridge: detect-dead + recreate agent bead (fast, not the bottleneck).
- **~90 s** `init-clone`: git-clones the workspace repo into the PVC on every cold spawn.
  **This is the single biggest cost** and the highest-leverage target if cold respawn must stay.
- **~59 s** coop boot + `claude --resume` (S3 session restore + Claude process start + first
  `SessionStart` hook). coop's own startup (s3 connect → HTTP listening) is sub-second
  (`14:24:21.78 → .86`); the bulk is the Claude binary launching and rehydrating context.

> Image pull was a non-factor here (both images warm on the node, 339 ms). On a cold node
> the 2.6 GB agent image pull would add materially — a separate variable not captured in this
> sample.

---

## (3) Warm-nudge latency — ~1–15 ms injection

When a pod *is* live, `nudgeAgentForMention` (`bot_mentions.go:1429`) /
`NudgeAgentWithBootRetry` POSTs the message straight into the running coop session. Measured
as bridge **"nudging assignee" → "nudge delivered via coop"** for live pods (same mechanism a
warm thread nudge would use):

```text
gardenio-spike-baseline-threadpod-imx            ~15 ms   (14:47:22.179 → .194)
gardenio-…gardener-release-owner-v3-giod          ~12 ms   (14:53:06.294 → .306)
…others                                          ~1 ms
```

The bridge→pod hop is **single-digit-to-low-tens of milliseconds**. After injection, the only
remaining latency is the Claude turn itself (model-bound) — identical to what the cold path
pays *after* its 126 s boot. So warm nudge reaches a live model in **~10 ms** vs the cold
path's **~126 s** before the model even starts.

---

## Baseline delta (the KPI the epic moves)

```
Cold respawn  (today, every follow-up):  ~126 s  before first hook   (range 107–146 s)
Warm nudge    (target, within idle TTL): ~0.01 s bridge injection → live model
------------------------------------------------------------------------------------
Controller-measurable delta:             ~126 s  (~4 orders of magnitude)
```

**Today the warm path is never taken** (advice ka-0000006028 forces `gb done` after one turn),
so **100% of thread follow-ups pay the full ~126 s cold tax.** The epic's idle-TTL keepalive
converts follow-ups inside the warm window from ~126 s → ~10 ms + one Claude turn.

---

## Recommendations to downstream epic tasks

1. **Implement task (ka-0000005793):** the change is primarily a **policy/lifecycle** flip, not
   a reaper change. Replace "`gb done` after one turn" with "park idle, keep pod warm until
   `now − last_message_ts > idle_ttl` (~90 m), then `gb stop`." The 2 h reaper + 5 m drain are
   already a safe backstop; `SyncAll`'s idle-skip (ka-0000003615) means an idle pod still ages
   out correctly.
2. **Instrumentation task (ka-0000001565):** add a `delivery_path={warm,cold}` label and a
   **respawn-duration histogram**. The single most useful sub-measurement is the
   **init-clone (≈90 s)** and **coop/claude-resume (≈59 s)** split — that's where any residual
   cold cost lives.
3. **If cold respawn must remain fast** (cold-start nodes, or warm window expired): the
   **~90 s init-clone** is the top target (warm PVC reuse / shallow clone / skip re-clone on
   resume), and the **prewarm-pool handoff** stretch task (ka-0000004739) would let even a cold
   thread land on a pre-booted, pre-cloned pod.
4. **Doc cleanup (small):** the stale comment in
   `controller/internal/statusreporter/stalereaper.go` ("SyncAll bumps updated_at every cycle
   regardless … so the wall-clock staleness check never trips") predates ka-0000003615 and is
   now wrong — idle pods *do* age out. Worth correcting so future readers trust the reaper.

---

## Reproduce

```sh
NS=gardenio
BR=$(kubectl get pods -n $NS | grep slack-bridge | awk '{print $1}')

# Cold respawn first-hook latency (spawning -> working per thread agent)
kubectl logs -n $NS $BR --since=6h | grep -E 'reaction-lifecycle.*"thread-' \
  | grep -E '"state":"(spawning|working)"'

# Pod phase breakdown
kubectl get pod -n $NS <thread-pod> -o json | jq '.status.conditions'
kubectl describe pod -n $NS <thread-pod> | sed -n '/Events:/,$p'

# Warm-nudge injection (bridge -> coop, ms)
kubectl logs -n $NS $BR --since=1h \
  | grep -E 'nudging assignee|nudge delivered via coop'

# Respawn trigger -> bead recreated
kubectl logs -n $NS $BR --since=6h \
  | grep -E 'no longer active, respawning|respawned thread agent'
```

*Caveats:* single-namespace, single-image (`2026.153.10`), warm-node sample (image pull ~0).
Cold-node spawns add agent-image pull (2.6 GB). N=6 cold respawns over one afternoon — enough
to size the KPI to ~±20 s, not a tight SLO. Numbers are controller-observable boundaries
(first hook), not Slack-rendered first token, which adds the Claude turn on top equally for
both paths.
