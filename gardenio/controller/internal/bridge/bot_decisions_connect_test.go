package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/slack-go/slack"
)

// connectSlackCapture is a fake Slack API server for the Slack Connect /
// external-workspace decision-resolve tests (ka-0000005533). It can be
// configured to fail views.open the way Slack does for a trigger_id minted
// by a user in a workspace where the app is not installed, and it records
// chat.postEphemeral and chat.update calls so tests can assert the bridge
// is loud rather than silent.
type connectSlackCapture struct {
	srv *httptest.Server

	mu            sync.Mutex
	failOpenView  bool
	openViewCalls int
	ephemerals    []ephemeralMsg
	updates       []string // channel IDs of chat.update calls
}

func newConnectSlackCapture(t *testing.T, failOpenView bool) *connectSlackCapture {
	t.Helper()
	cc := &connectSlackCapture{failOpenView: failOpenView}
	cc.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { _, _ = io.Copy(io.Discard, r.Body) }()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "views.open"):
			cc.mu.Lock()
			cc.openViewCalls++
			fail := cc.failOpenView
			cc.mu.Unlock()
			if fail {
				// What Slack returns when the trigger_id cannot be
				// exchanged cross-workspace.
				_, _ = w.Write([]byte(`{"ok":false,"error":"invalid_arguments"}`))
				return
			}
			_, _ = w.Write([]byte(`{"ok":true,"view":{"id":"V123"}}`))
		case strings.HasSuffix(r.URL.Path, "chat.postEphemeral"):
			_ = r.ParseForm()
			cc.mu.Lock()
			cc.ephemerals = append(cc.ephemerals, ephemeralMsg{
				channel: r.FormValue("channel"),
				user:    r.FormValue("user"),
				text:    r.FormValue("text"),
			})
			cc.mu.Unlock()
			_, _ = w.Write([]byte(`{"ok":true,"message_ts":"1234.5678"}`))
		case strings.HasSuffix(r.URL.Path, "chat.update"):
			_ = r.ParseForm()
			cc.mu.Lock()
			cc.updates = append(cc.updates, r.FormValue("channel"))
			cc.mu.Unlock()
			_, _ = w.Write([]byte(`{"ok":true,"ts":"1234.5678"}`))
		default:
			_, _ = w.Write([]byte(`{"ok":true,"message_ts":"1234.5678"}`))
		}
	}))
	t.Cleanup(cc.srv.Close)
	return cc
}

func (cc *connectSlackCapture) ephemeralTexts() []string {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	out := make([]string, 0, len(cc.ephemerals))
	for _, m := range cc.ephemerals {
		out = append(out, m.text)
	}
	return out
}

func (cc *connectSlackCapture) updateCount() int {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	return len(cc.updates)
}

func (cc *connectSlackCapture) openViews() int {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	return cc.openViewCalls
}

// responseURLCapture records webhook posts to an interaction response_url.
type responseURLCapture struct {
	srv *httptest.Server

	mu    sync.Mutex
	bodys []slack.WebhookMessage
}

func newResponseURLCapture(t *testing.T) *responseURLCapture {
	t.Helper()
	rc := &responseURLCapture{}
	rc.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg slack.WebhookMessage
		_ = json.NewDecoder(r.Body).Decode(&msg)
		rc.mu.Lock()
		rc.bodys = append(rc.bodys, msg)
		rc.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(rc.srv.Close)
	return rc
}

func (rc *responseURLCapture) all() []slack.WebhookMessage {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	out := make([]slack.WebhookMessage, len(rc.bodys))
	copy(out, rc.bodys)
	return out
}

// seedConnectDecision seeds a decision owned by ownerUserID with two options,
// the first carrying an artifact_type.
func seedConnectDecision(daemon *mockDaemon, decisionID, agentName, ownerUserID string) {
	seedDecisionWithOwner(daemon, decisionID, agentName, ownerUserID)
	opts := []map[string]string{
		{"id": "ship", "short": "Ship it", "label": "Ship the fix", "artifact_type": "report"},
		{"id": "hold", "short": "Hold", "label": "Hold for review"},
	}
	optsJSON, _ := json.Marshal(opts)
	daemon.mu.Lock()
	daemon.beads[decisionID].Fields["options"] = string(optsJSON)
	daemon.beads[decisionID].Fields["prompt"] = "Ship the Slack Connect fix?"
	daemon.mu.Unlock()
}

// optionClickCallback builds the block_actions callback an option-button
// click produces. TeamID differs from the bot's workspace to model an
// external / Slack Connect user.
func optionClickCallback(decisionID, userID string) slack.InteractionCallback {
	callback := slack.InteractionCallback{Type: slack.InteractionTypeBlockActions}
	callback.User.ID = userID
	callback.User.Name = "aleks"
	callback.User.TeamID = "TEXTERNAL"
	callback.Channel.ID = "C0BGZHMGUFQ"
	callback.Message.Timestamp = "1111.2222"
	callback.TriggerID = "12345.67890.abcdef"
	callback.ActionCallback.BlockActions = []*slack.BlockAction{
		{ActionID: "resolve_" + decisionID + "_1", Value: decisionID + ":1"},
	}
	return callback
}

// TestOptionButton_ExternalUser_ModalFails_ResolvesDirectly is the
// ka-0000005533 regression test: when views.open fails (Slack Connect
// external owner — the app is not authorized in the clicker's workspace),
// the bridge must resolve the decision directly from the click instead of
// going silently dark.
func TestOptionButton_ExternalUser_ModalFails_ResolvesDirectly(t *testing.T) {
	daemon := newMockDaemon()
	seedConnectDecision(daemon, "dec-1", "agent-x", "UEXT")
	cc := newConnectSlackCapture(t, true /* fail views.open */)
	bot := newTestBot(daemon, cc.srv)

	bot.handleBlockActions(context.Background(), optionClickCallback("dec-1", "UEXT"))

	if cc.openViews() != 1 {
		t.Fatalf("expected 1 views.open attempt, got %d", cc.openViews())
	}

	closed := daemon.getClosed()
	if len(closed) != 1 {
		t.Fatalf("expected decision to be closed via fallback, got %d closes", len(closed))
	}
	if closed[0].BeadID != "dec-1" {
		t.Errorf("closed wrong bead: %s", closed[0].BeadID)
	}
	// chosen must be the option ID ("ship"), not its label ("Ship the fix").
	// Storing the label here was the ka-0000000003 bug that left the option
	// un-badged in the viewer.
	if closed[0].Fields["chosen"] != "ship" {
		t.Errorf("expected chosen=%q (option id), got %q", "ship", closed[0].Fields["chosen"])
	}
	if !strings.Contains(closed[0].Fields["rationale"], "via Slack") {
		t.Errorf("expected Slack attribution in rationale, got %q", closed[0].Fields["rationale"])
	}
	if closed[0].Fields["required_artifact"] != "report" {
		t.Errorf("expected required_artifact=report from option artifact_type, got %q",
			closed[0].Fields["required_artifact"])
	}
	if closed[0].Fields["artifact_status"] != "pending" {
		t.Errorf("expected artifact_status=pending, got %q", closed[0].Fields["artifact_status"])
	}

	// The clicker must be told the choice was recorded (no response_url on
	// this callback, so it lands via chat.postEphemeral).
	texts := cc.ephemeralTexts()
	if len(texts) != 1 {
		t.Fatalf("expected 1 ephemeral confirmation, got %d (%v)", len(texts), texts)
	}
	if !strings.Contains(texts[0], "Recorded your choice") || !strings.Contains(texts[0], "Ship the fix") {
		t.Errorf("expected recorded-choice confirmation, got %q", texts[0])
	}

	// The decision message itself is updated to the resolved state.
	if cc.updateCount() != 1 {
		t.Errorf("expected 1 chat.update of the decision message, got %d", cc.updateCount())
	}
}

// TestOptionButton_ExternalUser_ResponseURLPreferred asserts the fallback
// confirmation goes through the interaction's response_url when present —
// the delivery channel Slack guarantees cross-workspace — instead of
// chat.postEphemeral.
func TestOptionButton_ExternalUser_ResponseURLPreferred(t *testing.T) {
	daemon := newMockDaemon()
	seedConnectDecision(daemon, "dec-1", "agent-x", "UEXT")
	cc := newConnectSlackCapture(t, true)
	rc := newResponseURLCapture(t)
	bot := newTestBot(daemon, cc.srv)

	callback := optionClickCallback("dec-1", "UEXT")
	callback.ResponseURL = rc.srv.URL

	bot.handleBlockActions(context.Background(), callback)

	if len(daemon.getClosed()) != 1 {
		t.Fatalf("expected decision closed, got %d closes", len(daemon.getClosed()))
	}
	posts := rc.all()
	if len(posts) != 1 {
		t.Fatalf("expected 1 response_url post, got %d", len(posts))
	}
	if posts[0].ResponseType != slack.ResponseTypeEphemeral {
		t.Errorf("expected ephemeral response_type, got %q", posts[0].ResponseType)
	}
	if posts[0].ReplaceOriginal {
		t.Errorf("confirmation must not replace the decision message")
	}
	if !strings.Contains(posts[0].Text, "Recorded your choice") {
		t.Errorf("expected recorded-choice confirmation, got %q", posts[0].Text)
	}
	if got := cc.ephemeralTexts(); len(got) != 0 {
		t.Errorf("expected no chat.postEphemeral when response_url works, got %v", got)
	}
}

// TestOptionButton_ModalOpens_NoFallbackResolve asserts the happy path is
// unchanged: when views.open succeeds, the decision stays open (the user
// confirms in the modal) and no fallback fires.
func TestOptionButton_ModalOpens_NoFallbackResolve(t *testing.T) {
	daemon := newMockDaemon()
	seedConnectDecision(daemon, "dec-1", "agent-x", "UOWNER")
	cc := newConnectSlackCapture(t, false /* views.open succeeds */)
	bot := newTestBot(daemon, cc.srv)

	bot.handleBlockActions(context.Background(), optionClickCallback("dec-1", "UOWNER"))

	if cc.openViews() != 1 {
		t.Fatalf("expected 1 views.open, got %d", cc.openViews())
	}
	if len(daemon.getClosed()) != 0 {
		t.Errorf("decision must stay open until modal submit, got %d closes", len(daemon.getClosed()))
	}
	if got := cc.ephemeralTexts(); len(got) != 0 {
		t.Errorf("expected no ephemeral on happy path, got %v", got)
	}
}

// TestOptionButton_FallbackCloseFails_LoudEphemeral asserts that when even
// the direct-resolve write is rejected by the daemon, the clicker gets a
// loud error instead of silence.
func TestOptionButton_FallbackCloseFails_LoudEphemeral(t *testing.T) {
	daemon := newMockDaemon()
	seedConnectDecision(daemon, "dec-1", "agent-x", "UEXT")
	daemon.mu.Lock()
	daemon.closeBeadErr = errors.New("edge-auth verification failed")
	daemon.mu.Unlock()
	cc := newConnectSlackCapture(t, true)
	bot := newTestBot(daemon, cc.srv)

	bot.handleBlockActions(context.Background(), optionClickCallback("dec-1", "UEXT"))

	if len(daemon.getClosed()) != 0 {
		t.Fatalf("close should have been rejected, got %d closes", len(daemon.getClosed()))
	}
	texts := cc.ephemeralTexts()
	if len(texts) != 1 {
		t.Fatalf("expected 1 loud error ephemeral, got %d (%v)", len(texts), texts)
	}
	if !strings.Contains(texts[0], "Couldn't record") || !strings.Contains(texts[0], "edge-auth verification failed") {
		t.Errorf("expected loud close-failure error with cause, got %q", texts[0])
	}
}

// TestOtherButton_ExternalUser_ModalFails_LoudEphemeral asserts the
// freeform "Other..." path — which cannot fall back to a direct resolve —
// tells the clicker the modal is unavailable and points at the thread-reply
// alternative, leaving the decision open.
func TestOtherButton_ExternalUser_ModalFails_LoudEphemeral(t *testing.T) {
	daemon := newMockDaemon()
	seedConnectDecision(daemon, "dec-1", "agent-x", "UEXT")
	cc := newConnectSlackCapture(t, true)
	bot := newTestBot(daemon, cc.srv)

	callback := optionClickCallback("dec-1", "UEXT")
	callback.ActionCallback.BlockActions = []*slack.BlockAction{
		{ActionID: "resolve_other_dec-1", Value: "dec-1"},
	}

	bot.handleBlockActions(context.Background(), callback)

	if len(daemon.getClosed()) != 0 {
		t.Errorf("decision must stay open when the other-modal fails, got %d closes", len(daemon.getClosed()))
	}
	texts := cc.ephemeralTexts()
	if len(texts) != 1 {
		t.Fatalf("expected 1 loud ephemeral, got %d (%v)", len(texts), texts)
	}
	if !strings.Contains(texts[0], "thread") {
		t.Errorf("expected thread-reply guidance, got %q", texts[0])
	}
}

// TestHandleResolveSubmission_CloseFails_LoudEphemeral asserts a modal
// submission whose daemon write fails reports the failure to the submitter
// (the modal has already closed at that point, so silence would strand the
// operator).
func TestHandleResolveSubmission_CloseFails_LoudEphemeral(t *testing.T) {
	daemon := newMockDaemon()
	seedConnectDecision(daemon, "dec-1", "agent-x", "UOWNER")
	daemon.mu.Lock()
	daemon.closeBeadErr = errors.New("daemon unavailable")
	daemon.mu.Unlock()
	cc := newConnectSlackCapture(t, false)
	bot := newTestBot(daemon, cc.srv)

	callback := slack.InteractionCallback{Type: slack.InteractionTypeViewSubmission}
	callback.User.ID = "UOWNER"
	callback.User.Name = "matt"
	callback.View.PrivateMetadata = "dec-1|Ship the fix|C0BGZHMGUFQ|1111.2222"
	callback.View.CallbackID = "resolve_decision"
	callback.View.State = &slack.ViewState{}

	bot.handleResolveSubmission(context.Background(), callback)

	if len(daemon.getClosed()) != 0 {
		t.Fatalf("close should have failed, got %d closes", len(daemon.getClosed()))
	}
	texts := cc.ephemeralTexts()
	if len(texts) != 1 {
		t.Fatalf("expected 1 loud error ephemeral, got %d (%v)", len(texts), texts)
	}
	if !strings.Contains(texts[0], "NOT recorded") || !strings.Contains(texts[0], "daemon unavailable") {
		t.Errorf("expected loud submission-failure error, got %q", texts[0])
	}
}

// TestDenyDecisionResolve_ResponseURLPreferred asserts the permission-denied
// message rides the response_url when available, so external / Slack Connect
// users are not silently ignored either (ka-0000005533).
//
// Since the operator's 2026-08-27 ruling the resolve gate is permissive, so
// this exercises denyDecisionResolve DIRECTLY rather than through a non-owner
// click. The helper is retained, wired at every resolve site, as the deny
// surface the decision `operated_by` rule (ka-0000000024) re-arms — this test
// keeps its delivery path covered in the meantime.
func TestDenyDecisionResolve_ResponseURLPreferred(t *testing.T) {
	daemon := newMockDaemon()
	seedConnectDecision(daemon, "dec-1", "agent-x", "UOWNER")
	cc := newConnectSlackCapture(t, true)
	rc := newResponseURLCapture(t)
	bot := newTestBot(daemon, cc.srv)

	callback := optionClickCallback("dec-1", "USTRANGER")
	callback.ResponseURL = rc.srv.URL

	bot.denyDecisionResolve(context.Background(), "dec-1",
		decisionResolvePermission{Allowed: false, OwnerUserID: "UOWNER"}, callback)

	posts := rc.all()
	if len(posts) != 1 {
		t.Fatalf("expected 1 response_url post, got %d", len(posts))
	}
	if !strings.Contains(posts[0].Text, "<@UOWNER>") || !strings.Contains(posts[0].Text, "permission") {
		t.Errorf("expected permission-denied naming the owner, got %q", posts[0].Text)
	}
	if cc.openViews() != 0 {
		t.Errorf("deny path must not open a modal, got %d views.open", cc.openViews())
	}
}

// TestOptionClick_NonOwnerResolves pins the permissive policy end-to-end on the
// Slack Connect path: a non-addressee clicking an option button gets the resolve
// modal, not a lecture.
func TestOptionClick_NonOwnerResolves(t *testing.T) {
	daemon := newMockDaemon()
	seedConnectDecision(daemon, "dec-1", "agent-x", "UOWNER")
	cc := newConnectSlackCapture(t, true)
	rc := newResponseURLCapture(t)
	bot := newTestBot(daemon, cc.srv)

	callback := optionClickCallback("dec-1", "USTRANGER")
	callback.ResponseURL = rc.srv.URL

	bot.handleBlockActions(context.Background(), callback)

	for _, p := range rc.all() {
		if strings.Contains(p.Text, "permission") {
			t.Errorf("non-owner must not be denied under the permissive policy, got %q", p.Text)
		}
	}
	if cc.openViews() != 1 {
		t.Errorf("expected the resolve modal to open for the non-owner, got %d views.open", cc.openViews())
	}
}
