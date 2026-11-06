package beadsapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

// expiredTokenChallenge is the WWW-Authenticate header the daemon emits
// when an access token's TTL has been crossed. The client uses this exact
// shape to decide whether to attempt a refresh exchange.
const expiredTokenChallenge = `Bearer error="invalid_token", error_description="token expired", refresh_endpoint="/v1/auth/refresh"`

// minimalBeadBody is the smallest GET /v1/beads/<id> response that decodes
// cleanly into beadJSON. Tests use GetBead as the request under test
// because it's the simplest doJSON path through the client.
const minimalBeadBody = `{"id":"gd-test","title":"t","kind":"issue","type":"task","status":"open","assignee":"","priority":0,"labels":[],"notes":"","fields":null,"description":"","created_by":""}`

// TestRefreshOn401 covers the happy path: a 401 with the expired-token
// challenge triggers a refresh exchange, and the original request is
// retried once with the new access token.
func TestRefreshOn401(t *testing.T) {
	var (
		beadCalls   atomic.Int32
		authHeaders []string
		authMu      sync.Mutex
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
		case "/v1/beads/gd-test":
			authMu.Lock()
			authHeaders = append(authHeaders, r.Header.Get("Authorization"))
			authMu.Unlock()
			n := beadCalls.Add(1)
			if n == 1 {
				w.Header().Set("WWW-Authenticate", expiredTokenChallenge)
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"token expired"}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(minimalBeadBody))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer srv.Close()

	c, err := New(Config{HTTPAddr: srv.URL, Token: "access-1", RefreshToken: "refresh-1"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	bead, err := c.GetBead(context.Background(), "gd-test")
	if err != nil {
		t.Fatalf("GetBead error = %v", err)
	}
	if bead == nil || bead.ID != "gd-test" {
		t.Errorf("bead = %+v, want id=gd-test", bead)
	}
	if beadCalls.Load() != 2 {
		t.Errorf("/v1/beads calls = %d, want 2 (initial + retry)", beadCalls.Load())
	}
	authMu.Lock()
	defer authMu.Unlock()
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

// TestRefreshSkippedWithoutToken asserts that the original 401 is returned
// to the caller verbatim when no refresh token is configured. The client
// MUST NOT attempt an /v1/auth/refresh round-trip in this case.
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

	c, err := New(Config{HTTPAddr: srv.URL, Token: "access-1"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.GetBead(context.Background(), "gd-test"); err == nil {
		t.Fatalf("GetBead = nil, want 401 error")
	}
}

// TestRefreshSkippedForOtherInvalidTokenReasons asserts we DO NOT
// auto-refresh when the daemon signals invalid_token for a non-expiry
// reason — those should bubble up so the caller can correct them rather
// than burning the refresh in a futile retry loop.
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

	c, err := New(Config{HTTPAddr: srv.URL, Token: "access-1", RefreshToken: "refresh-1"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.GetBead(context.Background(), "gd-test"); err == nil {
		t.Fatalf("GetBead = nil, want 401 error")
	}
	if n := refreshCalls.Load(); n != 0 {
		t.Errorf("refresh calls = %d, want 0", n)
	}
}

// TestRefreshSingleFlight asserts that N concurrent goroutines hitting an
// expired access token only trigger ONE call to /v1/auth/refresh. Without
// the refreshMu mutex + post-acquire revalidation the daemon would see N
// stampeding refresh requests right after a TTL expiry — exactly the
// thundering-herd scenario the bead description (ka-0000000681) calls out.
func TestRefreshSingleFlight(t *testing.T) {
	const concurrency = 16

	var (
		refreshCalls atomic.Int32
		seenAccess   sync.Map // access tokens observed at /v1/beads/gd-test
		gate         = make(chan struct{})
		gateOnce     sync.Once
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/auth/refresh":
			refreshCalls.Add(1)
			<-gate
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"access-2","expires_in":3600,"token_type":"Bearer"}`))
		case "/v1/beads/gd-test":
			auth := r.Header.Get("Authorization")
			seenAccess.Store(auth, true)
			if auth == "Bearer access-1" {
				w.Header().Set("WWW-Authenticate", expiredTokenChallenge)
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"token expired"}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(minimalBeadBody))
		}
	}))
	defer srv.Close()

	c, err := New(Config{HTTPAddr: srv.URL, Token: "access-1", RefreshToken: "refresh-1"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Open the gate as soon as the first refresh request arrives. By that
	// point all other goroutines are guaranteed to have either entered
	// refreshMu or completed the post-acquire re-read fast path — both of
	// which are the behaviours under test.
	go func() {
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
			if _, err := c.GetBead(context.Background(), "gd-test"); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("GetBead error = %v", err)
	}

	if n := refreshCalls.Load(); n != 1 {
		t.Errorf("refresh exchanges = %d, want 1 (single-flight)", n)
	}

	if _, ok := seenAccess.Load("Bearer access-1"); !ok {
		t.Errorf("did not observe initial Bearer access-1 request")
	}
	if _, ok := seenAccess.Load("Bearer access-2"); !ok {
		t.Errorf("did not observe retried Bearer access-2 request")
	}
}

// TestRefreshRotation asserts that when the daemon rotates the refresh
// token (RFC 6749 §10.4 single-use refresh) the client (a) installs the
// new refresh for subsequent exchanges and (b) fires the OnRefresh
// callback so external caches stay in sync.
func TestRefreshRotation(t *testing.T) {
	var refreshN atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/auth/refresh":
			refreshN.Add(1)
			body, _ := io.ReadAll(r.Body)
			var req map[string]string
			_ = json.Unmarshal(body, &req)
			n := refreshN.Load()
			if n == 1 && req["refresh_token"] != "refresh-1" {
				t.Errorf("first refresh = %q, want refresh-1", req["refresh_token"])
			}
			if n == 2 && req["refresh_token"] != "refresh-2" {
				t.Errorf("second refresh = %q, want refresh-2 (rotated)", req["refresh_token"])
			}
			w.Header().Set("Content-Type", "application/json")
			if n == 1 {
				_, _ = w.Write([]byte(`{"access_token":"access-2","refresh_token":"refresh-2","refresh_token_rotated":true,"expires_in":3600,"token_type":"Bearer"}`))
				return
			}
			_, _ = w.Write([]byte(`{"access_token":"access-3","refresh_token":"refresh-3","refresh_token_rotated":true,"expires_in":3600,"token_type":"Bearer"}`))
		case "/v1/beads/gd-test":
			auth := r.Header.Get("Authorization")
			// Expire access-1 and access-2 so each call needs a refresh.
			if auth == "Bearer access-1" || auth == "Bearer access-2" {
				w.Header().Set("WWW-Authenticate", expiredTokenChallenge)
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"token expired"}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(minimalBeadBody))
		}
	}))
	defer srv.Close()

	c, err := New(Config{HTTPAddr: srv.URL, Token: "access-1", RefreshToken: "refresh-1"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var (
		cbMu      sync.Mutex
		cbAccess  []string
		cbRefresh []string
	)
	c.SetOnRefresh(func(a, r string) {
		cbMu.Lock()
		defer cbMu.Unlock()
		cbAccess = append(cbAccess, a)
		cbRefresh = append(cbRefresh, r)
	})

	// First call: access-1 → 401 → refresh(refresh-1) → access-2 → still
	// expired per handler → second 401 surfaces to caller. Net: 1 refresh.
	if _, err := c.GetBead(context.Background(), "gd-test"); err == nil {
		t.Fatalf("first GetBead = nil, want 401 (access-2 also expired)")
	}
	if n := refreshN.Load(); n != 1 {
		t.Fatalf("refresh exchanges after first call = %d, want 1", n)
	}

	// Second call: client now holds access-2 + refresh-2. access-2 still
	// expired → second refresh consumes refresh-2 → access-3 → success.
	bead, err := c.GetBead(context.Background(), "gd-test")
	if err != nil {
		t.Fatalf("second GetBead error = %v", err)
	}
	if bead == nil || bead.ID != "gd-test" {
		t.Errorf("bead = %+v, want id=gd-test", bead)
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
		case "/v1/beads/gd-test":
			w.Header().Set("WWW-Authenticate", expiredTokenChallenge)
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"token expired"}`))
		}
	}))
	defer srv.Close()

	c, err := New(Config{HTTPAddr: srv.URL, Token: "access-1", RefreshToken: "refresh-1"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = c.GetBead(context.Background(), "gd-test")
	if err == nil {
		t.Fatalf("GetBead = nil, want error")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("GetBead error type = %T, want unwrappable to *APIError", err)
	}
	if apiErr.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", apiErr.StatusCode)
	}
	if apiErr.Message != "token expired" {
		t.Errorf("message = %q, want token expired (original 401, not the refresh failure)", apiErr.Message)
	}
}
