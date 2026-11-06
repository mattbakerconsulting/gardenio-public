package server

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"gardenio/kbeads/internal/model"
)

// mergeEscalationFieldsWithHistory merges extra into prior's fields and writes
// one mutation_history row per changed key (field "fields.<key>", values
// JSON-encoded), so approve, reject and result leave the same audit trail as a
// PATCH (P7 Task 15). No dibs flip: approving or resolving is not taking
// ownership.
func (s *BeadsServer) mergeEscalationFieldsWithHistory(ctx context.Context, prior *model.Bead, actor string, extra map[string]any) error {
	if err := s.mergeBeadFields(ctx, prior.ID, extra); err != nil {
		return err
	}
	old := map[string]json.RawMessage{}
	if len(prior.Fields) > 0 {
		_ = json.Unmarshal(prior.Fields, &old)
	}
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		nv, _ := json.Marshal(extra[k])
		if string(old[k]) == string(nv) {
			continue
		}
		s.recordMutationNoDibs(ctx, prior.ID, actor, "fields."+k, string(old[k]), string(nv), model.MutationTypeUpdate)
	}
	return nil
}

// approvalMethodFor derives a human approval's approval_method from the
// authenticated identity (spec D12). The request body's value is logged and
// ignored: a caller could otherwise record any method it liked.
func approvalMethodFor(ctx context.Context) string {
	if AuthIdentityFromContext(ctx).Type == "edge" {
		return "edge"
	}
	return "web_ui"
}

// checkEscalationOperationSpec refuses an escalation the daemon would hand to
// an executor (a server replay, or a local or service_account target) whose
// operation_spec does not say what to do. gb sends {} by default, which stays
// valid for verdict records and manual requests. A close is the exception
// among replays: target_bead_id names the bead and closed_by is optional, so
// an empty object is a complete close (an auto-filed denied close carries the
// request body, often {}).
func checkEscalationOperationSpec(bead *model.Bead) error {
	if bead == nil || bead.Type != model.TypeEscalation {
		return nil
	}
	f := escalationStringFields(bead)
	op := f["operation_type"]
	replay := isServerReplayOperation(op)
	if !replay && escalationDispatchOf(bead) == dispatchNone {
		return nil
	}
	n, isObject := operationSpecKeys(bead)
	if isObject && (n > 0 || normOp(op) == "close") {
		return nil
	}
	return inputError("operation_spec must be a non-empty JSON object for an escalation that names an executor or is replayed by the daemon (" + strings.TrimSpace(op) + ")")
}

// operationSpecKeys reports whether bead's operation_spec is a JSON object (or
// a string holding one) and how many keys it has.
func operationSpecKeys(bead *model.Bead) (int, bool) {
	raw := fieldRaw(bead, "operation_spec")
	if raw == "" {
		return 0, false
	}
	var s string
	if json.Unmarshal([]byte(raw), &s) == nil {
		raw = s
	}
	var m map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &m) != nil || m == nil {
		return 0, false
	}
	return len(m), true
}
