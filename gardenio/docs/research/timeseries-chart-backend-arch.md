# R5 Research: in-house chart backend architecture

**Bead:** ka-0000004684
**Parent epic:** ka-0000007704 — Umbrella: gardenio time-series metrics + chart platform
**Status:** research / read-only
**Author:** gardenio-r5-research-inhouse-qia
**Date:** 2026-04-25

## TL;DR

Build a thin **chart catalog service** in `kbeads/internal/server` that exposes named, parameterised charts as
`GET /v1/charts/{chart_id}` returning `{ data, spec, meta }`. Use **Vega-Lite** as the on-the-wire spec
language; render natively on web (vega-embed) and via **Swift Charts on iOS, mapped from the Vega-Lite spec
by a shared TypeScript→Swift adapter** — with a server-rendered PNG fallback (vl-convert) for chart shapes
the iOS adapter can't produce. Don't proxy raw PromQL to clients; don't build a generic ad-hoc query UI in
v1. Recommended option: **C′ (Vega-Lite spec backend with iOS native-mapping primary, PNG fallback
secondary)**.

The single most contested point is iOS rendering: a faithful in-app Vega-Lite renderer doesn't exist for
SwiftUI. Section 8 ("iOS reality check") lays out the trade. We recommend going native-first on iOS and
accepting that the "shared spec" is really a shared *contract* (semantic mark + encoding) rather than a
shared *renderer*.

This doc is read-only research. No code, no IAM, no scaffolding. Implementation work is gated on the
phase-2 decision against `ka-0000000386` (synthesis).

---

## 1. What does "chart backend" mean here?

The operator brief — *"its own chart backend ... ability to have awesome graphs on beads viewer and ios"* —
is doing two things at once: (1) signalling that we want chart-shaped artifacts the clients can consume
without each implementing PromQL composition, and (2) signalling that the look-and-feel should be
controlled centrally rather than diverging between web and iOS.

Three architectural shapes meet that brief.

### Option A — PromQL passthrough

Backend exposes `GET /v1/charts/query?q=<PromQL>&start=&end=&step=` (or just proxies `/api/v1/query_range`)
and clients render natively.

| Pros                                                                     | Cons                                                                            |
|--------------------------------------------------------------------------|---------------------------------------------------------------------------------|
| Trivial to ship — thin auth/cache wrapper around VictoriaMetrics         | "Chart UI" must be implemented twice (beads3d JS + Swift)                       |
| Maximum flexibility for ad-hoc exploration                               | Client must compose PromQL → leaks observability concepts to product surfaces   |
| Natural for power users / debugging                                      | No "look and feel" centralisation — web and iOS will drift                      |
| Easy to add Grafana later as another consumer                            | Versioning and migration of charts is per-client                                |
| Matches what already exists in `kbeads` (`/v1/events/histogram`)         | Doesn't satisfy operator's "awesome graphs" framing — that's a UX promise       |

### Option B — Server-rendered images

Backend returns SVG or PNG via a headless renderer (vl-convert, go-echarts SSR, gnuplot). Clients display
images.

| Pros                                                                     | Cons                                                                            |
|--------------------------------------------------------------------------|---------------------------------------------------------------------------------|
| Pixel-identical web/iOS                                                  | Loses interactivity (hover, brush, drill-down)                                  |
| iOS becomes trivial — just `AsyncImage`                                  | Re-render on every parameter change → cache thrash, latency cliff               |
| One renderer, one set of fonts, one theme                                | Accessibility regression — chart values aren't in the DOM                       |
| Easy to embed in Slack unfurls, PDFs, status pages                       | Dark/light mode requires duplicate cache entries                                |
| Side-effect: trivially cacheable in CDN                                  | Bigger payload than spec+data for small charts                                  |

### Option C — Vega-Lite spec backend (declarative)

Backend returns `{ data: [...], spec: {<vega-lite>}, meta: {...} }`. Web renders via vega-embed; iOS either
runs a Vega-Lite implementation (poor option, see §8) or maps a known subset of specs to Swift Charts.

| Pros                                                                     | Cons                                                                            |
|--------------------------------------------------------------------------|---------------------------------------------------------------------------------|
| Single source of truth for chart definition                              | iOS native-rendering Vega-Lite ecosystem is essentially absent                  |
| Web gets full interactivity + accessibility for free                     | Spec language complexity — easy to over-spec what iOS can actually mirror       |
| Spec is declarative + diffable — review-friendly in Git                  | Adds a JSON-spec serialisation surface area and a versioning concern            |
| Composes well with vl-convert PNG fallback for stragglers                | Vega-Lite is JS-first; non-JS tooling lags                                      |
| Shares a vocabulary with industry tooling (Vega editor, jupyter)         |                                                                                 |

### Recommendation — **C′ (Vega-Lite spec, iOS native-mapping primary, PNG fallback)**

Variant on C: rather than treating the spec as the *renderer contract*, treat it as the *semantic contract*.
The server picks Vega-Lite because (a) it's a clean declarative format, (b) `vl-convert` gives us a PNG/SVG
escape hatch, and (c) the web client renders it directly. iOS takes the spec and maps a curated subset to
Swift Charts via a small adapter; specs that fall outside the supported subset render as `vl-convert` PNG.

Why this beats pure A or pure B:

- It doesn't push PromQL to clients (Option A's worst feature).
- It preserves interactivity on the dominant viewing surface (web), unlike Option B.
- It avoids the worst version of Option C (shipping a Vega-Lite runtime to iOS) by making PNG the safety
  net rather than the primary path.
- It keeps a clean upgrade story: as Swift Charts coverage grows, the iOS adapter handles more specs natively
  without changing the API contract.

Why not pure C: a faithful Vega-Lite rendering on SwiftUI is not a thing in 2026. We'd be the ones building
it. That is out of scope.

Why not B as primary: charts on a beads-viewer dashboard or a per-bead panel that *can't* be hover-inspected
or drilled-into are not "awesome graphs" in any meaningful sense. We'd ship and immediately want interactivity.

---

## 2. API surface

### 2.1. Catalog endpoints (curated, the v1 default)

```
GET  /v1/charts                       → ChartCatalogList         (registered chart definitions)
GET  /v1/charts/{chart_id}            → ChartRender              (data + spec for one chart)
POST /v1/charts/{chart_id}/render     → ChartRender              (when params don't fit a query string)
GET  /v1/charts/{chart_id}/png        → image/png                (server-rendered fallback)
GET  /v1/charts/{chart_id}/svg        → image/svg+xml            (Slack unfurls, status pages)
```

`chart_id` is a stable slug from the catalog (`spend-by-agent`, `beads-events-rate`,
`agent-cost-per-bead`, `bead-status-flow`). Catalog entries are checked into the repo (see §6).

### 2.2. Query parameters (universal across all catalog charts)

| Param        | Type     | Default       | Notes                                                                  |
|--------------|----------|---------------|------------------------------------------------------------------------|
| `since`      | RFC3339  | `now-24h`     | start of window                                                        |
| `until`      | RFC3339  | `now`         | end of window                                                          |
| `step`       | duration | auto          | bucket width; auto-picked from window length when absent               |
| `tz`         | IANA     | `UTC`         | display TZ; passed through to the spec (does not change the query)     |
| `theme`      | enum     | `auto`        | `auto`/`light`/`dark` — affects spec colours only, not data            |
| `format`     | enum     | `vega-lite`   | `vega-lite` (default) / `data-only` / `png` / `svg`                    |

### 2.3. Per-chart parameters

Catalog entries declare typed params with validation. Example: `spend-by-agent` accepts
`agent`, `project`, `role`, `model`, `top_n`. Types: `string`, `string[]`, `enum`, `int-range`. Param schema
is part of the catalog response so the UI can build pickers.

### 2.4. Response shape — `ChartRender`

```json
{
  "chart_id": "spend-by-agent",
  "title": "Spend per agent (last 24h)",
  "subtitle": "USD, per-hour buckets",
  "params": { "project": "gardenio", "step": "1h", "since": "...", "until": "..." },
  "data": [
    { "t": "2026-04-25T00:00:00Z", "agent": "gardenio-gardener-intake", "spend_usd": 0.42 },
    { "t": "2026-04-25T01:00:00Z", "agent": "gardenio-gardener-intake", "spend_usd": 0.31 }
  ],
  "spec": {
    "$schema": "https://vega.github.io/schema/vega-lite/v5.json",
    "mark": { "type": "line", "point": true },
    "encoding": {
      "x": { "field": "t",        "type": "temporal", "title": "Time" },
      "y": { "field": "spend_usd","type": "quantitative", "title": "USD/hour" },
      "color": { "field": "agent","type": "nominal" }
    },
    "config": { "background": null }
  },
  "ios_hint": {
    "kind": "line-multi",
    "x_field": "t",
    "y_field": "spend_usd",
    "series_field": "agent",
    "y_format": "currency_usd",
    "supports_native": true
  },
  "drill": {
    "axis_x_to": { "type": "open_range", "param_set": ["since","until"] },
    "series_to": { "type": "filter_param", "param": "agent" }
  },
  "meta": {
    "vm_query_count": 1,
    "vm_query_time_ms": 38,
    "data_etag": "W/\"sha256:abc...\"",
    "computed_at": "2026-04-25T18:30:00Z",
    "step_aligned": true
  }
}
```

Notes:

- `data` is in **long format** (one row per (timestamp, series-key)). This format lines up with how Vega-Lite
  consumes `transform.fold`-style data and Swift Charts' `ForEach`-over-series rendering. Wide format
  (`{t, agent_a_spend, agent_b_spend}`) is convenient for spreadsheets and nothing else.
- `spec` is a **Vega-Lite v5** fragment. The server is responsible for `data` injection at render time —
  the spec referenced in the JSON is the spec *with data inlined*, so vega-embed needs zero additional
  fetches.
- `ios_hint` is the iOS adapter's life-saver. It tells the iOS client which Swift Charts shape to draw
  *without parsing the Vega-Lite spec*. If `supports_native: false`, iOS falls back to `GET /png`.
- `meta.data_etag` is what enables `If-None-Match` 304 short-circuits and the cache key in §5.
- `drill` is the structured drill-down contract (§7).

### 2.5. Catalog entry — `ChartCatalogList`

```json
{
  "charts": [
    {
      "chart_id": "spend-by-agent",
      "title": "Spend per agent",
      "category": "spend",
      "supported_surfaces": ["dashboard", "agent-profile", "bead-side-panel"],
      "params_schema": {
        "project":   { "type": "string", "required": false },
        "agent":     { "type": "string[]", "required": false },
        "top_n":     { "type": "int", "default": 10, "min": 1, "max": 50 }
      },
      "default_params": { "top_n": 10 }
    }
  ]
}
```

The catalog endpoint is itself cacheable (`Cache-Control: public, max-age=60`). The web/iOS client uses it
to render chart pickers and to build the "registered surfaces" map.

### 2.6. What we do **not** expose in v1

- `POST /v1/charts/render-spec` (raw Vega-Lite + raw PromQL submission). This is convenient for power users
  but it (a) creates an arbitrary code-execution surface (Vega-Lite expressions, signals) we'd have to
  sandbox, and (b) defeats the operator's "curated awesome graphs" framing. Defer to v2 with an explicit
  decision gate.
- A generic `/v1/promql/query_range` proxy. The closest thing v1 needs is the catalog. Operators who want
  PromQL today can use `kubectl port-forward svc/victoria-metrics 8428:8428` and `vmui` directly.

---

## 3. Where does this service live?

### 3.1. Options

1. **Add `/v1/charts/*` endpoints to `kbeads`** (the existing daemon serving `/v1/beads`,
   `/v1/events/histogram`, `/v1/embeddings`, etc.).
2. **Add to `gardenio/controller`** (the controller HTTP server).
3. **New microservice** (`gardenio-chart-backend`).
4. **BFF route inside beads3d's Vite/Express layer.**

### 3.2. Recommendation — **kbeads** (option 1)

Reasons:

- `kbeads` already terminates auth (ForwardAuth + agent bearer tokens), already has a REST surface that
  beads3d consumes (`/v1/beads`, `/v1/events/histogram`), already has SSE plumbing, and already has working
  caching primitives (the `fleetCollector` TTL pattern in `kbeads/internal/server/http_metrics.go`).
- The chart catalog will need to read **both** VictoriaMetrics and the beads database (e.g., joining
  per-bead labels, agent ownership lookups, project filters). Co-locating with the bead store keeps that
  cheap and consistent.
- One auth/identity story for all read traffic from web + iOS = less surface area, fewer cookies, fewer
  failure modes.
- Matches the existing kbeads HTTP convention: one file `http_charts.go`, one router section, same JSON
  shape conventions, same 304 idiom.

Reasons against the other options:

- **gardenio/controller**: the controller's HTTP surface is already heterogeneous (controller events, slack
  bridge, agent metrics push). Adding chart serving makes its responsibilities even fuzzier. The controller
  is a *reconciler* + agent ops surface; it shouldn't also be the user-facing read API.
- **New microservice**: adds a deployment, a service, an ingress, an auth integration, a Helm sub-chart, a
  release artifact, a CI lane, and a dashboard for itself — for a v1 that's "12 named charts that do PromQL
  and template a JSON spec". Premature service split. Re-evaluate when the catalog has 40+ charts or when
  multiple teams own different chart families.
- **BFF in beads3d Vite layer**: works for web but doesn't help iOS (iOS would have to talk to it through
  some other path). A backend that only one client can use isn't a backend.

### 3.3. Module layout (proposal, not for implementation)

```text
kbeads/
  internal/
    server/
      http_charts.go            # router + handlers
      http_charts_render.go     # data → spec template → response
      http_charts_cache.go      # step-aligned response cache
    charts/
      catalog.go                # in-process catalog registry (loaded from YAML at boot)
      vmclient.go               # VictoriaMetrics client (range + instant queries)
      types.go                  # ChartRender, ChartCatalogEntry, IOSHint, DrillSpec
    chartlib/                   # generic Vega-Lite spec assembly helpers
      line.go
      stacked_area.go
      bar.go
      heatmap.go
  cmd/charts/
    chart-tool/                 # CLI for previewing/exporting catalog charts (dev tool, optional)
gardenio/
  charts/
    catalog/                    # YAML chart definitions, checked in
      spend-by-agent.yaml
      beads-events-rate.yaml
      ...
```

Catalog YAML lives in `gardenio/charts/catalog/` so it's review-friendly and versioned with the rest of the
product spec, and is loaded by `kbeads` at boot from a configurable path (defaults to embedded FS for the
shipped catalog).

---

## 4. Auth

### 4.1. Web (beads-viewer)

Same path as beads-viewer's existing API access:
1. Browser → Tailscale → bridge proxy → kbeads.
2. Bridge applies ForwardAuth (Keycloak OIDC) and injects the user identity header.
3. kbeads enforces project-level RBAC on the chart's `params.project` (when present); charts that aggregate
   across projects require an `observability:read-all` group claim.

No new auth surface needed. Charts are just another `/v1/...` GET.

### 4.2. iOS

The iOS app today (via R6 we'll confirm) exchanges credentials for a bearer token; the same token is
attached to chart requests. Two open points to confirm with R6 / iOS gardener:

- **Tailscale on iOS**: if the app is required to be on Tailnet to reach kbeads, that's the same constraint
  as fetching beads. No change.
- **Image fallback path**: when iOS asks for `format=png`, the response is a binary body — make sure the
  bearer-token auth path the iOS client uses works for non-JSON responses (it should, but verify).

### 4.3. Tokens for chart-only views (status page, Slack unfurls)

Out of scope for v1. If/when needed: signed, expiring chart URLs (`?token=<jwt>` with `aud=chart-render`,
`exp<5m`, `chart_id` and frozen `params` in claims). Document but don't ship.

---

## 5. Caching / cost control

### 5.1. Why caching matters

A single dashboard with five panels = five chart requests. Five users open it = 25. Each request can be
several PromQL queries (multiple series, possibly multiple metrics). Without caching the platform falls
over fast, and chart latency dominates UX.

### 5.2. Two-layer cache

**Layer 1 — VM query result cache (step-aligned, in-memory).**

- Key = `(promql, start_aligned, end_aligned, step)` where `*_aligned` means rounded to the nearest `step`
  boundary using the VM `step` parameter convention.
- TTL = `step` for "until=now" queries (i.e., 1h-step charts cache for 1h; 1m-step charts cache for 1m), and
  effectively unbounded for windows where `until < now - step` (historical, immutable).
- Eviction: LRU with size cap (~256 MB default), expose Prometheus metrics for cache hit rate and bytes.
- This layer is shared across all chart_ids — multiple charts that hit the same PromQL share the cache line.

**Layer 2 — Rendered response cache.**

- Key = `(chart_id, sorted_params, format, theme, since_aligned, until_aligned, step)`.
- Holds the final `ChartRender` JSON (and the PNG/SVG body for the image variants).
- Same alignment + TTL rules as Layer 1.
- Backed by HTTP `ETag` + `If-None-Match`. `data_etag` in `meta` is the strong validator; the response
  emits `Cache-Control: public, max-age=<step-seconds>, stale-while-revalidate=30`.

### 5.3. Step auto-selection

To prevent expensive queries, the server caps `points = (until-since)/step` at a per-chart maximum (default
2000). If the requested `step` would exceed it, the server bumps `step` and adds
`meta.step_promoted_from`. This both bounds VM cost and matches what users actually want — a chart panel
on a phone at 6h window doesn't benefit from 1-minute resolution.

### 5.4. Concurrency / singleflight

When N requests for the same key arrive concurrently, only one runs the underlying VM query; the rest wait
on the result. `golang.org/x/sync/singleflight` is the obvious primitive. The existing `fleetCollector`
already uses a similar pattern (mu + lastFetch).

### 5.5. Backpressure

Per-tenant (or per-user) rate limits on `/v1/charts/*` — start at 60 req/min/user. Any request that would
trigger > N (start at 5) live VM queries returns 503 with `Retry-After`. The chart catalog is curated so
this should rarely fire; if it does, that's a signal the chart was specced too expensively.

---

## 6. Templates / "registered chart" concept

### 6.1. Yes, v1 is templated. No ad-hoc.

A registered chart is a static definition that includes:

- `chart_id` (slug, required, stable across versions)
- `title`, `subtitle_template` (Go text/template)
- `params_schema`
- `default_params`
- `queries[]`: PromQL templates (Go text/template — params injected at render time)
- `transforms[]`: optional reshape steps (e.g., topN by total, fill-gaps, rebin)
- `spec_template`: Vega-Lite spec template
- `ios_hint`: native rendering hint contract
- `drill`: drill-down rules
- `surfaces[]`: where this chart can appear (`dashboard`, `agent-profile`, `bead-side-panel`)
- `auth`: required group/role (default `observability:read-self`)

YAML format example:

```yaml
chart_id: spend-by-agent
title: Spend per agent
subtitle_template: '{{ .params.project | default "all projects" }} · last {{ .window }}'
category: spend
surfaces: [dashboard, agent-profile]
params_schema:
  project: { type: string }
  top_n:   { type: int, default: 10, min: 1, max: 50 }
queries:
  - id: spend
    promql: |
      topk({{ .params.top_n }},
        sum by (agent) (
          increase(gardenio_agent_cost_usd_total{
            project=~"{{ .params.project | default ".+" }}"
          }[{{ .step }}])
        )
      )
spec_template: chartlib/line_multi
ios_hint:
  kind: line-multi
  x_field: t
  y_field: spend_usd
  series_field: agent
  y_format: currency_usd
drill:
  series_to: { type: filter_param, param: agent }
  axis_x_to: { type: open_range, param_set: [since, until] }
```

### 6.2. Why YAML and not Go code

- Reviewable in MRs without compiling.
- Non-developers (the gardener agent, the operator) can edit catalog entries.
- The chart-tool CLI can lint and preview them locally.
- Per-environment overrides become trivial (overlay yaml).

### 6.3. Versioning

Each `chart_id` is stable. **Breaking changes get a new id** (`spend-by-agent-v2`); breaking changes
include: param removal, param semantics change, output schema change. Additive changes (new optional param,
new `meta` field) are not breaking. Dashboards reference `chart_id`s that don't move under them.

### 6.4. Initial v1 catalog (proposed)

| chart_id              | One-line                                              | Powers                              |
|-----------------------|-------------------------------------------------------|-------------------------------------|
| spend-by-agent        | LLM cost per agent over time                          | dashboard, agent-profile            |
| spend-by-project      | LLM cost per project over time                        | dashboard, project-page             |
| spend-by-model        | Cost split by Anthropic model                         | dashboard                           |
| beads-events-rate     | Bead lifecycle event rate by type                     | dashboard, project-page             |
| beads-events-by-state | Open beads by status (stacked area)                   | dashboard, project-page             |
| agent-cost-per-bead   | Avg LLM spend per closed bead by agent                | agent-profile                       |
| bead-status-flow      | Sankey-ish: claim → in_progress → closed cohorts      | dashboard                           |
| controller-recon-lat  | Controller reconcile latency (p50/p95/p99)            | platform admin dashboard            |
| bridge-msg-rate       | Slack bridge inbound/outbound message rate            | platform admin dashboard            |

R2 (metrics taxonomy) and R3/R4 (specific emitters) constrain which of these we can ship in the first
implementation pass; this list is the *catalog goal*, not a v1.0 must-ship.

---

## 7. Drill-down model

### 7.1. The contract

Each chart can declare zero or more **drill targets**. Click handlers on the client consume them. The
backend doesn't redirect — it provides the structured intent.

```json
"drill": {
  "axis_x_to":   { "type": "open_range",  "param_set": ["since","until"] },
  "axis_y_to":   { "type": "open_range",  "param_set": ["min_y","max_y"] },
  "series_to":   { "type": "filter_param","param": "agent" },
  "datum_to":    { "type": "open_bead",   "field": "bead_id" },
  "panel_to":    { "type": "open_chart",  "chart_id": "spend-by-model", "param_map": {"project":"project"} }
}
```

### 7.2. Drill action types

- `open_range` — clicking-and-dragging on an axis sets two params (typically `since`/`until`).
- `filter_param` — clicking a series legend or data point sets one param to that value (typically narrows
  by `agent`, `project`, `model`).
- `open_chart` — clicking opens another chart, mapping selected params through.
- `open_bead` — clicking a datum opens the bead detail view (when the chart is bead-keyed, e.g.
  per-bead spend).
- `open_url` — escape hatch, e.g. Grafana link for power users.

### 7.3. Why this layer is on the backend

If drill-down is encoded in the client, web and iOS will diverge. If it's in the spec
contract, both clients implement the same five `type`s and behaviour stays consistent. New drill kinds
require coordinated changes to backend + both clients, which is the right cost — drill-down semantics are
product decisions, not UI fluff.

### 7.4. URL state

Drill targets that change params should also be reflected in the URL (`?since=...&agent=...`). beads3d
already has URL-state plumbing for filters; the chart panel reuses that pattern.

---

## 8. iOS reality check

The `area:observability` brief makes "and ios" sound like a small footnote. It is not.

### 8.1. State of the iOS Vega-Lite ecosystem (2026)

There is no production-grade Vega-Lite renderer for SwiftUI/UIKit. Options that have been tried elsewhere:

- **WKWebView with vega-embed**: works, but launches a JS engine for every chart, adds 3–6 MB of JS, has
  poor scrolling integration, and looks like a webpage embedded in an app. Bad UX.
- **Compile Vega-Lite to JSON spec → custom Swift renderer**: writing one is a multi-quarter engineering
  project. Several open-source attempts exist in 2026; none are mature enough to bet on.
- **Server-rendered PNG via vl-convert**: works, looks identical to web, loses interactivity.

### 8.2. Why we recommend native-on-iOS with Swift Charts + spec mapper

Apple's Swift Charts (iOS 16+) is a SwiftUI-native chart library with line/bar/area/point/rule marks, axis
formatters, accessibility labels, gestures, and dark-mode support. It covers ≥80% of what our v1 catalog
will draw. The iOS adapter:

1. Reads `ios_hint.kind` from the response.
2. Maps it to a Swift Charts view (`.lineChart(.multi)`, `.barChart`, `.areaChart(.stacked)`, etc.).
3. Uses `data` rows directly (long format → SwiftUI `ForEach`).
4. Uses `ios_hint.y_format` to pick a Formatter (currency, decimal, percent).
5. If `ios_hint.supports_native == false`, falls back to `GET /v1/charts/{id}/png?...` and renders
   `AsyncImage`.

### 8.3. Spec-as-contract, not spec-as-renderer

The Vega-Lite spec on the wire is *what the chart looks like on the web*. The iOS client does **not** parse
it. The `ios_hint` block is the iOS equivalent contract. This means:

- Adding a new `ios_hint.kind` is a 3-step coordinated change: chartlib helper, iOS adapter, Swift Charts
  view. We expect this to happen ~once a quarter, not weekly.
- The PNG fallback is the safety net — if iOS hasn't shipped support yet for a new `kind`, the catalog
  entry author flags `supports_native: false` and the backend serves the rendered image. No iOS app
  release is blocked on a new chart shape.

### 8.4. The "Vega-Lite swift impl" temptation

If a contributor proposes building a native Vega-Lite renderer for SwiftUI as part of this work — push
back. It is an order of magnitude more complex than the iOS adapter described above and adds a
maintenance burden we cannot afford. Re-evaluate only if the v1 catalog grows to dozens of weird-shape
charts (it won't).

### 8.5. Risks to flag for R6 (UX) and the iOS gardener

- iOS 16 minimum (Swift Charts requirement) — confirm app baseline.
- Accessibility: `ios_hint` should carry `accessibility_summary` so VoiceOver gets a useful description.
- Dark mode: iOS will use system theme; the Vega-Lite `theme=dark` query param should match what iOS
  passes when in dark mode (so PNG fallback colours match the rest of the screen).
- Offline: charts are read-only and not cached client-side in v1. Acceptable.

---

## 9. Server-side rendering (PNG/SVG) — do we ship vl-convert from day one?

### 9.1. Yes, but only as a fallback path.

`vl-convert` is a Rust binary that compiles a Vega-Lite spec + data to SVG/PNG with no JS engine. Sidecar
in the kbeads pod (or a small standalone deployment) at `:8001`, kbeads calls it for `format=png|svg`.

### 9.2. Why vl-convert and not the alternatives

- `vega-cli` is Node-based; we'd ship Node in the chart sidecar.
- `go-echarts` SSR generates ECharts SVGs, which would diverge from the web's Vega-Lite rendering.
- `gnuplot` is a different chart family entirely.
- Python `altair` could render but means shipping Python.

vl-convert is the same spec language, fast, single binary, mature.

### 9.3. When PNG fires in v1

- iOS adapter has no native `kind` for the chart → request `format=png`.
- Slack unfurl bot wants an image → request `format=png` (out of scope for v1, plan ahead).
- Status pages / weekly emails (if/when) → request `format=png`.

### 9.4. When PNG **doesn't** fire

For web/beads-viewer, never. Don't request PNG from web unless the user explicitly hits "export as image".

---

## 10. Existing prior art to reuse

### 10.1. `kbeads/internal/server/http_events_histogram.go`

This is the closest existing endpoint to what we're building. Pattern to mirror:

- `GET /v1/events/histogram?since&until&bucket=1h` returns `{ buckets: [{t, count}, ...] }`.
- Bucket aliases (`5m → minute`) handled server-side; clients pass user-friendly tokens.
- Server-side aggregation cost is bounded, not pushed to clients.

For charts, we'll have a similar query-param idiom (`since`/`until`/`step`) and similar bucket-alias logic
for `step` values that don't align with VictoriaMetrics' native units.

### 10.2. `kbeads/internal/server/http_metrics.go` (`fleetCollector`)

The cache pattern here (mu, lastFetch, snapshot, TTL) is exactly the singleflight-with-TTL idiom we want
for the L1 cache. Don't reinvent it.

### 10.3. `gardenio/controller/cmd/gb/session_metrics_push.go`

Already pushes session usage metrics to vmagent. Confirms VictoriaMetrics is the storage target and that
the data we want to chart is already (partially) flowing. Coordinate with R3 for spend-attribution scope.

### 10.4. `beads3d/src/api.js`

The chart client library on the web side should be a small extension of `BeadsAPI` —
`api.getChart(chartId, params)`, `api.listCharts()`. Reuses the existing token/reauth/SSE plumbing.

### 10.5. `beads3d/src/release-health-page.js`, `beads3d/src/ratings-page.js`

These show beads-viewer's existing pattern for non-graph "data pages" — separate route, fetch-on-mount,
render. The chart panels follow the same shape. Don't introduce a new framework or bundler concept.

### 10.6. Custom-views (ka-0000004871) and bead-show constellation (ka-0000003267)

R6 owns the UX surface decisions, but for the backend: if "custom views" gain a "saved chart layouts"
concept, the chart catalog already declares which surfaces a chart supports (`surfaces[]`). Adding a
"custom-view" surface is a metadata change, not an API change.

### 10.7. `gardenio/controller/cmd/gb/session_usage.go` — cost data shape

Confirms the cost metric shape (USD, by session/agent/model/project). Charts touching cost should reuse
the same labels (R3 will canonicalise this).

---

## 11. Recommendation summary

| Decision                           | Recommendation                                                              |
|------------------------------------|-----------------------------------------------------------------------------|
| Architecture                       | Option C′: Vega-Lite spec backend, iOS native-mapped, PNG fallback          |
| Service placement                  | New `/v1/charts/*` endpoints inside `kbeads` (no new microservice)          |
| API style                          | REST, catalog-driven, `chart_id`-keyed                                      |
| Catalog format                     | YAML in `gardenio/charts/catalog/`                                           |
| Wire format                        | `ChartRender = { data, spec, ios_hint, drill, meta }`                       |
| Web rendering                      | vega-embed (Vega-Lite v5)                                                   |
| iOS rendering                      | Swift Charts via `ios_hint` adapter; PNG fallback via vl-convert            |
| Caching                            | Two-layer step-aligned cache + ETag/304 + singleflight                      |
| Auth                               | Existing kbeads ForwardAuth/bearer; project-scoped RBAC on chart params     |
| Drill-down                         | Structured `drill` block in response, five action `type`s                   |
| Server image rendering             | vl-convert sidecar, PNG/SVG only when explicitly requested                  |
| Ad-hoc PromQL UI                   | Out of scope for v1 (operator's framing is curated charts)                  |

---

## 12. Open questions / decision-required points

These are the points the synthesis (ka-0000000386) needs to surface to the operator. Each is a `gb decision`
candidate at synthesis time, not now.

1. **Confirm Option C′ over A or B.** The strongest counterargument to C′ is "B is simpler, charts on iOS
   can be PNGs and that's fine." If the operator is genuinely OK with non-interactive iOS charts, B becomes
   the right choice and the iOS adapter goes away.

2. **iOS app baseline / SwiftUI-or-UIKit / iOS 16 minimum.** R6 + iOS gardener own this; the chart backend
   answer flips between "Swift Charts" and "we need a UIKit chart lib" if iOS 16 is not the floor.

3. **Catalog versioning policy.** "New `chart_id` for breaking change" is the recommendation; the
   alternative is "version field in catalog, deprecation window". Go with the former unless there's a
   migration tool we want to avoid (TBD).

4. **Where does authn for "share a chart externally" land?** Out of v1, but we want to know if there's a
   pending need (status page? Slack unfurl bot in scope of phase 2?).

5. **vl-convert deployment shape.** Sidecar in kbeads pod vs separate Deployment. Probably sidecar — cheap,
   no new service. Confirm during R1 ops planning.

6. **Multi-tenant chart authorisation.** Today kbeads is single-tenant per cluster. If gardenio goes
   multi-tenant in the canonical-prod direction (cf. the parent canonical-prod work), `params.project` →
   tenancy enforcement becomes a hard requirement. Flag for the canonical-prod plan-of-record, not for
   timeseries v1.

7. **Spec safety surface.** Vega-Lite supports expression strings (e.g. `expr.signal`). Even in templated
   v1, we should bound this — strip or whitelist `expr` constructs in spec templates. Note for
   implementation; not a v1 architecture decision.

8. **Time-series TZ handling.** Server returns timestamps in UTC; client display TZ is `tz` param.
   Confirm with R6 that beads-viewer's existing TZ behaviour matches.

9. **Recording rules vs query-time aggregation.** R1 (VM ops) owns this. The chart backend can handle
   either (PromQL queries don't care), but expensive charts (like `bead-status-flow`) probably want
   recording rules. Coordinate.

10. **Embedded charts in Slack messages / notifications.** Out of v1; if pursued, builds on the PNG path.

---

## 13. Coordination notes for R1–R6 siblings + synthesis

- **R1 (VictoriaMetrics ops)**: confirm vmselect endpoint + auth path; confirm range-query step contract;
  confirm whether vmagent + vmselect run in the same namespace as kbeads (so the chart backend can talk
  via cluster DNS without an Ingress).
- **R2 (metrics taxonomy)**: catalog entries depend on stable label sets. Charts are useless if labels
  drift.
- **R3 (spend attribution)**: `spend-by-agent` etc. depend on the spend metric shape R3 lands. The catalog
  entries listed in §6.4 are placeholders pending R3.
- **R4 (beads events)**: `beads-events-rate` depends on R4's emitter design.
- **R6 (charts UX)**: §2 API surface, §7 drill-down, and §8 iOS reality check are all UX-bound. Treat this
  doc as the *backend's view* of the API contract; R6 refines surface placement and interaction.
- **Synthesis (ka-0000000386)**: this doc proposes E5 (chart backend) is gated only on E1 (VM deployed).
  E5 is independent of E2–E4 emitters except for the catalog content (an E5 implementation can ship with a
  catalog of zero charts and grow as emitters land).

---

## 14. Out-of-scope (for explicitness)

- Ad-hoc PromQL UI
- Alerting / threshold UI on charts
- Authoring tooling / a "build your own chart" UI
- Embedding charts in Slack messages (deferred)
- Shared signed-URL chart embeds (deferred)
- Charts over non-time-series data (e.g. embeddings projections, beads graph stats — those use existing
  beads-viewer surfaces)
- Multi-tenant authz beyond `params.project` filtering
