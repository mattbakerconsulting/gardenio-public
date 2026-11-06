package bridge

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// newSweepCloser builds an auto-closer with a fixed clock so the sweep's
// idle-since-update comparison is deterministic.
func newSweepCloser(d *mockDaemon, now time.Time) *ThreadTaskAutoCloser {
	c := NewThreadTaskAutoCloser(ThreadTaskAutoCloserConfig{
		Daemon: d,
		Logger: slog.Default(),
		Window: 30 * time.Minute,
	})
	c.nowFunc = func() time.Time { return now }
	return c
}

// assertHintedOpen fails unless threadID is still open, nothing was closed,
// and exactly one idle hint landed on it: the auto-closer never closes a wish
// (gd-Zhcpn9IZVl).
func assertHintedOpen(t *testing.T, d *mockDaemon, threadID string) {
	t.Helper()
	d.mu.Lock()
	status := d.beads[threadID].Status
	d.mu.Unlock()
	if status != "open" {
		t.Fatalf("expected %s to stay open, got %q", threadID, status)
	}
	if got := d.getClosed(); len(got) != 0 {
		t.Fatalf("expected no close calls, got %+v", got)
	}
	hints := 0
	for _, cm := range d.getComments() {
		if cm.BeadID == threadID && contains(cm.Text, idleHintMarker) {
			hints++
		}
	}
	if hints != 1 {
		t.Fatalf("expected exactly 1 idle hint on %s, got %d (%+v)", threadID, hints, d.getComments())
	}
}

// Sweep hints a thread task whose ONLY agent never carried
// slack_thread_bead_id (the linking gap): matching must key off the label.
//
// 1b-8 (ka-0000000126): the hint is positive-evidence-only, so the bound
// agent must still be LISTED with a terminal agent_state to serve as the
// ran-and-terminated witness. seedThreadAgent marks a "done" agent's bead
// closed (it drops out of ListAgentBeads), so we force its status back to an
// active value — exactly the still-listed-terminal case the sweep keys off
// (mirrors TestThreadTaskAutoCloserSweep_TerminalStateDoesNotKeepAlive).
func TestThreadTaskAutoCloserSweep_HintsLinkGapOrphan(t *testing.T) {
	d := newMockDaemon()
	channel := "C-sweep-1"
	threadTS := "1000.0001"
	threadID := seedThreadTask(d, channel, threadTS)
	// Agent bound to the thread by channel/ts metadata but with an EMPTY
	// slack_thread_bead_id — the exact linking gap MaybeSchedule can't bridge.
	seedThreadAgent(d, "agent-unlinked", "", channel, threadTS, "done")
	// Keep the terminal witness in the active-status listing (1b-8).
	d.mu.Lock()
	d.beads["agent-unlinked"].Status = "open"
	d.mu.Unlock()

	c := newSweepCloser(d, time.Now())
	c.sweepOnce(context.Background())

	assertHintedOpen(t, d, threadID)
}

// 1b-8 (ka-0000000126): a thread task with NO agent bead at all carries no
// terminal witness proving an agent ever ran for it, so the sweep leaves it
// alone. The positive case is TestSweep_HintsWithTerminalWitness below.
func TestThreadTaskAutoCloserSweep_AgentlessOrphanLeftOpen(t *testing.T) {
	d := newMockDaemon()
	channel := "C-sweep-2"
	threadTS := "2000.0002"
	threadID := seedThreadTask(d, channel, threadTS)
	// Long-idle so only the missing positive evidence (not the idle window)
	// keeps it open.
	d.mu.Lock()
	d.beads[threadID].UpdatedAt = time.Now().Add(-2 * time.Hour)
	d.mu.Unlock()

	c := newSweepCloser(d, time.Now())
	c.sweepOnce(context.Background())

	d.mu.Lock()
	status := d.beads[threadID].Status
	d.mu.Unlock()
	if status != "open" {
		t.Fatalf("expected agentless orphan to stay OPEN (positive-evidence-only), got %q", status)
	}
	if got := d.getClosed(); len(got) != 0 {
		t.Fatalf("expected no close calls for agentless orphan, got %+v", got)
	}
	if got := d.getComments(); len(got) != 0 {
		t.Fatalf("expected no hint for agentless orphan, got %+v", got)
	}
}

// Sweep must NOT hint a thread that still has a live (non-terminal) agent.
func TestThreadTaskAutoCloserSweep_SkipsLiveAgent(t *testing.T) {
	d := newMockDaemon()
	channel := "C-sweep-3"
	threadTS := "3000.0003"
	threadID := seedThreadTask(d, channel, threadTS)
	seedThreadAgent(d, "agent-live", threadID, channel, threadTS, "working")

	c := newSweepCloser(d, time.Now())
	c.sweepOnce(context.Background())

	d.mu.Lock()
	status := d.beads[threadID].Status
	d.mu.Unlock()
	if status != "open" {
		t.Fatalf("expected thread bead to stay open with live agent, got %q", status)
	}
	if got := d.getComments(); len(got) != 0 {
		t.Fatalf("expected no hint with a live agent, got %+v", got)
	}
}

// Sweep respects the idle-grace window: a thread updated within the window is
// left open even when all its agents are terminal.
func TestThreadTaskAutoCloserSweep_RespectsIdleWindow(t *testing.T) {
	d := newMockDaemon()
	channel := "C-sweep-4"
	threadTS := "4000.0004"
	threadID := seedThreadTask(d, channel, threadTS)
	seedThreadAgent(d, "agent-recent", threadID, channel, threadTS, "done")
	// 1b-8: keep the terminal witness listed so the hint turns purely on the
	// idle-window gate, which is what this test exercises.
	d.mu.Lock()
	d.beads["agent-recent"].Status = "open"
	d.mu.Unlock()

	now := time.Now()
	// Thread was touched 5m ago — inside the 30m window.
	d.mu.Lock()
	d.beads[threadID].UpdatedAt = now.Add(-5 * time.Minute)
	d.mu.Unlock()

	c := newSweepCloser(d, now)
	c.sweepOnce(context.Background())

	d.mu.Lock()
	status := d.beads[threadID].Status
	d.mu.Unlock()
	if status != "open" {
		t.Fatalf("expected thread within idle window to stay open, got %q", status)
	}
	if got := d.getComments(); len(got) != 0 {
		t.Fatalf("expected no hint within the idle window, got %+v", got)
	}

	// Advancing past the window lets the next sweep hint it.
	c2 := newSweepCloser(d, now.Add(40*time.Minute))
	c2.sweepOnce(context.Background())
	assertHintedOpen(t, d, threadID)
}

// A bead carrying source:slack but NO slack-thread:* label is not a thread task
// and must be left untouched (defensive: the list filter is broad).
func TestThreadTaskAutoCloserSweep_IgnoresNonThreadBead(t *testing.T) {
	d := newMockDaemon()
	d.mu.Lock()
	d.beads["not-a-thread"] = &beadsapi.BeadDetail{
		ID:     "not-a-thread",
		Title:  "some slack-sourced task",
		Type:   "task",
		Status: "open",
		Labels: []string{"source:slack", "project:gardenio"},
	}
	d.mu.Unlock()

	c := newSweepCloser(d, time.Now())
	c.sweepOnce(context.Background())

	if got := d.getClosed(); len(got) != 0 {
		t.Fatalf("expected non-thread bead untouched, got closes %+v", got)
	}
	if got := d.getComments(); len(got) != 0 {
		t.Fatalf("expected non-thread bead untouched, got comments %+v", got)
	}
}

// A terminal agent_state (done/failed/stopping) on a still-active-status bead
// does not suppress the hint.
func TestThreadTaskAutoCloserSweep_TerminalStateDoesNotKeepAlive(t *testing.T) {
	d := newMockDaemon()
	channel := "C-sweep-5"
	threadTS := "5000.0005"
	threadID := seedThreadTask(d, channel, threadTS)
	// stopping == frozen-awaiting-thaw; terminal for nudge purposes, so the
	// sweep treats it as not-live and hints the parent.
	seedThreadAgent(d, "agent-stopping", threadID, channel, threadTS, "stopping")
	// Force the agent bead into an active status so ListAgentBeads returns it.
	d.mu.Lock()
	d.beads["agent-stopping"].Status = "open"
	d.mu.Unlock()

	c := newSweepCloser(d, time.Now())
	c.sweepOnce(context.Background())

	assertHintedOpen(t, d, threadID)
}

// Run is a no-op on a nil receiver / nil daemon and returns promptly.
func TestThreadTaskAutoCloserSweep_RunNilSafe(t *testing.T) {
	var c *ThreadTaskAutoCloser
	c.Run(context.Background()) // nil receiver

	c2 := &ThreadTaskAutoCloser{}
	c2.Run(context.Background()) // nil daemon
}

// Run fires an immediate first sweep on boot (catching the historical backlog)
// before any ticker interval elapses.
func TestThreadTaskAutoCloserSweep_RunImmediateFirstPass(t *testing.T) {
	d := newMockDaemon()
	channel := "C-sweep-6"
	threadTS := "6000.0006"
	threadID := seedThreadTask(d, channel, threadTS)
	seedThreadAgent(d, "agent-done-6", threadID, channel, threadTS, "done")
	// 1b-8: keep the terminal witness listed so the boot-pass hint has its
	// positive evidence (a "done" agent's bead is otherwise closed and drops
	// out of ListAgentBeads).
	d.mu.Lock()
	d.beads["agent-done-6"].Status = "open"
	d.mu.Unlock()

	c := NewThreadTaskAutoCloser(ThreadTaskAutoCloserConfig{
		Daemon:        d,
		Logger:        slog.Default(),
		Window:        30 * time.Minute,
		SweepInterval: time.Hour, // long enough that only the boot pass runs
	})
	c.nowFunc = func() time.Time { return time.Now() }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.Run(ctx)
		close(done)
	}()

	// Poll for the immediate first-pass hint; cancel as soon as it lands.
	deadline := time.Now().Add(2 * time.Second)
	for len(d.getComments()) == 0 {
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("expected immediate first-pass sweep to hint the orphan")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	assertHintedOpen(t, d, threadID)
}

// DefaultSweepInterval is applied when SweepInterval is unset.
func TestThreadTaskAutoCloserSweep_DefaultInterval(t *testing.T) {
	c := NewThreadTaskAutoCloser(ThreadTaskAutoCloserConfig{Daemon: newMockDaemon()})
	if c.sweepInterval != DefaultThreadTaskSweepInterval {
		t.Errorf("expected default sweep interval %s, got %s",
			DefaultThreadTaskSweepInterval, c.sweepInterval)
	}
}

// --- 1b-8 (ka-0000000126): positive-evidence-only hinting ------------------
//
// The sweep must act ONLY on affirmative proof a bound agent ran for the
// thread (a still-listed terminal-state agent), never on the mere ABSENCE of a
// live agent. These tests pin that and the indeterminate-liveness (empty
// listing) guard.

// TestSweep_NoReapWithoutPositiveEvidence: an open, long-idle thread task with
// no terminal witness is left untouched — no hint, no close.
func TestSweep_NoReapWithoutPositiveEvidence(t *testing.T) {
	d := newMockDaemon()
	channel := "C-1b8-noevidence"
	threadTS := "7000.0001"
	threadID := seedThreadTask(d, channel, threadTS)
	// A live (non-terminal) agent for an UNRELATED thread, so ListAgentBeads
	// is non-empty (bypasses the indeterminate-liveness guard) yet provides
	// NO terminal witness for this thread.
	seedThreadAgent(d, "agent-other-live", "", "C-other", "9.9", "working")
	// Long-idle so only the missing witness keeps it open.
	now := time.Now()
	d.mu.Lock()
	d.beads[threadID].UpdatedAt = now.Add(-2 * time.Hour)
	d.mu.Unlock()

	c := newSweepCloser(d, now)
	c.sweepOnce(context.Background())

	d.mu.Lock()
	status := d.beads[threadID].Status
	d.mu.Unlock()
	if status != "open" {
		t.Fatalf("expected thread with no terminal witness to stay open, got %q", status)
	}
	if got := len(d.getClosed()); got != 0 {
		t.Fatalf("expected no close calls without positive evidence, got %d", got)
	}
	if got := len(d.getComments()); got != 0 {
		t.Fatalf("expected no comment/hint without positive evidence, got %d (%+v)", got, d.getComments())
	}
}

// TestSweep_NoReapOnEmptyAgentListing: an EMPTY ListAgentBeads while open
// thread beads exist is indeterminate liveness (a bridge boot/restart sweep or
// a degraded daemon), NOT proof every agent is gone. The sweep must skip the
// whole pass — no close, no hint — rather than treat absence as a reap signal.
func TestSweep_NoReapOnEmptyAgentListing(t *testing.T) {
	d := newMockDaemon()
	channel := "C-1b8-emptylist"
	threadTS := "7000.0002"
	threadID := seedThreadTask(d, channel, threadTS)
	// No agent beads seeded at all → ListAgentBeads returns empty.
	now := time.Now()
	d.mu.Lock()
	d.beads[threadID].UpdatedAt = now.Add(-2 * time.Hour)
	d.mu.Unlock()

	c := newSweepCloser(d, now)
	c.sweepOnce(context.Background())

	d.mu.Lock()
	status := d.beads[threadID].Status
	d.mu.Unlock()
	if status != "open" {
		t.Fatalf("expected thread to stay open on empty agent listing, got %q", status)
	}
	if got := len(d.getClosed()); got != 0 {
		t.Fatalf("expected no close on empty agent listing, got %d", got)
	}
	if got := len(d.getComments()); got != 0 {
		t.Fatalf("expected no comment on empty agent listing, got %d (%+v)", got, d.getComments())
	}
}

// TestSweep_HintsWithTerminalWitness: WITH positive evidence — an open,
// long-idle thread whose bound agent reached a terminal state and is still
// listed (bead status active) — the sweep hints and leaves the wish open.
func TestSweep_HintsWithTerminalWitness(t *testing.T) {
	d := newMockDaemon()
	channel := "C-1b8-witness"
	threadTS := "7000.0003"
	threadID := seedThreadTask(d, channel, threadTS)
	// Terminal agent bound by channel/ts; force its bead into an active status
	// so it stays in ListAgentBeads as the ran-and-terminated witness.
	seedThreadAgent(d, "agent-terminal-witness", "", channel, threadTS, "done")
	now := time.Now()
	d.mu.Lock()
	d.beads["agent-terminal-witness"].Status = "open"
	d.beads[threadID].UpdatedAt = now.Add(-2 * time.Hour)
	d.mu.Unlock()

	c := newSweepCloser(d, now)
	c.sweepOnce(context.Background())

	assertHintedOpen(t, d, threadID)
}

// TestSweep_NoReapForMidSpawnAgent: a thread whose only bound agent is still
// spawning (non-terminal) is NEITHER live-enough to have finished NOR a
// terminal witness — it must be left open. A mid-spawn agent that has not yet
// registered as live must never be misread as "done and gone" => hint.
func TestSweep_NoReapForMidSpawnAgent(t *testing.T) {
	d := newMockDaemon()
	channel := "C-1b8-midspawn"
	threadTS := "7000.0004"
	threadID := seedThreadTask(d, channel, threadTS)
	// "spawning" is non-terminal → records liveByLabel, not a terminal witness.
	// (seedThreadAgent keeps non-done/failed agents in an active status, so the
	// bead is listed.)
	seedThreadAgent(d, "agent-spawning", "", channel, threadTS, "spawning")
	now := time.Now()
	d.mu.Lock()
	d.beads[threadID].UpdatedAt = now.Add(-2 * time.Hour)
	d.mu.Unlock()

	c := newSweepCloser(d, now)
	c.sweepOnce(context.Background())

	d.mu.Lock()
	status := d.beads[threadID].Status
	d.mu.Unlock()
	if status != "open" {
		t.Fatalf("expected thread with only a mid-spawn agent to stay open, got %q", status)
	}
	if got := len(d.getClosed()); got != 0 {
		t.Fatalf("expected no close for a mid-spawn agent, got %d", got)
	}
	if got := d.getComments(); len(got) != 0 {
		t.Fatalf("expected no hint for a fresh mid-spawn agent, got %+v", got)
	}
}
