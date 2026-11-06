package server

import (
	"context"
	"net/http"
)

// callerClass is WHO is calling, for the escalation verbs (spec D7). It is not
// a role rank: requireRole(RoleOperator) admits every non-OIDC caller to
// operator routes, which is correct for bead CRUD (agents need it) and wrong
// for resolving an escalation.
//
// The class gate says who may ever resolve; the D6 approval gate
// (escalationResultDenial, BEADS_ESCALATION_RESULT_REQUIRES_APPROVAL) runs
// after it in the handler and says whether this caller may resolve this
// escalation now. Every /result caller holds a classed credential (bridges
// their fleet token, agents and the merge desk their per-agent token, runners
// a gr_ token, the controller's codex keeper its machine principal, the viewer
// the human's bearer); kbeads no longer accepts the shared static token at all
// (gd-ke6PC7bRto), so it has no class here.
type callerClass string

const (
	classOperator callerClass = "operator" // OIDC (or verified v2 edge) human with RoleOperator+, or a project-scoped one (the handler narrows it to its projects)
	classBridge   callerClass = "bridge"   // bridge fleet token (isBridgeFleetIdentity)
	classAgent    callerClass = "agent"    // per-agent fleet token (agentNameFromContext)
	classRunner   callerClass = "runner"   // gr_ runner bearer (runner_auth.go); /result is on its route allowlist
	classMachine  callerClass = "machine"  // registered Keycloak service account (the controller's codex keeper resolves its re-login escalation)
	classOther    callerClass = "other"    // authenticated, no escalation standing (OIDC below operator, v1 edge, land and window principals, other fleets)
	classNone     callerClass = "none"     // auth disabled (dev, unit tests)
)

func (s *BeadsServer) callerClassFromContext(ctx context.Context) callerClass {
	method := authMethodFromContext(ctx)
	id := AuthIdentityFromContext(ctx)
	switch {
	case method == authMethodNone:
		return classNone
	case method == authMethodOIDC && (id.Type == "oidc" || id.Type == "edge") && id.Subject != "" &&
		(hasMinimumRole(RolesFromContext(ctx), RoleOperator) || isScopedPrincipal(ctx)):
		// A scoped principal reached here through scopedRouteAdmits; the
		// handler's scopedApproverAllowed confines it to its own projects.
		return classOperator
	case method == authMethodMachine && id.Type == "machine" && id.Subject != "":
		return classMachine
	case method == authMethodRunner:
		return classRunner
	case id.Type == "fleet" && isBridgeFleetIdentity(FleetFromContext(ctx), id.Subject):
		return classBridge
	}
	if _, ok := agentNameFromContext(ctx); ok {
		return classAgent
	}
	return classOther
}

// requireEscalationResolver gates /v1/escalations/{id}/reject (operator only)
// and /result (operator, bridge, machine, runner, agent; the D6 gate and Task
// 10 narrow which agent). verb is "reject" or "result".
func (s *BeadsServer) requireEscalationResolver(verb string, handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c := s.callerClassFromContext(r.Context())
		allowed := c == classNone || c == classOperator ||
			(verb == "result" && (c == classBridge || c == classMachine || c == classRunner || c == classAgent))
		if allowed {
			handler(w, r)
			return
		}
		// The audit action enum has no "result": a result closes the escalation,
		// as authorizeResolutionTarget audits it.
		action, msg := "reject", "escalation reject requires an operator identity (OIDC or verified v2 edge with gardenio-operator)"
		if verb == "result" {
			action, msg = "close", "escalation result requires an operator, a bridge, a machine principal, a runner, or the dispatched agent"
		}
		msg += "; caller class: " + string(c)
		s.auditDenial(r, &AuthzDenial{Action: action, BeadType: "escalation", Message: msg}, r.PathValue("id"))
		writeError(w, http.StatusForbidden, msg)
	}
}
