package bridge

import (
	"context"
	"sync"
	"testing"
	"time"
)

// fakeTTFTRecorder captures RecordTTFT calls for assertions.
type fakeTTFTRecorder struct {
	mu    sync.Mutex
	calls []ttftCall
}

type ttftCall struct {
	agent  string
	ttftMs int64
}

func (f *fakeTTFTRecorder) RecordTTFT(_ context.Context, agentName string, ttftMs int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, ttftCall{agent: agentName, ttftMs: ttftMs})
}

func (f *fakeTTFTRecorder) getCalls() []ttftCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]ttftCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// waitForTTFTCalls polls until the recorder has at least n calls or the deadline
// passes. RecordTTFT is invoked from a goroutine off the event path, so tests
// must wait for it rather than read immediately.
func waitForTTFTCalls(rec *fakeTTFTRecorder, n int) []ttftCall {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if calls := rec.getCalls(); len(calls) >= n {
			return calls
		}
		time.Sleep(2 * time.Millisecond)
	}
	return rec.getCalls()
}

// TestBootTTFTRecordedOnFirstAssistantText verifies the boot→first-token latency
// is recorded once, anchored on SessionStart (liveAt), on the first
// assistant_text event — and is NOT re-recorded on later messages.
func TestBootTTFTRecordedOnFirstAssistantText(t *testing.T) {
	mock := &mockStreamClient{}
	mgr := NewAgentStreamManager(mock, testLogger())
	rec := &fakeTTFTRecorder{}
	mgr.SetTTFTRecorder(rec)
	ctx := context.Background()

	mgr.StartSession(ctx, "worker-1", "C123", "thread-ts-1", "U12345")
	// SessionStart proves life; backdate liveAt so the delta is a real positive.
	mgr.HandleSessionStart(ctx, "worker-1", "startup")
	mgr.mu.Lock()
	mgr.sessions["worker-1"].liveAt = time.Now().Add(-2 * time.Second)
	mgr.mu.Unlock()

	mgr.HandleMessageEvent("worker-1", &messageEvent{EventType: "assistant_text", Text: "Hello."})
	calls := waitForTTFTCalls(rec, 1)
	if len(calls) != 1 {
		t.Fatalf("expected exactly 1 RecordTTFT call, got %d", len(calls))
	}
	if calls[0].agent != "worker-1" {
		t.Errorf("recorded agent = %q, want worker-1", calls[0].agent)
	}
	// ~2s ⇒ ~2000ms; allow slack for test scheduling.
	if calls[0].ttftMs < 1500 || calls[0].ttftMs > 5000 {
		t.Errorf("ttftMs = %d, want ~2000 (1500..5000)", calls[0].ttftMs)
	}

	// A second assistant message must NOT record TTFT again (latched).
	mgr.HandleMessageEvent("worker-1", &messageEvent{EventType: "assistant_text", Text: "Still going."})
	// Give any (erroneous) goroutine a chance to fire before asserting no growth.
	time.Sleep(50 * time.Millisecond)
	if got := rec.getCalls(); len(got) != 1 {
		t.Errorf("second assistant_text must not re-record TTFT, got %d calls", len(got))
	}
}

// TestBootTTFTNoSessionStart verifies that without a SessionStart (liveAt zero)
// no TTFT is recorded — but the latch still fires so a later message is never
// mis-recorded as the first.
func TestBootTTFTNoSessionStart(t *testing.T) {
	mock := &mockStreamClient{}
	mgr := NewAgentStreamManager(mock, testLogger())
	rec := &fakeTTFTRecorder{}
	mgr.SetTTFTRecorder(rec)
	ctx := context.Background()

	mgr.StartSession(ctx, "worker-1", "C123", "thread-ts-1", "U12345")
	// No HandleSessionStart → liveAt stays zero.
	mgr.HandleMessageEvent("worker-1", &messageEvent{EventType: "assistant_text", Text: "Hi."})
	time.Sleep(50 * time.Millisecond)
	if got := rec.getCalls(); len(got) != 0 {
		t.Errorf("no SessionStart should record nothing, got %d calls", len(got))
	}
	// Latch fired: even if SessionStart lands late, the second message is not the first.
	if _, ok := mgr.consumeBootTTFT("worker-1"); ok {
		t.Errorf("consumeBootTTFT should be latched after the first assistant_text")
	}
}

// TestBootTTFTSeededFromPendingLive verifies the SessionStart instant buffered
// in pendingLive (hook beats session registration — prewarmed assign / cold
// respawn) seeds liveAt when the session is later created, so the boot→first
// -token measurement is still anchored on real proof-of-life.
func TestBootTTFTSeededFromPendingLive(t *testing.T) {
	mock := &mockStreamClient{}
	mgr := NewAgentStreamManager(mock, testLogger())
	rec := &fakeTTFTRecorder{}
	mgr.SetTTFTRecorder(rec)
	ctx := context.Background()

	// SessionStart fires BEFORE the session exists → buffered in pendingLive.
	mgr.HandleSessionStart(ctx, "worker-1", "startup")
	// Backdate the buffered instant so the eventual delta is a real positive.
	mgr.mu.Lock()
	mgr.pendingLive["worker-1"] = time.Now().Add(-3 * time.Second)
	mgr.mu.Unlock()

	mgr.StartSession(ctx, "worker-1", "C123", "thread-ts-1", "U12345")
	// Session should be born live with liveAt seeded from the buffered instant.
	mgr.mu.Lock()
	s := mgr.sessions["worker-1"]
	liveAtSet := !s.liveAt.IsZero()
	mgr.mu.Unlock()
	if !liveAtSet {
		t.Fatal("liveAt should be seeded from the buffered SessionStart instant")
	}

	mgr.HandleMessageEvent("worker-1", &messageEvent{EventType: "assistant_text", Text: "Resumed."})
	calls := waitForTTFTCalls(rec, 1)
	if len(calls) != 1 {
		t.Fatalf("expected 1 RecordTTFT call, got %d", len(calls))
	}
	if calls[0].ttftMs < 2500 || calls[0].ttftMs > 6000 {
		t.Errorf("ttftMs = %d, want ~3000 (2500..6000)", calls[0].ttftMs)
	}
}

// TestBootTTFTNilRecorderNoPanic verifies the assistant_text path is a safe
// no-op when no recorder is wired.
func TestBootTTFTNilRecorderNoPanic(t *testing.T) {
	mock := &mockStreamClient{}
	mgr := NewAgentStreamManager(mock, testLogger())
	ctx := context.Background()

	mgr.StartSession(ctx, "worker-1", "C123", "thread-ts-1", "U12345")
	mgr.HandleSessionStart(ctx, "worker-1", "startup")
	// No SetTTFTRecorder — must not panic and must not consume the latch.
	mgr.HandleMessageEvent("worker-1", &messageEvent{EventType: "assistant_text", Text: "Hi."})
	if mgr.ttftRecorder != nil {
		t.Fatal("recorder should be nil in this test")
	}
}

// TestConsumeBootTTFTLatches verifies consumeBootTTFT only ever returns a value
// once per session.
func TestConsumeBootTTFTLatches(t *testing.T) {
	mock := &mockStreamClient{}
	mgr := NewAgentStreamManager(mock, testLogger())
	ctx := context.Background()

	mgr.StartSession(ctx, "worker-1", "C123", "thread-ts-1", "U12345")
	mgr.HandleSessionStart(ctx, "worker-1", "startup")
	mgr.mu.Lock()
	mgr.sessions["worker-1"].liveAt = time.Now().Add(-1 * time.Second)
	mgr.mu.Unlock()

	if _, ok := mgr.consumeBootTTFT("worker-1"); !ok {
		t.Fatal("first consumeBootTTFT should return a value")
	}
	if _, ok := mgr.consumeBootTTFT("worker-1"); ok {
		t.Error("second consumeBootTTFT should be latched (no value)")
	}
}

// TestConsumeBootTTFTUnknownSession verifies a missing session is a safe no-op.
func TestConsumeBootTTFTUnknownSession(t *testing.T) {
	mock := &mockStreamClient{}
	mgr := NewAgentStreamManager(mock, testLogger())
	if _, ok := mgr.consumeBootTTFT("ghost"); ok {
		t.Error("unknown session should yield no value")
	}
}
