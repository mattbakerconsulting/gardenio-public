# VPA right-sizing — guardrails, observability & rollback

Safety net for the VPA crew-pod right-sizing rollout (epic **ka-0000005189**,
task **ka-0000004597**). It exists so the right-sizing **cannot silently start
failing agent runs**: recommended-vs-actual-vs-request dashboards, OOM /
recommender alerting, and a documented rollback that returns the cluster to the
flat-request status quo in one step.

This is the companion to the install chart (`acme-helm-chart/charts/vpa`, task
ka-0000004038) and the staged-rollout task (ka-0000008205). **The guardrails in
this doc are a HARD GATE: they must be live and green before any VPA enforce
flip** (updater enabled / `vpaRightSizing.updateMode` off `Off`).

The in-place enforce machinery + mid-run eviction guards themselves are task
**ka-0000003138** (shipped safe-by-default: `vpaRightSizing.updateMode: "Off"`);
see [Enforce mode & eviction guards](#enforce-mode--eviction-guards) below for
what the flip turns on and [Rollback](#rollback) for how to revert it.

---

## What ships

All gated behind one switch — `agents.metrics.prometheusRule.vpaGuardrails.enabled`
(default **false**) in the gardenio Helm chart — and opt-in per env:

| Component | Template | Purpose |
|---|---|---|
| `PrometheusRule` `…-vpa-guardrails` | `templates/vpa-guardrails/alerts.yaml` | 4 alerts: CrewOOMKilled, CrewMemoryNearLimit, VPARecommenderDown, VPARecommenderNoRecommendation |
| recommender `Service` + `ServiceMonitor` | `templates/vpa-guardrails/recommender-servicemonitor.yaml` | scrape vpa-recommender `:8942` operational metrics (`recommenderServiceMonitor.enabled`) |
| reco-value exporter (KSM CRS) | `templates/vpa-guardrails/recommendation-exporter.yaml` | surface `VPA.status.recommendation.target` as a metric (`recommendationExporter.enabled`) |
| Grafana dashboard ConfigMap | `templates/vpa-guardrails/dashboard-configmap.yaml` + `dashboards/vpa-right-sizing.json` | recommended vs actual vs request + OOM panels (`grafanaDashboard.enabled`) |

---

## Metric architecture (read this before editing queries)

The data series this task needs do **not** all live in one place, and **which
cluster you are on decides the design**:

- On `america-e2e-eks` (verified 2026-06-23) a shared **observability**
  kube-prometheus stack scrapes cadvisor + a cluster KSM, so the series
  already exist there.
- On `prod-gardenio-eks` (verified 2026-07-19, ka-0000000341) there is **no
  observability stack at all** — no kube-prometheus, no cluster KSM, no
  cadvisor scrape, and only the `monitoring.coreos.com` **CRDs** (no operator
  controller, so `PrometheusRule`/`ServiceMonitor` CRs are never reconciled).
  The series exist **nowhere** unless the gardenio chart provides them itself.

| Series | What | Source on america-e2e-eks | Source on prod-gardenio-eks |
|---|---|---|---|
| **actual** | `container_memory_working_set_bytes` | cadvisor → **observability** Prometheus | kubelet `/metrics/resource` → in-chart vmagent (`vmagent.kubeletScrape`) → `vmsingle` |
| **request** | `kube_pod_container_resource_requests{resource="memory"}` | cluster KSM → **observability** Prometheus | namespace-scoped KSM (`monitoring.podStateMetrics`) → in-chart vmagent → `vmsingle` |
| **OOM** | `kube_pod_container_status_last_terminated_reason{reason="OOMKilled"}`, `…_restarts_total` | KSM → **observability** Prometheus | `monitoring.podStateMetrics` → `vmsingle` |
| **recommended** | `kube_customresource_vpa_status_recommendation_target` | the **reco-value exporter** shipped here → **observability** Prometheus | reco-value exporter `:8080` → dedicated `vpa-reco-exporter` vmagent job (`recommendationExporter.enabled`) → `vmsingle` — once the VPA CRD + recommender install lands (ka-0000000525) |
| recommender health | `vpa_recommender_vpa_objects_count{matches_pods,has_recommendation,…}` | vpa-recommender `:8942` → recommender ServiceMonitor | vpa-recommender `:8942` → dedicated `vpa-recommender` vmagent job (`recommenderServiceMonitor.enabled`) → `vmsingle` — once the VPA install lands (ka-0000000525) |

Consequences that drive the design:

1. **gardenio's in-chart VictoriaMetrics (`vmsingle`) carries none of these by
   default.** `gardenio-vmagent` only scrapes gardenio pods annotated
   `prometheus.io/scrape`. Two default-off chart knobs close the gap
   (ka-0000000341):

   - **`vmagent.kubeletScrape.enabled`** — node-SD scrape of every kubelet's
     `/metrics/resource` (the small metrics-server endpoint, *not*
     `/metrics/cadvisor`), keep-filtered to the release namespace + a
     `keepMetrics` family list so whole-cluster product pods never enter
     vmsingle's retention. Renders its own ClusterRole (`nodes` get/list/watch
     + `nodes/metrics` get) and a vmagent→nodes:10250 egress allowance.
   - **`monitoring.podStateMetrics.enabled`** — a namespace-scoped
     kube-state-metrics (`--resources=pods --namespaces=<release ns>
     --metric-allowlist=…`) emitting exactly the request/limit/OOM/restart
     families, scraped by a dedicated `honor_labels: true` vmagent job (the
     default annotation job would clobber the subject `namespace`/`pod`
     labels with the exporter's own identity).

   On clusters that *do* have an observability Prometheus, pointing the board
   there remains valid — but on prod-gardenio-eks these knobs are the only
   source.

2. **"recommended" is not a metric by default.** kube-state-metrics removed its
   built-in `verticalpodautoscalers` collector in v2.x (confirmed empirically on
   v2.13.0), and the cluster KSM runs no Custom Resource State (CRS) config for
   VPAs. So we ship a dedicated, **namespace-scoped** KSM in CRS mode
   (`recommendationExporter`) that reads only gardenio's own VPA objects and
   emits `kube_customresource_vpa_status_recommendation_target` (bytes, labelled
   by `container` / `target_name` / `verticalpodautoscaler`). Validated against a
   live recommender: `target.memory` 100Mi surfaced as `1.048576e+08`. On fresh
   prod this stays dark until the VPA CRD + recommender install
   (acme-helm-chart `charts/vpa`) lands — the exporter has nothing to read.

   **Scrape path on prod (ka-0000000525).** Both VPA sources — the reco-value
   exporter (`recommendationExporter`, `:8080`) and the recommender
   (`recommenderServiceMonitor`, `:8942`) — ship a `ServiceMonitor`. That is
   honored only where a prometheus-operator / observability Prometheus runs
   (e.g. america-e2e-eks). The in-chart `gardenio-vmagent` runs a **static
   `-promscrape.config`** and does not discover ServiceMonitors, and
   prod-gardenio-eks has no observability Prometheus — so on prod these series
   reach `vmsingle` via **dedicated static vmagent scrape jobs** (`vpa-reco-exporter`
   with `honor_labels: true`, and `vpa-recommender` stamping `namespace`/`job`
   from SD meta), exactly mirroring the `pod-state-metrics` pattern above. Both
   jobs + their network-policy allowances (`:8080`/`:8942` ingress on the
   exporters, matching vmagent egress) are gated on the same
   `recommendationExporter.enabled` / `recommenderServiceMonitor.enabled` knobs
   that render the exporter Deployment + ServiceMonitor. The ServiceMonitors are
   kept for the operator-Prometheus path; they are simply inert on prod.

### Datasource

There is **no central Grafana** on the cluster; the operator's home is
`gardenio-grafana`, whose only provisioned datasource is the in-chart
VictoriaMetrics (uid `victoriametrics`). The `vpa-right-sizing` dashboard uses
a `$DS_PROMETHEUS` datasource **variable** (Grafana's standard import-time
prompt):

- **On prod-gardenio-eks** (and any cluster without an observability stack):
  enable `vmagent.kubeletScrape` + `monitoring.podStateMetrics`
  (ka-0000000341) and point `$DS_PROMETHEUS` at the in-chart
  **VictoriaMetrics** datasource — the series then live in `gardenio-vmsingle`.
- **On a cluster with a shared observability kube-prometheus** (e.g.
  america-e2e-eks): pointing `$DS_PROMETHEUS` at that Prometheus, e.g.
  `http://observability-kube-prometh-prometheus.observability.svc:9090`, also
  works. To make it selectable in `gardenio-grafana`, add it as a second
  Grafana datasource (provisioning supports a list) before enabling
  `grafanaDashboard`. If you run a kube-prometheus-stack Grafana, just import
  the dashboard there and pick its Prometheus.

#### Empty-state honesty (when `$DS_PROMETHEUS` lacks these series)

If the dashboard is pointed at a datasource that does **not** carry the
cadvisor/KSM/reco-exporter series (e.g. `gardenio-vmsingle`), the panels **must
not fabricate a verdict**. A top-of-dashboard banner states the scrape gap, and:

- **Value stats** (requested / actual / recommended / over-provision factor)
  render a neutral `noValue` string (`no data — needs … scrape`) on the default
  `text` threshold — never a green tile — because an absent scrape is *unknown*,
  not *zero*.
- **Guardrail count stats** (OOMKills-1h, pods-near-limit, VPAs-without-reco)
  keep an `or vector(0)` fallback so a genuine "scrape live, count is zero"
  reads a healthy green **0** — but that fallback is **gated on scrape
  liveness** (`… or (vector(0) and (count(<liveness metric>) > 0))`). When the
  underlying scrape is entirely absent the gate is false, the fallback drops
  out, and the tile falls through to its neutral `noValue`. This is what
  distinguishes a **fabricated 0** (scrape absent) from a **genuinely observed
  0** (scrape live, nothing to report).
- **`VPA recommender up`** is the deliberate exception: it stays `or vector(0)`
  → **DOWN/red** when no recommender heartbeat is present, because "no heartbeat"
  *is* the unhealthy state for a liveness probe (not an unknown).

This mirrors the `dark-drift` dashboard, which faces the same
series-not-federated-into-the-VM situation.

---

## Per-class breakdown caveat

Crew pods are **bare pods** (no ownerRef) named `crew-*` and labelled
`gardenio.io/project` / `gardenio.io/role` / `gardenio.io/mode=crew` (spike
ka-0000006358). But cadvisor/KSM series do **not** carry those labels — the
`kube_pod_labels` collector is disabled on the cluster KSM — so the alerts and
dashboard select crew pods by `namespace` + the `pod=~"crew-.*"` name prefix,
and the "actual" breakdown is **per-pod**, not per-project. True per-project ×
per-role rollups become available once per-class VPA objects exist (scoping task
ka-0000001210); the recommended-target panel already breaks out by
`target_name`, which will be the class key then.

---

## Enable it

```yaml
# values/<env>.yaml — gardenio chart
agents:
  metrics:
    prometheusRule:
      vpaGuardrails:
        enabled: true
        severity: page          # page in prod, ticket in rwx, none in dev
        recommenderServiceMonitor:
          enabled: true         # needs the charts/vpa recommender deployed
        recommendationExporter:
          enabled: true         # surfaces the "recommended" line
        grafanaDashboard:
          enabled: true         # needs the observability Prometheus as a DS
```

Prerequisites: the `charts/vpa` recommender must be deployed
(`helm upgrade --install vpa charts/vpa -n gardenio …`, task ka-0000004038) and at
least one `VerticalPodAutoscaler` with `updateMode: Off` must target the crew
workload class, or the recommended/health series are empty.

---

## The alerts

| Alert | Fires when | `for:` | Meaning |
|---|---|---|---|
| **CrewOOMKilled** | a `crew-*` container's last termination is `OOMKilled` **and** a new restart happened in 15m | 0m | A killed agent run. The HARD GATE the rollout must never breach. **Rollback signal.** |
| **CrewMemoryNearLimit** | working set > `nearLimitFraction` (0.9) of the memory **limit** | 10m | OOM precursor — widen bounds before a run dies. |
| **VPARecommenderDown** | no ready vpa-recommender scrape target | 10m | Recommendations going stale silently. |
| **VPARecommenderNoRecommendation** | VPA objects match pods but carry no recommendation | 10m | Recommender up but erroring (bad metrics-server / unsupported config). |

`CrewMemoryNearLimit` reads against the **limit**, not the request, because the
OOMKiller enforces the limit — under VPA the request shrinks but the limit stays
the kill threshold.

---

## Enforce mode & eviction guards

Task **ka-0000003138**. The crew VPAs default to `updateMode: "Off"`
(recommendation-only). Flipping `vpaRightSizing.updateMode` to
`"InPlaceOrRecreate"` in the gardenio chart values lets the VPA *updater*
right-size **long-lived** pods (gardeners/threads) **in place** mid-run
(KEP-1287 / `InPlacePodVerticalScaling`, verified on runc **and** gVisor/runsc,
spike ka-0000005298) instead of evicting them. It only takes effect once the
updater is also deployed (`acme-helm-chart/charts/vpa`, the operator-gated live
rollout ka-0000008205). The bulk request **shrink** (8Gi → ~1.5Gi) still happens
at **spawn** via the controller stamp, never via a mid-run resize.

Crew pods are **bare pods** (no controller owner), so a VPA `Recreate` eviction
is an unrecoverable mid-task KILL — nothing recreates the pod. Because the mode
is `InPlaceOrRecreate` (not `Recreate`), in-place is *preferred*; three guards,
rendered **only** when `updateMode != "Off"`, make sure the `Recreate` fallback
can never kill a live run:

| Guard | Value knob | What it does |
|---|---|---|
| **Grow-only** | `vpaRightSizing.guards.growOnly` (default `true`) | `evictionRequirements: changeRequirement=TargetHigherThanRequests`. The updater may act **only when the recommendation is higher than the current request** — it can relieve memory pressure on a live run but can never shrink a running pod's request (the dangerous shrink → node-pressure-eviction / OOM path is removed). |
| **minReplicas** | `vpaRightSizing.guards.minReplicas` (default `1`) | Bounds how aggressively the updater acts. Defense-in-depth with the PDB. |
| **No-Recreate PDB** | `vpaRightSizing.pdb.enabled` (default `true`) | A per-class `maxUnavailable: 0` PodDisruptionBudget selecting the real crew-pod labels. The updater evicts via the eviction API, which a `maxUnavailable: 0` PDB **refuses** — so an in-place resize that's infeasible is simply **skipped** (pod keeps running, re-stamped at next spawn) instead of falling back to deleting the pod. Layers on the `karpenter.sh/do-not-disrupt=true` annotation already on every agent pod (which blocks the *other* involuntary-disruption source, Karpenter consolidation). |

VPA has no native "this agent is mid-task" hook, so the PDB eviction-API block is
the structural substitute for "don't kill a running agent". Together these mean:
in-place when possible, **skip** when not — never an eviction.

To enable (operator-gated; do **not** flip live without sign-off — ka-0000008205):

```yaml
# values/<env>.yaml — gardenio chart
vpaRightSizing:
  enabled: true
  updateMode: "InPlaceOrRecreate"   # was "Off"
  # guards.growOnly / guards.minReplicas / pdb.enabled default to safe values;
  # override only with a specific reason.
```

---

## Rollback

The right-sizing has **three** enforcement surfaces. Roll back whichever is live;
all return to recommendation-only (safe) or flat-request (status quo ante) with no
agent-run impact.

The in-place enforce machinery (task **ka-0000003138**) is gated behind a single
chart value — `vpaRightSizing.updateMode` in the gardenio chart — that **defaults
to `"Off"`**. Turning enforce on is a one-line values flip to
`"InPlaceOrRecreate"`; **rolling back is reverting that one line**. No code change,
no image rebuild. When the mode is `"Off"`, the chart renders recommendation-only
VPAs and **no** eviction-blocking objects at all (no PDBs, no `evictionRequirements`),
i.e. byte-identical to the pre-enforce posture.

### 1. Flip the gardenio chart back to `updateMode: Off` — primary rollback

This is the fastest and most complete rollback: it both stops the updater from
acting AND removes the mid-run eviction guards (the no-Recreate PDBs), returning
the namespace to recommendation-only.

```sh
# In acme-helm-chart values/<env>.yaml (gardenio chart), revert:
#   vpaRightSizing:
#     updateMode: "InPlaceOrRecreate"   # -> "Off"
# then re-sync the gardenio release (or let Argo/Flux reconcile):
helm upgrade --install gardenio <chart> -n gardenio -f values/<env>.yaml
```

After this, every crew VPA reads `updateMode: Off` and the
`vpa-norecreate-*` PDBs are deleted. Recommendations keep being produced (the
dashboard stays live); nothing is applied to running pods and new pods are still
right-sized at spawn by the controller stamp.

### 2. Disable the VPA updater (the controller that enforces)

The updater itself lives in the `acme-helm-chart/charts/vpa` subchart and is the
process that performs in-place resizes/evictions. With it off, the
`updateMode` on the VPA objects is inert regardless of value:

```sh
# In acme-helm-chart values/<env>.yaml, revert:
#   vpa:
#     updater:
#       enabled: false      # was true
# then re-sync:
helm upgrade --install vpa charts/vpa -n gardenio \
  -f charts/vpa/values.yaml -f charts/vpa/values/<env>.yaml
```

This stops all eviction/in-place resize immediately. Recommendations keep being
produced (dashboard stays live) but nothing is applied.

### 3. Flip VPA objects to `updateMode: Off` directly (in-cluster, no redeploy)

Belt-and-suspenders, or when you cannot redeploy a chart quickly — patch the
objects directly so even a running updater takes no action. (This is transient:
the next chart re-sync restores whatever the values file says, so still revert
the value per step 1.)

```sh
for v in $(kubectl get vpa -n gardenio -o name); do
  kubectl patch -n gardenio "$v" --type merge \
    -p '{"spec":{"updatePolicy":{"updateMode":"Off"}}}'
done
```

### 4. Restore the flat request (full revert to status quo)

If a pod already had its request shrunk and you want the original 8Gi back
immediately (rather than waiting for the next spawn to pick up the default),
the crew request is stamped at spawn by the controller podspec — reverting the
controller's resource override (or the VPA admission mutation, if the admission
controller was ever enabled) and respawning returns to the flat 8Gi/16Gi
request. No data is lost; agents are ephemeral and respawn with the default.

### Verify rollback

```sh
kubectl get vpa -n gardenio            # MODE column should read Off
kubectl get deploy -n gardenio | grep vpa-updater || echo "updater gone (expected)"
kubectl get pdb -n gardenio | grep vpa-norecreate || echo "no-Recreate PDBs gone (expected after step 1)"
# Watch the dashboard: 'request' returns to the flat line; CrewOOMKilled clears.
```

---

## Recommender health

If `VPARecommenderDown` / `VPARecommenderNoRecommendation` fires:

```sh
kubectl get deploy -n gardenio vpa-recommender
kubectl logs -n gardenio deploy/vpa-recommender --tail=50      # runs at --v=4
# operational metrics (via the recommender ServiceMonitor):
#   vpa_recommender_vpa_objects_count{matches_pods,has_recommendation,update_mode}
#   vpa_recommender_metric_server_responses   # metrics-server source health
```

A common cause is the metrics-server source being unavailable — the recommender
derives usage from `tenant-system-metrics-server` (the chart sets
`metrics-server.enabled: false` deliberately to avoid a second one).
