package bridge

// thread_task_autoclose_demoted_test.go is the consolidated acceptance suite
// for the DEMOTED (hint-instead-of-close) world (1b-12, ka-0000000287). It was
// selected by SLACK_THREAD_TASK_HINT_INSTEAD_OF_CLOSE=true; since gd-Zhcpn9IZVl
// it is the auto-closer's only behaviour.
//
// The per-mechanism unit tests already live beside their features:
//   - thread_task_autoclose_test.go       — event-path hint/triage + dedupe
//   - thread_task_autoclose_sweep_test.go — backfill sweep + 1b-8 positive-evidence
//   - bot_reactions_wish_close_test.go    — 1b-11 user-close reaction handler
//   - thread_head_card_ts_test.go         — 1b-13 card-ts resolution
//
// This file is the STORY, not a re-derivation of those units: it exercises the
// nine acceptance scenarios named in ka-0000000287 as ONE coherent contract, so
// the flip is gated on a single readable artifact that asserts the demoted world
// hangs together. The through-line the operator is buying:
//
//	Under thread=wish, the idle auto-closer never KILLS a wish — it posts a
//	single actionable hint and leaves the wish OPEN. A satisfied wish is closed
//	by the human's ⏹️ reaction (1b-11) or by the herder. A crashed agent
//	holds the wish OPEN + needs-triage (1b-7) so a failure never vanishes. And the
//	sweep reaps NOTHING on absence-of-evidence (1b-8) — so demoting the closer
//	cannot regress into the never-dies orphan backlog it was built to drain.
//
// If any one leg regresses, it re-creates exactly the failure the
// hard-sequencing note in the task warned about: wishes that accumulate open
// forever (hint with no death authority) or wishes silently buried (a failed or
// idle wish closed without the user's say-so). Each scenario below is one such
// guardrail.

import (
	"context"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// newDemotedCloser builds an auto-closer with a deterministic clock.
func newDemotedCloser(d *mockDaemon, now time.Time) *ThreadTaskAutoCloser {
	c, _ := newTestAutoCloser(d)
	c.nowFunc = func() time.Time { return now }
	return c
}

// countHints returns how many idle-hint comments (carrying idleHintMarker) the
// mock recorded for beadID.
func countHints(d *mockDaemon, beadID string) int {
	n := 0
	for _, cm := range d.getComments() {
		if cm.BeadID == beadID && contains(cm.Text, idleHintMarker) {
			n++
		}
	}
	return n
}

// Scenario 1 — idle → hint-once. A done agent on a quiet thread posts a SINGLE
// actionable idle hint and LEAVES THE WISH OPEN. This is the core demotion: the
// death rule no longer kills; it nudges. The hint must carry the ⏹️ close
// affordance so the human knows how to actually end the wish.
func TestDemoted_Scenario1_IdleHintsOnceLeavesOpen(t *testing.T) {
	d := newMockDaemon()
	channel := "C-demoted-1"
	threadTS := "9100.0001"
	threadID := seedThreadTask(d, channel, threadTS)
	seedThreadAgent(d, "agent-1", threadID, channel, threadTS, "done")

	c := newDemotedCloser(d, time.Now())
	c.runCheck(context.Background(), "agent-1", threadID, channel, threadTS, false)

	d.mu.Lock()
	status := d.beads[threadID].Status
	d.mu.Unlock()
	if status != "open" {
		t.Fatalf("expected wish to stay OPEN after idle hint, got %q", status)
	}
	if got := len(d.getClosed()); got != 0 {
		t.Fatalf("demoted closer must not close on idle, got %d close calls", got)
	}
	if got := countHints(d, threadID); got != 1 {
		t.Fatalf("expected exactly 1 idle hint, got %d (%+v)", got, d.getComments())
	}
	hint := d.getComments()[0].Text
	if !contains(hint, "black_square_for_stop") {
		t.Errorf("idle hint must carry the ⏹️ close affordance, got %q", hint)
	}
	if contains(hint, "Auto-closed") {
		t.Errorf("idle hint must not claim the wish was closed, got %q", hint)
	}
}

// Scenario 2 — a re-mention (fresh live agent) suppresses the hint entirely. A
// follow-up @mention that spawns a new agent on the same thread before the check
// fires means the wish is being actively worked: no hint, no close. This is the
// "re-mention reopens" leg — activity on the thread must always win over the
// idle nudge.
func TestDemoted_Scenario2_RementionSuppressesHint(t *testing.T) {
	d := newMockDaemon()
	channel := "C-demoted-2"
	threadTS := "9100.0002"
	threadID := seedThreadTask(d, channel, threadTS)
	seedThreadAgent(d, "agent-2a", threadID, channel, threadTS, "done")

	c, sched := newTestAutoCloser(d)
	c.MaybeSchedule(agentDoneEvent("agent-2a", threadID, channel, threadTS))

	// A follow-up @mention spawns a fresh live agent before the window elapses.
	seedThreadAgent(d, "agent-2b", threadID, channel, threadTS, "working")

	sched.drain()

	d.mu.Lock()
	status := d.beads[threadID].Status
	d.mu.Unlock()
	if status != "open" {
		t.Fatalf("expected wish to stay open with a live re-mention agent, got %q", status)
	}
	if got := len(d.getClosed()); got != 0 {
		t.Fatalf("expected no close with a live agent, got %d", got)
	}
	if got := countHints(d, threadID); got != 0 {
		t.Fatalf("a live re-mention agent must suppress the idle hint, got %d hints", got)
	}
}

// Scenario 2b — dedupe is activity-relative, not a permanent gag: after a hint,
// FRESH real activity (a human reply) opens a NEW idle episode, so the next idle
// check is allowed to hint again. This is the other half of "re-mention
// reopens" — a re-touched-then-requiet thread earns a second nudge.
func TestDemoted_Scenario2b_FreshActivityReopensHintEpisode(t *testing.T) {
	d := newMockDaemon()
	channel := "C-demoted-2b"
	threadTS := "9100.0022"
	threadID := seedThreadTask(d, channel, threadTS)
	seedThreadAgent(d, "agent-2b1", threadID, channel, threadTS, "done")

	c := newDemotedCloser(d, time.Now())

	// Episode 1: two checks → exactly one hint (deduped).
	c.runCheck(context.Background(), "agent-2b1", threadID, channel, threadTS, false)
	c.runCheck(context.Background(), "agent-2b1", threadID, channel, threadTS, false)
	if got := countHints(d, threadID); got != 1 {
		t.Fatalf("episode 1: expected 1 hint, got %d", got)
	}

	// A human replies — a non-hint comment newer than the hint opens episode 2.
	if err := d.AddComment(context.Background(), threadID, "U-human", "still need this?"); err != nil {
		t.Fatalf("seed human comment: %v", err)
	}

	// Episode 2: the next check may hint again.
	c.runCheck(context.Background(), "agent-2b1", threadID, channel, threadTS, false)
	if got := countHints(d, threadID); got != 2 {
		t.Fatalf("episode 2: expected 2 total hints after fresh activity, got %d", got)
	}
	d.mu.Lock()
	status := d.beads[threadID].Status
	d.mu.Unlock()
	if status != "open" {
		t.Fatalf("wish must remain open across both episodes, got %q", status)
	}
}

// Scenario 3 — failed → needs-triage stays OPEN. A crashed agent is the opposite
// of "looks done": the closer must mark the wish needs-triage (label + field +
// comment) and hold it OPEN. It must NOT be
// idle-hinted ("looks done?") and must NOT be closed.
func TestDemoted_Scenario3_FailedAgentStaysOpenNeedsTriage(t *testing.T) {
	d := newMockDaemon()
	channel := "C-demoted-3"
	threadTS := "9100.0003"
	threadID := seedThreadTask(d, channel, threadTS)
	seedThreadAgent(d, "agent-3", threadID, channel, threadTS, "failed")

	c := newDemotedCloser(d, time.Now())
	c.runCheck(context.Background(), "agent-3", threadID, channel, threadTS, true /*failed*/)

	d.mu.Lock()
	bead := d.beads[threadID]
	d.mu.Unlock()
	if bead.Status != "open" {
		t.Fatalf("expected failed wish to stay OPEN, got %q", bead.Status)
	}
	if got := len(d.getClosed()); got != 0 {
		t.Fatalf("a failed wish must not be closed, got %d close calls", got)
	}
	if !beadHasLabel(bead, triageLabel) {
		t.Errorf("expected needs-triage label, got %v", bead.Labels)
	}
	if got := bead.Fields["triage_reason"]; got != triageReasonAgentFailed {
		t.Errorf("expected triage_reason=%q, got %q", triageReasonAgentFailed, got)
	}
	// Exactly one triage comment — and NOT an idle hint.
	if got := countHints(d, threadID); got != 0 {
		t.Errorf("a failure must never be idle-hinted, got %d hints", got)
	}
	comments := d.getComments()
	if len(comments) != 1 || !contains(comments[0].Text, triageMarker) {
		t.Fatalf("expected exactly 1 triage comment carrying the triage marker, got %+v", comments)
	}
}

// Scenario 4 — user-close is authoritative. A human ⏹️ reaction on the thread
// card closes the wish HEAD with close_reason=slack-thread-user-close, and the
// close is attributed to the reactor. This is the death authority the demotion
// hands to the Slack user, driven through the real handleReactionAdded entry.
func TestDemoted_Scenario4_UserCloseIsAuthoritative(t *testing.T) {
	d := newMockDaemon()
	srv := newFakeSlackServer(t)
	defer srv.Close()
	bot := newTestBot(d, srv)
	bot.botUserID = "BOT"

	seedWishHead(d, "head-4", "C-demoted-4", "9100.0004", "9100.5004")

	ev := makeReactionEvent("U_HUMAN", "black_square_for_stop", "C-demoted-4", "9100.5004")
	bot.handleReactionAdded(context.Background(), ev)

	head := d.beads["head-4"]
	if head.Status != "closed" {
		t.Fatalf("expected user ⏹️ to close the wish, got %q", head.Status)
	}
	if got := head.Fields["close_reason"]; got != wishCloseReason {
		t.Fatalf("expected close_reason=%q, got %q", wishCloseReason, got)
	}
	closed := d.getClosed()
	if len(closed) != 1 || closed[0].BeadID != "head-4" {
		t.Fatalf("expected exactly one close of head-4, got %+v", closed)
	}
	if op := beadsapi.OperatorFromContext(d.lastCloseBeadCtx); op != "slack:U_HUMAN" {
		t.Fatalf("expected close attributed to the reactor slack:U_HUMAN, got %q", op)
	}
}

// Scenario 5 — restart cold-cache card-close STILL works via 1b-13. The moment a
// user most wants to close a wish is exactly when its agent is done and gone and
// (after a bridge restart) the hot caches are cold. The ⏹️ react must still
// resolve the head via the persisted slack_card_ts field-query — independent of
// the agentCards cache and the terminal agent that the legacy metadata fallback
// skips. Without this leg, the death authority silently no-ops precisely when
// it's needed.
func TestDemoted_Scenario5_UserCloseAfterRestartColdCache(t *testing.T) {
	d := newMockDaemon()
	srv := newFakeSlackServer(t)
	defer srv.Close()
	// Fresh bot == simulated bridge restart: empty agentCards / message caches.
	bot := newTestBot(d, srv)
	bot.botUserID = "BOT"

	channel := "C-demoted-5"
	threadTS := "9100.0005"
	cardTS := "9100.5005"
	// Head carries the persisted card ref (1b-13 persistHeadCardRef).
	seedWishHead(d, "head-5", channel, threadTS, cardTS)
	// The bound agent is DONE and closed → drops out of ListAgentBeads and is
	// skipped by findAgentByThreadMetadata, so ONLY the head card-ts field-query
	// can resolve the wish.
	seedThreadAgent(d, "agent-5", "head-5", channel, threadTS, "done")

	// Guard: the legacy done-agent metadata path cannot resolve this thread.
	if got := bot.findAgentByThreadMetadata(channel, threadTS); got != "" {
		t.Fatalf("precondition: done agent must be skipped by metadata fallback, got %q", got)
	}

	// A ⏹️ react on the CARD ts (not the thread root) after the cold restart.
	ev := makeReactionEvent("U_HUMAN", "black_square_for_stop", channel, cardTS)
	bot.handleReactionAdded(context.Background(), ev)

	if d.beads["head-5"].Status != "closed" {
		t.Fatalf("expected card-react to close the wish via the 1b-13 card-ts path after restart, got %q",
			d.beads["head-5"].Status)
	}
	if got := d.beads["head-5"].Fields["close_reason"]; got != wishCloseReason {
		t.Fatalf("expected close_reason=%q, got %q", wishCloseReason, got)
	}
}

// Scenario 6 — the backfill sweep hints ONCE and leaves the orphan open, and
// re-hints NOTHING across subsequent intervals in the same idle episode. This is
// the anti-spam guarantee that MUST hold before the flip: a demoted closer that
// re-comments every 15m sweep would turn every quiet wish into a noise machine.
func TestDemoted_Scenario6_SweepHintsOnceAndDedupes(t *testing.T) {
	d := newMockDaemon()
	channel := "C-demoted-6"
	threadTS := "9100.0006"
	threadID := seedThreadTask(d, channel, threadTS)
	// Unlinked terminal witness (kept listed for 1b-8 positive evidence).
	seedThreadAgent(d, "agent-6", "", channel, threadTS, "done")
	d.mu.Lock()
	d.beads["agent-6"].Status = "open"
	d.mu.Unlock()
	// Leave UpdatedAt zero so the idle-window gate is skipped (it is guarded by
	// !UpdatedAt.IsZero()) and the sweep acts. This also keeps the dedupe clock
	// coherent: the mock stamps comment CreatedAt from a fixed epoch base, so a
	// real-time UpdatedAt would spuriously read as "newer than the hint" and
	// defeat the episode comparison. Mirrors the proven
	// TestThreadTaskAutoCloser_HintDedupe_SweepPath fixture.

	c := newDemotedCloser(d, time.Now())

	// Three sweeps within one idle episode → exactly one hint total.
	c.sweepOnce(context.Background())
	c.sweepOnce(context.Background())
	c.sweepOnce(context.Background())

	if got := countHints(d, threadID); got != 1 {
		t.Fatalf("sweep must hint exactly once per idle episode, got %d hints", got)
	}
	d.mu.Lock()
	status := d.beads[threadID].Status
	d.mu.Unlock()
	if status != "open" {
		t.Fatalf("sweep must leave the orphan OPEN in the demoted world, got %q", status)
	}
	if got := len(d.getClosed()); got != 0 {
		t.Fatalf("sweep must not close in the demoted world, got %d", got)
	}
}

// Scenario 7 — the sweep SKIPS a needs-triage wish entirely: never closes it
// (its failed agent's bead is gone from the liveness index, so without the guard
// the sweep would treat it as a stale idle orphan) and never idle-hints it
// ("looks done?" is wrong for a crash). This keeps a failed wish in the triage
// queue instead of silently burying it on a later sweep.
func TestDemoted_Scenario7_SweepSkipsNeedsTriage(t *testing.T) {
	d := newMockDaemon()
	channel := "C-demoted-7"
	threadTS := "9100.0007"
	threadID := seedThreadTask(d, channel, threadTS)
	// Pre-marked needs-triage and long-idle, with a live agent on an UNRELATED
	// thread so the listing is non-empty (bypasses the empty-listing guard) and
	// this scenario turns purely on the triage skip.
	seedThreadAgent(d, "agent-elsewhere", "", "C-other-7", "1.1", "working")
	d.mu.Lock()
	d.beads[threadID].Labels = append(d.beads[threadID].Labels, triageLabel)
	d.beads[threadID].UpdatedAt = time.Now().Add(-2 * time.Hour)
	d.mu.Unlock()

	c := newDemotedCloser(d, time.Now())
	c.sweepOnce(context.Background())

	d.mu.Lock()
	status := d.beads[threadID].Status
	d.mu.Unlock()
	if status != "open" {
		t.Fatalf("sweep must leave a needs-triage wish open, got %q", status)
	}
	if got := len(d.getClosed()); got != 0 {
		t.Fatalf("sweep must not close a needs-triage wish, got %d", got)
	}
	if got := countHints(d, threadID); got != 0 {
		t.Fatalf("sweep must not idle-hint a needs-triage wish, got %d hints", got)
	}
}

// Scenario 8 — positive-evidence-only reaping (1b-8). The sweep must never act on
// absence of evidence. Two indeterminate-liveness shapes that the pre-1b-8 sweep
// would have (wrongly) reaped or hinted must both leave the wish untouched, even
// in the demoted world:
//
//	(a) no terminal witness — an open, long-idle thread whose only listed agents
//	    belong to OTHER threads: nothing proves an agent ran-and-finished here.
//	(b) empty agent listing — a bridge boot/restart or degraded daemon reads as
//	    "no agents": that is indeterminate, NOT proof every agent is gone.
//
// Reaping (or even hinting) on either would re-introduce the negative-evidence
// close that demoting the closer is meant to stop, so the demoted world must be
// just as strict.
func TestDemoted_Scenario8_PositiveEvidenceOnly(t *testing.T) {
	t.Run("no-terminal-witness", func(t *testing.T) {
		d := newMockDaemon()
		channel := "C-demoted-8a"
		threadTS := "9100.0081"
		threadID := seedThreadTask(d, channel, threadTS)
		// A live agent for an UNRELATED thread: listing non-empty, but NO witness
		// for this thread.
		seedThreadAgent(d, "agent-8a-other", "", "C-other-8a", "2.2", "working")
		now := time.Now()
		d.mu.Lock()
		d.beads[threadID].UpdatedAt = now.Add(-2 * time.Hour)
		d.mu.Unlock()

		c := newDemotedCloser(d, now)
		c.sweepOnce(context.Background())

		d.mu.Lock()
		status := d.beads[threadID].Status
		d.mu.Unlock()
		if status != "open" {
			t.Fatalf("no terminal witness → wish must stay open, got %q", status)
		}
		if got := len(d.getClosed()); got != 0 {
			t.Fatalf("expected no close without positive evidence, got %d", got)
		}
		if got := countHints(d, threadID); got != 0 {
			t.Fatalf("expected no hint without positive evidence, got %d", got)
		}
	})

	t.Run("empty-agent-listing", func(t *testing.T) {
		d := newMockDaemon()
		channel := "C-demoted-8b"
		threadTS := "9100.0082"
		threadID := seedThreadTask(d, channel, threadTS)
		// No agent beads at all → ListAgentBeads returns empty (indeterminate).
		now := time.Now()
		d.mu.Lock()
		d.beads[threadID].UpdatedAt = now.Add(-2 * time.Hour)
		d.mu.Unlock()

		c := newDemotedCloser(d, now)
		c.sweepOnce(context.Background())

		d.mu.Lock()
		status := d.beads[threadID].Status
		d.mu.Unlock()
		if status != "open" {
			t.Fatalf("empty listing is indeterminate → wish must stay open, got %q", status)
		}
		if got := len(d.getClosed()); got != 0 {
			t.Fatalf("expected no close on empty agent listing, got %d", got)
		}
		if got := countHints(d, threadID); got != 0 {
			t.Fatalf("expected no hint on empty agent listing, got %d", got)
		}
	})
}

// Scenario 9 — user-close is REFUSED for a needs-triage wish. The user's ⏹️ is
// the close authority for a SATISFIED wish, but a crashed wish held for triage
// must not be silently buried by a "done" gesture. The user-close handler honors
// the same 1b-7 invariant the auto-closer does: it consumes the reaction (so no
// rule bead double-handles the card) but leaves the wish OPEN for triage.
func TestDemoted_Scenario9_UserCloseRefusedForNeedsTriage(t *testing.T) {
	d := newMockDaemon()
	srv := newFakeSlackServer(t)
	defer srv.Close()
	bot := newTestBot(d, srv)
	bot.botUserID = "BOT"

	id := seedWishHead(d, "head-9", "C-demoted-9", "9100.0009", "9100.5009")
	// Mark needs-triage exactly as markNeedsTriage would: open + durable label +
	// provenance field.
	d.mu.Lock()
	d.beads[id].Labels = append(d.beads[id].Labels, triageLabel)
	d.beads[id].Fields["triage_reason"] = triageReasonAgentFailed
	d.mu.Unlock()

	ev := makeReactionEvent("U_HUMAN", "black_square_for_stop", "C-demoted-9", "9100.5009")
	bot.handleReactionAdded(context.Background(), ev)

	if d.beads["head-9"].Status == "closed" {
		t.Fatal("a ⏹️ react must NOT close a needs-triage wish")
	}
	if got := len(d.getClosed()); got != 0 {
		t.Fatalf("expected no close for a needs-triage wish, got %+v", d.getClosed())
	}
}
