package oidcauth

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// flakyIssuer is an httptest server that serves the OIDC discovery document at
// /.well-known/openid-configuration, but returns HTTP 503 for the first
// failFirst requests to simulate a briefly-unreachable issuer at startup. It
// lets us exercise the background-retry + eventual-ready path (ka-0000001839)
// without a real Keycloak. JWKS is not served because Ready() only depends on
// successful discovery (token verification fetches keys lazily, later).
type flakyIssuer struct {
	srv       *httptest.Server
	url       string
	calls     atomic.Int32
	failFirst int32
}

func newFlakyIssuer(t *testing.T, failFirst int32) *flakyIssuer {
	t.Helper()
	fi := &flakyIssuer{failFirst: failFirst}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		if fi.calls.Add(1) <= fi.failFirst {
			http.Error(w, "issuer temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// Minimal valid discovery doc. issuer MUST equal the request URL or
		// oidc.NewProvider rejects it.
		fmt.Fprintf(w, `{
			"issuer": %q,
			"authorization_endpoint": %q,
			"token_endpoint": %q,
			"jwks_uri": %q,
			"id_token_signing_alg_values_supported": ["RS256"]
		}`, fi.url, fi.url+"/auth", fi.url+"/token", fi.url+"/keys")
	})
	srv := httptest.NewServer(mux)
	fi.srv = srv
	fi.url = srv.URL
	t.Cleanup(srv.Close)
	return fi
}

// waitReady blocks until the handle is fully serviceable or the deadline
// elapses. Provider discovery now runs in a background goroutine
// (ka-0000001839), so tests that exercise the Keycloak path must wait for the
// fail-closed->verifying swap rather than asserting readiness synchronously
// right after New.
//
// It gates on BOTH Ready() and oidcDiscovered(). Ready() alone is insufficient
// for a dual AVA+OIDC handle: AVA makes Ready() true synchronously in New,
// BEFORE the background OIDC discovery completes, so a test that returned on
// Ready() would race the Keycloak branch and see a transient 503
// ("authentication provider unavailable") from the still-fail-closed OIDC path
// (ka-0000003670). oidcDiscovered() is true immediately when Keycloak is
// disabled, so this stays correct for AVA-only and passthrough handles too.
func waitReady(t *testing.T, h *Handle) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if h.Ready() && h.oidcDiscovered() && h.saDiscovered() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("handle not fully ready after 5s (Ready()=%v, oidcDiscovered()=%v, saDiscovered()=%v); background discovery did not complete", h.Ready(), h.oidcDiscovered(), h.saDiscovered())
}

func TestMiddleware_EmptyIssuer_Passthrough(t *testing.T) {
	cfg := Config{IssuerURL: ""}
	mw := Middleware(context.Background(), cfg, slog.Default())

	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 OK when issuer is empty, got %d", rec.Code)
	}
}

func TestMiddleware_ProviderInitFails_FailsClosed(t *testing.T) {
	// F1 regression guard (ka-0000006185 / ka-0000004369): when OIDC provider
	// discovery fails (e.g. Keycloak down at startup), the middleware MUST
	// reject every request with 503 rather than passthrough.
	origAttempts, origDelay := providerRetryAttempts, providerRetryInitialDelay
	providerRetryAttempts = 1
	providerRetryInitialDelay = 0
	t.Cleanup(func() {
		providerRetryAttempts = origAttempts
		providerRetryInitialDelay = origDelay
	})

	// Point at a TCP endpoint that will refuse connections so discovery fails
	// quickly and deterministically.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}

	cfg := Config{IssuerURL: "http://" + addr, ClientID: "test"}
	mw := Middleware(context.Background(), cfg, slog.Default())

	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("inner handler was invoked — middleware passed through despite failed provider init")
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 when OIDC provider init failed, got %d", rec.Code)
	}
	if rec.Code == http.StatusOK {
		t.Fatal("middleware returned 200 passthrough — fail-closed contract broken (see ka-0000006185)")
	}
}

func TestNew_EmptyIssuer_ReadyTrue(t *testing.T) {
	h := New(context.Background(), Config{IssuerURL: ""}, slog.Default())
	if !h.Ready() {
		t.Error("Ready() = false for disabled OIDC; expected true so /readyz passes")
	}
}

func TestNew_ProviderInitFails_ReadyFalse(t *testing.T) {
	// F1 regression guard (ka-0000004618): when OIDC provider init fails,
	// Ready() MUST return false so the caller's /readyz probe fails and K8s
	// stops routing traffic to this pod. Pair with the 503-middleware
	// behavior in TestMiddleware_ProviderInitFails_FailsClosed.
	origAttempts, origDelay := providerRetryAttempts, providerRetryInitialDelay
	providerRetryAttempts = 1
	providerRetryInitialDelay = 0
	t.Cleanup(func() {
		providerRetryAttempts = origAttempts
		providerRetryInitialDelay = origDelay
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}

	h := New(context.Background(), Config{IssuerURL: "http://" + addr, ClientID: "test"}, slog.Default())
	if h.Ready() {
		t.Fatal("Ready() = true after provider init failed — readiness gate broken, K8s would route traffic to a fail-closed pod")
	}
}

func TestNew_DualAVAAndOIDC_ReadyWhileOIDCFailsClosed(t *testing.T) {
	origAttempts, origDelay := providerRetryAttempts, providerRetryInitialDelay
	providerRetryAttempts = 1
	providerRetryInitialDelay = 0
	t.Cleanup(func() {
		providerRetryAttempts = origAttempts
		providerRetryInitialDelay = origDelay
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}

	h := New(context.Background(), Config{
		IssuerURL: "http://" + addr,
		ClientID:  "test",
		AVA: AVAConfig{
			TrustSigner: "arn:aws:ec2:us-east-1:123456789012:verified-access-instance/vai-test",
			Region:      "us-east-1",
		},
	}, slog.Default())
	if !h.Ready() {
		t.Fatal("Ready() = false for dual AVA+OIDC; the synchronous AVA branch must keep the shared pod ready")
	}

	rec := httptest.NewRecorder()
	h.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("OIDC request reached inner handler while discovery was unavailable")
	})).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("OIDC branch status = %d, want 503 while discovery is unavailable", rec.Code)
	}
}

func TestNewProviderWithRetry_RetriesAndBacksOff(t *testing.T) {
	// Verify the retry loop actually calls oidc.NewProvider multiple times
	// before giving up. With attempts=3 and near-zero initial delay this
	// should finish in well under a second.
	origAttempts, origDelay, origMax := providerRetryAttempts, providerRetryInitialDelay, providerRetryMaxDelay
	providerRetryAttempts = 3
	providerRetryInitialDelay = 1 * time.Millisecond
	providerRetryMaxDelay = 2 * time.Millisecond
	t.Cleanup(func() {
		providerRetryAttempts = origAttempts
		providerRetryInitialDelay = origDelay
		providerRetryMaxDelay = origMax
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}

	start := time.Now()
	_, err = newProviderWithRetry(context.Background(), "http://"+addr, currentRetryPolicy(), slog.Default())
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected error after all retries exhausted, got nil")
	}
	// With 3 attempts and 1ms/2ms delays, minimum wait is ~3ms. Just assert
	// the loop doesn't short-circuit on attempt 1 (which would take <1ms).
	if elapsed < 2*time.Millisecond {
		t.Errorf("retry loop finished in %v — suggests only 1 attempt was made", elapsed)
	}
}

// TestNew_BackgroundDiscovery_EventuallyReady is the core ka-0000001839
// acceptance: discovery is OFF the synchronous critical path. New returns
// immediately fail-closed (Ready()==false, 503), and once the issuer becomes
// reachable the background goroutine flips Ready()==true and the middleware
// stops returning 503 — all WITHOUT a fresh process. This is exactly the
// transition the old one-shot code could never make.
func TestNew_BackgroundDiscovery_EventuallyReady(t *testing.T) {
	// Default attempts==0 means retry indefinitely; shrink the delays so the
	// test is fast.
	origAttempts, origDelay, origMax := providerRetryAttempts, providerRetryInitialDelay, providerRetryMaxDelay
	providerRetryAttempts = 0 // indefinite (production default)
	providerRetryInitialDelay = 2 * time.Millisecond
	providerRetryMaxDelay = 5 * time.Millisecond
	t.Cleanup(func() {
		providerRetryAttempts = origAttempts
		providerRetryInitialDelay = origDelay
		providerRetryMaxDelay = origMax
	})

	// Fail the first 3 discovery attempts, then succeed.
	fi := newFlakyIssuer(t, 3)

	h := New(context.Background(), Config{IssuerURL: fi.url, ClientID: "test"}, slog.Default())

	// Immediately after New: NOT ready, and the Keycloak path fail-closes 503.
	if h.Ready() {
		t.Fatal("Ready() = true synchronously after New — discovery must run in the background, not on the critical path")
	}
	rec := httptest.NewRecorder()
	h.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("inner handler invoked while discovery still pending — fail-closed contract broken")
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("pending discovery: status = %d, want 503", rec.Code)
	}

	// Background goroutine retries and eventually succeeds → Ready() flips
	// true without a process restart.
	waitReady(t, h)

	if got := fi.calls.Load(); got < 4 {
		t.Errorf("discovery succeeded after %d calls; want >=4 (3 failures + 1 success)", got)
	}

	// And the Keycloak middleware is now the verifier, not fail-closed: a
	// request with no token is rejected 401 (token missing) rather than 503
	// (provider unavailable). This proves the atomic swap took effect.
	rec2 := httptest.NewRecorder()
	h.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec2.Code == http.StatusServiceUnavailable {
		t.Fatal("middleware still returns 503 after discovery succeeded — swap did not take effect")
	}
	if rec2.Code != http.StatusUnauthorized {
		t.Errorf("post-discovery, no-token request: status = %d, want 401 (verifying middleware active)", rec2.Code)
	}
}

// TestNew_BackgroundDiscovery_FailClosedUntilReady proves the fail-closed
// invariant holds for the WHOLE pending window: while discovery keeps failing,
// every request gets 503 and the inner handler is never reached. This is the
// load-bearing security property (ka-0000006185 F1) under the new async model.
func TestNew_BackgroundDiscovery_FailClosedUntilReady(t *testing.T) {
	origAttempts, origDelay := providerRetryAttempts, providerRetryInitialDelay
	providerRetryAttempts = 0
	providerRetryInitialDelay = time.Millisecond
	t.Cleanup(func() {
		providerRetryAttempts = origAttempts
		providerRetryInitialDelay = origDelay
	})

	// Point at a closed port so discovery never succeeds.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}

	h := New(context.Background(), Config{IssuerURL: "http://" + addr, ClientID: "test"}, slog.Default())

	// Hammer the middleware across the pending window; it must 503 every time
	// and never pass through.
	for i := 0; i < 20; i++ {
		rec := httptest.NewRecorder()
		h.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			t.Fatal("inner handler invoked — fail-closed contract broken while discovery pending")
			w.WriteHeader(http.StatusOK)
		})).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("iteration %d: status = %d, want 503", i, rec.Code)
		}
		time.Sleep(time.Millisecond)
	}
	if h.Ready() {
		t.Fatal("Ready() = true with an unreachable issuer — readiness gate broken")
	}
}

// TestNew_DiscoveryInterruptibleByContext proves SIGTERM/shutdown can interrupt
// an in-flight discovery loop: cancelling the context passed to New stops the
// background goroutine instead of letting it spin forever (fixes the latent
// uninterruptible-discovery shutdown hang, ka-0000001839).
func TestNew_DiscoveryInterruptibleByContext(t *testing.T) {
	origAttempts, origDelay := providerRetryAttempts, providerRetryInitialDelay
	providerRetryAttempts = 0 // indefinite — only ctx cancellation stops it
	providerRetryInitialDelay = 10 * time.Millisecond
	t.Cleanup(func() {
		providerRetryAttempts = origAttempts
		providerRetryInitialDelay = origDelay
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	// Drive newProviderWithRetry directly so we can observe it returning
	// ctx.Err() promptly on cancellation rather than blocking.
	done := make(chan error, 1)
	go func() {
		_, e := newProviderWithRetry(ctx, "http://"+addr, currentRetryPolicy(), slog.Default())
		done <- e
	}()

	// Let it spin through at least one failed attempt, then cancel.
	time.Sleep(25 * time.Millisecond)
	cancel()

	select {
	case e := <-done:
		if e == nil {
			t.Fatal("expected non-nil error after context cancellation")
		}
		if !errorIsContextCanceled(e) {
			t.Errorf("error = %v, want context.Canceled", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("newProviderWithRetry did not return after context cancellation — discovery is uninterruptible (shutdown hang)")
	}
}

func errorIsContextCanceled(err error) bool {
	return err == context.Canceled || err == context.DeadlineExceeded
}

func TestExtractToken(t *testing.T) {
	tests := []struct {
		name          string
		authorization string
		forwarded     string
		want          string
	}{
		{
			name:          "Bearer header",
			authorization: "Bearer my-token-123",
			want:          "my-token-123",
		},
		{
			name:          "case-insensitive Bearer header",
			authorization: "bearer my-token-123",
			want:          "my-token-123",
		},
		{
			name:      "ForwardAuth header",
			forwarded: "forwarded-token",
			want:      "forwarded-token",
		},
		{
			name:          "Bearer takes precedence",
			authorization: "Bearer bearer-token",
			forwarded:     "forwarded-token",
			want:          "bearer-token",
		},
		{
			name:          "non-Bearer authorization falls back to ForwardAuth",
			authorization: "Basic credentials",
			forwarded:     "forwarded-token",
			want:          "forwarded-token",
		},
		{
			name: "empty",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tt.authorization != "" {
				req.Header.Set("Authorization", tt.authorization)
			}
			if tt.forwarded != "" {
				req.Header.Set("X-Auth-Request-Access-Token", tt.forwarded)
			}

			if got := extractToken(req); got != tt.want {
				t.Errorf("extractToken() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestIsAllowedEmailDomain(t *testing.T) {
	tests := []struct {
		email   string
		allowed []string
		want    bool
	}{
		{"user@acme.example", []string{"acme.example"}, true},
		{"user@acme.example", []string{"acme.example"}, true},
		{"USER@acme.example", []string{"acme.example"}, true},
		{"user@other.com", []string{"acme.example"}, false},
		{"user@acme.example", []string{"acme.example", "acme.example"}, true},
		{"user@acme.example", []string{"acme.example", "acme.example"}, true},
		{"user@evil.com", []string{"acme.example", "acme.example"}, false},
		{"invalid-email", []string{"acme.example"}, false},
		{"", []string{"acme.example"}, false},
		{"user@acme.example", nil, false},
		{"user@acme.example", []string{}, false},
	}

	for _, tt := range tests {
		got := isAllowedEmailDomain(tt.email, tt.allowed)
		if got != tt.want {
			t.Errorf("isAllowedEmailDomain(%q, %v) = %v, want %v", tt.email, tt.allowed, got, tt.want)
		}
	}
}

func TestHasMinimumRole(t *testing.T) {
	tests := []struct {
		name     string
		roles    []string
		required string
		want     bool
	}{
		{"admin has admin", []string{RoleAdmin}, RoleAdmin, true},
		{"admin has operator", []string{RoleAdmin}, RoleOperator, true},
		{"admin has viewer", []string{RoleAdmin}, RoleViewer, true},
		{"operator has operator", []string{RoleOperator}, RoleOperator, true},
		{"operator has viewer", []string{RoleOperator}, RoleViewer, true},
		{"operator lacks admin", []string{RoleOperator}, RoleAdmin, false},
		{"viewer has viewer", []string{RoleViewer}, RoleViewer, true},
		{"viewer lacks operator", []string{RoleViewer}, RoleOperator, false},
		{"viewer lacks admin", []string{RoleViewer}, RoleAdmin, false},
		{"no roles", []string{}, RoleViewer, false},
		{"nil roles", nil, RoleViewer, false},
		{"unknown required role", []string{RoleAdmin}, "gardenio-superadmin", false},
		{"matching custom role", []string{"custom-realm-role"}, "custom-realm-role", true},
		{"unrelated roles ignored", []string{"some-other-role"}, RoleViewer, false},
		{"mixed roles use highest", []string{"other", RoleOperator}, RoleOperator, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := HasMinimumRole(tt.roles, tt.required)
			if got != tt.want {
				t.Errorf("HasMinimumRole(%v, %q) = %v, want %v", tt.roles, tt.required, got, tt.want)
			}
		})
	}
}

func TestRequireRole_AllowsWhenNoRoles(t *testing.T) {
	// When no roles in context (internal caller), request passes through.
	mw := RequireRole(RoleAdmin, slog.Default())
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 when no roles in context, got %d", rec.Code)
	}
}

func TestRequireRole_AllowsSufficientRole(t *testing.T) {
	mw := RequireRole(RoleOperator, slog.Default())
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	ctx := context.WithValue(context.Background(), ctxRoles, []string{RoleAdmin})
	req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 when admin accesses operator route, got %d", rec.Code)
	}
}

func TestRequireRole_DeniesInsufficientRole(t *testing.T) {
	mw := RequireRole(RoleAdmin, slog.Default())
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	ctx := context.WithValue(context.Background(), ctxRoles, []string{RoleViewer})
	req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 when viewer accesses admin route, got %d", rec.Code)
	}
}
