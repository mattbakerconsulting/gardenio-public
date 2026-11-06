package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

var (
	errTestSpawn   = errors.New("test: SpawnAgent failed (simulated webhook-time dispatch failure)")
	errTestComment = errors.New("test: AddComment failed")
)

// Tracks A2 + A3 of plan ka-0000004007 (patrick-incident fixes).
//
// A2: when the webhook-time SpawnAgent fails after the task bead was already
// created, the drop must be operator-VISIBLE — a task-bead comment (forwarded
// to Slack) — so a human is never left wondering why the bot went silent, the
// exact silence Patrick hit on !1160.
//
// A3: when the webhook finds a live agent on the MR and NUDGES it instead of
// spawning a new one, the task it created has no linked agent bead. It must be
// stamped (nudged_agent) so the A1 catchup watchdog does not later mistake it
// for an orphan and respawn a redundant crew.

// TestHandleMention_A2_PostsCommentOnSpawnFailure asserts the dispatch-failure
// signal: SpawnAgent fails → HandleMention returns false (unhandled) AND an
// operator-visible comment lands on the task bead.
func TestHandleMention_A2_PostsCommentOnSpawnFailure(t *testing.T) {
	daemon := &mockMentionDaemon{
		createID: "gd-a2-task",
		spawnErr: errTestSpawn,
	}
	h := NewMentionHandler(MentionHandlerConfig{
		Daemon:  daemon,
		Trigger: "@gardenio",
		Project: "gardenio",
		Users:   testUserLookup(),
		Logger:  slog.Default(),
	})

	nc := noteContext{
		NoteableType: "MergeRequest",
		Note:         "@gardenio fix the flaky test",
		Author:       "reviewer",
		MR:           &webhookMR{IID: 1160, URL: "https://gitlab.com/group/project/-/merge_requests/1160"},
		DiscussionID: "disc-a2",
	}

	handled := h.HandleMention(context.Background(), nc)
	if handled {
		t.Fatal("expected HandleMention to return false when SpawnAgent fails")
	}

	// The task bead was created (the CreateBead half succeeded).
	if len(daemon.created) != 1 {
		t.Fatalf("expected 1 task bead created, got %d", len(daemon.created))
	}
	// No agent spawned.
	if len(daemon.spawned) != 0 {
		t.Fatalf("expected 0 agents spawned, got %d", len(daemon.spawned))
	}
	// The operator-visible failure comment landed on the task bead.
	if len(daemon.comments) != 1 {
		t.Fatalf("expected 1 dispatch-failure comment, got %d", len(daemon.comments))
	}
	c := daemon.comments[0]
	if c.BeadID != "gd-a2-task" {
		t.Errorf("comment bead = %q, want gd-a2-task", c.BeadID)
	}
	if c.Author != mentionDispatchActor {
		t.Errorf("comment author = %q, want %q", c.Author, mentionDispatchActor)
	}
	if !strings.Contains(strings.ToLower(c.Text), "failed") {
		t.Errorf("comment %q should mention the failure", c.Text)
	}
	// It must name the catchup watchdog so the operator knows recovery is
	// automatic (the request is not lost).
	if !strings.Contains(c.Text, "ka-0000000279") {
		t.Errorf("comment %q should reference the catchup watchdog", c.Text)
	}
}

// TestHandleMention_A2_CommentFailureDoesNotMaskSpawnError asserts the failure
// signal is best-effort: a comment error must not change the returned outcome
// (still unhandled) — a flaky notify path can never become a new silent-drop
// vector.
func TestHandleMention_A2_CommentFailureDoesNotMaskSpawnError(t *testing.T) {
	daemon := &mockMentionDaemon{
		createID:   "gd-a2-task",
		spawnErr:   errTestSpawn,
		commentErr: errTestComment,
	}
	h := NewMentionHandler(MentionHandlerConfig{
		Daemon:  daemon,
		Trigger: "@gardenio",
		Project: "gardenio",
		Users:   testUserLookup(),
		Logger:  slog.Default(),
	})

	nc := noteContext{
		NoteableType: "MergeRequest",
		Note:         "@gardenio address review",
		Author:       "reviewer",
		MR:           &webhookMR{IID: 1160, URL: "https://gitlab.com/group/project/-/merge_requests/1160"},
	}

	if handled := h.HandleMention(context.Background(), nc); handled {
		t.Fatal("expected HandleMention to return false when SpawnAgent fails, regardless of comment error")
	}
}

// TestHandleMention_A3_StampsNudgedAgent asserts that when the webhook nudges a
// live agent instead of spawning, the task bead is stamped with nudged_agent so
// the A1 watchdog can subtract it from the orphan set.
func TestHandleMention_A3_StampsNudgedAgent(t *testing.T) {
	daemon := &mockMentionDaemon{createID: "gd-a3-task"}

	// A coop server that accepts the nudge (so the webhook takes the
	// nudge-success branch and never spawns).
	coopServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]bool{"delivered": true})
	}))
	defer coopServer.Close()

	// A live agent bound to this MR so FindActiveAgentForMR returns it.
	mrURL := "https://gitlab.com/group/project/-/merge_requests/777"
	resolverDaemon := newMockResolverDaemon()
	resolverDaemon.agentBeads["mr-777-live"] = &beadsapi.BeadDetail{
		ID:    "bd-mr-777-live",
		Title: "mr-777-live",
		Type:  "agent",
		Notes: "coop_url: " + coopServer.URL,
		Fields: map[string]string{
			"agent":         "mr-777-live",
			"gitlab_mr_url": mrURL,
		},
	}
	resolver := newTestAgentResolver(resolverDaemon, nil)

	h := NewMentionHandler(MentionHandlerConfig{
		Daemon:   daemon,
		Resolver: resolver,
		Nudge:    resolver.deliverNudge,
		Trigger:  "@gardenio",
		Project:  "gardenio",
		Users:    testUserLookup(),
		Logger:   slog.Default(),
	})

	nc := noteContext{
		NoteableType: "MergeRequest",
		Note:         "@gardenio one more nit",
		Author:       "reviewer",
		MR:           &webhookMR{IID: 777, URL: mrURL},
	}

	if handled := h.HandleMention(context.Background(), nc); !handled {
		t.Fatal("expected mention to be handled via nudge")
	}
	// No new agent spawned — the nudge covered it.
	if len(daemon.spawned) != 0 {
		t.Fatalf("expected 0 agents spawned on nudge-success, got %d", len(daemon.spawned))
	}
	// The task bead was stamped with nudged_agent.
	if len(daemon.fieldUpdates) != 1 {
		t.Fatalf("expected 1 field update stamping nudged_agent, got %d", len(daemon.fieldUpdates))
	}
	up := daemon.fieldUpdates[0]
	if up.BeadID != "gd-a3-task" {
		t.Errorf("field update bead = %q, want gd-a3-task", up.BeadID)
	}
	if up.Fields["nudged_agent"] != "mr-777-live" {
		t.Errorf("nudged_agent = %q, want mr-777-live", up.Fields["nudged_agent"])
	}
	if up.Fields["nudge_delivered_at"] == "" {
		t.Error("nudge_delivered_at should be stamped")
	}
}
