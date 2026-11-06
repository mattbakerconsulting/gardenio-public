package server

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"gardenio/kbeads/internal/model"
)

// Who may answer a decision (gd-Du6wHclx4g).
//
// POST /v1/decisions/{id}/resolve and /cancel sit behind requireRole(RoleOperator),
// which lets every non-OIDC caller through, and decisionAttribution then stamps
// responded_by from the X-Operated-By relay header. Both the shared static token
// and a per-agent fleet token are credentials an agent pod holds, so a
// prompt-injected agent could answer its own (or a peer's) approval-gate decision
// with `X-Operated-By: oidc:<owner>`: the record shows the owner chose it, the
// Slack bridge respawn acts on `chosen`, and maybeRebindAgentOnDecisionAnswer
// rebinds the requesting agent to that relayed human.
//
// decisionResolver is the identity gate that closes this. The relay header stays
// attribution only; it never makes an agent credential a resolver.

// decisionResolverAllowed reports whether the authenticated caller is a
// principal that may answer, dismiss or close a decision, and a short reason when
// it is not.
//
//   - oidc / edge: a human's own session (the edge HMAC envelope is verified, the
//     OIDC bearer is the human's token). Role and project scope are checked by
//     the caller (requireRole, authorizeResolutionTarget).
//   - machine: a registered Keycloak service account (platform services; an agent
//     pod never holds one).
//   - a bridge fleet token: the Slack bridge relays the human who clicked, until
//     account linking lets it act with the linked person's own token. A bridge is
//     recognised by its fleet, not only by a rotator-minted subject: the deployed
//     Slack bridge authenticates with the hand-made bridge-tokens fleet-map entry
//     "<token>:slack-bridge", which carries no Subject, so its identity is
//     fleet "slack-bridge" / subject "slack-bridge". The rotator form (subject
//     "bridge:<name>") is admitted too. A per-agent token (fleet "agent-<bead
//     id>") is never a bridge, whatever it claims.
//   - no identity at all: no-auth dev / e2e mode, where nothing is verifiable and
//     the outer middleware decides.
//
// Refused: the shared primary static token (every agent pod holds it, as does any
// pod without a per-agent token) and every other fleet token, including each
// agent-* one.
func decisionResolverAllowed(ctx context.Context) (bool, string) {
	id := AuthIdentityFromContext(ctx)
	switch id.Type {
	case "oidc", "edge", "machine", "none", "anonymous", "":
		return true, ""
	case "fleet":
		if isBridgeFleetIdentity(FleetFromContext(ctx), id.Subject) {
			return true, ""
		}
	}
	return false, "answering a decision requires a human session (OIDC or edge-auth) or a bridge token; " +
		"agent and shared-service tokens cannot resolve, cancel or close a decision"
}

// isBridgeFleetIdentity reports whether a fleet identity is a service bridge:
// never a per-agent fleet, and either carrying a "bridge:<name>" subject
// (bridge-token rotator) or living in a "<name>-bridge" fleet (the fleet-map
// convention bridgeSubjectMatchesLegacyCreatedBy relies on). Fleet names come
// from operator-controlled config (BEADS_FLEET_MAP, the bridge-tokens Secret,
// the rotator), never from a request header.
func isBridgeFleetIdentity(fleet, subject string) bool {
	if isAgentFleet(fleet) {
		return false
	}
	return strings.HasPrefix(subject, "bridge:") || strings.HasSuffix(fleet, bridgeFleetSuffix)
}

// bridgeFleetSuffix is the fleet-name convention isBridgeFleetIdentity trusts.
// validateUntrustedMint reserves it so only the token-mint capability can name
// such a fleet.
const bridgeFleetSuffix = "-bridge"

// authorizeDecisionResolver writes a 403 and returns false when the caller is
// not allowed to answer a decision. Denials are audited like other authz denials.
func (s *BeadsServer) authorizeDecisionResolver(w http.ResponseWriter, r *http.Request, id, verb string) bool {
	if allowed, reason := decisionResolverAllowed(r.Context()); !allowed {
		s.auditDenial(r, &AuthzDenial{Action: verb, BeadType: "decision", Message: reason}, id)
		writeError(w, http.StatusForbidden, reason)
		return false
	}
	return true
}

// decisionCallerMayAnswer is decisionResolverAllowed without the reason, for the
// ownership carve-out in authorizeOwnership.
func decisionCallerMayAnswer(ctx context.Context) bool {
	ok, _ := decisionResolverAllowed(ctx)
	return ok
}

// decisionAnswerFields are the decision fields that record an answer. The
// Slack bridge reads `chosen` (and responded_by for attribution) off the
// beads.bead.closed event, and the close cascade unparks the requesting task, so
// whoever can write these on a decision can forge the operator's choice.
var decisionAnswerFields = map[string]bool{
	"chosen":        true,
	"responded_by":  true,
	"responded_at":  true,
	"response_text": true,
	"rationale":     true,
}

// authorizeDecisionAnswerFields is the field-level half of the resolver gate,
// for the generic write paths (PATCH/gRPC update, the single-field endpoints,
// and the extra fields on POST /v1/beads/{id}/close). A caller that fails
// decisionResolverAllowed may not put an answer on a decision, including one it
// filed itself: ownership of the bead is not the right to answer it
// (requester-is-not-resolver). It may still withdraw its own decision with a
// bare close that sets none of these fields. Other bead types are untouched.
func (s *BeadsServer) authorizeDecisionAnswerFields(ctx context.Context, action string, bead *model.Bead, keys ...string) *AuthzDenial {
	if bead == nil || bead.Type != model.TypeDecision || decisionCallerMayAnswer(ctx) {
		return nil
	}
	var changed []string
	for _, k := range keys {
		if decisionAnswerFields[k] {
			changed = append(changed, k)
		}
	}
	if len(changed) == 0 {
		return nil
	}
	sort.Strings(changed)
	return &AuthzDenial{
		Action:   action,
		BeadType: string(model.TypeDecision),
		Message: "forbidden: " + action + " on a decision setting " + strings.Join(changed, ", ") +
			" requires a human session (OIDC or edge-auth) or a bridge token; " +
			"agent and shared-service tokens cannot answer a decision, even one they filed " +
			"(a bare close withdraws it)",
	}
}

// decisionAnswerKeysInPatch returns the answer-field keys present in an update
// patch's fields object.
func decisionAnswerKeysInPatch(patch json.RawMessage) []string {
	if len(patch) == 0 {
		return nil
	}
	var p map[string]json.RawMessage
	if json.Unmarshal(patch, &p) != nil {
		return nil
	}
	return mapKeys(p)
}

// The resolver stamp (gd-ekhj3MJjEa). responded_by is attribution: the
// generic close the Slack bridge answers through never sets it, and any
// principal that may answer a decision may also write it directly. A consumer
// that acts on an answer only when a particular person gave it (the Lands apply
// reactor grants credits on an approval) needs a record the caller cannot
// shape, so every write that sets an answer on a decision (resolve, a close
// carrying fields, PATCH, the field endpoints) also stamps two fields that no
// client may write (decisionStampedKeys):
//
//   - resolver_subject: the authenticated answerer, as decisionAttribution
//     derives it (an OIDC session's email, an edge-auth envelope's subject, a
//     machine's "machine:<client>").
//   - resolver_admin: true only when that answerer authenticated as a human
//     (OIDC or edge-auth) holding gardenio-admin. A relayed operator, a bridge
//     token and a machine principal are never admin here, whatever they carry.
//   - resolver_client: the door the answer came through (callerClient), so an
//     agent on a person's `kd login` is told apart from the person on the
//     page; "" for a fleet or edge caller (gd-EmqfpYQ3NZ).
//
// The stamp is rewritten on every answering write, so a later change to the
// answer never inherits an earlier answerer's stamp.
const (
	decisionResolverSubjectField = "resolver_subject"
	decisionResolverAdminField   = "resolver_admin"
	decisionResolverClientField  = "resolver_client"
)

// decisionResolverIsAdmin reports whether the authenticated caller is a human
// session with gardenio-admin. Machine roles live under their own context key,
// so a machine token never reaches RoleAdmin here.
func decisionResolverIsAdmin(ctx context.Context) bool {
	switch AuthIdentityFromContext(ctx).Type {
	case "oidc", "edge":
		return hasMinimumRole(RolesFromContext(ctx), RoleAdmin)
	}
	return false
}

// writesDecisionAnswer reports whether a write of keys to bead sets an answer
// and so re-stamps the resolver. rationale does not: the bridge adds a thread
// reply to an answered decision that way, and it carries no authority.
func writesDecisionAnswer(bead *model.Bead, keys []string) bool {
	if bead == nil || bead.Type != model.TypeDecision {
		return false
	}
	for _, k := range keys {
		if k == "chosen" || k == "response_text" || k == "responded_by" {
			return true
		}
	}
	return false
}

// decisionResolverStamp is the stamp for an answer ctx's caller writes.
func (s *BeadsServer) decisionResolverStamp(ctx context.Context) map[string]any {
	return map[string]any{
		decisionResolverSubjectField: s.decisionAttribution(ctx, ""),
		decisionResolverAdminField:   decisionResolverIsAdmin(ctx),
		decisionResolverClientField:  callerClient(ctx),
	}
}
