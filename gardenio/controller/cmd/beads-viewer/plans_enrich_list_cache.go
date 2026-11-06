package main

// Short-TTL memo for the dominant page-invariant list query the /plans index
// enrich pass fans out on every render (ka-0000003113, thread ka-0000002759 — the
// operator's "still way too slow in prod; need to cache more").
//
// The N+1 and the serial-hop wins already landed (plan ka-0000002224): the enrich
// pass runs its ~dozen bulk joins in two concurrent errgroup waves. One wave-1
// join re-fetches a large, NON-scoped, page-INVARIANT universe on EVERY render and
// is the measured wall-clock floor of the page (profiled against the prod daemon,
// 2026-08-12):
//
//   - planAssistantBulk — ONE `type=agent, label=role:plan-gardener` list, newest
//     first, capped at planAssistantIndexCap (400). ~4.0 MB / ~730 ms in prod;
//     57,939 plan-gardener agent beads exist, so the cap is always hit and the
//     daemon serializes 400 full bead rows every render. It is the single dominant
//     leg of the enrich pass, and — crucially — it is NEVER mutated from /plans, so
//     caching it cannot break a read-after-write on the page.
//
// This query does not depend on the viewer, the operator scope, or which plans are
// on the page: it pulls a fixed role-scoped, newest-first window and the caller
// joins it to the page's epics in Go. The underlying set changes only when an agent
// bead is created or updated — minutes-scale churn against second-scale index
// traffic. That is exactly the shape planProjectCache already memoizes for the
// project-filter row, and gardenerOwnerCache for the operator directory: cache the
// raw daemon result for a short TTL so a burst of index loads folds into a single
// upstream call, and the join still refreshes within a minute of a real edit.
//
// NOT cached: the gardener-trigger list (planGardenersByEpic, ~6.6 MB / ~207 ms). The
// /plans index is the gardener control room — its per-row toggle, bulk enable/
// disable bar, and master switch all MUTATE those exact gardener beads and then
// re-render through this same enrich pass. Memoizing that list would break
// read-after-write: a just-toggled or just-bound gardener would keep serving its
// stale/absent state for the TTL window. The correctness cost is not worth the
// ~207 ms; only the never-mutated ~730 ms assistant leg is cached.
//
// Only SUCCESSFUL fetches are cached. A daemon error is never stored, so the
// assistant join's existing best-effort degrade path (empty map, card renders
// "none") is unchanged and a transient failure cannot pin a stale/empty list for
// the TTL.

import (
	"context"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// planEnrichListCacheTTL bounds how long a memoized enrich-list result is reused
// before a fresh fetch. Minutes-scale, matching planProjectCacheTTL / gardenerOwnerTTL
// for the same "mutable set, intra-minute win, staleness immaterial" reason: long
// enough that an index burst (and every concurrent viewer) folds into one upstream
// call, short enough that a newly-spawned assistant or newly-bound gardener shows up
// on the card within a minute.
const planEnrichListCacheTTL = 60 * time.Second

// Cache key for the fixed enrich list-query shape that IS safe to memoize. The
// cache stays keyed (not a single slot) so a future page-invariant, non-mutated
// list can be added without a collision, but only ONE enrich list is cached
// today: the plan-gardener AGENT list (planAssistantBulk), the dominant
// ~730 ms / 4.0 MB leg of the pass and never mutated from /plans. The gardener
// TRIGGER list (planGardenersByEpic) is deliberately NOT cached — /plans mutates
// those beads, so memoizing them would break read-after-write (see the comment on
// planGardenersByEpic).
const planEnrichListKeyAssistants = "plan_assistants"

// planEnrichListCache memoizes the raw *beadsapi.ListBeadsResult for the small set
// of page-invariant enrich list queries, keyed by a caller-supplied string. All
// state is guarded by mu. nil-safe: a nil *planEnrichListCache reports every read as
// a miss and drops every write, so the bare-&Server{} test path falls straight
// through to the daemon exactly as before.
type planEnrichListCache struct {
	mu      sync.Mutex
	entries map[string]planEnrichListEntry
}

// planEnrichListEntry is one memoized fetch: the result and when it was stored.
type planEnrichListEntry struct {
	value     *beadsapi.ListBeadsResult
	fetchedAt time.Time
}

// newPlanEnrichListCache returns an empty cache ready for use.
func newPlanEnrichListCache() *planEnrichListCache {
	return &planEnrichListCache{entries: make(map[string]planEnrichListEntry)}
}

// get returns the cached result for key when within ttl of its fetchedAt. The
// boolean second return is true on a fresh hit.
func (c *planEnrichListCache) get(key string, now time.Time, ttl time.Duration) (*beadsapi.ListBeadsResult, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || e.fetchedAt.IsZero() || now.Sub(e.fetchedAt) > ttl {
		return nil, false
	}
	return e.value, true
}

// set replaces the cached result for key. Callers store only successful fetches.
func (c *planEnrichListCache) set(key string, v *beadsapi.ListBeadsResult, now time.Time) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]planEnrichListEntry)
	}
	c.entries[key] = planEnrichListEntry{value: v, fetchedAt: now}
}

// listBeadsFilteredCached returns the ListBeadsFiltered result for q from the memo
// on a fresh hit for key, otherwise fetches through the daemon and memoizes a
// SUCCESSFUL result. On a daemon error it returns the error verbatim and caches
// nothing, so the caller's best-effort degrade path is preserved. key must be a
// stable identifier for the query shape (the enrich joins use fixed queries, so a
// constant per join is correct); it is the caller's responsibility not to reuse one
// key for two different query shapes.
func (s *Server) listBeadsFilteredCached(ctx context.Context, key string, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	if isScopedCtx(ctx) {
		// Shared across principals; see listProjectBeadsCached.
		return s.daemon.ListBeadsFiltered(ctx, q)
	}
	now := s.now()
	if v, ok := s.planEnrichListCache.get(key, now, planEnrichListCacheTTL); ok {
		return v, nil
	}
	res, err := s.daemon.ListBeadsFiltered(ctx, q)
	if err != nil {
		return nil, err
	}
	s.planEnrichListCache.set(key, res, now)
	return res, nil
}
