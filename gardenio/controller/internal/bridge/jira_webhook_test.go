package bridge

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// mockJiraWebhookDaemon implements JiraWebhookBeadClient for testing.
type mockJiraWebhookDaemon struct {
	mu       sync.Mutex
	beads    map[string]*beadsapi.BeadDetail
	comments []commentRecord

	// Injectable per-call error sequences for the comment-mirror durability
	// tests (epic ka-0000007401). Each call pops the head; a non-nil head is
	// returned WITHOUT recording the write (mirroring a real failed daemon
	// write), a nil head (or an exhausted/nil slice) behaves normally. This
	// lets a test drive retryJiraInboundWrite through transient-then-success
	// and terminal-failure paths.
	addCommentErrs  []error
	updateFieldErrs []error
	getCommentErrs  []error
	addCommentCalls int
	updateCalls     int
	getCommentCalls int
}

// nextErr pops the head of an injectable error sequence, returning nil when the
// slice is empty. Caller holds m.mu.
func nextErr(seq *[]error) error {
	if len(*seq) == 0 {
		return nil
	}
	e := (*seq)[0]
	*seq = (*seq)[1:]
	return e
}

func newMockJiraWebhookDaemon() *mockJiraWebhookDaemon {
	return &mockJiraWebhookDaemon{beads: make(map[string]*beadsapi.BeadDetail)}
}

// ListTaskBeadsForJiraKey mirrors the daemon's server-side fields.jira_key
// equality predicate so tests exercise the same narrowing as production.
func (m *mockJiraWebhookDaemon) ListTaskBeadsForJiraKey(_ context.Context, jiraKey string) ([]*beadsapi.BeadDetail, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if jiraKey == "" {
		return nil, nil
	}
	var result []*beadsapi.BeadDetail
	for _, b := range m.beads {
		if b.Fields["jira_key"] == jiraKey {
			result = append(result, b)
		}
	}
	return result, nil
}

func (m *mockJiraWebhookDaemon) ListTaskBeadsAll(_ context.Context) ([]*beadsapi.BeadDetail, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []*beadsapi.BeadDetail
	for _, b := range m.beads {
		result = append(result, b)
	}
	return result, nil
}

func (m *mockJiraWebhookDaemon) UpdateBeadFields(_ context.Context, beadID string, fields map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.updateCalls++
	if err := nextErr(&m.updateFieldErrs); err != nil {
		return err
	}
	bead, ok := m.beads[beadID]
	if !ok {
		return fmt.Errorf("bead %s not found", beadID)
	}
	for k, v := range fields {
		bead.Fields[k] = v
	}
	return nil
}

// UpdateBead applies a status + fields patch atomically, mirroring the daemon's
// UpdateBead. Reuses the updateFieldErrs injection sequence so the durability
// tests can drive it through transient/terminal failures.
func (m *mockJiraWebhookDaemon) UpdateBead(_ context.Context, beadID string, req beadsapi.UpdateBeadRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.updateCalls++
	if err := nextErr(&m.updateFieldErrs); err != nil {
		return err
	}
	bead, ok := m.beads[beadID]
	if !ok {
		return fmt.Errorf("bead %s not found", beadID)
	}
	if req.Status != nil {
		bead.Status = *req.Status
	}
	for k, v := range req.Fields {
		if v == nil {
			delete(bead.Fields, k)
			continue
		}
		bead.Fields[k] = fmt.Sprintf("%v", v)
	}
	return nil
}

func (m *mockJiraWebhookDaemon) AddComment(_ context.Context, beadID, author, text string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.addCommentCalls++
	if err := nextErr(&m.addCommentErrs); err != nil {
		return err
	}
	if _, ok := m.beads[beadID]; !ok {
		return fmt.Errorf("bead %s not found", beadID)
	}
	m.comments = append(m.comments, commentRecord{BeadID: beadID, Author: author, Text: text})
	return nil
}

func (m *mockJiraWebhookDaemon) getBead(id string) *beadsapi.BeadDetail {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.beads[id]
}

func (m *mockJiraWebhookDaemon) getComments() []commentRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]commentRecord, len(m.comments))
	copy(out, m.comments)
	return out
}

// GetComments lets the mock double as a jiraInboundCommentReplayClient so the
// DLQ drain tests (jira_inbound_comment_dlq_test.go) can run against the same
// recorded comments the webhook mirror writes. Optionally injectable errors via
// getCommentErrs drive the replay read-failure path.
func (m *mockJiraWebhookDaemon) GetComments(_ context.Context, beadID string) ([]beadsapi.Comment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.getCommentCalls++
	if err := nextErr(&m.getCommentErrs); err != nil {
		return nil, err
	}
	var out []beadsapi.Comment
	for _, c := range m.comments {
		if c.BeadID == beadID {
			out = append(out, beadsapi.Comment{Author: c.Author, Text: c.Text})
		}
	}
	return out, nil
}

func makeJiraWebhookPayload(event, jiraKey, commentBody, authorName string) []byte {
	payload := map[string]any{
		"webhookEvent": event,
		"issue": map[string]any{
			"key": jiraKey,
			"fields": map[string]any{
				"summary": "Test issue summary",
			},
		},
		"comment": map[string]any{
			"id":   "12345",
			"body": commentBody,
			"author": map[string]any{
				"displayName": authorName,
				"accountId":   "user-123",
			},
		},
	}
	b, _ := json.Marshal(payload)
	return b
}

// makeJiraWebhookPayloadWithAuthorID mirrors makeJiraWebhookPayload but lets a
// test set the comment author's Jira accountId — the field the self-wake
// author-set guard (ka-0000003581) keys on. The default helper hardcodes
// "user-123" (a human), so an author-identity test cannot use it.
func makeJiraWebhookPayloadWithAuthorID(event, jiraKey, commentBody, authorName, accountID string) []byte {
	payload := map[string]any{
		"webhookEvent": event,
		"issue": map[string]any{
			"key": jiraKey,
			"fields": map[string]any{
				"summary": "Test issue summary",
			},
		},
		"comment": map[string]any{
			"id":   "115851",
			"body": commentBody,
			"author": map[string]any{
				"displayName": authorName,
				"accountId":   accountID,
			},
		},
	}
	b, _ := json.Marshal(payload)
	return b
}

func signPayload(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestJiraWebhookHandler_CommentCreated_MatchesBead(t *testing.T) {
	daemon := newMockJiraWebhookDaemon()
	daemon.beads["bead-1"] = &beadsapi.BeadDetail{
		ID:       "bead-1",
		Status:   "in_progress",
		Assignee: "test-agent",
		Fields:   map[string]string{"jira_key": "PE-1234"},
	}

	var nudgedAgent, nudgedMsg string
	nudge := func(_ context.Context, agent, msg string) error {
		nudgedAgent = agent
		nudgedMsg = msg
		return nil
	}

	handler := JiraWebhookHandler(JiraWebhookConfig{
		Daemon:  daemon,
		Nudge:   nudge,
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

	// Verify comment was added to the bead.
	comments := daemon.getComments()
	if len(comments) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(comments))
	}
	if comments[0].BeadID != "bead-1" {
		t.Errorf("expected comment on bead-1, got %s", comments[0].BeadID)
	}
	if comments[0].Author != "jira-bridge" {
		t.Errorf("expected author jira-bridge, got %s", comments[0].Author)
	}
	if !strings.Contains(comments[0].Text, "John Doe") {
		t.Errorf("expected comment to contain author name, got %s", comments[0].Text)
	}
	if !strings.Contains(comments[0].Text, "Please fix the alignment") {
		t.Errorf("expected comment to contain body, got %s", comments[0].Text)
	}

	// Verify jira_has_comments was set.
	bead := daemon.getBead("bead-1")
	if bead.Fields["jira_has_comments"] != "true" {
		t.Errorf("expected jira_has_comments=true, got %s", bead.Fields["jira_has_comments"])
	}

	// Verify agent was nudged.
	if nudgedAgent != "test-agent" {
		t.Errorf("expected nudge to test-agent, got %s", nudgedAgent)
	}
	if !strings.Contains(nudgedMsg, "PE-1234") {
		t.Errorf("expected nudge message to contain jira key, got %s", nudgedMsg)
	}
}

func TestJiraWebhookHandler_CommentCreated_NoMatchingBead(t *testing.T) {
	daemon := newMockJiraWebhookDaemon()
	daemon.beads["bead-1"] = &beadsapi.BeadDetail{
		ID:     "bead-1",
		Fields: map[string]string{"jira_key": "PE-9999"},
	}

	handler := JiraWebhookHandler(JiraWebhookConfig{
		Daemon:  daemon,
		DevMode: true,
		Logger:  slog.Default(),
	})

	body := makeJiraWebhookPayload("comment_created", "PE-1234", "test comment", "User")
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	comments := daemon.getComments()
	if len(comments) != 0 {
		t.Errorf("expected 0 comments (no match), got %d", len(comments))
	}
}

func TestJiraWebhookHandler_IgnoresNonCommentEvents(t *testing.T) {
	daemon := newMockJiraWebhookDaemon()

	handler := JiraWebhookHandler(JiraWebhookConfig{
		Daemon:  daemon,
		DevMode: true,
		Logger:  slog.Default(),
	})

	body := makeJiraWebhookPayload("issue_updated", "PE-1234", "", "")
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "ignored") {
		t.Errorf("expected ignored response, got %s", w.Body.String())
	}
}

// comment_deleted is an EXPLICIT, logged no-op (GAP 5, epic ka-0000007401): it
// mutates no bead — the conversation mirror is append-only and the reply anchor
// is left intact — but it must return a clean 200 "ignored" so the deadman
// monitor still counts it as an authenticated, decoded delivery.
func TestJiraWebhookHandler_CommentDeleted_IsExplicitNoOp(t *testing.T) {
	resetWebhookMetrics()
	daemon := newMockJiraWebhookDaemon()
	daemon.beads["bead-1"] = &beadsapi.BeadDetail{
		ID:       "bead-1",
		Status:   "in_progress",
		Assignee: "test-agent",
		Fields:   map[string]string{"jira_key": "PE-1234", "jira_comment_id": "12345"},
	}

	nudged := false
	handler := JiraWebhookHandler(JiraWebhookConfig{
		Daemon:  daemon,
		DevMode: true,
		Nudge:   func(context.Context, string, string) error { nudged = true; return nil },
		Logger:  slog.Default(),
	})

	// A deleted comment on a ticket that has a matching bead.
	body := makeJiraWebhookPayload("comment_deleted", "PE-1234", "some text that was deleted", "Human")
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "ignored") {
		t.Errorf("expected ignored response, got %s", w.Body.String())
	}

	// No bead mutation: the conversation is append-only, so nothing is bridged
	// and no field is written.
	if comments := daemon.getComments(); len(comments) != 0 {
		t.Errorf("comment_deleted must not bridge a comment, got %d", len(comments))
	}
	if daemon.updateCalls != 0 {
		t.Errorf("comment_deleted must not update any bead field, got %d update calls", daemon.updateCalls)
	}
	// The reply anchor is left intact — clearing it is out of scope and would
	// expand the guarded write surface for a self-healing degradation.
	if got := daemon.getBead("bead-1").Fields["jira_comment_id"]; got != "12345" {
		t.Errorf("jira_comment_id must be left untouched, got %q", got)
	}
	if nudged {
		t.Error("comment_deleted must not nudge the assigned agent")
	}

	// It still counts as an authenticated, decoded delivery for the deadman
	// monitor: recorded as `ignored` and it stamps the liveness clock.
	if got := testutil.ToFloat64(jiraWebhookDeliveries.WithLabelValues(webhookOutcomeIgnored)); got != 1 {
		t.Errorf("ignored delivery count = %v, want 1", got)
	}
	if lastWebhookReceivedNano.Load() == 0 {
		t.Error("an authenticated comment_deleted delivery must stamp the liveness clock")
	}
}

// A comment_deleted payload missing the issue/comment objects must still be a
// clean, logged 200 no-op — never a panic or a 400.
func TestJiraWebhookHandler_CommentDeleted_SparsePayload(t *testing.T) {
	resetWebhookMetrics()
	handler := JiraWebhookHandler(JiraWebhookConfig{
		Daemon:  newMockJiraWebhookDaemon(),
		DevMode: true,
		Logger:  slog.Default(),
	})

	body, _ := json.Marshal(map[string]any{"webhookEvent": "comment_deleted"})
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "ignored") {
		t.Errorf("expected ignored response, got %s", w.Body.String())
	}
	if got := testutil.ToFloat64(jiraWebhookDeliveries.WithLabelValues(webhookOutcomeIgnored)); got != 1 {
		t.Errorf("ignored delivery count = %v, want 1", got)
	}
}

func TestJiraWebhookHandler_MethodNotAllowed(t *testing.T) {
	handler := JiraWebhookHandler(JiraWebhookConfig{
		Daemon:  newMockJiraWebhookDaemon(),
		DevMode: true,
		Logger:  slog.Default(),
	})

	req := httptest.NewRequest(http.MethodGet, "/webhook", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
}

func TestJiraWebhookHandler_HMACVerification(t *testing.T) {
	secret := "test-webhook-secret"
	daemon := newMockJiraWebhookDaemon()
	daemon.beads["bead-1"] = &beadsapi.BeadDetail{
		ID:     "bead-1",
		Fields: map[string]string{"jira_key": "PE-1234"},
	}

	handler := JiraWebhookHandler(JiraWebhookConfig{
		Daemon:        daemon,
		WebhookSecret: secret,
		Logger:        slog.Default(),
	})

	body := makeJiraWebhookPayload("comment_created", "PE-1234", "test", "User")

	// Valid signature should succeed.
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-Hub-Signature", signPayload(body, secret))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 with valid HMAC, got %d", w.Code)
	}

	// Invalid signature should be rejected.
	req = httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-Hub-Signature", "sha256=invalid")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 with invalid HMAC, got %d", w.Code)
	}

	// Missing signature should be rejected.
	req = httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 with missing HMAC, got %d", w.Code)
	}
}

func TestJiraWebhookHandler_EmptySecretRejects(t *testing.T) {
	// When no webhook secret is configured and dev mode is off, all requests must be rejected.
	handler := JiraWebhookHandler(JiraWebhookConfig{
		Daemon: newMockJiraWebhookDaemon(),
		Logger: slog.Default(),
		// No WebhookSecret, no DevMode — fail-closed.
	})

	body := makeJiraWebhookPayload("comment_created", "PE-1234", "test", "User")
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 when secret is empty and dev mode off, got %d", w.Code)
	}
}

func TestJiraWebhookHandler_DevModeSkipsHMAC(t *testing.T) {
	// With dev mode enabled and no secret, requests should be accepted.
	daemon := newMockJiraWebhookDaemon()
	daemon.beads["bead-1"] = &beadsapi.BeadDetail{
		ID:     "bead-1",
		Fields: map[string]string{"jira_key": "PE-1234"},
	}

	handler := JiraWebhookHandler(JiraWebhookConfig{
		Daemon:  daemon,
		DevMode: true,
		Logger:  slog.Default(),
	})

	body := makeJiraWebhookPayload("comment_created", "PE-1234", "test", "User")
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 in dev mode, got %d", w.Code)
	}
}

func TestJiraWebhookHandler_NoNudgeWithoutAssignee(t *testing.T) {
	daemon := newMockJiraWebhookDaemon()
	daemon.beads["bead-1"] = &beadsapi.BeadDetail{
		ID:       "bead-1",
		Assignee: "", // no assignee
		Fields:   map[string]string{"jira_key": "PE-1234"},
	}

	nudged := false
	nudge := func(_ context.Context, _, _ string) error {
		nudged = true
		return nil
	}

	handler := JiraWebhookHandler(JiraWebhookConfig{
		Daemon:  daemon,
		Nudge:   nudge,
		DevMode: true,
		Logger:  slog.Default(),
	})

	body := makeJiraWebhookPayload("comment_created", "PE-1234", "test", "User")
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if nudged {
		t.Error("should not nudge when bead has no assignee")
	}

	// Comment should still be added.
	if len(daemon.getComments()) != 1 {
		t.Error("expected comment to be added even without assignee")
	}
}

func TestVerifyJiraHMAC(t *testing.T) {
	secret := "my-secret"
	body := []byte(`{"test":"data"}`)

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	validSig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	if !verifyJiraHMAC(body, validSig, secret) {
		t.Error("expected valid HMAC to pass")
	}

	if verifyJiraHMAC(body, "sha256=deadbeef", secret) {
		t.Error("expected invalid HMAC to fail")
	}

	if verifyJiraHMAC(body, "", secret) {
		t.Error("expected empty signature to fail")
	}

	// Without sha256= prefix.
	rawSig := hex.EncodeToString(mac.Sum(nil))
	if !verifyJiraHMAC(body, rawSig, secret) {
		t.Error("expected valid HMAC without prefix to pass")
	}
}

func TestBuildJiraCommentNudgeMessage(t *testing.T) {
	event := jiraWebhookEvent{
		Issue: &jiraIssueRef{
			Key: "PE-1234",
		},
		Comment: &jiraComment{
			Body: json.RawMessage(`"This needs to be fixed"`),
		},
	}
	event.Issue.Fields.Summary = "Login page broken"
	event.Comment.Author.DisplayName = "Jane Smith"

	bead := &beadsapi.BeadDetail{
		ID:                "bead-42",
		OperatedBy:        "slack:U0JANE",
		OperatedByDisplay: "Jane Smith",
	}
	history := "- Bob (2026-07-01T10:00:00Z): initial report"
	msg := buildJiraCommentNudgeMessage(event, bead, history, "")

	if !strings.Contains(msg, "PE-1234") {
		t.Error("message should contain Jira key")
	}
	if !strings.Contains(msg, "Login page broken") {
		t.Error("message should contain issue summary")
	}
	if !strings.Contains(msg, "Jane Smith") {
		t.Error("message should contain author name")
	}
	if !strings.Contains(msg, "This needs to be fixed") {
		t.Error("message should contain comment body")
	}
	if !strings.Contains(msg, "bead-42") {
		t.Error("message should contain bead ID")
	}
	// F4 (plan ka-0000007401): comment history + resolved operator identity.
	if !strings.Contains(msg, "Recent comments on PE-1234") || !strings.Contains(msg, "initial report") {
		t.Errorf("message should contain the comment history transcript: %s", msg)
	}
	if !strings.Contains(msg, "Operator: Jane Smith (slack:U0JANE)") {
		t.Errorf("message should contain the resolved operator identity: %s", msg)
	}
	// F2 note: the conversational reply affordance must be discoverable.
	if !strings.Contains(msg, "gb jira reply") {
		t.Errorf("message should advertise gb jira reply: %s", msg)
	}
	// A woken/re-spawned agent must be told it can re-read the conversation
	// (G1, gb jira thread) and the current ticket body (G6, gb jira show) —
	// otherwise the mid-session re-read capabilities are undiscoverable from
	// the nudge path (epic ka-0000007401).
	if !strings.Contains(msg, "gb jira thread") {
		t.Errorf("message should advertise gb jira thread: %s", msg)
	}
	if !strings.Contains(msg, "gb jira show PE-1234") {
		t.Errorf("message should advertise gb jira show for the bound ticket: %s", msg)
	}
}

// TestBuildJiraCommentNudgeMessage_CleanedBody pins the --fresh strip
// (ka-0000008188): the caller passes a comment body with the --fresh token
// already removed, and the rendered nudge shows that cleaned text rather than
// the raw body — so the control flag never leaks into the prompt the
// (respawned) agent reads.
func TestBuildJiraCommentNudgeMessage_CleanedBody(t *testing.T) {
	event := jiraWebhookEvent{
		Issue:   &jiraIssueRef{Key: "PE-1234"},
		Comment: &jiraComment{Body: json.RawMessage(`"--fresh look at this again"`)},
	}
	event.Comment.Author.DisplayName = "Jane Smith"

	bead := &beadsapi.BeadDetail{ID: "bead-42"}
	// The caller strips --fresh before building the message.
	msg := buildJiraCommentNudgeMessage(event, bead, "", "look at this again")

	if strings.Contains(msg, "--fresh") {
		t.Errorf("cleaned body must not leak the --fresh token into the prompt: %s", msg)
	}
	if !strings.Contains(msg, "look at this again") {
		t.Errorf("cleaned comment body should be rendered: %s", msg)
	}
}

// TestBuildJiraCommentNudgeMessage_EmptyBodyFallsBack verifies the fallback: an
// empty commentBody arg renders the raw event body unchanged (the no-flag path).
func TestBuildJiraCommentNudgeMessage_EmptyBodyFallsBack(t *testing.T) {
	event := jiraWebhookEvent{
		Issue:   &jiraIssueRef{Key: "PE-1234"},
		Comment: &jiraComment{Body: json.RawMessage(`"unmodified body"`)},
	}
	event.Comment.Author.DisplayName = "Jane Smith"

	bead := &beadsapi.BeadDetail{ID: "bead-42"}
	msg := buildJiraCommentNudgeMessage(event, bead, "", "")

	if !strings.Contains(msg, "unmodified body") {
		t.Errorf("empty commentBody should fall back to the raw event body: %s", msg)
	}
}

// TestBuildJiraCommentNudgeMessage_NoOperatorNoHistory pins the degraded
// shape: no history and a non-human operated_by render neither section.
func TestBuildJiraCommentNudgeMessage_NoOperatorNoHistory(t *testing.T) {
	event := jiraWebhookEvent{
		Issue:   &jiraIssueRef{Key: "PE-1234"},
		Comment: &jiraComment{Body: json.RawMessage(`"ping"`)},
	}
	event.Comment.Author.DisplayName = "Jane Smith"

	bead := &beadsapi.BeadDetail{
		ID:     "bead-42",
		Fields: map[string]string{"operated_by": "fleet:jira-bridge"},
	}
	msg := buildJiraCommentNudgeMessage(event, bead, "", "")

	if strings.Contains(msg, "Recent comments") {
		t.Errorf("empty history must not render a transcript section: %s", msg)
	}
	if strings.Contains(msg, "Operator:") {
		t.Errorf("sentinel operated_by must not render an Operator line: %s", msg)
	}
}

// TestBuildJiraCommentNudgeMessage_OperatorFromFields pins the fields
// fallback: when the top-level OperatedBy is empty, the operated_by FIELD
// (what the mention handler stamps) supplies the identity.
func TestBuildJiraCommentNudgeMessage_OperatorFromFields(t *testing.T) {
	event := jiraWebhookEvent{
		Issue:   &jiraIssueRef{Key: "PE-1234"},
		Comment: &jiraComment{Body: json.RawMessage(`"ping"`)},
	}
	event.Comment.Author.DisplayName = "Jane Smith"

	bead := &beadsapi.BeadDetail{
		ID:     "bead-42",
		Fields: map[string]string{"operated_by": "oidc:jane@example.com"},
	}
	msg := buildJiraCommentNudgeMessage(event, bead, "", "")

	if !strings.Contains(msg, "Operator: oidc:jane@example.com") {
		t.Errorf("message should resolve operator from bead fields: %s", msg)
	}
}

// TestJiraWebhookHandler_NudgeCarriesHistoryAndOperator pins F4 (plan
// ka-0000007401) on the webhook nudge path: the message delivered to the
// assigned agent includes the ticket's recent comment history (minus the
// triggering comment) and the operator identity stamped on the task bead.
func TestJiraWebhookHandler_NudgeCarriesHistoryAndOperator(t *testing.T) {
	daemon := newMockJiraWebhookDaemon()
	daemon.beads["bead-1"] = &beadsapi.BeadDetail{
		ID:       "bead-1",
		Status:   "in_progress",
		Assignee: "test-agent",
		Fields: map[string]string{
			"jira_key":    "PE-1234",
			"operated_by": "slack:U0JANE",
		},
	}
	history := &fakeJiraHistory{comments: []JiraCommentEntry{
		// Newest first; "12345" is the webhook payload's triggering comment id.
		{ID: "12345", Author: JiraUser{DisplayName: "John Doe"}, Body: json.RawMessage(`"any update?"`)},
		{ID: "111", Author: JiraUser{DisplayName: "Alice"}, Body: json.RawMessage(`"original bug report"`), Created: "2026-07-01T09:00:00Z"},
	}}

	var nudgedMsg string
	handler := JiraWebhookHandler(JiraWebhookConfig{
		Daemon:  daemon,
		Nudge:   func(_ context.Context, _, msg string) error { nudgedMsg = msg; return nil },
		History: history,
		DevMode: true,
		Logger:  slog.Default(),
	})

	body := makeJiraWebhookPayload("comment_created", "PE-1234", "any update?", "John Doe")
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if history.gotKey != "PE-1234" {
		t.Errorf("history fetched for %q, want PE-1234", history.gotKey)
	}
	if !strings.Contains(nudgedMsg, "original bug report") {
		t.Errorf("nudge should carry prior comment history: %s", nudgedMsg)
	}
	if !strings.Contains(nudgedMsg, "Operator: slack:U0JANE") {
		t.Errorf("nudge should carry the resolved operator identity: %s", nudgedMsg)
	}
	if !strings.Contains(nudgedMsg, "gb jira reply") {
		t.Errorf("nudge should advertise gb jira reply: %s", nudgedMsg)
	}
	// The triggering comment appears once as "Comment:", not again in history.
	if strings.Count(nudgedMsg, "any update?") != 1 {
		t.Errorf("triggering comment should appear exactly once: %s", nudgedMsg)
	}
}

func TestNudgeThrottle_FirstCallAllowed(t *testing.T) {
	throttle := newNudgeThrottle(30 * time.Second)
	if !throttle.Allow("key-1") {
		t.Error("first call should be allowed")
	}
}

func TestNudgeThrottle_SecondCallWithinWindow(t *testing.T) {
	throttle := newNudgeThrottle(30 * time.Second)
	throttle.Allow("key-1")
	if throttle.Allow("key-1") {
		t.Error("second call within window should be throttled")
	}
}

func TestNudgeThrottle_AllowedAfterCooldown(t *testing.T) {
	throttle := newNudgeThrottle(50 * time.Millisecond)
	throttle.Allow("key-1")
	time.Sleep(60 * time.Millisecond)
	if !throttle.Allow("key-1") {
		t.Error("call after cooldown should be allowed")
	}
}

func TestNudgeThrottle_IndependentKeys(t *testing.T) {
	throttle := newNudgeThrottle(30 * time.Second)
	throttle.Allow("key-a")
	if !throttle.Allow("key-b") {
		t.Error("different key should not be throttled")
	}
	if throttle.Allow("key-a") {
		t.Error("same key should still be throttled")
	}
}

func TestJiraWebhookHandler_NudgeThrottled(t *testing.T) {
	daemon := newMockJiraWebhookDaemon()
	daemon.beads["bead-1"] = &beadsapi.BeadDetail{
		ID:       "bead-1",
		Status:   "in_progress",
		Assignee: "test-agent",
		Fields:   map[string]string{"jira_key": "PE-1234"},
	}

	var nudgeCount int
	nudge := func(_ context.Context, _, _ string) error {
		nudgeCount++
		return nil
	}

	handler := JiraWebhookHandler(JiraWebhookConfig{
		Daemon:        daemon,
		Nudge:         nudge,
		DevMode:       true,
		NudgeCooldown: 10 * time.Minute,
		Logger:        slog.Default(),
	})

	sendComment := func(body string) {
		payload := makeJiraWebhookPayload("comment_created", "PE-1234", body, "User")
		req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(payload))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
	}

	// First comment triggers nudge.
	sendComment("first comment")
	if nudgeCount != 1 {
		t.Fatalf("expected 1 nudge after first comment, got %d", nudgeCount)
	}

	// Second comment within window: nudge suppressed.
	sendComment("second comment")
	if nudgeCount != 1 {
		t.Fatalf("expected still 1 nudge after second comment, got %d", nudgeCount)
	}

	// Third comment: still suppressed.
	sendComment("third comment")
	if nudgeCount != 1 {
		t.Fatalf("expected still 1 nudge after third comment, got %d", nudgeCount)
	}

	// All three comments should be added to the bead.
	comments := daemon.getComments()
	if len(comments) != 3 {
		t.Errorf("expected 3 comments (no data loss), got %d", len(comments))
	}
}

func TestJiraWebhookHandler_MentionHandlerCalled(t *testing.T) {
	// Verify mention handler runs before comment bridging and suppresses nudge.
	daemon := newMockJiraWebhookDaemon()
	daemon.beads["bead-1"] = &beadsapi.BeadDetail{
		ID:       "bead-1",
		Status:   "in_progress",
		Assignee: "test-agent",
		Fields:   map[string]string{"jira_key": "PE-1234"},
	}

	var nudged bool
	nudge := func(_ context.Context, _, _ string) error {
		nudged = true
		return nil
	}

	// Create a mention handler with a mock daemon (separate from webhook daemon).
	mentionDaemon := &mockJiraMentionDaemon{
		createID: "task-1",
		spawnID:  "agent-1",
		triggerBeads: []*beadsapi.BeadDetail{
			{Fields: map[string]string{
				"action": "spawn_agent", "project": "gardenio",
				"role": "gardenio-crew", "enabled": "true",
			}},
		},
	}
	mentionHandler := NewJiraMentionHandler(JiraMentionHandlerConfig{
		Users:   allowJiraMentionUsers{},
		Daemon:  mentionDaemon,
		Project: "gardenio",
		Logger:  slog.Default(),
	})

	handler := JiraWebhookHandler(JiraWebhookConfig{
		Daemon:         daemon,
		Nudge:          nudge,
		DevMode:        true,
		MentionHandler: mentionHandler,
		Logger:         slog.Default(),
	})

	// Send a comment with @gardenio mention.
	body := makeJiraWebhookPayload("comment_created", "PE-1234", "@gardenio fix this bug", "Jane Doe")
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Mention handler should have spawned an agent.
	if len(mentionDaemon.spawned) != 1 {
		t.Errorf("expected 1 agent spawned by mention handler, got %d", len(mentionDaemon.spawned))
	}

	// Comment should still be bridged to the existing bead.
	comments := daemon.getComments()
	if len(comments) != 1 {
		t.Errorf("expected 1 comment bridged, got %d", len(comments))
	}

	// Nudge should be suppressed because mention was handled.
	if nudged {
		t.Error("expected nudge to be suppressed when mention was handled")
	}
}

func TestJiraWebhookHandler_NoMentionHandlerStillNudges(t *testing.T) {
	// Without mention handler, regular nudge should still work.
	daemon := newMockJiraWebhookDaemon()
	daemon.beads["bead-1"] = &beadsapi.BeadDetail{
		ID:       "bead-1",
		Status:   "in_progress",
		Assignee: "test-agent",
		Fields:   map[string]string{"jira_key": "PE-1234"},
	}

	var nudged bool
	nudge := func(_ context.Context, _, _ string) error {
		nudged = true
		return nil
	}

	handler := JiraWebhookHandler(JiraWebhookConfig{
		Daemon:  daemon,
		Nudge:   nudge,
		DevMode: true,
		Logger:  slog.Default(),
		// MentionHandler not set (nil)
	})

	body := makeJiraWebhookPayload("comment_created", "PE-1234", "@gardenio fix this bug", "Jane Doe")
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if !nudged {
		t.Error("expected nudge when no mention handler is configured")
	}
}

func TestJiraWebhookHandler_NudgeAllowedAfterCooldown(t *testing.T) {
	daemon := newMockJiraWebhookDaemon()
	daemon.beads["bead-1"] = &beadsapi.BeadDetail{
		ID:       "bead-1",
		Status:   "in_progress",
		Assignee: "test-agent",
		Fields:   map[string]string{"jira_key": "PE-1234"},
	}

	var nudgeCount int
	nudge := func(_ context.Context, _, _ string) error {
		nudgeCount++
		return nil
	}

	handler := JiraWebhookHandler(JiraWebhookConfig{
		Daemon:        daemon,
		Nudge:         nudge,
		DevMode:       true,
		NudgeCooldown: 50 * time.Millisecond,
		Logger:        slog.Default(),
	})

	sendComment := func(body string) {
		payload := makeJiraWebhookPayload("comment_created", "PE-1234", body, "User")
		req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(payload))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
	}

	// First comment: nudge fires.
	sendComment("first")
	if nudgeCount != 1 {
		t.Fatalf("expected 1 nudge, got %d", nudgeCount)
	}

	// Wait for cooldown to expire.
	time.Sleep(60 * time.Millisecond)

	// Next comment after cooldown: nudge fires again.
	sendComment("second")
	if nudgeCount != 2 {
		t.Fatalf("expected 2 nudges after cooldown, got %d", nudgeCount)
	}
}

func TestJiraWebhookHandler_CommentUpdated_MatchesBead(t *testing.T) {
	daemon := newMockJiraWebhookDaemon()
	daemon.beads["bead-1"] = &beadsapi.BeadDetail{
		ID:       "bead-1",
		Status:   "in_progress",
		Assignee: "test-agent",
		Fields:   map[string]string{"jira_key": "PE-1234"},
	}

	var nudgedAgent string
	nudge := func(_ context.Context, agent, _ string) error {
		nudgedAgent = agent
		return nil
	}

	handler := JiraWebhookHandler(JiraWebhookConfig{
		Daemon:  daemon,
		Nudge:   nudge,
		DevMode: true,
		Logger:  slog.Default(),
	})

	// Send comment_updated event.
	body := makeJiraWebhookPayload("comment_updated", "PE-1234", "Updated comment text", "Editor")
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "comment_updated") {
		t.Errorf("response should contain event name, got %s", w.Body.String())
	}

	// Verify comment was bridged.
	comments := daemon.getComments()
	if len(comments) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(comments))
	}
	if !strings.Contains(comments[0].Text, "Updated comment text") {
		t.Errorf("comment should contain updated text, got %s", comments[0].Text)
	}

	// Verify nudge fired.
	if nudgedAgent != "test-agent" {
		t.Errorf("expected nudge to test-agent, got %q", nudgedAgent)
	}
}

func TestJiraWebhookHandler_CommentUpdated_MentionTriggersSpawn(t *testing.T) {
	daemon := newMockJiraWebhookDaemon()

	mentionDaemon := &mockJiraMentionDaemon{
		createID: "task-1",
		spawnID:  "agent-1",
		triggerBeads: []*beadsapi.BeadDetail{
			{Fields: map[string]string{
				"action": "spawn_agent", "project": "gardenio",
				"role": "gardenio-crew", "enabled": "true",
			}},
		},
	}
	mentionHandler := NewJiraMentionHandler(JiraMentionHandlerConfig{
		Users:   allowJiraMentionUsers{},
		Daemon:  mentionDaemon,
		Project: "gardenio",
		Logger:  slog.Default(),
	})

	handler := JiraWebhookHandler(JiraWebhookConfig{
		Daemon:         daemon,
		DevMode:        true,
		MentionHandler: mentionHandler,
		Logger:         slog.Default(),
	})

	// Edit a comment to add @gardenio mention.
	body := makeJiraWebhookPayload("comment_updated", "PE-1234", "@gardenio investigate this after update", "Editor")
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Mention handler should have spawned an agent.
	if len(mentionDaemon.spawned) != 1 {
		t.Errorf("expected 1 agent spawned from comment_updated mention, got %d", len(mentionDaemon.spawned))
	}
}

func TestJiraWebhookHandler_CommentUpdated_DedupPreventsDoubleSpawn(t *testing.T) {
	// If comment_created already triggered a spawn, comment_updated should not double-spawn.
	mentionDaemon := &mockJiraMentionDaemon{
		createID: "task-1",
		spawnID:  "agent-1",
		beads: []*beadsapi.BeadDetail{
			{
				ID:     "existing-task",
				Status: "in_progress",
				Fields: map[string]string{
					"jira_key":     "PE-1234",
					"spawn_source": "jira-mention",
				},
			},
		},
	}
	mentionHandler := NewJiraMentionHandler(JiraMentionHandlerConfig{
		Users:   allowJiraMentionUsers{},
		Daemon:  mentionDaemon,
		Project: "gardenio",
		Logger:  slog.Default(),
	})

	daemon := newMockJiraWebhookDaemon()
	handler := JiraWebhookHandler(JiraWebhookConfig{
		Daemon:         daemon,
		DevMode:        true,
		MentionHandler: mentionHandler,
		Logger:         slog.Default(),
	})

	body := makeJiraWebhookPayload("comment_updated", "PE-1234", "@gardenio investigate this again", "Editor")
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Dedup should prevent second spawn.
	if len(mentionDaemon.spawned) != 0 {
		t.Errorf("expected 0 agents spawned (dedup), got %d", len(mentionDaemon.spawned))
	}
}

// TestJiraWebhookHandler_RejectsOversizeBody is the H2 regression test from
// AUDIT-2026-04-18 §H2 (ka-0000002162). A webhook POST whose body exceeds
// jiraMaxBodyBytes must be rejected before HMAC verification allocates
// unbounded memory. We assert the handler returns a non-2xx status and
// no bead mutations occurred.
func TestJiraWebhookHandler_RejectsOversizeBody(t *testing.T) {
	secret := "test-webhook-secret"
	daemon := newMockJiraWebhookDaemon()
	daemon.beads["bead-1"] = &beadsapi.BeadDetail{
		ID:     "bead-1",
		Fields: map[string]string{"jira_key": "PE-1234"},
	}

	handler := JiraWebhookHandler(JiraWebhookConfig{
		Daemon:        daemon,
		WebhookSecret: secret,
		Logger:        slog.Default(),
	})

	// Body one byte over the cap. MaxBytesReader must trip during io.ReadAll
	// before verifyJiraHMAC can be called.
	oversize := bytes.Repeat([]byte("A"), jiraMaxBodyBytes+1)
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(oversize))
	req.Header.Set("X-Hub-Signature", signPayload(oversize, secret))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code/100 == 2 {
		t.Errorf("expected non-2xx for oversize body, got %d", w.Code)
	}

	if got := len(daemon.getComments()); got != 0 {
		t.Fatalf("daemon AddComment fired from oversize request: got %d comments", got)
	}
}

// TestJiraWebhookHandler_PersistsJiraCommentID verifies the originating Jira
// comment id is stored on the matched task bead (F3 — previously only logged),
// so `gb jira reply` can thread to the comment that triggered the wake.
func TestJiraWebhookHandler_PersistsJiraCommentID(t *testing.T) {
	daemon := newMockJiraWebhookDaemon()
	daemon.beads["bead-1"] = &beadsapi.BeadDetail{
		ID:       "bead-1",
		Status:   "in_progress",
		Assignee: "test-agent",
		Fields:   map[string]string{"jira_key": "PE-1234"},
	}

	handler := JiraWebhookHandler(JiraWebhookConfig{
		Daemon:  daemon,
		Nudge:   func(_ context.Context, _, _ string) error { return nil },
		DevMode: true,
		Logger:  slog.Default(),
	})

	body := makeJiraWebhookPayload("comment_created", "PE-1234", "please adjust", "John Doe")
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	bead := daemon.getBead("bead-1")
	// makeJiraWebhookPayload stamps comment id "12345".
	if got := bead.Fields["jira_comment_id"]; got != "12345" {
		t.Errorf("jira_comment_id = %q; want 12345 persisted on the task bead", got)
	}
}

// TestJiraWebhookHandler_ClosedTaskStillBridged verifies a follow-up on a
// ticket whose task bead is CLOSED still bridges the comment and delivers to
// the assignee — the core F3 entry condition (the agent finished and closed
// its task before the operator replied).
func TestJiraWebhookHandler_ClosedTaskStillBridged(t *testing.T) {
	daemon := newMockJiraWebhookDaemon()
	daemon.beads["bead-1"] = &beadsapi.BeadDetail{
		ID:       "bead-1",
		Status:   "closed",
		Assignee: "test-agent",
		Fields:   map[string]string{"jira_key": "PE-1234"},
	}

	var nudgedAgent string
	handler := JiraWebhookHandler(JiraWebhookConfig{
		Daemon: daemon,
		Nudge: func(_ context.Context, agent, _ string) error {
			nudgedAgent = agent
			return nil
		},
		DevMode: true,
		Logger:  slog.Default(),
	})

	body := makeJiraWebhookPayload("comment_created", "PE-1234", "one more thing", "John Doe")
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if len(daemon.getComments()) != 1 {
		t.Error("expected comment bridged to closed task bead")
	}
	if nudgedAgent != "test-agent" {
		t.Errorf("expected delivery to test-agent for closed task, got %q", nudgedAgent)
	}
}

// newJiraFollowUpResolver builds a resolver whose daemon holds one STOPPED
// agent (terminal agent_state) named agentName, ready to assert the respawn.
func newJiraFollowUpResolver(agentName string) (*JiraAgentResolver, *mockJiraResolverDaemon) {
	daemon := &mockJiraResolverDaemon{
		agentBeads: map[string]*beadsapi.BeadDetail{
			agentName: {
				ID:    "agent-bead-stale",
				Title: agentName,
				Fields: map[string]string{
					"agent":       agentName,
					"agent_state": "done",
				},
			},
		},
		spawnID:      "respawned-agent-bead",
		triggerBeads: jiraTestSpawnTriggerBeads("gardenio"),
	}
	resolver := NewJiraAgentResolver(JiraAgentResolverConfig{
		Daemon: daemon,
		Nudge:  subscriberNudge(daemon.findAnyAgent),
		Logger: jiraResolverTestLogger(),
	})
	return resolver, daemon
}

// TestJiraWebhookHandler_StoppedAgent_RespawnsViaResolver drives the full F3
// webhook path: a follow-up comment on a ticket whose assigned agent has
// STOPPED re-spawns the agent (same name, jira_comment_id stamped) instead of
// dropping the conversation.
func TestJiraWebhookHandler_StoppedAgent_RespawnsViaResolver(t *testing.T) {
	daemon := newMockJiraWebhookDaemon()
	daemon.beads["bead-1"] = &beadsapi.BeadDetail{
		ID:       "bead-1",
		Status:   "closed",
		Assignee: "jira-pe-1234-abcd",
		Labels:   []string{"project:gardenio", "jira-mention"},
		Fields:   map[string]string{"jira_key": "PE-1234"},
	}

	resolver, resolverDaemon := newJiraFollowUpResolver("jira-pe-1234-abcd")

	var nudged bool
	handler := JiraWebhookHandler(JiraWebhookConfig{
		Daemon:   daemon,
		Nudge:    func(_ context.Context, _, _ string) error { nudged = true; return nil },
		Resolver: resolver,
		DevMode:  true,
		Logger:   slog.Default(),
	})

	body := makeJiraWebhookPayload("comment_created", "PE-1234", "any update on this?", "John Doe")
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// The stale terminal bead is closed, then the SAME agent name is respawned
	// with the originating comment id and the follow-up as prompt.
	if closed := resolverDaemon.getClosed(); len(closed) != 1 || closed[0] != "agent-bead-stale" {
		t.Errorf("closed = %v; want [agent-bead-stale]", closed)
	}
	spawned := resolverDaemon.getSpawned()
	if len(spawned) != 1 {
		t.Fatalf("expected 1 respawn via resolver, got %d", len(spawned))
	}
	if spawned[0].AgentName != "jira-pe-1234-abcd" {
		t.Errorf("respawn name = %q; want same agent name", spawned[0].AgentName)
	}
	if spawned[0].Project != "gardenio" {
		t.Errorf("respawn project = %q; want gardenio (from task bead labels)", spawned[0].Project)
	}
	if got := spawned[0].Extra["jira_comment_id"]; got != "12345" {
		t.Errorf("jira_comment_id = %q; want 12345 on respawned agent bead", got)
	}
	if spawned[0].TaskID != "bead-1" {
		t.Errorf("respawn task = %q; want bead-1", spawned[0].TaskID)
	}
	if !strings.Contains(spawned[0].Prompt, "any update on this?") {
		t.Errorf("respawn prompt should carry the follow-up comment, got %q", spawned[0].Prompt)
	}
	// The raw NudgeFunc must not fire — the resolver owned delivery.
	if nudged {
		t.Error("legacy nudge should not fire when resolver handles the follow-up")
	}
}

// TestJiraWebhookHandler_GitLabCrosslink_NoWake is the headline regression
// guard for ka-0000005445: GitLab's automated "mentioned this issue in a merge
// request" cross-reference must NOT wake the ticket's agent. These notices
// arrive unprompted whenever any branch anywhere cites the key, and because the
// task-bead lookup is deliberately all-statuses, an F3 respawn off one
// resurrects a FINISHED agent with nothing to do. Worse, the loop
// self-sustains: the woken agent files a bug, the fix branch cites the ticket,
// and that crosslink wakes it again. Observed live on DEVOPS-738 (3 respawns of
// the same agent in 64 minutes, the last two off its own predecessors' MRs).
//
// The comment must still be bridged to the bead — only the wake is suppressed.
func TestJiraWebhookHandler_GitLabCrosslink_NoWake(t *testing.T) {
	// The verbatim body GitLab posted on DEVOPS-738 as comment 112509, which
	// respawned this very agent 2 seconds later.
	const crosslink = "gardenio mentioned this issue in a merge request of " +
		"Pi Health / Core ACME / gardenio on branch " +
		"fix/ka-0000000661-jira-bridge-502:fix(gardenio/bridge,kbeads): " +
		"jira reply 502 on markdown + message-bead schema drift (ka-0000000661)"

	daemon := newMockJiraWebhookDaemon()
	daemon.beads["bead-1"] = &beadsapi.BeadDetail{
		ID:       "bead-1",
		Status:   "closed",
		Assignee: "jira-devops-738-abcd",
		Labels:   []string{"project:gardenio", "jira-mention"},
		Fields:   map[string]string{"jira_key": "DEVOPS-738"},
	}

	resolver, resolverDaemon := newJiraFollowUpResolver("jira-devops-738-abcd")

	var nudged bool
	handler := JiraWebhookHandler(JiraWebhookConfig{
		Daemon:   daemon,
		Nudge:    func(_ context.Context, _, _ string) error { nudged = true; return nil },
		Resolver: resolver,
		DevMode:  true,
		Logger:   slog.Default(),
	})

	body := makeJiraWebhookPayload("comment_created", "DEVOPS-738", crosslink, "Pi Health Root")
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if spawned := resolverDaemon.getSpawned(); len(spawned) != 0 {
		t.Errorf("GitLab crosslink must not respawn an agent, got %d spawn(s)", len(spawned))
	}
	if nudged {
		t.Error("GitLab crosslink must not nudge the agent either")
	}
	// The crosslink is still recorded on the bead so it stays visible.
	if got := daemon.getComments(); len(got) != 1 {
		t.Errorf("crosslink should still be bridged to the bead, got %d comment(s)", len(got))
	}
}

// TestIsGitLabCrosslinkComment pins the classifier: GitLab's generated object
// phrasings are automation, while a human merely talking ABOUT a merge request
// is a real follow-up that must still wake the agent (ka-0000005445).
func TestIsGitLabCrosslinkComment(t *testing.T) {
	tests := []struct {
		name string
		text string
		want bool
	}{
		{"merge request crosslink", "Gardenio Agent mentioned this issue in a merge request of Pi Health / Core ACME / monorepo on branch feat/x:feat: y", true},
		{"commit crosslink", "Matthew Baker mentioned this issue in a commit of Pi Health / Core ACME / monorepo on branch main:Merge branch 'z'", true},
		{"branch crosslink", "someone mentioned this issue in a branch of Some / Project", true},
		{"issue crosslink", "someone mentioned this issue in an issue of Some / Project", true},
		{"case insensitive", "GARDENIO MENTIONED THIS ISSUE IN A MERGE REQUEST of x", true},
		// The wiki-markup forms below are the REAL prod input (ka-0000006177):
		// GitLab's Jira Server integration wraps the actor and the object phrase
		// in [display|url] links, so the object marker is split by brackets and a
		// raw substring scan misses it. These are the verbatim shapes stored on
		// DEVOPS-897 (comments 112557/112560/112573) — the bodies that woke the
		// agent even with the guard deployed, because the classifier saw the
		// bracketed form and returned false.
		{"wiki-markup merge request crosslink", "[Gardenio Agent|https://gitlab.com/gardeniobot] mentioned this issue in [a merge request|https://gitlab.com/Acme/Platform/gardenio/-/merge_requests/6767] of [Pi Health / Core ACME / gardenio|https://gitlab.com/Acme/Platform/gardenio] on branch [fix/x|https://gitlab.com/x]:{quote}y{quote}", true},
		{"wiki-markup commit crosslink", "[Gardenio Agent|https://gitlab.com/gardeniobot] mentioned this issue in [a commit|https://gitlab.com/Acme/Platform/gardenio/-/commit/951e92ce] of [Pi Health / Core ACME / gardenio|https://gitlab.com/Acme/Platform/gardenio]:{quote}fix(gardenio/bridge): don't wake Jira agents on the bridge's own comments (ka-0000004998){quote}", true},
		{"human follow-up", "any update on this?", false},
		{"human asking about an MR", "did the merge request for this land yet?", false},
		{"human using similar words", "I mentioned this issue in standup — please take another look", false},
		// A human linking to an MR they are talking ABOUT must still wake the
		// agent: flattening the link leaves "did the [a merge request] land?" →
		// "did the a merge request land?", which does not carry the "mentioned
		// this issue in ..." verb, so it is correctly NOT classified as automation.
		{"human wiki-link to an MR", "did [a merge request|https://gitlab.com/x/-/merge_requests/1] for this land yet?", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isGitLabCrosslinkComment(tt.text); got != tt.want {
				t.Errorf("isGitLabCrosslinkComment(%q) = %v; want %v", tt.text, got, tt.want)
			}
		})
	}
}

// TestFlattenJiraWikiLinks pins the wiki-markup link flattener that makes the
// crosslink classifier work against prod's Jira Server bodies (ka-0000006177).
// It must reduce [display|target] to display, be a no-op on bracket-free text
// (the ADF-flattened Jira Cloud form), and never let a link span into
// surrounding prose.
func TestFlattenJiraWikiLinks(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"no brackets is a no-op", "mentioned this issue in a commit of X", "mentioned this issue in a commit of X"},
		{"single link", "in [a commit|https://x] of", "in a commit of"},
		{"two links", "[Gardenio Agent|https://a] mentioned this issue in [a merge request|https://b] of", "Gardenio Agent mentioned this issue in a merge request of"},
		{"empty", "", ""},
		// The alias segment stops at the first '|', so a link cannot swallow the
		// text between two separate links.
		{"adjacent links do not merge", "[a|u1] x [b|u2]", "a x b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := flattenJiraWikiLinks(tt.in); got != tt.want {
				t.Errorf("flattenJiraWikiLinks(%q) = %q; want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestJiraWebhookHandler_GitLabCrosslinkWikiMarkup_NoWake is the end-to-end
// regression guard for ka-0000006177: the SHIPPED crosslink guard
// (ka-0000005445) was deployed and still let GitLab crosslinks wake the agent,
// because prod's Jira Server delivers the notice as WIKI MARKUP — the object
// phrase is a [display|url] link — and the substring classifier could not match
// across the brackets. This exercises the full webhook path with the VERBATIM
// wiki-markup body GitLab posted on DEVOPS-897 (comment 112573), which nudged
// the assigned agent in the deployed-with-fix build. The comment must still be
// bridged to the bead; only the wake is suppressed.
func TestJiraWebhookHandler_GitLabCrosslinkWikiMarkup_NoWake(t *testing.T) {
	const crosslink = "[Gardenio Agent|https://gitlab.com/gardeniobot] mentioned this issue in " +
		"[a commit|https://gitlab.com/Acme/Platform/gardenio/-/commit/951e92ce7760368a791b909499f61650f1de6101] " +
		"of [Pi Health / Core ACME / gardenio|https://gitlab.com/Acme/Platform/gardenio]:" +
		"{quote}fix(gardenio/bridge): don't wake Jira agents on the bridge's own comments (ka-0000004998){quote}"

	daemon := newMockJiraWebhookDaemon()
	daemon.beads["bead-1"] = &beadsapi.BeadDetail{
		ID:       "bead-1",
		Status:   "closed",
		Assignee: "monorepo-auto-spawn-monorepo-crew-on-new-qrrr",
		Labels:   []string{"project:monorepo", "jira-mention"},
		Fields:   map[string]string{"jira_key": "DEVOPS-897"},
	}

	resolver, resolverDaemon := newJiraFollowUpResolver("monorepo-auto-spawn-monorepo-crew-on-new-qrrr")

	var nudged bool
	handler := JiraWebhookHandler(JiraWebhookConfig{
		Daemon:   daemon,
		Nudge:    func(_ context.Context, _, _ string) error { nudged = true; return nil },
		Resolver: resolver,
		DevMode:  true,
		Logger:   slog.Default(),
	})

	// Posted under "Pi Health Root" — the shared GitLab⇄Jira integration account
	// that also posts legitimate human-visible traffic, so author identity cannot
	// separate it.
	body := makeJiraWebhookPayload("comment_created", "DEVOPS-897", crosslink, "Pi Health Root")
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if spawned := resolverDaemon.getSpawned(); len(spawned) != 0 {
		t.Errorf("wiki-markup GitLab crosslink must not respawn an agent, got %d spawn(s)", len(spawned))
	}
	if nudged {
		t.Error("wiki-markup GitLab crosslink must not nudge the agent either")
	}
	if got := daemon.getComments(); len(got) != 1 {
		t.Errorf("crosslink should still be bridged to the bead, got %d comment(s)", len(got))
	}
}

// TestJiraWebhookHandler_BridgeGeneratedComment_NoWake is the regression guard
// for ka-0000004998, the sibling of the crosslink loop above: the bridge must
// not wake an agent off a comment IT posted. Observed live on DEVOPS-897 as a
// three-second chain — the gitlab-bridge reconcile sweep re-opened a task bead
// whose MR had been unmerged for 35 days, jira_sync saw the re-opened bead's
// mr_url and posted "Automated MR created", and this webhook respawned the
// assignee off that comment. The woken agent could not land the MR (protected
// main needs an operator), so the sweep re-opened the bead and it repeated.
//
// As with crosslinks, the comment must still be bridged to the bead.
func TestJiraWebhookHandler_BridgeGeneratedComment_NoWake(t *testing.T) {
	// The verbatim body of DEVOPS-897 comment 112552, which respawned this very
	// agent 2 seconds later — including the attribution line jira_sync appends.
	const generated = "Automated MR created: " +
		"https://gitlab.com/Acme/Platform/monorepo/-/merge_requests/1618" +
		"\n\nActed by oidc:matthew.baker@acme.example"

	daemon := newMockJiraWebhookDaemon()
	daemon.beads["bead-1"] = &beadsapi.BeadDetail{
		ID:       "bead-1",
		Status:   "open",
		Assignee: "monorepo-auto-spawn-monorepo-crew-on-new-qrrr",
		Labels:   []string{"project:monorepo", "jira-mention"},
		Fields:   map[string]string{"jira_key": "DEVOPS-897"},
	}

	resolver, resolverDaemon := newJiraFollowUpResolver("monorepo-auto-spawn-monorepo-crew-on-new-qrrr")

	var nudged bool
	handler := JiraWebhookHandler(JiraWebhookConfig{
		Daemon:   daemon,
		Nudge:    func(_ context.Context, _, _ string) error { nudged = true; return nil },
		Resolver: resolver,
		DevMode:  true,
		Logger:   slog.Default(),
	})

	// Posted under "Automated Gardenio Agent" — the second gardenio bot account,
	// which the singular BotAccountID guard would miss even if it were set.
	body := makeJiraWebhookPayload("comment_created", "DEVOPS-897", generated, "Automated Gardenio Agent")
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if spawned := resolverDaemon.getSpawned(); len(spawned) != 0 {
		t.Errorf("bridge-generated comment must not respawn an agent, got %d spawn(s)", len(spawned))
	}
	if nudged {
		t.Error("bridge-generated comment must not nudge the agent either")
	}
	if got := daemon.getComments(); len(got) != 1 {
		t.Errorf("bridge-generated comment should still be bridged to the bead, got %d comment(s)", len(got))
	}
}

// TestJiraWebhookHandler_BeadClosureNotice_NoWake is the regression guard for
// ka-0000004442 — the fourth of jira_sync's withAttribution call sites, missed
// when bridgeGeneratedMarkers was introduced for the other three.
//
// This is the tightest loop in the family and it needs no external input: the
// agent closes its own task bead, the SSE close event drives syncBeadClosure to
// post the notice, and this webhook wakes the agent that just closed it. The
// woken agent has nothing to do, investigates, closes its bead, and the notice
// fires again. Observed live on DEVOPS-738 — Finished since 2026-06-16.
func TestJiraWebhookHandler_BeadClosureNotice_NoWake(t *testing.T) {
	// The verbatim body of the DEVOPS-738 comment that woke this agent,
	// including the attribution line syncBeadClosure appends.
	const closure = "Task bead closed in beads system (bead gd-T0OtLbTYKr)." +
		"\n\nActed by slack:U0OPERATOR1"

	daemon := newMockJiraWebhookDaemon()
	daemon.beads["bead-1"] = &beadsapi.BeadDetail{
		ID:       "bead-1",
		Status:   "closed",
		Assignee: "monorepo-auto-spawn-monorepo-crew-on-new-l5gv",
		Labels:   []string{"project:monorepo", "jira-mention"},
		Fields:   map[string]string{"jira_key": "DEVOPS-738"},
	}

	resolver, resolverDaemon := newJiraFollowUpResolver("monorepo-auto-spawn-monorepo-crew-on-new-l5gv")

	var nudged bool
	handler := JiraWebhookHandler(JiraWebhookConfig{
		Daemon:   daemon,
		Nudge:    func(_ context.Context, _, _ string) error { nudged = true; return nil },
		Resolver: resolver,
		DevMode:  true,
		Logger:   slog.Default(),
	})

	body := makeJiraWebhookPayload("comment_created", "DEVOPS-738", closure, "Automated Gardenio Agent")
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if spawned := resolverDaemon.getSpawned(); len(spawned) != 0 {
		t.Errorf("bead-closure notice must not respawn an agent, got %d spawn(s)", len(spawned))
	}
	if nudged {
		t.Error("bead-closure notice must not nudge the agent either")
	}
	if got := daemon.getComments(); len(got) != 1 {
		t.Errorf("bead-closure notice should still be bridged to the bead, got %d comment(s)", len(got))
	}
}

// TestJiraWebhookHandler_BotAuthoredFreeText_NoWake is the regression guard for
// ka-0000003581 — the 4th self-wake variant and the first that content
// classification CANNOT catch. A gardenio agent's own FREE-TEXT `gb jira reply`
// posts arbitrary prose under a bot account, which no marker list can
// enumerate, so the guard keys on the AUTHOR's accountId instead. Observed live
// on PE-7289: comment 115851 (a fixVersion write-up, verified NOT a bridge
// template) F3-respawned the assignee of a task closed 5 months earlier, 4
// seconds after it was posted, and the woken agent's natural reply would post
// another such comment — a self-sustaining loop with no external re-trigger.
//
// The body deliberately opens like real agent prose ("Fix version set to …"),
// which isBridgeGeneratedComment does NOT match — proving the author-set guard
// is what suppresses the wake, not the content guards. As with the sibling
// guards, the comment must still be bridged to the bead.
func TestJiraWebhookHandler_BotAuthoredFreeText_NoWake(t *testing.T) {
	// Verbatim shape of PE-7289 comment 115851 — arbitrary agent prose, not a
	// bridge template. isBridgeGeneratedComment returns false on this by
	// construction (it matches 4 fixed prefixes; agent prose is unbounded).
	const freeText = "Fix version set to 2.5.0 (Morning Sync action item: the " +
		"date-format fix landed on IssueLogDataGrid via commit 1df342a8a, first " +
		"tagged v2.5.0-rc3). Ticket correctly sits at Validate awaiting QA."

	// Sanity: this body must NOT be caught by the content guard, or the test
	// would pass for the wrong reason.
	if isBridgeGeneratedComment(freeText) {
		t.Fatal("test body must be free-text that the content guard does NOT match; " +
			"the author-set guard is what this test exercises")
	}

	// The "Gardenio Agent" bot account that authored 115851.
	const botAccountID = "712020:66bb0993-fdcf-4652-8941-d4ac9a81b093"

	daemon := newMockJiraWebhookDaemon()
	daemon.beads["bead-1"] = &beadsapi.BeadDetail{
		ID:       "bead-1",
		Status:   "closed", // closed 5 months ago, like gd-J7pyvAOZz4
		Assignee: "monorepo-auto-spawn-monorepo-crew-on-new-pe72",
		Labels:   []string{"project:monorepo", "jira-mention"},
		Fields:   map[string]string{"jira_key": "PE-7289"},
	}

	resolver, resolverDaemon := newJiraFollowUpResolver("monorepo-auto-spawn-monorepo-crew-on-new-pe72")

	var nudged bool
	handler := JiraWebhookHandler(JiraWebhookConfig{
		Daemon:   daemon,
		Nudge:    func(_ context.Context, _, _ string) error { nudged = true; return nil },
		Resolver: resolver,
		// The SET seeded with both gardenio bot accounts — the fix under test.
		BotAccountIDs: []string{
			"712020:66bb0993-fdcf-4652-8941-d4ac9a81b093",
			"712020:a2f3e121-ea2e-48d1-aa6e-241713778a2b",
		},
		DevMode: true,
		Logger:  slog.Default(),
	})

	body := makeJiraWebhookPayloadWithAuthorID(
		"comment_created", "PE-7289", freeText, "Gardenio Agent", botAccountID)
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if spawned := resolverDaemon.getSpawned(); len(spawned) != 0 {
		t.Errorf("bot-authored free-text comment must not respawn an agent, got %d spawn(s)", len(spawned))
	}
	if nudged {
		t.Error("bot-authored free-text comment must not nudge the agent either")
	}
	if got := daemon.getComments(); len(got) != 1 {
		t.Errorf("bot-authored comment should still be bridged to the bead, got %d comment(s)", len(got))
	}
}

// TestJiraWebhookHandler_HumanFreeText_StillWakes is the carve-out twin of the
// test above: the author-set guard must NOT swallow a real human follow-up. A
// human comment is authored by a human account (not in JIRA_BOT_ACCOUNT_IDS),
// so the same free-text body that is suppressed for the bot MUST still nudge
// the assignee when a human posts it.
func TestJiraWebhookHandler_HumanFreeText_StillWakes(t *testing.T) {
	const freeText = "Please take another look — QA hit a regression on the grid."

	daemon := newMockJiraWebhookDaemon()
	daemon.beads["bead-1"] = &beadsapi.BeadDetail{
		ID:       "bead-1",
		Status:   "in_progress",
		Assignee: "test-agent",
		Fields:   map[string]string{"jira_key": "PE-7289"},
	}

	var nudgedAgent string
	handler := JiraWebhookHandler(JiraWebhookConfig{
		Daemon: daemon,
		Nudge:  func(_ context.Context, agent, _ string) error { nudgedAgent = agent; return nil },
		BotAccountIDs: []string{
			"712020:66bb0993-fdcf-4652-8941-d4ac9a81b093",
			"712020:a2f3e121-ea2e-48d1-aa6e-241713778a2b",
		},
		DevMode: true,
		Logger:  slog.Default(),
	})

	// Human account id — not a gardenio bot.
	body := makeJiraWebhookPayloadWithAuthorID(
		"comment_created", "PE-7289", freeText, "Matt Baker", "557058:human-acct-1")
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if nudgedAgent != "test-agent" {
		t.Errorf("human follow-up must still wake the assignee, got nudge=%q", nudgedAgent)
	}
	if got := daemon.getComments(); len(got) != 1 {
		t.Errorf("human comment should be bridged to the bead, got %d comment(s)", len(got))
	}
}

// TestIsBotAuthoredComment unit-tests the author-set membership check directly,
// including the case-insensitive / whitespace-trimmed matching that mirrors
// normalizeAccountIDs on the mention path.
func TestIsBotAuthoredComment(t *testing.T) {
	set := []string{
		"712020:66bb0993-fdcf-4652-8941-d4ac9a81b093",
		"712020:a2f3e121-ea2e-48d1-aa6e-241713778a2b",
	}
	cases := []struct {
		name      string
		accountID string
		set       []string
		want      bool
	}{
		{"first bot", "712020:66bb0993-fdcf-4652-8941-d4ac9a81b093", set, true},
		{"second bot", "712020:a2f3e121-ea2e-48d1-aa6e-241713778a2b", set, true},
		{"case-insensitive", "712020:66BB0993-FDCF-4652-8941-D4AC9A81B093", set, true},
		{"whitespace-padded set entry", "acct-1", []string{"  acct-1  "}, true},
		{"human account", "557058:human-acct-1", set, false},
		{"empty account", "", set, false},
		{"empty set", "712020:66bb0993-fdcf-4652-8941-d4ac9a81b093", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isBotAuthoredComment(tc.accountID, tc.set); got != tc.want {
				t.Errorf("isBotAuthoredComment(%q, %v) = %v; want %v",
					tc.accountID, tc.set, got, tc.want)
			}
		})
	}
}

// TestJiraWebhookHandler_GitLabCrosslink_CommentUpdated_NoWake is the EDIT-path
// twin of TestJiraWebhookHandler_GitLabCrosslink_NoWake, and the executable
// answer to the "CHECK BEFORE TICKING THE BOX" acceptance gate on task
// ka-0000004484: before the operator subscribes the gardenio-mentions webhook to
// `comment_updated`, both self-sustaining wake-loop suppression guards
// (ka-0000005445 crosslink, ka-0000004998 bridge-generated) must be proven to
// fire on the EDIT path, not just on comment_created — re-opening either loop is
// worse than the gap the box closes.
//
// The guards live in the shared handleJiraCommentWebhook (dispatched for both
// comment_created and comment_updated at jira_webhook.go), so they fire today.
// This test locks that in: a GitLab crosslink delivered as a comment EDIT must
// still not respawn or nudge the ticket's agent, while remaining bridged to the
// bead. Without this pin, a future refactor that special-cased the edit path
// could silently re-open the DEVOPS-738 respawn loop the moment the box is
// ticked.
func TestJiraWebhookHandler_GitLabCrosslink_CommentUpdated_NoWake(t *testing.T) {
	const crosslink = "gardenio mentioned this issue in a merge request of " +
		"Pi Health / Core ACME / gardenio on branch " +
		"fix/ka-0000000661-jira-bridge-502:fix(gardenio/bridge,kbeads): " +
		"jira reply 502 on markdown + message-bead schema drift (ka-0000000661)"

	daemon := newMockJiraWebhookDaemon()
	daemon.beads["bead-1"] = &beadsapi.BeadDetail{
		ID:       "bead-1",
		Status:   "closed",
		Assignee: "jira-devops-738-abcd",
		Labels:   []string{"project:gardenio", "jira-mention"},
		Fields:   map[string]string{"jira_key": "DEVOPS-738"},
	}

	resolver, resolverDaemon := newJiraFollowUpResolver("jira-devops-738-abcd")

	var nudged bool
	handler := JiraWebhookHandler(JiraWebhookConfig{
		Daemon:   daemon,
		Nudge:    func(_ context.Context, _, _ string) error { nudged = true; return nil },
		Resolver: resolver,
		DevMode:  true,
		Logger:   slog.Default(),
	})

	body := makeJiraWebhookPayload("comment_updated", "DEVOPS-738", crosslink, "Pi Health Root")
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if spawned := resolverDaemon.getSpawned(); len(spawned) != 0 {
		t.Errorf("edited-in GitLab crosslink must not respawn an agent, got %d spawn(s)", len(spawned))
	}
	if nudged {
		t.Error("edited-in GitLab crosslink must not nudge the agent either")
	}
	if got := daemon.getComments(); len(got) != 1 {
		t.Errorf("crosslink should still be bridged to the bead on the edit path, got %d comment(s)", len(got))
	}
}

// TestJiraWebhookHandler_GitLabCrosslinkWikiMarkup_CommentUpdated_NoWake is the
// EDIT-path twin of TestJiraWebhookHandler_GitLabCrosslinkWikiMarkup_NoWake
// (ka-0000006177). Prod's Jira Server delivers crosslinks as wiki markup, and
// the flattenJiraWikiLinks pre-pass is what makes the classifier match them; a
// comment can be delivered as wiki markup on the edit path just as on create, so
// the flatten+match guard must hold there too before comment_updated is enabled.
func TestJiraWebhookHandler_GitLabCrosslinkWikiMarkup_CommentUpdated_NoWake(t *testing.T) {
	const crosslink = "[Gardenio Agent|https://gitlab.com/gardeniobot] mentioned this issue in " +
		"[a commit|https://gitlab.com/Acme/Platform/gardenio/-/commit/951e92ce7760368a791b909499f61650f1de6101] " +
		"of [Pi Health / Core ACME / gardenio|https://gitlab.com/Acme/Platform/gardenio]:" +
		"{quote}fix(gardenio/bridge): don't wake Jira agents on the bridge's own comments (ka-0000004998){quote}"

	daemon := newMockJiraWebhookDaemon()
	daemon.beads["bead-1"] = &beadsapi.BeadDetail{
		ID:       "bead-1",
		Status:   "closed",
		Assignee: "monorepo-auto-spawn-monorepo-crew-on-new-qrrr",
		Labels:   []string{"project:monorepo", "jira-mention"},
		Fields:   map[string]string{"jira_key": "DEVOPS-897"},
	}

	resolver, resolverDaemon := newJiraFollowUpResolver("monorepo-auto-spawn-monorepo-crew-on-new-qrrr")

	var nudged bool
	handler := JiraWebhookHandler(JiraWebhookConfig{
		Daemon:   daemon,
		Nudge:    func(_ context.Context, _, _ string) error { nudged = true; return nil },
		Resolver: resolver,
		DevMode:  true,
		Logger:   slog.Default(),
	})

	body := makeJiraWebhookPayload("comment_updated", "DEVOPS-897", crosslink, "Pi Health Root")
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if spawned := resolverDaemon.getSpawned(); len(spawned) != 0 {
		t.Errorf("edited-in wiki-markup GitLab crosslink must not respawn an agent, got %d spawn(s)", len(spawned))
	}
	if nudged {
		t.Error("edited-in wiki-markup GitLab crosslink must not nudge the agent either")
	}
	if got := daemon.getComments(); len(got) != 1 {
		t.Errorf("crosslink should still be bridged to the bead on the edit path, got %d comment(s)", len(got))
	}
}

// TestJiraWebhookHandler_BridgeGeneratedComment_CommentUpdated_NoWake is the
// EDIT-path twin of TestJiraWebhookHandler_BridgeGeneratedComment_NoWake
// (ka-0000004998): the bridge must not wake an agent off a comment it posted
// itself, whether that comment arrives as a create or an edit. Completes the
// pair of guards the console-tick task requires proven on the edit path.
func TestJiraWebhookHandler_BridgeGeneratedComment_CommentUpdated_NoWake(t *testing.T) {
	const generated = "Automated MR created: " +
		"https://gitlab.com/Acme/Platform/monorepo/-/merge_requests/1618" +
		"\n\nActed by oidc:matthew.baker@acme.example"

	daemon := newMockJiraWebhookDaemon()
	daemon.beads["bead-1"] = &beadsapi.BeadDetail{
		ID:       "bead-1",
		Status:   "open",
		Assignee: "monorepo-auto-spawn-monorepo-crew-on-new-qrrr",
		Labels:   []string{"project:monorepo", "jira-mention"},
		Fields:   map[string]string{"jira_key": "DEVOPS-897"},
	}

	resolver, resolverDaemon := newJiraFollowUpResolver("monorepo-auto-spawn-monorepo-crew-on-new-qrrr")

	var nudged bool
	handler := JiraWebhookHandler(JiraWebhookConfig{
		Daemon:   daemon,
		Nudge:    func(_ context.Context, _, _ string) error { nudged = true; return nil },
		Resolver: resolver,
		DevMode:  true,
		Logger:   slog.Default(),
	})

	body := makeJiraWebhookPayload("comment_updated", "DEVOPS-897", generated, "Automated Gardenio Agent")
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if spawned := resolverDaemon.getSpawned(); len(spawned) != 0 {
		t.Errorf("edited-in bridge-generated comment must not respawn an agent, got %d spawn(s)", len(spawned))
	}
	if nudged {
		t.Error("edited-in bridge-generated comment must not nudge the agent either")
	}
	if got := daemon.getComments(); len(got) != 1 {
		t.Errorf("bridge-generated comment should still be bridged to the bead on the edit path, got %d comment(s)", len(got))
	}
}

// TestIsBridgeGeneratedComment pins the classifier against all four of
// jira_sync's withAttribution call sites, and against humans discussing the
// same subjects — a human asking why a bot notice fired is a real follow-up
// that must still wake the agent (ka-0000004998, ka-0000004442).
func TestIsBridgeGeneratedComment(t *testing.T) {
	tests := []struct {
		name string
		text string
		want bool
	}{
		{"MR created (jira_sync.go:539)", "Automated MR created: https://gitlab.com/x/-/merge_requests/1618", true},
		{"MR created with attribution", "Automated MR created: https://gitlab.com/x/-/merge_requests/1618\n\nActed by oidc:matthew.baker@acme.example", true},
		{"claim notice (jira_sync.go:504)", "Gardenio agent monorepo-auto-spawn-monorepo-crew-on-new-qrrr is working on this issue.", true},
		{"MR merged (jira_sync.go:556)", "MR merged — transitioning to Review.", true},
		// The bead-closure notice (ka-0000004442). Prefix-only: the bead ID is
		// interpolated and " MR: <url>" is appended only when the bead carries
		// one, so all three tail shapes must classify the same.
		{"bead closure (jira_sync.go:838)", "Task bead closed in beads system (bead gd-T0OtLbTYKr).", true},
		{"bead closure with attribution", "Task bead closed in beads system (bead gd-T0OtLbTYKr).\n\nActed by slack:U0OPERATOR1", true},
		{"bead closure with MR suffix", "Task bead closed in beads system (bead gd-gK1vRy0fip). MR: https://gitlab.com/x/-/merge_requests/6751", true},
		{"case insensitive", "AUTOMATED MR CREATED: https://gitlab.com/x", true},
		{"leading whitespace", "  Automated MR created: https://gitlab.com/x", true},
		{"human follow-up", "any update on this?", false},
		{"human quoting the notice mid-sentence", "why did \"Automated MR created\" fire twice on this ticket?", false},
		{"human talking about the agent", "can the gardenio agent take another look at this?", false},
		{"human reporting a merge", "I merged the MR — transitioning to Review.", false},
		// The closure marker is prefix-only, so its carve-out rests entirely on
		// prefix specificity. A human MENTIONING a closed bead must still wake
		// the agent — these pin that the prefix is narrow enough to tell the
		// generated sentence from a human talking about the same event.
		{"human asking about a closed bead", "why was the task bead closed in beads system before the MR landed?", false},
		{"human reporting a closure", "I closed the task bead for this ticket — reopen if the fix regresses", false},
		// A human opening by ADDRESSING the agent shares the claim notice's
		// "Gardenio agent " prefix. These are real follow-ups; suppressing one
		// silently drops a request, which is the worst failure this guard can
		// have — hence the claim marker matches prefix AND suffix.
		{"human addressing the agent", "Gardenio agent please take another look at this ticket", false},
		{"human addressing the agent, lowercase", "gardenio agent is this still blocked?", false},
		{"human addressing the agent with a comma", "Gardenio agent, can you re-check the OpsGenie requirement?", false},
		// The real crosslink bodies that woke this agent: automation, but the
		// CROSSLINK guard's job, not this one's. Overlap is fine (either
		// suppresses the wake) — pinned so a future narrowing of one classifier
		// does not silently assume the other still covers these.
		{"GitLab crosslink is not a bridge-generated comment", "Gardenio Agent mentioned this issue in a commit of Pi Health / Core ACME / gardenio on branch fix/x:fix(gardenio/bridge): y", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isBridgeGeneratedComment(tt.text); got != tt.want {
				t.Errorf("isBridgeGeneratedComment(%q) = %v; want %v", tt.text, got, tt.want)
			}
		})
	}
}

// TestJiraWebhookHandler_StaleReMention_RespawnsNotMuted is the end-to-end
// headline regression guard for ka-0000007091 GAP 1 (the Jira twin of GitLab
// ka-0000001106): a follow-up comment that ITSELF re-tags @gardenio on a ticket
// whose only open mention task is STALE (agent crashed/stopped without closing
// it) must NOT be swallowed. Before the fix, HandleMention returned true from
// its dedup branch, the webhook skipped its follow-up loop (mentionHandled==
// true), and the re-mention was delivered to neither path — permanent mute.
// Now the stale re-mention makes HandleMention return false, so the webhook
// follow-up loop runs and RE-SPAWNS the stopped agent via the resolver, exactly
// as a plain follow-up would. This is the "you did not respond to my previous
// comment" fix, mirrored onto Jira.
func TestJiraWebhookHandler_StaleReMention_RespawnsNotMuted(t *testing.T) {
	// The webhook daemon holds the assigned mention task bead (the comment is
	// bridged here, and its assignee drives the follow-up loop).
	daemon := newMockJiraWebhookDaemon()
	daemon.beads["bead-1"] = &beadsapi.BeadDetail{
		ID:       "bead-1",
		Status:   "open",
		Assignee: "jira-pe-1234-abcd",
		Labels:   []string{"project:gardenio", "jira-mention"},
		Fields:   map[string]string{"jira_key": "PE-1234", "spawn_source": "jira-mention"},
	}

	// The mention handler's daemon sees the SAME task as STALE (older than the
	// dedup window) — so a re-mention classifies as a stale re-mention and
	// returns false instead of muting.
	mentionDaemon := &mockJiraMentionDaemon{
		createID:     "should-not-create",
		spawnID:      "should-not-spawn",
		triggerBeads: jiraTestSpawnTriggerBeads("gardenio"),
		beads: []*beadsapi.BeadDetail{
			{
				ID:        "bead-1",
				Status:    "open",
				CreatedAt: time.Now().Add(-30 * time.Minute),
				Fields:    map[string]string{"jira_key": "PE-1234", "spawn_source": "jira-mention"},
			},
		},
	}
	mentionHandler := NewJiraMentionHandler(JiraMentionHandlerConfig{
		Users:   allowJiraMentionUsers{},
		Daemon:  mentionDaemon,
		Project: "gardenio",
		Logger:  slog.Default(),
	})

	// The resolver sees the assigned agent as STOPPED (terminal state) → respawn.
	resolver, resolverDaemon := newJiraFollowUpResolver("jira-pe-1234-abcd")

	var legacyNudged bool
	handler := JiraWebhookHandler(JiraWebhookConfig{
		Daemon:         daemon,
		Nudge:          func(_ context.Context, _, _ string) error { legacyNudged = true; return nil },
		Resolver:       resolver,
		MentionHandler: mentionHandler,
		DevMode:        true,
		Logger:         slog.Default(),
	})

	// The follow-up itself re-tags @gardenio — the worst case in the bead: a
	// re-mention on a ticket with a stale open task.
	body := makeJiraWebhookPayload("comment_created", "PE-1234", "@gardenio you did not respond to my previous comment", "John Doe")
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// The mention handler must NOT have spawned a second agent (it saw only a
	// stale task and deferred to the follow-up loop).
	if n := len(mentionDaemon.spawned); n != 0 {
		t.Errorf("mention handler must not spawn a second agent on a stale re-mention, got %d", n)
	}
	// The follow-up loop must have RE-SPAWNED the stopped agent via the resolver
	// — the delivery that was silently dropped before the fix.
	spawned := resolverDaemon.getSpawned()
	if len(spawned) != 1 {
		t.Fatalf("expected the stale re-mention to respawn the stopped agent, got %d spawns", len(spawned))
	}
	if spawned[0].AgentName != "jira-pe-1234-abcd" {
		t.Errorf("respawn name = %q; want same agent name (session continuity)", spawned[0].AgentName)
	}
	if !strings.Contains(spawned[0].Prompt, "you did not respond") {
		t.Errorf("respawn prompt should carry the re-mention comment, got %q", spawned[0].Prompt)
	}
	if legacyNudged {
		t.Error("legacy nudge should not fire when the resolver handles the re-mention")
	}
}

// TestJiraWebhookHandler_GBFSMFollowUp_RespawnsRegardlessOfOutboxToggle locks in
// the F3 re-spawn half of ka-0000001940: a follow-up comment on a GBFSM ticket
// whose assigned agent has STOPPED re-spawns the agent even when the outbox write
// toggle is OFF, because jiraFollowUpProjectAllowed now uses the spawn gate
// (AllowSpawn), not the write gate. Re-spawning is an agent-lifecycle action, not
// a Jira write, so it must not be blocked by JIRA_OUTBOX_ENABLED.
func TestJiraWebhookHandler_GBFSMFollowUp_RespawnsRegardlessOfOutboxToggle(t *testing.T) {
	for _, outboxOn := range []bool{false, true} {
		daemon := newMockJiraWebhookDaemon()
		daemon.beads["bead-gbfsm"] = &beadsapi.BeadDetail{
			ID:       "bead-gbfsm",
			Status:   "closed",
			Assignee: "jira-gbfsm-123-abcd",
			Labels:   []string{"project:gardenio", "jira-mention"},
			Fields:   map[string]string{"jira_key": "GBFSM-123"},
		}

		resolver, resolverDaemon := newJiraFollowUpResolver("jira-gbfsm-123-abcd")

		handler := JiraWebhookHandler(JiraWebhookConfig{
			Daemon:      daemon,
			Nudge:       func(_ context.Context, _, _ string) error { return nil },
			Resolver:    resolver,
			WritePolicy: storePolicy(outboxOn, AllowlistModeEnforce),
			DevMode:     true,
			Logger:      slog.Default(),
		})

		body := makeJiraWebhookPayload("comment_created", "GBFSM-123", "any update?", "John Doe")
		req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("outbox=%v: expected 200, got %d", outboxOn, w.Code)
		}
		if spawned := resolverDaemon.getSpawned(); len(spawned) != 1 {
			t.Errorf("outbox=%v: expected 1 GBFSM re-spawn (decoupled from outbox toggle), got %d",
				outboxOn, len(spawned))
		}
	}
}

// TestJiraWebhookHandler_LiveAgent_NudgedNotRespawned verifies the resolver
// path still nudges a LIVE agent (channel-first via the wired NudgeFunc) and
// never spawns a duplicate.
func TestJiraWebhookHandler_LiveAgent_NudgedNotRespawned(t *testing.T) {
	daemon := newMockJiraWebhookDaemon()
	daemon.beads["bead-1"] = &beadsapi.BeadDetail{
		ID:       "bead-1",
		Status:   "in_progress",
		Assignee: "jira-pe-1234-abcd",
		Labels:   []string{"project:gardenio"},
		Fields:   map[string]string{"jira_key": "PE-1234"},
	}

	resolverDaemon := &mockJiraResolverDaemon{
		agentBeads: map[string]*beadsapi.BeadDetail{
			"jira-pe-1234-abcd": {
				ID:    "agent-bead-live",
				Title: "jira-pe-1234-abcd",
				Notes: "coop_url: http://10.0.0.7:8080",
				Fields: map[string]string{
					"agent":       "jira-pe-1234-abcd",
					"agent_state": "working",
				},
			},
		},
		spawnID:      "should-not-spawn",
		triggerBeads: jiraTestSpawnTriggerBeads("gardenio"),
	}

	var nudgedAgent, nudgedMsg string
	nudge := func(_ context.Context, agent, msg string) error {
		nudgedAgent, nudgedMsg = agent, msg
		return nil
	}
	resolver := NewJiraAgentResolver(JiraAgentResolverConfig{
		Daemon: resolverDaemon,
		Nudge:  nudge,
		Logger: jiraResolverTestLogger(),
	})

	handler := JiraWebhookHandler(JiraWebhookConfig{
		Daemon:   daemon,
		Nudge:    nudge,
		Resolver: resolver,
		DevMode:  true,
		Logger:   slog.Default(),
	})

	body := makeJiraWebhookPayload("comment_created", "PE-1234", "quick question", "John Doe")
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if nudgedAgent != "jira-pe-1234-abcd" {
		t.Errorf("nudged agent = %q; want jira-pe-1234-abcd", nudgedAgent)
	}
	if !strings.Contains(nudgedMsg, "quick question") {
		t.Errorf("nudge message should carry the comment, got %q", nudgedMsg)
	}
	if len(resolverDaemon.getSpawned()) != 0 {
		t.Error("live agent must be nudged, not respawned")
	}
}

// TestJiraWebhookHandler_FSMProject_NoRespawn pins the F5 spine on the F3
// path: a follow-up on a blocked project (FSM) must NOT re-spawn an agent —
// the resolver is bypassed and delivery downgrades to the plain nudge.
func TestJiraWebhookHandler_FSMProject_NoRespawn(t *testing.T) {
	daemon := newMockJiraWebhookDaemon()
	daemon.beads["bead-1"] = &beadsapi.BeadDetail{
		ID:       "bead-1",
		Status:   "closed",
		Assignee: "jira-fsm-1-abcd",
		Labels:   []string{"project:gardenio"},
		Fields:   map[string]string{"jira_key": "FSM-1"},
	}

	resolver, resolverDaemon := newJiraFollowUpResolver("jira-fsm-1-abcd")

	var nudged bool
	handler := JiraWebhookHandler(JiraWebhookConfig{
		Daemon:   daemon,
		Nudge:    func(_ context.Context, _, _ string) error { nudged = true; return nil },
		Resolver: resolver,
		DevMode:  true,
		Logger:   slog.Default(),
	})

	body := makeJiraWebhookPayload("comment_created", "FSM-1", "customer follow-up", "Support Rep")
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if got := len(resolverDaemon.getSpawned()); got != 0 {
		t.Fatalf("FSM follow-up must not respawn an agent, got %d spawns", got)
	}
	// Delivery downgrades to the legacy nudge (which reaches nothing for a
	// stopped agent, but never resurrects one on a blocked project).
	if !nudged {
		t.Error("expected legacy nudge fallback for blocked project")
	}
}

// TestJiraWebhookHandler_BotOwnComment_Ignored verifies the bridge's own
// comments (agent replies / squawk forwards echoed back by the webhook) never
// bridge, nudge, or re-spawn — the F3 anti-loop guard.
func TestJiraWebhookHandler_BotOwnComment_Ignored(t *testing.T) {
	daemon := newMockJiraWebhookDaemon()
	daemon.beads["bead-1"] = &beadsapi.BeadDetail{
		ID:       "bead-1",
		Status:   "closed",
		Assignee: "jira-pe-1234-abcd",
		Fields:   map[string]string{"jira_key": "PE-1234"},
	}

	resolver, resolverDaemon := newJiraFollowUpResolver("jira-pe-1234-abcd")

	handler := JiraWebhookHandler(JiraWebhookConfig{
		Daemon:       daemon,
		Resolver:     resolver,
		BotAccountID: "user-123", // makeJiraWebhookPayload's author accountId
		DevMode:      true,
		Logger:       slog.Default(),
	})

	body := makeJiraWebhookPayload("comment_created", "PE-1234", "[gardenio agent] done", "gardenio bot")
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if len(daemon.getComments()) != 0 {
		t.Error("bot's own comment must not be bridged back onto the bead")
	}
	if len(resolverDaemon.getSpawned()) != 0 {
		t.Error("bot's own comment must never respawn an agent (loop guard)")
	}
}

// --- jira:issue_created mention-in-description (H2, epic ka-0000007401) ---

// makeJiraIssueCreatedPayload builds a jira:issue_created webhook body with the
// given description (plain string; the handler also accepts ADF) and reporter.
func makeJiraIssueCreatedPayload(jiraKey, summary, description, reporterName, reporterAccountID string) []byte {
	payload := map[string]any{
		"webhookEvent": "jira:issue_created",
		"issue": map[string]any{
			"key": jiraKey,
			"fields": map[string]any{
				"summary":     summary,
				"description": description,
				"reporter": map[string]any{
					"displayName": reporterName,
					"accountId":   reporterAccountID,
				},
			},
		},
	}
	b, _ := json.Marshal(payload)
	return b
}

// TestJiraWebhookHandler_IssueCreated_MentionSpawnsAgent is the core H2 case: a
// @gardenio mention in a new PE ticket's DESCRIPTION spawns an agent, exactly as
// a mention in a comment would.
func TestJiraWebhookHandler_IssueCreated_MentionSpawnsAgent(t *testing.T) {
	mentionDaemon := &mockJiraMentionDaemon{
		createID:     "task-bead-1",
		spawnID:      "agent-bead-1",
		triggerBeads: jiraTestSpawnTriggerBeads("gardenio"),
	}
	mention := NewJiraMentionHandler(JiraMentionHandlerConfig{
		Users:   allowJiraMentionUsers{},
		Daemon:  mentionDaemon,
		Project: "gardenio",
		Logger:  jiraMentionTestLogger(),
	})

	handler := JiraWebhookHandler(JiraWebhookConfig{
		Daemon:         newMockJiraWebhookDaemon(),
		MentionHandler: mention,
		DevMode:        true,
		Logger:         slog.Default(),
	})

	body := makeJiraIssueCreatedPayload("PE-2001", "New auth bug",
		"We are seeing 500s on login. @gardenio please investigate the auth failure.",
		"Jane Doe", "acct-jane")
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if len(mentionDaemon.created) != 1 {
		t.Fatalf("expected 1 task bead created, got %d", len(mentionDaemon.created))
	}
	req0 := mentionDaemon.created[0]
	if req0.Fields != nil && !strings.Contains(string(req0.Fields), "PE-2001") {
		t.Errorf("task bead fields should reference PE-2001: %s", req0.Fields)
	}
	if !strings.Contains(req0.Description, "investigate the auth failure") {
		t.Errorf("task description should carry the instruction: %q", req0.Description)
	}
	if !strings.Contains(req0.Description, "Jane Doe") {
		t.Errorf("task description should attribute the reporter as author: %q", req0.Description)
	}
	if len(mentionDaemon.spawned) != 1 {
		t.Fatalf("expected 1 agent spawned, got %d", len(mentionDaemon.spawned))
	}
	spawn := mentionDaemon.spawned[0]
	if spawn.Extra["jira_key"] != "PE-2001" {
		t.Errorf("spawn extra jira_key = %q, want PE-2001", spawn.Extra["jira_key"])
	}
	// No originating comment id: the mention is the description, so replies post
	// flat (no jira_comment_id stamped).
	if _, ok := spawn.Extra["jira_comment_id"]; ok {
		t.Errorf("description mention must not stamp a jira_comment_id: %v", spawn.Extra)
	}
}

// TestJiraWebhookHandler_IssueCreated_NoMentionNoSpawn confirms a plain
// description (no @gardenio) is a no-op — issue mirroring stays the poller's job.
func TestJiraWebhookHandler_IssueCreated_NoMentionNoSpawn(t *testing.T) {
	mentionDaemon := &mockJiraMentionDaemon{
		createID:     "task-bead-1",
		spawnID:      "agent-bead-1",
		triggerBeads: jiraTestSpawnTriggerBeads("gardenio"),
	}
	mention := NewJiraMentionHandler(JiraMentionHandlerConfig{
		Users:   allowJiraMentionUsers{},
		Daemon:  mentionDaemon,
		Project: "gardenio",
		Logger:  jiraMentionTestLogger(),
	})

	handler := JiraWebhookHandler(JiraWebhookConfig{
		Daemon:         newMockJiraWebhookDaemon(),
		MentionHandler: mention,
		DevMode:        true,
		Logger:         slog.Default(),
	})

	body := makeJiraIssueCreatedPayload("PE-2002", "Routine task",
		"Just a normal ticket with no mention.", "Jane Doe", "acct-jane")
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if len(mentionDaemon.created) != 0 || len(mentionDaemon.spawned) != 0 {
		t.Errorf("plain description must not spawn: created=%d spawned=%d",
			len(mentionDaemon.created), len(mentionDaemon.spawned))
	}
}

// TestJiraWebhookHandler_IssueCreated_FSMBlocked confirms the deny-by-default
// project gate still governs the description path — an @gardenio mention in an
// FSM ticket description must never spawn (FSM is customer-facing).
func TestJiraWebhookHandler_IssueCreated_FSMBlocked(t *testing.T) {
	mentionDaemon := &mockJiraMentionDaemon{createID: "x", spawnID: "y"}
	mention := NewJiraMentionHandler(JiraMentionHandlerConfig{
		Users:   allowJiraMentionUsers{},
		Daemon:  mentionDaemon,
		Project: "gardenio",
		Logger:  jiraMentionTestLogger(),
	})

	handler := JiraWebhookHandler(JiraWebhookConfig{
		Daemon:         newMockJiraWebhookDaemon(),
		MentionHandler: mention,
		DevMode:        true,
		Logger:         slog.Default(),
	})

	body := makeJiraIssueCreatedPayload("FSM-9001", "Customer report",
		"@gardenio fix the export timeout", "Customer", "acct-cust")
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if len(mentionDaemon.created) != 0 || len(mentionDaemon.spawned) != 0 {
		t.Errorf("FSM description mention must be blocked: created=%d spawned=%d",
			len(mentionDaemon.created), len(mentionDaemon.spawned))
	}
}

// TestJiraWebhookHandler_IssueCreated_BotReporterIgnored confirms the bridge
// never spawns off an issue it filed itself (loop/self-trigger guard), even if
// that issue's description mentions @gardenio.
func TestJiraWebhookHandler_IssueCreated_BotReporterIgnored(t *testing.T) {
	mentionDaemon := &mockJiraMentionDaemon{
		createID:     "task-bead-1",
		spawnID:      "agent-bead-1",
		triggerBeads: jiraTestSpawnTriggerBeads("gardenio"),
	}
	mention := NewJiraMentionHandler(JiraMentionHandlerConfig{
		Users:   allowJiraMentionUsers{},
		Daemon:  mentionDaemon,
		Project: "gardenio",
		Logger:  jiraMentionTestLogger(),
	})

	handler := JiraWebhookHandler(JiraWebhookConfig{
		Daemon:         newMockJiraWebhookDaemon(),
		MentionHandler: mention,
		BotAccountID:   "bot-acct",
		DevMode:        true,
		Logger:         slog.Default(),
	})

	body := makeJiraIssueCreatedPayload("PE-2003", "Bridge-filed ticket",
		"@gardenio do the thing", "gardenio bot", "bot-acct")
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if len(mentionDaemon.spawned) != 0 {
		t.Errorf("bot-reported issue must never spawn (loop guard): spawned=%d",
			len(mentionDaemon.spawned))
	}
}

// TestJiraWebhookHandler_IssueCreated_ADFDescription confirms the handler
// detects a mention in an ADF (Jira Cloud) description, not only plain text.
func TestJiraWebhookHandler_IssueCreated_ADFDescription(t *testing.T) {
	mentionDaemon := &mockJiraMentionDaemon{
		createID:     "task-bead-1",
		spawnID:      "agent-bead-1",
		triggerBeads: jiraTestSpawnTriggerBeads("gardenio"),
	}
	mention := NewJiraMentionHandler(JiraMentionHandlerConfig{
		Users:   allowJiraMentionUsers{},
		Daemon:  mentionDaemon,
		Project: "gardenio",
		Logger:  jiraMentionTestLogger(),
	})

	handler := JiraWebhookHandler(JiraWebhookConfig{
		Daemon:         newMockJiraWebhookDaemon(),
		MentionHandler: mention,
		DevMode:        true,
		Logger:         slog.Default(),
	})

	// ADF description body carrying the mention as text content.
	adf := map[string]any{
		"webhookEvent": "jira:issue_created",
		"issue": map[string]any{
			"key": "PE-2004",
			"fields": map[string]any{
				"summary": "ADF ticket",
				"description": map[string]any{
					"type":    "doc",
					"version": 1,
					"content": []any{
						map[string]any{
							"type": "paragraph",
							"content": []any{
								map[string]any{"type": "text", "text": "@gardenio triage this please"},
							},
						},
					},
				},
				"reporter": map[string]any{
					"displayName": "Jane Doe",
					"accountId":   "acct-jane",
				},
			},
		},
	}
	body, _ := json.Marshal(adf)
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if len(mentionDaemon.spawned) != 1 {
		t.Fatalf("expected 1 agent spawned from ADF description mention, got %d",
			len(mentionDaemon.spawned))
	}
	if !strings.Contains(mentionDaemon.created[0].Description, "triage this please") {
		t.Errorf("task description should carry the ADF-extracted instruction: %q",
			mentionDaemon.created[0].Description)
	}
}

// TestJiraWebhookHandler_IssueCreated_NoDescription is a no-op guard: an issue
// with no description body must not spawn or panic.
func TestJiraWebhookHandler_IssueCreated_NoDescription(t *testing.T) {
	mentionDaemon := &mockJiraMentionDaemon{
		createID:     "task-bead-1",
		spawnID:      "agent-bead-1",
		triggerBeads: jiraTestSpawnTriggerBeads("gardenio"),
	}
	mention := NewJiraMentionHandler(JiraMentionHandlerConfig{
		Users:   allowJiraMentionUsers{},
		Daemon:  mentionDaemon,
		Project: "gardenio",
		Logger:  jiraMentionTestLogger(),
	})

	handler := JiraWebhookHandler(JiraWebhookConfig{
		Daemon:         newMockJiraWebhookDaemon(),
		MentionHandler: mention,
		DevMode:        true,
		Logger:         slog.Default(),
	})

	payload := map[string]any{
		"webhookEvent": "jira:issue_created",
		"issue": map[string]any{
			"key":    "PE-2005",
			"fields": map[string]any{"summary": "No description"},
		},
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if len(mentionDaemon.spawned) != 0 {
		t.Errorf("empty description must not spawn: spawned=%d", len(mentionDaemon.spawned))
	}
}

// TestJiraWebhookHandler_IssueCreated_NoMentionHandlerNoPanic confirms the
// jira:issue_created path is inert when no mention handler is wired.
func TestJiraWebhookHandler_IssueCreated_NoMentionHandlerNoPanic(t *testing.T) {
	handler := JiraWebhookHandler(JiraWebhookConfig{
		Daemon:  newMockJiraWebhookDaemon(),
		DevMode: true,
		Logger:  slog.Default(),
	})

	body := makeJiraIssueCreatedPayload("PE-2006", "No handler",
		"@gardenio do it", "Jane Doe", "acct-jane")
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
}

// --- jira:issue_updated mention-added-by-edit (H5, epic ka-0000007401) ---

// jiraIssueUpdatedOpts parameterizes an issue_updated payload. Description is
// the issue's CURRENT (post-edit) description body — either a plain string or an
// ADF map; nil omits the field so the changelog toString is the only source.
type jiraIssueUpdatedOpts struct {
	JiraKey         string
	Summary         string
	Description     any    // current description: string, ADF map, or nil
	ChangedField    string // changelog item field/fieldId (default "description")
	FromString      string // description text before the edit
	ToString        string // description text after the edit
	NoChangelog     bool   // omit the changelog entirely
	EditorName      string
	EditorAccountID string
	EditorEmail     string
}

// makeJiraIssueUpdatedPayload builds a jira:issue_updated webhook body: an issue
// with a (possibly ADF) current description, a changelog carrying the before/after
// text of the edited field, and the editing user.
func makeJiraIssueUpdatedPayload(o jiraIssueUpdatedOpts) []byte {
	field := o.ChangedField
	if field == "" {
		field = "description"
	}
	fields := map[string]any{"summary": o.Summary}
	if o.Description != nil {
		fields["description"] = o.Description
	}
	payload := map[string]any{
		"webhookEvent": "jira:issue_updated",
		"issue": map[string]any{
			"key":    o.JiraKey,
			"fields": fields,
		},
		"user": map[string]any{
			"displayName":  o.EditorName,
			"accountId":    o.EditorAccountID,
			"emailAddress": o.EditorEmail,
		},
	}
	if !o.NoChangelog {
		payload["changelog"] = map[string]any{
			"items": []any{
				map[string]any{
					"field":      field,
					"fieldId":    field,
					"fromString": o.FromString,
					"toString":   o.ToString,
				},
			},
		}
	}
	b, _ := json.Marshal(payload)
	return b
}

func newIssueUpdatedTestHandler(t *testing.T) (*mockJiraMentionDaemon, http.Handler) {
	t.Helper()
	mentionDaemon := &mockJiraMentionDaemon{
		createID:     "task-bead-1",
		spawnID:      "agent-bead-1",
		triggerBeads: jiraTestSpawnTriggerBeads("gardenio"),
	}
	mention := NewJiraMentionHandler(JiraMentionHandlerConfig{
		Users:   allowJiraMentionUsers{},
		Daemon:  mentionDaemon,
		Project: "gardenio",
		Logger:  jiraMentionTestLogger(),
	})
	handler := JiraWebhookHandler(JiraWebhookConfig{
		Daemon:         newMockJiraWebhookDaemon(),
		MentionHandler: mention,
		BotAccountID:   "bot-acct",
		DevMode:        true,
		Logger:         slog.Default(),
	})
	return mentionDaemon, handler
}

// TestJiraWebhookHandler_IssueUpdated_NewMentionSpawns is the core H5 case: an
// edit that ADDS @gardenio to a previously-unmentioned description spawns an
// agent, exactly as a mention in a comment or a new ticket would.
func TestJiraWebhookHandler_IssueUpdated_NewMentionSpawns(t *testing.T) {
	mentionDaemon, handler := newIssueUpdatedTestHandler(t)

	body := makeJiraIssueUpdatedPayload(jiraIssueUpdatedOpts{
		JiraKey:         "PE-3001",
		Summary:         "Auth bug",
		Description:     "We see 500s on login. @gardenio please investigate.",
		FromString:      "We see 500s on login.",
		ToString:        "We see 500s on login. @gardenio please investigate.",
		EditorName:      "Jane Doe",
		EditorAccountID: "acct-jane",
	})
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if len(mentionDaemon.spawned) != 1 {
		t.Fatalf("expected 1 agent spawned from a newly-added description mention, got %d",
			len(mentionDaemon.spawned))
	}
	if len(mentionDaemon.created) != 1 {
		t.Fatalf("expected 1 task bead, got %d", len(mentionDaemon.created))
	}
	req0 := mentionDaemon.created[0]
	if !strings.Contains(req0.Description, "please investigate") {
		t.Errorf("task description should carry the instruction: %q", req0.Description)
	}
	if !strings.Contains(req0.Description, "Jane Doe") {
		t.Errorf("task description should attribute the editor as author: %q", req0.Description)
	}
	spawn := mentionDaemon.spawned[0]
	if spawn.Extra["jira_key"] != "PE-3001" {
		t.Errorf("spawn extra jira_key = %q, want PE-3001", spawn.Extra["jira_key"])
	}
	// No originating comment id: the mention is the description, so replies post flat.
	if _, ok := spawn.Extra["jira_comment_id"]; ok {
		t.Errorf("description-edit mention must not stamp a jira_comment_id: %v", spawn.Extra)
	}
}

// TestJiraWebhookHandler_IssueUpdated_ExistingMentionNoRefire is the re-fire
// guard: editing a description that ALREADY contained @gardenio (e.g. fixing a
// typo elsewhere) must never re-spawn.
func TestJiraWebhookHandler_IssueUpdated_ExistingMentionNoRefire(t *testing.T) {
	mentionDaemon, handler := newIssueUpdatedTestHandler(t)

	body := makeJiraIssueUpdatedPayload(jiraIssueUpdatedOpts{
		JiraKey:         "PE-3002",
		Summary:         "Auth bug",
		Description:     "@gardenio investigate the login 500s and the timeout.",
		FromString:      "@gardenio investigate the login 500s.",
		ToString:        "@gardenio investigate the login 500s and the timeout.",
		EditorName:      "Jane Doe",
		EditorAccountID: "acct-jane",
	})
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if len(mentionDaemon.spawned) != 0 || len(mentionDaemon.created) != 0 {
		t.Errorf("re-editing an already-mentioned description must not re-spawn: created=%d spawned=%d",
			len(mentionDaemon.created), len(mentionDaemon.spawned))
	}
}

// TestJiraWebhookHandler_IssueUpdated_NonDescriptionFieldNoOp confirms an edit
// to any field OTHER than the description is a no-op, even when the current
// description contains @gardenio (it was already handled at create/comment time).
func TestJiraWebhookHandler_IssueUpdated_NonDescriptionFieldNoOp(t *testing.T) {
	mentionDaemon, handler := newIssueUpdatedTestHandler(t)

	body := makeJiraIssueUpdatedPayload(jiraIssueUpdatedOpts{
		JiraKey:         "PE-3003",
		Summary:         "Auth bug",
		Description:     "@gardenio investigate",
		ChangedField:    "status",
		FromString:      "To Do",
		ToString:        "In Progress",
		EditorName:      "Jane Doe",
		EditorAccountID: "acct-jane",
	})
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if len(mentionDaemon.spawned) != 0 {
		t.Errorf("a non-description field edit must not spawn: spawned=%d", len(mentionDaemon.spawned))
	}
}

// TestJiraWebhookHandler_IssueUpdated_FSMBlocked confirms the deny-by-default
// project gate still governs the edit path — adding @gardenio to an FSM ticket
// description must never spawn.
func TestJiraWebhookHandler_IssueUpdated_FSMBlocked(t *testing.T) {
	mentionDaemon, handler := newIssueUpdatedTestHandler(t)

	body := makeJiraIssueUpdatedPayload(jiraIssueUpdatedOpts{
		JiraKey:         "FSM-9002",
		Summary:         "Customer report",
		Description:     "@gardenio fix the export timeout",
		FromString:      "please fix the export timeout",
		ToString:        "@gardenio fix the export timeout",
		EditorName:      "Customer",
		EditorAccountID: "acct-cust",
	})
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if len(mentionDaemon.spawned) != 0 || len(mentionDaemon.created) != 0 {
		t.Errorf("FSM description-edit mention must be blocked: created=%d spawned=%d",
			len(mentionDaemon.created), len(mentionDaemon.spawned))
	}
}

// TestJiraWebhookHandler_IssueUpdated_BotEditorIgnored confirms the bridge never
// spawns off an edit it made itself, even when that edit adds @gardenio.
func TestJiraWebhookHandler_IssueUpdated_BotEditorIgnored(t *testing.T) {
	mentionDaemon, handler := newIssueUpdatedTestHandler(t)

	body := makeJiraIssueUpdatedPayload(jiraIssueUpdatedOpts{
		JiraKey:         "PE-3004",
		Summary:         "Bridge edit",
		Description:     "@gardenio do the thing",
		FromString:      "do the thing",
		ToString:        "@gardenio do the thing",
		EditorName:      "gardenio bot",
		EditorAccountID: "bot-acct",
	})
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if len(mentionDaemon.spawned) != 0 {
		t.Errorf("bot-authored edit must never spawn (loop guard): spawned=%d",
			len(mentionDaemon.spawned))
	}
}

// TestJiraWebhookHandler_IssueUpdated_ADFDescription confirms the handler
// detects a newly-added mention when the current description is an ADF (Jira
// Cloud) body, and that the spawned agent's task carries the ADF-extracted
// instruction (not just the changelog's flattened text).
func TestJiraWebhookHandler_IssueUpdated_ADFDescription(t *testing.T) {
	mentionDaemon, handler := newIssueUpdatedTestHandler(t)

	adfDesc := map[string]any{
		"type":    "doc",
		"version": 1,
		"content": []any{
			map[string]any{
				"type": "paragraph",
				"content": []any{
					map[string]any{"type": "text", "text": "@gardenio triage the ADF edit please"},
				},
			},
		},
	}
	body := makeJiraIssueUpdatedPayload(jiraIssueUpdatedOpts{
		JiraKey:         "PE-3005",
		Summary:         "ADF edit",
		Description:     adfDesc,
		FromString:      "triage this",
		ToString:        "@gardenio triage the ADF edit please",
		EditorName:      "Jane Doe",
		EditorAccountID: "acct-jane",
	})
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if len(mentionDaemon.spawned) != 1 {
		t.Fatalf("expected 1 agent spawned from ADF description edit, got %d",
			len(mentionDaemon.spawned))
	}
	if !strings.Contains(mentionDaemon.created[0].Description, "triage the ADF edit please") {
		t.Errorf("task description should carry the ADF-extracted instruction: %q",
			mentionDaemon.created[0].Description)
	}
}

// TestJiraWebhookHandler_IssueUpdated_PlainTextFallback confirms detection falls
// back to the changelog's flattened toString when the webhook omits the issue's
// current description field.
func TestJiraWebhookHandler_IssueUpdated_PlainTextFallback(t *testing.T) {
	mentionDaemon, handler := newIssueUpdatedTestHandler(t)

	body := makeJiraIssueUpdatedPayload(jiraIssueUpdatedOpts{
		JiraKey:         "PE-3006",
		Summary:         "No description field",
		Description:     nil, // omit current description → fall back to changelog toString
		FromString:      "just a note",
		ToString:        "just a note @gardenio handle this",
		EditorName:      "Jane Doe",
		EditorAccountID: "acct-jane",
	})
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if len(mentionDaemon.spawned) != 1 {
		t.Fatalf("expected 1 agent spawned from changelog-toString fallback, got %d",
			len(mentionDaemon.spawned))
	}
	if !strings.Contains(mentionDaemon.created[0].Description, "handle this") {
		t.Errorf("task description should carry the instruction from toString: %q",
			mentionDaemon.created[0].Description)
	}
}

// TestJiraWebhookHandler_IssueUpdated_NoChangelogNoOp confirms an issue_updated
// with no changelog (or no description item) is a no-op and never panics.
func TestJiraWebhookHandler_IssueUpdated_NoChangelogNoOp(t *testing.T) {
	mentionDaemon, handler := newIssueUpdatedTestHandler(t)

	body := makeJiraIssueUpdatedPayload(jiraIssueUpdatedOpts{
		JiraKey:         "PE-3007",
		Summary:         "No changelog",
		Description:     "@gardenio do it",
		NoChangelog:     true,
		EditorName:      "Jane Doe",
		EditorAccountID: "acct-jane",
	})
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if len(mentionDaemon.spawned) != 0 {
		t.Errorf("issue_updated with no description change must not spawn: spawned=%d",
			len(mentionDaemon.spawned))
	}
}

// TestJiraWebhookHandler_IssueUpdated_NoMentionHandlerNoPanic confirms the
// jira:issue_updated path is inert when no mention handler is wired.
func TestJiraWebhookHandler_IssueUpdated_NoMentionHandlerNoPanic(t *testing.T) {
	handler := JiraWebhookHandler(JiraWebhookConfig{
		Daemon:  newMockJiraWebhookDaemon(),
		DevMode: true,
		Logger:  slog.Default(),
	})

	body := makeJiraIssueUpdatedPayload(jiraIssueUpdatedOpts{
		JiraKey:         "PE-3008",
		Summary:         "No handler",
		Description:     "@gardenio do it",
		FromString:      "do it",
		ToString:        "@gardenio do it",
		EditorName:      "Jane Doe",
		EditorAccountID: "acct-jane",
	})
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
}
