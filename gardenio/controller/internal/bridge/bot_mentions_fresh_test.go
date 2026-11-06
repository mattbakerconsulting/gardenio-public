package bridge

import "testing"

// TestParseFreshFlag mirrors TestParseListenFlag: --fresh is a scan-every-word
// bare flag (no value to consume), so it is recognized anywhere in the mention
// and composes with the other flag parsers regardless of order, and the token is
// always stripped so it never leaks into the resume Description the agent reads.
func TestParseFreshFlag(t *testing.T) {
	tests := []struct {
		name          string
		text          string
		wantFresh     bool
		wantRemaining string
	}{
		{
			name:          "no flag",
			text:          "keep going on the fix",
			wantFresh:     false,
			wantRemaining: "keep going on the fix",
		},
		{
			name:          "fresh at start",
			text:          "--fresh new question entirely",
			wantFresh:     true,
			wantRemaining: "new question entirely",
		},
		{
			name:          "fresh at end",
			text:          "new question entirely --fresh",
			wantFresh:     true,
			wantRemaining: "new question entirely",
		},
		{
			name:          "fresh in middle",
			text:          "new --fresh question entirely",
			wantFresh:     true,
			wantRemaining: "new question entirely",
		},
		{
			name:          "fresh only",
			text:          "--fresh",
			wantFresh:     true,
			wantRemaining: "",
		},
		{
			name:          "empty text",
			text:          "",
			wantFresh:     false,
			wantRemaining: "",
		},
		{
			// Composes with the other spawn flags; --fresh is consumed and the rest
			// (including a later flag another parser will consume) is left intact.
			name:          "fresh with project and ultracode",
			text:          "--fresh project:gardenio --ultracode=on ship it",
			wantFresh:     true,
			wantRemaining: "project:gardenio --ultracode=on ship it",
		},
		{
			// A bare word that merely CONTAINS the flag text is not the flag —
			// exact-token match only, matching parseListenFlag.
			name:          "not a fresh substring",
			text:          "make the bread fresh-baked",
			wantFresh:     false,
			wantRemaining: "make the bread fresh-baked",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fresh, remaining := parseFreshFlag(tt.text)
			if fresh != tt.wantFresh {
				t.Errorf("fresh = %v, want %v", fresh, tt.wantFresh)
			}
			if remaining != tt.wantRemaining {
				t.Errorf("remaining = %q, want %q", remaining, tt.wantRemaining)
			}
		})
	}
}

// TestStripLeadingOverrideFlags_StripsFresh locks in that the live-agent flag
// stripper consumes --fresh too, so the token never leaks verbatim into the
// prompt forwarded to an already-running agent (the bug class of ka-0000001727).
func TestStripLeadingOverrideFlags_StripsFresh(t *testing.T) {
	stripped, rawModel := stripLeadingOverrideFlags("--fresh keep going")
	if stripped != "keep going" {
		t.Errorf("stripped = %q, want %q", stripped, "keep going")
	}
	if rawModel != "" {
		t.Errorf("rawModel = %q, want empty", rawModel)
	}
}
