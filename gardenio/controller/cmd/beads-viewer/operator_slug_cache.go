package main

import (
	"context"
	"sync"

	"gardenio/controller/internal/beadsapi"
)

// operatorSlugCache is a tiny per-Server memo from operators(id) FK to the
// canonical homepage slug returned by the daemon's GetOperatorSlug endpoint.
// It backs the per-user-homepage M2 §6 (ka-0000007787) chip rewrite: every
// list/detail handler that renders an operator chip funnels its `*BeadDetail`
// slice through populateOperatedBySlugs, which fills `bead.OperatedBySlug`
// from this cache so the renderer can emit `<a href="/u/{slug}">` instead of
// the legacy `/search?operated_by=…` filter href.
//
// The cache stores both the slug *and* the no-slug-known sentinel (empty
// string + present in map) so that operators without a canonical slug don't
// hit the daemon once per row on every page render. Missing entries fall
// back to the migration-028 opaque sentinel via slugRefForOperator.
type operatorSlugCache struct {
	mu sync.RWMutex
	m  map[int64]string
}

func newOperatorSlugCache() *operatorSlugCache {
	return &operatorSlugCache{m: make(map[int64]string)}
}

// get returns (slug, true) if the cache has a hit for the operator id;
// otherwise ("", false). A cached empty slug is still a cache hit — the
// caller must not re-issue the daemon lookup.
func (c *operatorSlugCache) get(operatorID int64) (string, bool) {
	if c == nil {
		return "", false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.m[operatorID]
	return v, ok
}

func (c *operatorSlugCache) set(operatorID int64, slug string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[operatorID] = slug
}

// resolveOperatorSlug returns the canonical homepage slug for the given
// operator id, falling back to the `op-<id>` opaque sentinel when the
// daemon does not have a canonical slug on file. Read-through-cached; a
// failed daemon lookup returns the sentinel without caching the failure.
func (s *Server) resolveOperatorSlug(ctx context.Context, operatorID int64) string {
	if operatorID == 0 {
		return ""
	}
	if s.operatorSlugCache == nil {
		s.operatorSlugCache = newOperatorSlugCache()
	}
	// The cache is shared across principals; a scoped one neither reads an
	// entry an admin filled nor fills one (its lookup is refused by the daemon
	// anyway, so it renders the opaque sentinel).
	scoped := isScopedCtx(ctx)
	if slug, ok := s.operatorSlugCache.get(operatorID); ok && !scoped {
		if slug != "" {
			return slug
		}
		return slugRefForOperator(operatorID)
	}
	resp, err := s.daemon.GetOperatorSlug(ctx, operatorID)
	if err != nil {
		// Don't cache the failure — the next request may succeed.
		s.logger.Debug("operator slug lookup failed", "operator_id", operatorID, "error", err)
		return slugRefForOperator(operatorID)
	}
	slug := ""
	if resp != nil && resp.Found {
		slug = resp.Slug
	}
	if !scoped {
		s.operatorSlugCache.set(operatorID, slug)
	}
	if slug != "" {
		return slug
	}
	return slugRefForOperator(operatorID)
}

// populateOperatedBySlugs walks a slice of beads and fills each bead's
// OperatedBySlug field with the canonical homepage slug, so that the
// operator-chip renderer can emit /u/{slug} links. Beads without an
// OperatedByID FK are skipped (the chip falls through to the legacy
// FilterHref path). Safe to call with a nil slice.
func (s *Server) populateOperatedBySlugs(ctx context.Context, beads []*beadsapi.BeadDetail) {
	for _, b := range beads {
		if b == nil || b.OperatedByID == nil || *b.OperatedByID == 0 {
			continue
		}
		if b.OperatedBySlug != "" {
			continue
		}
		b.OperatedBySlug = s.resolveOperatorSlug(ctx, *b.OperatedByID)
	}
}

// populateOperatedBySlug fills a single bead's OperatedBySlug field. Used by
// detail handlers that operate on one bead rather than a slice. Nil-safe.
func (s *Server) populateOperatedBySlug(ctx context.Context, bead *beadsapi.BeadDetail) {
	if bead == nil || bead.OperatedByID == nil || *bead.OperatedByID == 0 {
		return
	}
	if bead.OperatedBySlug != "" {
		return
	}
	bead.OperatedBySlug = s.resolveOperatorSlug(ctx, *bead.OperatedByID)
}
