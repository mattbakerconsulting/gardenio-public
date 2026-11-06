package main

import (
	"strings"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
)

// ka-0000004918: `kd comment list --since <RFC3339|duration>` (alias --after)
// filters comments client-side so interval watchdogs stop hand-rolling
// timestamp greps against the pretty-printed prefix — one observed grep
// (`19:[1-5]`) silently matched nothing for minutes 06-59 and hid operator
// comments with zero error signal.

func TestParseCommentSince(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		val     string
		want    time.Time
		wantErr bool
	}{
		{"rfc3339", "2026-05-01T09:00:00Z", time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC), false},
		{"rfc3339_offset", "2026-05-01T09:00:00+02:00", time.Date(2026, 5, 1, 7, 0, 0, 0, time.UTC), false},
		{"duration_hours", "4h", now.Add(-4 * time.Hour), false},
		{"duration_minutes", "30m", now.Add(-30 * time.Minute), false},
		{"duration_days", "2d", now.Add(-48 * time.Hour), false},
		{"garbage", "yesterday", time.Time{}, true},
		{"date_only_rejected", "2026-07-18", time.Time{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseCommentSince(tc.val, now)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got %v", tc.val, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !got.Equal(tc.want) {
				t.Fatalf("cutoff = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFilterCommentsSince_BoundaryInclusive(t *testing.T) {
	cutoff := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	comments := []*model.Comment{
		{Text: "before", CreatedAt: cutoff.Add(-time.Second)},
		{Text: "exact", CreatedAt: cutoff},
		{Text: "after", CreatedAt: cutoff.Add(time.Second)},
	}
	got := filterCommentsSince(comments, cutoff)
	if len(got) != 2 {
		t.Fatalf("got %d comments, want 2 (exact + after): %+v", len(got), got)
	}
	if got[0].Text != "exact" || got[1].Text != "after" {
		t.Errorf("boundary comment must be included: got %q, %q", got[0].Text, got[1].Text)
	}
}

// setupCommentListClient swaps in a mock client seeded with comments on gd-cs1
// at 08:00, 09:00 and 10:00 UTC and restores the original on cleanup.
func setupCommentListClient(t *testing.T) {
	t.Helper()
	orig := beadsClient
	beadsClient = &mockBeadsClient{
		CommentsByBead: map[string][]*model.Comment{
			"gd-cs1": {
				{BeadID: "gd-cs1", Author: "a", Text: "old-comment", CreatedAt: time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC)},
				{BeadID: "gd-cs1", Author: "b", Text: "boundary-comment", CreatedAt: time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)},
				{BeadID: "gd-cs1", Author: "c", Text: "new-comment", CreatedAt: time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)},
			},
		},
	}
	t.Cleanup(func() { beadsClient = orig })
}

// resetCommentListFlags clears --since flag state between tests; cobra commands
// are package-level singletons so Changed() bookkeeping bleeds across cases.
func resetCommentListFlags(t *testing.T) {
	t.Helper()
	commentSince = ""
	commentListCmd.ResetFlags()
	commentListCmd.Flags().StringVar(&commentSince, "since", "", "only comments created at-or-after this RFC3339 timestamp or duration back from now (4h, 30m, 2d); alias: --after")
	aliasCommentSinceFlags(commentListCmd)
}

func runCommentList(t *testing.T, args []string) string {
	t.Helper()
	return captureStdout(t, func() {
		if err := commentListCmd.RunE(commentListCmd, args); err != nil {
			t.Errorf("comment list returned error: %v", err)
		}
	})
}

func TestCommentList_SinceRFC3339_FiltersAndKeepsBoundary(t *testing.T) {
	setupCommentListClient(t)
	resetCommentListFlags(t)
	if err := commentListCmd.Flags().Set("since", "2026-05-01T09:00:00Z"); err != nil {
		t.Fatal(err)
	}
	out := runCommentList(t, []string{"gd-cs1"})
	if strings.Contains(out, "old-comment") {
		t.Errorf("comment before cutoff must be filtered out:\n%s", out)
	}
	if !strings.Contains(out, "boundary-comment") {
		t.Errorf("comment exactly at cutoff must be included:\n%s", out)
	}
	if !strings.Contains(out, "new-comment") {
		t.Errorf("comment after cutoff must be included:\n%s", out)
	}
}

func TestCommentList_SinceDuration(t *testing.T) {
	setupCommentListClient(t)
	resetCommentListFlags(t)
	// All seeded comments are far in the past relative to time.Now(), so a
	// short duration window excludes everything.
	if err := commentListCmd.Flags().Set("since", "4h"); err != nil {
		t.Fatal(err)
	}
	out := runCommentList(t, []string{"gd-cs1"})
	if !strings.Contains(out, "No comments found.") {
		t.Errorf("expected empty-result message for 4h window over old comments:\n%s", out)
	}
}

func TestCommentList_AfterAlias(t *testing.T) {
	setupCommentListClient(t)
	resetCommentListFlags(t)
	if err := commentListCmd.Flags().Set("after", "2026-05-01T09:30:00Z"); err != nil {
		t.Fatal(err)
	}
	out := runCommentList(t, []string{"gd-cs1"})
	if strings.Contains(out, "boundary-comment") || strings.Contains(out, "old-comment") {
		t.Errorf("--after alias must filter like --since:\n%s", out)
	}
	if !strings.Contains(out, "new-comment") {
		t.Errorf("--after alias must keep newer comments:\n%s", out)
	}
}

func TestCommentList_SinceEmptyResultJSON(t *testing.T) {
	setupCommentListClient(t)
	resetCommentListFlags(t)
	if err := commentListCmd.Flags().Set("since", "2027-01-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	origJSON := jsonOutput
	jsonOutput = true
	t.Cleanup(func() { jsonOutput = origJSON })

	out := runCommentList(t, []string{"gd-cs1"})
	if strings.TrimSpace(out) != "[]" {
		t.Errorf("--json with no matching comments must print [], got:\n%s", out)
	}
}

func TestCommentList_SinceInvalidValueErrors(t *testing.T) {
	setupCommentListClient(t)
	resetCommentListFlags(t)
	if err := commentListCmd.Flags().Set("since", "not-a-time"); err != nil {
		t.Fatal(err)
	}
	err := commentListCmd.RunE(commentListCmd, []string{"gd-cs1"})
	if err == nil || !strings.Contains(err.Error(), "--since") {
		t.Fatalf("expected --since parse error, got %v", err)
	}
}

func TestCommentList_NoSinceListsAll(t *testing.T) {
	setupCommentListClient(t)
	resetCommentListFlags(t)
	out := runCommentList(t, []string{"gd-cs1"})
	for _, want := range []string{"old-comment", "boundary-comment", "new-comment"} {
		if !strings.Contains(out, want) {
			t.Errorf("without --since all comments must print; missing %q:\n%s", want, out)
		}
	}
}
