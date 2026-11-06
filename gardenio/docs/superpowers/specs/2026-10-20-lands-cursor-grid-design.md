# Lands: the cursor world, grid of grids and cursor-centred signs (design spec)

**Status:** accepted (revision 2) by the owner on 2026-10-20, decision gd-n0ZRagfdLA. Phase 2b of
plan gd-oi3liYjNRA (Homestead World). Phase gd-7bfFegkYjS ("Cursor world"), task gd-Fe972f8H9B.
The plan-gardener mints the build steps from section 11.
**Revision 2 (2026-10-20):** revised after the owner's answer to decision gd-jcC1S7FXbV: "loosen the
diegetic restriction in the interest of saving space; it's more important to be parsimonious with
space and attention than to keep the UX predictable; but both is good." That ranking is now rule
N-D0 and changes N-D3, N-D4, N-D5, N-D12, N-D15 and sections 4.1, 6 and 7. Section 14 lists what
changed from the first draft.
**Revision 2.1 (2026-10-20):** adds 6.6, the plot rollup at the project depth, after the owner's
direction in Slack thread gd-LN4sLOLu6i ("our LOD for homestead isn't acceptable; we need to show
much more detail; perhaps showing a visual rollup on a tile's contents") and the owner's answer to
decision gd-zBRZ3rUfWS: **B over A** (the rollup stamp, with the mini-beds under it once a plot is
90 px or more on screen). Build step 5 draws it. Nothing else in the spec moves.
**Revision 2.5 (2026-11-01):** every project gets its own yard (plan gd-LurZOR44M1, after the owner in Slack thread
gd-tjddkquhPY: "we should move the yard to the meadow; and make it per project everywhere"). The shared Yard cell goes.
Each biome, the Commons included, gets a home strip in its Meadow row, holding west to east the Mailbox, the Growing
field (merged with the Meadow's loose beads), the Barn and the Pen. The Commons becomes cell 0 and projects take cells 1
onwards (memo v4, one re-pack). `Home` goes to the cursor's project's strip, else the Commons's, and `at=<project>/home`
names a strip, with `at=yard` resolving to the Commons's. The design and its decisions (H-D1 to H-D15) are section 28 of
`gardenio/docs/homestead-world-design.md` (`HW §28`). It changes 3.1 (the world's cells), 3.2 (`Home`), 3.5 (the
opening cursor), 5.1 (the lattice and memo), 5.2 (the band row: `home` replaces Meadow) and 5.5 (the Yard), each marked
below. The plan's build steps merged by 2026-11-04; each marked section's revision note gives the live rule, and 5.5
describes the home cell as it is now, after `HW §29` (plan gd-rLs7EoTa7a) moved the home from a band row into the plot
grid's centre slot.
**Revision 3 (2026-11-04):** the free camera (task gd-w37mF7SvFU, after the owner in Slack thread gd-NUuhwOU1l3:
keyboard navigation is good but should no longer be primary and should no longer drive the camera). A mouse drag or one
finger pans, and the wheel, a trackpad pinch and two fingers zoom about the pointer or the fingers' midpoint. The cursor
keeps every key of 3.2 and stays the hot link for the outline, ARIA and the address (3.5, 3.6), but a move or a sibling
jump no longer pans or zooms: the cursor may stand off screen, and the title bar names it (its last part frames it on a
press). Depth follows the camera: a zoom that takes the camera's level (`WORLD.lodFor`) past the cursor's depth takes the
cursor in or out with it. N-D6, N-D7 and N-D13 are struck; 4.2, 4.3 and 7 are rewritten under it, below.
**Revision 3.1 (2026-11-04):** the phone pad goes (task gd-wAN7P4Armw, the owner in Slack thread gd-NUuhwOU1l3: the
pad is not worth its row; a phone deserves free pan and zoom). N-D15 is struck and section 7 is rewritten under it, below.
**Revision 3.2 (2026-11-04):** the full tier goes (task gd-zDdiA3HBh5, the owner in Slack thread gd-NUuhwOU1l3: get rid
of the summary that pops up in the field when a bead is selected, and put more detail in the bar). No plate stands beside
the cursor's Thing at any depth. The title bar (`#cursor-bar`, gd-yozHl8UqIs) carries what the label carried: the lineage
as links, the whole title, and the status line of 6.4 in full, the need words first. Space, or a press on the bar's title
or status, opens the bead view (spec 7 Start). The child and glyph tiers stay, and so does the World board (4b.2, decision
gd-H6ZPz5V262), now a plate of its own. 6.1 to 6.4 are revised under it, below.
**Inputs:** the owner's two messages in Slack thread gd-z7ivrTTzTR (2026-10-20), summarised in
gd-Fe972f8H9B; the thread agent's reading of them (message gd-oQzaCjdKZW); the Homestead design
`gardenio/docs/homestead-world-design.md` (cited `HW §n`); the farm read model
`gardenio/docs/homestead-farm-read-model.md` (cited `FRM`); the C1–C12 rubric
`gardenio/docs/commons-ux-rubric.md`.
**Siblings:** `2026-10-19-lands-commons-design.md`, `2026-10-19-lands-track-b-design.md`, and the
phone note in PR #674 (`2026-10-20-lands-commons-phone-design.md`, draft).

Every code claim was re-read on `main` at 1ef627ac. Paths are relative to
`gardenio/controller/internal/gardenhub/` unless they start with `gardenio/`.

---

## 0. Summary

The owner's observation: on `/garden`, "scroll around to look around" (free camera) and mouseover
focus are dead ends. The signs flicker because the text is a function of the pointer. The
abstraction that works is a **grid of grids**, walked LSDJ-style with the hands on the keys (or an
on-screen pad and discrete gestures on a phone).

This spec replaces the free camera and the pointer-aimed text with one input state, the
**cursor**:

1. **One cursor path**, `world > project > plan > phase > task`. The arrows move the cursor one cell
   inside the current grid. `Enter`/A descends into the cell; `Esc`/B ascends. `Shift`+arrow
   (LSDJ's Select+direction) jumps to the sibling grid at the same depth. The focused `#hots` link
   is the cursor, so the accessible outline, the deep link and the canvas share one model.
2. **The camera only follows.** It frames the cursor's grid and moves when the cursor does. Wheel and
   pinch stop being continuous zoom and become a **depth gesture**: one descend or ascend per
   gesture, which tracks the fingers between two framings and springs back if released early. Drag
   to pan is removed.
3. **Grid of grids.** The world is a row-major lattice of projects. A project is the existing 7×5
   slot grid, walked by grid coordinate (slots stay sticky). A plan is a 3×3 of phase beds (the
   9-phase cap). A phase is a 9×9 bed of tasks in 3×3 blocks of 3×3 (81 cells, at least the 60-task
   cap). Plots become square (32×32 tiles) to hold it.
4. **Signs where they cost least.** The cursor's Thing gets its full title and status, on one
   compact label anchored at the Thing. The label is drawn over the map and has no space set aside
   for it. Its children get short stakes where they fit. Everything else is glyph-only (ordinal,
   need mark, progress pips) or nothing. Text is diegetic (at the Thing) where that is free. Where
   it would cost space, it gives way. The sign set changes only when the cursor moves, and it can
   cross-fade when it does. The hover card of `HW §17` is removed.
5. **Phone.** A slim one-row pad under the map holds the arrows, Select, B and A (60 px plus the
   safe area). A swipe is one cursor step. A tap moves the cursor there, and a second tap on the
   same cell descends.

**The ranking behind every choice (N-D0):** spend as little screen space and attention as
possible first. Keep the UX predictable second. Where one design does both, take it.

None of this needs a backend change. The farm document already carries everything. The work is in
`web/world.js` (geometry), a new pure `web/cursor.js`, and `web/garden.js` (input, camera, signs).

## 1. Decisions taken in this spec

Say so on the thread if one is wrong. Each one feeds the build steps in section 11.

| # | Decision | Choice | Rejected, and why |
|---|---|---|---|
| N-D0 | Ranking (owner, gd-jcC1S7FXbV) | When they conflict, **saving space and attention beats predictability**, and predictability beats diegesis. Where a design does both, take it. Every row below names which way it leans. | Predictability first (the first draft): it reserved world space for sign boards, stopped the cursor on empty cells and kept a 148 px phone deck, all for consistency. |
| N-D1 | Input state | One cursor path, the ids from world to leaf. The leaf is the cursor's **Thing**. Its parent's grid is the **current grid**. Hover sets nothing. | Keep "aim" (focus, else nearest plot to the pointer, `garden.js:676`) alongside a cursor: two selections again, and text would still answer the hand. |
| N-D2 | Grid edges | A plain arrow at an edge does nothing and the cursor bumps (a 2 px nudge, none under reduced motion). Only `Shift`+arrow crosses into a sibling grid. | Wrap-around: LSDJ does not wrap a grid, and wrapping breaks the spatial reading. Auto-crossing on a plain arrow: a held arrow would run out of the plot without meaning to. |
| N-D3 | Empty cells | **Skipped** (N-D0: attention). An arrow moves to the nearest occupied cell in that direction (3.2). Empty cells are not stops, not hots and get no sign. A project's plots are packed into slots 0..n-1 (5.2, decision gd-VfOCGDpxg2), so the only empties inside a project grid are the tail of its outer ring; a plot moves only when a plan ahead of it in spiral order finishes, one spiral step for each, and the skip is the same every time for the same farm. | Resting on empties (the first draft): one press moves exactly one cell, but every fallow slot costs a press, a sign and an announcement that says nothing. |
| N-D4 | Where descend lands | On the child the cursor last left in that grid (per-grid memory, as LSDJ remembers a cursor per screen). Otherwise on the first child that needs you, in beacon order. Otherwise on the first occupied cell in reading order. | Always the first cell: predictable, but it lands attention on whatever is top-left instead of on the thing worth looking at. Memory still wins when there is one, so a return visit is predictable. |
| N-D5 | Camera | Framing is a pure function of the cursor path and the viewport: the current grid's **used bounds** (its occupied cells), or a window of them when cells would be too small (section 4.1). Nothing else moves the camera. | Framing the whole container rect (the first draft): steadier when tasks come and go, but a 5-task phase would fill a quarter of the screen with bare soil. Free camera with the cursor as a hint: the owner called free look a dead end, and two camera owners would fight. |
| ~~N-D6~~ | ~~Wheel and pinch~~ (struck by revision 3: the wheel and a pinch zoom the camera freely, 4.3) | A **depth gesture**: spread, or wheel up, descends into the cell under the gesture. Pinch in, or wheel down, ascends. One step per gesture. During the gesture the camera tracks it, bounded between the two framings. On release it commits past 30% of the way and springs back otherwise. | Remove pinch: the owner uses it on the phone, and it was just fixed (gd-iYlxddKzuu). A "look" peek that snaps back: a second camera state, with labels that either lie or lag. |
| ~~N-D7~~ | ~~Drag~~ (struck by revision 3: a drag pans, 4.3) | No pan. A touch swipe is one cursor step (N-D13). A mouse drag does nothing. | Keep drag-to-pan: it is the "scroll around to look around" the owner called dead. |
| N-D8 | World layout | A row-major lattice, 5 columns wide. The Commons is cell 0, then projects oldest first (revision 2.5; it was the Yard at 0 and the Commons at 1). The cell index is memo'd, so a project keeps its coordinate. | Keep the section 16 packing and derive a grid from it by spatial nearest-neighbour: neighbours become ambiguous and `Shift`+arrow unpredictable. A spiral lattice around the Yard: holes inside the box and no reading order. |
| N-D9 | Stability promise | Restated for a cursor world: **no Thing ever changes grid coordinate because of another Thing, with one exception: a plot moves when a plan ahead of it in spiral order finishes, one spiral step back for each** (packed slots, 5.2, decision gd-VfOCGDpxg2). Adding a project or plan moves nothing. Pixel positions may shift by whole column widths when a column's widest region grows, and the shift is eased. | Keep pixel stability (`HW §3.4`, §16): it forced reserves and hops. Under a cursor, spatial memory is the coordinate, not the pixel. |
| N-D10 | Plot geometry | Square plots, 32×32 tiles at a 34-tile pitch. A 3×3 of 9×9 phase beds sits inside the fence, with 1-tile paths. | Keep 16×12 and draw tasks at sub-tile size: breaks the 16 px pixel art. Size each bed to its task count: the plan grid would stop being a fixed 3×3. |
| N-D11 | Task order in a bed | Block-major: block 1 (top-left 3×3) holds tasks 1–9 in row-major order, block 2 (top-middle) holds 10–18, and so on. The farm's order puts in-flight work first, so it lands top-left. | Plain row-major over 9×9: a 5-task phase becomes a thin strip across the top instead of a tight cluster. |
| N-D12 | Signage tiers | Full title + status on the cursor's Thing, as one compact label drawn over the map; short stakes on its children; glyphs or nothing elsewhere. The container's name is a dim prefix on the label, not a sign of its own (section 6). Nothing in the world layout is reserved for text. | Sign boards on posts and gate signs in reserved paths (the first draft): diegetic, but they need world and screen space. Sibling titles where they fit (today's rounds): the "text all over the place" the owner wants gone, and a cost in attention. A HUD breadcrumb bar: a permanent strip of screen space. |
| ~~N-D13~~ | ~~Swipe direction~~ (struck by revision 3: one finger pans; a swipe is no cursor step) | The cursor moves the way the finger swipes (as in 2048). A swipe is a command, not a drag. | The content-drag convention: nothing pans any more, so there is no content to drag. |
| N-D14 | Info card | Removed. The cursor's label carries a status line, and the bead view (`HW §19`) carries the rest. `Space` or a tap on the label opens it. `web/cardplace.js` retires. | Keep a card for the cursor: it is a second text surface beside the sign, and its placement code (`cardplace.js`, `HW §17` and §20) exists only to dodge the hover. |
| ~~N-D15~~ | ~~Phone controls~~ (struck by revision 3.1: no pad; taps, one finger, a pinch and the camera column's In and Out, 7) | A **slim pad**, one row below the map: four arrows, Select, B and A, 60 px plus the safe area (section 7). Start is a tap on the label. The map shrinks above it by that much only. | The Game Boy deck (the first draft): a familiar cross layout, but about 148 px, 18% of a phone screen, for controls that swipe and tap already cover. An overlay pad on the map: thumbs and controls cover a third of a 390 px map. |

## 2. What exists (the code this replaces)

- **Pointer-aimed text.** `aim()` (`web/garden.js:676`) picks "the cursor's plot": the plot whose
  hot link has focus, else `nearPlot()` (`:665`), the plot nearest the mouse within `NEAR_PX` =
  160 px. `retarget()` (`:681`) re-runs `placeWorldLabels()` (`:796`) whenever the aim changes, on
  every `pointermove` (`:507`) that is not a pan. `camChanged()` (`:430`) re-aims on every camera
  change, which includes every frame of a wheel or pinch zoom. `placeWorldLabels` places text in
  rounds and drops anything that overlaps. `labelText()` (`:887`) cuts each plan title to its
  plot's on-screen width, so the cut changes with every zoom step. **Text is a function of pointer
  and zoom**, which is why it flits.
- **Free camera.** Drag and one-finger pan (`:495`, `:507`); wheel and trackpad zoom about the
  pointer (`:540`); two-finger pinch (`:507`); double-click flies in 2× (`:547`); arrows pan a tile,
  `Shift`+arrows a plot, `+`/`-` zoom, `0` all, `Home` the Yard (`:560`). `settleSoon()` (`:469`)
  snaps z ≥ 1 to `{1, 1.5, 2, 3, 4}`.
- **The card.** `cardAnchor()` (`:683`) picks the pinned, hovered or focused hot, else the aimed
  plot. `placeCard()` (`:654`) delegates to `CARDPLACE` (`web/cardplace.js`, 93 lines). Touch: the
  first tap pins, the second follows the link (`HW §17`).
- **Layout.** `SLOT` (`web/world.js:137`): 16×12 plots at an 18×14 pitch on a 7×5 grid. `SPIRAL`
  (`:150`) maps slot k to `{c, r}`, so every plot already has a grid coordinate. `planBeds()`
  (`:266`) lays out one 14-cell row per phase, up to `BED.rows` = 10. Regions are packed by
  nearest free centre (`layout()`, `:289`; `HW §16`).
- **Model pieces to reuse.** `resolve()` (`:463`) already resolves a project, plan, **phase id**,
  task, seed, harvested plan or meadow bead. `locate()` (`:487`) finds what is under a world point.
  `outline()` (`:569`) is the project > plan > bead tree. `#hots` is a diffed set of links in world
  px under one CSS transform (`HW §11`, §15).
- **Caps** (FRM "Bounds"): at most 60 listed tasks per phase (`farm.go:61`, `more` counts the rest).
  The 9-phase limit is stated in `HW §1` but not enforced by `farm.go`, and `planBeds` keeps 10
  rows. Section 5.3 says what happens past 9.

## 3. The cursor model

### 3.1 The tree

`CURSOR.tree(world)` builds the grid of grids from a `WORLD.layout()` result. Each node is
`{id, kind, rect, cols, rows, cells}`. `cells[r][c]` is a child id or `null` (empty). Kinds and ids:

| Depth | Grid of | Node kind | Id | Cell size |
|---|---|---|---|---|
| 0 | the world | `world` | `world` | one region (project, Commons or Yard; revision 2.5: project or Commons) |
| 1 | a project | `biome` | `project:<key>` (as today) | one plot slot, plus the band row (5.2) |
| 2 | a plan | `plot` | plan id | one phase bed |
| 3 | a phase | `bed` | phase id; `<planId>/other` for unphased | one task |

The **path** is the list of ids from the world's child down to the cursor's Thing:
`['project:gardenio', 'gd-oi3liYjNRA', 'gd-7bfFegkYjS', 'gd-Fe972f8H9B']`. Depth is
`path.length - 1`. The current grid is the node of `path[depth-1]`, or the world at depth 0.

### 3.2 Keys

| Key | Pad | Does |
|---|---|---|
| Arrows | D-pad | Move to the next occupied cell in that direction (below). Bump when there is none (N-D2). |
| `Enter`, `+` | A | Descend into the cursor's cell (N-D4). On a leaf (task, seed, flower, tree), open the bead view. On a crate, open what it stands for (5.3). |
| `Esc`, `-` | B | Ascend: the cursor goes to the container. At depth 0, nothing. `Esc` first closes the topmost overlay (jump, bead view, tour, outline) and ascends only when none is open. |
| `Shift`+arrows | Select (latched) + D-pad | Sibling jump (3.3). |
| `Space` | tap the label | Open the bead view for the cursor's Thing at any depth: a plan, a phase, a task. On a project, open the Plans drawer filtered to it. |
| `0` | | Ascend to depth 0, keeping the cursor on the current project. |
| `Home` | | Cursor to the Yard at depth 0. (Revision 2.5: to the home strip of the cursor's project, else the Commons's; `HW §28.6`.) |
| `n` / `N` | | Cursor jump to the next / previous Thing that needs you, in beacon order (section 8). |
| `/` | | The jump palette, unchanged (`HW §13`). Picking a result is a cursor jump. |

**Which cell an arrow picks** (N-D3). Right from `(c, r)` takes the occupied cell in the same row
with the smallest `c' > c`. If the row has none to the right, it takes the occupied cell with
`c' > c` that is nearest by `c' - c`, then by `|r' - r|`, ties to the smaller `r'`. The other
directions are the same rule rotated. It depends only on the occupied cells, so the same farm always
gives the same walk. In a dense grid (a phase's tasks, which have no holes) it is one cell per press.

`=` also descends, as the unshifted `+` key. A held arrow repeats at the OS key repeat
rate. Each repeat is one step. A move during a camera ease retargets the ease from where it is,
with no queue.

### 3.3 Sibling jump (`Shift`+arrow)

`jump(path, dir)` moves the **parent** one cell in `dir`, then comes back down to the same depth
on the child at the **same coordinate**. If that coordinate is outside the new grid, it is clamped
to the grid's used bounds. If the cell there is empty, the nearest occupied cell is used
(Manhattan distance, ties in reading order). If the parent is itself at an edge, the step bubbles up one more level:
the grandparent moves, then the path comes back down two levels by the same rule. At the world's
edge it bumps.

Examples. At depth 3 on task (2,1) of phase 4, `Shift`+Right lands on task (2,1) of phase 5. At
depth 2 on phase 3 of the rightmost plot in a project, `Shift`+Right moves into the next project
in the lattice, onto the plot at the same slot coordinate (or the nearest), on phase 3.

### 3.4 Data changing under the cursor

The cursor follows its Thing **by id** across layouts while the Thing exists, even if its cell
moves (a harvested task slides the bed, `HW §15`). When the Thing is gone (a task closed, a plan
completed), the cursor takes the cell now at the same coordinate. If that cell is empty, it takes
the nearest occupied cell. If the whole grid is empty, it ascends. A data-driven move is a cursor
move: the signs change (section 6) and the screen reader is not interrupted (`aria-live` stays
polite). The camera moves only if the cursor's cell has left the framing. A change in the used
bounds re-frames on the next cursor move, not at once, so data does not pull the view around
while you read.

### 3.5 The URL

`?at=` names the cursor's Thing, as today: `<project>`, `<project>/<planId>`,
`<project>/<phaseId>` or `<project>/<taskId>`. The kind gives the depth: a project means depth 0
with the cursor on that project, a plan means depth 1, a phase depth 2, a task depth 3.
`resolve()` already accepts every one of these. `z=` is no longer written. On read it is
ignored, so old links still land on their Thing. The write is `history.replaceState`, 400 ms after
the cursor settles, as today (`HW §13`). Every other parameter (`bead=`, the hash) is kept as
written.

The last path is also kept in `localStorage` `garden.cursor.v1` (try/catch, as for the memo).
The opening cursor (replacing `HW §20`'s fit) is, in order: a `?at=`; the stored path if it still
resolves; depth 0 on the first project after the Yard and the Commons. (Revision 2.5: depth 0 on the first project,
cell 1 onwards, else the Commons; a stored path that starts at the Yard no longer resolves, `HW §28.5`.)

Descending does not push a history entry. Back keeps meaning "the page before" or "close the bead
view" (`HW §19`). Open question 12.1 asks whether a phone's Back should ascend.

### 3.6 Focus, ARIA and the outline: the hot link is the cursor

- `#hots` holds the **current grid's occupied cells only**: at most 61 at depth 3 (60 tasks and a
  crate) and 35 plus the band row at depth 1. The 300 cap stays as a guard. It becomes
  `role="grid"` with `aria-label="<container name>: <N> items, <rows> by <cols>"`. Each cell is a
  `role="gridcell"` with `aria-rowindex`/`aria-colindex`, and holds the existing link or button.
  Empty cells have no hot (N-D3). ARIA allows gaps in the indices, so the coordinates stay true.
- **Roving tabindex.** Exactly one hot has `tabindex=0`: the cursor's. `Tab` into the map lands on
  the cursor, and `Tab` again leaves it. A cursor move moves DOM focus **only if focus was already
  in the map**, so a data-driven move never pulls focus out of the Commons composer.
- **The cursor graphic is the focus ring.** The cursor's hot carries class `cur` and draws the
  two-tone ring from `HW §14` (gold outside a dark band, `TestFocusRingContrast`) as LSDJ-style
  corner brackets. `:focus-visible` makes it heavier. The canvas draws no cursor of its own, so
  the outline, the deep link and the picture cannot disagree.
- **Accessible name**: the title, the status line (section 6.4), the weeds sentence
  (`WEEDS.aria`), then "row r of R, column c of C". A descend or ascend announces once in a polite
  live region: "Inside <plan>: 3 by 3 phases, on phase 2, <title>."
- **Enter on a focused link** descends (with `preventDefault`) when the Thing has children. On a
  leaf, Enter follows the link, which opens the bead view, as today.
- The **outline** (`HW §14`) stays as the tree alternative. It is built from the same
  `CURSOR.tree`, so its order is the grid's reading order. Enter on a row is a cursor jump, then
  focus goes to the cursor's hot (`focusCursor`; `landFocus` retired with the ride-along, 3b R13).

### 3.7 `web/cursor.js` (pure, no DOM)

```text
CURSOR.tree(world, home, opts)        -> Tree     // section 3.1; home zones from WORLD.homeFill (5.5)
CURSOR.pathFor(tree, id)              -> path     // any Thing id, or the `at` value
CURSOR.atFor(tree, path)              -> string   // round-trips with WORLD.resolve
CURSOR.move(tree, path, dir)          -> {path, bumped}
CURSOR.jump(tree, path, dir)          -> {path, bumped}
CURSOR.descend(tree, path, memory, needs) -> {path, bumped, open?}   // open: the leaf's bead; needs: beacon order
CURSOR.ascend(tree, path)             -> {path, bumped}
CURSOR.follow(tree, prevTree, path)   -> path     // section 3.4
CURSOR.frame(tree, path, vp, opts)    -> {cx, cy, z, window}     // section 4.1
CURSOR.signs(tree, path, vp, z)       -> [{id, tier, text, status?, anchor, slot}]  // section 6
```

`memory` is a plain map from a container id to its last child id. The page holds it and saves it
with the path. `signs()` has no pointer argument at all, so N-D1 is enforced by the type.

## 4. Camera

### 4.1 Framing

`CURSOR.frame()` fits the current grid's **used bounds**: the bounding box of its occupied cells
(N-D5), so screen space goes to Things, not to bare soil. A data change does not re-frame by itself
(3.4). The board minus the HUD insets is the viewport (the header `--bar`, the phone pad, the
Commons band from PR #672). Padding is 6%, plus room for the full label when the label would
otherwise leave the viewport (6.2). At z ≥ 1 the zoom rests on the
largest of `{1, 1.5, 2, 3, 4}` not above the fit, so pixel art stays crisp.

If a cell would then be smaller than `CELL_MIN` screen px (24 fine pointer, 32 coarse), the camera
frames a **window** instead: whole rows and columns around the cursor (whole 3×3 blocks at depth 3)
at the zoom where cells meet `CELL_MIN`. The window scrolls only when the cursor comes within one
cell (one block at depth 3) of its edge, like an LSDJ phrase scrolling. On a 390 px phone a full
9×9 bed fits whole at z = 2 (32 px cells), and a 5-task phase (one 3×3 block) frames at z = 4. The
full 7×5 project fits too: plots are about 48 px.

32 px is under the 44 px of `HW §8`/§14 for coarse pointers. That is deliberate. Inside the cursor
grid a mistap only moves the cursor, which is harmless and undone with one tap. 32 px still meets
WCAG 2.5.8 (24 px). Everything outside the grid (the pad, the camera strip, the beacons) keeps
44 px.

### 4.2 Motion

> **Revision 3:** a move or a sibling jump moves **no camera**, framed or not; a cursor that leaves the view stays off
> it until the viewer pans, or presses the title bar's last part, which frames it. The camera still frames the cursor
> on the opening, on a jump to a Thing (the palette, the outline, a beacon, `0`, `Home`, `n`) and on a press of a
> container's link, easing 240 ms. In and Out (Enter and Esc, `+` and `-`, the camera strip, the pad's A and B, a
> second click on the cursor's cell) move the cursor one depth and zoom one step about its Thing (below). The signs
> are placed against the camera at rest when the cursor moved (6.3), never per pan or zoom frame.

- A move inside a window that does not scroll moves **no camera**: only the cursor ring and the
  signs change. This is the calm, LSDJ-like case and the common one.
- A window scroll or a sibling jump pans in 160 ms. A descend or ascend eases centre and zoom
  (centre linear, zoom exponential: the existing `flyTo`) in 240 ms. Reduced motion makes every
  one a cut (`HW §14`).
- The terrain level of detail (`WORLD.lodFor`, chunk mips) still follows z, because it is about
  pixels. Text and hots follow depth. With square plots the framed z lands each depth in the level
  it reads best: depth 1 at z 0.2–0.35 (Project), depth 2 at 0.6–1 (Plan on a phone, Bead on a desktop),
  depth 3 at 2–3 (Bead).

### 4.3 What happens to each camera input

Revision 3 rewrites the Becomes column; the revision 2 column it replaced is in git history (this file at 817e31a6).

| Input | Today (`HW §11`) | Becomes (revision 3) |
|---|---|---|
| Drag / one-finger pan | pans | pans: a drag of d px moves the camera d/z world px. Past 5 px it is no click. The finger left from a pinch pans on. |
| Wheel, trackpad pinch (`ctrl`+wheel) | zoom about the pointer | zoom about the pointer, continuously, between `Z_MIN` and `Z_MAX`: 2^(−deltaY × 0.003), and × 0.01 with `ctrl`. The world point under the pointer stays put. Horizontal deltas are ignored. Depth follows (below). |
| Two-finger pinch | zoom about the midpoint | zoom by the fingers' spread about their midpoint, which also pans with the midpoint's travel. Depth follows. The pointer-capture guards from gd-iYlxddKzuu stay. |
| Double-click / double-tap | fly in 2× | a zoom step about the point, unless the click pair already went in or opened (a second click on the cursor's cell does). Depth follows. |
| Arrows, `Shift`+arrows | pan a tile / a plot | cursor move / sibling jump; the camera stays |
| `Enter` `+`, `Esc` `-`, the strip's In and Out, the pad's A and B | zoom 1.25× | the cursor descends or ascends one depth, and the camera zooms a step about the cursor's Thing when it is on the board, else about the board's middle (In then first moves the cursor to the cell there). The keys' one camera move: In and Out are camera buttons. |
| `0`, All | fit the world | depth 0, framed |
| `Home`, Yard | fly to the Yard | cursor to its project's home cell, framed |
| Minimap click / drag | fly / scrub | removed (3b R11, gd-dOMlE4c3SX): depth 0 is the overview, so `0` then a click, or `/`, reaches another project. |
| `/` jump, outline, beacons, bead view "Show on the map", the title bar | fly to a rect | cursor jump to the Thing (`pathFor`), then frame |
| A click or tap on a cell, or on a Thing outside the grid | pin or open | the cursor moves (or jumps) there; the camera stays. A second on the cursor's cell goes in (a leaf opens). |

**A zoom step** (`zoomStep` in `garden.js`) toward level L: twice the zoom (half, out), and at least into L's range
(`WORLD.levelRange`), so a step in from the Project level always reaches the Plan level; at 1× and over it rests on a
snap (`WORLD.snapZ`), so the pixel art stays crisp. In and Out step toward the cursor's new depth, so after one the
camera's level is the cursor's depth; a double click steps toward the next level.

**Depth follows the camera** (`follow` in `garden.js`). After a free zoom in (wheel, pinch, double click), while the
cursor's depth is under the camera's level, the cursor descends into the cell under the pointer or the midpoint (off
the cells, its own Thing; a cell outside its grid is a jump there first); after a zoom out, while its depth is over
the level, it ascends. It never opens a leaf, and the camera stays where the zoom put it. Evaluated on every zoom
event in the zoom's own direction only, so a framing that rests a depth at a finer level (a desktop frames a plan's
phases at 1×, the Bead level) moves no cursor until the viewer zooms in.

**Signs** (6.3) still change only on a discrete cursor move, a descend or ascend by `follow` included, and never on a
pan or zoom frame; cells smaller than `CELL_MIN` keep the glyph tier.

The camera strip's `+ − Yard All Go Outline` become `In Out Yard All Go Outline`. On a phone the
pad's A and B replace In and Out. Since 3b R8 the strip is `In Out Go Menu` (on a phone `Go Menu`): the
Menu, a disclosure (a button and a list, closed by Esc), holds Yard (Home since revision 2.5), All, Outline, Tend and Snap. 3b R9 adds
Plans and Legend & keys to it: each opens the drawer that was a button on the map. 3b R10 adds Sky, the header's
button that cycles auto, day and night, with its setting in the item's label.

## 5. Grid of grids

### 5.1 World: a row-major lattice of projects

> **Revision 2.5 (`HW §28.5`):** the Yard cell goes. The Commons is cell 0 and projects take cells 1 onwards; memo v4
> drops the v3 cells and keeps the slots, so the first load re-packs once.

- Lattice width `W = 5` cells. Cell k is at `(k mod 5, floor(k / 5))`. The Commons biome is cell 0
  at the world origin and takes no room while it is empty, so a one-project world has no hole
  there (`COMMONS_K`, H-D7). Projects take cells 1 onwards (`FIRST_K`), oldest first (the oldest
  `created` of their plans and seeds, ties by `h32(name, 1)` then name, as in `HW §16`). The width does not
  depend on the viewport, so a coordinate means the same on every screen.
- **Memo v5** (`HW §28.5`, §29.5): `{v:5, cells:{key:{k, t}}, slots:{planId:{s, t}}}`, with
  `k >= 1` and `s >= 1` (slot 0 is the farmstead). A project keeps its index k while it is seen. A
  newcomer takes the lowest free index. A project gone for 30 days frees its index (the
  `MEMO_DAYS` rule). A v4 memo keeps its cells and its slots shifted up by one; v1 to v3 memos
  keep their slots and drop their cells, so their first load re-packs the lattice once.
- **Pixels:** each column is as wide as its widest region and each row as tall as its tallest,
  with `GAP` = 16 tiles between them. A region sits centred in its cell. A region is sized to its
  content (`shape()` over the used rows and columns). `RESERVE` and the hop of `HW §16` go away,
  because N-D9 only promises coordinates. When a column widens, the columns east of it shift
  together, eased like a biome step.

### 5.2 Project: the 7×5 slots, walked by coordinate

- Slots and the `SPIRAL` are unchanged (`HW §3.3`), but slots are **packed** (decided,
  gd-VfOCGDpxg2, 12.2): a project's live plans fill spiral slots 0..n-1 with no holes. The order
  is the memo'd slot ascending (ties by `created`, then id), then the newcomers in
  `(created, id)` order, so the i-th plan takes slot i. When a plan finishes, only the plots after
  it step back one slot, in the same order; a new plan takes the next slot after the others. The
  slot memo keeps its v3 shape and now records a rank rather than a fixed home. The cursor walks
  `{c, r}`, not the spiral index, and follows its Thing by id (3.4), so a plot that moves keeps the
  cursor. The grid's bounds are the used rows × used columns, symmetric about the centre as
  today. That is 1×1, 3×3, 5×5 or 7×5. Only the tail of the outer ring can be unused (a 7-plan
  project is a 3×3 with its last two slots free); unused slots are drawn as fallow ground but are
  not cursor stops (N-D3).
- **Band row.** (Revision 2.5, `HW §28.6`: the home strip's cell replaces the Meadow's in the top row, and the loose
  beads move into its Growing field.) The seeds, harvested plans and loose beads become three cells: Meadow and Orchard
  in a row above the plots (where those bands are drawn), Nursery in a row below. Each spans the
  grid's width and descends into a row-major grid of its items, 9 wide. Up from any plot in the
  top row lands on the band cell above. Down from a band cell returns to the column the cursor
  came from. A band with no items is not a cell.
- **Overflow:** past 35 plans the `+N more plans` crate is a single cell in its own row under the
  Nursery. A opens the Plans drawer, as the crate does today.
- **Plot geometry** (N-D10): `SLOT = {w: 32, h: 32, dx: 34, dy: 34}`. The fence ring is 1 tile, so
  the inside is 30×30. Beds are 9×9 at offsets 2, 12 and 22 from the plot's corner on each axis.
  The 1-tile paths between beds are garden structure. A bed's stake glyph (ordinal and pips) is
  painted on the path tile at the bed's top-left corner, so it takes no extra space. No tile is
  reserved for text (N-D12). A full 7×5 project is 236×168 tiles; a typical 3×3 is 100×100.
  This is about 5× the area of today's plot. The camera frames by cursor, so that does not cost
  any navigation. Gardener spots (`WORLD.gardenerState`) and the truck ride (`web/transitions.js`)
  re-derive their positions from the new rects.

### 5.3 Plan: a 3×3 of phase beds

- The beds are the phases in `phase_order`, numbered 1–9 in reading order (1 2 3 / 4 5 6 /
  7 8 9). Then comes **Other** (`unphased`, FRM) when it holds anything.
- **Past 9:** when phases plus Other exceed 9, cells 1–8 hold the first eight and cell 9 is a crate
  "+N phases". A on it opens the plan's bead view, where the full phase list is. The plan also gets
  a weed, `overgrown` ("more than 9 phases; split the plan"), added to `web/weeds.js`, so the cap
  `HW §1` states becomes visible instead of silently truncated. Rejected: a second 3×3 page behind
  the crate, which would need world space that does not exist.
- **Order without hover** (the thread agent's open point): every bed's stake carries its ordinal
  as a pixel digit, plus a 4-pip progress bar (done ÷ total, in quarters). The **current phase**,
  the first one not closed, has a lit (gold) stake. A closed phase's bed is stubble. So "phase 1,
  then 2, then 3" and "we are on 4" both read at a glance, with no text. Dependency order between
  steps stays in the bead view (owner: "dependencies aren't as important as you might think").

### 5.4 Phase: 9×9 in 3×3 blocks

- Cells are filled block-major (N-D11) with the plants in the order they were created, then id
  (gd-wDdun2AVwq): the listed tasks and the listed closed steps together, so a step keeps its
  cell when its status changes or it closes, and a new step takes the next cell. Then, if
  `more > 0`, a crate cell "+N" whose A opens the phase's bead view, then stubble for the closed
  count past the listed steps. 60 tasks plus a crate fit in 81 cells. (Before gd-wDdun2AVwq the
  cells followed the farm's in-flight-first order, so a status change moved them.)
- Once placed, a bead keeps its cell (gd-xhpccozIcC): `WORLD.layout` takes a placement memo per
  plan, `{bed: {bead: cell}}`, kept per tab in `sessionStorage` (`garden.cells.v1`), so a new
  step, a close, a title edit or a reorder of the document moves nothing already planted, and a
  refresh keeps the cells too. A new bead takes the lowest free cell; a bead planted before and
  listed no longer (closed past the listed steps, cut by the cap) holds its cell as a dim marker
  drawn as stubble, not a cursor stop, rather than closing the gap; a full bed reclaims its
  lowest markers. A new tab repacks from the plant order above.
- The grid's bounds are the used **blocks'** bounding box (a 5-task phase is a 3×3 grid, 12 tasks
  are 6×3). The camera frames those bounds, not the whole bed (4.1).
- Blocks alternate two soil tones, so the 3×3 structure reads without gutters.

### 5.5 The home cell

> **Revision 2.5 (`HW §28`), as built.** The shared Yard, lattice cell 0 holding every project's letters, growing beads
> and resting gardeners, is gone. Every biome has its own home: first a band row at the top of the region (`HW §28.1`),
> then, under `HW §29`, the farmstead at the plot grid's centre. The text below is the live page.

Every biome, the Commons included, has a **home cell**: its farmstead, spiral slot 0, the centre cell of its plot grid
(`HW §29.1`). The arrows reach it as they reach any plot, and it is a cursor stop even when it holds nothing, so `Home`
always has somewhere to land (H-D5). It descends into a 4×2 grid laid out as the courtyard (`ZONE_AT` in `cursor.js`,
`HW §29.6`): the Barn, the Escalations and the Mailbox on the top row, the Pen and the Growing field below it. A zone
with no items is not a stop (N-D3), except the Barn while it counts a running agent (`HW §28.12`). Each zone descends
into its items in `WORLD.homeFill`'s slot order: the Mailbox's letters, the Escalations in `escOf`'s order with the
"+N" crate last, the Growing field's stands and then its loose beads, the Barn's release truck (its one item, `HW
§28.10`), and the Pen's goats, oldest first. Enter on a letter or a plant opens it. A stand takes the three-stop Enter
of `HW §28.12`. The dog is only a picture.

`Home` puts the cursor on the home cell of the project under the cursor, else on the Commons's, and a second press goes
to the Commons's (H-D11). The ids are `home:<key>` and `home:<key>/<zone>`, written `at=<project>/home` and
`at=<project>/<zone>` (`~` for the Commons). A legacy `at=yard` link lands on the Commons's home and a toast says
where it went (`HW §28.7`, H-D12).

### 5.6 The produce bin (revision 2.3, gd-HqcG2zcwnJ, gd-2qgcDRZUX6) — retired

> **Retired (2026-11-02, plan gd-PJB3D7glwF "Escalation yard", step gd-vDauUoOkj8).** The owner
> found the bin was not helping, so it is gone: `web/bin.js`, the `#bin` plate and its docked
> phone row, the I key, the row peek, and the plot's crate mark (`ART.drawProduceMark`,
> `ART.drawProduceIcon`). The read model stays: `FarmPlan.produce[]` (`farm_produce.go`) still
> feeds the bead view, the harvest cells and the goats' kin. The farm gains an escalations cell
> in its place (plan gd-PJB3D7glwF). The text below is kept as the record of what shipped.

The owner: "MRs and bundles and reports produced by a plan should show in a little produce bin
near the plan, with its own lil inventory and icons and such, so we can see what the plan
produced; stuff not yet in the bin should show at the top but grayed out cause it's not there
yet, but you can highlight to zoom to it in the plot."

**Data.** `FarmPlan.produce[]` and `produce_more` (`homestead-farm-read-model.md`, FarmProduce):
merged MRs on the plan's steps, closed steps included, open and draft MRs as **expected** produce
(`ready: false`), and the bundle and report beads under the plan. Ordered by kind (revision 2.4,
the owner: "most recent reports first, reports before MRs", gd-wgeINbOQMC): reports, then
bundles, then MRs, and the step rows after them; within a kind the expected first, then newest
first. Every row is listed and the bin scrolls; the farm's 300-row bound is a safety, not a
page. Each item names the step it hangs off (`task`, `phase`), which is the cell the
bin zooms to. Beside them (revision 2.3, the owner: "contain recent in progress and finished
stuff") the bin lists the plan's **recent work**, which the page reads from the plan's own
phases, not from produce, so the farm never sends a step twice: a leaf step in progress in
`tasks[]` as expected, and the 12 newest leaf steps in `closed[]` (gd-0Fj6E2vPql) as ready,
when the step carries no MR (a step with one is its MR, never a second row). They are merged
into the same order. Nothing else is derived on the page beyond the words.

**Where.** A plot has no spare tiles (32×32 on a 34 pitch; the 2-tile gap is the gardeners'
lane, 5.2), so the bin is not world geometry. It is a DOM overlay, like the label (6.2): a small
plate in the sign-plate palette anchored to the plot's rect, so it follows the plot when the
slots pack (12.2). It shows when the cursor's Thing is the plan's plot (depth 1) or inside it
(depth 2 and 3 within that plan), and for no other plot, and it **reads at the cursor's depth**
(revision 2.3, the owner: "just show info for the LOD selected"): on the plot the whole plan; on
a bed what that phase's steps made (the unphased bed its own, not the plan's narrative); on a
step what hangs off it, itself included. A depth that made nothing has no bin. The group's name
and the heading say the depth (`Produce of <plan>, phase <bed>: …`). Placement is the first slot of a fixed
order that fits inside the viewport and keeps off the full label and the grid's other cells:
**right** of the plot, then **left**, **below**, **above**; when none keeps off them, the first
that fits. On a phone (7) no side has room: the bin is one row of icons in the inset above the
pad, horizontally scrollable, its titles on the hots and in the caption line above the row.

**The inventory.** Every row (revision 2.3, the owner: "scrollable so you can see everything"):
the list is eight rows tall and scrolls past that, its wheel its own and the focused row kept in
view, with a `+N more` row only for what the farm's own cap left out (the plan's bead view lists
all). Each row is a 16 px pixel icon, the title cut to 24 ch with an ellipsis, and a state word:

| Item | Icon (`art.js`) | State word |
|---|---|---|
| merged MR | a crate (`b`/`B`) with a merge glyph: two `l` dots joined by a `k` line | `merged` |
| expected MR (opened, draft) | the same crate in the stone greys (`r`/`R`), the glyph `q`, at 55 % opacity with a 1 px dashed outline | `coming` |
| bundle | a tied sheaf (`c` stalks, `Y` tie) | `bundle` |
| report | a sheet (`j` face, `k` rule lines) | `report` |
| step in progress | the bed's own sprout on its mound, grayed and dashed as a coming row | `growing` |
| finished step | the bed's stubble on its mound | `done` |

Expected rows sit at the top (the farm's order), grayed and dashed: the word `coming` carries the
state, never the grey alone (`HW §6`, rule 1). The bin's heading is a tally, `3 in the bin · 1
coming`, and the same tally is the plot's glyph: a 5×5 px crate mark at the plot's bottom-left
on the fence, with the ready count as a pixel digit (9 at most), drawn at every depth like the
need mark (6.6), so a full bin reads from the project depth without opening it. An empty bin
draws no mark and no overlay.

**A peek ends with its row** (revision 2.3, the owner: "the bug where it persists after changing
ux focus"). A redraw of the rows (the cursor into a bed) or a hide of the bin (the cursor out of
the plan) while a row is lit puts its light out first, so the ring and the bin's "hold still"
never outlive the row; before this they did, and the bin sat where it was, ringed, after the
cursor had left.

**Highlight = peek, A = go.** Hovering or focusing a row **peeks** at its step: the camera frames
the step's cell with `CURSOR.frame(..., {also: cell})` and the cell gets a lit ring in the
produce colour, while the cursor path, the signs and the bin stay where they are (the bin is a
function of the cursor, so moving the cursor on hover would move the bin under the pointer).
Leaving the row returns the camera to the cursor's frame in the same 160 ms. A row whose step is
`""` (the plan's own bundle) peeks at the plot. Enter, Space or a click on a row is **go**: a
cursor jump to the step (`cursorTo(task)`) and then the thing itself opens, an MR in a new tab
(`url`, `rel=noopener`), a bundle or report in the bead view. Reduced motion makes the peek a
cut, as 4.2.

**The lit row reads in full (gd-bxWV04FKTV).** The owner: "does highlighted bin item show text?
Should." A row's 24-character cut is for the inventory; the row that is lit (hovered or focused)
is read in full by a **caption** under the rows: `<state word> <kind> <!iid>` in bold, the whole
title, `from <step title>`, clamped to two lines (`BIN.caption(row)`). The caption's box is
always there, so the plate is the same size lit or not and `place()` never moves the bin out
from under the pointer: while nothing is lit it holds a key hint (`hover or i reads one in
full`). On a phone the caption is the line above the icon row, holding the tally while nothing
is lit (the heading is not shown in the dock), and since a finger cannot hover, a **tap on a row
that is not lit lights it** (focus, peek, caption) and **a second tap on it goes**; a tap on
another row moves the light. The press's `pointerType` decides (`touch`, `pen`), not a media
query, so a touch laptop's mouse still goes in one click, and Enter, Space and modifier clicks
are unchanged. The caption is `aria-hidden`: the row's accessible name already says it all.

**Keys and ARIA.** The bin is `role=group`, named `Produce of <plan title>: 3 in the bin, 1
coming`, after `#hots` in the Tab order, its rows links with a roving tabindex; `i` moves focus
from the cursor into the bin and `Esc` returns it to the cursor's link. Each row's accessible
name is `<state word> <kind>: <title>, from <step title>`. The overlay is `aria-hidden` only
when it is empty; the icons are `aria-hidden` always.

**Code shape.** `web/bin.js`: pure `BIN.rows(plan, limit)` (order, cut, tally, step titles from
the plan's phases) and `BIN.place(plotRect, vp, keepOff)` (the slot order), run under node by
`TestBinUnderNode`, with `BIN.caption(row)`; the DOM part renders `#bin` from the `farmdoc`
document and the cursor path, and calls `GARDEN.peek(id, on)` and `cursorTo`. `ART.drawProduceIcon(ctx, pal, kind, ready, x, y,
s)` and the plot's crate mark in `drawPlotMarks`. New file: embed in `page.go`, `assetsTag`, a
`<script>` in `garden.html` after `cursor.js`. Goldens: `cursorplan` gains the bin (the fixture
farm's plan A has one expected MR; add a merged one and a bundle to the fixture); a `bin`
tolerance row in `goldenSurfaces`; `/commons-ux-acceptance` C1–C12 on `cursorplan`.

## 6. Signs: diegetic where it is free

N-D0 sets the rule. The owner's word was "diegetic": text at the Thing, centred on it,
emphasising it. Keep that wherever it costs nothing. When it would cost world space, screen space
or a second place to look, the text gives way. It sits over the map, compact, still anchored at the
Thing. The tiers say how much text there is, which is the attention budget. Placement (6.2) says
where it goes, which is the space budget.

### 6.1 Tiers

The sign set is `CURSOR.signs(tree, path, vp, z)`: a function of the cursor path and the
viewport only.

| Tier | Which Things | What shows | Budget |
|---|---|---|---|
| ~~**Full**~~ (struck by revision 3.2: the cursor's Thing has no label; its words are the title bar's, 6.4) | the cursor's Thing | one compact label: the Thing's lineage as a dim prefix (`Cursor world ›` on a plan, `Plan › Phase ›` on a task: the containers below the biome, or the biome for its own children), then the whole title, then ` · ` and the status line (6.4). One line when it fits in the label's width cap. Else the prefix shares the first line with the title when the two fit, or takes a line of its own; the title is never cut: it wraps whole across as many lines as it needs (a single word wider than the cap breaks at the cap); the status ends the title's last line when it fits there, else takes a line of its own. The prefix's names shorten only when the prefix line itself is past the cap, the longest first, down to 6 characters, and are never dropped, so the plan and the phase are on screen together (gd-kq4V4XWjWu, thread gd-gKdSThBOnC; before it, gd-LdeL5zW3vH capped the label at two lines and cut the title past them). A status past the cap gives up whole parts from its end, never the need words. | 1 |
| **Child** | the cursor's Thing's children | a short label (≤ 10 characters, first words, ellipsis) where the child is at least 56 px wide on screen, else its ordinal or glyph only | 35 (a project's plots), 9 (a plan's phases); tasks are glyph-only |
| **Glyph** | every other cell of the current grid | painted on the canvas, no DOM text: a need mark `!N`, progress pips, the phase ordinal, the existing state sprites (rock, fruit, sprout, wilt, thistle). A plan plot at the project depth carries the **rollup stamp** (6.6) | n/a |
| **Nothing** | everything outside the current grid | terrain, sprites and the need ring only | |

Siblings stay glyph-only on any screen size (open question 12.2 of the first draft, now decided by
N-D0). A screen with room for more titles does not show them, because each one costs attention. The
container no longer gets a gate sign of its own: its name is in the title bar's lineage, so there is
one text object to read, not two.

> **Revision 3.2:** the title bar is that one text object. On a desktop it keeps its place under the plaque, left of the
> Porch and camera rows: the lineage as links and the cursor's whole title (wrapping, never cut; on its own line when it
> does not fit beside the lineage), then the status line on a row of its own. On a phone it is the strip on the tab bar,
> two 22 px rows at most, the rest cut with an ellipsis; a tap on it opens the bead view. Its words come from
> `CURNAV nav.bar` and `CURSOR.statusOf`.

"State is never colour alone" (`HW §6`, rule 1) still holds. The glyph tier keeps every shape and
the need count, and every cell's accessible name carries the words.

### 6.2 Placement: the space budget

> **Revision 3.2:** there is no full label to place. Child labels keep off each other only; the slot order below is
> the World board's (4b.2, `CURSOR.worldSigns`), clear of the header plates first and the other projects second.

- **The label is an overlay, not a structure.** It is DOM text in `#lbls` at 12–13 px screen size
  (crisp at any z, as today), on a small plate in the sign-plate palette with a 1 px pointer to
  the Thing. It has no post or board art, and nothing in the layout or the framing makes room for
  it beyond the edge padding of 4.1. The plate is at most 28 ch wide on a phone and 40 ch on a
  desktop.
- **Where.** The label goes in the first slot of a fixed order that fits inside the viewport and
  does not cover the cursor's Thing: **inside** the Thing when it is at least as big as the label
  (a plot at depth 1 on a desktop, a bed at depth 2), then **above**, **below**, **right**,
  **left**. Of those, it takes the first that also keeps off the grid's other cells, so the label
  never hides a neighbour it could have missed (a wide label above a Meadow flower hid the next
  flower, and a click there opened the cursor's Thing; gd-UAKclcq5hu). When no slot keeps off them,
  it takes the first that fits and may cover sibling cells: they are glyph-tier and the label is
  the one thing worth reading. The order is fixed, so the same path and viewport always give the
  same placement. A different viewport can give a different slot, and that is allowed (N-D0).
- **Child labels** sit at the child's top-left, inside the child. They are dropped (never shrunk)
  if they would overlap the full label or each other, in reading order. A dropped label falls back
  to the child's glyph.
- **No collision lottery.** The full label is placed first and never dropped. The set depends only
  on the path and the viewport, never on the pointer, and never on a camera frame mid-ease.
- `#lbls` stays `aria-hidden`; the hot carries the words.

### 6.3 When signs change

> **Revision 3.2:** with no full label there is no cross-fade; the title bar is refilled with the signs, in one place.

- **Only on a discrete cursor move**: a key, a pad press, a tap, a swipe, a committed depth
  gesture, a cursor jump, or a data-driven follow (3.4). Not on pointer movement, and not on a
  camera frame. During an ease or a depth gesture the labels ride the world transform with their
  text fixed. A spring-back restores them unchanged. A viewport resize re-runs placement once,
  when it settles.
- **Transition:** the outgoing label fades as the incoming one grows from the Thing, a 120 ms
  cross-fade. It is skipped under reduced motion, and also when the next move arrives within
  120 ms (a held arrow), so a fast walk never stacks fades.

### 6.4 The status line

> **Revision 3.2:** the status line lives in the title bar, whole: nothing is cut to a cap, the need words come first, and
> a harvest cell shows its lead words, then every badge as a 12 px icon and its word in the §4.2 order (none dropped, no
> `+N`). The page adds its own words at the end, as it did to the label: a busy project, open wishes, a plot's goats past
> the cap, a release's.

| Thing | Status line |
|---|---|
| project | `7 plans · 2 need you · 3 gardeners` |
| plan | `phase 3 of 7 · 12/40 steps · 2 need you` |
| phase | `4/9 steps · 1 blocked · 1 growing` |
| task | `growing · @assignee · 2h` (state word, who, age) |
| harvest cell (a closed step with a `harvest` object) | `harvested · @who · 2d ago · merged #825 · graded 5/5 gates · 3 tests · checks green · merged after 2 rounds`: the base word (`harvested`, `abandoned`, `closed`), who, when, `checks failed` when CI did not go green, then each badge's words in the harvest spec's §4.2 priority order. The full label shows only the lead words in its status and puts the badges on a last row of 12 px icon and word pairs, cut at the 28/40 ch cap from the end with `+N` (harvest spec §5, gd-aN0u53sehO). A closed row without `harvest` keeps the task row's `closed · @who · 3d`. |
| seed, harvested plan, loose bead | the state word and age |
| home cell | `3 letters · 4 working · 2 growing · 1 open wish`, the need words first (`HW §28.6`) |
| home zone | `3 decisions waiting` / `2 escalations · 1 needs you` / `4 growing` / the Barn's `barnWords`, then its truck's words / `2 open wishes · 1 out grazing` |

Parts that would read zero are left out (`7 plans` alone, not `7 plans · 0 need you`). Weeds add
`· N weeds`, and the need words come first when a human is wanted. The full weed sentences,
decision lists and gardener names that the card held (`HW §17`, §21) move to the bead view. Build
step 6 adds a "Weeds" and a "Gardeners here" section to `web/bead.js` for a plan.

### 6.5 What goes away

`aim()`, `retarget()`, `nearPlot()`, `NEAR_PX`, `hoverNode`, `pinNode`, the `mouseenter`/
`mouseleave` card handlers, `cardAnchor()`, `updateCard()`, `placeCard()`, `#tip` and
`web/cardplace.js`. The label rounds in `placeWorldLabels()`, `labelText()`'s per-zoom cut and
`placePhases()` are replaced by rendering `CURSOR.signs()`. CSS `:hover` may tint a cell's outline
for mouse users. It never changes text.

### 6.6 The plot rollup at the project depth (revision 2.1)

At the project depth (depth 1) every plan is one flat plate in its crop colour, with the need
ring. The owner: that LOD "isn't acceptable; we need to show much more detail; perhaps showing a
visual rollup on a tile's contents" (Slack thread gd-LN4sLOLu6i). Three glyph-tier designs were
drawn at the real cell sizes (about 66 px on a 1280 px desktop and 48 px on a 390 px phone, 4.1):
**A** mini-beds (the plan depth shrunk), **B** a rollup stamp, **C** a jar with phase pips. The
mock is `gardenio/docs/prototypes/homestead/plot-rollup-options.html` (open at `file://`). The
owner chose **B over A** (decision gd-zBRZ3rUfWS): the stamp always, with A's beds under it once
the plot is big enough that they read.

This is the glyph tier (6.1) for a plan plot at depth 1. It is painted on the canvas in
`drawWorldLayer`'s `level === 1` branch in place of the flat plate. No DOM text, no change to the
layout, the camera or the farm document: it reads what `planBeds` already derives per bed (`n`,
`at`, `done`, `total`, `closed`, `current`, `other`), `plot.crate`, `plot.plan.counts`,
`plot.needs` and the gardener count from `gardenerSpots(...).plots[plot.id]`. The words stay on
the hot (3.6) and on the label's status line (6.4): `phase 3 of 7 · 12/40 steps · 2 need you`.

**Screen pixels.** Like the need ring (`drawRing`, `3/z`), every line of the stamp is sized in
screen px (`s = 1/cam.z` world units), so a 1 px frame is 1 px at any zoom and the plate side is
a whole number of px at the integer zooms of 4.1. `side` below is the plot's side on screen.

**The plate.** As today: a 1 px `k` frame, a 1 px ring in the crop colour (`PLANS.tile().crop`
through `CROP_COL`), then the `P` face. The gold need ring and the fence (`NB.fence`) are
unchanged.

**The stamp, always.** On the face, a 3×3 of phase cells, each in its bed's real position
(`bed.at`), so a descend lands where the eye already was. Cell side `c = floor((side − 10 − 2·gap)
/ 3)`; `gap` is 2 px on a plate of 60 px or more, else 1 px; the 3×3 is centred and its top is 4 px
under the plate's top. Positions with no bed stay the plate face. The Other bed takes its position
with no ordinal. The `+N phases` crate takes its position as a crate mark (two stacked `b`/`B`
boxes). Per phase cell:

| Part | Rule |
|---|---|
| Frame, 1 px | gold `Y` = the current phase (`bed.current`); green `L` = closed; red `e` = blocked and nothing moving (`blocked > 0` and `in_progress == 0`); orange `o` = something in review; else `k` |
| Face | soil `t`. A closed phase: leaf `l` with a white (`j`) tick, 7×5 px (2× on a cell of 48 px or more). An open phase fills from the bottom with `l` to `done / total` of the face height (`bed.done`, `bed.total`); a phase with no tasks stays soil |
| Sprout | `n`, 2 px wide, up the middle from the fill to 60% of the room above it, when `in_progress > 0` |
| Rock | `k`, 3×2 px at the top-left corner, when `blocked > 0` |
| Fruit | `o`, 3×3 px at the top-right corner, when `review > 0` |

State is never colour alone (`HW §6`, rule 1): a blocked phase has the rock, a reviewing one the
fruit, a growing one the sprout, a done one the tick, and the current one is the only gold frame.

**The bar.** Under the grid, 5 px above the plate's bottom: a 4 px bar, the plate's width minus
8 px, inside a 1 px `k` outline, every task of the plan by state from `plan.counts` in the order
closed `l`, in_progress `m`, review `o`, blocked `R`, open `c`, deferred `Z`, widths proportional
(`round(bw · n / total)`), zero-count states taking no width. A plan with no tasks shows the
outline only.

**The marks on the plate.** The need mark `!N` (a 13×9 px `Y` plate on a `k` outline, a pixel
exclamation, the count as a pixel digit, 9 at most) at the plate's top-right corner, riding the
fence, when `plot.needs > 0`. Gardener heads (5×5 px: `k` outline, the gardener's hat colour over
an `a` face), up to four, right-aligned on the bottom fence, one per gardener standing on the plot.
Both marks are drawn the same at every depth, so the stamp and the plan depth share one
vocabulary.

**The beds under the stamp (A), at 90 px and up.** When `side ≥ 90`, each phase cell's face is
that bed drawn as the plan depth draws it (`drawPlotFlat`'s blocks in `t`/`s`, one square per task
in its state colour with its glyph, stubble for done ones) scaled into the cell, and the progress
fill becomes a 2 px `l` strip along the cell's bottom edge, so the task squares stay visible. The
frame colours, the sprout/rock/fruit marks, the bar and the plate marks are unchanged. Zooming from
depth 1 to 2 then turns each cell into its bed: one continuous picture. On a 1280 px desktop the
default frame (about 66 px plates) shows the plain stamp; one zoom notch (z 1.5, about 99 px) adds
the beds. A phone (about 48 px) never shows them.

**Floor.** Under 36 px a side (`CELL_MIN` is 24 fine, 32 coarse, 4.1) the stamp drops to the bar
and the plate marks only; a 3×3 of 8 px cells is texture, not information.

**Code shape, for step 5.** `WORLD.plotRollup(plot, gardenerCount)` (pure, `world.js`) returns
`{cells: [{at, frame, fill, closed, sprout, rock, fruit, other, crate, empty}], bar: [{state, n}],
total, needs, gardeners}`; `ART.drawPlotRollup(ctx, pal, x, y, side, s, rollup)` (`art.js`) paints
it. `TestPlotRollup` covers the frame rules (current beats closed beats blocked beats review), the
fill, the empty-plan bar, the ninth-position crate and the Other bed. The `cursorproject` golden
rows (step 5) are shot at 1280 px and 390 px with the fixture farm holding at least one plan in
each of: on a phase with a blocked task, all phases done, needing a human, no tasks yet. The
blow-up in the mock (4×, "Homestead World") is the reference for what each mark looks like.

## 7. Phone

> **Revision 3.1 (gd-wAN7P4Armw):** there is no pad. Since revision 3 one finger pans and a pinch zooms, so the pad's
> arrows only moved a cursor the camera no longer follows, and its 60 px row cost the map more than it gave. Now the map
> runs down to the tab bar, and the title bar stands on the tab bar. A tap on a cell moves the cursor and a second tap on
> the cursor's cell goes in (on a leaf it opens). A double tap zooms a step about the point, and a pinch zooms freely.
> In and Out come back to the camera column on a phone, as 44 px targets clear of the tab bar and the title bar.
> There is no sibling jump on a phone: a tap on a Thing outside the current grid is already a cursor jump to it, and Go
> jumps anywhere. The keyboard keeps `Shift` and an arrow. What follows is the pad as it was, kept for its history;
> where it disagrees with this note, this note wins.

- **The pad** (N-D15) shows on `(pointer: coarse)` or ≤ 600 px, in the Farm view only. It is one
  row between the map and the tab bar, 60 px tall plus the safe area, and the map shrinks by that
  much only (`--pad`, added to the insets in 4.1). Left to right, with 16 px gutters and 8 px
  between groups:
  - four arrows `◀ ▲ ▼ ▶`, 44 px square, 4 px apart (188 px);
  - Select, 44 px, labelled **Jump**;
  - B and A, 48 px round, labelled **Out** and **In**, In (A) at the right edge under the thumb.
    The visible labels say what each does, not the console letter (gd-eh5fppdTT3).

  That is 352 px, which fits a 390 px phone. Under 376 px wide the arrows hide, and the row holds
  Jump, Out and In only. Swipes and taps cover moving. Every control is a `<button>` whose name
  starts with its visible label (WCAG 2.5.3): "Move left", "In: into the cursor's grid", "Out: up
  to the grid around it", "Jump: the next arrow jumps to the next grid". A held arrow repeats
  (300 ms delay, then every 90 ms).
- **Start** has no button. Its job, opening the bead view, is a tap on the full label, which is
  already where the eye is (6.2).
- **Select latches** for the next arrow press or swipe and lights up while latched, so a sibling
  jump is two taps with one thumb rather than a chord. Holding it also works (a chord).
- **Gestures** on the map (revision 3: one finger pans and two fingers zoom about their midpoint, depth
  following (4.3); a swipe is no cursor step and a pinch is no depth gesture; the rest of this item stands): a
  swipe (≥ 24 px, mostly along one axis) is one step in the swipe's direction. A tap on a cell moves the cursor there; a tap on a Thing outside the current grid is
  a cursor jump to it (`locate()` on the canvas, then `pathFor`). A second tap on the cursor's
  cell descends; on a leaf it opens the bead view. A tap on the full label opens the bead view. A
  pinch is the depth gesture (N-D6). This keeps today's two-tap rhythm (`HW §17`: the first tap
  pins, the second opens), with the cursor in place of the card.
- **Where the rest goes.** There is no minimap (3b R11 removed it on desktop too); depth 0 is the overview.
  The floating Projects button (`.projbtn`) goes: B to depth 0 is the project list, and the jump
  palette is still in the camera strip. The Commons button: PR #674's M1 moves the Commons to the
  tab bar. Until M1 lands, the Commons button stays where it is today, and the pad does not make
  room for it. The beacons (`HW §22`) treat the pad as a bottom inset, as they already do for the
  tab bar.
- Desktop shows no pad. The keyboard is the pad, and the camera strip's In and Out serve the
  mouse.

## 8. Phase 2 features under the cursor

| Feature | Today | Under the cursor |
|---|---|---|
| Beacons (`HW §22`, merged) | an edge arrow for a needs-you Thing off-screen; activating one flies there | "off-screen" means outside the current framing. Activating one is a cursor jump to the Thing at its natural depth (plot: depth 1). `n`/`N` walk the same list in the same clockwise order. |
| Weeds and fog (`HW §21`, merged) | thistles, fog, weed lines on the card | drawing unchanged; the lines move to the bead view (6.4); new rule `overgrown` (5.3) |
| Chore tour (PR #655, gd-S2Cn2dTsP0) | Tend walks five stops; the camera flies, a card offers an action | **the tour is a cursor walk**: each stop is `pathFor(stop)`, so the stop gets the full label and the ring is the cursor. The tour's action sheet stays a dialog, as #655 places it; Next is a cursor jump. Done or `Esc` ends the tour with the cursor on the last stop. `TOUR.frame()` gives way to `CURSOR.frame()` with the sheet as an inset. |
| Neighbours (PR #629, gd-MFcv2hSfue) | operator-coloured fences, Mine shade | unchanged; the operator name moves from the card to the plan's bead view and accessible name |
| Gardeners at their plots | sprites with hot links | still drawn at their plots and still clickable (to their Porch row). They are not grid cells; the plan's status line counts them. They walk between plots; "Go to plot" jumps the cursor into the gardener's plot, and a walk moves nothing (3.4; `homestead-agents-as-people.md` section 14). |
| Bead view (`HW §19`) | Show on the map flies | a cursor jump |

## 9. What each section of `homestead-world-design.md` becomes

| `HW` section | Becomes |
|---|---|
| 2 Coordinate model (camera, continuous zoom, snap on settle) | Camera from 4.1. z is framed, never driven; the snap rule applies to the framed z. Units and world px are unchanged. |
| 3.1, 3.2, 16 (domains, then packed regions) | The world lattice (5.1), memo v3. `h32` and its golden values stay. |
| 3.3 Plots, sticky spiral slots | Slots packed, no fallow holes (5.2, gd-VfOCGDpxg2); the plot is 32×32 at a 34 pitch; walked by `{c, r}`. |
| 3.4 Sticky memo | Kept (the slot entry is a rank); the promise is restated as N-D9. |
| 5 Semantic zoom | Depth replaces the level for text and hots; z-driven LOD remains for paint (4.2). Double-click and `Enter` behaviour per 3.2. |
| 6 LOD table, rules 2–3 | The hots row is "the current grid's cells". The labels row and rule 3 are replaced by section 6. The Project row's "coloured plate" becomes the rollup stamp (6.6). Rules 1, 4, 5 and 6 stand. |
| 7 Deep links | `at=` is the cursor's Thing; `z=` is ignored (3.5). |
| 8 Accessibility and input | Section 3.6 (grid role, roving focus, the hot is the cursor) and 4.3 (inputs). |
| 11 Camera build notes (input) | Superseded by 4.3. |
| 13 Minimap and fast travel | Cursor jumps (4.3); the minimap is gone (3b R11): depth 0 is the overview. |
| 14 Accessibility build notes | Outline kept and built from `CURSOR.tree`; reduced motion and the focus ring kept; coarse 44 px relaxed to 32 px inside the grid only (4.1). |
| 17 Cursor-aimed text and the info card | Removed (6.5). |
| 18 Text inside the window | Stands, and is the placement rule of 6.2: the label stays inside the viewport. |
| 20 Opening view | The opening cursor (3.5). |
| 21, 22 Weeds, beacons; 23 the chore tour (#655) | Section 8. |

## 10. Test plan

1. **`TestCursorUnderNode`** (`cursor_test.go`, the `TestPlansLogicUnderNode` pattern; `cursor.js`
   has no DOM):
   - `tree()` over the fixture farm and `testdata/world_tenx.js`: every placed Thing is in exactly
     one grid; coordinates match the layout rects; block-major order; Other and crate placement;
     the band row; the home cell's zones (revision 2.5).
   - `move`: one occupied cell per call by the rule of 3.2, skipping empties; the same farm gives
     the same walk; a bump when nothing is in that direction; one cell per press in a dense bed.
   - `jump`: the same coordinate in the next grid, clamping, nearest occupied, bubbling across
     plots and projects, a bump at the world edge.
   - `descend`/`ascend`: memory honoured, then the first child that needs you, then the first
     occupied; a leaf returns `open`.
   - `atFor`/`pathFor` round trip for every Thing in the 10x world, and through `WORLD.resolve`
     for old `at=`/`z=` links.
   - `follow`: by id, then same coordinate, then nearest, then ascend; a change in used bounds
     does not re-frame until the next move.
   - `frame`: used bounds versus window at 1280×800, 390×520 coarse and 1920×1080; a 5-task phase
     frames one block; the window scrolls only at its edge; the zoom snaps.
   - `signs`: exactly one Full; the container prefix; one line when it fits, two otherwise; tier
     budgets; siblings glyph-only at 1920 px; the placement slot follows the fixed order and never
     covers the Thing or leaves the viewport; output identical for the same path whatever viewport
     offset the camera has during an ease; it takes no pointer argument (a source check, like
     `TestReducedMotionCoversEveryAnimation`'s wiring checks).
2. **Layout** (`TestWorldLayoutUnderNode`, `world_tenx_test.js`, step 1): the lattice golden;
   memo v2 to v3 migration keeps slots; **coordinate stability**: adding a project, adding or
   closing a plan, and churning any state change no project's index and no plot's `{c, r}`; beds at
   the 3×3 offsets, no overlap, cells inside their bed; 9×9 capacity with 60 tasks plus a crate;
   past 9 phases gives eight beds, a crate and the `overgrown` weed. The golden is regenerated with
   `-update-world-golden`.
3. **Frame budget** (`TestWorldFrameBudgetUnderNode`): the sweeps become cursor walks (40 moves
   per depth, 10 descends and ascends, sibling jumps) instead of pans. Budgets: a move that does
   not scroll at most 2 ms of script and no chunk bake; a descend settles in at most 64 frames, as
   a fly does now. Label DOM nodes stay within the tier budgets.
4. **Chromium** (`TestCursorUnderChromium`, the `card_browser_test.go` harness, fixture farm plus
   the three five-plan projects from `HW §20`):
   - Desktop 1280×800: `Tab` lands on the cursor link; arrows move focus and the full label, and skip fallow slots; Enter
     descends to depth 3; `Esc` ascends; `Shift`+Right jumps plots; `at=` is written and a reload
     restores it.
   - **Flicker probe:** a `MutationObserver` on `#lbls` while the mouse sweeps 50 points across the
     board records **zero** mutations. One wheel notch is exactly one depth step.
   - Phone 390×844 (Pixel 5): the pad is one row of at most 60 px plus the safe area and its
     targets are at least 44 px; arrows, A, B and latched
     Select work; a swipe is one step; tap moves, tap again descends; a tap on the label opens the
     bead view; a full pinch spread descends and a 20% one springs back with no sign change; the
     pad and the tab bar do not overlap; at 360 px wide the arrows hide.
   - Reduced motion: no transition runs. Zero console errors.
5. **Goldens** (`make test-garden-goldens`, `tests/gardenhub-browser/GOLDENS.md`): new rows
   `cursorworld`, `cursorproject`, `cursorplan`, `cursorphase` on desktop and phone, light and
   dark. Existing rows that show the map are re-shot in the PR that changes them (steps 1, 3, 5,
   6), never in a later one.
6. **`/commons-ux-acceptance`** across C1–C12 on the four cursor rows. Special attention: C1 (the
   full label's one- and two-line cut and its placement slot), C3 (the pad at 390 px and 360 px), C8 and C12 (the grid role, roving focus,
   names with row and column), C9 (the current phase readable without text), C11 (one full label, no
   competing text). The grade goes on each step's PR, and the before/after PNGs on its bead.

## 11. Build steps

PR-sized, in order. The plan-gardener mints them under gd-7bfFegkYjS once this spec is accepted.
**Steps 1–3 wait for PR #629 and PR #655 to merge**: those touch `garden.js` and the world framing.

| # | Step | Touches | Depends on |
|---|---|---|---|
| 1 | **Square plots and 3×3 beds.** `world.js`: `SLOT` 32/34, `planBeds` as 3×3 beds of 9×9 block-major cells, crate past 9, stakes; the world lattice and memo v3; `shape()` without `RESERVE`; gardener spots re-derived. Art: block soil tones, stake with ordinal digit and pips, lit current stake. Still the free camera. Layout tests, 10x golden, goldens re-shot. | `world.js`, `art.js`, tests | #629, #655 merged |
| 2 | **`cursor.js`**, pure: tree, path, move, jump, descend, ascend, follow, atFor, frame, signs. `TestCursorUnderNode`. Not wired. | `web/cursor.js`, `cursor_test.go`, `page.go` embed | 1 |
| 3 | **Cursor drives keys, camera, hots, URL.** Arrows, Enter/A, `Esc`/B, `Shift`, `Space`, `+ - 0 Home n`; `CURSOR.frame` as the only camera owner; `#hots` as the current grid with `role=grid` and roving tabindex; `at=` write and read; `garden.cursor.v1`; outline from the tree; minimap as lattice; jump, beacons and bead view as cursor jumps. Arrow-pan is removed. Frame-budget sweeps rewritten. | `garden.js`, `garden.html`, tests | 2 |
| 4 | **Pointer and touch.** Click or tap = cursor, second = descend; wheel and pinch as the depth gesture with spring-back; drag-pan and double-click fly removed; swipe = one step. Chromium walk for each. | `garden.js` | 3 |
| 5 | **Signs and the plot rollup.** Render `CURSOR.signs`: the full label with its fixed placement order, child labels, glyphs, transitions; remove `aim`/`retarget`/`nearPlot`, the label rounds, the card and `cardplace.js`. Draw the plot rollup (6.6) at depth 1 in place of the flat plate: `WORLD.plotRollup` + `ART.drawPlotRollup`, `TestPlotRollup`. Flicker probe. Goldens re-shot, the `cursorproject` rows at 1280 px and 390 px. | `garden.js`, `garden.css`, `world.js`, `art.js`, tests, `cardplace.js` (deleted) | 3 |
| 6 | **Bead view carries the card's content.** Weeds, gardeners-here, operator and phase list sections for a plan; the `overgrown` weed. | `bead.js`, `weeds.js` | 5 |
| 7 | **Phone pad.** The one-row pad: arrows, Select latch, B and A, repeat, arrows hidden under 376 px; insets for the map, the beacons and the Commons; Projects button removed; minimap hidden on the phone. Phone goldens. | `garden.html`, `garden.css`, `garden.js` | 4; places the Commons per #674's outcome |
| 8 | **Tour as a cursor walk.** `TOUR` stops become cursor jumps; the sheet is an inset. | `tour.js`, `garden.js` | 3, #655 merged |
| 9 | **Docs and check-in.** `HW` build-notes section for the cursor world; `/commons-ux-acceptance` on the four rows; owner screenshots, desktop and phone, in the thread. | docs | 5, 7, 8 |
| 10 | **The bin by the plot (5.6, retired).** `FarmPlan.produce[]` in the farm read model (landed with this revision, `farm_produce.go`). `web/bin.js` pure rows and placement plus the `#bin` overlay; the four icons and the plot's crate mark in `art.js`; peek and go in `garden.js`; `i`/`Esc`; phone inset row. `TestBinUnderNode`; `cursorplan` golden re-shot with a fuller fixture; a `bin` golden row. | `web/bin.js` (new), `art.js`, `garden.js`, `garden.html`, `garden.css`, `page.go`, tests | 5, 7 |
| 11 | **Produce bin acceptance.** `/commons-ux-acceptance` C1–C12 on `cursorplan` and the phone row; owner screenshots in the thread; `HW` build notes. | docs | 10 |

Steps 4 and 5 can run in parallel after 3. Step 6 can run with 7 and 8.

## 12. Open questions (do not block step 1)

1. **Back on a phone.** Should a descend push a history entry, so Android Back ascends? It would
   feel native on a phone, but it competes with the bead view's history (`HW §19`) and with Back
   meaning "leave the page". This spec says no; easy to flip in step 3.
2. **How far to take N-D0.** The first draft's strict glyph-only question is decided (siblings stay
   glyph-only, 6.1). What N-D0 leaves open is how much predictability to trade next. Two
   candidates, both deferred to the step 5 check-in on screenshots: packing a project's plots so
   there are no fallow slots at all (gives up sticky slots, `HW §3.3`), and a plain arrow at a
   grid edge crossing into the sibling grid (gives up N-D2's guard against a held arrow).
   **Packing is decided** (owner decision gd-VfOCGDpxg2, 2026-10-22, option "pack"; built in
   gd-b228pTt33E): see 5.2, N-D3 and N-D9. The edge-crossing arrow stays open.
3. **Lattice width.** `W = 5` suits a wide screen. A phone sees a window of it. The width is a
   constant, so it can change once, before many people's memos hold v3. **Decided:** keep
   `W = 5` (owner decision gd-VfOCGDpxg2, 2026-10-22).
4. **Server-side phase cap.** Should `farm.go` (or `gb plan add-phase`) refuse a 10th phase, rather
   than the page showing a weed? It is a product rule, not a rendering one.
5. **Haptics.** `navigator.vibrate(5)` on a bump at an edge on Android? Cheap, and iOS ignores it.

## 13. Risks

- **The world gets bigger.** Plots grow about 5× in area. Chunk bakes per view grow too, so the
  frame budget (step 3) needs watching. Mitigations: the cursor framing shows less of the world at
  once than the free camera's fit did, and chunks already bake from cached tile bitmaps (`garden.js:191`, the `HW §15` follow-up).
- **Every golden moves** in step 1, and again in step 5. Each step re-shoots only what it changes,
  in its own PR, on merged main.
- **Muscle memory.** Today's users pinch and drag. The depth gesture keeps pinch meaningful.
  Dragging goes away, and the first swipe teaches the step.
- **Screen readers** in browse mode can take the arrow keys before the page sees them.
  `role=grid` switches NVDA and JAWS to focus mode on entry; step 3 checks VoiceOver on iOS with
  the pad's buttons as the fallback path.

## 14. Changes from the first draft (revision 2)

The owner answered decision gd-jcC1S7FXbV: loosen the diegetic rule to save space; space and
attention matter more than predictability; both is best. What moved:

| Was | Now | Why (N-D0) |
|---|---|---|
| Sign boards on posts, gate signs in a reserved 2-tile path, stakes in reserved paths | one compact overlay label at the Thing, in a fixed placement order; no tile reserved for text; stake glyphs painted on existing path tiles (6.2, 5.2) | space |
| A container gate sign as a separate tier | the container's name is a dim prefix on the label (6.1) | attention: one text object |
| Title and status always on two lines | one line when it fits, else the lineage, the whole title and the status on as few lines as they need; zero parts dropped (6.1, 6.4) | space |
| The cursor rests on empty cells | empties are skipped; arrows go to the next occupied cell in that direction (N-D3, 3.2) | attention |
| Descend lands on memory, else the first cell | memory, else what needs you first, else the first cell (N-D4) | attention, with memory keeping it predictable |
| The camera frames the whole container | it frames the used bounds; data changes wait for the next move (N-D5, 3.4, 4.1) | space, with the wait keeping it calm |
| A 148 px Game Boy deck with Start | a 60 px one-row pad, arrows hidden under 376 px; Start is a tap on the label (N-D15, 7) | space |
| Open question: how strict glyph-only is | decided: siblings stay glyph-only (6.1); the open question is now how far to take N-D0 (12.2) | attention |

Unchanged: the cursor path and keys, sibling jumps, the depth gesture, the grid of grids and plot
geometry, the rule that text changes only on a cursor move, the test plan's shape and the nine
build steps.
