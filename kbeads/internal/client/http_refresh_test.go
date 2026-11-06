package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

// expiredTokenChallenge is the WWW-Authenticate header the server emits
// when an access token's TTL has been crossed. The client must distinguish
// this from other 401 reasons.
const expiredTokenChallenge = `Bearer error="invalid_token", error_description="token expired", refresh_endpoint="/v1/auth/refresh"`

// TestRefreshOn401 covers the happy path: 401 with the expired-token
// challenge triggers a refresh exchange, and the original request is
// retried once with the new access token.
func TestRefreshOn401(t *testing.T) {
	var (
		healthCalls atomic.Int32
		authHeaders []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/auth/refresh":
			body, _ := io.ReadAll(r.Body)
			var req map[string]string
			if err := json.Unmarshal(body, &req); err != nil {
				t.Errorf("decode refresh body: %v", err)
			}
			if req["refresh_token"] != "refresh-1" {
				t.Errorf("refresh_token = %q, want refresh-1", req["refresh_token"])
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"access-2","expires_in":3600,"token_type":"Bearer"}`))
		case "/v1/health":
			authHeaders = append(authHeaders, r.Header.Get("Authorization"))
			n := healthCalls.Add(1)
			if n == 1 {
				w.Header().Set("WWW-Authenticate", expiredTokenChallenge)
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"token expired"}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, "access-1")
	c.SetRefreshToken("refresh-1")

	got, err := c.Health(context.Background())
	if err != nil {
		t.Fatalf("Health() error = %v", err)
	}
	if got != "ok" {
		t.Errorf("status = %q, want ok", got)
	}
	if healthCalls.Load() != 2 {
		t.Errorf("/v1/health calls = %d, want 2 (initial + retry)", healthCalls.Load())
	}
	if len(authHeaders) != 2 {
		t.Fatalf("auth headers captured = %d, want 2", len(authHeaders))
	}
	if authHeaders[0] != "Bearer access-1" {
		t.Errorf("first auth = %q, want Bearer access-1", authHeaders[0])
	}
	if authHeaders[1] != "Bearer access-2" {
		t.Errorf("second auth = %q, want Bearer access-2", authHeaders[1])
	}
}

// TestRefreshSkippedWithoutToken asserts that a 401-with-expired-challenge
// is surfaced to the caller verbatim when no refresh token is configured.
func TestRefreshSkippedWithoutToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/auth/refresh" {
			t.Errorf("refresh endpoint should not be called without a refresh token")
		}
		w.Header().Set("WWW-Authenticate", expiredTokenChallenge)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"token expired"}`))
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, "access-1")
	if _, err := c.Health(context.Background()); err == nil {
		t.Fatalf("Health() = nil, want 401 error")
	}
}

// TestRefreshSkippedForOtherInvalidTokenReasons asserts we DO NOT
// auto-refresh when the daemon signals invalid_token for a non-expiry
// reason — those should bubble up so the caller can correct them rather
// than burning the refresh token in a futile retry loop.
func TestRefreshSkippedForOtherInvalidTokenReasons(t *testing.T) {
	var refreshCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/auth/refresh" {
			refreshCalls.Add(1)
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token", error_description="malformed token"`)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"malformed token"}`))
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, "access-1")
	c.SetRefreshToken("refresh-1")

	if _, err := c.Health(context.Background()); err == nil {
		t.Fatalf("Health() = nil, want 401 error")
	}
	if n := refreshCalls.Load(); n != 0 {
		t.Errorf("refresh calls = %d, want 0", n)
	}
}

// TestRefreshSingleFlight asserts that N concurrent goroutines hitting an
// expired access token only trigger ONE call to /v1/auth/refresh. Without
// the mutex + revalidation the daemon would see N stampeding refresh
// requests right after a TTL expiry — exactly the thundering-herd scenario
// the bead description calls out.
func TestRefreshSingleFlight(t *testing.T) {
	const concurrency = 16

	var (
		refreshCalls atomic.Int32
		seenAccess   sync.Map // access tokens observed at /v1/health
		// Gate the refresh handler so all 16 callers pile up on refreshMu
		// before any of them is allowed to complete the exchange. Without
		// this, the first goroutine could finish refresh + retry before
		// the 16th has even produced its initial 401, masking races.
		gate    = make(chan struct{})
		gateOnce sync.Once
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/auth/refresh":
			refreshCalls.Add(1)
			<-gate
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"access-2","expires_in":3600,"token_type":"Bearer"}`))
		case "/v1/health":
			auth := r.Header.Get("Authorization")
			seenAccess.Store(auth, true)
			if auth == "Bearer access-1" {
				w.Header().Set("WWW-Authenticate", expiredTokenChallenge)
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"token expired"}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		}
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, "access-1")
	c.SetRefreshToken("refresh-1")

	// Release the refresh handler once we know all 16 callers are in
	// flight. We do NOT depend on a sleep — the test fans out the
	// goroutines first, then a single signal closes the gate.
	go func() {
		// Wait for the first refresh request to land before opening the
		// gate. Once the first hits, every other caller is guaranteed to
		// have either entered refreshMu or completed the post-acquire
		// re-read fast path — both of which are the behaviours under test.
		for refreshCalls.Load() == 0 {
		}
		gateOnce.Do(func() { close(gate) })
	}()

	var wg sync.WaitGroup
	errs := make(chan error, concurrency)
	wg.Add(concurrency)
	for range concurrency {
		go func() {
			defer wg.Done()
			if _, err := c.Health(context.Background()); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("Health() error = %v", err)
	}

	if n := refreshCalls.Load(); n != 1 {
		t.Errorf("refresh exchanges = %d, want 1 (single-flight)", n)
	}

	// Both access tokens should have hit /v1/health: access-1 for the
	// initial 401-producing requests, access-2 for the retried ones.
	if _, ok := seenAccess.Load("Bearer access-1"); !ok {
		t.Errorf("did not observe initial Bearer access-1 request")
	}
	if _, ok := seenAccess.Load("Bearer access-2"); !ok {
		t.Errorf("did not observe retried Bearer access-2 request")
	}
}

// TestRefreshRotation asserts that when the server rotates the refresh
// token (RFC 6749 §10.4 single-use refresh) the client (a) installs the
// new refresh for subsequent exchanges and (b) fires the OnRefresh
// callback so external caches (e.g. the kd CLI's on-disk token file)
// stay in sync.
func TestRefreshRotation(t *testing.T) {
	var refreshN atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/auth/refresh":
			refreshN.Add(1)
			body, _ := io.ReadAll(r.Body)
			var req map[string]string
			_ = json.Unmarshal(body, &req)
			if refreshN.Load() == 1 && req["refresh_token"] != "refresh-1" {
				t.Errorf("first refresh = %q, want refresh-1", req["refresh_token"])
			}
			if refreshN.Load() == 2 && req["refresh_token"] != "refresh-2" {
				t.Errorf("second refresh = %q, want refresh-2 (rotated)", req["refresh_token"])
			}
			n := refreshN.Load()
			w.Header().Set("Content-Type", "application/json")
			body2, _ := json.Marshal(map[string]any{
				"access_token":          "access-rotated",
				"refresh_token":         "refresh-2",
				"refresh_token_rotated": true,
				"expires_in":            3600,
				"token_type":            "Bearer",
			})
			_ = body2
			if n == 1 {
				_, _ = w.Write([]byte(`{"access_token":"access-2","refresh_token":"refresh-2","refresh_token_rotated":true,"expires_in":3600,"token_type":"Bearer"}`))
				return
			}
			_, _ = w.Write([]byte(`{"access_token":"access-3","refresh_token":"refresh-3","refresh_token_rotated":true,"expires_in":3600,"token_type":"Bearer"}`))
		case "/v1/health":
			auth := r.Header.Get("Authorization")
			// Expire every odd-numbered access token so the client must
			// refresh between the two health calls.
			if auth == "Bearer access-1" || auth == "Bearer access-2" {
				w.Header().Set("WWW-Authenticate", expiredTokenChallenge)
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"token expired"}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		}
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, "access-1")
	c.SetRefreshToken("refresh-1")

	var (
		cbMu       sync.Mutex
		cbAccess   []string
		cbRefresh  []string
	)
	c.SetOnRefresh(func(a, r string) {
		cbMu.Lock()
		defer cbMu.Unlock()
		cbAccess = append(cbAccess, a)
		cbRefresh = append(cbRefresh, r)
	})

	// First call: access-1 → 401 → refresh(refresh-1) → access-2.
	// access-2 also expires per the handler so the test triggers ONE
	// successful retry, then surfaces the second 401 on a single Health
	// call. We instead invoke Health twice with a fresh access-3 server
	// response on the second attempt.
	if _, err := c.Health(context.Background()); err == nil {
		t.Fatalf("first Health() = nil, want 401 (access-2 also expired)")
	}
	if n := refreshN.Load(); n != 1 {
		t.Fatalf("refresh exchanges after first call = %d, want 1", n)
	}

	// Second call: client now holds access-2 + refresh-2. access-2 is
	// expired per handler → second refresh consumes refresh-2 → access-3.
	got, err := c.Health(context.Background())
	if err != nil {
		t.Fatalf("second Health() error = %v", err)
	}
	if got != "ok" {
		t.Errorf("status = %q, want ok", got)
	}
	if n := refreshN.Load(); n != 2 {
		t.Errorf("refresh exchanges total = %d, want 2", n)
	}

	cbMu.Lock()
	defer cbMu.Unlock()
	if len(cbAccess) != 2 {
		t.Fatalf("OnRefresh fired %d times, want 2", len(cbAccess))
	}
	if cbAccess[0] != "access-2" || cbRefresh[0] != "refresh-2" {
		t.Errorf("first OnRefresh = (%q,%q), want (access-2,refresh-2)", cbAccess[0], cbRefresh[0])
	}
	if cbAccess[1] != "access-3" || cbRefresh[1] != "refresh-3" {
		t.Errorf("second OnRefresh = (%q,%q), want (access-3,refresh-3)", cbAccess[1], cbRefresh[1])
	}
}

// TestRefreshFailureSurfacesOriginal401 asserts that when /v1/auth/refresh
// itself returns 401 (e.g. the refresh has been revoked) the caller sees
// the ORIGINAL 401 from the resource request, not the refresh failure —
// because the caller's mental model is "my access token is bad" and that
// is exactly what we should report.
func TestRefreshFailureSurfacesOriginal401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/auth/refresh":
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_grant", error_description="refresh token expired"`)
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"refresh token expired"}`))
		case "/v1/health":
			w.Header().Set("WWW-Authenticate", expiredTokenChallenge)
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"token expired"}`))
		}
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, "access-1")
	c.SetRefreshToken("refresh-1")

	_, err := c.Health(context.Background())
	if err == nil {
		t.Fatalf("Health() = nil, want error")
	}
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("Health() error type = %T, want *APIError", err)
	}
	if apiErr.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", apiErr.StatusCode)
	}
	// Should be the ORIGINAL 401's error body ("token expired"), not the
	// refresh-token failure ("refresh token expired"). The operator wants
	// to know their access call failed, not that the recovery attempt did.
	if apiErr.Message != "token expired" {
		t.Errorf("message = %q, want token expired", apiErr.Message)
	}
}
