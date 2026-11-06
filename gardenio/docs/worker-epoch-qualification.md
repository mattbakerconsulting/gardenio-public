# Worker-epoch qualification suite (W4)

Workstream **W4** of the harness-improvements plan (epic `ka-0000007152`,
task `ka-0000002814`). Framework provenance: the `lopopolo/harness-engineering`
**fixed-worker / epochs** thesis — *hold the worker constant, and qualify every
change to it against outcomes.*

## The gap this closes

Gardenio flips the worker live — model, reasoning effort, `coop` version, agent
image — and has done so with **no standing suite proving the flip helped**. The
`fable+xhigh → opus-4-8+high` revert (`ka-0000001865`) was a hand-written,
deterministic *revert trigger* with zero before/after outcome measurement: it
put the worker back, but never answered *did the flip make jobs close more
often, with less human help?*

A **worker epoch** is a frozen tuple:

```
(model, effort, coop_version, image_tag)
```

Every change to any dimension starts a new epoch. This suite is the fixed
**condition** side of a fixed-worker experiment: a small, bounded catalog of
representative *whole-job* journeys that each epoch runs through, graded at the
**outcome boundary** and stored as beads so epochs are comparable across time.

## Grade at the outcome boundary — not tokens burned

The one rule that makes this suite trustworthy: **grade what the job produced,
not how the worker got there.** No "tokens produced" leaderboard, no turn count,
no wall-clock (`ka-0000005924` / the plan's non-goals). Three outcome-boundary
measurements per journey:

| Measurement | Question |
|---|---|
| `job_closed` | Did the originating bead / thread / decision reach terminal resolution **under the worker's own power** — not an operator finishing it by hand? |
| `proof_attached` | Where proof is required, is there a **claim-matched** artifact (MR PROOF block, closing comment with evidence, decision report)? Green CI alone does not count (`ka-0000008060`, W2). |
| `human_relays` | How many times did the worker hand back to a human to make progress **it could have made itself** — a redundant decision, an in-scope escalation, a question the context already answered? A *legitimate* privileged ask (a real merge approval, a secret mint) is **not** a relay. |

### Deterministic rubric

```
fail    : job_closed is false.
partial : job_closed is true, but a required proof is missing
          OR human_relays exceeds the journey's max_human_relays.
pass    : job_closed is true, required proof attached,
          human_relays within the journey's max_human_relays.
```

This is encoded exactly in `worker-epoch.py:grade_journey` so grading is
reproducible and blind to prose.

## The journeys

Catalog of record: [`scripts/worker-epoch-journeys.json`](../../scripts/worker-epoch-journeys.json)
(versioned, validated in CI). Each journey is a **whole job the fleet actually
does end-to-end** — deliberately *not* a single turn (that is what
[`scripts/model-ab-corpus.json`](../../scripts/model-ab-corpus.json) measures for
per-turn cost/latency). Kept bounded to **5–8** journeys — each one consumes a
real agent pod per epoch.

| id | category | Whole job |
|---|---|---|
| `thread-qa` | thread | Self-answerable operator question → in-thread answer, no bead churn |
| `mr-from-bug` | crew | Claim a bug → branch, fix, test, non-draft MR with a PROOF block, linked |
| `gardener-iteration` | gardener | Decompose an epic → dispatch crew → reconcile the prior wave |
| `release-watch` | release | Cut/verify a release and confirm the **deployed image tag** advanced |
| `decision-roundtrip` | decision | Hit a real fork → file a resolvable decision → act on the resolution |
| `mr-merge-agent-authored` | review | Review, fix on-branch, and **merge** an agent-authored gardenio MR |
| `bug-triage-file` | triage | Dedup-search → file one correctly-parented, linked bead |

The first five are the journeys the task names explicitly; the last two round out
the fleet's dominant whole-job shapes (landing others' work, and triage). The
validator enforces coverage of the required categories
(`thread, crew, gardener, release, decision`).

## The subtraction pass

A stronger worker should make some scaffolding **redundant** — advice it no
longer needs re-told, a hook that never fires because the failure it guards
against stops happening. Each journey carries a `scaffolding` list naming the
advice/hooks it stresses. `worker-epoch.py compare` surfaces
**subtraction candidates**: journeys the *candidate* epoch passes cleanly (either
newly, or steady-state on both epochs). Those scaffolding entries are the input
to a human subtraction review — *does a worker this good still need this prose?*
This is the deletion half of qualification, not just the addition half; it keeps
the corpus from growing monotonically (the W1/W6 concern).

Subtraction candidates are **candidates**, never automatic deletions: a journey
passing once is exploratory evidence (`ka-0000004800` — a small sample is not a
trend). Retire scaffolding only after it stays redundant across repeated epochs.

## The tool

[`scripts/worker-epoch.py`](../../scripts/worker-epoch.py) — pure stdlib,
hermetic (no daemon, no network, no `kd`/`gb`, no clock, no randomness), unit
suite in `scripts/worker-epoch_test.py`. It **defines, snapshots, grades, and
compares**; it does **not** run journeys (spawning pods and driving jobs is done
out of band, then the observed measurements are recorded).

```sh
# 1. Validate the catalog (also runs in CI).
python3 scripts/worker-epoch.py validate

# 2. Snapshot the current worker epoch -> stable epoch_id.
python3 scripts/worker-epoch.py snapshot \
    --model claude-opus-4-8 --effort high \
    --coop-version 0.9.1 --image-tag 2026.241.15

# 3. Emit a result skeleton (one un-graded row per journey) for this epoch.
python3 scripts/worker-epoch.py skeleton \
    --model claude-opus-4-8 --effort high \
    --coop-version 0.9.1 --image-tag 2026.241.15 > epoch-A.json

#    ... run each journey out of band, fill in observed.job_closed /
#    proof_attached / human_relays in epoch-A.json ...

# 4. Grade the filled file -> pass/partial/fail tally.
python3 scripts/worker-epoch.py grade epoch-A.json

# 5. After a flip, repeat 2-4 for epoch-B, then diff:
python3 scripts/worker-epoch.py compare epoch-A.json epoch-B.json
```

`epoch_id` is a 12-char content hash of the **normalized** tuple: incidental
case/whitespace never forks an epoch, but a change to any dimension always
produces a new id.

### Storing results as beads

Store each graded epoch result as a `type=doc` bead (body in `fields.content`,
`ka-0000006521`) parented to the qualification epic, titled with the `epoch_id`.
Cross-epoch comparison then reads two such beads. Link them back so the epochs
are traceable in beads-viewer rather than orphaned files.

## When to run it

On **every worker flip** — model, effort, `coop` version, or agent image tag —
run the suite for the new epoch and `compare` it against the last green epoch
before adopting the flip fleet-wide. A flip with **regressions** (a journey that
dropped a grade) is not qualified; a flip with only improvements/unchanged, and
new subtraction candidates, is.

## Relationship to the other eval lanes

- `scripts/model-ab-compare.py` — per-**turn** cost/latency across two models.
  Different question (how expensive is a turn), different granularity.
- `scripts/opus5-ab.py` — fleet context behaviour before/after a context trim.
- **This suite** — whole-**job** outcomes across worker epochs. The gap both of
  the above leave open: *does the job actually close, and with how much human
  help?*

## Non-goals

- No token/turn leaderboard — outcomes only.
- No automatic scaffolding deletion — subtraction produces *candidates* for human
  review.
- No context-curator sidecar or projection-hash machinery (the plan's held
  non-goals).
