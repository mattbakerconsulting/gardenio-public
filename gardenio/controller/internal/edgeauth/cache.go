package edgeauth

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

// cacheEntry is one PAT-keyed validation outcome. err is set for negative
// outcomes; identity is set for positive outcomes and for the ErrNotInGroup
// negative outcome (where it carries the partial identity resolved from
// /user for audit attribution).
type cacheEntry struct {
	identity *Identity
	err      error
	expires  time.Time
}

// cache is a thread-safe map keyed by a SHA-256 hash of the PAT (so raw
// PATs never live in memory beyond the call stack of Validate). Positive
// and negative outcomes use different TTLs.
type cache struct {
	mu          sync.Mutex
	entries     map[string]cacheEntry
	positiveTTL time.Duration
	negativeTTL time.Duration
	now         func() time.Time
}

func newCache(positive, negative time.Duration, now func() time.Time) *cache {
	return &cache{
		entries:     make(map[string]cacheEntry),
		positiveTTL: positive,
		negativeTTL: negative,
		now:         now,
	}
}

func hashPAT(pat string) string {
	sum := sha256.Sum256([]byte(pat))
	return hex.EncodeToString(sum[:])
}

// get returns (identity, err, true) when the cache holds an unexpired entry
// for the PAT. The identity is non-nil for positive hits and for the
// ErrNotInGroup negative hit (where it carries the partial identity from
// /user). err is non-nil for negative hits. ok=false means the caller
// must validate against GitLab.
func (c *cache) get(pat string) (*Identity, error, bool) {
	key := hashPAT(pat)
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return nil, nil, false
	}
	if c.now().After(entry.expires) {
		delete(c.entries, key)
		return nil, nil, false
	}
	return entry.identity, entry.err, true
}

func (c *cache) putPositive(pat string, id *Identity) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[hashPAT(pat)] = cacheEntry{
		identity: id,
		expires:  c.now().Add(c.positiveTTL),
	}
}

// putNegative stores a negative-cache entry. id is optional: pass a non-nil
// Identity for ErrNotInGroup so the partial identity rides along with the
// cached deny. Other negative outcomes (ErrInvalidToken) pass nil.
func (c *cache) putNegative(pat string, id *Identity, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[hashPAT(pat)] = cacheEntry{
		identity: id,
		err:      err,
		expires:  c.now().Add(c.negativeTTL),
	}
}
