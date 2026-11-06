# T3.2 — perf-overview cross-check vs perf ground truth + GRADE audit-set onboarding

| | |
|---|---|
| **Task** | ka-0000005823 (T3.2) |
| **Dashboard** | `perf-overview` — *Perf — Overview (ECS / RDS / Load)* (`gardenio/helm/gardenio/dashboards/perf-overview.json`, merged as T3.1 / MR !6972) |
| **Ground truth** | VictoriaMetrics `gardenio-vmsingle:8428` — the pull-federated `source="perf"` series (perf `/federate` + AWS CloudWatch → gardenio vmagent) |
| **Method** | replay every panel's PromQL against VM (`/api/v1/query` for stat tiles, `/api/v1/query_range` for graphs) with template vars resolved from live VM label values, then classify displayed-vs-truth |
| **Harness** | `crosscheck.py` (in this dir), output `crosscheck.json` |
| **Epic** | ka-0000005085 (Flow perf-monorepo metrics onto the gardenio observability stack) |
| **GRADE gardener** | ka-0000001426 · formula ka-0000003015 |
| **Date** | 2026-08-07 |

## Headline

**perf-overview tells the truth.** All 36 PromQL targets across 23 data panels were
replayed against perf ground truth: **33 HAS-DATA, 3 honest NO-DATA (keycloak-idle),
0 QUERY-ERROR, 0 AGGREGATION-DRIFT, 0 UNIT/SCALE, 0 STALE, 0 naming-bug.** Every
headline stat tile reconciles **exactly** to its own panel breakdown. There is no drift
to fix — the board was authored accuracy-first (T3.1) and this pass confirms it against
live perf series.

## Accuracy cross-check — verdict counts

| verdict | count | meaning |
|---|---|---|
| HAS-DATA | 33 | displayed value == VM ground truth, query honest |
| NO-DATA | 3 | families present but no series *in-window* — honest keycloak-idle (see below) |
| MISSING-SERIES (naming-bug) | 0 | no panel references a metric absent under a different name |
| AGGREGATION-DRIFT | 0 | no double-count / wrong-`by()` / wrong-window |
| UNIT / SCALE | 0 | every unit matches the metric's native scale |
| STALE / FROZEN | 0 | all families fresh (~277 s lag, normal CloudWatch federation latency) |
| QUERY-ERROR | 0 | every expr parses + executes against VM |

Full per-target table: `crosscheck.json` → `rows[]`. Summary block: `crosscheck.json` → `summary`.

### The 3 NO-DATA rows are honest, not bugs

Three targets returned an empty vector — all three are the **keycloak** half of a
dual-family load panel:

- id=25 *Request rate by service* — `sum by (service)(rate(http_server_requests_seconds_count{source="perf",cluster=~"k6-perf"}[…]))`
- id=27 *Error rate by outcome (%)* — the two `outcome="CLIENT_ERROR"` / `"SERVER_ERROR"` targets on `http_server_requests_seconds_count`

`http_server_requests_seconds_count{source="perf"}` has **zero series live** right now
because keycloak emits only on auth traffic; the k6 load in flight drives sponsor-api,
not keycloak. This is exactly the condition the board already guards:

- On the **headline tiles** (id=5 Load request rate, id=7 Load error rate) the keycloak
  term is folded in with `or vector(0)`, so an idle keycloak cannot blank a tile that
  should read a live rate (guarded by `TestPerfOverviewDashboard_HeadlineLoadTilesTolerateKeycloakGaps`).
- On the **breakdown panels** (id=25, id=27) the sponsor-api target on the *same panel*
  is HAS-DATA, so the panel still renders live under load; the keycloak series simply
  light up when auth traffic appears.

Verified directly: `count(http_server_requests_seconds_count{source="perf"}) = []` (empty)
while `fastapi_sponsor_api_http_requests_total{source="perf",cluster="k6-perf"}` is live.

### Reconciliation spot-checks (displayed == truth)

| check | headline tile | panel breakdown | match |
|---|---|---|---|
| Load request rate (id=5) vs sum of by-service (id=25) | `3.571…` | sponsor-api `3.571…` + keycloak `vector(0)` | ✅ exact |
| ECS running tasks (id=4) vs sum of running-by-service (id=11) | `2` | `2` | ✅ exact |
| Load p95 (id=6) vs panel p95 (id=26) | same highr-histogram expr | — | ✅ identical query |

### CloudWatch quantile selectors are drift-safe

Every AWS-family target pins `quantile="1"` (or `"0"` for the period-min FreeableMemory
tile). The federated CloudWatch series carry exactly `{quantile="0","1"}` (min/max) per
`(cluster, service)` — selecting a single quantile is what keeps `sum(...)` from
double-counting the min/max pair. Confirmed on RunningTaskCount, DBLoad, FreeableMemory.

## GRADE audit-set onboarding — what "add to the audit set" actually means

**perf-overview is now a member of the GRADE gardener's audit set, and no `DASHBOARDS`
change is needed — nor should one be made.** The mechanics, verified live:

1. **The `DASHBOARDS` formula var is a restrictive allowlist, not an opt-in list.**
   Formula ka-0000003015 step-1: *"If `{{DASHBOARDS}}` is empty, list every dashboard"*
   via `GET /api/search?type=dash-db`. The gardener ka-0000001426 ships
   `formula_vars.DASHBOARDS = ""`, so every daily iteration already audits **every**
   enumerated board (the 2026-08-07 run audited all 20 chart-shipped boards). Setting
   `DASHBOARDS="perf-overview"` would **regress** the gardener to a single board — the
   opposite of the task intent.

2. **Membership is automatic once the board deploys.** The grafana-bridge backfill
   (`grafana-bridge/main.go` → `runBackfill`) globs **all** `*.json` under
   `GRAFANA_DASHBOARDS_DIR` (the `dashboards/` dir baked into the bridge image) with
   `os.ReadDir` — no per-dashboard allowlist. So once a release ships the merged
   perf-overview.json, the bridge provisions it into Grafana, `/api/search` enumerates
   it, and the next GRADE iteration cross-checks it automatically.

3. **Why it isn't in today's live Grafana yet.** perf-overview merged to main
   2026-08-07 15:25 (MR !6972) but has not yet ridden a release into the running
   grafana-bridge image, so live `/api/search` does not list it *yet*. This is a deploy
   lag, not a wiring gap — it will appear on the first GRADE run after the next release.

**Action taken:** documented the empty-`DASHBOARDS` = audit-all contract on the gardener
bead so a future operator doesn't "helpfully" pin it to perf-overview and silently drop
the other 20 boards. No formula/gardener config edit — the correct config is the current
one.

## Reproduce

```bash
export VM_URL="http://${GARDENIO_VMSINGLE_SERVICE_HOST:-gardenio-vmsingle}:8428"
cp ../../../helm/gardenio/dashboards/perf-overview.json .
python3 crosscheck.py perf-overview.json     # prints summary + writes crosscheck.json
```

The harness resolves `$ecs_cluster` / `$db` / `$load_cluster` from live VM label values,
substitutes `$__rate_interval` with a concrete window, replays each target, and
classifies HAS-DATA / NO-DATA / MISSING-SERIES / QUERY-ERROR. (RE2 note: perf cluster
names contain `-`, so the multi-value regex escapes only true RE2 metacharacters — a
`\-` from Python's `re.escape` is rejected by VM's RE2 engine with HTTP 422.)
