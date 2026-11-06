package bridge

import (
	"context"
	"testing"

	"gardenio/controller/internal/beadsapi"

	"github.com/slack-go/slack"
)

// TestHandleInteraction_StampsEdgeAuthAndOperator verifies that handleInteraction
// attaches both the operator and edge-auth-operator values to the context that
// reaches the daemon. This is the bridge-side half of the ka-0000007637 fix:
// the daemon's ownership gate relies on the edge-auth-stamped Slack identity
// (slack:U…) matching bead.OperatedBy to authorize CloseBead on a decision
// bead. Regression test for "i still can't resolve decisions using the slack
// UI" — without these context stamps the close hits HTTP 403 ownership denied.
func TestHandleInteraction_StampsEdgeAuthAndOperator(t *testing.T) {
	daemon := newMockDaemon()
	daemon.mu.Lock()
	daemon.beads["dec-edge-1"] = &beadsapi.BeadDetail{
		ID:     "dec-edge-1",
		Type:   "decision",
		Status: "open",
	}
	daemon.mu.Unlock()

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()
	bot := newTestBot(daemon, slackSrv)

	callback := slack.InteractionCallback{
		Type: slack.InteractionTypeBlockActions,
		User: slack.User{ID: "U0OPERATOR1", Name: "matt"},
		ActionCallback: slack.ActionCallbacks{
			BlockActions: []*slack.BlockAction{
				{ActionID: "dismiss_decision", Value: "dec-edge-1"},
			},
		},
	}

	bot.handleInteraction(context.Background(), callback)

	daemon.mu.Lock()
	gotCtx := daemon.lastCloseBeadCtx
	closes := append([]closeCall(nil), daemon.closed...)
	daemon.mu.Unlock()

	if len(closes) != 1 {
		t.Fatalf("expected 1 CloseBead call, got %d", len(closes))
	}
	if gotCtx == nil {
		t.Fatal("CloseBead was not invoked with a context")
	}

	if got := beadsapi.EdgeAuthOperatorFromContext(gotCtx); got != "slack:U0OPERATOR1" {
		t.Errorf("edge-auth operator on CloseBead ctx = %q, want %q", got, "slack:U0OPERATOR1")
	}
	if got := beadsapi.OperatorFromContext(gotCtx); got == "" {
		t.Errorf("operator on CloseBead ctx is empty, want non-empty (operatorFromSlackUser fallback)")
	}
}

// TestHandleInteraction_NoUserID_NoStamping verifies that when callback.User.ID
// is empty (a corner case that should never happen in practice but the guard
// exists), no operator stamping occurs and the call still completes.
func TestHandleInteraction_NoUserID_NoStamping(t *testing.T) {
	daemon := newMockDaemon()
	daemon.mu.Lock()
	daemon.beads["dec-edge-2"] = &beadsapi.BeadDetail{
		ID:     "dec-edge-2",
		Type:   "decision",
		Status: "open",
	}
	daemon.mu.Unlock()

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()
	bot := newTestBot(daemon, slackSrv)

	callback := slack.InteractionCallback{
		Type: slack.InteractionTypeBlockActions,
		User: slack.User{ID: "", Name: ""},
		ActionCallback: slack.ActionCallbacks{
			BlockActions: []*slack.BlockAction{
				{ActionID: "dismiss_decision", Value: "dec-edge-2"},
			},
		},
	}

	bot.handleInteraction(context.Background(), callback)

	daemon.mu.Lock()
	gotCtx := daemon.lastCloseBeadCtx
	daemon.mu.Unlock()

	if gotCtx == nil {
		t.Fatal("CloseBead was not invoked")
	}
	if got := beadsapi.EdgeAuthOperatorFromContext(gotCtx); got != "" {
		t.Errorf("edge-auth operator should be empty when User.ID is empty, got %q", got)
	}
}
