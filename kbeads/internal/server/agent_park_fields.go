package server

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"gardenio/kbeads/internal/model"
)

// agentParkFields are the agent bead fields the Lands purse watcher parks on
// (gardenio/controller/internal/lands/watcher: FieldNudgedAt,
// FieldParkReason). The watcher suspends an agent once its grace from
// purse_nudged_at has run out, so an agent that could write the stamp could
// put it in the future, or clear it every pass, and never be parked
// (gd-qpFpWcWucD). They are the controller's: a per-agent token and a scoped
// principal may not set or change them, on create or on update.
var agentParkFields = []string{"purse_nudged_at", "park_reason"}

// isAgentParkField reports whether k names one of agentParkFields, in any case.
func isAgentParkField(k string) bool {
	for _, p := range agentParkFields {
		if strings.EqualFold(k, p) {
			return true
		}
	}
	return false
}

// agentParkFieldWriter reports whether the caller may write agentParkFields:
// every caller but a per-agent token and a scoped principal.
func agentParkFieldWriter(ctx context.Context) bool {
	if _, agent := agentBeadIDFromContext(ctx); agent {
		return false
	}
	return !isScopedPrincipal(ctx)
}

// agentParkFieldPatchDenial refuses a create body (existing nil) or an update
// patch on an agent bead that changes a park field, unless the caller may
// write them. An echo of the stored value is not a change, and an empty value
// is the same as none.
func agentParkFieldPatchDenial(ctx context.Context, action string, beadType model.BeadType, patch, existing json.RawMessage) *AuthzDenial {
	if beadType != typeAgent || len(patch) == 0 || agentParkFieldWriter(ctx) {
		return nil
	}
	pm, em := fieldsAsMap(patch), fieldsAsMap(existing)
	var changed []string
	for _, k := range agentParkFields {
		if pv, ok := pm[k]; ok && parkFieldText(pv) != parkFieldText(em[k]) {
			changed = append(changed, k)
		}
	}
	return agentParkFieldDenial(action, changed)
}

// agentParkFieldKeyDenial is agentParkFieldPatchDenial for the single-field
// endpoints, which know the key and not the value: touching a park field at
// all is refused.
func agentParkFieldKeyDenial(ctx context.Context, action string, bead *model.Bead, keys ...string) *AuthzDenial {
	if bead == nil || bead.Type != typeAgent || agentParkFieldWriter(ctx) {
		return nil
	}
	var changed []string
	for _, k := range keys {
		if isAgentParkField(k) {
			changed = append(changed, k)
		}
	}
	return agentParkFieldDenial(action, changed)
}

func agentParkFieldDenial(action string, changed []string) *AuthzDenial {
	if len(changed) == 0 {
		return nil
	}
	sort.Strings(changed)
	return &AuthzDenial{
		Action:   action,
		BeadType: string(typeAgent),
		Message:  "insufficient permissions: " + action + " on type agent field(s) " + strings.Join(changed, ", ") + ": the Lands purse watcher's park fields are written only by the controller",
	}
}

// parkFieldText is a park field's value as text, absent and null as "".
func parkFieldText(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	return fmt.Sprint(v)
}
