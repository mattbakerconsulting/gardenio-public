# R5-delta: chart-catalog backend applicability re-check

**Task bead:** ka-0000001813 · **Parent (E0):** ka-0000001798 · **Umbrella:** ka-0000007704
**Re-validates:** `gardenio/docs/research/timeseries-chart-backend-arch.md` (ka-0000004684, dated 2026-04-25)
**Author:** gardenio-r5-applicability-chartcatalog-ljn · **Date:** 2026-05-29

## TL;DR

The R5 architecture is **still substantially correct**: kbeads is the right home, vmsingle is
reachable in-cluster, the `{data, spec, meta}` contract holds, and a templated-catalog v1 (no ad-hoc
PromQL) remains the right scope. **Three pieces of drift matter** and one decision (D1) should be
re-pointed:

1. **The web client changed.** `beads3d` (Vite/JS, vega-embed) is **deprecated**; the live web client
   is **`beads-viewer`**, a Go server-rendered htmx app (`gardenio/controller/cmd/beads-viewer/`). The
   doc's "web renders Vega-Lite via vega-embed" premise no longer matches the dominant surface. This is
   the single biggest applicability hit and it reshapes D1.
2. **The cost metric name/labels differ from what the catalog assumes.** The doc's example PromQL uses
   `gardenio_agent_cost_usd_total{project=...}`. The actual emitted series is
   **`gardenio_session_cost_usd`** with labels **`agent_name, project, account, session_id`** (see
   `session_metrics_push.go`). Any catalog entry written verbatim from the doc would query a metric
   that does not exist.
3. **vmsingle ships but is disabled by default** (`vmsingle.enabled=false`, `vmagent.enabled=false`).
   Reachability is real but gated on R1 turning it on per-env. Not a blocker; a sequencing note.

Recommended D1 (spec format): **keep Vega-Lite as the on-the-wire `spec` for API stability and the
PNG/SVG `vl-convert` fallback, but for the v1 web surface render server-side in beads-viewer** (Go +
inline SVG, mirroring the existing constellation/coverage SVG pattern) rather than shipping vega-embed
into a JS bundle that no longer exists. Treat `spec` as the **portable contract + PNG source**, not as
"the thing the web client parses." This is C′ with the iOS reasoning extended to web.

---

## 1. Claims that still hold

| Claim in R5 doc | Status | Evidence |
|---|---|---|
| Build in `kbeads/internal/server`, one `http_charts.go` + router section | ✅ Holds | Router is a flat `mux.HandleFunc("GET /v1/...", requireRole(RoleViewer, …))` list in `http.go:97-134`. Adding `/v1/charts/*` is a one-block, one-file addition exactly as described. No `http_charts*` file exists yet — greenfield. |
| Mirror `http_events_histogram.go` for the `since`/`until`/`step` query idiom | ✅ Holds | `http_events_histogram.go` is intact: `since`/`until` RFC3339 with defaults, bucket-alias map, server-side aggregation, `{buckets:[...]}` shape. Direct template for the charts handler. |
| Reuse the `fleetCollector` mu+lastFetch+TTL cache idiom for L1 | ✅ Holds | `http_metrics.go` `fleetCollector.cachedSnapshot()` is the exact mu/lastFetch/ttl + stale-on-error pattern. Singleflight/TTL L1 cache can lift this verbatim. |
| Co-locate because charts read **both** VM and the bead DB | ✅ Holds, stronger | beads-viewer already joins beads + a VM-shaped narrative (`incident_intel_gallery.go` carries PromQL strings, `source_kind:"victoria-metrics"`), and `health.go` already consumes the events histogram. Co-location with the bead store is confirmed-cheap. |
| `{data, spec, meta}` wire contract + long-format data + ETag/304 | ✅ Holds | Nothing in current code contradicts it; `data_etag`/`If-None-Match` is consistent with existing viewer endpoints. |
| Auth = existing kbeads ForwardAuth/bearer, project-scoped RBAC | ✅ Holds | Every `/v1/...` route is `requireRole(RoleViewer, …)`; charts slot into the same wrapper with no new auth surface. |
| `vl-convert` PNG/SVG fallback as the iOS escape hatch | ✅ Holds | No native VM/chart renderer exists today; the fallback is still the cheapest path, and beads-viewer already emits inline SVG (`constellation.go`, `coverage.go`), proving SVG-on-the-server is an established idiom here. |
| v1 = templated catalog, **no** ad-hoc PromQL UI | ✅ Holds | No PromQL-proxy or query UI exists in kbeads or the viewer; the operator framing ("curated awesome graphs") is unchanged. Keep ad-hoc out of v1. |

## 2. Drift since 2026-04-25

| Area | Doc assumed | Reality today | Impact |
|---|---|---|---|
| **Web client** | `beads3d` JS, vega-embed, `beads3d/src/api.js` extended with `api.getChart()` | `beads3d` is **DEPRECATED** (root CLAUDE.md). Live client is **`beads-viewer`** — Go, `html/template` + htmx, server-rendered, inline-SVG charts. `beads3d/src/api.js` still exists but is not the surface we ship to. | **High.** §10.4/§10.5 prior-art pointers (beads3d api.js, release-health-page.js, ratings-page.js) are stale. The right prior art is now `beads-viewer/health.go` (histogram consumer), `constellation.go`/`coverage.go` (inline-SVG rendering). Reshapes D1 (see §4). |
| **Cost metric** | `gardenio_agent_cost_usd_total{project}` via `increase(...)` | Emitted series is `gardenio_session_cost_usd` (gauge, per-session) + `gardenio_session_tokens_total{type}` etc., labels `agent_name, project, account, session_id` (`session_metrics_push.go:73-90`). Pushed to vmagent `/api/v1/import/prometheus`, gated on `VMAGENT_URL`. | **High for catalog content, low for architecture.** The `spend-by-agent` PromQL in §6.1 must be rewritten against the real series (sum/topk by `agent_name`, not `agent`; it's a per-session gauge, so spend-over-time needs `sum_over_time`/`increase` care or a recording rule). R3 owns canonicalising this; flag the mismatch. |
| **vmsingle enablement** | "VictoriaMetrics is the storage target … data already (partially) flowing" | `vmsingle.enabled=false` and `vmagent.enabled=false` by default in `values.yaml`. Service template (`vmsingle/service.yaml`) is real and serves `:8428`; fullname helper `gardenio.vmsingle.fullname` → `http://<release>-vmsingle.<ns>.svc:8428`. | **Medium (sequencing).** Reachability is genuine but per-env opt-in. E5 still gated on E1 (VM deployed) as the doc said — confirmed, and the gate is currently *closed* in the default chart. |
| **No VM query client exists** | Implied `charts/vmclient.go` to be written | Confirmed: no `query_range`/PromQL client in kbeads or controller (only the ci-perf skill docs and a hardcoded PromQL string in `incident_intel_gallery.go`). | **Low.** Greenfield as expected; just note nothing to reuse — `vmclient.go` is net-new. |
| **iOS** | Swift Charts via `ios_hint`, R6 to confirm baseline | Unchanged; R6 (ka-0000005542, in progress) owns it. `ios_hint` contract still sound. | None for backend. |

## 3. Shortest path to the v1 deliverable now

Given the drift, the minimum viable v1 that actually lights up a chart on the live surface:

1. **R1 prerequisite:** enable `vmsingle` (and a scrape path: `vmagent` or cluster-Prometheus
   remote_write) in the target env. Until then E5 has no data source. (E5 still gated on E1.)
2. **`kbeads/internal/charts/vmclient.go`** — net-new thin client for VM `GET /api/v1/query_range`
   against `gardenio.vmsingle.fullname` cluster DNS. No reuse available.
3. **`kbeads/internal/server/http_charts.go`** — copy the `http_events_histogram.go` shape:
   `since`/`until`/`step` parsing + defaults, `requireRole(RoleViewer, …)` registration in
   `http.go`. Return `ChartRender{data, spec, meta}`.
4. **L1 cache** — lift the `fleetCollector` mu/lastFetch/TTL idiom; add `singleflight`.
5. **Catalog: start with one real chart.** Use a series that *actually exists today*:
   `gardenio_session_cost_usd` (spend) or `gardenio_session_tokens_total` (tokens), grouped by
   `agent_name`/`project`. **Do not** ship the doc's `gardenio_agent_cost_usd_total` PromQL verbatim.
6. **Web rendering = beads-viewer server-side SVG**, not vega-embed. Add a `/charts`-style route that
   fetches `ChartRender` and renders inline SVG with the existing `constellation.go`/`coverage.go`
   pattern. The `spec` field still ships in the API for iOS/PNG/external consumers, but the web path
   does not depend on a JS Vega runtime that no longer exists.
7. **PNG/SVG via vl-convert** — defer the sidecar to the iOS-fallback milestone; the web path renders
   SVG directly in Go, so vl-convert is not on the v1 web critical path.

This is shorter than the original doc implies for the **web** surface (no JS bundle, no vega-embed
wiring) and unchanged for the **API/iOS** surface.

## 4. Updated decision-point recommendation (D1 — spec format)

**D1 as originally framed:** Vega-Lite spec on the wire, web renders it via vega-embed, iOS native-maps
+ PNG fallback (Option C′).

**Re-pointed recommendation:** **Keep Vega-Lite as the wire `spec`, but demote it from "web renderer
input" to "portable contract + PNG/SVG source."** Concretely:

- **Wire format stays `{data, spec(vega-lite), ios_hint, drill, meta}`** — unchanged. Vega-Lite is
  still the best portable, diffable, vl-convert-compatible spec language. Don't switch to ECharts/raw.
- **Web (beads-viewer): render server-side in Go (inline SVG), driven by `ios_hint`-style semantic
  fields**, not by parsing the Vega-Lite spec in the browser. The web client is now Go-rendered htmx
  with an established inline-SVG idiom (`constellation.go`, `coverage.go`); shipping a JS Vega runtime
  would re-introduce the bundle that the beads3d deprecation removed. This makes the architecture
  symmetric: *both* web and iOS consume the semantic hint; the Vega-Lite `spec` becomes the
  cross-surface contract and the PNG/SVG render source rather than any client's live renderer.
- **`vl-convert` PNG/SVG** remains the fallback for surfaces that want a pixel-identical image (iOS
  no-native-kind, Slack unfurls, status pages). Lower priority for v1 since web renders SVG natively.

**Why re-point rather than keep C′ verbatim:** C′'s logic — "spec is a semantic *contract*, not a
shared *renderer*, because no faithful Vega-Lite renderer exists on the target" — was applied to iOS.
The beads3d→beads-viewer migration makes the **same argument true for web**: we no longer have a
vega-embed renderer on the dominant surface, and standing one back up is cost we removed on purpose.
So extend C′'s own reasoning to web. The API contract does not change; only which client "owns a Vega
runtime" changes (answer: none, in v1).

**Net:** D1 = "Vega-Lite as portable spec/contract + vl-convert image source; server-side SVG on web,
Swift Charts on iOS, both driven by the semantic hint." This is strictly *less* implementation than the
original (no JS renderer to wire) and keeps every future option open (a JS vega-embed surface can be
added later without an API change).

---

## 5. Verdict

R5's backend architecture survives re-validation: **kbeads placement, the `{data,spec,meta}` contract,
the histogram/fleetCollector prior-art, templated-catalog v1 scope, and ForwardAuth reuse all still
hold.** Two content-level corrections are mandatory before implementation — (a) the web renderer is
beads-viewer server-side SVG, not beads3d/vega-embed, and (b) the cost series is
`gardenio_session_cost_usd{agent_name,project,…}`, not `gardenio_agent_cost_usd_total` — and one
sequencing note: vmsingle ships disabled, so E5 is gated on R1 enabling it per-env. D1 is re-pointed to
"Vega-Lite as portable contract + PNG source; no JS renderer on web in v1."
