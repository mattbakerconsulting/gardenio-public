# Wake-intent reconciler — gardener liveness by *intent*, not just *evaluation*

_Design of record for the `wake_intent` field + the periodic wake-intent
reconciler agent. Slack thread ka-0000007909 (Matt Baker, 2026-07-22).
Prototype landed under task ka-0000006187._

## Contents

- [The problem](#the-problem)
- [The shape of the fix](#the-shape-of-the-fix)
- [What shipped (prototype)](#what-shipped-prototype)
- [The reconciler agent](#the-reconciler-agent)
  - [Cadence + wiring](#cadence--wiring)
  - [Per-pass algorithm](#per-pass-algorithm)
  - [Failure taxonomy](#failure-taxonomy)
  - [Reconciler prompt (draft)](#reconciler-prompt-draft)
- [Why this doesn't regress infinitely](#why-this-doesnt-regress-infinitely)
- [Follow-ups (not in the prototype)](#follow-ups-not-in-the-prototype)

## The problem

Plan-gardener discipline is suboptimal, and the crux is structural: **it is hard
to schedule a trigger that reliably wakes you, because if it fails you never
find out — you don't wake up to notice.**

Today there are two liveness backstops, both *mechanical*:

- **`SweepGardenerLiveness`** (`internal/bridge/gardener_liveness.go`): every
  Evaluate cycle it checks each gardener's `last_checked` is within N× its
  cadence and alerts if one goes dark. It runs *inside the engine*, so it cannot
  share the fate of the gardener it watches.
- **`gardenio_trigger_starved`** gauge (`trigger_starve.go`): catches a trigger
  locked out of firing by cooldown starvation.

Both watch **"was the trigger *evaluated*"** — not **"did it fire *when I meant
it to*."** A gardener whose cron window is wrong, or whose bead-query preflight
gate is subtly always-false, still gets evaluated every cycle →
`last_checked` stays fresh → it looks perfectly alive → it never actually wakes.

The root cause: **intent and mechanism are the same object.** The cron *is* the
intent. So nothing can detect when the mechanism fails to achieve the intent.
And the mirror problem matters too — **lots of unneeded gardener wakeups is also
bad** (wasted spawns, token spend, noise).

## The shape of the fix

Split intent from mechanism, then reconcile them periodically:

1. **`wake_intent` field** — a natural-language (optionally structured)
   statement of *when the trigger should wake*. e.g. `"Weekdays 9am PT, plus
   whenever open P0s > 0."` Never consumed by the fire path.
2. **A periodic reconciler** — a cheap (haiku) agent on its **own** cron
   (watcher ≠ watched). Each pass, per gardener, it reads `wake_intent` + the
   mechanical config + the deterministic observed-vs-expected fire diff, and:
   - edits the trigger to match intent when the drift is fixable, or
   - files a bug bead when the drift is impossible-to-express or habitual.
3. **A deterministic fire-diff helper** the reconciler calls, so the model never
   guesses timestamps — timestamp arithmetic is done in code.

## What shipped (prototype)

| Piece | Where | Notes |
|---|---|---|
| `wake_intent` trigger field | `kbeads/internal/server/config.go` (`type:trigger` schema) + `TriggerDef.WakeIntent` / `ParseTriggerDef` (`internal/bridge/triggers.go`) | Registered on the type so a write doesn't 400 "unknown field"; parsed verbatim (trimmed). |
| `--wake-intent` authoring flag | `cmd/gb/trigger.go` (`addCommonFlags` / `applyCommonFields`) | `gb trigger create <source> … --wake-intent '…'`. |
| `scheduler.ReconcileFires` | `internal/scheduler/reconcile.go` | Deterministic expected-vs-observed fire diff. Pure function, fully unit-tested (`reconcile_test.go`). |
| `gb trigger reconcile <id>` | `cmd/gb/trigger_reconcile.go` | Reconstructs observed fires from bead state, calls `ReconcileFires`, emits the JSON report the agent reads. |
| docs | `docs/triggers.md` (§ Wake-intent reconciliation) + this doc | — |

The reconciler **agent** itself (the haiku trigger + its prompt) is **not** in
the prototype — it is specified below and is a fast follow. Everything the agent
needs to do its job deterministically already exists and is tested.

## What shipped (agent half — fast follow, ka-0000001374)

| Piece | Where | Notes |
|---|---|---|
| Reconciler prompt (source of truth) | `gardenio/plans/gardener-prompts/wake-intent-reconciler.md` + README registration | The per-pass algorithm + failure taxonomy + rate-limit + dedup, using the real tooling. Bead prompt is synced from this file (never edited on the bead directly). |
| `gb trigger reconcile --all` | `cmd/gb/trigger_reconcile.go` (+ `trigger_reconcile_test.go`) | Fleet-survey mode: ONE call surveys every open trigger with a `wake_intent` and emits a single `{surveyed, reconciled, skipped, truncated, reports:[…]}` envelope — the cheap per-pass primitive the haiku agent runs (fetch-once, advice ka-0000002706). Triggers without a `wake_intent` are surveyed but skipped. |
| Reconciler trigger bead | `ka-0000002948` (created **DISABLED**) | `role=wake-intent-reconciler` (a cheap haiku role bead, `ka-0000003397`), own hourly cron `47 * * * *`, `max_concurrent_runs=1`, `perpetual:true`. Carries a bounded gardener blob (run_budget 60 + `hard_stop wall_clock` 720h + `no_progress_for:4` self-block) so `SweepGardenerLiveness` covers its own liveness — watcher ≠ watched. Enabling it is an operator decision. |
| `wake_intent` backfill (started) | live standing gardeners `ka-0000006210` (intake groomer), `ka-0000007515` (rescope) | The two enabled perpetual gardeners got an intent statement derived from their cron + condition gate. The rest of the fleet is backfilled incrementally (see follow-ups). |

The trigger is authored disabled deliberately: per the design, the reconciler
lands **behind an enable flag**, and flipping `enabled=true` is the operator's
call (it begins editing live gardener config on a cadence).

### The deterministic helper: `scheduler.ReconcileFires`

```
ReconcileFires(cronExpr, tz string, observed []time.Time, start, end time.Time,
               tolerance time.Duration) (*FireReconciliation, error)
```

- Enumerates the **unconditional** cron fires in `(start, end]` (in the
  trigger's local zone, normalized to UTC).
- Greedy one-to-one nearest matches each observed fire to an expected one within
  `tolerance` (default 10m — the engine polls ~30s and gates each fire behind
  cooldown + conditions, so a fire lands minutes after its cron minute).
- Reports `expected` (each flagged matched/missed + lateness), `missed`,
  and **`extra`** (observed fires with no cron counterpart — the over-firing
  signal), with counts.
- **Scope boundary (load-bearing):** it deliberately ignores `conditions`,
  `cooldown_minutes`, `blocked_by`, and `enabled`. Those are exactly the levers
  the reconciler reasons about, so a reported *missed* fire is a **candidate**
  miss, not a proven one — it may have been a legitimate gated no-fire. Folding
  the gates in here would collapse intent and mechanism back into one object,
  which is the bug.
- **Explanatory overlays (NOT folded into the diff):** `gb trigger reconcile`
  attaches three additive, fail-soft overlays that keep `ReconcileFires`
  unconditional while still explaining a candidate miss deterministically. All
  three are then **composed in code into a single `drift_candidate_missed`**
  field (see below) so the reconciler agent classifies against ONE number:
  - the **`gate`** block — a point-in-time snapshot of the trigger's pre-flight
    conditions against the live store (`would_fire_now` + per-leaf
    raw-vs-effective counts), so a condition-gated no-fire is not read as drift
    (`cmd/gb/trigger_reconcile_gate.go`; false positives ka-0000000876 /
    ka-0000005279). **Event-payload gates are un-evaluable, not store gates:** a
    trigger whose `conditions` include a field leaf (`{"field":...}`) matches an
    in-process bead/event PAYLOAD (e.g. an event-driven pour trigger firing on a
    step bead's close with `gate_passed==true`), not a store query. There is no
    payload in a point-in-time store snapshot, so `would_fire_now` has no
    meaning; the gate reports `evaluated:false` with `event_payload_gate:true`
    rather than a verdict. The reconciler MUST NOT read that as "gate ready" —
    an event-driven trigger's intent is audited against its event-match config +
    observed fires, never this gate. (Before the fix, the legacy condition parse
    silently dropped the `field` key, collapsing each field leaf to a no-op
    "no constraint" leaf that evaluated TRUE → a phantom `would_fire_now:true`
    that mis-filed the event-only trigger ka-0000002857 as impossible drift;
    fix ka-0000001670.)
  - the **`pause`** overlay — the fleet-wide `triggers-pause` / `scheduler-pause`
    / `gardeners-pause` gate windows that overlapped the audit window and applied
    to THIS trigger's fire path, splitting `missed` into `suppressed_missed`
    (inside a pause — a by-design gated no-fire) vs `residual_missed` (outside
    every pause — the only candidate-drift misses). The reconciler judges drift
    against `residual_missed`, so an operator maintenance pause is not read as
    "habitual gardener-logic drift" (`cmd/gb/trigger_reconcile_pause.go`; false
    positive ka-0000004707, fix ka-0000001687). Unlike `gate` (a point-in-time
    probe) a pause is a historical wall-clock interval, so it is attributed to
    specific past fires — the same shape as the disable-window clamp.
  - the **`lease`** overlay — the `max_concurrent` live-lease split, CHAINED onto
    `pause`. For a spawn-producing trigger the engine enforces `max_concurrent` as
    a LIVE lease (`bridge.liveSpawnCount`): a cron slot whose fire it deferred
    because a prior fire's agent still held the lease is a by-design no-fire, not a
    drop. The overlay reconstructs each spawned agent's lease-held interval from
    its `[created_at, closed_at]` lifetime — capped by the SAME staleness bounds
    the engine releases on (`leaseHolderStale` updated_at + 3h,
    `leaseHolderOverAgeCeiling` created_at + 6h) — and splits the pause-RESIDUAL
    misses into `suppressed_missed` (a slot at an instant with ≥ `max_concurrent`
    live holders) vs `residual_missed` (explained by NEITHER pause NOR lease). The
    staleness cap is load-bearing: a holder
    wedged past its release bound does NOT launder the dark-lane misses after it,
    so the overlay can never hide the very bug the lease-stale mechanism surfaces
    (ka-0000001095 / ka-0000002023). Like `pause` it attributes historical fires
    (`cmd/gb/trigger_reconcile_lease.go`; false positive ka-0000007902 — the `*/30`
    wedge-watch's 2 residual misses were a legitimately long 67–78min run holding
    the single lease across the next slot).
  - the **`time_window`** overlay — the quiet-hours split, CHAINED onto `lease`.
    A trigger gated by a `time_window` condition with `op="outside"` (a quiet-hours
    window, e.g. the release-owner's "no releases weekday 07:00-18:00 PT",
    ka-0000004693) reports every cron slot INSIDE that window as a raw `missed`
    fire — but the engine suppressed each BY DESIGN. Unlike the point-in-time
    `gate` (whose `would_fire_now` is non-deterministic — it zeros the residual
    when sampled during quiet hours and flags every historical quiet-hours miss as
    phantom drift when sampled outside), this overlay evaluates each missed slot at
    its OWN wall clock (`conditions.EvalTimeWindowLeafAt`) and attributes it to the
    window it actually fell in, so the verdict is independent of WHEN the reconciler
    runs. It splits the lease-RESIDUAL misses into `suppressed_missed` (fell inside
    a quiet window) vs `residual_missed` (explained by NEITHER pause NOR lease NOR
    time_window). `residual_missed == 0` while `suppressed_missed > 0` is the
    deterministic "the trigger's own quiet-hours window explains the whole deficit
    → IN SYNC" signal (`cmd/gb/trigger_reconcile_timewindow.go`; false positive
    ka-0000005666, recurrence ka-0000000211 — 21 of 21 raw misses over 168h fell
    strictly inside a Fri 07:00-18:00 PT quiet window).

    The same overlay also carries the FORWARD-looking half of the quiet-hours
    question, which no residual count can answer: `suppressed_now` (a
    `time_window` leaf is declining to fire at the reconcile instant),
    `suppressed_until` (RFC3339 UTC instant the hold lifts) and
    `suppressed_minutes_remaining` (the same as whole minutes). "Is the trigger
    firing as designed?" and "will this lane act on my problem soon?" are
    different questions, and a quiet-hours-gated trigger answers YES to the first
    and NO to the second SIMULTANEOUSLY — so a consumer keying only on
    `residual_missed == 0` reads a lane that is dark for the next ten hours as
    covering. That is exactly what bug ka-0000008105 was: the gardener-bugfix
    Step-3 release-owner liveness probe scored a suppressed owner as live and
    skipped dispatching a P0 crew onto a release-blocker bug for a whole ~11h
    PT-business-hours hold. Apply a horizon to `suppressed_minutes_remaining`
    ("will it fire before I cycle again?") rather than re-deriving cron+window
    arithmetic client-side. An EMPTY `suppressed_until` with
    `suppressed_now == true` means the hold runs past the overlay's 48h forward
    probe horizon — read it as "suppressed indefinitely", the safe direction for
    a coverage probe.
  - the composed **`drift_candidate_missed`** field — the FINAL, single
    deterministic miss verdict, folding all four overlays in code so the agent
    reads ONE number instead of reconciling five blocks by hand
    (`cmd/gb/trigger_reconcile_drift.go`; habitual false positive ka-0000004262,
    siblings ka-0000005279 / ka-0000001961; quiet-hours recurrence ka-0000000211).
    It walks the historical residual
    chain newest-first (`time_window.residual_missed` → `lease.residual_missed` →
    `pause.residual_missed` → `fires.missed_count`, first evaluated wins) and
    applies the point-in-time
    `gate` as the LAST link: when `gate.would_fire_now == false` the pre-flight is
    declining to fire now (an empty lane, or one its excludes floor to 0), so the
    residual is a run of gated no-fires → `drift_candidate_missed == 0` with
    `gate_explains_residual == true`; when `gate.would_fire_now == true` (or the
    gate is absent / un-evaluated) the residual STANDS — a positive value there
    with `would_fire_now == true` is the genuine dark-lane signal the fix must
    never hide (the same condition-suppressed-vs-dark split
    `internal/bridge/trigger_dark_lane.go` makes). The `gate` was the one overlay
    left OUT of the residual chain — it shipped (MR !6488) as a raw signal the
    haiku agent had to combine with `residual_missed` by hand, and it kept
    re-filing a `habitual` bug against gardener-rescope (ka-0000007515) DESPITE
    `subtraction_explains_miss == true`; prompt-only syncs of the classification
    rule already regressed once (ka-0000001961) because they leave the judgment to
    the model. Folding the gate into the residual removes that judgment.

> **Live-prompt sync is load-bearing (ka-0000000211).** The `drift_candidate_missed`
> composition and the `time_window` overlay live in CODE, but the reconciler is a
> haiku agent driven by the `prompt` field on schedule bead `ka-0000002948`, synced
> from `plans/gardener-prompts/wake-intent-reconciler.md`. When that sync lags a code
> change, the agent keeps applying the OLD rule against the NEW report: after the
> `time_window` overlay + `drift_candidate_missed` fold shipped (MR !7355 /
> ka-0000004262), the live prompt still said "judge drift against
> `lease.residual_missed`" — which for a quiet-hours gardener is the pre-time_window
> residual (21), so the agent re-filed `habitual` drift on the release-owner even
> though `drift_candidate_missed == 0`. This is the exact drift class the
> `gardener-prompt-drift-alarm` (README §Enforcement, ka-0000000874) guards against;
> a code change to the report SHAPE or the classification rule MUST be paired with a
> live re-sync of `ka-0000002948`.

`gb trigger reconcile` reconstructs `observed` from durable bead state without
any gardener self-report, de-duplicated to the cron's minute grain, from three
sources in order of fidelity:

1. Children carrying `schedule_fire_time` — the canonical per-fire stamp, but
   present only on beads the trigger creates directly (a `spawn_agent` task).
2. Allowed **audit-ledger** beads' `match_key` as a fallback per-fire record.
   This matters because not every fire produces a `schedule_fire_time`-stamped
   bead: an `apply_formula` fire pours a *molecule* (labeled `project:<p>` only —
   no `trigger:<id>` label, no `schedule_fire_time`) and an AssignBead-pinned
   gardener reuses one long-lived task, so for those triggers the only durable
   `trigger:<id>`-labeled per-fire record is the append-only audit-ledger bead.
   Skipping it collapsed `observed` to `last_run` alone and manufactured a
   phantom miss for every fire but the latest (bug ka-0000007130). A fire that
   already produced a `schedule_fire_time` bead dedups its audit receipt at the
   same cron minute; audit fires from a since-superseded cron are dropped by a
   current-cron check (regressions from ka-0000004209).
3. The trigger's own `last_run` as a floor.

## The reconciler agent

### Cadence + wiring

A `type=trigger` gardener on its **own** schedule (proposed default: hourly, or
`*/30` during business hours), `role: <a cheap haiku role>`, one spawn at a
time. It is **independent** of every gardener it audits — a dark gardener cannot
suppress it, because it is not downstream of any of them.

Its own liveness is covered by `SweepGardenerLiveness` (it is just another
enabled gardener trigger), so there is no new dark-class to worry about.

> **As shipped:** trigger `ka-0000002948`, cron `47 * * * *` (UTC), role
> `wake-intent-reconciler` (haiku), `max_concurrent_runs=1`, created DISABLED.
> Prompt: [`gardenio/plans/gardener-prompts/wake-intent-reconciler.md`](../plans/gardener-prompts/wake-intent-reconciler.md).

### Per-pass algorithm

For each enabled `type=trigger` bead with a non-empty `wake_intent` (skip the
ones without — they fall back to the mechanical sweep alone):

1. Run `gb trigger reconcile <id> --json` to get the deterministic report
   (`wake_intent`, mechanism config, and the `expected/matched/missed/extra`
   fire diff).
2. Judge intent vs. mechanism:
   - Does the cron/tz/window plausibly implement `wake_intent`?
   - Do the observed fires (matched vs. missed vs. extra) look like `wake_intent`
     in practice, accounting for the gate levers the diff ignores?
3. Classify (see taxonomy) and act:
   - **fixable drift** → `gb trigger` edit (fix cron/tz/condition), log the diff
     as a bead comment.
   - **impossible / habitual / chronic over-fire** → file a bug bead
     (`--type bug`), parented to the gardener, labelled `wake-intent-drift`.
4. Never touch the fire path or `last_checked`; never spawn work — this is an
   audit + config-repair loop, not a doer.

To avoid churn and respect "don't manufacture work," the reconciler should be
**rate-limited per trigger** (e.g. one edit or one bug per trigger per 24h) and
**dedup its bug beads** (idempotency by `trigger_id` + drift class), mirroring
how the pouring-stall and liveness sweeps dedup per episode.

### Failure taxonomy

| Class | Signal (from the report + judgment) | Action |
|---|---|---|
| **fixable drift** | intent is expressible with current primitives, mechanism is wrong: bad cron window, wrong tz, an always-false/typo'd condition, missed fires that aren't explained by any gate | edit the trigger so the mechanism matches intent; log the before/after diff |
| **impossible** | intent can't be expressed with the primitives, or is self-contradictory; a malformed cron (surfaced in the report `note`) | file a bug bead — needs a human or a new primitive |
| **habitual** | the same drift recurs across passes, or the trigger chronically over/under-fires despite prior fixes | file a bug bead — systemic signal, not a one-off fix |
| **over-firing** | `extra` fires ≫ 0 with no cron counterpart; observed ≫ intent | tighten `cooldown` / window / condition; if the cause is structural, file a bug |

### Reconciler prompt (draft)

> You are the **wake-intent reconciler**. Your job is to make each gardener
> trigger's *mechanism* (cron/timezone/conditions/cooldown) match its declared
> *intent* (`wake_intent`), and to flag drift you cannot safely fix.
>
> You are given, per trigger, the JSON output of `gb trigger reconcile <id>`:
> its `wake_intent`, its mechanical config, and a deterministic diff of
> `expected` vs `matched` / `missed` / `extra` fires over the window. **Trust
> the timestamps in that report — do not recompute them.** A `missed` fire is a
> *candidate* miss: the diff ignores conditions/cooldown/blocked_by/enabled, so
> a miss may be a legitimate gated no-fire. Reason about whether the gates
> explain it.
>
> For each trigger, decide one of: **in sync** (do nothing), **fixable drift**
> (edit the trigger, then log the diff), **impossible/habitual** (file a bug
> bead parented to the gardener), or **over-firing** (tighten the gate or file a
> bug). Prefer the smallest safe change. Never edit the fire path, never spawn
> work, and never act on a trigger you've already actioned within the last 24h.

## Why this doesn't regress infinitely

- **Engine sweep** = mechanical dead-man's switch. It runs inside the engine,
  iterates the full roster every cycle, and cannot go dark the way a single
  watched gardener can. This is the always-on floor.
- **Wake-intent reconciler** = semantic "fired as intended" layer, on its own
  independent cron, watching everything *including* itself-as-a-gardener via the
  engine sweep.
- **`ReconcileFires`** = deterministic ground truth for the timestamp half, so
  the semantic layer never has to guess.

Two layers, each covering the other's blind spot; the bottom layer is
non-agentic and unconditional, so the stack terminates.

## Follow-ups (still open)

- **Enable the reconciler trigger** (`ka-0000002948`) — shipped DISABLED behind
  the enable flag; an operator flips `enabled=true` when ready for it to begin
  editing live gardener config hourly.
- **Finish the `wake_intent` backfill** across the live gardener roster — the two
  enabled perpetual gardeners are done (`ka-0000006210`, `ka-0000007515`); the
  remaining gardeners (and their disabled predecessors) still need an intent
  statement. Until a trigger declares one, the reconciler correctly skips it and
  it falls back to the mechanical sweep alone.
- Optional structured `wake_intent` (a small JSON schema beside the NL string)
  so the deterministic layer can validate more without model judgment.
- Surface the latest reconcile verdict on `gb trigger show` / beads-viewer, like
  the pouring-stall banner, so drift is visible from a cheap field read.

### Shipped (fast follow ka-0000001374)

- ✅ The reconciler trigger bead + its prompt, behind an enable flag,
  rate-limited (one edit/bug per trigger per 24h) + bug-bead dedup (by
  `trigger_id` + drift class). See [What shipped (agent half)](#what-shipped-agent-half--fast-follow-ka-0000001374).
- ✅ `gb trigger reconcile --all` fleet-survey mode — the one-call per-pass
  primitive the reconciler runs.
- ✅ `wake_intent` backfill started on the perpetual/standing gardeners.
