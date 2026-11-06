package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// authStatus runs a bearer token through AuthMiddleware and returns the status.
func authStatus(t *testing.T, fc *FleetConfig, token string) int {
	t.Helper()
	h := AuthMiddleware(withTestToken(fc, "admin-secret"), nil, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/v1/beads", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func revokeFleetHTTP(t *testing.T, srv *BeadsServer, fleet string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, "/v1/bridge-tokens/"+fleet, nil)
	req.SetPathValue("bridge", fleet)
	req.Header.Set("Authorization", "Bearer admin-secret")
	req = asTokenMinter(req)
	rec := httptest.NewRecorder()
	srv.handleRevokeBridgeToken(rec, req)
	return rec.Code
}

func TestFleetConfig_RevokeBridgeToken_RemovesEveryToken(t *testing.T) {
	fc := NewFleetConfig("gardenio", map[string]string{
		"tok-a1": "agent-a",
		"tok-a2": "agent-a",
		"tok-b":  "agent-b",
	})
	removed := fc.RevokeBridgeToken("agent-a")
	if len(removed) != 2 {
		t.Fatalf("removed %v, want both agent-a tokens", removed)
	}
	if fc.HasToken("tok-a1") || fc.HasToken("tok-a2") {
		t.Fatal("an agent-a token survived the revoke")
	}
	if !fc.HasToken("tok-b") {
		t.Fatal("agent-b token must be untouched")
	}
}

// A fleet re-minted (pod recreated) holds two live tokens; revoking the fleet
// must make both return 401, not whichever map iteration visits second.
func TestHandleRevokeBridgeToken_TwoTokensBoth401(t *testing.T) {
	fc := NewFleetConfig("gardenio", map[string]string{
		"tok-a1": "agent-a",
		"tok-a2": "agent-a",
		"tok-b":  "agent-b",
	})
	srv, _ := newTestServerWithFleetAuth(t, "admin-secret", fc)

	for _, tok := range []string{"tok-a1", "tok-a2"} {
		if got := authStatus(t, fc, tok); got != http.StatusOK {
			t.Fatalf("%s before revoke: %d, want 200", tok, got)
		}
	}
	if got := revokeFleetHTTP(t, srv, "agent-a"); got != http.StatusNoContent {
		t.Fatalf("revoke: %d, want 204", got)
	}
	for _, tok := range []string{"tok-a1", "tok-a2"} {
		if got := authStatus(t, fc, tok); got != http.StatusUnauthorized {
			t.Fatalf("%s after revoke: %d, want 401", tok, got)
		}
	}
	if got := authStatus(t, fc, "tok-b"); got != http.StatusOK {
		t.Fatalf("other fleet's token: %d, want 200", got)
	}
}

// Revoking a fleet kills its refresh tokens and the access tokens minted from
// them, so a copied refresh token cannot mint a replacement.
func TestHandleRevokeBridgeToken_RevokesRefreshTokens(t *testing.T) {
	srv, fc, mgr := newTestServerWithRefresh(t, true)
	ctx := context.Background()

	r1, err := mgr.Issue(ctx, IssueRequest{Fleet: "agent-a"})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	r2, err := mgr.Issue(ctx, IssueRequest{Fleet: "agent-a"})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	other, err := mgr.Issue(ctx, IssueRequest{Fleet: "agent-b"})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	// r1 was already exchanged with rotation: the live refresh is its child, and
	// the minted access token must go too.
	ex, err := mgr.Exchange(ctx, r1.Plaintext, ExchangeOptions{Rotate: true})
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if got := authStatus(t, fc, ex.AccessToken); got != http.StatusOK {
		t.Fatalf("minted access token: %d, want 200", got)
	}

	if got := revokeFleetHTTP(t, srv, "agent-a"); got != http.StatusNoContent {
		t.Fatalf("revoke: %d, want 204 (fleet has refresh tokens even though no registered access token)", got)
	}

	for name, pt := range map[string]string{"issued": r2.Plaintext, "rotated child": ex.NewRefreshToken} {
		if _, err := mgr.Exchange(ctx, pt, ExchangeOptions{}); err == nil {
			t.Fatalf("%s refresh token still exchanges after fleet revoke", name)
		}
	}
	if got := authStatus(t, fc, ex.AccessToken); got != http.StatusUnauthorized {
		t.Fatalf("access token minted from revoked refresh: %d, want 401", got)
	}
	if _, err := mgr.Exchange(ctx, other.Plaintext, ExchangeOptions{}); err != nil {
		t.Fatalf("other fleet's refresh token must survive: %v", err)
	}
	if got := revokeFleetHTTP(t, srv, "agent-a"); got != http.StatusNotFound {
		t.Fatalf("second revoke: %d, want 404", got)
	}
}

// A failed registration must undo exactly the token it added, not a live token
// the fleet already holds.
func TestHandleRegisterBridgeToken_RollbackKeepsExistingToken(t *testing.T) {
	srv, fc, _ := newTestServerWithRefresh(t, false)
	fc.RegisterToken("tok-existing", "qa-fleet")

	body, _ := json.Marshal(registerBridgeTokenRequest{
		Token:       "tok-new",
		Fleet:       "qa-fleet",
		WithRefresh: true,
		RefreshTTL:  "not-a-duration",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/bridge-tokens", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin-secret")
	req = asTokenMinter(req)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.handleRegisterBridgeToken(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("register: %d, want 400; body: %s", rec.Code, rec.Body.String())
	}
	if fc.HasToken("tok-new") {
		t.Fatal("rolled-back token must be removed")
	}
	if !fc.HasToken("tok-existing") {
		t.Fatal("rollback removed a pre-existing token of the same fleet")
	}
}
