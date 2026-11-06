package main

import (
	"regexp"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// phaseLabelPrefix matches the boilerplate "Phase N — " lead a phase sub-epic
// title conventionally carries ("Phase 1 — Settle the e2e-reference unknown").
// Measured on the live prod board (ka-0000005352 iter-88): 92 of 155 rendered
// phase labels carry this prefix, and it eats ~60px of the label's ~112px
// visible width (54%) inside the 239px board lane — while 132/155 labels already
// clip to an ellipsis, so the boilerplate ordinal+word crowds the DISTINGUISHING
// phase name out of view. We split the ordinal into a compact fixed-width tag
// (splitPhaseLabel) so the reclaimed width goes to the name; the untouched full
// title still rides the row tooltip, so zero information is lost. The dash class
// covers the em-dash/en-dash/hyphen variants operators actually type.
var phaseLabelPrefix = regexp.MustCompile(`^Phase\s+(\d+)\s*[—–-]\s+(.*)$`)

// splitPhaseLabel separates a phase title's boilerplate "Phase N — " lead from
// its distinguishing name. It returns a compact ordinal tag ("P1") and the
// remaining name; when the title does not carry the prefix (the "Other" flat
// trailer, a pure-flat plan's untitled row, or a hand-titled phase) it returns
// an empty tag and the title verbatim, so those rows render exactly as before.
// A prefix with an empty remainder ("Phase 3 — ") also degrades to the verbatim
// title so a tag never renders beside a blank name.
func splitPhaseLabel(title string) (tag, name string) {
	m := phaseLabelPrefix.FindStringSubmatch(title)
	if m == nil {
		return "", title
	}
	rest := strings.TrimSpace(m[2])
	if rest == "" {
		return "", title
	}
	return "P" + m[1], rest
}

// plan_grid.go — the mini task-grid rendered on each /plans index card (Matt's
// ask, ka-0000001226): "a teeny little grid of color squares … one row for each
// phase, one square for each task, appropriate coloration for
// planned/blocked/in-progress/finished, with beads preview mouseover … and
// clicking through to get the full task."
//
// The grid is built from the SAME batched child walk planIndexStepData already
// runs for the progress rollup and the now/next chip (renderedStepRollup /
// collectRenderedStepIDs), so it costs no extra daemon round-trip. Each square
// is rendered as an <a href="{base}/ka-XXX"> in plan_card.html; the shared
// preview_popover.js classifies any such anchor as a bead link and gives the
// hover preview + click-through for free — no per-square JS.

// planGridSquare is one task square in a plan card's mini task-grid: one
// issue-type step. Status is the bead's own lifecycle status
// (open/in_progress/blocked/closed/deferred), which the template maps to a
// color via a data-status attribute. Number is the 1..N position WITHIN its
// group (matching the detail-page per-group numbering, sortStepBeads), so a
// square's tooltip #N agrees with the timeline.
type planGridSquare struct {
	ID     string
	Number int
	Title  string
	Status string
}

// planGridPhase is one ROW of the mini task-grid — a phase sub-epic (Title set)
// or the unphased flat-step group (Title empty for a plan with no phases, or a
// short label for the leftover flat steps of a phased plan). Squares are the
// row's ordered task squares. An empty phase (a phase sub-epic with no issue
// children yet) still renders as a labeled, square-less row so the plan's phase
// structure reads at a glance.
type planGridPhase struct {
	Title string
	// PhaseTag / PhaseName are the Title split at its boilerplate "Phase N — "
	// lead (splitPhaseLabel, ka-0000005352 iter-88): PhaseTag is the compact
	// fixed-width ordinal ("P1"), PhaseName the distinguishing remainder. When the
	// Title carries no such prefix PhaseTag is empty and PhaseName is the Title
	// verbatim, so the "Other" trailer and hand-titled rows render unchanged. The
	// card renders the tag + name (freeing ~60px for the name in the 239px lane)
	// while the untouched Title still rides the row tooltip — zero info lost.
	PhaseTag  string
	PhaseName string
	Squares   []planGridSquare
	// IsPhase distinguishes a REAL phase row (a phase sub-epic in the plan's
	// fields.phase_order, isPhaseContainer true) from the trailing UNPHASED
	// flat-step group (the "Other" row on a phased plan, or the single untitled
	// row of a pure-flat plan). Set at build time off the same isPhaseContainer
	// gate buildPlanGrid keys the phase pass on, so the phase-position facet
	// (planPhasePosition, ka-0000005352 iter-58) can count the plan's true phase
	// count without re-deriving it from the Title (a phase could legitimately be
	// titled "Other"). False for the flat trailer and for a pure-flat plan's one
	// row, so those never inflate the phase total.
	IsPhase bool
}

// buildPlanGrid derives the mini task-grid for one plan from the batched child
// data planIndexStepData already walked. It mirrors collectRenderedStepIDs /
// renderedStepRollup membership EXACTLY — phase grandchildren in phase_order,
// then the unphased direct-child remainder — so the grid shows precisely the
// steps the detail timeline renders, in the same order and with the same #N.
//
// A phased plan yields one row per phase (in phase_order), plus a trailing
// "Other" row when unphased steps remain. An unphased plan yields a single
// untitled row of its flat steps. Returns nil when the plan renders no steps
// (the card then shows "No steps yet" and no grid).
func buildPlanGrid(planEpic *beadsapi.BeadDetail, order, directChildIDs []string, grandchildIDsBySub map[string][]string, direct, grandchildren map[string]*beadsapi.BeadDetail) []planGridPhase {
	var phases []planGridPhase
	phaseStepIDs := make(map[string]struct{})
	phaseSet := make(map[string]struct{}, len(order))

	// squaresFor materializes the issue-type children among ids in the same
	// display order buildStepRows / pickCurrentOrNextStep use (explicit
	// step_order overlay first, then creation order — sortStepBeads), so the
	// square #N matches the timeline. When record is non-nil each counted issue
	// id is added so the flat pass can exclude phase-claimed steps.
	squaresFor := func(ids []string, beads map[string]*beadsapi.BeadDetail, stepOrder []string, record map[string]struct{}) []planGridSquare {
		steps := make([]*beadsapi.BeadDetail, 0, len(ids))
		for _, id := range ids {
			c := beads[id]
			if c == nil || !isIssueType(c.Type) {
				continue
			}
			if record != nil {
				record[id] = struct{}{}
			}
			steps = append(steps, c)
		}
		if len(steps) == 0 {
			return nil
		}
		sortStepBeads(steps, stepOrder)
		out := make([]planGridSquare, 0, len(steps))
		for i, c := range steps {
			out = append(out, planGridSquare{ID: c.ID, Number: i + 1, Title: c.Title, Status: c.Status})
		}
		return out
	}

	// Phase rows, in phase_order. A phase sub-epic's steps are ordered by its
	// OWN step_order (matching pickPlanCurrentOrNextStep). An empty phase still
	// renders (labeled, square-less) so the plan's phase structure is visible.
	for _, pid := range order {
		c := direct[pid]
		if !isPhaseContainer(c) {
			continue
		}
		if _, dup := phaseSet[pid]; dup {
			continue
		}
		phaseSet[pid] = struct{}{}
		tag, name := splitPhaseLabel(c.Title)
		phases = append(phases, planGridPhase{
			Title:     c.Title,
			PhaseTag:  tag,
			PhaseName: name,
			Squares:   squaresFor(grandchildIDsBySub[pid], grandchildren, planStepOrder(c), phaseStepIDs),
			IsPhase:   true,
		})
	}

	// Unphased flat steps: direct issue children not claimed by a phase, ordered
	// by the plan epic's own step_order.
	unphased := make([]string, 0, len(directChildIDs))
	for _, cid := range directChildIDs {
		if _, isPhase := phaseSet[cid]; isPhase {
			continue
		}
		if _, inPhase := phaseStepIDs[cid]; inPhase {
			continue
		}
		unphased = append(unphased, cid)
	}
	if flat := squaresFor(unphased, direct, planStepOrder(planEpic), nil); len(flat) > 0 {
		// A pure flat plan (no phases) gets a single untitled row; a phased plan
		// with leftover flat steps labels the trailing row so it reads distinct
		// from the phases above it.
		title := ""
		if len(phases) > 0 {
			title = "Other"
		}
		// The flat trailer / pure-flat row never carries a "Phase N — " prefix, so
		// splitPhaseLabel returns ("", title) — PhaseName mirrors Title and no tag
		// renders, exactly as before.
		tag, name := splitPhaseLabel(title)
		phases = append(phases, planGridPhase{Title: title, PhaseTag: tag, PhaseName: name, Squares: flat})
	}
	return phases
}

// planStepTally is the per-status breakdown of a plan's RENDERED steps — the
// same square set buildPlanGrid emits, so it never double-counts non-rendered
// descendants and its counts always agree with renderedStepRollup. It backs the
// lifecycle + step-breakdown density on the /plans card (ka-0000000851): the card
// already shows closed/total + %, so this surfaces the two live-signal counts an
// operator scans for — how many steps are moving right now, and how many are
// wedged — without re-deriving anything from the daemon.
type planStepTally struct {
	InProgress int
	Blocked    int
}

// tallyPlanGrid counts the in_progress and blocked squares across a plan's mini
// task-grid. It reads the SAME Grid buildPlanGrid already materialized for the
// card (each square carries its bead's own lifecycle status), so the breakdown
// costs no extra daemon round-trip and counts EXACTLY the steps the card's
// progress bar and the detail timeline render. open/closed/deferred are
// intentionally not tallied here — closed/total already read off the progress
// bar, and the two "needs attention" signals (moving vs wedged) are the density
// this adds.
func tallyPlanGrid(grid []planGridPhase) planStepTally {
	var t planStepTally
	for _, phase := range grid {
		for _, sq := range phase.Squares {
			switch sq.Status {
			case "in_progress":
				t.InProgress++
			case "blocked":
				t.Blocked++
			}
		}
	}
	return t
}

// gridStepMeta flattens a plan's mini task-grid into a step-id → (title, number)
// map — the step context the live/stuck crew face avatars carry on the /plans
// index (ka-0000004037, iter-15). It reads the SAME Grid buildPlanGrid already
// materialized for the card, so a crew face's "#N" step tag matches the "#N"
// square on the same card and the "#N: title" the cockpit crew roster shows —
// and it costs no extra daemon round-trip (the crewStep type is shared with the
// cockpit roster's crewStepMeta). A step double-parented across two phases has
// two squares with the same id but different per-group #N; the LAST one iterated
// wins, which is a harmless degenerate case (the crew face still gets a valid,
// grid-consistent number). Returns nil for an empty grid.
func gridStepMeta(grid []planGridPhase) map[string]crewStep {
	if len(grid) == 0 {
		return nil
	}
	meta := make(map[string]crewStep)
	for _, phase := range grid {
		for _, sq := range phase.Squares {
			if sq.ID == "" {
				continue
			}
			meta[sq.ID] = crewStep{Title: sq.Title, Number: sq.Number}
		}
	}
	return meta
}

// planStepsNotStarted is the still-open/deferred backlog of a plan's RENDERED
// step set — the count that is neither closed, in_progress, nor blocked
// (ka-0000007226). It is the fourth lifecycle bucket the segmented progress bar
// paints gray: total − closed − inProgress − blocked. Clamped at 0 because a
// step double-parented across two phases inflates `total` (renderedStepRollup
// counts it per phase) while each status bucket counts it once, so the raw
// subtraction can go negative on that degenerate shape; the bar must never emit
// a negative flex-grow. With well-formed (single-parent) steps the four buckets
// partition total exactly, which the segment-partition test pins.
func planStepsNotStarted(total, closed, inProgress, blocked int) int {
	n := total - closed - inProgress - blocked
	if n < 0 {
		return 0
	}
	return n
}

// planPhasePosition derives a multi-phase plan's PHASE MILESTONE — which phase
// of how many the plan is currently working — from the mini task-grid already on
// the row (ka-0000005352 iter-58). The card's progress bar/label answers "how
// far through the TASKS" and the now/next chip names ONE step, but neither
// answers "which PHASE" — the coarse lifecycle milestone the mission explicitly
// asks for ("add lifecycle/step-breakdown signals"). Measured on the live board
// (2026-08-11): 3 of 16 active plans are multi-phase (4-5 phases each) via
// fields.phase_order, yet the card surfaced the phase STRUCTURE (the grid rows)
// without ever saying WHICH phase is in flight.
//
// Returns (cur, total): total is the count of REAL phase rows (IsPhase — a phase
// sub-epic in phase_order, NOT the unphased "Other" flat trailer, which is not a
// milestone). cur is the 1-based index of the CURRENT phase — the earliest phase
// (in phase_order) that still carries a non-closed square, i.e. the first phase
// not yet fully done, which is where work is or will next be. When every phase's
// squares are closed the plan is on its LAST phase's completion, so cur clamps to
// total (a fully-done multi-phase plan reads "phase M/M", not "phase M+1/M").
//
// Derived PURELY from the Grid (buildPlanGrid), so it costs zero extra daemon
// call and can never disagree with the grid squares / progress rollup the card
// already renders off the same walk. Returns (0, 0) — the render gate's
// no-facet signal — for a plan with fewer than two real phases: a single-phase
// or pure-flat plan has no phase-milestone to show, so the facet adds no ink
// there (the same "add signal only where it means something" discipline the
// priority pill and the crew facets use).
//
// A phase counts as "done" only when it HAS squares and all are closed; an EMPTY
// phase (a phase sub-epic with no issue children yet — buildPlanGrid still emits
// it as a labeled square-less row) is treated as NOT-done, so it becomes the
// current phase rather than being silently skipped — an empty upcoming phase is
// exactly "the plan hasn't reached it yet", and an empty FIRST phase reads
// "phase 1/M" (work has not started), both truthful.
func planPhasePosition(grid []planGridPhase) (cur, total int) {
	for _, ph := range grid {
		if !ph.IsPhase {
			continue // the unphased "Other" flat trailer is not a milestone phase
		}
		total++
		if cur == 0 && !phaseSquaresAllClosed(ph.Squares) {
			cur = total // first not-fully-done phase (1-based) — the phase in flight
		}
	}
	if total < 2 {
		return 0, 0 // not a multi-phase plan — no milestone to surface
	}
	if cur == 0 {
		cur = total // every phase fully done → the plan is on its last phase
	}
	return cur, total
}

// phaseSquaresAllClosed reports whether a phase row's squares are ALL closed —
// the "this phase is done" test planPhasePosition uses to advance to the next
// phase. An EMPTY phase (no squares) is NOT all-closed: a phase with no steps
// yet has not been completed, it has not been reached, so it stays the current
// phase rather than being counted as done.
func phaseSquaresAllClosed(squares []planGridSquare) bool {
	if len(squares) == 0 {
		return false
	}
	for _, sq := range squares {
		if sq.Status != "closed" {
			return false
		}
	}
	return true
}
