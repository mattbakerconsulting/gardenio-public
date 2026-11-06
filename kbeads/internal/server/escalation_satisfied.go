package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// satisfiedCheck reports whether an operation_spec's write is already true of
// the store, and if so the result output that says so. get looks a bead up
// (nil when missing); the check itself writes nothing.
type satisfiedCheck func(spec map[string]json.RawMessage, get func(id string) *model.Bead) (string, bool)

// escalationSatisfiedChecks is keyed by canonical operation_type, then by
// operation_spec.action. A spec with no entry here is never satisfied, so its
// escalation keeps today's reroute and pingback (gd-rnQLRY3bly).
var escalationSatisfiedChecks = map[string]map[string]satisfiedCheck{
	"kbeads_admin_write": {
		"add_label": addLabelSatisfied,
	},
}

// addLabelSatisfied: every bead in spec.bead_ids (the filing convention; else
// spec.beads) exists and already carries spec.label.
func addLabelSatisfied(spec map[string]json.RawMessage, get func(id string) *model.Bead) (string, bool) {
	var ids []string
	var label string
	key := "bead_ids"
	if _, ok := spec[key]; !ok {
		key = "beads"
	}
	if json.Unmarshal(spec[key], &ids) != nil || json.Unmarshal(spec["label"], &label) != nil {
		return "", false
	}
	if label = strings.TrimSpace(label); label == "" || len(ids) == 0 {
		return "", false
	}
	for _, id := range ids {
		b := get(strings.TrimSpace(id))
		if b == nil || !slices.Contains(b.Labels, label) {
			return "", false
		}
	}
	return "already satisfied: " + strings.Join(ids, ", ") + " carry " + label + "; no write needed", true
}

// escalationOperationSpec decodes b's operation_spec, stored as a JSON object
// or as a string holding one.
func escalationOperationSpec(b *model.Bead) map[string]json.RawMessage {
	raw := fieldRaw(b, "operation_spec")
	var s string
	if json.Unmarshal([]byte(raw), &s) == nil {
		raw = s
	}
	var spec map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &spec) != nil {
		return nil
	}
	return spec
}

// escalationAlreadySatisfied reports whether b's structured operation_spec is
// already true of the store, with the result output to record.
func (s *BeadsServer) escalationAlreadySatisfied(ctx context.Context, b *model.Bead) (string, bool) {
	op, _, _ := exactFieldString(b.Fields, "operation_type")
	byAction := escalationSatisfiedChecks[canonicalOperationType(op)]
	if byAction == nil {
		return "", false
	}
	spec := escalationOperationSpec(b)
	var action string
	if spec == nil || json.Unmarshal(spec["action"], &action) != nil {
		return "", false
	}
	check := byAction[strings.TrimSpace(action)]
	if check == nil {
		return "", false
	}
	return check(spec, func(id string) *model.Bead {
		got, err := s.store.GetBead(ctx, id)
		if err != nil {
			return nil
		}
		return got
	})
}

// closeSatisfiedEscalation closes b as a success when it is approved,
// unclaimed and its operation is already satisfied, instead of rerouting or
// pinging it back: nobody need execute it, and its requester may be gone. The
// close runs the usual cascade, so close_requesting_task is honoured. It
// reports whether b was closed.
func (s *BeadsServer) closeSatisfiedEscalation(ctx context.Context, b *model.Bead, actor string) bool {
	if !escalationHasServerApproval(b) {
		return false
	}
	if c, _, _ := exactFieldString(b.Fields, "claimed_by"); strings.TrimSpace(c) != "" {
		return false
	}
	output, ok := s.escalationAlreadySatisfied(ctx, b)
	if !ok {
		return false
	}
	extra := map[string]any{
		"result_status": "success", "result": map[string]string{"output": output},
		"executor": actor, "execution_target": "server",
	}
	if err := s.mergeEscalationFieldsWithHistory(ctx, b, actor, extra); err != nil {
		slog.Warn("escalation routing: failed to record a satisfied result", "escalation", b.ID, "error", err)
		return false
	}
	closed, err := s.store.CloseBead(ctx, b.ID, actor)
	if err != nil || closed == nil {
		slog.Warn("escalation routing: failed to close a satisfied escalation", "escalation", b.ID, "error", err)
		return false
	}
	s.recordAndPublish(ctx, events.TopicBeadClosed, closed.ID, actor, events.BeadClosed{Bead: closed, ClosedBy: actor})
	s.afterBeadClosed(ctx, closed)
	slog.Info("escalation routing: closed an already-satisfied escalation", "escalation", b.ID, "output", output)
	return true
}
