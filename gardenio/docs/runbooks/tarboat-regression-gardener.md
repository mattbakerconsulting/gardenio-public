# Loop Runbook: Tarboat Test-in-Box Regression Gardener

| | |
|---|---|
| Trigger bead | ka-0000001347 (`trigger:schedule`) |
| Anchor epic | ka-0000003645 (Tarboat test-in-box regression suite; **long-lived**) |
| Cadence | hourly (`19 * * * *`), cooldown 5m, max_concurrent 1 |
| Role | gardener |
| Loop bounds | 180 runs max, loop wall-clock 168h (re-upped by operator), single run 30m |

## 1. Condition to keep true

Every released tarboat (gardenio-in-a-box tarball) passes its validation gates
against a fresh ephemeral box, and every open implementation child of
ka-0000003645 is moving: owned by a live agent, or queued with no silent
orphans. A regression in any gate exists as a typed bug under the epic within
one cycle of being observable — never only in a human's memory or a CI log.

## 2. Drift signal

- A gate in `scripts/inbox/tarboat-validate.sh` fails against the current
  calver that previously passed (a regression).
- An open child of ka-0000003645 has no live `in_progress` agent (orphaned
  implementation work).
- A crew decision sits `awaiting=gardener`, or a human decision sits
  `awaiting=human` for >4h without a reflag (stalled approvals).
- Loop-level wedge: the trigger's `runs_spawned` advances but wrap-ups show no
  progress and open children do not shrink — the gardener is spinning.

## 3. Restoring evidence

- Regression sweep: a validation run against a **fresh ephemeral box** whose
  gates all pass, or — for each failing gate — a typed bug parented to
  ka-0000003645 recording gate name + box calver + failing assertion (the E2
  mechanism). A regression is "handled" when its bug exists and crew is
  dispatched, not when it is merely noticed.
- Orphan handling: each formerly-orphaned child shows a live agent
  (`gb spawn --task <id> --role crew` succeeded) or a re-opened status.
- Do not verify via pod logs — agents cannot read them (advice ka-0000006777);
  verify by bead state, MR state, and validation-script exit codes.

## 4. Autonomous-vs-approval boundary

Autonomous:
- Spawn crew on open children and on regression bugs; walk crew MRs toward
  green (gardenio MRs are self-merge, advice ka-0000002230).
- Answer crew decisions `awaiting=gardener` when one option clearly dominates
  and `artifact_type` is not in {epic, plan, bug}.
- Reflag stale human decisions (>4h, `gb decision reflag`).
- File typed regression bugs under the epic.

Requires a human (`gb decision` / `gb escalation` + `gb yield`):
- Anything gated on a named human reviewer or privileged infra action.
- Retiring, re-scoping, or closing the epic itself.

Never:
- **Never `gb stop --wrapup` with goal_reached / terminal-goal-met.** The epic
  is LONG-LIVED: zero open children is the normal IDLE state, not completion.
  The loop is bounded by run/wall-clock caps only (advice ka-0000000974);
  the all-children-closed stop condition is deliberately omitted.
- Never disable the own trigger — dormancy is automatic (cron pre-flight +
  bead_event re-wake); only the operator disables a gardener trigger.

## 5. Durable state

- The child frontier of ka-0000003645: implementation tasks and typed
  regression bugs (gate + calver + assertion in the bug body) — the full
  regression history lives here.
- Decision beads raised by crew and their reflag timestamps.
- Cycle beads claimed during a run must be `kd close`d (or `kd unclaim`ed)
  before `gb stop`, else the despawn gate blocks with "claimed work pending".
- The trigger bead carries scheduling state only (`runs_spawned`, cooldowns).
  A fresh gardener reconstructs everything from `kd show ka-0000003645` + its
  children + `gb news`.

## Cycle playbook

1. **Load context**: `gb news`; `kd show ka-0000003645`; list its open
   children.
2. **Phase A — drive implementation children**: for each open child without a
   live `in_progress` agent owner, `gb spawn --task <id> --role crew`. Walk
   their MRs toward green. Drain crew decisions (`awaiting=gardener` → assess +
   `gb decision report` if one option dominates and `artifact_type` not in
   {epic, plan, bug}).
3. **Phase B — standing regression sweep** (once provisioning is wired,
   E4/ka-0000002700): assemble the tarball, run
   `scripts/inbox/tarboat-validate.sh` against a fresh ephemeral box, and for
   any regressed gate auto-file a typed bug parented to ka-0000003645
   (gate + box calver + failing assertion). Then drain those regression bugs
   by spawning crew.
4. **Ping stale human decisions**: `awaiting=human` older than 4h with
   empty/old reflag → `gb decision reflag <id> --reason '...'`.
5. **If you OWN an open decision awaiting a human** that you need resolved
   before you can continue: `gb yield` — it blocks until the operator resolves
   it. Reserve yield for exactly that case. Do **not** yield merely because
   crew is in-flight: that has nothing to wait on and will error. Let the
   scheduler respawn you on the next trigger instead — dispatched crew are
   re-adopted automatically.
6. **Otherwise (no open work)**: first `kd close` (or `kd unclaim`) any bead
   claimed this cycle, then `gb stop --wrapup` (see below). This ends only
   THIS RUN; the loop stays alive — the cron pre-flight goes dormant on zero
   open children and the bead_event trigger re-wakes the gardener the moment a
   regression bug or new child lands.

## Wrap-up contract

`gb stop --wrapup` every idle or completed cycle, e.g.:

```json
{"tldr":"idle sweep, no regressions","accomplishments":"idle sweep, no regressions","progress_score":1,"progress_summary":"..."}
```

`tldr` is validator-required — the wrap-up is rejected without it, so always
include it (one-line outcome, ≤500 chars).

`progress_score` ≥1 for any cycle that swept cleanly or moved work;
0 only when the cycle could not perform its sweep at all.

## Provenance

Migrated from the inline prompt of trigger ka-0000001347 on 2026-07-19
(task ka-0000007116, epic ka-0000007152, W3 gardener-runbooks workstream).
The trigger prompt is now a pointer to this file; scheduling mechanics stay on
the trigger bead.
