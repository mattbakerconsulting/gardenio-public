package bridge

import "testing"

// TestParseQuietFlag mirrors TestParseFreshFlag: --quiet is a scan-every-word
// bare flag (no value to consume), so it is recognized anywhere in the mention,
// composes with the other flag parsers regardless of order, and the token is
// always stripped so it never leaks into the task title or the agent prompt.
func TestParseQuietFlag(t *testing.T) {
	tests := []struct {
		name          string
		text          string
		wantQuiet     bool
		wantRemaining string
	}{
		{
			name:          "no flag",
			text:          "look into the failing deploy",
			wantQuiet:     false,
			wantRemaining: "look into the failing deploy",
		},
		{
			name:          "quiet at start",
			text:          "--quiet just tell me the root cause",
			wantQuiet:     true,
			wantRemaining: "just tell me the root cause",
		},
		{
			name:          "quiet at end",
			text:          "just tell me the root cause --quiet",
			wantQuiet:     true,
			wantRemaining: "just tell me the root cause",
		},
		{
			name:          "quiet in middle",
			text:          "just --quiet tell me the root cause",
			wantQuiet:     true,
			wantRemaining: "just tell me the root cause",
		},
		{
			name:          "quiet only",
			text:          "--quiet",
			wantQuiet:     true,
			wantRemaining: "",
		},
		{
			name:          "empty text",
			text:          "",
			wantQuiet:     false,
			wantRemaining: "",
		},
		{
			// Composes with the other spawn flags; --quiet is consumed and the rest
			// (including a later flag another parser will consume) is left intact.
			name:          "quiet with fresh and role",
			text:          "--quiet --fresh --role reviewer ship it",
			wantQuiet:     true,
			wantRemaining: "--fresh --role reviewer ship it",
		},
		{
			// A bare word that merely CONTAINS the flag text is not the flag —
			// exact-token match only, matching parseFreshFlag.
			name:          "not a quiet substring",
			text:          "keep the office quiet-hours policy",
			wantQuiet:     false,
			wantRemaining: "keep the office quiet-hours policy",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			quiet, remaining := parseQuietFlag(tt.text)
			if quiet != tt.wantQuiet {
				t.Errorf("quiet = %v, want %v", quiet, tt.wantQuiet)
			}
			if remaining != tt.wantRemaining {
				t.Errorf("remaining = %q, want %q", remaining, tt.wantRemaining)
			}
		})
	}
}

// TestJiraQuietSuppressed pins the shared field-read helper the two SSE-driven
// forwarders (squawk, completion) gate on: only the exact "true" value
// suppresses; anything else (absent, empty, "false") does not.
func TestJiraQuietSuppressed(t *testing.T) {
	cases := []struct {
		name   string
		fields map[string]string
		want   bool
	}{
		{"absent", map[string]string{"jira_key": "PE-1"}, false},
		{"nil map", nil, false},
		{"empty value", map[string]string{jiraQuietField: ""}, false},
		{"false value", map[string]string{jiraQuietField: "false"}, false},
		{"true value", map[string]string{jiraQuietField: "true"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := jiraQuietSuppressed(tc.fields); got != tc.want {
				t.Errorf("jiraQuietSuppressed(%v) = %v, want %v", tc.fields, got, tc.want)
			}
		})
	}
}
