package bridge

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// gardenio_operator_activity_total is the bridge-side copy of the backbone
// per-operator activity counter (ka-0000006613). It mirrors the kbeads-side
// metric of the same name/label-set so a single dashboard query unions
// activity across surfaces. The bridge owns the surface=slack series; kbeads
// owns surface=kbeads.
//
// Labels:
//   - operator_id:    the resolved operator identity string (the value the
//     resolver stamped onto the spawn ctx via WithOperator, e.g. "slack:U…").
//     An empty / unattributable operator collapses to "0".
//   - project:        the project the spawn happened under ("unknown" when
//     unset).
//   - activity_type:  HARD enum {spawn, mutation, session, view}. The bridge
//     only ever emits "spawn"; the enum guard rejects anything else so a
//     future caller cannot inflate cardinality.
//   - surface:        "slack" for bridge-originated activity.
//
// Registered globally against the controller's default registerer (promauto),
// matching the other bridge metrics (see slack_oidc_operator_alias_resync.go).
var operatorActivityTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "gardenio_operator_activity_total",
		Help: "Operator-attributed activity, labelled by operator_id (empty→\"0\"), project, activity_type (enum: spawn/mutation/session/view), and surface. Bridge owns the surface=slack series.",
	},
	[]string{"operator_id", "project", "activity_type", "surface"},
)

// Bounded activity_type enum — the only members allowed as the activity_type
// label. recordOperatorActivity refuses anything outside this set.
const (
	activityTypeSpawn    = "spawn"
	activityTypeMutation = "mutation"
	activityTypeSession  = "session"
	activityTypeView     = "view"
)

// surfaceSlack is the bridge's origin-surface label value.
const surfaceSlack = "slack"

// activityTypeValid reports whether t is a member of the bounded activity_type
// enum {spawn, mutation, session, view}.
func activityTypeValid(t string) bool {
	switch t {
	case activityTypeSpawn, activityTypeMutation, activityTypeSession, activityTypeView:
		return true
	default:
		return false
	}
}

// recordOperatorActivity increments gardenio_operator_activity_total for one
// bridge-originated activity. An out-of-enum activityType is DROPPED (no
// increment, returns false) so the metric's cardinality stays bounded. An
// empty operatorID collapses to "0" and an empty project to "unknown".
func (b *Bot) recordOperatorActivity(operatorID, project, activityType, surface string) bool {
	if !activityTypeValid(activityType) {
		b.logger.Warn("dropping operator activity: invalid activity_type",
			"activity_type", activityType, "surface", surface)
		return false
	}
	if operatorID == "" {
		operatorID = "0"
	}
	if project == "" {
		project = "unknown"
	}
	operatorActivityTotal.WithLabelValues(operatorID, project, activityType, surface).Inc()
	return true
}
