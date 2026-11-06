package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestTailBuffer_RetainsLastN(t *testing.T) {
	tb := newTailBuffer(10)

	// Below cap: keep all bytes.
	n, err := tb.Write([]byte("abc"))
	if err != nil || n != 3 {
		t.Fatalf("Write returned (%d, %v)", n, err)
	}
	if got := string(tb.Snapshot()); got != "abc" {
		t.Errorf("after small write: %q", got)
	}
	if tb.Len() != 3 {
		t.Errorf("Len=%d, want 3", tb.Len())
	}

	// Cross the cap: trim to last 10 bytes.
	if _, err := tb.Write([]byte("0123456789XYZ")); err != nil {
		t.Fatal(err)
	}
	if got := string(tb.Snapshot()); got != "3456789XYZ" {
		t.Errorf("after overflow: %q", got)
	}
	if tb.Len() != 10 {
		t.Errorf("Len=%d, want 10", tb.Len())
	}
}

func TestTailBuffer_SingleHugeWrite(t *testing.T) {
	tb := newTailBuffer(8)
	// Single write larger than max: should keep only the trailing 8 bytes.
	if _, err := tb.Write([]byte("abcdefghijklmnop")); err != nil {
		t.Fatal(err)
	}
	if got := string(tb.Snapshot()); got != "ijklmnop" {
		t.Errorf("got %q, want ijklmnop", got)
	}
}

func TestTailBuffer_Reset(t *testing.T) {
	tb := newTailBuffer(16)
	_, _ = tb.Write([]byte("hello"))
	tb.Reset()
	if tb.Len() != 0 {
		t.Errorf("after Reset, Len=%d", tb.Len())
	}
	_, _ = tb.Write([]byte("world"))
	if got := string(tb.Snapshot()); got != "world" {
		t.Errorf("after reset+write, got %q", got)
	}
}

func TestTailBuffer_SnapshotIsACopy(t *testing.T) {
	tb := newTailBuffer(16)
	_, _ = tb.Write([]byte("abc"))
	snap := tb.Snapshot()
	snap[0] = 'X'
	if got := string(tb.Snapshot()); got != "abc" {
		t.Errorf("Snapshot mutation leaked into buffer: %q", got)
	}
}

// fakeClock returns scripted timestamps.
type fakeClock struct {
	t time.Time
}

func (f *fakeClock) advance(d time.Duration) { f.t = f.t.Add(d) }
func (f *fakeClock) now() time.Time           { return f.t }

func TestCrashTracker_TriggersOnThirdSubWindowExit(t *testing.T) {
	clk := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	tr := newCrashTracker(3, 30*time.Second, 5*time.Minute)
	tr.now = clk.now

	if tr.Record(2 * time.Second) {
		t.Fatal("first sub-window exit should not trigger")
	}
	clk.advance(3 * time.Second)
	if tr.Record(5 * time.Second) {
		t.Fatal("second sub-window exit should not trigger")
	}
	clk.advance(3 * time.Second)
	if !tr.Record(1 * time.Second) {
		t.Fatal("third sub-window exit should trigger")
	}
}

func TestCrashTracker_LongRunResetsStreak(t *testing.T) {
	clk := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	tr := newCrashTracker(3, 30*time.Second, 5*time.Minute)
	tr.now = clk.now

	tr.Record(2 * time.Second)
	tr.Record(2 * time.Second)
	// Long-running session resets the streak.
	if tr.Record(2 * time.Minute) {
		t.Fatal("a long-running session should not trigger")
	}
	// Need three more sub-window exits before triggering again.
	if tr.Record(1 * time.Second) {
		t.Fatal("streak should have been reset by the long session")
	}
	if tr.Record(1 * time.Second) {
		t.Fatal("streak should have been reset by the long session")
	}
	if !tr.Record(1 * time.Second) {
		t.Fatal("third post-reset sub-window exit should trigger")
	}
}

func TestCrashTracker_PrunesOutsideLookback(t *testing.T) {
	clk := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	tr := newCrashTracker(3, 30*time.Second, 5*time.Minute)
	tr.now = clk.now

	// Two old crashes drop out of the lookback window before the next one.
	tr.Record(2 * time.Second)
	clk.advance(3 * time.Minute)
	tr.Record(2 * time.Second)
	clk.advance(3 * time.Minute) // now > 5 min after first crash
	// First crash is outside lookback; this is only the 2nd inside the window.
	if tr.Record(2 * time.Second) {
		t.Fatal("should not trigger — only 2 crashes inside the lookback window")
	}
}

func TestCrashTracker_ResetsAfterTrigger(t *testing.T) {
	clk := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	tr := newCrashTracker(3, 30*time.Second, 5*time.Minute)
	tr.now = clk.now

	tr.Record(1 * time.Second)
	tr.Record(1 * time.Second)
	if !tr.Record(1 * time.Second) {
		t.Fatal("third sub-window exit should trigger")
	}
	// One more sub-window exit immediately after should not re-trigger:
	// the burst was already reported.
	if tr.Record(1 * time.Second) {
		t.Fatal("tracker should have reset after triggering")
	}
}

func TestCrashTrackerMatchesProductionConstants(t *testing.T) {
	if fastCrashThreshold != 3 {
		t.Errorf("fastCrashThreshold = %d, want 3", fastCrashThreshold)
	}
	if fastCrashWindow != 30*time.Second {
		t.Errorf("fastCrashWindow = %v, want 30s", fastCrashWindow)
	}
	if stdoutTailBytes != 256*1024 {
		t.Errorf("stdoutTailBytes = %d, want 256 KiB", stdoutTailBytes)
	}
}

func TestDerefSessionID(t *testing.T) {
	var p atomic.Pointer[string]
	if got := derefSessionID(&p); got != "" {
		t.Errorf("nil pointer should yield empty: %q", got)
	}
	if got := derefSessionID(nil); got != "" {
		t.Errorf("nil receiver should yield empty: %q", got)
	}
	s := "abc"
	p.Store(&s)
	if got := derefSessionID(&p); got != "abc" {
		t.Errorf("got %q, want abc", got)
	}
}

func TestUploadStdoutTail_NoOpWhenUnconfigured(t *testing.T) {
	// No COOP_S3_BUCKET set → quiet no-op (no error, no upload).
	t.Setenv("COOP_S3_BUCKET", "")
	uploaded, err := uploadStdoutTail(t.Context(), "abc-123", []byte("hello"), "fast_crash")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if uploaded {
		t.Fatal("should not upload when COOP_S3_BUCKET is unset")
	}
}

func TestUploadStdoutTail_NoOpWhenSessionIDEmpty(t *testing.T) {
	t.Setenv("COOP_S3_BUCKET", "test-bucket")
	uploaded, err := uploadStdoutTail(t.Context(), "", []byte("hello"), "fast_crash")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if uploaded {
		t.Fatal("should not upload with empty session ID")
	}
}

func TestUploadStdoutTail_NoOpWhenTailEmpty(t *testing.T) {
	t.Setenv("COOP_S3_BUCKET", "test-bucket")
	uploaded, err := uploadStdoutTail(t.Context(), "abc-123", nil, "fast_crash")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if uploaded {
		t.Fatal("should not upload with empty tail")
	}
}

func TestStdoutTailKey(t *testing.T) {
	cases := []struct {
		name        string
		prefix      string
		agentBeadID string
		sessionID   string
		want        string
	}{
		{
			name:        "with agent bead id pins under <bead>/<session>/",
			prefix:      "coop",
			agentBeadID: "gd-abc123",
			sessionID:   "11111111-2222-3333-4444-555555555555",
			want:        "coop/gd-abc123/11111111-2222-3333-4444-555555555555/stdout-tail.txt",
		},
		{
			name:        "no agent bead id falls back to flat layout",
			prefix:      "coop",
			agentBeadID: "",
			sessionID:   "11111111-2222-3333-4444-555555555555",
			want:        "coop/11111111-2222-3333-4444-555555555555/stdout-tail.txt",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stdoutTailKey(tc.prefix, tc.agentBeadID, tc.sessionID); got != tc.want {
				t.Errorf("stdoutTailKey(%q,%q,%q) = %q, want %q",
					tc.prefix, tc.agentBeadID, tc.sessionID, got, tc.want)
			}
		})
	}
}

// Smoke check: TailBuffer absorbs a write that is exactly cap bytes.
func TestTailBuffer_ExactlyMaxWrite(t *testing.T) {
	tb := newTailBuffer(4)
	_, _ = tb.Write([]byte("abcd"))
	if got := string(tb.Snapshot()); got != "abcd" {
		t.Errorf("got %q, want abcd", got)
	}
	// Adding one more byte should slide the window.
	_, _ = tb.Write([]byte("e"))
	if got := string(tb.Snapshot()); got != "bcde" {
		t.Errorf("got %q, want bcde", got)
	}
}

// Loosely models heavy stdout volume — mostly to make sure the buffer never
// grows past max under realistic usage.
func TestTailBuffer_BoundedUnderManyWrites(t *testing.T) {
	tb := newTailBuffer(1024)
	chunk := strings.Repeat("x", 500)
	for range 50 {
		_, _ = tb.Write([]byte(chunk))
	}
	if l := tb.Len(); l > 1024 {
		t.Errorf("buffer exceeded max: %d > 1024", l)
	}
}

func TestTailReasonFromCloseReason(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"stop_gate_cooldown_involuntary: claude exited mid-work without gb done", "stop_gate_cooldown_involuntary"},
		{"stop_gate_rate_limited: claude exited without gb done", "stop_gate_rate_limited"},
		{"idle_ttl_expired", "idle_ttl_expired"},
		{"  spaced : detail ", "spaced"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := tailReasonFromCloseReason(tc.in); got != tc.want {
			t.Errorf("tailReasonFromCloseReason(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// stubClosedBead spins up a daemon stub whose GetBead returns a single bead with
// the given status/fields, points the package daemon at it, and sets KD_AGENT_ID.
func stubClosedBead(t *testing.T, status string, fields map[string]string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":     "gd-A1",
			"title":  "test agent",
			"status": status,
			"fields": fields,
		})
	}))
	t.Cleanup(srv.Close)
	swapDaemon(t, srv.URL)
	t.Setenv("KD_AGENT_ID", "gd-A1")
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
}

func TestAgentBeadClosedOutOfProcess_TerminalDeath(t *testing.T) {
	stubClosedBead(t, "closed", map[string]string{
		"agent_state":  "done",
		"close_reason": "stop_gate_cooldown_involuntary: claude exited mid-work without gb done",
	})
	reason, closed := agentBeadClosedOutOfProcess(context.Background())
	if !closed {
		t.Fatal("expected a terminally-closed bead to be detected")
	}
	if reason != "stop_gate_cooldown_involuntary: claude exited mid-work without gb done" {
		t.Errorf("unexpected close_reason: %q", reason)
	}
}

func TestAgentBeadClosedOutOfProcess_StillOpen(t *testing.T) {
	stubClosedBead(t, "in_progress", map[string]string{"agent_state": "working"})
	if _, closed := agentBeadClosedOutOfProcess(context.Background()); closed {
		t.Fatal("an open bead must not be treated as an out-of-process close")
	}
}

func TestAgentBeadClosedOutOfProcess_CleanStopNoReason(t *testing.T) {
	// A clean `gb stop` / `gb done` closes with agent_state=done and NO
	// close_reason — that is not a death and must not ship a tail here.
	stubClosedBead(t, "closed", map[string]string{"agent_state": "done"})
	if _, closed := agentBeadClosedOutOfProcess(context.Background()); closed {
		t.Fatal("a clean self-close (empty close_reason) must not short-circuit the loop")
	}
}

func TestAgentBeadClosedOutOfProcess_NoAgentID(t *testing.T) {
	t.Setenv("KD_AGENT_ID", "")
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	if _, closed := agentBeadClosedOutOfProcess(context.Background()); closed {
		t.Fatal("no agent bead in scope must yield not-closed")
	}
}

func TestAgentBeadClosedOutOfProcess_LookupError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	swapDaemon(t, srv.URL)
	t.Setenv("KD_AGENT_ID", "gd-A1")
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	if _, closed := agentBeadClosedOutOfProcess(context.Background()); closed {
		t.Fatal("a daemon lookup error must fail closed (not treated as a death)")
	}
}
