package bridge

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"

	"github.com/slack-go/slack/slackevents"
)

// --- parseMentionCommand tests for the new "summarize" keyword ---

func TestParseMentionCommand_Summarize(t *testing.T) {
	cmd, remaining := parseMentionCommand("summarize")
	if cmd != "summarize" {
		t.Errorf("expected cmd=summarize, got %q", cmd)
	}
	if remaining != "" {
		t.Errorf("expected empty remaining, got %q", remaining)
	}
}

func TestParseMentionCommand_SummarizeCaseInsensitive(t *testing.T) {
	cmd, _ := parseMentionCommand("SUMMARIZE")
	if cmd != "summarize" {
		t.Errorf("expected cmd=summarize, got %q", cmd)
	}
}

// --- buildSummaryTLDR pure-function tests ---

func TestBuildSummaryTLDR_NoAgentNoMessages(t *testing.T) {
	out := buildSummaryTLDR(summaryInputs{}, summarizeMaxChars)
	if !strings.Contains(out, "no agent bound") {
		t.Errorf("expected 'no agent bound' header, got %q", out)
	}
	if !strings.Contains(out, "no recent activity") {
		t.Errorf("expected 'no recent activity' note, got %q", out)
	}
}

func TestBuildSummaryTLDR_WithAgentAndMessages(t *testing.T) {
	in := summaryInputs{
		Agent:  "thread-1234-5678",
		BeadID: "ka-0000004973",
		Status: "running",
		Messages: []summaryMessage{
			{Author: "U001", Text: "hey can you fix the deploy"},
			{Author: "U002", Text: "looking now, see https://gitlab.com/foo/bar/-/merge_requests/42"},
			{BotID: "B999", Author: "bot", Text: "noise"},
		},
	}
	out := buildSummaryTLDR(in, summarizeMaxChars)

	if !strings.Contains(out, "thread-1234-5678") {
		t.Errorf("expected agent name, got %q", out)
	}
	if !strings.Contains(out, "(running)") {
		t.Errorf("expected status, got %q", out)
	}
	if !strings.Contains(out, "U001") || !strings.Contains(out, "U002") {
		t.Errorf("expected user mentions, got %q", out)
	}
	if strings.Contains(out, "noise") {
		t.Errorf("bot messages should be filtered, got %q", out)
	}
	if !strings.Contains(out, "ka-0000004973") {
		t.Errorf("expected bead link, got %q", out)
	}
	if !strings.Contains(out, "merge_requests/42") {
		t.Errorf("expected MR url, got %q", out)
	}
}

func TestBuildSummaryTLDR_EnforcesCharBudget(t *testing.T) {
	long := strings.Repeat("x", 1000)
	in := summaryInputs{
		Agent: "agent-x",
		Messages: []summaryMessage{
			{Author: "U001", Text: long},
			{Author: "U002", Text: long},
		},
	}
	out := buildSummaryTLDR(in, 500)
	if len(out) > 500 {
		t.Errorf("output exceeds 500 char budget: %d chars", len(out))
	}
	if !strings.HasSuffix(out, "…") {
		tailStart := 0
		if len(out) > 10 {
			tailStart = len(out) - 10
		}
		t.Errorf("expected ellipsis suffix on truncation, got tail %q", out[tailStart:])
	}
}

func TestBuildSummaryTLDR_DedupesLinks(t *testing.T) {
	in := summaryInputs{
		BeadID: "gd-IaBnvV2xMR",
		Messages: []summaryMessage{
			{Author: "U001", Text: "see gd-IaBnvV2xMR and gd-Other02YY"},
			{Author: "U002", Text: "gd-IaBnvV2xMR again"},
		},
	}
	out := buildSummaryTLDR(in, summarizeMaxChars)
	count := strings.Count(out, "gd-IaBnvV2xMR")
	if count != 1 {
		t.Errorf("expected gd-IaBnvV2xMR once, got %d times in %q", count, out)
	}
	if !strings.Contains(out, "gd-Other02YY") {
		t.Errorf("expected second bead link, got %q", out)
	}
}

func TestBuildSummaryTLDR_IncludesCommentsFromBead(t *testing.T) {
	in := summaryInputs{
		Agent:  "thread-x",
		BeadID: "gd-8YFoANEnZp",
		Comments: []beadsapi.Comment{
			{Author: "agent", Text: "merged the fix in !1234", CreatedAt: time.Unix(100, 0)},
		},
	}
	out := buildSummaryTLDR(in, summarizeMaxChars)
	if !strings.Contains(out, "merged the fix") {
		t.Errorf("expected comment text, got %q", out)
	}
	if !strings.Contains(out, "(bead)") {
		t.Errorf("expected (bead) annotation on comment, got %q", out)
	}
}

// --- trimComments helper ---

func TestTrimComments_KeepsMostRecent(t *testing.T) {
	now := time.Now()
	comments := []beadsapi.Comment{
		{ID: 1, Text: "first", CreatedAt: now.Add(-3 * time.Hour)},
		{ID: 2, Text: "second", CreatedAt: now.Add(-2 * time.Hour)},
		{ID: 3, Text: "third", CreatedAt: now.Add(-1 * time.Hour)},
	}
	got := trimComments(comments, 2)
	if len(got) != 2 {
		t.Fatalf("expected 2 comments, got %d", len(got))
	}
	if got[0].ID != 2 || got[1].ID != 3 {
		t.Errorf("expected ids [2,3], got [%d,%d]", got[0].ID, got[1].ID)
	}
}

// --- handleMentionSummarize integration test ---

func TestHandleMentionSummarize_PostsTLDRInThread(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	tmpDir := t.TempDir()
	sm, err := NewStateManager(filepath.Join(tmpDir, "state.json"))
	if err != nil {
		t.Fatalf("failed to create state manager: %v", err)
	}
	bot.state = sm

	// Bind an agent to the thread, with a comment on its bead.
	_ = sm.SetThreadAgent("C123", "1111.2222", "thread-1111-2222")
	daemon.mu.Lock()
	daemon.beads["thread-1111-2222"] = &beadsapi.BeadDetail{
		ID:     "bd-thread-agent",
		Title:  "thread-1111-2222",
		Type:   "agent",
		Fields: map[string]string{"agent": "thread-1111-2222", "agent_state": "running"},
	}
	if daemon.beadComments == nil {
		daemon.beadComments = map[string][]beadsapi.Comment{}
	}
	daemon.beadComments["bd-thread-agent"] = []beadsapi.Comment{
		{Text: "made progress on gd-gOt6Fnj7bl", CreatedAt: time.Unix(100, 0)},
	}
	daemon.mu.Unlock()

	ev := &slackevents.AppMentionEvent{
		Channel:         "C123",
		ThreadTimeStamp: "1111.2222",
		User:            "U456",
	}

	// Should not panic and should resolve agent + comments.
	bot.handleMentionSummarize(context.Background(), ev)
}

func TestHandleMentionSummarize_NoAgentBoundStillPosts(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	tmpDir := t.TempDir()
	sm, err := NewStateManager(filepath.Join(tmpDir, "state.json"))
	if err != nil {
		t.Fatalf("failed to create state manager: %v", err)
	}
	bot.state = sm

	ev := &slackevents.AppMentionEvent{
		Channel:         "C123",
		ThreadTimeStamp: "1111.2222",
		User:            "U456",
	}

	// Should not panic when no agent is bound — TLDR should still post.
	bot.handleMentionSummarize(context.Background(), ev)
}

