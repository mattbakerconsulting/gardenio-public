# Phase 0: idle:active agent ratio & idle memory waste measurement

**Bead:** [ka-0000006778](../../) · **Parent epic:** ka-0000003796 (Research: agent-substrate
gains & integration for gardenio)

## Why

The agent-substrate work (freeze/thaw, epic ka-0000003796, Phases 1-4) reclaims the pod an
**idle** agent is holding — default `8Gi` memory request
(`podmanager.DefaultMemoryRequest`) — by checkpointing the process out of the cluster and
thawing it back when the agent has work again. Before we can claim a saving, Phase 0 measures
the baseline the substrate has to beat: **how many agents are idle vs active, and how much
reserved memory idle agents waste while doing no work.**

## Where the measurement lives

`gardenio/controller/internal/reconciler/idle_metrics.go`, emitted once per reconcile pass from
`Reconciler.recordIdleMetrics`.

The reconcile pass is the one place that already holds **both halves of the join** in memory
every cycle:

- the **desired agent beads** (which carry `agent_state`), and
- the **live pods** (which carry the real memory *requests* kubelet reserves).

Deriving the measurement there reuses that in-hand data with **zero extra daemon or K8s
queries** — the same discipline `DeferredMetrics` follows and the paging/OOM guard advice
(ka-0000003555) demands. Emission is a pure `Reset` + `Set` over the two maps, so it cannot
page, fan out, or OOM.

## Metrics

Both are `gardenio`-namespaced gauges labelled `{project, activity}` where `activity` ∈
`{active, idle}`.

| Metric | Type | Meaning |
|---|---|---|
| `gardenio_agents_by_activity` | gauge | Desired agents split by activity. |
| `gardenio_agent_reserved_memory_bytes` | gauge | Sum of pod memory **requests** (bytes kubelet reserves, not live RSS) over agents that have a live pod, split by activity. |

### The two headline numbers (PromQL)

Idle:active agent ratio, fleet-wide:

```promql
sum(gardenio_agents_by_activity{activity="idle"})
  /
sum(gardenio_agents_by_activity{activity="active"})
```

Idle memory waste (bytes reserved by agents doing no work), fleet-wide:

```promql
sum(gardenio_agent_reserved_memory_bytes{activity="idle"})
```

Both series are also emitted **per project** (the `project` label), and each project always
emits an explicit `0` for the empty bucket, so the ratio is a well-defined division rather than
a divide-by-absent.

## What counts as "active" vs "idle"

`classifyActivity` maps `agent_state` to `active` for the enumerated work-in-progress set —
`working`, `spawning`, `assigning`, `resuming` — and to **idle** for **everything else**:
`idle`, `prewarmed`, `queued`, `yielding`, `rate_limited`, `budget_exceeded`, `stopping`, and
any unknown future state.

- **`yielding` is idle on purpose.** A yielding agent is blocked on a human decision (`gb
  yield`) and holds its full pod reservation while doing no work — exactly the waste Phase 1
  (coop snapshot-suspend for idle/yielded agents, ka-0000002921) targets.
- **Unknown states default to idle** — the conservative choice for a *waste* measurement: a
  new resting state we forget to enumerate surfaces as waste to investigate rather than
  silently vanishing from the idle figure.

## Reserved-memory accounting rules

- Only agents with a **live (non-terminal) pod** contribute reserved bytes. A deferred/podless
  agent (e.g. `queued`, capacity-deferred) is *counted* but reserves nothing yet, so it adds to
  `agents_by_activity` but not to `agent_reserved_memory_bytes`.
- Pods in `Failed`/`Succeeded` phase are excluded — their reservation is releasing this pass, so
  booking them would double-count against the recreated pod.
- Per-pod memory is the sum of **app-container** memory requests (init containers are
  short-lived and excluded).

## Second, independent cross-check

Per advice ka-0000005924 ("measure every important quantity two independent ways"), the idle
count has an independent derivation already in the fleet:
`gardenio_agents_active{state}` (bead-only poller,
`internal/metrics/metrics.go`) counts agents by raw `agent_state`. Summing its idle-side states
must reconcile with `gardenio_agents_by_activity{activity="idle"}`:

```promql
# derivation A (this measurement, reconciler, pod-join)
sum(gardenio_agents_by_activity{activity="idle"})

# derivation B (existing bead-only poller) — should agree
sum(gardenio_agents_active{state=~"idle|prewarmed|queued|yielding|rate_limited|budget_exceeded|stopping"})
```

A persistent divergence between A and B is a bug in one of them (a missed state in the
`classifyActivity` set, a poller/reconciler list skew, or a stale series) — not noise to
ignore. The controller also logs the fleet-wide split each pass on the `reconcile pass
complete` line (`active_agents`, `idle_agents`, `idle_reserved_mem_bytes`) so the measurement is
visible without a Grafana round-trip.
