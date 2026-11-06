package server

import (
	"encoding/json"
	"strings"
	"testing"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/model"
)

// TestGRPCUpdateBead_UnknownField_RoutesToKdFields is the end-to-end
// regression for ka-0000000365: `kd update <task> -f bogus=x` rejects the
// unknown field with a message that points at `kd fields task` (the schema
// introspection command) rather than a truncated "+N more" dump. type:task
// carries 75+ fields, so pre-fix the rejection listed 10 and hid the rest —
// exactly the case that made the introspection command worth reaching for but
// never routed the author there.
func TestGRPCUpdateBead_UnknownField_RoutesToKdFields(t *testing.T) {
	srv, ms, ctx := testCtx(t)

	const id = "gd-task-badfield"
	ms.beads[id] = &model.Bead{
		ID:        id,
		Title:     "a task",
		Kind:      model.KindIssue,
		Type:      model.TypeTask,
		Status:    model.StatusOpen,
		CreatedBy: defaultTestSubject,
	}

	// A far-miss unknown field (not close to any real task field).
	patch := json.RawMessage(`{"totally_bogus_zzz":"x"}`)
	_, err := srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{Id: id, Fields: patch})
	if err == nil {
		t.Fatal("expected rejection of unknown field")
	}
	msg := err.Error()
	if !strings.Contains(msg, "invalid fields:") {
		t.Errorf("expected the invalid-fields wrapper, got %q", msg)
	}
	if !strings.Contains(msg, "kd fields task") {
		t.Errorf("rejection should route to `kd fields task`, got %q", msg)
	}
	// The whole point: no truncated dump, no partial inline list. The old
	// far-miss form was "unknown field; valid fields: a, b, ... (+N more)";
	// its distinctive markers are the "; valid fields:" inline prefix and the
	// "(+N more)" truncation suffix. (Match "; valid fields:" precisely, not
	// "valid fields:" — the latter is a substring of the "invalid fields:"
	// wrapper this error is nested inside.)
	if strings.Contains(msg, "more)") || strings.Contains(msg, "; valid fields:") {
		t.Errorf("rejection should not dump a truncated field list, got %q", msg)
	}
}

// TestGRPCCreateBead_UnknownField_RoutesToKdFields is the create-path twin:
// a `kd create --type=task -f bogus=x` rejection must route to `kd fields task`
// too, so the pointer reaches authors on both the create and update choke
// points (both go through model.ValidateTypeFields with the stamped TypeName).
func TestGRPCCreateBead_UnknownField_RoutesToKdFields(t *testing.T) {
	srv, _, ctx := testCtx(t)

	_, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{
		Title:  "a task",
		Type:   "task",
		Fields: json.RawMessage(`{"totally_bogus_zzz":"x"}`),
	})
	if err == nil {
		t.Fatal("expected rejection of unknown field on create")
	}
	msg := err.Error()
	if !strings.Contains(msg, "kd fields task") {
		t.Errorf("create rejection should route to `kd fields task`, got %q", msg)
	}
	if strings.Contains(msg, "more)") || strings.Contains(msg, "; valid fields:") {
		t.Errorf("create rejection should not dump a truncated field list, got %q", msg)
	}
}

// TestGRPCCreateBead_MessageTriggerTitle_Accepted is the end-to-end regression
// for ka-0000005920: the controller's NoticeNotifier (notice.go) marshals
// trigger_title onto every closed type=message notice bead, but the type:message
// builtin schema did not declare it, so createBead's ValidateTypeFields 400'd
// "trigger_title: unknown field" and — because the publish helper swallows the
// create error — the notice was silently dropped (never closed, never posted to
// Slack). Same producer-vs-schema drift class as thread_id / slack_user_id /
// plan_id. This exercises the real create path (resolveTypeConfig falls back to
// the builtin since the mock store has no type:message row, then
// ValidateTypeFields), so it fails if trigger_title is ever dropped from the
// schema again. It mirrors the exact field map NoticeNotifier writes.
func TestGRPCCreateBead_MessageTriggerTitle_Accepted(t *testing.T) {
	srv, _, ctx := testCtx(t)

	fields := json.RawMessage(`{"source_agent":"trigger-engine","text":"a notice","trigger_title":"my-trigger"}`)
	_, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{
		Title:  "a notice",
		Type:   "message",
		Kind:   "data",
		Fields: fields,
	})
	if err != nil {
		t.Fatalf("type=message create with trigger_title should validate against the builtin schema, got %v", err)
	}
}
