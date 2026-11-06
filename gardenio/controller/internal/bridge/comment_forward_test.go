package bridge

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

func marshalCommentPayload(comment sseComment) []byte {
	data, _ := json.Marshal(sseCommentPayload{Comment: &comment})
	return data
}

func TestCommentForward_HandleCommentAdded_RelaysToAgent(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["gd-task1"] = &beadsapi.BeadDetail{
		ID:       "gd-task1",
		Assignee: "monorepo-gardener/my-agent",
	}

	cf := &CommentForward{
		daemon: daemon,
		logger: slog.Default(),
		seen:   make(map[int64]bool),
		// bot is nil — Slack post will be skipped, but processing still happens.
	}

	data := marshalCommentPayload(sseComment{
		ID:        42,
		BeadID:    "gd-task1",
		Author:    "monorepo-gardener/my-agent",
		Text:      "Audit findings: everything looks good",
		CreatedAt: time.Now(),
	})
	cf.handleCommentAdded(context.Background(), data)

	if !cf.alreadySeen(42) {
		t.Error("expected comment to be marked as seen after processing")
	}
}

func TestCommentForward_HandleCommentAdded_Dedup(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["gd-task1"] = &beadsapi.BeadDetail{
		ID:       "gd-task1",
		Assignee: "my-agent",
	}

	cf := &CommentForward{
		daemon: daemon,
		logger: slog.Default(),
		seen:   make(map[int64]bool),
	}

	data := marshalCommentPayload(sseComment{
		ID:        99,
		BeadID:    "gd-task1",
		Author:    "my-agent",
		Text:      "First comment",
		CreatedAt: time.Now(),
	})

	cf.handleCommentAdded(context.Background(), data)
	if !cf.alreadySeen(99) {
		t.Error("expected comment to be seen after first call")
	}

	// Record GetBead call count before second call.
	daemon.mu.Lock()
	callsBefore := daemon.getCalls
	daemon.mu.Unlock()

	// Second call should be deduped — no additional GetBead call.
	cf.handleCommentAdded(context.Background(), data)

	daemon.mu.Lock()
	callsAfter := daemon.getCalls
	daemon.mu.Unlock()

	if callsAfter != callsBefore {
		t.Errorf("expected dedup to skip GetBead, but got %d additional calls", callsAfter-callsBefore)
	}
}

func TestCommentForward_HandleCommentAdded_IgnoresMalformedPayload(t *testing.T) {
	cf := &CommentForward{
		daemon: newMockDaemon(),
		logger: slog.Default(),
		seen:   make(map[int64]bool),
	}

	// Malformed JSON should not panic.
	cf.handleCommentAdded(context.Background(), []byte(`{invalid`))

	// Null comment should not panic.
	cf.handleCommentAdded(context.Background(), []byte(`{"comment": null}`))

	// Missing bead_id should not panic.
	data, _ := json.Marshal(sseCommentPayload{Comment: &sseComment{ID: 1}})
	cf.handleCommentAdded(context.Background(), data)
}

func TestCommentForward_HandleCommentAdded_NoAgent(t *testing.T) {
	daemon := newMockDaemon()
	// Parent bead has no assignee and comment author is empty.
	daemon.beads["gd-orphan"] = &beadsapi.BeadDetail{
		ID: "gd-orphan",
	}

	cf := &CommentForward{
		daemon: daemon,
		logger: slog.Default(),
		seen:   make(map[int64]bool),
	}

	data := marshalCommentPayload(sseComment{
		ID:        7,
		BeadID:    "gd-orphan",
		Author:    "",
		Text:      "Orphaned comment",
		CreatedAt: time.Now(),
	})
	cf.handleCommentAdded(context.Background(), data)

	// Should still be marked as seen (processed, just skipped posting).
	if !cf.alreadySeen(7) {
		t.Error("expected comment to be marked as seen even with no agent")
	}
}

func TestCommentForward_HandleCommentAdded_LinksBeadRef(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["gd-linkedref"] = &beadsapi.BeadDetail{
		ID:       "gd-linkedref",
		Assignee: "link-agent",
	}

	srv, calls, mu := capturingSlackServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv)
	bot.beadsViewerURL = "https://viewer.example"
	bot.channel = "C-test"
	bot.threadingMode = "agent"
	bot.agentCards["link-agent"] = MessageRef{ChannelID: "C-test", Timestamp: "1234.5678"}
	enableVibeForAgent(daemon, "link-agent") // full-verbosity path (forwarded comments relay)

	cf := &CommentForward{
		daemon: daemon,
		bot:    bot,
		logger: slog.Default(),
		seen:   make(map[int64]bool),
	}

	data := marshalCommentPayload(sseComment{
		ID:        501,
		BeadID:    "gd-linkedref",
		Author:    "link-agent",
		Text:      "Progress update",
		CreatedAt: time.Now(),
	})
	cf.handleCommentAdded(context.Background(), data)

	got := getCalls(calls, mu)
	if len(got) == 0 {
		t.Fatalf("expected at least one Slack call")
	}
	want := "<https://viewer.example/gd-linkedref|`gd-linkedref`>"
	if !strings.Contains(got[0].Text, want) {
		t.Errorf("expected text to contain link %q, got %q", want, got[0].Text)
	}
}

func TestCommentForward_HandleCommentAdded_FallsBackToAuthor(t *testing.T) {
	daemon := newMockDaemon()
	// Parent bead has no assignee — should fall back to comment author.
	daemon.beads["gd-1jkq5ixzsu"] = &beadsapi.BeadDetail{
		ID: "gd-1jkq5ixzsu",
	}

	cf := &CommentForward{
		daemon: daemon,
		logger: slog.Default(),
		seen:   make(map[int64]bool),
	}

	data := marshalCommentPayload(sseComment{
		ID:        8,
		BeadID:    "gd-1jkq5ixzsu",
		Author:    "gardenio/crew/author-agent",
		Text:      "Comment from author",
		CreatedAt: time.Now(),
	})
	cf.handleCommentAdded(context.Background(), data)

	if !cf.alreadySeen(8) {
		t.Error("expected comment to be processed via author fallback")
	}
}

// A human-authored comment (Author is a human-operator id) is attributed to the
// person by name, not framed as the agent's own comment.
func TestCommentForward_HandleCommentAdded_AttributesHumanCommenter(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["gd-humancmt"] = &beadsapi.BeadDetail{
		ID:       "gd-humancmt",
		Assignee: "thread-agent",
	}

	srv, calls, mu := capturingSlackServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv)
	bot.channel = "C-test"
	bot.threadingMode = "agent"
	bot.agentCards["thread-agent"] = MessageRef{ChannelID: "C-test", Timestamp: "1234.5678"}
	enableVibeForAgent(daemon, "thread-agent") // full-verbosity path (forwarded comments relay)

	cf := &CommentForward{
		daemon: daemon,
		bot:    bot,
		logger: slog.Default(),
		seen:   make(map[int64]bool),
	}

	data := marshalCommentPayload(sseComment{
		ID:                601,
		BeadID:            "gd-humancmt",
		Author:            "slack:U0OPERATOR1",
		Text:              "please rename the button",
		OperatedBy:        "slack:U0OPERATOR1",
		OperatedByDisplay: "Matt Baker",
		CreatedAt:         time.Now(),
	})
	cf.handleCommentAdded(context.Background(), data)

	got := getCalls(calls, mu)
	if len(got) == 0 {
		t.Fatalf("expected at least one Slack call")
	}
	if !strings.Contains(got[0].Text, "Matt Baker") {
		t.Errorf("expected human commenter name in message, got %q", got[0].Text)
	}
	if strings.Contains(got[0].Text, ":memo:") {
		t.Errorf("human comment should not use the agent :memo: framing, got %q", got[0].Text)
	}
}

// An agent-authored comment keeps the original agent-attributed framing — no
// commenter name is injected (that would be noise).
func TestCommentForward_HandleCommentAdded_AgentCommentStaysUnattributed(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["gd-agentcmt"] = &beadsapi.BeadDetail{
		ID:       "gd-agentcmt",
		Assignee: "worker-agent",
	}

	srv, calls, mu := capturingSlackServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv)
	bot.channel = "C-test"
	bot.threadingMode = "agent"
	bot.agentCards["worker-agent"] = MessageRef{ChannelID: "C-test", Timestamp: "1234.5678"}
	enableVibeForAgent(daemon, "worker-agent") // full-verbosity path (forwarded comments relay)

	cf := &CommentForward{
		daemon: daemon,
		bot:    bot,
		logger: slog.Default(),
		seen:   make(map[int64]bool),
	}

	// Agent comment: Author is the bare agent name; OperatedBy carries the
	// spawning human, which must NOT be used to attribute the comment.
	data := marshalCommentPayload(sseComment{
		ID:                602,
		BeadID:            "gd-agentcmt",
		Author:            "worker-agent",
		Text:              "audit complete",
		OperatedBy:        "slack:U0OPERATOR1",
		OperatedByDisplay: "Matt Baker",
		CreatedAt:         time.Now(),
	})
	cf.handleCommentAdded(context.Background(), data)

	got := getCalls(calls, mu)
	if len(got) == 0 {
		t.Fatalf("expected at least one Slack call")
	}
	if strings.Contains(got[0].Text, "Matt Baker") {
		t.Errorf("agent comment should not be attributed to the spawning human, got %q", got[0].Text)
	}
	if !strings.Contains(got[0].Text, ":memo:") {
		t.Errorf("agent comment should keep the :memo: framing, got %q", got[0].Text)
	}
}

func TestIsHumanCommentAuthor(t *testing.T) {
	cases := []struct {
		author string
		want   bool
	}{
		{"slack:U0OPERATOR1", true},
		{"oidc:matt@acme.example", true},
		{"matthew.baker@acme.example", true},
		{"thread-1780683188-697789", false},
		{"gardenio/crew/author-agent", false},
		{"monorepo-gardener-kbfz", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isHumanCommentAuthor(tc.author); got != tc.want {
			t.Errorf("isHumanCommentAuthor(%q) = %v, want %v", tc.author, got, tc.want)
		}
	}
}

func TestHumanCommenterName_ResolutionOrder(t *testing.T) {
	// Display name wins when present.
	cf := &CommentForward{}
	got := cf.humanCommenterName(&sseComment{
		Author:            "slack:U0OPERATOR1",
		OperatedBy:        "slack:U0OPERATOR1",
		OperatedByDisplay: "Matt Baker",
		OperatedByEmail:   "matthew.baker@acme.example",
	})
	if got != "Matt Baker" {
		t.Errorf("expected display name to win, got %q", got)
	}

	// Email is used when no display name and no bot to resolve a Slack id.
	got = cf.humanCommenterName(&sseComment{
		Author:          "oidc:matt@acme.example",
		OperatedBy:      "oidc:matt@acme.example",
		OperatedByEmail: "matt@acme.example",
	})
	if got != "matt@acme.example" {
		t.Errorf("expected email fallback, got %q", got)
	}

	// Bare-email author with nothing else falls back to the author itself.
	got = cf.humanCommenterName(&sseComment{Author: "matthew.baker@acme.example"})
	if got != "matthew.baker@acme.example" {
		t.Errorf("expected author fallback, got %q", got)
	}

	// Agent author yields no name.
	if got := cf.humanCommenterName(&sseComment{Author: "worker-agent"}); got != "" {
		t.Errorf("expected empty name for agent author, got %q", got)
	}
}

// TestQuietScheduledWrapUp: only a trigger-fired run's wrap-up that says
// progress_score 0 and no blocker is held back from Slack (gd-rar4hYRbGC).
func TestQuietScheduledWrapUp(t *testing.T) {
	const wrap = "gb stop wrap-up:\n\nTLDR: Preflight clean: queued=0 in_progress=0. No action."
	agent := func(fields map[string]string) *beadsapi.BeadDetail {
		return &beadsapi.BeadDetail{ID: "gd-cicd1", Type: "agent", Fields: fields}
	}
	quiet := `{"tldr":"Preflight clean","progress_score":0}`
	cases := []struct {
		name  string
		agent *beadsapi.BeadDetail
		text  string
		want  bool
	}{
		{"webhook trigger, no progress", agent(map[string]string{"trigger_id": "gd-trig1", "wrapup": quiet}), wrap, true},
		{"schedule, no progress", agent(map[string]string{"spawn_source": "schedule", "wrapup": quiet}), wrap, true},
		{"schedule, progress", agent(map[string]string{"spawn_source": "schedule", "wrapup": `{"progress_score":1}`}), wrap, false},
		{"schedule, blocker", agent(map[string]string{"spawn_source": "schedule", "wrapup": `{"progress_score":0,"blockers":"CI red on main"}`}), wrap, false},
		{"schedule, no score", agent(map[string]string{"spawn_source": "schedule", "wrapup": `{"tldr":"x"}`}), wrap, false},
		{"schedule, bad wrapup", agent(map[string]string{"spawn_source": "schedule", "wrapup": "{"}), wrap, false},
		{"crew run", agent(map[string]string{"spawn_source": "slack-thread", "wrapup": quiet}), wrap, false},
		{"other comment", agent(map[string]string{"trigger_id": "gd-trig1", "wrapup": quiet}), "found a flaky test", false},
		{"task bead", &beadsapi.BeadDetail{ID: "gd-task1", Type: "task", Fields: map[string]string{"trigger_id": "gd-trig1", "wrapup": quiet}}, wrap, false},
		{"no bead", nil, wrap, false},
	}
	for _, c := range cases {
		if got := quietScheduledWrapUp(c.agent, c.text); got != c.want {
			t.Errorf("%s: quietScheduledWrapUp = %v, want %v", c.name, got, c.want)
		}
	}
}
