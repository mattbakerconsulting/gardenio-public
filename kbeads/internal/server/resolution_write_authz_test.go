package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// Regression tests for ka-0000002666: the escalation/decision resolution WRITE
// verbs (approve/reject/result, resolve/cancel) must enforce the SAME
// cross-project scope gate that GET /v1/escalations/{id} and
// GET /v1/decisions/{id} enforce. Before this fix they were wrapped only in
// requireRole(RoleOperator, …), which short-circuits with zero checks for any
// non-OIDC caller — so a project-bound fleet token scoped to project A could
// merge fields into, update, and close (and on approve, replay the denied
// privileged op against) an escalation/decision living in project B, even
// though the by-id READ twin returns 403 on the same token.
//
// These mirror the read-side denial tests in http_relations_read_authz_test.go
// and the by-id read gate in read_authz_scope.go.

// resolutionAuthzMux registers the five resolution write routes on the server
// under test WITHOUT AuthMiddleware, so tests inject the fleet-token / OIDC
// context directly (isolating authorizeResolutionTarget from the middleware).
func resolutionAuthzMux(s *BeadsServer) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/escalations/{id}/approve", s.handleApproveEscalation)
	mux.HandleFunc("POST /v1/escalations/{id}/reject", s.handleRejectEscalation)
	mux.HandleFunc("POST /v1/escalations/{id}/result", s.handleCaptureEscalationResult)
	mux.HandleFunc("POST /v1/decisions/{id}/resolve", s.handleResolveDecision)
	mux.HandleFunc("POST /v1/decisions/{id}/cancel", s.handleCancelDecision)
	return mux
}

// resolutionAuthzFixture wires a server with one escalation and one decision
// bead, both labeled project:<targetProject>. Populate a user bead so an OIDC
// caller's allowed_projects can be evaluated.
func resolutionAuthzFixture(t *testing.T, targetProject string) (*mockStore, http.Handler) {
	t.Helper()
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	s.userProvisioner = nil
	h := resolutionAuthzMux(s)

	escFields, _ := json.Marshal(map[string]any{
		"operation_type": "create",
		"operation_spec": json.RawMessage(`{"title":"forbidden","type":"task"}`),
		"context":        "denied privileged op",
	})
	ms.beads["gd-esc-target"] = &model.Bead{
		ID: "gd-esc-target", Kind: model.KindIssue, Type: model.TypeEscalation,
		Title: "Escalation: create on type task denied", Status: model.StatusOpen,
		Fields: escFields,
	}
	ms.labels["gd-esc-target"] = []string{"project:" + targetProject}

	decFields, _ := json.Marshal(map[string]any{
		"prompt":  "Deploy?",
		"options": json.RawMessage(`[{"id":"y","label":"Yes"}]`),
	})
	ms.beads["gd-dec-target"] = &model.Bead{
		ID: "gd-dec-target", Kind: model.KindData, Type: model.TypeDecision,
		Title: "Deploy?", Status: model.StatusOpen, Fields: decFields,
	}
	ms.labels["gd-dec-target"] = []string{"project:" + targetProject}

	return ms, h
}

// resolutionCases enumerates the five resolution write verbs with a minimal
// valid body for each. All target the seeded escalation/decision beads.
type resolutionCase struct {
	name string
	path string
	body map[string]any
}

func resolutionCases() []resolutionCase {
	return []resolutionCase{
		{"approve", "/v1/escalations/gd-esc-target/approve", map[string]any{"approved_by": "op@example.com"}},
		{"reject", "/v1/escalations/gd-esc-target/reject", map[string]any{"rejected_by": "op@example.com", "reason": "no"}},
		{"result", "/v1/escalations/gd-esc-target/result", map[string]any{"result_status": "success", "executor": "op@example.com"}},
		{"resolve", "/v1/decisions/gd-dec-target/resolve", map[string]any{"selected_option": "y"}},
		{"cancel", "/v1/decisions/gd-dec-target/cancel", map[string]any{"reason": "obsolete"}},
	}
}

func postJSON(h http.Handler, path string, body map[string]any, mutate func(*http.Request)) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if mutate != nil {
		mutate(req)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestResolutionWrites_DenyCrossProjectFleetToken is the core bypass
// reproduction: a fleet token scoped to project "otherproject" must NOT be
// able to resolve escalation/decision beads in "gardenio". Each verb answers
// 404, as for a missing bead (gd-7APTAIhdC5).
func TestResolutionWrites_DenyCrossProjectFleetToken(t *testing.T) {
	for _, tc := range resolutionCases() {
		t.Run(tc.name, func(t *testing.T) {
			ms, h := resolutionAuthzFixture(t, "gardenio")

			rec := postJSON(h, tc.path, tc.body, func(r *http.Request) {
				*r = *r.WithContext(WithTokenProject(r.Context(), "otherproject"))
			})

			if rec.Code != http.StatusNotFound {
				t.Fatalf("%s: expected 404 (as missing) for cross-project fleet token, got %d; body: %s",
					tc.name, rec.Code, rec.Body.String())
			}
			// The mutation must not have landed: the target bead stays open and
			// no replay-created bead exists.
			assertResolutionTargetsUntouched(t, ms)
		})
	}
}

// TestResolutionWrites_DenyCrossProjectOIDC covers the OIDC-caller arm of the
// same gate: an operator whose allowed_projects excludes "gardenio" is denied.
func TestResolutionWrites_DenyCrossProjectOIDC(t *testing.T) {
	for _, tc := range resolutionCases() {
		t.Run(tc.name, func(t *testing.T) {
			ms, h := resolutionAuthzFixture(t, "gardenio")
			makeUserBead(ms, "gd-user-mallory", "mallory@example.com", "otherproject", "operator")

			rec := postJSON(h, tc.path, tc.body, func(r *http.Request) {
				ctx := withAuthMethod(r.Context(), authMethodOIDC)
				ctx = WithRoles(ctx, []string{RoleOperator})
				ctx = WithAuthIdentity(ctx, AuthIdentity{Type: "oidc", Subject: "mallory@example.com"})
				*r = *r.WithContext(ctx)
			})

			if rec.Code != http.StatusNotFound {
				t.Fatalf("%s: expected 404 (as missing) for cross-project OIDC, got %d; body: %s",
					tc.name, rec.Code, rec.Body.String())
			}
			assertResolutionTargetsUntouched(t, ms)
		})
	}
}

// TestResolutionWrites_AllowSameProjectFleetToken proves the gate does not
// over-block: a fleet token scoped to the bead's own project resolves each
// verb successfully (200) — except approve, which taints ADR §6.3 correction
// 2 additionally requires an OIDC-derived identity for (gd-731gBeGgl7): a
// same-project fleet token clears the project-scope gate here but is still
// refused for lack of OIDC identity, since it is not the project scope but
// the identity requirement that blocks it.
func TestResolutionWrites_AllowSameProjectFleetToken(t *testing.T) {
	for _, tc := range resolutionCases() {
		t.Run(tc.name, func(t *testing.T) {
			_, h := resolutionAuthzFixture(t, "gardenio")

			rec := postJSON(h, tc.path, tc.body, func(r *http.Request) {
				ctx := WithTokenProject(r.Context(), "gardenio")
				if tc.name == "result" {
					// P7 Task 10: a result needs a resolver identity; a
					// bridge may post one for any operation.
					ctx = WithFleet(WithAuthIdentity(ctx, AuthIdentity{Type: "fleet", Subject: "slack-bridge"}), "slack-bridge")
				}
				*r = *r.WithContext(ctx)
			})

			wantCode := http.StatusOK
			if tc.name == "approve" {
				wantCode = http.StatusForbidden
			}
			if rec.Code != wantCode {
				t.Fatalf("%s: expected %d for same-project fleet token, got %d; body: %s",
					tc.name, wantCode, rec.Code, rec.Body.String())
			}
		})
	}
}

// assertResolutionTargetsUntouched checks a denied resolution left no side
// effects: both target beads remain open, and no replay-created bead slipped
// through the approve handler's replayEscalation static-token path.
func assertResolutionTargetsUntouched(t *testing.T, ms *mockStore) {
	t.Helper()
	if esc := ms.beads["gd-esc-target"]; esc != nil && esc.Status != model.StatusOpen {
		t.Errorf("denied resolution mutated escalation status to %q", esc.Status)
	}
	if dec := ms.beads["gd-dec-target"]; dec != nil && dec.Status != model.StatusOpen {
		t.Errorf("denied resolution mutated decision status to %q", dec.Status)
	}
	for id, b := range ms.beads {
		if id != "gd-esc-target" && id != "gd-dec-target" && b.Type == model.TypeTask {
			t.Errorf("denied approve replayed a privileged create: bead %s (%s)", id, b.Title)
		}
	}
}
