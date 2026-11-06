# CED Efficiency token-cost hard-cap

Token-eff 3 (ka-0000007291), under epic
[ka-0000003617](https://gardenio.e2e.example.com/beads/ka-0000003617).

The CED session-inspector (formula
[ka-0000004287](https://gardenio.e2e.example.com/beads/ka-0000004287)) grades
a real agent session on **Correctness / Efficiency / Discoverability**. Before
this change its Efficiency dimension counted only tool-call redundancy and
measured **zero token spend** — the fleet's single largest cost axis was
invisible to its own audit loop.

This change folds token spend in as a **distinct signal on the existing
Efficiency dimension** — *not* a separate 4th dimension — so the 7-bead molecule
keeps its shape (6 active steps + the molecule root bead). The measure/judge
split mirrors the fleet ethos:

| Step | Command | Role |
|---|---|---|
| `fetch-and-measure` | `gb session analyze <jsonl> --token-signals --json` | **Measure** — record the objective token-cost block. **No scoring.** |
| `judge` | `gb session analyze <jsonl> --efficiency-cap --json` | **Judge** — evaluate the ONE reproducible hard-cap over those counters. |

`--efficiency-cap` implies `--token-signals`, so the judge always has the signals
it grades.

## The measurement block (objective, no scoring)

`--token-signals` emits task-1's block (Token-eff 1, ka-0000003091). The
token-cost sub-dimension consumes four of its counters:

- `cache_hit_ratio` = `cache_read / (cache_read + input + cache_creation)`
- `output_per_request` = mean output tokens per API request
- `redundant_context_spikes` = turns whose `cache_creation` exceeds 2× the
  running median of prior nonzero writes **and** is at least `minSpikeTokens`
  (10000) in absolute size (re-primed / bloated context)
- `turn_count` (used only to gate the cache-hit floor to long sessions)

Every field is a token count or a token/token ratio. There is **no USD figure
computed from tokens** anywhere in the path — authoritative cost stays with
LiteLLM, and the cost-source AST guard
(`internal/metrics/cost_source_guard_test.go`, epic ka-0000000758) stays green.

## The hard-cap (judge, reproducible)

`--efficiency-cap` runs `evaluateEfficiencyCap` — a **pure function** of the
objective counters (no I/O, clock, randomness, or map iteration). Two graders
running it on the same session get byte-identical verdicts. When **any** of the
conditions below fires, the Efficiency dimension is capped at
**`efficiencyCapScore` = 6** (the judge applies `min(existing_score, 6)`), which
matches the existing `redundant_repeat_queries` / `brute_force_scans` caps and
keeps the dimension at-or-below the file-bugs threshold so token bloat reliably
surfaces a bug.

Conditions are evaluated in a **fixed order** (so the serialized verdict is
byte-stable) and each carries a remediation tag:

| # | Signal | Condition | Tag | Rationale |
|---|---|---|---|---|
| 1 | `cache_hit_ratio` | `< 0.60` **and** `turn_count >= 20` | **data-plane** | A long session below the cache-hit floor is thrashing context — re-priming/rebuilding instead of riding cache. A context-composition defect, crew-fixable. The turn gate exempts cold short sessions whose ratio is low by nature. |
| 2 | `redundant_context_spikes` | `>= 2` | **data-plane** | Two or more re-prime balloons is a repeated bloated-context / redundant-payload defect. One spike can be legitimate; a habit of them is not. Crew-fixable. A spike must be **both** disproportionate (`> 2×` the running median write) **and** absolutely large (`>= minSpikeTokens`) — see the spike floor below. |
| 3 | `output_per_request` | `> 8000` | **agent-behavior** | Verbose-output habit. Output is billed at the top rate and is a dominant cost lever, but trimming it is agent **education**, not a code fix — **advice-only, never crew-fixed**. |

Boundaries are strict where documented: `cache_hit_ratio` exactly at `0.60` does
**not** cap (strict `<`); `output_per_request` exactly at `8000` does **not** cap
(strict `>`); `redundant_context_spikes` caps **at** `2` (`>=`).

### The spike floor (`minSpikeTokens`)

A spike must clear **two** tests, not one: `cache_creation > 2× running median`
**AND** `cache_creation >= minSpikeTokens` (10000). The floor is inclusive — a
write exactly at 10000 that also clears 2× the median counts.

The ratio test alone is **scale-free**, and that made it a false-positive
generator (bug ka-0000003202). On a session whose typical rebuild is small
(~1.5k tokens), an ordinary `sed`/`grep` read of a source file writes ~5k and
trips ">2× the median" without re-priming anything. Measured on session
`ka-0000005126/7622b506`: four spikes of 3687/5153/5278/5434 tokens against a
~1.4k–1.8k median capped the Efficiency dimension of a 152-turn feature build
whose `cache_hit_ratio` was **0.9909** — definitionally the opposite of
re-priming.

Either test alone misfires, which is why both are required: the ratio alone flags
routine reads on small-baseline sessions, and the floor alone would flag a
session whose every write is legitimately large. 10000 sits well above a routine
source read and well below a genuine re-prime balloon (the bloated fixture's 50k
balloons against a 10k baseline still fire).

This mirrors the `turn_count >= 20` gate on the cache-hit floor: both are
carve-outs that keep a scale- or phase-sensitive signal from firing where it is
not meaningful.

### Threshold source of truth

The numbers above live once, as named constants:
`gardenio/controller/cmd/gb/session_efficiency_cap.go` holds the cap thresholds
(`cacheHitFloor`, `longSessionMinTurns`, `redundantSpikeCap`, `verbosityCeiling`,
`efficiencyCapScore`), and `session_usage.go` holds the detector's
`minSpikeTokens` floor next to the counter that applies it. This doc, the unit
tests, and the formula prose all cite the same constants — change them in one
place.

## Finding tags → remediation routing

The tags route directly into the formula's file-bugs and dispatch-remediation
steps:

- **`data-plane`** findings (prime/context-composition + redundant-payload
  defects) are **crew-fixable**: they become fix-tasks under the CED remediation
  epic ka-0000004371 and get a `dataplane-fixer` crew dispatch.
- **`agent-behavior`** findings (verbose-output habits) are **observe-only**:
  file/refresh a targeted advice bead, but never spawn crew to "fix" another
  agent's behavior.

## Fixtures + reproducibility test

Three captured JSONL fixtures pin the cap so two graders agree:

- `testdata/ced/token_cost_bloated.jsonl` — a long, cache-thrashing, verbose,
  re-priming session. Fires **all three** conditions (2 data-plane + 1
  agent-behavior).
- `testdata/ced/token_cost_healthy.jsonl` — a long session that rides cache,
  stays concise, and never re-primes. Fires **nothing**.
- `testdata/ced/token_cost_efficient_build.jsonl` — the **real** per-turn series
  of session `ka-0000005126/7622b506`: a 152-turn feature build at 0.9909
  cache-hit with four small source-file reads. Fires **nothing**, and is the
  regression pin for bug ka-0000003202 (before `minSpikeTokens` it capped on a
  single spurious `redundant_context_spikes` reason).

`session_efficiency_cap_test.go` asserts the bloated fixture caps deterministically
with the correct tag split, the healthy and efficient-build ones do not, and
repeated evaluations are byte-identical
(`TestEfficiencyCap_FixturesAgreeAcrossGraders`).

## Try it

```bash
gb session analyze testdata/ced/token_cost_bloated.jsonl --efficiency-cap
gb session analyze testdata/ced/token_cost_healthy.jsonl --efficiency-cap
```
