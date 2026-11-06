package main

import "testing"

// TestSanitizeRefusalSource asserts that only the closed set of known refusal
// labels is preserved verbatim; anything else falls back to "unknown" so the
// bug bead always has a safe, queryable label (ka-0000006731).
func TestSanitizeRefusalSource(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"header_missing", "header_missing"},
		{"header_non_human", "header_non_human"},
		{"fallback_agent_only", "fallback_agent_only"},
		{"chain_root_emailless", "chain_root_emailless"},
		{"unknown", "unknown"},
		{"  header_missing  ", "header_missing"},
		{"", "unknown"},
		{"freeform garbage", "unknown"},
		{"slack:U123", "unknown"},
	}
	for _, tc := range cases {
		if got := sanitizeRefusalSource(tc.in); got != tc.want {
			t.Errorf("sanitizeRefusalSource(%q): got %q want %q", tc.in, got, tc.want)
		}
	}
}

// TestSanitizeRefusalCommand asserts the command summary is stripped of all
// flag values and length-capped. The harness must NEVER leak free-form text
// (descriptions, comment bodies) from --flags into the bug bead.
func TestSanitizeRefusalCommand(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"kd comment add gd-XYZ", "kd comment add gd-XYZ"},
		{"kd comment add gd-XYZ --text secret", "kd comment add gd-XYZ"},
		{"kd create title --type=task --description='secret leak'", "kd create title"},
		{"  kd update gd-ABC  ", "kd update gd-ABC"},
		{"", "kd <unknown>"},
	}
	for _, tc := range cases {
		if got := sanitizeRefusalCommand(tc.in); got != tc.want {
			t.Errorf("sanitizeRefusalCommand(%q): got %q want %q", tc.in, got, tc.want)
		}
	}
}

func TestSanitizeRefusalCommandLengthCap(t *testing.T) {
	var b []byte
	for range 100 {
		b = append(b, []byte("kd ")...)
	}
	got := sanitizeRefusalCommand(string(b))
	if len(got) > 200 {
		t.Errorf("sanitizeRefusalCommand long input not capped, len=%d", len(got))
	}
}

// TestSanitizeRefusalAgent assets the agent name falls back to a stable
// "(unknown agent)" sentinel when empty, so the bug bead is never silently
// agent-less.
func TestSanitizeRefusalAgent(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"my-bot", "my-bot"},
		{"   ", "(unknown agent)"},
		{"", "(unknown agent)"},
	}
	for _, tc := range cases {
		if got := sanitizeRefusalAgent(tc.in); got != tc.want {
			t.Errorf("sanitizeRefusalAgent(%q): got %q want %q", tc.in, got, tc.want)
		}
	}
}
