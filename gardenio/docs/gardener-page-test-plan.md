# Gardener-page test plan (plan-of-record)

**Scope:** the operator-facing gardener surfaces served by `beads-viewer`
(`gardenio/controller/cmd/beads-viewer/`) — the `/gardener/{id}` life-thread page
and its lazy sub-fragments, the `/gardeners` management index, the create-gardener
modal, and the plan-embedded gardener controls.

**Purpose:** map every gardener UI surface to the *minimal, high-leverage* test
layer, inventory what today's tests actually cover, and name the gaps so the
sibling children of epic `ka-0000004480` execute against one agreed target
picture instead of piling redundant coverage on the surfaces that are already
safe.

**Task:** `ka-0000007524` · **Epic:** `ka-0000004480` (test research + coverage,
gardener page) · **Feature epic:** `ka-0000001579` (Gardener Page) ·
**CDQR audit:** `ka-0000001553`.

> This doc is descriptive of the code as it stands on `main`
> (`2026.215.8`). Bead ids in code comments are quoted verbatim so a reader can
> trace any invariant back to the change that introduced it.

---

## 1. The four test layers (what each is FOR)

The gardener feature is a Go server-side-rendered htmx app: Go builds a view-model
from beads, Go `html/template` renders it, htmx swaps fragments, and a browser
runs the interactions. That stack gives four distinct test layers, each cheapest
at catching a *different* class of regression:

| Layer | Harness | Catches | Cost | Where it lives |
|---|---|---|---|---|
| **Go unit** | `go test`, table-driven, no HTTP | Business-logic bugs in view-model assembly (ordering, counter reconciliation, goal-state derivation, cursor math) | ~ms, hermetic | `*_test.go` beside the source |
| **Go golden-render** | `go test` + `httptest` + `fakeDaemon`, assert on rendered HTML | Template↔view-model wiring: a field that stops rendering, a CSS-class hook that changes, a strip that should suppress but doesn't | ~ms, hermetic | `*_test.go`, `httptest.ResponseRecorder` |
| **Browser e2e** | Playwright, real viewer boot + seeded daemon | htmx swaps, SSE live rows, navigation, real-viewport layout, operator-gated writes end-to-end | seconds, needs a daemon | `gardenio/tests/beads-viewer-e2e/` |
| **Visual / screenshot** | Playwright capture + Python density (CDQR) | Pixel/layout quality regressions, density, overflow — **advisory, human-reviewed** | seconds, needs seed data | `gardenio/tests/beads-viewer-screenshots/` |

**The leverage rule this plan applies everywhere:** push each assertion to the
*lowest* layer that can prove it.

- Pure logic (does `orderGardenerIterations` number a broken chain correctly?) →
  **Go unit.** Never spend a browser on it.
- "Does the field reach the page, in the right element, and suppress when empty?"
  → **Go golden-render.** This is the gardener suite's workhorse idiom.
- "Does the htmx reveal actually fire and swap, does an operator write round-trip,
  does the page survive a 430px viewport?" → **browser e2e.** Only things that
  need a real browser/daemon belong here.
- "Does it *look* right / stay dense / not overflow?" → **visual**, and today
  that is capture-for-review + a numeric density score, **not** a pass/fail
  pixel gate (see §5).

### 1.1 The golden-render idiom (important — it is NOT byte-diff goldens)

Despite the name `gardener_golden_test.go`, the gardener suite does **not** diff
byte-for-byte `.golden` files. It renders the *real* template through the real
handler and asserts on **HTML substrings**:

```go
// renderGardenerGolden renders GET /gardener/{id} against a fakeDaemon at a pinned
// clock and returns the body, failing on any non-200.
mux := mkServerWithNow(t, f, gardenerPageNow)          // real routes + templates
req := httptest.NewRequest("GET", "/gardener/"+id, nil)
w := httptest.NewRecorder()
mux.ServeHTTP(w, req)
// assert: strings.Contains(body, `<section class="gardener-trajectory"`) etc.
```

- **Harness:** `mkServer` / `mkServerWithNow(t, daemon, now)` (defined in
  `bead_show_test.go`) wire the full mux + templates against a `fakeDaemon`
  (also `bead_show_test.go`, ~50 interface methods; `ListBeadsFiltered`,
  `GetBead`, `BatchGetBeads`, … are the gardener-relevant ones).
- **Clock:** `gardenerPageNow()` pins `2026-06-01 12:00 UTC` so `up 2h` / `fires
  in 1h` strings are deterministic (`gardener_page_test.go`).
- **Bead builders:** `mkGardenerBead` (`gardener_test.go`) and
  `goldenGardenerBead` (`gardener_golden_test.go`) encode a `GardenerSpec` map into
  the trigger bead's `gardener` JSON field.
- **Assertion style:** `wantPresent []string` / `wantAbsent []string` slices of
  substrings via `strings.Contains` / `strings.Count` / `strings.Index`
  (ordering). Deliberately robust to incidental markup churn while still locking
  the class hooks the CSS and the SSE client key off of. No `goldenassert`
  helper exists.
- **Gardener-specific render helpers** (`gardener_golden_test.go`):
  `renderGardenerGolden(t,f,id)` (GET the page, fail on non-200, return body),
  `renderGardenerRunsTail` (walk the `/runs` cursor chain to exhaustion and
  concatenate every page — so tail invariants assert against the *full* lazy
  tail), `goldenGardenerFixture` (wire a gardener + iterations into a fresh fake),
  and `renderedIterationCards(body)` = `strings.Count(body, class="bead-row
  gardener-iter")` (the card counter the counter==cards invariant keys on).
  Some golden cases even assert on the rendered `<style>` block (via
  `sliceAfter`) to lock narrow-viewport CSS rules — see
  `TestGardenerDetailGolden_ConnectedBeadsAndRailNoOverflow` (ka-0000006828).

The **only** true byte-diff golden file in the whole package is
`testdata/plan_no_pours_steps.golden.html` (used by `plan_no_pours_golden_test.go`,
which has an `-update` flag) — it is *not* a gardener test. There is **no**
`-update` golden workflow for gardener pages, and adding one is a listed
non-goal (§6).

---

## 2. Surface map — every gardener UI surface

Routes (from `server.go`):

| Route | Handler | Surface |
|---|---|---|
| `GET /gardener/{id}` | `handleGardenerDetail` | Eager detail page (header + rail + eager life-thread head + lazy shells) |
| `GET /gardener/{id}/runs` | `handleGardenerRunsSection` | Lazy "older runs" tail (cursor-paged) |
| `GET /gardener/{id}/invocations` | `handleGardenerInvocations` | Invocations drawer (tldr/full) — used by `/gardeners` rows |
| `GET /gardener/{id}/beads` | `handleGardenerBeads` | Connected-beads panel (constellation SVG + grouped list) |
| `GET /gardeners` | `handleGardeners` | Management index (rows, sort, spend, owned-epic rollup) |
| `GET /health/tile/gardeners` | `handleHealthTileGardeners` | Health-dashboard gardeners tile |
| `GET /create-gardener` · `POST /create-gardener` | `handleCreateGardenerSheet` / `handleCreateGardener` | Create-gardener modal (form + submit) |
| `POST /gardener/{id}/{enable,disable,pause-toggle,run-now,cancel-run,retire,mode}` | `gardener_ops.go` | Operator lifecycle levers |
| `GET /gardener/{id}/edit` · `POST /gardener/{id}/config` | `gardener_ops.go` | Operator config editor |
| `POST /plans/{id}/gardener/*` | `plans_gardener*.go` | Plan-embedded gardener controls (toggle/wake/bulk/master switch) |

The rendered page decomposes into these **surfaces** (template
`gardener_detail.html` + partials):

- **S1 — Header identity & lifecycle badges** (`gardener_detail.html` h1):
  name/title, `enabled`/`disabled`/`closed` chip, `🏁 GOAL ACHIEVED` /
  `goal not reached` badge, `⚠ STUCK — ESCALATED` badge, health verdict chip,
  `💸` spend chip, bead-title-divergence note.
- **S2 — Header counters** (`gardener_summary.html` `dl.gardener-stats`):
  authoritative `Iterations` (+`N live`, +`N never ran`), secondary
  `Engine count` (lifetime tally vs derived-from-life-thread), Max runs, Run
  budget, Consecutive failures, Resumes N/budget.
- **S3 — Schedule / cadence strip** (`gardener_summary.html`): `fires in …`,
  cron cadence chip, timezone, `not scheduled (disabled/closed)`, `cron error`;
  event-mode fallback-cadence.
- **S4 — Goal + config block** (`gardener_summary.html`): terminal goal, exit/
  on-exit/retry/one-spawn/timeout chips, Stop-spec & Progress-state `<details>`.
- **S5 — Trajectory strip** (`gardener_summary.html` `section.gardener-trajectory`):
  verdict, per-run sparkline bars (ok/flat/fail heights), aggregate outcome
  stats (outcome rate, MRs merged, beads closed, decisions resolved).
- **S6 — Owner crosslink** (`gardener_summary.html` + `gardener_owner.go`):
  three-tier owner resolution → `/u/{slug}` chip.
- **S7 — Life-thread cards** (`gardener_iteration_card.html`): run-number ribbon
  (`by time` / `by fire order` heuristic labels), outcome-first headline, MR
  pills, state/outcome chips, did-not-run shells, live badge.
- **S8 — History-truncated banner** (`gardener_detail.html`): per-key page-cap
  saturation notice.
- **S9 — Older-runs lazy tail** (`gardener_runs_section.html`): htmx
  `intersect once` reveal, cursor pagination, no-empty-tail dead-end guard.
- **S10 — Live SSE thread updates** (`gardener_detail.html` +
  `agents_stream.js`): gardener-scoped `agents/events?schedule_id=` stream, in
  place card swaps + prepend of new iterations.
- **S11 — Connected-beads panel** (`gardener_constellation.html` +
  `gardener_beads.html`): radial constellation SVG + type-grouped list,
  degree pills, dense-edge fade.
- **S12 — Operator control bar** (`gardener_controls.html`): enable/disable,
  timed⇄event mode toggle, run-now, cancel-run, edit-config, retire; the
  read-scoped "operator-only" note.
- **S13 — Config editor** (`gardener_config.html`): inline goal/budget/retry/
  stop-spec edit form.
- **S14 — Create-gardener modal** (`create_gardener.html`): epic-scoped form,
  mentor picker, submit → spawn.
- **S15 — `/gardeners` index** (`gardeners.html`): row projection, default sort,
  spend column, owned-epic % rollup, invocations drawer expand, fixture/mirror
  exclusion.
- **S16 — `/gardeners` invocations drawer** (`gardener_invocations.html`):
  tldr↔accomplishments toggle, run rows link to run bead (not back to gardener).
- **S17 — Health tile** (`health_tile_gardeners.html`): fleet gardeners
  count/health summary.
- **S18 — Plan-embedded gardener controls** (`plans_gardener*.go`): per-row
  pause/wake chip, master switch + banner, bulk select→disable, confirm-over-5.
- **S19 — Fixture / schedule-mirror exclusion** (`gardener_fixture.go` +
  `gardener_schedule_mirror.go`): keep synthetic/foreign beads off every live
  surface; 404 deep links.

---

## 3. Existing coverage inventory

489 `.go` files / 277 `*_test.go` in the package; ~25 gardener-specific test
files. Classified by layer (RENDER = boots the mux + asserts on rendered HTML;
UNIT = pure logic, Server-free):

### 3.1 Go unit (pure logic — no HTTP)

| File | Fns | Business logic under test |
|---|---:|---|
| `gardener_test.go` | 13 | `gardenerViewFromBead` spec decode / resume surfacing / no-spec; `reconcileCounters` engine-secondary & derived-from-life-thread; `gardenerIterations` chain order, broken-chain→time fallback, `schedule_id`/`trigger_id` dual-key filter, union+dedupe, empty, history-truncated flag |
| `gardener_timeline_test.go` | 18 | `orderGardenerIterations` — chain canonical, head inference, stale-head fallback, dangling/partial/cycle → by-time, `schedule_key` fire-order tier, one-broken-edge isolation, in-flight-on-top; `RunLabel`, `parseScheduleInt` |
| `gardener_trajectory_test.go` | 13 | `buildGardenerTrajectory` — making-progress/stalled/stuck/starting-up verdicts, run-log vs progress-state precedence, legacy-counter fallback, malformed-degrades, spark truncation |
| `gardener_live_goalstate_test.go` | 5 | `GardenerGoalState` live re-derive, `gardenerStatusIsClosed`, `gardenerGoalTargetID`, `populateLiveGoalState` (detail path), `enrichGardenerGoalStates` (index batch path) |
| `gardener_runs_cursor_test.go` | 7 | `runsCursor` encode/decode round-trip, junk→no-cursor, first-page-at-eager-boundary, tail-exhausts-once, pruned-anchor resync, legacy-empty-key, no-tail-within-head |
| `gardener_spend_test.go` | 2 | `applyGardenerSpend`, `sortGardenerRowsBySpend` |
| `gardeners_owned_epic_test.go` | 5 | `gardenerOwnedEpicID`, `ownedEpicRollupPctDone`, `enrichGardenerOwnedEpics` rollup/error/no-epics |
| `plans_gardener_state_test.go` | 6 | plan-gardener state derivation |
| `gardener_accuracy_invariants_test.go` | 16 | **property-based invariants** over generated thread shapes: (1) header-counter reconciliation, (2) run-ordering monotonic 1..N permutation + live-head-on-top, (3) every closed run resolves a non-empty outcome via the wrap-up cascade — exercises `orderGardenerIterations`/`reconcileCounters`/`countLiveIterations`/`parseWrapUpSummary`→`derivedRunOutcome` directly, Server-free |

> **`gardener_counters.go` has no `_test.go` of its own.** Its
> `reconcileCounters` / `fmtEngineCount` logic is covered *indirectly* —
> `reconcileCounters` by the two `TestReconcileCounters_*` unit cases in
> `gardener_test.go` plus the accuracy invariants, and `fmtEngineCount`'s
> rendered output (`N spawned / M completed (older runs pruned)`,
> `gardener-stat-secondary`/`-pruned`) by the golden `CounterRows` /
> `EngineCountFramed` cases. That is adequate coverage, not a gap — noted so a
> reader does not mistake the missing file for one.

### 3.2 Go golden-render (boots mux, asserts rendered HTML)

| File | Fns | Surfaces asserted |
|---|---:|---|
| `gardener_golden_test.go` | 12 | **S5** trajectory present/suppressed + spark class hooks; **S3** schedule strip (next-fire/cadence/tz/not-scheduled/cron-error/suppressed); event-mode cadence; **S1** lifecycle chip; **S7** iteration cards outcome-first; **S2** counter rows; counter-consistency / bounded-claim / no-empty-tail / engine-count-framed invariants; **S11** connected-beads + rail no-overflow, connected-beads below life-thread |
| `gardener_page_test.go` | 11 | `handleGardenerDetail` header+thread, header-count==rendered-cards, engine-pruned secondary, legacy spec-less derive, 404, trajectory, no-runs; `handleGardenerRunsSection` older-runs, cursor pages w/o dupes, no-empty-tail dead-end; outcome-first cards |
| `gardener_render_gate_test.go` | 3 | **S12** unauthenticated hides controls; edit-form unauth gets note; **render-gate == route-gate** (the page never advertises a lever the POST would 401) |
| `gardener_didnotrun_test.go` | 6 | **S7/S2** did-not-run shell detection, expired-shell flagging, run-count excludes shells, never-ran-shell golden render |
| `gardener_owner_test.go` | 3 | **S6** `resolveGardenerOwner`, detail owner chip, user-home "their gardeners" section |
| `gardener_owner_nplus1_test.go` | 2 | **S15** owner resolution bounded / scales sublinearly (N+1 guard) |
| `gardener_beads_test.go` | 15 | **S11** transitive-graph walk, dense/grouped/collapsed views, status lanes, degree clamp, mention/dep edges, session-bead drop, life-thread scoping; `sortGardenerRows`; next-fire render |
| `gardener_constellation_test.go` | 12 | **S11** radial layout, nil-when-empty, MR diamond, node cap + open-prioritization, depth-saturation/budget, dense-edge fade, constellation SVG render |
| `gardener_invocations_test.go` | 6 | **S16** tldr-only default, run rows link to run bead, full-shows-accomplishments, empty, wrap-up parse, skips-live-runs |
| `gardener_pause_test.go` | 7 | **S12/S18** pause toggle running↔paused, legacy-schedule, audit comment, closed-rejected, non-gardener-rejected, unauthenticated |
| `gardener_ops_test.go` | 20 | **S12/S13** enable/disable/run-now/cancel-run/retire/mode/config-save handlers + guards (the write surface) |
| `gardener_schedule_mirror_test.go` | 4 | **S19** mirror detection, index-excludes, detail 404, map 404 |
| `gardener_fixture_test.go` | 7 | **S19** fixture detection, env opt-in, index-excludes, detail 404, renders-under-opt-in, real-gardener-still-renders, name-diverges |
| `gardener_crosslinks_test.go` | 3 | agent-row schedule_id, agent-card→gardener link, trigger-row→gardener link |
| `gardeners_test.go` | 34 | **S15** the `/gardeners` index — projection, sorting, filters, columns, exclusions (broad) |
| `create_gardener_test.go` | 15 | **S14** sheet render, missing-epic 400, unauth 401, mentor roster, happy-path spawn, mentor mention+dep, project-derivation, error renders-at-200, epic-has-button / non-epic-hasn't, mentor batching |
| `plans_gardener_test.go` | 23 | **S18** plan-gardener rendering + control fragments |
| `plans_gardener_bind_test.go` | 6 | **S18** plan↔gardener bind |
| `plans_gardener_control_test.go` | 10 | **S18** paused chip, flip running→paused, master switch + banner, bulk select→disable OOB row update, confirm-over-5 gate |

### 3.3 Browser e2e (Playwright — `gardenio/tests/beads-viewer-e2e/`)

Boots the real viewer via Playwright `webServer:` with
`BEADS_VIEWER_DEV_AUTH_BYPASS=1` (auth bypass) and
`BEADS_VIEWER_RENDER_FIXTURE_GARDENERS=1` (so the seeded `[E2E fixture]` gardener
renders instead of 404-ing per the §S19 live-surface exclusion). Fixtures are
**seeded static beads** (`seed/fixtures.mjs`): a `fixture:e2e:gardener` trigger
bead + `gardener_run_1..3` closed iteration agents chained via
`schedule_prior_agent_id`, each with a distinct wrap-up.

| Spec | Gardener surfaces exercised |
|---|---|
| `gardener_detail.spec.js` | **S1** header+verdict, **S2** counters (Iterations=3, Max runs, Run budget, no drift), **S4** budget/exit chips, **S5** trajectory strip + spark class hooks + inline heights + aggregate stats, **S7** every card outcome-first (no `.gardener-iter-noutcome`) + exit chips + chain-ordered ribbon, **430px narrow viewport** no-horizontal-overflow |
| `gardeners_index.spec.js` | **S15/S16** runs-expand button, htmx-loads invocations drawer, run rows link to run bead (regression), tldr↔accomplishments toggle |
| `plan_gardener_control.spec.js` | **S18** paused chip, flip running→paused writes `enabled=false` + re-renders, master switch + banner open/close, bulk bar + row checkboxes, select-all→disable OOB updates, >5 confirm gate |
| `health.spec.js` | **S17** (gardener-relevant health-tile assertions) |

### 3.4 Visual / screenshot (`gardenio/tests/beads-viewer-screenshots/`)

`workers: 1`, `retries: 0`, `fullyParallel: false`, **two viewport projects —
desktop 1280×800 and narrow 480×800** (`colorScheme: light`,
`reducedMotion: reduce`). No `webServer` — it captures an already-running viewer
(`BEADS_VIEWER_URL`), optionally with a `BEADS_VIEWER_BEARER` for prod. The
README states outright it is **for MR before/after review, not CI-gating**.

- `tests/canonical-pages.spec.js` — captures full-page PNG + a `page.content()`
  HTML dump per page/viewport. The gardener-detail page (`/gardener/{id}`, page
  name `gardener-detail`) is captured **only when `SCREENSHOT_GARDENERS_DETAIL` is
  set** to a real gardener id (the gardeners-page CAMP audit formula exports one);
  `/gardeners` index always captures (`gardeners-index`).
- `tools/capture-gardener.mjs` / `capture-gardener-prefixed.mjs` (the prefixed one
  strips a `/beads` base-path and auto-scrolls to fire `intersect once` lazy
  sections so the `/runs` tail + Connected-beads panel resolve in-cluster) /
  `cap-cached.mjs` — standalone capture helpers used by audit formulas.
- `tools/measure-density.py` (shared primitives), `measure-gardener-cdqr.py`,
  `measure-gardeners.py` — **offline Python metrics → `metrics.json`**, pure
  objective counts/ratios (no judgement, no pixel-diff, never gate). The PNG
  signals are `near_white_ratio` (fraction of ≥240 pixels — emptiness) and
  `mean_luma`; the HTML signals are `dom_element_count`, `text_byte_per_dom`
  density, link/field counts. `measure-gardener-cdqr.py` scores the
  **CDQR** rubric (Correctness / Density / Quality-relevance / Operability) of a
  single `/gardener/{id}` — incl. `counter_consistency_ok` (visible Iterations ≥
  rendered cards), template-leak / htmx-unresolved / by-time-ribbon / broken-link
  markers, and `operator_gating_present` (mutation routes return 401/403).
  `measure-gardeners.py` scores the **CAMP** rubric (Coverage / Accuracy /
  Management / Polish) of the `/gardeners` index + detail, reconciling
  `rendered_row_count` against **`kd` ground truth** and `detail_iteration_cards`
  against `spec_runs_spawned`.

**Definitive finding:** there is **no pixel-diff visual-regression gate** for any
gardener surface. No `toHaveScreenshot`, no `toMatchSnapshot`, no committed
`.png` baselines, no `*-snapshots/` dirs exist in either suite (verified by grep
+ `git ls-files` across both directories — the only git-tracked "snapshot"
files are unrelated Go golden/JSON tests). The screenshot suite is
*capture-for-human-review* + numeric density/CDQR/CAMP scoring — advisory, not a
CI pass/fail on pixels. Any "did it change" judgement is a human eyeballing
before/after PNGs or an audit formula diffing `metrics.json` numbers across
iterations.

---

## 4. Surface → recommended layer (the plan of record)

For each surface: the layer(s) that should own it, and the current status.
**Own** = the layer that must fail if the surface breaks. Redundant lower-value
coverage is called out in §5.

| # | Surface | Primary layer (owns it) | Secondary | Current status |
|---|---|---|---|---|
| S1 | Header identity & badges | **Golden-render** (badge class hooks by state) | e2e smoke (1 case) | ✅ golden + e2e |
| S2 | Header counters | **Go unit** (`reconcileCounters`) + **golden-render** (rows present, `count==cards`) | e2e (1 case) | ✅ strong (unit+golden+invariants+e2e) |
| S3 | Schedule / cadence strip | **Golden-render** (`TestGardenerDetailGolden_Schedule`) | Go unit (`computeNextFire`) | ✅ golden (6 cases) |
| S4 | Goal + config chips | **Golden-render** | e2e (1 case) | ✅ golden + e2e |
| S5 | Trajectory strip | **Go unit** (`buildGardenerTrajectory` verdicts) + **golden-render** (strip present/suppressed, spark classes) | e2e (1 case, class hooks + heights) | ✅ strong |
| S6 | Owner crosslink | **Go unit** (`resolveGardenerOwner` tiers) + **golden-render** (chip → `/u/{slug}`) | — | ✅ unit + golden; ⚠ N+1 guard only on index |
| S7 | Life-thread cards | **Go unit** (`orderGardenerIterations`, accuracy invariants) + **golden-render** (outcome-first, chips) | e2e (outcome-first, chain order) | ✅ strong |
| S8 | History-truncated banner | **Golden-render** | Go unit (`gardenerIterations` truncated flag ✅) | ⚠ flag unit-tested; **banner HTML not golden-asserted** (gap G1) |
| S9 | Older-runs lazy tail | **Go unit** (`runsPage`/cursor) + **golden-render** (`handleGardenerRunsSection`) | **e2e** (the htmx `intersect` reveal) | ⚠ handler+cursor covered; **htmx reveal not e2e-tested** (gap G2) |
| S10 | Live SSE thread updates | **e2e** (real stream swap/prepend) | Golden-render (the stream `<span>` anchor renders) | ❌ **no e2e for gardener-scoped SSE** (gap G3) |
| S11 | Connected-beads panel | **Go unit** (graph walk, layout math) + **golden-render** (SVG + list) | e2e (the `intersect` load) | ✅ unit+golden strong; ⚠ htmx load not e2e |
| S12 | Operator control bar | **Golden-render** (render-gate==route-gate) + **Go** handler tests | **e2e** (a real operator write round-trip) | ⚠ handlers+gate covered; **no operator-write e2e on `/gardener/{id}`** (gap G4) |
| S13 | Config editor | **Go** handler (`config-save`) + **golden-render** (edit form) | e2e | ⚠ handler covered; form-render + e2e thin (gap G4) |
| S14 | Create-gardener modal | **Go golden-render/handler** (`create_gardener_test.go`) | **e2e** (open modal → submit → land) | ⚠ 15 handler tests; **no e2e for the modal open/submit** (gap G5) |
| S15 | `/gardeners` index | **Golden-render** (`gardeners_test.go`) + **Go unit** (sort/spend/rollup) | e2e (drawer) | ✅ strong |
| S16 | Invocations drawer | **Golden-render** + **e2e** (toggle swap) | — | ✅ both |
| S17 | Health tile | **Golden-render** | e2e (health.spec) | ✅ |
| S18 | Plan-embedded controls | **Golden-render**/handler + **e2e** (bulk/master swaps) | — | ✅ strong |
| S19 | Fixture/mirror exclusion | **Go unit** (predicate) + **golden-render** (404 / index-excludes) | — | ✅ strong |

**Layer-assignment principles that drove the table**

1. **Ordering, counters, cursor math, goal-state, trajectory verdicts, graph
   walk** are pure functions → **Go unit** owns them. These are the highest-risk
   logic and the cheapest to test; they already have the deepest coverage
   (incl. property-based invariants). *Do not* re-prove them in a browser.
2. **"Field reaches the page in the right element / suppresses when empty / has
   the right class hook"** → **Go golden-render**. This is where a template edit
   silently dropping a counter or renaming `.gardener-spark-fail` gets caught.
   The class hooks matter because CSS *and* the SSE client key off them.
3. **htmx reveals, SSE live rows, operator write round-trips, real-viewport
   layout** genuinely need a browser+daemon → **e2e**. Keep e2e *thin*: one
   representative case per interaction, not a re-test of every counter.
4. **Look/density/overflow** → **visual**, advisory. The one hard overflow
   assertion that must never regress (430px no-horizontal-scroll) is already
   pinned in e2e (`gardener_detail.spec.js`) as a *computed* check
   (`scrollWidth <= innerWidth`), which is the right home for it — a
   deterministic measurement, not a pixel compare.

---

## 5. Gaps, redundancy, and missing layer combinations

### 5.1 Coverage gaps (surfaces under-owned by their primary layer)

- **G1 — History-truncated banner (S8) has no golden-render.** The
  `gardenerIterations` truncated *flag* is unit-tested
  (`TestGardenerIterations_HistoryTruncated` / `_NotTruncatedBelowCap`), but no
  test asserts the `gardener-history-truncated` banner actually renders (or
  suppresses) in `gardener_detail.html`. **Fix:** one golden-render case with a
  `truncated=true` header. *Layer: golden-render. Low effort.*
- **G2 — Older-runs lazy reveal (S9) has no e2e.** `handleGardenerRunsSection`
  and the `runsPage` cursor are well covered in Go, but nothing exercises the
  htmx `intersect once` reveal in a browser (scroll → fragment loads →
  continuation cursor re-requests). The fixture seeds only 3 runs (< the
  `gardenerEagerRuns=25` head), so `HasMoreRuns` is never true in e2e today.
  **Fix:** seed >25 iterations for one fixture gardener and add an e2e that
  scrolls and asserts the appended page + no dupes. *Layer: e2e (+ seed).
  Medium effort.*
- **G3 — Gardener-scoped live SSE (S10) has no e2e.** The
  `agents/events?schedule_id=` stream — in-place card swap + prepend of a new
  iteration — is the page's one genuinely live surface and has zero browser
  coverage. **Fix:** an e2e that opens the page, pushes a bead event, asserts a
  card swaps / a new card prepends. *Layer: e2e. Higher effort (needs an event
  source in the harness).* This is the single highest-value missing combination.
- **G4 — Operator write round-trip on `/gardener/{id}` (S12/S13) has no e2e.**
  The lifecycle handlers (`gardener_ops_test.go`, 20 fns) and the
  render-gate==route-gate invariant (`gardener_render_gate_test.go`) are strong
  at the Go layer, and `plan_gardener_control.spec.js` proves operator writes
  end-to-end *on the plans surface* — but not one e2e clicks Enable/Disable/
  Run-now/Retire on the gardener page itself and asserts the control bar swaps.
  **Fix:** one e2e with an `X-Auth-Request-Email` operator header that flips
  enable→disable and asserts the OOB bar re-render. *Layer: e2e. Medium effort.*
  *Note:* the **negative** gate (a read-scoped viewer sees the muted
  "operator-only" note and no levers) is **correctly Go-only and must stay
  there** — the e2e harness's `BEADS_VIEWER_DEV_AUTH_BYPASS` can only inject an
  *operator* identity via `X-Auth-Request-Email`, with no way to model a
  non-operator OIDC role, so `gardener_render_gate_test.go` is the *only* place
  that check can live. Do not file this as an e2e gap.
- **G5 — Create-gardener modal (S14) has no e2e.** 15 Go handler/render tests
  cover the sheet and submit, but no browser test opens the modal from an epic
  page, fills it, submits, and lands on the new gardener. **Fix:** one e2e.
  *Layer: e2e. Medium effort.*
- **G6 — Config editor form render (S13).** `config-save` is handler-tested; the
  `GET /gardener/{id}/edit` **form markup** itself is only lightly asserted.
  **Fix:** a golden-render case on the edit form fields. *Layer: golden-render.
  Low effort.*

### 5.2 Redundancy / over-coverage to AVOID

- **Header counters (S2)** are proven at *four* layers (unit `reconcileCounters`,
  golden `CounterRows`, property `gardener_accuracy_invariants`, e2e). That is
  appropriate *because* counter-drift was the signature bug
  (`ka-0000002549`/`ka-0000003322`) — but it is the ceiling, **not the pattern to
  copy** for lower-risk surfaces. New surfaces should earn multi-layer coverage,
  not get it by default.
- **Do not add e2e that merely re-asserts a counter value or a chip's text** the
  golden-render already pins. e2e time is for interaction (swap/stream/write/
  viewport), not for re-reading static HTML a hermetic Go test already read.
- **Do not convert the substring golden-render tests into byte-diff `.golden`
  files.** The substring idiom is intentional (robust to incidental markup
  churn, readable diffs); a byte-diff golden would red-mask every benign
  template whitespace change. Keeping one convention across the package is worth
  more than pixel-exact template snapshots.

### 5.3 Missing layer *combinations* (the summary)

| Surface | unit | golden | e2e | visual | Missing high-value cell |
|---|:-:|:-:|:-:|:-:|---|
| S8 truncated banner | ✅ (flag) | ❌ | — | — | **golden** (G1) |
| S9 older-runs tail | ✅ | ✅ | ❌ | — | **e2e reveal** (G2) |
| S10 live SSE | — | ~ | ❌ | — | **e2e stream** (G3) |
| S12/13 operator writes | ✅ | ✅ (gate) | ❌ | — | **e2e write** (G4) |
| S14 create modal | ✅ | ✅ | ❌ | — | **e2e submit** (G5) |
| S13 edit form render | ✅ (save) | ~ | ❌ | — | **golden form** (G6) |

Everything else (S1–S7, S11, S15–S19) has its primary layer owned; the residual
e2e cells there are optional smoke, not gaps.

### 5.4 Recommended execution order (highest leverage first)

1. **G3 — gardener-scoped SSE e2e** (only live surface, zero coverage).
2. **G4 — operator-write e2e on `/gardener/{id}`** (write path, browser-only risk).
3. **G2 — older-runs lazy-reveal e2e** (+ seed >25 runs; unlocks `HasMoreRuns`).
4. **G5 — create-gardener modal e2e**.
5. **G1 + G6 — cheap golden-render cases** (truncated banner, edit form) — bundle
   into one MR; they are minutes of work each.

---

## 5.5 The deterministic suite is a HARD CI gate (not a daily-audit reference)

**Bottom line:** the gardener-page deterministic suite already **blocks every MR
in CI** — the strongest possible enforcement, strictly stronger than the "the
daily CDQR audit confirms the tests are green" wiring that task `ka-0000008179`
originally scoped. That task is satisfied by this stronger gate, not by wiring
anything into the CDQR audit formula (which is intentionally dark — see below).

**What runs, and where the gate lives:**

| Suite | Funcs | RWX task | Gate |
|---|---:|---|---|
| Go golden-render (`gardener_golden_test.go`) | 14 | `test-gardenio` | **required** — no `continue-on-error` |
| Go accuracy-invariants (`gardener_accuracy_invariants_test.go`) | 16 | `test-gardenio` | **required** |
| Go trajectory unit (`gardener_trajectory_test.go`) | 13 | `test-gardenio` | **required** |
| Go page-render (`gardener_page_test.go`) | 12 | `test-gardenio` | **required** |
| Browser e2e (`gardener_detail.spec.js`, `gardeners_index.spec.js`, `plan_gardener_control.spec.js`, …) | — | `test-beads-viewer-e2e` | **required** |

- Both RWX tasks are in the default MR task list (`.rwx/ci.yml` — the
  `on: {merge_request, push}` init blocks) with **no** `continue-on-error`, so a
  red gardener test fails `CI / Test` and **blocks forward progress**. This is the
  hard gate the task asked for.
- `test-gardenio` runs `go test -short ./...`. The 55 Go gardener funcs above carry
  **no `testing.Short()` skip guard**, so they execute on every MR under the exact
  CI invocation (verified 2026-08-21). A regression in counter reconciliation,
  run ordering, the wrap-up→outcome cascade, trajectory verdicts, or any template↔
  view-model wiring reds the build — it does **not** wait for a daily audit to
  re-discover the drift. That "fail CI, not just the daily audit" property is the
  whole point of the accuracy-invariants layer (§3.1).

**Why the gate is *not* wired into the CDQR audit formula.** The Gardener-Page
CDQR audit formula (`ka-0000005307`) and its gardener (`ka-0000006085`) are
deliberately **disabled** (`enabled:false`) per operator Matt Baker's explicit
2026-07-26 directive *"No more gardener page; focus on user page now"*
(decision `ka-0000006574`), reaffirmed when the gardener-page revival decision
`ka-0000007662` was dismissed as dead wood (operator-approved, 2026-08-08). A
disabled formula never pours, so referencing the deterministic tests from its
Correctness step would be inert. The CI gate above is the live, enforcing home
for "the locked-in gardener-page tests must stay green" — no CDQR-formula wiring
is needed or wanted. Should the CDQR lane ever be revived by operator decision,
its Correctness judge step should simply *cite* this CI gate (the tests are
already green by construction on any merged build) rather than duplicate it.

## 6. Non-goals / explicit decisions

- **No pixel-diff visual-regression gate.** The screenshot suite stays
  capture-for-review + numeric density (CDQR). Pixel baselines are brittle across
  fonts/themes and would gate MRs on cosmetic noise; the one overflow guarantee
  that matters is already a *computed* e2e assertion.
- **No byte-diff `.golden` files for gardener pages.** Keep the HTML-substring
  idiom (§1.1).
- **e2e stays thin and interaction-only.** Logic lives in Go; the browser proves
  the things only a browser can.
- **Do not wire the deterministic suite into the CDQR audit formula
  (`ka-0000005307`).** The formula is operator-disabled (§5.5); the hard CI gate
  in `test-gardenio` + `test-beads-viewer-e2e` already enforces the tests on every
  MR, which is strictly stronger than a daily-audit reference. Re-enabling the
  CDQR lane is an operator decision, not a test-wiring task.
- This plan is descriptive of `main` at authoring time; as the sibling children
  land the G-fixes, update the §4/§5.3 status cells rather than forking a new doc.

---

## 7. Quick reference — where each layer lives

```text
gardenio/controller/cmd/beads-viewer/
  gardener*.go, plans_gardener*.go, create_gardener.go     # source
  gardener*_test.go                                      # Go unit + golden-render
  templates/gardener_detail.html                         # the page
  templates/partials/gardener_*.html                     # header/summary/cards/controls/…
  testdata/plan_no_pours_steps.golden.html              # the ONLY byte-diff golden (not gardener)

gardenio/tests/beads-viewer-e2e/                          # browser behavioral (CI gate)
  tests/gardener_detail.spec.js, gardeners_index.spec.js, plan_gardener_control.spec.js
  seed/fixtures.mjs                                      # the seeded gardener + 3 runs

gardenio/tests/beads-viewer-screenshots/                  # visual capture (advisory)
  tests/canonical-pages.spec.js                          # SCREENSHOT_GARDENERS_DETAIL gates capture
  tools/measure-gardener-cdqr.py, measure-gardeners.py     # density metrics
```

Harness entry points: `mkServer` / `mkServerWithNow` + `fakeDaemon`
(`bead_show_test.go`), `gardenerPageNow` (`gardener_page_test.go`),
`mkGardenerBead` (`gardener_test.go`), `goldenGardenerBead` +
`renderGardenerGolden` (`gardener_golden_test.go`).
