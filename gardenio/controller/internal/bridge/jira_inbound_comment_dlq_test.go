package bridge

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"gardenio/controller/internal/beadsapi"
)

// newDLQState builds a real temp-file-backed StateManager so the DLQ tests
// exercise the actual persistence path (survives-a-crash contract), not an
// in-memory stand-in.
func newDLQState(t *testing.T) *StateManager {
	t.Helper()
	state, err := NewStateManager(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("NewStateManager: %v", err)
	}
	return state
}

// TestJiraInboundCommentDLQ_EnqueueOnTransientTerminalFailure proves that when
// the inbound comment mirror EXHAUSTS its bounded inline retry with a transient
// error, the mirror is queued to the durable DLQ (rather than silently dropped),
// with the enqueue counter incremented and the entry persisted to state.
func TestJiraInboundCommentDLQ_EnqueueOnTransientTerminalFailure(t *testing.T) {
	daemon := newMockJiraWebhookDaemon()
	daemon.beads["bead-1"] = &beadsapi.BeadDetail{
		ID:       "bead-1",
		Status:   "in_progress",
		Assignee: "test-agent",
		Fields:   map[string]string{"jira_key": "PE-1234"},
	}
	// Every add_comment attempt fails transiently → terminal failure after the
	// bounded retry.
	daemon.addCommentErrs = []error{transientDaemonErr(), transientDaemonErr(), transientDaemonErr()}

	state := newDLQState(t)
	dlq := NewJiraInboundCommentDLQ(state, daemon, slog.Default())
	if dlq == nil {
		t.Fatal("NewJiraInboundCommentDLQ returned nil for non-nil deps")
	}

	enqBefore := testutil.ToFloat64(jiraInboundCommentDLQEnqueued)

	handler := JiraWebhookHandler(JiraWebhookConfig{
		Daemon:            daemon,
		Nudge:             func(_ context.Context, _, _ string) error { return nil },
		DevMode:           true,
		InboundCommentDLQ: dlq,
		Logger:            slog.Default(),
	})

	body := makeJiraWebhookPayload("comment_created", "PE-1234", "please fix the thing", "John Doe")
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	// One mirror enqueued to the DLQ.
	if got := testutil.ToFloat64(jiraInboundCommentDLQEnqueued) - enqBefore; got != 1 {
		t.Errorf("expected 1 DLQ enqueue, got %v", got)
	}
	// The entry is durably in state, keyed by (bead, comment id).
	entries := state.AllJiraInboundCommentDLQ()
	if len(entries) != 1 {
		t.Fatalf("expected 1 queued entry, got %d", len(entries))
	}
	e := entries[0]
	if e.BeadID != "bead-1" || e.JiraKey != "PE-1234" || e.JiraComment != "12345" {
		t.Errorf("unexpected entry: %+v", e)
	}
	if wantKey := jiraInboundCommentDLQKey("bead-1", "12345", e.CommentText); e.Key != wantKey {
		t.Errorf("key = %q, want %q", e.Key, wantKey)
	}
	// EnqueuedAt must be stamped (not the zero time) so the 500-cap oldest-first
	// eviction has a real ordering key — parity with enqueueThreadReplyDLQ.
	if e.EnqueuedAt.IsZero() {
		t.Error("EnqueuedAt not stamped on enqueue — oldest-eviction ordering would be arbitrary")
	}
}

// TestJiraInboundCommentDLQ_NilDLQPreservesDropBehavior proves that a webhook
// configured WITHOUT a DLQ keeps the exact pre-DLQ behavior: a terminal transient
// failure is counted and dropped, and enqueue is not called (nil-safe).
func TestJiraInboundCommentDLQ_NilDLQPreservesDropBehavior(t *testing.T) {
	daemon := newMockJiraWebhookDaemon()
	daemon.beads["bead-1"] = &beadsapi.BeadDetail{
		ID:     "bead-1",
		Status: "in_progress",
		Fields: map[string]string{"jira_key": "PE-1234"},
	}
	daemon.addCommentErrs = []error{transientDaemonErr(), transientDaemonErr(), transientDaemonErr()}

	enqBefore := testutil.ToFloat64(jiraInboundCommentDLQEnqueued)
	failBefore := testutil.ToFloat64(jiraInboundCommentWriteFailures.WithLabelValues("add_comment"))

	handler := JiraWebhookHandler(JiraWebhookConfig{
		Daemon:  daemon,
		Nudge:   func(_ context.Context, _, _ string) error { return nil },
		DevMode: true,
		// InboundCommentDLQ deliberately nil.
		Logger: slog.Default(),
	})

	body := makeJiraWebhookPayload("comment_created", "PE-1234", "hi", "John Doe")
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if got := testutil.ToFloat64(jiraInboundCommentWriteFailures.WithLabelValues("add_comment")) - failBefore; got != 1 {
		t.Errorf("expected 1 terminal-failure increment, got %v", got)
	}
	// A nil DLQ counts a no_state drop and never enqueues.
	if got := testutil.ToFloat64(jiraInboundCommentDLQEnqueued) - enqBefore; got != 0 {
		t.Errorf("expected 0 DLQ enqueues with nil DLQ, got %v", got)
	}
}

// TestJiraInboundCommentDLQ_DrainReplaysAndDequeues proves the drain loop
// re-posts a queued mirror once the daemon recovers, sets the reply-anchor
// fields, and dequeues the entry.
func TestJiraInboundCommentDLQ_DrainReplaysAndDequeues(t *testing.T) {
	daemon := newMockJiraWebhookDaemon()
	daemon.beads["bead-1"] = &beadsapi.BeadDetail{
		ID:     "bead-1",
		Status: "in_progress",
		Fields: map[string]string{"jira_key": "PE-1234"},
	}
	state := newDLQState(t)
	dlq := NewJiraInboundCommentDLQ(state, daemon, slog.Default())

	// Seed a queued mirror directly (as if a prior webhook terminally failed).
	dlq.enqueue(JiraInboundCommentDLQEntry{
		BeadID:         "bead-1",
		JiraKey:        "PE-1234",
		JiraComment:    "99001",
		CommentText:    "**Jira comment** by Jane:\n\nlooks good, ship it",
		SetHasComments: true,
	})
	if state.JiraInboundCommentDLQDepth() != 1 {
		t.Fatalf("expected depth 1 after enqueue, got %d", state.JiraInboundCommentDLQDepth())
	}

	replayedBefore := testutil.ToFloat64(jiraInboundCommentDLQReplayed)

	dlq.drain(context.Background())

	// The comment was posted, the anchor fields set, and the entry dequeued.
	comments := daemon.getComments()
	if len(comments) != 1 || comments[0].BeadID != "bead-1" {
		t.Fatalf("expected 1 replayed comment on bead-1, got %+v", comments)
	}
	if got := daemon.getBead("bead-1").Fields["jira_comment_id"]; got != "99001" {
		t.Errorf("jira_comment_id = %q, want 99001", got)
	}
	if got := daemon.getBead("bead-1").Fields["jira_has_comments"]; got != "true" {
		t.Errorf("jira_has_comments = %q, want true", got)
	}
	if state.JiraInboundCommentDLQDepth() != 0 {
		t.Errorf("expected depth 0 after successful drain, got %d", state.JiraInboundCommentDLQDepth())
	}
	if got := testutil.ToFloat64(jiraInboundCommentDLQReplayed) - replayedBefore; got != 1 {
		t.Errorf("expected 1 replayed increment, got %v", got)
	}
}

// TestJiraInboundCommentDLQ_ReplayIsExactlyOnce proves the exactly-once guard:
// if the mirror body is ALREADY present on the bead (the original failure was a
// lost response, not a rejected write), the drain does NOT re-post it — it
// de-dups against existing comments and still dequeues.
func TestJiraInboundCommentDLQ_ReplayIsExactlyOnce(t *testing.T) {
	daemon := newMockJiraWebhookDaemon()
	daemon.beads["bead-1"] = &beadsapi.BeadDetail{
		ID:     "bead-1",
		Status: "in_progress",
		Fields: map[string]string{"jira_key": "PE-1234"},
	}
	body := "**Jira comment** by Jane:\n\nalready landed on the first (lost-response) try"
	// Pre-seed the comment as if the first attempt actually committed.
	if err := daemon.AddComment(context.Background(), "bead-1", "jira-bridge", body); err != nil {
		t.Fatalf("seed AddComment: %v", err)
	}
	addCallsAfterSeed := daemon.addCommentCalls

	state := newDLQState(t)
	dlq := NewJiraInboundCommentDLQ(state, daemon, slog.Default())
	dlq.enqueue(JiraInboundCommentDLQEntry{
		BeadID:         "bead-1",
		JiraKey:        "PE-1234",
		JiraComment:    "99002",
		CommentText:    body,
		SetHasComments: true,
	})

	dlq.drain(context.Background())

	// No SECOND AddComment — the drain saw the body already present and skipped it.
	if daemon.addCommentCalls != addCallsAfterSeed {
		t.Errorf("expected no additional AddComment (exactly-once), got %d extra",
			daemon.addCommentCalls-addCallsAfterSeed)
	}
	if got := len(daemon.getComments()); got != 1 {
		t.Errorf("expected exactly 1 comment (no duplicate), got %d", got)
	}
	// Anchor fields are still (re)set, and the entry is dequeued.
	if got := daemon.getBead("bead-1").Fields["jira_comment_id"]; got != "99002" {
		t.Errorf("jira_comment_id = %q, want 99002", got)
	}
	if state.JiraInboundCommentDLQDepth() != 0 {
		t.Errorf("expected depth 0 after dedup drain, got %d", state.JiraInboundCommentDLQDepth())
	}
}

// TestJiraInboundCommentDLQ_PoisonedEntryDroppedAfterBudget proves a mirror that
// keeps failing on replay is dropped once it exhausts jiraInboundCommentDLQMaxAttempts,
// so a permanently-poisoned entry cannot wedge the queue forever.
func TestJiraInboundCommentDLQ_PoisonedEntryDroppedAfterBudget(t *testing.T) {
	daemon := newMockJiraWebhookDaemon()
	daemon.beads["bead-1"] = &beadsapi.BeadDetail{
		ID:     "bead-1",
		Status: "in_progress",
		Fields: map[string]string{"jira_key": "PE-1234"},
	}
	state := newDLQState(t)
	dlq := NewJiraInboundCommentDLQ(state, daemon, slog.Default())
	dlq.enqueue(JiraInboundCommentDLQEntry{
		BeadID:      "bead-1",
		JiraKey:     "PE-1234",
		JiraComment: "99003",
		CommentText: "**Jira comment** by Jane:\n\nwill never land",
	})

	droppedBefore := testutil.ToFloat64(jiraInboundCommentDLQDropped.WithLabelValues("exhausted"))

	// Every replay read fails, so the entry never lands. Provide enough errors to
	// cover every drain call up to and including the drop.
	for i := 0; i < jiraInboundCommentDLQMaxAttempts+1; i++ {
		daemon.getCommentErrs = append(daemon.getCommentErrs, transientDaemonErr())
	}

	// Drain repeatedly; each call bumps Attempts by one until the budget is spent.
	for i := 0; i < jiraInboundCommentDLQMaxAttempts+1; i++ {
		dlq.drain(context.Background())
		if state.JiraInboundCommentDLQDepth() == 0 {
			break
		}
	}

	if state.JiraInboundCommentDLQDepth() != 0 {
		t.Errorf("expected poisoned entry dropped (depth 0), got depth %d", state.JiraInboundCommentDLQDepth())
	}
	if got := testutil.ToFloat64(jiraInboundCommentDLQDropped.WithLabelValues("exhausted")) - droppedBefore; got != 1 {
		t.Errorf("expected 1 exhausted-drop increment, got %v", got)
	}
}

// TestJiraInboundCommentDLQ_ReEnqueueCoalesces proves a re-enqueue of the same
// (bead, comment) key coalesces to a single entry rather than piling up
// duplicates.
func TestJiraInboundCommentDLQ_ReEnqueueCoalesces(t *testing.T) {
	daemon := newMockJiraWebhookDaemon()
	state := newDLQState(t)
	dlq := NewJiraInboundCommentDLQ(state, daemon, slog.Default())
	entry := JiraInboundCommentDLQEntry{
		BeadID:      "bead-1",
		JiraKey:     "PE-1234",
		JiraComment: "77",
		CommentText: "**Jira comment** by X:\n\nsame comment",
	}
	dlq.enqueue(entry)
	dlq.enqueue(entry)
	dlq.enqueue(entry)
	if got := state.JiraInboundCommentDLQDepth(); got != 1 {
		t.Errorf("expected 1 coalesced entry, got %d", got)
	}
}

// TestJiraInboundCommentDLQ_SurvivesReload proves the "survives a bridge crash"
// contract: a queued mirror written by one StateManager is readable by a fresh
// StateManager loading the same state file, so a restart mid-outage replays it.
func TestJiraInboundCommentDLQ_SurvivesReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	sm1, err := NewStateManager(path)
	if err != nil {
		t.Fatalf("NewStateManager: %v", err)
	}
	if _, err := sm1.EnqueueJiraInboundCommentDLQ(JiraInboundCommentDLQEntry{
		Key:         "bead-1:55",
		BeadID:      "bead-1",
		JiraKey:     "PE-1234",
		JiraComment: "55",
		CommentText: "**Jira comment** by Q:\n\npersist me",
	}); err != nil {
		t.Fatalf("EnqueueJiraInboundCommentDLQ: %v", err)
	}

	// A fresh manager on the same path loads the persisted queue.
	sm2, err := NewStateManager(path)
	if err != nil {
		t.Fatalf("reload NewStateManager: %v", err)
	}
	entries := sm2.AllJiraInboundCommentDLQ()
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry after reload, got %d", len(entries))
	}
	if entries[0].BeadID != "bead-1" || entries[0].JiraComment != "55" {
		t.Errorf("reloaded entry mismatch: %+v", entries[0])
	}
}

// TestNewJiraInboundCommentDLQ_NilDepsReturnNil proves the constructor returns a
// nil (valid, nil-safe) DLQ when a dependency is missing, so a caller without a
// state manager degrades cleanly.
func TestNewJiraInboundCommentDLQ_NilDepsReturnNil(t *testing.T) {
	if NewJiraInboundCommentDLQ(nil, newMockJiraWebhookDaemon(), slog.Default()) != nil {
		t.Error("expected nil DLQ when store is nil")
	}
	if NewJiraInboundCommentDLQ(newDLQState(t), nil, slog.Default()) != nil {
		t.Error("expected nil DLQ when daemon is nil")
	}
	// A nil DLQ's methods are safe no-ops.
	var d *jiraInboundCommentDLQ
	d.enqueue(JiraInboundCommentDLQEntry{BeadID: "x"})
	d.drain(context.Background())
	d.StartDrain(context.Background())
}
