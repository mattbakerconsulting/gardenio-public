package server

import (
	"net/http"
	"slices"
)

// meJSON is the GET /v1/me body: who the caller is, which lands it is in, and
// its tier in each (boundary recommendation gd-uYNbyW4a9Z §3: `land ls` reads
// it to find the caller's lands without a kbeads address of its own).
type meJSON struct {
	// OperatorID and Source are /v1/whoami's pair.
	OperatorID string `json:"operator_id"`
	Source     string `json:"source"`
	// Unrestricted is scopeFromContext's verdict, except for a runner bearer:
	// false means every read and write this caller makes is bound to Lands.
	Unrestricted bool `json:"unrestricted"`
	// TokenProject is a fleet token's own project binding, if any.
	TokenProject string `json:"token_project,omitempty"`
	// Agent and AgentBead are set only for a per-agent fleet token: the
	// agent's name and its agent bead id, both from the credential
	// (agentNameFromContext, agentBeadIDFromContext), never from X-Actor.
	// The slack-bridge resolves an agent's token to its agent through them
	// (gd-P5wjHDweih).
	Agent     string   `json:"agent,omitempty"`
	AgentBead string   `json:"agent_bead,omitempty"`
	Lands     []meLand `json:"lands"`
}

type meLand struct {
	Name string `json:"name"`
	Tier tier   `json:"tier"`
}

// computeMe resolves meJSON from the verified token on this request, through
// scopeFromContext, the same call the read scope and actionTable use. An
// unrestricted caller is listed in the lands its groups name, as tierOperator.
// A runner bearer is restricted and listed in its project allowlist (D8), as
// tierRunner: scopeFromContext passes it as unrestricted because
// runnerRouteGate and the allowlist bind it instead.
func computeMe(r *http.Request) meJSON {
	ctx := r.Context()
	op, src := computeWhoami(ctx)
	scope := scopeFromContext(ctx)
	me := meJSON{OperatorID: op, Source: src, Unrestricted: scope.Unrestricted, TokenProject: TokenProjectFromContext(ctx), Lands: []meLand{}}
	if name, ok := agentNameFromContext(ctx); ok {
		me.Agent = name
		me.AgentBead, _ = agentBeadIDFromContext(ctx)
	}
	if authMethodFromContext(ctx) == authMethodRunner {
		me.Unrestricted = false
		projects := slices.Clone(RunnerProjectsFromContext(ctx))
		slices.Sort(projects)
		for _, p := range slices.Compact(projects) {
			me.Lands = append(me.Lands, meLand{Name: p, Tier: tierRunner})
		}
		return me
	}
	if !scope.Unrestricted {
		for _, p := range scope.Projects {
			me.Lands = append(me.Lands, meLand{Name: p, Tier: scope.Tiers[p]})
		}
		return me
	}
	// Only a verified OIDC token carries groups, so another caller lists none.
	projects := groupsToProjects(GroupsFromContext(ctx))
	slices.Sort(projects)
	for _, p := range projects {
		me.Lands = append(me.Lands, meLand{Name: p, Tier: tierOperator})
	}
	return me
}

// handleMe handles GET /v1/me. No role wrapper, like POST /v1/users/me/terms:
// a land guest or applicant may hold no gardenio role, and the body describes
// only the caller's own token.
func (s *BeadsServer) handleMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, computeMe(r))
}
