package server

import (
	"encoding/json"
	"testing"

	"gardenio/kbeads/internal/model"
)

// baseInvestigationRequest returns the minimal valid create body for a
// type=investigation-request bead: the two required author fields, plus a
// project label so project-scoped write policy resolves cleanly.
func baseInvestigationRequest() map[string]any {
	return map[string]any{
		"title":  "investigate readmission spike",
		"type":   "investigation-request",
		"labels": []string{"project:gardenio"},
		"fields": map[string]any{
			"prompt": "Why did 30-day readmissions spike in Q2 for tenant acme?",
			"data_scope": map[string]any{
				"tenant": "acme",
				"db":     "clinical_ro",
				"tables": []string{"encounters", "diagnoses"},
			},
			"origin_bead": "gd-origin01",
		},
	}
}

// TestInvestigationRequestTypeRegistered verifies a low-privilege create of a
// type=investigation-request succeeds and is born pending — the whole point of
// B1: any agent may create one, and it does nothing until approved.
func TestInvestigationRequestTypeRegistered(t *testing.T) {
	_, _, h := newTestServer()
	rec := doJSON(t, h, "POST", "/v1/beads", baseInvestigationRequest())
	requireStatus(t, rec, 201)

	var resp map[string]any
	decodeJSON(t, rec, &resp)
	if resp["type"] != "investigation-request" {
		t.Fatalf("expected type=investigation-request, got %v", resp["type"])
	}
	fields, _ := resp["fields"].(map[string]any)
	if fields["approval_status"] != "pending" {
		t.Errorf("fresh request must be born pending, got approval_status=%v", fields["approval_status"])
	}
}

// TestInvestigationRequestTypeConfig verifies the builtin type config is served
// with kind=data (so it is discoverable via GET /v1/configs and the CLI).
func TestInvestigationRequestTypeConfig(t *testing.T) {
	_, _, h := newTestServer()
	rec := doJSON(t, h, "GET", "/v1/configs/type:investigation-request", nil)
	requireStatus(t, rec, 200)

	var cfg struct {
		Key   string          `json:"key"`
		Value json.RawMessage `json:"value"`
	}
	decodeJSON(t, rec, &cfg)
	if cfg.Key != "type:investigation-request" {
		t.Fatalf("expected key=type:investigation-request, got %q", cfg.Key)
	}
	var tc map[string]any
	if err := json.Unmarshal(cfg.Value, &tc); err != nil {
		t.Fatalf("decode type config value: %v", err)
	}
	if tc["kind"] != "data" {
		t.Fatalf("expected kind=data, got %v", tc["kind"])
	}
}

// TestInvestigationRequestRequiresPromptAndScope verifies the two required
// author fields are enforced by the type schema.
func TestInvestigationRequestRequiresPromptAndScope(t *testing.T) {
	for _, tc := range []struct {
		name string
		drop string
	}{
		{"missing prompt", "prompt"},
		{"missing data_scope", "data_scope"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, h := newTestServer()
			body := baseInvestigationRequest()
			delete(body["fields"].(map[string]any), tc.drop)
			rec := doJSON(t, h, "POST", "/v1/beads", body)
			requireStatus(t, rec, 400)
		})
	}
}

// TestInvestigationRequestRejectsApprovalFieldsAtCreate is the core fail-closed
// guard: a requester must NOT be able to arrive already-approved / already-run.
// Every forbidden lifecycle field, set at create, must 400.
func TestInvestigationRequestRejectsApprovalFieldsAtCreate(t *testing.T) {
	forbidden := []struct {
		field string
		value any
	}{
		{"approval_status", "approved"},
		{"approved_by", "gd-user-evil"},
		{"approved_at", "2026-07-21T00:00:00Z"},
		{"approval_method", "web_ui"},
		{"denied_by", "gd-user-evil"},
		{"run_id", "run-forged"},
		{"run_status", "success"},
		{"launched_at", "2026-07-21T00:00:00Z"},
		{"beads_doc_id", "gd-forgeddoc"},
		{"confidence", "0.99"},
		{"redacted_summary", "all clear (forged)"},
		{"release_status", "released"},
	}
	for _, f := range forbidden {
		t.Run(f.field, func(t *testing.T) {
			_, _, h := newTestServer()
			body := baseInvestigationRequest()
			body["fields"].(map[string]any)[f.field] = f.value
			rec := doJSON(t, h, "POST", "/v1/beads", body)
			requireStatus(t, rec, 400)
		})
	}
}

// TestInvestigationRequestApprovalStatusPendingAllowed verifies that an
// EXPLICIT approval_status=pending at create is accepted (it is the birth
// state) — only "approved"/"denied" are rejected.
func TestInvestigationRequestApprovalStatusPendingAllowed(t *testing.T) {
	_, _, h := newTestServer()
	body := baseInvestigationRequest()
	body["fields"].(map[string]any)["approval_status"] = "pending"
	rec := doJSON(t, h, "POST", "/v1/beads", body)
	requireStatus(t, rec, 201)
}

// TestInvestigationRequestStampsRequestedBy verifies requested_by is
// server-stamped from the authenticated identity and cannot be forged by the
// client — provenance of who asked must be un-spoofable.
func TestInvestigationRequestStampsRequestedBy(t *testing.T) {
	_, _, h := newTestServerAuth()
	body := baseInvestigationRequest()
	// Client attempts to forge a different requester.
	body["fields"].(map[string]any)["requested_by"] = "gd-someone-else"
	rec := doJSONAs(t, h, "POST", "/v1/beads", body, "alice@example.com")
	requireStatus(t, rec, 201)

	var resp map[string]any
	decodeJSON(t, rec, &resp)
	fields, _ := resp["fields"].(map[string]any)
	if fields["requested_by"] != "alice@example.com" {
		t.Errorf("requested_by must be server-stamped to the authenticated identity, got %v", fields["requested_by"])
	}
}

// TestInvestigationRequestForbiddenFieldsCoverAllLifecycle is a unit test over
// the guard's forbidden-set so the list can't silently drift below the
// approval/launch/egress fields the schema declares.
func TestInvestigationRequestForbiddenFieldsCoverAllLifecycle(t *testing.T) {
	for _, want := range []string{
		"approved_by", "approved_by_email", "approved_at", "approval_method",
		"denied_by", "denied_at", "denial_reason",
		"run_id", "run_status", "launched_at", "launched_by",
		"confidence", "beads_doc_id", "redacted_summary",
	} {
		if _, ok := investigationRequestCreateForbiddenFields[want]; !ok {
			t.Errorf("forbidden-at-create set missing lifecycle field %q", want)
		}
	}
	// prompt / data_scope / origin_bead / requesting_agent / context are author
	// fields and must NOT be forbidden.
	for _, ok := range []string{"prompt", "data_scope", "origin_bead", "requesting_agent", "context"} {
		if _, bad := investigationRequestCreateForbiddenFields[ok]; bad {
			t.Errorf("author field %q must not be create-forbidden", ok)
		}
	}
}

// TestBuiltinInvestigationRequestLifecycleFieldsDeclared guards the recurring
// writer-vs-schema drift class (the same class as the thread/message/account
// guards in builtin_types_test.go): the Phase B launcher, Phase C egress
// worker, and Phase D portal each stamp lifecycle fields onto a request bead.
// If the schema drops any, that privileged writer's update 400s
// "<field>: unknown field" and the approval/launch/egress step silently fails.
func TestBuiltinInvestigationRequestLifecycleFieldsDeclared(t *testing.T) {
	c, ok := builtinConfigs["type:investigation-request"]
	if !ok {
		t.Fatal("builtinConfigs missing \"type:investigation-request\"")
	}
	var tc model.TypeConfig
	if err := json.Unmarshal(c.Value, &tc); err != nil {
		t.Fatalf("unmarshal config value: %v", err)
	}
	have := make(map[string]struct{}, len(tc.Fields))
	for _, f := range tc.Fields {
		have[f.Name] = struct{}{}
	}
	// The union of author fields + every downstream lifecycle field the Phase
	// B/C/D writers stamp. Keep in sync with the schema and the guard.
	wantFields := []string{
		// Author (create).
		"prompt", "data_scope", "origin_bead", "requesting_agent", "requested_by", "context",
		// Approval (Phase D portal).
		"approval_status", "approved_by", "approved_by_email", "approved_at", "approval_method",
		"denied_by", "denied_at", "denial_reason",
		// Launch (Phase B2 launcher).
		"run_id", "run_status", "launched_at", "launched_by",
		// Egress (Phase C1 worker).
		"confidence", "beads_doc_id", "redacted_summary",
	}
	for _, want := range wantFields {
		if _, ok := have[want]; !ok {
			t.Errorf("type:investigation-request schema missing field %q (a Phase B/C/D writer stamps it; absence 400s the write)", want)
		}
	}
	// prompt and data_scope must be required.
	byName := make(map[string]model.FieldDef, len(tc.Fields))
	for _, f := range tc.Fields {
		byName[f.Name] = f
	}
	for _, req := range []string{"prompt", "data_scope"} {
		if !byName[req].Required {
			t.Errorf("field %q must be required", req)
		}
	}
	// Low-privilege create: no min_role gate (any agent may create).
	if tc.WritePolicy != nil && tc.WritePolicy.MinRole != "" {
		t.Errorf("investigation-request create must be low-privilege (no min_role), got min_role=%q", tc.WritePolicy.MinRole)
	}
}
