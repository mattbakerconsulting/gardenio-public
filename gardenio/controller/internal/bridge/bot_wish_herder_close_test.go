package bridge

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack/slackevents"

	"gardenio/controller/internal/beadsapi"
)

// closeWishHead closes a seeded head with the given close_reason, as the
// herder's kd update + kd close (or a person's ⏹️) leaves it.
func closeWishHead(daemon *mockDaemon, id, reason string) *beadsapi.BeadDetail {
	head := daemon.beads[id]
	head.Status = "closed"
	head.Fields["close_reason"] = reason
	return head
}

// A herder close puts the wish-close ⏹️ on the thread card, so the card reads
// closed as it does after a person's close.
func TestHerderCloseEvent_MarksThreadCard(t *testing.T) {
	daemon := newMockDaemon()
	var captured slackAPICalls
	srv := newCapturingSlackServer(t, &captured)
	defer srv.Close()
	b := newTestBot(daemon, srv)

	seedWishHead(daemon, "head-h", "C1", "100.0", "200.5")
	b.handleHerderCloseEvent(context.Background(), commonsClosedEvent(closeWishHead(daemon, "head-h", herderCloseReason)))

	body := captured.bodyForPath("/reactions.add")
	if !strings.Contains(body, "name="+herderCloseEmoji) || !strings.Contains(body, "timestamp=200.5") {
		t.Fatalf("want %s on the card 200.5, got reactions.add %q", herderCloseEmoji, body)
	}
}

// Only the herder's closes are marked: a person's ⏹️ is on the card already,
// and an idle autoclose is not a fulfilled verdict.
func TestHerderCloseEvent_IgnoresOtherCloses(t *testing.T) {
	for _, reason := range []string{wishCloseReason, "slack-thread-idle", ""} {
		daemon := newMockDaemon()
		var captured slackAPICalls
		srv := newCapturingSlackServer(t, &captured)
		b := newTestBot(daemon, srv)

		seedWishHead(daemon, "head-o", "C1", "100.0", "200.5")
		b.handleHerderCloseEvent(context.Background(), commonsClosedEvent(closeWishHead(daemon, "head-o", reason)))
		srv.Close()

		if captured.hasPath("/reactions.add") {
			t.Errorf("close_reason %q marked the card; only %q should", reason, herderCloseReason)
		}
	}
}

// A Slack reply in the thread of a herder-closed wish reopens it, clears the
// close reason and lifts the herder's ⏹️ from the card.
func TestSlackReply_ReopensHerderClosedWish(t *testing.T) {
	daemon := newMockDaemon()
	var captured slackAPICalls
	srv := newCapturingSlackServer(t, &captured)
	defer srv.Close()
	state, err := NewStateManager(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	b := newTestBot(daemon, srv)
	b.state = state
	b.botUserID = "U-BOT"
	b.lastThreadNudge = make(map[string]time.Time)

	seedWishHead(daemon, "head-r", "C-r", "3333.1111", "3333.1500")
	closeWishHead(daemon, "head-r", herderCloseReason)

	b.handleMessageEvent(context.Background(), &slackevents.MessageEvent{
		User:            "U-HUMAN",
		Channel:         "C-r",
		Text:            "not done: the button still 404s",
		TimeStamp:       "3333.2222",
		ThreadTimeStamp: "3333.1111",
	})

	head := daemon.beads["head-r"]
	if head.Status != "open" {
		t.Fatalf("reply did not reopen the herder-closed wish: status %q", head.Status)
	}
	if r, ok := head.Fields["close_reason"]; ok {
		t.Fatalf("reopen kept close_reason %q", r)
	}
	body := captured.bodyForPath("/reactions.remove")
	if !strings.Contains(body, "name="+herderCloseEmoji) || !strings.Contains(body, "timestamp=3333.1500") {
		t.Fatalf("want the herder's %s lifted from the card, got reactions.remove %q", herderCloseEmoji, body)
	}
}

// A person's close is theirs: a later reply does not undo it.
func TestSlackReply_LeavesPersonClosedWishClosed(t *testing.T) {
	daemon := newMockDaemon()
	srv := newFakeSlackServer(t)
	defer srv.Close()
	state, err := NewStateManager(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	b := newTestBot(daemon, srv)
	b.state = state
	b.botUserID = "U-BOT"
	b.lastThreadNudge = make(map[string]time.Time)

	seedWishHead(daemon, "head-p", "C-p", "4444.1111", "")
	closeWishHead(daemon, "head-p", wishCloseReason)

	b.handleMessageEvent(context.Background(), &slackevents.MessageEvent{
		User:            "U-HUMAN",
		Channel:         "C-p",
		Text:            "thanks!",
		TimeStamp:       "4444.2222",
		ThreadTimeStamp: "4444.1111",
	})

	if head := daemon.beads["head-p"]; head.Status != "closed" || head.Fields["close_reason"] != wishCloseReason {
		t.Fatalf("a reply reopened a person's close: status %q close_reason %q", head.Status, head.Fields["close_reason"])
	}
}

// A Commons reply to a herder-closed wish reopens it too.
func TestCommonsReply_ReopensHerderClosedWish(t *testing.T) {
	b, daemon, _, _ := newCommonsReplyBot(t)
	daemon.beads["gd-head-live"].Status = "closed"
	daemon.beads["gd-head-live"].Fields["close_reason"] = herderCloseReason

	if _, dispatched, err := b.dispatchCommonsMessage(context.Background(), commonsMessageFromDetail(daemon.beads["gd-cm1"])); err != nil || !dispatched {
		t.Fatalf("dispatch: dispatched=%v err=%v", dispatched, err)
	}
	if head := daemon.beads["gd-head-live"]; head.Status != "open" || head.Fields["close_reason"] != "" {
		t.Fatalf("commons reply did not reopen the herder-closed wish: status %q close_reason %q",
			head.Status, head.Fields["close_reason"])
	}
}
