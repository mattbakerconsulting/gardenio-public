package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// fakeS3Downloader serves a fixed body and records the key it was asked for.
type fakeS3Downloader struct {
	body    []byte
	err     error
	missIn  map[string]bool // buckets that answer NoSuchKey
	gotKeys []string
	gotBkts []string
}

func (f *fakeS3Downloader) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	f.gotKeys = append(f.gotKeys, *in.Key)
	f.gotBkts = append(f.gotBkts, *in.Bucket)
	if f.missIn[*in.Bucket] {
		return nil, &types.NoSuchKey{}
	}
	if f.err != nil {
		return nil, f.err
	}
	return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(f.body))}, nil
}

// --- Reference normalization. ------------------------------------------------

// TestImageKeyFromRef_AcceptsEveryShape: callers have whichever form they were
// handed — the viewer URL from a bead comment, the raw S3 line from stderr, or
// a bare key. Refusing any of them just sends them back to URL surgery, which
// is the manual step this command exists to remove.
func TestImageKeyFromRef_AcceptsEveryShape(t *testing.T) {
	const wantKey = "coop/sessions/images/2026-07-27/abc123-shot.png"
	for name, ref := range map[string]string{
		"viewer proxied URL":  "https://prod.gardenio.acme.example/beads/image/" + wantKey,
		"viewer no basePath":  "https://viewer.example/image/" + wantKey,
		"raw S3 virtual-host": "https://gardenio-sessions-prod.s3.amazonaws.com/" + wantKey,
		"raw S3 path-style":   "https://s3.amazonaws.com/gardenio-sessions-prod/" + wantKey,
		"bare key":            wantKey,
		"bare key leading /":  "/" + wantKey,
		"surrounding spaces":  "  " + wantKey + "  ",
	} {
		t.Run(name, func(t *testing.T) {
			got, err := imageKeyFromRef(ref, "gardenio-sessions-prod")
			if err != nil {
				t.Fatalf("imageKeyFromRef(%q): %v", ref, err)
			}
			if got != wantKey {
				t.Errorf("got key %q, want %q", got, wantKey)
			}
		})
	}
}

// TestImageKeyFromRef_DecodesPercentEncoding: gb image upload percent-encodes
// each path segment, so a filename with a space round-trips through the URL as
// %20 and must be decoded back to the literal key S3 stores.
func TestImageKeyFromRef_DecodesPercentEncoding(t *testing.T) {
	got, err := imageKeyFromRef(
		"https://prod.gardenio.acme.example/beads/image/coop/sessions/images/2026-07-27/abc-my%20file.png",
		"gardenio-sessions-prod")
	if err != nil {
		t.Fatal(err)
	}
	if want := "coop/sessions/images/2026-07-27/abc-my file.png"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestImageKeyFromRef_SplitsOnLastImageSegment: a key that itself contains
// "/image/" must not truncate the real key. Splitting on the FIRST occurrence
// would silently fetch the wrong object.
func TestImageKeyFromRef_SplitsOnLastImageSegment(t *testing.T) {
	const wantKey = "coop/sessions/images/2026-07-27/abc-shot.png"
	got, err := imageKeyFromRef("https://host/image/base/image/"+wantKey, "b")
	if err != nil {
		t.Fatal(err)
	}
	if got != wantKey {
		t.Errorf("got %q, want %q — must split on the LAST /image/", got, wantKey)
	}
}

// TestImageKeyFromRef_RefusesOutOfPrefix is the containment guard. The sessions
// bucket holds transcripts and other non-image material; this command must not
// become a general-purpose reader for it. Mirrors the viewer proxy's own check.
func TestImageKeyFromRef_RefusesOutOfPrefix(t *testing.T) {
	for name, ref := range map[string]string{
		"transcripts":    "coop/sessions/transcripts/secret.jsonl",
		"bucket root":    "some-other-thing.png",
		"traversal-ish":  "coop/sessions/images/../transcripts/secret.jsonl",
		"different tree": "other/prefix/images/x.png",
		"prefix tease":   "coop/sessions/imagesX/x.png",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := imageKeyFromRef(ref, "b")
			if err == nil {
				t.Fatalf("want a refusal for %q", ref)
			}
			if !errors.Is(err, errImageRefOutOfPrefix) {
				t.Errorf("want errImageRefOutOfPrefix, got %v", err)
			}
		})
	}
}

func TestImageKeyFromRef_RefusesEmpty(t *testing.T) {
	for _, ref := range []string{"", "   "} {
		if _, err := imageKeyFromRef(ref, "b"); err == nil {
			t.Errorf("want an error for %q", ref)
		}
	}
}

func TestImageFetchAllowedPrefix_HonorsEnv(t *testing.T) {
	t.Setenv("COOP_S3_PREFIX", "")
	if got := imageFetchAllowedPrefix(); got != "coop/sessions/images/" {
		t.Errorf("default prefix = %q, want coop/sessions/images/", got)
	}
	t.Setenv("COOP_S3_PREFIX", "custom/place/")
	if got := imageFetchAllowedPrefix(); got != "custom/place/images/" {
		t.Errorf("env prefix = %q, want custom/place/images/", got)
	}
}

// TestImageFetchAllowedPrefix_MatchesUploadLayout pins the coupling that makes
// fetch able to read what upload writes: both derive from COOP_S3_PREFIX and
// both put images under an `images/` segment. If upload's layout changes and
// this does not, every fetch starts refusing valid keys.
func TestImageFetchAllowedPrefix_MatchesUploadLayout(t *testing.T) {
	t.Setenv("COOP_S3_PREFIX", "coop/sessions")
	key := imageKey(s3Prefix(), "2026-07-27", "shot.png", []byte("bytes"))
	if !strings.HasPrefix(key, imageFetchAllowedPrefix()) {
		t.Errorf("upload writes %q, which fetch would refuse (allowed prefix %q)",
			key, imageFetchAllowedPrefix())
	}
}

// --- End-to-end through the command. -----------------------------------------

func withFakeDownloader(t *testing.T, f *fakeS3Downloader) {
	t.Helper()
	prev := newS3Downloader
	newS3Downloader = func(context.Context) (s3Downloader, error) { return f, nil }
	t.Cleanup(func() { newS3Downloader = prev })
}

func TestImageFetch_WritesFileNamedAfterKey(t *testing.T) {
	t.Setenv("COOP_S3_BUCKET", "test-bucket")
	t.Setenv("COOP_S3_PREFIX", "coop/sessions")
	f := &fakeS3Downloader{body: []byte("PNGBYTES")}
	withFakeDownloader(t, f)

	dir := t.TempDir()
	cmd := imageFetchCmd
	cmd.SetContext(context.Background())
	if err := cmd.Flags().Set("output-dir", dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Flags().Set("output-dir", ".") })

	if err := runImageFetch(cmd, []string{
		"https://prod.gardenio.acme.example/beads/image/coop/sessions/images/2026-07-27/abc-shot.png",
	}); err != nil {
		t.Fatalf("runImageFetch: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "abc-shot.png"))
	if err != nil {
		t.Fatalf("expected the file to be named after the key basename: %v", err)
	}
	if string(got) != "PNGBYTES" {
		t.Errorf("content = %q, want PNGBYTES", got)
	}
	if len(f.gotKeys) != 1 || f.gotKeys[0] != "coop/sessions/images/2026-07-27/abc-shot.png" {
		t.Errorf("fetched keys = %v, want the decoded key", f.gotKeys)
	}
	if f.gotBkts[0] != "test-bucket" {
		t.Errorf("bucket = %q, want test-bucket", f.gotBkts[0])
	}
}

func TestImageFetch_RequiresBucket(t *testing.T) {
	t.Setenv("COOP_S3_BUCKET", "")
	cmd := imageFetchCmd
	cmd.SetContext(context.Background())
	err := runImageFetch(cmd, []string{"coop/sessions/images/a/b.png"})
	if err == nil || !strings.Contains(err.Error(), "COOP_S3_BUCKET") {
		t.Errorf("want a clear COOP_S3_BUCKET error, got %v", err)
	}
}

func TestImageFetch_StdoutRejectsMultipleArgs(t *testing.T) {
	t.Setenv("COOP_S3_BUCKET", "b")
	cmd := imageFetchCmd
	cmd.SetContext(context.Background())
	if err := cmd.Flags().Set("stdout", "true"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Flags().Set("stdout", "false") })

	err := runImageFetch(cmd, []string{
		"coop/sessions/images/a/1.png",
		"coop/sessions/images/a/2.png",
	})
	if err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Errorf("streaming two images to one stdout is ambiguous and must be refused, got %v", err)
	}
}

// TestImageFetch_RefusesOutOfPrefixBeforeHittingS3: the containment check must
// run locally, so a bad ref costs no S3 call and cannot probe the bucket.
func TestImageFetch_RefusesOutOfPrefixBeforeHittingS3(t *testing.T) {
	t.Setenv("COOP_S3_BUCKET", "b")
	t.Setenv("COOP_S3_PREFIX", "coop/sessions")
	f := &fakeS3Downloader{body: []byte("x")}
	withFakeDownloader(t, f)

	cmd := imageFetchCmd
	cmd.SetContext(context.Background())
	if err := runImageFetch(cmd, []string{"coop/sessions/transcripts/secret.jsonl"}); err == nil {
		t.Fatal("want a refusal for an out-of-prefix key")
	}
	if len(f.gotKeys) != 0 {
		t.Errorf("must not call S3 for a refused key, called with %v", f.gotKeys)
	}
}

func TestImageFetch_SurfacesS3Error(t *testing.T) {
	t.Setenv("COOP_S3_BUCKET", "b")
	t.Setenv("COOP_S3_PREFIX", "coop/sessions")
	withFakeDownloader(t, &fakeS3Downloader{err: errors.New("NoSuchKey")})

	cmd := imageFetchCmd
	cmd.SetContext(context.Background())
	err := runImageFetch(cmd, []string{"coop/sessions/images/a/missing.png"})
	if err == nil {
		t.Fatal("want an error when S3 fails")
	}
	// The message must name the object, or a batch fetch failure is unactionable.
	if !strings.Contains(err.Error(), "missing.png") {
		t.Errorf("error %q should name the failing object", err)
	}
}

func TestWriteImageFile_ReportsByteCount(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "x.png")
	n, err := writeImageFile(dest, bytes.NewReader([]byte("12345")))
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Errorf("n = %d, want 5", n)
	}
}

func TestWriteImageFile_ErrorsOnUnwritableDest(t *testing.T) {
	if _, err := writeImageFile(filepath.Join(t.TempDir(), "no", "such", "dir", "x.png"),
		bytes.NewReader([]byte("x"))); err == nil {
		t.Error("want an error for an unwritable destination, not a silent drop")
	}
}

// gd-CmjVAU3vgd: with an images bucket set, fetch reads it first and falls
// back to the session bucket on a miss, so refs from before the switch still
// resolve. Any other error is not retried in the other bucket.
func TestImageFetch_FallsBackToSessionBucket(t *testing.T) {
	t.Setenv("COOP_S3_BUCKET", "gardenio-sessions")
	t.Setenv("COOP_S3_IMAGES_BUCKET", "gardenio-images")
	t.Setenv("COOP_S3_PREFIX", "coop/sessions")
	f := &fakeS3Downloader{body: []byte("OLD"), missIn: map[string]bool{"gardenio-images": true}}
	withFakeDownloader(t, f)

	dir := t.TempDir()
	cmd := imageFetchCmd
	cmd.SetContext(context.Background())
	if err := cmd.Flags().Set("output-dir", dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Flags().Set("output-dir", ".") })

	if err := runImageFetch(cmd, []string{"coop/sessions/images/2026-11-01/abc-old.png"}); err != nil {
		t.Fatalf("runImageFetch: %v", err)
	}
	if got := strings.Join(f.gotBkts, ","); got != "gardenio-images,gardenio-sessions" {
		t.Errorf("buckets read = %s, want gardenio-images then gardenio-sessions", got)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "abc-old.png")); string(got) != "OLD" {
		t.Errorf("content = %q, want the session bucket's bytes", got)
	}
}

func TestImageFetch_ImagesBucketHitReadsOnce(t *testing.T) {
	t.Setenv("COOP_S3_BUCKET", "gardenio-sessions")
	t.Setenv("COOP_S3_IMAGES_BUCKET", "gardenio-images")
	t.Setenv("COOP_S3_PREFIX", "coop/sessions")
	f := &fakeS3Downloader{body: []byte("NEW")}
	withFakeDownloader(t, f)

	out, err := getImageObject(context.Background(), f, "coop/sessions/images/a/new.png", imageReadBuckets())
	if err != nil {
		t.Fatal(err)
	}
	_ = out.Body.Close()
	if got := strings.Join(f.gotBkts, ","); got != "gardenio-images" {
		t.Errorf("buckets read = %s, want gardenio-images only", got)
	}
}

func TestImageFetch_NoFallbackOnOtherErrors(t *testing.T) {
	t.Setenv("COOP_S3_BUCKET", "gardenio-sessions")
	t.Setenv("COOP_S3_IMAGES_BUCKET", "gardenio-images")
	f := &fakeS3Downloader{err: errors.New("AccessDenied")}
	if _, err := getImageObject(context.Background(), f, "coop/sessions/images/a/x.png", imageReadBuckets()); err == nil {
		t.Fatal("want the error")
	}
	if len(f.gotBkts) != 1 {
		t.Errorf("a non-not-found error must not fall back, read %v", f.gotBkts)
	}
}

func TestImageReadBuckets(t *testing.T) {
	t.Setenv("COOP_S3_BUCKET", "s")
	t.Setenv("COOP_S3_IMAGES_BUCKET", "")
	if got := imageReadBuckets(); len(got) != 1 || got[0] != "s" {
		t.Errorf("unset images bucket: %v, want [s]", got)
	}
	t.Setenv("COOP_S3_IMAGES_BUCKET", "s")
	if got := imageReadBuckets(); len(got) != 1 {
		t.Errorf("same bucket twice must read once: %v", got)
	}
	t.Setenv("COOP_S3_BUCKET", "")
	t.Setenv("COOP_S3_IMAGES_BUCKET", "")
	if got := imageReadBuckets(); len(got) != 0 {
		t.Errorf("nothing set: %v, want none", got)
	}
}

func TestImageKeyFromRef_PathStyleImagesBucket(t *testing.T) {
	const wantKey = "coop/sessions/images/2026-11-05/abc-shot.png"
	for _, bucket := range []string{"gardenio-images", "gardenio-sessions"} {
		got, err := imageKeyFromRef("http://gardenio-minio:9000/"+bucket+"/"+wantKey, "gardenio-images", "gardenio-sessions")
		if err != nil || got != wantKey {
			t.Errorf("%s: got %q, %v; want %q", bucket, got, err, wantKey)
		}
	}
}
