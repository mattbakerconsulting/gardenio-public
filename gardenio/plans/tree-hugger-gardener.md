# Tree-Hugger Gardener — design

Status: draft / design-only (ka-0000003354)
Parent: ka-0000006582 (Slack thread), sibling ka-0000002617 (design discussion)
Operator: @U0OPERATOR1

## 0. TL;DR

A **tree-hugger gardener** is a scheduled+triggered gardener whose charter
is the *safe convergence* of a `feature-demo` branch into `main` via a
stack of `feature-chunk` MRs, while keeping all three loci validatable.
It owns the demo branch outright, takes feedback on the chunks, and
covers its ass on main (never causes a main regression).

Per Slack consensus (ka-0000002617), the MVP uses **no new bead type**:
a normal `type=epic` carrying `label=kind:tree-hug` plus structured
fields `demo_branch`, `base_branch`, `chunk_order`. Children are normal
task beads, one per chunk, each linked to its chunk MR. The gardener is
a `type=schedule` that polls those epics.

## 1. The convergence problem

A "big-diff demo, merged in chunks" workflow has three branches per
epic:

| Locus | Identity | What it proves | What it can break |
|---|---|---|---|
| `main` | trunk | the system at rest | other people's work, prod |
| `feature-demo` | end-state preview | "the full feature works" | only itself; review-only |
| `feature-chunk-N` | reviewer-sized slice of demo | "this slice works *in isolation* on current main" | main, on merge |

The hard parts:

- **Asymmetric validation.** Demo is the easiest to validate because
  it carries the whole feature. Chunks are the *hardest* — each chunk
  must be green on its own atop main, but the natural state in which
  the code was written was demo (with later chunks present).
- **Shared-infra coupling.** Multiple chunks may touch the same files
  (observability/k6/keycloak: the motivating case). The merge order
  matters; the chunk authored later may have implicitly depended on
  the earlier one.
- **Drift.** Main moves under the demo. Demo can silently rot.
- **Ambiguous demo→main relationship.** When does demo "match main"?
  Only when zero chunks are outstanding, and only if main wasn't
  changed by anyone else in the interim.
- **Locus ownership.** Humans/crew are usually mid-edit on chunk
  branches. The tree-hugger must not race them.

The *organizing principle* (operator's words) is **the safe convergence
of a working demo into main, by the facility of chunks**. Everything
else falls out from that.

### 1.1 Synthetic vs long-lived demo

Two ways to maintain demo:

- **(A) Long-lived branch** ("maintain mode"), with `main` and chunk
  branches merged in incrementally. Cheap per cycle. Preserves
  integration commits, manual conflict resolutions, MR review state,
  and CI history. Demo becomes a thing-with-history that humans can
  review as a normal MR.
- **(B) Synthetic demo** — re-derived each cycle as
  `main + (rebased outstanding chunks in stack order)`. Expensive per
  cycle, force-pushes. Demo becomes a *function* of `(main, chunks)`
  rather than a thing-with-history. Auto-heals when chunks land, but
  wipes any integration work that accumulated on the branch between
  cycles.

**MVP originally picked B; the alpha pilot retired B and switched to A
(maintain mode) as the operating default. See ka-0000000569 for the
policy bead and §1.1.1 below for the algorithm.**

#### Why we moved off synthetic-derive

The synthetic model is structurally hostile to the integration work it
should be preserving. In the first pilot (epic ka-0000004697), ~24
integration commits and 6 hand-resolved merge conflicts accumulated on
`integration/perf-observability-demo` over a few days. PILOT GUARD held
the first force-push by design (so an operator could review the
synthetic vs maintained diff before the rebuild). Once the guard
matched the maintained branch's commits could only survive by *blocking
the gardener entirely*. The gardener noop'd for ~48h while the integration
commits remained valuable and unreplayable.

Reviewer state on the demo MR (line comments, approvals, CI history)
is also meaningless in synthetic mode because the branch is reset each
tick. Reviewers can't carry a half-finished review across cycles.

Maintain mode trades the per-cycle "demo is a pure function of inputs"
property for a long-lived branch that humans can review and that
preserves operator effort across cycles.

### 1.1.1 Maintain mode algorithm (current default)

The gardener treats `demo_branch` as a long-lived integration branch and
does NOT force-push or recreate it. Each cycle:

```
git checkout $DEMO_BRANCH        # already exists; bootstrapped once at epic start
git merge --no-ff --no-edit origin/$BASE_BRANCH    # absorb new main commits

for chunk in $CHUNK_ORDER:
    NEW = git rev-parse origin/$chunk_branch
    LAST = chunk.last_integrated_sha           # from frontmatter, see §2.2
    if NEW == LAST: skip
    elif git merge-base --is-ancestor $LAST $NEW:
        git merge --no-ff --no-edit origin/$chunk_branch
        chunk.last_integrated_sha = NEW
    else:
        # chunk branch was force-pushed / rebased — file a decision,
        # do NOT auto-pick between revert+remerge and merge-over.
        file_decision(chunk, drift=True)
        continue

git push origin $DEMO_BRANCH     # NOT --force, NOT --force-with-lease
```

Failure modes:

- **Merge conflict** (main↔demo or chunk↔demo): abort the merge, file a
  decision under the offending bead naming the conflicting paths,
  stop the cycle. The next cycle retries after the decision resolves.
- **Non-fast-forward push** (someone pushed to demo directly): file a
  decision under the epic. Do NOT force-with-lease.
- **Chunk branch force-pushed upstream**: drift case above. The decision
  asks the operator to choose `revert_and_remerge` (revert the prior
  merge commit on demo, then merge fresh) or `merge_over` (merge new
  HEAD on top, treating the duplicate-content conflict as a normal
  resolution).

#### Drop-a-chunk semantics

When a chunk is removed from `chunk_order` (e.g. closed-without-merge):
`git revert <its merge commit on demo>`, commit, push. Auditable record;
no rewriting of demo history.

#### Force-push policy

Force-push is **forbidden** in maintain mode. The only allowed exceptions:

- Operator explicitly issues `gb tree-hug rebuild <epic-id>` (escape
  hatch — not part of any normal cycle, currently unimplemented).
- First-time bootstrap when `demo_branch` does not yet exist on origin
  (chunk_order empty or all chunks lack `last_integrated_sha`).

If the gardener ever tries to call `git push -f` or
`--force-with-lease` outside those exceptions, the prompt is mis-bound
and the cycle must stop.

#### Frontmatter additions (per chunk bead)

| Field | Type | Meaning |
|---|---|---|
| `last_integrated_sha` | string | the upstream chunk SHA last merged into demo |
| `last_integrated_at` | timestamp | for staleness UI |
| `bootstrap_drift` | bool | set true during bootstrap when the chunk had been rebased before maintain mode took over; next maintain cycle will surface a decision to reconcile |

These live in the YAML frontmatter of each chunk bead's description
(FU-2 form), not as typed fields. Aligns with how `chunk_branch`,
`mr_url`, `mr_state` are already encoded.

#### Frontmatter additions (per epic bead)

| Field | Type | Meaning |
|---|---|---|
| `mode` | string | `"maintain"` (default) or `"synthetic"` (legacy, deprecated) |
| `demo_mr_url` | string | URL of the long-lived demo MR |
| `bootstrap_tag` | string | git tag pushed before the first maintain cycle, for recovery |

A single gardener prompt supports both modes via the `mode` flag. The
prompt asserts `mode == "maintain"` and refuses to fall through to the
legacy synthetic-derive branch.

#### Bootstrap

Transitioning an existing epic from synthetic mode (with integration
commits on the demo branch already) to maintain mode requires a
one-time bootstrap procedure: freeze the epic, push a recovery tag,
compute `last_integrated_sha` per chunk from the current demo's
`git merge-base origin/$DEMO origin/$chunk_branch`, open the demo MR,
swap the gardener prompt to maintain mode, unfreeze. The procedure for
pilot epic ka-0000004697 is captured in bead ka-0000004742; future
epics that start in maintain mode skip this entirely (their first
cycle handles initial creation).

## 2. Gardener role contract

### 2.1 Identity

- **Role**: `gardener-tree-hug` (per-epic instances, dispatched by a
  parent scheduler).
- **Schedule bead**: one global `type=schedule` polling for
  `kd list --type=epic -l kind:tree-hug --status=open`. Each open
  tree-hug epic gets its own per-epic gardener dispatch when its tick
  fires (cadence: every 15m global scan; per-epic work is idempotent).
- **Cadence**: 15m baseline, plus trigger-driven re-entry (see 2.3).
  Ultralight when there is nothing to do.

### 2.2 Inputs (per epic)

Read from the tree-hug epic bead:

| Field | Type | Meaning |
|---|---|---|
| `base_branch` | string | defaults to `main` |
| `demo_branch` | string | required; the synthetic head this gardener owns |
| `chunk_order` | list[bead-id] | ordered list of child task beads, each linked to a chunk MR |
| `freeze` | bool | operator escape hatch — gardener must no-op when true |
| `last_demo_sha` | string (gardener-written) | bookkeeping for change-detection |

Each child task bead carries:

- `mr_url`, `mr_state`, `chunk_branch` (already populated by GitLab
  bridge for the first two).
- `chunk_owner` (optional, human/crew name — tree-hugger never
  force-pushes a chunk branch whose `chunk_owner` is alive in
  `gb news`).

External reads: GitLab MR state, CI pipeline status on each chunk and
on demo, `git ls-remote`.

### 2.3 Triggers (what wakes a per-epic gardener)

1. **Chunk MR transitions to CI-red** → dispatch a chunk-fix crew on
   that child bead (do not auto-edit code).
2. **Chunk MR gets a new review comment** → ping owner via `kd comment`
   on the child bead summarising the comment; spawn a crew only if
   `chunk_owner` is dead or absent.
3. **Chunk MR merged into main** → drop from `chunk_order`, re-derive
   demo, advance to next.
4. **`main` advances** (any merge, not just our chunks) → re-derive
   demo, rebase open chunks onto current main if the chunk_owner is
   dead/absent, file a comment for each conflict on the chunk's bead.
5. **Demo CI goes red** → bisect over `chunk_order` (cheapest first:
   pop chunks from the head until green; the popped chunk is the
   culprit), file a bug bead childed under the epic naming the chunk.
6. **Scheduled sanity sweep** — every 15m even with no triggers, to
   catch stuck state (e.g. demo branch deleted by hand).
7. **Operator `freeze=true`** → idempotent no-op, log only.

### 2.4 Outputs

- Maintain-mode (default): incremental merge commits on `demo_branch`
  for each main↔demo and chunk↔demo absorption, pushed (NOT
  force-pushed). See §1.1.1 for the algorithm.
- New bug beads (childed under the epic) for each demo-red / chunk-red
  attribution.
- Comments on chunk beads when feedback or conflict needs surfacing.
- Optional crew dispatches for chunk-fix work (never for chunk feature
  edits — that's the chunk_owner's lane).
- `kd update` of `last_demo_sha` on the epic.
- Per-cycle `gb stop --wrapup` with the KPI delta.

### 2.5 Mission KPI

Single primary metric, per epic, reported each cycle:

```
convergence_distance = open_chunks_count
                     + (1 if demo_red else 0)
                     + (1 if demo_drifted_from_main_plus_chunks else 0)
```

Targets:
- Each cycle either *decreases* `convergence_distance`, *advances*
  the demo SHA, or explicitly reports "nothing to do, all invariants
  hold" (which is a passing cycle, not a FAILED one).
- A cycle that runs without re-deriving demo, advancing a chunk, or
  filing/resolving a bug — when work was available — is a FAILED cycle.

Fleet-level rollup (for reaper/reviver): the KPI is **the count of
open tree-hug epics whose `convergence_distance` is dropping over a
24h window**. Persistent flat epics are candidates for operator
escalation.

### 2.6 The four convergence invariants

The gardener's hard contract:

- **INV1** `demo_branch == base_branch + apply(chunks in chunk_order)`.
  Always. Tree-hugger republishes whenever the equality breaks.
- **INV2** Every open chunk MR is rebased onto current `base_branch`
  and has green CI before merge eligibility. Tree-hugger rebases when
  `chunk_owner` is dead; otherwise it comments and waits.
- **INV3** Chunks have a defined stack order (`chunk_order` on the
  epic). Each chunk must be mergeable into main *given previously
  merged chunks* — no chunk depends on a *later* chunk in the order.
  Tree-hugger detects backwards deps by trial-merge and files a bug.
- **INV4** Tree-hugger never force-pushes a chunk branch whose
  `chunk_owner` is alive (in `gb news --json | jq .agents[].alive`)
  or whose MR was modified by a human in the last hour. It comments
  instead.

### 2.7 What the gardener does NOT do

- Does **not** write feature code on chunks. Chunk authors (crew or
  human) own that.
- Does **not** merge chunks. Merge is a human action (or, by separate
  operator policy, gardener-delivery's lane).
- Does **not** create the epic. Operators or gardener-intake create
  the tree-hug epic when carving up a big-diff piece of work.
- Does **not** open new chunk MRs. Carving the next chunk is a
  delivery/operator decision; the tree-hugger only operates the
  chunks it is told about via `chunk_order`.

## 3. Interactions with existing gardeners

| Gardener | Schedule bead | Interaction |
|---|---|---|
| **gardener-intake** | ka-0000001610 | Creates tree-hug epics during triage when an operator-spawned big-diff thread is in scope. Adds `kind:tree-hug` label and seeds `demo_branch`/`base_branch`. Does NOT manage chunks. |
| **gardener-delivery** | ka-0000000377 | Owns chunk-level *delivery* (code → MR → green CI → merge). When delivery merges a chunk, the tree-hugger reacts via trigger 3 (chunk merged). They do not race because delivery operates on the chunk's bead/branch and tree-hugger only force-pushes `demo_branch`. |
| **gardener-bugfix** | ka-0000005345 | Receives demo-red and chunk-red bugs filed by tree-hugger (childed under the epic). Tree-hugger sets `artifact_type=bug`, priority, and a routing comment "from tree-hug epic ka-XXX — chunk N broke demo CI". |
| **gardener-release** | ka-0000003737 | Independent. Release operates on `main`. Tree-hugger never touches main directly. Release-filed E2E bugs may *parent* a tree-hug epic if the operator wants the fix delivered in chunks, but that is operator-driven. |
| **gardener-log-monitor** | (see gardener-log-monitor-revival.md) | Independent. May surface symptoms that motivate a chunk-fix; routes through bugfix as normal. |
| **gardener-reviver** | ka-0000002321 | Curates *gardeners*, not chunks. May enable/disable the tree-hug scheduler when fleet load demands. Does not touch per-epic state. |
| **gardener-reaper** | ka-0000001548 | Reaps stale tree-hug crew dispatches like any other. Will NOT disable the tree-hug scheduler unless its last 3 cycles all wrapup-fail (same rule as everyone else). |
| **gardener-cycle-watchdog** | ka-0000001612 | Detects FAILED tree-hug cycles via the standard wrap-up grammar; the tree-hugger wrap-up MUST report `convergence_distance` delta to participate. |
| **gardener-placement** | (pending — gardener-placement.md) | Out of scope. Tree-hug is single-repo; placement is a cross-cluster concern. |

Key cross-gardener rule: **tree-hugger may file bugs and dispatch
chunk-fix crews, but never crosses into delivery, release, or main**.
This mirrors the existing hand-off policy: prefer doing the small
thing yourself (rebase a chunk whose owner is dead) over a cross-lane
hand-off, but never write feature code or touch trunk.

## 4. Validation matrix per locus

The gardener's behaviour is determined by *which locus changed* and
*what CI says about it*. This is the truth table.

Legend:
- `G` = CI green, `R` = CI red, `?` = unknown / not yet run
- "owner alive" = `chunk_owner` in `gb news` alive set, or human edit < 1h
- Action verbs: **derive** (recompute synthetic demo), **rebase** (chunk
  on main), **bug** (file bug bead under epic), **comment** (kd comment
  on chunk bead), **crew** (dispatch chunk-fix crew), **noop**

| Trigger | main | chunk | demo | owner alive | Action |
|---|---|---|---|---|---|
| chunk MR opened | G | ? | — | — | wait for chunk CI |
| chunk CI G | G | G | — | — | derive demo; if demo G then mark chunk merge-eligible (comment) |
| chunk CI R | G | R | — | yes | comment on chunk bead; do not crew |
| chunk CI R | G | R | — | no | crew (chunk-fix) on the chunk bead |
| chunk merged | G | — | — | — | drop from chunk_order; derive demo |
| chunk review comment | — | — | — | yes | comment summary on chunk bead, ping owner |
| chunk review comment | — | — | — | no | crew on chunk bead with comment text in prompt |
| main advances | G | G | — | — | derive demo |
| main advances | G | R-on-rebase | — | yes | comment "main moved — please rebase" |
| main advances | G | R-on-rebase | — | no | rebase chunk; if conflict, bug + comment |
| demo derived | G | all G | G | — | INV1 holds; noop |
| demo derived | G | all G | R | — | bisect chunk_order: pop until green; bug naming culprit chunk; if culprit owner alive, comment; else crew |
| demo derive failed (merge conflict) | G | G | — | — | bug naming the conflicting chunk; comment on that chunk bead |
| INV3 violation (chunk N depends on chunk N+M) | — | — | — | — | bug naming both chunks; recommend reorder in comment on epic |
| epic.freeze = true | — | — | — | — | noop; squawk only |

The "demo derive failed" and "INV3 violation" rows are the high-value
ones — they are the failure modes the gardener exists to detect early,
*before* a chunk is merged into main.

## 5. MVP scope and follow-ups

### 5.1 MVP (what we ship first)

The smallest thing that exercises the contract end-to-end on one real
epic. Bias toward primitives we already have.

1. **Bead model — no new type.**
   - Add `kind:tree-hug` label to the existing label taxonomy.
   - Document the four epic fields (`demo_branch`, `base_branch`,
     `chunk_order`, `freeze`) in the epic description as YAML
     frontmatter. No schema change to kbeads.
   - `chunk_order` is just a list of bead IDs, one per chunk, in
     stack order.

2. **Gardener prompt** under `gardenio/plans/gardener-prompts/tree-hug.md`,
   following the existing template (mission KPI, liveness block, cycle,
   hand-off, wrap-up grammar, concurrency budget).

3. **Schedule bead** of `type=schedule`, role `gardener-tree-hug`, cron
   `*/15 * * * *`, prompt sourced from the file above. Wire it up via
   the existing `kd update <id> -f prompt=$(cat …)` ritual.

4. **Synthetic demo derivation** as a shell recipe in the gardener
   prompt (no new binary). Sketch:

   ```sh
   git fetch origin
   git checkout -B "$DEMO_BRANCH" "origin/$BASE_BRANCH"
   for chunk in $CHUNK_ORDER; do
     git merge --no-ff --no-edit "origin/$chunk" \
       || { echo "conflict on $chunk"; exit 1; }
   done
   git push --force-with-lease origin "$DEMO_BRANCH"
   ```

   Force-with-lease, not bare force; if someone else pushed the demo
   branch, abort and file a bug.

5. **One real pilot epic**: convert the active observability/k6
   work (multiple MRs modifying shared infra — the motivating case)
   into a tree-hug epic. Use it to shake out the matrix in §4.

6. **Wrap-up grammar** that includes `convergence_distance` delta —
   so cycle-watchdog can grade FAILED vs OK without special-casing.

### 5.2 Explicitly OUT of MVP

- New `type=tree` / `type=convergence` bead type. Per Slack consensus
  (ka-0000002617): label-on-epic now, promote later if pressure
  arises on (i) clean schema or (ii) viewer rendering.
- Viewer changes (tree-shaped status widget).
- Auto-carving chunks from demo (i.e. tree-hugger proposing the next
  chunk). Operator-driven only.
- Auto-merge of chunks. Always human or gardener-delivery.
- Cross-repo tree-hug (the pattern generalises, but MVP is single-repo).

### 5.3 Follow-up tasks (file as child beads under ka-0000003354's
deliverable epic, after this design is approved)

| Follow-up | Notes |
|---|---|
| **FU-1** Create `kind:tree-hug` label taxonomy entry. | Doc change in advice + intake prompt. |
| **FU-2** Write `gardenio/plans/gardener-prompts/tree-hug.md`. | Mirror intake.md template. |
| **FU-3** Create schedule bead, role `gardener-tree-hug`. | `*/15 * * * *`, enabled=false initially. |
| **FU-4** Pilot: convert observability/k6 work to a tree-hug epic. | Coordinate with that epic's current owner. |
| **FU-5** Enable schedule on pilot; one-week trial; report KPI. | Reviver candidate after trial. |
| **FU-6** Decide on `type=tree`. | After FU-5, based on actual pressure on schema/rendering. |
| **FU-7** Viewer affordance (optional). | Tree-shaped widget over `chunk_order`. Cosmetic. |
| **FU-8** Cycle-watchdog tweak. | Confirm wrap-up grammar parser handles `convergence_distance:`. |
| **FU-9** Trigger wiring for MR events. | If polling-only (15m) proves too laggy, add MR webhook trigger via existing bridge. |
| **FU-10** Generalise to multi-demo per epic. | Speculative; only if a real case shows up. |

### 5.4 Open questions for operator

1. **Demo branch name convention.** `demo/<epic-slug>`? `feature/<epic>-demo`?
2. **Force-push policy on demo.** `--force-with-lease` is the default;
   what if a human pushes to the demo branch by accident — bug-and-stop
   or just overwrite?
3. **Chunk-author handoff.** When `chunk_owner` goes from alive→dead
   mid-MR, do we crew-respawn the chunk or escalate to operator?
   MVP says crew-respawn after 1h, but this is the kind of policy
   that wants a real case.
4. **Pilot epic.** Confirm the observability/k6 work is the right
   first victim, or pick another.

## 6. Why this design, in one paragraph

The tree-hugger is a gardener because the work is **continuous, low-stakes
per cycle, and high-stakes in aggregate** — exactly the shape gardeners
are for. It uses **synthetic demo** because that makes its job a pure
function of inputs, with no hidden history. It uses **epic + label + fields**
because no new bead-type pressure exists yet. It scopes its writes
narrowly (own the demo, comment on chunks, file bugs, dispatch chunk-fix
crews) because the cross-lane lesson from existing gardeners is *prefer
the small thing yourself, never write code in someone else's lane*. The
**four invariants** make the KPI computable, which makes failures
detectable, which is what lets cycle-watchdog and reviver/reaper grade
it like every other gardener.
