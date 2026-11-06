package bridge

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"

	"github.com/slack-go/slack"
)

// ephemeralCapture is a fake Slack server that records chat.postEphemeral calls
// so tests can assert what (if anything) the bot sent to the clicking user.
type ephemeralCapture struct {
	srv  *httptest.Server
	mu   sync.Mutex
	msgs []ephemeralMsg
}

type ephemeralMsg struct {
	channel string
	user    string
	text    string
}

func newEphemeralCapture(t *testing.T) *ephemeralCapture {
	t.Helper()
	ec := &ephemeralCapture{}
	ec.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "chat.postEphemeral") {
			_ = r.ParseForm()
			ec.mu.Lock()
			ec.msgs = append(ec.msgs, ephemeralMsg{
				channel: r.FormValue("channel"),
				user:    r.FormValue("user"),
				text:    r.FormValue("text"),
			})
			ec.mu.Unlock()
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"message_ts":"1234.5678"}`))
	}))
	t.Cleanup(ec.srv.Close)
	return ec
}

func (ec *ephemeralCapture) all() []ephemeralMsg {
	ec.mu.Lock()
	defer ec.mu.Unlock()
	out := make([]ephemeralMsg, len(ec.msgs))
	copy(out, ec.msgs)
	return out
}

// seedDecisionWithOwner stores a decision bead assigned to agentName plus an
// agent bead carrying the owning operator's slack_user_id.
func seedDecisionWithOwner(daemon *mockDaemon, decisionID, agentName, ownerUserID string) {
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	daemon.beads[decisionID] = &beadsapi.BeadDetail{
		ID:       decisionID,
		Type:     "decision",
		Assignee: agentName,
		Fields:   map[string]string{},
	}
	daemon.beads[agentName] = &beadsapi.BeadDetail{
		ID:     agentName,
		Type:   "agent",
		Fields: map[string]string{"slack_user_id": ownerUserID},
	}
}

func TestCheckDecisionResolvePermission_OwnerAllowed(t *testing.T) {
	daemon := newMockDaemon()
	seedDecisionWithOwner(daemon, "dec-1", "agent-x", "UOWNER")
	bot := newTestBot(daemon, newFakeSlackServer(t))

	perm := bot.checkDecisionResolvePermission(context.Background(), "dec-1", "UOWNER")
	if !perm.Allowed {
		t.Errorf("expected owner to be allowed, got denied")
	}
	if perm.OwnerUserID != "UOWNER" {
		t.Errorf("expected OwnerUserID=UOWNER, got %q", perm.OwnerUserID)
	}
}

// TestCheckDecisionResolvePermission_NonOwnerAllowed pins the permissive policy
// (operator ruling 2026-08-27, thread ka-0000006045): a non-addressee may answer.
// The addressee identity is still resolved and returned — that is the seam the
// forthcoming `operated_by` rule (ka-0000000024) re-arms against, and the
// identity CrossOperatorNotify DMs when someone else answers.
func TestCheckDecisionResolvePermission_NonOwnerAllowed(t *testing.T) {
	daemon := newMockDaemon()
	seedDecisionWithOwner(daemon, "dec-1", "agent-x", "UOWNER")
	bot := newTestBot(daemon, newFakeSlackServer(t))

	perm := bot.checkDecisionResolvePermission(context.Background(), "dec-1", "USTRANGER")
	if !perm.Allowed {
		t.Errorf("expected non-owner to be ALLOWED under the permissive policy, got denied")
	}
	if perm.OwnerUserID != "UOWNER" {
		t.Errorf("expected OwnerUserID=UOWNER to still be resolved, got %q", perm.OwnerUserID)
	}
}

func TestCheckDecisionResolvePermission_NoOwnerFailsOpen(t *testing.T) {
	daemon := newMockDaemon()
	// Decision assigned to an agent with no slack_user_id → ownerless.
	daemon.mu.Lock()
	daemon.beads["dec-1"] = &beadsapi.BeadDetail{
		ID:       "dec-1",
		Type:     "decision",
		Assignee: "agent-x",
		Fields:   map[string]string{},
	}
	daemon.beads["agent-x"] = &beadsapi.BeadDetail{
		ID:     "agent-x",
		Type:   "agent",
		Fields: map[string]string{}, // no slack_user_id
	}
	daemon.mu.Unlock()
	bot := newTestBot(daemon, newFakeSlackServer(t))

	perm := bot.checkDecisionResolvePermission(context.Background(), "dec-1", "USTRANGER")
	if !perm.Allowed {
		t.Errorf("expected ownerless decision to fail open (allow), got denied")
	}
}

// TestHandleBlockActions_OptionButton_NonOwnerAllowed asserts the end-to-end
// button path under the permissive policy: a non-addressee clicking an option
// button is NOT shown a permission error — the resolve modal opens instead
// (which posts no ephemeral on this fake server).
func TestHandleBlockActions_OptionButton_NonOwnerAllowed(t *testing.T) {
	daemon := newMockDaemon()
	seedDecisionWithOwner(daemon, "dec-1", "agent-x", "UOWNER")
	ec := newEphemeralCapture(t)
	bot := newTestBot(daemon, ec.srv)

	callback := slack.InteractionCallback{
		Type: slack.InteractionTypeBlockActions,
	}
	callback.User.ID = "USTRANGER"
	callback.Channel.ID = "C123"
	callback.ActionCallback.BlockActions = []*slack.BlockAction{
		{ActionID: "resolve_dec-1_1", Value: "dec-1:1"},
	}

	bot.handleBlockActions(context.Background(), callback)

	for _, m := range ec.all() {
		if strings.Contains(m.text, "permission") {
			t.Errorf("non-owner should NOT get a permission error under the permissive policy, got %q", m.text)
		}
	}
}

// TestHandleBlockActions_OptionButton_OwnerNoError asserts the owner clicking
// does NOT receive a permission-denied ephemeral (the resolve modal opens
// instead, which posts no ephemeral on this fake server).
func TestHandleBlockActions_OptionButton_OwnerNoError(t *testing.T) {
	daemon := newMockDaemon()
	seedDecisionWithOwner(daemon, "dec-1", "agent-x", "UOWNER")
	ec := newEphemeralCapture(t)
	bot := newTestBot(daemon, ec.srv)

	callback := slack.InteractionCallback{Type: slack.InteractionTypeBlockActions}
	callback.User.ID = "UOWNER"
	callback.Channel.ID = "C123"
	callback.ActionCallback.BlockActions = []*slack.BlockAction{
		{ActionID: "resolve_dec-1_1", Value: "dec-1:1"},
	}

	bot.handleBlockActions(context.Background(), callback)

	for _, m := range ec.all() {
		if strings.Contains(m.text, "permission") {
			t.Errorf("owner should not get a permission error, got %q", m.text)
		}
	}
}
