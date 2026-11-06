package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

func TestUserProvisionerMiddleware_NonOIDC(t *testing.T) {
	ms := newMockStore()
	up := NewUserProvisioner(ms, &events.NoopPublisher{})

	called := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		// No user bead ID should be set.
		if id := UserBeadIDFromContext(r.Context()); id != "" {
			t.Errorf("expected empty user bead ID for non-OIDC, got %q", id)
		}
	})

	handler := up.Middleware(inner)

	// Static-token auth — not OIDC, should pass through.
	req := httptest.NewRequest("GET", "/v1/beads", nil)
	ctx := withAuthMethod(req.Context(), authMethodStatic)
	ctx = WithAuthIdentity(ctx, AuthIdentity{Type: "static", Subject: "static"})
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !called {
		t.Fatal("inner handler was not called")
	}
	if rec.Header().Get("X-User-Bead-ID") != "" {
		t.Error("X-User-Bead-ID should not be set for non-OIDC requests")
	}
}

func TestUserProvisionerMiddleware_NoEmail(t *testing.T) {
	ms := newMockStore()
	up := NewUserProvisioner(ms, &events.NoopPublisher{})

	called := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	})

	handler := up.Middleware(inner)

	// OIDC auth but empty subject (no email).
	req := httptest.NewRequest("GET", "/v1/beads", nil)
	ctx := withAuthMethod(req.Context(), authMethodOIDC)
	ctx = WithAuthIdentity(ctx, AuthIdentity{Type: "oidc", Subject: ""})
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !called {
		t.Fatal("inner handler was not called")
	}
	if rec.Header().Get("X-User-Bead-ID") != "" {
		t.Error("X-User-Bead-ID should not be set when email is empty")
	}
}

func TestUserProvisionerMiddleware_CreatesUserBead(t *testing.T) {
	ms := newMockStore()
	pub := &events.NoopPublisher{}
	up := NewUserProvisioner(ms, pub)

	var gotBeadID string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBeadID = UserBeadIDFromContext(r.Context())
	})

	handler := up.Middleware(inner)

	req := httptest.NewRequest("GET", "/v1/beads", nil)
	ctx := withAuthMethod(req.Context(), authMethodOIDC)
	ctx = WithAuthIdentity(ctx, AuthIdentity{Type: "oidc", Subject: "alice@example.com"})
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// Should have created exactly one user bead.
	if gotBeadID == "" {
		t.Fatal("expected user bead ID in context")
	}
	if rec.Header().Get("X-User-Bead-ID") != gotBeadID {
		t.Errorf("header mismatch: got %q, want %q", rec.Header().Get("X-User-Bead-ID"), gotBeadID)
	}

	// Verify the bead in the store.
	bead := ms.beads[gotBeadID]
	if bead == nil {
		t.Fatal("bead not found in store")
	}
	if bead.Type != "user" {
		t.Errorf("type = %q, want %q", bead.Type, "user")
	}
	if bead.Kind != "config" {
		t.Errorf("kind = %q, want %q", bead.Kind, "config")
	}
	if bead.Title != "alice" {
		t.Errorf("title = %q, want %q", bead.Title, "alice")
	}
	if bead.CreatedBy != "auto-provision" {
		t.Errorf("created_by = %q, want %q", bead.CreatedBy, "auto-provision")
	}

	var fields map[string]string
	if err := json.Unmarshal(bead.Fields, &fields); err != nil {
		t.Fatalf("unmarshal fields: %v", err)
	}
	if fields["email"] != "alice@example.com" {
		t.Errorf("email field = %q, want %q", fields["email"], "alice@example.com")
	}

	// Regression (ka-0000007442): auto-provisioned user beads MUST carry a
	// non-nil operator FK so the NOT NULL constraint reinstated by migration
	// 022 on beads.operated_by_id is satisfied.
	if bead.OperatedByID == nil {
		t.Error("operated_by_id is nil; provisioner bypassed operator resolver")
	}
	if bead.CreatedByOperatorID == nil {
		t.Error("created_by_operator_id is nil; provisioner did not stamp originator FK")
	}
	if bead.OperatedByID != nil && bead.CreatedByOperatorID != nil &&
		*bead.OperatedByID != *bead.CreatedByOperatorID {
		t.Errorf("operated_by_id (%d) != created_by_operator_id (%d); first-create should match",
			*bead.OperatedByID, *bead.CreatedByOperatorID)
	}
	if bead.OperatedBy != "oidc:alice@example.com" {
		t.Errorf("operated_by = %q, want %q", bead.OperatedBy, "oidc:alice@example.com")
	}
	// The operator + alias should be reachable via the same store calls
	// that BeadsServer.resolveOperator uses on every subsequent write.
	aliasID, err := ms.LookupOperatorAlias(context.Background(), "oidc", "alice@example.com")
	if err != nil {
		t.Errorf("alias lookup after provision: %v", err)
	}
	if bead.OperatedByID != nil && aliasID != *bead.OperatedByID {
		t.Errorf("alias resolves to %d, bead stamped %d", aliasID, *bead.OperatedByID)
	}
}

func TestUserProvisionerMiddleware_FindsExistingBead(t *testing.T) {
	ms := newMockStore()
	up := NewUserProvisioner(ms, &events.NoopPublisher{})

	// Pre-create a user bead.
	fieldsJSON, _ := json.Marshal(map[string]string{"email": "bob@example.com"})
	ms.beads["gd-existing"] = &model.Bead{
		ID:     "gd-existing",
		Title:  "bob",
		Type:   "user",
		Kind:   "config",
		Status: model.StatusOpen,
		Fields: fieldsJSON,
	}

	var gotBeadID string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBeadID = UserBeadIDFromContext(r.Context())
	})

	handler := up.Middleware(inner)

	req := httptest.NewRequest("GET", "/v1/beads", nil)
	ctx := withAuthMethod(req.Context(), authMethodOIDC)
	ctx = WithAuthIdentity(ctx, AuthIdentity{Type: "oidc", Subject: "bob@example.com"})
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if gotBeadID != "gd-existing" {
		t.Errorf("expected existing bead ID %q, got %q", "gd-existing", gotBeadID)
	}
	if rec.Header().Get("X-User-Bead-ID") != "gd-existing" {
		t.Errorf("header = %q, want %q", rec.Header().Get("X-User-Bead-ID"), "gd-existing")
	}

	// Should NOT have created a new bead — only the pre-existing one.
	userBeads := 0
	for _, b := range ms.beads {
		if b.Type == "user" {
			userBeads++
		}
	}
	if userBeads != 1 {
		t.Errorf("expected 1 user bead, found %d", userBeads)
	}
}

func TestUserProvisionerMiddleware_CachesLookup(t *testing.T) {
	ms := newMockStore()
	up := NewUserProvisioner(ms, &events.NoopPublisher{})

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	handler := up.Middleware(inner)

	// First request — creates the bead.
	req1 := httptest.NewRequest("GET", "/v1/beads", nil)
	ctx := withAuthMethod(req1.Context(), authMethodOIDC)
	ctx = WithAuthIdentity(ctx, AuthIdentity{Type: "oidc", Subject: "cache@example.com"})
	req1 = req1.WithContext(ctx)
	handler.ServeHTTP(httptest.NewRecorder(), req1)

	beadCount := len(ms.beads)

	// Second request — should hit cache, no new bead.
	req2 := httptest.NewRequest("GET", "/v1/beads", nil)
	req2 = req2.WithContext(ctx)
	handler.ServeHTTP(httptest.NewRecorder(), req2)

	if len(ms.beads) != beadCount {
		t.Errorf("expected %d beads (cached), got %d", beadCount, len(ms.beads))
	}
}

func TestUserBeadIDContext(t *testing.T) {
	ctx := context.Background()
	if id := UserBeadIDFromContext(ctx); id != "" {
		t.Errorf("expected empty from bare context, got %q", id)
	}

	ctx = WithUserBeadID(ctx, "gd-test123")
	if id := UserBeadIDFromContext(ctx); id != "gd-test123" {
		t.Errorf("expected %q, got %q", "gd-test123", id)
	}
}
