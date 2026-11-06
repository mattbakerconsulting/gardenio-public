package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"gardenio/controller/internal/coopapi"
)

// A login banner at startup must surface as the errStartupLoginBanner sentinel
// so the lifecycle can classify a credential-not-ready death and feed the
// startup-login failure budget (ka-0000007536). Also asserts bypassStartup
// requested a coop shutdown.
func TestBypassStartup_LoginBannerReturnsSentinel(t *testing.T) {
	var shutdownCalled atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agent":
			json.NewEncoder(w).Encode(coopapi.AgentState{State: "starting"})
		case "/api/v1/screen/text":
			_, _ = w.Write([]byte("Not logged in · Please run /login"))
		case "/api/v1/shutdown":
			shutdownCalled.Store(true)
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	coop := coopapi.New(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := bypassStartup(ctx, coop, false)
	if err != errStartupLoginBanner {
		t.Fatalf("bypassStartup on a login banner returned %v, want errStartupLoginBanner", err)
	}
	if !shutdownCalled.Load() {
		t.Fatal("bypassStartup did not request coop shutdown on the login banner")
	}
}

// The "resume failed" startup death must NOT be misclassified as a login block
// (it returns a different error), so it never counts toward the login budget.
func TestBypassStartup_ResumeFailedIsNotLoginBanner(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agent":
			json.NewEncoder(w).Encode(coopapi.AgentState{State: "starting"})
		case "/api/v1/screen/text":
			_, _ = w.Write([]byte("No conversation found"))
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	coop := coopapi.New(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := bypassStartup(ctx, coop, false)
	if err == errStartupLoginBanner {
		t.Fatal("a resume failure was misclassified as errStartupLoginBanner")
	}
	if err == nil {
		t.Fatal("expected a resume-failed error, got nil")
	}
}

// The core wedge fix: once the lifecycle goroutine returns, if coop has not
// exited within the grace window, the watchdog forces the session context
// closed (which the caller wires to SIGTERM→SIGKILL). Without this, a coop that
// ignored coop.Shutdown() would block coopCmd.Wait() forever and freeze the
// restart loop (ka-0000007536).
func TestForceTeardown_ForcesAfterGrace(t *testing.T) {
	prev := sessionForceTeardownGrace
	sessionForceTeardownGrace = 20 * time.Millisecond
	t.Cleanup(func() { sessionForceTeardownGrace = prev })

	sessionCtx, sessionCancel := context.WithCancel(context.Background())
	defer sessionCancel()

	lifecycleDone := make(chan struct{})
	go forceTeardownAfterLifecycle(sessionCtx, lifecycleDone, sessionCancel)

	// Lifecycle ends (dead session, coop.Shutdown fired) but coop stays up:
	// sessionCtx is NOT cancelled by the caller because coopCmd.Wait() is still
	// blocked. The watchdog must force the teardown.
	close(lifecycleDone)

	select {
	case <-sessionCtx.Done():
		// forced — good
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog did not force session teardown after the grace window")
	}
}

// When coop exits on its own after the lifecycle ends, the caller cancels the
// session context (defer sessionCancel / explicit cancel after Wait). The
// watchdog must observe that and return WITHOUT sending a redundant force-kill
// signal of its own — i.e. it must not fire after the session already ended.
func TestForceTeardown_NoForceWhenSessionEndsFirst(t *testing.T) {
	prev := sessionForceTeardownGrace
	sessionForceTeardownGrace = time.Hour // long: only the Done branch should win
	t.Cleanup(func() { sessionForceTeardownGrace = prev })

	sessionCtx, realCancel := context.WithCancel(context.Background())

	var forced atomic.Bool
	wrappedCancel := func() {
		forced.Store(true)
		realCancel()
	}

	lifecycleDone := make(chan struct{})
	done := make(chan struct{})
	go func() {
		forceTeardownAfterLifecycle(sessionCtx, lifecycleDone, wrappedCancel)
		close(done)
	}()

	// Lifecycle finished (coop exited), and the caller cancels the session
	// context — the normal, healthy path.
	close(lifecycleDone)
	realCancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog did not return after the session context was cancelled")
	}
	if forced.Load() {
		t.Fatal("watchdog force-cancelled the session even though it ended on its own")
	}
}

// If the session context is already done when the lifecycle returns (e.g. pod
// SIGTERM), the watchdog must return immediately without forcing.
func TestForceTeardown_NoForceWhenAlreadyCancelled(t *testing.T) {
	prev := sessionForceTeardownGrace
	sessionForceTeardownGrace = time.Hour
	t.Cleanup(func() { sessionForceTeardownGrace = prev })

	sessionCtx, realCancel := context.WithCancel(context.Background())
	realCancel() // already cancelled

	var forced atomic.Bool
	wrappedCancel := func() { forced.Store(true); realCancel() }

	lifecycleDone := make(chan struct{})
	close(lifecycleDone)

	done := make(chan struct{})
	go func() {
		forceTeardownAfterLifecycle(sessionCtx, lifecycleDone, wrappedCancel)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog did not return when the session context was already cancelled")
	}
	if forced.Load() {
		t.Fatal("watchdog force-cancelled an already-cancelled session")
	}
}
