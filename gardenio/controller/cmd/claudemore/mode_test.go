package main

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestNormalizeMode(t *testing.T) {
	cases := map[string]string{
		"":         modeLive,
		"live":     modeLive,
		"LIVE":     modeLive,
		"  live  ": modeLive,
		"replay":   modeReplay,
		"REPLAY":   modeReplay,
		"replya":   "replya", // typo passes through so run() fails loudly
	}
	for in, want := range cases {
		if got := normalizeMode(in); got != want {
			t.Errorf("normalizeMode(%q): got %q want %q", in, got, want)
		}
	}
}

func TestParseConfig_ModeDefaultsLive(t *testing.T) {
	t.Setenv("CLAUDEMORE_MODE", "")
	if cfg := parseConfig(); cfg.mode != modeLive {
		t.Errorf("mode default: got %q want live", cfg.mode)
	}
	t.Setenv("CLAUDEMORE_MODE", "replay")
	if cfg := parseConfig(); cfg.mode != modeReplay {
		t.Errorf("mode from env: got %q want replay", cfg.mode)
	}
}

func TestBuildHandler_UnknownModeErrors(t *testing.T) {
	_, _, err := buildHandler(context.Background(), config{mode: "bogus"}, testLogger())
	if err == nil {
		t.Error("unknown mode should error")
	}
}

func TestBuildHandler_ReplayRequiresBucket(t *testing.T) {
	// REPLAY with no bucket must be a hard error, not a silent pass-through.
	_, _, err := buildHandler(context.Background(), config{mode: modeReplay}, testLogger())
	if err == nil {
		t.Error("REPLAY with empty bucket should error")
	}
	if !strings.Contains(err.Error(), "COOP_S3_BUCKET") {
		t.Errorf("REPLAY no-bucket error should mention COOP_S3_BUCKET: %v", err)
	}
}

func TestBuildHandler_LiveNoBucketPassthrough(t *testing.T) {
	// LIVE with no bucket builds a strict pass-through handler (no cleanup).
	h, cleanup, err := buildHandler(context.Background(), config{mode: modeLive, upstreamURL: "http://litellm:4000"}, testLogger())
	if err != nil {
		t.Fatalf("live no-bucket: %v", err)
	}
	if h == nil {
		t.Error("nil handler")
	}
	if cleanup != nil {
		cleanup()
	}
}

func TestBuildHandler_ReplayFilesystemStoreSatisfiesRequirement(t *testing.T) {
	// A CLAUDEMORE_CASSETTE_DIR (filesystem store) satisfies REPLAY's
	// store-required gate with NO S3 bucket — the Gasbox/BYO + hermetic-e2e path.
	dir := t.TempDir()
	h, cleanup, err := buildHandler(context.Background(),
		config{mode: modeReplay, cassetteDir: dir}, testLogger())
	if err != nil {
		t.Fatalf("REPLAY with cassetteDir should build, got err: %v", err)
	}
	if h == nil {
		t.Error("nil handler")
	}
	if cleanup != nil {
		cleanup()
	}
}

func TestBuildHandler_LiveFilesystemStoreAttachesRecorder(t *testing.T) {
	// LIVE + CLAUDEMORE_CASSETTE_DIR must attach the recorder (cleanup non-nil),
	// mirroring the S3-bucket record path but with a filesystem store.
	dir := t.TempDir()
	h, cleanup, err := buildHandler(context.Background(),
		config{mode: modeLive, upstreamURL: "http://litellm:4000", cassetteDir: dir}, testLogger())
	if err != nil {
		t.Fatalf("live with cassetteDir: %v", err)
	}
	if h == nil {
		t.Error("nil handler")
	}
	if cleanup == nil {
		t.Error("LIVE with a cassette store must attach the recorder (non-nil cleanup)")
	} else {
		cleanup()
	}
}

func TestConfig_HasCassetteStore(t *testing.T) {
	cases := []struct {
		name   string
		cfg    config
		expect bool
	}{
		{"none", config{}, false},
		{"s3 only", config{s3Bucket: "b"}, true},
		{"dir only", config{cassetteDir: "/tmp/x"}, true},
		{"both", config{s3Bucket: "b", cassetteDir: "/tmp/x"}, true},
	}
	for _, tc := range cases {
		if got := tc.cfg.hasCassetteStore(); got != tc.expect {
			t.Errorf("%s: hasCassetteStore()=%v want %v", tc.name, got, tc.expect)
		}
	}
}

// TestRun_ReplayModeServesCassetteMiss drives the full run() wiring in REPLAY
// mode against a bucket (the S3 client builds lazily; the first Get misses since
// nothing was recorded), asserting the server comes up and fail_on_miss fires.
func TestRun_ReplayModeServesCassetteMiss(t *testing.T) {
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	cfg := config{
		listenAddr: "127.0.0.1:18145",
		mode:       modeReplay,
		logLevel:   "error",
		s3Bucket:   "test-bucket",
		s3Region:   "us-east-1",
		// Point S3 at a closed local port so Get fails fast with connection
		// refused rather than reaching out to the network — a Get error (not a
		// clean miss); either way the run wiring must NOT fall back to LIVE. We
		// assert the server responds with a hard-failure status and the replay
		// marker header.
		s3Endpoint: "http://127.0.0.1:1",
	}
	cancel, done := startRun(t, cfg)

	resp, err := http.Post("http://127.0.0.1:18145/v1/messages", "application/json",
		strings.NewReader(`{"model":"m","messages":[]}`))
	if err != nil {
		t.Fatalf("replay request: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	// A cassette store error or miss both mean "no successful cassette served" —
	// the status must be a 4xx/5xx hard failure, never 200, and the REPLAY marker
	// header must be present (proving REPLAY handled it, not a LIVE proxy).
	if resp.StatusCode == http.StatusOK {
		t.Errorf("REPLAY served 200 with no cassette — must fail loudly")
	}
	if resp.Header.Get("X-Claudemore-Replay") == "" {
		t.Errorf("REPLAY marker header absent — request may have hit LIVE path")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(6 * time.Second):
		t.Fatal("run did not shut down after cancel")
	}
}

// TestRun_UnknownModeErrors proves run() refuses an unknown mode rather than
// silently serving.
func TestRun_UnknownModeErrors(t *testing.T) {
	err := run(context.Background(), config{listenAddr: "127.0.0.1:18146", mode: "nonsense"}, testLogger())
	if err == nil {
		t.Error("run with unknown mode should error")
	}
}
