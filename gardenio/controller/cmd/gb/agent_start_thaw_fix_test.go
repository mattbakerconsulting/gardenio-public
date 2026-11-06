package main

// agent_start_thaw_fix_test.go — RED→GREEN proofs for the gb thaw
// rehydrate-path fixes (ka-0000001637):
//
//   - RANK3 (TestBeadsAPI_CloseIdleConnections_DropsKeepAlivePool): the daemon
//     HTTP client's keep-alive pool is evicted on demand so a thaw never reuses
//     a pre-freeze conn bound to Pod-A's dead 4-tuple. RED on unmodified
//     source: the client wraps an otelhttp.Transport that does NOT forward
//     CloseIdleConnections, so http.Client.CloseIdleConnections is a silent
//     no-op and the next request reuses the warm idle conn. GREEN after this
//     branch's fix: the Client owns its base http.Transport directly and
//     evicts it via Client.CloseIdleConnections.
//
//   - RANK4 (TestRetryUntilDeadline_PerAttemptConnectClamp_AllowsManyProbes):
//     a thaw-retry loop facing a black-holed peer makes many fast attempts
//     instead of burning the whole overall deadline on one hung connect. RED
//     on unmodified source: retryUntilDeadline only wrapped the parent ctx,
//     so each attempt ate the full remaining deadline. GREEN after this
//     branch's fix: each attempt is clamped to thawConnectTimeout (default 2s)
//     under a fresh per-attempt context (which is also what triggers the live
//     DNS re-resolve, since Go's resolver runs under the request's ctx).
//
//   - Negative invariant (TestGBThawHandler_HappyPathHeartbeat_StillFast):
//     a happy-path heartbeat to a healthy listener still completes well
//     under 5s — the per-attempt clamp does not regress the warm path.

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/thawepoch"
)

// connCountingListener wraps a net.Listener and counts every accepted
// connection — the only reliable proof that the client dialed a fresh
// TCP/4-tuple instead of reusing a pooled keep-alive conn (a reused conn
// generates zero new Accepts on the listener side).
type connCountingListener struct {
	net.Listener
	count atomic.Int64
}

func (l *connCountingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.count.Add(1)
	}
	return c, err
}

// newCountingHTTPServer stands up an httptest.Server whose ONLY purpose is
// to count distinct accepted connections. Replies 200 OK to anything so the
// beadsapi Client treats the call as a successful daemon round-trip.
func newCountingHTTPServer(t *testing.T) (srv *httptest.Server, listener *connCountingListener) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	listener = &connCountingListener{Listener: ln}
	srv = &httptest.Server{
		Listener: listener,
		Config: &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Any 2xx will do; the body and shape don't matter for the
			// keep-alive pool semantics we're probing.
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		})},
	}
	srv.Start()
	t.Cleanup(srv.Close)
	return srv, listener
}

// TestBeadsAPI_CloseIdleConnections_DropsKeepAlivePool primes a beadsapi
// Client with a warm keep-alive connection by issuing one request, then
// evicts the pool via Client.CloseIdleConnections, then issues a SECOND
// request and asserts the listener observed TWO distinct Accepts — i.e. the
// second request dialed fresh instead of reusing the pooled conn.
//
// RED today: http.Client.CloseIdleConnections is a no-op through the
// otelhttp wrap, so the second request rides
// the same keep-alive conn (Accepts stays at 1). The Client did not expose
// any non-no-op eviction surface, so the test could not even compile against
// unmodified source — the type assertion below is the documentation of that
// RED state.
//
// GREEN post-fix: Client.CloseIdleConnections directly evicts the owned
// base *http.Transport, so the second request dials fresh (Accepts == 2).
func TestBeadsAPI_CloseIdleConnections_DropsKeepAlivePool(t *testing.T) {
	srv, ln := newCountingHTTPServer(t)

	client, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}

	// Prime: any successful round-trip leaves the conn idle in the pool. We
	// use EmitHook because that's the post-thaw heartbeat path; any 2xx call
	// would have the same keep-alive effect.
	if _, err := client.EmitHook(context.Background(), beadsapi.EmitHookRequest{
		AgentBeadID: "gd-prime",
		HookType:    "PreToolUse",
	}); err != nil {
		t.Fatalf("prime EmitHook: %v", err)
	}
	if got := ln.count.Load(); got != 1 {
		t.Fatalf("prime: expected 1 accept, got %d", got)
	}

	// Sanity: assert the new public eviction surface exists at the type
	// level — RED on unmodified source where the method does not exist.
	var _ interface{ CloseIdleConnections() } = client

	// Evict the pool. Give the runtime a tick for the connection's reader
	// goroutine to observe the close and drop the conn from the pool.
	client.CloseIdleConnections()
	runtime.Gosched()
	time.Sleep(50 * time.Millisecond)

	// Second round-trip MUST dial fresh now that the pool is empty.
	if _, err := client.EmitHook(context.Background(), beadsapi.EmitHookRequest{
		AgentBeadID: "gd-postevict",
		HookType:    "PreToolUse",
	}); err != nil {
		t.Fatalf("post-evict EmitHook: %v", err)
	}
	if got := ln.count.Load(); got != 2 {
		// RED today (1) — keep-alive reuse survived through the wrap chain.
		// GREEN post-fix (2) — fresh dial after eviction.
		t.Fatalf("post-evict: expected 2 distinct accepts (fresh dial after eviction), got %d", got)
	}
}

// TestRetryUntilDeadline_PerAttemptConnectClamp_AllowsManyProbes simulates a
// black-holed peer (each attempt hangs until either the per-attempt clamp or
// the parent deadline fires). With a 30s overall deadline and a per-attempt
// clamp of ~2s, the loop MUST get many probes in, modeling coop's ~25
// probes vs the pre-fix gb's ~2.
//
// RED today: retryUntilDeadline carried no per-attempt clamp, so a hung
// attempt would burn the FULL ctx deadline; with a single 30s deadline, the
// loop completes 1–2 attempts before timing out. We model that by having
// each attempt block on its own ctx.Done() — without the clamp, that ctx is
// the parent (deadline=30s), so the FIRST blocking attempt eats the whole
// budget.
//
// GREEN post-fix: each attempt runs under a clamped child ctx (default 2s);
// a hung attempt aborts at the clamp boundary, the loop retries (with a
// fresh ctx that re-runs DNS resolution), and the test sees ≥4 attempts,
// half of the ~8 the budget fits (gd-v6bWqeYDV9).
func TestRetryUntilDeadline_PerAttemptConnectClamp_AllowsManyProbes(t *testing.T) {
	t.Setenv("GB_THAW_CONNECT_TIMEOUT_MS", "100")

	// Models the prod 30s/2s scenario at 10× speed. The capped-exponential
	// backoff (100ms→1s) dominates the budget once it saturates, so a 6s
	// deadline fits ~7-8 attempts (cumulative ~0.2s, 0.5s, 1.0s, 1.9s, 3.0s,
	// 4.1s, 5.2s, ...). The floor of 4 leaves room for a loaded CI runner;
	// without the clamp the first attempt eats the whole deadline, so 1 attempt
	// still fails it (gd-v6bWqeYDV9).
	const overall = 6 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), overall)
	defer cancel()

	var attempts atomic.Int64
	start := time.Now()
	err := retryUntilDeadline(ctx, func(actx context.Context) error {
		attempts.Add(1)
		// Model a black-holed connect: block until our per-attempt ctx is
		// done. Without the per-attempt clamp this is `ctx.Done()` of the
		// parent (deadline=overall), and the first attempt eats it all.
		<-actx.Done()
		return errors.New("connect: hung (black-hole)")
	})
	elapsed := time.Since(start)
	// Final attempt error must be surfaced (we never turn a hard failure
	// into nil) — but the count, not the err value, is the proof.
	if err == nil {
		t.Fatalf("expected hung-attempt error to surface, got nil after %v", elapsed)
	}
	const minAttempts = 4
	if got := attempts.Load(); got < minAttempts {
		// RED today: per-attempt clamp absent → ~1 attempt eats `overall`.
		// GREEN post-fix: ~7-8 attempts, bounded by the backoff curve.
		t.Fatalf("per-attempt connect clamp not enforced: got %d attempts in %v (want >= %d)", got, elapsed, minAttempts)
	}
}

// TestRetryUntilDeadline_ClampDisableEnvHonored documents the operator
// escape hatch: GB_THAW_CONNECT_TIMEOUT_MS=0 disables the clamp, restoring
// pre-fix semantics. Useful when a future post-mortem needs to bisect
// "clamp on vs clamp off" without a redeploy. Not a RED/GREEN gate — a
// guardrail against silently dropping the env knob in a future refactor.
func TestRetryUntilDeadline_ClampDisableEnvHonored(t *testing.T) {
	t.Setenv("GB_THAW_CONNECT_TIMEOUT_MS", "0")
	if got := thawConnectTimeout(); got != 0 {
		t.Fatalf("env=0 should disable clamp, got %v", got)
	}
	t.Setenv("GB_THAW_CONNECT_TIMEOUT_MS", "1500")
	if got := thawConnectTimeout(); got != 1500*time.Millisecond {
		t.Fatalf("env=1500 should yield 1.5s, got %v", got)
	}
	t.Setenv("GB_THAW_CONNECT_TIMEOUT_MS", "garbage")
	if got := thawConnectTimeout(); got != thawConnectTimeoutDefault {
		t.Fatalf("garbage env should fall back to default %v, got %v", thawConnectTimeoutDefault, got)
	}
	t.Setenv("GB_THAW_CONNECT_TIMEOUT_MS", "")
	if got := thawConnectTimeout(); got != thawConnectTimeoutDefault {
		t.Fatalf("empty env should fall back to default %v, got %v", thawConnectTimeoutDefault, got)
	}
}

// TestRetryUntilDeadline_FreshContextPerAttempt asserts the fix property
// that EACH attempt sees a context distinct from the previous attempt's —
// the property that triggers Go's resolver to run a fresh DNS lookup
// (resolution runs under the request's ctx, so a fresh ctx never carries
// any prior attempt's cached state). RED today: the same parent ctx was
// passed into fn on every iteration. GREEN post-fix: each iteration gets a
// freshly-derived child via withAttemptClamp.
func TestRetryUntilDeadline_FreshContextPerAttempt(t *testing.T) {
	t.Setenv("GB_THAW_CONNECT_TIMEOUT_MS", "100")
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	seen := make(map[context.Context]struct{})
	var attempts int
	_ = retryUntilDeadline(ctx, func(actx context.Context) error {
		seen[actx] = struct{}{}
		attempts++
		if attempts >= 3 {
			return nil // succeed so the loop exits before the deadline
		}
		return errors.New("transient")
	})
	if attempts < 3 {
		t.Fatalf("expected at least 3 attempts, got %d", attempts)
	}
	// Each attempt's ctx must be a distinct value — if the same ctx were
	// reused, the map would have len 1.
	if len(seen) < attempts {
		// RED today: parent ctx reused → len(seen) == 1.
		// GREEN post-fix: fresh child per attempt → len(seen) == attempts.
		t.Fatalf("expected %d distinct per-attempt contexts, got %d", attempts, len(seen))
	}
}

// TestGBThawHandler_HappyPathHeartbeat_StillFast is the negative invariant
// from the brief: when the heartbeat target is healthy, the handler MUST
// still complete well under 5s (no regression on the warm path). Asserts:
//   - the handler returns OK,
//   - the eviction hook (RANK3 fix) fires once,
//   - the heartbeat stage runs and succeeds,
//   - total wall time is well under 5s.
func TestGBThawHandler_HappyPathHeartbeat_StillFast(t *testing.T) {
	t.Setenv("GB_THAW_CONNECT_TIMEOUT_MS", "2000")

	var evictCount atomic.Int64
	var heartbeats atomic.Int64
	h := newGBThawHandler(thawDeps{
		heartbeat: func(context.Context) error {
			heartbeats.Add(1)
			return nil
		},
		evictIdleConns: func() { evictCount.Add(1) },
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	ack, err := h(ctx, thawepoch.Marker{Epoch: 1})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !ack.OK {
		t.Fatalf("expected OK ack on healthy heartbeat, got %+v", ack)
	}
	if evictCount.Load() != 1 {
		t.Fatalf("expected eviction hook to fire exactly once, got %d", evictCount.Load())
	}
	if heartbeats.Load() != 1 {
		t.Fatalf("expected exactly one heartbeat on healthy path, got %d", heartbeats.Load())
	}
	if ack.Stages["heartbeat"] != "ok" {
		t.Fatalf("expected heartbeat ok, got %q", ack.Stages["heartbeat"])
	}
	if ack.Stages["evict_idle_conns"] != "ok" {
		t.Fatalf("expected evict_idle_conns ok stage, got %q", ack.Stages["evict_idle_conns"])
	}
	// Warm-path budget: well under 5s. Stay generous (1s) to avoid
	// scheduler-noise flakes while still proving the per-attempt clamp does
	// not regress the no-failure path.
	if elapsed > time.Second {
		t.Fatalf("happy-path heartbeat regression: took %v (want < 1s)", elapsed)
	}
}

// TestGBThawHandler_EvictsBeforeHeartbeat asserts the ORDERING property
// from the brief: the keep-alive pool eviction MUST happen BEFORE the
// first heartbeat post-thaw, because the whole point is to drop conns
// pinned to Pod-A before the heartbeat can pick one up. Implemented by
// recording the call order via a shared monotonic counter.
func TestGBThawHandler_EvictsBeforeHeartbeat(t *testing.T) {
	var seq atomic.Int64
	var evictAt, heartbeatAt int64

	h := newGBThawHandler(thawDeps{
		evictIdleConns: func() {
			evictAt = seq.Add(1)
		},
		heartbeat: func(context.Context) error {
			heartbeatAt = seq.Add(1)
			return nil
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := h(ctx, thawepoch.Marker{Epoch: 7}); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if evictAt == 0 || heartbeatAt == 0 {
		t.Fatalf("expected both hooks to fire; evictAt=%d heartbeatAt=%d", evictAt, heartbeatAt)
	}
	if !(evictAt < heartbeatAt) {
		t.Fatalf("eviction must precede heartbeat (RANK3): evictAt=%d heartbeatAt=%d", evictAt, heartbeatAt)
	}
}

// TestGBThawHandler_NilEvictIdleConnsIsSkipped asserts the optional-hook
// degradation contract: a thawDeps with no evictIdleConns hook (test wiring
// without a live beadsapi client) does not record an evict_idle_conns stage
// and does not panic. The handler still acks OK on a healthy heartbeat.
func TestGBThawHandler_NilEvictIdleConnsIsSkipped(t *testing.T) {
	h := newGBThawHandler(thawDeps{
		heartbeat: func(context.Context) error { return nil },
		// evictIdleConns left nil on purpose.
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ack, err := h(ctx, thawepoch.Marker{Epoch: 1})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !ack.OK {
		t.Fatalf("expected OK ack, got %+v", ack)
	}
	if _, ok := ack.Stages["evict_idle_conns"]; ok {
		t.Fatalf("expected no evict_idle_conns stage when hook is nil, got %v", ack.Stages)
	}
}

// TestBeadsAPI_CloseIdleConnections_FreshClient is a smoke proof that a
// freshly-constructed Client exposes CloseIdleConnections and tolerates it on
// an empty pool; behavior is exercised end-to-end in the main test above.
func TestBeadsAPI_CloseIdleConnections_FreshClient(t *testing.T) {
	// Smoke: a Client must be constructible AND expose CloseIdleConnections.
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	// MUST NOT panic on a freshly-constructed Client (empty pool).
	c.CloseIdleConnections()
	c.CloseIdleConnections() // idempotent
}

// TestStartK8sGBThawWatcher_ArmsDNSPrewarmForInClusterDaemon is the wiring RED→
// GREEN proof for bug ka-0000000696: on a gVisor pod (COOP_THAW_STATE_DIR set)
// whose daemon points at an IN-CLUSTER beads host, the thaw watcher entry MUST
// arm the daemon client's in-cluster DNS-preresolve at boot so the cached
// ClusterIP survives the freeze and the first post-thaw heartbeat dials the
// literal (zero cold-netns DNS).
//
// RED on unmodified source: startK8sGBThawWatcher never touched the daemon's
// DNS path, so the heartbeat re-resolved the bare in-cluster name on the cold
// post-restore netns → "context deadline exceeded" (the exact prod symptom).
// GREEN post-fix: ThawDNSPrewarmTarget reports enabled=true after wiring.
func TestStartK8sGBThawWatcher_ArmsDNSPrewarmForInClusterDaemon(t *testing.T) {
	// A live in-cluster-looking daemon: 127.0.0.1 listener, but we point the
	// client at a bare service name on the same port so the boot prewarm has a
	// real host to (attempt to) resolve. The prewarm resolve missing is fine —
	// the wiring proof is that the rewrite is ARMED for an in-cluster host.
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: "http://gardenio-beads.gardenio.svc.cluster.local:8080"})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	old := daemon
	daemon = c
	t.Cleanup(func() { daemon = old })

	dir := t.TempDir()
	t.Setenv("COOP_THAW_STATE_DIR", dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	startK8sGBThawWatcher(ctx, "gd-agent", "agent-name", t.TempDir())

	_, _, enabled := daemon.ThawDNSPrewarmTarget()
	if !enabled {
		t.Fatalf("startK8sGBThawWatcher must arm the daemon's in-cluster DNS-preresolve on a gVisor pod (bug ka-0000000696)")
	}
}

// TestStartK8sGBThawWatcher_DoesNotArmForExternalDaemon proves the scope guard
// at the wiring layer: an EXTERNAL daemon URL (public host) is NOT armed — its
// address may be a rotating LB/CDN and the external path already survives the
// thaw (coop's aws_sts:ok), so pinning it would be wrong.
func TestStartK8sGBThawWatcher_DoesNotArmForExternalDaemon(t *testing.T) {
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: "https://gardenio.app.e2e.dev.acme.example/beads"})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	old := daemon
	daemon = c
	t.Cleanup(func() { daemon = old })

	dir := t.TempDir()
	t.Setenv("COOP_THAW_STATE_DIR", dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	startK8sGBThawWatcher(ctx, "gd-agent", "agent-name", t.TempDir())

	if _, _, enabled := daemon.ThawDNSPrewarmTarget(); enabled {
		t.Fatalf("external daemon URL must NOT be armed for DNS-preresolve (no LB/CDN pinning)")
	}
}
