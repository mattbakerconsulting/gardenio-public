package main

// Per-Server memo for gardener owner resolution (perf regression fix,
// ka-0000003665). The per-user gardener ownership feature (ka-0000002818)
// resolves every gardener row's owner through resolveGardenerOwner, whose two
// strong-key tiers each hit the daemon UNcached, once per row:
//
//   - ownerFromUserBead    → GetBead(userBeadID) per row
//   - ownerFromSlackUserID → LookupOperatorByAlias(slack, id) per row PLUS
//                            directoryDisplayForOperator → ListOperators,
//                            a FULL operator-directory scan, per row
//
// On /gardeners (up to gardenersListLimit=500 rows) and the /u/{slug}
// "their gardeners" section this is an O(rows) fan of serial RTTs, and the
// ListOperators leg refetches the entire directory for every Slack-owned
// gardener. The operatorSlugCache memoizes only the slug leg, so it never
// covered this.
//
// gardenerOwnerCache collapses that cost two ways:
//
//  1. Per-input memo — ownerFromUserBead / ownerFromSlackUserID results are
//     keyed by their input id, so repeated owners (the common case: a handful
//     of operators own dozens of gardeners) resolve to zero daemon calls after
//     the first.
//  2. Directory memo — the operator directory (operator id → display) is loaded
//     ONCE per TTL window and shared across every row, so even a worst-case set
//     of all-distinct Slack owners pays a single ListOperators scan rather than
//     one per row.
//
// Entries carry a short TTL so a display/slug edit becomes visible without a
// process restart — unlike the permanent operatorSlugCache, gardener owner
// displays are mutable directory fields and should not pin to first-seen for
// the Server's lifetime. The TTL is generous (minutes) because a single page
// render touches the cache hundreds of times; the win is intra-request and
// intra-minute, where staleness is immaterial.

import (
	"context"
	"sync"
	"time"
)

// gardenerOwnerTTL bounds how long a memoized owner resolution (or the loaded
// operator directory) is reused before a fresh daemon lookup. Minutes is the
// right order: long enough that a /gardeners render and the bursts of repeated
// renders around it are fully served from cache, short enough that a directory
// edit shows up promptly.
const gardenerOwnerTTL = 2 * time.Minute

// gardenerOwnerResolution is one memoized tier result: the (display, slug, ok)
// triple returned by ownerFromUserBead / ownerFromSlackUserID, plus its expiry.
type gardenerOwnerResolution struct {
	display string
	slug    string
	ok      bool
	expires time.Time
}

// gardenerOwnerCache memoizes the per-row owner-resolution daemon calls. All
// maps are guarded by mu. nil-safe: a nil *gardenerOwnerCache reports every
// lookup as a miss so callers fall through to the daemon (used by the zero
// value before lazy init).
type gardenerOwnerCache struct {
	mu sync.Mutex

	// userBead memoizes ownerFromUserBead keyed by user bead id.
	userBead map[string]gardenerOwnerResolution
	// slack memoizes ownerFromSlackUserID keyed by Slack user id.
	slack map[string]gardenerOwnerResolution

	// dir is the operator directory (operator id → display) loaded as a single
	// batch; dirExpires bounds its reuse. dirLoaded distinguishes a loaded-empty
	// directory from a never-loaded one.
	dir        map[int64]string
	dirLoaded  bool
	dirExpires time.Time
}

func newGardenerOwnerCache() *gardenerOwnerCache {
	return &gardenerOwnerCache{
		userBead: make(map[string]gardenerOwnerResolution),
		slack:    make(map[string]gardenerOwnerResolution),
	}
}

// getUserBead returns the memoized ownerFromUserBead result for a user bead id,
// or ok=false when absent or expired.
func (c *gardenerOwnerCache) getUserBead(id string, now time.Time) (gardenerOwnerResolution, bool) {
	if c == nil {
		return gardenerOwnerResolution{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.userBead[id]
	if !ok || now.After(e.expires) {
		return gardenerOwnerResolution{}, false
	}
	return e, true
}

// setUserBead memoizes an ownerFromUserBead result for a user bead id.
func (c *gardenerOwnerCache) setUserBead(id string, res gardenerOwnerResolution, now time.Time) {
	if c == nil {
		return
	}
	res.expires = now.Add(gardenerOwnerTTL)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.userBead[id] = res
}

// getSlack returns the memoized ownerFromSlackUserID result for a Slack user
// id, or ok=false when absent or expired.
func (c *gardenerOwnerCache) getSlack(id string, now time.Time) (gardenerOwnerResolution, bool) {
	if c == nil {
		return gardenerOwnerResolution{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.slack[id]
	if !ok || now.After(e.expires) {
		return gardenerOwnerResolution{}, false
	}
	return e, true
}

// setSlack memoizes an ownerFromSlackUserID result for a Slack user id.
func (c *gardenerOwnerCache) setSlack(id string, res gardenerOwnerResolution, now time.Time) {
	if c == nil {
		return
	}
	res.expires = now.Add(gardenerOwnerTTL)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.slack[id] = res
}

// directoryDisplay returns the cached display for an operator id, loading the
// whole directory through loadDir on a cold or expired cache. A single load
// serves every row in a /gardeners render. Returns "" when the operator has no
// directory entry (a cached negative — the empty string is still a hit).
func (c *gardenerOwnerCache) directoryDisplay(ctx context.Context, operatorID int64, now time.Time, load func(context.Context) map[int64]string) string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	if !c.dirLoaded || now.After(c.dirExpires) {
		c.mu.Unlock()
		// Load outside the lock — a daemon RTT must not serialize other rows'
		// cache hits. A concurrent loser simply reloads; both writes are
		// idempotent (same directory) so the last writer wins harmlessly.
		loaded := load(ctx)
		c.mu.Lock()
		c.dir = loaded
		c.dirLoaded = true
		c.dirExpires = now.Add(gardenerOwnerTTL)
	}
	display := c.dir[operatorID]
	c.mu.Unlock()
	return display
}
