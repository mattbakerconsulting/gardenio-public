package bridge

import (
	"context"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// seedWishHead adds a thread (wish) HEAD bead carrying the canonical thread
// label and (optionally) the stamped slack_card_ts. Returns the head id.
func seedWishHead(daemon *mockDaemon, id, channel, threadTS, cardTS string) string {
	fields := map[string]string{
		"slack_thread_channel": channel,
		"slack_thread_ts":      threadTS,
	}
	labels := []string{threadKindLabel, threadBeadLabel(channel, threadTS)}
	if cardTS != "" {
		fields["slack_card_channel"] = channel
		fields["slack_card_ts"] = cardTS
	}
	daemon.beads[id] = &beadsapi.BeadDetail{
		ID:     id,
		Type:   "task",
		Status: "open",
		Labels: labels,
		Fields: fields,
	}
	return id
}

// TestHandleWishCloseReaction_HumanCheckClosesWish is the core acceptance test:
// a :black_square_for_stop: (⏹️) reaction by a human on the thread card closes
// the wish HEAD with the audited close_reason.
func TestHandleWishCloseReaction_HumanCheckClosesWish(t *testing.T) {
	daemon := newMockDaemon()
	srv := newFakeSlackServer(t)
	defer srv.Close()
	bot := newTestBot(daemon, srv)
	bot.botUserID = "BOT"

	seedWishHead(daemon, "head-1", "C1", "100.0", "200.5")

	ev := makeReactionEvent("U_HUMAN", "black_square_for_stop", "C1", "200.5")
	bot.handleReactionAdded(context.Background(), ev)

	head := daemon.beads["head-1"]
	if head.Status != "closed" {
		t.Fatalf("expected wish head closed, got status %q", head.Status)
	}
	if got := head.Fields["close_reason"]; got != wishCloseReason {
		t.Fatalf("expected close_reason=%q, got %q", wishCloseReason, got)
	}

	closed := daemon.getClosed()
	if len(closed) != 1 || closed[0].BeadID != "head-1" {
		t.Fatalf("expected exactly one close of head-1, got %+v", closed)
	}
	// The close ctx must carry the reactor as the per-call operator so the
	// daemon attributes closed_by to the human (it ignores client-supplied
	// closed_by and derives it from this header). U_HUMAN has no resolvable
	// email in tests, so it resolves to the slack: sentinel.
	if op := beadsapi.OperatorFromContext(daemon.lastCloseBeadCtx); op != "slack:U_HUMAN" {
		t.Fatalf("expected close ctx operator slack:U_HUMAN, got %q", op)
	}
}

// TestHandleWishCloseReaction_EachEmojiInSetCloses verifies every emoji in the
// wish-close SET triggers a close, so a client/skin-tone variant never
// silently no-ops.
func TestHandleWishCloseReaction_EachEmojiInSetCloses(t *testing.T) {
	for emoji := range wishCloseEmojis {
		t.Run(emoji, func(t *testing.T) {
			daemon := newMockDaemon()
			srv := newFakeSlackServer(t)
			defer srv.Close()
			bot := newTestBot(daemon, srv)
			bot.botUserID = "BOT"

			seedWishHead(daemon, "head-1", "C1", "100.0", "200.5")

			ev := makeReactionEvent("U_HUMAN", emoji, "C1", "200.5")
			bot.handleReactionAdded(context.Background(), ev)

			if daemon.beads["head-1"].Status != "closed" {
				t.Fatalf("emoji %q: expected wish head closed, got %q",
					emoji, daemon.beads["head-1"].Status)
			}
			if got := daemon.beads["head-1"].Fields["close_reason"]; got != wishCloseReason {
				t.Fatalf("emoji %q: expected close_reason=%q, got %q", emoji, wishCloseReason, got)
			}
		})
	}
}

// TestHandleWishCloseReaction_BotSelfReactIgnored verifies the ev.User ==
// botUserID identity guard at the top of handleReactionAdded: even if the bot
// itself emits the wish-close glyph (⏹️) on a card, the self-react is dropped
// before the wish-close branch. This is belt-and-suspenders on top of the glyph
// being distinct from the bot's ✅ done-lifecycle reaction (ka-0000002200) —
// the identity guard must independently hold.
func TestHandleWishCloseReaction_BotSelfReactIgnored(t *testing.T) {
	daemon := newMockDaemon()
	srv := newFakeSlackServer(t)
	defer srv.Close()
	bot := newTestBot(daemon, srv)
	bot.botUserID = "BOT"

	seedWishHead(daemon, "head-1", "C1", "100.0", "200.5")

	ev := makeReactionEvent("BOT", "black_square_for_stop", "C1", "200.5")
	bot.handleReactionAdded(context.Background(), ev)

	if daemon.beads["head-1"].Status == "closed" {
		t.Fatal("bot's own reaction must NOT close the wish")
	}
	if len(daemon.getClosed()) != 0 {
		t.Fatalf("expected no close calls for bot self-react, got %+v", daemon.getClosed())
	}
}

// TestHandleWishCloseReaction_BotDoneGlyphDoesNotClose verifies the second,
// independent guard: the bot's outbound ✅ done-lifecycle reaction is NOT in the
// wish-close set, so even a HUMAN ✅ on the card (mirroring the bot's glyph) does
// not close the wish. ✅ means "agent finished"; ⏹️ means "user declares done".
func TestHandleWishCloseReaction_BotDoneGlyphDoesNotClose(t *testing.T) {
	daemon := newMockDaemon()
	srv := newFakeSlackServer(t)
	defer srv.Close()
	bot := newTestBot(daemon, srv)
	bot.botUserID = "BOT"

	seedWishHead(daemon, "head-1", "C1", "100.0", "200.5")

	// A human reacts with the bot's done glyph (✅), not the close glyph.
	ev := makeReactionEvent("U_HUMAN", "white_check_mark", "C1", "200.5")
	bot.handleReactionAdded(context.Background(), ev)

	if daemon.beads["head-1"].Status == "closed" {
		t.Fatal("the ✅ done glyph must NOT close the wish — only ⏹️ does")
	}
	if len(daemon.getClosed()) != 0 {
		t.Fatalf("expected no close calls for a ✅ reaction, got %+v", daemon.getClosed())
	}
}

// TestHandleWishCloseReaction_AlreadyClosedNoOp verifies re-reacting on an
// already-closed wish is a safe no-op: no error, no double-close.
func TestHandleWishCloseReaction_AlreadyClosedNoOp(t *testing.T) {
	daemon := newMockDaemon()
	srv := newFakeSlackServer(t)
	defer srv.Close()
	bot := newTestBot(daemon, srv)
	bot.botUserID = "BOT"

	id := seedWishHead(daemon, "head-1", "C1", "100.0", "200.5")
	daemon.beads[id].Status = "closed"

	ev := makeReactionEvent("U_HUMAN", "black_square_for_stop", "C1", "200.5")
	bot.handleReactionAdded(context.Background(), ev)

	if len(daemon.getClosed()) != 0 {
		t.Fatalf("expected no close call for an already-closed wish, got %+v", daemon.getClosed())
	}
}

// TestHandleWishCloseReaction_RootTSViaThreadLabel verifies a reaction on the
// thread ROOT ts (no card_ts stamped) still resolves the wish head via the
// canonical slack-thread:{channel}:{ts} label.
func TestHandleWishCloseReaction_RootTSViaThreadLabel(t *testing.T) {
	daemon := newMockDaemon()
	srv := newFakeSlackServer(t)
	defer srv.Close()
	bot := newTestBot(daemon, srv)
	bot.botUserID = "BOT"

	// No card_ts: only the thread label binds the head to (channel, ts).
	seedWishHead(daemon, "head-1", "C1", "100.0", "")

	ev := makeReactionEvent("U_HUMAN", "black_square_for_stop", "C1", "100.0")
	bot.handleReactionAdded(context.Background(), ev)

	if daemon.beads["head-1"].Status != "closed" {
		t.Fatalf("expected root-ts reaction to close wish via thread label, got %q",
			daemon.beads["head-1"].Status)
	}
}

// TestHandleWishCloseReaction_AgentFallback verifies the fragile last-resort
// path: when neither the card-ts index nor the thread label resolve, the wish
// head is recovered from the bound agent's slack_thread_bead_id.
func TestHandleWishCloseReaction_AgentFallback(t *testing.T) {
	daemon := newMockDaemon()
	srv := newFakeSlackServer(t)
	defer srv.Close()
	bot := newTestBot(daemon, srv)
	bot.botUserID = "BOT"

	// Head carries NO card_ts and NO thread label for the reacted ts — only
	// reachable via the agent's slack_thread_bead_id.
	daemon.beads["head-1"] = &beadsapi.BeadDetail{
		ID:     "head-1",
		Type:   "task",
		Status: "open",
		Labels: []string{threadKindLabel},
	}
	// A live agent bound to the thread, carrying the head pointer. The mock's
	// FindAgentBead keys off the map key (the real daemon resolves by agent
	// name), and getAgentByThread returns the agent NAME from ListAgentBeads —
	// so key the bead by its agent name to mirror the two-hop production path
	// (getAgentByThread -> FindAgentBead) used by handleWishCloseReaction.
	daemon.beads["card-agent"] = &beadsapi.BeadDetail{
		ID:     "card-agent",
		Type:   "agent",
		Status: "open",
		Fields: map[string]string{
			"agent":                "card-agent",
			"agent_state":          "working",
			"slack_thread_channel": "C1",
			"slack_thread_ts":      "300.0",
			"slack_thread_bead_id": "head-1",
		},
	}

	ev := makeReactionEvent("U_HUMAN", "black_square_for_stop", "C1", "300.0")
	bot.handleReactionAdded(context.Background(), ev)

	if daemon.beads["head-1"].Status != "closed" {
		t.Fatalf("expected agent-fallback resolution to close wish, got %q",
			daemon.beads["head-1"].Status)
	}
	if got := daemon.beads["head-1"].Fields["close_reason"]; got != wishCloseReason {
		t.Fatalf("expected close_reason=%q, got %q", wishCloseReason, got)
	}
}

// TestHandleWishCloseReaction_UnresolvedNoOp verifies a check-react on a
// message that resolves to no wish head does nothing.
func TestHandleWishCloseReaction_UnresolvedNoOp(t *testing.T) {
	daemon := newMockDaemon()
	srv := newFakeSlackServer(t)
	defer srv.Close()
	bot := newTestBot(daemon, srv)
	bot.botUserID = "BOT"

	ev := makeReactionEvent("U_HUMAN", "black_square_for_stop", "C1", "999.9")
	bot.handleReactionAdded(context.Background(), ev)

	if len(daemon.getClosed()) != 0 {
		t.Fatalf("expected no close for an unresolved reaction, got %+v", daemon.getClosed())
	}
}

// TestHandleWishCloseReaction_NeedsTriageRefused verifies the 1b-7 invariant:
// a wish whose bound agent FAILED is held open with the needs-triage label, and
// a "done" check-react must NOT close it (a crash is the opposite of done).
func TestHandleWishCloseReaction_NeedsTriageRefused(t *testing.T) {
	daemon := newMockDaemon()
	srv := newFakeSlackServer(t)
	defer srv.Close()
	bot := newTestBot(daemon, srv)
	bot.botUserID = "BOT"

	id := seedWishHead(daemon, "head-1", "C1", "100.0", "200.5")
	// Mark the wish needs-triage exactly as thread_task_autoclose.markNeedsTriage
	// does: leave it open, add the durable label + provenance field.
	daemon.beads[id].Labels = append(daemon.beads[id].Labels, triageLabel)
	daemon.beads[id].Fields["triage_reason"] = triageReasonAgentFailed

	ev := makeReactionEvent("U_HUMAN", "black_square_for_stop", "C1", "200.5")
	bot.handleReactionAdded(context.Background(), ev)

	if daemon.beads["head-1"].Status == "closed" {
		t.Fatal("a needs-triage wish must NOT be closed by a ⏹️ react")
	}
	if len(daemon.getClosed()) != 0 {
		t.Fatalf("expected no close call for a needs-triage wish, got %+v", daemon.getClosed())
	}
}

// TestHandleWishCloseReaction_OffCardFallsThroughToRule verifies a wish-close
// emoji (⏹️) reaction on an ORDINARY message (one that resolves to no wish head)
// is NOT swallowed: it falls through to the legacy reaction-rule spawn path, so
// a rule bead keyed on black_square_for_stop still fires off-card.
func TestHandleWishCloseReaction_OffCardFallsThroughToRule(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProject("monorepo")
	srv := newFakeSlackServerWithHistory(t, "please re-run the thing")
	defer srv.Close()
	bot := newTestBot(daemon, srv)
	bot.botUserID = "BOT"

	// A reaction-rule keyed on the wish-close emoji. There is NO wish head for
	// the reacted ts, so the wish-close branch must fall through to this rule.
	bot.reactionRuleCache = &reactionRuleCache{
		rules: []*beadsapi.BeadDetail{
			makeReactionRule("rule-1", "black_square_for_stop", "", "monorepo", "crew", "approved — proceed"),
		},
		fetchedAt: time.Now(),
		ttl:       5 * time.Minute,
	}

	ev := makeReactionEvent("U_HUMAN", "black_square_for_stop", "C-other", "777.7")
	bot.handleReactionAdded(context.Background(), ev)

	if len(daemon.getClosed()) != 0 {
		t.Fatalf("off-card ⏹️ react must not close anything, got %+v", daemon.getClosed())
	}
	if n := len(filterAgentBeads(daemon.beads)); n != 1 {
		t.Fatalf("expected the black_square_for_stop reaction-rule to spawn 1 agent off-card, got %d", n)
	}
}

// TestIsWishCloseEmoji guards the emoji SET membership. The wish-close glyph is
// :black_square_for_stop: (⏹️), deliberately distinct from the bot's ✅
// done-lifecycle glyph (ka-0000002200) — so white_check_mark and friends must
// NOT be wish-close emojis, alongside the bar_chart human-survey built-in.
func TestIsWishCloseEmoji(t *testing.T) {
	for _, e := range []string{"black_square_for_stop"} {
		if !isWishCloseEmoji(e) {
			t.Errorf("expected %q to be a wish-close emoji", e)
		}
	}
	for _, e := range []string{
		"white_check_mark", "heavy_check_mark", "ballot_box_with_check",
		"bar_chart", "x", "eyes", "thumbsup", "black_square_for_stop_2", "",
	} {
		if isWishCloseEmoji(e) {
			t.Errorf("expected %q NOT to be a wish-close emoji", e)
		}
	}
}
