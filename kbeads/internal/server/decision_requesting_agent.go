package server

import (
	"context"
	"encoding/json"
	"reflect"

	"gardenio/kbeads/internal/model"
)

// Server-stamped requesting agent on a decision (gd-VuYwGGYLLT).
//
// fields.requesting_agent_bead_id names the agent a decision is on behalf of:
// the Slack bridge reads it off the closed decision to respawn that agent, and
// the same-role continuation gate (spawn_continuation_authz.go) lets that
// respawn into a privileged role without an escalation. `gb decision create`
// fills it from the local BOAT_AGENT_BEAD_ID, which is client-controlled, so an
// agent could name a captain's bead in it and have the bridge respawn a captain
// with the agent's own prompt once the decision closed.
//
// The create guard below closes that the way escalation_requesting_agent.go does
// for escalations: a caller on its own per-agent token (whose bead id comes from
// the fleet name the controller minted, never a header) has the field stamped
// from its credential, overriding what the body said, and
// requesting_agent_verified=true records that the stamp is credential-derived.
// Any other caller keeps whatever it supplied in requesting_agent_bead_id (the
// field still drives routing and display) but never gets the marker, and the
// marker is cleared if the body carried one, so only a credential stamp can set
// it. Both fields are immutable after create.
const (
	decisionRequestingAgentField         = "requesting_agent_bead_id"
	decisionRequestingAgentVerifiedField = "requesting_agent_verified"
)

// decisionStampedKeys are the decision fields only the server writes: the
// create guard's requesting agent, the resolver handleResolveDecision
// records (decisionResolverSubjectField, decisionResolverAdminField,
// decisionResolverClientField), and the
// user whose standing consent answered it (standingConsentUserField).
var decisionStampedKeys = map[string]bool{
	decisionRequestingAgentField:         true,
	decisionRequestingAgentVerifiedField: true,
	decisionResolverSubjectField:         true,
	decisionResolverAdminField:           true,
	decisionResolverClientField:          true,
	standingConsentUserField:             true,
}

// decisionCreateAnswerKeys are the answer fields a decision cannot be created
// with by a caller that may not answer it. rationale is left alone: it carries no
// authority, and `gb decision create` may set it.
var decisionCreateAnswerKeys = []string{"chosen", "responded_by", "responded_at", "response_text"}

// applyDecisionRequestingAgentGuard stamps the requesting agent on a decision
// being created and strips anything a caller that may not answer a decision put
// in its answer fields, so a decision cannot be born already answered. Mutates
// bead.Fields in place.
func (s *BeadsServer) applyDecisionRequestingAgentGuard(ctx context.Context, bead *model.Bead) {
	bead.Fields = deleteFieldKey(bead.Fields, decisionRequestingAgentVerifiedField)
	bead.Fields = deleteFieldKey(bead.Fields, decisionResolverSubjectField)
	bead.Fields = deleteFieldKey(bead.Fields, decisionResolverAdminField)
	bead.Fields = deleteFieldKey(bead.Fields, decisionResolverClientField)
	bead.Fields = deleteFieldKey(bead.Fields, standingConsentUserField)
	if beadID, ok := agentBeadIDFromContext(ctx); ok {
		bead.Fields = setField(bead.Fields, decisionRequestingAgentField, beadID)
		bead.Fields = setField(bead.Fields, decisionRequestingAgentVerifiedField, "true")
	}
	if !decisionCallerMayAnswer(ctx) {
		for _, k := range decisionCreateAnswerKeys {
			bead.Fields = deleteFieldKey(bead.Fields, k)
		}
	}
}

// checkDecisionRequestingAgentImmutable rejects (400, via inputError) a PATCH
// that changes either stamped field on a decision. bead.Fields must hold the
// post-merge state and priorFields the blob as it was before the PATCH, so a
// read-modify-write client echoing the stored values back is unaffected.
func checkDecisionRequestingAgentImmutable(bead *model.Bead, priorFields json.RawMessage) error {
	if bead.Type != model.TypeDecision {
		return nil
	}
	prior := fieldsAsMap(priorFields)
	current := fieldsAsMap(bead.Fields)
	for k := range decisionStampedKeys {
		priorVal, priorOK := prior[k]
		currentVal, currentOK := current[k]
		if priorOK != currentOK || !reflect.DeepEqual(priorVal, currentVal) {
			return inputError(k + " is immutable after create")
		}
	}
	return nil
}

// denyDecisionStampedKeyWrite gates the single-field mutation endpoints and the
// extra-fields merge on close, which write by key and so cannot compare values:
// touching a stamped key, or a key of the decision's question
// (decisionQuestionKeys), on a decision at all is refused.
func denyDecisionStampedKeyWrite(action string, bead *model.Bead, keys []string) *AuthzDenial {
	if bead == nil || bead.Type != model.TypeDecision {
		return nil
	}
	for _, k := range keys {
		if decisionStampedKeys[k] {
			return &AuthzDenial{
				Action:   action,
				BeadType: string(model.TypeDecision),
				Message:  "forbidden: " + k + " is stamped by the server and cannot be changed",
			}
		}
		if decisionQuestionKeys[k] {
			return &AuthzDenial{
				Action:   action,
				BeadType: string(model.TypeDecision),
				Message:  "forbidden: " + k + " is part of the question a decision asks and cannot be changed after create; close it and file a new decision instead",
			}
		}
	}
	return nil
}
