package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// undoMux registers just the undo route on a fresh BeadsServer/handler so
// tests can drive it without going through AuthMiddleware. Identity is
// injected into the request context via WithRoles + WithAuthIdentity, the
// same path used by the relations-authz tests.
func undoMux(s *BeadsServer) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/mutations/{id}/undo", s.handleUndoMutation)
	return mux
}

func newUndoHTTPFixture(t *testing.T) (*BeadsServer, *mockStore, http.Handler, int64) {
	t.Helper()
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	ms.beads["gd-h-1"] = &model.Bead{
		ID:     "gd-h-1",
		Title:  "Newer",
		Type:   "task",
		Kind:   model.KindIssue,
		Status: model.StatusOpen,
	}
	mut := &model.MutationRow{
		BeadID:       "gd-h-1",
		Field:        "title",
		OldValue:     "Older",
		NewValue:     "Newer",
		MutationType: model.MutationTypeUpdate,
		CreatedAt:    time.Now(),
	}
	if err := ms.RecordMutation(context.Background(), mut); err != nil {
		t.Fatalf("seeding mutation: %v", err)
	}
	muts, _ := ms.ListMutations(context.Background(), "gd-h-1")
	return s, ms, undoMux(s), muts[0].ID
}

func TestHandleUndo_HappyPath_Admin(t *testing.T) {
	_, ms, h, mutID := newUndoHTTPFixture(t)

	req := oidcCtxReq("POST", "/v1/mutations/"+strconv.FormatInt(mutID, 10)+"/undo", nil,
		"admin@example.com", []string{RoleAdmin})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, body=%s", rec.Code, rec.Body.String())
	}
	if got := ms.beads["gd-h-1"].Title; got != "Older" {
		t.Errorf("title after undo=%q, want Older", got)
	}
	var resp struct {
		Bead   *model.Bead        `json:"bead"`
		Undone *model.MutationRow `json:"undone"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Undone == nil || resp.Undone.ID != mutID {
		t.Errorf("undone.id mismatch: %+v", resp.Undone)
	}
	if resp.Bead == nil || resp.Bead.Title != "Older" {
		t.Errorf("bead title=%q, want Older", resp.Bead.Title)
	}
}

func TestHandleUndo_NonAdminDeniedAndAuditWritten(t *testing.T) {
	_, ms, h, mutID := newUndoHTTPFixture(t)

	req := oidcCtxReq("POST", "/v1/mutations/"+strconv.FormatInt(mutID, 10)+"/undo", nil,
		"mallory@example.com", []string{RoleOperator})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d, body=%s", rec.Code, rec.Body.String())
	}
	// Bead's title must be unchanged.
	if ms.beads["gd-h-1"].Title != "Newer" {
		t.Errorf("title changed despite denial: %q", ms.beads["gd-h-1"].Title)
	}
	// One audit bead must have been written.
	found := false
	for _, b := range ms.beads {
		if b.Type != "audit" {
			continue
		}
		var fields map[string]string
		if err := json.Unmarshal(b.Fields, &fields); err != nil {
			continue
		}
		if fields["actor"] == "mallory@example.com" && fields["action"] == "undo" && fields["outcome"] == "deny" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected an audit bead recording the denied undo attempt")
	}
}

func TestHandleUndo_NotFound(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	h := undoMux(s)

	req := oidcCtxReq("POST", "/v1/mutations/9999/undo", nil,
		"admin@example.com", []string{RoleAdmin})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleUndo_Conflict_ReturnsConflict(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	ms.beads["gd-conflict"] = &model.Bead{
		ID: "gd-conflict", Title: "Drift", Type: "task", Kind: model.KindIssue, Status: model.StatusOpen,
	}
	mut := &model.MutationRow{
		BeadID:       "gd-conflict",
		Field:        "title",
		OldValue:     "Old",
		NewValue:     "Newer",
		MutationType: model.MutationTypeUpdate,
		CreatedAt:    time.Now(),
	}
	_ = ms.RecordMutation(context.Background(), mut)
	muts, _ := ms.ListMutations(context.Background(), "gd-conflict")

	req := oidcCtxReq("POST", "/v1/mutations/"+strconv.FormatInt(muts[0].ID, 10)+"/undo", nil,
		"admin@example.com", []string{RoleAdmin})
	rec := httptest.NewRecorder()
	undoMux(s).ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d, body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleUndo_BadID(t *testing.T) {
	_, _, h, _ := newUndoHTTPFixture(t)

	req := oidcCtxReq("POST", "/v1/mutations/not-a-number/undo", nil,
		"admin@example.com", []string{RoleAdmin})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleUndo_StaticTokenBypassesRoleCheck(t *testing.T) {
	_, ms, h, mutID := newUndoHTTPFixture(t)

	req := httptest.NewRequest("POST", "/v1/mutations/"+strconv.FormatInt(mutID, 10)+"/undo", nil)
	ctx := withAuthMethod(req.Context(), authMethodStatic)
	ctx = WithAuthIdentity(ctx, AuthIdentity{Type: "static", Subject: "primary-static"})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, body=%s", rec.Code, rec.Body.String())
	}
	if got := ms.beads["gd-h-1"].Title; got != "Older" {
		t.Errorf("title after undo=%q, want Older", got)
	}
}
