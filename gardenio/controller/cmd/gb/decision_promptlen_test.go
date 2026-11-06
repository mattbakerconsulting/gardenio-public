package main

import (
	"strings"
	"testing"
)

// TestCheckDecisionPromptLength_OverLimit verifies the pre-check rejects an
// over-length prompt with an actionable message that names --prompt (not the
// server's "title" field) and points at --context (ka-0000006105).
func TestCheckDecisionPromptLength_OverLimit(t *testing.T) {
	longPrompt := strings.Repeat("x", maxDecisionPromptRunes+1)
	err := checkDecisionPromptLength(longPrompt)
	if err == nil {
		t.Fatal("expected an error for an over-length prompt, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "--prompt") {
		t.Errorf("error should name --prompt, got: %s", msg)
	}
	if !strings.Contains(msg, "--context") {
		t.Errorf("error should point at --context for the overflow, got: %s", msg)
	}
	if !strings.Contains(msg, "501") {
		t.Errorf("error should report the actual rune count (501), got: %s", msg)
	}
	// Regression guard: must NOT surface the raw server phrasing that names a
	// "title" field the agent never set.
	if strings.Contains(msg, "must be 500 characters or fewer") {
		t.Errorf("client pre-check should replace the raw server 'title' message, got: %s", msg)
	}
}

// TestCheckDecisionPromptLength_AtLimit confirms a prompt of exactly the limit
// passes (boundary).
func TestCheckDecisionPromptLength_AtLimit(t *testing.T) {
	if err := checkDecisionPromptLength(strings.Repeat("x", maxDecisionPromptRunes)); err != nil {
		t.Errorf("prompt at exactly the limit should pass, got: %v", err)
	}
}

// TestCheckDecisionPromptLength_Empty confirms an empty prompt passes the length
// check (the empty-prompt case is handled by a separate required-flag guard).
func TestCheckDecisionPromptLength_Empty(t *testing.T) {
	if err := checkDecisionPromptLength(""); err != nil {
		t.Errorf("empty prompt should pass the length check, got: %v", err)
	}
}

// TestCheckDecisionPromptLength_CountsRunesNotBytes verifies the check uses rune
// count (matching the server's len([]rune(...)) rule): 500 three-byte runes
// (1500 bytes) is accepted, and 501 runes is rejected.
func TestCheckDecisionPromptLength_CountsRunesNotBytes(t *testing.T) {
	atLimit := strings.Repeat("あ", maxDecisionPromptRunes) // 500 runes, 1500 bytes
	if err := checkDecisionPromptLength(atLimit); err != nil {
		t.Errorf("500 multi-byte runes should pass the rune-based check, got: %v", err)
	}
	overLimit := strings.Repeat("あ", maxDecisionPromptRunes+1)
	if err := checkDecisionPromptLength(overLimit); err == nil {
		t.Error("501 multi-byte runes should be rejected by the rune-based check")
	}
}
