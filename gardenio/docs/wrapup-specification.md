# Wrap-Up Specification & Audit (Phase 2)

> **Status:** v1 (2026-08-12). Deliverable for [[ka-0000003135]] "Phase 2:
> Audit and formalize wrapup specification", under epic [[ka-0000004384]]
> ("Formalize gardeners"). Builds on the Phase 1 end-to-end trace
> ([[ka-0000005039]], bundle [[ka-0000002547]]).
>
> **Canonical machine-readable schema:**
> [`gardenio/docs/schemas/wrapup.schema.json`](./schemas/wrapup.schema.json)
> (JSON Schema 2020-12). This doc is the human-readable audit + rationale
> behind it.

This document answers the four Phase-2 questions from the bead:

1. What fields do gardeners currently emit?
2. Which are machine-parseable, which are text blobs, which are missing?
3. What would gardeners NEED to emit for D1/D2/D3/D4 scoring to work?
4. Are there schema mismatches between what gardeners emit and what the
   scoring formula expects?

The answer to (4) reframes the whole task: **the wrap-up is not the primary
input to the gardener scorer.** Only D1 reads a wrap-up field; D2–D5 are
derived from the bead graph and D6 from gardener config. So the wrap-up's job
is narrower and more precise than "feed the scorer" — it is the **D1
self-report layer, the Slack-card surface, and the succession/handoff
contract**. The schema formalizes exactly that.

---

## 1. The wrap-up contract today

The producer is `gb stop --wrapup '<json>'` (also `gb done`). The payload is
validated at the stop boundary, a human summary comment is posted, and the
JSON is stored verbatim in the agent bead's `fields["wrapup"]`. Source of
truth: the `WrapUp` struct in
[`gardenio/controller/cmd/gb/wrapup.go`](../controller/cmd/gb/wrapup.go).

### 1.1 Field inventory (source of truth)

| Field | Type | Enforcement today | Machine-parseable? | Consumer |
|---|---|---|---|---|
| `tldr` | string (≤500, 1 line) | **hard-required** (default) | yes (surfaced, not scored) | Slack card headline |
| `accomplishments` | string | **hard-required** (default) | no — prose blob | Slack card, humans |
| `blockers` | string | optional | no — prose blob; presence is a signal | card ⛔ glyph |
| `handoff_notes` | string | optional | no — prose blob; presence is a signal | successor agent, card ↗️ |
| `beads_closed` | []string | optional, agent-supplied (F3 resolved) | yes | none effective today |
| `pull_requests` | []uri | optional | yes | auto-linked to beads at stop |
| `progress_score` | int (0,1,≥2) | **hard-required iff** schedule `progress.surface.in_wrapup=true` | **yes** | **D1 self-report** |
| `progress_summary` | string | required with `progress_score` | no — prose | D1 justification |
| `goal_satisfied` | enum `yes`/`no` | required iff bead has `terminal_goal` | yes | respawn/succession reconcile |
| `goal_report` | string | required with `goal_satisfied` | no — prose | humans |
| `respawn_after` | Go duration | optional | yes | respawn scheduler |
| `restart_when` | string | optional (nudged) | free text (bead-id survives) | card "Wakes when …" |
| `successor` | enum `none`/`same`/`new` | validated (rejects unknown) | yes | succession engine |
| `harness_gaps` | string | optional | free text (MLD sensor) | groomer/CED corroboration (plan §W6) |
| `custom` | map[string]string | advice-driven | yes | advice extensions |
| `timestamp` | RFC3339 | auto | yes | ordering |

### 1.2 What gardeners actually emit — empirical audit

Corpus: 25 recent gardener agent beads (delivery, release-owner,
meta-gardener, plan-gardener, phase0-haiku; sampled 2026-08-12). 23 carried a
parseable wrap-up object.

| Field | Present / 23 | Reading |
|---|---|---|
| `tldr` | 23 (100%) | Hard-require works — universal. |
| `accomplishments` | 23 (100%) | Hard-require works — universal. |
| `progress_score` | 15 (65%) | Present only where the **schedule opts in**. The 8 misses are gardeners whose schedule has no `progress.surface.in_wrapup` — expected, not a defect. |
| `progress_summary` | 11 (48%) | Weaker than `progress_score` even where scored — see F2. |
| `handoff_notes` | 5 (22%) | Sparse, as designed (only when handing off). |
| `blockers` | 1 (4%) | Sparse, as designed (only when blocked). |
| `pull_requests` | 0 | Gardeners dispatch crew; the crew open MRs. Gardeners rarely open MRs themselves, so 0 is honest here. |
| `beads_closed` | 0 | Never populated — was documented auto-populated but never wired (finding **F3**, since resolved by re-documenting it as optional/agent-supplied). |
| `goal_satisfied` / `restart_when` / `successor` | 0 | Inert for gardeners today: they spawn in crew mode without `terminal_goal`, and default succession is terminal. |

**Headline:** the two hard-required fields achieve 100% conformance. Every
field that is *enforced by policy* (hard-require, or schedule-gated
progress) is emitted; every field that is *purely nudged* is emitted only
opportunistically. **Enforcement, not exhortation, is what produces a field.**

---

## 2. What the scorer actually needs (and where it reads it)

This is the crux. The gardener-effectiveness rubric
([`gardener-effectiveness-rubric.md`](./gardener-effectiveness-rubric.md))
scores six dimensions. Their data sources:

| Dim | Weight | Reads from | Wrap-up dependency |
|---|---|---|---|
| **D1** progress yield | 30 | `progress_score` in the run's wrap-up **OR** graph events (`child_closed`, `mr_merged`, `decision_resolved`) credited in the run window | **Yes — the only dimension that reads a wrap-up field.** |
| **D2** throughput | 20 | Bead graph: epic child closures in the window (reverse `kd list --parent`) | No |
| **D3** compute economy | 15 | `progress_state.runs` (outcome, rate-limit, failure) + dormancy | No (run outcomes, not wrap-up prose) |
| **D4** crew health | 15 | Bead graph: crew beads the gardener dispatched (agent_state, live pod) | No |
| **D5** operator burden | 10 | Bead graph: decisions raised vs. self-adjudicated, stale reflags | No |
| **D6** loop hygiene | 10 | Gardener **config** (`terminal_goal`, `max_runs`, `loop_end`, cron) — deterministic `bridge.ScoreGardenerD6` | No |

**Consequence:** you cannot "fix scoring" by adding wrap-up fields for D2–D5
— those are graph-derived by design, and deliberately so (the rubric's §D1
"OR credited" clause exists precisely because self-reported scores lie on
async-delegation runs). The wrap-up's scoring role is exactly one field:
`progress_score`, as the D1 self-report that the engine then cross-checks
against real graph events.

So the correct D1 signal is **dual-source**:

```
progress_run(run) := run.progress_score >= 1
                     OR ∃ credited-event e in [run.started_at, ∞)
                        with e.occurred_at >= run.started_at
```

The wrap-up supplies the first clause; the graph supplies the second. The
schema pins the first; the rubric §D1 box pins the second.

---

## 3. Findings (schema mismatches & gaps)

### F1 — Wrap-up is misframed as "the scorer's input" (design clarity, not a bug)

The Phase-2 bead asks "what would gardeners NEED to emit for D1/D2/D3/D4
scoring." The audit's answer: **for D2/D3/D4, nothing** — those read the
graph. Only D1 needs a wrap-up field (`progress_score`), and only when the
schedule opts in. The formalized schema encodes this explicitly in each
field's description so future gardener authors don't over-invest in wrap-up
prose expecting it to move D2–D5.

### F2 — `progress_summary` lags `progress_score` (48% vs 65%)

Where progress is scored, the justification is emitted only ~three-quarters
of the time, even though `wrapup_progress.go` requires both under
`in_wrapup`. The gap is agents providing `progress_score` but an empty
`progress_summary` on `--force` stops, or on schedules whose enforcement is
soft. **Recommendation:** keep the paired hard-require (already coded); the
schema marks them co-required. No code change needed — this is a conformance
observation, tracked so a future enforcement audit can confirm closure.

### F3 — `beads_closed` was documented auto-populated but never written (0/23) — RESOLVED

**Original finding:** `wrapup.go` documented `beads_closed` as "auto-populated
with IDs of beads the agent closed," and the field is read by
`wrapUpFieldPresent` and the comment renderer. It round-trips if the agent
*supplies* it in the raw JSON (`TestProcessWrapUp_WithBeadsAndPRs`), but
**no code path in the `gb stop` flow ever auto-derived it from the session's
close activity** — so the "auto-populated" contract was unmet and, in
practice, no gardener supplied it by hand either: every one of the 23 audited
wrap-ups had it absent. This was a latent documentation/behavior mismatch. It
did **not** corrupt scoring (D2 reads the graph, not this field), but it made
the wrap-up less self-describing and would have misled any future consumer
that trusted the "auto-populated" contract.

**Resolution (ka-0000005070):** the doc was corrected to match behavior rather
than the behavior wired to match the doc — **option (b)**. `beads_closed` is
now documented as **optional, agent-supplied** across all three surfaces (the
`WrapUp` struct comments in `wrapup.go`, `wrapup.schema.json`, and this spec).
`gb stop` continues to store and round-trip whatever the agent passes in
`--wrapup` but never fabricates the field. Option (a) — auto-deriving from
session close activity — was rejected as disproportionate: it consumes no
scorer (D2 is graph-derived), so wiring it would require a new `closed_by` +
`closed_after` daemon filter and a reliable session-start timestamp purely to
populate a self-describing convenience field. The schema `x-status` is now
`optional-agent-supplied`.

### F4 — No machine-readable schema existed until now

Before this deliverable, the contract lived only as Go struct tags + prose
in `wrapup.go`. There was no artifact a non-Go consumer (a dashboard, a
validator, a successor-agent framework, an external tool) could load to
validate a wrap-up. `wrapup.schema.json` closes that: it is the single
formal contract both old and new gardeners conform to, versioned alongside
the code.

### F5 — Old vs. new gardener generations already share one struct (no split)

A concern in the parent epic was that "old" gardeners emit a different shape
than "new" ones. The audit finds **no shape split**: all wrap-ups
deserialize into the same `WrapUp` struct, and the only cross-generation
difference is *which optional fields are populated*, governed by schedule
opt-in and hard-require policy — not by gardener vintage. The schema's
`additionalProperties: false` + minimal `required` set (`tldr`,
`accomplishments`) is therefore satisfiable by every gardener generation
without migration.

---

## 4. The formalized specification

**All gardeners (old and new) MUST conform to
[`wrapup.schema.json`](./schemas/wrapup.schema.json).** In prose:

1. **Always required:** `tldr` (≤500 chars, one line) and
   `accomplishments`. Enforced hard by `DefaultWrapUpRequirements`.
2. **Required when the schedule opts in** (`progress.surface.in_wrapup=true`):
   `progress_score` (int; 0/1/≥2) and `progress_summary`. This is the D1
   self-report layer. Enforced by `wrapup_progress.go`.
3. **Required when the agent bead carries `terminal_goal`:**
   `goal_satisfied` (`yes`/`no`) and `goal_report`.
4. **Conditional-by-meaning (omit unless true):** `blockers` (only a real
   blocker), `handoff_notes` (only on handoff), `restart_when` (only a real
   wake condition), `pull_requests` (MRs actually opened/advanced).
5. **Validated enums:** `successor` ∈ {none, same, new}; `goal_satisfied` ∈
   {yes, no}; unknown values rejected at the boundary.
6. **`additionalProperties: false`** — extension fields go through `custom`
   (advice-declared), never as ad-hoc top-level keys, so the contract stays
   closed and validatable.

### 4.1 Conformance status

| Requirement | Status |
|---|---|
| Schema exists and is valid JSON Schema 2020-12 | ✅ this deliverable |
| Matches the `WrapUp` Go struct field-for-field | ✅ verified against `wrapup.go` |
| Hard-required fields conform in the wild | ✅ 100% (`tldr`, `accomplishments`) |
| Schedule-gated `progress_score` conforms | ✅ where opted-in |
| `beads_closed` doc/behavior alignment | ✅ F3 resolved — re-documented optional/agent-supplied (ka-0000005070) |
| `progress_summary` co-require conformance | ⚠️ F2 — 48%, enforcement holds where hard |

---

## 5. Recommended follow-ups (not in this spec-only change)

These are filed as beads under the parent epic rather than done here (this
task's deliverable is the specification):

- **FU-1 (F3, [[ka-0000005070]]):** ✅ **Done** — resolved by re-documenting
  `beads_closed` as optional/agent-supplied across `wrapup.go`,
  `wrapup.schema.json`, and this spec (option b), rather than wiring
  auto-populate. See §3 F3 for the rationale.
- **FU-2 (F2, [[ka-0000005466]]):** Add a conformance check that
  `progress_summary` is non-empty wherever `progress_score` is present,
  surfaced on the meta-gardener scorecard as a hygiene note.
- **FU-3 (Phase 3):** With the schema pinned, Phase 3 (D1 signal design) can
  reference `wrapup.schema.json#/properties/progress_score` as the canonical
  self-report source and the rubric §D1 events as the graph source — the
  dual-source signal is now fully specified on both ends.

---

## Appendix A — Reproducing the audit

```bash
# Collect recent gardener agent beads
kd search "gardener" --type agent --json | jq -r '.[]?.id' | head -40 > ids.txt

# Dump wrap-up objects
while read id; do
  kd show "$id" --json | jq -c '{id, role:.fields.role,
    wrapup:(.fields.wrapup | if type=="string" then fromjson else . end)}'
done < ids.txt > wrapups.jsonl

# Field-presence tally (see §1.2)
jq -s '{total:length,
  tldr:[.[].wrapup|select(.tldr!=null and .tldr!="")]|length,
  progress_score:[.[].wrapup|select(.progress_score!=null)]|length,
  beads_closed:[.[].wrapup|select((.beads_closed//[])|length>0)]|length}' wrapups.jsonl
```
