# Alerting runbook — gardenio prod "starter alert pack"

**Scope:** the alerts landed by the starter alert pack (task
[ka-0000001461](https://gardenio.example.com/beads/ka-0000001461), epic
[ka-0000000770](https://gardenio.example.com/beads/ka-0000000770)). These are the
alerts the 2026-07-08 spawn-stall incident
([ka-0000000335](https://gardenio.example.com/beads/ka-0000000335)) exposed as
gaps. Full inventory + PromQL + fireability matrix:
[/bundle/ka-0000004470](https://gardenio.example.com/beads/bundle/ka-0000004470).

## How alerting is wired on prod (read this first)

```text
in-chart vmagent ── scrapes annotated gardenio-system pods ──▶ vmsingle (TSDB)
                                                                  │
                              vmalert evaluates /config/*.yaml rules against it
                                                                  │  firing alerts
                                                                  ▼
                                       Alertmanager (severity-routed receivers)
                                                                  │  Slack incoming webhook
                                                                  ▼
                                                          #gardenio-alerts
```

Load-bearing facts (why rules are authored the way they are):

- **Rules MUST be vmalert ConfigMap keys, NOT PrometheusRule/VMRule CRDs.** On
  new prod (`prod-gardenio-eks`) the VictoriaMetrics operator *converts*
  `monitoring.coreos.com/v1` PrometheusRules to `VMRule`, but there is **no
  VMAlert** provisioned to evaluate them — CRD-path rules are inert. The only
  rule path that fires is the in-chart **vmalert**, which loads
  `-rule=/config/*.yaml` from its own ConfigMap
  (`templates/vmalert/configmap.yaml`, keys `recording.yaml` + `gap-alerts.yaml`).
- **The in-chart vmagent scrapes only annotated pods in `gardenio-system`.** So
  vmsingle has `gardenio_*`/`beads_*` counters, bridge metrics, `aws_*` (from the
  cloudwatch-exporter), and a synthesized `up{}` per annotated pod — but **no**
  kube-state-metrics, cadvisor, karpenter, or node-exporter series. That gap is
  why three of the six incident alerts are blocked pending prerequisite work
  (see [Blocked alerts](#blocked-alerts)).
- **Alert delivery does not flow through slack-bridge.** Alertmanager POSTs the
  Slack incoming webhook directly. A dead slack-bridge degrades bead
  notifications and mention handling but does **not** break paging.

The Slack webhook URL is synced from AWS Secrets Manager
(`gardenio/alertmanager-slack-webhook`) via ESO and mounted as a file — it is
never in the config ConfigMap. If the SM key is not yet seeded, Alertmanager
comes up READY but delivery fails lazily at first fire (send error logged) until
an operator seeds it.

**On k3s** (`deploy-values/gardenio-k3s.yaml`, decision
gd-gMjUuvb3DT, task
gd-YmPhRPJIfp) the same chain
runs with one small Alertmanager (one replica, emptyDir storage). k3s has no ESO,
so the operator creates the Secret by hand:
`kubectl -n gardenio create secret generic alertmanager-slack-webhook --from-literal=url=<incoming webhook>`,
then `kubectl -n gardenio rollout restart deploy/gardenio-alertmanager` if the pod
started first. Delivery test: the always-firing `Watchdog` posts within a minute
of Alertmanager getting the Secret, then once a day. A manual test alert:

```bash
curl -s -XPOST http://gardenio-alertmanager.gardenio.svc:9093/api/v2/alerts \
  -H 'Content-Type: application/json' \
  -d '[{"labels":{"alertname":"DeliveryTest","severity":"warn"},"annotations":{"summary":"k3s Alertmanager -> Slack delivery test"}}]'
```

Until 2026-10-25 k3s ran vmalert with `notifierBlackhole`, so nothing reached Slack.

**Links in the Slack message** (ka-0000008039). Each firing alert renders up to
three links: **📊 Graph** — a Grafana deep link to the
[System Health board](https://gardenio.example.com/grafana/d/gardenio-system-health)
panel that shows the exact signal, pre-scoped to the last hour (built from the
alert's `dashboard` annotation → `gapAlerts.grafanaBaseURL`); **Runbook** — the
`#anchor` in this doc; and **Bead** — the incident bead
(`gapAlerts.incidentBead`). The 📊 Graph link is the one to click first — it
answers "what does this look like right now" without leaving Slack. Set
`vmalert.gapAlerts.grafanaBaseURL=""` to drop the graph link (the alert still
fires).

## Severity vocabulary

| severity | routing | meaning |
|---|---|---|
| `page` | `slack-page` receiver, `<!here>` mention, 1h repeat | act now — user-facing or fleet-wide impact |
| `warn` | `slack-warn` receiver, quiet, 4h repeat | investigate — degraded but not down |
| `ticket` | `slack-warn` receiver, quiet | file/track — growing risk, not yet critical |
| `watchdog` | `slack-watchdog` receiver, 24h heartbeat | dead-man's-switch pulse (see below) |

---

# Fireable alerts

These fire on prod today against series already in vmsingle.

## BeadsDaemonDown

<a id="beadsdaemondown"></a>

**Alerts:** `BeadsDaemonDown` (`up{component="beads"} == 0`, 2m),
`BeadsDaemonAbsent` (`absent(up{component="beads"})`, 5m) · **severity:** page

**Why it matters:** kbeads is the orchestration spine — while it is down, **all**
agent spawns, gates, and decisions halt. `up{component="beads"}` exists because
the kbeads pod is annotated `prometheus.io/scrape=true` on `:9091`.

`up{component="beads"} == 0` is a **scrape-level** signal: vmagent could not
successfully scrape `/metrics` within `scrape_timeout` (10s). That happens for
**two** distinct reasons, and they have opposite fixes — so check latency
*before* assuming the daemon is dead:

1. **The daemon is genuinely down** — crashlooping, OOMKilled, gone.
2. **The daemon is UP but `/metrics` is slow** — it answers, but takes longer
   than `scrape_timeout`, so vmagent scores the scrape as failed and `up`
   reads 0. A perfectly healthy control plane still pages this way.

Case 2 fired for real on 2026-08-24 (ka-0000006261): a saturated Postgres (the
695k-row beads table being seq-scanned by lexical search, ka-0000006300) made
the DB-backed `/metrics` collectors take 10–20s, and `BeadsDaemonDown` paged on
a daemon with 32h uptime and 0 restarts. The fix moved those collectors off the
scrape path onto background refreshers, so `/metrics` is now pure in-memory and
this decoupling holds — but the two staleness gauges below are how you confirm it.

**Triage — latency FIRST:**

```sh
# 1. Is /metrics slow, or is the daemon actually down?
POD=$(kubectl -n gardenio-system get pod -l app.kubernetes.io/component=beads -o name | head -1)
kubectl -n gardenio-system port-forward "$POD" 9091:9091 &
time curl -s localhost:9091/metrics >/dev/null    # >10s => daemon UP, /metrics slow (case 2)
# If it returns fast but up==0 lingered, the daemon really was down/restarting (case 1).

# 2. If /metrics is slow, the background collector refreshers are stalled on the DB.
#    These gauges climb without bound when a refresher can't reach Postgres:
curl -s localhost:9091/metrics | grep -E 'gardenio_beads_(fleet_metrics|embedding_stats)_staleness_seconds'
#    Climbing => chase the DB (RDS reachability, connection exhaustion, a hot
#    seq-scan) — NOT the daemon.

# 3. Only after ruling out latency, check liveness:
kubectl -n gardenio-system get pods -l app.kubernetes.io/component=beads
kubectl -n gardenio-system logs deploy/gardenio-beads --tail=200
# Common case-1 root causes:
#   - RDS unreachable / connection exhaustion (daemon /v1/health pings the DB)
#   - migrate-job failure on a new release
#   - PVC / OOMKill
kubectl -n gardenio-system describe pod -l app.kubernetes.io/component=beads | grep -A5 -i "last state\|reason"
```

`Down` (up==0) = the target is scraped but failing (unreachable OR too slow).
`Absent` = the target has vanished entirely (never scraped / pod deleted). Fix
the pod (or the DB saturation), confirm `/v1/health` returns 200 and `/metrics`
returns well under 10s, and the alert resolves on the next scrape.

## CostMetricsAbsent

<a id="costmetricsabsent"></a>

**Alert:** `CostMetricsAbsent` (`absent(gardenio_account_cost_usd) or
absent(gardenio_agent_cost_usd)`, 10m) · **severity:** page

**Why it matters:** while these series are missing, **every cost/spend panel in
Grafana is dark and fleet spend is unobservable**. The controller exports them
on `:9092`, so if it is crashlooping nothing is scraped.

This alert exists because that exact failure ran **~7.75h unnoticed** on
2026-07-26 (ka-0000006669) — the controller was OOMKill-crashlooping and no
alert covered it. It was caught only because a human noticed the panels were
flat. Backtested against that outage, this rule would have fired at 18:40Z,
10 minutes in.

Note it asserts the **user-visible fact** (the cost numbers exist) rather than
controller pod health. Container-OOM alerts already existed and did *not*
surface this as "cost metrics are dark", so any future cause — OOM, a bad
query, a scrape-config change, an exporter regression — trips this one.

**Triage:**

```sh
kubectl -n gardenio-system get pods -l app.kubernetes.io/component=agents
# Look for OOMKilled / CrashLoopBackOff:
kubectl -n gardenio-system describe pod -l app.kubernetes.io/component=agents | grep -A5 -i "last state\|reason"
kubectl -n gardenio-system logs deploy/gardenio-agents --tail=200
```

**If OOMKilled:** the controller schedules *all* agent pods, so a crashloop here
also starves the crew sent to fix it — raise the memory limit FIRST to break
that deadlock, then work the real cause:

```sh
kubectl -n gardenio-system set resources deploy/gardenio-agents \
  --containers=agents --limits=memory=1Gi --requests=memory=256Mi
```

Then land the limit in `gardenio/helm/gardenio/values.yaml` (`agents.resources`) —
a `kubectl`-only change is reverted by the next helm deploy, which re-opens the
outage. That recurrence happened for real at 03:02Z on 2026-07-27.

The usual root cause is a **query that materializes too many beads** in the
controller. See `advice.FleetArchetypesPaged` for the paged pattern that
replaced the unbounded fetch, and note the daemon silently clamps list requests
to `maxListBeadsLimit = 10000` without erroring.

## VlsingleDiskUsageHigh

<a id="vlsinglediskusagehigh"></a>

**Alert:** `VlsingleDiskUsageHigh` (`sum vl_data_size_bytes{component="vlsingle"}
/ vl_max_disk_space_usage_bytes{component="vlsingle"} >= 0.75`, 1h) ·
**severity:** warn

**Why it matters:** `gardenio-victoria-logs` runs with two retention limits at
once — `-retentionPeriod=30d` **and**
`-retention.maxDiskSpaceUsageBytes=8GiB`. VictoriaLogs enforces
`min(30d, whatever fits in the cap)`: once on-disk bytes approach the cap it
self-trims the **oldest** partitions regardless of the 30d time window. So the
effective retention can silently drop below 30d — the oldest EKS-audit /
edge-detection logs age out early — with **no other signal**. This alert makes
that shortening visible before it happens (watch item ka-0000001302).

The expression is a **ratio against the cap metric itself**
(`vl_max_disk_space_usage_bytes`, which VictoriaLogs exports as the reflected
value of the `-retention.maxDiskSpaceUsageBytes` flag), not a hardcoded 8GiB, so
it tracks the flag automatically and cannot drift from it. `storage` + `indexdb`
are summed because both count against the cap; the `> 0` guard on the
denominator makes the rule inert rather than dividing by zero if the cap metric
is ever absent.

**Prerequisite:** this rule only fires where the `vl_*` series reach vmsingle,
i.e. `vlsingle.metrics.scrape=true` (the pod is then annotated
`prometheus.io/scrape` and the in-chart vmagent discovers it). On prod that is
set in the canonical-prod VictoriaLogs overlay. Without the scrape the rule is a
silent never-fire no-op (advice ka-0000002586).

**Triage:**

```sh
# Live usage vs cap (both are vmsingle series once scraped):
#   sum without(type)(vl_data_size_bytes{component="vlsingle"})
#   vl_max_disk_space_usage_bytes{component="vlsingle"}
# Or straight off the pod's /metrics on :9428:
kubectl -n gardenio-system exec sts/gardenio-victoria-logs -- \
  wget -qO- http://localhost:9428/metrics | grep -E 'vl_data_size_bytes|vl_max_disk_space_usage_bytes'
# What grew the store — ingest by source:
#   vl_rows_ingested_total{component="vlsingle"} by type
```

**If the store legitimately needs more shelf** (a new transport was adopted, a
backfill landed), raise **both** `vlsingle.retention.maxDiskSpaceUsageBytes`
**and** `vlsingle.persistence.size` in IaC together — the PVC must stay larger
than the cap — then reconcile prod. Bumping only the cap wedges the pod on a
full volume; bumping only the PVC leaves the cap trimming early.

## BridgeDown

<a id="bridgedown"></a>

**Alerts:** `BridgeDown` (`up{component=~"slack-bridge|jira-bridge|gitlab-bridge"} == 0`,
5m), `BridgeAbsent` (`absent(up{...})` per bridge, 10m) · **severity:** warn

`BridgeAbsent` has one `absent()` clause per bridge the release enables
(`slackBridge`/`jiraBridge`/`gitlabBridge.enabled`); a disabled bridge has no
`up{}` series by design and used to fire forever (gd-YmPhRPJIfp).

**Why it matters:** the firing series' `component` label names the affected
bridge. Effects by bridge:

- **slack-bridge** — bead lifecycle → Slack, mention handling, `gb slack`
  interactions stall. (Does NOT stop alert paging — Alertmanager posts the
  webhook directly.)
- **jira-bridge** — JIRA ingestion / poller stalls.
- **gitlab-bridge** — MR/pipeline tracking and release-drought watchdog stall.

**Triage:**

```sh
# substitute the component from the alert's label
kubectl -n gardenio-system get pods -l app.kubernetes.io/component=slack-bridge
kubectl -n gardenio-system logs deploy/gardenio-slack-bridge --tail=200
```

**Most common cause is an expired bridge token** — see
[`bridge-token-runbook.md`](bridge-token-runbook.md) for issue/rotate/revoke.

> Coverage note: grafana-bridge is **not** scraped yet
> (no `prometheus.io/scrape` annotation), so it is outside this alert. A
> follow-up bead tracks adding its scrape + a precise crashloop expr
> (`kube_pod_container_status_restarts_total`, which needs KSM).

## BridgeTokenRotationOverdue

<a id="bridgetokenrotationoverdue"></a>

**Alerts:** `BridgeTokenRotationOverdue`
(`gardenio_bridge_token_expires_at_timestamp_seconds - time() < 604800`, i.e. a
bridge fleet token is < 7d from expiry, 1h), and its deadman
`BridgeTokenRotatorStale` (`time() - <last_check> > 3h or absent(<last_check>)`,
15m) · **severity:** page

**Why it matters:** the controller-side rotator
(`controller/internal/bridgetokenrotator`) refreshes each bridge fleet token
before its 720h TTL cliff — updating the `bridge-fleet-tokens` Secret and
bouncing the bridge deployment (task ka-0000002353). If it falls behind, the
token 401s and that bridge's beads writes stop: bead lifecycle → Slack, mention
handling, and JIRA/GitLab ingestion for the firing `fleet` all stall. The alert
sits **inside** the rotator's 14d refresh window (fires at 7d) so it pages only
once rotation is genuinely stuck, not merely "not yet due", leaving a week of
runway. `BridgeTokenRotatorStale` covers the case where the rotator loop itself
wedges — the expiry gauges freeze at their last value, so the overdue rule alone
would go blind.

**Triage:**

```sh
# Which fleet, and how close to expiry (the alert's `fleet` label names it):
#   gardenio_bridge_token_remaining_seconds{fleet="..."}
# Is the rotator erroring rather than idle?
#   gardenio_bridge_token_rotations_total{result="error"}
#   gardenio_bridge_token_rotator_check_errors_total
kubectl -n gardenio-system logs deploy/gardenio-controller --tail=300 \
  | grep fleet-token-rotator
```

**Common causes:**

- **Rotator disabled / dry-run left on** — check
  `agents.bridgeTokenRotation.{enabled,dryRun}` on the release. A `dry_run`
  result counter that keeps climbing while `success` stays flat means the loop
  is exercising the path but never writing.
- **RBAC forbidden** — the controller Role needs `update` on the
  `bridge-fleet-tokens` Secret and the bridge Deployments; a `forbidden` in the
  rotate error log is this.
- **Daemon 5xx** — `check_errors_total` climbing means the daemon token-list
  call is failing; the expiry gauges are held stale (not zeroed) so a transient
  blip does not itself trip this.

**Manual rotation** (if the rotator can't be unblocked in time): follow
[`bridge-token-runbook.md`](bridge-token-runbook.md) to re-issue the fleet token,
patch the Secret, and bounce the deployment by hand.

## PostgresS3BackupStale

<a id="postgress3backupstale"></a>

**Alerts:** `PostgresS3BackupStale`
(`time() - kube_cronjob_status_last_successful_time{cronjob="<release>-postgres-s3-backup"} > 14400`,
15m), and its no-data companion `PostgresS3BackupUnmonitored` (`absent(...)` of
the same series, 4h) · **severity:** warn

**Why it matters:** the `postgres-s3-backup` CronJob
(`templates/postgres/s3-backup-cronjob.yaml`) is the only off-node copy of the
postgres data. A failed run, or one that stays Pending until
`activeDeadlineSeconds` kills it, leaves no trace anywhere else: on 2026-10-20 no
backup succeeded for 16 h and nothing paged (gd-Li0fBnupv2). The series is the
CronJob's `status.lastSuccessfulTime`, which is the completion time of the last
good run. On the k3s schedule (every 2 h, runs of up to 110 min) two good runs
complete at most ~3.5 h apart, so 4 h means one slot was missed.
`PostgresS3BackupUnmonitored` fires when the series is missing: the backup has
never succeeded since install, or `monitoring.podStateMetrics` is off, down, or
cannot list cronjobs. Its 4h hold covers a fresh install's first run.

The series comes from the namespaced kube-state-metrics
(`monitoring.podStateMetrics`), which adds `cronjobs` to its `--resources` and
Role whenever `postgres.s3Backup.enabled` is true. Tune with
`vmalert.gapAlerts.postgresS3Backup.{staleSeconds,for,absentFor,severity}`.

**Triage:**

```sh
NS=gardenio   # the release namespace
kubectl -n $NS get cronjob gardenio-postgres-s3-backup \
  -o jsonpath='{.status.lastSuccessfulTime}{"\n"}{.status.lastScheduleTime}{"\n"}'
kubectl -n $NS get jobs,pods -l app.kubernetes.io/component=postgres-s3-backup
kubectl -n $NS describe pod -l app.kubernetes.io/component=postgres-s3-backup | tail -20
```

**Common causes:**

- **Pending pod** — the backup pod must land in postgres's zone
  (gd-RbR8vI9K5e); no room there means it waits until the deadline kills it.
  `describe pod` shows `FailedScheduling`.
- **DeadlineExceeded** — the dump grew past `activeDeadlineSeconds`
  (2026-10-20: the beads DB reached 3.2 GB and every run hit 900 s). Raise the
  deadline in deploy-values or shrink the DB.
- **Upload failed** — the aws-cli container's log shows an S3 `AccessDenied`:
  the node role lost the bucket prefix grant (terraform `extra_backup_prefixes`).
- **Unmonitored only** — `kubectl get cronjob` shows a recent
  `lastSuccessfulTime` but the alert fires: check the pod-state-metrics pod and
  that its Role lists `batch/cronjobs`.

Restore runbook: [`postgres-backup-restore.md`](postgres-backup-restore.md).

## KeycloakRoleDriftProbeFailing

<a id="keycloakroledriftprobefailing"></a>

**Alerts:** `KeycloakRoleDriftProbeFailing`
(`time() - kube_cronjob_status_last_successful_time{cronjob="<release>-keycloak-runtime-role-drift"} > 5400`,
5m), and its no-data companion `KeycloakRoleDriftProbeUnmonitored` (`absent(...)`
of the same series, 3h) · **severity:** warn

**Why it matters:** a realm user who holds `gardenio-viewer` (or any ranked
`gardenio-*` role) and no `gardenio-project-member` is an unrestricted reader in
kbeads and the viewer, and every Lands applicant is a fresh user
(gd-L2MyfEe9lL, [`security/lands-threat-model.md`](security/lands-threat-model.md)).
The chart never declares such a default, and the EDP operator never removes a
leftover composite, so a hand change persists until someone looks; on 10-25 a
leftover `gardenio-viewer` on `default-roles-gardenio` was found by hand
(gd-llFKMS8Pkc). Every new user holds `default-roles-<realm>`, so the hourly
`keycloak-runtime-role-drift` CronJob (`files/keycloak/role-drift-probe.sh`)
checks **R1**: `default-roles-<realm>` composes no `gardenio-*` role.

A run fails on drift and also when it cannot look (sign-in or read failed), so a
firing alert is either real drift or a blind probe. The series comes from
`monitoring.podStateMetrics`, which watches cronjobs while the probe renders.
Tune with `vmalert.gapAlerts.keycloakRoleDrift.{staleSeconds,for,absentFor,severity}`;
the probe itself is `keycloak.runtime.roleDriftProbe`. Neither the probe nor its
alerts render while `keycloak.realm.defaultGroups` names a group holding a
`gardenio-*` role (the chart default `gardenio-viewers`): there every new user is
a viewer by design. The k3s overlay sets `defaultGroups: []`. A grant through a
default group added by hand is not seen here (operator decision gd-TgsaQWJPQ3
kept the probe read-only, with no canary user); `lands-fence-matrix.sh` K2 checks
it at check-in.

**Credential:** the probe signs in to the realm as its own confidential client
`gardenio-role-drift-probe` (rendered by `keycloak/clients.yaml` while the probe
is on), whose service account holds realm-management `view-realm` and nothing
else. Its secret lives in the chart-made Secret
`<release>-keycloak-runtime-role-drift-client` (key `clientSecret`, kept across
upgrades), which the EDP operator imports into Keycloak.

**Triage:**

```sh
NS=gardenio
kubectl -n $NS get jobs -l app.kubernetes.io/component=keycloak-runtime-role-drift
kubectl -n $NS logs job/<newest role-drift job>     # FAIL R1 names the role
```

**Fix drift** (in the Keycloak pod, after a master-realm `kcadm.sh config credentials`):

```sh
kcadm.sh remove-roles -r gardenio --rname default-roles-gardenio --rolename gardenio-viewer
```

Then re-run the probe with
`kubectl -n $NS create job --from=cronjob/gardenio-keycloak-runtime-role-drift role-drift-manual`.
A sign-in failure (`FAIL setup`) points at the `gardenio-role-drift-probe`
KeycloakClient's status (did the operator import the secret?) or its Secret; a
refused read (`FAIL R1 could not read`, HTTP 403) means the service account lost
`view-realm`; a connection error points at the `keycloak-role-drift-egress`
NetworkPolicy or Keycloak itself.

## OpenPROrphansHigh

<a id="openprorphanshigh"></a>

**Alerts:** `OpenPROrphansHigh` (`max(gardenio_open_prs{state="orphaned"}) > 5`, 5m),
`OpenPROldestOverdue` (`max(gardenio_open_pr_oldest_age_hours{state="open"}) > 48`, 5m),
and `OpenPRHealthStale` (no new count for 4h) · **severity:** warn

**Why it matters:** on 2026-10-28, 64 of gardenio's 92 open PRs were orphans: open,
with no merge escalation and no living owner, so nobody would ever merge or close
them (plan gd-3F4W22h2nT). The hourly PR sweeper ([`docs/pr-sweeper.md`](../../docs/pr-sweeper.md))
now escalates or closes each orphan it finds, and at the end of every run it
records its count as the `pr_health` field of its own agent bead. The controller
republishes the last run's record as the `gardenio_open_prs*` gauges
(`internal/metrics/open_prs.go`, read through `internal/prhealth`), and the
pinned Slack Agent Dashboard shows the same figures as its open-PR line.

- `OpenPROrphansHigh`: the sweeper found more than 5 orphans in one run. It has
  already escalated or closed them; what is wrong is that `gb stop`'s PR park and
  the `kd close` guard let that many through. Read the sweeper's last wrap-up
  (it lists each PR and rule), then find out why those crews ended without
  `gb stop` (OOM, eviction, wall-clock kill).
- `OpenPROldestOverdue`: an open PR into main (desk train/revert and human PRs
  excluded) is more than 48 h old. The record's `oldest_open_pr` names it. It is
  usually a send-back nobody reworked (the sweeper closes those at 72 h), a PR
  the desk keeps returning red or conflicting, or a draft.
- `OpenPRHealthStale`: the sweeper has not recorded a count for 4 h, so the two
  alerts above are reading a frozen count. The trigger is disabled or unsealed
  (`last_outcome` on the trigger bead), or its runs die before Step 2.

The series are absent until the sweeper has recorded a run, so none of the three
fires while it is disabled. Tune with
`vmalert.gapAlerts.openPRs.{maxOrphans,maxOldestOpenHours,staleHours,for,severity}`.
Panels: System Health, row "Open PRs" (panels 24-27).

**Triage:**

```sh
T=$(kd list --type trigger --label pr-sweeper --json | jq -r '.[0].id')
kd show "$T" --json | jq '.fields | {enabled, last_run, last_outcome, last_agent_id}'
kd show "$(kd show "$T" --json | jq -r .fields.last_agent_id)" --json | jq '.fields.pr_health'
gh pr list --state open --base main --json number,createdAt,isDraft,headRefName | jq 'sort_by(.createdAt) | .[:5]'
```

## Dead-man's-switch (alert pipeline liveness)

<a id="dead-mans-switch"></a>

**Alerts:**
- `Watchdog` (`vector(1)`, always firing) · **severity:** watchdog
- `VMAlertNotEvaluating` (`absent_over_time(gardenio:beads_events:rate5m[6m])`,
  5m) · **severity:** page
- `VMAlertDown` (`up{component="vmalert"} == 0`, 5m) · **severity:** page

**The problem this solves:** if the alert pipeline itself dies, no alert can
fire — including an alert about the pipeline being dead. Three complementary
signals:

1. **`Watchdog`** — an *always-firing* alert routed to the `slack-watchdog`
   receiver as a quiet 24h heartbeat ("pipeline is alive"). Its **absence** is
   the real signal: an external monitor (a Dead Man's Snitch, or a cron that
   checks the last heartbeat delivery) must alert when the Watchdog **stops**
   arriving. That absence means vmalert, Alertmanager, or the Slack webhook is
   dead. **Wiring the external absence-check is a tracked follow-up** — until it
   exists, the Watchdog primitive is landed and delivering, but the loop is not
   fully closed.
2. **`VMAlertNotEvaluating`** — the real *internal* self-check, no external
   dependency. vmalert continuously remote-writes the recording rule
   `gardenio:beads_events:rate5m` back to vmsingle. A 0 event-rate is still a
   *fresh* sample, so a quiet fleet does not trip it; the series only goes
   absent when vmalert **stops evaluating** — i.e. vmalert is dead, wedged, or
   its remoteWrite to vmsingle is broken. Expressed as
   `absent_over_time(gardenio:beads_events:rate5m[6m])`, **not**
   `time() - timestamp(series) > N`: with no `-datasource.queryStep` set,
   `timestamp()`'s implicit lookbehind is the 1m `evaluationInterval`, so a
   stale sample ages out of the lookback after ~1m and the staleness value can
   never climb to a multi-minute threshold — that form would be **silent
   exactly when vmalert dies**. `absent_over_time` uses an explicit range and is
   immune. It **can co-fire with `BeadsDaemonDown`** if kbeads itself stopped
   being scraped (its counter vanishes → `rate()` empty → the recorded series
   goes absent); that is itself page-worthy, so the overlap is acceptable
   defence-in-depth, not a false positive.
3. **`VMAlertDown`** — `up{component="vmalert"} == 0`. Requires
   `vmalert.selfScrape` (the vmalert pod is annotated for scrape on `:8880`).
   Bootstrapping caveat: a fully-dead vmalert cannot fire its own down alert, so
   this catches degraded-not-dead; the external Watchdog absence-check remains
   the true safety net.

**Triage when `VMAlert*` fires:**

```sh
kubectl -n gardenio-system get pods -l app.kubernetes.io/component=vmalert
kubectl -n gardenio-system logs deploy/gardenio-vmalert --tail=200
# confirm vmalert can reach vmsingle (datasource + remoteWrite):
kubectl -n gardenio-system exec deploy/gardenio-vmalert -- wget -qO- http://gardenio-vmsingle:8428/health
```

**Triage when the Watchdog heartbeat STOPS (no #gardenio-alerts pulse for >1
cycle):** walk the pipeline hop by hop — vmalert alive? Alertmanager alive
(`kubectl -n gardenio-system get pods -l app.kubernetes.io/component=alertmanager`)?
Slack webhook still valid (check Alertmanager logs for send errors / the SM key
seeded)?

---

## AgentInvoluntaryDeaths

<a id="agentinvoluntarydeaths"></a>

**Alert:** `sum(increase(gardenio_agent_involuntary_deaths_total{family!="context_cancelled_unclaimed"}[1h])) > 5`, `for: 15m` · **severity:** warn

Agents killed mid-work (a *claimed* task in flight) at more than 5/hour for
15m. The `family` label says how: `context_cancelled` (pod SIGTERM'd mid-turn —
node drain / rollout / Spot reclaim), `stop_gate_cooldown` / `stop_gate_rate_limited`
/ `stop_gate_escape_hatch` (Claude exited mid-turn during account rotation
churn), `other` (novel). A sustained rate wastes spend and loses in-flight work.

The `context_cancelled_unclaimed` family (C9 ka-0000001146 — a SIGTERM'd agent
with tool calls on the bead but no claimed issue) is counted on the metric and
charted on the dashboard but **excluded from this alert**: it is a coarse
completeness signal (warm-parked thread agents killed between turns land there
too), and folding it in would re-arm the rollout-churn false positives the
ka-0000008254 precision fix removed.

**Triage:** open the graph (breaks down by `family`), then the dedicated
[involuntary-deaths dashboard](https://gardenio.example.com/grafana/d/gardenio-involuntary-deaths)
for the by-role / by-project split. If `context_cancelled` dominates → node
churn (check karpenter drain / Spot reclaim / a rollout in flight). If
`stop_gate_*` dominates → account rotation is thrashing (check account
utilization + rotation events). Tune the threshold via
`vmalert.gapAlerts.involuntaryDeaths.threshold`.

**`stop_gate_cooldown` specifically — before you go account-hunting.** The
cooldown close (`"Session idled out without an explicit wrap-up"`) fires on a
*quiet turn end*, which is not always an idle agent: an agent that backgrounds a
long command and ends its turn looks identical to one that has stopped working.
Two guards in `cmd/gb/hook.go` (`stopGateCooldownSparesAgent`) already spare that
agent — it still holds a claimed in-progress task, or its pod has a live,
non-shell child process (the spare is logged as
`[stop-gate] cooldown reached but agent is still working (...)`, naming the task
ID or the child `pid N (cmd)`). If closes persist past both guards for a role
whose ordinary turns simply run long, the interim knob is
**`STOP_GATE_COOLDOWN_SECS`** — a plain per-pod container env var (read in
`stopGateInCooldown`; default `StopGateDefaultCooldownSecs` = 30s, doubling per
consecutive block), so it is raised without a code change through the
project/role bead `env_json` field. `env_json` is applied for projects
(`podspec.go` `applyProjectDefaults`) and for roles (`podspec.go`
`applyRoleDefaults`, which runs after project defaults and so wins on a key
both set); there is no agent-level `env_json` override:

```sh
kd update <project-or-role-bead> -f 'env_json={"STOP_GATE_COOLDOWN_SECS":"60"}'
```

Raise it for the narrowest scope that is actually affected — a fleet-wide bump
delays every genuinely-idle close by the same amount.

## AgentNodeReclaimDeaths

<a id="agentnodereclaimdeaths"></a>

**Alert:** `sum(increase(gardenio_agent_involuntary_deaths_total{family="context_cancelled"}[1h])) > 2`, `for: 15m` · **severity:** warn

The **node-reclaim regression sentinel** (epic ka-0000004429 WS4). Same series as
`AgentInvoluntaryDeaths` but filtered to the one family the durable-interruption
work targets: `family="context_cancelled"` — an agent pod SIGTERM'd mid-turn by a
*forced node teardown* (Spot reclaim, AWS health/retirement, interruption-queue),
the class `do-not-disrupt` cannot stop. A **lower** threshold (2/h vs the broad
alert's 5/h) so a node-churn regression that leaves stop-gate churn quiet — and
therefore leaves the 5/h aggregate silent — still fires.

Why it exists: WS1 (graceful-drain nudge, MR !5577) + WS2 (node-churn reduction,
MR !5578) are meant to keep this class rare and always-graceful. This alert is how
you find out they regressed.

**Triage:**

1. **Attribute the death by `close_reason` family before chasing node churn.**
   This alert fires *only* on `family="context_cancelled"` — a pod SIGTERM'd
   mid-turn by a forced node teardown. It does **not** fire on the other
   involuntary-death families, and those are easy to mistake for a node reclaim
   because they also close mid-work "without gb done". The discriminator is the
   agent bead's `close_reason` prefix (the `family` label is derived from it —
   `internal/metrics/events.go` `InvoluntaryDeathFamily`):
   - `context_cancelled_involuntary…` → **node reclaim / forced teardown** (this alert; Spot, AWS health, interruption-queue). Proceed with the steps below.
   - `stop_gate_cooldown_involuntary…` → **account-exhaustion / rotation churn**, *not* node churn (the P0 ka-0000003464 failover class). The pod exited because its account was throttled/swapped mid-session, not because its node went away. Chasing Karpenter here is a dead end — pivot to `AgentInvoluntaryDeaths` (the all-family alert) and the account-pool health, not the node-lifecycle board.
   - `stop_gate_escape_hatch` / `stop_gate_rate_limited` / `other` → other stop-gate paths; likewise not node reclaim.

   List the recent involuntary closes and tally the family — do this by
   *outcome* (bead `close_reason`), which agents can read even where
   `kubectl`/logs are RBAC-denied (advice ka-0000006777):

   ```sh
   kd list --type=agent --status=closed --closed-after <ISO8601> --limit 2000 --json \
     | jq -r '.[] | .fields.close_reason // "null"
              | if test("_involuntary") then (split("_involuntary")[0]+"_involuntary") else . end' \
     | sort | uniq -c | sort -rn
   ```

   If the `context_cancelled_involuntary` count is low but `stop_gate_cooldown_involuntary`
   is high, the deaths are **account churn, not node reclaim** — stop here and
   route to the failover track. Only continue below when the node-reclaim family
   is the one that is actually elevated.
2. Open the [involuntary-deaths dashboard](https://gardenio.example.com/grafana/d/gardenio-involuntary-deaths)
   → the "Node-reclaim sentinel" row shows the per-hour rate against the 2/h line.
3. Confirm work is *not* being lost: each death should still close `done` with a
   wrap-up (drain nudge landed) and any WIP is salvaged to S3. If deaths are
   closing dirty, WS1's `COOP_NODE_DRAIN_NUDGE` may be off — check
   `agents.nodeDrainNudge` on the controller.
4. Find the churn source:
   - **Spot / AWS health:** `kubectl get nodes -l karpenter.sh/nodepool=agent-pool -o wide` and check for `karpenter.sh/disrupt*` taints / recent node replacements.
   - **expiry / AMI drift:** WS2 set `expireAfter=720h` + disruption budgets; if reclaims cluster at a release/terraform-apply, AMI drift is recycling nodes — verify the WS2 budgets applied.
5. The harm is a cold-resume (the turn pauses then resumes), *not* data loss.
   Pivot to the [resume-latency board](https://gardenio.example.com/grafana/d/gardenio-resume-latency)
   (cross-linked top-right of the involuntary-deaths board) to see how fast
   affected agents came back. Tune via `vmalert.gapAlerts.nodeReclaimDeaths.threshold`.

## AgentOOMKilled

<a id="agentoomkilled"></a>

**Alert:** `sum by (project, role) (increase(gardenio_agent_lifecycle_events_total{event="oom_killed"}[1h])) > 0`, `for: 5m` · **severity:** warn

The **direct agent-OOM** alert (Track D, ka-0000001509; operator ask
ka-0000006448). Fires when the controller status reporter observes a kubelet
`OOMKilled` container termination on an agent pod — the live
`gardenio_agent_lifecycle_events_total{event="oom_killed"}` counter — for any
`(project, role)`, so the firing series names who is OOMing.

Why it exists: the only *dedicated* OOM rules before this were the fsm-composite
`FSMAgentOOMWithECSRestart` and the vpa-guardrails `CrewOOMKilled`, and **both are
PrometheusRule CRDs — inert on new prod** (converted to VMRules nothing
evaluates). fsm-composite additionally requires a *coincident ECS task restart*.
So a plain agent OOM reached Slack through **no alert** until this one. It is the
un-joined, always-fireable sibling of fsm-composite, on the vmalert path that
actually fires.

The killed runs are auto-salvaged to S3 (the harm is a cold-resume, not lost
work), so this is `warn`, not `page`. A *sustained* OOM rate is the real signal:
the agent memory limit is too tight for the workload, or a run is leaking.

**Triage:**

1. The graph breaks down by `(project, role)` — identify who is OOMing.
2. Open the [VPA right-sizing dashboard](https://gardenio.example.com/grafana/d/gardenio-vpa-right-sizing)
   for that role: compare working-set memory vs the limit. If the workload is
   riding the limit, widen the memory limit (or the VPA `minAllowed`) per
   [docs/vpa-guardrails.md](vpa-guardrails.md).
3. Deep-dive the OOM slice on the
   [involuntary-deaths dashboard](https://gardenio.example.com/grafana/d/gardenio-involuntary-deaths).
4. If a single run leaked (one-off spike, not a steady rate), no infra change is
   needed — the run cold-resumed. Tune the threshold/severity via
   `vmalert.gapAlerts.oomKilled.{threshold,severity}` (set `threshold` above 0 to
   tolerate a low steady OOM rate for a role, or `severity: page` to page on any
   OOM).

## AgentContainerOOMKilled

<a id="agentcontaineroomkilled"></a>

**Alert:** `sum by (namespace, pod) (increase(container_oom_events_total{namespace=~"gardenio|gardenio-rwx.*"}[1h])) > 0`, `for: 5m` · **severity:** warn

The **child-process-OOM-aware** sibling of [`AgentOOMKilled`](#agentoomkilled)
(ka-0000004184; operator ask Matt 2026-07-24: "a lot of deaths when agents do
workflows … we REALLY need to detect ooms and alert in slack"). Fires when the
cAdvisor cgroup OOM-kill counter `container_oom_events_total` increases for an
agent pod, by `(namespace, pod)` so the firing series names the exact pod.

**Why it exists — the workflow-OOM blind spot:** `AgentOOMKilled` reads the
kubelet's per-container `reason=OOMKilled` status, which the kubelet sets **only
when the container's PID1 is the OOM victim**. Agents run **coop as PID1** and
`claude` + workflow fan-out (`node`/`playwright`/subagents) as **child
processes**. When a workflow balloons and the cgroup OOM-killer reaps a **child**,
coop (PID1) survives → the container is never flagged `OOMKilled` →
`AgentOOMKilled` stays silent, and the death is mislabeled as a generic
`stop_gate_*_involuntary` close. `container_oom_events_total` is the cgroup's
**own** `oom_kill` count (from `memory.events`), so it increments for **any**
process the OOM-killer reaps in the container cgroup — child or PID1 — closing
the blind spot. It also fires under **gVisor** (`runtime_class=runsc`): the K8s
memory limit is enforced at the **host pod cgroup** the host kubelet's cadvisor
observes, regardless of what the sandbox exposes internally.

Same `warn`-not-`page` rationale as `AgentOOMKilled`: a single OOM is a
cold-resume (run auto-salvaged to S3); a *sustained* rate is the
under-provisioning / workflow-memory-leak signal.

**Prerequisite (blocked gap):** `container_oom_events_total` lands in vmsingle
only when the cAdvisor scrape is on
(`vmagent.kubeletScrape.cadvisorOOM.enabled`, which needs
`vmagent.kubeletScrape.enabled`). Arming this alert without that scrape evaluates
a missing series and never fires. The prod overlay
`container-oom-values.yaml` flips **both** together — do the same in any env that
opts in.

**Triage:**

1. The graph breaks down by `(namespace, pod)` — identify the OOMing pod. Map it
   back to its agent bead (the pod name carries the agent name) to see what it
   was doing (a workflow fan-out is the usual culprit).
2. Open the [VPA right-sizing dashboard](https://gardenio.example.com/grafana/d/gardenio-vpa-right-sizing)
   for that role: compare working-set vs the 16Gi limit. A workflow that spawns
   many `node`/`playwright`/subagent children can transiently exceed it.
3. If it is a steady rate for a role, widen the memory limit (or VPA
   `minAllowed`) per [docs/vpa-guardrails.md](vpa-guardrails.md), or reduce the
   workflow's concurrency.
4. Tune via `vmalert.gapAlerts.containerOOMKilled.{threshold,severity}` (raise
   `threshold` above 0 to tolerate a low steady rate, or `severity: page`).

## AgentNodeLifecycleEvents

<a id="agentnodelifecycleevents"></a>

**Alert:** `sum(increase(gardenio_node_lifecycle_events_total{reason="terminated"}[1h])) > 15`, `for: 15m` · **severity:** warn

The **node-lifecycle churn** alert (Track D, ka-0000001509; operator ask
ka-0000006448 — "monitoring and alerts for … node lifecycle events; create a
metric if we do not have one"). Reads the **new** controller-emitted
`gardenio_node_lifecycle_events_total{reason=...}` counter
(`internal/nodelifecycle`), which sweeps the cluster Node API and counts
lifecycle edges: `disrupting` (a Karpenter disruption taint appeared —
consolidation / drift / AMI replacement / `expireAfter` TTL), `deleting` (Node
`DeletionTimestamp` set), `terminated` (a tracked node vanished — instance
reclaim: Spot / AWS health / interruption-queue / scale-down), and `not_ready` (a
Ready-condition flap).

**The alert counts `terminated` only — one count per node that goes away — so the
threshold reads as _nodes per hour_** (ka-0000005966). A single graceful teardown
walks all three going-away edges in sequence (`disrupting` → `deleting` →
`terminated`); summing `disrupting|deleting|terminated` therefore counted ~3
events for one node going away and tripped the per-node `6/h` threshold on just
~2 clean consolidations/h — routine healthy scale-down, not churn. `terminated`
fires exactly once per node regardless of the teardown path (graceful drain **or**
abrupt Spot/AWS-health reclaim), which also keeps sensitivity uniform: an abrupt
reclaim emits `terminated` with little/no `disrupting`, so summing would have made
the alert _least_ sensitive to the most dangerous churn. `disrupting`/`deleting`
are still emitted for the dashboard reason-breakdown (triage step 1); `not_ready`
stays out of the alert entirely (a health blip, not a teardown).

Why it exists — and how it differs from `AgentNodeReclaimDeaths`: that alert
fires only once node churn actually *kills an agent* (`context_cancelled`
involuntary deaths). **This one fires on the teardowns themselves**, including the
ones that miss the agents — the leading indicator that node churn is climbing
before it starts cold-resuming (or losing) work. Node churn is expected
background (Spot, TTL), so this is `warn`; a *sustained* rate above threshold is
the signal.

**Blocked gap / opt-in:** the counter exists only when the collector is enabled
(`agents.nodeLifecycle.enabled`, which also provisions the cluster-scoped
`nodes:list/watch` ClusterRole). The alert (`vmalert.gapAlerts.nodeLifecycle.enabled`)
is therefore **off by default** and flipped on **together** with the collector,
in the same values file. Enabling the alert without
the collector would evaluate a missing series and never fire (silent).

**Do not look for gardenio node logs in Mezmo.** Mezmo ingests the `ld86`
platform cluster, **not** the gardenio prod EKS cluster (`prod-gardenio-eks`); the
only gardenio trace in Mezmo is the ALB access log. There is no node-log source
there to answer "was this node reclaimed" — which is exactly *why* Track D
(ka-0000001509) emits this signal as a controller Prometheus counter on the
already-scraped `:9092` endpoint instead of relying on a log pipeline. Node
lifecycle is answered by this metric (churn) + `AgentNodeReclaimDeaths`
(kills-by-outcome), not by log search. Agents cannot read pod/node logs on prod
anyway (`kubectl` RBAC-denied, advice ka-0000006777) — verify node churn by these
metrics and by agent `close_reason` (the family attribution in
`AgentNodeReclaimDeaths` triage step 1), i.e. by **outcome**.

**Triage:**

1. The graph breaks down by `reason` — `disrupting`/`deleting` lead
   `terminated` for a graceful teardown; a spike in `terminated` with little
   `disrupting` means abrupt reclaims (Spot / AWS health).
2. `kubectl get nodes -l karpenter.sh/nodepool=agent-pool -o wide` — look for
   `karpenter.sh/disrupt*` taints and recent node ages/replacements.
3. Correlate with `AgentNodeReclaimDeaths`: if node churn is up **and** reclaim
   deaths are up, agents are being caught — verify the WS1 drain nudge
   (`agents.nodeDrainNudge`) is on. If churn is up but deaths are flat, the
   teardowns are missing the agents (good) but still worth understanding.
4. Check the churn source: Karpenter disruption budgets, the node `expireAfter`
   TTL (720h on prod per WS2), and any AMI-drift rollout or `terraform apply` in
   flight that recycles nodes. Tune via `vmalert.gapAlerts.nodeLifecycle.threshold`
   to the cluster's steady-state node count. **Calibrated 6 → 15/h on 2026-07-23
   (ka-0000005205):** the 6 was a placeholder never sized to prod. Once counting
   was accurate (per-node + agent-pool-scoped), a busy prod fleet's healthy
   Karpenter WhenEmpty consolidations (empty-node scale-down after completion
   bursts) exceeded 6/h with zero agent impact — so 15/h now sits above busy-hour
   burst-drain while a genuine reclaim storm (spot/AWS-health wave ≫15/h) still
   trips it.

## ThreadReplyDLQBacklog

<a id="threadreplydlqbacklog"></a>

**Alert:** `sum(gardenio_thread_reply_dlq_depth) > 0`, `for: 10m` · **severity:** warn

Slack thread replies are failing and piling up in the dead-letter queue — the
bot has **silently stopped answering** some threads (the "it just went quiet"
failure that otherwise has no alarm). The `component` label names the bridge.

**Triage:**

```sh
kubectl -n gardenio-system logs deploy/gardenio-slack-bridge --tail=200 | grep -i "dlq\|reply\|thread"
```

Look for the reply error (token expiry, Slack 5xx, a malformed thread ref).
`gardenio_thread_reply_dlq_replayed_total` should climb as the queue drains once
the underlying error clears; if it stays flat, the replay loop is stuck.

## TriggerStarved

<a id="triggerstarved"></a>

**Alert:** `sum(gardenio_trigger_starved) > 0`, `for: 15m` · **severity:** warn

A trigger/gardener met its fire condition but has not fired for 15m (cooldown
wedged or starved) — scheduled automation is stalled with no crash to point at.
The graph legend names the offending trigger bead id.

**Triage:** `kd show <trigger-id>` for the trigger's `stop`/`progress` state,
then the engine logs for why it isn't firing:

```sh
kubectl -n gardenio-system logs deploy/gardenio-trigger-engine --tail=200 | grep -i "<trigger-id>\|starv\|cooldown"
```

A legitimately-cooling trigger should clear on its own; a persistently-starved
one usually means its fire condition can never be satisfied (bad query) or the
engine's cooldown accounting wedged.

## TriggerEngineScheduleDark

<a id="triggerenginescheduledark"></a>

**Alert:** `increase(gardenio_trigger_engine_evaluations_total[15m]) == 0 or absent(gardenio_trigger_engine_evaluations_total)`, `for: 10m` · **severity:** page

The trigger-engine's main **evaluation loop is not running at all** — no
`Evaluate()` cycle completed in the window. Every scheduled trigger, gardener
pre-fire, and engine-side liveness sweep (`TriggerStarved`, dark-lane,
gardener-liveness) runs *inside* that loop, so when it wedges the entire fleet's
scheduled automation goes globally dark **with no crash to point at**: the pod
can stay `Ready` with `restartCount=0`. This is the durable hardening for the
30h schedule blackout of 2026-08-09→08-10 (task ka-0000005325), which was
invisible to every existing guard.

**Why `TriggerStarved` cannot cover this.** `gardenio_trigger_starved` is computed
*inside* the same `Evaluate()` loop, so a wedged loop freezes it at its last
value — it can never fire on its own stall. `gardenio_trigger_engine_evaluations_total`
is different: the engine beats it once per **completed** cycle on the poll timer
(default 30s), unconditionally, so `increase(...)==0` catches the wedge and
`absent(...)` catches the pod being gone / never scraped.

**First check /healthz** — it is now LIVE (503s when no `Evaluate()` completed in
`HEALTH_STALENESS_MULTIPLE`× the poll interval, default 10×30s = 300s), so k8s
liveness *should already be restarting* a wedged pod. If it is not, the probe or
the deployment is misconfigured.

**Triage:**

```sh
kubectl -n gardenio-system get pods -l app.kubernetes.io/component=trigger-engine
kubectl -n gardenio-system logs deploy/gardenio-trigger-engine --tail=300
# Look for the last "trigger engine started" / evaluation log line and what came
# after: a hung goroutine, a deadlock, or a blocked daemon call (the loop calls
# the beads daemon each cycle — a daemon outage can stall it).
kubectl -n gardenio-system exec deploy/gardenio-trigger-engine -- wget -qO- localhost:8094/healthz
```

If the pod is Running but `/healthz` 503s and k8s is not restarting it, delete
the pod to force a fresh loop; then find the root cause in the pre-wedge logs. A
concurrent platform outage (e.g. the ka-0000003606 litellm/vmsingle OOM
crashloop that triggered the original incident) can wedge the loop indirectly.

## TriggerEngineMachineAuthUnavailable

<a id="triggerenginemachineauthunavailable"></a>

**Alert:** `sum(increase(gardenio_machine_token_unavailable_total{client="gardenio-trigger-engine"}[10m])) > 0`, `for: 5m` · **severity:** page

The trigger-engine authenticates to the beads daemon as its own Keycloak machine
principal (`machine:gardenio-trigger-engine`, gd-Hc34EENbO9) and **fails closed**
when it has no token: the client Secret is optional on the pod, and a Secret that
was never created simply leaves the mounted file missing; Keycloak can also
refuse or be unreachable. Each such daemon call (trigger stamp, agent spawn,
run-task create / close, sweep) is refused before it is sent and is never
substituted with the static token.

**Why `TriggerEngineScheduleDark` does not cover this.** That alert watches
`gardenio_trigger_engine_evaluations_total`, which counts *completed* `Evaluate()`
cycles. A cycle whose writes all failed `auth_unavailable` still completes, so the
pod stays Ready, the dark alert stays green, and scheduled triggers silently stop
firing. `gardenio_machine_token_unavailable_total{client}` counts exactly the
refused calls.

**Triage:**

```sh
kubectl -n gardenio-system logs deploy/gardenio-trigger-engine --tail=300 | grep auth_unavailable
# The log line carries the cause: "secret ... unreadable" (Secret missing or
# not mounted), "secret file ... is empty", or "token fetch ... failed" with
# Keycloak's response (invalid_client = the KeycloakClient is not reconciled yet).
kubectl -n gardenio-system get secret gardenio-machine-gardenio-trigger-engine
kubectl -n gardenio-system get keycloakclient gardenio-trigger-engine
```

Create the missing Secret (operator step, `clientSecret` key) or wait for the
Keycloak operator to reconcile the client; the engine recovers on the next
refresh with no restart (the secret file is re-read on every fetch).

## TriggerEngineMachineForbidden

<a id="triggerenginemachineforbidden"></a>

**Alert:** `sum(increase(gardenio_machine_forbidden_total{client="gardenio-trigger-engine"}[5m])) > 20`, `for: 5m` · **severity:** page

The trigger-engine holds a valid machine token (`machine:gardenio-trigger-engine`)
but the beads daemon **denies its writes with 403** (`authz:deny`). Unlike
`TriggerEngineMachineAuthUnavailable` (no token at all) the token fetch succeeds,
and unlike `TriggerEngineScheduleDark` the loop still completes cycles, so no
other alert sees it. `gardenio_machine_forbidden_total{client}` counts every 403
on a request carrying that client's token; the daemon's own
`gardenio_authz_denials_total` has no caller label.

**Most likely cause: the first-roll role race.** A release that adds a
`gardenio-machine-*` realm role and the client that uses it together can give the
client a token before Keycloak attaches the role to its service account (2026-10-15,
#370: 155 denied writes in 20 minutes). The client now heals this itself: a token
that lacks the component's expected role is re-fetched after 30s, and any 403
drops the cached token and re-acquires it once (at most once a minute), retrying
the request. If the alert still fires, the role is genuinely missing or does not
cover the write.

**Triage:**

```sh
kubectl -n gardenio-system logs deploy/gardenio-trigger-engine --tail=300 \
  | grep -E 'machine token (acquired|lacks)|re-acquiring'
# "machine token acquired ... realm_roles=[...]" must list gardenio-machine-trigger-ops.
kubectl -n gardenio-system get keycloakrealmrole | grep machine-trigger-ops
```

If the role is in `realm_roles`, the grant does not cover the denied write:
compare the denied action with `kbeads/internal/server/testdata/machine_grants.golden.json`
and the `authz:deny` audit beads. If it is missing, check the KeycloakRealmRole
reconciled (a role description over 255 chars fails the roll; the chart test
`TestKeycloakDescriptionsFitKeycloakLimit` guards that) and that the service
account is mapped to it; restarting the engine also forces a fresh token.

## SlackBridgeMachineAuthUnavailable

<a id="slackbridgemachineauthunavailable"></a>

**Alert:** `sum(increase(gardenio_machine_token_unavailable_total{client="gardenio-slack-bridge"}[10m])) > 0`, `for: 5m` · **severity:** page

The slack-bridge authenticates its **background writes only** as its own Keycloak
machine principal (`machine:gardenio-slack-bridge`, gd-Hc34EENbO9 component 4):
the startup config seed and the dm delivery writes (delivery stamp, close, reply
capture, reminder stamp). It **fails those closed** when it has no token: the
client Secret is optional on the pod, and a Secret that was never created leaves
the mounted file missing; Keycloak can also refuse or be unreachable. Each
refused call is never sent and never substituted with the bridge's fleet token.
Everything else the bridge does, and all reads, stay on the fleet token, so the
pod stays Ready and no other alert moves: DMs still reach Slack, but the dm bead
is not stamped, a fire-and-forget DM is not closed and a question DM's reply is
not captured for the asking agent.

**Triage:**

```sh
kubectl -n gardenio logs deploy/gardenio-slack-bridge --tail=300 | grep auth_unavailable
# The log line carries the cause: "secret ... unreadable" (Secret missing or
# not mounted), "secret file ... is empty", or "token fetch ... failed" with
# Keycloak's response (invalid_client = the KeycloakClient is not reconciled yet).
kubectl -n gardenio get secret gardenio-machine-gardenio-slack-bridge
kubectl -n gardenio get keycloakclient gardenio-slack-bridge
```

Create the missing Secret (operator step, `clientSecret` key) or wait for the
Keycloak operator to reconcile the client; the bridge recovers on the next
refresh with no restart (the secret file is re-read on every fetch). The startup
config seed retries by itself with backoff. Rollback:
`slackBridge.machinePrincipal.enabled: false` and roll.

## SlackBridgeMachineForbidden

<a id="slackbridgemachineforbidden"></a>

**Alert:** `sum(increase(gardenio_machine_forbidden_total{client="gardenio-slack-bridge"}[10m])) > 3`, `for: 5m` · **severity:** page

The slack-bridge holds a valid machine token (`machine:gardenio-slack-bridge`)
but the beads daemon **denies its background writes with 403** (`authz:deny`):
the startup config seed or the dm delivery writes. Unlike
`SlackBridgeMachineAuthUnavailable` (no token at all) the token fetch succeeds,
and the pod stays Ready, so no other alert sees it: DMs still reach Slack but the
dm beads are not stamped or closed and question DMs lose their reply capture.
`gardenio_machine_forbidden_total{client}` counts every 403 on a request carrying
that client's token. dm traffic is light, so the threshold is low.

**Most likely cause: the first-roll role race.** The roles
`gardenio-machine-slack-config-seed` and `gardenio-machine-slack-dm-delivery` and
the client that uses them can arrive in one release, and Keycloak may hand the
bridge a token before it attaches the roles. The bridge heals this itself: a
token lacking an expected role is re-fetched after 30s, any 403 drops the cached
token and re-acquires it once (at most once a minute), and the startup config
seed retries on 403 with backoff. If the alert still fires, a role is genuinely
missing or does not cover the write.

**Triage:**

```sh
kubectl -n gardenio logs deploy/gardenio-slack-bridge --tail=300 \
  | grep -E 'machine token (acquired|lacks)|re-acquiring'
# "machine token acquired ... realm_roles=[...]" must list both
# gardenio-machine-slack-config-seed and gardenio-machine-slack-dm-delivery.
kubectl -n gardenio get keycloakrealmrole | grep machine-slack
```

If both roles are in `realm_roles`, the grant does not cover the denied write:
compare the denied action with `kbeads/internal/server/testdata/machine_grants.golden.json`
and the `authz:deny` audit beads. If one is missing, check the KeycloakRealmRole
reconciled (a description over 255 chars fails the roll; the chart test
`TestKeycloakDescriptionsFitKeycloakLimit` guards that) and that the service
account is mapped to it; restarting the bridge also forces a fresh token.
Rollback: `slackBridge.machinePrincipal.enabled: false` and roll.

## SlackBridgeRespawnPhaseSlow

<a id="slackbridgerespawnphaseslow"></a>

**Alert:** `sum by (phase) (increase(gardenio_slackbridge_respawn_phase_slow_total[1h])) > 0`, `for: 5m` · **severity:** warn

A cold thread/DM agent respawn had a phase run **anomalously slow** — the
firing `phase` label localizes where the time went:

- `phase=dispatch` (>15s): the **bridge** side — parsing the wake-up, resolving
  project/role, and minting the resume bead (including the transient-daemon
  retry loop). A slow dispatch means the beads daemon is struggling
  (5xx/timeout/retry), not that the pod is slow.
- `phase=boot` (>150s): the **pod** side — the controller picks up the
  AgentSpawn, the pod is scheduled, the image is pulled, coop replays the prior
  transcript (`--resume`), and Claude boots to first proof-of-life. A
  multi-minute image-pull or an unschedulable-node stall lands here.
- `phase=total` (>180s): end-to-end (== `gardenio_mention_cold_respawn_seconds`).
  The ~4-min respawn that motivated this alert (task ka-0000001457, Slack thread
  1780697427.499069) sits above this line.

`dispatch + boot ≈ total` by construction — a persistent divergence is itself a
bug in the instrumentation.

**Triage:** graph `gardenio_slackbridge_respawn_phase_seconds` by `phase` on the
System Health board (the 📊 Graph link), then read the slack-bridge logs around
the respawn:

```sh
kubectl -n gardenio-system logs deploy/gardenio-slack-bridge --tail=400 | grep -i "respawn\|resume\|SessionStart"
```

If `phase=boot` dominates, check the target node's image cache / pod scheduling
(`kubectl -n gardenio-system get pods -o wide`, describe the respawned pod); if
`phase=dispatch` dominates, check beads-daemon health (`up{component="beads"}`,
its 5xx rate) — the mint is retrying against a struggling daemon.

## UnattributedSpendHigh

<a id="unattributedspendhigh"></a>

**Alert:** unattributed / (unattributed + attributed) trailing-1h bill ratio
`> 0.15`, gated on `> $5` 1h fleet spend, `for: 1h` · **severity:** ticket

The share of the LiteLLM bill carrying **no `agent:` request tag** exceeded the
threshold. tagSpendSync (`cmd/controller/tag_spend_sync.go`) splits the whole
bill each 5m tick: `gardenio_agent_attributed_cost_usd` (agent-tagged — what
per-agent settlement can book) + `gardenio_unattributed_cost_usd` (each proxy's
`/global/spend` bill minus its `agent:*` tag totals), so the two always sum to
the real bill (ka-0000003988). A rising residual means money the per-agent
settlement pipeline can never attribute: a new untagged consumer (an internal
service or operator key without `x-litellm-tags`) or a tag-propagation
regression (credential rotation dropping the tags header — ka-0000000333 is the
known open case and the reason the threshold starts at 0.15; ratchet to ~0.05
once fixed).

**Triage:** enumerate the untagged rows (master key from Secrets Manager
`gardenio/prod/litellm/master-key`):

```sh
curl -s "http://gardenio-litellm:4000/spend/logs/ui?start_date=<Y-m-d>%2000:00:00&end_date=<Y-m-d>%2000:00:00&page=1&page_size=100" \
  -H "Authorization: Bearer $MASTER_KEY" |
  jq '.data[] | select((.request_tags // []) | map(startswith("agent:")) | any | not)
      | {spend, key_alias: .metadata.user_api_key_alias, ip: .metadata.requester_ip_address, tags: .request_tags}'
```

Key alias + requester IP identify the consumer. An untagged row whose only tags
are `User-Agent: claude-cli` is an agent pod that lost its header
(ka-0000000333 pattern — check whether the flip coincides with a credential
rotation); a non-claude-cli UA is a genuinely new consumer that needs
`x-litellm-tags` added at its call site.

## DarkSpendOverThreshold

<a id="darkspendoverthreshold"></a>

**Alert:** `sum(increase(gardenio_agent_spend_usd_total{project=~"monorepo-dark"}[1h])) > 25`
(USD/hour), `for: 15m` · **severity:** ticket · **off by default**
(`vmalert.gapAlerts.darkSpend.enabled`)

The autonomous (**dark**) fleet's trailing-1h LiteLLM spend rate crossed the
operator-set guardrail. This is the **observe** half of the R6-FU6 cost pair
(ka-0000007149); the per-agent/day token budget is the **enforce** half. The
signal is keyed to **litellm-cost-tracking**: `gardenio_agent_spend_usd_total`
is the per-turn LiteLLM spend counter the controller `tag_spend_sync` daemon
feeds from LiteLLM spend logs (labels `{agent,project,role,model,account}`) —
the same counter the `gardenio:agent_cost_usd:rate1h` recording rule and the
**Gardenio Dark Spend** dashboard (`/d/gardenio-dark-spend`) read.

**Triage:** open the Gardenio Dark Spend board and read the per-agent and
per-gardener spend-rate panels to see WHO is driving the burn. Then decide:

- **Expected growth** (more agents legitimately working) → raise
  `vmalert.gapAlerts.darkSpend.threshold` (and the board's `threshold`
  variable to match).
- **Runaway gardener/loop** → throttle or kill the offending gardener
  (kill-switch); a single agent or gardener dominating the top-spenders table
  with a climbing rate is the classic shape.
- **Spend regression** (money you cannot attribute) → cross-check
  [UnattributedSpendHigh](#unattributedspendhigh): an untagged consumer or a
  tag-propagation regression inflates the bill without a matching agent.

**Silent-before-federation caveat:** dark-fleet spend only exists in this
vmsingle once the dark controller's metrics are scraped/federated here (cf. the
parked `gitlab-dark-drift` board, whose exporter is not yet applied). Until
then `increase()` over the absent series is no-data and the rule cannot fire —
which is why it ships **off by default**. Flip `darkSpend.enabled=true` and tune
`threshold` / `projectRegex` the day dark spend lands.

## CostXCheckDrift

<a id="costxcheckdrift"></a>

**Alert:** `gardenio_xcheck_agreement{pair!~"<overridden pairs>"} < 0.95`,
`for: 6h` (plus one rule per non-zero `pairThresholds` override) · **severity:** ticket

A cost-audit **conservation pair** — the same quantity derived from two
*independent* sources by the controller x-check sweep
(`cmd/controller/xcheck_sweep.go`, C2 ka-0000000950) — has scored below its
agreement floor for six hours straight. Agreement is
`1 − |A−B| / max(A,B,ε)` in `[0,1]`; a sustained low score means one of the
two derivations is mismeasuring (the operator's "double-check principle":
corroborated numbers are probably real, diverging ones are a bug somewhere).
The firing labels name the `pair` and `env`. The sweep never publishes a pair
it could not derive both sides of, so this alert cannot fire on missing data —
a *vanished* pair shows as a gap on the dashboard instead.

What each pair corroborates (full derivations: plan bundle
[ka-0000004761](https://gardenio.example.com/beads/bundle/ka-0000004761)):

| pair | side A | side B |
|---|---|---|
| `bill-conservation` (P1) | Σ LiteLLM `/key/info` per account | attributed + unattributed gauges — **floor 0.85**: side A carries each key's lifetime spend (incl. proxies no longer polled) and double-counts any two accounts sharing a `litellm_key`, so it structurally reads ~9% high vs side B; 0.85 catches the reconstruction path widening past that (ka-0000002467) |
| `tag-conservation` (P2) | Σ tags dim=role | Σ tags dim=project / dim=model |
| ~~`settled-vs-bill` (P3)~~ | **RETIRED (ka-0000000543).** Side A was the at-close per-agent settled *counter* `gardenio_agent_spend_usd_total`, which went dead in prod ($36 lifetime, ~0.09% of a ~$40k bill), so the pair read structurally low at any floor — a broken measurement, not a settlement collapse (the unsettled backlog sawtooths to 0 and attribution conserves; P0 ka-0000002699). The bill is corroborated by P1 `bill-conservation` (~0.99) and per-agent settled attribution by P10 `agent-spend-parity` (~0.93–0.99, the stateless `/spend/logs` recompute that replaced this deprecated stateful path). The counter's non-xcheck consumers are retired in ka-0000005951. |
| `sessions-vs-agent` (P4) | Σ session `settled_cost_usd` | agent LiteLLM close − first-baseline |
| `rollup-vs-tree` (P5) | gardener `gardener_spend_usd` rollup | live BFS sum over the crew tree |
| `token-parity` (P6) | coop-scraped session token counters | gb-pushed `gardenio_session_tokens_total` |
| `interrupt-parity` (P9) | `increase(gardenio_agent_involuntary_deaths_total[24h])` | kbeads scan of close_reasons the emitter's classifier counts (`_involuntary` families + `context_cancelled_unclaimed`), same window |

> _(P7 `estimate-vs-settled` was **removed** — ka-0000007296. Its side A, the
> in-pod JSONL cost estimate, has had no producer since coop went cost-free on
> 2026-06-06, so it never published a score.)_

> **The structural pair P1 `bill-conservation` ships with a tuned per-pair floor
> (0.85), not the 0.95
> default.** Its two sides are not constructed to reconcile to within 5%, so a
> flat 0.95 floor fires forever on a healthy fleet — the divergence is real but
> *stable and expected* (ka-0000002467). Each floor sits below the observed
> steady-state with margin, so the alert still trips on a genuine widening.
> P3 `settled-vs-bill` was the third such pair until its side-A counter went dead
> and it was **retired** (ka-0000000543) rather than re-floored — the honest fix
> when a pair's source stops measuring is to remove the pair, not to loosen a
> floor to hide a broken number. If you need per-agent settled attribution to
> actually agree with the bill (not just stay flat), that is the P10 stateless
> `/spend/logs` recompute path that replaced P3's at-close counter — not an
> alert-threshold change.

**Triage:**

1. Open the [xcheck-trust dashboard](https://gardenio.example.com/grafana/d/gardenio-xcheck-trust)
   (the 📊 Graph link lands there pre-scoped to 24h). The "Side A vs side B —
   \<pair\>" panel shows **which** derivation moved and **when** — line up the
   inflection with a release, a rotation event, or a poller change.
2. Check the sweep's own health first: `kubectl -n gardenio-system logs
   deploy/gardenio-controller --tail=300 | grep -i xcheck` — `pair compute
   failed` / `scan truncated` lines mean the pair is degraded at the source,
   not genuinely diverging.
3. Then chase the side that moved: metric-backed sides (P1/P2/P6, P9-A)
   are instant queries against vmsingle — run the pair's PromQL by hand;
   bead-scan sides (P4/P5, P9-B) re-derive from kbeads fields — spot-check
   a few beads with `kd show`.
4. Tune via `vmalert.gapAlerts.xcheckDrift.threshold` (default floor),
   `.pairThresholds.<pair>` (per-pair floor; `0` opts the pair out), `.for`.

A drifting pair does **not** stop the fleet — it flags that every cost number
the pair qualifies is untrustworthy until reconciled. Treat a wontfix-style
mute (raising a pair's floor) as a documented decision, not a reflex.

---

# Blocked alerts

<a id="blocked-alerts"></a>

These are landed in the chart **disabled by default** because the metric they
need is not in vmsingle today (the vmagent scrapes only annotated
`gardenio-system` pods — no KSM / cadvisor / karpenter / node-exporter). Each has
a `vmalert.gapAlerts.<name>.enabled` toggle and a `prereqBead`. **Do not enable
one until its prerequisite series exists in vmsingle**, or it evaluates against a
missing series and never fires (silent).

## AgentFleetSizeHigh (P1b — fleet-size / cost)

<a id="agentfleetsizehigh"></a>

`gardenio_agent_node_count > 30` for 15m · **warn** · routed to
**`#aws-cost-management`** (via `subsystem="cost"` + `alertmanager.costRoute`).
Toggle `vmalert.gapAlerts.fleetSize.enabled`; **prereq bead** `ka-0000001509`
(the node-lifecycle collector — same collector, RBAC, and prereq as
[AgentNodeLifecycleEvents](#agentnodelifecycleevents)).

**Why it matters (P1b, ka-0000003023).** The gardenio account's EC2 spend is
agent-fleet nodes scaling under Karpenter, and the fleet is **100% On-Demand** —
so a bigger fleet is a bigger bill. This alerts on the **leading** indicator, not
the trailing charge: `gardenio_agent_node_count` (a controller-emitted
current-state gauge over the agent-pool node list) rises the *moment* Karpenter
provisions a node, hours before the `BoxUsage` line lands on the bill. The
companion gauge `gardenio_agent_node_vcpus` carries total provisioned vCPU — a
better $/hr proxy when instance types are mixed (`r7i.2xlarge` vs `m5.large`).

**Why controller-emitted, not `karpenter_*` / CloudWatch Container Insights.**
Both gauges are a tally over the node LIST the node-lifecycle collector already
runs, published on the controller `:9092` endpoint prod already scrapes with zero
extra config. `karpenter_*` needs the optional cross-namespace scrape (absent in
the base chart); Container Insights needs the CloudWatch agent. This path is
guaranteed present wherever the OOM/node-lifecycle series are.

**Triage:**

1. Is the growth **real demand** (a legitimate work surge — many concurrent
   gardeners/crew) or **waste** (under-utilized/idle nodes Karpenter has not
   consolidated)? Check `gardenio_agent_node_vcpus` and `gardenio_agents_active`
   against the node count — a high node count with low active-agent count is idle
   capacity the **P2b right-sizing** (ka-0000006799) should reclaim.
2. `kubectl get nodes -l karpenter.sh/nodepool=agent-pool -o wide` — confirm the
   live count and instance types.
3. If sustained, this is the cost-control epic's signal (ka-0000006909): tighten
   Karpenter consolidation/TTL, right-size over-provisioned types, or kill stale
   parked environments.

**Idle-node detection is delivered; real-usage utilization is still blocked.**
The *unblocked* half of the utilization ask — "provisioned but nothing scheduled"
idle nodes — now ships as [AgentFleetIdleNodes](#agentfleetidlenodes) below
(ka-0000001398). Alerting on *sustained low CPU/mem utilization* (busy-looking
nodes barely using their requests) is a separate follow-up that remains blocked:
it needs per-node CPU/mem series (node-exporter or cadvisor / Container Insights)
that are **not** in vmsingle today — the vmagent scrapes only annotated
`gardenio-system` pods. It is deliberately not half-built (a util alert against a
missing series would silently never fire).

## AgentFleetIdleNodes (P1b-follow-up — idle nodes / cost)

<a id="agentfleetidlenodes"></a>

`gardenio_agent_idle_node_count > 2` for 30m · **warn** · routed to
**`#aws-cost-management`** (via `subsystem="cost"` + `alertmanager.costRoute`).
Toggle `vmalert.gapAlerts.idleNodes.enabled`; **prereq bead** `ka-0000001398`
(idle-node detection — `agents.nodeLifecycle.idleNodes.enabled`, itself gated on
the node-lifecycle collector [AgentNodeLifecycleEvents](#agentnodelifecycleevents)).

**Why it matters (P1b-follow-up, ka-0000001398).** An agent-pool node hosting
**zero active agent pods** is On-Demand capacity Karpenter has not yet
consolidated — the fleet is paying for a node doing nothing. This is the direct
*waste* signal, complementary to the *growth* signal
[AgentFleetSizeHigh](#agentfleetsizehigh): a high node count with idle nodes is
capacity to reclaim, not real demand.

**Why controller-emitted, no new scrape.** `gardenio_agent_idle_node_count` is
derived by the **same** node-lifecycle collector from the agent-pool node list it
already runs, plus a namespace-scoped agent-pod LIST
(`app.kubernetes.io/name=gardenio`) using the controller's **existing** pods:list
RBAC. No per-node CPU/mem series, no new scrape, no new grant. This is the
UNBLOCKED half of the utilization ask; **real sustained-low CPU/mem utilization**
(a busy node barely using its requests) is a separate blocked follow-up needing
node-exporter/cadvisor series absent from vmsingle.

A pod counts a node *busy* unless it is unscheduled or in a terminal phase
(`Succeeded`/`Failed`) — a node hosting only completed pods is idle capacity.

**Triage:**

1. Confirm the idle count against the fleet: compare `gardenio_agent_idle_node_count`
   to `gardenio_agent_node_count` and `gardenio_agents_active`. A large idle
   fraction is reclaimable capacity.
2. `kubectl get nodes -l karpenter.sh/nodepool=agent-pool` then
   `kubectl get pods -A --field-selector spec.nodeName=<node>` — confirm the idle
   nodes host no active agent.
3. If sustained, tighten Karpenter consolidation / `expireAfter` TTL so idle
   nodes are torn down faster, or right-size over-provisioned instance types. This
   is the cost-control epic's signal (ka-0000006909).

## AgentPodsPendingTooLong (GAP-1)

<a id="agentpodspendingtoolong"></a>

`sum(gardenio_agent_pods_pending) > 0` for 10m · page. **Blocked on:** a
controller current-state gauge `gardenio_agent_pods_pending`. The existing
`gardenio_agent_pod_phase_transitions_total` is a *counter* of transitions, not a
live gauge of how many pods are Pending right now. This alert would have caught
the 2026-07-08 incident directly. Enable after the controller gauge lands.

## AgentPoolMemoryNearLimit (GAP-2)

<a id="agentpoolmemorynearlimit"></a>

`karpenter_nodepools_usage / karpenter_nodepools_limit >= 0.90` for 15m · page.
**Blocked on:** scraping `karpenter_*` into the in-chart vmagent (cross-namespace
scrape into `kube-system` + RBAC beyond the current namespace-scoped Role). This
is the ~12h-early capacity warning for the incident's root cause. Metric names
vary across karpenter versions — verify against the running build
(`curl karpenter-metrics:8080/metrics | grep nodepool`) before enabling. When
you raise the nodepool memory limit, do it in IaC and reconcile any live
`kubectl patch` drift.

## AccountThrottled

<a id="accountthrottled"></a>

**Alert:** `count(max by (account) (gardenio_account_throttled{account!~"(?i:e2e-.*|probe-acct-.*)"}) > 0) > 0`,
`for: 15m` · **severity:** warn · **default OFF**
(`vmalert.gapAlerts.accountThrottled.enabled`)

One or more **credential accounts** have been rate-limited by the provider
(HTTP 429) for the hold window. `gardenio_account_throttled` is set to 1 by the
controller (`internal/metrics/metrics.go`) when the Anthropic rate-limit probe
reports `throttle_status="rejected"` for an account bead. E2E/probe throwaway
accounts are excluded at the expr (byte-identical to the beads-viewer by-account
rollup matcher, ka-0000005167), and `count(max by (account)(...)>0)` dedups the
per-pod gauge republish so a single throttled account counts once.

**Why warn, not page:** the coopmux credential broker fails over to un-throttled
accounts, so an individual throttle degrades **capacity headroom**, not
availability — work continues on the remaining pool. A *sustained* count is the
signal that the fleet is leaning on fewer and fewer accounts and is approaching a
real capacity squeeze.

**Triage:** open the **Account Capacity & Rate-Limit Health** board
(`/d/gardenio-account-capacity`). Its throttle timeline shows *which* account(s)
throttled and *when*; the 5h/7d utilization panels alongside answer the *why* —
a throttle almost always follows an account's 5h-window utilization riding near
1.0. Correlate:

- If the throttled account was already **near its 5h ceiling** (util ≥ ~0.9 on
  the 5h panel): expected transient under heavy load — watch that the broker's
  warming/soft-failover timeline shows it easing back in, and that load
  redistributes to accounts with headroom.
- If **several** accounts throttle at once, or one stays throttled while others
  have headroom (a stuck/mis-weighted pool): the fleet is capacity-constrained —
  consider adding credential-account capacity or reducing concurrent fleet size.

Tune `vmalert.gapAlerts.accountThrottled.threshold` (raise to only fire when
multiple accounts throttle simultaneously) and `.for` to taste per env.

## ClaudeUpstreamOutage

<a id="claudeupstreamoutage"></a>

`sum(gardenio_agent_upstream_error_active{category="server_error"}) > 1` for 10m ·
**page**. Leg A of the Claude-outage detector (epic
[ka-0000000487](https://gardenio.example.com/beads/ka-0000000487)).

**What it means.** Two or more agents have been parked on an upstream provider
error (coop `error_category=server_error` — HTTP 500/502/503/529, `api_error`,
`overloaded`) for ten minutes. `gb init`'s in-place recovery (dismiss the error
screen → short backoff, base 5s capped at 2m → re-inject the prompt) has
therefore already tried and failed several times, so this is not a blip.

**Triage — the one question is "us or them?"**

1. **Click the 📊 Graph link, or open <https://status.claude.com>.** Read
   `gardenio_claude_status_component{component="claude_api"}` (leg B): `0`
   operational, `1` degraded, `2` partial_outage, `3` major_outage. Also check
   `gardenio_claude_status_open_incidents`.
2. **If the status page corroborates → it is THEIRS.** There is no gardenio fix.
   Agents self-recover when upstream does — the recovery branch keeps re-nudging
   the same session, so work is not lost. The useful action is to stop feeding
   the fleet new work (hold spawns, pause gardeners) so you are not burning
   retries, and to let the thread know. Do not go debugging gardenio.
3. **If the status page is CLEAN → it is OURS**, and the clean page is real
   evidence, not an absence of it. Suspect, in order: the LiteLLM proxy
   (`kubectl -n gardenio-system logs -l app.kubernetes.io/component=litellm`),
   pod egress/DNS, or a credential path returning 5xx. Check whether the errors
   are concentrated in one `project`/`role` — the gauge is labelled, and a
   single-project blast radius points at that project's account or model config
   rather than at Anthropic.
4. **Check the sibling categories.** The same gauge carries `rate_limited`,
   `model_rate_limited`, `no_internet`, etc. `sum by (category)
   (gardenio_agent_upstream_error_active)` disambiguates a provider outage from
   pool exhaustion or a local connectivity fault in one query.

**Why it is gated on our errors, not on the status page.** Alerting on the status
page alone pages for blips that never touch our traffic (claude.ai down while
`api.anthropic.com` is fine). Alerting on our errors alone leaves the oncall
guessing whose fault it is. The page fires on leg A and *quotes* leg B, so the
Slack message self-diagnoses.

**History.** Filed after the 2026-07-29 outage (status.claude.com incident
`q2kg8n613kr3`, "Elevated errors across all models", impact critical) ran ~2h
with `#gardenio-alerts` silent. It was the second in five days — see the comment
at `controller/internal/bridge/init.go:103-113` recording the 2026-07-25 opus-5
outage that ran ~4.5h fleet-wide with `total_errors=0` on every affected bead.

## ClaudeStatusPageOutage

<a id="claudestatuspageoutage"></a>

`max(gardenio_claude_status_component{component=~"claude_api|claude_code"}) >= 2`
for 5m · **warn** (deliberately not a page).

**What it means.** status.claude.com reports partial_outage or worse on a
component our agent traffic depends on, but no gardenio agents are confirmed
failing — if they were, `ClaudeUpstreamOutage` would be firing alongside this,
and *that* is the one to act on.

**What to do.** Treat it as advance warning: it usually precedes leg A and
pre-explains the pages that follow. Consider holding back new spawns. No gardenio
change will help.

**Two reasons this is not a page.** It is scoped to `claude_api` / `claude_code`
only — claude.ai, Console and Cowork degradation is filtered out because it does
not touch us. And Statuspage keeps an incident open through its `monitoring`
window well after service is restored, a tail state observed live while building
this detector (components back to `operational`, incident still open at
`critical`), so a pager keyed on the status page would stick long after recovery.

## ClaudeStatusPollerStale

<a id="claudestatuspollerstale"></a>

`absent(gardenio_claude_status_indicator)` or last successful check older than
600s, for 15m · warn.

**What it means.** Leg B is blind. A firing `ClaudeUpstreamOutage` will not be
able to tell the oncall whether the fault is ours or the provider's — the alert
still fires, it just loses its self-diagnosis.

**Triage.** Check the controller logs for `claude status poll failed` and the
`gardenio_claude_status_check_errors_total` counter, then verify pod egress to
`status.claude.com` (no auth required; the poller follows the
status.anthropic.com → status.claude.com 302). Note the poller is disabled
outright when `GARDENIO_CLAUDE_STATUS_POLL_INTERVAL=0`, which is the first thing
to rule out.

The poller deliberately leaves its status gauges **stale** on a failed fetch
rather than zeroing them: publishing "all operational" on a failed poll would let
a network partition — or the status page itself going down, which correlates with
an outage — actively suppress `ClaudeUpstreamOutage`. That is why staleness needs
its own alert.

## AgentUpstreamErrorEmitterDown

<a id="agentupstreamerroremitterdown"></a>

`absent(up{component="agents"}) or max(up{component="agents"}) == 0` for 15m ·
warn.

**What it means.** The controller's `/metrics` target is gone, so the status
reporter is not publishing `gardenio_agent_upstream_error_active` and
**`ClaudeUpstreamOutage` cannot fire** — the fleet is un-alerted for provider
outages, which is exactly the 2026-07-29 blind spot this epic closed.

**Why it does not watch the gauge directly.** `gardenio_agent_upstream_error_active`
is a snapshot cleared to no-data whenever no agent is erroring, so
`absent()` on it is the *healthy* steady state and such a rule would fire
permanently on a well-behaved fleet — no `for` window rescues that. This rule
watches the scrape path the gauge depends on instead. Note the label value is
`agents`, not `controller`: the controller Deployment carries
`app.kubernetes.io/component=agents`, and a rule written against
`up{component="controller"}` matches no series and is silently never-firing.

**Triage.** Confirm the controller pod is Running and its `:9092/metrics`
responds, then check vmagent is still scraping it.

## Postgres efficiency budgets

<a id="postgres-efficiency-budgets"></a>

Seven warn alerts on the postgres_exporter sidecar's series
(`templates/_postgres-budgets.tpl`, thresholds in `vmalert.postgresBudgets`;
gd-ylgk1jBhLi, epic gd-PkZRCb0BuB). They render only with
`postgres.exporter.enabled` and a notifier. The fix is a cheaper query or
fewer calls, not a bigger node.

| Alert | Fires when |
|---|---|
| `PostgresQueryDominatesExecTime` | one statement is over 20% of the hour's statement exec time, once the hour's total is over 60 s |
| `PostgresSlowFrequentQuery` | a statement averages over 1 s per call over the hour, with over 100 calls |
| `PostgresLongActiveQuery` | an active query is older than 30 s (for 1m) |
| `PostgresQueryPileUp` | more than 3 backends run one queryid at once (for 2m) |
| `PostgresTableGrowth` | a table (with indexes and TOAST) grew over 500 MiB, or over 25% while over 100 MiB, in 24h |
| `PostgresBeadsCacheHitLow` | the beads cache hit ratio is under 95% for 15m |
| `PostgresCPUSaturatingNode` | the postgres container uses over 80% of its node's CPU for 30m |

pg_dump's `COPY ... TO stdout` statements count in the total but never breach
the statement budgets, and `gardenio-backup` backends never breach the long-query
or pile-up budgets: a dump is long and heavy by design.

**Triage.** Open the Grafana board "Postgres — Efficiency"
(`gardenio-postgres-efficiency`). The alert's `queryid` and `query` labels name
the statement; `pg_active_statements_backends` by `application_name` names who
is running it; for `beads`, `gardenio_beads_store_query_duration_seconds` by
`method` ranks the kbeads store methods.

**The daily report.** The controller (`internal/pgbudget`) posts the day's top
5 statements by total time with the change on the day before, the tables by
24h growth and the budget alerts that fired to the gardenio land's `#machines`
at `postgres.exporter.report.hourUTC` (08:00 UTC). It judges each of the last
two days by the same thresholds, and a statement or table over budget on both
gets one task (label `pg-budget`, field `pg_budget_key`, under the epic). One
task per queryid or table while it is open or closed in the last 7 days, at
most 3 new tasks a day.

## REAPER_NSGUARD — reaper never-match invariant (ka-0000001816)

<a id="reaper-nsguard"></a>

The `rwx-reaper` carries a **never-match denylist** (`rwxReaper.protectedNamespaces`,
default `[gitlab-dark]`). A protected namespace like **gitlab-dark**
(monorepo-dark) is non-ephemeral and survives today ONLY because its name does
not start with `NS_PREFIX=gardenio-rwx-` — `lifecycle=keep` is a no-op for a
non-prefixed namespace (the keep check sits inside the prefix-gated loop). If a
future edit widens `NS_PREFIX` or the lab is renamed under the prefix, that
accidental protection evaporates.

The guard is the defense in depth: a protected namespace that matches the prefix
is **refused fail-closed** at classification and again at each delete/finalize
site (it is never deleted), the reaper logs the distinctive **`REAPER_NSGUARD`**
marker, and the Job **exits non-zero (exit 3)** so the invariant breach surfaces
as a failed Job in the CronJob `failedJobsHistory`.

**If you see `REAPER_NSGUARD` in a reaper log / a failed reaper Job:** the
never-match invariant broke — no protected namespace was deleted, but
`NS_PREFIX` now matches one. **Narrow the prefix** (or rename the namespace out
from under it) immediately; do not widen the reaper's blast radius. The
log-plane alert for this marker is the Mezmo View in `deploy-values/gitlab-dark/mezmo/`
(onprem GitLab Runner infra, excluded from this export; operator-gated on
prod→Mezmo log ingest — the reaper's stdout is not ingested into Mezmo today;
see that README and the "do not look for gardenio node logs in Mezmo" note
above).

---

## Doc drift reconciled (ka-0000004714)

The root `CLAUDE.md` formerly described the rwx-reaper as `maxConcurrent=20` /
prune past `90m`. The **live new-prod config is `maxConcurrent=10`,
`ttlMinutes=45`** (`day0-wiring/rwx-reaper-values.yaml`, matching the Helm chart
`values.yaml` defaults). The root
`CLAUDE.md` wording was reconciled to `ttlMinutes=45` / `maxConcurrent=10` in
ka-0000004714.

---

# PROVE run — 2026-07-09 (acceptance gate ka-0000004137)

<a id="prove-2026-07-09"></a>

This is the acceptance-gate verification for the starter alert pack: proof that a
firing alert reaches `#gardenio-alerts` on prod, with severity + runbook link, and
that severity-based routing works. Task
[ka-0000004137](https://gardenio.example.com/beads/ka-0000004137) (parent epic
[ka-0000000770](https://gardenio.example.com/beads/ka-0000000770)). Re-run this
recipe any time you need to re-verify the delivery path.

## Environment at verification time

Probed live from a `gardenio-system` pod (which shares the pod network with the
`ClusterIP` services), release **2026.190.3**:

- Alertmanager `v0.28.1`, config loaded (5.6 KB), single-node (`cluster.status =
  disabled`), Slack webhook mounted via `slack_api_url_file:
  /etc/alertmanager/slack/url` (ESO-synced from SM
  `gardenio/prod/alertmanager-slack-webhook`).
- Route tree: default `receiver=slack-warn`; `severity="page"` → `slack-page`
  (`repeat_interval=1h`, `<!here>` mention); `severity="watchdog"` →
  `slack-watchdog` (`repeat_interval=1d`). `group_wait=30s`. **All three receivers
  post to `#gardenio-alerts`.**
- vmalert reachable at `http://gardenio-vmalert:8880/vmalert`; the always-firing
  `Watchdog` rule (`vector(1)`) is `firing` and routing to `slack-watchdog`.

> **Agent RBAC note.** The scoped agent SA
> (`system:serviceaccount:gardenio-system:gardenio-agent`) is RBAC-**denied**
> `create pods` / `create namespaces` / `get pods|secrets|configmaps` in
> `gardenio-system`. So the ConfigMap-edit and "unschedulable pod in a scratch ns"
> approaches from the original acceptance text are **not** available to an agent.
> The verification below uses the in-cluster **network path** only (Alertmanager
> `/api/v2` + Prometheus metrics), which needs no k8s write.

## Part 1 — synthetic-alert delivery (test-rule proof)

Instead of committing a temporary always-firing rule and cutting a release (the
`Watchdog` rule already *is* a landed always-firing rule — see
[Dead-man's-switch](#dead-mans-switch)), POST a **self-expiring** synthetic alert
straight to Alertmanager. It exercises the identical route → receiver → Slack
webhook path a real rule would, auto-clears after `endsAt`, and needs no cleanup
or config change.

```sh
# From any gardenio-system pod. NOW / END bound a 5-minute self-expiring alert.
NOW=$(date -u +%Y-%m-%dT%H:%M:%S.000Z)
END=$(date -u -d '+5 minutes' +%Y-%m-%dT%H:%M:%S.000Z)

# severity=warn -> slack-warn receiver
curl -s -XPOST http://gardenio-alertmanager:9093/api/v2/alerts \
  -H 'Content-Type: application/json' -d '[{
    "labels":{"alertname":"PROVESyntheticTest","severity":"warn","namespace":"gardenio-system"},
    "annotations":{
      "summary":"PROVE synthetic test alert (warn)",
      "description":"Self-expiring synthetic alert to prove the AM->Slack path.",
      "runbook_url":"https://github.com/mattbakerconsulting/gardenio/blob/main/gardenio/docs/alerting-runbook.md",
      "bead":"https://gardenio.example.com/beads/ka-0000004137"},
    "startsAt":"'"$NOW"'","endsAt":"'"$END"'"}]'

# severity=page -> slack-page receiver (the <!here> paging path)
curl -s -XPOST http://gardenio-alertmanager:9093/api/v2/alerts \
  -H 'Content-Type: application/json' -d '[{
    "labels":{"alertname":"PROVESyntheticPage","severity":"page","namespace":"gardenio-system"},
    "annotations":{
      "summary":"PROVE synthetic test alert (page)",
      "runbook_url":"https://github.com/mattbakerconsulting/gardenio/blob/main/gardenio/docs/alerting-runbook.md#agentpodspendingtoolong",
      "bead":"https://gardenio.example.com/beads/ka-0000004137"},
    "startsAt":"'"$NOW"'","endsAt":"'"$END"'"}]'

# Confirm both registered and routed correctly:
curl -s http://gardenio-alertmanager:9093/api/v2/alerts \
  | python3 -c 'import sys,json;[print(a["labels"]["alertname"],a["labels"]["severity"],"->",[r["name"] for r in a["receivers"]]) for a in json.load(sys.stdin)]'
# => PROVESyntheticTest warn -> ['slack-warn']
#    PROVESyntheticPage  page -> ['slack-page']
```

**Delivery proof (the load-bearing check).** The Slack incoming-webhook posts as a
separate app identity that the `gb slack` bot cannot read, so confirm delivery via
Alertmanager's own notification counters — a stronger check than eyeballing,
because the *success* counter must climb by exactly the number of alerts POSTed
**and** every *failure* bucket must stay flat:

```sh
curl -s http://gardenio-alertmanager:9093/metrics \
  | grep -E '^alertmanager_notifications(_failed)?_total\{integration="slack"'
```

Observed at 2026-07-09 04:12–04:13 UTC: `alertmanager_notifications_total{slack}`
climbed **3 → 5** (baseline was 3 `Watchdog` heartbeats; +1 for the warn route, +1
for the page route), while **every**
`alertmanager_notifications_failed_total{integration="slack",reason=…}` bucket
(`clientError`, `serverError`, `contextCanceled`, `contextDeadlineExceeded`,
`other`) stayed at **0**. A non-empty webhook that errored would have incremented
`serverError`/`clientError`; a stale/empty ESO secret would have made the send
fail — so a clean +2 with zero failures is positive proof that (a) both routes
deliver to `#gardenio-alerts`, and (b) the `gardenio/prod/alertmanager-slack-webhook`
ExternalSecret synced and is non-empty. When the alerts hit `endsAt`, both
receivers (`send_resolved: true` on `slack-warn`/`slack-page`) fired the resolved
notification, taking the counter to **7** — proving the full firing → resolved
lifecycle. No cleanup: the synthetic alerts self-expire.

## Part 2 — fire drill (paging path an unschedulable pod would use)

**The literal drill in the original acceptance text — "create an unschedulable pod
→ confirm the pending-pods alert fires" — is not runnable on prod today, for two
independent reasons, and was intentionally substituted:**

1. **The pending-pods alert is a [blocked alert](#agentpodspendingtoolong).**
   `AgentPodsPendingTooLong` (GAP-1) is shipped **disabled** because it needs a
   `gardenio_agent_pods_pending` controller gauge that does not exist in vmsingle
   yet (the in-chart vmagent scrapes only annotated `gardenio-system` pods — no
   KSM/cadvisor). An unschedulable pod would therefore fire **nothing**.
2. **Agent RBAC forbids pod/namespace creation** (see the RBAC note above).

The drill's real purpose is to prove the **paging route** fires and reaches Slack.
That is exactly what the `severity=page` synthetic alert in Part 1
(`PROVESyntheticPage → slack-page`) exercises end-to-end: it took the `slack-page`
receiver (the `<!here>` paging template), delivered to `#gardenio-alerts`, and
incremented the success counter with zero failures. So the paging delivery path a
future `AgentPodsPendingTooLong` will ride is **proven live today**; only the
rule's *source series* is still pending (tracked by GAP-1's `prereqBead`).

**To run the true unschedulable-pod drill once GAP-1 is unblocked** (operator or a
cluster-write identity), create an impossible resource request in a scratch ns and
confirm the gauge climbs → the alert transitions to `firing` after its 10m
`for:` → routes to `slack-page`:

```sh
kubectl create ns gardenio-firedrill
kubectl -n gardenio-firedrill run unschedulable --image=pause:3.9 \
  --overrides='{"spec":{"containers":[{"name":"c","image":"pause:3.9","resources":{"requests":{"cpu":"1000","memory":"1000Gi"}}}]}}'
# wait >10m, then:
curl -s http://gardenio-vmalert:8880/vmalert/api/v1/alerts \
  | python3 -c 'import sys,json;[print(a["name"],a["state"]) for a in json.load(sys.stdin)["data"]["alerts"]]'
# expect: AgentPodsPendingTooLong firing
kubectl delete ns gardenio-firedrill   # cleanup
```

## Genuine rule-path liveness (bonus, always-on)

Part 1 proves the AM→Slack hop via an injected alert; the always-firing `Watchdog`
rule proves the **full vmalert→AM→Slack** hop continuously with no injection. It is
`firing` (`source` = the vmalert pod's `/vmalert/alert?...` URL, i.e. a genuine
rule evaluation, not an API POST) and routes to `slack-watchdog` every 24h. Its
three baseline heartbeats are what the counter measured up from. Confirm any time:

```sh
curl -s http://gardenio-vmalert:8880/vmalert/api/v1/rules \
  | python3 -c 'import sys,json;[print(r["name"],r["state"]) for g in json.load(sys.stdin)["data"]["groups"] for r in g["rules"] if r.get("type")=="alerting"]'
```
