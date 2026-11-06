package metrics

import (
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

// Decision lifecycle + escalation counters (customer-1 telemetry plan §5).
//
// These ride the existing eventbus decision-event emit site rather than
// introducing a new instrumentation point: every decision event that flows
// through eventbus.Bus.Dispatch bumps gardenio_decisions_lifecycle_total, and
// DecisionEscalated additionally bumps gardenio_escalations_total. Operator
// identity stays OUT of the live series — escalation is labelled by role, not
// operator_id; identity is recovered from the decision bead / exemplar
// (plan §4 privacy allow-list). All labels are bounded enums so a future
// vmalert rule needs no re-instrumentation.

// Decision lifecycle `event` label values. Each maps 1:1 onto an eventbus
// decision EventType (EventDecisionCreated/Responded/Escalated/Expired,
// kbeads/internal/eventbus/types.go:34-37). The eventbus emit site owns the
// EventType→label mapping; this package only sinks the bounded value.
const (
	DecisionEventCreated   = "created"
	DecisionEventResponded = "responded"
	DecisionEventEscalated = "escalated"
	DecisionEventExpired   = "expired"
)

// Urgency label values for gardenio_decisions_lifecycle_total. Bounded enum;
// any unrecognized or absent urgency buckets to UrgencyUnknown so a misspelled
// or new urgency string can never inflate cardinality (plan §3.3 convention).
const (
	UrgencyLow      = "low"
	UrgencyMedium   = "medium"
	UrgencyHigh     = "high"
	UrgencyCritical = "critical"
	UrgencyUnknown  = "unknown"
)

// Escalation role label values for gardenio_escalations_total.escalated_by_role.
// Bounded to the known agent roles (~7); anything else buckets to RoleOther.
// Role — never operator_id — keeps override-pattern analysis identity-free
// (plan §4).
const (
	RoleCrew        = "crew"
	RoleGardener    = "gardener"
	RoleThread      = "thread"
	RoleOperator    = "operator"
	RoleConcierge   = "concierge"
	RoleJiraMention = "jira-mention"
	RoleTriage      = "triage"
	RoleOther       = "other"
)

// Escalation outcome label values. Bounded enum; unrecognized buckets to
// OutcomeOther, and an absent outcome buckets to OutcomeRequested because the
// DecisionEscalated event itself represents an escalation being raised — the
// approve/reject resolution arrives later on the escalation bead.
const (
	OutcomeRequested = "requested"
	OutcomeApproved  = "approved"
	OutcomeRejected  = "rejected"
	OutcomeExpired   = "expired"
	OutcomeOther     = "other"
)

var (
	// DecisionsLifecycle counts decision lifecycle events by type and urgency.
	// Increments on every decision event dispatched through the eventbus.
	DecisionsLifecycle = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gardenio_decisions_lifecycle_total",
			Help: "Count of decision lifecycle events, by event type and urgency.",
		},
		[]string{"event", "urgency"},
	)

	// Escalations counts decision escalations by the role of the escalating
	// actor and the escalation outcome. Increments on DecisionEscalated.
	// Role, not operator_id — identity-free override-signal analysis.
	Escalations = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gardenio_escalations_total",
			Help: "Count of decision escalations, by escalating role and outcome.",
		},
		[]string{"escalated_by_role", "outcome"},
	)
)

// RegisterDecisions registers the decision lifecycle + escalation counters on
// the given registry. Safe to call once per registry.
func RegisterDecisions(reg prometheus.Registerer) {
	reg.MustRegister(DecisionsLifecycle, Escalations)
}

// ObserveDecisionLifecycle bumps gardenio_decisions_lifecycle_total for one
// decision event. eventLabel must be one of the DecisionEvent* constants;
// callers that cannot map their event type should skip the call. urgency is
// normalized to the bounded enum.
func ObserveDecisionLifecycle(eventLabel, urgency string) {
	if eventLabel == "" {
		return
	}
	DecisionsLifecycle.WithLabelValues(eventLabel, NormalizeUrgency(urgency)).Inc()
}

// ObserveEscalation bumps gardenio_escalations_total for one decision
// escalation. role and outcome are normalized to their bounded enums.
func ObserveEscalation(role, outcome string) {
	Escalations.WithLabelValues(NormalizeRole(role), NormalizeOutcome(outcome)).Inc()
}

// NormalizeUrgency maps an arbitrary urgency string onto the bounded enum.
// Unrecognized / empty → UrgencyUnknown.
func NormalizeUrgency(urgency string) string {
	switch strings.ToLower(strings.TrimSpace(urgency)) {
	case UrgencyLow:
		return UrgencyLow
	case UrgencyMedium:
		return UrgencyMedium
	case UrgencyHigh:
		return UrgencyHigh
	case UrgencyCritical:
		return UrgencyCritical
	}
	return UrgencyUnknown
}

// NormalizeRole maps an arbitrary role string onto the bounded role enum.
// A leading "role:" label prefix is stripped. Unrecognized / empty → RoleOther.
func NormalizeRole(role string) string {
	r := strings.ToLower(strings.TrimSpace(role))
	r = strings.TrimPrefix(r, "role:")
	switch r {
	case RoleCrew:
		return RoleCrew
	case RoleGardener:
		return RoleGardener
	case RoleThread:
		return RoleThread
	case RoleOperator:
		return RoleOperator
	case RoleConcierge:
		return RoleConcierge
	case RoleJiraMention:
		return RoleJiraMention
	case RoleTriage:
		return RoleTriage
	}
	return RoleOther
}

// NormalizeOutcome maps an arbitrary outcome string onto the bounded enum.
// Empty → OutcomeRequested (the escalation was raised); other unrecognized
// values → OutcomeOther.
func NormalizeOutcome(outcome string) string {
	o := strings.ToLower(strings.TrimSpace(outcome))
	switch o {
	case "":
		return OutcomeRequested
	case OutcomeRequested:
		return OutcomeRequested
	case OutcomeApproved:
		return OutcomeApproved
	case OutcomeRejected:
		return OutcomeRejected
	case OutcomeExpired:
		return OutcomeExpired
	}
	return OutcomeOther
}
