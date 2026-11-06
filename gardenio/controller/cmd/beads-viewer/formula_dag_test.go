package main

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

func stepIDs(nodes []formulaDAGNode) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.Step.ID)
	}
	return out
}

func nodeByID(t *testing.T, dag *formulaDAG, id string) formulaDAGNode {
	t.Helper()
	for _, n := range dag.Nodes {
		if n.Step.ID == id {
			return n
		}
	}
	t.Fatalf("node %q not in DAG (have %v)", id, stepIDs(dag.Nodes))
	return formulaDAGNode{}
}

func TestBuildFormulaDAG_LayersByLongestPath(t *testing.T) {
	// build → test → deploy, plus lint depending only on build: deploy must
	// land in layer 2 even though lint sits in layer 1.
	steps := []formulaStep{
		{ID: "build", Title: "Build"},
		{ID: "lint", Title: "Lint", DependsOn: []string{"build"}},
		{ID: "test", Title: "Test", DependsOn: []string{"build"}},
		{ID: "deploy", Title: "Deploy", DependsOn: []string{"test", "lint"}},
	}
	dag := buildFormulaDAG(steps, nil)
	if dag == nil {
		t.Fatal("expected DAG, got nil")
	}
	if dag.Layers != 3 {
		t.Fatalf("expected 3 layers, got %d", dag.Layers)
	}
	for id, want := range map[string]int{"build": 0, "lint": 1, "test": 1, "deploy": 2} {
		if got := nodeByID(t, dag, id).Layer; got != want {
			t.Errorf("layer(%s) = %d, want %d", id, got, want)
		}
	}
	if len(dag.Edges) != 4 {
		t.Errorf("expected 4 edges, got %d", len(dag.Edges))
	}
	if !dag.HasEdges {
		t.Error("HasEdges should be true")
	}
	// Columns advance left → right with the fixed node width + gap.
	if b, d := nodeByID(t, dag, "build"), nodeByID(t, dag, "deploy"); d.X <= b.X {
		t.Errorf("deploy.X (%v) should be right of build.X (%v)", d.X, b.X)
	}
}

func TestBuildFormulaDAG_MergesNeedsAlias(t *testing.T) {
	// parseFormulaSteps merges needs into DependsOn; the layout must see it.
	raw := `[
		{"id":"a","title":"A"},
		{"id":"b","title":"B","needs":["a"]},
		{"id":"c","title":"C","depends_on":["a"],"needs":["a","b"]}
	]`
	steps := parseFormulaSteps(raw, map[string]string{})
	if got := steps[2].DependsOn; len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("expected c deps [a b], got %v", got)
	}
	dag := buildFormulaDAG(steps, nil)
	if got := nodeByID(t, dag, "c").Layer; got != 2 {
		t.Errorf("layer(c) = %d, want 2", got)
	}
}

func TestBuildFormulaDAG_ExternalExtendsRefs(t *testing.T) {
	// A dep on a step not defined here (inherited via extends) becomes a
	// dashed phantom root instead of silently dropping the edge.
	steps := []formulaStep{
		{ID: "signoff", Title: "Sign off", DependsOn: []string{"mr-ready"}},
		{ID: "announce", Title: "Announce", DependsOn: []string{"signoff"}},
	}
	dag := buildFormulaDAG(steps, nil)
	if dag.ExternalCount != 1 {
		t.Fatalf("expected 1 external node, got %d", dag.ExternalCount)
	}
	ext := nodeByID(t, dag, "mr-ready")
	if !ext.External || ext.Layer != 0 {
		t.Errorf("external node = %+v, want External=true Layer=0", ext)
	}
	if got := nodeByID(t, dag, "signoff").Layer; got != 1 {
		t.Errorf("layer(signoff) = %d, want 1 (below the external root)", got)
	}
	if len(dag.Edges) != 2 {
		t.Errorf("expected 2 edges (external kept), got %d", len(dag.Edges))
	}
}

func TestBuildFormulaDAG_CycleFlaggedNotFatal(t *testing.T) {
	steps := []formulaStep{
		{ID: "a", DependsOn: []string{"b"}},
		{ID: "b", DependsOn: []string{"a"}},
	}
	dag := buildFormulaDAG(steps, nil)
	if dag == nil {
		t.Fatal("cycle must degrade, not return nil")
	}
	if !dag.Cyclic {
		t.Error("Cyclic flag should be set")
	}
	if len(dag.Nodes) != 2 {
		t.Errorf("both nodes should still render, got %d", len(dag.Nodes))
	}
}

func TestBuildFormulaDAG_EmptyAndOversize(t *testing.T) {
	if dag := buildFormulaDAG(nil, nil); dag != nil {
		t.Error("nil steps should yield nil DAG")
	}
	big := make([]formulaStep, formulaDAGMaxNodes+1)
	for i := range big {
		big[i] = formulaStep{ID: fmt.Sprintf("s%d", i)}
	}
	if dag := buildFormulaDAG(big, nil); dag != nil {
		t.Error("oversize graph should yield nil DAG (fall back to list)")
	}
}

func TestBuildFormulaDAG_GlyphsAndLegend(t *testing.T) {
	raw := `[
		{"id":"plain","title":"Plain task"},
		{"id":"approve","title":"Approve","gate":{"type":"decision","prompt":"go?"},"depends_on":["plain"]},
		{"id":"fanout","title":"Fan out","loop":{"var":"region","values":["us-east-1","eu-west-1"]},"depends_on":["approve"]},
		{"id":"pick","title":"Pick env","branch":{"on":"env","cases":{"prod":{"expand":"gd-x"}}},"depends_on":["approve"]},
		{"id":"nest","title":"Nested","children":[{"id":"c1","title":"C1"},{"id":"c2","title":"C2"}],"depends_on":["fanout"]},
		{"id":"defer","title":"Deferred","on_complete":{"for_each":"component","expand":"gd-y"},"depends_on":["nest"]}
	]`
	steps := parseFormulaSteps(raw, map[string]string{})
	dag := buildFormulaDAG(steps, nil)

	for id, want := range map[string]string{
		"plain":   glyphTask,
		"approve": glyphGate,
		"fanout":  glyphLoop,
		"pick":    glyphBranch,
		"nest":    glyphChildren,
		"defer":   glyphChildren,
	} {
		if got := nodeByID(t, dag, id).Glyph; got != want {
			t.Errorf("glyph(%s) = %q, want %q", id, got, want)
		}
	}

	if got := nodeByID(t, dag, "approve").KindLabel; got != "gate · decision" {
		t.Errorf("KindLabel(approve) = %q", got)
	}
	if got := nodeByID(t, dag, "fanout").KindLabel; got != "loop · region ∈ [us-east-1, eu-west-1]" {
		t.Errorf("KindLabel(fanout) = %q", got)
	}
	if got := nodeByID(t, dag, "pick").KindLabel; got != "branch · on env" {
		t.Errorf("KindLabel(pick) = %q", got)
	}
	if got := nodeByID(t, dag, "nest").KindLabel; got != "children × 2" {
		t.Errorf("KindLabel(nest) = %q", got)
	}

	// Legend lists exactly the glyphs present (no external ⬡ here).
	glyphs := map[string]bool{}
	for _, l := range dag.Legend {
		glyphs[l.Glyph] = true
	}
	for _, want := range []string{glyphTask, glyphGate, glyphLoop, glyphBranch, glyphChildren} {
		if !glyphs[want] {
			t.Errorf("legend missing glyph %q", want)
		}
	}
	if glyphs[glyphExternal] {
		t.Error("legend should not list the external glyph when none render")
	}
}

func TestBuildFormulaDAG_CurrentStepMarked(t *testing.T) {
	steps := []formulaStep{
		{ID: "a", Status: "completed"},
		{ID: "b", Status: "in_progress", DependsOn: []string{"a"}},
	}
	cur := &steps[1]
	dag := buildFormulaDAG(steps, cur)
	if !nodeByID(t, dag, "b").IsCurrent {
		t.Error("current step should be marked IsCurrent")
	}
	if nodeByID(t, dag, "a").IsCurrent {
		t.Error("non-current step must not be marked")
	}
}

// TestBuildFormulaDAG_LinearChainLaidVertical covers the ka-0000001807 fix: a
// pure single-chain recipe stacks top-to-bottom (fits the desktop pane, no
// horizontal scroll) rather than running left-to-right off the fold.
func TestBuildFormulaDAG_LinearChainLaidVertical(t *testing.T) {
	steps := []formulaStep{
		{ID: "boot", Title: "Boot"},
		{ID: "capture", Title: "Capture", DependsOn: []string{"boot"}},
		{ID: "measure", Title: "Measure", DependsOn: []string{"capture"}},
		{ID: "judge", Title: "Judge", DependsOn: []string{"measure"}},
		{ID: "file", Title: "File issues", DependsOn: []string{"judge"}},
		{ID: "report", Title: "Report", DependsOn: []string{"file"}},
	}
	dag := buildFormulaDAG(steps, nil)
	if dag == nil {
		t.Fatal("expected DAG, got nil")
	}
	if !dag.Vertical {
		t.Fatal("a 6-step linear chain should lay out vertically")
	}
	// All nodes share the single column x; each is strictly below the previous.
	boot, report := nodeByID(t, dag, "boot"), nodeByID(t, dag, "report")
	if boot.X != report.X {
		t.Errorf("vertical layout: nodes should share x (boot=%v report=%v)", boot.X, report.X)
	}
	if report.Y <= boot.Y {
		t.Errorf("report.Y (%v) should be below boot.Y (%v)", report.Y, boot.Y)
	}
	// The whole graph must fit a single node-width column, not 6 across.
	if dag.Width > int(2*formulaDAGMargin+formulaDAGNodeW) {
		t.Errorf("vertical layout width = %d, should fit one node column", dag.Width)
	}
	if dag.Height <= dag.Width {
		t.Errorf("vertical layout should be taller than wide (w=%d h=%d)", dag.Width, dag.Height)
	}
}

// linearChain builds n steps a0→a1→…→a(n-1), each depending on the previous.
func linearChain(n int) []formulaStep {
	steps := make([]formulaStep, n)
	for i := range steps {
		steps[i] = formulaStep{ID: fmt.Sprintf("a%d", i), Title: fmt.Sprintf("Step %d", i)}
		if i > 0 {
			steps[i].DependsOn = []string{fmt.Sprintf("a%d", i-1)}
		}
	}
	return steps
}

// TestWrapChainLayout covers the balanced-column arithmetic for a wrapped
// linear chain (ka-0000001826). Columns must never exceed the max-rows cap and
// must be balanced (last column at most one row short of the rest).
func TestWrapChainLayout(t *testing.T) {
	cases := []struct{ n, cols, rows int }{
		{1, 1, 1},
		{8, 1, 8},  // exactly the cap — one column
		{9, 2, 5},  // 9 → 2 cols of 5+4, not 8+1
		{11, 2, 6}, // the golden formula: 2 cols of 6+5
		{16, 2, 8}, // 2 full columns
		{17, 3, 6}, // 17 → 3 cols of 6+6+5
		{24, 3, 8}, // 3 full columns
	}
	for _, tc := range cases {
		cols, rows := wrapChainLayout(tc.n)
		if cols != tc.cols || rows != tc.rows {
			t.Errorf("wrapChainLayout(%d) = (cols=%d rows=%d), want (cols=%d rows=%d)",
				tc.n, cols, rows, tc.cols, tc.rows)
		}
		if rows > formulaDAGWrapMaxRows {
			t.Errorf("wrapChainLayout(%d): rowsPerCol %d exceeds cap %d", tc.n, rows, formulaDAGWrapMaxRows)
		}
		// Balance: cols·rows covers n, and dropping a row would leave the last
		// column with fewer than a full column of slack.
		if cols*rows < tc.n {
			t.Errorf("wrapChainLayout(%d): %d cols × %d rows = %d < n", tc.n, cols, rows, cols*rows)
		}
	}
}

// TestBuildFormulaDAG_ShortChainStaysSingleColumn guards that a chain at or
// under the wrap threshold keeps the original single top-to-bottom column
// (ka-0000001807) — the wrap fix (ka-0000001826) is purely additive.
func TestBuildFormulaDAG_ShortChainStaysSingleColumn(t *testing.T) {
	dag := buildFormulaDAG(linearChain(formulaDAGWrapThreshold), nil)
	if dag == nil || !dag.Vertical {
		t.Fatalf("an %d-step chain should be a vertical single column", formulaDAGWrapThreshold)
	}
	if dag.WrapCols > 1 {
		t.Errorf("chain of %d (== threshold) must NOT wrap, got WrapCols=%d", formulaDAGWrapThreshold, dag.WrapCols)
	}
	// One node column: width fits a single node, height is taller than wide.
	if dag.Width > int(2*formulaDAGMargin+formulaDAGNodeW) {
		t.Errorf("single-column width = %d, should fit one node", dag.Width)
	}
	xs := map[float64]bool{}
	for _, n := range dag.Nodes {
		xs[n.X] = true
	}
	if len(xs) != 1 {
		t.Errorf("single-column layout: all nodes should share one x, got %d distinct", len(xs))
	}
}

// TestBuildFormulaDAG_LongChainWrapsIntoBalancedColumns covers the
// ka-0000001826 density fix: a linear chain longer than the wrap threshold is
// laid out in balanced columns so the graph is short + wide (fills the recipe
// page's right band) instead of a tall ribbon that strands a bottom-right void.
func TestBuildFormulaDAG_LongChainWrapsIntoBalancedColumns(t *testing.T) {
	// 11 steps — the golden formula (ka-0000001189) shape that filed the bug.
	dag := buildFormulaDAG(linearChain(11), nil)
	if dag == nil || !dag.Vertical {
		t.Fatal("an 11-step chain should still be a vertical (linear) layout")
	}
	if dag.WrapCols != 2 {
		t.Fatalf("11-step chain should wrap into 2 columns, got WrapCols=%d", dag.WrapCols)
	}

	// Nodes must occupy exactly 2 distinct x (two columns) and each column's
	// rows advance top-to-bottom in dependency order.
	xs := map[float64][]formulaDAGNode{}
	for _, n := range dag.Nodes {
		xs[n.X] = append(xs[n.X], n)
	}
	if len(xs) != 2 {
		t.Fatalf("wrapped layout should use 2 columns, got %d distinct x", len(xs))
	}

	// The first 6 nodes (a0..a5) land in the left column, a6..a10 in the right.
	a0, a5 := nodeByID(t, dag, "a0"), nodeByID(t, dag, "a5")
	a6, a10 := nodeByID(t, dag, "a6"), nodeByID(t, dag, "a10")
	if a0.X != a5.X {
		t.Errorf("a0 and a5 should share the left column x (a0=%v a5=%v)", a0.X, a5.X)
	}
	if a6.X <= a0.X {
		t.Errorf("a6 (second column) should be right of a0 (a6.X=%v a0.X=%v)", a6.X, a0.X)
	}
	if a6.X != a10.X {
		t.Errorf("a6 and a10 should share the right column x (a6=%v a10=%v)", a6.X, a10.X)
	}
	// The wrap: a6 sits at the TOP of the second column, above a5's row.
	if a6.Y >= a5.Y {
		t.Errorf("a6 should sit at the top of the next column, above a5 (a6.Y=%v a5.Y=%v)", a6.Y, a5.Y)
	}

	// The wrapped graph is now WIDER than one node and SHORTER than the
	// equivalent single column — this is the whole density point.
	single := buildFormulaDAG(linearChain(11), nil) // same input; just for the single-col dims math
	_ = single
	if dag.Width <= int(2*formulaDAGMargin+formulaDAGNodeW) {
		t.Errorf("wrapped width = %d, should span more than one node column", dag.Width)
	}
	tallSingle := int(2*formulaDAGMargin + 11*formulaDAGNodeH + 10*formulaDAGVGap)
	if dag.Height >= tallSingle {
		t.Errorf("wrapped height = %d should be well under the single-column height %d", dag.Height, tallSingle)
	}

	// Every consecutive step keeps an edge (nothing dropped by the wrap).
	if len(dag.Edges) != 10 {
		t.Errorf("11-step chain should have 10 edges, got %d", len(dag.Edges))
	}
}

// TestBuildFormulaDAG_BranchingStaysHorizontal guards that a graph with any
// multi-node stage keeps the left-to-right layered layout — vertical stacking
// is only for pure linear chains.
func TestBuildFormulaDAG_BranchingStaysHorizontal(t *testing.T) {
	steps := []formulaStep{
		{ID: "build", Title: "Build"},
		{ID: "lint", Title: "Lint", DependsOn: []string{"build"}},
		{ID: "test", Title: "Test", DependsOn: []string{"build"}},
		{ID: "deploy", Title: "Deploy", DependsOn: []string{"test", "lint"}},
	}
	dag := buildFormulaDAG(steps, nil)
	if dag.Vertical {
		t.Error("a branching graph (2 nodes in stage 1) must not lay out vertically")
	}
}

// TestBuildFormulaDAG_StripsSharedTitlePrefix covers the ka-0000001807 label
// fix: audit-style recipes prefix every step title with the formula name +
// iteration, so the disambiguating tail truncates away and every node reads
// identically. The shared prefix is stripped before clipping so labels read
// distinctly; the full title survives in the tooltip.
func TestBuildFormulaDAG_StripsSharedTitlePrefix(t *testing.T) {
	steps := []formulaStep{
		{ID: "boot", Title: "Maps beauty (2026-07-21): boot and warmup"},
		{ID: "capture", Title: "Maps beauty (2026-07-21): capture", DependsOn: []string{"boot"}},
		{ID: "measure", Title: "Maps beauty (2026-07-21): measure", DependsOn: []string{"capture"}},
		{ID: "report", Title: "Maps beauty (2026-07-21): report", DependsOn: []string{"measure"}},
	}
	dag := buildFormulaDAG(steps, nil)
	labels := map[string]string{}
	for _, n := range dag.Nodes {
		labels[n.Step.ID] = n.Label
	}
	// Before the fix every label truncated to the same "Maps beauty (2026-0…".
	if labels["boot"] == labels["capture"] {
		t.Fatalf("labels should read distinctly, both are %q", labels["boot"])
	}
	if got := labels["capture"]; got != "capture" {
		t.Errorf("capture label = %q, want %q (prefix stripped)", got, "capture")
	}
	if got := labels["boot"]; !strings.HasPrefix(got, "boot") {
		t.Errorf("boot label = %q, want it to start with the disambiguating tail", got)
	}
	// The full, unstripped title still lives in the tooltip.
	if got := nodeByID(t, dag, "capture").TitleAttr; !strings.Contains(got, "Maps beauty (2026-07-21): capture") {
		t.Errorf("tooltip should keep the full title, got %q", got)
	}
}

// TestStripSharedTitlePrefix_NoOpCases guards that ordinary recipes (no shared
// prefix, single step, or a prefix whose removal would blank a label) are left
// untouched.
func TestStripSharedTitlePrefix_NoOpCases(t *testing.T) {
	cases := []struct {
		name   string
		titles []string
		want   string
	}{
		{"no common prefix", []string{"Build image", "Run tests", "Deploy"}, ""},
		{"single title", []string{"Only step"}, ""},
		{"short shared head is noise", []string{"a one", "a two"}, ""},
		{"prefix would blank a label", []string{"Deploy: ", "Deploy: run tests"}, ""},
		{"audit prefix", []string{"Maps beauty (x): boot", "Maps beauty (x): capture"}, "Maps beauty (x): "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripSharedTitlePrefix(tc.titles); got != tc.want {
				t.Errorf("stripSharedTitlePrefix(%v) = %q, want %q", tc.titles, got, tc.want)
			}
		})
	}
}

func TestParseFormulaSteps_GateDefaultsToDecision(t *testing.T) {
	steps := parseFormulaSteps(`[{"id":"g","gate":{"prompt":"ok?"}}]`, map[string]string{})
	if got := steps[0].GateType; got != "decision" {
		t.Errorf("bare gate should default to decision, got %q", got)
	}
}

func TestParseFormulaSteps_LoopModes(t *testing.T) {
	steps := parseFormulaSteps(`[
		{"id":"a","loop":{"var":"i","count":3}},
		{"id":"b","loop":{"var":"n","range":"1..5"}}
	]`, map[string]string{})
	if got := steps[0].LoopDesc; got != "i × 3" {
		t.Errorf("count loop desc = %q", got)
	}
	if got := steps[1].LoopDesc; got != "n ∈ 1..5" {
		t.Errorf("range loop desc = %q", got)
	}
}

func TestFormulaCLICommand_PlaceholdersForUnboundVars(t *testing.T) {
	vars := []formulaVar{
		{Name: "version", Value: "2026.114.1"},
		{Name: "component"},
	}
	got := formulaCLICommand("pour", "gd-frm", vars)
	want := "kd formula pour gd-frm --var version=2026.114.1 --var component=<component>"
	if got != want {
		t.Errorf("PourCommand = %q, want %q", got, want)
	}
}

// TestBeadShow_FormulaDAGRender drives the full bead-show page for a formula
// with a gate + branch + extends-inherited dep and asserts the structure SVG,
// glyph legend, side-rail actions, and raw-JSON reveal all render.
func TestBeadShow_FormulaDAGRender(t *testing.T) {
	f := newFake()
	f.beads["gd-dagfrm"] = &beadsapi.BeadDetail{
		ID:     "gd-dagfrm",
		Title:  "gated pipeline",
		Type:   "formula",
		Status: "open",
		Fields: map[string]string{
			"extends": `["gd-parentFormula"]`,
			"vars":    `[{"name":"env","description":"target env"}]`,
			"steps": `[
				{"id":"prep","type":"task","title":"Prep workspace","depends_on":["inherited-root"]},
				{"id":"approve","type":"task","title":"Operator approval","gate":{"type":"manual","timeout":"24h"},"depends_on":["prep"]},
				{"id":"ship","type":"task","title":"Ship it","branch":{"on":"env","cases":{"prod":{"expand":"gd-x"}}},"depends_on":["approve"]}
			]`,
			"step_status_prep": "completed",
			"step_bead_prep":   "gd-prep-1",
		},
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/gd-dagfrm", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	for _, want := range []string{
		`class="formula-dag-svg"`,
		glyphGate,               // gate glyph in the SVG
		glyphBranch,             // branch glyph
		glyphExternal,           // inherited-root phantom node
		"fdag-external",         // dashed styling class
		"gate · manual",         // kind label in tooltip
		"inherited via extends", // legend entry
		`href="/gd-prep-1"`,     // completed step links to its bead
		"kd formula pour gd-dagfrm --var env=&lt;env&gt;", // pour command (HTML-escaped)
		"kd formula cook gd-dagfrm",                       // cook command
		"View raw steps JSON",
		`href="/gd-parentFormula"`, // extends chip links to parent recipe
	} {
		if !strings.Contains(body, want) {
			t.Errorf("formula DAG render missing %q", want)
		}
	}
}

// TestBeadShow_FormulaDAGShrinkToFit pins the ka-0000002500 fix: the
// .formula-dag-scroll box shrinks to the graph's rendered width (width:max-content)
// instead of stretching to the full pane width, so a narrow/linear DAG (e.g. a
// vertical 6-step chain ~204px wide) doesn't sit in a full-width bordered box with
// a large right-side void. max-width:100% + overflow-x:auto are retained so a wide
// DAG still fills the pane and scrolls inside its own container (375px page-level
// overflow stays 0).
func TestBeadShow_FormulaDAGShrinkToFit(t *testing.T) {
	f := newFake()
	f.beads["gd-linfrm"] = &beadsapi.BeadDetail{
		ID:     "gd-linfrm",
		Title:  "session inspector",
		Type:   "formula",
		Status: "open",
		Fields: map[string]string{
			// A linear 6-step chain — lays out vertically as a narrow column,
			// the exact shape that exposed the right-side void.
			"steps": `[
				{"id":"pick-session","type":"task","title":"Pick a session"},
				{"id":"fetch-and-measure","type":"task","title":"Fetch and measure","depends_on":["pick-session"]},
				{"id":"judge","type":"task","title":"Judge","depends_on":["fetch-and-measure"]},
				{"id":"file-bugs","type":"task","title":"File bugs","depends_on":["judge"]},
				{"id":"report","type":"task","title":"Report","depends_on":["file-bugs"]},
				{"id":"dispatch-remediation","type":"task","title":"Dispatch remediation","depends_on":["report"]}
			]`,
		},
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/gd-linfrm", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	// The scroll box must shrink-to-fit (width:max-content) yet stay bounded
	// (max-width:100%) and keep its own horizontal scroll container.
	for _, want := range []string{
		"width: max-content",
		"max-width: 100%",
		"overflow-x: auto",
	} {
		if !strings.Contains(body, want) {
			t.Errorf(".formula-dag-scroll must declare %q so a narrow DAG hugs its graph without a right-side void", want)
		}
	}
}

// TestBeadShow_FormulaDAGPaneDensity pins the ka-0000006902 fix: a strictly-
// linear DAG must not leave a wide whitespace void to the RIGHT of its thin
// graph ribbon. This is a DENSITY-OUTCOME assertion, deliberately stronger than
// the two predecessor rule-only tests it replaces (which each passed while the
// void merely MOVED):
//
//   - ka-0000002500 / MR !5855 hugged the .formula-dag-scroll BOX
//     (width:max-content, ~204px) — void survived to the pane's right.
//   - ka-0000005917 / MR !5873 hugged the PANE (flex:0 1 auto) so the rail slid
//     left — but the step-list still stacked BELOW the ~222px ribbon, leaving a
//     measured ~555×690px 100%-white band to the ribbon's right (near_white
//     0.873 flat). The old TestBeadShow_FormulaDAGPaneHugsContent pinned ONLY
//     that flex rule, so a relocated void shipped green.
//
// The density fix lays the graph and the step-list SIDE-BY-SIDE inside a
// .formula-dag-body flex row (mirroring the molecule page, ka-0000006510): the
// step-list fills the band the thin graph leaves instead of stacking below it.
// A rule-only test could not catch a relocated void, so this asserts the whole
// structural contract that makes the two columns sit beside each other:
//
//  1. the DAG and step-list share a .formula-dag-body flex container;
//  2. that body becomes a row at desktop (>=1024px);
//  3. the step-list GROWS to fill the band (flex:1 1 <basis>) at desktop;
//  4. the graph lives in its own .formula-dag-col so it can hug while the
//     step-list is a sibling column, not a block stacked underneath it;
//  5. the pane still GROWS to fill the row beside the side-rail (a hugging
//     pane, flex:0 1 auto, is the relocated-void regression — reject it).
//
// The exact right-of-graph pixel-band measurement (no all-white run wider than
// ~400px) lives in the headless Playwright suite (formula_dag_density.spec.js),
// which renders real CSS geometry the httptest body cannot.
func TestBeadShow_FormulaDAGPaneDensity(t *testing.T) {
	f := newFake()
	f.beads["gd-linfrm2"] = &beadsapi.BeadDetail{
		ID:     "gd-linfrm2",
		Title:  "session inspector",
		Type:   "formula",
		Status: "open",
		Fields: map[string]string{
			// Same narrow/linear chain shape that exposed the void.
			"steps": `[
				{"id":"pick-session","type":"task","title":"Pick a session"},
				{"id":"fetch-and-measure","type":"task","title":"Fetch and measure","depends_on":["pick-session"]},
				{"id":"judge","type":"task","title":"Judge","depends_on":["fetch-and-measure"]},
				{"id":"file-bugs","type":"task","title":"File bugs","depends_on":["judge"]},
				{"id":"report","type":"task","title":"Report","depends_on":["file-bugs"]},
				{"id":"dispatch-remediation","type":"task","title":"Dispatch remediation","depends_on":["report"]}
			]`,
		},
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/gd-linfrm2", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()

	// (1) The graph and the step-list must share the two-column inner body, and
	//     the graph must live in its own column so it can hug while the
	//     step-list is a SIBLING (not a block stacked below it in the pane).
	if !strings.Contains(body, `<div class="formula-dag-body">`) {
		t.Errorf("structure pane must wrap the graph + step-list in a .formula-dag-body so they sit side-by-side, not stacked (ka-0000006902)")
	}
	if !strings.Contains(body, `<div class="formula-dag-col">`) {
		t.Errorf("the DAG must live in its own .formula-dag-col column so the step-list is a sibling beside it, not a block underneath (ka-0000006902)")
	}

	// (2) The body must switch to a row at desktop so the columns sit beside
	//     each other where there IS a band to fill.
	if !strings.Contains(body, "@media (min-width: 1024px)") ||
		!strings.Contains(body, ".formula-dag-body { flex-direction: row;") {
		t.Errorf(".formula-dag-body must become flex-direction:row at >=1024px so the graph and step-list are side-by-side on desktop (ka-0000006902)")
	}

	// (3) The step-list must GROW to fill the band the thin graph leaves —
	//     flex-grow:1 with min-width:0 so it takes the right-of-graph space
	//     instead of leaving it 100% white.
	if !strings.Contains(body, ".formula-step-list { flex: 1 1 340px; min-width: 0;") {
		t.Errorf(".formula-step-list must declare flex:1 1 340px (grow to fill the right-of-graph band) at desktop — a non-growing step-list leaves the void that shipped in MR !5873 (ka-0000006902)")
	}

	// (4) The pane must GROW to fill the structure row beside the side-rail. The
	//     predecessor fix's hugging pane (flex:0 1 auto) is exactly the shape
	//     that relocated the void — reject it so this can never regress back.
	if !strings.Contains(body, ".formula-dag-pane { flex: 1 1 480px; min-width: 0; }") {
		t.Errorf(".formula-dag-pane must declare flex:1 1 480px (grow to fill, keep min-width:0) so the filled step-list column has room — ka-0000006902")
	}
	if strings.Contains(body, ".formula-dag-pane { flex: 0 1 auto") {
		t.Errorf(".formula-dag-pane still declares flex:0 1 auto — the hugging pane from MR !5873 that RELOCATED the void rather than filling it (ka-0000006902)")
	}
}
