package bridge

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// thawStatusServer is a stub controller exposing GET /api/v1/thaw/status. It
// reports thawing per the supplied func so tests can model a thaw that releases
// after N polls. It also serves the coop nudge endpoint so a delivered buffer
// nudge can be observed end-to-end.
type thawStatusServer struct {
	thawing    func(agent string) bool
	nudged     atomic.Bool
	statusHits atomic.Int32
}

func newThawStatusServer(thawing func(agent string) bool) (*httptest.Server, *thawStatusServer) {
	s := &thawStatusServer{thawing: thawing}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/thaw/status", func(w http.ResponseWriter, r *http.Request) {
		s.statusHits.Add(1)
		agent := r.URL.Query().Get("agent")
		_ = json.NewEncoder(w).Encode(map[string]any{"agent": agent, "thawing": s.thawing(agent)})
	})
	// coop nudge endpoint (the agent's coop_url points here once "live").
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		s.nudged.Store(true)
		_ = json.NewEncoder(w).Encode(map[string]bool{"delivered": true})
	})
	return httptest.NewServer(mux), s
}

// TestIsAgentThawing covers the GET /api/v1/thaw/status probe (ka-0000005830).
func TestIsAgentThawing(t *testing.T) {
	t.Run("no controllerURL is a no-op (false)", func(t *testing.T) {
		b := &Bot{logger: slog.Default()}
		if b.isAgentThawing(context.Background(), "a") {
			t.Error("want false when controllerURL unset")
		}
	})

	t.Run("controller reports thawing=true", func(t *testing.T) {
		srv, _ := newThawStatusServer(func(string) bool { return true })
		defer srv.Close()
		b := &Bot{logger: slog.Default(), controllerURL: srv.URL}
		if !b.isAgentThawing(context.Background(), "agentsim") {
			t.Error("want true when controller reports thawing")
		}
	})

	t.Run("controller reports thawing=false", func(t *testing.T) {
		srv, _ := newThawStatusServer(func(string) bool { return false })
		defer srv.Close()
		b := &Bot{logger: slog.Default(), controllerURL: srv.URL}
		if b.isAgentThawing(context.Background(), "agentsim") {
			t.Error("want false when controller reports not thawing")
		}
	})

	t.Run("unreachable controller is a no-op (false)", func(t *testing.T) {
		b := &Bot{logger: slog.Default(), controllerURL: "http://127.0.0.1:1"}
		if b.isAgentThawing(context.Background(), "agentsim") {
			t.Error("want false when controller is unreachable (route as before)")
		}
	})
}

// TestWaitForThawAndNudge_BuffersThenDelivers is the BUFFER acceptance (design
// ka-0000005125, e2e scenario 1): a nudge arriving mid-thaw is held until the
// thaw releases (thawing flips false AND coop_url is present), then delivered to
// the freshly-rehydrated agent — exactly one delivery, no respawn.
func TestWaitForThawAndNudge_BuffersThenDelivers(t *testing.T) {
	defer withFastThawBuffer(t)()

	var polls atomic.Int32
	srv, stub := newThawStatusServer(func(string) bool {
		// Thawing for the first 2 polls, then released.
		return polls.Add(1) <= 2
	})
	defer srv.Close()

	daemon := newMockDaemon()
	// The agent bead gains its coop_url (it is the rehydrated Pod B pointing at
	// the stub's nudge endpoint) — present from the start; the gate is `thawing`.
	daemon.beads["agentsim"] = &beadsapi.BeadDetail{
		ID:     "agentsim",
		Notes:  "coop_url: " + srv.URL,
		Fields: map[string]string{"agent_state": "running"},
	}
	b := &Bot{logger: slog.Default(), controllerURL: srv.URL, daemon: daemon}

	delivered := b.waitForThawAndNudge(context.Background(), "agentsim", "hello", "gd-BEAD")
	if !delivered {
		t.Fatal("want delivered=true once thaw releases")
	}
	if !stub.nudged.Load() {
		t.Error("nudge was never delivered to the rehydrated agent")
	}
	// It must have polled at least until the thaw released (3rd poll).
	if got := stub.statusHits.Load(); got < 3 {
		t.Errorf("status polls = %d, want >=3 (buffer should poll until release)", got)
	}
}

// TestWaitForThawAndNudge_DeadlineTimeout is the wedged-thaw acceptance (e2e
// scenario 4): a thaw that never releases must hit the buffer deadline and
// return false so the caller takes the existing fallback — never an infinite
// "Resuming…".
func TestWaitForThawAndNudge_DeadlineTimeout(t *testing.T) {
	defer withFastThawBuffer(t)()

	srv, _ := newThawStatusServer(func(string) bool { return true }) // never releases
	defer srv.Close()
	daemon := newMockDaemon()
	daemon.beads["agentsim"] = &beadsapi.BeadDetail{
		ID:    "agentsim",
		Notes: "coop_url: " + srv.URL,
	}
	b := &Bot{logger: slog.Default(), controllerURL: srv.URL, daemon: daemon}

	delivered := b.waitForThawAndNudge(context.Background(), "agentsim", "hello", "gd-BEAD")
	if delivered {
		t.Error("want delivered=false when the thaw never releases (deadline must fire)")
	}
}

// TestWaitForThawAndNudge_FailClosedTerminal is the fail-closed acceptance (e2e
// scenario 3): the thaw releases but the agent ended up terminal (rehydrate
// fail-closed past a recoverable cold start). The buffer must defer to the
// caller's fallback (return false) rather than nudge a dead pod.
func TestWaitForThawAndNudge_FailClosedTerminal(t *testing.T) {
	defer withFastThawBuffer(t)()

	srv, stub := newThawStatusServer(func(string) bool { return false }) // released immediately
	defer srv.Close()
	daemon := newMockDaemon()
	daemon.beads["agentsim"] = &beadsapi.BeadDetail{
		ID:     "agentsim",
		Notes:  "coop_url: " + srv.URL,
		Fields: map[string]string{"agent_state": "failed"}, // terminal
	}
	b := &Bot{logger: slog.Default(), controllerURL: srv.URL, daemon: daemon}

	delivered := b.waitForThawAndNudge(context.Background(), "agentsim", "hello", "gd-BEAD")
	if delivered {
		t.Error("want delivered=false when the agent is terminal post-thaw")
	}
	if stub.nudged.Load() {
		t.Error("must not nudge a terminal agent")
	}
}

// TestWaitForThawAndNudge_CtxCancel verifies the buffer loop honours ctx
// cancellation (e.g. bridge shutdown) and returns promptly without delivering.
func TestWaitForThawAndNudge_CtxCancel(t *testing.T) {
	defer withFastThawBuffer(t)()

	srv, _ := newThawStatusServer(func(string) bool { return true })
	defer srv.Close()
	daemon := newMockDaemon()
	daemon.beads["agentsim"] = &beadsapi.BeadDetail{ID: "agentsim", Notes: "coop_url: " + srv.URL}
	b := &Bot{logger: slog.Default(), controllerURL: srv.URL, daemon: daemon}

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	var delivered bool
	go func() {
		defer wg.Done()
		delivered = b.waitForThawAndNudge(ctx, "agentsim", "hello", "gd-BEAD")
	}()
	cancel()
	wg.Wait()
	if delivered {
		t.Error("want delivered=false on ctx cancel")
	}
}

// withFastThawBuffer shrinks the buffer poll interval + deadline for tests and
// restores them on cleanup.
func withFastThawBuffer(t *testing.T) func() {
	t.Helper()
	origPoll, origDeadline := thawBufferPollInterval, thawBufferDeadline
	thawBufferPollInterval = 5 * time.Millisecond
	thawBufferDeadline = 200 * time.Millisecond
	return func() {
		thawBufferPollInterval = origPoll
		thawBufferDeadline = origDeadline
	}
}
