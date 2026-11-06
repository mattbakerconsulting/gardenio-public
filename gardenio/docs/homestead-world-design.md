# Homestead World: coordinate model, layout and zoom levels

Design for Phase 1 of the Homestead World plan (epic gd-oi3liYjNRA, phase gd-NJWmhHcu99,
task gd-jHT9DIiAMl). It is the contract the Phase 1 build steps share:

| Step | Builds | Reads this page for |
|---|---|---|
| gd-dJjXKBP2II | camera, chunked canvas, hot virtualisation | sections 2, 5, 6 |
| gd-hoJXhBwLMw | project biomes | sections 3, 4 |
| gd-gFUEJRyVRr | minimap, `/` jump, deep links | sections 2, 7 |
| gd-s69orAcBCf | keyboard outline, reduced motion, coarse targets | sections 5, 8 (build notes: 13) |
| gd-K2bb18BKFu | perf budget and layout-stability tests | sections 3.5, 6, 9 |

Phase 1 needs **no backend change**. Everything below is computed in the browser from the
documents the page already holds: the `/garden` snapshot and the `/garden/farm` document
(`homestead-farm-read-model.md`). One additive server field is proposed in section 10, and
nothing here depends on it.

> **Cursor world (phase 2b, gd-7bfFegkYjS), live on `main` since 2026-10-21.** The free camera and the pointer-aimed
> text were replaced by an LSDJ-style cursor over a grid of grids, with signs that change only when the cursor
> moves: [`superpowers/specs/2026-10-20-lands-cursor-grid-design.md`](superpowers/specs/2026-10-20-lands-cursor-grid-design.md)
> (task gd-Fe972f8H9B). That spec supersedes the camera of **section 2**,
> project placement in **3.1, 3.2 and 16** (now a row-major lattice), the plot size of **3.3** (now
> 32 x 32 with 3 x 3 phase beds; slots are kept), the stability promise of **3.4** (now restated
> for grid coordinates), semantic zoom in **5**, the labels and hots rows of **6**, `z=` in **7**,
> input in **8, 11 and 13**, the hots of **14**, the whole of **17** (cursor-aimed text and the
> info card are removed), and the opening view of **20**. Sections 21 and 22 (weeds, beacons) and
> the chore tour change as its section 8 says. Its section 9 maps every section here to what it
> becomes. All its build steps have merged: **section 26** records what shipped and where it differs from the spec, and
> where this page and the spec disagree, the spec and section 26 describe what is live.
> Section 27 lists, phase by phase (2b, 3, 3b, 4), where the live page differs from this design.
> Section 28 is the accepted design that replaces the shared Yard with a home strip in every project (plan gd-LurZOR44M1);
> until its build steps merge, sections 3.1, 26.10 and 26.12 still describe the live Yard.
> Section 29 is the accepted design that moves each project's farmstead into its centre plot slot, with lanes and hedges
> (plan gd-rLs7EoTa7a); until its build steps merge, section 28 describes the live home strip.

## 1. Facts this design rests on

- The farm document already carries `project` on every plan, seed, closed plan and meadow
  bead. Beads with no `project:` label have `project: ""`.
- The live daemon has **one** project today (`gardenio`). The world must look right at N=1 and
  stay stable at N=50. Do not tune for the current count.
- A plan is a plot with up to 9 phases; a phase lists at most 60 tasks (`more` counts the
  rest); a task is one plant. The farm is bounded (about 300 KB raw at 10x), so the client
  can hold and lay out the whole world in memory. No server paging is needed.
- The art is 16 px tiles drawn with `fillRect` (`web/art.js`). The existing 384x240 scene
  (24x15 tiles: barn, mailbox, dog, growing field, signposts, villagers) is kept intact as the
  **Yard** (section 3.1), so nothing regresses.
- Pure logic lives in a DOM-free module that Go tests run under node (`TestPlansLogicUnderNode`
  is the pattern). The world layout follows it: new file `web/world.js`, exported as `WORLD`,
  embedded and hashed into `assetsTag` like the other sources.

## 2. Coordinate model

Three spaces, one direction convention (x east, y south, matching canvas):

| Space | Unit | Use |
|---|---|---|
| **Tile** | 1 tile (integer) | All layout. Domains, plots, slots, bands are tile-aligned. |
| **World px** | 1 logical px = 1/16 tile | What `art.js` draws in. Sprite pixel = 1 world px. |
| **Screen** | CSS px | Camera output. `screen = (world - cam.center) * cam.z + viewport/2`. |

- The world origin `(0,0)` is the **centre of the Yard**. Tile `(tx,ty)` covers world px
  `[16tx, 16tx+16)`.
- **Camera** is `{cx, cy, z}`: centre in world px and zoom in CSS px per world px. `z = 1` is
  the native pixel art (one art pixel = one CSS pixel, a tile is 16 CSS px). Valid range
  `Z_MIN = 0.04` to `Z_MAX = 4`. The camera clamps its centre to the world bounding box plus
  one domain of slack, so you cannot lose the map.
- **Device pixels.** The canvas backing store is `viewport * devicePixelRatio`. Draw sprites
  with `imageSmoothingEnabled = false`. When the camera rests at `z >= 1` it snaps to the
  nearest value of `{1, 1.5, 2, 3, 4}` so pixel art stays crisp; pinch and wheel move
  continuously and snap on settle. A gesture that leaves a resting zoom by 8% or more but
  not far enough for the nearest to differ rests one level on in its direction, so a modest
  pinch never springs back to where it began. New input cancels a pending settle. Below 1
  smoothing is on, because the terrain is downsampled.
- **World size is open-ended, not fixed.** It is the bounding box of the occupied domains
  (section 3.2), recomputed from the data. There is no hard-coded map size.

### 2.1 Pure API (`WORLD`, no DOM)

The camera, biome, minimap and test steps all call these, so they cannot disagree:

```text
WORLD.h32(str, seed) -> uint32                    // section 3.2, golden-tested
WORLD.layout(doc, memo, opts) -> World            // section 3; memo is the sticky store
WORLD.lodFor(z) -> 0|1|2|3                        // section 5, a pure function of z
WORLD.visible(world, rect, level) -> VisibleSet   // section 6, two-level cull
WORLD.fit(bbox, viewport, opts) -> {cx, cy, z}    // frame a rect; used by every "fly to"
WORLD.resolve(world, at) -> {level, bbox, kind}   // deep-link target; section 7
```

`World` is plain data: `{yard, commons, biomes[], bbox}`. A biome is
`{key, name, domain:{i,j}, palette, seed, used:{bbox}, plots[], nursery[], orchard[], meadow[]}`.
Every placed thing has `{id, kind, x, y, w, h}` in tiles, so culling, hit-testing, the
minimap and the outline all read the same rectangles.

## 3. Deterministic layout

Goal: the same set of projects and plans gives the same map in every browser, a project never
moves because its neighbours' beads change, and growth is outward, not a reshuffle.

### 3.1 Domains: one fixed cell per project

> **Superseded for placement by section 16 (phase 1b, gd-pXUGI64tyk).** Projects are no longer
> one domain cell each; they are packed regions a plot apart around the Yard. The domain
> constant survives only as the Yard's rect (`domainRect(0,0)`), the camera's clamp slack and
> the empty world's bbox. Sections 3.3 and 3.4 (slots, memo) still hold as written, with the
> memo at v2.

The world is a lattice of **domains**, each 176 x 144 tiles (2816 x 2304 world px). Domain
`(i,j)` spans tiles `x in [176i - 88, 176i + 88)`, `y in [144j - 72, 144j + 72)`.

- Domain `(0,0)` is the **Yard**: the current scene, drawn with its centre at the origin
  (tiles `x -12..11`, `y -8..6`). It keeps the barn, mailbox, dog, the Growing field, the
  signposts and the villagers (agents) exactly as today. The barn is where agents rest; Phase 3
  walking gardeners later leave it for their plots.

  > **Where a gardener stands is the agents-as-people plan's (gd-AbFYcF2BMr), not this
  > layout's.** The farm document's `gardeners[]` gives each live agent a `plot` (`{plan,
  > phase}`, from the same ancestor walk that places its task's plant; read model, FarmGardener).
  > `WORLD.gardenerSpots` puts it on that plot's bottom fence under its task's column when the
  > plot is in the world and its bucket is `alive` or `stuck`; everyone else is at the barn
  > (`homestead-agents-as-people.md` section 4). So Phase 3 "walking gardeners"
  > (gd-gMMtrgIT1W) adds no placement of its own: a walk animates a change of `plot` (or of
  > column) for the same `id` between two farm snapshots. The truck ride out of the barn and the trip home are agents-as-people section 7.
- Domain `(1,0)` is the **Commons**: the biome for beads with no `project:` label, and for
  anything whose project cannot be told. Pinned, so it never competes with real projects.
- Every other project gets one domain, found by the placement below. A domain is a fixed
  footprint, so a project's bounding box never changes with its data and a growing project
  cannot push a neighbour. The unused margin is the **wild**: forest, ponds and stream,
  generated from the same hash. That margin (at least 50 tiles between two biomes) is the
  visible "separation by project".

Why a fixed cell and not a packed treemap: a packed layout moves everything when one project
gains a plan. This one moves nothing.

### 3.2 Placing a project: hash, then probe

> **Superseded by section 16.** `h32` and its golden values stay (palette, seed, band cells and
> the tie-break of the cold order still use it); the probe is gone.

```text
h32(s, seed): h = (2166136261 ^ seed) >>> 0
              for each UTF-16 code unit c of s:  h ^= c;  h = imul(h, 16777619) >>> 0
              h ^= h>>>16; h = imul(h, 0x85ebca6b)>>>0; h ^= h>>>13
              h = imul(h, 0xc2b2ae35)>>>0; h ^= h>>>16;  return h >>> 0
```

FNV-1a, then a murmur3 finaliser so the low bits are usable. (`art.js` `hashStr` is bare
FNV-1a and stays for sprite picks; do not reuse it for layout.) Golden values:

| name | `h32(n,1)` priority | `h32(n,2)` palette | `h32(n,100)` first cell |
|---|---|---|---|
| `gardenio` | 3263587388 | 1292538903 | 3456188741 |
| `music-clonk` | 700575029 | 474542740 | 1277858624 |
| `moorpark` | 3971456384 | 4134240447 | 515575165 |

Project key = the `project:` label value, trimmed and lowercased. The display name keeps the
original case.

```text
taken = { (0,0): yard, (1,0): commons }
for name in projects sorted by (h32(name,1), name):        // the priority order
  for k = 0, 1, 2, ...:
    R = 2 + k;  side = 2R + 1;  h = h32(name, 100 + k)
    i = (h % side) - R;  j = (floor(h / side) % side) - R
    if (i,j) not in taken: taken[(i,j)] = name; break
```

Attempt `k` draws from the `(2R+1)^2` window around the Yard: 5x5, then 7x7, and so on.
It always terminates because the window grows. A lone project lands within two cells of the
Yard. Examples (computed): `gardenio` alone is at `(-1,1)`; with `music-clonk` and
`moorpark`, `music-clonk` is `(2,2)` and `moorpark` `(-2,1)`; all land on their first
attempt.

Measured on 200 random project sets per size (a script, not shipped):

| Projects | Needed a 2nd attempt | Mean distance from Yard (domains) | Existing project moved when one is added (cold) |
|---|---|---|---|
| 1 | 4.5% (the Yard cell) | 1.7 | n/a |
| 12 | 24% | 1.9 | 1.6% of projects, about 1 add in 5 moves one |
| 30 | 48% | 2.3 | 1.6% |
| 60 | 70% | 3.0 | 1.4% |

So a cold computation is mostly stable but not perfectly: adding a project whose priority
beats an existing project that collides with it displaces that one. Section 3.4 closes the
gap.

### 3.3 Plots: sticky spiral slots inside the biome

Inside a domain, plans occupy **slots** on a fixed 7 x 5 grid centred on the domain centre.
A slot is `16 x 12` tiles (one plot) at pitch `18 x 14`, so there is a 2-tile path between
plots. Capacity is 35 plots per biome.

Slot order is a square spiral from the centre, clipped to the grid: slot 0 is the centre,
slots 1..8 ring it clockwise from the top-left, slots 9..24 are the 5x5 ring, slots 25..34 are
the outer columns. **Slot `k` is always at the same place, and the biome fills outward.**

**Packed** (owner decision gd-VfOCGDpxg2, 2026-10-22; cursor-grid spec 5.2 and 12.2; built in
gd-b228pTt33E). A project's live plans fill slots `0..n-1` with no holes. Order, in authority:

1. **Memo.** The plans the memo holds a slot for come first, by that slot ascending (ties by
   `(created asc, id)`). The memo'd slot is read as a rank, not a fixed home.
2. **Cold order.** Then the newcomers, in `(created asc, id)` order. Both are immutable, so every
   browser derives the same cold layout from the same plan set.

**Staked plots** (plan gd-2RdeAx8nnW, step 2). A `ready` or `asked` seed of the project ranks with its plans for a slot,
by the same two rules (the memo's `slots` map is keyed by bead id, so a seed id ranks beside the plan ids with no memo
change). So a plan waiting on its owner's answer is laid out in the fields as a **staked plot** (`kind: "plot"`,
`staked: true`, `seed` its FarmSeed) in the slot it will keep: approving it moves its id from `seeds` to `plans` (`seeds.rm`
+ `plans.up`) and it is planted in place, nothing moves. Its `plan` is a plan-shaped record built from the seed
(`WORLD.stakedPlan`): no phases, one unphased bed (`id: ""`, titled Steps) with one cell per entry of `steps`, in step
order, each in state `planned`; the bed is there even before the seed has steps. Its one human wait is its open decision
(`needs_human` 1 while it is `asked`). A document caught mid-approve, with the id in both lists, lays out the plan's plot
only. A staked plot counts toward the 35-slot cap, the overflow marker and the biome's `used` rect like any plot. Drafts
(`frosted`) stay pots in the Nursery.

The i-th plan in that order takes slot i. Until the decision, a plan kept its memo'd slot for good
and a closed plan left a fallow hole until a new plan refilled it. That cost the cursor (whose
arrows skip empties) a gap on screen and gave nothing back once the cursor walked by coordinate.

Over 35 plans in one project: the first 35 are placed, the rest collapse into one overflow
plot-sized marker labelled `+N more plans` that links to that project's list in the outline.
A truncated farm (`meta.truncated`) shows the same banner the Plans panel shows today.

A closed plan frees its slot, and the plots after it in spiral order step back one slot each,
keeping their order. The plots ahead of it stay. A new plan takes the next slot after the others,
so adding a plan moves nothing. Only the tail of the outer ring can be empty: a 7-plan project is
a 3x3 with its last two slots free, which follows from the symmetric shape.

Other things in the biome sit in bands anchored to the **used** plot rows, so a biome with two
plans is compact, not stretched across a 35-slot canvas:

| Band | Holds | Cap shown | Placement |
|---|---|---|---|
| Orchard | closed plans (`closed_plans.shown`, that project) as 1-tile trees | 24 | one row, 3 tiles above the top used row |
| Meadow | loose beads (`meadow.shown`, that project) as wildflowers | 40 | 7-tile strip above the Orchard |
| Nursery | drafts (`seeds` with `state: "frosted"`, that project) as 3x3 pots; a ready or asked seed is staked out on a plot instead | 40 | two rows of pitch 4x5 below the bottom used row |

Wildflower and pot positions come from `h32(id, 3)`, jittered inside their band, so they do
not need a slot memo. Used rows grow 1 -> 3 -> 5 at the 2nd and the 10th plan, so the
bands step outward by 14 tiles at those two counts only. The biome step eases that move.
The label sign sits above the topmost band.

Biome terrain footprint: the used rect (plots plus bands) padded by `3 + 2*noise(tx,ty,seed)`
tiles. The palette is `h32(name,2) % 8` and the decor seed `h32(name,3)`. Colour and art are
gd-hoJXhBwLMw's; this page fixes only their inputs so they are reproducible.

Piles from `farm.piles` (closed decisions, escalations, work) have no project. They live in the
Yard as the compost heap and the archive shed, not in any biome.

### 3.4 Sticky memo: why layout holds when beads change

The pure functions give the cold answer. The **memo** `{v:2, domains:{key:{x,y,r,t}}, slots:{planOrSeedId:slot}}`
(v1 held a domain cell `{i,j}` per project; section 16 has the v2 shape and migration) makes
the answer sticky in a live browser:

- It is read from `localStorage` key `garden.world.v1` (try/catch every access; the page must
  render with it empty) and written after each layout that changed it.
- `layout()` places every memo'd project and plan first, exactly where the memo says, then
  places newcomers with the rules above on top. A newcomer is never allowed to take a memo'd
  cell, and a new plan ranks after every memo'd one. So in one browser, **adding a project or
  plan moves nothing already placed.** Closing a plan moves only the plots after it in its
  project, one slot each (packed slots, 3.3); the memo is rewritten with their new slots.
- A project or plan that leaves the document keeps its memo entry for 30 days, so a project
  that goes quiet and returns finds its old place. The memo is capped at 256 projects and 4096
  slots, oldest-seen dropped first.
- Layout never writes the memo from a stale read. If the document is the empty or truncated
  one, `layout()` is called with `opts.readonly = true`.

Known limit, stated plainly: two browsers with different histories can differ after churn,
because the memo is per browser. Cold layout still agrees whenever the project set and plan
set match. Anything addressed by name, `?at=<project>/<id>`, stays correct either way, since
it resolves through `layout()`, not saved coordinates. Section 10 lists the server-side fix.

### 3.5 What the tests must pin (gd-K2bb18BKFu)

Golden tests, run under node by a Go test like `TestPlansLogicUnderNode`:

1. `h32` golden values (table above) and the three-project example layout.
2. Determinism: shuffling the project list and the plan list gives byte-identical output.
3. Stability: with a memo, adding a project or plan, closing a plan, or changing any bead's
   state or title changes **no existing rectangle** (assert deep-equal on every placed
   `{id,x,y,w,h}` that survived).
4. Spiral: slot 0..34 positions are unique, inside the grid, and the same for every N.
5. Capacity: 36 plans in one project gives 35 plots plus one overflow marker.
6. No overlap: no two placed rectangles in a biome intersect; every one is inside its domain.
7. Empty and N=1 documents lay out without error (an empty world is the Yard alone).

## 4. What the biome step receives

For each project the biome step gets `biome.used.bbox`, the plot rectangles, the band
rectangles, the palette index and the seed. It draws terrain, the fence ring and the label
sign. It must not choose positions. A biome with only seeds, only meadow beads or only closed
plans is still a biome (it has a sign and bands, no plots).

## 5. Semantic zoom: four levels

The level is a pure function of the camera zoom `z` (CSS px per world px). A tile is 16 world
px, so the middle column is "CSS px per tile".

| Level | Name | `z` range | px / tile | Question it answers |
|---|---|---|---|---|
| 0 | **World** | `z < 0.11` | under 1.8 | Where are my projects, and which ones need me? |
| 1 | **Project** | `0.11 <= z < 0.41` | 1.8 to 6.6 | What plans does this project have, and what is each doing? |
| 2 | **Plan** | `0.41 <= z < 0.92` | 6.6 to 14.7 | What phase is this plan in, and what is stuck? |
| 3 | **Bead** | `z >= 0.92` | 14.7 and up | What exactly is this task, and who has it? |

No hysteresis: `lodFor(z)` is the same level whichever way the camera arrived, so one zoom is
always one rendering. (Until gd-bwI1PZ6jnM a band of 8% about each boundary kept the previous
level, and a plan's phases were the Project depth's rollup boxes when zooming in but the Plan
depth's beds when zooming out to the same zoom; each boundary now sits where the finer level
used to take over on the way out.) Programmatic moves (fly-to, deep link, double-click) land at
a `z` well inside the target level: Project 0.3, Plan 0.7, Bead 2. `fit()` clamps its result
into the level range the caller asked for.

Moving between levels:

- Wheel, pinch and `+`/`-` change `z` continuously. Each level change is a pure re-render,
  with no fetch (all data is in memory).
- Double-click or `Enter` on a biome, plot or bead flies one level in, centred on it.
  `Backspace` or `Esc` flies one level out. Keys and clicks use the same `fit()`.
- Reduced motion: every fly-to is a jump cut and nothing animates (section 8).

## 6. Level of detail: what is drawn, and what is in the DOM

Culling comes first, level second. `visible()` tests the viewport (padded by 25%) against
domain bounding boxes, then plot and band rectangles, then, only at levels 2 and 3, the
tasks inside a visible plot. At 10x (360 plans, 50 projects) a viewport can hold at most
about 12 plots at level 2 and 4 plots (up to about 430 tasks, typically under 150) at level 3.
No spatial tree is needed at these counts; the two-level rectangle test is cheap.

| | L0 World | L1 Project | L2 Plan | L3 Bead |
|---|---|---|---|---|
| Terrain | one flat grass fill | one flat grass fill | native chunk | native chunk |
| Yard | barn, mailbox glyphs only | simplified | full scene, static | full scene, animated |
| Biome | filled blob in palette colour, name | blob, fence, name sign | fence, sign | fence, sign |
| Plan plot | not drawn | rect coloured by `tile().crop`, plan title | phase beds as rows, state-coloured squares | beds with sprites (`art.js`), plant titles on focus |
| Task | not drawn | not drawn | 1-tile coloured square per task (flat) | full sprite, wilt/fruit/blocked states |
| Seeds, orchard, meadow | not drawn | counted in the sign | dots | pots, trees, wildflower sprites |
| Beacon for "needs you" | `!N` on each plan's stamp and board row (the pulsing ring went, gd-GgKjBgsc0U) | `!N` on the plot's rollup (no ring) | ring on the plan | `?` / `!` bubble (existing `drawBubble`) |
| Labels | project names (and plot short signs where a plot is 20 px), **max 24** | project names, plan signs, **max 64** | phase titles, **max 64** | task titles on hover/focus only |
| Focusable DOM (`#hots`) | one link per **biome in view** | one link per **plot in view** | one per plot + decision/escalation | one per **task in view**, **max 300** |
| Villagers (gardeners) | heads on each plan's stamp and live counts on the boards (gd-H6ZPz5V262); counted on biome signs | heads on the plot's rollup; counted on biome and plot signs | on their plots, or beside the wildflower, pot, tree, headstone or goat their task is drawn as (3 per stand, then "+N"); home ones at rest in the Yard | on their plots or beside their band item (6 per stand) and in the Yard; a change of plot walks (Phase 3) |
| Animation | none | none | none | existing 4-frame sprite loop, 6 fps cap |

Rules that hold at every level:

1. **State is never colour alone.** Each state keeps its glyph or shape (rock, weeds, fruit,
   wilt, `?`) at levels 2 and 3, and the label text at 0 and 1 includes a count word
   (`3 need you`). Contrast follows `TestPaletteTextContrastAA`.
2. **The DOM is virtualised.** `#hots` holds only what is in the padded viewport at the
   current level, rebuilt when the visible set changes (not every frame). It is capped at 300
   nodes; above the cap, the nearest 300 to the viewport centre win and the rest stay
   reachable through the outline (gd-s69orAcBCf). `aria-hidden` labels stay out of the DOM
   cap.
3. **Labels are screen-sized.** Font stays 12 CSS px at every `z`; position follows the world
   anchor. Labels are placed in priority order (needs-you first, then in-flight, then
   newest) and any label that would overlap one already placed is dropped, never shrunk.
   The per-level maxima above are hard.
4. **Chunks.** Static terrain is cached in 512 x 512 world-px chunks (32 x 32 tiles), built
   lazily from `(chunk, palette, night)`, with an LRU of 64 native chunks. World and Project draw no chunks: their
   terrain is one flat grass fill under the biome blobs (the 1/4 and 1/16 mips were that grass, scaled; gd-fsuxCGGbZr).
   Only chunks that intersect the viewport are drawn. A data change invalidates only the
   chunks under the changed biome's `used.bbox`. Day/night flips every chunk (the sky is the OS
   preference, the clock from 19:00 to 07:00 local, or the Menu's Sky item, the header's Sky button before 3b R10: `web/sky.js`, gd-SwRqFcjWc6).
5. **Dynamic layer is redrawn only on change.** Plants and state glyphs are drawn per
   visible item; the layer repaints when the farm version, the visible set, the level or the
   animation frame (level 3 only) changes. Nothing repaints on a pure idle frame.
6. **Frame budget.** Pan and zoom at 10x farm hold 60 fps on a mid laptop: at most 16 ms per
   frame, of which layout is 0 (it ran at data time, not frame time). The perf step measures
   this; the thresholds above are what it tunes if it misses.

## 7. Identity, deep links and search

- Every placed thing has a stable id: `project:<key>`, a plan or seed id, a task id, or
  `yard`. A bead id resolves to its plot through the farm document (task, plan, phase), else
  to the meadow in its project, else it is "not in the world" (closed, or past a cap).
- Deep link: `/garden?at=<project>/<id>&z=<n>`, where `n` is the **level** 0..3 (not a raw
  zoom, which depends on screen size). `at` may be `<project>`, `<project>/<planId>` or
  `<project>/<beadId>`; the project segment is URL-encoded, and `~` means the Commons.
  `resolve()` picks the target rectangle and `fit()` frames it at level `n`, defaulting to
  the target's natural level (project 1, plan 2, bead 3). An unknown id falls back to the
  project and says so in a toast. The camera writes the URL with `history.replaceState`
  after it settles, never during a drag.
- A seed id is a staked plot's id while the seed is `ready` or `asked`, and the same id stays that plot's when the plan is
  approved, so `?at=<project>/<plan id>` and the cursor reach it before and after. A staked plot's steps are its cells,
  by step id. A draft's id is its pot in the Nursery.
- The `/` jump and the minimap search index `{name, id, title}` for every placed thing, built
  from `World` once per layout.
- Minimap: world bounding box scaled into a 160 x 110 CSS px corner canvas. It draws one
  rectangle per biome, a ring for any biome that needs a human, and the camera frustum. It is
  redrawn per layout and per camera change, not per frame of data.

## 8. Accessibility and input

- The canvas is `aria-hidden`; the outline is the accessible model. The outline is a
  `project > plan > bead` tree that mirrors `World` exactly (same ids, same order as the
  slots), always in the DOM but off-screen-clipped, not `display:none`, and it works with
  no camera at all. Selecting an entry flies there and moves focus to the matching hot link.
- Keyboard camera: arrows pan one tile (`Shift` = one plot), `+`/`-` zoom, `0` fits the world,
  `Home` goes to the Yard. Focus in `#hots` follows the spiral slot order, so Tab order is
  stable.
- `prefers-reduced-motion`: fly-to jumps, no ring pulse, no sprite animation.
- Coarse pointers: hot targets are at least 44 CSS px. Where a tile is smaller on screen
  (levels 1 and 2), the hot link is the plot or biome, not the task.
- Touch: one finger pans, two fingers pinch-zoom, double-tap flies in. The page must not
  scroll under a pan (`touch-action: none` on the board only).

## 9. Decisions taken, with the alternative

| Decision | Why | Rejected |
|---|---|---|
| ~~Fixed 176 x 144 domain per project, wild between~~ Packed regions one plot apart (section 16) | The owner found the fixed cells "incredibly far apart"; a region reserves room to grow and the memo keeps its centre, so neighbours still never move | Fixed cells: four projects spread over five domains of grass |
| ~~Hash plus probe, priority by hash~~ Nearest free centre, oldest project first (section 16) | A new project is the youngest, so it never displaces one; the Commons leads so loose beads sit by the Yard | Hash priority: a new name could out-rank and displace an old one cold |
| Spiral slots, lowest free slot | Fills outward; slot `k` fixed forever; compact when small | Row-major grid: a 1-plan project sits in a corner |
| Per-browser memo on top | Makes growth in one session move nothing, with no backend | Server ledger now: needs a new write path (section 10) |
| Level by `z`, deep link by level | The same link frames well on a phone and a monitor | Raw zoom in the URL |
| Yard unchanged at the origin | Zero regression of today's scene and tests | Folding the Growing field into plots now: two data sources to join |
| Bands anchored to used rows | Small projects look small | Fixed bands: a gap of 40 tiles under a 1-plan biome |

## 10. Open questions (not blocking Phase 1)

1. **Shared map after churn.** The memo is per browser. To make everyone see the same map, add
   a `projects: [{key, name, created}]` field to `/garden/farm`. It is additive (no
   `meta.schema` bump) and costs one daemon list of `type=project` beads, already in the slow
   tier's budget. Use `created` instead of the hash as the placement priority, so a new project
   is always the youngest and can never displace one; the cold stability row in section 3.2
   goes to 0%. It also lets empty projects show a biome. Suggested as a Phase 2 step; a durable
   per-slot ledger would go further and is only worth it if people complain.
2. **Where in-progress beads live.** Today the Yard's Growing field shows the cross-project
   in-progress beads from the snapshot. When Phase 3 draws agents at their plots, those beads
   would be better drawn in their plan's plot, joined to the farm tasks by id. Phase 1 keeps
   the Yard field; revisit at the Phase 2 check-in.
3. **Visibility scope.** Still the existing open question (gd-uzktet9xCC): the farm document
   is admin-only and viewer-neutral. The world inherits that. "Mine" only highlights; it
   never hides a biome in Phase 1, so a project-scoped cut later is a filter on `layout()`'s
   input, not a change to positions.
4. **Per-project plot cap.** 35 plots is generous (the whole farm holds 36 plans today, in every
   state and across every project). If a project passes it, widen the grid by adding spiral rings; slots 0..34
   stay where they are, so this is backwards compatible.

## 11. Camera build notes (gd-dJjXKBP2II)

What the camera step shipped, and where it differs from the sections above:

- `web/world.js` is the pure half of `WORLD`: `h32` (golden-tested), `lodFor`, `levelRange`, `snapZ`, `zoomAt`, `panBy`,
  `clampCam`, `fit`, `cull`, `nearest`, `chunks`, `lru`, `domainRect`. `layout`, `visible` and `resolve` are left to the biome
  and link steps; they add to the same object. `visible` is `cull` over the plot and task rects those steps produce.
- **Chunks** are always 512 x 512 bitmaps of native terrain (`chunks` and `chunkSpan` still take a mip; the page uses 1).
  Chunks hold wild grass only: the Yard is its own
  384 x 240 bitmap blitted on top, which keeps chunks valid across data updates (only day/night rebuilds them).
- **Input**: drag and one-finger pan; wheel and trackpad pinch zoom about the pointer; two-finger pinch; double-click or
  double-tap flies in 2x; on-screen `+ - Yard All` buttons; keys `arrows` (one tile, never under 40 px; `Shift` one plot),
  `+ -`, `0` (whole world), `Home` (Yard). At rest at or above 1x the zoom snaps to `{1, 1.5, 2, 3, 4}`.
- **Hots** are a child-diff, not a rebuild: the links for Yard items inside the 25%-padded viewport, nearest 300 at most,
  under one CSS transform. Beads are links from level 2 (decisions) and level 3 (all); the mailbox, barn and dog at every level.
- **Deviation**: the Yard keeps animating at level 2 and closer (the design said level 3 only) because the default fit view
  of the Yard on a phone is level 2, and today's page animates there.
- Not built here: LOD glyph simplification of the Yard at levels 0 and 1, the minimap, deep links, and 44 px coarse targets.

## 12. Biome build notes (gd-hoJXhBwLMw)

What the biome step shipped, and where it differs from sections 3, 4 and 6:

- `web/world.js` gains the pure half of the layout: `layout(doc, memo, opts)` (domains by hash and probe, memo'd projects
  and plans first, spiral slots, bands, sign, `used` and `foot` rects in tiles, an `index` by id, the memo to persist and
  `memoChanged`), `visible(world, view, level)` (the two-level cull), `inFoot` (the terrain fringe), `slotRect`, `SPIRAL`,
  `tiles`, `union`, `projectKey`, `emptyMemo`. `resolve` is left to the deep-link step; `index` gives it the rectangles.
  `TestWorldLayoutUnderNode` pins the section 3.5 list (golden placement, determinism, memo stability, spiral, capacity,
  no overlap, empty document).
- **Data path.** `plans.js` already holds the farm document; it dispatches a `farmdoc` DOM event on load, reset and every
  patch, and `garden.js` lays the world out from that. Neither file reads the other's state.
- **Memo** is `{v:1, domains:{key:{i,j,t}}, slots:{planId:{s,t}}}` in `localStorage` `garden.world.v1`; `t` is last seen
  (ms) for the 30-day retention and the caps. A memo'd cell that is the Yard or the Commons is refused. The memo is read
  only when the document is empty or `meta.truncated`.
- **Bands** have a minimum width of 52 tiles (three slots) so a one-plan biome still holds a usable nursery: two rows of
  pots at pitch 4 hold 26 there (the 40 cap is reached from five used columns). Flowers sit in 2 x 2 cells of the meadow
  strip and pots and trees in their grid cells, each picked by `h32(id, 3)` with a linear probe past a taken cell, so
  they need no memo. Used rows are symmetric about the centre (`2 * max|row| + 1`), so bands step at the 2nd and 10th plan.
- **Terrain is baked into the native chunks**: biome ground (the palette's grass with a one-pixel dark rim), the fence
  ring around `used` padded by one tile, and each plot's fence and soil beds. A chunk is rebuilt only when a biome whose
  footprint meets it changes (`used`, palette, seed, or its plot set). Levels 0 and 1 draw the footprint as a flat blob
  instead, since their chunks are flat wild grass.
- **Per level.** World: blob, name label, and since owner decision gd-H6ZPz5V262 the Project stamps (heads and `!N` per plan)
  under the project boards. The big pulsing ring that marked a biome needing a human went (gd-GgKjBgsc0U, decision
  gd-rIYWnrjguI): the stamps' heads and `!N` and the boards say who is there and what waits, and the Mailbox, the beacons and
  N still find what needs you. Project: blob, thin fence, sign
  plate, plots as plates in the crop colour (`PLANS.tile().crop`), band items as one-tile dots (the design said "counted
  in the sign"; the dots cost nothing and make a quiet biome read as lived in), the rollup's `!N` and heads on a plot (no ring). Plan:
  flat beds with a state-coloured square per task carrying a glyph (rock, fruit, sprout, mulch, seed), 6 px band dots.
  Bead: the Yard's plant sprites per task (`ART.taskBead`), stubble for a phase's closed count while its row has room, a
  crate where a phase overflows its row, pots (frosted or green), wildflowers, one-tile trees, a `?` bubble on a plot
  that needs a human.
- **Labels** at the Bead level are not tooltip-only: the biome name and plan title still show over their signs (the sign
  plate carries no painted text), capped at 40; task titles are hover and focus only, as designed.
- **Hots.** World: one button per biome footprint (flies to the Project level). Project and Plan: a button per sign
  (flies to the Plan level), a link per plot, decision and escalation tasks from the Plan level. Bead: a link per task,
  the plot's top fence row links to the plan, and every pot, tree and flower links to its bead. `+N more plans` links to
  `/plans?project=<key>`. Hot ids are `kind:id`, so a Yard plant and a plot task with the same bead id coexist.
- **Fixed on the way:** the chunk painter drew every chunk at absolute world px onto its 512 px bitmap, so only chunk
  `(0,0)` ever held grass; chunks now translate to their own origin.
- Not built here: the minimap, deep links and `resolve`, the outline, the perf measurements (section 9 thresholds untuned),
  and the "Mine" highlight on biomes.

## 13. Minimap and fast-travel build notes (gd-gFUEJRyVRr)

What the link step shipped, and where it differs from sections 2.1 and 7:

- `web/world.js` gains the pure half of section 7: `parseAt`, `atFor`, `resolve(world, at)` (project, plan, task, seed,
  closed plan, meadow bead; a **phase id** resolves to its bed; a plan past the plot cap resolves to the overflow marker;
  an unknown id falls back to its project with `found:false, missing:<id>`; a bare id without a project segment is tried as
  an id; a known id wins over a stale project segment), `locate(world, x, y, level)` (what is under the camera centre, for the
  URL write-back), the query helpers `parseQuery`, `queryGet`, `linkQuery`, `levelParam` (other query parameters are kept
  as written), the minimap transform `minimap`, `miniFromWorld`, `miniToWorld`, `miniRect`, and the jump index
  `searchIndex` and `search`. `TestWorldLinksUnderNode` pins all of it, including an atFor-then-resolve round trip over
  every indexed thing.
- **Deep links.** `?at=` is read once at load and held until the first layout (`farmdoc`), then framed with `fit()` at
  `z=` (a level 0..3) or the target's natural level (project 1, plan 2, bead 3). A one-tile bead stops at 2x rather than
  Z_MAX so it is not a wall of pixels. A deep link opens on the map, the only
  view there is with script (3b R4 retired the Farm/Ledger toggle). An unknown id toasts and shows the project; an unknown project toasts, leaves the Yard, and keeps
  the address as typed until the camera moves.
- **Write-back.** 400 ms after the camera settles (no drag, pinch, minimap drag or flight) the address becomes
  `?at=<what is under the centre>&z=<level>`: the task cell at Bead, the plot at Plan and in, else the biome, else `yard`;
  over the wild `at` and `z` are dropped. The hash (`#mail`, `#porch`) is preserved. A page the user has not moved never
  rewrites its address.
- **Minimap.** A 160 x 110 canvas (96 x 66 under 640 px) bottom-right of the board: the world bbox in wild green, one
  rectangle per biome footprint in its palette (3 px minimum), a 2 px gold ring where a human is needed, the Yard, and the
  camera frustum (white with a dark halo, clamped to the canvas, 3 px minimum). Redrawn from `camChanged()` and per
  layout, never on an idle data frame. Click flies there at the current zoom; drag scrubs. The canvas is `aria-hidden`;
  the keyboard path is the `/` jump. Removed in 3b R11 (gd-dOMlE4c3SX), with `minimap`, `miniFromWorld`, `miniToWorld`,
  `miniRect` and `CURNAV.miniPick`: a phone never had it, depth 0 is the overview (`CG §7`), and `0` then a click, or `/`,
  reaches another project.
- **`/` jump.** `/` (when the map is the key target) or the `Go` button opens a combobox-over-listbox palette inside the
  board: the empty query lists the Yard and the projects; typing ranks an exact id, then id prefixes, then title prefix,
  word prefix, substring, then several words all present; results are capped at 8. Arrows choose, `Enter` flies at the
  natural level and moves focus to the target's hot link (sign, plot, plotsign or task, whichever the level has in the
  DOM), `Esc` closes and returns focus to the board. Keys inside the input never reach the camera. A query that looks
  like a bead id but is not on the map offers "Open <id>", which goes to the bead page, so "not in the world" is one
  keystroke from the answer.
- **Fixed on the way:** the `.board canvas` rule stretched every canvas to the board; it is now scoped to `#cv`.
- Deviation: the level readout next to the camera buttons was hidden under 480 px, since five buttons fill a phone's width.
  It was removed outright in 3b R2 (gd-l75gmca13p): it named the free camera's zoom band, not the cursor's depth, which
  the cursor's label already says.
- Not built here: `Backspace`/`Esc` to fly one level out (section 5), the outline (gd-s69orAcBCf), and the minimap search
  index being shared with the outline (both read `searchIndex`, so that is a one-line hookup).


## 14. Accessibility build notes (gd-s69orAcBCf)

What the accessibility step shipped against section 8, and where it differs:

- **Outline.** `WORLD.outline(world)` is the pure accessible model: roots are the biomes in world order, each with its plots
  in slot order (each plot with its task cells in bed order), then the overflow marker, then seeds, harvested plans and
  loose beads left to right. Every id in `world.index` appears exactly once, carrying the same `kind:id` its hot link uses
  (a biome's is its sign, since the blob is a World-level hot only) and its natural level (project 1, plan 2, bead 3).
  `TestWorldOutlineUnderNode` pins this. `garden.js` prepends a **Yard** root whose children are the ledger's plants
  (decisions, growing, gardeners), so every hot link on the map has an outline entry.
- **Panel.** `<nav class="outline">` holds a WAI-ARIA tree (`role=tree`, roving `tabindex`, `aria-level`,
  `aria-expanded`, `aria-selected`): one tab stop after the camera buttons; Up/Down move, Right/Left open and close,
  Home/End jump, Enter or Space flies the camera to the entry (`fit` at its natural level) and, once the camera settles,
  hands focus to the hot link (`focusAfter` / `landFocus`; under the cursor, `focusCursor`, and 3b R13 retired the pair). It is clipped off-screen (`clip-path`, never `display:none`)
  until it has focus or the new **Outline** toolbar button pins it open; a hand-off to the map unpins it, Escape closes it
  and returns focus to the button. Branches render their children on first open and drop them on close, so a 10x farm
  costs its project rows until someone drills in. The outline is rebuilt by key on every `farmdoc` (and its Yard branch
  on every ledger change), keeping open branches and the current row.
- **Deviation:** the design said selecting an entry "flies there and moves focus to the matching hot link". When the link
  is not in the DOM at that level (past the 300 cap, or a one-tile sprite on a coarse pointer, below) focus stays in the
  outline and a toast says so; the camera still went there. The panel's close button is mouse-only (`tabindex=-1`), so
  Tab from the camera buttons lands straight on the tree.
- **Reduced motion** is read live (`matchMedia` change), not once at load. The three motions all gate on it: fly-to is a
  jump cut, the Plan level's "needs you" ring does not pulse, the sprite loop does not run. `TestReducedMotionCoversEveryAnimation`
  proves every CSS animation is either inside a `no-preference` block or switched off for the same selector in the
  `reduce` block, and that the three script gates are still wired.
- **Coarse pointers.** `WORLD.wantHot(h, level, z, coarse)` is the per-level mask plus the 44 px rule: a one-tile sprite
  (task, tree, wildflower) under 44 CSS px is not in the DOM; at the Bead level the plot link stands in for its tasks until
  the tile is wide enough (2.75x). Yard plants stay (nothing stands in for them). Every remaining hot link gets a centred
  `::after` hit box of `max(100%, 44px / --z)` world px, so it is 44 px on screen at any zoom; the camera buttons and the
  outline rows grow to 44 px. `TestCoarsePointerTargets` and the node test pin it.
- **Contrast.** `TestPaintedTextContrastAA` checks the painted labels against their 1 px outline colour (that is what
  they sit on, not a palette surface) and the pressed-button ink on `--accent`. The hot focus ring is now two-tone, gold
  (`#ffe680`) outside a dark band (`#2a1a14`): `TestFocusRingContrast` proves the bands contrast with each other and that
  on wild grass in both schemes and on all eight biome grounds at least one band reaches the 3:1 non-text minimum. The
  old single accent-coloured dashed ring was 1.3:1 on light grass.
- Verified in headless Chromium (desktop, reduced motion, Pixel 5 emulation): Tab reaches the tree, Enter on a plan
  lands focus on its plot link at the Plan level, on a task at the Bead level, on a Yard decision in the Yard; under
  reduced motion the hand-off is immediate; on the phone every hot link hits at least 44 px and task links give way to
  the plot below 2.75x. Zero console errors. Not built here: the minimap, deep links, `resolve`, the perf measurements.

## 15. Perf budget and stability tests (gd-K2bb18BKFu)

What the perf step pinned, measured, and tuned. Both tests run under node from Go, like the others, and skip
without node.

**The 10x document** is `internal/gardenhub/testdata/world_tenx.js`: 50 projects (the three section 3.2
names plus 47 generated), 360 plans with 2-7 phases of 2-13 listed tasks (about 12,000 task cells; the server
would cap this at `farmRowCap` rows, so it is a superset of what a browser receives), 222 seeds, the 24 shown
harvested plans, the 40 shown loose beads (some in the Commons). Seeded, so it is byte-identical everywhere.

**`TestWorldTenXLayoutUnderNode`** (`testdata/world_tenx_test.js`, golden `testdata/world_tenx_golden.json`,
regenerate with `-update-world-golden`):

- Golden: every project's domain and a digest of every rectangle (biome used, foot, sign, bands; plots; beds;
  task cells; pots, flowers, trees; the overflow marker). In a crowd of 50 the section 3.2 example cells are
  taken by names with a higher hash priority, so `gardenio` lands at `(5,1)`, not `(-1,1)`: the "needed a 2nd
  attempt" column of section 3.2, now pinned.
- Determinism: a shuffled document, a memo of the same world, and a memo round-tripped through JSON all give
  the golden digest.
- Stability with a memo, each from the base world: state, title, assignee, priority, MR, count and needs
  churn on every task and plan moves nothing and places the same ids; a new project with three plans moves
  nothing and every old project keeps its domain; a plan added to every project with 3-8 or 10-25 plans (the
  used rows and columns do not step) moves nothing; 20 plans closing changes no project's index, the survivors of
  each project keep their order and fill slots `0..n-1` (each steps back one slot per closed plan ahead of it,
  packed slots, 3.3), a project that lost no plan moves as a whole, each closed plan keeps its memo entry, and
  the next new plan takes the next slot after the survivors. Two documented exceptions: closing three of the
  36-plan project's plans lets its overflowed plan take the next slot, so the marker goes; and
  a task closing slides the cells to its right in the same bed one tile (the harvest, section 12), nothing else.
  Band items have no memo (section 3.3), so pruning seeds may move another pot in the same band of the same
  biome; plots and cells never move for it. A readonly (truncated) layout gives the same map and leaves the memo
  as read. The memo holds 359 slots and 51 domains (13 KB) and prunes 5,000 stale entries under the caps.
- Cull bounds on a 1920x1080 view padded 25%: at most one biome's 35 plots at Plan or Bead; the worst visible
  set at Bead is about 1,150 things; the whole world is reachable through `visible()`.
- Data-time budget: cold layout 8-40 ms, from a memo under 12 ms (budgets 400 and 200).

**`TestWorldFrameBudgetUnderNode`** (`testdata/world_frame_test.js`) boots `garden.js` under
`testdata/world_dom_mock.js`: a DOM wide enough for the page, canvas contexts that count draw calls instead of
painting, timers that never fire, and `requestAnimationFrame` as a queue the test flushes one generation at a
time. It feeds the 10x document on the `farmdoc` event and drives the camera the way a person does: `0` fits
the world, the biome link flies to Project, the sign link to Plan, `+` steps to Bead; at each level 40 key pans
of a tile, 20 of a plot, a 40-frame pointer drag, and wheel or key zooms. A frame is one input event plus the
render it queues. What is measured is the page's own JavaScript per frame (culling, hot-link and label
reconciliation, the draw calls issued); raster is not modelled, the draw-call count bounds it.

Budgets, per frame: median 4 ms, p95 12 ms (a quarter of the 16.7 ms frame for script, with room for garbage
collection); at most 8,000 draw calls; 300 hot nodes; the section 6 label maxima; no layout inside a frame
(counted); a pan bakes at most 8 bitmaps, a level change up to the 64-chunk LRU; a fly settles in at most 64
frames with a native chunk bake p90 under 24 ms; a farm document (first or a patch) is handled in under a
second and never moves the camera. Time is the main thread's CPU time, not the wall: a loaded CI runner
preempts and throttles the script, which charged the wall for slices it never ran (gd-WsKtzYxI5c). Sweeps run up to
three times and each time figure is judged at its best attempt; the counts are exact and judged at their worst.

Measured on this pod (node 24, 2 vCPU, mock DOM), medians per frame: World pan 0.9 ms, drag 0.8; Project pan
0.7; Plan pan 1.8-2.4 (4,400 draw calls: 35 flat plots with a glyph per task); Bead pan 2.6-2.8 (1,043 draws,
300 hots, 43 labels), drag 1.3-1.7. Entering Bead over the 35-plot biome is one 12-16 ms frame (the sprite
bitmaps bake). First farm document 160 ms, a patch 60-100 ms.

Tuned on the way, because the first run missed:

- **A native chunk bake is the largest unit of frame work.** One 512 px chunk over a dense biome issues about
  100,000 offscreen `fillRect`s (wild grass, biome ground, fences, plot beds). The first Plan-level view of the
  35-plot biome baked 28 of them in one frame: 200 ms of script before any raster. `drawChunks` now bakes at
  most `BAKE_PER_FRAME` (1) native chunks per frame and draws the rest as flat grass, tinted flat
  where a biome footprint crosses, then asks for another frame; the view sharpens over 28 frames at 6-16 ms
  each instead of stalling one. Wild grass under a biome's ground is skipped (`ART.drawWild` takes a `skip`),
  which cut the bake by about a third. A view wider than the LRU (a 4K screen at the far end of Plan needs
  about 170 native chunks) keeps its uncached chunks coarse rather than thrashing the cache every frame.
  Follow-up gd-v6poFCszc2: bake from cached tile bitmaps (one `drawImage` per tile) for a 10-30x cut.
- **Hot links were refilled on every pan.** With 300 in view at Bead, each pan step rebuilt every label, link
  and tooltip although nothing about them had changed. `syncHots` now refills a node only when its hot object
  or the data generation (`hotsGen`, bumped by a model refresh or a new world) changed. Bead pan median 4.3 to
  2.6 ms.
- **Bug found by the 10x golden**: the Commons cell was marked taken with `""`, which reads as free, so at 50
  projects the hash probe placed a project on top of the Commons. Fixed in `layout()`; the N=3 example never
  hit it.

Not built here: the raster side of the budget (a Playwright run in a real browser, when the beads-viewer e2e
suite covers `/garden`), and the tile-bitmap bake above.

## 16. Compact world build notes (gd-pXUGI64tyk, phase 1b)

Owner feedback on the live phase 1 world: "we've put the farms incredibly far apart". With four
projects the hash-and-probe cells (section 3.2) spread 60-tile biomes over a field five domains
wide, and the page opened on the Yard, so the farm was mostly grass. This step packs the regions
and opens on the whole farm. `world.js` only; `garden.js` changes four camera lines.

**Regions, not cells.** A project's **region** is the rectangle it reserves: its foot (the used
rect padded 5, section 3.3) grown to at least the shape of a 3 x 3 plot grid (`RESERVE`), so a
project with one plan has room for nine before anything has to give. Regions keep `GAP = 16`
tiles (one plot) clear of each other and of the Yard scene (`YARD_RECT`, tiles `-12..11 x
-8..6`). The biome's own rect (`b.x, b.y, b.w, b.h`) is its region; `b.centre` replaces
`b.domain`. The region is pure in `(rows, cols, overflow)` through `shape()`, so it is sized
before it is placed.

**Placement** (`layout()` step 2) is a nearest-free search: candidate centres on an 8-tile grid
(`GRID`), square rings outward from the origin, and the smallest `x^2 + (1.6 y)^2` wins
(`ASPECT`: screens are wider than tall, so the farm packs wider than tall and zoom-to-fit gets a
larger zoom). Ties go to the smaller `|y|`, then east, then south. A ring whose radius exceeds
the best distance cannot improve it, so the search stops; the 51-biome 10x world lays out cold
in about 30 ms (budget 400). Cold order: the Commons first (loose beads by the Yard), then
projects oldest first (the oldest `created` of their plans and seeds), then `h32(name, 1)` and
the name as tie-breaks. A new project is the youngest, so the cold answer never moves an old one
either: section 10's open question 1, closed without the server field.

**Stability** keeps section 3.4's promise with the memo at v2:
`domains[key] = {x, y, r: {x, y, w, h}, t}`, the centre, the region reserved and when it was
last seen. Each layout honours memo'd projects before newcomers: first those whose current
region still fits inside their reserved one (last-seen, then key order), then those that grew.
A grown project keeps its centre when the grown region is still clear of everything placed; if
it is not, **only that project hops**, to the nearest free centre around its old one. That is
the one move the design allows, it happens on the growth step that broke the gap (the 10th plan
widens a biome from 3 to 5 columns, the 26th to 7, an overflow marker adds a row), and nothing
else moves for it. A project that shrinks keeps its reserved rect for the memo's 30 days, so
closing and re-adding plans moves nothing. `opts.tidy` ignores the memo'd centres (slots are
kept) and re-packs from the cold answer: the explicit "tidy" of the task, pure and ready for a
button, with no UI in this step. A v1 memo keeps its slots and drops its cells, so the one-time
re-pack keeps every plot where it was inside its biome.

**Camera.** `defaultCam()` is the world fit (`worldCam`, pad 5%) once a document has laid the
farm out and the Yard until then; `resize()` and the first farm document apply it while `auto`
(nothing has moved the camera yet) unless a `?at=` link is pending, which wins. `0` sets `auto`
again so a resize keeps the fit; `Home` no longer does, the Yard being no longer the default
framing. The bbox is the Yard scene with a GAP of grass around it plus every foot (the empty
world keeps the Yard domain). With the live four projects the fit at 1400 x 800 lands at
`z = 0.26`, the Project level, every sign readable.

**Measured** (the node tests): four projects, 22 + 2 + 1 plans and a Commons, pack into
304 x 171 tiles with 17-18 tile gaps, against about five domains (880 x 300 tiles) before; the
10x world's 51 regions pack into 1014 x 653 tiles (the probe window reached 12 x 11 domains,
2112 x 1584). A packed world changes one section 6 budget: a Plan-level 1920 x 1080 view over
the 35-plot biome now also meets its neighbours, so up to about two biomes' plots (75 measured,
80 pinned) show at Plan; at Bead the view is still inside one biome (35). Frame budgets are
unchanged; the per-fly settle numbers under node match main (the Plan sign fly 6-7 ms, the Bead
step 16-17 ms), with the same CPU-steal outliers on a shared pod.

**Tests.** `TestWorldLayoutUnderNode` pins the new golden (a lone project at `(64, 0)`, the
three-project example at `(64,0)`, `(-64,0)`, `(144,0)`, one plot apart, in under two domains),
the Commons first, a memo'd centre honoured, a centre on the Yard refused, the v1 migration,
growth in place to nine plans, the short hop at the 10th with the neighbour unmoved, and tidy
equal to cold. `world_tenx_test.js` records centres in the golden, checks the gap between every
pair of regions and the Yard, and keeps every stability section (3a-3h) as written. Golden
regenerated with `-update-world-golden`.

## 17. Cursor-aimed text and the info card build notes (gd-D3G9dMA1TU, phase 1b)

> **Since gd-D1QRStvmCh (2026-10-20) the mouse opens no card.** The owner found the hover cards too much ("way too much
> mouseover action, very large popups flashing"), and the accepted cursor-world spec
> (`superpowers/specs/2026-10-20-lands-cursor-grid-design.md`, N-D1 "hover sets nothing", N-D14 the card retires in 2b.5)
> points the same way. Until 2b.5 lands, `cardAnchor()` answers only the pinned hot (a tap) and the hot with keyboard
> focus; the hover and the "plot nearest the pointer" anchors below are history. A hover still tints the link's outline
> and a click opens the thing; the aim (`nearId`) still picks which label reads in full. The notes below describe the
> step as built.

Owner feedback after the phase-1 check-in: text should answer the mouse, cursor or tap, and plan titles at the
Project level were lying across neighbouring plots (on a phone they covered most of the map). What this step
changed in `garden.js` and `garden.css`, against sections 6 and 8:

- **The cursor's plot.** `aim()` picks one plot: the one holding keyboard focus (a plot, plot sign or task link in
  `#hots`), else the plot nearest the mouse or pen (`nearPlot`: the visible plot whose screen rectangle is closest,
  within `NEAR_PX` = 160 px of its edge; none at the World level). Touch never sets the pointer, so a phone aims by
  tap and focus only. The aim is recomputed on every pointer move and once per camera change, never in flight.
- **Label text follows the aim (section 6, rule 3 refined).** The per-level maxima and the drop-never-shrink
  collision rule stand. A plan label now says its full title (40 chars + the need count) only when it fits over
  its own plot plus a 24 px margin, or the plot is the cursor's (`.lbl.near`, placed first, priority -1). Elsewhere
  it is a short sign: the need count when a human is wanted (state is never colour alone), else the title cut to
  the plot's width, or nothing under six characters. At Project zoom (0.3x) a plot is 86 px wide, so the signs are
  about 13 characters; at Bead zoom titles fit whole. The cut is sliced by precomputed code-point offsets and
  memoised per zoom, so pans and settles reuse it; the text is cut only for candidates the placement loop reaches.
  Biome, phase and overflow labels are unchanged.
- **The info card** replaces the `#tip` tooltip. Whose card, in order: the hot a tap pinned, the hot under the
  mouse, the hot with keyboard focus, else the cursor's plot (a plot past the 300-node cap still gets a card from
  its layout, without a link). A plot's card is the plan title, `done of total steps closed (pct)` with growing
  and blocked counts, `N decisions are waiting`, `Gardener(s): ` (assignees of its growing steps) and
  `id · project · last activity N ago`; every other hot keeps its one-line text. It is built when shown, not when
  a level change fills 300 links in one frame. `aria-label` on the links is the one-line text, as before.
- **Never over the thing it describes.** `placeCard` measures the card at the origin (an absolutely positioned box
  shrinks to the room right of its `left`), then tries above, below, right and left of the anchor's screen
  rectangle, clamped 4 px inside the board; the first that does not intersect the anchor wins. When no side holds
  the whole card (a 232 px phone board before the layout step), it shrinks, scrollable, into the roomiest side
  that then holds it (`CARD_MIN` 56 px); only when not even a strip is free does it take the corner farthest from
  the pointer. Hidden while the camera flies, drags or pinches; re-shown once it settles.
  The map's own controls (camera bar, drawers, minimap, Commons rail, Projects button, tab bar) count too: a side
  must clear them, or the card slides past the one it lands on. On a touch screen, or for a pinned card, a card that
  fits no side whole shrinks into the room between its anchor and the nearest control in its column before any side
  may cover a control (gd-qy7WF5RPzV: a tall card for a plot mid-phone used to land on Commons and Projects with its
  last lines hidden). Its column may slide left or right past a control beside the anchor (the Commons rail), and a
  spot that covers no other plot wins over a roomier one that does: a touch near a card's link goes to the link, so
  a plot under a shrunk card could not be tapped. Such a card is `.tip.fit`: it takes the finger so it scrolls, and a
  tap on it off its links is handed to what is under it.
- **Touch (section 8).** The first tap on a link hot (plot, task, tree, flower, seed, overflow) pins the card and
  does not follow the link; the card then carries a 44 px **Open** link under the title (so a shrunk card still
  shows it) and that link is the only part of the card that takes the pointer (a shrunk `.tip.fit` card takes it all, to scroll, and hands taps on): taps on the card body fall through
  to the map, so a tap on a neighbouring plot moves the card there. The second tap on the pinned hot follows its
  link. A tap elsewhere on the board or Escape lets the card go. Buttons (biome, sign, mailbox, barn) act on the
  first tap as before. A tap's synthetic `mouseenter` is ignored (`ptrType`), so no hover card sticks on a phone.
- **Keyboard** focus shows the same card and aims the same full label (`focus`/`blur` re-aim); Escape hides the
  card, blurs the link and drops a pin. Reduced motion: the card's 120 ms fade is inside the `no-preference` block
  and switched off in the `reduce` block (`TestReducedMotionCoversEveryAnimation` pins both).
- **Verified** by `TestCardUnderChromium` (`card_browser_test.go`, script `testdata/world_card_browser.js`):
  Chromium under `playwright-core` against `/garden` on an httptest server fed the fixture farm plus three
  long-titled plans. Desktop 1280x800 with reduced motion: hover shows the card beside the plot with title,
  progress, decisions, gardener and activity, inside the board and not over the plot; exactly one full plan label
  (the hovered one), the others end in an ellipsis; the pointer on grass 30 px below the lowest plot still shows
  that plot's card; off the board the card goes; focus brings it back and aims the label; Escape hides it; the
  card's `animation-name` is `none`. Pixel 5: a tap pins the card with a 44 px Open link and the URL does not
  change, a tap on another plot moves it, the second tap navigates. Zero console errors on both. It skips without
  `gardenio/tests/gardenhub-browser/node_modules`; `make test-gardenhub-browser` installs playwright-core and
  Chromium and runs it (this pod had Chromium preinstalled under `PLAYWRIGHT_BROWSERS_PATH`). Not in CI yet: the
  runners have no browser.
- **Frame budget.** The level-change frame into Project (section 15, `world_zoom`) pays about 0.5 ms more for
  the per-label fit test and the extra short signs it places (24 labels where 19 full titles fitted). Measured
  on this 2 vCPU pod, `TestWorldFrameBudgetUnderNode` fails on bake-dominated settle frames 3 of 6 runs on this
  branch and 4 of 6 on `main`; the budget is tuned for the CI runner, not a shared agent pod.

## 18. Text inside the window build notes (gd-YJI2vPZwp4, phase 1b)

Owner feedback at the phase 1 check-in: "have more of the text appear within its window rather
than all over the place outside". Once the map owned the viewport (gd-rdMNXor7Ip) the drawers
already held the Plans panel (plan counts, filter chips, archive piles, seeds, harvested), the
legend and keys, and the Porch/Mailbox dock. What still read as page chrome was the header: a
full-width sky bar with a border across the top of the map, two rows tall on a phone, and the
phone tab bar under it. CSS and markup only, plus the counts line in `garden.js`.

**The header is a HUD on the farm.** `body.view-farm .crumb` is transparent with no border and
`pointer-events:none`, so a drag that starts between its pieces pans the map; its children take
pointer events back. The moon, the title, the counts and the live state are one **title plaque**
(`.plaque`, the drawer-button look) top-left; the scope and sky buttons and the Beads chip float
top-right as they were. `--bar` is still the header's measured height (48 px desktop, 104 px on a
390 px phone, was 45 and 91), so the camera, drawers, jump and outline sit under it unchanged.
The ledger keeps the plain bar: it is the page without script, a list, not the world. With script there
is no route to it (3b R4).

**Counts.** The line stays on the plaque at every width (a phone used to hide it). On a phone the
`.lw` words drop ("1 to harvest · 2 growing · 2 gardeners"); the Mine/Everyone buttons already say
whose. The Beads chip joins the control row on a phone so the plaque gets the width. Every number
from before is still on screen or one drawer away: the plaque counts, the Yard signs (Plan level
and in), the dock and tab-bar pips, and the Plans drawer.

**Phone tab bar** is transparent on the farm, its buttons floating over the map like the rest of
the HUD. That exposed an old
bug: the outline's phone `bottom` lost to the later `.outline` rule, so the panel ran behind the
tab bar; it now stops above it.

**Outline rows** no longer break mid-word ("0 ste ps" on a phone): the title and count use
`overflow-wrap:break-word` (a word breaks only when it cannot fit a line by itself) and the row
wraps, so a long count line drops under the title as a whole.

**Measured** in Chrome against the fixture farm: at 1707 x 842, 1280 x 720 and 390 x 844 the
document scrollHeight equals innerHeight, the header box paints no background, and no text sits
outside the map. Before and after screenshots are in `docs/screenshots/homestead-1b-text-inside`.
`TestTextLivesInsideTheWindow` pins the HUD contract.

## 19. In-garden bead view build notes (gd-BdhucNqv0i, phase 1c)

Owner feedback: "all links are internal to the garden/farm viewer and nothing goes to
beads-viewer". Every bead the garden showed linked the beads-viewer bead page (109 anchors on
Farm/Mine). Now every one opens in the garden.

**Address.** A bead's link is `/garden?bead=<id>`, built in one place per source: `render.go
beadHref` (Ledger, Porch and Mailbox titles, and through them the Yard plants), `garden.js
beadHref` (plots, plot signs, tasks, harvested trees, meadow flowers, seeds) and `plans.js link`
(seeds, harvested plans, the meadow); a plan card opens its plan's bead view. The camera's `at=`/`z=` and
`bead=` sit side by side in the query; each writer keeps the other's parameters as written.

**The panel** (`web/bead.js`, `#bead`) is a sibling of the board inside the map window, so pan,
zoom and the map keys never see it: a column on the right on a desktop (under the camera buttons,
above the minimap), a sheet above the tab bar on a phone, over the dock drawers. A plain click on
any `/garden?bead=` link is caught and opens it in place; a modified click opens a new tab at the
same address, which opens the same panel. Enter on a focused hot is a click. On touch the first
tap still only pins the info card (section 17) and the second, or the card's Open link, opens
the panel. It paints at once from what the page knows (`window.GARDEN.known`: the Ledger row, the
world index) and fills in from `GET /garden/bead/{id}` (see `homestead-farm-read-model.md`):
description, notes, parents, children, waits on, holds up, related, comments (newest first),
fields and labels. Text goes in through `textContent` only.

**Actions** are the ones the farm already has: Show on the map (flies the camera, as the jump
does), the pull request (external, new tab), and for a decision or escalation in the Mailbox a
copy of its answer row, which `garden.js act()` drives unchanged. Everything else is read-only.
A plan has this one detail view: its phases are listed, each linking to its own bead (the plan
modal retired, 3b R12).

**History.** Opening pushes a history entry, so Back closes the panel or returns to the bead a
cross-reference was opened from; Escape and the close button drop `bead=` without adding one.
A deep link with no `at=` flies to the bead once the farm document lands. A bead not on the map
(closed, past a cap, or typed into the jump box) still opens: the panel reads it from the server.

**What else moved.** The overflow crate opens the Plans drawer instead of the viewer's Plans
page; the jump box's "not on the map" entry opens the panel instead of navigating.
`TestGardenLinksStayInside` lost every bead-page key; with the Beads chip and the agent-chat link
gone (gd-AXfec5afZo) the inventory is empty, so it is now a plain pass that fails on any new leak. `TestCardUnderChromium` walks the panel in a real
browser: click, Escape, reload of `?bead=`, a child link, Back, an unknown id, a phone tap, and
fails on any request that leaves `/garden`.

## 20. Opening view build notes (gd-N0AEWzgVsg, phase 1b)

Found on LIVE 2026.292.4 at 1707 x 842: the page opened at the zoom-to-fit (Project 0.32x) with no
text at all, project signs blank and plots bare coloured squares; the header said "1 gardeners";
a hover card for a top-right plot sat under the camera bar. What this step changed in `garden.js`:

- **The labels were there, then detached.** `buildLabels()` (the Yard's labels, rebuilt on every
  ledger refresh) cleared `#lbls` with `textContent=''`. The world labels share that layer and
  `placeWorldLabels` keeps its nodes in `shownLbl`, so after the first refresh every sign stayed
  out of the DOM until it was dropped and re-added. That is why only the aimed plot or its
  neighbour ever showed text. `buildLabels` now removes only its own nodes.
- **Every plot wears a sign at the fit (section 6, rule 3 refined again).** Placement runs in
  rounds, dropping (never shrinking) anything that would overlap: the project signs first (falling
  back to the name and a `!N` need count when the full sign meets a neighbour); then the cursor's
  plot in full; then every other plot's short sign, a few characters that fit inside its own
  width: `!N` when a human is wanted, else `done/total` steps, else the title's first three
  characters (`shortSign`); then, in priority order, each plot's longer text (full title, need
  words, or the title cut to the plot) replaces its short sign where it fits; then overflow
  crates and phases. Short signs also show at the World level where a plot is 20 px on screen.
  A sign whose anchor is on screen is kept inside the board horizontally. The Project and Plan
  maxima are 64 (were 40), so a 35-plot biome in view keeps a sign per plot.
- **The opening fit stops at the Project level.** `openCam()` is the fit with `min` at
  `levelRange(1)[0]` (0.13x), the smallest zoom where a plot is wide enough for a short sign.
  A farm wider than that opens on its centre: a 390 px phone shows the two or three projects
  nearest the Yard, named and signed, with the rest on the minimap. `0` and `All` still fit the
  whole farm. A portrait-aware layout, rather than panning, would be a layout change (the memo
  keeps regions where they are) and is out of scope here.
- **The card stays off the controls.** `placeCard` clamps its top under the header and treats
  the camera group, the drawers and the minimap as obstacles (3b R9 took the drawer buttons off the map): each side is
  tried clear of the anchor and of them, then slid below or left of a control it lands on, and
  only then allowed over a control, before the shrink and corner fallbacks of section 17.
- **Counts** say `1 gardener` (`plural`).
- **Verified** by `TestCardUnderChromium`, which now adds three more five-plan projects to the
  fixture. At 1707 x 842 (desktop) and 390 x 844 (phone) it opens `/garden` with no address, runs a
  ledger refresh, and asserts the fit is not the World level, that every project whose sign is on
  screen names its project (all four on the desktop), and that every plot on screen has a visible
  sign of at least two characters. On the desktop it then drags the top-right plot up to the camera
  bar, hovers it, and asserts the card meets none of the header, camera group, drawers or minimap.
  Each of the two fixes was reverted on its own to confirm the test fails without it.
  `GARDENHUB_SHOTS=<dir>` saves the screenshots in `docs/screenshots/homestead-1b-opening`.

## 21. Weeds and fog build notes (gd-ytTNc3Fu9M, phase 2)

Maintenance lint drawn on the world. The rules are pure, in `web/weeds.js`, and `garden.js` keeps five one-line hooks:
index per document (and once a minute, since wilt and dormancy come with age), draw at the end of the world layer, card
lines, the hot links' accessible names, and two legend rows.

- **Rules** (thresholds are named constants): `unlabelled` (a plan, seed or loose bead with no `project:` label; a step
  only when its plan is labelled, so an unlabelled plan is one weed, not one per step), `phaseless` (a plan being built
  with no phases), `frosted` (a draft with no update for `FROST_S`, 7 days), `orphan` (an open PR whose bead is closed or
  missing, from the farm's new `piles.orphan_mrs`), `wilt` (an in-progress step or loose bead with no update for `WILT_S`,
  12 hours, the scene's existing threshold). A project with no activity for `DORMANT_S` (14 days) across its plans,
  seeds, loose beads and harvested plans is dormant.
- **Each weed says why** in one sentence, prefixed by its rule's name, on the card of what it sits on: a plot lists its
  plan's weeds and its steps' (four, two on a phone so the card fits, then "+N more"), a step, pot or flower its own, a
  region its dormancy, a tally and its orphaned PRs. A step's or orphan's line links into the in-garden bead view (the
  closed bead, for an orphan); nothing links out of `/garden`. The same facts go into the accessible names (`Weeds: 1
  wilted bead, 1 plan without phases.`, `Dormant for 20 days.`): state is never colour alone.
- **Glyph.** A thistle (purple head, spiky stem), not the grass tuft the blocked state already uses for "weeds and a
  rock". Per level: World, fog only; Project, a thistle on the corner of each plot that holds a weed at a constant 20
  screen px, and on weedy pots and flowers; Plan, the same corner plus a thistle on each weedy cell (the corner matters
  because a phase label covers the first cells of its row); Bead, the corner plus smaller thistles off the plant's middle,
  except on a step that is only wilted, whose wilted sprite already says so.
- **Fog** is a pale wash plus a few drifting banks placed by the region's seed, drawn over the dormant footprint at the
  World and Project levels only. It never animates.
- **Ranking.** `WEEDS.rank` orders the weeds by neglect for the chore tour (gd-S2Cn2dTsP0): a rule weight (orphan 3,
  wilt 2, others 1) times `1 + log2(1 + days)`, so age counts without drowning the rule.
- **Cost** on the 10x farm: lint and index about 27 ms once per document; drawing at most 0.2 ms and about 200 draw
  calls per frame (the Plan level over the densest biome).
- **Tests.** `TestWeedsLintUnderNode` (rules, texts, index, card lines, names, draw counts per level),
  `TestWeedsUnderChromium` (thistle pixels on the canvas, fog lighter than a live region, cards on desktop and phone,
  the orphan's in-garden link, no request leaves `/garden`), `TestFarmOrphanedPRs` (the server read, its lookup budget
  and cache).

## 22. Beacons build notes (gd-gtr7qBCn9z, phase 2)

What needs you and is off-screen gets an arrow on the map's edge (`web/beacons.js`, node-tested in `beacons_test.go`,
walked in Chromium by `testdata/beacons_browser.js`). garden.js only calls `syncBeacons()` from `camChanged`, `onFarmDoc`
and `refreshFarm`.

- **Targets.** One per plot whose plan waits on a human (`needs`, the same count as the ring), at the plot. One for the
  Yard's Mailbox, counting only the decisions and escalations that no plan's `waiting` list carries, so one ask is never
  pointed at twice. No backend change.
- **On screen** means the target shows at least 24 px on each axis below the header (or all of itself, if smaller).
- **Placement.** The arrow sits where the ray from the middle of the map to the target leaves an inset edge (half an arrow
  plus 14 px, room for the point), then slides along the edge off the camera group, the drawers, the minimap, the phone
  Projects button and tab bar (a full-width bottom bar lifts the bottom edge instead). With every edge covered (an open
  phone drawer) no arrow is drawn.
- **Merging.** Arrows that would touch become one, with the summed count and `!` when any target is an escalation (`?`
  otherwise). It sits at, points at and flies to its nearest target; the label names up to three ("3 need you to the
  north-east: A, B and C. Fly to A."). Arrows are in clockwise order from the top-left, which is the Tab order.
- **Flying.** The shared `flyTo`, so it is a jump cut under reduced motion. A plan lands at the Plan level (or stays at
  Bead), the Yard at the home camera. From the keyboard the target's link then takes focus (`focusAfter`).
- **Size.** 36 px, 44 px for a coarse pointer. Gold on dark like the ring; the glyph, the point and the count carry the
  meaning, so it is never colour alone.
- **Cost.** The controls' rects are measured at most once a second while the camera rests and at once on a resize or a
  drawer toggle. During a flight or a drag the last measurement is reused, so a moving frame forces no layout.

## 23. Neighbours build notes (gd-MFcv2hSfue, phase 2)

Whose plan a plot is, at a glance and in words. The logic is `web/neighbours.js` (pure, node-tested
by `TestNeighboursLogicUnderNode`); `garden.js` keeps three small hooks: the styles and legend per
farm document (`onFarmDoc`), the fence and the dimming per plot (`drawWorldLayer`), and the
operator line on the plot card and its link label.

- **Operator.** A plan's lowercased `operated_by` alias (`FarmPlan.operator`), else its `owner`. No
  backend change: both were already on `/garden/farm`. A plan naming neither keeps a plain fence.
- **Fence.** Each operator gets one of 8 colours and one of 4 patterns (solid, dashed, dotted,
  double), hashed from the alias and probed past those already taken on this farm, so up to 8
  operators get distinct colours and up to 4 distinct patterns, and the same set of operators
  always gets the same styles. The palette avoids the gold "needs you" ring and the crop colours.
  The band is painted on the plot's edge in the dynamic layer, over a dark edge, at the Project,
  Plan and Bead levels; it is at least 4 screen px (a double line still reads as two at the opening
  zoom) and a quarter tile. The chunks are untouched, so an operator change costs no re-bake.
- **Not colour alone.** The legend drawer gains "Whose fence": a swatch per operator with
  "matt (you), teal solid fence", the viewer's own first. The plot card and the plot link's
  accessible name say "Operator: flo, brick double fence".
- **Mine.** The header's existing Mine / Everyone toggle (`localStorage garden.scope`, Mine by
  default for a signed-in viewer) now also lights the world: with Mine on, every plot that is not
  the viewer's is shaded, and at the World level so is every region holding none of the viewer's
  plots or seeds. Nothing is hidden and nothing moves (section 10.3). The "needs you" ring and
  bubble draw after the shade, so a waiting decision on someone else's plot still shows.
- **Tests.** `TestNeighboursUnderChromium` walks the card fixture farm as `oidc:matt@example.com`
  with four operators and one owner-only plan, at 1440x900 and 390x844, light and dark: Mine dims
  flo's plot and leaves matt's lit (canvas luminance), Everyone lifts it and survives a reload, each
  fence carries its operator's colour (edge pixels), and the legend, the card and the link label
  say the operator. `GARDENHUB_SHOTS=<dir>` saves its screenshots. The goldens moved because every
  fixture plot names an operator.

## 24. Chore tour build notes (gd-S2Cn2dTsP0, phase 2)

**Tend** (the last camera button) walks the five most neglected things on the farm, one stop at a time. The ranking, the
actions and the framing are pure, in `web/tour.js` (node-tested in `tour_test.go`, walked in Chromium by
`testdata/tour_browser.js`). garden.js mounts it once, and the info card and the beacons keep off its card.

- **Stops.** The weeds (`WEEDS.rank` order) and the beacons' targets (a plan waiting on a human, the Yard's Mailbox for asks no
  plan carries) form one list, heaviest first, cut at five. An ask weighs 3 (like an orphaned PR, the heaviest weed) times
  `1 + log2(1 + days)` since its oldest ask, so age decides between them. A bead with several weeds is one stop naming each.
  A step past the plot cap stops at its plot, an orphaned PR at its closed bead's project. An unlinked PR (no bead to act
  on) and anything off the map are left out.
- **Actions**, suggested first: an ask is **Answered** (its oldest ask in the bead view, or the Mailbox). A weed is
  **Pinged** (a comment addressed to its gardener or owner, with every reason the tour stopped there; never your own),
  **Adopted** (claimed: assignee you, in progress), **Deferred** (status `deferred`, the viewer's new
  `POST /b/{id}/defer`, like `kd defer` with no date) or **Closed**. A closed bead (an orphaned PR's) only takes a ping. The
  farm's `orphan_mrs` rows gained an additive `assignee` for that ping. Close and Defer post only on a second tap, like the
  Mailbox's answers. All of them are the viewer's own endpoints, so permission and audit are the bead page's. A refusal
  is said on the card, which stays on the stop. A success moves to the next stop and says what was done.
- **Framing.** The card sits at the bottom centre on a desktop. On a phone it is a sheet under the header, left of the
  camera strip. Each stop is fitted at its natural level into the largest
  strip of map the card leaves free (`TOUR.free`), and the camera moves so the stop sits in that strip's middle
  (`TOUR.frame`). A dashed ring in world px inside `#hots` marks it, with lines divided by `--z` so they keep their screen
  width. Flights use the shared `flyTo`, so reduced motion gets a jump cut.
- **Focus.** Each stop focuses its title. Escape, the close button or Done ends the tour, clears the ring and returns focus
  to Tend. The card sits beside the board, not in it, so a drag or a map key never starts from it.
- **Owners** are a step's or loose bead's assignee ("Gardener") and a plan's owner or operator or a seed's operator
  ("Owner", without an `oidc:` scheme).

## 25. Cross-project edges build notes (gd-CNhEh30Ykf, phase 3)

A step that waits on a bead in another project is drawn as an **irrigation channel** between the two regions: water would
run from the blocker into the step, so a wooden sluice gate sits at the end that waits, shut until the blocker closes. The
logic is `web/edges.js` (pure, node-tested by `TestEdgesLogicUnderNode`); `garden.js` keeps same-line hooks only (its line
count is unchanged): the index per farm document (`onFarmDoc`), the draw at the top of `drawWorldLayer`, the card lines
(`cardFor`, `plotCard`), the hot links' names (`fillWorldHot`) and a legend row.

- **Data.** `blocked_by` was already on `/garden/farm` (open blockers only, at most 8). One additive field,
  `blocked_by_project`, names the project of each blocker whose label differs from the step's, so an edge to a blocker the
  document does not carry (past a cap, inside a seed) still has a region. No new route.
- **Which edges.** A region is where a thing is drawn: a step's is its plan's, whatever its own label says. An edge is
  drawn when its two ends lie in different regions and both regions are on the map; one into a project with no region is
  still said in words.
- **Ends per level.** World: the middle of each region. Project, Plan and Bead: the step's plot and the blocker's plot (or its
  flower or pot), else the blocker's region. Edges with the same two ends merge into one channel; at most 48 channels are
  drawn per level, most edges first.
- **Route.** Square and through grass, never across a plot: out of the plot into the 2-tile gutter between plot rows (or
  columns, for regions stacked one above the other) on the side facing the other end, along it out of the region, a turn in
  the middle of the gap between the two regions, and in the same way into the plot that waits. Channels sharing a stretch of
  one line take lanes side by side.
- **Drawing.** In the dynamic layer, before the signs and plots, never baked into chunks: a dark bank, water and light
  ripples, 8 screen px wide (below the World level at most 1.6 tiles, so it fits the gutter), culled to the view. The night
  palette is brighter, because the night ground is dark teal.
- **Not colour alone.** The step's card and link say `Blocked by gd-x in kbeads`, the plot's add whose step waits on it, the
  blocker's side says `Blocks gd-y in gardenio`, and a region's link sums its channels (`Irrigation channels: 1 step here
  waits on beads in kbeads.`). The bead view's relation rows gained the other bead's project and say `in kbeads` when it
  differs. The legend has a channel row.
- **Tests.** `TestFarmBlockedByProject` (the field), `TestEdgesLogicUnderNode` (which edges, routes, merging, the cap, lanes,
  no run across a plot, words, draw counts and culling), `TestEdgesUnderChromium` (one edge on the card fixture farm: channel
  pixels at the Project and World levels where `edges.js` routes it and none beside it, at 1440x900 and 390x844, light and
  dark; the link names, the card, the bead view and the legend). The goldens do not move: the fixture farm has no
  cross-project edge.
- **Cursor world.** The hooks touch no camera, cursor or URL code. When 2b.3 (gd-tdB8vCIltU) lands, the draw keeps reading
  `cam.z` and the view rect.

## 26. Cursor world build notes (gd-7bfFegkYjS, phase 2b)

The free camera and the pointer-aimed text are gone. An LSDJ-style cursor walks a grid of grids
(`world > project > plan > phase > task`), the camera only follows it, and text changes only when the cursor moves. The
spec is [`superpowers/specs/2026-10-20-lands-cursor-grid-design.md`](superpowers/specs/2026-10-20-lands-cursor-grid-design.md)
(revision 2.1, decision gd-n0ZRagfdLA, cited `CG §n`). Its section 9 maps each section of this page to what replaced it.
Sections 2, 3.1, 3.2, 3.3 (plot size), 5, 6 (labels and hots rows), 7 (`z=`), 8, 11, 13, 14 (hots), 17 and 20 now
describe the old page; this section and the spec describe what is live. What shipped, step by step:

| Step | PR | What it did |
|---|---|---|
| 2b.1 gd-zjATSCW3s1 | #689 | Square plots (`SLOT` 32/34), 3x3 beds of 9x9 cells filled block-major, the 5-wide world lattice, memo v3, stakes, `overgrown` |
| 2b.2 gd-WConFPNPie | #687 | `web/cursor.js`, pure: `tree`, `pathFor`/`atFor`, `move`, `jump`, `descend`, `ascend`, `follow`, `frame`, `signs` |
| 2b.3 gd-tdB8vCIltU | #708 (+#763) | The cursor drives the keys, the camera, `#hots` (`role=grid`, roving tabindex), `at=` and `garden.cursor.v1` |
| 2b.4 gd-0aFW0WhV5d | #769 | Click or tap moves the cursor and a second one goes in; wheel and pinch are the depth gesture; a swipe is one step |
| 2b.5 gd-09kroduL5H | #770 (+#743) | Signs from `CURSOR.signs`; `aim`, the label rounds, the info card and `cardplace.js` deleted |
| 2b.5b gd-dmYYfaQYuA | #825 | The plot rollup stamp at the project depth (`CG §6.6`) |
| 2b.6 gd-1OIxIiJQkg | #824 | The bead view carries the old card's content |
| 2b.7 gd-POdVKGNXfV | #816 | The phone's one-row pad; the Projects button and the phone minimap removed |
| 2b.8 gd-SBgEzsM2XR | #768 | The chore tour is a cursor walk |
| fix gd-UAKclcq5hu | #826 | The full label keeps off the grid's other cells |
| bin gd-LohQMNM3wU | #921 | The bin beside a plan's plot, its crate mark and the phone's row (`CG §5.6`, step 10; retired, 26.8) |

Merged alongside it, under the same cursor: walking gardeners and ride-along (#771, gd-gMMtrgIT1W; the ride-along retired in 3b R13), adopt by dragging a
Meadow flower (#814, gd-4lkPfWppp8, polished by #831; retired by 3b R14), and the phone camera column's inset (#830, gd-pyrowrsfDF).

### 26.1 Geometry and the cursor model

- **Layout (#689).** As `CG §5`, with three judgement calls. An empty lattice cell keeps its index but takes **no width**, so
  the Commons cell (k = 1) is a zero-width column until something sits in it, and the projects east of it then shift by one
  column (N-D9 allows it; N-D0 says space first). In a phase bed, the `+N` crate comes **straight after the listed tasks**
  and the stubble after it (`CG §5.4` lists stubble first), so the cursor walks tasks then crate. A memo index stays taken
  while it is remembered, even while its project is absent, and 30 days unseen frees it. Gardener spots stand on the
  32-tile plot's bottom fence. `drawBiomeGround` skips the tiles under plots, so a chunk bake paints no grass that a plot
  then covers.
- **`cursor.js` (#687).** It matches `CG §3.7`, plus an `opts` argument to `tree` (#771's `opts.also` used it until 3b R13). It
  falls back to the old layout fields when a 2b.1 field is missing. A band cell (Meadow, Orchard, Graveyard, Nursery in a
  project; the pen in the Yard, 26.10 and 26.11) spans the grid's width and is reached only by Up and Down, and on a phone
  also from the camera column's Go (26.12). Status lines (`CG §6.4`) are written into the tree, so `signs()` only places them.
  `TestCursorUnderNode` round-trips about 14,600 nodes of the 10x world, and `TestCursorSignsTakeNoPointer` is the source
  check that `signs()` takes no pointer argument (N-D1).
- **Keys and navigation (#708).** A separate pure layer, `web/curnav.js` (`TestCurNavUnderNode`), turns keys, pad presses
  and picks into cursor moves, so `garden.js` keeps only the wiring. It is now 1,277 lines, under the 1,500-line quench cap.
  Gardener links moved out of the grid into `#ghots`, and the polite live region is `#cursay`. A project's grid is named
  "N plans" rather than "N items". A ledger patch re-grafts only the Yard's subtree. Back does not ascend (`CG §12.1`'s
  default). The minimap drew the lattice with a gold ring on the projects that need you, until 3b R11 removed it. The camera strip's `+ −` became
  **In** and **Out**; wherever the pad shows, its A and B are In and Out and the strip drops them (3b R1, `CG §4.3`).

### 26.2 Camera and input

- **One camera owner.** `CURSOR.frame` is the only thing that moves the camera: 240 ms for a descend or ascend, 160 ms for a
  pan, and a cut under reduced motion (`CG §4.2`). The camera's viewport is the board minus the header, the phone's camera
  column (`camInset`, #830: about 52 px on the right), the pad (`padInset`) and, while it is up, the tour's sheet
  (`TOUR.measure`).
- **Pointer and touch (#769).** As `CG §4.3` and §7. A click or tap on a Thing outside the current grid jumps there **no
  deeper than the cursor's current depth** (`CURNAV.nav().pick`), so a tap never dives two levels. The wheel and pinch
  depth gesture is 120 px a step, ends after 200 ms idle and commits past 30 %; `TestPinchZoomUnderChromium` became
  `TestPinchDepthUnderChromium`. `settleSoon`, `zoomBy`, drag-pan and the grab cursor were removed. The cursor-grid spec's revision 3 (gd-w37mF7SvFU) brought drag-pan and free zoom back as the free camera, with the cursor's
  depth following the zoom, and the test is `TestPinchZoomUnderChromium` again.
- **Two exceptions to "nothing else moves the camera" (N-D5, N-D7).** Ride-along (#771): "Go to plot" on a gardener
  followed it as it walked between plots (`walkto`, 100 px/s, 1-7 s), the one data-driven re-frame, opt-in, with a **Stop
  ride** button. 3b R13 retired it: "Go to plot" is now a jump, as "Show on the map" is. Drag to adopt (#814): a drag that starts on a Meadow flower lifted it (6 px
  with a mouse, a 450 ms hold on touch) and the camera pulled back to show the plots, without moving the cursor. Both are on
  the 3b cut list (R13, R14 in decision gd-KQJk4oNcVG). R14 (gd-7dO64x4OkB) removed the drag (`web/adoptdrag.js`, its CSS
  and `TestAdoptDrag*`): the bead view's **Adopt into a plan** (#707) is the one way to adopt a loose bead. Since
  gd-BXbYNWyOcF one press hands the bead to a gardener (an @gardenio mention in the land's general channel, the Commons
  compose path, which starts a thread agent to read the bead, choose the plan and phase, and write the edge; the panel
  links the thread), as the owner asked: "it should most definitely just call an agent to do its best on that, not make
  the user pick". The picker is behind **Pick the plan yourself…**, and opens by itself where no gardener can be asked.

### 26.3 Signs (#770, #826)

- **As `CG §6`.** One full label, child labels at 56 px and up, and glyphs drawn in a 3x5 pixel font. The full label
  cross-fades in 120 ms. The flicker probe (a `MutationObserver` on `#lbls` while the mouse sweeps 50 points) records zero
  mutations.
- **Additions.** The full label has a 44 px hit area on a coarse pointer, since a tap on it opens the bead view. `signVp`
  keeps labels clear of the header and the tab bar. The plate's edge is `--box-gold`. A status line over the cap drops whole
  parts from its end, ends with " …", and never drops the need words ("1 needs you"). The Yard's text labels were removed:
  its zones are glyphs and hot names, like every other grid.
- **Placement rule (#826, gd-UAKclcq5hu).** `placeLabel(th, size, view, others)` tries inside (when the Thing is big
  enough), above, below, right, then left. It takes the first slot that fits the viewport **and keeps off the grid's other
  cells**. If none keeps off them, it takes the first slot that fits. A wide label over a Meadow flower had hidden the next
  flower, and a click there opened the cursor's own Thing. `CG §6.2` was edited to match. `frame()` did not change.
- **Not built.** The LSDJ corner brackets of `CG §3.6`: the cursor's hot (`.hot.cur`) still draws the two-tone ring of
  section 14 as an outline and a box shadow.

### 26.4 The rollup stamp (#825, `CG §6.6`)

`WORLD.plotRollup` and `ART.drawPlotRollup` (`drawRollupPlate`, `drawRollupStamp`, `drawRollupMarks`), tested by
`TestPlotRollup` and `TestPlotRollupWired`. The beds show under the stamp from 90 px a side, and under 36 px the stamp drops
to the bar and the marks. Fixture plans E and F were added for the golden cases. Where it differs from the spec:

- **Paint order** is plate, fence, stamp, dim, need ring, marks, because `NB.fence` painted over the stamp.
- **The current-phase frame and the `!N` mark use `G` (`#ffe680`), not `Y`.** `Y` dims to grey in the night palette.
- The second argument to `plotRollup` also accepts the gardeners' looks, so the heads carry their hat colours.
- Each stamp is baked into an LRU of 96 bitmaps: about 0.7 ms a frame, against 0.5 ms for the flat plate.

### 26.5 Bead view, phone pad, tour

- **Bead view (#824).** A plan's bead view gained **Weeds** (first), **Gardeners here**, **Operator**, **Across
  projects** and **Phases (N)**, through `renderActs`, `BEAD.phases()` and `phaseNote()`. `CG §6.4` asked only for the
  first two; the other three hold the rest of what the card said. `plotWho` returns `{id, name, word, task}`. The edge and
  weed line caps are `LINES_MAX` (8). New golden: `beadplan`. The `overgrown` weed had already landed in 2b.1.
- **Phone pad (#816).** `web/pad.js`: one 60 px row (44 px arrows, 48 px round B and A), repeat at 300 ms then every 90 ms,
  arrows hidden under 376 px. Select latches for the next arrow **or swipe**. The pad's box runs behind the floating tab
  bar, and the pad hides while a Commons sheet is up (`body.cm-sheet`). The phone camera also frames under the floating
  header. Beacon keep-off rects are re-measured when the header resizes. `TestCursorPadUnderChromium`; the `projects`
  golden row became `pad`. The pad, its test and its golden row were removed in gd-wAN7P4Armw (cursor-grid spec 7, revision 3.1).
- **Tour (#768).** Each stop is a cursor jump (`cursorTo`), so the stop gets the full label and the ring. The sheet is an
  inset (`TOUR.inset`, `TOUR.measure`). `TOUR.place`, `TOUR.frame`, `TOUR.ring`, `.tour-ring` and the tour's own `flyTo`
  were removed. A Yard ask goes to `yard:decision`.

### 26.6 UX acceptance (2b.9, gd-teuyCvgQ5e)

`/commons-ux-acceptance` ran on the four golden cursor rows on `main` at c966223b, across C1-C12: phone-light, phone-dark,
desktop-light, desktop-dark, mid 820x1180 and the keyboard pass. Every PNG was read. C6 is N/A (no card theme) and C10 is
N/A (static grade), so the score is out of 10.

| Row | Score | Band | FAIL |
|---|---|---|---|
| `cursorworld` (depth 0, the cursor on gardenio) | 100 % | accept | none |
| `cursorproject` (depth 1, on plan A) | 100 % | accept | none |
| `cursorplan` (depth 2, on phase 1) | 70 % | revise | C1 gd-LdeL5zW3vH, C9 gd-b0cdLl09ub, C3 gd-SWjiYouIQt |
| `cursorphase` (depth 3, on a task) | 90 % | accept with nits | C1 gd-LdeL5zW3vH |

- **C1, gd-LdeL5zW3vH.** The full label gives the Thing's title one line minus the container prefix. So "Foundations"
  reads "Foundati…" on a phone, and a 30-character task title keeps 13 characters. The cut is honest (an ellipsis, the full
  name on the hot, Space opens the bead view), but `CG §6.1` allows the title a second line before cutting it.
- **C9, gd-b0cdLl09ub.** On a phone and at 820 px, the label's "above" slot covers the phase stakes. Those are the ordinal
  and the gold current-phase stake, and they sit on path tiles outside every cell rect, so #826's keep-off misses them.
- **C3, gd-SWjiYouIQt.** On a phone at the plan depth, the left-edge beacon sits on the cursor's own bed. The camera's
  viewport has no left inset for the beacons.
- **Keyboard.** Focus reaches the cursor's hot in 8 Tab presses at depths 0 and 1, and in 20 at depths 2 and 3, where the
  gardeners' links in `#ghots` come first. `:focus-visible` draws the gold ring in both themes. The hot's name carries the
  full title and "row r of R, column c of C". The skill's keyboard pass now looks for focus in `#hots` for these rows
  (`FOCUS_ROOTS` in its `shoot.js`): `#lbls` is aria-hidden by design, so it never took focus.
- **Contrast** on the canvas was pixel-sampled. In the night palette the stake ordinals measure 3.3:1 to 5.4:1 against the
  path tile, and the gold `!N` 14.6:1. Both clear 3:1 for glyphs.

All four rows were already goldens, so none was promoted. The three FAILs are filed as bugs under gd-7bfFegkYjS. They touch
`web/cursor.js` and the `garden.js` framing, so they wait for the 3b cleanup rows that are editing those files.

### 26.7 Packed plot slots (gd-b228pTt33E, decision gd-VfOCGDpxg2)

The owner chose "pack" (`CG §12.2`) and kept `W = 5` (`CG §12.3`). `layout()` step 1 ranks a project's plans by memo'd slot
(ties by `created`, then id), then puts the newcomers in `(created, id)` order, and gives the i-th plan spiral slot i
(section 3.3). The memo keeps its v3 shape, and a slot entry now reads as a rank, so no version bump was needed. When a
plan finishes, only the plots after it step back, one slot each, in the same order. A new plan takes the next slot, so it
moves nobody. A cold layout was already packed, so the 10x golden digest and every browser golden are unchanged.

- **Tests.** `TestWorldLayoutUnderNode` closes the middle plan of five: v3 and v4 each step back one spiral step, v0 and
  v1 stay, and a sixth plan, the oldest of all, takes the next slot. A plan that reopens with its memo entry gets its old
  place back, and seven plans sit in a 3x3 with the last two slots free. `world_tenx_test.js` §3d is inverted. When 20
  plans close, no project changes index. In every project the survivors keep their order and fill `0..n-1`, each one back
  one slot per closed plan ahead of it. A project that lost no plan moves as a whole.
- **Cursor and camera (`CG §3.4`).** The cursor follows its plot by id. `onFarmDoc` keeps `cam`, so the camera does not
  move until the next cursor move re-frames it. At the project depth the framed 3x3 still holds a plot that stepped back.
  At the plan depth and deeper, a one-step move (34 tiles) can carry the cursor's plot out of the framing until the next
  move. The off-screen beacon then points at it. A data-driven re-frame for that case would be a `garden.js` change. This
  step did not make it, because it was scoped to `world.js` only.
- **UX acceptance.** `/commons-ux-acceptance` was run on `cursorproject` with a memo seeded as a browser held it before the
  gardenio plan in slot 2 closed, then the farm served without that plan. Before: a fallow hole at the top middle. After:
  slots 0..5 filled, and the tail of the ring (bottom, bottom-left, left) is fallow. The cursor and its label stay on plan
  A, and the label moves to a free side. The probe hits (hover-only rules, untokened colours, focus on `#lbls`) are
  page-wide and the same before and after.

### 26.8 The bin by the plot (gd-LohQMNM3wU, #921; acceptance gd-E7jGutigmx) — retired

> **Retired** in plan gd-PJB3D7glwF (step 4, gd-vDauUoOkj8, #1989): `web/bin.js`, the `#bin` plate, its phone row, the
> I key and the plot's crate mark are gone, and the farm's Escalations cell (28.13) answers the asks instead. The read
> model stays: `FarmPlan.produce[]` (`farm_produce.go`) still feeds the bead view, the harvest cells (26.9) and the goats'
> kin (26.10, 26.11). The notes below are the record of what shipped.

`CG §5.6`, build steps 10 and 11. A plan's MRs, bundles and reports (`FarmPlan.produce[]`, `produce_more`) show in a small
plate beside its plot while the cursor is on the plot or inside it. The data was already in the farm read model
(`farm_produce.go`), so the page derives only the words.

- **Code.** `web/bin.js` (new, after `cursor.js`): pure `BIN.rows`, `BIN.count` and `BIN.place`, plus the `#bin` DOM part
  (`BIN.wire`). `art.js` adds `ART.drawProduceIcon` and `ART.drawProduceMark`. `CURSOR.frame` takes `opts.also`, a rect the
  framing widens to take in. It zooms out, never in, and is the peek. `garden.js` adds `syncBin`, which runs from
  `refreshSigns`, so the bin is measured once per cursor move, never per frame, and then rides the world with the signs. It
  also adds `peekRow` and `goRow`, and the `i` key. The board's `aria-label` names `I`. Fixture: plan A gained a merged MR on
  a finished step and the plan's narrative bundle, next to its open MR. Tests: `TestBinUnderNode`, `TestBinWired`,
  `TestBinUnderChromium`, the `frame` `also` cases in `cursor_test.js`, and a `bin` golden row (depth 1, `i` into the bin,
  the coming row focused and peeking).
- **Where it differs from the spec.**
  - **"From".** A closed step is not in the farm document. So a row's "from" names the step when the map has it, else its
    phase, else the plan. A peek or a go aims at the first of step, phase bed and plot that the map has (`row.to`).
  - **The crate mark.** It is a 13x9 px plate astride the plot's bottom fence at the left: a 5x5 crate and the ready count as
    a pixel digit, the need mark mirrored. The spec says only "5x5 at the bottom-left". A bin holding only expected produce
    draws a stone-grey crate with no digit.
  - **Icon colours.** The icons are painted in the day palette in both themes, because the plate is dark in both.
  - **Hard rects.** `BIN.place` takes a fifth argument, `hard`: rects it never covers (the full label, the HUD, an open bead
    view or tour). Its other cells stay soft, as in the spec: when no side keeps off them, it takes the first side that
    fits.
  - **Docking.** `syncBin` docks the bin as the phone's row on a coarse pointer or at 600 px and under, as the spec says. It
    also docks the bin on a desktop whenever `place` returns null. That second case is the C4 FAIL below.
  - **Tab stops.** Desktop rows are 22 px tall under a fine pointer. The docked row's cells are 44x44.
- **UX acceptance (step 11).** `/commons-ux-acceptance` ran on `cursorplan` (depth 2) and on the `bin` row (depth 1) on
  `main` at b51d022c. That build carries 3b R1, R5 and R11, but not R4 (#884) or R8 (#901). The cells were phone 390x844
  light and dark, desktop 1440x900 light and dark, mid 820x1180, and the keyboard pass. C6 is N/A (no card theme) and C10 is
  N/A (static grade). **80 %, revise**: C1, C2, C5, C7, C8, C9, C11 and C12 pass. Two FAILs are filed under gd-7bfFegkYjS:
  - **C4, gd-Zbg4TEnOWE.** At 1440x900 on the plan depth, the plot fills the camera's viewport, so no side holds the bin. It
    docks as a full-width strip at the board's bottom, and the "Legend & keys" tab lies over all three rows: no produce is
    visible or clickable. The 1280x800 golden does not show this, because there the bin fits on the plot's right. 3b R9
    moves Legend & keys into the Menu, so the tab is no longer on the map.
  - **C3, gd-xPz1vKRlMV.** The phone row ends `camInset().right` short of the edge, but `#cam-tend` floats left of the camera
    column, over the row's right end. Filled to the cap of 8 and scrolled to its end, the last item sits under Tend. 3b R8
    moves Tend into the Menu. Re-check this then.

  The keyboard pass met the spec. The bin is `role=group`, named "Produce of Edge IdP rollout: 2 in the bin, 1 coming", and
  it is the next tab stop after `#hots`. Each row's name is `<state word> <kind>: <title>, from <step>`, and focus draws the
  gold ring in both themes. At depth 1 on a desktop, the bin sits right of plot A over the top-left beds of the plot beside
  it. The full label holds the left side and every other side meets a cell, so the spec's fallback applies. No golden was
  promoted or re-shot: `bin` and `cursorplan` were already goldens, and R4 and R8 own the next re-shoot. The grade and the
  PNGs are on gd-E7jGutigmx.

### 26.9 Harvest cells (4b.6, gd-nsaGxscX79; spec `2026-10-22-lands-harvest-icons-design.md`)

A plan's listed closed steps (`FarmPhase.closed[]`, #872) are harvest cells: cursor stops whose sprite says how the step
ended and what it proved. Six PRs, one per spec step.

- **Writers (#911).** kbeads gained the `evidence` type. The `commons-ux-acceptance` skill files one per grade
  (`kind=ux`, `score`, `of`, `urls`) parented to the step. github-bridge writes `ci_state`, `tests_touched` and
  `goldens_touched` at merge. The merge desk writes `review_rounds`.
- **Read model (#960).** `farm_harvest.go`: a `harvest` object on the 12 newest closed rows of each plan, joined to the
  evidence list by `step`. Older closed rows stay plain stubble and are still cursor stops.
- **Bead view (#922).** A closed step's view opens on a Harvest section: the base word and badge words, the newest grade's
  pictures through the image proxy, every grade, lead time and cost in words.
- **Cells (#971).** `art.js` draws the sheaf, the trampled base and the six badges (`ART.harvestBead`). Two badges fit at
  1x and four at 2x. At night only the lantern stays lit.
- **Words (#972).** The status line on a harvest cell reads `harvested · @who · 2d ago · merged #825 · …`. The full label
  has a second line of icon and word pairs, cut at the plate's width with `+N`.
- **The bin (step 6, gd-HCYHJLfDPO; retired with it, 26.8).** When the cursor is on a closed step, `BIN.mark(rows, step)` marks the bin
  rows the step made: its MRs, bundles and reports, whose `task` is that step. A marked row gets a 3 px gold bar on its
  leading edge and a tint (`.bin-r.hl`). In the phone's docked row the bar sits under the icon. Its accessible name ends
  `(this step)`. The idle caption starts `1 from this step` at full ink, ahead of the dimmed key hint, and the dock's tally
  ends with it. `i` lands on the first marked row. Peek and go are unchanged. `binStep` in `garden.js` picks the cursor's
  node when it is a closed `task`, with or without a `harvest` object: a step past the 12 still made what it made. Tests:
  `mark` cases in `bin_test.js`; in `bin_browser.js`, the walk on `gd-A1y` and `gd-A1x` on a desktop and on the phone's
  dock; and a new `binharvest` golden.
  - **Under the bin's LOD (#959, gd-2qgcDRZUX6).** The bin reads at the cursor's depth, and the owner asked for "just the
    info for the LOD selected". On a step that is the step's own bin (`binScope` gives `{task}`), so on a harvest cell every
    row it made is marked. The mark does not pick rows out of the plan's list; it ties the bin to the gold cell and says what
    the bin holds. `BIN.mark` is depth-blind, so a wider bin would mark only the step's rows, and `bin_test.js` checks that
    too. The bin was not widened to the bed on a harvest cell, which would have overruled the owner's LOD. If the owner wants
    the step's rows among its phase's, that is a one-line change to `binScope`.
  - **The step's own `done` row is not marked.** #959 lists a finished step with no MR as a `done` row (kind `step`). That
    row is the step itself, not its produce. On `gd-A1x` the bin is that one row: unmarked, no words.
  - **Where it differs from the spec.** The spec says only "highlights". The count in the caption and `i` landing on the
    step's row are additions, so the mark is never colour alone.

### 26.10 The goat pen (3.1 of plan gd-EGpbDpqsec, gd-kVKuWqIK1v)

Since plan gd-LurZOR44M1 (steps 3 and 8) each project has its own pen, the Pen cell of its home strip (28.2; the
farmstead's south-west quarter since 29.2), id `home:<key>/pen`, and the Commons pens the project-less wishes. The pen south
of the Yard below is how it was; its goats, gate, sign and cards are unchanged.

- **Layout.** `WORLD.penOf(doc.wishes)` is a fenced band south of the Yard scene (`world.pen`, id `yard:pen`): `PEN.gap` (2)
  tiles of grass under the Yard, then a 1-tile fence round `PEN.cols` (7) cells a row, each `PEN.dx` x `PEN.dy` (3 x 2) tiles,
  one per open wish in the document's order (newest first), at most `PEN.cap` (63, nine rows). It is sized from the count
  alone, so the same wishes give a byte-identical cold layout, and it is no wider than the Yard (23 tiles under its 24), so
  the Commons and the projects east of cell 0 never move. Cell 0's size becomes the Yard and its pen, mirrored about the
  origin (a cell is centred on it), so no region in the next row reaches the fence. The world's bbox takes the pen with a
  `GAP` of grass round it.
- **Index and links.** Each listed wish is in `world.index` once, as `wish:<id>` (kind `goat`, its cell, `out` when a plot of
  its kin is on the map); the pen is `yard:pen` (kind `pen`). `at=yard/pen` and `at=yard/wish:<id>` resolve to them, and a bare
  wish id finds its goat through the cursor's alias. The headstones of 3.2 use the same `wish:<id>` form for closed wishes.
- **Cursor.** With a pen the Yard's grid is 3 x 2: the zones, then the pen as a spanning band (Down from any zone), whose
  own grid is `PEN.cols` wide with a goat per cell. A Yard-only rebuild (`CURNAV` `yard()`) keeps the pen. The pen's status
  reads "N open wishes · M out grazing"; a goat's "in the pen" or "out grazing by its plan", tended or not, and its age.
- **Drawn (`web/goats.js`; the 16 x 12 goat of 26.15).** The fence from the Project level in (`ART.drawFenceClip`, through the page's sprite cache); at
  Plan and Bead every penned goat (no `MAX` cap in the pen), grazing at its cell, looking up and turning between spells,
  ghost or solid as anywhere; reduced motion keeps it still, head down. A goat out grazing by its kin keeps its cell, where
  an open gate (`GATE`, 24 x 20 world px, in the fence's wood) says so; hovering the gate names the wish, and its card adds
  "out grazing by its plan". Kinless goats no longer lap the Yard, so 2.5's Yard count moved to the pen's sign: at Plan and
  Bead it adds "+N not in the pen" for the wishes past `PEN.cap` and those the farm's per-project cap left out (`open_more`).
- **Zoom to it (gd-xlRpUpTjPZ).** A goat's card, and so the gate's, ends "Thread in the Commons · Zoom to it on the map": a
  cursor jump (`GOATS.goTo`, `GOATS.wire`'s `go` through `STIP`'s `env().go`) into the plot the goat grazes by when it is out
  (the cursor on its first phase, as a gardener's "Show on the map": the plot itself frames its project at the Project level,
  where no goat is drawn), else to its cell in the pen at the Bead level, so from the Yard a goat is a click from where it
  stands. A hovered goat's card stays `LEAVE_MS` after the mouse leaves the goat, so the mouse can reach its links. The link's own `href` is the place's `?at=`
  (`<project>/<plot>`, `yard/wish:<id>`), for a load without script and a middle click. Enter on an out goat's pen cell, which
  has nothing to descend into, follows it the same way (`GOATS.grazes`, `GOATS.wire`'s `enter`, from `garden.js` `cursorAct`); on a penned goat it bumps as before.
  The cursor's cell buttons that span the pen (the pen band's, the Yard's, a goat's own) covered every penned goat and gate, so
  from the Yard no hover or tap reached one; `GOATS.wire`'s `penGround` now lets a goat hit through them (hover, and a click in
  the board's capture, before the cell's own `cellClick`), as on the bare canvas.

### 26.11 The graveyard (3.2 of plan gd-EGpbDpqsec, gd-s9KzRk6boh)

A closed wish (the farm document's `wishes.closed[]`, a closed thread head) is a headstone in its project's graveyard.

- **Layout.** `WORLD.shape` takes a sixth argument, the stone count (at most `CAPS.graveyard`, 24, the farm's
  `maxClosedWishesPerProject`). The graveyard is a band of `ceil(n / cols)` rows of 1-tile stones at pitch 2 (`cols` is the
  band width over 2, at least 26, so one row today), between the Orchard and the plots: `orchardGap` above the plots, then the
  stones, then `graveGap` (2) and the Orchard, which with the Meadow and the sign moves up by the band. With no stone the band
  does not exist and the region is the size it was, so a farm with no closed wish lays out byte for byte as before (the test
  holds this). Stones go newest first in reading order, not hashed like the Orchard's trees, so a wish that closes takes the
  first stone and the rest step along one; that is the order the step asked for.
- **Identity.** A stone's id, its `world.index` key and its hot are `wish:<id>`, never the bare bead id (the pen's goats use
  the same key for open wishes; a wish is one or the other). `at=gardenio/wish%3A<id>` resolves to it, and the cursor's
  `pathFor` finds it by the bead id too (an alias). Its link opens the wish's head bead.
- **Cursor.** `ROW` gains `graveyard: 2` between the Orchard and the plots, so the plots, the Nursery and the crate are one
  row lower in the project grid; an empty row is stepped over. The band reads "Graveyard", "3 closed wishes"; a stone reads
  its short title and `done · closed 2d ago` (the outcome and when it closed). Stones are glyph-only (`TEXTLESS`).
- **Sprite.** `ART.drawHeadstone`: a round-topped grey stone with two carved lines on a tuft of grass, and a pink flower at
  its foot when the wish was done. Abandoned and expired stones are both plain, as the step says; the outcome is in words on
  the status line and the tip. Below the Bead level a stone is a band dot in the stone's light grey.
- **Hover.** `GOATS.stoneCard`, in `STIP` as a steward's tip is (hover, focus, a finger's first tap): who asked, the outcome
  and its reason in words (`slack-thread-user-close` reads "the asker closed it", the idle autoclose "the thread went quiet and
  closed itself", an agent failure "its agent failed and nobody took it up", any other reason as written), when it closed,
  what its kin merged (each merged MR a link, at most 6 and a count of the rest) and its thread in the Commons. The read
  model's `reason`, `produce` and `href` on `FarmClosedWish` feed it (`homestead-farm-read-model.md`, Wishes).
- **Tests and goldens.** `TestGraveyardUnderNode` (layout, determinism, index and links, cursor rows and moves, the card, the
  sprite), `TestWishProduce` and the `TestFarmWishes` additions. The fixture farm gained three closed wishes on gardenio
  (done with a kin's merged MR, abandoned, expired), which grew gardenio's region and moved every golden that shows the world;
  the new `graveyard` golden is the cursor on the done stone with its tip open.

### 26.12 The pen and the graveyard on a phone (3.3 of plan gd-EGpbDpqsec, gd-NbWjIdLTnd)

Since step 8 of plan gd-LurZOR44M1 the jump index has one pen row per project with open wishes (`home:<key>/pen`, "<project>
pen"), and every pen, goat, Graveyard and stone row is a cursor path under its own project (`TestGraveyardUnderNode`).

The pen and the graveyards are band cells (26.1), so a keyboard reaches them by Down from a Yard zone and by Up from a
project's plots. A phone has the pad's arrows too, but the way it is meant to find a thing is the camera column's **Go**,
the `/` jump list, so both are in it.

- **The jump index.** `WORLD.searchIndex` lists, besides what it listed before, the pen (kind `pen`, id `yard:pen`, "The
  pen", its count of open wishes as `note`), one row per penned goat (kind `goat`, id `wish:<id>`, the wish's title, `note`
  "untended", "tended" or "out grazing by its plan") and, per project with a closed wish, its Graveyard band (kind
  `graveyard`, id `band:<key>/graveyard`, its count as `note`). Each id is a cursor node, so choosing a row is a cursor jump
  (`CURNAV.to`) that frames it as the keys would: the pen whole under the Yard, a Graveyard band in its project (A then
  goes in among the stones), a goat or a stone at the Bead level.
- **Matching.** A goat and a stone carry the wish's own id as `bead`, and `WORLD.search` matches ids on it instead of the
  `wish:` key: the bead id typed exactly finds the goat or stone first (no "Open gd-..." row), and "wish" no longer
  matches every goat and stone by id prefix. The pen and a Graveyard also answer to `aka` words ("goats open wishes",
  "headstones closed wishes") as a word prefix, so "wish" lists the pen, the Graveyards, then the goats and stones. The
  empty query lists the Yard, the pen and the projects. Ties go Yard, pen, projects, plans, beads, goats, graveyards,
  stones. The rows read "pen", "open wish", "graveyard" and "closed wish" (`KIND_WORD`), each with its `note`.
- **390 px.** Nothing new is drawn for the phone. The list is the jump palette's (`min(460px, 100% - 24px)` wide, rows cut
  with an ellipsis), the pen's framing fits it whole above the pad (23 tiles wide), and the cursor's full label on the pen
  reads "The Yard › The pen" with its status cut at the sign's cap like any other ("2 open wishes …").
- **Goldens.** Phone only: `jumpwish` (Go, "wish": the pen, gardenio's Graveyard, the goats, the stones), `pen` (its row
  tapped: the pen framed with its label) and `graveyardjump` (the Graveyard's row tapped: the band in its project). Tests:
  `TestGoatsUnderNode` and `TestGraveyardUnderNode` (the index rows, the ranking, each id a cursor path).

### 26.13 Staked plots (step 2 of plan gd-2RdeAx8nnW, gd-1j42h1tg6y)

A `ready` or `asked` seed takes a plot slot (section 3.3); this step lays it out and draws it. Reading it (the sign's
"staked" word, the tip's step list, the bead view's Steps section) is step 3.

- **Art (`art.js`).** `drawStakedBase` is `drawPlotBase` with surveyor's string for the fence (a string line round the
  ring, a stake at each corner and every fourth tile) and pale turned soil for the beds (the soil furrows in sand tones).
  At the Bead level each step's cell is a stake marker, a post with a paper tag (`drawStepStake`), where a planted step has
  its plant; a frosted variant (ice-blue tag) exists for drafts and is unused while drafts are pots. At the Plan level the
  frame is the pale string colour (`crop: "staked"`), the beds two sand tones, and a step is the same marker in flat
  boxes. The operator's coloured fence (section 23) is not drawn on a staked plot: the string is its fence until it is
  planted.
- **Tip.** A staked plot's link wears its seed's pot card (`web/stip.js`, class `w-staked`) on hover and focus, as its pot
  did, and drops it once planted. A click or tap on it is a plot's: the cursor, then in. On a phone the card is in the
  plan's bead view (`?bead=<plan id>`, the Staked out section); step 3 adds the first-tap tip on a tablet (26.14).
- **Tests.** `TestWorldLayoutStaked` (node) pins the slot order beside the plans, the bed of planned cells, the same slot
  and pixels after approve, drafts in the Nursery, the cap and overflow, and that a farm whose seeds are all drafts lays out
  its plots exactly as one with no seeds. `TestPotUnderChromium` hovers the staked plot, answers its card and checks the
  plot does not move when it is planted. The 10x fixture (`world_tenx.js`) now makes every seed a draft, so the 10x golden
  digest is unchanged: it is the farm with no ready seed. The browser walks that took the first plot in reading order
  skip a staked one. The golden fixture's ready seed `gd-seed2` (no steps) is staked in the gardenio fields, so the
  `/garden` goldens that frame the gardenio project were re-shot in this step.

### 26.14 Reading the plan from the plot (step 3 of plan gd-2RdeAx8nnW, gd-53IHFkfPLk)

A staked plot reads the whole plan: its sign, its tip and its bead view.

- **Sign (`cursor.js` buildPlot).** The cursor's full label on a staked plot is the plan's short title (`seeds[].short_title`,
  homestead-farm-read-model.md FarmSeed; the title when the plan has none) with its status on the line under it, always (`fullText`'s `under`):
  `staked · N steps · 1 needs you`, cut from its end to fit as any status is, never the need words. A planted plot's label
  is as it was.
- **Tip (`web/stip.js`).** Under the pot card (the title, its state in words, the answer card) the tip lists the steps from
  `steps`, numbered as the stakes, each a link to the step's bead view; then "Open the plan". On a desktop it opens on hover
  and focus. A finger's first tap on a staked plot opens it, pinned (`STIP.tap`, which `cellClick` asks before the plot's
  own tap, and which moves the cursor onto the plot when it was elsewhere); the second closes it and goes in, as on any
  plot. The cursor's full label lies over the plot: a tap on it opens the plan's bead view, as a label always does.
- **Bead view (`web/workbench.js`).** The plan's first section is named "Staked out" (it is "In the Nursery" on a draft's
  pot). After it, "Steps (N)" (`STIP.potSteps`): each step in step order, numbered, its title opening its bead view, and its
  body from the bead route's child rows (`children[].body`, read model step 1) cut to its first 280 runes until a click or
  Enter opens it whole (the open ones stay open across the view's repaints); then "The plan", the narrative from
  `summary`. The steps are not listed again under Children.
- **Deep links.** `?at=<project>/<plan id>` resolves to the staked plot at the Plan level, and a step's id to its cell;
  `?bead=<plan id>` opens the plan's bead view. Once approved the same `at=` is the planted plot, in the same place.
- **Tests.** `TestStakedDeepLink` (node: resolve, the cursor's path and `at=`, the sign at desktop and phone widths, the
  planted plot after approve). `TestPotUnderChromium` gives `gd-seed2` two steps with bodies and walks the tip's step list
  on a desktop, the bead view's Steps (the cut body opening on a tap, the narrative, no Children list) on a phone, and the
  first and second tap on a tablet. The `pot` and `potbead` goldens give `gd-seed2` three steps, bodies and a narrative in
  their routes (step 4 adds a ten-step plan to the fixture itself).

### 26.15 Birds and goats at twice the resolution (plan gd-ZsV0W4qKLy)

The birds (`web/birds.js`, section 4b of the agents-as-people spec) and the goats (`web/goats.js`, 26.10) were two-colour
sprites blown up to big blocks: a sparrow 5 x 3, a crow 7 x 4 at 3 CSS px per sprite px (21 CSS px wide at Plan), a goat
8 x 6 at 2 (16 x 12 CSS px). Next to the 16 x 24 gardeners they read as blobs. The plan redrew both at twice the sprite
resolution with more colours and halved the blow-up, so the footprint on screen stayed the same.

- **Sprites.** Rows of the letters `a` to `f`, each the colour at its place in the palette (step 1, #1174: the run encoder
  takes any letter). The sparrow, finch and pigeon are 10 x 6; the crow, twig bird, gull, swift and hawk 14 x 8, each in
  four colours (body, wing or back, belly or beak, eye) by day and by night (step 3, #1194). The goat is 16 x 12 in five
  colours (coat, shade, horns and hooves, eye, beard) and three coats, white, brown and black (step 2, #1189).
- **Frames.** A bird has three, a flap up, level, down, level; the hawk a fourth, its glide, which it soars and lands in. A
  goat has four: grazing head down, walking, chewing (its jaw a px lower), and the walk's second step (step 4, #1198).
- **Size.** Device px per sprite px stay whole (`pxOf`), so every sprite px is a block of screen px.

  | | Plan (`PX`) | Bead (`ZK` x zoom, never under `BEAD_PX`) | On screen at Plan |
  |---|---|---|---|
  | Birds, before | 3 | 2.5 x zoom, never under 4 | crow 21 CSS px |
  | Birds, now | 1.5, rounded | 1.25 x zoom, never under 2 | crow 21 CSS px at DPR 2, 28 at DPR 1 |
  | Goats, before | 2 | 2 x zoom, never under 3 | 16 x 12 CSS px |
  | Goats, now | 1 | 1 x zoom, never under 2 | 16 x 12 CSS px |

  At DPR 2 a bird is the size it was; at DPR 1 the 1.5 rounds up to 2 device px, so a bird is one notch bigger (a crow 28
  px). A goat is 16 x 12 at Plan on every screen. At Bead a bird is the size it was; a goat below 2x zoom is 32 x 24 (it
  was 24 x 18 below 1.5x), the same from 2x in. The goat still fits the 2-tile path round a plot at every Plan zoom, and
  its body a gutter at Bead from 1.5x across y and 2x across x (`goats_test.go`).
- **Contrast.** Unchanged rules, on the new palettes: body against its ring and the ring against every ground at 3:1, day
  and night; the goat's horns 3:1 off its coat (`birds_test.go`, `goats_test.go`).
- **Owner's check-in.** Live in 2026.300.2 (helm rev 250). Matthew chose to keep it as drawn (decision gd-jMkiTmohau): no
  bigger Bead size, no flatter palette, no species redrawn.
- **Goldens.** Steps 2 to 4 re-shot every golden that shows a bird or a goat (GOLDENS.md, the goats paragraph). Step 6
  (gd-tjW8sTFfJH) graded them with `commons-ux-acceptance`.

### 26.16 The release truck (plan gd-UZKZ9H25UD: step 7 gd-yOOVahJMCh, 9a gd-r4xwtDmrn2, 9b gd-rSz81Me1Pe, 10 gd-HHMReWCHUi)

`web/truck.js`, in the goats' pattern: pure `norm()`, `bayOf()`, `place()`, `load()`, `depart()`, `pose()`,
`board()`, `tagFit()`, `shippedOf()` and `card()` under node (`truck_test.go`, `testdata/truck_test.js`), `draw()` per
render. The record it shows is [`releases-design.md`](releases-design.md) section 3; the blob it reads is
[`homestead-farm-read-model.md`](homestead-farm-read-model.md#releases). These are the build notes as shipped.

- **What it reads.** The farm document's `releases` blob, per project, through `TRUCK.norm` (the read model's "How the
  page reads it"), and a plot's `FarmPlan.shipped`. An empty deployments list is "No deployments yet", never an error.
  A project with a release source and no release yet has no truck.
- **Where it stands.** One truck per project with a newest release and a biome on the map, in its home strip's loading
  bay: `bayOf(b)` returns `b.home.bay`, a 6 x 4 tile rect at the Barn cell's south-east corner (28.10), under the
  project's sign (28.1). It faces east, towards the Pen, the way it drives out. `world.index` gains `release:<tag>`
  (kind `truck`, the bay's tiles), or `release:<project>/<tag>` when two projects share a tag;
  `?at=<project>/release:<tag>` resolves to it at the Plan level. The strip paints two wheel ruts on the bay whenever
  the project has any release (28.10).
- **Size.** The ride truck's map (`ART.TRUCK`) in its own red with an empty cab (`ART.drawReleaseTruck`), at `S` = 3
  world px per sprite px: a 24 x 16 sprite, so a 72 x 48 world px box (`TW` x `TH`), standing centred on the floor of
  its 6 x 4 tile bay with its wheels on the bay's ruts (`place`: `x` = the bay's left plus half the spare width, `y` =
  the bay's bottom less `TH`). 9a had doubled it from `S` = 2, when at the Plan level it was about 40 px wide on a
  desktop and its crates and tag could not be read without the tip; at `S` = 4 it filled the bay and dwarfed the strip's
  figures, so the owner had it brought down to 3 (gd-Ir8jxl4c57). Its detail (`BED`, `PLATE`, `DOOR`, `SIGN`, `CRATE`,
  `PITCH`) is laid out in art px of `K` = `S` / 2 world px (1.5), so one knob sizes the whole truck.
- **Crates.** `load(rel)`: one crate per plan, most PRs first, then one for the unplanned PRs, at most `MAX_CRATES`
  (8), in two rows of four on the bed. Each is a 5 x 5 crate with the bin icon's lid (`ART.drawCrate`, `K` px per crate
  px), `PITCH` 6 apart. A loading truck carries them; a parked one carries none.
- **Tag plate.** A side plate of `PLATE` = 22 x 9 art px below the bed: a dark rim (`k`) and a gold face (`G`, which
  keeps its light at night), painted before the crates so its top rim is their bottom edge. The tag is in 3 x 5 letter
  glyphs, `K` px each, in `k`. `tagFit(tag, PLATE.w - 4)` keeps the tag's tail: whole dot-parts first, then
  characters (`306.3` for `2026.306.3`); the tip names it in full. The 4 art px it leaves keep a gold art px round the
  letters on every side (step 10: the plate was 20 x 7 with the letters on its rim, C1 of the rubric).
- **Door.** The cab's door (`DOOR`) carries the release's PR count (`count`, every contents row, up to 999) in `j`,
  centred. No count, nothing on the door.
- **Board over the cab.** While the truck loads or stands parked, never on the road, a board (`SIGN`, 9 art px high,
  on a post 12 art px above the box) says where it runs or what it is, and since when (`board()`): a live release's
  first target and `+N` for more (`PROD 2H`, `PROD+1 3D`), else its status (`CUT 40M`, `BUILT 5H`). The age is
  minutes under an hour, hours under 48, then days. The board is gold when live, `J` when superseded and `j`
  otherwise, at most `BOARD` (44) art px wide, the word cut short before the age. 9b put it in place of step 7's gold
  SHIPPED plate, which said only that it had gone.
- **Moving.** `depart(prev, rels, t)` compares two documents. When a truck was loading (its release was shipping) and
  that release is now live, it drives out at `t`, and the page shows the toast "Released 2026.306.3 to prod
  (gardenio)". The first document never starts a drive. `pose` takes `DRIVE` (3.5 s) east along the bay's row to past
  the world's bbox, stays `AWAY` (1.5 s), takes `BACK` (3.5 s) to return facing the other way and empty, then parks.
  `draw` returns true while a truck is moving, so the page asks for the next frame. Under reduced motion there is no
  drive: the truck stands still, empty, its board gold with the target. Honest limit: the drive follows the bay's row
  in a straight line and can pass over a neighbour's region on its way out.
- **Levels.** Trucks are drawn after the plots and before the goats, at the Plan and Bead levels only. At the Project
  level the project's sign says "3 releases, 1 shipping" on a row of its own (`TRUCK.line`); the count is the releases
  in the blob's `recent`, at most 12. The World level shows nothing.
- **A shipped plot.** A plot whose newest merged PR a release carries (`FarmPlan.shipped`, whatever that release's
  status: the stamp comes with the cut, not the deploy) gets the shipped stamp (`ART.drawShippedMark`, `scene.js`) from
  the Project level in: a 9 x 9 plate (`k` outline, `N` face) with a 5 x 5 crate on it, on the plot's top fence at
  the left, the need mark's mirror. It is drawn at 2x when the plot is 64 px or more on screen, as the heads are, and
  sits one px above the fence, low enough that the cursor's ring (`.hot.cur`, outside the plot) covers only its rim
  (step 10: it rode 4 px above the fence at 1x, and the ring hid it). At the Plan and Bead levels the plot's full label
  ends with a row "shipped in 2026.306.3 · prod" (`TRUCK.line`, `CURSOR.signs`' `vp.extra(n, cap)`): the targets
  listed are those running that release or a newer one, in `recent` order, so before the deploy the row is "shipped in
  2026.306.3" alone. The label's box is sized and placed with the
  row, so it keeps off the safe area's edge and the beacon ring (`TestSignFitUnderChromium`). Past the cap (28
  characters on a phone, 40 on a desktop) the targets go first, then the tag keeps its tail ("shipped in 306.3").
- **Link, tip and keys.** Each truck is a link in `#ghots` (`TRUCK.hots`, kind `truck`, levels Plan and Bead, subject
  to the 44 px rule on a phone). Hover, focus or a first tap shows `TRUCK.card` in the steward's tip: "Release
  <tag>"; "Loading: Cut, not built or deployed yet · cut 3h ago"; "34 PRs from 5 plans, 2 unplanned"; "prod runs it
  since 2h ago" (or "prod runs <older tag>", or "No deployments yet"); and the link "Open the changelog". The
  tip's text is the link's accessible name. A click opens the release panel, `/garden?release=<id>` (`TRUCK.href`);
  with no id in the blob it links the truck itself, `?at=<project>/<truck id>`, since a link must never leave
  `/garden` (`TestGardenLinksStayInside`). On the cursor the truck is the Barn zone's last item (28.10): Right from the
  last gardener reaches it and Enter opens the same panel.
- **The release panel.** `web/release.js`, the Workbench's `release` kind (9b): it reads the viewer's
  `/api/releases/<id>` and paints the status, the cut time, where it runs, links to the previous and next release and
  to the release bead, then "Changelog" with a summary ("34 PRs from 5 plans") and the PRs grouped by plan, then step:
  plans the release finished first, then most PRs, unplanned last ("Not in a plan"). A plan or step with no title
  reads "Untitled plan" or "Untitled step", its id in the tooltip, never a bare id (step 10). The release bead's own
  face in the bead view (`web/relface.js`, gd-j3bQqfZSt7) links back with "Open the changelog".
- **Legend.** The red truck's row: "a release by its project's sign, a crate per plan, its PR count on the door and
  over the cab where it runs or its status, and since when; it drives out when the release goes live, and a click opens
  its changelog".
- **Goldens.** `truckload` (loading, Plan level), `truckgone` (live, still under reduced motion, the gold board) and
  `truck` (the tip at the Bead level), each on phone and desktop in light and dark, fed
  `testdata/releases_fixture.json` through `TRUCK.feed`, because it has the two moments `truckgone` needs, shipping
  then live. Step 10 adds `release` (Enter on the focused truck: the panel), `shippedplot` (plan A's stamp with the
  cursor on it, from the fixture farm's releases blob) and `facerelease` (the release face in the bead view). The pin
  holds against the live stream's documents.

## 27. Deviations by phase (2b, 3, 3b, 4)

Where the live page differs from the Phase 1 design above, one phase at a time. The build notes cited hold the detail.

- **2b, cursor world (gd-7bfFegkYjS).** The free camera and the pointer-aimed text were replaced by a cursor over a grid of
  grids (#687, #689, #708, #769, #770, #816, #825). Sections 2, 3.1 to 3.3, 5 to 8, 11, 13, 14, 17 and 20 now describe the
  old page; section 26 and the cursor-grid spec describe what is live.
- **3, connections (gd-DyWUfS4n94).** Cross-project edges are irrigation channels (#706, section 25). Gardeners walk
  between plots (#771). Meadow adoption is an admin POST (#707). Project-scoped visibility was decided, not built (#711,
  farm read model Authorization). Of these, the ride-along camera and drag-to-adopt (#814, #831) were later retired by 3b.
- **3b, chrome cut (gd-of9UrulNWk, R1 to R15).** The bar is In · Out · Go · Menu, or Go · Menu on a phone (#874, #901).
  The Menu holds Yard, All, Plans, Outline, Tend, Snap, Postcard, Sky and Legend & keys (#901, #938, #946). Gone: the desktop
  minimap (#883), the zoom readout (#875), the Farm/Ledger toggle (#884; the Ledger is the no-script page), the plan modal
  (#886; the bead view is the one plan view), ride-along (#881), drag-to-adopt (#869), the Commons band's grip and fold
  (#880), the extra cursor key aliases (#863) and dead `#tip` handlers (#870). The Porch and Mailbox are two buttons (#888),
  and a gardener click opens its thread (#865).
- **4, postcards (gd-BrMFV0JbHL).** The Menu's Postcard sends the plot at the cursor to its plan's thread (#963; farm read
  model, Postcards). The time-lapse scrubber was cut from Phase 4 and parked in gd-mnDfHWSfHR. Its design is
  `docs/superpowers/specs/2026-10-23-farm-history-timelapse-design.md`.
- **Stable cells (gd-wDdun2AVwq).** A bed plants its listed tasks and listed closed steps together by created, then id
  (`world.js` `plantOrder`; the farm sends `created` on both and lists a phase's tasks in that order), and the pen plants its
  goats oldest first, so a status change, a close or a new wish moves nothing already placed. Only a plant leaving the bed
  (a closed step falling off the listing) or a wish closing steps the later ones back one cell, as plots do (3.3). Before,
  the bed followed the farm's in-flight-first order and the pen newest first, so every status change or new wish reshuffled
  them.
- **Beads keep their cell once placed (gd-xhpccozIcC).** `WORLD.layout` takes a placement memo per plan, `{bed: {bead:
  cell}}`, which `garden.js` keeps per tab in `sessionStorage` (`garden.cells.v1`), so a new step (even one older than the
  planted ones), a close, a title edit or a reorder of the document moves nothing already in a bed, and a refresh keeps the
  cells too. A new bead takes the lowest free cell; a bead planted before and listed no longer holds its cell as a dim
  marker (`bed.gone`, drawn as stubble, no cursor stop) rather than closing the gap, and a full bed reclaims its lowest
  markers. A new tab repacks from the plant order above. Cursor-grid spec 5.4.
- **Every project gets its own yard (plan gd-LurZOR44M1, section 28).** The shared Yard, lattice cell 0 with one scene
  of every project's letters, growing beads and resting gardeners and every open wish's goat in the pen under it, is gone
  (#2250 and the steps before it; the dead scene code, `YARD_RECT` and `bayOf`'s sign-side road in gd-frrkHmoOd7). Each
  biome, the Commons included, has its own Mailbox, Escalations, Growing field, Barn and Pen, holding only its own
  project's items; the Commons is cell 0 and projects take cells 1 onwards. `Home` goes to the cursor's project's home,
  `at=<project>/home` names it, and `at=yard` lands on the Commons's home with a toast. The release truck stands in the
  bay under its project's Barn (28.10). Section 28 placed the home in a band at the top of the region; section 29 then
  moved it into the plot grid's centre slot as the farmstead, so 28 says what a home holds and 29 where it stands.
  Sections 3.1, 6's Yard row, 26.5, 26.10, 26.12 and 26.16's bay beside the sign now describe the old page. Cursor-grid
  spec 5.1 and 5.5.
- **The farm at the crossroads (plan gd-rLs7EoTa7a, section 29).** A project's home is no longer a strip in a band above
  the Orchard. It is the farmstead: spiral slot 0, the centre of its plot grid, its five cells the quarters of a
  courtyard round a lane cross, with the sign on its north edge (#2214). Plans take slots 1 onwards (34 a biome) and
  ring 1 starts east. Four 3-tile lanes frame the farm out to the region's edges, and the trucks, the gardeners and the
  goats use them (#2214, #2247). The region ring and the Growing field's and the Escalations' fences are gone, and the
  planted plots are hedged (#2229, #2260). The cursor's home cell is the grid's centre cell, its zones a 2 x 2 (#2216).
  Links, the outline, the phone and the snapshot frame the farmstead (#2271). The band code went with those steps:
  `BAND.minW` (#2214), `ROW.home` and the spanning home row (#2216), the ring bake and the field's fence (#2229), and in
  gd-CBK0GXrMx9 `shape()`'s home band, now its own `home` rect beside `bands`. A pan that starts on another cell's link
  (since #2271 the farmstead is at the board's centre) leaves the cursor where it was (gd-CBK0GXrMx9). Section 28's
  rects and widths (28.1, 28.6's home row, 28.8's fenced field, 28.12's and 28.13's band widths) now describe the old
  page; 28 still says what a home holds. Memo v5 (29.5).

## 28. Home strips: every project gets its own yard (plan gd-LurZOR44M1, design gd-UJeh0L7g2N)

The owner, in thread gd-tjddkquhPY: "we should move the yard to the meadow; and make it per project everywhere. the yard
is too far from everything now and it's weird". The Yard (sections 3.1, 26.5, 26.10, 26.12) is lattice cell 0 at the world
origin: one shared scene holding every project's letters, growing beads and resting gardeners, with every project's open
wishes in the pen under it. As projects fill the lattice, the Yard ends up a long way from the plots it is about. This
section is the design the plan's later steps build. Each decision is listed in 28.9 with the alternative it beat, so a
step reads it here and does not decide it again. Those steps merged by 2026-11-04 (section 27's entry); section 29
then moved the strip into the plot grid's centre. So wherever this section places the home in a band (28.1's rect and
cells, 28.6's home row, 28.12's and 28.13's widths), it records the page before section 29: 29 says where a home stands,
and 28 still says what it holds.

| Step | Builds | Reads |
|---|---|---|
| 2 gd-GYo4PWlGUb | `project` on every ledger row; per-project home counts in the farm document | 28.2, 28.3 |
| 3 gd-9DYilwsxub | `world.js`: the strip in `layout()`, the lattice, memo v4, ids, `resolve`; the bay rect in the Barn cell | 28.1 to 28.6, 28.10 |
| 4 gd-xnPZx5dSCT | `art.js`: `ART.drawHome`, the strip painted in its biome; the bay's ruts | 28.1, 28.2, 28.8, 28.10 |
| 5 gd-VFtLgEIM6f | `cursor.js`, `curnav.js`: the home cell, its zones, Home; the truck in the Barn zone | 28.5, 28.6, 28.10 |
| 6 gd-2vAMq1HHI9 | `garden.js`: the model per project, the Menu's Home, beacons | 28.2, 28.6, 28.8 |
| 7 gd-9bRQLfvrZQ | gardeners, the ride truck, birds and walks to their own barn; the release truck at the bay | 28.4, 28.10 |
| 8 gd-CcjzYeOXFB | the pen per project | 28.2 (Pen) |
| 9 gd-rKCiu4WGLe | `at=`, the jump list, the outline, postcards, words | 28.7 |
| 10 gd-cTKXpm8JH7 | the phone | 28.6, 28.7 |
| 11 gd-frrkHmoOd7 | the dead Yard code, the tests, section 27's entry and spec 5.5 | all of 28 |
| 12 gd-HEDhxsJ2JG | the check-in and its follow-ups | 28.9 (the rejected options are the follow-up candidates) |

### 28.1 The strip

Every biome, the Commons included, got a **home strip**: a band in its Meadow row, above the Orchard, in place of the
Meadow band. From top to bottom a biome was its sign, the home strip, the Orchard, the Graveyard, the plots, the
Nursery and the overflow crate. **Superseded by section 29:** the home is no longer a band. It is the farmstead in the
plot grid's centre slot (29.1), its cells the courtyard's quarters (29.2), and `shape()` returns it as `home`, beside the
`bands` (the Orchard, the Graveyard and the Nursery). The bullets below record the strip as it was built.

- **Rect.** `bands.home = {x: bx, y: homeY, w: bw, h: HOME.h}` at first: the band width every other band uses (`shape()`:
  the plots' width, at least `BAND.minW`, 63 since 28.13); since 28.12 it is the cells' union, at most 66 tiles, centred under the
  sign. `HOME.h` is 12 tiles, fixed. `shape()` places it where
  `bands.meadow` was (`orchardY - HOME.h`), and the sign sits above it as it sat above the Meadow. Every region is
  `HOME.h - BAND.meadowH` (5) tiles taller than it was. That is a one-time move, and every golden that shows the world
  re-shoots once, in step 3.
- **Cells.** Five cell rects in a row, west to east: the **Mailbox**, the **Escalations** (28.13), the **Growing field**,
  the **Barn** and the **Pen**, with a 1-tile path between neighbours. The Mailbox, the Escalations and the Pen are 10 tiles
  wide each, the Barn is `HOME.barn` (8, its bay under its keep), and the Growing field takes the band's rest clamped to
  [16, 24] tiles: 21 at the 63-tile minimum, 24 from a 66-tile band up. A band wider than the cells leaves grass, and the cells stay centred
  under the sign (28.12). Every cell is the strip's full height.
- **Fixed by shape, never by count.** The cell rects are pure in `(rows, cols, overflow, graves)`, the arguments
  `shape()` already takes. No count changes a strip. A new letter, a gardener coming home or a new wish never moves a
  plot or a neighbour. The counts decide only how many items a cell shows and its `+N`.
- **Capacity.** A cell holds what its rect fits at the Yard's pitches (`HOME.pitch` in `world.js`, once `ART.ZONES`, for letters, plants and
  figures; `PEN.dx` x `PEN.dy` for goats, inside a 1-tile fence). Past that, the cell's status line says `+N`, as the
  Yard's zones do now (`m.over`). Step 3 may tune the `HOME` constants against the art in step 4, but not the rules
  above.
- **Not in the strip.** The piles (`farm.piles`) have no project, and the page never drew them (section 3.3 placed a
  compost heap and an archive shed in the Yard, but neither was built). No strip gets a compost heap or a shed. Per-project
  piles are a follow-up for the check-in (step 12). The dog stays a picture: one per strip, by its mailbox, not a cursor
  stop.

### 28.2 What each cell holds

All four cells hold only their own project's items. The Commons strip holds the items that have no project.

| Cell | Holds | Source | Order |
|---|---|---|---|
| Mailbox | open decisions, the letters ("mine" scope filtered as `readSec` does now); escalations are the Escalations cell's (28.13) | the ledger's `decision` section, by `Item.Project` | `place()`'s sticky slots |
| Growing field | every running gardener of the project, solid, each beside a plant for its bead (28.12); then the project's **loose** beads: its Meadow rows (any open state), plus any in-progress ledger `work` row the farm document does not place on a plot, in at most half the field's slots | the farm document's `gardeners[]` through `gardenerState`; its `meadow.shown`, and the ledger's `work` section | the stands first, in need order; the loose beads in sticky slots |
| Barn | its picture, its sign and its release truck at the bay (28.10); no figures. `barnWords` is still the account of every running agent (28.12) | the farm document's `gardeners[]` for the words; its `releases` blob through `TRUCK` | the truck, its only item |
| Pen | the project's open wishes, one goat each | the farm document's `wishes.open`, plus `open_more[key]` for the `+N` | oldest first (gd-wDdun2AVwq) |

- **A loose bead is drawn once.** This rule is about the loose beads only: since 28.12 a gardener's plant in the field
  stands beside its figure even when its bead is also a cell on a plot. An in-progress bead on a plan's plot is drawn on
  that plot only, never also as a loose bead. A ledger `work` row whose id
  is a task cell in `world.index` is skipped by the Growing field. This closes open question 2 of section 10: the Yard's
  field used to show plan steps that were already plants on their plots. A Meadow row and a ledger row with the same id
  are one plant. The plant is drawn as growing while it is in progress, and as a wildflower (the Meadow's sprite) while it
  is merely open. The Meadow's cap (40) and its hashed spots (`hashCell`) go: the field uses sticky slots and its
  capacity.
- **Known limit.** A plan step past its phase's 60-task listing is placed nowhere by the farm document. If it is in
  progress, the ledger's `work` row puts it in the Growing field as well as in the phase's `+N` crate. This is rare,
  because phases rarely pass 60 listed tasks. Telling it apart would need a parent join on the ledger row, which step 2
  does not add.
- **Placement is pure.** Today `place()` in `garden.js` assigns the Yard's slots. It moves into `world.js` as
  `WORLD.homeFill(world, rows, slots)`, keyed `slots[project][zone]`. The page keeps the `slots` object and reads the
  ledger. `homeFill` adds each placed letter, plant, figure and goat to `world.index` with its cell rect and `project`, so
  `resolve`, the cursor and the outline find them. The page calls it after each `layout()` and after each ledger patch.
  A ledger patch needs no new `layout()`, because the strip's rects do not depend on counts (28.1).
- **The Pen.** `penOf(wishes, key, cell)` fills one project's Pen cell from that project's `wishes.open` and
  `open_more[key]`, planting the goats oldest first as now, so a new wish moves no goat. A goat out grazing by its plan
  keeps its cell with the gate open (26.10). Nothing laps a strip. The graveyard stays where it is, past the Orchard,
  since it is already per project.
- **The Barn and stewards.** A steward still lives at its land's homestead, the project's sign
  (`homestead-agents-as-people.md` section 9.2). It is never in a barn.

### 28.3 Which biomes exist; the Commons

- **A project with only home items is a biome.** That means open letters, loose growing beads, gardeners whose home it is,
  or open wishes, with no plan, seed, closed plan or headstone. Such a project gets a sign and a strip and no plots, as a
  biome with only seeds has always been a biome (section 4). `layout()` reads which projects have home items from the
  farm document's gardeners and wishes, and from the per-project home counts step 2 adds to the document (open letters,
  growing): the document's `home` blob (`homestead-farm-read-model.md`, Home). The ledger is never an input to
  `layout()`.
- **The Commons is cell 0.** It is the biome for the empty project key. It is built whenever it has anything: plans,
  seeds, loose beads, closed plans, headstones, or home items with no project. On a live farm that is nearly always, so
  the farm-wide strip is at the origin.
- **An empty Commons takes no room.** If the Commons has nothing, cell 0 keeps its index but takes no width (section 26.1's
  rule for an empty cell). A world with one project then has no hole at the origin.
- **A home item whose project has no biome** goes to the Commons strip, until the next `layout()` builds that project's
  biome. This happens to a gardener whose project's biome has not been laid out yet, or to a letter that arrived on the
  ledger before the farm document counted it. So nothing is ever left with nowhere to stand.

### 28.4 Gardeners: `homeFor`, `gardenerHome`, `gardenerSpots`

- **`homeFor(g)`** now names the biome whose strip is the gardener's home, not where it stands. It returns
  `"project:<key>"` for a gardener with a project. It returns `"project:"`, the Commons, for a gardener with no project and
  for every **thread agent** (`isThread`), whatever its project label. A thread agent serves a thread in the Commons, and
  its words already say "In the Commons: …". Callers fall back to `"project:"` when `world.index` has no such biome (28.3).
  The return value `"yard"` is gone.
- **Stewards** are told apart by `isSteward(g)`, not by `homeFor`. `gardenerSpots` and `stewardState` test `isSteward`
  where they now test `land !== "yard"`.
- **`gardenerSpots`** gives a gardener at home `{home: "project:<key>"}` (it was `{home: true}`). `out.home` becomes
  `out.home[biomeId]`, each in need order. (A ghost list, keyed the same way, put each gardener out on a plot into its own
  project's barn at half alpha; plan gd-ryynhxINtp removes it, and every running gardener stands in its project's Growing
  field instead: 28.12.)
- **`gardenerHome(st, mine, base, cap, keep, biomeId)`** takes the barn it fills. The cap is that Barn cell's capacity
  (28.1). Plan gd-ryynhxINtp replaces it with `gardenerField`, which fills the Growing field (28.12).
- **Walks, the ride truck and birds** (step 7) use the gardener's own barn. The truck leaves from that barn's door and
  comes back to it. The finished walk ends there with its tldr label. The burst's fly-back aims there. Birds (done,
  gd-CykqEHeEpX) lift from the gardener's figure in its strip's Barn cell, up off its head, under a per-barn cap of 8.
- **The release truck's bay** (26.16) was placed beside the sign "on the Yard's side". With no Yard it moves into the
  strip, at the project's barn: 28.10.

### 28.5 The lattice and memo v4

- **Cell indices.** `COMMONS_K = 0` and `FIRST_K = 1`. Projects take cells 1 onwards, oldest first, by the same cold
  order as section 16 and spec 5.1. Cell 0's centre stays the world origin, and is now the Commons's region, not a scene.
  `YARD_RECT`, `world.yard`, `world.pen` and `penOf`'s anchor south of the Yard are removed. `taken[0] = "yard"` goes.
- **`bbox`** is the union of the biomes' feet. The Yard scene's rect and the pen are no longer added. An empty world keeps
  `domainRect(0, 0)`, so the camera has something to frame.
- **Memo v4:** `{v: 4, cells: {key: {k, t}}, slots: {…}}`, with `k >= FIRST_K` (1). `cloneMemo` reads v1, v2 and v3
  memos by keeping their `slots` and dropping their `cells`. So the first load after the move re-packs the projects into
  the lattice once, oldest first from cell 1, and every plot keeps its slot inside its biome. This is the same one-time
  re-pack the v1 to v2 and v2 to v3 moves made. The `localStorage` key stays `garden.world.v1`, since the version is
  inside the value.
- **`CURSOR.tree`'s fallback** for a biome with no memo'd `cell.k` follows the same numbers: the Commons is 0, and others
  are numbered from 1.
- **The stored cursor** (`garden.cursor.v1`) may hold a path that starts at `yard`. That path no longer resolves, so the
  opening cursor falls through to its next rule: depth 0 on the first project (cell 1 onwards), else the Commons.

### 28.6 Cursor, Home and the Menu

- **The home cell** (superseded by 29.6: the home cell is now the plot grid's centre cell, `ROW.home` is gone and the band
  rows renumber from 0). In a biome's grid, `ROW.home = 0` replaced `ROW.meadow`. The Orchard and the Graveyard keep rows 1
  and 2. The home cell is a band cell, so it spans the grid's width and is reached by Up and Down (26.1). The cursor
  stops on it in **every** biome, even when all four zones are empty, because Home must always have somewhere to land.
  Enter on an empty strip bumps, and its status line says so ("No letters · nobody in the field · nothing growing").
- **Zones.** The home cell descends into a 4 x 1 grid: Mailbox, Growing field, Barn, Pen. A zone with no items is not a
  cursor stop (N-D3). Each zone descends into its items in `homeFill` slot order, as the Yard's zones did. The pen's grid
  is as wide as its cell holds goats. Up from the top plot row moves to the nearest band above that holds anything. That
  is the home cell when the Orchard and the Graveyard are empty; otherwise a second Up reaches it.
- **Status lines** (`CG §6.4`). The home cell reads "3 letters · 4 working · 2 growing · 1 open wish" (the field's
  stands, 28.12, then its loose beads), with the need words first when a letter needs you. That is the same sentence step 2's counts give a project's sign. Each zone keeps
  its Yard wording for its own project ("3 decisions waiting", `barnWords`, "N open wishes · M out grazing"), except the
  Growing field's, which counts its stands and its loose beads: "4 working · 2 growing" (28.12).
- **Home key.** `Home` puts the cursor on the home cell of the project the cursor is in or on: `path[0]`'s biome, at any
  depth. With no project under the cursor, or with the cursor on the Commons, it goes to the Commons's home cell. A second
  `Home` from a project's home cell goes to the Commons's home cell, so two presses always reach the farm-wide strip.
- **The Menu.** The Menu's **Yard** item becomes **Home**, with the `Home` key hint. Its `aria-label` is "Cursor to your
  project's home". It does what the key does (`cursorAct({act: 'home'})`). `nav.to('yard')` and `homeCam()` go.
- **A ledger patch** rebuilds only the home subtrees of the biomes it touches. It replaces the Yard-only rebuild
  (`CURNAV` `yard()`, `t.yardRect`, `t.pen`).
- **Panes.** The Porch and the Mailbox panes stay whole-farm. Opened from a strip, they scroll to that project's group.
  A letter's beacon (`BEACONS.targets`) and a tour stop for an ask aim at its project's Mailbox cell, not `yard:decision`.

### 28.7 Ids and `at=`

| Thing | Node and index id | `at=` written |
|---|---|---|
| a home strip | `home:<key>` (`home:` for the Commons, as the Commons biome is `project:`) | `<project>/home`, `~/home` |
| a zone | `home:<key>/mailbox`, `/growing`, `/barn`, `/pen` | `<project>/mailbox`, `/growing`, `/barn`, `/pen` |
| a letter, a growing bead | its bead id | `<project>/<bead id>` |
| a gardener at home | its gardener id | `<project>/<gardener id>` |
| a gardener's field figure (28.12) | `field:<gardener id>` | `<project>/growing`, the cursor on `field:<gardener id>` |
| a goat | `wish:<id>`, as now | `<project>/wish%3A<id>` |

- **`parseAt` and `resolve`** read the forms above. `home`, `mailbox`, `growing`, `barn` and `pen` are reserved words
  in the id segment. No bead id can be one, because a bead id always has a prefix and a dash. `NATURAL` gains
  `home: 1` and `zone: 2`, and keeps `pen: 2` and `goat: 3`.
- **Legacy links** resolve, and the page shows a toast saying where they went. `at=yard` resolves to the Commons's home
  strip. `at=yard/pen` resolves to the Commons's pen when there is one, else to the Commons's strip. `at=yard/wish:<id>`
  and `at=yard/<bead id>` resolve to that thing in whatever project holds it, else to the Commons's strip. No link writes
  `yard` again.
- **`locate`** no longer falls back to `world.yard`. The strip is inside its biome's foot, so the biome answers.
- **The jump list** (`searchIndex`, `search`). The single "The Yard" row and the single pen row become one **home** row
  per biome: kind `home`, id `home:<key>`, title "<Project> home", `aka` "yard mailbox barn letters", and `note` the
  home cell's status. Each home row is ranked after its project. Each project with open wishes keeps a pen row (kind
  `pen`, id `home:<key>/pen`). In `KIND_RANK`, `yard` goes, `home` follows `biome`, and `pen` follows `home`. The empty
  query lists the projects and their home rows. Typing "yard" still finds every home strip.
- **The outline** (`WORLD.outline`, `garden.js` `buildOutline`). The Yard branch and the Gardeners group become a home
  branch at the top of each biome: its zones, each with its items.
- The plan's step texts name `jumpEntries`, `placeAt` and `outlineOf`. In the code these are `searchIndex`, `locate` and
  `outline`/`buildOutline`.
- **As built (step 9, gd-rKCiu4WGLe).** A legacy link's toast says where it went ("The Yard is gone: every project has its
  own home now. Showing the Commons home."), and the page rewrites the address to the new form (`~/home`) at once, not on
  the next move. The outline's farm-wide Gardeners root is gone: `WORLD.gardenerOutline` returns one Gardeners group per
  project, keyed by its home cell's id, and `CURNAV.outline(tree, extra)` lists each group after that home cell's zones
  (a gardener belongs to its plot's project, else to its land's or its barn's). `VIEWCUT.want` counts a gardener at home from
  its own project's Barn cell, not from the old Yard rect at the origin. A snapshot taken over the wild is named
  `farm-<land>-map-<time>.png`. Step 7's escalation executor stands, already in each strip's Escalations cell, are renamed off the Yard:
  `WORLD.escStands`, `gardenerState(...).esc`, ids `xs:<gardener id>`, links of kind `gesc`.

### 28.8 Drawing

- **`ART.drawHome(ctx, strip, model, pal)`** paints one strip inside its rect, at its biome's palette and seed. It draws
  the barn, the mailbox and its dog, the fenced Growing field with its plants, and the pen's fence. The Yard's sprites
  are kept. Only where they are placed changes: `zoneRect` and `slotPos` take the strip's cell rects instead of fixed
  Yard pixels. Night and reduced motion follow the biome rules.
- **Level of detail** (section 6's Yard row, per biome). At World, nothing is drawn. The biome blob and its sign carry the
  counts, and `!N` carries the need. At Project, the strip's ground, the barn, the mailbox with a letter flag when it has
  any, and the pen's fence are drawn. At Plan, the letters, plants, figures and goats are added, still. At Bead, the 6 fps
  loop runs, as the Yard's did.
- **Caching.** A strip is baked with its biome's chunks, or into a per-biome bitmap. It is never one shared `yardCv`.
- **As built (step 4).** The base (the paths between the cells, the field's soil and fence, the barn, the mailbox and its
  flag, the bay's ruts) is baked with the biome's chunks, and `scene.js` drops a strip's chunks when its flag or ruts change. At
  Project it is drawn flat, a few fills and two sprite blits per strip. The dog, the letters, plants and figures are drawn each
  frame from Plan in, still at Plan. The Pen's fence stays `GOATS`'s. The Yard's blank name plates are not drawn: a strip's
  zones are named by the cursor's status lines.
- **Keeps.** `HOME.keep` names the tiles at the north-west corner of the Mailbox (4 x 3) and of the Barn (7 x 5) that the
  mailbox with its dog and the barn stand on. `slotsOf` skips them as it skips the bay, so the Mailbox holds 50 letters.
  The Barn holds no figures (28.12): its keep and its bay fill it.

### 28.9 Decisions, with the alternative

| # | Decision | Why | Rejected |
|---|---|---|---|
| H-D1 | A home strip in each biome's Meadow row, the Commons included (28.1) | The owner asked for it: the yard moves to the meadow, per project | One shared Yard, moved nearer the projects: it is still far from all but one |
| H-D2 | The strip replaces the Meadow band, and its Growing field holds the loose beads (28.2) | One band, not two, for the same beads (N-D0: space first); a bead is drawn once | A strip beside a Meadow band: a loose in-progress bead would be drawn twice |
| H-D3 | Cells west to east: Mailbox, Growing field, Barn, Pen | What needs you first in reading order; the pen beside the barn (step 8); the field, the widest cell, in the middle | The Yard's order (barn top-left): it puts the gardeners before the letters |
| H-D4 | Strip and cell rects fixed by `shape()`'s inputs, never by counts (28.1). Superseded for the courtyard's cells by X-D13 (29.10), and for the farmstead's width by X-D14 (29.11): the width `W` is now one of `shape()`'s inputs, computed from the cells' widths in `layout()`'s step 1. A count reaches a plot or a neighbour only through `W`, in 4-tile steps, east of the farm and never vertically. The second and third reasons still hold | A letter, a gardener or a wish never moves a plot; the region is still sized before it is placed; a ledger patch needs no `layout()` | Rects sized from counts (the plan's step 3 wording): every letter or new goat would move the biome's plots and its neighbours, as the Yard's pen grew cell 0 (26.10) |
| H-D5 | The cursor always stops on the home cell; an empty zone is not a stop (28.6) | Home always lands somewhere; arrows still skip empties | A strip that is a stop only when it holds something: Home would have nowhere to go in a quiet project |
| H-D6 | A project with only home items is a biome (28.3) | Its letters and gardeners do not move when its first plan lands | Its home items go to the Commons strip until it has a plan |
| H-D7 | The Commons is cell 0, projects from 1, and an empty Commons takes no room (28.5) | The farm-wide strip is at the origin, where Home and old links land; no gap at N=1 | The Commons kept at cell 1 with cell 0 empty: an empty first column on every farm |
| H-D8 | Memo v4 drops the v3 cells and re-packs once (28.5) | The move changes every region's pixels anyway (taller strip, no Yard scene), so nothing the eye remembers survives either way; the re-pack also converges browsers that drifted apart | Shift every v3 index down by one: keeps each browser's order, but carries its per-browser drift forward |
| H-D9 | `homeFor` names the home biome; `isSteward` decides the sign (28.4) | One question per function | Keep `homeFor` as "where it stands": it would need a third return shape for "at the barn of project X" |
| H-D10 | Thread agents live in the Commons's barn, whatever their project (28.4) | They serve Commons threads, and their words already say "In the Commons" | Their project's barn: a project's barn would fill with short-lived chat agents |
| H-D11 | Home goes to the cursor's project's strip, else the Commons's; a second Home goes to the Commons's (28.6) | The nearest strip is the useful one, and the farm-wide one is still one key away | Home always to the Commons: it brings back the trip the owner complained about |
| H-D12 | `at=<project>/home` and zone words; `at=yard` resolves to the Commons strip with a toast (28.7) | Readable links; old links still land and say where | `at=yard` an error: breaks links already posted in Slack |
| H-D13 | Home items placed by a pure `WORLD.homeFill` in `world.js` (28.2) | `resolve`, the cursor and the outline need them in `world.index`, and they are tested under node like the rest of `WORLD` | Keep `place()` in `garden.js`: resolve could not find a letter without the DOM |
| H-D14 | No compost heap or shed in any strip; per-project piles are a follow-up (28.1) | The piles have no project, and the page never drew them | A heap in every strip at farm-wide counts: the same number fifty times |
| H-D15 | The Porch and Mailbox panes stay whole-farm, opened scrolled to the project's group (28.6) | One place to answer everything; the strip is the way in | A pane per project: a follow-up for the check-in if the owner wants it |
| H-D16 | The release truck's bay is a fixed rect in the Barn cell, and the truck is the Barn zone's last item (28.10) | Releases are per project, so the truck belongs in the project's own yard; the barn is where crates are loaded and where the other truck already leaves from; no cell is added, so the strip's minimum width and the cursor's zones are unchanged | A fifth cell, the Bay, east of the Pen: 8 more tiles on every strip's minimum width (`BAND.minW` 52 to 60) for a truck most projects never have, or a Growing field 7 tiles wide at the minimum. The bay kept beside the sign (26.16): the truck would stand outside the yard the owner asked for, and the sign's row is the one place the strip does not reach |

### 28.10 The release truck at the barn (gd-j3bQqfZSt7, thread gd-qROlrb9kvX)

The owner, tending release gd-34XdyOrYP3: "we gotta make sure we integrate trucks with the per-project farm thing; since
release trucks are per project". Section 26.16 put each project's truck in a bay beside its sign, the one thing of the
project's that was not in its yard. With the strip, the truck moves into it.

- **The bay.** `bands.home.bay` is a 6 x 4 tile rect (the truck's box was `TRUCK.TW` x `TRUCK.TH` in tiles; since
  gd-Ir8jxl4c57 the 72 x 48 px truck stands centred on the bay's floor, and the bay keeps its size) in the
  south-east corner of the Barn cell: `{x: barn.x + barn.w - 6, y: barn.y + barn.h - 4, w: 6, h: 4}`. Like every cell
  rect it is fixed by `shape()`'s inputs (28.1): every strip has one, whether or not the project has a release. With
  the Barn cut to 8 tiles (28.12) the bay stands directly under the barn's keep, at the cell's west edge:
  `{x: barn.x, y: barn.y + HOME.keep.barn.h, w: 6, h: 4}`, and the Barn holds no figures. `TRUCK.bayOf(b)` reads `b.home.bay` and no longer places from the sign; `world.index` keeps
  `release:<tag>` (kind `truck`) on the bay's tiles, and `?at=<project>/release:<tag>` resolves as it does now.
- **Facing and the drive.** The truck faces east, towards the Pen, and `depart` drives it east along the strip's foot,
  past the Pen and off the biome's edge (`DRIVE`, `AWAY`, `BACK` unchanged). Under reduced motion it stands with its
  board, as now. The ride truck (the gardeners' pickup, `transitions.js`) leaves from the barn's door beside it; the two
  are told apart by colour (red, its own) and by the board, as section 26.16 says.
- **Drawn.** `ART.drawHome` paints two wheel ruts on the bay's ground whenever the project has any release (the
  `releases` blob names it), and nothing when it has none: an empty mark in every strip would be H-D14's same picture
  fifty times. The truck itself is drawn by `TRUCK.draw` after the strip, at the Plan and Bead levels only, as now. At
  Project the sign's release row (`TRUCK.line`) stands.
- **Cursor.** The truck is the Barn zone's only item (28.12), so the zone descends straight to it and Enter opens it
  (its tip, its link). With no truck the Barn zone is not a stop. The Barn zone's status line gains the truck's words at its end:
  `barnWords`, then "release 2026.305.3 loading", or "live on prod". The home cell's own line does not mention it.
- **No release source.** A project with no `release_tag_pattern` has a bay and no truck, no ruts, and no cursor item:
  nothing in the strip says "release" for it.
- **Steps.** Step 3 adds the rect and `bayOf`'s new read (one branch gone from `truck.js`); step 4 the ruts; step 5 the
  zone item and the status words; step 7 the facing and the drive; step 11 deletes the sign-side branch (`bayOf` returns
  the rect alone and every truck faces east) and the `truckload` and `truckgone` goldens re-shoot once, with the rest. The releases plan's step 10 (gd-HHMReWCHUi) grades
  the truck where it stands then.

### 28.11 Escalation kinds and their words (plan gd-PJB3D7glwF, step 2 gd-ftQFRlBDZI)

Every open escalation is drawn as its own icon, its sprite by family (plan gd-PJB3D7glwF), in its project's home strip,
on its plot and in its bead view. All three say it in one vocabulary, `web/esckind.js` (`ESCKIND`), a pure module that
reads one FarmWait (`homestead-farm-read-model.md`, Escalations) and draws nothing, so any later step can use it wherever
the icon lands. The farm names the family (`FarmWait.family`, from the one Go table in `farm_escalations.go`); the page
never maps an operation type itself.

| Family | Noun | Accessible name opens |
|---|---|---|
| `merge` | merge | Merge escalation |
| `deploy` | deploy | Deploy escalation |
| `config` | config edit | Config edit escalation |
| `prompt` | prompt install | Prompt install escalation |
| `beads` | beads admin | Beads admin escalation |
| `access` | access | Access escalation |
| `infra` | infra | Infra escalation |
| `read` | read-only query | Read-only query escalation |
| `other` | escalation | Escalation |

A family the page does not know reads as `other`. `TestEscKindUnderNode` holds `ESCKIND.FAMILIES` and the Go `Esc*`
constants to one set, so a new family cannot land on one side only: add it to both tables, this one, and the read
model's.

- **State words.** The word always says the state, so colour is never its only sign (section 14): `pending` is
  "waiting on you" when it waits on a human, else "waiting on" its `waits_on` ("the merge desk"); `approved` is
  "approved, running", or "approved, waiting on you" when it falls to a human (a manual action); `rejected` and `failed`
  are "rejected, still open" and "failed, still open".
- **Who.** "you" for a human wait, else `waits_on`, else nobody.
- **Accessible name.** "Merge escalation: <title>, waiting on you, since 2 h": the noun, the title (its id when it has
  none), the state word, "waits on <who>" when the word has not already said who, and the time since the current wait
  began (`since`, in min, h or d).
- **Order.** The Yard's: those waiting on you first, then those in hand (approved, or pending with the desk or the
  operators), then rejected and failed; newest first within a group, then id. This is not `escalations.open`'s order
  (longest wait first), which the farm keeps for its cap; a cell re-sorts with `ESCKIND.order`.
- **The cell's grid** (step 6 gd-4FIF5pzC6O). `ESCKIND.cells(list, {cols, id, total})` lays the escalations out one
  per cell in that order, `cols` wide (the cell's slot columns, `HOME.pitch.escalation`). Past `ESCKIND.CAP` (24) the
  first 23 keep their cells and the 24th is a "+N" crate, as a bed's is; N also counts what the farm counted but did not
  send (`total` is the project's own count, `escalations.by_project`), so none is hidden without a count. When the farm's
  120 cap cuts `open`, it keeps the longest waits, so the newest of a project can be in the crate's N rather than a cell.
  Since 29.10 there is no `ESCKIND.CAP`: `escOf` passes `cap`, the cell's slot count at its width, so the crate comes only
  once the cell is at its widest and full.
- **Where it came from.** The plan whose `waiting[]` lists it (`ESCKIND.lineage(plans)`, the farm's
  `requesting_plan`/`requesting_task` join), named by its short title (else its title, else its id), else its project,
  else nothing (an unprojected one).
- **Sign tip.** `ESCKIND.tip`: the title, then "Merge escalation, waiting on you, since 2 h, from plan <title>.", the
  Mailbox's tip shape in the accessible name's words.
- **In-place answer.** A cell offers `web/answer.js`'s Approve and Reject only for a `pending` escalation and only to
  an account that may approve (`body[data-esc]`): `ESCKIND.answerable`.
- **Sprites** (step 3 gd-UDx80F47JN, `web/art.js`). One 16 x 16 two-tone sprite per family, outlined like the other
  plants: merge two crates joined by a plank, deploy a wheelbarrow, config a hoe laid over a plan sheet, prompt a scroll,
  beads a string of beads, access a key, infra a water pump, read a spyglass, and other the chest it always was (an
  unknown family draws the chest). The state is drawn over the body, never colour alone: a raised flag while `pending`, a
  gear that turns by frame while `approved` (frame 0 under reduced motion), a crack down the body once `rejected` or
  `failed`. `ART.drawEscalation` draws it at a whole number of device px per sprite px with the plot's rim, in the
  Escalations cell (28.13) and on the escalation bead's own plot in place of the chest (FarmTask `family` and `wait`), so
  the plot and the cell agree.
- **Sign count.** A project's Yard sign says `ESCKIND.count(n)`, "3 escalations": the Home cell's and the Escalations
  zone's status. The World level draws no escalation, so its project board's head counts them ("7 plans · 1 needs you ·
  3 escalations").

### 28.12 The Growing field: its size, who stands in it, and the three-stop Enter (plan gd-ryynhxINtp, thread gd-VdxyebESAP)

The owner, changing plan gd-IlEcNCxdDi (decision gd-K2kVOIFRIi): "we should make the growing field not be too big, but
also not be totally dense with growing stuff so agents have a chance to stand somewhere. also, single click or 'enter'
should bring up glass + transcript, enter again should move to farm view with gardener highlighted and full zoom, enter
again should move back to yard". Until this plan, a gardener out on a plot left a ghost, a copy at 50% alpha, in its
project's Barn cell, and the Barn, mostly picture and bay, pushed those ghosts into its "+N" where nobody saw them. This
section makes the Growing field the project's board of who is working on what. It is the rule the plan's steps built;
all have merged, and the page draws no ghost.

| Step | Builds | Reads |
|---|---|---|
| 1 gd-abGCvA4yzt | this section; `homestead-agents-as-people.md` 4.1, 4.4, 4.5, 6.1, 6.2 and 12.3 | all of it |
| 2 gd-D701rKJoyr | `world.js`: the field cut to size, the pair slots, the loose beads' half | Size, Density |
| 3 gd-GPu096omMw | the Glass's transcript section and its "Go to its plot" button | The three stops |
| 4 gd-mdNpKAyEVw | `world.js`: a field stand for every running gardener; the ghost list goes (after gd-9bRQLfvrZQ) | Who, Ids, Density |
| 5 gd-PeLoWLR7Lq | `art.js`: the pairs drawn; the Barn draws its picture, truck and sign only | Who, Density |
| 6 gd-g2t34ZIUKh | one key, three stops: the links, the cursor, Enter in the Glass | The three stops, Ids |
| 7 gd-xx7tjsBkXO | words, counts, the outline and the jump board know the stands | Who, Ids |
| 8 gd-rbALUvumqV | walks, the ride truck and the birds start from the field stand | Who |
| 9 gd-a628FpJRO5 | the phone and acceptance | all |
| 10 gd-ooLMrx9FNi | the dead ghost code, the docs settled, the deploy | all |

**Size.**

- **The Growing cell** (since 29.2 the courtyard's fixed 16 x 16 quarter) took the band's rest, the band width less the Mailbox, the Barn, the Pen and the three paths
  (`bw - 31`), clamped to [16, 24] tiles. `HOME.cells`' growing entry gains that minimum and maximum. At the 52-tile
  `BAND.minW` the field is 21 tiles; from a 55-tile band up it is 24. The minimum cannot bite at today's constants; it
  keeps a field a figure can stand in if a fixed cell ever grows.
- **The Barn cell** narrows from 14 tiles to 8 (`HOME.barn`). Its keep (7 x 5, the barn picture) stays at its north-west
  corner, and its bay (6 x 4, 28.10) is stacked directly under the keep, at the cell's west edge. It holds no figures.
- **The strip** (since 29.2 the courtyard) was the four cells and their paths, at most 10 + 24 + 8 + 10 + 3 = 55 tiles, centred under the sign
  (the band's centre, `cx`). A band wider than that leaves grass either side, east of the Pen and west of the Mailbox
  (the odd tile east): the biome's ground, with nothing on it and no cursor stop. `homeStrip` stays pure in its
  arguments (H-D4), so no count widens or moves the field. The cells move once with step 2, so the 10x golden's digest
  re-generates there and the next bless on main re-shoots every browser golden that shows a strip; no region changes
  size.

**Density.**

- **Pair slots.** The field's slots are pairs, each 2 tiles wide on a 2-tile pitch, inside the field's 1-tile margin: a
  plant on the slot's west tile and a figure on its east tile, facing it. Since gd-7UDNpFoHWw (the owner on gd-riac8SR62O:
  "see how dense you can make it without occlusion (mostly)") the rows are 32 px apart, the closest at which a figure's
  task arms, drawn on its hat, clear the feet of the row above (a "?" or "!" bubble may overlap them; at 28 px the arms
  covered those feet), so the 16-tile cell holds 7 rows: a field of `w` tiles holds `floor((w - 2) / 2)` columns by 7
  rows, 21 slots at 8 tiles, 49 at 16, 70 at 22.
- **The stands go first**, in need order: down, then asking, then working, then resting (4.3's order; ties to the
  earliest `since`, then the id). Each keeps its slot by id while it is in the field (sticky slots), so a newcomer moves
  nobody.
- **The loose beads follow**, the `growingBead` rows (growing while in progress, a beetle on a bug, a wildflower when
  merely open), in sticky slots by id as now, **two to a slot**: a loose bead is one tile, on its slot's west or east tile
  (gd-7UDNpFoHWw). A Meadow flower a running gardener off a plot is out at takes a whole slot, so that gardener stands on
  its east tile (`standBeside`). They take at most half the field's slots (`MEADOW_SHARE` 0.5, so `floor(slots / 2)`
  slots, twice as many beads) and never a slot a stand holds; the field is sized for `max(stands + ceil(loose / 2),
  2 * ceil(loose / 2))` slots, a tended flower counted twice. The rest of the field is open ground, so a gardener that arrives
  always has somewhere to stand. A loose bead that is a stand's task is drawn once, as that stand's plant.
- **One "+N".** When the stands or the loose beads do not all fit, the field's last slot is a "+N" badge (the barn's
  badge, 4.4, moved), and N counts both: the stands past the field's other slots and the loose beads past their half.
  A crash or an ask is never the one behind it, because the stands are placed in need order.

**Who.**

- **Every running gardener of a project stands in its project's field, solid.** Its project is its home, `homeFor(g)`
  (28.4): a thread agent's is the Commons. A gardener out on a plot (or beside a band item or a goat, agents-as-people
  4.1) is drawn in two places, both solid, both in its pose: on the plot of its task, and in the field beside the plant
  for its bead. A gardener at home with a task stands beside its task's plant. One with no task (idle, dormant, a thread
  agent, whose thread head is not drawn as a plant) stands in the field with no plant. A steward still stands at its
  homestead, the sign, and is never in the field (28.2).
- **The Barn cell** draws only its picture, the release truck at its bay and its sign. The truck is the Barn zone's only
  cursor item (28.10). `barnWords` and `barnCount` stay the account of every running agent, on the barn's card and the
  Barn zone's status line, so the operator still reads how many are running, down or asking in one place.
- **"A bead is drawn once"** (28.2) is narrowed to the loose beads: a gardener's plant stands beside its figure even
  when its bead is a cell on a plot too.
- **Walks, the ride truck and the birds** start from the field stand (step 8): a walk to a plot leaves the stand, a
  finished walk's tldr label lands on it, and the ride truck still leaves from the Barn's door and picks the gardener up
  at its stand.

**Ids.**

- A field figure is `field:<gardener id>` (kind `stand`), in `world.index` with its slot rect and `project`. Its `at=`
  is `<project>/growing` with the cursor on `field:<gardener id>`, and `resolve` reads it back. The plot figure keeps the
  gardener's own id.
- `ghost:`, `WORLD.GHOST` and `ghostOf` go.

**The three stops.** One key walks the whole trip: yard, Glass, farm, and back to the yard.

| You are on | Click or Enter does | The page says |
|---|---|---|
| a field figure (`field:<gid>`) | opens its Glass (`benchOpen('glass', gid)`, the figure the opener) with its transcript under the terminal, above the header rows. A viewer below the operator tier (`body[data-glass]` not "1") gets the transcript panel alone (`?transcript=<id>`) | "Watching <agent> in the Glass; Enter again goes to its plot" |
| its Glass, the panel focused | Enter closes the Glass and flies to the gardener's plot at the map's full zoom (`cam.z` at its maximum), the cursor on its plot figure. The same act is the "Go to its plot" button in the panel's actions (`bv-acts`). The transcript panel alone carries the same button and the same Enter | "On <plot>, <agent>: Enter goes home to the yard" |
| its plot figure (the gardener's id) | comes back to the yard: `?at=<project>/growing`, the strip framed, the cursor on `field:<gid>` | the field figure's label |

- **Enter in the Glass** counts only when the event's target is the panel or the terminal's fit box: never an input, a
  textarea, the nudge box or an attached terminal, which `glasskey.js` claims. Escape still only closes the Workbench.
- **A resting gardener** (at home, no plot) has no plot to go to: its Glass's Enter bumps and says "<agent> is not on a
  plot", and its "Go to its plot" button is hidden.
- **The flight is a camera move**, never a walk: the figure is already on its plot. Under reduced motion the camera cuts.
- **The thread** leaves the figure's click. It stays one click away on the figure's card ("Thread in the Commons") and
  in the Porch row. The speech bubble still opens its Mailbox letter, and the "+N" badge its list.

| # | Decision | Why | Rejected |
|---|---|---|---|
| H-D17 | The Growing field is clamped to [16, 24] tiles; the strip is centred, grass either side | The owner: "not too big"; 24 tiles hold 33 pairs, and a sea of flowers no longer buries the figures | The band's whole rest (63 tiles in a 3 x 3 biome, mostly flowers); a fixed 24 (a field wider than a 52-tile band) |
| H-D18 | The Barn narrows to 8 tiles: picture, bay and sign only | It holds no figures now, so its width goes to the field; the bay under the keep keeps the truck in the yard (H-D16) | The Barn at 14 with an empty forecourt: 6 tiles of nothing beside the field that needs them |
| H-D19 | Pair slots, stands first, loose beads at most half, one "+N" | The owner: "not totally dense with growing stuff so agents have a chance to stand somewhere"; need-first placement keeps a crash or an ask in view | Stands and beads in one sticky order (a busy project's flowers crowd out a new gardener); separate badges (two counts for one field) |
| H-D20 | A running gardener stands solid in its field and on its plot; the ghost goes | Each place shows the whole gardener: the yard is the project's board of who works on what, the plot is where the work is | The ghost in the Barn (it fell into the "+N" of a cell that is mostly picture); only on the plot (the yard loses its board) |
| H-D21 | Click or Enter on a field figure opens its Glass with its transcript; Enter again goes to its plot; Enter there comes back to the yard | The owner's three stops, one key for the whole trip, in both directions | The thread on the figure's click (it stays on the card and in the Porch); Enter on the plot figure reopening the Glass (a two-stop loop that never comes home) |

### 28.13 The Escalations cell (plan gd-PJB3D7glwF, step 5 gd-sRsrH8j0PX)

The owner, in thread gd-qqILFtfc2H, asked for an area for escalations in place of the bin by the plot (26.8, retired). It is a fifth cell of every
home strip, between the Mailbox and the Growing field, and the Mailbox keeps decisions only, so one ask is never drawn twice.

- **Cells.** West to east: Mailbox, **Escalations**, Growing field, Barn, Pen. The Escalations cell is 10 tiles wide,
  the Mailbox's width, at the Mailbox's pitch (`HOME.pitch.escalation`): 7 x 8 slots, 56, past `ESCKIND.CAP` (24), so step 6's
  grid and its `+N` crate always fit (gd-4FIF5pzC6O's merge-desk note 2).
- **Width: `BAND.minW` 52 -> 63, not a narrower cell** (#2186; the choice the step 10 check-in confirms, gd-G1QSqrXGOU).
  Section 29 then moved the cells into the farmstead's courtyard and dropped `BAND.minW` (29.1): the Escalations cell is
  now the north-east quarter's inner cell, 7 x 15 tiles beside the Mailbox (29.2), and it has a post with the flag in
  place of a fence (29.4). The reasoning below is the record of the strip's choice. At 52 a 10-tile cell leaves the Growing field 4 tiles, and an 8-tile
  one leaves it 6: 4 plant columns, and a gardener sent to a loose bead finds no free tile beside it
  (`stand_test.js`, gd-6FCoWelnx2). So the band minimum grows by the cell and its path and the field keeps its 15 tiles. Only a
  biome one plot column wide grows (two columns are already 66); the strip stays pure in `shape()`'s inputs (28.1, H-D4).
  Rejected: an 8-tile cell at the 52 minimum (the cramped field above); H-D16's reason against a fifth cell (a truck most
  projects never have) does not hold here, since the cell answers asks.
- **With 28.12.** Built here at today's constants (the Barn 14, the field the band's rest: 15 at 63). Plan gd-ryynhxINtp's
  narrower Barn (8) and clamped field ([16, 24]) count this cell too: the field is the band less 42 tiles, 21 at the
  63-tile minimum, and the strip is at most 66 tiles. Under those constants a band of 58 would do; its step 2
  (gd-D701rKJoyr) may lower `BAND.minW` to 58 then.
- **Holds.** The farm document's `escalations.open` (`homestead-farm-read-model.md`, Escalations), by project key; the Commons
  holds the unprojected ones, and a project with only an escalation is a biome (28.3). `WORLD.escOf` places them in
  `layout()`, as `penOf` places goats, in `ESCKIND.order` (waiting on you first, newest first), one per slot, the rest in
  `more`. An item is `esc:<id>` (kind `escalation`, its FarmWait in `wait`), apart from the bead's own task cell on its plot.
- **Cursor and ids.** A zone like the others (title "Escalations", status "3 escalations · 1 needs you"); its item opens the
  bead, and the bead id finds the cell unless the bead is a plan's task. `at=<project>/escalation` and
  `at=<project>/esc%3A<id>`; `escalation` is a reserved id word (28.7). The home cell's needs count the escalations waiting on
  a human with the letters. The outline and the jump list list each escalation.
- **Words.** `WORLD.itemWords("escalation", w)`: "Merge escalation", "A merge escalation waits for a human to approve it. Open
  it to decide."; the cell's link carries them.
- **Beacons and N.** A strip's Escalations get one `!` arrow for those waiting on a human that no plan's `waiting[]` carries
  (a plan's are its plot's arrow); N walks each of them. The Mailbox's arrow counts decisions only.
- **Pane.** Opening the zone opens the Mail pane narrowed to escalations, with a "Show all" button.
- **Drawn.** `ART.drawHome` fences the cell like the field, raises a flag on its gate while one waits on a human (as the
  mailbox's flag is up with letters), and draws each escalation's step 3 sprite at its slot; an approved one's gear turns
  by frame, still under reduced motion (the page passes frame 0).
- **Step 6 (gd-4FIF5pzC6O).** `escOf` lays the cell out with `ESCKIND.cells`: `items` the escalations, `crate` the
  "+N" in the last slot past the cap (drawn as a crate with its number, a cursor stop `<zone>/more` that opens the Mail
  pane on every escalation, as the zone does), `total` the project's count. A cell's link is named
  `ESCKIND.name`, "from plan <title>" or "from project <key>" after it; hover and focus show `STIP`'s tip with
  `ESCKIND.tip`'s words, and for `ESCKIND.answerable` (pending, `body[data-esc]`, not read-only) the Mailbox's
  `ANSWER.box`, which keeps the tip as a pot's card does (Tab goes in, Escape comes back). An answered one says so in the
  tip and leaves the cell on the next farm document. Enter, Space and a click open its bead view.
- **Step 8 (gd-AmIpCiVTvY): keyboard and phone.** The zone's grid (`#hots`, a `role=grid` with one roving tab stop) is named
  for its cells' states, `"Escalations, 3 waiting on you, 2 running"` (then "waiting on others", "rejected or failed" and the
  crate's "N more", each only when it is not zero; `CURNAV` `escLabel`). Inside it the Home key goes to the first cell and End
  to the last; Home on the first goes on to the home cell as before, and the Menu's Home always does. Escape comes out to the
  zone. A phone reaches it with the pad as any zone (29.6), and since the pad moves no focus, no tip rises: the title bar over
  the pad reads the cursor escalation's tip line (`nav.bar`'s `tip`). There is no docked row between the map and the pad.
  `TestEscalationYardUnderChromium` walks it.
- **Step 7 (gd-B5aDmNhmjz).** An escalation's executor (a gardener with `escalation` and no task: farm escalation) has two
  stands. Its plot stand is unchanged (the escalation's step, else the barn), and the truck ride and the walk home start and
  end there. `WORLD.yardStands` adds a Yard stand beside its icon while the icon is drawn: feet on the icon's foot, right of
  its slot when that is clear of the cell's other icons and the crate, else on the cell's east margin column, which no slot
  reaches; facing the icon, one figure per spot, need first. It comes and goes with the icon and never walks. Its link
  (kind `gyard`) wears `STIP`'s tip with its name and `WORLD.gardenerCard`. The card, the Porch row and the outline row say
  `WORLD.runningWords`: "Running merge escalation: <title>", the family from the farm document's `escalations.open`.
  `TestEscalationExecutorUnderChromium` shoots it.

## 29. The farm at the crossroads (plan gd-rLs7EoTa7a, thread gd-vTL3AyeH5f)

The owner, in thread gd-vTL3AyeH5f: "id like the projects farm to be right in the middle of all the fields not way off in
the distance; yard and meadow should be with it, and trucks and goats and such as well, but less fencing maybe". Of the
three sketches posted in the thread (today, A the hub, C the crossroads) the owner answered "definitely C". Section 28 put
each project's yard in a home strip, a band at the top of its region, so at every zoom the farm still reads as off beyond
the orchard. This section moves the farmstead into the centre plot slot with the plans around it, runs four dirt lanes past
it to the region's edges, and cuts the fencing to the pen alone, with the plots hedged. It is the design the plan's later
steps build; each decision is in 29.9 with the alternative it beat, so a step reads it here and does not decide it again.
Those steps merged by 2026-11-04 (section 27's entry), so this section, not 28, says where a home stands. Everything section 28 says about what a cell holds (28.2),
which biomes exist (28.3), who stands where (28.4, 28.12), ids (28.7), the truck (28.10) and escalations (28.11, 28.13)
still holds; only where the cells are changes.

| Step | Builds | Reads |
|---|---|---|
| 1 gd-W8BVaquVcE | this section | all of it |
| 2 gd-eUlTiTqEkz | `world.js`: the farmstead at slot 0, its courtyard, the lanes, the sign, ring 1 rotated, `SLOT_CAP` 34, memo v5 (after gd-9bRQLfvrZQ merges) | 29.1 to 29.5, 29.8 |
| 3 gd-Hj4MLgugHv | `cursor.js`, `curnav.js`: home is the grid's centre cell, its zones a 2 x 2 | 29.6 |
| 4 gd-U44MrG7P1K | `art.js`, `scene.js`: the courtyard, the lanes, the region ring and the field's fence gone | 29.2, 29.3, 29.7 |
| 5 gd-BwSHVamI4j | hedged plots | 29.7 |
| 6 gd-tl9bVIBKTZ | trucks, gardeners and goats use the lanes | 29.3 (Who uses a lane) |
| 7 gd-vtkyDBES19 | links, the outline, the phone, the snapshot | 29.6 |
| 8 gd-CBK0GXrMx9 | the dead band code, the tests, section 27's entry, section 28's wording | all of 29 |
| 9 gd-mR6BIMOuAg | the check-in and its follow-ups | 29.9 (the rejected options are the follow-up candidates) |

### 29.1 The farmstead takes slot 0

- **Slot 0 is home.** In every biome, the Commons included, spiral slot 0, the plot grid's centre (column 3, row 2 of the
  7 x 5), is the **farmstead**: one 32 x 32 plot rect, `shape()`'s `home`, `slotRect(c, 0)` (one of its `bands` until gd-CBK0GXrMx9). No plan ever takes it. Plans take
  `SPIRAL[1..]`: the i-th packed plan (decision gd-VfOCGDpxg2, unchanged) is in slot i + 1.
- **Capacity 34.** `SLOT_CAP` is `SLOT.cols * SLOT.rows - 1`, 34. The 35th live plan goes to the overflow crate, as the
  36th does today.
- **Ring 1 starts east.** The spiral's first ring runs clockwise from the east: slot 1 is east of the farm, then
  south-east, south, south-west, west, north-west, north, north-east (slots 1 to 8). A one-plan project is its farm and its
  plot side by side. Rings 2 and 3 keep today's order (clockwise from their top-left, clipped to the grid).
- **The plot area is what is used, and at least two slots.** The plot area is the bounding box of the farm (slot 0), slot 1
  and every used slot, no longer the symmetric `2 * max + 1` rows and columns. So a project fills out from the farm without
  an empty slot west of it: 1 plan is 2 x 1 slots, 2 or 3 plans 2 x 2, 4 or 5 plans 3 x 2, 6 to 8 plans 3 x 3. Slot 1 is
  always inside it, whether or not a plan holds it: a farm-only biome (28.3) is two slots wide, its east slot open grass,
  and its first plan lands there without widening the region. `fit[key].grid` keeps its shape (`c0`, `r0`, `cols`, `rows`
  on the 7 x 5) and becomes the input `shape()` reads in place of `(rows, cols)`.
- **`BAND.minW` goes.** The bands above and below the plots (the Orchard, the Graveyard, the Nursery) are the plot area's
  width. At two slots and the lanes (29.3) that is at least 70 tiles, wider than the strip's 63-tile minimum (28.13), so
  the clamp has nothing left to do.
- **The bands stay bands.** The Orchard and the Graveyard stay above the plot area, and the Nursery and the overflow crate
  below it, by their section 3.3 and 26.11 rules (`BAND.orchardGap`, `graveGap`, `nurseryH`, `CAPS`). Pulling them in beside
  the lanes is a follow-up candidate for the check-in (step 9).
- **Fixed by shape, never by count** (H-D4 holds). The farmstead's rect and the lanes are pure in `shape()`'s inputs. No
  letter, gardener or wish moves a plot or a neighbour. The cells inside the farm were fixed too until 29.10, which sizes them
  from their counts inside this fixed rect (X-D13). Since 29.11 the farm's width follows its cells (X-D14): a count moves the
  plots east of the farm and the projects in the lattice columns east of its own, in 4-tile steps, and never vertically.

### 29.2 The courtyard

Inside the farmstead, a 1-tile **lane cross** (path tiles, the cross's arms meeting the farm's four edges at their middles)
splits the 32 x 32 into four **quarters**. With an even 32 and a 1-tile cross the halves cannot be equal: the cross's
north-south arm is the farm's column 15 and its east-west arm its row 15, so the west and north quarters are 15 tiles and
the east and south quarters 16. Three quarters are one cell each; the north-east quarter is the **asks**, the Escalations
cell (28.13) and the Mailbox side by side with a 1-tile path between, so the five cells of 28.13 are all here. Relative to
the farm's top-left `(fx, fy)`:

| Cell | Where | Rect | Holds (at 28's pitches) |
|---|---|---|---|
| Barn | north-west | `{fx, fy, 15, 15}` | the barn on its keep (7 x 5, the cell's north-west corner) and the bay under it (6 x 4, `{fx, fy + 5}`, 28.12); no figures |
| Escalations | north-east, inner | `{fx + 16, fy, 7, 15}` | the escalations and the "+N" crate at the Mailbox's pitch: 4 x 11, 44 less the sign's corner (28.13's 56), past `ESCKIND.CAP`'s 24, so the crate always fits; no fence |
| Mailbox | north-east, outer | `{fx + 24, fy, 8, 15}` | the mailbox and its dog on its keep (4 x 3, the cell's north-west corner as in 28.1) and the letters: 5 x 11 less the keep, 49 (28.1's 50) |
| Pen | south-west | `{fx, fy + 16, 15, 16}` | the goats inside the pen's fence: 4 x 7, 28 (28.1's 10) |
| Growing field | south-east | `{fx + 16, fy + 16, 16, 16}` | 7 x 5 pair slots, 35 (28.12's 33), with no fence; 7 x 7, 49, since rows went to 32 px (gd-7UDNpFoHWw) |

- **The Growing field is fixed at 16 x 16.** 28.12's clamp (`bw - 31` into [16, 24]) has no band to take the rest of:
  the cell is the courtyard's quarter. Its pair slots, the stands first in need order, the loose beads in at most
  `MEADOW_SHARE` of the slots and the one "+N" (28.12, H-D19) are unchanged.
- **The sign** stands on the farmstead's north edge: an 8 x 2 rect over the cross's north arm,
  `{x: cx - 4, y: fy, w: 8, h: 2}`, the farm's top two rows. It no longer has a row of its own above the bands. It covers
  the top of the Barn's and the Escalations' inner corners, clear of both keeps; `slotsOf` skips the sign as it skips a
  keep. The steward still lives at the sign (28.2).
- **The gates.** The cross's four arms end on the farm's edges, each beside a lane (29.3): those are the farm's gates. The
  bay's west side is the farm's west edge, so the truck pulls straight out onto the west lane.
- **Sized by their counts since 29.10.** The table above is today's widths (Escalations 7, Mailbox 8, Pen 15, field 16).
  29.10 moves the north arm, the asks' path, the south arm and the pen's fence with what each cell holds. The farm's rect, the
  east-west arm, the keeps, the bay and the sign stay where this section puts them.
- **Constants.** `HOME.h` and the strip's widths go; `HOME` names the five cells (the north-east quarter's split), the
  cross's tile and the keeps. Step 2 may tune the keeps and the pitches against the art in step 4, but not the rules above.

### 29.3 The lanes

- **Four lanes, 3 tiles wide.** The **west** and **east** lanes run north-south in the gaps either side of the centre
  column, from the foot's north edge to its south edge. The **north** and **south** lanes run west-east in the gaps either
  side of the centre row, from the foot's west edge to its east edge. They frame the farmstead, whose four corners are the
  crossroads. `s.lanes` lists their four rects, each with its name.
- **Every gap is a lane's width.** The plot pitch becomes 35 in both directions (`SLOT.dx`, `SLOT.dy` 34 to 35), so every
  gap between plots is 3 tiles and a lane is simply the gap it runs in. Gaps that hold no lane stay grass, between hedges.
  `slotRect` keeps its form, `c.x + s.c * SLOT.dx - SLOT.w / 2`.
- **The plot area includes the lane frame.** The 38 x 38 square of the farm and the four lanes beside it is always inside
  the plot area, even where no plot stands beyond a lane. So the smallest region (29.1) is 70 x 38 tiles of plot area:
  the west lane, the farm, the east lane and slot 1, the north and south lanes above and below.
- **Lanes cross the bands.** The west and east lanes run through the Orchard, the Graveyard, the Nursery and the overflow
  crate's row to the foot. No band item stands on a lane tile: the Orchard's tree cells, the Graveyard's stone cells and the
  Nursery's pot cells that meet a lane are skipped before items are placed, and `CAPS` are unchanged. The bands are still one
  rect each, and one cursor cell each.
- **Ground.** A lane is path tiles in the biome's palette, baked with the biome's chunks (29.7). It is not a cursor stop,
  not in `world.index` and has no id: it is ground, like grass.
- **Who uses a lane** (step 6). A vehicle, once out of its bay, drives on lane tiles only. The release truck (28.10) pulls
  west out of the bay onto the west lane, turns north to the north-west crossroads, and drives east along the north lane,
  past the sign and off the foot's east edge, then off the world (`DRIVE`, `AWAY` and `BACK` read the lane path instead of
  the strip's foot); it comes home the same way reversed. The ride truck (`transitions.js`) leaves the same bay the same
  way. A gardener walking from its field stand (28.12) to its plot leaves the farm by the nearest gate, walks the lanes to
  the gap beside its plot and turns in at the plot's gate column; it comes home the same way. A goat out grazing (26.10)
  stands on a lane's **verge**, the grass tile beside a lane, never on a lane tile. Under reduced motion nothing walks or
  drives: the figure is at its spot, as now.

### 29.4 Fences

There are five fence rings today. Three go, one stays, one becomes a hedge.

| Ring | Today | Now |
|---|---|---|
| the region's perimeter (`scene.js`'s `drawFenceClip` over `b.used`, and its 2 px outline at World) | a fence round every region | gone: the biome's ground colour already says where a project ends, and the lanes run out through where it stood |
| the Growing field's | a fence round the field | gone: the field is a quarter of the courtyard, beside the barn |
| the Escalations' (28.13) | a fence round the cell, its flag on the gate | gone: the cell is half of the asks quarter, beside the Mailbox. Its flag, up while one waits on a human, stands on a post at the cell's south-west corner, on the cross, as the mailbox's flag stands on the mailbox |
| the Pen's | `GOATS`'s fence and gate | kept: an open gate is how a goat says it is out grazing (26.10) |
| each plot's | `fenceH`/`fenceV` round a planted plot | a **hedge** (step 5): hedge sprites in the palette's dark green on the same ring. The rect is the same, so the operator fence colour band (`NB.fence`'s 6 px over the ring), the rollup plate and stamp, the "!N", the heads on the top edge and the gardeners' feet on the bottom row stay where they are. A staked plot keeps its surveyor's string. Night dims a hedge as it dims a fence |

### 29.5 Memo v5

- **Shape.** `{v: 5, cells: {key: {k, t}}, slots: {planId: {s, t}}}`, with `k >= FIRST_K` and `s >= 1`. The
  `localStorage` key stays `garden.world.v1`.
- **From v4.** `cloneMemo` keeps a v4 memo's `cells`, so every project keeps its lattice index, and keeps its `slots`
  shifted by one (`s + 1`), so the order the plans were packed in is kept and no remembered slot is the farm's. The first
  `layout()` then re-packs each biome's plans once, in that order, into slots 1 onwards, and writes the memo
  (`memoChanged`). The rank-1 plan moves from north-west of centre to east of the farm (ring 1's new order), and every
  later plan to its rank's new slot.
- **From v1 to v3.** As 28.5: their `cells` are dropped and their `slots` kept, shifted by one like v4's.
- **The stored cursor** (`garden.cursor.v1`) is a path of node ids, and no id changes (29.6), so it needs no migration:
  a path into a farm still resolves through `home:<key>`.

### 29.6 The cursor, ids and links

- **The home cell is the grid's centre cell.** `ROW.home` and the spanning home row go. In a biome's plot grid the home
  cell is slot 0's cell, reached by the arrows like any plot. It is still a stop in every biome even when all four zones are
  empty (H-D5), and Home still lands on it (H-D11). Up from the top plot row reaches the nearest band above that holds
  anything (the Graveyard, else the Orchard), and the band rows renumber from 0: `ROW.orchard` 0, `ROW.graveyard` 1, then
  the plots, the Nursery and the crate.
- **Its zones are a 2 x 2** matching the courtyard's quarters, the north-east one holding two: Barn, Escalations and
  Mailbox on the top row, Pen and Growing field below. An empty zone is not a stop (N-D3); each zone descends into its
  items in `homeFill` slot order (the Escalations in `escOf`'s, its crate last), as now. The arrows inside it move as the
  picture does: Right from the Barn is the Escalations, then the Mailbox; Down from the Barn is the Pen, and Down from the
  Escalations or the Mailbox the Growing field.
- **Unchanged.** The ids (`home:<key>`, `home:<key>/<zone>`, `esc:<id>`, `field:<gardener id>`), `homeFill`, `penOf`, `escOf`,
  `gardenerSpots`, the status lines ("3 letters · 4 working · 2 growing · 1 open wish"), `barnWords`, the Home key and its
  second press to the Commons, the Menu's Home, and the three-stop Enter (28.12). They read the rects, which is all that
  moves.
- **Links.** `at=<project>/home` and its zone words resolve to the farmstead cell and its courtyard cells; `at=yard` still
  lands on the Commons's farmstead with its toast (H-D12). The outline lists the home cell among the plots in grid order,
  not as a branch above them. The jump board and the pad's Go land on the farmstead. A postcard of a project and the
  `snap.js` crops frame the farmstead with its first ring of plots. The minimap draws the lanes as path-coloured lines.
- **As built (step 7, gd-vtkyDBES19).** `WORLD.outline` lists the farmstead among the plots by row, then west to east, its
  zones in the courtyard's reading order (Barn, Escalations, Mailbox, Pen, Growing field); the page's outline already read the
  cursor tree in grid order. The camera frames a project on its farmstead (`cursor.js` `frame`): the grid's bounds grow to be
  centred there, but never past the level of detail the bounds alone fit at, and then the camera moves off the farm only as
  far as keeps the bounds in view, so a narrow phone shows the farm as near the middle as its project allows. Depth 0 centres
  the lattice window on the cursor's project's farmstead. The jump board's and Go's home rows land on it through that framing.
  There is no minimap since 3b R11: the World level's biome blobs, the map's overview, draw the lanes in the path colour. The
  Menu's Postcard with the cursor in a project but on no plan sends the project's card: its farmstead and its ring 1 plots
  (`POSTCARD.farm`), to the project's #general. A snapshot is the whole view, so it needs no crop of its own; its context
  already names the project first, since the sign stands on the farmstead (29.2).
- **On a phone (step 10 of plan gd-LurZOR44M1, gd-cTKXpm8JH7).** The farmstead is a plot cell, so the pad's arrows reach
  it as they reach any plot; 28.6's "Up from the top plot row" no longer applies. Go's empty list leads with the home `Home` would go to:
  the cursor's project's, else the Commons's (`WORLD.search`'s `lead`). Under cursor-grid revision 3 the pad's In is a zoom
  step about the cursor's Thing; an In into the farmstead's zones, or into a zone's letters, escalations or goats, caps that
  step so the grid it entered (with the new Thing, a goat out grazing included) fits whole between the header, the camera
  column and the pad, centred (`fitHome` in `garden.js`). At 360 px that shows all of the farmstead's zones, where a doubled
  zoom from the Commons's tighter framing had shown only its Barn. Every map link already hits 44 px under a coarse pointer
  (`.hot::after`). Goldens, phone only: `gohome`, `homein`, `commonsin`, `homeletter`, `homegoat`.

### 29.7 Drawing

- **`ART.drawHome`** paints the courtyard inside the farmstead's rect: the cross in path tiles, the barn and its bay (with
  the ruts when the project has a release, 28.10) in the north-west, the escalations unfenced with their flag's post
  (29.4) and the mailbox, its dog and its flag in the north-east,
  the pen's ground in the south-west (its fence stays `GOATS`'s), and the Growing field's soil in the south-east, unfenced.
  The sign is drawn on the farm's north edge.
- **`scene.js`** stops baking the region ring, and bakes the lanes from `s.lanes` as path tiles in the biome's ground
  layer; `invalidateBiomes` drops them with the rest of a biome's chunks.
- **Level of detail** keeps 28.8's rules: at World nothing of the farm is drawn (the biome blob and its sign carry the
  counts); at Project the base only (the ground, the cross, the lanes, the barn, the mailbox and the escalations' post with
  their flags, the pen's fence,
  the hedges); the letters, plants, figures and goats from Plan in; the 6 fps loop at Bead.
- **Goldens.** Every golden that shows the world re-shoots once in the step that moves its pixels (steps 2, 4, 5 and 7, as
  each step's text says), never in a later sweep.

### 29.8 Sizes

| Plans | Plot grid (slots) | Plot area (tiles) |
|---|---|---|
| 0 (a farm-only biome) | 2 x 1, the east slot open | 70 x 38 |
| 1 | 2 x 1 | 70 x 38 |
| 2 or 3 | 2 x 2 | 70 x 70 |
| 4 or 5 | 3 x 2 | 102 x 70 |
| 6 to 8 | 3 x 3 | 102 x 102 |
| 34 (the cap) | 7 x 5 | 242 x 172 |

The region is the plot area, the bands above and below it, and the foot's 5-tile fringe, as 3.3 says. A one-plan region
is 70 x 53 tiles before its fringe, where today it is 52 x 62: wider by the second slot and the lanes, and 9 tiles shorter
(the sign's row and the strip, 15 rows, go; the lane frame adds 6). These are at `g = 0`: since 29.11 a grown farm adds
`g` (0 to 16) to each width and nothing to any height.

### 29.9 Decisions, with the alternative

| # | Decision | Why | Rejected |
|---|---|---|---|
| X-D1 | The farmstead is spiral slot 0, one 32 x 32 plot (since 29.11, 32 tall and 32 to 48 wide, X-D14), in every biome; plans take slots 1 onwards; capacity 34 (29.1) | The owner: "right in the middle of all the fields". The spiral already starts at the centre, so the farm sits where every project's plots grow from | B, the wide hub (the strip kept whole across the two centre slots, 66 x 32): centred only from three rows of plots up, and it dwarfs a one-plan project. The home strip kept as a band (28): the "off in the distance" the owner asked to end. Keeping 35 plots by growing the grid: the 7 x 5 is spec 5.2's, and a 35th plan already has the crate |
| X-D2 | The first plot is east of the farm: ring 1 runs clockwise from the east (29.1) | A one-plan project reads as a farm and its field side by side, on one row; the thread's sketch caveat named it | Ring 1 from the north-west, as the spiral has it today: the only plot sits up and to the left, a diagonal pair that reads as the farm in a corner again. Every ring rotated to start east: rings 2 and 3 only move projects with nine or more plans, and the approved plan names ring 1; a follow-up if the check-in finds the ninth plan's jump odd |
| X-D3 | The plot area is the box of the used slots with the farm and slot 1 always in it, so a region is at least two slots wide; `BAND.minW` goes (29.1) | A project grows out from the farm with no empty slot west of it; a farm-only biome already has room for its first plan, so that plan does not widen the region or move its lattice column's neighbours; the bands are never narrower than the strip's 63 tiles (28.13) | The symmetric `2 * max + 1` box: one plan makes a 3 x 1 with an empty slot west of the farm, two a 3 x 3 with six empty. A farm-only biome one slot wide (38 tiles with its lanes): bands narrower than today's 63 tiles, and its first plan widens it. `BAND.minW` kept: a clamp that can never bind |
| X-D4 | The courtyard is a 2 x 2 in a 1-tile cross: Barn north-west, the asks (Escalations and Mailbox, X-D12) north-east, Pen south-west, Growing field south-east; the west and north quarters 15 tiles, the east and south 16 (29.2) | Option C as drawn, which the owner picked; the busiest cell, the field, gets the largest quarter (35 pair slots, more than 28.12's 33); the bay on the farm's west edge puts the truck straight onto a lane; the asks keep the top row | A 2-tile cross, all four quarters 15 x 15: the cross reads as a fifth lane, and the field drops to 24 pairs. 28's five cells in a row inside the farm: strips about 6 tiles wide, the barn's keep filling its strip and the field 2 pairs a row. H-D3's reading order (Mailbox north-west, Barn north-east): as good on its own, but it redraws what the owner approved |
| X-D5 | Four lanes, 3 tiles wide, in the gaps either side of the centre column and the centre row, from foot edge to foot edge; the plot pitch is 35 everywhere, except that since 29.11 the farmstead's column is `32 + g` wide, so every slot east of the farm is `g` further east: one offset in `slotRect`, which nothing inverts (X-D14) (29.3) | Option C's lanes: the farm at the crossroads, with a road out on every side for the trucks, the gardeners and the goats' verges. One pitch keeps `slotRect`, the cursor's and the outline's pixel maths to one rule | Widening only the four lane gaps to 3 (two pitches in one grid, an offset per side in `slotRect` and everything that inverts it). A lane in every gap: a lattice of dirt, more ground built over than grass. 2-tile lanes in today's 2-tile gaps: no wider than the 1-tile cross, so not a road |
| X-D6 | The west and east lanes run through the bands; no band item stands on a lane tile; the bands stay one rect and one cursor cell each (29.3) | A lane that ends at the Orchard is a dead end, and the trucks would leave across the trees | Lanes that stop at the plot area (the dead end). Each band split in two around each lane: more rects and more cursor cells for the same trees |
| X-D7 | The sign stands on the farm's north edge over the cross's north arm (since 29.10 fixed at `fx + 12`, and since 29.11 always over the Barn's forecourt); the keeps stay at their cells' north-west corners, clear of it (29.2) | The name is on the farm it names, and the region loses the sign's own row | The sign kept in its row above the bands: it names a farm three bands away. The sign on the north lane: the truck's route east (29.3) would drive through it |
| X-D8 | The region ring, the Growing field's and the Escalations' fences go, the pen's fence stays, the plots are hedged with the operator fence band kept over them (29.4) | The owner: "less fencing maybe". Two of the four rings carried nothing; the pen's gate says a goat is out; a plot's ring carries the operator colour, the "!N" and the gardeners' heads and feet, so it changes material, not place | Dropping the plot rings too: the operator band, the "!N" and the feet would float on grass. Keeping the perimeter: the lanes cross it at every end. An open pen: a goat out grazing would look like a goat escaped |
| X-D9 | Memo v5 keeps v4's lattice cells and shifts every remembered slot up by one, then re-packs the plans once in that order (29.5) | Every plan keeps its rank and every project its place in the lattice, which is what the eye knows at World; v4's re-pack (H-D8) is days old, so browsers have not drifted apart since | Dropping v4's cells and re-packing the lattice as H-D8 did: moves every project at World for nothing. Keeping the slots unshifted: the same order, but a remembered slot 0 would say a plan stands on the farm |
| X-D10 | The home cell is the plot grid's centre cell, still a stop when empty (H-D5); its zones are a 2 x 2 matching the courtyard's quarters; `ROW.home` goes and the band rows renumber from 0 (29.6) | The arrows go where the eye does: Left from the plot east of the farm is the farm, Down from the Barn is the Pen | The home cell kept as a spanning band row while drawn at the centre: Up would leave the farm for a row where nothing is drawn. 28's 5 x 1 zone row inside a 2 x 2 courtyard: Right from the Growing field would land on the Barn, up and to the left |
| X-D11 | Vehicles drive lane tiles only, and the release truck leaves east along the north lane; gardeners walk the lanes and turn in at their plot's gate column; goats graze the verges (29.3) | The owner asked for the trucks and goats with the farm; the lanes are what makes the crossroads read as one place, and 28.10's eastward drive is kept | Walks and drives as today, ignoring the lanes: the lanes would be drawn and never used. Goats on the lanes: the truck's route would run through them |
| X-D12 | The Escalations cell (28.13) and the Mailbox share the north-east quarter: Escalations 7 tiles on the cross's side, a 1-tile path, the Mailbox 8 on the farm's edge (29.2) | The asks stay together, as 28.13 put them, at the Mailbox's pitch; each still holds what it held in the strip (44 escalations less the sign's corner against `ESCKIND.CAP`'s 24, 49 letters against 50); the other three quarters and their sizes are as the owner picked | The Escalations in the Barn's quarter (Barn 8, Escalations 6): fills the barn's empty forecourt, but splits the asks across the north arm and leaves 33 slots less the sign, close to the cap. A 3 x 2 courtyard of six cells: every cell narrower than its strip width and the field below 35 pairs. The escalations back in the Mailbox: 28.13 split them out so one ask is never drawn twice |
| X-D13 | Supersedes H-D4 for the courtyard's cells, and only for them: the farmstead stays slot 0's fixed 32 x 32 (the envelope is superseded by X-D14, 29.11: the farm grows instead, and the south half's slack rule goes), and inside it the Escalations, the Mailbox, the Pen and the Growing field are each sized from their own count in the farm document, in whole slot columns between a minimum and a maximum, with a 2-item hysteresis on shrink. The Barn takes what the asks leave in the north half. The east-west arm stays on row 15; the north arm, the asks' path, the south arm and the pen's east fence move. The south half's slack is grass (29.10) | The owner: "We should make the escalations cell and others get bigger and smaller depending on contents" (decision gd-ghNIZ2Z4N9). A fixed envelope keeps H-D4's first reason: no plot, lane, region or neighbour moves on a count. The Barn is the one cell with no count, so it absorbs the asks' slack. Splitting the north-south arm in two lets the asks and the south half size apart: a busy pen never narrows the Escalations. The hysteresis keeps one letter that arrives and leaves from flapping the farm | The farmstead itself growing and shrinking: slot 0 is one of 35 plots on X-D5's single 35-tile pitch. A bigger farm needs either a wider centre column and row (two pitches in `slotRect` and everything that inverts it, the option X-D5 rejected) or a bigger `SLOT` for every plot. Then every plot in the biome moves on a letter, the region's size changes, and so does its lattice column's width and row's height, so the projects beside and below it move at World. `biomeSig` changes and the biome's whole foot re-bakes. The camera's framing of a project jumps with it. That is the cost H-D4 was written to stop. A straight cross that moves as one: the Barn and the Pen share a width, so the Barn's empty forecourt could not take the asks' slack, and a busy pen would narrow the Escalations. The south slack given to the field: an empty field 50 pairs wide, the "too big" H-D17 refused. No hysteresis: a cell flaps at every column boundary. The previous widths kept in a memo v6: a version bump to store what any reload can recompute |
| X-D14 | Supersedes X-D13's fixed envelope: the farmstead is slot 0, 32 tall and `W` wide, with `W = 32 + 4 * ceil(max(0, max(24 + E + M, P + 1 + F) - 32) / 4)`. The Barn is at least 22 and the grass at least 0. The biome slides: everything east of the farm's base east edge moves `g = W - 32` tiles east, every rect across that edge is `g` wider, and nothing west of it or vertical moves. Regions are placed in their lattice cell by their base width, so only the columns east of a widened one move. On a re-layout the camera holds the farm corner of the biome in view still, and eases to the cursor's new framing when the camera is still the page's own framing (29.11) | The owner picked B, "Grow the whole farmstead instead (neighbours and the camera move on a count)" (decision gd-ArfegepUfk, shots gd-zOYdzVT7XZ). The Barn's empty forecourt stops absorbing the asks, and a busy pen no longer squeezes the field. One cut line puts each X-D13 cost in one place. The pitch has one offset, east of the farm, that nothing inverts. The region grows only in width, so no lattice row moves. Base placement keeps a grown farm's own column still. `biomeSig` already re-bakes the feet that move. The 4-tile step, a multiple of every band pitch, keeps the band items on their cells and lets a neighbour move at most four times as a farm fills. The farm's north-west corner is the one point in a biome that its own counts never move, so it is the camera's anchor | The prototype's module-wide `GROW` (fe30b20f1): `slotRect` and `shape()` impure, so any later call gets the wrong farm. A farm that also grows taller: every plot row and its lattice row's height move, and so do the projects below. A bigger `SLOT` for every plot: all 34 plots move on a count. Re-centring the grown region in its cell: its own west half and its whole column slide by `g / 2` on every step. Left-aligning every region: every project moves once at World for nothing base placement does not give. A 1-tile step (the shots' 34 and 35): 14 widths, so a neighbour moves for one letter at a column boundary. Rounding lattice columns to a coarser width: up to that much empty grass in every column, to halve the moves at best. Its own hysteresis on `W`: the cells' hysteresis already stops it flapping. The cursor's Thing as the camera anchor: a zone or a plot moves inside its biome on a count, so the whole farm would slide on the screen around it. Tweening the slide: the ground is baked in chunks. Keeping the south half's floor rule: it squeezes a pen that now has room. Centring the sign on the wider farm: the name would move on a count and walk onto the north arm |

### 29.10 Cells sized by what they hold (plan gd-PJB3D7glwF, step 11 gd-qClQvIyaj8)

The owner answered the escalation yard's check-in (decision gd-ghNIZ2Z4N9, thread gd-qqILFtfc2H): "We should make the
escalations cell and others get bigger and smaller depending on contents". Until now, every courtyard cell was a fixed rect
(29.2): the Escalations 7 x 15 whether it held none or forty, and `ESCKIND.CAP` put the 24th into a "+N" crate while the Barn's
forecourt beside it stood empty. This section overturns H-D4 (28.1) for the courtyard's cells only. X-D13 records the
choice and the alternatives it beat. Step 12 (gd-nOkEhUBDk4) builds it and decides nothing. Since 29.11 (X-D14) the farm
also grows with its cells, so the Barn, the envelope, the south half's floor rule and the sign below say where 29.11 changes
them.

**Scope: which cells flex.**

- **The Escalations, the Mailbox, the Pen and the Growing field** each take a width from their own count.
- **The Barn flexes, but only as the remainder.** It has no count and holds no items (28.12): its picture on its keep (7 x 5)
  and the bay under it (6 x 4) at the farm's north-west corner, the door, the truck and the ride truck's turn. It takes the
  width the asks leave in the north half, never less than **10 tiles**: its 7-tile keep and 3 tiles of forecourt, which is
  what the two asks' maxima (20 tiles together) leave. Its keep, its bay and its door never move, so the truck's way out west (29.3) is unchanged.
  Since 29.11 its minimum is **22**, the empty farm's width: the farm widens before the Barn narrows.
- **Heights do not flex.** The east-west arm stays on row 15, so the north cells stay 15 tall and the south cells 16. The bay,
  the west and east gates and the truck's route hang off that row. Width is where the slack is: an asks column holds 11 items,
  and the Barn's forecourt is the empty room.

**The envelope: the farmstead stays one 32 x 32 slot.** No count changes `slotRect(c, 0)`, `shape()`, the plot pitch, the lanes,
the region, the lattice or the memo. A new letter, escalation, goat or gardener never moves a plot or a neighbour, which
keeps H-D4's first reason. H-D4's second reason (a region is sized before it is placed) and third (a ledger patch needs no
`layout()`) also still hold; see "When it re-lays out" below. Only lines inside the farm move. The alternative, a farm that
grows, is costed in X-D13's rejected column. **Superseded by 29.11:** the farm is now `W` wide, 32 to 48, and the rects below
read `W` where they read 32 (`B = W - 2 - E - M`, the Mailbox at `W - M`, the field at `W - F`, the grass `W - 1 - P - F`).

**The courtyard's lines.** The 1-tile north-south arm splits in two at the east-west arm: a **north arm** between the Barn and
the Escalations, and a **south arm** between the grass and the Growing field. Each moves with its own half. The quarters keep
their corners and X-D4's reading order: Barn north-west, the asks north-east (Escalations inner, Mailbox outer, X-D12), Pen
south-west, Growing field south-east. With `E`, `M`, `P` and `F` the widths of the Escalations, the Mailbox, the Pen and the
Growing field, the rects off the farm's top-left `(fx, fy)` are:

| Cell or path | Rect |
|---|---|
| Barn | `{0, 0, B, 15}`, `B = 30 - E - M` (at least 10) |
| north arm | `{B, 0, 1, 15}` |
| Escalations | `{B + 1, 0, E, 15}` |
| asks' path | `{31 - M, 0, 1, 15}` |
| Mailbox | `{32 - M, 0, M, 15}`, its keep at its own north-west corner as now |
| east-west arm | `{0, 15, 32, 1}`, unchanged |
| Pen | `{0, 16, P, 16}` |
| grass | `{P, 16, 31 - P - F, 16}` when wider than 0: the biome's ground, no cell, no cursor stop, no id |
| south arm | `{31 - F, 16, 1, 16}` |
| Growing field | `{32 - F, 16, F, 16}` |

- **Today is one case.** E 7, M 8, P 15 and F 16 give 29.2's picture: Barn 15, both arms on column 15, no grass.
- **`home.cross`** becomes `{xn: B, xs: 31 - F, y: 15}` (it was `{x, y}`). The farm's gates are the arms' ends: west and east at
  row 15 as now, north at `xn`, south at `xs`. The two arms share a column only when `B = 31 - F`.
- **The sign stays put** at `{fx + 12, fy, 8, 2}`, 29.2's rect, which today is over the north arm. It no longer follows an
  arm. A count never moves the name, or the steward who stands at it (28.2). It meets the Barn's forecourt, the north arm or the
  Escalations' top row, and `slotsOf` skips it as now. It never meets a keep: the Barn's keep is columns 0 to 6, and the
  Mailbox starts at `32 - M`, column 23 at the widest. Since 29.11 it is still at `fx + 12`, now always over the Barn's
  forecourt (at least 22 wide), so it meets no Escalations slot.
- **Anchors.** The Mailbox is anchored on the farm's east edge, so it moves only when its own count changes its width. When the
  Escalations grow, they move the north arm and the Barn's east wall west. The Pen is anchored on the west edge and the field on
  the east edge, and the grass between them takes up the difference.

**The sizing rule.** `layout()` sizes the cells per biome from the farm document alone. `layout()` never reads the ledger
(28.3).

| Cell | Count `n` | Capacity per width (its pitch, less its keep and the sign) | Minimum | Maximum | Past the maximum |
|---|---|---|---|---|---|
| Escalations | `escOf`'s `total` (`escalations.by_project` folded to the key, at least `open`'s) | 11 a slot column: widths 3, 5, 6, 7, 8, 9, 10, 11 hold 1 to 8 columns, 11 to 88 less the sign's slots (80 at the widest, where the sign takes up to 8) | 3 tiles, one column | 11 tiles, eight columns | the crate in the last slot (28.11) |
| Mailbox | `home[key].letters` less the Escalations' `n`, at least 0 (the farm document counts escalations among the letters, 28.13 split them out) | widths 5, 6, 7, 8, 9 hold 18, 27, 38, 49, 60 (11 a column, less the keep's 2 rows over its first columns) | 5 tiles: the keep and a slot column | 9 tiles | the zone's "+N not shown", as now |
| Pen | `home[key].wishes` (listed and `open_more`) | `floor((P - 2) / 3)` columns of 7: widths 8, 11, 14, 17, 20, 23 hold 14 to 49 | 8 tiles, two columns | 23 tiles | `penOf`'s `more`, as now |
| Growing field | `max(s + l, 2 * l)`: `s` its stands, `l` = `home[key].field`, so the loose beads still fit in `MEADOW_SHARE` (H-D19) | 5 pairs a column: widths 8, 10, 12, ..., 22 hold 15 to 50 | 8 tiles, three columns | 22 tiles, ten columns | the badge in the last slot (28.12) |

- **Whole slot columns.** Each cell takes the narrowest width on its ladder whose capacity holds `n`, between its minimum and
  maximum. A width that adds no column (Escalations 4, an odd field width, Pen 9 or 10) is never chosen, so a cell changes
  only when a column fills or empties. Capacity is `slotsOf` at the cell's real place (`penOf`'s formula for the Pen). The sign
  can take Escalations slots, so the Mailbox is sized first, then the Escalations at the place that `M` gives them, then the
  Barn takes the rest. The north half never overflows: the two maxima sum to 20, which leaves the Barn its 10. Since 29.11
  the sign never meets the Escalations, so their capacity is 11 a column at every width (88 at the widest) and the order no
  longer changes a width; it is kept.
- **The south half can overflow.** Size the Pen to `P*` and the field to `F*`. If `P* + F* <= 31`, both stand and the rest is
  grass. If not, today's split is each one's floor. The Pen takes the widest width on its ladder at most
  `min(P*, max(15, 31 - F*))`, and the field takes the widest on its ladder at most `min(F*, 31 - P)`. A busy farm whose pen
  and field are both full falls back to 29.2's 14 + 16 (28 goats, 35 pairs). Neither is squeezed below what it held before
  this section. **Dropped by 29.11:** the farm widens to `P + 1 + F` instead, so the Pen and the field each keep their own
  width and the south half never overflows.
- **The minimum.** The home cell is the stop that is always there (H-D5), and a zone with nothing in it is still not a stop
  (N-D3). The minimum is what keeps a cell's picture drawn at every count: the post and its flag, the mailbox and its dog, the
  pen's fence and gate, the field's soil. It also gives the first item a slot. A letter that comes on a ledger patch before the
  farm document counts it lands, and its zone becomes a stop without a re-shape.
- **The maximum and the "+N".** `ESCKIND.CAP` (24) goes as a layout limit. `ESC.cells` takes `cap`, the slot count `escOf`
  has, so the crate appears only once the Escalations are at their maximum and full. The `open` list's 120 is the hard ceiling.
  The other cells keep their "+N" as now.
- **The stands `s`** are counted by the same rule `gardenerSpots` uses to fill `st[3].fields`. Each gardener in `gardeners[]`
  counts at `homeFor(g)`'s biome, else the Commons's. A steward whose project has a biome stands at its sign and is not
  counted. Step 12 moves that rule into one function that both call, so the size and the stands cannot drift apart.
- **The farm's counts, not the viewer's.** A "mine" scope draws fewer letters or stands in a cell than the farm holds. The
  cell keeps the farm's size, so `layout()` stays independent of the viewer.

**When it re-lays out.**

- **Nothing new triggers `layout()`.** `onFarmDoc` already runs `layout()` on every farm document. The sizing runs inside it,
  so a count change re-shapes that one farm in the same pass. An escalation, a wish and a gardener reach the page only through
  the farm document, so their cell is sized the moment they are drawn.
- **A letter or a loose work row** can reach the page first on a ledger patch. The patch still calls only `homeFill`, on the
  rects the last `layout()` made, and needs no `layout()` (H-D4's third reason holds). A letter that does not fit goes into
  the zone's "+N not shown" until the next farm document re-shapes the farm. That document counts it, since `home.letters`
  is built from the same ledger rows (`farm_home.go` `lettersOf`).
- **The same counts change nothing.** A patch with the same counts runs no `layout()`, as before. A document with the same
  counts runs `layout()` as always, but every rect comes out the same, so nothing moves or re-bakes.
- **Hysteresis.** A cell grows at once to the narrowest width that holds `n`. It shrinks only when the narrowest width that
  holds `n + HOME.slack` (2) is narrower than its width now, and then it shrinks to that width. Example: one Escalations
  column holds 11. The 12th makes it two columns, and it goes back to one only when 9 are left. So one letter or escalation that
  arrives and leaves never flaps the farm. The south half's floor rule applies after the hysteresis (until 29.11 drops it).
- **Where the previous widths live.** `layout(doc, memo, opts)` takes `opts.prev`, the page's last world, and reads each
  biome's cell widths from `prev`'s `b.home.cells`. They live in memory only. A first layout, or a reload, sizes every cell to
  the narrowest width that holds its count. `layout()` stays pure in its inputs, and a test passes `prev` to drive it.
- **What a re-shape costs.** `scene.js`'s `homeCheck` adds the five cell rects to the key it drops a farm's chunks on, so the
  farm's 32 x 32 re-bakes. `biomeSig` does not change, so the biome's foot does not drop (since 29.11 it does when the
  farm's width steps; see "What a slide costs" there). When a cell's column count
  changes, its items re-flow in reading order and keep their sticky slot indices (`homeFill`'s `slots`, `penOf`'s
  oldest-first order, `escOf`'s need order). A cell that only slides (the Escalations when the Mailbox grows) carries its
  items with it unchanged.

**What reads the rects, and what step 12 does with each.**

| Reader | Reads | Step 12 |
|---|---|---|
| `world.js` `farmstead(r, w)`, `layout()` | `HOME.cells`' fixed offsets today | `farmstead` takes the four widths and builds the rects above. `HOME.cells` keeps the zone order the cursor and the outline read, and gains each cell's minimum, maximum and ladder. `HOME.cross` (15) becomes the row only. `HOME.slack` 2 |
| `escOf`, `penOf`, `slotsOf`, `homeFill` | the cell rects | no change but `escOf` passing `cap` to `ESC.cells` (`esckind.js`), whose `CAP` constant goes |
| `cursor.js` `buildHome`, `zoneAt` | `ZONE_AT`'s fixed 2 x 2 (X-D10). Each zone's columns and rows come from its rect | none: the zones keep their places, and the grids follow the rects |
| `curnav.js` arrows, `homeSpec`, the outline (`WORLD.outline`, `CURNAV.outline`) | the 2 x 2. Zones sorted by rect | none: the quarters keep their corners, so the order is unchanged |
| `people.js` `escStands` | the icon's right, else the cell's east margin column | none: it follows the rect. At the 3-tile minimum, the east margin is the column right of the icon |
| `people.js` `gardenerSpots` | the field-stand rule | the rule moves into the shared function the sizing counts with |
| `art.js` `drawHome`, `homePaths` | `home.cross.x/y` and the asks' path from the Escalations' rect | `homePaths` returns four paths: the east-west arm, the north arm at `xn`, the south arm at `xs`, the asks' path. The flag post stays at the Escalations' south-west corner, which is still the north arm's bottom tile. The pen's ground, the field's soil and the mailbox on its keep follow their rects. The grass is not painted |
| `scene.js` `homeCheck` | the release, letters and asks flags | adds the cell rects to the key |
| `lanes.js` `walkLanes` | `home.cross.x` for the north and south ways out | the south way runs from the stand west to `xs`, then down. The north way runs up to row 15, along it to `xn`, then up. The west and east ways are unchanged. Walks start only at field stands (28.12), and escalation stands never walk (28.13) |
| `truck.js` `bayOf`, `lanes.js` `bayOut`, the ride truck | `home.bay` | none: the bay is fixed under the Barn's keep |
| `goats.js` | the pen rect: fence, gates, grazing in the pen | none: it follows the rect |
| `beacons.js` (Mailbox), `birds.js` (Barn), `viewcut.js` (Growing field) | their cell rect | none |
| `garden.js` `fitHome`, `homeIn` | the farmstead's 32 x 32 or the zone entered | none: a 3-tile zone is still capped by the zoom step, as now |
| `snap.js`, `POSTCARD.farm`, the jump board, Home, `at=` | the farmstead and its ring 1 plots, ids | none: the farm's rect and every id are unchanged |
| the memo, v5 | `{v: 5, cells: {key: {k, t}}, slots: {planId: {s, t}}}` (29.5): lattice indices and plot slots | none, and no version bump: it stores no cell rect, before or after. `garden.cursor.v1` holds node ids. `homeFill`'s `slots` hold slot indices in page memory. The hysteresis's previous widths live only in the page's last world |

**What step 12 tests.** These run under node like the rest of `WORLD`:

- an empty farm at every minimum (Barn 22, both arms on columns 22 and 23, 15 tiles of grass);
- each cell's ladder, one column a step, with no width that adds no column;
- each cell full at its maximum, then one more: the Escalations' crate, the field's badge, the Pen's and the Mailbox's `+N`;
- the south half's floor rule (both full: 14 + 16);
- the hysteresis: 11, 12, 11, 10, 9 Escalations, with `prev` given, are 1, 2, 2, 2, 1 columns;
- one count change moves only that farm's cells: `slotRect`, `shape()`, the region and every other biome are deep-equal;
- today's counts at today's widths give 29.2's rects;
- `walkLanes` from a field stand when `xn != xs`.

**Built (step 12, gd-nOkEhUBDk4).** `WORLD.homeWidths` sizes the cells from `WORLD.homeCounts` and `opts.prev`; the capacities
come from `slotsOf` and `penOf`'s formula at each width (`WORLD.capAt`), the ladders from the pitches (`WORLD.ladderOf`), and the
stands from `PEOPLE.fieldHome`, the rule `gardenerSpots` uses. `shape()` now returns the farm's rect (`farm`) and its sign,
and `layout()` builds the cells with `farmstead(farm, widths)`; a `farmstead(r)` with no widths is the empty farm's. The 10x
golden's digest moved with the cells and the flowers in them; every project's domain, region and plot is unchanged. The
tests are `testdata/home_cells_test.js` (`TestHomeCellsUnderNode`).

Goldens re-shoot once, in step 12's bless PR, kept apart from the feature PR (#2190): only the shots whose fixture farm's
counts move a line.

### 29.11 The farm grows with its cells (plan gd-PJB3D7glwF, step 14 gd-Lvz2J4ejV3)

The owner answered step 13's check-in (decision gd-ArfegepUfk, thread gd-qqILFtfc2H, shots gd-zOYdzVT7XZ) with option B: "Grow
the whole farmstead instead (neighbours and the camera move on a count)". The Barn keeps the empty farm's 22 tiles and the farm
widens instead. This section replaces 29.10's envelope (X-D13's first clause), answers X-D13's rejected column item by item,
and is recorded as X-D14. Step 15 (gd-gFMObwPAzs) builds it and decides nothing. Step 16 (gd-aLxzuhqVxL) is the check-in.

**The farm's width.** With `E`, `M`, `P` and `F` the cells' widths from `homeWidths` (29.10):

- `need = max(24 + E + M, P + 1 + F)`. The north half needs the Barn's 22, the north arm, the asks' path and the two asks; the
  south half needs the Pen, the south arm and the field.
- `W = 32 + HOME.step * ceil(max(0, need - 32) / HOME.step)`, with `HOME.step` 4, so `W` is 32, 36, 40, 44 or 48.
  `g = W - 32` is the farm's growth.
- The Barn is `W - 2 - E - M`, at least 22. The grass is `W - 1 - P - F`, at least 0. The rects are 29.10's, with `W` where
  they read 32.

| Farm | E, M, P, F | need | W | Barn | grass |
|---|---|---|---|---|---|
| empty | 3, 5, 8, 8 | 32 | 32 | 22 | 15 |
| 12 escalations, little else | 5, 5, 8, 8 | 34 | 36 | 24 | 19 |
| 29.2's widths | 7, 8, 15, 16 | 39 | 40 | 23 | 8 |
| every cell at its maximum | 11, 9, 23, 22 | 46 | 48 | 26 | 2 |

- **Width only.** The farm stays 32 tall. The east-west arm stays on row 15, so the north cells stay 15 tall and the south cells
  16, as 29.10 has it. A taller farm would move every plot row beside it and change its lattice row's height. Then the projects
  below it would move too. Each cell's ladder is in its width: an asks column already holds 11.
- **The step of 4.** It is the smallest step that is a multiple of every band pitch (trees 2, stones 2, pots 4), so the slide
  below keeps every band item on its cell grid. `need` moves one tile at a time from 32 to 46. A 1-tile step (the owner's
  shots, 34 and 35) gives 14 widths, and the project next door could move for one letter at a Mailbox column boundary. The
  4-tile step leaves five widths, so a neighbour moves at most four times while one farm fills. The slack goes to the Barn's
  forecourt and the grass.
- **No hysteresis of its own.** `W` is a function of the four widths. Those already carry 29.10's 2-item hysteresis (E, no
  hysteresis, stays rejected), so `W` flaps only if a cell does, and no cell flaps.
- **The south half's floor rule goes.** `homeWidths` sizes the Pen and the field each on its own ladder. The farm widens to
  `P + 1 + F`, so neither is squeezed.
- **Sized before it is placed.** `homeWidths` and `W` move into `layout()`'s step 1, beside the grid and before the regions are
  sized (H-D4's second reason). `fit[key]` gains `widths`, and `fit[key].grid` gains `g`. The placing loop builds the farm from
  `fit[key].widths` and runs no second `homeWidths`. `capAt` builds its trial farm at the `W` its widths give, so the Barn is
  22 there too and the sign is clear of the Escalations.

**The slide: how a biome makes room.** One rule covers the whole biome. A biome with growth `g` is its `g = 0` biome cut along
the farm's base east edge, `cut = fx + 32`, which is the east lane's west side. Everything east of the cut moves `g` tiles east,
and every rect that crosses the cut is `g` tiles wider. Nothing west of the cut moves, and nothing moves vertically.

| What | With `g` |
|---|---|
| the farmstead, slot 0 | `W x 32` at the same `(fx, fy)` |
| slot k with `SPIRAL[k].c > 0` (the east column of plots) | `x + g` |
| every other slot, the west lane | unchanged |
| the east lane, the overflow crate | `x + g` |
| the north and south lanes, the Orchard, the Graveyard, the Nursery, `plotArea`, `used`, the foot | `w + g` |
| a band's trees, pots and stones | placed on the `g = 0` band, then `x + g` when east of the cut |
| the sign, the bay, the keeps, the Barn's door | unchanged: west of the cut |
| the region | `{ox, oy, w + g, h}` |

- **`slotRect(c, k, g)`** adds `g` to `x` when `SPIRAL[k].c > 0`, and adds `g` to `w` when `k` is 0. `shape()` reads `g` from
  `grid.g`. It computes the bands and lanes at `g = 0` as today, then slides them, and `regionSize` follows. The prototype's
  `slotRect` (fe30b20f1, never merged) is this rule. Its module-wide `GROW` is not: here `g` is an input to `slotRect` and
  `shape()`, so a later call from any reader gets the same farm.
- **Band items stay on their cells.** `layout()` places the Orchard's trees, the Nursery's pots and the Graveyard's stones as it
  does today, on the `g = 0` band's width and lanes, then slides each one east of the cut. `hashCell` gets the same count, so no
  tree or pot re-hashes when the farm steps. X-D6 leaves every band cell that a lane crosses empty, and the cut is on the east
  lane's side, so no item straddles it. The `g` columns the slide opens in each band, over and under the farm's new east part,
  stay empty. The Graveyard's rows are counted on the `g = 0` columns, so a step never changes a region's height.
- **X-D5's cost, paid.** "Two pitches in `slotRect` and everything that inverts it" comes down to one offset, east of the farm,
  and nothing inverts it. The cursor's grids are logical (`slotCell`'s 7 x 5, `ZONE_AT`'s 2 x 2). Its pointer `pick` hit-tests
  node rects. The lanes read `b.lanes`, and `people.js` reads each plot's own rect (plots stay 32 x 32). A larger `SLOT` for
  every plot stays rejected: it moves all 34 plots on every count.

**The lattice: who moves next door.** Today each region is centred in its lattice cell, and each column is as wide as its widest
region. A grown region would re-centre on every step, moving the farm's own west half and its whole column by `g / 2`.
Instead, a region is placed by its base width:

- `w0 = w - g`, and `colW0[c]` is the largest `w0` in column `c`. A region's offset in its cell is
  `off = floor((colW0[c] - w0) / 2)`, today's centring on the base widths, and its `rx` is the cell's `x + off`.
- `colW[c]` is the largest `off + w` in the column, the grown regions' east edges. `colX` and `rowH` follow as today.
- With every `g` at 0 this is today's layout exactly. So step 15 moves no project at an empty farm, and the memo's cells
  (X-D9) are untouched.

On a step of a farm in lattice column `c`:

- **its own biome** moves only east of the cut, as above;
- **the other projects in column `c`**, in every lattice row, do not move: they are placed on base widths;
- **the projects in the columns east of `c`**, in every lattice row, move east by the change in `colW[c]`. That change is not 0
  only when this region's east edge passes the column's furthest east edge, and it is at most 16 tiles in all;
- **nothing moves west or vertically**: no region's height and no `rowH` depends on a count.

**What a slide costs, and when it is paid.**

- **The foot re-bakes.** `biomeSig` holds `used` and the lanes. A step changes the grown biome's `used.w` and the moved biomes'
  `used.x`, so `onFarmDoc` drops their old and new feet, as it does for any changed sig; this needs no new code. The cost is
  paid only on a step of `W` or a move of a column: at most four steps as a farm fills from empty to full, and the hysteresis's
  on the way down. A letter that changes no cell width pays nothing.
- **`homeCheck`** needs no change. Its key holds the cells' `x` and `w`, which a step changes, and the foot drop covers the farm.
- **Running transitions.** `transitions.js` plans each one on the last world's rects. In a biome that slid or moved, step 15
  cuts its running transitions to their end state, as reduced motion does. The goats, the trucks and the birds draw from the
  rects every frame, so they follow.
- **The memo** is unchanged, with no version bump. `W` is recomputed from the counts and `prev`'s cell widths, and the memo
  stores no rect.
- **Goldens.** Every shot whose fixture farm has `need` over 32 moves. They re-shoot in step 15's bless PR, apart from the
  feature PR, as step 12b (gd-XgHu8hNr8a) did.

**The camera.** Today a farm document leaves the camera where it was, in world px, so a re-layout shows as the world moving
under it. Step 15 adds an anchor and a reframe to `onFarmDoc` and `drawDoc`.

- **The anchor.** Before the swap, the page picks one biome to hold still. It takes the cursor's biome when the cursor's Thing
  meets the view, else the biome whose region holds the view's centre, else none. After the new world is laid out, the camera
  moves by the change in that biome's farmstead top-left `(fx, fy)`. The move happens in the same frame as the new world is
  drawn, and it applies to `cam`, `camTo`, a running fly's target and `signFr`. The farm's corner is the one point in a biome
  that none of its own counts moves: the slide is east of the cut, and the cells move inside the farm. So the anchor moves only
  when a column west of it widened, and then the whole biome moved as one, so holding the corner holds all of it. The anchor
  move does not ease: on the screen, the biome is still where it was. What visibly moves is the grown farm's east part and the
  projects in the columns east of the one that widened.
- **The reframe.** When the camera is still the page's own framing, it eases to the new framing:
  - **`looked` false** (the camera is `frameCursor`'s, desktop or phone): after the anchor, `frameCursor('')` runs. It returns
    when the framing has not changed (its own half-tile test), and otherwise flies there over 240 ms with `flyTo`'s ease (a cut
    under reduced motion).
  - **After a `fitHome` step** into the farm or a zone (`zoomStep` with a fit, which sets `looked`), the page keeps
    `fitAt = {id, cam}`, the node that was fitted and the camera it gave. `userCam` clears it. If the camera is still `fitAt.cam`
    after the anchor, `fitHome` runs again on that node's new rect and the camera flies there over 240 ms. So a phone framed on
    the farm keeps the whole farm in view as it widens, zooming out a little: a 48-tile farm fits a 360 px board at about 0.45.
  - **Otherwise** (the viewer has moved the camera), only the anchor runs.
- **Mid-gesture.** A drag or a pinch (`ptrs` not empty) gets the anchor but no reframe. Pans and pinches are relative to the
  last pointer position, so the finger stays on the same ground, and the gesture's `userCam` has already cleared `fitAt`. A fly
  that is running is re-aimed: its target moves by the anchor's delta, and it restarts its 240 ms from where the camera is.
- **Not tweened.** The slide itself is a cut, as every re-layout is today (a new plan's plot appears without a tween). The ground
  is baked in chunks, so a tween would bake every frame or draw the biome twice.

**What reads the rects, and what step 15 does with each** (29.10's reader list, walked again):

| Reader | Step 15 |
|---|---|
| `world.js` `slotRect`, `shape()`, `regionSize` | take `g` (`grid.g`) and slide (above) |
| `world.js` `layout()` | `homeWidths` and `W` in step 1; the lattice placed on base widths; the band items placed on the base band and slid |
| `world.js` `farmstead(r, wd)` | none: it already reads `r.w` (`B`, the Mailbox at `r.w - M`, the field at `r.w - F`, `cross.xs`). `HOME.cells`' Barn minimum becomes 22, and `HOME.step` 4 is added |
| `world.js` `signOf` | the sign is fixed at `fx + (SLOT.w - HOME.sign.w) / 2`, `fx + 12`, not centred on `r.w`. A count never moves the name, which now always stands over the Barn's forecourt |
| `homeWidths`, `capAt` | the south half's floor rule goes; `capAt`'s trial farm is `W` wide |
| `escOf`, `penOf`, `slotsOf`, `homeFill` | none |
| `cursor.js` `buildHome`, `zoneAt`, `slotCell`, the framing (`farmOf`, `around`) | none: the 2 x 2 and the 7 x 5 are logical, and the framing reads the home's rect |
| `curnav.js` arrows, `homeSpec`, `pick`, the outline (`WORLD.outline`, `CURNAV.outline`) | none: the zones' order is unchanged, and `pick` hit-tests rects |
| `people.js` `escStands`, `gardenerSpots`, `stewardSpot` | none: they read the icon, the plot rects (still 32) and the sign |
| `art.js` `drawHome`, `homePaths`, the flag post | none: they read `home.w`, `home.cross` and the Escalations' rect |
| `scene.js` `homeCheck`; `garden.js` `biomeSig`, `onFarmDoc` | the bake needs no change; `onFarmDoc` and `drawDoc` gain the anchor and the reframe |
| `transitions.js` | cuts the running transitions of a biome that slid or moved |
| `lanes.js` `walkLanes`, `bayOut`, `rideLanes`, `plotDoor` | none: they read `b.lanes`, `home.cross`, `home.bay` and the plot's rect |
| `truck.js` `bayOf`, the release truck, the ride truck | none: the bay is west of the cut |
| `goats.js`, `beacons.js`, `birds.js`, `viewcut.js` | none: they read their rects |
| `garden.js` `fitHome`, `homeIn` | none inside them; the reframe keeps `fitAt` |
| `snap.js`, `POSTCARD.farm`, the jump board, Home, `at=` | none: every id is unchanged, and `POSTCARD.farm` takes the union of the home's and ring 1's rects |
| the memo v5, `garden.cursor.v1` | none |
| `page_test.go`'s home-rect asserts | the home's `w` is `W`, and slot 1 is at `SLOT.dx + g` |

**What step 15 tests.** These run under node like the rest of `WORLD`, and the camera's through the page's browser checks:

- `W` at each row of the table above, and at the step's edges (`need` 32, 33, 36 and 37 give 32, 36, 36 and 40);
- the slide: a biome at `g` deep-equals its `g = 0` biome with every rect east of the cut at `x + g` and every rect across it at
  `w + g`. No `y` or `h` changes, no rect west of the cut changes, and every band item keeps its cell;
- the region's height and the Graveyard's rows are the same at every `g`;
- the lattice: with every `g` at 0 the layout deep-equals today's; a step in column `c` moves no other project in `c`, and
  moves the columns east of `c` only by the change in `colW[c]`;
- the hysteresis through `W`: 11, 12, 11, 10 and 9 escalations, with `prev` given and every other cell at its minimum, give
  `W` 32, 36, 36, 36 and 32;
- a full Pen and a full field give `P` 23, `F` 22 and `W` 48, with no squeeze;
- the sign is at `fx + 12` at every `W`, and the Escalations hold 11 a column at every Mailbox width;
- the camera: a document that widens a column west of the cursor's project leaves that project's farm at the same screen px;
  one that grows the cursor's framed farm flies to the new framing; with a pointer down, only the anchor runs.

**Built (step 15, gd-gFMObwPAzs).** `WORLD.homeW` is the rule above, and `HOME.step` 4 and the Barn's minimum 22 are in
`HOME`. `layout()` sizes each farm's cells in step 1 (`fit[key].widths`, `grid.g`). `slotRect(c, k, g)` and `shape()` slide,
and `shape()` keeps the `g = 0` band in `base` for the band items. The lattice centres each region on `colW0`, and column 0's
`x` is also placed on it, so a grown farm in column 0 moves nothing west either. `garden.js`'s `anchorOf` and `anchorCam` hold
the anchor in `onFarmDoc`. `reframe` runs in `drawDoc`, and `zoomStep` keeps `fitAt`. `ANIM.update` takes `cut`, the old feet
of the biomes that slid or moved. The rule tests are `testdata/farm_grows_test.js` (`TestFarmGrowsUnderNode`), and the camera
walk is `testdata/camgrow_browser.js` (`TestCameraGrowsUnderChromium`). Goldens re-shoot in their own bless PR.
