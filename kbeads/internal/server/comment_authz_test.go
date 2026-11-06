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

// Comment-authz decoupling regression tests (ka-0000005884).
//
// Comments are append-only annotations, not mutations of the bead's own
// fields, so comment-create must NOT be gated by the bead-mutation ownership
// rule. Any authenticated identity authorized to write to the bead's
// type/project may comment regardless of created_by/assignee/owner. Ownership
// gating MUST remain in force for real field mutations (status/assignee/owner/
// close) and for comment delete/trim.

// commentAuthzMux wires the comment, close, and update routes without
// AuthMiddleware so tests can inject OIDC identity directly into the request
// context (mirrors bareMux / ownershipBareMux).
func commentAuthzMux(s *BeadsServer) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/beads/{id}/comments", s.handleAddComment)
	mux.HandleFunc("POST /v1/beads/{id}/close", s.handleCloseBead)
	mux.HandleFunc("PATCH /v1/beads/{id}", s.handleUpdateBead)
	return mux
}

// taskOwnedBy returns a minimal issue-kind task bead created by `owner`. The
// "task" type carries no write_policy, so write-policy authz is a no-op and the
// ownership gate is the only thing that could deny a non-owner — exactly the
// behavior under test.
func taskOwnedBy(id, owner string) *model.Bead {
	return &model.Bead{
		ID:        id,
		Title:     "someone else's task",
		Type:      "task",
		Kind:      model.KindIssue,
		Status:    model.StatusOpen,
		CreatedBy: owner,
	}
}

// A non-owner authenticated OIDC operator CAN comment on a bead they did not
// create — the repro from ka-0000005884. Before the fix this returned 403
// "ownership denied".
func TestAddComment_NonOwnerAllowed(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	h := commentAuthzMux(s)
	ms.beads["gd-cmt1"] = taskOwnedBy("gd-cmt1", "alice@example.com")

	req := oidcCtxReq("POST", "/v1/beads/gd-cmt1/comments",
		map[string]any{"text": "drive-by note from a non-owner"},
		"mallory@example.com", []string{RoleOperator})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("non-owner should be allowed to comment, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if len(ms.comments["gd-cmt1"]) != 1 {
		t.Fatalf("expected 1 comment persisted, got %d", len(ms.comments["gd-cmt1"]))
	}
	// Commenter identity is stamped for attribution.
	c := ms.comments["gd-cmt1"][0]
	if c.OperatedBy != "oidc:mallory@example.com" {
		t.Errorf("comment operated_by should attribute the commenter, got %q", c.OperatedBy)
	}
	if c.OperatedByID == nil || *c.OperatedByID == 0 {
		t.Errorf("comment operated_by_id should be a resolved non-zero operator FK, got %v", c.OperatedByID)
	}
	// The comment-added event must have fired.
	found := false
	for _, e := range ms.events {
		if e.Topic == events.TopicCommentAdded {
			found = true
		}
	}
	if !found {
		t.Error("expected comment-added event on allowed comment")
	}
}

// The edge bare-email landmine (ka-0000005884 / ka-0000001692): a GitLab-PAT
// edge caller arrives as Type "edge" with a BARE email Subject (no oidc:
// prefix). That path must also be allowed to comment on a bead it did not
// create, and the attribution must resolve through the resolver.
func TestAddComment_NonOwnerEdgeBareEmailAllowed(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	h := commentAuthzMux(s)
	ms.beads["gd-cmt2"] = taskOwnedBy("gd-cmt2", "agent-bot")

	body, _ := json.Marshal(map[string]any{"text": "comment via GitLab-PAT edge"})
	req := httptest.NewRequest("POST", "/v1/beads/gd-cmt2/comments", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// EdgeAuthMiddleware stamps a BARE-email Subject on the AuthIdentity (no
	// oidc: prefix — the ka-0000005884 landmine) and separately records the
	// verified attribution as "oidc:<email>" via WithOperatedByHeader. We
	// reproduce both so the comment both passes authz AND attributes.
	ctx := withAuthMethod(req.Context(), authMethodEdge)
	ctx = WithRoles(ctx, []string{RoleOperator})
	ctx = WithAuthIdentity(ctx, AuthIdentity{Type: "edge", Subject: "matt@acme.example"})
	ctx = WithOperatedByHeader(ctx, "oidc:matt@acme.example")
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("edge bare-email non-owner should be allowed to comment, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if len(ms.comments["gd-cmt2"]) != 1 {
		t.Fatalf("expected 1 comment persisted, got %d", len(ms.comments["gd-cmt2"]))
	}
}

// Negative control: a non-owner still CANNOT close a bead they did not own.
// Closing is a real field mutation and remains ownership-gated.
func TestCloseBead_NonOwnerStillDenied(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	h := commentAuthzMux(s)
	ms.beads["gd-cmt3"] = taskOwnedBy("gd-cmt3", "alice@example.com")

	req := oidcCtxReq("POST", "/v1/beads/gd-cmt3/close",
		map[string]any{}, "mallory@example.com", []string{RoleOperator})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner close must remain denied, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

// Negative control: a non-owner still CANNOT mutate status/assignee/owner on a
// bead they did not own. Field mutations remain ownership-gated.
func TestUpdateBead_NonOwnerStillDenied(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	h := commentAuthzMux(s)
	ms.beads["gd-cmt4"] = taskOwnedBy("gd-cmt4", "alice@example.com")

	newAssignee := "mallory@example.com"
	req := oidcCtxReq("PATCH", "/v1/beads/gd-cmt4",
		map[string]any{"assignee": newAssignee},
		"mallory@example.com", []string{RoleOperator})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner field mutation must remain denied, got %d; body: %s", rec.Code, rec.Body.String())
	}
}
