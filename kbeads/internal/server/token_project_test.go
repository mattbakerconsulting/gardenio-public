package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/model"
	"google.golang.org/grpc/codes"
)

func TestRegisterFleetToken_WithProject(t *testing.T) {
	fc := NewFleetConfig("gardenio", nil)
	srv, _ := newTestServerWithFleetAuth(t, "admin-secret", fc)

	body, _ := json.Marshal(registerBridgeTokenRequest{
		Token:   "proj-tok-abc",
		Fleet:   "proj-fleet",
		Project: "myproject",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/fleet-tokens", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin-secret")
	req = asTokenMinter(req)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.handleRegisterBridgeToken(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d; body: %s", rec.Code, rec.Body.String())
	}

	// Verify fleet resolution.
	if got := fc.ResolveFleet("proj-tok-abc"); got != "proj-fleet" {
		t.Fatalf("expected fleet 'proj-fleet', got %q", got)
	}

	// Verify project resolution.
	if got := fc.ResolveProject("proj-tok-abc"); got != "myproject" {
		t.Fatalf("expected project 'myproject', got %q", got)
	}

	// Verify response includes project.
	var resp map[string]string
	_ = json.NewDecoder(rec.Body).Decode(&resp)
	if resp["project"] != "myproject" {
		t.Fatalf("expected project in response, got %v", resp)
	}

	// Verify config persisted with project.
	cfg, err := srv.store.GetConfig(req.Context(), "bridge_token:proj-fleet")
	if err != nil {
		t.Fatalf("expected config persisted, got error: %v", err)
	}
	var stored map[string]string
	_ = json.Unmarshal(cfg.Value, &stored)
	if stored["project"] != "myproject" {
		t.Fatalf("stored project = %q, want 'myproject'", stored["project"])
	}
}

func TestRegisterFleetToken_WithoutProject(t *testing.T) {
	fc := NewFleetConfig("gardenio", nil)
	srv, _ := newTestServerWithFleetAuth(t, "admin-secret", fc)

	body, _ := json.Marshal(registerBridgeTokenRequest{
		Token: "noproj-tok",
		Fleet: "noproj-fleet",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/fleet-tokens", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin-secret")
	req = asTokenMinter(req)
	rec := httptest.NewRecorder()

	srv.handleRegisterBridgeToken(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d", rec.Code)
	}

	// Project should be empty for tokens without project scope.
	if got := fc.ResolveProject("noproj-tok"); got != "" {
		t.Fatalf("expected empty project, got %q", got)
	}
}

func TestTokenProjectScope_GetBead_Allowed(t *testing.T) {
	srv, ms, _ := newTestServer()

	// Create a bead with project label.
	bead := &model.Bead{ID: "bead-1", Title: "Test", Type: "task", Kind: "issue", Status: model.StatusOpen}
	_ = ms.CreateBead(context.Background(), bead)
	_ = ms.AddLabel(context.Background(), "bead-1", "project:myproject")

	req := httptest.NewRequest(http.MethodGet, "/v1/beads/bead-1", nil)
	req.SetPathValue("id", "bead-1")
	// Set project scope in context.
	ctx := WithTokenProject(req.Context(), "myproject")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	srv.handleGetBead(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestTokenProjectScope_GetBead_Denied(t *testing.T) {
	srv, ms, _ := newTestServer()

	// Create a bead with a different project label.
	bead := &model.Bead{ID: "bead-2", Title: "Test", Type: "task", Kind: "issue", Status: model.StatusOpen}
	_ = ms.CreateBead(context.Background(), bead)
	_ = ms.AddLabel(context.Background(), "bead-2", "project:otherproject")

	req := httptest.NewRequest(http.MethodGet, "/v1/beads/bead-2", nil)
	req.SetPathValue("id", "bead-2")
	ctx := WithTokenProject(req.Context(), "myproject")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	srv.handleGetBead(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 (as missing), got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestTokenProjectScope_GetBead_NoScope(t *testing.T) {
	srv, ms, _ := newTestServer()

	// Create a bead with any project label.
	bead := &model.Bead{ID: "bead-3", Title: "Test", Type: "task", Kind: "issue", Status: model.StatusOpen}
	_ = ms.CreateBead(context.Background(), bead)
	_ = ms.AddLabel(context.Background(), "bead-3", "project:anyproject")

	// No project scope in context — should be unrestricted.
	req := httptest.NewRequest(http.MethodGet, "/v1/beads/bead-3", nil)
	req.SetPathValue("id", "bead-3")
	rec := httptest.NewRecorder()

	srv.handleGetBead(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestTokenProjectScope_ListBeads_FiltersProject(t *testing.T) {
	srv, ms, _ := newTestServer()

	// Create beads in two different projects.
	b1 := &model.Bead{ID: "lb-1", Title: "Mine", Type: "task", Kind: "issue", Status: model.StatusOpen}
	b2 := &model.Bead{ID: "lb-2", Title: "Theirs", Type: "task", Kind: "issue", Status: model.StatusOpen}
	_ = ms.CreateBead(context.Background(), b1)
	_ = ms.CreateBead(context.Background(), b2)
	_ = ms.AddLabel(context.Background(), "lb-1", "project:myproject")
	_ = ms.AddLabel(context.Background(), "lb-2", "project:otherproject")

	req := httptest.NewRequest(http.MethodGet, "/v1/beads", nil)
	ctx := WithTokenProject(req.Context(), "myproject")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	srv.handleListBeads(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var resp struct {
		Beads []*model.Bead `json:"beads"`
		Total int           `json:"total"`
	}
	_ = json.NewDecoder(rec.Body).Decode(&resp)

	if len(resp.Beads) != 1 {
		t.Fatalf("expected 1 bead, got %d", len(resp.Beads))
	}
	if resp.Beads[0].ID != "lb-1" {
		t.Fatalf("expected bead lb-1, got %s", resp.Beads[0].ID)
	}
}

func TestTokenProjectScope_CreateBead_Denied(t *testing.T) {
	srv, _, _ := newTestServer()

	body, _ := json.Marshal(createBeadInput{
		Title:  "Wrong project bead",
		Type:   "task",
		Labels: []string{"project:otherproject"},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/beads", bytes.NewReader(body))
	ctx := WithTokenProject(req.Context(), "myproject")
	ctx = withAuthMethod(ctx, authMethodStatic) // fleet tokens use static auth
	ctx = WithAuthIdentity(ctx, AuthIdentity{Type: "fleet", Subject: "test-bridge"})
	ctx = WithOperatedByHeader(ctx, "slack:U0OPERATOR2")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	srv.handleCreateBead(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestTokenProjectScope_CreateBead_Allowed(t *testing.T) {
	srv, _, _ := newTestServer()

	body, _ := json.Marshal(createBeadInput{
		Title:  "Right project bead",
		Type:   "task",
		Labels: []string{"project:myproject"},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/beads", bytes.NewReader(body))
	ctx := WithTokenProject(req.Context(), "myproject")
	ctx = withAuthMethod(ctx, authMethodStatic)
	ctx = WithAuthIdentity(ctx, AuthIdentity{Type: "fleet", Subject: "test-bridge"})
	ctx = WithOperatedByHeader(ctx, "slack:U0OPERATOR2")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	srv.handleCreateBead(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestTokenProjectScope_UpdateBead_Denied(t *testing.T) {
	srv, ms, _ := newTestServer()

	bead := &model.Bead{ID: "upd-1", Title: "Test", Type: "task", Kind: "issue", Status: model.StatusOpen}
	_ = ms.CreateBead(context.Background(), bead)
	_ = ms.AddLabel(context.Background(), "upd-1", "project:otherproject")

	title := "Updated Title"
	body, _ := json.Marshal(updateBeadInput{Title: &title})
	req := httptest.NewRequest(http.MethodPatch, "/v1/beads/upd-1", bytes.NewReader(body))
	req.SetPathValue("id", "upd-1")
	ctx := WithTokenProject(req.Context(), "myproject")
	ctx = withAuthMethod(ctx, authMethodStatic)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	srv.handleUpdateBead(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 (as missing), got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestTokenProjectScope_CloseBead_Denied(t *testing.T) {
	srv, ms, _ := newTestServer()

	bead := &model.Bead{ID: "cls-1", Title: "Test", Type: "task", Kind: "issue", Status: model.StatusOpen}
	_ = ms.CreateBead(context.Background(), bead)
	_ = ms.AddLabel(context.Background(), "cls-1", "project:otherproject")

	req := httptest.NewRequest(http.MethodPost, "/v1/beads/cls-1/close", bytes.NewReader([]byte("{}")))
	req.SetPathValue("id", "cls-1")
	ctx := WithTokenProject(req.Context(), "myproject")
	ctx = withAuthMethod(ctx, authMethodStatic)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	srv.handleCloseBead(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 (as missing), got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestTokenProjectScope_DeleteBead_Denied(t *testing.T) {
	srv, ms, _ := newTestServer()

	bead := &model.Bead{ID: "del-1", Title: "Test", Type: "task", Kind: "issue", Status: model.StatusOpen}
	_ = ms.CreateBead(context.Background(), bead)
	_ = ms.AddLabel(context.Background(), "del-1", "project:otherproject")

	req := httptest.NewRequest(http.MethodDelete, "/v1/beads/del-1", nil)
	req.SetPathValue("id", "del-1")
	ctx := WithTokenProject(req.Context(), "myproject")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	srv.handleDeleteBead(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 (as missing), got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestFleetConfig_ResolveProject(t *testing.T) {
	fc := NewFleetConfig("gardenio", nil)
	fc.RegisterTokenWithProject("tok-proj", "fleet-a", "myproject")
	fc.RegisterToken("tok-noproj", "fleet-b")

	if got := fc.ResolveProject("tok-proj"); got != "myproject" {
		t.Fatalf("expected 'myproject', got %q", got)
	}
	if got := fc.ResolveProject("tok-noproj"); got != "" {
		t.Fatalf("expected empty project, got %q", got)
	}
	if got := fc.ResolveProject("nonexistent"); got != "" {
		t.Fatalf("expected empty project for unknown token, got %q", got)
	}
}

func TestTokenProjectContext(t *testing.T) {
	ctx := context.Background()

	// No project set.
	if got := TokenProjectFromContext(ctx); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}

	// With project set.
	ctx = WithTokenProject(ctx, "testproj")
	if got := TokenProjectFromContext(ctx); got != "testproj" {
		t.Fatalf("expected 'testproj', got %q", got)
	}
}

func TestBeadMatchesTokenProject(t *testing.T) {
	t.Run("NoScope", func(t *testing.T) {
		ctx := context.Background()
		if !beadMatchesTokenProject(ctx, []string{"project:anything"}) {
			t.Fatal("expected match when no project scope")
		}
	})

	t.Run("Match", func(t *testing.T) {
		ctx := WithTokenProject(context.Background(), "myproject")
		if !beadMatchesTokenProject(ctx, []string{"project:myproject", "other:label"}) {
			t.Fatal("expected match")
		}
	})

	t.Run("NoMatch", func(t *testing.T) {
		ctx := WithTokenProject(context.Background(), "myproject")
		if beadMatchesTokenProject(ctx, []string{"project:otherproject"}) {
			t.Fatal("expected no match")
		}
	})

	t.Run("NoLabels", func(t *testing.T) {
		ctx := WithTokenProject(context.Background(), "myproject")
		if beadMatchesTokenProject(ctx, nil) {
			t.Fatal("expected no match for bead with no labels")
		}
	})
}

func TestAuthMiddleware_SetsTokenProject(t *testing.T) {
	fc := NewFleetConfig("gardenio", nil)
	fc.RegisterTokenWithProject("proj-tok-123", "agent-fleet", "myproject")

	var capturedProject string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedProject = TokenProjectFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	mw := AuthMiddleware(withTestToken(fc, "admin-secret"), nil, handler)

	req := httptest.NewRequest(http.MethodGet, "/v1/beads", nil)
	req.Header.Set("Authorization", "Bearer proj-tok-123")
	rec := httptest.NewRecorder()

	mw.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if capturedProject != "myproject" {
		t.Fatalf("expected project 'myproject' in context, got %q", capturedProject)
	}
}

func TestAuthMiddleware_NoProjectForUnscopedToken(t *testing.T) {
	fc := NewFleetConfig("gardenio", nil)
	fc.RegisterToken("unscoped-tok", "agent-fleet")

	var capturedProject string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedProject = TokenProjectFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	mw := AuthMiddleware(withTestToken(fc, "admin-secret"), nil, handler)

	req := httptest.NewRequest(http.MethodGet, "/v1/beads", nil)
	req.Header.Set("Authorization", "Bearer unscoped-tok")
	rec := httptest.NewRecorder()

	mw.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if capturedProject != "" {
		t.Fatalf("expected empty project for unscoped token, got %q", capturedProject)
	}
}

// §C2 (ka-0000006767): gRPC CloseBead must enforce token project scope.
// Previously skipped — caller could close beads in other projects.
func TestTokenProjectScope_GRPCCloseBead_Denied(t *testing.T) {
	srv, ms, _ := newTestServer()
	ms.beads["gc-cls-1"] = &model.Bead{
		ID: "gc-cls-1", Title: "Test", Type: model.TypeTask,
		Kind: model.KindIssue, Status: model.StatusOpen,
		Labels: []string{"project:otherproject"},
	}

	ctx := WithTokenProject(context.Background(), "myproject")
	ctx = withAuthMethod(ctx, authMethodStatic)

	_, err := srv.CloseBead(ctx, &beadsv1.CloseBeadRequest{Id: "gc-cls-1"})
	requireCode(t, err, codes.NotFound)
}

// §C2: gRPC DeleteBead must enforce token project scope.
func TestTokenProjectScope_GRPCDeleteBead_Denied(t *testing.T) {
	srv, ms, _ := newTestServer()
	ms.beads["gc-del-1"] = &model.Bead{
		ID: "gc-del-1", Title: "Test", Type: model.TypeTask,
		Kind: model.KindIssue, Status: model.StatusOpen,
		Labels: []string{"project:otherproject"},
	}

	ctx := WithTokenProject(context.Background(), "myproject")
	ctx = withAuthMethod(ctx, authMethodStatic)

	_, err := srv.DeleteBead(ctx, &beadsv1.DeleteBeadRequest{Id: "gc-del-1"})
	requireCode(t, err, codes.NotFound)
}

// §C2: gRPC CloseBead must enforce fleet-token op_scope.
// A static token scoped to only "create" must not be able to close beads.
func TestTokenOpScope_GRPCCloseBead_Denied(t *testing.T) {
	srv, ms, _ := newTestServer()
	ms.beads["gc-ops-cls"] = &model.Bead{
		ID: "gc-ops-cls", Title: "Test", Type: model.TypeTask,
		Kind: model.KindIssue, Status: model.StatusOpen,
	}

	ctx := withAuthMethod(context.Background(), authMethodStatic)
	ctx = WithTokenOpScope(ctx, []string{"create"})

	_, err := srv.CloseBead(ctx, &beadsv1.CloseBeadRequest{Id: "gc-ops-cls"})
	requireCode(t, err, codes.PermissionDenied)
}

// §C2: gRPC DeleteBead must enforce fleet-token op_scope.
func TestTokenOpScope_GRPCDeleteBead_Denied(t *testing.T) {
	srv, ms, _ := newTestServer()
	ms.beads["gc-ops-del"] = &model.Bead{
		ID: "gc-ops-del", Title: "Test", Type: model.TypeTask,
		Kind: model.KindIssue, Status: model.StatusOpen,
	}

	ctx := withAuthMethod(context.Background(), authMethodStatic)
	ctx = WithTokenOpScope(ctx, []string{"create", "update"})

	_, err := srv.DeleteBead(ctx, &beadsv1.DeleteBeadRequest{Id: "gc-ops-del"})
	requireCode(t, err, codes.PermissionDenied)
}

// §C2: HTTP handleDeleteBead must enforce fleet-token op_scope.
// Previously the handler only checked TokenProjectFromContext — a token with
// a project scope but op_scope=["create"] could DELETE, which is wrong.
func TestTokenOpScope_HTTPDeleteBead_Denied(t *testing.T) {
	srv, ms, _ := newTestServer()
	bead := &model.Bead{
		ID: "http-del-op", Title: "Test", Type: model.TypeTask,
		Kind: model.KindIssue, Status: model.StatusOpen,
	}
	_ = ms.CreateBead(context.Background(), bead)

	req := httptest.NewRequest(http.MethodDelete, "/v1/beads/http-del-op", nil)
	req.SetPathValue("id", "http-del-op")
	ctx := withAuthMethod(req.Context(), authMethodStatic)
	ctx = WithTokenOpScope(ctx, []string{"create"})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	srv.handleDeleteBead(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d; body: %s", rec.Code, rec.Body.String())
	}
}
