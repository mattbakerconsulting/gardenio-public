# Strict data readability & correctness rubric (metrics + traces)

> Epic [[ka-0000000756]] · task [[ka-0000006877]] (W3a).
> Operator: Matt Baker — *"need a strong strict rubric for data readability and
> correctness"* (Slack thread ka-0000007422, 2026-06-17).
>
> This **extends, does not fork**, the GRADE harness
> (`grafana-grade-audit-harness.md`, formula `ka-0000003015`). GRADE scored
> Grafana dashboards 0–10 on five soft dimensions. This rubric makes the two
> dimensions Matt cares about — **correctness** and **readability** — *strict*
> (binary pass/fail gates, not 0–10 vibes) and extends the ground-truth
> cross-check to **trace** panels and to **beads-embedded** chart-catalog panels,
> not just Grafana.

## 0. Scope & vocabulary

A **panel** is any rendered data surface:
- a Grafana panel (`/api/dashboards/uid/<uid>` → `panels[].targets[].expr`),
- a beads-viewer **chart-catalog** chart (`GET /api/v1/charts/{id}` → `data[]`,
  built from kbeads `chartCatalog()` PromQL), or
- a beads-embedded `:::chart <id>` / `:::grafana <uid>` directive.

**Ground truth** is the backing store queried directly, bypassing the panel:
- metrics → VictoriaMetrics `VM_URL/api/v1/query{,_range}` (`http://gardenio-vmsingle:8428`).
- traces → VictoriaTraces `VT_URL/select/jaeger/api/*` and the VT
  `/select/logsql`-style trace count APIs (`http://gardenio-victoria-traces:10428`).

Every panel claim must be **replayable**: a reviewer (human or gardener) can copy
one command from the panel's audit row and reproduce the displayed number.

## 1. The strict gates (ALL must pass — binary)

A panel **ships** only if it passes every C-gate and every R-gate below.
A single failure blocks the panel (files a bug; W3b wires this as the W2
acceptance gate). This is the "strict" part: no averaging, no partial credit.

### Correctness gates (C1–C6)

| Gate | Statement | How it's checked | Fail class |
|---|---|---|---|
| **C1 Replay-match** | Displayed value == ground-truth replay of the panel's own query over the panel's own time window, within tolerance. | Extract `expr`/catalog query, run against VM/VT for the panel's `[from,to]`, compare. Tolerance: exact for counts; ±1 in last significant digit for rates/quantiles (float + step alignment). | `REPLAY-MISMATCH` |
| **C2 Honest-aggregation** | The query measures what the title/legend claims. No silent `sum()` over states that inflates ("Active Agents" must not sum prewarmed+idle+queued — the ka-0000002809 bug). | Read the query's grouping & filters; confirm they match the stated semantics; check label filters are present where the title implies a subset. | `AGGREGATION-DRIFT` |
| **C3 Unit-scale** | Displayed unit == series unit. No bytes-as-decbytes (the 1e15 mbytes bug), no ratio shown as raw, no ms/s confusion. | Compare panel `fieldConfig.unit` (or catalog `y_format`) against the metric's true unit (seconds-suffixed histograms are seconds; `_bytes` is bytes; `_total` rate is per-second). | `UNIT-SCALE` |
| **C4 Freshness** | Data is live, not frozen. Last sample within an expected staleness budget for the metric's cadence. | Query `max(timestamp(<series>))`; assert `now - last < stalenessBudget` (default 5× scrape/relay interval). | `STALE-FROZEN` |
| **C5 Existence-honesty** | If a referenced series/service does not exist in the store, the panel says "no data" — it does not render a misleading zero or a broken query. | Confirm every `__name__`/`service_name` referenced exists in `VM_URL/api/v1/label/__name__/values` or VT `/select/jaeger/api/services`; if absent, panel must be in its empty state. | `MISSING-SERIES` |
| **C6 Window-integrity** | Rate/increase/quantile windows are ≥ the data's native step and don't straddle relay gaps that fabricate spikes. | Check `rate()[w]`/`increase()[w]` window ≥ step; for trace-derived panels, confirm the window > relay `interval`+`lookbackOverlap` so a poll boundary isn't read as a dropout. | `WINDOW-ARTIFACT` |

### Readability gates (R1–R5)

| Gate | Statement | How it's checked | Fail class |
|---|---|---|---|
| **R1 Legible-at-a-glance** | A reader names what the panel shows and whether it's good/bad in <5s. Title is a claim, not a metric name (`gardenio_agent_llm_request_latency_seconds`). | Title/legend/axis review; the title states the question, units are on the axis. | `UNREADABLE-LABEL` |
| **R2 Honest-empty** | Empty renders as an explicit "no data yet" state, never a blank canvas or a phantom flat line at 0. | Confirm the empty branch exists (chart-catalog: built-in; Grafana: `noValue`/`No data` text). | `BLANK-EMPTY` |
| **R3 Scale-sanity** | Y-axis/units make the signal visible (no everything-flat-at-bottom from a single outlier; log scale or split where warranted). | Eyeball + check max/min spread vs axis range. | `SCALE-CRUSH` |
| **R4 Self-locating** | The panel carries its own context: time window shown, units labeled, series legend present, and (for service panels) which service/env. | Presence check. | `NO-CONTEXT` |
| **R5 Drill-path** | A reader can get from the panel to the underlying records (trace table, agent profile, raw query). The ACME "Open in Explore" pattern. | Confirm a drill link / `:::grafana` explore / catalog `data-chart-drill` exists for aggregate panels. | `DEAD-END` |

## 2. Trace-specific correctness (the new surface)

Traces are not metrics; C1–C6 still apply but the ground-truth replay differs.
For any **span-derived (RED) metric panel** (`span_calls_total`,
`span_duration_milliseconds_*`):

1. **Count reconciliation** — `sum(increase(span_calls_total{service_name="X"}[w]))`
   over the window must reconcile (±5%, allowing spanmetrics aggregation) with the
   **actual trace count** from VT's Jaeger API:
   `GET VT_URL/select/jaeger/api/traces?service=X&start=<from_us>&end=<to_us>&limit=…`
   counted. A large divergence means the spanmetrics connector and the raw span
   stream disagree → `SPANMETRIC-DRIFT` (a trace-flavored AGGREGATION-DRIFT).
2. **Latency-quantile honesty** — `histogram_quantile(0.95, …span_duration…_bucket)`
   must use a bucket layout that actually has resolution near p95 (no quantile
   pinned to `+Inf`). Check the bucket boundaries cover the observed range.
3. **Service-existence** — every `service_name=~"…"` in a panel resolves against
   `VT_URL/select/jaeger/api/services` (today: keycloak, site-api, sponsor-api,
   study-web from the perf relay). A panel referencing a service the relay does
   not mirror must read empty, not zero (C5).
4. **Relay-lag awareness** — trace panels must not be read as "down" during the
   relay's `interval`+`lookbackOverlap` trailing window. The freshness budget
   (C4) for trace panels = `relay.interval + relay.lookbackOverlap + 1×interval`.

## 3. The replay harness (how a panel is audited)

Per panel, the audit produces one **AuditRow**:

```text
{
  panel:    "<dashboard-uid>/<panel-id>" | "catalog:<chart-id>",
  surface:  "grafana" | "catalog" | "embed",
  store:    "vm" | "vt",
  window:   {from, to, step},
  query:    "<exact expr / catalog query>",
  displayed: <number|series>,
  truth:     <number|series>,      # replayed
  gates:    { C1:pass, C2:pass, …, R5:pass },
  verdict:  "OK" | "<first failing fail-class>",
  replay_cmd: "<one curl the reviewer can paste>"
}
```

`verdict == OK` ⇔ every gate passed. Any other verdict files a bug bead
(parented under the epic, one per panel) with the `replay_cmd` so the fix is
self-evident. This is the machine-checkable contract W3b enforces on W2 panels.

### In-cluster access (verified pattern, mirrors GRADE)
```bash
VM_URL=http://gardenio-vmsingle:8428                 # metrics ground truth
VT_URL=http://gardenio-victoria-traces:10428         # traces ground truth
GRAFANA_URL=http://gardenio-grafana:3000             # admin basic-auth in-cluster
VIEWER_URL=http://gardenio-beads-viewer:8080         # catalog: /api/v1/charts/{id}

# metric replay
curl -fsS "$VM_URL/api/v1/query?query=<expr>&time=<to>"
# trace count replay
curl -fsS "$VT_URL/select/jaeger/api/traces?service=<svc>&start=<from_us>&end=<to_us>&limit=1000" | jq '.data|length'
# catalog panel data
curl -fsS "$VIEWER_URL/api/v1/charts/<id>?since=<from>&until=<to>&step=<step>" | jq '.data'
```
The public ingress stays behind AVA forwardAuth (agent pods can't complete the
code flow) — always audit **in-cluster by ClusterIP**.

## 4. Verdict reporting & cadence

- One `REPORT.md` per audit iteration under `docs/data-correctness/<ITERATION>/`,
  listing every AuditRow and the pass/fail matrix — diffable across runs
  (maps-beauty / GRADE precedent).
- One bug bead per failing panel, fail-class in the title, `replay_cmd` in body,
  parented under the audit epic.
- Cadence: the W1c perf→VT gardener runs this rubric against the trace panels each
  cycle; the GRADE gardener (ka-0000001426) absorbs the metric panels. Both stay
  operator-gated until go-ahead.

## 5. Relationship to GRADE (no duplication)

| | GRADE (ka-0000003015) | This rubric (strict) |
|---|---|---|
| Output | 0–10 per G/R/A/D/E dimension, bar=8 | binary pass/fail per gate |
| Surface | Grafana only | Grafana + beads-embedded catalog + traces |
| Correctness | "G" dimension, one of five | C1–C6, the whole point |
| Use | quality-improvement loop | **acceptance gate** for shipping a panel |

GRADE answers *"how good is this dashboard?"*. This rubric answers *"is this panel
allowed to ship?"* — the strict gate Matt asked for. A panel can score 9/10 on
GRADE and still fail C1 (shows a wrong number beautifully); it does not ship.

## 6. The gate, enforced (W3b)

> Task [[ka-0000000418]] (W3b). Wires §1–§2 as a machine-checkable acceptance
> gate so a W2b/W2c chart-catalog panel cannot ship as a dashboard-bead unless
> every gate passes. Code: `kbeads/internal/server/charts_audit.go` (+ test).

The gates split by what they need to be decided — and that split is the whole
implementation:

### 6.1 Static gates — enforced at build time (the merge gate)

Several gates are decidable from the catalog `ChartDef` alone, with no live
store: structural integrity, **C2** (the active-agents silent-sum bug —
[[ka-0000002809]]), **C3** unit-scale, **C6** window-integrity, **R1**
legible-label, **R4** self-locating, **R5** drill-path. `auditChartDefStatic()`
returns one `GateViolation` per failing gate, each carrying the gate id, fail
class, and a self-evident detail string.

`TestCatalog_DataCorrectnessGate` runs these over **every** `chartCatalog()`
entry and **fails `go test`** on any violation. This is the gate that blocks a
new perf/trace panel from merging — the same build-time-invariant shape as the
existing cost-chart guard (`charts_catalog_cost_guard_test.go`). Because it
iterates the catalog, it covers W2b/W2c entries automatically the moment they
land. `TestGate_BitesOnBadDefs` proves each gate fires on a deliberately bad
def, so a green run means the catalog is clean — not that the gate is asleep.

A W2b/W2c author runs `go test ./internal/server/ -run DataCorrectnessGate`;
red output names the chart, the gate, and what to fix.

### 6.2 Live classifiers — the AuditRow verdict (the gardener gate)

The remaining gates need the store (you cannot know if a *displayed* number is
right without replaying it): **C1** replay-match, **C4** freshness, **C5**
existence-honesty, plus the trace-specific **§2.1** span-count reconciliation
and **§2.4** trace-freshness budget. These are pure functions —
`ClassifyReplay`, `ClassifyFreshness`, `ClassifyExistence`, `ClassifySpanCount`,
`TraceFreshnessBudget` — that take the displayed value and the ground-truth
replay and return the rubric's fail class. The W1c perf→VT gardener (and any
reviewer) calls them per panel against VM/VT-by-ClusterIP (§3 access recipe) to
build the `AuditRow`; `Verdict()` collapses the per-gate map to the single
"first failing, no partial credit" verdict the rubric demands.

`referencedMetrics()` extracts the series a query touches so C5 knows which
`__name__`/`service_name` values to confirm against
`VM_URL/api/v1/label/__name__/values` (or VT `/select/jaeger/api/services`).

A panel **ships** iff its static gates pass at build time **and** its live
AuditRow verdict is `OK` when the gardener replays it against the live store.
