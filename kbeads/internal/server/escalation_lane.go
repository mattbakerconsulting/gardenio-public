package server

import (
	"fmt"
	"strings"

	"gardenio/kbeads/internal/model"
)

// laneKind is the kind of a lane ref: the one grammar for an escalation's
// approvers entries, execute_by and held_by (escalation-routing plan D2).
type laneKind string

const (
	laneOperator    laneKind = "operator"
	laneCoordinator laneKind = "coordinator"
	laneSteward     laneKind = "steward"
	laneLandOwner   laneKind = "land-owner"
	laneRequester   laneKind = "requester"
	laneServer      laneKind = "server"
	// laneLegacy is a pre-routing approvers entry (oidc:, team:, slack:, bare
	// email). It is valid in approvers only and is matched by
	// escalationApproverListed, never by lane membership.
	laneLegacy laneKind = "legacy"
)

// laneRef is one parsed lane ref. Name is the coordinator name or the land for
// the named kinds; Raw is the trimmed input.
type laneRef struct {
	Kind laneKind
	Name string
	Raw  string
}

var legacyApproverPrefixes = []string{"oidc:", "team:", "slack:"}

// isLegacyPersonApprover reports whether an approvers entry names one person:
// oidc:<email>, slack:<id> or a bare email. Lane refs (escalation-routing D2)
// and team: entries do not, so #756's forced-owner rule ignores them.
func isLegacyPersonApprover(entry string) bool {
	lane, err := parseLaneRef(entry)
	return err == nil && lane.Kind == laneLegacy && !strings.HasPrefix(lane.Raw, "team:")
}

// parseLaneRef parses one lane ref. An unknown or malformed ref is an error:
// callers fail closed (route to operator, deny coordinators, WARN).
func parseLaneRef(s string) (laneRef, error) {
	raw := strings.TrimSpace(s)
	switch raw {
	case "":
		return laneRef{}, fmt.Errorf("empty lane ref")
	case string(laneOperator), string(laneRequester), string(laneServer):
		return laneRef{Kind: laneKind(raw), Raw: raw}, nil
	}
	for _, k := range []laneKind{laneCoordinator, laneSteward, laneLandOwner} {
		if name, ok := strings.CutPrefix(raw, string(k)+":"); ok {
			if !validLaneName(name) {
				return laneRef{}, fmt.Errorf("lane ref %q: invalid name %q", raw, name)
			}
			return laneRef{Kind: k, Name: name, Raw: raw}, nil
		}
	}
	for _, p := range legacyApproverPrefixes {
		if rest, ok := strings.CutPrefix(raw, p); ok && strings.TrimSpace(rest) != "" {
			return laneRef{Kind: laneLegacy, Raw: raw}, nil
		}
	}
	if strings.Contains(raw, "@") && !strings.ContainsAny(raw, " :") {
		return laneRef{Kind: laneLegacy, Raw: raw}, nil
	}
	return laneRef{}, fmt.Errorf("unknown lane ref %q", raw)
}

// validLaneName reports whether n can name a coordinator or a land.
func validLaneName(n string) bool {
	if n == "" || len(n) > 128 {
		return false
	}
	for _, r := range n {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

// String renders the canonical form: what the server stamps and what a
// held_by filter compares.
func (r laneRef) String() string {
	switch r.Kind {
	case laneCoordinator, laneSteward, laneLandOwner:
		return string(r.Kind) + ":" + r.Name
	case laneOperator, laneRequester, laneServer:
		return string(r.Kind)
	}
	return r.Raw
}

// escalationRouteFields are the routing and approval-provenance fields only the
// server writes (plan Global Constraint 5). held_at (RFC3339 UTC) is restamped
// whenever held_by changes and drives the idle hand-up. Writers: the create-time route stamp
// (escalation_policy.go), the approve paths, and claim/hand-up. approvers is in
// the older stamped lists already. permissions_at_approval/snapshot_at are the
// approve paths' ServiceAccount snapshot (spawn_permission_snapshot.go): a
// requester able to write them could plant a wide baseline that hides drift.
// approved_targets is the approve paths' stamp of the beads the approval names
// (escalation_executor_grant.go): a client able to write it could widen what
// the executor may write.
var escalationRouteFields = []string{
	"held_by", "held_at", "execute_by", "operator_only", "policy_rule",
	"approved_by", "approved_by_email", "approved_at", "approval_method", "approved_as",
	"permissions_at_approval", "snapshot_at",
	"claimed_by", "claimed_at", "handed_up_by", "handed_up_at", "hand_up_reason",
	approvedTargetsField, operatorClassField,
}

// escalationHasServerApproval reports whether b carries an approval the server
// stamped. approved_at is the witness: since escalationRouteFields went
// server-only, no client can write it on create, PATCH or /result.
// approved_by alone proves nothing, because /result may carry it as a display
// stamp (handleCaptureEscalationResult).
func escalationHasServerApproval(b *model.Bead) bool {
	if b == nil {
		return false
	}
	for _, v := range escalationFieldValues(b.Fields, []string{"approved_at"}) {
		if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
			return true
		}
	}
	return false
}
