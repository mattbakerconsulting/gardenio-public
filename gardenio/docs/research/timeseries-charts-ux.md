# Time-series charts UX — beads-viewer (web) + iOS (Gardenio app)

**Bead:** ka-0000005001 (R6, research). Parent epic: ka-0000007704 (umbrella: gardenio
time-series metrics + chart platform).
**Status:** research/design only — no code, no design files.
**Sibling streams:** R1 VictoriaMetrics ops (ka-0000000179), R2 metrics taxonomy
(ka-0000007788), R3 per-agent spend ingestion (ka-0000001029), R4 beads → metrics
(ka-0000002387), **R5 chart backend architecture (ka-0000004684 — UX choices in
this doc constrain that API)**.

---

## 0. Scope disambiguation (read first)

The bead title says "beads-viewer (web) + iOS" and parenthesises `beads3d/`, but
in this repo two distinct surfaces both get called "the web viewer":

| Surface | Path | Stack | Today | Audience |
|---|---|---|---|---|
| **beads-viewer** | `gardenio/controller/cmd/beads-viewer/` | Go + html/template + htmx + plain CSS | iOS-parity tab strip (`/active`, `/decisions`, `/agents`, `/advice`, `/schedules`, `/costs`), per-type detail templates, custom-views registry, golden-constellation SVG on bead-show | the operator's day-to-day browser |
| **beads3d** | `beads3d/` | Vite + plain JS + three.js + 3d-force-graph (no framework) | 3D force-graph of all beads, deep-link, filter dashboard, ratings page, release-health dashboard | the operator's "big picture" / map view |
| **Gardenio (iOS)** | `usr/Gardenio/Gardenio/` | SwiftUI; tabs: Beads / InProgress / Decisions / Agents / Advice / Costs; `KbeadsClient` over kbeads HTTP; OIDC auth | List + detail views; **no chart UI today** (`import Charts` is absent everywhere; `UsageMonitor` is numeric-only) | the operator on phone / iPad |

The umbrella epic's deliverable list — *"beads-viewer dashboard + per-bead chart
panels"* + *"iOS chart consumption"* — and the operator's brief
(*"awesome graphs on beads viewer and ios"*) name **beads-viewer** (the htmx
viewer) and **iOS** as primary. **beads3d** is a separate surface with separate
ergonomics and is in scope here because it is the natural home for "big-picture
fleet-wide" dashboards. This doc covers all three.

Recommendation if the operator wants to narrow scope at the decision gate: ship
v1 to **beads-viewer + iOS** first (the day-to-day surfaces), with beads3d as a
phase-2 addition for fleet-wide dashboards.

---

## 1. TL;DR

- **Surfaces (web — beads-viewer, htmx).** Three primary insertion points: (a) a
  new `/charts` top-level dashboard tab in the existing tab strip, (b) per-bead
  chart panels under `bead_show.html` for `agent` / `account` / `project`
  beads, (c) a chart partial reused inside the existing `/costs` page (the
  closest precedent in the codebase). All three reuse the umbrella's
  **chart catalog** concept: each chart is a named id (`spend-by-agent-1h`,
  `beads-events-rate`, etc.) that the backend resolves to spec + data.
- **Surfaces (web — beads3d).** One insertion point: a new
  `release-health-page.js`-shaped page module (`charts-page.js`) toggled from
  the existing left sidebar. Beads3d is *not* the natural home for per-bead
  drill-down — that lives in beads-viewer. Beads3d's contribution is fleet-wide
  story panels.
- **Surfaces (iOS).** Two: (a) a new top-level **Charts** tab in the existing
  tab strip alongside Costs, and (b) a per-bead chart card inside
  `BeadDetailView` for the same `agent` / `account` / `project` types as web.
  No chart UI exists today, so we are adding rather than retrofitting.
- **Web chart lib (recommended): ECharts.** Bundle-size cost (~150 KB gzipped
  tree-shaken) is acceptable next to beads3d's existing three.js footprint
  (~600 KB), it has the strongest dark-mode + a11y + tooltip ergonomics of the
  candidates, plain-JS (no React/Vue) integrates cleanly with both viewers, and
  it tolerates a sparse v1 catalog gracefully (good empty/loading behavior).
  Vega-Lite is the runner-up if R5 picks the "shared spec" backend (option C in
  R5's three options); Chart.js is fine for beads-viewer alone but underpowered
  if beads3d and beads-viewer share components.
- **iOS rendering (recommended): native Swift Charts (iOS 16+) over the same
  chart catalog backend, with the chart spec returned as a generic
  series-of-points JSON (NOT Vega-Lite).** Reason: zero existing iOS chart code,
  Swift Charts gives interactivity / dark-mode / a11y / Dynamic Type for free,
  and rendering time-series data on-device avoids server-side image cost and
  retina/scale headaches. Server-rendered SVG/PNG is the **fallback** for
  charts whose spec the iOS catalog doesn't yet implement, not the default.
- **Drill-down model.** Charts emit "filter intents" that map to existing
  beads-viewer / iOS routes. Clicking the *spend-by-agent* slice for agent
  `gardenio-r6-...` navigates to `/agents/gardenio-r6-...` (beads-viewer) or pushes
  the agent detail onto the navigation stack (iOS). Charts never invent new
  filtering UI — they delegate to existing list/detail screens.
- **Time-range picker.** A shared 1h / 6h / 24h / 7d / custom toolbar at the
  top of the dashboard page, plus a per-chart override badge for charts that
  carry an intrinsic time scope (e.g. "today's spend"). On iOS, the same
  control as a `Picker` segmented control under the navigation bar.
- **Empty / loading / error states.** Sparse v1 catalog is the *common* case,
  not an edge case. Every chart renders a `ContentUnavailableView`-equivalent
  placeholder ("no data yet — instrumentation lands in epic XX") rather than a
  jagged near-empty axis. Loading is a skeleton (axes drawn, dashed grid, no
  data). Error is a banner + retry.
- **Open decisions (5).** Listed in §10. The hard ones for the operator:
  (a) **chart-spec contract** between R5 backend and clients — Vega-Lite vs
  series-of-points JSON vs both (this paper recommends series-of-points;
  R5 may push back); (b) **beads3d scope in v1** — include or defer;
  (c) per-bead panels: render inline or a separate `/charts?bead=<id>`
  route; (d) iOS auth path for charts (reuse OIDC, or anon-readable charts);
  (e) chart catalog ownership — versioned in repo or stored as `chart` beads.

The rest of this doc backs these recommendations.

---

## 2. Existing UX patterns in the repo (what to reuse, what NOT to invent)

### 2.1 beads-viewer (htmx)

The viewer already ships several patterns that the chart UX should mirror, not
displace:

- **Tab strip in `layout.html`** — `/active`, `/decisions`, `/agents`,
  `/advice`, `/schedules`, `/costs`. Adding `/charts` as a sibling tab is the
  least-surprising IA move.
- **Per-type detail dispatch** — `bead_show.html` selects a per-type body
  template (`agent`, `decision`, `schedule`, `advice`, `mr`, ...). Per-bead
  chart panels should live as a *partial* (`partials/charts.html`) included
  conditionally from the type-specific bodies that benefit (`agent`,
  `account`, `project`, maybe `mr`). New code path is a `partials/` include,
  not a new top-level template.
- **Card layout convention** — `costs.html` is the closest precedent: a
  summary card at top, then a list of per-thing cards with chip strips and
  inline numbers. The dashboard at `/charts` should adopt the same card-on-card
  pattern (summary "what is this" + grid of named charts), so the viewer's
  visual language stays coherent.
- **htmx swap targets** — interactive controls (time-range picker, drill-down)
  should use existing `hx-get` patterns. A chart partial returns the
  `<canvas>` host + the JSON payload as a `<script type="application/json">`
  block; client JS hydrates on `htmx:afterSwap`. **No new SPA framework.**
- **Constellation precedent** — `constellation.go` server-renders an SVG for
  the per-bead similar+deps graph. This proves the team is comfortable with
  server-rendered visuals when interactivity is not needed. The chart UX
  diverges (we want pan / zoom / hover / drill-down), but the constellation's
  "small SVG inline on bead-show" is the right *container* shape for our
  per-bead charts.
- **No build step on the htmx side** — beads-viewer ships static CSS/JS in
  `templates/static/`. Whichever JS lib we pick must work as a single
  `<script>` tag (UMD or ESM via `<script type="module">`); we are not adding
  a bundler to beads-viewer for v1. ECharts, Chart.js, and Vega-Embed all
  ship usable UMD bundles. (Note: this also bounds bundle size — see §4.)

### 2.2 beads3d (Vite + plain JS)

Beads3d already has the right scaffolding for adding a charts page:

- **`release-health-page.js`** is a clean precedent — a separate page module
  toggled from a sidebar control, with its own data-fetch and render path,
  injected with `setReleaseHealthDeps({ api, showStatusToast })`. A
  `charts-page.js` should mirror this pattern exactly: same dependency-inject
  shape, same lifecycle (`activate()` / `deactivate()` if present),
  registered from `main.js`.
- **Vite bundler is already in place**, so we may use ESM imports of any
  chosen chart lib without touching beads-viewer's "no build" constraint.
- **`api.js`** already centralises kbeads calls. A new `chartsApi` module
  alongside it (or new methods on `BeadsAPI`) is the right place to put the
  chart-backend client.
- **Filter dashboard / left-sidebar** — the existing `filter-dashboard.js` and
  `left-sidebar.js` give us a place to expose "open charts dashboard" without
  inventing new chrome.

### 2.3 iOS (Gardenio app)

The iOS app's tab structure is the load-bearing reference:

- **Tabs today** — Beads (BeadsTab) / InProgress / Decisions / Agents / Advice
  / Costs. Adding a top-level **Charts** tab fits the precedent. Alternative:
  fold charts into Costs (since v1 spend metrics naturally live there) and
  defer a top-level tab until the catalog grows. *Recommendation: separate
  `Charts` tab from day one — Costs stays a per-account list (it is iOS-parity
  with `UsageMonitor`); Charts is the home for cross-cutting time-series.*
- **Detail-view surface** — `BeadDetailView` lives inline inside
  `BeadsTab.swift`. Chart cards go inside `BeadDetailView` for the right bead
  types, not as a separate detail view.
- **`UsageMonitor`** — already polls account data on a timer, exposes an
  `@Published` model. A new `MetricsMonitor` should mirror this: poll the
  chart-catalog endpoint on a timer, expose `@Published` series, drive the
  Charts tab. Same OIDC / KbeadsClient auth path.
- **`KbeadsClient`** — adding a `getChart(id:range:)` method here is the right
  place; iOS does not need a separate "chart client" type.

---

## 3. Surface inventory — what we add, where, and what each looks like

This section is the meat of the deliverable: every place a chart appears, with
mockup-level detail.

### 3.1 beads-viewer (htmx) — surfaces

#### S1. `/charts` — top-level Charts dashboard (NEW route)

A new tab in the strip. Shape, top-to-bottom:

```
┌─────────────────────────────────────────────────────────────────┐
│ Charts        [1h] [6h] [24h] [7d] [custom▾]      refresh ↻     │
├─────────────────────────────────────────────────────────────────┤
│  ┌─ Spend per agent ────────┐  ┌─ Beads events / hour ────────┐ │
│  │  $ stacked area chart    │  │  bar chart by event type     │ │
│  │  legend = agent          │  │  legend = create/claim/...   │ │
│  └──────────────────────────┘  └──────────────────────────────┘ │
│  ┌─ Spend by model ─────────┐  ┌─ Active agents over time ────┐ │
│  │  line chart, $ y-axis    │  │  step line, count y-axis     │ │
│  └──────────────────────────┘  └──────────────────────────────┘ │
└─────────────────────────────────────────────────────────────────┘
```

- **Route**: `GET /charts?range=24h` (server template). Time-range query string
  is the source of truth; the picker buttons are server-rendered links that
  swap the URL. `htmx:afterSwap` re-hydrates each chart with its new payload.
- **Layout**: 2-column responsive grid. Each chart is a `<section
  class="chart-card">` with title, chart canvas, legend, optional drill-down
  affordance.
- **Catalog source**: server reads the chart catalog (R5 decision: bead-stored
  vs config) and emits a `<section>` per chart in the catalog's "dashboard"
  set. Adding a new chart to the dashboard is a catalog change, not a code
  change.
- **Empty v1 reality**: in week 1, only `spend-by-agent` and
  `beads-events-rate` exist. The dashboard renders just those two cards plus a
  banner: *"More charts coming as instrumentation lands. See umbrella epic
  ka-0000007704."* This is by design — sparse v1 is the design point.

#### S2. Per-bead chart panel inside `bead_show.html` (NEW partial)

For these bead types, the existing detail body includes a charts partial:

| Bead type | Chart(s) |
|---|---|
| `agent` | spend (1h/24h/7d), event rate, active windows |
| `account` | spend, throttle events, session count |
| `project` | spend by agent (last 7d), beads created/closed |
| `mr` *(optional v1)* | pipeline durations over time *(deferred — depends on R4 events)* |

Visual: collapsed by default ("**Charts** ▾ 3 charts"), expand to show
inline 3-up grid. Reuses the dashboard's chart-card partial. **No new full
page** — keeps URL = bead URL.

#### S3. Inline charts on existing pages (NEW partial — small)

Two narrow inserts that reuse the same chart-card partial:

- **`/costs`** — single line chart "spend last 24h" above the per-account
  list, summarising the existing numeric summary card visually. **High value
  for v1** because Costs is the most-trafficked tab and its v1 metric is
  shipping anyway.
- **`/agents`** — sparkline next to each agent row showing event rate or
  spend last 24h. **Defer** to phase 2 (sparkline-shaped = different
  rendering profile, not strictly needed for v1).

### 3.2 beads3d — surfaces

#### S4. Charts page module (NEW)

A `charts-page.js` mirroring `release-health-page.js`:

- Toggle from the left sidebar ("Charts" item below "Release health").
- Hides the 3D graph; shows a full-screen pane with the same dashboard charts
  as beads-viewer S1, plus optional fleet-wide variants (e.g. "spend by
  project across the fleet" — beads3d has the cross-project view by nature).
- ESM import of ECharts is fine here (Vite handles bundling).
- **Drill-down**: clicking a bar/series triggers either the existing 3D filter
  ("show only beads matching this filter") or a deep link to beads-viewer. The
  former is more on-brand for beads3d; the latter is simpler. Recommend
  filter-on-3D as the v1 behaviour and a "open in viewer ↗" affordance for
  exits.

#### S5. Per-molecule charts (DEFER)

Open question: should clicking a bead in beads3d's 3D graph show a chart panel
in the existing detail panel? Probably yes, but the per-bead chart logic is
already a beads-viewer concern (S2). Defer until S2 ships and we can decide
whether to embed a small subset of the partial via iframe / inline / link.

### 3.3 iOS Gardenio app — surfaces

#### S6. Top-level Charts tab (NEW)

A sibling of Costs in the tab bar. Shape:

```
┌─────────────────────────────────────────────────────────┐
│ Charts                                  [refresh ⟳]      │
├─────────────────────────────────────────────────────────┤
│ Range: ◉ 1h  ○ 6h  ○ 24h  ○ 7d  ○ custom               │
├─────────────────────────────────────────────────────────┤
│ ┌─ Spend per agent ─────────────────────────────────┐   │
│ │  Swift Charts area chart                         │   │
│ │  agent legend (tap → drill)                      │   │
│ └──────────────────────────────────────────────────┘   │
│ ┌─ Beads events / hour ─────────────────────────────┐   │
│ │  Swift Charts bar chart                          │   │
│ └──────────────────────────────────────────────────┘   │
│ ... (scrollable)                                        │
└─────────────────────────────────────────────────────────┘
```

- **Pattern**: same `ScrollView { VStack { ... } }` as `CostsTab`, with cards
  styled to match `CostAccountCard`.
- **Picker**: `Picker(...).pickerStyle(.segmented)` placed below the
  navigation bar. Stored in `MetricsMonitor.range` (`@Published`); picker change
  triggers `await monitor.refresh()`.
- **Refresh control**: same `.refreshable` pull-to-refresh + toolbar refresh
  button as Costs.
- **Drill-down**: `NavigationLink` from a tapped legend item → existing
  detail (e.g. `AgentDetailView`).

#### S7. Per-bead chart card in `BeadDetailView`

Same logic as S2: for `agent` / `account` / `project` beads, append a
`MetricsCard` view with the bead's relevant charts. Reuses the same
`MetricsMonitor` machinery scoped to that bead.

#### S8. Costs tab inline chart (OPTIONAL v1)

A single `Chart { ... }` above the account list summarising 24h fleet spend.
Direct iOS-parity with beads-viewer S3.

### 3.4 Surface map (one table)

| ID | Surface | Where | Status |
|----|---------|-------|--------|
| S1 | Charts dashboard | beads-viewer `/charts` | **v1** |
| S2 | Per-bead panel | beads-viewer `bead_show.html` (agent/account/project) | **v1** |
| S3a | Inline 24h chart on `/costs` | beads-viewer `/costs` | **v1** |
| S3b | Sparkline on `/agents` rows | beads-viewer `/agents` | defer |
| S4 | beads3d charts page | `beads3d/src/charts-page.js` | phase 2 |
| S5 | beads3d per-molecule chart | beads3d detail panel | defer |
| S6 | iOS Charts tab | `Gardenio/ChartsTab.swift` (NEW) | **v1** |
| S7 | iOS per-bead `MetricsCard` | `BeadDetailView` | **v1** |
| S8 | iOS Costs inline chart | `CostsTab` | optional v1 |

---

## 4. Web chart library — recommendation + survey

**Recommendation: ECharts.** Brief survey of the four candidates the bead
called out, scored on the criteria that matter for *this* repo:

| Criterion | ECharts | Vega-Lite | Chart.js | D3 |
|---|---|---|---|---|
| Bundle size (gzipped, realistic build) | ~150 KB tree-shaken (full ~330 KB) | ~250–300 KB (Vega + Vega-Lite) | ~70 KB | 50–150 KB depending on usage |
| Plain-JS friendly (no React) | yes (UMD + ESM) | yes (Vega-Embed UMD) | yes | yes |
| Works in beads-viewer "no build" world | yes (UMD `<script>`) | yes (vega-embed UMD bundle) | yes | yes |
| Works in beads3d (Vite ESM) | yes | yes | yes | yes |
| Dark-mode | first-class theme API | manual via config | manual via config | manual |
| A11y / keyboard | strong (built-in `aria` exporter, table fallback) | weak | weak | manual |
| Interactivity (hover, zoom, brush) | strong out of box | strong | basic | "build it yourself" |
| Drill-down (event → click handler) | clean `chart.on('click', ...)` | via Vega signals | basic | n/a (hand-rolled) |
| Empty/loading state ergonomics | `loading: true` flag | manual | manual | manual |
| Time-series ergonomics | category + time axis built-in | excellent (declarative) | basic | manual |
| iOS spec re-use (same chart cat → iOS Swift Charts) | needs translation | **same Vega-Lite spec** if iOS impl exists | needs translation | n/a |

**Why ECharts:**

- The single biggest win is **a11y + keyboard + dark-mode for free** with no
  per-chart hand-rolling. beads-viewer already inherits OS dark-mode via its
  CSS; we want charts to follow without a per-chart theme to maintain.
- Bundle size at ~150 KB tree-shaken is acceptable when beads3d already loads
  three.js (~600 KB). beads-viewer is the more cost-sensitive surface
  (no bundler), but the UMD bundle is reasonable and only loaded on
  `/charts`, `/costs`, and bead-show pages.
- `chart.on('click', ...)` produces clean drill-down events that map directly
  to existing route navigation (§5).
- The team has *no* prior framework investment (no React, no Vue), so we are
  not paying a wrapper cost.
- Plays well with both surfaces: UMD for beads-viewer, ESM for beads3d.

**Why not Vega-Lite (still a serious contender):**

- The big win for Vega-Lite is **shared spec across web + iOS** if R5 picks
  the spec-passthrough backend (R5 option C). But there is **no first-class
  iOS Vega-Lite renderer** today (`SwiftVega` and similar are toys; none ships
  charts at the quality of Swift Charts). So the "same spec on iOS" promise is
  *only* via server-rendered images — losing interactivity on iOS, which
  contradicts §5's recommendation.
- Bundle size is twice ECharts'.
- a11y is weaker than ECharts'.

If R5 picks "Vega-Lite spec backend with PNG fallback for iOS", reconsider
this — but R6 recommends iOS keep its own native renderer regardless (§5),
which makes the shared-spec promise much weaker in practice.

**Why not Chart.js:**

- Smallest bundle, but weakest on a11y, drill-down, and time-series. It is the
  right pick if we only ship S1 + S3a (a couple of simple charts) and nothing
  else. As soon as S2 (per-bead panels) and S4 (beads3d) land, the
  feature-gap shows.

**Why not D3:**

- D3 is a kit, not a chart library. Building all chart variants from scratch
  is a 5x time cost vs picking a higher-level library. Reserve D3 for the
  *one* highly custom visualisation we don't have — the constellation SVG
  already shows the team is comfortable hand-rolling SVG when needed; that is
  the right home for D3-style work, not the v1 chart catalog.

---

## 5. iOS rendering path — native Swift Charts (recommended)

Three paths considered:

| Path | Description | iOS interactivity | Server cost | iOS impl cost |
|---|---|---|---|---|
| **Native (Swift Charts, recommended)** | Server returns series-of-points JSON. iOS renders with `Chart { ... }`. | Full (tap, zoom, drag) | Low | Moderate (one `Chart` builder per chart type — line/bar/area covers v1) |
| **Server-rendered PNG/SVG** | Server returns a rendered image; iOS shows `AsyncImage`. | Tap-only (re-fetch with new range) | High (rasterise + scale + cache) | Trivial |
| **Vega-Lite spec → custom renderer** | Server returns a Vega-Lite spec; iOS hand-rolls a renderer. | Theoretically full | Low | High (no maintained Swift Vega impl) |

**Recommendation: native Swift Charts as the default; server-rendered PNG as a
fallback for chart types whose Swift Charts implementation we have not yet
written.** Concretely:

- The chart backend (R5) returns, for each chart id, either:
  - a **`series` payload** — `{type: "timeSeriesArea" | "timeSeriesBar" |
    "timeSeriesLine", series: [{name, points: [{t, v, ...labels}]}]}` —
    rendered natively on iOS with a `Chart` builder, OR
  - an **`image` payload** — `{type: "image", url, width, height,
    altText}` — used only when the chart's `type` is not yet in the iOS
    catalog. Rendered as `AsyncImage`.
- The iOS catalog ships v1 with `timeSeriesArea`, `timeSeriesBar`,
  `timeSeriesLine` builders. Anything richer (heatmap, sankey, candlestick)
  falls through to image.
- **No Vega-Lite on iOS.** The maintenance cost of a Swift Vega renderer is
  too high; existing community impls are not production-grade.

**Cost analysis:**

- *iOS impl cost.* Swift Charts (iOS 16+) has builder syntax that maps cleanly
  to "iterate over series, emit `LineMark` / `BarMark` / `AreaMark`". The v1
  three chart types are ~150 LOC each in Swift. Total: ~500 LOC + a
  `MetricsMonitor` ~150 LOC. Manageable.
- *Server cost.* Swift Charts path requires the chart backend to ship
  series-of-points JSON anyway; this is the cheapest backend response. PNG
  fallback only kicks in for richer chart types (none in v1).
- *Parity cost.* Web (ECharts) and iOS (Swift Charts) render different
  artifacts from the same series JSON. They will look subtly different. We
  accept this — the alternative (server-rendered PNG everywhere) is a much
  worse iOS experience and degrades web interactivity.

**iOS reality check** (the bead asked for this):

- Swift Charts requires iOS 16+. The Gardenio app's deployment target should
  already be at or above this; if not, that is a separate decision (operator
  call).
- Swift Charts does not natively support all ECharts behaviours (e.g.,
  brushing, complex tooltip layouts). Acceptable — v1 chart catalog uses only
  axes, legends, tap-to-drill.
- Dynamic Type and VoiceOver are first-class in Swift Charts; we get those
  for free.

---

## 6. Drill-down model — clicks navigate to existing routes

The pattern: charts emit a structured "click intent" with the dimension(s) the
user clicked, and the surface maps that intent to an existing detail route.
Charts never invent new filtering UI.

### 6.1 Click intents

| User action | Intent payload | Destination |
|---|---|---|
| Click a series in legend / area | `{dim: "agent", value: "<agent-id>"}` | `/agents/<agent-id>` (web) / `AgentDetailView` (iOS) |
| Click a bar in beads-events-rate | `{dim: "event_type", value: "claim", t_from, t_to}` | `/active?event_type=claim&since=<t_from>` (web) — *deferred until query strings on `/active` exist; otherwise: open a filtered list URL the existing tab supports* |
| Click a slice in spend-by-model | `{dim: "model", value: "claude-opus-4-7"}` | `/agents?model=<model>` (web) |
| Click a project series | `{dim: "project", value: "gardenio"}` | `/projects/gardenio` if exists, else filtered `/active` |

### 6.2 Implementation contract

- Every chart in the catalog declares a `drillDown` field:
  `{enabled: bool, dim: "agent"|"project"|"model"|"event_type", routeTemplate: "/agents/{value}"}`. Backend is responsible.
- Web chart-card layer reads `drillDown.routeTemplate` and substitutes the
  clicked value, then navigates (`window.location.href = ...` or htmx
  `hx-push-url`).
- iOS does the same with `NavigationLink` resolution: a `ChartIntent` enum is
  switched in the parent view to push the right detail.
- If the destination route doesn't exist yet, the chart simply does not emit
  drill-down; the user gets a tooltip + nothing on click. **No broken links.**

### 6.3 Drill-down depth

V1: one level (chart → detail). Phase 2: drill from detail back to a chart
filtered to that entity (e.g. agent detail page already shows S2 per-bead
chart panel — closes the loop).

---

## 7. Time-range UX

### 7.1 Picker placement

- **beads-viewer**: top of `/charts`, right-aligned, server-rendered link
  group `[1h] [6h] [24h] [7d] [custom▾]`. Server query string `?range=24h`
  drives all charts on the page. Custom opens a small inline `<form>` with
  two `<input type="datetime-local">` fields.
- **beads3d**: same picker, top of the charts page module. ESM-side state.
- **iOS**: `Picker(...).pickerStyle(.segmented)` with the same range options
  under the navigation bar; "custom" pushes a sheet with two `DatePicker`s.
- **Per-bead panels (S2 / S7)**: the panel inherits the surrounding range from
  the URL/state if present, otherwise default to `24h`. A small overlay
  badge on each chart shows the resolved range.

### 7.2 Per-chart override

Some charts carry an intrinsic time scope ("today's spend so far"). For these,
the catalog declares `timeScope: "fixed"`; the global picker is hidden or
shown disabled with a tooltip ("This chart is fixed to today.").

### 7.3 Default range

`24h` for all v1 charts. Operator can override; choice is persisted to
localStorage (web) and `UserDefaults` (iOS) per surface.

### 7.4 What we don't do in v1

- No relative-vs-absolute toggle (all relative — "last X").
- No timezone picker (server uses UTC, surfaces render local time in tooltips
  with explicit "(local)" label).
- No chart-by-chart range override beyond the catalog-declared `timeScope`.

---

## 8. Empty / loading / error states

### 8.1 Empty (the v1 common case)

- **Web**: rendered as an in-card placeholder — axes drawn with dashed grid,
  a centered icon + caption ("No data yet — instrumentation lands in epic
  XX"). NOT an absent or jagged chart.
- **iOS**: `ContentUnavailableView` *inside* the chart card (same pattern
  CostsTab already uses for "No Accounts").
- **Catalog flag**: each catalog entry declares
  `expectedFirstData: "ts" | "tbd"` so the placeholder can be specific
  ("Spend metrics expected after 2026-05-01" vs "TBD").

### 8.2 Loading

- **Web**: skeleton — axes + dashed grid + a subtle pulse animation. ECharts
  has `chart.showLoading()`.
- **iOS**: `ProgressView()` in-card; do not blank the card.

### 8.3 Error

- **Web**: per-card error banner ("Couldn't load chart. Retry ↻"); other
  cards still render.
- **iOS**: same — per-card error banner; `Button("Retry")`.
- **Auth errors** bubble up to the surrounding surface's existing auth-error
  flow (sign-in redirect on web; auth sheet on iOS). Charts don't reinvent
  this.

### 8.4 Partial data

- A chart with some series populated and others empty renders the populated
  series and a footer note ("`agent-foo` has no data in this range").

---

## 9. Constraints this places on the chart backend (R5)

R6 chooses UX before R5 finalises the API. These are the constraints the UX
imposes on R5:

1. **Catalog of named charts, not ad-hoc PromQL.** Every chart referenced by
   the UX is a named catalog entry. Clients address charts by id. *(Aligned
   with R5 question #2 and #6.)*
2. **Series-of-points JSON shape.** The default response is a series-of-points
   payload as in §5 — `{type, series: [{name, points: [{t, v, labels...}]}]}` —
   plus optional metadata (`drillDown`, `timeScope`, `expectedFirstData`,
   `unit`, `format`).
3. **Image fallback supported.** Some chart catalog entries return
   `{type: "image", url, width, height, altText}` for richer chart types not
   yet implemented natively on iOS. *(Constrains R5 to support both response
   shapes — a small lift if R5 picks option A or C; a bigger lift if R5 picks
   pure spec-passthrough.)*
4. **Time-range is a first-class request param.** `GET /charts/<id>?from=<ts>&to=<ts>` (or `?range=24h`).
5. **No client-side PromQL composition.** Clients send chart id + range; the
   backend owns all PromQL. (This locks down R5 question #2 in favour of
   "templated, not ad-hoc".)
6. **Drill-down metadata is in the catalog response.** R5 doesn't need to know
   *how* the client navigates; it just declares the destination route
   template. (See §6.2.)
7. **Empty state is a first-class response, not a 404.** A chart with no
   data returns `{type: "timeSeriesLine", series: [], expectedFirstData: "..."}`
   — clients render the empty placeholder. **Charts with no data are the
   common v1 case.**
8. **iOS does not get a Vega-Lite path.** R5 may still ship Vega-Lite for web
   if the team picks it, but the iOS contract is series-of-points + image
   fallback only.

If R5 lands in conflict with any of these, raise a `gb decision` and reconcile
before either side breaks ground.

---

## 10. Open decisions (to escalate via `gb decision`)

These are the design forks UX research surfaced. Each must be resolved
before implementation epics are written.

### D1. Chart-spec contract between R5 backend and clients

- **Options.**
  - **A. Series-of-points JSON for both web and iOS** (this doc's
    recommendation). Web ECharts hand-renders series; iOS Swift Charts
    hand-renders series. Backend ships one JSON shape.
  - **B. Vega-Lite spec for web; series-of-points (same payload) for iOS.**
    Web renders via vega-embed; iOS uses series only. Backend ships two
    shapes.
  - **C. Vega-Lite spec for web; server-rendered PNG for iOS.** Backend
    ships spec + image. Loses iOS interactivity.
- **Where decided.** R5's deliverable picks the backend; this UX doc tells R5
  what each option costs the clients. R5 owns the formal decision.

### D2. beads3d scope in v1

- **Options.**
  - Ship S4 (beads3d charts page) in v1.
  - Defer beads3d to phase 2; v1 = beads-viewer + iOS only.
- **Recommendation:** defer. beads-viewer is the day-to-day surface; beads3d
  charts are nice-to-have.

### D3. Per-bead panel rendering

- **Options.**
  - Inline partial inside `bead_show.html` (this doc's recommendation).
  - Separate `/charts?bead=<id>` route.
- **Recommendation:** inline. Keeps URL = bead URL, reuses constellation's
  precedent.

### D4. iOS auth path for charts

- **Options.**
  - Reuse `KbeadsClient` + OIDC (this doc's assumption).
  - Anon-readable charts (no auth) — easier for share/export, weaker
    security.
- **Recommendation:** reuse OIDC; revisit if a "share this chart" feature
  becomes a phase-2 ask.

### D5. Chart catalog ownership

- **Options.**
  - Catalog versioned in repo (e.g., `gardenio/controller/internal/charts/catalog.yaml`).
  - Catalog as `chart` beads in kbeads (operator can edit live).
- **Recommendation:** repo for v1. The catalog is a v1 contract surface; the
  operator should not edit it ad hoc until the schema stabilises. Phase 2
  may move to bead-stored once the schema is proven.

---

## 11. References

- **This bead**: ka-0000005001 (R6, research).
- **Umbrella epic**: ka-0000007704.
- **Sibling research**:
  - R1 VictoriaMetrics ops: ka-0000000179
  - R2 metrics taxonomy: ka-0000007788
  - R3 per-agent spend ingestion: ka-0000001029
  - R4 beads → metrics: ka-0000002387
  - **R5 chart backend architecture**: ka-0000004684 — *most relevant; see §9.*
- **Related closed work**:
  - beads-viewer custom-views: ka-0000004871
  - beads-viewer constellation SVG: ka-0000003267
  - beads-viewer iOS-parity phase 2: ka-0000002538
  - beads-viewer human-ratings (style reference): `gardenio/docs/research/beads-viewer-human-ratings.md`
- **Code surveyed**:
  - `beads3d/src/release-health-page.js` (precedent for new beads3d page modules)
  - `beads3d/src/state.js`, `main.js`, `api.js`
  - `gardenio/controller/cmd/beads-viewer/costs.go`, `templates/costs.html`,
    `constellation.go`, `templates/layout.html`
  - `usr/Gardenio/Gardenio/CostsTab.swift`, `BeadsTab.swift`,
    `UsageMonitor.swift`, `KbeadsClient.swift`
