package decisionview

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// EvidenceCache is LoadEvidence behind a short cache, so a surface reads a
// decision's evidence once per render and a page that lists the same decision
// twice, or a refresh a few seconds later, reads it once (gd-Pi7C83262N).
//
// An entry is the caller's: the key carries the request's user bearer and
// operator (beadsapi.WithUserBearer / WithOperator), so one viewer is never
// served the images another viewer's reads found. The key also carries the
// decision's evidence ids, so a decision that names new evidence is read
// again at once.
type EvidenceCache struct {
	ttl     time.Duration
	timeout time.Duration
	now     func() time.Time

	mu      sync.Mutex
	entries map[string]evidenceEntry
}

type evidenceEntry struct {
	imgs []Image
	at   time.Time
}

const (
	// EvidenceTTL is how long a decision's evidence is trusted. A screenshot
	// posted to the task after the ask shows within it.
	EvidenceTTL = time.Minute
	// evidenceTimeout bounds one decision's reads: a slow daemon costs the
	// strip, never the decision it hangs under.
	evidenceTimeout = 3 * time.Second
	// evidenceCacheMax bounds the cache; past it, expired entries are dropped,
	// and then everything.
	evidenceCacheMax = 2048
)

// NewEvidenceCache returns a cache that trusts an entry for ttl
// (EvidenceTTL when ttl <= 0).
func NewEvidenceCache(ttl time.Duration) *EvidenceCache {
	if ttl <= 0 {
		ttl = EvidenceTTL
	}
	return &EvidenceCache{ttl: ttl, timeout: evidenceTimeout, now: time.Now, entries: map[string]evidenceEntry{}}
}

// Load is LoadEvidence for the caller on ctx, from the cache when a load of
// the same decision and evidence ids is younger than the TTL. A nil cache
// reads every time. A decision with no evidence beads reads nothing.
func (c *EvidenceCache) Load(ctx context.Context, r EvidenceReader, decision *beadsapi.BeadDetail, max int) []Image {
	ids := EvidenceBeads(decision)
	if r == nil || len(ids) == 0 {
		return nil
	}
	if c == nil {
		return LoadEvidence(ctx, r, decision, max)
	}
	key := evidenceKey(ctx, decision.ID, ids, max)
	now := c.now()
	c.mu.Lock()
	e, ok := c.entries[key]
	c.mu.Unlock()
	if ok && now.Sub(e.at) < c.ttl {
		return e.imgs
	}
	lctx, cancel := context.WithTimeout(ctx, c.timeout)
	imgs := LoadEvidence(lctx, r, decision, max)
	cancel()
	if ctx.Err() != nil {
		return imgs // the caller gave up: nothing learned worth keeping
	}
	c.mu.Lock()
	if len(c.entries) >= evidenceCacheMax {
		for k, old := range c.entries {
			if now.Sub(old.at) >= c.ttl {
				delete(c.entries, k)
			}
		}
		if len(c.entries) >= evidenceCacheMax {
			c.entries = map[string]evidenceEntry{}
		}
	}
	c.entries[key] = evidenceEntry{imgs: imgs, at: now}
	c.mu.Unlock()
	return imgs
}

// LoadAll is Load for each of decisions, a few at a time, keyed by decision
// id. A decision with no evidence has no entry.
func (c *EvidenceCache) LoadAll(ctx context.Context, r EvidenceReader, decisions []*beadsapi.BeadDetail, max int) map[string][]Image {
	out := map[string][]Image{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for _, d := range decisions {
		if d == nil || len(EvidenceBeads(d)) == 0 {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(d *beadsapi.BeadDetail) {
			defer func() { <-sem; wg.Done() }()
			if imgs := c.Load(ctx, r, d, max); len(imgs) > 0 {
				mu.Lock()
				out[d.ID] = imgs
				mu.Unlock()
			}
		}(d)
	}
	wg.Wait()
	return out
}

// evidenceKey is the cache key: who reads (a digest, so no bearer is kept),
// the decision, its evidence ids and the image cap.
func evidenceKey(ctx context.Context, id string, ids []string, max int) string {
	who := sha256.Sum256([]byte(beadsapi.UserBearerFromContext(ctx) + "\x00" + beadsapi.OperatorFromContext(ctx)))
	return hex.EncodeToString(who[:12]) + "\x00" + id + "\x00" + strings.Join(ids, ",") + "\x00" + strconv.Itoa(max)
}
