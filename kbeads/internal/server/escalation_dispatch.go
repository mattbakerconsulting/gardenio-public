package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"gardenio/kbeads/internal/model"
)

// The one executor of an approved escalation (P7 Task 12, spec D10), recorded
// in fields.execution_dispatch by the approve handlers before anything runs.
const (
	dispatchServer         = "server"
	dispatchLocal          = "local"
	dispatchServiceAccount = "service_account"
	dispatchNone           = "none"
)

// escalationDispatchFor picks the single executor for an approved
// escalation (spec D10). Verdict records, replay-unfit types (decision
// gd-UQTnDZATTU, even one some write path gave a target) and escalations with
// no executor are "none": the daemon dispatches nothing and a human or
// reviewer posts the result. There is no fallback from local to spawning a
// crew.
func escalationDispatchFor(f map[string]string) string {
	op := f["operation_type"]
	switch {
	case isVerdictOperation(op), isReplayUnfitOperation(op):
		return dispatchNone
	case isServerReplayOperation(op) && strings.TrimSpace(f["operation_spec"]) != "":
		return dispatchServer
	case f["execution_target"] == "service_account":
		return dispatchServiceAccount
	case f["execution_target"] == "local":
		return dispatchLocal
	}
	return dispatchNone
}

// claimEscalationDispatch claims bead's dispatch. proceed=false means another
// approve already dispatched it (caller returns 409). A repeated server
// dispatch proceeds: replayEscalation's own claim serialises the retry of a
// failed replay. A repeated "none" proceeds too, since nothing executes.
func (s *BeadsServer) claimEscalationDispatch(ctx context.Context, bead *model.Bead) (string, bool, error) {
	return s.claimEscalationDispatchAs(ctx, bead, escalationDispatchOf(bead))
}

// claimEscalationDispatchAs is claimEscalationDispatch for a dispatch other
// than the one bead's fields name (a refused executor pod claims none).
func (s *BeadsServer) claimEscalationDispatchAs(ctx context.Context, bead *model.Bead, want string) (string, bool, error) {
	claimed, current, err := s.store.ClaimEscalationExecution(ctx, bead.ID, want)
	if err != nil {
		return "", false, fmt.Errorf("claim escalation dispatch: %w", err)
	}
	if claimed {
		// ClaimEscalationExecution writes the field outside mergeBeadFields.
		s.recordMutationNoDibs(ctx, bead.ID, actorFromContext(ctx), "fields.execution_dispatch", "", `"`+want+`"`, model.MutationTypeUpdate)
		return want, true, nil
	}
	if current == want && (want == dispatchServer || want == dispatchNone) {
		return want, true, nil
	}
	return current, false, nil
}

// escalationDispatchOf is escalationDispatchFor over bead's fields, with the
// JSON operation_spec included (escalationStringFields skips non-strings).
func escalationDispatchOf(bead *model.Bead) string {
	f := escalationStringFields(bead)
	if raw := fieldRaw(bead, "operation_spec"); raw != "" {
		f["operation_spec"] = raw
	}
	return escalationDispatchFor(f)
}

// fieldRaw returns the raw JSON text of b's field key when it holds an
// object, array, or non-empty string; "" for absent, null, or "". An empty
// object counts: a close replays with operation_spec {} (replayEscalation's
// own gate is only "non-empty raw bytes").
func fieldRaw(b *model.Bead, key string) string {
	if b == nil || len(b.Fields) == 0 {
		return ""
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(b.Fields, &m) != nil {
		return ""
	}
	raw := bytes.TrimSpace(m[key])
	var v any
	if len(raw) == 0 || json.Unmarshal(raw, &v) != nil {
		return ""
	}
	switch t := v.(type) {
	case map[string]any, []any:
	case string:
		if strings.TrimSpace(t) == "" {
			return ""
		}
	default:
		return ""
	}
	return string(raw)
}

// withEscalationTarget returns a copy of b whose fields carry
// execution_target=target, for choosing the dispatch an approve-time
// override asks for before the override is merged.
func withEscalationTarget(b *model.Bead, target string) *model.Bead {
	if target == "" {
		return b
	}
	m := map[string]json.RawMessage{}
	if len(b.Fields) > 0 {
		_ = json.Unmarshal(b.Fields, &m)
	}
	m["execution_target"], _ = json.Marshal(target)
	c := *b
	c.Fields, _ = json.Marshal(m)
	return &c
}
