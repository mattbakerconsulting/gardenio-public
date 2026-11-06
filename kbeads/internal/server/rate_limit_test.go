package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRateLimitClass(t *testing.T) {
	tests := []struct {
		method, path, want string
	}{
		{"GET", "/v1/health", ""},
		{"GET", "/v1/events/stream", ""},
		{"POST", "/v1/events/stream", ""},
		{"GET", "/v1/bus/events", ""},
		// Snapshot endpoints that contain "events" must NOT bypass the
		// rate limiter — ka-0000002324. Previously the substring matcher
		// classified these as streaming and exempted them, which let
		// abusive clients pin response buffers in memory.
		{"GET", "/v1/events", "read"},
		{"GET", "/v1/beads/gd-abc/events", "read"},
		{"GET", "/v1/beads/gd-abc", "read"},
		{"HEAD", "/v1/beads/gd-abc", "read"},
		{"OPTIONS", "/v1/beads", "read"},
		{"POST", "/v1/beads", "mutate"},
		{"PATCH", "/v1/beads/gd-abc", "mutate"},
		{"DELETE", "/v1/beads/gd-abc", "mutate"},
		{"PUT", "/v1/beads/gd-abc", "mutate"},
	}
	for _, tc := range tests {
		got := rateLimitClass(tc.method, tc.path)
		if got != tc.want {
			t.Errorf("rateLimitClass(%q, %q) = %q; want %q", tc.method, tc.path, got, tc.want)
		}
	}
}

func TestRateLimitIdentityKey(t *testing.T) {
	tp := &TrustedProxyConfig{}
	tests := []struct {
		name string
		id   AuthIdentity
		want string
	}{
		{"machine keyed", AuthIdentity{Type: "machine", Subject: "machine:gardenio-controller"}, "id:machine:machine:gardenio-controller"},
		{"fleet token keyed", AuthIdentity{Type: "fleet", Subject: "squad-a"}, "id:fleet:squad-a"},
		{"oidc keyed by subject", AuthIdentity{Type: "oidc", Subject: "alice@example.com"}, "id:oidc:alice@example.com"},
	}
	for _, tc := range tests {
		r := httptest.NewRequest("GET", "/v1/beads", nil)
		r = r.WithContext(WithAuthIdentity(r.Context(), tc.id))
		if got := rateLimitIdentityKey(r, tp); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestRateLimitIdentityKeyAnonymousFallsBackToIP(t *testing.T) {
	r := httptest.NewRequest("POST", "/v1/beads", nil)
	r.RemoteAddr = "10.1.2.3:5555"
	key := rateLimitIdentityKey(r, nil)
	if key != "ip:10.1.2.3" {
		t.Errorf("got %q, want ip:10.1.2.3", key)
	}
}

// countingHandler records how many requests make it through.
type countingHandler struct{ hits int }

func (c *countingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.hits++
	w.WriteHeader(http.StatusNoContent)
}

func TestRateLimitMiddleware_BurstThen429(t *testing.T) {
	cfg := RateLimitConfig{
		Enabled:      true,
		MutateRPS:    1, // refill barely trickles during the test
		MutateBurst:  3,
		ReadRPS:      100,
		ReadBurst:    100,
		IdleEviction: time.Minute,
	}
	inner := &countingHandler{}
	h := RateLimitMiddleware(cfg, nil, inner)

	send := func() int {
		r := httptest.NewRequest("POST", "/v1/beads", nil)
		r = r.WithContext(WithAuthIdentity(r.Context(), AuthIdentity{Type: "fleet", Subject: "squad-a"}))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}

	// First 3 requests (= burst) succeed.
	for i := 0; i < 3; i++ {
		if code := send(); code != http.StatusNoContent {
			t.Fatalf("request %d: got %d, want 204", i+1, code)
		}
	}

	// 4th should 429 with Retry-After.
	r := httptest.NewRequest("POST", "/v1/beads", nil)
	r = r.WithContext(WithAuthIdentity(r.Context(), AuthIdentity{Type: "fleet", Subject: "squad-a"}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("got %d, want 429", w.Code)
	}
	if ra := w.Header().Get("Retry-After"); ra != "1" {
		t.Errorf("Retry-After = %q, want %q", ra, "1")
	}
	if inner.hits != 3 {
		t.Errorf("inner hits = %d, want 3", inner.hits)
	}
}

func TestRateLimitMiddleware_PerIdentityIsolation(t *testing.T) {
	cfg := RateLimitConfig{
		Enabled:      true,
		MutateRPS:    0.001,
		MutateBurst:  1,
		ReadRPS:      0.001,
		ReadBurst:    1,
		IdleEviction: time.Minute,
	}
	inner := &countingHandler{}
	h := RateLimitMiddleware(cfg, nil, inner)

	send := func(subject string) int {
		r := httptest.NewRequest("POST", "/v1/beads", nil)
		r = r.WithContext(WithAuthIdentity(r.Context(), AuthIdentity{Type: "oidc", Subject: subject}))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}

	// alice gets her 1-token burst.
	if code := send("alice"); code != http.StatusNoContent {
		t.Fatalf("alice first: got %d, want 204", code)
	}
	// alice is now out of tokens.
	if code := send("alice"); code != http.StatusTooManyRequests {
		t.Fatalf("alice second: got %d, want 429", code)
	}
	// bob's bucket is independent — still gets his burst.
	if code := send("bob"); code != http.StatusNoContent {
		t.Fatalf("bob first: got %d, want 204", code)
	}
}

// Nothing is exempt by identity any more: the retired primary static token was
// the one exemption. A fleet identity gets exactly the configured burst (the
// machine-principal boost is covered by
// TestRateLimitMiddleware_MachinePrincipalBoostedNotExempt).
func TestRateLimitMiddleware_NoIdentityExemption(t *testing.T) {
	cfg := RateLimitConfig{
		Enabled:      true,
		MutateRPS:    0.001,
		MutateBurst:  1,
		ReadRPS:      0.001,
		ReadBurst:    1,
		IdleEviction: time.Minute,
	}
	inner := &countingHandler{}
	h := RateLimitMiddleware(cfg, nil, inner)

	var limited bool
	for i := 0; i < 5; i++ {
		r := httptest.NewRequest("POST", "/v1/beads", nil)
		r = r.WithContext(WithAuthIdentity(r.Context(), AuthIdentity{Type: "fleet", Subject: "gardenio-crew-x"}))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code == http.StatusTooManyRequests {
			limited = true
		}
	}
	if !limited {
		t.Error("a fleet identity was never rate limited")
	}
	if inner.hits >= 5 {
		t.Errorf("inner hits = %d, want fewer than 5", inner.hits)
	}
}

// A machine principal keeps its own per-identity bucket (it is not exempt
// like the static token) but with MachineMultiplier times the budget, so a
// per-bead sweep is not throttled like a user while a runaway client is
// still bounded.
func TestRateLimitMiddleware_MachinePrincipalBoostedNotExempt(t *testing.T) {
	cfg := RateLimitConfig{
		Enabled:      true,
		MutateRPS:    0.001,
		MutateBurst:  2,
		ReadRPS:      0.001,
		ReadBurst:    2,
		IdleEviction: time.Minute,
	}
	inner := &countingHandler{}
	h := RateLimitMiddleware(cfg, nil, inner)
	send := func(id AuthIdentity) int {
		r := httptest.NewRequest("GET", "/v1/beads", nil)
		r = r.WithContext(WithAuthIdentity(r.Context(), id))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	allowed := func(id AuthIdentity) int {
		n := 0
		for i := 0; i < 100; i++ {
			if send(id) == http.StatusNoContent {
				n++
			}
		}
		return n
	}

	machine := AuthIdentity{Type: "machine", Subject: "machine:gardenio-trigger-engine", ClientID: "gardenio-trigger-engine"}
	if got, want := allowed(machine), 2*defaultMachineRateMultiplier; got != want {
		t.Errorf("machine principal allowed %d of 100 reads; want the 10x burst %d, then 429", got, want)
	}
	user := AuthIdentity{Type: "oidc", Subject: "alice@example.com"}
	if got := allowed(user); got != 2 {
		t.Errorf("oidc user allowed %d of 100 reads; want the 1x burst 2", got)
	}

	// MachineMultiplier=1 turns the boost off.
	cfg.MachineMultiplier = 1
	h = RateLimitMiddleware(cfg, nil, inner)
	if got := allowed(machine); got != 2 {
		t.Errorf("MachineMultiplier=1: machine allowed %d; want 2", got)
	}
}

func TestRateLimitMiddleware_HealthAndStreamExempt(t *testing.T) {
	cfg := RateLimitConfig{
		Enabled:      true,
		MutateRPS:    0.001,
		MutateBurst:  1,
		ReadRPS:      0.001,
		ReadBurst:    1,
		IdleEviction: time.Minute,
	}
	inner := &countingHandler{}
	h := RateLimitMiddleware(cfg, nil, inner)

	// /v1/health never throttles.
	for i := 0; i < 20; i++ {
		r := httptest.NewRequest("GET", "/v1/health", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusNoContent {
			t.Fatalf("/v1/health iter %d: got %d", i, w.Code)
		}
	}

	// Streaming paths never throttle.
	for i := 0; i < 20; i++ {
		r := httptest.NewRequest("GET", "/v1/events/stream", nil)
		r = r.WithContext(WithAuthIdentity(r.Context(), AuthIdentity{Type: "oidc", Subject: "alice"}))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusNoContent {
			t.Fatalf("stream iter %d: got %d", i, w.Code)
		}
	}
}

// The land door's admission check (GET /v1/me with X-Land-Door and
// X-Land-Door-Check) never throttles. A caller's own GET /v1/me through the
// door, which carries X-Land-Door but no check marker, is counted, as is the
// same GET off the door and any other route through it (gd-Utbyy4xJRX).
func TestRateLimitMiddleware_LandDoorAuthCheckExempt(t *testing.T) {
	cfg := RateLimitConfig{
		Enabled:      true,
		MutateRPS:    0.001,
		MutateBurst:  1,
		ReadRPS:      0.001,
		ReadBurst:    1,
		IdleEviction: time.Minute,
	}
	h := RateLimitMiddleware(cfg, nil, &countingHandler{})
	land := AuthIdentity{Type: "land", Subject: "land:acme"}
	get := func(path, door, check string) int {
		r := httptest.NewRequest("GET", path, nil)
		r = r.WithContext(WithAuthIdentity(r.Context(), land))
		if door != "" {
			r.Header.Set(LandDoorHeader, door)
		}
		if check != "" {
			r.Header.Set(LandDoorCheckHeader, check)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	for i := 0; i < 20; i++ {
		if code := get("/v1/me", "acme", "1"); code != http.StatusNoContent {
			t.Fatalf("door check iter %d: got %d", i, code)
		}
	}
	if code := get("/v1/me", "acme", ""); code != http.StatusNoContent {
		t.Fatalf("caller's first GET /v1/me through the door: got %d, want it to spend the burst", code)
	}
	if code := get("/v1/me", "acme", ""); code != http.StatusTooManyRequests {
		t.Errorf("caller's second GET /v1/me through the door: got %d, want 429", code)
	}
	if code := get("/v1/me", "acme", "yes"); code != http.StatusTooManyRequests {
		t.Errorf("GET /v1/me with a malformed check marker: got %d, want 429", code)
	}
	if code := get("/v1/beads", "acme", "1"); code != http.StatusTooManyRequests {
		t.Errorf("door read with the check marker: got %d, want 429", code)
	}
	if code := get("/v1/me", "", "1"); code != http.StatusTooManyRequests {
		t.Errorf("GET /v1/me off the door: got %d, want 429", code)
	}
}

func TestRateLimitMiddleware_DisabledPassthrough(t *testing.T) {
	cfg := RateLimitConfig{Enabled: false, MutateRPS: 1, MutateBurst: 1}
	inner := &countingHandler{}
	h := RateLimitMiddleware(cfg, nil, inner)

	for i := 0; i < 100; i++ {
		r := httptest.NewRequest("POST", "/v1/beads", nil)
		r = r.WithContext(WithAuthIdentity(r.Context(), AuthIdentity{Type: "oidc", Subject: "alice"}))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusNoContent {
			t.Fatalf("iter %d: got %d, want 204 (disabled middleware must pass through)", i, w.Code)
		}
	}
}

func TestRateLimiter_IdleEviction(t *testing.T) {
	cfg := RateLimitConfig{
		Enabled:      true,
		MutateRPS:    1,
		MutateBurst:  1,
		ReadRPS:      1,
		ReadBurst:    1,
		IdleEviction: 5 * time.Minute,
	}
	rl := newRateLimiter(cfg)
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	rl.now = func() time.Time { return base }

	rl.bucket("a")
	rl.bucket("b")
	if len(rl.buckets) != 2 {
		t.Fatalf("got %d buckets, want 2", len(rl.buckets))
	}

	// Advance past idle horizon; a fresh insertion triggers eviction.
	rl.now = func() time.Time { return base.Add(10 * time.Minute) }
	rl.bucket("c")
	if _, ok := rl.buckets["a"]; ok {
		t.Errorf("bucket a should have been evicted")
	}
	if _, ok := rl.buckets["b"]; ok {
		t.Errorf("bucket b should have been evicted")
	}
	if _, ok := rl.buckets["c"]; !ok {
		t.Errorf("bucket c should exist")
	}
}
