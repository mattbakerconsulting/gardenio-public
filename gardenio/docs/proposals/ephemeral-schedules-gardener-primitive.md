# Proposal: Ephemeral Schedules as a Scheduled-Gardener Primitive

**Status:** Proposal / RFC
**Author:** gardenio thread agent
**Date:** 2026-04-21
**Tracking bead:** ka-0000006392

## Motivation

Schedule beads today are an infinite cron trigger: they fire on a cron
expression forever, spawning an agent with a static prompt. Each run is blind
— it doesn't know it's the Nth run of M, doesn't know the schedule's end goal,
and the schedule never ends on its own.

Matt's framing: we already have gardeners (orchestrator agents that drive work,
spawn crew, and stop between cycles) — we want schedules to formalize them
into a **scheduled-gardener primitive**: a bounded loop that kicks a gardener at
a task repeatedly until a goal is reached, then auto-terminates. That means
schedules should carry:

1. **Progress context** — how many runs left, how many have fired, what state
   prior runs left behind.
2. **A terminal goal** — a natural-language description of done, plus
   (optionally) a machine-checkable exit condition.
3. **Richer triggers than cron** — e.g. fire a run when a crew's decision is
   resolved, when a bead closes, when a mail arrives.

This doc sketches the data model and agent-facing API to get us there without
rewriting the scheduler.

## What counts as a "run"

This is the load-bearing definition for the whole proposal. A **run** is a
full attempt to make progress toward the terminal goal — not a single Claude
turn, not a single wake-up, not a single pod lifetime.

Concretely, one run consists of:

1. The scheduler (or a non-cron trigger) fires.
2. An agent is spawned with the loop context (run N of M, prior-run wrap-up,
   terminal goal).
3. That agent assesses state, spawns crew as needed, **waits for its crew**,
   **resolves any decisions the crew surfaces** (delegating to the human only
   when the scheduled agent genuinely cannot decide), and produces a wrap-up.
4. The agent calls `gb stop --wrapup '{…}'`. The stop consumes one unit of
   `max_runs` budget and closes the run.

The non-obvious corollary: **waiting is part of the run, not the end of it.**
A scheduled agent that spawns three crew pods and then exits has produced a
partial run and wasted its budget. A scheduled agent that spawns three crew
pods, `gb yield`s on their decisions, resolves them, and then stops has spent
one budget unit on one complete unit of progress — which is the point.

### `gb yield` vs `gb stop`: the one rule

Two primitives, one rule: **`gb yield` keeps the run alive; `gb stop` ends it
and debits the budget.**

| Situation | Use |
|---|---|
| Crew agent raised a decision and you can answer it yourself | Answer the decision, keep working — do NOT yield or stop |
| Crew agent raised a decision that only a human can answer | `gb yield` — run stays open, budget unchanged, you resume when the human resolves |
| You have dispatched crew and are waiting for them to finish | `gb yield` (the pod de-hydrates; you wake on crew mail / decision-resolution / bead-close) |
| You assessed state, nothing actionable, this run is done | `gb stop --wrapup` — costs a run |
| You reached the terminal goal | `gb stop --wrapup '{"goal_reached":true,…}'` — costs a run, auto-closes the schedule |

`gb stop` is the **only** way to consume a run budget unit. Everything else —
yielding, sleeping, waiting for NATS events, being nudged by mail — keeps the
current run open. This is the inverse of the habit gardeners have today
(`gb stop` after every cycle, trust the scheduler to wake them), and the
Scheduled Loop Context prompt needs to make the flip explicit.

### Implications for the scheduler

- `runs_spawned` is bumped on spawn; `runs_completed` is bumped on the stop
  that consumes the run (i.e. when `trackLastAgentStatus` observes the agent
  bead closed with a wrap-up). A yield-and-wake cycle inside one run must not
  double-count.
- Waking an agent from yield is NOT "spawning a run" — it reuses the existing
  agent bead and the existing run budget slot. The scheduler must distinguish
  "fire a new run" (new agent pod, `runs_spawned++`) from "resume a yielded
  run" (wake the existing pod, counters untouched).
- `max_concurrent` on scheduled-gardener schedules should stay at 1, since a run may
  sleep for hours while crew works. Two concurrent runs of the same schedule
  would fight over the same bead state.
- Cron ticks that arrive while a run is still yielding should be dropped (or
  queued at most one deep) rather than spawning a parallel run. This is the
  same `max_concurrent=1` rule enforced at the trigger layer.
- Agent-bead timeout handling needs an escape hatch: a run that has been in
  yield for longer than `schedule.run_timeout` should be reaped and counted as
  a completed (failed) run, so a wedged decision can't silently burn a
  schedule's forward progress.

## Current state (what we have)

- `schedule` beads carry `cron`, `enabled`, `project`, `role`, `prompt`,
  `timezone`, `last_run`, `last_agent_id`, `consecutive_failures`,
  `max_concurrent`, `timeout`, `max_retries`.
- Scheduler (`gardenio/controller/internal/scheduler/scheduler.go`) fires cron,
  spawns an agent, stamps `schedule_id` / `schedule_title` / `schedule_cron`
  onto the agent bead.
- The trigger engine (`trigger_source_schedule.go`) already supports
  cron-plus-pre-flight-conditions on `trigger`-type beads — this is a second
  lane next to schedule beads and is a useful model to borrow from.
- `gb prime` (agent context primer) does **not** read schedule_* fields today.
  Scheduled agents have no idea they're part of a loop.

## Proposed changes

### 1. Add run-budget and goal fields to schedule beads

New fields on `schedule` beads (all optional — absence = legacy unlimited cron):

| Field | Type | Semantics |
|---|---|---|
| `max_runs` | int | After this many successful spawns, auto-close the schedule bead. `0` / unset = unlimited (legacy). |
| `runs_completed` | int | Counter bumped by the scheduler each time `trackLastAgentStatus` sees a closed agent. Distinct from spawn count so we can distinguish "fired 5 times" vs "completed 5 runs". |
| `runs_spawned` | int | Counter bumped each time `spawnScheduledAgent` succeeds. |
| `terminal_goal` | string | Natural-language description of what "done" looks like for this loop. Injected into every run's context. |
| `exit_condition` | JSON | Optional machine-checkable check (reusing the `scheduleCondition` schema already in `trigger_source_schedule.go`). Evaluated before each tick; if true, auto-close. |
| `on_exit` | string | Action when the schedule terminates: `close` (default), `notify_slack`, `spawn_followup`. |

Schema wiring lives in `gardenio/controller/internal/bridge/init.go` (where
schedule fields are already declared). Scheduler `listSchedules` /
`trackLastAgentStatus` / `spawnScheduledAgent` gain parse + persist logic.

### 2. Inject loop context into every spawned agent

In `spawnScheduledAgent` we already pass `schedFields` to `SpawnAgent` — we
extend that map:

```go
schedFields := map[string]string{
    "schedule_id":             sched.ID,
    "schedule_title":          sched.Title,
    "schedule_cron":           sched.Cron,
    "schedule_runs_spawned":   strconv.Itoa(sched.RunsSpawned + 1),
    "schedule_runs_completed": strconv.Itoa(sched.RunsCompleted),
    "schedule_max_runs":       strconv.Itoa(sched.MaxRuns),  // "0" = unlimited
    "schedule_terminal_goal":  sched.TerminalGoal,
    "spawned_by":              "scheduler:" + sched.ID,
}
```

Then `gb prime` gains a new section that renders when any `schedule_*` fields
are set on the agent bead:

```markdown
## Scheduled Loop Context

You are run **3 of 10** of schedule **"Grafana dashboard cleanup"** (ka-abc123).

**Terminal goal:** No open P1/P2 dashboard-drift bugs in project `gardenio`, and
the on-call dashboard renders without errors.

**Prior-run summary (wrap-up of last run):**
> Fixed 2 panel queries. 1 remaining dashboard still fails to render.

**Runs remaining after this one:** 7. If the goal is reached, stop with
`gb stop --wrapup '{"goal_reached":true,"goal_reason":"…"}'` to close the loop.

**Run lifecycle (READ CAREFULLY — this is the opposite of non-scheduled
gardener behavior):**

- `gb stop` **consumes one of your 10 runs.** Use it only when this run is
  complete — meaning your crew has finished, you have resolved their
  decisions, and you have produced a wrap-up.
- `gb yield` **does NOT consume a run.** Use it whenever you are waiting on
  crew, on a human decision, or on an event. The pod de-hydrates; you wake
  when the thing you were waiting for happens. This is how you "finish a
  run" that takes hours.
- If your crew raises a decision that you can answer yourself (approve a
  merge, pick between two implementation options with clear criteria,
  re-prompt them), answer it and keep the run going. Only escalate to a
  human (`gb decision create` then `gb yield`) when you genuinely cannot
  decide.

In short: **a run is an attempt to finish the goal, not a wake-up.** Spawn
crew, yield while they work, resolve what they raise, then stop.
```

The prior-run summary comes from `last_agent_id`'s `wrapup.accomplishments`
(already populated by `gb stop --wrapup`), so scheduled gardeners get a running
narrative without an extra store.

### 3. A `gb schedule` agent-facing CLI

Today only the Slack slash command `/schedule` exists. We add agent-facing
subcommands:

- `gb schedule complete <id> --reason "…"` — close the schedule, stop
  further runs. Intended for an agent that decided the goal is reached.
- `gb schedule extend <id> --runs +5` — give the loop more iterations.
- `gb schedule progress <id> --note "…"` — append a structured progress
  comment without closing (alternative to wrap-up for mid-run updates).

Permission: guarded by bot ACL like other `gb` commands.

### 4. Non-cron triggers (the real unlock)

The current `ScheduleTriggerSource` demonstrates the pattern: a trigger bead
with pre-flight conditions. We generalize the schedule bead itself to support
multiple **trigger modes**, declared via a new `trigger_mode` field:

| `trigger_mode` | Fires when… | Extra fields |
|---|---|---|
| `cron` (default, legacy) | cron matches | `cron`, `timezone` |
| `decision_resolved` | a decision bead is resolved on a matched agent/bead | `watch_project`, `watch_role`, `watch_bead_id` |
| `bead_closed` | a bead matching filters closes | `watch_labels`, `watch_types`, `watch_project` |
| `mail_received` | the bridge's mail watcher sees a new thread | `watch_channel`, `watch_from` |
| `manual` | someone runs `gb schedule fire <id>` | — |

Implementation: the scheduler gains a pluggable `TriggerMode` layer that
produces "should-fire" events alongside cron. For event-driven modes, the
existing NATS subscriber (`internal/subscriber/`) already receives bead
lifecycle events and decision events — we add handlers that match events
against schedules with `trigger_mode=decision_resolved|bead_closed` and
enqueue a fire.

**Why this matters for scheduled gardeners:** the canonical gardener loop is
"run, produce decision, human answers, loop". With
`trigger_mode=decision_resolved` a schedule can sit dormant until a crew
agent's decision is answered, then immediately kick a new run with the
decision context threaded through. That's the interactive scheduled-gardener
behavior we don't have today.

### 5. Exit conditions and auto-termination

Before each tick (in `Reconcile`), evaluate `exit_condition` if set. If it
passes, don't spawn; close the schedule bead with status=closed and comment
"terminated: exit_condition met". Same condition DSL as
`trigger_source_schedule.go`'s `scheduleCondition`:

```json
{
  "type": "bead_query",
  "op": "==",
  "value": "0",
  "types": ["bug"],
  "labels": ["project:gardenio", "severity:p1"]
}
```

After `max_runs` is hit, close with "terminated: max_runs exhausted".

### 6. Agent can self-declare goal reached

Extend the wrap-up schema (`gb stop --wrapup '{…}'`) with:

```json
{
  "accomplishments": "…",
  "goal_reached": true,
  "goal_reason": "All P1 dashboards render clean; no open drift bugs."
}
```

`trackLastAgentStatus` reads this and, if `goal_reached=true`, short-circuits
the loop regardless of `max_runs` or `exit_condition`.

## Migration / backwards compat

- All new fields optional. Absent fields ⇒ legacy cron behavior.
- Existing schedules fire unchanged.
- New `gb prime` section only renders when `schedule_id` is set on the agent
  bead, so non-scheduled agents are unaffected.

## Work breakdown

Epic with children, in suggested order:

1. Add `max_runs`, `runs_spawned`, `runs_completed`, `terminal_goal`,
   `exit_condition`, `on_exit`, `run_timeout` fields; parse/persist in
   scheduler; bump `runs_spawned` on spawn, `runs_completed` on the
   observed stop. (Scheduler-only, no agent changes.)
2. **Yield-vs-stop accounting.** Ensure `runs_spawned` is NOT bumped when a
   yielded agent wakes back up — waking is run continuation, not a new run.
   Add a reaper for runs stuck in yield past `run_timeout`, counted as a
   failed completed run.
3. Extend `schedFields` in `spawnScheduledAgent`; add the `## Scheduled Loop
   Context` block to `gb prime` for scheduled runs; pull prior-run wrap-up
   into the context. Make the `gb yield` / `gb stop` semantics block in
   §2 above **inline** in the rendered prime, because the behavior it
   describes is the inverse of the gardener default.
4. `gb schedule complete|extend|progress` subcommands.
5. `goal_reached` in wrap-up → scheduler auto-close.
6. `exit_condition` evaluation in `Reconcile` (reuse `scheduleCondition`).
7. `trigger_mode=manual` + `gb schedule fire <id>`.
8. `trigger_mode=decision_resolved` (NATS subscriber integration). This is
   the piece that makes "yield while crew works" efficient: instead of
   polling, the yielded agent is woken by the decision-resolved event.
9. `trigger_mode=bead_closed` and `mail_received`.
10. Docs: update `docs/user-guide/10-scheduling-automation.md` with the
    scheduled-gardener section; add a worked example (e.g. "close all P1 dashboard drift
    bugs"). Include a sample loop transcript showing one run spanning
    multiple yields.

Steps 1–6 are the 80% of the value — they give us bounded loops with real
context. 7–9 unlock the non-cron trigger story.

## Validating the decision path

The whole "yield keeps the run alive" story falls apart if the decision
plumbing isn't reliable end-to-end. Before we call this done we need to
prove, on real infrastructure, that:

1. A scheduled gardener spawns crew.
2. Crew raises a `gb decision create` and the parent gardener is able to
   observe it (via NATS `decision_created` or a poll of child bead state).
3. The gardener can either:
   - Resolve the decision itself (via `gb decision report <id>`), OR
   - `gb yield` and be reliably woken when the human resolves it.
4. On resume, the gardener's context includes the chosen option and artifact.
5. `runs_spawned` / `runs_completed` counters match reality across the yield
   cycle (the yield-wake must not count as a new run).
6. The schedule closes cleanly on `max_runs`, `exit_condition`, or
   `goal_reached` — and the pod doesn't auto-respawn on stop (see "Open
   questions → Stop-hook coexistence").

We should instrument the scheduler with structured logs (`schedule.fire`,
`schedule.yield`, `schedule.resume`, `schedule.stop`, `schedule.close`) so
these transitions are visible during validation.

## E2E tests (post-MVP, in the plan)

Not required for MVP, but the implementation epic MUST include a follow-up
step to add claudeless-based E2E scenarios under `gardenio/tests/e2e/claudeless/`:

| Scenario | TOML | What it proves |
|---|---|---|
| `schedule-gardener-happy-path` | fire → spawn → crew + decision (self-resolved) → stop | end-to-end run completes, counters advance by 1 |
| `schedule-gardener-yield-on-human-decision` | fire → spawn → crew raises decision → gardener yields → human resolves → gardener wakes → stops | yield does NOT bump `runs_spawned`; woken gardener sees chosen option |
| `schedule-gardener-max-runs-close` | run N times, last run goal-reached wrap-up | schedule auto-closes on `goal_reached`; further cron ticks are a no-op |
| `schedule-gardener-exit-condition` | exit_condition met before spawn | scheduler closes schedule without spawning a run |
| `schedule-gardener-run-timeout-reaper` | yield past `run_timeout` | reaper fires, counts as a completed (failed) run, schedule continues |
| `schedule-gardener-stop-no-respawn` | scheduled agent `gb stop` | pod does NOT auto-respawn while schedule has remaining runs; next cron fires normally |
| `schedule-decision-resolved-trigger` | schedule with `trigger_mode=decision_resolved` | resolving the watched decision fires a new run; `{{decision_choice}}` is injected |

Because these simulate Claude without API calls, they can run on every PR
and in the nightly E2E lane, catching scheduler regressions that unit tests
miss (pod lifecycle, NATS fan-out, wrap-up parsing, stop-hook interaction).

## Open questions

- Should `runs_completed` count only successful (non-failed) runs, or all
  closed runs? I lean "all closed" with `runs_successful` as a separate
  counter if we need it.
- Do we want a global `max_loop_budget` knob that caps how many concurrent
  ephemeral schedules a single human user can spawn? (Protects against "human
  kicks off 50 gardener loops" foot-guns.)
- For `trigger_mode=decision_resolved`, should the scheduler auto-inject the
  decision's chosen option into the next run's prompt? I think yes, as
  `{{decision_choice}}` and `{{decision_artifact}}` template vars.
- Interaction with `assign_bead`: if a schedule is pinned to a single bead,
  completing the bead probably *should* close the schedule — worth adding as
  an implicit `exit_condition`.
- **Yield budget.** Should we cap how long a single run can stay yielded
  (`run_timeout`, mentioned in §"Implications for the scheduler") or how
  many times one run can yield-and-wake? A pathological run that yields 200
  times on minor crew decisions would still be "one run" by this proposal,
  which is probably fine — but worth a default ceiling to catch infinite
  ping-pong bugs.
- **Stop-hook coexistence.** The current stop-gate hook treats `gb stop`
  without a cleared decision gate as a restart trigger. For scheduled runs
  we want the opposite: stop should be allowed (and debit the run), and the
  pod should NOT restart. The scheduler's new "run" abstraction needs to
  disable or short-circuit the stop-hook restart path when the agent bead
  has a `schedule_id` and the schedule is still within budget. Otherwise a
  scheduled agent's `gb stop` becomes `gb stop` + auto-respawn, which
  quietly burns the whole budget in seconds.
