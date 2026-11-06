# ADR: VictoriaMetrics Deployment Topology for Gardenio

**Status:** Proposed
**Date:** 2026-04-05
**Author:** gardenio-research-victoriametrics-deployment-zzv
**Task:** ka-0000002947
**Parent Epic:** ka-0000001059 (Emit gardenio cost metrics to VictoriaMetrics)

## Context

Gardenio emits Prometheus-compatible cost/usage metrics from the controller
(`gardenio-agents` deployment) via `prometheus/client_golang` on port 8091
at `/metrics`. These metrics cover:

- **Per-agent gauges** (11 metrics, labels: agent/project/role): cost_usd,
  input/output/cache tokens, requests, sessions, tool_calls, errors, compactions
- **Per-account gauges** (11 metrics, label: account): cost_usd, tokens,
  utilization_5h/7d, throttled, warming
- **Fleet gauge** (1 metric, labels: project/role/state): agents_active
- **Scrape metadata** (2 metrics): last_scrape_timestamp, scrape_errors_total

The controller pod has `prometheus.io/scrape` annotations, but the cluster
Prometheus only scrapes via ServiceMonitor CRDs — **gardenio metrics are not
currently being scraped**.

## Cluster Observability Inventory

| Component | Namespace | Details |
|---|---|---|
| Prometheus (kube-prometheus-stack) | observability | v2.55.0, 2 replicas, 50Gi PVC each, **2h retention** |
| prometheus-operator | observability | Manages Prometheus via CRDs |
| kube-state-metrics | observability | Standard K8s state metrics |
| node-exporter | observability | DaemonSet across all nodes |
| OpenTelemetry Collector | observability | Traces (Jaeger/OTLP protocols) |
| VictoriaLogs Collector | observability | DaemonSet log shipping (v1.43.1) |
| Grafana | **not deployed** | No Grafana instance found anywhere in cluster |
| VictoriaMetrics (TSDB) | **not deployed** | No vmsingle, vmcluster, vmagent, or VM operator CRDs |

**Key finding:** Prometheus has only **2h retention**. This makes it unsuitable
for cost dashboards, trend analysis, or budget alerting — all of which require
days-to-months of history.

## Questions Answered

### 1. Is VictoriaMetrics already running in the cluster?

**No.** Only VictoriaLogs collectors exist (for log shipping). There is no
VictoriaMetrics time-series database, no VM operator, and no VM-related CRDs.
Prometheus is the only metrics TSDB, with only 2h retention.

### 2. Single-node vs cluster mode?

**Recommendation: vmsingle (single-node).**

Cardinality estimate for gardenio metrics:
- Agents: ~20 concurrent (peaks ~50 during gardener spawning bursts)
- Accounts: ~5 active
- Projects: ~7
- Metric families: 25
- Unique time series: ~20 agents × 11 metrics + 5 accounts × 11 metrics +
  fleet states ≈ **300-600 active series**

This is trivially small. vmsingle handles millions of series on a single node.
vmcluster (vmselect + vminsert + vmstorage) is unnecessary overhead for this
scale and adds operational complexity (3 separate components, shard management).

### 3. Should we use victoria-metrics-k8s-stack Helm chart?

**Recommendation: No. Use the standalone vmsingle Helm chart + a ServiceMonitor.**

The `victoria-metrics-k8s-stack` chart bundles Grafana, vmagent, vmalert,
kube-state-metrics, node-exporter, and more. The cluster already has all of
these via kube-prometheus-stack. Installing the full stack would create
duplicate infrastructure and potential conflicts.

Instead:
- Deploy **vmsingle** via the `victoria-metrics-single` Helm chart (minimal)
- Add a **ServiceMonitor** to the gardenio Helm chart to tell existing
  Prometheus to scrape the controller
- Optionally deploy **vmagent** as a lightweight scraper that both reads
  existing Prometheus targets and remote-writes to vmsingle for long-term
  storage

### 4. Where does scrape config live?

**Recommendation: ServiceMonitor CRDs.**

The existing Prometheus has `serviceMonitorSelector: {}` and
`serviceMonitorNamespaceSelector: {}` — it will pick up ServiceMonitors from
any namespace. This is the standard kube-prometheus-stack pattern.

Add a ServiceMonitor to `gardenio/helm/gardenio/templates/controller/`:

```yaml
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: gardenio-agents
  labels:
    app: gardenio-agents
spec:
  selector:
    matchLabels:
      app: gardenio-agents
  endpoints:
    - port: http
      path: /metrics
      interval: 30s
```

This immediately fixes the "not being scraped" problem and gives 2h of data
in Prometheus. For long-term storage, vmsingle with remote-write from
Prometheus or a standalone vmagent is needed.

### 5. Storage sizing

With ~500 active series, 30s scrape interval, and VictoriaMetrics compression:

| Retention | Estimated Storage |
|---|---|
| 30 days | ~50 MB |
| 90 days | ~150 MB |
| 1 year | ~600 MB |

VictoriaMetrics achieves ~1-2 bytes per data point with compression. At 500
series × 2 points/min × 525,600 min/year ≈ 525M data points/year ≈ 600 MB.

**Recommendation: 10Gi PVC with 90-day retention.** This is generous (>60x
headroom) and allows for 10x cardinality growth. Revisit if agent fleet grows
past 200 concurrent agents.

### 6. Retention policy

**Recommendation: 90 days.**

- 30 days is too short for monthly cost reporting and budget reviews
- 1 year is more data than needed for operational metrics (cost data is also
  stored in bead fields as the system of record)
- 90 days covers quarterly reviews and gives enough trend data for alerting

## Recommended Architecture

```
┌─────────────────────────────────────────────────────────┐
│ gardenio namespace                                       │
│                                                         │
│  gardenio-agents ──► :8091/metrics                       │
│       │                    ▲                             │
│       │              ServiceMonitor                     │
│       │                    │                             │
│       │     ┌──────────────┘                             │
│       │     │                                           │
│  ┌────▼─────▼────┐                                      │
│  │  Prometheus    │ ◄── existing, 2h retention           │
│  │ (observability)│                                      │
│  └───────┬───────┘                                      │
│          │ remote_write                                  │
│          ▼                                              │
│  ┌───────────────┐                                      │
│  │   vmsingle    │ ◄── NEW, 90d retention, 10Gi PVC     │
│  │  :8428/api    │                                      │
│  └───────┬───────┘                                      │
│          │                                              │
│          ▼                                              │
│  ┌───────────────┐                                      │
│  │   Grafana     │ ◄── NEW (optional, Phase 2)          │
│  │  dashboards   │                                      │
│  └───────────────┘                                      │
└─────────────────────────────────────────────────────────┘
```

### Phase 1: Fix scraping (immediate, no new infra)

1. Add `ServiceMonitor` to gardenio Helm chart for the controller
2. Verify metrics appear in Prometheus (`gardenio_agents_active`, etc.)
3. This gives 2h of real-time data — enough for live fleet monitoring

### Phase 2: Deploy vmsingle for long-term storage

1. Add `victoria-metrics-single` as a Helm subchart dependency, or deploy
   as a separate release in the `gardenio` (or `observability`) namespace
2. Configure Prometheus `remoteWrite` to push gardenio metrics to vmsingle:
   ```yaml
   # In Prometheus CRD spec or kube-prometheus-stack values
   remoteWrite:
     - url: http://vmsingle-gardenio.gardenio.svc:8428/api/v1/write
       writeRelabelConfigs:
         - sourceLabels: [__name__]
           regex: 'gardenio_.*'
           action: keep
   ```
3. Set retention: `-retentionPeriod=90d`
4. PVC: 10Gi gp2

### Phase 3: Grafana dashboards (optional)

1. Deploy Grafana (standalone or via `grafana` Helm chart) pointed at vmsingle
2. Pre-provision dashboards for:
   - Fleet cost overview (cost/hour, cost/agent, cost/project)
   - Account utilization and throttling
   - Token usage trends
3. Alternatively, use an external Grafana instance if one becomes available

## Alternative Considered: vmagent Instead of Prometheus Remote Write

Instead of adding remote_write to the shared Prometheus, deploy a standalone
`vmagent` in the gardenio namespace that:
- Scrapes the controller ServiceMonitor directly
- Writes to vmsingle

**Pros:** No changes to shared Prometheus config, fully self-contained.
**Cons:** Duplicate scraping, another component to manage.

This is a viable fallback if modifying the shared Prometheus remote_write
config is not feasible (requires access to the `observability` Helm release).

## Resource Estimates

| Component | CPU Request | Memory Request | Storage |
|---|---|---|---|
| vmsingle | 100m | 256Mi | 10Gi PVC (gp2) |
| vmagent (if used) | 50m | 128Mi | — |
| Grafana (if used) | 100m | 128Mi | 1Gi PVC |

Total incremental cost is minimal — well under 1 vCPU and 512Mi RAM.

## Decision

Deploy **vmsingle** (single-node VictoriaMetrics) with **90-day retention** and
**10Gi storage**. Feed it via **Prometheus remote_write** filtered to `gardenio_*`
metrics. Add a **ServiceMonitor** to the gardenio Helm chart immediately to fix
the current scraping gap.

Do NOT deploy the full victoria-metrics-k8s-stack — the cluster already has
Prometheus, kube-state-metrics, and node-exporter via kube-prometheus-stack.
