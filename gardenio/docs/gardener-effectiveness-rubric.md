# Gardener Effectiveness Rubric

> **Status:** v1 (2026-06-05). Authored under epic [[ka-0000007687]] in
> response to a Slack request from Matt Baker: *"design a gardener
> effectiveness rubric and a gardener to twice a day score all gardeners for
> effectiveness … the formula should have a step for making and enforcing
> suggestions on the gardeners' schedules and triggers and prompts."*

This document defines **how to score a gardener** for effectiveness, and is
the rubric the [meta-gardener](#the-meta-gardener-gardener-of-gardeners) applies
twice a day to every live gardener in the fleet.

A **gardener** is a `type=trigger` bead with `role=gardener`,
`action=spawn_agent`, pinned to a parent ("epic") bead via a
`gardener-epic:<id>` label. It wakes on a cron pre-flight (and, for hybrid
gardeners, on bead events), spawns/steers crew, adjudicates decisions, and
drives its epic's children to closure. See
[`gardener-loop-runbook.md`](./gardener-loop-runbook.md) for the loop
primitive.

---

## 1. What "effective" means

A gardener exists to **convert wake-ups into closed work** with minimal
operator burden and minimal wasted compute. An effective gardener:

1. **Makes progress** — every run moves the epic forward (child closed,
   crew dispatched, decision adjudicated, MR merged).
2. **Doesn't spin** — it doesn't burn runs producing nothing, and it goes
   dormant the moment there's no actionable state.
3. **Doesn't leak** — crew it spawns finish or are reaped; it doesn't pile
   up zombie agents or orphaned decisions.
4. **Doesn't over-escalate** — it adjudicates what it can itself and only
   surfaces genuine forks to the operator.
5. **Terminates** — it has a real `terminal_goal` and stop conditions, and
   it actually reaches them rather than running against a wall-clock cap.
6. **Is well-tuned** — its cron cadence, trigger set, and prompt match the
   actual shape of its work.

The rubric scores these as six weighted dimensions on a 0–100 scale.

---

## 2. The six dimensions

Each dimension is scored 0–5 (rounded to integers), then weighted. The
weights sum to 100.

| # | Dimension | Weight | What it measures |
|---|---|---|---|
| D1 | **Progress yield** | 30 | Fraction of recent runs that produced a counted progress event. |
| D2 | **Throughput** | 20 | Net closure of the epic's children over the scoring window. |
| D3 | **Compute economy** | 15 | Runs / progress event; rate-limited & failed runs; dormancy when idle. |
| D4 | **Crew health** | 15 | Crew it spawned that completed cleanly vs. leaked / rate-limited / stalled. |
| D5 | **Operator burden** | 10 | Decisions self-adjudicated vs. escalated; stale un-pinged decisions. |
| D6 | **Loop hygiene** | 10 | Has terminal_goal, run_budget, sane stop conditions; not running on the wall-clock backstop. |

> **Score = Σ (Dᵢ_raw / 5) × weightᵢ.** Range 0–100.

### D1 — Progress yield (weight 30)

The single most important signal. From the gardener's `gardener.run_log`
(last N runs, default N=10), count runs whose `outcome` is `done` AND whose
wrap-up declared `progress_score ≥ 1` OR which the progress engine credited
with a progress event (`child_closed`, `decision_resolved`, `mr_merged`,
`crew_dispatched`).

```
yield = progress_runs / scored_runs
D1_raw = round(5 × yield)
```

| yield | D1_raw | Reading |
|---|---|---|
| ≥ 0.80 | 5 | Almost every wake does something. |
| 0.60–0.79 | 4 | Healthy. |
| 0.40–0.59 | 3 | Half its wakes are wasted — tune cadence/preflight. |
| 0.20–0.39 | 2 | Mostly spinning. |
| > 0 | 1 | Barely moving. |
| 0 | 0 | Pure spin — candidate for stop or redesign. |

> **NB (ka-0000004982): read the deterministic D1 from `gb gardener status
> <id> --json` → `.d1.raw`, do NOT hand-derive it.** The per-run
> `progress_score` lives in `progress_state.runs[].progress_score`, **not** in
> `run_log[]` (whose entries carry no score — always `null`). `.d1.raw`
> already implements the "score ≥ 1 OR credited-event" union off
> `progress_state.runs` + `events` described below, and exposes `has_runs`
> (false ⇒ UNMEASURABLE, grade on D2/D3/D4 — never a "pure spin" D1=0). Hand-
> deriving D1 from `run_log` — or from `state.events` alone — reads a **false
> 0** (the 2026-08-26 scorecard bug, ka-0000005712).

> **The "OR credited" clause is load-bearing — you MUST read the events, not
> just the self-reported score (bug ka-0000003502).** `progress_score` is
> honestly `0` on a run that only *dispatched crew*: `crew_dispatched` is
> deliberately excluded from the engine's self-credit rescue
> (`CreditedProgressKinds`) because dispatching is not yet an outcome. The
> dispatched crew then close the epic's children **asynchronously**, *after*
> that run's wrap-up window has already closed at score 0. So a healthy
> delegating gardener shows a tail of honest `progress_score: 0` runs even
> while its epic is draining fast — and if you score D1 off `progress_score`
> alone you read a **false 0**. This is a *measurement* artifact, not a
> gardener defect: gardener ka-0000002880 scored **C (D1 0%)** one window and
> **A (D1 100%, 80 events)** the next with *no config change*; the identical
> artifact hit sibling ka-0000000136. To evaluate the "OR credited" clause you
> must pull `fields.gardener.progress_state.events` (STEP 2) and count a run as
> a `progress_run` when a `child_closed` / `mr_merged` / `decision_resolved`
> event has an `occurred_at` **at or after that run's `started_at`** — the
> child may close after the run's `closed_at`, so do NOT clamp the upper bound
> to `closed_at`. Equivalently, a `child_closed`/`mr_merged` event whose
> `occurred_at` falls in the scoring window credits the run that most recently
> started before it. **Never file a D1-driven C/D/F grade off zero
> `progress_score` alone while `progress_state.events` shows children closing
> in the window** — see the D1/D2 consistency guard in §2.1 and the §5
> "signal-mismatch, not spin" carve-out.

#### 2.1 D1/D2 consistency guard (mandatory)

D1 and D2 read the **same underlying fact** — the epic's children closing — from
two angles (per-run credit vs. net closures). They cannot legitimately diverge to
the extremes. **If D2 ≥ 4 (children actively closing this window) then D1 must be
≥ 3.** A computed `D1 == 0` alongside `D2 >= 4` is definitionally a
*measurement bug in your D1 count*, never a real "pure spin" gardener: a gardener
whose children are closing is not spinning. When you hit that contradiction, do
**not** emit the low grade — re-derive D1 from `progress_state.events` per the
box above, and if it still reads 0, treat it as a scorer defect (the events did
not surface) and grade the gardener on D2/D3/D4 evidence, noting the D1 signal was
unreliable this window.

### D2 — Throughput (weight 20)

Net child closures of the gardener's epic over the scoring window
(default = since the previous meta-gardener run, ~12h). Normalize against
the epic's open-child count so small epics aren't penalized.

```
closed_in_window = children closed since last scoring
open_now         = open|in_progress|blocked children now
burn_rate        = closed_in_window / max(1, closed_in_window + open_now)
D2_raw = clamp(round(5 × min(1, burn_rate / 0.15)), 0, 5)
```

Interpretation: closing ~15% of remaining work per 12h window scores full
marks. A gardener that closed nothing while children remain open scores 0;
a gardener whose epic has **no open children** is *not penalized here* — it
should be a stop candidate (see D6 / §4).

### D3 — Compute economy (weight 15)

Penalize waste. Three sub-signals, averaged:

- **Runs-per-progress:** `scored_runs / max(1, progress_runs)`. ≤1.25 → 5,
  ≤1.5 → 4, ≤2 → 3, ≤3 → 2, ≤5 → 1, else 0.
- **Failure rate:** fraction of last-N runs with outcome in
  {`rate_limited`, `error`, `podless_leak`, `crashed`}. 0 → +0 penalty;
  scale down to 0 at ≥40%.
- **Idle dormancy:** did the gardener correctly go dormant (no run spawned)
  when its preflight was false? A gardener firing on an empty epic loses
  points.

> **Score the preflight gate SHAPE-AWARE, off the machine verdict — never by
> eyeballing the top-level `conditions` field (bug ka-0000006215).** WHERE a
> gardener's cron gate lives depends on its trigger shape, and reading the wrong
> field produces a false "no preflight gate / fires every hour into empty
> `gb stop` calls" ding:
> - **`trigger:multi`** (a non-empty `triggers` JSON array — the authoritative
>   modern shape): the cron floor is ONE entry (`kind=="cron"`) whose per-entry
>   `preflight` (`triggers[].preflight`) is the engine-enforced fail-closed gate
>   (`scheduler/triggers.go`, `cronPreflightHolds`). On a multi trigger the
>   top-level `conditions` field is **inert** — `trigger_source_schedule.go` is
>   its only reader and it never claims a multi bead — so a `conditions=[]` there
>   is the vestigial legacy default, NOT evidence of a missing gate.
> - **`trigger:schedule`** (legacy flat shape, empty `triggers[]`): the gate is
>   the top-level `conditions` JSON array, evaluated before the flat `cron` fires.
> - **Pure event-wake** (no cron entry / no `cron` field): there is no cron floor
>   to gate — inherently gated, not a tuning gap.
>
> This dimension is now **machine-computable** (ka-0000006215), the same remedy
> D6 got: `bridge.ScoreGardenerCronGating` dispatches on shape off the SAME fields
> the engine reads and answers "are the cron wakes gated?" deterministically.
> Read it straight off `gb gardener status <id> --json` → `.cron_gating`
> (`shape`, `has_cron`, `gated`, `gated_cron_entries`/`total_cron_entries`,
> `reason`) rather than re-deriving it by inspecting `conditions`/`triggers` by
> hand. **Only a `gated == false` verdict justifies an "ungated cron" D3 ding or
> a preflight tune-task** — and even then, cross-check the actual spawn count
> (§2.2) before concluding the gardener is *actually* over-firing.

#### 2.2 Idle-spawn cross-check (mandatory before any "fires into empty runs" claim)

A D3 idle-dormancy ding or a "fires every hour into empty `gb stop` calls" tune
premise is a claim about **behavior**, so verify it against **behavior**, not
config shape. Before filing it, read the real spawn count — `gb gardener status
<id> --json` → `runs_spawned` (engine counter, floored by ground-truth spawned
beads), and the spawn-task audit trail (`kd list --created-by <gardener> --type
agent --created-after <window>`). A gardener whose `runs_spawned` has not moved
across the idle window did **not** fire idle hourly runs, regardless of what its
cron cadence *could* do. (Real incident: ka-0000001347 was graded C for "fires
every hour into empty `gb stop` calls" while its fire audit showed
`runs_spawned=1` since 2026-06-11 — zero idle spawns — because the scorer read
cron cadence + the inert `conditions=[]` instead of the spawn count and the
per-entry preflight.)

### D4 — Crew health (weight 15)

From the crew the gardener dispatched in the window:

```
clean = crew that reached gb stop with a wrap-up
leaked = crew with agent_state in {rate_limited, error} and no pod, OR
         in_progress with no live pod > 1h (per advice ka-0000007726)
D4_raw = round(5 × clean / max(1, clean + leaked))
```

### D5 — Operator burden (weight 10)

```
self  = decisions the gardener adjudicated itself (reported an artifact)
esc   = decisions it left awaiting=human
stale = decisions awaiting=human > 4h not recently reflagged (violates
        the 4h-reflag playbook step)
D5_raw = round(5 × self / max(1, self + esc)) − stale_penalty
```

A gardener that dumps every fork on the operator, or lets human-awaiting
decisions rot without pinging, scores low. (Genuine forks *should* go to
the operator — this dimension only penalizes **over**-escalation and
**neglect**, never legitimate escalation.)

### D6 — Loop hygiene (weight 10)

A static checklist scored against the gardener bead's own config:

| Check (code slug) | Points |
|---|---|
| Has a non-trivial `terminal_goal` (not the bare default). (`non_trivial_terminal_goal`) | 1 |
| Has `max_runs` / run budget set (advice [[ka-0000000974]]). (`run_budget`) | 1 |
| Has a progress-aware or bead_query `loop_end` (not wall-clock-only). (`intelligent_loop_end`) | 1 |
| Cron cadence is sane for the work — the cron parses, or the gardener is pure event-wake (not firing every minute on a weekly epic). (`sane_cadence`) | 1 |
| Not currently surviving **only** because of the wall-clock backstop — it carries a progress-aware wedge detector (`no_progress_for` + a progress spec) so it stops on a real stall before the wall-clock elapses. (`not_wall_clock_only`) | 1 |

> **D6 is now MACHINE-ENFORCEABLE (ka-0000000452).** These five checks are
> computed deterministically by `bridge.ScoreGardenerD6` /
> `bridge.ScoreGardenerD6Blob`
> (`gardenio/controller/internal/bridge/gardener_d6.go`) off the same decoded
> `gardener` blob the engine reads, so no two meta-gardener runs can score the same
> gardener differently. `gb gardener status <id>` renders the D6 line + each failed
> check inline, and `gb gardener status <id> --json` carries the full `d6` object
> (`raw`, `max`, `weighted`, `checks[]`). Score D6 from that output rather than
> re-deriving it by eye — `TestD6CheckNames_Doc` pins the check slugs so the code
> and this table can never silently drift. A spec-less / empty-blob gardener scores
> a fail-closed 0 (the "unmanaged" gardener the auto-block audit flags).

> **Wrap-up conformance note: `progress_summary` gap (audit F2, ka-0000005466).**
> The wrap-up enforcer pairs `progress_score` with a one-line `progress_summary`,
> but that hard-require holds only under `in_wrapup`+`hard` enforcement — `--force`
> stops and soft schedules leak past it. So a run can record a non-zero
> `progress_score` with an empty `progress_summary`, degrading the D1 self-report
> the meta-gardener reads. `gb gardener status <id>` flags this off the rolling
> `progress_state`: the text render prints a `⚠️ progress_summary gap: N of M
> scored run(s)…` line, and `--json` carries `progress_summary_gaps` /
> `progress_summary_scored`. This is a hygiene *observation*, not a D6 point — it
> does not change the 0–5 score; read it as a conformance signal alongside the D6
> checklist.

---

## 3. Grade bands

| Score | Grade | Meaning | Default action |
|---|---|---|---|
| 85–100 | **A** | Exemplary. | None. Optionally promote its config as a template. |
| 70–84 | **B** | Healthy. | Optional minor tuning suggestion. |
| 55–69 | **C** | Underperforming. | **Mandatory** tuning suggestion (cadence / preflight / prompt). |
| 40–54 | **D** | Wasteful or stuck. | Config-edit **proposed via operator decision** (applied only on `apply` — see §5). |
| 0–39 | **F** | Spinning / broken. | Redesign-or-stop **proposed via operator decision** (see §5). |

Plus the §4 special-case short-circuit grades (assigned before scoring, no numeric score):

| Grade | Meaning | Default action |
|---|---|---|
| **DORMANT-OK** | Recurring gardener, zero open children, recent closed children — healthy interim dormancy. | None. Do not flag. |
| **STALE-STOP** | Finite gardener, zero open children, still enabled — should have gone dormant. | Recommend `gb gardener stop` (§5 F-tier, operator-gated). |
| **NEW** | Zero runs ever (just created). | No score; re-evaluate next window. |
| **BROKEN** | Hard-stopped / consecutive failures. | Enforce a config fix or escalate. |

---

## 4. Special cases (evaluated before scoring)

These short-circuit the numeric score. **Evaluate them in priority order and
short-circuit on the first match** — STALE-STOP is the LAST resort, reached only
after the exempting cases below have all been checked and missed. Ordering is
NOT optional: on 2026-08-28 five gardeners were STALE-STOPped as false positives
because the exempting gates (E2E-fixture skip, NEW, DORMANT-OK) were applied
loosely rather than as an ordered short-circuit (bug ka-0000007466). The order:
**(0) E2E-fixture skip → (1) NEW → (2) DORMANT-OK → (3) STALE-STOP.**

- **E2E fixture** (title starts `[E2E fixture]`, or an `e2e`/`test-fixture`
  label) → **skip entirely**. Test infrastructure seeded/torn-down by the E2E
  suites, not an epic-completion gardener. Never score, never STALE-STOP (false
  positives 2026-08-28: ka-0000004536, ka-0000005860, ka-0000004925).
- **Zero runs ever** (just created) → grade = **NEW**, no score, re-evaluate
  next window — evaluated BEFORE STALE-STOP (a gardener that never fired cannot
  be "stale"; false positive 2026-08-28: ka-0000004974, 0 runs ever).
- **No open children + still enabled** → recommend `gb gardener stop`
  (it should have gone dormant). Grade = **STALE-STOP**. Determine "no open
  children" with the REVERSE child query
  `kd list --parent <epic> -s open,in_progress,blocked` (the cycle-safe
  subtree CTE the gardener's own dormancy gate uses) — NEVER `kd dep list
  <epic>`, which reads the epic's OWN forward edges and returns 0 children
  for any epic that carries an upward parent-child edge to a grandparent,
  producing a FALSE STALE-STOP (ka-0000000208). `kd list --parent` counts the
  subtree **across all projects** — it suppresses the caller's implicit
  project-label filter (ka-0000008241), so a gardenio plan with
  `project:monorepo` children is no longer miscounted as childless. (Before
  that fix the query silently ANDed the caller's `project:gardenio` label onto
  the subtree and dropped cross-project children — the direct cause of false
  "stop the gardener" proposals on live plans.) When in doubt, cross-check the
  open decisions / recent MRs in the epic subtree before proposing a stop: a
  gardener merging MRs this window is not stale. Even if the scorer miscounts,
  `gb gardener stop` is **fail-closed**: it refuses to close while the subtree
  holds any open/in_progress/blocked issue-kind child (task, bug, epic,
  feature, chore — bundles excluded) unless `--ignore-open-children` is
  passed, a defense-in-depth backstop against a bad upstream count.
- **Recurring gardener, empty frontier** → **DORMANT-OK**, *not* STALE-STOP.
  This carve-out is evaluated BEFORE the STALE-STOP case above and
  suppresses it. A **recurring** gardener is one that self-declares a
  perpetual cadence via a `gardener-cadence:recurring` (or `gardener:perpetual`)
  label on its trigger bead — agents may set this on their own bead; no
  schema change. A pure-cron trigger (`type=schedule` / `trigger:schedule`
  with a `cron` and **no** epic-drain `bead_query` `loop_end`) ALSO qualifies
  on the pure-cron shape ALONE — a null `gardener_recurring` field / absent
  `gardener-cadence:recurring` label does NOT disqualify it (keying "recurring"
  off the label alone is the bug that STALE-STOPped unlabelled pure-cron
  janitors on 2026-08-28: ka-0000007481, ka-0000007466). Such a gardener with
  **zero open children** is in its healthy resting state — an empty frontier
  between ticks is exactly what a standing inspector looks like when it is
  caught up — so grade it **DORMANT-OK** and do NOT recommend a stop as long as
  it is **still firing** (runs_spawned moving on its cadence, recent closed
  children, or a recent A/B scorecard grade). Only fall through to STALE-STOP
  if it is genuinely dead: its cron no longer fires (runs_spawned static) OR it
  has produced **zero closed children across N consecutive scoring windows**
  (default N=3) measured against **its own cron period**, not the 12h meta
  window — a `0 */8 * * *` janitor that finds nothing to clean for a few ticks
  is HEALTHY, not dead. The
  recurring signal is what separates this from a finite epic-completion
  gardener (the STALE-STOP case above), which lacks the signal and so is
  correctly stopped once its epic drains. The operator-gate is unchanged —
  STALE-STOP only ever *recommends* `gb gardener stop`, never autonomously
  disables (§5 F-tier) — so the carve-out's job is to stop emitting repeated
  noise decisions against healthy recurring gardeners (real incident: standing
  inspector ka-0000004081 was STALE-STOPped despite 46 closed children + a 6h
  re-pour cadence, then sat disabled ~11 days until an operator revived it).
- **Disabled** (`enabled=false`) → skip; not a live gardener.
- **Hard-stopped / consecutive failures** → grade = **BROKEN**, enforce a
  config fix or escalate.

---

## 5. The suggestion + enforcement step

This is the part Matt called out explicitly: the formula has a **step for
making and enforcing suggestions on the gardeners' schedules, triggers, and
prompts.** Scoring without action is useless; the meta-gardener closes the
loop.

For each scored gardener, the meta-gardener derives **targeted suggestions**
from *which dimension(s) dragged the score down*:

| Low dimension | Suggested change |
|---|---|
| **D1 low BUT D2 ≥ 4 (children closing)** | **No tuning — signal mismatch, not spin.** The gardener is a healthy delegator whose crew close children async; its honest `progress_score: 0` dispatch runs read as a false D1 0. Do NOT file a C tune-task or a prompt rewrite. Re-derive D1 from `progress_state.events` (§D1 box) and grade on real throughput. At most an **advisory comment** noting the artifact. (This is exactly the false positive that produced spurious tune-task ka-0000003502 against ka-0000002880 and the "no tuning needed" note on ka-0000000136.) |
| D1 low + many empty wakes (`.cron_gating.gated == false` **and** `runs_spawned` actually moving on empty wakes) | **Tighten the cron preflight** (add/repair the open-children gate) and/or **slow the cron** (e.g. `*/10` → `*/30`). For a `trigger:multi` gardener the gate is the per-entry `triggers[].preflight`, NOT the top-level `conditions` field — repair it there. **Do NOT file this against a gardener whose `.cron_gating.gated == true` or whose `runs_spawned` has not moved** (§2.2 / bug ka-0000006215): the gate is already present and engine-enforced, or the empty wakes never happened. |
| D1 low + preflight already gates (`.cron_gating.gated == true`) | **Rewrite the prompt** — the playbook isn't producing actions. Add explicit "if no actionable state, `gb stop`" and concrete per-step verbs. |
| D2 low, D1 ok | Crew throughput problem, not gardener spin → **suggest more `max_concurrent` crew** or escalate a crew-side blocker. |
| D3 low (high failure) | **Add backoff / reduce cadence**; investigate rate-limit source. |
| D4 low (leaks) | Prompt fix: add a "reap stale crew" step (per advice [[ka-0000007726]]). |
| D5 low (over-escalation) | Prompt fix: add self-adjudication rule for non-{epic,plan,bug} artifact decisions. |
| D5 low (stale decisions) | Prompt fix: enforce the 4h-reflag step. |
| D6 low | Set missing `terminal_goal` / `max_runs` / `loop_end`; fix cadence. |

### Enforcement tiers

The **action depends on the grade band** (§3), so enforcement is
proportional and never silently rewrites a healthy gardener. **The
meta-gardener never edits or stops a gardener on its own authority — every
config change and every stop is proposed to the operator via a
`gb decision` and only applied after the operator picks it.**

- **A / B** — *Advisory only.* File the suggestion as a `kd comment` on the
  gardener bead (or skip for A). No config change, no decision.
- **C** — *Mandatory suggestion, not applied.* File a `type=task`
  child under the gardener's epic titled `tune: <gardener>` with the concrete
  diff, **and** `kd comment` the gardener. The gardener owner (or next meta
  run) applies it. This keeps a human-auditable trail. No decision yet —
  C is a documented recommendation, not an enforced change.

  **Target-liveness is a precondition, and the target must be a real edge
  (bug [[ka-0000003967]]).** A `tune:` task is only performable while its
  target gardener is still live, so:
  1. **Stamp the sweep's selector label** (bug [[ka-0000005778]]): add
     `-l area:gardener-tuning` at create time (`kd create … -l
     area:gardener-tuning`) or immediately after with `kd label set <tune-task>
     area:gardener-tuning`. This is the label the reconcile sweep selects on;
     without it the sweep is structurally blind to the row it just filed. The
     `blocks` edge (step 3) is the belt-and-suspenders fallback, but stamp the
     label so future label-only queries find it too.
  2. **Re-resolve the target immediately before filing** and *skip the file*
     if `status != open` (or, for a `type=trigger` target, `enabled != "true"`)
     — a gardener can close between the scoring pass and the file, and a
     tune-task for a dead gardener is un-performable the moment it lands.
  3. **Record the target as a dependency edge, not just prose.** After
     creating the tune-task, `kd dep add <tune-task> <target> --type blocks`
     (the task is *blocked by* the target — un-actionable if the target is
     gone). This makes target closure machine-visible (cascade / blocked-by /
     the reconcile sweep) and navigable in beads-viewer, instead of a string
     buried in the title.
  4. **Stamp a filing cycle** (`--field tune_cycle=<ISO8601>`) so the
     reconcile sweep can expire prescriptions older than N cycles.

  **Reconcile sweep (run at the top of every meta run, before scoring).**
  The meta-gardener closes its own stale emissions. **Enumerate robustly — the
  label alone is NOT sufficient (bug [[ka-0000005778]]):** the producer only
  started stamping `area:gardener-tuning` after that bug, so every tune-task
  filed before it (measured: 9 of 10) carries only `project:gardenio`, and a
  label-only sweep reaps ~10% of its own emissions while reporting a clean
  queue. UNION two selectors and dedup by id:
  `kd list -l area:gardener-tuning --all-types -s open,in_progress` **plus**
  `kd list --all-types -s open,in_progress` filtered to titles matching
  `^[Tt]une:`. A row is an in-scope tune-task ONLY if it is BOTH `type=task`
  AND tune-shaped (title `^[Tt]une:` OR carrying a `blocks` edge to a
  trigger/epic). **Skip anything failing that shape even if it carries the
  label** — this bug bead ([[ka-0000005778]], a `type=bug`) carries
  `area:gardener-tuning` as a test row, so a reap-by-label-alone would try to
  close it. For each in-scope row resolve the target (via its `blocks`
  dependency edge or the title id) and
  `kd close` it as **moot** when the target's `status != open` (or a trigger's
  `enabled != "true"`), or as **expired** when the target is still open but the
  task is older than N cycles (default 3, ~36h) and still unclaimed. This is
  hygiene on the meta-gardener's *own* lane output — it needs **no operator
  decision** (the guardrail below governs mutating a *different* gardener, not
  reaping your own stale tune-tasks). Cap: ≤15 moot closes per run.

  > Why this matters: without it, this lane monotonically accumulates
  > un-performable rows in `gb ready` at the meta-gardener's cadence. Each row
  > is individually well-formed (real title, P2, parented, `project:` label),
  > so it passes every `gb ready` relevance gate while its premise has already
  > resolved — invisible to relevance scoring, ~5% of the ready queue, and
  > each one costs an agent a claim + investigation to discover it is moot
  > (measured 2026-08-20: 10 of 11 open tune-tasks targeted a closed
  > trigger/epic).
- **D** — *Propose config-only changes via an operator decision.* The
  meta-gardener assembles the **safe, reversible** field edits it wants to
  make (`cron`, `cooldown_minutes`, `max_concurrent`, repairing the cron
  preflight — the per-entry `triggers[].preflight` on a `trigger:multi`
  gardener, or the top-level `conditions` on a legacy `trigger:schedule`
  gardener, per §D3 / bug ka-0000006215 — setting `terminal_goal` / `max_runs`) into a
  `gb decision` with the concrete before→after diff and offers:
  (a) **apply** the proposed edits, (b) **skip** this window, (c) **edit
  differently** (operator supplies values). On `apply`, the meta-gardener
  runs the `kd update` and logs the before/after in a `kd comment`. Prompt
  rewrites are proposed in the same decision text but go in a tune-task —
  never folded into the auto-apply path. The meta-gardener **never applies
  the edit before the operator picks `apply`.**
- **F** — *Propose the destructive choice via an operator decision.* The
  meta-gardener opens a `gb decision` offering: (a) apply the proposed
  redesign, (b) **stop the gardener** (`gb gardener stop`), or (c) leave it
  and re-flag next window — then `gb yield`s and acts on the pick. It never
  stops a gardener on its own authority.

> **Guardrail.** The meta-gardener has **no autonomous write authority over
> other gardeners.** It may freely *read* every gardener, *score* it, and
> *comment* (A/B advisory) or *file a tune-task* (C). But any change that
> mutates a gardener's behaviour — a config-field edit (D) or a stop (F) —
> is gated behind a `gb decision` and applied **only** after the operator
> selects the apply/stop option. It never auto-edits a prompt, never
> auto-stops, never auto-edits config, and never touches a gardener graded
> **A–C**. Every applied edit is additionally logged as a `kd comment` with
> the before/after value so it's fully auditable and reversible. This bounds
> blast radius to zero unattended writes: the worst a buggy meta-gardener run
> can do is raise a spurious decision, which the operator declines.

---

## 6. The meta-gardener ("gardener of gardeners")

A perpetual, event-light, **cron-only** gardener that fires **twice a day**
and scores every live gardener. It is itself a `type=trigger` bead, but it is
*not* pinned to a single epic — its "epic" is the fleet of gardeners, so its
preflight is "there exists ≥1 enabled gardener", which is effectively always
true. It is therefore a **watchdog-style** gardener (runbook §5.2), the same
shape as cycle-watchdog, not an epic-driver.

### Schedule

```
cron:      "23 7,19 * * *"        # 07:23 and 19:23 — twice a day, off the :00/:30 marks
timezone:  America/Los_Angeles
cooldown:  60 (minutes)
max_concurrent: 1
```

(07:23 / 19:23 are deliberately off the round marks so the meta-gardener
doesn't collide with the dozens of gardeners that fire on `:00`/`:30`.)

### Preflight

```json
[{"type":"bead_query","op":">","value":"0","types":["trigger"],
  "labels":["role:gardener"],"statuses":["open"]}]
```

Fires only while at least one open gardener exists.

### Stop / budget (per advice [[ka-0000000974]] — every gardener must self-block)

- `terminal_goal`: *"All live gardeners are graded B or better, or have an
  open tune-task / operator decision addressing their deficiency."*
- `max_runs`: 60 (≈30 days at twice-daily) — re-authored on expiry.
- `loop_end`: `no_progress_for` 6 runs **OR** wall_clock 720h.
- `run_end`: `no_actionable_state` OR 30m wall-clock.
- `hard_stop`: 5 consecutive failures.

### Playbook prompt

The meta-gardener prompt is maintained as an **advice bead** (per the
tree-hugger v2 convention, advice [[ka-0000002990]]) and referenced by the
gardener; the canonical text also lives in §7 below for review. Each run:

1. Enumerate live gardeners: `kd list --type=trigger --label role:gardener`
   filtered to `enabled=true` and `status=open`.
2. For each gardener, pull its bead JSON (`gardener.run_log`, `cron`,
   `conditions`, `terminal_goal`, `max_runs`), its epic's child counts, the
   crew it dispatched, and the decisions raised in its subtree.
3. Compute D1–D6 and the weighted score; assign a grade (§3) after applying
   the §4 special-cases.
4. Apply the §5 suggestion + enforcement step at the tier the grade dictates.
5. Write a single **scorecard report** bead (one per run) under epic
   [[ka-0000007687]], one row per gardener (grade, score, dimension
   breakdown, action taken), and link each gardener bead via a typed mention
   so the scorecard is navigable in beads-viewer.
6. `gb squawk` a one-line fleet summary (e.g. *"Scored 14 gardeners: 9 A/B,
   3 C (tune-tasks filed), 1 D (cron-slow decision kd-… opened), 1 F (stop
   decision kd-… opened)"*).
7. `gb yield` if any D/F operator decision is open (wake when resolved,
   then apply the picked option); else `gb stop --wrapup` with
   `progress_score` and counts.

---

## 7. Canonical meta-gardener prompt

> Maintained as advice bead [[ka-0000002990]]-style; mirrored here for
> review. `gb gardener start` folds budget/goal into this text.

See [`gardener-meta-scoring-prompt.md`](./gardener-meta-scoring-prompt.md) for
the full playbook prompt text used to author the gardener.

---

## 8. Worked scoring example

A gardener with last-10 run_log: 6 `done` (5 with progress), 2
`rate_limited`, 2 `done`-but-zero-progress. Epic: closed 2 children this
window, 11 still open. Dispatched 4 crew (3 clean, 1 leaked). Decisions: 2
self-adjudicated, 1 awaiting human (pinged). Config: has terminal_goal +
max_runs + bead_query loop_end, cron `*/10` on a slow epic.

- D1: 5/10 progress → yield 0.5 → **3**
- D2: 2/(2+11)=0.15 burn → min(1, 0.15/0.15)=1 → **5**
- D3: runs/progress 10/5=2 → 3; failure 2/10=20% → mild; dormancy ok → ~**3**
- D4: 3/(3+1)=0.75 → **4**
- D5: 2/(2+1)=0.67, 0 stale → **3**
- D6: terminal_goal✓ max_runs✓ loop_end✓ cadence✗(too fast) not-wallclock✓ → 4/5 → **4**

Score = (3/5·30)+(5/5·20)+(3/5·15)+(4/5·15)+(3/5·10)+(4/5·10)
= 18+20+9+12+6+8 = **73 → Grade B.** Action: advisory comment suggesting
`*/10 → */30` cron (D6 cadence ding + D1 sub-max yield both point at
over-firing).
