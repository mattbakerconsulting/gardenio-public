package bridge

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// WatchedResolver materializes the `watched` list exposed to CEL trigger
// expressions: the set of beads connected to a root bead, hydrated into
// BeadEvents so an expression can read their fields/labels/status.
//
// It composes the existing SubgraphResolver (bounded BFS over the connected
// subgraph — node cap, depth cap, TTL cache) with a daemon GetBead hydration
// step, and adds its OWN bounded TTL cache over the hydrated result so the hot
// path does not re-fetch N beads on every Evaluate cycle. This is the
// "explicitly-resolved watched list, bounded like the existing subgraph
// resolver (node cap + TTL cache)" the F4 acceptance asks for.
//
// FAIL-CLOSED posture: any error (nil resolver, nil daemon, BFS error,
// hydration error) is returned to the caller, which treats `watched` as empty
// AND fails the expression closed. A partial hydration (some GetBead calls
// fail) returns the beads that did hydrate plus a nil error — best-effort,
// matching the subgraph resolver's saturation semantics; an author who needs
// strictness should narrow the watched root.
type WatchedResolver struct {
	subgraph *SubgraphResolver
	daemon   beadHydrator

	// maxNodes caps how many beads are hydrated for one root, independent of
	// (and never larger than) the subgraph node cap. Bounds the GetBead fan-out
	// per evaluation.
	maxNodes int
	ttl      time.Duration
	now      func() time.Time

	mu    sync.Mutex
	cache map[string]watchedCacheEntry
}

type watchedCacheEntry struct {
	beads     []*BeadEvent
	expiresAt time.Time
}

// Default bounds for the watched resolver. The node cap is intentionally
// smaller than DefaultSubgraphMaxNodes: hydrating a watched bead is a GetBead
// round-trip (vs. an edge walk), so a tighter cap keeps the per-eval fan-out
// modest. The TTL matches the subgraph resolver so the two caches expire in
// lockstep.
const (
	DefaultWatchedMaxNodes = 256
	DefaultWatchedTTL      = 30 * time.Second
)

// WatchedResolverConfig configures a WatchedResolver. Zero values fall back to
// the DefaultWatched* constants.
type WatchedResolverConfig struct {
	MaxNodes int
	TTL      time.Duration
	// Now is the clock for TTL expiry; nil falls back to time.Now. Tests
	// override this to drive expiry deterministically.
	Now func() time.Time
}

// NewWatchedResolver constructs a WatchedResolver over a subgraph resolver and
// a hydrating daemon. Either may be nil — a nil resolver or daemon makes
// Resolve return an error (fail-closed) rather than panicking, so partial
// wiring and test fixtures stay safe.
func NewWatchedResolver(subgraph *SubgraphResolver, daemon beadHydrator, cfg WatchedResolverConfig) *WatchedResolver {
	if cfg.MaxNodes <= 0 {
		cfg.MaxNodes = DefaultWatchedMaxNodes
	}
	if cfg.TTL <= 0 {
		cfg.TTL = DefaultWatchedTTL
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &WatchedResolver{
		subgraph: subgraph,
		daemon:   daemon,
		maxNodes: cfg.MaxNodes,
		ttl:      cfg.TTL,
		now:      cfg.Now,
		cache:    make(map[string]watchedCacheEntry),
	}
}

// Resolve returns the hydrated watched list for the connected subgraph rooted
// at root. The root bead itself is excluded (the expression already has it as
// `bead`); only the related beads are returned. Results are cached per root for
// the resolver TTL.
//
// A nil WatchedResolver returns (nil, nil) so the expression simply sees an
// empty watched list when no resolver is wired — an expression that does not
// reference `watched` still evaluates. (Expressions that DO reference watched
// over an unwired resolver get an empty list, which is the safe under-match.)
func (w *WatchedResolver) Resolve(ctx context.Context, root string) ([]*BeadEvent, error) {
	if w == nil {
		return nil, nil
	}
	if root == "" {
		return nil, nil
	}
	if w.subgraph == nil || w.daemon == nil {
		return nil, fmt.Errorf("watched resolver: subgraph or daemon not configured")
	}

	now := w.now()
	w.mu.Lock()
	if entry, ok := w.cache[root]; ok && entry.expiresAt.After(now) {
		beads := entry.beads
		w.mu.Unlock()
		return beads, nil
	}
	w.mu.Unlock()

	ids, err := w.subgraph.ClosureIDs(ctx, root)
	if err != nil {
		return nil, fmt.Errorf("watched resolver: resolving closure for %s: %w", root, err)
	}

	beads := make([]*BeadEvent, 0, len(ids))
	for _, id := range ids {
		if id == root {
			continue // root is exposed as `bead`, not in `watched`
		}
		if len(beads) >= w.maxNodes {
			break // node cap — best-effort truncation
		}
		detail, derr := w.daemon.GetBead(ctx, id)
		if derr != nil || detail == nil {
			// Best-effort: a single hydration failure drops that bead rather
			// than failing the whole resolve. The expression sees a slightly
			// smaller watched set — an under-match, which is the safe direction.
			continue
		}
		ev := beadEventFromDetail(detail)
		beads = append(beads, &ev)
	}

	w.mu.Lock()
	w.cache[root] = watchedCacheEntry{
		beads:     beads,
		expiresAt: w.now().Add(w.ttl),
	}
	w.mu.Unlock()
	return beads, nil
}
