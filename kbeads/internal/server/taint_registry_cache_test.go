package server

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// TestTaintRegistryNonEmpty_ConcurrentFirstUse reproduces the health-snapshot
// fan-out shape that found the data race (gd-lbEqC5ngPM): several goroutines
// call taintRegistryNonEmpty on a freshly-constructed server at the same
// time, before any of them has populated the cache's "checked" verdict. Prior
// to the fix, initTaintRegistryCache's unsynchronised
// `if s.taintRegistryCache == nil { ... }` raced under `go test -race`.
func TestTaintRegistryNonEmpty_ConcurrentFirstUse(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})

	const goroutines = 32
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			if _, err := s.taintRegistryNonEmpty(context.Background()); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("taintRegistryNonEmpty: %v", err)
	}
}

// TestTaintRegistryNonEmpty_EmptyVerdictExpiresAfterTTL is the cross-replica
// backstop from the Phase 2 2-replica spike (gd-XxrObXvc8r): a process that
// cached "empty" has no way to learn about a taint created on a different
// replica (InvalidateTaintRegistryCache only clears the SAME process's
// cache), so the cached "empty" verdict must expire on its own after
// taintRegistryEmptyTTL rather than being trusted forever.
func TestTaintRegistryNonEmpty_EmptyVerdictExpiresAfterTTL(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})

	nonEmpty, err := s.taintRegistryNonEmpty(context.Background())
	if err != nil {
		t.Fatalf("taintRegistryNonEmpty: %v", err)
	}
	if nonEmpty {
		t.Fatal("expected empty verdict on a fresh registry")
	}
	if got := s.taintRegistryCache.loaderCalls; got != 1 {
		t.Fatalf("loaderCalls after first read = %d, want 1", got)
	}

	// Simulate a taint created on a DIFFERENT replica: the store now has one,
	// but this process's cache was never invalidated (no local
	// InvalidateTaintRegistryCache call) — only the TTL can surface it.
	if err := ms.SetTaint(context.Background(), &model.Taint{Name: "pii", OwnerTeam: "team-x"}); err != nil {
		t.Fatalf("SetTaint: %v", err)
	}

	// Still within the TTL window: cache must NOT re-check the store.
	nonEmpty, err = s.taintRegistryNonEmpty(context.Background())
	if err != nil {
		t.Fatalf("taintRegistryNonEmpty: %v", err)
	}
	if nonEmpty {
		t.Fatal("expected the stale cached empty verdict to still be served within the TTL window")
	}
	if got := s.taintRegistryCache.loaderCalls; got != 1 {
		t.Fatalf("loaderCalls within TTL window = %d, want 1 (no re-check)", got)
	}

	// Force the cached verdict to look stale by backdating checkedAt past
	// taintRegistryEmptyTTL, standing in for the passage of real time.
	s.taintRegistryCache.mu.Lock()
	s.taintRegistryCache.checkedAt = time.Now().Add(-taintRegistryEmptyTTL - time.Second)
	s.taintRegistryCache.mu.Unlock()

	nonEmpty, err = s.taintRegistryNonEmpty(context.Background())
	if err != nil {
		t.Fatalf("taintRegistryNonEmpty: %v", err)
	}
	if !nonEmpty {
		t.Error("expected the TTL to force a re-check that observes the cross-replica taint")
	}
	if got := s.taintRegistryCache.loaderCalls; got != 2 {
		t.Fatalf("loaderCalls after TTL expiry = %d, want 2 (one re-check)", got)
	}
}

// TestTaintRegistryNonEmpty_NonEmptyVerdictNeverExpires guards the other half
// of the TTL change: a cached "non-empty" verdict is a monotonic fact and
// must stay cached forever, TTL or not — re-checking it would defeat the
// point of the ADR §10.1 hot-path short-circuit.
func TestTaintRegistryNonEmpty_NonEmptyVerdictNeverExpires(t *testing.T) {
	ms := newMockStore()
	if err := ms.SetTaint(context.Background(), &model.Taint{Name: "pii", OwnerTeam: "team-x"}); err != nil {
		t.Fatalf("SetTaint: %v", err)
	}
	s := NewBeadsServer(ms, &events.NoopPublisher{})

	if nonEmpty, err := s.taintRegistryNonEmpty(context.Background()); err != nil || !nonEmpty {
		t.Fatalf("taintRegistryNonEmpty = (%v, %v), want (true, nil)", nonEmpty, err)
	}

	s.taintRegistryCache.mu.Lock()
	s.taintRegistryCache.checkedAt = time.Now().Add(-10 * taintRegistryEmptyTTL)
	s.taintRegistryCache.mu.Unlock()

	nonEmpty, err := s.taintRegistryNonEmpty(context.Background())
	if err != nil || !nonEmpty {
		t.Fatalf("taintRegistryNonEmpty after long backdate = (%v, %v), want (true, nil)", nonEmpty, err)
	}
	if got := s.taintRegistryCache.loaderCalls; got != 1 {
		t.Fatalf("loaderCalls = %d, want 1 — a non-empty verdict must never be re-checked", got)
	}
}

// TestHandleCreateTaint_InvalidatesRegistryCache pins the missing wiring the
// spike doc found: handleCreateTaint must call InvalidateTaintRegistryCache
// so THIS process's own subsequent reads see the new taint immediately
// rather than waiting out taintRegistryEmptyTTL.
func TestHandleCreateTaint_InvalidatesRegistryCache(t *testing.T) {
	s, _, h := newTaintTestServer(map[string][]string{
		"admin@example.com": {RoleAdmin},
	})

	if nonEmpty, err := s.taintRegistryNonEmpty(context.Background()); err != nil || nonEmpty {
		t.Fatalf("taintRegistryNonEmpty on a fresh registry = (%v, %v), want (false, nil)", nonEmpty, err)
	}

	rec := taintDoJSONAs(t, h, "POST", "/v1/taints", map[string]any{
		"name":       "customer-pii",
		"owner_team": "gd-team-prod",
	}, "admin@example.com")
	requireStatus(t, rec, http.StatusCreated)

	nonEmpty, err := s.taintRegistryNonEmpty(context.Background())
	if err != nil {
		t.Fatalf("taintRegistryNonEmpty after create: %v", err)
	}
	if !nonEmpty {
		t.Error("expected the registry cache to be invalidated by handleCreateTaint, still reporting empty")
	}
}
