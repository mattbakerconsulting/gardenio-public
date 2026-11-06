# Victoria observability query reference (ACME traces / metrics / logs)

How to extract traces, metrics, and logs from the ACME VictoriaTraces / VictoriaMetrics /
VictoriaLogs stack in any environment. Each store exposes a Jaeger, PromQL, or LogsQL API
behind HTTP Basic auth from `VICTORIA_*` env vars injected on monorepo agent pods.

Source advice beads: `ka-0000005045` (dev query cookbook), `ka-0000000864` (multi-env trace
extraction + inventory). See also `ka-0000005080` (dev/uat OTEL access + secret-fetch flow)
and `gardenio/docs/agent-operations.md`.

## Environments & endpoints

| Environment | Base domain | Env-var suffix | Retention |
|---|---|---|---|
| **Prod** | `lens.acme.net` | `_PROD_` | 7d |
| **Perf** | `perf.acme.example` | `_PERF_` | 7d |
| **Dev**  | `dev.acme.example`  | `_DEV_` (also unsuffixed) | 7d |

Each domain hosts three backends behind a vmauth proxy: `/victoria-traces` (OTLP/Jaeger),
`/victoria-logs` (LogsQL), `/victoria-metrics` (PromQL).

## Auth

All requests use HTTP Basic auth. Env vars per environment (`{ENV}` = `PROD`/`PERF`/`DEV`):
`VICTORIA_TRACES_{ENV}_URL` / `_USERNAME` / `_PASSWORD` (and `_METRICS_`, `_LOGS_`). Dev is
also available unsuffixed. These are injected on every monorepo agent pod from the
`monorepo-victoria-{traces,logs,metrics}-{env}-credentials` K8s secrets — if a query seems
blocked, check the env vars first. (Secrets live in AWS Secrets Manager, dev account
234567890123, pattern `gardenio/monorepo-victoria-{traces,logs,metrics}-{env}-credentials`.)

```bash
# Traces (Jaeger) — list services
curl -su "$VICTORIA_TRACES_PERF_USERNAME:$VICTORIA_TRACES_PERF_PASSWORD" \
     "$VICTORIA_TRACES_PERF_URL/select/jaeger/api/services"
# Metrics (PromQL)
curl -su "$VICTORIA_METRICS_PERF_USERNAME:$VICTORIA_METRICS_PERF_PASSWORD" \
     "$VICTORIA_METRICS_PERF_URL/api/v1/query?query=up"
# Logs (LogsQL)
curl -su "$VICTORIA_LOGS_PERF_USERNAME:$VICTORIA_LOGS_PERF_PASSWORD" \
     "$VICTORIA_LOGS_PERF_URL/select/logsql/query?query=*&limit=50"
```

## Services & what they trace

- **site-api** (Python/FastAPI, Harmony site backend): HTTP `/tsda/*`, `/isf/*`, `/utils/*`;
  SQL statements with code location; DB connections. NO user-context tags.
- **sponsor-api** (Python/FastAPI, Unity sponsor backend): HTTP `/sysAdmin/*`, `/ctms/*`,
  `/etmf/*`, `/encoder/*`, `/user/*`; SQL with code location; HAS user context (`user.id`,
  `user.email`, `user.name`, `role.name`, `role.id`).
- **study-web** (React SPA): HTTP fetches to both backends (full URL paths, not
  parameterized), page navigations, click events, frontend errors, 30-min session tracking.

## Jaeger query API

Base `$VICTORIA_TRACES_URL/select/jaeger`:
- `GET /api/services` — list services
- `GET /api/services/{service}/operations`
- `GET /api/traces?service=sponsor-api&lookback=1h&limit=20`
- `GET /api/traces?service=sponsor-api&minDuration=5s&limit=10` — slow requests
- `GET /api/traces/{traceID}` — one trace (full 32-hex; a 16-hex short id 404s)

### Tag filters

Pass tags as JSON in the `tags` param. **Only single-tag filters work reliably — a
multi-tag JSON object returns 0 results.**

- HTTP (server): `{"http.status_code":"500"}`, `{"http.route":"/ctms/queries/{study_id}/select"}`, `{"http.method":"POST"}`
- SQL (client): `{"db.system":"postgresql"}`, `{"db.name":"unty-phlth-devel-sponsor"}`, `{"code.function":"db_select_with_all_grants"}`
- User context (sponsor-api only): `{"user.email":"…"}`, `{"role.name":"Data Manager"}`
- Frontend (study-web): `{"error":"true"}`, `{"page.path":"/conduct/patients"}`, `{"session.id":"<uuid>"}`, `{"category":"click"}`

## Prometheus metrics API

Base `$VICTORIA_METRICS_URL`:
- `GET /api/v1/query?query=up` — instant
- `GET /api/v1/query_range?query=up&start=T&end=T&step=15s` — range
- `GET /api/v1/label/__name__/values` — list metric names

The OTEL collector generates span metrics (`span_*`) with histogram buckets [5ms–10s] sliced
by db.system, db.operation, db.name, http.method, http.status_code, http.route.

## Logs correlation

Traces link to logs via `otelTraceID`:

```bash
curl -su "$VICTORIA_LOGS_USERNAME:$VICTORIA_LOGS_PASSWORD" \
  "$VICTORIA_LOGS_URL/select/logsql/query" -d "query=otelTraceID:<traceID>"
```

**Full-text search does NOT work for structured fields** like
`resource_attr:deployment.namespace` — fetch with `query=*` and filter client-side.

## Trace structure

Typically 3 levels: study-web (navigation/session) → study-web (HTTP fetch) → site-api or
sponsor-api (FastAPI handler) → SQL + DB-connect spans. Distributed traces cross services via
the W3C traceparent header; the browser orchestrates all cross-service calls (no direct
sponsor-api↔site-api). ASGI http send/receive spans are lifecycle noise — focus on the server
span for timing.

## Limitations

- One tag per query (multi-tag returns 0).
- `/api/dependencies` is not populated.
- study-web operations use full URL paths, so each unique URL is a separate operation.
- site-api spans lack user.id/user.email/role.name (sponsor-api only).

## Architecture & terraform

```
service → otel-collector (Service Connect) → https://{domain}/victoria-traces/insert/opentelemetry/v1/traces
                                              ↓ (vmauth proxy, path routing + auth)
                                         victoria-traces (EFS-backed)
```

Per-tenant otel-collectors forward to a shared per-environment Victoria backend. Terraform:
`site-deployment/terraform/new-modules/ecs-services/{victoria-traces,vmauth,observability}/`;
env configs `site-deployment/terraform/live/{prod,perf,devel}/env_cfg.hcl`.

## Prod/dev/perf app inventory (as of 2026-04-08, DEVOPS-547 audit 2026-05-01)

- **Prod (`lens.acme.net`):** sponsor-sandbox (sponsor/site/study, ~7.7k spans orig but only 2 in
  the 2026-05-01 sample — possibly idle/drained); shared-prod keycloak 26.1.4; phlth-prod
  sponsor-api; phlth2tst-prod sponsor-api (~20 spans/7d, added 2026-05-24); sponsor-prod
  sponsor-api. Tracing enabled only for `acme/sponsor-next` and `gsk/sponsor-next`;
  legacy `sponsor`/`study` deployments have no OTEL tracing.
- **Perf (`perf.acme.example`):** k6-perf (sponsor/site/study, tenant k6, ~9.4k spans/7d, very high
  during load tests).
- **Dev (`dev.acme.example`):** phlth-devel (~8k), prvl-qa (~1.3k), mgtn-qa (~97), phlth-qa (~14).
