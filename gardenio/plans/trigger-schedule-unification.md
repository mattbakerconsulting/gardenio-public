# Trigger / Schedule Unification

**Tracking**: [ka-0000007654](https://beads-viewer.gardenio.e2e.example.com/ka-0000007654)
**Symptom that started this**: [ka-0000005670](https://beads-viewer.gardenio.e2e.example.com/ka-0000005670) — "Inbox Revamp Gardener (every 30m)" — never fires.
**Slack thread**: thread-1778268607-618399 (original); thread-1779403236-753269 (3b research, 2026-05-21); thread-1779466178-986089 (3b re-land, 2026-05-22).

> **Re-land note (2026-05-22, thread-1779466178-986089):** The original Phase 3
> recommendation in this doc was 3c (fold `trigger:schedule` into `type=schedule`).
> Operator (Matt Baker) overrode that on 2026-05-21 in favor of **3b — `type=trigger`
> absorbs `type=schedule`** — strictly more powerful end state: gardeners fire on any
> trigger source, not just cron+bead+decision. This re-land rewrites the Phase 3 section
> with 3b as the canonical direction and links the Phase 3b epic. Phase 1 has shipped;
> Phase 2 (the `/triggers` UI) remains the prerequisite for Phase 3b.

## TL;DR

We have **two parallel primitives** for cron-driven gardeners:

| Primitive | Storage | Loop runtime | Status |
|---|---|---|---|
| `type=schedule` bead | `kind=config` | `internal/scheduler/` | **Live**. Run-budget, exit_condition, terminal_goal, prior-run summaries, `gb schedule complete/extend/progress/fire`. |
| `type=trigger` bead, `trigger:schedule` label | `kind=data` | `internal/bridge/` (`ScheduleTriggerSource` + `TriggerEngine`) | **Live.** Registered in `cmd/trigger-engine/main.go` (commit `969040082`, 2026-05-09). Was a silent no-op when this doc was first written. |

> **Status update (2026-06-18):** This TL;DR captured the state at authoring
> time. Phase 1 has since shipped: `ScheduleTriggerSource` (`969040082`,
> 2026-05-09) and `BeadStateTriggerSource` (`6c18f1344`, 2026-05-10) are
> registered & live in `cmd/trigger-engine/main.go`. `AccountTriggerSource` was
> registered in C0 (ka-0000004826). Only `trigger:slack-reaction` remains
> unregistered (follow-up ka-0000002523). The historical narrative below is
> preserved as-written for provenance.

Six `trigger:schedule` beads were open and configured at authoring time (ka-0000005670, ka-0000003676, ka-0000003790, ka-0000001007, ka-0000001580, ka-0000008167). **At the time none of them fired.** The same fate hit `trigger:bead`, `trigger:account`, and `trigger:slack-reaction` — only `jira`, `gitlab`, and `webhook` sources were wired up in prod. (Now corrected: bead + schedule + account sources are registered; see the status update above.)

The user's request decomposes into three layers:
1. **Bug**: stop pretending these triggers work — either wire them up or rip the docs/CLI/advice that lead agents to write them.
2. **UX (centerpiece)**: rebuild beads-viewer `/schedules` into `/triggers`, with cron schedules as one source among many, and considerably better UX (per the operator's explicit ask in thread).
3. **Design**: unify the primitives themselves so authors aren't forced to pick between them.

---

## Why ka-0000005670 doesn't fire (proof)

```
$ grep -rn "RegisterSource" gardenio/controller/cmd/ | grep -v _test
gardenio/controller/cmd/gitlab-bridge/main.go:236: triggerEngine.RegisterSource(bridge.NewGitLabTriggerSource(...))
gardenio/controller/cmd/trigger-engine/main.go:100: triggerEngine.RegisterSource(webhookSrc)
```

JIRA is wired through `bridge.NewJiraTriggerEvaluator` in `cmd/jira-bridge/main.go`. **At authoring time, no process registered `ScheduleTriggerSource`, `BeadStateTriggerSource`, `AccountTriggerSource`, or `SlackReactionTriggerSource`.** (Resolved by Phase 1 / C0: schedule, bead, and account sources are now registered in `cmd/trigger-engine/main.go`; only `SlackReactionTriggerSource` remains — see the status update in the TL;DR.)

Confirming by audit: `kd list -l auto:trigger-audit -l source:schedule` returns zero rows — no fire ever recorded for any `trigger:schedule` bead. Compare with 226k+ `trigger:jira allow ...` audit beads. The trigger-audit pipeline works; nothing is calling it for schedule sources.

The `ScheduleTriggerSource.LoadTriggers` / `Evaluate` code itself is correct — it's just unreached.

Source: `gardenio/controller/internal/bridge/trigger_source_schedule.go:43-117`. Feature commit `4bb8bb72` (2026-04-12) added 1,463 lines, never followed by a registration commit.

---

## How the two primitives differ today

### `type=schedule` (Scheduler)

`gardenio/controller/internal/scheduler/scheduler.go:945`

```go
result, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
    Types:    []string{"schedule"},
    Statuses: []string{"open", "in_progress"},
})
```

Gardener-loop primitive. Strengths the bridge engine doesn't have:

- **Run budget** (`max_runs`, `runs_spawned`, `runs_completed`, `consecutive_failures` → auto-disable on N failures).
- **Terminal goal** + prior-run wrap-up summary surfaced via `gb prime`.
- **Exit condition** that closes the schedule when met.
- **Manual fire** (`gb schedule fire`) and self-management subcommands.
- **One-spawn-at-a-time** semantics tied to `last_agent_id` lifecycle.
- **Triggers / stop / progress** condition families on a single schedule (`triggers.go` evaluator), including the `preflight` field discussed in the parent plan `gardener-preflight-gating.md`.

### `type=trigger` + `trigger:schedule` (TriggerEngine)

`gardenio/controller/internal/bridge/trigger_source_schedule.go:57`

Generic trigger model. Strengths the scheduler doesn't have:

- **Pluggable sources** (jira, gitlab, bead-state, account, slack-reaction, webhook) with shared dedup, cooldown, max_concurrent, audit, permission/spawn-authz recheck.
- **Pre-flight `Conditions`** (`ready_queue`, `active_agents`, `gate`, `bead_query`) that are *implemented* in `scheduler/conditions/` and shared between the two systems.
- **Per-fire trigger result bead** with `trigger:<id>` label — first-class history.
- **`apply_formula`, `create_bead`, `notify` actions** in addition to `spawn_agent`.

### Where they overlap today

- `cron` semantics: both use `scheduler.ParseCron`. Same expression syntax.
- Conditions DSL: `scheduler.Condition` and `scheduler.EvaluateConditions` are shared by both.
- `spawn_agent` action: both produce an agent pod with role + project + prompt.

The cores literally call into the same packages. The split is operational (which binary owns the loop) rather than algorithmic.

---

## The user's complaints, mapped to fixes

| Complaint | Fix |
|---|---|
| "this bead ka-0000005670 I don't think is working, which is bad and annoying" | **Phase 1**: wire up `ScheduleTriggerSource` + the other three dead sources, OR explicitly retire `trigger:schedule` and migrate the 6 gardeners to `type=schedule`. |
| "trigger beads like this should show up as schedules" + "make schedules just part of [triggers]; make the UX considerably better in the process" | **Phase 2 (centerpiece)**: rebuild `/schedules` as `/triggers` with all source kinds (cron, jira, gitlab, bead, account, webhook, slack-reaction), source pills, multi-axis filter chips, per-row drawer with fire history, force-fire button, summary bar, sparklines for fire rate, "silent triggers" health pill. Operators get one honest console for every firing thing. |
| "we should plan some kind of unification" | **Phase 3**: pick one primitive as canonical and converge. |

---

## Proposed phasing

Three phases. Phase 1 unblocks today's pain; Phase 2 hides the split from operators; Phase 3 closes the divergence permanently. Each phase is independently shippable and reversible.

### Phase 1 — Stop the silent no-op (small, urgent)

**Problem**: Six gardeners that look correct and reference the right primitive are silently doing nothing. Worse, agents are *still being told* to author `trigger:schedule` beads — ka-0000004019 and `gb trigger create schedule` actively encourage it.

**Fix path A — wire it up (preferred, low risk)**:

1. Register `ScheduleTriggerSource` in the binary that already owns `TriggerEngine` for the bead-bus sources. The natural home is the controller (`cmd/controller/main.go`) since `BeadTriggerNATSDispatcher` already forwards `bead.created/closed` events into `sched.TriggerEngineRef()`. Currently nothing constructs a `bridge.TriggerEngine` here — `sched.TriggerEngineRef()` returns the *scheduler's* internal engine. Two options:
   - **A1**: Add a `bridge.TriggerEngine` to the controller and register `Schedule + BeadState + Account + SlackReaction` sources there. Drive its `Evaluate` from a 30s ticker (matches `TRIGGER_POLL_INTERVAL` default).
   - **A2**: Move the sources behind the existing `slack-bridge` binary, which is already long-running and has the Slack client wired up (needed for `notify` action and slack-reaction source).
   
   Recommendation: **A2 for `slack-reaction`** (it needs the Slack client anyway), **A1 for `schedule`/`bead`/`account`** (controller already has bead-bus). Each registration is ~10 LOC.

2. Unit tests already exist (`trigger_source_schedule_test.go` is 1,017 lines). Add an integration smoke that spins up the engine with the registered source and asserts a fire produces a `source:schedule` audit bead.

3. Verify on prod by re-saving ka-0000005670 (touch `last_checked` to force a reload), waiting one cron tick (≤30 min for `17,47 * * * *`), and confirming an audit bead appears under label `trigger:ka-0000005670`.

4. Either:
   - Backfill the missing **dogfood site** for `trigger:bead`/`trigger:account` (the rollout epic ka-0000002187 planned this but the work paused), OR
   - Mark those sources experimental in docs until somebody actually uses them.

**Fix path B — retire `trigger:schedule`**:

If we decide the canonical primitive is `type=schedule`, then:
1. Migrate the 6 open `trigger:schedule` beads to `type=schedule` (script: copy fields, set `trigger_mode=cron`, close the trigger bead with a comment pointing at the new schedule).
2. Make `gb trigger create schedule` print a deprecation pointer to `gb schedule create`.
3. Delete `ScheduleTriggerSource` and its tests.
4. Update ka-0000004019 (advice) and `docs/triggers.md` "Sources → schedule" to reflect retirement.

**Recommendation: Path A.** The trigger engine already wins on dedup/audit/permission/multi-action breadth, and Path A unblocks the 4 dead sources at once. Path B is more code deletion but throws away `trigger:bead` and `trigger:account` use cases that the rollout epic wanted.

**Cost**: ~40 LOC registration + ~80 LOC integration test + 1 doc patch. ~1 crew session.

### Phase 2 — Rebuild `/schedules` as `/triggers` (CENTERPIECE)

**Per operator (Matt, in thread)**: "make a large part of the plan to enhance bead-viewer 'schedule' into 'triggers' and make schedules just part of it; and make the UX CONSIDERABLY better in the process."

This is the unification surface that operators actually touch. The current `/schedules` page (`gardenio/controller/cmd/beads-viewer/schedules.go`, 95-line template) is a single table of `type=schedule` beads with a 3-chip filter (`all/enabled/disabled`) and a 2-option sort (`next-fire/cron`). It hides the 6 live `trigger:schedule` beads, all `trigger:jira`/`trigger:gitlab`/`trigger:bead` configs, and gives no insight into what those triggers actually *did* recently.

The redesign turns `/schedules` into `/triggers` — one page that shows **everything that can fire an action**, with `/schedules` becoming a saved view (`/triggers?source=cron`).

#### 2a. Data model: query everything that fires

Replace the single query with a parallel fetch (5-10 parallel calls, all bounded, none > 200 rows):

| Query | Source | Notes |
|---|---|---|
| `Types=[schedule]` | scheduler | Existing rows. |
| `Types=[trigger] Labels=[trigger:schedule]` | bridge schedule source | The 6 currently-dead gardeners. |
| `Types=[trigger] Labels=[trigger:jira]` | bridge jira source | High-volume firing. |
| `Types=[trigger] Labels=[trigger:gitlab]` | bridge gitlab source | E.g. release-owner. |
| `Types=[trigger] Labels=[trigger:bead]` | bridge bead-state source | Lifecycle-driven. |
| `Types=[trigger] Labels=[trigger:account]` | bridge account source | Threshold-driven. |
| `Types=[trigger] Labels=[trigger:webhook]` | bridge webhook source | Inbound HTTP. |
| `Types=[trigger] Labels=[trigger:slack-reaction]` | bridge slack source | Emoji reactions. |

Each row is normalized into a single `triggerRowView` shape. Fields that don't apply for a source render as `—` (e.g. `cron` is empty for jira; `event_type` is empty for cron schedules).

#### 2b. UX upgrades — "considerably better"

Today's page is a scrollable static table. The redesign makes it the gardener console.

1. **Source-aware columns and badges**.
   - Each row gets a colored source pill: `cron` / `jira` / `gitlab` / `bead` / `account` / `webhook` / `slack-reaction`. One glance to see what kind of trigger it is.
   - For cron rows: render `Next fire` (existing) plus a "fires every X" sparkline of the last N runs (5 dots green/red for last 5 fires). `kd list -l trigger:<id> --kind=event --limit=5` already gives us the data.
   - For event rows (jira/gitlab/bead): render `Last fired` + "fired N times in last 24h" instead of cron next-fire.
   - **Health column** (new): green if last fire succeeded; yellow if cooldown-suppressed within last cycle; red if last fire errored / spawn-denied; gray if never fired.

2. **Filter chips upgraded from {all,enabled,disabled} to a multi-axis filter strip**:
   - **Source**: cron / jira / gitlab / bead / account / webhook / slack-reaction — multi-select.
   - **Status**: enabled / disabled / closed.
   - **Health**: healthy / warning / failing / never-fired.
   - **Project**: dynamically extracted from the row set (only the projects actually represented).
   - **Role**: same, only roles that appear.
   - All chips are URL-state (`?source=cron,jira&health=failing`) so views are bookmarkable and shareable in Slack.

3. **Inline expansion: per-trigger drawer** (replaces "click bead, navigate away").
   - Click any row → drawer expands underneath showing:
     - Last 10 fire audits (`kd list -l trigger:<id> --kind=event --limit=10`), each linked to the produced agent / task bead.
     - The current cooldown / next-fire status.
     - The last failure reason if any.
     - Actions: **Disable**, **Force-fire** (calls `gb schedule fire` for `type=schedule` and the equivalent for trigger beads — see Phase 1.5 below), **Edit** (jumps to bead detail).
   - Drawer keeps the table sorted/filtered context; no full-page reload.

4. **Top-of-page summary bar**.
   - "32 triggers configured · 6 silent (no recent fires) · 2 failing · ~480 fires/day average".
   - Each pill is a filter shortcut. The "6 silent" pill auto-filters to current dead `trigger:schedule` beads — making it obvious which gardeners aren't firing without having to scroll.

5. **Sort axes** (replaces cron/next-fire only):
   - `next-fire` (cron only, mixed sources push event rows below).
   - `last-fired` (newest at top — what's hot right now).
   - `fire-rate-24h` (most active first).
   - `health` (failures first).
   - `project / role` (group by).

6. **Manual fire button on the row** for cron rows — the most-requested operator action, currently buried in `gb schedule fire <id>`. Phase 1 wires up an HTTP POST endpoint (`POST /triggers/{id}/fire`); Phase 2 adds the button in the drawer with confirm-modal.

7. **Compact + dense view toggle**. Default = compact rows showing source / title / next-fire / health / 24h count. Dense view = current full-row layout. Operator preference persisted in localStorage.

8. **Empty-state guidance**. When a source has no rows, render "No `trigger:bead` triggers configured. Author one with `gb trigger create bead --help`." Closes the loop on agent advice ka-0000004019 discoverability.

#### 2c. Make `/schedules` a saved view

Keep `/schedules` as a redirect (or alias) to `/triggers?source=cron` so existing bookmarks, advice, and Slack links don't break. Nav-bar shows "Triggers" with the cron-only legacy link reachable via the source filter.

#### 2d. Dependencies / Phase ordering

- **Phase 1** must land first: until `ScheduleTriggerSource` is registered, the new `/triggers` page would render 6 gardeners as configured-but-silent and operators would distrust the data.
- **Phase 1.5 (small)**: server-side endpoints `POST /triggers/{id}/fire` and `POST /triggers/{id}/disable` to back the row buttons. ~50 LOC, can ship with Phase 2 or just before.
- **Phase 2** can ship in two PRs:
  - **2-pass-A**: data layer + new template + source pills + multi-axis filters (the "make `/schedules` show everything" piece). ~400 LOC + ~250 LOC tests.
  - **2-pass-B**: drawer expansion + summary bar + sparkline + force-fire button. ~500 LOC + ~250 LOC tests.

Splitting lets us put 2-pass-A behind operators within a day of Phase 1, with the polished UX following.

#### 2e. Test plan for Phase 2

- Per-source query coverage: each of the 7 source-label queries returns rows in unit tests with a stub daemon.
- Filter combinatorics: `?source=cron&health=failing` returns only rows that match both axes.
- URL state round-trip: every chip click writes a query param; loading the URL reproduces the chip state.
- Drawer fetch: clicking a row emits one HTTP fetch for fire-history, render result on success, surface error on failure.
- Force-fire flow: clicking the button calls `POST /triggers/{id}/fire`, polls for a new audit bead, confirms within 30s.
- Backwards-compat: `/schedules` still renders identically for users who haven't migrated.

**Cost**: 2-pass-A ~1.5 crew sessions, 2-pass-B ~2 crew sessions. Total Phase 2 ≈ 3-4 crew sessions of focused work. The visible payoff is large — operators get a single, honest console for everything that runs without them.

### Phase 3 — Converge primitives (large)

> **Operator decision (2026-05-21, thread-1779403236-753269 → thread-1779466178-986089):**
> **Direction is 3b — `type=trigger` absorbs `type=schedule`.** Authoritative; this section
> supersedes the original "Recommendation: 3c" call. Reasoning: triggers are strictly more
> general (gardeners fire on any source: cron + bead_event + decision_resolved + jira + gitlab
> + webhook + slack-reaction + account), and "the trigger engine wins on dedup / audit /
> permission / multi-action breadth" (Phase 1 doc, above). The gardener-loop fields
> (run_budget / terminal_goal / stop spec / progress / run_log) are the *missing piece* on
> `type=trigger`, and that piece is well-scoped to add. Once added, `type=schedule` has no
> remaining unique surface and can be retired.

**Option 3a — Keep both, add a façade.** ❌ Not pursued. Defers the convergence indefinitely
and leaves authors stuck choosing between two primitives.

**Option 3b — Fold `type=schedule` into `type=trigger` (CHOSEN).** Move the gardener-loop
fields (`triggers:` array, `stop`, `progress`, `terminal_goal`, `run_budget`,
`max_runs`, `runs_spawned`, `runs_completed`, `consecutive_failures`, `run_log`,
`last_run`, `last_agent_id`, `one_spawn_at_a_time`) onto the trigger bead schema. Migrate
the controller's `internal/scheduler/` loop to operate on label-selected `type=trigger`
beads (likely label `kind:gardener` or field `role:gardener`). `TriggerEngine` becomes the
single firing surface for everything; the scheduler becomes a gardener-decoration package
that consumes fire events from `TriggerEngine`. Net: one bead type. ~3-4 weeks of focused
work; touches the scheduler engine, ~30 tests, all open `type=schedule` beads (16 today),
docs, and the `kd` / `gb` CLIs.

**Option 3c — Fold `type=trigger:schedule` into `type=schedule`.** ❌ Not pursued.
Originally recommended on the "gardener-loop fields are harder to retrofit" argument, but
operator preference is the more-powerful end state: gardeners fire on *any* source, not
just cron + bead + decision. Carrying two bead types forever is a footgun and 3c leaves
trigger beads as second-class citizens forever.

**Sequencing.** This phase is gated on Phase 2 (`/triggers` UI) — the page rename and the
density-managed trigger view land **first** so operators have a single console before the
schema convergence starts churning bead types under them. Phase 3b kicks off ~3-4 weeks
after Phase 2 ships.

**Scope guardrails for 3b.** The convergence is large. To keep it tractable:

- **Schema convergence first, then engine convergence.** Land the new gardener-loop fields
  on `type=trigger` and dual-write from `type=schedule` so both engines see the same
  shape. Only after that's stable, move the scheduler loop to consume triggers.
- **One gardener at a time during migration.** Each existing `type=schedule` bead gets
  migrated and verified on its own cadence before moving the next. No big-bang.
- **Keep `type=schedule` as an alias during the migration window.** Don't remove the
  type until every open schedule is migrated and the scheduler engine is retired.

Detailed child tasks live on the Phase 3b epic (see `Pointers` below).

---

## Test plan

Each phase ships with its own tests; the cross-cutting verification is:

1. **Phase 1 verification (live)**:
   - Apply Phase 1 MR.
   - `kd update ka-0000005670 -f last_checked=` (clear) and wait ≤30m.
   - Expect: an audit bead `kd list -l trigger:ka-0000005670` appears within 1 cycle, and an Inbox Revamp Gardener agent pod starts.
   - Repeat for ka-0000001007 (next at :13/:43) — different cron, different role, second confirmation.

2. **Phase 2 verification (UI)**:
   - Open `/schedules?filter=enabled`. Expect the 6 trigger:schedule beads to appear alongside the type=schedule beads.
   - Click through, see correct cron / next-fire / last-run.

3. **Phase 3 verification (placeholder — design first)**: covered when the decision lands.

---

## Risks

- **Cap risk if Phase 1 lights up 6 gardeners at once**. Recommend: enable one at a time. Disable the other 5 (`-f enabled=false`) before merging Phase 1, then re-enable in sequence as we confirm each gardener behaves. Spawnguard already caps total spawns; this is belt-and-suspenders.
- **Audit volume**. Each `trigger:schedule` fire writes an audit bead. JIRA already does ~226k of these; another 6 gardeners × every 30m = ~290 audits/day, fine.
- **`max_concurrent` semantics**. Schedule beads use `runs_spawned`/`runs_completed` for one-spawn-at-a-time; trigger beads use `max_concurrent` per evaluation cycle. Matt's existing gardeners all pass `max_concurrent=1` so this is a no-op in practice, but document the difference in Phase 2's column.

## Open questions for the operator

These are real decision points where the plan branches. Captured here for `gb decision` if/when we move into implementation.

1. **Path A vs Path B for Phase 1?** (Wire up the 4 dead sources, or retire `trigger:schedule` and migrate 6 gardeners?)
2. **Which binary owns `ScheduleTriggerSource` if Path A?** (controller, slack-bridge, or new `trigger-engine` deployment?)
3. **Phase 3 direction?** (3a façade, 3b fold-into-trigger, 3c fold-into-schedule.)
   **Resolved 2026-05-21 → 3b** (`type=trigger` absorbs `type=schedule`). See Phase 3 section.

The decisions of record are now **Path A → Option 3b**.

---

## Pointers

- `gardenio/controller/internal/scheduler/scheduler.go:943-947` — `type=schedule` query.
- `gardenio/controller/internal/bridge/trigger_source_schedule.go:57-117` — `trigger:schedule` query (unreached in prod).
- `gardenio/controller/cmd/beads-viewer/schedules.go:87-91` — `/schedules` UI query (only sees `type=schedule`).
- `gardenio/controller/cmd/gitlab-bridge/main.go:236` and `cmd/trigger-engine/main.go:100` — only two `RegisterSource` call sites.
- `gardenio/docs/triggers.md` §"schedule" — current operator-facing claim that `trigger:schedule` works.
- `gardenio/docs/gardener-loop-runbook.md` — `type=schedule` runbook.
- ka-0000002187 — original "make trigger:schedule the default" rollout epic.
- ka-0000004019 — current advice routing agents toward `trigger:schedule` (will need updating after whichever path we pick).
- `plans/gardener-preflight-gating.md` — the parent task that surfaced this thread; its `Preflight` field on `triggers[]` lives in the *scheduler* path, separate from the dead `trigger:schedule` `Conditions` path.
- **Phase 3b epic**: filed under this thread (see ka-0000006296 tracking bead) — child tasks for schema convergence, engine migration, per-gardener migration, deprecation, and removal.
- **Phase 3b gardener** (drives the epic to completion): a `type=schedule` gardener bead (yes, ironic — the last gardener before gardeners move to triggers) cron-scheduled to spawn cycles against the Phase 3b epic until all children close.
