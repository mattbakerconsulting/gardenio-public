package bridge

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"gardenio/controller/internal/beadsapi"
)

// transientDaemonErr is a daemon 503 — retryable by isTransientDaemonError, so
// it drives the bounded-retry path in retryJiraInboundWrite.
func transientDaemonErr() error {
	return &beadsapi.APIError{StatusCode: http.StatusServiceUnavailable, Message: "daemon restarting"}
}

// TestJiraWebhookHandler_CommentMirror_RetriesTransientThenSucceeds proves a
// momentary daemon blip during the inbound comment mirror is absorbed by the
// bounded retry: the first AddComment attempt returns a transient 503, the
// retry succeeds, and the human follow-up is recorded on the bead rather than
// silently dropped (epic ka-0000007401 — Slack inbound-durability parity).
func TestJiraWebhookHandler_CommentMirror_RetriesTransientThenSucceeds(t *testing.T) {
	daemon := newMockJiraWebhookDaemon()
	daemon.beads["bead-1"] = &beadsapi.BeadDetail{
		ID:       "bead-1",
		Status:   "in_progress",
		Assignee: "test-agent",
		Fields:   map[string]string{"jira_key": "PE-1234"},
	}
	// First AddComment attempt fails transiently; the retry (second call)
	// records normally.
	daemon.addCommentErrs = []error{transientDaemonErr()}

	retriesBefore := testutil.ToFloat64(jiraInboundCommentWriteRetries.WithLabelValues("add_comment"))
	failuresBefore := testutil.ToFloat64(jiraInboundCommentWriteFailures.WithLabelValues("add_comment"))

	handler := JiraWebhookHandler(JiraWebhookConfig{
		Daemon:  daemon,
		Nudge:   func(_ context.Context, _, _ string) error { return nil },
		DevMode: true,
		Logger:  slog.Default(),
	})

	body := makeJiraWebhookPayload("comment_created", "PE-1234", "Please fix the alignment", "John Doe")
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// The comment WAS eventually recorded (not dropped), and it took two
	// AddComment calls (the transient failure + the successful retry).
	if daemon.addCommentCalls != 2 {
		t.Errorf("expected 2 AddComment calls (fail + retry), got %d", daemon.addCommentCalls)
	}
	comments := daemon.getComments()
	if len(comments) != 1 {
		t.Fatalf("expected 1 recorded comment after retry, got %d", len(comments))
	}
	if !strings.Contains(comments[0].Text, "Please fix the alignment") {
		t.Errorf("expected recorded comment to carry the body, got %q", comments[0].Text)
	}

	// One retry counted, no terminal failure.
	if got := testutil.ToFloat64(jiraInboundCommentWriteRetries.WithLabelValues("add_comment")) - retriesBefore; got != 1 {
		t.Errorf("expected 1 add_comment retry increment, got %v", got)
	}
	if got := testutil.ToFloat64(jiraInboundCommentWriteFailures.WithLabelValues("add_comment")) - failuresBefore; got != 0 {
		t.Errorf("expected 0 add_comment failure increments, got %v", got)
	}
}

// TestJiraWebhookHandler_CommentMirror_TerminalFailureCountedAndIsolated proves
// that when the comment mirror exhausts its retries (every attempt transient),
// the failure is counted on the metric AND a second matching bead is still
// processed — a poisoned write on one bead never starves the others in the same
// event.
func TestJiraWebhookHandler_CommentMirror_TerminalFailureCountedAndIsolated(t *testing.T) {
	daemon := newMockJiraWebhookDaemon()
	daemon.beads["bead-fail"] = &beadsapi.BeadDetail{
		ID:     "bead-fail",
		Status: "in_progress",
		Fields: map[string]string{"jira_key": "PE-1234"},
	}
	daemon.beads["bead-ok"] = &beadsapi.BeadDetail{
		ID:     "bead-ok",
		Status: "in_progress",
		Fields: map[string]string{"jira_key": "PE-1234"},
	}
	// Fail every AddComment attempt for the whole event. threadWriteMaxAttempts
	// (3) attempts are consumed by the first bead; the surviving errors keep the
	// second bead's mirror failing too, so BOTH beads terminally fail — but both
	// are still ATTEMPTED (the loop is not aborted by the first failure).
	daemon.addCommentErrs = []error{
		transientDaemonErr(), transientDaemonErr(), transientDaemonErr(),
		transientDaemonErr(), transientDaemonErr(), transientDaemonErr(),
	}

	failuresBefore := testutil.ToFloat64(jiraInboundCommentWriteFailures.WithLabelValues("add_comment"))

	handler := JiraWebhookHandler(JiraWebhookConfig{
		Daemon:  daemon,
		Nudge:   func(_ context.Context, _, _ string) error { return nil },
		DevMode: true,
		Logger:  slog.Default(),
	})

	body := makeJiraWebhookPayload("comment_created", "PE-1234", "hello", "John Doe")
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	// The webhook still returns 200 (Jira Cloud would not retry regardless), and
	// no comment was recorded on either bead.
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if got := len(daemon.getComments()); got != 0 {
		t.Fatalf("expected 0 recorded comments (all attempts failed), got %d", got)
	}

	// Both beads were attempted — the first bead's terminal failure did not
	// abort processing of the second (3 attempts each = 6 total AddComment
	// calls).
	if daemon.addCommentCalls != 6 {
		t.Errorf("expected 6 AddComment calls (3 attempts x 2 beads), got %d", daemon.addCommentCalls)
	}
	// Two terminal failures counted (one per bead).
	if got := testutil.ToFloat64(jiraInboundCommentWriteFailures.WithLabelValues("add_comment")) - failuresBefore; got != 2 {
		t.Errorf("expected 2 add_comment terminal-failure increments, got %v", got)
	}
}

// TestJiraWebhookHandler_CommentMirror_DeterministicErrorNotRetried proves a
// non-transient (deterministic 4xx) daemon error is NOT retried — retrying a
// deterministic rejection only burns the synchronous webhook budget. The mirror
// fails once, is counted, and no retry increment is recorded.
func TestJiraWebhookHandler_CommentMirror_DeterministicErrorNotRetried(t *testing.T) {
	daemon := newMockJiraWebhookDaemon()
	daemon.beads["bead-1"] = &beadsapi.BeadDetail{
		ID:     "bead-1",
		Status: "in_progress",
		Fields: map[string]string{"jira_key": "PE-1234"},
	}
	// A 400 is deterministic — not retryable.
	daemon.addCommentErrs = []error{&beadsapi.APIError{StatusCode: http.StatusBadRequest, Message: "bad request"}}

	retriesBefore := testutil.ToFloat64(jiraInboundCommentWriteRetries.WithLabelValues("add_comment"))
	failuresBefore := testutil.ToFloat64(jiraInboundCommentWriteFailures.WithLabelValues("add_comment"))

	handler := JiraWebhookHandler(JiraWebhookConfig{
		Daemon:  daemon,
		Nudge:   func(_ context.Context, _, _ string) error { return nil },
		DevMode: true,
		Logger:  slog.Default(),
	})

	body := makeJiraWebhookPayload("comment_created", "PE-1234", "hello", "John Doe")
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	// Exactly one attempt (no retry on a deterministic error).
	if daemon.addCommentCalls != 1 {
		t.Errorf("expected 1 AddComment call (no retry on 4xx), got %d", daemon.addCommentCalls)
	}
	if got := testutil.ToFloat64(jiraInboundCommentWriteRetries.WithLabelValues("add_comment")) - retriesBefore; got != 0 {
		t.Errorf("expected 0 retry increments on deterministic error, got %v", got)
	}
	if got := testutil.ToFloat64(jiraInboundCommentWriteFailures.WithLabelValues("add_comment")) - failuresBefore; got != 1 {
		t.Errorf("expected 1 terminal-failure increment, got %v", got)
	}
}

// TestJiraWebhookHandler_FieldUpdate_RetriesTransientThenSucceeds proves the
// jira_has_comments / jira_comment_id field update (the reply-anchor write) is
// also retried: a dropped jira_comment_id would leave a later gb jira reply
// threading to a stale older comment.
func TestJiraWebhookHandler_FieldUpdate_RetriesTransientThenSucceeds(t *testing.T) {
	daemon := newMockJiraWebhookDaemon()
	daemon.beads["bead-1"] = &beadsapi.BeadDetail{
		ID:       "bead-1",
		Status:   "in_progress",
		Assignee: "test-agent",
		Fields:   map[string]string{"jira_key": "PE-1234"},
	}
	// First UpdateBeadFields attempt fails transiently; the retry records it.
	daemon.updateFieldErrs = []error{transientDaemonErr()}

	retriesBefore := testutil.ToFloat64(jiraInboundCommentWriteRetries.WithLabelValues("set_fields"))
	failuresBefore := testutil.ToFloat64(jiraInboundCommentWriteFailures.WithLabelValues("set_fields"))

	handler := JiraWebhookHandler(JiraWebhookConfig{
		Daemon:  daemon,
		Nudge:   func(_ context.Context, _, _ string) error { return nil },
		DevMode: true,
		Logger:  slog.Default(),
	})

	body := makeJiraWebhookPayload("comment_created", "PE-1234", "hello", "John Doe")
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	// The reply anchor (jira_comment_id) survived the blip.
	bead := daemon.getBead("bead-1")
	if bead.Fields["jira_has_comments"] != "true" {
		t.Errorf("expected jira_has_comments=true after retry, got %q", bead.Fields["jira_has_comments"])
	}
	if bead.Fields["jira_comment_id"] != "12345" {
		t.Errorf("expected jira_comment_id=12345 after retry, got %q", bead.Fields["jira_comment_id"])
	}
	if daemon.updateCalls != 2 {
		t.Errorf("expected 2 UpdateBeadFields calls (fail + retry), got %d", daemon.updateCalls)
	}
	if got := testutil.ToFloat64(jiraInboundCommentWriteRetries.WithLabelValues("set_fields")) - retriesBefore; got != 1 {
		t.Errorf("expected 1 set_fields retry increment, got %v", got)
	}
	if got := testutil.ToFloat64(jiraInboundCommentWriteFailures.WithLabelValues("set_fields")) - failuresBefore; got != 0 {
		t.Errorf("expected 0 set_fields failure increments, got %v", got)
	}
}
