package bridge

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// gardener_resume_total counts gardener-loop resume decisions made by the
// terminal_goal master gate (bead ka-0000005353). Every time a *premature*
// stop (out of runs/time/patience/crashing) fires on a gardener whose
// terminal_goal is unmet, gardenerResumeOrEscalate either RE-ARMS the gardener
// or, once its resume budget is spent, ESCALATES it to the operator. This
// counter makes both visible so an operator can watch the gate working before
// the resumer watchdog is automated (decision ka-0000006918: observability
// first).
//
// Labels:
//   - gardener_id: the gardener (trigger) bead ID. Bounded by the live-gardener
//     fleet (tens), so safe as a label.
//   - action: HARD enum {rearm, escalate, dormant, reopen}. rearm = budget
//     remained, gardener re-armed for another loop; escalate = budget spent
//     with no open plan, gardener flagged gardener-stuck-escalate + closed;
//     dormant = parked behind an open operator blocker, or on its plan after
//     spending its resume budget (gd-dNxyjFLGRJ);
//     reopen = an out-of-band operator reopen normalized into a durable
//     re-arm (ka-0000004191).
//   - reason: the premature stop kind that triggered it — HARD enum
//     {max_runs, no_progress_for, wall_clock, consecutive_failures}. Anything
//     else collapses to "other" so cardinality stays bounded.
//
// Registered globally against the controller's default registerer (promauto),
// matching the other bridge metrics (see operator_activity_metrics.go).
var gardenerResumeTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "gardener_resume_total",
		Help: "Gardener-loop resume decisions by the terminal_goal master gate, labelled by gardener_id, action (enum: rearm/escalate/dormant/reopen/renew), and reason (premature stop kind: max_runs/no_progress_for/wall_clock/consecutive_failures/other).",
	},
	[]string{"gardener_id", "action", "reason"},
)

// gardener_resumes_without_progress is a gauge of each gardener's current
// consecutive-fruitless-resume count, so a gardener creeping toward its resume
// budget (and therefore toward operator escalation) is graphable. Set on every
// re-arm/escalate persist.
var gardenerResumesWithoutProgress = promauto.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "gardener_resumes_without_progress",
		Help: "Per-gardener count of consecutive resumes that produced no progress; resets to 0 on any credited progress event. Approaching resume_budget means imminent operator escalation.",
	},
	[]string{"gardener_id"},
)

// Resume metric action label values (bounded enum).
const (
	resumeActionRearm    = "rearm"
	resumeActionEscalate = "escalate"
	// resumeActionDormant records a premature stop that was routed to DORMANT
	// (stay armed, fire suppressed) instead of CLOSED because the gardener's
	// frontier is wedged behind an open operator escalation/decision
	// (bug ka-0000003082).
	resumeActionDormant = "dormant"
	// resumeActionReopen records an out-of-band operator re-arm being
	// normalized: an OPEN gardener still carrying terminate markers got the
	// durable reset + marker clear so the bead-level reopen actually sticks
	// (bug ka-0000004191).
	resumeActionReopen = "reopen"
	// resumeActionRenew records a STANDING gardener whose DESIGNED wall_clock
	// hard_stop ceiling fired being renewed IN PLACE (fresh wall_clock window,
	// bead left open + enabled) instead of closed. A perpetual gardener aging out
	// on the 720h ceiling is expected+recurring, not a break, so silently closing
	// it took the release lane dark for ~18h with no successor and no alert
	// (bug ka-0000004573). reason is always wall_clock.
	resumeActionRenew = "renew"
)

// resumeReasonLabel maps a premature stop kind to the bounded reason-label
// enum, collapsing anything unexpected to "other" so the metric cannot be
// inflated by a future stop kind.
func resumeReasonLabel(kind string) string {
	switch kind {
	case "max_runs", "no_progress_for", "wall_clock", "consecutive_failures":
		return kind
	default:
		return "other"
	}
}

// recordGardenerResume increments gardener_resume_total for one resume decision.
// action must be rearm or escalate; reason is canonicalized via
// resumeReasonLabel.
func recordGardenerResume(gardenerID, action, reason string) {
	if gardenerID == "" {
		gardenerID = "unknown"
	}
	gardenerResumeTotal.WithLabelValues(gardenerID, action, resumeReasonLabel(reason)).Inc()
}

// gardener_second_life_total counts second-life adjudication decisions (bead
// ka-0000004010). Where the resume gate above re-arms an OPEN gardener, the
// second-life gate revives a gardener ALREADY CLOSED on max_runs with an unmet
// terminal_goal — or escalates once its second-life budget (max_second_lives,
// the mirror of terminal-goals max_respawns) is spent. This counter makes the
// don't-resurrect-the-doomed brake observable.
//
// Labels:
//   - gardener_id: the gardener (trigger) bead ID. Bounded by the live-gardener
//     fleet (tens), so safe as a label.
//   - action: HARD enum {grant, escalate}. grant = budget remained, the SAME
//     bead was reopened for another loop; escalate = budget spent, gardener
//     flagged gardener-second-life-exhausted and left closed for the operator.
var gardenerSecondLifeTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "gardener_second_life_total",
		Help: "Gardener second-life adjudication decisions, labelled by gardener_id and action (enum: grant/escalate). grant reopens the same closed-on-max_runs gardener; escalate denies the revive once max_second_lives is spent (don't-resurrect-the-doomed brake).",
	},
	[]string{"gardener_id", "action"},
)

// Second-life metric action label values (bounded enum).
const (
	secondLifeActionGrant    = "grant"
	secondLifeActionEscalate = "escalate"
)

// recordGardenerSecondLife increments gardener_second_life_total for one
// second-life decision. action must be grant or escalate.
func recordGardenerSecondLife(gardenerID, action string) {
	if gardenerID == "" {
		gardenerID = "unknown"
	}
	gardenerSecondLifeTotal.WithLabelValues(gardenerID, action).Inc()
}

// setGardenerResumesWithoutProgress publishes a gardener's current
// fruitless-resume count to the gauge.
func setGardenerResumesWithoutProgress(gardenerID string, count int) {
	if gardenerID == "" {
		gardenerID = "unknown"
	}
	gardenerResumesWithoutProgress.WithLabelValues(gardenerID).Set(float64(count))
}
