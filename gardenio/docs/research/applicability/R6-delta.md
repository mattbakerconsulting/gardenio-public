# R6-delta: charts UX applicability re-check

**Task bead:** ka-0000005542 · **Parent (E0):** ka-0000001798 · **Umbrella:** ka-0000007704
**Re-validates:** `gardenio/docs/research/timeseries-charts-ux.md` (ka-0000005001, dated 2026-04-25)
**Parity bar:** ka-0000001797 · **Plan-of-record:** `gardenio/plans/timeseries-platform.md` (E6/E7, D5/D6)
**Author:** gardenio-r6-applicability-charts-9st · **Date:** 2026-05-29

## TL;DR

R6's core UX recommendations **still hold**: a new `/charts` tab + per-bead panel + `/costs`
inline chart on web; a new **Charts** tab + per-bead card on iOS; **ECharts** for web (D5) and
**native Swift Charts** for iOS (D6); a shared series-of-points `data` contract with drill-down
to existing routes. The insertion points are all still greenfield (no chart code exists on any
surface). **Five pieces of drift matter**, and one cross-stream conflict with the freshly-landed
**R5-delta** must be reconciled before E5/E6 charters are written:

1. **beads3d is now DEPRECATED** (root CLAUDE.md: "superseded by `beads-viewer`"). R6 §2.2 / §3.2
   (S4/S5 beads3d charts page) target a dead surface. Already deferred to v1.1 (E9) — but E9 should
   be **reframed off beads3d entirely** (its fleet-wide story panels belong in beads-viewer, not a
   Vite app we no longer ship). Strengthens R6's own "beads-viewer + iOS first" recommendation.
2. **Per-bead panel target types are wrong.** R6 S2/S7 attach charts to `agent` / `account` /
   `project` bead **types**. `bead_show.html` dispatches on 11 real types — `agent` exists, but
   **`account` and `project` are not bead types** in the viewer (nor in the iOS detail view). V1
   per-bead panels realistically attach to `type_agent.html` (+ `type_generic` fallback) only.
3. **Drill-down route is wrong.** R6 §6.1 sends an agent-slice click to `/agents/<agent-id>`. There
   is **no `GET /agents/{id}` detail page** — the canonical bead-detail route is `GET /{id}`
   (e.g. `/ka-XXXX`); `/agents/{id}/*` are agent-terminal sub-resources (`/screen`, `/events`,
   `/prime`). Drill-down must target `/{bead-id}` (or the `/agents` list), not `/agents/<id>`.
4. **iOS tab list drifted.** Actual tabs: **Active / Costs / Beads / Advice / Agents / Settings**
   (`GardenioApp.swift:3-5`). No Decisions tab; "InProgress" is labelled "Active"; Settings is new.
   Adding a **Charts** tab as a sibling of Costs still fits — the claim list is just stale.
5. **`/schedules` → `/triggers`.** The web tab was renamed (ka-0000005521, 301 redirect). Cosmetic
   for charts (we add `/charts` regardless), but R6 §2.1's tab inventory is stale.

**Cross-stream conflict (must reconcile):** R5-delta (ka-0000001813, landed 2026-05-29) re-pointed
its D1 to *"render server-side SVG on web"* (mirroring `constellation.go` / `coverage.go`), since
beads3d/vega-embed is dead. **That conflicts with R6's D5 (ECharts) and, more importantly, with the
parity bar.** A static server-rendered SVG has no hover/tooltip/tap-to-drill — so pairing static-web-SVG
against interactive iOS Swift Charts is exactly the "one platform richer, one degraded" outcome the
parity bar (ka-0000001797 AC#3) forbids. **R6's verdict: the parity bar forces interactive web →
keep D5 = ECharts (UMD `<script>`, no bundler) as the primary web renderer; server-side SVG/PNG is
the *fallback + Slack-unfurl + iOS-image-fallback* source, not the primary web path.** See §4.

---

## 1. Claims that still hold

| Claim in R6 doc | Status | Evidence |
|---|---|---|
| Add `/charts` as a sibling tab in the existing strip | ✅ Holds | Tab strip is server-rendered links in `templates/layout.html` (the `/triggers` anchor lives at `layout.html:2439`); adding a `/charts` anchor + `GET /charts` handler in `server.go` (route table ~825-1051) is a one-block addition. |
| Per-bead chart panel as a `partials/` include in `bead_show.html` | ✅ Holds (type list wrong — see §2) | `bead_show.html:11-25` does per-type dispatch into `templates/partials/type_*.html`; a `partials/charts.html` included from `type_agent.html` is exactly the right shape. |
| `/costs` inline chart above the per-account list | ✅ Holds, high value | `costs.go` `costAccountView` + `templates/costs.html` is a summary-card + per-account-card page; a chart card slots on top cleanly. Costs is the v1 metric surface — best first chart. |
| `constellation.go` proves the team server-renders inline SVG | ✅ Holds | `constellation.go` server-renders a 1000×500 viewBox SVG; this is the right *container* precedent and the right home for the SVG **fallback** path. |
| beads-viewer has **no build step** (UMD `<script>` only) | ✅ Holds | No `package.json` / bundler in `cmd/beads-viewer/`. ECharts UMD bundle works as a single `<script>`; this constraint is unchanged and load-bearing for D5. |
| iOS: add a top-level **Charts** tab + per-bead card in `BeadDetailView` | ✅ Holds | `BeadDetailView` is defined inline in `BeadsTab.swift:299` (reused by `InProgressTab.swift:72`); tab bar is a `TabView` over `AppTab` (`GardenioApp.swift`). New tab + new card both fit existing patterns. |
| iOS has **no chart UI today** — we add, not retrofit | ✅ Holds | `import Charts` appears **nowhere** in `usr/Gardenio/Gardenio/*.swift`. `UsageMonitor.swift` is numeric-only (`@Published` Doubles). Greenfield as claimed. |
| iOS deployment target ≥ iOS 16 (Swift Charts) | ✅ Holds, stronger | `IPHONEOS_DEPLOYMENT_TARGET = 17.0` (`project.pbxproj`). Comfortably above the iOS-16 floor; D6 has no OS-version risk. |
| `UsageMonitor` is the model to mirror for a `MetricsMonitor` | ✅ Holds, stronger | `UsageMonitor.swift` already polls accounts on a timer and **already computes `hourlyRate = total / hoursElapsed`** (`:7,:103`). The parity-bar "hourly rate" has a numeric precursor on iOS today — the chart just renders it as a windowed series. |
| `KbeadsClient` is the home for a `getChart(id:range:)` method | ✅ Holds | Unchanged; iOS still routes all reads through `KbeadsClient` over kbeads HTTP with the OIDC path (D4 assumption intact). |
| Catalog of named charts, client sends id+range, backend owns PromQL | ✅ Holds | Confirmed by R5-delta: kbeads `/v1/charts/{id}` is greenfield and templated-catalog v1 (no ad-hoc PromQL) remains the scope. R6 §9 constraints on R5 are still satisfiable. |
| Empty/loading/error as first-class states; sparse v1 is the common case | ✅ Holds, stronger | R5-delta confirms `vmsingle.enabled=false` by default and the cost series is `gardenio_session_cost_usd` (not the doc's `gardenio_agent_cost_usd_total`). V1 charts will frequently be empty until R1 enables VM per-env — the empty-state design is now *more* central, not less. |

## 2. Drift since 2026-04-25

| Area | R6 doc assumed | Reality today | Impact |
|---|---|---|---|
| **beads3d surface** | Live Vite/JS surface; S4 `charts-page.js` mirrors `release-health-page.js`; S5 per-molecule charts | **beads3d is DEPRECATED** (root CLAUDE.md). `release-health-page.js` / `api.js` / `main.js` still exist but the app is not shipped to users. | **High.** S4/S5 (R6 §2.2, §3.2) target a dead surface. Already v1.1 (E9), but **E9 must be reframed**: fleet-wide story panels belong in beads-viewer (a `/charts?scope=fleet` view), not a resurrected beads3d. Drop the beads3d insertion points. |
| **Per-bead panel types** | Attach panels to `agent` / `account` / `project` **bead types** (S2/S7) | `bead_show.html:11-25` dispatches `decision/schedule/formula/merge-request/agent/advice/rating/rich-report/report/bundle/widget-type/generic`. **No `account`, no `project` type.** Only `agent` matches. | **High for E6/E7 insertion points.** V1 per-bead panels attach to `type_agent.html` only (account/project spend rolls up under the `/charts` dashboard + `/costs`, addressed by `agent`/`project` **query params**, not bead-type detail pages). iOS likewise: `BeadDetailView` is type-agnostic, so gate the `MetricsCard` on `bead.type == "agent"`. |
| **Drill-down target route** | Agent-slice click → `/agents/<agent-id>` (R6 §6.1) | No `GET /agents/{id}` page. Canonical detail = `GET /{id}` (`server.go:1051`); `/agents/{id}/*` are agent-terminal sub-resources only. | **Medium.** Drill-down `routeTemplate` must be `/{value}` where value is the **bead id** (agents are `agent`-type beads), or `/agents` (list). The §6.2 "if route doesn't exist, don't emit drill-down" guard already covers this — but the default template in the catalog must be fixed to `/{value}`, not `/agents/{value}`. |
| **iOS tab inventory** | Beads / InProgress / Decisions / Agents / Advice / Costs | **Active / Costs / Beads / Advice / Agents / Settings** (`AppTab` enum, `GardenioApp.swift:3-5`; `TabView` body ~65-100). No Decisions tab; "InProgress" → "Active"; Settings added. | **Low.** Charts-tab-as-Costs-sibling still fits; only the documented list is stale. Insert the new `.charts` case between `.costs` and `.beads` (or after Costs) in `AppTab`. |
| **Web tab rename** | `/schedules` tab in the strip | `/schedules` → **301 redirect to `/triggers`** (ka-0000005521). | **Cosmetic.** R6 §2.1 tab list stale; no effect on adding `/charts`. |
| **Cost metric name/labels** (inherited from R5-delta) | `spend-by-agent` PromQL implied `gardenio_agent_cost_usd_total{agent,...}` | Emitted series is **`gardenio_session_cost_usd`** (per-session gauge) + `gardenio_session_tokens_total{type}`, labels `agent_name, project, account, session_id` (`session_metrics_push.go`). | **Low for UX, high for catalog content.** Does not change any R6 surface/renderer choice; the spend chart's series name + legend key must use `agent_name`. R3/E5 own the canonical PromQL. |

## 3. Shortest path to the v1 deliverable now

The v1 R6 deliverable is "a real, parity-compliant chart visible on both web and iOS." Shortest path,
given the drift, in dependency order off **E5** (kbeads chart catalog):

1. **One chart end-to-end first: the parity-bar hourly-rate spend chart.** It is the operator's
   explicit ask (ka-0000001797), Costs is the most-trafficked surface, and iOS already has a numeric
   `hourlyRate` precursor to validate against. Catalog id e.g. `spend-rate` with `window=1d|7d`.
2. **Web (E6), minimal first cut:**
   - Add `GET /charts` handler + `/charts` anchor in `layout.html`; render the dashboard card grid
     (initially one card) with an empty/loading/error state.
   - Add the ECharts UMD `<script>` (served from `cmd/beads-viewer/` static assets — the first JS
     dependency the viewer ships; keep it lazy-loaded only on `/charts`, `/costs`, and `agent`
     bead-show). Hydrate the `<canvas>` from a `<script type="application/json">` data block on
     `htmx:afterSwap`, consuming the catalog's `data` (series-of-points).
   - Add the `/costs` inline chart card (reuses the same partial) — highest-value, smallest insert.
   - Per-bead panel: include `partials/charts.html` from `type_agent.html` only (drop account/project).
3. **iOS (E7), minimal first cut:**
   - Add `.charts` to `AppTab` + a `ChartsTab.swift` (mirror `CostsTab` `ScrollView{VStack}`), with a
     `MetricsMonitor` mirroring `UsageMonitor` (timer poll, `@Published series`, `range` picker).
   - One `Chart { LineMark/AreaMark }` builder for the spend-rate series fed by the catalog `data`.
   - Per-bead `MetricsCard` in `BeadDetailView` gated on `bead.type == "agent"`.
4. **Parity validation:** both clients consume the **same `data` payload** for `spend-rate`; assert
   matching window controls (1d/7d), axis units, and drill-down target. This is the parity-bar test.
5. **Defer:** beads3d (E9 — reframe off beads3d), `/agents` sparklines (S3b), account/project per-bead
   panels, richer chart shapes (heatmap/sankey → image fallback). All explicitly out of v1.

The critical dependency is unchanged: **E6/E7 are blocked on E5** shipping `{data, spec, meta}` with
the corrected `gardenio_session_cost_usd` PromQL and a `window` param. Nothing on the UX side blocks
today beyond that contract.

## 4. Updated decision-point recommendations

| ID | Original default | Updated verdict | Why |
|---|---|---|---|
| **D5** — web chart lib | **ECharts** | **Hold: ECharts** (and *defend it against R5-delta's server-side-SVG re-point*) | The parity bar (ka-0000001797 AC#3: "axes, legend, **tooltip**, drill-down on **tap/click** materially the same") requires an **interactive** web chart to match interactive iOS Swift Charts. Server-rendered static SVG (R5-delta's re-pointed D1) cannot hover/tap-drill and would violate parity. ECharts as a UMD `<script>` honours beads-viewer's no-build constraint, is loaded only on chart pages, and consumes the shared `data` payload. **Server-side SVG/PNG stays as the fallback + Slack-unfurl + iOS-image-fallback, not the primary web renderer.** |
| **D6** — iOS renderer | **Swift Charts native, PNG fallback** | **Hold, unchanged** | iOS 17 target confirmed; zero existing chart code; Swift Charts gives interactivity/Dynamic Type/VoiceOver for free and consumes the catalog `data` directly. No drift. |
| **D1** — spec contract (R5-owned) | hybrid `{data, spec, meta}`; web vega-embed | **Re-affirm `data` (series-of-points) as the parity contract both clients consume; `spec` (Vega-Lite) is the portable PNG/SVG source, not what web parses.** Web parses `data` and renders via **ECharts**, *not* vega-embed (dead) and *not* server-SVG-as-primary. | Reconciles R5-delta (which correctly killed vega-embed but over-rotated to server-SVG) with R6 + parity. Both clients render natively/interactively from `data`; `spec`→SVG/PNG is the degraded fallback only. |
| **D2** — beads3d in v1 | defer | **Defer harder: reframe E9 off beads3d entirely** (deprecated). Fleet-wide panels → a beads-viewer `/charts?scope=fleet` view in v1.1. | beads3d is no longer a shipping surface. |
| **D3** — per-bead inline vs `/charts?bead=` | inline partial | **Hold: inline**, but scope to `type_agent.html` only in v1 (account/project are not bead types). | Drift #2. |
| **D4** — iOS auth | reuse OIDC | **Hold, unchanged** | `KbeadsClient`/OIDC path intact. |

**Recommendation on escalation:** the D5/D1 reconciliation (interactive ECharts vs server-side SVG)
is a cross-stream engineering reconciliation with a clear answer (the parity bar decides it), **not a
50/50 operator fork** — so it does not warrant a blocking `gb decision`. It is flagged here and in the
bead verdict for the **gardener to absorb when writing the E5 and E6 charters**, so the two deltas don't
ship contradictory renderer assumptions.

---

## 5. References

- R6 source: `gardenio/docs/research/timeseries-charts-ux.md` (ka-0000005001)
- R5-delta (cross-stream conflict): `gardenio/docs/research/applicability/R5-delta.md` (ka-0000001813)
- Parity bar: ka-0000001797 · Plan-of-record E6/E7/D5/D6: `gardenio/plans/timeseries-platform.md`
- Code surveyed (tip-of-main, 2026-05-29):
  - web: `gardenio/controller/cmd/beads-viewer/server.go` (routes), `templates/layout.html:2439`,
    `templates/bead_show.html:11-25`, `templates/partials/type_*.html`, `costs.go`, `constellation.go`
  - iOS: `usr/Gardenio/Gardenio/GardenioApp.swift:3-5`, `BeadsTab.swift:299`, `UsageMonitor.swift:7,103`,
    `CostsTab.swift`, `KbeadsClient.swift`; `Gardenio.xcodeproj/project.pbxproj` (iOS 17 target)
  - beads3d (deprecated): `beads3d/src/release-health-page.js`, `api.js`, `main.js`
