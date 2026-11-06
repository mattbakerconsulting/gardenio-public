package bridge

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation/surfacetest"
)

// herderAsked seeds the herder's stale-wish decision about the say fixture's
// head (sayHeadID), answered with answer, and names it on the head.
func herderAsked(daemon *mockDaemon, id string, answer map[string]string) *beadsapi.BeadDetail {
	fields := map[string]string{"requesting_task": sayHeadID, "prompt": "Stale wish: why is CI red?", "responded_by": "oidc:ada@example.com"}
	for k, v := range answer {
		fields[k] = v
	}
	d := &beadsapi.BeadDetail{ID: id, Type: "decision", Status: "closed", CreatedBy: "gardenio-herder-tend-open-wishes-x1", Fields: fields}
	daemon.beads[id] = d
	daemon.beads[sayHeadID].Fields[herderDecisionField] = id
	return d
}

func headComments(daemon *mockDaemon, head string) []string {
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	var out []string
	for _, c := range daemon.comments {
		if c.BeadID == head {
			out = append(out, c.Text)
		}
	}
	return out
}

func closesOf(daemon *mockDaemon, id string) []closeCall {
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	var out []closeCall
	for _, c := range daemon.closed {
		if c.BeadID == id {
			out = append(out, c)
		}
	}
	return out
}

// The owner's close closes the wish with close_reason=herder-owner-close and
// says so in its Slack thread, once however often the close event comes.
func TestHerderDecision_CloseClosesTheWish(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	a, daemon := newWakeAPI(t, twin)
	dec := herderAsked(daemon, "gd-ask1", map[string]string{"chosen": herderAskClose, "rationale": "Chosen by @ada via Slack"})

	for range 2 {
		a.bot.handleHerderDecisionClosed(context.Background(), commonsClosedEvent(dec))
	}

	closes := closesOf(daemon, sayHeadID)
	if len(closes) != 1 || closes[0].Fields["close_reason"] != herderOwnerCloseReason {
		t.Fatalf("want one close with %s, got %+v", herderOwnerCloseReason, closes)
	}
	comments := headComments(daemon, sayHeadID)
	if len(comments) != 1 || comments[0] != herderAnsweredMarker+"gd-ask1: close." {
		t.Fatalf("want one answer record, got %q", comments)
	}
	posted := twin.Calls()
	if len(posted) != 1 || posted[0].Params["thread_ts"] != sayRootTS || !strings.Contains(posted[0].Params["text"], "gd-ask1") {
		t.Fatalf("want one note in the thread naming the decision, got %+v", posted)
	}
}

// The owner's revive gives their note to the thread's agent and stamps
// herder_woke_at; a free-text answer is such a note.
func TestHerderDecision_ReviveWakesTheThreadAgent(t *testing.T) {
	for name, answer := range map[string]map[string]string{
		"revive":    {"chosen": herderAskRevive, "rationale": "the fix is on main now — @ada via Slack"},
		"free text": {"response_text": "the fix is on main now"},
	} {
		t.Run(name, func(t *testing.T) {
			a, daemon := newWakeAPI(t, surfacetest.NewSlackTwin())
			coop, coopCalls := newDeliveringCoop(t)
			seedLiveThreadAgent(a.bot, daemon, "thread-ci", sayHeadID, "gardenio", coop)
			_ = a.bot.state.SetThreadAgent(sayChannel, sayRootTS, "thread-ci")
			dec := herderAsked(daemon, "gd-ask2", answer)

			a.bot.handleHerderDecisionClosed(context.Background(), commonsClosedEvent(dec))

			calls := strings.Join(coopCalls(), "\n")
			if !strings.Contains(calls, "the fix is on main now") || !strings.Contains(calls, "Revive from the owner") {
				t.Fatalf("the thread's agent did not get the owner's note: %s", calls)
			}
			if daemon.beads[sayHeadID].Fields["herder_woke_at"] == "" {
				t.Fatal("herder_woke_at was not stamped")
			}
			if n := len(closesOf(daemon, sayHeadID)); n != 0 {
				t.Fatalf("a revive closed the wish %d times", n)
			}
		})
	}
}

// Keep waiting, a dismissal and an expiry change nothing but the record.
func TestHerderDecision_WaitOnlyRecords(t *testing.T) {
	for _, chosen := range []string{herderAskWait, "dismissed", "_expired"} {
		a, daemon := newWakeAPI(t, surfacetest.NewSlackTwin())
		coop, coopCalls := newDeliveringCoop(t)
		seedLiveThreadAgent(a.bot, daemon, "thread-ci", sayHeadID, "gardenio", coop)
		dec := herderAsked(daemon, "gd-ask3", map[string]string{"chosen": chosen})

		a.bot.handleHerderDecisionClosed(context.Background(), commonsClosedEvent(dec))

		if n := len(closesOf(daemon, sayHeadID)); n != 0 {
			t.Errorf("%s: closed the wish", chosen)
		}
		if got := coopCalls(); len(got) != 0 {
			t.Errorf("%s: reached the thread's agent: %v", chosen, got)
		}
		if c := headComments(daemon, sayHeadID); len(c) != 1 || !strings.HasSuffix(c[0], ": wait.") {
			t.Errorf("%s: want a wait record, got %q", chosen, c)
		}
	}
}

// A decision that cannot be read is not acted on and leaves no record, so a
// later event or pass acts on the owner's real answer: the close event alone
// would read as a "wait" that no replay could undo.
func TestHerderDecision_UnreadDecisionDoesNothing(t *testing.T) {
	a, daemon := newWakeAPI(t, surfacetest.NewSlackTwin())
	dec := herderAsked(daemon, "gd-ask6", map[string]string{"chosen": herderAskClose})
	daemon.getBeadErr = func(id string) error {
		if id == dec.ID {
			return errors.New("daemon unavailable")
		}
		return nil
	}
	// The event carries no answer, as a close event may not.
	ev := commonsClosedEvent(&beadsapi.BeadDetail{ID: dec.ID, Type: "decision", Status: "closed", CreatedBy: dec.CreatedBy,
		Fields: map[string]string{"requesting_task": sayHeadID}})

	a.bot.handleHerderDecisionClosed(context.Background(), ev)

	if c := headComments(daemon, sayHeadID); len(c) != 0 {
		t.Fatalf("recorded an answer it could not read: %q", c)
	}

	daemon.getBeadErr = nil
	a.bot.handleHerderDecisionClosed(context.Background(), ev)

	if n := len(closesOf(daemon, sayHeadID)); n != 1 {
		t.Fatalf("the retry did not act on the owner's close: %d closes", n)
	}
}

// Only the owner's answer is acted on: one with no responder, from the herder
// that asked, from an agent of the land or from a system sweep is refused, as
// it is when the land's agents cannot be read to check.
func TestHerderDecision_RefusesAnAnswerNotTheOwners(t *testing.T) {
	for name, c := range map[string]struct {
		by      string
		listErr error
	}{
		"no responder":      {by: ""},
		"the herder":        {by: "gardenio-herder-tend-open-wishes-x1"},
		"an agent":          {by: "thread-ci"},
		"a system sweep":    {by: "system:decision-aging"},
		"agents unreadable": {by: "oidc:ada@example.com", listErr: errors.New("daemon unavailable")},
	} {
		t.Run(name, func(t *testing.T) {
			twin := surfacetest.NewSlackTwin()
			a, daemon := newWakeAPI(t, twin)
			coop, coopCalls := newDeliveringCoop(t)
			seedLiveThreadAgent(a.bot, daemon, "thread-ci", sayHeadID, "gardenio", coop)
			dec := herderAsked(daemon, "gd-ask7", map[string]string{"chosen": herderAskClose, "responded_by": c.by})
			daemon.listBeadsErr = c.listErr

			a.bot.handleHerderDecisionClosed(context.Background(), commonsClosedEvent(dec))

			if n := len(closesOf(daemon, sayHeadID)); n != 0 {
				t.Errorf("closed the wish on %q's answer", c.by)
			}
			if got := headComments(daemon, sayHeadID); len(got) != 0 {
				t.Errorf("recorded %q's answer as the owner's: %q", c.by, got)
			}
			if posted := twin.Calls(); len(posted) != 0 {
				t.Errorf("told the thread the owner answered: %+v", posted)
			}
			if got := coopCalls(); len(got) != 0 {
				t.Errorf("reached the thread's agent: %v", got)
			}
		})
	}
}

// A decision the head does not name is not the herder's to act on, whatever
// its requesting task.
func TestHerderDecision_IgnoresOtherDecisions(t *testing.T) {
	a, daemon := newWakeAPI(t, surfacetest.NewSlackTwin())
	dec := herderAsked(daemon, "gd-ask4", map[string]string{"chosen": herderAskClose})
	daemon.beads[sayHeadID].Fields[herderDecisionField] = "gd-another"

	a.bot.handleHerderDecisionClosed(context.Background(), commonsClosedEvent(dec))

	if n := len(closesOf(daemon, sayHeadID)); n != 0 || len(headComments(daemon, sayHeadID)) != 0 {
		t.Fatalf("acted on a decision the head does not name: %d closes", n)
	}
}

// The herder that asked is not nudged or respawned when the owner answers:
// the bridge acts on the answer.
func TestHerderDecision_DoesNotRespawnTheHerder(t *testing.T) {
	_, daemon := newWakeAPI(t, surfacetest.NewSlackTwin())
	dec := herderAsked(daemon, "gd-ask5", map[string]string{"chosen": herderAskWait})
	d := NewDecisions(DecisionsConfig{Daemon: daemon, Logger: slog.Default()})

	d.nudgeAgent(context.Background(), BeadEvent{ID: dec.ID, Type: "decision", CreatedBy: dec.CreatedBy, Fields: dec.Fields})

	daemon.mu.Lock()
	mails := len(daemon.mails)
	daemon.mu.Unlock()
	d.spawnMu.Lock()
	respawns := len(d.recentSpawn)
	d.spawnMu.Unlock()
	if mails != 0 || respawns != 0 {
		t.Fatalf("the herder was nudged (%d mails) or respawned (%d)", mails, respawns)
	}
}

// An owner's close reads closed on the card and reopens on a reply, as the
// herder's own close does.
func TestHerderOwnerClose_IsAHerderClose(t *testing.T) {
	if !isHerderClose(herderOwnerCloseReason) || !isHerderClose(herderCloseReason) || isHerderClose(wishCloseReason) {
		t.Fatal("isHerderClose must hold for the herder's two closes and no other")
	}
}
