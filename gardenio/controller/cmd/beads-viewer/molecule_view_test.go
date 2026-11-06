package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// seedMoleculeFake builds a poured molecule: formula gd-recipe with three
// steps scan → build → verify (linear depends_on), molecule gd-mol1 with one
// step bead per recipe step. scan is closed (done), build is in_progress with
// an assignee + a child agent bead carrying cost, verify is open but its
// dependency (build) is not terminal → pending.
func seedMoleculeFake() *fakeDaemon {
	f := newFake()
	f.beads["gd-recipe"] = &beadsapi.BeadDetail{
		ID: "gd-recipe", Title: "Ship a release", Kind: "data", Type: "formula",
		Status: "open",
		Fields: map[string]string{
			"steps": `[
				{"id":"scan","title":"Scan the repo"},
				{"id":"build","title":"Build artifacts","depends_on":["scan"]},
				{"id":"verify","title":"Verify in staging","depends_on":["build"],"gate":{"type":"manual"}}
			]`,
		},
	}
	f.beads["gd-mol1"] = &beadsapi.BeadDetail{
		ID: "gd-mol1", Title: "Ship a release 2026.1", Kind: "issue", Type: "molecule",
		Status: "in_progress",
		Fields: map[string]string{
			"formula_id":   "gd-recipe",
			"applied_vars": `{"version":"2026.1"}`,
		},
	}
	f.beads["gd-stepA"] = &beadsapi.BeadDetail{
		ID: "gd-stepA", Title: "Scan the repo", Kind: "issue", Type: "task",
		Status: "closed",
		Fields: map[string]string{"source_formula": "gd-recipe", "source_step_id": "scan"},
	}
	f.beads["gd-stepB"] = &beadsapi.BeadDetail{
		ID: "gd-stepB", Title: "Build artifacts", Kind: "issue", Type: "task",
		Status: "in_progress", Assignee: "gardenio-build-crew-x1",
		Fields: map[string]string{"source_formula": "gd-recipe", "source_step_id": "build"},
	}
	f.beads["gd-stepC"] = &beadsapi.BeadDetail{
		ID: "gd-stepC", Title: "Verify in staging", Kind: "issue", Type: "task",
		Status: "open",
		Fields: map[string]string{"source_formula": "gd-recipe", "source_step_id": "verify"},
	}
	f.beads["gd-agentB"] = &beadsapi.BeadDetail{
		ID: "gd-agentB", Title: "gardenio-build-crew-x1", Kind: "issue", Type: "agent",
		Status: "in_progress",
	}
	f.revDeps = map[string][]beadsapi.Dependency{
		"gd-mol1": {
			{BeadID: "gd-stepA", DependsOnID: "gd-mol1", Type: "parent-child"},
			{BeadID: "gd-stepB", DependsOnID: "gd-mol1", Type: "parent-child"},
			{BeadID: "gd-stepC", DependsOnID: "gd-mol1", Type: "parent-child"},
		},
		"gd-stepB": {
			{BeadID: "gd-agentB", DependsOnID: "gd-stepB", Type: "parent-child"},
		},
	}
	f.moleculeUsageResp = &beadsapi.MoleculeUsage{
		MoleculeID:   "gd-mol1",
		TotalCostUSD: 12.34,
		Steps: []beadsapi.MoleculeStepUsage{
			{BeadID: "gd-stepA", TotalCostUSD: 2.00},
			{BeadID: "gd-agentB", TotalCostUSD: 10.34},
		},
	}
	return f
}

func moleculeViewFor(t *testing.T, f *fakeDaemon, molID string) *moleculeExecView {
	t.Helper()
	srv := NewServer(f, slog.Default(), "", "", "", nil)
	v := srv.buildMoleculeExecView(context.Background(), f.beads[molID])
	if v == nil {
		t.Fatalf("buildMoleculeExecView returned nil")
	}
	return v
}

func TestBuildMoleculeExecView_StatusFrontierAndProgress(t *testing.T) {
	v := moleculeViewFor(t, seedMoleculeFake(), "gd-mol1")

	if v.FormulaID != "gd-recipe" || v.FormulaTitle != "Ship a release" {
		t.Errorf("formula link = %q/%q, want gd-recipe/Ship a release", v.FormulaID, v.FormulaTitle)
	}
	if len(v.Steps) != 3 {
		t.Fatalf("steps = %d, want 3", len(v.Steps))
	}
	wantStatus := map[string]string{"scan": "done", "build": "in_progress", "verify": "pending"}
	for _, row := range v.Steps {
		if row.Status != wantStatus[row.StepID] {
			t.Errorf("step %s status = %q, want %q", row.StepID, row.Status, wantStatus[row.StepID])
		}
	}
	// Frontier: build is in_progress — the single "you are here" row.
	if len(v.Frontier) != 1 || v.Frontier[0].StepID != "build" {
		t.Errorf("frontier = %+v, want [build]", v.Frontier)
	}
	// Progress over the three step beads.
	if v.Total != 3 || v.Done != 1 || v.InProgress != 1 || v.Pending != 1 || v.PctDone != 33 {
		t.Errorf("progress = total %d done %d inprog %d pending %d pct %d, want 3/1/1/1/33",
			v.Total, v.Done, v.InProgress, v.Pending, v.PctDone)
	}
	// Cost: molecule total, and build's row rolls up its child agent's spend.
	if v.TotalCostUSD != "$12.34" {
		t.Errorf("total cost = %q, want $12.34", v.TotalCostUSD)
	}
	// At least one step carries cost → the Cost column renders (ka-0000005731).
	if !v.AnyStepCost {
		t.Errorf("AnyStepCost = false, want true (build row carries $10.34)")
	}
	var buildRow *moleculeStepExec
	for i := range v.Steps {
		if v.Steps[i].StepID == "build" {
			buildRow = &v.Steps[i]
		}
	}
	if buildRow == nil || buildRow.CostUSD != "$10.34" {
		t.Errorf("build row cost = %+v, want $10.34", buildRow)
	}
	// Owner surfaces on the build row.
	if len(buildRow.Beads) != 1 || buildRow.Beads[0].Assignee != "gardenio-build-crew-x1" {
		t.Errorf("build row beads = %+v, want assignee gardenio-build-crew-x1", buildRow.Beads)
	}
	// DAG present with the frontier node marked current.
	if v.DAG == nil {
		t.Fatalf("DAG = nil, want a laid-out graph")
	}
	var current []string
	for _, n := range v.DAG.Nodes {
		if n.IsCurrent {
			current = append(current, n.Step.ID)
		}
	}
	if len(current) != 1 || current[0] != "build" {
		t.Errorf("DAG current nodes = %v, want [build]", current)
	}
}

// TestBuildMoleculeExecView_NoStepCostDropsColumn pins ka-0000005731: when no
// step carries cost (the common cost-less molecule), AnyStepCost is false so the
// template drops the whole Cost column instead of drawing a 100%-empty one.
func TestBuildMoleculeExecView_NoStepCostDropsColumn(t *testing.T) {
	f := seedMoleculeFake()
	f.moleculeUsageResp = nil // no cost rollup → every step row is cost-less
	v := moleculeViewFor(t, f, "gd-mol1")

	if v.AnyStepCost {
		t.Errorf("AnyStepCost = true, want false when no step carries cost")
	}
	for _, row := range v.Steps {
		if row.CostUSD != "" {
			t.Errorf("step %s CostUSD = %q, want empty", row.StepID, row.CostUSD)
		}
	}

	// The rendered page must omit the Cost column entirely — header and cells.
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/gd-mol1", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if body := w.Body.String(); strings.Contains(body, "<th>Cost</th>") {
		t.Errorf("cost-less molecule page must not render the Cost column header")
	}
}

func TestBuildMoleculeExecView_ReadyWhenDepsDone(t *testing.T) {
	f := seedMoleculeFake()
	f.beads["gd-stepB"].Status = "closed" // build done → verify becomes ready + frontier
	v := moleculeViewFor(t, f, "gd-mol1")

	for _, row := range v.Steps {
		if row.StepID == "verify" && row.Status != "ready" {
			t.Errorf("verify status = %q, want ready once build closed", row.Status)
		}
	}
	if len(v.Frontier) != 1 || v.Frontier[0].StepID != "verify" {
		t.Errorf("frontier = %+v, want [verify]", v.Frontier)
	}
}

func TestBuildMoleculeExecView_PartialCountsAsFailed(t *testing.T) {
	f := seedMoleculeFake()
	f.beads["gd-stepB"].Status = "partial"
	v := moleculeViewFor(t, f, "gd-mol1")

	if v.Failed != 1 {
		t.Errorf("failed = %d, want 1 (partial step)", v.Failed)
	}
	// partial unblocks downstream: verify becomes ready.
	for _, row := range v.Steps {
		if row.StepID == "verify" && row.Status != "ready" {
			t.Errorf("verify status = %q, want ready after partial build", row.Status)
		}
	}
}

func TestBuildMoleculeExecView_LoopIterationsAggregate(t *testing.T) {
	f := newFake()
	f.beads["gd-recipe"] = &beadsapi.BeadDetail{
		ID: "gd-recipe", Title: "Deploy everywhere", Kind: "data", Type: "formula",
		Status: "open",
		Fields: map[string]string{
			"steps": `[{"id":"deploy-{{region}}","title":"Deploy {{region}}","loop":{"var":"region","values":["us","eu"]}}]`,
		},
	}
	f.beads["gd-mol2"] = &beadsapi.BeadDetail{
		ID: "gd-mol2", Title: "Deploy everywhere", Kind: "issue", Type: "molecule",
		Status: "in_progress",
		Fields: map[string]string{"formula_id": "gd-recipe", "applied_vars": `{}`},
	}
	f.beads["gd-dep-us"] = &beadsapi.BeadDetail{
		ID: "gd-dep-us", Title: "Deploy us", Kind: "issue", Type: "task", Status: "closed",
		Fields: map[string]string{"source_step_id": "deploy-us"},
	}
	f.beads["gd-dep-eu"] = &beadsapi.BeadDetail{
		ID: "gd-dep-eu", Title: "Deploy eu", Kind: "issue", Type: "task", Status: "in_progress",
		Fields: map[string]string{"source_step_id": "deploy-eu"},
	}
	f.revDeps = map[string][]beadsapi.Dependency{
		"gd-mol2": {
			{BeadID: "gd-dep-us", DependsOnID: "gd-mol2", Type: "parent-child"},
			{BeadID: "gd-dep-eu", DependsOnID: "gd-mol2", Type: "parent-child"},
		},
	}
	v := moleculeViewFor(t, f, "gd-mol2")

	if len(v.Steps) != 1 {
		t.Fatalf("steps = %d, want 1 aggregated loop row", len(v.Steps))
	}
	row := v.Steps[0]
	if len(row.Beads) != 2 {
		t.Fatalf("loop row beads = %d, want both iterations", len(row.Beads))
	}
	if row.Status != "in_progress" {
		t.Errorf("loop row status = %q, want in_progress (one iteration running)", row.Status)
	}
	if len(v.Extras) != 0 {
		t.Errorf("extras = %+v, want none (both iterations matched)", v.Extras)
	}
	// Progress counts each iteration.
	if v.Total != 2 || v.Done != 1 || v.InProgress != 1 {
		t.Errorf("progress = %d/%d/%d, want 2 total, 1 done, 1 in progress", v.Total, v.Done, v.InProgress)
	}
}

func TestBuildMoleculeExecView_MissingFormulaDegrades(t *testing.T) {
	f := seedMoleculeFake()
	delete(f.beads, "gd-recipe")
	v := moleculeViewFor(t, f, "gd-mol1")

	if v.FormulaError == "" {
		t.Errorf("expected a FormulaError note when the recipe is unfetchable")
	}
	if v.DAG != nil {
		t.Errorf("DAG should be nil without a recipe")
	}
	// Step beads still surface as extras so execution stays visible.
	if len(v.Extras) != 3 {
		t.Errorf("extras = %d, want all 3 step beads", len(v.Extras))
	}
	if v.Total != 3 || v.Done != 1 {
		t.Errorf("progress still computed from beads: total %d done %d, want 3/1", v.Total, v.Done)
	}
}

func TestBeadShow_MoleculeRendersExecutionView(t *testing.T) {
	mux := mkServer(t, seedMoleculeFake())
	req := httptest.NewRequest("GET", "/gd-mol1", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"Molecule execution",
		`id="molecule-exec"`,
		"You are here",                    // frontier callout
		"1/3 done",                        // progress label
		"formula-dag-svg",                 // shared recipe DAG
		"fdag-current",                    // frontier node ring
		"gd-recipe",                       // formula link
		"$12.34",                          // rolled-up total cost
		"$10.34",                          // per-step cost (build subtree)
		"<th>Cost</th>",                   // Cost column renders when a step carries cost (ka-0000005731)
		"gardenio-build-crew-x1",           // owner
		"molecule_events.js",              // live-updates script
		"data-molecule-events",            // SSE anchor
		"/molecule/gd-mol1/events",        // SSE URL
		`href="/gd-stepB"`,                // step bead link
		"Applied variables",               // vars reveal
		"2026.1",                          // bound var value
		`class="molecule-step-status status-in_progress"`, // live status chip
		`<div class="molecule-steps-scroll">`, // overflow-x container so the wide steps table scrolls in its own box, not the page (ka-0000007727)
	} {
		if !strings.Contains(body, want) {
			t.Errorf("molecule page missing %q", want)
		}
	}
	// The generic Fields dump must NOT render — molecule has a dedicated view.
	if strings.Contains(body, "<dt>formula_id</dt>") {
		t.Errorf("molecule page should not fall through to the generic fields dump")
	}
}

// TestBeadShow_MoleculeExecSideBySide pins the ka-0000006510 fix: on desktop
// (>=1024px) the molecule execution view lays the thin shrink-to-fit DAG
// overlay and the per-step execution table SIDE-BY-SIDE — the steps fill the
// band the ~250px DAG leaves empty instead of stacking below it under a ~1000px
// white void (the molecule-page analogue of the recipe-page fix
// ka-0000002500 / TestBeadShow_FormulaDAGShrinkToFit). Below 1024px the layout
// collapses back to the already-designed full-width stack (no narrow regression).
func TestBeadShow_MoleculeExecSideBySide(t *testing.T) {
	mux := mkServer(t, seedMoleculeFake())
	req := httptest.NewRequest("GET", "/gd-mol1", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	// The DAG + steps table must be wrapped in the two-pane body and its panes,
	// so the flex row layout can place them side-by-side on desktop.
	for _, want := range []string{
		`class="molecule-exec-body"`,  // flex container over DAG + steps
		`class="molecule-dag-pane"`,   // DAG (left)
		`class="molecule-steps-pane"`, // steps table (right, fills the void)
	} {
		if !strings.Contains(body, want) {
			t.Errorf("molecule exec page missing %q — DAG and steps must share a two-pane body so desktop lays them side-by-side (ka-0000006510)", want)
		}
	}
	// The side-by-side row must be gated to desktop only, so narrow keeps the
	// designed full-width stack (overflow_molecule_narrow stays 0).
	for _, want := range []string{
		"@media (min-width: 1024px)",
		"flex-direction: row",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("molecule exec styles missing %q — the row layout must be desktop-gated so narrow doesn't regress (ka-0000006510)", want)
		}
	}
}

func TestHandleMoleculeEvents_StepChangeEmitsFrame(t *testing.T) {
	f := seedMoleculeFake()
	ch := make(chan beadsapi.SSEEvent, 2)
	f.eventStreamCh = ch
	mux := mkServer(t, f)

	// A step closes: its bead lifecycle event lands.
	f.beads["gd-stepB"].Status = "closed"
	enqueuePlanEvent(t, ch, "beads.bead.updated", "gd-stepB")
	// An unrelated bead's event must not emit a frame.
	enqueuePlanEvent(t, ch, "beads.bead.updated", "gd-aaa")
	close(ch)

	got := drainAgentsEventsStream(t, mux, "/molecule/gd-mol1/events")
	if n := strings.Count(got, "event: molecule-updated"); n != 1 {
		t.Errorf("molecule-updated frames = %d, want exactly 1 (unrelated bead filtered): %s", n, got)
	}
	if !strings.Contains(got, `id="molecule-exec"`) {
		t.Errorf("the frame should carry the re-rendered execution fragment: %s", got)
	}
	// The re-render reflects the NEW state: 2 of 3 done.
	if !strings.Contains(got, "2/3 done") {
		t.Errorf("expected the fragment to reflect the step close (2/3 done): %s", got)
	}
	if f.eventStreamLastTopics != moleculeEventsTopics {
		t.Errorf("EventStream topics = %q, want %q", f.eventStreamLastTopics, moleculeEventsTopics)
	}
}

func TestHandleMoleculeEvents_DependencyAddRebuildsInterest(t *testing.T) {
	f := seedMoleculeFake()
	ch := make(chan beadsapi.SSEEvent, 2)
	f.eventStreamCh = ch
	mux := mkServer(t, f)

	// A runtime expansion adds a step under the molecule mid-flight.
	f.beads["gd-stepD"] = &beadsapi.BeadDetail{
		ID: "gd-stepD", Title: "Expansion step", Kind: "issue", Type: "task",
		Status: "open",
		Fields: map[string]string{"source_step_id": "expansion-1"},
	}
	f.revDeps["gd-mol1"] = append(f.revDeps["gd-mol1"],
		beadsapi.Dependency{BeadID: "gd-stepD", DependsOnID: "gd-mol1", Type: "parent-child"})
	ch <- beadsapi.SSEEvent{
		Event: "beads.dependency.added",
		Data:  []byte(`{"dependency":{"bead_id":"gd-stepD","depends_on_id":"gd-mol1","type":"parent-child"}}`),
	}
	close(ch)

	got := drainAgentsEventsStream(t, mux, "/molecule/gd-mol1/events")
	if !strings.Contains(got, "event: molecule-updated") {
		t.Errorf("expected a molecule-updated frame on the new step edge: %s", got)
	}
	if !strings.Contains(got, "gd-stepD") {
		t.Errorf("the frame should carry the freshly-added step: %s", got)
	}
}

func TestHandleMoleculeEvents_NonMolecule404(t *testing.T) {
	mux := mkServer(t, seedMoleculeFake())
	req := httptest.NewRequest("GET", "/molecule/gd-aaa/events", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for a non-molecule bead, got %d", w.Code)
	}
}

func TestMatchesLoopPattern(t *testing.T) {
	cases := []struct {
		pattern, src string
		want         bool
	}{
		{"deploy-{{region}}", "deploy-us-east-1", true},
		{"deploy-{{region}}", "deploy-", false}, // placeholder must match non-empty
		{"deploy-{{region}}", "verify-us", false},
		{"plain-step", "plain-step", false}, // no placeholder → exact match handled elsewhere
		{"{{i}}-of-{{n}}", "1-of-3", true},
	}
	for _, c := range cases {
		if got := matchesLoopPattern(c.pattern, c.src); got != c.want {
			t.Errorf("matchesLoopPattern(%q, %q) = %v, want %v", c.pattern, c.src, got, c.want)
		}
	}
}
