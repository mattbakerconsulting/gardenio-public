package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// TestAddComment_FleetTokenWithSubject_UsesSubject covers the C1.4 happy path:
// a fleet token registered with a non-empty Subject yields an AuthIdentity
// whose Subject is the registered value (e.g. "agent:gd-X"), not the fleet
// name. The comment author records the Subject, giving per-identity audit
// attribution.
func TestAddComment_FleetTokenWithSubject_UsesSubject(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})

	fc := NewFleetConfig("default", nil)
	fc.RegisterTokenWithSubject("pat-tok-1", "agent-gd-abc", "", nil, nil, time.Time{}, "agent:gd-abc")
	h := s.NewHTTPHandler(nil, withTestToken(fc, "primary-token"))

	ms.beads["gd-cmt-subj"] = &model.Bead{ID: "gd-cmt-subj", Title: "B", Type: model.TypeTask, Status: model.StatusOpen}

	body, _ := json.Marshal(map[string]any{"text": "probe"})
	req := httptest.NewRequest(http.MethodPost, "/v1/beads/gd-cmt-subj/comments", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer pat-tok-1")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if got := ms.comments["gd-cmt-subj"][0].Author; got != "agent:gd-abc" {
		t.Fatalf("expected author=%q (subject), got %q", "agent:gd-abc", got)
	}
}

// TestAddComment_FleetTokenWithoutSubject_FallsBackToFleet covers the
// back-compat path: a fleet token registered without a Subject still records
// the fleet name as the audit author. This is the pre-C1.4 behavior.
func TestAddComment_FleetTokenWithoutSubject_FallsBackToFleet(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})

	fc := NewFleetConfig("default", nil)
	fc.RegisterToken("legacy-tok", "legacy-fleet")
	h := s.NewHTTPHandler(nil, withTestToken(fc, "primary-token"))

	ms.beads["gd-cmt-legacy"] = &model.Bead{ID: "gd-cmt-legacy", Title: "B", Status: model.StatusOpen}

	body, _ := json.Marshal(map[string]any{"text": "probe"})
	req := httptest.NewRequest(http.MethodPost, "/v1/beads/gd-cmt-legacy/comments", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer legacy-tok")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if got := ms.comments["gd-cmt-legacy"][0].Author; got != "legacy-fleet" {
		t.Fatalf("expected author=%q (fleet name fallback), got %q", "legacy-fleet", got)
	}
}

// TestRegisterFleetToken_AcceptsSubject covers the HTTP-side: the register
// endpoint accepts a `subject` field, sanitises it, persists it on the
// in-memory FleetConfig, and round-trips it in the response.
func TestRegisterFleetToken_AcceptsSubject(t *testing.T) {
	fc := NewFleetConfig("default", nil)
	srv, _ := newTestServerWithFleetAuth(t, "admin-secret", fc)

	body, _ := json.Marshal(map[string]any{
		"token":   "pat-tok-2",
		"fleet":   "svc-gd-xyz",
		"subject": "svc-gd-xyz",
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
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["subject"] != "svc-gd-xyz" {
		t.Fatalf("response subject=%v, want %q", resp["subject"], "svc-gd-xyz")
	}
	if got := fc.ResolveSubject("pat-tok-2"); got != "svc-gd-xyz" {
		t.Fatalf("ResolveSubject = %q, want %q", got, "svc-gd-xyz")
	}

	// Persisted JSON must include the subject so it survives a daemon restart.
	cfg, err := srv.store.GetConfig(req.Context(), "bridge_token:svc-gd-xyz")
	if err != nil {
		t.Fatalf("expected persisted config, got error: %v", err)
	}
	var stored map[string]any
	if err := json.Unmarshal(cfg.Value, &stored); err != nil {
		t.Fatalf("unmarshal stored: %v", err)
	}
	if stored["subject"] != "svc-gd-xyz" {
		t.Fatalf("persisted subject=%v, want %q", stored["subject"], "svc-gd-xyz")
	}
}

// TestRegisterFleetToken_AcceptsPrivileged covers the HTTP-side for the
// privileged service-fleet flag (ka-0000001946): the register endpoint accepts
// `privileged:true`, persists it on the in-memory FleetConfig, round-trips it
// in the response, and stores it so it survives a daemon restart.
func TestRegisterFleetToken_AcceptsPrivileged(t *testing.T) {
	fc := NewFleetConfig("default", nil)
	srv, _ := newTestServerWithFleetAuth(t, "admin-secret", fc)

	body, _ := json.Marshal(map[string]any{
		"token":      "priv-tok-1",
		"fleet":      "gitlab-bridge",
		"subject":    "bridge:gitlab",
		"privileged": true,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/fleet-tokens", bytes.NewReader(body)).
		WithContext(machineCtx("gardenio-controller", "gardenio-machine-token-mint"))
	req.Header.Set("Authorization", "Bearer admin-secret")
	req = asTokenMinter(req)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.handleRegisterBridgeToken(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["privileged"] != true {
		t.Fatalf("response privileged=%v, want true", resp["privileged"])
	}
	if !fc.ResolvePrivileged("priv-tok-1") {
		t.Fatalf("ResolvePrivileged = false, want true")
	}

	// Persisted JSON must include privileged so it survives a daemon restart.
	cfg, err := srv.store.GetConfig(req.Context(), "bridge_token:gitlab-bridge")
	if err != nil {
		t.Fatalf("expected persisted config, got error: %v", err)
	}
	var stored map[string]any
	if err := json.Unmarshal(cfg.Value, &stored); err != nil {
		t.Fatalf("unmarshal stored: %v", err)
	}
	if stored["privileged"] != true {
		t.Fatalf("persisted privileged=%v, want true", stored["privileged"])
	}
}

// TestRegisterFleetToken_DefaultsNotPrivileged ensures an ordinary fleet
// token registration leaves the privilege bit unset — the bypass is opt-in.
func TestRegisterFleetToken_DefaultsNotPrivileged(t *testing.T) {
	fc := NewFleetConfig("default", nil)
	srv, _ := newTestServerWithFleetAuth(t, "admin-secret", fc)

	body, _ := json.Marshal(map[string]any{
		"token": "ord-tok-1",
		"fleet": "slack-bridge",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/fleet-tokens", bytes.NewReader(body)).
		WithContext(machineCtx("gardenio-controller", "gardenio-machine-token-mint"))
	req.Header.Set("Authorization", "Bearer admin-secret")
	req = asTokenMinter(req)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.handleRegisterBridgeToken(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if fc.ResolvePrivileged("ord-tok-1") {
		t.Fatalf("ordinary fleet token should not be privileged")
	}
}

// TestRegisterFleetToken_RejectsBadSubject ensures subject sanitisation
// matches the X-Actor rules (≤128 chars, no control characters).
func TestRegisterFleetToken_RejectsBadSubject(t *testing.T) {
	fc := NewFleetConfig("default", nil)
	srv, _ := newTestServerWithFleetAuth(t, "admin-secret", fc)

	body, _ := json.Marshal(map[string]any{
		"token":   "pat-tok-3",
		"fleet":   "agent-gd-bad",
		"subject": "evil\r\nX-Injected: yes",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/fleet-tokens", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin-secret")
	req = asTokenMinter(req)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.handleRegisterBridgeToken(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d; body: %s", rec.Code, rec.Body.String())
	}
	// Must not register the token on rejection.
	if fc.HasToken("pat-tok-3") {
		t.Fatalf("token should not be registered on subject-validation failure")
	}
}
