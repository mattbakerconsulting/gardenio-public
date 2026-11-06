package bridge

import (
	"strings"
	"testing"
	"unicode/utf8"

	"gardenio/controller/internal/beadsapi"
)

// TestBeadDisplayName_Order pins the resolution order: the short_title column,
// then fields.short_title, then shorttitle.Derive, then the id. The heuristics
// themselves are pinned in internal/shorttitle (TestDerive).
func TestBeadDisplayName_Order(t *testing.T) {
	cases := []struct {
		name string
		bead beadsapi.BeadDetail
		want string
	}{
		{
			name: "column short_title wins over fields and heuristics",
			bead: beadsapi.BeadDetail{ID: "gd-lGvUZtjpRQ", Type: "task",
				Title:      "P0: forward duration_ms + tool_use_id through gb hook relay",
				ShortTitle: "x",
				Fields:     map[string]string{"short_title": "y"}},
			want: "x",
		},
		{
			name: "authored fields short_title wins over every heuristic",
			bead: beadsapi.BeadDetail{ID: "gd-lGvUZtjpRQ", Type: "task",
				Title:  "P0: forward duration_ms + tool_use_id through gb hook relay",
				Fields: map[string]string{"short_title": "forward duration_ms through gb hook relay"}},
			want: "forward duration_ms through gb hook relay",
		},
		{
			name: "blank column falls through to fields",
			bead: beadsapi.BeadDetail{ID: "gd-x", Type: "task", Title: "t", ShortTitle: "  ",
				Fields: map[string]string{"short_title": "y"}},
			want: "y",
		},
		{
			name: "authored short_title is still bounded",
			bead: beadsapi.BeadDetail{ID: "gd-x", Type: "task", Title: "t",
				ShortTitle: strings.Repeat("word ", 20)},
			want: "word word word word word word word word…",
		},
		{
			name: "no short_title falls to the heuristics",
			bead: beadsapi.BeadDetail{ID: "gd-6LYU2D5VUQ", Type: "escalation",
				Title: "Merge PR #209: P4 per-session slowest tool calls (gd-YcmtujOTrC)"},
			want: "Merge PR #209",
		},
		{
			name: "agent: handle, never the title shape",
			bead: beadsapi.BeadDetail{ID: "gd-DDG9qRDcmq", Type: "agent", Title: "gardenio-p3-measure-the-u4n"},
			want: "@gardenio-p3-measure-the-u4n",
		},
		{
			name: "empty title falls back to the id",
			bead: beadsapi.BeadDetail{ID: "gd-empty", Type: "task", Title: "   "},
			want: "gd-empty",
		},
		{
			name: "title that is only an id falls back to the id",
			bead: beadsapi.BeadDetail{ID: "gd-only", Type: "task", Title: "gd-abcdef"},
			want: "gd-only",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bead := tc.bead
			got := beadDisplayName(&bead)
			if got != tc.want {
				t.Fatalf("beadDisplayName(%q)\n got %q\nwant %q", tc.bead.Title, got, tc.want)
			}
			if n := utf8.RuneCountInString(got); n > displayNameMax {
				t.Fatalf("display name %q is %d runes, over %d", got, n, displayNameMax)
			}
		})
	}
}

func TestBeadDisplayName_NeverContainsBeadID(t *testing.T) {
	titles := []string{
		"Plan-gardener gd-Oc3GVm9gc6 watchdog gd-Lh590iWd8Y budget",
		"ka-0000001234 fix the thing (ka-0000005678)",
		"gd-abc: colon after id",
	}
	for _, title := range titles {
		got := beadDisplayName(&beadsapi.BeadDetail{ID: "gd-self", Type: "task", Title: title})
		if bareBeadIDPattern.MatchString(got) && got != "gd-self" {
			t.Errorf("display name for %q leaked an id: %q", title, got)
		}
	}
}

func TestBeadDisplayName_NilSafe(t *testing.T) {
	if got := beadDisplayName(nil); got != "" {
		t.Fatalf("nil bead: got %q, want empty", got)
	}
}
