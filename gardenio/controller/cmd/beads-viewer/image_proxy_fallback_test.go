package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// gd-CmjVAU3vgd: with images in a bucket of their own, the /image/ proxy reads
// that bucket first and the session bucket on a miss, so refs from before the
// switch still open, and counts which one answered.

// bucketFakeS3 serves objects per bucket and records the buckets read.
type bucketFakeS3 struct {
	objects map[string]map[string][]byte // bucket → key → body
	reads   []string
}

func (f *bucketFakeS3) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	f.reads = append(f.reads, *in.Bucket)
	body, ok := f.objects[*in.Bucket][*in.Key]
	if !ok {
		return nil, &s3types.NoSuchKey{}
	}
	return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(body))}, nil
}

func serveImage(t *testing.T, f *bucketFakeS3, fallback, key string) *httptest.ResponseRecorder {
	t.Helper()
	srv := &Server{logger: slog.Default()}
	srv.SetImageProxy(f, "gardenio-images", "coop/sessions")
	srv.SetImageFallbackBucket(fallback)
	rr := httptest.NewRecorder()
	newImageProxyMux(srv).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/image/"+key, nil))
	return rr
}

func TestHandleImage_FallsBackToSessionBucket(t *testing.T) {
	key := "coop/sessions/images/2026-11-01/abc-old.png"
	f := &bucketFakeS3{objects: map[string]map[string][]byte{"gardenio-sessions": {key: []byte("OLD")}}}
	before := testutil.ToFloat64(imageProxyReads.WithLabelValues("fallback"))

	rr := serveImage(t, f, "gardenio-sessions", key)
	if rr.Code != http.StatusOK || rr.Body.String() != "OLD" {
		t.Fatalf("got %d %q, want 200 OLD from the session bucket", rr.Code, rr.Body.String())
	}
	if len(f.reads) != 2 || f.reads[0] != "gardenio-images" || f.reads[1] != "gardenio-sessions" {
		t.Errorf("reads = %v, want images then sessions", f.reads)
	}
	if got := testutil.ToFloat64(imageProxyReads.WithLabelValues("fallback")) - before; got != 1 {
		t.Errorf("fallback reads counted %v, want 1", got)
	}
}

func TestHandleImage_ImagesBucketHitNeverFallsBack(t *testing.T) {
	key := "coop/sessions/images/2026-11-05/abc-new.png"
	f := &bucketFakeS3{objects: map[string]map[string][]byte{"gardenio-images": {key: []byte("NEW")}}}
	rr := serveImage(t, f, "gardenio-sessions", key)
	if rr.Code != http.StatusOK || rr.Body.String() != "NEW" {
		t.Fatalf("got %d %q, want 200 NEW", rr.Code, rr.Body.String())
	}
	if len(f.reads) != 1 {
		t.Errorf("reads = %v, want the images bucket only", f.reads)
	}
}

func TestHandleImage_MissInBothIs404(t *testing.T) {
	f := &bucketFakeS3{}
	before := testutil.ToFloat64(imageProxyReads.WithLabelValues("missing"))
	rr := serveImage(t, f, "gardenio-sessions", "coop/sessions/images/x/gone.png")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", rr.Code)
	}
	if got := testutil.ToFloat64(imageProxyReads.WithLabelValues("missing")) - before; got != 1 {
		t.Errorf("missing reads counted %v, want 1", got)
	}
}

func TestHandleImage_NoFallbackWhenUnset(t *testing.T) {
	f := &bucketFakeS3{}
	for _, fallback := range []string{"", "gardenio-images"} {
		f.reads = nil
		if rr := serveImage(t, f, fallback, "coop/sessions/images/x/gone.png"); rr.Code != http.StatusNotFound {
			t.Fatalf("fallback %q: got %d, want 404", fallback, rr.Code)
		}
		if len(f.reads) != 1 {
			t.Errorf("fallback %q: reads = %v, want one", fallback, f.reads)
		}
	}
}

func TestRewriteSessionImageURL_FallbackBucket(t *testing.T) {
	ctx := &bundleRenderCtx{
		imageBucket:         "gardenio-images",
		imageFallbackBucket: "gardenio-sessions",
		imagePrefix:         "coop/sessions",
	}
	for _, bucket := range []string{"gardenio-images", "gardenio-sessions"} {
		in := "https://" + bucket + ".s3.amazonaws.com/coop/sessions/images/gd-A/shot.png"
		if got, want := rewriteSessionImageURL(ctx, in), "/image/coop/sessions/images/gd-A/shot.png"; got != want {
			t.Errorf("%s: got %q want %q", bucket, got, want)
		}
	}
	if got := rewriteSessionImageURL(ctx, "https://other.s3.amazonaws.com/coop/sessions/images/gd-A/shot.png"); got != "" {
		t.Errorf("a third bucket must not be rewritten, got %q", got)
	}
}
