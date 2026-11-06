package server

import (
	"log/slog"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
)

// gardenio_operator_activity_total is the backbone per-operator activity
// counter (customer-1 telemetry plan). It records discrete operator-attributed
// actions across surfaces so the operator-engagement dashboards can slice by
// who did what, where.
//
// Labels:
//   - operator_id:    the resolved operators(id) FK as a decimal string. A
//     NULL / unattributable operator collapses to "0" (the sentinel the
//     fail-closed resolver uses for "no operator"), keeping the label always
//     present and bounded by the operator table (tens of humans).
//   - project:        the project scope the activity happened under
//     ("unknown" when no project scope is in context). Bounded by the project
//     bead set.
//   - activity_type:  a HARD enum {spawn, mutation, session, view}. Any value
//     outside the enum is REJECTED (no increment) so a typo or new shape can
//     never inflate cardinality. See activityTypeValid / recordOperatorActivity.
//   - surface:        the origin surface of the activity (e.g. "kbeads",
//     "slack"). Bounded by the small set of emit sites.
//
// Registered on the internal-only /metrics handler (MetricsHandler).
var operatorActivityTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "gardenio_operator_activity_total",
		Help: "Operator-attributed activity, labelled by operator_id (NULL→\"0\"), project, activity_type (enum: spawn/mutation/session/view), and surface.",
	},
	[]string{"operator_id", "project", "activity_type", "surface"},
)

// Bounded activity_type enum. Adding a value here is the ONLY way to widen the
// set — recordOperatorActivity refuses anything not listed.
const (
	activityTypeSpawn    = "spawn"
	activityTypeMutation = "mutation"
	activityTypeSession  = "session"
	activityTypeView     = "view"
)

// Bounded surface label values used by the kbeads emit sites. Other binaries
// (e.g. the Slack bridge) own their own copies of this metric and surfaces.
const surfaceKbeads = "kbeads"

// activityTypeValid reports whether t is a member of the bounded activity_type
// enum {spawn, mutation, session, view}. Pure — the single source of truth for
// enum membership, shared by the emit guard and the unit test.
func activityTypeValid(t string) bool {
	switch t {
	case activityTypeSpawn, activityTypeMutation, activityTypeSession, activityTypeView:
		return true
	default:
		return false
	}
}

// operatorIDLabel renders an operators(id) FK as the operator_id label. A
// NULL / unattributable operator (id == 0) maps to the "0" sentinel so the
// label is always present.
func operatorIDLabel(operatorID int64) string {
	return strconv.FormatInt(operatorID, 10)
}

// recordOperatorActivity increments gardenio_operator_activity_total for one
// operator-attributed action. It enforces the activity_type enum: an
// out-of-set type is logged and DROPPED (no increment, returns false) so the
// metric's cardinality can never blow up from an unexpected value. A valid
// type increments the counter and returns true.
//
// operatorID is the resolved operators(id) FK (0 → "0" sentinel for an
// unattributable / NULL operator). project is the project scope ("" →
// "unknown"). surface is the origin surface label.
func recordOperatorActivity(operatorID int64, project, activityType, surface string) bool {
	if !activityTypeValid(activityType) {
		slog.Warn("dropping operator activity: invalid activity_type",
			"activity_type", activityType, "surface", surface)
		return false
	}
	if project == "" {
		project = "unknown"
	}
	operatorActivityTotal.WithLabelValues(
		operatorIDLabel(operatorID),
		project,
		activityType,
		surface,
	).Inc()
	return true
}
