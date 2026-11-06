package badge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// memStore is an in-memory BadgeStore for exercising the cache without S3.
// getErr / putErr, when set, force the corresponding operation to fail so we
// can assert the advisory-error / fall-through behavior.
type memStore struct {
	mu      sync.Mutex
	objects map[string][]byte
	gets    int
	puts    int
	getErr  error
	putErr  error
}

func newMemStore() *memStore { return &memStore{objects: map[string][]byte{}} }

func (m *memStore) Get(_ context.Context, key string) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gets++
	if m.getErr != nil {
		return nil, false, m.getErr
	}
	v, ok := m.objects[key]
	return v, ok, nil
}

func (m *memStore) Put(_ context.Context, key string, svg []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.puts++
	if m.putErr != nil {
		return m.putErr
	}
	cp := make([]byte, len(svg))
	copy(cp, svg)
	m.objects[key] = cp
	return nil
}

// subject is the bead the cache tests draw: plan gd-pQZO9ie11D's arms on a
// line-art badge.
var subject = CacheSubject{ID: "ka-0000002812", Title: "Phase 2 — Generator + cache", Arms: "🏅🖌️🔎", Style: StyleLineArtBold}

// TestCacheKey_ContentAddressed locks the properties the cache rests on: the
// same subject gives the same key, and a change to ANY of what the picture is
// drawn from (id, title, arms, style) gives a different key, so each
// invalidates naturally.
func TestCacheKey_ContentAddressed(t *testing.T) {
	k1 := CacheKey(subject)
	if k2 := CacheKey(subject); k1 != k2 {
		t.Fatalf("same subject must give same key: %q != %q", k1, k2)
	}
	if len(k1) != 64 {
		t.Fatalf("key must be a 64-char hex sha256, got %d chars: %q", len(k1), k1)
	}

	changed := map[string]CacheSubject{}
	s := subject
	s.ID = "gd-OTHER"
	changed["id"] = s
	s = subject
	s.Title = "Phase 2 — retitled"
	changed["title (a retitle)"] = s
	s = subject
	s.Arms = "🔥🧭📦"
	changed["arms"] = s
	s = subject
	s.Arms = "🔎🖌️🏅"
	changed["arms order"] = s
	s = subject
	s.Arms = ""
	changed["arms removed"] = s
	s = subject
	s.Style = StylePoster
	changed["style"] = s
	for what, c := range changed {
		if CacheKey(c) == k1 {
			t.Errorf("a change of %s must change the key", what)
		}
	}
}

// TestCacheKey_HashesPromptVersion: the key is the hash of the subject and
// PromptVersion, so bumping the version moves every key.
func TestCacheKey_HashesPromptVersion(t *testing.T) {
	join := func(v int) string {
		parts := []string{subject.ID, subject.Title, CanonicalArms(subject.Arms), subject.Style, strconv.Itoa(v)}
		sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
		return hex.EncodeToString(sum[:])
	}
	if got := CacheKey(subject); got != join(PromptVersion) {
		t.Fatalf("CacheKey = %s, want the hash of the subject and PromptVersion %d (%s)", got, PromptVersion, join(PromptVersion))
	}
	if join(PromptVersion) == join(PromptVersion+1) {
		t.Fatal("a PromptVersion bump must move the key")
	}
}

// TestCacheKey_CanonicalArms: two spellings of the same arms (with and without
// the emoji presentation selector) are one picture
// and share a key; arms that do not parse are drawn as no arms and share the
// armless key.
func TestCacheKey_CanonicalArms(t *testing.T) {
	bare := subject
	bare.Arms = "🏅🖌🔎"
	if CacheKey(bare) != CacheKey(subject) {
		t.Errorf("arms with and without U+FE0F must share a key")
	}
	armless := subject
	armless.Arms = ""
	junk := subject
	junk.Arms = "not arms"
	if CacheKey(junk) != CacheKey(armless) {
		t.Errorf("unparseable arms are drawn as no arms and must share the armless key")
	}
	if CanonicalArms("not arms") != "" {
		t.Errorf("CanonicalArms of junk = %q, want \"\"", CanonicalArms("not arms"))
	}
}

// TestCacheKey_NoBoundaryCollision guards the NUL separator: without it,
// ("ab","c") and ("a","bc") would hash the same bytes.
func TestCacheKey_NoBoundaryCollision(t *testing.T) {
	if CacheKey(CacheSubject{ID: "ab", Title: "c"}) == CacheKey(CacheSubject{ID: "a", Title: "bc"}) {
		t.Fatal("id/title boundary must be unambiguous (NUL separator)")
	}
}

// TestCacheObjectKey_Layout checks the storage path shape and that the id is
// sanitized into a single safe segment.
func TestCacheObjectKey_Layout(t *testing.T) {
	s := CacheSubject{ID: "ka-0000002812", Title: "A Title", Style: StyleLineArtBold}
	got := CacheObjectKey("coop/sessions", s)
	if want := "coop/sessions/images/badges/ka-0000002812/" + CacheKey(s) + ".svg"; got != want {
		t.Fatalf("object key = %q, want %q", got, want)
	}

	// A trailing slash on the prefix must not double up.
	if CacheObjectKey("coop/sessions/", s) != CacheObjectKey("coop/sessions", s) {
		t.Fatal("trailing slash on prefix must be normalized")
	}

	// A hostile id cannot inject a path separator or escape the badge prefix:
	// the id must land as exactly ONE segment between "badges/" and the
	// "<cachekey>.svg" leaf, with no '/' of its own.
	evil := CacheObjectKey("coop/sessions", CacheSubject{ID: "../../etc/passwd", Title: "t"})
	const marker = "/images/badges/"
	if !strings.HasPrefix(evil, "coop/sessions"+marker) {
		t.Fatalf("hostile id escaped the badge prefix: %q", evil)
	}
	rest := evil[strings.Index(evil, marker)+len(marker):]
	if strings.Count(rest, "/") != 1 {
		t.Fatalf("expected exactly <id>/<cachekey>.svg after badges/, got %q", rest)
	}
}

// modelBadge / tier0Badge are generate-function stand-ins.
func modelGen(svg string) func(context.Context) (Badge, error) {
	return func(context.Context) (Badge, error) {
		return Badge{SVG: svg, Tier: TierModel}, nil
	}
}

// TestLoadOrGenerate_MissThenHit is the core reproducibility guarantee: the
// first call generates and writes through; the second call for the SAME
// subject (id, title, arms, style) is served from cache WITHOUT re-generating, returning the pinned
// bytes even though the generator would now produce something different.
func TestLoadOrGenerate_MissThenHit(t *testing.T) {
	store := newMemStore()
	ctx := context.Background()
	first, hit, err := LoadOrGenerate(ctx, store, "coop/sessions", subject, false, modelGen("<svg viewBox=\"0 0 100 100\">A</svg>"))
	if err != nil {
		t.Fatalf("first LoadOrGenerate: %v", err)
	}
	if hit {
		t.Fatal("first call must be a cache MISS")
	}
	if first.Tier != TierModel {
		t.Fatalf("first badge tier = %q, want %q", first.Tier, TierModel)
	}
	if store.puts != 1 {
		t.Fatalf("model badge must be written through once, got %d puts", store.puts)
	}

	// Second call: the generator would return DIFFERENT bytes, but the cache
	// must serve the pinned original.
	genCalled := false
	second, hit, err := LoadOrGenerate(ctx, store, "coop/sessions", subject, false,
		func(context.Context) (Badge, error) {
			genCalled = true
			return Badge{SVG: "<svg viewBox=\"0 0 100 100\">DIFFERENT</svg>", Tier: TierModel}, nil
		})
	if err != nil {
		t.Fatalf("second LoadOrGenerate: %v", err)
	}
	if !hit {
		t.Fatal("second call for the same subject must be a cache HIT")
	}
	if genCalled {
		t.Fatal("a cache hit must NOT invoke the generator")
	}
	if second.SVG != first.SVG {
		t.Fatalf("cache hit must return the pinned bytes: got %q, want %q", second.SVG, first.SVG)
	}
	if second.Tier != TierCached {
		t.Fatalf("cache-hit badge tier = %q, want %q", second.Tier, TierCached)
	}
	if store.puts != 1 {
		t.Fatalf("a hit must not write again, got %d puts total", store.puts)
	}
}

// TestLoadOrGenerate_RetitleInvalidates confirms a retitle misses the cache and
// regenerates (rather than serving the stale glyph for the old subject).
func TestLoadOrGenerate_RetitleInvalidates(t *testing.T) {
	store := newMemStore()
	ctx := context.Background()
	retitled := subject
	retitled.Title = "Retitled subject"

	if _, _, err := LoadOrGenerate(ctx, store, "coop/sessions", subject, false, modelGen("<svg viewBox=\"0 0 100 100\">ORIG</svg>")); err != nil {
		t.Fatalf("seed: %v", err)
	}

	got, hit, err := LoadOrGenerate(ctx, store, "coop/sessions", retitled, false, modelGen("<svg viewBox=\"0 0 100 100\">NEW</svg>"))
	if err != nil {
		t.Fatalf("retitle LoadOrGenerate: %v", err)
	}
	if hit {
		t.Fatal("a retitle must MISS the cache (different content address)")
	}
	if got.SVG != "<svg viewBox=\"0 0 100 100\">NEW</svg>" {
		t.Fatalf("retitle must regenerate, got %q", got.SVG)
	}
	if store.puts != 2 {
		t.Fatalf("retitle must write a new object, got %d puts", store.puts)
	}
}

// TestLoadOrGenerate_ArmsKeyed is gd-NyYxIxxbuw's done-when: the same arms
// hit the pinned picture, and changed arms miss and redraw, rather than serve
// the picture of the old arms. A changed style misses too: a poster is never
// served the line-art glyph.
func TestLoadOrGenerate_ArmsKeyed(t *testing.T) {
	store := newMemStore()
	ctx := context.Background()
	const pinned = "<svg viewBox=\"0 0 100 100\">MEDAL BRUSH LENS</svg>"
	if _, _, err := LoadOrGenerate(ctx, store, "coop/sessions", subject, false, modelGen(pinned)); err != nil {
		t.Fatalf("seed: %v", err)
	}

	same := subject
	same.Arms = "🏅🖌🔎" // the same arms, spelled without U+FE0F
	got, hit, err := LoadOrGenerate(ctx, store, "coop/sessions", same, false, modelGen("<svg viewBox=\"0 0 100 100\">REDRAWN</svg>"))
	if err != nil || !hit || got.SVG != pinned {
		t.Fatalf("the same arms must hit the pinned picture: hit=%v err=%v svg=%q", hit, err, got.SVG)
	}

	for what, mutate := range map[string]func(*CacheSubject){
		"new arms":   func(s *CacheSubject) { s.Arms = "🔥🧭📦" },
		"arms gone":  func(s *CacheSubject) { s.Arms = "" },
		"poster":     func(s *CacheSubject) { s.Style = StylePoster },
		"subtle art": func(s *CacheSubject) { s.Style = StyleLineArtSubtle },
	} {
		c := subject
		mutate(&c)
		redrawn := "<svg viewBox=\"0 0 100 100\">" + what + "</svg>"
		got, hit, err := LoadOrGenerate(ctx, store, "coop/sessions", c, false, modelGen(redrawn))
		if err != nil || hit || got.SVG != redrawn {
			t.Errorf("%s must miss and redraw: hit=%v err=%v svg=%q", what, hit, err, got.SVG)
		}
	}
}

// TestLoadOrGenerate_RefreshRedraws: refresh skips the cached picture, and the
// redraw replaces it, so the next plain read serves the new one.
func TestLoadOrGenerate_RefreshRedraws(t *testing.T) {
	store := newMemStore()
	ctx := context.Background()
	const oldSVG, newSVG = "<svg viewBox=\"0 0 100 100\">OLD</svg>", "<svg viewBox=\"0 0 100 100\">NEW</svg>"
	if _, _, err := LoadOrGenerate(ctx, store, "coop/sessions", subject, false, modelGen(oldSVG)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	gets := store.gets

	got, hit, err := LoadOrGenerate(ctx, store, "coop/sessions", subject, true, modelGen(newSVG))
	if err != nil || hit || got.SVG != newSVG {
		t.Fatalf("refresh must redraw: hit=%v err=%v svg=%q", hit, err, got.SVG)
	}
	if store.gets != gets {
		t.Fatalf("refresh must not read the cache (gets %d -> %d)", gets, store.gets)
	}

	got, hit, _ = LoadOrGenerate(ctx, store, "coop/sessions", subject, false, modelGen("<svg viewBox=\"0 0 100 100\">UNUSED</svg>"))
	if !hit || got.SVG != newSVG {
		t.Fatalf("after a refresh the cache must serve the redraw: hit=%v svg=%q", hit, got.SVG)
	}

	// A refresh that falls back to Tier-0 leaves the redraw pinned.
	_, _, _ = LoadOrGenerate(ctx, store, "coop/sessions", subject, true,
		func(context.Context) (Badge, error) {
			return Tier0(subject.ID, subject.Title), errors.New("model down")
		})
	if got, hit, _ := LoadOrGenerate(ctx, store, "coop/sessions", subject, false, modelGen("<svg/>")); !hit || got.SVG != newSVG {
		t.Fatalf("a Tier-0 refresh must not displace the pinned picture: hit=%v svg=%q", hit, got.SVG)
	}
}

// TestLoadOrGenerate_Tier0NotCached ensures a deterministic fallback is never
// written through — Tier-0 is free to recompute and pinning it would mask a
// later-recoverable model.
func TestLoadOrGenerate_Tier0NotCached(t *testing.T) {
	store := newMemStore()
	ctx := context.Background()

	badge, hit, err := LoadOrGenerate(ctx, store, "coop/sessions", CacheSubject{ID: "gd-X", Title: "t"}, false,
		func(context.Context) (Badge, error) {
			// Simulate the always-succeeds contract: a valid Tier-0 badge plus
			// an advisory error explaining why the model tier was skipped.
			return Tier0("gd-X", "t"), errors.New("model disabled")
		})
	if err == nil {
		t.Fatal("expected the advisory generate error to propagate")
	}
	if hit {
		t.Fatal("a fresh generate is not a hit")
	}
	if badge.Tier != TierDeterministic {
		t.Fatalf("badge tier = %q, want %q", badge.Tier, TierDeterministic)
	}
	if store.puts != 0 {
		t.Fatalf("Tier-0 badge must NOT be cached, got %d puts", store.puts)
	}
}

// TestLoadOrGenerate_StoreErrorsAreAdvisory proves a flaky store never denies a
// plan its badge: a read error falls through to generate, and a write error
// still returns the freshly generated badge.
func TestLoadOrGenerate_StoreErrorsAreAdvisory(t *testing.T) {
	ctx := context.Background()
	svg := "<svg viewBox=\"0 0 100 100\">X</svg>"

	// Read error -> fall through and generate.
	readBad := newMemStore()
	readBad.getErr = errors.New("s3 down")
	b, hit, err := LoadOrGenerate(ctx, readBad, "coop/sessions", CacheSubject{ID: "gd-X", Title: "t"}, false, modelGen(svg))
	if err != nil {
		t.Fatalf("a read error must not fail the call: %v", err)
	}
	if hit || b.SVG != svg {
		t.Fatalf("read error must fall through to generate, hit=%v svg=%q", hit, b.SVG)
	}

	// Write error -> still return the generated badge, with an advisory error.
	writeBad := newMemStore()
	writeBad.putErr = errors.New("s3 write denied")
	b, hit, err = LoadOrGenerate(ctx, writeBad, "coop/sessions", CacheSubject{ID: "gd-X", Title: "t"}, false, modelGen(svg))
	if err == nil {
		t.Fatal("a write error should surface as an advisory error")
	}
	if hit || b.SVG != svg {
		t.Fatalf("write error must still return the generated badge, got hit=%v svg=%q", hit, b.SVG)
	}
}

// TestLoadOrGenerate_NilStore works with no cache at all (store == nil): it
// simply always generates.
func TestLoadOrGenerate_NilStore(t *testing.T) {
	ctx := context.Background()
	svg := "<svg viewBox=\"0 0 100 100\">X</svg>"
	b, hit, err := LoadOrGenerate(ctx, nil, "coop/sessions", CacheSubject{ID: "gd-X", Title: "t"}, false, modelGen(svg))
	if err != nil || hit || b.SVG != svg {
		t.Fatalf("nil store must generate without error: hit=%v err=%v svg=%q", hit, err, b.SVG)
	}
}
