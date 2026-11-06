# Research: gardenio health page in beads viewer

**Bead:** ka-0000008186 · **Slack thread:** thread-1777555375-303019 · **Author:** gardener (thread-1777555375-303019), 2026-04-30.
**Status:** research / design only — no code lands until operator picks D1+D2 below.

---

## AMENDMENT (2026-04-30, post-operator-decision)

Operator (Matt) resolved the surface decision in Slack:

> *"htmx (beads-viewer) ONLY. beads3d is largely deprecated (you should note this)."*

Implications for the rest of this doc:

- **Surface change:** every reference to **beads3d** as the host for the
  health page is superseded. The v1 health page lives on the **htmx
  beads-viewer** (`gardenio/controller/cmd/beads-viewer/`) as a new
  `/health` route, rendered with the same `html/template` + `hx-get`
  pattern as the existing pages there (active, agents, releases, ratings).
- **What carries over unchanged:** the 7-tile content design (§4), the
  data shape (§6), the snapshot endpoints (§3), and the operator-visible
  acceptance criteria (§12). Only the rendering surface moves.
- **What is dropped:** the `beads3d/src/health-page.js` page-module
  pattern (§5) and the HUD-button/overlay UX. Replace with: a Go handler
  in `beads-viewer`, a per-tile `html/template` partial, and `hx-get`
  polling per tile (30 s) — matching the rest of the htmx app.
- **D2 (data source) unchanged:** still a snapshot endpoint each from
  kbeads (T1: `ka-0000000587`) and controller (T2: `ka-0000006360`).
  The htmx handler in beads-viewer reads those, not Prometheus directly.
- **T3 (`ka-0000005766`) and T4 (`ka-0000000085`)** bead descriptions
  have been retargeted to beads-viewer — see those beads for the
  updated scope.
- **D3, D4, D5** recommendations (24h trend tiles for events + incidents,
  30 s polling, incident definition = release bugs + agent crashes +
  stuck gates) all still apply on the new surface.
- **Note on beads3d deprecation:** the operator's standing direction is
  that beads3d is largely deprecated. New operator-facing surfaces in
  this monorepo should default to beads-viewer (htmx) unless there is a
  specific reason force-graph context is required.

The text below is preserved verbatim as the original research baseline;
read it for the *what* and *why* of each tile, but treat any "beads3d"
reference as "beads-viewer (htmx)" per this amendment.

---

## 0. The brief, in one sentence

The operator (Matt) asked, in Slack:

> *begin research for a gardenio health page to be put in beads viewer based on
> our recent metric graph work. Research and plan, create epics and tasks.*

This doc takes that brief, reads it against what already exists in this repo,
and proposes a small, shippable "is the gardenio platform OK right now?" page
that drops onto **the existing beads3d viewer** (the same surface that hosts
release-health, ratings, my-work today), reuses the metric work that has
already merged, and stays inside the design envelope of the larger
time-series + chart platform plan-of-record (ka-0000000386) so that nothing
shipped here has to be torn out when the chart catalog lands.

---

## 1. TL;DR

- **Surface = beads3d.** "Beads viewer" in this thread maps to the beads3d
  3D-force-graph viewer (the surface that already hosts
  `release-health-page.js`, `ratings-page.js`, `my-work-page.js`), not the
  htmx beads-viewer. Recent metric-graph work — the events histogram endpoint
  (ka-0000004198) — was explicitly built to back the beads3d sequencer
  heatmap (ka-0000002251). The health page is the next page module along
  that same axis. **§2.**
- **Recommendation: a single overlay page ("Health") wired into the existing
  HUD button strip, parallel to "releases" / "ratings" / "my work".** New
  page module `beads3d/src/health-page.js`, hash route `#health`, HUD button
  `hud-btn-health`. Uses the same overlay open/close pattern as
  `release-health-page.js` (≈250 LOC). **§4.**
- **What it shows: 7 health "tiles", each a small chart or stat block.** Picked
  for **operator-actionable signal** at a glance, not metric exhaustiveness.
  Each tile maps to data we already emit:
  1. **Fleet liveness** — agents running / idle / crashed / OOM'd, last 30 min.
  2. **Decision queue depth** — pending / total, with rolling rate.
  3. **Bead-event throughput** — sparkline from `/v1/events/histogram` (already
     shipped).
  4. **Spend rate** — USD/hour, last 24 h (via existing `gardenio_agent_cost_usd`
     gauges or LiteLLM reconciliation, depending on D2).
  5. **Account health** — throttled / warming / OK; per-account utilisation
     bars.
  6. **Daemon + bridge health** — kbeads `/v1/health`, slack-bridge SSE last
     heartbeat, last fleet-collector scrape time + scrape-error count.
  7. **Recent incidents** — last 5 release bugs (release-health summary card)
     + last 5 agent crashes (controller event filter).
  **§5 + §6.**
- **Data source for v1: a small read-only `/v1/health/snapshot` JSON aggregator
  on kbeads, plus a parallel `/healthz/snapshot` on the controller.** Not a
  chart-catalog. Not vmsingle/vmagent. Returns one bounded JSON payload sized
  for the page. Two thin endpoints we own, no infra dependency. Refreshed
  every 30 s via SSE (reuses the existing `/api/events` mutation stream as a
  trigger; no new SSE channel). When the umbrella time-series plan
  (ka-0000000386) ships E5 (chart catalog backend), the health page swaps to
  catalog `chart_id`s without changing the UI. **§7.**
- **Rendering: vanilla canvas + SVG, no chart lib in v1.** The page already
  has the FPS-sparkline canvas pattern in `main.js:363` (~30 LOC). The
  release-health page is a styled HTML table (~250 LOC). Reuse both. ECharts /
  chart-catalog rendering arrives with E6 of ka-0000000386; until then a
  dependency-free shim keeps bundle size flat and avoids a forced redesign
  when D1 (Vega-Lite vs series-of-points) lands. **§8.**
- **Effort: one researchable epic + four implementation tasks (~1 week
  staffed).** No new infra, no new K8s objects, no Helm changes. **§9.**
- **Decisions for the operator (5).** D1 surface (beads3d only vs also htmx),
  D2 data source (snapshot endpoint vs scrape live `/metrics`), D3 page scope
  (just-now health vs include 24 h trend), D4 refresh model (SSE vs polled
  fetch), D5 incident definition (which event kinds count). **§10.**

The rest of this doc backs these recommendations.

---

## 2. Surface disambiguation — which "beads viewer"?

This repo has two surfaces both colloquially called "beads viewer":

| Surface | Path | Stack | Today | Fits "health page"? |
|---|---|---|---|---|
| **beads3d** | `beads3d/` | Vite + plain JS + three.js + 3d-force-graph | Overlay pages: `release-health`, `ratings`, `my-work`, `decisions`, `agents`, sidebar, filter dashboard, FPS sparkline | **Yes — primary recommendation.** Existing precedent, recent metric work targets it, same overlay pattern. |
| **beads-viewer** | `gardenio/controller/cmd/beads-viewer/` | Go html/template + htmx + plain CSS | iOS-parity tab strip (`/active`, `/decisions`, `/agents`, `/advice`, `/schedules`, `/costs`), per-type detail templates | Yes for a v1.1 mirror — the htmx viewer's `/costs` tab is the closest analog and would inherit the same data contract. |

Three pieces of evidence point at **beads3d** as the right v1 surface:

1. **The recent metric-graph work was for beads3d.** kbeads
   `/v1/events/histogram` (ka-0000004198, merged 2026-04-25) explicitly says
   *"backs the beads3d sequencer heatmap (parent epic ka-0000002251)"*. The
   sequencer page is itself an under-construction beads3d page. The health
   page is in the same family.
2. **beads3d already has the overlay-page pattern.**
   `beads3d/src/release-health-page.js` (249 LOC) is a near-perfect template:
   HUD button → hash route → overlay open/close + escape-key → fetch → render
   into `<div id="release-health-page">`. We copy it.
3. **The htmx viewer's tab strip is busier and more constrained.** Adding a
   "Health" tab there competes with the existing list-shaped tabs; adding it
   as an overlay in beads3d doesn't.

The operator can override at D1 (§10) — they may want both, or htmx instead.
This doc's primary recommendation is beads3d-first; htmx mirror is a v1.1
follow-up sized in §9.

---

## 3. What "recent metric graph work" already covers

Inventory of metric-graph-shaped work merged in the last ~3 weeks (the
"recent" the operator referenced). All of these are **inputs** to the health
page; none of them are blockers for it.

| Item | Bead | Merged | What it gives the health page |
|---|---|---|---|
| Events histogram endpoint `GET /v1/events/histogram` | ka-0000004198 | 2026-04-25 | Bead-event throughput sparkline (tile 3). 1m/5m/15m/1h/1d buckets, server-side aggregation, bounded payload. |
| operated_by observability metrics + Grafana dashboard | ka-0000007803 / P6a | 2026-04-29 | Pattern for "metric → dashboard" instrumentation we mirror for health-page-specific metrics. Adds 5 new counters in kbeads (header arrival, write classification, resolver errors, fallback outcome, ambiguous agent). The dashboard JSON in `gardenio/helm/gardenio/dashboards/operated-by.json` is the design vocabulary. |
| Fleet collector — SQL aggregation rewrite | ka-0000006525 | 2026-04-29 | The fleet-collector now does aggregation at the DB level, fixing the OOM in ka-0000004022. The health page can call the underlying `Store.FleetSnapshot` shape via a thin wrapper without re-implementing the aggregation. |
| ListBeads default + max limit cap | ka-0000006889 | 2026-04-29 | Defends the snapshot endpoint from accidentally large queries. |
| Operated_by UX in beads-viewer | ka-0000004608 | 2026-04-29 | Precedent for landing a metric-shaped surface in a viewer. |
| Time-series + chart platform plan-of-record | ka-0000000386 | 2026-04-25 | The umbrella plan that the health page must fit inside without violating. **Specifically:** the health page must not lock us into a chart spec that conflicts with D1 of that plan (Vega-Lite vs series-of-points hybrid). v1 stays renderer-agnostic by using vanilla canvas / SVG. |

Existing **Grafana dashboards** (in `gardenio/helm/gardenio/dashboards/`) are the
operator's current power-user health surface:

| Dashboard | Panels | Audience |
|---|---|---|
| `cost-overview.json` | 14 panels — total cost, active agents, cost rate, top spenders, account utilisation, throttle/warming timeline, budget thresholds | operator power-user via Grafana |
| `kbeads-embedding.json` | embedding pipeline — duration p50/p95, errors, search duration | platform engineer |
| `operated-by.json` | rollout state + audit-row writes + resolver health + fallback outcomes + ambiguous detections | platform engineer during operated_by rollout |

The health page is **not a Grafana replacement.** Grafana stays. The health
page is a "**operator's first 30 seconds**" view for the on-call /
day-to-day operator — answers the question *"is anything visibly broken right
now?"* without leaving the viewer they're already in.

---

## 4. Page architecture (beads3d)

Mirror the existing `release-health-page.js` structure exactly:

```text
beads3d/
├── index.html
│   ├── #health-page (overlay container — same CSS scaffolding as #release-health-page)
│   └── HUD: <button id="hud-btn-health">health</button>
└── src/
    └── health-page.js  (new — ~250 LOC)
        ├── setHealthPageDeps({ api, showStatusToast })
        ├── openHealthPage() / closeHealthPage()
        ├── renderHealth(container, snapshot)
        └── tile renderers:
            ├── renderFleetLiveness(...)
            ├── renderDecisionQueue(...)
            ├── renderEventThroughput(...)
            ├── renderSpendRate(...)
            ├── renderAccountHealth(...)
            ├── renderDaemonBridgeHealth(...)
            └── renderRecentIncidents(...)
```

`main.js` wiring (~10 LOC, alongside the existing release-health block at
`main.js:3857`):

```js
import { setHealthPageDeps, openHealthPage, closeHealthPage } from './health-page.js';
setHealthPageDeps({ api, showStatusToast });
const hudBtnHealth = document.getElementById('hud-btn-health');
if (hudBtnHealth) hudBtnHealth.onclick = () => openHealthPage();
if (window.location.hash === '#health') setTimeout(() => openHealthPage(), 100);
window.addEventListener('hashchange', () => {
  if (window.location.hash === '#health') openHealthPage();
  else if (document.getElementById('health-page')?.classList.contains('open')) closeHealthPage();
});
```

Layout: 7 tiles in a CSS grid, breakpoint to single-column under ~640 px.
Header bar with title + close + last-refresh timestamp + manual-refresh
button. Each tile has a fixed footprint (240 × 160 px target), an explicit
empty / loading / error state, and links to the deeper view (e.g. tile 1
links to `#agents`, tile 2 to `#decisions`, tile 7 to `#release-health`).

**Why an overlay rather than a side-panel or sidebar tab.** The HUD button
strip is the established pattern for full-screen-modal-style operator views.
Health is operator-mode, not graph-mode. An overlay also keeps the 3D scene
paused under the hood (we already do this for release-health) so the page
doesn't fight for GPU.

---

## 5. The seven tiles — picked for operator-actionable signal

Each tile answers one question the operator asks daily.

### Tile 1 — Fleet liveness ("are agents OK?")

**Question.** How many agents are alive, idle, crashed, OOM'd, deferred, in
the last 30 min?

**Render.** Five-stat bar (running / idle / crashed / OOM / deferred) +
30-min sparkline of "active agents" with a faint band for the daily median.

**Data.** `Store.FleetSnapshot` (already exists; refactored 2026-04-29
ka-0000006525) gives `activeAgentsByProject`. Crash / OOM counts come from
the controller's recent agent-bead status transitions. Deferred count comes
from `gardenio_reconciler_agents_deferred_total` (already exposed at
`/metrics`).

**Drill-down.** Click → `#agents` (existing agents page).

### Tile 2 — Decision queue ("is anything waiting on me?")

**Question.** How many decisions are pending? How fast is the queue draining?

**Render.** Big "N pending" stat + 30-min sparkline of queue depth + a "next
3 decisions" mini-list with bead links.

**Data.** `GET /v1/decisions?status=open` (already exists) + a new
trailing-30-min counter sampled by the snapshot endpoint.

**Drill-down.** Click → `#decisions` (existing decision center).

### Tile 3 — Bead-event throughput ("is the fleet doing work?")

**Question.** Are we receiving events? Did we just go quiet?

**Render.** Bar-histogram, 30 buckets × 1m, from `/v1/events/histogram`
(already shipped). Vertical reference line at "median per minute over last
24 h". Color-grade bars: gray = quiet, green = normal, amber = surge.

**Data.** `GET /v1/events/histogram?since=<now-30m>&until=<now>&bucket=1m`.
Already returns the right shape.

**Drill-down.** Click → opens the sequencer (ka-0000002251) when it ships;
until then, deep-links to `#filter` with the time range pre-filled.

### Tile 4 — Spend rate ("are we burning money?")

**Question.** USD/hour right now? Trending up or down vs typical?

**Render.** Big stat ($X/hour) + 24 h area chart of cumulative cost vs a
faint dashed "yesterday" line.

**Data.** Sum of `gardenio_agent_cost_usd` (existing gauge) divided by the
window. v1 reads from a server-side derivation in the snapshot endpoint to
keep the client-side compute trivial. v1.1 (when E3 of ka-0000000386 lands)
swaps to `increase(gardenio_agent_spend_usd_total[1h])` against vmsingle.

**Drill-down.** Click → `#costs` in beads-viewer (existing) **or** the
chart-catalog `spend-by-agent-1h` chart when ka-0000000386 E5 lands.

### Tile 5 — Account health ("are any anthropic accounts throttled?")

**Question.** Which accounts are throttled / warming / clear?

**Render.** Per-account row: name + 5 h utilisation bar + 7 d utilisation
bar + status pill (OK / warming / throttled).

**Data.** `gardenio_account_throttled`, `gardenio_account_warming`,
`gardenio_account_utilization_5h`, `gardenio_account_utilization_7d` —
all already emitted by `gardenio/controller/internal/metrics`. Snapshot
endpoint returns one row per account.

**Drill-down.** Click → `#agents?account=<name>`.

### Tile 6 — Daemon + bridge health ("is the platform itself OK?")

**Question.** Is kbeads up? Is the slack-bridge consuming SSE? Is the fleet
collector scraping?

**Render.** Three stat-with-pill rows:
- kbeads: "OK" / "degraded" / "down" + last `/v1/health` ping time.
- slack-bridge: "live" / "stalled" + last SSE heartbeat. Stalled if >60 s
  since last `messages.*` event.
- fleet-collector: "fresh" / "stale" + last scrape time. Stale if >5×TTL
  since `lastScrapeTime` gauge tick.

**Data.** `GET /v1/health` (exists), bridge gets a `last_event_at`
field added to the snapshot endpoint, fleet-collector reuses the existing
`gardenio_fleet_metrics_last_scrape_timestamp` gauge.

**Drill-down.** Stale / down rows expand to show the most recent error from
`scrape_errors_total` or the bridge's error log via SSE.

### Tile 7 — Recent incidents ("did anything just break?")

**Question.** Any release failures, agent crashes, decision misfires, or
gates stuck in the last 24 h?

**Render.** Compact list — last 5 `release:<ver>` bugs + last 5 agent
crashes + count of stuck decisions. Each row links to its bead.

**Data.** Reuse `release-health-page.js`'s
`_api.list({ type: 'bug', search: 'release', limit: 500 })` (already cached
client-side for ~5 min). Crash list comes from
`api.list({ search: 'crashed', since: '24h' })` filtered to agent kinds.

**Drill-down.** "View all" → `#release-health`.

---

## 6. Tile-to-data-source matrix (cheat-sheet)

| Tile | Existing data | New plumbing needed |
|---|---|---|
| 1 Fleet liveness | `Store.FleetSnapshot`, `gardenio_reconciler_agents_deferred_total` | Snapshot endpoint joins these into one shape |
| 2 Decision queue | `GET /v1/decisions?status=open` | 30-min queue-depth time-series in snapshot endpoint |
| 3 Event throughput | `GET /v1/events/histogram` (shipped) | None |
| 4 Spend rate | `gardenio_agent_cost_usd` gauges | Snapshot endpoint sums + time-bucketed cost (cheap; values already cached) |
| 5 Account health | `gardenio_account_*` gauges | Snapshot endpoint flattens per-account |
| 6 Daemon + bridge | `GET /v1/health`, `lastScrapeTime` gauge, bridge log | Snapshot endpoint records bridge `last_event_at`; trivial |
| 7 Recent incidents | `api.list({type:bug,search:release})` (existing release-health), bead-event search | Reuse existing client-side caches |

**Net new server work:** one `GET /v1/health/snapshot` endpoint in kbeads
(reads existing collectors + adds the bridge `last_event_at`) + one
`GET /healthz/snapshot` mirror on the controller for things only the
controller knows (deferred count, account utilisation aggregations). Both
are bounded JSON, both are read-only, both are cacheable for ~10 s
server-side.

---

## 7. Data source decision: snapshot endpoint vs live scrape vs vmsingle

Three options for where the health page reads from. Recommendation:
**option B (snapshot endpoint).**

### Option A — client scrapes `/metrics` directly

**Shape.** beads3d fetches `/metrics` from kbeads and the controller, parses
the Prometheus text format in JS, picks the lines it needs.

**Pros.** Zero new endpoints. Always-fresh data.

**Cons.**
- Auth — `/metrics` is on the internal port (`:9091`/`:9092`), not exposed
  through the same ingress as the API. Either we expose it externally (bad —
  metrics endpoints leak), or we proxy it (more work than option B).
- Parsing Prometheus text format in browser-side JS is fiddly and brittle
  against label-set changes.
- The fleet collector's per-scrape work would happen on every health-page
  open, undoing the ka-0000006525 aggregation fix.

**Verdict.** No.

### Option B — kbeads `/v1/health/snapshot` + controller `/healthz/snapshot` (RECOMMENDED)

**Shape.** Two thin handlers return one bounded JSON each:

```jsonc
// GET /v1/health/snapshot   (kbeads)
{
  "schema_version": 1,
  "now": "2026-04-30T14:05:00Z",
  "fleet": {
    "agents_running": 12,
    "agents_idle": 3,
    "agents_crashed_30m": 1,
    "agents_oom_30m": 0,
    "active_by_project": [{"project": "gardenio", "count": 8}, ...]
  },
  "decisions": {
    "pending": 4,
    "depth_30m": [{"t":"...", "depth": 3}, ...],
    "next_3": [{"id":"kd-...", "title":"...", "age_seconds": 120}, ...]
  },
  "events": { "histogram": [{"t":"...","count":42}, ...] },
  "incidents": {
    "recent_release_bugs": [{"id":"kd-...", "version":"2026.120.1", ...}, ...],
    "recent_agent_crashes": [...]
  },
  "kbeads": { "ok": true, "last_health_at": "..." },
  "fleet_collector": { "ok": true, "last_scrape_at": "...", "scrape_errors_total": 2 }
}
```

```jsonc
// GET /healthz/snapshot   (controller)
{
  "schema_version": 1,
  "now": "...",
  "spend": { "usd_per_hour_now": 4.87, "usd_24h_cumulative": [...] },
  "accounts": [{"name":"acct-a","throttled":false,"warming":false,"util_5h":0.42,"util_7d":0.31}, ...],
  "deferred_agents": 0,
  "slack_bridge": { "ok": true, "last_event_at": "..." }
}
```

**Pros.**
- One round-trip per surface (page calls both in parallel).
- Bounded, auth'd, observable as a regular API call.
- Server-side cacheable for 10 s — opening the page repeatedly costs ~nothing.
- Survives the eventual vmsingle/vmagent migration: when E5 of ka-0000000386
  lands, this endpoint gets re-implemented as a thin caller of the chart
  catalog, with the same JSON shape, no client change.

**Cons.**
- Two new endpoints to write and test.
- Schema versioning needed if we want to evolve the shape without breaking
  cached pages.

**Verdict.** Yes.

### Option C — wait for vmsingle / vmagent (umbrella E1)

**Shape.** Health page uses chart-catalog `chart_id`s once E1+E5 of
ka-0000000386 land.

**Pros.** Most architecturally pure; everything is a chart.

**Cons.** Blocks on the broader plan; the operator wants this **now**, not
in 4–6 weeks.

**Verdict.** Defer to v1.1. Health-page v1 becomes a fast follower when E5
ships.

---

## 8. Rendering: vanilla canvas + SVG, no chart library in v1

Three reasons.

1. **The codebase already has the pattern.** `main.js:363` is a 30 LOC
   canvas-based FPS sparkline. We extend that into a tiny `tile-chart.js`
   helper exposing `drawSparkline`, `drawBars`, `drawAreaFill`. ~80 LOC.
2. **No bundle-size hit.** beads3d already ships ~600 KB (three.js) — adding
   ECharts (~150 KB) doubles the sparkline-only footprint of the health
   page. Vanilla canvas is free.
3. **Renderer-agnostic.** D1 of the umbrella plan (Vega-Lite vs
   series-of-points hybrid) is unresolved. If we ship ECharts now and the
   operator picks Vega-Lite-via-vega-embed later, we throw away ECharts. If
   we ship vanilla canvas now, we throw away ~80 LOC.

When the chart catalog lands (E5/E6 of ka-0000000386), each tile becomes a
`chart_id` lookup. The renderer is then whatever the catalog returns —
vanilla canvas, ECharts, or Vega-Lite. The page doesn't know.

---

## 9. Effort + sequencing

One epic + four implementation tasks. Sized for ~1 week parallelizable
across two crews; serial it's ~5–7 working days.

```text
E-health (epic) — gardenio health page on beads3d
   ├── T1 — server: GET /v1/health/snapshot in kbeads (~1 day)
   ├── T2 — server: GET /healthz/snapshot in controller (~1 day)
   ├── T3 — beads3d: health-page.js + tile renderers + index.html scaffold (~2 days)
   └── T4 — beads3d: e2e + visual baseline + CSS polish (~1 day)
   └── T5 — docs + Slack bridge field for last_event_at (~0.5 day)
```

T1 and T2 are independent and can run in parallel. T3 depends on **the JSON
shapes** from T1+T2 (not on them being deployed); we draft those in T1+T2's
PRs first so T3 can stub against them. T4 and T5 follow T3.

**Critical path is T1 + T3 + T4** — about a week.

A v1.1 follow-up:
- **htmx mirror.** A `/health` tab in beads-viewer that consumes the same
  snapshot endpoints. Sized at ~3 days; gated on D1 (operator may want only
  one surface).
- **Chart-catalog migration.** When ka-0000000386 E5 lands, replace the
  snapshot-endpoint reads with chart-catalog reads. Sized at ~2 days; not on
  the v1 critical path.

---

## 10. Decision points (operator approval before code lands)

Each decision names the option the gardener will offer in `gb decision
create`. **The gardener will offer D1+D2 first** (those gate the most work);
the rest can be rolled into a single bundled approval at the
`begin-coding` gate.

| Id | Decision | Recommendation | Reversible? |
|---|---|---|---|
| **D1** | **Surface scope**: beads3d only, beads-viewer (htmx) only, or both | **beads3d only for v1; htmx mirror as v1.1** | Easy — adding the htmx mirror later is additive |
| **D2** | **Data source**: snapshot endpoint vs live `/metrics` scrape vs wait for vmsingle | **Snapshot endpoint (option B)** — one new kbeads handler + one controller handler, JSON | Medium — schema versioning lets us evolve shape; switching sources is rewriting the handler bodies, not the API |
| **D3** | **Page scope**: just-now health (5-min view) vs include 24 h trend tiles | **Include 24 h** for tiles 3 + 4 (the only ones with trend value); rest stay just-now | Easy — tile-by-tile |
| **D4** | **Refresh model**: client polls every 30 s vs SSE-driven via existing `/api/events` mutation stream | **Client polls 30 s** in v1; SSE upgrade is a v1.1 polish | Easy |
| **D5** | **Incident definition**: which event kinds count for tile 7 (release bugs only? + agent crashes? + stuck gates? + decision misfires?) | **Release bugs + agent crashes + stuck gates** (3 kinds, all already searchable) | Easy — list is data-driven |

---

## 11. Risks + non-goals

**Risks.**
- **Snapshot endpoint cost.** If the operator opens the page 10× / minute,
  cache TTL must protect the underlying queries. Recommendation: 10 s
  server-side cache (matches the fleet-collector TTL).
- **Bridge `last_event_at` introspection.** The slack-bridge does not
  currently emit a "last consumed event" timestamp. Adding it is a small
  change to the bridge's NATS consumer (5–10 LOC). Sized into T5.
- **Tile-renderer drift from chart catalog.** If E5/E6 ship a different
  data shape than the snapshot endpoint, the migration is more than a
  rename. Mitigation: design the snapshot JSON to **already look like** the
  R5 contract (`{ data, spec, meta }`-shaped per tile, with `data` as a
  series-of-points).
- **Operator-actionability vs metric-completeness.** Pull toward
  metric-completeness will make the page another mini-Grafana, which
  defeats the point. The "first 30 seconds" framing is the design rule.
  Anything that fails it is cut.

**Non-goals (and why).**
- **No new metrics emitted.** v1 surfaces what we already emit. New
  metrics live in E2 of ka-0000000386.
- **No alerting.** vmalert is out of scope of the umbrella plan; the
  health page surfaces *current state*, it does not page anyone. (ka-7027f134
  added some alerts under operated_by; those continue to live in
  Alertmanager.)
- **No mobile parity in v1.** iOS health card lives downstream of E7 of
  ka-0000000386. The desktop browser is the v1 audience.
- **No interactive drill-into-time-range.** Tile clicks navigate to
  existing detail surfaces; the page does not introduce a brushable time
  selector. That belongs in the chart catalog UX (E6).
- **No persistence of operator preferences.** No "favourite tiles", no
  "hide tile X". Seven tiles, fixed order, every time.

---

## 12. Acceptance criteria (operator-visible)

- [ ] Operator opens beads3d, presses the **`health`** HUD button, sees the
      Health overlay open within ~150 ms.
- [ ] All 7 tiles render either data or an explicit empty / loading / error
      state — no blank canvases, no half-states.
- [ ] The page refreshes within 30 s of an event (poll v1; SSE v1.1).
- [ ] Each tile has a working drill-down click.
- [ ] Closing with `Escape` or the close button hides the overlay and
      restores the 3D scene.
- [ ] Two new endpoints (`/v1/health/snapshot`, `/healthz/snapshot`) are
      ≤200 ms p95 under default cache TTL.
- [ ] No new bundle bloat (>5 KB minified) in beads3d.
- [ ] All new code passes existing `quench check` + `npm run lint` +
      Playwright suites.

---

## 13. Sources

- `gardenio/plans/timeseries-platform.md` — umbrella plan-of-record, D1
  spec-format decision still open.
- `gardenio/docs/research/timeseries-charts-ux.md` — surface analysis
  (beads-viewer + iOS + beads3d) the health page slots into.
- `kbeads/internal/server/http_events_histogram.go` — already-shipped
  endpoint (ka-0000004198).
- `kbeads/internal/server/http_metrics.go` — fleet collector (post
  ka-0000006525 rewrite).
- `gardenio/controller/internal/metrics/metrics.go` — emitted gauges /
  counters the page reads.
- `gardenio/helm/gardenio/dashboards/cost-overview.json` — Grafana panel
  vocabulary the page mirrors at lower fidelity.
- `gardenio/helm/gardenio/dashboards/operated-by.json` — pattern for
  metric-shaped surface design.
- `beads3d/src/release-health-page.js` — the overlay-page template.
- Slack thread: thread-1777555375-303019 (operator brief).
