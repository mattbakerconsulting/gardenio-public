# Grafana GRADE — New panel ideas (Depth), iteration 2026-08-19T07:23:00Z

Each idea is grounded in a metric with **verified live series** in gardenio-vmsingle
(counts checked this iteration). Ordered by operator value. Format: **title** — panel-type —
`PromQL` — *the operator question it answers*.

1. **Compaction pressure leaderboard** — table/bar — `topk(15, gardenio_agent_compactions_current)`
   (3 live) + rate `sum by (project,role) (rate(gardenio_session_compactions_total[1h]))` —
   *Which agents are burning turns on context compaction right now?* Compaction is a direct
   token-cost and latency tax; no board surfaces it per-agent today.

2. **Advice-corpus weight vs prime budget** — timeseries + stat —
   `sum(gardenio_advice_catalog_bytes)` and `topk(10, gardenio_advice_catalog_bytes)` (164 live) —
   *Is the advice corpus bloating the prime preamble?* Every advice bead is injected into every
   session; this is the lever behind boot-context growth on context-observability.

3. **Operator cost leaderboard** — bar/table —
   `topk(10, gardenio_operator_cost_usd_total)` (14 live) —
   *Which human operator is driving the most agent spend?* cost-overview breaks down by
   project/account/agent but never by the human who triggered the work.

4. **Pod-phase transition Sankey / state-timeline** — state-timeline —
   `sum by (phase) (rate(gardenio_agent_pod_phase_transitions_total[5m]))` (23 live) —
   *How is the fleet flowing through Pending→Running→Succeeded/Failed?* Surfaces stuck-Pending
   backlogs before they become capacity incidents.

5. **Reconcile-loop liveness dead-man** — stat + timeseries —
   `rate(gardenio_reconcile_total[10m])` (1 live) —
   *Is the controller reconciler actually ticking?* A second, independent control-plane liveness
   signal complementary to the probe-loop dead-man (measure-two-ways, advice ka-0000005924).

6. **Pending-pod saturation** — timeseries with threshold —
   `gardenio_agent_pods_pending` (1 live) —
   *Are we running out of node capacity to place agent pods?* Pairs with the deferred-wait SLO.

7. **Mention cold-respawn delivery rate** — timeseries —
   `sum by (source,outcome) (rate(gardenio_mention_delivery_total[5m]))` (2 live) —
   *Are @gardenio mentions being delivered, and how fast?* The mention path is a top operator
   entry point with no dedicated liveness panel.

8. **Stale-task passthrough rate** — timeseries —
   `sum by (source) (rate(gardenio_gitlab_mention_stale_task_passthrough_total[1h]))` (4 live) +
   the jira sibling — *How often do mentions land on a stale/closed task?* A quality signal for
   the mention router.

9. **Node lifecycle churn by reason** — timeseries (already on involuntary-deaths as a sub-panel;
   promote to a fleet-capacity board) — `sum by (reason) (rate(gardenio_node_lifecycle_events_total[1h]))`
   (4 live) — *Is Karpenter/node churn correlating with involuntary agent deaths?*

10. **Prewarm-pool assign throughput vs hit-rate** — timeseries —
    `sum(rate(gardenio_prewarmed_assigns_total[5m]))` (7 live) with warm-hit ratio overlay —
    *Is the prewarmed pool actually saving spin-up time, and at what fill rate?* Companion to the
    resume-latency warm-path gate.

11. **Account-warming timeline correlated with utilization** — state-timeline —
    `gardenio_account_warming` (9 live) overlaid on 5h-window utilization —
    *Which accounts enter soft-failover warming, and does it track the rate-limit ceiling?*

12. **LLM/tool latency by role heatmap** — heatmap —
    `sum by (le,role) (rate(gardenio_user_request_latency_seconds_bucket[$__rate_interval]))`
    (90 live) — *Do certain roles (gardeners vs crew) see systematically worse round-trip latency?*
    agent-latency-distribution has the fleet view but not the per-role cut.

13. **Span-metrics RED by live service** — (fix + extend perf-traces) —
    `histogram_quantile(0.95, sum by (le,service_name) (rate(span_duration_milliseconds_bucket{span_kind="SPAN_KIND_SERVER"}[$__rate_interval])))`
    (12,084 live) — *What is the p95 server-span latency per live ACME service?* Requires fixing
    the stale `$service=keycloak` default first (see bug below).

Recommended Depth follow-up: a small **"Fleet Capacity & Placement"** board carrying #4/#6/#9
(pod-phase flow, pending saturation, node churn) — the placement-side companion to
control-plane-liveness.
