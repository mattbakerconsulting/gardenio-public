package bridge

import (
	"reflect"
	"testing"
)

// Options JSON shared by the pure-helper tests. Mirrors the shape produced by
// `gb decision create` (id + short + label + artifact_type).
const chosenTestOptions = `[
	{"id":"mr-now","short":"Open MR now","label":"I open the one-line MR adding the tag now","artifact_type":"diff-summary"},
	{"id":"capacity-first","short":"Pool-size first","label":"Coordinate DevOps to add headroom first","artifact_type":"plan"},
	{"id":"hold","short":"Just track it","label":"No MR now — leave the epic filed","artifact_type":"report"}
]`

func TestMatchOptionID(t *testing.T) {
	opts := parseDecisionOptionRows(chosenTestOptions)
	if len(opts) != 3 {
		t.Fatalf("setup: expected 3 options, got %d", len(opts))
	}

	tests := []struct {
		name    string
		input   string
		wantID  string
		wantHit bool
	}{
		{"exact id", "mr-now", "mr-now", true},
		{"id case-insensitive", "MR-NOW", "mr-now", true},
		{"id trimmed", "  hold  ", "hold", true},
		{"short match", "Pool-size first", "capacity-first", true},
		{"short case-insensitive", "just track it", "hold", true},
		{"label match", "I open the one-line MR adding the tag now", "mr-now", true},
		{"label trimmed+ci", "  no mr now — leave the epic filed  ", "hold", true},
		{"free-text no match", "i dont like either of these", "", false},
		{"empty", "", "", false},
		{"whitespace only", "   ", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotID, gotHit := matchOptionID(opts, tt.input)
			if gotID != tt.wantID || gotHit != tt.wantHit {
				t.Errorf("matchOptionID(%q) = (%q, %v), want (%q, %v)",
					tt.input, gotID, gotHit, tt.wantID, tt.wantHit)
			}
		})
	}
}

func TestMatchOptionID_IDBeatsLabelCollision(t *testing.T) {
	// A pathological set where one option's LABEL equals another option's ID.
	// The id pass must win so "b" resolves to option b, not option a.
	opts := parseDecisionOptionRows(`[
		{"id":"a","label":"b"},
		{"id":"b","label":"beta"}
	]`)
	if id, ok := matchOptionID(opts, "b"); !ok || id != "b" {
		t.Errorf("matchOptionID(\"b\") = (%q, %v), want (\"b\", true) — id match must beat label", id, ok)
	}
}

func TestDecisionResolutionFieldsFor(t *testing.T) {
	tests := []struct {
		name       string
		optionsRaw string
		text       string
		want       map[string]string
	}{
		{
			name:       "matches option label -> chosen is id",
			optionsRaw: chosenTestOptions,
			text:       "Pool-size first",
			want:       map[string]string{"chosen": "capacity-first"},
		},
		{
			name:       "matches option id -> chosen is id",
			optionsRaw: chosenTestOptions,
			text:       "mr-now",
			want:       map[string]string{"chosen": "mr-now"},
		},
		{
			name:       "free text -> response_text, no chosen",
			optionsRaw: chosenTestOptions,
			text:       "i dont like either of these, there must be a way to wait",
			want:       map[string]string{"response_text": "i dont like either of these, there must be a way to wait"},
		},
		{
			name:       "no options -> free text to response_text",
			optionsRaw: "",
			text:       "blue",
			want:       map[string]string{"response_text": "blue"},
		},
		{
			name:       "shutdown keyword stays in chosen (respawn-skip guard)",
			optionsRaw: chosenTestOptions,
			text:       "shut down",
			want:       map[string]string{"chosen": "shut down"},
		},
		{
			name:       "shutdown phrase within sentence stays in chosen",
			optionsRaw: "",
			text:       "please stand down for now",
			want:       map[string]string{"chosen": "please stand down for now"},
		},
		{
			name:       "text is trimmed before storing",
			optionsRaw: "",
			text:       "  blue  ",
			want:       map[string]string{"response_text": "blue"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := decisionResolutionFieldsFor(tt.optionsRaw, tt.text)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("decisionResolutionFieldsFor(_, %q) = %v, want %v", tt.text, got, tt.want)
			}
		})
	}
}

func TestParseDecisionOptionRows_Malformed(t *testing.T) {
	if rows := parseDecisionOptionRows(""); rows != nil {
		t.Errorf("empty -> want nil, got %v", rows)
	}
	if rows := parseDecisionOptionRows("not json"); rows != nil {
		t.Errorf("malformed -> want nil, got %v", rows)
	}
	if rows := parseDecisionOptionRows("[]"); len(rows) != 0 {
		t.Errorf("empty array -> want 0 rows, got %d", len(rows))
	}
}
