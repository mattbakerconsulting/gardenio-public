# Gardener Config Builder — Schema Spec & Guided-Builder Design

_Task R0.2 (ka-0000001914) + decision ka-0000004391 ("guided") under epic
**ka-0000001611** — Gardener Configuration Surface. This doc is the authoritative
reverse-engineered schema a viewer builder UI must cover, plus the concrete
guided-builder design that implementation task **I2.3 (ka-0000000897)** consumes._

**Operator:** Matt Baker (oidc:matthew.baker@acme.example)
**Sources of truth (engine, do not re-derive — cite these):**
- `gardenio/controller/internal/scheduler/conditions/expression.go` — the
  `Expression` / `Leaf` DSL (wake `conditions[]`, trigger `match`/`preflight`).
- `gardenio/controller/internal/scheduler/conditions/leaf_daemon.go` — daemon
  leaf evaluators (`bead_query`, `gate`, `ready_queue`, `active_agents`,
  `acceptance_metric`).
- `gardenio/controller/internal/scheduler/conditions/leaf_field.go` +
  `eval.go` (`CompareCount`) — field-leaf ops + count comparators.
- `gardenio/controller/internal/scheduler/stop.go` — `StopSpec` (loop_end /
  run_end / hard_stop slots, all stop kinds).
- `gardenio/controller/internal/scheduler/gardener_stop.go` —
  `EvaluateGardenerStopSpec` (which kinds actually fire, and how).
- `gardenio/controller/internal/scheduler/progress/spec.go` — `progress` spec
  (events / windows / surface).
- `gardenio/controller/internal/scheduler/triggers.go` — `TriggerEntry`
  (the `triggers[]` wake-source array: cron / bead_event / decision_resolved /
  manual, each with `match` / `preflight` / `throttle` / `debounce`).
- `gardenio/controller/internal/bridge/triggers.go` — `GardenerSpec` (the
  `gardener` JSON field) + `TriggerDef` (trigger-level knobs).

> **Coordination note (sibling epic ka-0000007837 — scriptable triggers).**
> This epic CONSUMES the conditions primitive; it does not redefine it. The
> builder targets the `conditions.Expression` DSL as it exists today. If
> ka-0000007837 lands a CEL/Starlark expression leaf, the guided builder gains
> a new leaf *kind* (an "expression" row) — the row-based IA below already has
> the seam for it (§4, "raw escape hatch per row"). No structural rework needed.

---

## 1. The three authorable JSON surfaces

A gardener is a `type=trigger` bead with `role:gardener`. Its behaviour is governed
by config that lives in **three distinct places** on the bead. A builder must
cover all three; they have different shapes and different homes:

| Surface | Bead home | Go type | What it controls |
|---|---|---|---|
| **Wake** | `conditions` field (JSON array) **and/or** `triggers` field (JSON array of `TriggerEntry`) | `[]conditions.Expression` / `[]TriggerEntry` | *When the gardener wakes* — pre-flight gates + event/cron sources |
| **Stop** | `gardener` field → `.stop` (JSON object) | `scheduler.StopSpec` | *When the gardener stops* — loop_end / run_end / hard_stop |
| **Progress** | `gardener` field → `.progress` (JSON object) | `progress.Spec` | *Whether the gardener is making headway* — feeds `no_progress_for` stop + prime/wrap-up surfaces |

Plus the scalar GardenerSpec knobs (`run_budget`, `max_runs`, `one_spawn_at_a_time`,
…) which the edit form already covers — see §6 for the coverage delta.

> **Important shape distinction.** `conditions[]` (the bare pre-flight gate that
> `gb gardener start` writes, surfaced as `TriggerDef.HasConditions`) is a flat
> array of `Expression`s ANDed together. The richer `triggers[]` array is the
> multi-source fan-in (each entry is a `TriggerEntry` with its own `match` /
> `preflight` `Expression`s). The guided builder's **condition-row component is
> the same** in both places — it authors one `Expression` — but the array it
> writes into differs. §4 specifies the shared row component; §5 specifies how
> rows compose into each home.

---

## 2. The conditions DSL — `conditions.Expression` (authoritative)

Every wake gate, every `match`, every `preflight` is one `Expression`. An
`Expression` is **either** a boolean composition **or** a leaf.

### 2.1 Composition nodes

```jsonc
{"all": [<expr>, <expr>, …]}   // boolean AND — all children must match
{"any": [<expr>, <expr>, …]}   // boolean OR  — any child matches
{"not": <expr>}                // boolean NOT
```

Exactly one of `all` / `any` / `not` is set on a composition node. Nesting is
unbounded (`all` of `any` of leaves, etc.).

### 2.2 Leaf nodes — two families

A leaf is a **daemon leaf** (has `type`, queries the bead store) or a **field
leaf** (has `field`, matches against an in-process event/bead payload). A leaf
with neither is a no-op that evaluates `true` ("no constraint").

#### 2.2.1 Daemon leaves (`type` set) — `leaf_daemon.go`

| `type` | Queries | `op` (comparator) | `value` | Extra filters | Fires when |
|---|---|---|---|---|---|
| `bead_query` | count of beads matching filters | `> >= < <= == eq != ne` | integer threshold | `project`, `types[]`, `statuses[]` (default `["open"]`), `labels[]`, `parent` | `CompareCount(count, op, value)` |
| `ready_queue` | count of open `issue` kind beads (project-scoped) | same count ops | integer | `project` | open ready-queue size `op` value |
| `active_agents` | count of `type=agent` beads in `open`/`in_progress` | same count ops | integer | `project`, `role` | live-agent count `op` value |
| `gate` | presence of an open/in_progress `type=gate` bead by title | `present` \| `absent` (default `absent`) | gate **title** | — | gate present/absent as asked |
| `acceptance_metric` | acceptance-metric leaf (see `leaf_acceptance_metric.go`) | metric-specific | metric-specific | metric-specific | metric threshold met |

**Count comparator set (`CompareCount`, `eval.go`):** `>`, `>=`, `<`, `<=`,
`==`/`eq`, `!=`/`ne`. Any other op → `false`. Non-numeric `value` → `false`.

**`bead_query` closed-root short-circuit (ka-0000008051):** when `parent` is set
and that parent bead is itself `closed`, the subtree count is forced to `0`
(the gardener auto-dormants even if stray open descendants remain). The builder
should surface this as a tooltip on the `parent` field, not as a separate knob.

#### 2.2.2 Field leaves (`field` set) — `leaf_field.go`

Field leaves match the **event payload** (for `match`) or bead fields. The
`field` path syntax:
- `parent` — top-level payload key.
- `raised_by_agent.task_id` — dotted nested `map[string]any` walk.
- `label:project:gardenio` — membership test against `payload["labels"]`.

| `op` | Semantics | Uses |
|---|---|---|
| `==` | string-equal `value` | `value` |
| `!=` | not string-equal `value` | `value` |
| `in` | field value ∈ `values[]` | `values[]` |
| `not_in` | field value ∉ `values[]` | `values[]` |
| `contains` | `value` is a substring of field value | `value` |
| `present` | field path resolves | — |
| `absent` | field path does NOT resolve | — |
| `in_subtree_of` | field value is a descendant of bead-id `value` via parent-child deps (`IsAncestor`) | `value` |

A non-present-style op over a missing field → `false` (the trigger does not fire).

---

## 3. The stop spec — `scheduler.StopSpec` (authoritative)

Lives at `gardener.stop`. Three slots, each `{any: [<entry>, …]}` (no `all:` for
stop — that's a wedge waiting to happen). **Precedence: hard_stop > loop_end >
run_end.**

```jsonc
{
  "loop_end":  {"any": [<entry>, …]},   // close the gardener (no more runs)
  "run_end":   {"any": [<entry>, …]},   // end current run, debit a run, loop continues
  "hard_stop": {"any": [<entry>, …]},   // kill running pod AND close gardener
  "on_loop_end": "close"                 // close (default) | notify_slack | spawn_followup
}
```

### 3.1 Stop entry kinds — `stop.go` constants + `gardener_stop.go` evaluation

Every entry is a `StopEntry{kind, …}`. The populated fields depend on `kind`:

| `kind` | Fires when | Fields | Evaluated against |
|---|---|---|---|
| `max_runs` | `RunsSpawned >= RunBudget` | (none — reads counters) | gardener counters (also enforced as a pre-fire gate via `run_budget`) |
| `wall_clock` | elapsed time ≥ `duration` | `duration` (Go dur, e.g. `"48h"`), optional `when:"yielded"` | gardener creation time (plumbed via GardenerStopContext) |
| `bead_query` | a `bead_query` predicate is true | `op`, `value`, `project`, `types[]`, `statuses[]`, `labels[]`, `parent` | bead store (same shape as the daemon `bead_query` leaf) |
| `specific_bead_closed` | the named bead is closed | `bead_id` | bead store |
| `gate_present` | a gate with title `value` exists | `value` (gate title) | bead store |
| `gate_absent` | no gate with title `value` exists | `value` (gate title) | bead store |
| `no_progress_for` | last `runs` runs scored 0 progress | `runs` (int window) | `progress` RunRecords (REQUIRES a `progress` spec configured) |
| `consecutive_failures` | `ConsecutiveFailures >= value` | `value` (int) | gardener counters |
| `manual` | the named bead field is present | `reason_field` | gardener bead field |
| `manual_force` | the named label is present | `label` | gardener bead label |
| `yielded_then_woken_then_idle` | run yielded, was woken, went idle w/o progress | (none) | run-state machine — **scheduler-only** |
| `no_actionable_state` | nothing actionable for the gardener to do | (none) | run-state machine — **scheduler-only** |

> **Builder caveat (`gardener_stop.go:200`):** on `type=trigger` gardeners (the
> canonical shape this surface authors), `no_actionable_state` and
> `yielded_then_woken_then_idle` are **fail-open no-ops** — they depend on
> run-state machinery the bridge doesn't have yet. The builder should mark these
> two kinds "scheduler-only — no effect on trigger gardeners" in the selector so
> operators don't author a stop that silently never fires.

**`value` is tolerant** (`StopEntry.UnmarshalJSON`): accepts a JSON number OR
string for `value` and canonicalizes to string. The builder should always emit
a string, but must parse both when loading an existing spec.

**Advice constraint (ka-0000000974):** every gardener MUST declare `run_budget`
**and** a progress-aware or `bead_query` `loop_end` so it terminates when it
stops making progress — not just on a multi-week `wall_clock`. The guided
builder should **default-seed** a `loop_end` slot and warn (non-blocking) if the
operator saves a gardener with no intelligent auto-block. See §7 validation.

---

## 4. Guided-builder design — the shared condition-row component

Decision **ka-0000004391 = "guided"**: form rows per condition type, lowest
authoring friction. The atom of the whole builder is **one condition row** that
authors a single `Expression` leaf. This component is reused by the wake-gate
list, the per-source `match`/`preflight` editors, and the `bead_query` stop
entry (which shares the exact filter shape).

### 4.1 Row anatomy

A row is: **[kind selector] → [kind-specific fields] → [live preview] → [✕]**

1. **Kind selector** (`<select>`): the first choice. Options grouped:
   - _Bead-store gates_ → `bead_query`, `ready_queue`, `active_agents`, `gate`,
     `acceptance_metric`
   - _Event-payload match_ → field leaf (`field`/`op`/`value`)
   - _(future)_ `expression` (raw CEL/Starlark, gated on ka-0000007837)
   - _Composition_ → `all` / `any` / `not` (renders a nested row group)

2. **Kind-specific fields** swap in based on the selector. Each field is a
   labelled control, NOT raw JSON:

   | Kind | Controls rendered |
   |---|---|
   | `bead_query` | op `<select>` (`>`,`>=`,`<`,`<=`,`==`,`!=`); value `<input type=number>`; collapsible "filters": project `<input>`, types multi-chip, statuses multi-chip (default `open`), labels chip input, parent bead-picker |
   | `ready_queue` | op `<select>`; value number; project `<input>` |
   | `active_agents` | op `<select>`; value number; project `<input>`; role `<input>` |
   | `gate` | present/absent toggle; gate-title `<input>` |
   | field leaf | field-path `<input>` (with `label:` / dotted hints); op `<select>` (`==`,`!=`,`in`,`not_in`,`contains`,`present`,`absent`,`in_subtree_of`); value `<input>` OR values chip-input (for `in`/`not_in`); present/absent hide the value control |

3. **Live preview** (read-only): renders the row's `Expression` as a plain-English
   clause — e.g. _"open tasks under ka-X > 0"_, _"no gate titled 'scheduler-pause'"_,
   _"payload.label includes project:gardenio"_. This is the "legible" half the
   epic wants (ka-0000005782 reuses the same renderer).

4. **Per-row raw escape hatch** (collapsed `<details>`): shows the row's emitted
   JSON, editable. This is the seam that keeps "guided" from being a cage —
   power users (and the future `expression` leaf) drop to raw for one row without
   abandoning the form. Edits round-trip back through the parser; an invalid
   blob disables save with an inline error scoped to that row.

### 4.2 Why guided (decision rationale, for the record)

The "guided" choice trades impl cost for authoring friction. The cost is
bounded because **there is one row component**, parameterized by a kind→fields
table (§4.1) that mirrors the Go `Leaf` struct field-for-field. The validated-JSON
alternative (option `json`) is cheaper to ship but reproduces today's "operator
hand-authors engine JSON" friction that the epic exists to remove. Guided +
per-row raw escape hatch (§4.1.4) captures most of JSON's flexibility without
the floor of "you must know the schema to do anything."

---

## 5. How rows compose into each home

### 5.1 Wake conditions (`conditions[]` — flat AND gate)

A vertical list of condition rows, implicitly ANDed (matches engine
`EvaluateConditions` = all-must-pass). "+ Add condition" appends a row.
Emits a JSON array of `Expression`. This is the `conditions` bead field that
sets `TriggerDef.HasConditions`.

### 5.2 Wake sources (`triggers[]` — fan-in array of `TriggerEntry`)

A list of **source cards**, each one `TriggerEntry`:
- **Kind** `<select>`: `cron` | `bead_event` | `decision_resolved` | `manual`.
- `cron` → cron `<input>` + timezone `<select>`.
- `bead_event` → events multi-chip (`created`/`updated`/`closed`/`mail_sent`).
- `decision_resolved` → (no extra source fields).
- Each card has two **optional** nested condition-row editors:
  - **`preflight`** (world-state gate at fire time) — one `Expression`.
  - **`match`** (event-payload predicate) — one `Expression`.
- Plus `throttle` / `debounce` (Go-duration `<input>`s) and `label`.

The card → `TriggerEntry` mapping is 1:1 with the Go struct. `match`/`preflight`
reuse the §4 row component (a single `Expression`, so wrap multi-clause in an
`all` composition row).

### 5.3 Stop spec (`gardener.stop` — three slots)

Three labelled sections (loop_end / run_end / hard_stop), each a list of
**stop-entry rows**. The stop-entry row is a sibling of the condition row but
keyed off the **stop `kind`** table (§3.1) instead of the leaf-`type` table:

- Kind `<select>` (the 12 kinds), kind-specific fields swap in.
- `bead_query` stop entries reuse the §4.1 `bead_query` filter sub-form verbatim.
- `no_progress_for` shows an inline warning if no `progress` spec is configured
  (it silently never fires without one — see `gardenerAgentTracker` finalize-run).
- `on_loop_end` `<select>` (`close` / `notify_slack` / `spawn_followup`) sits
  above the three slots.

### 5.4 Progress spec (`gardener.progress`)

Lower priority for I2.3 (it's the input to `no_progress_for`, not a wake/stop
lever itself). Shape for completeness (`progress/spec.go`):
- **events[]**: rows of `EventMatcher{kind, …}` where kind ∈
  `child_closed` | `decision_resolved` | `mr_merged` | `crew_dispatched` |
  `wrapup_field`. Filter fields: `parent`, `raised_by_agent_subtree_of`,
  `author_label`, `role`, and `field`/`op`/`value` (for `wrapup_field`).
  Templates `{{assign_bead}}` / `{{schedule_id}}` are resolved at eval time.
- **windows[]**: `Window{name, scope, n?, duration?}` where scope ∈
  `since_run_start` | `last_n_runs` (requires `n>0`) | `since` (requires
  `duration`). Name is required.
- **surface**: three checkboxes — `in_prime`, `in_squawk_on_loop_end`, `in_wrapup`.

A "Progress (advanced)" disclosure section covers this; I2.3 may stub it as the
existing raw-JSON textarea and let a follow-up (I2.2/I2.3 sub-task) build the
guided rows, since `no_progress_for` is the only consumer.

---

## 6. Coverage delta — what the builder must add vs today

From `create_gardener.go` and `gardener_ops.go` (`gardenerConfigView`):

| Knob | Home | Create form today | Edit form today | Builder target |
|---|---|---|---|---|
| `conditions[]` (wake gates) | `conditions` field | ✗ none | ✗ none | **§5.1 guided rows** |
| `triggers[]` (wake sources) | `triggers` field | ✗ none | ✗ none | **§5.2 source cards** |
| `cron` | trigger field | ✗ | ✗ | source card (`cron` kind) |
| `cooldown_minutes` | trigger field | ✗ | ✗ | scalar `<input>` |
| `max_concurrent` | trigger field | ✗ | ✗ | scalar `<input>` |
| `stop` | `gardener.stop` | ✗ | **raw JSON textarea** | **§5.3 guided slots** |
| `progress` | `gardener.progress` | ✗ | ✗ (preserved, not editable) | §5.4 (disclosure / stub) |
| `terminal_goal`, `exit_condition`, `on_exit` | `gardener.*` | ✗ | ✓ text | keep |
| `run_budget`, `max_runs`, `max_retries` | `gardener.*` | ✗ | ✓ number | keep |
| `retry_on_failure`, `one_spawn_at_a_time` | `gardener.*` | ✗ | ✓ check | keep |
| `run_timeout`, `timeout` | `gardener.*` | ✗ | ✓ text | keep |

The edit form's existing `stop_json` textarea (`gardener_ops.go:506`,
`gardener_config.html`) is the thing §5.3 replaces. The save handler already
preserves engine-owned runtime fields by overlay (`handleGardenerConfigSave`) —
the builder must keep that overlay discipline: author only the operator knobs,
never marshal over `RunsSpawned` / `ProgressState` / `RunLog` / `LastAgentID`.

---

## 7. Validation (mirrors engine, so the UI never persists a spec the engine chokes on)

This is task I2.5's contract; the builder consumes it. Per-row, client-checkable:

- **count ops**: value must be an integer; op ∈ the `CompareCount` set.
- **field-leaf ops**: `present`/`absent` take no value; `in`/`not_in` require a
  non-empty `values[]`; others require `value`.
- **gate**: title required for `present`/`absent` to be meaningful.
- **stop slots**: `{any:[…]}` only — never emit `all` in a stop slot.
- **wall_clock duration / throttle / debounce / window duration**: must parse as
  a Go duration (`time.ParseDuration`).
- **window**: `name` required; `last_n_runs` ⇒ `n>0`; `since` ⇒ valid `duration`.
- **no_progress_for**: warn (non-blocking) if no `progress` spec configured.
- **gardener-shape advice (ka-0000000974)**: warn (non-blocking) if saving with
  `run_budget==0` AND no `loop_end` `no_progress_for`/`bead_query` entry.

Round-trip is the test surface (I2.5): parse golden GardenerSpecs → render into
the guided builder's view model → re-emit → assert semantic-equal JSON.

---

## 8. Open questions / hand-off to D1.1 + I2.3

1. **`conditions[]` vs `triggers[]` precedence in the IA.** Live gardeners use
   both shapes; the survey (R0.3, ka-0000001939) should confirm which is the
   common path so the create form leads with the right one. Recommendation:
   lead with `triggers[]` source cards (richer, the forward path), keep
   `conditions[]` as a "simple pre-flight gate" affordance for the bare shape.
2. **Composition depth in the UI.** Unbounded nesting is legal but rare. Cap the
   guided nesting at 2 levels (`all`/`any` of leaves) and push deeper trees to
   the per-row raw escape hatch.
3. **Progress builder scope for I2.3.** Recommend stubbing §5.4 as the current
   raw textarea and filing a follow-up; `no_progress_for` is the only consumer
   and the rows are the highest-cost/lowest-traffic part of the surface.

This doc is the schema contract for I2.3 (ka-0000000897). The IA decisions in §8
belong to D1.1 (ka-0000002591); the legible-render renderer in §4.1.3 is shared
with D1.2 (ka-0000006103) / I2.4 (ka-0000005782).
