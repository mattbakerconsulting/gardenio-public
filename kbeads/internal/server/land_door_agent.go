package server

import (
	"context"
	"database/sql"
	"errors"
	"net/http"

	"gardenio/kbeads/internal/store"
)

// An agent behind the land door (gd-mh9ANzCHIE, follow-up to 10.1
// gd-g77WcV22SK and Door D.18 gd-nvvTcZeBZD). A land principal carries no
// agent identity, so it cannot stop "its own" agent: there is no own agent to
// name, and tierLand gets nothing on type agent (one land process must not
// stop every agent in the land). The agent forwards its per-agent token
// instead (Lands Track B spec §5.1: a tenant agent is a per-agent token bound
// to the project), and the door admits it when that project is the door's
// tenant land and the token is that land's own-machine agent's
// (landDoorAgentDenial). Through the door the token is held to:
//
//   - the route allowlist, as every land-door caller is (landDoorRouteGate);
//   - its project binding and every other check it meets straight at the
//     daemon, the lands-tenant tier and restrict taints included
//     (restricted_ops.go);
//   - one agent bead, its own: it may record its wrap-up on and close the
//     agent bead the token names (gb stop), and write no other agent bead
//     (landDoorAgentWriteGate) and spawn nothing (landDoorAgentSpawnDenial).
//     Straight at the daemon a project-bound agent token may still close a
//     sibling's agent bead; the door does not carry that over.

const landDoorContextKey contextKey = "land.door"

// withLandDoor records the land whose door the request came through.
func withLandDoor(ctx context.Context, land string) context.Context {
	return context.WithValue(ctx, landDoorContextKey, land)
}

// landDoorFromContext returns the land whose door the request came through,
// set by landDoorMiddleware once the credential is admitted to it.
func landDoorFromContext(ctx context.Context) (string, bool) {
	land, _ := ctx.Value(landDoorContextKey).(string)
	return land, land != ""
}

// landDoorAgentDenial rules on a per-agent token at door. handled is false
// for every other credential. Only a token the Lands API minted for a
// member's own-machine agent (MintAgent: an agent bead isLandAgent names) in
// tenant land door is admitted. An unbound token, another land's, a
// privileged one, a pod agent's, or one bound to an operator land is refused,
// so a token taken from a pod is no key from the internet (gd-Utbyy4xJRX). A
// lookup that fails denies.
func (s *BeadsServer) landDoorAgentDenial(ctx context.Context, door string) (denial string, handled bool) {
	beadID, ok := agentBeadIDFromContext(ctx)
	if !ok || authMethodFromContext(ctx) != authMethodStatic {
		return "", false
	}
	switch project := TokenProjectFromContext(ctx); {
	case AuthIdentityFromContext(ctx).Privileged:
		return "land door: a privileged agent token may not use the land door", true
	case project == "":
		return "land door: agent token is bound to no land", true
	case project != door:
		return "land door: agent token is for another land", true
	}
	if s.store == nil {
		return "land door: cannot verify the agent token's land", true
	}
	tenant, err := s.tenantLands.isTenant(ctx, s, door)
	switch {
	case err != nil:
		return "land door: cannot verify the agent token's land; retry", true
	case !tenant:
		return "land door: an agent token uses only a tenant land's door", true
	}
	bead, err := s.store.GetBead(ctx, beadID)
	switch {
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return "land door: cannot verify the agent token's agent; retry", true
	case err != nil || !isLandAgent(bead, door):
		return "land door: only an agent a land member started on their own machine uses the land door", true
	}
	return "", true
}

// landDoorAgentBeadID returns the calling agent's own bead id when the
// request is a per-agent token's through the land door.
func landDoorAgentBeadID(ctx context.Context) (string, bool) {
	if _, door := landDoorFromContext(ctx); !door {
		return "", false
	}
	return agentBeadIDFromContext(ctx)
}

// landDoorAgentWriteGate refuses a per-agent token, through the land door,
// any write to an agent bead other than its own. It runs per route, inside
// the mux match, so it reads the route's {id}; a bead that does not resolve
// is left to the handler's own 404. A lookup that fails otherwise denies.
func landDoorAgentWriteGate(st store.Store, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		own, ok := landDoorAgentBeadID(r.Context())
		id := r.PathValue("id")
		if !ok || id == "" || id == own || r.Method == http.MethodGet || r.Method == http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}
		if st == nil {
			writeError(w, http.StatusForbidden, "land door: cannot verify the bead an agent token writes")
			return
		}
		bead, err := st.GetBead(r.Context(), id)
		switch {
		case errors.Is(err, sql.ErrNoRows) || (err == nil && bead == nil):
		case err != nil:
			writeError(w, http.StatusForbidden, "land door: cannot verify the bead an agent token writes; retry")
			return
		case bead.Type == typeAgent:
			writeAuthzDenial(w, &AuthzDenial{
				Action:   r.Method,
				BeadType: string(typeAgent),
				Message:  "land door: an agent token writes only its own agent bead " + own + ", not " + id,
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// landDoorAgentSpawnDenial refuses a per-agent token, through the land door,
// putting an agent into a role: an agent bead create, or an update that sets
// a role (authorizeAgentRole). Spawning stays an in-cluster act.
func landDoorAgentSpawnDenial(ctx context.Context, action string) *AuthzDenial {
	if _, ok := landDoorAgentBeadID(ctx); !ok {
		return nil
	}
	return &AuthzDenial{
		Action:   action,
		BeadType: string(typeAgent),
		Message:  "land door: an agent token spawns no agent and sets no agent's role through the land door",
	}
}
