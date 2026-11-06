# Gardener Loop Runbook

> **Phase 3b status (2026-05-27): `type=trigger` has absorbed `type=schedule`.**
> All open gardeners were migrated to `type=trigger` in Phase 3b/C8
> (see [`gardenio/plans/trigger-schedule-unification.md`](../plans/trigger-schedule-unification.md)).
> Gardeners are authored as `type=trigger` with gardener fields
> (`triggers:`, `stop:`, `progress:`, `terminal_goal`, `run_budget`,
> `max_runs`). The text below uses `type=trigger` throughout.

Operator-facing reference for the **scheduled-gardener** primitive: how to
configure the loop, how to stop it, and how to tell whether it is making
progress. The plan-of-record is
[`gardenio/plans/epic-mention-looping-gardener.md`](../plans/epic-mention-looping-gardener.md);
this runbook paraphrases it in the order an operator (or an LLM debugging
a stuck gardener) actually needs.

A **gardener** is a `type=trigger` bead carrying gardener-loop fields, whose
loop is fully described by three condition families:

| Family | Question it answers | Where it lives on the bead |
|---|---|---|
| **Trigger** | What fires a run? | `triggers:` (JSON array) |
| **Stop**    | When does this run end? When does the loop end? | `stop:` (JSON object) |
| **Progress** | Did this run move us forward? Is the loop wedged? | `progress:` (JSON object) + `progress_state` (scheduler-managed) |

If you only remember one thing: every gardener is a small declarative
program over those three slots. The Go scheduler (`gardenio/controller/internal/scheduler/`)
evaluates them; you author them as bead fields.

## Contents

- [1. Trigger conditions](#1-trigger-conditions)
  - [Common DSL Pitfalls — nested `triggers:`](#common-dsl-pitfalls--nested-triggers)
- [2. Stop conditions](#2-stop-conditions)
- [3. Progress conditions](#3-progress-conditions)
- [4. Shared condition DSL](#4-shared-condition-dsl)
- [5. Worked examples](#5-worked-examples)
  - [5.1 Epic gardener](#51-epic-gardener)
  - [5.2 Cycle-watchdog](#52-cycle-watchdog)
  - [5.3 Ready-queue drainer](#53-ready-queue-drainer)
  - [5.4 Hybrid gardeners: responsive + polling wake](#54-hybrid-gardeners-responsive--polling-wake)
- [6. Debugging a gardener](#6-debugging-a-gardener)
- [7. Scheduler internals — where to look in code](#7-scheduler-internals--where-to-look-in-code)
- [8. Wake-condition recipe cookbook](#8-wake-condition-recipe-cookbook)
  - [8.1 The payload-shape rule (read this first)](#81-the-payload-shape-rule-read-this-first)
  - [8.2 Recipe: wake when one of my MRs goes green](#82-recipe-wake-when-one-of-my-mrs-goes-green)
  - [8.3 Recipe: wake when an escalation resolves](#83-recipe-wake-when-an-escalation-resolves)
  - [8.4 Recipe: wake when my crew's decision is answered](#84-recipe-wake-when-my-crews-decision-is-answered)
  - [8.5 Recipe: wake when a child of my epic closes](#85-recipe-wake-when-a-child-of-my-epic-closes)
  - [8.6 Recipe: hybrid event + cron floor (the default)](#86-recipe-hybrid-event--cron-floor-the-default)
  - [8.6b Recipe: wake on a graph-edge change (comment / dependency / label)](#86b-recipe-wake-on-a-graph-edge-change-comment--dependency--label)
  - [8.7 What does NOT work yet](#87-what-does-not-work-yet)
- [9. Second-life lane — reviver / reaper coverage of dormant + closed gardeners](#9-second-life-lane--reviver--reaper-coverage-of-dormant--closed-gardeners)
  - [9.1 The three cohorts](#91-the-three-cohorts)
  - [9.2 Coverage matrix](#92-coverage-matrix)
  - [9.3 Why the cohorts are disjoint](#93-why-the-cohorts-are-disjoint)
  - [9.4 The second-life grant (engine mechanic)](#94-the-second-life-grant-engine-mechanic)

## 1. Trigger conditions

The `triggers:` field is a JSON array. Each entry is one **trigger source**
with its own kind, its own match/preflight rules, and its own throttle.
Any one entry firing produces one run; entries compose by fan-in.

### Schema

```json
[
  {
    "kind": "cron",
    "cron": "*/30 * * * *",
    "timezone": "America/Los_Angeles",
    "preflight": {"all": [
      {"type": "bead_query", "op": ">", "value": "0",
       "types": ["task", "bug"], "parent": "{{assign_bead}}",
       "status": ["open", "in_progress"]}
    ]}
  },
  {
    "kind": "bead_event",
    "events": ["closed", "updated"],
    "match": {"all": [
      {"field": "parent", "op": "==", "value": "{{assign_bead}}"}
    ]},
    "throttle": "60s"
  },
  {
    "kind": "decision_resolved",
    "match": {"all": [
      {"field": "raised_by_agent.task_id", "op": "in_subtree_of",
       "value": "{{assign_bead}}"}
    ]}
  },
  {"kind": "manual", "label": "operator-fire"}
]
```

### Trigger kinds

| `kind` | Fires when… |
|---|---|
| `cron` | The cron expression matches and `preflight` is true at fire time. |
| `bead_event` | A bead transitions on one of the listed `events` (`closed`, `updated`, `created`) AND the event payload satisfies `match`. |
| `decision_resolved` | A decision bead resolves AND the decision's payload satisfies `match`. |
| `manual` | An operator runs `gb schedule fire <id>` (or the `gb gardener` CLI equivalent). |
| `mail_received` / `webhook` / `slack_reaction` | Reserved per plan §1.4; deferred to a follow-on. |

### Per-entry knobs

| Field | Meaning |
|---|---|
| `preflight` | Daemon-side condition expression. Evaluated **at fire time**; if false, the source dropped this fire (no run, no dedup mark). Use for "I should have run, but conditions say no". |
| `match` | Condition expression evaluated **against the event payload** (only for event-kind entries). Different from `preflight`: preflight asks the world a question; match asks the event a question. |
| `throttle` | Minimum wall-clock between two fires from this entry. Excess events drop with last-write-wins on the most-recent payload. |
| `debounce` | Coalesce a burst of events in this window into a single fire after the burst settles. |

### Engine semantics

- **Fan-in.** Any one entry firing produces one run. `max_concurrent`
  (default 1 for gardener schedules) means only one run lives at a time;
  subsequent fires queue up to one deep, the rest drop.
- **Yield-aware fire.** If a yielded run already exists, the next fire
  *wakes the yielded run* (re-uses the agent bead, does NOT bump
  `runs_spawned`) instead of spawning a parallel pod.
- **Catch-up on restart.** Each entry uses its own per-source catch-up:
  cron via `last_run`, event kinds via the audit beads with
  `trigger:<id>` labels.
- **Why this run?** `gb prime` renders a "Last fire trigger" line in the
  Scheduled Loop Context block ("woken by `decision_resolved` on ka-DEC1
  at 13:42") so the gardener prompt can branch on cause.

### Backwards compatibility

Legacy `cron` / `timezone` / `trigger_mode` fields keep working when
`triggers:` is empty; the scheduler lowers them at parse time:

| Legacy field | Lowered to |
|---|---|
| `cron`, `timezone`, `trigger_mode=cron` | `triggers: [{kind: "cron", cron, timezone}]` |
| `trigger_mode=manual` | `triggers: [{kind: "manual"}]` |

Mixing legacy fields with a `triggers:` array is allowed: the lowering
appends to the array.

### Common DSL Pitfalls — nested `triggers:`

> **⚠️ Authoring a gardener with a nested `triggers:[]` blob (or a nested
> `gardener` blob carrying `triggers:` / `stop:` / `progress:`) produces an
> INERT gardener that NEVER fires.** The rich nested shape shown throughout
> this runbook is the trigger-engine's *decode capability* — not the live
> authoring schema. If you hand `kd create`/`gb trigger create` a bead whose
> firing declaration lives only inside a nested array, one of two things
> happens and **neither** gets you a running gardener:
>
> 1. The live `type:trigger` config schema (config bead `ka-0000001965`)
>    does **not** yet permit `triggers` / `stop` / `progress` / `assign_bead`
>    as bead keys, so bead creation **rejects** them outright; or
> 2. If the keys slip through onto the bead, `LoadTriggers`
>    (`internal/bridge/trigger_source_schedule.go`) still keys firing off the
>    **top-level** `trigger:schedule` (or `trigger:bead`) label plus the flat
>    `cron` field — it does **not** synthesize a schedule source from a nested
>    `triggers:[{"kind":"cron",…}]` blob. The bead reads healthy (`enabled=true`,
>    `status=open`) but **silently never fires** — the same dark-gardener trap as
>    an inert flat self-block field (see §"Inspecting a gardener's bounds").

**What the engine actually requires to fire.** A gardener fires only when it
carries, at the **top level of the bead**:

- a `trigger:schedule` label (cron-based) or `trigger:bead` label
  (event-driven) — see [`triggers.md`](triggers.md);
- for `trigger:schedule`, a flat top-level `cron` field (5-field cron), with
  optional `timezone` and a flat `conditions` JSON array for the pre-flight
  gate; and
- `action: spawn_agent` with `role: gardener`.

This is exactly the **bare firing shape** `gb gardener start` emits (see the
note in [§5.1 Epic gardener](#51-epic-gardener)): `type=trigger` /
`trigger:schedule` / `action=spawn_agent`, a flat `cron` + `conditions`
pre-flight, and the epic pinned via a `gardener-epic:<id>` label. The terminal
goal, run budget, and wall-clock caps are folded into the gardener **prompt**
(the agent owns its own `gb stop`).

**Why the nested shape appears in this runbook at all.** The nested
`triggers:` / `stop:` / `progress:` blobs document the engine's full decode
*capability* — the shape a future schema widening will accept. Until the
`type:trigger` config schema is widened to permit `assign_bead` / `stop` /
`progress` / `triggers` (tracked in [§5.1](#51-epic-gardener)), author from the
bare firing shape. When in doubt, copy a **known-live** gardener rather than the
worked examples below, and verify it actually fires with
`gb gardener status <bead-id>` (check `spawned` increments — a plausible-looking
`enabled=true` is not evidence it fires; see §6).

## 2. Stop conditions

The `stop:` field is a JSON object with **three slots**: `loop_end`,
`run_end`, and `hard_stop`. Each slot is `{any: [<entry>, ...]}` — first
match wins. There is no `all:` for stop slots (semantically nonsensical
— if you need AND, compose it inside one entry via the condition DSL).

### Schema

```json
{
  "loop_end": {"any": [
    {"kind": "max_runs"},
    {"kind": "wall_clock", "duration": "72h"},
    {"kind": "bead_query", "op": "==", "value": "0",
     "types": ["task", "bug", "epic"],
     "parent": "{{assign_bead}}",
     "status": ["open", "in_progress", "blocked"]},
    {"kind": "specific_bead_closed", "bead_id": "{{assign_bead}}"},
    {"kind": "no_progress_for", "runs": 3},
    {"kind": "manual", "reason_field": "operator_close_reason"}
  ]},
  "run_end": {"any": [
    {"kind": "yielded_then_woken_then_idle"},
    {"kind": "wall_clock", "duration": "30m"},
    {"kind": "no_actionable_state"}
  ]},
  "hard_stop": {"any": [
    {"kind": "manual_force", "label": "force-close"},
    {"kind": "consecutive_failures", "value": 5},
    {"kind": "wall_clock", "duration": "168h"}
  ]},
  "on_loop_end": "close"
}
```

### Slot semantics

| Slot | Evaluated | Effect |
|---|---|---|
| `loop_end.any` | Before each fire AND at end of each run | Close the schedule cleanly. `on_loop_end` chooses the post-closure action (`close`, `notify_slack`, `spawn_followup`). |
| `run_end.any` | When the agent calls `gb stop` (advisory). The scheduler also enforces `wall_clock` here as a hard timeout. | End the current run, debit `max_runs`, fire next trigger normally. |
| `hard_stop.any` | Continuously by the scheduler / reaper | Kill the run pod (if any), close schedule with `terminated: hard_stop:<kind>`. Skips `on_loop_end`. |

### Stop-condition kinds

| `kind` | Args | Meaning |
|---|---|---|
| `max_runs` | — | `runs_completed >= max_runs` (0 = unlimited). |
| `wall_clock` | `duration` | Time since `created_at` (or `first_run_at`) exceeds duration. In `run_end`, it's per-run; in `loop_end`/`hard_stop`, schedule-wide. |
| `bead_query` | same as `scheduler.Condition.bead_query` | Existing `exit_condition` shape; `parent` filter (transitive) is supported per T2. **Closed-root short-circuit (ka-0000008051):** when a `parent` is named and that root bead is itself `closed`, the subtree count is forced to `0` (with a loud `slog.Warn` naming the masked residual work). A closed root epic is the operator's "done" signal — grafting follow-on work under it no longer keeps the loop alive forever. So a `loop_end` `{bead_query, op:"==", value:"0", parent:<root>}` becomes terminal, and a pre-flight `{op:">", value:"0", parent:<root>}` stops firing, once the root closes. **Externally-blocked exclusion (ka-0000002198):** set `exclude_blocked_by_open_types: ["escalation","decision"]` and any matched child that is `blocked_by` an OPEN bead of a listed type is subtracted from the count. This lets a `loop_end` `{op:"==", value:"0"}` distinguish *open+waiting-on-operator* (a child blocked on an open escalation/decision) from *open+stalled* (real work), so a gardener whose only remaining child is parked on an operator escalation stops instead of looping into a STALE-STOP grade. "Blocked by" mirrors `gb ready` (a forward `blocks`/`blocked-by`/`depends-on` dep to a non-terminal blocker — `open`/`in_progress`/`blocked`/`deferred`; only `closed`/`partial` clears the wait, ka-0000006285). Fail-safe: any uncertainty (scan >500 rows, fetch error, client without batch support) keeps the full count so the gardener never over-stops. Empty/omitted = legacy count-only. |
| `specific_bead_closed` | `bead_id` | The named bead has `status=closed`. |
| `gate_present` / `gate_absent` | `value` | Existing gate semantics. |
| `no_progress_for` | `runs` int OR `duration` | Progress-aware (see §3). Requires `progress` configured. |
| `consecutive_failures` | `value` int | `consecutive_failures >= value`. |
| `manual` | `reason_field` (optional) | Operator runs `gb schedule complete <id> --reason ...`; this kind matches its presence. |
| `manual_force` | `label` | Operator runs `gb schedule force-close <id>`; `hard_stop` only. |
| `yielded_then_woken_then_idle` | — | Standard "agent decided this run is done via wrap-up". |
| `no_actionable_state` | — | Progress-aware shortcut: "no in-flight crew and no decisions, stop the run". |
| `mr_merged` | `parent` (optional) | **Block-until-MR-merged (`loop_end` only, ka-0000007387).** Not a close — a **park**. While any tracked MR (`bead --produces--> merge-request`) under `parent` is unmerged, the gardener goes **dormant** (stays open + enabled, does **not** spawn) and auto-wakes when every tracked MR merges. `parent` defaults to the gardener's frontier epic (`assign_bead` / `gardener-epic:<id>`). See "Dormancy" below. |

### Precedence

`hard_stop` wins. Then `loop_end` at fire time wins over a fresh fire.
Then `run_end` is evaluated against the agent's state at `gb stop`.
Then a normal `gb stop` debits as today.

`goal_reached` in a wrap-up stays as the agent's "I unilaterally declare
done" override — it isn't a condition and lives outside `stop:`.

### Backwards compatibility

| Legacy field | Lowered to |
|---|---|
| `max_runs` | `stop.loop_end.any: [..., {kind: "max_runs"}]` |
| `exit_condition` | `stop.loop_end.any: [..., {kind: "bead_query", ...}]` |
| `run_timeout` | `stop.run_end.any: [..., {kind: "wall_clock", duration: <run_timeout>, when: "yielded"}]` |
| `consecutive_failures` / `max_retries` | `stop.hard_stop.any: [{kind: "consecutive_failures", value: <max_retries>}]` |

Note: `run_end.wall_clock` is a behavior change versus the legacy
`run_timeout` (which only reaped *yielded* runs). It will kill an
unyielded long-running pod; the default `run_end` slot is empty so
existing gardeners keep their current behavior unless they opt in.

### Retirement (formal archive)

**Retiring a gardener = closing its trigger bead.** There is no separate
`retired` lifecycle field: a gardener is *live* while its bead is `status=open`
and *retired* once the bead is `closed`. The retire affordances stamp the close
with `gardener_terminated_slot=operator` + `gardener_terminated_kind=retired` for
provenance, but the **single source of truth is the bead status**.

How retirement happens:

| Path | Effect |
|---|---|
| `/gardener/{id}/retire` button (beads-viewer) | Comments "retired by …", closes the bead with `terminated_kind=retired`. |
| `kd close <gardener-id>` | Same terminal effect (any close retires it). |
| A `loop_end` stop firing with `on_loop_end: close` | The gardener retires *itself* when it reaches its goal. |
| First-mate graceful-retire of a self-declared-done gardener | Closes the bead (reversible by reopen). |

**Everyone must honor retirement by checking bead status — and the live paths
already do:**

- **Scheduler / dispatch** — `LoadTriggers`
  (`internal/bridge/trigger_source_schedule.go`) lists only `status=open`
  triggers and skips `enabled=="false"`, so a retired (closed) gardener **never
  fires another run**. The wedged-trigger self-heal only reopens `in_progress`
  beads, never `closed` ones — a retire is permanent unless explicitly reopened.
- **Resume / escalate gate** — `gardenerResumeOrEscalate`
  (`internal/bridge/gardener_evaluator.go`) only re-arms a gardener that stopped
  *prematurely*; an operator-willed close (`slot=operator`, `kind=retired`) is a
  terminal disposition, not a premature stop, so it is never resurrected.
- **First mate & other meta-gardeners** — enumerate the fleet with `kd list
  --type trigger -l role:gardener` filtered to **`status` open** (see the
  first-mate prompt §1.2). A retired gardener is out of the working set: do not
  score it, re-plan it, or spawn a successor that re-fires it.
- **beads-viewer `/gardeners`** — defaults to the **Live** view (every non-retired
  gardener). Retired gardeners move to the **🗄 Archived** chip and are hidden from
  the default page (ka-0000004655).

To **un-retire**, reopen the bead (`kd update --status open <id>`); it re-enters
`LoadTriggers` on the next sweep. The engine's out-of-band re-arm normalization
(`gardenerRearmFromOperatorReopen`, bug ka-0000004191) then finishes the revive on
that first sweep — clearing the stale terminate markers and applying the durable
reset (ProgressState, resume bookkeeping, run_budget refill) so the reopen sticks
instead of re-closing within a cycle. The safe *order* of that reopen is
non-obvious — do it wrong and you get a revive that looks green and silently
re-closes, or one that strands stale counters. The procedure below is the one
validated live on the ka-0000005191 revival (task ka-0000002726).

#### Reviving a closed + enabled gardener — the safe order

**Reviving a retired gardener is NOT `gb trigger enable <id>`.** `LoadTriggers`
lists only `status=open` beads (and *then* skips `enabled=="false"`), so flipping
`enabled` on a *closed* gardener is a no-op — the bead is retired regardless of the
flag. `enabled=true, status=closed` is the normal fingerprint of a retired
gardener, not a broken one. **Reopening the bead is the revive.** Do the two
operator steps in this order, then let the engine finish:

1. **Bounds first, while the bead is still `closed`.** Check the backstop verdict
   (`gb gardener status <id> --json | jq '.has_backstop'`). If it is `false`, add
   the missing bound(s) — `run_budget > 0` and a `hard_stop` `wall_clock` in
   `(0, 720h]` — by patching the **nested `gardener` blob** with the read-patch-write
   jq recipe in [§"Editing a live self-block"](#gardener-bounded-stop-enforcement-global).
   Do **not** write the flat `run_budget`/`stop` fields — the daemon never reads
   them (bug ka-0000004562), so a gardener you *think* you bounded stays unbounded.
   Bounding before the reopen means the gardener is never simultaneously live and
   unbounded, and it lands `has_backstop: true` on the same sweep that reopens.
   *(No expiry arithmetic is needed anymore — see the wall_clock note below.)*

2. **Reopen with the terminate markers LEFT INTACT.** Run
   `kd update --status open <id>` and do **nothing** to
   `gardener_terminated_slot` / `gardener_terminated_kind` /
   `gardener_stuck_escalated`. This is the counter-intuitive step: a tidiness
   instinct says "clear the stale markers on the way in" — **do not.** An *open*
   gardener still carrying those markers is the exact fingerprint
   `gardenerRearmFromOperatorReopen` (`bridge/gardener_operator_reopen.go`) matches,
   and the engine only ever stamps them in the same write that *closes* a gardener.
   Matching that fingerprint is what triggers the durable reset on the next sweep:
   ProgressState cleared, `runs_finalized_after` stamped, `wall_clock_since`
   stamped to now, resume bookkeeping zeroed, `consecutive_failures` zeroed, and
   `run_budget` refilled — *then* the markers are cleared, in one write, so the
   normalization is one-shot. **Clearing the markers by hand skips all of that**
   and strands the stale counters (`consecutive_failures` still at its
   hard-stop threshold, an exhausted `run_budget`, a rebuilt ProgressState), which
   re-close or deadlock the gardener within a cycle (bug ka-0000004191). You do
   **not** need to zero any counter yourself — preserving the markers is precisely
   what delegates that to the engine.

3. **Verify.** `gb gardener status <id>` should immediately read `status=open`,
   `enabled=true`, `has_backstop: true`. Then within ~1 engine cycle (~30s) the
   terminate markers self-clear, a **"gardener re-armed from operator reopen"**
   comment appears on the bead, and `run_budget` shows the refill (observed on
   ka-0000005191: `run_budget 200→21`). If the markers are still set after a couple
   of cycles, the re-arm write is failing — check the daemon logs for
   `gardener operator-reopen: … failed`; the engine skips the fire and retries
   each cycle rather than firing against stale counters.

> **wall_clock-terminated gardeners (ka-0000006465).** A gardener that hard-stopped
> on `wall_clock` used to be *unrevivable in place*: the wall_clock window was
> measured from the bead's immutable `created_at` (floored at the grandfather
> epoch), with no resettable term, so a bare reopen re-tripped the same expired
> window on the next reconcile — a reopen→re-close loop. As of ka-0000006465 the
> revive paths stamp a per-bead `wall_clock_since` baseline to *now*, and the
> stop evaluator measures the window from `max(wall_clock_since, created_at,
> epoch)`. So the reopen above now grants a **fresh wall_clock window** and the
> gardener fires again on the same bead (ledger intact). No manual field edit is
> needed — reopening is sufficient. (Creating a brand-new gardener via
> `gb gardener start` still works but orphans the run/comment ledger, so prefer
> the reopen.)

### Gardener bounded-stop enforcement (global)

For **every** gardener the run-budget + hard wall-clock pair is not advice — it
is **enforced** (ka-0000006744; advice ka-0000000974). The invariant is GLOBAL,
not scoped to any label or project: an unbounded gardener is the single biggest
cost/RSI footgun in the fleet. (This supersedes the earlier dark-only guard,
declined by the operator in decision ka-0000003439 as the wrong axis.) A gardener
MUST carry **both**:

- `run_budget > 0` — a positive run cap (semantically validated: a positive
  integer, not just a present field), **and**
- a `hard_stop` `wall_clock` entry whose `duration` parses to a bounded,
  positive value `≤ 720h` (30 days). A `wall_clock` in `loop_end`/`run_end`
  does **not** count — only the `hard_stop` slot is a true emergency-brake
  ceiling. `"0s"`, `"-1h"`, `"forever"`, and `"100000h"` are all rejected.

Enforcement is **asymmetric by design** so the rule is maximally protective at
authoring time without knocking live gardeners offline mid-flight:

- **Authoring (fail-closed, all gardeners)** — `gb gardener start` and
  `gb trigger create` exit non-zero and write nothing for an unbounded gardener
  (the default `gb gardener start` blob is already bounded; this only bites when
  an operator hand-overrides `--max-runs 0` or `--loop-wall-clock forever`).
  `bridge.ValidateGardenerBlob` is the single source of truth.
- **Firing (warn-with-grace, all gardeners)** — when a live gardener bead is
  unbounded (hand-edited bead, pre-existing bead predating the guard, schedule
  mirror), the engine **warns once** (`warnUnboundedGardener` in
  `internal/bridge/triggers.go`) but still lets it fire. It is **not**
  auto-disabled — a fleet-wide auto-disable keyed on a check is exactly the
  blast-radius the operator declined. Re-author with both bounds to clear the
  warning.

There is **no label/project discriminator** — the rule is universal, so there
is nothing to mis-scope and no label-keyed fleet-wide auto-disable.

**Inspecting a gardener's bounds — use the shared verdict, never the flat
fields.** A gardener's self-block (`run_budget` + `hard_stop` `wall_clock`) lives
**only inside the nested `gardener` JSON blob** — the same blob `ParseTriggerDef`
decodes and `ValidateGardenerBlob` validates at fire time. The flat top-level
bead fields (`fields.run_budget`, `fields.stop`, `fields.loop_end`,
`fields.terminal_goal`) are **inert** — the daemon never reads them, and they
are `null` on **every** normally-authored gardener. Any survey or reviewer that
judges "is this gardener bounded?" by eyeballing those flat fields will
false-negative on every bounded gardener in the fleet (bug ka-0000004852: the
trigger-reviver reported "run_budget/stop/loop_end/terminal_goal all null" and
recommended revise-before-revive on a gardener that was already fully bounded and
passed the real guard).

So do **not** hand-parse fields. Ask the engine's own verdict:

```sh
gb gardener status <bead-id> --json | jq '.has_backstop, .self_block, .self_block_reason'
# true   "ok"    null            → both axes green (see the two axes below)
# false  "ok"    "bounded + …"   → progress-aware but NO engine backstop
# true   "weak"  "terminates …"  → backstop present but no wedge detector
```

**Two safety axes — do not conflate them** (bug ka-0000000849). "Is this gardener
bounded?" is actually **two orthogonal questions**, and a gardener can pass one
and fail the other:

| Axis | What it is | Where enforced | `gb gardener status` | audit |
|---|---|---|---|---|
| **backstop** | `run_budget>0` **and** a `hard_stop` `wall_clock` in `(0,720h]` — the emergency brake the engine/reaper fire unconditionally | `bridge.ValidateGardenerBounds` (fire-time guard + authoring guard) | `.has_backstop` / `backstop:` line (legacy `.bounded`/`.bounds_reason` still mirror it) | `has_backstop` on every line |
| **self-block** | a *progress-aware* (`no_progress_for` + a `progress` spec) or *goal-completion* (`bead_query`/`specific_bead_closed`/`max_runs`) `loop_end` — terminates when forward progress stops, per advice ka-0000000974 | advisory (audit sentinel) | `.self_block` (`ok`/`weak`/`none`) / `self-block:` line | `verdict` (`ok`/`weak`/`offender`) |

Neither implies the other. A gardener with `run_budget` + `no_progress_for` + a
progress spec but whose only `hard_stop` is `consecutive_failures` is
**self-block `ok`** yet **`has_backstop: false`** — exactly the shape of
ka-0000003772, which is why `gb gardener status` and
`gardener-autoblock-audit.py` used to look like they contradicted each other.
They don't: they measure different properties. Both tools now report **both**
axes, off the same decoded blob, so the verdicts are legible together. A fully
safe gardener wants **both** green: the backstop is the guaranteed ceiling; the
self-block is what stops it burning that whole ceiling on a wedged loop. Fixing
one does **not** add the other.

`gb gardener status` computes the backstop axis by running
`bridge.ValidateGardenerBlob` against the `gardener` blob — the **exact** guard the
authoring CLIs and firing engine use — and the self-block axis via
`bridge.ClassifyGardenerBlob`, which mirrors the audit classifier. The
human-readable output shows both on `backstop:` and `self-block:` lines.
`gardener-autoblock-audit.py --summary` reports how many classified gardeners lack
the backstop.

**Editing a live self-block — write the blob, never the flat field.** The same
inertness that trips up bounds surveys is a silent-*write* footgun (bug
ka-0000004562): `kd update <trigger> --field terminal_goal=…` (or `run_budget` /
`stop` / `loop_end`) writes the **flat top-level field the daemon never reads**,
so the goal/budget you *think* you tightened silently no-ops while the engine
keeps firing against the stale blob value. A gardener can then retire "goal met"
against a bar it never actually enforced. To change a gardener's self-block, edit
the **blob**:

```sh
# read the blob, patch the ONE key with jq, write it back whole
kd show <trigger> --json | jq -r '.fields.gardener' > /tmp/cap.json
jq '.terminal_goal = "…new goal…"' /tmp/cap.json > /tmp/cap.new && mv /tmp/cap.new /tmp/cap.json
kd update <trigger> --field gardener=@/tmp/cap.json
```

(`--field-set gardener.terminal_goal=…` does **not** work — the blob is a
JSON-encoded *string*, not a map, so the dotted-set path errors with "JSON-string
value does not contain a JSON object".)

`gb gardener status` guards against having done this by mistake: it emits a
`⚠️  DIVERGENCE <field>` banner (and a `field_divergences[]` array in `--json`)
whenever a flat self-block field is **present and disagrees** with the blob the
engine reads. A normally-authored gardener — all flat self-block fields `null`,
the self-block only in the blob — shows zero divergences, so the banner only
fires on a real dead-write. Surveyors that already consult `.has_backstop` /
`.self_block` should also treat a non-empty `.field_divergences` as an
actionable finding.

**Kill-switch.** Open a `gate` bead titled exactly **`gardeners-pause`**
(`kd create "gardeners-pause" --type gate`) to halt **all** gardener firing
fleet-wide; close it to release. This is the only firing-time hard stop — it is
operator-engaged, evaluated each cycle in `gardenerPreFireCheck`
(`internal/bridge/gardener_evaluator.go`), and does **not** modify gardener beads.

From Slack, prefer **`/schedule off`** — it opens both the `gardeners-pause`
gate (this kill-switch) *and* the `scheduler-pause` gate (silences cron-fired
`trigger:schedule` + `schedule-mirror` sources) in one shot, so all time-based
activity stops while event-driven GitLab/Jira triggers keep firing. `/schedule
on` closes both. See `internal/bridge/bot_schedule_cmd.go`. (`/trigger off` is
the broader hammer — it pauses the *entire* trigger engine including
GitLab/Jira.)

**MR-generation caps (rate + concurrency).** The bounded-stop guard bounds how
many *times* a dark gardener spawns; two additional caps bound the MR **blast
radius** so a runaway gardener cannot flood review or burn CI minutes by opening
MR after MR (R6-FU6c, ka-0000002822, `internal/bridge/dark_mr_cap.go`). Both are
evaluated in `gardenerPreFireCheck` for dark gardeners only — **non-dark gardeners
never reach them**:

- **Fleet concurrent-open-dark-MR cap** (`darkMaxConcurrentOpenDarkMRs`, 40):
  while the total number of OPEN dark MR beads across the fleet is at/over the
  ceiling, **every** dark gardener's fire is suppressed
  (`dark_mr_fleet_cap`) until the open-MR backlog drains via review / merge /
  close. Like the kill-switch, this modifies no gardener bead — it is
  self-releasing.
- **Per-gardener MR-rate window** (`darkMaxMRsPerGardenerWindow`, 8, over
  `darkMRPerGardenerWindow`, 1h): when a **single** gardener has produced that
  many dark MRs inside the rolling window, just that gardener pauses
  (`dark_mr_per_gardener_cap`) until its older MRs age out of the window or
  close. Attribution walks each open dark MR back to its producing task beads
  (the reverse `produces` edge); an MR counts against the gardener when a
  producing task carries the gardener's `trigger_id`/`schedule_id` or is the
  gardener's `assign_bead`.

A "dark MR" is identified by the MR bead's `mr_url` host (the **gitlab-dark**
host marker) or a dark label; read-only **upstream-mirror** beads
(`external-mirror:true` / `external_mirror=true`) are excluded — they are not
dark-gardener products. Both caps **fail-closed** on a confirmed over-limit count
and **fail-open** on a daemon read error (a flaky read must never freeze the
dark universe; the bounded-stop guard + kill-switch are independent backstops).
The caps are fleet safety rails, not per-gardener knobs — they are tuned in code,
not on the gardener bead.

### Dormancy: parking a gardener instead of spinning it (ka-0000003082, ka-0000007387)

A gardener whose entire remaining frontier is **waiting on a human** — an
unresolved operator escalation/decision, or green-but-unmerged MRs — has nothing
to do until that human acts. Left un-braked it wakes on every cron tick, re-derives
"nothing to do, waiting on a human," and closes with `progress_score: 1` for
"reflagging the escalation." That reflag-churn is invisible to oversight (it reads
as progress) yet burns a pod cold-start every cycle — one merge-lane gardener burned
~110 spawns over 24h this way (ka-0000007387).

The brake is **dormancy**: the gardener stays **open + enabled** (so the cron
preflight can still wake it) but its fire is **suppressed** each cycle until the
blocker clears. No spawn, no close, no watchdog reopen-churn. Every park also comments on the frontier epic, naming the blocker, so the operator sees it on the plan. Four paths enter
dormancy, discriminated by an engine-owned `dormant_blocker_kind` that selects the
wake predicate:

| `dormant_blocker_kind` | Enters when | Wakes when |
|---|---|---|
| *(empty)* | Resume budget exhausted **and** the frontier is fully wedged: an open operator escalation/decision sits on it and the actionable-child count is **0** (legacy path, ka-0000003082). | Same as `operator_wedge`: the frontier is no longer fully wedged. Until gd-FUC635HJ2E it waited for *every* open escalation/decision in the subtree to close, so one owner-at-device escalation parked plans with actionable work for days. |
| `operator_wedge` | **Proactively**, before any resume bookkeeping: the *entire* open frontier is single-threaded behind open operator escalation/decision beads — actionable-child count (via `exclude_blocked_by_open_types`) is **0** (ka-0000007387 Prong 2). | The frontier is no longer fully wedged (an actionable child appears, or the epic closes). |
| `mr_merged` | The gardener **declared** `{"kind":"mr_merged","parent":<epic>}` in `stop.loop_end` and a tracked MR under `parent` is unmerged (ka-0000007387 Prong 1). | Every tracked MR (`bead --produces--> merge-request`, `mr_outcome=merged`) under `parent` is merged. |
| `resume_budget_spent` | Resume budget exhausted, the frontier is **not** wedged, and the gardener drives an open plan epic. It keeps the `gardener-stuck-escalate` label and stamps `dormant_activity_digest`, the id:status of every open/in_progress/blocked bead under the plan (gd-dNxyjFLGRJ). Before this it was closed, and the plan lost its gardener. | The digest changes: a step closes, reopens, starts, returns to ready or is added. The wake refills `run_budget` and resets the resume budget. A read error keeps it parked. A gardener with no open plan still closes. |

The proactive `operator_wedge` path is the reachability fix: the legacy dormancy
check lived *after* the resume-budget gate, so a gardener that scored
`progress_score=1` every cycle (reflag-churn) pinned `resumes_without_progress` at
0, the budget never exhausted, and the dormancy check was never consulted. The
proactive gate does not depend on that counter — it parks a provably-wedged gardener
directly, on the first cycle it is wedged.

**Declaring block-until-MR-merged (the operator-facing facility).** Add one entry
to your gardener's `stop.loop_end`:

```json
{
  "loop_end": {"any": [
    {"kind": "mr_merged", "parent": "ka-myEpic"},
    {"kind": "bead_query", "op": "==", "value": "0", "parent": "ka-myEpic",
     "types": ["task","bug","epic"], "statuses": ["open","in_progress","blocked"]}
  ]},
  "hard_stop": {"any": [{"kind": "wall_clock", "duration": "168h"}]}
}
```

Now the gardener sleeps whenever its work is blocked on a merge it cannot perform,
and resumes the instant the human merges — no spawns wasted in between. `parent` is
optional; omit it to scope tracked MRs to the gardener's frontier epic
(`assign_bead` / `gardener-epic:<id>`).

The `hard_stop` `wall_clock` entry is still mandatory to *author* (the bounded-stop
guard rejects a gardener without one). But note it is **not evaluated while the
gardener is dormant** — dormancy is a park, not a ceiling, and short-circuits before
the stop spec each cycle (this is the pre-existing behavior of every dormant
gardener, not new to `mr_merged`). A parked gardener spawns no pods, so the runaway-
cost the `wall_clock` guards against does not apply while it sleeps; the wall-clock
bounds a gardener that is actively *firing*. The escape hatches from an `mr_merged`
park are the bead graph itself — every tracked MR merges, the producing children
close, or the parent epic closes — plus an operator reopen/retire.

**Everything fails safe toward *staying live*.** Any daemon read error, a truncated
subtree scan, a closed parent, or an ambiguous count leaves the gardener awake for
one more normal cycle — a wrongful non-park is a self-correcting no-op, whereas a
wrongful park is avoided by construction (a park requires a positively-proven
blocker). Implementation: `internal/bridge/gardener_block_on_mr.go` (gates + wake
predicates), wired in `gardenerPreFireCheck`.

## 3. Progress conditions

Progress is the new piece — neither trigger nor stop existed without it.
It instruments the question "did this run actually move the loop
forward?" so that wedge detection (`no_progress_for`) and self-attestation
in wrap-up are mechanically checkable.

### Schema

```json
{
  "events": [
    {"kind": "child_closed",       "parent": "{{assign_bead}}"},
    {"kind": "decision_resolved",  "raised_by_agent_subtree_of": "{{assign_bead}}"},
    {"kind": "mr_merged",          "author_label": "gardener:{{schedule_id}}"},
    {"kind": "crew_dispatched",    "role": "crew", "parent": "{{assign_bead}}"},
    {"kind": "wrapup_field",       "field": "progress_score", "op": ">=", "value": "1"}
  ],
  "windows": [
    {"name": "this_run",     "scope": "since_run_start"},
    {"name": "last_3_runs",  "scope": "last_n_runs", "n": 3},
    {"name": "last_24h",     "scope": "since", "duration": "24h"}
  ],
  "surface": {
    "in_prime": true,
    "in_squawk_on_loop_end": true,
    "in_wrapup": true
  }
}
```

### Three pieces

1. **Events** — what counts as progress. Generic shapes (bead transitions,
   decision transitions, MR transitions, wrap-up fields). Each event is
   matched against the daemon's history within the current window.
2. **Windows** — named time / run scopes. `this_run`,
   `last_3_runs`, `last_24h` are the sensible defaults; operators can add
   more.
3. **Surface** — where progress shows up to humans and to the gardener
   prompt:
   - `in_prime` → a `## Loop Progress` section in `gb prime` output.
   - `in_squawk_on_loop_end` → a Slack squawk summary when the loop ends.
   - `in_wrapup` → `gb stop` rejects wrap-ups missing `progress_score`
     (int) and `progress_summary` (string).

### The Progress block in `gb prime`

When `progress.surface.in_prime: true`, prime renders something like:

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

The gardener reads this at the top of each run. The same counters feed
the `no_progress_for` stop kind: "if window `last_3_runs` has zero
events of kind `child_closed | mr_merged`, fire `loop_end`".

### The `progress_score` wrap-up convention

When `progress.surface.in_wrapup: true`, gardeners MUST fill
`progress_score` (integer ≥ 0) and `progress_summary` (short string) in
their `gb stop --wrapup`. Convention:

| Score | Meaning |
|---|---|
| `0` | Did nothing material (yielded, woke, nothing actionable, stopped to wait for next tick). |
| `1` | Incremental progress (dispatched a crew, resolved one decision, pinged one stale escalation). |
| `≥2` | Substantive progress (closed a child, merged an MR, completed a milestone). |

`no_progress_for: {runs: 3}` triggers when the last 3 closed runs all
have `progress_score == 0`.

## 4. Shared condition DSL

Every place that needs "is this true?" — trigger preflight, trigger
match, stop bead_query, progress event matching — uses the same
recursive expression schema:

```json
// Leaf (daemon-side)
{"type": "bead_query", "op": "==", "value": "0", "parent": "{{assign_bead}}",
 "types": ["task"], "status": ["open"]}

// Boolean composition
{"all": [<expr>, <expr>, ...]}    // AND
{"any": [<expr>, <expr>, ...]}    // OR
{"not": <expr>}                   // NOT

// Field reference (event-payload leaf)
{"field": "label:cycle:failed", "op": "present"}
{"field": "parent", "op": "==", "value": "{{assign_bead}}"}
{"field": "raised_by_agent.task_id", "op": "in_subtree_of",
 "value": "{{assign_bead}}"}
```

### Leaf shapes

| Shape | Use | Where evaluated |
|---|---|---|
| `bead_query` | "How many beads match X?" | Daemon-side count query (preflight, stop). |
| `field/op/value` | "Does this event/payload field match?" | In-process against trigger event payload (trigger match), bead state (bead-event match), decision state (decision-resolved match). |
| `gate_present` / `gate_absent` | "Does a gate bead by this title exist?" | Daemon (preflight, stop). |
| `ready_queue` | "How many *ready* (open, no open deps) issue beads match?" — `gb ready` semantics; honors `labels` / `exclude_labels`. | Daemon (preflight, stop). |
| `active_agents` | "How many active agent beads match?" — honors `project` / `role`. | Daemon (preflight, stop). |

### Field operators

`==`, `!=`, `in`, `not_in`, `contains`, `present`, `absent`,
`in_subtree_of`. The last requires a daemon ancestry helper
(`bead.IsAncestor(parent, child)`).

### Template variables

Strings inside any condition or trigger entry are template-expanded at
evaluation time:

| Variable | Source |
|---|---|
| `{{schedule_id}}`, `{{schedule_title}}` | The schedule bead. |
| `{{assign_bead}}` | `schedule.assign_bead` field. |
| `{{run_number}}`, `{{runs_remaining}}` | Scheduler counters. |
| `{{event.<path>}}` | Event payload (only inside `match` expressions). |
| `{{progress.<window>.<kind>}}` | Progress counters (only inside `stop` and `match` expressions). |

This unlocks "fire on bead_event matching parent={{assign_bead}}"
without hand-editing the schedule bead per consumer.

## 5. Worked examples

### 5.1 Epic gardener

The original feature request: `@gardenio <epic-id>` spawns a looping
gardener that drives all children of the epic to closure.

> **What `gb gardener start` actually emits today.** The full structured
> shape below (inline `triggers:` / `stop:` / `progress:` blobs, plus
> `assign_bead`) is the engine's *capability* — the trigger-engine decodes
> all of it. But the live `type:trigger` config schema (config bead
> `ka-0000001965`) does **not** yet allow those keys, so bead creation
> rejects them. `gb gardener start` therefore emits the **bare** firing
> shape the known-live gardeners use: `type=trigger` / `trigger:schedule` /
> `action=spawn_agent`, with a flat `cron` + `conditions` pre-flight
> (open-children-of-epic gate) and the epic pinned via a `gardener-epic:<id>`
> label. The terminal goal, run budget, and wall-clock caps are folded into
> the gardener **prompt** (the agent owns its own `gb stop`), and the cron
> pre-flight makes the loop go dormant once the epic's children all close.
> To author the rich shape below, the `type:trigger` config schema must
> first be widened to permit `assign_bead` / `stop` / `progress` / `triggers`.

```yaml
type: trigger
title: "Gardener for ka-0000007308 (Slack ratings v2)"
fields:
  enabled: "true"
  role: gardener
  project: gardenio
  assign_bead: ka-0000007308
  prompt: "<gardener playbook prompt — see plan appendix>"

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

What this expresses:

- **Trigger:** wake on cron (every 30 min) when there is open work; wake
  immediately on any child transition; wake when one of my crew's
  decisions resolves; allow operator manual fire.
- **Stop:** close cleanly when all children are closed, or when the epic
  itself closes, or after 20 runs, or after 3 consecutive zero-progress
  runs, or after a 1-week absolute backstop. Each run ends on yield+woke,
  no actionable state, or 30-minute wall clock. Hard-stop on 5
  consecutive failures.
- **Progress:** count child closures, decision resolutions, MR merges,
  and crew dispatches; render in prime; require gardener to self-attest
  with `progress_score`.

### 5.2 Cycle-watchdog

Pure event-driven loop, no cron. Fires whenever a task bead closes with
the `cycle:failed` label.

```yaml
type: trigger
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

Today this requires a `trigger:bead` bead that can't share a `max_runs`
budget with anything; under the new primitive it lives on the same
schedule shape as cron-driven loops.

### 5.3 Ready-queue drainer

Cron with rich preflight: only fire if there is open project work AND
either we have crew headroom or there is no deploy freeze.

```yaml
type: trigger
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

Demonstrates `any:` / `all:` composition and the `gate` leaf gating
preflight.

### 5.4 Hybrid gardeners: responsive + polling wake

Many real gardeners are *poll-and-react* loops: they sweep on a cron just
in case, but the thing they care about is a bead transition that the
scheduler already knows how to deliver as a `bead_event`. Wiring the
event as a second trigger entry alongside the cron makes the gardener
**responsive** (fires within a tick of the actual transition) without
giving up the **eventually-correct** safety net (the cron sweep catches
anything the event stream missed — restarts, dropped SSE frames,
mis-built payloads).

#### Worked conversion: cycle-watchdog (ka-0000001612)

Cycle-watchdog inspects recently-closed `gardener-{intake,delivery,release}`
agent beads, classifies each as GREEN or FAILED, and squawks the result
to Slack. Today it polls every 30 minutes; in the worst case a FAILED
cycle sits unflagged for ~30 minutes after the gardener closes.

**Before** — pure cron:

```json
[
  {"kind": "cron", "cron": "17,47 * * * *", "timezone": "UTC"}
]
```

**After** — hybrid cron sweep + bead_event:

```json
[
  {"kind": "cron", "cron": "17,47 * * * *", "timezone": "UTC"},
  {"kind": "bead_event",
   "events": ["closed"],
   "match": {"all": [
     {"field": "watch_type", "op": "==", "value": "agent"},
     {"field": "watch_role", "op": "in",
      "values": ["gardener-intake", "gardener-delivery", "gardener-release"]}
   ]},
   "throttle": "60s"}
]
```

Plus a `stop` block (the legacy bead had no `stop:` at all; this just
codifies the existing `consecutive_failures` runtime field as an
explicit hard-stop):

```json
{
  "hard_stop": {"any": [
    {"kind": "consecutive_failures", "value": "5"}
  ]}
}
```

#### Why this shape

- The cron entry stays the same — same expression, same timezone. It is
  now the **sweep / fallback**, not the primary signal. Two reasons it
  stays: (1) the scheduler trigger engine is process-local and replays
  at most one debounce window's worth of buffered events on restart, so
  a controller restart can drop a `closed` event; (2) the `closed` event
  carries the *bead's* labels at close time, which means a gardener that
  closes without its `role:` label (mis-built agent, label race) would
  not match the bead_event entry — the cron sweep catches it.
- The `bead_event` entry uses `events: ["closed"]` (not `updated`) because
  only `beads.bead.closed` is currently wired into the scheduler's
  trigger engine — see `gardenio/controller/internal/scheduler/nats_bead_closed.go`.
  When `updated` and `created` listeners ship, gardeners that need
  status-flip semantics rather than terminal closure can fan them in
  here.
- The Match expression mirrors the watchdog prompt's existing
  last-mile filter (`fields.role ∈ {gardener-intake, gardener-delivery,
  gardener-release}`). The `watch_type` and `watch_role` payload fields
  are populated by `BeadClosedEventListener.handle` from the bead's
  labels, so the gardener doesn't have to introspect the raw bead.
- The 60-second `throttle` prevents a burst of cycle closures
  (gardener-delivery + gardener-intake closing in the same tick) from
  spawning two runs back-to-back; the second event wins via
  last-write-wins and waits out the window.
- `stop.hard_stop.consecutive_failures = 5` is the explicit form of the
  legacy `max_retries` field. The watchdog runs hot (one fire per cycle
  close on top of the cron sweep), so the auto-disable backstop matters
  more here than in a tame cron-only gardener.

#### Cron-as-fallback reasoning

The cron sweep is **not** redundant just because the bead_event entry
exists. Keep it whenever any of the following is true:

1. **Event delivery isn't guaranteed.** SSE/NATS replays at most one
   debounce window's worth of events on reconnect; a controller restart
   between event emission and ingest drops the event silently. The cron
   sweep gives an eventually-correct backstop.
2. **The match predicate depends on bead state that may not be on the
   payload yet.** `BeadClosedEventListener` populates `watch_role` from
   the bead's labels at close time. If a bead is closed without its
   `role:` label, the bead_event match drops it; the cron sweep can
   still pick it up by polling the daemon directly.
3. **The gardener wants to verify a sliding window**, not just react to
   one event. Cycle-watchdog cares about *all* cycles in the last 35
   minutes, not just the one that triggered it — the cron sweep enforces
   that completeness guarantee.

If none of these apply (e.g. the gardener only cares about one specific
bead-ID's closure and the loop ends after that), drop the cron entry and
use the pure event-driven shape from §5.2 instead.

#### Field-validation matrix

The `==` and `in` operators used above are exercised under live event
flow by the field-validation matrix landed in
[ka-0000002765](https://beads.gardenio.e2e.example.com/beads/ka-0000002765)
(MR !2478, scenarios under `tests/e2e/claudeless/`). The matrix asserts:
parent==, in_subtree_of, label-predicate, decision_resolved Match, throttle
drop, debounce coalesce, stop.loop_end satisfied, stop.hard_stop
consecutive_failures, and cron+bead_event fan-in. When adding new
hybrid gardeners, point new Match operators at that matrix so silent
Match failures get caught before they reach a polling-masked production
loop.

## 6. Debugging a gardener

When a gardener is misbehaving, walk the three families in order.

### "It isn't firing"

0. **Is it actually a `type=trigger` bead?** Phase 3b/D10 removed
   `type=schedule` firing entirely — the trigger-engine only loads and
   fires `type=trigger` beads (`trigger:schedule` via the schedule source).
   A bare `type=schedule` gardener loads into **no** source and silently
   never runs (bug ka-0000005960). `kd show <gardener-id>` — confirm
   `type=trigger` and label `trigger:schedule`. If you see `type=schedule`,
   it is dead; re-create it with `gb gardener start <epic>` (which now emits
   the correct shape) or `gb trigger create schedule … --action spawn_agent
   --role gardener`.
1. **Is it enabled?** `kd show <gardener-id>` — confirm `enabled=true`,
   `status=open`. The scheduler auto-disables on `consecutive_failures
   >= max_retries`.
2. **Did the trigger source actually receive the event?** For event
   kinds, look for the `trigger:<schedule-id>` audit beads (`kd list
   --label trigger:<id>`). No audit bead means the source never fired.
3. **Did `preflight` fail?** A preflight-false fire produces a log line
   in the controller pod and does NOT mark the dedup key. Re-run the
   `bead_query` by hand against the daemon (`kd list --parent
   <assign_bead> --status open`) to see what the scheduler saw.
4. **Did `match` reject the event payload?** Match runs in-process
   against the event payload — controller logs include the payload and
   the leaf-level evaluation result.
5. **Did `throttle` / `debounce` drop it?** Each entry's last-fire
   timestamp is in the schedule's `trigger_state` field.
6. **Is `max_concurrent` reached?** With one run already live (and one
   queued), additional fires drop. `kd list --type agent --status
   in_progress --label schedule:<id>` shows live runs.
7. **Is the single-live-instance gate deferring the fire?** A gardener
   defaults to **one live run per trigger** (`max_concurrent_runs`,
   default 1) — a late/catch-up cron fire is deferred while a prior run is
   still `spawning`/`working`, so the gardener does not double-spawn
   (ka-0000003097). Look for `gardener max_concurrent_runs: at live-run cap,
   deferring fire` in the controller log, or count live runs with
   `kd list --type agent --status open,in_progress -l spawned_by:trigger:<id>`.
   A gardener that genuinely wants bounded fan-out raises the cap by setting
   `max_concurrent_runs` > 1 on the trigger bead.

### "It keeps firing but never finishes"

1. **Check `progress`.** `gb prime` output (or the `## Loop Progress`
   section if you can grep the run pod logs) shows the windows. All-zero
   counters mean the loop is wedged.
2. **Add or tighten `no_progress_for`.** The most common fix is shipping
   a `no_progress_for: {runs: 3}` entry in `loop_end.any`.
3. **Check the gardener's wrap-ups.** `kd list --type agent --label
   schedule:<id> --status closed` then `kd show <agent>` for each — the
   wrap-up `progress_score` should match what the daemon sees. If the
   gardener claims `progress_score=2` but no `child_closed` events
   landed, the gardener prompt is mis-attesting.
4. **Look at `stop.loop_end` evaluations.** The scheduler logs each
   slot evaluation per reconcile tick. A `bead_query` returning a
   different count than you expect usually means the `parent:` filter
   is missing or the recursion-depth cap (default 2) is too shallow.

### "It stopped suddenly with `terminated: hard_stop:...`"

`hard_stop` skips `on_loop_end`, so there is no graceful followup. The
two common causes:

- `consecutive_failures` — the run pods themselves are crashing or
  failing CI before reaching `gb stop`. Look at the agent beads' exit
  codes and pod logs.
- `wall_clock` (1 week absolute) — the loop has been alive too long.
  Either the workload genuinely needs more than the cap (raise it) or
  the `loop_end` conditions are wrong (the loop should have ended
  earlier on `bead_query` / `no_progress_for`).

### "Why is this run happening?"

`gb prime` renders a "Last fire trigger" line in the Scheduled Loop
Context block. If the line says `decision_resolved on ka-DEC1`, the
gardener woke because a downstream crew decision resolved. If it says
`cron`, the cron entry's preflight passed. Use this to branch the
gardener's behavior on cause without it having to introspect the
trigger array.

### Manual operator handles

| Action | Command |
|---|---|
| Fire a `manual` trigger entry now | `gb schedule fire <schedule-id>` |
| Soft-close the loop (lets `on_loop_end` run) | `gb schedule complete <schedule-id> --reason '...'` |
| Hard-close (kill any live run pod) | `gb schedule force-close <schedule-id>` |
| Inspect last-fire / progress state | `kd show <schedule-id> --json` and look at `trigger_state`, `progress_state` |

## 7. Scheduler internals — where to look in code

When the runbook isn't enough and you need to read the source:

| Concern | Path |
|---|---|
| Condition DSL parser + evaluator (T1) | `gardenio/controller/internal/scheduler/conditions/` (`expression.go`, `eval.go`, `leaf_daemon.go`, `leaf_field.go`). |
| Condition parse/lowering rules | `gardenio/controller/internal/scheduler/condition.go`. |
| Cron evaluation | `gardenio/controller/internal/scheduler/cron.go`. |
| Gardener stop-spec evaluator (type=trigger path) | `gardenio/controller/internal/scheduler/gardener_stop.go` (`EvaluateGardenerStopSpec` / `EvaluateGardenerStopEntry`). `no_progress_for` reads `ProgressState` from the `GardenerStopContext`; `no_actionable_state` / `yielded_then_woken_then_idle` remain not-met on triggers (need live-run state the bridge does not track). |
| Gardener progress recording (type=trigger path) | `gardenio/controller/internal/bridge/gardener_evaluator.go` (`gardenerAgentTracker` calls `progress.FinalizeRun` per closed run when a `progress` spec is set) and `gardener_hooks.go` (`gardenerDiscoverProgress` credits events). Both write back into the gardener JSON's `progress_state`. |
| Trigger sources (legacy `type=trigger`) | `gardenio/controller/internal/bridge/trigger_source_*.go`. |
| Trigger system reference (the broader `trigger:*` engine) | [`gardenio/docs/triggers.md`](triggers.md). |
| Plan-of-record (canonical semantics) | [`gardenio/plans/epic-mention-looping-gardener.md`](../plans/epic-mention-looping-gardener.md). |

### Schedule/trigger field reference (you do NOT need to read the source)

Before reaching for `internal/scheduler/`, note that the **field semantics**
for every schedule/gardener field are already documented — you should not have
to reverse-engineer them from code:

| Field(s) | Reference |
|---|---|
| `schedule_runs_spawned`, `schedule_max_runs`, `schedule_terminal_goal`, `schedule_prior_agent_id`, `goal_reached` (the `## Scheduled Loop Context` block `gb prime` injects on a scheduled run) | agent-facing: [`gardenio/docs/agent-triggers-guide.md` — "Where Scheduled Loop Context appears in `gb prime`"](agent-triggers-guide.md#where-scheduled-loop-context-appears-in-gb-prime) |
| `max_runs` / `run_budget`, `terminal_goal`, `exit_condition`, `run_timeout`, scheduler-managed counters (`runs_spawned`, `runs_completed`, `consecutive_failures`, `last_run`, `last_agent_id`), and the wrap-up signals (`goal_reached`, `goal_reason`, `accomplishments`) | operator field tables: [`gardenio/docs/triggers.md` — Run-budget & terminal goal / Counters / Wrap-up signals](triggers.md#run-budget--terminal-goal-ephemeral-schedules-gardener-primitive) |

(On the unified `type=trigger` gardener path the same accounting lives in the
serialized `gardener` JSON; `run_budget` and the `stop:`/`progress:` field shapes
are specified in §1–§3 above.)

When the runbook and the code disagree, the code wins for behavior, the
plan wins for intent — file a bug if they have drifted apart.

## 8. Wake-condition recipe cookbook

This section is a copy-paste catalogue for the most-wanted opportunistic
wake patterns — turning a cron-only gardener into one that fires *when
something actually changes*. Each recipe is a `triggers:` array entry you
drop straight onto the `type=trigger` bead. The schema, knobs, and engine
semantics are in §1; the hybrid pattern's rationale is in §5.4. This
section is the lookup table.

Source of truth for the payload shapes below:
`gardenio/controller/internal/scheduler/nats_bead_closed.go` (close path),
`gardenio/controller/internal/scheduler/nats_decision.go` (decision path),
`gardenio/controller/internal/bridge/trigger_source_multi.go`
(`beadEventToPayload`, the created/updated/closed SSE path), and the match
evaluator `gardenio/controller/internal/scheduler/conditions/leaf_field.go`.

### 8.1 The payload-shape rule (read this first)

A `bead_event` / `decision_resolved` `match` is evaluated **only against
the event payload** — `lookupField` in `leaf_field.go` reads `env.Payload`
and nothing else. The daemon is consulted **only** for `in_subtree_of`
(ancestry walk) and for the daemon leaves (`bead_query`, `ready_queue`,
`gate`, …) used in `preflight`. **A plain field leaf on a key the payload
doesn't carry resolves to "not present", and every non-`absent` operator
over a missing field returns false — so the entry silently never fires.**
This is the #1 reason a hand-written wake condition looks right and does
nothing.

So the only question that matters per recipe is: *which keys are on the
payload for this event?* There are **two** ingestion paths feeding the same
engine, and they expose **different** payload shapes:

| Path | Wired in | Events delivered | Payload keys available to `match` |
|---|---|---|---|
| **SSE full-payload** (`MultiTriggerSource.RegisterSSE` → `beadEventToPayload`) | `created`, `updated`, `closed` | the full bead | `id`, `type`, `title`, `status`, `assignee`, `created_by`, `priority`, `closed_by`, **plus every custom field** (`mr_state`, `mr_pipeline_status`, `parent`, …), plus `labels` (via `label:<name>` leaves) |
| **SSE edge-hydrated** (`MultiTriggerSource.RegisterSSE` → `hydrateEdgeEvent` → `beadDetailToPayload`) | `dependency.added`/`removed`, `label.added`/`removed`, `comment.added`/`deleted`, `follower.added`/`removed`, `bead.deleted`, `bead.transferred` | the hydrated affected bead (one `GetBead`) | same full-bead keys as the SSE full-payload row, **plus** `topic` (the originating edge topic) and `edge_event` (the short event type). Fails open: a hydration miss skips the event, never fires an id-only one. See §8.6b |
| **NATS close listener** (`BeadClosedEventListener`) | `closed` only | a fixed projection | `watch_bead_id`, `watch_type`, `watch_project`, `watch_role`, `closed_by`, `title`, `status`, plus decision-answer keys (`chosen`, `response_text`, …) when the closed bead is a decision, plus `labels` |
| **NATS decision listener** (`DecisionEventListener`) | `decision_resolved` | a fixed projection | `decision_id`, `question`, `urgency`, `requested_by`, `option_count`, `chosen_index`, `chosen_label`, `resolved_by`, `rationale`, `watch_project`, `watch_role`, `watch_bead_id`, plus `labels` |

Practical consequence: in production both the SSE handler and the NATS
close listener run, so a `closed` event arrives **twice** — once with the
full payload, once with the `watch_*` projection. Write your `match` so it
is satisfied by **whichever shape carries the field you need**, and lean on
`throttle` to collapse the duplicate fire. When you only need
`type`/`status`/custom-fields, match on the SSE shape (`type`, `parent`,
`mr_pipeline_status`); when you need a decision answer or the `watch_role`
projection, match on the NATS shape. Do **not** mix keys from both shapes
in one `all:` block — no single event carries both `type` and `watch_role`,
so an `all:` over both never matches.

### 8.2 Recipe: wake when one of my MRs goes green

For delivery / release gardeners that want to act the moment CI passes.
GitLab MR state is mirrored to `type=merge-request` beads carrying
`mr_state`, `mr_pipeline_status`, `mr_pipeline_url` (gitlab-sync's
`UpsertMergeRequestBead`). Those are custom fields, so they ride the **SSE
full-payload** path on `updated`/`closed`:

```json
{"kind": "bead_event",
 "events": ["updated", "closed"],
 "match": {"all": [
   {"field": "type", "op": "==", "value": "merge-request"},
   {"field": "mr_pipeline_status", "op": "==", "value": "success"}
 ]},
 "throttle": "120s"}
```

Scope it to *your* MRs so you don't wake on every green pipeline fleet-wide
— add a label or author leaf:

```json
   {"field": "label:project:gardenio", "op": "present"},
   {"field": "created_by", "op": "==", "value": "<your-bot-name>"}
```

Freshness (see §8.7): this is **near-real-time**, not sync-bound. GitLab
fires a pipeline webhook that stamps `mr_pipeline_status` onto the MR bead
immediately (`handlePipelineWebhook`), and the resulting `bead.updated`
event rides the SSE full-payload path into the wake engine — so you wake
within webhook + SSE delivery latency of the pipeline flip. The gitlab-sync
poller is only a merged-MR fallback; it does not gate open-MR pipeline
freshness. Keep a cron floor (§8.6) only as insurance against a dropped
webhook or SSE frame.

**First-class flags (task ka-0000005598).** For the common "drive ONE MR to
merge" gardener, `gb gardener start` exposes two opt-in flags that expand to the
JSON above — no hand-rolled `triggers`/`stop` needed:

- `--wake-on-mr <mr_url>` — flips the gardener to event mode (`trigger:multi`)
  and adds a `bead_event` entry scoped to that exact MR's mirror bead
  (`type == merge-request` AND `mr_url == <url>`, on `created`/`updated`/
  `closed`), so it wakes on the FIRST update to the MR (new commit, comment,
  pipeline-status flip, or merge). A fallback cron floor carrying the
  open-children preflight is kept as insurance.
- `--stop-on-mr-merged <mr_url>` — adds a terminal `specific_mr_merged`
  `loop_end` entry: the gardener CLOSES as success the moment that exact MR's
  mirror bead reports merged (`mr_outcome`/`mr_state == merged`). Unlike the
  `mr_merged` subtree PARK (§7), this is scoped to one URL and is terminal
  (reaching the merge IS the goal). Both flags are optional and compose.

### 8.3 Recipe: wake when an escalation resolves

Escalations are beads that close when resolved. Match on the SSE `closed`
shape (`type` is a top-level column on both shapes, but only the SSE path
carries `parent`/custom fields if you want to scope further):

```json
{"kind": "bead_event",
 "events": ["closed"],
 "match": {"all": [
   {"field": "type", "op": "==", "value": "escalation"},
   {"field": "label:project:gardenio", "op": "present"}
 ]},
 "throttle": "60s"}
```

To wake only on escalations under *your* epic, swap the label leaf for an
ancestry leaf (this one IS daemon-backed, so it works on either payload as
long as the bead id resolves):

```json
   {"field": "id", "op": "in_subtree_of", "value": "{{assign_bead}}"}
```

**Don't loop while the escalation is still open.** The mirror-image hazard is a
gardener that keeps firing because its only open child is *parked* on an
unresolved escalation — the child never closes, so a `loop_end`
`{bead_query, op:"==", value:"0"}` never fires and the gardener reads as
STALE-STOP (ka-0000002198). Add `exclude_blocked_by_open_types` to that
`loop_end` bead_query so a child `blocked_by` an OPEN escalation/decision is
subtracted from the count and the gardener can stop:

```json
{"kind": "bead_query", "op": "==", "value": "0",
 "types": ["task", "bug", "epic"],
 "parent": "{{assign_bead}}",
 "status": ["open", "in_progress", "blocked"],
 "exclude_blocked_by_open_types": ["escalation", "decision"]}
```

Pair it with the `closed`-escalation wake above: the gardener stops while the
escalation is open, and the operator resolving it re-wakes the gardener to pick
the child back up. See §7's `bead_query` row for the full semantics and
fail-safe rules.

### 8.4 Recipe: wake when my crew's decision is answered

A blocking `gb decision` your crew raised resolves and you want to act on
the pick. Use the first-class `decision_resolved` kind. **Important
correction to older recipes:** the decision payload does **not** carry
`raised_by_agent.task_id` — that key is never populated by
`DecisionEventListener`, so a match on it silently never fires. Match on
the keys the payload actually carries (`watch_project`, `watch_role`,
`watch_bead_id`, `chosen_label`, `chosen_index`, `resolved_by`):

```json
{"kind": "decision_resolved",
 "match": {"all": [
   {"field": "watch_project", "op": "==", "value": "gardenio"},
   {"field": "watch_role", "op": "==", "value": "crew"}
 ]}}
```

Branch on the operator's pick by matching the chosen option directly:

```json
   {"field": "chosen_label", "op": "==", "value": "ship-it"}
```

To scope to a decision raised inside your epic's subtree, use the
daemon-backed ancestry leaf on the decision bead id:

```json
   {"field": "watch_bead_id", "op": "in_subtree_of", "value": "{{assign_bead}}"}
```

### 8.5 Recipe: wake when a child of my epic closes

For an epic gardener that should re-plan whenever a child task closes. This
needs the `parent` custom field, which is only on the **SSE full-payload**
shape — so match on `parent` directly (do not use the `watch_*` NATS keys
here):

```json
{"kind": "bead_event",
 "events": ["closed", "updated"],
 "match": {"all": [
   {"field": "parent", "op": "==", "value": "{{assign_bead}}"}
 ]},
 "throttle": "60s"}
```

Prefer this `parent ==` form over `id in_subtree_of` when you only care
about *direct* children — it's a cheap payload compare with no daemon
ancestry walk. Use `in_subtree_of` only when you need the whole subtree
(grandchildren and deeper).

### 8.6 Recipe: hybrid event + cron floor (the default)

The recommended shape for almost every opportunistic gardener: an event
entry for fast response **plus** a cron entry as a slow catch-up floor, so
you still tick if an event is dropped (controller restart, missed SSE
frame, a payload that didn't carry the field you matched on). The cron
entry carries a `preflight` so the floor tick is skipped when there's
genuinely nothing to do:

```json
[
  {"kind": "bead_event",
   "events": ["closed", "updated"],
   "match": {"all": [
     {"field": "parent", "op": "==", "value": "{{assign_bead}}"}
   ]},
   "throttle": "60s"},
  {"kind": "cron",
   "cron": "17 */6 * * *",
   "timezone": "America/Los_Angeles",
   "preflight": {"all": [
     {"type": "bead_query", "op": ">", "value": "0",
      "parent": "{{assign_bead}}",
      "types": ["task", "bug", "epic"],
      "statuses": ["open", "in_progress", "blocked"]}
   ]}}
]
```

Keep the cron floor whenever **any** of these is true (full reasoning in
§5.4): event delivery isn't guaranteed; the match predicate depends on a
field that may not be on the payload; or the gardener verifies a sliding
window of state rather than reacting to one event. If none apply — you only
care about one specific bead-id closing and the loop ends after that — drop
the cron entry and ship the pure event form.

### 8.6b Recipe: wake on a graph-edge change (comment / dependency / label)

Edge-change wakes are **functional as of 2026-06-03** (gap #1 closed —
tasks ka-0000001427 / ka-0000001097 / ka-0000002304). The production SSE
stream now subscribes the edge topics (`cmd/trigger-engine/main.go` builds
`Topics` as `{bead.created/updated/closed} + bridge.EdgeEventTopics()`), and
each affected bead is hydrated via one `GetBead` before `match`
(`hydrateEdgeEvent` → `beadDetailToPayload`). So the hydrated edge payload
carries the **full bead** keys (`type`, `status`, `parent`, custom fields,
`labels`) — *plus* `topic` and `edge_event` — and `match` predicates on them
evaluate normally.

Name the edge event in `events` using the short topic form
(`comment.added`, `dependency.added`, `label.added`, …) — the bare leaf
(`added`) is ambiguous and matches every `*.added` topic. Wake when a new
comment lands on any task under your epic:

```json
{"kind": "bead_event",
 "events": ["comment.added"],
 "match": {"all": [
   {"field": "type", "op": "==", "value": "task"},
   {"field": "id", "op": "in_subtree_of", "value": "{{assign_bead}}"}
 ]},
 "throttle": "60s"}
```

Wake when a `blocks` dependency lands on one of your beads — note
`dependency.added` fires for **both** endpoints of the edge, so scope with
`in_subtree_of` (or a label) to avoid double-waking:

```json
{"kind": "bead_event",
 "events": ["dependency.added"],
 "match": {"all": [
   {"field": "id", "op": "in_subtree_of", "value": "{{assign_bead}}"}
 ]},
 "throttle": "60s", "debounce": "5s"}
```

Hydration **fails open**: a nil daemon or `GetBead` miss skips the event
rather than firing an unmatchable id-only one, so an edge wake never
misfires fleet-wide — but it also means a transient daemon hiccup silently
drops that one edge event. Keep a cron floor (§8.6) if you cannot tolerate a
dropped edge wake. Comment/label storms are guarded by `throttle` +
`debounce` and the `event_type:bead_id` dedup key.

### 8.7 What does NOT work yet

No outstanding wake-engine gaps block the recipes above. One design note
(tracked under epic ka-0000006245, assessment in
`gardener-wake-conditions-assessment.md` at git tag `archive/docs-prune-pre-delete-2026-10-31`):

- **No first-class `pipeline_status` / `mr_event` wake kind — by design; use
  the §8.2 recipe.** "Wake on CI green" works via the §8.2
  `bead_event`-on-`merge-request` recipe, and that recipe is **near-real-time**,
  not sync-bound. `mr_pipeline_status` is stamped onto the MR bead by a GitLab
  pipeline **webhook** (`handlePipelineWebhook` in `gitlab_sync.go`, dispatched
  on `object_kind=pipeline`), and that bead-update rides the SSE full-payload
  path into the wake engine — so the gardener wakes within webhook + SSE delivery
  latency of the pipeline flip, **not** within a poll cadence. (The
  `GitLabPoller` is only a fallback that reconciles recently *merged* MRs; it
  does not drive open-MR pipeline-status freshness.) Keep a §8.6 cron floor only
  as insurance against a *dropped* webhook or SSE frame, not against sync lag.
  The build-vs-document evaluation (task ka-0000007885, report ka-0000003648)
  concluded **document, do not build**: a dedicated wake kind would duplicate
  the webhook→bead→SSE signal for a marginal ergonomic gain.
  Separately, there **is** a first-class GitLab pipeline trigger on the
  **spawn** side (`gb trigger gitlab-pipeline`, label `trigger:gitlab`,
  `event_type=pipeline`; see `triggers.md` §gitlab) — but it *creates* work on a
  pipeline match (it polls the GitLab API and spawns beads/agents), it does not
  *wake* an existing gardener. Reach for it when you want spawn-on-pipeline, not
  wake-on-pipeline.

> **Edge-change wakes** (`comment.added` / `dependency.added` /
> `label.added`) **used to** be listed here as non-functional. They were
> wired up on 2026-06-03 — see §8.6b for the working recipe.

## 9. Second-life lane — reviver / reaper coverage of dormant + closed gardeners

A gardener that stops loop-ing is not necessarily *finished*. Two
independent meta-gardeners sweep the fleet for gardeners that fell dormant
and adjudicate whether each deserves a **second life** — a fresh run
budget rather than permanent retirement:

- **`gardener-reviver`** (ka-0000002321) — curates coverage: enables a
  dormant gardener when its mission area has backlog, disables an enabled
  gardener whose area has drained.
- **`gardener-reaper`** (ka-0000001548) — hygiene + revive-review: disables
  repeatedly-failing gardeners, then reviews the *disabled* and
  *closed-on-budget* backlogs and files an operator decision per candidate.

The three lanes below (reviver §3, reaper §5, reaper §5b) partition the
dormant-gardener space into **disjoint cohorts** — no gardener is seen by
two lanes at once. Section 9.3 proves it; §9.2 is the at-a-glance matrix.
Origin: Matt Baker's "disappointed gardener" ask (Slack thread
1780685362.470639); design epic ka-0000001575.

### 9.1 The three cohorts

A gardener bead carries two orthogonal axes the lanes filter on:

- **`status`** — `open` while the gardener is a live loop the scheduler
  still fires; `closed` once it terminates. `gardenerTerminate`
  (`controller/internal/bridge/gardener_evaluator.go`) CLOSES the bead
  when the run budget is spent, stamping `gardener_terminated_slot=loop_end`
  and `gardener_terminated_kind=max_runs`.
- **`enabled`** — a schedule/trigger field the operator (or reviver/reaper)
  toggles to arm or park a gardener. **`enabled` is independent of
  `status`**: `gardenerTerminate` does NOT touch `enabled`, so a
  budget-closed gardener is typically `status=closed` **and still
  `enabled==true`**.

That independence is the whole reason a third lane was needed. The three
cohorts:

1. **open + disabled** — a live gardener schedule an operator (or reviver)
   parked. Both reviver §3 (enable-if-backlog) and reaper §5
   (revival-review decision) act here. This is the classic "dormant but
   revivable" case.
2. **closed + `max_runs`, goal-unmet** — the "disappointed gardener": ran
   out of run budget WITHOUT its `terminal_goal` reached. It is `closed`
   (so §3/§5's `status=open` queries never see it) **and often
   `enabled==true`** (so an `enabled==false` filter would miss it too) —
   doubly invisible to the older lanes. Reaper §5b owns it exclusively.
3. **closed + goal-met, or closed for any non-`max_runs` reason** — a
   gardener that finished its job, or was hard-stopped / archived. **No
   lane revives it** — a met goal or a deliberate retirement is a
   terminal state, not a candidate for resurrection.

### 9.2 Coverage matrix

| `status` | `enabled` | terminated kind | goal | Cohort | Owning lane(s) | Action |
|---|---|---|---|---|---|---|
| `open` | `true` | — | — | live loop | *(none — scheduler fires it normally)* | runs |
| `open` | `false` | — | — | dormant, parked | **reviver §3** + **reaper §5** | reviver may `enabled=true` if backlog justifies; reaper files a revival-review decision (rate-limited 24h, marker `reaper-revival-review:`) |
| `closed` | `true`/`false` | `max_runs` | **unmet** | "disappointed gardener" | **reaper §5b** | files a second-life-review decision (rate-limited 24h, marker `reaper-second-life-review:`); grant reopens via `gardenerGrantSecondLife` |
| `closed` | any | `max_runs` | **met** | finished its job | *(none)* | terminal — goal reached, no revival |
| `closed` | any | `hard_stop` / archive / other | — | retired | *(none)* | terminal — deliberate retirement |

Notes on the matrix:

- **open + disabled is deliberately double-covered** by reviver §3 and
  reaper §5, but the two never collide: they use *different write
  surfaces* and *mutually-exclusive rate-limit markers*. Reviver §3
  *autonomously flips* `enabled` (a coverage action needing no operator).
  Reaper §5 *files a decision* (never flips `enabled` on the revive side —
  see reaper HARD RULES: "NEVER touch a schedule's `enabled` on the revive
  side — operator decides"). And each skips a gardener that carries the
  *other's* fresh marker (reviver §3 skips a `reaper-revival-review:`
  <24h; reaper §5 rate-limits on `reaper-revival-review:`), so they don't
  re-review the same gardener within the window.
- **`max_runs` goal-met never revives.** §5b's cohort query filters
  goal-unmet only: it excludes any gardener whose `progress_state.runs`
  contains a run with `goal_satisfied == "yes"`. A gardener that declared
  its goal reached is finished, not disappointed.
- **The `max_runs` vs non-`max_runs` split matters.** §5b filters
  server-side on `-f gardener_terminated_kind=max_runs`. A gardener closed
  by a `hard_stop` (wall-clock backstop, operator kill) or formally
  archived (§2 Retirement) is a deliberate terminal state, not a spent
  budget — it is out of scope for second-life.

### 9.3 Why the cohorts are disjoint

The three lanes' selection filters are mutually exclusive by
construction, so no gardener is adjudicated by two lanes in one cycle:

| Lane | `status` filter | `enabled` filter | terminated-kind filter |
|---|---|---|---|
| reviver §3 | `open` | `enabled==false` (disable path) / dormant (enable path) | — |
| reaper §5 | `open` | `enabled==false` | — |
| reaper §5b | `closed` | *(none — axis is irrelevant)* | `gardener_terminated_kind==max_runs` + goal-unmet |

- **§3 / §5 vs §5b are disjoint on `status`.** §3 and §5 both query
  `--status=open`; §5b queries `--status=closed`. A bead is exactly one of
  open/closed, so no gardener is in both an open-lane and §5b at once. This
  is the *doubly-invisible* gap epic ka-0000001575 identified: the
  disappointed cohort is `closed` (invisible to `status=open` lanes) AND
  usually `enabled==true` (invisible to any `enabled==false` filter),
  which is precisely why §5b keys off `status=closed` +
  `gardener_terminated_kind=max_runs` instead of the `enabled` axis.
- **§3 vs §5 overlap on cohort but never on action** (see §9.2 notes):
  different write surface (autonomous `enabled` flip vs operator decision)
  and reciprocal rate-limit-marker skips.
- **Quotas keep the lanes independent even inside reaper.** Reaper may
  file a §5 revival-review AND a §5b second-life-review in the *same*
  cycle (disjoint cohorts, disjoint markers), but never two of either
  (reaper HARD RULES). The markers `reaper-revival-review:` and
  `reaper-second-life-review:` are distinct prefixes, so the two lanes
  never suppress each other's rate-limit.

### 9.4 The second-life grant (engine mechanic)

Reaper §5b never reopens a gardener itself — it files a decision. The
reopen is an operator-authorized engine action,
`gardenerGrantSecondLife` (`controller/internal/bridge/gardener_second_life.go`),
which on the `second-life` grant:

- reopens the SAME bead (`status=open`) so history/lineage stay on one
  bead — RunsSpawned reset, RunBudget bumped (bounded), terminate markers
  cleared, `second_life_count` incremented.

The **don't-resurrect-the-doomed guardrail** (Matt's explicit "do NOT
blind-reset"): a second-life budget `max_second_lives` (default **5** when
unset; a negative value disables second-life entirely) caps resurrections.
When `second_life_count >= max_second_lives` — or the bead carries the
`gardener-second-life-exhausted` label — the grant is DENIED: the gardener
stays closed and the lane must recommend `escalate`, never `second-life`.
Flat / no-progress gardeners (a trailing run of zero `progress_score`) get
`rest` or `escalate`, never an automatic reopen.

The §5b decision offers four options (mirroring §5's shape):

| Option | Meaning |
|---|---|
| `second-life` | Reopen as-is + bump run budget (bounded by `max_second_lives`); mission live and trending toward goal |
| `revise+revive` | Spawn a crew to apply prompt/scope fixes, then reopen |
| `rest` | Keep closed — goal genuinely unreachable / gardener flat-wedged |
| `escalate` | Second-life budget spent (or goal needs redefining) — operator decides |

Cross-references: reaper lane text — `gardenio/plans/gardener-prompts/reaper.md`
§5 / §5b; reviver lane text — `gardenio/plans/gardener-prompts/reviver.md` §3;
engine gate — `controller/internal/bridge/gardener_second_life.go`; design
epic — ka-0000001575.
