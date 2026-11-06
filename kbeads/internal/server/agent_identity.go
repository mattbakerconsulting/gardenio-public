package server

import (
	"context"
	"fmt"
	"strings"

	"gardenio/kbeads/internal/model"
)

// Agent identity for per-agent fleet tokens. See actingAgentRef for the shared
// lookup every agent-keyed server step goes through.

// agentFleetPrefix is the fleet-name prefix the controller gives every
// per-agent token (agentTokenBridgeName in gardenio/controller/cmd/controller/
// agenttoken.go). A fleet identity with this prefix is an AGENT: its subject is
// the agent's own name, issued by the controller, and it is the only beads
// credential an agent pod holds once agents.perAgentTokens is on (P7 Task 2).
const agentFleetPrefix = "agent-"

func isAgentFleet(fleet string) bool {
	return strings.HasPrefix(fleet, agentFleetPrefix) && len(fleet) > len(agentFleetPrefix)
}

// agentBeadIDFromContext returns the agent bead id of the authenticated agent
// for a per-agent fleet token: the controller names the fleet "agent-<bead id>"
// (agentTokenBridgeName), so the id is the suffix, and it comes from the
// credential, never from a header. Taints, taint grants, spawn-chain walks and
// the agent-bead lookups key by this id, not by the agent name (names are not
// unique across projects).
func agentBeadIDFromContext(ctx context.Context) (string, bool) {
	id := AuthIdentityFromContext(ctx)
	if id.Type != "fleet" {
		return "", false
	}
	fleet := FleetFromContext(ctx)
	if !isAgentFleet(fleet) {
		return "", false
	}
	return strings.TrimPrefix(fleet, agentFleetPrefix), true
}

// actingAgentRef returns the reference that identifies the acting agent in
// agent-bead lookups: the bead id taken from the fleet name of its own
// per-agent fleet token. The agent's identity comes from its credential only;
// the X-Actor self-attribution the retired shared static token allowed
// (gd-ke6PC7bRto) is gone.
//
// Every server-side step that acts "as the agent" (auto parent, thread_id
// inheritance, owner defaulting, taint context and grants, the spawn-chain
// walk) goes through this one function.
func actingAgentRef(ctx context.Context) (string, bool) {
	return agentBeadIDFromContext(ctx)
}

// validateAgentFleetRegistration refuses an agent-* fleet registration whose
// subject is not a plain agent name. The controller validates the name before
// the mint too; this is the daemon-side backstop, so a compromised or buggy
// minter cannot register a token that carries a service identity.
func validateAgentFleetRegistration(fleet, subject string) error {
	if !isAgentFleet(fleet) {
		return nil
	}
	if strings.ContainsAny(subject, ":@") {
		return fmt.Errorf("invalid subject for an agent token: %q must not contain ':' or '@'", subject)
	}
	return nil
}

// callerOpenRoleBeads returns the open type:role beads named by the role of the
// per-agent-token caller's own agent bead (looked up by the bead id in the
// fleet name, never by a client-supplied value). Every failure (not an agent
// token, no agent bead, unreadable fields, store error) returns nil, so every
// grant read from the result fails closed.
func (s *BeadsServer) callerOpenRoleBeads(ctx context.Context) []*model.Bead {
	beadID, ok := agentBeadIDFromContext(ctx)
	if !ok || s == nil || s.store == nil {
		return nil
	}
	bead, err := s.store.GetBead(ctx, beadID)
	if err != nil || bead == nil || bead.Type != model.BeadType("agent") {
		return nil
	}
	role, _, err := exactFieldString(bead.Fields, agentRoleField)
	if err != nil {
		return nil
	}
	elems := parseRoleList(role)
	if len(elems) == 0 {
		return nil
	}
	roles, err := roleBeadsByTitle(ctx, s.store, elems)
	if err != nil {
		return nil
	}
	open := roles[:0:0]
	for _, rb := range roles {
		if rb.Status != model.StatusClosed {
			open = append(open, rb)
		}
	}
	return open
}

// agentNameFromContext returns the authenticated agent name for a per-agent
// fleet token. The primary static token is never an agent identity here, even
// when its X-Actor names an agent: X-Actor is a claim, the per-agent token is
// a credential.
func agentNameFromContext(ctx context.Context) (string, bool) {
	id := AuthIdentityFromContext(ctx)
	if id.Type != "fleet" || id.Subject == "" || !isAgentFleet(FleetFromContext(ctx)) {
		return "", false
	}
	return id.Subject, true
}
