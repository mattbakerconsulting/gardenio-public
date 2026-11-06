package main

// /image/{key...} proxy route (ka-0000006131).
//
// Streams S3 objects from the gardenio-sessions bucket through the viewer's
// existing OIDC chain so bundle/markdown renderers can embed images by their
// proxy URL instead of relying on a public bucket ACL or a presigned redirect.
// The route is authz-parity with the bundle page (ka-0000007953) — any
// authenticated viewer (Keycloak or AVA) can fetch images, matching the
// bundle HTML itself. This file is responsible for the key-prefix
// authorisation, S3 transport, and header passthrough.
//
// Key authz: only S3 objects under `<COOP_S3_PREFIX>/images/` (default
// `coop/sessions/images/`) are proxied. The renderer-side URL rewrite uses
// the same prefix derivation, so a renderer that produces a /image/ URL is
// guaranteed to pass the handler's check — and a hand-crafted URL pointing
// at e.g. `/image/coop/sessions/transcripts/foo` is refused with 403.
//
// Why proxy and not presign/redirect: presigns require knowing the viewer's
// session lifetime up front (they expire on a fixed wall-clock); redirects
// would lose the viewer's OIDC cookie when the browser follows to S3.
// Streaming through the controller keeps the entire request inside the
// already-authenticated origin, at the cost of one in/out network hop —
// fine for the small screenshot/diagram payloads the upload path produces.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/prometheus/client_golang/prometheus"
)

// s3ImageGetter is the subset of *s3.Client surface the /image/* handler
// needs. Defining it as an interface lets tests inject an in-memory fake
// without spinning up an HTTP server (mirrors s3Uploader on the gb side).
type s3ImageGetter interface {
	GetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error)
}

// imageProxyAWSConfigTimeout caps AWS credential resolution so a broken
// IRSA / IMDS environment doesn't deadlock the viewer at startup. Mirrors
// the gb-side awsConfigTimeout — long enough for STS to issue creds on a
// healthy pod, short enough that a misconfig surfaces as a warn-level log
// rather than a stuck process.
const imageProxyAWSConfigTimeout = 8 * time.Second

// newImageProxyS3Client builds an S3 client using the controller's IRSA
// credentials (or whatever the standard AWS config chain resolves). It's
// the beads-viewer counterpart to gb's newS3Client: same env vars, same
// region passthrough, same timeout discipline. Returns an interface so
// the route can be unit-tested against an in-memory fake.
func newImageProxyS3Client(ctx context.Context) (s3ImageGetter, error) {
	loadCtx, cancel := context.WithTimeout(ctx, imageProxyAWSConfigTimeout)
	defer cancel()

	var opts []func(*awsconfig.LoadOptions) error
	if region := strings.TrimSpace(os.Getenv("COOP_S3_REGION")); region != "" {
		opts = append(opts, awsconfig.WithRegion(region))
	}
	cfg, err := awsconfig.LoadDefaultConfig(loadCtx, opts...)
	if err != nil {
		return nil, err
	}
	return s3.NewFromConfig(cfg, imageProxyS3EndpointOpts()...), nil
}

// imageProxyS3EndpointOpts mirrors gb's s3EndpointOpts: when COOP_S3_ENDPOINT
// is set, the client targets that endpoint (e.g. an in-box MinIO URL) with
// path-style addressing. Empty endpoint => no options => default AWS S3
// endpoint and virtual-hosted-style addressing, unchanged.
func imageProxyS3EndpointOpts() []func(*s3.Options) {
	endpoint := strings.TrimSpace(os.Getenv("COOP_S3_ENDPOINT"))
	if endpoint == "" {
		return nil
	}
	return []func(*s3.Options){
		func(o *s3.Options) {
			o.BaseEndpoint = aws.String(endpoint)
			o.UsePathStyle = true
		},
	}
}

// imageProxyPrefixFromEnv returns the S3 key prefix the proxy should serve
// under, derived from COOP_S3_PREFIX (default `coop/sessions`). Matches
// gb image upload's s3Prefix() so render-time URL rewrites and proxy-time
// prefix checks agree.
func imageProxyPrefixFromEnv() string {
	if p := strings.TrimSpace(os.Getenv("COOP_S3_PREFIX")); p != "" {
		return p
	}
	return "coop/sessions"
}

// imageProxyMaxStreamTimeout caps an individual proxy fetch at ~30s — large
// enough for slow S3 ranges, short enough that a stuck handler doesn't pin
// a connection forever. The render-side URLs target small images (screenshot
// / diagram sized); blobs that approach this ceiling are misuse of the path.
const imageProxyMaxStreamTimeout = 30 * time.Second

// imageProxyCacheControl is the response Cache-Control header. private (so
// shared CDNs don't pool it across viewers — these URLs are gated on the
// viewer's OIDC session) and a short max-age so a key rotation propagates
// within a few minutes without manual cache invalidation.
const imageProxyCacheControl = "private, max-age=300"

// SetImageProxy wires the S3 client + bucket + prefix used by the /image/*
// proxy. Called from main when COOP_S3_BUCKET is set; unset leaves the
// handler in degraded mode (503 Service Unavailable). Idempotent — last
// write wins, but main calls it at most once.
func (s *Server) SetImageProxy(client s3ImageGetter, bucket, prefix string) {
	s.imageS3 = client
	s.imageBucket = bucket
	s.imagePrefix = prefix
}

// SetImageFallbackBucket names the bucket a read that misses in imageBucket
// is retried in: the session bucket, once images have a bucket of their own
// (COOP_S3_IMAGES_BUCKET, gd-CmjVAU3vgd), so refs from before the switch still
// open. Empty, or equal to imageBucket, means no fallback.
func (s *Server) SetImageFallbackBucket(bucket string) {
	if bucket == s.imageBucket {
		bucket = ""
	}
	s.imageFallbackBucket = bucket
}

// imageProxyReads counts /image/ reads by where the object was found:
// "primary" (imageBucket), "fallback" (imageFallbackBucket) or "missing".
// The fallback is dropped once source="fallback" stays at zero over enough
// reads (gd-CmjVAU3vgd), never on a date.
var imageProxyReads = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "gardenio_image_proxy_reads_total",
	Help: "Viewer /image/ proxy reads by the bucket that answered: primary, fallback (the session bucket) or missing.",
}, []string{"source"})

// handleImage streams an object from `s3://<imageBucket>/<key>` to the
// viewer. The route is registered without a role gate (ka-0000007953) —
// authz parity with the bundle page, which is also reachable by any
// authenticated OIDC user. Defense in depth: the handler enforces the
// key prefix below, the bucket is private, and the OIDC chain still
// authenticates the request.
func (s *Server) handleImage(w http.ResponseWriter, r *http.Request) {
	if s.imageS3 == nil || s.imageBucket == "" {
		// No S3 client wired (typical for local dev without AWS creds);
		// be explicit so operators see the misconfiguration rather than
		// a generic 5xx.
		http.Error(w, "image proxy unavailable", http.StatusServiceUnavailable)
		return
	}
	key := r.PathValue("key")
	if key == "" {
		http.Error(w, "missing key", http.StatusBadRequest)
		return
	}
	// PathValue gives us the un-escaped key — Go's net/http ServeMux
	// percent-decodes path components before invoking the handler.
	if !strings.HasPrefix(key, imageProxyAllowedPrefix(s.imagePrefix)) {
		// Out-of-prefix keys are refused outright — the proxy is scoped
		// to the images subtree of the sessions bucket. Use 403 not 404
		// so an operator debugging a misrendered URL can tell "wrong
		// shape" from "really missing".
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), imageProxyMaxStreamTimeout)
	defer cancel()

	in := &s3.GetObjectInput{
		Bucket: aws.String(s.imageBucket),
		Key:    aws.String(key),
	}
	// If-None-Match → conditional GET. S3 returns 304 with no body when
	// the ETag matches; we mirror that to the browser so the htmx swap /
	// page reload doesn't re-download unchanged screenshots.
	if etag := r.Header.Get("If-None-Match"); etag != "" {
		in.IfNoneMatch = aws.String(etag)
	}
	source := "primary"
	out, err := s.imageS3.GetObject(ctx, in)
	if err != nil && s.imageFallbackBucket != "" && isImageNotFound(err) {
		source = "fallback"
		in.Bucket = aws.String(s.imageFallbackBucket)
		out, err = s.imageS3.GetObject(ctx, in)
	}
	if err != nil {
		if isImageNotFound(err) {
			imageProxyReads.WithLabelValues("missing").Inc()
		}
		s.writeImageProxyError(w, r, key, err)
		return
	}
	imageProxyReads.WithLabelValues(source).Inc()
	defer out.Body.Close()

	if out.ContentType != nil {
		w.Header().Set("Content-Type", *out.ContentType)
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	if out.ETag != nil {
		w.Header().Set("ETag", *out.ETag)
	}
	w.Header().Set("Cache-Control", imageProxyCacheControl)
	if out.ContentLength != nil && *out.ContentLength > 0 {
		w.Header().Set("Accept-Ranges", "bytes")
	}
	if _, err := io.Copy(w, out.Body); err != nil {
		// Body started streaming — headers are already on the wire and
		// http.Error would just emit a body fragment after the legitimate
		// bytes. Log only.
		s.logger.Warn("image proxy stream interrupted", "key", key, "error", err)
	}
}

// isImageNotFound reports whether err is S3's "no such object" (NoSuchKey or
// a 404 NotFound), the only error a fallback read is tried for.
func isImageNotFound(err error) bool {
	var noKey *s3types.NoSuchKey
	var notFound *s3types.NotFound
	return errors.As(err, &noKey) || errors.As(err, &notFound)
}

// writeImageProxyError maps an S3 error to a viewer-safe HTTP response —
// no AWS error body leaks to the browser. NotModified is handled as the
// 304 it semantically is; NoSuchKey / NotFound is a 404; access-denied
// is a 403 (defense in depth: IRSA has the perm in practice, so this is
// a "shouldn't happen" branch); everything else is a generic 502.
func (s *Server) writeImageProxyError(w http.ResponseWriter, _ *http.Request, key string, err error) {
	// S3 GetObject returns NotModified as an APIError code rather than a
	// typed exception — match on the code.
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NotModified":
			w.Header().Set("Cache-Control", imageProxyCacheControl)
			w.WriteHeader(http.StatusNotModified)
			return
		case "AccessDenied":
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
	}
	var noKey *s3types.NoSuchKey
	if errors.As(err, &noKey) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	var notFound *s3types.NotFound
	if errors.As(err, &notFound) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	s.logger.Error("image proxy upstream error", "key", key, "error", err)
	http.Error(w, "image upstream error", http.StatusBadGateway)
}
