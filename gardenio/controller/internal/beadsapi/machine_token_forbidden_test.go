package beadsapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// roleToken builds a fixture token carrying exactly roles; tag keeps tokens
// distinct when the roles are equal.
func roleToken(t *testing.T, tag string, roles ...string) string {
	t.Helper()
	return fixtureJWT(t, map[string]any{
		"azp":         "gardenio-controller",
		"tag":         tag,
		"realm_roles": roles,
		"iat":         1700000000,
		"exp":         1700000000 + 1800,
	})
}

func (e *tokenEndpoint) setToken(tok string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.token = tok
}

func newForbiddenFixture(t *testing.T, first string, opts ...MachineTokenOption) (*tokenEndpoint, *machineTokenSource, *time.Time) {
	t.Helper()
	ep := &tokenEndpoint{t: t, token: first, expiresIn: 1800}
	srv := httptest.NewServer(ep.handler())
	t.Cleanup(srv.Close)
	file := writeSecretFile(t, t.TempDir(), fixtureSecret)
	ts := NewMachineTokenSource(context.Background(), nil, srv.URL, "gardenio-controller", file, opts...)
	src := ts.(*machineTokenSource)
	clock := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	src.now = func() time.Time { return clock }
	return ep, src, &clock
}

// The first-roll race: Keycloak minted a token before attaching the role. The
// role-less token must be re-fetched soon, not pinned to half-life.
func TestMachineTokenSource_MissingExpectedRoleRefetchesSoon(t *testing.T) {
	const role = "gardenio-machine-trigger-ops"
	roleless := roleToken(t, "a", "gardenio-viewer")
	withRole := roleToken(t, "b", "gardenio-viewer", role)
	ep, src, clock := newForbiddenFixture(t, roleless, WithExpectedRoles(role))
	ctx := context.Background()
	before := testutil.ToFloat64(machineTokenMissingRoleTotal.WithLabelValues("gardenio-controller", role))

	if got, err := src.Token(ctx); err != nil || got != roleless {
		t.Fatalf("first token: %v %v", got, err)
	}
	if d := testutil.ToFloat64(machineTokenMissingRoleTotal.WithLabelValues("gardenio-controller", role)) - before; d != 1 {
		t.Fatalf("missing-role counter delta = %v, want 1", d)
	}

	// Keycloak attaches the role; inside the retry window the cache still serves.
	ep.setToken(withRole)
	*clock = clock.Add(machineMissingRoleRetry - time.Second)
	if got, _ := src.Token(ctx); got != roleless || ep.count() != 1 {
		t.Fatalf("inside retry window must serve cache; got fetches=%d", ep.count())
	}

	// Past the window the token is re-fetched, then cached to half-life.
	*clock = clock.Add(2 * time.Second)
	if got, err := src.Token(ctx); err != nil || got != withRole {
		t.Fatalf("after retry window want role-bearing token, got %v %v", got, err)
	}
	*clock = clock.Add(10 * time.Minute)
	if _, _ = src.Token(ctx); ep.count() != 2 {
		t.Fatalf("role-bearing token must be cached to half-life, fetches=%d", ep.count())
	}
}

func TestMachineTokenSource_NoExpectedRolesKeepsHalfLife(t *testing.T) {
	ep, src, clock := newForbiddenFixture(t, roleToken(t, "a", "gardenio-viewer"))
	if _, err := src.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	*clock = clock.Add(5 * time.Minute)
	if _, _ = src.Token(context.Background()); ep.count() != 1 {
		t.Fatalf("no expected roles: want cached, fetches=%d", ep.count())
	}
}

func TestMachineTokenSource_OnForbiddenReacquiresOnceRateLimited(t *testing.T) {
	old := roleToken(t, "old", "gardenio-viewer")
	fresh := roleToken(t, "fresh", "gardenio-viewer", "gardenio-machine-trigger-ops")
	ep, src, clock := newForbiddenFixture(t, old)
	ctx := context.Background()
	if _, err := src.Token(ctx); err != nil {
		t.Fatal(err)
	}
	before := testutil.ToFloat64(machineForbiddenTotal.WithLabelValues("gardenio-controller"))

	if src.OnForbidden(ctx, "someone-elses-bearer") {
		t.Fatal("a token that is not ours must not trigger a retry")
	}
	if src.OnForbidden(ctx, "") {
		t.Fatal("empty token must not trigger a retry")
	}

	ep.setToken(fresh)
	if !src.OnForbidden(ctx, old) {
		t.Fatal("403 on our cached token must re-acquire and ask for a retry")
	}
	if got, _ := src.Token(ctx); got != fresh || ep.count() != 2 {
		t.Fatalf("want fresh token after re-acquire, fetches=%d", ep.count())
	}
	// A concurrent caller that sent the pre-refresh token is told to retry
	// without another fetch.
	if !src.OnForbidden(ctx, old) || ep.count() != 2 {
		t.Fatalf("peer with the previous token should retry without a fetch, fetches=%d", ep.count())
	}

	// A genuine deny on the fresh token: rate-limited, no fetch inside the window.
	if src.OnForbidden(ctx, fresh) || ep.count() != 2 {
		t.Fatalf("second 403 inside the rate limit must not re-fetch, fetches=%d", ep.count())
	}
	// Past the window it may try again; Keycloak returns the same token, so no retry.
	*clock = clock.Add(machineForbiddenRefreshMin + time.Second)
	if src.OnForbidden(ctx, fresh) {
		t.Fatal("same token back from Keycloak: retrying cannot help")
	}
	if ep.count() != 3 {
		t.Fatalf("want one more fetch after the window, got %d", ep.count())
	}
	if d := testutil.ToFloat64(machineForbiddenTotal.WithLabelValues("gardenio-controller")) - before; d != 4 {
		t.Fatalf("forbidden counter delta = %v, want 4 (our-token 403s only)", d)
	}
}

func TestMachineTokenSource_OnForbiddenFetchFailureKeepsCache(t *testing.T) {
	old := roleToken(t, "old", "gardenio-viewer")
	ep, src, _ := newForbiddenFixture(t, old)
	ctx := context.Background()
	if _, err := src.Token(ctx); err != nil {
		t.Fatal(err)
	}
	ep.fail.Store(true)
	if src.OnForbidden(ctx, old) {
		t.Fatal("failed re-acquire must not ask for a retry")
	}
	if got, err := src.Token(ctx); err != nil || got != old {
		t.Fatalf("cache must survive a failed re-acquire: %v %v", got, err)
	}
}

// End to end through the client: the daemon denies the role-less token and
// admits the fresh one; one write succeeds after a single transparent retry.
func TestClient_403OnMachineTokenReacquiresAndRetriesOnce(t *testing.T) {
	old := roleToken(t, "old", "gardenio-viewer")
	fresh := roleToken(t, "fresh", "gardenio-viewer", "gardenio-machine-trigger-ops")
	ep, src, _ := newForbiddenFixture(t, old)

	var calls atomic.Int32
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+fresh {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"denied"}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(daemon.Close)
	c, err := New(Config{HTTPAddr: daemon.URL, TokenSource: src})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if _, err := src.Token(context.Background()); err != nil { // cache the stale token
		t.Fatal(err)
	}
	ep.setToken(fresh)
	var out map[string]any
	if err := c.doJSON(context.Background(), http.MethodPatch, "/v1/beads/x", map[string]any{"a": 1}, &out); err != nil {
		t.Fatalf("write must succeed after re-acquire: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("want exactly 2 daemon calls (deny, retry), got %d", calls.Load())
	}

	// A persistent deny does not loop: rate limit stops the second re-acquire.
	ep.setToken(old)
	src.mu.Lock()
	src.token, src.prevToken = old, ""
	src.refreshAt = src.now().Add(time.Hour)
	src.mu.Unlock()
	calls.Store(0)
	err = c.doJSON(context.Background(), http.MethodPatch, "/v1/beads/x", map[string]any{"a": 1}, &out)
	var apiErr *APIError
	if err == nil || !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusForbidden {
		t.Fatalf("want final 403, got %v", err)
	}
	if calls.Load() > 2 {
		t.Fatalf("403 must retry at most once, daemon calls=%d", calls.Load())
	}
}
