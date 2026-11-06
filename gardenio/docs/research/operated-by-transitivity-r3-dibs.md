# R3 — "Dibs" rule: last modifier becomes operator

Bead: ka-0000006715
Parent epic: ka-0000003404 (Research: operated_by transitivity, dibs, audit)
Status: research only — no implementation, no test code in scope.

## 1. Problem statement

Today `beads.operated_by` is **sticky**: it is set on insert (P3 server resolver, migration 012) and never updated. Subsequent mutations write `mutation_history.operated_by` per-row but leave the bead's headline operator untouched. That is at odds with Matt's stated rule:

> "we must ensure we use the 'dibs' method where the last modifier of an object becomes its operator".

The viewer chip, `kd list -f operated_by=...`, and the dashboard composite index (`idx_beads_operated_by_fleet_status`) all consume `beads.operated_by`. Stickiness means a bead Matt last touched five minutes ago still shows the agent that created it last week.

This doc proposes a precise dibs rule, decides each open question raised in the epic, and renders the answers as a decision table.

## 2. Inputs to the rule

Each mutation arriving at the server carries:

| Input            | Source                                                                    | Possible values                                                                       |
|------------------|---------------------------------------------------------------------------|---------------------------------------------------------------------------------------|
| `incoming`       | `X-Operated-By` header (resolved by server `operatedByFromContext`)       | `slack:U…` · `oidc:…@…` · `system:scheduler` · `fleet:gardenio` · `""` (unresolved)   |
| `current`        | `beads.operated_by` row value                                             | same lattice as `incoming`                                                            |
| `mutation_type`  | router knob: `update` / `comment` / `dep` / `status` / `field-set` / etc. | enum                                                                                  |
| `target_kind`    | what the mutation touches: bead row, comment row, dep row, event row      | enum                                                                                  |
| `field`          | for `update`/`field-set`: which column changed                            | string                                                                                |
| `now`            | server clock                                                              | timestamptz                                                                           |

The rule is **pure** — given those inputs and the current `beads.operated_by` plus its `operated_by_set_at` (proposed new column, see §6), the new value is determined.

## 3. Operator class lattice

We classify every `operated_by` value into one of three **classes** by prefix. The dibs rule reads classes, not raw strings.

| Class       | Members                                                  | Meaning                                  | Takes dibs? |
|-------------|----------------------------------------------------------|------------------------------------------|-------------|
| `human`     | `slack:U…`, `oidc:<email>`                               | A person caused this mutation.            | **Yes**     |
| `sentinel`  | `system:scheduler`, `fleet:gardenio`, future `system:*`   | Known-autonomous infra. Transparent.      | **No**      |
| `unknown`   | `""` (empty string)                                       | Pre-cutover, unresolved, or bridge gap.   | **No**      |

The set of sentinel prefixes is closed and small (today: `system:`, `fleet:`). Classification is a prefix lookup, not a database call.

Rule of thumb: **only `human`-class incoming values can take dibs.** Everything else is transparent — it is recorded in `mutation_history` for the audit trail, but `beads.operated_by` is left alone.

### 3.1 Why there is no "agent" class

An earlier draft of this doc reserved an `agent:<bead-id>` class for "an autonomous agent acting on its own." That was wrong, and Matt called it out on the merged MR: **an operator's agent must transmit the operator to any beads it modifies.** Agents are never operators; they are conduits.

Concretely: every running agent has an *inherited operator* — the human (or sentinel, in the all-system spawn case) on whose behalf the agent is acting. Every `kd` / `gb` call from inside the agent pod stamps that inherited operator into `X-Operated-By`. The server therefore never sees `agent:<bead-id>` as `incoming`; it sees the human (or sentinel) the agent is currently inheriting.

The mechanism that maintains "currently inheriting" is owned by R2 (push/pull propagation) and §5.6 below, but R3's invariant is simply: **`incoming` is always one of {human, sentinel, unknown}, never agent**. The dibs rule does not need a fourth class.

If a caller path ever did manage to send an `agent:`-prefixed `X-Operated-By`, the server should reject it at the resolver (treat as a misconfigured client) rather than write it to the bead. That's a server-side guard, not a dibs branch.

## 4. The Dibs Rule (precise statement)

On every successful mutation that touches a bead, after the row is written and before the response is returned, the server runs the following resolver against the *bead* row (not per-field, not per-comment):

```
DibsRule(incoming, current, current_set_at, now, mutation_type, target_kind):
  1. If incoming is empty → no-op. (Cannot take dibs without an operator.)
  2. If class(incoming) ≠ human → no-op. (Sentinels are transparent. See §3.1
     for why agent-class incoming should never reach this rule.)
  3. If target_kind ∉ {bead, comment, dep, status, field-set, event} → no-op.
     (Read-only ops, search, and infra-internal calls do not change dibs.)
  4. If mutation_type is "create" → no-op.
     (Insert path already wrote operated_by — see §5 for why we re-affirm it.)
  5. If incoming == current → refresh current_set_at to now and return.
     (Same human re-touching: keep the chip stable, advance the clock.)
  6. If class(current) ∈ {sentinel, unknown} → SET current := incoming,
     current_set_at := now. (Human displaces non-human, always.)
  7. If class(current) == human:
       a. If now − current_set_at < DEBOUNCE_WINDOW (default 60s):
            → no-op. (Two humans in quick succession: keep the first.
            See §7 for rationale.)
       b. Else: SET current := incoming, current_set_at := now.
            (Different human, outside debounce: dibs flips.)
  8. Always APPEND to mutation_history with the actor and the incoming
     operated_by, regardless of whether dibs flipped.
```

That is the whole rule. Eight branches, one server function, no per-field special cases, no cross-table joins.

## 5. Why these answers (per epic question)

### 5.1 Should the server overwrite `beads.operated_by` when a non-empty `X-Operated-By` arrives?

**Yes, conditionally** — branches 5–7 of §4. Specifically: yes when `incoming` is human-class and either (a) `current` is non-human-class, or (b) `current` is human-class and the debounce window has elapsed. This matches the epic's default proposal ("yes if non-empty AND non-sentinel") and adds the debounce nuance from §7.

The "create" carve-out at branch 4 exists because the insert resolver already chose `operated_by` from the same `X-Operated-By` header; re-running the rule would be a no-op, but skipping it explicitly avoids ordering bugs where the dibs hook fires before `created_at` is committed.

### 5.2 Sentinel handling

**Transparent.** A `system:scheduler` cron run that updates `last_run_at` on a schedule bead, or a `fleet:gardenio` reconciler that flips `lifecycle` from spawning→ready, must NOT take dibs from the human who last meaningfully touched the bead. Branch 2 enforces this. The mutation is still appended to `mutation_history` (branch 8) so the audit trail is complete.

This is the load-bearing answer for "transparent to the agent" in Matt's framing: once the human creates a schedule, the schedule's autonomous behavior never overwrites their dibs even though hundreds of cron-triggered mutations may touch the bead.

(See §5.6 for the related but distinct invariant: agents acting *on behalf of* a human stamp the human, not a sentinel. The sentinel-transparency rule above kicks in for genuinely autonomous infra mutations — schedule firings, fleet reconciliation — not for crew agents working under a human's operator.)

### 5.3 Per-field, per-bead, or top-level only?

**Top-level only.** Dibs lives on the bead row, full stop.

Per-field dibs (a separate operator per column) would require either a new `field_operated_by` table or per-row JSON, and would surface in the viewer as a chip salad. Per-comment dibs already exists implicitly: `comments.operated_by` is set on insert and is the natural author of that comment. The bead-level operator is the single answer to "who is this bead 'about' right now?" — it should not fragment.

A comment write *does* trigger the dibs rule for the bead (branch 3 includes `target_kind=comment`). The author of a comment becomes the bead's dibs holder under the same conditions as any other mutation. This matches Matt's intent: if I comment on a bead, I am claiming engagement, not just authoring a row.

### 5.4 Conflict / churn / debounce window for two humans

**60-second debounce, configurable.** Branches 7a/7b. Within 60s of the previous human modifier, a second human's mutation is recorded in `mutation_history` but does NOT flip the bead's dibs. Outside the window, it flips.

Why 60s and not 0:

- The viewer chip is the most visible consumer. If two humans rapid-fire-comment in a Slack thread that mirrors to beads, the chip shouldn't strobe between their avatars on every keystroke.
- 60s is short enough that "I'm done, you take it" handoffs (separated by tens of seconds of typing) still flip cleanly.
- Longer than that risks defeating the rule: if I take a bead from you 90 seconds after your last comment, I genuinely have dibs.

Why 60s and not 5 minutes:

- The chip should track *current* engagement, not "who was here recently". Five minutes is long enough that a real handoff feels stuck.
- Mutation_history retains every attempted flip, so analytics / "who really did the work" queries are unaffected.

Implementation note (out of scope for this doc, but: the debounce check needs `current_set_at`. Today migration 012 only stores `operated_by` and `operated_by_display`. R4 (the audit-trail design) will decide whether to add a `beads.operated_by_set_at TIMESTAMPTZ` column or derive it from the most-recent `mutation_history` row where `field='operated_by'`. The decision table below assumes the column exists.)

### 5.5 Display semantics in the viewer

**Show the current dibs holder by default; surface lineage on hover or in an activity pane.**

- The bead chip on cards, list rows, and the bead detail header reads `beads.operated_by` directly. That is the dibs holder.
- A hover tooltip / "ownership" tab reads `mutation_history` filtered to `field='operated_by'` (or the dedicated table proposed in R4) and renders a transitions list: `created by X → flipped to Y at T1 → flipped to Z at T2`. Mutations by sentinel or agent classes appear in the activity feed but NOT in the ownership transitions list (they didn't take dibs).
- "Originally created by X" is shown alongside the current chip when the original creator differs from the current holder. This addresses Matt's audit-trail requirement at the per-bead UI level without needing a separate page.
- Filter `kd list -f operated_by=me` returns beads I currently hold dibs on. A separate `--ever-operated-by=me` (or viewer toggle) returns beads I've ever held — this maps to a join over the audit table from R4.

### 5.6 Agent operator inheritance and decision-answer rebind

This section covers behavior that is logically owned by R2 (propagation) but is load-bearing for the dibs rule and was explicitly called out by Matt on the merged R3 MR. R2 will own the propagation mechanism in detail; what follows is the **rule** the dibs design assumes.

#### 5.6.1 Spawn-time inheritance (push)

When an agent is spawned, its inherited operator is the operator of the spawner:

- A schedule with `operated_by = slack:U_alice` fires its gardener → gardener bead and gardener pod inherit `slack:U_alice`.
- That gardener spawns a crew agent for a child task → the spawn carries `slack:U_alice` forward via metadata + `BEADS_OPERATED_BY` in the pod env.
- The crew agent's `kd create` / `kd update` / `kd add-comment` calls all stamp `X-Operated-By: slack:U_alice`. The dibs rule then treats those as plain human-class mutations (rows 3, 4, 7b, 13, 14 of the table below).

In the unusual case where the original spawn root has no human operator (e.g. an internal scheduler tick spawning a system housekeeping agent), the chain inherits a sentinel like `system:scheduler`. By branch 2 of §4, that sentinel is transparent — it never takes dibs. This is the desired "spawn chains rooted in nobody do not appear to claim beads" behavior.

#### 5.6.2 Decision-answer rebind (the rule Matt called out)

When an agent creates a `decision` bead and a human answers it, the answering human becomes the agent's inherited operator from that point forward. Concretely:

1. Agent `A` (currently inheriting `slack:U_alice`) creates decision `D` and yields.
2. Human `slack:U_bob` answers `D` (via Slack interaction or the viewer).
3. Server applies the dibs rule to `D` itself — Bob takes dibs on the decision bead per the normal rules.
4. **In addition, the agent bead `A.operated_by` is updated to `slack:U_bob`** (the responder), and `A.operated_by_set_at = now`. This is a side-effect of decision-answer, not the dibs rule firing on `A` directly.
5. The agent pod's `BEADS_OPERATED_BY` env (or the equivalent header it stamps on outgoing `kd`/`gb` calls) is refreshed to `slack:U_bob` before the agent resumes. The refresh path is owned by R2; the simplest viable mechanism is "the agent pod re-reads its own bead's `operated_by` on resume", which already works because the pod has read access to its bead.
6. Every subsequent mutation by `A` — including `kd create` for new task beads, comments on existing beads, status flips — stamps `slack:U_bob` and is treated as a normal human-class mutation by the dibs rule.

This makes "ownership of the agent" a movable handle: spawn establishes it, decision-answer can transfer it. There is no separate "owner" field on the agent bead — `operated_by` IS the answer.

Rebind is **only triggered by human-class responders.** If a sentinel (e.g. an automated decision-resolver) somehow answers a decision, the agent's inherited operator does not change. (This case is unusual — decisions are by definition human-gated — but the rule is symmetric with the rest of the design.)

#### 5.6.3 Why decision-answer specifically (and not "any human comment on the agent bead")

A human commenting on an agent bead is engagement, not authorisation. The agent should not silently start acting on behalf of a random commenter. Decision-answer is a privileged event: the human is explicitly directing the agent's next move. That asymmetry makes decision-answer the right rebinding event — it is the only place where a human's reply carries the semantics of "do as I say from here on."

If we later discover other event kinds with the same authorisation semantics (e.g. an explicit `gb take` or operator hand-off), they should be added to the rebind trigger set deliberately, not by accident.

#### 5.6.4 Cascading consequences

- **Tasks created post-rebind** carry the new operator on insert (per the existing P3 server resolver). Those task beads start their lives as Bob's beads.
- **Beads modified post-rebind** flow through the dibs rule normally — Bob's `incoming` mutates `current` per branches 5–7.
- **Pre-rebind beads** are not retroactively re-attributed. If the agent created task `T` while inheriting Alice, then was rebound to Bob, then *modified* `T`, dibs flips to Bob (if outside the debounce window) — but `T`'s creation history still records Alice, and the audit trail (R4) shows the lineage clearly.

#### 5.6.5 Implications for R2 and the impl epic

R2's propagation design must include a "rebind on decision-answer" step in addition to "inherit at spawn". The simplest implementation is:

- **Server-side**: when a decision is resolved, in the same transaction that records the answer, update the answering agent bead's `operated_by` to the responder if the responder is human-class.
- **Pod-side**: agents re-read their bead's `operated_by` on resume (e.g. as part of the `gb yield` return path or an explicit `gb prime`-style refresh) and update the env they stamp on outgoing calls.

The R3 dibs rule does not need to know any of this — it just sees a flow of human-class `incoming` values from the agent, with the operator changing at the rebind moment. R2 owns the mechanism; this section names the requirement.

## 6. Schema implications (for the impl epic to confirm)

This doc does not write schema, but the rule above implies the following minimum surface area for the implementer:

- Either a new column `beads.operated_by_set_at TIMESTAMPTZ NOT NULL DEFAULT NOW()` (cheap, denormalised, supports debounce check in the same row read), or a fast lookup of the most recent `mutation_history` row for the bead with `field='operated_by'`. The dedicated audit table (option B in the R4 doc) makes the latter trivially fast and is mildly preferred; the column is the fallback if R4 lands on option A.
- An `operated_by_dibs_changed` event on the bead's event stream when branches 5/6/7b fire, so the viewer can react in real time.
- A `field='operated_by'` row appended to `mutation_history` whenever dibs flips. This is in addition to whatever caused the flip; it makes the "who held dibs at time T" query a single index scan.

The dibs resolver itself is pure and has no new external dependencies.

## 7. Decision table

Notation: `H1`, `H2` are distinct human-class operators; `S` is any sentinel (`system:scheduler` / `fleet:gardenio`). `Δt` is `now − current_set_at`. `D` = debounce window (default 60s). `MH+` means a row appended to `mutation_history`. `→` means `beads.operated_by` is set to the new value. (Per §3.1 there is no agent-class incoming — agents stamp their inherited human or sentinel operator.)

| #  | `current` | `current` class | `incoming` | `incoming` class | `mutation_type` | `target_kind` | `Δt` vs `D` | `beads.operated_by` outcome | `mutation_history` | Branch in §4 |
|----|-----------|-----------------|------------|------------------|-----------------|---------------|-------------|------------------------------|---------------------|--------------|
|  1 | any       | any             | `""`       | unknown          | any             | any           | n/a         | unchanged                    | MH+ with `actor`, empty `operated_by` | 1 |
|  2 | `""`      | unknown         | `H1`       | human            | create          | bead          | n/a         | `→ H1` (insert path)         | MH+ create row      | 4 (insert resolver) |
|  3 | `""`      | unknown         | `H1`       | human            | update          | bead          | n/a         | `→ H1`                       | MH+, dibs-flip event | 6 |
|  4 | `S`       | sentinel        | `H1`       | human            | comment         | comment       | n/a         | `→ H1`                       | MH+, dibs-flip event | 6 |
|  5 | `H1`      | human           | `H1`       | human            | update          | bead          | any         | unchanged value, refresh `set_at` | MH+ | 5 |
|  6 | `H1`      | human           | `H2`       | human            | update          | bead          | `Δt < D`    | unchanged                    | MH+ (no dibs-flip event) | 7a |
|  7 | `H1`      | human           | `H2`       | human            | update          | bead          | `Δt ≥ D`    | `→ H2`                       | MH+, dibs-flip event | 7b |
|  8 | `H1`      | human           | `H2`       | human            | comment         | comment       | `Δt ≥ D`    | `→ H2`                       | MH+, dibs-flip event | 7b |
|  9 | `H1`      | human           | `S`        | sentinel         | status          | bead          | any         | unchanged                    | MH+ | 2 |
| 10 | `H1`      | human           | `S`        | sentinel         | field-set       | bead          | any         | unchanged                    | MH+ | 2 |
| 11 | `S`       | sentinel        | `S`        | sentinel         | status          | bead          | any         | unchanged                    | MH+ | 2 |
| 12 | `H1`      | human           | `H1` (via crew agent inheriting `H1`) | human | comment | comment | any | unchanged value, refresh `set_at` | MH+ | 5 |
| 13 | `H1`      | human           | `H2`       | human            | dep             | dep           | `Δt ≥ D`    | `→ H2`                       | MH+, dibs-flip event | 7b |
| 14 | `H1`      | human           | `H2`       | human            | status          | bead          | `Δt ≥ D`    | `→ H2`                       | MH+, dibs-flip event | 7b |
| 15 | `""`      | unknown         | `S`        | sentinel         | update          | bead          | n/a         | unchanged (`""`)             | MH+ | 2 |
| 16 | `H1`      | human           | `""`       | unknown          | update          | bead          | n/a         | unchanged                    | MH+ | 1 |
| 17 | `H1`      | human           | `H1`       | human            | comment         | comment       | `Δt < D`    | unchanged value, refresh `set_at` | MH+ | 5 |
| 18 | `H1`      | human           | `H1`       | human            | dep-add         | dep           | `Δt ≥ D`    | unchanged value, refresh `set_at` | MH+ | 5 |
| 19 | `H1`      | human           | `H2`       | human            | view/read       | none          | any         | unchanged                    | (no MH row — reads do not write) | 3 |
| 20 | `S`       | sentinel        | `H1`       | human            | spawn-side-effect (e.g. gardener firing an autonomous spawn that invokes `kd update` with the human's `X-Operated-By` carried via R2 push) | bead | any | `→ H1` | MH+, dibs-flip event | 6 |
| 21 | `""`      | unknown         | `""`       | unknown          | update          | bead          | n/a         | unchanged                    | MH+ | 1 |
| 22 | `H1`      | human           | `H1`       | human            | status          | bead          | `Δt ≥ 7 days` | unchanged value, refresh `set_at` | MH+ | 5 |
| 23 | `H1`      | human           | `H2`       | human            | rapid: 3 mutations from `H2` within `D` seconds | bead | each `Δt < D` after first (which itself was `Δt ≥ D` from `H1`) | first flip: `→ H2`; subsequent within `D`: refresh `set_at` (branch 5, since `current` is now `H2`) | MH+ each | 7b then 5 |
| 24 | `H1`      | human           | `H2`       | human (agent crew, post-decision-answer rebind: inherited operator was `H1`, became `H2` when `H2` answered the agent's decision) | update | bead | `Δt ≥ D` | `→ H2` | MH+, dibs-flip event | 7b (see §5.6) |
| 25 | `""`      | unknown         | `H2`       | human (new task created by agent crew after decision-answer rebind from `H1` to `H2`) | create | bead | n/a | `→ H2` (insert path) | MH+ create row | 4 (insert resolver) |

### Notes on the table

- **Row 1 / 16 / 21:** Empty `incoming` is rejected by the dibs rule but the underlying mutation still succeeds. Empty `incoming` should be unusual after R7 backfill and R9 enforcement — it indicates a caller path that didn't set `X-Operated-By`. R1 inventory will enumerate those callers.
- **Row 2:** The insert path is logically out of dibs scope (branch 4) but is included for completeness — the same human-class value lands on the bead because the insert resolver chose it.
- **Row 5 / 17 / 18 / 22:** Same human re-touching always refreshes `set_at` regardless of mutation kind. This keeps the debounce window anchored to actual engagement, not to the first-ever touch.
- **Rows 9–11:** Whenever `incoming` is non-human-class, the bead row is left alone. The mutation_history row is still written so an audit query can reconstruct everything that touched the bead, including transparent infra activity.
- **Row 19:** Reads (`kd show`, list, search, embedding probes) do not pass through this rule at all. Dibs is a write-side concept.
- **Row 20:** This is the R2 propagation case — a gardener or scheduler spawn forwards the human's `X-Operated-By` so that the resulting mutations on child beads are attributed to the human. The dibs rule treats it indistinguishably from a direct human mutation, which is exactly the desired behavior.
- **Row 23:** Demonstrates that bursts from a single human after a flip don't re-flip the bead — they refresh `set_at` only. This bounds debounce-window thrash to first-flip-only.
- **Rows 12, 24, 25:** Crew agents stamp their inherited operator and are indistinguishable from the human at the dibs layer. Row 12 is a crew agent inheriting `H1` re-touching `H1`'s bead — same-human refresh per branch 5. Row 24 is a crew agent post-decision-answer rebind: `H2` answered the agent's decision, the agent's inherited operator updated to `H2`, and now the agent's `kd update` carries `H2` as `incoming`. Row 25 is the consequent rule Matt called out — tasks created by the agent after the rebind belong to `H2`, not the original spawn-time operator. See §5.6.

## 8. Open questions for the decision checkpoint

These should be folded into the parent epic's `gb decision create` (ka-0000003404) rather than answered unilaterally here:

1. **Debounce default.** 60s is a defensible default; we may want different windows for different `mutation_type`s (e.g. 0s for `status`, 60s for `comment`). Recommend: ship with a single 60s knob, revisit if logs show flip churn.
2. **Display threshold.** Should the chip show "originally X / now Y" only when X≠Y, or always render the lineage? Recommend: only when X≠Y, to keep the common case clean.
3. **kd CLI surface for "ever-operated-by".** New flag (`--ever-operated-by=me`) vs viewer-only feature. Recommend: viewer-only initially, add CLI when an audit user actually asks for it.
4. **Dibs flip on dep removals.** Branch 13 covers `dep` add; should dep *remove* flip dibs the same way? Recommend: yes — removing a dependency is as engagement-positive as adding one. The decision table treats `mutation_type=dep` symmetrically.
5. **Cross-bead cascades.** If I take dibs on a parent epic, does that propagate to its open child tasks? Recommend: **no**. Dibs is per-bead. Cascade is the responsibility of R2's push-propagation rules at *spawn* time, not at *modification* time.
6. **Decision-answer rebind trigger set.** §5.6 defines the rebind on decision-answer. Should other operator-handoff events trigger the same rebind (`gb take`, explicit reassignment, MR review approval)? Recommend: ship with decision-answer only; expand the trigger set when concrete cases come up. Mechanism is owned by R2.

## 9. What this doc is not

- Not a schema migration. The new column / new table choice belongs to R4.
- Not a server-side patch. The implementation epic gated by the parent decision checkpoint will write `dibs_resolve()` and integrate it with the existing `operatedByFromContext` resolver.
- Not a backfill plan. Backfilling historic beads to "most recent modifier" is explicitly out of scope per the parent epic and is a separate question.
- Not a test plan. Per the parent epic's hard rule, no test code or test design lives here. Test strategy will be sequenced under the implementation epic.

## 10. Links

- Parent epic: ka-0000003404
- This task: ka-0000006715
- Sibling research:
  - R1 inventory (ka-0000000973) — `operated-by-transitivity-r1-inventory.md` (TBD)
  - R2 propagation (ka-0000005258) — `operated-by-transitivity-r2-propagation.md` (TBD)
  - R4 audit (ka-0000006418) — `operated-by-transitivity-r4-audit.md` (TBD)
  - R5 rollout (ka-0000001481) — `operated-by-transitivity-r5-rollout.md` (TBD)
- Prior research plan: ka-0000003603 (R1-R10, closed)
- Implementation epic that introduced `operated_by`: ka-0000001133
- Schema: migration `012_human_attribution.up.sql` (operated_by columns), `008_mutation_history.up.sql` (per-field history)
