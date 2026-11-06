package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func newTestRefreshManager(t *testing.T) (*RefreshTokenManager, *FleetConfig, *mockStore) {
	t.Helper()
	ms := newMockStore()
	fc := NewFleetConfig("default", nil)
	mgr := NewRefreshTokenManager(ms, fc)
	return mgr, fc, ms
}

func TestRefreshManager_IssueAndExchange(t *testing.T) {
	mgr, fc, _ := newTestRefreshManager(t)

	issue, err := mgr.Issue(context.Background(), IssueRequest{
		Fleet:        "qa",
		Project:      "alpha",
		Subject:      "agent:gd-AAA",
		AllowedTypes: []string{"task"},
		AllowedOps:   []string{"read"},
	})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if issue.Plaintext == "" {
		t.Fatal("expected non-empty plaintext")
	}
	if issue.Row.TokenHash == hashRefreshToken("") {
		t.Fatal("hash should not be empty-input hash")
	}
	if issue.Row.ExpiresAt.Before(time.Now()) {
		t.Fatal("default TTL should put expires_at in the future")
	}

	// Exchange (no rotation): returns access token, original refresh stays valid.
	res, err := mgr.Exchange(context.Background(), issue.Plaintext, ExchangeOptions{Rotate: false})
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if res.AccessToken == "" {
		t.Fatal("expected access token")
	}
	if res.NewRefreshToken != "" {
		t.Fatal("expected no rotated refresh when Rotate=false")
	}
	if got := fc.ResolveFleet(res.AccessToken); got != "qa" {
		t.Fatalf("access token fleet = %q, want qa", got)
	}
	if got := fc.ResolveProject(res.AccessToken); got != "alpha" {
		t.Fatalf("access token project = %q, want alpha", got)
	}
	if got := fc.ResolveTypeScope(res.AccessToken); len(got) != 1 || got[0] != "task" {
		t.Fatalf("access token type scope = %v, want [task]", got)
	}

	// Re-exchange the same refresh — also valid (stable refresh).
	if _, err := mgr.Exchange(context.Background(), issue.Plaintext, ExchangeOptions{}); err != nil {
		t.Fatalf("second Exchange: %v", err)
	}
}

// TestRefreshManager_PrivilegedSurvivesExchange asserts the privileged
// service-fleet flag (ka-0000001946) snapshotted on a refresh token is
// re-applied to every access token minted from it — across both a plain
// exchange and a rotation — so a privileged bridge keeps its ownership-bypass
// after its access token TTL expires and it refreshes.
func TestRefreshManager_PrivilegedSurvivesExchange(t *testing.T) {
	mgr, fc, _ := newTestRefreshManager(t)

	issue, err := mgr.Issue(context.Background(), IssueRequest{
		Fleet:      "gitlab-bridge",
		Subject:    "bridge:gitlab",
		Privileged: true,
	})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	// Plain exchange → access token carries the privilege.
	res, err := mgr.Exchange(context.Background(), issue.Plaintext, ExchangeOptions{Rotate: false})
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if !fc.ResolvePrivileged(res.AccessToken) {
		t.Fatal("access token from privileged refresh should be privileged")
	}

	// Rotation → the new refresh AND its access token keep the privilege.
	rot, err := mgr.Exchange(context.Background(), issue.Plaintext, ExchangeOptions{Rotate: true})
	if err != nil {
		t.Fatalf("Exchange (rotate): %v", err)
	}
	if !fc.ResolvePrivileged(rot.AccessToken) {
		t.Fatal("rotated access token should remain privileged")
	}
	res2, err := mgr.Exchange(context.Background(), rot.NewRefreshToken, ExchangeOptions{})
	if err != nil {
		t.Fatalf("Exchange (rotated refresh): %v", err)
	}
	if !fc.ResolvePrivileged(res2.AccessToken) {
		t.Fatal("access token from rotated privileged refresh should be privileged")
	}
}

// TestRefreshManager_OrdinaryNotPrivileged ensures the privilege bit is not
// granted by default — an ordinary refresh mints ordinary access tokens.
func TestRefreshManager_OrdinaryNotPrivileged(t *testing.T) {
	mgr, fc, _ := newTestRefreshManager(t)

	issue, err := mgr.Issue(context.Background(), IssueRequest{Fleet: "slack-bridge", Subject: "bridge:slack"})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	res, err := mgr.Exchange(context.Background(), issue.Plaintext, ExchangeOptions{})
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if fc.ResolvePrivileged(res.AccessToken) {
		t.Fatal("ordinary refresh must not mint a privileged access token")
	}
}

func TestRefreshManager_ExchangeRotation(t *testing.T) {
	mgr, fc, _ := newTestRefreshManager(t)

	issue, err := mgr.Issue(context.Background(), IssueRequest{Fleet: "qa"})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	res, err := mgr.Exchange(context.Background(), issue.Plaintext, ExchangeOptions{Rotate: true})
	if err != nil {
		t.Fatalf("Exchange (rotate): %v", err)
	}
	if res.NewRefreshToken == "" {
		t.Fatal("expected a rotated refresh token")
	}
	if res.NewRefreshRow == nil || res.NewRefreshRow.ParentRefreshHash != issue.Row.TokenHash {
		t.Fatalf("rotated refresh parent_refresh_hash = %v, want %s", res.NewRefreshRow, issue.Row.TokenHash)
	}

	// Original refresh is now revoked.
	if _, err := mgr.Exchange(context.Background(), issue.Plaintext, ExchangeOptions{}); err == nil {
		t.Fatal("expected error exchanging the rotated-out refresh")
	} else if err != ErrRefreshRevoked {
		t.Fatalf("got %v, want ErrRefreshRevoked", err)
	}

	// New refresh works.
	res2, err := mgr.Exchange(context.Background(), res.NewRefreshToken, ExchangeOptions{})
	if err != nil {
		t.Fatalf("Exchange new refresh: %v", err)
	}
	if got := fc.ResolveFleet(res2.AccessToken); got != "qa" {
		t.Fatalf("access fleet = %q, want qa", got)
	}
}

func TestRefreshManager_ExpiredRefresh(t *testing.T) {
	mgr, _, _ := newTestRefreshManager(t)

	issue, err := mgr.Issue(context.Background(), IssueRequest{
		Fleet: "qa",
		TTL:   1 * time.Hour,
	})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	// Move the clock past expiry.
	mgr.SetNowFunc(func() time.Time { return issue.Row.ExpiresAt.Add(time.Second) })

	if _, err := mgr.Exchange(context.Background(), issue.Plaintext, ExchangeOptions{}); err != ErrRefreshExpired {
		t.Fatalf("got %v, want ErrRefreshExpired", err)
	}
}

func TestRefreshManager_RevokeCascade(t *testing.T) {
	mgr, fc, _ := newTestRefreshManager(t)

	// Build a chain: r0 -> r1 -> r2 (each rotation revokes its predecessor).
	r0, err := mgr.Issue(context.Background(), IssueRequest{Fleet: "qa"})
	if err != nil {
		t.Fatalf("Issue r0: %v", err)
	}
	exch1, err := mgr.Exchange(context.Background(), r0.Plaintext, ExchangeOptions{Rotate: true})
	if err != nil {
		t.Fatalf("Exchange r0: %v", err)
	}
	r1Plain := exch1.NewRefreshToken
	r1Hash := exch1.NewRefreshRow.TokenHash

	exch2, err := mgr.Exchange(context.Background(), r1Plain, ExchangeOptions{Rotate: true})
	if err != nil {
		t.Fatalf("Exchange r1: %v", err)
	}
	r2Plain := exch2.NewRefreshToken

	// Sanity: r2 still works.
	res, err := mgr.Exchange(context.Background(), r2Plain, ExchangeOptions{})
	if err != nil {
		t.Fatalf("Exchange r2: %v", err)
	}
	accessFromR2 := res.AccessToken
	if !fc.HasToken(accessFromR2) {
		t.Fatal("access token should be in fleet config")
	}

	// Revoke at r1: must cascade to r2 (descendant of r1) but leave r0
	// alone (already revoked from rotation, not r1's descendant).
	n, err := mgr.RevokeByHash(context.Background(), r1Hash)
	if err != nil {
		t.Fatalf("RevokeByHash: %v", err)
	}
	if n < 1 {
		t.Fatalf("expected ≥1 newly-revoked rows, got %d", n)
	}

	// r2 must now be rejected.
	if _, err := mgr.Exchange(context.Background(), r2Plain, ExchangeOptions{}); err != ErrRefreshRevoked {
		t.Fatalf("after cascade, r2 exchange got %v, want ErrRefreshRevoked", err)
	}
	// access tokens minted under r1 should be removed from fleet config.
	if fc.HasToken(accessFromR2) {
		t.Fatal("access token should have been removed from fleet config")
	}
}

func TestRefreshManager_UnknownToken(t *testing.T) {
	mgr, _, _ := newTestRefreshManager(t)
	if _, err := mgr.Exchange(context.Background(), "deadbeef-not-a-real-token", ExchangeOptions{}); err != ErrRefreshNotFound {
		t.Fatalf("got %v, want ErrRefreshNotFound", err)
	}
}

// --- HTTP handler tests ---

func newTestServerWithRefresh(t *testing.T, rotate bool) (*BeadsServer, *FleetConfig, *RefreshTokenManager) {
	t.Helper()
	ms := newMockStore()
	srv := NewBeadsServer(ms, nil)
	fc := NewFleetConfig("default", nil)
	srv.SetFleetAuth(fc)
	mgr := NewRefreshTokenManager(ms, fc)
	srv.SetRefreshTokenManager(mgr)
	srv.SetRefreshRotation(rotate)
	return srv, fc, mgr
}

func TestHandleRefreshToken_Success(t *testing.T) {
	srv, _, mgr := newTestServerWithRefresh(t, false)

	issue, err := mgr.Issue(context.Background(), IssueRequest{Fleet: "qa"})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	body, _ := json.Marshal(refreshRequest{RefreshToken: issue.Plaintext})
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/refresh", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.handleRefreshToken(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var resp refreshResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode resp: %v", err)
	}
	if resp.AccessToken == "" {
		t.Fatal("expected access_token")
	}
	if resp.TokenType != "Bearer" {
		t.Fatalf("token_type = %q, want Bearer", resp.TokenType)
	}
	if resp.ExpiresIn <= 0 {
		t.Fatalf("expires_in = %d, want >0", resp.ExpiresIn)
	}
	if resp.RefreshToken != "" {
		t.Fatal("expected no rotated refresh when rotation disabled")
	}
}

func TestHandleRefreshToken_RotationOn(t *testing.T) {
	srv, _, mgr := newTestServerWithRefresh(t, true)

	issue, err := mgr.Issue(context.Background(), IssueRequest{Fleet: "qa"})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	body, _ := json.Marshal(refreshRequest{RefreshToken: issue.Plaintext})
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/refresh", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.handleRefreshToken(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var resp refreshResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode resp: %v", err)
	}
	if resp.RefreshToken == "" || !resp.RefreshTokenRotated {
		t.Fatalf("expected rotated refresh, got refresh_token=%q rotated=%v", resp.RefreshToken, resp.RefreshTokenRotated)
	}
}

func TestHandleRefreshToken_InvalidGrant(t *testing.T) {
	srv, _, _ := newTestServerWithRefresh(t, false)

	body, _ := json.Marshal(refreshRequest{RefreshToken: "this-is-not-a-real-refresh"})
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/refresh", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.handleRefreshToken(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); got == "" || !contains(got, "invalid_grant") {
		t.Fatalf("WWW-Authenticate = %q, want one with invalid_grant", got)
	}
}

func TestHandleRefreshToken_ExpiredHint(t *testing.T) {
	srv, _, mgr := newTestServerWithRefresh(t, false)

	issue, err := mgr.Issue(context.Background(), IssueRequest{Fleet: "qa", TTL: time.Hour})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	mgr.SetNowFunc(func() time.Time { return issue.Row.ExpiresAt.Add(time.Second) })

	body, _ := json.Marshal(refreshRequest{RefreshToken: issue.Plaintext})
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/refresh", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.handleRefreshToken(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); got == "" || !contains(got, "refresh token expired") {
		t.Fatalf("WWW-Authenticate = %q, want hint about refresh expiry", got)
	}
}

func TestHandleRefreshToken_MissingBody(t *testing.T) {
	srv, _, _ := newTestServerWithRefresh(t, false)

	body, _ := json.Marshal(refreshRequest{RefreshToken: ""})
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/refresh", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.handleRefreshToken(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

// TestHandleRegisterFleetToken_WithRefresh covers the opt-in
// with_refresh:true flag on POST /v1/fleet-tokens.
func TestHandleRegisterFleetToken_WithRefresh(t *testing.T) {
	srv, fc, _ := newTestServerWithRefresh(t, false)

	body, _ := json.Marshal(registerBridgeTokenRequest{
		Token:       "fleet-tok-qa-xyz",
		Fleet:       "qa",
		WithRefresh: true,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/fleet-tokens", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin-secret")
	req = asTokenMinter(req)
	rec := httptest.NewRecorder()

	srv.handleRegisterBridgeToken(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d; body: %s", rec.Code, rec.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode resp: %v", err)
	}
	rt, ok := resp["refresh_token"].(string)
	if !ok || rt == "" {
		t.Fatalf("expected refresh_token in response, got %#v", resp["refresh_token"])
	}
	if got := fc.ResolveFleet("fleet-tok-qa-xyz"); got != "qa" {
		t.Fatalf("expected access token registered, got fleet %q", got)
	}
	// And the refresh works for an exchange.
	body, _ = json.Marshal(refreshRequest{RefreshToken: rt})
	req2 := httptest.NewRequest(http.MethodPost, "/v1/auth/refresh", bytes.NewReader(body))
	rec2 := httptest.NewRecorder()
	srv.handleRefreshToken(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("refresh exchange failed: %d %s", rec2.Code, rec2.Body.String())
	}
}
