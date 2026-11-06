package server

import (
	"context"
	"sync"
	"time"
)

// TaintSet is a set of taint ids (taints.id / bead_taints.taint_id).
type TaintSet map[int64]bool

// taintContextCacheEntry holds one cached resolution, keyed by agent bead id.
type taintContextCacheEntry struct {
	cachedAt time.Time
	taints   TaintSet
}

// taintContextCache memoises the taint set of an agent bead for a bounded
// TTL. Per ADR docs/adr/taints.md §4.2, the per-agent taint set is tiny and
// changes rarely, so a cache hit performs no store query at all.
type taintContextCache struct {
	ttl time.Duration
	now func() time.Time

	mu      sync.Mutex
	entries map[string]*taintContextCacheEntry

	// loaderCalls counts store queries triggered by a cache miss. For tests.
	loaderCalls int64
}

func newTaintContextCache(ttl time.Duration) *taintContextCache {
	return &taintContextCache{
		ttl:     ttl,
		now:     time.Now,
		entries: make(map[string]*taintContextCacheEntry),
	}
}

// get returns the cached taint set for agentBeadID and true on a live hit.
func (c *taintContextCache) get(agentBeadID string) (TaintSet, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[agentBeadID]
	if !ok || c.now().Sub(e.cachedAt) >= c.ttl {
		return nil, false
	}
	return e.taints, true
}

// set installs a freshly-loaded taint set for agentBeadID.
func (c *taintContextCache) set(agentBeadID string, taints TaintSet) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[agentBeadID] = &taintContextCacheEntry{cachedAt: c.now(), taints: taints}
}

// LoaderCalls returns the cumulative store-query count. For tests.
func (c *taintContextCache) LoaderCalls() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.loaderCalls
}

// identityCacheTTL is the lifetime of the per-agent taint-context and
// per-operator team-membership caches: short enough that a transition is seen
// within one human tick, long enough to collapse a burst of reads into one
// store query.
const identityCacheTTL = 3 * time.Second

// initTaintContextCache lazily wires the cache on first use.
func (s *BeadsServer) initTaintContextCache() {
	if s.taintContextCache == nil {
		s.taintContextCache = newTaintContextCache(identityCacheTTL)
	}
}

// taintAttributionAgentKey is the context key for WithTaintAttributionAgent.
type taintAttributionAgentKey struct{}

// WithTaintAttributionAgent overrides which agent bead's taints are consulted
// and propagated by the write-taint-propagation hooks (taintContextOf via
// resolveTaintAgentBeadID), WITHOUT touching AuthIdentityFromContext(ctx).
//
// This exists for escalation replay (docs/adr/taints.md §4.2 row 7): a
// replayed create/update runs under the APPROVER's identity (replayCtx is
// WithMachineOperator(ctx), which only overrides the operated_by FK, not
// AuthIdentity), but the write's inherit:write/inherit:create taint
// propagation must attribute to the original REQUESTING agent instead.
// Ownership checks, CreatedBy stamping, and audit — which read
// AuthIdentityFromContext directly — are deliberately unaffected and keep
// seeing the approver.
func WithTaintAttributionAgent(ctx context.Context, agentBeadID string) context.Context {
	if agentBeadID == "" {
		return ctx
	}
	return context.WithValue(ctx, taintAttributionAgentKey{}, agentBeadID)
}

// resolveTaintAgentBeadID resolves the agent bead id whose taints should be
// consulted for ctx: the WithTaintAttributionAgent override when set, else
// the calling static-token identity's subject, per ADR docs/adr/taints.md
// §4.1. ok is false when ctx carries no agent taint context at all (an OIDC
// human, the primary static token subject=="static", or a fleet token, with
// no override set) — principals hold grants, they do not carry taints.
func resolveTaintAgentBeadID(ctx context.Context) (agentBeadID string, ok bool) {
	if v, vok := ctx.Value(taintAttributionAgentKey{}).(string); vok && v != "" {
		return v, true
	}
	return actingAgentRef(ctx)
}

// taintContextOf resolves the taint context of the calling principal, per
// ADR docs/adr/taints.md §4.1: a static token whose subject is an agent bead
// id resolves to the taints currently on that agent bead (the agent bead is
// the session carrier). An OIDC human, the primary static token
// (subject == "static"), or a fleet token resolves to an empty set —
// principals hold grants, they do not carry taints. See
// WithTaintAttributionAgent for the escalation-replay override.
//
// The agent-bead lookup is cached per agent bead id for identityCacheTTL;
// a cache hit performs no query against bead_taints.
func (s *BeadsServer) taintContextOf(ctx context.Context) (TaintSet, error) {
	agentBeadID, ok := resolveTaintAgentBeadID(ctx)
	if !ok {
		return TaintSet{}, nil
	}

	s.initTaintContextCache()
	if cached, ok := s.taintContextCache.get(agentBeadID); ok {
		return cached, nil
	}

	taintIDs, err := s.store.GetAllTaintsForBead(ctx, agentBeadID)
	if err != nil {
		return nil, err
	}
	set := make(TaintSet, len(taintIDs))
	for _, t := range taintIDs {
		set[t] = true
	}
	s.taintContextCache.mu.Lock()
	s.taintContextCache.loaderCalls++
	s.taintContextCache.mu.Unlock()
	s.taintContextCache.set(agentBeadID, set)
	return set, nil
}
