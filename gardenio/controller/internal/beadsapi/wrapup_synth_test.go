package beadsapi

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestSynthesizeReapWrapUp_BuildsValidJSON verifies the controller-side
// fallback (Fix A, ka-0000007639) produces a valid wrap-up JSON with a tl;dr
// derived from the reap reason, the title appended, and the synthesized marker
// + close_reason preserved in custom.
func TestSynthesizeReapWrapUp_BuildsValidJSON(t *testing.T) {
	out := SynthesizeReapWrapUp("stale: no activity for 10m0s", "gardenio-thread-foo-abcd")
	if out == "" {
		t.Fatal("SynthesizeReapWrapUp returned empty")
	}
	var wu synthWrapUp
	if err := json.Unmarshal([]byte(out), &wu); err != nil {
		t.Fatalf("synthesized wrap-up is not valid JSON: %v\n%s", err, out)
	}
	if strings.TrimSpace(wu.TLDR) == "" {
		t.Errorf("synthesized wrap-up has empty tldr: %s", out)
	}
	if !strings.Contains(wu.TLDR, "stale") {
		t.Errorf("tldr should reflect the stale reason, got: %q", wu.TLDR)
	}
	if !strings.Contains(wu.TLDR, "gardenio-thread-foo-abcd") {
		t.Errorf("tldr should include the agent identity, got: %q", wu.TLDR)
	}
	if wu.Custom["synthesized"] != "true" {
		t.Errorf("wrap-up should be marked synthesized=true, got: %v", wu.Custom)
	}
	if wu.Custom["close_reason"] != "stale: no activity for 10m0s" {
		t.Errorf("wrap-up should preserve close_reason, got: %v", wu.Custom)
	}
}

// TestReapReasonTLDR_Families verifies each reap-reason family maps to a
// non-empty, recognizable phrase and unknown reasons surface verbatim.
func TestReapReasonTLDR_Families(t *testing.T) {
	cases := []struct {
		reason  string
		wantSub string
	}{
		{"", "Reaped by the controller"},
		{"orphan: source task gd-X already closed", "orphaned"},
		{"stale: no activity for 10m0s", "stale"},
		{"stale: stuck in spawning state for 5m0s", "stale"},
		{"podless_leak: agent_state=rate_limited but no pod found", "pod was gone"},
		{"weird_custom_reason", "weird_custom_reason"},
	}
	for _, tc := range cases {
		got := reapReasonTLDR(tc.reason)
		if strings.TrimSpace(got) == "" {
			t.Errorf("reapReasonTLDR(%q) returned empty", tc.reason)
		}
		if !strings.Contains(got, tc.wantSub) {
			t.Errorf("reapReasonTLDR(%q) = %q, want substring %q", tc.reason, got, tc.wantSub)
		}
	}
}

// TestSynthesizeReapWrapUp_NoTitle verifies the tl;dr still works (no trailing
// separator) when no title is available.
func TestSynthesizeReapWrapUp_NoTitle(t *testing.T) {
	out := SynthesizeReapWrapUp("orphan: source task gd-X already closed", "")
	var wu synthWrapUp
	if err := json.Unmarshal([]byte(out), &wu); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if strings.Contains(wu.TLDR, "—") {
		t.Errorf("tldr should have no title separator when title is empty, got: %q", wu.TLDR)
	}
	if !strings.Contains(wu.TLDR, "orphaned") {
		t.Errorf("tldr should reflect the orphan reason, got: %q", wu.TLDR)
	}
}

// TestHasRealWrapUp distinguishes a genuine agent-recorded wrap-up from an
// empty/malformed one and from a controller-synthesized fallback (ka-0000006105).
func TestHasRealWrapUp(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"empty string", "", false},
		{"whitespace only", "   ", false},
		{"malformed JSON", "{not json", false},
		{"valid JSON, empty accomplishments", `{"tldr":"hi","accomplishments":""}`, false},
		{"real wrap-up", `{"tldr":"done","accomplishments":"triaged 5 beads"}`, true},
		{"real wrap-up, accomplishments only", `{"accomplishments":"did the thing"}`, true},
		{
			"synthesized fallback is not real",
			SynthesizeReapWrapUp("stale: no activity for 10m0s", "gardenio-foo"),
			false,
		},
		{
			"accomplishments present but explicitly synthesized",
			`{"accomplishments":"x","custom":{"synthesized":"true"}}`,
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := HasRealWrapUp(tc.raw); got != tc.want {
				t.Errorf("HasRealWrapUp(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

// TestReapWrapUpTitle_PrefersAgentName verifies the title helper prefers the
// agent field, falls back to the bead title, and is nil-safe.
func TestReapWrapUpTitle_PrefersAgentName(t *testing.T) {
	if got := ReapWrapUpTitle(nil); got != "" {
		t.Errorf("ReapWrapUpTitle(nil) = %q, want empty", got)
	}
	withAgent := &BeadDetail{Title: "some title", Fields: map[string]string{"agent": "gardenio-foo"}}
	if got := ReapWrapUpTitle(withAgent); got != "gardenio-foo" {
		t.Errorf("ReapWrapUpTitle should prefer agent name, got %q", got)
	}
	titleOnly := &BeadDetail{Title: "some title", Fields: map[string]string{}}
	if got := ReapWrapUpTitle(titleOnly); got != "some title" {
		t.Errorf("ReapWrapUpTitle should fall back to title, got %q", got)
	}
}
