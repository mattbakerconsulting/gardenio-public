package beadsapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestBatchGetBeads_ShardsRunConcurrently is the regression guard for
// ka-0000000376: the sharded batch helpers must dispatch their shards
// CONCURRENTLY, not in a serial loop.
//
// It asserts the property rather than the timing: the handler blocks until it
// has seen at least two shards in flight simultaneously, so a serial
// implementation deadlocks (and fails on the context deadline) while a
// concurrent one completes immediately. That makes the test meaningful without
// making it a flaky wall-clock assertion.
func TestBatchGetBeads_ShardsRunConcurrently(t *testing.T) {
	const wantOverlap = 2

	var mu sync.Mutex
	inFlight := 0
	reachedOverlap := make(chan struct{})
	var overlapOnce sync.Once

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		inFlight++
		if inFlight >= wantOverlap {
			overlapOnce.Do(func() { close(reachedOverlap) })
		}
		mu.Unlock()

		// Hold the request open until the required overlap is observed. A
		// serial client never gets here with 2 in flight, so it blocks until
		// the request context is cancelled by the deadline below.
		select {
		case <-reachedOverlap:
		case <-r.Context().Done():
		}

		mu.Lock()
		inFlight--
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client(), batchShardLimiter: newBatchShardLimiter()}

	// Enough IDs for several shards, but comfortably within the concurrency cap
	// so the required overlap is actually reachable.
	ids := make([]string, BatchGetBeadsMaxIDs*wantOverlap)
	for i := range ids {
		ids[i] = fmt.Sprintf("gd-%04d", i)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := c.BatchGetBeads(ctx, ids); err != nil {
		t.Fatalf("BatchGetBeads: %v (shards did not overlap — are they running serially?)", err)
	}

	select {
	case <-reachedOverlap:
	default:
		t.Fatalf("shards never overlapped: want >= %d concurrent shard requests", wantOverlap)
	}
}

// TestForEachShard_RespectsClientWideLimit verifies the shard limiter is shared
// across concurrent calls on one Client, not scoped per call. A per-call
// limiter would let N concurrent joins put N*batchShardConcurrency requests on
// the daemon at once — the over-subscription that measured SLOWER than a
// correctly shared cap.
func TestForEachShard_RespectsClientWideLimit(t *testing.T) {
	c := &Client{batchShardLimiter: newBatchShardLimiter()}

	var mu sync.Mutex
	inFlight, maxInFlight := 0, 0

	work := func(context.Context, []string) error {
		mu.Lock()
		inFlight++
		maxInFlight = max(maxInFlight, inFlight)
		mu.Unlock()
		time.Sleep(2 * time.Millisecond)
		mu.Lock()
		inFlight--
		mu.Unlock()
		return nil
	}

	ids := make([]string, 10*batchShardConcurrency)
	for i := range ids {
		ids[i] = fmt.Sprintf("gd-%04d", i)
	}

	// Several joins in flight at once, exactly like the /plans enrich waves.
	const joins = 4
	var wg sync.WaitGroup
	for range joins {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.forEachShard(context.Background(), ids, 1, work); err != nil {
				t.Errorf("forEachShard: %v", err)
			}
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if maxInFlight > batchShardConcurrency {
		t.Errorf("max concurrent shards = %d, want <= %d (limiter is per-call, not client-wide)",
			maxInFlight, batchShardConcurrency)
	}
}

// TestForEachShard_PropagatesShardError verifies the concurrent fan-out kept
// the serial loop's failure semantics: one shard's error fails the whole batch.
func TestForEachShard_PropagatesShardError(t *testing.T) {
	c := &Client{batchShardLimiter: newBatchShardLimiter()}
	sentinel := errors.New("shard boom")

	var calls atomic.Int64
	ids := make([]string, 20)
	for i := range ids {
		ids[i] = fmt.Sprintf("gd-%04d", i)
	}

	err := c.forEachShard(context.Background(), ids, 1, func(_ context.Context, shard []string) error {
		if calls.Add(1) == 3 {
			return sentinel
		}
		return nil
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("forEachShard err = %v, want %v", err, sentinel)
	}
}

// TestForEachShard_SingleShardSkipsFanOut pins the fast path: a call that fits
// in one shard must not pay for the errgroup/semaphore machinery.
func TestForEachShard_SingleShardSkipsFanOut(t *testing.T) {
	c := &Client{batchShardLimiter: newBatchShardLimiter()}
	var calls int
	err := c.forEachShard(context.Background(), []string{"gd-A", "gd-B"}, 50, func(_ context.Context, shard []string) error {
		calls++
		if len(shard) != 2 {
			t.Errorf("shard len = %d, want 2", len(shard))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("forEachShard: %v", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
}

// TestForEachShard_CancellationIsNotSilentSuccess guards a bug found in review
// of this very file: on caller cancellation, forEachShard used to return nil
// after running only some shards.
//
// Every shard that HAD run returned nil, so errgroup.Wait reported success —
// but the launch loop had broken out early on a failed semaphore Acquire. The
// batch helpers merge shard results into one map, so the caller received a
// silently truncated map that is indistinguishable from "those beads do not
// exist". That is the worst shape of failure for this code: a page renders,
// with rows quietly missing, and nothing logs.
func TestForEachShard_CancellationIsNotSilentSuccess(t *testing.T) {
	c := &Client{batchShardLimiter: newBatchShardLimiter()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ids := make([]string, 40)
	for i := range ids {
		ids[i] = fmt.Sprintf("gd-%04d", i)
	}

	var ran atomic.Int64
	// One shard per id, so there are far more shards than the concurrency cap
	// and the launch loop is still running when the cancel lands.
	err := c.forEachShard(ctx, ids, 1, func(_ context.Context, _ []string) error {
		if ran.Add(1) == 3 {
			cancel()
		}
		return nil // every shard that runs SUCCEEDS
	})

	if ran.Load() >= int64(len(ids)) {
		t.Skip("all shards ran before cancellation took effect; nothing to assert")
	}
	if err == nil {
		t.Fatalf("ran only %d/%d shards but returned nil — caller cannot tell the result is partial",
			ran.Load(), len(ids))
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to wrap context.Canceled", err)
	}
}
