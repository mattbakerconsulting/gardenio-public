package bridge

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// lastAppendChunks returns the chunksJSON of the most recent appendChunks call,
// or "" if none. Helper for TTFT assertions.
func lastAppendChunks(calls []streamCall) string {
	for i := len(calls) - 1; i >= 0; i-- {
		if calls[i].method == "appendChunks" {
			return calls[i].chunksJSON
		}
	}
	return ""
}

func TestSlackTSToTime(t *testing.T) {
	cases := []struct {
		ts      string
		wantOK  bool
		wantSec int64
	}{
		{"1719331200.123456", true, 1719331200},
		{"1719331200.000000", true, 1719331200},
		{"1719331200", true, 1719331200},
		{"", false, 0},
		{"not-a-number", false, 0},
		{"0", false, 0},
		{"-5", false, 0},
	}
	for _, c := range cases {
		got, ok := slackTSToTime(c.ts)
		if ok != c.wantOK {
			t.Errorf("slackTSToTime(%q) ok = %v, want %v", c.ts, ok, c.wantOK)
			continue
		}
		if ok && got.Unix() != c.wantSec {
			t.Errorf("slackTSToTime(%q) sec = %d, want %d", c.ts, got.Unix(), c.wantSec)
		}
	}
	// Microsecond precision is preserved.
	got, ok := slackTSToTime("1719331200.500000")
	if !ok || got.Nanosecond() < 499_000_000 || got.Nanosecond() > 501_000_000 {
		t.Errorf("slackTSToTime micro precision: ok=%v nsec=%d", ok, got.Nanosecond())
	}
}

func TestFormatTTFT(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{2300 * time.Millisecond, "2.3s"},
		{500 * time.Millisecond, "0.5s"},
		{9900 * time.Millisecond, "9.9s"},
		{10 * time.Second, "10s"},
		{47200 * time.Millisecond, "47s"},
		{90500 * time.Millisecond, "91s"},
		{-1 * time.Second, "0.0s"},
	}
	for _, c := range cases {
		if got := formatTTFT(c.d); got != c.want {
			t.Errorf("formatTTFT(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

// TestTTFTOnFirstAssistantText verifies the first assistant_text message after a
// known trigger send-time carries a "ttft:" title, and subsequent messages do not.
func TestTTFTOnFirstAssistantText(t *testing.T) {
	mock := &mockStreamClient{}
	mgr := NewAgentStreamManager(mock, testLogger())
	ctx := context.Background()

	// Trigger sent 3s ago; session starts now.
	mgr.MarkTriggerSent("worker-1", time.Now().Add(-3*time.Second))
	mgr.StartSession(ctx, "worker-1", "C123", "thread-ts-1", "U12345")

	// First assistant message → should carry ttft.
	mgr.HandleMessageEvent("worker-1", &messageEvent{EventType: "assistant_text", Text: "Hello, working on it."})
	first := lastAppendChunks(mock.getCalls())
	if !strings.Contains(first, "ttft:") {
		t.Fatalf("first assistant_text should contain ttft: title, got %s", first)
	}

	// Second assistant message → must NOT repeat ttft.
	mgr.HandleMessageEvent("worker-1", &messageEvent{EventType: "assistant_text", Text: "Still going."})
	second := lastAppendChunks(mock.getCalls())
	if strings.Contains(second, "ttft:") {
		t.Errorf("second assistant_text should not contain ttft:, got %s", second)
	}
	if !strings.Contains(second, "Generating response") {
		t.Errorf("second assistant_text should use default title, got %s", second)
	}
}

// TestTTFTNoTriggerTime verifies that without a known trigger time, no ttft is
// shown and the default title is used — but the first-message latch still fires.
func TestTTFTNoTriggerTime(t *testing.T) {
	mock := &mockStreamClient{}
	mgr := NewAgentStreamManager(mock, testLogger())
	ctx := context.Background()

	mgr.StartSession(ctx, "worker-1", "C123", "thread-ts-1", "U12345")
	mgr.HandleMessageEvent("worker-1", &messageEvent{EventType: "assistant_text", Text: "Hi."})
	got := lastAppendChunks(mock.getCalls())
	if strings.Contains(got, "ttft:") {
		t.Errorf("no trigger time should yield no ttft, got %s", got)
	}
	if !strings.Contains(got, "Generating response") {
		t.Errorf("expected default title, got %s", got)
	}
}

// TestMarkTriggerSentBeforeSession verifies the buffered trigger time is drained
// into a session created afterward (prewarmed/cold-respawn ordering).
func TestMarkTriggerSentBeforeSession(t *testing.T) {
	mock := &mockStreamClient{}
	mgr := NewAgentStreamManager(mock, testLogger())
	ctx := context.Background()

	// Stamp BEFORE the session exists (e.g. cold respawn).
	mgr.MarkTriggerSent("worker-1", time.Now().Add(-2*time.Second))
	mgr.StartSession(ctx, "worker-1", "C123", "thread-ts-1", "U12345")
	mgr.HandleMessageEvent("worker-1", &messageEvent{EventType: "assistant_text", Text: "Resumed."})

	got := lastAppendChunks(mock.getCalls())
	if !strings.Contains(got, "ttft:") {
		t.Errorf("buffered trigger time should produce ttft on first message, got %s", got)
	}
}

// TestMarkTriggerSentKeepsEarliest verifies a burst of follow-ups keeps the
// earliest send-time as the anchor (the message that actually woke the agent).
func TestMarkTriggerSentKeepsEarliest(t *testing.T) {
	mock := &mockStreamClient{}
	mgr := NewAgentStreamManager(mock, testLogger())
	ctx := context.Background()

	mgr.StartSession(ctx, "worker-1", "C123", "thread-ts-1", "U12345")
	early := time.Now().Add(-5 * time.Second)
	late := time.Now().Add(-1 * time.Second)
	mgr.MarkTriggerSent("worker-1", late)
	mgr.MarkTriggerSent("worker-1", early) // earlier — should win

	got := mgr.consumeTTFT("worker-1")
	// Earliest anchor (5s) ⇒ "5s" (>= 10s? no, 5s ⇒ one decimal "5.0s").
	if !strings.HasPrefix(got, "5") {
		t.Errorf("expected ttft anchored on earliest (~5s), got %q", got)
	}
}

// TestConsumeTTFTLatches verifies consumeTTFT only ever returns a value once.
func TestConsumeTTFTLatches(t *testing.T) {
	mock := &mockStreamClient{}
	mgr := NewAgentStreamManager(mock, testLogger())
	ctx := context.Background()

	mgr.MarkTriggerSent("worker-1", time.Now().Add(-2*time.Second))
	mgr.StartSession(ctx, "worker-1", "C123", "thread-ts-1", "U12345")

	if got := mgr.consumeTTFT("worker-1"); got == "" {
		t.Fatal("first consumeTTFT should return a value")
	}
	if got := mgr.consumeTTFT("worker-1"); got != "" {
		t.Errorf("second consumeTTFT should be empty (latched), got %q", got)
	}
}

// TestMarkTriggerSentZeroIgnored verifies a zero time is a no-op.
func TestMarkTriggerSentZeroIgnored(t *testing.T) {
	mock := &mockStreamClient{}
	mgr := NewAgentStreamManager(mock, testLogger())
	ctx := context.Background()

	mgr.MarkTriggerSent("worker-1", time.Time{})
	mgr.StartSession(ctx, "worker-1", "C123", "thread-ts-1", "U12345")
	mgr.HandleMessageEvent("worker-1", &messageEvent{EventType: "assistant_text", Text: "Hi."})
	got := lastAppendChunks(mock.getCalls())
	if strings.Contains(got, "ttft:") {
		t.Errorf("zero trigger time should be ignored, got %s", got)
	}
}

// TestTTFTEndToEndViaSlackTS exercises the full parse→stamp→render path using a
// realistic Slack ts string a few seconds in the past.
func TestTTFTEndToEndViaSlackTS(t *testing.T) {
	mock := &mockStreamClient{}
	mgr := NewAgentStreamManager(mock, testLogger())
	ctx := context.Background()

	sentTS := fmt.Sprintf("%d.000000", time.Now().Add(-4*time.Second).Unix())
	if at, ok := slackTSToTime(sentTS); ok {
		mgr.MarkTriggerSent("worker-1", at)
	} else {
		t.Fatalf("slackTSToTime(%q) failed to parse", sentTS)
	}
	mgr.StartSession(ctx, "worker-1", "C123", "thread-ts-1", "U12345")
	mgr.HandleMessageEvent("worker-1", &messageEvent{EventType: "assistant_text", Text: "On it."})

	got := lastAppendChunks(mock.getCalls())
	if !strings.Contains(got, "ttft:") {
		t.Errorf("end-to-end ttft should render, got %s", got)
	}
}
