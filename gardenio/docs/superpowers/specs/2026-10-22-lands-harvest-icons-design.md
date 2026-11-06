# Lands: what a closed step shows (design spec)

**Status:** accepted by the owner on 2026-10-22, decision gd-FLelheBzpE, option **A, full**. Step
4b.6 of epic gd-7NMvfOCjBP (4b. Refine), task gd-nsaGxscX79. The build steps in §11 are filed as
child tasks of gd-nsaGxscX79, one PR each.
**Input:** the research proposal `2026-10-22-lands-harvest-icons-research.md` (cited `R §n`,
PR #868). This spec answers its nine open questions (R §9) with the owner's chosen package. The
proposal's facts (R §1) and its icon sketches (R §8) are not repeated here.
**Neighbours:** PR #860 (`FarmPlan.produce[]` and the bin by the plot, cursor-grid spec §5.6) and
PR #872 (a bed's closed steps are cursor stops, `FarmPhase.closed[]`). Both are open and waiting
for a Fable-tier review (decisions gd-CCgr9tzsAp, gd-drDifhG1Cm). This spec builds on both, and
§10 says what changes if either is sent back.

> **The bin is retired** (plan gd-PJB3D7glwF, step gd-vDauUoOkj8; cursor-grid spec §5.6): the plot's bin and its
> highlight below are gone. `FarmPlan.produce[]` stays and is still read by the bead view, the harvest cells and the
> goats' kin; the farm's Escalations cell (`homestead-world-design.md` 28.13) took the bin's place.

Every code claim was re-read on `main` at 3c1ff53f and on the branches of #860 and #872. Paths are
relative to `gardenio/controller/internal/gardenhub/` unless they start with `gardenio/`, `kbeads/`
or `.claude/`.

---

## 0. Summary

A closed step becomes a **harvest cell**: a cursor stop with a base sprite that says how the step
ended, up to six badges that say what it proved, a status line and label row that say it in words,
and a **Harvest** section in the bead view with the full inventory.

1. **Writers** (§2). Our own tools record the quality signals as structured data. The
   `commons-ux-acceptance` skill files a `type=evidence` bead on the step. github-bridge writes
   `ci_state`, `tests_touched` and `goldens_touched` on the step at merge. The merge desk writes
   `review_rounds`. The MR fields stay where they are. Recording starts when step 1 lands, with no
   backfill.
2. **Read model** (§3). The 12 newest closed steps of each plan carry a compact `harvest` object in
   the farm document: the base and the badge facts. Older closed steps stay plain stubble, with no
   age cut-off.
3. **Cell** (§4). Bases: **sheaf** (it yielded something), **trampled** (abandoned), **bare
   stubble** (closed with nothing attached). Badges: **crate** (merged), **rosette** (UX graded),
   **photo** (pictures), **vial** (tests), **lantern** (CI green, the only badge lit at night),
   **seal** (review rounds).
4. **Alongside** (§5–§7), in build order: the bead view's Harvest section first, then the
   label's icon row, then the bin highlight once #860's step 10 lands. Cost appears only as
   words in the bead view, summed over every crew that claimed the step.

## 1. Decisions (R §9, answered)

| # | Question | Answer |
|---|---|---|
| Q1 | Evidence model | `type=evidence` beads parented to the step for the UX grade (§2.1). Flat fields on the step for `review_rounds` (merge desk) and `ci_state`, `tests_touched`, `goldens_touched` (github-bridge). The MR stays the existing `mr_*` fields. |
| Q2 | The six | crate, rosette, photo, vial, lantern, seal (§4.2). |
| Q3 | The bound | 12 `harvest` rows per plan, newest `closed_at` first, no age drop-off. Older steps stay plain stubble. |
| Q4 | Base sprite | sheaf when there is a yield, trampled when abandoned, bare stubble otherwise (§4.1). |
| Q5 | Cost | Words in the bead view only, never a badge. Summed over every crew agent that claimed the step (§6.3). |
| Q6 | Where alongside | Bead view Harvest section first (step 3), then the label icon row (step 5), then the bin highlight (step 6, after #860's step 10). |
| Q7 | Backfill | None. Steps closed before step 1 lands draw as bare stubble or a sheaf from what is already structured (MR, produce). |
| Q8 | Walking | Harvest cells are cursor stops on phone and desktop. They come after the crate, and Shift+arrow skips the run. #872 does this for every listed closed step. |
| Q9 | Tests | github-bridge counts the test files a PR touched, at merge (§2.3). |

## 2. Writers

### 2.1 The evidence bead

A new data-kind bead type, `evidence`, holds one quality record. It is created **parented to the
step** (`parent-child` edge) and closed straight away. It is data, not work, so no queue, `gb ready`
or farm count lists it.

| Field | Meaning | Example |
|---|---|---|
| `kind` | what was measured. This spec writes only `ux`. The field stays open for later kinds. | `ux` |
| `step` | the step's id again, so the farm can join without an edge read (§3.2) | `gd-dmYYfaQYuA` |
| `subject` | what was graded | `cursorphase, bead view` |
| `score` | gates passed | `5` |
| `of` | gates graded (N/A excluded) | `5` |
| `percent` | the rubric score | `100` |
| `urls` | the PNG links, newline-joined (`gb image upload` output) | `https://gardenio.clonk.tv/image/…` |
| `pr` | the PR the grade was for, when there is one | `https://github.com/…/pull/825` |

**Who and when** come from the bead itself (`created_by`, `created_at`), stamped by the server and
not taken from fields. The title is `UX grade 5/5: cursorphase`. A re-grade is a **new** evidence
bead and never an edit. The farm and the label use the newest `ux` bead for a step. The bead view
lists every one, newest first.

Registration: `TypeEvidence BeadType = "evidence"` in `kbeads/internal/model/bead.go` next to
`TypeReport`. It has no `write_policy`, so any authenticated caller may create one. Step 1 checks
that a crew pod's token scopes (`scopedWriteDenial`) allow the type and adds it if they do not.

### 2.2 The skill writes it

`.claude/skills/commons-ux-acceptance/SKILL.md` §5 "Attach the evidence" gains one command after
the comment (the comment stays, because people read it):

```
kd create "UX grade 5/5: cursorphase" --type evidence --parent <step> \
  -f kind=ux -f step=<step> -f subject=cursorphase \
  -f score=5 -f of=5 -f percent=100 -f urls=@urls.txt [-f pr=<url>]
kd close <evidence-id>
```

The grade is filed on the **step** the surface belongs to, which is the task the crew holds and
not the PR (`--parent` defaults to `$BOAT_TASK_ID`, the crew's own task). The summary line
the skill prints gains `evidence: <evidence-id>`.

### 2.3 github-bridge at merge

When a PR merges, `cmd/github-bridge/pullrequest.go` already stamps `mr_state=merged` on the
producing tasks (`forgebeads`, `ProducingTaskBeadsForMR`). In the same pass it writes, on each
producing task:

| Field | Value | Source |
|---|---|---|
| `ci_state` | `success`, `failed`, or whatever else the MR bead's `mr_pipeline_status` holds at merge | the MR bead, which `pipeline.go` already keeps current |
| `tests_touched` | the number of files in the PR whose path is a test: `*_test.go`, `*_test.rs`, `*.test.js`, `*.spec.ts`, `testdata/*_browser.js`, or anything under `tests/` except `tests/**/golden*/` | one paged `GET /repos/{o}/{r}/pulls/{n}/files` |
| `goldens_touched` | files under a `golden/` or `goldens/` directory | the same list |

The bridge writes zero as `0`, so "no tests" can be told apart from "not recorded". A failed files
read writes neither count, logs the error, and never fails the merge handling. The GitLab path is
out of scope: no GitLab project is live.

### 2.4 The merge desk at merge

The merge desk gardener (trigger gd-WksO1FSmvV) writes `review_rounds` on the step when it merges:
1 for a first-time merge, plus one for each time it sent the PR back on this step. A send-back
leaves a desk comment and a re-filed merge escalation, so the desk counts its own escalations on the
step. The change is to the gardener's prompt bead, not to code. Step 1 makes the edit with the
operator's approval if the prompt bead is operator-owned.

## 3. Read model

### 3.1 The `harvest` object

A closed step's row in `FarmPhase.closed[]` (#872) gains an optional `harvest` object. Zero and
empty fields are left out.

```json
{"id":"gd-dmYYfaQYuA","title":"2b.5b plot rollup stamp","state":"closed","updated":1792600000,
 "harvest":{"closed":1792600000,"base":"sheaf","mr":"#825","ux":[5,5],"pics":9,
            "tests":3,"ci":"success","rounds":2}}
```

| Key | From | Drives |
|---|---|---|
| `closed` | kbeads `closed_at` (unix s) | ordering, `2d ago` |
| `base` | §4.1 rule: `sheaf`, `trampled`, or absent for bare | base sprite, the first status word |
| `mr` | `mr_state=merged` plus the forge's number (`#825` on GitHub, `!825` on GitLab) | crate |
| `ux` | newest `kind=ux` evidence bead: `[score, of]` | rosette |
| `pics` | that bead's URL count plus `goldens_touched` | photo |
| `tests` | `tests_touched` when it is above 0 | vial |
| `ci` | `ci_state` | lantern when `success` |
| `rounds` | `review_rounds` | seal |

The page derives the badges from these keys (§4.2). The document sends facts, not a bitmask, so
the status line and the label have their numbers without a second fetch.

### 3.2 How it is read

- **Which rows.** For each plan, take the 12 newest closed steps by `closed_at` across all its
  phases. Only those rows carry `harvest`. Every other listed closed row from #872 stays a plain
  `state: "closed"` row: still a cursor stop, drawn as bare stubble. The count of the plan's closed
  steps past 12 that #872 lists without `harvest` is never sent as its own field. `counts.closed`
  already covers it.
- **Evidence.** The slow tier adds one list: `type=evidence`, every status, newest first, capped
  at 600 rows like #860's produce lists. Rows are joined to steps by the `step` field. A row whose
  step is not among a plan's 12 is dropped.
- **Yield for the sheaf** joins #860's `produce[]` rows by `task` (a bundle or report hung off the
  step counts as a yield).
- The step's own fields (`mr_state`, `ci_state`, `tests_touched`, `goldens_touched`,
  `review_rounds`, `close_reason`) are on the closed row #872 already reads, so there is no extra
  call.
- A failed evidence read keeps the last evidence index and never fails the refresh (as #860 does).

### 3.3 Bounds (add to `gardenio/docs/homestead-farm-read-model.md`)

| Bound | Value |
|---|---|
| `harvest` rows per plan | 12 |
| evidence rows read per slow tick | 600 |
| bytes per `harvest` object | ≤ 120 |
| added payload, 36-plan fixture | ≤ 432 × 120 B ≈ 52 KB raw, inside the 300 KB budget test |
| added payload at 10x | ≈ 520 KB worst case, so step 2 must re-measure the 700 KB cap and cut `pics`/`rounds` first if it does not fit |

## 4. The cell

### 4.1 Base sprite

The base is decided in this order. The first rule that matches wins.

1. **Trampled** when the step was abandoned: `close_reason` starts with `wontfix`, `abandon`,
   `duplicate`, `obsolete` or `supersede` (case-insensitive), or the step's MR was closed without a
   merge and there is no other yield. Word: `abandoned`.
2. **Sheaf** when there is a yield: `mr` is set, or #860 lists a produce row for the step, or an
   evidence bead exists. Word: `harvested`.
3. **Bare stubble** otherwise, including every closed row without `harvest`. Word: `closed`.

Sprites follow R §8: the sheaf is the stubble stalks drawn taller and leaning in, with a `Y` band at
mid height and a `Z` shadow. Trampled is the stalks bent flat, in `Z` and `T` only. Both are 16×16
row maps in `art.js` next to `S.stubble`, picked in `beadRows` the way `s: "done"` picks the
stubble today.

### 4.2 Badges

| Badge | Lit when | Word | Digit at 2x+ |
|---|---|---|---|
| crate (merge glyph, as §5.6) | `mr` | `merged #825` | none |
| rosette | `ux` | `graded 5/5 gates` | gates passed |
| photo | `pics` > 0 | `9 pictures` | none |
| vial | `tests` > 0 | `3 tests` | none |
| lantern | `ci == "success"` | `checks green` | none |
| seal | `rounds` ≥ 1 | `merged first time` / `merged after 2 rounds` | rounds when ≥ 2 |

- A `ci` that is not `success` lights no badge. Its word, `checks failed`, goes in the status line.
- Badges are 5×5 marks in the cell's top-right, composed as extra 16×16 layers in `beadRows` the way
  `S.flag` and `S.frost` are. A cell shows **2 badges at 1x and 4 at 2x and above**. When there is
  no room, they are kept in priority order: crate, rosette, vial, lantern, photo, seal. The label row
  and the bead view always show all six.
- Night: the night palette dims every badge except the lantern's `G` flame, so at night a lit
  lantern marks the green-CI steps.
- Phone: the cell draws the base and the 1x badge budget. The label row and the bead view carry the
  rest.
- Plan depth (level 2) and project depth (level 1) do not change. The flat square stays bare, as
  #872 keeps it, and the rollup stamp is untouched.

### 4.3 Rules the art must pass

As in R §1.6. State is never colour alone: every badge has its word in the hot's accessible name and
in the status line. No surface shows more than 6 distinct glyphs (C7). Every badge needs the C5
light/dark pass at 390 and 1440 px. No emoji anywhere. `garden.js` is at 1,374 of its 1,500-line
quench cap, so the art and its composition live in `art.js` and `garden.js` changes only where it
calls `beadRows`.

## 5. Status line and label row

- **Status line** (cursor-grid spec §6.4, new row for a harvest cell): words that matter first,
  and a part with nothing to say is left out:
  `harvested · @who · 2d ago · merged #825 · graded 5/5 gates · 3 tests · checks green · merged after 2 rounds`.
  `@who` is the step's last assignee. A closed row without `harvest` keeps #872's `closed · @who · 3d`.
- **Label icon row** (step 5). The full label of a harvest cell gains a second line of
  `icon word` pairs. Each icon is a 12 px canvas drawn by `art.js` with `alt=""`, because the word
  beside it is the name. The line is capped at the plate's 28 ch on a phone and 40 ch on a desktop.
  Pairs that do not fit are dropped from the end in the §4.2 priority order, followed by a `+N`
  word.

## 6. The bead view: Harvest section (step 3)

### 6.1 Payload

`GET /garden/bead/{id}` (`h.beadView`) gains a `harvest` block when the bead is a closed work bead.
The block is built on request, not in the farm tick:

| Part | Read |
|---|---|
| evidence | the step's `type=evidence` children, newest first (all of them, re-grades included) |
| MR | the step's `mr_*` fields |
| CI, tests, goldens, rounds | the step's fields from §2.3 and §2.4 |
| produce | bundle/report children (the kinds #860 reads) |
| follow-ups | open and closed children that are work beads: count and titles |
| cost | sum of `total_cost_usd` over the agent beads whose `task_id` is the step, follow-through crews included. Words: `$0.29 across 2 crews` |
| lead time | `created_at` to `closed_at`: `closed in 2d` |
| wrap-up | the newest of those agent beads' `wrapup.tldr` |

### 6.2 Layout

The Harvest section comes first, above Description, on a closed step:
1. One headline line: the base word, then the badges as icon and word pairs (§4.2 order).
2. **Pictures**: thumbnails of the newest grade's URLs through the image proxy, each one a link.
   On a phone they are a 3-up grid at 44 px or larger.
3. **Evidence** rows: every grade with `by`, `at`, `subject` and a link. CI, tests and rounds as
   words.
4. **Metrics** in words: lead time, then cost.
5. **Produce and follow-ups**: one line each, linking to the produce beads and the children.
6. The crew's wrap-up `tldr`.

The section shows for any closed step, however it was reached (`?bead=`, the outline, a link), not
only from the map.

## 7. The bin highlight (step 6)

When the cursor's Thing is a harvest cell, the plot's bin (#860's §5.6, build step 10,
gd-LohQMNM3wU) highlights its rows whose `task` is that step. Peek and go are reused. Nothing new
is drawn when the bin has no row for the step.

## 8. Tests

| Step | Pins |
|---|---|
| 1 | github-bridge: a merged-PR fixture with 3 test files and 2 goldens writes `tests_touched=3`, `goldens_touched=2`, and `ci_state` from the MR bead. A failed files read writes neither count. kbeads: `evidence` creates and closes under a crew-scoped token. |
| 2 | `farm_test`: 12 per plan (a 14-closed-step plan sends 12 `harvest` objects, newest first), the evidence join by `step`, the sheaf/trampled/bare rules, the budget test and the fixture golden regenerated. |
| 3 | `bead.go` test for the `harvest` block (evidence order, cost summed over two agents). Browser test: the section is first on a closed step and absent on an open one. Goldens re-shot. |
| 4 | `page_test` `beadRows`: base by rule, badge priority at 1x (2) and 2x (4). Night palette: only the lantern stays lit. Goldens re-shot. |
| 5 | `cursor_browser.js`: the status line words in order with zero parts left out. Label row truncation with `+N`. |
| 6 | The bin highlights the step's rows. Final `/commons-ux-acceptance` C1–C12 on `cursorphase` and the bead view, owner screenshots in the thread, HW build notes. |

Every step that changes a surface runs `/commons-ux-acceptance` on that surface and attaches the
before and after PNGs to its PR. It also files an evidence bead on its own step, so 4b.6 is the
first harvest.

## 9. Not in this spec

- Badges for bundles, reports, follow-ups and decisions (R §3 rows 7 to 9). These stay in the bead
  view's inventory.
- The level-2 phase status line counts (`7 merged · 5 graded`, R §6). They would be inexact past
  the 12-row bound. Revisit once the bound is known to be enough.
- Mining old comments for grades (R §4 C). Q7 says no backfill.
- GitLab writers, since no GitLab project is live.

## 10. If a neighbour PR is sent back

- **#872 sent back:** step 2 adds `FarmPhase.closed[]` itself, but only for the 12 `harvest` rows
  per plan. The rest of `counts.closed` stays anonymous, unwalkable stubble, as R §5 O2 proposed.
  The cursor node work #872 does (`kind: "task"`, `state: "closed"`, after the crate) moves into
  step 2.
- **#860 sent back:** the sheaf rule drops its produce clause and step 6 is not built. Steps 1 to
  5 do not change.

## 11. Build steps

| Step | What | Files | Depends on |
|---|---|---|---|
| 1 | Writers: `evidence` type; the skill files evidence; github-bridge writes `ci_state`, `tests_touched` and `goldens_touched` at merge; the merge desk prompt writes `review_rounds` | `kbeads/internal/model/bead.go`, `.claude/skills/commons-ux-acceptance/SKILL.md`, `cmd/github-bridge/pullrequest.go` (+ test), trigger gd-WksO1FSmvV's prompt | this spec |
| 2 | Read model: `harvest` on the 12 newest closed rows per plan; the evidence list; docs and fixture | `farm_harvest.go` (new), `farm.go`, `farm_model.go`, `farm_test.go`, `testdata/farm_fixture.json`, `gardenio/docs/homestead-farm-read-model.md` | #860, #872, 1 (field names) |
| 3 | Bead view Harvest section and the `/garden/bead/{id}` `harvest` block | `bead.go` (`beadView`), `web/bead.js`, goldens | 1 |
| 4 | Art and cells: sheaf, trampled and six badges in `art.js`; `beadRows` layers; the 1x/2x badge budget; goldens | `web/art.js`, `web/garden.js` (call site only), `page_test.go` | 2 |
| 5 | Status line row and label icon row | `web/cursor.js`, `web/world.js`, `testdata/cursor_browser.js`, cursor-grid spec §6.4 | 4 |
| 6 | Bin highlight; final C1–C12 acceptance; owner screenshots; HW build notes | `web/bin.js` (from #860 step 10), docs | 5, gd-LohQMNM3wU |

## 12. Risks

- **Two neighbour PRs are unmerged.** Steps 2, 4 and 5 touch the same files as #860 and #872, and
  as #867, which re-shoots the same `cursorphase` goldens. Step 2 does not start until both have
  merged or been sent back (§10).
- **The 10x payload.** See §3.3. Step 2 measures it, not this spec.
- **Self-grading.** The crew that built a surface grades it. The evidence bead records who graded
  and when, and the merge desk's seal is an independent signal, but this does not make the grade
  independent. The bead view shows `by` for that reason.
- **The merge desk is a prompt.** `review_rounds` depends on an LLM gardener following its
  prompt. If the field is missing on merged steps a week after step 1, move the count into
  github-bridge (count the step's merge escalations at merge).
- **Hot cap.** Level 3's `#hots` cap of 300 grows by the harvest cells in view. #872 already adds up
  to 81 per bed, so step 4 re-checks the frame budget against `world_tenx.js` with closed rows in
  it.
