package main

// plans_tree.go — the ZOOMABLE FOREST: `?view=tree` on /plans, the third view
// beside the Jira board (?view= default) and the historical grid (?view=grid).
//
// Matt's ask (Slack thread ka-0000005850, plan ka-0000002627): steal the
// forest→tree→node navigation from pines (github.com/edeng23/pines) — "yeah,
// zoomable forest, that's it." The noun stays PLAN (a plan is the thing you
// approve and build; trees just grow); what we steal is the VIEW.
//
// WHY THIS VIEW EXISTS. The plan data model is ALREADY a tree — plan epic →
// phase sub-epics (fields.phase_order) → steps (fields.step_order) — and every
// existing surface flattens it: the board answers "what LANE is this plan in",
// the card's mini task-grid (plan_grid.go) answers "how many squares of each
// color". Neither answers "what SHAPE is this plan right now" — how deep, how
// wide, where the work is bunched, which limb is wedged — and that shape is the
// thing an operator reads at a glance across a whole portfolio.
//
// COSTS NOTHING NEW. buildPlanTree is a PURE function of the planRowView the
// enrich pass already produced — specifically the SAME []planGridPhase
// buildPlanGrid materialized for the card's mini-grid. So the forest renders
// precisely the steps the card, the progress bar, the tally and the detail
// timeline render, in the same order and with the same #N, for ZERO extra
// daemon round-trips and zero new stored field. Two derivations of one truth
// (advice ka-0000005924): if the forest and the grid ever disagreed, one of
// them would be lying — they cannot, because they read the same slice.
//
// ZOOM IS PURE CSS. Every tree is emitted into the SAME normalized viewBox
// (planTreeViewW × planTreeViewH), so "zoom" is a tile-size + chrome-visibility
// change on the container, not a re-layout and not a re-fetch. Canopy (dense,
// glyph-only), grove (the default), and tree (roomy, in-SVG phase labels) are
// three CSS classes over one server-rendered DOM. plans_tree.js flips the class
// and syncs ?zoom= so a zoom level is linkable; with JS off the server-rendered
// ?zoom= still lands, so the control degrades rather than breaks.

import (
	"math"
	"strings"
)

// The normalized SVG coordinate space every tree is laid out in. Fixed, so a
// 64px canopy glyph and a 260px tree are the same markup at different CSS
// sizes — the zoom control never triggers a server round-trip or a re-layout.
const (
	planTreeViewW = 100.0
	planTreeViewH = 120.0

	// planTreeGroundY is the baseline the trunk stands on; planTreeApexY is the
	// virtual apex the trunk always reaches, so trees of wildly different phase
	// counts share one silhouette envelope and read as a FOREST rather than a
	// ragged row of unrelated glyphs.
	planTreeGroundY = 116.0
	planTreeApexY   = 20.0
	planTreeTrunkX  = planTreeViewW / 2
)

// planTreeMaxBranches / planTreeMaxLeaves cap what one glyph draws. Past the cap
// the tree would stop being readable (branches thinner than their stroke, leaves
// overlapping into a blob) and start lying about density. NOTHING is silently
// dropped: the overflow counts ride on the view model and the template renders
// them as an explicit "+N" marker, so a clipped tree announces its own clipping
// (advice ka-0000004800 — a truncated view is not absence).
const (
	planTreeMaxBranches = 9
	planTreeMaxLeaves   = 12
)

// planTreeLabelBudget is how many runes an in-SVG limb label may paint.
//
// SVG <text> neither wraps nor ellipsizes, and the tile deliberately does not
// clip (the limbs themselves need to overhang), so a long phase name renders as
// a straight run of glyphs across whatever is next to it. At tree zoom a tile is
// ~240px and the label font is 5 user units in a 100-unit viewBox — so ~12px per
// glyph, ~20 glyphs to a tile width, and the label is centered on the limb
// midpoint (half a tile from either edge). 18 runes plus the ellipsis is the
// budget that fits; anything longer is clipped HERE rather than painted over a
// neighbour. The full title survives on the tooltip, so nothing is lost.
const planTreeLabelBudget = 18

// planTreeShortLabel renders a phase's tag + name clipped to the paint budget.
// Truncation is rune-wise (a multi-byte phase name must not be cut mid-rune) and
// marked with an ellipsis so a clipped label reads as clipped rather than as a
// phase that happens to be named "Metrics & observability (folds in ka-ZHwy9ZHk".
func planTreeShortLabel(tag, label string) string {
	s := strings.TrimSpace(label)
	if tag != "" && s != "" {
		s = tag + " " + s
	} else if tag != "" {
		s = tag
	}
	if s == "" {
		return ""
	}
	r := []rune(s)
	if len(r) <= planTreeLabelBudget {
		return s
	}
	return strings.TrimRight(string(r[:planTreeLabelBudget]), " ") + "…"
}

// planTreeLeaf is one STEP: a dot on a branch, colored by the step bead's own
// lifecycle status (the same data-status vocabulary the card's mini-grid
// squares use, so one CSS palette serves both surfaces). Number is the step's
// 1..N position within its phase — the same #N sortStepBeads assigns on the
// detail timeline, so a leaf's tooltip agrees with the cockpit.
type planTreeLeaf struct {
	ID     string
	Title  string
	Status string
	Number int
	// CX/CY are the leaf center in the normalized viewBox; R its radius (which
	// shrinks as a branch gets crowded so a 12-step phase still reads as
	// distinct leaves rather than one smear).
	CX float64
	CY float64
	R  float64
}

// planTreeBranch is one PHASE (a phase sub-epic in the plan's phase_order) — or
// the unphased flat-step group, which renders as a branch with no label. The
// branch is a straight limb from the trunk out to (EndX, EndY); its leaves ride
// along the outer 65% of it.
type planTreeBranch struct {
	// Title is the phase sub-epic's untouched title (the tooltip); Tag/Label are
	// splitPhaseLabel's compact ordinal ("P1") + distinguishing remainder, so the
	// in-SVG label at tree zoom shows the NAME rather than boilerplate.
	Title string
	Tag   string
	Label string
	// ShortLabel is Tag+Label clipped to planTreeLabelBudget runes — what the
	// in-SVG limb label actually PAINTS at tree zoom. SVG <text> does not wrap or
	// ellipsize and the tile does not clip it, so an unbudgeted 44-char phase name
	// renders as a run of glyphs straight across its neighbours (MEASURED before
	// this existed: 137/158 labels overflowed, the worst by 334px — wider than a
	// whole tile). The untruncated Title still rides the label's own <title>
	// tooltip and the row tooltip, so the clip costs no information.
	ShortLabel string
	// LabelX/LabelY anchor the label over the limb's MIDPOINT, text-anchor=middle,
	// so it spreads symmetrically about the limb instead of streaming outward from
	// the tip. A tip-anchored label had almost no room on a long lower limb (whose
	// tip sits within ~6 units of the viewBox edge); the midpoint always has half
	// the trunk-to-tip run on either side of it.
	LabelX float64
	LabelY float64
	// StartX/StartY is the trunk attach point, EndX/EndY the limb tip.
	StartX float64
	StartY float64
	EndX   float64
	EndY   float64
	// Side is -1 for a left-hand limb, +1 for a right-hand one. The template
	// uses it to anchor the in-SVG phase label on the correct side.
	Side   int
	Leaves []planTreeLeaf
	// LeafOverflow is how many of the phase's steps did NOT get a leaf
	// (len(Squares) - planTreeMaxLeaves, 0 when nothing was clipped). Rendered
	// as an explicit "+N" at the limb tip — never silently swallowed.
	LeafOverflow int
	// Empty marks a phase sub-epic with no steps yet: a bare limb, no leaves.
	// The plan's phase STRUCTURE stays visible before any step lands, exactly as
	// the mini-grid renders an empty phase as a labeled square-less row.
	Empty bool
}

// planTreeView is one PLAN rendered as a tree. It is a pure projection of a
// planRowView — no daemon call, no clock read — so it is trivially table-testable
// and the forest can never disagree with the card it was derived from.
type planTreeView struct {
	ID      string
	Title   string
	Project string
	Status  string
	URL     string
	// Attention is the planAttention tier as a CSS-safe token
	// ("decision"/"escalation"/"ready"/"stalled"/"closeout"/""). It tints the
	// CROWN — the one signal that must survive all the way down to a 64px canopy
	// glyph, because at that size the whole point is spotting which tree in the
	// stand is on fire without reading a word.
	Attention string
	// TrunkTopY is where the trunk stops (the apex); the trunk always spans
	// ground → apex so the stand shares one silhouette envelope.
	TrunkTopY float64
	TrunkX    float64
	GroundY   float64
	Branches  []planTreeBranch
	// BranchOverflow is how many phases did NOT get a limb. Rendered explicitly.
	BranchOverflow int
	// Step rollup, carried through so the tile caption can state the numbers the
	// shape only implies. These are the row's OWN already-computed counts — the
	// forest re-derives nothing.
	StepsTotal  int
	StepsClosed int
	PctDone     int
	// CrewWorking is the live-crew count; a non-zero value lights a pulsing dot
	// at the trunk base — "something is grinding on this tree RIGHT NOW", the
	// second signal that must survive to canopy zoom.
	CrewWorking int
	// Bare marks a plan with no rendered steps at all: a trunk and no limbs. It
	// still draws (a seedling reads as "planned, nothing grown yet") rather than
	// vanishing from the stand.
	Bare bool
}

// HasCrew reports whether the live-crew dot should render.
func (t planTreeView) HasCrew() bool { return t.CrewWorking > 0 }

// planTreeAttention maps a row's attention tier to the CSS token that tints the
// crown. It reads the SAME planAttention the board lanes and the deck rank on,
// so the forest's "which tree needs me" and the board's "which lane is this in"
// are one derivation, not two.
func planTreeAttention(r planRowView) string {
	switch planAttention(r) {
	case planAttentionDecision:
		return "decision"
	case planAttentionEscalation:
		return "escalation"
	case planAttentionReady:
		return "ready"
	case planAttentionStalled:
		return "stalled"
	case planAttentionCloseout:
		return "closeout"
	default:
		return ""
	}
}

// planTreeLeafRadius shrinks the leaf as its branch gets crowded, so a 12-step
// phase reads as twelve distinct dots instead of one smear, while a 2-step
// phase gets fat legible leaves.
func planTreeLeafRadius(n int) float64 {
	switch {
	case n <= 4:
		return 3.4
	case n <= 7:
		return 2.9
	case n <= 10:
		return 2.4
	default:
		return 2.0
	}
}

// buildPlanTree projects one enriched plan row into its tree glyph.
//
// LAYOUT. The trunk is vertical at planTreeTrunkX, ground → apex, ALWAYS —
// every tree in the stand shares that envelope, which is what makes a wall of
// them read as a forest. The n branches are spread evenly between ground and
// apex, bottom-first (phase 1 lowest — a tree grows upward, so reading bottom-up
// is reading the plan in phase order), alternating left/right. Lower limbs are
// longer than upper ones, which is both how conifers actually look and a useful
// lie-free bias: the earliest phase is the widest part of the silhouette.
//
// Leaves ride the outer 65% of a limb in a slight zigzag (±perpendicular
// offset), which packs more of them legibly than a single-file line and gives
// the glyph its needled look at canopy zoom.
func buildPlanTree(r planRowView, basePath string) planTreeView {
	t := planTreeView{
		ID:          r.ID,
		Title:       r.Title,
		Project:     r.Project,
		Status:      r.Status,
		URL:         basePath + "/plans/" + r.ID,
		Attention:   planTreeAttention(r),
		TrunkX:      planTreeTrunkX,
		GroundY:     planTreeGroundY,
		TrunkTopY:   planTreeApexY,
		StepsTotal:  r.StepsTotal,
		StepsClosed: r.StepsClosed,
		PctDone:     r.PctDone,
		CrewWorking: r.Crew.Working,
	}

	phases := r.Grid
	if len(phases) > planTreeMaxBranches {
		t.BranchOverflow = len(phases) - planTreeMaxBranches
		phases = phases[:planTreeMaxBranches]
	}
	if len(phases) == 0 {
		t.Bare = true
		return t
	}

	n := len(phases)
	// Evenly spread n limbs strictly between ground and apex: limb i sits at
	// ground - (i+1)*span/(n+1), so neither the lowest limb nor the highest ever
	// touches the ends and the spread is the same shape for n=1 and n=9.
	span := planTreeGroundY - planTreeApexY
	step := span / float64(n+1)

	for i, ph := range phases {
		side := 1
		if i%2 == 0 {
			side = -1
		}
		y := planTreeGroundY - float64(i+1)*step

		// Lower limbs longer than upper ones — the conifer taper. The factor runs
		// 1.0 at the base to ~0.45 at the top.
		taper := 1.0
		if n > 1 {
			taper = 1.0 - 0.55*float64(i)/float64(n-1)
		}
		length := 14.0 + 30.0*taper
		// Limbs rise as they go out; the rise is a fixed fraction of the run so a
		// short upper limb keeps the same angle as a long lower one.
		dx := float64(side) * length
		dy := -0.34 * length

		br := planTreeBranch{
			Title:      ph.Title,
			Tag:        ph.PhaseTag,
			Label:      ph.PhaseName,
			ShortLabel: planTreeShortLabel(ph.PhaseTag, ph.PhaseName),
			StartX:     planTreeTrunkX,
			StartY:     y,
			EndX:       planTreeTrunkX + dx,
			EndY:       y + dy,
			Side:       side,
			// Anchored on the limb midpoint (text-anchor=middle in the template).
			LabelX: planTreeTrunkX + dx/2,
			LabelY: y + dy/2,
		}

		squares := ph.Squares
		if len(squares) > planTreeMaxLeaves {
			br.LeafOverflow = len(squares) - planTreeMaxLeaves
			squares = squares[:planTreeMaxLeaves]
		}
		if len(squares) == 0 {
			br.Empty = true
			t.Branches = append(t.Branches, br)
			continue
		}

		// Unit vectors along and perpendicular to the limb, for leaf placement.
		norm := math.Hypot(dx, dy)
		if norm == 0 {
			norm = 1
		}
		ux, uy := dx/norm, dy/norm
		px, py := -uy, ux

		radius := planTreeLeafRadius(len(squares))
		lo, hi := 0.35, 1.0
		for j, sq := range squares {
			// Single leaf sits at the tip; otherwise spread evenly over [lo, hi].
			frac := hi
			if len(squares) > 1 {
				frac = lo + (hi-lo)*float64(j)/float64(len(squares)-1)
			}
			along := frac * norm
			// Zigzag: alternate leaves to either side of the limb line.
			offset := radius * 0.95
			if j%2 == 1 {
				offset = -offset
			}
			br.Leaves = append(br.Leaves, planTreeLeaf{
				ID:     sq.ID,
				Title:  sq.Title,
				Status: sq.Status,
				Number: sq.Number,
				CX:     planTreeTrunkX + ux*along + px*offset,
				CY:     y + uy*along + py*offset,
				R:      radius,
			})
		}
		t.Branches = append(t.Branches, br)
	}
	return t
}

// planForestStand is one labeled group of trees in the forest — the stand-level
// sectioning that keeps a 100-tree wall navigable. The forest sections by the
// SAME board lanes the ?view= default uses (planBoardLanes), so "which stand is
// this tree in" and "which lane is this card in" are one answer: switching
// between the two views never re-partitions the work.
type planForestStand struct {
	Key   string
	Label string
	Trees []planTreeView
	// Truncated mirrors the lane's own truncation note (the Done lane's bounded
	// recent-closed feed), so a clipped stand says so.
	Truncated bool
}

// planForestView is the whole ?view=tree payload: the stands, the zoom level,
// and the portfolio-level tallies the header states in words (the shape shows
// you where the work is; the numbers keep it honest).
type planForestView struct {
	Stands []planForestStand
	Zoom   string
	// Trees / Steps / StepsClosed are the portfolio rollup across every rendered
	// tree. Summed from the SAME per-row counts the tiles render, so the header
	// and the tiles are two derivations of one truth.
	Trees       int
	Steps       int
	StepsClosed int
	// NeedsHuman counts trees carrying a non-empty Attention token — the same
	// planAttention != none predicate the /deck pill and the ranker apply.
	NeedsHuman int
	// ZoomControls are the canopy/grove/tree chips, each a real URL so a zoom
	// level is linkable and the control works with JS off.
	ZoomControls []planControl
}

// Pct is the portfolio completion percentage across every rendered tree, for
// the forest header. Zero steps reads 0 rather than dividing by zero.
func (f planForestView) Pct() int {
	if f.Steps <= 0 {
		return 0
	}
	return int(math.Round(100 * float64(f.StepsClosed) / float64(f.Steps)))
}

// planZoom* are the ?zoom= levels. grove is the default: big enough to read a
// plan's shape and its caption, small enough to hold a screenful of them.
const (
	planZoomCanopy = "canopy"
	planZoomGrove  = "grove"
	planZoomTree   = "tree"
)

// normalizePlanZoom canonicalizes a raw ?zoom= value, defaulting to grove. The
// aliases are the words people actually type when they mean "further out" /
// "closer in".
func normalizePlanZoom(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case planZoomCanopy, "far", "out", "forest":
		return planZoomCanopy
	case planZoomTree, "near", "in", "close":
		return planZoomTree
	default:
		return planZoomGrove
	}
}

// buildPlanForest turns the enriched, already-ranked rows into the forest view
// model, sectioned by the SAME board lanes ?view=board renders. Pure over the
// rows (buildPlanTree does the per-plan work); no daemon call.
//
// lanes is the already-computed []planBoardLane so the two views cannot drift;
// an empty lane contributes no stand (a wall of empty headers is noise in a
// view whose entire job is spatial scanning).
func buildPlanForest(lanes []planBoardLane, zoom string, basePath string, zoomURL func(string) string) planForestView {
	f := planForestView{Zoom: normalizePlanZoom(zoom)}
	for _, lane := range lanes {
		if len(lane.Rows) == 0 {
			continue
		}
		stand := planForestStand{Key: lane.Key, Label: lane.Label, Truncated: lane.Truncated}
		for _, row := range lane.Rows {
			tree := buildPlanTree(row, basePath)
			f.Trees++
			f.Steps += tree.StepsTotal
			f.StepsClosed += tree.StepsClosed
			if tree.Attention != "" {
				f.NeedsHuman++
			}
			stand.Trees = append(stand.Trees, tree)
		}
		f.Stands = append(f.Stands, stand)
	}
	if zoomURL != nil {
		for _, z := range []struct{ key, label string }{
			{planZoomCanopy, "Canopy"},
			{planZoomGrove, "Grove"},
			{planZoomTree, "Tree"},
		} {
			f.ZoomControls = append(f.ZoomControls, planControl{
				Label:  z.label,
				URL:    zoomURL(z.key),
				Active: f.Zoom == z.key,
			})
		}
	}
	return f
}
