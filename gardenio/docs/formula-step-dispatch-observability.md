# Formula step-dispatcher observability

Operator runbook for the **formula step-role dispatcher**
(`gardenio/controller/internal/bridge/formula_step_dispatch.go`, instrumentation
task ka-0000004839).

## Why this exists

The dispatcher spawns a role-matched agent when a formula step's blocking deps
clear. Its whole reason to exist is preventing **silent loss** of a handoff:
FSM-472 was a gate that cleared into a void — the fix step never ran, no error,
no nudge, and a human noticed 50 minutes later by asking in Slack.

A loss-prevention component that itself runs silently is the wrong default.
Before this instrumentation, a spawn was a single `Info` log line and every
decline was `Debug`. Prod happens to run at debug level so the declines *are* in
the logs — but buried one-per-event in the fleet-wide firehose, not
aggregatable. An operator could not answer the two questions that matter:

1. **Did it spawn anything today?** (and for which lane?)
2. **If not, why not?**

Concretely: gap **G1** — all three opt-in steps of a formula were unreachable —
would have been obvious the instant a flat-zero spawn counter sat beside a
climbing decline counter. It instead took a code trace to find.

## The metrics

`trigger-engine` hosts the dispatcher and exposes the default Prometheus
gatherer on its internal `:9092` `/metrics` endpoint
(`cmd/trigger-engine/main.go` `buildMetricsServer`). Two series
(`formula_step_dispatch_metrics.go`):

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `gardenio_formula_step_dispatch_spawned_total` | counter | `role`, `formula`, `project` | one increment per agent the dispatcher actually spawned |
| `gardenio_formula_step_dispatch_declined_total` | counter | `reason` | one increment per candidate step NOT dispatched, keyed by a bounded reason code |

Both series are bounded by construction: the decline `reason` is a hard
compile-time enum (an out-of-enum value routes to `unknown`, never a new label),
and the spawn labels are small stable catalogs (a step declares one role and one
project; `formula` is a bead id from the dozens-large formula catalog). No raw
per-bead id ever becomes a label value.

### The decline reasons

Each reason maps to exactly one early-return site in the dispatch path.

**Expected / benign** (the dispatcher working correctly):

| `reason` | Fires when |
|---|---|
| `not_opted_in` | the formula did not set `suggest_new_agent` on this step — **most** formulas intend one driver to walk the whole chain, so this dominates a healthy window |
| `still_blocked` | at least one sibling blocking dep is still open (the step blocked on 3 siblings, only 1 closed) |
| `already_dispatched_process` | in-process dedup — an SSE replay or a burst of sibling closes for a step this process already handled |
| `already_stamped` | the durable `dispatched_agent` stamp is already present (a prior process dispatched it) |
| `already_assigned` | a live driver or human already holds the step |
| `not_formula_step` | the closed bead's dependent is not a poured formula step (no `source_step_id`) |
| `not_open` | the candidate step is not `open` |
| `no_fields` | the candidate has no fields at all |
| `no_role` | the step opted in but declares no role to dispatch to (a formula-authoring bug worth chasing if sustained) |
| `lost_race` | two goroutines raced the same step; one lost the in-process claim (harmless — the winner spawns) |

**Error-class** (a step that *should* have dispatched didn't — watch these):

| `reason` | Fires when |
|---|---|
| `guard_refused` | the fleet spawn guard (rate limit / circuit breaker) refused — the step will retry on a later close event |
| `spawn_failed` | `SpawnAgent` itself errored; the durable stamp was rolled back so the step is retryable |
| `stamp_failed` | writing the `dispatched_agent` stamp failed, so we did not spawn |
| `hydrate_failed` | the candidate step bead could not be re-read from the daemon |
| `listdeps_failed` | listing the closed bead's dependents failed — the **whole** close event is lost for dispatch |
| `block_check_failed` | the blocking-dep check errored (fail closed) |
| `unknown` | a caller passed an unrecognized reason code — a **code bug**; should never appear |

## The dashboard

**Gardenio — Formula Step Dispatcher** (`uid: gardenio-formula-step-dispatch`,
`helm/gardenio/dashboards/formula-step-dispatch.json`). Ships in the controller
`grafana_dashboard=1` ConfigMap; reaches live Grafana on the next tagged release
or an out-of-band `scripts/sync-dashboards.sh` (see `dashboard-sync.md`).

- **Top row** answers "did it spawn?": spawn count and decline count over the
  range, spawns/hour by role, spawns/hour by formula.
- **Second row** answers "why not?": declines/hour stacked by reason, a
  reason-share donut, and an **error-class-only** panel isolating the reasons
  that mean a real handoff was dropped.

### The G1 signature to watch for

> spawn count **flat at 0** while the decline count is **climbing** — the
> dispatcher is declining every candidate. Open the by-reason panel: if the
> climb is entirely `not_opted_in`, that is expected (no formula opted a step
> in). If a lane you *expect* to be active shows zero spawns while a formula
> that declared `suggest_new_agent` steps never appears in the by-formula panel,
> that is G1 — the opt-in steps are unreachable.

### Current state: the emitter is LIVE and reconciles (ka-0000000749)

As of **2026-08-14** the spawn counter is emitting and correct. This supersedes
the earlier "known empty gap" framing (bug **ka-0000007851**, filed 2026-07-28),
which was accurate *at the time* — back then no formula declared
`suggest_new_agent`, so every candidate declined at `not_opted_in` before
`dispatch()` and the counter had zero series. That is no longer true:

- **8 formulas** now declare `suggest_new_agent` steps, and the dispatcher has
  spawned agents against them. `gardenio_formula_step_dispatch_spawned_total`
  carries **5 series over 30d** (`increase[7d]=3`, `increase[24h]=1`), most
  recently 2026-08-13 (`role=monorepo-fix`, `formula=ka-0000006208`,
  `project=monorepo`).
- **Reconciliation agrees** (advice ka-0000005924): **derivation B** —
  `kd count --all-types --all-projects -f dispatched_by=formula-step-dispatcher`
  — is **also 5**. Derivation A == derivation B == 5, so the count is the **true**
  value, not an undercount.

**Why the counter used to read "No data" — and the durable fix (bug
ka-0000003699).** A Prometheus `CounterVec` publishes **no series at all** until
its first `WithLabelValues()` call. A formula spawn is **bursty and infrequent**
(a handful per week) and trigger-engine is a long-running Deployment that
**restarts on every release** (multiple per day), so a freshly-restarted pod has
almost always emitted **zero** spawns — and therefore **zero** `spawned_total`
series — at any given scrape. An **instantaneous** series-presence probe —
`count(spawned_total)`, which is exactly what the dashboard-accuracy auditor's C5
existence check (`kbeads charts_audit.go` `ClassifyExistence`) runs — then reads
**0 series**, indistinguishable from a dead emitter. This false negative was
re-found across **three** GRADE cycles (ka-0000007851 → ka-0000000749 →
ka-0000003699) even though the emitter was always correct and reconciled A == B.

The fix: the dispatcher **seeds a zero-valued baseline series at startup**
(`seedFormulaStepDispatchMetrics`, called from `NewFormulaStepDispatcher`), using
a sentinel `role=formula=project="none"` label set that no real spawn ever
carries. The series **name is therefore present from t=0**, so
`count(spawned_total)` is never 0 and the C5 existence check passes on the merit
of the series existing. The seed contributes **0** to every `increase()`/`sum()`
over any window (increase of a constant-0 counter is 0), so it changes no
headline value and preserves the A == B reconciliation. This mirrors
`initGitLabWebhookDeadmanMetrics` (`gitlab_webhook_deadman.go`), which seeds the
same way so `absent()`/`increase()` have a baseline sample on a fresh pod.

Even so, always query spawns with `increase()` over a window, never the raw
instantaneous series — a narrow window (e.g. `increase[1h]`) legitimately
contains no spawn, so widen the dashboard range to see them.

The headline panels stay honest for a genuinely empty window: the two stat panels
coalesce the empty vector with `or vector(0)` so they read a real **0**, and every
spawn-counter panel's `noValue` affirms the emitter is **live** and reconciles
with the `dispatched_by` bead count — so an empty window reads as *dispatcher idle
/ all-declined in this window*, not a broken board. The panel-3 window was widened
from `[1h]` to `[$__range]` so the reconciliation stat matches the window you run
derivation B over.

## Reconciliation (advice ka-0000005924 — measure it two ways)

The spawn counter is **derivation A** of "how many agents did the dispatcher
mint". The independent **derivation B** is the count of beads carrying
`dispatched_by=formula-step-dispatcher` — the durable field `dispatch()` stamps
on exactly the steps it successfully spawned for (the stamp is rolled back on
spawn failure, so a bead carries it **iff** a spawn succeeded).

Prometheus cannot query the beads daemon, and a daemon-polling gauge on a
long-lived controller risks the OOM / silent-truncation class of advice
ka-0000003555. So derivation B is an operator `kd` query, run on demand and
compared against the counter over the same window:

```bash
# Derivation A — the counter, over the last 24h (from Grafana or the API):
#   sum(increase(gardenio_formula_step_dispatch_spawned_total[24h]))

# Derivation B — beads stamped by the dispatcher in the same window.
# Let the SERVER filter + count: --all-types (the stamp rides kind=task/agent
# beads the default issue-kind scoping hides) and --all-projects (dispatched
# steps span projects). A client-side `kd list --json | jq` scan returns a
# SILENT false-empty because --json is a single 20-row page (advice ka-0000001856).
kd count --all-types --all-projects \
  -f dispatched_by=formula-step-dispatcher \
  --created-after 24h
```

Within one process lifetime the two must agree. Counter **resets on restart**
are expected — compare over a window with `increase()`, not the raw total, and
keep the window inside the current process's uptime for an exact match.

A **persistent divergence** is a bug in one derivation:

- counter **>** beads → a spawn incremented the counter but its stamp is missing
  (e.g. a stamp rollback that shouldn't have run), or the window crossed a
  restart;
- beads **>** counter → a bead carries the stamp but no spawn was counted
  (a stamp left behind by a spawn that later failed its rollback — the
  `spawn_failed`-with-failed-rollback path the code logs at `Error`).

## Related

- `assistant-pane-smoke-check.md` — the sibling "silent absence" runbook this
  one mirrors.
- `dashboard-sync.md` — how dashboard JSON reaches live Grafana between releases.
- Advice ka-0000005924 (two-way measurement), ka-0000003555 (don't OOM the
  controller with unpaged bead scans).
