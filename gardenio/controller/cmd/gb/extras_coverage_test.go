package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gardenio/controller/internal/advice"
	"gardenio/controller/internal/beadsapi"
)

// ── prime_advice_diff.go ─────────────────────────────────────────────────

func TestDetectRoleMismatch(t *testing.T) {
	// No labels
	if got := detectRoleMismatch(nil, "thread"); got != "" {
		t.Errorf("nil labels: %q", got)
	}
	// Match
	if got := detectRoleMismatch([]string{"role:thread"}, "thread"); got != "" {
		t.Errorf("match: %q", got)
	}
	// Mismatch
	if got := detectRoleMismatch([]string{"role:worker"}, "thread"); got != "worker" {
		t.Errorf("mismatch: %q", got)
	}
	// Non-role labels ignored
	if got := detectRoleMismatch([]string{"project:x", "team:y"}, "thread"); got != "" {
		t.Errorf("non-role: %q", got)
	}
}

func TestOutputAdviceDiffItems(t *testing.T) {
	var buf bytes.Buffer
	items := []advice.MatchedAdvice{
		{
			Scope: "project", ScopeTarget: "gardenio", ScopeHeader: "Project: gardenio",
			Bead: &beadsapi.BeadDetail{Title: "Rule A", Description: "Body A"},
		},
		{
			Scope: "global", ScopeTarget: "", ScopeHeader: "Global",
			Bead: &beadsapi.BeadDetail{Title: "Rule B", Description: ""},
		},
	}
	outputAdviceDiffItems(&buf, "gardenio/crews/test", items)
	out := buf.String()
	if out == "" {
		t.Error("expected non-empty output")
	}
}

// ── init_agent.go: shouldExitBeforeStart ─────────────────────────────────

func TestShouldExitBeforeStart_NoEnv(t *testing.T) {
	t.Setenv("KD_AGENT_ID", "")
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	if shouldExitBeforeStart(context.Background()) {
		t.Error("expected false when no agent id")
	}
}

func TestShouldExitBeforeStart_NoDaemon(t *testing.T) {
	t.Setenv("KD_AGENT_ID", "gd-A1")
	oldD := daemon
	daemon = nil
	t.Cleanup(func() { daemon = oldD })
	if shouldExitBeforeStart(context.Background()) {
		t.Error("expected false when daemon nil")
	}
}

func TestShouldExitBeforeStart_NoStopRequested(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "gd-A1", "title": "t", "type": "agent",
			"fields": json.RawMessage(`{}`),
		})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	t.Setenv("KD_AGENT_ID", "gd-A1")
	if shouldExitBeforeStart(context.Background()) {
		t.Error("expected false")
	}
}

func TestShouldExitBeforeStart_StopRequested(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "gd-A1", "title": "t", "type": "agent",
			"fields": json.RawMessage(`{"stop_requested":"true"}`),
		})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	t.Setenv("KD_AGENT_ID", "gd-A1")
	if !shouldExitBeforeStart(context.Background()) {
		t.Error("expected true")
	}
}

// ── init_agent.go: pure helpers ──────────────────────────────────────────

func TestEnvDuration(t *testing.T) {
	t.Setenv("MYDUR", "")
	if got := envDuration("MYDUR", 3*time.Second); got != 3*time.Second {
		t.Errorf("default: %v", got)
	}
	t.Setenv("MYDUR", "10")
	if got := envDuration("MYDUR", 3*time.Second); got != 10*time.Second {
		t.Errorf("parsed: %v", got)
	}
	t.Setenv("MYDUR", "not-a-num")
	if got := envDuration("MYDUR", 3*time.Second); got != 3*time.Second {
		t.Errorf("bad: %v", got)
	}
}

// ── agent_start_k8s.go: parseResetDuration ───────────────────────────────

func TestParseResetDuration(t *testing.T) {
	// No "resets " prefix
	if got := parseResetDuration("some error"); got != 0 {
		t.Errorf("no prefix: %v", got)
	}
	// Bad hour
	if got := parseResetDuration("resets 99am"); got != 0 {
		t.Errorf("bad hour: %v", got)
	}
	// No ampm
	if got := parseResetDuration("resets 9"); got != 0 {
		t.Errorf("no ampm: %v", got)
	}
	// Valid am
	if got := parseResetDuration("resets 9am"); got < 0 || got > 24*time.Hour {
		t.Errorf("9am out of range: %v", got)
	}
	// Valid pm
	if got := parseResetDuration("resets 9pm"); got < 0 || got > 24*time.Hour {
		t.Errorf("9pm out of range: %v", got)
	}
	// 12am edge
	if got := parseResetDuration("resets 12am"); got < 0 || got > 24*time.Hour {
		t.Errorf("12am out of range: %v", got)
	}
	// 12pm edge
	if got := parseResetDuration("resets 12pm"); got < 0 || got > 24*time.Hour {
		t.Errorf("12pm out of range: %v", got)
	}
	// Invalid ampm
	if got := parseResetDuration("resets 9xy"); got != 0 {
		t.Errorf("bad ampm: %v", got)
	}
}

func TestRateLimitBackoff(t *testing.T) {
	old := rateLimitBackoffOverride
	rateLimitBackoffOverride = 0
	oldStreak := rateLimitNoHintStreak
	rateLimitNoHintStreak = 0
	t.Cleanup(func() { rateLimitBackoffOverride = old; rateLimitNoHintStreak = oldStreak })

	// Unparseable (no reset hint) → jittered exponential backoff. The first
	// hit must land within [base/2, base] and never exceed the 30m cap.
	if got := rateLimitBackoff("garbage"); got < rateLimitBackoffBase/2 || got > rateLimitBackoffCap {
		t.Errorf("unparseable first hit out of range: %v", got)
	}
	// Override
	rateLimitBackoffOverride = 5 * time.Second
	if got := rateLimitBackoff(""); got != 5*time.Second {
		t.Errorf("override: %v", got)
	}
}
