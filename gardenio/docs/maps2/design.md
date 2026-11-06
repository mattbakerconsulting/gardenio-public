# maps2 — gardeners as connected cards

**Epic:** ka-0000001644 · **Operator:** Matt Baker · **Slack thread:** ka-0000002233

> Replace the beads-viewer `/maps` page — a good *name* on a garbage page — with a
> view of **all gardeners as connected cards**, each wired to its **trigger
> condition** and its **recent run results**. Built to grow.

---

## 1. The problem with old `/maps`

Today `/maps` is three bolted-together modes over the gardener set:

| Route | What it is | Why it's garbage |
|---|---|---|
| `GET /maps` | a gardener **picker** + a global **fleet grid** of dots | the grid is undifferentiated disks; you can't see *what fires a gardener* or *how its runs went* without hovering each dot |
| `GET /maps/{id}` | a per-gardener **radial constellation** of its bead graph | pretty, but it answers "what beads are near this gardener", not "is this gardener healthy and what triggers it" |

The page never shows the two things an operator actually wants at a glance:
**the trigger condition** and **recent run outcomes**. It's a graph toy, not a
fleet dashboard.

![old vs new](https://gardenio-sessions.s3.amazonaws.com/coop/sessions/images/ka-0000002428/8370cd83-maps2-old-vs-new.png)

[source: img/maps2-old-vs-new.svg]

## 2. The maps2 idea

One **card unit per gardener**, three regions, read left→right as a sentence:

> **trigger** *fires* → **gardener** *(pursuing a goal)* → **recent runs** *(how it's going)*

![maps2 board](https://gardenio-sessions.s3.amazonaws.com/coop/sessions/images/ka-0000002428/39e58494-maps2-board.png)

[source: img/maps2-board.svg]

### Card anatomy

![card anatomy](https://gardenio-sessions.s3.amazonaws.com/coop/sessions/images/ka-0000002428/acf13fbd-maps2-card-anatomy.png)

[source: img/maps2-card-anatomy.svg]

Every region maps onto data that **already exists** — maps2 is a new *view*, not
a new data pipeline:

| Card region | Source (existing) | Notes |
|---|---|---|
| ① **Trigger node** (left, wired in) | `gardenerViewFromBead` → `Cron`, `NextFireRelative`, `NextFire`; trigger def kind (`schedule` / `bead_query` / event) via `bridge.ParseTriggerDef` | color-coded by kind: cron=blue, bead-query=red, on-event=purple, paused=grey-dashed |
| ② **Identity** | `gardenerRowView` → `Name`, `Title`, `TerminalGoal`, `OwnerDisplay`/`OwnerSlug`, `Initials`, verdict color | verdict palette already used by the fleet map (`progress`/`stalled`/`stuck`/neutral) |
| ③ **Counters** | `gardenerRowView` → `RunsSpawned`, `RunsCompleted` (engine snapshot, or `RunsDerived` from iteration beads) | small "42 / 39" subline |
| ④ **Recent-run strip** | `gardener_timeline.go` → `schedule_prior_agent_id` chain → last N iterations, each carrying a `VerdictKind` (from `gardener_trajectory.go`) | newest on the right; one pill per run, colored by verdict |
| **Card ring** | `gardenerVerdictNeedsAttention` (`stalled`/`stuck`) + `StuckEscalated` | red ring + emphasis when the gardener needs an operator now |
| **Disabled state** | `Enabled` / `IsClosed` | dashed + dimmed |

### Interactions

- click a **card** → `/gardener/{id}` life-thread (existing detail page)
- click the **trigger node** → `/triggers`
- click a **run pill** → that iteration's agent bead (`/agent/{id}` / `/bead/{id}`)
- hover a card → tooltip with full counters + absolute next-fire (reuse the
  fleet-map `<title>` text builder)

### Layout

A responsive grid of card units (≈2 columns at desktop width, 1 at narrow).
Cards are sorted **needs-attention first**, then by next-fire soonest, then name —
so the gardeners an operator should look at float to the top. (Reuse the
`/gardeners` live-fleet ordering + the fleet board's `fleetGardenerIsLive`
predicate for the default filter.)

## 3. Reuse map (what feeds maps2)

maps2 should be a thin handler + pure view-model builder + one template, sitting
on the **same data spine** as `/gardeners` and old `/maps`:

```text
loadGardenerRows(ctx)                 // gardeners.go — the shared projection (same as /gardeners)
  └─ gardenerViewFromBead(bead)       // gardener.go — Cron/NextFire/verdict/counters/trigger surface
  └─ populateLiveGoalState(...)      // live target-epic status (optional in v1)
gardenerIterations / gardener_timeline // gardener_timeline.go — schedule_prior_agent_id chain → ordered runs
  └─ VerdictKind per run             // gardener_trajectory.go
```

New code is just:
- `maps2.go` — `handleMaps2` (GET /maps2): load rows, attach last-N runs, build view-model
- `maps2_view.go` — pure `buildMaps2Board([]gardenerRowView, runs) maps2BoardView` (unit-testable, no http/daemon)
- `templates/maps2.html` — the card grid
- a `Maps` nav link re-point (later: promote `/maps2` → `/maps`)

**Recent-runs fan-out caution:** a naive "fetch last N iterations per gardener"
is N+1 against the daemon. v1 options, cheapest first:
1. Cap N small (≈6) and batch-hydrate iteration heads across all gardeners in one
   bounded pass (mirror `gardeners_enrich.go`'s `CountOnly` enrichment pattern).
2. Bound total work with a wall-clock budget like `mapsHydrateTime` — degrade to
   "counters only, no run strip" for gardeners past the budget rather than hang.

## 4. Teardown checklist (old /maps)

Files to delete (verified present under `gardenio/controller/cmd/beads-viewer/`):

```
maps.go  maps_chat.go  maps_controls.go  maps_filter.go  maps_fleet.go
maps_fleet_filter.go  maps_overlay.go  maps_view.go
gardener_constellation.go            # radial layout (only old /maps/{id} uses it — verify no other caller)
templates/maps_index.html  templates/maps_view.html
assets/static/maps.js
+ all maps_*_test.go
```

Routes to remove in `server.go`:
```go
mux.HandleFunc("GET /maps", s.handleMapsIndex)
mux.HandleFunc("GET /maps/{gardenerID}", s.handleMap)
```
Nav link in `templates/layout.html` (`<a ...>Maps</a>`) → re-point at maps2.

**Verify-before-delete:** grep each file's exported symbols for cross-callers
outside the maps/old set before removing (e.g. `gardener_constellation.go` is also
referenced by `bead_chat.go`'s comments and possibly the bead-show page —
confirm). Keep `/gardeners`, `/gardener/{id}`, and the gardener data spine intact.

## 5. Build order (epic children)

1. **Research** (ka-0000007576) — confirm this reuse map against live code, finalize the delete list, file any sub-tasks. ← the research gardener drives this.
2. **Teardown** (ka-0000000247) — delete old /maps, land green build+lint+test.
3. **Build** (ka-0000005805) — GET /maps2 board + view-model + template + tests.
4. **Promote + nav** (ka-0000002415) — point `/maps` at maps2, update nav.

## 6. Deliberately deferred ("lots of other stuff later") — all shipped (maps2 v2)

v1 shipped the core sentence: **every gardener, its trigger, its recent runs.**
The "lots of other stuff later" roadmap below was the maps2 v2 backlog (epic
ka-0000006790), and as of 2026-06-19 **every item has shipped** — the epic is
fully drained. Each item links the bead + MR that landed it:

- ~~Live-agent overlay on each card~~ (shipped, ka-0000000595 / !4636 — a
  pulsing live dot + working-agent link per card, projected off the same
  last-agent bead the last-run peek already fetched; see `mapsCardView.LiveAgent*`).
- ~~Owned-epic rollups per gardener card~~ (shipped, ka-0000001266 / !4637 — a
  per-card `<closed>/<total> done · <pct>%` drain bar over the gardener's
  `gardener-epic:<id>` subtree, batch-counted to avoid N+1; see `gardeners_owned_epic.go`).
- ~~Cost/context chips~~ (shipped, ka-0000001668 / !4642 — a `$spend` chip off
  the `gardener_spend_usd` roll-up + a context-occupancy mini-bar off the live
  agent; thin view addition, no new pipeline; see `mapsCardView.HasSpend`/`HasContext`).
- ~~Filter rail~~ (shipped, ka-0000008126 / !4638 — a chip-strip facet rail
  (trigger / health / owner / state) over `?filter=` using the `maps_fleet_filter`
  cross-filter wire format; cross-key AND, within-key OR; see `maps_filter.go`).
- ~~Search~~ (shipped, ka-0000006434 / !4640 — a client-side search box filtering
  cards by name/title/goal/owner/id, query persisted in `?q=`; see the
  `data-maps-search` box in `maps.html` + `mapsCardSearchText`).
- ~~Grouping by owner~~ (shipped, ka-0000002262 / !4641 — `?group=owner` buckets
  the standalone card grid into per-owner collapsible sections, reusing the row's
  `OwnerSlug`/`OwnerDisplay`; see `maps_group.go`).
- ~~Trajectory sparkline instead of discrete pills~~ (shipped, ka-0000001693 /
  !4639 — a compact verdict-over-time line drawn from the gardener's full bounded
  run history, with the discrete run pills behind a client-only toggle / small-N
  fallback; see `maps_sparkline.go` + `mapsCardView.Sparkline`).

The graph layer also landed: the gardener `chains_to` workflow rail renders live
on /maps once gardeners declare chain edges (dogfooded in ka-0000002101). Beyond
this roadmap, epic ka-0000006790 carries **standing health duty** — keep /maps
building green, fix regressions, and keep it fast (no N+1 daemon fan-out).
