package bridge

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Prometheus instrumentation for the formula step-role dispatcher
// (formula_step_dispatch.go, task ka-0000004839).
//
// WHY THIS EXISTS. The dispatcher's whole purpose is preventing SILENT loss of
// a formula handoff (FSM-472: a gate cleared into a void and a human noticed 50
// minutes later). Yet the dispatcher itself shipped SILENT: a spawn was a single
// Info line and every decline was Debug. Prod happens to run at debug level so
// declines ARE in the logs — but buried in the fleet-wide firehose, one line per
// event, not aggregatable. An operator could not answer the two questions that
// matter for a loss-prevention component: "did it spawn anything today?" and
// "if not, why not?".
//
// Concretely, gap G1 — all three opt-in steps of a formula were unreachable —
// would have been obvious the instant a flat-zero spawn counter sat next to a
// nonzero decline counter. It instead took a code trace to find.
//
// TWO SERIES:
//
//   - gardenio_formula_step_dispatch_spawned_total (role, formula, project):
//     one increment per agent the dispatcher actually spawned. "Did the
//     monorepo-fix lane spawn anything today?" is increase(...{role="monorepo-fix"}[1d]).
//
//   - gardenio_formula_step_dispatch_declined_total (reason): one increment
//     every time a candidate step was NOT dispatched, keyed by a BOUNDED reason
//     code. "Why didn't my step fire?" is a topk() over reason.
//
// RECONCILIATION (advice ka-0000005924 — measure every important quantity two
// independent ways). The spawn counter is one derivation of "how many agents did
// the dispatcher mint". The SECOND, independent derivation is the count of beads
// carrying dispatched_by=formula-step-dispatcher, the durable field dispatch()
// stamps on exactly the steps it successfully spawned for (the stamp is rolled
// back on spawn failure, so a bead carries it iff a spawn succeeded). Within one
// process lifetime the two must agree:
//
//	sum(increase(gardenio_formula_step_dispatch_spawned_total[<window>]))
//	  ==  count(beads created in <window> with dispatched_by=formula-step-dispatcher)
//	      (kd list --json | jq '[.[]|select(.fields.dispatched_by=="formula-step-dispatcher")]|length')
//
// A persistent divergence is a bug in one derivation — a spawn that stamped but
// did not count, or a stamp left behind by a spawn that later failed its
// rollback. Counter resets on restart are expected; compare over a window with
// increase(), not the raw total.
//
// CARDINALITY. Both series are bounded by construction. The decline reason is a
// HARD compile-time enum (declineReason* below); recordDecline routes anything
// outside it to declineUnknown rather than minting a new label. The spawn
// labels are all small stable catalogs — a step declares exactly one role and
// one project, and source_formula is a bead id from the (dozens-large) formula
// catalog — so no raw per-bead id ever becomes a label value.
//
// Registered globally against the controller's default registerer (promauto),
// matching every other bridge metric (assistant_event_metrics.go,
// cicd_metrics.go, gitlab_mention_metrics.go). trigger-engine is the binary that
// hosts the dispatcher, and it exposes the default gatherer on its :9092
// /metrics endpoint (cmd/trigger-engine/main.go buildMetricsServer), so these
// series are scraped there.
var (
	// formulaStepDispatchSpawnedTotal counts agents the dispatcher spawned,
	// faceted by the step's resolved role, its source formula, and its project.
	// A flat-zero series next to a nonzero decline series is the G1 signature.
	formulaStepDispatchSpawnedTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gardenio_formula_step_dispatch_spawned_total",
			Help: "Agents spawned by the formula step-role dispatcher, labelled by resolved role, source formula, and project. Reconciles against count(beads with dispatched_by=formula-step-dispatcher) — two independent derivations of the same quantity (advice ka-0000005924). Flat-zero here beside a nonzero decline counter is the 'all opt-in steps unreachable' (G1) signature.",
		},
		[]string{"role", "formula", "project"},
	)

	// formulaStepDispatchDeclinedTotal counts candidate steps the dispatcher did
	// NOT spawn for, keyed by a bounded reason code. This is the "why didn't my
	// step fire?" series — every early return in the dispatch path records here.
	formulaStepDispatchDeclinedTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gardenio_formula_step_dispatch_declined_total",
			Help: "Candidate formula steps NOT dispatched, labelled by a bounded reason code (not_opted_in/no_role/still_blocked/already_stamped/guard_refused/spawn_failed/…). topk by reason answers 'why didn't the dispatcher spawn?' — the decline visibility the component shipped without. NOTE: role_unbacked is the one facet that co-occurs WITH a spawn (a soft role-config warning), not instead of one.",
		},
		[]string{"reason"},
	)
)

// Bounded decline-reason enum. Each constant maps to exactly one early-return
// site in the dispatch path (OnStepClosed / considerStep / stepDispatchEligible
// / dispatch). recordDecline refuses anything outside this set so a future
// caller cannot inflate cardinality — the same HARD-enum guard the other bridge
// counters use (assistant_event_metrics.go).
const (
	// OnStepClosed: could not even list the closed bead's dependents, so the
	// whole close event is lost for dispatch purposes.
	declineListDepsFailed = "listdeps_failed"

	// considerStep: cheap in-process dedup — this step was already dispatched by
	// this process (an SSE replay or a burst of sibling closes).
	declineAlreadyDispatchedProcess = "already_dispatched_process"
	// considerStep: the candidate step bead could not be hydrated.
	declineHydrateFailed = "hydrate_failed"
	// considerStep: the blocking-dep check itself errored (fail closed).
	declineBlockCheckFailed = "block_check_failed"
	// considerStep: at least one blocking dep is still open.
	declineStillBlocked = "still_blocked"

	// stepDispatchEligible sub-reasons — the enumerated eligibility gates.
	declineNotOpen         = "not_open"         // status is not open
	declineNoFields        = "no_fields"        // no fields at all
	declineNotFormulaStep  = "not_formula_step" // no source_step_id
	declineNotOptedIn      = "not_opted_in"     // suggest_new_agent not set
	declineNoRole          = "no_role"          // no step role to dispatch to
	declineAlreadyStamped  = "already_stamped"  // durable dispatched_agent stamp present
	declineAlreadyAssigned = "already_assigned" // a live driver/human already holds it

	// dispatch: the resolved step role had no backing type:role bead (and is not
	// a builtin) — the spawn STILL happened (roles are free strings, soft-warned
	// only), so this facet is a signal of a likely-typo'd/misconfigured role, NOT
	// a decline. It is counted alongside a successful spawn, so it is the one
	// reason code that can co-occur with a spawn increment rather than replace
	// one. topk() over reason surfaces a role catalog gap without a log dive.
	declineRoleUnbacked = "role_unbacked"

	// dispatch: lost the in-process claim race to a concurrent goroutine.
	declineLostRace = "lost_race"
	// dispatch: the fleet spawn guard refused (rate limit / circuit breaker).
	declineGuardRefused = "guard_refused"
	// dispatch: stamping dispatched_agent failed, so we did not spawn.
	declineStampFailed = "stamp_failed"
	// dispatch: SpawnAgent itself failed (stamp rolled back for retry).
	declineSpawnFailed = "spawn_failed"

	// declineUnknown is the catch-all bucket recordDecline routes an
	// unrecognized reason code into — a bounded landing spot so a caller bug
	// surfaces as a labelled count instead of either a dropped signal or an
	// unbounded new label.
	declineUnknown = "unknown"
)

// stepDispatchDeclineReasonValid reports whether reason is a member of the
// bounded decline-reason enum (declineUnknown included — it is a legal terminal
// bucket, not an error).
func stepDispatchDeclineReasonValid(reason string) bool {
	switch reason {
	case declineListDepsFailed,
		declineAlreadyDispatchedProcess,
		declineHydrateFailed,
		declineBlockCheckFailed,
		declineStillBlocked,
		declineNotOpen,
		declineNoFields,
		declineNotFormulaStep,
		declineNotOptedIn,
		declineNoRole,
		declineAlreadyStamped,
		declineAlreadyAssigned,
		declineRoleUnbacked,
		declineLostRace,
		declineGuardRefused,
		declineStampFailed,
		declineSpawnFailed,
		declineUnknown:
		return true
	default:
		return false
	}
}

// recordDecline increments the decline counter for one non-dispatch decision.
// An out-of-enum reason is recorded under declineUnknown (never dropped) — this
// is a loss-prevention component, so undercounting a decline is worse than a
// single bounded catch-all bucket. All in-tree callers pass a constant, so the
// unknown path is defense-in-depth.
func (d *FormulaStepDispatcher) recordDecline(reason string) {
	if !stepDispatchDeclineReasonValid(reason) {
		d.logger.Warn("step dispatch: recording decline under 'unknown' — unrecognized reason code",
			"reason", reason)
		reason = declineUnknown
	}
	formulaStepDispatchDeclinedTotal.WithLabelValues(reason).Inc()
}

// stepDispatchSeedLabel is the sentinel label value the startup baseline series
// carries. It is deliberately DISTINCT from stepDispatchLabelOrUnknown's
// "unknown" (which marks a REAL spawn whose source_formula/project was empty) so
// the baseline can never be mistaken for an actual dispatch: no real spawn ever
// carries role/formula/project all equal to this value.
const stepDispatchSeedLabel = "none"

// seedFormulaStepDispatchMetrics touches the spawn counter to a zero baseline so
// the series NAME is present in the store the instant the trigger-engine pod is
// first scraped — before any formula step has actually been dispatched.
//
// WHY THIS EXISTS (bug ka-0000003699, the 3rd re-find of ka-0000007851 /
// ka-0000000749). A Prometheus CounterVec publishes NO series at all until its
// first WithLabelValues() call. A formula spawn is a BURSTY, infrequent event (a
// handful per week) and trigger-engine is a long-running Deployment that RESTARTS
// on every release (multiple per day), so a freshly-restarted pod has almost
// always emitted zero spawns — and therefore zero spawned_total series — at any
// given scrape. The dashboard-accuracy auditor's C5 existence check
// (kbeads charts_audit.go ClassifyExistence) probes whether the metric NAME is
// present in the store; an absent name on a non-empty panel is MISSING-SERIES.
// That is why the same "spawned_total is dark" bug kept being re-found across
// three GRADE cycles even though recordSpawn (below) is demonstrably correct and
// reconciles with the dispatched_by bead count (A == B): the emitter was never
// broken, the series was simply ABSENT between rare spawns.
//
// Seeding a single zero-valued sentinel series makes the name permanently
// present, so count(spawned_total) is never zero and the C5 check passes on the
// merit of the series existing — not on the fragile empty-state-text heuristics
// the two prior interim fixes leaned on. The seed contributes 0 to every
// increase()/sum() over any window (increase of a constant-0 counter is 0), so
// it changes no headline value and preserves the spawn/dispatched_by
// reconciliation (advice ka-0000005924). It is the exact pattern
// initGitLabWebhookDeadmanMetrics (gitlab_webhook_deadman.go) uses to give
// absent()/increase() a baseline sample on a fresh pod.
//
// Idempotent (Add(0) on an existing series is a no-op accumulation), so it is
// safe to call from the constructor on every dispatcher construction.
func seedFormulaStepDispatchMetrics() {
	formulaStepDispatchSpawnedTotal.WithLabelValues(
		stepDispatchSeedLabel, stepDispatchSeedLabel, stepDispatchSeedLabel,
	).Add(0)
}

// recordSpawn increments the spawn counter for one agent the dispatcher minted.
// Empty labels are coerced to "unknown" so a missing source_formula/project
// never produces a blank, un-selectable series (role is guaranteed non-empty by
// the eligibility gate, but coerced for symmetry).
func (d *FormulaStepDispatcher) recordSpawn(role, formula, project string) {
	formulaStepDispatchSpawnedTotal.WithLabelValues(
		stepDispatchLabelOrUnknown(role),
		stepDispatchLabelOrUnknown(formula),
		stepDispatchLabelOrUnknown(project),
	).Inc()
}

// stepDispatchLabelOrUnknown returns v, or "unknown" when v is empty, so a label
// value is never the empty string.
func stepDispatchLabelOrUnknown(v string) string {
	if v == "" {
		return "unknown"
	}
	return v
}

// ─────────────────────────────────────────────────────────────────────────────
// Dispatch latency (task ka-0000006705)
// ─────────────────────────────────────────────────────────────────────────────
//
// WHY A THIRD SERIES. The two counters above answer "did it spawn?" and "if
// not, why not?" — both BOOLEAN questions. Neither answers the question an
// operator actually asks when the dispatcher is spawning but feels slow: HOW
// LONG did a ready step wait for its agent? A GRADE richness pass wanting a
// distribution panel on this board had to borrow an unrelated prewarm-assign
// histogram, because the dispatcher published no latency series of its own.
//
// WHAT IS MEASURED. One observation per SUCCESSFUL spawn, of the interval
//
//	(the instant the step became workable)  ->  (the instant its agent existed)
//
// The left edge is NOT "when the dispatcher noticed". It is the daemon-side
// timestamp of the LAST blocking dep to clear — max(closed_at) over the step's
// blocking deps, which stillBlocked already hydrates on the dispatch path, so
// the readiness instant costs zero extra daemon round-trips. That edge is the
// honest one: a step is ready the moment its last blocker closes, whether or
// not this process heard about it. Measuring from event ingress instead would
// hide exactly the delay the FSM-472 failure class is made of (a readiness
// signal that reached nobody), which is the whole reason the dispatcher exists.
//
// TWO PATHS, ONE FAMILY. The `path` label separates the two structurally
// different ways a spawn happens, because pooling them would make every
// quantile meaningless:
//
//   - path=event — the SSE close/update event drove it. Latency here is
//     daemon-publish + stream delivery + a handful of dispatcher round-trips:
//     healthy is sub-second to a few seconds. This is the number that answers
//     "is the dispatcher keeping up?".
//   - path=backfill — the periodic sweep (BackfillReadySteps, 2-minute default)
//     drove it. Latency here is bounded BELOW by nothing and above by however
//     long the step was stranded, so it legitimately runs to minutes (a
//     guard-refused step waiting for budget) or hours/days (a step whose close
//     event was missed entirely and which the startup sweep recovered). A fat
//     backfill tail is not noise — it is the recovery path doing its job, and
//     the volume of it is the signal that the event path dropped something.
//
// RARE-EVENT CAVEAT (mirrors respawn_phase_metrics.go). A formula spawn is a
// bursty, handful-per-week event, so histogram_quantile over a short window is
// jumpy and often empty. The robust companion reads on the same data are the
// windowed mean (increase(_sum)/increase(_count)) and the bucket heatmap; the
// board carries both next to the quantiles for exactly this reason.
//
// RECONCILIATION (advice ka-0000005924). A spawn either produces a latency
// observation or is accounted for as unmeasurable — never silently dropped:
//
//	sum(increase(gardenio_formula_step_dispatch_latency_seconds_count[w]))
//	  + sum(increase(gardenio_formula_step_dispatch_latency_unmeasured_total[w]))
//	  ==  sum(increase(gardenio_formula_step_dispatch_spawned_total[w]))
//
// A persistent divergence means a spawn path bypassed the readiness accounting.
// The unmeasured counter is what keeps the histogram's _count honest as a
// denominator instead of a mystery undercount.
var (
	// formulaStepDispatchLatencySeconds is the readiness -> spawn latency
	// distribution, labelled by dispatch path. The bucket set spans sub-second
	// (a healthy event-path dispatch) to an hour (a step recovered late by the
	// sweep) so both paths land in real buckets rather than the +Inf overflow.
	formulaStepDispatchLatencySeconds = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "gardenio_formula_step_dispatch_latency_seconds",
			Help:    "Formula step dispatch latency (seconds) from the step becoming workable (max closed_at over its blocking deps) to its agent being spawned, labelled by path (event=SSE-driven, healthy sub-second-to-seconds; backfill=periodic sweep recovery, legitimately minutes-to-hours). _count + gardenio_formula_step_dispatch_latency_unmeasured_total reconciles against gardenio_formula_step_dispatch_spawned_total (advice ka-0000005924). Quantiles are jumpy at this event rate — read the windowed mean and the heatmap alongside them.",
			Buckets: []float64{0.25, 0.5, 1, 2, 5, 10, 30, 60, 120, 300, 900, 1800, 3600},
		},
		[]string{"path"},
	)

	// formulaStepDispatchLatencyUnmeasuredTotal counts spawns for which no
	// readiness instant could be established, keyed by a bounded reason. These
	// are the spawns MISSING from the histogram — published so _count is a
	// trustworthy denominator rather than a silent undercount.
	formulaStepDispatchLatencyUnmeasuredTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gardenio_formula_step_dispatch_latency_unmeasured_total",
			Help: "Successful dispatches with NO latency observation, by bounded reason (no_blocking_deps/blocker_time_missing/clock_skew). The accounting complement of gardenio_formula_step_dispatch_latency_seconds_count: the two sum to gardenio_formula_step_dispatch_spawned_total, so a gap in the histogram is always attributable instead of mysterious.",
		},
		[]string{"reason"},
	)
)

// Bounded dispatch-path enum for the latency histogram's `path` label. Exactly
// two values, decided by whether the dispatch was driven by an SSE event or by
// the backfill sweep (stepDispatchPathFor).
const (
	stepDispatchPathEvent    = "event"
	stepDispatchPathBackfill = "backfill"
)

// Bounded reason enum for formulaStepDispatchLatencyUnmeasuredTotal. Each value
// is a distinct, real way a successful spawn can carry no measurable readiness
// instant — kept separate because they mean different things to an operator.
const (
	// The step had no blocking dep at all, so there is no "it became ready"
	// instant to measure from. Reachable via the backfill sweep, which can
	// dispatch an eligible, un-stamped step that was never dep-chained (a
	// single-step formula, or a first step whose chain edge was never written).
	latencyUnmeasuredNoBlockingDeps = "no_blocking_deps"
	// A cleared blocker carried neither a parseable closed_at nor an updated_at,
	// so max(closed_at) would have been a guess. Refusing to guess keeps the
	// histogram free of fabricated samples.
	latencyUnmeasuredBlockerTimeMissing = "blocker_time_missing"
	// The readiness instant resolved to the FUTURE relative to this pod's clock
	// — daemon/pod clock skew. A negative sample would corrupt every quantile,
	// so it is dropped and counted here where it is diagnosable.
	latencyUnmeasuredClockSkew = "clock_skew"
)

// stepDispatchPathFor maps a dispatch's triggering-close id to the bounded
// `path` label: the backfill sentinel means the sweep drove it, anything else
// is a real bead id delivered by the SSE event path.
func stepDispatchPathFor(closedID string) string {
	if closedID == stepDispatchBackfillSentinel {
		return stepDispatchPathBackfill
	}
	return stepDispatchPathEvent
}

// seedFormulaStepDispatchLatencyMetrics publishes both latency families at a
// zero baseline so their NAMES exist in the store on a freshly-restarted
// trigger-engine pod — the same C5-existence problem seedFormulaStepDispatchMetrics
// solves for the spawn counter (bug ka-0000003699), and the same fix.
//
// Safe in a way the spawn counter's seed is not: a histogram child created with
// no observations has count=0 and every bucket 0, so it adds nothing to any
// sum(rate(..._bucket)) and cannot move a quantile — histogram_quantile over an
// all-zero bucket set is NaN ("no data"), never a fabricated 0. That is why
// this seeds the REAL path label values rather than a "none" sentinel: there is
// no value a zero-count child could distort, and seeding the real labels means
// a per-path panel renders its axes before the first spawn.
//
// Idempotent — WithLabelValues on an existing child is a lookup, not a reset —
// so it is safe to call from the constructor on every construction.
func seedFormulaStepDispatchLatencyMetrics() {
	formulaStepDispatchLatencySeconds.WithLabelValues(stepDispatchPathEvent)
	formulaStepDispatchLatencySeconds.WithLabelValues(stepDispatchPathBackfill)
	formulaStepDispatchLatencyUnmeasuredTotal.WithLabelValues(latencyUnmeasuredNoBlockingDeps).Add(0)
	formulaStepDispatchLatencyUnmeasuredTotal.WithLabelValues(latencyUnmeasuredBlockerTimeMissing).Add(0)
	formulaStepDispatchLatencyUnmeasuredTotal.WithLabelValues(latencyUnmeasuredClockSkew).Add(0)
}

// recordDispatchLatency accounts for exactly one successful spawn on the latency
// families: it observes the readiness -> now interval when ready carries a
// resolved instant, and otherwise bumps the unmeasured counter under ready's
// bounded reason. Every spawn lands in exactly one of the two, which is what
// makes _count + unmeasured == spawned_total hold.
//
// A readiness instant in the future (clock skew between the daemon that wrote
// closed_at and this pod) is DROPPED rather than clamped: a clamped 0 would read
// as a suspiciously fast dispatch, while the clock_skew bucket reads as what it
// is. Mirrors recordRespawnPhase's negative-duration drop.
func (d *FormulaStepDispatcher) recordDispatchLatency(path string, ready stepReadiness) {
	if path != stepDispatchPathEvent && path != stepDispatchPathBackfill {
		// Unreachable from in-tree callers (stepDispatchPathFor is total), but a
		// bounded landing spot beats minting a new label value.
		path = stepDispatchPathEvent
	}
	if ready.at.IsZero() {
		d.recordLatencyUnmeasured(ready.reason)
		return
	}
	elapsed := d.now().Sub(ready.at)
	if elapsed < 0 {
		d.logger.Debug("step dispatch: readiness instant is in the future — dropping latency sample as clock skew",
			"ready_at", ready.at, "skew", -elapsed)
		d.recordLatencyUnmeasured(latencyUnmeasuredClockSkew)
		return
	}
	formulaStepDispatchLatencySeconds.WithLabelValues(path).Observe(elapsed.Seconds())
}

// recordLatencyUnmeasured bumps the unmeasured counter, routing an out-of-enum
// reason to blocker_time_missing (the generic "we could not resolve the
// timestamp" bucket) rather than minting an unbounded label value.
func (d *FormulaStepDispatcher) recordLatencyUnmeasured(reason string) {
	switch reason {
	case latencyUnmeasuredNoBlockingDeps, latencyUnmeasuredBlockerTimeMissing, latencyUnmeasuredClockSkew:
	default:
		reason = latencyUnmeasuredBlockerTimeMissing
	}
	formulaStepDispatchLatencyUnmeasuredTotal.WithLabelValues(reason).Inc()
}
