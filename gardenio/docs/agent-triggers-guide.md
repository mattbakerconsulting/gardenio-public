# Agent Tutorial: Authoring Condition-Driven Gardeners

This is the **agent-facing** tutorial for the trigger system. If you are an
agent and you have been asked to "write a gardener" or "make this run on a
schedule" or "fire something when a bead changes", read this doc first.

The operator-facing reference — full schema, every source, every field — is
[`docs/triggers.md`](triggers.md). This doc is the short path: pick a primitive,
author it with `gb trigger create`, verify it fired.

## Contents

- [Pick the right primitive](#pick-the-right-primitive)
- [Tutorial 1 — bead-event-driven gardener](#tutorial-1--bead-event-driven-gardener)
- [Tutorial 2 — cron-gated gardener](#tutorial-2--cron-gated-gardener)
- [Scheduled gardener primitive (`type=schedule`)](#scheduled-gardener-primitive-typeschedule)
- [Inspecting, testing, and disabling](#inspecting-testing-and-disabling)
- [Common pitfalls](#common-pitfalls)

## Pick the right primitive

Three shapes are in use today. Pick by how the gardener decides to fire.

| If the gardener should fire... | Use | Authoring command |
|---|---|---|
| ...the moment a bead transitions (created / updated / closed) and matches some predicate | `trigger:bead` | `gb trigger create bead` |
| ...on a cron schedule, optionally only when work exists (e.g. ready queue non-empty) | `trigger:schedule` | `gb trigger create schedule` |
| ...on a cron schedule unconditionally, no work-aware gating | legacy `type=schedule` | `kd schedule create ...` |

**Default to `trigger:*`.** The legacy `type=schedule` shape exists because it
predates the trigger engine; new gardeners should use `trigger:schedule` (with no
`--bead-query` if you genuinely want unconditional fires). The win is uniform
authoring, audit beads, allowlists, and a single engine to reason about.

Decision rule of thumb:

- **Reactive** ("when X happens, do Y") → `trigger:bead`. Edge-driven via SSE.
- **Polled but work-aware** ("every N minutes, but only if there's work") →
  `trigger:schedule` with a `--bead-query` precondition.
- **Pure cron** ("every Monday at 9am no matter what") → `trigger:schedule`
  with no conditions, OR legacy `type=schedule` if you really want the
  scheduler-bead semantics. Prefer the trigger.

For the full background on why this shape exists, see the prior-art doc at
`docs/research/condition-triggered-schedules.md` and the parent epic
ka-0000002187.

## Tutorial 1 — bead-event-driven gardener

**Goal.** Fire a triage crew the moment a gardener task closes with
`status:closed` and `label:cycle:failed`. This is the "gardener-cycle-watchdog"
shape: react to a known-bad terminal state on another agent's task and route
it to a human-or-crew.

### Step 1 — model the trigger

Decide the four pieces:

| Piece | Value |
|---|---|
| Event types | `closed` (we only care about the terminal transition) |
| Conditions | `label:cycle:failed` is present, and `type == task` |
| Action | `spawn_agent` with role `crew` and project `gardenio` |
| Prompt | "Gardener cycle {{bead_id}} ({{bead_title}}) failed — investigate." |

The condition shape for the `bead` source is `field:op[:value]`, where
`field` may be a top-level bead field (`type`, `status`, `title`, `assignee`,
`created_by`, `closed_by`, `id`), a key in the bead's extensible `Fields` map,
or `label:<name>` to test label presence. See
[`gardenio/controller/cmd/gb/trigger_bead.go`](../controller/cmd/gb/trigger_bead.go)
(`parseBeadCondition`) for the exact parse.

### Step 2 — author it with `gb trigger create bead`

```bash
gb trigger create bead \
  --title 'Gardener cycle watchdog: failed cycles → crew' \
  --event-types closed \
  --condition type:==:task \
  --condition label:cycle:failed:present \
  --action spawn_agent \
  --role crew \
  --agent-project gardenio \
  --prompt 'Gardener cycle {{bead_id}} ({{bead_title}}) closed in FAILED state.

Investigate: pull the cycle log, identify the failure cause, file a follow-up
bug bead, and (if cheap) push a fix.' \
  --max-concurrent 1 \
  --cooldown 5m \
  --allowed-project gardenio \
  --allowed-role crew
```

A few notes on the flags:

- `--event-types` accepts CSV or repeated values. Short names (`created`,
  `updated`, `closed`) and full SSE topics (`beads.bead.closed`) both work.
- `--condition` is **operator-aware**: the parser scans tokens left-to-right
  for the first known operator, so a label key with its own colons
  (`label:cycle:failed`) round-trips correctly. Operators: `==`, `!=`, `in`,
  `not_in`, `contains`, `present`, `absent`. `present` / `absent` are unary.
- `--max-concurrent 1` matters for watchdog-shaped gardeners: if a burst of
  failures lands in the same eval cycle, you almost certainly do not want N
  parallel triage agents. The remaining matches stay unseen and pick up on
  the next eval.
- `--cooldown` is informational today (the engine enforces a 30s floor
  globally). Set it anyway — it makes intent explicit and is reserved for
  future per-trigger enforcement.
- `--allowed-project` and `--allowed-role` are belt-and-suspenders: the
  trigger only fires for matches that target those projects / spawn those
  roles. Use them on user-created triggers; system-created triggers
  (`created_by == ""`) skip the check.

The command prints the new trigger's bead ID and a one-liner reminder of how
to disable it. Hold onto the ID for step 4.

### Step 3 — verify it fires

The fastest sanity check: do the thing the trigger watches for, and grep the
controller logs.

```bash
# 1. Create a fake gardener task with the matching label.
TEST_ID=$(kd create 'TEST: simulated failed cycle' \
  --type=task -l 'cycle:failed' -l 'project:gardenio' \
  --json | jq -r .id)

# 2. Close it. The 'closed' SSE event fires.
kd close "$TEST_ID"

# 3. Watch for the spawn in the controller logs (or in 'gb news').
kubectl -n gardenio logs deploy/gardenio-controller -f | grep -E 'trigger|spawn'
gb news | head
```

If the trigger matches, the engine creates a child task bead with labels
`source:bead`, `trigger:<trigger-id>`, `project:gardenio`, and spawns a crew
agent named `trigger-<bead-id>:closed-<suffix>` with the expanded prompt.

If nothing fires, see [Common pitfalls](#common-pitfalls). The most likely
culprits are condition typos (`label:` prefix wrong) and missing event types.

### Step 4 — clean up

For test triggers:

```bash
gb trigger disable <trigger-id>
kd close <trigger-id>           # archive the bead
```

`disable` flips `enabled=false`. The bead stays in place for audit. `close`
takes it out of the list view for everyone.

## Tutorial 2 — cron-gated gardener

**Goal.** Run gardener-delivery every minute, but only when there are open
ready issues in the `gardenio` project. This is the canonical work-aware-cron
shape: cron is the heartbeat, the bead-query precondition keeps the engine
from spawning a gardener into an empty queue.

This is the gardener-delivery conversion described in the parent epic
(ka-0000002187): the legacy `type=schedule` cron `*/10` becomes a
`trigger:schedule` cron `*/1` with a ready-queue precondition. Polling more
often is fine because the precondition guards the actual fire.

### Step 1 — model the trigger

| Piece | Value |
|---|---|
| Cron | `* * * * *` (every minute) |
| Timezone | `UTC` (default) |
| Pre-flight | `bead_query`: open `task` beads in project `gardenio`, count `> 0` |
| Action | `spawn_agent`, role `gardener-delivery`, project `gardenio` |
| Prompt | "Drive the gardenio ready queue: pick the highest-priority READY bead, scope it, dispatch a crew." |

The precondition shape for `--bead-query` is a comma-separated list of
key=value pairs. Recognised keys: `op` (default `>`), `value` (default `0`),
`labels` (pipe-separated for multiple values), `exclude_labels`, `types`,
`statuses` (default `["open"]`), `project`, `role`, and `type` (defaults to
`bead_query`; switch to `ready_queue`, `active_agents`, or `gate` for those
condition shapes). A `ready_queue` leaf counts with `gb ready` semantics —
open `kind=issue` beads with no open dependencies — and honors `labels` /
`exclude_labels`, so it won't fire while the labeled work is
dependency-blocked.
See [`gardenio/controller/cmd/gb/trigger_schedule.go`](../controller/cmd/gb/trigger_schedule.go)
(`parseBeadQueryCondition`) for the exact parse.

When you pass `project=<name>` without an explicit `labels=`, the project is
auto-propagated as a `project:<name>` label so the count query stays scoped.
This is the right behavior 95% of the time — pass an explicit `labels=` only
when you mean it.

### Step 2 — author it with `gb trigger create schedule`

```bash
gb trigger create schedule \
  --title 'gardener-delivery: drive ready queue when work exists' \
  --cron '* * * * *' \
  --bead-query 'project=gardenio,types=task,statuses=open,op=>,value=0' \
  --action spawn_agent \
  --role gardener-delivery \
  --agent-project gardenio \
  --prompt 'Drive the gardenio ready queue.

Steps:
1. gb ready --limit 5
2. Pick the highest-priority unblocked task.
3. Scope it (read deps, parent, comments).
4. Dispatch a crew via gb spawn, or claim + plan if you can do it yourself.

Cron fire time: {{fire_time_local}}' \
  --max-concurrent 1 \
  --cooldown 1m \
  --allowed-project gardenio \
  --allowed-role gardener-delivery
```

A few notes on the flags:

- `--cron` is validated at create time against `scheduler.ParseCron`. Bad
  expressions (`* * * *`, `*/0 * * * *`, `@invalid`) are rejected before the
  bead is written. Five-field cron with standard aliases (`@hourly`,
  `@daily`, `@weekly`, etc.) is supported.
- `--tz` accepts an IANA zone (e.g. `America/Los_Angeles`). Default is UTC.
  Useful when the schedule is human-shaped — "weekday mornings 9-11" reads
  better in local time than UTC.
- `--bead-query` is repeatable. Multiple preconditions are AND'd; all must
  pass for the schedule to fire on a given fire-time.
- `--max-concurrent 1` is mandatory-ish for gardener-shaped triggers: a
  schedule fires once per minute granularity, and you almost never want more
  than one gardener spawned per fire.
- `{{fire_time}}` (RFC3339 UTC) and `{{fire_time_local}}` are available in
  the prompt. Useful for traceability when the gardener bead lands in
  `gb news`. `{{fire_time_iso}}` is an engine-level alias for
  `{{fire_time}}` (ka-0000006473) — both spellings work in `prompt` and
  `formula_vars` on every source. For `apply_formula` triggers, any
  `formula_vars` value still containing a `{{...}}` placeholder after
  expansion (e.g. a typo'd var no source publishes) fails the fire instead
  of leaking the literal into step titles.

### Step 3 — verify it fires

Cron triggers are slower to verify than bead triggers. Two strategies:

**Strategy A — speed up cron temporarily.** Set `--cron '* * * * *'` (every
minute) and watch one minute pass. If the precondition is false, the trigger
won't fire — confirm by checking the precondition manually:

```bash
# Mirror of what bead_query checks:
kd list --project=gardenio --types=task --statuses=open --json | jq 'length'
```

If that count is `> 0` and a fire-minute has elapsed, you should see a
`schedule trigger fired` log line and a new gardener agent in `gb news`.

**Strategy B — `gb trigger list` plus `last_run`.** The engine writes
`last_run` to the trigger bead after every successful fire. You can poll:

```bash
gb trigger list --source schedule
kd show <trigger-id> --json | jq '.fields | {cron, last_run, last_checked}'
```

If `last_checked` advances every ~30s but `last_run` does not, the cron
matched but the precondition rejected. If `last_checked` does not advance,
the controller is not loading the trigger — most often a missing
`trigger:schedule` label or `enabled=false`.

### Step 4 — converting from legacy `type=schedule`

If you are migrating an existing gardener (the gardener-delivery conversion is
the worked example), the safe path is:

1. Author the new `trigger:schedule` bead with `--enabled=false` (or
   `gb trigger disable <id>` immediately after create).
2. Verify the precondition logic by hand against current state.
3. `kd schedule disable <legacy-schedule-id>` to stop the old gardener.
4. `gb trigger enable <new-trigger-id>`.
5. Watch one full cycle. If anything looks wrong, `gb trigger disable`
   and `kd schedule enable` to revert.

The two systems do not deduplicate against each other — leaving both
enabled means double-spawns. Cut one, then the other.

## Scheduled gardener primitive (`type=schedule`)

`trigger:schedule` (above) is for fanning out simple work-aware-cron actions:
"every minute, if the ready queue is non-empty, spawn a gardener-delivery
agent." Each fire is independent.

`type=schedule` is the **scheduled gardener** primitive: a long-running,
goal-bearing loop that owns a single agent at a time, sees its prior run's
wrap-up, and can self-terminate when the goal is reached. Reach for it
when the gardener needs *continuity* across runs ("did the prior run finish
the migration step?") rather than just "fire on a tick."

The reference for the schedule bead schema and every field is in
[`docs/triggers.md`](triggers.md#scheduled-gardeners-typeschedule). This
section is the agent-facing how-to for the four pieces a gardener author
needs to get right.

### `run_budget` is REQUIRED — a gardener is never unbounded

> **A gardener MUST declare BOTH `run_budget > 0` AND a `hard_stop`
> `wall_clock` in `(0, 720h]`.** This is not a style preference — it is
> **fail-closed enforced** (bug ka-0000006744; advice ka-0000000974). At
> authoring time `gb gardener start` / `gb trigger create` exit non-zero and
> write **nothing** for an unbounded gardener (`bridge.ValidateGardenerBlob`).
> There is no "leave it at `0`" option for a gardener. An unbounded gardener
> is the single biggest cost/RSI footgun in the fleet — it spins forever,
> spawns crew forever, and burns tokens + CI minutes + namespaces with no
> ceiling.

The invariant, the two orthogonal safety axes (**backstop** vs
**self-block**), the asymmetric authoring-vs-firing rollout, and the
"inspect the engine verdict, not the flat bead fields" rule are all
specified once, canonically, in the
[gardener-loop runbook — *Gardener bounded-stop enforcement (global)*](gardener-loop-runbook.md#gardener-bounded-stop-enforcement-global).
Read that before authoring. The short version for picking a budget:

| Shape | `run_budget` (gardeners) / `max_runs` (schedules) | Reasoning |
|---|---|---|
| **Bounded**: you can describe "done" as a goal the gardener can self-evaluate | **Required, and pick a generous ceiling** (e.g. `20`, `50`) — the budget is a *budget*, not an SLA. Pair it with a progress-aware or `bead_query` `loop_end`. | The whole point of the gardener primitive is loops that terminate. The budget bounds *spin*; the `loop_end` closes on real completion; a re-arm on credited progress refills the budget, so the ceiling only ever brakes a wedged loop. |
| **Standing / perpetual**: a true heartbeat with no natural "done" (release-owner, delivery merge-owner) | **Still required.** Set a **high** `run_budget` (e.g. `360`, `555`) + the `hard_stop` `wall_clock`, and mark it `perpetual:true` (or `standing:true` in the blob). | A `perpetual` gardener **re-arms in place forever** on zero-progress quiet periods instead of escalate-closing (bug ka-0000006241), so the budget never wrongly kills it — but the `hard_stop` `wall_clock` remains the unconditional emergency brake. |
| **One-shot**: should run exactly once on operator demand | `run_budget=1` / `max_runs=1`, `trigger_mode=manual`. | One-shot tools (re-embed everything, force-rebuild caches) shouldn't sit dormant on a cron. |
| **Not a gardener at all**: a preflight-gated cron spawn-worker (no `gardener` blob) whose cost is bounded by a `bead_query` preflight (`ready_queue>0`) | It has **no** gardener budget because it is **not** a gardener — it never enters the gardener fire-path (`gardenerPreFireCheck` short-circuits at `trig.Gardener == nil`). Cost is ceilinged by work-availability, not a run count. | Don't bolt a `gardener` blob onto a bare cron worker just to satisfy this rule — that *adds* the escalate-close surface. If it's a loop that drives an epic, make it a real gardener with both bounds; if it's a work-availability-gated spawner, leave it blob-less. |

If you reach for a heartbeat gardener doing more than "observe + report",
reconsider — most "drive X to completion" loops are better as bounded
gardeners with a `terminal_goal`, a progress-aware `loop_end`, and a
generous `run_budget`, not a perpetual one.

### Wrap-ups: `goal_reached` short-circuits the loop

Each run ends with a `gb stop --wrapup '<json>'`. The wrap-up JSON is
read by the scheduler when the agent's bead closes. Two keys matter for
loop control:

```bash
# Goal reached — close the schedule, regardless of remaining max_runs.
gb stop --wrapup '{"goal_reached":true,"goal_reason":"all open gardener-shaped beads drained","accomplishments":"Closed ka-AAA, ka-BBB, ka-CCC."}'

# Goal not yet reached — just record what happened. Loop continues.
gb stop --wrapup '{"accomplishments":"Drove ka-AAA from READY to MR-OPEN; ka-BBB still pending decision."}'
```

`goal_reached:true` outranks `max_runs` and `exit_condition`. Use it
when *your run* observed the goal — not when you are merely tired of
running. `goal_reason` is a one-line human-readable explanation that
shows up on the schedule bead's close comment. `accomplishments` is
shown verbatim (truncated at 300 chars) in the schedule's run-history
comments and is rendered as the **Prior-run summary** block in the next
run's `gb prime` output, so write it for the *next gardener reading it*,
not for an operator skimming Slack.

### Manual fire (`trigger_mode=manual`) + `gb schedule fire`

Set `trigger_mode=manual` to make a schedule dormant on cron — it will
not fire from `Reconcile` even with `enabled=true` and a valid `cron`.
The only way to trigger a run is:

```bash
gb schedule fire <schedule-id> --reason 'after migration cutover'
```

Useful when:

- The schedule is a one-shot tool a human or another gardener runs on
  demand (re-embed, rebuild, replay).
- The schedule has a complex external precondition that can't be
  expressed as a `bead_query` (an upstream pipeline finished, a deploy
  rolled out).
- You are testing a schedule's spawn path before turning on cron.

`gb schedule fire` is also legal for `trigger_mode=cron` schedules — it
forces an extra spawn between cron ticks, with the same gates the
scheduler enforces (refuses on closed/disabled, `max_runs`-exhausted,
passing `exit_condition`, or concurrency-limited). It's the right tool
for "I just merged the fix; run gardener-delivery once now without
waiting for the next */10 tick."

### Self-management subcommands

When a gardener is running and decides to alter its own loop:

```bash
gb schedule progress <id> --note 'crew dispatched to ka-AAA; yielding'
gb schedule extend <id>   --runs +5     # need more iterations
gb schedule complete <id> --reason 'goal reached, no further runs needed'
```

`progress` is purely informational (a timestamped `[progress]` comment).
`extend` bumps `max_runs` so the loop can keep going. `complete` is the
hard-stop alternative to `goal_reached` in a wrap-up — use it when you
need to close the schedule **without** a pending run (e.g. you are an
operator-style agent and the loop is done; the wrap-up path is for the
gardener that *is* the run).

### Where Scheduled Loop Context appears in `gb prime`

When an agent is spawned by a `type=schedule` (i.e. its bead carries
`schedule_id`), `gb prime` injects an additional `## Scheduled Loop
Context` section after the standard preamble. It contains:

- "You are run **N of M** of schedule **\"<title>\"** (<id>)" — or
  "run **N**" if `max_runs=0`.
- The schedule's `terminal_goal`, if set.
- The prior run's `accomplishments` (read from
  `schedule_prior_agent_id`'s `wrapup`), rendered as a quoted block.
- "Runs remaining after this one: <K>" with concrete `gb stop --wrapup`
  syntax for declaring `goal_reached`.
- An explicit reminder that **`gb stop` consumes a run, `gb yield`
  does not** — the inverse of non-scheduled gardener behavior. This is
  the most common mistake: a gardener that yields N times and then
  stops thinking each yield was a run, blowing the budget on no work.

The block is rendered by `outputScheduledLoopContext` in
[`gardenio/controller/cmd/gb/prime_schedule.go`](../controller/cmd/gb/prime_schedule.go).
It is **inline and cannot be elided** — the run lifecycle is too easy
to get wrong without it.

The fields driving this block (`schedule_id`, `schedule_title`,
`schedule_runs_spawned`, `schedule_max_runs`, `schedule_terminal_goal`,
`schedule_prior_agent_id`) are stamped on the agent bead at spawn time
by `scheduler.spawnScheduledAgent`. Operators do not set them; they
mirror the schedule bead's state at the moment of spawn.

### Schedule/trigger field reference (one table)

This is the canonical agent-facing index of every schedule/gardener field —
**you should not have to read `internal/scheduler/` to learn what these
mean.** The operator-facing reference with the full per-field R/W and
default columns lives in
[`docs/triggers.md` — Run-budget & terminal goal / Counters / Wrap-up signals](triggers.md#run-budget--terminal-goal-ephemeral-schedules-gardener-primitive).

| Field | Who sets it | What it means |
|---|---|---|
| `max_runs` / `run_budget` | operator (initial); agent via `gb schedule extend` | Hard cap on total spawns. On `type=trigger` gardeners the same cap is expressed as `run_budget` in the gardener spec and is **REQUIRED** — a gardener with `run_budget <= 0` is refused at authoring time (`bridge.ValidateGardenerBlob`, fail-closed). Loop auto-closes at `runs_completed >= max_runs`. |
| `terminal_goal` (stamped as `schedule_terminal_goal`) | operator | Natural-language "done" definition, surfaced to every run via `gb prime`'s `## Scheduled Loop Context`. |
| `standing` (gardener blob) / `perpetual:true` label | operator | Marks a PERPETUAL gardener whose `terminal_goal` is a standing mandate (never self-declares "done") pinned to no terminal epic — e.g. the release-owner and delivery merge-owner. A standing gardener that spends its resume budget on zero-progress runs (a normal quiet period) **re-arms in place forever** instead of being flagged `gardener-stuck-escalate` + closed; only the terminal `hard_stop` brakes (`consecutive_failures` / `wall_clock`) can close it. Recognized via EITHER the blob flag OR the `perpetual:true` label (bug ka-0000006241). |
| `exit_condition` | operator | DSL condition re-checked before each spawn; passing it auto-closes the schedule. |
| `run_timeout` | operator | Reaps a run stuck in `yielding` past this duration. |
| `schedule_runs_spawned` / `runs_spawned` | scheduler | How many runs have been spawned. The "N" in "run **N of M**". |
| `runs_completed` | scheduler | How many spawned runs have closed; drives the `max_runs` auto-close. |
| `consecutive_failures` | scheduler | Failed-close streak; auto-disables the schedule at `max_retries` (default 3). Reset on a clean close. |
| `schedule_prior_agent_id` / `last_agent_id` | scheduler | Prior run's agent bead; `gb prime` reads its wrap-up to render the prior-run summary. |
| `goal_reached` | **agent** (in `gb stop --wrapup`) | `true` short-circuits the whole loop — highest-priority stop, beats `max_runs`/`exit_condition`. |
| `goal_reason` | agent | Human-readable reason appended to the goal-reached close comment. |
| `accomplishments` | agent | Surfaced verbatim in the schedule's run-completed comment and as the next run's prior-run summary. |

`run_budget` and the `stop:` / `progress:` field shapes for the unified
`type=trigger` gardener path are specified in the
[gardener-loop runbook](gardener-loop-runbook.md) (§1–§3); that runbook's
§7 "Schedule/trigger field reference" row links back here.

## Inspecting, testing, and disabling

Same surface for both shapes:

```bash
gb trigger list                         # all triggers, all sources
gb trigger list --source bead           # just trigger:bead
gb trigger list --source schedule       # just trigger:schedule
gb trigger disable <trigger-id>         # enabled=false; bead stays
gb trigger enable  <trigger-id>         # enabled=true
kd show <trigger-id>                    # full schema, last_checked, last_run
kd show <trigger-id> --json | jq .fields
```

For audit, every fire by a user-created trigger writes an `event` bead with
labels `[auto:trigger-audit, source:<source>, trigger:<id>]`. Search for
recent fires:

```bash
kd list --types=event --labels='trigger:<trigger-id>' --limit 20
```

System-created triggers (no `created_by`) skip audit to avoid bead spam.

## Worked example — auto-forensics on involuntary death

The auto-forensics pipeline (epic ka-0000005447) is a `trigger:bead` +
`spawn_agent` loop that turns every involuntary agent death into a "why did this
agent die" report. It is the reference use of the `is_involuntary_death`
condition operator and the `gb session death-report --bead` command.

**The loop:** an agent dies mid-work → its bead closes with a `close_reason`
carrying the `_involuntary` marker → the trigger fires a forensics crew → that
crew runs `gb session death-report --bead <deadBead>`, which pulls the DEAD
agent's own S3 recording (the dead bead id IS the coop session key), summarizes
the last N turns, and links the full transcript → the crew squawks a short
report.

```bash
gb trigger create bead \
  --title 'Auto-forensics: involuntary agent death' \
  --event-types closed \
  --condition type:==:agent \
  --condition close_reason:is_involuntary_death \
  --action spawn_agent --role crew --agent-project gardenio \
  --max-concurrent 1 \
  --prompt 'Forensics: agent {{bead_id}} died involuntarily (family {{death_family}}). Run: gb session death-report --bead {{bead_id}} -n 3 — it pulls the dead agent'"'"'s S3 recording, summarizes the last 3 turns, and prints the full-transcript S3 URL. Then squawk a <=3-line report: agent name, death family, last words, and the S3 URL. Then gb stop.'
```

Notes specific to this loop:

- **`is_involuntary_death` keys off the `_involuntary` marker in `close_reason`,
  NOT the pod exit code** (a SIGTERM'd pod always exits 143, so exit code cannot
  discriminate a mid-work kill from a clean idle-out). It delegates to the SAME
  predicate that gates `gardenio_agent_involuntary_deaths_total`, so the trigger
  fires on exactly the deaths the metric counts. The optional value narrows to
  one family (e.g. `close_reason:is_involuntary_death:stop_gate_cooldown`).
- **`{{death_family}}`** is surfaced on the match only when the reason classifies
  as a counted death, so the prompt can name the shape without re-parsing.
- **The forensics agent needs S3 access.** `gb session death-report --bead`
  reads `COOP_S3_BUCKET`; agent pods carry it, the slack-bridge pod does not —
  which is why forensics runs as a spawned agent, not in the bridge. Pass
  `--no-s3` to force the local-log / close-reason-only fallback.
- **`--max-concurrent 1`** keeps a death storm from spawning a swarm; the
  bridge's own death-report Slack post is separately rate-gated
  (`bot_agent_death.go`).

## Common pitfalls

- **Wrong condition operator.** `parseBeadCondition` rejects an unknown
  operator with a hard error. If you see "no recognised operator" the field
  almost certainly contains a colon and the parser walked past your intended
  op. Quote the value or split the rule.
- **`label:project:` prefix matching needs the trailing colon.**
  `label:project:` matches any `project:<name>` label;
  `label:project:gardenio` matches that exact label only. The CLI parses both
  correctly, but you have to mean what you typed.
- **`--bead-query project=X` without `labels=`** auto-injects
  `labels=project:X`. If you also pass an explicit `labels=`, the project
  is **not** auto-injected — your `labels=` wins. This catches people who
  expect "intersection" semantics; the CLI is "use what I gave you".
- **Cron at minute granularity dedups by minute.** A trigger keyed
  `2026-04-25T17:42` fires at most once. If you bounce the controller and
  the same fire-minute is still current, `CatchUpKeys` prevents a re-fire
  via the trigger-id label on the previous spawn.
- **`max_concurrent` defaults to 3.** That is fine for inbox-shaped
  triggers (JIRA, GitLab MRs) but wrong for gardener-shaped triggers. Set
  `--max-concurrent 1` whenever a single matched evaluation should produce
  a single agent.
- **`enabled=false` looks like "trigger missing".** A disabled trigger is
  still loaded by `LoadTriggers` but skipped by the engine. `gb trigger list`
  shows the `ENABLED` column — check it before assuming the trigger has
  been deleted.
- **Allowlists silently drop fires.** A user-created trigger with
  `allowed_projects=gardenio` and a match for `project=monorepo` will pass
  evaluation, fail authorization, and write a `deny` audit bead. If
  `gb trigger list` shows recent fires but no agents, grep audit for
  `outcome:deny`.
- **Project-less (repo-less) agent-spawning prompts must not name git paths.**
  A trigger whose `project` **field** is empty spawns a pod with **no working
  tree** (this covers both `spawn_agent` and `apply_formula`, which pours the
  molecule then spawns a driver crew from the same prompt) —
  `buildInitCloneContainer` clones nothing without a project's
  `git_url`, and there is no role-level skip-clone flag. A prompt that sends the
  agent to `gardenio/docs/…` (or any repo path) then 404s at runtime
  (bug ka-0000004796: the repo-less first-mate gardener pointed at its own
  playbook docs and hit "file does not exist"). Inline the content into an
  advice body / bundle the agent reads via `gb advice show` / `gb bundle show`
  (advice ka-0000002990), or give the trigger a real `project` so the repo is
  cloned. Note the `project:<name>` **label** is orthogonal to the `project`
  **field** — a trigger can carry the label for advice targeting while its field
  is empty, which is still repo-less. `gb trigger create` / `gb gardener start`
  and the engine warn (never block) when a repo-less prompt names a git path
  without a "you run repo-less — do NOT open it" disclaimer (advice
  ka-0000006996).

## See also

- [`docs/triggers.md`](triggers.md) — operator-facing reference (every
  source, every field, every action).
- [`docs/runbooks/README.md`](runbooks/README.md) — **loop runbooks**: for a
  long-lived gardener, the operating contract (candidate selection, scope,
  guardrails, wrap-up contract) belongs in a versioned
  `docs/runbooks/<loop>.md`, and the trigger prompt shrinks to a pointer.
  Keep the trigger prompt thin; the repo owns the loop contract.
- [`gardenio/controller/cmd/gb/trigger_bead.go`](../controller/cmd/gb/trigger_bead.go)
  — `gb trigger create bead` source. Read `parseBeadCondition` for the
  condition grammar.
- [`gardenio/controller/cmd/gb/trigger_schedule.go`](../controller/cmd/gb/trigger_schedule.go)
  — `gb trigger create schedule` source. Read `parseBeadQueryCondition` for
  the precondition grammar.
- [`gardenio/controller/internal/bridge/triggers.go`](../controller/internal/bridge/triggers.go)
  — engine, `ParseTriggerDef`, action dispatch.
- Advice bead **ka-0000004019** — "When to use `trigger:bead` vs
  `trigger:schedule` vs `type=schedule` for gardeners" — short decision rule
  paired with this doc.
- Parent epic **ka-0000002187** — rollout context, gardener-delivery
  conversion, out-of-scope follow-ups.
