package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// TestBuildAgentCardTLDR_TldrFeatured asserts the validated `tldr` becomes the
// featured headline with the clean-finish glyph/outcome.
func TestBuildAgentCardTLDR_TldrFeatured(t *testing.T) {
	bead := &beadsapi.BeadDetail{
		ID:     "gd-agt",
		Type:   "agent",
		Status: "closed",
		Fields: map[string]string{
			"agent":  "crew-1",
			"wrapup": `{"tldr":"Merged MR 3186; card layout tidied","accomplishments":"details"}`,
		},
	}
	got := buildAgentCardTLDR(bead)
	if got.Text != "Merged MR 3186; card layout tidied" {
		t.Errorf("Text = %q, want the tldr", got.Text)
	}
	if got.Glyph != "✅" || got.Outcome != "ok" {
		t.Errorf("glyph/outcome = %q/%q, want ✅/ok", got.Glyph, got.Outcome)
	}
}

// TestBuildAgentCardTLDR_BlockedOutcome asserts a wrap-up carrying blockers is
// classed as a blocked stop even when a tldr is present.
func TestBuildAgentCardTLDR_BlockedOutcome(t *testing.T) {
	bead := &beadsapi.BeadDetail{
		ID:     "gd-agt",
		Type:   "agent",
		Status: "closed",
		Fields: map[string]string{
			"wrapup": `{"tldr":"Stuck on secret","blockers":"needs operator to mint token"}`,
		},
	}
	got := buildAgentCardTLDR(bead)
	if got.Text != "Stuck on secret" {
		t.Errorf("Text = %q", got.Text)
	}
	if got.Glyph != "⛔" || got.Outcome != "fail" {
		t.Errorf("glyph/outcome = %q/%q, want ⛔/fail", got.Glyph, got.Outcome)
	}
}

// TestBuildAgentCardTLDR_HandoffOutcome asserts handoff_notes (no blockers)
// classes the banner as handed-off.
func TestBuildAgentCardTLDR_HandoffOutcome(t *testing.T) {
	bead := &beadsapi.BeadDetail{
		ID:     "gd-agt",
		Type:   "agent",
		Status: "closed",
		Fields: map[string]string{
			"wrapup": `{"tldr":"Phase 1 done","handoff_notes":"phase 2 still open"}`,
		},
	}
	got := buildAgentCardTLDR(bead)
	if got.Glyph != "↗️" || got.Outcome != "handoff" {
		t.Errorf("glyph/outcome = %q/%q, want ↗️/handoff", got.Glyph, got.Outcome)
	}
}

// TestBuildAgentCardTLDR_FallbackAccomplishments asserts a wrap-up with no tldr
// falls back to accomplishments.
func TestBuildAgentCardTLDR_FallbackAccomplishments(t *testing.T) {
	bead := &beadsapi.BeadDetail{
		ID:     "gd-agt",
		Type:   "agent",
		Status: "closed",
		Fields: map[string]string{
			"wrapup": `{"accomplishments":"Did the thing without a tldr"}`,
		},
	}
	got := buildAgentCardTLDR(bead)
	if got.Text != "Did the thing without a tldr" {
		t.Errorf("Text = %q, want accomplishments fallback", got.Text)
	}
}

// TestBuildAgentCardTLDR_FallbackCloseReason asserts a terminal bead with no
// wrap-up at all derives a headline from close_reason.
func TestBuildAgentCardTLDR_FallbackCloseReason(t *testing.T) {
	bead := &beadsapi.BeadDetail{
		ID:     "gd-agt",
		Type:   "agent",
		Status: "closed",
		Fields: map[string]string{
			"close_reason": "context_cancelled: pod evicted",
		},
	}
	got := buildAgentCardTLDR(bead)
	if got.Text != "context_cancelled: pod evicted" {
		t.Errorf("Text = %q, want close_reason fallback", got.Text)
	}
}

// TestBuildAgentCardTLDR_LiveAgentEmpty asserts a live (non-terminal) agent
// renders no banner — the working-state header is untouched.
func TestBuildAgentCardTLDR_LiveAgentEmpty(t *testing.T) {
	bead := &beadsapi.BeadDetail{
		ID:     "gd-agt",
		Type:   "agent",
		Status: "open",
		Fields: map[string]string{
			"agent_state": "working",
			"wrapup":      `{"tldr":"should not show while live"}`,
		},
	}
	if got := buildAgentCardTLDR(bead); got.Text != "" {
		t.Errorf("live agent should have empty banner, got %q", got.Text)
	}
}

// TestBuildAgentCardTLDR_NoSignalEmpty asserts a terminal bead with neither a
// wrap-up nor a close_reason yields an empty (suppressed) banner.
func TestBuildAgentCardTLDR_NoSignalEmpty(t *testing.T) {
	bead := &beadsapi.BeadDetail{
		ID:     "gd-agt",
		Type:   "agent",
		Status: "closed",
		Fields: map[string]string{"agent": "crew-1"},
	}
	if got := buildAgentCardTLDR(bead); got.Text != "" {
		t.Errorf("no-signal terminal bead should have empty banner, got %q", got.Text)
	}
}

// TestBuildAgentCardTLDR_PlatformFailure asserts a zero-inference-turn run with
// no human wrap-up is re-badged as a platform-status headline.
func TestBuildAgentCardTLDR_PlatformFailure(t *testing.T) {
	bead := &beadsapi.BeadDetail{
		ID:     "gd-agt",
		Type:   "agent",
		Status: "closed",
		Fields: map[string]string{
			"total_request_count": "1",
			"total_input_tokens":  "0",
			"total_tool_calls":    "0",
			"wrapup":              `{"tldr":"Pod terminated before an explicit wrap-up was submitted","synthesized":"true"}`,
		},
	}
	got := buildAgentCardTLDR(bead)
	if got.Outcome != "platform" || got.Glyph != "🛰️" {
		t.Errorf("glyph/outcome = %q/%q, want 🛰️/platform", got.Glyph, got.Outcome)
	}
	if !strings.Contains(got.Text, "never got an inference turn") {
		t.Errorf("Text = %q, want platform-failure headline", got.Text)
	}
}

// TestBuildAgentCardTLDR_PlatformFailure_HumanTLDRWins asserts a real
// human-authored tldr wins over the platform re-badge even on a
// zero-inference-turn bead.
func TestBuildAgentCardTLDR_PlatformFailure_HumanTLDRWins(t *testing.T) {
	bead := &beadsapi.BeadDetail{
		ID:     "gd-agt",
		Type:   "agent",
		Status: "closed",
		Fields: map[string]string{
			"total_request_count": "1",
			"total_input_tokens":  "0",
			"total_tool_calls":    "0",
			"wrapup":              `{"tldr":"Real work summary","accomplishments":"did stuff"}`,
		},
	}
	got := buildAgentCardTLDR(bead)
	if got.Text != "Real work summary" || got.Outcome == "platform" {
		t.Errorf("human tldr should win: text=%q outcome=%q", got.Text, got.Outcome)
	}
}

// TestBeadShow_AgentType_TLDR_Rendered asserts the banner renders at the top of
// the HTML agent card with the outcome class and text.
func TestBeadShow_AgentType_TLDR_Rendered(t *testing.T) {
	f := newFake()
	f.beads["gd-agt-tldr"] = &beadsapi.BeadDetail{
		ID:     "gd-agt-tldr",
		Title:  "crew-1",
		Type:   "agent",
		Status: "closed",
		Fields: map[string]string{
			"agent":  "crew-1",
			"role":   "crew",
			"wrapup": `{"tldr":"Shipped the TLDR banner","accomplishments":"x"}`,
		},
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/gd-agt-tldr", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`class="agent-tldr agent-tldr-ok"`,
		"Shipped the TLDR banner",
		`aria-label="Wrap-up summary"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("card missing %q\nbody: %s", want, body)
		}
	}
	// The banner must render BEFORE the header block (top of card). Anchor on
	// the rendered banner div class (which the head CSS does not contain) so the
	// index reflects the card body, not the stylesheet.
	tldrIdx := strings.Index(body, `class="agent-tldr agent-tldr-ok"`)
	hdrIdx := strings.Index(body, `class="agent-header"`)
	if tldrIdx < 0 || hdrIdx < 0 || tldrIdx > hdrIdx {
		t.Errorf("TLDR banner (idx %d) must precede agent-header (idx %d)", tldrIdx, hdrIdx)
	}
}

// TestBeadShow_AgentType_TLDR_LiveOmitted asserts a live agent renders no
// banner in the HTML card.
func TestBeadShow_AgentType_TLDR_LiveOmitted(t *testing.T) {
	f := newFake()
	f.beads["gd-agt-live"] = &beadsapi.BeadDetail{
		ID:     "gd-agt-live",
		Title:  "crew-live",
		Type:   "agent",
		Status: "open",
		Fields: map[string]string{
			"agent":       "crew-live",
			"role":        "crew",
			"agent_state": "working",
			"wrapup":      `{"tldr":"should not show"}`,
		},
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/gd-agt-live", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	// "agent-tldr" also appears in the page's inline CSS, so assert on the
	// rendered banner div + its aria-label, which appear only when the banner
	// is emitted.
	if body := w.Body.String(); strings.Contains(body, `aria-label="Wrap-up summary"`) {
		t.Errorf("live agent card unexpectedly rendered a TLDR banner\nbody: %s", body)
	}
}
