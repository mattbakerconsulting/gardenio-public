# Design: Terminal goals on agent beads

**Status:** Draft (awaiting operator green-light)
**Tracking bead:** ka-0000007594
**Origin:** Matt Baker, Slack thread 1778118981

## Problem

Agents call `gb stop` whenever they have done "enough for this turn." Today,
the wrap-up captures *what was done* (accomplishments, blockers, handoff
notes) but does not bind the stop event to a measurable success criterion.
A scheduled gardener or thread agent can stop after one cycle with no real
movement on its terminal objective and the operator only finds out by reading
the wrap-up later — by which time the pod is gone and a fresh spawn is the
only path forward.

Matt's framing in the originating thread:

> we should make it so all agent beads can have a terminal goal, and if
> they do they should be required to report on it and answer yes no whether
> it is satisfied, and if no we should respawn the agent after a timer
> (optionally specify able by the agent in the call to stop, or immediate)

## Goals

1. **Optional `terminal_goal` on every agent bead.** Set at spawn time or
   later via `kd update`. When present, the goal is surfaced to the agent
   on every prime cycle and at stop time.
2. **Required y/n self-report at stop.** If `terminal_goal` is set, `gb stop`
   refuses to despawn until the wrap-up carries
   `goal_satisfied: yes|no` plus a one-line `goal_report` justification.
3. **Respawn on unsatisfied.** When `goal_satisfied=no`, the controller
   schedules a fresh spawn of the same agent identity after a timer. The
   agent picks the timer (`--respawn-after 10m`, or `--respawn-after immediate`).
4. **Bounded loop.** A respawn budget (`max_respawns`, default 5) prevents
   infinite loops when the goal is unreachable; budget exhaustion escalates
   to a decision bead for the operator.

## Non-goals

- Replacing the existing `progress_score` / `progress_summary` flow used by
  scheduled gardeners. Terminal goals are orthogonal — `progress_score`
  measures cycle-level material progress; `terminal_goal` measures
  end-state satisfaction.
- Generating goals automatically from the spawning context. The caller
  (operator, parent agent, trigger) is responsible for setting the goal.
- Adjudicating whether the agent's y/n answer is *correct*. The agent
  self-reports; the operator audits via beads-viewer.

## Proposed schema

### Agent bead fields

| Field | Type | Description |
|---|---|---|
| `terminal_goal` | string | Plain-text goal statement. e.g. "Open MR for ka-XYZ that closes the bug and gets CI green." |
| `terminal_goal_set_at` | RFC3339 | When the goal was attached to the bead. |
| `respawn_count` | int (string-coded) | Number of respawns triggered by unsatisfied goals on this identity. |
| `max_respawns` | int (string-coded) | Override default cap (5). |
| `next_respawn_at` | RFC3339 | When the controller should next spawn this identity. Cleared by the entrypoint after the spawn fires. |

### `WrapUp` schema additions (`gardenio/controller/cmd/gb/wrapup.go`)

```go
// GoalSatisfied is the agent's y/n self-assessment of whether
// agent-bead.terminal_goal was met this run. Required when the agent bead
// has terminal_goal set. Values: "yes", "no". (Empty = not answered.)
GoalSatisfied string `json:"goal_satisfied,omitempty"`

// GoalReport is a one-to-three-line free-text justification of
// GoalSatisfied. Required when GoalSatisfied is set.
GoalReport string `json:"goal_report,omitempty"`

// RespawnAfter is the duration the agent requests before being respawned
// when GoalSatisfied=="no". Parsed as a Go duration ("10m", "1h"); the
// literal string "immediate" is shorthand for 0. Ignored when goal is
// satisfied. Optional; defaults to controller policy.
RespawnAfter string `json:"respawn_after,omitempty"`
```

## Proposed flow

### Spawn

`gb start` and `gb spawn` learn an optional `--goal "<text>"` flag. The
controller records `terminal_goal` and `terminal_goal_set_at` on the agent
bead at creation. Operators can also set the goal post-hoc with
`kd update <agent-id> --field terminal_goal="..."`.

### Prime injection (`gb prime`)

When the agent bead has `terminal_goal` set, `gb prime` emits a banner
near the top of the workflow context:

```
## Terminal Goal

This agent bead carries a terminal goal:

> {terminal_goal}

At `gb stop` time you MUST include `goal_satisfied: yes|no` and a brief
`goal_report` in the wrapup JSON. If `no`, optionally specify
`respawn_after: "10m"` (or "immediate") to schedule the next attempt.
Respawn budget: {respawn_count}/{max_respawns}.
```

### `gb stop` validation

`stop.go::runStop` adds a gate after the existing wrap-up validation:

1. Fetch the agent bead. If `terminal_goal` is empty → no-op (today's
   behavior).
2. If `terminal_goal` is set and `wrapup.goal_satisfied` is empty →
   error, instruct the agent to re-run with the field set. Honor `--force`
   exactly like the other stop gates.
3. If `goal_satisfied=="no"`:
   - Parse `wrapup.respawn_after` (default = controller policy, e.g. 30m).
   - Check `respawn_count + 1 <= max_respawns`. If exceeded, escalate by
     creating a decision bead with options
     `[adjust_goal, retry_with_more_budget, abandon]` and skip respawn.
   - Otherwise, write `next_respawn_at = now + respawn_after` and
     `respawn_count++` on the bead **before** closing it. The reconciler
     observes `next_respawn_at` on the closed bead and spawns a fresh pod
     with the same agent identity (same name, same project, same role).

The existing decision-gate / pushed-commit / wrap-up-required gates still
apply unchanged; the goal gate runs alongside them.

### Controller respawn

The simplest implementation reuses the existing scheduled-gardener plumbing
in `internal/scheduler/`:

- The reconciler scans closed agent beads with non-empty `next_respawn_at`.
- When `now >= next_respawn_at`, it spawns a fresh agent pod using the
  same bead schema as the original (name, role, project, parent task,
  goal preserved). The new bead's `respawn_count` is carried over;
  `max_respawns` and `terminal_goal` are inherited.
- The newly-spawned agent reads its predecessor's wrap-up from the closed
  bead so it has context on what was tried.

Alternative: register a one-shot `trigger:schedule` bead at stop time that
fires once at `next_respawn_at` and re-runs the spawn. Cleaner separation
from the reconciler but adds a new entity per stop. Recommend the
reconciler approach for v1; revisit if it gets noisy.

### Decision-bead escalation

When `respawn_count` reaches `max_respawns`, the agent's stop creates a
decision bead instead of scheduling a respawn:

```
prompt: "Agent {name} has exhausted {max_respawns} respawns without
satisfying its goal: {terminal_goal}. Last report: {goal_report}."
options:
- [adjust_goal]   Edit terminal_goal and reset respawn_count
- [retry_budget]  Add N more respawns and continue
- [abandon]       Drop the goal; future stops behave normally
```

This keeps the respawn loop bounded and gets the operator's eyes on
genuinely-stuck work.

## Implementation phases

| Phase | Scope | Owner | Status |
|---|---|---|---|
| 0 | Design review (this doc), operator green-light | thread agent | in flight |
| 1 | Schema: add `terminal_goal*` fields to agent bead, `WrapUp` JSON additions, marshalling | crew | not started |
| 2 | `gb stop` gate + prime injection | crew | not started |
| 3 | Reconciler respawn loop + `max_respawns` decision escalation | crew | not started |
| 4 | `gb start --goal` / `gb spawn --goal` plumbing | crew | not started |
| 5 | beads-viewer surfacing (terminal goal pill on agent rows, goal-unsatisfied filter) | crew | not started |
| 6 | E2E test: spawn with goal, agent reports `no`, observe respawn | crew | not started |

## Open questions for the operator

1. **Default `max_respawns`?** Proposal: 5. Cheap to bump per-bead via field
   override.
2. **Default `respawn_after` when the agent doesn't specify?** Proposal:
   30m. Long enough that an immediate-fail loop doesn't burn pod-start
   thrash, short enough to feel responsive.
3. **Should `goal_satisfied=="yes"` close the parent task automatically?**
   Lean: no. The agent already controls task closure with `kd close`;
   conflating goal satisfaction with task closure removes a useful
   manual checkpoint.
4. **Respawn identity reuse vs. fresh name?** Proposal: same name, same
   bead lineage (parent edge), so beads-viewer shows the chain. Today's
   `respawnThreadAgent` already does this for thread agents.

## Risk / failure modes

- **Hot loop on a misconfigured goal.** Mitigated by `max_respawns` and
  the decision escalation.
- **Respawn races a delayed Slack-bridge restart.** The bridge's existing
  `respawnThreadAgent` already uses an atomic check-and-set to dedupe;
  the reconciler-driven path needs the same idempotency key (e.g.
  `respawn:{agent_name}:{respawn_count}`).
- **Operator confusion when goal mode and progress-score mode both fire
  on the same scheduled gardener.** Document the distinction in
  `gardener-loop-runbook.md`: progress = per-cycle, goal = end-state.
