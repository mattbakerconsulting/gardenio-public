# EKS public-API intrusion-detection runbook (IDP)

The operator runbook for the **intrusion-detection program (IDP)** that remediates
audit finding **F1** — *EKS API public endpoint open to `0.0.0.0/0` on
`prod-gardenio-eks`* (`ka-0000000578`, audit report `ka-0000003522`). It is the
alert → triage → response reference for every IDP detection, the incident
procedures behind them, and the worked scenarios an on-call responder walks when
one fires.

- **Design of record:** `ka-0000003550` (bundle `ka-0000007676`).
- **Plan:** `ka-0000007128` (`/plans`) — Phase 0 foundations → Phase 1 metrics-first
  detective control → Phase 2 VictoriaLogs plane → Phase 3 harden & validate.
- **Phase-1 alert-group card** (metric contract, PromQL, values keys):
  [`eks-api-intrusion-runbook.md`](./archive/eks-api-intrusion-runbook.md).
- **Rules:** `gardenio/helm/gardenio/templates/eks-api-intrusion-alerts.yaml`
  (shared `gardenio.eksApiIntrusion.rules` define; also rendered into the vmalert
  rules ConfigMap — that ConfigMap path is the one that actually fires on
  canonical-prod).

This document is the **umbrella**: it covers all six detections (Phase-1 metric
alerts *and* the Phase-2 LogsQL detections), the cross-cutting response
procedures, and the example scenarios. The alert-group card above is the terse
per-alert reference for the three Phase-1 alerts and their metric contract; read
it when you need the exact PromQL, values keys, or threshold defaults. Read this
one when an alert has fired and you need to decide what it means and what to do.

---

## 1. Why detection, not a firewall (read this first)

The `prod-gardenio-eks` API endpoint is public
(`endpoint_public_access_cidrs = ["0.0.0.0/0"]`) **and it stays public.** F1's
first-instinct fix — narrow the CIDR — is barred: an out-of-band narrowing to two
`/32`s once locked CI out of the EKS API and **froze golden promotion for three
consecutive tags** (RC1, `ka-0000004055` / `ka-0000002512`). deploy-prod runs
`terraform apply` on RWX cloud runners with **dynamic egress IPs**, so a static
allowlist cannot reliably admit CI.

So F1 is remediated by a **detective compensating control** rather than a
preventive one. The EKS control plane already emits a full audit trail; the IDP
routes that trail into the VictoriaMetrics observability plane and fires
Slack-delivered alerts on intrusion signals. The public endpoint stays reachable,
but any anomalous or unauthorized use of it becomes a paged alert.

**The property that makes this safe:** the known-good egress set that feeds the
detections is a **soft alerting input**, not a terraform-enforced firewall. A
wrong or missing entry degrades to a **false-positive alert** (annoying) — never a
locked-out runner (an outage). This sidesteps the entire RC1 failure mode while
still closing the CIS EKS 5.4.1 detection gap.

**Severity framing.** Auth is fully intact: every request is IAM + EKS
access-entry (SigV4) gated, and `endpointPrivateAccess=true`. F1 is
**network-exposure defense-in-depth, not an authz bypass** — effective severity
Medium. The residual risks a public endpoint carries are exactly the two the IDP
detects:

- **(a) an EKS API CVE** in the window before AWS patches the managed control
  plane → shows in the audit log as anomalous verbs / paths / user-agents.
- **(b) a leaked CI or access-entry credential** usable *from anywhere* rather
  than only an allowlisted egress → shows as a known principal appearing from an
  unknown source IP.

The IDP converts "we can't prevent reach" into "we *know* the moment reach is
abused."

---

## 2. Signal sources (what the detections read)

| Source | State today | Carries |
|---|---|---|
| EKS **audit** log | ON → CloudWatch `/aws/eks/prod-gardenio-eks/cluster`, 30d | every API request: `user.username`, `sourceIPs`, `verb`, `objectRef` (resource/ns/name), `responseStatus.code`, `userAgent`, `requestReceivedTimestamp` |
| EKS **authenticator** log | ON → same group | IAM→K8s identity mapping + auth-denied (SigV4 / access-entry) events |
| CloudTrail | account default | `UpdateClusterConfig`, `eks get-token`, access-entry mutations — the control-plane-of-record, used as the independent second derivation |
| VPC Flow Logs | (verify per env) | L3/L4 to the API ENIs — independent source-IP corroboration |

All of this lives in CloudWatch. The alerting plane (`vmalert` →
`gardenio-alertmanager` → Slack) reads from
**VictoriaMetrics** (`gardenio-vmsingle:8428`); **VictoriaLogs is not yet
deployed.** Bridging that gap is the load-bearing work of the plan:

```text
                    Phase 1 (fast detective control, reuse everything)
  CloudWatch audit log group ──(metric filters, ka-0000005580)──► CloudWatch metrics
        │                                                 │
        │                                    cloudwatch exporter / YACE via IRSA (ka-0000002106)
        │                                                 ▼
        │                                    gardenio-vmsingle:8428 ──► vmalert
        │                                                                 │
        │                          eks-api-intrusion rules (chart) ──► gardenio-alertmanager
        │                                                                 │
        │                                                                 └──► Slack (#sec)
        │
        │             Phase 2 (rich detection + forensics)
        └──(subscription filter → Firehose/Vector, IRSA logs:GetLogEvents)──► VictoriaLogs (vlsingle, NEW)
                                                                                    │
                                                        LogsQL detections + vmalert (vlogs) ──► same Slack path
```

> **Deployment state.** Phase 1 rules ship in the chart but are **off by default**
> (`eksApiIntrusionAlerts.enabled=false`) and the source metric filters +
> exporter are not yet deployed, so every `expr` currently evaluates to an empty
> vector and **nothing fires**. The rules are inert-but-ready: safe to enable
> ahead of the pipeline. Phase-2 (LogsQL) detections #3/#5/#6 are authored once
> VictoriaLogs and the log transport land. This runbook documents the full IDP so
> it is complete the day each detection goes live; a detection marked *(planned)*
> is not yet alerting.

---

## 3. The known-good egress reference set

Detections #3 and #4 test whether a source IP is a **known-good egress**. That set
is the single piece of config the IDP maintains (`ka-0000002105`), and it is the
*same* data the CIDR-narrowing path needed — only here it is a soft alerting input,
so it can start incomplete and improve each iteration with zero outage risk.

Members (verify current values against `prod-real.tfvars` and the RWX/VPN configs
when triaging):

- **RWX egress** — per-account static egress IPs if requested from RWX (they exist
  "upon request"), else the RWX NAT ranges. This is the CI/deploy path.
- **Operator VPN egress** — the office / operator VPN NAT.
- **Agent NAT egress** — the `prod-real.tfvars` NAT box the agent fleet egresses
  through.
- **Cluster node / pod CIDRs** — the cluster's own nodes and pods talking to the
  API in-VPC (private endpoint path).

When a detection reports a source IP, the first triage question is always: **is it
in the known-good set?** A hit that is *in* the set is almost always benign
(re-tune, don't page); a hit *outside* the set on a successful authenticated call
is the high-value signal.

---

## 4. Detection catalog — triage → response

Each detection below gives: what it is, the alert (name + whether it pages), how to
**triage** (confirm real, identify who/where), and how to **respond**. Detections
#1/#2/#4 are the live Phase-1 metric alerts; #3/#5/#6 are Phase-2 LogsQL and marked
*(planned)*.

The three Phase-1 alerts share one metric contract (counters emitted by the
CloudWatch metric filters, surfaced into `vmsingle` by the exporter). Thresholds
are conservative starting points, tuned to zero false positives against a 7-day
baseline in Phase 3 (`ka-0000002508`); override via
`.Values.eksApiIntrusionAlerts.thresholds.*` per env, **never** by editing PromQL.

### Detection #1 — Anonymous / unauthenticated reach  ·  alert `EKSApiAnonymousReach`  ·  **ticket**

**Signal.** Over the trailing window, either any request logged as
`user.username == "system:anonymous"`, or a burst of 401/403 responses above
`thresholds.unauthorizedResponses` (default 20) from the public endpoint. A public
endpoint invites internet scanning; this is the highest-volume early-warning
signal.

**Triage**
1. Confirm the signal is real, not a pipeline blip: in Grafana / `gardenio-vmsingle`
   check the series actually moved —
   `increase(eks_audit_anonymous_requests_total[5m])` and
   `increase(eks_audit_unauthorized_responses_total[5m])`.
2. Get the source-IP breakdown. Phase-1 metric filters are **counts only**; use the
   Phase-2 LogsQL forensic surface (`ka-0000007948`) or a direct CloudWatch Logs
   Insights query on `/aws/eks/prod-gardenio-eks/cluster` (see §7) grouped by
   `sourceIPs` and `responseStatus.code`.
3. Cross-check those IPs against the known-good egress set (§3).

**Response**
- **Internet scanners** hitting anonymous/unauth paths with **no success** →
  expected background noise for a public endpoint. If chronic, raise
  `thresholds.unauthorizedResponses` for this env and note it in Phase-3 tuning.
- A **successful authenticated** call from an unknown IP is a *different, higher*
  signal — that is detection #3. Escalate as a possible stolen credential (§5).

### Detection #2 — Auth-failure spike from a novel source  ·  alert `EKSApiAuthFailureSpike`  ·  **ticket**

**Signal.** The EKS authenticator records more than `thresholds.authFailures`
(default 10) AccessDenied / SigV4 failures over the window. Above baseline this is
credential-probing or stolen-key testing against the SigV4 + access-entry gate.

**Triage**
1. Confirm the series moved: `increase(eks_audit_auth_failures_total[5m])`.
2. Break the failures down by `sourceIPs` and `user.username` (Logs Insights /
   Phase-2 LogsQL). **A single novel origin** driving the spike is
   credential-probing or stolen-key testing; **diffuse failures across many IPs**
   is internet noise.
3. **Measure two ways** (advice `ka-0000005924`): correlate against CloudTrail
   `eks get-token` / access-entry events. A persistent divergence between the
   audit-log view and CloudTrail is a **pipeline bug**, not an intrusion — fix the
   pipeline, don't chase a phantom.

**Response**
- **Confirmed probing of a specific principal** → rotate / scope the targeted
  credential; consider a temporary access-entry tightening for that principal (§5).
- **Baseline drift** (a noisy CI identity retrying) → tune
  `thresholds.authFailures` and record it in Phase-3 tuning.

### Detection #3 — Successful call from an UNKNOWN source IP  ·  *(planned, Phase 2, `ka-0000006485`)*  ·  **page**

**Signal (★ the single most valuable rule).** An authenticated, `2xx`/`3xx`
request whose `sourceIP ∉ known-good egress set` (§3). This is the detective analog
of the allowlist: it catches a **working stolen credential** exercising the public
endpoint from an attacker's location. Fails safe — a missing known-good entry only
raises a benign alert. Impractical as a metric filter (needs the full source-IP
set), so it is authored in LogsQL once the VictoriaLogs plane is up; a coarse
metric-filter approximation is possible earlier.

**Triage**
1. Identify the **principal** (`user.username`), the **verb + objectRef**, the
   **sourceIP**, and the **userAgent** from the audit record.
2. Confirm the sourceIP truly is outside the known-good set (§3) — the set is the
   soft input here, so first rule out a *missing legitimate egress* (a new RWX NAT,
   a rotated VPN IP). If the IP is a legitimate-but-unlisted egress, add it to the
   set and downgrade.
3. Cross-check CloudTrail for a matching `eks get-token` from that IP/principal
   (measure two ways).

**Response**
- **Genuine unknown IP with a real principal + successful call** → treat as an
  active credential compromise. **Rotate the implicated credential immediately**,
  review what it touched (`objectRef` history), and open an incident (§5).
- **Legitimate unlisted egress** → add it to the known-good set (`ka-0000002105`),
  document, and move on. No CIDR change — the set is alerting-only.

### Detection #4a — Sensitive verb exec  ·  alert `EKSApiSensitiveVerbExec`  ·  **page** (crown-jewel)

> Detection #4 is **split** into two series (Phase-3 Stage-2, `ka-0000008111`):
> the exec branch (this, **page**) and the secrets branch (`EKSApiSensitiveVerbSecrets`,
> ticket, below). exec/attach/portforward is small and clean (24h max 27), so it
> pages at real fidelity; secrets/CRB is a high-volume floor dominated by a
> non-enumerable ephemeral-namespace class, so it only tickets on a volume anomaly.

**Signal.** More than `thresholds.sensitiveVerbExec` sensitive pod `exec` /
`attach` / `portforward` requests over the window (no principal exclusion — every
principal counts). These are the crown-jewel operations. The canonical bad case is
the **`ci_deploy` cluster-admin identity used outside a deploy job**.

**Triage**
1. Confirm the series moved: `increase(eks_audit_sensitive_verb_exec_total[5m])`.
2. Identify the **principal** (`user.username`), the **verb + objectRef** (which
   resource / namespace / name), the **sourceIP**, and the **userAgent**.
3. Establish context: was a legitimate deploy or operator action in flight at that
   timestamp? Check the GitLab pipeline history (a real deploy-prod job), operator
   session logs, and whether the sourceIP is known-good (§3).

**Response**
- **No legitimate action maps to it** → treat as active credential compromise:
  rotate the implicated credential immediately, review `objectRef` history for what
  it touched, and open an incident (§5). If it is `ci_deploy` outside a deploy job,
  this is the stolen-key signature — page and escalate hard.
- **Legitimate but noisy** (a routine operator `exec`) → raise `thresholds.sensitiveVerbExec`
  in the env overlay against the observed baseline, and document the exception in
  Phase-3 tuning. The exec filter carries **no** principal exclusion by design.

### Detection #4b — Sensitive secrets/RBAC access  ·  alert `EKSApiSensitiveVerbSecrets`  ·  ticket

**Signal.** More than `thresholds.sensitiveVerbSecrets` `secrets` `get`/`list` or
`clusterrolebindings` `create`/`patch`/`update` requests over the window, **after**
subtracting the measured benign controller/deploy principals. Tickets, not pages:
the counter cannot separate a stolen key reading secrets from the ephemeral
`mr-<slug>:setup-jobs` noise floor by verb alone — the page-worthy discriminator
is the source IP (detection #3).

**Triage**
1. Confirm the series moved: `increase(eks_audit_sensitive_verb_secrets_total[5m])`,
   and whether it is a genuine anomaly vs the (high) baseline floor.
2. Pivot to the **source IP** and the Logs-Insights secrets breakdown; cross-check
   detection #3 for an unexpected principal/IP.

**Response**
- **Unexpected principal/IP behind a volume spike** → escalate as potential
  secrets-exfil and cross-check detection #3.
- **Routine** (a new wave of ephemeral MR namespaces) → re-baseline and, if the
  floor shifted durably, raise `thresholds.sensitiveVerbSecrets`. Re-baseline
  ~24-48h before tightening.

### Detection #5 — New-ASN / impossible-travel for a known principal  ·  *(planned, Phase 2)*  ·  **ticket**

**Signal.** A known IAM principal suddenly appearing from a new geo / ASN, or two
far-apart source IPs for the same principal inside a short window (physically
impossible travel). Catches a credential used simultaneously by its legitimate
owner and a thief.

**Triage**
1. Pull the principal's recent source-IP history (LogsQL over the audit log).
2. Determine whether the "new" location is a legitimate change (operator
   travelling, a new NAT) or genuinely anomalous. Cross-check CloudTrail geo.

**Response**
- **Anomalous** → rotate the credential and open an incident (§5).
- **Legitimate relocation** → add the new egress to the known-good set and
  document.

### Detection #6 — Per-IP request-rate enumeration  ·  *(planned, Phase 2)*  ·  **ticket**

**Signal.** Request volume from one `sourceIP` far above its baseline — the
signature of list/watch scraping (an attacker enumerating resources after a
foothold).

**Triage**
1. Identify the sourceIP and principal, and the resources being enumerated
   (`objectRef`, `verb ∈ {list, watch}`).
2. Determine whether it is a legitimate high-volume client (a controller, a backup
   job) or an unexpected principal scraping.

**Response**
- **Unexpected principal enumerating** → treat as post-compromise reconnaissance:
  rotate the credential, scope its access-entry, and open an incident (§5).
- **Legitimate high-volume client** → baseline it into the recording rule / raise
  the per-IP threshold and document.

---

## 5. Incident response procedures

The detections tell you *something happened*; these procedures are *what to do*.
Two of the six detections **page** (#3, #4) — those are the ones that assume active
compromise until proven otherwise. The rest **ticket** — investigate within the
business day.

### 5.1 Severity & routing

| | Detections | Default severity | SLA |
|---|---|---|---|
| **Page** | #3 successful-unknown-IP, #4a sensitive-verb exec | `page` | Respond now — assume active compromise |
| **Ticket** | #1 anonymous-reach, #2 auth-failure-spike, #4b sensitive-verb secrets, #5 new-ASN, #6 rate-enum | `ticket` | Same business day |

Routing is via the Alertmanager `severity` label; downgrade per env with
`eksApiIntrusionAlerts.severity.*` (`page` → `ticket` → `none`) in the deploy-values
overlay — **never** edit the template. Alerts deliver to Slack through the proven
`vmalert → gardenio-alertmanager → Slack` chain; Slack routing to the
security channel is wired in `ka-0000002447`.

### 5.2 First 15 minutes on a page (#3 / #4)

1. **Grab the five facts** from the audit record: `user.username` (principal),
   `verb` + `objectRef` (what), `sourceIPs` (where from), `userAgent` (with what),
   `requestReceivedTimestamp` (when). See §7 for the Logs Insights query.
2. **Is the sourceIP known-good?** (§3.) If yes and the action is expected, this is
   likely a tuning miss — downgrade, note for Phase-3 tuning, no rotation.
3. **Does a legitimate action map to it?** Check GitLab pipelines (a real
   deploy-prod job), operator sessions, scheduled jobs at that timestamp.
4. **If unexplained → assume compromise.** Proceed to containment (§5.3).

### 5.3 Containment — credential rotation

The residual risk F1 carries is a leaked credential usable from anywhere. If a
detection is unexplained, the fastest containment is to **invalidate the
credential**, which does not touch the network path (RC1-safe):

1. **Identify the principal → its credential source.** IAM principal → is it an
   RWX/CI key, an access-entry, an agent IRSA identity, an operator token?
2. **Rotate / revoke it.**
   - IAM access key → deactivate + rotate the key.
   - EKS access-entry → remove or scope the entry
     (`aws eks delete-access-entry` / `update-access-entry`) for the implicated
     principal. This is an access-entry change, **not** an
     `endpoint_public_access_cidrs` change — no golden-promotion risk.
   - Agent IRSA / cluster credential → rotate per the relevant credential runbook.
3. **Confirm the credential is dead:** re-run the failing detection's query; the
   principal should stop appearing.

> **Never** reach for `aws eks update-cluster-config` to narrow
> `endpoint_public_access_cidrs` as an incident response. That is the RC1 outage
> path and it is barred (§1). Contain at the *credential / access-entry* layer,
> which fails safe.

### 5.4 Investigation — blast radius

Once contained, reconstruct what the credential touched:

1. **`objectRef` history** for the principal over the compromise window (LogsQL /
   Logs Insights): every resource it read, created, patched, or deleted.
2. **Secrets exposure:** if `secrets get`/`list` appears, treat every secret in the
   touched namespaces as potentially exfiltrated → rotate them.
3. **Privilege changes:** if `clusterrolebinding` create/patch or access-entry
   mutation appears, audit the resulting grants and reverse any illegitimate ones.
4. **Persistence:** check for created workloads, service accounts, or webhooks the
   attacker may have planted.

### 5.5 Measure two ways (before you page a human)

Every source-IP / principal claim from the audit-log path must reconcile with an
independent derivation (advice `ka-0000005924`, plan step `ka-0000002458`):

- audit-log source IPs ↔ **CloudTrail** `eks get-token` events, and/or
- audit-log source IPs ↔ **VPC Flow Logs** to the API ENIs.

If the two derivations **agree**, the detection is trustworthy. If they
**persistently diverge**, the bug is in the pipeline (a metric filter miscount, a
dropped subscription-filter batch), not in the cluster — fix the pipeline first.
This guard keeps a noisy pipeline from generating phantom incidents.

This is now **automated** as the `EKSApiCrossCheckDivergence` alert: the two
reach counts (`eks_audit_external_reach_total` ↔
`eks_independent_external_reach_total`) are compared continuously and a sustained
gap tickets — **never pages** (a divergence is a pipeline bug, not an intrusion).
Per-alert card, PromQL, and the `crossCheck.*` values:
[`eks-api-intrusion-runbook.md`](./archive/eks-api-intrusion-runbook.md#alert-eksapicrosscheckdivergence--detection-3-cross-check-ticket-never-pages).

### 5.6 Pipeline health — the dead-man's-switch

A detective control is blind if its ingestion silently stops — then "detections
have gone quiet" is not peace, it is a dead pipeline nobody can see. The
**dead-man's-switch** (`ka-0000003782`) makes a stopped pipeline itself page,
mirroring the gap-alerts `VMAlertNotEvaluating` + `VMAlertDown` self-liveness pair.
Two composing layers, both on the shared eks-api-intrusion rule define
(`eksApiIntrusionAlerts.deadMansSwitch.*`):

- **`EKSApiAuditIngestionStalled`** (page) — the **end-to-end net**. Watches the
  ingestion *heartbeat* (`metrics.ingestionHeartbeat`, default
  `eks_audit_events_total`) — a monotonic count of *all* audit records the pipeline
  carries, which climbs continuously on a live cluster because the fleet + CI hit
  the API every minute — and fires when it is **absent** (`absent_over_time[15m]`,
  series gone) **or flat** (`increase[15m] == 0`, series present but not moving).
  It keys off the heartbeat, **not** the sparse intrusion counters (#1/#4 are
  legitimately 0 on a healthy quiet cluster; watching those for absence would
  false-positive on peace). It fires no matter which link broke, and even while the
  exporter pod is `up == 1`.
- **`EKSApiAuditExporterDown`** (page) — the direct `VMAlertDown` analog:
  `up{component="cloudwatch-exporter"} == 0`. Catches a degraded-not-gone exporter
  earlier/more specifically. A fully-*gone* target (no `up{}` at all) is caught by
  the stalled alert's absent clause instead — the layers compose over
  down/degraded/gone. Gated on `deadMansSwitch.exporterDown.enabled` so a
  deployment that does not self-scrape the exporter leans on the freshness net
  rather than shipping a rule that can never fire.

Both ride the same evaluation + delivery path as detections #1/#2/#4 (they render
inert-but-ready until the exporter publishes the heartbeat, and only page once
`eksApiIntrusionAlerts.enabled=true` per env). **When either fires, do not read
quiet detections as "all clear."** Verify the pipeline is alive: check the
heartbeat is still incrementing in `vmsingle`
(`increase(eks_audit_events_total[15m])`), that the metric filters exist on
`/aws/eks/prod-gardenio-eks/cluster`, and that the exporter pod is healthy. Per-alert
cards, PromQL, and config knobs:
[`eks-api-intrusion-runbook.md`](./archive/eks-api-intrusion-runbook.md#alert-eksapiauditingestionstalled--dead-mans-switch-page).

#### 5.6.1 Edge log shippers — `EdgeLogShipperStalled` (page)

The apex/edge side has its own dead-man's-switch for the **edge log shippers**
(`edgeShipperLiveness.*`, task `ka-0000006616`, epic `ka-0000000472`). It is the
direct answer to the 39h AVA outage (`ka-0000002843`): `gardenio-ava-log-shipper`
was `Running 1/1` for 39h while delivering **zero** rows (a 100% poll failure
logged every 60s that nobody reads), so a 52%-of-edge-traffic OCSF blind spot
stayed open and **nothing alerted**. Pod liveness is not shipper liveness.

- **`EdgeLogShipperStalled`** (page, `type: vlogs`) — one rule per enabled +
  deployed shipper. Each counts **rows delivered** to VictoriaLogs over
  `edgeShipperLiveness.stalenessWindow` (default `15m`) with a shipper-specific
  LogsQL selector (`entryPointName:*` for traefik, `activity:*` for ava — aligned
  with each `cmd/<shipper>/main.go` `jsonlineURL` `_stream_fields`) and fires when
  the count is at/below `floor` (default `0`). Rows delivered **increments on
  success** (advice `ka-0000002586`), so it reads *differently* on a healthy vs a
  broken shipper — unlike the `1/1 Running` pod status, which reads the same on
  both. Verified live 2026-08-17 against prod-gardenio-eks VictoriaLogs: traefik =
  684 rows/15m (silent), ava = 0 rows/24h (fires) — while both pods were `1/1`.
- The expr uses a **bare `stats count()`** (not `stats by`): VictoriaLogs emits a
  `0`-row from a bare count even when the selector field is entirely absent, so
  the floor fires on a *fully-gone* pipeline too. `stats by` would be silent on
  empty input — the silent no-op trap.
- **No `up{}==0` exporter-down layer.** The shippers dial *out* only (no
  in-cluster listener, not prometheus-scraped, no kube-state-metrics), so no
  `up{}` series exists for them and a mirror of `EKSApiAuditExporterDown` could
  never fire — a silent no-op giving false confidence. The rows-delivered stall
  net is the load-bearing and only valid layer here; it fires no matter which
  link broke (IRSA revoked, poll failing, source group empty/detached, pod gone).

**When it fires:** do not read a quiet shipper as healthy. Confirm rows stopped in
Grafana / VictoriaLogs (`<selector> | stats count()` over the window); check the
shipper pod **logs** (not just its `1/1` status) for a repeating poll/deliver
failure; for ava verify the IRSA role (`gardenio-prod-ava-log-reader`) + the AVA
access-log group still exist (the 39h outage was a missing role + missing group,
`ka-0000002843`); confirm the vlsingle sink is reachable. Off by default; enable
per env (`edgeShipperLiveness.enabled=true`) once the shipper(s) + VictoriaLogs +
`vmalert-logs` + a notifier are live.

---

## 6. Example scenarios

Four worked walkthroughs. Each names the detection, the triage path, and the
response — the shape a real on-call responder follows.

### Scenario A — Internet scanner (benign, the common case)

**What fires.** `EKSApiAnonymousReach` (#1, ticket): a 401/403 burst, ~40 over 5m,
from a spread of source IPs.

**Triage.** Series confirmed real in Grafana. Logs Insights breakdown shows ~30
distinct source IPs across several ASNs, all `system:anonymous` or unauthenticated,
**zero successes** (`responseStatus.code` all 401/403), hitting `/version`,
`/healthz`, and random paths. None are in the known-good set — but none *succeed*.

**Response.** This is background internet scanning of a public endpoint — expected
noise, **not** an incident. No rotation. If this becomes chronic and drowns signal,
raise `thresholds.unauthorizedResponses` for this env and record it in Phase-3
tuning (`ka-0000002508`). Close the ticket.

### Scenario B — Stolen CI key exercised from an attacker IP (the real incident)

**What fires.** `EKSApiSensitiveVerbExec` (#4a, **page**) if the stolen key
`exec`s/`portforward`s; `EKSApiSensitiveVerbSecrets` (#4b, ticket) if it only reads
secrets — and, once Phase 2 is live, `EKSApiFromUnknownIP` (#3, page) alongside
either, which is the page-worthy discriminator for the secrets-read case.

**Triage.** The five facts: principal `ci_deploy` (cluster-admin), verb `get` on
`secrets` in `gardenio-system`, sourceIP `203.0.113.44` — **not** in the known-good
set — userAgent a plain `kubectl`, timestamp 14:02 UTC. Check GitLab: **no
deploy-prod pipeline was running** at 14:02. CloudTrail shows a matching
`eks get-token` from the same unknown IP (measure two ways → agree). This is the
canonical stolen-key signature: the crown-jewel identity used outside a deploy job
from an attacker's location.

**Response.** Assume active compromise.
1. **Contain:** revoke/rotate the `ci_deploy` credential and remove its
   access-entry (`aws eks delete-access-entry` for that principal) — an
   access-entry change, **not** a CIDR change.
2. **Investigate blast radius:** pull `objectRef` history for `ci_deploy` over the
   window; every secret in the touched namespaces is now suspect → rotate them;
   check for planted clusterrolebindings / workloads.
3. **Open an incident**, page the operator, and record the timeline.

The endpoint is never touched — containment is entirely at the credential /
access-entry layer, so release/CI is unaffected.

### Scenario C — Credential-probing spike (targeted, escalate)

**What fires.** `EKSApiAuthFailureSpike` (#2, ticket): ~60 AccessDenied over 5m.

**Triage.** Break down by source and principal: **a single source IP**
(`198.51.100.9`, not known-good) driving nearly all 60 failures, cycling through
several `user.username` values (probing for a valid IAM→K8s mapping). CloudTrail
corroborates the same IP hammering `eks get-token` (measure two ways → agree). This
is not diffuse internet noise — it is a **targeted** attempt to find a working
credential.

**Response.** No credential has succeeded *yet*, so this is pre-compromise. Treat as
elevated: consider a temporary access-entry tightening for any probed principal,
watch for a *transition* to detection #3/#4 (a success from that IP → immediate page
+ rotation per Scenario B), and record the source IP. If it is purely noise from a
misconfigured-but-legit client, tune `thresholds.authFailures` and document.

### Scenario D — Legitimate egress not yet in the known-good set (false positive → fix the set, not the endpoint)

**What fires.** `EKSApiFromUnknownIP` (#3, page) *(Phase 2)*: a successful
authenticated call from an IP outside the known-good set.

**Triage.** Principal is the RWX deploy identity, verb/objectRef match a normal
apply, sourceIP is a **new RWX NAT address** that was rotated in but never added to
the known-good set. GitLab confirms a real deploy-prod pipeline was running at that
timestamp. This is a legitimate-but-unlisted egress, not an intrusion.

**Response.** **Add the new RWX NAT range to the known-good egress set**
(`ka-0000002105`) and downgrade the alert. No rotation, and — critically — **no CIDR
change**: the known-good set is a soft alerting input, so a missing entry only ever
produced a benign false-positive page, exactly the fail-safe property that makes
this control safe (§1). Document the addition so the set improves each iteration.

---

## 7. Forensic queries (source-IP / principal breakdown)

Phase-1 metric filters are counts only. To get the *who/where* behind an alert, use
the Phase-2 LogsQL surface (`ka-0000007948`) once VictoriaLogs is up, or query
CloudWatch Logs Insights directly against `/aws/eks/prod-gardenio-eks/cluster`
(requires the IRSA log-read grant, `ka-0000006138`):

```
# Source-IP + response-code breakdown behind an anonymous/unauth alert (#1)
fields @timestamp, user.username, sourceIPs.0 as srcIP, verb, responseStatus.code, userAgent
| filter responseStatus.code >= 400
| stats count(*) as hits by srcIP, responseStatus.code
| sort hits desc

# Sensitive-verb detail behind a #4 page — who touched what, from where
fields @timestamp, user.username, verb, objectRef.resource, objectRef.namespace, objectRef.name, sourceIPs.0 as srcIP, userAgent
| filter verb in ["exec","attach","portforward","get","list","create","patch"]
  and (objectRef.resource in ["secrets","pods/exec","pods/attach","pods/portforward","clusterrolebindings"])
| sort @timestamp desc

# Auth-failure breakdown by source + principal (#2)
fields @timestamp, user.username, sourceIPs.0 as srcIP, responseStatus.code, annotations."authorization.k8s.io/reason"
| filter responseStatus.code in [401, 403]
| stats count(*) as fails by srcIP, user.username
| sort fails desc
```

Adjust field paths to the real audit record shape once confirmed against a live
event (`ka-0000002311`). Always cross-check the result against CloudTrail /
VPC Flow Logs (§5.5) before escalating.

---

## 8. Tuning & maintenance

- **Thresholds & windows** — Phase 3 (`ka-0000002508`, `ka-0000000495`): tune
  against a 7-day baseline of normal fleet + CI traffic to reach zero false
  positives. Override via `.Values.eksApiIntrusionAlerts.thresholds.*` /
  `.window` / `.for` per env overlay — never edit the PromQL.
- **Severity / silence** — downgrade or silence per env with
  `eksApiIntrusionAlerts.severity.*` (`page` → `ticket` → `none`) in the
  deploy-values overlay.
- **Known-good egress set** — keep `ka-0000002105` current as RWX NAT ranges, VPN
  egress, and NAT boxes change. A stale set produces false positives (never
  outages), so update it whenever Scenario D recurs.
- **Metric contract** — the source series names live in
  `.Values.eksApiIntrusionAlerts.metrics.*` precisely so the exporter and the rules
  can be aligned without editing PromQL. If the exporter publishes different names,
  change them there.
- **Dead-man's-switch** — the ingestion-stopped alerts
  (`EKSApiAuditIngestionStalled` + `EKSApiAuditExporterDown`, `ka-0000003782`) ship
  on the shared rule define; a silent pipeline pages itself so it is never mistaken
  for a quiet one (§5.6). Align `deadMansSwitch.exporterDown.component` with the
  exporter's `app.kubernetes.io/component` (`ka-0000006679`) when it lands, and tune
  `stalenessWindow` against the 7-day baseline in Phase 3.
- **Validation** — the staging red-team test (`ka-0000006556`) drives a synthetic
  `kubectl` call from a non-allowlisted egress and asserts detection #3 pages.
  See the [red-team validation runbook](./archive/eks-api-intrusion-redteam-runbook.md):
  the offline oracle (CI-enforced, available now) proves the predicate, and the
  live-fire staging procedure confirms a real Slack page once the Phase-2 pipeline
  lands. Re-run it after any rule, egress-set, or threshold change.

---

## 9. F1 disposition — remediated-by-detection

F1 (`ka-0000000578`) is closed as **remediated-by-detection**, not accepted-risk:

- The `endpoint_public_access_cidrs = 0.0.0.0/0` posture is **retained
  deliberately** (RC1 bars narrowing it), but the residual risk it carries — a
  CVE or a leaked credential exercising the public endpoint — is now a **paged,
  Slack-delivered detective control** with documented triage and response (this
  runbook).
- This is the compensating control CIS EKS 5.4.1 and the audit finding ask for:
  where we cannot *prevent* reach, we *detect* abuse of it.
- The control **fails safe** — the RC1 outage mode is structurally impossible here
  because no network path is touched; a wrong known-good entry yields a
  false-positive alert, never a locked-out runner.

**Residual / accepted (documented, not silently accepted):** until the full plan
lands, the alerts are inert (metric filters + exporter + VictoriaLogs pending — the
Phase-0/1/2 plan steps), and detections #3/#5/#6 are Phase-2. The remaining phased
work is tracked under plan `ka-0000007128`; F1 itself is remediated by the
program-of-record and this runbook, with the phased build as the follow-through.

---

## 10. Related beads

- **Finding F1:** `ka-0000000578` · audit report `ka-0000003522` · audit epic
  `ka-0000003872` · consolidation/rollup `ka-0000000609`.
- **Design of record:** `ka-0000003550` (bundle `ka-0000007676`).
- **Plan:** `ka-0000007128` — Phase 0 `ka-0000000178`, Phase 1 `ka-0000004743`,
  Phase 2 `ka-0000007666`, Phase 3 `ka-0000000495`.
- **Phase-1 alert-group card (metric contract):**
  [`eks-api-intrusion-runbook.md`](./archive/eks-api-intrusion-runbook.md).
- **Build steps:** metric filters `ka-0000005580` · exporter `ka-0000002106` ·
  PrometheusRule `ka-0000006648` (merged) · Slack routing `ka-0000002447` ·
  transport decision `ka-0000003460` / `ka-0000000935` · IAM log-read grant
  `ka-0000006138` · known-good egress set `ka-0000002105` · VictoriaLogs deploy
  `ka-0000007261` · LogsQL #3 `ka-0000006485` · forensic surface `ka-0000007948` ·
  measure-two-ways `ka-0000002458` · dead-man's-switch `ka-0000003782` ·
  threshold tuning `ka-0000002508` · staging red-team `ka-0000006556`.
- **RC1 outage precedent:** `ka-0000004055` / `ka-0000002512`;
  `prod-real.tfvars:162-186`.
