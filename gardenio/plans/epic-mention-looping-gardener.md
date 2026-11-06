# Plan: Gardener lifecycle as conditions — trigger, stop, progress

**Status:** Plan-of-record — pending operator decision (revised twice
2026-04-28: first per "less mentions, more loop+triggering"; this revision
per "prioritize rich trigger, stop, and progress conditions for planning,
emphasize or eliminate other stuff").
**Tracking bead:** ka-0000002674
**Builds on:**
- [`docs/proposals/ephemeral-schedules-gardener-primitive.md`](../docs/proposals/ephemeral-schedules-gardener-primitive.md)
  — `max_runs`, `terminal_goal`, `exit_condition`, `run_timeout` shipped in
  the scheduled-gardener primitive (Phases 1–7).
- [`docs/triggers.md`](../docs/triggers.md) — the `trigger:*` engine, the
  `scheduleCondition` DSL (`ready_queue`, `active_agents`, `gate`,
  `bead_query`), and per-source dedup/cooldown semantics.

## Frame

Gardeners are loops. A loop is fully described by three families of
**conditions**:

| Family | Question it answers | Today's primitives | Gap |
|---|---|---|---|
| **Trigger** | What fires a run? | `cron`, `manual`, preflight `conditions[]` (`ready_queue`/`active_agents`/`gate`/`bead_query`), `trigger:bead`, `trigger:slack-reaction`, etc. | No composition between sources; no per-trigger debounce/throttle; no event payload threading into the run |
| **Stop** | When does this run end? When does the loop end? | `gb stop`, `gb yield`, `max_runs`, `exit_condition`, `goal_reached` wrap-up, `run_timeout`, `consecutive_failures` | Single-condition slot for loop-stop; no expression of "stop on any-of"; no wall-clock cap; no "first child closes ⇒ goal" |
| **Progress** | Did this run move us forward? Is the loop wedged? | None. `runs_completed` counts spawns, not progress. Wrap-up `accomplishments` is human-readable narrative. | No machine-checkable progress, no wedge detection, no progress-driven exit, no "show progress in next prime" |

This plan **prioritizes the conditions DSL** — the schemas, the engine
semantics, and the composition rules — because everything else (the
epic-gardener product, mention entrypoints, decision-resolution policy)
is *downstream of* having the right primitives. The original feature
request ("@gardenio <epic-id> spawns a looping gardener") becomes a worked
example at the end of this plan, not the centerpiece.

Out of scope for this plan: mention-text parsing rules, decision
self-resolve policy, gardener naming/uniqueness ergonomics. They will be
re-litigated in a follow-up bead once the conditions primitives are
landed and the epic gardener is the natural test case for them.

## 1. Trigger conditions

### 1.1 Today's shape

Two trigger surfaces exist:

- **`type=schedule` bead** with `trigger_mode = cron | manual`. Cron-only
  with no preflight conditions. The `scheduler` package owns evaluation.
- **`type=trigger` bead** with `labels=[trigger:<source>]` for sources
  `jira | gitlab | webhook | bead | account | schedule | slack-reaction`.
  Per-source preflight via `conditions[]` (cron flavor) or per-source
  `event_types`+`conditions[]` (bead flavor). Each source has its own
  dedup key, its own cooldown, its own catch-up logic.

Both surfaces fire `spawn_agent` (and others). The two surfaces don't
talk to each other: a `type=schedule` cannot wake on a bead-close event,
and a `type=trigger` cannot carry the `max_runs`/`terminal_goal`
scheduled-gardener context.

### 1.2 Rich trigger schema (proposed)

Generalize the schedule bead's "what fires me" surface from a single
mode to a **list of trigger sources, each with its own condition
expression**. The schedule bead gains a single new field:

```yaml
triggers: |
  [
    {
      "kind": "cron",
      "cron": "*/30 * * * *",
      "timezone": "America/Los_Angeles",
      "preflight": {"all": [
        {"type":"bead_query","op":">","value":"0",
         "types":["task","bug"],"parent":"{{assign_bead}}",
         "status":["open","in_progress"]}
      ]}
    },
    {
      "kind": "bead_event",
      "events": ["closed", "updated"],
      "match": {"all": [
        {"field":"parent","op":"==","value":"{{assign_bead}}"}
      ]},
      "throttle": "60s"
    },
    {
      "kind": "decision_resolved",
      "match": {"all": [
        {"field":"raised_by_agent.task_id","op":"in_subtree_of",
         "value":"{{assign_bead}}"}
      ]}
    },
    {
      "kind": "manual",
      "label": "operator-fire"
    }
  ]
```

Field-by-field:

| Field | Type | Notes |
|---|---|---|
| `kind` | enum | `cron \| bead_event \| decision_resolved \| mail_received \| webhook \| manual \| slack_reaction`. One per trigger entry. |
| `preflight` | condition expression (see §4.1) | Evaluated against the daemon at fire time. If false, the trigger source dropped that fire (no run spawned, dedup key NOT marked seen). |
| `match` | condition expression | Evaluated against the *event payload* (for event-driven kinds: bead, decision, mail, webhook, reaction). Different from preflight: preflight asks the world a question; match asks the event a question. |
| `throttle` | duration | Minimum wall-clock between two fires from this trigger entry. Drops events inside the window (last-write-wins on the dropped events; payload from the most recent retained for next fire). |
| `debounce` | duration | Coalesce a burst of events in this window into a single fire after the burst settles. |
| `events` / `cron` / `timezone` / `label` / `emoji` / `token` | per-kind | Same as today's per-source fields. |

Three new things this gets us:

1. **Composition of fire reasons.** A gardener can run on a 30-min cron
   floor *and* react instantly when one of its children closes *and*
   re-run when a crew decision resolves — all from one schedule bead.
   Today this requires three separate beads with no shared run budget.
2. **Match against event payload.** `bead_event.match` gates the fire on
   facts about the bead that *caused* the event. The closing of an
   unrelated bead is not a trigger; the closing of one of *my children*
   is. This was previously possible only at the per-source-bead level
   (via `conditions[]` on `trigger:bead`) and not on schedules.
3. **Throttle/debounce per trigger entry.** A burst of 50 child updates
   in 5 seconds debounces into one wakeup, not 50. A 30-second cron
   firing while a `bead_event` already fired 10 seconds ago is throttled
   out.

### 1.3 Trigger semantics

- **Fan-in.** Any one trigger entry firing produces one fire. The
  scheduler's `max_concurrent` (default 1 for gardener schedules) means
  only one run lives at a time; subsequent fires queue up to one
  deep, the rest are dropped (and dedup-key-marked-seen so they don't
  re-fire on restart).
- **Yield-aware fire.** If a yielded run exists, a fire wakes the
  yielded run (re-uses the agent bead, does NOT bump `runs_spawned`)
  rather than spawning a parallel pod. This is the `decision_resolved`
  pathway from the primitive proposal §4 generalized to all event-kind
  triggers.
- **Catch-up on restart.** Each trigger entry uses the existing
  per-source catch-up: cron uses `last_run`, bead/decision/mail use
  the audit beads with `trigger:<id>` labels.

### 1.4 What changes today

- **Schema.** New optional `triggers` field on `type=schedule` bead
  (JSON array). When present, takes precedence over the legacy
  `trigger_mode`/`cron`/`timezone` fields. Legacy fields keep working
  unchanged when `triggers` is empty.
- **Scheduler engine.** Refactor `spawnScheduledAgent` to dispatch
  through a `TriggerEngine`-style fan-in instead of a single
  cron-or-manual switch. Reuse the existing `scheduler.Condition`
  evaluator for `preflight` and a new payload-aware evaluator for
  `match`.
- **`gb prime` Scheduled Loop Context** gains a "Last fire trigger"
  line ("woken by `decision_resolved` on ka-DEC1 at 13:42") so the
  gardener knows *why* this run is happening.

## 2. Stop conditions

### 2.1 Today's shape

Loop-stop is wired through six independent mechanisms:

| Mechanism | Owner | Effect |
|---|---|---|
| `gb stop` (no flags) | agent | Ends current run, debits 1 of `max_runs` |
| `gb stop --wrapup '{"goal_reached":true}'` | agent | Ends run + auto-closes schedule (highest priority) |
| `max_runs` | scheduler | When `runs_completed >= max_runs`, scheduler closes schedule |
| `exit_condition` (single `bead_query`-shaped JSON) | scheduler | Evaluated before each fire; if true, close schedule |
| `consecutive_failures >= max_retries` (default 3) | scheduler | Auto-disable schedule (`enabled=false`) |
| `run_timeout` reap | scheduler | Yielded run past timeout closes as failed; loop continues |

The expressivity gap: only one `exit_condition` slot, only one shape
(`scheduler.Condition`), no wall-clock cap, no "stop on any-of",
no "soft stop after current run finishes vs hard stop now",
no "first specific child closes ⇒ goal".

### 2.2 Rich stop schema (proposed)

Three classes of stop, each with its own slot. Any stop firing is
sufficient to end the loop.

```yaml
stop:
  loop_end:                    # close the schedule bead, no more runs
    any:
      - {kind: "max_runs"}     # legacy: runs_completed >= max_runs
      - {kind: "wall_clock", duration: "72h"}  # 3 days from first run
      - {kind: "bead_query", op: "==", value: "0",
         types: ["task","bug","epic"],
         parent: "{{assign_bead}}",
         status: ["open","in_progress","blocked"]}
      - {kind: "specific_bead_closed", bead_id: "{{assign_bead}}"}
      - {kind: "no_progress_for", runs: 3}     # progress-aware (see §3)
      - {kind: "manual", reason_field: "operator_close_reason"}

  run_end:                     # end current run (debit budget, keep loop alive)
    any:
      - {kind: "yielded_then_woken_then_idle"} # default: standard stop
      - {kind: "wall_clock", duration: "30m"}  # max single-run length
      - {kind: "no_actionable_state"}          # progress-aware: nothing to do

  hard_stop:                   # immediate, kill the run pod, close schedule
    any:
      - {kind: "manual_force", label: "force-close"}
      - {kind: "consecutive_failures", value: 5}
      - {kind: "wall_clock", duration: "168h"} # 1 week absolute backstop

  on_loop_end: "close"         # close | notify_slack | spawn_followup
```

Field-by-field:

| Slot | When evaluated | Effect |
|---|---|---|
| `loop_end.any` | Before each fire AND at end of each run | Close the schedule cleanly. `on_loop_end` chooses what to do after closure. |
| `run_end.any` | When the agent calls `gb stop` (advisory: the agent is responsible for noticing `run_end` and stopping). The scheduler also enforces `wall_clock` here as a hard timeout. | End the run, debit `max_runs`, fire next trigger normally. |
| `hard_stop.any` | Continuously by the scheduler / reaper | Kill the run pod (if any), close schedule with `terminated: hard_stop:<kind>`. Skips `on_loop_end` (no graceful followup). |

Stop-condition kinds:

| `kind` | Args | Meaning |
|---|---|---|
| `max_runs` | — | `runs_completed >= max_runs` (0 = unlimited) |
| `wall_clock` | `duration` | Time since schedule's `created_at` (or `first_run_at`) exceeds duration |
| `bead_query` | same as `scheduler.Condition.bead_query` | Existing `exit_condition` shape; `parent` filter newly supported (see T2) |
| `specific_bead_closed` | `bead_id` | The named bead has `status=closed` |
| `gate_present` / `gate_absent` | `value` | Re-uses gate semantics |
| `no_progress_for` | `runs` int OR `duration` | Progress-aware (§3); requires progress instrumentation |
| `consecutive_failures` | `value` int | `consecutive_failures >= value` |
| `manual` | `reason_field` (optional) | `gb schedule complete <id> --reason ...` sets a field; this kind matches its presence |
| `manual_force` | `label` | `gb schedule force-close <id>` for hard_stop |
| `yielded_then_woken_then_idle` | — | Standard agent decided "no actionable state, this run is done" via wrap-up |
| `no_actionable_state` | — | Progress-aware shortcut; aliases the gardener's playbook step "if no in-flight crew and no decisions, stop the run" |

### 2.3 Composition rules

- **`any:` only.** No `all:` for stop slots — semantically nonsensical
  ("close only when ALL of these are true" is a wedge waiting to
  happen; if you mean it, AND the conditions inside one entry of
  `any:` via the condition-expression DSL).
- **Precedence.** `hard_stop` wins. Then `loop_end` at fire time wins
  over a fresh fire. Then `run_end` is evaluated against the
  agent's state at `gb stop`. Then a normal `gb stop` debits as today.
- **`goal_reached` wrap-up.** Stays as the agent's "I unilaterally
  declare done" override; it is not in the `stop:` block because it
  isn't a *condition* — it's an explicit signal. Equivalent to
  `loop_end: {any: [{kind: "agent_declared_goal"}]}` with the agent
  controlling it. We could harmonize later; not now.

### 2.4 What changes today

- **Schema.** New optional `stop` field (JSON object) on
  `type=schedule` bead. When present, supersedes legacy `max_runs` /
  `exit_condition` / `run_timeout`. Legacy fields keep working
  unchanged when `stop` is empty (compose by lowering each legacy
  field into one entry of `loop_end.any` or `run_end.any` at parse
  time).
- **Scheduler engine.** New `StopEvaluator` evaluates `loop_end` /
  `hard_stop` slots on every reconcile tick and after every `gb stop`.
  `run_end.wall_clock` is the new piece (kills the pod if a run
  exceeds duration even without yielding; covers the "gardener stuck
  in a long Claude turn" case that `run_timeout` does NOT cover today
  because `run_timeout` only reaps yielded runs).

## 3. Progress conditions (the genuinely new piece)

### 3.1 Problem

Today nothing instruments "did this run make progress?" The scheduler
counts `runs_completed` (spawns), `consecutive_failures` (failed
agents), and reads `accomplishments` strings out of wrap-ups. None of
those tell us:

- Was this run *useful*? (Did it close any children, resolve any
  decisions, merge any MRs, dispatch any crew?)
- Is the loop *wedged*? (3 runs in a row of "spawned, yielded, woken,
  did nothing useful, stopped".)
- How much *forward progress* is left?

Without progress instrumentation, `no_progress_for` (§2.2) is
unimplementable, and the operator has to read every run's wrap-up to
spot a wedge.

### 3.2 Rich progress schema (proposed)

```yaml
progress:
  events:
    - {kind: "child_closed",       parent: "{{assign_bead}}"}
    - {kind: "decision_resolved",  raised_by_agent_subtree_of: "{{assign_bead}}"}
    - {kind: "mr_merged",          author_label: "gardener:{{schedule_id}}"}
    - {kind: "crew_dispatched",    role: "crew", parent: "{{assign_bead}}"}
    - {kind: "wrapup_field",       field: "progress_score", op: ">=", value: "1"}

  windows:
    - {name: "this_run", scope: "since_run_start"}
    - {name: "last_3_runs", scope: "last_n_runs", n: 3}
    - {name: "last_24h", scope: "since", duration: "24h"}

  surface:
    in_prime: true               # render in `gb prime` Progress block
    in_squawk_on_loop_end: true  # squawk a summary when the loop ends
    in_wrapup: true              # require gardener to fill `progress_score` field
```

Three pieces:

1. **Events.** The list of things that count as progress. Generic
   shapes (bead transitions, decision transitions, MR transitions,
   wrap-up fields) — extensible. Each event is matched against the
   daemon's history within the current window.
2. **Windows.** Named time / run scopes against which progress is
   reported. `this_run`, `last_3_runs`, `last_24h` are sensible
   defaults; operators can add more.
3. **Surface.** Where progress shows up: the prime block on the next
   run, the loop-end squawk, and a required `progress_score` field
   in wrap-up so the gardener self-attests.

### 3.3 The Progress block in `gb prime`

When `progress.surface.in_prime: true`, `gb prime` renders:

```markdown
## Loop Progress

Window `this_run` (since 2026-04-28 13:42):
  • crew_dispatched   ×1   (ka-aaa @ 13:43)
  • child_closed      ×0
  • decision_resolved ×0
  • mr_merged         ×0
  Net: spawned 1 crew. No closes/merges yet.

Window `last_3_runs`:
  • child_closed      ×2   (most recent: ka-eee @ 09:17)
  • decision_resolved ×1
  • mr_merged         ×1
  • crew_dispatched   ×4
  Trend: making progress.

Window `last_24h`:
  • child_closed      ×3 of 7 ⇒ 43% drained
  Estimated runs remaining at current rate: ~6.
```

The gardener reads this at the top of each run. It also feeds the
`no_progress_for` stop condition: "if window `last_3_runs` has zero
events of kind `child_closed | mr_merged`, fire `loop_end`".

### 3.4 The `progress_score` wrap-up field

Gardeners must fill `progress_score` (integer ≥ 0) and `progress_summary`
(short string) in their `gb stop --wrapup`. This is enforced by `gb
stop` rejecting wrap-ups that don't have these fields when the
schedule's `progress.surface.in_wrapup: true`.

Conventions:

- `0` — this run did nothing material (yielded, woke, nothing actionable,
  stopped to wait for next cron tick).
- `1` — incremental progress (dispatched a crew, resolved one decision,
  pinged one stale escalation).
- `≥2` — substantive progress (closed a child, merged an MR, completed
  a milestone).

`no_progress_for: {runs: 3}` triggers when the last 3 closed runs all
have `progress_score == 0`.

### 3.5 Why progress is the load-bearing addition

Trigger and stop are both extensions of conditions we already have.
Progress is *new*. It does three things at once:

- **Drives auto-close on wedge.** `no_progress_for` saves operator
  attention from runaway loops more reliably than `max_runs`, which
  catches only the exhaustion case, not the wedge case.
- **Feeds the gardener's playbook.** A gardener that sees "0 progress
  in last 3 runs" can self-escalate (`gb decision create` to operator
  asking "should I close?") instead of grinding the budget.
- **Replaces narrative wrap-ups with structured data.** Aggregated
  across schedules, progress events feed reporting (`gb gardener audit`,
  beads3d, future dashboards) — narrative `accomplishments` strings
  cannot.

## 4. Engine semantics & shared DSL

### 4.1 Condition expression DSL (shared by trigger preflight,
trigger match, stop bead_query, progress event matching)

A single recursive expression schema, used everywhere:

```yaml
# Leaf
{type: "bead_query", op: "==", value: "0", parent: "{{assign_bead}}",
 types: ["task"], status: ["open"]}

# Boolean composition
{all: [<expr>, <expr>, ...]}    # AND
{any: [<expr>, <expr>, ...]}    # OR
{not: <expr>}                   # NOT

# Field reference (for trigger match against event payload)
{field: "label:cycle:failed", op: "present"}
{field: "parent", op: "==", value: "{{assign_bead}}"}
{field: "raised_by_agent.task_id", op: "in_subtree_of",
 value: "{{assign_bead}}"}
```

Leaf shapes:

| Shape | Use | Where evaluated |
|---|---|---|
| `bead_query` | "How many beads match X?" | Daemon-side count query (preflight, stop) |
| `field/op/value` | "Does this event/payload field match?" | In-process against trigger event payload (trigger match), bead state (bead-event match), decision state (decision-resolved match) |
| `gate_present` / `gate_absent` | "Does a gate bead by this title exist?" | Daemon (preflight, stop) |
| `ready_queue` / `active_agents` | Legacy aliases | Lower to `bead_query` at parse time |

This is the same DSL used by `scheduler.Condition` today, generalized
with `all/any/not` composition and a `field` leaf for event-payload
matching. The daemon already supports the bead-side queries; the
expansion is mostly schema and parser.

### 4.2 Template variables

Every condition expression and every trigger entry's strings are
template-expanded at evaluation time with:

| Variable | Source |
|---|---|
| `{{schedule_id}}`, `{{schedule_title}}` | The schedule bead |
| `{{assign_bead}}` | `schedule.assign_bead` field |
| `{{run_number}}`, `{{runs_remaining}}` | Scheduler counters |
| `{{event.<path>}}` | Event payload (only inside `match` expressions) |
| `{{progress.<window>.<kind>}}` | Progress counters (only inside `stop` and `match` expressions) |

This unlocks "fire on bead_event matching parent={{assign_bead}}"
without hand-editing the schedule bead per consumer.

### 4.3 Backwards compatibility

Every legacy schedule field has a one-line lowering:

| Legacy field | Lowered to |
|---|---|
| `cron`, `timezone`, `trigger_mode=cron` | `triggers: [{kind: "cron", cron, timezone}]` |
| `trigger_mode=manual` | `triggers: [{kind: "manual"}]` |
| `max_runs` | `stop.loop_end.any: [..., {kind: "max_runs"}]` |
| `exit_condition` | `stop.loop_end.any: [..., {kind: "bead_query", ...}]` |
| `run_timeout` | `stop.run_end.any: [..., {kind: "wall_clock", duration: <run_timeout>, when: "yielded"}]` |
| `consecutive_failures` / `max_retries` | `stop.hard_stop.any: [{kind: "consecutive_failures", value: <max_retries>}]` |

The lowering happens in the scheduler at parse time; existing schedule
beads need no migration. New schedules can use either surface; mixing
is permitted (legacy fields plus a `stop` block compose with `any:`).

## 5. Worked examples

### 5.1 Epic gardener (the original feature request)

```yaml
type: schedule
title: "Gardener for ka-0000007308 (Slack ratings v2)"
fields:
  enabled: "true"
  role: gardener
  project: gardenio
  assign_bead: ka-0000007308
  prompt: "<gardener playbook prompt — see §6.1>"

  triggers: |
    [
      {"kind":"cron","cron":"7,37 * * * *",
       "timezone":"America/Los_Angeles",
       "preflight":{"all":[
         {"type":"bead_query","op":">","value":"0",
          "types":["task","bug","epic"],
          "parent":"{{assign_bead}}",
          "status":["open","in_progress","blocked"]}
       ]}},
      {"kind":"bead_event","events":["closed","updated"],
       "match":{"all":[{"field":"parent","op":"==","value":"{{assign_bead}}"}]},
       "throttle":"60s"},
      {"kind":"decision_resolved",
       "match":{"all":[
         {"field":"raised_by_agent.task_id",
          "op":"in_subtree_of","value":"{{assign_bead}}"}
       ]}},
      {"kind":"manual","label":"operator-fire"}
    ]

  stop: |
    {
      "loop_end": {"any": [
        {"kind":"bead_query","op":"==","value":"0",
         "types":["task","bug","epic"],
         "parent":"{{assign_bead}}",
         "status":["open","in_progress","blocked"]},
        {"kind":"specific_bead_closed","bead_id":"{{assign_bead}}"},
        {"kind":"max_runs"},
        {"kind":"no_progress_for","runs":3},
        {"kind":"wall_clock","duration":"168h"}
      ]},
      "run_end": {"any": [
        {"kind":"yielded_then_woken_then_idle"},
        {"kind":"no_actionable_state"},
        {"kind":"wall_clock","duration":"30m"}
      ]},
      "hard_stop": {"any": [
        {"kind":"consecutive_failures","value":5}
      ]}
    }

  progress: |
    {
      "events": [
        {"kind":"child_closed","parent":"{{assign_bead}}"},
        {"kind":"decision_resolved",
         "raised_by_agent_subtree_of":"{{assign_bead}}"},
        {"kind":"mr_merged",
         "author_label":"gardener:{{schedule_id}}"},
        {"kind":"crew_dispatched","role":"crew",
         "parent":"{{assign_bead}}"}
      ],
      "windows": [
        {"name":"this_run","scope":"since_run_start"},
        {"name":"last_3_runs","scope":"last_n_runs","n":3}
      ],
      "surface": {"in_prime":true,"in_squawk_on_loop_end":true,
                  "in_wrapup":true}
    }

  max_runs: "20"
  terminal_goal: "All children of ka-0000007308 are closed."
```

The Slack `@gardenio <epic-id>` entrypoint becomes a 30-line shell-out
that creates this schedule. The "looping gardener" feature reduces to
the conditions block above.

### 5.2 Cycle-watchdog (existing use case, demonstrates fan-in)

```yaml
type: schedule
fields:
  triggers: |
    [
      {"kind":"bead_event","events":["closed"],
       "match":{"all":[
         {"field":"type","op":"==","value":"task"},
         {"field":"label:cycle:failed","op":"present"}
       ]}}
    ]
  stop: |
    {"loop_end": {"any": [
      {"kind":"manual"},
      {"kind":"max_runs"}
    ]}}
```

Pure event-driven loop, no cron. Today this requires a `trigger:bead`
bead that can't share a `max_runs` budget with anything; under this
plan it lives on the same primitive as cron-driven loops.

### 5.3 Read-queue drainer (cron with rich preflight)

```yaml
type: schedule
fields:
  triggers: |
    [
      {"kind":"cron","cron":"*/10 * * * *",
       "preflight":{"all":[
         {"type":"bead_query","op":">","value":"0",
          "types":["task"],"status":["open"],
          "labels":["project:gardenio"]},
         {"any":[
           {"type":"active_agents","op":"<","value":"3","role":"crew"},
           {"type":"gate","op":"absent","value":"deploy-freeze"}
         ]}
       ]}}
    ]
  stop: |
    {"loop_end": {"any": [
      {"kind":"bead_query","op":"==","value":"0",
       "types":["task"],"status":["open"],
       "labels":["project:gardenio"]},
      {"kind":"no_progress_for","duration":"4h"},
      {"kind":"wall_clock","duration":"168h"}
    ]}}
```

Demonstrates `any:`/`all:` composition and the new `gate` leaf used in
preflight (deploy-freeze gate gates work).

## 6. Implementation phases

Sized so each phase ships a single PR and can be reviewed independently.

### T1 — Condition DSL parser + evaluator

`gardenio/controller/internal/scheduler/conditions/`:

- New package; lifts `scheduler.Condition` types from
  `scheduler/scheduler.go`.
- Adds `all/any/not` composition.
- Adds `field/op/value` leaf with operators `== | != | in | not_in |
  contains | present | absent | in_subtree_of`.
- Daemon-side evaluator (existing) handles `bead_query` and `gate`.
- In-process evaluator for `field` leaves (used by trigger `match` and
  payload-aware stop conditions).
- `in_subtree_of` requires a daemon helper (`bead.IsAncestor(parent,
  child)`) — verify or add.

Tests: round-trip parse, evaluator unit tests for each leaf, nested
composition.

### T2 — `parent` filter on `bead_query`

Daemon-side. Verify the existing `bead_query` count endpoint accepts
`parent: <bead-id>` (transitive) — if not, add it. One-line addition
if the index already exists.

Tests: counts including/excluding subtree; ensures
`exit_condition` / `loop_end.bead_query.parent` work.

### T3 — `triggers:` field on schedule beads

`gardenio/controller/internal/scheduler/`:

- New `triggers` field on `Schedule` struct. Parse JSON array of
  trigger entries.
- Refactor `spawnScheduledAgent` and the cron evaluator into a
  `TriggerSet` per schedule. Each entry is one source.
- Wire `cron`, `manual`, `bead_event`, `decision_resolved` kinds.
  `mail_received`, `webhook`, `slack_reaction` deferred to a follow-on.
- `throttle` / `debounce` per entry (in-memory window store keyed on
  `<schedule_id>:<entry_index>`).
- Lowering rules for legacy `cron`/`trigger_mode` fields.
- Catch-up across all entries on restart.

Tests: legacy schedule continues to fire on cron; new schedule fires on
mixed cron+bead_event; throttle drops bursts; manual fires regardless.

### T4 — `stop:` field on schedule beads

`gardenio/controller/internal/scheduler/`:

- New `stop` field on `Schedule` struct. Parse JSON.
- New `StopEvaluator` evaluates `loop_end` and `hard_stop` slots on
  every reconcile tick. Closes schedule with structured reason
  (`terminated: <slot>:<kind>`) on first match.
- `run_end.wall_clock` enforced by a per-run deadline goroutine that
  kills the pod via the existing reaper. Logs as a normal closed run
  (debits one of `max_runs`).
- Lowering rules for legacy `max_runs` / `exit_condition` /
  `run_timeout` / `max_retries`.

Tests: each kind individually; precedence between `hard_stop`,
`loop_end`, `run_end`; legacy fields lower correctly.

### T5 — Progress instrumentation

`gardenio/controller/internal/scheduler/progress/`:

- New `progress` field on `Schedule` struct. Parse JSON.
- New `ProgressEvaluator` runs daemon-side queries against the
  configured `events` and `windows`. Stores per-schedule rolling
  counters in the schedule bead (`progress_state` opaque JSON field;
  scheduler-managed).
- `gb prime` "## Loop Progress" block (when
  `progress.surface.in_prime: true`).
- `gb stop --wrapup` enforces `progress_score` (int) and
  `progress_summary` (string) when
  `progress.surface.in_wrapup: true`. Reject wrap-ups missing them
  with a clear error.
- New stop kinds `no_progress_for: {runs|duration}` and
  `no_actionable_state` evaluate against the progress state.

Tests: events fire correctly on bead transitions; windows roll over;
prime renders; wrap-up enforcement; no_progress_for triggers cleanly.

### T6 — `gb gardener` CLI (downstream consumer; small)

`gardenio/controller/cmd/gb/gardener.go`:

- `gb gardener start <bead-id>` — idempotent; checks for existing
  schedule with `(assign_bead=<bead-id>, role=gardener, enabled=true)`
  and exits non-zero with the link if found, otherwise creates the
  schedule using the §5.1 shape. Lifts the JSON blocks into a Go
  template.
- `gb gardener status <bead-id>` — pretty-prints schedule, last run,
  progress windows, recent stop-condition evaluations.
- `gb gardener stop <bead-id>` — wrapper over `gb schedule complete`.
- `gb gardener force-close <bead-id>` — sets a `force-close` label
  field used by a `hard_stop.manual_force` entry.

Tests: idempotent re-create, schedule shape matches §5.1, CLI golden
outputs.

### T7 — Slack `@gardenio <bead-id>` entrypoint (downstream consumer; tiny)

`gardenio/controller/internal/bridge/bot_mentions.go`: helper +
shell-out to `gb gardener start <id>` when the leading token is a
bead ID and the bead is `type=epic`. Falls through otherwise.
Idempotent because `gb gardener start` is idempotent.

Tests: matched-epic path, fall-through cases.

### T8 — Runbook + advice

- `gardenio/docs/gardener-loop-runbook.md` (new) — operator doc covering
  the three condition families, examples, debugging.
- One-paragraph note in `gardenio/CLAUDE.md` pointing at the runbook.
- Global advice bead with one-line hook + pointer.

### T9 — E2E (claudeless)

`gardenio/tests/e2e/claudeless/` covers:

- `gardener-conditions-fan-in` — schedule fires on cron AND bead_event;
  throttle drops a burst.
- `gardener-stop-no-progress` — three runs with `progress_score=0`
  trigger `no_progress_for`.
- `gardener-stop-wall-clock` — schedule older than `wall_clock` closes
  on next tick.
- `gardener-progress-prime` — prime renders progress block correctly.
- `gardener-gardener-start-idempotent` — second `gb gardener start`
  returns existing.
- `gardener-mention-entrypoint` — `@gardenio <epic-id>` round-trips to
  same schedule shape as `gb gardener start`.

## 7. Risks / open questions

- **Schema sprawl.** `triggers` + `stop` + `progress` is a lot of
  optional JSON on one bead. Mitigation: every schedule the
  scheduler creates today still works without any of them; CLI
  helpers (T6 + a new `gb schedule template <kind>`) absorb the
  authoring burden.
- **Daemon-side `parent` recursion cost.** `bead_query.parent` over a
  large epic could be expensive. Mitigation: bound recursion depth
  (default 2), cache per-schedule per-tick.
- **Progress instrumentation correctness.** `mr_merged` requires the
  GitLab event subscriber be present and tagging beads correctly;
  `crew_dispatched` requires the agent bead carry the gardener's
  `schedule_id` (already true via `spawned_by`). Document the
  dependency in T5.
- **`run_end.wall_clock` is a behavior change.** Today, a run can take
  hours of Claude time without being killed. Adding a 30-min default
  could surprise existing gardeners. Mitigation: ship with default
  `run_end` slot empty (legacy behavior); each consumer opts in.
- **`field/op/value` against event payloads needs clear schema.** The
  bead-event payload, decision-resolved payload, and webhook payload
  have different field names. T1 must publish a per-kind payload
  schema as part of the parser.
- **Phase 8/9 of the primitive proposal lands trigger_mode events
  separately.** This plan supersedes that piecemeal approach by
  rolling all event kinds into the new `triggers:` array. We should
  close ka-0000003931-style phase beads as superseded once T3 lands.

## 8. Operator decision request

`artifact_type=diff-summary` for code-shaped options, `plan` for
rewrites:

- `[approve-T1-T5]` Approve the conditions DSL + scheduler engine
  changes (T1–T5). Defer T6 (gb gardener CLI) and T7 (Slack
  entrypoint) until conditions land. T8/T9 follow.
- `[approve-all]` Approve T1–T9 in order; let me proceed end-to-end.
- `[approve-T1-T3-only]` Trigger DSL only — land triggers + parent
  filter. Defer stop and progress to follow-on plans (recommended if
  you want to validate the schema gradually before committing to the
  larger surface).
- `[redo-progress]` Trigger and stop conditions are good; rewrite
  progress (§3) — the events/windows/surface shape isn't right.
- `[redo-stop]` Trigger and progress are good; rewrite stop (§2).
- `[redo-trigger]` Stop and progress are good; rewrite trigger (§1).
- `[redo-all]` Plan still not aligned — give specific direction and I
  will rewrite again.
- `[defer]` Defer; close ka-0000002674; revisit later.

## Appendix — gardener playbook prompt (for the §5.1 worked example)

```
You are the gardener for parent bead {{assign_bead}}.

Read your "## Scheduled Loop Context", "## Loop Progress", and
"## Epic Loop Context" sections. Then execute this playbook:

1. For each open child without a live agent owner: gb spawn --task <id>
   --role crew. Skip children that already have an in_progress agent.

2. For each decision raised by my crew with awaiting=gardener: assess.
   If one option clearly dominates and artifact_type is not in
   {epic, plan, bug}, gb decision report <id> --content '...' AND
   gb squawk the source thread with link+choice+rationale.

3. For each decision awaiting=human older than 4h AND reflagged_at
   empty or older than 4h: gb decision reflag <id> --reason '...'
   (bumps its priority toward P0 and reposts it to Slack so the
   operator sees it again).

4. If you OWN an open decision awaiting a human that you need
   resolved before you can continue: gb yield (it blocks until the
   operator resolves it). Do NOT yield merely because crew is
   in-flight — yield has nothing to wait on and will error.

5. Otherwise (crew dispatched, or nothing left to do this turn):
   gb stop --wrapup '{"accomplishments": "...", "progress_score":
   <0|1|2>, "progress_summary": "..."}'

The scheduler handles loop closure based on stop conditions. Don't
gb stop with goal_reached unless you've personally verified all
children are closed; the bead_query stop condition will handle the
common case.
```
