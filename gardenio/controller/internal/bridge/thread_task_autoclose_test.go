package bridge

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// newTestAutoCloser builds an auto-closer wired to the supplied mockDaemon
// with a synchronous afterFunc — the scheduled callback is captured and
// invoked explicitly by the test via runScheduled. This lets us assert
// scheduling behavior without sleeping for the real grace window.
func newTestAutoCloser(d *mockDaemon) (*ThreadTaskAutoCloser, *deferredScheduler) {
	sched := &deferredScheduler{}
	c := &ThreadTaskAutoCloser{
		daemon:    d,
		logger:    slog.Default(),
		window:    30 * time.Minute,
		afterFunc: sched.AfterFunc,
		timers:    make(map[string]*time.Timer),
	}
	return c, sched
}

type deferredScheduler struct {
	mu            sync.Mutex
	pending       []func()
	scheduleCount int
}

func (s *deferredScheduler) AfterFunc(_ time.Duration, f func()) *time.Timer {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending = append(s.pending, f)
	s.scheduleCount++
	return time.NewTimer(time.Hour) // returned timer isn't used by tests
}

func (s *deferredScheduler) runOne() bool {
	s.mu.Lock()
	if len(s.pending) == 0 {
		s.mu.Unlock()
		return false
	}
	f := s.pending[0]
	s.pending = s.pending[1:]
	s.mu.Unlock()
	f()
	return true
}

func (s *deferredScheduler) drain() int {
	n := 0
	for s.runOne() {
		n++
	}
	return n
}

func (s *deferredScheduler) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.scheduleCount
}

// seedThreadTask seeds the mock with an open thread task bead carrying the
// canonical slack-thread:{channel}:{ts} label and returns its id.
func seedThreadTask(d *mockDaemon, channel, threadTS string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	id := "thread-" + sanitizeTS(threadTS)
	d.beads[id] = &beadsapi.BeadDetail{
		ID:     id,
		Title:  "Slack thread " + channel + "/" + threadTS,
		Type:   "task",
		Kind:   "issue",
		Status: "open",
		Labels: []string{"source:slack", threadBeadLabel(channel, threadTS)},
		Fields: map[string]string{
			"slack_thread_channel": channel,
			"slack_thread_ts":      threadTS,
		},
	}
	return id
}

// seedThreadAgent seeds the mock with an agent bead bound to a slack thread.
func seedThreadAgent(d *mockDaemon, id, threadBeadID, channel, threadTS, state string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.beads[id] = &beadsapi.BeadDetail{
		ID:    id,
		Title: id,
		Type:  "agent",
		Status: func() string {
			if state == "done" || state == "failed" {
				return "closed"
			}
			return "open"
		}(),
		Fields: map[string]string{
			"agent":                id,
			"agent_state":          state,
			"slack_thread_bead_id": threadBeadID,
			"slack_thread_channel": channel,
			"slack_thread_ts":      threadTS,
			"spawn_source":         "slack-thread",
		},
	}
}

func agentDoneEvent(id, threadBeadID, channel, threadTS string) BeadEvent {
	return BeadEvent{
		ID:     id,
		Type:   "agent",
		Status: "closed",
		Fields: map[string]string{
			"agent_state":          "done",
			"slack_thread_bead_id": threadBeadID,
			"slack_thread_channel": channel,
			"slack_thread_ts":      threadTS,
			"spawn_source":         "slack-thread",
		},
	}
}

func TestThreadTaskAutoCloser_DefaultWindow(t *testing.T) {
	c := NewThreadTaskAutoCloser(ThreadTaskAutoCloserConfig{
		Daemon: newMockDaemon(),
	})
	if c.Window() != DefaultThreadTaskCloseWindow {
		t.Errorf("expected default window %s, got %s", DefaultThreadTaskCloseWindow, c.Window())
	}
}

func TestThreadTaskAutoCloser_CustomWindow(t *testing.T) {
	c := NewThreadTaskAutoCloser(ThreadTaskAutoCloserConfig{
		Daemon: newMockDaemon(),
		Window: 5 * time.Minute,
	})
	if c.Window() != 5*time.Minute {
		t.Errorf("expected 5m window, got %s", c.Window())
	}
}

// A second agent spawned for the same thread before the window elapses must
// suppress the idle hint.
func TestThreadTaskAutoCloser_SkipsWhenAgentRespawned(t *testing.T) {
	d := newMockDaemon()
	channel := "C-auto-2"
	threadTS := "3333.4444"
	threadID := seedThreadTask(d, channel, threadTS)
	seedThreadAgent(d, "agent-done-2", threadID, channel, threadTS, "done")

	c, sched := newTestAutoCloser(d)
	c.MaybeSchedule(agentDoneEvent("agent-done-2", threadID, channel, threadTS))

	// Simulate a follow-up @mention spawning a new agent on the same thread
	// before the window elapses.
	seedThreadAgent(d, "agent-fresh-2", threadID, channel, threadTS, "working")

	sched.drain()

	// Thread bead must remain open.
	d.mu.Lock()
	bead := d.beads[threadID]
	d.mu.Unlock()
	if bead.Status != "open" {
		t.Fatalf("expected thread bead open, got %q", bead.Status)
	}
	if got := len(d.getClosed()); got != 0 {
		t.Fatalf("expected no close calls, got %d", got)
	}
	if got := len(d.getComments()); got != 0 {
		t.Fatalf("expected no comments, got %d", got)
	}
}

// Multiple successive done closes for the same thread coalesce: the timer is
// rescheduled per close, and the deferred checks post one hint between them.
func TestThreadTaskAutoCloser_ReschedulesPerClose(t *testing.T) {
	d := newMockDaemon()
	channel := "C-auto-3"
	threadTS := "5555.6666"
	threadID := seedThreadTask(d, channel, threadTS)
	seedThreadAgent(d, "agent-done-3a", threadID, channel, threadTS, "done")
	seedThreadAgent(d, "agent-done-3b", threadID, channel, threadTS, "done")

	c, sched := newTestAutoCloser(d)
	c.MaybeSchedule(agentDoneEvent("agent-done-3a", threadID, channel, threadTS))
	c.MaybeSchedule(agentDoneEvent("agent-done-3b", threadID, channel, threadTS))

	if got := sched.count(); got != 2 {
		t.Fatalf("expected 2 scheduled checks (one per close), got %d", got)
	}

	// Drain both deferred checks; the second finds this episode already
	// hinted and skips.
	sched.drain()

	if got := len(d.getComments()); got != 1 {
		t.Fatalf("expected exactly 1 hint (second run is deduped), got %d (%+v)", got, d.getComments())
	}
	if got := len(d.getClosed()); got != 0 {
		t.Fatalf("expected no close calls, got %d", got)
	}
}

func TestThreadTaskAutoCloser_IgnoresNonThreadAgents(t *testing.T) {
	d := newMockDaemon()
	c, sched := newTestAutoCloser(d)

	// No slack_thread_bead_id → must not schedule.
	c.MaybeSchedule(BeadEvent{
		ID:   "regular-agent",
		Type: "agent",
		Fields: map[string]string{
			"agent_state": "done",
		},
	})
	if sched.count() != 0 {
		t.Errorf("expected no schedule for non-thread agent, got %d", sched.count())
	}
}

// A non-terminal agent state must not schedule any check.
func TestThreadTaskAutoCloser_IgnoresNonTerminalCloses(t *testing.T) {
	d := newMockDaemon()
	c, sched := newTestAutoCloser(d)

	// agent_state=working → not terminal; nothing to propagate yet.
	c.MaybeSchedule(BeadEvent{
		ID:   "live-agent",
		Type: "agent",
		Fields: map[string]string{
			"agent_state":          "working",
			"slack_thread_bead_id": "thread-x",
			"slack_thread_channel": "C-x",
			"slack_thread_ts":      "1.2",
		},
	})
	if sched.count() != 0 {
		t.Errorf("expected no schedule for a non-terminal agent, got %d", sched.count())
	}
}

// Failed→needs-triage split (1b-7, ka-0000003060), event path: a FAILED agent
// DOES schedule a deferred check (unlike the old behavior that dropped it), and
// that check marks the wish needs-triage and LEAVES IT OPEN rather than
// idle-hinting it — a crash is the opposite of "looks done".
func TestThreadTaskAutoCloser_FailedAgent_MarksNeedsTriage(t *testing.T) {
	d := newMockDaemon()
	channel := "C-triage-1"
	threadTS := "4242.0001"
	threadID := seedThreadTask(d, channel, threadTS)
	seedThreadAgent(d, "agent-failed-1", threadID, channel, threadTS, "failed")

	c, sched := newTestAutoCloser(d)

	c.MaybeSchedule(BeadEvent{
		ID:   "agent-failed-1",
		Type: "agent",
		Fields: map[string]string{
			"agent_state":          "failed",
			"slack_thread_bead_id": threadID,
			"slack_thread_channel": channel,
			"slack_thread_ts":      threadTS,
		},
	})
	if got := sched.count(); got != 1 {
		t.Fatalf("expected 1 scheduled check for a failed agent, got %d", got)
	}
	sched.drain()

	// Wish must stay OPEN and must NOT be closed.
	d.mu.Lock()
	bead := d.beads[threadID]
	d.mu.Unlock()
	if bead.Status != "open" {
		t.Fatalf("expected wish to stay open after failure, got %q", bead.Status)
	}
	if got := len(d.getClosed()); got != 0 {
		t.Fatalf("expected no close calls on failure, got %d", got)
	}

	// needs-triage label + triage_reason field must be stamped.
	if !beadHasLabel(bead, triageLabel) {
		t.Errorf("expected needs-triage label, got %v", bead.Labels)
	}
	if got := bead.Fields["triage_reason"]; got != triageReasonAgentFailed {
		t.Errorf("expected triage_reason=%q, got %q", triageReasonAgentFailed, got)
	}
	if got := bead.Fields["triaged_agent"]; got != "agent-failed-1" {
		t.Errorf("expected triaged_agent=agent-failed-1, got %q", got)
	}

	// Exactly one triage comment, referencing the failed agent, and it must
	// NOT be a clean-close or "looks done" idle-hint comment.
	comments := d.getComments()
	if len(comments) != 1 {
		t.Fatalf("expected 1 triage comment, got %d (%+v)", len(comments), comments)
	}
	if !contains(comments[0].Text, "agent-failed-1") {
		t.Errorf("expected triage comment to reference the failed agent, got %q", comments[0].Text)
	}
	if !contains(comments[0].Text, triageMarker) {
		t.Errorf("expected triage comment to carry the triage marker, got %q", comments[0].Text)
	}
	if contains(comments[0].Text, "Auto-closed") {
		t.Errorf("triage comment must not claim the wish was closed, got %q", comments[0].Text)
	}
	if contains(comments[0].Text, idleHintMarker) {
		t.Errorf("triage comment must not be an idle hint, got %q", comments[0].Text)
	}
}

// Failed→needs-triage split (1b-7): the mark is idempotent — a re-fired timer
// (e.g. bridge restart re-armed the check) on an already-triaged wish posts no
// duplicate comment and re-stamps nothing.
func TestThreadTaskAutoCloser_FailedAgent_TriageIdempotent(t *testing.T) {
	d := newMockDaemon()
	channel := "C-triage-2"
	threadTS := "4242.0002"
	threadID := seedThreadTask(d, channel, threadTS)
	seedThreadAgent(d, "agent-failed-2", threadID, channel, threadTS, "failed")

	c, _ := newTestAutoCloser(d)

	// Two deferred checks for the same failed episode → exactly one triage mark.
	c.runCheck(context.Background(), "agent-failed-2", threadID, channel, threadTS, true)
	c.runCheck(context.Background(), "agent-failed-2", threadID, channel, threadTS, true)

	if got := len(d.getComments()); got != 1 {
		t.Fatalf("expected exactly 1 triage comment across two checks (idempotent), got %d (%+v)", got, d.getComments())
	}
	d.mu.Lock()
	labels := d.beads[threadID].Labels
	d.mu.Unlock()
	triageCount := 0
	for _, l := range labels {
		if l == triageLabel {
			triageCount++
		}
	}
	if triageCount != 1 {
		t.Fatalf("expected the needs-triage label exactly once, got %d (%v)", triageCount, labels)
	}
}

// Failed→needs-triage split (1b-7), sweep path: the backfill sweep must SKIP a
// wish already marked needs-triage — never idle-hint it (its failed agent bead
// is gone from the liveness index, so it would otherwise look like a stale idle
// orphan).
func TestThreadTaskAutoCloser_Sweep_SkipsNeedsTriage(t *testing.T) {
	d := newMockDaemon()
	channel := "C-triage-3"
	threadTS := "4242.0003"
	threadID := seedThreadTask(d, channel, threadTS)
	// Pre-mark the wish needs-triage (as the event path would have) and make
	// it look long-idle so the sweep would otherwise act on it.
	d.mu.Lock()
	d.beads[threadID].Labels = append(d.beads[threadID].Labels, triageLabel)
	d.beads[threadID].UpdatedAt = time.Now().Add(-2 * time.Hour)
	d.mu.Unlock()

	c, _ := newTestAutoCloser(d)
	c.nowFunc = func() time.Time { return time.Now() }

	c.sweepOnce(context.Background())

	d.mu.Lock()
	status := d.beads[threadID].Status
	d.mu.Unlock()
	if status != "open" {
		t.Fatalf("expected triaged wish to stay open, got %q", status)
	}
	if got := len(d.getClosed()); got != 0 {
		t.Fatalf("expected sweep to skip triaged wish (no close), got %d", got)
	}
	if got := len(d.getComments()); got != 0 {
		t.Fatalf("expected sweep to skip triaged wish (no comment/hint), got %d (%+v)", got, d.getComments())
	}
}

// The hint comment must carry explicit operator attribution on the write
// context so it never depends on the mutable client-level SetSystemOperator
// default. The daemon's AddComment write path fail-closes with HTTP 400 when
// no operator resolves (gd-SuKQU4gHuD). When the thread bead exposes a human
// operator, the comment is attributed to that human.
func TestThreadTaskAutoCloser_StampsHumanOperatorFromThreadBead(t *testing.T) {
	d := newMockDaemon()
	channel := "C-op-1"
	threadTS := "9999.0000"
	threadID := seedThreadTask(d, channel, threadTS)
	// Stamp a human operator on the thread bead.
	d.mu.Lock()
	d.beads[threadID].OperatedBy = "slack:U0OPERATOR1"
	d.mu.Unlock()
	seedThreadAgent(d, "agent-op-1", threadID, channel, threadTS, "done")

	c, sched := newTestAutoCloser(d)
	c.MaybeSchedule(agentDoneEvent("agent-op-1", threadID, channel, threadTS))
	sched.drain()

	d.mu.Lock()
	commentCtx := d.lastAddCommentCtx
	d.mu.Unlock()

	if commentCtx == nil {
		t.Fatal("expected AddComment to receive a non-nil ctx")
	}
	if got := beadsapi.OperatorFromContext(commentCtx); got != "slack:U0OPERATOR1" {
		t.Errorf("expected X-Operated-By=slack:U0OPERATOR1 on comment ctx, got %q", got)
	}
	// A resolved human wins over the system sentinel — no X-System-Operator.
	if got := beadsapi.SystemOperatorFromContext(commentCtx); got != "" {
		t.Errorf("expected no system-operator override when human resolved, got %q", got)
	}
}

// When the thread bead carries no human-shaped operator, the auto-closer
// falls back to the X-System-Operator sentinel so the write is always
// attributable and the daemon never fail-closes the comment.
func TestThreadTaskAutoCloser_FallsBackToSystemOperator(t *testing.T) {
	d := newMockDaemon()
	channel := "C-op-2"
	threadTS := "1212.3434"
	threadID := seedThreadTask(d, channel, threadTS)
	// A non-human sentinel on the thread bead → fallback to system operator.
	d.mu.Lock()
	d.beads[threadID].OperatedBy = "fleet:slack-bridge"
	d.mu.Unlock()
	seedThreadAgent(d, "agent-op-2", threadID, channel, threadTS, "done")

	c, sched := newTestAutoCloser(d)
	c.MaybeSchedule(agentDoneEvent("agent-op-2", threadID, channel, threadTS))
	sched.drain()

	d.mu.Lock()
	commentCtx := d.lastAddCommentCtx
	d.mu.Unlock()

	if commentCtx == nil {
		t.Fatal("expected AddComment to receive a non-nil ctx")
	}
	if got := beadsapi.SystemOperatorFromContext(commentCtx); got != autoCloseSystemOperator {
		t.Errorf("expected X-System-Operator=%q on comment ctx, got %q", autoCloseSystemOperator, got)
	}
	// A non-human sentinel must NOT be re-sent as X-Operated-By.
	if got := beadsapi.OperatorFromContext(commentCtx); got != "" {
		t.Errorf("expected no X-Operated-By for non-human operator, got %q", got)
	}
}

// The system-operator label must satisfy the daemon's
// sanitizeSystemOperatorHeader rules, or the daemon drops the header and the
// write fail-closes with the very 400 this fix prevents (gd-SuKQU4gHuD). This
// mirrors TestSystemOperatorSourceIsValidLabel in cmd/slack-bridge.
func TestAutoCloseSystemOperatorIsValidLabel(t *testing.T) {
	s := autoCloseSystemOperator
	if len(s) == 0 || len(s) > 64 {
		t.Fatalf("label %q length out of [1,64]", s)
	}
	if s[0] == '-' || s[len(s)-1] == '-' {
		t.Fatalf("label %q must not start/end with dash", s)
	}
	prevDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			prevDash = false
		case r == '-':
			if prevDash {
				t.Fatalf("label %q has double dash", s)
			}
			prevDash = true
		default:
			t.Fatalf("label %q has invalid char %q", s, r)
		}
	}
}

func TestIsHumanShapedOperator(t *testing.T) {
	cases := []struct {
		op   string
		want bool
	}{
		{"slack:U0OPERATOR1", true},
		{"oidc:matt@acme.example", true},
		{"fleet:slack-bridge", false},
		{"system:slack-bridge", false},
		{"", false},
		{"gardenio", false},
	}
	for _, tc := range cases {
		if got := isHumanShapedOperator(tc.op); got != tc.want {
			t.Errorf("isHumanShapedOperator(%q) = %v, want %v", tc.op, got, tc.want)
		}
	}
}

// Event path (ka-0000007276, gd-Zhcpn9IZVl): a done agent with no surviving
// agent makes runCheck post a hint comment and LEAVE the thread bead open; the
// auto-closer never closes a wish.
func TestThreadTaskAutoCloser_HintsOrphanedThreadTask(t *testing.T) {
	d := newMockDaemon()
	channel := "C-hint-1"
	threadTS := "9999.0001"
	threadID := seedThreadTask(d, channel, threadTS)
	seedThreadAgent(d, "agent-hint-1", threadID, channel, threadTS, "done")

	c, sched := newTestAutoCloser(d)

	c.MaybeSchedule(agentDoneEvent("agent-hint-1", threadID, channel, threadTS))
	if got := sched.count(); got != 1 {
		t.Fatalf("expected 1 deferred check, got %d", got)
	}
	sched.drain()

	// Thread bead must remain OPEN.
	d.mu.Lock()
	bead := d.beads[threadID]
	d.mu.Unlock()
	if bead.Status != "open" {
		t.Fatalf("expected thread bead to stay open, got %q", bead.Status)
	}

	// No close call at all.
	if got := len(d.getClosed()); got != 0 {
		t.Fatalf("expected no close calls, got %d", got)
	}

	// A single hint comment referencing the last agent must be posted.
	comments := d.getComments()
	if len(comments) != 1 {
		t.Fatalf("expected 1 hint comment, got %d (%+v)", len(comments), comments)
	}
	if comments[0].BeadID != threadID {
		t.Errorf("expected hint comment on %s, got %s", threadID, comments[0].BeadID)
	}
	if !contains(comments[0].Text, "agent-hint-1") {
		t.Errorf("expected hint comment to reference agent id, got %q", comments[0].Text)
	}
	if contains(comments[0].Text, "Auto-closed") {
		t.Errorf("hint comment must not claim the bead was closed, got %q", comments[0].Text)
	}
}

// Sweep path (ka-0000007276, gd-Zhcpn9IZVl): sweepOnce posts a hint and
// LEAVES the orphaned thread bead open.
func TestThreadTaskAutoCloser_SweepHintsOrphanedThreadTask(t *testing.T) {
	d := newMockDaemon()
	channel := "C-hint-2"
	threadTS := "9999.0002"
	threadID := seedThreadTask(d, channel, threadTS)
	// Unlinked terminal agent → the sweep hints on this orphan.
	seedThreadAgent(d, "agent-hint-2", "", channel, threadTS, "done")
	// 1b-8: the hint is gated on positive evidence, so the terminal witness
	// must remain listed for the sweep to act on this thread at all.
	d.mu.Lock()
	d.beads["agent-hint-2"].Status = "open"
	d.mu.Unlock()

	c, _ := newTestAutoCloser(d)
	c.nowFunc = func() time.Time { return time.Now() }

	c.sweepOnce(context.Background())

	d.mu.Lock()
	status := d.beads[threadID].Status
	d.mu.Unlock()
	if status != "open" {
		t.Fatalf("expected thread bead to stay open, got %q", status)
	}
	if got := len(d.getClosed()); got != 0 {
		t.Fatalf("expected no close calls, got %d", got)
	}
	comments := d.getComments()
	if len(comments) != 1 || comments[0].BeadID != threadID {
		t.Fatalf("expected 1 hint comment on %s, got %+v", threadID, comments)
	}
	if contains(comments[0].Text, "Auto-closed") {
		t.Errorf("hint comment must not claim the bead was closed, got %q", comments[0].Text)
	}
}

// 1b-6 dedupe (ka-0000001768), sweep path: a thread that stays open across
// MULTIPLE sweep intervals must be hinted exactly ONCE per idle episode — the
// second sweep within the same episode posts no new comment. This is the core
// anti-spam guarantee of the hint path.
func TestThreadTaskAutoCloser_HintDedupe_SweepPath(t *testing.T) {
	d := newMockDaemon()
	channel := "C-dedupe-1"
	threadTS := "8888.0001"
	threadID := seedThreadTask(d, channel, threadTS)
	seedThreadAgent(d, "agent-dedupe-1", "", channel, threadTS, "done")
	// 1b-8: keep the terminal witness listed so the hint path is reachable.
	d.mu.Lock()
	d.beads["agent-dedupe-1"].Status = "open"
	d.mu.Unlock()

	c, _ := newTestAutoCloser(d)
	c.nowFunc = func() time.Time { return time.Now() }

	// First sweep posts the single idle hint.
	c.sweepOnce(context.Background())
	if got := len(d.getComments()); got != 1 {
		t.Fatalf("first sweep: expected 1 hint comment, got %d (%+v)", got, d.getComments())
	}

	// Second sweep within the same idle episode must NOT re-hint.
	c.sweepOnce(context.Background())
	if got := len(d.getComments()); got != 1 {
		t.Fatalf("second sweep: expected still 1 hint comment (dedupe), got %d (%+v)", got, d.getComments())
	}

	// A third sweep for good measure — still one.
	c.sweepOnce(context.Background())
	if got := len(d.getComments()); got != 1 {
		t.Fatalf("third sweep: expected still 1 hint comment (dedupe), got %d (%+v)", got, d.getComments())
	}

	// And the bead stays open the whole time.
	d.mu.Lock()
	status := d.beads[threadID].Status
	d.mu.Unlock()
	if status != "open" {
		t.Fatalf("expected thread bead to stay open, got %q", status)
	}
	if got := len(d.getClosed()); got != 0 {
		t.Fatalf("expected no close calls, got %d", got)
	}
}

// 1b-6 dedupe (ka-0000001768), event path: repeated deferred checks for the
// same idle episode post exactly one hint.
func TestThreadTaskAutoCloser_HintDedupe_EventPath(t *testing.T) {
	d := newMockDaemon()
	channel := "C-dedupe-2"
	threadTS := "8888.0002"
	threadID := seedThreadTask(d, channel, threadTS)
	seedThreadAgent(d, "agent-dedupe-2", threadID, channel, threadTS, "done")

	c, _ := newTestAutoCloser(d)

	// Run the deferred check directly twice (e.g. a re-fire after a bridge
	// restart re-armed the timer) — only the first posts a hint.
	c.runCheck(context.Background(), "agent-dedupe-2", threadID, channel, threadTS, false)
	c.runCheck(context.Background(), "agent-dedupe-2", threadID, channel, threadTS, false)

	if got := len(d.getComments()); got != 1 {
		t.Fatalf("expected exactly 1 hint comment across two checks (dedupe), got %d (%+v)", got, d.getComments())
	}
}

// 1b-6 dedupe (ka-0000001768): fresh real activity (a human comment) AFTER a
// hint opens a NEW idle episode, so the next sweep is allowed to hint again —
// dedupe is activity-relative, not a permanent gag.
func TestThreadTaskAutoCloser_HintDedupe_RehintsAfterFreshActivity(t *testing.T) {
	d := newMockDaemon()
	channel := "C-dedupe-3"
	threadTS := "8888.0003"
	threadID := seedThreadTask(d, channel, threadTS)
	seedThreadAgent(d, "agent-dedupe-3", "", channel, threadTS, "done")
	// 1b-8: keep the terminal witness listed so the hint path is reachable
	// across both idle episodes this test drives.
	d.mu.Lock()
	d.beads["agent-dedupe-3"].Status = "open"
	d.mu.Unlock()

	c, _ := newTestAutoCloser(d)
	c.nowFunc = func() time.Time { return time.Now() }

	// Episode 1: first sweep hints once; second is deduped.
	c.sweepOnce(context.Background())
	c.sweepOnce(context.Background())
	if got := len(d.getComments()); got != 1 {
		t.Fatalf("episode 1: expected 1 hint, got %d (%+v)", got, d.getComments())
	}

	// A human replies on the thread — recorded as a non-hint comment, which is
	// newer than the prior hint and thus opens a new idle episode.
	if err := d.AddComment(context.Background(), threadID, "U-human", "any update here?"); err != nil {
		t.Fatalf("seed human comment: %v", err)
	}

	// Episode 2: the next sweep is allowed to hint again (now 1 hint + 1 human
	// comment recorded = 2 comments referencing the thread via getComments,
	// but exactly 2 HINTs total).
	c.sweepOnce(context.Background())
	hints := 0
	for _, cm := range d.getComments() {
		if cm.BeadID == threadID && contains(cm.Text, idleHintMarker) {
			hints++
		}
	}
	if hints != 2 {
		t.Fatalf("episode 2: expected 2 total hints after fresh activity, got %d (%+v)", hints, d.getComments())
	}
}

// 1b-6 actionable hint (ka-0000001768): the idle nudge must carry the in-place
// close affordance (⏹️ react — the wish-close gesture, ka-0000002200) so it is
// actionable in Slack, and must carry the machine dedupe marker.
func TestActionableIdleHint_Shape(t *testing.T) {
	withAgent := actionableIdleHint(30*time.Minute, "agent-x")
	if !contains(withAgent, "agent-x") {
		t.Errorf("expected hint to reference the last agent, got %q", withAgent)
	}
	if !contains(withAgent, "black_square_for_stop") {
		t.Errorf("expected hint to carry the ⏹️-react close affordance, got %q", withAgent)
	}
	if !contains(withAgent, idleHintMarker) {
		t.Errorf("expected hint to carry the dedupe marker, got %q", withAgent)
	}
	if contains(withAgent, "Auto-closed") {
		t.Errorf("hint must not claim the bead was closed, got %q", withAgent)
	}

	// Sweep variant (no known agent) still carries the affordance + marker.
	noAgent := actionableIdleHint(30*time.Minute, "")
	if !contains(noAgent, "black_square_for_stop") || !contains(noAgent, idleHintMarker) {
		t.Errorf("sweep-variant hint missing affordance/marker, got %q", noAgent)
	}
}

// End-to-end: Agents.handleClosed for a slack-thread agent fires the idle
// hint. Pins the wiring at the watcher boundary.
func TestAgents_HandleClosed_PropagatesThreadIdleHint(t *testing.T) {
	d := newMockDaemon()
	channel := "C-handle-1"
	threadTS := "7777.8888"
	threadID := seedThreadTask(d, channel, threadTS)
	seedThreadAgent(d, "agent-h1", threadID, channel, threadTS, "done")

	closer, sched := newTestAutoCloser(d)

	notif := &mockAgentNotifier{}
	a := NewAgents(AgentsConfig{
		Notifier:             notif,
		Logger:               slog.Default(),
		ThreadTaskAutoCloser: closer,
	})

	payload := marshalSSEBeadPayload(agentDoneEvent("agent-h1", threadID, channel, threadTS))
	a.handleClosed(context.Background(), payload)

	if sched.count() != 1 {
		t.Fatalf("expected 1 scheduled check, got %d", sched.count())
	}
	sched.drain()

	d.mu.Lock()
	status := d.beads[threadID].Status
	d.mu.Unlock()
	if status != "open" {
		t.Fatalf("expected thread bead to stay open, got %q", status)
	}
	comments := d.getComments()
	if len(comments) != 1 || !contains(comments[0].Text, idleHintMarker) {
		t.Fatalf("expected 1 idle hint on %s, got %+v", threadID, comments)
	}
}
