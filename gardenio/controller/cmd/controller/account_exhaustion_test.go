package main

import (
	"encoding/json"
	"log/slog"
	"os"
	"testing"
	"time"
)

func testExhaustionLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func TestAccountExhaustionListener_NilWhenNoNats(t *testing.T) {
	l := newAccountExhaustionListener(accountExhaustionConfig{
		NatsURL:  "",
		Failover: &authFailover{},
		Logger:   testExhaustionLogger(),
	})
	if l != nil {
		t.Fatal("expected nil listener when NATS URL is empty")
	}
}

func TestAccountExhaustionListener_NilWhenNoFailover(t *testing.T) {
	l := newAccountExhaustionListener(accountExhaustionConfig{
		NatsURL:  "nats://localhost:4222",
		Failover: nil,
		Logger:   testExhaustionLogger(),
	})
	if l != nil {
		t.Fatal("expected nil listener when failover is nil")
	}
}

func TestAccountExhaustionListener_Debounce(t *testing.T) {
	l := &accountExhaustionListener{
		debounce: 100 * time.Millisecond,
		lastSeen: make(map[string]time.Time),
		logger:   testExhaustionLogger(),
	}

	key := "myproj/myaccount"

	// First call should pass.
	if !l.tryDebounce(key) {
		t.Fatal("first call should not be debounced")
	}

	// Immediate second call should be debounced.
	if l.tryDebounce(key) {
		t.Fatal("immediate second call should be debounced")
	}

	// Wait for debounce window to expire.
	time.Sleep(150 * time.Millisecond)

	// Third call should pass.
	if !l.tryDebounce(key) {
		t.Fatal("call after debounce window should not be debounced")
	}
}

func TestAccountExhaustionListener_DebounceDifferentKeys(t *testing.T) {
	l := &accountExhaustionListener{
		debounce: 100 * time.Millisecond,
		lastSeen: make(map[string]time.Time),
		logger:   testExhaustionLogger(),
	}

	// Different keys should not interfere.
	if !l.tryDebounce("proj-a/acct-1") {
		t.Fatal("first key first call should not be debounced")
	}
	if !l.tryDebounce("proj-b/acct-2") {
		t.Fatal("second key first call should not be debounced")
	}
	if l.tryDebounce("proj-a/acct-1") {
		t.Fatal("first key second call should be debounced")
	}
}

func TestAccountExhaustionEvent_Unmarshal(t *testing.T) {
	raw := `{
		"event": "account_exhausted",
		"session_id": "sess-123",
		"parked_reason": "all profiles rate-limited",
		"resume_at_epoch_ms": 1711000000000,
		"agent": "worker-1",
		"project": "gardenio",
		"account": "claude-max-main"
	}`

	var evt accountExhaustionEvent
	if err := json.Unmarshal([]byte(raw), &evt); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if evt.Event != "account_exhausted" {
		t.Errorf("event = %q, want %q", evt.Event, "account_exhausted")
	}
	if evt.Project != "gardenio" {
		t.Errorf("project = %q, want %q", evt.Project, "gardenio")
	}
	if evt.Account != "claude-max-main" {
		t.Errorf("account = %q, want %q", evt.Account, "claude-max-main")
	}
	if evt.Agent != "worker-1" {
		t.Errorf("agent = %q, want %q", evt.Agent, "worker-1")
	}
	if evt.ResumeAtEpochMS == nil || *evt.ResumeAtEpochMS != 1711000000000 {
		t.Errorf("resume_at_epoch_ms = %v, want 1711000000000", evt.ResumeAtEpochMS)
	}
}

func TestAccountExhaustionListener_DefaultDebounce(t *testing.T) {
	l := newAccountExhaustionListener(accountExhaustionConfig{
		NatsURL:  "nats://localhost:4222",
		Failover: &authFailover{},
		Logger:   testExhaustionLogger(),
	})
	if l == nil {
		t.Fatal("expected non-nil listener")
	}
	if l.debounce != 30*time.Second {
		t.Errorf("default debounce = %v, want 30s", l.debounce)
	}
}
