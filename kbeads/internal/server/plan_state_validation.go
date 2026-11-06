// Plan-lifecycle state-machine enforcement for type=epic plan beads.
//
// A plan epic moves through an authoring lifecycle stamped on its
// fields.plan_state enum — draft → ready → building → {complete | abandoned},
// with reopen as the back-edge from either terminal state ({complete,
// abandoned} → building). The enum VALUES are already constrained by the
// type:epic schema (config.go); this file adds the missing BEHAVIOR: rejecting
// illegal JUMPS between those values (draft→complete, ready→complete,
// building→draft, complete→abandoned, …) so a half-baked plan can never skip the
// gate the /plans cockpit enforces in its UI (plans_lifecycle.go). It runs at
// the updateBead choke point, so gRPC, HTTP PATCH, escalation replay, and undo
// are all covered identically.
//
// complete and abandoned are the two DISTINCT terminal states (task
// ka-0000001377): complete = the plan finished its work; abandoned = the plan
// was deliberately dropped WITHOUT finishing. Both close the plan epic and are
// reached from a non-terminal working state, but they carry opposite meaning so
// /plans can offer a cheap "give up on this" exit that reads differently from a
// proud "done". They are never adjacent to each other — to convert one to the
// other an operator reopens to building first (the single back-edge), so a
// terminal state can never silently flip its meaning.
//
// BACKWARD COMPATIBILITY is load-bearing (task ka-0000004011, epic
// ka-0000006896, honoring ka-0000002616): an UNSET plan_state means the epic
// never opted into the lifecycle (every plan minted before the field, plus any
// epic an operator marked a plan by hand) and is treated as UNGATED. So a
// transition FROM unset (opting in) is always allowed, and a transition TO
// unset (clearing the field back to ungated) is always allowed — the guard only
// constrains movement BETWEEN two concrete lifecycle states.
package server

import (
	"encoding/json"
	"fmt"
)

// planStateValue is the authoring-lifecycle state read off an epic's
// fields.plan_state. It mirrors the viewer's planState (plans_lifecycle.go); the
// zero value planStateUnset ("") is the ungated/backward-compat state.
type planStateValue string

const (
	planStateUnset     planStateValue = ""
	planStateDraft     planStateValue = "draft"
	planStateReady     planStateValue = "ready"
	planStateBuilding  planStateValue = "building"
	planStateComplete  planStateValue = "complete"
	planStateAbandoned planStateValue = "abandoned"
)

// planStateFieldName is the epic field holding the authoring lifecycle state.
// Kept in sync with the type:epic schema (config.go) and the viewer
// (plans_lifecycle.go planStateFieldKey).
const planStateFieldName = "plan_state"

// legalPlanStateEdges is the adjacency set of legal transitions BETWEEN two
// concrete lifecycle states. Self-transitions and any edge touching unset are
// handled separately in validatePlanStateTransition and are intentionally
// absent here. The set matches the transitions the /plans cockpit actually
// performs (plans_lifecycle.go):
//
//	draft → ready        "Mark ready"      (handlePlanState)
//	ready → draft        "Keep editing"    (handlePlanState)
//	ready → building     "Approve & build" (handlePlanBuild)
//	building → complete  "Mark complete"   (handlePlanComplete)
//	complete → building  "Reopen"          (handlePlanReopen)
//	draft → abandoned    "Abandon"         (handlePlanAbandon)
//	ready → abandoned    "Abandon"         (handlePlanAbandon)
//	building → abandoned "Abandon"         (handlePlanAbandon)
//	abandoned → building "Reopen"          (handlePlanReopen)
//
// abandoned is the "cheap exit" (epic ka-0000007470): a plan can be dropped from
// ANY non-terminal working state — a draft that never went anywhere, a ready
// plan the operator changed their mind on, or a building plan whose work is no
// longer wanted — without first walking it forward to building just to complete
// it. Like complete it reopens to building (never to draft/ready — an abandoned
// plan was authored, so reopening restores the buildable state), and the two
// terminal states are never adjacent (converting one to the other goes through
// reopen).
var legalPlanStateEdges = map[planStateValue]map[planStateValue]bool{
	planStateDraft:     {planStateReady: true, planStateAbandoned: true},
	planStateReady:     {planStateDraft: true, planStateBuilding: true, planStateAbandoned: true},
	planStateBuilding:  {planStateComplete: true, planStateAbandoned: true},
	planStateComplete:  {planStateBuilding: true},
	planStateAbandoned: {planStateBuilding: true},
}

// planStateFromFields reads and normalizes the plan_state off a raw fields blob.
// An absent field, an empty/whitespace value, or any value the schema does not
// recognize normalizes to planStateUnset (ungated), mirroring the viewer's
// planStateOf. Malformed JSON also degrades to unset (the structural validator
// upstream is what rejects bad JSON).
func planStateFromFields(fields json.RawMessage) planStateValue {
	if len(fields) == 0 {
		return planStateUnset
	}
	var m map[string]any
	if err := json.Unmarshal(fields, &m); err != nil {
		return planStateUnset
	}
	raw, ok := m[planStateFieldName]
	if !ok {
		return planStateUnset
	}
	s, ok := raw.(string)
	if !ok {
		return planStateUnset
	}
	switch planStateValue(s) {
	case planStateDraft, planStateReady, planStateBuilding, planStateComplete, planStateAbandoned:
		return planStateValue(s)
	default:
		return planStateUnset
	}
}

// validatePlanStateTransition rejects an illegal plan_state jump on a plan-epic
// update. prior is the epic's fields BEFORE the patch; next is the merged
// fields AFTER. Returns nil (legal) when:
//
//   - the state is unchanged (idempotent no-op, incl. patches that don't touch
//     plan_state at all — the dominant case);
//   - the prior state is unset (a legacy/hand-marked plan opting into the
//     lifecycle — backward-compat is load-bearing);
//   - the next state is unset (clearing the field back to ungated — an operator
//     escape hatch that restores pre-lifecycle behavior);
//   - the (prior → next) edge is in legalPlanStateEdges.
//
// Otherwise it returns an inputError-worthy error naming the rejected jump and
// the legal moves out of the current state.
func validatePlanStateTransition(prior, next json.RawMessage) error {
	from := planStateFromFields(prior)
	to := planStateFromFields(next)

	// Unchanged, opting-in from ungated, or clearing back to ungated: always OK.
	if from == to || from == planStateUnset || to == planStateUnset {
		return nil
	}
	if legalPlanStateEdges[from][to] {
		return nil
	}
	return fmt.Errorf("plan_state: illegal transition %s → %s — legal moves are %s (a legacy/ungated plan may enter any state)",
		from, to, legalMovesFrom(from))
}

// legalMovesFrom renders the legal next-states out of a concrete lifecycle
// state for the rejection message, e.g. "draft→ready". Returns "none (terminal
// for forward moves)" when the state has no outgoing concrete edges.
func legalMovesFrom(from planStateValue) string {
	edges := legalPlanStateEdges[from]
	if len(edges) == 0 {
		return "none"
	}
	// Deterministic order for a stable message: draft, ready, building,
	// complete, abandoned.
	var out string
	for _, to := range []planStateValue{planStateDraft, planStateReady, planStateBuilding, planStateComplete, planStateAbandoned} {
		if edges[to] {
			if out != "" {
				out += ", "
			}
			out += string(from) + "→" + string(to)
		}
	}
	return out
}
