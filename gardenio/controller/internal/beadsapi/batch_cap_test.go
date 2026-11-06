package beadsapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// oldDaemon stands in for a daemon still running the pre-!6159 cap: it rejects
// any batch request wider than maxIDs exactly the way kbeads does, and records
// the shard widths it was actually asked for.
func oldDaemon(t *testing.T, maxIDs int, param string, okBody string) (*httptest.Server, func() []int) {
	t.Helper()
	var mu sync.Mutex
	var widths []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := len(strings.Split(r.URL.Query().Get(param), ","))
		mu.Lock()
		widths = append(widths, n)
		mu.Unlock()
		if n > maxIDs {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, `{"error":"maximum %d IDs per batch request"}`, maxIDs)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(okBody))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []int {
		mu.Lock()
		defer mu.Unlock()
		return append([]int(nil), widths...)
	}
}

func testIDs(n int) []string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("gd-%05d", i)
	}
	return ids
}

// TestBatchGetBeads_SurvivesOldDaemon is the headline guard for ka-0000001212.
//
// Raising BatchGetBeadsMaxIDs to 500 is only safe against a daemon that accepts
// 500. The deploy rule (daemon first) covers the planned rollout, but a daemon
// ROLLBACK re-creates old-daemon/new-client at a moment nobody is thinking
// about the coupling — and BatchGetBeads has ~148 call sites, so a hard failure
// there is a broad outage. The client must therefore degrade to more
// round-trips, never to an error.
func TestBatchGetBeads_SurvivesOldDaemon(t *testing.T) {
	const daemonCap = 50
	srv, widths := oldDaemon(t, daemonCap, "ids", `{"results":[]}`)
	c := &Client{baseURL: srv.URL, httpClient: srv.Client(), batchShardLimiter: newBatchShardLimiter()}

	if _, err := c.BatchGetBeads(context.Background(), testIDs(120)); err != nil {
		t.Fatalf("BatchGetBeads against an old daemon must succeed by narrowing, got: %v", err)
	}
	for _, w := range widths()[1:] {
		if w > daemonCap {
			t.Errorf("after the first rejection every request must fit the daemon cap; got width %d > %d", w, daemonCap)
		}
	}
	if got := c.effectiveBatchIDCap(); got != daemonCap {
		t.Errorf("effective cap = %d, want %d (client should have pinned itself to the daemon's advertised limit)", got, daemonCap)
	}
}

// TestBatchGetBeads_OldDaemonProbesOnlyOnce verifies the narrowing is a
// one-time cost, not a per-call one: a client that re-probed with an oversized
// request on every call would pay a wasted round-trip forever.
func TestBatchGetBeads_OldDaemonProbesOnlyOnce(t *testing.T) {
	const daemonCap = 50
	var rejections atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n := len(strings.Split(r.URL.Query().Get("ids"), ",")); n > daemonCap {
			rejections.Add(1)
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, `{"error":"maximum %d IDs per batch request"}`, daemonCap)
			return
		}
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer srv.Close()
	c := &Client{baseURL: srv.URL, httpClient: srv.Client(), batchShardLimiter: newBatchShardLimiter()}

	for range 3 {
		if _, err := c.BatchGetBeads(context.Background(), testIDs(120)); err != nil {
			t.Fatalf("BatchGetBeads: %v", err)
		}
	}
	// Shards run concurrently, so the single oversized ATTEMPT can produce more
	// than one rejection; what must not happen is a fresh probe per call.
	if n := rejections.Load(); n == 0 || n > 3 {
		t.Errorf("rejections = %d across 3 calls, want a one-time probe (1..3, from the first call's concurrent shards)", n)
	}
}

// TestBatchGetBeads_CapRetryDiscardsPartialResults guards the subtle half of
// the retry: shards from the rejected attempt must not leak into the result.
//
// The fan-out is concurrent, so when one 500-wide shard is rejected, sibling
// shards may already have returned rows. If the retry reused that accumulator,
// those rows would be merged twice — harmless for a map — but worse, an
// accumulator carried across attempts is exactly how a partial result survives
// a failure. Asserted by having the daemon serve DIFFERENT content pre- and
// post-narrowing and requiring only the post-narrowing content to survive.
func TestBatchGetBeads_CapRetryDiscardsPartialResults(t *testing.T) {
	const daemonCap = 50
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ids := strings.Split(r.URL.Query().Get("ids"), ",")
		if len(ids) > daemonCap {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, `{"error":"maximum %d IDs per batch request"}`, daemonCap)
			return
		}
		// Narrow (accepted) requests answer with a marker title.
		var b strings.Builder
		b.WriteString(`{"results":[`)
		for i, id := range ids {
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, `{"id":%q,"bead":{"id":%q,"title":"narrow"}}`, id, id)
		}
		b.WriteString(`]}`)
		_, _ = w.Write([]byte(b.String()))
	}))
	defer srv.Close()
	c := &Client{baseURL: srv.URL, httpClient: srv.Client(), batchShardLimiter: newBatchShardLimiter()}

	ids := testIDs(120)
	out, err := c.BatchGetBeads(context.Background(), ids)
	if err != nil {
		t.Fatalf("BatchGetBeads: %v", err)
	}
	if len(out) != len(ids) {
		t.Fatalf("got %d beads, want all %d — the retry must refetch every id, not just the un-rejected shards", len(out), len(ids))
	}
	for id, b := range out {
		if b.Title != "narrow" {
			t.Fatalf("bead %s came back with title %q, want %q (a result from the discarded attempt leaked through)", id, b.Title, "narrow")
		}
	}
}

// TestBatchGetDependencies_SurvivesOldDaemon covers the second batch-ID surface
// — it shares the cap constant and the ratchet, so it must share the behavior.
func TestBatchGetDependencies_SurvivesOldDaemon(t *testing.T) {
	const daemonCap = 50
	srv, widths := oldDaemon(t, daemonCap, "ids", `{"results":[]}`)
	c := &Client{baseURL: srv.URL, httpClient: srv.Client(), batchShardLimiter: newBatchShardLimiter()}

	if _, err := c.BatchGetDependencies(context.Background(), testIDs(120), "forward"); err != nil {
		t.Fatalf("BatchGetDependencies against an old daemon must succeed by narrowing, got: %v", err)
	}
	for _, w := range widths()[1:] {
		if w > daemonCap {
			t.Errorf("request width %d exceeds daemon cap %d after narrowing", w, daemonCap)
		}
	}
}

// TestBatchCapRatchet_SharedAcrossSurfaces pins that the two batch-ID surfaces
// share one ratchet: whichever one meets the old daemon first, the other must
// not have to rediscover the limit.
func TestBatchCapRatchet_SharedAcrossSurfaces(t *testing.T) {
	const daemonCap = 50
	srv, widths := oldDaemon(t, daemonCap, "ids", `{"results":[]}`)
	c := &Client{baseURL: srv.URL, httpClient: srv.Client(), batchShardLimiter: newBatchShardLimiter()}

	if _, err := c.BatchGetBeads(context.Background(), testIDs(120)); err != nil {
		t.Fatalf("BatchGetBeads: %v", err)
	}
	before := len(widths())
	if _, err := c.BatchGetDependencies(context.Background(), testIDs(120), "forward"); err != nil {
		t.Fatalf("BatchGetDependencies: %v", err)
	}
	for _, w := range widths()[before:] {
		if w > daemonCap {
			t.Errorf("dependencies re-probed at width %d after beads already learned the cap is %d", w, daemonCap)
		}
	}
}

// TestNoteBatchCapRejection_IgnoresUnrelatedErrors: only the over-cap 400 may
// narrow the client. Everything else — including the neighbouring caps that
// move independently — must surface unchanged, or a real error gets swallowed
// into a retry loop.
func TestNoteBatchCapRejection_IgnoresUnrelatedErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{"not found", &APIError{StatusCode: http.StatusNotFound, Message: "bead not found"}},
		{"server error", &APIError{StatusCode: http.StatusInternalServerError, Message: "boom"}},
		{"other 400", &APIError{StatusCode: http.StatusBadRequest, Message: "ids parameter is required"}},
		{"bad direction", &APIError{StatusCode: http.StatusBadRequest, Message: "direction must be 'forward' or 'reverse'"}},
		// The sibling endpoints' caps are NOT this cap and are raised
		// separately; matching them would narrow the wrong surface.
		{"children-by-status cap", &APIError{StatusCode: http.StatusBadRequest, Message: "maximum 100 parent IDs per request"}},
		{"names cap", &APIError{StatusCode: http.StatusBadRequest, Message: "maximum 200 names per batch request"}},
		{"non-api error", fmt.Errorf("dial tcp: connection refused")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Client{}
			if c.noteBatchCapRejection(tt.err, BatchGetBeadsMaxIDs) {
				t.Errorf("%v must not narrow the batch cap", tt.err)
			}
			if got := c.effectiveBatchIDCap(); got != BatchGetBeadsMaxIDs {
				t.Errorf("cap = %d, want it untouched at %d", got, BatchGetBeadsMaxIDs)
			}
		})
	}
}

// TestNoteBatchCapRejection_NeverRaises: the ratchet is one-way. A daemon
// advertising a HIGHER cap than we used must not widen us — that would be a
// request we have no evidence any daemon in the fleet accepts, and during a
// rolling deploy the next request may well hit the older pod.
func TestNoteBatchCapRejection_NeverRaises(t *testing.T) {
	c := &Client{}
	err := &APIError{StatusCode: http.StatusBadRequest, Message: "maximum 5000 IDs per batch request"}
	if c.noteBatchCapRejection(err, 500) {
		t.Error("a cap larger than the one we used must not trigger a retry")
	}
	if got := c.effectiveBatchIDCap(); got != BatchGetBeadsMaxIDs {
		t.Errorf("cap = %d, want %d — the ratchet must never widen", got, BatchGetBeadsMaxIDs)
	}
}

// TestNoteBatchCapRejection_TerminatesOnUnparseableCap: a daemon that rejects
// with a cap we cannot trust must still leave the retry loop terminating. The
// floor (50) is the width every daemon has accepted since the endpoint existed.
func TestNoteBatchCapRejection_TerminatesOnUnparseableCap(t *testing.T) {
	c := &Client{}
	// A number too large for Atoi falls back to the floor rather than looping.
	err := &APIError{StatusCode: http.StatusBadRequest, Message: "maximum 99999999999999999999999 IDs per batch request"}
	if !c.noteBatchCapRejection(err, BatchGetBeadsMaxIDs) {
		t.Fatal("an unparseable cap should still narrow to the floor and retry")
	}
	if got := c.effectiveBatchIDCap(); got != batchIDCapFloor {
		t.Errorf("cap = %d, want the floor %d", got, batchIDCapFloor)
	}
	// And a second identical rejection at the floor must NOT retry again.
	if c.noteBatchCapRejection(err, batchIDCapFloor) {
		t.Error("retrying at the floor after a floor-width rejection would loop forever")
	}
}

// TestWithBatchCapRetry_TerminatesOnPersistentRejection is the liveness guard,
// and it bites: the strictly-decreasing ratchet alone makes this loop
// *terminate*, but a daemon shaving one ID off the cap per rejection would
// converge only after ~500 round-trips — a hang as far as any caller is
// concerned. The retry budget is what makes it fail fast instead.
func TestWithBatchCapRetry_TerminatesOnPersistentRejection(t *testing.T) {
	c := &Client{}
	var attempts int
	err := c.withBatchCapRetry(func(perRequest int) error {
		attempts++
		if attempts > maxBatchCapNarrowings+1 {
			t.Fatalf("withBatchCapRetry ran %d attempts, want at most %d", attempts, maxBatchCapNarrowings+1)
		}
		// A pathological daemon: always claims a cap one below whatever we
		// used, so the ratchet keeps making "progress" without converging.
		return &APIError{
			StatusCode: http.StatusBadRequest,
			Message:    fmt.Sprintf("maximum %d IDs per batch request", max(perRequest-1, 0)),
		}
	})
	if err == nil {
		t.Fatal("persistent rejection must surface as an error, not silent success")
	}
}

// TestWithBatchCapRetry_OneNarrowingIsEnough pins the case that actually
// happens — an old daemon with a fixed cap — at exactly one retry, so the
// budget above is never load-bearing in the real rollout.
func TestWithBatchCapRetry_OneNarrowingIsEnough(t *testing.T) {
	c := &Client{}
	var attempts int
	err := c.withBatchCapRetry(func(perRequest int) error {
		attempts++
		if perRequest > 50 {
			return &APIError{StatusCode: http.StatusBadRequest, Message: "maximum 50 IDs per batch request"}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("withBatchCapRetry: %v", err)
	}
	if attempts != 2 {
		t.Errorf("attempts = %d, want 2 (one probe + one success)", attempts)
	}
}
