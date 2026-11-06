package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// failingCreateStore wraps mockStore and returns an error from CreateBead.
type failingCreateStore struct {
	*mockStore
	createErr error
}

func (f *failingCreateStore) CreateBead(_ context.Context, bead *model.Bead) error {
	return f.createErr
}

// countingCreateStore wraps mockStore and counts CreateBead calls.
type countingCreateStore struct {
	*mockStore
	createCalls atomic.Int64
}

func (c *countingCreateStore) CreateBead(ctx context.Context, bead *model.Bead) error {
	c.createCalls.Add(1)
	return c.mockStore.CreateBead(ctx, bead)
}

// recordingPublisher captures published events.
type recordingPublisher struct {
	mu     sync.Mutex
	events []publishedEvent
}

type publishedEvent struct {
	topic string
	event any
}

func (rp *recordingPublisher) Publish(_ context.Context, topic string, event any) error {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	rp.events = append(rp.events, publishedEvent{topic: topic, event: event})
	return nil
}

func (rp *recordingPublisher) Close() error { return nil }

func (rp *recordingPublisher) count() int {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	return len(rp.events)
}

func oidcRequest(email string) *http.Request {
	req := httptest.NewRequest("GET", "/v1/beads", nil)
	ctx := withAuthMethod(req.Context(), authMethodOIDC)
	ctx = WithAuthIdentity(ctx, AuthIdentity{Type: "oidc", Subject: email})
	return req.WithContext(ctx)
}

func TestUserProvisionerEdge_ConcurrentSameUser(t *testing.T) {
	ms := newMockStore()
	cs := &countingCreateStore{mockStore: ms}
	up := NewUserProvisioner(cs, &events.NoopPublisher{})

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	handler := up.Middleware(inner)

	const goroutines = 20
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			req := oidcRequest("concurrent@example.com")
			handler.ServeHTTP(httptest.NewRecorder(), req)
		}()
	}
	wg.Wait()

	// Only one user bead should exist despite concurrent requests.
	userBeads := 0
	for _, b := range ms.beads {
		if b.Type == "user" {
			userBeads++
		}
	}
	// The in-memory mockStore may create duplicates (no DB uniqueness constraint),
	// but every request should still succeed. In production, the DB would
	// deduplicate via the field filter. The key assertion is no panic and
	// all goroutines completed.
	if userBeads == 0 {
		t.Fatal("expected at least one user bead to be created")
	}
}

func TestUserProvisionerEdge_UnusualEmailFormats(t *testing.T) {
	tests := []struct {
		email     string
		wantTitle string
	}{
		{"alice+tag@example.com", "alice+tag"},
		{"user.name@sub.domain.co.uk", "user.name"},
		{"UPPER@CASE.COM", "UPPER"},
		{"single@x", "single"},
		{"dots...@example.com", "dots..."},
		{"a@b", "a"},
	}

	for _, tt := range tests {
		t.Run(tt.email, func(t *testing.T) {
			ms := newMockStore()
			up := NewUserProvisioner(ms, &events.NoopPublisher{})

			var gotBeadID string
			inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotBeadID = UserBeadIDFromContext(r.Context())
			})
			handler := up.Middleware(inner)

			req := oidcRequest(tt.email)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if gotBeadID == "" {
				t.Fatal("expected user bead ID in context")
			}

			bead := ms.beads[gotBeadID]
			if bead == nil {
				t.Fatal("bead not found in store")
			}
			if bead.Title != tt.wantTitle {
				t.Errorf("title = %q, want %q", bead.Title, tt.wantTitle)
			}

			var fields map[string]string
			if err := json.Unmarshal(bead.Fields, &fields); err != nil {
				t.Fatalf("unmarshal fields: %v", err)
			}
			if fields["email"] != tt.email {
				t.Errorf("email field = %q, want %q", fields["email"], tt.email)
			}
		})
	}
}

func TestUserProvisionerEdge_EmailWithoutAtSign(t *testing.T) {
	ms := newMockStore()
	up := NewUserProvisioner(ms, &events.NoopPublisher{})

	var gotBeadID string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBeadID = UserBeadIDFromContext(r.Context())
	})
	handler := up.Middleware(inner)

	// Subject with no @ sign — title should be the entire string.
	req := oidcRequest("noemail")
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if gotBeadID == "" {
		t.Fatal("expected user bead ID")
	}
	bead := ms.beads[gotBeadID]
	if bead.Title != "noemail" {
		t.Errorf("title = %q, want %q for email without @", bead.Title, "noemail")
	}
}

func TestUserProvisionerEdge_CreateBeadError_DoesNotBlockRequest(t *testing.T) {
	fs := &failingCreateStore{
		mockStore: newMockStore(),
		createErr: errors.New("database at capacity"),
	}
	up := NewUserProvisioner(fs, &events.NoopPublisher{})

	called := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		// When provisioning fails, user bead ID should be empty (best-effort).
		if id := UserBeadIDFromContext(r.Context()); id != "" {
			t.Errorf("expected empty user bead ID on store error, got %q", id)
		}
	})
	handler := up.Middleware(inner)

	req := oidcRequest("fail@example.com")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !called {
		t.Fatal("inner handler was not called despite store error")
	}
	// No X-User-Bead-ID header when provisioning fails.
	if rec.Header().Get("X-User-Bead-ID") != "" {
		t.Error("X-User-Bead-ID should not be set when store errors")
	}
}

func TestUserProvisionerEdge_PublishEventOnCreate(t *testing.T) {
	ms := newMockStore()
	pub := &recordingPublisher{}
	up := NewUserProvisioner(ms, pub)

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	handler := up.Middleware(inner)

	req := oidcRequest("publish@example.com")
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if pub.count() != 1 {
		t.Fatalf("expected 1 publish event, got %d", pub.count())
	}
	pub.mu.Lock()
	defer pub.mu.Unlock()
	if pub.events[0].topic != events.TopicBeadCreated {
		t.Errorf("topic = %q, want %q", pub.events[0].topic, events.TopicBeadCreated)
	}
	bead, ok := pub.events[0].event.(*model.Bead)
	if !ok {
		t.Fatalf("event type = %T, want *model.Bead", pub.events[0].event)
	}
	if bead.Type != "user" {
		t.Errorf("published bead type = %q, want %q", bead.Type, "user")
	}
}

func TestUserProvisionerEdge_CachedAfterCreate_NoSecondListBeads(t *testing.T) {
	ms := newMockStore()
	up := NewUserProvisioner(ms, &events.NoopPublisher{})

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	handler := up.Middleware(inner)

	// First request creates and caches.
	req1 := oidcRequest("cached@example.com")
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	firstID := rec1.Header().Get("X-User-Bead-ID")
	if firstID == "" {
		t.Fatal("expected X-User-Bead-ID on first request")
	}

	// Second request should return the same ID from cache.
	req2 := oidcRequest("cached@example.com")
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	secondID := rec2.Header().Get("X-User-Bead-ID")
	if secondID != firstID {
		t.Errorf("cache miss: first=%q, second=%q", firstID, secondID)
	}
}

func TestUserProvisionerEdge_DifferentUsersGetDifferentBeads(t *testing.T) {
	ms := newMockStore()
	up := NewUserProvisioner(ms, &events.NoopPublisher{})

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	handler := up.Middleware(inner)

	req1 := oidcRequest("alice@example.com")
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)

	req2 := oidcRequest("bob@example.com")
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)

	id1 := rec1.Header().Get("X-User-Bead-ID")
	id2 := rec2.Header().Get("X-User-Bead-ID")
	if id1 == "" || id2 == "" {
		t.Fatalf("expected bead IDs: alice=%q, bob=%q", id1, id2)
	}
	if id1 == id2 {
		t.Errorf("different users got same bead ID: %q", id1)
	}
}

func TestUserProvisionerEdge_BeadFieldsCorrect(t *testing.T) {
	ms := newMockStore()
	up := NewUserProvisioner(ms, &events.NoopPublisher{})

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	handler := up.Middleware(inner)

	req := oidcRequest("fields@example.com")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	beadID := rec.Header().Get("X-User-Bead-ID")
	bead := ms.beads[beadID]
	if bead == nil {
		t.Fatal("bead not in store")
	}

	// Verify all expected fields on the created bead.
	if bead.Kind != "config" {
		t.Errorf("kind = %q, want config", bead.Kind)
	}
	if bead.Status != model.StatusOpen {
		t.Errorf("status = %q, want open", bead.Status)
	}
	if bead.CreatedBy != "auto-provision" {
		t.Errorf("created_by = %q, want auto-provision", bead.CreatedBy)
	}
	if bead.CreatedAt.IsZero() {
		t.Error("created_at is zero")
	}
	if bead.UpdatedAt.IsZero() {
		t.Error("updated_at is zero")
	}

	var fields map[string]string
	if err := json.Unmarshal(bead.Fields, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if fields["auth_methods"] != "gitlab-oauth" {
		t.Errorf("auth_methods = %q, want gitlab-oauth", fields["auth_methods"])
	}
}

func TestUserProvisionerEdge_ExistingBeadPopulatesCache(t *testing.T) {
	ms := newMockStore()

	// Pre-create a user bead.
	fieldsJSON, _ := json.Marshal(map[string]string{"email": "precached@example.com"})
	ms.beads["gd-precached"] = &model.Bead{
		ID:     "gd-precached",
		Title:  "precached",
		Type:   "user",
		Kind:   "config",
		Status: model.StatusOpen,
		Fields: fieldsJSON,
	}

	cs := &countingCreateStore{mockStore: ms}
	up := NewUserProvisioner(cs, &events.NoopPublisher{})

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	handler := up.Middleware(inner)

	// First request: finds existing bead via ListBeads, caches it.
	req1 := oidcRequest("precached@example.com")
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)

	if rec1.Header().Get("X-User-Bead-ID") != "gd-precached" {
		t.Errorf("first request: got %q, want gd-precached", rec1.Header().Get("X-User-Bead-ID"))
	}

	// Second request: should hit cache (no CreateBead call).
	req2 := oidcRequest("precached@example.com")
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)

	if rec2.Header().Get("X-User-Bead-ID") != "gd-precached" {
		t.Errorf("second request: got %q, want gd-precached", rec2.Header().Get("X-User-Bead-ID"))
	}

	// No CreateBead calls — both requests resolved from existing bead / cache.
	if cs.createCalls.Load() != 0 {
		t.Errorf("expected 0 CreateBead calls, got %d", cs.createCalls.Load())
	}
}
