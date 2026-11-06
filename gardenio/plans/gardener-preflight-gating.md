# Gardener Preflight Gating — gate spawns on attached open beads to the epic

**Tracking bead:** ka-0000006196
**Slack origin:** Matt Baker — "can we make some of our gardeners gated on having
attached open beads to their epic? do we like, support that in config today?
make a plan for where we should do it if we can. gotta tesst it too."

## TL;DR

- **Half-supported today.** The `triggers[].preflight` field exists in the
  schema (`gardenio/controller/internal/scheduler/triggers.go:44`) and is
  documented in the gardener-loop runbook, but it is **never evaluated** —
  `grep -rn Preflight gardenio/controller/internal/scheduler/` returns only
  the struct definition. Adding it is a small wiring change.
- **A nearby mechanism does work**, with different semantics:
  `stop.loop_end.bead_query` (and its legacy alias `exit_condition`) with
  `parent: "{{assign_bead}}"` *closes the schedule* when the parent's
  open-bead count hits zero. That terminates the gardener — too aggressive
  for "skip this fire" gating.
- **Plan:** implement the missing `Preflight` evaluation on cron + event
  trigger entries, add tests, and dogfood on one production gardener.

## Today's behavior (what config supports)

Two condition slots on `type=schedule` beads can reference the epic's
open-bead count. Both already use the same `bead_query` leaf with
transitive `parent` filtering (proven in
`gardenio/controller/internal/scheduler/condition_test.go:55-100`).

| Slot | Effect when "0 open beads under epic" | Status in code |
|---|---|---|
| `triggers[].preflight` (per-trigger) | Skip *this* fire; loop stays alive | **Field parsed, never evaluated** (gap) |
| `stop.loop_end.any[].bead_query` / legacy `exit_condition` | **Close** the schedule permanently | Wired up + tested (`exit_condition_test.go`) |

Matt's ask matches preflight semantics, not loop_end. We do not want a
gardener to permanently retire just because its epic momentarily has no
open work — children come and go as crews close and operators re-open.

## Where to wire `Preflight` (the implementation)

The struct already lands the JSON. We need to call
`conditions.Evaluate` against it before the trigger reports a fire.

### File-level changes

- **`gardenio/controller/internal/scheduler/triggers.go`**
  - Add a private helper `evaluatePreflight(ctx, sched, entry) (ok bool, err error)`
    that wraps `conditions.Evaluate(ctx, *entry.Preflight, conditions.Env{
    Bead: s.daemon, Ancestor: s.daemon, DefaultProject: sched.Project})`.
    Returns `true` (allow fire) when `Preflight == nil` or when the
    expression evaluates true.
  - Call it from `evaluateCronEntry` *after* the cron-window/last-run
    check but *before* returning `FireDecision{Fire: true}`. On
    `ok == false`, return zero-value `FireDecision{}` (no fire, no
    `recordFire`). On evaluator error, log and **fail open** (consistent
    with how malformed `exit_condition` is handled today —
    `TestReconcile_ExitConditionMalformed_FailsOpen`).
  - Call it from `evaluateEventEntry` after `Match` succeeds and before
    debounce / final fire return. Same fail-open policy.
  - Template-expand string leaves (`{{assign_bead}}`,
    `{{schedule_id}}`, etc.) at evaluation time. The conditions package
    already does this for stop-slot `bead_query`; the helper should plumb
    the same env so authors can write `parent: "{{assign_bead}}"`
    without hand-substituting.

- **`gardenio/controller/internal/scheduler/triggers_test.go`**
  - `TestEvaluateCronEntry_PreflightTrue_Fires` — preflight expression
    evaluates true, normal fire decision returned.
  - `TestEvaluateCronEntry_PreflightFalse_SkipsFire` — preflight evaluates
    false (e.g. zero open children), `Fire == false`, `recordFire` NOT
    called.
  - `TestEvaluateCronEntry_PreflightMalformed_FailsOpen` — malformed
    preflight expression logs a warning and lets the fire through.
  - `TestEvaluateEventEntry_Preflight*` — same three for event triggers.
  - Use the existing `stubConditionClient` pattern from
    `condition_test.go` to mock daemon counts.

- **`gardenio/docs/gardener-loop-runbook.md`**
  - The runbook already documents preflight (§1, "Per-entry knobs").
    No doc edit required *if* we keep the schema as documented; flag if
    we deviate (e.g. if we choose `match` semantics over `preflight`).

### Sketch (not committed, illustrative only)

```go
// evaluatePreflight returns true when the entry has no preflight or the
// preflight evaluates true. Errors fail open: log and allow the fire,
// matching exit_condition's malformed-fails-open contract.
func (s *Scheduler) evaluatePreflight(ctx context.Context, sched *Schedule, entry TriggerEntry, idx int) bool {
    if entry.Preflight == nil {
        return true
    }
    ok, err := conditions.Evaluate(ctx, *entry.Preflight, conditions.Env{
        Bead:           s.daemon,
        Ancestor:       s.daemon,
        DefaultProject: sched.Project,
        // TODO: template vars for {{assign_bead}}, {{schedule_id}}.
    })
    if err != nil {
        s.logger.Warn("trigger preflight eval failed, firing anyway",
            "schedule", sched.ID, "index", idx, "error", err)
        return true
    }
    return ok
}
```

Call site in `evaluateCronEntry` (after the `LastRun.Before(lastFire)` gate):

```go
if !s.evaluatePreflight(ctx, sched, entry, idx) {
    return FireDecision{} // gated, drop this tick silently
}
```

## Proposed config shape (operator-facing)

For an existing gardener whose `assign_bead` is `ka-EPIC`, append a
`triggers:` field. Authors keep the legacy `cron:` field too — the
lowering layer ignores it once `triggers:` is non-empty (per
`LowerLegacyToTriggers` doc comment).

```bash
kd update ka-GARDENER -f triggers='[
  {"kind":"cron","cron":"19,49 * * * *","timezone":"America/Los_Angeles",
   "preflight":{"all":[
     {"type":"bead_query","op":">","value":"0",
      "types":["task","bug"],
      "parent":"{{assign_bead}}",
      "status":["open","in_progress"]}
   ]}}
]'
```

Result: the gardener only fires when its epic subtree has at least one
open or in-progress task/bug. Closed epics auto-quiesce; reopened
children re-arm the gardener at the next cron tick. Schedule stays alive.

## Test plan

### Unit tests (must land with the implementation)

1. `triggers_test.go` — three new cron tests + three new event tests
   (preflight true / false / malformed for each path), wired with the
   existing `stubConditionClient`. Locks in fail-open on evaluator
   error and "no fire, no `recordFire`" on a clean false.

2. `condition_test.go` already proves the `bead_query` leaf with
   `parent` walks the subtree. No new coverage needed there.

### Integration / dogfood test (manual, before announcing)

- Pick **scan-vm Skill Gardener** (`ka-0000001667`, epic
  `ka-0000006453`) as the test target. It is a real cron gardener with a
  short tick (`19,49 * * * *`) and an epic whose open-bead count
  fluctuates as crews finish phase work.
- Steps:
  1. Set `triggers:` JSON with preflight bead_query as in §"Proposed
     config shape".
  2. Close every open child of `ka-0000006453` (or pick a gardener whose
     epic is already drained).
  3. Watch two consecutive cron ticks pass with **no** new
     `Run started → kd-…` comment on the schedule bead.
  4. Re-open one child task; on the next tick, observe a fresh run.
- Failure modes to specifically eyeball: (a) preflight bug stops fires
  even when an open child exists, (b) preflight evaluator error
  silently kills all fires (must fail open).
- Roll forward: leave preflight on for that one gardener for ~24h, then
  audit `runs_completed` vs. expected ticks.

### Regression coverage

- Existing `exit_condition_test.go` cases (close-on-zero) must still
  pass — preflight is independent of stop slots.
- Existing `triggers_test.go` cases (no preflight) must still pass —
  helper is a no-op when `entry.Preflight == nil`.

## Out of scope (file as follow-ups if interesting)

- A `gb gardener edit-preflight <id>` CLI shim. Operators can use
  `kd update -f triggers=…` for now; if every gardener ends up wanting
  this, a sugar command is a follow-up.
- Surfacing "skipped this tick due to preflight" in `gb prime` /
  beads-viewer. The runbook hints at a "Last fire trigger" line; we
  could add a "Last preflight skip" line. Useful but not load-bearing.
- A reverse-direction `cooldown` knob ("don't fire more than once per
  N minutes after the last child closes") — different problem.

## Estimated effort

- Implementation: ~30 lines of Go + ~120 lines of tests.
- Dogfood test on one gardener: 1–2 cron windows wall-clock.
- Total: comfortable in one MR, gated by tests + a manual
  preflight-skip observation on the dogfood gardener.
