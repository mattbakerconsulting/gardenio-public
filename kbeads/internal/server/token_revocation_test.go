package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"gardenio/kbeads/internal/model"
	dto "github.com/prometheus/client_model/go"
)

func hashHex(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func TestTokenRevocationStore_SeedAndLookup(t *testing.T) {
	ms := newMockStore()
	rs := NewTokenRevocationStore(ms)
	rs.Seed([]string{hashHex("tok-a"), hashHex("tok-b"), ""}) // empty skipped

	if !rs.IsRevokedToken("tok-a") {
		t.Fatal("tok-a should be revoked via seed")
	}
	if !rs.IsRevokedToken("tok-b") {
		t.Fatal("tok-b should be revoked via seed")
	}
	if rs.IsRevokedToken("tok-c") {
		t.Fatal("tok-c should not be revoked")
	}
	if rs.IsRevokedToken("") {
		t.Fatal("empty token must not report revoked")
	}
	if n := rs.Size(); n != 2 {
		t.Fatalf("size=%d, want 2", n)
	}
}

func TestTokenRevocationStore_RevokeHashPersistsAndCaches(t *testing.T) {
	ms := newMockStore()
	rs := NewTokenRevocationStore(ms)

	inserted, err := rs.RevokeToken(context.Background(), "secret-token", "admin", "leaked")
	if err != nil {
		t.Fatalf("RevokeToken err: %v", err)
	}
	if !inserted {
		t.Fatal("expected first revocation to report inserted=true")
	}
	// Second call is idempotent.
	inserted, err = rs.RevokeToken(context.Background(), "secret-token", "admin", "leaked")
	if err != nil {
		t.Fatalf("RevokeToken repeat err: %v", err)
	}
	if inserted {
		t.Fatal("expected second revocation to report inserted=false")
	}

	// Persisted in mock store.
	hashes, err := ms.ListRevokedTokenHashes(context.Background())
	if err != nil {
		t.Fatalf("ListRevokedTokenHashes err: %v", err)
	}
	if len(hashes) != 1 || hashes[0] != hashHex("secret-token") {
		t.Fatalf("persisted hashes = %v, want [%s]", hashes, hashHex("secret-token"))
	}

	if !rs.IsRevokedToken("secret-token") {
		t.Fatal("secret-token should be revoked after RevokeToken")
	}
}

// TestTokenRevocationStore_LoadRestoresFromDB exercises the startup path:
// rows in the DB must be loaded into the in-process cache so a daemon
// restart does not resurrect a previously-revoked token.
func TestTokenRevocationStore_LoadRestoresFromDB(t *testing.T) {
	ms := newMockStore()

	// Simulate daemon #1: revoke a token, which persists the row.
	rs1 := NewTokenRevocationStore(ms)
	if _, err := rs1.RevokeToken(context.Background(), "old-token", "admin", "key-rotation"); err != nil {
		t.Fatalf("RevokeToken err: %v", err)
	}

	// Simulate daemon #2 startup: fresh cache, then Load() from the same
	// backing store. The previously-revoked token must still be rejected.
	rs2 := NewTokenRevocationStore(ms)
	if rs2.IsRevokedToken("old-token") {
		t.Fatal("fresh cache should not know about old-token before Load")
	}
	if err := rs2.Load(context.Background()); err != nil {
		t.Fatalf("Load err: %v", err)
	}
	if !rs2.IsRevokedToken("old-token") {
		t.Fatal("Load did not restore persisted revocation — restart would resurrect token")
	}
	if n := rs2.Size(); n != 1 {
		t.Fatalf("size after Load = %d, want 1", n)
	}
}

func TestTokenRevocationStore_OnRevokeCallback(t *testing.T) {
	ms := newMockStore()
	rs := NewTokenRevocationStore(ms)
	var calls int
	rs.SetOnRevoke(func(revokedBy, reason string) {
		calls++
		if revokedBy != "admin" || reason != "r" {
			t.Errorf("callback args: got (%q,%q)", revokedBy, reason)
		}
	})
	if _, err := rs.RevokeToken(context.Background(), "t", "admin", "r"); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	// Re-revoke must NOT fire the callback (dedupe).
	if _, err := rs.RevokeToken(context.Background(), "t", "admin", "r"); err != nil {
		t.Fatalf("RevokeToken repeat: %v", err)
	}
	if calls != 1 {
		t.Fatalf("onRevoke fired %d times, want 1", calls)
	}
}

func TestHandleRevokeToken_AdminAcceptsHash(t *testing.T) {
	ms := newMockStore()
	srv := NewBeadsServer(ms, nil)
	srv.SetFleetAuth(NewFleetConfig("default", nil))

	before := testGetCounter(t, tokenRevocationsTotal)

	body, _ := json.Marshal(revokeTokenRequest{
		TokenHash: hashHex("leaked-token"),
		Reason:    "exposed in screenshot",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/revoke-token", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin-secret")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.handleRevokeToken(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	var resp revokeTokenResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.AlreadyRevoked {
		t.Fatal("first revocation reported already_revoked=true")
	}
	if !resp.Revoked {
		t.Fatal("revoked=false")
	}

	// Middleware should now reject this token.
	if !srv.revocation.IsRevokedToken("leaked-token") {
		t.Fatal("store did not mark token as revoked")
	}

	// Persisted row.
	hashes, _ := ms.ListRevokedTokenHashes(context.Background())
	if len(hashes) != 1 {
		t.Fatalf("persisted hashes = %v", hashes)
	}

	// Counter bumped exactly once.
	after := testGetCounter(t, tokenRevocationsTotal)
	if after-before != 1 {
		t.Fatalf("counter delta = %v, want 1", after-before)
	}

	// Audit bead recorded.
	var auditFound bool
	for _, b := range ms.beads {
		if b.Type != "audit" {
			continue
		}
		var f map[string]any
		_ = json.Unmarshal(b.Fields, &f)
		if f["action"] == "revoke_token" {
			auditFound = true
			break
		}
	}
	if !auditFound {
		t.Fatal("no audit bead with action=revoke_token was created")
	}
}

func TestHandleRevokeToken_AcceptsPlaintextToken(t *testing.T) {
	ms := newMockStore()
	srv := NewBeadsServer(ms, nil)
	srv.SetFleetAuth(NewFleetConfig("default", nil))

	body, _ := json.Marshal(revokeTokenRequest{
		Token:  "some-raw-token",
		Reason: "bad-day",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/revoke-token", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin-secret")
	rec := httptest.NewRecorder()

	srv.handleRevokeToken(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !srv.revocation.IsRevokedToken("some-raw-token") {
		t.Fatal("plaintext token not marked revoked")
	}
}

func TestHandleRevokeToken_RejectsBadHash(t *testing.T) {
	ms := newMockStore()
	srv := NewBeadsServer(ms, nil)
	srv.SetFleetAuth(NewFleetConfig("default", nil))

	cases := []struct{ name, body string }{
		{"empty", `{}`},
		{"short", `{"token_hash":"deadbeef"}`},
		{"uppercase", `{"token_hash":"DEADBEEFDEADBEEFDEADBEEFDEADBEEFDEADBEEFDEADBEEFDEADBEEFDEADBEEF"}`},
		{"non-hex", `{"token_hash":"z0000000000000000000000000000000000000000000000000000000000000z"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/admin/revoke-token", bytes.NewReader([]byte(tc.body)))
			req.Header.Set("Authorization", "Bearer admin-secret")
			rec := httptest.NewRecorder()
			srv.handleRevokeToken(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("%s: status=%d body=%s", tc.name, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestTokenRevocationMiddleware_RejectsRevokedToken(t *testing.T) {
	ms := newMockStore()
	rs := NewTokenRevocationStore(ms)
	rs.Seed([]string{hashHex("bad-token")})

	nextCalled := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusOK)
	})
	mw := TokenRevocationMiddleware(rs, next)

	// Revoked token → 401, next not called.
	req := httptest.NewRequest(http.MethodGet, "/v1/beads", nil)
	req.Header.Set("Authorization", "Bearer bad-token")
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked token: status=%d", rec.Code)
	}
	if nextCalled {
		t.Fatal("revoked token reached next handler")
	}

	// Health check is exempt even with a revoked token.
	nextCalled = false
	req = httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	req.Header.Set("Authorization", "Bearer bad-token")
	rec = httptest.NewRecorder()
	mw.ServeHTTP(rec, req)
	if !nextCalled {
		t.Fatal("health check should bypass revocation middleware")
	}

	// Good token passes through.
	nextCalled = false
	req = httptest.NewRequest(http.MethodGet, "/v1/beads", nil)
	req.Header.Set("Authorization", "Bearer good-token")
	rec = httptest.NewRecorder()
	mw.ServeHTTP(rec, req)
	if !nextCalled {
		t.Fatal("good token was rejected")
	}
}

func TestTokenRevocationMiddleware_NilStoreIsNoop(t *testing.T) {
	nextCalled := false
	mw := TokenRevocationMiddleware(nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
	}))
	req := httptest.NewRequest(http.MethodGet, "/v1/beads", nil)
	req.Header.Set("Authorization", "Bearer anything")
	mw.ServeHTTP(httptest.NewRecorder(), req)
	if !nextCalled {
		t.Fatal("nil store middleware should pass through")
	}
}

// testGetCounter reads a prometheus.Counter's current value. Small test
// helper to avoid pulling in the full testutil package for one metric.
func testGetCounter(t *testing.T, c interface{ Write(*dto.Metric) error }) float64 {
	t.Helper()
	m := &dto.Metric{}
	if err := c.Write(m); err != nil {
		t.Fatalf("counter write: %v", err)
	}
	if m.Counter == nil {
		return 0
	}
	return *m.Counter.Value
}

var _ = model.StatusOpen
