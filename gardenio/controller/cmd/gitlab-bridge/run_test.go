// Startup/shutdown wiring tests for run() — the testable core extracted from
// main(). These exercise the full subsystem wiring (beads client, state manager,
// HTTP + metrics listeners, pollers, SSE stream, trigger engine) without a live
// GitLab or beads daemon: the config points the daemon at an unreachable address
// and the listeners at ephemeral ports, so every goroutine starts, fails
// gracefully against the dead backend, and run() returns once ctx is cancelled.
package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// freePorts returns n addresses of the form "127.0.0.1:<port>" that were free at
// call time. There is an inherent TOCTOU race (the port could be taken before
// the server binds), but for a localhost test that runs in milliseconds it is
// reliable enough — and run() tolerates a bind failure anyway (it logs and the
// goroutine exits without taking the process down).
func freePorts(t *testing.T, n int) []string {
	t.Helper()
	addrs := make([]string, 0, n)
	for range n {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("reserve free port: %v", err)
		}
		addrs = append(addrs, l.Addr().String())
		_ = l.Close()
	}
	return addrs
}

// runTestConfig builds a config that wires every subsystem at ephemeral ports
// with the beads daemon pointed at an unreachable address, so run() exercises
// the full startup path but never reaches a real backend.
func runTestConfig(t *testing.T, beadsAddr string) *config {
	t.Helper()
	ports := freePorts(t, 2)
	return &config{
		beadsHTTPAddr:            beadsAddr,
		listenAddr:               ports[0],
		metricsAddr:              ports[1],
		logLevel:                 "error",
		statePath:                filepath.Join(t.TempDir(), "state.json"),
		pollInterval:             time.Hour,
		triggerPollInterval:      time.Hour,
		mrBacklinkInterval:       time.Hour,
		mrBacklinkWindow:         24 * time.Hour,
		upstreamBackstopInterval: time.Hour,
		upstreamBackstopLabel:    "upstream:true",
		trustMetricsInterval:     time.Hour,
		trustMetricsWindow:       24 * time.Hour,
		trustMetricsBranches:     []string{"main"},
		mentionTrigger:           "@gardenio",
		mentionProject:           "gardenio",
	}
}

// newDeadBeadsServer returns an httptest server that immediately closes every
// connection, simulating an unreachable/erroring beads daemon. Using a real
// (but failing) server keeps run()'s startup deterministic — the beads client
// constructor succeeds, and the subsequent calls (EnsureConfigs, userDir
// refresh, SSE connect) all fail gracefully.
func newDeadBeadsServer(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestRun_StartsAndShutsDownCleanly is the core wiring smoke test. It runs the
// full service against a dead beads backend with an already-armed short-lived
// context and asserts run() returns nil once ctx is cancelled (graceful
// shutdown path). This covers the bulk of the startup wiring + the shutdown
// block in run().
func TestRun_StartsAndShutsDownCleanly(t *testing.T) {
	cfg := runTestConfig(t, newDeadBeadsServer(t))

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- run(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned error, want nil: %v", err)
		}
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("run did not return within 5s of ctx timeout")
	}
}

// TestRun_AllOptionalSubsystemsEnabled flips on the opt-in subsystems
// (upstream backstop poller + reconciler, trust metrics, AI approval, slack
// notifier, MR-backlink file-unlinked) so their startup branches in run() are
// exercised too. A non-zero group_id is required for the trust-metrics and
// backlinker branches.
func TestRun_AllOptionalSubsystemsEnabled(t *testing.T) {
	cfg := runTestConfig(t, newDeadBeadsServer(t))
	cfg.gitlabGroupID = 4242
	cfg.gitlabBaseURL = "https://gitlab.example.invalid"
	cfg.upstreamBackstopEnabled = true
	cfg.trustMetricsEnabled = true
	cfg.aiApprovalEnabled = true
	cfg.aiApprovalRuleName = "AI review"
	cfg.aiApprovalRequired = 2
	cfg.aiApprovalBotUsernames = []string{"gardenio-bot"}
	cfg.aiApprovalReviewerGroupIDs = []int{7}
	cfg.slackBotToken = "xoxb-test"
	cfg.slackChannel = "#ops"
	cfg.mrBacklinkFileUnlinked = true

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- run(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()

	select {
	case err := <-done:
		// The trust-metrics collector registers gauges on the process-global
		// prometheus.DefaultRegisterer, which cannot be unregistered. On a fresh
		// process (the CI default, -count=1) the first call wires every subsystem
		// and returns nil; a repeat invocation in the same process (-count>1)
		// returns the duplicate-registration error before completing wiring.
		// Tolerate that one global-registry artifact so the test is stable under
		// reruns — any other error is a real failure.
		if err != nil && !strings.Contains(err.Error(), "duplicate metrics collector registration") {
			t.Fatalf("run with optional subsystems returned unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("run did not return within 5s of ctx timeout")
	}
}

// TestRun_PreCancelledContext verifies run() returns promptly (and nil) when ctx
// is already cancelled before the call — the goroutines spin up and the <-ctx
// .Done() returns immediately, driving straight to the graceful shutdown block.
func TestRun_PreCancelledContext(t *testing.T) {
	cfg := runTestConfig(t, newDeadBeadsServer(t))

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already done

	done := make(chan error, 1)
	go func() {
		done <- run(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run with pre-cancelled ctx returned error, want nil: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run with pre-cancelled ctx did not return within 5s")
	}
}

// TestRun_InvalidBeadsAddrReturnsError covers the fatal setup-error path: an
// empty beads HTTP address makes beadsapi.New fail, so run must return a non-nil
// error rather than proceeding.
func TestRun_InvalidBeadsAddrReturnsError(t *testing.T) {
	cfg := runTestConfig(t, "")
	cfg.beadsHTTPAddr = "" // beadsapi.New requires a non-empty HTTPAddr

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	err := run(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil {
		t.Fatal("run with empty beads addr returned nil, want error")
	}
}

// TestBuildMetricsServer verifies the internal metrics server is constructed
// with the requested address, a non-nil handler, and that the handler serves
// the Prometheus /metrics endpoint off the default gatherer.
func TestBuildMetricsServer(t *testing.T) {
	srv := buildMetricsServer(":9092")
	if srv == nil {
		t.Fatal("buildMetricsServer returned nil")
	}
	if srv.Addr != ":9092" {
		t.Errorf("Addr = %q, want :9092", srv.Addr)
	}
	if srv.Handler == nil {
		t.Fatal("metrics server handler is nil")
	}
	if srv.ReadHeaderTimeout != 5*time.Second {
		t.Errorf("ReadHeaderTimeout = %v, want 5s", srv.ReadHeaderTimeout)
	}

	// Exercise the handler: GET /metrics should return 200 with the standard
	// Prometheus text exposition content type.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	srv.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct == "" {
		t.Error("GET /metrics returned empty Content-Type")
	}
}
