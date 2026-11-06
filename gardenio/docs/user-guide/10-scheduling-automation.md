# Scheduling Automation

How to author and operate scheduled gardeners — bounded cron loops that drive
a goal to completion, then auto-terminate. This guide covers what is shipped
on `main` today; deferred phases are flagged in [Future work](#future-work).

For the design rationale, see
[`docs/proposals/ephemeral-schedules-gardener-primitive.md`](../proposals/ephemeral-schedules-gardener-primitive.md).
For richer condition-based loops (the `triggers:` / `stop:` / `progress:`
blocks), see
[`docs/gardener-loop-runbook.md`](../gardener-loop-runbook.md).

## What is a schedule bead

A **schedule bead** (`type=schedule`) declares an autonomous cron loop. The
controller's scheduler reads enabled schedule beads each tick, evaluates
their cron expression, and spawns an agent on a fire.

Minimum field set:

| Field | Required | Purpose |
|---|---|---|
| `cron` | yes | 5-field cron expression (e.g., `"7 * * * *"`) |
| `project` | yes | Project label for the spawned agent |
| `prompt` | yes | The prompt the agent receives at spawn |
| `enabled` | implicit | Schedules only fire when `enabled=true` |
| `role` | optional | Role label for the spawned agent |
| `timezone` | optional | Per-schedule timezone (overrides project default) |
| `slack_channel` | optional | Override the bridge's notification channel |
| `assign_bead` | optional | Pin runs to an existing task bead (skip per-run task creation) |

Create with `kd create --type=schedule --title='…' --field cron='…' …`. To
pause or resume all time-based activity from Slack, see
[Pausing everything from Slack](#pausing-everything-from-slack-schedule-onoff).

## Run-budget fields (Phase 1)

A schedule that should not run forever carries a **run budget**. All fields
are optional; their absence is the legacy "unlimited cron" behavior.

| Field | Type | Semantics |
|---|---|---|
| `max_runs` | int (string) | Auto-close the schedule after this many completed runs. `0` / unset = unlimited. |
| `runs_spawned` | int (string) | Counter bumped each time the scheduler successfully spawns a run. |
| `runs_completed` | int (string) | Counter bumped when the scheduler observes the spawned agent close. |
| `terminal_goal` | string | Natural-language description of "done"; injected into every run's `gb prime`. |
| `exit_condition` | JSON | Machine-checkable check evaluated before each tick (see [Phase 6](#exit_condition-evaluation-phase-6)). |
| `on_exit` | string | Parsed but not yet wired beyond `close` (default). Reserved for `notify_slack` / `spawn_followup`. |
| `run_timeout` | duration string | Per-run wall-clock ceiling for yielded runs (see [Phase 2](#yield-vs-stop-and-the-stuck-yield-reaper-phase-2)). |

Counters live on the schedule bead and are written by the bridge trigger
engine (`gardenio/controller/internal/bridge/triggers.go`). Treat them as
read-only from the agent side — use `gb schedule` subcommands to mutate.

## Yield vs stop, and the stuck-yield reaper (Phase 2)

The single load-bearing rule: **`gb yield` keeps the run alive; `gb stop`
ends it and debits the budget.**

| Situation | Use |
|---|---|
| Crew raised a decision you can resolve yourself | Resolve it; keep working — do NOT yield or stop |
| Crew raised a decision only a human can answer | `gb yield` — run stays open, budget unchanged |
| Crew is mid-flight and you are waiting on them | `gb yield` — pod de-hydrates, wakes on event |
| Run finished (crew done, decisions resolved, wrap-up ready) | `gb stop --wrapup '{…}'` — costs one run |
| Terminal goal reached | `gb stop --wrapup '{"goal_reached":true,…}'` — costs one run, auto-closes the schedule |

Bookkeeping consequences enforced by the scheduler:

- `runs_spawned` is bumped exactly once per spawn. Waking a yielded agent is
  **not** a spawn — counters do not move.
- `runs_completed` is bumped when `trackLastAgentStatus` observes the agent
  bead closed (with or without a wrap-up).
- A yielded run that exceeds the schedule's `run_timeout` is **reaped** by
  the scheduler's `reapStuckYields` pass: it closes the agent bead with
  `agent_state=failed`, posts a "Run reaped" comment on the schedule, bumps
  `runs_completed`, and counts the reap as a failure for retry/auto-disable
  tracking. When `run_timeout` is unset, the global `stalereaper` (2× the
  generic `timeout`) is the only safety net.

## Scheduled Loop Context (Phase 3)

When the scheduler spawns a run, it stamps schedule context onto the agent
bead. `gb prime` reads those fields and renders a `## Scheduled Loop
Context` block at the top of the agent's primer. This block is **inline**
and cannot be elided — it inverts the default gardener stop/yield habit, so
every scheduled run sees it verbatim.

The fields the scheduler attaches to the agent bead:

| Field | Source |
|---|---|
| `schedule_id` | Schedule bead ID |
| `schedule_title` | Schedule bead title |
| `schedule_cron` | Schedule's cron expression |
| `schedule_max_runs` | Copied from `max_runs` (`"0"` = unlimited) |
| `schedule_runs_spawned` | Run number for this spawn (1-indexed) |
| `schedule_runs_completed` | Snapshot at spawn time |
| `schedule_terminal_goal` | Copied from `terminal_goal` |
| `schedule_prior_agent_id` | The previous run's agent bead, used to surface its wrap-up |

`gb prime` renders:

- **Run N of M** (or "run N — `max_runs` unlimited" when no budget).
- **Terminal goal** (when set).
- **Prior-run summary**: the `accomplishments` field from the prior agent's
  `wrapup` JSON, quoted.
- **Runs remaining after this one** + the exact `gb stop --wrapup
  '{"goal_reached":true,"goal_reason":"…"}'` invocation to close the loop.
- **Run lifecycle** rules — `gb stop` consumes a run, `gb yield` does not,
  resolve crew decisions yourself when you can.

The implementation lives in
`gardenio/controller/cmd/gb/prime_schedule.go`.

## `gb schedule` subcommands (Phases 4 + 7)

Agent-facing subcommands let a scheduled gardener self-manage its loop.
All are guarded by the standard `gb` ACL.

### `gb schedule complete <id> --reason "…"`

Closes the schedule bead (status=closed) with the reason recorded as a
comment. Use when the gardener has decided the terminal goal is reached but
the wrap-up path (see [Phase 5](#goal_reached-wrap-up-phase-5)) is not
appropriate — for example, when concluding from outside the scheduled run
itself, or when stopping early without burning the current run on a wrap-up.

```bash
gb schedule complete ka-abc123 --reason "P1 dashboard drift queue empty for 3 runs"
```

`--reason` is required. Refuses if the bead is not a schedule, no-ops if
already closed.

### `gb schedule extend <id> --runs +N`

Increments the schedule's `max_runs` by `N` (positive integer; a leading
`+` is allowed for clarity). Lets a gardener that hit its budget but has not
finished give itself more iterations without an operator in the loop.

```bash
gb schedule extend ka-abc123 --runs +5     # max_runs 24 → 29
```

Posts an audit comment recording the actor, prior, new, and delta values.

### `gb schedule progress <id> --note "…"`

Appends a timestamped progress comment to the schedule bead without
closing it. Alternative to a wrap-up for mid-run updates that should be
visible on the schedule's history without ending the run.

```bash
gb schedule progress ka-abc123 --note "Closed 7 of 12 known drift bugs this run"
```

### `gb schedule fire <id> --reason "…"`

Force-spawns a single immediate run, bypassing the cron fire-time check.
Equivalent to one cron tick: spawns the agent, bumps `runs_spawned`,
updates `last_run` + `last_agent_id`, and posts a comment recording the
fire and the `--reason`.

```bash
gb schedule fire ka-abc123 --reason "post-deploy smoke loop kickoff"
```

Refuses when:

- the schedule bead is closed or `enabled != "true"`,
- `runs_spawned >= max_runs > 0` (budget exhausted),
- the schedule's `exit_condition` currently evaluates true.

For `trigger_mode=manual` schedules (see [Phase 7](#trigger_modemanual-phase-7))
this is the **only** way to trigger a run.

### Choosing between them

| Goal | Command |
|---|---|
| Close the loop early, without burning a run on a wrap-up | `gb schedule complete` |
| Got more work to do — extend the budget | `gb schedule extend` |
| Note something for the operator without closing | `gb schedule progress` |
| Trigger an immediate run (manual schedules, or just speed up a cron loop) | `gb schedule fire` |
| Close cleanly from inside a run, having reached the goal | `gb stop --wrapup '{"goal_reached":true,…}'` (see Phase 5) |

## `goal_reached` wrap-up (Phase 5)

A scheduled gardener can self-declare the loop done by including
`goal_reached: true` in its wrap-up JSON:

```bash
gb stop --wrapup '{
  "accomplishments": "Closed last 3 drift bugs; dashboards render clean.",
  "goal_reached": true,
  "goal_reason": "Zero open P1 drift bugs in project gardenio for 2 consecutive runs."
}'
```

When `trackLastAgentStatus` observes the agent bead closed with this
wrap-up, the scheduler:

1. Posts `terminated: goal_reached — <reason>` to the schedule bead.
2. Closes the schedule bead.
3. Logs `schedule auto-closed: goal_reached`.

This short-circuits `max_runs` and `exit_condition` — `goal_reached` is the
strongest signal a gardener can emit. `goal_reason` is optional but recommended
for the audit trail.

## `exit_condition` evaluation (Phase 6)

`exit_condition` is a JSON-encoded machine-checkable check that the
scheduler evaluates **before each tick**. When it passes, the scheduler
auto-closes the schedule bead with `terminated: exit_condition met` rather
than spawning a new run.

The condition DSL is shared with the trigger engine
(`gardenio/controller/internal/scheduler/condition.go`). The most common
shape is a bead-count query:

```json
{
  "type": "bead_query",
  "op": "==",
  "value": "0",
  "types": ["bug"],
  "labels": ["project:gardenio", "severity:p1"]
}
```

Failure modes are deliberately fail-open: malformed JSON or unknown
condition types log a warning and the schedule continues to fire. Authors
should validate their condition before relying on it to terminate a loop —
in practice, pair `exit_condition` with a finite `max_runs` so a typoed
condition cannot wedge a loop forever.

## `trigger_mode=manual` (Phase 7)

A schedule with `trigger_mode=manual` is dormant: the scheduler skips it on
every cron tick. The only way to spawn a run is `gb schedule fire <id>
--reason "…"`.

Use cases:

- Operator-triggered loops where each run is intentional (release rollouts,
  one-shot remediation gardeners).
- Loops driven by external events that an outside system translates into
  `gb schedule fire` calls.

`trigger_mode=cron` (or unset) is the default and preserves legacy
behavior.

## Worked example: dashboard-drift cleanup gardener

A scheduled gardener that aims to close all P1 dashboard-drift bugs in
project `gardenio`, runs hourly, and exits when zero remain or after 24
runs.

### 1. Create the schedule

```bash
kd create --type=schedule \
  --title="Dashboard drift cleanup gardener" \
  --label project:gardenio \
  --field cron='17 * * * *' \
  --field project=gardenio \
  --field role=gardener \
  --field enabled=true \
  --field timezone=UTC \
  --field max_runs=24 \
  --field run_timeout=2h \
  --field terminal_goal='No open bugs labeled severity:p1 + area:dashboards in project gardenio.' \
  --field exit_condition='{"type":"bead_query","op":"==","value":"0","types":["bug"],"labels":["project:gardenio","severity:p1","area:dashboards"]}' \
  --field prompt='You are the dashboard-drift cleanup gardener. Each run: triage open P1 dashboard bugs, spawn a crew agent per fix, yield while they work, resolve any decisions you can, and stop when this run has nothing more to drive forward.'
```

### 2. What the gardener does each cycle

When the cron tick fires (every hour at `:17`), the scheduler:

1. Evaluates `exit_condition`. If zero matching bugs, it closes the
   schedule with `terminated: exit_condition met` and stops.
2. Otherwise spawns an agent with `schedule_*` fields stamped on the agent
   bead.

The agent boots, `gb prime` injects the **Scheduled Loop Context** with
the prior-run wrap-up. The gardener then:

1. `kd list --type=bug --label severity:p1 --label area:dashboards
   --status=open` — find work.
2. For each bug, `gb spawn --bead <id> --role crew` — dispatch a crew
   agent.
3. `gb stop --wrapup` once crew is dispatched. The pod is respawned on
   the next scheduled run, which reviews crew output. (`gb yield` is
   ONLY for a decision you own that a human must resolve before you can
   continue — it blocks on that decision and errors if you have none, so
   do not yield merely to wait on crew.)
4. On the next run: review crew output. If a crew agent raised a decision
   the gardener can answer (e.g., "approve the MR" with clear criteria),
   use `gb decision report <id>` and keep going.
5. When all dispatched crew have closed and there are no actionable
   decisions left, `gb stop --wrapup '{"accomplishments":"Closed bugs A,
   B, C; bug D in review."}'` — costs one of the 24 runs.

### 3. How the run completes

The scheduler observes the agent bead closed and bumps `runs_completed`
(say, 1 → 2). The next cron tick fires another run an hour later, with
the prior-run accomplishments visible in the new run's primer.

### 4. How the schedule terminates

Whichever happens first:

- **Goal reached early.** A run finds zero matching bugs and stops with
  `gb stop --wrapup '{"goal_reached":true,"goal_reason":"All P1
  dashboard drift cleared for 2 consecutive runs."}'`. The scheduler
  closes the schedule with `terminated: goal_reached`.
- **`exit_condition` passes between runs.** A drift bug closes via
  another path; on the next tick the scheduler evaluates the condition,
  sees zero, and closes with `terminated: exit_condition met` without
  spawning a run.
- **Budget exhausted.** All 24 runs complete without clearing the queue.
  After the last run closes, the scheduler closes the schedule with
  `terminated: max_runs exhausted`. An operator may decide to extend the
  budget with `gb schedule extend kd-… --runs +12` or revisit the goal.
- **Run wedged in yield.** A run yields on a decision that never
  resolves. After 2 hours (`run_timeout`), `reapStuckYields` closes the
  agent bead, bumps `runs_completed`, posts a "Run reaped" comment, and
  the loop continues normally on the next cron tick.

## Pausing everything from Slack (`/schedule on|off`)

`/schedule` is the operator kill-switch for **all time-based activity** — both
cron-fired schedule triggers and every gardener loop. It is deliberately scoped
to time/loop-based firing: event-driven GitLab and Jira triggers are **not**
affected (those are managed by `/trigger`).

| Command | Effect |
|---|---|
| `/schedule off` | Opens the `scheduler-pause` gate (silences `trigger:schedule` + `schedule-mirror` cron sources) **and** the `gardeners-pause` gate (halts all gardeners). GitLab/Jira keep firing. |
| `/schedule on` | Closes both gates. Each schedule/gardener resumes on its next evaluation with its individual `enabled` state intact. |
| `/schedule` | Shows the current pause state of both gates plus a count of affected schedules and gardeners. |
| `/schedule off <project>` | Sets `schedules_enabled=false` on that project bead only (per-project pause): that project's `trigger:schedule` / `schedule-mirror` triggers and its gardeners, scheduled or event-mode (`trigger:multi`), stop firing. A land's steward is not affected. Other projects are untouched. |
| `/schedule on <project>` | Sets `schedules_enabled=true` on that project bead. |

`/schedule off` never modifies individual schedule or gardener beads — it only
opens the two well-known gate beads the engine already honors
(`scheduler-pause` in `internal/bridge/triggers.go`, `gardeners-pause` in
`internal/bridge/gardener_evaluator.go`). This means `/schedule on` restores the
fleet to exactly the state it was in before the pause. The command is wired in
`internal/bridge/bot_schedule_cmd.go` and gated behind the `slash_command`
capability.

> **`/schedule` vs `/trigger`:** `/schedule` pauses only time-based firing
> (crons + gardeners). `/trigger off` is the broader hammer — it pauses the
> *entire* trigger engine, GitLab and Jira included.

## Future work

Phases 8 and 9 of the proposal — non-cron triggers driven by NATS events
— are deferred. They are NOT available on `main`:

- `trigger_mode=decision_resolved` — fire when a watched decision bead is
  resolved.
- `trigger_mode=bead_closed` — fire when a bead matching filters closes.
- `trigger_mode=mail_received` — fire when the bridge's mail watcher sees
  a new thread.

Until those land, schedules can approximate event-driven behavior with
short cron intervals + an `exit_condition` that checks for the watched
state. Authors who need true event-driven gardeners today should consider
the richer `triggers:` / `stop:` block model documented in
[`docs/gardener-loop-runbook.md`](../gardener-loop-runbook.md), which
already supports condition-based firing alongside cron.

The `on_exit` field is currently parsed but only the default `close`
behavior is wired; `notify_slack` and `spawn_followup` are reserved for
future work.
