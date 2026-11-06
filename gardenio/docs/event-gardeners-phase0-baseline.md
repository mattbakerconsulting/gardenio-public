# Event-Driven Gardeners — Phase 0 Baseline

> **Task:** gd-JLWLjcTfDz, under epic gd-BYKE0Zi0gh ("Event-driven gardeners: wake on
> work, dormant when done, SLA-nudged escalations, no scheduled runs").
> **Snapshot taken:** 2026-09-20, ~20:30 UTC, from `kd`/`gb` against the live fleet and
> from a read of `main` at the commit checked out for this task.
> **Purpose:** (1) inventory the wake surface that exists in code today, (2) map each of
> the 7 live epic-gardeners to its epic/cron/recommended wake events, (3) baseline
> cron-only fire counts, no-op rate, and spend per gardener over the last 48h. **This
> baseline table is what Phase 3 compares against** to prove the event-wake migration
> actually cut no-op fires and spend.

## Contents

- [1. The wake surface that exists in code today](#1-the-wake-surface-that-exists-in-code-today)
- [2. The 7 live gardeners: epic, cron, recommended wake events](#2-the-7-live-gardeners-epic-cron-recommended-wake-events)
- [3. Baseline: fires, no-op rate, spend per gardener (last 48h)](#3-baseline-fires-no-op-rate-spend-per-gardener-last-48h)
- [4. Summary going into Phase 1+](#4-summary-going-into-phase-1)

## 1. The wake surface that exists in code today

### 1.1 Trigger kinds and match ops (`scheduler/triggers.go`)

`TriggerEntry` (`gardenio/controller/internal/scheduler/triggers.go:38-48`) is the unit
the engine evaluates: `Kind, Cron, Timezone, Events []string, Match *conditions.Expression,
Preflight *conditions.Expression, Throttle, Debounce, Label`. Four kinds exist
(`triggers.go:23-26`):

- `cron` — fires on a cron schedule, gated by an optional `Preflight` bead-query
  expression (see 1.2).
- `manual` — fires only via an explicit `gb gardener fire`/operator action carrying a
  matching `Label` (e.g. `operator-fire`, used as the manual escape hatch in every
  reference shape in `gardener_wake_migration_test.go`).
- `bead_event` — fires when a buffered bead event's `EventType` (`"created"`,
  `"updated"`, or `"closed"` for bead events; `"resolved"` for decisions —
  `triggers.go:246-256`) and payload match `Events`/`Match`, subject to `Throttle`/
  `Debounce`.
- `decision_resolved` — fires when a decision bead resolves; `Match` typically keys on
  `watch_bead_id op:in_subtree_of value:<epic>` to scope to one epic's subtree.

`Match`/`Preflight` expressions are built by `conditions.Expression` (`all`/`any` trees
of field/op/value leaves); legacy per-bead watcher fields (`watch_project`,
`watch_role`, `watch_bead_id`, `watch_type`, `watch_channel`, `watch_from`,
`label:<name>`) are lowered into the same AND/OR shape by `watchersToMatch()` and
`LowerLegacyToTriggers()` (`triggers.go`, `LegacyWatchers` struct), which also maps
legacy `trigger_mode=bead_closed` → `bead_event` events=["closed"] and
`trigger_mode=mail_received` → `bead_event` events=["mail_sent"].

`EvaluateTriggers`/`EvaluateTriggersAll` (`triggers.go:555-653`) dispatch per-kind:
cron entries go through `EvaluateCronEntry` (`triggers.go:658`); bead_event/
decision_resolved go through `EvaluateEventEntry`/`EvaluateEventEntryAll` against
whatever the shared `EntryEngine` has buffered since the last drain.

### 1.2 Cron-with-preflight (the bead_query gate our 7 gardeners actually use)

`cronPreflightHolds` (`triggers.go:708`) evaluates a cron entry's `Preflight`
expression before allowing the fire; it is **fail-closed** — a nil daemon or an
evaluator error returns `false` (suppresses the fire), fixing the "documented
no-op floor never actually no-ops" bug (ka-0000007730). This is the *only* gate our
7 live gardeners have today (see 1.8): a `conditions` bead-query on `trigger:schedule`
beads, structurally the same idea as `Preflight` but evaluated by a different,
older code path (`ScheduleTriggerSource`, not `EvaluateCronEntry`) — see 1.8.

### 1.3 Reference hybrid shapes locked by `gardener_wake_migration_test.go`

`gardener_wake_migration_test.go:34-49` hard-codes the exact `triggers[]` JSON applied
to 4 already-migrated gardeners (runbook §8.6 hybrid shape: **one fast event entry +
one slow cron floor + a manual escape hatch**), and the test fires each through the
real `ParseTriggers`+`EvaluateTriggers` path so a bead-field drift breaks CI, not just
docs:

- `wakeAX` (ka-0000007578, epic-driver): `bead_event` on `closed`/`updated` matching
  `parent==<epic>` (throttle 60s) + `decision_resolved` matching
  `watch_bead_id in_subtree_of <epic>` + a `cron` floor (`7 */6 * * *`) gated by a
  `Preflight` bead_query (`parent=<epic>`, types task/bug/epic,
  statuses open/in_progress/blocked, `> 0`) + `manual` (`operator-fire`).
- `wakeKR` (ka-0000008247): same `bead_event`+`cron`+`manual` shape but **no**
  preflight gate on the cron floor — deliberate, because this gardener validates a
  sliding window of live envs every cycle even with an empty backlog (runbook §5.4
  reason #3; also called out in this doc's §2 notes).
- `wakeNR` (ka-0000007624, Resumer Watchdog): `bead_event` on `label.added` matching
  `label:gardener-stuck-escalate present` (throttle 60s, debounce 5s) + a slow cron
  floor + `manual`.
- `wakeOO` (ka-0000006911): `bead_event` (closed/updated on `parent==<epic>`) +
  `decision_resolved` (in-subtree) + a daily freshness-guard `cron` + `manual`.

This is the template Phase 1 should reuse for the 7 gardeners in §2: swap `<epic>` for
each gardener's target epic, keep the cron floor (interval per §2's existing cadence)
as the slow catch-up, and add `manual`/`operator-fire` for the escape hatch.

### 1.4 `gb gardener start` flags, incl. `--wake-on-mr`

`gardenio/controller/cmd/gb/gardener.go:2018` registers `--wake-on-mr <mr-url>`:
"wake the gardener on the FIRST update to that MR (new commit/comment/status
change/merge). Adds a near-real-time bead_event trigger on the MR's mirror bead + a
fallback cron floor (the §8.2 hybrid recipe)." `buildWakeOnMRTriggers(mrURL, cron, tz,
epicID)` (`gardener.go:673-712+`) builds: a `bead_event` entry (`Events:
["created","updated","closed"]`, `Match: type=="merge-request" AND mr_url==<url>`,
`Throttle: "60s"`, `Label: "wake-on-mr"`) plus, if `cron` is set, a
`fallback-watchdog` cron entry gated by the same `parent=<epic>` /
open-in_progress-blocked `Preflight` as 1.2/1.3. **Caveat the flag's own comments
call out** (`gardener.go:434-442, 482-485`): an event-mode `--wake-on-mr` gardener MUST
carry the `trigger:multi` label, not `trigger:schedule`, or `MultiTriggerSource`
never loads it and the event entry is dead weight (see 1.8).

### 1.5 `gb decision reflag`

`gardenio/controller/cmd/gb/decision.go:677-825`. `gb decision reflag <id>` bumps a
decision's priority one step toward P0 (or to an explicit `--priority`, capped
0-4), stamps `reflag_count`/`reflagged_at`/`reflagged_by` (+ `reflag_reason` if
`--reason` given), and — once priority reaches P0/P1 — adds the `escalated` label so
the Slack bridge reposts a 🚨 notification (deduped per-bead with a TTL so repeat
reflags at P0 don't spam Slack). Idempotent at P0: further reflags only bump
`reflag_count`, priority does not go negative. This is not itself a *wake* primitive
for gardeners — it's an operator/crew nudge on a decision's visibility/priority — but
`decision_resolved` (1.1) is what a gardener's `triggers[]` would key off once the
decision this reflag surfaced actually resolves.

### 1.6 `gb escalation reflag` / `gb escalation pingback`

`gb escalation reflag` shares `runReflag` with `gb decision reflag`
(`decision.go:704-771`, doc comment cites ka-0000004918): identical priority-bump/
audit-comment behavior, **plus** (escalation-only, `decision.go:755-771`) it stamps
the pingback handshake fields (`pingback_requested_at`, `pingback_count`,
`pingback_by`, `pingback_source: "reflag"`) so the Slack bridge's Escalations watcher
re-posts the escalation card — the doc comment notes the `escalated` label alone only
auto-reposts *decision* beads; escalations need the pingback fields.

`gb escalation pingback <id>` (`gardenio/controller/cmd/gb/escalation_pingback.go:20-105`)
is the narrower verb: re-surfaces a stalled escalation in Slack **without** touching
priority (unlike reflag). Rate-limited to one per hour unless `--force`
(`pingbackMinInterval = 1h`, line 17). Doc comment (lines 34-36): "the engine also
fires automatic pingbacks on a backoff cadence (~12h, ~24h, ...)" — i.e. there is
already a time-based nudge loop for stale escalations independent of any gardener;
manual pingback just gets ahead of that cadence.

### 1.7 `stop.go` `on_loop_end` actions — declared, only partially wired

`StopSpec.OnLoopEnd` (`gardenio/controller/internal/scheduler/stop.go:221-223`)
documents three values: `"close"` (default) | `"notify_slack"` | `"spawn_followup"`,
enforced as a closed set by `stop_validate.go:74-117` (`validOnLoopEndActions`,
unrecognized values rejected at write time). **But the engine-side execution path,
`gardenerTerminate` (`gardenio/controller/internal/bridge/gardener_evaluator.go:1306-1327`),
never reads `OnLoopEnd` at all** — it unconditionally does the same three things on
every loop_end: add a terminating comment, (best-effort) spawn a chain heir if the
gardener declared a spawn-mode `chain.to` (`maybeSpawnChainHeir`, unrelated to
`spawn_followup`), close the bead, and disarm `enabled`. Every other reference to
`OnLoopEnd` in the tree is UI-side only — `beads-viewer`'s gardener-authoring form
(`gardener_stop_builder.go:313,449,454,674-675,801-809`) and the human-legible summary
renderer (`gardener_legible.go:114,332`, `templates/partials/gardener_config.html:213-215`).
**Net: `notify_slack` and `spawn_followup` are authorable and validated, but today
they behave identically to `"close"` at runtime** — a documented-but-unimplemented
gap Phase 1 should either close (wire `gardenerTerminate` to branch on `OnLoopEnd`) or
explicitly scope out of this epic.

### 1.8 Which bead events kbeads publishes today (`kbeads/internal/events/events.go`)

Full topic list (`events.go:10-75`): `beads.bead.created/updated/closed/deleted`,
`beads.dependency.added/removed`, `beads.label.added/removed`,
`beads.comment.added/deleted`, `beads.follower.added/removed`,
`beads.advice.created/updated/deleted`, `beads.session.*`, `beads.bead.transferred`,
`beads.plan.step.added`, `beads.mail.sent`, `beads.jack.*`, `beads.taint.*`.

**A `"created"` event exists** (`TopicBeadCreated = "beads.bead.created"`,
`events.go:11`) — contradicting the epic brief's implied uncertainty. **A
distinguished "child added under this epic" event does not exist.** The closest
thing, `TopicDependencyAdded` (`beads.dependency.added`, `events.go:15`), "fires on
EVERY parent-child edge in the fleet and forces a consumer to re-walk the graph to
decide plan membership" (doc comment, `events.go:41-42`) — it is unscoped and
untyped-by-relationship at the wire level. By contrast, `TopicPlanStepAdded`
(`beads.plan.step.added`, `events.go:38-56,191-224`) is the pattern for what a real
"child added under epic" signal would look like: resolved **server-side** at the
write chokepoint (fail-closed membership check — only fires when the daemon
confirms the new parent really is a plan/phase epic), carries the resolved
`PlanID/PhaseID/StepID/StepTitle/StepType/Operator` so a consumer never re-walks the
graph, and is **dual-published** — fire-and-forget core-NATS plus a durable
JetStream `PLAN_EVENTS` copy with a deterministic `Nats-Msg-Id` for dedup/replay
safety (`events.go:50-55`). Phase 1's "wake on new child under epic" leg (§2's gap
(c)) has two options: (i) a `bead_event` trigger matching `beads.dependency.added`
restricted client-side to parent-child edges whose parent is the watched epic (cheap,
reuses existing infra, pays the "re-walk to confirm membership" cost per fire), or
(ii) a new `TopicEpicChildAdded`-style distinguished event following the
`PlanStepAdded` template (server-resolved, dual-published) — more engine work, but
removes the re-walk and gets JetStream durability for free.

`BeadClosed` (`events.go:95-103`) carries an optional `DecisionAnswer` (chosen
option/response text/responder/cancelled) when the closed bead is a decision — so a
`bead_event` "closed" match can already observe which option was chosen without a
second fetch (GAP-4/ka-0000005470).

### 1.9 How these events actually reach a trigger: NATS listeners vs. always-live SSE

`startSchedulerNATSListeners` (`gardenio/controller/cmd/trigger-engine/main.go:~1473-1512`)
spawns three scheduler-specific NATS subscribers — `DecisionEventListener`
(`nats_decision.go`), `BeadClosedEventListener` (`nats_bead_closed.go:165`, emits only
`"closed"`), `MailEventListener` (`nats_mail.go:121`, emits `"mail_sent"`) — but
**no-ops entirely when `NATS_URL` is unset** (`main.go`: `if natsURL == "" { ...
return }`). These three feed `multiSrc.Engine()` (the same `EntryEngine`
`MultiTriggerSource` reads).

Independent of NATS, `MultiTriggerSource.RegisterSSE`
(`gardenio/controller/internal/bridge/trigger_source_multi.go:93-136`) is wired
**unconditionally at startup** (`main.go:231-232`, alongside `beadSrc.RegisterSSE`)
and subscribes to the full-payload bead topics (`beads.bead.created/updated/closed`)
plus edge topics (dependency/comment/label/follower add+remove, delete, transfer) —
edge events are hydrated via a `GetBead` call before firing (fails open/skips on a
hydration miss, ka-0000001427). **This means a `trigger:multi`-labeled gardener
already gets `bead_event` wakes for created/updated/closed and
`dependency.added`/`removed` today, with zero NATS wiring** — the "missing NATS_URL"
framing undersells what's already live; NATS only adds `decision_resolved`,
`bead_closed` (redundant with SSE's `closed`), and `mail_sent`. `decision_resolved`
wakes for the 7 gardeners in §2 DO require `NATS_URL` (no SSE path publishes decision
resolutions today) — this is the one real NATS dependency for the migration.

### 1.10 The actual blocker for the 7 live gardeners: label, not `triggers[]` content

`ScheduleTriggerSource.LoadTriggers`
(`gardenio/controller/internal/bridge/trigger_source_schedule.go:100-171`) is the
loader for `trigger:schedule`-labeled beads — what all 7 live gardeners in §2 are
labeled today. **Lines 119-132: a `trigger:schedule` bead's `triggers[]` array is
silently ignored** — only `cron` + `conditions` (an older, bead-query-only gate
mechanism, not the `Preflight` expression type §1.2 describes) drive firing; a
warning logs "schedule trigger: triggers[] array ignored — relabel trigger:schedule
-> trigger:multi to fire event wakes" (ka-0000002562). Event-driven evaluation only
happens for beads `MultiTriggerSource` loads (label `trigger:multi`) or
`ScheduleMirrorTriggerSource` loads (label `schedule-mirror`,
`trigger_source_schedule_mirror.go:1-80` — a **different, third** label for
already-migrated dual-write mirror beads left over from the Phase 3b/D10 removal of
`type=schedule` firing; not a path our 7 `trigger:schedule` gardeners are on or can
opt into without becoming mirrors themselves).

**Consequence for Phase 1:** simply populating `fields.triggers` on one of the 7
existing gardener beads (§2) would have **no effect** — the bead must be relabeled
`trigger:schedule` → `trigger:multi` (dropping the `conditions` field's role in favor
of a `Preflight` inside a `cron` trigger entry, per the §1.3 reference shapes) for
`MultiTriggerSource` to load and evaluate it at all. This refines the epic brief:
the blocker is a label/loader mismatch, not solely an absent `triggers[]` value or
missing NATS wiring.

## 2. The 7 live gardeners: epic, cron, recommended wake events

All 7 are `type=trigger`, `kind=data`, `role=gardener`, `action=spawn_agent`, labeled
`trigger:schedule` + `gardener-epic:<epic-id>`, driven purely by a `cron` field and a
`conditions` bead-query preflight (`parent=<epic>`, open/in_progress/blocked children
`> 0`). **`fields.triggers` is `null` on all 7** (confirmed live, 2026-09-20) — none of
them carry the event-wake `triggers[]` array from §1; every fire today is a cron poll
gated only by "does the epic still have open children."

| Gardener (schedule bead) | Target epic | Epic title | Cron | Open children (now) | Lifetime spend | Recommended wake events (Phase 1+) |
|---|---|---|---|---|---|---|
| `gd-2KD60Qc9Gt` | `gd-qbBJGHiGHo` | Spawn escalation pathway for privileged roles (cluster self-modification) | `0 */4 * * *` (every 4h) | 9 | $41.40 | `bead_event` child closed under epic; `decision_resolved` in subtree; (missing today) child-added |
| `gd-yIETbQ4baQ` | `gd-irs5C9BEBi` | CI build speed: make image/test builds much faster (spike, then fixes) | `5 * * * *` (hourly) | 8 | $44.76 | same three |
| `gd-HQQbcD8z1v` | `gd-ljoVmxrW8O` | Cut real calver releases and stop deploying `:latest` | `15 * * * *` (hourly) | 6 | $26.32 | same three |
| `gd-L6JP0RTXYx` | `gd-akt1qKz1Si` | Ziti internet front door and invite-based registration: plan | `30 * * * *` (hourly) | 12 | $47.96 | same three |
| `gd-UyPvgCIT2x` | `gd-Vn8uWTbQ9S` | Move the OpenZiti controller off the Vultr quickstart box into k3s | `45 */4 * * *` (every 4h) | 13 | $51.32 | same three |
| `gd-Czax0vn3N4` | `gd-6fmhiIEiLr` | Taints: implementation plan | `50 * * * *` (hourly) | 29 | $98.77 | same three |
| `gd-HIJuyeZ2BH` | `gd-StII9fmzym` | HA under spot churn: multi-replica critical services, proven by drills | `35 * * * *` (hourly) | 12 | $6.08 | same three |

Notes:

- "Same three" = the epic's target wake set from gd-BYKE0Zi0gh: **(a)** a child bead
  closing under the epic, **(b)** a decision in the epic's subtree resolving, **(c)** a
  new child being added under the epic (the "revived when work is done" case).
  (a) and (b) already have engine support (`bead_event`/`decision_resolved` trigger
  kinds, §1) — migrating these 7 gardeners onto them is a `triggers[]` field edit, no
  code change. (c) has no dedicated bead-event today in the form the trigger engine's
  `bead_event` kind consumes (see §1's finding on `beads.dependency.added` vs.
  `beads.bead.created`) — this is the one gap Phase 1's design needs to close, either by
  wiring `bead_event` to match `beads.dependency.added` restricted to `parent-child`
  edges under the watched epic, or by adding a distinguished event the way
  `beads.plan.step.added` already does for plan steps (§1).
- 5 of the 7 (`yIETbQ4baQ`, `HQQbcD8z1v`, `Czax0vn3N4`, `HIJuyeZ2BH`, and effectively
  `L6JP0RTXYx`/`UyPvgCIT2x` for their older tail) only have run-log history inside the
  last ~24h at the time of this snapshot — gardeners were started at different points
  over the last few days, so "48h of cron-only fires" is not uniformly 48h of *live*
  history for every gardener. Per-gardener fire counts in §3 are reported both for the
  window that actually has data and annotated where a gardener's full history is
  shorter than 48h.
- "Open children (now)" is a live count taken at snapshot time, not a 48h-ago count —
  it is context for how much backlog each gardener is still polling against, not a
  baseline metric itself.

## 3. Baseline: fires, no-op rate, spend per gardener (last 48h)

**Method.** `gb gardener-audit --schedule-id <id> --since 48h --json` gives one row per
gardener-cycle agent bead with `wrapup_present`, `tldr`, `accomplishments`,
`decisions_filed`, `decisions_resolved`, `pull_requests`, and `failed` (no wrap-up
written). Per-run cost comes from `kd list --all-types -t agent -f trigger_id=<id>
--created-after 48h --json` reading `fields.total_cost_usd` on each cycle's agent bead,
joined by `agent_id`. A cycle counts as **useful** if it filed/resolved a decision,
recorded a pull request, or its wrap-up text names a concrete action (spawned crew,
closed a bead, opened a PR, filed an escalation, reflagged a decision); otherwise it is
**no-op** (pure status-check/re-verification, including every cycle that failed to
write a wrap-up at all — reaped-as-stale cycles count as no-op, not useful).

| Gardener | Fires, last 24h | Useful | No-op | Failed (no wrap-up) | Cost, last 24h | Fires, 24–48h ago | Useful | No-op | Cost, 24–48h ago | Fires, 48h total | Useful % (48h) | Cost, 48h total |
|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|
| `gd-2KD60Qc9Gt` (spawn-escalation) | 20 | 5 | 15 | 2 | $15.48 | 10 | 3 | 7 | $2.93 | 30 | 27% | $18.41 |
| `gd-yIETbQ4baQ` (ci-build-speed) | 17 | 12 | 5 | 0 | $10.10 | 0¹ | — | — | — | 17 | 71% | $10.10 |
| `gd-HQQbcD8z1v` (calver-release) | 25 | 18 | 7 | 0 | $14.99 | 0¹ | — | — | — | 25 | 72% | $14.99 |
| `gd-L6JP0RTXYx` (ziti-front-door) | 25 | 9 | 16 | 1 | $18.97 | 10 | 3 | 7 | $6.40 | 35 | 34% | $25.36 |
| `gd-UyPvgCIT2x` (ziti-controller-move) | 22 | 15 | 7 | 0 | $22.42 | 9 | 6 | 3 | $5.30 | 31 | 68% | $27.72 |
| `gd-Czax0vn3N4` (taints-plan) | 17 | 17 | 0 | 0 | $13.54 | 0¹ | — | — | — | 17 | 100% | $13.54 |
| `gd-HIJuyeZ2BH` (ha-spot-churn) | 4 | 3 | 1 | 0 | $1.84 | 0¹ | — | — | — | 4 | 75% | $1.84 |
| **Total** | **130** | **79** | **51** | **3** | **$97.34** | **29** | **12** | **17** | **$14.63** | **159** | **57%** | **$111.97** |

¹ These gardeners' entire cycle history inside the queried window falls in the last 24h
(earliest cycle timestamps: `yIETbQ4baQ` 2026-09-20 04:06 UTC, `HQQbcD8z1v` 2026-09-19
23:04 UTC, `Czax0vn3N4` 2026-09-20 02:50 UTC, `HIJuyeZ2BH` 2026-09-20 17:24 UTC) — not a
measurement gap, just less than 48h of that gardener actually being alive/cron-firing at
snapshot time. Their "48h total" column is therefore really "all fires since start, ≤
24h of history."

**Reading this table:**

- **159 cron fires across 7 gardeners in (up to) 48h**, 57% useful by the heuristic
  above. The two gardeners with the highest fire *count* and lowest usefulness are
  `gd-2KD60Qc9Gt` (30 fires, 27% useful — its target epic's in-lane work finished
  hours ago and it has spent the last ~24h of 4-hourly fires re-verifying a
  steady-state "all closed, nothing to do" epic) and `gd-L6JP0RTXYx` (35 fires, 34%
  useful — hourly cron against a plan that is usually already fully dispatched: most
  cycles are "crew still in flight, nothing new ready, no stalls" health-checks with no
  action taken, per its own wrap-up text).
- `gd-Czax0vn3N4` (taints-plan) is the cleanest case at 100% useful over 17 fires — its
  epic has 29 open children (the largest backlog of the 7) so an hourly cron rarely
  finds nothing to do; it is the gardener event-wake will help *least*.
- `gd-2KD60Qc9Gt` is the clearest case *for* event-wake + dormancy: once its terminal
  goal is met (all in-lane children closed) it should go dormant and be revived only by
  a new child appearing under its epic or a status change on the two out-of-lane
  RBAC/ServiceAccount tasks it is intentionally not touching — instead it is paying for
  a 4-hourly cron fire (~$0.5–0.7/cycle) indefinitely.
- 3 of 159 fires (2%) failed to write a wrap-up at all (reaped as stale or a bare
  `outcome:"working"` still-in-flight cycle counted conservatively as no-op) — a small
  but nonzero tax that event-wake does not fix by itself (that is the trigger-engine
  restart-loop work, gd-egWmqMoXGJ/gd-niQPoeR7ja, tracked separately in this epic).
- Total observed spend across all 7 gardeners in the (partial, up to 48h) window: **$111.97**,
  of which the no-op share (51+17=68 no-op fires out of 159, 43% of fires) is
  disproportionately cheap per-fire (a pure re-verification cycle runs a handful of
  read-only `kd`/`gb` calls and a status comment — well under the ~$0.6–0.9 a
  spawn-and-verify cycle costs) but is not zero: eliminating cron-only no-op fires is a
  spend reduction, not just a noise reduction.

## 4. Summary going into Phase 1+

This table is the **baseline**: 159 cron fires / 57% useful / $111.97 spend across 7
gardeners in the last (up to) 48h, with `triggers[]` null on all of them today. Phase 3
should re-run the identical `gb gardener-audit --schedule-id <id> --since 48h` +
per-run-cost query against the same 7 gardeners (or their successors) after the
event-wake migration and compare fire count, useful %, and spend directly against this
table.
