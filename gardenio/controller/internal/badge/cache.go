package badge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"gardenio/controller/internal/heraldry"
)

// cache.go — content-addressable, reproducible badge caching (Phase 2 task
// ka-0000005930 under epic ka-0000002812; keyed on arms by gd-NyYxIxxbuw).
//
// WHY A CACHE. The Tier-2 (model-authored) badge is the recognizable one, but
// it costs a model round-trip and — unlike Tier-0 — is NOT byte-stable: ask the
// model twice and you may get two different (both valid) glyphs. That is fine
// for a one-off render but wrong for /plans, where a plan's badge must not
// silently change every time the page is rebuilt. The cache pins the FIRST
// good model badge for a given subject and serves it forever after, so a
// bead's icon is stable across restarts and cheap on the hot path.
//
// CONTENT-ADDRESSED ON WHAT THE PICTURE IS DRAWN FROM. The cache key is the
// SHA-256 of a CacheSubject: the bead id, its title, the canonical run of the
// arms it is drawn from, the style, and PromptVersion. Each is something the
// picture depends on, so changing any of them lands on a DIFFERENT key, misses
// the cache, and redraws: a retitle changes the subject (plan ka-0000005980
// Risks section), new arms change the charges the model composes, a poster is
// a different picture from the line-art glyph a 24px slot shows, and a
// reworded prompt asks for a different picture. The stale object is simply
// orphaned (harmless; S3 lifecycle can sweep it), never served.
//
// TIER-0 IS NOT CACHED. Tier0(id, title) is already a pure, zero-cost, byte-
// stable function — recomputing it is cheaper than a network GET, so caching it
// would only waste storage. Only Tier-2 (model) output is written through. But
// a read hit is served regardless of tier, so once a model badge is pinned it
// keeps being served even if the model later becomes unreachable.

// TierCached marks a badge that came back from the content-address cache rather
// than being freshly generated. The stored bytes are whatever tier originally
// produced them (in practice always TierModel, since Tier-0 is never written);
// the distinct label lets a caller see a cache hit without re-deriving it.
const TierCached = "cached"

// badgeCachePrefix is the path segment under "<prefix>/images/" that badge
// objects live in, keeping them grouped and out of the way of ad-hoc
// screenshot uploads that share the same images/ tree. Objects written before
// the key took in the arms sit under "plan-badges/"; they are still under
// images/, so the viewer proxy keeps serving any URL that points at one, but
// no new key reaches them.
const badgeCachePrefix = "badges"

// CacheSubject is everything a cached badge is drawn from, and so everything
// its cache key hashes.
type CacheSubject struct {
	// ID and Title are the bead's.
	ID    string
	Title string
	// Arms is the resolved arms in a stored form ("" for none). The key takes
	// its canonical run (CanonicalArms), so two spellings of the same arms
	// share a picture.
	Arms string
	// Style is the art direction the model was asked for (Style*).
	Style string
}

// CanonicalArms is the canonical run of stored arms, the form the cache key
// hashes. Arms that do not parse give "": the prompt ignores them and draws
// from the title and description (buildPrompt), so the picture is the armless
// one and shares its key.
func CanonicalArms(stored string) string {
	arms, err := heraldry.Parse(stored)
	if err != nil {
		return ""
	}
	return arms.Canonical()
}

// CacheKey returns the content address for a badge: the lowercase hex SHA-256
// of the subject's id, title, canonical arms, style and PromptVersion, joined
// by NUL bytes. The NUL separator makes the encoding unambiguous — no two
// subjects can collide by shifting bytes across a boundary. Same subject ->
// same key forever; change any part and the key changes, which is the whole
// basis of the cache's reproducibility-and-invalidation contract.
func CacheKey(s CacheSubject) string {
	parts := []string{s.ID, s.Title, CanonicalArms(s.Arms), s.Style, strconv.Itoa(PromptVersion)}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

// CacheObjectKey returns the storage key a badge for subject s lives at under
// prefix:
//
//	<prefix>/images/badges/<id>/<cachekey>.svg
//
// prefix is the deployment's object-store prefix (COOP_S3_PREFIX, default
// "coop/sessions"), so badges land under the same coop/sessions/images/ tree
// the viewer image proxy already serves. The bead id is included as a path
// segment purely for human inspectability (all of a bead's badge revisions
// sort together); correctness rests entirely on the content-addressed
// cachekey, so two subjects for the same id never overwrite each other. The id
// segment is sanitized to keep the key path-safe.
func CacheObjectKey(prefix string, s CacheSubject) string {
	return fmt.Sprintf("%s/%s.svg", BadgeDir(prefix, s.ID), CacheKey(s))
}

// BadgeDir is the directory every stored picture of bead id lives in,
// "<prefix>/images/badges/<id>": the cached SVGs and the files gb badge
// --upload writes beside them.
func BadgeDir(prefix, id string) string {
	prefix = strings.TrimRight(prefix, "/")
	return fmt.Sprintf("%s/images/%s/%s", prefix, badgeCachePrefix, cachePathSegment(id))
}

// cachePathSegment sanitizes a bead id into a single path segment: it keeps
// alphanumerics, dot, dash and underscore and collapses anything else to '-',
// so an unexpected id can never inject a '/' or escape the badge prefix. An
// empty id degrades to "unknown" so the key is always well-formed.
func cachePathSegment(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return "unknown"
	}
	var b strings.Builder
	b.Grow(len(id))
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := b.String()
	if out == "" {
		return "unknown"
	}
	return out
}

// BadgeStore is the object-storage backend the cache reads and writes badges
// through. Get returns found=false (with a nil error) for a plain miss, and a
// non-nil error only for a real backend failure. Defining it as an interface
// keeps the badge package free of any S3 dependency: the gb CLI supplies an
// S3-backed implementation, and tests supply an in-memory one.
type BadgeStore interface {
	// Get returns the stored SVG bytes for key. found is false with a nil
	// error when the object simply does not exist.
	Get(ctx context.Context, key string) (svg []byte, found bool, err error)
	// Put stores svg at key, overwriting any existing object.
	Put(ctx context.Context, key string, svg []byte) error
}

// LoadOrGenerate returns a badge for subject s, reading through the
// content-address cache in store.
//
// Flow:
//  1. Compute the subject's object key and, unless refresh is set, try
//     store.Get.
//  2. On a hit, return the cached SVG as a TierCached badge WITHOUT generating.
//  3. On a miss (or refresh), call generate (the model+Tier0 pipeline), and —
//     only if it produced a model-authored (non-deterministic) badge — write
//     it back so the next read is a hit. Tier-0 fallbacks are not written:
//     they are free to recompute and pinning one would mask a
//     later-recoverable model.
//
// refresh is the owner asking for a redraw: the cached picture is not read,
// and a new model picture overwrites it. A refresh that falls back to Tier-0
// leaves the cached picture in place.
//
// The returned bool is true on a cache hit. Store errors are ADVISORY and
// returned alongside a valid badge: a read error falls through to generate, and
// a write error still returns the freshly generated badge — a flaky object
// store can never deny a bead its icon. generate itself must always return a
// usable Badge (the badge package's whole contract); LoadOrGenerate only propagates a
// generate error, never fabricates one.
func LoadOrGenerate(
	ctx context.Context,
	store BadgeStore,
	prefix string,
	s CacheSubject,
	refresh bool,
	generate func(context.Context) (Badge, error),
) (Badge, bool, error) {
	key := CacheObjectKey(prefix, s)

	if store != nil && !refresh {
		// A read error is advisory: a flaky read is handled exactly like a
		// miss — fall through and regenerate rather than deny the bead a
		// badge. Only a genuine hit with non-empty bytes short-circuits.
		if data, found, err := store.Get(ctx, key); err == nil && found && len(data) > 0 {
			return Badge{SVG: string(data), Tier: TierCached}, true, nil
		}
	}

	badge, genErr := generate(ctx)
	if genErr != nil {
		// generate still returns a valid (Tier-0) badge on failure; hand both
		// back so the caller can log the reason without losing the badge.
		return badge, false, genErr
	}

	// Only pin model-authored badges: Tier-0 is byte-stable and free to
	// recompute, so caching it wastes storage and would keep serving the
	// fallback after the model recovers.
	if store != nil && badge.Tier == TierModel && badge.SVG != "" {
		if err := store.Put(ctx, key, []byte(badge.SVG)); err != nil {
			// Advisory: return the badge and the write error together.
			return badge, false, fmt.Errorf("badge: caching badge at %s: %w", key, err)
		}
	}

	return badge, false, nil
}
