package bridge

import (
	"context"
	"fmt"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// SubgraphClient is the subset of beadsapi.Client needed to walk the connected
// subgraph of a root bead. It pulls all four edge directions (forward and
// reverse dependencies, forward and reverse typed mentions) so the walk can
// treat the graph as undirected.
type SubgraphClient interface {
	GetDependencies(ctx context.Context, beadID string) ([]beadsapi.Dependency, error)
	GetReverseDependencies(ctx context.Context, beadID string) ([]beadsapi.Dependency, error)
	GetMentions(ctx context.Context, beadID string, limit int) ([]beadsapi.BeadReferenceSummary, error)
	GetMentionedBy(ctx context.Context, beadID string, limit int) ([]beadsapi.BeadReferenceSummary, error)
}

// SubgraphResolverConfig configures a SubgraphResolver. Zero values fall back
// to safe defaults (see DefaultSubgraph*).
type SubgraphResolverConfig struct {
	// MaxDepth bounds BFS expansion. The root sits at depth 0; a value of 4
	// admits up to ~10k beads in dense graphs, which is more than any real
	// gardener subgraph today. Defaults to DefaultSubgraphMaxDepth.
	MaxDepth int
	// MaxNodes bounds the total number of beads admitted into one closure
	// (root included). Once exceeded, the BFS stops and the closure is marked
	// "saturated" — IsInSubgraph then returns false for any candidate not
	// already in the closure. Defaults to DefaultSubgraphMaxNodes.
	MaxNodes int
	// TTL is how long a cached closure survives before re-expansion. Trades
	// freshness for query cost. Defaults to DefaultSubgraphTTL.
	TTL time.Duration
	// MentionLimit is the per-bead cap passed to GetMentions /
	// GetMentionedBy. Defaults to DefaultSubgraphMentionLimit.
	MentionLimit int
	// Now is the clock used for TTL expiry. nil falls back to time.Now —
	// tests override this to drive the clock deterministically.
	Now func() time.Time
}

// Default bounds for the subgraph resolver. Picked to make a 4-hop walk over
// the typical tree-hugger demo subgraph (≤ 100 beads) cheap, while capping
// pathological epic graphs that would otherwise stall an Evaluate cycle.
const (
	DefaultSubgraphMaxDepth     = 4
	DefaultSubgraphMaxNodes     = 1024
	DefaultSubgraphTTL          = 30 * time.Second
	DefaultSubgraphMentionLimit = 200
)

// SubgraphResolver answers "is candidate in the connected subgraph rooted at
// X?" by BFS over the union of dependency edges and typed mentions, with a
// TTL cache per root.
//
// The connected subgraph of root R is the set of beads reachable from R via
// any of:
//   - forward dependencies (R depends on B, any dep type)
//   - reverse dependencies (B depends on R, any dep type)
//   - forward mentions (R's text references B)
//   - reverse mentions (B's text references R)
//
// This matches the operator-facing intent — wake when ANY bead connected
// to my demo bead mutates, regardless of which way the edge points. See
// gardenio/docs/triggers.md § conditions for the user-facing description.
type SubgraphResolver struct {
	client SubgraphClient
	cfg    SubgraphResolverConfig

	mu    sync.Mutex
	cache map[string]subgraphCacheEntry
}

type subgraphCacheEntry struct {
	closure   map[string]struct{}
	saturated bool
	expiresAt time.Time
}

// NewSubgraphResolver constructs a resolver against the given client. nil
// client is allowed (the resolver becomes a no-op that errors on use) so test
// fixtures and partial wiring don't crash at construction time.
func NewSubgraphResolver(client SubgraphClient, cfg SubgraphResolverConfig) *SubgraphResolver {
	if cfg.MaxDepth <= 0 {
		cfg.MaxDepth = DefaultSubgraphMaxDepth
	}
	if cfg.MaxNodes <= 0 {
		cfg.MaxNodes = DefaultSubgraphMaxNodes
	}
	if cfg.TTL <= 0 {
		cfg.TTL = DefaultSubgraphTTL
	}
	if cfg.MentionLimit <= 0 {
		cfg.MentionLimit = DefaultSubgraphMentionLimit
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &SubgraphResolver{
		client: client,
		cfg:    cfg,
		cache:  make(map[string]subgraphCacheEntry),
	}
}

// IsInSubgraph reports whether candidate sits in the connected subgraph of
// root. Root is trivially in its own subgraph. A nil client or empty root /
// candidate returns false with no error so call sites can be defensive without
// branching.
//
// Closure is cached per-root for cfg.TTL. On a saturated closure (BFS hit
// MaxNodes before completing) the result is best-effort: candidates already
// in the closure return true; everything else returns false. Callers that
// need stricter correctness should narrow the subgraph root.
func (r *SubgraphResolver) IsInSubgraph(ctx context.Context, root, candidate string) (bool, error) {
	if r == nil || r.client == nil {
		return false, fmt.Errorf("subgraph resolver: no client configured")
	}
	if root == "" || candidate == "" {
		return false, nil
	}
	if root == candidate {
		return true, nil
	}
	closure, err := r.closureFor(ctx, root)
	if err != nil {
		return false, err
	}
	_, ok := closure[candidate]
	return ok, nil
}

// closureFor returns the (possibly cached) closure set for root.
func (r *SubgraphResolver) closureFor(ctx context.Context, root string) (map[string]struct{}, error) {
	now := r.cfg.Now()

	r.mu.Lock()
	entry, ok := r.cache[root]
	if ok && entry.expiresAt.After(now) {
		closure := entry.closure
		r.mu.Unlock()
		return closure, nil
	}
	r.mu.Unlock()

	closure, saturated, err := r.expand(ctx, root)
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	r.cache[root] = subgraphCacheEntry{
		closure:   closure,
		saturated: saturated,
		expiresAt: r.cfg.Now().Add(r.cfg.TTL),
	}
	r.mu.Unlock()
	return closure, nil
}

// expand runs BFS from root through dependency edges (both directions) and
// typed mentions (both directions). Returns the closure set (including root)
// and a saturated flag indicating whether MaxNodes or MaxDepth cut the walk
// short.
func (r *SubgraphResolver) expand(ctx context.Context, root string) (map[string]struct{}, bool, error) {
	closure := map[string]struct{}{root: {}}
	frontier := []string{root}
	saturated := false

	for depth := 0; depth < r.cfg.MaxDepth; depth++ {
		if len(frontier) == 0 {
			break
		}
		next := make([]string, 0, len(frontier))
		for _, cur := range frontier {
			neighbours, err := r.neighbours(ctx, cur)
			if err != nil {
				return nil, false, err
			}
			for _, n := range neighbours {
				if n == "" {
					continue
				}
				if _, seen := closure[n]; seen {
					continue
				}
				if len(closure) >= r.cfg.MaxNodes {
					saturated = true
					return closure, saturated, nil
				}
				closure[n] = struct{}{}
				next = append(next, n)
			}
		}
		frontier = next
	}
	if len(frontier) > 0 {
		// Hit MaxDepth with more to walk — frontier members are already in
		// the closure but their further neighbours were not explored.
		saturated = true
	}
	return closure, saturated, nil
}

// neighbours collects every bead ID reachable from id in one hop via any of
// the four edge directions.
func (r *SubgraphResolver) neighbours(ctx context.Context, id string) ([]string, error) {
	out := make([]string, 0, 8)

	fwd, err := r.client.GetDependencies(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("subgraph: fwd deps for %s: %w", id, err)
	}
	for _, d := range fwd {
		if d.DependsOnID != "" {
			out = append(out, d.DependsOnID)
		}
	}

	rev, err := r.client.GetReverseDependencies(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("subgraph: rev deps for %s: %w", id, err)
	}
	for _, d := range rev {
		if d.BeadID != "" {
			out = append(out, d.BeadID)
		}
	}

	mentions, err := r.client.GetMentions(ctx, id, r.cfg.MentionLimit)
	if err != nil {
		return nil, fmt.Errorf("subgraph: mentions for %s: %w", id, err)
	}
	for _, m := range mentions {
		if m.ID != "" {
			out = append(out, m.ID)
		}
	}

	mentionedBy, err := r.client.GetMentionedBy(ctx, id, r.cfg.MentionLimit)
	if err != nil {
		return nil, fmt.Errorf("subgraph: mentioned-by for %s: %w", id, err)
	}
	for _, m := range mentionedBy {
		if m.ID != "" {
			out = append(out, m.ID)
		}
	}
	return out, nil
}

// Invalidate drops any cached closure for root. Used by tests; in production
// the TTL is the only invalidation path today.
func (r *SubgraphResolver) Invalidate(root string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.cache, root)
}

// ClosureIDs returns the (possibly cached) set of bead IDs in the connected
// subgraph rooted at root, including root itself. The result is a fresh slice
// the caller may mutate. Order is unspecified. Used by the WatchedResolver to
// materialize the `watched` list for CEL expressions — the same bounded BFS,
// TTL cache, and saturation semantics as IsInSubgraph apply (a closure that
// hit MaxNodes is returned best-effort/truncated). A nil resolver or client
// returns an error so callers can fail closed.
func (r *SubgraphResolver) ClosureIDs(ctx context.Context, root string) ([]string, error) {
	if r == nil || r.client == nil {
		return nil, fmt.Errorf("subgraph resolver: no client configured")
	}
	if root == "" {
		return nil, nil
	}
	closure, err := r.closureFor(ctx, root)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(closure))
	for id := range closure {
		ids = append(ids, id)
	}
	return ids, nil
}
