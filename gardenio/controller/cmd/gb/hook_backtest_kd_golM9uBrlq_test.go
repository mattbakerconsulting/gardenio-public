package main

import (
	"context"
	"os"
	"testing"
	"time"
)

// BACKTEST — reproduce the death of monorepo-fsmtriageclassify-reflow-mode-iqr
// (agent bead gd-0bd39AIZR4, task gd-xzvAWFt65M) and prove the CLAIM GUARD
// (ka-0000006454, decision ka-0000005194 = "claim-guard") neutralizes it.
//
// This is the companion to the thread-1784851806 backtest, and deliberately
// covers the case that one CANNOT: that agent was genuinely parked on an open
// decision, so the yield-marker fast-path (ka-0000002844) already saves it. This
// agent was NOT parked — it filed zero decisions and held no yield marker — so
// it fell straight through the yield fast-path into the cooldown close. That is
// the residual population the claim-guard exists for, and at 34 deaths/7d it was
// the #2 involuntary-death vector fleet-wide (~8x the stuck-watchdog that was
// disabled in !6378).
//
// OBSERVED DEATH (from the agent bead + Slack death alert):
//
//	role=crew, project=monorepo, dispatched to REVIEW-AND-MERGE monorepo!2008.
//	SIGTERM (exit 143) at 2026-07-31T01:35:47Z, 11 min into the session, after
//	39 requests / 38 tool calls / $3.49 — demonstrably mid-work, not idle. It
//	held claimed in-progress task gd-xzvAWFt65M, which the close then reopened
//	and unassigned. MR !2008 was still open: the whole review pass was wasted.
//
// The mechanism is NOT a hang and NOT a park — it is simply two quiet turn-ends
// inside one cooldown window, which is ordinary behavior for a working agent.

// TestBacktest_kd_golM9uBrlq_NotSavedByYieldFastPath pins the premise that makes
// this a distinct bug rather than a duplicate of ka-0000002844: with no yield
// marker and no open decision, the parked fast-path does NOT fire, so the agent
// reaches the cooldown branch. If a future change made the yield path cover this
// population, this test tells the reader the claim-guard's justification moved.
func TestBacktest_kd_golM9uBrlq_NotSavedByYieldFastPath(t *testing.T) {
	withStopGateScratchFiles(t)
	t.Setenv("KD_AGENT_ID", "gd-0bd39AIZR4")

	// The real agent left no yield marker — it never called `gb yield`.
	if _, err := os.Stat(stopGateYieldMarkerFile); err == nil {
		t.Fatal("precondition: this agent held no yield marker")
	}
	// And it owned no open decision (verified on the bead: zero decisions filed).
	setupEmptyDecisionDaemon(t)
	if stopGateAgentParkedOnOpenDecision(context.Background()) {
		t.Fatal("precondition: this agent was NOT parked on a decision — the yield fast-path cannot save it")
	}
}

// TestBacktest_kd_golM9uBrlq_ClaimGuardNeutralizesDeath is the "after" half: at
// the exact fatal state (in cooldown, below the escape hatch, holding a live
// claim) the claim guard reports work in flight, so the hook exits 0 WITHOUT
// closing the bead and the reproduced death does not occur.
//
// Verified to FAIL against the pre-fix code: before stopGateClaimedTask existed,
// the cooldown branch closed the bead unconditionally.
func TestBacktest_kd_golM9uBrlq_ClaimGuardNeutralizesDeath(t *testing.T) {
	withStopGateScratchFiles(t)
	t.Setenv("STOP_GATE_COOLDOWN_SECS", "")

	// Reconstruct the fatal state: a block on record inside the active window.
	// count=2 => 60s window; 20s elapsed => in cooldown. Pre-fix, the hook falls
	// straight into closeAgentBeadOnExit("cooldown").
	stampBlockElapsed(t, 20*time.Second, 2)
	if !stopGateInCooldown() {
		t.Fatal("precondition: reconstructed state must be in cooldown (the pre-fix death condition)")
	}
	// The death was the COOLDOWN path, not the escape hatch — count=2 < 5. This
	// matters: the escape hatch is the cost-sink guard we must NOT weaken, and
	// it was nowhere near firing.
	if stopGateEscapeHatch() {
		t.Fatal("precondition: escape hatch must not be armed — this death came from the cooldown path")
	}

	// The agent held claimed in-progress task gd-xzvAWFt65M at the moment of death.
	withActor(t, "monorepo-fsmtriageclassify-reflow-mode-iqr")
	setupClaimedTaskDaemon(t, "gd-xzvAWFt65M", "in_progress")

	// THE ASSERTION THAT MATTERS: the cooldown branch must choose to SPARE.
	// This is the kill/spare decision itself, not merely the claim lookup — a
	// test of the lookup alone passes against the pre-fix code and proves
	// nothing, because pre-fix the branch never consulted it.
	sparedTask, spare := stopGateCooldownSparesAgent(context.Background())
	if !spare {
		t.Fatal("FIX REGRESSED: cooldown branch chose to CLOSE the bead of an agent holding a live claim — the involuntary death of gd-0bd39AIZR4 recurs")
	}
	if sparedTask != "gd-xzvAWFt65M" {
		t.Errorf("spared task = %q, want gd-xzvAWFt65M (the claim held at the moment of death)", sparedTask)
	}
}

// TestBacktest_kd_golM9uBrlq_FinishedAgentStillCloses is the control that keeps
// the guard from becoming a blanket "never close". An agent that reached the
// same cooldown state having RELEASED its claim is a genuinely-finished agent,
// and must still be closed so its pod is torn down rather than lingering.
//
// This is what makes the claim a sound proxy for work-in-flight: `gb stop`
// refuses to despawn while a claim is held (checkClaimedWork, stop.go), so a
// cleanly-finished agent never arrives here still holding one.
func TestBacktest_kd_golM9uBrlq_FinishedAgentStillCloses(t *testing.T) {
	withStopGateScratchFiles(t)
	t.Setenv("STOP_GATE_COOLDOWN_SECS", "")
	stampBlockElapsed(t, 20*time.Second, 2)
	if !stopGateInCooldown() {
		t.Fatal("precondition: reconstructed state must be in cooldown")
	}

	withActor(t, "monorepo-fsmtriageclassify-reflow-mode-iqr")
	setupClaimedTaskDaemon(t, "", "") // claim released

	if _, spare := stopGateCooldownSparesAgent(context.Background()); spare {
		t.Fatal("cooldown branch spared an agent holding NO claim — the guard must not become a blanket never-close, or a finished agent's pod lingers")
	}
}
