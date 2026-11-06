# Gardener Stop Conditions — Review & Proposal (2026-06-07)

> **Status: Historical snapshot (2026-06-07).** This is the "two gardener
> systems in one codebase" review that motivated the resume epic; its proposal
> has since landed. See [`gardener-system-reference.md`](./gardener-system-reference.md)
> (§3 stop/resume gates) for the current state and
> [`gardener-resume-design-2026-06-09.md`](./gardener-resume-design-2026-06-09.md)
> for the resulting design. Kept for provenance.

> **Author:** thread agent `thread-1780847101-610759`
> **Operator:** Matt Baker (`oidc:matthew.baker@acme.example`)
> **Origin:** Slack thread — *"review the current gardeners stop conditions
> wrt e.g MR requests and consider what a better endpoint and pattern would
> look like for the system; prepare suggestions and a bindle/decision."*
> **Tracking bead:** ka-0000005185

## TL;DR

There are **two gardener systems living in one codebase**, and they are
out of sync:

1. **The documented engine** — a rich declarative `stop:` / `triggers:` /
   `progress:` DSL (`gardener-loop-runbook.md`). The Go scheduler
   (`internal/scheduler/gardener_stop.go`, `triggers.go`, `progress/`)
   *implements* most of it: `loop_end`/`run_end`/`hard_stop`, `max_runs`,
   `wall_clock`, `bead_query`, `no_progress_for`, MR-aware `bead_event`
   wakes, progress scoring.

2. **What gardeners actually run** — a *bare* `cron` + `conditions`
   (open-children-of-epic `bead_query`) trigger, with the terminal goal,
   run budget, and wall-clock caps folded into the **prompt as prose**.
   The structured `stop` / `run_budget` / `terminal_goal` / `progress`
   fields are **null on essentially every live gardener**.

The capability Matt asked about — *"stop/wake with respect to MR
requests"* — **already exists and is near-real-time**, but it is
undiscoverable and unwired for ~all gardeners. The fix is mostly about
**closing the authoring gap** (schema + endpoint + defaults), not building
new engine.

## 1. Empirical state of the fleet (2026-06-07)

16 live `type=trigger` gardeners surveyed. Configuration shape:

| Stop mechanism | # gardeners | How "stop" actually works |
|---|---|---|
| `cron` + `conditions` bead_query + **prose prompt** | 14 | Cron pre-flight only fires while the epic has open children; the agent reads "run budget 20 / wall-clock 168h / gb stop when done" *from its prompt* and self-polices. |
| `triggers:` array (MR/event-aware) | 1 | `gardener-delivery` (ka-0000001510) — wakes on `merge-request` bead `mr_pipeline_status ∈ {success, failed}` + a cron floor gated on ready-queue. |
| Neither (cron-only, no gate) | 1 | weekly roll-up gardeners. |

**Structured loop fields actually populated:** `run_budget` = 0/null,
`max_runs` = null, `terminal_goal` = null, `stop` = null, `progress` =
null on **every** gardener inspected. The gardeners that *do* carry a
`gardener` JSON blob (ka-0000004561, ka-0000007954, ka-0000001271, …) carry
only **runtime bookkeeping** (`runs_spawned`, `run_log`, `last_agent_id`,
owner attribution) — never an authored stop spec.

### Why the structured path is dead on arrival

`gb gardener start` (`cmd/gb/gardener.go`) emits the bare shape **on
purpose**, and the code comments say so:

> *"The blob is intentionally inert as a loop spec: run_budget=0, no stop
> conditions … The bare trigger:schedule shape can't carry the §5.1
> structured \[fields]."*

The blocker is the **config schema**. The `type:trigger` config bead
(`ka-0000001965`) enumerates the allowed fields. `triggers`, `stop`,
`progress`, `run_budget`, `max_runs`, `terminal_goal`, `assign_bead`,
`exit_condition`, `run_timeout`, `consecutive_failures` are **NOT in the
schema** → bead creation rejects them as `unknown field`. So:

- The engine can *read* a nested `gardener` blob (it bypasses the flat
  schema via `bead.Fields["gardener"]` JSON unmarshal in `triggers.go`),
  **but the CLI never writes structured loop behaviour into it.**
- The runbook's §5.1 "rich shape" is documented as the engine's
  *capability*, with an explicit caveat that the live schema rejects it.

This is the core finding: **the stop-condition engine is more capable
than the authoring surface lets anyone use.**

## 2. Consequences (the "stop conditions wrt MR requests" problem)

### 2a. Stop is prose, so it's unenforceable and unobservable

A prose stop condition ("run budget 20, wrap up when children close") is:

- **Not machine-checkable** — nothing debits the budget; the scheduler's
  `max_runs`/`no_progress_for`/`consecutive_failures` kinds never fire
  because the fields are null. The gardener only stops if *the agent
  chooses to* `gb stop`, or the cron pre-flight goes dormant because the
  epic's children all closed.
- **The only real backstop is the cron pre-flight** (epic still has open
  children?). That is a *coarse* stop: it cannot catch a wedged loop
  spinning no-ops against still-open children, a runaway spend, or a
  gardener that keeps re-opening / re-parenting work under the epic.
- **Invisible to the meta-gardener.** The effectiveness rubric
  (`gardener-effectiveness-rubric.md`, D6 "loop hygiene") scores gardeners
  on *having* `terminal_goal` / `run_budget` / sane stop conditions — but
  since the authoring path can't write them, every gardener structurally
  fails D6 and the rubric can't enforce its own suggestions.

### 2b. MR-aware wake/stop exists but is orphaned

"Wake when my MR goes green / e2e lands" is the canonical
delivery-gardener need. The runbook §8.2 recipe works and is
**near-real-time** (GitLab pipeline webhook → `handlePipelineWebhook`
stamps `mr_pipeline_status` on the `merge-request` bead → `bead.updated`
rides the SSE full-payload path into the wake engine). Exactly **one**
gardener uses it.

There is **no first-class "MR" stop/wake kind** — by design (task
ka-0000007885 concluded *document, don't build*). But "document" only
helps if the authoring surface surfaces it. Today an operator running
`gb gardener start <epic>` gets a cron-only gardener with **zero** MR
awareness and no flag to add it.

The symmetric gap on the **stop** side: there's no clean way to say
*"end this gardener's loop once MR !1234 is merged"* — you'd hand-write a
`specific_bead_closed` / `bead_query` against the MR bead into a `stop`
blob the schema rejects.

## 3. What a better endpoint + pattern looks like

Three changes, ordered by leverage. They are independent but compound.

### Proposal A — Widen the `type:trigger` config schema to admit the loop spec (foundational)

Add `triggers`, `stop`, `progress`, `run_budget`, `max_runs`,
`terminal_goal`, `assign_bead`, `exit_condition`, `run_timeout`,
`consecutive_failures` to the `type:trigger` config schema
(`ka-0000001965`), OR — cleaner — bless the **nested `gardener` blob** as
the single carrier and have the schema validate `gardener` as a structured
sub-object (the engine already round-trips it). This unblocks A→C; nothing
else can land structured stop conditions until the schema stops rejecting
them.

**Cost:** small (schema bead edit + a validation pass). **Risk:** low —
the engine already decodes these fields; we're legalizing what it reads.

### Proposal B — `gb gardener start` writes structured stop conditions by default (ergonomics)

Today `--max-runs` / `--terminal-goal` / `--loop-wall-clock` /
`--run-wall-clock` exist as flags but are **only injected into the prompt
prose**. After A, have `gb gardener start` *also* emit a real `stop` blob:

```json
{
  "loop_end": {"any": [
    {"kind": "bead_query", "op": "==", "value": "0",
     "types": ["task","bug","epic"], "parent": "{{assign_bead}}",
     "status": ["open","in_progress","blocked"]},
    {"kind": "max_runs"},
    {"kind": "no_progress_for", "runs": 3}
  ]},
  "hard_stop": {"any": [
    {"kind": "consecutive_failures", "value": 5},
    {"kind": "wall_clock", "duration": "168h"}
  ]}
}
```

plus `run_budget` / `max_runs` / `terminal_goal` as real fields and
`progress.surface.in_wrapup: true` so `gb stop` enforces a
`progress_score`. Keep the prose as a *human-readable echo*, but make the
machine fields the source of truth. This makes every new gardener pass the
rubric's D6 and gives the scheduler real backstops (wedge detection,
failure cap) instead of trusting the agent to self-stop.

**Cost:** medium (code in `gardener.go` + tests). **Risk:** low-medium —
behaviour change is *adding* backstops; the cron pre-flight stays as-is.

### Proposal C — First-class MR awareness on the gardener endpoint (the MR-request ask)

Give `gb gardener start` (and a `gb gardener watch-mr` subcommand) opt-in
MR wake/stop flags that expand to the §8.2 recipe:

- `--wake-on-mr` → append a `bead_event` trigger entry matching
  `type==merge-request` + `mr_pipeline_status ∈ {success,failed}` scoped to
  the gardener's project/epic, **plus** the cron floor (the hybrid §8.6
  default). The gardener wakes the moment its crew's MR turns green/red
  instead of waiting up to a full cron interval.
- `--stop-on-mr-merged <mr-bead-or-url>` → append a `specific_bead_closed`
  (MR beads close on merge) to `stop.loop_end.any`, so a delivery gardener
  whose *whole job* is "land this MR" terminates exactly when it lands.

This turns the orphaned-but-working MR recipe into a one-flag default for
the gardeners that need it, which is the concrete answer to *"a better
endpoint/pattern wrt MR requests."*

**Cost:** medium (flag plumbing + recipe expansion, reusing
`gardener-delivery`'s proven shape). **Risk:** low — it's the existing
recipe behind a flag.

### Non-goals / explicitly rejected

- **A dedicated `pipeline_status` / `mr_event` wake *kind*.** Already
  evaluated (ka-0000007885) and rejected: it would duplicate the
  webhook→bead→SSE signal. Proposal C rides the existing `bead_event`
  path; we are *not* reopening that.
- **Ripping out the cron pre-flight.** It's a good coarse floor and a
  cheap dormancy signal; keep it as the §8.6 hybrid floor under C.

## 4. Recommendation

Do **A then B then C**, but A is the gate — nothing structured can land
without it. B delivers the biggest correctness win (real, enforced stop
backstops fleet-wide + rubric D6 finally meaningful). C is the direct
answer to the MR-request framing and is cheap once A+B land.

If only one thing ships: **A+B together** (schema + default structured
stop), because that's what makes "stop conditions" mean something the
machine enforces rather than prose the agent may ignore.

## Appendix — source map

| Concern | File |
|---|---|
| Stop-spec evaluator (engine) | `gardenio/controller/internal/scheduler/gardener_stop.go` |
| Stop-spec types / slots | `gardenio/controller/internal/scheduler/stop.go` |
| Trigger parse + `GardenerSpec` struct | `gardenio/controller/internal/bridge/triggers.go` |
| Gardener fire / stop reuse | `gardenio/controller/internal/bridge/gardener_evaluator.go` |
| `gb gardener start` (authoring endpoint) | `gardenio/controller/cmd/gb/gardener.go` |
| Config schema (rejects loop fields) | config bead `ka-0000001965` |
| MR pipeline → bead stamp | `gitlab_sync.go` `handlePipelineWebhook` / `UpsertMergeRequestBead` |
| Runbook (documented DSL) | `gardenio/docs/gardener-loop-runbook.md` |
| Wake-condition assessment | `gardenio/docs/gardener-wake-conditions-assessment.md` (git tag `archive/docs-prune-pre-delete-2026-10-31`) |
| Effectiveness rubric (D6 hygiene) | `gardenio/docs/gardener-effectiveness-rubric.md` |
