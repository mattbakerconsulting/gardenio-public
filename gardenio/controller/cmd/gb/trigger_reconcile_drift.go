// Package main — final drift-candidate composition for `gb trigger reconcile`.
//
// The wake-intent report attaches four additive overlays to the unconditional
// cron diff (scheduler.ReconcileFires): the point-in-time `gate` probe
// (trigger_reconcile_gate.go), and the historical `pause` (trigger_reconcile_pause.go)
// and `lease` (trigger_reconcile_lease.go) overlays. The pause and lease overlays
// each compute a deterministic residual_missed that CHAINS — pause subtracts its
// suppressed misses from fires.missed_count, lease subtracts its own from the
// pause residual — so the reconciler agent reads ONE refined number
// (lease.residual_missed) instead of re-deriving the subtraction by hand.
//
// THE GAP THIS CLOSES (ka-0000004262, 3rd recurrence on the gardener-rescope drain
// ka-0000007515; siblings ka-0000005279, ka-0000001961). The `gate` probe was the
// ONE overlay left OUT of that residual chain. It shipped (MR !6488) as a raw
// point-in-time signal (would_fire_now + per-leaf raw-vs-effective counts) that
// the agent had to MANUALLY reconcile against the big residual_missed number —
// and the haiku reconciler kept getting it wrong for a condition-gated trigger
// whose entire deficit is legitimate gated no-fires: it re-filed a `habitual`
// drift bug DESPITE gate.subtraction_explains_miss == true, the exact state the
// gate HARD RULE forbids. Prompt-only fixes (syncing the classification rule into
// the reconciler bead) already regressed once (ka-0000001961) because they leave
// the judgment to the model.
//
// The fix mirrors what pause/lease already do: make the gate the FINAL link in
// the residual chain, computed in code. When the point-in-time pre-flight is
// deterministically declining to fire right now (gate.would_fire_now == false),
// the remaining historical residual is a run of gated no-fires — drift candidate
// 0. When the pre-flight WOULD fire now (gate.would_fire_now == true) yet fires
// are still missed, that is the genuine dark-lane signal and the residual stands.
// This is exactly the condition-suppressed-vs-genuinely-dark split the dark-lane
// detector already makes (internal/bridge/trigger_dark_lane.go), and exactly the
// approximation the gate HARD RULE already mandates ("would_fire_now == false ⇒
// IN SYNC, never drift, no matter how low the raw hit rate") — now emitted as a
// single deterministic field so the model no longer has to apply it by hand.
package main

// finalResidualBeforeGate returns the most-refined HISTORICAL residual miss count
// available on the report — the end of the pause→lease chain — BEFORE the
// point-in-time gate is applied. It walks the overlays newest (most refined)
// first and falls back as each is absent or un-evaluated, mirroring the
// fallback ladder the reconciler prompt already documents:
//
//	time_window.residual_missed (when the time_window overlay evaluated)
//	  → lease.residual_missed  (when only the lease overlay evaluated)
//	    → pause.residual_missed  (when only the pause overlay evaluated)
//	      → fires.missed_count   (when no historical overlay evaluated)
//
// Returns (count, ok): ok is false when there is no fires diff at all (event-only
// or disable-collapsed trigger), so the caller leaves the drift-candidate count
// at its zero value and the gate composition is skipped.
func finalResidualBeforeGate(r *reconcileReport) (int, bool) {
	if r.Fires == nil {
		return 0, false
	}
	if r.TimeWindow != nil && r.TimeWindow.Evaluated {
		return r.TimeWindow.ResidualMissed, true
	}
	if r.Lease != nil && r.Lease.Evaluated {
		return r.Lease.ResidualMissed, true
	}
	if r.Pause != nil && r.Pause.Evaluated {
		return r.Pause.ResidualMissed, true
	}
	return r.Fires.MissedCount, true
}

// applyDormancyAndGateToResidual composes the live gardener-dormancy verdict and
// the point-in-time gate probe onto the historical pause→lease→time_window
// residual and stamps the report's single deterministic DriftCandidateMissed
// (+ DormancyExplainsResidual / GateExplainsResidual) field. Idempotent and
// fail-soft: when both overlays are absent / not-parked / un-evaluated, the
// historical residual passes through unchanged so nothing that was already a
// drift candidate is silently erased.
//
// Precedence — dormancy first, then the gate:
//
//   - dormancy present, Parked == true → the gardener is provably parked right
//     now behind an open operator blocker (its whole frontier single-threaded
//     behind a human) or a declared block-until-MR-merged. It fires NOTHING by
//     design, so the entire residual is parks, not drift → candidate 0,
//     DormancyExplainsResidual set when it zeroed a positive residual. This is
//     the HIGHEST-precedence link: a park subsumes any gate reasoning (a parked
//     gardener's pre-flight is never even reached). It CANNOT mask a genuine
//     lease-wedge outage or a real drift — the probe sets Parked only on a
//     positively-proven live frontier wedge, never from the stamped fields.
//   - otherwise, gate present, evaluated, would_fire_now == false → the
//     pre-flight is deterministically declining to fire now (an empty lane, or
//     one its exclude subtractions floor to 0). The residual is a run of gated
//     no-fires → candidate 0, GateExplainsResidual set when it zeroed a positive
//     residual.
//   - otherwise (not parked AND would_fire_now == true, or both overlays nil /
//     un-evaluated) → the residual stands. A positive value here with neither a
//     park nor a declining gate is the genuine dark-lane signal the reconciler
//     MUST still surface.
func applyDormancyAndGateToResidual(r *reconcileReport) {
	residual, ok := finalResidualBeforeGate(r)
	if !ok {
		return // no fires diff — nothing to classify
	}

	// Default: the historical residual is the drift candidate (both overlays
	// absent or un-evaluable — fall back exactly as the prompt's ladder does).
	r.DriftCandidateMissed = residual

	// Dormancy takes precedence: a parked gardener fires nothing by design, so its
	// misses are parks, not drift — regardless of any gate verdict.
	if d := r.Dormancy; d != nil && d.Parked {
		if residual > 0 {
			r.DormancyExplainsResidual = true
		}
		r.DriftCandidateMissed = 0
		return
	}

	g := r.Gate
	if g == nil || !g.Evaluated {
		return
	}
	// The point-in-time pre-flight is currently declining to fire: the remaining
	// misses are gated no-fires, not drift. Zero the candidate and flag WHY, but
	// only mark GateExplainsResidual when the gate actually changed a positive
	// residual to 0 — a gate that fires now (or a residual already 0) explains
	// nothing.
	if !g.WouldFireNow {
		if residual > 0 {
			r.GateExplainsResidual = true
		}
		r.DriftCandidateMissed = 0
	}
}
