# Lands: what a closed step shows (research proposal)

> **Status:** research, not a spec. Written for the owner to turn into a spec with the thread
> (Slack thread gd-fxx6PMK9ME, bead gd-YGObmRzseg). Every fact below about the code was read from
> `main` at 95c35ac9 plus PR #860's branch on 2026-10-22; the proposals are proposals.
>
> **The bin is retired** (plan gd-PJB3D7glwF, step gd-vDauUoOkj8; cursor-grid spec §5.6): the plot's bin and its
> highlight below are gone. `FarmPlan.produce[]` stays and is still read by the bead view, the harvest cells and the
> goats' kin; the farm's Escalations cell (`homestead-world-design.md` 28.13) took the bin's place.

The ask, in the owner's words: "time to get more interesting with the whole 'closed step' graphic
in homestead/lands. We've left it as just stubble, which is very apt and all; but a variety of
icons associated with created artifacts that we can use to show quality / proof / metrics / etc
should be present, and when the tile is selected should be displayed alongside."

## 0. Summary

- Today a closed step is **not a thing**. The farm document sends no closed bead as a row
  (`homestead-farm-read-model.md`, Bounds: "Closed beads sent as rows: 0"); a phase carries
  `counts.closed`, a number. `planBeds` turns the number into N stubble sprites, `cursor.js` makes no
  node for them, so a stubble cell has no id, no label, no A, and nothing to show alongside.
- The artifacts a step produced **do exist**, scattered: the MR link and state as fields on the
  step; the UX acceptance grade, the goldens and the merge desk's verdict as free-text comments;
  bundles and reports as beads of their own type; cost and the wrap-up on the crew's agent bead;
  the close time and reason in kbeads. Only the MR, the documents, the children, cost and the
  times are structured. Quality signals (UX gates passed, review rounds, tests, CI) are prose.
- PR #860 (in the merge queue, gd-811k2dTxtr) is the nearest neighbour: `FarmPlan.produce[]`
  lists merged and expected MRs, bundles and reports **per plan**, each naming the step it hangs
  off, and spec §5.6 puts them in a bin by the plot. It reaches closed steps through a
  closed-ancestor edge cache. This work should build on it, not beside it.
- The proposal is three changes, each small enough to spec on its own: (1) make the quality
  signals structured where they are written; (2) carry a bounded list of **harvest** cells per
  phase in the farm document so a stubble cell has an id and an icon mask; (3) make the stubble a
  cursor stop whose sprite carries up to a few badges, with the full inventory in the bead view
  and a compact icon row on the full label.
- The open questions for the spec session are in §9. The main ones: evidence as beads or as
  fields; which six icons earn a place on a 16 px cell; the per-plan bound; whether cost belongs
  on a tile at all.

## 1. What exists (the facts)

### 1.1 The stubble today

| Where | What is drawn | Source |
|---|---|---|
| `art.js` `S.stubble` | three cut stalks (`p` tip, `Y`, `Z` shaft) on a 16×16 sprite; `beadRows` picks it for `s: "done"` | `art.js:132`, `art.js:355` |
| `world.js` `planBeds` | a bed lists the open tasks, then the `+N` crate, then `stubble: min(counts.closed, 81 − stops)` anonymous cells from `stubbleAt` | `world.js:292` |
| `garden.js` level 3 | `bmp('stub')` per stubble cell, a cached 16×16 bitmap of `drawBead({s:'done', wild:true})` | `garden.js:351` |
| `art.js` level 2 | the flat 1-tile square for done ones, scaled | `art.js:682` |
| level 1 | nothing per step; the rollup stamp shows `done / total` fill and a tick on a closed phase | spec §6.6 |
| `cursor.js` | tasks and the crate get nodes; stubble gets none, so it is skipped by the arrows and has no label, status line or hot | `cursor.js:206` |
| status line | the phase says `4/9 steps`; a task says `growing · @who · 2h`; no row exists for a closed step | spec §6.4 |

Cursor-grid spec §5.4 says stubble follows the tasks; build note 26.1 moved the crate in front of
it so the cursor walks tasks then crate. Nothing after the crate is walkable.

### 1.2 The farm document has no closed steps

`FarmTask`: "Closed beads are never tasks." A closed phase is listed with `tasks: []` and
`counts.closed` = its pile. The budget test pins a 36-plan farm under 300 KB raw, and the slow tier
already reads closed counts, closed phase titles and the closed-plans list (24 newest) every 20 s.
So the closed world is counted, never named. That is the design choice this proposal revisits,
narrowly.

### 1.3 What a real closed step carries

`kd show gd-dmYYfaQYuA` (2b.5b, the plot rollup stamp, PR #825), which is a typical crew step:

| Evidence | Where it is | Structured? |
|---|---|---|
| the pull request | field `mr_url` (plus `forge`; the forge bridge writes `mr_state`, `mr_title`, `mr_merged` on the step) | yes |
| merged, by whom, which sha | merge desk comment "Merged as c0562a04…"; the `gitlab_mr_merge` escalation beads (two: one send-back, one merge) on `blocks` edges | half: the escalations are beads with results, the sha is prose |
| review rounds | merge desk comment "PR #825 sent back. Required: …", then a second escalation | prose; countable as escalations |
| UX acceptance | a comment: scoped grade, "5 of 5 graded gates PASS, C6 N/A", a gate table, "Score … 100 %", nine before/after PNG URLs on `gardenio.clonk.tv/image/…` | prose |
| goldens re-shot | listed in the same comment and in the PR's files | prose |
| tests | `TestPlotRollup` named in the description; `_test.go` files in the PR | not on the bead |
| CI | "CI green on …" in merge desk comments | not on the bead |
| follow-ups filed | `Children: 2` | yes (edges) |
| who did it, what it cost | the crews' agent beads: `total_cost_usd: 0.29`, `litellm_spend_*`, `wrapup` JSON (`tldr`, `accomplishments`, `pull_requests`, `handoff_notes`) | yes, on another bead, joined by assignee or by `pull_requests` |
| when, how it closed | kbeads `closed_at`; label `close-on-escalation-execute`; `close_reason` on thread beads | yes |
| documents | `type=bundle` (plan narratives) and `type=report` ("Report (plan) for …") beads under plans and steps | yes (#860 reads them) |

So the proof-of-shipping and the produce are structured; the **quality** signals, which is what
the owner wants to see, are prose written by our own tools (the `commons-ux-acceptance` skill,
the merge desk, the forge bridge). That is the lever: they are ours to change.

### 1.4 PR #860: the bin by the plot

`FarmProduce` is `{id, kind: mr|bundle|report, title, url, iid, state, ready, task, phase,
updated}`, 24 per plan, not-ready first, read in the slow tier from two lists (work beads with an
`mr_url`, and bundle/report beads, 600 rows each, every status) walked up to their plan through a
cached closed-ancestor edge read. Spec §5.6 draws it as a DOM overlay by the plot with four 16 px
icons (crate with a merge glyph, grayed expected crate, sheaf, sheet), peek on hover, go on Enter,
and a 5×5 crate mark with a digit on the plot's fence. Build steps 10 and 11 are filed
(gd-LohQMNM3wU, gd-E7jGutigmx) under the cursor world epic gd-7bfFegkYjS.

What it gives this work for free: the closed-ancestor cache, the per-step `task` and `phase` on
every produce row, three icons and their state words, and the peek/go interaction. What it does
not give: a closed step with no produce is still invisible; the bin is per plan, not per cell;
quality is not a kind it knows.

### 1.5 The bead view

`web/bead.js` renders any bead: meta, a "Pull request" button from `mr_url`/`pr_url`, Description,
Notes, relation sections, comments newest first. On a desktop it docks beside the world; on a phone
it is a sheet. It already opens on A for every cursor Thing. This is the obvious "alongside" for the
full inventory; it needs a section, not a new surface.

### 1.6 Rules the icons must obey

- State is never colour alone (`HW §6` rule 1, rubric C9): every badge is a shape with a word in
  the accessible name and the status line.
- Glyphs are few: at most 6 distinct per surface, none reused for two states, each glued to its
  word in text (rubric C7).
- Light and dark from the token palette (rubric C5). The rollup stamp learned that `Y` dims to
  grey at night; `G` (`#ffe680`, emissive) is the only colour that stays lit.
- The 24 px legibility rule from the badge art spec ([`badge-art-spec.md`](../../badge-art-spec.md)): one bold subject, no fine detail. A 16 px
  tile cell is smaller than that, so a badge on a cell is a 5×5 or 6×6 mark, not a picture.
- Touch targets 44 px under `pointer: coarse`; the cell is 16 px at 1x, so phone selection goes
  through the pad and the cursor, as every task does today.
- `garden.js` is 1,277 lines under a 1,500 quench cap; `art.js`, `cursor.js` and `garden.js` are
  being touched by PRs #861, #865, #867 and by #860's build step 10.

## 2. The gap in one sentence

A closed step is a count in the document and a skipped cell in the cursor model, and the
artifacts that would decorate it are prose in comments; naming the step, structuring the prose
and giving the cell a sprite and a label row are three separable changes.

## 3. Evidence inventory: what an icon could stand for

Candidates, graded by what proves it and how it is found. "Source" is today; "writer" is who
would make it structured.

| # | Artifact | Proves | Source today | Writer to change | Candidate icon (16 px, palette letters) | State words |
|---|---|---|---|---|---|---|
| 1 | merged MR | it shipped | `mr_url`, `mr_state=merged` | none (exists) | the §5.6 crate with the merge glyph (`b`/`B`, two `l` dots on a `k` line) | `merged !825` |
| 2 | UX acceptance grade | quality, with pictures | the skill's comment: gates PASS/FAIL, score, PNG URLs | `commons-ux-acceptance` skill | a **rosette** (county-fair ribbon: `e` ribbon tails, `Y` disc, `j` centre) with a pixel digit for gates passed at 2x and up | `graded 5/5 gates`, `graded 4/6 gates` |
| 3 | goldens / screenshots | proof you can look at | PNG URLs in the same comment; PNG files in the PR | the skill (`evidence[]` URLs); forge bridge (`goldens_touched`) | a **photo** (a `j` sheet with an `x` sky block and a `l` ground line) | `6 pictures` |
| 4 | tests | proof it is pinned | test names in prose; `_test.go` in the PR files | forge bridge, counting test files touched on merge | a **vial** (`q` glass, `w` liquid, `k` rim) | `3 tests` |
| 5 | CI | the checks ran green | merge desk prose | forge bridge (`ci_state` on the step at merge) | a **lantern** (`G` flame, `k` frame; the one emissive badge, so it reads at night) | `checks green` |
| 6 | review rounds | how hard it was to land | merge desk send-back comments; the count of `gitlab_mr_merge` escalations on the step | merge desk (`review_rounds`) | a **seal** (`e` wax, `j` highlight), one for a first-time merge, with a digit past that | `merged first time`, `merged after 2 rounds` |
| 7 | bundle / report | a document came out | `type=bundle` / `type=report` beads (#860) | none (exists) | the §5.6 sheaf and sheet | `bundle`, `report` |
| 8 | follow-ups filed | it seeded more work | child edges | none (exists) | a **seed packet**, or no badge: the nursery already draws pots for seeds | `2 seeds filed` |
| 9 | decisions answered | the owner was asked | closed `type=decision` children | none (exists) | the existing signpost pile (`S.pile`) | `1 decision` |
| 10 | cost | a metric | `total_cost_usd` on the crews' agent beads (sum over agents that claimed the step; follow-through crews count) | farm join, or the scheduler writes `cost_usd` onto the step at close | **words only**, not an icon (a coin on a tile reads as a value judgement; see §9 Q5) | `$0.29` |
| 11 | lead time | a metric | `created_at` → `closed_at` | none (exists) | words only | `closed in 2d` |
| 12 | how it closed | the harvest's nature | `close_reason`, `close-on-escalation-execute`, `plan_state` | none for merges; `kd close --reason` for hand closes | the **stubble itself**: cut stalks for a merge, trampled stalks (`Z` bent flat) for abandoned / wontfix, bare stubble for a hand close with nothing attached | `harvested`, `abandoned` |

Six are enough for a cell, by rubric C7: 1 crate, 2 rosette, 3 photo, 4 vial, 5 lantern, 6 seal.
Rows 7 to 9 already have sprites elsewhere and belong to the inventory list. Rows 10 and 11 are
metrics and belong in the status line, in words. Row 12 changes the base sprite, not a badge.

## 4. Where the evidence comes from: three sources, by cost

- **A. Already structured.** MR fields, bundle/report beads, children, `closed_at`, agent cost.
  Read-model work only.
- **B. Structured by changing our own writers.** The acceptance skill, the merge desk and the
  forge bridge write the quality rows today as prose. Each would write a record instead (and keep
  the prose comment, which humans read). Three small changes. How to write it is the first spec
  question:
  - (i) flat fields on the step (`ux_score`, `ux_gates_pass`, `ux_gates_total`, `ux_evidence`,
    `review_rounds`, `ci_state`, `tests_touched`, `goldens_touched`). Simplest; the farm reads
    them off the row it already has. Fields are strings, so URL lists are joined.
  - (ii) one `evidence` JSON field appended by every writer. Compact, but three writers appending
    to one field is a clobber waiting to happen.
  - (iii) **evidence beads**, `type=evidence`, one per record, parented to the step:
    `{kind, score, of, url(s), by, at}`. Fits the model the farm already uses for bundles and
    reports (#860 reads those by type), shows in the step's relations in the bead view for free,
    is auditable (who graded what, when), and a re-grade is a second bead, not an overwrite.
    Costs a daemon list per refresh (bounded like the produce lists) and a few hundred bytes a
    bead. Recommended, with (i) for the MR because it is already there.
- **C. Mining the prose.** Parsing "5 of 5 graded gates PASS" and PNG URLs out of comments.
  Brittle, cannot be pinned by the fixture tests, and every writer change breaks it. Rejected,
  except as a one-time backfill script if old steps should not stay bare (§9 Q7).

## 5. Read model: how a closed step gets a name

Options for the document, against the Bounds table (36 plans under 300 KB raw; 10x under 700 KB):

- **O1. Ride #860's produce.** Add kinds (`ux`, `tests`, `ci`, `review`) to `FarmProduce` and let
  the page group rows by `task` to decorate cells. Cheapest; no new list. But a closed step with no
  produce is still unnamed, stubble stays a count, and the 24-per-plan bound is shared between
  produce and evidence.
- **O2. A `harvest[]` list per phase** (recommended). `{id, title, closed (unix s), reason,
  who, badges}` where `badges` is a small array of kinds (or a bitmask) and the rest of the
  evidence stays in the evidence beads for the bead view to fetch. Read in the slow tier from one
  list of closed work beads, newest first, walked to its phase through #860's closed-ancestor
  cache (closed is terminal, so the walk caches well). Bounded **per plan**, not per phase, so a
  busy plan cannot blow the budget: 12 newest per plan by default, `harvest_more` for the rest
  (the phase's bead view lists all). Cost at 36 plans: at most 432 rows × ~90 B ≈ 40 KB raw; at 10x,
  4,320 rows ≈ 390 KB, inside the 700 KB cap. The older closed steps past the bound stay anonymous
  stubble, as today, so nothing regresses.
- **O3. Fetch on select.** The page asks for the bead when a stubble cell is selected. Already how
  the bead view works, and it is the right path for the **full** inventory (pictures, scores,
  cost, the wrap-up). It cannot decorate unselected cells, and a cell needs an id first. Pair with
  O2, not instead.

Bounds to add to the read-model doc: harvest per plan (12), closed-work list rows read per tick
(600, like produce), badges per row (6). The slow-tier cost grows by one list and reuses the
ancestor cache.

## 6. The cell, by depth

- **Bead depth (level 3, 16 px tiles).** The stubble stays the base; it is apt, and the owner said
  so. Two proposals on top, for the spec to pick between or combine:
  - **Badges.** Up to two 5×5 marks in the cell's top-right at 1x (a 16 px cell holds no more while
    the stalks still read), up to four at 2x and above, composed as extra 16×16 layers in
    `beadRows` the way `S.flag` and `S.frost` are. Priority when space is short: crate, rosette,
    vial, lantern, photo, seal.
  - **The sheaf.** A closed step **with** produce draws a bound sheaf (stalks tied with a `Y`
    band) instead of loose stubble; bare stubble means closed with nothing attached; trampled
    stubble means abandoned (§3 row 12). The base sprite then says "there was a yield" from across
    the bed before any badge is read, which is the glance the owner asked for.
- **Plan depth (level 2, flat squares).** No badges: the cells are 1 tile and flat. The phase's
  status line grows instead: `9/9 steps · 7 merged · 5 graded`.
- **Project depth (level 1).** Nothing new on the stamp; #860's crate mark with the ready count is
  the plan's yield at this depth.
- **Phone.** The cell shows the base sprite only; the label row (§7) and the bead view carry the
  rest.

## 7. Selection: the stubble becomes a Thing

- `planBeds` emits `harvest` cells after the crate, one per `harvest[]` row, newest first, then
  anonymous stubble for the remainder of `counts.closed` as today. `cursor.js` makes a node per
  harvest cell (`kind: "harvest"`, `open: id`), so the arrows reach it, Shift+arrow jumps over the
  run, and the hot carries the words. The anonymous remainder stays unwalkable.
- **Status line** (a new row for spec §6.4): `harvested · @who · 2d ago · merged !825 · 5/5
  gates · 3 tests`, need words first as always, zero parts left out. Cost and lead time are words
  here if the spec wants them (§9 Q5).
- **Alongside**, three places, cheapest first:
  - **The full label.** The sign already is the one text object for the cursor's Thing; a closed
    step's label gains a second line of `icon word` pairs (the badge at 12 px as an inline canvas
    or `<img>` from `art.js`, never an emoji: rubric C7), capped at the plate's 28/40 ch. Diegetic
    where free (spec §6): no new overlay.
  - **The bead view.** A **Harvest** section for a closed step: every evidence row with its icon,
    state word and link, the pictures as thumbnails through the image proxy, the metrics in words
    (cost, lead time, rounds), and the crew's wrap-up `tldr`. This is where "quality / proof /
    metrics" are read in full. It also shows for a closed step reached from anywhere else
    (`?bead=`), not only from the map.
  - **The bin** (#860). When the cursor's Thing is a harvest cell, the bin highlights the
    rows whose `task` is that step, reusing peek and go. Free once build step 10 lands.

## 8. Icon vocabulary (draft, for the spec)

All 16×16 in `art.js` row maps, composed into the bead sprite at 5×5 (badge) or drawn whole in the
label row and the bead view. Letters are the `DAY` palette; the night palette dims all but `G`.

| Icon | Rows (sketch) | Day letters | Night note |
|---|---|---|---|
| crate + merge glyph | §5.6's | `b` `B` `k` `l` | as §5.6 |
| rosette | a 5-wide `Y` disc, `j` centre dot, two `e` tails below | `Y` `j` `e` | `Y` dims; the `j` centre carries it |
| photo | `j` sheet 6×7, a 3×2 `x` block top, 1 px `l` line bottom | `j` `x` `l` `k` | `j` stays bright |
| vial | `q` outline, `w` fill in the lower half, `k` rim | `q` `w` `k` | `w` is blue in both |
| lantern | `k` frame 4×6, `G` 2×2 flame | `k` `G` | the only emissive badge |
| seal | `e` 5×5 disc, `j` 1 px highlight, `E` rim | `e` `E` `j` | `e` keeps contrast at night |
| sheaf (base) | the stubble stalks drawn tall and leaning in, a `Y` band at mid height, `Z` shadow | `p` `Y` `Z` | as stubble |
| trampled (base) | the stalks bent flat, `Z` only | `Z` `T` | reads dark |

Contrast is checked by `TestPaletteTextContrastAA` for text; the badges need the C5 pass of
`/commons-ux-acceptance` at 390 and 1440, light and dark, which is where the stamp's `Y`
problem was caught.

## 9. Open questions for the spec session

1. **Evidence model.** Evidence beads (§4 B iii), flat fields (B i), or both? Who may write one
   (the skill runs in a crew; the merge desk and forge bridge are the bridge)?
2. **The six.** Which badges earn the cell: the proposed crate, rosette, vial, lantern, photo,
   seal? Or fewer, with the rest as words?
3. **The bound.** Harvest rows per plan in the document (12? 24?), and whether steps older than
   some age (30 days, like the memo) drop back to anonymous stubble.
4. **The base sprite.** Sheaf for a step with produce, trampled for abandoned, stubble for bare?
   Or stubble always and badges only?
5. **Cost.** Is `$` a signal to show on a tile at all? If yes, whose: the sum over every crew that
   claimed the step (follow-through crews included), or the first? Proposal: words in the bead
   view only, never a badge.
6. **Where alongside.** Label row, bead view Harvest section, bin highlight: all three, or the bead
   view first and the label row after a look at it?
7. **Backfill.** Writers start recording from the day the spec lands; old steps stay bare stubble.
   Or a one-off comment-mining backfill for the current plans?
8. **Walking.** Are harvest cells cursor stops on a phone too (a 9×9 bed of 60 stubble is a long
   walk; Shift+arrow skips it), and do they come before or after the crate?
9. **Tests and CI.** Is "tests touched" from the PR's file list a signal worth the forge-bridge
   change, or is CI green enough proof that the tests ran?

## 10. Build shape, if the spec follows the recommendation

| Step | What | Files | Depends on |
|---|---|---|---|
| 1 | Evidence writers: the acceptance skill files an evidence bead (gates, score, URLs); the merge desk writes `review_rounds` and `ci_state`; the forge bridge counts tests and goldens at merge | the skill, `bridge/`, kbeads type registration if `evidence` is new | the spec |
| 2 | Read model: `harvest[]` per phase, bounded per plan, through the closed-ancestor cache; doc and fixture golden | `farm_harvest.go` (new), `farm_model.go`, `farm.go`, `homestead-farm-read-model.md` | #860 merged |
| 3 | World and cursor: harvest cells in `planBeds`, nodes and the status line row in `cursor.js`, `TestCursorUnderNode` | `world.js`, `cursor.js`, spec §5.4 and §6.4 | 2 |
| 4 | Art: sheaf, trampled, six badges, the layers in `beadRows`, the label-row renderer; goldens re-shot | `art.js`, `garden.js` (small) | 3 |
| 5 | Bead view Harvest section; the bin highlight | `bead.js`, `bin.js` | 1, 2, #860 step 10 |
| 6 | `/commons-ux-acceptance` C1–C12 on `cursorphase` and the bead view; owner screenshots in the thread; `HW` build notes | docs | 4, 5 |

Risks: the farm budget test (step 2 must add its own fixture rows and stay under the caps); the
`#hots` cap of 300 at level 3 grows by the harvest cells in view; three in-flight PRs on the same
files; a quality signal that is written by an agent about its own work (the skill grades what the
crew built), which the seal and the merge desk's record balance but do not remove.

## 11. A prototype before the spec?

`gardenio/docs/prototypes/homestead/plot-rollup-options.html` settled the rollup stamp by showing
options at 1x, 2x and 4x in both skies before any code. The same page for the closed cell, with
stubble / sheaf / trampled bases and the six badges at each scale, is a half-day and would make
§6, §8 and questions 2 and 4 a look rather than a read. Offered as the next step if wanted.
