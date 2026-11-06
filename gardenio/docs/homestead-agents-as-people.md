# Homestead: agents as people (gardeners on their plots)

Design for Phase 1 of the "Agents as people" plan (epic gd-AbFYcF2BMr, phase gd-FyB9OEPqcB,
task gd-DBDxOAC3dj). The rest of the plan builds from it:

| Step | Builds | Reads this page for |
|---|---|---|
| 1b gd-DKMSc1Y91I | `gardeners[]` on `/garden/farm` | sections 2, 3 |
| 1c gd-e4uXZs8coZ | sprites, placement, card, outline | sections 4, 5, 6 |
| 1d gd-ikrXlqYTL5 | the Porch grouped by plot | sections 2, 6 |
| gd-elQWxzYhdm | truck ride, explode-and-fly-back, walk home | section 7 |
| CHECK-IN gd-vaFzNuIWJt | the walk-through for the owner | sections 8, 11 |
| Phase 2 (reason) | `reason{kind,text,href}` | section 2.3 |
| World Phase 3 gd-gMMtrgIT1W | walking between plots, camera follow | sections 9.1, 14 |
| Lands 9.8 (plan gd-vmw5s07A1F) | the steward on the map | section 9.2 |
| Phase 3, 3b gd-CnT77QLH7w | chat on the Commons thread: chip, selection, quick replies | section 12 |
| Plan gd-ryynhxINtp | every running gardener in its project's Growing field; one key, three stops | sections 4.4, 4.5, 12.3; world design 28.12 |

It extends two contracts and does not change them: the farm read model
(`homestead-farm-read-model.md`) and the World layout (`homestead-world-design.md`). Every
addition is additive, so `meta.schema` stays `1`.

## 1. Facts this design rests on

- **The farm reads no agents as rows.** `farmWorkTypes` (`internal/gardenhub/farm.go`) leaves
  out `agent`. It reads open agent beads only as a liveness roster for `waits_on`: one
  `ListBeadsFiltered{Types: agent, Statuses: open,in_progress, Limit: 500}`, at most every
  `farmRosterEvery` (20 s), and only while some plan has an open decision or escalation.
- **The hub's fast tier already reads the same list.** `Hub.fetch` (`hub.go`) lists open and
  in_progress agent beads on every refresh, for the `agent` section (the Porch and the Yard
  villagers). The farm and the hub refresh on the same daemon events.
- **Agent bead fields used here:** `agent` (name, else the title), `role` (`crew`,
  `plan-gardener`, `plan-captain`, `release-captain`, `reviewer`, `cicd`, `infra`, `thread`,
  and sometimes a comma list such as `gardener,plan-gardener`), `project` (and the `project:`
  label), `agent_state`, `task_id`, `last_progress_at` (RFC 3339, stamped only when a turn with
  real usage lands), `thread_id` / `slack_thread_bead_id`, `operated_by`. When the bead closes
  it also has `stop_requested` (`true` after `gb stop`) and `wrapup` (JSON with `tldr`).
- **`agent_state` values seen:** `spawning`, `working`, `yielding` (alive); `failed`,
  `rate_limited`, `budget_exceeded` (stuck); `idle`, `assigning`, `queued`, `prewarmed`
  (idle bucket); `done`, `stopping`, `stop_requested`, `expired` (on the way out); `dormant`
  (closed beads only). The buckets come from `agentStateBucket` in
  `cmd/beads-viewer/agents.go`.
- **A plot is 16 x 12 tiles.** Its interior is 14 x 10: one bed row per phase, a 1-tile plant
  per listed task (`planBeds` in `web/world.js`). The bottom row (`plot.y + 11`) is the fence.
  Under the bottom row of plots, the nursery band starts one tile lower (`nurseryY = bottom + 1`).
  So there is no free 2-tile path under every plot to stand on.
- **Sprites** are 16 x 24 string maps drawn with `fillRect` (`VIL0`, `VIL1`, `VILSIT`,
  `SCARE` in `web/art.js`), cached as bitmaps by `spriteCache` (PR #545). The Yard draws agents
  as villagers in `ZONES.agent` (14 x 3 = 42 slots), dressed as the farm's gardeners (5.2).
- **The legend already promises** "Gardener at work", "Gardener resting or starting",
  "Scarecrow: the agent is down" and "Speech bubble: waiting on a human" (`buildLegend` in
  `web/garden.js`).
- **Chat moves to the Commons.** Lands 2.10 (gd-EwISaXh6Xk, PR #618) adds
  `GET /garden/c/bead/{id}`. For an agent bead it finds the thread through `thread_id` (or the
  legacy `slack_thread_bead_id`), cut by the viewer's lands. It answers JSON to a client that
  asks for it and a 303 otherwise, and 404 when there is no thread. All `/agents/chat*` routes
  become redirects.

## 2. Data: `gardeners[]` on the farm document

### 2.1 Shape

A new top-level array on `GET /garden/farm`, next to `plans`:

```jsonc
"gardeners": [
  {
    "id": "gd-4sSwB54J3I",                 // the agent bead
    "agent": "gardenio-1a-design-note-c2s", // fields.agent, else the title
    "role": "crew",                        // raw fields.role
    "project": "gardenio",                 // the agent's project: label, else fields.project
    "operator": "oidc:owner@example.com",  // lowercased operated_by alias (the "Mine" scope)
    "state": "working",                    // raw agent_state ("" when unset)
    "bucket": "alive",                     // alive | stuck | idle | dormant
    "task": "gd-DBDxOAC3dj",               // raw task_id ("" when unset)
    "task_title": "1a Design note: ...",   // the task's title when the farm read it, cut at 120 runes
    "plot": { "plan": "gd-AbFYcF2BMr", "phase": "gd-FyB9OEPqcB" }, // or null
    "since": 1792420380,                   // unix seconds, floored to the minute
    "href": "/garden?bead=gd-4sSwB54J3I",  // the in-garden bead view
    "chat": "/garden/c/bead/gd-4sSwB54J3I",// the agent's Commons thread; omitted when it has none
    "reason": null                         // null while working; Phase 2: {kind, text, href} (section 2.3)
  }
]
```

| field | rule |
|---|---|
| `id`, `agent` | `agent` is `agentFromBead`'s name: `fields.agent`, else the trimmed title |
| `role` | raw. The client maps it to a sprite group (section 5.2). The server does not interpret it |
| `project` | the agent's own project, not the plan's. The plot says where it works; this says where it comes from (the hat colour) |
| `operator` | as on `FarmPlan.operator`. Lets "Mine" and the Porch put the viewer's own gardeners first |
| `state`, `bucket` | raw `agent_state`, and its bucket (section 2.2) |
| `task`, `task_title` | `task_id` as written. `task_title` is set only when the task is among the open work rows the farm read |
| `plot` | `{plan, phase}` from resolving the task (section 2.4), or `null`. `phase` is `""` when the task reaches the plan without a phase, and when the task *is* the plan epic |
| `since` | `last_progress_at`, else the bead's `updated_at`; floored to 60 s (section 3.3) |
| `href` | path below the viewer root; the page prefixes its `base`, as `beadHref` does |
| `chat` | `/garden/c/bead/<id>` when the bead has `thread_id` or `slack_thread_bead_id`, else omitted. A path below the viewer root, like `href` |
| `reason` | always present. `null` while simply working; else `{kind, text, href}` (section 2.3) |

`href` and `chat` are paths, not absolute URLs. The document stays viewer-neutral (section 3.1
of the read model): the place route does the per-viewer land cut when it is followed, so the farm
never reads the Commons. A `chat` link can still 404 for a viewer outside the agent's land. The
client then opens `href` instead, as the Porch does after PR #618.

**Dependency:** `chat` names a route that PR #618 adds. 1b must not merge before #618, or must
leave `chat` out until it lands. It must not point at `/agents/chat`, which #618 turns into a
redirect.

**Stability for Lands.** Lands Phase 9 reads 1b, 1c and Phase 2's reason (9.8 draws the steward
from this layer). Under `meta.schema = 1` these names and meanings do not change. New keys may
be added. A consumer must ignore keys it does not know and must treat `plot: null` as "at home"
(section 4.4).

### 2.2 Who is a gardener, and the bucket

One entry per agent bead that is all of:

1. type `agent`, status `open` or `in_progress` (the same list the hub and the roster read);
2. not an unengaged pool shell (`isUnengagedPrewarmedAgent`: `agent_state=prewarmed`, or
   `queued`/`idle` with a `prewarmed-` name and no `slack_thread_ts` or `task_id`);
3. not on the way out: `agent_state` is not `done`, `stopping`, `stop_requested` or
   `expired`. These beads close within seconds. Leaving them out means a gardener leaves the
   set when it stops, and the exit (section 3.2) says how.

`failed` and `budget_exceeded` stay in the set while the bead is open: they are the
scarecrows. They differ from the farm's `deadAgentStates`, which drives `waits_on` liveness, and
the roster keeps that meaning.

`bucket` is `agentStateBucket`: `dormant` for `dormant`, `alive` for
`working`/`spawning`/`yielding`, `stuck` for `failed`/`rate_limited`/`budget_exceeded`, `idle` for
everything else, including `""`, `assigning` and `queued`. `dormant` appears only on closed beads,
so in practice it never reaches this list. It is kept so the two functions cannot disagree.

**Shared code: copy and pin, not move.** 1b adds `AgentBucket(state string) string` and
`IsPoolShell(b *beadsapi.BeadDetail) bool` to `internal/gardenhub`, copied from
beads-viewer. `cmd/beads-viewer` already imports `gardenhub`, so a table test there
(`TestAgentBucketMatchesGardenhub`) runs both functions over every literal in section 1, plus
`""` and an unknown state, and over a pool-shell table (prewarmed, queued/idle shells, an
assigned shell with a `task_id`, an assigned shell with a thread). If either copy drifts, the
build goes red.

Why not a shared package: beads-viewer reads `agentStateAlive` and `agentStateStuck` directly
at about ten call sites (`agents.go`). Moving them is a refactor of `/agents` that this plan
does not own. Moving only the two functions would still leave the maps duplicated, so a pinning
test is needed either way.

### 2.3 `reason`

The key is in every entry from 1b. Phase 2 (2a, gd-TvJosTKfAt) fills it server-side in
`farm_reason.go`, so the card, the Porch and Lands say the same words. It is `null` for a gardener
that is simply working, so the card stays short. The kinds, first that holds wins:

| `kind` | when | example `text` | pose (2b) |
|---|---|---|---|
| `down` | stuck state, `pod_phase` failed/unknown, or a wrap-up with `blockers` | `Failed: OOMKilled (exit 137)`, `Blocked: needs the RBAC grant` | scarecrow |
| `parked` | an unanswered decision or pending escalation it filed, or one parking its task (`requesting_task`) | `Waiting on Merge PR #700` | speech bubble |
| `sleeping` | `dormant`, or a wrap-up `restart_when` | `Sleeping until ka-X merges` | zzz |
| `stale` | alive, but no progress for an hour (the `/agents` threshold) | `No progress for 3h` | wilting |
| `starting` | `spawning`, `assigning`, `queued` | `Waiting for a key in the account pool` | sprout |

`href` is set only for `parked`: the handle's in-garden bead view, which already carries the Mailbox's
answer row, so the reason links straight to the letter. The wire contract, with every source field, is
in `homestead-farm-read-model.md` (FarmGardener). A wrap-up is read only while the agent is not alive, so
a respawned agent is not judged by its last run's blockers. A `working` agent with an open handle has
moved on and is not parked until it also goes stale. Lands 9.7 adds `purse_empty`; a client ignores a
kind it does not know. A reason may change the pose; it never changes the placement.

**Drawn (2b, gd-J3l6W4X6DJ).** `web/reason.js` maps a known kind to its pose (section 5.1) and builds
the card's first line from `text`, so the card leads with the cause. For `parked` that line, and the
speech bubble over the gardener (a `gask` button, at the sprite's levels on a plot and at Bead level in
the Yard), open the Mailbox at the handle's letter (`#d-mail` row `i-<id>`, marked and focused) so it
is answered in place; when the letter is not in the Mailbox (the Mine scope) the handle's bead view
opens instead. The need order counts the pose, so a gardener down or asking ranks first whatever its
`state`, and the Porch lists every one of them under **Needs you**, its first group, with the reason
under its state word (red when down). Without a reason, or with a kind it does not know, the pose
comes from `state` as in Phase 1.

### 2.4 Resolving the plot

The task is resolved with the farm's existing walk. `resolve(b)` in `refreshOnce` climbs
`ParentID` through the open rows `byID` for up to `farmMaxDepth` (6) hops, until it reaches a
phase listed in a plan's `phase_order` or a plan. For a gardener:

| `task_id` names | `plot` |
|---|---|
| an open work row that resolves to a plan | `{plan, phase}` from `resolve` |
| a phase container itself | `{plan: <its plan>, phase: <the phase>}` |
| a plan epic itself (a plan gardener's usual task) | `{plan: <it>, phase: ""}` |
| a seed (`plan_state` draft/ready) | `null`: seeds are pots in the nursery, not plots (the client stands it by the pot, 4.1) |
| a loose bead (the meadow), a closed bead, a bead not read (row cap), or nothing | `null` (the client stands it by its wildflower, tree, headstone or goat when the map drew one, 4.1) |

It is the same function, over the same rows, in the same refresh, so a gardener always lands
on the plot its task is drawn in. A test pins a task two hops below its phase (task under a
sub-epic under the phase).

Only `plot` is computed on the server. The bed cell (which column) is the client's: it is
layout, and the client already knows where each task is drawn (section 4.2).

### 2.5 Order and bounds

- The snapshot array is sorted by `id`. Diffs are keyed by `id`, so order carries no meaning.
- `maxGardeners = 300`. That is five times the largest fleet seen, and one page of the existing
  read. Past it, the newest 300 by `updated_at` are kept and `meta.gardeners_truncated` is
  `true`. It is shown, never silent, as with `meta.truncated`.
- An entry is about 330 B raw. 60 gardeners add about 20 KB raw (2 to 3 KB gzipped). 300 add
  about 100 KB. The 10x budget test in `farm_test.go` moves from "under 600 KB" to "under
  700 KB" with 300 gardeners in the fixture.

## 3. Refresh and wire

### 3.1 Tier: the fast tier, one read for both

`gardeners[]` rides the farm's **fast tier**: one agent list per refresh, the same query as
`readRoster` (`Types: agent, Statuses: open,in_progress, Limit: listLimit`). The live-agent
roster is then built from that same result (filtered by `deadAgentStates`, exactly as today).
`readRoster`'s own 20 s gate goes away. The farm makes one agent read per refresh, never two.

Why the fast tier and not the slow one (20 s):

- Section 7 animates `agent_state` changes. A truck that leaves 20 s after the agent started,
  or a burst 20 s after a crash, says nothing true. The fast tier puts a state change on screen
  about 0.4 to 0.5 s after the daemon event (the read model's freshness numbers).
- The cost is one more list call per refresh, on top of up to 5 pages of work rows. `farmMinGap`
  (1 s) still bounds refreshes under a steady event stream. The hub already makes this exact
  read on the same events.
- The roster gets fresher as a side effect (each refresh instead of each 20 s). That can only
  make `waits_on` more accurate. Its stale rule (`farmRosterStale`, 2 min) is unchanged.

**A failed or truncated agent read never fails the refresh.** The farm keeps the last good
`gardeners` set and publishes no change for it. If no good read has happened for
`farmRosterStale`, `meta.gardeners_as_of` (unix seconds of the last good read) is set, and the
barn card says "gardeners as of N ago". When reads are fresh the key is omitted, so `meta` does
not change on every refresh.

### 3.2 SSE: a keyed set, plus exits

A patch gains one set and one transient list:

```text
{
  "v": ..., "prev": ...,
  "gardeners": { "up": [ /* whole gardener */ ], "rm": ["gd-..."] },
  "exits": [ { "id": "gd-...", "agent": "...", "kind": "finished", "tldr": "Merged MR 3286 ...",
               "plot": { "plan": "gd-...", "phase": "gd-..." }, "at": 1792420500 } ]
  // ...plans, seeds and the blobs as today
}
```

- `gardeners` follows `plans` exactly: `diffEntries` over a map of marshalled entries, whole
  object replaced, never merged. A `reset` carries the full `gardeners` array.
- `exits` is **patch-only**. It is never in the snapshot or a reset, and the client never stores
  it. It says *why* a gardener left the set, which `rm` cannot. For each id in `rm` (at most 20
  per refresh, the rest get no exit), the farm makes one `BatchGetBeads` and reads:

| `kind` | when | `tldr` |
|---|---|---|
| `finished` | `stop_requested=true`, and the task is closed, or is a plan or phase container, or there is no task | `wrapup.tldr`, cut at 140 runes |
| `parked` | `stop_requested=true`, and the task is still an open work step (it stopped behind an open handle: a merge escalation, a decision) | `wrapup.tldr` |
| `interrupted` | no `stop_requested`: killed, pod lost, reaped, expired | omitted (the synthetic "closed without a wrap-up" text is not shown) |

  `plot` is the gardener's last plot (`null` if it was at the barn). `at` is when the farm
  saw it leave.
- Exits are computed only when this read and the previous one were complete. Without that, an
  id dropped by the cap or by a failed read would look like an exit.
- Patches and resets are otherwise unchanged. A client that misses an exit (reconnect, reset)
  only misses an animation: the set itself is always right.

### 3.3 Churn

The entry leaves out everything that ticks with each turn: cost, tokens, `live_turns`,
`context_tokens`. `since` is floored to the minute, so a gardener upserts when its state, task,
plot or minute changes. That is at most about once a minute while it works. 60 working
gardeners add about one upsert a second across the fleet, about 330 B on each open stream. That
is well inside what the plan patches already send.

## 4. Placement

All of it is client-side and pure: `WORLD.gardenerSpots(world, gardeners, level)` in
`web/world.js`. It is DOM-free, so the Go tests run it under node like `TestPlansLogicUnderNode`.
It returns, per gardener, either `{plot, col, x, y}` in world px or `{home: true}`, plus
per-plot `{shown, more}`. It runs when the data changes, never per frame (World design rule 5).

### 4.1 On a plot or at home

A gardener stands on a plot when `plot` is not null **and** its bucket is `alive` or `stuck`.
One that is `alive` or `stuck` with no plot in the world stands **beside its work** when the map
draws it somewhere else (gd-6FCoWelnx2, the owner: "workers in the orchard and other areas
should show up there"): its `task` (else `tends`; a wish by its goat or headstone) found in the
layout index as a Meadow wildflower (the home strip's Growing field), a Nursery pot, an Orchard
tree, a Graveyard headstone or a goat in its project's pen (`WORLD.standItem`). It stands with its
feet on the item's bottom row, from the column right of the item, in the nearest free column
inside the item's band, never on another of the band's items, under the same per-level cap and
`+N` badge as a plot's fence (4.3); `plots[<item id>]` holds the group, with no lane, so a walk to
or from it goes straight (section 7). It counts on its biome's sign like a plot-stander, and its
Porch row, outline entry, card and field figure say **"In the Meadow: <title>"** (`WORLD.standWhere`;
the Nursery, the Orchard, the Graveyard, the pen) where a plot-stander's say the plan; the Porch
button reads "Go there" and puts the cursor on the item. The server still sends `plot: null` for
these (2.4): the stand is the client's, from the same index that drew the item.

Everything else is at home, in its project's Growing field only (4.4): `idle`, `dormant`, or a
task drawn nowhere (past a band's cap, the overflow marker, `+N more plans`, not read). That
covers "idle gardeners wait at the farm" (owner, gd-elQWxzYhdm): an idle gardener with a task
stands at home beside its task's plant, and its card names the task. Every gardener on a plot
or beside an item stands in its field as well (4.4).

**Thread agents (5b, gd-1kFVOmUjiK).** A thread agent (a `role` token `thread`, or `dm`) is one the
bridge sends to answer a thread in the Commons or Slack. Its task is its thread head, never under a
plan, so it is at home unless it tends a plan's bead (`tends`). It stops after each reply and
comes back on the next message, so one that has stopped is rightly not there; one the stale reaper
finds with no room in the cluster (`pod_scheduling_blocked`, or `agent_state` failed with the reaper's
`close_reason`) is down, a scarecrow saying why, until its bead closes. At home its words say what it
serves, not where it stands: its Porch row, outline entry (6.2), card, tip, barn link and the
cursor's label read **"In the Commons: <thread_title>"** (the farm's `thread_title`, "a thread" when
it was not read), where anyone else's at home say where it stands (world design 28.12). Its thread
is on its card and its Porch row (12.3); its bead face names the thread head ("In the Commons", a
link). A field figure at work wears its doing pose (5.1) as on a plot. Its tool calls lift birds
(web/birds.js) from its figure, up off its head and away, the call's caption left on its head
(gd-CykqEHeEpX), over its project's strip, which counts as one plot, at most 8 birds over it; a
gardener out on a plot lifts them from its plot figure only, so one call is never shown twice.

### 4.2 Where on the plot: on an empty tile of its field, facing its task

A gardener stands on an **empty tile of its plot's field**, inside the fence, and **looks at what it
tends** (the owner's call, gd-HB0xs0CTbk, Slack thread gd-FikFgWvLMr). An empty tile holds no mark:
no task cell, no bed's `+N` crate or stubble, no phase stake (the tile up and left of its bed) and
nothing of the `+N phases` crate's bed (`WORLD.plotMarks`). Feet are on the tile's row, sprite top at
world px `16 * row - 8`, and the spot carries `face`, `"l"` or `"r"`: the side its target is on.
`ART.drawGardener` mirrors the figure for `"l"` (cached per facing, as the truck's `TRUCK.l` and
`TRUCK.r`), so the figure visibly looks at its task.

- **Its task is a cell on the plot.** It takes the free tile nearest that cell, left or right of it
  (never in the cell's column, so it can face it): fewest steps across and down, ties to the task's
  row, then the rows above, then right before left (`WORLD.besideOf`). A tile where the figure's head
  (the sprite's top 8 px, in the row above) would cover a mark or another figure comes after every
  tile that covers nothing, so a stake's pips and the plant above stay in view. Two on one task take
  the nearest two empty tiles. These are placed first and are **never capped**: at any level that
  draws people (Plan, Bead) every one of them is drawn.
- **Its task is not a cell on the plot** (the plan, a phase, a `+N` cell, off the plot). It tends a
  bed (`WORLD.bedBox`): its phase's bed when its task is one here, else the growing bed (a step in
  progress) nearest the gate, else the nearest bed with steps. It stands on the nearest free tile
  beside the bed's used blocks, facing it. These are capped (4.3).

The **bottom fence** is only the overflow: a gardener with no empty tile left stands on it (feet on
the fence row, `plot.y + 31`), in the nearest free fence column from the gate (`plot.x + 1`), right
first then left, still facing its target.

Before gd-HB0xs0CTbk a gardener stood **on** its task's cell (gd-lXIyn1hqFl), covering the plant it
tended, and everyone else stood on the bottom fence. Before that, everyone stood on the fence under
its task's column. The owner wanted both the claim and the plant visible: a figure beside its task,
looking at it.

### 4.3 Fan-out and "+N"

On one plot, gardeners are ordered by need first: `stuck`, then `yielding` (ask), then
`working`, then the rest. Ties go to the earliest `since`, then `id`. They are placed in that
order, those on a task cell first. The cap below counts **those not on a task cell only**; a
gardener tending its task's cell is drawn whatever the count (4.2).

| Level | Gardeners not on a task cell drawn per plot | Past the cap |
|---|---|---|
| World, Project | none (nobody is drawn) | counted on the signs (4.5) |
| Plan | 3 (`WORLD.GARDENER_CAP[2]`) | a "+N" badge in the next free fence column |
| Bead | 6 (`WORLD.GARDENER_CAP[3]`) | the same badge |

The need-first order means a crash or an ask is never the one hidden behind "+N". The caps are
constants so the check-in can tune them (section 11). The badge is a 16 x 16 plate on the fence
after the last fence-stander, or at the gate when nobody is on the fence. Its number is a DOM label (`aria-hidden`, under the label caps). Its
hot is a button that opens the plot's card, which lists every gardener on the plot.

### 4.4 Home: the Growing field of its project

Once the farm document has loaded, the strip draws from `gardeners[]`. Before then, or when the
farm is failing, it draws the hub's `agent` section as today. The read model's rule "the ledger
and the scene never depend on the farm" holds.

**Every running gardener stands in its project's Growing field** (plan gd-ryynhxINtp, world
design 28.12; the owner in thread gd-VdxyebESAP). Its project is its home, `homeFor(g)` (world
design 28.4): a thread agent's is the Commons. Each is a **field stand**, `field:<gardener id>`,
in a pair slot of the field: the plant for its bead (its task's `growingBead` row: growing while
in progress, a beetle on a bug, a wildflower when merely open) beside the figure, which stands
solid, in its pose, with its bubble, facing the plant. A gardener at home with a task stands
beside its task's plant; one with no task (idle, dormant, a thread agent) stands with no plant.
A gardener out on a plot, or beside a band item or a goat (4.1), is drawn in two places, solid
in both: there, and in its field. The stands are placed first, in need order (down, then
asking, then working, then resting), each keeping its slot by id; the loose beads take at most
half the field's slots and never one a stand holds; past the field, one "+N" badge counts both
(world design 28.12, Density). A steward is never in the field: it stands at its homestead.

The Barn's ghosts of 5a (gd-JnmuZLs0kD) were removed by plan gd-ryynhxINtp: the field stand above
replaces them.

**The barn is still the account of every running agent** (5a; the owner in Commons thread
gd-KawmXXnLtg: "it's important we have an account of all agents running for the operator"), in
words. The Barn cell draws only its picture, the release truck at its bay and its sign; it holds
no figures. The barn card and the Barn's link read "N running: H resting, M out in the plots"
(", L at a homestead" when a steward run is up; `WORLD.barnWords`), H those at home on no plot; the
Barn's cursor sign says it short, "N running, M out", so it fits one line beside the Barn on a
desktop. N is the Porch's count (`PORCH.groups`), so the barn, the Porch and the title plaque always
agree, past the badge too.

**The stands in words** (gd-xx7tjsBkXO). A stand is an outline entry under its project's Growing
field, "<agent> — working: <task title>", or "<agent> — resting" with no task (`WORLD.standLine`);
the '/' jump board lists it by its agent's name (or bead id) under its project, saying "working:
<task title>" or "resting", and Enter on it goes to its field figure. A Porch row's place words say
where it stands: "In the field of <project> and on <plot>" for one out ("and in the Meadow: <bead>"
beside a band item), "In the field of <project>" for one at home (`WORLD.fieldWhere`); those at home
are grouped under "In the field", and their Go button reads "Go to the field". The home cell's line,
its sign and the jump board's home row count the field as "N working · M growing" (`WORLD.homeWords`),
with no barn count. A farm snapshot frames a stand as its gardener's bead, in its line's words.

"Home" is one function, `homeFor(g)`, which names the biome whose Growing field the gardener
stands in (world design 28.4). Lands 9.8 makes a steward's home its land's homestead:
`project:<key>`, the biome of its project (section 9.2), at its sign.

### 4.5 Biomes you are not looking at

A gardener out on a plot is drawn in two places (4.4): the plot of its task, in the biome of
the **plan's** project, and the Growing field of its own project. The counts below count it once,
where its plot is. The world shows every biome, so "not looking at it" means it is off screen or
the zoom is World/Project, where people are too small to draw:

- **Project zoom:** the plot sign label adds a count word, for example `· 2 at work`,
  `· 1 down`. **World and Project zoom:** the biome sign adds one too, for example
  `3 gardeners · 1 down`. The words go in the existing label strings, so there are no new labels
  and the label caps and drop rules are unchanged. State is never colour alone.
- The **outline's Gardeners group** (6.2) and the **Porch** (1d) list everyone, wherever they
  are. Activating one flies there.
- A gardener from project A working on a plan in biome B stands on its plot in B, wearing A's
  hat colour, and in A's field. The hat colour is how a visitor shows.
- **Mine:** a gardener is in "Mine" when its `operator` is the viewer's. On the map it follows
  the plan rule (it is drawn when its plot is). In the Porch, "Mine" filters by `operator`.
  Project-scoped viewers are World step gd-uzktet9xCC's. When that step hides biome B, it must
  list B's visiting gardeners under "Away" in the Porch, not drop them.

## 5. Sprites

### 5.1 Poses

Every pose is 16 x 24 in the `VIL0` style. Hat letters are `H`/`h`, shirt letters `S`/`s`.

| Pose | When (client, from `state`/`bucket`) | Sprite | Legend row it makes true |
|---|---|---|---|
| work | `working` | person with a hoe. Two frames, hoe up and hoe down, at Bead level only (the 6 fps loop) | Gardener at work |
| stand | `spawning`, and every home gardener (idle, assigning, ...) | `VIL0`. A home gardener in the Yard keeps today's `VIL0`/`VIL1` step at Bead level | Gardener resting |
| ask | `yielding`, or reason `parked` | `VIL0` plus the existing `drawBubble(..., "?")` over the head | Speech bubble: waiting on a human; click it to answer in the Mailbox |
| down | bucket `stuck`, or reason `down` | `SCARE`, recoloured: its straw hat (`Y`) takes the gardener's hat colour and its shirt (`v`) the role colour, so you can tell whose scarecrow it is | Scarecrow: the agent is down; its card says why |
| sleep | reason `sleeping` | `VIL0` with shut eyes under a zzz (`VILSIT` with shut eyes at the barn) | Zzz: asleep until its wake condition |
| wilt | reason `stale` | `VILWILT`, slumped, a wilted sprout on its hat (the wilt palette) | Wilting: no progress for an hour or more |
| start | reason `starting` | `VIL0` with a green sprout growing from its hat | Sprout: a gardener starting up |

Phase 2 adds a pose for each `reason` (section 2.3, `web/reason.js`), which wins over `state`: `down`
is the scarecrow and `parked` the ask pose (the bubble is a button to the letter); `sleeping` is `VIL0`
with its eyes shut under a rising zzz (sitting, `VILSIT`, at the barn); `stale` is slumped (`VILWILT`:
head and arms hang) under a wilted sprout in the plants' wilt palette; `starting` stands with a green
sprout growing from its hat. The marks (zzz, sprouts) carry the same 1 px rim, are drawn over the sprite, never
into it, and stay in its own column, so a neighbour on the plot never hides one. Section 7 adds four transition
sprites:
truck (24 x 16), fly (arms up), walk with a scroll (two frames) and a 2 x 2 particle.

**Doing poses** (step 4c of plan gd-gAKb8Bf0Nm, gd-kxCviRUVA2; the classes are the farm read model's
"Activity"). A gardener whose pose is `work` wears the pose for what it is doing now, `ACTIVITY.pose`
(`web/activity.js`), through `WORLD.gardenerState`'s `lookOf`, which `ART.drawPlotGardeners` asks per
frame; a reason's pose and every non-work pose win over it. Each is two frames (`<pose>0`/`<pose>1` in
`ART.GD_MAPS`), its prop in the figure's padding the way the hoe is. 4f (gd-1EMXaKkRPI) made them tell
apart at Bead zoom, one CSS px a pixel: each has its own whole-body posture and a big prop out to the
padding's edge (columns 2 to 19, rows 2 to 23 of the padded map, never above the hat, where the still bird
perches), in a colour (`ART.DOING_PROP`) 10 or more (CIE76) from every shirt and every ground by day and by
night. Any two of the hoe and the doing poses differ by 40 or more pixels of silhouette in the still frame
(`TestGardenersUnderNode`; 4c's crate and clipboard differed by 2).

| class | Pose | Legend row |
|---|---|---|
| `edit`, `shell` | the hoe (work, unchanged) | Hoe: a gardener at work, editing a file or running a command |
| `read` | knelt low, the hat 5 rows down, a big brass magnifier held over the bed, its glass glinting | Kneeling with a magnifier: reading files |
| `test` | a silver watering can in the right hand, its spout past the hip, pouring; the drops fall by turns | Watering can, pouring: running tests |
| `build` | knelt behind a big iron pot on the fire, the long spoon stirring | Kneeling at a pot on the fire: building |
| `git` | striding, a crate as wide as the padding in both arms, bobbing | Striding with a crate: git |
| `beads` | a tall white clipboard held up on the left, eyes on it, the pencil writing | Clipboard held up: writing beads |
| `deploy` | the left arm up on a tall pole, its red pennant over the hat's corner | Arm up on a flagpole: deploying |
| `web` | a brass spyglass to the eye, out past the right edge, sweeping up | Spyglass: on the web |
| `delegate` | both arms flung up in a V, calling the hawk circling the plot (4b), a leather glove waving; no second figure | Both arms up, calling the hawk: a subagent is out |
| `think` | the stand under a `"..."` bubble that bobs | Bubble with three dots: thinking |
| `waiting` | leant hard on the hoe planted on the left, the body tilted over it, head up at the bird | Leaning hard on the hoe: waiting on a tool for 5 s or more |

A subagent back (its stop) is the hoe again, and so is a doing last heard 120 s ago or more with no call
out (`ACTIVITY.STALE_S`): no longer news. The document's `delegate` seeds a subagent out. The pose never
speaks alone: the card's line says `doing: go test · 4 s ago`, and the outline row says the word
(`WORLD.doingWord`: "at work · running tests · <plot>"). Honest limit: hook events are the only per-action
signal and the model's intent is not visible, so the poses are by tool, never by purpose. A steward keeps
its ledger. The page repaints on each `farmactivity` and every 5 s, so `waiting` and a stale doing show at
the Plan level and under reduced motion too.

### 5.2 Colours

**A shirt of its own.** Each gardener wears one of eight shirts, `ART.GD_WEAR[h32(id, 3) % 8]`,
so one tells from the next at the barn and on a plot (the owner's ask, gd-wJVEevAYPP; before it
the shirt said the role group, which left a crew-heavy barn all one blue):

| index | shirt | `S` / `s` |
|---|---|---|
| 0 | blue | `#4c7ae0` / `#3758a1` |
| 1 | orange | `#f28c28` / `#ae651d` |
| 2 | violet | `#a672d8` / `#78529c` |
| 3 | amber | `#ffbe0b` / `#b88908` |
| 4 | teal | `#4fc3c8` / `#398c90` |
| 5 | cream | `#d9d0c0` / `#9c968a` |
| 6 | salmon | `#ffadad` / `#b87d7d` |
| 7 | slate | `#8d99ae` / `#666e7d` |

The eight are dE 26+ (CIE76) from each other, and 10+ from every doing prop and every ground by
day and by night (`TestGardenersUnderNode`). Two groups dress by role instead, `ART.GD_SHIRTS`,
each dE 15+ from every shirt above and from the other: the steward (`steward`; Lands 9.8, its own
sprite with a shepherd's crook, section 9.2) in raspberry `#c4405a` / `#86243a`, and a thread agent
(`thread`, `dm`; 5b, answering a thread in the Commons) in pink `#e0559a` / `#9c2e66`, which the
legend promises. The group is still the first comma-separated token of `role` that names one
(`WORLD.ROLE_GROUP`: crew, gardener, infra, steward, thread, other); it picks the sprite set and
these two shirts, nothing else.

**Hat by plan.** The index is `h32(plan id, 2) % 8` of the plot the gardener stands on, so
everyone on a plot wears that plot's hat and the plot's heads badge (`plotHeads`) shows one hat;
with no plot it is the straw hat (8). A steward has no plot and minds its land, so its hat is its
land's biome index, `h32(project, 2) % 8`, the same input that picks the biome's ground (World
design 3.3). The ground palettes are all greens, so no hat uses them: the list is the old `HATS`
by index, with the green entry (index 4) replaced:

| index | hat |
|---|---|---|
| 0 | `#d94a3a` |
| 1 | `#3a86e0` |
| 2 | `#f2d44a` |
| 3 | `#a672d8` |
| 4 | `#fffaf0` (was `#5fae3e`, the meadow grass itself) |
| 5 | `#f28c28` |
| 6 | `#ff9ec4` |
| 7 | `#78c0f4` |
| 8 | `#e4cc94` (`P`, straw: no plot) |

The Yard villagers wear this scheme too, so a gardener looks the same at the barn and on its
plot. Before the farm document is in, the Yard draws the hub's agent rows; each row carries
`data-role`, and `hubWear` (garden.js) gives it the look its gardener will have (its own shirt, the
straw hat until its plot is known), so the farm streaming in changes nobody's colours
(gd-wJVEevAYPP: the hash-chosen placeholder it replaced turned a colourful barn blue).

### 5.3 Contrast in both schemes

None of the hat or shirt colours reach 3:1 against the green grounds (the best is about 2.3:1).
The hues differ but the lightness is close, so the figure has to read by its silhouette. The
pose shapes carry the state; colour only says who. The bare outline `k` is not enough on its
own: on the darkest grass it is 2.71:1 by day and 2.28:1 at night, where `night()` darkens the
outline and the ground together.

So people get a **1 px rim outside the `k` outline**: cream `j` (`#fffaf0`) by day, and the
emissive `G` (`#ffe680`, not dimmed at night) at night. With the rim, the silhouette (the
better of outline and rim) is at least 4.0:1 by day and 3.6:1 at night against every biome
ground (`g`, `h`, `d` of all eight palettes) and against the fence and soil it stands on.

`TestPaletteTextContrastAA` covers only CSS text tokens, so 1c adds
`TestGardenerSpriteContrast` (under node, against `ART.palette` and `ART.BIOME_PALS`). It
asserts that silhouette contrast is at least 3:1 (WCAG 1.4.11, non-text) for every ground in
both schemes. The card, the "+N" label and the sign count words use existing text tokens, so
`TestPaletteTextContrastAA` must keep passing unchanged.

### 5.4 Cache and motion

- Each pose is baked once per `(pose, frame, group, hat index, shirt, scheme)` into the
  `spriteCache` (PR #545), keyed `gd|<pose>|<frame>|<group>|<hat>|<d|n>`. The ask pose
  reuses the stand bitmap under a bubble, so that is at most 3 x 2 x 4 x 9 x 2 = 432 bitmaps
  of 16 x 24, made lazily. Chunk bakes never draw people, so they stay cheap: people are on
  the dynamic layer, like plants.
- **Reduced motion:** frame 0 always (the hoe does not swing, no doing pose moves, the Yard does not step). Every
  transition in section 7 is a jump cut. Bubbles do not bob.
- At Plan level people are static. At Bead level they animate with the existing 4-frame loop,
  capped at 6 fps. That is World design section 6, unchanged.

## 6. Interaction

### 6.1 Hot links and the card

- A new hot kind, `gardener`, joins `LINK_KINDS`. It is an `a` whose `href` is `base + href`,
  so keyboard users reach it and middle-click opens the bead. Its rect is the 16 x 24 sprite.
  It is in the DOM at Plan and Bead level (`lv` = levels 2 and 3) when the plot is in view, and
  for field figures (4.4) at Bead level, as with the strip's other items. It is a `FINE` kind: on a coarse pointer
  under 44 CSS px the plot stands in, and the outline keeps the gardener reachable
  (`wantHot`). The "+N" badge is a `button` hot of kind `gmore`.
- **Card** (through `cardFor` / `describe`, the existing `showTip` path, one card):
  - title: the agent name;
  - role · project;
  - the state word and `since` ("At work · 4m"; "Down (failed) · 2h"; "Waiting on a human ·
    12m"; "Resting at the barn · 1h");
  - "Tending: <task_title> in <plan title>" when it is on a plot. When it is at home with a task:
    "Assigned: <task_title> (not on a plot)". When it is at home with no task: nothing. The line
    carries the task's line arms (`task_arms`, heraldry step 5.9, gd-QboM9sPYCg): the Porch row draws
    the run before the words (a plan's "Gardeners here" does not: two pictures there put a phone's
    cold bead view over budget B1, decision gd-C5WGUm5SLu), the link's name says it in words, and
    the figure wears it as a badge over its head at the Plan and Bead levels (`HERALDRY.badge`),
    on a plot and in the field, unless its speech bubble is up;
  - links: "Open bead" (`href`, the in-garden bead view) and "Thread in the Commons" (`chat`;
    named for where it goes, not "Chat", which read as the retired agent chat, gd-2Y9GQqK0Ok). It
    follows chat.js after #618: ask the place route for JSON, open the Commons pane on the thread,
    and on 404 fall back to the bead view. With no `chat` there is no thread link;
  - Phase 2: the reason line goes above the links.
- The **plot card** gains "Gardeners here: <names>" from `gardeners[]` placed on that plot, in
  need-first order, the first 6 then "+N". It replaces today's line built from the assignees of
  in-progress steps, which names people who may not be agents and misses plan gardeners.
- Clicking a gardener opened the Porch at its row (1d), the only click change on the map; its
  links stay links. Phase 3 and plan gd-ryynhxINtp change the click: 12.3.

### 6.2 The outline: a "Gardeners" group

`WORLD.outline(world)` gains one root after the Yard, before the biomes: **Gardeners
(N)**. It lists every gardener in need-first order, then by name. Each entry is
`{kind: "gardener", id, hot: "gardener:<id>", title: <agent>, state, plot: <plan title or, at
home, where it stands in its field, or for a thread agent at home "In the Commons: <thread_title>" (4.1)>, level: 2}`. Enter flies to its spot (its plot at Plan level, or its field figure) and hands
focus to its hot when the camera settles, as other entries do. Gardeners are not repeated
under the plots: "every id in world.index appears once". The "Gardeners" group is the one
place, and it is where a screen reader user finds who is down.

### 6.3 Legend

The four existing rows (`working`, `idle`, `crashed`, `ask`) draw the new sprites through one
`ART.drawGardener` call, using the crew shirt and the meadow hat, so the legend is the real
thing. The `ask` row draws a person under the bubble, not a bubble alone. gd-elQWxzYhdm adds one
row: "Truck: a gardener on its way to a plot". 5b (gd-1kFVOmUjiK) adds "Pink shirt: a thread agent,
answering a Commons thread; at the barn it says which", the thread shirt standing.

## 7. Transitions (gd-elQWxzYhdm)

The owner asked for this (gd-plgKbuTqwq): "showing agents working in a plot/plan seems way
better than having them hang out at the farm". A gardener rides a truck from the farm to its
plot when it starts. It explodes and flies back when it is interrupted. It waits at the farm
when idle, and walks back with its wrap-up when it finishes.

### 7.1 What triggers what

The client keeps the last applied entry per id. On each live patch it compares the old spot
(4.1) and pose with the new ones, and reads `exits`:

| From | To | Animation | Reduced motion |
|---|---|---|---|
| absent, or home | on a plot of its home biome | **walk out** of its field stand (28.12) along the lanes to the spot (world design 29.3); with no stand drawn (past the field's "+N"), the truck ride below | appears at the spot, and at its stand |
| absent, or home | on a plot of another biome | **truck ride**: the truck leaves its Barn's door, picks the gardener up at its field stand and drives straight to the spot; the figure steps off and takes its pose. With no stand drawn it drives from the door, on the lanes when they reach | appears at the spot, and at its stand |
| on plot A | on plot B (the task moved), or another column of A | **walk** along the paths between the plots (World Phase 3, section 14) | jump cut |
| on a plot, alive | on a plot, stuck (`failed`, `budget_exceeded`, `rate_limited`) | **burst**: 8 particles in hat and shirt colours at the spot; a scarecrow rises in its place; a fly-pose figure arcs back to its field stand and puffs out | the scarecrow replaces the person |
| on a plot, stuck | on a plot, alive (recovered) | the scarecrow drops and the person stands up in place (2 frames) | jump cut |
| on a plot | home (now idle, or the task went off the plots) | **walk back** to its field stand the way it came; from another biome's plot the **truck ride back**, by its stand to the barn's door | at its stand only |
| on a plot | exit `finished` | **walk home**: walking with a scroll to its field stand; on arrival a DOM label over the stand shows the `tldr` (cut at 60 characters) for 10 s | removed; the barn card shows "Just finished: <agent>: <tldr>" for 10 minutes |
| on a plot | exit `interrupted` or `parked` | **burst** as above; no scarecrow is left (the bead is gone) | removed |
| home | any exit | a small puff at its field stand | removed |
| absent | home | none (it just appears at its field stand) | none |

The rows cover what the owner listed: killed, crashed, parked and budget-stopped all explode
and fly back. The one unsure mapping is "parked" (section 11).

The page places each farm document's field stands (`WORLD.homeFill`) before `ANIM` plans its
transitions, since every document is a fresh layout: so a spawn's walk and a fly-back start
from, and aim at, the stand the new document draws. The Glass's flight from the transcript to the
plot (world design H-D21) is a camera move, never a walk: the figure is already on its plot.
Under reduced motion the camera cuts there instead of flying.

### 7.2 Rules that keep it honest and cheap

1. **The model never waits for an animation.** Placement (section 4) is the truth the moment a
   patch lands. The animation layer only draws an interpolated figure in place of that id's
   static sprite until it finishes. A moving gardener has no hot link until it lands, and hots
   are not rebuilt per frame.
2. **Nothing animates that the viewer did not just see happen.** That rules out the first
   document, a `reset`, every patch in the first 2 s after a (re)connect (the catch-up replay),
   an exit whose `at` is more than 10 s old, and a patch that would start more than 8
   transitions (a fleet restart, which cuts to the end state).
3. **Only what is in view.** At Plan and Bead level only, and only when the path's bounding box
   meets the padded viewport. Otherwise it is a jump cut. At most 12 run at once. More are cut.
4. **Timing.** A leisurely pace, so a viewer can see who left the barn and where they went
   (gd-OpU3ePStLF; it was 320 px/s and 0.8 to 2.5 s, which read as a blink). Truck and walk:
   `clamp(distance / 100 world px per s, 2.5 s, 7 s)`, so a far biome is still not a long wait.
   Burst: 1 s of particles, then a 2.5 to 4.5 s arc and a 0.6 s puff; the scarecrow drops in
   0.9 s. Easing is linear on position, as `flyTo` does. The constants are the top of
   `web/transitions.js`.
5. **Render loop.** While at least one transition is in view, the dynamic layer repaints every
   animation frame at Plan and Bead level. When none are running it goes back to the 6 fps
   Bead-level loop, or to nothing. World rule 5 still holds: nothing repaints on an idle frame.
   The progress clock is one function, `ANIM.now()`, so tests can pin it (8).
6. **Perf.** `world_perf_test.go` adds a 10x farm with 60 gardeners and 12 concurrent
   transitions. It must stay inside the existing frame budget.

## 8. Goldens and acceptance

The Looking glass gate (`TestGardenGoldens`, gd-E1TBc0lNdL) is live. Each new surface gets a
`goldenSurfaces` row, at `golden.Default` unless its PR argues for a tolerance. Each is shot at
phone and desktop, light and dark (`<surface>-<phone|desktop>-<light|dark>.png`).

**Fixture** (extend `cardWorld` and `goldenPorch`, timed against `goldenNow`):

| Agent | State | Task | Shows |
|---|---|---|---|
| `g-work-1`..`g-work-5` | working, crew (one `plan-gardener` on the plan epic) | steps of plan A | fan-out: with `g-visit`, six on plot A, so "+3" at Plan and all six at Bead; the foreman at the gate |
| `g-ask` | yielding | a step of plan B | ask |
| `g-down` | failed | a step of plan B | down, need-first order |
| `g-start` | spawning, infra | a step of plan C | stand |
| `g-idle` | idle | none | at the barn |
| `g-away` | working, another project | a meadow bead | at the barn, other hat |
| `g-visit` | working, project `zeta` | a step of plan A | a visitor's hat |
| `prewarmed-1` | prewarmed | none | excluded (absent from every shot) |

**Surfaces 1c and 1d add:**

| Surface | Shot |
|---|---|
| `gardeners` | Plan level framing plots A and B: three on A plus "+3", ask and down on B |
| `gardenersbead` | Bead level on plot A: the lineup under the task columns |
| `barn` | the Yard at Bead level, with `g-idle` and `g-away` and the barn card |
| `gardenercard` | `g-down`'s card, focused on desktop (a hover opens no card since gd-D1QRStvmCh) and pinned by a tap on phone |
| `legend` | the legend panel's gardener rows |
| `porchplots` | (1d) the Porch grouped by plot with "In the field" (those at home) last |

**Surfaces gd-elQWxzYhdm adds**, one keyframe each with `ANIM.now` pinned. The harness needs
a patch injector it lacked (#618 graded C10 "no SSE driver"): `cardWorld`'s test-only
`POST /test/farm/scene` edits the fake daemon so the farm publishes a real patch to the open stream, and the shot pins
`ANIM`'s clock past the 2 s catch-up window first (built in gd-elQWxzYhdm, `transitions_test.go`).

| Surface | Keyframe |
|---|---|
| `ride` | `g-ride-c` starting (spawning) on plot C: the truck at 50% of the barn-to-plot-C path. (`g-start` going working is no ride: spawning already stands on its plot, 4.1) |
| `burst` | `g-ask` going failed: particles at 15% |
| `flyback` | the same, the figure at 60% of its arc, the scarecrow already up |
| `walkhome` | an exit `finished` for the plan gardener `g-foreman` (its task is the plan, so a stop is finished, not parked; `g-work-2` is behind "+N" at Plan): walking with the scroll at 50% |
| `arrive` | the same at 100%: the `tldr` label over the barn |
| `reduced` | the `ride`, `burst` and `walkhome` patches applied with `prefers-reduced-motion: reduce`: the end state only. A DOM probe asserts no animation frame is scheduled afterwards |

**Acceptance:** 1c, 1d and gd-elQWxzYhdm each run `/commons-ux-acceptance` on their surfaces
before asking a human (C1-C12, `docs/commons-ux-rubric.md`). The gates that matter most here
are C5 (sprites in dark), C7 (sprites, not glyphs), C9 (every state at a glance, empty
farm, failed farm read), C10 (live in place: no jump, no duplicate figure, an honest reconnect),
and C12 (the Gardeners outline group and keyboard reach). Each PR attaches before and after
PNGs.

## 9. Forward compatibility

### 9.1 World Phase 3: walking gardeners (gd-gMMtrgIT1W)

What that step needs, and where this design provides it:

- **A stable identity.** `id` is the agent bead. It never changes while the gardener is live.
- **A plot that can change.** `task` and `plot` are upserted whole, and the client already
  diffs old against new (7.1, "plot A to plot B"). Phase 1 cuts. Phase 3 replaces that one row
  with a walk, and can reuse the transition layer (7.2) and its suppression rules as they are.
- **Positions it can interpolate.** `gardenerSpots` is pure and returns world px, so a walk is
  between two of its outputs. If Phase 3 routes walks along the 2-tile paths between plots, the
  truck rides should take the same routes.
- **A camera target.** The outline id `gardener:<id>` and `world.index` entries give a jump to
  a gardener a target. Phase 3 owns the deep-link syntax (World design section 7). This design
  reserves the kind name `gardener` in `WORLD.resolve`.
- **Order of work.** Phase 3 waits for this plan's Phase 1 check-in (edge
  gd-gMMtrgIT1W <- gd-vaFzNuIWJt, now in place).

### 9.2 Lands 9.8: the steward on the map

A steward is a gardener with role `steward`. 9.8 (gd-BdJ7R2i7Cg) gives it its own sprite (the
`steward` role group: a raspberry waistcoat and a shepherd's crook at its side; at work it reads its
ledger instead of hoeing) and makes `homeFor(g)` return the land's homestead instead of the Yard
barn. Nothing in `gardeners[]` changed for it.

- **Where.** One figure per land, right of its biome's name sign, feet on the sign's posts' row
  (`WORLD.stewardSpot`), drawn at the Plan and Bead levels like other people. It stands there
  whether or not a run is live: the land's cost row carries the steward (read model, "cost";
  its trigger, name and reason), and a live run in `gardeners[]` is merged into the same figure
  (`WORLD.stewardState`), so a steward is never also on a plot or at the barn. A live run without
  a cost row (no spend source) still stands there.
- **What it says.** Its reason, built server side: the purse wall ("Parked: the purse is empty",
  asleep), else the first line of its memory's In flight, else of its Promises (amendment
  2026-10-23 §8: the text comes from memory, not recomputed). A line naming a handle still
  waiting on a person is `parked`: the ask pose, and its bubble opens the letter. While a run is
  at work, the pose is the run's and the memory line stays on the card under it.
- **Hover and tap.** Its link opens the steward's tip, its card (`WORLD.stewardCard`), beside the
  sprite: on hover and keyboard focus, and on a finger's first tap; the second tap (or a click
  after hover) goes to its thread in the Commons while a run is live, else its bead. Escape or a
  press elsewhere closes it. The steward is not a "fine" target (section 8): it stands alone by
  its sign, so its padded hit box crowds nothing and a phone can always tap it.
- **Golden.** `steward`: music's steward at the Bead level with its tip open.

### 9.3 Phase 3 of this plan: the chat dock

Phase 3 planned a third dock pane reusing `/agents/chat*` fragments (3a gd-3ef0EJoyFT). Lands
2.10 retires those routes. The Porch and this card's Thread in the Commons link open the Commons pane on the
agent's thread instead. At the Phase 2 check-in the owner chose to reshape 3a around that thread
(decision gd-DPx4n1bTgx): the Commons pane is the dock. Section 12 designs what it still lacks.

### 9.4 Parity with the prototype's people and chat dock (PR #331)

The prototype (PR #331, `gardenio/docs/prototypes/homestead/` on its branch: `src/data.js`
`AGENTS`, `src/app.js` `renderChat`) was the design target for people and their chat. What
became of each part once Phase 3 (3b gd-CnT77QLH7w, 3c gd-3xrmzPMA1Y) shipped. **Live**: built
as the prototype showed it. **Changed**: built, in another shape, for the reason given.
**Dropped**: not built on `/garden`.

| Prototype | On `/garden` now | Mark |
|---|---|---|
| **Hat and shirt colours.** Each agent had its own invented hat and shirt (`mo`: amber hat, blue shirt) | A shirt of its own by id, a steward's and a thread agent's by role; hat by the plan it stands on (5.2). The Yard villagers use the same scheme, so a gardener looks the same at the barn and on its plot | Changed, then changed back (gd-wJVEevAYPP): shirts by role group left a crew-heavy barn all one blue, so the shirt tells gardeners apart and the hat says the plan |
| **States.** `running` (watering can), `waiting` (speech bubble), `idle` (sitting by the barn), `crashed` (scarecrow); an `!` bubble for alive but quiet over an hour | Poses from `bucket` and `reason` (5.1): work (hoe), stand, ask (the bubble, a button to the Mailbox letter), down (the scarecrow in the gardener's colours), sleep (zzz), wilt (no progress for an hour), start (sprout). Idle gardeners rest at the barn | Live, with two states added (`sleeping`, `starting`). The quiet one wilts instead of showing `!` |
| **Role and current task.** The card said "gardener of Edge IdP rollout" and "Working: Staging soak test" | The card (6.1): name, role · project, state word and `since`, "Tending: <task_title> in <plan>" or "Assigned: … (not on a plot)", then the reason line. The Porch row says the same | Live. The role is the agent's real `role` field, not "gardener of <plan>" |
| **Placement.** One resident gardener per plot; crew visited | Every live agent stands on the plot of its task, on the bottom fence under its plant (4.2); idle ones at the barn | Changed: a plan's gardener stands at the gate like any other agent; nobody is a plot's resident |
| **Chat threads.** One Chat pane with an agent picker (a chip per agent with a state dot) and a per-agent log; a plot's chat talked to its gardener; replies were canned | Each agent's own Commons thread, when its bead names one (`chat`, through `GET /garden/c/bead/{id}`), opened from the card's Thread in the Commons link, a Porch row or a click on the sprite (12.3). Real messages, mirrored to the agent's Slack thread. The Porch is the list of people; on a phone the Commons tab is the chat | Changed: the owner made the Commons pane the dock (gd-DPx4n1bTgx). The picker and the per-plot chat are dropped |
| **Quick replies.** Four fixed commands (`/status`, `/why is this blocked`, `/plant a seed`, `/water`) that filled the input | At most three, built from the gardener's `reason` and the chip (12.4): Answer (opens the Mailbox letter), What went wrong?, Where are you on <task>?, Is this blocking you? They fill the textarea and never send | Changed: no agent understands those commands; each reply now matches the gardener's state |
| **Context chip.** Clicking a plant set "About <id>: <title>" with a clear button; the message went out as `[<id>] <words>` | The bead open in the bead view sets the chip; it follows the selection until the person types, then pins (12.2, 12.9). It is sent as a last line, `About <id>: <title>`, with the id only across lands | Live. The id goes last instead of first, and the title stays in its own land |
| **Restart action.** A crashed agent posted "Restart me" with a Restart button in its chat; approving the "needs a bigger pod" letter also restarted it, with 4Gi | Wake (reason `sleeping`) or Restart (reason `down`) on the card and in the thread header, with one confirm step, admin only, `POST /garden/gardener/{id}/wake` (section 13). The agent resumes its conversation and task on the same pod spec | Changed: a button on the person, not a message from it. A bigger pod stays an escalation. Restarting a stale gardener whose pod is still up is gd-LUmZlr6CKM |
| **Pause / Wake the gardener** in a plot's action belt | `/plans` (`POST /plans/{id}/gardener/pause-toggle`, `.../wake`), with the other plan gates | Dropped from `/garden`, with the plan gates (read model, Not in this document) |

## 10. Decisions taken, with the alternative

| Decision | Why | Alternative not taken |
|---|---|---|
| `gardeners[]` on the farm document | Placement needs the farm's task-to-plan walk over the same rows; one snapshot, one stream | A separate `/garden/gardeners` endpoint (a second stream and a second resolve) |
| Fast tier, one agent read per refresh feeding the roster too | Transitions need about 0.5 s freshness; the hub already pays this read | The slow tier (20 s): cheaper, but every animation would be up to 20 s late |
| Copy `AgentBucket` / `IsPoolShell` plus a pinning test in beads-viewer | Small diff; drift reds the build | A shared package (moves about ten `/agents` call sites this plan does not own) |
| `plot` = `{plan, phase}` on the server; the column on the client | The server owns resolution; the client owns layout | A server-side cell index (couples the document to the bed layout) |
| Leaving states (`done`, `stopping`, ...) are not gardeners; exits say why | A stopping agent leaves once, with a reason the animation can use | Keep them as a "leaving" pose until the bead closes (two signals for one event) |
| Stand on the bottom fence under the task's column | Never covers the plant; fits every plot | Stand in the bed row (hides the plant), or a lane under the plot (no room above the nursery) |
| Caps of 3 at Plan and 6 at Bead, need-first | A crash or an ask is never hidden behind "+N" | One cap of 3 at both levels (hides real work at Bead zoom) |
| Hat from the plan's index, from the existing `HATS` minus green (was the biome's, gd-wJVEevAYPP) | Ties a gardener to the plot it stands on; a steward keeps its land's | The biome ground colour itself (green on green) |
| A light 1 px rim on people | The only way the silhouette clears 3:1 in both schemes | Relying on the `k` outline (2.71:1 by day, 2.28:1 at night) |
| `chat` is the Commons place route | 2.10 retires `/agents/chat`; the route does the per-viewer cut | A `/agents/chat` link (a redirect hop) or a thread id the farm would have to read |
| `since` floored to the minute | One upsert a minute per working gardener at most | Exact seconds (an upsert every turn) |

## 11. Open questions for the check-in (gd-vaFzNuIWJt)

1. **"Parked" explodes.** The owner listed parked among interruptions, so a crew that stops
   behind a merge escalation (`exits.kind = parked`) bursts and flies back. If that reads as
   alarming for routine parking, a one-line change makes `parked` walk home instead (with a
   letter for the open handle instead of a scroll). Ask after the owner has seen it.
2. **The caps.** 3 at Plan and 6 at Bead are a guess at what reads. Tune them after the owner
   has seen a busy plot.
3. **Project-scoped viewers.** The route is admin-only and the document viewer-neutral (read
   model, Authorization). A scoped cut of `gardeners[]` belongs with the World step
   gd-uzktet9xCC, which must keep visitors listed (4.5).

## 12. Phase 3: chat on the Commons thread (3a, gd-3ef0EJoyFT)

Decision gd-DPx4n1bTgx (2026-10-20) made the Commons pane the chat dock. Lands 2.10 retired
`/agents/chat`, so this phase reuses nothing from it. Talking to a gardener without leaving
`/garden` already works: the card's Thread in the Commons link and a Porch row call `CHAT.open`
(`web/chat.js`), which asks `GET /garden/c/bead/{id}` for the thread and opens the Commons pane
(`web/commons.js`) on it, with its composer, live stream, typing line and the phone's Commons
tab. This section designs the four things still missing: the context chip, what a farm click on
a gardener does, quick replies, and their place on a phone. 3b (gd-CnT77QLH7w) builds it.

**No new conversation store, no new pane, no new route, no new message field.** Everything
hangs off the thread fragment (`internal/commons/render.go`) and the pane's client.

### 12.1 Which threads get the chip and quick replies

Only a **gardener's thread**: one that `CHAT.open` opened. `CHAT.open` already knows the agent
id and gets back the place `{land, channel, thread}`. It hands both to the pane
(`COMMONS.open(at, {agent: id})`), and the pane keeps a session map `thread id -> agent id`. The
chip and quick replies show in the thread drawer's composer while the thread on screen is in
that map. The agent's entry in the farm document's `gardeners[]` (looked up by `id`) gives
everything else: `task`, `task_title`, `project`, `reason`.

A channel timeline, or a thread reached any other way (a URL, the channel list), gets neither.
There they would put a bead into a conversation nobody asked about. A reload of
`/garden/c/.../t/<thread>` therefore loses them until the gardener is opened again from the
farm or the Porch. That is acceptable for a convenience, and it saves reading the thread head
to find its agent.

### 12.2 The context chip

**What it names.** The bead the viewer has selected on the farm:

1. the bead open in the bead view (`?bead=`, `web/bead.js`), when there is one. Opening a bead
   is the most explicit selection there is;
2. else, once the cursor world lands (2b.3, gd-tdB8vCIltU), the cursor's Thing when it is a
   bead (a plan, a phase, a task, or a loose bead in the Commons biome). A project, a biome or
   the Yard is not a bead and gives no chip. Until 2b.3 lands, only rule 1 applies. The old
   `aim()` (the nearest plot to the pointer) is not a selection and gives no chip.

The gardener's own `task` is **not** a default chip. The agent already knows its task, and a
chip the person did not choose would be noise on every message.

**How it looks.** One line above the textarea, inside the form: "About", then the bead's title
(cut at 60 runes, the full title in `title=`), its id in the mono face, and a 44 px clear
button named "Remove the bead from this message". Its text goes in through `textContent`.
The server renders an empty `<div class="cm-about" data-slot="about" hidden>` in the thread
composer (the `compose` template, thread branch only), and the client fills it. This is the
`[data-slot]` pattern the typing line and unread badges already use.

**How it is carried: as text.** On Send, the client appends one line to the message text:

```
<the person's words>

About gd-AbCdEfGhIj: <title, cut at 80 runes>
```

The alternatives were a field on the message bead: a new `about` field, or the `snap` field the
farm snapshot already uses. Text wins on every count:

- **It reaches the agent.** An agent reads a Commons message through the Slack mirror and the
  thread forward (`internal/bridge/commons_mirror.go`). They carry the text, the file links and a
  snapshot's view link, and no other field. A new field would need bridge work to reach the one
  reader it is for. The `snap` field is dropped when a message has no file (`snapField`), and a
  chip is not a picture.
- **It renders already.** `linkify` (`render.go`) links a bead id in a message to the in-garden
  bead view, so the chip's id is a link in the Commons with no change.
- **It changes no storage semantics**, which 3b's scope forbids.

The line goes last, so the person's words lead the bead title, the folded band's newest-message
line and a push preview. The client keeps the words plus the line within the textarea's 10 000
limit (`maxlength`, and `checkText` on the server). When the words leave no room, the line is
shortened to `About gd-AbCdEfGhIj`, without the title. A message that is only a chip (no words,
no files) is not sent: Send does nothing until there is something to say, as today.

**Across lands, the id only.** A thread is read by everyone in its land. When the bead's
project is not the thread's land (a visitor gardener, a cross-project task), the chip shows and
sends the id without the title: `About gd-AbCdEfGhIj`. The title belongs to the other land's
readers. The id opens the bead view, which applies the reader's own cut. The client knows both
sides: the bead's project from the farm document, the land from the place answer.

**When the selection changes.**

- While the composer is **empty**, the chip follows the selection. A selection with no bead
  hides it.
- Once the person has **typed**, the chip is pinned to the bead it showed. A later selection
  does not replace it under their words. To change it, they clear it, and the next selection
  fills it again.
- **Clear** (the button, or Backspace in an empty textarea) hides the chip for this draft. The
  next selection change offers a chip again, so clearing is never permanent.
- **Send** clears the draft, and the chip goes back to following the selection.
- A **snapshot** (`COMMONS.snap`) and a chip can travel together: the picture shows what was in
  view, and the chip names the one bead the person means.

Selection changes only touch the chip. They never move the pane (12.3), and they never take
focus from the composer (cursor design 3.6 already keeps a data-driven move out of it).

### 12.3 A farm click on a gardener

| Where | Today | Phase 3 | Phase 4 (plan gd-ryynhxINtp, world design 28.12) |
|---|---|---|---|
| Plot figure (hot `gardener`), click or Enter | Opens the Porch at its row (`garden.js`, `PORCH.show`) | Opens its thread through `CHAT.open`, the same on both widths: the Commons band on desktop, the Commons sheet on a phone. The Porch is one tab away (3b R7, gd-6PFC4nXAVR) | Comes back to the yard: `?at=<project>/growing`, the strip framed, the cursor on its field figure `field:<gid>` |
| Field figure (`field:<gid>`), click or Enter | One out on a plot or beside a band item: Go to plot (`porchGo`), the cursor inside its plot or on the item it stands at (the owner, gd-ExtFqp653O: "agents in the yard don't zoom to map location when clicked"); one at home: its thread, as on a plot, else its Porch row | Unchanged | Opens its Glass (`benchOpen('glass', gid)`) with its transcript under the terminal; a viewer below the operator tier (`body[data-glass]` not "1") gets the transcript panel alone. Says "Watching <agent> in the Glass; Enter again goes to its plot" |
| Its Glass: Enter with the panel focused, or "Go to its plot" in the panel's actions | — | — | Closes the Glass and flies to its plot at the map's full zoom, the cursor on its plot figure: "On <plot>, <agent>: Enter goes home to the yard". Never while a text box, the nudge box or an attached terminal has focus. A resting gardener has no plot: Enter bumps and says "<agent> is not on a plot", and the button is hidden |
| Speech bubble (`gask`) | Opens its Mailbox letter (`REASON.open`) | Unchanged: the bubble is an answer, not a chat | Unchanged |
| Card: Thread in the Commons | `CHAT.open` | Unchanged | Unchanged: with the Porch row, the way to its thread |
| "+N" (`gmore`) | The plot card | Unchanged | Unchanged; the field's "+N" lists the field |
| Porch row | `CHAT.open` | Unchanged | Unchanged |

**Phase 4: one key, three stops** (the owner on plan gd-IlEcNCxdDi, decision gd-K2kVOIFRIi: "single
click or 'enter' should bring up glass + transcript, enter again should move to farm view with
gardener highlighted and full zoom, enter again should move back to yard"). A field figure opens
its Glass, Enter in the Glass flies to its plot figure, and Enter on the plot figure comes back
to its field figure. The flight is a camera move, never a walk; under reduced motion it cuts.
The thread leaves the figure's click and stays one click away, on its card and its Porch row.
The Workbench's Escape still only closes.

In Phase 3 the thread is what the person reached for, so a sprite click opens it alone on both widths; the
Porch no longer opens beside it on desktop (3b R7). A gardener with no thread (no `chat` on its entry, or a 404 from the place route) opens its Porch
row on both, as today, and never an empty pane.

**The pane stays put, except on an explicit gardener click.** Live updates (a gardener changing
plot, a new reason), cursor moves and bead selections never switch the thread on screen. A click
on another gardener does switch it. If the current thread's composer holds unsent words, the pane
keeps them, chip included, in a session map by thread id. They come back when that thread opens
again, and they are dropped on Send or a reload. So a switch never loses a draft and never asks
a question.

### 12.4 Quick replies

A row of at most three buttons between the chip and the textarea, built from the gardener's
`gardeners[]` entry. It is a pure function, `quick(g, chip)` in `web/chat.js`, tested under node
like `agentOf` and `placeOf`. The rows below are in display order:

| Holds when | Label | Does |
|---|---|---|
| `reason.kind == parked` and `REASON.letter(g)` gives a handle id | `Answer: <reason text after "Waiting on ">` (cut at 40) | `REASON.open(id)`: the Mailbox at the handle's letter, as the bubble does. **Sends nothing** |
| `reason.kind == down` | `What went wrong?` | Fills the textarea |
| `reason.kind == stale` | `Where are you on <task_title>?` (`Where are you?` without one) | Fills the textarea |
| a chip is shown | `Is this blocking you?` | Fills the textarea (the chip line names the bead) |

The first three are exclusive (one reason per gardener), so the row holds two buttons at most
today; the cap of three leaves room for Lands 9.7's `purse_empty`. `sleeping` and `starting` add
nothing: a sleeping gardener is woken by its condition or by 3c, not by a message, and a starting
one has not read its thread yet. A working gardener with no chip has no quick replies, and the
row is not drawn.

**A quick reply never sends.** It puts the words in the textarea, places the caret at the end
and leaves Send to the person. A canned message an agent acts on should be read before it goes.
Filling the textarea counts as typing for the chip, so it pins (12.2).

**The handle comes from the reason, which names one.** Phase 2a gives each gardener one `reason`
with one `href`: the handle it is parked on, first that holds wins. So the row has at most one
Answer. A gardener with several open handles shows the rest in the Mailbox, one tap away.
Listing them all would need a read of handles per agent, which the farm document does not carry
and this phase does not add.

**Decision cards in the thread.** Commons II step 3 renders decision cards with their buttons
(`internal/commons/decision_cards.go`). When the decision the Answer names has its card on
screen in this thread, the Answer button may scroll to and focus that card instead of opening
the Mailbox. 3b does not build cards, and falls back to the Mailbox whenever the card is not in
the thread.

**Live.** The row is rebuilt when the gardener's entry is upserted on the farm stream, only if
its labels change, and a focused button keeps focus when its label is still there (C10: no
jump).

### 12.5 Phone

- The chip and quick replies are part of the thread composer, so they live **inside the Commons
  sheet**. The sheet already follows the visual viewport (`viewport()` in `commons.js`), so they
  stay above the on-screen keyboard together with the textarea.
- Quick replies scroll sideways in one row (`overflow-x: auto`, no wrap), 44 px tall. The chip
  is one line, cut with an ellipsis. Together they add at most 96 px over the textarea, so a
  390 x 844 screen with the keyboard up still shows two messages of the thread.
- **Clear of the camera buttons.** While the sheet shows, it covers the map, the `.cam` strip
  and the cursor world's pad (cursor design step 7), so nothing of the map sits over the
  composer. 3b proves it with a DOM probe: `document.elementFromPoint` at the four corners of
  the composer form, the chip and each quick reply returns a node inside that form, with the
  keyboard down and with the visual viewport shrunk to stand in for it up.
- The phone's tab bar keeps its four tabs (Farm, Commons, Porch, Mailbox). There is no fifth
  "Chat" tab: the Commons tab is the chat.

### 12.6 Authz and the farm document

- **Posting follows the Commons' own rules.** The composer is rendered only when the Gate allows
  a reply (`composeFor`, `ActionReply`). Where it does not, the thread shows its existing note
  ("Only the members of ... post here", "Sign in to post here") and **no** chip or quick
  replies: both live in the composer's slots. The Answer button goes with them; the Mailbox
  applies its own rules when reached another way.
- **The farm document is unchanged.** The chip reads the bead view and the cursor. The quick
  replies read `reason`, `task_title` and `project`, already on every `gardeners[]` entry. No
  field is added, and the document stays viewer-neutral (read model, Authorization). The
  cross-land rule in 12.2 applies the land cut to what a message says; it does not change what
  the farm shows.

### 12.7 Acceptance for 3b

- **node:** `quick(g, chip)` for every reason kind, with and without a chip and a task title.
  `aboutLine(text, bead, land)` for the same land (id and title), another land (id only), the
  80-rune cut, and the 10 000 limit (the title dropped, then the line itself). The chip's
  follow, pin and clear rules as a small pure state machine (`chipNext(state, event)`).
- **Go:** the `compose` template's `data-slot="about"` and `data-slot="quick"` render in the
  thread composer only, never in a timeline's composer or the read-only note (`render.go` tests
  and the goldens).
- **`/commons-ux-acceptance`** on a gardener's thread with a chip and quick replies: C3 (390 px,
  keyboard up and down, the corner probe of 12.5), C4 (the desktop band beside the Porch), C5
  (chip and buttons in dark), C8 (Send, clear, Backspace, double-fire), C10 (an upsert that
  changes the reason rebuilds the row without moving focus), C12 (the clear button's name, the
  quick replies' names, Tab order: chip clear, replies, textarea, Send).
- Headless Chromium with zero console errors, as for every `/garden` change.

### 12.8 Decisions taken, with the alternative

| Decision | Why | Alternative not taken |
|---|---|---|
| The Commons pane is the dock | The owner's choice (gd-DPx4n1bTgx); thread, stream, typing and phone tab exist | A third dock pane (rebuilds a conversation view 2.10 retired) |
| Chip carried as a text line `About <id>: <title>` | Reaches the agent through the mirror unchanged; `linkify` already links it; storage semantics untouched | A new `about` field (invisible to the agent without bridge work); the `snap` field (needs a file) |
| The line goes last | The person's words lead the title, the folded band and push previews | A leading `Re:` line (every preview starts with an id) |
| Title only within the thread's land | A thread is read by its whole land; a title belongs to its own | Always the title (leaks across lands); no chip across lands (hides a real cross-project ask) |
| The chip follows the selection until typing, then pins | The bead never changes under the person's words | Always follow (silent swaps); never follow (a stale chip after a new click) |
| Chip and quick replies only on a gardener's thread | Only there is "the bead I am looking at" a question for this agent | Every composer (a bead pushed into #general unasked) |
| A sprite click opens the thread too, the same on desktop and phone (Phase 4 moves the thread to the card and the Porch row: world design H-D21) | The thread is what a person reaches a gardener for; one path for both widths (3b R7) | Porch only (one more tap before every chat); the Porch beside the thread on desktop (retired in 3b R7) |
| The pane switches only on an explicit gardener click; drafts kept per thread | Live data never moves what the person is reading or writing | Follow every selection (the thread jumps under the reader) |
| Quick replies fill the textarea, never send | A message an agent acts on is read before it goes | One-tap canned sends (the prototype's) |
| At most one Answer, from `reason.href` | Phase 2a names one handle; the Mailbox lists the rest | A per-agent handle read on the farm (a new field and a new read) |
| No fifth phone tab | The Commons tab already is the chat | A Chat tab (two ways to the same thread) |

### 12.9 As built (3b, gd-CnT77QLH7w)

Where the build settled something this section left open, or differs from it:

- **The selection** is the bead open in the bead view: `web/bead.js` fires `garden:bead` `{id, title, project, type}` when a
  bead opens, again when its document lands, and `{id: ''}` when the panel closes. A gardener's own bead (`type` agent) is
  no selection: it is the person being talked to. The cursor world's Thing (rule 2) is not wired yet; it can fire the same
  event once 2b.3 lands.
- **On a phone** the bead view and the sheet are never on screen together, so a chip shows in the sheet when the bead view
  is still open beneath it, or pinned: words typed first, then a bead opened over the sheet and closed again.
- **A sprite on a touch screen** pins its card on the first tap and opens the thread on the second, as every map link does.
- **The chip's id is a link** to its bead view (C12: an id is a link), so Tab meets the id, the clear button, the replies,
  the field. On a coarse pointer the chip's line is 32 px tall, and the id and the clear button reach 44 px through an
  invisible hit area, which keeps the chip and the replies within 96 px over the field (12.5).
- **Cuts** count grapheme clusters where the browser has `Intl.Segmenter`, so an emoji sequence is never split; the 10 000
  limit counts UTF-8 bytes, as the server's `checkText` does.
- **A quick reply over words already typed** goes on a new line after them.
- **The pane's hooks:** `web/commons.js` fires `commons:compose` `{form, text}` before a post (a listener may rewrite
  `text`) and `commons:sent` `{form}` after it. It keeps every thread's unsent words for the session, not only a
  gardener's, so a switch never loses a draft.
- **Acceptance:** `TestChatLogicUnderNode` (`quick`, `aboutLine`, `chipNext`, `cut`), `TestComposerStates` (the slots in a
  thread's composer only), `TestGardenerChatUnderChromium` (`testdata/gardener_chat_browser.js`: the whole of 12.7's walk,
  the double fire and the corner probe), and the `commonsgardener` and `commonsgardenerstress` surfaces of
  `/commons-ux-acceptance`.

## 13. Phase 3: wake or restart a gardener (3c, gd-3xrmzPMA1Y)

**There was no restart action to reuse.** `cmd/beads-viewer/agent_restart.go` only words the
card's "↻ Restart:" line, and `/agents` has no wake or restart button: its agent actions (keys,
nudge, compact, respond) talk to a live pod. The one revive in the codebase is the PATCH
`gb wake` and the wake-scanner send. So 3c adds one route, `POST /garden/gardener/{id}/wake`
(`internal/gardenhub/wake.go`), which sends that same PATCH. It is now
`beadsapi.ReviveAgentFields`, shared by all three paths: delete `agent_state` and
`next_respawn_at`, bump `respawn_count`, and stamp `wake_reason=manual`. A closed bead is
reopened in the same PATCH, as `gb wake` does. With the state cleared, the reconciler recreates
the pod under the same name, and the agent resumes its conversation and its task.

| Reason | Offered | Why |
|---|---|---|
| `sleeping` | **Wake** | Its condition can wait; the person chose now |
| `down` | **Restart** | A stuck state keeps the reconciler from creating a pod (`failed` is skipped); clearing it lets one start |
| `stale` | nothing | Its pod is up, and a revive would not stop it. `pod_phase` is never written, so the viewer cannot tell a hung pod from a slow one. Its thread's quick reply asks where it is (12.4) |
| `parked`, `starting`, working | nothing | Answered in the Mailbox, or already on its way |

- **Where.** On the gardener's card (`WORLD.gardenerCard`, extended in `garden.js` `cardFor`),
  as a button that works on a pinned card (a tap), as the card's links do. In its Commons thread
  header, through an empty `<div class="cm-wake" data-slot="wake" hidden>` that
  `internal/commons/render.go` puts in every thread. `web/wake.js` fills it only for a thread
  `CHAT.open` opened for a gardener: `chat.js` sends a `garden:chat` event with the agent and the
  thread. On a desktop the thread header is the way to it, because a hover card lets the pointer
  through.
- **Steps.** "Restart…" (or "Wake…"), then the question "Restart <agent>? It will resume its
  task <title>." with Restart and Cancel, then "Restarting <agent>…", then the answer in place.
  On success the button is gone; the next farm snapshot shows the gardener starting
  (`Farm.Invalidate`). A refusal says why and offers the button again.
- **Who.** Admin only. The mux applies `operatorOnly`, the route is classed `routeAdminOnly`, and
  `CanWake` (`viewerCanAdmin`, the gate gardener run-now applies to a spawn) both sets the page's
  `data-wake` and is checked again in the handler. The write goes on the operator's own bearer
  and `X-Operated-By`, as every viewer write does. An audit comment on the agent's bead names
  the operator.
- **Refused without a write:** a bead that is not an agent; an agent still running (`working`,
  `spawning`, `yielding`) or stopping; a closed agent not parked to be woken
  (`successor` is not `same`); `goal_unachievable`; a spent respawn budget (`gb wake --force`
  stays the way past it).
- **Not built here:** restarting a live pod. That needs a coop shutdown and then the revive,
  as the Slack bridge's Restart Agent does. It is follow-up gd-LUmZlr6CKM.

## 14. World Phase 3: walking gardeners (gd-gMMtrgIT1W)

Built on the cursor world (`superpowers/specs/2026-10-20-lands-cursor-grid-design.md`, 2b.3), so the
camera still only follows the cursor.

- **The walk.** The plot A to plot B row of 7.1 is now a `walkto` transition in
  `web/transitions.js`. It runs when a gardener shown on a plot is shown on another plot, or in
  another column of the same plot, and is not down at either end (a scarecrow does not walk). The
  7.2 rules apply unchanged: a live patch only, Plan level and in, in view, at most eight per patch.
  While it walks the figure stands in for its sprite at the destination, and it has no link until
  it lands (7.2.1).
- **The route keeps to the paths.** `WORLD.gardenerSpots` gives each plot a `lane`: the middle of
  the 2-tile path under it and of the paths left and right of it, as the sprite's top left, plus its
  project. `ANIM.route` goes down off the fence, along the path under plot A, then up or down the
  path beside A on B's side when B is in another row, along the path under B and up onto its fence.
  It never crosses a bed. Two projects' paths do not line up (packed regions), so a walk to another
  project goes straight. Along one plot's fence it is a straight step. The truck rides stay
  straight lines: they start in the Yard, which has no such paths.
- **Pace.** The route's length at the usual 100 world px per second, clamped to 1 to 7 s
  (`ANIM.WALK`). The 1 s floor is for a step of a few columns along a fence; a walk between plots is
  at least 34 tiles and takes 5 s or more.
- **"Go to plot" is a jump.** "Go to plot" on a Porch row and a gardener's row in the outline put
  the cursor in its plot, as the bead view's "Show on the map" and a `?bead=` link do. A walk does
  not move the cursor or the camera: data waits for the viewer's next move (spec 3.4). Phase 3 also
  shipped a ride-along, where the cursor followed the gardener from plot to plot with a **Stop
  ride** button; 3b R13 (gd-kbKSr58bnZ) retired it, with its button, its live-region text,
  `ANIM.eta` and `CURSOR.frame`'s `opts.also`.
- **Verified** by `TestTransitionsUnderNode` (route, lane, pace, hide, cut rules),
  `TestGoToPlotJumps`, and `TestGoToPlotUnderChromium` (`testdata/goto_browser.js`). The browser
  test checks that Go to plot gives the camera "Show on the map" gives, then applies the `walkto`
  farm scene (g-ask-b's task moves from plan B to plan C) and checks the walk with no link while the
  cursor and the camera stay in plot B, and that Go to plot again lands in plot C. No golden moved.
  A `walkto` golden keyframe waits for 2b.4: the goldens' `transition()` helper still drag-pans, and
  2b.4 is removing drag.
