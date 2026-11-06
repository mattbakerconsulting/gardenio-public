package bridge

import (
	"context"
	"testing"
	"time"
)

// TestSweep_HintsWithDurableTerminalWitness is the regression for ka-0000006383:
// a finished slack-thread wish whose only agent ran, went done, then had its
// AGENT BEAD CLOSED (dropping out of ListAgentBeads' active-status window) has
// NO in-listing terminal witness — yet the durable thread_agent_terminal_at
// marker stamped on the thread bead at agent-terminal time still authorizes the
// sweep to hint it. Without the durable-witness fix the sweep takes the
// positive-evidence-only branch and the wish is never nudged.
func TestSweep_HintsWithDurableTerminalWitness(t *testing.T) {
	d := newMockDaemon()
	channel := "C-durable-witness"
	threadTS := "8000.0001"
	threadID := seedThreadTask(d, channel, threadTS)

	// The wish's only agent already RAN and its agent bead is CLOSED and gone —
	// modelled by seeding no agent bead for this thread at all, so ListAgentBeads
	// yields NO terminal witness for it. Stamp the durable witness on the thread
	// bead, exactly as MaybeSchedule would have at the clean-close moment.
	d.mu.Lock()
	d.beads[threadID].Fields[threadTerminalWitnessField] = time.Now().Add(-90 * time.Minute).UTC().Format(time.RFC3339)
	d.beads[threadID].Fields[threadTerminalWitnessAgentField] = "agent-gone"
	d.mu.Unlock()

	// A live agent for an UNRELATED thread keeps ListAgentBeads non-empty so the
	// indeterminate-liveness guard does not short-circuit the pass.
	seedThreadAgent(d, "agent-other-live", "", "C-other", "9.9", "working")

	now := time.Now()
	d.mu.Lock()
	d.beads[threadID].UpdatedAt = now.Add(-2 * time.Hour) // idle past the window
	d.mu.Unlock()

	c := newSweepCloser(d, now)
	c.sweepOnce(context.Background())

	assertHintedOpen(t, d, threadID)
}

// TestSweep_NoDurableWitnessLeavesWishOpen is the negative control: an
// otherwise-identical long-idle wish with NEITHER an in-listing witness NOR the
// durable marker is left untouched. This proves the hint in the test above is
// driven by the durable marker specifically, not by idle-time alone.
func TestSweep_NoDurableWitnessLeavesWishOpen(t *testing.T) {
	d := newMockDaemon()
	channel := "C-durable-nowitness"
	threadTS := "8000.0002"
	threadID := seedThreadTask(d, channel, threadTS)

	seedThreadAgent(d, "agent-other-live", "", "C-other", "9.9", "working")

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
		t.Fatalf("expected wish with no witness (in-listing or durable) to stay open, got %q", status)
	}
	if got := len(d.getClosed()); got != 0 {
		t.Fatalf("expected no close without any witness, got %d", got)
	}
	if got := len(d.getComments()); got != 0 {
		t.Fatalf("expected no hint without any witness, got %d", got)
	}
}

// TestMaybeSchedule_StampsDurableTerminalWitnessOnDone: a clean (done) terminal
// agent close stamps the durable thread_agent_terminal_at marker (+ the agent
// provenance) on the thread bead synchronously, while the agent binding is
// fresh — before the agent bead is reaped out of ListAgentBeads.
func TestMaybeSchedule_StampsDurableTerminalWitnessOnDone(t *testing.T) {
	d := newMockDaemon()
	channel := "C-stamp-done"
	threadTS := "8100.0001"
	threadID := seedThreadTask(d, channel, threadTS)

	c, _ := newTestAutoCloser(d)
	c.MaybeSchedule(agentDoneEvent("agent-done-stamp", threadID, channel, threadTS))

	d.mu.Lock()
	ts := d.beads[threadID].Fields[threadTerminalWitnessField]
	who := d.beads[threadID].Fields[threadTerminalWitnessAgentField]
	d.mu.Unlock()
	if ts == "" {
		t.Fatalf("expected durable terminal-witness timestamp stamped on done close, got empty")
	}
	if _, err := time.Parse(time.RFC3339, ts); err != nil {
		t.Errorf("expected RFC3339 timestamp, got %q (%v)", ts, err)
	}
	if who != "agent-done-stamp" {
		t.Errorf("expected witness agent provenance agent-done-stamp, got %q", who)
	}
}

// TestMaybeSchedule_NoDurableWitnessOnFailed: a FAILED terminal close must NOT
// stamp the durable clean-witness marker — a crash drives the needs-triage split
// and must never look like a ran-and-finished witness that authorizes an
// auto-close.
func TestMaybeSchedule_NoDurableWitnessOnFailed(t *testing.T) {
	d := newMockDaemon()
	channel := "C-stamp-failed"
	threadTS := "8100.0002"
	threadID := seedThreadTask(d, channel, threadTS)

	c, _ := newTestAutoCloser(d)
	c.MaybeSchedule(BeadEvent{
		ID:     "agent-failed-stamp",
		Type:   "agent",
		Status: "closed",
		Fields: map[string]string{
			"agent_state":          "failed",
			"slack_thread_bead_id": threadID,
			"slack_thread_channel": channel,
			"slack_thread_ts":      threadTS,
			"spawn_source":         "slack-thread",
		},
	})

	d.mu.Lock()
	ts := d.beads[threadID].Fields[threadTerminalWitnessField]
	d.mu.Unlock()
	if ts != "" {
		t.Fatalf("expected NO durable terminal witness on failed close, got %q", ts)
	}
}
