// Spec D8 (P7 Task 11, gd-tZYAufHCdo): the daemon owns every escalation
// decision field, and the filing fields are frozen once the escalation
// exists. Both rules hold on create, PATCH (and gRPC UpdateBead), close with
// fields, and the field-edit endpoints, for every caller: a gardenio-admin
// too, as checkEscalationRequestingAgentImmutable (the pattern this replaces)
// already held requesting_agent. The daemon's own verbs (approve, reject,
// result, the dispatch and replay claims) write through the store
// (mergeBeadFields, the replay claim), never through these paths.
package server

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"strings"

	"gardenio/kbeads/internal/model"
)

// escalationServerOnlyFields are written only by the daemon's escalation
// verbs and create guards. requesting_agent and its two markers are the
// create guard's stamp (escalation_requesting_agent.go).
var escalationServerOnlyFields = []string{
	"requesting_agent", escalationRequestingAgentVerifiedField, escalationRequestingAgentProjectField,
	"approved_by", "approved_by_email", "approved_at", "approval_method", "approval_comment", "policy_rule",
	"executor", "execution_dispatch", "replay_in_progress", "result_status", "result", "result_client", "rejected_by", "rejected_client",
}

// escalationFilingFields are what the approver saw. Set at filing, never
// changed afterwards (the filer's merge-spec amend, filerMayAmendMergeSpec,
// is the one exception).
var escalationFilingFields = []string{
	"operation_type", "operation_spec", "target_bead_id", "execution_target", "target_runner",
	"spawn_role", "execution_script", "requires_four_eyes", "approvers", "sensitivity",
}

var escalationD8Fields = append(append([]string{}, escalationServerOnlyFields...), escalationFilingFields...)

// escalationD8Reason returns why key (in any letter case) may not be written
// on an escalation, or "" when it may.
func escalationD8Reason(key string) string {
	for _, n := range escalationServerOnlyFields {
		if strings.EqualFold(key, n) {
			return key + " is written only by the daemon's escalation verbs"
		}
	}
	for _, n := range escalationFilingFields {
		if strings.EqualFold(key, n) {
			return key + " is fixed when an escalation is filed (it is what the approver saw)"
		}
	}
	return ""
}

// checkEscalationFieldsImmutable refuses an update that adds, changes or
// removes a D8 field of an escalation. bead.Fields holds the POST-merge
// state, priorFields the state before, priorStatus the status before.
func checkEscalationFieldsImmutable(ctx context.Context, bead *model.Bead, priorFields json.RawMessage, priorStatus model.Status) error {
	if bead == nil || bead.Type != model.TypeEscalation {
		return nil
	}
	prior := escalationFieldValues(priorFields, escalationD8Fields)
	current := escalationFieldValues(bead.Fields, escalationD8Fields)
	if reflect.DeepEqual(prior, current) || filerMayAmendMergeSpec(ctx, bead, priorStatus, prior, current) {
		return nil
	}
	keys := make([]string, 0, len(prior)+len(current))
	for k := range prior {
		keys = append(keys, k)
	}
	for k := range current {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		pv, pok := prior[k]
		cv, cok := current[k]
		if pok != cok || !reflect.DeepEqual(pv, cv) {
			return inputError(escalationD8Reason(k))
		}
	}
	return nil
}

// checkEscalationFieldWrite is checkEscalationFieldsImmutable for the
// field-level write endpoints (/fields/{f}/edit|append|reorder, /increment)
// and close with fields, which go straight to the store.
func checkEscalationFieldWrite(_ context.Context, bead *model.Bead, fieldKeys ...string) error {
	if bead == nil || bead.Type != model.TypeEscalation {
		return nil
	}
	for _, k := range fieldKeys {
		if reason := escalationD8Reason(k); reason != "" {
			return inputError(reason)
		}
	}
	return nil
}

// checkEscalationCreateFields refuses a client-supplied server-only field at
// create. requesting_agent is accepted: the create guard stamps it for an
// agent and keeps it as routing data for everyone else. The daemon's own
// filings (handleEscalation, machine writes) are exempt.
func checkEscalationCreateFields(ctx context.Context, bead *model.Bead) error {
	if bead.Type != model.TypeEscalation || escalationFrozenFieldsExempt(ctx) {
		return nil
	}
	keys := make([]string, 0)
	for k := range escalationFieldValues(bead.Fields, escalationServerOnlyFields) {
		if !strings.EqualFold(k, "requesting_agent") {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return nil
	}
	sort.Strings(keys)
	return inputError(escalationD8Reason(keys[0]) + "; omit it when filing")
}
