package main

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// flakyTagsRouter serves GET /spend/tags. For the first `failCalls` requests it
// returns the failMode (a 5xx, or an empty entry list simulating indexing lag);
// after that it returns `entries`. callCount records total hits. This drives the
// transient-then-recover path the retry loop is meant to handle (ka-0000005130).
func flakyTagsRouter(failCalls int, failMode string, entries []tagSpendEntry, callCount *int) http.Handler {
	var mu sync.Mutex
	calls := 0
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if callCount != nil {
			mu.Lock()
			*callCount = n
			mu.Unlock()
		}
		if r.Header.Get("Authorization") != "Bearer test-master-key" {
			http.Error(w, "missing master key", http.StatusForbidden)
			return
		}
		if n <= failCalls {
			switch failMode {
			case "5xx":
				http.Error(w, "litellm boom", http.StatusBadGateway)
				return
			case "absent":
				_ = json.NewEncoder(w).Encode([]tagSpendEntry{})
				return
			}
		}
		_ = json.NewEncoder(w).Encode(entries)
	})
}

// noWaitReconciler wires r with the retry queue allocated and a no-op sleeper so
// tests exercise backoff/retry logic without wall-clock delay.
func noWaitReconciler(r *sessionSpendReconciler) {
	r.retryQueue = make(chan spendRetryJob, spendRetryQueueSize)
	r.sleep = func(context.Context, time.Duration) bool { return true }
}

func TestReconcileAtClose_TransientErrorEnqueuesRetry(t *testing.T) {
	// Inline attempt hits a 5xx → nothing written, but the session is queued for
	// a background retry rather than abandoned.
	entries := []tagSpendEntry{{IndividualRequestTag: "agent:alpha", LogCount: 20, TotalSpend: 7.75}}
	daemon := &daemonStub{
		beads: []map[string]any{makeAgentBead("crew-acme-crew-alpha", map[string]any{spendBaselineField: "4.25"})},
	}
	r, _ := newTestReconciler(t, daemon, flakyTagsRouter(1, "5xx", entries, nil))
	noWaitReconciler(r)

	bead := &beadsapi.BeadDetail{ID: "crew-acme-crew-alpha", Fields: map[string]string{spendBaselineField: "4.25"}}
	r.reconcileAtClose(context.Background(), bead, "alpha")

	if updates := daemon.getUpdates(); len(updates) != 0 {
		t.Fatalf("inline 5xx must not write; got %d updates", len(updates))
	}
	select {
	case job := <-r.retryQueue:
		if job.beadID != "crew-acme-crew-alpha" || job.attempt != 1 {
			t.Fatalf("unexpected enqueued job: %+v", job)
		}
	default:
		t.Fatal("expected a retry job to be enqueued after inline failure")
	}
}

func TestProcessRetry_RecoversAfterTransientError(t *testing.T) {
	// A 5xx on the inline + first retry, then success: the authoritative
	// total_cost_usd must land via the background path.
	entries := []tagSpendEntry{{IndividualRequestTag: "agent:alpha", LogCount: 20, TotalSpend: 7.75}}
	daemon := &daemonStub{
		beads: []map[string]any{makeAgentBead("crew-acme-crew-alpha", map[string]any{spendBaselineField: "4.25"})},
	}
	r, _ := newTestReconciler(t, daemon, flakyTagsRouter(2, "5xx", entries, nil))
	noWaitReconciler(r)

	// Inline attempt (call 1) fails → enqueues attempt 1.
	bead := &beadsapi.BeadDetail{ID: "crew-acme-crew-alpha", Fields: map[string]string{spendBaselineField: "4.25"}}
	r.reconcileAtClose(context.Background(), bead, "alpha")

	// Drain attempt 1 (call 2, still 5xx) → re-enqueues attempt 2.
	job := <-r.retryQueue
	r.processRetry(context.Background(), job)
	// Drain attempt 2 (call 3, succeeds).
	job = <-r.retryQueue
	r.processRetry(context.Background(), job)

	updates := daemon.getUpdates()
	if len(updates) != 2 {
		t.Fatalf("expected settlement + first-baseline back-fill once recovered, got %d", len(updates))
	}
	if cost := updates[0].Fields["total_cost_usd"].(float64); cost != 3.50 {
		t.Errorf("total_cost_usd = %v, want 3.50", cost)
	}
	if len(r.retryQueue) != 0 {
		t.Errorf("queue should be empty after success, has %d", len(r.retryQueue))
	}
}

func TestProcessRetry_IndexingLagEventuallyLands(t *testing.T) {
	// The tag is absent (indexing lag) on inline + first retry, then appears.
	// The fix's whole point: an absent tag is retried, not treated as terminal.
	//
	// The bead carries a genuinely-captured 0 baseline — the production invariant,
	// since captureBaseline stashes one at spawn (a fresh agent's first session is
	// captured as 0). Without it the baseline-miss guard (ka-0000000046) refuses to
	// attribute, which is correct behavior but not what this retry test exercises.
	entries := []tagSpendEntry{{IndividualRequestTag: "agent:alpha", LogCount: 5, TotalSpend: 2.00}}
	daemon := &daemonStub{
		beads: []map[string]any{makeAgentBead("crew-acme-crew-alpha", map[string]any{spendBaselineField: "0"})},
	}
	r, _ := newTestReconciler(t, daemon, flakyTagsRouter(2, "absent", entries, nil))
	noWaitReconciler(r)

	bead := &beadsapi.BeadDetail{ID: "crew-acme-crew-alpha", Fields: map[string]string{spendBaselineField: "0"}}
	r.reconcileAtClose(context.Background(), bead, "alpha")

	job := <-r.retryQueue
	r.processRetry(context.Background(), job) // attempt 1: still absent → re-enqueue
	job = <-r.retryQueue
	r.processRetry(context.Background(), job) // attempt 2: tag present → lands

	updates := daemon.getUpdates()
	if len(updates) != 2 {
		t.Fatalf("expected settlement + first-baseline back-fill once tag indexed, got %d", len(updates))
	}
	if cost := updates[0].Fields["total_cost_usd"].(float64); cost != 2.00 {
		t.Errorf("total_cost_usd = %v, want 2.00", cost)
	}
}

func TestProcessRetry_GivesUpAfterMaxRetries(t *testing.T) {
	// Tag never appears: the loop must stop after maxRetries and not re-enqueue
	// forever, leaving total_cost_usd untouched (the gb estimate stands).
	daemon := &daemonStub{
		beads: []map[string]any{makeAgentBead("crew-acme-crew-alpha", map[string]any{spendBaselineField: "4.25"})},
	}
	r, _ := newTestReconciler(t, daemon, tagsRouter([]tagSpendEntry{}, nil)) // always absent
	noWaitReconciler(r)
	r.maxRetries = 3

	// Walk the queue manually from attempt 1 until it stops re-enqueuing.
	r.enqueueRetry(spendRetryJob{beadID: "crew-acme-crew-alpha", agentName: "alpha", attempt: 1})
	iterations := 0
	for len(r.retryQueue) > 0 {
		iterations++
		if iterations > 10 {
			t.Fatal("retry loop did not terminate; re-enqueuing past maxRetries")
		}
		job := <-r.retryQueue
		r.processRetry(context.Background(), job)
	}
	// attempts 1,2,3 processed; attempt 3 == maxRetries so no attempt 4.
	if iterations != 3 {
		t.Errorf("processed %d attempts, want 3 (bounded by maxRetries)", iterations)
	}
	if updates := daemon.getUpdates(); len(updates) != 0 {
		t.Errorf("absent tag must never write total_cost_usd, got %d updates", len(updates))
	}
}

func TestEnqueueRetry_NoopWithoutWorker(t *testing.T) {
	// Without a started worker (retryQueue nil) enqueue is a silent no-op — the
	// inline attempt is the only attempt, matching the prior best-effort contract.
	daemon := &daemonStub{}
	r, _ := newTestReconciler(t, daemon, tagsRouter(nil, nil))
	if r.retryQueue != nil {
		t.Fatal("retryQueue should be nil before RunRetryLoop starts")
	}
	r.enqueueRetry(spendRetryJob{beadID: "x", agentName: "y", attempt: 1}) // must not panic
}

func TestReconcileAtClose_HappyPathDoesNotEnqueue(t *testing.T) {
	// When the inline attempt lands, nothing is queued — no wasted retries.
	entries := []tagSpendEntry{{IndividualRequestTag: "agent:alpha", LogCount: 20, TotalSpend: 7.75}}
	daemon := &daemonStub{
		beads: []map[string]any{makeAgentBead("crew-acme-crew-alpha", map[string]any{spendBaselineField: "4.25"})},
	}
	r, _ := newTestReconciler(t, daemon, tagsRouter(entries, nil))
	noWaitReconciler(r)

	bead := &beadsapi.BeadDetail{ID: "crew-acme-crew-alpha", Fields: map[string]string{spendBaselineField: "4.25"}}
	r.reconcileAtClose(context.Background(), bead, "alpha")

	if len(r.retryQueue) != 0 {
		t.Errorf("happy path must not enqueue, queue has %d", len(r.retryQueue))
	}
	if updates := daemon.getUpdates(); len(updates) != 2 {
		t.Fatalf("expected inline settlement + first-baseline back-fill, got %d", len(updates))
	}
}

func TestBackoff_ExponentialCapped(t *testing.T) {
	r := &sessionSpendReconciler{retryBase: 10 * time.Second, retryMax: 60 * time.Second}
	cases := map[int]time.Duration{
		0: 10 * time.Second, // clamped to attempt 1
		1: 10 * time.Second,
		2: 20 * time.Second,
		3: 40 * time.Second,
		4: 60 * time.Second, // 80s capped to 60s
		9: 60 * time.Second, // stays capped
	}
	for attempt, want := range cases {
		if got := r.backoff(attempt); got != want {
			t.Errorf("backoff(%d) = %v, want %v", attempt, got, want)
		}
	}
}

func TestProcessRetry_ContextCancelDuringBackoff(t *testing.T) {
	// A cancelled context during the backoff sleep aborts the attempt cleanly:
	// no query, no write, no re-enqueue.
	var queried int
	daemon := &daemonStub{
		beads: []map[string]any{makeAgentBead("crew-acme-crew-alpha", nil)},
	}
	r, _ := newTestReconciler(t, daemon, tagsRouter([]tagSpendEntry{}, &queried))
	r.retryQueue = make(chan spendRetryJob, spendRetryQueueSize)
	r.sleep = func(context.Context, time.Duration) bool { return false } // simulate cancel

	r.processRetry(context.Background(), spendRetryJob{beadID: "crew-acme-crew-alpha", agentName: "alpha", attempt: 1})

	if queried != 0 {
		t.Errorf("query must not run after cancelled backoff, ran %d times", queried)
	}
	if len(r.retryQueue) != 0 {
		t.Errorf("cancelled backoff must not re-enqueue, queue has %d", len(r.retryQueue))
	}
}

func TestRunRetryLoop_DrainsAndStopsOnCancel(t *testing.T) {
	// End-to-end through the goroutine worker: a queued transient job recovers,
	// then the loop exits on context cancel.
	entries := []tagSpendEntry{{IndividualRequestTag: "agent:alpha", LogCount: 5, TotalSpend: 2.00}}
	daemon := &daemonStub{
		// Genuinely-captured 0 baseline (production invariant: captureBaseline
		// stashes one at spawn). processRetry re-fetches the bead each attempt, so
		// the baseline must live on the daemon-side bead for the worker to attribute.
		beads: []map[string]any{makeAgentBead("crew-acme-crew-alpha", map[string]any{spendBaselineField: "0"})},
	}
	// Signal when the worker's PATCH lands so the test synchronizes on the write
	// event itself rather than polling a fixed wall-clock deadline — the latter
	// flakes under CI load when the worker goroutine is scheduled late
	// (gd-OjJRI6Fqug). Set before the worker starts; the buffered channel + drop
	// keeps the notify non-blocking even if the daemon writes more than once.
	wrote := make(chan struct{}, 1)
	daemon.onUpdate = func() {
		select {
		case wrote <- struct{}{}:
		default:
		}
	}

	r, _ := newTestReconciler(t, daemon, flakyTagsRouter(1, "absent", entries, nil))
	r.sleep = func(context.Context, time.Duration) bool { return true }
	// Allocate the queue before starting the worker so the enqueue below races
	// neither the worker's lazy alloc nor itself (RunRetryLoop reuses a non-nil
	// queue).
	r.retryQueue = make(chan spendRetryJob, spendRetryQueueSize)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go r.RunRetryLoop(ctx)
	r.enqueueRetry(spendRetryJob{beadID: "crew-acme-crew-alpha", agentName: "alpha", attempt: 1})

	// Block until the write lands (attempt 1 absent → re-enqueue → attempt 2
	// lands). The 10s timeout is a hang-guard, not a race-prone deadline: the
	// worker signals the instant it writes, so a healthy run returns immediately
	// and only a genuinely stuck worker waits out the clock.
	select {
	case <-wrote:
	case <-time.After(10 * time.Second):
		t.Fatal("worker did not write within 10s (expected 1 write via worker)")
	}
	if got := len(daemon.getUpdates()); got != 1 {
		t.Fatalf("expected 1 write via worker, got %d", got)
	}
}
