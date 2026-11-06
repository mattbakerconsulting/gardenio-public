package server

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"time"

	"gardenio/kbeads/internal/model"
)

// typeConfigCacheTTL bounds how long a cached type:* row is trusted. Writes
// through this daemon's config set/delete drop the entry at once; the TTL
// only covers edits that bypass it (another replica, a direct SQL update).
const typeConfigCacheTTL = 30 * time.Second

type typeConfigCacheEntry struct {
	cachedAt time.Time
	// config is the store row, or nil when the store has none (a negative
	// entry: most type lookups fall through to builtinConfigs).
	config *model.Config
}

// typeConfigCache memoises the store's type:* config rows for
// resolveTypeConfig, which runs on every bead create, update, type-filtered
// list and write-authz check. Without it each of those paid one GetConfig
// query, 97% of which found no row (gd-i3QuMqdgDB). The zero value is ready
// to use, so a BeadsServer literal in a test needs no constructor.
//
// It caches the raw row, not the resolved TypeConfig, so callers still get a
// fresh TypeConfig they may mutate.
type typeConfigCache struct {
	mu      sync.Mutex
	entries map[string]typeConfigCacheEntry
	// gen counts invalidations. A fill that started before one is dropped,
	// so a read racing a config write cannot reinstall the old row.
	gen uint64
	now func() time.Time // nil means time.Now
}

func (c *typeConfigCache) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// lookup returns the cached row for key and true on a live hit, and otherwise
// the generation a later fill must present.
func (c *typeConfigCache) lookup(key string) (*model.Config, bool, uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || c.clock().Sub(e.cachedAt) >= typeConfigCacheTTL {
		return nil, false, c.gen
	}
	return e.config, true, c.gen
}

// fill installs a freshly-loaded row unless an invalidation happened since
// the lookup that returned gen.
func (c *typeConfigCache) fill(key string, cfg *model.Config, gen uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if gen != c.gen {
		return
	}
	if c.entries == nil {
		c.entries = make(map[string]typeConfigCacheEntry)
	}
	c.entries[key] = typeConfigCacheEntry{cachedAt: c.clock(), config: cfg}
}

// invalidate drops key's entry. Keys outside the type: namespace are never
// cached, so they are a no-op.
func (c *typeConfigCache) invalidate(key string) {
	if !strings.HasPrefix(key, "type:") {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gen++
	delete(c.entries, key)
}

// getTypeConfigRow returns the store's row for a type:* key through the
// cache, or nil when the store has none. Store errors other than a missing
// row are returned and not cached.
func (s *BeadsServer) getTypeConfigRow(ctx context.Context, key string) (*model.Config, error) {
	cfg, ok, gen := s.typeConfigs.lookup(key)
	if ok {
		return cfg, nil
	}
	cfg, err := s.store.GetConfig(ctx, key)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		cfg = nil
	}
	s.typeConfigs.fill(key, cfg, gen)
	return cfg, nil
}
