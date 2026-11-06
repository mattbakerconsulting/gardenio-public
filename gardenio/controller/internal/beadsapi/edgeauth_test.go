package beadsapi

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestSendOnce_StampsEdgeAuthEnvelope verifies that when an HMAC key is
// configured AND the context carries WithEdgeAuthOperator, the client
// stamps a valid edge-auth envelope (X-Operator + X-Edge-Auth-Sig +
// X-Edge-Auth-Ts + X-Auth-Audit-Id) and SKIPS X-Operated-By. This is
// the slack-bridge close-from-Slack path (ka-0000007637).
func TestSendOnce_StampsEdgeAuthEnvelope(t *testing.T) {
	key := []byte("test-shared-key-32-bytes-padding!")
	wantOp := "slack:U0OPERATOR1"

	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c, err := New(Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.SetEdgeAuthHMACKey(key)

	ctx := WithEdgeAuthOperator(context.Background(), wantOp)
	if _, _, _, _, _, err := c.sendOnce(ctx, http.MethodPost, "/v1/beads/gd-x/close", []byte(`{}`), ""); err != nil {
		t.Fatalf("sendOnce: %v", err)
	}

	if got.Header.Get("X-Operator") != wantOp {
		t.Errorf("X-Operator = %q, want %q", got.Header.Get("X-Operator"), wantOp)
	}
	if got.Header.Get("X-Operated-By") != "" {
		t.Errorf("X-Operated-By must be omitted on edge-auth path, got %q", got.Header.Get("X-Operated-By"))
	}

	auditID := got.Header.Get("X-Auth-Audit-Id")
	if auditID == "" {
		t.Fatal("X-Auth-Audit-Id missing")
	}
	ts := got.Header.Get("X-Edge-Auth-Ts")
	if ts == "" {
		t.Fatal("X-Edge-Auth-Ts missing")
	}
	if _, err := strconv.ParseInt(ts, 10, 64); err != nil {
		t.Errorf("X-Edge-Auth-Ts %q is not unix seconds: %v", ts, err)
	}
	if abs := time.Now().Unix() - parseInt(ts); abs > 5 || abs < -5 {
		t.Errorf("X-Edge-Auth-Ts %q skewed from now by %ds", ts, abs)
	}

	sig := got.Header.Get("X-Edge-Auth-Sig")
	if sig == "" {
		t.Fatal("X-Edge-Auth-Sig missing")
	}

	// Verify the signature.
	m := hmac.New(sha256.New, key)
	_, _ = m.Write([]byte(wantOp + "\n" + auditID + "\n" + "/v1/beads/gd-x/close" + "\n" + ts))
	want := base64.StdEncoding.EncodeToString(m.Sum(nil))
	if !hmac.Equal([]byte(sig), []byte(want)) {
		t.Errorf("signature mismatch: got %q, want %q", sig, want)
	}
}

// TestSendOnce_NoHMACKeyFallsBack verifies that when ctx carries an edge-
// auth operator but no key is configured, the client falls back to the
// legacy X-Operated-By header. This is the pre-rollout safe state — the
// envelope is not stamped (would never verify on the daemon side anyway).
func TestSendOnce_NoHMACKeyFallsBack(t *testing.T) {
	wantOp := "slack:U0OPERATOR1"

	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c, err := New(Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// No HMAC key set.

	ctx := WithEdgeAuthOperator(context.Background(), wantOp)
	ctx = WithOperator(ctx, wantOp) // legacy stamp the bridge already uses
	if _, _, _, _, _, err := c.sendOnce(ctx, http.MethodPost, "/v1/beads/gd-x/close", []byte(`{}`), ""); err != nil {
		t.Fatalf("sendOnce: %v", err)
	}

	if got.Header.Get("X-Operator") != "" {
		t.Errorf("X-Operator must be empty without HMAC key, got %q", got.Header.Get("X-Operator"))
	}
	if got.Header.Get("X-Edge-Auth-Sig") != "" {
		t.Errorf("X-Edge-Auth-Sig must be empty without HMAC key, got %q", got.Header.Get("X-Edge-Auth-Sig"))
	}
	if got.Header.Get("X-Operated-By") != wantOp {
		t.Errorf("X-Operated-By fallback = %q, want %q", got.Header.Get("X-Operated-By"), wantOp)
	}
}

func parseInt(s string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return n
}

// TestSignEdgeAuthV2_CrossLanguageKnownVector pins the v2 wire format to the
// same vector the daemon (kbeads/internal/edgeauth) and cmd/edge-auth test, so
// the three implementations cannot drift apart.
func TestSignEdgeAuthV2_CrossLanguageKnownVector(t *testing.T) {
	got := signEdgeAuthV2([]byte("shared-secret"), "operator@acme.example", "entra",
		"gardenio-operator,gardenio-viewer", "audit-123", http.MethodPost, "/v1/beads", "1700000000")
	const want = "KNAEp9aRG8u2NQXkBgNsyD/R3X7l6RdnkI+CFL7mkBY="
	if got != want {
		t.Fatalf("v2 wire vector = %q, want %q", got, want)
	}
}

// TestSendOnce_StampsV2IdentityEnvelope verifies WithEdgeAuthIdentity signs a
// role-bearing v2 envelope bound to the HTTP method and path, with roles sorted
// into the daemon's canonical form, and omits X-Operated-By.
func TestSendOnce_StampsV2IdentityEnvelope(t *testing.T) {
	key := []byte("test-shared-key-32-bytes-padding!")
	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c, err := New(Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.SetEdgeAuthHMACKey(key)

	ctx := WithEdgeAuthIdentity(context.Background(), EdgeAuthIdentity{
		Operator: "oidc:owner@example.com",
		AuthMode: "slack",
		Roles:    []string{"gardenio-viewer", "gardenio-admin"}, // deliberately unsorted
	})
	if _, _, _, _, _, err := c.sendOnce(ctx, http.MethodPatch, "/v1/beads/gd-x", []byte(`{}`), ""); err != nil {
		t.Fatalf("sendOnce: %v", err)
	}

	for header, want := range map[string]string{
		"X-Operator":          "oidc:owner@example.com",
		"X-Edge-Auth-Version": "2",
		"X-Edge-Auth-Mode":    "slack",
		"X-Edge-Auth-Roles":   "gardenio-admin,gardenio-viewer",
	} {
		if g := got.Header.Get(header); g != want {
			t.Errorf("%s = %q, want %q", header, g, want)
		}
	}
	if got.Header.Get("X-Operated-By") != "" {
		t.Errorf("X-Operated-By must be omitted, got %q", got.Header.Get("X-Operated-By"))
	}
	want := signEdgeAuthV2(key, "oidc:owner@example.com", "slack", "gardenio-admin,gardenio-viewer",
		got.Header.Get("X-Auth-Audit-Id"), http.MethodPatch, "/v1/beads/gd-x", got.Header.Get("X-Edge-Auth-Ts"))
	if g := got.Header.Get("X-Edge-Auth-Sig-V2"); g != want {
		t.Errorf("X-Edge-Auth-Sig-V2 = %q, want %q", g, want)
	}
}

// TestSendOnce_V2IdentityFailsClosed: a requested role-bearing identity must
// never degrade to the fleet token + X-Operated-By when it cannot be signed.
func TestSendOnce_V2IdentityFailsClosed(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c, err := New(Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	id := EdgeAuthIdentity{Operator: "oidc:owner@example.com", AuthMode: "slack", Roles: []string{"gardenio-admin"}}

	// No HMAC key.
	if _, _, _, _, _, err := c.sendOnce(WithEdgeAuthIdentity(context.Background(), id), http.MethodGet, "/v1/x", nil, ""); err == nil {
		t.Error("expected error with no HMAC key")
	}
	// Key present but no roles.
	c.SetEdgeAuthHMACKey([]byte("k"))
	id.Roles = nil
	if _, _, _, _, _, err := c.sendOnce(WithEdgeAuthIdentity(context.Background(), id), http.MethodGet, "/v1/x", nil, ""); err == nil {
		t.Error("expected error with no roles")
	}
	if hit {
		t.Error("request reached the daemon despite fail-closed identity")
	}
}

// TestSendOnce_NoEnvelopeOverUserBearer: a request that carries a per-request
// user bearer is never signed (gd-dMof1PW1oJ). The daemon takes an envelope
// over the bearer when X-Operator is present, so a v1 envelope there would
// demote the human's own OIDC token to a role-less edge identity.
func TestSendOnce_NoEnvelopeOverUserBearer(t *testing.T) {
	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c, err := New(Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.SetEdgeAuthHMACKey([]byte("test-shared-key-32-bytes-padding!"))
	c.SetEdgeAuthHMACKeyV2([]byte("test-v2-key-32-bytes-padding!!!!"))

	for name, ctx := range map[string]context.Context{
		"v1 operator": WithEdgeAuthOperator(context.Background(), "oidc:owner@example.com"),
		"v2 identity": WithEdgeAuthIdentity(context.Background(), EdgeAuthIdentity{
			Operator: "oidc:owner@example.com", AuthMode: "slack", Roles: []string{"gardenio-admin"},
		}),
	} {
		if _, _, _, _, _, err := c.sendOnce(ctx, http.MethodPost, "/v1/escalations/gd-e/approve", []byte(`{}`), "user-jwt"); err != nil {
			t.Fatalf("%s: sendOnce: %v", name, err)
		}
		if g := got.Header.Get("Authorization"); g != "Bearer user-jwt" {
			t.Errorf("%s: Authorization = %q, want the user bearer", name, g)
		}
		for _, h := range []string{"X-Operator", "X-Edge-Auth-Sig", "X-Edge-Auth-Sig-V2", "X-Edge-Auth-Version"} {
			if g := got.Header.Get(h); g != "" {
				t.Errorf("%s: %s = %q, want no envelope over a user bearer", name, h, g)
			}
		}
	}

	// Control: the same v1 operator without a user bearer is still signed.
	if _, _, _, _, _, err := c.sendOnce(WithEdgeAuthOperator(context.Background(), "oidc:owner@example.com"),
		http.MethodPost, "/v1/escalations/gd-e/approve", []byte(`{}`), ""); err != nil {
		t.Fatalf("sendOnce: %v", err)
	}
	if got.Header.Get("X-Edge-Auth-Sig") == "" {
		t.Error("no user bearer: the v1 envelope was not signed")
	}
}

// TestSendOnce_V2EnvelopeUsesV2Key: once SetEdgeAuthHMACKeyV2 is set, a v2
// envelope is signed with it and a v1 envelope keeps the shared key, so a
// holder of the v1 key alone cannot mint a v2 envelope (gd-dMof1PW1oJ).
func TestSendOnce_V2EnvelopeUsesV2Key(t *testing.T) {
	v1Key, v2Key := []byte("v1-key"), []byte("v2-key")
	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c, err := New(Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.SetEdgeAuthHMACKey(v1Key)
	c.SetEdgeAuthHMACKeyV2(v2Key)

	ctx := WithEdgeAuthIdentity(context.Background(), EdgeAuthIdentity{
		Operator: "oidc:owner@example.com", AuthMode: "slack", Roles: []string{"gardenio-admin"},
	})
	if _, _, _, _, _, err := c.sendOnce(ctx, http.MethodPatch, "/v1/beads/gd-x", []byte(`{}`), ""); err != nil {
		t.Fatalf("sendOnce: %v", err)
	}
	sig := func(key []byte) string {
		return signEdgeAuthV2(key, "oidc:owner@example.com", "slack", "gardenio-admin",
			got.Header.Get("X-Auth-Audit-Id"), http.MethodPatch, "/v1/beads/gd-x", got.Header.Get("X-Edge-Auth-Ts"))
	}
	if g := got.Header.Get("X-Edge-Auth-Sig-V2"); g != sig(v2Key) || g == sig(v1Key) {
		t.Errorf("X-Edge-Auth-Sig-V2 = %q, want it signed with the v2 key", g)
	}

	if _, _, _, _, _, err := c.sendOnce(WithEdgeAuthOperator(context.Background(), "slack:U1"),
		http.MethodPost, "/v1/beads/gd-x/close", []byte(`{}`), ""); err != nil {
		t.Fatalf("sendOnce v1: %v", err)
	}
	want := signEdgeAuth(v1Key, "slack:U1", got.Header.Get("X-Auth-Audit-Id"), "/v1/beads/gd-x/close", got.Header.Get("X-Edge-Auth-Ts"))
	if g := got.Header.Get("X-Edge-Auth-Sig"); g != want {
		t.Errorf("v1 X-Edge-Auth-Sig = %q, want it signed with the shared key", g)
	}
}
