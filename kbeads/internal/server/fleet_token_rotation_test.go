package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestFleetTokenExpiry_AuthMiddleware401 verifies that an expired fleet
// token causes AuthMiddleware to return 401 with
// `WWW-Authenticate: Bearer error="invalid_token"`. Mirrors the bridge
// fleet-token-rotation behaviour required by gd-37h1LUVQXa: when a
// bridge presents an access token past its TTL, the daemon must reject
// it with the OAuth 2.0 standard `invalid_token` error so the bridge's
// refresh-on-401 path can fire.
func TestFleetTokenExpiry_AuthMiddleware401(t *testing.T) {
	fc := NewFleetConfig("default", nil)

	now := time.Date(2026, 5, 24, 0, 0, 0, 0, time.UTC)
	fc.SetNowFunc(func() time.Time { return now })

	// Register a bridge-style fleet token with TTL already elapsed.
	const tok = "bridge-slack-tok"
	past := now.Add(-1 * time.Second)
	fc.RegisterTokenWithSubject(tok, "bridge-slack", "", nil, nil, past, "bridge:slack")

	handler := AuthMiddleware(withTestToken(fc, "admin-secret"), nil, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/beads", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for expired fleet token, got %d; body: %s", rec.Code, rec.Body.String())
	}

	got := rec.Header().Get("WWW-Authenticate")
	if got == "" {
		t.Fatal("expected WWW-Authenticate header to be set")
	}
	if !strings.Contains(got, `error="invalid_token"`) {
		t.Fatalf(`WWW-Authenticate = %q, want one containing error="invalid_token"`, got)
	}
	// Bridges rely on the refresh_endpoint hint to know where to POST
	// their stored refresh token after a 401.
	if !strings.Contains(got, "/v1/auth/refresh") {
		t.Fatalf("WWW-Authenticate = %q, want refresh_endpoint hint", got)
	}
}

// TestFleetTokenExpiry_NotYetExpiredAllows is the positive twin: a
// fleet token whose TTL has not yet elapsed must pass AuthMiddleware.
func TestFleetTokenExpiry_NotYetExpiredAllows(t *testing.T) {
	fc := NewFleetConfig("default", nil)

	now := time.Date(2026, 5, 24, 0, 0, 0, 0, time.UTC)
	fc.SetNowFunc(func() time.Time { return now })

	const tok = "bridge-fresh-tok"
	future := now.Add(1 * time.Hour)
	fc.RegisterTokenWithSubject(tok, "bridge-slack", "", nil, nil, future, "bridge:slack")

	handler := AuthMiddleware(withTestToken(fc, "admin-secret"), nil, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/beads", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for not-yet-expired fleet token, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

// TestRefreshRoundTrip_PreservesSubject verifies that exchanging a
// refresh token yields a fresh access token whose audit subject matches
// the subject embedded in the refresh row. This is the C1.4 contract: a
// bridge identified as `bridge:slack` keeps that attribution across
// refresh rotation rather than collapsing to the bare fleet name.
func TestRefreshRoundTrip_PreservesSubject(t *testing.T) {
	ms := newMockStore()
	fc := NewFleetConfig("default", nil)
	mgr := NewRefreshTokenManager(ms, fc)

	const subject = "bridge:slack"
	issue, err := mgr.Issue(context.Background(), IssueRequest{
		Fleet:   "bridge-slack",
		Subject: subject,
	})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if issue.Row.Subject != subject {
		t.Fatalf("issued refresh row subject = %q, want %q", issue.Row.Subject, subject)
	}

	// First exchange (no rotation) — access token must carry subject.
	res, err := mgr.Exchange(context.Background(), issue.Plaintext, ExchangeOptions{})
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if got := fc.ResolveSubject(res.AccessToken); got != subject {
		t.Fatalf("access token subject = %q, want %q", got, subject)
	}
	if got := fc.ResolveFleet(res.AccessToken); got != "bridge-slack" {
		t.Fatalf("access token fleet = %q, want bridge-slack", got)
	}

	// Rotation — the new refresh row inherits Subject, and the access
	// token minted alongside it must carry the same subject claim.
	rot, err := mgr.Exchange(context.Background(), issue.Plaintext, ExchangeOptions{Rotate: true})
	if err != nil {
		t.Fatalf("Exchange (rotate): %v", err)
	}
	if rot.NewRefreshRow == nil || rot.NewRefreshRow.Subject != subject {
		t.Fatalf("rotated refresh row subject = %v, want %q", rot.NewRefreshRow, subject)
	}
	if got := fc.ResolveSubject(rot.AccessToken); got != subject {
		t.Fatalf("rotated access token subject = %q, want %q", got, subject)
	}

	// Exchange the new refresh once more — the subject must survive
	// across the rotation boundary (chained refresh).
	res2, err := mgr.Exchange(context.Background(), rot.NewRefreshToken, ExchangeOptions{})
	if err != nil {
		t.Fatalf("Exchange chained refresh: %v", err)
	}
	if got := fc.ResolveSubject(res2.AccessToken); got != subject {
		t.Fatalf("post-rotation access token subject = %q, want %q", got, subject)
	}
}
