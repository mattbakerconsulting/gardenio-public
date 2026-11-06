# uptime-probe

Availability probes for `/lands/`, `/garden` and the Commons API, and the off-cluster status
page (Lands Track B §5.8, gd-rqtbyqCMZJ). The security prober next door (`scripts/edge-probe/`)
asks "is anything open that should not be?" every 15 minutes; this one asks "is it up?" every
minute, from the same place: a Lambda outside any VPC (module
`gardenio/terraform/modules/edge-probes`), so it keeps answering while gardenio is down.

```
python3 scripts/uptime-probe/uptime_probe.py --out-dir /tmp/uptime   # one run, files in /tmp/uptime
python3 scripts/uptime-probe/uptime_probe_test.py                    # unit tests, no network
python3 scripts/uptime-probe/uptime_probe.py --check-notice notice.json          # validate a notice
python3 scripts/uptime-probe/uptime_probe.py --sla-month 2026-11 --state state.json   # a month's availability figures
```

Stdlib only; `boto3` is imported lazily, in the Lambda handler only.

## Targets

`targets.json`, one entry per availability SLO. The name is the `target` label in-cluster, the
`Target` dimension in CloudWatch, and the terraform module makes one down-alarm per entry, so
adding a target here adds its alarm on the next apply.

| Target | Request | Up means |
|---|---|---|
| `lands` | `GET https://gardenio.clonk.tv/lands/` | 200 and the page says `Gardenio Lands`: edge, viewer-authproxy and beads-viewer all answered |
| `garden` | `GET https://gardenio.clonk.tv/garden` (a navigation) | 302 to `https://gardenio.clonk.tv/oauth2/start`: edge and the sign-in check (forwardauth) answered |
| `commons_api` | `GET https://gardenio.clonk.tv/garden/c/unread` with `Sec-Fetch-Mode: cors` | 401 (the edge's answer to a scripted call with no session): edge and forwardauth answered |

The probes carry no credential, so `/garden` and the Commons API are proven only as far as an
outsider gets. What happens after sign-in is the in-cluster server-side SLIs' job (the Lands
SLO rules, `agents.metrics.prometheusRule.landsSLO` in the chart). Redirects are not followed,
and a detail is a status code, an error class or a redirect target cut to scheme+host+path.

## What each run writes

To the status bucket (in Lambda mode) or `--out-dir`:

| Key | Public | What |
|---|---|---|
| `state.json` | no | Cumulative counters, the failure streak, hourly buckets for 90 days. The only state. |
| `status.json` | yes | The status page data: overall and per-target state, 24h/7d/30d/90d availability, 90 daily bars |
| `index.html` | yes | The status page: server-rendered, inline CSS, no script, refreshes every minute |
| `metrics` | yes | Prometheus text for the in-cluster vmagent (`vmagent.uptimeProbeScrape`) |

It also reads `notice.json` (private, written only by the operator): an incident or maintenance
notice shown above the checks and copied into `status.json`. A notice that does not validate is
left off, and `status.json` carries `notice_error`; a `resolved` or `completed` one drops off 24
hours after its newest update. The format and how to post one:
[status-page-runbook.md](../../gardenio/docs/lands/status-page-runbook.md). The Lambda's
`s3:GetObject` on the bucket already covers it; nothing in terraform changes.

`--sla-month` turns `state.json`'s hourly buckets into each target's availability for one UTC
calendar month, and the lowest of them (`service_availability`), the month's figure in the
[incident process](../../gardenio/docs/lands/incident-process.md#monthly-the-availability-figure)
(and the figure a published [SLA](../../gardenio/docs/lands/sla.md) §2 would use; none is in force for v1). `--exclude START/END` (whole hours, repeatable)
leaves out announced maintenance. `state.json` keeps 90 days, so run it within that.

A target is **up** after a passing probe, **degraded** after one or two failures in a row, and
**down** after three (`DOWN_STREAK`). The page's overall state is the worst target.

The `metrics` file:

| Series | Type |
|---|---|
| `gardenio_uptime_probe_total{target}` | counter, since the first run |
| `gardenio_uptime_probe_failures_total{target}` | counter |
| `gardenio_uptime_probe_success{target}` | gauge, last probe 1/0 |
| `gardenio_uptime_probe_duration_seconds{target}` | gauge, last probe |
| `gardenio_uptime_probe_last_run_timestamp_seconds` | gauge; the in-cluster `LandsUptimeProbeStale` alert reads it |

In Lambda mode it also writes CloudWatch `GardenioUptime/Success` and `LatencyMs` per `Target`,
and `Heartbeat`, which the module's down and dead-man alarms read (SNS email, never Slack).

## Add a target

1. Add it to `targets.json` with the response an outsider should get.
2. Run `uptime_probe.py --out-dir` locally against the live edge and check it passes.
3. Add a `landsSLO` objective for it in the chart if it should have an SLO.
4. The next `terraform apply -target=module.edge_probes` adds its down-alarm.
