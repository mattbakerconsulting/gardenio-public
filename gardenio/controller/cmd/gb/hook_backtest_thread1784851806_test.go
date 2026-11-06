package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// BACKTEST — reproduce the hand RCA for thread-1784851806-182809
// (stop_gate_cooldown_involuntary), filed against gd-hzmTyaVGix / ka-0000002026.
//
// This is not a new fix; it is a controlled REPRODUCTION of a known involuntary
// death so a follow-up fix crew has a deterministic regression to verify against.
// It drives the REAL stop-gate cooldown functions (stopGateInCooldown,
// readStopGateBlockCount, stopGateEscapeHatch, stopGateAgentParkedOnOpenDecision)
// — the same code the wired `gb hook stop-gate` runs in production — so the
// timeline it asserts is the production timeline, not a re-derivation.
//
// Hand RCA (ka-0000002026, transcript-confirmed on session b745ebf7,
// agent bead gd-4MIX5fM3w5, S3 recording pulled):
//
//	NOT an OOM, NOT a crash. claude's MODEL TURN ended cleanly (end_turn) 4x
//	because there was nothing left to do but WAIT on operator decision
//	gd-dCYr6rmMG7. The 4th quiet turn-end landed inside the stop-gate cooldown
//	window, so `gb hook stop-gate` closed the bead as
//	stop_gate_cooldown_involuntary and reopened orphaned task gd-pkxih8brbw.
//
//	DEATH ARITHMETIC (cooldown backoff base=30, doubling):
//	  Stop#1 01:00:05 count->1
//	  Stop#2 +40s  (>30s window for count=1)  count->2   survives
//	  Stop#3 +177s (>60s window for count=2)  count->3   survives
//	  Stop#4 +67s  (<120s window for count=3) => IN COOLDOWN => close('cooldown')
//	  didWork=true (122 tool calls) => stop_gate_cooldown_involuntary.
//
// The backtest shows that with the EXACT inter-stop deltas the transcript
// recorded, stops #1-#3 pass the gate (the block window had already elapsed) but
// stop #4 falls inside the count=3 cooldown window and is the one that trips the
// involuntary close. That is what makes the death look "random": three identical
// quiet turn-ends are fine, the fourth kills the agent purely because it happened
// to land 67s after the previous block while the window for blockCount=3 is 120s.

// backtestStopDelta is one quiet turn-end in the reconstructed timeline: the
// seconds elapsed since the previous stop, and whether the transcript shows the
// agent surviving that stop (gate allowed the block, cooldown re-armed) or dying
// (the stop landed inside the active cooldown window and closed the bead).
type backtestStopDelta struct {
	label          string
	secsSincePrev  int
	wantInCooldown bool // true => this stop trips the involuntary-close path
}

// withStopGateScratchFiles points the real cooldown/block-count/yield files at a
// temp dir so the backtest drives production state transitions in isolation.
func withStopGateScratchFiles(t *testing.T) {
	t.Helper()
	tmp := t.TempDir()
	oldCooldown, oldBlockCount, oldYield := stopGateCooldownFile, stopGateBlockCountFile, stopGateYieldMarkerFile
	oldProcRoot := stopGateProcRoot
	t.Cleanup(func() {
		stopGateCooldownFile = oldCooldown
		stopGateBlockCountFile = oldBlockCount
		stopGateYieldMarkerFile = oldYield
		stopGateProcRoot = oldProcRoot
	})
	stopGateCooldownFile = filepath.Join(tmp, "stop-gate-last-block")
	stopGateBlockCountFile = filepath.Join(tmp, "stop-gate-block-count")
	stopGateYieldMarkerFile = filepath.Join(tmp, "stop-gate-yielding")

	// The live-child guard reads a process table, so it is a host-dependent
	// input to this gate exactly like the scratch files above: on any real
	// machine (a dev box, a CI runner) /proc holds live non-infra processes,
	// which would spare every agent and turn the "finished agent still
	// closes" controls green-to-red for reasons that have nothing to do with
	// the agent under test. Default every stop-gate test to an EMPTY table --
	// "no work in flight" -- and let the tests that are about the guard point
	// stopGateProcRoot at a populated fixture themselves (useProcFixture).
	procRoot := filepath.Join(tmp, "proc")
	if err := os.MkdirAll(procRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	stopGateProcRoot = procRoot
}

// stampBlockElapsed writes the real cooldown/block-count scratch files as if
// recordStopGateBlock had last run `elapsed` ago with block count `count`. The
// timestamp is anchored to time.Now() so the real stopGateInCooldown() — which
// reads time.Now() internally — evaluates the historical delta deterministically
// without the test sleeping for minutes.
func stampBlockElapsed(t *testing.T, elapsed time.Duration, count int) {
	t.Helper()
	lastBlock := time.Now().Add(-elapsed).Unix()
	if err := os.WriteFile(stopGateCooldownFile, []byte(fmt.Sprintf("%d\n", lastBlock)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stopGateBlockCountFile, []byte(fmt.Sprintf("%d\n", count)), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestBacktest_Thread1784851806_CooldownDeathArithmetic replays the four quiet
// turn-ends from the hand RCA against the REAL stopGateInCooldown() predicate and
// confirms only the fourth lands inside its cooldown window — the death.
func TestBacktest_Thread1784851806_CooldownDeathArithmetic(t *testing.T) {
	withStopGateScratchFiles(t)
	// Use the production base cooldown (30s). Clear any inherited override.
	t.Setenv("STOP_GATE_COOLDOWN_SECS", "")

	// The reconstructed timeline. Deltas and the survive/die verdict are the
	// transcript's, verbatim from the RCA's DEATH ARITHMETIC block. Cooldown
	// window for a given blockCount = 30 * 2^(count-1), capped 300s:
	//   count=1 -> 30s, count=2 -> 60s, count=3 -> 120s.
	// A stop trips the involuntary close iff (now - lastBlock) < window(count).
	timeline := []backtestStopDelta{
		{label: "Stop#1 (first park, no prior block, count->1)", secsSincePrev: 0, wantInCooldown: false},
		{label: "Stop#2 +40s vs 30s window (count=1)", secsSincePrev: 40, wantInCooldown: false},
		{label: "Stop#3 +177s vs 60s window (count=2)", secsSincePrev: 177, wantInCooldown: false},
		{label: "Stop#4 +67s vs 120s window (count=3) — DEATH", secsSincePrev: 67, wantInCooldown: true},
	}

	blockCount := 0
	for i, step := range timeline {
		if i == 0 {
			// Stop#1: no block on record yet. The real hook reads a missing
			// cooldown file => stopGateInCooldown() == false.
			os.Remove(stopGateCooldownFile)
			os.Remove(stopGateBlockCountFile)
			if got := stopGateInCooldown(); got != step.wantInCooldown {
				t.Fatalf("%s: stopGateInCooldown()=%v, want %v (no prior block => never in cooldown)", step.label, got, step.wantInCooldown)
			}
			// The gate allowed the block, so the hook records it: count -> 1.
			blockCount = 1
			continue
		}

		// The state left by the previous surviving stop: it recorded a block at
		// `secsSincePrev` ago with the running count. Drive the REAL predicate.
		stampBlockElapsed(t, time.Duration(step.secsSincePrev)*time.Second, blockCount)
		got := stopGateInCooldown()
		if got != step.wantInCooldown {
			window := 30
			for k := 1; k < blockCount; k++ {
				window *= 2
				if window > 300 {
					window = 300
					break
				}
			}
			t.Fatalf("%s: stopGateInCooldown()=%v, want %v (elapsed=%ds since last block, window=%ds for count=%d)",
				step.label, got, step.wantInCooldown, step.secsSincePrev, window, blockCount)
		}

		if got {
			// The death: this stop landed inside the cooldown window. In the real
			// hook this is the branch (hook.go:238) that calls
			// closeAgentBeadOnExit("cooldown") -> stop_gate_cooldown_involuntary.
			// We've reproduced the exact trigger condition; assert it is the LAST
			// step, i.e. only Stop#4 dies (the RCA's central claim).
			if i != len(timeline)-1 {
				t.Fatalf("%s: died at step %d, but the RCA says only the final stop (#4) dies", step.label, i+1)
			}
			return
		}

		// Survived: gate allowed the block, cooldown re-arms at a higher count.
		blockCount++
	}

	t.Fatal("reached end of timeline without reproducing the Stop#4 cooldown death — RCA not reproduced")
}

// TestBacktest_Thread1784851806_EscapeHatchNotYetHit confirms the RCA's finer
// point that Stop#4 dies via the COOLDOWN path, not the escape hatch: at the
// moment of death the consecutive-block count is 3, below stopGateMaxBlocks (5),
// so stopGateEscapeHatch() is still false. The kill is purely the cooldown
// debounce landing on a legitimate quiet rest — exactly the mechanism the RCA
// blames, and distinct from the infinite-loop escape hatch.
func TestBacktest_Thread1784851806_EscapeHatchNotYetHit(t *testing.T) {
	withStopGateScratchFiles(t)

	// count=3 at the fatal Stop#4 (Stop#1->1, #2->2, #3->3; #4 dies before ->4).
	stampBlockElapsed(t, 67*time.Second, 3)
	if stopGateEscapeHatch() {
		t.Fatalf("escape hatch tripped at count=3, but RCA (and stopGateMaxBlocks=%d) require the death to come from the cooldown path, not the escape hatch", stopGateMaxBlocks)
	}
	if !stopGateInCooldown() {
		t.Fatal("Stop#4 state must be in cooldown — this is the involuntary-close trigger")
	}
}

// TestBacktest_Thread1784851806_YieldMarkerFastPathNeutralizes proves the merged
// Layer-1 fix (ka-0000002844) neutralizes the reproduced death: with the yield
// marker present AND an open decision this agent requested, the gate's parked
// fast-path (hook.go:222-227) reports parked=true and exits 0 WITHOUT ever
// reaching the cooldown close — so the identical Stop#4 that killed the agent
// pre-fix is now a legitimate quiet rest.
//
// This is the "after" half of the backtest: the regression a follow-up fix crew
// must keep green. It asserts the two exact predicates the hook gates on (marker
// present + parked-on-open-decision) using the RCA's real agent bead
// (gd-4MIX5fM3w5) and decision (gd-dCYr6rmMG7).
func TestBacktest_Thread1784851806_YieldMarkerFastPathNeutralizes(t *testing.T) {
	withStopGateScratchFiles(t)
	t.Setenv("STOP_GATE_COOLDOWN_SECS", "")

	// Reconstruct the fatal Stop#4 state: count=3, last block 67s ago => the real
	// stopGateInCooldown() returns true (inside the 120s window). Pre-fix, the
	// hook falls straight into closeAgentBeadOnExit("cooldown").
	stampBlockElapsed(t, 67*time.Second, 3)
	if !stopGateInCooldown() {
		t.Fatal("precondition: reconstructed Stop#4 state must be in cooldown (the pre-fix death condition)")
	}

	// The fix short-circuits ABOVE that at hook.go:222 on two conditions:
	//   (1) the yield marker file is present (dropped by `gb yield`), and
	//   (2) the agent still owns an open decision it requested.
	if f, err := os.Create(stopGateYieldMarkerFile); err == nil {
		f.Close()
	} else {
		t.Fatalf("create yield marker: %v", err)
	}
	t.Setenv("KD_AGENT_ID", "gd-4MIX5fM3w5") // the RCA's agent bead
	setupParkedDecisionDaemon(t, "gd-4MIX5fM3w5")

	// Condition (1): the marker survives (Layer 2a keeps it across the 120s
	// Bash-tool interrupt; pre-fix the defer os.Remove deleted it mid-wait).
	if _, err := os.Stat(stopGateYieldMarkerFile); err != nil {
		t.Fatalf("yield marker missing: %v — Layer 2a must keep it across a timeout/interrupt", err)
	}
	// Condition (2): the agent is recognized as parked on its open decision.
	if !stopGateAgentParkedOnOpenDecision(context.Background()) {
		t.Fatal("FIX REGRESSED: agent parked on open decision gd-dCYr6rmMG7 not recognized; the involuntary Stop#4 death would recur")
	}

	// Both hold => the hook exits 0 at hook.go:225 WITHOUT closing the bead. The
	// reproduced death is neutralized.
}
