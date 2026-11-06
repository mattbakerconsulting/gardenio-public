# EKS public-API intrusion-detection runbook (Phase 1)

Operator runbook for the **eks-api-intrusion** alert group — the detective
compensating control for audit finding **F1** (`ka-0000000578`). Design of
record: `ka-0000003550`. Phase-1 epic: `ka-0000004743`.

> **Why alerts, not a firewall.** The `prod-gardenio-eks` API endpoint is public
> (`endpoint_public_access_cidrs = 0.0.0.0/0`) and stays public — narrowing that
> CIDR froze golden promotion for three tags (RC1, `ka-0000004055`). Auth is
> fully intact (IAM + EKS access-entry SigV4, `endpointPrivateAccess=true`); F1
> is **network-exposure defence-in-depth, not an authz bypass**. Instead of
> blocking reach we **detect abuse of it** in the EKS audit log and page. The
> known-good egress set that feeds these rules is a **soft alerting input**, so a
> missing entry degrades to a benign false-positive alert — never a release
> outage.

## Pipeline (what produces the signal)

```
EKS audit log (/aws/eks/prod-gardenio-eks/cluster, CloudWatch)
  → CloudWatch metric filters (ka-0000005580): count anonymous / 401-403 /
    auth-failure / sensitive-verb events
  → cloudwatch exporter via IRSA (ka-0000002106) → gardenio-vmsingle:8428
  → vmalert evaluates the eks-api-intrusion rules
  → gardenio-alertmanager → Slack
```

Both rule paths render from one shared Helm `define`
(`gardenio.eksApiIntrusion.rules`) so they cannot drift:

- **PrometheusRule CRD** — `templates/eks-api-intrusion-alerts.yaml`. Fires on
  clusters where a prometheus-operator Prometheus (or a VMAlert wired to
  converted VMRules) evaluates PrometheusRules.
- **vmalert rules-ConfigMap key** — `templates/vmalert/configmap.yaml`, key
  `eks-api-intrusion-alerts.yaml`. **This is the path that actually fires on
  canonical-prod** (the VM operator converts the CRD to a VMRule that nothing
  evaluates — `ka-0000004766`). It renders only when a vmalert notifier is wired
  (alerting rules with no notifier crashloop vmalert).

Enable per-env via `eksApiIntrusionAlerts.enabled=true` (off by default). Until
the metric filters + exporter land, the source series are absent, every `expr`
evaluates to an empty vector, and nothing fires — so it is safe to enable ahead
of the pipeline.

## Slack routing to a dedicated security channel (`ka-0000002447`)

Every alert in this pack carries the label `subsystem: intrusion-detection`
(detections #1/#2/#4, the #3 measure-two-ways cross-check, the Phase-2 LogsQL
detections, and the dead-man's-switch pair). Alertmanager's
`alertmanager.securityRoute` matches that label and routes the **whole pack** to
a **dedicated security channel**, isolated from the `#gardenio-alerts` ops
firehose so the security owner sees the F1 detective-control signal without ops
noise.

- **Off by default.** With `securityRoute.enabled=false` the pack routes by
  severity into the shared `slack-page` / `slack-warn` receivers exactly as
  before — enabling the intrusion pack does not silently change delivery.
- **Terminal, first-match route.** The `subsystem="intrusion-detection"` route is
  placed **before** the general `severity="page"` route and does not `continue`,
  so a page-severity intrusion alert (sensitive-verb, dead-man's-switch) lands in
  the security channel — a nested `severity="page"` child sends it to the loud
  `slack-security-page` receiver (with the `@`-mention) while ticket-severity
  detections post quietly to `slack-security-warn` in the same channel.
- **Channel-lock caveat + fail-safe.** A Slack incoming webhook only ever posts
  to the channel it was minted against. To deliver to a *genuinely separate*
  channel, mint a **second** incoming webhook against the security channel and
  point `securityRoute.slack.secretName` at a Secret carrying it (provision with
  `externalSecrets.alertmanagerSecuritySlackWebhook.remoteRef`, or seed the K8s
  Secret out-of-band). If `slack.secretName` is empty the receivers **fall back**
  to the global ops webhook with a best-effort `channel:` override — the alerts
  still deliver (to the ops webhook's locked channel), they are never dropped;
  the isolation is just cosmetic until the dedicated webhook is wired.

**Enable per-env** (an overlay flips):

```yaml
alertmanager:
  enabled: true
  securityRoute:
    enabled: true
    channel: "#gardenio-security"        # the security channel
    slack:
      secretName: alertmanager-security-slack-webhook   # dedicated webhook (channel-locked)
    pageMention: "<!subteam^SSEC>"      # ping the security owner on a page
externalSecrets:
  enabled: true
  alertmanagerSecuritySlackWebhook:
    remoteRef: gardenio/alertmanager-security-slack-webhook   # AWS SM key (operator seeds once)
```

**Verify routing** with `amtool` against the rendered config (no live cluster
needed — this is the routing half of the success criterion; the live metric-filter
fire waits on `ka-0000005580`):

```sh
helm template gardenio helm/gardenio --show-only templates/alertmanager/configmap.yaml \
  --set alertmanager.enabled=true --set alertmanager.securityRoute.enabled=true \
  --set alertmanager.securityRoute.slack.secretName=alertmanager-security-slack-webhook \
  | yq eval '.data."alertmanager.yml"' - > /tmp/am.yml
amtool config routes test --config.file /tmp/am.yml subsystem=intrusion-detection severity=page   # → slack-security-page
amtool config routes test --config.file /tmp/am.yml subsystem=intrusion-detection severity=ticket # → slack-security-warn
amtool config routes test --config.file /tmp/am.yml severity=page                                 # → slack-page (unchanged)
```

The `TestAlertmanager_SecurityRoute_*` Go tests lock this wiring in CI (the
`amtool` routing proof runs when the binary is on PATH, else skips gracefully —
same pattern as the promtool firing proof).

## Source series & thresholds (the metric contract)

Configured under `.Values.eksApiIntrusionAlerts` — override there, never edit the
PromQL. Each series is a monotonic counter incrementing by the count of matching
audit events, so `increase(<counter>[window])` reads as an event count.

| values key (`metrics.*`)     | default series                              | detection |
|------------------------------|---------------------------------------------|-----------|
| `anonymousRequests`          | `eks_audit_anonymous_requests_total`        | #1 |
| `unauthorizedResponses`      | `eks_audit_unauthorized_responses_total`    | #1 |
| `authFailures`               | `eks_audit_auth_failures_total`             | #2 |
| `sensitiveVerbExec`          | `eks_audit_sensitive_verb_exec_total`       | #4a (exec/attach/portforward — **page**) |
| `sensitiveVerbSecrets`       | `eks_audit_sensitive_verb_secrets_total`    | #4b (secrets/CRB — ticket) |
| `auditExternalReach`         | `eks_audit_external_reach_total`            | #3 cross-check (A) |
| `independentExternalReach`   | `eks_independent_external_reach_total`      | #3 cross-check (B) |
| `ingestionHeartbeat`         | `eks_audit_events_total`                    | dead-man's-switch  |
| `infraResourceDeletions`     | `eks_audit_infra_resource_deletions_total`  | #7 |

Thresholds (`thresholds.*`, over `window` = 5m by default) start conservative
and are tuned to zero false positives against a 7-day baseline in Phase 3
(`ka-0000002508`): `anonymousRequests: 0`, `unauthorizedResponses: 20`,
`authFailures: 10`, `sensitiveVerbExec: 0`, `sensitiveVerbSecrets: 0` (chart
defaults; prod overlay arms exec at `30` / page and secrets at a volume-anomaly
threshold / ticket — Phase-3 Stage-2 split `ka-0000008111`), `infraResourceDeletions: 0`.

The `infraResourceDeletions` row is **detection #7** (`ka-0000001964`): a count of
out-of-band `delete`/`deletecollection` requests against a high-value routing
resource (Traefik `Middleware`/`IngressRoute`/`TLSOption`/…, core
`Ingress`/`IngressClass`), minus the stable deploy/GC principals in
`var.eks_audit_infra_deletion_excluded_principals`. Unlike the high-volume
sensitive-verb counter, these resources are deleted ~never in steady state, so the
threshold is `0` (any out-of-band delete pages) and needs no source-side tuning —
see the `EKSApiInfraResourceDeletion` alert section below.

The `auditExternalReach` / `independentExternalReach` rows are the
**measure-two-ways cross-check** (advice `ka-0000005924`): two
*independently-derived* counts of authenticated reach from outside the known-good
egress set — **(A)** from the audit-log pipeline, **(B)** from a different AWS
source (CloudTrail `eks get-token` and/or VPC Flow Logs). Their divergence, not
their level, is what alerts. Config lives under `eksApiIntrusionAlerts.crossCheck`
(`window: 1h`, `for: 30m`, `tolerance: 0.25`, `minReachEvents: 4`).

> **Honest status — derivation B now has a producer, off by default (`ka-0000004765`,
> honesty parent `ka-0000004700`).**
> Derivation **(A)** is produced by the shipped chart (the `ExternalReach`
> metric filter → exporter → vmagent rename), always-on once Phase-1 is enabled.
> Derivation **(B)**, `eks_independent_external_reach_total`, is now produced by
> the **CloudTrail** trail + `ExternalReachIndependent` metric filter
> (`composition/canonical-prod/eks_independent_reach_cloudtrail.tf`), scraped via
> the off-by-default derivation-B exporter entry
> (`cloudwatchExporter.independentExternalReach`) and renamed onto the B counter.
> **CloudTrail, not VPC Flow Logs**, because F1 is the *public* endpoint: it is
> fronted by AWS-managed infra outside this VPC, so VPC Flow Logs on customer ENIs
> cannot observe public-endpoint reach at all — CloudTrail records every
> authenticated AWS-principal EKS/STS call from its real source IP, the actual F1
> surface, via a wholly separate log pipeline.
>
> Both halves — the CloudTrail terraform and the chart seam — are **off by default**
> and land together at an operator-gated derivation-B go-live (a ROOT-targeted
> `terraform apply` with a cost sign-off, plus the
> `eks-api-intrusion-independent-reach-values.yaml` overlay). Until then B is
> absent, `sum(increase(eks_independent_external_reach_total[1h]))` is an empty
> vector, and `EKSApiCrossCheckDivergence` *fails safe* (silent). So on a
> Phase-1-only env the cross-check is still measure-*one*-way — do **not** read its
> silence as corroboration of detection #3 until the derivation-B go-live lands.
> A and B measure the same quantity (external authenticated control-plane reach)
> through structurally independent pipelines but are **not** numerically identical
> populations (kube-apiserver request volume vs. AWS-API/STS call volume), so the
> cross-check stays a generously-toleranced **TICKET** whose job is to catch a
> gross pipeline fault (one derivation drops to zero), not to assert count
> equality; `crossCheck.tolerance` and the CloudTrail pattern are tuned to the live
> 7-day baseline in Phase 3 (`ka-0000002508`). The CI guard
> (`eks_api_intrusion_crosscheck_honesty_test.go`) locks this true state: B absent
> from base/Phase-1, present only once the derivation-B overlay is layered.

The `ingestionHeartbeat` row is the **dead-man's-switch** pulse
(`EKSApiAuditIngestionStalled` below): a monotonic count of *all* audit records the
pipeline carries — not a detection — so a stale or flat heartbeat means ingestion
stopped and the whole control has gone blind. It is deliberately distinct from the
sparse intrusion counters, which are legitimately 0 on a healthy quiet cluster.

## Deploying the exporter (the transport, `ka-0000002106`)

The five audit-log-derived contract series are landed in `gardenio-vmsingle` by an
in-chart **cloudwatch-exporter** component (`cloudwatchExporter.*`, off by
default) plus a **vmagent rename job** (`vmagent.cloudwatchExporterScrape.*`).
The exporter (`prometheus/cloudwatch_exporter`, IRSA) polls the CloudWatch
`GetMetricStatistics` / `ListMetrics` API for the metric-filter series
(`ka-0000005580`) and serves them on `:9106`; vmagent scrapes that endpoint and
renames each raw gauge onto the contract counter name.

**Enable per-env** (an overlay flips all of):

```yaml
cloudwatchExporter:
  enabled: true
  irsaRoleArn: arn:aws:iam::123456789012:role/gardenio-prod-cloudwatch-exporter  # cloudwatch_exporter_irsa.tf
  region: us-east-1                 # region the EKS-audit metric filters publish into
  metricNamespace: GardenioSecurity/EKSAudit   # must match the metric-filter terraform
vmagent:
  enabled: true
  cloudwatchExporterScrape:
    enabled: true                   # the scrape + rename job
```

The IRSA role (least-privilege: `cloudwatch:GetMetricStatistics` +
`cloudwatch:ListMetrics` only) is created by
`terraform/composition/canonical-prod/cloudwatch_exporter_irsa.tf`; pin its
output ARN into `cloudwatchExporter.irsaRoleArn`. The chart **fails render** if
`irsaRoleArn` or `region` is unset while enabled (an empty ARN silently disables
IRSA → every CloudWatch call `AccessDenied`s). Add the exporter image to
`.rwx/mirror.yml` + re-dispatch `gardenio-mirror-images` **before** the release
that enables it (an unmirrored tag `ImagePullBackOff`s at deploy).

**The gauge→counter reconciliation** (owned jointly with the metric-filter bead):
`cloudwatch_exporter` serves a per-period **`_sum` gauge**, but the alert exprs
read `increase(<name>_total[window])` — a **monotonic counter**. The metric
filter must publish a cumulative count (or the exporter/rename layer must present
one) so `increase()` is well-defined. Until `ka-0000005580` lands the filters,
the exporter serves no `GardenioSecurity/EKSAudit` series (`cloudwatch_exporter_scrape_error`
reads `1`), every `expr` is an empty vector, and nothing fires — inert-but-ready.

**Verify vmsingle is scraping it** on a deploy target:

```sh
# exporter up + no scrape error
kubectl exec -n gardenio-system gardenio-vmsingle-0 -- \
  wget -qO- 'http://127.0.0.1:8428/api/v1/query?query=up{job="cloudwatch-exporter"}'
kubectl exec -n gardenio-system gardenio-vmsingle-0 -- \
  wget -qO- 'http://127.0.0.1:8428/api/v1/query?query=cloudwatch_exporter_scrape_error'
# contract series present (once the filters publish data)
kubectl exec -n gardenio-system gardenio-vmsingle-0 -- \
  wget -qO- 'http://127.0.0.1:8428/api/v1/query?query=eks_audit_anonymous_requests_total'
```

---

## Alert: `EKSApiAnonymousReach` — detection #1 (ticket)

**Fires when** over the trailing window either any anonymous request
(`user.username == "system:anonymous"`) is recorded, or a 401/403 burst above
`unauthorizedResponses` arrives from the public endpoint.

**Triage**
1. Confirm the signal is real, not a pipeline blip: open the **EKS API Intrusion
   Overview** metrics board (uid `gardenio-eks-api-intrusion-overview` — see
   [Metrics-plane overview](#metrics-plane-overview-eks-api-intrusion-overview-dashboard))
   and read the detection-event-rate panel, instead of hand-typing
   `max(max_over_time(eks_audit_anonymous_requests_total[5m]))` into
   `gardenio-vmsingle` (the `eks_audit_*` series are non-monotonic windowed-Sum
   gauges, so `max_over_time` is the correct reducer, not `increase()` —
   ka-0000005736). The
   board's **Alert state** row also shows which detections are firing right now.
2. Identify the source IPs. In Phase 1 the metric filter is coarse (counts only);
   use the Phase-2 forensic dashboard (**EKS API Audit Forensics**, uid
   `gardenio-eks-api-forensics` — see [Forensic drill-down](#forensic-drill-down-eks-api-audit-forensics-dashboard),
   task `ka-0000007948`) or a direct CloudWatch Logs Insights query on
   `/aws/eks/prod-gardenio-eks/cluster` for a `sourceIPs` breakdown.
3. Cross-check those IPs against the known-good egress set (`ka-0000002105`).

**Response**
- IPs are internet scanners hitting an anonymous/unauth path with no success →
  expected background noise for a public endpoint; if chronic, raise
  `thresholds.unauthorizedResponses` for this env and note it in Phase-3 tuning.
- A *successful* authenticated call from an unknown IP is a different, higher-value
  signal — that is detection #3 (Phase 2, `ka-0000006485`); escalate as a possible
  stolen credential.

---

## Alert: `EKSApiAuthFailureSpike` — detection #2 (ticket)

**Fires when** the EKS authenticator records more than `authFailures`
AccessDenied / SigV4 failures over the window.

**Triage**
1. Break the failures down by `sourceIPs` and `user.username` (Logs Insights /
   Phase-2 LogsQL). A single novel origin driving the spike is credential-probing
   or stolen-key testing; diffuse failures across many IPs is internet noise.
2. Correlate with CloudTrail `eks get-token` / access-entry events (measure two
   ways, advice `ka-0000005924`) — a persistent divergence between the audit-log
   view and CloudTrail is a **pipeline bug**, not an intrusion.

**Response**
- Confirmed probing of a specific principal → rotate/scope the targeted
  credential; consider a temporary access-entry tightening for that principal.
- Baseline drift (a noisy CI identity retrying) → tune `thresholds.authFailures`
  and record in Phase-3 tuning.

---

## Alert: `EKSApiSensitiveVerbExec` — detection #4a (**page**)

Detection #4 is **split** into two series (Phase-3 Stage-2, `ka-0000008111`): the
exec branch (this alert, **page**) and the secrets branch (`EKSApiSensitiveVerbSecrets`,
ticket, below). The split exists because the two populations are structurally
different — exec/attach/portforward is small and clean (24h max 27, pages at real
fidelity), while secrets/CRB is a high-volume floor dominated by a non-enumerable
ephemeral-namespace class that only a volume-anomaly threshold can handle.

**Fires when** the audit log records more than `sensitiveVerbExec` pod
`exec` / `attach` / `portforward` requests over the window (no principal
exclusion — every principal counts). This pages — it is the crown-jewel signal;
a stolen cluster-admin key opening an exec can never be silenced here.

**Triage**
1. Identify the **principal** (`user.username`), the **verb + objectRef**, the
   **sourceIP**, and the **userAgent** from the audit record.
2. The canonical bad case: the `ci_deploy` cluster-admin identity used **outside a
   deploy job**. Confirm whether a legitimate deploy / operator action was in
   flight at that timestamp (GitLab pipeline, operator session).

**Response**
- No legitimate action maps to it → treat as an active credential compromise:
  rotate the implicated credential immediately, review what it touched
  (`objectRef` history), and open an incident.
- Legitimate but noisy (a routine operator `exec`) → raise `thresholds.sensitiveVerbExec`
  in the env overlay against the observed baseline, and document the exception in
  Phase-3 tuning so the page stays high-signal. The exec metric filter carries **no**
  principal exclusion by design (crown-jewel invariant); do not add one.

---

## Alert: `EKSApiSensitiveVerbSecrets` — detection #4b (ticket)

**Fires when** the audit log records more than `sensitiveVerbSecrets` `secrets`
`get`/`list` or `clusterrolebindings` `create`/`patch`/`update` requests over the
window, **after** subtracting the measured benign controller/deploy principals
(`eks_audit_sensitive_verb_excluded_principals`). This **tickets**, not pages: the
dimensionless counter cannot separate a stolen key reading secrets from the
ephemeral `mr-<slug>:setup-jobs` noise floor by verb alone. The page-worthy
secrets-exfil discriminator is the **source IP** — detection #3 (Phase-2 LogsQL
`EKSApiFromUnknownIP`), not this counter.

**Triage**
1. Confirm it is a genuine volume anomaly vs the baseline (the ephemeral-namespace
   floor keeps this series high on a healthy fleet — see the overview dashboard's
   secrets baseline panel).
2. Pivot to the **source IP** and the Logs-Insights secrets breakdown to confirm
   whether an unexpected principal/IP is behind the spike.

**Response**
- A departure from the volume baseline with an unexpected principal/IP → escalate
  as a potential secrets-exfil and cross-check detection #3.
- Routine (a new wave of ephemeral MR namespaces) → re-baseline and, if the floor
  shifted durably, raise `thresholds.sensitiveVerbSecrets` in the overlay. Re-baseline
  ~24-48h before tightening (`ka-0000008111` AC-4).

---

## Alert: `EKSApiInfraResourceDeletion` — detection #7 (**page**)

**Fires when** the audit log records more than `infraResourceDeletions` (default
`0`) `delete` / `deletecollection` requests over the window against a **high-value
routing resource** — a Traefik `Middleware` / `IngressRoute` / `TLSOption` /
`MiddlewareTCP` / `IngressRouteTCP` / `ServersTransport`, or a core `Ingress` /
`IngressClass` — **after** subtracting the stable deploy/GC principals in
`var.eks_audit_infra_deletion_excluded_principals`. These resources are deleted
~never in steady state, so any counted delete pages.

**Why this exists.** In May 2026 a chart-owned Traefik `Middleware`
(`gardenio-beads-ratelimit`) was deleted **out of band** (bug `ka-0000001917`). A
dangling `middlewareRef` makes Traefik **silently drop just that router** (→ 404,
no error, no k8s event), so `/v1/beads` routing broke until a human noticed. The
monorepo and `acme-helm-chart` contain **no** delete path and the cluster runs
**no** GitOps pruner, so the delete was a one-off manual action — nothing detected
it. This is the durable detective control for any recurrence (`ka-0000001964`).

**Triage**
1. Open the **`…07-infra-resource-deletion`** saved Logs-Insights query (or run it
   over the audit log group) to get the **principal** (`user.username`), the
   **objectRef** (namespace + name of the deleted resource), the **sourceIP**, and
   the **userAgent** of the delete.
2. Interactive `kubectl delete …` from a human/gardener IP is the signal; a
   `helm upgrade` reconcile or a k8s garbage-collection delete from the deploy/GC
   identity during a release is benign — confirm the principal + source IP + the
   deploy context (was a release/pipeline in flight at that timestamp?).

**Response**
- **Restore routing first.** Reconcile the deleted resource back with a
  `helm upgrade` — the chart re-creates a chart-owned Middleware/IngressRoute via
  three-way merge. Confirm the affected route serves 200 again (e.g. `/v1/beads`).
- No legitimate action maps to it → treat as an out-of-band mutation of the edge:
  identify how the principal obtained delete on the routing CRDs, rotate/scope the
  implicated credential, and open an incident.
- Legitimate but recurring (a deploy/GC identity that legitimately deletes one of
  these) → add its **exact** `system:serviceaccount:<ns>:<sa>` (or IAM ARN) to
  `eks_audit_infra_deletion_excluded_principals` in `prod-real.tfvars` (never a
  wildcard — CloudWatch filter `!=` is a full-value match) so the page stays
  high-signal.

---

# Phase 2 — the LogsQL detections (`#3` / `#5` / `#6`)

The log-native detections that are impractical as CloudWatch metric filters.
They run as **vmalert `type: vlogs` rules** evaluated against **VictoriaLogs**
(LogsQL `stats` queries), and fire through the **same**
vmalert → `gardenio-alertmanager` → Slack path as
`#1`/`#2`/`#4`.

## Why a second evaluator (`vmalert-logs`)

vmalert's `-datasource.url` is a **single global flag**. The shared
`gardenio-vmalert` evaluates PromQL against `vmsingle:8428`; the LogsQL rules must
be evaluated against the VictoriaLogs stats API
(`/select/logsql/stats_query` on `:9428`). Retargeting the shared evaluator
would break every PromQL rule, so the chart stands up a dedicated
**`gardenio-vmalert-logs`** Deployment pointed at VictoriaLogs
(`-rule.defaultRuleType=vlogs`, alerting-only — no `-remoteWrite.url`) that
notifies the same Alertmanager.

```
EKS audit log (CloudWatch) ──(subscription filter → Firehose/Vector, IRSA)──►
  VictoriaLogs (vlsingle, :9428)
    → vmalert-logs evaluates the type:vlogs detections (#3/#5/#6)
    → gardenio-alertmanager → Slack
```

Both `vmalertLogs.enabled` and `eksApiIntrusionLogs.enabled` default **off**.
Until VictoriaLogs (`ka-0000007261`) and the CloudWatch→VL transport
(`ka-0000007889`) land, the datasource answers nothing, every rule evaluates to
an empty result, and nothing fires — **inert-but-ready**, safe to enable ahead
of the pipeline.

## The log-field contract

The VictoriaLogs field names the audit records land under are
**transport-dependent** (Vector vs Firehose flatten the audit JSON
differently — decision `ka-0000000935`), so they are configurable under
`.Values.eksApiIntrusionLogs.fields.*` — the LogsQL analog of the `#1`/`#2`/`#4`
metric contract. Align them to whatever the transport publishes; **never edit
the LogsQL**.

| values key (`fields.*`) | default field                | used by |
|-------------------------|------------------------------|---------|
| `sourceIP`              | `sourceIPs`                  | #3, #6  |
| `user`                  | `user.username`              | #3, #5  |
| `responseCode`          | `responseStatus.code`        | #3      |
| `verb`                  | `verb`                       | #6      |
| `asn`                   | `sourceASN` (enriched)       | #5      |

`sourceASN` is an **enriched** field the transport must add (the raw audit
record has no ASN); until enrichment lands, `#5` matches nothing and is inert.

---

## Metrics-plane overview: EKS API Intrusion Overview dashboard

Before you drill into a single principal/IP in the LogsQL forensics board, the
**EKS API Intrusion Overview** Grafana dashboard (uid
`gardenio-eks-api-intrusion-overview`, source
`gardenio/helm/gardenio/dashboards/eks-api-intrusion-overview.json`, task
`ka-0000000526`) is the metrics-plane read: it plots the six `eks_audit_*`
contract counters in `gardenio-vmsingle` and the live firing state of the six
`eks-api-intrusion` alert rules. It answers *"is the signal real, and which
detections are firing?"* without hand-typing PromQL.

**How to use it**

1. Open Grafana → the **EKS API Intrusion Overview** board (or deep-link its uid).
2. **Alert state** row — which `eks-api-intrusion` rules are firing / pending /
   inactive right now, with severity, read from the `ALERTS` series `vmalert`
   remote-writes into `vmsingle` (`ALERTS{subsystem="intrusion-detection"}`). A
   firing `page`-severity row (`#4a` sensitive-verb exec, or a dead-man's-switch) is a
   crown-jewel event.
3. **Detection signal** row — `increase(<counter>[5m])` per detection: anonymous
   reach (`#1`), unauthorized 401/403 (`#1`), auth failures (`#2`), sensitive
   verbs (`#4`). This is the "confirm the signal is real" panel step 1 of every
   triage sends you to.
4. **Sensitive-verb baseline** row — the empirical `#4` event-rate distribution.
   This is the evidence the operator reads to choose the go-live arming fork on
   escalation `ka-0000005309` (A downgrade `#4` page→ticket / B re-scope pattern /
   C hold for a 7-day baseline / D arm as-is): `#4` at threshold 0 + severity
   `page` fires on routine fleet traffic, so this panel makes the always-firing-
   on-noise problem visible.
5. **Measure-two-ways** row — external-reach derivation A (audit log) vs B
   (CloudTrail/VPC-Flow) and their divergence fraction; a sustained gap is the
   `EKSApiCrossCheckDivergence` pipeline-health ticket, not an intrusion (advice
   `ka-0000005924`).
6. **Pipeline liveness** row — exporter `up{}`, the ingestion heartbeat rate, and
   its timeline: is the detective control actually *watching*, or blind
   (`EKSApiAuditIngestionStalled` / `EKSApiAuditExporterDown`)? Do NOT read quiet
   detections as "all clear" while the heartbeat is flat.

**Metric-name alignment.** The board references each contract counter through a
hidden `constant` template variable (`$m_anon` / `$m_unauth` / `$m_authfail` /
`$m_sensitive` / `$m_extreach_a` / `$m_extreach_b` / `$m_heartbeat`), whose
defaults match `eksApiIntrusionAlerts.metrics.*`. If a per-env exporter publishes
different series names, edit those dashboard variables — **never the panel
queries** — exactly as you would align `.Values.eksApiIntrusionAlerts.metrics.*`
for the rules.

**Enable it.** The board binds the always-provisioned `VictoriaMetrics`
datasource (uid `victoriametrics`), so unlike the forensics board it needs no
extra datasource toggle. The `eks_audit_*` series it plots appear once the
metric pipeline is enabled per-env (`cloudwatchExporter.enabled=true` **and**
`vmagent.cloudwatchExporterScrape.enabled=true` — the operator go-live on
`ka-0000005309`). Until then every panel reads empty — **inert-but-ready**, same
as the detections; the dashboard JSON itself always rides the grafana-bridge
image (provisioned as a `type=dashboard` bead). It touches no
`endpoint_public_access_cidrs`, no thresholds, and no arming seam — a read-only
observability surface that cannot repeat the RC1 outage.

---

## Forensic drill-down: EKS API Audit Forensics dashboard

When one of these detections pages, you need to *investigate* — not just read the
alert labels. The **EKS API Audit Forensics** Grafana dashboard (uid
`gardenio-eks-api-forensics`, source `gardenio/helm/gardenio/dashboards/eks-api-forensics.json`,
task `ka-0000007948`) is the on-demand drill-down surface over the same EKS audit
log in VictoriaLogs the detections evaluate. Open it, scope it, and read the
story behind the alert.

**How to use it**

1. Open Grafana → the **EKS API Audit Forensics** board (or deep-link its uid).
2. Expand **Is this board fed?** first. It reports two *unscoped* counts — audit
   records, and rows of any kind — so an empty board tells you *why* it is empty
   before you read anything into it (bug `ka-0000005037`):

   | audit records | all rows | means |
   |---|---|---|
   | 0 | 0 | VictoriaLogs is empty/unreachable or the datasource is misconfigured — fix that first |
   | 0 | >0 | the store is healthy but the CloudWatch→VictoriaLogs **audit transport has not landed** (`eksApiIntrusionLogs`, plan `ka-0000003550`) — the panels below cannot show anything, and their emptiness is **not** evidence of "no intrusion" |
   | >0 | >0 | the board is fed; an empty panel below is telling you about your **scope**, not about the pipeline |

3. Set the **Principal** and **Source IP** textbox variables at the top from the
   firing alert's `principal` / `source_ip` labels, and widen the **time range**
   to bracket the alert.

   Both boxes are matched as **anchored regexps against the whole field value**,
   so paste an identity **verbatim** — colons and `@` included
   (`system:serviceaccount:kube-system:coredns`, `matthew.baker@acme.example`) —
   and it selects that principal and nothing else. The default `.+` means "any
   non-empty value". For a family of identities write a regexp
   (`system:serviceaccount:kube-system:.*`); for a subnet, `10\.0\.1\..*`.

   Do **not** type a bare `*`. LogsQL's `field:value` is a **word** filter, not an
   exact-value filter: measured against prod VictoriaLogs 2026-09-02,
   `principal:acme.example` returned **14,791** rows (every `*@acme.example`
   identity) where `principal:~"^acme.example$"` correctly returned **0**, and an
   `@`-bearing principal in the bare form **HTTP 422**s the whole board. That is
   why the board interpolates these variables as `~"^…$"` (bug
   `ka-0000005037`) — but a hand-written LogsQL query in the sections below still
   needs the quoting done by hand.

4. Read the drill-downs:
   - **Headline stats** — total events, successful (1xx–3xx) calls, distinct
     source IPs (>1 for one principal is the impossible-travel signal), distinct
     principals.
   - **Source-IP timeline** — request volume per source IP over the window; a new
     IP appearing mid-window for an established principal is the visual for a
     stolen credential. Plus a success-vs-failure timeline (a wall of failures
     then a success is the probe-then-hit pattern, `#2`→`#3`).
   - **Principal history** — every `(principal, source_ip)` pair with counts,
     successes, and last-seen; sort by successes to surface a working credential
     from an unexpected IP.
   - **Verb breakdown** — every verb attempted + a verb-mix donut; watch for
     recon (`list`/`watch`, `#6`) and sensitive verbs (`exec`/`attach`/
     `portforward`, `secrets get/list`, `*clusterrolebinding`, `#4`).
   - **Unknown-IP & enumeration views** — successful calls by source IP (the `#3`
     analog: which IP a working credential succeeded from → check it against the
     known-good egress set) and request-rate by source IP (the `#6` analog).
   - **Raw audit records** (collapsed row) — the underlying audit lines newest
     first: read the `userAgent`, `objectRef`, and `responseStatus` of a
     suspicious call directly.

**Field-name alignment.** The dashboard references the audit fields through hidden
`constant` template variables (`$f_sourceip` / `$f_user` / `$f_code` / `$f_verb`),
whose defaults match `eksApiIntrusionLogs.fields.*` (the same log-field contract
the detections use). If a per-env transport flattens the audit JSON to different
field names, edit those dashboard variables — **never the panel queries** — exactly
as you would align `.Values.eksApiIntrusionLogs.fields.*` for the rules.

**Enable it** with `grafana.victoriaLogsDatasource.enabled=true` alongside
`vlsingle.enabled=true` (per-env, same posture as `vmalertLogs` /
`eksApiIntrusionLogs`). That provisions the VictoriaLogs Grafana datasource (uid
`victorialogs`) the board binds to, seeded from the pinned, commercial-signed
`victoriametrics-logs-datasource` plugin via a copy-only initContainer — so PROD
keeps `grafana.plugins.allowCatalogEgress:false` and its zero-off-cluster-egress
posture (`ka-0000007541`). The dashboard JSON itself always rides the
grafana-bridge image (provisioned as a `type=dashboard` bead), so it is present
regardless; this toggle wires the datasource it queries. Until the CloudWatch→VL
transport (`ka-0000007889`) populates the store, every panel returns empty —
**inert-but-ready**, same as the detections.

---

## Alert: `EKSApiFromUnknownIP` — detection `#3` (**★ page**)

**Fires when** an authenticated **successful** (1xx/2xx/3xx) request reaches the
public API from a `sourceIP` **outside** the known-good egress set over the
window. This is the **detective analog of the CIDR allowlist** and the single
highest-value rule — it catches a *working* stolen credential. The firing alert
carries the offending `principal` + `source_ip` as labels. The success range
starts at 1xx so a `pods/exec` (a 1xx WebSocket-upgrade response, audit code
101) by a stolen cluster-admin key is counted — the red-team `#3` oracle's 101
exec fixture is a MUST-page (`ka-0000006556`).

**LogsQL shape** (rendered from values):
```
_time:<window> responseStatus.code:range(100, 399)
  -user.username:in(<excludeUsers>)          # anonymous/unauth excluded by default
  -sourceIPs:ipv4_range("<cidr>") …          # one per knownGoodCIDRs entry
  | rename user.username as principal, sourceIPs as source_ip
  | stats by (principal, source_ip) count() as hits
  | filter hits:><thresholds.unknownSourceIP>
```

**Fail-safe:** the known-good set (`eksApiIntrusionLogs.knownGoodCIDRs`) is a
**soft alerting input**, not a firewall. With an **empty** set every
authenticated success alerts (fail-safe toward detection); a missing legitimate
egress only raises a **benign** alert — never a locked-out runner (the RC1
failure mode this whole plan sidesteps).

**Triage**
1. Read the `principal` + `source_ip` off the alert labels.
2. Does that principal legitimately operate from that IP (RWX/NAT, operator VPN,
   agent NAT, node/pod CIDRs — `ka-0000002105`)? Cross-check CloudTrail
   `eks get-token` origins (measure two ways, `ka-0000005924`).

**Response**
- Not a legitimate egress → active credential compromise: rotate the implicated
  credential immediately and open an incident.
- A new *legitimate* egress not yet allow-listed → add the CIDR to
  `eksApiIntrusionLogs.knownGoodCIDRs` (per-env overlay) and note it in Phase-3
  tuning.

---

## Alert: `EKSApiImpossibleTravel` — detection `#5` (ticket)

**Fires when** one authenticated `principal` is observed from **more than
`thresholds.distinctAsns`** source ASNs within the window. Simultaneous far-apart
origins are physically impossible for one identity; a new ASN for an established
principal is the stolen-key-from-elsewhere signature.

```
_time:<window> sourceASN:* -user.username:in(<excludeUsers>)
  | rename user.username as principal
  | stats by (principal) count_uniq(sourceASN) as distinct_asns
  | filter distinct_asns:><thresholds.distinctAsns>
```

**Triage / response** — cross-check against CloudTrail `eks get-token` origins:
a persistent audit-vs-CloudTrail divergence is a **pipeline/enrichment bug**, not
an intrusion. A confirmed novel ASN for a real principal → rotate that
credential.

---

## Alert: `EKSApiPerIpEnumeration` — detection `#6` (ticket)

**Fires when** a single `source_ip` issues **more than `thresholds.perIpRequests`**
`list`/`watch` requests over the window — request volume far above a normal
client is API reconnaissance / resource scraping.

```
_time:<window> verb:in(<enumerationVerbs>)
  | rename sourceIPs as source_ip
  | stats by (source_ip) count() as requests
  | filter requests:><thresholds.perIpRequests>
```

**Triage / response** — cross-check the `source_ip` against the known-good set: a
busy CI identity may need `thresholds.perIpRequests` raised (Phase-3 tuning); an
unknown IP scraping `list`/`watch` is reconnaissance — correlate with `#3`.

---

## Alert: `EKSApiCrossCheckDivergence` — detection #3 cross-check (ticket, **never pages**)

The **measure-two-ways** guard (advice `ka-0000005924`, plan step
`ka-0000002458`). Detection #3 — *authenticated reach from outside the known-good
egress set* — is *designed to be* counted **two independent ways**, and this
alert fires when the two counts stop reconstructing each other.

- **(A)** `eks_audit_external_reach_total` — from the EKS **audit log**, the same
  metric-filter → exporter → `vmsingle` pipeline as the other counters. **Produced
  by the shipped chart** (always-on once Phase-1 is enabled).
- **(B)** `eks_independent_external_reach_total` — from a **different AWS source**:
  the **CloudTrail** trail + `ExternalReachIndependent` metric filter
  (`composition/canonical-prod/eks_independent_reach_cloudtrail.tf`, `ka-0000004765`),
  scraped via the off-by-default `cloudwatchExporter.independentExternalReach`
  exporter entry and renamed onto this counter. CloudTrail (not VPC Flow Logs)
  because the *public* endpoint is AWS-managed infra outside this VPC, invisible to
  customer-VPC flow logs. **Off by default** — both the CloudTrail terraform and
  the chart seam land together at an operator-gated derivation-B go-live (root-
  targeted `terraform apply` + the `eks-api-intrusion-independent-reach-values.yaml`
  overlay). Until that go-live the cross-check is measure-*one*-way and this alert
  is silent by construction (see the fail-safe note below). A and B measure the
  same quantity through independent pipelines but are not numerically identical
  populations, so the tolerance stays generous and Phase-3-tuned (`ka-0000002508`).

**Fires when** over the trailing `crossCheck.window` (1h) the two counts differ by
more than `crossCheck.tolerance` (25%) of their mean **and** their combined volume
clears `crossCheck.minReachEvents` (4) — held for `crossCheck.for` (30m) so only a
*sustained* gap fires. It is a **ticket and MUST NOT page**: a divergence is a
**pipeline bug**, not an intrusion.

> **Why this is not a security alert.** If A and B agree, both pipelines are
> measuring a real thing and detection #3's numbers can be trusted to page. If
> they persistently diverge, exactly one signal is lying — a metric-filter
> miscount, a dropped subscription-filter batch, a stale exporter. Paging a human
> on a broken measurement is the exact antipattern the advice guards against;
> instead we ticket so the pipeline is fixed *before* its numbers are trusted.

**Triage**
1. Open the two series in Grafana over the divergence window and confirm **which
   one moved** — a step in A with B flat (or vice-versa) localizes the fault to
   one pipeline.
2. Reconcile a **sample of source IPs** across the two sources: pull recent
   authenticated audit-log requests from outside the known-good set, and the
   CloudTrail `eks get-token` events for the same principals/window. A row present
   in one derivation and absent in the other is the miscount.
3. Confirm neither pipeline is *stale* (the dead-man's-switch, `ka-0000003782`,
   covers a fully-stopped pipeline; this alert covers a *miscounting* one).

**Response**
- **A over-counts / B under-counts** → inspect the audit-log metric filter
  (pattern too broad, double-counting retries) or the exporter (label drop,
  scrape gap). Fix and confirm the two series re-converge.
- **B over-counts / A under-counts** → inspect the CloudTrail/VPC-Flow derivation
  (a `get-token` is not always a successful API call; Flow-Log sampling). Adjust
  the derivation so it measures the same event A does.
- **Genuinely different populations** (the two sources legitimately see different
  slices) → widen `crossCheck.tolerance` for this env and document it in Phase-3
  tuning (`ka-0000002508`) — but only after confirming the gap is structural, not
  a bug. **Never** raise its severity to `page`.
- Fail-safe: if B has **not landed yet** the expr is an empty vector and this
  alert is silent by construction — that is expected until the CloudTrail/VPC-Flow
  pipeline is deployed; it is not a suppressed divergence.

---

## Alert: `EKSApiCrossCheckHalfDark` — cross-check half-dark visibility (ticket, **never pages**)

The companion that makes the divergence rule's fail-safe silence **visible**
(bug `ka-0000001321`, advice `ka-0000005924`). `EKSApiCrossCheckDivergence` above
fails safe while derivation **B** (`eks_independent_external_reach_total`) is
absent — its expr is an empty vector and it is silent by construction. Correct as
a *divergence* guard, but it means detection #3 is running **one-way** and nothing
says so: a silent detector is indistinguishable from a dead one. The divergence
comment punts this state to the dead-man's-switch (`ka-0000003782`), but that pair
watches the **A-side** heartbeat only and never references the B series, so the
hand-off lands nowhere. This alert closes that gap.

**Fires when** over the trailing `crossCheck.halfDark.window` (15m) derivation B
is **absent** (`absent_over_time`) **and** the A-side reach series is **healthy**
(`max(max_over_time(eks_audit_external_reach_total[15m])) > 0`), held for
`crossCheck.halfDark.for` (1h). It is a **ticket and MUST NOT page**: a
not-yet-landed second derivation is a **build-out gap**, not an intrusion or an
outage.

> **Positive anchor (advice `ka-0000005170`).** The `absent()` half is a negation;
> it is paired with a **positive existence check** of the A-side series so the rule
> can never annex unrelated streams *and* can never fire on a total-pipeline outage
> — if the whole metric-filter → exporter pipeline goes dark, A is absent too, the
> anchor's `max()` is an empty vector, the `and` is empty, and this stays silent. A
> fully-stopped pipeline is the dead-man's-switch's job; this alert is specifically
> the **B-half dark while A is alive** state.

**Self-clearing.** The moment derivation B lands and starts publishing,
`absent_over_time(B)` is empty and this alert clears itself — so it doubles as the
**go-live proof** that the cross-check is now genuinely two-way. If B lands and
later *stalls*, it re-fires, covering a B-side outage the fail-safe divergence expr
can never see.

**Response**
- **Close it by landing B** (recommended, makes measure-two-ways real): set
  `eks_independent_reach_cloudtrail_enabled = true` in
  `composition/canonical-prod/prod-real.tfvars`, layer the
  `eks-api-intrusion-independent-reach-values.yaml` overlay, and run the
  operator-gated root-targeted `terraform apply`. Verify with:
  ```bash
  VM=http://gardenio-vmsingle.gardenio-system:8428
  curl -s "$VM/prometheus/api/v1/query" \
    --data-urlencode 'query=max_over_time(max(eks_independent_external_reach_total)[7d:5m])'
  # non-empty result => derivation B is live and the cross-check is two-way
  ```
- **Accept it as a standing ticket** tracking that measure-two-ways is currently
  one-way (until B is scheduled), or disable via
  `eksApiIntrusionAlerts.crossCheck.halfDark.enabled=false` /
  `severity.crossCheckHalfDark: none` for an env that will never land B. **Never**
  raise its severity to `page`.

---

## Alert: `EKSApiAuditIngestionStalled` — dead-man's-switch (**page**)

The detective control is only as trustworthy as its ingestion. If the
metric-filter → `cloudwatch-exporter` → `gardenio-vmsingle` pipeline silently stops,
detections #1/#2/#4 and the #3 cross-check all go quiet — and that quiet is
**blind, not safe**. This alert (`ka-0000003782`) makes a stopped pipeline page
itself, mirroring the gap-alerts `VMAlertNotEvaluating` self-liveness check.

**Fires when** the ingestion heartbeat
(`metrics.ingestionHeartbeat`, default `eks_audit_events_total`) has, over
`deadMansSwitch.stalenessWindow` (15m), either **no samples at all**
(`absent_over_time` — the series is gone: exporter down, IRSA revoked, metric
filter deleted, audit logging detached) **or a flat count** (`increase == 0` — the
series still publishes, e.g. a metric filter with `defaultValue=0`, while the
upstream audit feed is dead). A live cluster never has a whole window with zero
audit events, so either condition means ingestion stopped. Held `for:` 10m to ride
out a rolling exporter restart.

> **Why the heartbeat, not the detections?** The sparse intrusion counters
> (anonymous, sensitive-verb exec) are legitimately `0` on a healthy quiet cluster, so
> keying a liveness check off them would false-positive on peace. The heartbeat is
> a total audit-event count that climbs continuously while the fleet + CI use the
> API — the correct liveness pulse.

**Triage**
1. Confirm the heartbeat actually went stale in Grafana / `gardenio-vmsingle`
   (`increase(eks_audit_events_total[15m])`) — distinguish "series stopped" from
   "series flat".
2. Check the exporter pod
   (`kubectl -n gardenio-system get pods -l app.kubernetes.io/component=cloudwatch-exporter`)
   and its IRSA / CloudWatch reachability. `EKSApiAuditExporterDown` (below) fires
   alongside if the exporter target itself is `up == 0`.
3. Confirm the audit metric filters still exist on
   `/aws/eks/prod-gardenio-eks/cluster` and that EKS audit logging is still ON.

**Response**
- **Do not** close out quiet detections as "all clear" while this fires — the
  control is blind until the heartbeat resumes.
- Restore the broken link (redeploy/repair the exporter, re-grant IRSA, re-create a
  deleted metric filter) and confirm the heartbeat resumes incrementing.
- Fail-safe: before the exporter first publishes the heartbeat, `absent_over_time`
  is true by construction — the alert only pages once `eksApiIntrusionAlerts.enabled`
  is set for the env (i.e. deliberately, after the pipeline lands), so it is
  inert-but-ready with the rest of the pack, never a bootstrap false page.

## Alert: `EKSApiAuditExporterDown` — dead-man's-switch, exporter self-scrape (**page**)

The direct `VMAlertDown` analog and the second dead-man's-switch layer.

**Fires when** `up{component="cloudwatch-exporter"} == 0` for
`deadMansSwitch.exporterDown.for` (5m) — the exporter scrape target is reachable
but reporting unhealthy. Catches a degraded-not-gone exporter earlier and more
specifically than the end-to-end stall above (which must wait a full
`stalenessWindow`). A fully **gone** target (no `up{}` series at all) is caught by
`EKSApiAuditIngestionStalled`'s absent clause instead — the two layers compose over
down / degraded / gone.

**Triage / response** — `kubectl -n gardenio-system get pods -l
app.kubernetes.io/component=cloudwatch-exporter`; check its logs and IRSA/CloudWatch
reachability; restart/repair. Gated on `deadMansSwitch.exporterDown.enabled` +
`.component`: a deployment that does not self-scrape the exporter (no `up{}` series
for it) turns this off and leans on the end-to-end freshness net, rather than
shipping a rule that can never fire. Align `.component` with the exporter chart's
`app.kubernetes.io/component` (`ka-0000006679`).

---

## Silence / tuning

- Downgrade or silence per env with `eksApiIntrusionAlerts.severity.*` (Phase 1)
  or `eksApiIntrusionLogs.severity.*` (Phase 2 LogsQL) — `page` → `ticket` →
  `none` in the deploy-values overlay — do **not** edit the template.
- Phase-2 LogsQL tuning is values-only too: the detective allowlist
  (`eksApiIntrusionLogs.knownGoodCIDRs`), the log-field contract
  (`eksApiIntrusionLogs.fields.*`), and thresholds
  (`eksApiIntrusionLogs.thresholds.*`) — never the LogsQL.
- All threshold + window tuning is Phase 3 (`ka-0000000495`): tune against a
  7-day baseline of normal fleet + CI traffic to reach zero false positives. The
  dead-man's-switch (`ka-0000003782`, `EKSApiAuditIngestionStalled` +
  `EKSApiAuditExporterDown`) ships on the shared rule define so a silent ingestion
  pipeline pages itself; tune `deadMansSwitch.stalenessWindow` / `.for` against the
  same baseline and align `.exporterDown.component` with the exporter when it lands.
- **The Phase-3 tuning method-of-record is
  [`eks-api-intrusion-threshold-tuning.md`](eks-api-intrusion-threshold-tuning.md)**
  (`ka-0000002508`): it records how each threshold was tuned, the live-cluster
  evidence, and the exact PromQL re-derivation procedure for the volume
  thresholds once the pipeline emits the counters. Note detection #4's
  false-positive firehose is **structural**: it is fixed by *splitting* the
  metric filter (Phase-3 Stage-2, `ka-0000008111`) into `sensitiveVerbExec`
  (`exec`/`attach`/`portforward`, no exclusion — always pages at real fidelity)
  and `sensitiveVerbSecrets` (benign controller principals excluded via
  `eks_audit_sensitive_verb_excluded_principals`, tickets at a volume-anomaly
  threshold because the dominant residual is a non-enumerable ephemeral-namespace
  class) — **not** by raising a single combined threshold.

## Related

- **Phase-2 LogsQL detections `#3`/`#5`/`#6`** are documented above; rules in
  `templates/vmalert-logs/configmap.yaml`, evaluator in
  `templates/vmalert-logs/`. Task `ka-0000006485`, epic `ka-0000007666`. The
  `EKSApiCrossCheckDivergence` alert above is detection #3's **measure-two-ways**
  guard — it corroborates that same reach signal against CloudTrail/VPC-Flow
  before the #3 numbers are trusted to page (`ka-0000002458`, advice
  `ka-0000005924`).
- VictoriaLogs deploy (`ka-0000007261`) + CloudWatch→VL transport
  (`ka-0000007889`) are the Phase-2 prerequisites the LogsQL rules wait on.
- **Forensic drill-down** — the **EKS API Audit Forensics** dashboard (uid
  `gardenio-eks-api-forensics`, `dashboards/eks-api-forensics.json`) is the
  on-demand investigation surface over the same audit log; see
  [Forensic drill-down](#forensic-drill-down-eks-api-audit-forensics-dashboard)
  above. Task `ka-0000007948`.
- Red-team validation of detection `#3` (a synthetic non-allowlisted `kubectl`
  call must page) is proven offline today and live-fires once Phase 2 lands:
  [`eks-api-intrusion-redteam-runbook.md`](./eks-api-intrusion-redteam-runbook.md).
- Known-good egress reference set: `ka-0000002105`.
- IAM log-read prerequisite for Phase 2 transport: `ka-0000006138`.
- Closing F1 as remediated-by-detection: `ka-0000006438`.
