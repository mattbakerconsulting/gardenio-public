package main

// Unit tests for desiredSoftFailoverState — the pure decision function that
// drives the `soft_failover_state` bead field reconciliation.
//
// Regression coverage for ka-0000004575: after a controller restart while an
// account was in `warming`, the previous in-memory `lastWarming` map started
// empty, so the cooled transition could never fire and the bead stayed
// "warming" forever. The new logic reads the bead's persisted state, so the
// fresh process can still observe `wasWarming=true` and reconcile correctly.

import "testing"

func TestDesiredSoftFailoverState_StuckWarmingRecoversAfterRestart(t *testing.T) {
	// Scenario from the bug: bead was left in "warming" by a prior controller
	// process; the new process probes and sees utilization well below the
	// cooldown floor — the desired state must be "normal".
	got := desiredSoftFailoverState("warming", probeResult{
		Status:        "allowed",
		Utilization5h: 0.0, // gardenio3 case
	}, 0.80)
	if got != "normal" {
		t.Errorf("got %q, want %q (low util after restart should cool down)", got, "normal")
	}
}

func TestDesiredSoftFailoverState_WarmingHoldsInsideHysteresisBand(t *testing.T) {
	// 0.80 threshold, 0.05 hysteresis ⇒ cool only below 0.75. At 0.78 we stay
	// warming.
	got := desiredSoftFailoverState("warming", probeResult{
		Status:        "allowed",
		Utilization5h: 0.78,
	}, 0.80)
	if got != "warming" {
		t.Errorf("got %q, want %q (within hysteresis band)", got, "warming")
	}
}

func TestDesiredSoftFailoverState_NormalCrossesIntoWarming(t *testing.T) {
	got := desiredSoftFailoverState("normal", probeResult{
		Status:        "allowed_warning",
		Utilization5h: 0.85,
	}, 0.80)
	if got != "warming" {
		t.Errorf("got %q, want %q", got, "warming")
	}
}

func TestDesiredSoftFailoverState_NormalStaysNormalBelowThreshold(t *testing.T) {
	got := desiredSoftFailoverState("normal", probeResult{
		Status:        "allowed",
		Utilization5h: 0.10,
	}, 0.80)
	if got != "normal" {
		t.Errorf("got %q, want %q", got, "normal")
	}
}

func TestDesiredSoftFailoverState_ThrottledLeavesPriorStateUntouched(t *testing.T) {
	// While throttled, hard failover owns the account — soft_failover_state
	// should be left as-is so we don't churn the field.
	for _, prev := range []string{"", "normal", "warming"} {
		got := desiredSoftFailoverState(prev, probeResult{
			Status:        "rejected",
			Utilization5h: 1.0,
		}, 0.80)
		if got != prev {
			t.Errorf("prev=%q: got %q, want %q (throttled should preserve prev)", prev, got, prev)
		}
	}
}

func TestDesiredSoftFailoverState_EmptyPrevTreatedAsNormal(t *testing.T) {
	// Newly-created account beads have no soft_failover_state field. They
	// should be reconciled to "normal" on the first probe when utilization is
	// low.
	got := desiredSoftFailoverState("", probeResult{
		Status:        "allowed",
		Utilization5h: 0.20,
	}, 0.80)
	if got != "normal" {
		t.Errorf("got %q, want %q", got, "normal")
	}
}
