# Lands SLOs, burn-rate alerts and the status page

Track B §5.8, step 10.8 (gd-rqtbyqCMZJ). This page covers how the measurements and alerts
work. Step 10.9 (gd-OI9oGIDoCq) wrote the words: the [incident process](incident-process.md),
the [status page runbook](status-page-runbook.md) and a draft [SLA](sla.md). The objectives
below are internal. There is no SLA for Open day (owner decision gd-yjw85aEUao, `no-sla-v1`), so
they promise tenants nothing. They stay as they are until the payments decision (step 11.6)
revisits the SLA; any SLA it publishes should promise less than these objectives (99.0% a month
was recommended), so a burn alert fires before the SLA is missed
([incident-process.md](incident-process.md#if-an-sla-is-published-later)).

## The pieces

```text
Lambda gardenio-uptime-probe (outside any VPC, every minute)
  ├─ probes /lands/, /garden, the Commons API      scripts/uptime-probe/targets.json
  ├─ writes the status page + metrics file          S3 gardenio-status-<account> (public: 3 keys)
  └─ CloudWatch GardenioUptime/*                    down alarms per target, dead-man → SNS email
                    │
in-cluster vmagent ─┘ scrapes the metrics file      vmagent.uptimeProbeScrape
beads-viewer        → gardenio_user_request_latency_seconds, gardenio_commons_event_lag_seconds
vmalert             → SLIs + LandsSLOFastBurn / LandsSLOSlowBurn → Alertmanager → Slack
                                                    agents.metrics.prometheusRule.landsSLO
```

The off-cluster alarms are the backstop for when the cluster cannot alert at all. The
in-cluster rules are the SLOs.

## SLIs and objectives

Each objective records `gardenio:lands_slo_errors:ratio_rate<w>{slo}` (the bad share of events)
for 5m, 30m, 1h, 2h, 6h, 1d, 3d and 30d, and `gardenio:lands_slo:objective{slo}`.

| `slo` | SLI (bad / all) | Source | Target | State |
|---|---|---|---|---|
| `lands_availability` | failed / all probes of `/lands/` (200 and the page) | uptime prober | 99.5% | on |
| `garden_availability` | failed / all probes of `/garden` (302 to sign-in) | uptime prober | 99.5% | on |
| `commons_api_availability` | failed / all probes of `/garden/c/unread` (401 to a scripted call) | uptime prober | 99.5% | on |
| `garden_requests` | 5xx / all `/garden`, `/garden/{bead,farm,state,events}…` responses | beads-viewer | 99.5% | on |
| `commons_api_requests` | 5xx / all `/garden/c…` responses | beads-viewer | 99.5% | on |
| `commons_event_latency` | events slower than 5s / all events, kbeads write to hub broadcast | beads-viewer `gardenio_commons_event_lag_seconds` | 99% | on |
| `tenant_spawn_to_working` | tenant agents not working within 300s of spawn | controller `gardenio_lands_spawn_to_working_seconds` | 95% | on |
| `steward_response` | human `#general` messages not answered within 15 min | beads-viewer `gardenio_lands_steward_response_seconds` | 90% | on |

The probes carry no credential, so for `/garden` and the Commons API they prove the edge and
the sign-in check answer. What happens after sign-in is what the `*_requests` objectives measure.

## Alerts

Burn rate = error ratio / (1 − target); the SRE workbook's multi-window pairs:

| Alert | Severity | Fires when |
|---|---|---|
| `LandsSLOFastBurn` | `page` | burn > 14.4 over 1h **and** 5m, or > 6 over 6h **and** 30m, for 2m |
| `LandsSLOSlowBurn` | `ticket` | burn > 3 over 1d **and** 2h, or > 1 over 3d **and** 6h, for 15m |
| `LandsUptimeProbeStale` | `ticket` | the prober's metrics file is over 600s old or missing, for 10m |

Every alert carries `slo` (except the stale one) and `subsystem: lands-slo`, and routes like
every other alert by severity. `promtool` firing proofs: `gardenio/helm/gardenio/tests/lands_slo_rules_test.yaml`.

Off-cluster (SNS email): `gardenio-uptime-probe-<target>-down` (3 of 5 minutes failing, mails
again on recovery), `-heartbeat-missing` (15 minutes with no run) and `-lambda-errors`.

## Turning it on

1. **Terraform (operator).** `./scripts/tf-gardenio.sh apply -target=module.edge_probes` creates
   the bucket, the Lambda and the alarms (see `gardenio/terraform/modules/edge-probes/README.md`,
   including the account-level S3 Block Public Access check). Then `terraform output
   status_page_url` and `uptime_metrics_url`.
2. **Deploy values (operator).** In the k3s overlay:
   `vmagent.uptimeProbeScrape: {enabled: true, url: <uptime_metrics_url>}` and
   `agents.metrics.prometheusRule.landsSLO.enabled: true`. The alerts fire only through the
   vmalert ConfigMap path, so vmalert needs a notifier (Alertmanager), which is off on k3s today;
   until it is on, the SLIs still record if vmalert runs with `notifierBlackhole`, and nothing pages.
3. Check: `gardenio:lands_slo_errors:ratio_rate5m` has a series per enabled `slo` in Grafana, and
   the status page shows "Everything is up".

## Changing a target

`--set agents.metrics.prometheusRule.landsSLO.objectives.<slo>.target=0.999` (objectives are a
map, so an overlay changes one without restating the rest). A probe target is added in
`scripts/uptime-probe/targets.json` first; see its README.
