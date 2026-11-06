# Non-AVA Webhook Carve-Out: Inbound Jira/GitLab Bridges

Source: operator decision (Matt Baker, 2026-06-03),
[ka-0000005632](https://beads.gardenio.e2e.example.com/i/ka-0000005632).
Parent epic: [ka-0000000914](https://beads.gardenio.e2e.example.com/i/ka-0000000914)
— *AVA-only external access: adopt AVA everywhere, retire all non-AVA external doors.*

## Summary

AWS Verified Access (AVA) is the target front door for **all** external access
to the gardenio AI factory. AVA is browser/cookie-only (auth-code OIDC), so it
**cannot** authenticate a headless third-party POSTer. Inbound Jira and GitLab
webhooks are exactly that: Atlassian Cloud and GitLab SaaS POST to a URL with no
browser, no interactive login, and no cookie jar.

The operator decision (2026-06-03) is therefore to keep the two inbound webhook
endpoints as a **permanent, tightly-scoped NON-AVA exception** — *not* to
re-architect them into a pull model. The exception is accepted **only** because
it is compensated by two controls:

1. **Logs** — structured records of all non-AVA bridge traffic.
2. **Metrics** — receive-path liveness metrics, fronted on Grafana and the
   beads-viewer Health page.

This doc is the carve-out's done-definition: it enumerates the perimeter, the
current state of each compensating control (with file references), the
acceptance criteria that make the carve-out safe, and the residual gaps still
tracked as follow-ups.

## Scope

The two endpoints in the carve-out:

| Door | Endpoint | Component | Handler source |
|---|---|---|---|
| Jira | `POST /webhook` | `jira-bridge` | `gardenio/controller/internal/bridge/jira_webhook.go` |
| GitLab | `POST /webhook` | `gitlab-bridge` | `gardenio/controller/internal/bridge/gitlab_sync.go` (`GitLabWebhookHandlerWithConfig`) |

**Not in scope:** every *other* external door in the parent epic (the GitLab-PAT
edge-auth shim `cli.gardenio`, the legacy VPN-IP `beads.gardenio` daemon, the
per-app legacy hosts). Those are being *removed* and re-homed behind AVA; they
are not carve-outs. This doc is exclusively about the two inbound webhooks that
**stay** non-AVA by design.

## The perimeter (primary auth)

Both endpoints skip AVA/ForwardAuth (step 6 of the edge middleware chain) and
authenticate at the application layer instead. See
`gardenio/docs/research/gardenio-in-a-box-ingress-edge-access-control.md` §2.2/§4.

- **Jira — HMAC-SHA256.** Header `X-Hub-Signature` (`sha256=<hex>`), verified in
  `jira_webhook.go` (`verifyJiraHMAC`, constant-time `hmac.Equal`). **Fail-closed:**
  a missing secret or a bad signature is a 403 and is recorded as `hmac_failed`.
  `JIRA_WEBHOOK_DEV_MODE=true` skips verification for local dev only.
- **GitLab — shared-secret token.** Header `X-Gitlab-Token`, constant-time
  `crypto/subtle.ConstantTimeCompare` against `GITLAB_WEBHOOK_SECRET` (GitLab's
  native webhook-token model, not a body signature). **Fail-closed:** empty
  secret or token mismatch is a 403, recorded as `token_failed`.
- **Body bound before auth work.** Both handlers wrap the request body in
  `http.MaxBytesReader` (2 MiB) so an attacker-controlled body cannot exhaust
  memory before verification. See AUDIT-2026-04-18 §H2 / AUDIT-2026-04-19 §J2.
- **`/metrics` is never on the public listener.** Each bridge serves Prometheus
  on an internal-only port (jira `:9093`, gitlab `:9092`) separate from the
  public `/webhook` listener; NetworkPolicy admits scrape egress only to the
  metrics ports.
- **Optional IP allowlist (defense-in-depth, opt-in).** An ingress-layer CIDR
  allowlist can restrict source IPs to the published Atlassian / GitLab SaaS
  egress ranges — Traefik `ipWhiteList` middleware
  (`templates/{jira,gitlab}-bridge/ingressroute.yaml`) or the ALB
  `inbound-cidrs` annotation (`templates/*/webhook-ingress.yaml`). **This is OFF
  by default** (`sourceRange: []`); see GAP-A below.

## Compensating control 1 — Logs

**State: Jira + GitLab receive-path logs; durable in-chart audit pipeline now
wired (GAP-B, ka-0000000320).** Both bridges emit structured `slog` JSON to
stdout (`slog.NewJSONHandler(os.Stdout, …)` in each `cmd/*/main.go`). Every
receive path logs an outcome with fields — Jira logs `event`, `jira_key`,
`author`, `comment_id`; GitLab logs the token/body failures and the per-event
downstream work. A rejected delivery is logged at `Warn`/`Error`, an accepted
one at `Info`.

**GAP-B closed — the `bridge-log-shipper` transport.** The in-chart
`bridge-log-shipper` component (off by default;
`gardenio/controller/cmd/bridge-log-shipper`, all logic in
`internal/bridgeshipper`) now routes that stdout into the VictoriaLogs audit
sink. It opens FOLLOW streams of the jira-bridge and gitlab-bridge pods' stdout
via the Kubernetes pod-log API, keeps only the webhook receive-path records
(filtering on the slog `msg` prefix — `jira-webhook:` / `webhook:` — so the
bridges' MR-poll/nudge/catch-up chatter does not dilute the audit stream), stamps
each record with its logical door name (`source=jira-bridge|gitlab-bridge`), and
POSTs them to the in-chart VictoriaLogs `/insert/jsonline` — the same durable
store `idp-log-shipper` (EKS audit) and `traefik-log-shipper` (edge access log)
feed. It reuses the sanctioned stream→filter→sink shape of `traefik-log-shipper`
(the same pod-log follow streamer and VictoriaLogs `HTTPSink`); the only
structural difference is it follows TWO in-namespace bridge pods rather than one
cross-namespace Traefik pod, so its RBAC is same-namespace. At-least-once
delivery: `streamSinceSeconds` bounds the re-read window on each reconnect so a
bridge pod rollout does not silently drop the disconnect gap. Enable alongside
`vlsingle` (the sink) and at least one of `jiraBridge`/`gitlabBridge` (the
sources) — the render fails fast if neither bridge is enabled.

The Prometheus deliveries counter (control 2) remains the always-on receive-path
liveness signal; the shipper adds the durable, field-queryable **audit record**
of each individual delivery on top of it.

## Compensating control 2 — Metrics (Grafana / Health page)

**State: Jira full; GitLab now at parity — deliveries counter + dark-hook
deadman (GAP-C, ka-0000002218).**

### Jira (pre-existing)

`gardenio/controller/internal/bridge/jira_webhook_deadman.go`, served on `:9093`:

- `gardenio_jira_webhook_deliveries_total{outcome}` — inbound deliveries by
  terminal outcome (`processed|ignored|hmac_failed|bad_request`); the
  receive-path liveness signal.
- `gardenio_jira_webhook_dark` — Go-computed 0/1 verdict: 1 while no
  authenticated delivery arrived within the deadman window **and** an
  independent count still shows upstream activity (the "dark hook" detector; the
  Jira hook was silently dark in prod for 8 days — bug ka-0000000426).
- `gardenio_jira_webhook_evaluations_total{result}` — monitor heartbeat.
- `gardenio_jira_webhook_upstream_matches` — context gauge.
- PrometheusRule `templates/jira-bridge/webhook-deadman-alert.yaml` (two alerts;
  default-off, per-env opt-in).

### GitLab (added by ka-0000005632)

`gardenio/controller/internal/bridge/gitlab_webhook_metrics.go`, served on `:9092`:

- `gardenio_gitlab_webhook_deliveries_total{outcome}` — inbound deliveries by
  terminal outcome (`processed|ignored|token_failed|bad_request|error`).
  Recorded on **every** terminal path of `GitLabWebhookHandlerWithConfig`, seeded
  to a zero baseline at startup (`InitGitLabWebhookMetrics`, wired in
  `cmd/gitlab-bridge/main.go`). The outcome vocabulary mirrors Jira's so one
  Grafana panel can chart both bridges with a single label set.

Before this change the GitLab door emitted **no** receive-path metric at all, so
"the GitLab webhook is receiving nothing" was indistinguishable from "healthy and
quiet" — the exact blind spot that let the Jira hook stay dark. The counter
closes the deliveries-parity gap.

The **dark-hook deadman** (GAP-C, ka-0000002218) is now added on top of the
counter, in `gardenio/controller/internal/bridge/gitlab_webhook_deadman.go`
(served on `:9092`):

- `gardenio_gitlab_webhook_dark` — Go-computed 0/1 verdict: 1 while no
  authenticated delivery arrived within the deadman window (default 2h,
  `GITLAB_WEBHOOK_DEADMAN_WINDOW`) **and** an independent scan of the group's
  recently-updated merge requests (`ListGroupMergeRequests`, the GitLab parallel
  to Jira's JQL `ApproximateCount`) still shows activity — the dark-hook
  detector. Keying "alive" on **authenticated** deliveries
  (`processed|ignored|error`) makes a wrong-secret hook (`token_failed` only)
  page rather than read as healthy; `token_failed`/`bad_request` never refresh
  the liveness clock, while `error` (a post-decode 500) does, so a daemon outage
  does not false-page as a dark hook.
- `gardenio_gitlab_webhook_evaluations_total{result}` — monitor heartbeat
  (`ok|scan_error`), backing the deadman leg that catches a dead/hung monitor.
- `gardenio_gitlab_webhook_upstream_matches` — context gauge (recent group-MR
  count on the last dark evaluation).
- PrometheusRule `templates/gitlab-bridge/webhook-deadman-alert.yaml` (two
  alerts — `GitLabWebhookDark` + `GitLabWebhookMonitorDeadman`; default-off,
  per-env opt-in via `gitlabBridge.metrics.prometheusRule.webhookDeadman`).

The monitor runs only when `GITLAB_GROUP_ID` is set — the upstream-activity
confirmation arm needs a group scope; without one it would always read empty and
never page, so it is disabled rather than left to fire on a false-quiet upstream.

### Surfacing

Both counters are scraped via the pod `prometheus.io/scrape` annotations already
on each bridge deployment. The beads-viewer Health page "Bridges & controller"
tile (`cmd/beads-viewer/health.go`) shows per-bridge `last_event_at` freshness,
and — **GAP-D closed (ka-0000006361)** — now also reads the dark-hook deadman
verdict for the two webhook doors. `cmd/beads-viewer/webhook_dark.go` runs one
vmsingle instant query per door for `gardenio_jira_webhook_dark` /
`gardenio_gitlab_webhook_dark` (the same read path the `/costs` headline uses);
a tripped gauge escalates that row to a loud **`dark`** pill regardless of its
`last_event_at` age — so a silently-dark hook can no longer read as merely
"slow"/"stale". A confirmed-live verdict renders a `monitored` affordance; when
no verdict is available (no vmsingle wired, upstream error, or the deadman
monitor is not running in this env) the row degrades to the prior age-only
classification. The authoritative alerting still lives in Prometheus + the
(default-off) PrometheusRule; the tile is the at-a-glance surface.

## Acceptance criteria (what makes the carve-out safe / closeable)

The carve-out is **safe to keep** when all of the following hold. Items marked
✅ are satisfied by the current code; ⏳ are tracked follow-ups (the carve-out
degrades gracefully without them — it is auditable via metrics — but they raise
the bar to the full posture).

1. ✅ **Fail-closed auth on both doors.** Missing/bad Jira HMAC and missing/wrong
   GitLab token both 403 and are counted (`hmac_failed` / `token_failed`).
2. ✅ **Body is size-bounded before auth work** on both doors.
3. ✅ **Receive-path liveness metric on both doors.** Jira and GitLab each emit
   `*_webhook_deliveries_total{outcome}`, seeded to a zero baseline, so a
   rejected-but-arriving stream is visible and a scrape has a sample immediately.
4. ✅ **Metrics served off the public listener** (internal-only ports).
5. ✅ **GitLab dark-hook deadman** (`gardenio_gitlab_webhook_dark` + monitor +
   PrometheusRule), matching Jira's (GAP-C, ka-0000002218). Runs where
   `GITLAB_GROUP_ID` is configured.
6. ✅ **Grafana dashboard** charting both bridges' deliveries/dark metrics
   (GAP-E, ka-0000000590) — `gardenio-webhook-liveness`
   (`dashboards/webhook-liveness.json`) charts both doors' deliveries-by-outcome,
   the dark verdict, and the deadman heartbeat.
7. ✅ **Durable audit-log pipeline** routing bridge webhook stdout into the
   in-chart VictoriaLogs audit sink (GAP-B, ka-0000000320) — the
   `bridge-log-shipper` component. Default-off; enable alongside `vlsingle` + the
   bridge(s).
8. ⏳ **IP allowlist enabled in production overlays** to the published SaaS
   egress CIDRs (GAP-A) — off by default today.
9. ✅ **Health tile surfaces the dark verdict** (GAP-D, ka-0000006361) — the
   beads-viewer "Bridges & controller" tile reads the deadman gauge per door and
   escalates a tripped hook to a `dark` pill, so the at-a-glance surface no
   longer conflates "silently dark" with "healthy and quiet".

The **minimum bar** for accepting the carve-out — fail-closed auth + a
receive-path metric on **both** doors so neither can go silently dark
undetected — is items 1–4, and it is now met. Items 5–8 harden it further and
are tracked as child follow-ups of ka-0000005632.

## Gap ledger

| ID | Control | State | Follow-up |
|---|---|---|---|
| GAP-A | IP allowlist scopes webhook sources | Off by default (`sourceRange: []`) in shipped overlays; example CIDRs only | Enable + set SaaS CIDRs in prod overlay |
| GAP-B | Logs reach a durable audit sink | ✅ Closed (ka-0000000320) — the in-chart `bridge-log-shipper` follows both bridge pods' webhook stdout and POSTs it to the VictoriaLogs `/insert/jsonline` audit store; default-off, enable with `vlsingle` + the bridge(s) | — |
| GAP-C | GitLab dark-hook deadman | ✅ Closed (ka-0000002218) — `_dark` gauge + monitor + evaluations heartbeat + PrometheusRule ported from Jira; default-off, runs where `GITLAB_GROUP_ID` is set | — |
| GAP-D | Health page surfaces the deadman | ✅ Closed (ka-0000006361) — the beads-viewer bridges tile reads `gardenio_{jira,gitlab}_webhook_dark` via a vmsingle instant query (`cmd/beads-viewer/webhook_dark.go`) and escalates a tripped door to a `dark` pill over its age-based state; degrades to age-only when no verdict is available | — |
| GAP-E | Metrics charted in Grafana | ✅ Closed (ka-0000000590) — `gardenio-webhook-liveness` dashboard (`dashboards/webhook-liveness.json`) charts both doors' deliveries-by-outcome + dark verdict + deadman heartbeat | — |

## Related

- `gardenio/docs/compliance/webhook-non-ava-carveout.md` — the auditor-facing
  compliance companion: control statement, mechanism, and the live validation
  record proving the compensating controls are running (ka-0000005632).
- `gardenio/helm/gardenio/dashboards/webhook-liveness.json` — the Grafana dashboard
  charting both doors' deliveries/dark metrics (closes GAP-E, ka-0000000590).
- `gardenio/controller/cmd/bridge-log-shipper/` +
  `gardenio/controller/internal/bridgeshipper/` — the non-AVA webhook-audit
  transport that ships both bridge doors' webhook stdout into VictoriaLogs
  (closes GAP-B, ka-0000000320). Chart component:
  `gardenio/helm/gardenio/templates/bridge-log-shipper/`.
- `gardenio/controller/internal/bridge/gitlab_webhook_metrics.go` — GitLab
  deliveries counter + the receive-liveness clock the deadman reads.
- `gardenio/controller/internal/bridge/gitlab_webhook_deadman.go` — the GitLab
  dark-hook deadman monitor + gauges (GAP-C, ka-0000002218).
- `gardenio/controller/internal/bridge/jira_webhook_deadman.go` — the Jira
  metrics + deadman suite this GitLab work mirrors.
- `gardenio/controller/internal/bridge/jira_webhook.go`,
  `gardenio/controller/internal/bridge/gitlab_sync.go` — the two webhook handlers.
- `gardenio/helm/gardenio/templates/jira-bridge/webhook-deadman-alert.yaml` — the
  Jira PrometheusRule (default-off).
- `gardenio/helm/gardenio/templates/gitlab-bridge/webhook-deadman-alert.yaml` —
  the GitLab PrometheusRule (default-off, GAP-C).
- `gardenio/docs/research/gardenio-in-a-box-ingress-edge-access-control.md` §2.2/§4
  — the edge middleware chain and why webhook routes skip ForwardAuth.
- `gardenio/docs/health-page.md` — the beads-viewer Health page tiles.
