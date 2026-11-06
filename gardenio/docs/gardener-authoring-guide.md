# Gardener Authoring Guide (canonical)

> **This is the single front-door for authoring a gardenio gardener.** It folds
> together what used to be ~7 scattered advice beads (self-block mandate,
> default cadence, wake-condition recipes, repo-less rule, prompts-as-advice,
> try-before-you-escalate) into one guide, and points at the deeper references
> for mechanics. If you are creating, reviving, or reviewing a gardener, read
> this first.
>
> **Echoed by the gate.** `gb gardener start` refuses to run without
> `--i-read-the-gardener-instructions` and prints the companion advice bead
> (label `gardener-authoring-guide`, currently ka-0000004752) inline so the
> author loads the rules into context before a gardener is ever created (epic
> ka-0000003900, task ka-0000007415). That advice bead is the one-screen
> checklist; **this doc is the long-form it points to.**

## Contents

- [What a gardener is](#what-a-gardener-is)
- [The one command](#the-one-command)
- [The six rules (non-negotiable)](#the-six-rules-non-negotiable)
- [Anatomy of a gardener bead](#anatomy-of-a-gardener-bead)
- [Wake-condition recipes](#wake-condition-recipes)
- [Authoring checklist](#authoring-checklist)
- [Reviewing / reviving a gardener](#reviewing--reviving-a-gardener)
- [Deeper references](#deeper-references)

## What a gardener is

A **gardener** is a `type=trigger` bead (`trigger:schedule`,
`action=spawn_agent`, `role=gardener`) that fires on a schedule and/or on
bead-graph events and spawns crew to drive an epic's children to closure. Its
loop behaviour — when it wakes, when it stops, how it measures progress — lives
in a nested `gardener` JSON blob on the bead (`fields.gardener`). The trigger
engine decodes that blob (`internal/bridge/triggers.go`); the flat top-level
bead fields are **inert** (see [Rule 3](#rule-3-the-self-block-lives-in-the-blob-not-the-flat-fields)).

There are **two gardener systems in one codebase** and it is easy to conflate
them (full history: `docs/gardener-stop-conditions-review-2026-06-07.md`):

1. **The engine** — a rich declarative `triggers:` / `stop:` / `progress:`
   DSL. It implements `loop_end`/`run_end`/`hard_stop`, `max_runs`,
   `wall_clock`, `bead_query`, `no_progress_for`, MR-aware `bead_event` wakes,
   and progress scoring.
2. **What gardeners historically ran** — a *bare* cron + `conditions`
   (open-children-of-epic `bead_query`) preflight, with the run budget /
   terminal goal / wall-clock folded into the **prompt as prose**.

The gap between the two was the whole reason this hardening epic exists: the
engine was more capable than the authoring surface let anyone use. Author to
the engine, not to the prose.

## The one command

Do **not** hand-roll a gardener bead. Use the endpoint:

```sh
gb gardener start <epic-bead-id> --i-read-the-gardener-instructions
```

It is idempotent (finds and links an existing gardener on the epic instead of
duplicating a loop), pins the epic via a `gardener-epic:<id>` label, and — as of
the hardening epic — emits a **bounded** default blob (`run_budget` + a
`hard_stop` `wall_clock` + a `bead_query` loop_end + `no_progress_for`), so a
default gardener already satisfies [Rule 1](#rule-1-every-gardener-must-self-block)
and [Rule 2](#rule-2-default-to-a-30-minute-cadence). Useful flags:

| Flag | Default | Purpose |
|---|---|---|
| `--max-runs` | 20 | hard run cap (the `run_budget`) |
| `--loop-wall-clock` | 168h | absolute loop backstop (the `hard_stop` ceiling) |
| `--run-wall-clock` | 30m | per-run cap |
| `--terminal-goal` | *"All children of `<epic>` are closed."* | set a **non-trivial** goal (D6 rewards it) |
| `--cron` | staggered `*/30` seeded from the epic id | override cadence |
| `--cooldown` | 5m | minimum interval between firings (keep `< 30m`) |

Full flag list: `gb gardener start --help`.

## The six rules (non-negotiable)

### Rule 1: every gardener MUST self-block

*(folds in advice ka-0000000974 — the load-bearing self-block mandate, cited in
37+ files; and ka-0000002635, the auto-block sentinel playbook.)*

An unbounded gardener is the single biggest cost / runaway-spend footgun in the
fleet. Every gardener carries **two orthogonal safety axes — do not conflate
them** (bug ka-0000000849):

| Axis | What it is | Enforcement |
|---|---|---|
| **backstop** | `run_budget > 0` **and** a `hard_stop` `wall_clock` in `(0, 720h]` — the emergency brake the engine fires unconditionally | `bridge.ValidateGardenerBlob` — **fail-closed at authoring**, warn-with-grace at fire time |
| **self-block** | a *progress-aware* (`no_progress_for` + a `progress` spec) **or** *goal-completion* (`bead_query` / `specific_bead_closed` / `max_runs`) `loop_end` — terminates when forward progress stops | advisory (audit sentinel) |

Neither implies the other, and fixing one does **not** add the other. A fully
safe gardener wants **both** green: the backstop is the guaranteed ceiling; the
self-block is what stops it burning that whole ceiling on a wedged loop. The
default `gb gardener start` blob gives you both.

Author `run_budget` plus **at least one** intelligent `loop_end`:

1. `run_budget` (`max_runs`) — hard run cap. Always set it as the floor of
   safety.
2. A **progress-aware** stop — `{"kind":"no_progress_for","runs":3}` — fires
   when the last N runs all scored zero (crew blocked, nothing closing).
   Requires a `progress` spec so the daemon can score runs. This is the real
   wedge detector; `consecutive_failures` only counts FAILED pods, not BLOCKED
   ones, and the default wall_clock is often weeks.

   `no_progress_for` takes **either** a count window `runs` **or** a time window
   `duration` (`{"kind":"no_progress_for","duration":"90m"}` — the
   `no_progress_for: <minutes>` form). `runs` fires when the last N finalized
   runs all scored zero; `duration` fires when **every** run that closed within
   the last `<duration>` scored zero. Prefer `duration` for a gardener on a slow
   or irregular cadence, where "the last 3 runs" spans too wide a wall-clock
   stretch to be a timely wedge signal; prefer `runs` for a steady cadence. If
   both are set, `runs` wins. Either form binds to the **same** `progress`
   scoring: a run counts as forward progress only when it produced a *credited*
   event — `mr_merged`, `child_closed`, or `decision_resolved`. `crew_dispatched`
   is deliberately **not** credited, so a gardener that keeps dispatching crew but
   merges nothing scores zero every cycle and trips the detector rather than
   self-reporting 10/10 forever (the gardener-delivery wedge, ka-0000005141).
3. A **goal-completion** stop —
   `{"kind":"bead_query","op":"==","value":"0","parent":"{{assign_bead}}","status":["open","in_progress","blocked"]}`
   — closes when the epic is drained.

Verify with the engine's own verdict — never hand-parse fields:

```sh
gb gardener status <bead-id> --json | jq '.has_backstop, .self_block, .self_block_reason'
# true  "ok"   null           → both axes green
# false "ok"   "bounded + …"  → progress-aware but NO engine backstop
# true  "weak" "terminates …" → backstop present but no wedge detector
```

Audit the whole fleet on demand:
`python3 gardenio/scripts/gardener-autoblock-audit.py --include-weak --summary`.

**Kill-switch:** a `gate` bead titled exactly `gardeners-pause` halts all gardener
firing fleet-wide; close it to release.

### Rule 2: default to a 30-minute cadence

*(folds in advice ka-0000000174 — the standing operator cadence rule.)*

Every new gardener / `type=trigger` schedule loop defaults to a **30-minute**
cadence: `cron '*/30 * * * *'` — NOT hourly or multi-hour (3h is far too long
between supervision passes). Set `cooldown_minutes` strictly **less than 30**
(e.g. 20) so a `*/30` cron is never suppressed by its own cooldown.
Self-quiescing `bead_query` preflights keep idle gardeners from spawning needless
work, so the tight cadence is cheap. `gb gardener start` already emits a
deterministic staggered `*/30` (seeded from the epic id, so default-cadence
gardeners don't synchronize into a spawn herd). Existing long-cadence gardeners
may be left as-is unless actively in use.

### Rule 3: the self-block lives in the BLOB, not the flat fields

Bounds and the self-block live **only inside the nested `gardener` JSON blob** —
the same blob `ParseTriggerDef` decodes and `ValidateGardenerBlob` validates at
fire time. The flat top-level bead fields (`fields.run_budget`, `fields.stop`,
`fields.loop_end`, `fields.terminal_goal`) are **inert** — the daemon never
reads them, and they are `null` on every normally-authored gardener.

- **Do not survey bounds by eyeballing flat fields** — you will false-negative
  on every bounded gardener (bug ka-0000004852). Ask `gb gardener status`.
- **Do not edit a live self-block via the flat field** — `kd update <trigger>
  --field terminal_goal=…` writes the field the daemon never reads, so your
  tightened goal silently no-ops (bug ka-0000004562). Edit the blob:

  ```sh
  kd show <trigger> --json | jq -r '.fields.gardener' > /tmp/cap.json
  jq '.terminal_goal = "…new goal…"' /tmp/cap.json > /tmp/cap.new && mv /tmp/cap.new /tmp/cap.json
  kd update <trigger> --field gardener=@/tmp/cap.json
  ```

  (`--field-set gardener.terminal_goal=…` does NOT work — the blob is a
  JSON-encoded *string*, not a map.) `gb gardener status` emits a `⚠️ DIVERGENCE
  <field>` banner whenever a flat self-block field disagrees with the blob.

### Rule 4: repo-less gardeners must not point crew at git paths

*(folds in advice ka-0000006996.)*

A gardener whose trigger `project` **field** is empty spawns a pod with **no
working tree** (`buildInitCloneContainer` clones nothing). A prompt that then
names a git path (e.g. `gardenio/docs/gardener-first-mate-prompt.md`) points at a
phantom: the agent Reads it, gets "file does not exist", and burns reads
recovering (the live first-mate 404, bug ka-0000004796). So if the trigger's
`project` field is empty AND it spawns an agent:

- **INLINE** the playbook where a repo-less pod can read it — an advice body
  (`gb advice show <id>`) and/or a `gb bundle`; OR
- give the trigger a **real `project`** so the pod clones that repo and paths
  resolve; OR
- if you must name a path only to forbid it, say so explicitly ("you run
  repo-less — do NOT open `gardenio/docs/*.md`, those 404 at runtime").

The `project:<name>` **label** is orthogonal to the `project` **field** — the
FIELD drives the clone. A warn-only guard flags this
(`bridge/repoless_gitpath_guard.go`); it never rejects.

### Rule 5: crew/gardener prompts live as ADVICE, not git files

*(folds in advice ka-0000002990.)*

Gardener and crew **prompt content** lives as `type=advice` beads, not as files
committed under `gardenio/plans/`. Advice beads are queryable, attachable to
context via `gb advice show <id>`, updatable without git churn, and visible in
beads-viewer. Scope a single gardener's playbook with the `agent:<that-gardener-
bead-id>` label (NOT `role:gardener`, which injects it into every gardener; NOT a
bare tag, which targets nothing) and reference it explicitly from the prompt.
This dovetails with Rule 4 — a repo-less gardener's playbook *must* be an advice
body a working-tree-less pod can read.

> **Operator note (ka-0000003852, historical/tree-hugger v2):** for
> human-in-the-loop gardener families, the highest-leverage checkpoint is the
> *birth* of a new gardener — file a `gb decision create` presenting the gardener
> details (target epic, subgraph, crew shapes, advice, trigger config) and
> `gb yield` for operator approval before the gardener bead is created. This does
> NOT apply to a gardener's per-cycle work; per-cycle judgment is the gardener's
> job and is not operator-gated.

### Rule 6: you own the decisions your lane files

*(folds in advice ka-0000007999 — the standing "gardener owns crew decision
resolution" authority; learned from the gb-news open-decision-wall queue,
decision ka-0000003001, option `gardeners-resolve`.)*

A gardener **owns** — and is expected to **answer** — the open `type=decision`
beads its own lane files. Standing authority is already granted: when your crew
files a `gb decision create` + `gb yield`, resolve it directly from your
knowledge of the mission, MR state, and operator preferences. Do **not** relay
every crew fork up to the human operator. Only a genuinely **cross-cutting** or
**privilege-gated** fork (touching human-authored branches, ambiguous
requirements, a policy question, or an action only the operator can perform)
stays operator-gated — and then you file *your own* decision, you don't just
pass the crew's along.

The failure mode this rule exists to prevent: the fleet files genuine forks
faster than one human answers them, nothing bounds the aggregate, and the
operator's `gb news` open-decision section becomes an un-drainable wall (73 open
gardenio decisions, 64% older than a week, when this rule was written). Every
decision a lane leaves un-owned is a row on that wall.

Two operator hard rules govern what may reach the operator at all — both
learned from that same queue:

**(a) NOT-A-DECISION: standing policy already covers it → it must not reach the
operator.** A fleet-internal item that existing policy or advice already answers
is not a decision. **The test: if you cannot name what materially CHANGES
between the options, it is not a decision** — `gb squawk` the status and stop.
Do not manufacture a fork (ack/done/continue, or two options that lead to the
same next action) just to have something to file; the server-side guard refuses
zero-option and single-ack decisions for exactly this reason.

**(b) FIX THE GENERATOR, NOT THE INSTANCE.** When a stale or redundant decision
(or bead, or alert) keeps reappearing, it was almost always emitted by a
*generator* — a gardener loop, a schedule, a cross-post rule. Sweeping the
instances is throughput you will repay every cycle; the fix is to edit the
**generator**. In the 2026-08 batch, three generators each filed a fresh row on
a schedule (a weekday standup cross-post, a nightly directive-count watch, and
two gardeners carrying contradictory needs-intake policies). All three were
retired by editing the generator — the cron, the cross-post rule, the gardener
blob — not by cancelling the rows they had already produced.

## Anatomy of a gardener bead

The rich shape the engine decodes. `gb gardener start` emits a bounded subset of
this by default; hand-authored gardeners use the full form (worked example:
`gardener-loop-runbook.md` §5.1):

```yaml
type: trigger
title: "Gardener for kd-… (<goal>)"
fields:
  enabled: "true"
  role: gardener
  project: gardenio            # drives the clone — empty = repo-less (Rule 4)
  assign_bead: kd-…           # the epic this gardener drives
  prompt: "<gardener playbook — or a gb advice show <id> pointer (Rule 5)>"
  gardener: |                  # the NESTED BLOB — the only thing the daemon reads (Rule 3)
    {
      "run_budget": 20,       # Rule 1: the backstop floor
      "terminal_goal": "All children of kd-… are closed.",
      "triggers": [ … ],      # when to wake — see recipes below
      "stop": {
        "loop_end": {"any": [
          {"kind":"bead_query","op":"==","value":"0","parent":"{{assign_bead}}",
           "status":["open","in_progress","blocked"]},   # goal-completion self-block
          {"kind":"max_runs"},
          {"kind":"no_progress_for","runs":3}             # wedge-detector self-block
        ]},
        "hard_stop": {"any": [
          {"kind":"wall_clock","duration":"168h"},        # Rule 1: the backstop ceiling
          {"kind":"consecutive_failures","value":5}
        ]}
      },
      "progress": {
        "events": [ {"kind":"child_closed","parent":"{{assign_bead}}"}, … ],
        "surface": {"in_prime":true,"in_wrapup":true}     # gb stop enforces progress_score
      }
    }
```

Template variables (`{{assign_bead}}`, `{{schedule_id}}`) and the full leaf DSL
(operators, `in_subtree_of`, etc.) are in `gardener-loop-runbook.md` §4.

## Wake-condition recipes

*(folds in advice ka-0000001467 and ka-0000005707 — now retired in favour of
this section.)*

Turn a cron-only gardener into an opportunistic, event-driven one.

**THE ONE RULE THAT BITES EVERYONE:** a `bead_event` / `decision_resolved`
`match` is evaluated **only against the event payload** (`leaf_field.go`), never
the live bead. A field leaf on a key the payload does not carry resolves to "not
present" and the entry **silently never fires**. Two ingestion paths expose
DIFFERENT shapes — never mix their keys in one `all:` block:

- **SSE full-payload** (created/updated/closed + hydrated edge events): `type`,
  `status`, `assignee`, `created_by`, `priority`, custom fields (`mr_state`,
  `mr_pipeline_status`, `parent`, …), and labels. Match here for
  type/parent/MR-field/label.
- **NATS close/decision listeners**: only `watch_bead_id` / `watch_type` /
  `watch_project` / `watch_role` + `chosen_label` / `chosen_index` /
  `resolved_by`. Match here for decision answers and role/project scoping.

Daemon-backed ops (`in_subtree_of`, `bead_query` / `ready_queue` / gate
preflights) work regardless of shape.

Building blocks (full JSON + gotchas: `gardener-loop-runbook.md` §8,
`triggers.md`):

- **Child-closed wake** — `bead_event` on `parent == {{assign_bead}}` (SSE
  path), `throttle:"60s"`.
- **MR-green wake** — `bead_event` on `type == merge-request` AND
  `mr_pipeline_status == success`, scoped to your MRs with a label/author leaf.
  Near-real-time via the pipeline webhook (§8.2). This is the canonical
  delivery-gardener need and is now a one-flag default (`--wake-on-mr`, task
  ka-0000005598).
- **Escalation / decision-resolved wake** — match `watch_project` /
  `watch_role` / `watch_bead_id` + `chosen_label` (NATS path). Note the payload
  does NOT carry `raised_by_agent.task_id` — scope via `watch_bead_id` +
  `in_subtree_of`.
- **The recommended HYBRID default** (§8.6) — pair a fast `bead_event` entry
  with a `cron` entry carrying a `bead_query` preflight as a slow catch-up floor
  (skipped when there's no work). Keep the floor unless you only care about one
  bead-id closing.
- **PLAN gardeners are born in event mode** (gd-gsvlS6YDj3) — `plangardener.Bind`
  (Approve & build, the cockpit's *Enable gardener*, `gb plan gardener`) authors
  `trigger:multi` with the plan-activity `bead_event` entry plus a **daily**
  fallback watchdog staggered from the plan id. `gb plan gardener --timed` (or
  `--cron`) binds the old timed shape instead.
- **Flipping a PLAN gardener by hand is the exception** — `gb gardener mode
  <plan-epic> --event` on a plan gardener (`plan-gardener` marker) gets **no
  default fallback cron**; the `/plans/{id}` *Firing mode* + *Cadence* editor
  does the same. Add a watchdog only with an explicit `--fallback-cron '<expr>'`
  (or by filling the cockpit cadence field).
  When a fallback IS present, the plan's open-children pre-flight gate is carried
  onto it as a per-entry `preflight`, so the watchdog never fires an idle
  iteration after every step closes. Rationale + operator ask: `ka-0000008237`.
- **Retuning an EXISTING gardener's cadence** — use `gb gardener cadence <epic>`,
  not `gb gardener mode`. It is mode-aware: in timed mode `--cron` sets the flat
  firing schedule (it cannot be cleared — a timed gardener with no cron never
  fires); in event mode `--cron` installs/replaces the fallback watchdog and
  `--none` removes it entirely, leaving the gardener purely event-driven. With no
  flag it prints the current mode + effective cadence. **Gotcha:** `gb gardener
  mode --event --no-fallback-cron` is a **no-op** on a gardener that is *already*
  in event mode (the flip early-returns before the flag is read), so it cannot
  strip a legacy watchdog off a gardener flipped before plan gardeners stopped
  getting a default one — `gb gardener cadence <epic> --none` is the verb that
  can (`ka-0000005992`).
- **`bead_query` loop_end** — self-terminate when the subtree empties (a closed
  root epic short-circuits the count to 0).

## Authoring checklist

Before you create or revive a gardener, confirm every line:

- [ ] Used `gb gardener start <epic> --i-read-the-gardener-instructions` (not a
      hand-rolled bead).
- [ ] **Backstop:** `run_budget > 0` AND a `hard_stop` `wall_clock` in
      `(0, 720h]` — `gb gardener status … | jq '.has_backstop'` is `true`.
- [ ] **Self-block:** a `no_progress_for` (+ `progress` spec) OR a goal-completion
      `loop_end` — `.self_block` is `ok`, not `weak`/`none`.
- [ ] **Cadence** is `*/30` with `cooldown < 30m`.
- [ ] **Non-trivial `terminal_goal`** (not the bare default) so D6 rewards it.
- [ ] All bounds/goal edits went into the **blob**, not flat fields; `gb gardener
      status` shows **no** `⚠️ DIVERGENCE`.
- [ ] If **repo-less** (empty `project` field): prompt names **no** git paths.
- [ ] Gardener/crew **prompts are advice beads** (`agent:<id>`-scoped), not
      `gardenio/plans/*.md` files.
- [ ] Wake `match` leaves use keys from **one** ingestion shape (SSE OR NATS),
      not both.
- [ ] Lane answers its own decisions; only cross-cutting or privilege-gated
      forks are operator-gated.

## Reviewing / reviving a gardener

- **Try before you escalate** *(folds in advice ka-0000002866).* A
  trigger-spawned gardener's prompt is a floor, not a ceiling. Before yielding
  because a child is "blocked on operator escalation", check whether the blocker
  is something you can fix yourself: small YAML/config edits in `.rwx/`,
  `gardenio/helm/`, `deploy-values/`; rebasing/reviving conflicting MRs (never
  retrigger, rerun or refresh PR CI: owner rule 2026-10-31, root CLAUDE.md "CI on
  pull requests");
  self-merging your crew's green MRs (no operator approval needed when CI is
  green and the diff is sound); cleaning up duplicate decisions; terraform/AWS
  work where you have IAM. Diagnose each cycle: read `progress_state.runs[-3:]`
  — three consecutive `progress_score == 0` runs means you're wedged; investigate
  the blocker yourself. Only file **one** `gb escalation create` when you
  genuinely cannot fix it — not a decision asking permission.
- **Verify it's actually firing** with a field that increments on success (`gb
  gardener status → spawned=N`), never a plausible-looking liveness field. Test
  any liveness field against a KNOWN-GOOD control first (advice ka-0000002586).
- **Confirm a merge is still needed** before (re)filing an escalation/decision on
  a review-and-merge gardener — re-check the live MR, not memory (advice
  ka-0000002429).
- **Grade** against the effectiveness rubric D6 (loop hygiene):
  `gardener-effectiveness-rubric.md`.

## Deeper references

| Concern | Where |
|---|---|
| Whole-system map (lifecycle, wake, stop/resume, meta layer) + doc index | `docs/gardener-system-reference.md` |
| Loop DSL: `triggers:` / `stop:` / `progress:`, worked examples, debugging | `docs/gardener-loop-runbook.md` |
| Agent one-table field reference | `docs/agent-triggers-guide.md` |
| Operator-facing trigger schema / sources / actions | `docs/triggers.md` |
| Effectiveness rubric (D6 loop hygiene) | `docs/gardener-effectiveness-rubric.md` |
| Why this epic exists (two-systems review) | `docs/gardener-stop-conditions-review-2026-06-07.md` |
| Advice targeting + hygiene contract | `docs/advice-hygiene.md` |
| Fleet auto-block audit | `gardenio/scripts/gardener-autoblock-audit.py` |

**Source map** — `gb gardener start` (`cmd/gb/gardener.go`); blob parse +
`GardenerSpec` (`internal/bridge/triggers.go`); stop evaluator
(`internal/scheduler/gardener_stop.go`); bounds guard
(`bridge.ValidateGardenerBlob`); fire-time check
(`internal/bridge/gardener_evaluator.go`).
