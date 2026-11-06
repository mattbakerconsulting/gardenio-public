package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestFindBeadIDs(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "single id",
			in:   "see ka-0000000316 for details",
			want: []string{"ka-0000000316"},
		},
		{
			name: "multiple unique in order",
			in:   "parent ka-0000000044, child ka-0000000316, then ka-0000005040",
			want: []string{"ka-0000000044", "ka-0000000316", "ka-0000005040"},
		},
		{
			name: "duplicates deduped, first wins",
			in:   "gd-FNHTKRBDIQ and again gd-FNHTKRBDIQ plus gd-HKBGWAOTHB",
			want: []string{"gd-FNHTKRBDIQ", "gd-HKBGWAOTHB"},
		},
		{
			name: "rejects wrong length and prefix",
			in:   "gd-short gd-TOOLONGIDENT123 KD-UPPERCASE1 gd-lfe8fd1ank",
			want: []string{"gd-lfe8fd1ank"},
		},
		{
			name: "embedded in punctuation",
			in:   "(ka-0000000316),[gd-u08atreejd]!",
			want: []string{"ka-0000000316", "gd-u08atreejd"},
		},
		{
			name: "empty",
			in:   "",
			want: []string{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := findBeadIDs(tc.in)
			// Normalize nil-vs-empty for equality.
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("findBeadIDs(%q)\n  got  = %v\n  want = %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestSpellID(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		// All-letter id: preserves case, spaced per character, dash as word.
		{"ka-0000000316", "k a dash zero zero zero zero zero zero zero three one six"},
		// Digits expand to english words.
		{"ka-0000005040", "k a dash zero zero zero zero zero zero five zero four zero"},
		// All zeros.
		{"gd-tGxyEe0SGY", "g d dash t G x y E e zero S G Y"},
		// Empty returns empty.
		{"", ""},
	}
	for _, tc := range tests {
		got := spellID(tc.in)
		if got != tc.want {
			t.Errorf("spellID(%q)\n  got  = %q\n  want = %q", tc.in, got, tc.want)
		}
	}
}

func TestSpellingTemplates(t *testing.T) {
	id := "gd-TEZqH3yWHt"
	got := spellingTemplates(id)
	if len(got) != 3 {
		t.Fatalf("want 3 templates, got %d: %v", len(got), got)
	}
	spell := spellID(id)
	wantPrefixes := []string{"bead ", "", "see "}
	for i, tpl := range got {
		prefix := wantPrefixes[i]
		if !strings.HasSuffix(tpl, spell) {
			t.Errorf("template %d = %q does not end with spelling %q", i, tpl, spell)
		}
		if !strings.HasPrefix(tpl, prefix) {
			t.Errorf("template %d = %q does not start with prefix %q", i, tpl, prefix)
		}
	}
	// Empty id -> no templates.
	if got := spellingTemplates(""); got != nil {
		t.Errorf("spellingTemplates(\"\") = %v, want nil", got)
	}
}

// Regression: the spec explicitly calls out this example from the parent
// plan comment — bead ID "ka-0000003232" should spell to
// "k a dash zero zero zero zero zero zero three two three two".
func TestSpellID_PlanExample(t *testing.T) {
	const id = "ka-0000003232"
	const want = "k a dash zero zero zero zero zero zero three two three two"
	if got := spellID(id); got != want {
		t.Fatalf("spellID(%q) = %q, want %q", id, got, want)
	}
}
