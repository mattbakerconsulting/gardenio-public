package main

// Tests for the /plans zoomable forest (?view=tree; plans_tree.go,
// plan_tree.html, thread ka-0000005850). Two halves:
//
//   - the PURE layout engine (buildPlanTree / buildPlanForest): geometry
//     invariants, the palette/order agreement with the mini task-grid, and the
//     no-silent-truncation guarantee.
//   - the RENDER path (GET /plans?view=tree): the view chip, the forest branch,
//     the zoom control, and the board's non-regression.

import (
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

// treeRow builds an enriched row with a phase/step grid, the shape
// buildPlanGrid emits.
func treeRow(id string, phases ...planGridPhase) planRowView {
	r := planRowView{ID: id, Title: "Plan " + id, Status: "in_progress", Grid: phases}
	for _, ph := range phases {
		for _, sq := range ph.Squares {
			r.StepsTotal++
			if sq.Status == "closed" {
				r.StepsClosed++
			}
		}
	}
	if r.StepsTotal > 0 {
		r.PctDone = int(math.Round(100 * float64(r.StepsClosed) / float64(r.StepsTotal)))
	}
	return r
}

func treePhase(title string, statuses ...string) planGridPhase {
	tag, name := splitPhaseLabel(title)
	ph := planGridPhase{Title: title, PhaseTag: tag, PhaseName: name, IsPhase: title != ""}
	for i, st := range statuses {
		ph.Squares = append(ph.Squares, planGridSquare{
			ID: "gd-s" + string(rune('a'+i)), Number: i + 1, Title: "Step", Status: st,
		})
	}
	return ph
}

// --- geometry invariants ----------------------------------------------------

// TestBuildPlanTree_SharedSilhouetteEnvelope is the invariant that makes a wall
// of tiles read as a FOREST rather than a ragged row of unrelated glyphs: every
// tree's trunk spans exactly ground→apex regardless of how many phases it has,
// so a 1-phase plan and a 9-phase plan are the same height.
func TestBuildPlanTree_SharedSilhouetteEnvelope(t *testing.T) {
	small := buildPlanTree(treeRow("gd-a", treePhase("Phase 1 — Only", "open")), "")
	big := treeRow("gd-b")
	for i := 0; i < 6; i++ {
		big.Grid = append(big.Grid, treePhase("Phase 1 — P", "open", "closed"))
	}
	large := buildPlanTree(big, "")

	for _, tr := range []planTreeView{small, large} {
		if tr.GroundY != planTreeGroundY || tr.TrunkTopY != planTreeApexY {
			t.Errorf("%s: trunk %v→%v, want %v→%v", tr.ID, tr.GroundY, tr.TrunkTopY, planTreeGroundY, planTreeApexY)
		}
		if tr.TrunkX != planTreeTrunkX {
			t.Errorf("%s: trunk x = %v, want %v", tr.ID, tr.TrunkX, planTreeTrunkX)
		}
	}
}

// TestBuildPlanTree_LimbsInsideViewBox proves every drawn coordinate lands
// inside the normalized viewBox. A limb tip or leaf outside it would clip
// silently at canopy zoom — the tree would LOOK like it had fewer steps.
func TestBuildPlanTree_LimbsInsideViewBox(t *testing.T) {
	row := treeRow("gd-c")
	for i := 0; i < planTreeMaxBranches; i++ {
		row.Grid = append(row.Grid, treePhase("Phase 1 — P",
			"open", "open", "in_progress", "closed", "closed", "blocked",
			"open", "closed", "closed", "deferred", "open", "closed"))
	}
	tr := buildPlanTree(row, "")
	if len(tr.Branches) != planTreeMaxBranches {
		t.Fatalf("branches = %d, want %d", len(tr.Branches), planTreeMaxBranches)
	}
	for _, br := range tr.Branches {
		if br.EndX < 0 || br.EndX > planTreeViewW || br.EndY < 0 || br.EndY > planTreeViewH {
			t.Errorf("limb tip (%v,%v) outside viewBox %vx%v", br.EndX, br.EndY, planTreeViewW, planTreeViewH)
		}
		for _, lf := range br.Leaves {
			if lf.CX-lf.R < 0 || lf.CX+lf.R > planTreeViewW || lf.CY-lf.R < 0 || lf.CY+lf.R > planTreeViewH {
				t.Errorf("leaf %s at (%v,%v) r=%v escapes viewBox", lf.ID, lf.CX, lf.CY, lf.R)
			}
		}
	}
}

// TestBuildPlanTree_PhaseOrderIsBottomUp pins the reading direction: a tree
// grows upward, so phase 1 must be the LOWEST limb. If this inverts, reading a
// tree bottom-up stops being reading the plan in phase order.
func TestBuildPlanTree_PhaseOrderIsBottomUp(t *testing.T) {
	tr := buildPlanTree(treeRow("gd-d",
		treePhase("Phase 1 — First", "open"),
		treePhase("Phase 2 — Second", "open"),
		treePhase("Phase 3 — Third", "open"),
	), "")
	if len(tr.Branches) != 3 {
		t.Fatalf("branches = %d, want 3", len(tr.Branches))
	}
	for i := 1; i < len(tr.Branches); i++ {
		if tr.Branches[i].StartY >= tr.Branches[i-1].StartY {
			t.Errorf("phase %d attaches at y=%v, not ABOVE phase %d at y=%v",
				i+1, tr.Branches[i].StartY, i, tr.Branches[i-1].StartY)
		}
	}
	if tr.Branches[0].Label != "First" || tr.Branches[0].Tag != "P1" {
		t.Errorf("lowest limb = %q/%q, want P1/First", tr.Branches[0].Tag, tr.Branches[0].Label)
	}
	// Limbs alternate sides so a 3-phase tree is not a one-sided comb.
	if tr.Branches[0].Side == tr.Branches[1].Side {
		t.Errorf("adjacent limbs on the same side (%d, %d)", tr.Branches[0].Side, tr.Branches[1].Side)
	}
}

// TestPlanTreeShortLabel_PaintBudget guards the defect this caught in live
// rendering: SVG <text> neither wraps nor ellipsizes and the tile does not clip
// it, so an unbudgeted phase name paints straight across its NEIGHBOURING tiles.
// Measured against the real prod board before the budget existed: 137 of 158
// limb labels overflowed, the worst by 334px — wider than a whole 240px tile.
func TestPlanTreeShortLabel_PaintBudget(t *testing.T) {
	long := "Metrics & observability (folds in gd-ZHwy9ZHk)"
	got := planTreeShortLabel("P3", long)
	if r := []rune(got); len(r) > planTreeLabelBudget+1 {
		t.Errorf("short label %q is %d runes, over the %d budget (+ellipsis)", got, len(r), planTreeLabelBudget)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("a clipped label must READ as clipped, got %q", got)
	}
	if !strings.HasPrefix(got, "P3 ") {
		t.Errorf("the compact ordinal must survive the clip, got %q", got)
	}

	// Short enough to fit: verbatim, no ellipsis.
	if got := planTreeShortLabel("P1", "Ship it"); got != "P1 Ship it" {
		t.Errorf("a fitting label must be verbatim, got %q", got)
	}
	// No tag (the "Other" trailer / hand-titled rows) keeps the name alone.
	if got := planTreeShortLabel("", "Other"); got != "Other" {
		t.Errorf("untagged label = %q, want %q", got, "Other")
	}
	if got := planTreeShortLabel("", ""); got != "" {
		t.Errorf("empty label must stay empty (no text element), got %q", got)
	}
	// Multi-byte names must not be cut mid-rune.
	multi := planTreeShortLabel("", strings.Repeat("é", planTreeLabelBudget+9))
	if !utf8.ValidString(multi) {
		t.Errorf("clip produced invalid UTF-8: %q", multi)
	}
}

// TestBuildPlanTree_LabelAnchoredOnLimbMidpoint pins the anchor that keeps a
// centered label inside its tile: on the limb MIDPOINT, which always has half
// the trunk-to-tip run on either side of it. Anchoring at the tip (the original
// bug) left almost no room on a long lower limb, whose tip sits within a few
// units of the viewBox edge.
func TestBuildPlanTree_LabelAnchoredOnLimbMidpoint(t *testing.T) {
	tr := buildPlanTree(treeRow("gd-lbl",
		treePhase("Phase 1 — Base", "open"),
		treePhase("Phase 2 — Upper", "open"),
	), "")
	for _, br := range tr.Branches {
		wantX := (br.StartX + br.EndX) / 2
		wantY := (br.StartY + br.EndY) / 2
		if math.Abs(br.LabelX-wantX) > 0.01 || math.Abs(br.LabelY-wantY) > 0.01 {
			t.Errorf("label anchor (%v,%v), want the limb midpoint (%v,%v)", br.LabelX, br.LabelY, wantX, wantY)
		}
		// The midpoint must sit inside the viewBox with room for a centered label.
		if br.LabelX < 20 || br.LabelX > planTreeViewW-20 {
			t.Errorf("label anchor x=%v leaves no centered room inside the %v-wide viewBox", br.LabelX, planTreeViewW)
		}
	}
}

// TestBuildPlanTree_LeavesMirrorTheGrid is the agreement guarantee: the forest
// and the card's mini task-grid read the SAME []planGridPhase, so a leaf's id,
// #N and status must match its square exactly. If these ever diverge, one of
// the two surfaces is lying about the same plan (advice ka-0000005924).
func TestBuildPlanTree_LeavesMirrorTheGrid(t *testing.T) {
	row := treeRow("gd-e", treePhase("Phase 1 — Work", "open", "in_progress", "closed", "blocked"))
	tr := buildPlanTree(row, "")
	if len(tr.Branches) != 1 {
		t.Fatalf("branches = %d, want 1", len(tr.Branches))
	}
	leaves := tr.Branches[0].Leaves
	squares := row.Grid[0].Squares
	if len(leaves) != len(squares) {
		t.Fatalf("leaves = %d, squares = %d", len(leaves), len(squares))
	}
	for i := range squares {
		if leaves[i].ID != squares[i].ID || leaves[i].Number != squares[i].Number || leaves[i].Status != squares[i].Status {
			t.Errorf("leaf %d = %+v, want to mirror square %+v", i, leaves[i], squares[i])
		}
	}
}

// TestBuildPlanTree_TruncationIsAnnounced guards the no-silent-truncation rule
// (advice ka-0000004800): past the caps the tree stops DRAWING everything, but
// it must never stop SAYING so — the overflow counts are what the template
// renders as "+N".
func TestBuildPlanTree_TruncationIsAnnounced(t *testing.T) {
	row := treeRow("gd-f")
	statuses := make([]string, planTreeMaxLeaves+5)
	for i := range statuses {
		statuses[i] = "open"
	}
	for i := 0; i < planTreeMaxBranches+3; i++ {
		row.Grid = append(row.Grid, treePhase("Phase 1 — P", statuses...))
	}
	tr := buildPlanTree(row, "")

	if tr.BranchOverflow != 3 {
		t.Errorf("BranchOverflow = %d, want 3", tr.BranchOverflow)
	}
	if len(tr.Branches) != planTreeMaxBranches {
		t.Errorf("drew %d branches, want the cap %d", len(tr.Branches), planTreeMaxBranches)
	}
	for i, br := range tr.Branches {
		if br.LeafOverflow != 5 {
			t.Errorf("branch %d LeafOverflow = %d, want 5", i, br.LeafOverflow)
		}
		if len(br.Leaves) != planTreeMaxLeaves {
			t.Errorf("branch %d drew %d leaves, want the cap %d", i, len(br.Leaves), planTreeMaxLeaves)
		}
	}
}

// TestBuildPlanTree_EmptyPhaseAndBarePlan proves neither degenerate case
// vanishes: an empty phase keeps its (dashed) limb so the plan's phase
// STRUCTURE is visible before any step lands, and a plan with no steps at all
// still draws as a seedling rather than an absent tile.
func TestBuildPlanTree_EmptyPhaseAndBarePlan(t *testing.T) {
	withEmpty := buildPlanTree(treeRow("gd-g",
		treePhase("Phase 1 — Done", "closed"),
		treePhase("Phase 2 — Not started yet"),
	), "")
	if len(withEmpty.Branches) != 2 {
		t.Fatalf("branches = %d, want 2 (the empty phase must keep its limb)", len(withEmpty.Branches))
	}
	if !withEmpty.Branches[1].Empty || len(withEmpty.Branches[1].Leaves) != 0 {
		t.Errorf("empty phase = %+v, want Empty with no leaves", withEmpty.Branches[1])
	}
	if withEmpty.Bare {
		t.Error("a plan with phases must not read as Bare")
	}

	bare := buildPlanTree(planRowView{ID: "gd-h", Title: "Brand new"}, "")
	if !bare.Bare || len(bare.Branches) != 0 {
		t.Errorf("stepless plan = %+v, want Bare with no branches", bare)
	}
	// A seedling still gets the full trunk, so it occupies a real slot in the stand.
	if bare.GroundY != planTreeGroundY || bare.TrunkTopY != planTreeApexY {
		t.Error("a seedling must still draw the shared-envelope trunk")
	}
}

// TestPlanTreeAttention_MirrorsTheRanker pins the crown tint to the SAME
// planAttention the board lanes and the deck rank on — the crown is the one
// signal that must survive to a 64px canopy glyph, and it must not invent its
// own opinion about which plan needs a human.
func TestPlanTreeAttention_MirrorsTheRanker(t *testing.T) {
	dec := treeRow("gd-i", treePhase("", "open"))
	dec.OpenDecisions = 1
	esc := treeRow("gd-j", treePhase("", "open"))
	esc.Escalations = planEscalationInfo{Count: 1}
	ready := treeRow("gd-k", treePhase("", "open"))
	ready.PlanState = string(planStateReady)
	stalled := treeRow("gd-l", treePhase("", "open"))
	stalled.Stall = planStallInfo{Stalled: true}
	calm := treeRow("gd-m", treePhase("", "open"))

	for _, tc := range []struct {
		row  planRowView
		want string
	}{
		{dec, "decision"}, {esc, "escalation"}, {ready, "ready"},
		{stalled, "stalled"}, {calm, ""},
	} {
		if got := planTreeAttention(tc.row); got != tc.want {
			t.Errorf("%s: attention = %q, want %q", tc.row.ID, got, tc.want)
		}
		if got := buildPlanTree(tc.row, "").Attention; got != tc.want {
			t.Errorf("%s: tree.Attention = %q, want %q", tc.row.ID, got, tc.want)
		}
	}
}

// TestBuildPlanTree_CarriesRowCounts proves the tile caption re-derives nothing:
// the numbers come straight off the row the enrich pass already computed.
func TestBuildPlanTree_CarriesRowCounts(t *testing.T) {
	row := treeRow("gd-n", treePhase("Phase 1 — Go", "closed", "closed", "open", "open"))
	row.Crew = planCrewRollup{Total: 3, Working: 2}
	tr := buildPlanTree(row, "/base")

	if tr.StepsTotal != 4 || tr.StepsClosed != 2 || tr.PctDone != 50 {
		t.Errorf("counts = %d/%d %d%%, want 2/4 50%%", tr.StepsClosed, tr.StepsTotal, tr.PctDone)
	}
	if tr.CrewWorking != 2 || !tr.HasCrew() {
		t.Errorf("crew = %d (HasCrew %v), want 2/true", tr.CrewWorking, tr.HasCrew())
	}
	if tr.URL != "/base/plans/gd-n" {
		t.Errorf("URL = %q, want /base/plans/gd-n", tr.URL)
	}
}

// --- forest assembly --------------------------------------------------------

// TestBuildPlanForest_SectionsByBoardLanes proves the forest and the board
// partition the SAME work into the SAME columns, and that an EMPTY lane
// contributes no stand (a wall of empty headers is noise in a view whose whole
// job is spatial scanning).
func TestBuildPlanForest_SectionsByBoardLanes(t *testing.T) {
	lanes := []planBoardLane{
		{Key: "open", Label: "Open", Rows: []planRowView{treeRow("gd-o", treePhase("", "open"))}},
		{Key: "in_progress", Label: "In progress", Rows: nil},
		{Key: "closed", Label: "Done", Rows: []planRowView{treeRow("gd-p", treePhase("", "closed"))}, Truncated: true},
	}
	f := buildPlanForest(lanes, "", "", nil)

	if len(f.Stands) != 2 {
		t.Fatalf("stands = %d, want 2 (the empty lane must contribute none)", len(f.Stands))
	}
	if f.Stands[0].Key != "open" || f.Stands[1].Key != "closed" {
		t.Errorf("stand keys = %q/%q, want open/closed", f.Stands[0].Key, f.Stands[1].Key)
	}
	if !f.Stands[1].Truncated {
		t.Error("a clipped lane must carry its truncation note into the stand")
	}
	if f.Trees != 2 || f.Steps != 2 || f.StepsClosed != 1 || f.Pct() != 50 {
		t.Errorf("rollup = %d trees, %d/%d steps (%d%%), want 2, 1/2, 50%%",
			f.Trees, f.StepsClosed, f.Steps, f.Pct())
	}
}

// TestBuildPlanForest_NeedsHumanMatchesDeckPredicate proves the header's "N
// need you" counts exactly the trees carrying an attention tint — the same
// planAttention != none predicate /deck and countPlansNeedingHuman apply.
func TestBuildPlanForest_NeedsHumanMatchesDeckPredicate(t *testing.T) {
	dec := treeRow("gd-q", treePhase("", "open"))
	dec.OpenDecisions = 1
	rows := []planRowView{dec, treeRow("gd-r", treePhase("", "open"))}
	f := buildPlanForest([]planBoardLane{{Key: "open", Label: "Open", Rows: rows}}, "", "", nil)

	if f.NeedsHuman != 1 {
		t.Errorf("NeedsHuman = %d, want 1", f.NeedsHuman)
	}
	if want := countPlansNeedingHuman(rows); f.NeedsHuman != want {
		t.Errorf("NeedsHuman = %d but countPlansNeedingHuman = %d — the two must agree", f.NeedsHuman, want)
	}
}

// TestBuildPlanForest_PctZeroSteps guards the divide-by-zero: an all-seedling
// forest reads 0%, not a panic or a NaN.
func TestBuildPlanForest_PctZeroSteps(t *testing.T) {
	f := buildPlanForest([]planBoardLane{
		{Key: "open", Label: "Open", Rows: []planRowView{{ID: "gd-s", Title: "Seedling"}}},
	}, "", "", nil)
	if f.Pct() != 0 {
		t.Errorf("Pct = %d, want 0", f.Pct())
	}
	if f.Trees != 1 || !f.Stands[0].Trees[0].Bare {
		t.Error("a stepless plan must still be counted and drawn as a seedling")
	}
}

// TestNormalizePlanZoom covers the canonical tokens, the friendly aliases
// people actually type, and the grove default.
func TestNormalizePlanZoom(t *testing.T) {
	for raw, want := range map[string]string{
		"":         planZoomGrove,
		"grove":    planZoomGrove,
		"nonsense": planZoomGrove,
		"canopy":   planZoomCanopy,
		"forest":   planZoomCanopy,
		"  OUT  ":  planZoomCanopy,
		"tree":     planZoomTree,
		"Close":    planZoomTree,
	} {
		if got := normalizePlanZoom(raw); got != want {
			t.Errorf("normalizePlanZoom(%q) = %q, want %q", raw, got, want)
		}
	}
}

// TestBuildPlanForest_ZoomControls proves the three chips are real, active-
// flagged links — the progressive-enhancement contract: with JS off the ?zoom=
// URL is what makes the control work at all.
func TestBuildPlanForest_ZoomControls(t *testing.T) {
	f := buildPlanForest(
		[]planBoardLane{{Key: "open", Label: "Open", Rows: []planRowView{treeRow("gd-t", treePhase("", "open"))}}},
		planZoomCanopy, "", func(z string) string { return "/plans?zoom=" + z },
	)
	if len(f.ZoomControls) != 3 {
		t.Fatalf("zoom controls = %d, want 3", len(f.ZoomControls))
	}
	var active []string
	for _, c := range f.ZoomControls {
		if c.URL == "" {
			t.Errorf("chip %q has no URL — the no-JS path depends on it", c.Label)
		}
		if c.Active {
			active = append(active, c.Label)
		}
	}
	if len(active) != 1 || active[0] != "Canopy" {
		t.Errorf("active chips = %v, want exactly [Canopy]", active)
	}
}

// --- ?view= plumbing --------------------------------------------------------

// TestNormalizePlanView_Tree proves the new token and its aliases resolve, and
// that the board/grid defaults are untouched.
func TestNormalizePlanView_Tree(t *testing.T) {
	for raw, want := range map[string]string{
		"tree": planViewTree, "forest": planViewTree, "TREES": planViewTree,
		"grid": planViewGrid, "": planViewBoard, "bogus": planViewBoard,
	} {
		if got := normalizePlanView(raw); got != want {
			t.Errorf("normalizePlanView(%q) = %q, want %q", raw, got, want)
		}
	}
	if !planViewLaned(planViewTree) || !planViewLaned(planViewBoard) {
		t.Error("board and tree must both be laned views")
	}
	if planViewLaned(planViewGrid) {
		t.Error("grid is not a laned view")
	}
}

// TestPlanFilterState_ZoomIsForestOnly proves ?zoom= never leaks onto a
// board/grid URL (where it would be inert state) and that it IS preserved
// across a filter switch inside the forest.
func TestPlanFilterState_ZoomIsForestOnly(t *testing.T) {
	forest := planFilterState{status: planStatusActive, view: planViewTree, zoom: planZoomCanopy, base: "/plans"}
	got := forest.chipURL(map[string]string{"project": "gardenio"})
	if !strings.Contains(got, "zoom=canopy") || !strings.Contains(got, "view=tree") {
		t.Errorf("forest chip URL = %q, want it to preserve view=tree and zoom=canopy", got)
	}

	// Grove is the default: no key.
	grove := planFilterState{status: planStatusActive, view: planViewTree, zoom: planZoomGrove, base: "/plans"}
	if strings.Contains(grove.chipURL(nil), "zoom=") {
		t.Errorf("default zoom must be omitted, got %q", grove.chipURL(nil))
	}

	// On board/grid the key is inert and must not appear.
	board := planFilterState{status: planStatusActive, view: planViewBoard, zoom: planZoomCanopy, base: "/plans"}
	if strings.Contains(board.chipURL(nil), "zoom=") {
		t.Errorf("board URL must carry no zoom, got %q", board.chipURL(nil))
	}
}

// TestPlanFilterState_ViewChips proves all three chips exist, that exactly one
// is active per view, and that the Tree chip drops ?group= (the forest sections
// by lane, so a carried group would be inert state that flips the view back to
// grid the moment the operator switches to Board).
func TestPlanFilterState_ViewChips(t *testing.T) {
	for _, view := range []string{planViewBoard, planViewGrid, planViewTree, ""} {
		st := planFilterState{status: planStatusActive, view: view, group: planGroupProject, base: "/plans"}
		c := st.controls(nil)
		if len(c.Views) != 3 {
			t.Fatalf("view %q: %d chips, want 3", view, len(c.Views))
		}
		active := 0
		for _, chip := range c.Views {
			if chip.Active {
				active++
			}
			if chip.Label == "Tree" && strings.Contains(chip.URL, "group=") {
				t.Errorf("Tree chip must clear group, got %q", chip.URL)
			}
		}
		if active != 1 {
			t.Errorf("view %q: %d active chips, want exactly 1", view, active)
		}
	}
}

// --- render path ------------------------------------------------------------

// TestPlansIndex_TreeViewRendersForest is the end-to-end proof: ?view=tree
// returns the forest markup (SVG trees, the stand sectioning, the zoom control)
// and NOT the board, and the leaves link through to their step beads.
func TestPlansIndex_TreeViewRendersForest(t *testing.T) {
	f := seedPlansFake()
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?view=tree&operated_by=", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	for _, want := range []string{
		`class="plans-forest`, // the forest container + zoom class
		`plans-tree-svg`,      // a tree actually drew
		`plans-tree-trunk`,    // ... with a trunk
		`plans-stand`,         // sectioned into stands
		`data-forest-zoom`,    // the zoom chips
		`plans_tree.js`,       // the enhancement script is loaded on this view
		`/gd-step1`,           // a leaf links through to its step bead
	} {
		if !strings.Contains(body, want) {
			t.Errorf("?view=tree body missing %q", want)
		}
	}
	if strings.Contains(body, `class="plans-board"`) {
		t.Error("?view=tree must not render the board lanes")
	}
}

// TestPlansIndex_BoardUnchangedByForest is the non-regression guard: the
// default board render must not grow forest markup, an inert ?zoom= key, or the
// forest script.
func TestPlansIndex_BoardUnchangedByForest(t *testing.T) {
	f := seedPlansFake()
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	if !strings.Contains(body, `class="plans-board"`) {
		t.Fatal("the default view must still be the board")
	}
	// Assert on MARKUP tokens, not bare class names: the forest's CSS is part of
	// plans.html's one inline stylesheet and therefore ships on every view (the
	// board's own .plans-board rules ship on a grid render for the same reason).
	// What must not appear is a rendered tree, the forest container, or the
	// enhancement script.
	for _, unwanted := range []string{`<div class="plans-forest`, `<svg class="plans-tree-svg"`, `plans_tree.js`, `zoom=`} {
		if strings.Contains(body, unwanted) {
			t.Errorf("board render leaked forest markup: %q", unwanted)
		}
	}
	// The Tree chip IS offered from the board — that is how it's discovered.
	if !strings.Contains(body, "view=tree") {
		t.Error("the board must offer the Tree view chip")
	}
}

// TestPlansIndex_TreeZoomLevelLands proves the server honors ?zoom= on its own
// — the no-JS path. Without this the zoom chips would be dead links for anyone
// whose JS did not load.
func TestPlansIndex_TreeZoomLevelLands(t *testing.T) {
	for _, tc := range []struct{ query, class string }{
		{"view=tree&zoom=canopy", "plans-forest-canopy"},
		{"view=tree&zoom=tree", "plans-forest-tree"},
		{"view=tree", "plans-forest-grove"},
		{"view=tree&zoom=bogus", "plans-forest-grove"},
	} {
		f := seedPlansFake()
		mux := mkServer(t, f)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=&"+tc.query, ""))
		if w.Code != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d", tc.query, w.Code)
		}
		if !strings.Contains(w.Body.String(), tc.class) {
			t.Errorf("%s: body missing zoom class %q", tc.query, tc.class)
		}
	}
}
