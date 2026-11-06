package main

import "testing"

// mkChainCard is a minimal mapsCardView for the workflow-partition tests.
func mkChainCard(id, name, goalState string) mapsCardView {
	return mapsCardView{ID: id, Name: name, GoalState: goalState, Href: "/gardener/" + id, Enabled: true}
}

func cardsByIDFrom(cards ...mapsCardView) map[string]mapsCardView {
	m := make(map[string]mapsCardView, len(cards))
	for _, c := range cards {
		m[c.ID] = c
	}
	return m
}

// TestBuildMapsWorkflows_SingleChainOrderedAndPhased: a 3-gardener chain
// a→b→c where a has handed off (a achieved) lights the a→b arrow and ignites
// b's halo; the cards are ordered predecessor→heir; phase reflects the frontier.
func TestBuildMapsWorkflows_SingleChainOrderedAndPhased(t *testing.T) {
	cards := cardsByIDFrom(
		mkChainCard("gd-a", "research", "achieved"),
		mkChainCard("gd-b", "impl", ""),
		mkChainCard("gd-c", "ux", ""),
	)
	edges := []mapsChainEdge{{Pred: "gd-a", Heir: "gd-b"}, {Pred: "gd-b", Heir: "gd-c"}}

	wfs, standalone := buildMapsWorkflows(cards, edges)
	if len(standalone) != 0 {
		t.Fatalf("expected no standalone, got %v", standalone)
	}
	if len(wfs) != 1 {
		t.Fatalf("expected 1 workflow, got %d", len(wfs))
	}
	wf := wfs[0]
	if wf.GardenerCount != 3 {
		t.Errorf("GardenerCount=%d want 3", wf.GardenerCount)
	}
	// Predecessor→heir order.
	gotOrder := []string{wf.Cards[0].ID, wf.Cards[1].ID, wf.Cards[2].ID}
	wantOrder := []string{"gd-a", "gd-b", "gd-c"}
	for i := range wantOrder {
		if gotOrder[i] != wantOrder[i] {
			t.Errorf("card order[%d]=%s want %s (full %v)", i, gotOrder[i], wantOrder[i], gotOrder)
		}
	}
	// Root is the first card; b ignited (a achieved); c not.
	if !wf.Cards[0].IsRoot {
		t.Errorf("first card should be root")
	}
	if !wf.Cards[1].HaloIgnited {
		t.Errorf("heir b should be ignited (predecessor a achieved)")
	}
	if wf.Cards[2].HaloIgnited {
		t.Errorf("heir c should NOT be ignited (predecessor b not achieved)")
	}
	// Arrow a→b lit, b→c dim.
	var litAB, dimBC bool
	for _, ar := range wf.Arrows {
		if ar.Pred == "gd-a" && ar.Heir == "gd-b" && ar.Lit {
			litAB = true
		}
		if ar.Pred == "gd-b" && ar.Heir == "gd-c" && !ar.Lit {
			dimBC = true
		}
	}
	if !litAB {
		t.Errorf("a→b arrow should be lit")
	}
	if !dimBC {
		t.Errorf("b→c arrow should be dim")
	}
	// Phase: a handed off to b, so b (position 2) is the live frontier. The
	// frontier's 1-based position == the 0-based index of the first un-ignited
	// card (c at index 2) → phase 2 of 3.
	if wf.Phase != 2 {
		t.Errorf("Phase=%d want 2", wf.Phase)
	}
	if wf.Dangling {
		t.Errorf("workflow should not be dangling")
	}
	if wf.Name != "research workflow" {
		t.Errorf("Name=%q want 'research workflow'", wf.Name)
	}
}

// TestBuildMapsWorkflows_StandaloneSplit: gardeners with no chain edges land in
// standalone, not in any workflow.
func TestBuildMapsWorkflows_StandaloneSplit(t *testing.T) {
	cards := cardsByIDFrom(
		mkChainCard("gd-a", "chained-pred", ""),
		mkChainCard("gd-b", "chained-heir", ""),
		mkChainCard("gd-z", "lonely", ""),
	)
	edges := []mapsChainEdge{{Pred: "gd-a", Heir: "gd-b"}}

	wfs, standalone := buildMapsWorkflows(cards, edges)
	if len(wfs) != 1 {
		t.Fatalf("expected 1 workflow, got %d", len(wfs))
	}
	if len(standalone) != 1 || standalone[0] != "gd-z" {
		t.Fatalf("expected standalone [gd-z], got %v", standalone)
	}
}

// TestBuildMapsWorkflows_DanglingHeir: an edge to an off-board heir keeps the
// edge as a dangling ghost so the broken link is visible.
func TestBuildMapsWorkflows_DanglingHeir(t *testing.T) {
	cards := cardsByIDFrom(mkChainCard("gd-a", "pred", "achieved"))
	edges := []mapsChainEdge{{Pred: "gd-a", Heir: "gd-ghost001"}}

	wfs, _ := buildMapsWorkflows(cards, edges)
	if len(wfs) != 1 {
		t.Fatalf("expected 1 workflow, got %d", len(wfs))
	}
	wf := wfs[0]
	if !wf.Dangling {
		t.Errorf("workflow should be flagged dangling")
	}
	if wf.GardenerCount != 1 {
		t.Errorf("GardenerCount=%d want 1 (only the real predecessor)", wf.GardenerCount)
	}
	var foundGhost bool
	for _, c := range wf.Cards {
		if c.ID == "gd-ghost001" && c.DanglingHeir {
			foundGhost = true
		}
	}
	if !foundGhost {
		t.Errorf("expected a dangling ghost card for gd-ghost001, cards=%+v", wf.Cards)
	}
}

// TestBuildMapsWorkflows_NoEdges: with no edges every card is standalone and no
// workflows are produced (board renders exactly as before C2).
func TestBuildMapsWorkflows_NoEdges(t *testing.T) {
	cards := cardsByIDFrom(mkChainCard("gd-a", "a", ""), mkChainCard("gd-b", "b", ""))
	wfs, standalone := buildMapsWorkflows(cards, nil)
	if len(wfs) != 0 {
		t.Errorf("expected 0 workflows, got %d", len(wfs))
	}
	if len(standalone) != 2 {
		t.Errorf("expected 2 standalone, got %v", standalone)
	}
}

// TestBuildMapsWorkflows_CycleStillRenders: a cycle a→b→a (which C1's guard
// refuses at authoring time, but the renderer never trusts) still renders every
// node rather than dropping the component.
func TestBuildMapsWorkflows_CycleStillRenders(t *testing.T) {
	cards := cardsByIDFrom(mkChainCard("gd-a", "a", ""), mkChainCard("gd-b", "b", ""))
	edges := []mapsChainEdge{{Pred: "gd-a", Heir: "gd-b"}, {Pred: "gd-b", Heir: "gd-a"}}
	wfs, _ := buildMapsWorkflows(cards, edges)
	if len(wfs) != 1 {
		t.Fatalf("expected 1 workflow, got %d", len(wfs))
	}
	if wfs[0].GardenerCount != 2 {
		t.Errorf("cycle dropped nodes: GardenerCount=%d want 2", wfs[0].GardenerCount)
	}
}

// TestApplyMapsChainEdges_MovesChainedOutOfGrid: chained cards leave the
// standalone grid and appear under Workflows; unchained cards stay in Cards in
// their original order.
func TestApplyMapsChainEdges_MovesChainedOutOfGrid(t *testing.T) {
	board := mapsBoardView{
		Cards: []mapsCardView{
			mkChainCard("gd-a", "a", "achieved"),
			mkChainCard("gd-b", "b", ""),
			mkChainCard("gd-z", "z", ""),
		},
		Total: 3,
	}
	edges := []mapsChainEdge{{Pred: "gd-a", Heir: "gd-b"}}

	got := applyMapsChainEdges(board, edges)
	if got.WorkflowCount != 1 {
		t.Fatalf("WorkflowCount=%d want 1", got.WorkflowCount)
	}
	if len(got.Cards) != 1 || got.Cards[0].ID != "gd-z" {
		t.Errorf("standalone grid should be [gd-z], got %+v", got.Cards)
	}
}

// TestApplyMapsChainEdges_NoEdgesNoop: with no edges the board is unchanged.
func TestApplyMapsChainEdges_NoEdgesNoop(t *testing.T) {
	board := mapsBoardView{Cards: []mapsCardView{mkChainCard("gd-a", "a", "")}, Total: 1}
	got := applyMapsChainEdges(board, nil)
	if len(got.Workflows) != 0 || len(got.Cards) != 1 {
		t.Errorf("expected no-op, got workflows=%d cards=%d", len(got.Workflows), len(got.Cards))
	}
}
