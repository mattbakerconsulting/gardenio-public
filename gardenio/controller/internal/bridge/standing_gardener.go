// Package bridge — standing (perpetual) gardener discriminator.
//
// A STANDING gardener is a perpetual loop whose terminal_goal is a standing
// mandate — "drive READY beads -> MR -> merge" (gardener-delivery ka-0000001510),
// "main -> golden tag -> UAT" (gardener-release-owner ka-0000006579) — that never
// self-declares "done", and which is pinned to NO terminal epic. For such a
// gardener, a quiet period where every run legitimately scores zero progress is
// NORMAL steady state, not a stuck loop.
//
// The resume gate's give-up heuristic (gardenerResumeOrEscalate) was written for
// EPIC-BOUNDED gardeners: once a gardener has re-armed ResumeBudget times with no
// progress and its terminal_goal is still unmet, it flags
// gardener-stuck-escalate and parks dormant until its plan changes, or CLOSES
// when it has no open plan (gd-dNxyjFLGRJ). Applied to a standing gardener that is idle only because there
// is nothing to do (main +0 commits, no ready beads), that heuristic
// systematically REAPED exactly the gardeners that must never die — the release
// lane went dark during quiet periods (bug ka-0000006241, operator decision
// ka-0000000928).
//
// gardenerIsStanding is the discriminator the resume gate consults to treat a
// standing gardener differently: it re-arms IN PLACE forever on a spent resume
// budget instead of escalating + closing. The terminal hard_stop brakes
// (consecutive_failures, wall_clock) are UNAFFECTED — a genuinely broken
// standing gardener (5 crashes in a row, 30 days elapsed) still closes; only the
// no-progress give-up is suppressed.
package bridge

import "slices"

// standingGardenerLabel is the trigger-bead label that marks a perpetual
// gardener. The live release-owner + delivery gardeners already carry it, so
// recognizing it makes the bug ka-0000006241 fix retroactive with no data
// migration — no operator has to re-author either gardener blob.
const standingGardenerLabel = "perpetual:true"

// gardenerHasStandingMarker reports whether a gardener carries a machine-readable
// standing marker: EITHER its gardener blob sets standing:true OR its label set
// carries the perpetual:true label. This is the SINGLE definition of "what
// counts as a standing marker" — gardenerIsStanding (the engine discriminator)
// and ValidateGardenerStandingMarker (the authoring/firing guard for bug
// ka-0000004311) both consult it, so the guard can never disagree with the
// engine on which signals make a gardener standing. It is intentionally
// permissive on the OR side (either signal ⇒ marked) so the fix covers the live
// label-only gardeners without requiring a blob edit, and covers a future
// blob-only gardener that predates or omits the label.
func gardenerHasStandingMarker(spec *GardenerSpec, labels []string) bool {
	if spec != nil && spec.Standing {
		return true
	}
	return slices.Contains(labels, standingGardenerLabel)
}

// gardenerIsStanding reports whether a gardener trigger is a standing (perpetual)
// gardener, for which exhausting the resume budget on zero-progress runs is
// normal steady state rather than a stuck loop to give up on. A gardener is
// standing when it carries a machine-readable standing marker (see
// gardenerHasStandingMarker).
//
// NOTE (bug ka-0000004311): this reads only MACHINE-READABLE markers, never
// terminal_goal PROSE. A gardener that self-describes as "STANDING …" in prose
// but carries neither marker is NOT recognized here — the engine treats it as
// epic-bounded and the max_runs ceiling / resume give-up path reap it. That
// prose/marker mismatch is caught at authoring time (fail-closed reject) and at
// firing time (warn-with-grace) by ValidateGardenerStandingMarker /
// warnStandingProseWithoutMarker, rather than by silently widening this
// discriminator to parse prose (an adjectival "standing warm node" must not
// flip a genuinely epic-bounded gardener to perpetual).
func gardenerIsStanding(trig TriggerDef) bool {
	return gardenerHasStandingMarker(trig.Gardener, trig.Labels)
}

// GardenerIsStanding is the exported view over gardenerIsStanding for out-of-package
// callers that render gardener surfaces (beads-viewer). It shares the SINGLE
// standing-marker definition (gardenerHasStandingMarker) so a surface can never
// disagree with the engine on which gardeners are perpetual — the same guarantee
// gardenerIsStanding and ValidateGardenerStandingMarker already rely on. Added for
// the run-budget burn meter, which must be suppressed for a standing gardener
// (its run_budget/max_runs are non-enforced accounting, so a burn bar reads as a
// false "about to stop / runaway" signal — bug ka-0000006238).
func GardenerIsStanding(trig TriggerDef) bool {
	return gardenerIsStanding(trig)
}
