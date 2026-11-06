package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"

	"gardenio/kbeads/internal/model"
)

// Same-role continuation spawn authorization (gd-VuYwGGYLLT).
//
// Once reviewer, gardener, plan-gardener, plan-captain, captain and
// release-captain carry an all-types ownership_grants row, every one of them is
// privileged and authorizeNonHumanSpawn denies a non-OIDC spawn into it as
// escalatable. Most callers never send X-Escalate, so the denial is a plain 403
// and the spawn is dropped: the Slack bridge respawning a decision or
// escalation requester, `gb stop` successor:"new", and any agent that restarts
// its own lane.
//
// Those are not new grants of authority. Each spawn re-creates an agent in the
// role an existing agent bead already holds, in the same project, on behalf of
// that bead. A role can only reach an agent bead through this same gate
// (create and role-update both clear authorizeAgentRole), so the prior bead IS
// the authorization; the continuation inherits it and nothing wider.
//
// fields.spawned_by names the prior bead, and it is a client value, so it
// authorizes nothing on its own. It counts only when the CALLER is a principal
// whose identity comes from its credential and that may speak for a prior
// agent:
//   - the prior agent itself, authenticated with its own per-agent token
//     (agentBeadIDFromContext: the bead id comes from the fleet name the
//     controller minted, never a header), continuing its own lane; or
//   - the slack-bridge service principal, which respawns agents on behalf of
//     the decisions and escalations they were waiting on. The bridge is trusted
//     to relay that a resolution happened, not to say which agent it was for:
//     the field it stamps (spawned_by) and the bead it names (decision_bead_id,
//     escalation_bead_id) are re-read here, and the continuation counts only when
//     that bead is a resolution an agent could not have forged (bridgeEvidence).
//
// Either way the prior bead must still be resumable (priorResumable): a failed,
// expired, killed or retired agent authorizes nothing, whatever token it holds.
//
// The shared static token is never such a caller: every agent pod holds it and
// X-Actor is client-chosen, which is the bypass authorizeNonHumanSpawn exists
// to refuse (gd-HWWEVbCy8N). Everything else falls through to the gate.

// slackBridgeMachineSubject and slackBridgeFleet identify the Slack bridge, as
// a Keycloak machine principal or as a bridge fleet token. A non-minter cannot
// register a token under the reserved fleet name (validateUntrustedMint).
const (
	slackBridgeMachineSubject = machineSubjectPrefix + "gardenio-slack-bridge"
	slackBridgeFleet          = "slack-bridge"
)

type sameRoleContinuationKey struct{}

// sameRoleContinuation is the server-computed authorization for one spawn.
type sameRoleContinuation struct {
	PriorBeadID string
	Caller      string
}

// sameRoleContinuationFromContext reports whether ctx carries a continuation
// authorization. Only withSameRoleContinuationAuth sets it and the key is
// unexported, so no caller-supplied value can forge one.
func sameRoleContinuationFromContext(ctx context.Context) (sameRoleContinuation, bool) {
	v, ok := ctx.Value(sameRoleContinuationKey{}).(sameRoleContinuation)
	return v, ok && v.PriorBeadID != ""
}

// priorBeadRef is the bead id in a fields.spawned_by value: the plain id the
// bridge stamps, or the "successor:<id>" form `gb stop` stamps. Other shapes
// ("trigger:<id>", an agent name, an escalation id) name no prior agent bead.
func priorBeadRef(spawnedBy string) string {
	ref := strings.TrimSpace(spawnedBy)
	ref = strings.TrimPrefix(ref, "successor:")
	if ref == "" || strings.ContainsAny(ref, ": \t") {
		return ""
	}
	return ref
}

// continuationCaller classifies who is asking to continue a prior agent bead.
type continuationCaller int

const (
	callerNone continuationCaller = iota
	// callerSelf is the prior agent itself, on its own per-agent token.
	callerSelf
	// callerBridge is the Slack bridge, relaying a decision or escalation.
	callerBridge
)

// callerMayContinueFor reports whether the authenticated caller may speak for
// the prior agent bead priorID, and in which capacity.
func callerMayContinueFor(ctx context.Context, priorID string) (caller string, kind continuationCaller) {
	if self, isAgent := agentBeadIDFromContext(ctx); isAgent {
		if self != priorID {
			return agentFleetPrefix + self, callerNone
		}
		return agentFleetPrefix + self, callerSelf
	}
	id := AuthIdentityFromContext(ctx)
	switch {
	case authMethodFromContext(ctx) == authMethodMachine && id.Subject == slackBridgeMachineSubject:
		return id.Subject, callerBridge
	case id.Type == "fleet" && FleetFromContext(ctx) == slackBridgeFleet:
		return slackBridgeFleet, callerBridge
	}
	return "", callerNone
}

// Agent states a prior bead can be in and still be continued. A terminal-negative
// state never is. A closed bead is resumable through the bridge only when it
// stopped cleanly: the agent kill path closes a bead without touching
// agent_state, so a closed bead still "working" was killed.
var (
	agentStatesNotResumable   = map[string]bool{"failed": true, "expired": true, "killed": true, "retired": true}
	agentStatesCleanlyStopped = map[string]bool{"done": true, "idle": true, "stopping": true}
)

// priorResumable returns "" when the prior agent bead may be continued by this
// kind of caller, and the reason when it may not.
func priorResumable(prior *model.Bead, kind continuationCaller) string {
	state, _, _ := exactFieldString(prior.Fields, "agent_state")
	if agentStatesNotResumable[state] {
		return "prior_" + state
	}
	if prior.Status != model.StatusClosed {
		return ""
	}
	if kind == callerSelf {
		// An agent whose bead is closed is not running: a token that outlived
		// its bead (killed, retired, finished) must not mint a successor.
		return "prior_closed"
	}
	if !agentStatesCleanlyStopped[state] {
		return "prior_closed_uncleanly"
	}
	return ""
}

// bridgeEvidence returns "" when the bead the bridge names proves a resolution
// the prior agent was waiting on, and the reason it does not otherwise. The
// bridge's own claim is never enough: it respawns whatever its event stream
// names, and a decision's requesting_agent_bead_id is client-writable.
func (s *BeadsServer) bridgeEvidence(ctx context.Context, prior *model.Bead, fields json.RawMessage) string {
	source, _, _ := exactFieldString(fields, "spawn_source")
	switch source {
	case "decision-resume":
		id, _, _ := exactFieldString(fields, "decision_bead_id")
		return s.decisionResolutionEvidence(ctx, prior, id)
	case "escalation-resume":
		id, _, _ := exactFieldString(fields, "escalation_bead_id")
		return s.escalationResolutionEvidence(ctx, prior, id)
	}
	return "no_resolution_evidence"
}

// decisionResolutionEvidence checks that decision id was filed by prior on its
// own credential (requesting_agent_verified, stamped by
// applyDecisionRequestingAgentGuard) and was ANSWERED: chosen or response_text
// is set, fields only a resolver principal can write (authorizeDecisionAnswerFields),
// and a create strips them for everyone else. A bare close leaves both empty, so
// the filing agent cannot close its way to a respawn.
func (s *BeadsServer) decisionResolutionEvidence(ctx context.Context, prior *model.Bead, id string) string {
	if id == "" {
		return "no_decision"
	}
	dec, err := s.store.GetBead(ctx, id)
	if err != nil || dec == nil || dec.Type != model.TypeDecision {
		return "decision_not_found"
	}
	if dec.Status != model.StatusClosed {
		return "decision_not_resolved"
	}
	f := parseFieldsToStringMap(dec.Fields)
	if f[decisionRequestingAgentVerifiedField] != "true" || f[decisionRequestingAgentField] != prior.ID {
		return "decision_not_filed_by_prior"
	}
	chosen := strings.TrimSpace(f["chosen"])
	if (chosen == "" || chosen == "_expired") && strings.TrimSpace(f["response_text"]) == "" {
		return "decision_not_answered"
	}
	// Set fields are not enough: any bridge-shaped fleet token may write them.
	// The answer must be attributed to a human or the Slack bridge itself.
	if !answeredByHumanOrSlackBridge(f["responded_by"]) {
		return "decision_not_answered_by_human"
	}
	return ""
}

// answeredByHumanOrSlackBridge reports whether a decision's responded_by (stamped
// from the authenticated caller, decisionAttribution) names a human (an OIDC
// subject, which is an email, or a slack:U... / oidc:<email> operator) or the
// Slack bridge's own principal. Agent, other-bridge and system:* actors (the
// aging sweep) are not.
func answeredByHumanOrSlackBridge(respondedBy string) bool {
	v := strings.TrimSpace(respondedBy)
	switch {
	case v == "":
		return false
	case v == slackBridgeMachineSubject, v == slackBridgeFleet:
		return true
	case isHumanOperator(v):
		return true
	case isSentinelOperator(v):
		return false
	}
	at := strings.IndexByte(v, '@')
	return at > 0 && at < len(v)-1 && !strings.ContainsAny(v, " \t:")
}

// escalationResolutionEvidence checks that escalation id is a successful or
// partial result for prior's own request. An escalation cannot be generically
// closed (escalationBlocksGenericClose), so a result_status means an approver
// or the executor ran it. The requester is matched on the agent BEAD ID, the
// same key the bridge resolves the requester by, and only when the server
// stamped it from prior's own credential (requesting_agent_verified, see
// applyEscalationRequestingAgentGuard): a static-token filer can type any
// requesting_agent, so an unverified one never authorizes a respawn.
func (s *BeadsServer) escalationResolutionEvidence(ctx context.Context, prior *model.Bead, id string) string {
	if id == "" {
		return "no_escalation"
	}
	esc, err := s.store.GetBead(ctx, id)
	if err != nil || esc == nil || esc.Type != model.TypeEscalation {
		return "escalation_not_found"
	}
	if esc.Status != model.StatusClosed {
		return "escalation_not_resolved"
	}
	f := parseFieldsToStringMap(esc.Fields)
	if rs := f["result_status"]; rs != "success" && rs != "partial" {
		return "escalation_not_resolved"
	}
	if f[escalationRequestingAgentVerifiedField] != "true" || f["requesting_agent"] != prior.ID {
		return "escalation_not_filed_by_prior"
	}
	return ""
}

// withSameRoleContinuationAuth returns ctx stamped with the continuation
// authorization when this agent create qualifies, and ctx unchanged otherwise.
// It never fails a request: a spawn that does not qualify meets the
// privileged-role gate as it always did.
//
// The create qualifies only when ALL hold:
//   - the caller may speak for the prior bead (callerMayContinueFor);
//   - fields.spawned_by names an existing agent bead in the SAME project, and
//     that bead is resumable (priorResumable);
//   - through the bridge, the bead it names proves a resolution the prior agent
//     was waiting on (bridgeEvidence);
//   - every requested role element is a role that prior bead holds, so the
//     continuation can narrow the role but never widen it.
func (s *BeadsServer) withSameRoleContinuationAuth(ctx context.Context, fields json.RawMessage) context.Context {
	spawnedBy, _, err := exactFieldString(fields, "spawned_by")
	if err != nil {
		return ctx
	}
	priorID := priorBeadRef(spawnedBy)
	if priorID == "" {
		return ctx
	}
	caller, kind := callerMayContinueFor(ctx, priorID)
	if kind == callerNone {
		return ctx
	}
	deny := func(reason string) context.Context {
		slog.Info("spawn authorization: same-role continuation not applied",
			"prior", priorID, "reason", reason, "caller", caller)
		return ctx
	}

	prior, err := s.store.GetBead(ctx, priorID)
	if err != nil || prior == nil {
		return deny("prior_not_found")
	}
	if prior.Type != model.BeadType("agent") {
		return deny("prior_not_an_agent")
	}
	if reason := priorResumable(prior, kind); reason != "" {
		return deny(reason)
	}
	project := projectFromFields(fields)
	if project == "" || projectFromFields(prior.Fields) != project {
		return deny("project_mismatch")
	}
	requested, _, rErr := exactFieldString(fields, agentRoleField)
	held := parseRoleList(roleFromFields(prior.Fields))
	want := parseRoleList(requested)
	if rErr != nil || len(want) == 0 || len(held) == 0 {
		return deny("role_missing")
	}
	for _, r := range want {
		if !containsOrWildcardExact(held, r) {
			return deny("role_not_held_by_prior")
		}
	}
	if kind == callerBridge {
		if reason := s.bridgeEvidence(ctx, prior, fields); reason != "" {
			return deny(reason)
		}
	}
	return context.WithValue(ctx, sameRoleContinuationKey{}, sameRoleContinuation{PriorBeadID: priorID, Caller: caller})
}

// containsOrWildcardExact reports whether role is one of held. Unlike
// containsOrWildcard it gives "*" no meaning: a role string is never a pattern.
func containsOrWildcardExact(held []string, role string) bool {
	for _, h := range held {
		if h == role {
			return true
		}
	}
	return false
}
