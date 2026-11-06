package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
)

func newTestServerWithFleetAuth(t *testing.T, authToken string, fc *FleetConfig) (*BeadsServer, *mockStore) {
	t.Helper()
	ms := newMockStore()
	s := NewBeadsServer(ms, nil)
	s.SetFleetAuth(fc)
	return s, ms
}

func TestHandleRegisterFleetToken(t *testing.T) {
	fc := NewFleetConfig("gardenio", nil)
	srv, _ := newTestServerWithFleetAuth(t, "admin-secret", fc)

	t.Run("Success", func(t *testing.T) {
		body, _ := json.Marshal(registerBridgeTokenRequest{
			Token: "fleet-tok-qa-abc123", // gitleaks:allow: fake token
			Fleet: "qa-fleet",
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

		// Verify token is registered in memory.
		if got := fc.ResolveFleet("fleet-tok-qa-abc123"); got != "qa-fleet" {
			t.Fatalf("expected fleet 'qa-fleet', got %q", got)
		}

		// Verify config was persisted.
		cfg, err := srv.store.GetConfig(req.Context(), "bridge_token:qa-fleet")
		if err != nil {
			t.Fatalf("expected config to be persisted, got error: %v", err)
		}
		var stored map[string]string
		if err := json.Unmarshal(cfg.Value, &stored); err != nil {
			t.Fatalf("failed to unmarshal stored token: %v", err)
		}
		if stored["token"] != "fleet-tok-qa-abc123" {
			t.Fatalf("stored token = %q, want %q", stored["token"], "fleet-tok-qa-abc123")
		}
	})

	t.Run("NonAdminForbidden", func(t *testing.T) {
		body, _ := json.Marshal(registerBridgeTokenRequest{
			Token: "some-token",
			Fleet: "other-fleet",
		})
		req := httptest.NewRequest(http.MethodPost, "/v1/fleet-tokens", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer fleet-tok-qa-abc123") // fleet token, not admin
		rec := httptest.NewRecorder()

		srv.handleRegisterBridgeToken(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Fatalf("expected 403, got %d; body: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("MissingToken", func(t *testing.T) {
		body, _ := json.Marshal(registerBridgeTokenRequest{Fleet: "qa-fleet"})
		req := httptest.NewRequest(http.MethodPost, "/v1/fleet-tokens", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer admin-secret")
		req = asTokenMinter(req)
		rec := httptest.NewRecorder()

		srv.handleRegisterBridgeToken(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", rec.Code)
		}
	})

	t.Run("MissingFleet", func(t *testing.T) {
		body, _ := json.Marshal(registerBridgeTokenRequest{Token: "some-token"})
		req := httptest.NewRequest(http.MethodPost, "/v1/fleet-tokens", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer admin-secret")
		req = asTokenMinter(req)
		rec := httptest.NewRecorder()

		srv.handleRegisterBridgeToken(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", rec.Code)
		}
	})
}

func TestHandleRegisterFleetToken_WithScopes(t *testing.T) {
	fc := NewFleetConfig("gardenio", nil)
	srv, _ := newTestServerWithFleetAuth(t, "admin-secret", fc)

	body, _ := json.Marshal(registerBridgeTokenRequest{
		Token:        "fleet-tok-scoped",
		Fleet:        "scoped-fleet",
		Project:      "myproject",
		AllowedTypes: []string{"agent", "config"},
		AllowedOps:   []string{"create", "update"},
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

	// Verify scopes registered in memory.
	if got := fc.ResolveProject("fleet-tok-scoped"); got != "myproject" {
		t.Fatalf("expected project 'myproject', got %q", got)
	}
	types := fc.ResolveTypeScope("fleet-tok-scoped")
	if len(types) != 2 || types[0] != "agent" || types[1] != "config" {
		t.Fatalf("expected type scope [agent config], got %v", types)
	}
	ops := fc.ResolveOpScope("fleet-tok-scoped")
	if len(ops) != 2 || ops[0] != "create" || ops[1] != "update" {
		t.Fatalf("expected op scope [create update], got %v", ops)
	}

	// Verify scopes persisted to config store.
	cfg, err := srv.store.GetConfig(req.Context(), "bridge_token:scoped-fleet")
	if err != nil {
		t.Fatalf("expected config to be persisted, got error: %v", err)
	}
	var stored map[string]any
	if err := json.Unmarshal(cfg.Value, &stored); err != nil {
		t.Fatalf("failed to unmarshal stored config: %v", err)
	}
	if stored["project"] != "myproject" {
		t.Fatalf("stored project = %v, want myproject", stored["project"])
	}
	storedTypes, ok := stored["allowed_types"].([]any)
	if !ok || len(storedTypes) != 2 {
		t.Fatalf("stored allowed_types = %v, want [agent config]", stored["allowed_types"])
	}

	// Verify response includes scopes.
	var resp map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp["fleet"] != "scoped-fleet" {
		t.Fatalf("response fleet = %v, want scoped-fleet", resp["fleet"])
	}
	if resp["project"] != "myproject" {
		t.Fatalf("response project = %v, want myproject", resp["project"])
	}
	respTypes, ok := resp["allowed_types"].([]any)
	if !ok || len(respTypes) != 2 {
		t.Fatalf("response allowed_types = %v, want [agent config]", resp["allowed_types"])
	}
}

func TestHandleRegisterFleetToken_WithTTL(t *testing.T) {
	fc := NewFleetConfig("gardenio", nil)
	srv, _ := newTestServerWithFleetAuth(t, "admin-secret", fc)

	body, _ := json.Marshal(registerBridgeTokenRequest{
		Token: "tok-ttl",
		Fleet: "ttl-fleet",
		TTL:   "1h",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/fleet-tokens", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin-secret")
	req = asTokenMinter(req)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	before := time.Now()
	srv.handleRegisterBridgeToken(rec, req)
	after := time.Now()

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d; body: %s", rec.Code, rec.Body.String())
	}

	gotExp := fc.ResolveExpiresAt("tok-ttl")
	if gotExp.IsZero() {
		t.Fatal("expected non-zero expiration after TTL registration")
	}
	if gotExp.Before(before.Add(time.Hour)) || gotExp.After(after.Add(time.Hour+time.Second)) {
		t.Fatalf("expiration %v is outside expected window [%v..%v]", gotExp, before.Add(time.Hour), after.Add(time.Hour+time.Second))
	}

	// Round-trip the persisted JSON: it must include expires_at (RFC3339), and
	// reloading must reproduce the same expiry.
	cfg, err := srv.store.GetConfig(req.Context(), "bridge_token:ttl-fleet")
	if err != nil {
		t.Fatalf("expected persisted config, got error: %v", err)
	}
	var stored map[string]any
	if err := json.Unmarshal(cfg.Value, &stored); err != nil {
		t.Fatalf("unmarshal stored: %v", err)
	}
	expRaw, ok := stored["expires_at"].(string)
	if !ok || expRaw == "" {
		t.Fatalf("stored config missing expires_at: %v", stored)
	}
	parsed, perr := time.Parse(time.RFC3339Nano, expRaw)
	if perr != nil {
		t.Fatalf("stored expires_at not RFC3339Nano: %v (raw=%s)", perr, expRaw)
	}
	if !parsed.Equal(gotExp) {
		t.Fatalf("persisted expiry %v differs from in-memory %v", parsed, gotExp)
	}
}

func TestHandleRegisterFleetToken_WithExpiresAt(t *testing.T) {
	fc := NewFleetConfig("gardenio", nil)
	srv, _ := newTestServerWithFleetAuth(t, "admin-secret", fc)

	expires := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	body, _ := json.Marshal(registerBridgeTokenRequest{
		Token:     "tok-abs",
		Fleet:     "abs-fleet",
		ExpiresAt: expires.Format(time.RFC3339),
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
	got := fc.ResolveExpiresAt("tok-abs")
	if !got.Equal(expires) {
		t.Fatalf("ResolveExpiresAt = %v, want %v", got, expires)
	}
}

func TestHandleRegisterFleetToken_TTLAndExpiresMutuallyExclusive(t *testing.T) {
	fc := NewFleetConfig("gardenio", nil)
	srv, _ := newTestServerWithFleetAuth(t, "admin-secret", fc)

	body, _ := json.Marshal(registerBridgeTokenRequest{
		Token:     "tok",
		Fleet:     "f",
		TTL:       "1h",
		ExpiresAt: "2030-01-01T00:00:00Z",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/fleet-tokens", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin-secret")
	req = asTokenMinter(req)
	rec := httptest.NewRecorder()
	srv.handleRegisterBridgeToken(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestHandleRegisterFleetToken_InvalidTTL(t *testing.T) {
	fc := NewFleetConfig("gardenio", nil)
	srv, _ := newTestServerWithFleetAuth(t, "admin-secret", fc)

	for _, tc := range []struct {
		name string
		ttl  string
	}{
		{"NotADuration", "abc"},
		{"Zero", "0s"},
		{"Negative", "-5m"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, _ := json.Marshal(registerBridgeTokenRequest{
				Token: "t", Fleet: "f", TTL: tc.ttl,
			})
			req := httptest.NewRequest(http.MethodPost, "/v1/fleet-tokens", bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer admin-secret")
			req = asTokenMinter(req)
			rec := httptest.NewRecorder()
			srv.handleRegisterBridgeToken(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d; body: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestHandleRegisterFleetToken_BackwardCompatNoTTL(t *testing.T) {
	// No TTL or ExpiresAt → token never expires (zero ExpiresAt).
	fc := NewFleetConfig("gardenio", nil)
	srv, _ := newTestServerWithFleetAuth(t, "admin-secret", fc)

	body, _ := json.Marshal(registerBridgeTokenRequest{Token: "tok-noexp", Fleet: "f"})
	req := httptest.NewRequest(http.MethodPost, "/v1/fleet-tokens", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin-secret")
	req = asTokenMinter(req)
	rec := httptest.NewRecorder()
	srv.handleRegisterBridgeToken(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d", rec.Code)
	}
	if !fc.ResolveExpiresAt("tok-noexp").IsZero() {
		t.Fatal("token registered without ttl/expires_at must have zero expiry")
	}
	if fc.IsExpired("tok-noexp") {
		t.Fatal("token registered without ttl/expires_at must not be reported expired")
	}

	// Persisted JSON must NOT contain expires_at when not set.
	cfg, err := srv.store.GetConfig(req.Context(), "bridge_token:f")
	if err != nil {
		t.Fatalf("expected persisted config, got: %v", err)
	}
	var stored map[string]any
	_ = json.Unmarshal(cfg.Value, &stored)
	if _, present := stored["expires_at"]; present {
		t.Fatalf("persisted config should not include expires_at when not set: %v", stored)
	}
}

func TestHandleListFleetTokens(t *testing.T) {
	fc := NewFleetConfig("gardenio", map[string]string{
		"tok-qa":      "qa-fleet",
		"tok-staging": "staging",
	})
	srv, _ := newTestServerWithFleetAuth(t, "admin-secret", fc)

	t.Run("Success", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/fleet-tokens", nil)
		req.Header.Set("Authorization", "Bearer admin-secret")
		req = asTokenMinter(req)
		rec := httptest.NewRecorder()

		srv.handleListBridgeTokens(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
		}

		var resp struct {
			Fleets []string `json:"fleets"`
		}
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if len(resp.Fleets) != 2 {
			t.Fatalf("expected 2 fleets, got %d: %v", len(resp.Fleets), resp.Fleets)
		}
		// ListBridgeTokens returns sorted results.
		if resp.Fleets[0] != "qa-fleet" || resp.Fleets[1] != "staging" {
			t.Fatalf("unexpected fleets: %v", resp.Fleets)
		}
	})

	t.Run("NonAdminForbidden", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/fleet-tokens", nil)
		req.Header.Set("Authorization", "Bearer tok-qa")
		rec := httptest.NewRecorder()

		srv.handleListBridgeTokens(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Fatalf("expected 403, got %d", rec.Code)
		}
	})

	t.Run("EmptyList", func(t *testing.T) {
		emptyFc := NewFleetConfig("gardenio", nil)
		emptySrv, _ := newTestServerWithFleetAuth(t, "admin-secret", emptyFc)

		req := httptest.NewRequest(http.MethodGet, "/v1/fleet-tokens", nil)
		req.Header.Set("Authorization", "Bearer admin-secret")
		req = asTokenMinter(req)
		rec := httptest.NewRecorder()

		emptySrv.handleListBridgeTokens(rec, req)

		var resp struct {
			Fleets []string `json:"fleets"`
		}
		_ = json.NewDecoder(rec.Body).Decode(&resp)
		if len(resp.Fleets) != 0 {
			t.Fatalf("expected empty list, got %v", resp.Fleets)
		}
	})
}

func TestHandleListFleetTokens_Detailed(t *testing.T) {
	fc := NewFleetConfig("gardenio", nil)
	expSlack := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	expGitlab := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	fc.RegisterTokenWithSubject(
		"tok-slack", "slack-bridge", "", nil, nil, expSlack, "bridge:slack",
	)
	fc.RegisterTokenWithSubject(
		"tok-gitlab", "gitlab-bridge", "", nil, nil, expGitlab, "bridge:gitlab",
	)
	fc.RegisterToken("tok-seed", "default") // no expiry

	srv, _ := newTestServerWithFleetAuth(t, "admin-secret", fc)

	req := httptest.NewRequest(http.MethodGet, "/v1/fleet-tokens?detailed=true", nil)
	req.Header.Set("Authorization", "Bearer admin-secret")
	req = asTokenMinter(req)
	rec := httptest.NewRecorder()

	srv.handleListBridgeTokens(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Fleets []struct {
			Name           string `json:"name"`
			Project        string `json:"project"`
			Subject        string `json:"subject"`
			ExpiresAt      string `json:"expires_at"`
			CrossFleetRead bool   `json:"cross_fleet_read"`
		} `json:"fleets"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Fleets) != 3 {
		t.Fatalf("expected 3 fleets, got %d: %+v", len(resp.Fleets), resp.Fleets)
	}
	// Sorted by name: default, gitlab-bridge, slack-bridge.
	if resp.Fleets[0].Name != "default" || resp.Fleets[1].Name != "gitlab-bridge" || resp.Fleets[2].Name != "slack-bridge" {
		t.Fatalf("unexpected fleet order: %+v", resp.Fleets)
	}
	if resp.Fleets[0].ExpiresAt != "" {
		t.Fatalf("default fleet (no expiry) should omit expires_at, got %q", resp.Fleets[0].ExpiresAt)
	}
	if resp.Fleets[1].Subject != "bridge:gitlab" || resp.Fleets[2].Subject != "bridge:slack" {
		t.Fatalf("unexpected subjects: %+v", resp.Fleets)
	}
	if resp.Fleets[1].ExpiresAt == "" || resp.Fleets[2].ExpiresAt == "" {
		t.Fatalf("expected expires_at on bridge fleets, got %+v", resp.Fleets)
	}
}

func TestHandleRevokeBridgeToken(t *testing.T) {
	fc := NewFleetConfig("gardenio", map[string]string{
		"tok-qa": "qa-fleet",
	})
	srv, ms := newTestServerWithFleetAuth(t, "admin-secret", fc)

	// Pre-persist the config so delete has something to remove.
	tokenJSON, _ := json.Marshal("tok-qa")
	_ = ms.SetConfig(context.TODO(), &model.Config{
		Key:   "fleet_token:qa-fleet",
		Value: tokenJSON,
	})

	t.Run("Success", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodDelete, "/v1/fleet-tokens/qa-fleet", nil)
		req.SetPathValue("fleet", "qa-fleet")
		req.Header.Set("Authorization", "Bearer admin-secret")
		req = asTokenMinter(req)
		rec := httptest.NewRecorder()

		srv.handleRevokeBridgeToken(rec, req)

		if rec.Code != http.StatusNoContent {
			t.Fatalf("expected 204, got %d; body: %s", rec.Code, rec.Body.String())
		}

		// Verify removed from memory.
		if fc.HasToken("tok-qa") {
			t.Fatal("expected token to be removed from memory")
		}
	})

	t.Run("NotFound", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodDelete, "/v1/fleet-tokens/nonexistent", nil)
		req.SetPathValue("fleet", "nonexistent")
		req.Header.Set("Authorization", "Bearer admin-secret")
		req = asTokenMinter(req)
		rec := httptest.NewRecorder()

		srv.handleRevokeBridgeToken(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d", rec.Code)
		}
	})

	t.Run("NonAdminForbidden", func(t *testing.T) {
		// Re-register a token for this test.
		fc.RegisterToken("tok-staging", "staging")

		req := httptest.NewRequest(http.MethodDelete, "/v1/fleet-tokens/staging", nil)
		req.SetPathValue("fleet", "staging")
		req.Header.Set("Authorization", "Bearer tok-staging")
		rec := httptest.NewRecorder()

		srv.handleRevokeBridgeToken(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Fatalf("expected 403, got %d", rec.Code)
		}
	})
}

func TestFleetTokenRegistrationAuthFlow(t *testing.T) {
	// End-to-end: register a fleet token, then verify it works for authentication.
	fc := NewFleetConfig("gardenio", nil)
	srv, _ := newTestServerWithFleetAuth(t, "admin-secret", fc)

	// Register a new fleet token.
	body, _ := json.Marshal(registerBridgeTokenRequest{
		Token: "fleet-new-token",
		Fleet: "new-fleet",
	})
	regReq := httptest.NewRequest(http.MethodPost, "/v1/fleet-tokens", bytes.NewReader(body))
	regReq.Header.Set("Authorization", "Bearer admin-secret")
	regReq = asTokenMinter(regReq)
	regReq.Header.Set("Content-Type", "application/json")
	regRec := httptest.NewRecorder()
	srv.handleRegisterBridgeToken(regRec, regReq)

	if regRec.Code != http.StatusCreated {
		t.Fatalf("registration failed: %d %s", regRec.Code, regRec.Body.String())
	}

	// Verify the newly registered token is valid for auth.
	if !fc.HasToken("fleet-new-token") {
		t.Fatal("newly registered fleet token should be valid")
	}

	// Verify the token resolves to the correct fleet.
	if got := fc.ResolveFleet("fleet-new-token"); got != "new-fleet" {
		t.Fatalf("expected 'new-fleet', got %q", got)
	}

	// Revoke the token.
	revokeReq := httptest.NewRequest(http.MethodDelete, "/v1/fleet-tokens/new-fleet", nil)
	revokeReq.SetPathValue("fleet", "new-fleet")
	revokeReq.Header.Set("Authorization", "Bearer admin-secret")
	revokeReq = asTokenMinter(revokeReq)
	revokeRec := httptest.NewRecorder()
	srv.handleRevokeBridgeToken(revokeRec, revokeReq)

	if revokeRec.Code != http.StatusNoContent {
		t.Fatalf("revocation failed: %d", revokeRec.Code)
	}

	// Verify the token is no longer valid.
	if fc.HasToken("fleet-new-token") {
		t.Fatal("revoked fleet token should no longer be valid")
	}
}

func TestIsAdminRequest(t *testing.T) {
	srv, _ := newTestServerWithFleetAuth(t, "", NewFleetConfig("default", nil))
	req := func(ctx context.Context) *http.Request {
		return httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
	}

	t.Run("OIDCAdmin", func(t *testing.T) {
		if !srv.isAdminRequest(asOIDCAdmin(req(context.Background()))) {
			t.Fatal("expected an OIDC admin to be an admin request")
		}
	})
	t.Run("OIDCNonAdmin", func(t *testing.T) {
		r := req(WithRoles(withAuthMethod(context.Background(), authMethodOIDC), []string{RoleOperator}))
		if srv.isAdminRequest(r) {
			t.Fatal("expected an OIDC operator not to be an admin request")
		}
	})
	t.Run("FleetTokenWithAdminLookingBearer", func(t *testing.T) {
		r := req(withAuthMethod(context.Background(), authMethodStatic))
		r.Header.Set("Authorization", "Bearer admin-token")
		if srv.isAdminRequest(r) {
			t.Fatal("expected a fleet token never to be an admin request")
		}
	})
	t.Run("MachinePrincipal", func(t *testing.T) {
		if srv.isAdminRequest(req(machineCtx("gardenio-controller", "gardenio-machine-token-mint"))) {
			t.Fatal("expected a machine principal not to be an admin request")
		}
	})
	t.Run("NoIdentity", func(t *testing.T) {
		if srv.isAdminRequest(req(context.Background())) {
			t.Fatal("expected a request with no identity not to be an admin request")
		}
	})
	t.Run("AuthDisabled", func(t *testing.T) {
		if !srv.isAdminRequest(req(authDisabledCtx())) {
			t.Fatal("expected true when no auth is configured")
		}
	})
}

// TestIsTokenMintRequest pins who may use the bridge/fleet-token routes now
// that the shared static token is gone (gd-ke6PC7bRto): the token-mint machine
// principal, and an auth-disabled daemon's callers. A bearer that looks like
// the old admin token, a fleet token and an OIDC admin are all refused.
func TestIsTokenMintRequest(t *testing.T) {
	srv, _ := newTestServerWithFleetAuth(t, "", NewFleetConfig("default", nil))
	req := func(ctx context.Context) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/v1/bridge-tokens", nil).WithContext(ctx)
		r.Header.Set("Authorization", "Bearer admin-secret")
		return r
	}
	cases := []struct {
		name string
		ctx  context.Context
		want bool
	}{
		{"machine with token-mint", machineCtx("gardenio-controller", "gardenio-machine-token-mint"), true},
		{"machine without token-mint", machineCtx("gardenio-controller", "gardenio-machine-accounts"), false},
		{"auth disabled", authDisabledCtx(), true},
		{"fleet token", withAuthMethod(context.Background(), authMethodStatic), false},
		{"oidc admin", WithRoles(withAuthMethod(context.Background(), authMethodOIDC), []string{RoleAdmin}), false},
		{"no identity", context.Background(), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := srv.isTokenMintRequest(req(tc.ctx)); got != tc.want {
				t.Fatalf("isTokenMintRequest = %v, want %v", got, tc.want)
			}
		})
	}
}

// mintReq posts a registration with the given Authorization header and request
// context, so a test can present the static token, an OIDC admin or a machine
// principal to handleRegisterBridgeToken.
func mintReq(t *testing.T, srv *BeadsServer, ctx context.Context, bearer string, body registerBridgeTokenRequest) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/bridge-tokens", bytes.NewReader(b)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+bearer)
	rec := httptest.NewRecorder()
	srv.handleRegisterBridgeToken(rec, req)
	return rec
}

// TestHandleRegisterBridgeToken_UntrustedCallerCannotMintServiceIdentity pins
// gd-ua4DGNtZdG: a caller that is not the token-mint machine principal (only an
// auth-disabled daemon admits one) must not mint a token whose subject or
// privilege is a service identity.
func TestHandleRegisterBridgeToken_UntrustedCallerCannotMintServiceIdentity(t *testing.T) {
	cases := []struct {
		name string
		body registerBridgeTokenRequest
	}{
		{"scif portal subject", registerBridgeTokenRequest{Token: "t1", Fleet: "zz", Subject: "bridge:acme-scif"}},
		{"bridge subject on a reserved fleet", registerBridgeTokenRequest{Token: "t2", Fleet: "acme-scif", Subject: "bridge:acme-scif"}},
		{"reserved relay subject", registerBridgeTokenRequest{Token: "t3", Fleet: "zz", Subject: "slack-bridge"}},
		{"reserved subject, mixed case", registerBridgeTokenRequest{Token: "t4", Fleet: "zz", Subject: " Slack-Bridge "}},
		{"github relay subject", registerBridgeTokenRequest{Token: "t9", Fleet: "qa", Subject: "github-bridge"}},
		{"email subject", registerBridgeTokenRequest{Token: "t5", Fleet: "zz", Subject: "op@example.com"}},
		{"reserved fleet, no subject", registerBridgeTokenRequest{Token: "t6", Fleet: "gitlab-bridge"}},
		{"namespaced fleet name", registerBridgeTokenRequest{Token: "t7", Fleet: "bridge:acme-scif"}},
		{"privileged", registerBridgeTokenRequest{Token: "t8", Fleet: "zz", Privileged: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := NewFleetConfig("gardenio", nil)
			srv, ms := newTestServerWithFleetAuth(t, "admin-secret", fc)
			rec := mintReq(t, srv, authDisabledCtx(), "admin-secret", tc.body)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403; body: %s", rec.Code, rec.Body.String())
			}
			if got := fc.ResolveFleet(tc.body.Token); got == tc.body.Fleet {
				t.Fatalf("token was registered under fleet %q despite the refusal", got)
			}
			if _, err := ms.GetConfig(context.Background(), "bridge_token:"+tc.body.Fleet); err == nil {
				t.Fatalf("refused mint was persisted")
			}
		})
	}
}

// A name an operator adds to BEADS_BRIDGE_SUBJECTS is trusted to relay
// X-Operated-By, so an untrusted caller must not mint it as a subject or fleet even
// though it is not in reservedServiceNames (gd-wStnnYEt3M).
func TestHandleRegisterBridgeToken_UntrustedCallerCannotMintConfiguredBridgeSubject(t *testing.T) {
	for name, body := range map[string]registerBridgeTokenRequest{
		"subject":             {Token: "c1", Fleet: "qa", Subject: "teams-relay"},
		"subject, mixed case": {Token: "c2", Fleet: "qa", Subject: " Teams-Relay "},
		"fleet":               {Token: "c3", Fleet: "teams-relay", Subject: "qa-runner"},
	} {
		t.Run(name, func(t *testing.T) {
			fc := NewFleetConfig("gardenio", nil)
			srv, _ := newTestServerWithFleetAuth(t, "admin-secret", fc)
			srv.SetBridgeSubjects([]string{"slack-bridge", "teams-relay"})
			rec := mintReq(t, srv, authDisabledCtx(), "admin-secret", body)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403; body: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestHandleRegisterBridgeToken_UntrustedCallerOrdinaryFleetStillWorks keeps the
// non-service path open: a plain fleet, a plain subject.
func TestHandleRegisterBridgeToken_UntrustedCallerOrdinaryFleetStillWorks(t *testing.T) {
	fc := NewFleetConfig("gardenio", nil)
	srv, _ := newTestServerWithFleetAuth(t, "admin-secret", fc)
	rec := mintReq(t, srv, authDisabledCtx(), "admin-secret",
		registerBridgeTokenRequest{Token: "ok-tok", Fleet: "qa-fleet", Subject: "qa-runner"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body: %s", rec.Code, rec.Body.String())
	}
	if got := fc.ResolveSubject("ok-tok"); got != "qa-runner" {
		t.Fatalf("subject = %q, want qa-runner", got)
	}
}

// TestHandleRegisterBridgeToken_TrustedMintersMayMintServiceIdentity: the
// controller (token-mint machine principal) keeps the bridge:* and
// privileged mints the rotator relies on.
func TestHandleRegisterBridgeToken_TrustedMintersMayMintServiceIdentity(t *testing.T) {
	body := func(tok string) registerBridgeTokenRequest {
		return registerBridgeTokenRequest{Token: tok, Fleet: "gitlab-bridge", Subject: "bridge:gitlab", Privileged: true}
	}
	t.Run("machine with token-mint", func(t *testing.T) {
		fc := NewFleetConfig("gardenio", nil)
		srv, _ := newTestServerWithFleetAuth(t, "admin-secret", fc)
		rec := mintReq(t, srv, machineCtx("gardenio-controller", "gardenio-machine-token-mint"), "machine-jwt", body("m-tok"))
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201; body: %s", rec.Code, rec.Body.String())
		}
		if !fc.ResolvePrivileged("m-tok") || fc.ResolveSubject("m-tok") != "bridge:gitlab" {
			t.Fatalf("machine mint did not keep subject/privileged")
		}
	})
	t.Run("machine without token-mint is still refused", func(t *testing.T) {
		fc := NewFleetConfig("gardenio", nil)
		srv, _ := newTestServerWithFleetAuth(t, "admin-secret", fc)
		rec := mintReq(t, srv, machineCtx("gardenio-controller", "gardenio-machine-accounts"), "machine-jwt", body("x-tok"))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
	})
}

// TestHandleRegisterBridgeToken_UntrustedCallerCannotMintAgentFleet pins the
// agent-* reservation (gd-VuYwGGYLLT): the bead id the continuation gate, the
// ownership bypass and the taint lookups act on is the suffix of an agent-*
// fleet name, so the static token every pod holds must not be able to pick it.
func TestHandleRegisterBridgeToken_UntrustedCallerCannotMintAgentFleet(t *testing.T) {
	for name, fleet := range map[string]string{
		"a captain's bead id":     "agent-gd-captain1",
		"mixed case prefix":       "Agent-gd-captain1",
		"padded":                  " agent-gd-captain1 ",
		"bare prefix with a name": "agent-x",
	} {
		t.Run(name, func(t *testing.T) {
			fc := NewFleetConfig("gardenio", nil)
			srv, ms := newTestServerWithFleetAuth(t, "admin-secret", fc)
			rec := mintReq(t, srv, authDisabledCtx(), "admin-secret",
				registerBridgeTokenRequest{Token: "forged-tok", Fleet: fleet, Subject: "crew-a"})
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403; body: %s", rec.Code, rec.Body.String())
			}
			if got := fc.ResolveFleet("forged-tok"); isAgentFleet(got) {
				t.Fatalf("token resolves to agent fleet %q despite the refusal", got)
			}
			if _, err := ms.GetConfig(context.Background(), "bridge_token:"+fleet); err == nil {
				t.Fatalf("refused mint was persisted")
			}
		})
	}
}

// TestHandleRegisterBridgeToken_UntrustedCallerCannotMintBridgeSuffixFleet pins the
// *-bridge reservation (gd-VuYwGGYLLT): isBridgeFleetIdentity admits any fleet
// ending in -bridge as a decision resolver, so the static token every pod holds
// must not be able to name one.
func TestHandleRegisterBridgeToken_UntrustedCallerCannotMintBridgeSuffixFleet(t *testing.T) {
	for name, fleet := range map[string]string{
		"plain suffix":      "x-bridge",
		"mixed case suffix": "X-Bridge",
		"padded":            " x-bridge ",
		"unreserved prefix": "my-team-bridge",
	} {
		t.Run(name, func(t *testing.T) {
			fc := NewFleetConfig("gardenio", nil)
			srv, ms := newTestServerWithFleetAuth(t, "admin-secret", fc)
			rec := mintReq(t, srv, authDisabledCtx(), "admin-secret",
				registerBridgeTokenRequest{Token: "forged-tok", Fleet: fleet, Subject: "crew-a"})
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403; body: %s", rec.Code, rec.Body.String())
			}
			if got := fc.ResolveFleet("forged-tok"); isBridgeFleetIdentity(got, "crew-a") {
				t.Fatalf("token resolves to bridge fleet %q despite the refusal", got)
			}
			if _, err := ms.GetConfig(context.Background(), "bridge_token:"+fleet); err == nil {
				t.Fatalf("refused mint was persisted")
			}
		})
	}
}

// The controller (token-mint machine principal) is the one minter of *-bridge.
func TestHandleRegisterBridgeToken_TokenMintMachineMayMintBridgeSuffixFleet(t *testing.T) {
	fc := NewFleetConfig("gardenio", nil)
	srv, _ := newTestServerWithFleetAuth(t, "admin-secret", fc)
	rec := mintReq(t, srv, machineCtx("gardenio-controller", "gardenio-machine-token-mint"), "machine-jwt",
		registerBridgeTokenRequest{Token: "b-tok", Fleet: "x-bridge", Subject: "x-runner"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body: %s", rec.Code, rec.Body.String())
	}
}

// The controller (token-mint machine principal) is the one minter of agent-*.
func TestHandleRegisterBridgeToken_TokenMintMachineMayMintAgentFleet(t *testing.T) {
	fc := NewFleetConfig("gardenio", nil)
	srv, _ := newTestServerWithFleetAuth(t, "admin-secret", fc)
	rec := mintReq(t, srv, machineCtx("gardenio-controller", "gardenio-machine-token-mint"), "machine-jwt",
		registerBridgeTokenRequest{Token: "agent-tok", Fleet: "agent-gd-captain1", Subject: "captain-a"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body: %s", rec.Code, rec.Body.String())
	}
	if got := fc.ResolveFleet("agent-tok"); got != "agent-gd-captain1" {
		t.Fatalf("fleet = %q, want agent-gd-captain1", got)
	}
}
