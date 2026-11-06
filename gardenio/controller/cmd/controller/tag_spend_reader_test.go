package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// countingTagsServer serves GET /spend/tags with the current total for one tag
// and counts the reads. gate, when non-nil, holds every response until closed,
// so a test can keep a read in flight.
type countingTagsServer struct {
	reads atomic.Int64
	total atomic.Int64 // whole dollars for agent:a
	fail  atomic.Bool
	gate  chan struct{}
}

func (s *countingTagsServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.reads.Add(1)
	if s.gate != nil {
		<-s.gate
	}
	if s.fail.Load() {
		http.Error(w, "statement timeout", http.StatusInternalServerError)
		return
	}
	_ = json.NewEncoder(w).Encode([]tagSpendEntry{{IndividualRequestTag: "agent:a", TotalSpend: float64(s.total.Load())}})
}

// fakeClock is a settable clock for tagSpendReader.now.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newTestTagReader(interval time.Duration) (*tagSpendReader, *fakeClock) {
	clock := &fakeClock{t: time.Date(2026, 10, 31, 18, 0, 0, 0, time.UTC)}
	r := newTagSpendReader("k", interval, quietLogger())
	r.now = clock.now
	return r, clock
}

// spendOf reads baseURL and returns agent:a's total, failing the test on error.
func spendOf(t *testing.T, r *tagSpendReader, ctx context.Context, baseURL string, wait bool) float64 {
	t.Helper()
	entries, err := r.Read(ctx, baseURL, wait)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %+v, want one", entries)
	}
	return entries[0].TotalSpend
}

// waitReads polls until the server has seen want reads (a background refresh
// runs on its own goroutine).
func waitReads(t *testing.T, s *countingTagsServer, want int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for s.reads.Load() < want {
		if time.Now().After(deadline) {
			t.Fatalf("reads = %d, want %d", s.reads.Load(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitIdle polls until no read of baseURL is in flight.
func waitIdle(t *testing.T, r *tagSpendReader, baseURL string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		r.mu.Lock()
		p := r.proxies[baseURL]
		busy := p != nil && p.flight != nil
		r.mu.Unlock()
		if !busy {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("read still in flight")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Concurrent callers on a cold cache share one read (gd-oih6JxjJMM: each
// caller used to start its own full-table scan).
func TestTagSpendReader_ConcurrentCallersShareOneRead(t *testing.T) {
	s := &countingTagsServer{gate: make(chan struct{})}
	s.total.Store(7)
	srv := httptest.NewServer(s)
	defer srv.Close()
	r, _ := newTestTagReader(tagSpendReadInterval)

	var wg sync.WaitGroup
	got := make([]float64, 8)
	errs := make([]error, 8)
	for i := range got {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			entries, err := r.Read(context.Background(), srv.URL, i%2 == 0)
			errs[i] = err
			if err == nil && len(entries) == 1 {
				got[i] = entries[0].TotalSpend
			}
		}(i)
	}
	waitReads(t, s, 1)
	close(s.gate)
	wg.Wait()

	if n := s.reads.Load(); n != 1 {
		t.Errorf("reads = %d, want 1", n)
	}
	for i := range got {
		if errs[i] != nil || got[i] != 7 {
			t.Errorf("caller %d: spend %v err %v, want 7", i, got[i], errs[i])
		}
	}
}

// Inside the interval nothing re-reads; once it is due, wait=true gets the
// fresh total and wait=false gets the old one while a background read
// refreshes it.
func TestTagSpendReader_IntervalAndBackgroundRefresh(t *testing.T) {
	s := &countingTagsServer{}
	s.total.Store(1)
	srv := httptest.NewServer(s)
	defer srv.Close()
	r, clock := newTestTagReader(5 * time.Minute)
	ctx := context.Background()

	if v := spendOf(t, r, ctx, srv.URL, true); v != 1 {
		t.Fatalf("first read = %v, want 1", v)
	}
	s.total.Store(2)
	clock.advance(4 * time.Minute)
	if v := spendOf(t, r, ctx, srv.URL, true); v != 1 {
		t.Errorf("inside the interval = %v, want cached 1", v)
	}
	if n := s.reads.Load(); n != 1 {
		t.Errorf("reads inside the interval = %d, want 1", n)
	}

	clock.advance(time.Minute)
	if v := spendOf(t, r, ctx, srv.URL, true); v != 2 {
		t.Errorf("wait=true when due = %v, want fresh 2", v)
	}

	s.total.Store(3)
	clock.advance(5 * time.Minute)
	if v := spendOf(t, r, ctx, srv.URL, false); v != 2 {
		t.Errorf("wait=false when due = %v, want the old snapshot 2", v)
	}
	waitReads(t, s, 3)
	waitIdle(t, r, srv.URL)
	if v := spendOf(t, r, ctx, srv.URL, false); v != 3 {
		t.Errorf("after the background refresh = %v, want 3", v)
	}
	if n := s.reads.Load(); n != 3 {
		t.Errorf("reads = %d, want 3", n)
	}
}

// A failed read keeps the last good snapshot and is not retried before the
// interval is up; with no snapshot the caller gets the error.
func TestTagSpendReader_FailureIsThrottled(t *testing.T) {
	s := &countingTagsServer{}
	s.fail.Store(true)
	srv := httptest.NewServer(s)
	defer srv.Close()
	r, clock := newTestTagReader(5 * time.Minute)
	ctx := context.Background()

	if _, err := r.Read(ctx, srv.URL, true); err == nil {
		t.Fatal("cold read against a failing proxy: want an error")
	}
	clock.advance(time.Minute)
	if _, err := r.Read(ctx, srv.URL, true); err == nil {
		t.Error("throttled read with no snapshot: want the last error")
	}
	if n := s.reads.Load(); n != 1 {
		t.Errorf("reads after a failure inside the interval = %d, want 1", n)
	}

	s.fail.Store(false)
	s.total.Store(4)
	clock.advance(4 * time.Minute)
	if v := spendOf(t, r, ctx, srv.URL, true); v != 4 {
		t.Fatalf("read once due = %v, want 4", v)
	}

	s.fail.Store(true)
	clock.advance(5 * time.Minute)
	if v := spendOf(t, r, ctx, srv.URL, true); v != 4 {
		t.Errorf("failed refresh = %v, want the last good 4", v)
	}
	clock.advance(time.Minute)
	if v := spendOf(t, r, ctx, srv.URL, true); v != 4 {
		t.Errorf("throttled after a failed refresh = %v, want 4", v)
	}
	if n := s.reads.Load(); n != 3 {
		t.Errorf("reads = %d, want 3", n)
	}
}

// A caller that gives up does not cancel the read: other callers may be
// waiting on it, and hanging up would leave LiteLLM's scan running.
func TestTagSpendReader_CallerCancelDoesNotAbortRead(t *testing.T) {
	s := &countingTagsServer{gate: make(chan struct{})}
	s.total.Store(5)
	srv := httptest.NewServer(s)
	defer srv.Close()
	r, _ := newTestTagReader(5 * time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := r.Read(ctx, srv.URL, true)
		done <- err
	}()
	waitReads(t, s, 1)
	cancel()
	if err := <-done; err == nil {
		t.Fatal("cancelled caller: want ctx error")
	}
	close(s.gate)
	waitIdle(t, r, srv.URL)
	if v := spendOf(t, r, context.Background(), srv.URL, true); v != 5 {
		t.Errorf("after the cancelled caller = %v, want 5 from the same read", v)
	}
	if n := s.reads.Load(); n != 1 {
		t.Errorf("reads = %d, want 1", n)
	}
}

// Uncached (interval 0), every Read gets a read of its own, so a test can
// change the server between calls.
func TestTagSpendReader_UncachedReadsEveryTime(t *testing.T) {
	s := &countingTagsServer{}
	srv := httptest.NewServer(s)
	defer srv.Close()
	r, _ := newTestTagReader(0)
	for want := int64(1); want <= 3; want++ {
		s.total.Store(want)
		if v := spendOf(t, r, context.Background(), srv.URL, false); v != float64(want) {
			t.Errorf("read %d = %v, want %d", want, v, want)
		}
	}
	if n := s.reads.Load(); n != 3 {
		t.Errorf("reads = %d, want 3", n)
	}
}
