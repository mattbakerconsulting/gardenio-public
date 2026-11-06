package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// ── gb stop does not re-file a PR sent back at its current head (gd-bV2iypX2mD) ──

const sentBackHead = "374c99e67888bb8b47d8c26e806dc94e3a675493"

// fakeSentBackPR fakes gh for parkPRURL: state reads answer an open ready PR at
// sentBackHead, comment reads answer comments (or commentsErr), and every gb
// call is recorded.
func fakeSentBackPR(t *testing.T, comments []prVerdictComment, commentsErr error) *[][]string {
	t.Helper()
	calls := fakePRPark(t, nil)
	prGHRun = func(_ context.Context, args ...string) (string, error) {
		if len(args) < 3 || args[0] != "pr" || args[1] != "view" || args[2] != parkPRURL {
			return "", fmt.Errorf("no pull requests found for branch")
		}
		if args[len(args)-1] == "comments" {
			if commentsErr != nil {
				return "", commentsErr
			}
			b, _ := json.Marshal(map[string]any{"comments": comments})
			return string(b), nil
		}
		return fmt.Sprintf(`{"number":42,"url":%q,"title":"Fix the thing","state":"OPEN","isDraft":false,"headRefName":"agent/fix","headRefOid":%q}`,
			parkPRURL, sentBackHead), nil
	}
	return calls
}

func verdict(at, line string) prVerdictComment {
	return prVerdictComment{Body: line + "\n\nRequired changes:\n- fix it", CreatedAt: at, URL: parkPRURL + "#issuecomment-" + at[11:13]}
}

func TestParkOpenPRs_SentBackAtHeadFilesNothing(t *testing.T) {
	calls := fakeSentBackPR(t, []prVerdictComment{
		{Body: "Looks close.", CreatedAt: "2026-11-02T20:00:00Z"},
		verdict("2026-11-02T21:00:00Z", "Verdict: sent back (merge desk gd-desk, model), at "+sentBackHead),
		{Body: "Thanks, on it.", CreatedAt: "2026-11-02T22:00:00Z"},
	}, nil)
	results := parkOpenPRs(context.Background(), parkDaemon(), parkAgent, "crew-a", nil)
	if len(*calls) != 0 {
		t.Fatalf("sent back at the current head: want no gb call, got %v", *calls)
	}
	if len(results) != 1 || results[0].Filed || results[0].Task != parkTask || !strings.HasSuffix(results[0].SentBack, "#issuecomment-21") {
		t.Fatalf("want one sent-back result for %s, got %+v", parkTask, results)
	}
	if got := heldPRTasks(results); len(got) != 0 {
		t.Errorf("heldPRTasks = %v, want empty: a send-back must not pass the claimed-work gate", got)
	}
	note := prParkNote(results)
	for _, want := range []string{"sent back " + parkPRURL + " at its current head 374c99e", "gb pr ready", "kd unclaim " + parkTask} {
		if !strings.Contains(note, want) {
			t.Errorf("note %q missing %q", note, want)
		}
	}
}

func TestParkOpenPRs_SentBackShortSHAFilesNothing(t *testing.T) {
	calls := fakeSentBackPR(t, []prVerdictComment{
		verdict("2026-11-02T21:00:00Z", "Verdict: sent back (merge desk gd-desk), reviewed at 374c99e6"),
	}, nil)
	results := parkOpenPRs(context.Background(), parkDaemon(), parkAgent, "crew-a", nil)
	if len(*calls) != 0 || len(results) != 1 || results[0].SentBack == "" {
		t.Fatalf("short-sha send-back at head: want skipped, got %+v, gb calls %v", results, *calls)
	}
}

func TestParkOpenPRs_SentBackFilesAsBefore(t *testing.T) {
	cases := map[string]struct {
		comments []prVerdictComment
		err      error
	}{
		"older sha": {comments: []prVerdictComment{
			verdict("2026-11-02T21:00:00Z", "Verdict: sent back (merge desk gd-desk), at 2dbf296a9aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		}},
		"newer mergeable verdict": {comments: []prVerdictComment{
			verdict("2026-11-02T22:00:00Z", "Verdict: mergeable (merge desk gd-desk), at "+sentBackHead),
			verdict("2026-11-02T21:00:00Z", "Verdict: sent back (merge desk gd-desk), at "+sentBackHead),
		}},
		"too-short sha": {comments: []prVerdictComment{
			verdict("2026-11-02T21:00:00Z", "Verdict: sent back (merge desk gd-desk), at 374c99"),
		}},
		"gh comments read error": {err: fmt.Errorf("gh: rate limited")},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			calls := fakeSentBackPR(t, c.comments, c.err)
			results := parkOpenPRs(context.Background(), parkDaemon(), parkAgent, "crew-a", nil)
			if len(results) != 1 || !results[0].Filed || results[0].SentBack != "" || len(*calls) != 1 {
				t.Fatalf("want one filed escalation, got %+v, gb calls %v", results, *calls)
			}
		})
	}
}
