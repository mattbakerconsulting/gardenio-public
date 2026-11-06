# Event-Driven Gardeners — Phase 2: Escalation + Decision SLA Spec

> **Task:** gd-RxGnZ8ZUav, under epic gd-BYKE0Zi0gh ("Event-driven gardeners: wake on
> work, dormant when done, SLA-nudged escalations, no scheduled runs").
> **Status:** design proposal — **acceptance is coordinator review of this doc before
> Phase 2 implementation starts.** Nothing here is wired up yet; every "existing" field
> or loop cited below is live in `main` today, every "new" field or behavior is a
> proposal for the implementation task(s) this doc unblocks.
> **Scope:** SLA-by-priority, the 3-rung nudge ladder, quiet hours, dedupe, what stops
> nudging, and where the SLA config lives — for **escalation** and **decision** beads,
> the two types a human is ever "awaiting" on.

## Contents

- [1. What exists today (the measured starting point)](#1-what-exists-today-the-measured-starting-point)
- [2. SLA by priority](#2-sla-by-priority)
- [3. The nudge ladder (3 rungs)](#3-the-nudge-ladder-3-rungs)
- [4. Quiet hours](#4-quiet-hours)
- [5. Dedupe](#5-dedupe)
- [6. What stops nudging](#6-what-stops-nudging)
- [7. Where the SLA lives: policy bead vs per-bead fields](#7-where-the-sla-lives-policy-bead-vs-per-bead-fields)
- [8. Field schema](#8-field-schema)
- [9. Gaps this design leaves for implementation](#9-gaps-this-design-leaves-for-implementation)

## 1. What exists today (the measured starting point)

Two independent nudge mechanisms already exist, at different levels of automation, and
they do **not** share a priority-aware SLA concept:

| Mechanism | Type(s) | Automation | Cadence | Priority-aware? |
|---|---|---|---|---|
| `EscalationPingbackLoop` (`gardenio/controller/internal/bridge/escalation_pingback_loop.go`) | escalation only | Engine-owned, always on | Doubling backoff: 12h → 24h → 48h → 96h → 192h → ... (`autoPingbackThreshold`, line 1-90 area), capped at `maxAutoPingbacks = 10` | **No** — flat for every priority |
| `EscalationStalenessDigestLoop` (`gardenio/controller/internal/bridge/escalation_staleness_digest.go`) | escalation only | Engine-owned, **opt-in** via `ESCALATION_STALE_DIGEST` (off by default) | Sweep every `defaultDigestSweepInterval = 20m`; digest-worthy at P0 > 2h, P1 > 8h (`defaultP0StaleThreshold`/`defaultP1StaleThreshold`); re-warms every `defaultDigestReWarmInterval = 6h` | **Yes**, but only P0/P1 — P2+ never digested, and thresholds are hardcoded constants, not policy-driven |
| Gardener manual sweep (`gardenio/controller/cmd/gb/gardener.go:60-95`) | decision only | Manual — a gardener's playbook prompt tells it to run this each cycle | Flat 4h: "For each decision awaiting=human older than 4h whose `reflagged_at` is empty or older than 4h: `gb decision reflag`" | No — one flat threshold, no engine loop at all |
| `gb escalation/decision reflag` (`runReflag`, `gardenio/controller/cmd/gb/decision.go:660-820`) | both | Manual, agent- or operator-invoked | On demand | N/A — a manual action, not a cadence |
| `gb escalation pingback` | escalation only | Manual, agent- or operator-invoked | On demand, rate-limited to 1/hour/bead (`pingbackMinInterval`, `escalation_pingback.go:17`) | N/A |
| `successor` fallback (`gb escalation/decision successor set/resolve`) | both | Manual invocation of `resolve`; re-routes, never resolves | Idle-window param at call time (`--idle`, default 1h in the runbook example) | No |

Three gaps this task exists to close:

1. **No declared SLA-by-priority** — every threshold above is a hardcoded constant
   somewhere in Go, not a value the operator can see or change without a code change.
2. **Decisions have no automated engine loop at all** — only escalations get
   `EscalationPingbackLoop`. A decision today is nudged only if a gardener's manual
   4h-sweep step happens to run.
3. **No rung-3 "overdue label + daily digest" for decisions**, and for escalations the
   existing digest is opt-in, P0/P1-only, and not SLA-driven.

## 2. SLA by priority

Proposed default SLA — the elapsed time an escalation/decision may sit `awaiting=human`
(or open + unapproved, for escalations) before it is considered **overdue** and eligible
for rung-3 treatment (§3):

| Priority | SLA (proposal) | Rung 1 fires at | Rung 2 fires at | Rung 3 (overdue) at |
|---|---|---|---|---|
| P0 | 1h | 25% of SLA (~15m) | 60% of SLA (~36m) | 100% of SLA (1h) |
| P1 | 4h | ~1h | ~2.4h | 4h |
| P2 | 24h | ~6h | ~14h | 24h |
| P3 | 72h | ~18h | ~43h | 72h |

These are the operator's numbers from the task brief, unchanged. The rung-1/rung-2
percentages are a proposed default split (not mandated by the task) — see §7 for how the
operator changes any of these without a code deploy.

This SLA table directly **replaces** the flat pingback backoff (12h/24h/48h/...) and the
hardcoded P0>2h/P1>8h digest thresholds as the source of truth for "is this bead
overdue," for beads that opt into policy-driven SLA (§7). It does not remove those two
loops — it changes what feeds their per-priority threshold (§9 covers the migration
question explicitly, since today's `autoPingbackThreshold` growth curve is exponential
per-nudge, not a single flat "SLA," and reconciling the two shapes is implementation
work, not something this doc can silently resolve).

## 3. The nudge ladder (3 rungs)

Per the task brief, three rungs, mapped onto **existing** mechanisms wherever one
already does the job:

### Rung 1 — reflag: priority bump + repost

**Fully covered by the existing `runReflag` path** (`gardenio/controller/cmd/gb/decision.go:660-820`),
shared by `gb decision reflag` and `gb escalation reflag`. No new mechanism needed —
rung 1 is: call reflag automatically once SLA-elapsed crosses the rung-1 fraction.

What reflag already does (unchanged):
- Bumps priority one step toward P0.
- Stamps `reflag_count`, `reflagged_at`, `reflagged_by`, `reflag_reason` (both types).
- Escalation-only: also stamps `pingback_requested_at`, `pingback_count`,
  `pingback_by`, `pingback_source="reflag"`, `pingback_reason`, and adds the
  `"escalated"` label at P0/P1 to trigger a Slack repost.

What's new: an **engine-owned automatic trigger** that calls this same code path for
decisions too (today only escalations get any engine automation, and even that is the
pingback loop, not reflag) — i.e. extend the SLA-driven sweep to invoke `runReflag`'s
underlying logic on behalf of the system, stamped `reflagged_by="system:sla-nudge"`
(mirroring the existing `responded_by: system:decision-aging` convention from
`BEADS_DECISION_AGING_ENABLED`) rather than a human actor.

### Rung 2 — repeat on the operator channel

**Covered by `gb escalation pingback`'s broadcast-reply mechanism** for escalations
(post to the original Slack card thread) — extended to run automatically at the rung-2
SLA fraction instead of only on manual/backoff cadence. For decisions, there is no
existing "repost to original card" primitive today (decisions don't have a pingback
loop) — this rung needs the **same reflag-driven Slack repost decisions already get at
P0/P1** (the `"escalated"` label path) generalized to fire regardless of priority, once
rung 2 is reached, and explicitly directed to **the operator queue channel** rather than
just the original thread — this is the "repeat on the operator channel" language in the
brief, distinct from rung 1's own-thread repost. Concretely: reuse
`NotifyEscalationQueueAlert` (the same notifier `EscalationPingbackLoop` calls past 48h
today) as the rung-2 delivery mechanism, but gate it on SLA-elapsed rather than the fixed
48h constant, and extend it to decisions (today it's escalation-only).

### Rung 3 — mark overdue label and include in the daily digest

- **"Mark overdue label"** is new: no bead today carries an `sla_overdue` (or similarly
  named) label/field. Proposal: stamp a boolean field `sla_breached: true` plus a label
  `overdue` at the moment SLA-elapsed crosses 100%, so it's queryable via
  `kd list --label-any overdue` (per the AND/OR footgun in `kbeads/CLAUDE.md`, always
  `--label-any` for this kind of "any overdue" gate query) without scanning every open
  escalation/decision's timestamps client-side.
- **"Daily digest"** is the closest-existing-match case: `EscalationStalenessDigestLoop`
  already does a consolidated Slack digest of stale P0/P1 escalations, just (a)
  opt-in/off by default, (b) 20-minute sweep not daily, (c) escalation-only, (d) P0/P1
  only, (e) thresholds hardcoded not SLA-policy-driven. Proposal: generalize this loop
  (rename conceptually to a staleness digest that reads the SLA policy bead, §7) to (a)
  default-on once Phase 2 lands (the current opt-in flag was a deliberate first-release
  caution per its own doc comment, not a permanent design choice), (b) cover every
  priority (P2/P3 included, since they have SLAs too now), (c) cover decisions as well
  as escalations, (d) source thresholds from the SLA policy bead instead of the
  `default*Threshold` Go constants. The 20-minute sweep interval does not need to change
  to satisfy "daily digest" — entries re-warm every `reWarm` window (proposal: leave at
  6h, or make it operator-configurable, see §7) and a bead that's been overdue since
  before the last daily boundary keeps showing up; a literal once-a-day cadence is not
  required by the brief's wording ("include in the daily digest" describes the
  destination artifact, not a mandate that the sweep itself run once/day).

## 4. Quiet hours

**America/Los_Angeles**, per the task brief. No nudge (rung 1, 2, or 3 auto-fire) should
fire during the operator's quiet-hours window; a nudge whose scheduled fire time falls
inside quiet hours is deferred to the window's end, not dropped.

The existing reusable shape for this is the trigger engine's `time_window` condition
leaf (`gardenio/controller/internal/scheduler/conditions/expression.go:112-128`):
`Timezone` (IANA, default UTC), `Window` ("HH:MM-HH:MM" 24h local, wraps past midnight),
`Days` (weekday list, or `"weekdays"`/`"weekends"` shorthand). This is **already used**
for a structurally identical purpose — the trigger engine's cron gating
(`gardenio/controller/cmd/gb/trigger_reconcile_timewindow.go`) and the release
gardener's own separate "quiet hours" (a release-freeze window, weekday 07:00-18:00
America/Los_Angeles per `gardenio/controller/cmd/gb/release.go:85-105` — note this is a
**different concept**, release-freeze, not human-sleep hours; the two should not be
conflated even though they reuse the same shape and even the same default timezone).

**Proposal: reuse the `time_window` leaf's field shape, not its wiring.** The SLA policy
bead (§7) carries a `quiet_hours` field whose value is a JSON object with the same three
keys (`timezone`, `window`, `days`) as the `time_window` leaf — e.g.:

```json
{"timezone": "America/Los_Angeles", "window": "21:00-08:00", "days": []}
```

`days: []` (empty) means every day, matching the leaf's own "empty ⇒ every day"
semantics. This is new wiring — the `time_window` leaf itself is currently consumed only
by the trigger engine's cron preflight, not by any escalation/decision notification
code path — but it reuses the exact field shape rather than inventing a new quiet-hours
representation, so a future unification (e.g. quiet hours expressed as a literal
`time_window` `Expression` gating the SLA sweep the same way it gates a cron trigger)
is a refactor, not a schema migration.

## 5. Dedupe

Two dedupe axes, per the brief: **one nudge per bead per rung**, and a **fleet-wide cap
across all beads**.

### One nudge per bead per rung

Already the pattern `EscalationPingbackLoop.evaluateBead` uses today — "fires at most
one ladder step per sweep" — and `EscalationStalenessDigestLoop.evaluateBead`'s
`last_digested_at` re-warm-window check. Proposal: extend the same per-bead-per-rung
guard using the existing count/timestamp fields:

- Rung 1 already dedupes via `reflag_count`/`reflagged_at` — an automatic rung-1 nudge
  only fires if `reflagged_at` is empty or older than the rung-1 SLA fraction, exactly
  the gardener's existing manual 4h check generalized to be SLA-driven and automatic.
- Rung 2 dedupes via `pingback_count`/`pingback_requested_at` (escalation) — needs the
  equivalent pair added for decisions (none exist today, since decisions never got a
  pingback concept — see field schema §8, `decision_pingback_count` /
  `decision_pingback_requested_at` proposed as new fields, explicitly modeled on the
  existing escalation ones rather than invented from scratch).
- Rung 3 dedupes via the new `sla_overdue`/`sla_breached` stamp (§3) plus the digest
  loop's existing `last_digested_at` re-warm check — a bead that's already overdue
  doesn't re-stamp `sla_breached` on every sweep, only on the transition into breach.

### Fleet-wide cap: never more than N nudges per hour, across all beads

**No existing mechanism does this today** — every current rate limit
(`pingbackMinInterval`) is per-bead, not fleet-wide. This is genuinely new: proposal is
a single counter field on the SLA policy bead itself (§7), e.g.
`nudges_sent_this_hour` + `nudge_hour_window_started_at`, incremented by whichever loop
(pingback, digest, the new decision-rung-1/2 sweep) actually posts to Slack, reset when
`now - nudge_hour_window_started_at >= 1h`. Any rung-eligible nudge that would exceed
`max_nudges_per_hour` (policy field, default proposal: 20) is deferred to the next hour
window rather than dropped — it remains eligible and fires as soon as the window resets
and it is still the oldest unfired nudge, so a burst of simultaneous SLA breaches drains
in FIFO order across hour windows rather than silently losing the excess. This needs a
single shared mutation point (all three loops write through one helper) to avoid a
race where two loops independently read a stale counter and both think they have budget
— implementation detail for the follow-up task, flagged here so it isn't missed.

## 6. What stops nudging

Three conditions, per the brief, mapped to existing fields wherever possible:

| Stop condition | Existing mechanism | New? |
|---|---|---|
| **Resolution** | Bead closes (`status=closed`) via the type's own resolution verb (`gb escalation approve/reject/close`, decision `resolve`). Every sweep loop already filters to `Statuses: ["open", "in_progress"]` (`escalation_staleness_digest.go:215`) — a closed bead simply stops matching the sweep query. No new field. | No |
| **Snooze** | **No real snooze field exists today.** The closest analog is the generic bead primitive `status=deferred` + `defer_until` (`kbeads/cmd/kd/defer.go` — `kd defer <id> --until <RFC3339>`, `kd undefer <id>`). The beads-viewer UI's "snooze" button is a `Deferred: true` stub wired to an unbuilt "oracle chatbox" — not a real backend field (`gardenio/controller/cmd/beads-viewer/up_actions.go`). **Recommendation: reuse `status=deferred`/`defer_until` as the snooze mechanism** rather than inventing an `snoozed_until` field — an SLA sweep simply excludes `status=deferred` from its query (add `"deferred"` to the sweep's exclusion, since today's `Statuses` filter already positively lists `open`/`in_progress` and so implicitly excludes `deferred` — this is a no-op change, the exclusion is already structurally correct). `kd undefer` (clears `defer_until`, sets `status=open`) is the un-snooze path, and per the generic primitive's own semantics the SLA clock should resume from wherever it left off (the `pingbackAnchor` function already anchors on `notified_at`/`CreatedAt`, not `updated_at` — a defer/undefer cycle does not reset those fields, so the anchor is naturally snooze-safe with zero extra logic). | No (reuse) |
| **Reassignment** | Existing `assignee`/`owner_seat`/`owner_gardener` fields (decision schema, `kbeads/internal/server/config.go:731-777`) plus the existing `successor` mechanism (`gb escalation/decision successor set/resolve`) already re-routes an idle item to a fallback owner. Proposal: a sweep should stop nudging the **previous** assignee once `assignee` changes — compare the assignee at nudge-eligibility time against the assignee at the anchor timestamp; if they differ, the SLA clock does **not** reset (the bead may still be overdue against its original creation), but the nudge target (who gets pinged) switches to the new assignee. This is existing-field reuse; the only new behavior is the sweep reading `assignee` instead of assuming a fixed operator target. | No (reuse) |

## 7. Where the SLA lives: policy bead vs per-bead fields

**Recommendation: a policy bead per project, with per-bead override fields.**

### Why not per-bead fields only

Stamping the full SLA table (4 priorities × 3 rung fractions) onto every escalation/
decision bead at create time means: (a) every existing open bead is missing the fields
until backfilled, (b) an operator who wants to change the P1 threshold fleet-wide has to
bulk-edit every open bead instead of one config bead, (c) it duplicates the same 12
numbers onto every bead, most of which will never be read.

### Why not a policy bead only (no override)

Some escalations are legitimately more or less urgent than their priority alone implies
(the existing `reflag`/priority-bump mechanism already accepts this — priority is
mutable specifically because static priority isn't the whole story). A per-bead override
lets one bead opt out of the fleet default without a policy fork.

### Proposal

A `type=data` config bead, one per project, analogous in spirit to the existing
`type:spawn_policy` per-role rule beads (`BEADS_SPAWN_POLICY_ENABLED`,
`kbeads/CLAUDE.md`) and the trigger engine's own config-bead pattern — **note: no
`type:policy` bead type exists in the codebase today** (checked
`kbeads/internal/server/config.go` and the `gb`/`gardenio` command tree — the only
"policy" hit is the unrelated `approval_method` enum value `"policy"` used by the
spawn-policy feature). This SLA policy bead is a genuinely **new** bead type,
`type:escalation_sla_policy` (or reuse a generic `type:policy` umbrella if the
coordinator prefers one config-bead-type family for all future policy beads — a
naming/scope call for the coordinator, not this doc, see §9).

Fields on the policy bead (one per project, e.g. `id=policy-gardenio-sla`):
- `sla_p0`, `sla_p1`, `sla_p2`, `sla_p3` — duration strings (e.g. `"1h"`, `"4h"`).
- `rung1_fraction`, `rung2_fraction` — floats, default 0.25/0.6.
- `quiet_hours` — JSON object, shape per §4.
- `max_nudges_per_hour` — integer, default 20 (§5).
- `digest_rewarm` — duration string, default `"6h"` (generalizes
  `defaultDigestReWarmInterval`).

Per-bead override: any escalation/decision bead may set its own `sla_override` field
(JSON, same shape as the priority-keyed subset of the policy bead it wants to override —
e.g. `{"sla_p0": "30m"}`) — the sweep reads the policy bead first, then applies any
keys present in the bead's own `sla_override`. Absent `sla_override`, the policy bead's
value applies unconditionally. This mirrors the existing `auto_default_after_days`
per-decision-override pattern already in production for `BEADS_DECISION_AGING_ENABLED`
(`kbeads/CLAUDE.md`: "A decision may extend (not shorten) its own window with an
`auto_default_after_days` field") — same shape (policy default + narrow per-bead
extension field), reused rather than invented fresh.

## 8. Field schema

Every field this design touches, marked **existing** (cited to its schema location) or
**new** (proposed, with the existing field it's modeled on).

### Existing fields (reused as-is, no schema change)

| Field | Type | Bead type(s) | Source |
|---|---|---|---|
| `reflag_count` | string | escalation, decision | `kbeads/internal/server/config.go:754` (decision), escalation schema (1500-1630 block) |
| `reflag_reason` | string | escalation, decision | same |
| `reflagged_at` | string | escalation, decision | same |
| `reflagged_by` | string | escalation, decision | same |
| `pingback_requested_at` | string | escalation | escalation schema |
| `pingback_count` | string | escalation | escalation schema |
| `pingback_by` | string | escalation | escalation schema |
| `pingback_source` | string | escalation | escalation schema (`"reflag"` \| `"auto"` \| `"manual"`) |
| `pingback_reason` | string | escalation | escalation schema |
| `auto_pingback_count` | — | escalation | `escalation_pingback_loop.go` |
| `queue_alert_requested_at` | — | escalation | `escalation_pingback_loop.go` |
| `last_digested_at` | string (RFC3339) | escalation | `escalation_staleness_digest.go:270` |
| `assignee` | string | escalation, decision | core `Bead` struct, `kbeads/internal/model/bead.go` |
| `owner_seat`, `owner_gardener` | string | decision | `config.go:745-746` |
| `successor` field family | — | escalation, decision | `gardenio/docs/decision-queue-owner.md` |
| `status=deferred` + `defer_until` | enum + timestamp | any (generic primitive) | `kbeads/cmd/kd/defer.go`, `kbeads/internal/model/bead.go` |
| `default_option` / `auto_default_after_days` | string / string | decision | `kbeads/CLAUDE.md` (`BEADS_DECISION_AGING_ENABLED`) — cited as the precedent pattern for §7's `sla_override` |
| `requesting_task` | string | escalation, decision | `config.go:741` (decision); escalation schema |
| `priority` | int | escalation, decision | core `Bead` struct |

### New fields (proposed — none exist today)

| Field | Type | Bead type(s) | Modeled on |
|---|---|---|---|
| `sla_breached` | boolean | escalation, decision | new — rung-3 marker, §3 |
| label `overdue` | label | escalation, decision | new — queryable rung-3 marker alongside `sla_breached`, follows the existing label-as-queryable-flag pattern (`"escalated"` label already used at P0/P1 reflag) |
| `decision_pingback_requested_at` | string | decision | modeled 1:1 on escalation's `pingback_requested_at` — decisions get their own because reusing the escalation-only field name across types would collide with the escalation schema's `write_policy` scoping |
| `decision_pingback_count` | string | decision | modeled on `pingback_count` |
| `decision_pingback_source` | string | decision | modeled on `pingback_source` |
| `sla_override` | json | escalation, decision | modeled on the existing `auto_default_after_days` per-bead-extends-policy pattern |
| **Policy bead** `type:escalation_sla_policy` (name TBD, see §7/§9) | data bead | new type | modeled on `type:spawn_policy` |
| — `sla_p0`/`sla_p1`/`sla_p2`/`sla_p3` | string (duration) | policy bead | new |
| — `rung1_fraction`/`rung2_fraction` | float | policy bead | new |
| — `quiet_hours` | json | policy bead | field *shape* reused from `conditions.Leaf`'s `Timezone`/`Window`/`Days` (§4); the container field itself is new |
| — `max_nudges_per_hour` | integer | policy bead | new (§5) |
| — `nudges_sent_this_hour` / `nudge_hour_window_started_at` | integer / string | policy bead | new (§5, fleet-wide dedupe counter) |
| — `digest_rewarm` | string (duration) | policy bead | generalizes the Go constant `defaultDigestReWarmInterval` into a policy field |

## 9. Gaps this design leaves for implementation

Flagged explicitly rather than silently resolved, per the "coordinator reviews before
implementation starts" acceptance gate:

1. **`type:escalation_sla_policy` vs a generic `type:policy` umbrella.** This doc
   proposes a narrowly-named type; the coordinator may prefer a shared policy-bead type
   family across future features (spawn policy, SLA policy, others) rather than one type
   per feature. Naming/scope decision, not a technical blocker.
2. **Migrating the existing exponential pingback backoff (12h/24h/48h/96h/...) onto the
   flat SLA-rung-fraction model** is a real behavior change for escalations already
   relying on the current cadence — needs either a migration window (both mechanisms
   coexist keyed by whether the bead has an `sla_override` or the project has a policy
   bead) or a flag day. Not resolved here.
3. **Turning `ESCALATION_STALE_DIGEST` default-on** as part of this work is a
   production-behavior change (currently opt-in) — flagged as a decision point for
   implementation, not assumed.
4. **The fleet-wide `max_nudges_per_hour` shared-counter race** (§5) needs a concurrency
   design (single writer helper, or a DB-level atomic increment) — sketched, not
   designed in full here.
5. **Decision beads gaining an engine-owned sweep loop at all** (today: zero automation,
   only a gardener's manual 4h check) is the single largest net-new piece of engine code
   this design implies — everything else is either reuse or a bounded extension of code
   that already exists for escalations.
