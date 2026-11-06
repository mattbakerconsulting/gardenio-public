package server

import (
	"encoding/json"
	"testing"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/model"
)

// The closed+enabled DRAIN sweep (gardenio controller,
// internal/bridge/closed_enabled_drain.go) persists its crash-safety /
// idempotency state on beads, not in memory. It writes three fields:
//
//   - drain_candidate_ids  — JSON array of trigger IDs, on a type=decision at CREATE
//   - drain_executed_at     — RFC3339 stamp, on a type=decision at UPDATE
//   - drain_reviewed_at     — RFC3339 stamp, on a type=trigger at UPDATE
//
// None were registered in the builtin type schemas, so every write 400'd
// "unknown field" server-side (gd-Ghs16gezXA): the grouped decision was never
// filed, exactly-once execution never gated, and the keep-all cooldown never
// applied — all silently, because the sweep logs-and-continues on the 400.
//
// The controller's own closed_enabled_drain_test.go could not catch this: its
// mockTriggerDaemon accepts arbitrary field maps and performs no schema
// validation, so the 18 tests passed against a mock that cannot reproduce the
// daemon's rejection. These tests close that gap on the daemon side, exercising
// the real create/update choke points (both route through
// model.ValidateTypeFields with the resolved TypeConfig) so the next field the
// sweep adds cannot silently repeat the bug.

// drainDecisionFields are the fields the sweep writes on the grouped
// type=decision bead: drain_candidate_ids at create, drain_executed_at at the
// post-resolution update.
var drainDecisionFields = []struct {
	name string
	typ  model.FieldType
}{
	{"drain_candidate_ids", model.FieldTypeJSON},
	{"drain_executed_at", model.FieldTypeString},
}

// TestResolveTypeConfig_DrainDecisionFields asserts the two decision-bead
// fields are present (with the right types) in the builtin type:decision
// schema, so a fresh seed / snapshot restore cannot drop them.
func TestResolveTypeConfig_DrainDecisionFields(t *testing.T) {
	srv, _, ctx := testCtx(t)

	tc, err := srv.resolveTypeConfig(ctx, "decision")
	if err != nil {
		t.Fatalf("resolveTypeConfig(decision): %v", err)
	}
	if tc == nil {
		t.Fatal("type:decision config missing from builtins")
	}
	byName := make(map[string]model.FieldDef, len(tc.Fields))
	for _, f := range tc.Fields {
		byName[f.Name] = f
	}
	for _, want := range drainDecisionFields {
		f, ok := byName[want.name]
		if !ok {
			t.Errorf("type:decision missing built-in field %q (drain sweep writes it)", want.name)
			continue
		}
		if f.Type != want.typ {
			t.Errorf("type:decision field %q type=%q, want %q", want.name, f.Type, want.typ)
		}
	}
}

// TestResolveTypeConfig_DrainTriggerField asserts the trigger-bead cooldown
// stamp is present (as a string) in the builtin type:trigger schema.
func TestResolveTypeConfig_DrainTriggerField(t *testing.T) {
	srv, _, ctx := testCtx(t)

	tc, err := srv.resolveTypeConfig(ctx, "trigger")
	if err != nil {
		t.Fatalf("resolveTypeConfig(trigger): %v", err)
	}
	if tc == nil {
		t.Fatal("type:trigger config missing from builtins")
	}
	var found bool
	for _, f := range tc.Fields {
		if f.Name == "drain_reviewed_at" {
			found = true
			if f.Type != model.FieldTypeString {
				t.Errorf("type:trigger drain_reviewed_at type=%q, want string", f.Type)
			}
			break
		}
	}
	if !found {
		t.Error("type:trigger missing built-in field drain_reviewed_at (drain sweep writes it)")
	}
}

// TestResolveTypeConfig_PipelineRefPatternField asserts the GitLab pipeline
// trigger's pipeline_ref_pattern field (gd-zndckdlsyb) is a string on the
// builtin type:trigger schema. The gardenio controller writes it on
// gitlab-pipeline trigger beads to suppress non-train ref firings; if the
// daemon's schema does not list it, EVERY write that sets it 400s with
// "pipeline_ref_pattern: unknown field" (type:trigger does not
// allowAdditionalFields). pipeline_max_age and min_duration are asserted
// alongside it because all three are pipeline-source string fields the gardenio
// gitlab-pipeline trigger authors. min_duration (ka-0000001474) was the exact
// regression this guards against: its feature commit (3562b73d7) registered the
// field in the controller/gb/viewer but NOT here, so `gb trigger create
// gitlab-pipeline --min-duration 45m` 400'd with "min_duration: unknown field"
// against every real daemon until it was added.
func TestResolveTypeConfig_PipelineRefPatternField(t *testing.T) {
	srv, _, ctx := testCtx(t)

	tc, err := srv.resolveTypeConfig(ctx, "trigger")
	if err != nil {
		t.Fatalf("resolveTypeConfig(trigger): %v", err)
	}
	if tc == nil {
		t.Fatal("type:trigger config missing from builtins")
	}
	byName := map[string]model.FieldType{}
	for _, f := range tc.Fields {
		byName[f.Name] = f.Type
	}
	for _, name := range []string{"pipeline_ref_pattern", "pipeline_max_age", "min_duration"} {
		typ, ok := byName[name]
		if !ok {
			t.Errorf("type:trigger missing built-in field %q", name)
			continue
		}
		if typ != model.FieldTypeString {
			t.Errorf("type:trigger %s type=%q, want string", name, typ)
		}
	}
}

// TestCreateDecision_DrainCandidateIDs is the end-to-end create-path regression:
// filing the grouped drain decision (fileDrainDecision) with drain_candidate_ids
// in the fields blob must NOT 400. Pre-fix this create was rejected whole, so
// the grouped decision was never filed (bug fix 2 non-functional in prod).
func TestCreateDecision_DrainCandidateIDs(t *testing.T) {
	srv, _, ctx := testCtx(t)

	ids, _ := json.Marshal([]string{"gd-aaa", "gd-bbb"})
	options, _ := json.Marshal([]map[string]string{
		{"id": "archive-all", "short": "Archive all", "label": "Disable all 2 candidate trigger(s)"},
		{"id": "keep-all", "short": "Keep all", "label": "Leave all enabled; re-review in 7 days"},
	})
	fields, _ := json.Marshal(map[string]any{
		"prompt":              "trigger-reviver drain: archive 2 trigger(s)?",
		"options":             json.RawMessage(options),
		"drain_candidate_ids": json.RawMessage(ids),
	})
	_, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{
		Title:  "trigger-reviver drain",
		Type:   "decision",
		Kind:   "data",
		Fields: fields,
	})
	if err != nil {
		t.Fatalf("CreateBead(decision, drain_candidate_ids) rejected: %v", err)
	}
}

// TestUpdateDecision_DrainExecutedAt is the end-to-end update-path regression
// for the exactly-once execution gate: stamping drain_executed_at on a resolved
// decision must NOT 400. Pre-fix the stamp was rejected, so the field stayed
// empty forever and a resolved decision re-executed on every sweep run.
func TestUpdateDecision_DrainExecutedAt(t *testing.T) {
	srv, ms, ctx := testCtx(t)

	const id = "gd-drain-decision"
	ms.beads[id] = &model.Bead{
		ID:     id,
		Title:  "trigger-reviver drain",
		Kind:   model.KindData,
		Type:   "decision",
		Status: model.StatusClosed,
		// prompt is required on type=decision; the sweep only ever stamps
		// drain_executed_at on a decision it already filed with a prompt, so
		// the post-merge effective state carries it.
		Fields:    json.RawMessage(`{"prompt":"trigger-reviver drain"}`),
		CreatedBy: defaultTestSubject,
	}
	patch := json.RawMessage(`{"drain_executed_at":"2026-07-29T08:00:00Z"}`)
	if _, err := srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{Id: id, Fields: patch}); err != nil {
		t.Fatalf("UpdateBead(decision, drain_executed_at) rejected: %v", err)
	}
}

// TestUpdateTrigger_DrainReviewedAt is the end-to-end update-path regression for
// the keep-all cooldown: stamping drain_reviewed_at on a kept trigger must NOT
// 400. Pre-fix the stamp was rejected, so parseLivenessTimestamp read empty and
// the 7-day cooldown never suppressed a re-proposed batch. This is the exact
// write that surfaced the bug (agent gd-REJqMla2TI, executing decision
// gd-TVD4fXrvd3).
func TestUpdateTrigger_DrainReviewedAt(t *testing.T) {
	srv, ms, ctx := testCtx(t)

	const id = "gd-drain-trigger"
	ms.beads[id] = &model.Bead{
		ID:     id,
		Title:  "some closed+enabled trigger",
		Kind:   model.KindData,
		Type:   "trigger",
		Status: model.StatusClosed,
		// action + source are required on type=trigger; a real trigger the
		// sweep stamps already carries them, so the post-merge state does too.
		Fields:    json.RawMessage(`{"action":"spawn_agent","source":"schedule"}`),
		CreatedBy: defaultTestSubject,
	}
	patch := json.RawMessage(`{"drain_reviewed_at":"2026-07-29T08:00:00Z"}`)
	if _, err := srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{Id: id, Fields: patch}); err != nil {
		t.Fatalf("UpdateBead(trigger, drain_reviewed_at) rejected: %v", err)
	}
}
