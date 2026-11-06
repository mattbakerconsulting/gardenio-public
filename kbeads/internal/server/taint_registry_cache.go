package server

import (
	"context"
	"sync"
	"time"
)

// taintRegistryEmptyTTL bounds how long a cached "empty" verdict may be
// trusted before the next read re-queries the store. On a single process this
// cache is invalidated explicitly (InvalidateTaintRegistryCache, called from
// handleCreateTaint) the instant a taint is created, so the TTL never fires.
// On 2+ replicas, a taint created via replica A has no way to reach replica
// B's in-process cache — B keeps serving the stale "empty" verdict (allow-all
// past the ADR §10.1 gate) until this TTL forces a re-check. ADR §10.1 §15.1
// already describes the empty short-circuit as "a performance optimisation...
// not a correctness assumption", so bounding staleness to one TTL window
// stays within the design's stated tolerance (gd-XxrObXvc8r).
const taintRegistryEmptyTTL = 30 * time.Second

// taintRegistryCache memoises the monotonic fact "the taints registry holds
// at least one row" process-wide, per ADR docs/adr/taints.md §10.1: "No
// taints exist -> nothing changes. No table is read on the hot path; the gate
// short-circuits on an empty registry cached per process."
//
// Unlike taintContextCache / teamMembershipCache (bounded-TTL caches of a
// fact that can change either way), this cache tracks a fact that only ever
// flips false -> true, never back: once a taint is created it can be retired
// but the taints table is never emptied by ordinary operation. So a "non-empty"
// verdict is cached forever. An "empty" verdict is cached until an explicit
// invalidation on this process OR the taintRegistryEmptyTTL elapses, whichever
// comes first — the explicit invalidation keeps the single-process hot path
// instant, and the TTL is the cross-replica backstop for processes that never
// got the invalidation because the taint was created on a different replica.
type taintRegistryCache struct {
	mu        sync.Mutex
	checked   bool // true once HasAnyTaints has ever been queried
	nonEmpty  bool // the cached verdict, valid only when checked
	checkedAt time.Time

	// loaderCalls counts store queries triggered by a cache miss. For tests.
	loaderCalls int64
}

func newTaintRegistryCache() *taintRegistryCache {
	return &taintRegistryCache{}
}

// taintRegistryNonEmpty reports whether the taints registry holds at least
// one row, consulting the cache before ever touching the store. Both a cached
// "non-empty" AND a cached "empty" verdict short-circuit with no query — that
// second case is the ADR §10.1 hot path: a fleet with zero taints configured
// never issues a bead_taints (or taints) query on an ordinary bead read.
func (s *BeadsServer) taintRegistryNonEmpty(ctx context.Context) (bool, error) {
	c := s.taintRegistryCache

	c.mu.Lock()
	if c.checked && (c.nonEmpty || time.Since(c.checkedAt) < taintRegistryEmptyTTL) {
		nonEmpty := c.nonEmpty
		c.mu.Unlock()
		return nonEmpty, nil
	}
	c.mu.Unlock()

	nonEmpty, err := s.store.HasAnyTaints(ctx)
	if err != nil {
		return false, err
	}

	c.mu.Lock()
	c.loaderCalls++
	c.checked = true
	c.nonEmpty = nonEmpty
	c.checkedAt = time.Now()
	c.mu.Unlock()
	return nonEmpty, nil
}

// InvalidateTaintRegistryCache clears the cached registry-emptiness verdict
// so the next read/list/write gate check re-queries the store. This MUST be
// called after any successful store.SetTaint insert of a brand-new taint
// (e.g. the POST /v1/taints handler, gd-sygX3b8TAg) — otherwise a process
// that had already cached "empty" keeps allow-all access to the newly-taint-
// eligible beads for its remaining lifetime.
func (s *BeadsServer) InvalidateTaintRegistryCache() {
	s.taintRegistryCache.mu.Lock()
	defer s.taintRegistryCache.mu.Unlock()
	s.taintRegistryCache.checked = false
}
