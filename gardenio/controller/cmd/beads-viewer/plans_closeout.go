package main

// plans_closeout.go — the /plans EXIT RAMP (Matt's ask, Slack thread
// ka-0000001785: "we need a better way to close out plans so we dont have to
// look at every plan we've every made"; decision ka-0000001273 chose
// build-now: tasks ka-0000004467 + ka-0000005361).
//
// THE GAP THIS CLOSES. The daemon ALREADY detects a finished plan. When a plan
// epic's whole issue-kind subtree drains, kbeads' auto-close cascade
// deliberately does NOT hard-close a PLAN epic (the operator's Mark-complete is
// the gate, decision ka-0000004595 chose PROPOSE) — it FLAGS the epic with the
// `autoclose-ready` label plus an audit comment and publishes a bead-updated
// event (kbeads internal/server/auto_close.go flagPlanEpicReady). The viewer
// then never rendered it: before this file, `grep autoclose` across the whole
// beads-viewer returned ONE hit, a code comment. The signal was computed,
// published, and thrown away, so a drained plan sat on the index looking
// identical to a live one and the index accumulated every plan ever made.
//
// Measured on the live daemon (2026-08-12): 56 open plan epics against 89
// closed; 2 already carried `autoclose-ready`; 39 of 56 had plan_state UNSET.
//
// WHAT THIS FILE OWNS. Two pure projections onto the already-enriched plan row,
// plus the row-level legality predicate the one-tap lever gates on:
//
//   - planCloseoutReady — does the plan carry the daemon's flag?
//   - planCloseoutDrained — the viewer-side twin: every rendered step closed on a
//     still-open plan (the iter-41 "ghost-done" signal, StepsAllDone). The flag
//     only fires on a CASCADE (a child close event with MR work merged), so a
//     plan drained before the feature landed, or drained by a route that
//     bypassed the cascade, is drained-but-unflagged. Both roads lead to "this
//     plan looks finished".
//   - planCompleteReachable — THE LEGALITY GATE (see below).
//
// Everything here is a pure function of fields planIndexEnrich already computed,
// so the Done? lane, the card badge, and the bulk verb cost ZERO extra daemon
// call and cannot disagree with each other.
//
// ── THE LEGALITY GATE (load-bearing; do not render a lever without it) ────────
// "Mark complete" is NOT reachable from every plan_state. The daemon's
// validatePlanStateTransition (kbeads plan_state_validation.go) admits exactly
// one edge INTO complete — building → complete — plus the backward-compat
// carve-out that any transition FROM unset is legal (a legacy/ungated plan may
// enter any state). So:
//
//	unset    → complete   LEGAL (the backward-compat opt-in edge; 39 of 56 live
//	                      open plans are unset, so this is the DOMINANT case)
//	building → complete   LEGAL (the cockpit's own Mark-complete edge)
//	draft    → complete   REJECTED by the daemon (400)
//	ready    → complete   REJECTED by the daemon (400)
//	complete → complete   no-op (handlePlanComplete is idempotent)
//
// This is not hypothetical. Of the two live plans carrying `autoclose-ready`,
// one (ka-0000005352) is plan_state=READY — drained, flagged, and NOT legally
// completable. Rendering a one-tap "Mark complete" on it would post a request
// the daemon is guaranteed to reject, which is worse than rendering nothing: the
// operator taps a lever that cannot work and learns nothing about why.
//
// So the lever gates on planCompleteReachable, and a drained-but-unreachable
// plan still gets the BADGE (the honest "this looks finished" signal) with a
// tooltip naming the blocked lifecycle instead of a dead button. Draining a plan
// that was never approved for build is precisely the case `plan_state=abandoned`
// exists to serve — step ka-0000001377, which the operator deliberately deferred
// out of this build (decision ka-0000001273 chose build-now over design-first).
// Until that state exists, the honest surface is a badge plus a named reason.
// ─────────────────────────────────────────────────────────────────────────────

// planCloseoutReadyLabel is the label kbeads' auto-close cascade stamps on a
// fully-drained PLAN epic instead of hard-closing it (flagPlanEpicReady). Kept
// in sync with kbeads internal/server/auto_close.go's autoCloseReadyLabel — the
// daemon is the writer, the viewer is a pure reader.
const planCloseoutReadyLabel = "autoclose-ready"

// planCloseoutBoardLane is the derived "Done?" board column — the drained /
// flagged plans awaiting an operator's one-tap retire. It is DELIBERATELY
// distinct from the terminal "closed" lane: these plans are still OPEN, they
// merely look finished. Placing them in their own column is what turns the
// exit ramp into a routine (open the board, drain the Done? lane) instead of an
// archaeology dig through every card.
const planCloseoutBoardLane = "closeout"

// planCloseoutReady reports whether the DAEMON flagged this plan as drained —
// the `autoclose-ready` label its auto-close cascade stamps. This is the
// authoritative signal (the daemon saw every child close AND checked that
// MR-linked work merged); the viewer only reads it.
func planCloseoutReady(r planRowView) bool {
	return r.CloseoutFlagged
}

// planCloseoutDrained reports whether the plan LOOKS finished to the viewer even
// without the daemon's flag: every rendered step closed while the plan epic
// itself is still non-terminal. This is exactly the iter-41 "ghost-done" signal
// the card already badges as "✓ steps done" (StepsAllDone already folds in the
// !planStatusTerminal guard and the empty-in-flight-buckets guard).
//
// Why BOTH signals: flagPlanEpicReady fires only from the auto-close CASCADE, on
// a child-close event, with the MR-merged check satisfied. A plan drained before
// that feature shipped, or drained by a path that never triggered the cascade,
// is drained-but-unflagged forever — the daemon never retro-scans. Measured
// live: 5 open plans had zero open children while only 2 carried the flag. Union
// them so the Done? lane surfaces the real pile, not just the flagged subset.
func planCloseoutDrained(r planRowView) bool {
	return r.StepsAllDone
}

// planNeedsCloseout is the union: a still-open plan that either the daemon
// flagged or that the viewer can see is drained. This is the Done?-lane
// membership test and the card badge's render gate.
//
// Gated on non-terminal status (via both inputs): CloseoutFlagged is cleared for
// a terminal plan at the enrich seam and StepsAllDone carries its own
// !planStatusTerminal guard — a CLOSED plan is already done, so it belongs in
// the Closed lane, never in "Done?". This is the same terminal-reconcile
// discipline the iter-25/27/38/41 card signals follow.
func planNeedsCloseout(r planRowView) bool {
	return planCloseoutReady(r) || planCloseoutDrained(r)
}

// planCompleteReachable reports whether plan_state=complete is a LEGAL next
// state for this plan — the gate the one-tap "Mark complete" lever renders
// behind. It mirrors the daemon's validatePlanStateTransition exactly (see the
// file header's table): unset (the backward-compat opt-in edge) and building
// (the cockpit's own edge) can reach complete; draft and ready cannot.
//
// A plan ALREADY complete returns false: there is nothing left to do, and
// handlePlanComplete would idempotently re-render. The caller renders Reopen
// there instead, exactly as the cockpit + peek drawer do.
//
// Mirroring rather than importing is deliberate and is the same arrangement
// gardenerEnableGatedByState already uses for the R6 gate: the viewer is a
// separate Go module from kbeads, the daemon enforces the invariant
// unconditionally at its own choke point regardless of caller (carve-out
// ka-0000004319), and this predicate is a UX pre-check only. If the two ever
// drift, the daemon still rejects the illegal write — the failure mode is a
// button that errors inline, never a corrupted lifecycle.
func planCompleteReachable(st planState) bool {
	switch st {
	case planStateUnset, planStateBuilding:
		return true
	default: // draft / ready (daemon rejects), complete (already terminal)
		return false
	}
}

// planCloseoutBlockedReason names WHY a drained plan cannot be one-tap
// completed, for the badge tooltip. Empty when completion IS reachable (the
// lever renders instead) or when the plan is already complete.
//
// This exists so a blocked plan is never silently undecorated: the operator sees
// "drained, but its lifecycle blocks completion, and here is the word" rather
// than a badge with no lever and no explanation. Both blocked states are
// pre-build (draft/ready), i.e. a plan whose work drained without ever being
// approved — the `abandoned` terminal state's exact use case (step
// ka-0000001377, deferred by decision ka-0000001273).
func planCloseoutBlockedReason(st planState) string {
	switch st {
	case planStateDraft:
		return "still a draft — it was never approved for build, so the lifecycle has no legal move to complete. " +
			"Approve & build it first, or reopen the plan-state design (an `abandoned` terminal state is the real fix)."
	case planStateReady:
		return "sitting in ready — it drained without ever being approved for build, so the lifecycle has no legal move to complete. " +
			"Approve & build it first, or reopen the plan-state design (an `abandoned` terminal state is the real fix)."
	default:
		return ""
	}
}

// KNOWN ASYMMETRY with the cockpit + peek drawer (deliberately not "fixed" here).
// Those two surfaces gate their own Mark complete on `st == planStateBuilding`
// alone (plans_gardener.go planGardenerBarData, plans_board.go renderPlanPeek),
// which is STRICTER than planCompleteReachable: they offer no lever on an UNSET
// plan even though unset→complete is legal. That is safe (a stricter gate never
// renders an illegal lever) but it means an UNSET drained plan can be retired
// from the /plans index card and NOT from its own cockpit — 39 of 56 live open
// plans are unset, so this is the common case, and an operator who opens the
// cockpit to check before retiring will find the button missing.
//
// Left alone ON PURPOSE: widening those two gates changes the cockpit's terminal
// affordance for every ungated legacy plan, which is a behavior change well
// outside "surface the flag the daemon already computes" (tasks ka-0000004467 +
// ka-0000005361) and belongs with the `abandoned` lifecycle work that is
// revisiting these edges anyway (step ka-0000001377). Filed as its own follow-up
// rather than smuggled in here.
//
// planCloseoutBadge is the card's "looks finished" decoration, derived once at
// the enrich seam so the badge, the Done? lane, and the bulk verb all read the
// same projection.
type planCloseoutBadge struct {
	// Show gates the whole badge — planNeedsCloseout.
	Show bool
	// Flagged marks the DAEMON-flagged case (`autoclose-ready`), which reads
	// louder than the viewer-derived drained case: the daemon verified every
	// child closed AND MR work merged, so it is the higher-confidence signal.
	Flagged bool
	// CanComplete gates the one-tap "Mark complete" lever — planCompleteReachable.
	CanComplete bool
	// BlockedReason names why the lever is absent (planCloseoutBlockedReason).
	// Non-empty only when Show && !CanComplete on a pre-build plan.
	BlockedReason string
	// OpenSteps is the plan's remaining open-step count, threaded into the
	// confirm prompt the SAME way the cockpit/peek Mark-complete levers do. Zero
	// for the drained case by construction; a flagged plan can in principle
	// carry a leftover non-issue child, so it is passed through rather than
	// assumed.
	OpenSteps int
}

// planCloseoutBadgeFor derives the badge for one enriched row. Pure over fields
// planIndexEnrich already computed — zero extra daemon call — so it is applied at
// the single shared enrich seam and the page render, the single-row OOB
// re-render (renderPlanRowWith), and the bulk response can never disagree.
func planCloseoutBadgeFor(r planRowView) planCloseoutBadge {
	if !planNeedsCloseout(r) {
		return planCloseoutBadge{}
	}
	st := planState(r.PlanState)
	b := planCloseoutBadge{
		Show:        true,
		Flagged:     planCloseoutReady(r),
		CanComplete: planCompleteReachable(st),
		OpenSteps:   r.StepsOpen,
	}
	if !b.CanComplete {
		b.BlockedReason = planCloseoutBlockedReason(st)
	}
	return b
}
