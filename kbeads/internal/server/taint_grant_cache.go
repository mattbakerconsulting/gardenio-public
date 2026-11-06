package server

import (
	"slices"
	"strings"
	"sync"
	"time"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// grantSetCacheTTL bounds how long a principal's resolved taint grant set
// (grantsOf) is trusted. Every write on this process that can change a grant
// set invalidates it explicitly (invalidateGrantSetsForEvent, plus the
// assign-grant, agent-close-revoke and taint-create call sites); the TTL is
// the backstop for writes on another replica and for operator_aliases links,
// which nothing here watches. It is the same staleness bound the registry
// cache takes (taintRegistryEmptyTTL), and grants expire in days (ADR
// docs/adr/taints.md §10.4), so 30 s is within the gate's tolerance
// (gd-eawnzZZEjV).
const grantSetCacheTTL = 30 * time.Second

// spawnChainRootCacheTTL bounds how long a resolved spawn-chain root is
// trusted. spawned_by is set at spawn and never rewritten, so the TTL only
// covers a root agent or user bead whose human fields are edited later.
const spawnChainRootCacheTTL = 10 * time.Minute

// principalCacheMaxEntries caps a principalCache. Past it, set drops expired
// entries, and if that frees nothing, the whole map: every agent ever seen is
// a key, so an unpruned map would grow for the life of the process.
const principalCacheMaxEntries = 4096

type principalCacheEntry[V any] struct {
	cachedAt time.Time
	value    V
}

// principalCache memoises a per-principal resolution for a bounded TTL.
//
// gen makes invalidation safe against a concurrent miss: load reads gen
// before it queries the store and installs its result only if no
// invalidation ran in between, so a miss that raced a grant write can never
// re-install the pre-write value. A nil *principalCache caches nothing.
type principalCache[V any] struct {
	ttl time.Duration
	now func() time.Time

	mu      sync.Mutex
	gen     uint64
	entries map[string]principalCacheEntry[V]

	// loaderCalls counts resolutions run on a cache miss. For tests.
	loaderCalls int64
}

func newPrincipalCache[V any](ttl time.Duration) *principalCache[V] {
	return &principalCache[V]{
		ttl:     ttl,
		now:     time.Now,
		entries: make(map[string]principalCacheEntry[V]),
	}
}

// load returns key's live cached value, or runs resolve and caches what it
// returns. cacheable decides whether a resolved value may be installed; an
// error is never cached.
func (c *principalCache[V]) load(key string, resolve func() (V, error), cacheable func(V) bool) (V, error) {
	if c == nil {
		return resolve()
	}
	c.mu.Lock()
	if e, ok := c.entries[key]; ok && c.now().Sub(e.cachedAt) < c.ttl {
		c.mu.Unlock()
		return e.value, nil
	}
	gen := c.gen
	c.loaderCalls++
	c.mu.Unlock()

	v, err := resolve()
	if err != nil || (cacheable != nil && !cacheable(v)) {
		return v, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if gen != c.gen {
		return v, nil
	}
	if len(c.entries) >= principalCacheMaxEntries {
		now := c.now()
		for k, e := range c.entries {
			if now.Sub(e.cachedAt) >= c.ttl {
				delete(c.entries, k)
			}
		}
		if len(c.entries) >= principalCacheMaxEntries {
			clear(c.entries)
		}
	}
	c.entries[key] = principalCacheEntry[V]{cachedAt: c.now(), value: v}
	return v, nil
}

// invalidate drops keys, or every entry when keys is empty, and fences off
// any resolution already in flight.
func (c *principalCache[V]) invalidate(keys ...string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gen++
	if len(keys) == 0 {
		clear(c.entries)
		return
	}
	for _, k := range keys {
		delete(c.entries, k)
	}
}

// LoaderCalls returns the cumulative cache-miss count. For tests.
func (c *principalCache[V]) LoaderCalls() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.loaderCalls
}

// Grant-set cache keys, one namespace per principal kind grantsOf resolves.
// NUL cannot appear in a bead id, fleet subject, operator alias or group.
func agentGrantSetKey(agentBeadID string) string { return "agent\x00" + agentBeadID }

// agentOwnGrantSetKey keys agentOwnGrants: the agent's own rows, without its
// spawn-chain root's.
func agentOwnGrantSetKey(agentBeadID string) string { return "agent-own\x00" + agentBeadID }

func fleetGrantSetKey(subject string) string { return "fleet\x00" + subject }

// operatorGrantSetKey keys an operator's grant set by alias and token groups:
// teams match on oidc_groups (teamsForOperator), so the same human with a
// different group claim can hold a different set.
func operatorGrantSetKey(kind, subject string, groups []string) string {
	sorted := slices.Clone(groups)
	slices.Sort(sorted)
	return "operator\x00" + kind + "\x00" + subject + "\x00" + strings.Join(sorted, "\x00")
}

// invalidateAgentGrantSet drops one agent's cached grant set after a write
// that grants or revokes on that agent bead id alone.
func (s *BeadsServer) invalidateAgentGrantSet(agentBeadID string) {
	s.grantSetCache.invalidate(agentGrantSetKey(agentBeadID), agentOwnGrantSetKey(agentBeadID))
}

// invalidateAllGrantSets drops every cached grant set, and the team
// membership cache under it, after a write whose reach is not one principal:
// a team, taint ownership, or a grant to a team, operator or fleet (which an
// agent inherits through its spawn-chain root).
func (s *BeadsServer) invalidateAllGrantSets() {
	s.grantSetCache.invalidate()
	if s.teamMembershipCache != nil {
		s.teamMembershipCache.mu.Lock()
		clear(s.teamMembershipCache.entries)
		s.teamMembershipCache.mu.Unlock()
	}
}

// invalidateGrantSetsForEvent is recordAndPublish's hook: a taint grant or
// revoke, a bead delete (the event does not say whether it was a team), or a
// create, update or close of a type:team bead clears every cached grant set.
func (s *BeadsServer) invalidateGrantSetsForEvent(topic string, event any) {
	switch topic {
	case events.TopicTaintGranted, events.TopicTaintRevoked, events.TopicBeadDeleted:
		s.invalidateAllGrantSets()
		return
	}
	var bead *model.Bead
	switch e := event.(type) {
	case events.BeadCreated:
		bead = e.Bead
	case events.BeadUpdated:
		bead = e.Bead
	case events.BeadClosed:
		bead = e.Bead
	}
	if bead != nil && bead.Type == model.TypeTeam {
		s.invalidateAllGrantSets()
	}
}
