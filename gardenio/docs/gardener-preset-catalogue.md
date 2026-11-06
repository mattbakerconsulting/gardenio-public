# Gardener Preset Catalogue

> **Origin:** ka-0000001939 (R0.3 — Survey live gardeners; derive preset templates),
> child of epic ka-0000001611 (Gardener Configuration Surface).
> **Method:** live survey of every open gardener on the fleet
> (`kd list --type=trigger -l role:gardener --all-projects --status open --limit 500`),
> **34 gardeners** examined 2026-08-13, every field read from the live trigger bead.
> **Schema source of truth:** `GardenerSpec` + `TriggerDef` in
> `gardenio/controller/internal/bridge/triggers.go`.

## What a gardener bead actually is

A gardener is a **`type=trigger` bead** with `role:gardener`. Two layers of config:

1. **Trigger-level fields** (top-level on the bead) — the *schedule + gate*:
   `action` (`spawn_agent` | `apply_formula`), `cron`, `timezone`, `enabled`,
   `max_concurrent` (per-cycle fire cap), `cooldown_minutes`, `conditions`
   (the pre-flight `bead_query` / `ready_queue` gate that makes the gardener go
   dormant when there is no work), `prompt` (or `formula_id` + `formula_vars`),
   `role`/`project` (what it spawns), `slack_channel`.
2. **The `gardener` block** (a JSON blob in the `gardener` field) — the *loop
   controller*: `terminal_goal`, `stop` (`hard_stop` / `loop_end` / `run_end`
   + `on_loop_end`), `progress` (events / windows / surface), `run_budget`,
   `max_runs`, `standing`, `resume_budget`, `assign_bead`, `slack_user_id`,
   `user_bead_id`.

Every preset below gives concrete values for **both** layers. The `stop` and
`progress` blocks are near-identical across most gardeners — that shared skeleton
is documented once in the **Common building blocks** appendix and referenced by
name so each preset only shows what it *changes*.

---

## The six presets at a glance

| # | Preset | Terminal? | Action | Cadence | Gate (`conditions`) | Live examples |
|---|--------|-----------|--------|---------|---------------------|---------------|
| 1 | **Plan-Gardener (Orphan-Watchdog)** | yes (plan drains) | `spawn_agent` | `:NN,:NN * * * *` (~30 min) | `bead_query > 0` on plan epic | 8 |
| 2 | **Epic-Driver** | yes (epic closes) | `spawn_agent` | `:NN,:NN * * * *` (~30 min) | `bead_query > 0` on epic | 8 |
| 3 | **Ready-Queue Drainer** | standing | `spawn_agent` | `NN */6 * * *` or `*/15` | `ready_queue`/`bead_query` on **label** | 1 |
| 4 | **CDQR Continual-Quality Gardener** | standing | `apply_formula` / `spawn_agent` | daily / `:NN,:NN` | rubric-label `bead_query` (or none) | 8 |
| 5 | **MR-Green Watcher** | yes (MR lands) | `spawn_agent` | `*/15` or bead-event | `ready_queue`/`bead_query` on MR label | 2 |
| 6 | **Cron Roll-up (Report-Only Auditor)** | single-shot, re-armed | `spawn_agent` | weekly/daily (`NN NN * * 1`) | none (unconditional) | 3 |

Fixtures (`fixture:e2e:*` — ka-0000004536, ka-0000005860, ka-0000006065,
ka-0000004925) are excluded from the patterns; they exist to exercise the
scheduler, not to do work.

---

## Preset 1 — Plan-Gardener (Orphan-Watchdog)

**Mission.** Be the single watchdog for one `/plans` plan epic: never let a step
orphan silently and never silently park. Each wake it batch-scans the plan's open
child frontier (`gb open-children <epic>` → one multi-id `kd show`), re-opens
steps whose agent died >1h ago (clearing the assignee), reflags stale
decisions/escalations gating downstream steps, and — when the whole subtree is
closed with no open blocker — *proposes* (never executes) plan completion. It
**steers** by minting new steps with `kd create … && kd dep add … --type
parent-child` (never `gb plan add-step`, which rejects an agent identity).

**When to use.** You created a **plan** (epic + `plan` label rendered on
`/plans`) and want a bound agent keeping its steps alive between crew runs. This
is the default gardener for any multi-step plan. One gardener per plan; it goes
dormant automatically when the plan drains.

**GardenerSpec + trigger fields**

```jsonc
// Trigger-level fields
{
  "action": "spawn_agent",
  "role": "crew",                       // what it dispatches onto steps
  "project": "<plan's project>",
  // Event mode (gd-gsvlS6YDj3): wake on plan activity, plus a daily watchdog
  // staggered per plan. `gb plan gardener --timed` binds a flat "cron" instead.
  "triggers": [
    { "kind": "bead_event", "label": "plan-activity",
      "events": ["created","updated","closed"],
      "match": { "field": "id", "op": "in_subtree_of", "value": "<PLAN_EPIC_ID>" },
      "debounce": "10m", "throttle": "15m" },
    { "kind": "cron", "label": "fallback-watchdog", "cron": "23 4 * * *",
      "timezone": "America/Los_Angeles",
      "preflight": "<the conditions gate below, as one expression>" }
  ],
  "timezone": "America/Los_Angeles",
  "enabled": true,
  "max_concurrent": 1,                  // singleton watchdog — never two at once
  "cooldown_minutes": 5,
  "conditions": [                       // dormancy gate: fire only while the plan has open work
    { "type": "bead_query", "op": ">", "value": "0",
      "types": ["task","bug","epic"],
      "statuses": ["open","in_progress","blocked"],
      "parent": "<PLAN_EPIC_ID>" }
  ],
  "prompt": "<the plan-gardener playbook — see below>"
}
```
```jsonc
// gardener block
{
  "terminal_goal": "All steps of plan <PLAN_EPIC_ID> are closed (or the operator has retired the plan).",
  "stop": {
    "loop_end": { "any": [
      { "kind": "bead_query", "op": "==", "value": "0",
        "types": ["task","bug","epic"],
        "statuses": ["open","in_progress","blocked"], "parent": "<PLAN_EPIC_ID>" },
      { "kind": "max_runs" },
      { "kind": "no_progress_for", "runs": 3 }
    ]},
    "run_end": {},
    "hard_stop": { "any": [
      { "kind": "consecutive_failures", "value": "5" },
      { "kind": "wall_clock", "duration": "168h" }
    ]},
    "on_loop_end": "close"
  },
  "progress": "<STANDARD_PROGRESS — see appendix>",
  "run_budget": 20,
  "max_runs": 8,
  "one_spawn_at_a_time": true,
  "assign_bead": "<PLAN_EPIC_ID>",      // the epic this gardener is bound to
  "slack_user_id": "U0OPERATOR1",
  "user_bead_id": "<operator user bead>"
}
```

**Labels:** `role:gardener`, `trigger:multi` (`trigger:schedule` when bound
`--timed`), `plan-gardener`,
`gardener-epic:<PLAN_EPIC_ID>`, `project:<project>`.

**Live examples (8):** ka-0000004590, ka-0000005973, ka-0000004265,
ka-0000000485, ka-0000006689, ka-0000001203, ka-0000002063, ka-0000001682.
The last two carry `trigger:multi` (`triggers` fan-in array) so an MR/bead event
also wakes them, not only the clock.

**Notes / gotchas.**
- The `conditions` gate and the `loop_end` `bead_query` are mirror images (`> 0`
  to fire, `== 0` to close): the loop closes exactly when the plan drains.
- `on_loop_end: close` is correct here — a plan gardener is *terminal*, not
  standing. Do not add `standing:true`.
- Never let the agent disable its own trigger outright; dormancy is automatic via
  the `conditions` gate. The one exception is an idle on a named bead: the
  gardener may set `enabled=false` with `awaiting=<bead-id>` on its own trigger,
  and closing that bead turns the trigger back on
  (`kbeads/docs/ownership-grants.md`, "Gardener self-idle").

---

## Preset 2 — Epic-Driver

**Mission.** Drive one delivery **epic** to completion: for each open child
without a live agent owner, `gb spawn --task <id> --role crew`; walk each crew's
MR toward green + merged; adjudicate crew decisions (`awaiting=gardener` →
`gb decision report` when one option dominates and `artifact_type ∉
{epic,plan,bug}`); reflag stale `awaiting=human` decisions >4h; close when every
child is closed. Differs from Preset 1 in that the terminal goal is a *rich
delivery outcome* ("a stacked set of merged MRs that PROVE …"), not just "steps
closed", and it dispatches directly against epic children rather than a
`/plans` step frontier.

**When to use.** A concrete, **bounded** engineering epic with a definition of
done ("ship feature X / land these N MRs"). You want it driven autonomously to
merged code and then to *stop*. Not for perpetual quality loops (use Preset 4)
and not for a `/plans`-rendered plan (use Preset 1).

**GardenerSpec + trigger fields**

```jsonc
// Trigger-level fields
{
  "action": "spawn_agent",
  "role": "crew",
  "project": "<epic's project>",
  "cron": "27,57 * * * *",
  "timezone": "America/Los_Angeles",
  "enabled": true,
  "max_concurrent": 2,                  // 2-3 when the epic has parallelizable children
  "cooldown_minutes": 5,
  "conditions": [
    { "type": "bead_query", "op": ">", "value": "0",
      "types": ["task","bug","epic"],
      "statuses": ["open","in_progress","blocked"], "parent": "<EPIC_ID>" }
  ],
  "prompt": "<epic-driver playbook: spawn-per-open-child, walk MRs to green, adjudicate decisions>"
}
```
```jsonc
// gardener block
{
  "terminal_goal": "All N children of <EPIC_ID> are closed, having landed the merged MRs that PROVE <the epic's concrete done-definition>.",
  "stop": {
    "loop_end": { "any": [
      { "kind": "bead_query", "op": "==", "value": "0",
        "types": ["task","bug","epic"],
        "statuses": ["open","in_progress","blocked"], "parent": "<EPIC_ID>" },
      { "kind": "max_runs" },
      { "kind": "no_progress_for", "runs": 3 }
    ]},
    "run_end": {},
    "hard_stop": { "any": [
      { "kind": "consecutive_failures", "value": "5" },
      { "kind": "wall_clock", "duration": "168h" }
    ]},
    "on_loop_end": "close"
  },
  "progress": "<STANDARD_PROGRESS — child_closed + mr_merged + wrapup_field>",
  "run_budget": 24,
  "max_runs": 24,
  "assign_bead": "<EPIC_ID>",
  "slack_user_id": "U0OPERATOR1",
  "user_bead_id": "<operator user bead>"
}
```

**Labels:** `role:gardener`, `trigger:schedule`, `gardener-epic:<EPIC_ID>`,
`project:<project>`.

**Live examples (8):** ka-0000007350 (RWX CI ownership), ka-0000005355 (E2E
coverage sprint), ka-0000002632 (JIRA outbox isolation), ka-0000007400
(firehose CW cutover), ka-0000005478 (DEVOPS-312 9-stack pilot),
ka-0000004474 (metrics/telemetry platform), ka-0000001311 (FSM nightly burn),
ka-0000004701 (FSM wave-staged backlog drain — a hardened epic-driver variant
with operator-released waves + read-only-Jira safety rails and `cooldown 15`).

**Notes / gotchas.**
- `max_concurrent` is the knob to tune here: 1 for serial/stacked work
  (ka-0000005478 stacks MRs so it caps at 1), 2-3 when children are independent.
- The `resume_budget` (default) re-arms the gardener on a *no-progress* premature
  stop rather than closing it, so a quiet epic doesn't die; only a genuinely
  broken loop (`consecutive_failures` / `wall_clock`) hard-stops.
- For MR-touching-release changes, the epic-driver must file a `gb decision`
  before merging — it can't self-approve protected `main`.

---

## Preset 3 — Ready-Queue Drainer

**Mission.** Continually drain an **unbounded, label-scoped queue** of ready
beads rather than a fixed epic subtree: each cycle, find open beads matching a
label/project filter, put a live crew agent on any that lack one, and keep the
lane moving. Unlike Presets 1-2 the gate is *not* a single `parent` epic — it's
a **label or project filter**, so newly-filed matching beads keep the gardener
alive indefinitely. This is a **standing** gardener.

**When to use.** A rolling lane that never "finishes": a triage queue, an
intake-grooming lane, a labeled backlog anyone can add to, a cost/CI-reduction
epic whose scope is "all open beads carrying label X". Mark it `standing:true`
(or add the `perpetual:true` label) so exhausting the resume budget during a
quiet period re-arms in place instead of reaping the gardener.

**GardenerSpec + trigger fields**

```jsonc
// Trigger-level fields
{
  "action": "spawn_agent",
  "role": "crew",
  "project": "gardenio",
  "cron": "17 */6 * * *",               // gentler cadence — standing lanes don't need :NN,:NN
  "timezone": "America/Los_Angeles",
  "enabled": true,
  "max_concurrent": 3,
  "cooldown_minutes": 5,
  "conditions": [                       // gate on a LABEL/queue, not a parent epic
    { "type": "ready_queue", "op": ">", "value": "0",
      "project": "gardenio", "labels": ["<lane-label>"] }
    // or bead_query form: { "type":"bead_query","op":">","value":"0",
    //   "labels":["<lane-label>"], "statuses":["open"] }
  ],
  "prompt": "<queue-drain playbook: find matching open beads, ensure each has a live crew owner>"
}
```
```jsonc
// gardener block
{
  "terminal_goal": "<standing mandate, e.g. 'Keep the <lane-label> queue drained: every open bead carrying it has a live owner'>",
  "standing": true,                      // perpetual — do NOT reap on a quiet resume budget
  "stop": {
    "loop_end": { "any": [ { "kind": "max_runs" }, { "kind": "no_progress_for", "runs": 3 } ]},
    "run_end": {},
    "hard_stop": { "any": [
      { "kind": "consecutive_failures", "value": "5" },
      { "kind": "wall_clock", "duration": "720h" }   // long — a standing lane runs for weeks
    ]},
    "on_loop_end": "close"
  },
  "progress": "<STANDARD_PROGRESS>",
  "run_budget": 100,
  "max_runs": 100,
  "slack_user_id": "U0OPERATOR1",
  "user_bead_id": "<operator user bead>"
}
```

**Labels:** `role:gardener`, `trigger:schedule`, `perpetual:true`,
`<kind:lane-label>`, `project:<project>`.

**Live example (1 clean form):** ka-0000007674 (CI-minutes-reduction — gate is
`bead_query > 0` on `labels:[project:gardenio] statuses:[open]`, cron `17 */6 * * *`,
`max_concurrent 3`). The fleet's other standing-drain gardeners (e.g. the
`gardener-rescope` needs-rescope drainer, the intake groomer) follow the same
shape from outside the `role:gardener`/open slice sampled here.

**Notes / gotchas.**
- The **only** structural difference from Preset 2 is the gate: **label/queue,
  not `parent`**. That single change flips a terminal epic-driver into a
  standing drainer. Because there is no `parent` `bead_query == 0` in `loop_end`,
  it never self-declares "done" — hence `standing:true` is mandatory (bug
  ka-0000006241: without it, a quiet standing gardener gets reaped as "stuck").
- Use `ready_queue` when you want the daemon's ready-frontier semantics
  (unblocked + unassigned); use `bead_query` when a raw status/label match is
  enough.

---

## Preset 4 — CDQR Continual-Quality Gardener

**Mission.** Perpetually hold a surface (a page, a rubric, a data-plane) to a
quality bar. Each cycle it **pours a rubric-audit formula** (or dispatches an
audit crew), scores the surface on a named rubric, files one bug per
below-bar dimension, drives the fixes, and re-pours next cycle. Never
"complete" — an empty child frontier is the healthy resting state. Named rubrics
in the live fleet: **CLUB** (Clarity/Linkage/UX/Beauty), **CAMP**, **CREWS**,
**CED**, **CRISP** (Correct/Reconciled/Intelligible/Sanitized/Presentation),
**GRADE**.

**When to use.** An operator-designated *flagship surface* ("the home page …
it's important") that must not rot: a beads-viewer page, a formula/DAG UX, cost
accuracy, Grafana dashboards, data-plane hygiene. You want continual,
incremental, self-scoring improvement with a hard "8+ across all dimensions"
bar. This is the pattern to reach for when the goal is *sustained quality*, not
*ship-and-stop*.

**GardenerSpec + trigger fields** (two sub-shapes)

```jsonc
// Sub-shape A — apply_formula (CRISP/GRADE: the audit IS a formula)
{
  "action": "apply_formula",
  "formula_id": "<rubric formula bead, e.g. ka-0000007333 for CRISP>",
  "formula_vars": "{ \"ITERATION\":\"{{fire_time}}\", \"PARENT_EPIC\":\"<epic>\", \"TARGET_URL\":\"http://localhost:8095\", ... }",
  "cron": "23 9 * * *",                 // daily — a full audit is expensive
  "timezone": "UTC",
  "enabled": false,                     // rubric gardeners ship disabled, operator flips on go-ahead
  "max_concurrent": 1,
  "cooldown_minutes": 25                 // long cooldown — one heavy audit per fire
}
```
```jsonc
// Sub-shape B — spawn_agent (CLUB/CAMP: gardener pours the formula + dispatches crew each cycle)
{
  "action": "spawn_agent",
  "role": "crew",
  "project": "gardenio",
  "cron": "23,53 * * * *",
  "timezone": "America/Los_Angeles",
  "enabled": false,
  "max_concurrent": 3,
  "cooldown_minutes": 5,
  "conditions": [
    { "type": "bead_query", "op": ">", "value": "0",
      "types": ["task","bug","epic"],
      "statuses": ["open","in_progress","blocked"], "parent": "<QUALITY_EPIC_ID>" }
  ]
}
```
```jsonc
// gardener block (common to both sub-shapes)
{
  "terminal_goal": "All <rubric> dimensions score 8+ for <surface> across two consecutive audit iterations.",
  "standing": true,                      // or the perpetual:true label — an empty frontier is NOT done
  "stop": {
    "loop_end": { "any": [
      { "kind": "bead_query", "op": "==", "value": "0", "types": ["task","bug","epic"],
        "statuses": ["open","in_progress","blocked"], "parent": "<QUALITY_EPIC_ID>" },
      { "kind": "max_runs" },
      { "kind": "no_progress_for", "runs": 3 }
    ]},
    "run_end": {},
    "hard_stop": { "any": [
      { "kind": "consecutive_failures", "value": "5" },
      { "kind": "wall_clock", "duration": "720h" }   // 30 days — quality loops are long-lived
    ]},
    "on_loop_end": "close"
  },
  "progress": "<STANDARD_PROGRESS — a clean report scores progress even with 0 bug fixes>",
  "run_budget": 500,
  "max_runs": 500,
  "slack_user_id": "U0OPERATOR1",
  "user_bead_id": "<operator user bead>"
}
```

**Labels:** `role:gardener`, `trigger:schedule`, `perpetual:true`,
`kind:<rubric>` (`kind:crisp`, `kind:grafana-grade`, `kind:users-camp`, …),
`area:<surface>`, `gardener-epic:<QUALITY_EPIC_ID>`, `project:<project>`.

**Live examples (8):** ka-0000000748 (CLUB / home page), ka-0000000242 (CLUB /
maps), ka-0000005864 (CAMP / people), ka-0000006201 (CAMP / gardeners page),
ka-0000001871 (CREWS / formulas — `apply_formula`, `cooldown 300`),
ka-0000004081 (CED / session inspector — `13 */6 * * *`, standing per decision
ka-0000005530), ka-0000008137 (CRISP / costs — `apply_formula`),
ka-0000001426 (GRADE / Grafana — `apply_formula`).

**Notes / gotchas.**
- `run_budget`/`max_runs` are **high** (500) precisely because these never
  finish — the budget is a spin-brake, not a completion count. Pair with
  `standing:true`/`perpetual:true` so budget exhaustion during a quiet period
  re-arms in place.
- **Every cycle must emit a numeric `progress_score` in the wrap-up**, and a
  *clean report counts as progress (score 1)* — the engine reads `progress_score`
  off the wrap-up and a few zero-scored cycles will self-close an otherwise
  healthy loop. This is the single most common way a quality gardener dies wrongly.
- `apply_formula` shape re-pours the whole rubric molecule each fire (via
  `{{ITERATION}}`); the `spawn_agent` shape pours the formula from *inside* the
  gardener and must link every poured step to the epic (`kd dep add … parent-child`)
  or the loop won't see its own children.

---

## Preset 5 — MR-Green Watcher

**Mission.** Drive one specific **merge request** (or one live preview
environment) from "opened" to "green + merged + verified usable". Each fire:
check the MR's pipeline / conflicts / review state, dispatch crew to
rebase/fix red or conflicted MRs, run the real end-to-end gate (browser login,
data-viewing, log-parity — often via Playwright MCP), and file a
`gb escalation` when a human merge/approval is the only thing left (agents can't
self-approve protected `main`). Terminal: the MR lands / the env is proven, then
stop.

**When to use.** A single high-value MR or deploy-preview that needs babysitting
to green, or a recurring "is this live env actually usable by a human?" gate.
Best driven **event-first**: use `trigger:bead` / a `ready_queue` gate so MR
pipeline-status changes wake the gardener, rather than pure cron polling.

**GardenerSpec + trigger fields**

```jsonc
// Trigger-level fields
{
  "action": "spawn_agent",
  "role": "crew",
  "project": "<repo project>",
  "cron": "*/15 * * * *",               // frequent while an MR is in flight; or bead-event-driven
  "timezone": "UTC",
  "enabled": false,                     // enable when the MR is ready to be driven
  "max_concurrent": 1,
  "cooldown_minutes": 15,
  "conditions": [                       // gate on the MR's label queue, not an epic subtree
    { "type": "ready_queue", "op": ">", "value": "0",
      "project": "<project>", "labels": ["<mr-label>"] }
  ],
  "prompt": "<per-MR playbook: pipeline+conflict+review check, e2e gate, escalate-on-human-merge>"
}
```
```jsonc
// gardener block
{
  "terminal_goal": "MR !<N> is merged and <the concrete usable-outcome, e.g. 'VictoriaLogs stream identity verified at parity with Mezmo'>; all epic children closed.",
  "stop": {
    "loop_end": { "any": [
      { "kind": "bead_query", "op": "==", "value": "0", "types": ["task","bug","epic"],
        "statuses": ["open","in_progress","blocked"], "parent": "<MR_EPIC_ID>" },
      { "kind": "max_runs" },
      { "kind": "no_progress_for", "runs": 3 }
    ]},
    "run_end": {},
    "hard_stop": { "any": [
      { "kind": "consecutive_failures", "value": "3" },   // tighter — a wedged MR shouldn't churn
      { "kind": "wall_clock", "duration": "168h" }
    ]},
    "on_loop_end": "close"
  },
  "progress": "<STANDARD_PROGRESS — mr_merged is the headline event>",
  "run_budget": 24,
  "max_runs": 24,
  "assign_bead": "<MR_EPIC_ID>",
  "slack_user_id": "U0OPERATOR1",
  "user_bead_id": "<operator user bead>"
}
```

**Labels:** `role:gardener`, `trigger:schedule`, `<feature-label>`
(e.g. `fluentbit-mr7`), `gardener-epic:<MR_EPIC_ID>`, `project:<project>`.

**Live examples (2):** ka-0000004788 (fluentbit MR !7 — `ready_queue` gate on
`fluentbit-mr7`, `*/15`, devel-only hard guardrails), ka-0000008247 (deploy-mr
login & data-viewing validation — Playwright browser gate, `13 13 * * *`).
The `docs/ci-green-watchdog-gardener.md` design is the fleet-wide superset of
this preset (keep *all* MR + main pipelines green).

**Notes / gotchas.**
- Tighten `consecutive_failures` (3, not 5) and use a `ready_queue`/bead gate so
  the gardener sleeps between pipeline events instead of polling a red MR forever.
- The gardener's job **ends at a human merge boundary**: file a `gb escalation`
  (`gitlab_mr_merge`, `--close-on-execute`) and let the operator merge — never
  self-approve protected `main`.
- Guardrails belong **in the prompt** (env scope, "devel ONLY, never prod,
  surface-don't-run any non-devel apply"). The spec can't encode them.

---

## Preset 6 — Cron Roll-up (Report-Only Auditor)

**Mission.** On a periodic clock, produce **one diffable report bead** and stop.
Pull objective data (`gb gardener-audit`, a repo scan, a metrics query), compute
trends, **diff against the previous report of the same `kind`**, land a single
`type=doc` report (or bundle), squawk the headline, and terminate. **REPORT-ONLY
and single-shot**: it never spawns crew, never edits/disables another gardener,
never merges — it observes and reports.

**When to use.** A recurring health/drift/trend report where the *deliverable is
the report itself*: weekly gardener-audit roll-up, weekly OSS-sanitization drift
scan, a nightly lint of the trigger fleet, a scheduled one-time pour at a fixed
future instant. Do **not** use it for anything that acts on the fleet — that's a
different gardener's job (name the finding in the report and let an actor pick
it up).

**GardenerSpec + trigger fields**

```jsonc
// Trigger-level fields
{
  "action": "spawn_agent",
  "role": "gardener",                    // it does the report itself; often no crew dispatch at all
  "project": "gardenio",
  "cron": "11 8 * * 1",                 // weekly (Mon 08:11); or "13 9 * * 1", or daily "23 9 * * *"
  "timezone": "America/Los_Angeles",
  "enabled": true,
  "max_concurrent": 1,
  "cooldown_minutes": 25,               // 25-30 — one heavy pass per fire
  "conditions": null,                   // UNCONDITIONAL — a report runs on the clock regardless of queue
  "prompt": "<report playbook: pull data, compute trends, diff vs last report, land ONE doc, squawk, stop>"
}
```
```jsonc
// gardener block  (minimal — a single-shot reporter needs little loop machinery)
{
  "terminal_goal": "<the standing report mandate, e.g. 'A fresh <kind> roll-up report bead lands each <period>'>",
  "standing": true,                      // it fires forever on the clock — never self-completes
  "stop": {
    "run_end": {},                       // single shot: produce one report, gb stop
    "hard_stop": { "any": [ { "kind": "consecutive_failures", "value": "5" } ]},
    "on_loop_end": "close"
  },
  "progress": "<STANDARD_PROGRESS — landing the report bead IS progress (score 1), even with zero findings>",
  "run_budget": 1,                       // one substantive run per fire
  "max_runs": 1,
  "slack_user_id": "U0OPERATOR1",
  "user_bead_id": "<operator user bead>"
}
```

**Labels:** `role:gardener`, `trigger:schedule`, `perpetual:true`,
`kind:<report-kind>` (`kind:gardener-audit-rollup`, `kind:oss-sanitization-audit`),
`project:<project>`.

**Live examples (3):** ka-0000005781 (weekly gardener-audit fail%/no-op% roll-up,
`11 8 * * 1`, REPORT-ONLY), ka-0000007535 (weekly OSS-sanitization drift,
`13 9 * * 1`, `cooldown 30`), ka-0000000021 (deferred one-shot pour at a fixed
future instant `33 2 2 8 *` — the "scheduled single fire" degenerate case).

**Notes / gotchas.**
- `conditions: null` is deliberate — a report is not gated on a work queue; it
  runs on the clock and lands a "nothing to report" bead when the window is empty
  (never fails silently).
- **Report-only is a discipline the prompt must state in bold** ("never disable,
  edit, or spawn"); the spec can't enforce it. A clean report still scores
  `progress_score 1` — it is the deliverable, not a no-op.
- Find the prior report to diff with:
  `kd list --type=doc -l kind:<report-kind> --json | jq 'sort_by(.created_at)|reverse|.[0]'`.

---

## Choosing between the presets

```text
Is the deliverable a periodic REPORT you diff over time, with zero fleet actions?
  └─ yes → Preset 6 (Cron Roll-up, report-only)

Is the goal SUSTAINED QUALITY of a surface against a named rubric (never "done")?
  └─ yes → Preset 4 (CDQR Continual-Quality)

Are you babysitting ONE specific MR / preview env to green+merged+usable?
  └─ yes → Preset 5 (MR-Green Watcher)

Is the gate an UNBOUNDED label/queue anyone keeps adding to (standing lane)?
  └─ yes → Preset 3 (Ready-Queue Drainer, standing:true)

Is it a BOUNDED epic with a done-definition you want driven to merged code + closed?
  ├─ and it renders on /plans as a plan  → Preset 1 (Plan-Gardener / Orphan-Watchdog)
  └─ otherwise (a plain delivery epic)    → Preset 2 (Epic-Driver)
```

The load-bearing distinctions:
- **Terminal vs standing** — Presets 1/2/5 close when their epic drains
  (`on_loop_end:close`, no `standing`); Presets 3/4/6 run forever
  (`standing:true` / `perpetual:true` label). Getting this wrong either reaps a
  standing gardener as "stuck" (bug ka-0000006241) or leaves a terminal gardener
  spinning after its work is done.
- **Gate shape** — `parent` `bead_query` (bounded epic subtree: Presets 1/2/4/5)
  vs **label/queue** filter (unbounded lane: Preset 3) vs **none** (unconditional
  report: Preset 6).
- **Action** — `spawn_agent` (dispatch crew) vs `apply_formula` (re-pour a rubric
  molecule: Preset 4 sub-shape A).

---

## Appendix — Common building blocks

These blocks are near-identical across the whole live fleet. Author them once and
reference by name.

### `STANDARD_PROGRESS`

Every gardener surveyed uses this exact `progress` block (only the `parent` in the
events changes). It credits three progress signals, over two windows, surfaced in
prime + wrap-up + loop-end squawk.

```jsonc
{
  "events": [
    { "kind": "child_closed", "parent": "<EPIC_ID>" },
    { "kind": "mr_merged",    "parent": "<EPIC_ID>" },
    { "kind": "wrapup_field", "field": "progress_score", "op": ">=", "value": "1" }
  ],
  "windows": [
    { "name": "this_run",    "scope": "since_run_start" },
    { "name": "last_3_runs", "scope": "last_n_runs", "n": 3 }
  ],
  "surface": { "in_prime": true, "in_squawk_on_loop_end": true, "in_wrapup": true }
}
```

Any credited progress event resets `resumes_without_progress` to 0, so a gardener
making headway re-arms indefinitely; only genuine spin is bounded.

### `stop` block anatomy

- **`hard_stop`** — the un-negotiable brakes that close *any* gardener, standing or
  not: `consecutive_failures` (5 typical, 3 for a wedge-prone MR watcher) and
  `wall_clock` (`168h`=7d terminal, `720h`=30d standing).
- **`loop_end`** — the *soft* completion set: `bead_query == 0` on the bound epic
  (the "work is done" signal), plus `max_runs` and `no_progress_for` (3 runs).
  On a gardener whose `terminal_goal` is unmet, `max_runs`/`no_progress_for` **re-arm**
  (refill `run_budget`) up to `resume_budget` fruitless resumes rather than
  closing — that's why quiet gardeners survive.
- **`run_end`** — usually `{}` (empty): per-single-run stop conditions, rarely set.
- **`on_loop_end`** — `close` for every live gardener (terminal) *except* rely on
  `standing:true`/`perpetual:true` to suppress the no-progress give-up for
  standing ones.

### Runtime / engine-owned fields (do NOT author)

`runs_spawned`, `runs_completed`, `consecutive_failures`, `last_agent_id`,
`lineage_root`, `run_log`, `progress_state`, `resumes_without_progress`,
`last_resume_reason`, `runs_finalized_after`, `wall_clock_since`,
`dormant_on_escalation` / `dormant_blocker` / `dormant_blocker_kind`,
`second_life_count`. The engine stamps these; a preset never sets them.

### Operator-binding fields (set once, same for all presets on a fleet)

- `slack_user_id` — the operator's Slack id (e.g. `U0OPERATOR1`), where the
  gardener's card/squawks route.
- `user_bead_id` — the operator's `type=user` bead.
- `slack_channel` (trigger-level, optional) — override the project default
  channel for this gardener's spawned agents (stamped as `schedule_slack_channel`).
- `assign_bead` — the epic/plan the gardener is bound to (Presets 1/2/4/5; absent
  for label-gated Preset 3 and unconditional Preset 6).

### Cadence conventions observed on the fleet

- **`:NN,:NN * * * *`** (twice-hourly, distinct minute offsets per gardener) — the
  standard for terminal epic/plan drivers; offsets are staggered (03/33, 04/34,
  05/35, 07/37, 11/41, 17/47, 20/50, 23/53, 27/57, 28/58, 29/59) so the fleet
  doesn't all fire on the same minute.
- **`NN */6 * * *`** (every 6h) — standing lanes / inspectors that don't need
  tight latency.
- **daily `NN H * * *`** — expensive rubric audits (Presets 4A) and reports.
- **weekly `NN H * * 1`** — Monday roll-ups (Preset 6).
- **`*/15 * * * *`** — an in-flight MR watcher (Preset 5).
- Timezone is `America/Los_Angeles` for operator-facing cadences, `UTC` for
  machine audits.
