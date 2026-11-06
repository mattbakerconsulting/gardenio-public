package bridge

import (
	"strings"
	"testing"
)

func TestFormatTriggerCardLine(t *testing.T) {
	tests := []struct {
		name string
		in   triggerCardInfo
		want string
	}{
		{
			name: "empty trigger renders nothing",
			in:   triggerCardInfo{},
			want: "",
		},
		{
			name: "id only falls back to id as label",
			in:   triggerCardInfo{ID: "gd-AAA"},
			want: "🌩️ trigger: gd-AAA",
		},
		{
			name: "source and match key render with run count",
			in: triggerCardInfo{
				ID:       "gd-BBB",
				Title:    "Watch PROJ-123 transitions",
				Source:   "jira",
				MatchKey: "PROJ-123",
				RunCount: 7,
			},
			want: "🌩️ trigger: Watch PROJ-123 transitions \u00b7 via jira: `PROJ-123` \u00b7 run #7",
		},
		{
			name: "viewer base url linkifies title",
			in: triggerCardInfo{
				ID:            "gd-CCC",
				Title:         "Nightly cleanup",
				Source:        "schedule",
				RunCount:      0,
				ViewerBaseURL: "https://beads.example/",
			},
			want: "🌩️ trigger: <https://beads.example/gd-CCC|Nightly cleanup> \u00b7 via schedule",
		},
		{
			name: "match key only (no source)",
			in: triggerCardInfo{
				ID:       "gd-DDD",
				Title:    "Adhoc",
				MatchKey: "abc123",
			},
			want: "🌩️ trigger: Adhoc \u00b7 `abc123`",
		},
		{
			name: "title with slack meta characters is escaped",
			in: triggerCardInfo{
				ID:            "gd-EEE",
				Title:         "<script> & friends",
				ViewerBaseURL: "https://beads.example",
			},
			want: "🌩️ trigger: <https://beads.example/gd-EEE|&lt;script&gt; &amp; friends>",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := formatTriggerCardLine(Garden, tc.in)
			if got != tc.want {
				t.Errorf("formatTriggerCardLine(%+v):\n got: %q\nwant: %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestEscapeSlackMrkdwn(t *testing.T) {
	in := "a < b & c > d"
	got := escapeSlackMrkdwn(in)
	if strings.ContainsAny(got, "<>") {
		t.Errorf("expected angle brackets escaped; got %q", got)
	}
	want := "a &lt; b &amp; c &gt; d"
	if got != want {
		t.Errorf("escapeSlackMrkdwn(%q) = %q; want %q", in, got, want)
	}
}

// TestFormatTriggerCardLine_BusinessTheme: Business swaps the glyph for the
// [TRIGGER] tag and keeps the verbal "trigger:" label and the rest of the line.
func TestFormatTriggerCardLine_BusinessTheme(t *testing.T) {
	got := formatTriggerCardLine(Business, triggerCardInfo{ID: "gd-AAA", Source: "jira", MatchKey: "PROJ-1", RunCount: 3})
	want := "[TRIGGER] trigger: gd-AAA \u00b7 via jira: `PROJ-1` \u00b7 run #3"
	if got != want {
		t.Errorf("Business trigger line = %q, want %q", got, want)
	}
}
