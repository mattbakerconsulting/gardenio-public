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

// Regression tests for AUDIT-2026-04-18 §C4: label/dependency/comment
// handlers must call authorizeWriteForBead so an OIDC user with operator
// role on project X cannot mutate beads labeled project:Y.

// oidcCtxReq builds an HTTP request carrying OIDC auth context for the given
// user + roles. The handlers under test must read this context (via
// authMethodFromContext + RolesFromContext + AuthIdentityFromContext) to
// evaluate the project-scoped write policy.
func oidcCtxReq(method, path string, body any, email string, roles []string) *http.Request {
	var req *http.Request
	if body != nil {
		b, _ := json.Marshal(body)
		req = httptest.NewRequest(method, path, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	ctx := withAuthMethod(req.Context(), authMethodOIDC)
	ctx = WithRoles(ctx, roles)
	ctx = WithAuthIdentity(ctx, AuthIdentity{Type: "oidc", Subject: email})
	return req.WithContext(ctx)
}

// mutationTopics are the event topics a denied relation handler must NOT emit.
// The audit-denial path does create a bead-created event for the audit bead,
// so we can't assert len(events)==0; we assert that no mutation-specific
// event slipped through the authz gate.
var mutationTopics = map[string]bool{
	"beads.label.added":        true,
	"beads.label.removed":      true,
	"beads.comment.added":      true,
	"beads.dependency.added":   true,
	"beads.dependency.removed": true,
}

func assertNoMutationEvent(t *testing.T, ms *mockStore) {
	t.Helper()
	for _, e := range ms.events {
		if mutationTopics[e.Topic] {
			t.Errorf("denied mutation must not record %q event", e.Topic)
		}
	}
}

// bareMux builds an http.Handler that registers the relation-write routes on
// the bead server but skips AuthMiddleware, so tests can inject OIDC identity
// directly into the request context. This isolates the authz logic under test
// (authorizeWriteForBead in the relation handlers) from the separately-tested
// middleware pipeline.
func bareMux(s *BeadsServer) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/beads/{id}/labels", s.handleAddLabel)
	mux.HandleFunc("DELETE /v1/beads/{id}/labels/{label}", s.handleRemoveLabel)
	mux.HandleFunc("POST /v1/beads/{id}/comments", s.handleAddComment)
	mux.HandleFunc("POST /v1/beads/{id}/dependencies", s.handleAddDependency)
	mux.HandleFunc("DELETE /v1/beads/{id}/dependencies", s.handleRemoveDependency)
	return mux
}

// relAuthzFixture wires up a BeadsServer + HTTP handler with a single
// project-scoped (agent-type) target bead and a user bead that grants
// spawn permission to a specific project. Use wrong-project email to
// simulate cross-project access attempts.
func relAuthzFixture(t *testing.T, targetProject, userEmail, userProjects string) (*mockStore, http.Handler, string) {
	t.Helper()
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	h := bareMux(s)

	targetID := "gd-target1"
	ms.beads[targetID] = &model.Bead{
		ID:     targetID,
		Title:  "target",
		Type:   "agent",
		Kind:   "config",
		Status: model.StatusOpen,
	}
	// mockStore.GetBead attaches labels from ms.labels[id] (not bead.Labels).
	ms.labels[targetID] = []string{"project:" + targetProject}
	makeUserBead(ms, "gd-user-rel", userEmail, userProjects, "crew")
	return ms, h, targetID
}

func TestAddLabel_DeniesCrossProjectOIDC(t *testing.T) {
	ms, h, id := relAuthzFixture(t, "gardenio", "mallory@example.com", "otherproject")

	req := oidcCtxReq("POST", "/v1/beads/"+id+"/labels",
		map[string]any{"label": "project:otherproject"},
		"mallory@example.com", []string{RoleOperator})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 (as missing), got %d; body: %s", rec.Code, rec.Body.String())
	}
	// AddLabel must not have been invoked on the store.
	assertNoMutationEvent(t, ms)
}

func TestRemoveLabel_DeniesCrossProjectOIDC(t *testing.T) {
	ms, h, id := relAuthzFixture(t, "gardenio", "mallory@example.com", "otherproject")

	req := oidcCtxReq("DELETE", "/v1/beads/"+id+"/labels/project:gardenio", nil,
		"mallory@example.com", []string{RoleOperator})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 (as missing), got %d; body: %s", rec.Code, rec.Body.String())
	}
	assertNoMutationEvent(t, ms)
}

func TestAddComment_DeniesCrossProjectOIDC(t *testing.T) {
	ms, h, id := relAuthzFixture(t, "gardenio", "mallory@example.com", "otherproject")

	req := oidcCtxReq("POST", "/v1/beads/"+id+"/comments",
		map[string]any{"author": "mallory@example.com", "text": "hijack"},
		"mallory@example.com", []string{RoleOperator})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 (as missing), got %d; body: %s", rec.Code, rec.Body.String())
	}
	assertNoMutationEvent(t, ms)
}

func TestAddDependency_DeniesCrossProjectOIDC(t *testing.T) {
	ms, h, id := relAuthzFixture(t, "gardenio", "mallory@example.com", "otherproject")

	req := oidcCtxReq("POST", "/v1/beads/"+id+"/dependencies",
		map[string]any{"depends_on_id": "gd-anything", "type": "blocks", "created_by": "mallory@example.com"},
		"mallory@example.com", []string{RoleOperator})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 (as missing), got %d; body: %s", rec.Code, rec.Body.String())
	}
	assertNoMutationEvent(t, ms)
}

func TestRemoveDependency_DeniesCrossProjectOIDC(t *testing.T) {
	ms, h, id := relAuthzFixture(t, "gardenio", "mallory@example.com", "otherproject")

	req := oidcCtxReq("DELETE",
		"/v1/beads/"+id+"/dependencies?depends_on_id=gd-anything&type=blocks", nil,
		"mallory@example.com", []string{RoleOperator})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 (as missing), got %d; body: %s", rec.Code, rec.Body.String())
	}
	assertNoMutationEvent(t, ms)
}

// Positive control: an authorized OIDC user on the same project can still
// perform relation mutations after the authz gate is added.
func TestAddLabel_AllowsSameProjectOIDC(t *testing.T) {
	ms, h, id := relAuthzFixture(t, "gardenio", "alice@example.com", "gardenio")

	req := oidcCtxReq("POST", "/v1/beads/"+id+"/labels",
		map[string]any{"label": "urgent"},
		"alice@example.com", []string{RoleOperator})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if len(ms.events) == 0 {
		t.Error("expected label-added event on authorized mutation")
	}
}

// Admin role should bypass project scope on relation mutations.
func TestAddLabel_AdminBypass(t *testing.T) {
	_, h, id := relAuthzFixture(t, "gardenio", "admin@example.com", "otherproject")

	req := oidcCtxReq("POST", "/v1/beads/"+id+"/labels",
		map[string]any{"label": "reviewed"},
		"admin@example.com", []string{RoleAdmin})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 for admin, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

// Sanity: a missing bead returns 404, not 403. The authz path must not leak
// existence of beads outside the caller's project, but the existing handlers
// have never distinguished those cases — we preserve the pre-existing 404.
func TestAddLabel_NotFound(t *testing.T) {
	_, h, _ := relAuthzFixture(t, "gardenio", "alice@example.com", "gardenio")

	req := oidcCtxReq("POST", "/v1/beads/gd-missing/labels",
		map[string]any{"label": "urgent"},
		"alice@example.com", []string{RoleOperator})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for missing bead, got %d; body: %s", rec.Code, rec.Body.String())
	}
}
