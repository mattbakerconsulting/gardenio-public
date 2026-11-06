package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"

	"gardenio/clicreds/agentname"
	"gardenio/kbeads/internal/model"
)

// Approved bead-close escalations (gd-Ti5JGqdhpd, owner decision gd-7h2bWUl0Uv
// option a; duplicate gd-ahckTWLhUE).
//
// A kbeads_bead_close / bead_close escalation asks the operator to close beads
// the requester cannot: their creator is gone, or a human owns them. The
// daemon has no replay for these operation types, so the bridge spawns an
// executor agent (escalation-<id>) to run the approved script. That executor
// owns none of the beads and holds no admin role, so every kd close it ran
// came back "HTTP 403 ownership denied" (gd-Z7Pz2reaBG, gd-7zjEgAp7vf) and the
// escalation type could never succeed.
//
// The operator's approval is the authority for exactly what they approved.
// So the executor of an approved bead-close escalation may CLOSE exactly the
// bead ids that escalation names, and nothing else: no update, no delete, no
// other bead, no other caller. Every input is read server-side, never from the
// close request:
//
//   - the caller is an agent on its own per-agent token; its agent bead id
//     comes from the credential (agentBeadIDFromContext) and its name from the
//     token subject, which the controller mints;
//   - that agent bead's escalation_id field says which escalation to look at
//     (the bridge stamps it at spawn, spawnExecutionAgent). It only finds the
//     escalation and binds nothing: an agent can write its own bead;
//   - the binding is the token subject alone: it must equal the name the
//     bridge spawns that escalation's executor under,
//     agentname.EscalationExecutor("escalation-", <escalation id>), the helper
//     the bridge's executorAgentName calls too. The escalation's executor
//     field is not consulted: it is writable (not in escalationImmutableFields,
//     and /result takes it from the request), so an agent that set it to its
//     own name and pointed its own bead at the escalation would otherwise get
//     the grant;
//   - the escalation is open, carries a server-stamped approval (approved_at,
//     escalationHasServerApproval: no client can write it), and its
//     operation_type is a bead-close type;
//   - the bead is listed in operation_spec.bead_ids or is target_bead_id, the
//     fields the approver reviewed, which are immutable after create
//     (escalationImmutableFields). execution_script and requesting_task are
//     free text anyone on the shared queue may edit, so they name nothing here.
//
// Decisions are excluded, as in every other ownership bypass (gd-Du6wHclx4g):
// closing one withdraws or answers it, which is not a bookkeeping close. The
// grant expires with the escalation: once the executor records its result and
// the escalation closes, it names nothing.
//
// The grant relaxes only the ownership gate. Project scope, write policy,
// token scopes, the kill switch and the taint gate still run, and each close
// it admits leaves a mutation_history row naming the escalation
// (recordEscalationCloseGrant).

// beadCloseOperationTypes are the escalation operation types whose executor
// may close the beads the escalation names. Both spellings are in use: gb
// escalation create files kbeads_bead_close, thread agents have filed bead_close.
var beadCloseOperationTypes = map[string]bool{
	"kbeads_bead_close": true,
	"bead_close":        true,
}

// escalationIDField is the agent-bead field the bridge stamps on an
// escalation's executor (spawnExecutionAgent in controller/internal/bridge).
const escalationIDField = "escalation_id"

// escalationCloseAuthorityField is the mutation_history field of the row a
// granted close records.
const escalationCloseAuthorityField = "close_authority"

// escalationCloseGrant returns the id of the approved bead-close escalation
// whose deterministic executor the caller is and which names bead, when
// action is a close;
// "" otherwise. Every failure (no agent token, unreadable fields, store error)
// is "": the caller falls through to the ownership denial.
func (s *BeadsServer) escalationCloseGrant(ctx context.Context, action string, bead *model.Bead) string {
	if action != "close" || bead == nil || bead.Type == model.TypeDecision || s == nil || s.store == nil {
		return ""
	}
	agentBeadID, ok := agentBeadIDFromContext(ctx)
	if !ok {
		return ""
	}
	agentName := AuthIdentityFromContext(ctx).Subject
	if agentName == "" {
		return ""
	}
	agent, err := s.store.GetBead(ctx, agentBeadID)
	if err != nil || agent == nil || agent.Type != model.BeadType("agent") {
		return ""
	}
	escID, _, err := exactFieldString(agent.Fields, escalationIDField)
	if err != nil || strings.TrimSpace(escID) == "" {
		return ""
	}
	esc, err := s.store.GetBead(ctx, strings.TrimSpace(escID))
	if err != nil || esc == nil || esc.Type != model.TypeEscalation || esc.Status == model.StatusClosed {
		return ""
	}
	if !escalationHasServerApproval(esc) {
		return ""
	}
	// Exact keys only (exactFieldString): encoding/json would fold a case
	// variant such as "Operation_Type" onto the real key.
	opType, _, err := exactFieldString(esc.Fields, "operation_type")
	if err != nil || !beadCloseOperationTypes[normalizeOperationType(opType)] {
		return ""
	}
	if agentName != agentname.EscalationExecutor(agentname.EscalationExecutorPrefix, esc.ID) {
		return ""
	}
	for _, id := range escalationCloseBeadIDs(esc.Fields) {
		if id == bead.ID {
			return esc.ID
		}
	}
	return ""
}

// escalationCloseBeadIDs returns the bead ids a bead-close escalation names:
// target_bead_id plus operation_spec.bead_ids, read by their exact keys. A
// bead_ids value that is not a list of strings names nothing.
func escalationCloseBeadIDs(fields json.RawMessage) []string {
	var ids []string
	if t, _, err := exactFieldString(fields, "target_bead_id"); err == nil && strings.TrimSpace(t) != "" {
		ids = append(ids, strings.TrimSpace(t))
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(fields, &m) != nil {
		return ids
	}
	var spec map[string]json.RawMessage
	if json.Unmarshal(m["operation_spec"], &spec) != nil {
		return ids
	}
	var listed []string
	if json.Unmarshal(spec["bead_ids"], &listed) != nil {
		return ids
	}
	for _, id := range listed {
		if id = strings.TrimSpace(id); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// recordEscalationCloseGrant writes the mutation_history row for a close made
// by the executor of an approved bead-close escalation that names the bead:
// field close_authority, new value "escalation:<id>". The row says which
// approval the executor acted on, so it is written whether or not another rule
// (say, the executor also being the assignee) would have admitted the close
// too. It is not an ownership signal, so it does not take dibs. Any other
// write, and a no-op close of a bead already closed, records nothing.
func (s *BeadsServer) recordEscalationCloseGrant(ctx context.Context, action string, bead *model.Bead) {
	if action != "close" || bead == nil || bead.Status == model.StatusClosed {
		return
	}
	escalationID := s.escalationCloseGrant(ctx, action, bead)
	if escalationID == "" {
		return
	}
	actor := actorFromContext(ctx)
	slog.Info("close by the executor of an approved bead-close escalation",
		"bead", bead.ID, "escalation", escalationID, "executor", actor)
	s.recordMutationNoDibs(ctx, bead.ID, actor, escalationCloseAuthorityField, "", "escalation:"+escalationID, model.MutationTypeUpdate)
}
