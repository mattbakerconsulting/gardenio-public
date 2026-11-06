# Perf → gardenio vmsingle metrics federation

> Design / ADR for epic [[ka-0000004560]], design task [[ka-0000002749]].
> Scope decision [[ka-0000005605]] resolved **push-both**: copy *both* the perf
> application series *and* the perf DB (postgres/RDS) series into the gardenio
> VictoriaMetrics surface, not a read-only datasource.
> Sibling precedent: `obs-w1-site-remote-write.md` (site → gardenio, the mirror
> direction). Rollout: e2e gardenio first ([[ka-0000005301]]), then the new prod
> gardenio ([[ka-0000002272]]).

## TL;DR

Add a **second scrape job** to the gardenio in-chart `vmagent` that pulls the
perf VictoriaMetrics `/federate` endpoint over the public `perf.acme.example` host
with basic auth, drops the high-cardinality trace buckets, stamps a
`source="perf"` label, and lets the existing `vmagent → vmsingle` remote-write
land the series. No new component, no perf-side change, no PMM access required.

```text
perf vmsingle (perf.acme.example)        gardenio (EKS)
  /federate  ──basic_auth, pull──▶  vmagent (NEW scrape job "perf-federation")
                                       │ writeRelabel: drop span_* buckets, stamp source="perf"
                                       ▼
                                     vmagent remote_write (existing)
                                       ▼
                                     gardenio-vmsingle  (gardenio_* / beads_* / perf_* coexist)
                                       ▼
                                     gardenio Grafana (existing datasource)
```

## 1. Why pull-federation, not push remote_write

"push-both" in the decision names the *intent* (a real copy into the gardenio
VM, both surfaces) — not the wire direction. Two directions could achieve a
copy:

| Option | Mechanism | Verdict |
|---|---|---|
| **A. perf-side remote_write → gardenio ingest** | mirror of W1: stand up the vmsingle ingest IngressRoute on gardenio, then add a second `remote_write` on the *perf* env vmagent | **rejected for now** — requires a human to land terraform in the perf account (gardenio never writes perf TF), and exposes a public write endpoint on gardenio. Heavy for a metrics copy gardenio can pull itself. |
| **B. gardenio-side pull via `/federate`** | one extra scrape job on the *existing* gardenio vmagent, pulling perf's public `/federate` | **chosen** — entirely inside this chart, zero perf-side change, no new ingress surface, reuses the vmagent→vmsingle remote-write that already exists. |

Option B is the lighter, fully self-service path and still produces a real copy
in gardenio-vmsingle (queryable offline from perf, retained 1y) — which is the
substance of "push", just sourced by a pull. If perf ever firewalls its public
VM, we fall back to Option A (the W1 receiver scaffolding already exists behind
`vmsingle.ingest.enabled`).

## 2. Source reality (probed live 2026-06-16)

`GET https://perf.acme.example/victoria-metrics/...` with
`VICTORIA_METRICS_PERF_USERNAME:VICTORIA_METRICS_PERF_PASSWORD`:

- `/federate?match[]=up` → **HTTP 200** (federation endpoint is open).
- `/api/v1/status/tsdb` → **33,728 active series**, 122k total, 1,084 distinct
  metric names.
- **Both decision surfaces are already in this one VM** (the prior epic
  [[ka-0000004697]] populated it):
  - **App series**: `fastapi_*`, `jvm_*` (×57), `otelcol_*` (×46), `http_*`,
    `rpc_*`, `agroal_*`, `netty_*`, `worker_*`.
  - **DB series**: `pg_*` (×397, including `pg_stat_statements_*`),
    `amazonaws_*` (×129 — AWS/RDS CloudWatch), `cloudwatch_*`.
  - **Keycloak**, `ecs_*` (×47, container insights), `process_*`, `go_*`.
- Series already carry clean external labels: `environment="perf"`,
  `cluster="shared-perf"|"k6-perf"`, `customer`, `service`, `job`, `platform`.
  These do **not** collide with gardenio's `env`/`cluster` external_labels.

### PMM / QAN is intentionally out of scope of the wire

The standalone PMM box (`K6_PERF_PMM_URL=https://52.55.59.69`) is
**bastion-gated** — a direct connect from a gardenio pod gets
`Connection reset by peer`. Two reasons that is fine:

1. PMM's postgres_exporter scrape (`pg_*`) is *the same data already in the perf
   VM* — we get the DB metrics surface without touching PMM.
2. True QAN *query digests* are not Prometheus series (`digest`/`qan` metric
   names are absent from the perf VM); QAN lives in PMM's ClickHouse and is not
   federatable as time-series anyway. Reproducing QAN in gardenio Grafana would
   be a separate ClickHouse-ingest project, explicitly **not** this epic.

So "DB metrics" is satisfied by the `pg_*` + `amazonaws_*` families in the perf
VM. This is called out as a scope boundary in §6.

## 3. Cardinality & retention budget

| Family | Series (≈) | Federate? |
|---|---|---|
| `span_duration_milliseconds_bucket` | 14,064 | **drop** — trace histograms; belong in VictoriaTraces, not vmsingle |
| `span_*` (calls/count/sum) | ~3,500 | **drop** — same |
| `pg_*` (incl. `pg_stat_statements`) | per `pg_` family | keep |
| `amazonaws_*` / `cloudwatch_*` (RDS) | per family | keep |
| `fastapi_*`, `jvm_*`, `otelcol_*`, `http_*`, `rpc_*`, app | remainder | keep |
| keycloak, `ecs_*`, `process_*`, `go_*` | remainder | keep |

Net after dropping `span_*`: **~17k series**. Against a 50Gi gp3 / 1y vmsingle
that today holds only `gardenio_*`/`beads_*`/`kbeads_*` (low-thousands of
series), ~17k extra is a small fraction of headroom — gp3 supports online
expansion (ADR D2) if it ever grows. **No retention change needed**; perf series
inherit the chart's 1y.

The `span_*` drop is the single most important relabel — it removes ~50% of perf
series for free and keeps trace data where it belongs.

## 4. Label namespacing

Every federated series gets `source="perf"` stamped via `writeRelabel` so a
gardenio dashboard can always filter perf-origin series and they never alias with
gardenio's own. Perf's own `environment="perf"`/`cluster=...` labels are
preserved (they don't collide), so PromQL like
`pg_database_size_bytes{source="perf", cluster="shared-perf"}` is unambiguous.

`honor_labels: true` on the federation job so perf's `instance`/`job` survive
instead of being overwritten by the scrape target address.

## 5. Implementation shape (for [[ka-0000005301]])

A `vmagent.extraScrapeConfigs` value (new, default empty) appended into
`templates/vmagent/configmap.yaml`, plus a basic-auth Secret via the existing
ExternalSecret pattern. The e2e overlay (`deploy-values/gardenio.yaml`) opts in:

```yaml
vmagent:
  enabled: true
  # NEW: federate the perf VM. Default empty in values.yaml — chart renders
  # identically until an env sets this.
  extraScrapeConfigs:
    - job_name: perf-federation
      honor_labels: true
      scrape_interval: 60s
      metrics_path: /victoria-metrics/federate
      scheme: https
      basic_auth:
        username_file: /etc/vmagent-perf/username
        password_file: /etc/vmagent-perf/password
      static_configs:
        - targets: ["perf.acme.example"]
      params:
        # Pull everything that is up, then drop trace buckets in relabel.
        'match[]':
          - '{__name__=~"pg_.+|amazonaws_.+|cloudwatch_.+|fastapi_.+|jvm_.+|otelcol_.+|http_.+|rpc_.+|keycloak.+|ecs_.+|up"}'
      relabel_configs: []
      metric_relabel_configs:
        - source_labels: [__name__]
          regex: 'span_.+'
          action: drop
      write_relabel_configs:
        - target_label: source
          replacement: perf
```

Secret (mirror W1's ExternalSecret) materializes `username`/`password` keys from
the secret store into a Secret mounted at `/etc/vmagent-perf`. The plaintext
values are the same `VICTORIA_METRICS_PERF_*` already provisioned to agent pods;
they get a store entry (e.g. `gardenio/perf-vm-federation`) for the chart.

> Note: VictoriaMetrics vmagent supports `username_file`/`password_file`; if the
> pinned vmagent (v1.108.1) needs inline `username`/`password`, mount the Secret
> as env and template them — the implementer should confirm against the vmagent
> version when wiring [[ka-0000005301]].

Validation gate for the e2e task:
1. `vmagent` `/targets` shows `perf-federation` UP.
2. `count({source="perf"})` > 0 in gardenio-vmsingle; `count({__name__=~"span_.+", source="perf"})` == 0.
3. A perf dashboard (or a panel on an existing one) renders `pg_*` + a `fastapi_*`
   latency series in gardenio Grafana.

## 6. Scope boundaries (explicit)

- **QAN query-digest analytics** (PMM ClickHouse) are **not** in scope — not
  federatable as Prometheus series. If wanted, file a separate epic.
- **No perf-side change.** This is pull-only; perf TF is untouched.
- **Prod cutover** ([[ka-0000002272]]) is the same scrape job in the prod
  gardenio overlay + the secret-store entry in the prod store, gated on the new
  prod gardenio existing.

## 7. Rollback

Set `vmagent.extraScrapeConfigs: []` (or remove the overlay block) and
redeploy — vmagent drops the job, no perf series are written going forward.
Already-written perf series age out at the 1y retention or can be deleted via
vmsingle's `/api/v1/admin/tsdb/delete_series?match[]={source="perf"}`.
