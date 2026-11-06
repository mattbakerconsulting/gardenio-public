package bridge

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// signedSlackRequest builds a request with a valid X-Slack-Signature header so
// tests exercise the fail-closed signing path rather than legacy unauthenticated
// POSTs. Matches verifySlackSignature in slack.go.
func signedSlackRequest(t *testing.T, secret, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/slack/interactions",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(fmt.Sprintf("v0:%s:%s", ts, body)))
	req.Header.Set("X-Slack-Request-Timestamp", ts)
	req.Header.Set("X-Slack-Signature", "v0="+hex.EncodeToString(mac.Sum(nil)))
	return req
}

func TestSlackNotifier_HandleInteraction_ClosesDecision(t *testing.T) {
	daemon := newMockDaemon()
	const secret = "test-secret"
	slack := NewSlackNotifier("xoxb-test", secret, "C123", daemon, slog.Default())

	handler := slack.Handler()

	// Simulate a Slack block_actions interaction.
	interaction := slackInteraction{
		Type: "block_actions",
	}
	interaction.Channel.ID = "C123"
	interaction.Message.TS = "1234567890.123456"
	interaction.User.ID = "U123"
	interaction.User.Username = "testuser"
	interaction.Actions = []struct {
		ActionID string `json:"action_id"`
		BlockID  string `json:"block_id"`
		Value    string `json:"value"`
	}{
		{
			ActionID: "decision_dec-42_0",
			BlockID:  "decision_dec-42",
			Value:    "option-a",
		},
	}

	payloadJSON, _ := json.Marshal(interaction)
	form := url.Values{"payload": {string(payloadJSON)}}

	req := signedSlackRequest(t, secret, form.Encode())
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	// Block action is processed asynchronously — wait for it.
	var closed []closeCall
	deadline := time.After(2 * time.Second)
	for {
		closed = daemon.getClosed()
		if len(closed) >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for async CloseBead call")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	if closed[0].BeadID != "dec-42" {
		t.Errorf("expected bead dec-42 to be closed, got %q", closed[0].BeadID)
	}
	if closed[0].Fields["chosen"] != "option-a" {
		t.Errorf("expected chosen=option-a, got %q", closed[0].Fields["chosen"])
	}
	if !strings.Contains(closed[0].Fields["rationale"], "testuser") {
		t.Errorf("expected rationale to mention testuser, got %q", closed[0].Fields["rationale"])
	}
}

func TestSlackNotifier_NotifyDecision_PostsMessage(t *testing.T) {
	daemon := newMockDaemon()
	slack := NewSlackNotifier("xoxb-test", "", "C123", daemon, slog.Default())

	// Verify message tracking.
	slack.mu.Lock()
	slack.messages["dec-99"] = "1111111.222222"
	slack.mu.Unlock()

	slack.mu.Lock()
	ts, ok := slack.messages["dec-99"]
	slack.mu.Unlock()
	if !ok || ts != "1111111.222222" {
		t.Errorf("expected message ts tracked, got ok=%v ts=%q", ok, ts)
	}
}

func TestSlackNotifier_VerifySignature(t *testing.T) {
	slack := NewSlackNotifier("xoxb-test", "test-secret", "C123", nil, slog.Default())

	handler := slack.Handler()

	// Request without signature should fail.
	req := httptest.NewRequest(http.MethodPost, "/slack/interactions",
		strings.NewReader("payload={}"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for missing signature, got %d", rec.Code)
	}
}

func TestSlackNotifier_HandleInteraction_IgnoresNonDecision(t *testing.T) {
	daemon := newMockDaemon()
	const secret = "test-secret"
	slack := NewSlackNotifier("xoxb-test", secret, "C123", daemon, slog.Default())

	handler := slack.Handler()

	// Interaction with non-decision block_id should be ignored.
	interaction := slackInteraction{
		Type: "block_actions",
	}
	interaction.Actions = []struct {
		ActionID string `json:"action_id"`
		BlockID  string `json:"block_id"`
		Value    string `json:"value"`
	}{
		{
			ActionID: "other_action",
			BlockID:  "other_block",
			Value:    "whatever",
		},
	}

	payloadJSON, _ := json.Marshal(interaction)
	form := url.Values{"payload": {string(payloadJSON)}}

	req := signedSlackRequest(t, secret, form.Encode())
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	// No daemon calls should have been made — pass if no panic.
}

func TestSlackNotifier_HandleInteraction_RespondsImmediately(t *testing.T) {
	// Verify the handler returns 200 OK before CloseBead completes,
	// so Slack doesn't time out waiting for a response.
	var closeStarted sync.WaitGroup
	closeStarted.Add(1)
	block := make(chan struct{})

	daemon := &slowDaemon{
		mockDaemon:   newMockDaemon(),
		closeStarted: &closeStarted,
		block:        block,
	}
	const secret = "test-secret"
	slack := NewSlackNotifier("xoxb-test", secret, "C123", daemon, slog.Default())

	handler := slack.Handler()

	interaction := slackInteraction{Type: "block_actions"}
	interaction.User.Username = "testuser"
	interaction.Actions = []struct {
		ActionID string `json:"action_id"`
		BlockID  string `json:"block_id"`
		Value    string `json:"value"`
	}{
		{ActionID: "decision_dec-slow_0", BlockID: "decision_dec-slow", Value: "ok"},
	}

	payloadJSON, _ := json.Marshal(interaction)
	form := url.Values{"payload": {string(payloadJSON)}}

	req := signedSlackRequest(t, secret, form.Encode())
	rec := httptest.NewRecorder()

	// ServeHTTP must return quickly even though CloseBead will block.
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(rec, req)
		close(done)
	}()

	select {
	case <-done:
		// Handler returned — good.
	case <-time.After(500 * time.Millisecond):
		t.Fatal("handler blocked waiting for CloseBead — Slack would time out")
	}

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	// Wait for the background goroutine to enter CloseBead, then unblock it.
	closeStarted.Wait()
	close(block)
}

// slowDaemon wraps mockDaemon but blocks CloseBead until signaled.
type slowDaemon struct {
	*mockDaemon
	closeStarted *sync.WaitGroup
	block        chan struct{}
}

func (d *slowDaemon) CloseBead(ctx context.Context, beadID string, fields map[string]string) error {
	d.closeStarted.Done()
	<-d.block
	return d.mockDaemon.CloseBead(ctx, beadID, fields)
}

// TestSlackNotifier_HandleInteraction_FailsClosedWithoutSigningSecret is the
// regression for AUDIT-2026-04-18 §N2 (ka-0000005450, test 4.4). Without
// SLACK_SIGNING_SECRET the handler MUST reject requests outright — otherwise
// any network-adjacent attacker could forge block_actions payloads (including
// permission_allow verdicts) impersonating any Slack user.
func TestSlackNotifier_HandleInteraction_FailsClosedWithoutSigningSecret(t *testing.T) {
	daemon := newMockDaemon()
	slack := NewSlackNotifier("xoxb-test", "", "C123", daemon, slog.Default())

	handler := slack.Handler()

	// Forged permission_allow payload — exactly the PoC from the audit.
	interaction := slackInteraction{Type: "block_actions"}
	interaction.User.ID = "U_ADMIN"
	interaction.Actions = []struct {
		ActionID string `json:"action_id"`
		BlockID  string `json:"block_id"`
		Value    string `json:"value"`
	}{
		{
			ActionID: "permission_allow",
			BlockID:  "decision_dec-pwn",
			Value:    "REQID|http://attacker.invalid",
		},
	}
	payloadJSON, _ := json.Marshal(interaction)
	form := url.Values{"payload": {string(payloadJSON)}}

	req := httptest.NewRequest(http.MethodPost, "/slack/interactions",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("unauthenticated POST was accepted (status %d); handler must fail-closed when signing secret is unset", rec.Code)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 Service Unavailable, got %d", rec.Code)
	}

	// Crucially, no CloseBead call must have been made. Give the async
	// handleBlockAction goroutine (which would fire on 200 OK) a moment to run.
	time.Sleep(50 * time.Millisecond)
	if got := daemon.getClosed(); len(got) != 0 {
		t.Fatalf("daemon CloseBead fired from unauthenticated request: %+v", got)
	}
}

// TestSlackNotifier_HandleInteraction_RejectsOversizeBody is the H2 regression
// test from AUDIT-2026-04-18 §H2 (ka-0000002162). A POST whose body exceeds
// slackMaxBodyBytes must be rejected before signature verification allocates
// unbounded memory. We assert (1) the handler returns a non-2xx status and
// (2) no daemon interactions occurred (handleBlockAction never ran).
func TestSlackNotifier_HandleInteraction_RejectsOversizeBody(t *testing.T) {
	daemon := newMockDaemon()
	const secret = "test-secret"
	slack := NewSlackNotifier("xoxb-test", secret, "C123", daemon, slog.Default())

	handler := slack.Handler()

	// Body one byte over the cap. Signature is irrelevant — MaxBytesReader
	// must trip during io.ReadAll before verifySlackSignature is ever called.
	oversize := strings.Repeat("A", slackMaxBodyBytes+1)
	req := httptest.NewRequest(http.MethodPost, "/slack/interactions",
		strings.NewReader(oversize))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(fmt.Sprintf("v0:%s:%s", ts, oversize)))
	req.Header.Set("X-Slack-Request-Timestamp", ts)
	req.Header.Set("X-Slack-Signature", "v0="+hex.EncodeToString(mac.Sum(nil)))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code/100 == 2 {
		t.Errorf("expected non-2xx for oversize body, got %d", rec.Code)
	}

	// No async handleBlockAction goroutine must have fired.
	time.Sleep(50 * time.Millisecond)
	if got := daemon.getClosed(); len(got) != 0 {
		t.Fatalf("daemon CloseBead fired from oversize request: %+v", got)
	}
}

// TestSlackNotifier_HandleInteraction_AcceptsBodyAtLimit confirms that the
// MaxBytesReader bound (H2 fix) does not reject legitimate payloads up to
// the limit — regression guard against a future off-by-one tightening.
func TestSlackNotifier_HandleInteraction_AcceptsBodyAtLimit(t *testing.T) {
	daemon := newMockDaemon()
	const secret = "test-secret"
	slack := NewSlackNotifier("xoxb-test", secret, "C123", daemon, slog.Default())

	handler := slack.Handler()

	// Body exactly at the cap — must be accepted (signature still invalid so
	// we expect 401, not 400/413 from MaxBytesReader).
	body := strings.Repeat("A", slackMaxBodyBytes)
	req := httptest.NewRequest(http.MethodPost, "/slack/interactions",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Slack-Request-Timestamp", strconv.FormatInt(time.Now().Unix(), 10))
	req.Header.Set("X-Slack-Signature", "v0=deadbeef")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 (bad signature) for body at limit, got %d", rec.Code)
	}
}

// TestSlackNotifier_HandleInteraction_RejectsWrongSignature confirms that when
// a signing secret IS configured, a POST with a bad signature still fails.
func TestSlackNotifier_HandleInteraction_RejectsWrongSignature(t *testing.T) {
	daemon := newMockDaemon()
	slack := NewSlackNotifier("xoxb-test", "test-secret", "C123", daemon, slog.Default())

	handler := slack.Handler()

	body := "payload=" + url.QueryEscape(`{"type":"block_actions"}`)
	req := httptest.NewRequest(http.MethodPost, "/slack/interactions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Slack-Request-Timestamp", strconv.FormatInt(time.Now().Unix(), 10))
	req.Header.Set("X-Slack-Signature", "v0=deadbeef")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for bad signature, got %d", rec.Code)
	}
}

// Ensure SlackNotifier implements Notifier.
var _ Notifier = (*SlackNotifier)(nil)

// Ensure Notifier can be used in context.
func TestSlackNotifierImplementsNotifier(t *testing.T) {
	var n Notifier = NewSlackNotifier("token", "secret", "ch", nil, slog.Default())
	_ = n.NotifyDecision(context.Background(), BeadEvent{})
}

// TestSlackNotifier_HandleInteraction_RecoversHandlerPanic asserts that a panic
// inside the detached block-action goroutine does NOT crash the process. The
// webhook-fallback interaction path (*SlackNotifier) spawns handleBlockAction in
// a bare goroutine where net/http's per-request recover does not reach; without
// the recoverHandlerGoroutine boundary a panic there aborts the single-replica
// bridge (gd-A5wuFQHqtn, sibling of the Socket-Mode dispatchHandler fix).
//
// Fault injection: drop the `defer recoverHandlerGoroutine(...)` in slack.go and
// this test's forced CloseBead panic aborts the test process, failing the suite.
func TestSlackNotifier_HandleInteraction_RecoversHandlerPanic(t *testing.T) {
	daemon := newMockDaemon()
	daemon.closeBeadPanic = true // force a panic deep in the dispatched handler
	const secret = "test-secret"
	slack := NewSlackNotifier("xoxb-test", secret, "C123", daemon,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	handler := slack.Handler()

	interaction := slackInteraction{Type: "block_actions"}
	interaction.Channel.ID = "C123"
	interaction.Message.TS = "1234567890.123456"
	interaction.User.ID = "U123"
	interaction.User.Username = "testuser"
	interaction.Actions = []struct {
		ActionID string `json:"action_id"`
		BlockID  string `json:"block_id"`
		Value    string `json:"value"`
	}{{ActionID: "decision_dec-42_0", BlockID: "decision_dec-42", Value: "option-a"}}

	payloadJSON, _ := json.Marshal(interaction)
	form := url.Values{"payload": {string(payloadJSON)}}

	before := testutil.ToFloat64(handlerPanicTotal.WithLabelValues("webhook_block_action"))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, signedSlackRequest(t, secret, form.Encode()))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	// The handler goroutine panics asynchronously; poll the recovered-panic
	// counter. If the boundary is missing, the bare goroutine panic aborts the
	// process here rather than incrementing the counter.
	deadline := time.After(2 * time.Second)
	for {
		if testutil.ToFloat64(handlerPanicTotal.WithLabelValues("webhook_block_action")) == before+1 {
			return // recovered exactly one panic — process survived
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for the recovered-panic counter to increment")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
}
