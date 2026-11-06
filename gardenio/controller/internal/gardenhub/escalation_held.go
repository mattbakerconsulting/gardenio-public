package gardenhub

import (
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// AgentLaneHolder names the agent lane (a coordinator or a steward) holding a
// non-floor escalation, as a person reads it ("the merge desk"), or "" when
// Approve is the viewer's primary action: a floor item, one the owner or a
// land owner holds, or one with no held_by. Where it is set, a surface reads
// "<holder> has this" and offers approval only as a secondary "Approve
// anyway"; who may approve is still the daemon's can-approve (gd-Mq0uDXHswK).
func AgentLaneHolder(f map[string]string) string {
	if beadsapi.EscalationAudienceForced(f) {
		return ""
	}
	held := beadsapi.HeldByFromFields(f)
	switch kind, name, _ := strings.Cut(held, ":"); {
	case kind == "coordinator" && name == "merge-desk":
		return WaitsOnDesk
	case (kind == "coordinator" || kind == "steward") && name != "":
		return beadsapi.LaneLabel(held)
	}
	return ""
}
