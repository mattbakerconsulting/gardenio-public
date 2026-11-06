package references

import (
	"reflect"
	"testing"
)

func TestExtract(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "empty",
			in:   "",
			want: nil,
		},
		{
			name: "no matches",
			in:   "this text has no bead ids in it at all",
			want: nil,
		},
		{
			name: "single match",
			in:   "see gd-iMrpT42pGz for details",
			want: []string{"gd-iMrpT42pGz"},
		},
		{
			name: "embedded in markdown",
			in:   "Closes [gd-iMrpT42pGz](/p/foo). Related: `gd-n5F2I5gzEp`. (gd-xfshRmkdC5).",
			want: []string{"gd-iMrpT42pGz", "gd-n5F2I5gzEp", "gd-xfshRmkdC5"},
		},
		{
			name: "duplicates collapsed",
			in:   "gd-iMrpT42pGz see also gd-iMrpT42pGz and gd-n5F2I5gzEp, but gd-iMrpT42pGz again",
			want: []string{"gd-iMrpT42pGz", "gd-n5F2I5gzEp"},
		},
		{
			name: "first-seen order preserved",
			in:   "gd-XIIDB212HW then gd-FNHTKRBDIQ then gd-OWZVUD8K22",
			want: []string{"gd-XIIDB212HW", "gd-FNHTKRBDIQ", "gd-OWZVUD8K22"},
		},
		{
			name: "length boundary 9 chars rejected",
			in:   "gd-abcDEF012 is too short",
			want: nil,
		},
		{
			name: "length boundary 10 chars accepted",
			in:   "gd-iMrpT42pGz is right",
			want: []string{"gd-iMrpT42pGz"},
		},
		{
			name: "length boundary 11 chars rejected",
			in:   "gd-abcDEF01234 is too long",
			want: nil,
		},
		{
			name: "bad alphabet underscore",
			in:   "gd-abcDEF_123 has an underscore",
			want: nil,
		},
		{
			name: "bad alphabet hyphen",
			in:   "gd-abcDEF-123 has a hyphen",
			want: nil,
		},
		{
			name: "bad alphabet space",
			in:   "gd-abcDEF 123 has a space",
			want: nil,
		},
		{
			name: "case preservation",
			in:   "gd-AbCdEfGhIj and gd-aBcDeFgHiJ are distinct",
			want: []string{"gd-AbCdEfGhIj", "gd-aBcDeFgHiJ"},
		},
		{
			name: "multiline",
			in:   "first line gd-iMrpT42pGz\nsecond line gd-n5F2I5gzEp\nthird gd-iMrpT42pGz again",
			want: []string{"gd-iMrpT42pGz", "gd-n5F2I5gzEp"},
		},
		{
			name: "word boundary: leading letter rejects",
			in:   "xgd-abcDEF0123 should not match",
			want: nil,
		},
		{
			name: "word boundary: trailing alnum extends past 10",
			in:   "gd-abcDEF0123x has trailing letter",
			want: nil,
		},
		{
			name: "punctuation boundaries accepted",
			in:   "(gd-iMrpT42pGz),[gd-n5F2I5gzEp];{gd-xfshRmkdC5}!",
			want: []string{"gd-iMrpT42pGz", "gd-n5F2I5gzEp", "gd-xfshRmkdC5"},
		},
		{
			name: "all digits",
			in:   "gd-UxaO9Bh6Zc is valid",
			want: []string{"gd-UxaO9Bh6Zc"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Extract(tt.in)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Extract(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestPattern_String(t *testing.T) {
	want := `\b(?:gd|ka)-[A-Za-z0-9]{10}\b`
	if got := Pattern.String(); got != want {
		t.Errorf("Pattern.String() = %q, want %q", got, want)
	}
}
