package formula

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"gardenio/kbeads/internal/model"
)

func TestShortTitle(t *testing.T) {
	cases := []struct {
		name, declared, title, want string
	}{
		{"declared wins", "ship auth", "Ship the auth service", "ship auth"},
		{"blank declared falls back to title", "  \t", "Build auth", "Build auth"},
		{"title collapsed", "", "Build\n  auth \t now", "Build auth now"},
		{"long title cut at a word", "", "Review the auth change against the rollout checklist", "Review the auth change against the…"},
		{"long declared clipped", "auth auth auth auth auth auth auth auth auth", "x", "auth auth auth auth auth auth auth auth…"},
		{"no boundary cut hard", "", strings.Repeat("a", 50), strings.Repeat("a", 39) + "…"},
		{"trailing punctuation dropped before the cut", "", "Design the auth change: then the rollout and checklist", "Design the auth change: then the…"},
		{"exactly the limit kept", "", strings.Repeat("b", model.ShortTitleMax), strings.Repeat("b", model.ShortTitleMax)},
		{"both empty", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ShortTitle(tc.declared, tc.title)
			if got != tc.want {
				t.Errorf("ShortTitle(%q, %q) = %q; want %q", tc.declared, tc.title, got, tc.want)
			}
			if n := utf8.RuneCountInString(got); n > model.ShortTitleMax {
				t.Errorf("ShortTitle(%q, %q) is %d runes; want <= %d", tc.declared, tc.title, n, model.ShortTitleMax)
			}
		})
	}
}

// A declared short_title gets the same {{var}} substitution as the title,
// including a loop step's per-iteration variable.
func TestExpandStepsSubstitutesShortTitle(t *testing.T) {
	steps := []Step{
		{ID: "plain", Title: "Build {{component}}", ShortTitle: "build {{component}}"},
		{ID: "each-{{i}}", Title: "Shard {{i}}", ShortTitle: "shard {{i}} of {{component}}", Loop: &Loop{Var: "i", Values: []string{"a", "b"}}},
	}
	got, err := ExpandSteps(context.Background(), mapGetter{}, steps, map[string]string{"component": "auth"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"build auth", "shard a of auth", "shard b of auth"}
	if len(got) != len(want) {
		t.Fatalf("got %d steps; want %d", len(got), len(want))
	}
	for i, es := range got {
		if es.ShortTitle != want[i] {
			t.Errorf("step %s short_title = %q; want %q", es.ID, es.ShortTitle, want[i])
		}
	}
}
