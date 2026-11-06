package bridge

import "testing"

func TestParseUltracodeFlag(t *testing.T) {
	tests := []struct {
		name            string
		text            string
		wantValue       string
		wantUnrecognized string
		wantRemaining   string
	}{
		{
			name:          "bare flag is on",
			text:          "--ultracode fix the bug",
			wantValue:     "on",
			wantRemaining: "fix the bug",
		},
		{
			name:          "=on",
			text:          "--ultracode=on ship it",
			wantValue:     "on",
			wantRemaining: "ship it",
		},
		{
			name:          "=off",
			text:          "--ultracode=off just answer",
			wantValue:     "off",
			wantRemaining: "just answer",
		},
		{
			name:          "true alias",
			text:          "--ultracode=true",
			wantValue:     "on",
			wantRemaining: "",
		},
		{
			name:          "0 alias",
			text:          "--ultracode=0",
			wantValue:     "off",
			wantRemaining: "",
		},
		{
			// Flag is recognized anywhere in the text (no space-separated value),
			// so it composes with the other flag parsers regardless of order.
			name:          "flag not at start",
			text:          "fix the bug --ultracode=off",
			wantValue:     "off",
			wantRemaining: "fix the bug",
		},
		{
			name:             "unrecognized value",
			text:             "--ultracode=maybe do it",
			wantUnrecognized: "maybe",
			wantRemaining:    "do it",
		},
		{
			// Absent flag yields ("","",text) — critically NOT "off", so a prior
			// carried-forward "off" is never clobbered on a no-flag wake-up.
			name:          "absent",
			text:          "just a normal prompt",
			wantValue:     "",
			wantRemaining: "just a normal prompt",
		},
		{
			name:          "empty",
			text:          "",
			wantValue:     "",
			wantRemaining: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			value, unrecognized, remaining := parseUltracodeFlag(tt.text)
			if value != tt.wantValue {
				t.Errorf("value = %q, want %q", value, tt.wantValue)
			}
			if unrecognized != tt.wantUnrecognized {
				t.Errorf("unrecognized = %q, want %q", unrecognized, tt.wantUnrecognized)
			}
			if remaining != tt.wantRemaining {
				t.Errorf("remaining = %q, want %q", remaining, tt.wantRemaining)
			}
		})
	}
}
