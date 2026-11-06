# Plan-of-Record: gardenio time-series metrics + chart platform (v1)

**Synthesis bead:** ka-0000000386 · **Umbrella epic:** ka-0000007704
**Status:** ✅ **SHIPPED** (updated 2026-08-06) — was: plan-of-record pending operator decision.
**Author:** gardener (thread-1777141509-460959), 2026-04-25.

> ## ✅ This plan was approved and built — E1–E8 all closed 2026-06-05
>
> The "pending operator decision before any implementation epics get created"
> status below is **historical**. The operator chose to build it; every epic
> E1–E8 shipped and v1 is live in production. Mapping:
>
> | Epic | Landed as |
> |---|---|
> | E1 vmsingle + vmagent | live in `helm/gardenio` (now also vmalert + VictoriaLogs + federation) |
> | E3 spend-as-counter | MR !3580 · `controller/cmd/beads-viewer/costs_rate.go` |
> | E4 beads-events counters | `kbeads/internal/metrics/beadevents.go` (+ `events_backfill.go`) |
> | E5 chart catalog `/v1/charts` | MR !3571 · `kbeads/internal/server/charts_catalog.go` |
> | E6 beads-viewer web charts | `controller/cmd/beads-viewer/charts.go` + ECharts |
> | E8 dashboards + polish | `helm/gardenio/dashboards/` |
>
> The **decision points in §4 are resolved** (the fleet executed the recommended
> defaults). Follow-on work now lives in separate live epics: ka-0000000741
> (upstream metrics), ka-0000004205 (customer-1), ka-0000006272 (E2 v1.1 metric
> renames), ka-0000005064 (account-scoped chart catalog). The rest of this
> document is preserved as the original design record.

This consolidates the six R-tasks (R1–R6) into a sequenced v1 implementation
plan. Every claim here has a source: see `gardenio/docs/research/timeseries-*.md`.
A reader should grok the v1 architecture and sequencing in 10 minutes.

---

## 1. Architecture summary

```
┌─────────────────────────────── gardenio namespace ────────────────────────────────┐
│                                                                                  │
│  PRODUCERS                            COLLECTOR              STORE               │
│  ───────────                          ─────────              ─────               │
│  controller :9092/metrics  ─scrape─►                                             │
│  kbeads     :9091/metrics  ─scrape─► vmagent (1)  ──remote-write──► vmsingle (1) │
│  coop       :9093/metrics  ─scrape─►   filter:gardenio_*/beads_*/    50Gi gp3     │
│                                       kbeads_*                      retain 1y    │
│  agent pod (gb session                                                           │
│   close push)              ─push──►                                              │
│                                                                                  │
│                                                                  ▼               │
│                                            CHART CATALOG  ──query── vmsingle     │
│                                            kbeads /v1/charts/{id}                │
│                                            { data, spec, meta }                  │
│                                                  │                               │
│                                                  ▼                               │
│              ┌───────────────────────────────────────────┐                      │
│              ▼                                            ▼                      │
│        beads-viewer (web)                          Slack unfurl /              │
│        ECharts via                                 server PNG                  │
│        chart partial                               (vl-convert / vega-pong)    │
│              │                                            ▲                      │
│              └────── hourly-rate, daily/weekly windows ───┘                      │
│                       — one data contract, web surface                           │
└──────────────────────────────────────────────────────────────────────────────────┘
```

**Single paragraph:** vmsingle (single-node, in-namespace, 1y retention, 50 Gi
gp3) stores `gardenio_*` / `beads_*` / `kbeads_*` long-retention metrics. vmagent
sits in front and (a) scrapes existing `/metrics` endpoints (controller, kbeads,
new coop endpoint) and (b) accepts the existing `gb session` push at session
close. A new **chart catalog** service in **kbeads** exposes named, parameterised
charts at `/v1/charts/{chart_id}` returning `{ data, spec, meta }`. The web
client (beads-viewer htmx) calls the catalog and renders the spec via a JS
chart lib. A server-rendered PNG fallback (vl-convert) covers Slack unfurls and
chart shapes the web adapter can't yet produce. The umbrella's v1 metrics —
**spend per agent per hour** and **beads events by type** — drop straight onto
this stack: spend is enriched into a true counter (`gardenio_agent_spend_usd_total`)
fed by coop's NATS `usage` events, beads-events become `gardenio_beads_events_total`
counters incremented inside kbeads at event-emission time. Operator constraint
ka-0000001797 — *hourly rate, daily/weekly rolling windows for web* — is
enforced by the chart-catalog contract and one PromQL transform
(`increase(...[1h])`) under the hood.

**Why this shape (and what we are not doing):** all infrastructure is
**already plumbed** — the gardenio Helm chart ships vmsingle / vmagent /
Grafana templates gated by `*.enabled: false`; the controller and kbeads
already expose `/metrics`; the agent already pushes per-session metrics on
close. v1 is enabling what is wired, adding three thin pieces (coop /metrics,
event counters in kbeads, a chart-catalog handler in kbeads), and writing the
web client that renders the contract. We do **not** build a generic ad-hoc
PromQL UI, do not deploy vmcluster, do not deploy vmalert, do not deploy
vmbackup, and do not stand up a separate chart-rendering service.

---

## 2. Implementation epics (proposed)

Seven epics, each one-paragraph charter. Sized to ship v1 in roughly the listed
order; E1+E2 are the critical path; E3–E5 fan out in parallel; E6 is the
client work; E8 is polish.

### E1 — vmsingle + vmagent enabled (the storage layer)

Enable the Helm-chart-shipped `vmsingle` + `vmagent` in `gardenio-test` →
`gardenio-rwx` → `gardenio-prod`, sequenced. PVC 50 Gi `gardenio-storage`,
retention `1y`, single replica each, in-namespace only (no Ingress on either).
Add a single relabel on vmagent's remote-write to keep only `(gardenio|beads|
kbeads|vmagent|vmsingle|process)_.*` metric names — this protects the
long-retention store from accidental pollution. Grafana enabled in dev/rwx,
deferred in prod (register vmsingle as a datasource in the ACME observability
Grafana instead). Owner: caller-side change to `acme-helm-chart/charts/gardenio/
values/<env>.yaml`; gardenio repo is untouched. **Source:** R1 §4.

### E2 — Metric inventory cleanup + emitter additions

Three threads in one epic because they share files:

1. **Cleanup**: rename `gardenio_agent_*_total` poller-gauges to
   `*_current` (Prometheus convention — `_total` ⇒ counter only); normalise
   the kbeads `gardenio_fleet_*` vs `beads_*` prefix split to `gardenio_*`;
   add the missing `env` label to gauges that today alias across envs.
2. **New emitters in Go services**: add `gardenio_agent_lifecycle_events_total`,
   `gardenio_agent_pod_phase_transitions_total`, `gardenio_agent_session_duration_seconds`
   counters/histograms in controller, hooked to the existing event paths.
3. **coop `/metrics`**: add `metrics-exporter-prometheus` to `coop/crates/cli`,
   listen on `:9093`, scrape annotation, expose
   `gardenio_agent_tool_call_latency_seconds` and `gardenio_agent_llm_request_latency_seconds`
   histograms. **Source:** R2 §2 + R2 §3.

### E3 — Spend-as-counter (per-agent, per-hour ready)

Make "spend per agent per hour" a first-class counter rather than a polled
gauge. Two changes: (a) enrich coop's existing `messages.{actor}.usage` NATS
event with `model` and `response_cost` (LiteLLM already injects
`response_cost` into the JSONL — coop just doesn't forward it); (b) add a
small NATS→vmagent forwarder in the controller (or kbeads) that subscribes
to `usage` events and emits
`gardenio_agent_spend_usd_total{agent,project,role,model,account}` as a
Prometheus counter. Per-hour spend then = `increase(gardenio_agent_spend_usd_total[1h])`.
Keep the existing 5-min `spendSync` against LiteLLM `/key/info` as a
reconciliation backstop, and the session-close push as the per-session
truth. **Source:** R3 TL;DR + §"current-state map".

### E4 — Beads-events counters at the source

Increment `gardenio_beads_events_total{type,project,…}` inside kbeads at the
same place the event is published — right next to `store.RecordEvent()` /
`eventbus.Bus.Dispatch()`, **after** the txn commits. No second consumer, no
JetStream durable, no postgres dedup state. v1 covers the high-traffic
families: `bead.created`, `bead.closed`, `bead.updated` (broken out by
inferred sub-type — `claimed`, `released`, `status_changed`),
`comment.added`, `dependency.added`, `dependency.removed`, `decision.created`,
`decision.responded`, `agents.AgentStarted/Stopped/Crashed`, plus a generic
catch-all bucket for the long tail. Adds an optional postgres backfill path
that replays the existing `events` table into vmagent's `/api/v1/import`
to seed historical buckets. **Source:** R4 §4.1 + §5.

### E5 — Chart catalog backend (the kbeads /v1/charts/* handler)

Build a thin **chart catalog** in `kbeads/internal/server` exposing
`GET /v1/charts/{chart_id}` returning `{ data, spec, meta }`. The catalog is
a small set of named charts checked into the repo
(`spend-by-agent`, `beads-events-rate`, `agent-cost-per-bead`,
`bead-status-flow`, plus the operator's hourly-rate-with-rolling-window).
Universal query parameters: `since`, `until`, `step`, `window` (1d / 7d
rolling), and chart-specific params (e.g. `agent`, `project`).
Backend translates the params to PromQL, queries vmsingle, and returns a
deterministic data payload + a chart spec. Server-rendered fallback at
`/v1/charts/{chart_id}/png` and `.../svg` for Slack unfurls and the web
PNG fallback path. **Source:** R5 §1 + §2.

> **Spec format is a decision point — see §4.D1.** R5 recommends Vega-Lite;
> R6 recommends a generic series-of-points JSON with web doing ECharts.
> Plan-of-record recommends a hybrid: ship `{data, spec, meta}` where `data`
> is a generic series-of-points (the part the client consumes), and `spec` is
> a Vega-Lite document used by web for rendering and by the server for
> PNG/SVG fallback.

### E6 — beads-viewer chart UX (web)

Three insertion points in beads-viewer: (a) a new `/charts` top-level
dashboard tab in the existing tab strip; (b) per-bead chart panels under
`bead_show.html` for `agent` / `account` / `project` types via a new
`partials/charts.html`; (c) a chart card reused inside the existing
`/costs` page. Renderer: **ECharts** (~150 KB gzipped, plain-JS, strong
dark-mode + a11y + tooltip ergonomics, integrates with htmx via
`hx-get` + `<script type="application/json">` data block hydrated on
`htmx:afterSwap`). Shared time-range picker (1h / 6h / 24h / 7d / custom)
at the top of the dashboard, with a per-chart override badge for charts
that carry an intrinsic time scope (e.g. "today's spend"). All charts
render an empty/loading/error state explicitly — sparse v1 catalog is the
common case, not an edge case. Drill-down maps chart clicks to existing
bead routes (e.g. clicking a `spend-by-agent` slice for an agent →
`/agents/<id>`). **Source:** R6 §1 + §2.

### E8 — Recording rules, dashboards, polish

Author the small set of vmagent recording rules that are demonstrably hot
on real data (do not pre-author speculatively): e.g.
`gardenio:agent_cost_usd:rate1h` and `gardenio:beads_events:rate5m`. Add a
1 Gi `emptyDir` mount on vmagent for `-remoteWrite.tmpDataPath` so the
buffer survives container restarts. Add the two existing chart-pack
dashboards (`cost-overview.json`, `kbeads-embedding.json`) to the ACME
observability Grafana via the observability chart's datasource provisioning
in prod. Backfill `gardenio_beads_events_total` from the kbeads `events`
table for the last 90 days where useful. **Source:** R1 §3 + R1 §4 + R4 §5.

### E9 — beads3d charts page (deferred to v1.1)

Out of v1 scope. Beads3d gets fleet-wide story panels (release-health
shaped) in a follow-up. The day-to-day operator surface is beads-viewer;
beads3d is the "big picture" surface and can lag the v1 by a
release cycle. **Source:** R6 §0.

---

## 3. Sequencing (dependency graph)

```text
       E1 (vmsingle + vmagent)
        │
        ├──► E2 (metric cleanup + emitters)
        │     │
        │     ├──► E3 (spend-as-counter)        ┐
        │     ├──► E4 (beads-event counters)    │
        │     │                                 │
        │     └──► E5 (chart catalog backend) ──┴──► E6 (beads-viewer charts)
        │
        └──► E8 (recording rules + dashboards + backfill, after E2..E5 land)
```

- **E1 unblocks everything.** Without vmsingle running we can't validate
  anything else end-to-end.
- **E2 unblocks E3 / E4 / E5** because the chart catalog and the new
  emitters depend on the cleaned-up metric names.
- **E3, E4, E5 fan out** in parallel; they don't share files except via
  the catalog's PromQL strings, which are easy to update.
- **E6 fans out** off E5, consuming the one chart-catalog data contract —
  this is where the ka-0000001797 hourly-rate / rolling-window bar lands.
- **E8 follows** when there's real data to derive rules from and a
  Grafana to register dashboards into.

**Estimated calendar shape** (rough — gardener estimate, not a commitment):
E1 is days; E2 is a week or so; E3 / E4 / E5 are each 1–2 weeks parallelized;
E6 is 1–2 weeks; E8 trails. Net: a "v1 visible to
operator" milestone in roughly 4–6 calendar weeks if staffed, with the
`spend per agent per hour` chart visible end-to-end at the end of week 3.

---

## 4. Decision points (operator approval before any code)

The plan recommends a default for each; the operator can accept/override.
Each decision name maps to an option that the gardener will offer in
`gb decision create`.

| Id | Decision | Recommendation | Reversible? |
|---|---|---|---|
| **D1** | **Chart spec format**: Vega-Lite (R5) vs series-of-points JSON (R6) vs hybrid | **Hybrid** — `data` is generic series-of-points (the web client consumes); `spec` is Vega-Lite (web renders directly, server uses for PNG/SVG fallback) | Hard — defines the API contract. Pick before E5 starts. |
| **D2** | **vmsingle PVC size + retention**: 25 Gi/90d vs 50 Gi/1y vs 100 Gi/2y | **50 Gi / 1y**, ≈$10/mo | Easy (`gardenio-storage` allows online expansion; can shrink retention later) |
| **D3** | **Prod Grafana**: in-chart vs ACME-shared | **Dev/rwx in-chart, prod ACME-shared** | Easy |
| **D4** | **Beads-events: instrument new counters in kbeads vs derive via vmagent recording rules** | **Instrument in kbeads** (canonical metric exists at scrape time, not after rule eval) | Hard once dashboards land |
| **D5** | **Web chart lib**: ECharts vs Vega-Lite-via-vega-embed vs Chart.js | **ECharts** | Medium |
| **D7** | **beads3d in v1?** | **Defer to v1.1** | Easy (E9 charter is already a follow-up) |
| **D8** | **Backfill scope**: 7d / 30d / 90d / all-time on `gardenio_beads_events_total` | **90d** (matches the longest dashboard window the operator has asked for so far) | Easy |

The gardener will offer **D1 + D2 + D7 first** (those gate the most work),
and either escalate the rest individually as we approach each epic, or roll
them into a single bundled approval at this synthesis decision-gate.

---

## 5. Risks + open questions

Things research did not fully answer, and the leverage points if any of them
shift the plan:

1. **Cardinality explosion via session_id.** The session-close push uses
   `session_id` as a label. At ~5k sessions/year, vmsingle handles this,
   but there is a future risk if session frequency 10×s. Mitigation:
   keep session_id only on the per-session push metrics, not on the
   per-agent rollups; promote to a recording rule that drops session_id
   if active series ever crosses ~100k.
2. **NATS at-least-once vs counter increments.** R4 mitigates this by
   incrementing counters inline with `store.RecordEvent()` after txn
   commit (so dropped/duplicated NATS messages can't double-count). E3's
   spend counter is more exposed — coop's NATS `usage` events are the
   counter source. Mitigation: use the LiteLLM `/key/info` reconciliation
   loop as a sanity check; alert if 1h cumulative drift > 1%.
3. **kbeads becoming the chart-catalog backend** couples chart query
   logic to the bead store's release lifecycle. v1 is fine because the
   catalog is small and read-only against vmsingle. v2 may want to split
   the catalog into a separate binary if it grows beyond ~10 chart
   definitions or starts owning auth state. Not a v1 concern.
4. **Operator constraint ka-0000001797 — hourly rate + rolling windows.**
   The plan satisfies it in the web surface via the chart-catalog `data`
   contract and a single PromQL transform (`increase(...[1h])`), with
   operator-selectable daily / weekly rolling windows. A regression here
   would be a window-math test failing — see §6 acceptance criteria.
5. **"Awesome graphs" is a UX promise.** This plan ships chart shapes,
   not motion / interactivity polish. If the operator wants animated
   transitions, brushed-zoom, or per-point exemplars (jump-to-trace), that
   is a v1.1 concern. Flag if the operator's expectation differs.
6. **No alerting in v1.** vmalert is out of scope. If the operator wants
   "page me when an agent's spend crosses $X/hour" that becomes the first
   v1.1 epic. The wire is ready (see R1 §6 — re-use ACME Alertmanager).

---

## 6. v1 acceptance criteria (operator-visible)

- [ ] vmsingle running in `gardenio-prod`, retaining `gardenio_*` /
      `beads_*` / `kbeads_*` for 1y, scraped by an in-namespace vmagent.
- [ ] Operator opens beads-viewer `/charts` and sees an **hourly-rate
      spend-per-agent chart with daily and weekly rolling-window
      controls** (the ka-0000001797 constraint).
- [ ] Operator opens a bead like `kd-<agent>` in beads-viewer and
      sees a per-bead chart card (cost-over-time / sessions-over-time /
      tool-calls-over-time).
- [ ] `gardenio_beads_events_total{type=…}` is queryable for the last
      90 days, including the backfill from postgres.
- [ ] A Slack unfurl of `https://gardenio-charts.../v1/charts/spend-by-agent.png?...`
      shows a recognisable chart (PNG fallback works).
- [ ] All charts render an explicit empty / loading / error state (no
      blank canvases).

---

## 7. What this plan does NOT cover (and why)

- **vmcluster / HA storage** — out of scope; vmsingle handles ≥1 year at
  current scale (R1 §3 Q1).
- **vmalert / alerting** — explicit v1 deferral (R1 §3 Q6).
- **vmbackup / DR** — explicit v1 deferral (R1 §3 Q8); every metric is
  reproducible.
- **Generic ad-hoc PromQL UI for clients** — explicit non-goal (R5 §1
  Option A's worst feature is leaking PromQL into product surfaces).
- **beads3d charts** — deferred to v1.1 (E9 / D7).
- **Per-tool / per-message granularity** — would 10× series count;
  revisit only when a use case demands it.
- **Anthropic billing API integration** — every API call already routes
  through LiteLLM, which is the cost source-of-truth (R3 TL;DR).

---

## 8. Successor work after operator approval

1. Gardener creates implementation epics E1–E8 as children of umbrella
   `ka-0000007704` with the charters above.
2. Gardener files the small successor tasks already enumerated by the
   research (R1 §6 `T-vm-1..9`, R2 §"open questions", R3 §"work items",
   R4 §"open questions", R5 §"open questions", R6 §10).
3. **No code lands** until E1 is ready and the operator has signed off
   D1 (chart spec format) — that is the choice the rest of the chart
   plumbing depends on.

---

## 9. Sources

- `gardenio/docs/research/timeseries-victoriametrics-ops.md` (R1, ka-0000000179)
- `gardenio/docs/research/timeseries-metrics-catalog.md` (R2, ka-0000007788)
- `gardenio/docs/research/timeseries-spend-attribution.md` (R3, ka-0000001029)
- `gardenio/docs/research/timeseries-beads-events-metrics.md` (R4, ka-0000002387)
- `gardenio/docs/research/timeseries-chart-backend-arch.md` (R5, ka-0000004684)
- `gardenio/docs/research/timeseries-charts-ux.md` (R6, ka-0000005001)
- Operator constraint: ka-0000001797 (hourly rate, daily/weekly rolling windows — web)
- Umbrella epic: ka-0000007704
