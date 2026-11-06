# Status page runbook

Track B step 10.9 (gd-OI9oGIDoCq). How the operator posts, updates and clears a notice on the
Lands status page, and what to do when the page itself is wrong. When to post is the
[incident process](incident-process.md); how the page is built is
[slos.md](slos.md) and [`scripts/uptime-probe/README.md`](../../../scripts/uptime-probe/README.md).

## What the page is

`index.html` in the S3 bucket `gardenio-status-<account>`, rewritten every minute by the
`gardenio-uptime-probe` Lambda, which runs outside any VPC and outside the cluster. Nothing in
gardenio serves it, so it is up when gardenio is down. The URL is
`terraform output status_page_url` of `module.edge_probes`:
<https://gardenio-status-123456789012.s3.amazonaws.com/index.html>.

It shows two things:

| Part | Comes from | The operator |
|---|---|---|
| The banner ("Everything is up", "Some checks are failing", "Something is down") and the three checks with 90 daily bars | The probes, every minute | Never edits it. If it is wrong, fix the probe (below) |
| The **notice**: an incident or a maintenance, with timed updates | `notice.json`, written by the operator | Posts and updates it, as here |

The banner stays the probes' verdict whatever the notice says. That is on purpose: a notice
adds what the probes cannot see (agents stuck, a provider outage, a planned upgrade), and the
banner cannot be talked into "Everything is up" while a check fails.

## The notice

`notice.json` in the same bucket. It is **private**: the bucket policy makes only `index.html`,
`status.json` and `metrics` public, and the Lambda copies the notice into the first two. It
needs no deploy: the next run (within a minute) shows it. (The Lambda reads notices from the
first `apply -target=module.edge_probes` after step 10.9's PR merged; before that it ignores
`notice.json`.)

```json
{
  "kind": "incident",
  "state": "investigating",
  "title": "Agents are not starting in any land",
  "updates": [
    {"at": "2026-11-03T14:05Z", "text": "New agents are not starting. Agents already running keep working. We are looking into it."}
  ]
}
```

| Field | Values |
|---|---|
| `kind` | `incident` or `maintenance` |
| `state` | incident: `investigating`, `identified`, `monitoring`, `resolved`. maintenance: `scheduled`, `in_progress`, `completed` |
| `title` | What tenants see, up to 120 characters |
| `updates` | 1 to 20 `{at, text}`; `at` is UTC as `2026-11-03T14:05Z`; `text` up to 1000 characters. Any order; the page shows the newest first |
| `window` | maintenance only, required: `{"start": "…Z", "end": "…Z"}`. Book whole hours, so the month's availability figure can leave them out (`--exclude`) |

Everything is shown as plain text (HTML is escaped). A `resolved` or `completed` notice leaves
the page by itself 24 hours after its newest update. One notice at a time: for two incidents at
once, one notice with both in the title.

**Writing it.** Say what tenants see and what still works, in their words. No bead ids, no
internal names (`kd`, `coop`, pod names), no guess at the cause until it is known, and nothing
about any one tenant: the page is public.

## Post a notice

Run these from a laptop with the operator's AWS credentials (the same ones as
`tf-gardenio.sh`).

```bash
BUCKET=gardenio-status-123456789012
$EDITOR /tmp/notice.json                                              # from the template above
python3 scripts/uptime-probe/uptime_probe.py --check-notice /tmp/notice.json   # must print the notice
aws s3 cp /tmp/notice.json s3://$BUCKET/notice.json --content-type application/json
```

Then check it took, a minute later:

```bash
curl -s https://$BUCKET.s3.amazonaws.com/status.json | python3 -c 'import json,sys; s=json.load(sys.stdin); print(s.get("notice_error") or s["notice"])'
```

or open the page. If `status.json` has `notice_error`, the notice failed to validate and is not
shown; the error says why (the Lambda's log in CloudWatch `/aws/lambda/gardenio-uptime-probe`
has it too). `--check-notice` runs the same validation, so check before uploading.

## Update it

Add an update to the **top or bottom** of `updates` (order does not matter), change `state` if
it moved, check and upload again, the same three commands. Keep every earlier update: the page
is the public timeline of the incident.

| Moment | `state` | An update like |
|---|---|---|
| First post | `investigating` | "Since 14:00 UTC new agents are not starting. Running agents keep working. We are looking into it." |
| Cause known | `identified` | "A bad release stopped the controller. We are rolling it back." |
| Fix in, watching | `monitoring` | "Rolled back at 14:40. New agents are starting again; we are watching it." |
| Over | `resolved` | "Fixed. Between 14:00 and 14:38 UTC no new agents started. Running agents and data were not affected." |

## Announce maintenance

At least 48 hours before it starts, post `kind: maintenance`,
`state: scheduled`, with the `window`. Move it to `in_progress` when work starts and to
`completed` when it ends, with an update each time. Maintenance that runs past its window is an
incident from the window's end: say so in an update, and the overrun counts in the month's
availability figure. (There is no SLA for v1. If one is published, its §3.1 sets the notice and
the monthly cap on maintenance left out of the figure.)

## Clear it

A finished notice clears itself after 24 hours. To clear one at once (a notice posted by
mistake):

```bash
aws s3 rm s3://$BUCKET/notice.json
```

Do not delete a real incident's notice early: tenants who look later should see that it
happened.

## When the page is wrong

| Symptom | Likely cause | Do |
|---|---|---|
| "Updated" time is more than a few minutes old | The Lambda is not running or failing; the `-heartbeat-missing` or `-lambda-errors` email will say so | CloudWatch logs `/aws/lambda/gardenio-uptime-probe`. The page keeps its last state until a run succeeds |
| A check is red but the thing works | The probe's expected answer changed (a new redirect, a new status code) | Check by hand with `python3 scripts/uptime-probe/uptime_probe.py --out-dir /tmp/uptime`; fix `targets.json` and apply the module. The failed minutes stay in the record; take them out of the month's figures only if the Service really was up, and say so in the month's record |
| A check is green but tenants say it is down | The probe proves only what an outsider sees (see slos.md) | Post a notice; the in-cluster `*_requests` SLOs measure after sign-in |
| The notice does not show | `notice_error` in `status.json`, or the notice is `resolved` and over 24 hours old | Fix and re-upload |

The page can only be changed by the Lambda or by someone with write access to the bucket. If
the Lambda is broken and an incident needs a notice now, uploading `notice.json` still works:
the next successful run shows it. Do not hand-edit `index.html`; the next run overwrites it.
