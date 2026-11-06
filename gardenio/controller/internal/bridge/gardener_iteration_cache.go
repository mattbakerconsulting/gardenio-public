package bridge

import (
	"cmp"
	"context"
	"log/slog"
	"slices"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// Closed-iteration cache for the trigger-engine (gd-6PXLuR3juD).
//
// gardenerFinalizeRunsFromIterations and gardenerReconcileRunLogFromIterations
// each re-listed every gardener's closed iteration agents by both owner keys on
// every 30 s cycle: four lists of up to 500 agents per gardener per cycle, about
// 12 MB a cycle on 2026-10-24, which is most of the trigger-engine's 33.6 GB/day
// of GET /v1/beads. Closed iterations hardly ever change, so the engine keeps
// each (gardener, owner key) set and reads only the delta: the agents with that
// owner key updated since the last read, in any status. A closed one is upserted
// and anything else is dropped, so an iteration that is reopened leaves the set
// the same cycle. A full re-list every iterationFullRelistEvery (and whenever a
// delta fills its page) repairs anything a delta missed.
//
// The result is what the full list returns: the newest
// gardenerIterationFinalizeLimit closed iterations by created_at, the same beads
// with the same lean projection. A delta only ever adds iterations at the new
// end, so the view keeps converging on the full list as runs accumulate.

const (
	// iterationFullRelistEvery bounds how stale a cached set can get if a delta
	// misses an update (clock skew past iterationCursorSlack, a long commit).
	// One full pass over every gardener is about 6 MB.
	iterationFullRelistEvery = 30 * time.Minute

	// iterationCursorSlack steps each delta's updated_after back from the time
	// the previous read started. It covers engine-to-Postgres clock skew,
	// updated_at being stamped before commit, and the second precision of the
	// updated_after parameter (strict >, RFC3339 without fractions), which on
	// its own would skip a bead updated in the cursor's second (gd-o7niMQlohc).
	// Re-reading a bead is harmless: the delta upserts by id.
	iterationCursorSlack = 10 * time.Second

	// iterationCacheIdleEvict drops the set of a gardener nobody has asked
	// about for this long (closed or deleted gardeners).
	iterationCacheIdleEvict = 2 * iterationFullRelistEvery
)

// gardenerIterationOwnerKeys are the two fields the spawn paths stamp with the
// owning gardener's id: trigger_id (bridge path) and schedule_id (legacy
// scheduler path).
var gardenerIterationOwnerKeys = []string{"trigger_id", "schedule_id"}

// closedIterationSource is implemented by a daemon that can answer
// gardenerClosedIterations from a cache. gardenerClosedIterations uses it when
// the daemon it is handed has one, and lists directly otherwise.
type closedIterationSource interface {
	closedIterations(ctx context.Context, logger *slog.Logger, gardenerID string) []*beadsapi.BeadDetail
}

// iterationCachingDaemon is the daemon evalOne hands the iteration reconcilers:
// every gardenerDaemon method goes straight to the engine's daemon, and
// gardenerClosedIterations is answered from the engine's cache.
type iterationCachingDaemon struct {
	TriggerBeadClient
	cache *closedIterationCache
}

func (d iterationCachingDaemon) closedIterations(ctx context.Context, logger *slog.Logger, gardenerID string) []*beadsapi.BeadDetail {
	return d.cache.closedIterations(ctx, d.TriggerBeadClient, logger, gardenerID)
}

// iterationDaemon is the daemon evalOne hands the iteration reconcilers: the
// engine's daemon, with closed iterations served from the engine's cache when
// it has one.
func (e *TriggerEngine) iterationDaemon() gardenerDaemon {
	if e.iterations == nil {
		return e.daemon
	}
	return iterationCachingDaemon{TriggerBeadClient: e.daemon, cache: e.iterations}
}

// closedIterationCache holds each gardener's closed iterations per owner key.
type closedIterationCache struct {
	now func() time.Time

	mu   sync.Mutex
	sets map[iterationSetKey]*iterationSet
}

type iterationSetKey struct {
	gardenerID string
	ownerKey   string
}

type iterationSet struct {
	beads    map[string]*beadsapi.BeadDetail
	cursor   time.Time // updated_after for the next delta
	listedAt time.Time // last full list
	usedAt   time.Time
}

func newClosedIterationCache(now func() time.Time) *closedIterationCache {
	if now == nil {
		now = time.Now
	}
	return &closedIterationCache{now: now, sets: make(map[iterationSetKey]*iterationSet)}
}

// closedIterations is gardenerClosedIterations over the cache: the union of
// both owner keys' sets, deduped by id, newest first. The slice is new on
// every call, so callers may sort it in place; the beads are shared and must
// not be modified.
func (c *closedIterationCache) closedIterations(ctx context.Context, daemon gardenerDaemon, logger *slog.Logger, gardenerID string) []*beadsapi.BeadDetail {
	if gardenerID == "" {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	c.evictIdle(now)

	out := make([]*beadsapi.BeadDetail, 0, gardenerIterationFinalizeLimit)
	seen := make(map[string]bool)
	for _, key := range gardenerIterationOwnerKeys {
		for _, b := range c.refresh(ctx, daemon, logger, iterationSetKey{gardenerID, key}, now) {
			if seen[b.ID] {
				continue
			}
			seen[b.ID] = true
			out = append(out, b)
		}
	}
	return out
}

// refresh brings one (gardener, owner key) set up to date and returns it in
// the full list's order and bound. A failed read keeps the last known set,
// so a daemon blip neither empties nor shrinks it.
func (c *closedIterationCache) refresh(ctx context.Context, daemon gardenerDaemon, logger *slog.Logger, k iterationSetKey, now time.Time) []*beadsapi.BeadDetail {
	set := c.sets[k]
	if set == nil || now.Sub(set.listedAt) >= iterationFullRelistEvery {
		if full, ok := listClosedIterations(ctx, daemon, logger, k.gardenerID, k.ownerKey); ok {
			set = &iterationSet{beads: make(map[string]*beadsapi.BeadDetail, len(full)), listedAt: now}
			for _, b := range full {
				set.beads[b.ID] = b
			}
			set.cursor = now.Add(-iterationCursorSlack)
			c.sets[k] = set
		}
		if set == nil {
			return nil
		}
		set.usedAt = now
		return set.view()
	}
	set.usedAt = now

	res, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:        []string{"agent"},
		Fields:       map[string]string{k.ownerKey: k.gardenerID},
		UpdatedAfter: set.cursor,
		Sort:         "updated_at",
		Limit:        gardenerIterationFinalizeLimit,
		Omit:         leanAgentOmit,
		OmitFields:   leanAgentOmitFields,
	})
	if err != nil {
		logger.Warn("gardener finalize-iterations: delta list failed, using the cached set",
			"trigger", k.gardenerID, "key", k.ownerKey, "error", err)
		return set.view()
	}
	if res != nil && len(res.Beads) >= gardenerIterationFinalizeLimit {
		// The delta filled its page, so it may have been cut short: re-list.
		set.listedAt = time.Time{}
		return c.refresh(ctx, daemon, logger, k, now)
	}
	set.cursor = now.Add(-iterationCursorSlack)
	if res != nil {
		for _, b := range res.Beads {
			if b == nil || b.ID == "" || b.Fields[k.ownerKey] != k.gardenerID {
				continue
			}
			if b.Status == "closed" {
				set.beads[b.ID] = b
			} else {
				delete(set.beads, b.ID)
			}
		}
	}
	return set.view()
}

// view returns the set as the full list would: newest first by created_at
// (the daemon breaks ties by ascending id), at most
// gardenerIterationFinalizeLimit beads.
func (s *iterationSet) view() []*beadsapi.BeadDetail {
	out := make([]*beadsapi.BeadDetail, 0, len(s.beads))
	for _, b := range s.beads {
		out = append(out, b)
	}
	slices.SortFunc(out, func(a, b *beadsapi.BeadDetail) int {
		if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	})
	if len(out) > gardenerIterationFinalizeLimit {
		out = out[:gardenerIterationFinalizeLimit]
	}
	return out
}

func (c *closedIterationCache) evictIdle(now time.Time) {
	for k, s := range c.sets {
		if now.Sub(s.usedAt) >= iterationCacheIdleEvict {
			delete(c.sets, k)
		}
	}
}
