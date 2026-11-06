# Compliance: Inbound Webhooks Are a Named NON-AVA Carve-Out

> **Control statement.** The two inbound webhook endpoints (Jira bridge, GitLab
> bridge) are a **permanent, tightly-scoped exception** to the "all external
> access goes through AWS Verified Access (AVA)" rule. The exception is
> **accepted** because it is compensated by two always-on controls — structured
> **logs** and receive-path **metrics fronted on Grafana**.

- **Decision of record:** operator decision (Matt Baker, 2026-06-03),
  [ka-0000005632](https://beads.gardenio.e2e.example.com/i/ka-0000005632).
- **Parent epic:** [ka-0000000914](https://beads.gardenio.e2e.example.com/i/ka-0000000914)
  — *AVA-only external access; retire all other non-AVA external doors.*
- **Security done-definition + gap ledger:**
  [`docs/security/non-ava-webhook-carveout.md`](../security/non-ava-webhook-carveout.md).
  This compliance doc is the *auditor-facing* companion: it states the control,
  the mechanism, and the **live validation evidence** that the compensating
  controls are actually running. It does not repeat the security doc's file-level
  perimeter analysis — read that for the auth internals.

This document exists so an auditor asking *"why is there an unauthenticated-by-AVA
door, and how do you know the compensating controls are live?"* has one page with
the answer and the evidence.

## 1. Why the exception exists (architecture)

AVA is browser/cookie-only: it completes an OIDC **authorization-code** flow and
issues a session cookie. That model requires an interactive browser. Inbound
webhooks are the opposite shape:

- **Atlassian Cloud** (Jira) and **GitLab SaaS** POST to a URL from their own
  servers — no browser, no interactive login, no cookie jar.
- They cannot be made to complete an AVA auth-code exchange, and re-architecting
  to a *pull* model (poll Jira/GitLab on a timer instead of receiving webhooks)
  was explicitly **rejected** in the 2026-06-03 decision: it trades a bounded,
  auditable push door for constant polling latency and API-quota cost.

So the two `/webhook` endpoints **skip AVA/ForwardAuth at the edge** (step 6 of
the edge middleware chain) and authenticate at the **application layer**:

| Door | Endpoint | Primary auth | Fail-closed behavior |
|---|---|---|---|
| Jira | `POST /webhook` (`jira-bridge`) | HMAC-SHA256 over the body, header `X-Hub-Signature` (`sha256=…`), constant-time `hmac.Equal` | missing secret or bad signature → **403**, counted `hmac_failed` |
| GitLab | `POST /webhook` (`gitlab-bridge`) | shared-secret token, header `X-Gitlab-Token`, constant-time `subtle.ConstantTimeCompare` against `GITLAB_WEBHOOK_SECRET` | empty secret or mismatch → **403**, counted `token_failed` |

Both handlers bound the request body with `http.MaxBytesReader` (2 MiB) *before*
auth work, and serve `/metrics` on an internal-only port (jira `:9093`, gitlab
`:9092`) that is never on the public listener. An optional ingress-layer IP
allowlist (Atlassian / GitLab SaaS egress CIDRs) is available as defense-in-depth
but ships **off by default** (see the security doc, GAP-A). Handler sources:
`gardenio/controller/internal/bridge/jira_webhook.go` and
`gardenio/controller/internal/bridge/gitlab_sync.go`
(`GitLabWebhookHandlerWithConfig`).

## 2. Compensating control 1 — Logs

Both bridges emit structured `slog` JSON to stdout
(`slog.NewJSONHandler(os.Stdout, …)` in each `cmd/*/main.go`). Every receive path
logs an outcome with fields:

- **Jira** logs `event`, `jira_key`, `author`, `comment_id`, and the downstream
  bead write; a rejected delivery logs at `Warn`/`Error`, an accepted one at
  `Info`.
- **GitLab** logs the token/body outcome and the per-event downstream work.

These lines are captured by the cluster's stdout collection and are queryable in
the log plane. **GAP-B (durable in-chart audit pipeline) is now closed
([ka-0000000320](https://beads.gardenio.e2e.example.com/i/ka-0000000320)):**
the in-chart `bridge-log-shipper` component (off by default) follows both bridge
pods' webhook stdout via the Kubernetes pod-log API, keeps only the receive-path
records (slog `msg` prefix `jira-webhook:` / `webhook:`), stamps each with its
door name (`source`), and POSTs them to the in-chart VictoriaLogs
`/insert/jsonline` audit store — the same store `idp-log-shipper` and
`traefik-log-shipper` feed. So each individual delivery is now a durable,
field-queryable audit record, not just transient stdout. The **Prometheus
deliveries counter** (control 2) remains the always-on receive-path liveness
signal, durably retained by VictoriaMetrics and alertable.

## 3. Compensating control 2 — Metrics fronted on Grafana

Each bridge exports a receive-path metric suite, scraped into VictoriaMetrics
(the `victoriametrics` Grafana datasource) via the pod `prometheus.io/scrape`
annotations already on each deployment.

| Metric | Bridge | Meaning |
|---|---|---|
| `gardenio_jira_webhook_deliveries_total{outcome}` | jira | deliveries by terminal outcome (`processed\|ignored\|hmac_failed\|bad_request`) |
| `gardenio_gitlab_webhook_deliveries_total{outcome}` | gitlab | deliveries by terminal outcome (`processed\|ignored\|token_failed\|bad_request\|error`) |
| `gardenio_jira_webhook_dark` / `gardenio_gitlab_webhook_dark` | both | Go-computed 0/1 dark-hook verdict (1 = no authenticated delivery in the deadman window while upstream still shows activity) |
| `gardenio_{jira,gitlab}_webhook_evaluations_total{result}` | both | deadman monitor heartbeat (`ok\|scan_error`/`count_error`) |
| `gardenio_{jira,gitlab}_webhook_upstream_matches` | both | context gauge (recent upstream activity on the last dark evaluation) |

The deliveries counter matters most: it is seeded to a **zero baseline** at
startup, so a *rejected-but-arriving* stream shows as `hmac_failed`/`token_failed`
rather than as silence, and a fully-dark hook (deregistered / wrong secret at the
SaaS console, producing no delivery at all) shows as a flat zero over the
authenticated outcomes — which is exactly what the `*_webhook_dark` deadman keys
on. This is the blind spot that once left the Jira hook silently dark in prod for
8 days (bug ka-0000000426); the GitLab door had **no** receive-path metric at all
until [ka-0000005632](https://beads.gardenio.e2e.example.com/i/ka-0000005632).

### Grafana surfacing

- **Dashboard:** `Gardenio — Inbound Webhook Liveness (non-AVA carve-out)`, uid
  `gardenio-webhook-liveness`, source
  [`gardenio/helm/gardenio/dashboards/webhook-liveness.json`](../../helm/gardenio/dashboards/webhook-liveness.json).
  It charts, for **both** doors: the dark verdict (stat + timeline),
  authenticated-deliveries-in-1h, deliveries-by-outcome (rate/5m), and the
  deadman heartbeat. Dashboards under `dashboards/` are baked into the
  grafana-bridge image and backfilled into `type=dashboard` beads at runtime,
  which provisions them into live Grafana via the `grafana_dashboard="1"`
  ConfigMap + sidecar path (mechanism: `docs/beads-viewer-dev.md`,
  `cmd/grafana-bridge/main.go`).
- **Alerting:** default-off, per-env opt-in PrometheusRules
  `templates/jira-bridge/webhook-deadman-alert.yaml` and
  `templates/gitlab-bridge/webhook-deadman-alert.yaml`.
- **Health page:** the beads-viewer Health page "Bridges & controller" tile
  shows per-bridge `last_event_at` freshness (a coarse heartbeat, not the
  deadman — GAP-D).

## 4. Validation record (2026-08-08, prod `prod-gardenio-eks`)

Evidence that the compensating controls are **live**, gathered by querying the
running cluster (namespace `gardenio-system`) and the `victoriametrics`
datasource Grafana reads. All values are point-in-time; the point is that the
series exist, are populated, and the auth/liveness logic is behaving.

**Bridges are running:** `gardenio-jira-bridge`, `gardenio-gitlab-bridge`,
`gardenio-grafana`, and `gardenio-vmsingle-0` all `Running`.

**Metrics present in VictoriaMetrics** (the exact PromQL each dashboard panel
runs, executed against the live datasource):

| Panel expression | Live result |
|---|---|
| `max(gardenio_jira_webhook_dark{component="jira-bridge"})` | `0` (ALIVE) |
| `max(gardenio_gitlab_webhook_dark{component="gitlab-bridge"})` | `0` (ALIVE) |
| `sum(increase(gardenio_jira_webhook_deliveries_total{outcome=~"processed\|ignored"}[1h]))` | `10` |
| `sum(increase(gardenio_gitlab_webhook_deliveries_total{outcome=~"processed\|ignored\|error"}[1h]))` | `71` |
| `sum by (outcome) (rate(gardenio_gitlab_webhook_deliveries_total[5m]))` | `processed` ≈ `0.011/s`, others `0` |
| `sum by (result) (rate(gardenio_jira_webhook_evaluations_total[5m]))` | `ok` ≈ `0.0067/s` (heartbeat live) |
| `sum by (result) (rate(gardenio_gitlab_webhook_evaluations_total[5m]))` | `ok` ≈ `0.0067/s` (heartbeat live) |

All outcome series (`hmac_failed`, `token_failed`, `bad_request`, `error`, …) are
present at their zero baseline, so an alert on `increase()==0` / `absent()` has a
sample to evaluate immediately rather than reading absent-because-never-scraped.

**Logs present** (representative live lines, redacted):

```
jira-bridge  INFO  jira-webhook: comment event   event=comment_created jira_key=PE-14175 author="…" comment_id=113521
jira-bridge  INFO  jira-webhook: added comment to bead  bead=ka-0000005873 jira_key=DEVOPS-958
gitlab-bridge INFO gitlab-webhook deadman monitor enabled  group_id=14176856
```

**Reproduce** (cluster access required):

```sh
# deliveries + dark + heartbeat, straight from the datasource Grafana proxies
kubectl exec -n gardenio-system gardenio-vmsingle-0 -- \
  wget -O- 'http://127.0.0.1:8428/api/v1/query?query=gardenio_gitlab_webhook_deliveries_total'
kubectl exec -n gardenio-system gardenio-vmsingle-0 -- \
  wget -O- 'http://127.0.0.1:8428/api/v1/query?query=gardenio_jira_webhook_dark'

# structured logs on each door
kubectl logs -n gardenio-system deploy/gardenio-jira-bridge   | grep jira-webhook
kubectl logs -n gardenio-system deploy/gardenio-gitlab-bridge | grep -i webhook
```

## 5. Compliance posture summary

| Requirement | Status | Evidence |
|---|---|---|
| Exception is **named and bounded** to exactly two endpoints | ✅ | §1; only `jira-bridge` + `gitlab-bridge` `/webhook` |
| **Fail-closed** auth on both doors | ✅ | §1 table; 403 + `hmac_failed`/`token_failed` counters |
| **Logs** of all non-AVA bridge traffic | ✅ | §2; stdout + durable VictoriaLogs audit pipeline (GAP-B closed → ka-0000000320) |
| **Metrics** on both doors, seeded to zero baseline | ✅ | §3, §4 |
| Metrics **fronted on Grafana** | ✅ | §3; dashboard `gardenio-webhook-liveness` (GAP-E → ka-0000000590) |
| Dark-hook **deadman** on both doors | ✅ | §3; GAP-C → ka-0000002218 |
| IP allowlist enabled in prod overlays | ⏳ tracked | GAP-A (security doc) |
| Durable in-chart audit pipeline (bridge stdout → VictoriaLogs) | ✅ | GAP-B closed → ka-0000000320 (`bridge-log-shipper`) |

The **minimum bar** for accepting the carve-out — fail-closed auth plus a
receive-path metric on **both** doors so neither can go silently dark undetected,
now charted on Grafana — is met. The ⏳ items harden it further and are tracked as
child follow-ups of ka-0000005632.

## Related

- [`docs/security/non-ava-webhook-carveout.md`](../security/non-ava-webhook-carveout.md)
  — security done-definition, perimeter analysis, full gap ledger.
- [`docs/research/gardenio-in-a-box-ingress-edge-access-control.md`](../research/gardenio-in-a-box-ingress-edge-access-control.md)
  §2.2/§4 — the edge middleware chain and why webhook routes skip ForwardAuth.
- [`docs/health-page.md`](../health-page.md) — the beads-viewer Health page tiles.
- `gardenio/helm/gardenio/dashboards/webhook-liveness.json` — the Grafana dashboard
  this doc's control §3 refers to.
