# Lands incident process

Track B step 10.9 (gd-OI9oGIDoCq). What the operator does when something breaks for tenant
lands, from the first alert to the review and the month's availability figure. The status page
mechanics are in [status-page-runbook.md](status-page-runbook.md); the measurements and alerts
are in [slos.md](slos.md).

**There is no SLA for Open day** (owner decision gd-yjw85aEUao, `no-sla-v1`, 2026-10-21).
[sla.md](sla.md) stays an unpublished draft and the Terms §8.1 promises nothing. The times on
this page (notice within 30 minutes, updates every 2 hours, review within 7 days) are the
operator's own aims, not promises to tenants, and no SLA credits are paid. The SLA comes back at
the payments decision (step 11.6); [below](#if-an-sla-is-published-later) is what changes then.

One person is the operator, the support desk and the incident responder (spec §9). The process
is sized for that: few steps, each one a command, and the status page first, because a visible
outage costs less trust than a silent one.

## Severity

| Sev | Means | Examples | Status page | `#support` |
|---|---|---|---|---|
| **1** | Tenant lands cannot work, or tenant data or isolation is at risk | a status-page check down; spawns refused for every land; one land seeing another's beads; spend running past the wall | notice within **30 min**, updates every **2 h** | each affected land, if the Commons is up |
| **2** | Lands work but something they rely on is broken or slow | `LandsSLOSlowBurn`; Commons events lagging; one land's agents stuck; a GitHub or model provider outage | notice if tenants can see it | the affected land |
| **3** | Minor, or seen only by the operator | a dashboard gap, a flaky alert | none | none |

When in doubt, call it the higher one.

## Where incidents come from

| Signal | Reaches the operator by | Read |
|---|---|---|
| `gardenio-uptime-probe-<target>-down`, `-heartbeat-missing`, `-lambda-errors` | SNS email, from outside the cluster | The status page, then CloudWatch `GardenioUptime/*` |
| `LandsSLOFastBurn` (`page`), `LandsSLOSlowBurn` and `LandsUptimeProbeStale` (`ticket`) | Alertmanager to Slack. **Off on k3s today**: vmalert runs with `notifierBlackhole`, so these only record; the SNS emails are the live signal until Alertmanager is on ([slos.md](slos.md#turning-it-on)) | Grafana, `gardenio:lands_slo_errors:ratio_rate5m{slo=…}` |
| A tenant in `#support`, or mail to `support@` | The support bead and the operator's Slack DM, or the forwarded mail (step 11.1, [support-runbook.md](support-runbook.md)) | The support bead |
| A security or isolation concern (one land reaching another, leaked repo content, abuse) | Any of the above, or a report under AUP §7 | Sev 1. Freeze first (step 3). If tenant data is affected, Privacy Policy §7 applies: tell the affected tenants without undue delay. The Lands threat model (step 10.7, `gardenio/docs/security/lands-threat-model.md`) names the threats |

## The steps

### 1. Open it (first 10 minutes)

```bash
kd create "Incident: <what tenants see, in their words>" --type=bug --priority 0 \
  -l project:gardenio -l incident -l sev:1 \
  -d "Started: <UTC time of the first failed check or report>. Seen by: <signal>. Lands affected: <all | names>."
```

The bead is the incident's record: the timeline goes in comments (`kd comment add <id> "..."`,
one per action or finding, with the UTC time), and every follow-up bug links back to it. The
start time is the first failed check (the status page's `status.json`, or CloudWatch), not
when the email arrived.

### 2. Say so (within 30 minutes for Sev 1)

Post the status page notice: [status-page-runbook.md](status-page-runbook.md#post-a-notice),
state `investigating`. Write what tenants see and what still works, not the cause you suspect.
If the Commons is up, post the same words in each affected land's `#support`.

### 3. Stop the harm

Pick the smallest lever that stops it getting worse. Each one is reversible:

| Lever | Use when | Undo |
|---|---|---|
| `gb lands pause --reason "incident <id>: …"` | New tenant agents would make it worse (runaway spend, a bad image, an isolation doubt). Running agents carry on | `gb lands resume` |
| `gb lands freeze <land> --reason "incident <id>: …"` | One land is the problem or is at risk. Parks its agents after the grace; members cannot post | `gb lands thaw <land>`, then resume the parked agents |
| Roll back the release | The incident started with a deploy ([release-pipeline.md](../release-pipeline.md), "Deploy / rollback / roll-forward") | Roll forward |

A freeze made for an incident is ours, not the tenant's. Say so in the freeze reason
(`incident <id>`) so the month's record can tell it apart from a freeze for the land's own
breach.

### 4. Keep people told

Update the notice at least every 2 hours for Sev 1, and whenever the state
changes: `identified` when the cause is known, `monitoring` when the fix is in and you are
watching it hold. An update that says "still working on it" is still an update.

### 5. Close it

When the checks have passed for 30 minutes and tenants can work again:

1. Set the notice to `resolved` with one line on what happened. It leaves the page by itself 24
   hours after that update.
2. Undo any lever from step 3 that is no longer needed (`gb lands resume`, `gb lands thaw`).
3. Comment the end time on the incident bead, and the minutes each status-page check was failing
   (from `--sla-month` or CloudWatch).
4. Keep the bead open until the review is done.

### 6. Review (within 7 days for Sev 1)

On the incident bead, a comment with:

- **Timeline**: start, noticed, notice posted, mitigated, resolved (UTC).
- **Impact**: which lands, and for how long.
- **Cause**: what broke, and why the alerts did or did not catch it first.
- **Follow-ups**: one bug bead per change, each naming the incident bead in its description.
  The incident closes when the review is posted, not when every follow-up lands.

Then post a short, plain account in each affected land's `#support`: what tenants
saw, the cause in a sentence, and what changed. Close the incident bead.

## Monthly: the availability figure

On the first working day of each month, for the month before:

1. **Work out the month's availability** from the prober's state, leaving out announced
   maintenance (whole hours, as posted on the status page):

   ```bash
   aws s3 cp s3://gardenio-status-<account>/state.json /tmp/state.json
   python3 scripts/uptime-probe/uptime_probe.py --sla-month 2026-11 --state /tmp/state.json \
     --exclude 2026-11-14T02:00Z/2026-11-14T04:00Z
   ```

   `service_availability` is the lowest of the three targets, the figure an SLA would use
   ([sla.md](sla.md) §2.3). If the report carries a `warning`, part of the month has aged out of
   `state.json` (90 days); use CloudWatch `GardenioUptime/Success` for that part instead.

2. **Record the month** on the Lands epic or plan bead: the figure, each target's figure, the
   maintenance left out and the incidents. These are the numbers the payments decision (step
   11.6) uses to set an SLA, if it sets one.

A tenant who asks for a month's figures gets them, from step 1, for any month in the last 90
days.

## If an SLA is published later

The owner chose no SLA for v1 (decision gd-yjw85aEUao, `no-sla-v1`) and to revisit it at the
payments decision (step 11.6, gd-cjItlf1QKP). If that decision publishes one:

1. Fill the `{{PLACEHOLDER}}`s in [sla.md](sla.md) with the values chosen, set its `status` to
   `published`, and change the Terms §8.1 to name it. That is a new version of the legal set
   ([legal/README.md](legal/README.md#versions)).
2. Put the SLA's times back on this page where they become promises: the Sev 1 notice and update
   times, the review time, and "an SLA miss is always Sev 1 once it is known". The review's
   **Impact** says whether the month's SLA is now missed, and the account in `#support` follows
   SLA §5.2. A freeze for an incident is not an exclusion under SLA §3.2, and its time counts
   against us.
3. Add the monthly credits to the routine above, within the SLA's `{{REMEDY_PAY_DAYS}}`:
   - **If the remedy is a share of spend**, record each tenant land's purse on the 1st, as the
     next month's baseline (`snap=$(gb lands purse <land> --json)`, then
     `kd comment add <land-project-bead> "SLA purse snapshot $(date -u +%F): $snap"`). The
     month's spend is this snapshot's `spent` minus last month's. A land's first month counts
     from its opening. A flat-credit remedy needs no snapshot.
   - **Take out what the prober cannot see**, land by land: hours a land was frozen for its own
     breach, at its purse wall, or paused at its request (SLA §3.2), so a land's figure can be
     higher than the service's. Outages of GitHub or a model provider (§3.3) do not show on the
     probes, so nothing is taken out for them.
   - **Pay** each land owed credits, then post the month's figure and the grant in its
     `#support`:

     ```bash
     gb lands credit add <land> <credits> \
       --reason "SLA credit 2026-11: availability 98.71% (target 99.0%); incidents gd-…, gd-…"
     ```

   - A tenant's claim (SLA §4.3) runs these steps for that month and land and is answered with
     the figures, whether or not credits are owed.

The values recommended to the owner on 2026-10-21, sized for one operator and one k3s cluster,
for the 11.6 revisit:

| Placeholder | Recommended | Why |
|---|---|---|
| `{{SLA_TARGET}}` | 99.0% per calendar month (about 7 h 18 min of downtime) | Looser than the 99.5% internal SLOs ([slos.md](slos.md)), so an SLO burn alert fires well before the SLA is missed. One cluster and one operator cannot honestly promise more |
| `{{REMEDY_TABLE}}` | Below 99.0%: 10% of the credits the land spent that month. Below 97.0%: 25%. Below 95.0%: 50%. At least 5 credits whenever credits are owed | Scales with how much the land relies on the Service; the floor keeps it from rounding to nothing for a land the outage stopped from spending. The other options offered: fixed 10/25/50 credits per tier, or a looser 98.0% target with tiers at 98/95/90% |
| `{{MAINTENANCE_NOTICE}}`, `{{MAINTENANCE_CAP}}` | 48 hours; 4 hours a month | Enough for a planned cluster upgrade without eating the error budget |
| `{{REMEDY_PAY_DAYS}}` | 10 | |
| `{{NOTICE_WITHIN}}`, `{{NOTICE_UPDATE_EVERY}}` | 30 minutes; 2 hours | The off-cluster alarm needs 3 failing minutes, and the operator is one person. Already this page's aims |
| `{{REVIEW_WITHIN}}` | 7 days | Already this page's aim |
| `{{RESPONSE_TARGETS}}` | Urgent (your land cannot work, or a security or data concern): 4 support hours. Everything else: 2 working days | |
| `{{SUPPORT_HOURS}}` | The owner's working hours, named with their time zone | Only the owner knows them; not given with the v1 decision |
| `{{STATUS_PAGE_URL}}` | <https://gardenio-status-123456789012.s3.amazonaws.com/index.html> (`terraform output status_page_url`, live since 2026-10-21) | A custom domain needs CloudFront and DNS; not in v1 |
| `{{SUPPORT_EMAIL}}` | As the Terms: `support@lands.clonk.tv`, forwarded to the operator by `modules/ses-support-inbound` (step 11.1) | |
