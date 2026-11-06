package main

import (
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// newEnrichListCacheServer builds a minimal Server wired to fake f with the clock
// pinned to *nowp and a real planEnrichListCache, so a test can advance time
// deterministically across listBeadsFilteredCached calls.
func newEnrichListCacheServer(f *fakeDaemon, nowp *time.Time) *Server {
	srv := NewServer(f, testLogger(), "", "", "", nil)
	srv.SetNow(func() time.Time { return *nowp })
	return srv
}

// TestPlanEnrichListCache_MemoizesWithinTTL proves a burst of listBeadsFilteredCached
// calls for the same key within the TTL folds into ONE upstream ListBeadsFiltered
// round-trip — the /plans enrich list-cache win (ka-0000003113).
func TestPlanEnrichListCache_MemoizesWithinTTL(t *testing.T) {
	f := newFake()
	q := beadsapi.ListBeadsQuery{Types: []string{"agent"}, Labels: []string{planGardenerRoleLabel}}
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	srv := newEnrichListCacheServer(f, &now)

	for i := 0; i < 3; i++ {
		if _, err := srv.listBeadsFilteredCached(t.Context(), planEnrichListKeyAssistants, q); err != nil {
			t.Fatalf("call %d: unexpected error %v", i, err)
		}
		now = now.Add(planEnrichListCacheTTL / 4) // still within one TTL window
	}
	if got := len(f.listAllQ); got != 1 {
		t.Errorf("expected 1 upstream ListBeadsFiltered within TTL, got %d", got)
	}
}

// TestPlanEnrichListCache_RefetchesAfterTTL proves the memo expires: a call past
// the TTL boundary issues a fresh round-trip, so a newly-spawned assistant shows up
// on the card within a minute.
func TestPlanEnrichListCache_RefetchesAfterTTL(t *testing.T) {
	f := newFake()
	q := beadsapi.ListBeadsQuery{Types: []string{"agent"}, Labels: []string{planGardenerRoleLabel}}
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	srv := newEnrichListCacheServer(f, &now)

	if _, err := srv.listBeadsFilteredCached(t.Context(), planEnrichListKeyAssistants, q); err != nil {
		t.Fatalf("first call: %v", err)
	}
	now = now.Add(planEnrichListCacheTTL + time.Second) // past the window
	if _, err := srv.listBeadsFilteredCached(t.Context(), planEnrichListKeyAssistants, q); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if got := len(f.listAllQ); got != 2 {
		t.Errorf("expected 2 upstream fetches across the TTL boundary, got %d", got)
	}
}

// TestPlanEnrichListCache_KeysAreIndependent proves two distinct keys don't collide:
// each query shape is fetched once and cached under its own key, so a future second
// cached list can never serve the assistant list's result (the cache stays keyed
// even though only the assistant list is memoized today).
func TestPlanEnrichListCache_KeysAreIndependent(t *testing.T) {
	f := newFake()
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	srv := newEnrichListCacheServer(f, &now)

	const otherKey = "test_other_shape"
	qa := beadsapi.ListBeadsQuery{Types: []string{"agent"}, Labels: []string{planGardenerRoleLabel}}
	qb := beadsapi.ListBeadsQuery{Types: []string{"epic"}, Labels: []string{"plan"}}
	for i := 0; i < 2; i++ {
		if _, err := srv.listBeadsFilteredCached(t.Context(), planEnrichListKeyAssistants, qa); err != nil {
			t.Fatalf("assistants call %d: %v", i, err)
		}
		if _, err := srv.listBeadsFilteredCached(t.Context(), otherKey, qb); err != nil {
			t.Fatalf("other-key call %d: %v", i, err)
		}
	}
	// One fetch per key (two total), not two per key: each key memoizes independently.
	if got := len(f.listAllQ); got != 2 {
		t.Errorf("expected 2 upstream fetches (one per key), got %d", got)
	}
}

// TestPlanEnrichListCache_ErrorNotCached proves a daemon error is never memoized:
// each join's best-effort degrade path is preserved and a transient failure cannot
// pin a stale/empty result for the TTL — the next call re-fetches and can recover.
func TestPlanEnrichListCache_ErrorNotCached(t *testing.T) {
	f := newFake()
	f.listErr = errFake("boom")
	q := beadsapi.ListBeadsQuery{Types: []string{"agent"}, Labels: []string{planGardenerRoleLabel}}
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	srv := newEnrichListCacheServer(f, &now)

	if _, err := srv.listBeadsFilteredCached(t.Context(), planEnrichListKeyAssistants, q); err == nil {
		t.Fatalf("first call: expected error, got nil")
	}
	// Recover the daemon; the very next call (same TTL window) must re-fetch rather
	// than serve a cached error.
	f.listErr = nil
	if _, err := srv.listBeadsFilteredCached(t.Context(), planEnrichListKeyAssistants, q); err != nil {
		t.Fatalf("recovered call: %v", err)
	}
	if got := len(f.listAllQ); got != 2 {
		t.Errorf("expected 2 upstream fetches (error not cached), got %d", got)
	}
}

// TestPlanEnrichListCache_NilSafe proves the nil-cache path (bare &Server{} with no
// planEnrichListCache) falls straight through to the daemon on every call rather
// than panicking.
func TestPlanEnrichListCache_NilSafe(t *testing.T) {
	f := newFake()
	q := beadsapi.ListBeadsQuery{Types: []string{"agent"}, Labels: []string{planGardenerRoleLabel}}
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	srv := &Server{daemon: f, logger: testLogger(), now: func() time.Time { return now }}

	for i := 0; i < 2; i++ {
		if _, err := srv.listBeadsFilteredCached(t.Context(), planEnrichListKeyAssistants, q); err != nil {
			t.Fatalf("call %d: unexpected error %v", i, err)
		}
	}
	if got := len(f.listAllQ); got != 2 {
		t.Errorf("nil cache must not memoize; expected 2 fetches, got %d", got)
	}
}
