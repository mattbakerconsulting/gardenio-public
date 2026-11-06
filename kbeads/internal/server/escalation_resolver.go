package server

import (
	"context"

	"gardenio/kbeads/internal/model"
)

// resultResolver returns the server-derived identity allowed to post bead's
// result, or a refusal. Spec §5.2. It runs after requireEscalationResolver
// (the class gate) and before the D6 approval gate (escalationResultDenial).
//
// Two widenings over §5.2: runners and machine principals (admitted to /result
// by the class gate) resolve as their subject, and a requesting agent, the
// current assignee of its requesting task or a later occupant of its seat
// (callerMayWithdraw) may withdraw the escalation (result_status=failure),
// the withdrawal D6(c) also admits.
func (s *BeadsServer) resultResolver(ctx context.Context, bead *model.Bead, resultStatus string) (identity, refusal string) {
	switch s.callerClassFromContext(ctx) {
	case classNone:
		if a := actorFromContext(ctx); a != "" {
			return a, ""
		}
		return "anonymous", ""
	case classOperator:
		id, ok := s.escalationApprover(ctx)
		if !ok {
			id = actorFromContext(ctx)
		}
		// Four-eyes covers an operator's result as well as approve (spec D11,
		// P7 Task 14). Executors and bridges are not the requesting human.
		if escalationRequiresFourEyes(bead) && s.escalationFourEyesViolation(ctx, bead, id) {
			return "", "four-eyes required: the resolver must differ from the requester"
		}
		return id, ""
	case classBridge, classMachine, classRunner:
		return AuthIdentityFromContext(ctx).Subject, ""
	case classAgent:
		name, _ := agentNameFromContext(ctx)
		f := escalationStringFields(bead)
		// The dispatcher records the executor by agent name; the local
		// watcher's first stamp is the agent bead id.
		if ex := f["executor"]; ex != "" {
			if ex == name {
				return name, ""
			}
			if cb := s.resolveAgentBeadBySubject(ctx, name); cb != nil && cb.ID == ex {
				return name, ""
			}
		}
		requester := s.isRequesterOrSpawnedByRequester(ctx, name, f["requesting_agent"])
		if resultStatus == "failure" && (requester || s.callerMayWithdraw(ctx, bead) || s.policyLaneMayReject(ctx, bead)) {
			return name, ""
		}
		if !isVerdictOperation(f["operation_type"]) {
			return "", "only the dispatched executor may post this escalation's result"
		}
		if requester {
			return "", "the requesting agent (or an agent it spawned) cannot resolve its own escalation"
		}
		return name, ""
	}
	return "", "escalation result requires an operator, a bridge, a machine principal, a runner, or the dispatched agent"
}

// isRequesterOrSpawnedByRequester: requester may be an agent name or an agent
// bead id (both shapes occur in requesting_agent).
func (s *BeadsServer) isRequesterOrSpawnedByRequester(ctx context.Context, caller, requester string) bool {
	if requester == "" {
		return false
	}
	if caller == requester {
		return true
	}
	cb := s.resolveAgentBeadBySubject(ctx, caller)
	if cb == nil {
		return false
	}
	if cb.ID == requester {
		return true
	}
	spawnedBy := escalationStringFields(cb)["spawned_by"]
	if spawnedBy == "" {
		return false
	}
	if spawnedBy == requester {
		return true
	}
	if rb := s.resolveAgentBeadBySubject(ctx, requester); rb != nil && spawnedBy == rb.ID {
		return true
	}
	return false
}
