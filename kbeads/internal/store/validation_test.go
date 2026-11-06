package store

import "testing"

// TestValidFieldKey exercises the ValidFieldKey grammar
// (^[A-Za-z_][A-Za-z0-9_]*$). This key is prod-called before every jsonb
// path interpolation (http_beads.go, postgres/queries.go, array_fields.go,
// subfield_edits.go), so both the accept and reject branches carry a
// SQL-injection safety guarantee and must stay covered.
func TestValidFieldKey(t *testing.T) {
	cases := []struct {
		name string
		key  string
		want bool
	}{
		// Accept: identifier-shaped keys.
		{"simple lower", "status", true},
		{"simple upper", "STATUS", true},
		{"mixed case", "TaskID", true},
		{"leading underscore", "_private", true},
		{"underscore only", "_", true},
		{"digits after letter", "field1", true},
		{"digits and underscores", "a_1_b_2", true},
		{"single letter", "x", true},
		{"all underscores", "___", true},
		{"trailing digits", "operated_by_id", true},

		// Reject: anything outside the identifier grammar.
		{"empty", "", false},
		{"leading digit", "1field", false},
		{"digit only", "9", false},
		{"contains dash", "field-key", false},
		{"contains dot", "a.b", false},
		{"contains space", "field key", false},
		{"leading space", " field", false},
		{"trailing space", "field ", false},
		{"contains colon", "slack:U0OPERATOR1", false},
		{"jsonb path traversal", "a'->>'b", false},
		{"sql injection attempt", "x; DROP TABLE beads", false},
		{"quote", "field\"", false},
		{"newline", "field\nkey", false},
		{"tab", "field\tkey", false},
		{"unicode letter", "naïve", false},
		{"embedded newline valid-looking parts", "valid\nvalid", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ValidFieldKey(tc.key); got != tc.want {
				t.Errorf("ValidFieldKey(%q) = %v, want %v", tc.key, got, tc.want)
			}
		})
	}
}
