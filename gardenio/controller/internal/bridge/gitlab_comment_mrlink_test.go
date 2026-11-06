package bridge

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

func marshalMRCommentPayload(c mrCommentBody) []byte {
	data, _ := json.Marshal(mrCommentPayload{Comment: &c})
	return data
}

func newMRCommentLinkerWith(daemon MRCommentLinkerDaemon) *MRCommentLinker {
	return &MRCommentLinker{
		daemon: daemon,
		logger: slog.Default(),
		seen:   make(map[int64]bool),
	}
}

func TestMRCommentLinker_BackfillsFromMROpenedComment(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["gd-task1"] = &beadsapi.BeadDetail{ID: "gd-task1", Type: "task"}

	l := newMRCommentLinkerWith(daemon)

	const mrURL = "https://gitlab.com/Acme/Platform/monorepo/-/merge_requests/916"
	data := marshalMRCommentPayload(mrCommentBody{
		ID:     1,
		BeadID: "gd-task1",
		Author: "some-agent",
		Text:   "MR opened: " + mrURL,
	})
	l.handleCommentAdded(context.Background(), data)

	if got := daemon.beads["gd-task1"].Fields["mr_url"]; got != mrURL {
		t.Fatalf("mr_url = %q, want %q", got, mrURL)
	}
}

func TestMRCommentLinker_ExtractsURLFromSurroundingProse(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["gd-task1"] = &beadsapi.BeadDetail{ID: "gd-task1", Type: "bug"}

	l := newMRCommentLinkerWith(daemon)

	const mrURL = "https://gitlab.com/org/repo/-/merge_requests/42"
	// URL embedded mid-sentence with trailing punctuation that must be excluded.
	data := marshalMRCommentPayload(mrCommentBody{
		ID:     2,
		BeadID: "gd-task1",
		Text:   "Opened the fix at " + mrURL + ", please review.",
	})
	l.handleCommentAdded(context.Background(), data)

	if got := daemon.beads["gd-task1"].Fields["mr_url"]; got != mrURL {
		t.Fatalf("mr_url = %q, want %q (trailing comma must be stripped)", got, mrURL)
	}
}

func TestMRCommentLinker_NoURL_NoWrite(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["gd-task1"] = &beadsapi.BeadDetail{ID: "gd-task1", Type: "task"}

	l := newMRCommentLinkerWith(daemon)

	daemon.mu.Lock()
	callsBefore := daemon.getCalls
	daemon.mu.Unlock()

	data := marshalMRCommentPayload(mrCommentBody{
		ID:     3,
		BeadID: "gd-task1",
		Text:   "Just a regular comment with no MR link in it.",
	})
	l.handleCommentAdded(context.Background(), data)

	if got := daemon.beads["gd-task1"].Fields["mr_url"]; got != "" {
		t.Fatalf("mr_url = %q, want empty (no URL in comment)", got)
	}
	// Should bail before even reading the bead.
	daemon.mu.Lock()
	callsAfter := daemon.getCalls
	daemon.mu.Unlock()
	if callsAfter != callsBefore {
		t.Errorf("GetBead called %d times, want 0 (should short-circuit on no URL)", callsAfter-callsBefore)
	}
}

func TestMRCommentLinker_NonMRGitlabURL_Ignored(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["gd-task1"] = &beadsapi.BeadDetail{ID: "gd-task1", Type: "task"}

	l := newMRCommentLinkerWith(daemon)

	// An issue URL, not a merge_requests URL — must not match.
	data := marshalMRCommentPayload(mrCommentBody{
		ID:     4,
		BeadID: "gd-task1",
		Text:   "see https://gitlab.com/org/repo/-/issues/7 for context",
	})
	l.handleCommentAdded(context.Background(), data)

	if got := daemon.beads["gd-task1"].Fields["mr_url"]; got != "" {
		t.Fatalf("mr_url = %q, want empty (issue URL is not an MR URL)", got)
	}
}

func TestMRCommentLinker_AlreadyLinked_NoClobber(t *testing.T) {
	daemon := newMockDaemon()
	const existing = "https://gitlab.com/org/repo/-/merge_requests/100"
	daemon.beads["gd-task1"] = &beadsapi.BeadDetail{
		ID:     "gd-task1",
		Type:   "task",
		Fields: map[string]string{"mr_url": existing},
	}

	l := newMRCommentLinkerWith(daemon)

	data := marshalMRCommentPayload(mrCommentBody{
		ID:     5,
		BeadID: "gd-task1",
		Text:   "MR opened: https://gitlab.com/org/repo/-/merge_requests/200",
	})
	l.handleCommentAdded(context.Background(), data)

	if got := daemon.beads["gd-task1"].Fields["mr_url"]; got != existing {
		t.Fatalf("mr_url = %q, want %q (must not clobber existing link)", got, existing)
	}
}

func TestMRCommentLinker_NonWorkBeadType_Skipped(t *testing.T) {
	for _, typ := range []string{"merge-request", "agent", "schedule", "trigger"} {
		t.Run(typ, func(t *testing.T) {
			daemon := newMockDaemon()
			daemon.beads["gd-x"] = &beadsapi.BeadDetail{ID: "gd-x", Type: typ}

			l := newMRCommentLinkerWith(daemon)

			data := marshalMRCommentPayload(mrCommentBody{
				ID:     6,
				BeadID: "gd-x",
				Text:   "MR opened: https://gitlab.com/org/repo/-/merge_requests/9",
			})
			l.handleCommentAdded(context.Background(), data)

			if got := daemon.beads["gd-x"].Fields["mr_url"]; got != "" {
				t.Fatalf("mr_url = %q on type=%s, want empty (non-work bead must be skipped)", got, typ)
			}
		})
	}
}

func TestMRCommentLinker_Dedup(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["gd-task1"] = &beadsapi.BeadDetail{ID: "gd-task1", Type: "task"}

	l := newMRCommentLinkerWith(daemon)

	data := marshalMRCommentPayload(mrCommentBody{
		ID:     7,
		BeadID: "gd-task1",
		Text:   "MR opened: https://gitlab.com/org/repo/-/merge_requests/55",
	})
	l.handleCommentAdded(context.Background(), data)

	daemon.mu.Lock()
	callsBefore := daemon.getCalls
	daemon.mu.Unlock()

	// Redelivery of the same comment (SSE reconnect) must not re-process.
	l.handleCommentAdded(context.Background(), data)

	daemon.mu.Lock()
	callsAfter := daemon.getCalls
	daemon.mu.Unlock()
	if callsAfter != callsBefore {
		t.Errorf("GetBead called again on dedup'd comment: before=%d after=%d", callsBefore, callsAfter)
	}
}

func TestMRCommentLinker_EmptyBeadID_Ignored(t *testing.T) {
	daemon := newMockDaemon()
	l := newMRCommentLinkerWith(daemon)

	// Missing bead_id — must be a no-op (and not panic).
	data := marshalMRCommentPayload(mrCommentBody{
		ID:   8,
		Text: "MR opened: https://gitlab.com/org/repo/-/merge_requests/3",
	})
	l.handleCommentAdded(context.Background(), data)
}

func TestCommentMRURLPattern_Matches(t *testing.T) {
	cases := []struct {
		text string
		want string
	}{
		{"MR opened: https://gitlab.com/a/b/-/merge_requests/1", "https://gitlab.com/a/b/-/merge_requests/1"},
		{"https://gitlab.com/a/b/-/merge_requests/12).", "https://gitlab.com/a/b/-/merge_requests/12"},
		{"<https://gitlab.com/a/b/-/merge_requests/12>", "https://gitlab.com/a/b/-/merge_requests/12"},
		{"deep https://gitlab.com/Acme/Platform/x/y/-/merge_requests/999 path", "https://gitlab.com/Acme/Platform/x/y/-/merge_requests/999"},
		{"no url here", ""},
		{"https://gitlab.com/a/b/-/issues/1", ""},
	}
	for _, c := range cases {
		if got := commentMRURLPattern.FindString(c.text); got != c.want {
			t.Errorf("FindString(%q) = %q, want %q", c.text, got, c.want)
		}
	}
}
