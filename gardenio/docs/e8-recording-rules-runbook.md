# E8 — Recording rules, dashboards & backfill runbook

**Epic:** ka-0000006536 · **Plan:** [`plans/timeseries-platform.md`](../plans/timeseries-platform.md) §2.E8
**Sources:** R1 (`docs/research/timeseries-victoriametrics-ops.md` §Q3/§Q6), R4 §5

This is the operational reference for the E8 polish layer of the time-series
platform: the vmalert recording-rule evaluator, the chart-pack Grafana
dashboards, and the 90-day beads-events backfill.

---

## 1. Recording rules (vmalert)

### What it is

`vmalert` is a single-binary VictoriaMetrics component (same deploy pattern as
`vmsingle`/`vmagent`) that evaluates recording rules against `vmsingle`
(`-datasource.url`) and remote-writes the recorded series back to the same
`vmsingle` (`-remoteWrite.url`). Neither `vmagent` nor `vmsingle` evaluate
recording rules on their own — that is what `vmalert` adds.

Chart wiring:

| File | Role |
|---|---|
| `helm/gardenio/templates/vmalert/deployment.yaml` | the evaluator workload (hardened: non-root, read-only rootfs, no SA token) |
| `helm/gardenio/templates/vmalert/configmap.yaml` | the rule file (`recording.yaml`) |
| `helm/gardenio/templates/vmalert/service.yaml` | ClusterIP `:8880` (web UI / `/metrics`) |
| `helm/gardenio/templates/networkpolicies/networkpolicies.yaml` | `vmalert-egress` → vmsingle:8428 + kube-dns; vmsingle ingress admits vmalert |

Enable with `vmalert.enabled: true` alongside `vmsingle` + `vmagent`. Prod has
this set in `deploy-values/gardenio.yaml`.

### The two rules

Authored deliberately small — only the queries demonstrably hot on the live
`/charts` path and the cost-overview dashboard (do **not** pre-author
speculatively; R1 §Q3):

```
gardenio:agent_cost_usd:rate1h
  = sum by (agent, project) (increase(gardenio_agent_spend_usd_total[1h]))

gardenio:beads_events:rate5m
  = sum by (event, project, bead_type) (rate(gardenio_beads_events_total[5m]))
```

- `gardenio_agent_spend_usd_total` is the E3 per-turn spend counter
  (labels `{agent,project,role,model,account}`).
- `gardenio_beads_events_total` is the E4 beads-event counter
  (labels `{event,project,bead_type}`).

Naming follows the Prometheus `level:metric:operation` convention so the
recorded series never collide with the raw counters.

### Verify after deploy

```bash
# Pod healthy + rules loaded
kubectl -n gardenio get deploy gardenio-vmalert
kubectl -n gardenio port-forward deploy/gardenio-vmalert 8880:8880 &
curl -s localhost:8880/api/v1/rules | jq '.data.groups[].rules[].name'
#   "gardenio:agent_cost_usd:rate1h"
#   "gardenio:beads_events:rate5m"

# Recorded series materialised in vmsingle
kubectl -n gardenio port-forward svc/gardenio-vmsingle 8428:8428 &
curl -s 'localhost:8428/api/v1/query?query=gardenio:agent_cost_usd:rate1h' | jq '.data.result | length'
```

### Adding a rule

Only promote a query to a recording rule once it is shown slow on real data and
sits on a hot dashboard path (R4 §5). Add the `record:` entry to
`recording.yaml`, keep the `level:metric:operation` naming, and add a presence
assertion to `tests/vmalert_test.go`.

### Land credit series (plan step 3.2)

A tenant sees credits, never dollars (`purse.CreditsOnly`). The tenant charts
(3.3, `GET /garden/charts/costs-land`) therefore read only series that are
already credits. vmalert records them per land, from the land account's USD
and the credit rate in force. The rules live in
`helm/gardenio/templates/_lands-credits.tpl` (the full maths is in its header
comment) and render as the ConfigMap key `lands-credits.yaml`. They are on
wherever vmalert runs (`vmalert.landCredits.enabled`, default `true`). They are
recording rules only, so they need no notifier.

| Series (label `land`) | What | Group |
|---|---|---|
| `gardenio:land_spend_credits:total` | Credits spent to date: the purse's `spent_credits` before in-flight | 1m |
| `gardenio:land_credits:granted` | Credits granted: the land's ledger | 1m |
| `gardenio:land_credits:remaining` | granted − spent. The purse also takes off in-flight agent cost, so this settled figure can read a little above the land header | 1m |
| `gardenio:land_spend_credits:rate1h` | Credits per hour over the trailing hour | 1m |
| `gardenio:land_spend_credits:rate1d` | Credits per hour, the trailing day's mean (the run-out projection) | 1m |
| `gardenio:land_spend_credits:by_agent` (also `agent`) | Credits per agent, from the spend-logs gauge's ~48 h lookback slice | 1m |
| `gardenio:land_spend_credits:today` | Credits since 00:00 UTC | 5m |
| `gardenio:land_spend_credits:increase7d` / `:increase30d` | Credits over the trailing 7 / 30 days (retention caps 30d) | 5m |

Inputs:

- `gardenio_account_cost_usd{account="lands-<land>"}` is the land account's
  lifetime LiteLLM bill. It is the same `total_cost_usd` the purse reads.
- `gardenio_agent_spend_logs_usd{account="lands-<land>"}` is per-agent USD.
- `gardenio_lands_credit_rate{land}` is new with 3.2. The controller exports
  it (`internal/metrics/lands_credits.go`): the `llm_multiplier` of the rate
  in force (`gb lands rates show`), for every tenant land. It is absent when
  no rate is in force.
- `gardenio_lands_credits_granted{land}` is new with 3.2: the sum of the land's
  grants (`gb lands credit add`). The controller re-reads both every 5 minutes.

Rules the series keep:

- **Credits = USD × the rate now**, as in the purse. A rate change reprices
  the whole history, so every window takes the USD increase first and
  multiplies by the rate at evaluation time.
- **No rate, no series.** A land with no rate in force, and every operator
  account, records nothing. The `* on (land)` join with the rate drops it. A
  missing series reads "No credit history yet" on the tenant card, never 0.
- **No `increase()`.** Prometheus extrapolates it and VictoriaMetrics does
  not, so promtool and production would disagree. A trailing window is the
  gauge now less its lowest value in the window. "today" is a sum of 5-minute
  increments, weighted by UTC day.
- **No record names USD.** `TestLandsCredits_RecordsOnlyCredits` fails if one
  does. The hub's tenant queries select only `gardenio:land_spend_credits:*`
  and `gardenio:land_credits:*`.

Tests (`make test-helm`, needs promtool):
`tests/lands_credits_test.go` and `tests/lands_credits_rules_test.yaml` cover
spent, remaining, today across midnight, the windows, a scrape gap, a key
reset, a rate change, per-agent credits and the empty cases.
`TestLandsCredits_PurseCrossCheck` runs the rules on the farm in
`controller/internal/lands/purse/testdata/credits_crosscheck.json`.
`TestLandPurseAPI_CreditsCrossCheck` (beads-viewer) checks that
`GET /api/lands/{land}/purse` returns the same figures for that farm, and
`TestCollectLandCredits_CrossCheck` (internal/metrics) checks the gauges.
Change one side and the fixture, and all three must still agree.

Verify after deploy (the controller must run the 3.2 build first, or there is
no rate series and nothing records):

```bash
# The inputs: one rate and one granted series per tenant land
curl -s 'localhost:8428/api/v1/query?query=gardenio_lands_credit_rate' | jq '.data.result[] | [.metric.land, .value[1]]'
curl -s 'localhost:8428/api/v1/query?query=gardenio_lands_credits_granted' | jq '.data.result[] | [.metric.land, .value[1]]'

# The records (vmalert UI on :8880 lists both groups)
curl -s localhost:8880/api/v1/rules | jq '.data.groups[] | select(.name|startswith("gardenio.lands_credits")) | .rules[].name'

# Cross-check one land against its purse. spent should match spent_credits
# within one spend sync (5 min); remaining should match remaining_credits plus
# in_flight_credits.
LAND=<land>
curl -s "localhost:8428/api/v1/query?query=gardenio:land_spend_credits:total{land=\"$LAND\"}" | jq -r '.data.result[0].value[1]'
curl -s "localhost:8428/api/v1/query?query=gardenio:land_credits:remaining{land=\"$LAND\"}" | jq -r '.data.result[0].value[1]'
gb lands purse "$LAND"     # or GET /api/lands/$LAND/purse (admin)
```

When a land has no credit series:

| Symptom | Cause | Fix |
|---|---|---|
| No `gardenio_lands_credit_rate` at all | No rate in force, or the controller predates 3.2 | `gb lands rates show`; roll the controller |
| Rate present, no `:total` for the land | No `gardenio_account_cost_usd{account="lands-<land>"}`: the land has no account bead yet, or bills to another account (the purse reads unknown too) | `gb lands purse <land>` says which |
| `:total` present, `:today` absent | The 5m group has not evaluated yet, or the gauge had no samples today | Wait one 5m evaluation |
| `:total` lower than the purse's `spent_credits` | The spend sync stamped the bead after the last scrape | Wait 5 minutes; a lasting gap is a scrape problem (`up{job=~".*controller.*"}`) |

### Alerting (future)

v1 carries recording rules **only**, so vmalert runs with no notifier flag (a
notifier is required only for *alerting* rules). When alerting lands, wire
vmalert at the existing ACME `kube-prometheus-stack` Alertmanager — **do not**
deploy a second Alertmanager (R1 §Q6).

---

## 2. Chart-pack dashboards

The dashboards live in `helm/gardenio/dashboards/` and are rendered into a
`grafana_dashboard: "1"`-labelled ConfigMap by
`templates/controller/grafana-dashboard-configmap.yaml`, which the ACME
observability Grafana sidecar auto-discovers and loads against the
VictoriaMetrics datasource.

| Dashboard | File |
|---|---|
| Cost overview | `dashboards/cost-overview.json` |
| kbeads embedding | `dashboards/kbeads-embedding.json` |
| Beads operated-by | `dashboards/operated-by.json` |

Enabled in prod via `agents.metrics.grafanaDashboard.enabled: true` in
`deploy-values/gardenio.yaml`. Verify:

```bash
kubectl -n gardenio get configmap -l grafana_dashboard=1
# then confirm the dashboards appear in the ACME Grafana UI.
```

---

## 3. 90-day beads-events backfill (D8)

Operator-approved via decision ka-0000001695. The replay reconstructs
`gardenio_beads_events_total` history from the durable kbeads `events` table so
the counter is queryable for the last 90 days, including data from before the
live counter was instrumented.

Implemented as the `kd events backfill` command
(`kbeads/cmd/kd/events_backfill.go`). It pages `GET /v1/events` chronologically,
projects each row onto the same bounded `{event,project,bead_type}` schema the
live counter uses (so backfilled and live samples share one series identity),
and POSTs Prometheus-exposition samples — each timestamped at
`events.created_at` — to the VictoriaMetrics import endpoint.

### Run it (one-shot, operational)

The import endpoint is `<vm-url>/api/v1/import/prometheus`. Point `--vm-url` at
**vmagent** (`:8429`) so the write buffers through the same path the live push
uses, or directly at vmsingle (`:8428`).

```bash
# From a pod/jumphost with HTTP access to the kbeads daemon + VM stack:

# 1. Preview — project + count, print a sample, POST nothing.
kd events backfill --dry-run

# 2. Real run — default window is the last 90 days (D8).
kd events backfill --vm-url http://gardenio-vmagent:8429

# Narrower window / tuning:
kd events backfill \
  --since 2026-03-01T00:00:00Z --until 2026-06-01T00:00:00Z \
  --vm-url http://gardenio-vmagent:8429
```

Key flags (`kd events backfill --help`):

| Flag | Default | Purpose |
|---|---|---|
| `--since` | 90d before `--until` | replay window start (RFC3339) |
| `--until` | now | replay window end (RFC3339) |
| `--vm-url` | `$VM_IMPORT_URL` | VM base URL; required unless `--dry-run` |
| `--page-size` | 1000 | events per `/v1/events` page (server cap 1000) |
| `--flush-lines` | 5000 | exposition lines per import POST |
| `--max-pages` | 0 | cap pages scanned (0 = no cap) |
| `--dry-run` | false | count + sample only, no POST |

Backfill is **idempotent on series identity** — re-running over the same window
overwrites the same timestamped samples rather than double-counting, because
each sample carries its own `events.created_at` timestamp.

### Verify

```bash
kubectl -n gardenio port-forward svc/gardenio-vmsingle 8428:8428 &
# Counter present across the backfilled window:
curl -s 'localhost:8428/api/v1/query?query=gardenio_beads_events_total' | jq '.data.result | length'
# Spot-check a historical point (>1d old):
curl -s 'localhost:8428/api/v1/query_range?query=sum(gardenio_beads_events_total)&start=<90d-ago-unix>&end=<now-unix>&step=3600' | jq '.data.result[0].values | length'
```
