# Consuming upstream (non-gardenio) ACME production metrics for FSM troubleshooting — plan of record

**Task bead:** ka-0000007880 (synthesis). **Parent epic:** ka-0000000741 (FSM
troubleshooting & analysis). **Umbrella:** ka-0000007704 (gardenio time-series
metrics + chart platform).
**Status:** research / plan-of-record. No terraform, Helm, or producer code is
modified by this doc. Implementation children file under ka-0000000741 and are
gated on an operator `gb decision`.

This consolidates four scope-research results (inventory, ingestion paths, FSM
correlation model, tenancy/security) into a single decisive plan for how
non-gardenio ACME production signals (RDS, ECS, ELB, plus ACME application
producers) become queryable alongside gardenio agent activity for fast
field-service-management troubleshooting.

---

## 0. Inventory — what the non-gardenio ACME estate emits today

| Producer | Signal | Labels today | Current store | Retention | Owner / account |
|---|---|---|---|---|---|
| `cw-ecs-exporter`, `cloudwatch-rds` (yace, Fargate) | AWS/ECS, AWS/ECS/ContainerInsights, AWS/RDS, AWS/ApplicationELB, AWS/NATGateway | `cluster_name`, `service_name`, `aws_rds_db_instance_identifier`, `aws_account_id`, `aws_region` | per-env VictoriaMetrics (via vmagent) | ≈14d | per-env AWS account |
| `site-api`, `sponsor-api`, `study-web`, `harmony`, `unity` (ECS) | OTLP/HTTP metrics + traces | service-scoped (no gardenio labels); **PHI-bearing** attrs | tenant-local OTEL Collector → tenant vmagent → tenant VM | per-tenant | per-tenant ECS |
| Keycloak (ECS, shared) | Prometheus `/metrics`; traces to shared-prod-otel | service-scoped; `user.email` (pii-direct) | tenant vmagent → tenant VM; shared OTEL | per-tenant | shared |
| ACME internal K8s (devops EKS): agent-dispatcher, deploy-dispatcher, langfuse, litellm, dolt, pulsar, vllm, tenant-system | Prometheus `/metrics` | kube-state + node-exporter dims | kube-prometheus-stack (15d) + observability-chart `vmcluster` (14d, RF=2) | 14–15d | platform team / devops EKS |
| Mezmo / LogDNA DaemonSet | **logs (not metrics)** | n/a | Mezmo SaaS | Mezmo policy | tenant-system chart |
| **gardenio** controller / kbeads / `gb session` push | `gardenio_*`, `beads_*`, `kbeads_*` | `agent, project, role, model, env, task_id, fleet` | gardenio in-chart `vmsingle` (today **disabled**) | 1y (planned) | gardenio |

**Key facts:** each ACME env *already* stores its metrics in a per-env
VictoriaMetrics (fed by vmagent) — so the cross-estate question is read/federation,
not first-mile ingestion. The **W1 central OTEL Collector** (one pool, devops EKS,
VM+VT pair, PII gateway front-stage) is **designed but not yet deployed** and is the
future single front door. **PHI is the dominant constraint**: `site-api` /
`sponsor-api` / `tsda-api` / `study-web` emit clinical-trial PHI in metric/span
attributes (`subject.mrn`, `subject.dob`, `adverse_event.narrative`, RUM form
inputs) — which is precisely why the ACME path has a mandatory redaction gateway and
why gardenio must not become a second uncontrolled sink. (Cross-ref: ACME obs
architecture bundle **ka-0000003601**; perf-gap master epic **ka-0000004697**.)

---

## 1. The decision: read-federate, do not duplicate-ingest

**Primary architecture: gardenio READS from the ACME observability store; it does
NOT ingest ACME production metrics into the gardenio in-chart vmsingle.**

The four research scopes split on this question. The inventory and tenancy scopes
described a gardenio-native vmsingle and assumed ACME metrics might land in it; the
ingestion scope argued for read-federation; the FSM-correlation scope assumed a
shared query surface. The load-bearing documents resolve the split decisively in
favor of read-federation:

1. **`timeseries-victoriametrics-ops.md` §9 is an explicit hard scope.** The
   gardenio in-chart vmsingle stores *only* `gardenio_*`/`beads_*`/`kbeads_*`
   (plus `vmagent_*`/`vmsingle_*`/`process_*` self-monitoring), at 1-year
   retention, in-cluster-only, no Ingress. vmagent scrapes only the gardenio
   namespace and filters everything else out before remote_write. ACME
   production metrics are out of scope by construction.

2. **`obs-centralization-w1-topology.md` puts the ACME metrics store under a
   different epic and a different owner.** The central OTEL Collector backplane
   (epic ka-0000001783, @matthewbaker) writes to *its own* VictoriaMetrics +
   VictoriaTraces pair in devops EKS, behind a mandatory PII-redaction gateway
   (W2). Producers reference the backplane by channel name and never know its
   location. Pointing any producer at gardenio's vmsingle would violate W1
   constraint #4 (no hard location coupling) and W3 producer-location-agnosticity.

3. **The FSM use cases are read-time correlations, not storage problems.** The
   operator ask on ka-0000000741 is "correlate an agent incident timestamp with
   RDS CPU/connection spikes, ECS task restarts, ELB 5xx." That is a join across
   two stores at query time, satisfiable by registering the ACME store as a
   second Grafana datasource and writing cross-source dashboards. It does not
   require copying ACME series into gardenio's TSDB.

**Why duplicate-ingest is rejected as the primary path:**

- It re-introduces the very PHI surface the W2 gateway exists to eliminate
  (ACME metrics that have not traversed the gateway must never land in a
  gardenio-owned store), and it forces gardenio to either trust upstream
  redaction or re-redact — both are design smells.
- It couples gardenio's chart lifecycle to ACME topology (N producer
  dependencies on gardenio's write endpoint, or a cross-cluster NetworkPolicy
  carve-out from devops EKS into the gardenio namespace).
- It contradicts the committed §9 scope, which would have to be amended and
  re-sized (PVC growth, retention, backup posture all change).
- It buys nothing the read path lacks for the named FSM use cases.

**Where duplicate-ingest is preserved as an explicit, deferred fallback:** if a
later, *named* compliance/audit requirement emerges — e.g. gardenio must hold a
durable per-producer cost-attribution ledger as billing evidence beyond ACME's
14-day retention — then a narrow "funnel exporter" is the only ingest path that
respects W1. It is a *second* `prometheusremotewrite` exporter added to the
central OTEL Collector (post-redaction, gated by a config knob), filtered to a
small instrumentation/cost slice, writing to gardenio's vmagent. This is **not**
v1, requires the W1+W2 gateway to exist first, and requires its own operator
decision. It is sized as a late epic below, not a near-term deliverable.

---

## 2. Recommended architecture

```text
  ACME producers (ECS apps, Keycloak,         gardenio producers
  exporters, ACME-internal K8s)               (controller, beads, gb session push)
        | observability_channel="central"           | scrape + push
        v                                            v
  W1 central OTEL Collector (devops EKS)        gardenio vmagent (gardenio ns)
   - W2 PII-redaction gateway (first stage)      - filter: gardenio_*/beads_*/kbeads_* only
   - producer-class tag from bearer token        |
        |                                         v
        v                                    gardenio vmsingle (1y retention, in-cluster)
  ACME VictoriaMetrics + VictoriaTraces            |
  (devops EKS, 14d platform / unbounded central)   |
        \                                          /
         \                                        /
          v                                      v
        ====== FSM read surface (Grafana) ======
        Two datasources registered side by side:
          DS-gardenio  -> gardenio vmsingle  (gardenio_*/beads_*)
          DS-acme     -> ACME VM/VT central store (RDS/ECS/ELB + app producers)
        Cross-source FSM dashboards join on shared labels at query time.
                              |
                              v
        (Phase 2) chart-backend API brokers both datasources, returns
        a correlated result set; iOS + web consume the same envelope.
```

Read access in v1 is **entirely through Grafana**, gated by Keycloak forwardAuth
(plus IP allowlist in prod), exactly as `timeseries-victoriametrics-ops.md` §7
specifies. No raw `/api/v1/query_range` surface is exposed; vmauth stays deferred
until an off-cluster consumer needs it. The Phase-2 chart backend (umbrella
ka-0000004684) is the eventual broker that fans a query envelope to both stores
and merges results, which is the right home for any cross-TSDB join logic that
PromQL cannot express in a single datasource.

**The join model (the hard part of FSM).** gardenio metrics and ACME metrics share
*no* label dimensions today. Gardenio carries `agent, project, role, model, env,
task_id`; ACME ECS metrics carry `cluster_name, service_name, task_family,
aws_ecs_*`; RDS carries `aws_rds_db_instance_identifier, aws_account_id,
aws_region`. The two estates therefore join on **(time window, env) plus an
explicit resource-mapping table**, not on a naturally-shared key:

- **`env`** is the one dimension both sides can carry; ensure ACME producer
  metrics are labeled with an `env`/`acme_environment` consistent with gardenio's
  `env` values, normalized at the ACME ingest point (OTEL transform / vmagent
  relabel), never in dashboards.
- **A `project → {ECS cluster_name, ECS service_name, RDS instance_id}` mapping**
  is maintained gardenio-side (controller-emitted metric
  `gardenio_resource_mapping{project,cluster,service,db_instance}` or a chart
  ConfigMap) so a dashboard variable can pivot from an agent project to the ACME
  resources it exercises. This is the primary join key and is a gardenio-owned
  artifact — it does not require any change to ACME producers.
- **Time alignment**: accept 1–5s wall-clock skew; both stores are millisecond
  Prometheus-sample precision. Correlation predicates window at T ± 30s.

**Worked correlation sketches** (the `$db_for_project` / `$service_for_project`
variables resolve via `gardenio_resource_mapping`):

```promql
# (a) agent OOM correlated to ECS task restart, same env window
gardenio_agent_lifecycle_events_total{event="oom_killed", env="prod"}
  and on(env) (rate(aws_ecs_task_restart_total{env="prod"}[1m]) > 0)

# (b) agent p99 tool latency vs RDS connection saturation (pivot via mapping)
gardenio_tool_latency_seconds{quantile="0.99", project="acme", env="prod"}
aws_rds_database_connections{aws_rds_db_instance_identifier=~"$db_for_project"}

# (c) agent cost jump vs ELB 5xx
increase(gardenio_agent_cost_usd{project="acme", env="prod"}[5m])
sum by (service_name) (rate(aws_applicationelb_httpcode_target_5xx_count{env="prod"}[5m]))
```

Cardinality budgets: `project ≤ 50`, `service ≤ 50`, `role ≤ 50`, `env ≤ 5`,
`account ≤ 30`, `model ≤ 20`. `agent` / `task_id` / `bead_id` are **forbidden as
join keys** (unbounded); per-agent drill-down goes through exemplars / logs.

---

## 3. What is nearly-free vs net-new

**Nearly-free (already plumbed, values-only or read-only):**
- gardenio vmsingle + vmagent + Grafana templates exist in the chart behind
  `enabled: false`. Enabling them in one environment is the umbrella v1 ship.
- Registering the ACME observability store as a second Grafana datasource is a
  provisioning entry, not new infrastructure.
- Spend-per-agent-per-hour and beads-events-by-type producers already emit
  (per umbrella R3/R4); the gap is the receiver + one counter.

**Net-new for FSM specifically:**
- The cross-estate `env` label normalization on ACME metrics (small relabel,
  but it must be confirmed/added at the ACME ingest point — ownership question).
- The `project → ECS/RDS resource` mapping artifact (gardenio-owned, new).
- The FSM correlation dashboard set + a reusable PromQL/query template library.
- The composite-alert rule (agent OOM ∧ ECS task restart in same window).

---

## 4. Sequenced implementation epics (file as children under ka-0000000741)

Dependency-ordered. Titles only — do not invent bead IDs.

1. **Enable gardenio vmsingle + vmagent in gardenio-test (umbrella v1 receiver).**
   Pure wrapper-chart values enablement; gardenio repo untouched. Establishes the
   gardenio-native read endpoint that FSM dashboards sit beside. Depends on: none.
   (This is umbrella R1 §6 task T-vm-5; FSM consumes it.) Acceptance: vmsingle
   stores `gardenio_*`/`beads_*` at 1y; vmagent metric-name allow-list relabel in
   place; Grafana renders the two prebuilt dashboards.

2. **Register the ACME observability store as a second Grafana datasource.**
   Read-only. In dev/rwx via in-chart Grafana; in prod register gardenio vmsingle
   into the ACME Grafana instead (single-Grafana convergence). Depends on: epic 1.
   Acceptance: a Grafana instance can query both gardenio `gardenio_*` series and
   ACME `aws_ecs_*`/`aws_rds_*` series; access gated by Keycloak forwardAuth.

3. **Define and publish the FSM cross-estate label/join contract.**
   New doc `FSM_CORRELATION_CONTRACT.md`: join keys (project, env), the
   `project → ECS/RDS` mapping schema, timestamp tolerance, cardinality budgets
   (project ≤50, service ≤50, env ≤5), and the supported metric pairs. Depends
   on: none (can run parallel to 1–2). Acceptance: contract reviewed; named
   join keys exist on both estates or are flagged as a producer-side gap.

4. **Emit the `project → ECS/RDS resource` mapping from gardenio.**
   Controller-side `gardenio_resource_mapping{project,cluster,service,db_instance}`
   gauge (or chart ConfigMap if the mapping is static per env). This is the
   primary join key for read-time correlation. Depends on: epic 3. Acceptance:
   a Grafana dashboard variable can resolve an agent project to its ACME
   resource identifiers.

5. **Confirm/normalize the shared `env` label on ACME production metrics.**
   Audit where ECS Container Insights / RDS metrics get an environment label at
   the ACME ingest point (OTEL transform or vmagent relabel) and ensure values
   align with gardenio `env`. This is the ownership question that must be resolved
   with the centralized-observability epic. Depends on: epic 3. Acceptance: ACME
   metrics carry an `env` value that matches gardenio's, queryable cross-source.

6. **Build the FSM correlation dashboard + query-template library.**
   The 2x2 grid (gardenio fleet/cost top-left, ACME infra top-right, beads-events
   timeline bottom-left, correlation scatter bottom-right) with a shared
   {time_range, project, env} filter, plus 3–5 worked PromQL correlation panels
   (agent incident vs RDS CPU/conns; agent OOM vs ECS restart; cost spike vs ELB
   5xx). Depends on: epics 2, 4, 5. Acceptance: an operator can pivot from an
   agent incident timestamp to the correlated ACME infra panels in one dashboard.

7. **Composite FSM alert: agent OOM ∧ ECS task restart in the same window.**
   The operational signal ka-0000000741 exists to produce. Routes via the
   existing ACME kube-prometheus-stack Alertmanager (do not deploy a second one);
   links both metric dashboards + the session-recording S3 key. Depends on:
   epics 5, 6. Acceptance: a synthetic OOM+restart correlation fires the
   composite alert with both deep links.

8. **(Deferred / conditional) Funnel-exporter for ACME-metric duplicate-ingest.**
   Only if a named compliance/audit requirement to durably hold a ACME-derived
   slice in gardenio emerges. A second post-redaction `prometheusremotewrite`
   exporter on the W1 central collector, config-gated, filtered to a cost/
   instrumentation slice, writing to gardenio vmagent. Depends on: W1+W2 gateway
   live (epic ka-0000001783), and a separate operator decision. Acceptance: not
   built unless the requirement is named; documented here as the only
   W1-respecting ingest path so nobody reaches for per-tenant direct remote_write.

---

## 5. Operator decision points

- **D-A (gate): read-federation vs duplicate-ingest.** This plan recommends
  read-federation as primary, with the funnel exporter deferred and conditional.
  Confirm, or name the specific audit/billing requirement that would justify
  duplicate-ingest now.
- **D-B: who owns the `env`-label normalization on ACME metrics** — the FSM epic
  (ka-0000000741) or the centralized-observability epic (ka-0000001783)? The
  vmagent/transform relabel lives ACME-side; the join contract is gardenio-side.
- **D-C: prod Grafana** — register gardenio vmsingle into the ACME observability
  Grafana (single login, recommended for prod) vs run in-chart Grafana
  (self-contained, recommended for dev/rwx).
- **D-D: top FSM correlation pairs to ship first.** Recommend the top three:
  (a) agent OOM → ECS task restart, (b) agent latency spike → RDS connection
  saturation, (c) agent cost jump → ELB 5xx. Confirm priority.
- **D-E: retention mismatch.** gardenio metrics retain 1y; ACME platform metrics
  retain 14d (central backplane store retention TBD by ka-0000001783). A 30-day
  agent incident cannot correlate against a 14-day-old RDS event. Decide whether
  FSM needs ACME infra retention dialed up (cost trade-off owned by the obs epic)
  or whether FSM correlation windows are bounded to the ACME retention horizon.
- **D-F: query-broker home** — keep FSM joins as PromQL-per-datasource in Grafana
  (v1) vs move them into the Phase-2 chart backend (umbrella ka-0000004684) for
  true cross-TSDB merge. Recommend Grafana for v1, backend for cross-TSDB joins.
- **D-G: cardinality guardrail.** Confirm the cardinality budgets in the join
  contract and that high-cardinality dimensions (`agent`, `task_id`, `bead_id`)
  are forbidden as join keys (use exemplars/logs for per-agent drill-down).

---

## 6. Cross-document deltas (where this extends or contradicts committed research)

- **Extends `timeseries-victoriametrics-ops.md` §9** by adding a second,
  *read-only* Grafana datasource (the ACME central store) beside gardenio
  vmsingle. It does **not** relax the §9 write scope: gardenio vmsingle still
  stores only `gardenio_*`/`beads_*`. The §9 hint that ACME Prometheus might
  remote_write gardenio metrics into vmsingle is explicitly the inverse of what
  FSM needs and is not used here.
- **Defers, does not adopt, the ingestion scope's "funnel exporter" fallback.**
  The ingestion research correctly identified the funnel exporter as the only
  W1-respecting ingest path; this plan agrees but moves it out of v1 and gates it
  on a named compliance requirement plus W1+W2 being live.
- **Reconciles the inventory + tenancy scopes' assumption** that ACME metrics
  land in gardenio vmsingle: they do not in v1. Those scopes' PII/redaction and
  per-producer-class tagging analysis remains correct and load-bearing — but it
  governs the W1 backplane (ka-0000001783), not gardenio's store. Gardenio never
  re-redacts; anything it might later hold (epic 8) is already redacted by the
  gateway.
- **Inherits the W1/W4 ownership boundary cleanly.** The central backplane,
  PII gateway, channel resolver, and producer cutover sequence belong to
  ka-0000001783 / @matthewbaker. FSM (ka-0000000741) is a *consumer* of that
  store and must not couple its delivery to the W4 migration completing — FSM
  read-federation works against whatever the ACME observability store is today
  (kube-prometheus-stack / victoria-metrics-k8s-stack vmcluster), and transitions
  transparently when W4 lands because the datasource is registered by store
  identity, not producer topology.
- **Drift found:** the four scopes used inconsistent bead-ID anchors for W2
  (ka-0000001206 vs ka-0000002675 per MR !2764) and the obs epic
  (ka-0000001783 vs ka-0000004697 vs ka-0000003601). The authoritative anchors
  from the committed docs: obs epic = **ka-0000001783**; W1 = ka-0000006336;
  W2 = ka-0000001206 (MR !2764); W3 = ka-0000008215; W4 = ka-0000007188;
  timeseries ops = ka-0000000179; umbrella = ka-0000007704.
