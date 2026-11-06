# FSM Cross-Estate Label / Join Contract

**Status:** Normative · v1
**Owner:** gardenio (FSM thrust — epic [ka-0000000741])
**Source of record:** `gardenio/docs/research/upstream-metrics-plan.md` §0–§3 (plan-of-record, MR !3200)
**Consumers:** the FSM correlation dashboards, query-template library, and composite
alert rules built under [ka-0000000741]; the `gardenio_resource_mapping` producer;
the ACME `env`-label normalization audit.

---

## 0. Purpose & scope

This document is the **contract** that makes cross-estate FSM (field-service-management)
troubleshooting possible. The gardenio metrics estate and the non-gardenio ACME
production estate are stored in **two separate VictoriaMetrics stores** and joined
at **read time** in Grafana (read-federation — see plan §1/§2; gardenio does **not**
duplicate-ingest ACME series into its own vmsingle). Those two estates share **no
naturally-common label dimension**. This contract defines the *only* sanctioned way
to join them:

- the **join keys** both estates must carry (§1),
- the gardenio-owned **`project → {ECS, RDS}` resource-mapping schema** that is the
  primary pivot (§2),
- the **timestamp tolerance** for correlation predicates (§3),
- the **cardinality budgets** that keep joins bounded (§4),
- the **supported metric pairs** FSM ships against (§5), and
- the dimensions that are **forbidden as join keys** (§6).

Anything not described here is out of contract. A correlation that cannot be
expressed in terms of these keys is not a v1 FSM correlation — extend this contract
(with operator review) before building it.

---

## 1. Join keys

There are exactly **two** sanctioned join dimensions in v1:

| Join key | gardenio label | ACME label | Notes |
|---|---|---|---|
| **`env`** | `env` | `env` (normalized — see below) | The one dimension both estates can carry directly. The coarse correlation key. |
| **`project`** | `project` | *(none — resolved via the mapping table, §2)* | gardenio-only. Pivots to ACME resources through `gardenio_resource_mapping`. The primary fine-grained key. |

### 1.1 `env` — the shared coarse key

`env` is the single label both estates carry natively. ACME production metrics
(`aws_ecs_*`, `aws_rds_*`, `aws_applicationelb_*`, ACME app producers) do **not**
carry an `env` label that is guaranteed to match gardenio's `env` values today. The
contract requires:

- ACME producer metrics MUST be labeled with an `env` whose **values are identical**
  to gardenio's `env` values (e.g. `prod`, `staging`, `test` — not `production`,
  `prd`, etc.).
- Normalization happens **at the ACME ingest point** (OTEL transform processor or
  vmagent `relabel_config`), **never in dashboards**. Dashboards assume the label is
  already aligned.
- If the ACME side cannot supply a matching `env`, that is a **producer-side gap** and
  MUST be flagged (it is the explicit ownership question **D-B** in the plan — owned
  by the centralized-observability epic [ka-0000001783] vs the FSM epic). Do not paper
  over it with per-dashboard label rewriting.

`env` cardinality is budgeted at **≤ 5** (§4).

### 1.2 `project` — the primary fine-grained key

`project` exists only on the gardenio side. ACME infrastructure metrics are keyed by
`cluster_name` / `service_name` / `aws_rds_db_instance_identifier`, which gardenio
metrics never carry. The two are bridged **only** through the gardenio-owned
`project → {ECS cluster/service, RDS instance}` mapping table (§2). A dashboard
variable resolves a `project` to its ACME resource identifiers; the join predicate
then matches on those resolved identifiers plus `env`.

`project` cardinality is budgeted at **≤ 50** (§4).

> **No other label is a join key in v1.** `role`, `model`, `account`, `fleet`,
> `cluster_name`, `service_name`, `task_family` may appear as *filter* or *group-by*
> dimensions on one side, but they are not cross-estate join keys. `agent`,
> `task_id`, and `bead_id` are **forbidden** (§6).

---

## 2. The `project → {ECS, RDS}` resource-mapping schema

Because `project` does not exist on the ACME estate, the join is carried by a
**gardenio-owned mapping artifact** that declares, per project and env, which ACME
resources that project exercises. This artifact is the **primary join key** for
read-time correlation and requires **no change to ACME producers**.

### 2.1 Canonical form — `gardenio_resource_mapping` gauge

The controller emits (or a chart `ConfigMap` materializes, if the mapping is static
per env) a gauge whose label set *is* the mapping row:

```
gardenio_resource_mapping{
  project     = "<gardenio project>",     # join key, ≤ 50 distinct
  env         = "<gardenio env>",         # join key, ≤ 5 distinct
  cluster     = "<ECS cluster_name>",    # ACME-side identifier
  service     = "<ECS service_name>",    # ACME-side identifier, ≤ 50 distinct
  db_instance = "<RDS aws_rds_db_instance_identifier>"
} 1
```

| Field | Type | Maps to (ACME label) | Cardinality budget |
|---|---|---|---|
| `project` | string | *(join key — gardenio-only)* | ≤ 50 |
| `env` | string | `env` (normalized) | ≤ 5 |
| `cluster` | string | ECS `cluster_name` | — |
| `service` | string | ECS `service_name` | ≤ 50 |
| `db_instance` | string | RDS `aws_rds_db_instance_identifier` | — |

Value is always `1` (presence gauge). A `(project, env)` pair MAY map to **multiple**
services and/or DB instances — emit one series per `(project, env, cluster, service,
db_instance)` tuple. The series set is the mapping table.

### 2.2 How dashboards consume it

Grafana template variables resolve a selected `project`/`env` to ACME identifiers:

```promql
# services a project exercises in an env
$service_for_project = label_values(
  gardenio_resource_mapping{project="$project", env="$env"}, service)

# RDS instances a project exercises in an env
$db_for_project = label_values(
  gardenio_resource_mapping{project="$project", env="$env"}, db_instance)
```

Correlation panels then constrain ACME series with the resolved variables (worked
examples in §5).

### 2.3 Rules

- The mapping is **gardenio-owned and gardenio-maintained**. ACME producers are never
  asked to learn about `project`.
- It is **declarative presence data**, not a metric of activity — value is always `1`.
- Keep it **bounded**: `project ≤ 50`, `service ≤ 50`, `env ≤ 5` (§4). The mapping's
  series count is `Σ rows`, which these budgets keep well inside cardinality limits.
- The producer is the controller metrics collector
  (`gardenio/controller/internal/metrics/metrics.go`, `collectResourceMapping`):
  each managed project bead declares its tuples in a `resource_mapping` JSON field
  (`[{"env","cluster","service","db_instance"}, ...]`, parsed into
  `beadsapi.ResourceMappingEntry`), and the collector emits one
  `gardenio_resource_mapping` presence series per `(project, env, cluster, service,
  db_instance)` tuple on each scrape. Rows with an empty `env` are skipped (env is a
  join key). This contract defines the **schema and semantics**; the cited code is its
  implementation (plan §4 epic 4).

---

## 3. Timestamp tolerance (T ± 30s)

Cross-estate correlation predicates window at **T ± 30 seconds**.

- Both stores hold millisecond-precision Prometheus samples; expected wall-clock skew
  between the estates is **1–5s**. The ±30s window absorbs that skew plus scrape-interval
  phase difference (ACME Container-Insights/RDS export is coarser than gardenio scrape)
  without smearing distinct events together.
- The window is applied in the correlation predicate — e.g. an `and on(...)` join over a
  range-vector rate (`[1m]`), or an explicit `T ± 30s` bound in a chart-backend query
  envelope (Phase 2) — **not** by widening dashboard time ranges.
- ±30s is the **default and the contract value**. A specific correlation MAY justify a
  tighter window; it MUST NOT silently widen it (a wider window inflates false-positive
  correlations). Any deviation is documented on the panel/alert that uses it.

---

## 4. Cardinality budgets

Join keys and mapping dimensions are bounded so that cross-estate queries stay cheap
and the mapping series set stays small. These are **contract ceilings**, enforced at
the producer / relabel layer:

| Dimension | Budget | Role |
|---|---|---|
| `project` | **≤ 50** | join key (gardenio) |
| `service` | **≤ 50** | mapping → ACME ECS service |
| `env` | **≤ 5** | join key (both estates) |
| `role` | ≤ 50 | filter only (not a join key) |
| `account` | ≤ 30 | filter only |
| `model` | ≤ 20 | filter only |

A producer or relabel rule that would push any join/mapping dimension past its budget
is a contract violation — raise it as a cardinality issue (plan decision **D-G**), do
not ship past the ceiling. High-cardinality drill-down does not go through labels at
all (§6).

---

## 5. Supported metric pairs (v1)

FSM ships against the following **three** correlation pairs (plan §2 worked sketches;
operator priority is decision **D-D**). Each pair is `(gardenio signal) × (ACME signal)`
joined on the keys in §1 and windowed per §3. `$db_for_project` / `$service_for_project`
resolve via `gardenio_resource_mapping` (§2).

### (a) Agent OOM ↔ ECS task restart — joined on `env`, same window

```promql
gardenio_agent_lifecycle_events_total{event="oom_killed", env="prod"}
  and on(env) (rate(aws_ecs_task_restart_total{env="prod"}[1m]) > 0)
```

### (b) Agent p99 tool latency ↔ RDS connection saturation — pivot via mapping

```promql
gardenio_tool_latency_seconds{quantile="0.99", project="acme", env="prod"}
aws_rds_database_connections{aws_rds_db_instance_identifier=~"$db_for_project"}
```

### (c) Agent cost jump ↔ ELB 5xx — pivot via mapping

```promql
increase(gardenio_agent_cost_usd{project="acme", env="prod"}[5m])
sum by (service_name) (
  rate(aws_applicationelb_httpcode_target_5xx_count{env="prod"}[5m]))
```

| Pair | gardenio signal | ACME signal | Join path |
|---|---|---|---|
| (a) | `gardenio_agent_lifecycle_events_total{event="oom_killed"}` | `aws_ecs_task_restart_total` | `on(env)`, T±30s |
| (b) | `gardenio_tool_latency_seconds{quantile="0.99"}` | `aws_rds_database_connections` | `project`→`db_instance` (§2), `env` |
| (c) | `gardenio_agent_cost_usd` | `aws_applicationelb_httpcode_target_5xx_count` | `project`→`service`, `env` |

Pair **(a)** is the operational signal the FSM epic exists to produce and is the basis
of the composite alert (plan §4 epic 7: *agent OOM ∧ ECS task restart in the same
window*). Additional pairs are **out of contract until added here** with operator review.

### 5.1 The shipped composite alert (pair (a))

The composite alert for pair (a) is implemented as a Helm-rendered
`PrometheusRule` (ka-0000006124):

- **Template:** `gardenio/helm/gardenio/templates/fsm-composite-alert.yaml`
- **Values:** `fsmCompositeAlert.*` in `values.yaml` (off by default; per-env
  overlays opt in).
- **Alert:** `FSMAgentOOMWithECSRestart` in group `gardenio.fsm.composite`.

**Routing — no second Alertmanager.** The rule is a plain
`monitoring.coreos.com/v1` `PrometheusRule` CR. It is discovered by the
**existing ACME kube-prometheus-stack** prometheus-operator `ruleSelector` and
routed through **that cluster's Alertmanager**. The chart deploys **no**
Alertmanager of its own. If the cluster's `ruleSelector` matches on a label
(most installs match `release: <kube-prom-stack release>`), set it via
`fsmCompositeAlert.prometheusRuleSelectorLabels` so the rule is picked up.

**Expr — the join.** The contract sketch `and on(env)` correlates only at the
coarse `env` level. The shipped rule tightens the window to
`(env, cluster, service)`, as the task requires, by pivoting the OOM count
through the gardenio-owned `gardenio_resource_mapping` (§2) and joining the
renamed ECS restart series on all three keys:

```promql
(
  gardenio_resource_mapping
  * on (env, project) group_left ()
  (
    sum by (env, project) (
      increase(gardenio_agent_lifecycle_events_total{event="oom_killed"}[5m])
    ) > 0
  )
)
and on (env, cluster, service)
(
  label_replace(
    label_replace(
      sum by (env, cluster_name, service_name) (
        increase(aws_ecs_task_restart_total[5m])
      ) > 0,
      "cluster", "$1", "cluster_name", "(.+)"
    ),
    "service", "$1", "service_name", "(.+)"
  )
)
```

The `group_left()` keeps the multiply many-to-one (mapping rows are the "many"
side, the per-`(env,project)` OOM count the "one" side), so the result carries
the `project`, `cluster`, `service`, and `db_instance` labels needed for the
deep links. The `[5m]` window + `for: 0m` is the same-window predicate (default
5m absorbs the documented T±30s estate skew, §3).

**Annotations — deep links + S3 key.** The alert carries:

- `dashboard_agent_url` — deep link to the gardenio agent/fleet dashboard,
  pre-filtered by `env`/`project`.
- `dashboard_acme_url` — deep link to the ACME infra / FSM correlation dashboard
  (ka-0000007874), pre-filtered by `env`/`cluster`/`service`.
- `session_recording_s3` — the S3 key of the OOM'd session's recording,
  resolved at notification time via a `topk(1, ...{event="oom_killed", env, project})`
  template `query`. `session_id` is a §6 drill-down identifier surfaced as an
  annotation only — it is **never** a join key.

**Synthetic-firing proof (acceptance).** `tests/fsm_composite_alert_rules_test.yaml`
is a `promtool` unit test that feeds synthetic series for an OOM and an ECS
restart in the same `(env,cluster,service)` window and asserts the alert fires
with both deep links and the S3 key resolved — plus three negatives (OOM-only,
restart-only, restart-on-unmapped-service) that must NOT fire. The Go test
`TestFSMCompositeAlert_SyntheticOOMRestartFires` renders the rule and runs that
proof (skips gracefully when `helm`/`yq`/`promtool` are absent).

---

## 6. Forbidden join keys

The following dimensions are **FORBIDDEN as cross-estate join keys**:

- **`agent`**
- **`task_id`**
- **`bead_id`**

These are **unbounded / high-cardinality** identifiers. Using any of them as a join key
would explode the cross-product, blow the cardinality budgets (§4), and make read-time
joins prohibitively expensive — this is plan decision **D-G**, confirmed.

Per-agent / per-task / per-bead drill-down does **not** go through metric labels at all.
It goes through **exemplars and logs** (trace/exemplar links off a metric panel, or a
Mezmo/log query keyed by the identifier), keeping the high-cardinality lookup off the
time-series join path.

> Rule of thumb: a join key must fit a cardinality budget in §4. If a dimension is
> per-agent, per-task, per-bead, or otherwise unbounded, it is a drill-down identifier,
> never a join key.

---

## 7. Conformance checklist

A cross-estate FSM correlation is **in contract** iff:

- [ ] it joins only on `env` and/or `project` (via the §2 mapping) — §1;
- [ ] any `project`→ACME pivot resolves through `gardenio_resource_mapping` — §2;
- [ ] its correlation predicate windows at T ± 30s (or documents a tighter window) — §3;
- [ ] every join/mapping dimension is within its cardinality budget — §4;
- [ ] it is one of the supported metric pairs, or has been added to §5 with operator
      review — §5;
- [ ] it uses **no** `agent` / `task_id` / `bead_id` as a join key — §6.

---

## References

- Plan of record: `gardenio/docs/research/upstream-metrics-plan.md` (§0 inventory, §1
  read-federation decision, §2 architecture + join model, §3 nearly-free vs net-new,
  §5 operator decisions) — MR !3200.
- Epic: **[ka-0000000741]** — Consume upstream metrics from non-gardenio production
  services (FSM troubleshooting & analysis).
- R0 research/plan task: **[ka-0000007880]**.
- Storage scope (gardenio vmsingle): `gardenio/docs/research/timeseries-victoriametrics-ops.md`
  §9 — gardenio store holds only `gardenio_*`/`beads_*`/`kbeads_*`.
- Centralized-observability backplane (ACME store, PII gateway, `env`-normalization
  ownership): epic **ka-0000001783**; topology `gardenio/docs/research/obs-centralization-w1-topology.md`.

[ka-0000000741]: https://beads.gardenio.e2e.example.com/bead/ka-0000000741
[ka-0000007880]: https://beads.gardenio.e2e.example.com/bead/ka-0000007880
[ka-0000001783]: https://beads.gardenio.e2e.example.com/bead/ka-0000001783
