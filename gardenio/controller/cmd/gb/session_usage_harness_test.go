package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

const coopUsageBody = `{"input_tokens":1200,"output_tokens":340,"cache_read_tokens":800,"cache_write_tokens":50,"request_count":4,"total_api_ms":9000,"uptime_secs":120,"total_cost_usd":0,"context_tokens":2050}`

func fakeCoop(t *testing.T, hits *atomic.Int32, status int) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != "/api/v1/session/usage" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(coopUsageBody))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("COOP_URL", srv.URL+"/")
}

func TestLoadSessionUsage_CodexReadsCoop(t *testing.T) {
	var hits atomic.Int32
	fakeCoop(t, &hits, http.StatusOK)
	t.Setenv("GARDENIO_HARNESS", "codex")

	// The log path is irrelevant (and here deliberately a nonexistent file).
	u, err := loadSessionUsage("/nonexistent/rollout.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if u.InputTokens != 1200 || u.OutputTokens != 340 || u.CacheReadTokens != 800 ||
		u.CacheWriteTokens != 50 || u.RequestCount != 4 || u.ContextTokens != 2050 || u.TotalCostUSD != 0 {
		t.Errorf("usage = %+v", u)
	}
	if hits.Load() != 1 {
		t.Errorf("coop hits = %d", hits.Load())
	}
}

func TestLoadSessionUsage_CodexCoopDownIsAnError(t *testing.T) {
	resetCoopUsageSnapshot()
	t.Cleanup(resetCoopUsageSnapshot)
	var hits atomic.Int32
	fakeCoop(t, &hits, http.StatusInternalServerError)
	t.Setenv("GARDENIO_HARNESS", "codex")
	if u, err := loadSessionUsage(""); err == nil || u != nil {
		t.Fatalf("want error, got %+v, %v", u, err)
	}
}

func TestLoadSessionUsage_ClaudeStillParsesLogAndSkipsCoop(t *testing.T) {
	var hits atomic.Int32
	fakeCoop(t, &hits, http.StatusOK)
	for _, h := range []string{"", "claude"} {
		t.Setenv("GARDENIO_HARNESS", h)
		log := filepath.Join(t.TempDir(), "s.jsonl")
		line := `{"type":"assistant","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":10,"output_tokens":5,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}` + "\n"
		if err := os.WriteFile(log, []byte(line), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := loadSessionUsage(log)
		if err != nil {
			t.Fatal(err)
		}
		want, _ := parseSessionUsage(log)
		if *got != *want || got.InputTokens != 10 {
			t.Errorf("harness %q: loadSessionUsage = %+v, parseSessionUsage = %+v", h, got, want)
		}
	}
	if hits.Load() != 0 {
		t.Errorf("claude path called coop %d times", hits.Load())
	}
}

func TestSessionUsageFromCoop_Harnesses(t *testing.T) {
	for h, want := range map[string]bool{"": false, "claude": false, "codex": true, "gemini": true, "bogus": false} {
		t.Setenv("GARDENIO_HARNESS", h)
		if got := sessionUsageFromCoop(); got != want {
			t.Errorf("GARDENIO_HARNESS=%q: sessionUsageFromCoop = %v, want %v", h, got, want)
		}
	}
}

func TestCanLoadSessionUsage(t *testing.T) {
	t.Setenv("GARDENIO_HARNESS", "")
	if canLoadSessionUsage("") || !canLoadSessionUsage("/x.jsonl") {
		t.Error("claude: guard must stay logPath != \"\"")
	}
	t.Setenv("GARDENIO_HARNESS", "codex")
	if !canLoadSessionUsage("") {
		t.Error("codex: usage is loadable without a log path")
	}
}

// The close path runs after coop exited: coop answered once, then went away.
func TestLoadSessionUsage_CodexFallsBackToSnapshotWhenCoopExited(t *testing.T) {
	resetCoopUsageSnapshot()
	t.Cleanup(resetCoopUsageSnapshot)
	t.Setenv("GARDENIO_HARNESS", "codex")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(coopUsageBody))
	}))
	t.Setenv("COOP_URL", srv.URL)
	if _, err := loadSessionUsage(""); err != nil {
		t.Fatal(err)
	}
	srv.Close() // coop has exited

	u, err := loadSessionUsage("")
	if err != nil || u == nil {
		t.Fatalf("want cached snapshot, got %+v, %v", u, err)
	}
	if u.InputTokens != 1200 || u.OutputTokens != 340 || u.RequestCount != 4 || u.ContextTokens != 2050 {
		t.Errorf("snapshot = %+v", u)
	}

	// The returned value is a copy: mutating it must not corrupt the cache.
	u.InputTokens = 1
	if again, _ := loadSessionUsage(""); again.InputTokens != 1200 {
		t.Errorf("cache mutated through returned value: %+v", again)
	}
}

func TestLoadSessionUsage_CodexLiveReadWinsOverStaleSnapshot(t *testing.T) {
	resetCoopUsageSnapshot()
	t.Cleanup(resetCoopUsageSnapshot)
	storeCoopUsageSnapshot(&beadsapi.SessionUsage{InputTokens: 5})
	var hits atomic.Int32
	fakeCoop(t, &hits, http.StatusOK)
	t.Setenv("GARDENIO_HARNESS", "codex")
	u, err := loadSessionUsage("")
	if err != nil || u.InputTokens != 1200 {
		t.Fatalf("got %+v, %v", u, err)
	}
}

func TestStartCoopUsageSnapshot_PollsWithoutVMAgentAndResets(t *testing.T) {
	storeCoopUsageSnapshot(&beadsapi.SessionUsage{InputTokens: 99}) // previous session
	t.Cleanup(resetCoopUsageSnapshot)
	t.Setenv("GARDENIO_HARNESS", "codex")
	t.Setenv("VMAGENT_URL", "")
	old := coopUsageSnapshotInterval
	coopUsageSnapshotInterval = 5 * time.Millisecond
	t.Cleanup(func() { coopUsageSnapshotInterval = old })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(coopUsageBody))
	}))
	t.Setenv("COOP_URL", srv.URL)

	stop := startCoopUsageSnapshot(context.Background(), 0)
	if cachedCoopUsageSnapshot() != nil {
		t.Fatal("previous session's snapshot survived the reset")
	}
	deadline := time.Now().Add(2 * time.Second)
	for cachedCoopUsageSnapshot() == nil && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	stop()
	srv.Close()
	snap := cachedCoopUsageSnapshot()
	if snap == nil || snap.InputTokens != 1200 {
		t.Fatalf("poller did not snapshot coop usage: %+v", snap)
	}
}

func TestStartCoopUsageSnapshot_ExportsCoopURLFromPort(t *testing.T) {
	t.Setenv("GARDENIO_HARNESS", "codex")
	t.Setenv("COOP_URL", "")
	stop := startCoopUsageSnapshot(context.Background(), 8123)
	stop()
	if got := os.Getenv("COOP_URL"); got != "http://localhost:8123" {
		t.Errorf("COOP_URL = %q", got)
	}
	t.Setenv("COOP_URL", "http://elsewhere:1") // an explicit value is kept
	startCoopUsageSnapshot(context.Background(), 8123)()
	if got := os.Getenv("COOP_URL"); got != "http://elsewhere:1" {
		t.Errorf("COOP_URL overwritten: %q", got)
	}
}

func TestStartCoopUsageSnapshot_ClaudeIsInert(t *testing.T) {
	storeCoopUsageSnapshot(&beadsapi.SessionUsage{InputTokens: 7})
	t.Cleanup(resetCoopUsageSnapshot)
	t.Setenv("GARDENIO_HARNESS", "")
	t.Setenv("COOP_URL", "")
	startCoopUsageSnapshot(context.Background(), 8123)()
	if os.Getenv("COOP_URL") != "" {
		t.Error("claude path must not touch COOP_URL")
	}
}
