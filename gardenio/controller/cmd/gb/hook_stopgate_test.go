package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// overrideStopGateCooldownFile sets the cooldown file to a test path.
func overrideStopGateCooldownFile(path string) {
	stopGateCooldownFile = path
}

// restoreStopGateCooldownFile restores the original cooldown file path.
func restoreStopGateCooldownFile(original string) {
	stopGateCooldownFile = original
}

func TestStopGateInCooldown_NoFile(t *testing.T) {
	// Ensure the cooldown file doesn't exist.
	old := stopGateCooldownFile
	t.Cleanup(func() { restoreStopGateCooldownFile(old) })
	overrideStopGateCooldownFile(filepath.Join(t.TempDir(), "no-such-file"))

	if stopGateInCooldown() {
		t.Error("stopGateInCooldown() = true, want false when file does not exist")
	}
}

func TestStopGateInCooldown_RecentBlock(t *testing.T) {
	tmp := t.TempDir()
	f := filepath.Join(tmp, "last-block")
	old := stopGateCooldownFile
	t.Cleanup(func() { restoreStopGateCooldownFile(old) })
	overrideStopGateCooldownFile(f)

	// Write a recent timestamp (now).
	if err := os.WriteFile(f, []byte(fmt.Sprintf("%d\n", time.Now().Unix())), 0o644); err != nil {
		t.Fatal(err)
	}

	if !stopGateInCooldown() {
		t.Error("stopGateInCooldown() = false, want true for recent block")
	}
}

func TestStopGateInCooldown_ExpiredBlock(t *testing.T) {
	tmp := t.TempDir()
	f := filepath.Join(tmp, "last-block")
	old := stopGateCooldownFile
	t.Cleanup(func() { restoreStopGateCooldownFile(old) })
	overrideStopGateCooldownFile(f)

	// Write an old timestamp (60 seconds ago, cooldown is 30s by default).
	oldTime := time.Now().Unix() - 60
	if err := os.WriteFile(f, []byte(fmt.Sprintf("%d\n", oldTime)), 0o644); err != nil {
		t.Fatal(err)
	}

	if stopGateInCooldown() {
		t.Error("stopGateInCooldown() = true, want false for expired block")
	}
}

func TestStopGateInCooldown_CustomCooldown(t *testing.T) {
	tmp := t.TempDir()
	f := filepath.Join(tmp, "last-block")
	old := stopGateCooldownFile
	t.Cleanup(func() { restoreStopGateCooldownFile(old) })
	overrideStopGateCooldownFile(f)

	// Block happened 10 seconds ago.
	blockTime := time.Now().Unix() - 10
	if err := os.WriteFile(f, []byte(fmt.Sprintf("%d\n", blockTime)), 0o644); err != nil {
		t.Fatal(err)
	}

	// With default 30s cooldown, should still be in cooldown.
	if !stopGateInCooldown() {
		t.Error("stopGateInCooldown() = false, want true (10s ago, 30s cooldown)")
	}

	// Override cooldown to 5 seconds — should now be expired.
	t.Setenv("STOP_GATE_COOLDOWN_SECS", "5")
	if stopGateInCooldown() {
		t.Error("stopGateInCooldown() = true, want false (10s ago, 5s cooldown)")
	}
}

func TestRecordAndClearStopGateBlock(t *testing.T) {
	tmp := t.TempDir()
	f := filepath.Join(tmp, "last-block")
	old := stopGateCooldownFile
	t.Cleanup(func() { restoreStopGateCooldownFile(old) })
	overrideStopGateCooldownFile(f)

	// Initially no file.
	if _, err := os.Stat(f); err == nil {
		t.Fatal("cooldown file should not exist initially")
	}

	// Record a block.
	recordStopGateBlock()
	if _, err := os.Stat(f); err != nil {
		t.Fatalf("cooldown file should exist after recording: %v", err)
	}

	// Should be in cooldown now.
	if !stopGateInCooldown() {
		t.Error("should be in cooldown after recording block")
	}

	// Clear.
	clearStopGateCooldown()
	if _, err := os.Stat(f); err == nil {
		t.Error("cooldown file should not exist after clearing")
	}
	if stopGateInCooldown() {
		t.Error("should not be in cooldown after clearing")
	}
}

func TestStopGateInCooldown_InvalidContent(t *testing.T) {
	tmp := t.TempDir()
	f := filepath.Join(tmp, "last-block")
	old := stopGateCooldownFile
	t.Cleanup(func() { restoreStopGateCooldownFile(old) })
	overrideStopGateCooldownFile(f)

	// Write garbage.
	if err := os.WriteFile(f, []byte("not-a-number\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if stopGateInCooldown() {
		t.Error("stopGateInCooldown() = true, want false for invalid content")
	}
}

func TestInjectStopGateText_CustomFile(t *testing.T) {
	tmp := t.TempDir()
	claudeDir := filepath.Join(tmp, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	textFile := filepath.Join(claudeDir, "stop-gate-text.md")
	customText := "<system-reminder>Custom stop gate text</system-reminder>\n"
	if err := os.WriteFile(textFile, []byte(customText), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HOME", tmp)

	// Capture stdout.
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	injectStopGateTextTo(func(s string) { fmt.Print(s) })

	w.Close()
	out := make([]byte, 4096)
	n, _ := r.Read(out)
	os.Stdout = oldStdout

	got := string(out[:n])
	if got != customText {
		t.Errorf("injectStopGateTextTo() = %q, want %q", got, customText)
	}
}

func TestInjectStopGateText_Fallback(t *testing.T) {
	// Set HOME to a dir with no .claude/stop-gate-text.md.
	t.Setenv("HOME", t.TempDir())

	// Capture stdout.
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	injectStopGateTextTo(func(s string) { fmt.Print(s) })

	w.Close()
	out := make([]byte, 8192)
	n, _ := r.Read(out)
	os.Stdout = oldStdout

	got := string(out[:n])
	if got == "" {
		t.Error("injectStopGateTextTo() should produce fallback text")
	}
	if len(got) < 50 {
		t.Errorf("fallback text too short: %q", got)
	}
}

// TestSynthesizeFallbackWrapUp_EmptyWrapUp covers the core ka-0000000101 fix:
// when a thread agent is closed by the stop gate without ever submitting a
// wrap-up, closeAgentBeadOnExit synthesizes a minimal wrap-up whose tldr is
// derived from the close reason, so the Slack agent card gets a headline
// instead of rendering metadata-only.
func TestSynthesizeFallbackWrapUp_EmptyWrapUp(t *testing.T) {
	bead := &beadsapi.BeadDetail{
		Title:  "thread-agent identity",
		Fields: map[string]string{"agent": "gardenio-thread-foo-abcd"},
	}
	out := synthesizeFallbackWrapUp("cooldown", "stop_gate_cooldown: claude exited without gb done", bead)
	if out == "" {
		t.Fatal("synthesizeFallbackWrapUp returned empty for an empty-wrapup bead")
	}

	var wu WrapUp
	if err := json.Unmarshal([]byte(out), &wu); err != nil {
		t.Fatalf("synthesized wrap-up is not valid JSON: %v\n%s", err, out)
	}
	if strings.TrimSpace(wu.TLDR) == "" {
		t.Errorf("synthesized wrap-up has empty tldr: %s", out)
	}
	// The tldr should carry both the idle-out phrasing and the agent identity.
	if !strings.Contains(wu.TLDR, "idled out") {
		t.Errorf("tldr should reflect the cooldown reason, got: %q", wu.TLDR)
	}
	if !strings.Contains(wu.TLDR, "gardenio-thread-foo-abcd") {
		t.Errorf("tldr should include the agent identity, got: %q", wu.TLDR)
	}
	if wu.Custom["synthesized"] != "true" {
		t.Errorf("synthesized wrap-up should be marked synthesized=true, got: %v", wu.Custom)
	}
	if wu.Custom["close_reason"] == "" {
		t.Errorf("synthesized wrap-up should preserve close_reason, got: %v", wu.Custom)
	}
}

// TestStopGateFallbackTLDR_AllReasons verifies every stop-gate / teardown
// reason maps to a non-empty, distinct tldr — including context_cancelled,
// which the bug called out as a crash path that must also yield a tldr.
func TestStopGateFallbackTLDR_AllReasons(t *testing.T) {
	for _, reason := range []string{"cooldown", "escape_hatch", "rate_limited", "context_cancelled", "stop_requested", "idle_ttl_expired", "something_unexpected"} {
		got := stopGateFallbackTLDR(reason, 0)
		if strings.TrimSpace(got) == "" {
			t.Errorf("stopGateFallbackTLDR(%q) returned empty", reason)
		}
	}
	// The Fix A close paths (ka-0000007639) flow these reasons through the
	// shared helper — they must produce reason-specific tldrs.
	if sr := stopGateFallbackTLDR("stop_requested", 0); !strings.Contains(sr, "requested stop") {
		t.Errorf("stop_requested tldr should mention a requested stop, got: %q", sr)
	}
	if it := stopGateFallbackTLDR("idle_ttl_expired", 0); !strings.Contains(it, "idled out") {
		t.Errorf("idle_ttl_expired tldr should mention idling out, got: %q", it)
	}
	// context_cancelled must produce a meaningful, reason-specific tldr.
	if cc := stopGateFallbackTLDR("context_cancelled", 0); !strings.Contains(cc, "terminated") {
		t.Errorf("context_cancelled tldr should mention termination, got: %q", cc)
	}
}

// TestStopGateFallbackTLDR_HonestWhenWorkDone verifies that when the agent made
// tool calls before the turn ended without `gb stop`, the cooldown/escape_hatch
// fallback tldr reports the work honestly instead of claiming the agent "idled
// out" (ka-0000000083). A genuine zero-work idle must still read as idled out.
func TestStopGateFallbackTLDR_HonestWhenWorkDone(t *testing.T) {
	// Work was done → honest "after N tool calls" headline, no "idled out".
	got := stopGateFallbackTLDR("cooldown", 49)
	if strings.Contains(got, "idled out") {
		t.Errorf("cooldown tldr with work done must not claim idled out, got: %q", got)
	}
	if !strings.Contains(got, "49 tool calls") {
		t.Errorf("cooldown tldr with work done should report the tool-call count, got: %q", got)
	}
	if !strings.Contains(got, "without an explicit wrap-up") {
		t.Errorf("cooldown tldr should still note the missing wrap-up, got: %q", got)
	}

	// Escape hatch with work done is likewise honest.
	if eh := stopGateFallbackTLDR("escape_hatch", 12); strings.Contains(eh, "idled out") || !strings.Contains(eh, "12 tool calls") {
		t.Errorf("escape_hatch tldr with work done should report tool calls and not idle, got: %q", eh)
	}

	// Singular grammar for exactly one tool call.
	if one := stopGateFallbackTLDR("cooldown", 1); !strings.Contains(one, "1 tool call") || strings.Contains(one, "1 tool calls") {
		t.Errorf("cooldown tldr should use singular 'tool call' for n=1, got: %q", one)
	}

	// Zero work → original idled-out phrasing is preserved.
	if zero := stopGateFallbackTLDR("cooldown", 0); !strings.Contains(zero, "idled out") {
		t.Errorf("cooldown tldr with zero tool calls should still read as idled out, got: %q", zero)
	}
}

// TestSynthesizeFallbackWrapUp_HonestWhenWorkDone verifies the synthesized
// wrap-up threads the bead's total_tool_calls into the tldr so an engaged agent
// closed by cooldown is not mislabeled as having idled out (ka-0000000083).
func TestSynthesizeFallbackWrapUp_HonestWhenWorkDone(t *testing.T) {
	bead := &beadsapi.BeadDetail{
		Title:  "first-mate identity",
		Fields: map[string]string{"agent": "gardenio-first-mate-abcd", "total_tool_calls": "49"},
	}
	out := synthesizeFallbackWrapUp("cooldown", "stop_gate_cooldown: claude exited without gb done", bead)
	var wu WrapUp
	if err := json.Unmarshal([]byte(out), &wu); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if strings.Contains(wu.TLDR, "idled out") {
		t.Errorf("tldr for an agent with 49 tool calls must not claim idled out, got: %q", wu.TLDR)
	}
	if !strings.Contains(wu.TLDR, "49 tool calls") {
		t.Errorf("tldr should report the tool-call count, got: %q", wu.TLDR)
	}
	if !strings.Contains(wu.TLDR, "gardenio-first-mate-abcd") {
		t.Errorf("tldr should still include the agent identity, got: %q", wu.TLDR)
	}
}

// TestBeadToolCallCount covers the lenient parse of the total_tool_calls field.
func TestBeadToolCallCount(t *testing.T) {
	cases := []struct {
		raw  string
		want int
	}{
		{"49", 49},
		{"49.0", 49},
		{"0", 0},
		{"", 0},
		{"garbage", 0},
	}
	for _, c := range cases {
		bead := &beadsapi.BeadDetail{Fields: map[string]string{"total_tool_calls": c.raw}}
		if got := beadToolCallCount(bead); got != c.want {
			t.Errorf("beadToolCallCount(%q) = %d, want %d", c.raw, got, c.want)
		}
	}
	if got := beadToolCallCount(nil); got != 0 {
		t.Errorf("beadToolCallCount(nil) = %d, want 0", got)
	}
}

// TestSynthesizeFallbackWrapUp_FallsBackToTitle verifies the tldr still works
// when no agent field is set (it appends the bead title for specificity).
func TestSynthesizeFallbackWrapUp_FallsBackToTitle(t *testing.T) {
	bead := &beadsapi.BeadDetail{Title: "render gardener constellation"}
	out := synthesizeFallbackWrapUp("context_cancelled", "context_cancelled", bead)
	var wu WrapUp
	if err := json.Unmarshal([]byte(out), &wu); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if !strings.Contains(wu.TLDR, "render gardener constellation") {
		t.Errorf("tldr should fall back to bead title, got: %q", wu.TLDR)
	}
}

// TestSynthesizeFallbackWrapUp_ZeroInferencePlatformFailure verifies the fix
// for ka-0000000315: when the bead's usage fields prove the agent never got an
// inference turn (a request was counted but no context reached the model and no
// tool ran — the 2026-07-25 opus-5 outage shape), the synthesized tldr names
// the platform failure instead of the misleading "…without an explicit wrap-up"
// idle phrasing, and the wrap-up is stamped platform_failure=true for the
// render side.
func TestSynthesizeFallbackWrapUp_ZeroInferencePlatformFailure(t *testing.T) {
	bead := &beadsapi.BeadDetail{
		Title: "crew identity",
		Fields: map[string]string{
			"agent":               "gardenio-crew-neveran",
			"total_request_count": "1",
			"total_input_tokens":  "0",
			"total_tool_calls":    "0",
		},
	}
	out := synthesizeFallbackWrapUp("context_cancelled", "context_cancelled", bead)
	var wu WrapUp
	if err := json.Unmarshal([]byte(out), &wu); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if !strings.Contains(wu.TLDR, "Platform failure") || !strings.Contains(wu.TLDR, "never got an inference turn") {
		t.Errorf("tldr should name the platform failure, got: %q", wu.TLDR)
	}
	if strings.Contains(wu.TLDR, "without an explicit wrap-up") {
		t.Errorf("zero-inference tldr must not use the idle/laziness phrasing, got: %q", wu.TLDR)
	}
	if wu.Custom["platform_failure"] != "true" {
		t.Errorf("zero-inference wrap-up should be stamped platform_failure=true, got: %v", wu.Custom)
	}
	// The agent identity is still appended for specificity.
	if !strings.Contains(wu.TLDR, "gardenio-crew-neveran") {
		t.Errorf("tldr should include the agent identity, got: %q", wu.TLDR)
	}
}

// TestBeadZeroInferenceTurn covers the write-side never-started classifier
// across the reported outage shape, a normal run, and absent-usage legacy beads.
func TestBeadZeroInferenceTurn(t *testing.T) {
	cases := []struct {
		name   string
		fields map[string]string
		want   bool
	}{
		{"nil bead", nil, false},
		{"empty fields (legacy bead)", map[string]string{}, false},
		{"reported outage shape", map[string]string{"total_request_count": "1", "total_input_tokens": "0", "total_tool_calls": "0"}, true},
		{"float-formatted counters", map[string]string{"total_request_count": "1.0", "total_input_tokens": "0", "total_tool_calls": "0"}, true},
		{"request with input tokens", map[string]string{"total_request_count": "1", "total_input_tokens": "128", "total_tool_calls": "0"}, false},
		{"request with tool calls", map[string]string{"total_request_count": "1", "total_input_tokens": "0", "total_tool_calls": "2"}, false},
		{"no request counted", map[string]string{"total_input_tokens": "0", "total_tool_calls": "0"}, false},
	}
	for _, tc := range cases {
		var bead *beadsapi.BeadDetail
		if tc.fields != nil {
			bead = &beadsapi.BeadDetail{Fields: tc.fields}
		}
		if got := beadZeroInferenceTurn(bead); got != tc.want {
			t.Errorf("%s: beadZeroInferenceTurn = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestResetStopGateSessionState verifies the ka-0000007830 fix: the per-session
// stop-gate scratch files (cooldown timestamp, block counter, yield marker) are
// all removed by resetStopGateSessionState, so a cold-resumed session cannot
// inherit a stale block count from a previous session that died without
// reaching a gate-clear path. The core regression is the escape-hatch counter:
// a value >= stopGateMaxBlocks persists indefinitely across restarts and would
// otherwise force-exit the very next resumed session on its first turn-end.
func TestResetStopGateSessionState(t *testing.T) {
	tmp := t.TempDir()
	cooldown := filepath.Join(tmp, "last-block")
	blockCount := filepath.Join(tmp, "block-count")
	yield := filepath.Join(tmp, "yielding")

	oldCooldown, oldBlockCount, oldYield := stopGateCooldownFile, stopGateBlockCountFile, stopGateYieldMarkerFile
	t.Cleanup(func() {
		stopGateCooldownFile = oldCooldown
		stopGateBlockCountFile = oldBlockCount
		stopGateYieldMarkerFile = oldYield
	})
	stopGateCooldownFile = cooldown
	stopGateBlockCountFile = blockCount
	stopGateYieldMarkerFile = yield

	// Seed a dirty state: a recent block AND a count at/over the escape-hatch cap.
	if err := os.WriteFile(cooldown, []byte(fmt.Sprintf("%d\n", time.Now().Unix())), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blockCount, []byte(fmt.Sprintf("%d\n", stopGateMaxBlocks)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(yield, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}

	// Sanity: the dirty state trips both the cooldown and the escape hatch.
	if !stopGateInCooldown() {
		t.Fatal("precondition: dirty state should be in cooldown")
	}
	if !stopGateEscapeHatch() {
		t.Fatal("precondition: dirty state should trip the escape hatch")
	}

	resetStopGateSessionState()

	// All three files must be gone.
	for _, f := range []string{cooldown, blockCount, yield} {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Errorf("file %s should be removed after reset, stat err = %v", f, err)
		}
	}
	// And the derived gate predicates must read clean.
	if stopGateInCooldown() {
		t.Error("stopGateInCooldown() should be false after reset")
	}
	if stopGateEscapeHatch() {
		t.Error("stopGateEscapeHatch() should be false after reset")
	}
	if readStopGateBlockCount() != 0 {
		t.Errorf("readStopGateBlockCount() = %d after reset, want 0", readStopGateBlockCount())
	}

	// Idempotent: a second reset on already-clean state is a no-op, not an error.
	resetStopGateSessionState()
}
