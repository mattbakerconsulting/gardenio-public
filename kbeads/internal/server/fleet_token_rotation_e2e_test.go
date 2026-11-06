package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/kbeads/internal/events"
)

// TestFleetTokenRotationE2E exercises the bridge-fleet-token rotation
// flow against a full HTTP handler stack:
//
//  1. Operator registers a fleet token with TTL=1s + with_refresh=true.
//  2. The token initially passes auth (positive control).
//  3. After the TTL window, an authenticated request fails with 401 +
//     `WWW-Authenticate: Bearer error="invalid_token"` and the
//     `refresh_endpoint` hint.
//  4. The bridge exchanges the stored refresh-token at
//     `POST /v1/auth/refresh` and gets a fresh access token.
//  5. The fresh access token immediately authenticates.
//
// This is the daemon-side guard for the bridge rotation cron
// (ka-0000002353): the cron mints a new access token via refresh; if
// any step here breaks the bridges silently 401 in steady-state.
func TestFleetTokenRotationE2E(t *testing.T) {
	const adminToken = "admin-rotation-e2e"

	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	fc := NewFleetConfig("default", nil)
	srv.SetFleetAuth(fc)
	mgr := NewRefreshTokenManager(ms, fc)
	srv.SetRefreshTokenManager(mgr)
	srv.SetRefreshRotation(false)

	handler := withMinterRoutes(srv, adminToken, srv.NewHTTPHandler(nil, withTestToken(fc, adminToken)))
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	// ── Step 1: register a fleet token with ttl=1s and ask for a refresh.
	reqBody, _ := json.Marshal(registerBridgeTokenRequest{
		Token:       "bridge-slack-rotate-tok",
		Fleet:       "bridge-slack",
		Subject:     "rotation-subject",
		TTL:         "1s",
		WithRefresh: true,
	})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/fleet-tokens", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("register: expected 201, got %d; body: %s", resp.StatusCode, body)
	}
	var regResp map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&regResp); err != nil {
		resp.Body.Close()
		t.Fatalf("decode register response: %v", err)
	}
	resp.Body.Close()
	refreshTok, _ := regResp["refresh_token"].(string)
	if refreshTok == "" {
		t.Fatalf("expected refresh_token in register response, got %#v", regResp)
	}

	// ── Step 2: positive control — token works before TTL elapses.
	if code, _, _ := doAuthed(t, ts, http.MethodGet, "/v1/beads", "bridge-slack-rotate-tok", nil); code != http.StatusOK {
		t.Fatalf("pre-expiry request: expected 200, got %d", code)
	}

	// ── Step 3: wait past the TTL boundary and assert 401 +
	// WWW-Authenticate: Bearer error="invalid_token".
	time.Sleep(1500 * time.Millisecond)
	code, hdr, _ := doAuthed(t, ts, http.MethodGet, "/v1/beads", "bridge-slack-rotate-tok", nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("post-expiry request: expected 401, got %d", code)
	}
	wwwAuth := hdr.Get("WWW-Authenticate")
	if !strings.Contains(wwwAuth, `error="invalid_token"`) {
		t.Fatalf(`WWW-Authenticate = %q, want one containing error="invalid_token"`, wwwAuth)
	}
	if !strings.Contains(wwwAuth, "/v1/auth/refresh") {
		t.Fatalf("WWW-Authenticate = %q, want refresh_endpoint hint", wwwAuth)
	}

	// ── Step 4: exchange refresh for a new access token.
	rBody, _ := json.Marshal(refreshRequest{RefreshToken: refreshTok})
	rReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/auth/refresh", bytes.NewReader(rBody))
	rReq.Header.Set("Content-Type", "application/json")
	rResp, err := http.DefaultClient.Do(rReq)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if rResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(rResp.Body)
		rResp.Body.Close()
		t.Fatalf("refresh: expected 200, got %d; body: %s", rResp.StatusCode, body)
	}
	var refreshResp refreshResponse
	if err := json.NewDecoder(rResp.Body).Decode(&refreshResp); err != nil {
		rResp.Body.Close()
		t.Fatalf("decode refresh response: %v", err)
	}
	rResp.Body.Close()
	if refreshResp.AccessToken == "" {
		t.Fatal("expected fresh access_token from refresh exchange")
	}
	if refreshResp.TokenType != "Bearer" {
		t.Fatalf("token_type = %q, want Bearer", refreshResp.TokenType)
	}
	// Subject must survive the rotation boundary.
	if got := fc.ResolveSubject(refreshResp.AccessToken); got != "rotation-subject" {
		t.Fatalf("refreshed access token subject = %q, want rotation-subject", got)
	}

	// ── Step 5: fresh access token authenticates immediately.
	if code, _, _ := doAuthed(t, ts, http.MethodGet, "/v1/beads", refreshResp.AccessToken, nil); code != http.StatusOK {
		t.Fatalf("post-refresh request: expected 200, got %d", code)
	}
}

// doAuthed makes a request with a Bearer token and returns the status,
// response headers, and body bytes. Helper for the rotation E2E.
func doAuthed(t *testing.T, ts *httptest.Server, method, path, token string, body any) (int, http.Header, []byte) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, ts.URL+path, r)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, b
}
