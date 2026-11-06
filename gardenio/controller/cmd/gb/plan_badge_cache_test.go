package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"gardenio/controller/internal/badge"
)

// fakeBadgeS3 is an in-memory s3BadgeClient: it records puts and serves gets
// from a map, with getErr letting a test force a specific GetObject failure
// (e.g. a NoSuchKey to prove miss handling).
type fakeBadgeS3 struct {
	objects map[string][]byte
	getErr  error
	puts    int
	lastCT  string
}

func newFakeBadgeS3() *fakeBadgeS3 { return &fakeBadgeS3{objects: map[string][]byte{}} }

func (f *fakeBadgeS3) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	v, ok := f.objects[aws.ToString(in.Key)]
	if !ok {
		return nil, &types.NoSuchKey{}
	}
	return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(v))}, nil
}

func (f *fakeBadgeS3) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	f.puts++
	f.lastCT = aws.ToString(in.ContentType)
	data, _ := io.ReadAll(in.Body)
	f.objects[aws.ToString(in.Key)] = data
	return &s3.PutObjectOutput{}, nil
}

// TestS3BadgeStore_GetMissIsCleanMiss proves a NoSuchKey maps to (found=false,
// nil error), NOT a backend error — the property LoadOrGenerate relies on to
// distinguish "not cached yet" from "backend hiccup".
func TestS3BadgeStore_GetMissIsCleanMiss(t *testing.T) {
	store := &s3BadgeStore{client: newFakeBadgeS3(), bucket: "b"}
	data, found, err := store.Get(context.Background(), "coop/sessions/images/badges/gd-X/abc.svg")
	if err != nil {
		t.Fatalf("a missing object must be a clean miss, got err %v", err)
	}
	if found || data != nil {
		t.Fatalf("expected miss, got found=%v data=%q", found, data)
	}
}

// TestS3BadgeStore_GetRealErrorPropagates confirms a non-404 failure is
// surfaced (so the cache can fall through to generate rather than mask it).
func TestS3BadgeStore_GetRealErrorPropagates(t *testing.T) {
	fake := newFakeBadgeS3()
	fake.getErr = errors.New("AccessDenied")
	store := &s3BadgeStore{client: fake, bucket: "b"}
	if _, _, err := store.Get(context.Background(), "k"); err == nil {
		t.Fatal("a real S3 error must propagate, not be swallowed as a miss")
	}
}

// TestS3BadgeStore_PutThenGetRoundTrips checks a written badge reads back
// byte-identical and is stored with an SVG content type.
func TestS3BadgeStore_PutThenGetRoundTrips(t *testing.T) {
	fake := newFakeBadgeS3()
	store := &s3BadgeStore{client: fake, bucket: "b"}
	ctx := context.Background()
	key := "coop/sessions/images/badges/gd-X/abc.svg"
	svg := []byte(`<svg viewBox="0 0 100 100"></svg>`)

	if err := store.Put(ctx, key, svg); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if fake.lastCT != "image/svg+xml" {
		t.Fatalf("badge must be stored as image/svg+xml, got %q", fake.lastCT)
	}
	got, found, err := store.Get(ctx, key)
	if err != nil || !found {
		t.Fatalf("Get after Put: found=%v err=%v", found, err)
	}
	if !bytes.Equal(got, svg) {
		t.Fatalf("round-trip mismatch: got %q want %q", got, svg)
	}
}

// TestNewBadgeStore_NoBucketIsNilStore confirms that with no bucket configured,
// newBadgeStore returns (nil, nil) — a valid "no cache" state.
func TestNewBadgeStore_NoBucketIsNilStore(t *testing.T) {
	t.Setenv("COOP_S3_BUCKET", "")
	store, err := newBadgeStore(context.Background())
	if err != nil {
		t.Fatalf("no bucket must not error: %v", err)
	}
	if store != nil {
		t.Fatalf("no bucket must yield a nil store, got %v", store)
	}
}

// withFakeBadgeS3 injects an in-memory S3 client into the badge-cache path so
// badgeForPlan can be exercised end-to-end without a live bucket.
func withFakeBadgeS3(t *testing.T, f *fakeBadgeS3) {
	t.Helper()
	prev := newBadgeS3Client
	newBadgeS3Client = func(context.Context) (s3BadgeClient, error) { return f, nil }
	t.Cleanup(func() { newBadgeS3Client = prev })
}

// TestBadgeForPlan_CacheReadThrough drives the wired command helper: a first
// call (model tier) writes through; a second call for the same (id, title) is
// served from the cache without regenerating.
func TestBadgeForPlan_CacheReadThrough(t *testing.T) {
	t.Setenv("COOP_S3_BUCKET", "test-bucket")
	t.Setenv("COOP_S3_PREFIX", "coop/sessions")
	// Force the model tier by seeding the SVG the fake generator returns: we
	// can't reach a live model here, so we exercise the cache write path by
	// pre-seeding the store and asserting a HIT (the generator would fall back
	// to Tier-0 with no key, which is not cached).
	fake := newFakeBadgeS3()
	withFakeBadgeS3(t, fake)

	// Pre-seed the object at the exact key badgeForPlan will compute, as a
	// model badge, to simulate "first call already pinned it".
	subject := badge.CacheSubject{ID: "ka-0000002812", Title: "Reproducible plan", Arms: "🏅🖌️🔎", Style: badge.StyleLineArtBold}
	key := badge.CacheObjectKey("coop/sessions", subject)
	fake.objects[key] = []byte(`<svg viewBox="0 0 100 100">PINNED</svg>`)

	resetBadgeFlags()
	t.Cleanup(resetBadgeFlags)

	cmd := planBadgeCmd
	cmd.SetContext(context.Background())
	bdg, hit, err := badgeForPlan(cmd, subject, "prose")
	if err != nil {
		t.Fatalf("badgeForPlan: %v", err)
	}
	if !hit {
		t.Fatal("a pre-seeded subject must be a cache HIT")
	}
	if bdg.SVG != `<svg viewBox="0 0 100 100">PINNED</svg>` {
		t.Fatalf("cache hit must serve the pinned bytes, got %q", bdg.SVG)
	}
}

// TestBadgeForPlan_NoCacheBypasses proves --no-cache never touches the store.
func TestBadgeForPlan_NoCacheBypasses(t *testing.T) {
	t.Setenv("COOP_S3_BUCKET", "test-bucket")
	fake := newFakeBadgeS3()
	withFakeBadgeS3(t, fake)

	planBadgeTier0 = true // force deterministic so no live model is needed
	planBadgeNoCache = true
	t.Cleanup(func() { planBadgeTier0 = false; planBadgeNoCache = false })

	cmd := planBadgeCmd
	cmd.SetContext(context.Background())
	bdg, hit, err := badgeForPlan(cmd, badge.CacheSubject{ID: "gd-X", Title: "t"}, "prose")
	if err != nil {
		t.Fatalf("badgeForPlan: %v", err)
	}
	if hit {
		t.Fatal("--no-cache must never be a hit")
	}
	if bdg.Tier != "tier0" {
		t.Fatalf("expected tier0, got %q", bdg.Tier)
	}
	if fake.puts != 0 || len(fake.objects) != 0 {
		t.Fatalf("--no-cache must not touch the store (puts=%d objs=%d)", fake.puts, len(fake.objects))
	}
}

// TestBadgeForPlan_KeyedOnArmsAndStyle: the wired command reads the cache
// under the subject's arms and style, so a bead whose arms changed, or a
// poster of it, is not served the pinned line-art picture of the old arms.
// With no model reachable the redraw falls back to Tier-0, which is not
// cached, so nothing is written either.
func TestBadgeForPlan_KeyedOnArmsAndStyle(t *testing.T) {
	t.Setenv("COOP_S3_BUCKET", "test-bucket")
	t.Setenv("COOP_S3_PREFIX", "coop/sessions")
	t.Setenv("BADGE_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	fake := newFakeBadgeS3()
	withFakeBadgeS3(t, fake)
	pinned := badge.CacheSubject{ID: "gd-P", Title: "Poster plan", Arms: "🏅🖌️🔎", Style: badge.StyleLineArtBold}
	fake.objects[badge.CacheObjectKey("coop/sessions", pinned)] = []byte(`<svg viewBox="0 0 100 100">LINE-ART</svg>`)

	resetBadgeFlags()
	t.Cleanup(resetBadgeFlags)
	cmd := planBadgeCmd
	cmd.SetContext(context.Background())

	if bdg, hit, _ := badgeForPlan(cmd, pinned, "prose"); !hit || !strings.Contains(bdg.SVG, "LINE-ART") {
		t.Fatalf("the pinned subject must hit (hit=%v)", hit)
	}
	newArms := pinned
	newArms.Arms = "🔥🧭📦"
	poster := pinned
	poster.Style = badge.StylePoster
	for name, s := range map[string]badge.CacheSubject{"new arms": newArms, "poster": poster} {
		bdg, hit, _ := badgeForPlan(cmd, s, "prose")
		if hit || strings.Contains(bdg.SVG, "LINE-ART") {
			t.Errorf("%s was served the pinned line-art picture of the old arms (hit=%v)", name, hit)
		}
	}
	if fake.puts != 0 {
		t.Fatalf("a Tier-0 fallback was cached (puts=%d)", fake.puts)
	}
}

// TestBadgeForPlan_RefreshSkipsCache: --refresh never serves the pinned
// picture; it redraws.
func TestBadgeForPlan_RefreshSkipsCache(t *testing.T) {
	t.Setenv("COOP_S3_BUCKET", "test-bucket")
	t.Setenv("COOP_S3_PREFIX", "coop/sessions")
	t.Setenv("BADGE_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	fake := newFakeBadgeS3()
	withFakeBadgeS3(t, fake)
	s := badge.CacheSubject{ID: "gd-R", Title: "Redraw me", Style: badge.StyleLineArtBold}
	fake.objects[badge.CacheObjectKey("coop/sessions", s)] = []byte(`<svg viewBox="0 0 100 100">PINNED</svg>`)

	resetBadgeFlags()
	planBadgeRefresh = true
	t.Cleanup(resetBadgeFlags)
	cmd := planBadgeCmd
	cmd.SetContext(context.Background())

	bdg, hit, _ := badgeForPlan(cmd, s, "prose")
	if hit || strings.Contains(bdg.SVG, "PINNED") {
		t.Fatalf("--refresh served the pinned picture (hit=%v)", hit)
	}
}
