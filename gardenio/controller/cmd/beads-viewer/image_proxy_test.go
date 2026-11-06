package main

// /image/{key...} proxy tests (ka-0000006131, ka-0000007953).
// Covers both the renderer-side URL rewrite (virtual-hosted, path-style,
// non-S3 passthrough, out-of-prefix) and the handler-side proxy (200, 404,
// 403 out-of-prefix, 304, 503 unwired, bundle-parity no-role-gate).

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/oidcauth"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// fakeS3 implements s3ImageGetter against an in-memory key→bytes map.
// Set wantErr to inject a specific S3 error; gotIfNoneMatch records the
// last If-None-Match header value passed through.
type fakeS3 struct {
	objects          map[string][]byte
	wantErr          error
	gotIfNoneMatch   string
	getCalls         int
	contentTypes     map[string]string
	etags            map[string]string
	notModifiedKeys  map[string]struct{}
	lastRequestedKey string
}

func newFakeS3() *fakeS3 {
	return &fakeS3{
		objects:         map[string][]byte{},
		contentTypes:    map[string]string{},
		etags:           map[string]string{},
		notModifiedKeys: map[string]struct{}{},
	}
}

func (f *fakeS3) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	f.getCalls++
	if in.Key != nil {
		f.lastRequestedKey = *in.Key
	}
	if in.IfNoneMatch != nil {
		f.gotIfNoneMatch = *in.IfNoneMatch
	}
	if f.wantErr != nil {
		return nil, f.wantErr
	}
	if in.Key == nil {
		return nil, &smithyAPIError{code: "InvalidRequest"}
	}
	key := *in.Key
	if _, nm := f.notModifiedKeys[key]; nm {
		return nil, &smithyAPIError{code: "NotModified"}
	}
	body, ok := f.objects[key]
	if !ok {
		return nil, &s3types.NoSuchKey{}
	}
	out := &s3.GetObjectOutput{
		Body: io.NopCloser(bytes.NewReader(body)),
	}
	if ct, ok := f.contentTypes[key]; ok {
		out.ContentType = aws.String(ct)
	}
	if et, ok := f.etags[key]; ok {
		out.ETag = aws.String(et)
	}
	length := int64(len(body))
	out.ContentLength = &length
	return out, nil
}

// smithyAPIError lets tests inject an APIError with a chosen code without
// importing the internal AWS SDK error machinery.
type smithyAPIError struct {
	code string
	msg  string
}

var _ smithy.APIError = (*smithyAPIError)(nil)

func (e *smithyAPIError) Error() string                 { return e.code + ": " + e.msg }
func (e *smithyAPIError) ErrorCode() string             { return e.code }
func (e *smithyAPIError) ErrorMessage() string          { return e.msg }
func (e *smithyAPIError) ErrorFault() smithy.ErrorFault { return smithy.FaultClient }

// ── URL rewrite tests ─────────────────────────────────────────────────────

func TestRewriteSessionImageURL_VirtualHostedRegionless(t *testing.T) {
	ctx := &bundleRenderCtx{
		basePath:    "",
		imageBucket: "gardenio-sessions",
		imagePrefix: "coop/sessions",
	}
	in := "https://gardenio-sessions.s3.amazonaws.com/coop/sessions/images/gd-ABC/abcd1234-shot.png"
	got := rewriteSessionImageURL(ctx, in)
	want := "/image/coop/sessions/images/gd-ABC/abcd1234-shot.png"
	if got != want {
		t.Errorf("virtual-hosted regionless: got %q want %q", got, want)
	}
}

func TestRewriteSessionImageURL_VirtualHostedRegional(t *testing.T) {
	ctx := &bundleRenderCtx{
		basePath:    "",
		imageBucket: "gardenio-sessions",
		imagePrefix: "coop/sessions",
	}
	in := "https://gardenio-sessions.s3.us-west-2.amazonaws.com/coop/sessions/images/gd-X/diagram.png"
	got := rewriteSessionImageURL(ctx, in)
	want := "/image/coop/sessions/images/gd-X/diagram.png"
	if got != want {
		t.Errorf("virtual-hosted regional: got %q want %q", got, want)
	}
}

func TestRewriteSessionImageURL_PathStyle(t *testing.T) {
	ctx := &bundleRenderCtx{
		basePath:    "",
		imageBucket: "gardenio-sessions",
		imagePrefix: "coop/sessions",
	}
	in := "https://s3.amazonaws.com/gardenio-sessions/coop/sessions/images/gd-Y/pic.jpg"
	got := rewriteSessionImageURL(ctx, in)
	want := "/image/coop/sessions/images/gd-Y/pic.jpg"
	if got != want {
		t.Errorf("path-style: got %q want %q", got, want)
	}
}

func TestRewriteSessionImageURL_PathStyleRegional(t *testing.T) {
	ctx := &bundleRenderCtx{
		basePath:    "",
		imageBucket: "gardenio-sessions",
		imagePrefix: "coop/sessions",
	}
	in := "https://s3.eu-west-1.amazonaws.com/gardenio-sessions/coop/sessions/images/gd-Z/foo.png"
	got := rewriteSessionImageURL(ctx, in)
	want := "/image/coop/sessions/images/gd-Z/foo.png"
	if got != want {
		t.Errorf("path-style regional: got %q want %q", got, want)
	}
}

func TestRewriteSessionImageURL_BasePathPrepended(t *testing.T) {
	ctx := &bundleRenderCtx{
		basePath:    "/beads",
		imageBucket: "gardenio-sessions",
		imagePrefix: "coop/sessions",
	}
	in := "https://gardenio-sessions.s3.amazonaws.com/coop/sessions/images/x/y.png"
	got := rewriteSessionImageURL(ctx, in)
	want := "/beads/image/coop/sessions/images/x/y.png"
	if got != want {
		t.Errorf("basePath prepend: got %q want %q", got, want)
	}
}

func TestRewriteSessionImageURL_NonS3HostPassthrough(t *testing.T) {
	ctx := &bundleRenderCtx{
		basePath:    "",
		imageBucket: "gardenio-sessions",
		imagePrefix: "coop/sessions",
	}
	for _, in := range []string{
		"https://evil.example.com/x.png",
		"https://example.org/foo.jpg",
		"https://gardenio-sessions.example.com/coop/sessions/images/x/y.png",
	} {
		got := rewriteSessionImageURL(ctx, in)
		if got != "" {
			t.Errorf("non-S3 host %q: want empty (passthrough), got %q", in, got)
		}
	}
}

func TestRewriteSessionImageURL_WrongBucketPathStyle(t *testing.T) {
	ctx := &bundleRenderCtx{
		basePath:    "",
		imageBucket: "gardenio-sessions",
		imagePrefix: "coop/sessions",
	}
	in := "https://s3.amazonaws.com/some-other-bucket/coop/sessions/images/x/y.png"
	got := rewriteSessionImageURL(ctx, in)
	if got != "" {
		t.Errorf("wrong bucket: want empty, got %q", got)
	}
}

func TestRewriteSessionImageURL_OutOfPrefix(t *testing.T) {
	ctx := &bundleRenderCtx{
		basePath:    "",
		imageBucket: "gardenio-sessions",
		imagePrefix: "coop/sessions",
	}
	// Transcripts subtree — not under the images/ prefix.
	in := "https://gardenio-sessions.s3.amazonaws.com/coop/sessions/transcripts/x/y.log"
	got := rewriteSessionImageURL(ctx, in)
	if got != "" {
		t.Errorf("out-of-prefix key: want empty, got %q", got)
	}
}

func TestRewriteSessionImageURL_EmptyConfig(t *testing.T) {
	// nil ctx → empty (fail-closed).
	if got := rewriteSessionImageURL(nil, "https://gardenio-sessions.s3.amazonaws.com/coop/sessions/images/x.png"); got != "" {
		t.Errorf("nil ctx: want empty, got %q", got)
	}
	// Unconfigured bucket → empty (image proxy disabled).
	ctx := &bundleRenderCtx{imageBucket: "", imagePrefix: "coop/sessions"}
	if got := rewriteSessionImageURL(ctx, "https://gardenio-sessions.s3.amazonaws.com/coop/sessions/images/x.png"); got != "" {
		t.Errorf("unset bucket: want empty, got %q", got)
	}
}

func TestRewriteSessionImageURL_PercentEncodedKey(t *testing.T) {
	ctx := &bundleRenderCtx{
		basePath:    "",
		imageBucket: "gardenio-sessions",
		imagePrefix: "coop/sessions",
	}
	// gb image upload percent-encodes each segment (sees a space → %20).
	in := "https://gardenio-sessions.s3.amazonaws.com/coop/sessions/images/gd-A/my%20file.png"
	got := rewriteSessionImageURL(ctx, in)
	want := "/image/coop/sessions/images/gd-A/my%20file.png"
	if got != want {
		t.Errorf("percent-encoded key: got %q want %q", got, want)
	}
}

func TestRenderBundleMarkdown_InlineImage_ProxyRewrite(t *testing.T) {
	ctx := newTestBundleRenderCtx()
	ctx.imageBucket = "gardenio-sessions"
	ctx.imagePrefix = "coop/sessions"
	card := bundleDocCard{
		Format:  "md",
		Content: `![alt](https://gardenio-sessions.s3.amazonaws.com/coop/sessions/images/gd-A/x.png)`,
	}
	got, err := renderBundleMarkdown(card, ctx)
	if err != nil {
		t.Fatalf("renderBundleMarkdown: %v", err)
	}
	s := string(got)
	if !strings.Contains(s, `src="/image/coop/sessions/images/gd-A/x.png"`) {
		t.Errorf("want rewritten /image/* URL, got %q", s)
	}
	if !strings.Contains(s, `class="bundle-md-image"`) {
		t.Errorf("want sanitized <img>, got %q", s)
	}
	// Whitelist fallback path must NOT fire — the rewrite path takes over.
	if strings.Contains(s, `bundle-md-image-fallback`) {
		t.Errorf("rewrite should bypass fallback link, got %q", s)
	}
}

func TestRenderBundleImage_ProxyRewrite(t *testing.T) {
	ctx := newTestBundleRenderCtx()
	ctx.imageBucket = "gardenio-sessions"
	ctx.imagePrefix = "coop/sessions"
	card := bundleDocCard{
		Format:     "image",
		Headline:   "Screenshot",
		ContentURL: "https://gardenio-sessions.s3.amazonaws.com/coop/sessions/images/gd-B/shot.png",
	}
	got, err := renderBundleImage(card, ctx)
	if err != nil {
		t.Fatalf("renderBundleImage: %v", err)
	}
	s := string(got)
	if !strings.Contains(s, `src="/image/coop/sessions/images/gd-B/shot.png"`) {
		t.Errorf("want rewritten /image/* URL, got %q", s)
	}
	if !strings.Contains(s, `class="bundle-image"`) {
		t.Errorf("want bundle-image <img>, got %q", s)
	}
	if !strings.Contains(s, `alt="Screenshot"`) {
		t.Errorf("want headline as alt, got %q", s)
	}
}

// ── Handler tests ─────────────────────────────────────────────────────────

func newImageProxyServerForTest(t *testing.T, f s3ImageGetter) *Server {
	t.Helper()
	srv := &Server{logger: slog.Default()}
	if f != nil {
		srv.SetImageProxy(f, "gardenio-sessions", "coop/sessions")
	}
	return srv
}

func newImageProxyMux(srv *Server) *http.ServeMux {
	mux := http.NewServeMux()
	// ka-0000007953: /image/{key...} has authz parity with the bundle page,
	// so it is registered without a role gate. Matches server.go.
	mux.HandleFunc("GET /image/{key...}", srv.handleImage)
	return mux
}

func TestHandleImage_200(t *testing.T) {
	f := newFakeS3()
	key := "coop/sessions/images/gd-A/shot.png"
	f.objects[key] = []byte("PNG-BYTES")
	f.contentTypes[key] = "image/png"
	f.etags[key] = `"abc123"`

	srv := newImageProxyServerForTest(t, f)
	mux := newImageProxyMux(srv)

	req := httptest.NewRequest(http.MethodGet, "/image/"+key, nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%q", rr.Code, rr.Body.String())
	}
	if got := rr.Body.String(); got != "PNG-BYTES" {
		t.Errorf("body: got %q want %q", got, "PNG-BYTES")
	}
	if got := rr.Header().Get("Content-Type"); got != "image/png" {
		t.Errorf("Content-Type: got %q want image/png", got)
	}
	if got := rr.Header().Get("ETag"); got != `"abc123"` {
		t.Errorf("ETag: got %q", got)
	}
	if got := rr.Header().Get("Cache-Control"); got != imageProxyCacheControl {
		t.Errorf("Cache-Control: got %q want %q", got, imageProxyCacheControl)
	}
	if f.lastRequestedKey != key {
		t.Errorf("S3 key requested: got %q want %q", f.lastRequestedKey, key)
	}
}

func TestHandleImage_404OnMissing(t *testing.T) {
	f := newFakeS3() // empty
	srv := newImageProxyServerForTest(t, f)
	mux := newImageProxyMux(srv)

	req := httptest.NewRequest(http.MethodGet, "/image/coop/sessions/images/gd-X/missing.png", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status: got %d want 404; body=%q", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "NoSuchKey") {
		t.Errorf("body leaks AWS error: %q", rr.Body.String())
	}
}

func TestHandleImage_403OutOfPrefix(t *testing.T) {
	f := newFakeS3()
	// Even if the key existed, the handler must refuse out-of-prefix paths
	// before touching S3 — defense-in-depth.
	f.objects["coop/sessions/transcripts/foo/bar.log"] = []byte("x")
	srv := newImageProxyServerForTest(t, f)
	mux := newImageProxyMux(srv)

	req := httptest.NewRequest(http.MethodGet, "/image/coop/sessions/transcripts/foo/bar.log", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status: got %d want 403; body=%q", rr.Code, rr.Body.String())
	}
	if f.getCalls != 0 {
		t.Errorf("out-of-prefix request should not reach S3; got %d calls", f.getCalls)
	}
}

func TestHandleImage_403OnAccessDenied(t *testing.T) {
	f := newFakeS3()
	f.wantErr = &smithyAPIError{code: "AccessDenied", msg: "denied"}
	srv := newImageProxyServerForTest(t, f)
	mux := newImageProxyMux(srv)

	req := httptest.NewRequest(http.MethodGet, "/image/coop/sessions/images/x/y.png", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status: got %d want 403; body=%q", rr.Code, rr.Body.String())
	}
}

func TestHandleImage_502OnUnknownUpstreamError(t *testing.T) {
	f := newFakeS3()
	f.wantErr = errors.New("connection refused")
	srv := newImageProxyServerForTest(t, f)
	mux := newImageProxyMux(srv)

	req := httptest.NewRequest(http.MethodGet, "/image/coop/sessions/images/x/y.png", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status: got %d want 502; body=%q", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "connection refused") {
		t.Errorf("body leaks upstream error: %q", rr.Body.String())
	}
}

func TestHandleImage_304OnIfNoneMatch(t *testing.T) {
	f := newFakeS3()
	key := "coop/sessions/images/gd-A/x.png"
	f.objects[key] = []byte("bytes")
	f.notModifiedKeys[key] = struct{}{}

	srv := newImageProxyServerForTest(t, f)
	mux := newImageProxyMux(srv)

	req := httptest.NewRequest(http.MethodGet, "/image/"+key, nil)
	req.Header.Set("If-None-Match", `"abc"`)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotModified {
		t.Fatalf("status: got %d want 304", rr.Code)
	}
	if f.gotIfNoneMatch != `"abc"` {
		t.Errorf("If-None-Match: got %q want %q", f.gotIfNoneMatch, `"abc"`)
	}
}

func TestHandleImage_503WhenNotConfigured(t *testing.T) {
	srv := newImageProxyServerForTest(t, nil) // no S3 client wired
	mux := newImageProxyMux(srv)

	req := httptest.NewRequest(http.MethodGet, "/image/coop/sessions/images/x/y.png", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d want 503", rr.Code)
	}
}

func TestHandleImage_BundleParity_NonViewerRoleServed(t *testing.T) {
	// ka-0000007953: /image/{key...} has authz parity with the bundle page,
	// which is reachable by any authenticated OIDC user (Keycloak realm role
	// OR AVA + GitLab groups, neither of which is required to include
	// `gardenio-viewer`). Pin that the route does NOT require the viewer role
	// — a request stamped with a non-viewer role is still served.
	f := newFakeS3()
	key := "coop/sessions/images/x/y.png"
	f.objects[key] = []byte("PNG-BYTES")
	f.contentTypes[key] = "image/png"
	srv := newImageProxyServerForTest(t, f)
	mux := newImageProxyMux(srv)

	req := httptest.NewRequest(http.MethodGet, "/image/"+key, nil)
	// Stamp an OIDC role context with a role that isn't viewer-tier — would
	// be rejected if viewerOnly were still wrapped around the route.
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{"not-a-real-role"}))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200 (no role gate); body=%q", rr.Code, rr.Body.String())
	}
	if got := rr.Body.String(); got != "PNG-BYTES" {
		t.Errorf("body: got %q want %q", got, "PNG-BYTES")
	}
	if f.getCalls == 0 {
		t.Errorf("expected S3 GetObject call, got 0")
	}
}

// TestImageProxyS3EndpointOpts_Unset verifies that with COOP_S3_ENDPOINT unset,
// the helper returns no options, leaving default AWS S3 endpoint + addressing.
func TestImageProxyS3EndpointOpts_Unset(t *testing.T) {
	t.Setenv("COOP_S3_ENDPOINT", "")

	if opts := imageProxyS3EndpointOpts(); opts != nil {
		t.Fatalf("expected nil opts when COOP_S3_ENDPOINT unset, got %d", len(opts))
	}
	var o s3.Options
	for _, f := range imageProxyS3EndpointOpts() {
		f(&o)
	}
	if o.BaseEndpoint != nil {
		t.Errorf("expected nil BaseEndpoint, got %q", *o.BaseEndpoint)
	}
	if o.UsePathStyle {
		t.Error("expected UsePathStyle=false when endpoint unset")
	}
}

// TestImageProxyS3EndpointOpts_Set verifies COOP_S3_ENDPOINT sets BaseEndpoint
// and enables path-style addressing (in-box MinIO).
func TestImageProxyS3EndpointOpts_Set(t *testing.T) {
	t.Setenv("COOP_S3_ENDPOINT", "  http://minio.gardenio.svc:9000  ")

	opts := imageProxyS3EndpointOpts()
	if len(opts) != 1 {
		t.Fatalf("expected 1 opt when endpoint set, got %d", len(opts))
	}
	var o s3.Options
	for _, f := range opts {
		f(&o)
	}
	if o.BaseEndpoint == nil || *o.BaseEndpoint != "http://minio.gardenio.svc:9000" {
		t.Errorf("expected trimmed BaseEndpoint, got %v", o.BaseEndpoint)
	}
	if !o.UsePathStyle {
		t.Error("expected UsePathStyle=true when endpoint set")
	}
}
