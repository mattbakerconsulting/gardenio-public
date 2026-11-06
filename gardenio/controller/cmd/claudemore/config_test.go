package main

import (
	"context"
	"net/http"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"gardenio/controller/internal/claudemore"
)

// newCmdTestCassette builds a valid cassette via the real canonicalizer so a
// filesystem-store round-trip in the cmd package uses a genuine (model, hash).
func newCmdTestCassette(t *testing.T) claudemore.Cassette {
	t.Helper()
	req, model, hash, err := claudemore.CanonicalizeRequest(claudemore.RequestIdentity{
		Method: http.MethodPost, Path: "/v1/messages",
		Body: []byte(`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hi"}]}`),
	})
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	return claudemore.NewCassette(req, model, hash,
		claudemore.CassetteResponse{StatusCode: 200, Body: []byte("ok")}, 1700000000000)
}

func TestParseConfig_Defaults(t *testing.T) {
	// No CLAUDEMORE_/COOP_ env set in the test process → defaults apply.
	for _, k := range []string{
		"CLAUDEMORE_LISTEN", "CLAUDEMORE_UPSTREAM_URL", "LOG_LEVEL",
		"COOP_S3_BUCKET", "COOP_S3_PREFIX", "COOP_S3_REGION", "COOP_S3_ENDPOINT",
	} {
		t.Setenv(k, "")
	}
	cfg := parseConfig()
	if cfg.listenAddr != ":8100" {
		t.Errorf("listenAddr default: got %q", cfg.listenAddr)
	}
	if cfg.upstreamURL != "http://gardenio-litellm:4000" {
		t.Errorf("upstreamURL default: got %q", cfg.upstreamURL)
	}
	if cfg.s3Bucket != "" {
		t.Errorf("s3Bucket should default empty (recording off): got %q", cfg.s3Bucket)
	}
}

func TestParseConfig_FromEnv(t *testing.T) {
	t.Setenv("CLAUDEMORE_LISTEN", ":9000")
	t.Setenv("CLAUDEMORE_UPSTREAM_URL", "http://litellm.test:4000")
	t.Setenv("COOP_S3_BUCKET", "my-bucket")
	t.Setenv("COOP_S3_PREFIX", "cass/x")
	t.Setenv("COOP_S3_REGION", "us-west-2")
	t.Setenv("COOP_S3_ENDPOINT", "http://minio:9000")
	t.Setenv("CLAUDEMORE_CASSETTE_DIR", "/var/cassettes")
	cfg := parseConfig()
	if cfg.listenAddr != ":9000" || cfg.upstreamURL != "http://litellm.test:4000" {
		t.Errorf("listen/upstream not read from env: %+v", cfg)
	}
	if cfg.s3Bucket != "my-bucket" || cfg.s3Prefix != "cass/x" ||
		cfg.s3Region != "us-west-2" || cfg.s3Endpoint != "http://minio:9000" {
		t.Errorf("S3 config not read from env: %+v", cfg)
	}
	if cfg.cassetteDir != "/var/cassettes" {
		t.Errorf("cassetteDir not read from env: %q", cfg.cassetteDir)
	}
}

func TestBuildCassetteStore_FilesystemWhenDirSet(t *testing.T) {
	dir := t.TempDir()
	store, err := buildCassetteStore(context.Background(), config{cassetteDir: dir})
	if err != nil {
		t.Fatalf("buildCassetteStore(fs): %v", err)
	}
	if store == nil {
		t.Fatal("nil store")
	}
	// It must be the filesystem store, not S3 — an S3 client would have needed
	// creds/region. Round-trip through it to prove it writes to the dir.
	c := newCmdTestCassette(t)
	if err := store.Put(context.Background(), c); err != nil {
		t.Fatalf("fs store Put: %v", err)
	}
	got, err := store.Get(context.Background(), c.Model, c.RequestHash)
	if err != nil {
		t.Fatalf("fs store Get: %v", err)
	}
	if got.RequestHash != c.RequestHash {
		t.Errorf("round-trip hash mismatch: %s vs %s", got.RequestHash, c.RequestHash)
	}
}

func TestEnvOr(t *testing.T) {
	t.Setenv("SOME_CLAUDEMORE_KEY", "")
	if got := envOr("SOME_CLAUDEMORE_KEY", "fallback"); got != "fallback" {
		t.Errorf("envOr empty: got %q want fallback", got)
	}
	t.Setenv("SOME_CLAUDEMORE_KEY", "set")
	if got := envOr("SOME_CLAUDEMORE_KEY", "fallback"); got != "set" {
		t.Errorf("envOr set: got %q want set", got)
	}
}

func TestSetupLogger_Levels(t *testing.T) {
	for _, lvl := range []string{"debug", "warn", "error", "info", "bogus", ""} {
		if setupLogger(lvl) == nil {
			t.Errorf("setupLogger(%q) returned nil", lvl)
		}
	}
}

func TestCassetteS3EndpointOpts(t *testing.T) {
	if opts := cassetteS3EndpointOpts(config{s3Endpoint: ""}); opts != nil {
		t.Error("empty endpoint should yield no opts (default AWS endpoint)")
	}
	opts := cassetteS3EndpointOpts(config{s3Endpoint: "http://minio:9000"})
	if len(opts) != 1 {
		t.Fatalf("expected 1 endpoint opt, got %d", len(opts))
	}
	var o s3.Options
	opts[0](&o)
	if aws.ToString(o.BaseEndpoint) != "http://minio:9000" || !o.UsePathStyle {
		t.Errorf("endpoint opt not applied: base=%q pathStyle=%v", aws.ToString(o.BaseEndpoint), o.UsePathStyle)
	}
}

func TestNewCassetteS3Client_Builds(t *testing.T) {
	// Region set so config load doesn't reach for IMDS; no creds needed to
	// construct the client (they resolve lazily on first call).
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	c, err := newCassetteS3Client(context.Background(), config{s3Region: "us-east-1"})
	if err != nil {
		t.Fatalf("newCassetteS3Client: %v", err)
	}
	if c == nil {
		t.Fatal("nil client")
	}
}

func TestBuildRecorder_RejectsEmptyBucket(t *testing.T) {
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	// s3Bucket empty → NewS3CassetteStore inside buildRecorder errors.
	_, err := buildRecorder(context.Background(), config{s3Region: "us-east-1"}, testLogger())
	if err == nil {
		t.Error("buildRecorder with empty bucket should error")
	}
}

func TestBuildRecorder_Succeeds(t *testing.T) {
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	rec, err := buildRecorder(context.Background(), config{s3Region: "us-east-1", s3Bucket: "b", s3Prefix: "p"}, testLogger())
	if err != nil {
		t.Fatalf("buildRecorder: %v", err)
	}
	if rec == nil {
		t.Fatal("nil recorder")
	}
	rec.Close()
}
