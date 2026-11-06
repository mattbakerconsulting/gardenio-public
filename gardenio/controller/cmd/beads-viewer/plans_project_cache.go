package main

// Short-TTL memo for the /plans + /deck project list (ka-0000006264).
//
// The user/project/status filter feature (ka-0000000209) made the project-filter
// chip row available to EVERY viewer, not just operators — so every /plans (and
// /deck) render now issues one ListProjectBeads round-trip to build that row,
// where read-only viewers previously issued none. The daemon call is cheap and
// degrades soft on error, but on a busy index it is one avoidable RTT per page
// view of a list that changes only when a project bead is created/renamed —
// minutes-scale churn against second-scale traffic.
//
// planProjectCache collapses that: the raw ListProjectBeads map is memoized for
// a short TTL, so a burst of index loads folds into a single upstream call and
// the filter chip row still refreshes within a minute of a project-registry
// edit. Mirrors mrStatusCache / feedSeedCache (index_mr_status.go, feed.go):
// process-local, goroutine-safe, and the whole win is intra-minute where
// staleness is immaterial.
//
// Only SUCCESSFUL fetches are cached. A daemon error is never stored, so the
// error-degrade path (planProjectOptions → just the "gardenio" default) is
// unchanged and a transient failure cannot pin an empty list for the TTL.

import (
	"context"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// planProjectCacheTTL bounds how long a memoized project list is reused before a
// fresh ListProjectBeads. Minutes-scale: long enough that an index burst folds
// into one call, short enough that a newly-registered/renamed project shows up
// in the filter chip row promptly. Matches the gardenerOwnerTTL order for the
// same "mutable directory field, intra-minute win" reason.
const planProjectCacheTTL = 60 * time.Second

// planProjectCache holds the last-fetched ListProjectBeads map and when it was
// fetched. nil-safe: a nil *planProjectCache reports every read as a miss and
// drops every write, so the zero-value / bare-&Server{} test path falls straight
// through to the daemon.
type planProjectCache struct {
	mu        sync.Mutex
	value     map[string]beadsapi.ProjectInfo
	fetchedAt time.Time
}

// get returns the cached project map when within ttl of fetchedAt. The boolean
// second return is true on a fresh hit.
func (c *planProjectCache) get(now time.Time, ttl time.Duration) (map[string]beadsapi.ProjectInfo, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fetchedAt.IsZero() || now.Sub(c.fetchedAt) > ttl {
		return nil, false
	}
	return c.value, true
}

// set replaces the cached project map. Callers store only successful fetches.
func (c *planProjectCache) set(v map[string]beadsapi.ProjectInfo, now time.Time) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.value = v
	c.fetchedAt = now
}

// listProjectBeadsCached returns the project map from planProjectCache on a
// fresh hit, otherwise fetches through the daemon and memoizes a SUCCESSFUL
// result. On a daemon error it returns the error verbatim and caches nothing,
// so planProjectOptions' soft-degrade path is preserved. This is the single
// choke point behind planProjectOptions, so /plans and /deck share one memo.
func (s *Server) listProjectBeadsCached(ctx context.Context) (map[string]beadsapi.ProjectInfo, error) {
	if isScopedCtx(ctx) {
		// Shared across principals: the project list a scoped principal sees is
		// its own, so it neither reads an admin's entry nor fills one.
		return s.daemon.ListProjectBeads(ctx)
	}
	now := s.now()
	if v, ok := s.planProjectCache.get(now, planProjectCacheTTL); ok {
		return v, nil
	}
	v, err := s.daemon.ListProjectBeads(ctx)
	if err != nil {
		return nil, err
	}
	s.planProjectCache.set(v, now)
	return v, nil
}
