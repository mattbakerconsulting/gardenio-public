# edge-probes

The standing edge vantage (gd-xuxyIMbfCU, plan gd-P78ygCUgA9). A Lambda **outside any VPC**
runs `scripts/edge-probe/edge_probe.py` (`edge_probe.handler`, the same code the GitHub
workflow runs) every 15 minutes over the public internet. It reads AWS APIs, so it does not
depend on the edge it checks. Alerts go to SNS email, never through Slack (which runs in the
cluster).

## What it creates

| Resource | Notes |
|---|---|
| `aws_lambda_function.edge_probe` | `gardenio-edge-probe`, python3.13, 120 s, 256 MB, **no `vpc_config`** |
| `aws_iam_role.edge_probe` + policy | own log group; `cloudwatch:PutMetricData` (namespace `GardenioEdgeProbe`); `ec2:Describe{SecurityGroups,Instances,Addresses}`; `route53:ListResourceRecordSets` on the one zone; `sns:Publish` on the one topic |
| `aws_cloudwatch_event_rule.edge_probe` | `rate(15 minutes)` + target + `aws_lambda_permission` |
| `aws_sns_topic.edge_probe` + email subscription | the owner must confirm the subscription from the email AWS sends |
| 4 alarms, all notify the topic | `failures` and `broken` (>= 1 in 15 min), `heartbeat-missing` (Sum < 1 over 2 x 30 min, **missing data = breaching**: the dead-man), `lambda-errors` (>= 1) |

Every run writes `GardenioEdgeProbe/Heartbeat` = 1, `Failures` = count of FAIL, `Broken` =
count of BROKEN. On any FAIL or BROKEN it also publishes one SNS message: check ids, hosts,
results. No secrets.

## Inputs

`alert_email` (no default; the composition takes it from `var.edge_probe_alert_email`, set in
the operator's untracked `terraform.tfvars`), `zone_id`, `vpc_id`, and optional `tags`,
`schedule_expression`, `log_retention_days`.

## Operate

```
./scripts/tf-gardenio.sh plan  -target=module.edge_probes    # expect creates only
./scripts/tf-gardenio.sh apply -target=module.edge_probes
aws lambda invoke --function-name gardenio-edge-probe --profile gardenio --region us-east-1 /tmp/edge-probe.json
```

Dead-man drill: `aws events disable-rule --name gardenio-edge-probe ...`, expect the
`heartbeat-missing` email within about an hour, then `enable-rule`.
Rollback: `-destroy -target=module.edge_probes`. Nothing else depends on it.

## Availability prober and status page (`uptime.tf`)

Lands Track B §5.8 (gd-rqtbyqCMZJ). A second Lambda, `gardenio-uptime-probe`, also outside any
VPC, runs `scripts/uptime-probe/uptime_probe.py` **every minute** against the targets in
`scripts/uptime-probe/targets.json` (`/lands/`, `/garden`, the Commons API) and rewrites the
status page in the bucket `gardenio-status-<account>`. Nothing in the cluster serves it, so it is
up when gardenio is down. See `scripts/uptime-probe/README.md` for what is probed and written.

| Resource | Notes |
|---|---|
| `aws_s3_bucket.status` | Public **by bucket policy only**, for exactly `index.html`, `status.json` and `metrics` (`s3:GetObject`); `state.json` stays private; ACLs blocked (`BucketOwnerEnforced`) |
| `aws_lambda_function.uptime_probe` | python3.13, 50 s, 128 MB, **no `vpc_config`**, no async retries and a timeout inside the minute, so one run at a time (each run reads and rewrites `state.json`) |
| `aws_iam_role.uptime_probe` + policy | own log group; `cloudwatch:PutMetricData` (namespace `GardenioUptime`); `s3:GetObject`/`PutObject` on the bucket's objects; `s3:ListBucket` on the bucket (a missing `state.json` must read as NoSuchKey, not 403) |
| `aws_cloudwatch_event_rule.uptime_probe` | `rate(1 minute)` (`uptime_schedule_expression`) |
| one `uptime_down` alarm per target | `Success` below 1 in 3 of the last 5 minutes; emails on alarm **and** on recovery |
| `uptime_heartbeat` | no `Heartbeat` for 15 minutes, **missing data = breaching**: the prober is dead and the page is stale |
| `uptime_lambda_errors` | 3 or more errored runs in 15 minutes |

All of them notify the same SNS topic as the edge prober. These alarms are the backstop for when
the cluster cannot alert at all; the SLO burn-rate rules run in-cluster from the bucket's
`metrics` file (chart `vmagent.uptimeProbeScrape` + `agents.metrics.prometheusRule.landsSLO`).

Outputs: `status_page_url`, `uptime_metrics_url` (set `vmagent.uptimeProbeScrape.url` to it),
`uptime_function_name`, `uptime_alarm_names`.

**Account-level S3 Block Public Access.** If `block_public_policy` is on for the account
(`aws s3control get-public-access-block --account-id <id>`), the bucket policy is refused at
apply. The status page needs it off at the account level; the bucket-level block stays on for ACLs.

```
./scripts/tf-gardenio.sh plan  -target=module.edge_probes    # expect creates only
./scripts/tf-gardenio.sh apply -target=module.edge_probes
aws lambda invoke --function-name gardenio-uptime-probe --profile gardenio --region us-east-1 /tmp/uptime.json
curl -s "$(terraform output -raw status_page_url)" | head    # from the composition
```

Rollback: `terraform destroy -target=module.edge_probes.aws_lambda_function.uptime_probe` stops
the runs (the page then shows its last update time; the heartbeat alarm fires). Empty the bucket
before destroying it.

## Tests

`terraform test` (mock providers, `command = plan`): no `vpc_config`, heartbeat alarm
breaches on missing data, schedule is `rate(15 minutes)`, IAM grants exactly the list above
with no wildcard action; `tests/uptime.tftest.hcl` locks the uptime part (no `vpc_config`,
every minute, one run at a time, three public keys only, one down alarm per target, the
dead-man breaching on missing data, exact IAM). CI runs it in `checks.yml` job `terraform`.
