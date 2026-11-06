package beadsapi

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// IsAncestor reports whether `child` is a descendant of `parent` via the
// parent-child dependency chain. Returns true when parent == child (a bead
// is trivially in its own subtree). Walks at most maxAncestorDepth levels
// before erroring; depth caps are intentional because epic graphs sometimes
// contain cycles or very deep trees and we want bounded latency for trigger
// evaluation.
//
// This is the daemon-side helper required by the in_subtree_of operator in
// the conditions DSL (gardenio/plans/epic-mention-looping-gardener.md §T1).
// The daemon already exposes the parent-child edges via GetDependencies; this
// helper walks them so callers get a single yes/no answer.
func (c *Client) IsAncestor(ctx context.Context, parent, child string) (bool, error) {
	if parent == "" || child == "" {
		return false, nil
	}
	if parent == child {
		return true, nil
	}
	visited := make(map[string]bool, 8)
	cur := child
	for range maxAncestorDepth {
		if visited[cur] {
			// Cycle in the parent-child graph; treat as not-an-ancestor
			// rather than erroring because the alternative is breaking
			// every trigger evaluation that touches a malformed graph.
			return false, nil
		}
		visited[cur] = true

		next, err := c.parentOf(ctx, cur)
		if err != nil {
			return false, fmt.Errorf("conditions: walking parent chain of %s: %w", cur, err)
		}
		if next == "" {
			// No parent edge — child has no further ancestors, so parent
			// is not an ancestor.
			return false, nil
		}
		if next == parent {
			return true, nil
		}
		cur = next
	}
	return false, fmt.Errorf("conditions: ancestor walk exceeded depth %d for child %s", maxAncestorDepth, child)
}

// maxAncestorDepth bounds the parent-chain walk. Real epic trees are
// typically 2-4 levels deep; 32 leaves comfortable headroom and still
// terminates in O(round-trips) on a degenerate graph.
const maxAncestorDepth = 32

// parentOf returns id's parent-child parent ("" when it has none), from the
// parent cache when EnableParentCache armed it and the entry is fresh.
func (c *Client) parentOf(ctx context.Context, id string) (string, error) {
	pc := c.parents
	var gen uint64
	if pc != nil {
		parent, g, ok := pc.get(id)
		if ok {
			return parent, nil
		}
		gen = g
	}
	deps, err := c.GetDependencies(ctx, id)
	if err != nil {
		return "", err
	}
	parent := ""
	for _, d := range deps {
		if d.Type == "parent-child" && d.DependsOnID != "" {
			parent = d.DependsOnID
			break
		}
	}
	if pc != nil {
		pc.put(id, parent, gen)
	}
	return parent, nil
}

// EnableParentCache makes IsAncestor remember each bead's parent-child parent
// for ttl, so a long-running evaluator that asks in_subtree_of for every event
// against every gardener trigger pays one GET /v1/beads/{id}/dependencies per
// bead per ttl rather than one per chain level per evaluation (gd-ZaztjcPrDc).
//
// A caller that enables it must call InvalidateParents for both endpoints of
// every dependency added or removed; ttl only bounds how stale a parent can be
// when such an event is missed. Call it once, before the client is shared.
// ttl <= 0 leaves the cache off, which is the default.
func (c *Client) EnableParentCache(ttl time.Duration) {
	if ttl <= 0 {
		c.parents = nil
		return
	}
	c.parents = &parentCache{ttl: ttl, now: time.Now, entries: make(map[string]parentEntry)}
}

// InvalidateParents drops the cached parent of each id. A no-op when the
// parent cache is off.
func (c *Client) InvalidateParents(ids ...string) {
	if c.parents != nil {
		c.parents.invalidate(ids)
	}
}

// parentCacheMaxEntries bounds the parent cache; reaching it clears the map,
// which costs one refetch per live bead and keeps memory flat.
const parentCacheMaxEntries = 50000

type parentEntry struct {
	parent string
	at     time.Time
}

// parentCache maps a bead id to its parent-child parent. gen advances on
// every invalidation; a fetch that began under an older gen is not stored,
// so a lookup racing a dependency event cannot re-cache the pre-event parent.
type parentCache struct {
	ttl     time.Duration
	now     func() time.Time
	mu      sync.Mutex
	gen     uint64
	entries map[string]parentEntry
}

// get returns the fresh cached parent of id, or ok=false and the generation a
// subsequent put must present.
func (pc *parentCache) get(id string) (parent string, gen uint64, ok bool) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	e, hit := pc.entries[id]
	if hit && pc.now().Sub(e.at) < pc.ttl {
		return e.parent, pc.gen, true
	}
	if hit {
		delete(pc.entries, id)
	}
	return "", pc.gen, false
}

func (pc *parentCache) put(id, parent string, gen uint64) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	if gen != pc.gen {
		return
	}
	if len(pc.entries) >= parentCacheMaxEntries {
		clear(pc.entries)
	}
	pc.entries[id] = parentEntry{parent: parent, at: pc.now()}
}

func (pc *parentCache) invalidate(ids []string) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	pc.gen++
	for _, id := range ids {
		delete(pc.entries, id)
	}
}
