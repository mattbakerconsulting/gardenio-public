package bridge

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// --- 1b-9 (ka-0000007017): SpawnStallGrace + self-exclusion + deep-subtree ---
//
// These tests pin the three refinements 1b-9 adds to the backfill sweep's
// LIVENESS/suppression side, layered on top of 1b-8's positive-evidence gate:
//
//   - SpawnStallGrace: a non-terminal member wedged past the grace stops
//     suppressing (it demotes to a witness) — a stalled spawn nudges.
//   - self-exclusion: a member never counts as evidence about its own wish head
//     (structural — the head is a task, never in the agent listing).
//   - deep-subtree: a fresh non-terminal DERIVATIVE (bound only by
//     fields.thread_id, no channel/ts) suppresses the wish's hint.

// seedThreadDerivative seeds an agent bead that is a DEEP DERIVATIVE of a wish:
// it carries fields.thread_id pointing at the head but NO slack_thread_channel/
// ts binding (the gardener-chain / restart-spawn shape the sweep's label index
// cannot see). updatedAt drives the spawn-stall-grace check.
func seedThreadDerivative(d *mockDaemon, id, headID, state string, updatedAt time.Time) {
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
		UpdatedAt: updatedAt,
		Fields: map[string]string{
			"agent":       id,
			"agent_state": state,
			"thread_id":   headID,
		},
	}
}

func newStallCloser(d *mockDaemon, now time.Time, grace time.Duration) *ThreadTaskAutoCloser {
	c := NewThreadTaskAutoCloser(ThreadTaskAutoCloserConfig{
		Daemon:          d,
		Logger:          slog.Default(),
		Window:          30 * time.Minute,
		SpawnStallGrace: grace,
	})
	c.nowFunc = func() time.Time { return now }
	return c
}

// DefaultSpawnStallGrace is applied when SpawnStallGrace is unset.
func TestThreadTaskAutoCloser_DefaultSpawnStallGrace(t *testing.T) {
	c := NewThreadTaskAutoCloser(ThreadTaskAutoCloserConfig{Daemon: newMockDaemon()})
	if c.spawnStallGrace != DefaultSpawnStallGrace {
		t.Errorf("expected default spawn-stall grace %s, got %s",
			DefaultSpawnStallGrace, c.spawnStallGrace)
	}
}

// TestSweep_FreshSpawningAgentSuppresses: a non-terminal agent whose updated_at
// is INSIDE the spawn-stall grace is a live member — it suppresses the hint,
// exactly as before 1b-9 (the mid-spawn guard from
// TestSweep_NoReapForMidSpawnAgent still holds for a FRESH agent).
func TestSweep_FreshSpawningAgentSuppresses(t *testing.T) {
	d := newMockDaemon()
	channel := "C-1b9-fresh"
	threadTS := "9100.0001"
	threadID := seedThreadTask(d, channel, threadTS)
	now := time.Now()
	// Spawning agent updated 5m ago — well inside the 30m grace.
	seedThreadAgent(d, "agent-fresh-spawn", "", channel, threadTS, "spawning")
	d.mu.Lock()
	d.beads["agent-fresh-spawn"].UpdatedAt = now.Add(-5 * time.Minute)
	d.beads[threadID].UpdatedAt = now.Add(-2 * time.Hour)
	d.mu.Unlock()

	c := newStallCloser(d, now, 30*time.Minute)
	c.sweepOnce(context.Background())

	d.mu.Lock()
	status := d.beads[threadID].Status
	d.mu.Unlock()
	if status != "open" {
		t.Fatalf("expected wish with a FRESH spawning member to stay open, got %q", status)
	}
	if got := len(d.getClosed()); got != 0 {
		t.Fatalf("expected no close with a fresh live member, got %d", got)
	}
	if got := len(d.getComments()); got != 0 {
		t.Fatalf("expected no hint with a fresh live member, got %d", got)
	}
}

// TestSweep_StalledSpawnHints: the headline 1b-9 case — a non-terminal agent
// wedged 45m past its updated_at (> 30m grace) stops suppressing and becomes a
// witness that an agent was dispatched, so the sweep nudges and the wish stays
// open.
func TestSweep_StalledSpawnHints(t *testing.T) {
	now := time.Now()
	d := newMockDaemon()
	channel := "C-1b9-stall"
	threadTS := "9200.0001"
	threadID := seedThreadTask(d, channel, threadTS)
	seedThreadAgent(d, "agent-stalled", "", channel, threadTS, "spawning")
	d.mu.Lock()
	d.beads["agent-stalled"].UpdatedAt = now.Add(-45 * time.Minute)
	d.beads[threadID].UpdatedAt = now.Add(-2 * time.Hour)
	d.mu.Unlock()

	c := newStallCloser(d, now, 30*time.Minute)
	c.sweepOnce(context.Background())

	assertHintedOpen(t, d, threadID)
}

// TestSweep_TerminalWitnessHintsAlongsideStall: a wish with a clean terminal
// witness AND a stalled agent is hinted once.
func TestSweep_TerminalWitnessHintsAlongsideStall(t *testing.T) {
	d := newMockDaemon()
	channel := "C-1b9-mixed"
	threadTS := "9300.0001"
	threadID := seedThreadTask(d, channel, threadTS)
	now := time.Now()
	seedThreadAgent(d, "agent-done-mixed", "", channel, threadTS, "done")
	seedThreadAgent(d, "agent-stall-mixed", "", channel, threadTS, "spawning")
	d.mu.Lock()
	d.beads["agent-done-mixed"].Status = "open" // keep terminal witness listed
	d.beads["agent-stall-mixed"].UpdatedAt = now.Add(-45 * time.Minute)
	d.beads[threadID].UpdatedAt = now.Add(-2 * time.Hour)
	d.mu.Unlock()

	c := newStallCloser(d, now, 30*time.Minute)
	c.sweepOnce(context.Background())

	assertHintedOpen(t, d, threadID)
}

// TestSweep_DeepDerivativeSuppresses: deep-subtree liveness — a FRESH
// non-terminal DERIVATIVE (bound only by fields.thread_id, no channel/ts)
// suppresses the wish's hint even though the label index cannot see it and the
// wish carries an otherwise-hintable terminal witness.
func TestSweep_DeepDerivativeSuppresses(t *testing.T) {
	d := newMockDaemon()
	channel := "C-1b9-deriv"
	threadTS := "9400.0001"
	threadID := seedThreadTask(d, channel, threadTS)
	now := time.Now()
	// A terminal witness bound by channel/ts (would normally authorize a hint)…
	seedThreadAgent(d, "agent-done-deriv", "", channel, threadTS, "done")
	d.mu.Lock()
	d.beads["agent-done-deriv"].Status = "open"
	d.beads[threadID].UpdatedAt = now.Add(-2 * time.Hour)
	d.mu.Unlock()
	// …but a FRESH working derivative points at the head via thread_id only.
	seedThreadDerivative(d, "agent-deriv-live", threadID, "working", now.Add(-3*time.Minute))

	c := newStallCloser(d, now, 30*time.Minute)
	c.sweepOnce(context.Background())

	d.mu.Lock()
	status := d.beads[threadID].Status
	d.mu.Unlock()
	if status != "open" {
		t.Fatalf("expected wish with a live deep derivative to stay open, got %q", status)
	}
	if got := len(d.getClosed()); got != 0 {
		t.Fatalf("expected no close with a live derivative, got %d", got)
	}
	if got := len(d.getComments()); got != 0 {
		t.Fatalf("a running derivative must suppress the hint, got %d comments", got)
	}
}

// TestSweep_StalledDerivativeStopsSuppressing: the spawn-stall grace applies to
// derivatives too — a derivative wedged past the grace no longer suppresses, so
// a wish whose only members are a terminal witness + a stalled derivative is
// hinted on the terminal witness.
func TestSweep_StalledDerivativeStopsSuppressing(t *testing.T) {
	d := newMockDaemon()
	channel := "C-1b9-deriv-stall"
	threadTS := "9500.0001"
	threadID := seedThreadTask(d, channel, threadTS)
	now := time.Now()
	seedThreadAgent(d, "agent-done-ds", "", channel, threadTS, "done")
	d.mu.Lock()
	d.beads["agent-done-ds"].Status = "open"
	d.beads[threadID].UpdatedAt = now.Add(-2 * time.Hour)
	d.mu.Unlock()
	// Derivative wedged 90m ago (> 30m grace) → no longer a live suppressor.
	seedThreadDerivative(d, "agent-deriv-stalled", threadID, "working", now.Add(-90*time.Minute))

	c := newStallCloser(d, now, 30*time.Minute)
	c.sweepOnce(context.Background())

	assertHintedOpen(t, d, threadID)
}

// TestSweep_HeadOnlyThreadStillProtectedWithoutEvidence: the 1b-8
// positive-evidence gate is preserved — a wish with NO member at all (head-only,
// agentless) and no stall/terminal witness is left untouched. 1b-9 refines
// liveness; it does not reopen the negative-evidence action 1b-8 removed. (A non-empty listing
// via an unrelated live agent bypasses the empty-listing guard.)
func TestSweep_HeadOnlyThreadStillProtectedWithoutEvidence(t *testing.T) {
	d := newMockDaemon()
	channel := "C-1b9-headonly"
	threadTS := "9600.0001"
	threadID := seedThreadTask(d, channel, threadTS)
	now := time.Now()
	// Unrelated live agent so the listing is non-empty but gives THIS wish no
	// evidence of its own.
	seedThreadAgent(d, "agent-unrelated", "", "C-other", "1.1", "working")
	d.mu.Lock()
	d.beads["agent-unrelated"].UpdatedAt = now
	d.beads[threadID].UpdatedAt = now.Add(-2 * time.Hour)
	d.mu.Unlock()

	c := newStallCloser(d, now, 30*time.Minute)
	c.sweepOnce(context.Background())

	d.mu.Lock()
	status := d.beads[threadID].Status
	d.mu.Unlock()
	if status != "open" {
		t.Fatalf("expected head-only agentless wish to stay open (positive-evidence-only preserved), got %q", status)
	}
	if got := len(d.getClosed()) + len(d.getComments()); got != 0 {
		t.Fatalf("expected no action on an agentless wish, got %d close+comment calls", got)
	}
}

// TestSweep_DerivativeSelfExclusion: self-exclusion generalized to the sweep —
// a member never counts as evidence about its OWN wish head. A derivative's
// thread_id points at the head; the head (a task) is never in the agent listing,
// so it can neither suppress nor witness itself. Here a terminal DERIVATIVE
// bound only by thread_id is the sole evidence: it authorizes the wish's hint
// without being mistaken for a live self-suppressor.
func TestSweep_DerivativeTerminalWitnessHints(t *testing.T) {
	d := newMockDaemon()
	threadID := seedThreadTask(d, "C-1b9-selfx", "9700.0001")
	now := time.Now()
	d.mu.Lock()
	d.beads[threadID].UpdatedAt = now.Add(-2 * time.Hour)
	d.mu.Unlock()
	// A terminal derivative bound ONLY by thread_id (deep-subtree witness), still
	// listed. No channel/ts binding at all.
	seedThreadDerivative(d, "agent-deriv-done", threadID, "done", now.Add(-90*time.Minute))
	d.mu.Lock()
	d.beads["agent-deriv-done"].Status = "open" // keep it listed as the witness
	d.mu.Unlock()

	c := newStallCloser(d, now, 30*time.Minute)
	c.sweepOnce(context.Background())

	assertHintedOpen(t, d, threadID)
}

// TestBuildMemberEvidence_Classification is a focused unit test on the evidence
// rollup: fresh non-terminal → live; stalled non-terminal → stalledWitness;
// terminal → terminalWitness; and the label + thread_id surfaces both resolve.
func TestBuildMemberEvidence_Classification(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	c := NewThreadTaskAutoCloser(ThreadTaskAutoCloserConfig{
		Daemon:          newMockDaemon(),
		SpawnStallGrace: 30 * time.Minute,
	})
	label := threadBeadLabel("C-ev", "1.2")
	head := "thread-head-xyz"

	agents := []beadsapi.AgentBead{
		// fresh live, bound by channel/ts
		{ID: "a1", AgentState: "working", UpdatedAt: now.Add(-1 * time.Minute),
			Metadata: map[string]string{"slack_thread_channel": "C-ev", "slack_thread_ts": "1.2"}},
		// stalled, bound by thread_id
		{ID: "a2", AgentState: "spawning", UpdatedAt: now.Add(-40 * time.Minute),
			Metadata: map[string]string{"thread_id": head}},
		// terminal witness, bound by both surfaces
		{ID: "a3", AgentState: "done", UpdatedAt: now.Add(-2 * time.Minute),
			Metadata: map[string]string{"slack_thread_channel": "C-ev", "slack_thread_ts": "1.2", "thread_id": head}},
		// unbound → ignored
		{ID: "a4", AgentState: "working", UpdatedAt: now, Metadata: map[string]string{}},
	}

	ev := c.buildMemberEvidence(agents, now)

	if v := ev.forWish(label, ""); !v.live || !v.terminalWitness {
		t.Errorf("label surface: expected live+terminalWitness, got %+v", v)
	}
	if v := ev.forWish("", head); !v.stalledWitness || !v.terminalWitness {
		t.Errorf("thread_id surface: expected stalled+terminalWitness, got %+v", v)
	}
	// a4 (unbound) must not create any key.
	if ev.live[""] || ev.stalledWitness[""] || ev.terminalWitness[""] {
		t.Errorf("unbound agent must not register any evidence key, got %+v", ev)
	}
}
