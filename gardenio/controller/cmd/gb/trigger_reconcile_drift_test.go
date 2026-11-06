package main

import (
	"testing"

	"gardenio/controller/internal/bridge"
	"gardenio/controller/internal/scheduler"
)

// firesWithMissed builds a minimal FireReconciliation carrying a given
// missed-fire count — enough for the residual-composition logic, which only
// reads MissedCount as the last-resort fallback denominator.
func firesWithMissed(missed int) *scheduler.FireReconciliation {
	return &scheduler.FireReconciliation{MissedCount: missed}
}

// TestApplyGateToResidual_GatedLaneZeroesDrift is the live-fire regression for
// bug ka-0000004262 (3rd recurrence on gardener-rescope ka-0000007515). The
// historical overlays leave a large residual (271 in production, after the pause
// overlay), but the point-in-time gate declines to fire (would_fire_now=false)
// because every needs-rescope bead is decision-gated (subtraction_explains_miss).
// The composed drift candidate MUST be 0 with GateExplainsResidual set — the
// deterministic "IN SYNC, never drift" verdict the model kept failing to apply.
func TestApplyGateToResidual_GatedLaneZeroesDrift(t *testing.T) {
	r := &reconcileReport{
		Fires: firesWithMissed(332),
		Pause: &pauseExclusion{Evaluated: true, SuppressedMissed: 61, ResidualMissed: 271},
		Gate: &gateEval{
			Evaluated:    true,
			WouldFireNow: false,
			Leaves: []gateLeaf{{
				RawCount: 4, EffectiveCount: 0, SubtractionExplainsMiss: true,
			}},
		},
	}
	applyDormancyAndGateToResidual(r)
	if r.DriftCandidateMissed != 0 {
		t.Errorf("drift_candidate_missed: want 0 (gate declines to fire — all misses gated), got %d", r.DriftCandidateMissed)
	}
	if !r.GateExplainsResidual {
		t.Error("gate_explains_residual must be TRUE: the gate zeroed a positive (271) residual")
	}
}

// TestApplyGateToResidual_WouldFireNowKeepsResidual guards against the fix
// hiding a GENUINE dark lane. When the pre-flight WOULD fire now
// (would_fire_now=true — there is actionable work) yet fires are still missed,
// that is the real dark-lane signal: the residual must STAND and
// GateExplainsResidual must be false.
func TestApplyGateToResidual_WouldFireNowKeepsResidual(t *testing.T) {
	r := &reconcileReport{
		Fires: firesWithMissed(40),
		Pause: &pauseExclusion{Evaluated: true, ResidualMissed: 40},
		Lease: &leaseExclusion{Evaluated: true, ResidualMissed: 40},
		Gate:  &gateEval{Evaluated: true, WouldFireNow: true},
	}
	applyDormancyAndGateToResidual(r)
	if r.DriftCandidateMissed != 40 {
		t.Errorf("drift_candidate_missed: want 40 (would_fire_now=true — genuine dark lane), got %d", r.DriftCandidateMissed)
	}
	if r.GateExplainsResidual {
		t.Error("gate_explains_residual must be FALSE: the gate fires now, it explains nothing")
	}
}

// TestApplyGateToResidual_PrefersLeaseResidual proves the composition walks the
// overlay chain newest-first: when the lease overlay evaluated, its residual is
// the historical denominator (not pause's, not fires.missed_count).
func TestApplyGateToResidual_PrefersLeaseResidual(t *testing.T) {
	r := &reconcileReport{
		Fires: firesWithMissed(100),
		Pause: &pauseExclusion{Evaluated: true, ResidualMissed: 30},
		Lease: &leaseExclusion{Evaluated: true, ResidualMissed: 2},
		Gate:  &gateEval{Evaluated: true, WouldFireNow: true}, // fires now → residual stands
	}
	applyDormancyAndGateToResidual(r)
	if r.DriftCandidateMissed != 2 {
		t.Errorf("drift_candidate_missed: want 2 (lease residual is the refined count), got %d", r.DriftCandidateMissed)
	}
}

// TestApplyGateToResidual_FallsBackToPauseThenFires proves the fallback ladder
// when finer overlays are un-evaluated: lease un-evaluated → pause residual;
// both un-evaluated → fires.missed_count.
func TestApplyGateToResidual_FallsBackToPauseThenFires(t *testing.T) {
	// Lease un-evaluated (holder scan failed) → fall back to pause residual.
	r := &reconcileReport{
		Fires: firesWithMissed(100),
		Pause: &pauseExclusion{Evaluated: true, ResidualMissed: 30},
		Lease: &leaseExclusion{Evaluated: false},
		Gate:  &gateEval{Evaluated: true, WouldFireNow: true},
	}
	applyDormancyAndGateToResidual(r)
	if r.DriftCandidateMissed != 30 {
		t.Errorf("lease un-evaluated: want pause residual 30, got %d", r.DriftCandidateMissed)
	}

	// Both historical overlays un-evaluated → fall back to fires.missed_count.
	r2 := &reconcileReport{
		Fires: firesWithMissed(100),
		Pause: &pauseExclusion{Evaluated: false},
		Lease: &leaseExclusion{Evaluated: false},
		Gate:  &gateEval{Evaluated: true, WouldFireNow: true},
	}
	applyDormancyAndGateToResidual(r2)
	if r2.DriftCandidateMissed != 100 {
		t.Errorf("both overlays un-evaluated: want fires.missed_count 100, got %d", r2.DriftCandidateMissed)
	}
}

// TestApplyGateToResidual_AbsentGatePassesResidualThrough proves fail-soft
// behavior: an unconditional trigger (gate nil) or a failed gate probe
// (Evaluated=false) must leave the historical residual UNTOUCHED — never erase a
// real candidate just because the gate could not be read.
func TestApplyGateToResidual_AbsentGatePassesResidualThrough(t *testing.T) {
	// gate nil (unconditional trigger).
	r := &reconcileReport{
		Fires: firesWithMissed(50),
		Pause: &pauseExclusion{Evaluated: true, ResidualMissed: 50},
	}
	applyDormancyAndGateToResidual(r)
	if r.DriftCandidateMissed != 50 || r.GateExplainsResidual {
		t.Errorf("nil gate: want residual 50 pass-through, explains=false; got %d explains=%v", r.DriftCandidateMissed, r.GateExplainsResidual)
	}

	// gate un-evaluated (probe failed).
	r2 := &reconcileReport{
		Fires: firesWithMissed(50),
		Pause: &pauseExclusion{Evaluated: true, ResidualMissed: 50},
		Gate:  &gateEval{Evaluated: false, Error: "boom"},
	}
	applyDormancyAndGateToResidual(r2)
	if r2.DriftCandidateMissed != 50 || r2.GateExplainsResidual {
		t.Errorf("un-evaluated gate: want residual 50 pass-through, explains=false; got %d explains=%v", r2.DriftCandidateMissed, r2.GateExplainsResidual)
	}
}

// TestApplyGateToResidual_ZeroResidualNoFalseExplain proves GateExplainsResidual
// is only set when the gate actually zeroed a POSITIVE residual. A healthy
// trigger whose historical residual is already 0 (pause/lease explained it) with
// a gate that declines now must report drift 0 but NOT claim the gate explained
// anything — the explanation belongs to the overlay that did the work.
func TestApplyGateToResidual_ZeroResidualNoFalseExplain(t *testing.T) {
	r := &reconcileReport{
		Fires: firesWithMissed(61),
		Pause: &pauseExclusion{Evaluated: true, SuppressedMissed: 61, ResidualMissed: 0},
		Gate:  &gateEval{Evaluated: true, WouldFireNow: false},
	}
	applyDormancyAndGateToResidual(r)
	if r.DriftCandidateMissed != 0 {
		t.Errorf("drift_candidate_missed: want 0, got %d", r.DriftCandidateMissed)
	}
	if r.GateExplainsResidual {
		t.Error("gate_explains_residual must be FALSE: the residual was already 0 (pause explained it), the gate added nothing")
	}
}

// TestApplyGateToResidual_TimeWindowIsFinalResidualLink is the live-fire
// regression for bug ka-0000000211 (recurrence of ka-0000005666 on the
// quiet-hours release-owner gardener ka-0000003719). The trigger fires
// `13,43 * * * *` PT gated by a `time_window outside 07:00-18:00 Mon-Fri PT`
// quiet-hours condition. Over the window it missed 21 raw fires, ALL of which
// fell inside a Fri quiet window — so the `time_window` overlay attributes every
// one (suppressed_missed=21, residual_missed=0). Crucially the `lease` overlay
// runs BEFORE time_window in the chain and leaves residual_missed=21 (a
// quiet-hours miss is not a lease deferral), and the reconciler is sampled on a
// Saturday (outside quiet hours) so `gate.would_fire_now=true`. The composed
// drift_candidate_missed MUST key off the newest evaluated overlay
// (time_window.residual_missed=0), NOT lease.residual_missed=21 — otherwise the
// haiku agent re-files a phantom `habitual` drift bug, exactly what recurred.
func TestApplyGateToResidual_TimeWindowIsFinalResidualLink(t *testing.T) {
	r := &reconcileReport{
		Fires:      firesWithMissed(21),
		Pause:      &pauseExclusion{Evaluated: true, ResidualMissed: 21},
		Lease:      &leaseExclusion{Evaluated: true, ResidualMissed: 21},
		TimeWindow: &timeWindowExclusion{Evaluated: true, SuppressedMissed: 21, ResidualMissed: 0},
		// Sampled outside quiet hours: the point-in-time gate would fire now, so
		// it must NOT be the thing that zeroes the residual — the deterministic
		// time_window overlay is.
		Gate: &gateEval{Evaluated: true, WouldFireNow: true},
	}
	applyDormancyAndGateToResidual(r)
	if r.DriftCandidateMissed != 0 {
		t.Errorf("drift_candidate_missed: want 0 (time_window explains all 21 quiet-hours misses), got %d — judging against lease.residual_missed=21 is the ka-0000000211 false positive", r.DriftCandidateMissed)
	}
	if r.GateExplainsResidual {
		t.Error("gate_explains_residual must be FALSE: the time_window overlay (not the would-fire-now gate) zeroed the residual")
	}
}

// TestApplyGateToResidual_TimeWindowResidualStandsWhenDark guards the mirror of
// the above: a quiet-hours trigger whose time_window overlay left a POSITIVE
// residual (a genuinely missed fire OUTSIDE quiet hours) with the gate firing now
// is a real dark-lane signal — the residual must STAND so the reconciler still
// surfaces it. This proves the time_window link cannot launder a genuine miss.
func TestApplyGateToResidual_TimeWindowResidualStandsWhenDark(t *testing.T) {
	r := &reconcileReport{
		Fires:      firesWithMissed(25),
		Pause:      &pauseExclusion{Evaluated: true, ResidualMissed: 25},
		Lease:      &leaseExclusion{Evaluated: true, ResidualMissed: 25},
		TimeWindow: &timeWindowExclusion{Evaluated: true, SuppressedMissed: 21, ResidualMissed: 4},
		Gate:       &gateEval{Evaluated: true, WouldFireNow: true},
	}
	applyDormancyAndGateToResidual(r)
	if r.DriftCandidateMissed != 4 {
		t.Errorf("drift_candidate_missed: want 4 (time_window residual — 4 misses outside quiet hours, gate fires now), got %d", r.DriftCandidateMissed)
	}
}

// TestApplyGateToResidual_NoFiresDiffIsNoOp proves an event-only / disable-
// collapsed trigger (Fires nil) is skipped: DriftCandidateMissed stays at its
// zero value and nothing panics on the nil diff.
func TestApplyGateToResidual_NoFiresDiffIsNoOp(t *testing.T) {
	r := &reconcileReport{Gate: &gateEval{Evaluated: true, WouldFireNow: false}}
	applyDormancyAndGateToResidual(r)
	if r.DriftCandidateMissed != 0 || r.GateExplainsResidual {
		t.Errorf("no fires diff: want drift 0 explains=false, got %d explains=%v", r.DriftCandidateMissed, r.GateExplainsResidual)
	}
}

// TestApplyDormancy_ParkedZeroesDrift is the live-fire regression for bug
// ka-0000007872: a correctly-parked gardener (dormancy.Parked=true) whose
// historical residual is large (336 in production) must compose to a 0 drift
// candidate with DormancyExplainsResidual set — the deterministic "PARKED, not
// MISSED" verdict the reconcile report previously failed to apply, calling a
// by-design-dormant gardener 100% drift.
func TestApplyDormancy_ParkedZeroesDrift(t *testing.T) {
	r := &reconcileReport{
		Fires:    firesWithMissed(336),
		Pause:    &pauseExclusion{Evaluated: true, ResidualMissed: 336},
		Lease:    &leaseExclusion{Evaluated: true, ResidualMissed: 336},
		Dormancy: &bridge.ReconcileDormancy{Evaluated: true, Parked: true, Kind: "operator_wedge", LiveBlocker: "gd-esc-1"},
	}
	applyDormancyAndGateToResidual(r)
	if r.DriftCandidateMissed != 0 {
		t.Errorf("drift_candidate_missed: want 0 (gardener parked — every miss is a by-design park), got %d", r.DriftCandidateMissed)
	}
	if !r.DormancyExplainsResidual {
		t.Error("dormancy_explains_residual must be TRUE: the park zeroed a positive (336) residual")
	}
	if r.GateExplainsResidual {
		t.Error("gate_explains_residual must be FALSE: dormancy, not the gate, explained the residual")
	}
}

// TestApplyDormancy_TakesPrecedenceOverGate proves dormancy is the
// highest-precedence link: a parked gardener sampled while its point-in-time gate
// WOULD fire now (would_fire_now=true) must still compose to 0 drift — a parked
// gardener's pre-flight is never even reached, so a live would-fire snapshot must
// not resurrect the residual as drift.
func TestApplyDormancy_TakesPrecedenceOverGate(t *testing.T) {
	r := &reconcileReport{
		Fires:    firesWithMissed(50),
		Pause:    &pauseExclusion{Evaluated: true, ResidualMissed: 50},
		Dormancy: &bridge.ReconcileDormancy{Evaluated: true, Parked: true, Kind: "operator_wedge", LiveBlocker: "gd-esc-1"},
		Gate:     &gateEval{Evaluated: true, WouldFireNow: true}, // gate would fire — but the gardener is parked
	}
	applyDormancyAndGateToResidual(r)
	if r.DriftCandidateMissed != 0 {
		t.Errorf("drift_candidate_missed: want 0 (dormancy outranks a would-fire gate), got %d", r.DriftCandidateMissed)
	}
	if !r.DormancyExplainsResidual {
		t.Error("dormancy_explains_residual must be TRUE")
	}
}

// TestApplyDormancy_NotParkedFallsThroughToGate proves the break-it path: a
// gardener the probe found NOT parked (an actionable frontier despite a stamped
// dormant flag) does not zero the residual on dormancy — it falls through to the
// normal gate logic, so a genuine dark lane (would_fire_now=true) still reports
// drift.
func TestApplyDormancy_NotParkedFallsThroughToGate(t *testing.T) {
	r := &reconcileReport{
		Fires:    firesWithMissed(40),
		Pause:    &pauseExclusion{Evaluated: true, ResidualMissed: 40},
		Dormancy: &bridge.ReconcileDormancy{Evaluated: true, Parked: false, DormantDeclared: true, ActionableChildren: 3},
		Gate:     &gateEval{Evaluated: true, WouldFireNow: true},
	}
	applyDormancyAndGateToResidual(r)
	if r.DriftCandidateMissed != 40 {
		t.Errorf("drift_candidate_missed: want 40 (not parked + gate fires now — genuine drift stands), got %d", r.DriftCandidateMissed)
	}
	if r.DormancyExplainsResidual {
		t.Error("dormancy_explains_residual must be FALSE: the gardener was not parked")
	}
}

// TestApplyDormancy_NilDormancyPreservesGateBehavior proves a non-gardener trigger
// (nil dormancy) leaves the existing gate composition untouched — the gate still
// zeroes a gated residual exactly as before, with no dormancy interference.
func TestApplyDormancy_NilDormancyPreservesGateBehavior(t *testing.T) {
	r := &reconcileReport{
		Fires: firesWithMissed(20),
		Pause: &pauseExclusion{Evaluated: true, ResidualMissed: 20},
		Gate:  &gateEval{Evaluated: true, WouldFireNow: false},
		// Dormancy nil (non-gardener trigger).
	}
	applyDormancyAndGateToResidual(r)
	if r.DriftCandidateMissed != 0 {
		t.Errorf("drift_candidate_missed: want 0 (gate declines — unchanged legacy behavior), got %d", r.DriftCandidateMissed)
	}
	if !r.GateExplainsResidual {
		t.Error("gate_explains_residual must be TRUE: with nil dormancy the gate still explains the residual")
	}
	if r.DormancyExplainsResidual {
		t.Error("dormancy_explains_residual must be FALSE: there is no dormancy overlay")
	}
}
