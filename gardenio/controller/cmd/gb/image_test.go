package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// fakeUploader records every PutObject call so tests can inspect the key,
// body, and content-type without spinning up a real S3 endpoint.
type fakeUploader struct {
	mu      sync.Mutex
	puts    []putCall
	failNow bool
}

type putCall struct {
	Bucket      string
	Key         string
	Body        []byte
	ContentType string
}

func (f *fakeUploader) PutObject(ctx context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNow {
		return nil, errors.New("simulated put failure")
	}
	body, _ := io.ReadAll(in.Body)
	call := putCall{Body: body}
	if in.Bucket != nil {
		call.Bucket = *in.Bucket
	}
	if in.Key != nil {
		call.Key = *in.Key
	}
	if in.ContentType != nil {
		call.ContentType = *in.ContentType
	}
	f.puts = append(f.puts, call)
	return &s3.PutObjectOutput{}, nil
}

func withFakeUploader(t *testing.T, fu *fakeUploader) {
	t.Helper()
	prev := newS3Uploader
	newS3Uploader = func(ctx context.Context) (s3Uploader, error) {
		return fu, nil
	}
	t.Cleanup(func() { newS3Uploader = prev })
}

func resetImageFlags(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		_ = imageUploadCmd.Flags().Set("bead", "")
		_ = imageUploadCmd.Flags().Set("bundle", "")
		_ = imageUploadCmd.Flags().Set("content-type", "")
		_ = imageUploadCmd.Flags().Set("max-bytes", "10485760")
		_ = imageUploadCmd.Flags().Set("headline", "")
	})
}

func TestImageScope(t *testing.T) {
	fixed := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		bead, bundle, want string
	}{
		{"gd-Bead", "", "gd-Bead"},
		{"", "gd-Bundle", "gd-Bundle"},
		{"gd-Bead", "gd-Bundle", "gd-Bead"},
		{"", "", "2026-05-15"},
	}
	for _, c := range cases {
		if got := imageScope(c.bead, c.bundle, fixed); got != c.want {
			t.Errorf("imageScope(%q,%q) = %q, want %q", c.bead, c.bundle, got, c.want)
		}
	}
}

func TestImageKey_LayoutAndSha8(t *testing.T) {
	data := []byte("hello world")
	// sha256("hello world") = b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9
	got := imageKey("coop/sessions", "gd-Bead", "screenshot.png", data)
	want := "coop/sessions/images/gd-Bead/b94d27b9-screenshot.png"
	if got != want {
		t.Errorf("imageKey = %q, want %q", got, want)
	}

	// Trailing-slash prefix should not produce a double slash.
	got = imageKey("coop/sessions/", "gd-Bead", "screenshot.png", data)
	if got != want {
		t.Errorf("imageKey with trailing slash = %q, want %q", got, want)
	}
}

func TestImageKey_SanitizesBasename(t *testing.T) {
	data := []byte("x")
	got := imageKey("p", "scope", "spaced name (1).png", data)
	if strings.Contains(got, " ") || strings.Contains(got, "(") {
		t.Errorf("imageKey did not sanitize: %q", got)
	}
	if !strings.HasSuffix(got, ".png") {
		t.Errorf("imageKey lost extension: %q", got)
	}
}

func TestSanitizeBasename(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"screenshot.png", "screenshot.png"},
		{"spaced name.png", "spaced-name.png"},
		{"weird?name#1.jpg", "weird-name-1.jpg"},
		{"", "file"},
		{".", "file"},
		{"path/to/file.txt", "file.txt"},
	}
	for _, c := range cases {
		if got := sanitizeBasename(c.in); got != c.want {
			t.Errorf("sanitizeBasename(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestImageURL_RegionForms(t *testing.T) {
	cases := []struct {
		bucket, region, key, want string
	}{
		{"b", "", "p/images/scope/abc-x.png", "https://b.s3.amazonaws.com/p/images/scope/abc-x.png"},
		{"b", "us-east-1", "p/k.png", "https://b.s3.amazonaws.com/p/k.png"},
		{"b", "us-west-2", "p/k.png", "https://b.s3.us-west-2.amazonaws.com/p/k.png"},
	}
	for _, c := range cases {
		if got := imageURL(c.bucket, c.region, c.key); got != c.want {
			t.Errorf("imageURL(%q,%q,%q) = %q, want %q", c.bucket, c.region, c.key, got, c.want)
		}
	}
}

func TestEncodeS3Key_KeepsSlashesEscapesSegments(t *testing.T) {
	got := encodeS3Key("a/b c/d.png")
	if got != "a/b%20c/d.png" {
		t.Errorf("encodeS3Key = %q, want a/b%%20c/d.png", got)
	}
}

func TestSniffContentType(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"shot.png", "image/png"},
		{"a.jpg", "image/jpeg"},
		{"a.json", "application/json"},
		{"binary", "application/octet-stream"},
		// Not ".xyz": Debian/Ubuntu's /etc/mime.types registers it as
		// chemical/x-xyz, and mime.TypeByExtension reads that table on the
		// CI runners. Pick a suffix no registry knows.
		{"unknown.zzznotatype", "application/octet-stream"},
	}
	for _, c := range cases {
		got := sniffContentType(c.path)
		// MIME registry sometimes adds charset for json; accept prefix match.
		if !strings.HasPrefix(got, c.want) {
			t.Errorf("sniffContentType(%q) = %q, want prefix %q", c.path, got, c.want)
		}
	}

	// .md and .log are forced overrides regardless of system MIME table.
	if !strings.HasPrefix(sniffContentType("notes.md"), "text/") {
		t.Errorf("notes.md should be text/*")
	}
}

func TestImageUpload_PutObjectAndPrintsURL(t *testing.T) {
	resetImageFlags(t)
	t.Setenv("COOP_S3_BUCKET", "my-bucket")
	t.Setenv("COOP_S3_PREFIX", "coop/sessions")
	t.Setenv("COOP_S3_REGION", "us-west-2")

	dir := t.TempDir()
	p := filepath.Join(dir, "shot.png")
	body := bytes.Repeat([]byte("a"), 17)
	if err := os.WriteFile(p, body, 0o644); err != nil {
		t.Fatal(err)
	}

	fu := &fakeUploader{}
	withFakeUploader(t, fu)

	cmd := imageUploadCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("bead", "gd-Bead1")

	if err := cmd.RunE(cmd, []string{p}); err != nil {
		t.Fatalf("RunE: %v", err)
	}

	if len(fu.puts) != 1 {
		t.Fatalf("expected 1 put, got %d", len(fu.puts))
	}
	put := fu.puts[0]
	if put.Bucket != "my-bucket" {
		t.Errorf("bucket = %q", put.Bucket)
	}
	if !strings.HasPrefix(put.Key, "coop/sessions/images/gd-Bead1/") {
		t.Errorf("key has wrong scope: %q", put.Key)
	}
	if !strings.HasSuffix(put.Key, "-shot.png") {
		t.Errorf("key missing basename suffix: %q", put.Key)
	}
	if !bytes.Equal(put.Body, body) {
		t.Errorf("body bytes mismatch (len got=%d want=%d)", len(put.Body), len(body))
	}
	if !strings.HasPrefix(put.ContentType, "image/png") {
		t.Errorf("content-type = %q, want image/png", put.ContentType)
	}
}

func TestImageProxyURL(t *testing.T) {
	cases := []struct {
		name, viewer, key, want string
	}{
		{
			name:   "viewer set with /beads prefix",
			viewer: "https://gardenio.app.e2e.dev.acme.example/beads",
			key:    "coop/sessions/images/2026-06-22/abc12345-chart.png",
			want:   "https://gardenio.app.e2e.dev.acme.example/beads/image/coop/sessions/images/2026-06-22/abc12345-chart.png",
		},
		{
			name:   "trailing slash trimmed",
			viewer: "https://host/beads/",
			key:    "coop/sessions/images/gd-Bead/abc-x.png",
			want:   "https://host/beads/image/coop/sessions/images/gd-Bead/abc-x.png",
		},
		{
			name:   "spaces in basename are encoded per-segment",
			viewer: "https://host/beads",
			key:    "p/images/scope/abc-spaced name.png",
			want:   "https://host/beads/image/p/images/scope/abc-spaced%20name.png",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("BEADS_VIEWER_URL", c.viewer)
			if got := imageProxyURL(c.key); got != c.want {
				t.Errorf("imageProxyURL(%q) = %q, want %q", c.key, got, c.want)
			}
		})
	}

	// Unset env falls back to the configured host's default viewer URL, still
	// absolute and still carrying the /image/ proxy segment. There is no
	// compiled-in host, so pin one as a build or $GARDENIO_DEFAULT_HOST would.
	t.Run("falls back to default viewer when unset", func(t *testing.T) {
		t.Setenv("BEADS_VIEWER_URL", "")
		saved := defaultGardenioViewerURL
		t.Cleanup(func() { defaultGardenioViewerURL = saved })
		defaultGardenioViewerURL = viewerURLForHost("gardenio.example.com")
		got := imageProxyURL("coop/sessions/images/x/y.png")
		if !strings.HasPrefix(got, "https://") {
			t.Errorf("fallback proxy URL not absolute: %q", got)
		}
		if !strings.Contains(got, "/image/coop/sessions/images/x/y.png") {
			t.Errorf("fallback proxy URL missing /image/ segment: %q", got)
		}
	})
}

// TestImageUpload_PrintsProxiedURLAsPrimary asserts the human-facing stdout is
// the viewer-proxied (shareable) URL — NOT the raw private-S3 URL (ka-0000003520).
func TestImageUpload_PrintsProxiedURLAsPrimary(t *testing.T) {
	resetImageFlags(t)
	t.Setenv("COOP_S3_BUCKET", "gardenio-sessions")
	t.Setenv("COOP_S3_PREFIX", "coop/sessions")
	t.Setenv("COOP_S3_REGION", "us-east-1")
	t.Setenv("BEADS_VIEWER_URL", "https://gardenio.app.e2e.dev.acme.example/beads")

	dir := t.TempDir()
	p := filepath.Join(dir, "chart.png")
	if err := os.WriteFile(p, []byte("png-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	fu := &fakeUploader{}
	withFakeUploader(t, fu)

	cmd := imageUploadCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("bead", "gd-Bead1")

	out := captureStdout(t, func() error {
		return cmd.RunE(cmd, []string{p})
	})

	stdout := strings.TrimSpace(string(out))
	// Primary stdout line must be the viewer-proxied URL.
	if !strings.HasPrefix(stdout, "https://gardenio.app.e2e.dev.acme.example/beads/image/coop/sessions/images/gd-Bead1/") {
		t.Errorf("stdout primary line is not the proxied URL: %q", stdout)
	}
	// The raw private-S3 URL must NOT be on stdout (it 403s for humans).
	if strings.Contains(stdout, "s3.amazonaws.com") {
		t.Errorf("raw S3 URL leaked onto stdout: %q", stdout)
	}
}

// TestImageUpload_JSONPrimaryURLIsProxied asserts the JSON "url" field is the
// proxied URL and the raw S3 URL is demoted to "storage_url".
func TestImageUpload_JSONPrimaryURLIsProxied(t *testing.T) {
	resetImageFlags(t)
	t.Setenv("COOP_S3_BUCKET", "gardenio-sessions")
	t.Setenv("COOP_S3_PREFIX", "coop/sessions")
	t.Setenv("COOP_S3_REGION", "us-east-1")
	t.Setenv("BEADS_VIEWER_URL", "https://host/beads")

	dir := t.TempDir()
	p := filepath.Join(dir, "chart.png")
	if err := os.WriteFile(p, []byte("png-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	fu := &fakeUploader{}
	withFakeUploader(t, fu)

	prevJSON := jsonOutput
	jsonOutput = true
	t.Cleanup(func() { jsonOutput = prevJSON })

	cmd := imageUploadCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("bead", "gd-Bead1")

	out := captureStdout(t, func() error {
		return cmd.RunE(cmd, []string{p})
	})

	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal json output: %v (raw=%q)", err, string(out))
	}
	primary, _ := got["url"].(string)
	if !strings.HasPrefix(primary, "https://host/beads/image/coop/sessions/images/gd-Bead1/") {
		t.Errorf("json url is not the proxied URL: %q", primary)
	}
	storage, _ := got["storage_url"].(string)
	if !strings.Contains(storage, "gardenio-sessions.s3.amazonaws.com") {
		t.Errorf("json storage_url is not the raw S3 URL: %q", storage)
	}
}

func TestImageUpload_RejectsMissingBucket(t *testing.T) {
	resetImageFlags(t)
	t.Setenv("COOP_S3_BUCKET", "")

	dir := t.TempDir()
	p := filepath.Join(dir, "x.png")
	_ = os.WriteFile(p, []byte("x"), 0o644)

	cmd := imageUploadCmd
	cmd.SetContext(context.Background())
	err := cmd.RunE(cmd, []string{p})
	if err == nil || !strings.Contains(err.Error(), "COOP_S3_BUCKET") {
		t.Fatalf("expected bucket-not-set error; got %v", err)
	}
}

func TestImageUpload_RejectsMissingPath(t *testing.T) {
	resetImageFlags(t)
	t.Setenv("COOP_S3_BUCKET", "b")

	cmd := imageUploadCmd
	cmd.SetContext(context.Background())
	err := cmd.RunE(cmd, []string{"/nonexistent/path/screen.png"})
	if err == nil || !strings.Contains(err.Error(), "stat") {
		t.Fatalf("expected stat error; got %v", err)
	}
}

func TestImageUpload_RejectsDirectory(t *testing.T) {
	resetImageFlags(t)
	t.Setenv("COOP_S3_BUCKET", "b")

	dir := t.TempDir()
	cmd := imageUploadCmd
	cmd.SetContext(context.Background())
	err := cmd.RunE(cmd, []string{dir})
	if err == nil || !strings.Contains(err.Error(), "directory") {
		t.Fatalf("expected directory error; got %v", err)
	}
}

func TestImageUpload_EnforcesMaxBytes(t *testing.T) {
	resetImageFlags(t)
	t.Setenv("COOP_S3_BUCKET", "b")

	dir := t.TempDir()
	p := filepath.Join(dir, "big.bin")
	if err := os.WriteFile(p, make([]byte, 1024), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := imageUploadCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("max-bytes", "100")

	err := cmd.RunE(cmd, []string{p})
	if err == nil || !strings.Contains(err.Error(), "max-bytes") {
		t.Fatalf("expected max-bytes error; got %v", err)
	}
}

func TestImageUpload_PropagatesPutFailure(t *testing.T) {
	resetImageFlags(t)
	t.Setenv("COOP_S3_BUCKET", "b")

	dir := t.TempDir()
	p := filepath.Join(dir, "shot.png")
	_ = os.WriteFile(p, []byte("hello"), 0o644)

	fu := &fakeUploader{failNow: true}
	withFakeUploader(t, fu)

	cmd := imageUploadCmd
	cmd.SetContext(context.Background())

	err := cmd.RunE(cmd, []string{p})
	if err == nil || !strings.Contains(err.Error(), "uploading to s3://b/") {
		t.Fatalf("expected upload error; got %v", err)
	}
}

func TestImageUpload_ContentTypeOverride(t *testing.T) {
	resetImageFlags(t)
	t.Setenv("COOP_S3_BUCKET", "b")
	t.Setenv("COOP_S3_PREFIX", "p")
	t.Setenv("COOP_S3_REGION", "")

	dir := t.TempDir()
	p := filepath.Join(dir, "blob")
	_ = os.WriteFile(p, []byte("xx"), 0o644)

	fu := &fakeUploader{}
	withFakeUploader(t, fu)

	cmd := imageUploadCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("content-type", "application/vnd.test")

	if err := cmd.RunE(cmd, []string{p}); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	if fu.puts[0].ContentType != "application/vnd.test" {
		t.Errorf("content-type = %q", fu.puts[0].ContentType)
	}
}

func TestImageUpload_BundleCreatesDocBead(t *testing.T) {
	resetImageFlags(t)
	t.Setenv("COOP_S3_BUCKET", "b")
	t.Setenv("COOP_S3_PREFIX", "p")
	t.Setenv("COOP_S3_REGION", "")

	stub := newBundleStub()
	stub.seed("gd-Bundle1", map[string]any{
		"title": "bundle", "type": "bundle", "kind": "issue",
	})
	setupBundleDaemon(t, stub)

	fu := &fakeUploader{}
	withFakeUploader(t, fu)

	dir := t.TempDir()
	p := filepath.Join(dir, "shot.png")
	_ = os.WriteFile(p, []byte("png-bytes"), 0o644)

	cmd := imageUploadCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("bundle", "gd-Bundle1")
	_ = cmd.Flags().Set("headline", "Login screen")

	if err := cmd.RunE(cmd, []string{p}); err != nil {
		t.Fatalf("RunE: %v", err)
	}

	if len(fu.puts) != 1 {
		t.Fatalf("expected 1 put, got %d", len(fu.puts))
	}
	if !strings.Contains(fu.puts[0].Key, "/images/gd-Bundle1/") {
		t.Errorf("bundle scope missing from key: %q", fu.puts[0].Key)
	}

	if len(stub.creates) != 1 {
		t.Fatalf("expected 1 doc create, got %d", len(stub.creates))
	}
	created := stub.creates[0]
	if created["type"] != "doc" {
		t.Errorf("created type = %v, want doc", created["type"])
	}
	if created["title"] != "shot.png" {
		t.Errorf("doc title = %v, want shot.png", created["title"])
	}

	// fields should include format=image, content_url=<https…>, headline=Login screen.
	rawJSON, _ := json.Marshal(created)
	var reparsed struct {
		Fields json.RawMessage `json:"fields"`
	}
	_ = json.Unmarshal(rawJSON, &reparsed)
	var fields map[string]any
	if err := json.Unmarshal(reparsed.Fields, &fields); err != nil {
		t.Fatalf("unmarshal fields: %v", err)
	}
	if fields["format"] != "image" {
		t.Errorf("format = %v, want image", fields["format"])
	}
	if u, _ := fields["content_url"].(string); !strings.HasPrefix(u, "https://b.s3.amazonaws.com/p/images/gd-Bundle1/") {
		t.Errorf("content_url unexpected: %v", fields["content_url"])
	}
	if fields["headline"] != "Login screen" {
		t.Errorf("headline = %v, want Login screen", fields["headline"])
	}

	// Doc should be linked parent-child under bundle, and closed.
	if len(stub.deps) != 1 || stub.deps[0]["depends_on_id"] != "gd-Bundle1" {
		t.Errorf("expected dep doc→bundle, got %+v", stub.deps)
	}
	if len(stub.closes) != 1 {
		t.Errorf("expected 1 close, got %d", len(stub.closes))
	}
}

func TestImageUpload_BundleRejectsNonBundle(t *testing.T) {
	resetImageFlags(t)
	t.Setenv("COOP_S3_BUCKET", "b")

	stub := newBundleStub()
	stub.seed("gd-Task", map[string]any{
		"title": "task", "type": "task", "kind": "issue",
	})
	setupBundleDaemon(t, stub)

	fu := &fakeUploader{}
	withFakeUploader(t, fu)

	dir := t.TempDir()
	p := filepath.Join(dir, "shot.png")
	_ = os.WriteFile(p, []byte("x"), 0o644)

	cmd := imageUploadCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("bundle", "gd-Task")

	err := cmd.RunE(cmd, []string{p})
	if err == nil || !strings.Contains(err.Error(), "is not a bundle") {
		t.Fatalf("expected non-bundle error; got %v", err)
	}
	// Upload still happened — we only fail the doc-bead step.
	if len(fu.puts) != 1 {
		t.Errorf("expected upload to succeed even when bundle binding fails; got %d puts", len(fu.puts))
	}
}

func TestImageUpload_DefaultScopeIsToday(t *testing.T) {
	resetImageFlags(t)
	t.Setenv("COOP_S3_BUCKET", "b")
	t.Setenv("COOP_S3_PREFIX", "p")

	dir := t.TempDir()
	pfile := filepath.Join(dir, "img.png")
	_ = os.WriteFile(pfile, []byte("xx"), 0o644)

	fu := &fakeUploader{}
	withFakeUploader(t, fu)

	cmd := imageUploadCmd
	cmd.SetContext(context.Background())
	if err := cmd.RunE(cmd, []string{pfile}); err != nil {
		t.Fatalf("RunE: %v", err)
	}

	today := time.Now().UTC().Format("2006-01-02")
	if !strings.Contains(fu.puts[0].Key, "/images/"+today+"/") {
		t.Errorf("expected today-scoped key %q; got %q", today, fu.puts[0].Key)
	}
}

// gd-CmjVAU3vgd: COOP_S3_IMAGES_BUCKET takes uploads off the session bucket,
// with the same key shape.
func TestImageUpload_UsesImagesBucket(t *testing.T) {
	resetImageFlags(t)
	t.Setenv("COOP_S3_BUCKET", "gardenio-sessions")
	t.Setenv("COOP_S3_IMAGES_BUCKET", "gardenio-images")
	t.Setenv("COOP_S3_PREFIX", "coop/sessions")

	p := filepath.Join(t.TempDir(), "dot.png")
	if err := os.WriteFile(p, []byte{0x89, 'P', 'N', 'G'}, 0o644); err != nil {
		t.Fatal(err)
	}
	fu := &fakeUploader{}
	withFakeUploader(t, fu)

	cmd := imageUploadCmd
	cmd.SetContext(context.Background())
	if err := cmd.RunE(cmd, []string{p}); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	if len(fu.puts) != 1 || fu.puts[0].Bucket != "gardenio-images" {
		t.Fatalf("puts = %+v, want one put to gardenio-images", fu.puts)
	}
	if !strings.HasPrefix(fu.puts[0].Key, "coop/sessions/images/") {
		t.Errorf("key %q lost the <prefix>/images/ shape", fu.puts[0].Key)
	}
}
