package beadsapi

import "strings"

// Escalation lane refs the controller names directly (D2 of the escalation
// routing plan). The grammar, membership and the lane → audience mapping live
// in kbeads (escalation_lane.go, escalation_policy.go laneAudience); the
// controller only READS lanes, to label them. Whether a surface shows an
// escalation is #756's audience (EscalationForOwner); whether a viewer may
// act is the daemon's can-approve, never these helpers.
const (
	LaneOperator  = "operator" // the owner (gardenio-admin), not #756's operator audience
	LaneRequester = "requester"
	LaneServer    = "server"
)

// HeldByFromFields returns an escalation's held_by, or "" when it has none
// (filed before routing shipped, or while the policy was unreadable). An
// unstamped escalation is shown where its audience puts it (Global
// Constraint 7), so "" never means "the operator".
func HeldByFromFields(f map[string]string) string {
	return strings.TrimSpace(f["held_by"])
}

// EscalationHeldBy is HeldByFromFields for a bead; a nil bead is "".
func EscalationHeldBy(b *BeadDetail) string {
	if b == nil {
		return ""
	}
	return HeldByFromFields(b.Fields)
}

// EscalationExecuteBy returns the escalation's execute_by lane, or "" when it
// has none, in which case the bridge runs the legacy execution_target path.
func EscalationExecuteBy(b *BeadDetail) string {
	if b == nil {
		return ""
	}
	return strings.TrimSpace(b.Fields["execute_by"])
}

// LandOwnerLane reports whether ref is land-owner:<land>, and which land.
func LandOwnerLane(ref string) (string, bool) {
	land, ok := strings.CutPrefix(strings.TrimSpace(ref), "land-owner:")
	return land, ok && land != ""
}

// LaneLabel is how a surface names a lane to a person; "" for no lane.
func LaneLabel(ref string) string {
	ref = strings.TrimSpace(ref)
	kind, name, _ := strings.Cut(ref, ":")
	switch kind {
	case "":
		return ""
	case LaneOperator:
		return "the owner"
	case "coordinator":
		return "coordinator " + name
	case "steward":
		return "the " + name + " steward"
	case "land-owner":
		return "the " + name + " owner"
	case LaneRequester:
		return "the requesting agent"
	case LaneServer:
		return "the server"
	}
	return ref
}
