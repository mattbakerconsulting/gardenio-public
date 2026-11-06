# Event-Driven Gardeners — Phase 2: SLA Nudger Implementation Notes

> **Task:** gd-Jc3NHOcgDj, implementing the design in
> [`event-gardeners-phase2-escalation-sla-spec.md`](event-gardeners-phase2-escalation-sla-spec.md)
> (gd-RxGnZ8ZUav, closed).
> **Code:** `gardenio/controller/internal/bridge/sla_nudge_{policy,metrics,notify,sweep}.go`.

## Why a sweep, not a system-gardener

The task instructions asked for a sweep (pattern: `scheduler/auto_intake_sweep.go`) unless
that's a genuinely poor fit for a system-gardener approach, with the mismatch documented. A
sweep is the right fit here, not a fallback:

- **The spec itself already frames this as an engine loop.** §1 of the spec catalogs the
  *existing* mechanisms this task extends — `EscalationPingbackLoop` and
  `EscalationStalenessDigestLoop` — both engine-owned sweeps in `internal/bridge/`, not
  gardeners. The gap the spec names (gap 2: "decisions have no automated engine loop at
  all — only escalations get `EscalationPingbackLoop`") is explicitly a request to give
  decisions the *same kind* of engine loop escalations already have, not a request for a
  new gardener.
- **A gardener is a scheduled/triggered LLM invocation with its own cost, latency, and
  drift surface.** The SLA ladder is a deterministic function of (priority, age, quiet
  hours) — `SLAPolicy.RungForAge` — with no judgment call in it. Running that through an
  LLM gardener on a cron would add reasoning-model cost and non-determinism to a decision
  that unit tests can already pin down exactly (this PR's `sla_nudge_policy_test.go`
  table covers every rung boundary for all 4 priorities).
- **The existing gardener-based flat sweep this replaces already showed the failure mode.**
  Spec §1 documents `gb gardener.go:60-95`'s decision sweep as "a gardener's playbook prompt
  tells it to run this each cycle" at a flat 4h threshold — no priority awareness, and it
  only runs if that gardener's cycle happens to fire. The engine sweep fixes both: it's
  always on (opt-in via `SLA_NUDGE_SWEEP`) and priority-aware.
- **Side effects the ladder drives are themselves deterministic field/label writes and
  Slack posts** (reflag, label, comment, `UpdateBeadFieldsTyped`), the same category of
  action `EscalationPingbackLoop` and `EscalationStalenessDigestLoop` already take without
  a gardener in the loop.

No gardener-fallback is used. `SLANudgeSweep` (`sla_nudge_sweep.go`) is a `Run(ctx)` loop
started from `cmd/slack-bridge/main.go`, ticking every `SLA_NUDGE_SWEEP` interval (default
5m — tight enough for P0's ~15m rung-1 fire).

## What's implemented

- **Ladder timing** (`sla_nudge_policy.go`): `SLAPolicy.RungForAge(priority, age)`, pure
  and clock-free. Defaults match spec §2: P0=1h/P1=4h/P2=24h/P3=72h SLA, rung1 at 25%,
  rung2 at 60%, rung3 (overdue) at 100%. Exhaustive boundary-table unit tests
  (`sla_nudge_policy_test.go`) prove every rung transition with a fake `now`.
- **Snooze** (spec §6): zero new code. The sweep only lists `Statuses: ["open",
  "in_progress"]`, which structurally excludes `status=deferred`
  (`TestSLASweep_ListsOnlyOpenAndInProgress` asserts the query shape).
- **Dedupe** (spec §5): per-bead-per-rung, reusing existing stamp-and-compare-age fields
  wherever one already existed for that purpose, rather than adding a parallel
  "last rung fired" tracking field:
  - Rung 1 dedupes on `reflagged_at` (both types) — the same field `gb reflag` stamps.
  - Rung 2 (escalation) dedupes on `queue_alert_requested_at` — **deliberately not**
    `pingback_requested_at` (rung 1's escalation field): reusing rung 1's field for rung
    2's dedupe would let a fresh rung-1 fire mask a due rung-2 fire, since rung 1's
    younger timestamp reads as "not stale enough yet" under rung 2's longer threshold.
  - Rung 2 (decision) dedupes on new `decision_pingback_requested_at`.
  - Rung 3 is one-shot: dedupes on the new `sla_breached` boolean never re-firing once
    true.
- **Quiet hours** (spec §4): `SLAPolicy.IsQuietHours` reuses the existing
  `conditions.EvalTimeWindowLeafAt` primitive that release-freeze windows already use,
  forcing `Op: "inside"`. Default window: America/Los_Angeles 21:00-08:00
  (`DefaultSLAQuietHours`). A due nudge found during quiet hours is skipped with **no**
  dedupe field stamped, so it fires on the first sweep after the window ends rather than
  being dropped. Unlike the underlying leaf (fail-closed on a bad window), this wrapper
  fails **open** (logs, treats as not-quiet) — a timezone/window misconfiguration must
  never be able to silently disable the whole nudger.
- **Metrics**: `sla_nudges_sent_total{bead_type,rung}` (counter, the task's `nudges_sent`)
  and `sla_overdue_count{bead_type}` (gauge, recomputed every sweep from the standing
  rung-3 population, the task's `overdue_count`) — `sla_nudge_metrics.go`.
- **Delivery**, reusing existing Slack paths wherever one already covers the rung, so no
  new Slack code was needed for 3 of 5 (type × early-rung) combinations:
  - Rung 1, both types: stamping `pingback_requested_at` (escalation) / adding the
    `escalated` label (decision) is already watched by the existing SSE handlers
    (`Escalations.maybeNotifyPingback`, `decisions.go handleUpdated`) that repost to
    Slack — no new post call.
  - Rung 2, escalation: stamping `queue_alert_requested_at` is already watched by
    `Escalations.maybeNotifyQueueAlert` → `NotifyEscalationQueueAlert`.
  - Rung 2 (decision) and rung 3 (both types): no existing path, so `sla_nudge_notify.go`
    adds `SLANudgeNotifier`/`Bot.NotifySLANudge`, mirroring
    `NotifyEscalationQueueAlert`'s Block Kit construction.

## Scope deliberately left out (documented gaps, not oversights)

Matching the spec's own §9 "gaps this design leaves for implementation" framing:

- **No `type:escalation_sla_policy` bead type / no per-bead `sla_override`** (spec §7).
  `SLAPolicy` is a Go struct with the spec's default table hardcoded, mirroring the
  precedent `EscalationStalenessDigestConfig` already set (its P0/P1 thresholds are also
  Go constants, not policy beads). Making SLA operator-editable without a code change is
  real future work, but the task's acceptance criteria (ladder timing, dedupe, quiet
  hours, metrics) don't require it, and a policy-bead type is a materially bigger schema
  and caching surface than this change's footprint justifies today.
- **No fleet-wide `max_nudges_per_hour` cap** (spec §5, flagged unresolved in spec §9 gap
  4). Left for a follow-up once real nudge volume is observed.
- **No digest-loop generalization for rung 3** (spec §3/§9 gap 3: sketches folding rung 3
  into a generalized `EscalationStalenessDigestLoop`-style batched digest across all
  priorities and both types). This change instead does a simple one-shot direct notify +
  `sla_breached` stamp + `overdue` label per bead. Simpler, ships now; a batched digest
  can replace the direct-notify path later without touching the ladder/dedupe logic.

## Schema additions

Minimal, additive-only (`kbeads/internal/server/config.go`):

- `type:decision`: `sla_breached` (boolean), `decision_pingback_requested_at` (string),
  `decision_pingback_count` (string), `decision_pingback_source` (string).
- `type:escalation`: `sla_breached` (boolean).

No new bead type. Everything else reuses fields the schema already declared
(`reflagged_at`, `pingback_requested_at`, `queue_alert_requested_at`, etc).

## Wiring

`cmd/slack-bridge/main.go`: opt-in via `SLA_NUDGE_SWEEP` (`"on"`/`"true"` for the 5m
package default, or a duration like `"2m"`; unset/`"off"` disables — same convention as
`ESCALATION_STALE_DIGEST`). Added `_ "time/tzdata"` to this binary's imports (previously
only `cmd/controller/main.go` had it), since the default quiet-hours window loads
`America/Los_Angeles` via `time.LoadLocation`, which needs embedded tzdata in a minimal
container image.
