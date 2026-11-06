package main

import "regexp"

// Phase-mislabel hint (ka-0000002630). A /plans PHASE is a direct-child
// type=epic whose id is listed in the plan epic's fields.phase_order — minted
// by `gb plan add-phase` or the cockpit "＋ New phase" verb (plan_edit.go). A
// bead merely TITLED "Phase N" and linked as a type=task child is NOT detected
// as a phase; it renders as one more ungrouped flat step. Matt reported plans
// whose phases show as tasks (thread ka-0000004243) — an AUTHORING mistake:
// agents `kd create`d "Phase N" tasks instead of using add-phase.
//
// We do NOT auto-promote such steps into phases: phase membership is
// phase-iff-in-phase_order by deliberate design (operator decision
// ka-0000001034), and silently reinterpreting a task as an epic would fight
// that decision and could misfire on a legitimately-named step. Instead we
// surface a NON-DESTRUCTIVE hint on the timeline naming the offending steps, so
// whoever is looking at the plan can re-author them.
//
// The hint's SUGGESTED FIX is audience-split (ka-0000004472): an operator is
// looking at this in the browser, so the template points them at the in-page
// ＋ New phase button and the plan agent — never a shell command. Only a
// non-operator viewer (an agent / headless caller) is shown the
// `gb plan add-phase` CLI form, for whom the command IS the actionable path.
// The hint fires on the render surface itself, so it catches EVERY authoring
// path (kd create + kd dep add, the cockpit add-step form, a formula, …), not
// just one entry point.

// phaseTitleRe matches a flat step title that looks like a phase declaration:
// the word "Phase" (any case), optional separators, then a number — with an
// optional trailing label ("Phase 1", "phase 2 — research", "Phase 3: build").
// A bare "Phase" with no number does NOT match: the phase-vs-step confusion is
// specifically about the numbered "Phase N" convention agents reach for.
var phaseTitleRe = regexp.MustCompile(`(?i)^\s*phase\b[\s:._#-]*\d+`)

// looksLikePhaseTitle reports whether a step title reads as a "Phase N"
// declaration — the shape an agent uses when it means to create a phase
// sub-epic but files a flat task instead.
func looksLikePhaseTitle(title string) bool {
	return phaseTitleRe.MatchString(title)
}

// misfiledPhaseSteps returns the titles of flat (unphased) steps that look like
// phase declarations — the steps a plan author probably meant to mint as phase
// sub-epics via `gb plan add-phase`. Only the UNPHASED steps are considered:
// a step already grouped under a phase (in phase_order) is correctly authored
// and must not be flagged. Order and duplicates follow the input step order;
// the caller renders these verbatim in the hint. Returns nil when nothing looks
// misfiled (the common, no-hint case).
func misfiledPhaseSteps(flatSteps []planStepView) []string {
	var out []string
	for _, st := range flatSteps {
		if looksLikePhaseTitle(st.Title) {
			out = append(out, st.Title)
		}
	}
	return out
}
