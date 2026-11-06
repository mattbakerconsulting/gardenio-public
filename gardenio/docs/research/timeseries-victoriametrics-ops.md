# VictoriaMetrics deployment & ops on the gardenio cluster — research note

**Bead:** ka-0000000179 (R1, research). Parent epic: ka-0000007704 — _gardenio time-series metrics + chart platform_.
**Status:** research/design only — no deployments, no Helm installs, no cluster mutations. Recommendations land here; an operator decision-gate authorizes any rollout.
**Reader:** the gardener that consolidates the six R-tasks into the implementation plan, and the operator who picks the rollout path.

This note answers the ten questions in the parent bead. It surveys the existing scaffolding in the chart and the ACME observability stack, picks a deploy mode, and emits sample `values.yaml` snippets ready for a per-environment values file.

---

## 1. TL;DR — recommendation block

- **Deploy mode:** `victoria-metrics-single` (vmsingle). At gardenio's current scale (one cluster, single agent fleet, ≪ 10k active series, < 1k samples/sec) the single-node binary is sufficient for ≥ 1 year. Migrate to `victoria-metrics-cluster` only when (a) sustained ingest > 100k samples/sec, (b) we need cross-AZ HA on the storage path, or (c) we shard tenants. None of those apply at v1. **Concrete trigger:** alert on `vm_rows_inserted_per_second > 50000` averaged over 1h, _or_ `vm_active_timeseries > 1_000_000`, _or_ p99 query latency > 2s — any of those three sustained for a week is the cue to plan a cluster migration.
- **Helm chart:** **do not pull in `victoriametrics/victoria-metrics-single` as a subchart**. The gardenio chart **already ships a hand-written, scoped vmsingle + vmagent + grafana set of templates** at `gardenio/helm/gardenio/templates/{vmsingle,vmagent,grafana}/` — keep using them. They are smaller, scoped to our exact ingest and scrape needs, integrate with our NetworkPolicies, and avoid pulling in the upstream chart's CRDs (vmoperator etc.) that we do not want.
- **Namespace:** **`gardenio`** (the chart's release namespace). Do not introduce a `gardenio-metrics` namespace. The existing NetworkPolicy template explicitly allows vmagent → controller and vmagent → beads scrape paths in-namespace; cross-namespace would require new policies and new ServiceAccount/RBAC. The "co-locate with the workload" pattern matches what the rest of the chart does (postgres, nats, grafana, beads-viewer all co-located).
- **Storage:** PVC, **`gp3`** storage class (`gardenio-storage` is the chart-default StorageClass, pinned to `ebs.csi.aws.com` + `gp3`), **50 Gi** for v1 (5× the chart default of 10 Gi — see §4 for the math), **`WaitForFirstConsumer`**, **retention 365d (1 year)** for v1. Long retention is the whole reason vmsingle exists in this chart — the ACME-wide `kube-prometheus-stack` only keeps **15d**.
- **Ingestion:** **mixed: scrape `+` push.** Scrape via vmagent for the existing controller + beads `/metrics` endpoints (already annotated with `prometheus.io/scrape=true`). Push via the existing `gb session_metrics_push.go` path for terminal session metrics that must survive pod tear-down (`gardenio_session_*`). **Do not** add a new write protocol; the chart already wires both paths.
- **vmagent:** **yes, deploy it.** It is the scrape collector for in-cluster `/metrics`, the ingest endpoint for the `gb session metrics push` path (`/api/v1/import/prometheus`), and the buffer-and-forward target for any future push from off-cluster agents. One replica is fine (loss tolerance: a few seconds during restarts; no rate-of-change semantics on session push so a missed push only loses one final-state sample).
- **vmalert:** out of v1 scope. Note the integration path in §7 — when we want alerting we point vmalert at vmsingle as the datasource and at the existing `kube-prometheus-stack` Alertmanager (deployed by the ACME observability chart) as the notifier; we **do not** deploy a second Alertmanager.
- **Auth + network:** **in-cluster only for v1.** vmsingle and vmagent never get an Ingress; only Grafana does. Grafana's IngressRoute already gates behind Keycloak `forwardAuth` middleware. **Defer vmauth** until we expose vmsingle externally (e.g. for an iOS chart consumer that hits `/api/v1/query_range` directly) — at that point vmauth becomes the natural fit, but for v1 the chart backend (Phase-2 deliverable in the umbrella epic) brokers all reads.
- **Backup / DR:** **vmbackup not in v1.** vmsingle is *not* the source of truth for any business data — every metric it stores is reproducible (push: re-emit on next session; scrape: re-derive from the controller's in-memory state). v1 acceptable RPO is "since last enable", v1 acceptable RTO is "redeploy the StatefulSet" (~5 min). Add vmbackup with daily snapshot to S3 in v2 _only if_ the operator decides historical loss is unacceptable for cost-attribution evidence. See §8.
- **Coexists with**, does not replace, the ACME-ecosystem `kube-prometheus-stack` and the `victoria-metrics-k8s-stack` vmcluster deployed by the `observability` chart. Those serve infra metrics with 15d / 14d retention. The gardenio in-chart vmsingle serves **only `gardenio_*` and `beads_*` metrics with 1-year retention**, gated by a vmagent scrape filter. See §9 for the demarcation.
- **Cost (rough):** **~$10/month** in EBS at v1 sizing (50 Gi gp3 ≈ $4/mo + Δ snapshots in v2 ≈ $2/mo) plus **~$5/month** in compute (50 m+50 m CPU + 256 MiB+64 MiB RAM aggregate fits in spare worker headroom — no new node group). Total < $20/month for a year of long-retention gardenio metrics. See §10.

---

## 2. What already exists in the chart (read this first — most of the work is done)

The gardenio chart already contains a **complete** vmsingle + vmagent + grafana scaffolding, all gated by `*.enabled` flags that are `false` everywhere today. No prod or staging environment enables any of them. Nothing is deployed; everything is plumbed.

| Component | Path | Default | What's there |
|---|---|---|---|
| vmsingle StatefulSet | `helm/gardenio/templates/vmsingle/statefulset.yaml` | `vmsingle.enabled: false` | StatefulSet with PVC, 1 replica, retention from `vmsingle.retentionPeriod`, image `victoriametrics/victoria-metrics:v1.108.1` |
| vmsingle Service | `helm/gardenio/templates/vmsingle/service.yaml` | gated | ClusterIP `:8428` |
| vmagent Deployment | `helm/gardenio/templates/vmagent/deployment.yaml` | `vmagent.enabled: false` | Deployment, 1 replica, scrapes K8s SD, remote-writes to vmsingle |
| vmagent ConfigMap | `helm/gardenio/templates/vmagent/configmap.yaml` | gated | Scrape config: pods with `prometheus.io/scrape=true` in release namespace |
| vmagent RBAC | `helm/gardenio/templates/vmagent/rbac.yaml` | gated | ServiceAccount + Role for pod SD |
| vmagent Service | `helm/gardenio/templates/vmagent/service.yaml` | gated | ClusterIP `:8429` |
| Grafana Deployment | `helm/gardenio/templates/grafana/deployment.yaml` | `grafana.enabled: false` | Deployment, optional sidecar, datasource pre-wired to vmsingle |
| Grafana IngressRoute | `helm/gardenio/templates/grafana/ingressroute.yaml` | gated | Traefik IngressRoute with optional Keycloak forwardAuth, IP whitelist, rate limit |
| Grafana ConfigMap | `helm/gardenio/templates/grafana/configmap.yaml` | gated | Datasources file pre-wires VictoriaMetrics datasource at `http://<release>-vmsingle.<ns>.svc:8428` |
| NetworkPolicy carve-outs | `helm/gardenio/templates/networkpolicies/networkpolicies.yaml` | gated by `vmagent.enabled` | vmagent → beads:9091, vmagent → controller:9090 ingress allowed |
| Helpers | `helm/gardenio/templates/_helpers.tpl` | always | `gardenio.vmsingle.fullname`, `gardenio.vmagent.fullname`, `gardenio.vmsingle.writeURL`, labels, selector labels |
| Prebuilt dashboards | `helm/gardenio/dashboards/{cost-overview,kbeads-embedding}.json` | shipped | Two Grafana dashboards already authored against `gardenio_*` and `beads_*` metric names |

Producer side, also already shipped:

| Producer | Where | Metrics |
|---|---|---|
| controller `/metrics` | `gardenio/controller/cmd/controller/main.go:266` listens on `:9092`; pod annotation `prometheus.io/scrape=true`, `path=/metrics`, `port=9092` | `gardenio_agents_active`, `gardenio_agent_cost_usd`, `gardenio_agent_{input,output,cache_read,cache_write}_tokens`, `gardenio_agent_requests`, `gardenio_agent_sessions`, `gardenio_agent_tool_calls`, `gardenio_agent_errors`, `gardenio_agent_compactions`, `gardenio_account_*` (parallel set, 12 metrics including `gardenio_account_utilization_{5h,7d}`, `gardenio_account_throttled`, `gardenio_account_warming`), `gardenio_last_scrape_time`, `gardenio_scrape_errors_total` — see `gardenio/controller/internal/metrics/metrics.go` |
| beads `/metrics` | `kbeads/cmd/kd/serve.go:258` — separate listener on `metricsPort` (default `9091`) | `beads_embed_duration_seconds`, `beads_embed_errors_total`, `beads_semantic_search_duration_seconds`, `beads_semantic_search_results_total`, `beads_embedding_coverage_ratio`, `beads_embedding_stale_count`, `kbeads_token_revocations_total`, `kbeads_revoked_tokens` |
| `gb session` push | `gardenio/controller/cmd/gb/session_metrics_push.go` — POSTs to `$VMAGENT_URL/api/v1/import/prometheus` on session terminate | `gardenio_session_cost_usd`, `gardenio_session_tokens_total{type=…}`, `gardenio_session_duration_seconds`, `gardenio_session_request_count`, `gardenio_session_tool_calls_total`, `gardenio_session_errors_total`, `gardenio_session_compactions_total` — labels: `agent_name`, `project`, `account`, `session_id` |

The umbrella epic's "v1 metrics" — *spend per agent per hour*, *beads events by type* — are **already emitted** today. The gap is the receiver. **Enabling vmsingle and vmagent in one environment is the v1 ship.**

> ⚠ One gap to flag for v1: there is no `gardenio_beads_event_total{type=…}` counter on the controller side. The beads-event-rate panel that the umbrella epic v1 metrics describe will need either (a) a new counter on the controller or (b) a vmagent recording-rule that derives it from controller bead-state-transition events. That's a small instrumentation task; flag it as a follow-up bead in the synthesis phase.

---

## 3. Question-by-question answers

### Q1 — Deploy mode: single-node vs cluster?

**Single-node (vmsingle).** Vetted by VictoriaMetrics' own guidance: vmsingle handles 100k+ samples/sec on a single node and is _the_ recommended mode below ~1M active series. Gardenio's expected v1 series count is **well under 10k** — 13 controller-side gauges × {agent_name, project, role, model, env} dimensionality (cardinality is bounded by agent count, currently O(50) and operator-controlled), plus 7 beads metrics, plus 8 session push metrics × {agent_name, project, account, session_id} for session lifetime only.

| Mode | Pros for gardenio | Cons | Verdict |
|---|---|---|---|
| **vmsingle** (recommended) | 1 pod, 1 PVC, no operator, no CRDs. Existing chart templates work as-is. Atomic backups (one PVC). Faster restore. 5-minute redeploy on failure. | No HA. Reads block during compaction (rare at this size). | **Use this for v1.** |
| vmcluster (vmstorage + vmselect + vminsert) | HA across AZ, horizontal scale on storage and query, multi-tenancy via `tenant` URL prefix. | 3+ pods minimum, ~3× cost. Requires `victoria-metrics-operator` CRDs. Operationally significant to migrate (data is movable but not zero-effort). | **Defer.** Migration triggers in §1. |

The ACME-ecosystem `observability` chart already runs `victoria-metrics-k8s-stack` with `vmcluster.enabled: true`, replicationFactor=2 — that vmcluster is *not* the right home for gardenio's `gardenio_*` long-retention metrics because (a) its retention is 14d, (b) it lives in the `observability` namespace and would need NetworkPolicy + RBAC churn to receive our push, (c) it is operated by the platform team for infra metrics and we should not couple gardenio's product telemetry to its lifecycle. See §9.

### Q2 — Helm chart: which one, where, and our conventions

**Use the in-chart hand-written templates** (`templates/vmsingle/`, `templates/vmagent/`, `templates/grafana/`). Reasons:

1. They already exist, lint, and pass `helm template` rendering with our values style.
2. They are scoped to single-node + single-vmagent; pulling in the upstream `victoria-metrics-single` chart (or `victoria-metrics-k8s-stack`) would either (a) drag in CRDs we don't need or (b) require a `fullnameOverride` + extensive value-passthrough to make the resource names line up with our existing dashboards and ServiceMonitor expectations.
3. They integrate with our patterns: chart-shared `_helpers.tpl` selectors, NetworkPolicy carve-outs, Traefik IngressRoute, Keycloak forwardAuth, our `gardenio-storage` StorageClass, our nodeSelector helper.
4. The upstream chart's defaults (security context, probe paths, retention flag spelling) match ours; there's no behavioural gap to bridge.

**File layout convention** matches the rest of the chart:
- `helm/gardenio/templates/<component>/{statefulset|deployment,service,configmap,rbac,ingressroute,pvc}.yaml`
- One value-stanza per component at the top of `values.yaml` with the convention `<component>.enabled` + image + persistence + resources + nodeSelector + tolerations + affinity.
- Wrapper-chart override per environment in `acme-helm-chart/charts/gardenio/values/<env>.yaml`.

**Namespace:** the chart release namespace, today `gardenio`. The chart already deploys postgres, nats, grafana, controller, bridges, beads, beads-viewer, beads3d into this single namespace; vmsingle + vmagent fit the same model. **Do not introduce a `gardenio-metrics` namespace** — every cross-namespace edge becomes a NetworkPolicy + ServiceAccount delta.

**Resource sizing (v1):** chart defaults are appropriate.
- vmsingle: requests `100m / 256Mi`, limits `500m / 512Mi` — fine for the projected ingest. Scale up only if `process_resident_memory_bytes` consistently > 80% of the limit or query p99 > 1s.
- vmagent: requests `50m / 64Mi`, limits `200m / 128Mi` — fine for in-namespace SD (~5–10 scrape targets at 30s).
- Grafana: requests `50m / 64Mi`, limits `200m / 128Mi` — fine for one operator at a time.

### Q3 — Storage: PVC, retention, downsampling

**PVC sizing: 50 Gi for v1.** The chart default is 10 Gi; that is too tight for 1 year of retention if the controller scrape footprint grows. Math, conservatively:

- VictoriaMetrics typically achieves ~0.5–1 byte per data point on disk (well-documented; Prometheus is 1–2 bytes per point).
- v1 active-series ceiling: ≈ 50 agents × ~13 metrics + ~50 accounts × ~12 metrics + ~7 beads/kbeads metrics = **~1300 series**, plus session push for ~5k completed sessions/year × 8 metrics = **~40k more series, single-sample, low cardinality** (session_id is the only burst; series are short-lived).
- Scrape rate: 13 + 12 + 7 = ~32 metrics × 50 agents/accounts ÷ 30s ≈ **~50 samples/sec sustained**.
- Annual ingest: 50 × 86400 × 365 ≈ **1.6B samples/year** × 1 byte = **~1.6 GiB/year** for scrape; session pushes add a few hundred MiB.
- **50 Gi gives ~30× headroom** to absorb growth in agent count, label cardinality, and an eventual move to scrape interval = 15s without re-sizing. 50 Gi gp3 is ~$4/mo; the safety margin is worth it. Volume expansion is enabled on `gardenio-storage`, so trimming to 25 Gi later is non-trivial but growing is one-line.

**Storage class:** **`gardenio-storage`** (the chart's own SC, gp3, ebs.csi.aws.com, `WaitForFirstConsumer`, `Delete` reclaim, expansion-allowed). Set `vmsingle.persistence.storageClass: ""` and let it inherit `global.storageClass`. Do *not* hardcode `gp2`, do *not* introduce a new SC.

**Retention: `1y` (365d)** for v1.

| Retention | Why |
|---|---|
| 90d (chart default) | Matches what the chart suggests as a starting point, but the entire reason to add vmsingle when we already have a 15d kube-prometheus-stack is to keep cost and rate metrics across quarter-over-quarter comparisons. |
| **1y (recommended)** | Operator-stated desire to track "spend per agent per hour" — a metric whose value compounds with retention. 1.6 GiB/year footprint makes this almost free. |
| 2y / forever | Diminishing returns; postpone until we've validated query patterns + downsampling. |

**Downsampling:** **not in v1.** vmsingle does not natively downsample (cluster mode does). For v1 our cardinality + sample rate is so low that raw retention beats downsampling on every metric — and our cost-overview dashboard's longest-range query is "1y" which vmsingle handles in milliseconds at this size. Revisit if (a) we add per-tool / per-message granularity that 10×s the series count, or (b) we move to cluster mode.

**Recording rules** — yes, but as a *vmagent* config addition rather than vmsingle. Useful v1 pre-aggregations:

- `gardenio:agent_cost_usd:rate1h` = `rate(gardenio_session_cost_usd_sum[1h])` per `agent_name,project` — collapses per-session cost into a per-agent time series.
- `gardenio:beads_events:rate5m` = `rate(<beads-event-counter>[5m])` per `type,project` — resolves the beads-event-rate v1 metric.

These are tiny config additions, not separate components. Keep them in `vmagent.recording_rules` once the metric names solidify; do not lock them in v1.

### Q4 — Ingestion: scrape vs push

**Mixed.** Use both, and route them through vmagent.

| Producer | Direction | Why |
|---|---|---|
| `gardenio-controller` | scrape — vmagent → `:9092/metrics` | Long-running deployment; scrape interval semantics fit the gauge-shaped controller metrics; matches existing pod annotations. |
| `gardenio-beads` | scrape — vmagent → `:9091/metrics` | Same reasoning; counters are well-suited to scrape. |
| `gb session` (terminating agent pod) | push — pod → `vmagent:8429/api/v1/import/prometheus` | Pods can disappear in 30s; a missed scrape on a 30s interval ≈ 100% miss rate for short sessions. Push-on-terminate guarantees the final cost row lands. **This is already implemented.** |
| Off-cluster producers (future) | push — auth'd Ingress → vmauth → vmsingle | Out of v1 scope. |
| `vmagent` itself | self-scrape | Standard vmagent pattern; useful for `vmagent_remotewrite_*`. |

**Why vmagent in front, not direct push to vmsingle:** vmagent gives us
- buffer-and-forward (so a vmsingle restart doesn't lose pushed samples),
- a single ingest endpoint to point producers at (decouples push code from where vmsingle lives),
- the future option to fan-out to a cluster instance without touching producer code,
- a cheap place for relabeling / recording-rule additions.

The `gb session_metrics_push.go` envelope ( `/api/v1/import/prometheus`, Prometheus text exposition format) is already vmagent-compatible and works end-to-end. **Do not add a new wire format** (no native InfluxDB line protocol, no Graphite, no OTLP) for v1.

### Q5 — vmagent: do we need it?

Yes. Three roles:
1. **Scrape collector** — discovers `prometheus.io/scrape=true` pods in the gardenio namespace and remote-writes to vmsingle. Replaces the (theoretical) need for a per-namespace ServiceMonitor + the kube-prometheus-stack scraping into our namespace, and importantly **lets us filter to `gardenio_*` and `beads_*`** before remote-write, which keeps the long-retention dataset clean.
2. **Push receiver** — the `/api/v1/import/prometheus` endpoint accepts the `gb session` push payload (already wired via `VMAGENT_URL` env var on the agent pod).
3. **Buffer/forward** — vmagent's local on-disk queue absorbs vmsingle restarts and back-pressures producers gracefully.

**Scrape configs** — already authored in `templates/vmagent/configmap.yaml`. The default config scrapes any pod in the release namespace with the `prometheus.io/scrape=true` annotation. **One additional relabel** is worth adding before enabling: drop everything that is *not* `gardenio_*` or `beads_*` or `kbeads_*` or `vmagent_*` or `vmsingle_*` from the remote_write — this prevents accidental ingestion of, e.g., postgres or NATS exporter metrics if we ever add them and they pollute the long-retention store. See sample snippet in §11.

**Label enrichment** — the existing scrape config writes `namespace`, `pod`, `component`, `instance_name`. For the umbrella's v1 metrics ("spend per agent per hour"), `agent_name` is what we want — the controller emits it as a metric label already. No vmagent-side enrichment needed.

**Buffer-and-forward resiliency** — vmagent's default `-remoteWrite.tmpDataPath` is in-memory. For real-world resiliency we should add a small (e.g., 1 Gi) `emptyDir` mount and `-remoteWrite.tmpDataPath=/tmp/vmagent` so the buffer survives container restarts in the same pod. This is a small followup; not blocking v1 enablement. Track as `kd-...` follow-up bead.

### Q6 — vmalert / alerting

**Out of v1 scope.** Note the integration path:

- vmalert reads rules from a ConfigMap, evaluates them against vmsingle, and posts alerts to an Alertmanager.
- The ACME observability chart already runs **kube-prometheus-stack's Alertmanager** (`alertmanager.enabled: true`, with PVC + Slack notifiers configured). When we want gardenio alerts:
  1. Add a `vmalert` Deployment in the gardenio chart (we have the precedent — same single-binary pattern as vmsingle/vmagent).
  2. Point vmalert at `<release>-vmsingle:8428` for queries.
  3. Point vmalert at the existing `kube-prometheus-stack` Alertmanager (cross-namespace, ClusterIP) for notifications. The receiver routing tree there can route by label match on `team=gardenio`.
- Initial alert candidates: `gardenio_account_throttled > 0 for 10m`, `gardenio_session_cost_usd:rate1h > $X for 1h` (budget guard), `up{job="gardenio-controller"} == 0 for 5m`.

**Do not deploy a second Alertmanager.** Re-use the ACME one. This matches the "VictoriaMetrics K8s stack" config at `observability/values.yaml:317` (`alertmanager.enabled: false # Use kube-prometheus-stack Alertmanager`).

### Q7 — Auth + network

**v1: in-cluster only.** The data plane (vmsingle, vmagent) never gets an Ingress.

- **Read access** (queries, dashboards): only via Grafana, whose IngressRoute is already gated by Keycloak forwardAuth (per `templates/grafana/ingressroute.yaml`). Production should set `grafana.ingress.ipWhitelist.enabled: true` *in addition* to forwardAuth — defense in depth, matches beads-viewer's pattern. Production Grafana lives at e.g. `gardenio-grafana.<env>.acme.example`.
- **Write access**: only inside the cluster.
  - Pods scraped by vmagent: scrape-ingress is allowed by NetworkPolicy carve-outs (`vmagent` selector → `:9091`, `:9092`). Scraping itself is unauthenticated — the NetworkPolicy is the boundary.
  - `gb session` push: agent pod → vmagent (in-cluster). The agent egress NetworkPolicy must permit `vmagent.gardenio.svc:8429` — *check* this is added when enabling vmagent (the existing template already references vmagent in agent egress carve-outs, see `networkpolicies.yaml`).
  - vmagent remote-write to vmsingle: same namespace, no NP delta.
- **vmauth**: defer. Reasons:
  - v1 has no off-cluster consumer of the raw `/api/v1/query` API. The chart backend (umbrella's Phase-2 deliverable) is the broker.
  - vmauth is an extra layer of yaml-driven RBAC ahead of vmsingle — useful for per-tenant query budgets, useless for our single-tenant case.
  - When the iOS chart consumer or external dashboard wants to hit `/api/v1/query_range` directly, **that** is the moment to add vmauth. At that point: vmauth listens on an Ingress (not vmsingle), the chart backend issues per-token configs, and vmauth proxies to vmsingle internally.
- **Tailscale path** — we do not currently expose any data-plane component via Tailscale. beads-viewer uses Keycloak + IP allowlist via Traefik; same pattern works for Grafana and (later) vmauth. Don't add Tailscale to vmsingle.

### Q8 — Backup / DR

**vmbackup not in v1.** Justification:

- Every metric in vmsingle is reproducible: scraped state is re-derived from the controller; pushed state is a final-snapshot write per session and is duplicative with the per-bead session-cost record stored in postgres (the controller already maintains its own session-cost ledger).
- Acceptable RPO at v1: "since last `helm upgrade`" — losing a few hours/days of metric history in exchange for not running a backup pipeline is a reasonable tradeoff for an _internal observability_ store at this scale.
- Acceptable RTO: ~5 min (StatefulSet + PVC redeploy; if PVC is also lost, vmsingle restarts empty and re-fills from forward scrape).

**v2 path (when historical retention becomes load-bearing for cost-attribution evidence or when a regulator asks):** add vmbackup as a sidecar in the vmsingle StatefulSet, snapshot to an `s3://gardenio-vm-backups-<env>/` prefix daily, retain 30 dailies + 12 monthlies. ExternalSecret-managed AWS credentials. vmrestore tooling already exists for the inverse path. Cost: ~$2/mo at 50 Gi compressed snapshots.

### Q9 — Existing Prometheus / monitoring in the cluster

This is the most important demarcation question; spelling it out.

**ACME ecosystem level (`observability` namespace, deployed by `acme-helm-chart/charts/observability`):**

| Component | Deployed | Retention | Purpose |
|---|---|---|---|
| `kube-prometheus-stack` Prometheus | yes | 15d / 45 GiB | Cluster-wide scraping, infra metrics, kube-state-metrics, node-exporter |
| `kube-prometheus-stack` Alertmanager | yes | n/a | Cluster-wide alert routing + Slack |
| `kube-prometheus-stack` Grafana | yes | n/a | Cluster-wide dashboards (postgres-backed) |
| `victoria-metrics-k8s-stack` vmcluster | yes | 14d, RF=2 | HA metrics storage; replaces Prometheus on the read path; selector-driven scraping |
| `victoria-metrics-k8s-stack` vmsingle | **no** (`vmsingle.enabled: false`) | — | Disabled in favor of vmcluster |
| Tempo | yes | 72h | Distributed tracing |
| VictoriaLogs | optional, currently `enabled: false` | — | Centralized logs |

**Gardenio chart level (`gardenio` namespace):**

| Component | Today | After v1 enable |
|---|---|---|
| vmsingle | shipped, disabled | enabled, **1y retention**, gardenio_*/beads_* only |
| vmagent | shipped, disabled | enabled, scrapes only the gardenio namespace, filters to `gardenio_*`/`beads_*`/`kbeads_*` before write |
| Grafana | shipped, disabled | enabled per-env (only enable in `gardenio-rwx`/`gardenio-prod`); pre-provisioned with the two cost/embedding dashboards |
| ServiceMonitor | shipped, disabled | leave disabled — vmagent's K8s SD already covers our pods, and the ACME Prometheus does **not** need to scrape gardenio metrics (it would fight retention with our store) |

**Coexistence rule:** vmagent **only** scrapes the gardenio namespace and **only** writes `gardenio_*`/`beads_*`/`kbeads_*` (and `vmagent_*`/`vmsingle_*` for self-monitoring) to the in-chart vmsingle. The ACME-level kube-prometheus-stack continues to scrape infra metrics from gardenio pods (CPU, memory, restarts) at 15d retention — *no overlap with the long-retention business metrics*. This is the inverse of the comment at the top of `vmsingle:` in `values.yaml` which assumes the ACME Prometheus *remote-writes* gardenio metrics to gardenio's vmsingle; that is one valid design but the **vmagent-direct-scrape** path is simpler and already wired in our chart, so prefer it.

**Grafana question:** do we want a *separate* gardenio Grafana, or do we register the vmsingle datasource into the ACME observability Grafana? Both work. The chart ships its own Grafana (with our two pre-built dashboards). Recommended split:

- **dev / e2e / rwx**: enable the in-chart Grafana. Operator gets a self-contained env they can stand up and tear down without touching observability infra.
- **prod**: register vmsingle as a datasource in the ACME observability Grafana. Reasons: one Grafana to log into, dashboards-as-code via the same provisioning path, postgres-backed Grafana state is more durable than the chart's optional PVC.

**Will VM coexist or replace?** Coexist. Phasing out kube-prometheus-stack is out of scope and not desirable — it serves the platform team, not gardenio.

### Q10 — Cost: rough storage + CPU footprint

At v1 sizing, in `us-east-1` AWS pricing (April 2026):

| Resource | Size | $/mo | Notes |
|---|---|---|---|
| vmsingle PVC (gp3, 50 Gi) | 50 Gi | $4.00 | gp3 base price $0.08/GiB-mo; baseline IOPS/throughput is plenty |
| vmsingle pod | 100m / 256Mi req | $0–2 | Fits in spare worker headroom; depends on whether the cluster autoscales vs. fixed-size |
| vmagent pod | 50m / 64Mi req | $0–1 | Same |
| Grafana pod | 50m / 64Mi req | $0–1 | Only enabled in 1–2 envs |
| Optional vmbackup S3 (v2) | ~50 Gi compressed × 1 daily + 12 monthly | $2.00 | Out of v1 scope |
| **Total v1, per env** | | **< $10/mo** | First-year EBS dominates; compute is rounding |

Operationally cheap enough that retention can be 1y from day one without a separate cost discussion. If anything, the operator should ask whether the *ACME* `kube-prometheus-stack` should grow its retention from 15d (currently 50 GiB / 15d ≈ $4/mo) — but that's a different epic.

---

## 4. Recommendation block — sample values.yaml snippets

The chart's existing scaffolding means a working v1 is a values-only change. Drop into `acme-helm-chart/charts/gardenio/values/<env>.yaml` (under the `gardenio:` subchart key, since that wrapper chart depends on the gardenio subchart):

```yaml
# === Long-retention metrics: in-chart vmsingle + vmagent ===

vmsingle:
  enabled: true
  # Use the chart's gardenio-storage class (gp3, ebs.csi.aws.com, expandable).
  # Leave storageClass empty so global.storageClass / chart default applies.
  retentionPeriod: "1y"
  persistence:
    enabled: true
    size: 50Gi
    storageClass: ""
  resources:
    requests:
      cpu: 100m
      memory: 256Mi
    limits:
      cpu: 500m
      memory: 512Mi
  # Schedule on amd64 worker nodes (same as the rest of the gardenio stack).
  nodeSelector:
    kubernetes.io/arch: amd64

vmagent:
  enabled: true
  remoteWriteURL: ""  # auto-wires to in-chart vmsingle via gardenio.vmsingle.writeURL
  resources:
    requests:
      cpu: 50m
      memory: 64Mi
    limits:
      cpu: 200m
      memory: 128Mi
  nodeSelector:
    kubernetes.io/arch: amd64

# === Producer: agent push ===
# The controller already plumbs VMAGENT_URL onto agent pods when this is set;
# wire the cluster-internal vmagent service URL.
controller:
  env:
    VMAGENT_URL: "http://gardenio-vmagent.gardenio.svc:8429"

# === Optional: Grafana (enable in dev/rwx; defer in prod in favor of ACME Grafana) ===
grafana:
  enabled: true
  adminPasswordSecret: "gardenio-grafana-admin"  # ExternalSecret synced from AWS SM
  persistence:
    enabled: true
    size: 1Gi
  ingress:
    enabled: true
    host: "gardenio-grafana.<env>.acme.example"
    ipWhitelist:
      enabled: true
      sourceRange:
        - "<office-egress-cidr>"
  sidecar:
    enabled: true   # auto-loads dashboards/cost-overview.json + dashboards/kbeads-embedding.json
```

**Per-environment phasing:**

| Env | vmsingle | vmagent | grafana | Notes |
|---|---|---|---|---|
| `gardenio-test` | enable | enable | disable | Smoke test the writers/scrapers |
| `gardenio-rwx` | enable | enable | enable | Operator-facing dashboards; can validate forwardAuth path |
| `gardenio-prod` | enable | enable | **defer** — register vmsingle as datasource in ACME Grafana | Single-Grafana convergence in prod |
| `gardenio-next` | follow `gardenio-rwx` | follow | follow | Stays config-mirrored to RC per the `gardenio-next` epic |

**vmagent metric filter to add** (small followup, not blocking — file as a successor task):

```yaml
# In templates/vmagent/configmap.yaml, append to the relabel_configs in the
# kubernetes-pods job:
- source_labels: [__name__]
  regex: '(gardenio|beads|kbeads|vmagent|vmsingle|process)_.*'
  action: keep
```

---

## 5. Open decisions to escalate

These are the forks the gardener should turn into a `gb decision` before the implementation phase begins:

1. **PVC size: 25 Gi vs 50 Gi vs 100 Gi.** Recommended 50 Gi. Cost difference < $5/mo; 25 Gi shaves $2/mo at the cost of having to expand within ~6 months of growth; 100 Gi is over-provisioning. Operator may have an opinion.
2. **Retention: 90d vs 1y vs 2y.** Recommended 1y. Operator-stated v1 metric ("spend per agent per hour") rewards long retention; the 1.6 GiB/year footprint is rounding error. 90d is the chart default and a defensible "let's see what we use" answer.
3. **Prod Grafana: in-chart vs ACME-shared.** Recommended ACME-shared in prod, in-chart in dev/rwx. Trade-off: operational unity vs blast-radius isolation.
4. **Do we add `gardenio_beads_event_total`** as a controller-side counter, or derive it via vmagent recording rule? Recommend: instrument it. ~30 lines of Go in `controller/internal/metrics/metrics.go`. Recording-rule path means the metric only exists post-evaluation, which complicates dashboards.
5. **Drop-list filter on vmagent's remote_write** — adopt now or defer? Recommend: adopt at enable time. One-line config; protects long-retention store from accidental pollution.

---

## 6. Successor tasks (to be filed by the synthesis gardener)

Pre-flight (research-only, can be done before implementation gate):

- `T-vm-1` — *Survey existing pod annotations.* Confirm every gardenio-namespace pod that exposes `/metrics` carries `prometheus.io/scrape=true` and that the port is consistent with vmagent SD expectations. (controller, beads, slack-bridge, gitlab-bridge, jira-bridge, coopmux, beads3d.)
- `T-vm-2` — *Beads-event counter.* Spec the `gardenio_beads_event_total{type, project}` counter on the controller side: which event source feeds it (controller's NATS subscriber? a kbeads /metrics shim?), and what bucket of types it covers. This is required for the umbrella v1 metric.
- `T-vm-3` — *vmagent buffer durability.* Decide whether vmagent gets a small `emptyDir` or PVC-backed `tmpDataPath`. Recommend `emptyDir` 1 Gi.
- `T-vm-4` — *ACME-Grafana datasource registration.* Spec how the prod gardenio vmsingle ClusterIP becomes a datasource entry in the ACME observability Grafana — likely via the observability chart's datasource provisioning, with values added to `acme-helm-chart/charts/observability/values/<env>.yaml`.

Implementation (gate behind operator approval):

- `T-vm-5` — *Enable vmsingle+vmagent in `gardenio-test`.* Wrapper chart values change in acme-helm-chart only; gardenio repo is untouched.
- `T-vm-6` — *Enable in `gardenio-rwx`* (with Grafana).
- `T-vm-7` — *Enable in `gardenio-prod`* (without Grafana; with ACME Grafana datasource).
- `T-vm-8` — *Add the metric-name allow-list relabel* to vmagent's scrape config.
- `T-vm-9` — *Author the chart-backend → vmsingle query path* (umbrella's Phase-2 deliverable; not this research).

---

## 7. Sources

- `gardenio/helm/gardenio/values.yaml` lines 962–1118 — existing vmsingle/vmagent/grafana stanzas.
- `gardenio/helm/gardenio/templates/{vmsingle,vmagent,grafana,networkpolicies}/` — existing K8s manifests.
- `gardenio/controller/internal/metrics/metrics.go` — emitted metric names and labels.
- `gardenio/controller/cmd/gb/session_metrics_push.go` — push protocol + payload format.
- `kbeads/internal/metrics/embedding.go` and `kbeads/internal/server/token_revocation_metrics.go` — beads-side metric names.
- `acme-helm-chart/charts/observability/values.yaml` — kube-prometheus-stack + victoria-metrics-k8s-stack baseline.
- `acme-helm-chart/charts/gardenio/values/*.yaml` — wrapper-chart per-env conventions.
- VictoriaMetrics docs: vmsingle vs vmcluster scaling guidance, vmbackup/vmrestore, vmauth — no link inlined; pinned to v1.108.1 as currently used in the chart.
- Bead history: ka-0000000179 (this task), ka-0000007704 (umbrella).
