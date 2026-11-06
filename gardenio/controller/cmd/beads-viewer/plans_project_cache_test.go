package main

import (
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// newProjectCacheServer builds a minimal Server wired to fake f with the clock
// pinned to *nowp, so a test can advance time deterministically across
// listProjectBeadsCached calls.
func newProjectCacheServer(f *fakeDaemon, nowp *time.Time) *Server {
	srv := NewServer(f, testLogger(), "", "", "", nil)
	srv.SetNow(func() time.Time { return *nowp })
	return srv
}

// TestPlanProjectCache_MemoizesWithinTTL proves a burst of planProjectOptions
// calls within the TTL folds into ONE ListProjectBeads round-trip — the
// read-only /plans + /deck project-list round-trip polish (ka-0000006264).
func TestPlanProjectCache_MemoizesWithinTTL(t *testing.T) {
	f := newFake()
	f.projectsResp = map[string]beadsapi.ProjectInfo{
		"gardenio": {Name: "gardenio"},
		"kbeads":   {Name: "kbeads"},
	}
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	srv := newProjectCacheServer(f, &now)

	// Three renders inside the TTL (a /plans load, then /deck, then a refresh).
	for i := 0; i < 3; i++ {
		opts := srv.planProjectOptions(t.Context(), "")
		if len(opts) != 2 {
			t.Fatalf("call %d: expected 2 projects, got %v", i, opts)
		}
		now = now.Add(planProjectCacheTTL / 4) // still within one TTL window
	}
	if f.projectsCalls != 1 {
		t.Errorf("expected 1 upstream ListProjectBeads within TTL, got %d", f.projectsCalls)
	}
}

// TestPlanProjectCache_RefetchesAfterTTL proves the memo expires: a call past
// the TTL boundary issues a fresh round-trip (so a newly-registered project
// shows up in the filter chip row within a minute).
func TestPlanProjectCache_RefetchesAfterTTL(t *testing.T) {
	f := newFake()
	f.projectsResp = map[string]beadsapi.ProjectInfo{"gardenio": {Name: "gardenio"}}
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	srv := newProjectCacheServer(f, &now)

	srv.planProjectOptions(t.Context(), "")
	now = now.Add(planProjectCacheTTL + time.Second) // past the window
	srv.planProjectOptions(t.Context(), "")

	if f.projectsCalls != 2 {
		t.Errorf("expected 2 upstream fetches across the TTL boundary, got %d", f.projectsCalls)
	}
}

// TestPlanProjectCache_ErrorNotCached proves a daemon error is never memoized:
// the soft-degrade path (planProjectOptions → just [gardenio]) is preserved and
// a transient failure cannot pin an empty list for the TTL — the next call
// re-fetches and can recover.
func TestPlanProjectCache_ErrorNotCached(t *testing.T) {
	f := newFake()
	f.projectsErr = errFake("boom")
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	srv := newProjectCacheServer(f, &now)

	// First call errors → degrades to [gardenio], caches nothing.
	if opts := srv.planProjectOptions(t.Context(), ""); len(opts) != 1 || opts[0] != "gardenio" {
		t.Fatalf("error should degrade to [gardenio], got %v", opts)
	}
	// Recover the daemon; the very next call (same TTL window) must re-fetch
	// rather than serve a cached empty/error result.
	f.projectsErr = nil
	f.projectsResp = map[string]beadsapi.ProjectInfo{
		"gardenio": {Name: "gardenio"},
		"coop":     {Name: "coop"},
	}
	opts := srv.planProjectOptions(t.Context(), "")
	if len(opts) != 2 {
		t.Fatalf("recovered call should re-fetch, got %v", opts)
	}
	if f.projectsCalls != 2 {
		t.Errorf("expected 2 upstream fetches (error not cached), got %d", f.projectsCalls)
	}
}

// TestPlanProjectCache_NilSafe proves the nil-cache path (bare &Server{} with no
// planProjectCache) falls straight through to the daemon on every call rather
// than panicking.
func TestPlanProjectCache_NilSafe(t *testing.T) {
	f := newFake()
	f.projectsResp = map[string]beadsapi.ProjectInfo{"gardenio": {Name: "gardenio"}}
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	srv := &Server{daemon: f, logger: testLogger(), now: func() time.Time { return now }}

	for i := 0; i < 2; i++ {
		if _, err := srv.listProjectBeadsCached(t.Context()); err != nil {
			t.Fatalf("call %d: unexpected error %v", i, err)
		}
	}
	if f.projectsCalls != 2 {
		t.Errorf("nil cache must not memoize; expected 2 fetches, got %d", f.projectsCalls)
	}
}
