package server

import (
	"context"
	"strings"
)

// X-Operated-By is attribution, never authority (P7 Task 3, spec D2).
//
// Every caller may send X-Operated-By and it lands in operated_by, but only a
// bridge fleet identity's relay may stand in for a human in an access-control
// decision: team membership (teamsOf), taint grants (grantsOfOperator,
// granterOperatorID) and a decision's responder (decisionAttribution). The
// shared static token is held by the viewer, controller, coopmux and every pod
// without a per-agent token, and a per-agent token is the agent itself; either
// relaying "oidc:owner@…" is a claim, not a proof, so it resolves nothing.

// defaultBridgeSubjects are the bridge fleet subjects trusted to relay a human.
// Overridable with BEADS_BRIDGE_SUBJECTS.
var defaultBridgeSubjects = []string{"slack-bridge", "gitlab-bridge", "jira-bridge", "github-bridge"}

func bridgeSubjectSet(subjects []string) map[string]bool {
	m := make(map[string]bool, len(subjects))
	for _, s := range subjects {
		if s = strings.TrimSpace(s); s != "" {
			m[s] = true
		}
	}
	return m
}

// SetBridgeSubjects replaces the bridge subject list. Blank entries are
// dropped; nil keeps the default.
func (s *BeadsServer) SetBridgeSubjects(subjects []string) {
	if subjects == nil {
		return
	}
	s.bridgeSubjects = bridgeSubjectSet(subjects)
}

// isBridgeSubject reports whether subject names a trusted bridge. The
// bridge-token rotator mints fleet "<name>-bridge" with subject "bridge:<name>"
// (deriveSubject: slack-bridge -> bridge:slack), so that form names the
// "<name>-bridge" entry.
func (s *BeadsServer) isBridgeSubject(subject string) bool {
	set := s.bridgeSubjects
	if set == nil {
		set = bridgeSubjectSet(defaultBridgeSubjects)
	}
	if name, ok := strings.CutPrefix(subject, "bridge:"); ok {
		subject = name + "-bridge"
	}
	return set[subject]
}

// relayTrusted reports whether the caller's X-Operated-By may be used for an
// access-control decision: only a bridge fleet ("*-bridge", a namespace only
// the token-mint capability can name) whose subject is a listed bridge. The
// subject alone is not enough: an untrusted minter picks its own subject, so a
// fleet "qa" with subject "github-bridge" must not relay. A per-agent fleet
// ("agent-<bead id>") never qualifies, whatever its agent happens to be called.
func (s *BeadsServer) relayTrusted(ctx context.Context) bool {
	id := AuthIdentityFromContext(ctx)
	fleet := FleetFromContext(ctx)
	return id.Type == "fleet" && !isAgentFleet(fleet) && strings.HasSuffix(fleet, bridgeFleetSuffix) &&
		s.isBridgeSubject(id.Subject)
}

// trustedRelayOperator returns the relayed human only for a trusted relay, ""
// otherwise.
func (s *BeadsServer) trustedRelayOperator(ctx context.Context) string {
	if !s.relayTrusted(ctx) {
		return ""
	}
	return operatedByHeaderFromContext(ctx)
}
