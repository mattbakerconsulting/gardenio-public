# Gardener mode flip: timed (cron) ⇄ event-driven (bead events)

**Date:** 2026-07-13
**Author:** Matt + Claude (session e392deb3)
**Motivation:** 14 plan gardeners all share the hardcoded default cron
`7,37 * * * *`, producing a 16-wide simultaneous spawn herd twice an hour
(~58 gardener spawns/hr fleet-wide, most of them watchdog no-ops). The herd
shoves the spawner past `COOP_BURST_LIMIT=3` (visible "queued" agents) and
burns ~$30–100/hr of mostly-idle sessions. Gardeners are currently paused
fleet-wide as a stopgap. The fix: let a gardener fire on *activity in its
subtree* instead of a clock, with a slow staggered fallback cron, and give
operators a UX point to flip between the two modes.

## What already exists (no new engine work)

- `trigger:multi` (`controller/internal/bridge/trigger_source_multi.go`):
  a trigger bead labeled `trigger:multi` with a `triggers[]` JSON array of
  `scheduler.TriggerEntry` — `{kind, cron, timezone, events[], match,
  preflight, throttle, debounce, label}`. First firing entry wins; dedup
  keys prevent double-fires; `SkipRuntimeBootstrap` prevents thundering
  herd on creation.
- Match expressions support ancestor resolution, so "event on any bead
  under plan epic X" is directly expressible.
- The `/gardeners` page already classifies firing mode (`cron` /
  `bead_query` / `event`) and renders cadence + next-fire.
- `/plans` index rows join to the bound plan gardener and render a unified
  state chip (plan-gardeners control-room epic ka-0000004792, step 1).

## Component 1 — `SetGardenerMode` (single server-side implementation)

New package `controller/internal/gardenermode` with one entry point:

```go
// Mode is "timed" or "event".
func Set(ctx, client, triggerBeadID string, mode Mode, opts Opts) (Result, error)
```

Rewrites the gardener's trigger bead atomically (single update call):

**→ event:**
- labels: remove `trigger:schedule`, add `trigger:multi`
- `fields.timed_cron` ← current `fields.cron` (backup for flip-back;
  only if non-empty)
- `fields.cron` ← "" (cleared so the legacy schedule loader and the
  mode classifier stop treating it as clock-fired)
- `fields.triggers` ← JSON array:
  1. `{kind: "bead_event", label: "plan-activity",
      events: [<status transitions>, <decision resolved>,
               <escalation/bug created>],
      match: <ancestor == epic id>, debounce: "10m", throttle: "15m"}`
  2. `{kind: "cron", label: "fallback-watchdog",
      cron: staggeredCron(epicID, 6h), timezone: <existing tz or default>}`
- comment on the bead: `mode flip timed→event by <operator> (was cron X)`

**→ timed:**
- labels: remove `trigger:multi`, add `trigger:schedule`
- `fields.cron` ← `fields.timed_cron` if set, else `staggeredCron(epicID,
  30m)` (NOT the literal 7,37)
- `fields.triggers` ← "" (cleared)
- comment as above

Idempotent: flipping to the current mode is a no-op returning the current
state (no bead write, no comment). Unknown/ambiguous label state (both or
neither trigger label) → error, no write.

### Event set (plan gardeners)

- Bead status transitions on descendants of the epic (child opened,
  in_progress, closed, blocked)
- `decision_resolved` for decisions in the subtree
- New `escalation` or `bug` bead created in the subtree

Exact event-name strings must be taken from the existing `EntryEngine`
bead-event vocabulary (`scheduler` package / SSE event types) — do not
invent new event names. If an event in the list has no existing engine
vocabulary, drop it from v1 rather than adding engine surface.

### `staggeredCron(seed string, period)`

Deterministic: minute = FNV-1a(seed) % 60 (and for 30m period the pair
`M, (M+30)%60`; for 6h the shape `M H/6 * * *` with hour offset also
seeded). Same seed → same cron forever (no churn on re-flip). Unit-tested
for distribution (no two of the current 14 plan epics collide on the same
minute — test with the real epic ids as fixtures).

## Component 2 — default-cron stagger (independent quick win)

`firstNonEmptyCron()` (`cmd/gb/gardener.go`) takes the epic/bead id and
returns `staggeredCron(id, 30m)` instead of the literal `7,37 * * * *`.
Callers that pass an explicit `--cron` are untouched. This defangs the
herd even for gardeners that stay timed, and ships independently of
everything else.

## Component 3 — surfaces

1. **`/gardeners/<id>` Operate bar** (canonical): Timed ⏱ / Event ⚡
   toggle. POST verb on the gardener detail page → `gardenermode.Set`.
   Renders current mode from the classifier; disabled with tooltip when
   the trigger bead is in an ambiguous state.
2. **`/plans` gardener chip popover**: same POST verb, chip renders mode
   glyph (⏱/⚡) next to the existing state.
3. **CLI**: `gb gardener mode <epic-or-trigger-id> [--event|--timed]
   [--fallback-cron "..."] [--json]`. No flag = print current mode.
   Resolves the trigger bead the same way `gb gardener status` does.

Classifier change (`gardeners.go` firing-mode): `trigger:multi` label with
a non-empty event entry in `triggers[]` → `event`, even though a fallback
cron entry exists. (Today a parseable `fields.cron` wins → since Set
clears `fields.cron` in event mode, the existing rule almost works; the
change is only to look inside `triggers[]` for the ⚡-with-fallback case
rather than showing it as bare `event` with no cadence — the cadence slot
shows the fallback cron.)

## Validation & rollout (the bug-fear section)

Layered, each gate must pass before the next:

**L1 — unit (TDD, in the MR):**
- `gardenermode`: golden before/after bead fixtures for both flip
  directions, idempotence, ambiguous-label error, timed_cron restore,
  stagger determinism + collision test on the real 14 epic ids.
- Classifier: table test for all mode shapes (cron / multi+events /
  bead_query / bare event / ambiguous).
- CLI: flag parsing + resolver tests mirroring existing gardener_test.go
  patterns.

**L2 — engine integration (in the MR):**
- Extend `trigger_source_multi_test.go`: a flipped plan-gardener fixture
  (real prompt/blob shape) fires on a subtree bead event, respects
  debounce/throttle, fallback cron fires when no events, and does NOT
  double-fire via the schedule loader (label moved).
- Viewer golden tests for the toggle + chip glyph (existing
  gardener_golden_test.go harness).

**L3 — claudeless e2e (in the MR if the harness reaches triggers,
otherwise a follow-up bead BEFORE prod rollout):** flip a fixture gardener
via the viewer verb, emit a child bead event, assert exactly one spawn.

**L4 — prod canary (manual, this is the real validation):**
1. Deploy release with everything OFF by default (the code is inert until
   a gardener is flipped).
2. Flip **one** low-stakes plan gardener (candidate: the test plan
   ka-0000003778 "creating a simple test") to event mode. Gardeners stay
   paused; unpause is NOT required to validate the rewrite itself —
   verify the bead shape, chip, classifier, `gb gardener mode` output.
3. Unpause the fleet with only that one gardener in event mode. Watch 24h:
   it should wake on subtree activity within ~10m (debounce), never more
   than 4×/h (throttle), and at least 4×/day (fallback). Metrics to
   watch: `gardenio_agent_involuntary_deaths_total` (new metric, already
   shipping), reconcile `deferred` counts, trigger-engine memory.
4. Flip the remaining 13 plan gardeners in one pass (`gb gardener mode`
   loop). Watch one more day. The :07/:37 herd should be gone from the
   spawner logs (`deferred: 0` steady-state).
5. Only then: change the plan-assistant default for NEW plan gardeners to
   event mode (separate one-line MR — kept out of this ship so a revert
   never touches the verb).

**Rollback:** `gb gardener mode <epic> --timed` restores the exact prior
cron from `timed_cron` — the flip is its own rollback. Fleet-level
kill-switch (gardeners-pause) remains available and is orthogonal.

## Out of scope (explicitly)

- Changing what plan gardeners DO on wake (prompt/playbook untouched).
- Migrating non-plan gardeners (they can use the same verb, but no bulk
  flip).
- Event-mode default for new plan gardeners (step 5 above, separate MR).
- Retiring the burst limiter or resizing it.

## Risks

- **Missed events** (SSE outage) → 6h fallback cron bounds staleness.
- **Self-echo loops** (gardener wakes on events its own crew causes) →
  15m throttle bounds worst case at 4 wakes/h, same order as today's
  cadence; watch during canary.
- **Double-fire during flip** (schedule loader + multi loader both claim
  the bead mid-rewrite) → single atomic bead update; loaders read labels
  from the same snapshot. Integration test covers the label-moved case.
- **Ambiguous states from manual edits** → verb refuses to write, chip
  shows warning state.
