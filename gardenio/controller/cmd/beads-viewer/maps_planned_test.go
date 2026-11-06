package main

import (
	"testing"

	"gardenio/controller/internal/beadsapi"
)

func TestPlannedHeirsFromBead(t *testing.T) {
	// No chain → no planned heirs.
	if got := plannedHeirsFromBead(&beadsapi.BeadDetail{ID: "gd-x", Fields: map[string]string{}}); got != nil {
		t.Errorf("no-chain bead: planned = %v, want nil", got)
	}

	// Single heir → one planned entry, not fan-out, synthetic id namespaced.
	b := &beadsapi.BeadDetail{ID: "gd-pred", Fields: map[string]string{
		"chain": `{"to":{"phase":"impl","epic":"gd-e"}}`,
	}}
	got := plannedHeirsFromBead(b)
	if len(got) != 1 {
		t.Fatalf("single heir: planned = %d, want 1", len(got))
	}
	if got[0].FanOut {
		t.Errorf("single heir should not be FanOut")
	}
	if got[0].Label != "impl" {
		t.Errorf("label = %q, want impl", got[0].Label)
	}
	if got[0].ID != "planned:gd-pred:0" {
		t.Errorf("synthetic id = %q, want planned:gd-pred:0", got[0].ID)
	}

	// Fan-out → each entry FanOut, deterministic ids.
	b2 := &beadsapi.BeadDetail{ID: "gd-pred2", Fields: map[string]string{
		"chain": `{"to":[{"phase":"a"},{"phase":"b"}]}`,
	}}
	got2 := plannedHeirsFromBead(b2)
	if len(got2) != 2 {
		t.Fatalf("fan-out: planned = %d, want 2", len(got2))
	}
	if !got2[0].FanOut || !got2[1].FanOut {
		t.Errorf("fan-out entries should be FanOut")
	}
	if got2[1].ID != "planned:gd-pred2:1" {
		t.Errorf("second synthetic id = %q, want planned:gd-pred2:1", got2[1].ID)
	}
}

// maps_planned_test.go — planned-successor (pending) rail coverage (task
// ka-0000005273 / "C3"). A gardener that DECLARES a chain.to but whose heir has
// not materialized contributes a PLANNED edge to a synthetic pending node,
// rendered distinctly from a broken dangling ghost.

func TestPlannedChainEdges_FromRows(t *testing.T) {
	rows := []gardenerRowView{
		{ID: "gd-a", PlannedHeirs: []gardenerPlannedHeir{
			{ID: "planned:gd-a:0", Label: "impl", FanOut: false},
		}},
		{ID: "gd-b"}, // no chain
		{ID: "gd-c", PlannedHeirs: []gardenerPlannedHeir{
			{ID: "planned:gd-c:0", Label: "impl-a", FanOut: true},
			{ID: "planned:gd-c:1", Label: "impl-b", FanOut: true},
		}},
	}
	edges := plannedChainEdges(rows)
	if len(edges) != 3 {
		t.Fatalf("planned edges = %d, want 3", len(edges))
	}
	for _, e := range edges {
		if !e.Planned {
			t.Errorf("edge %+v not marked Planned", e)
		}
	}
	// The fan-out edges carry the fan-out flag + labels.
	var cLabels []string
	for _, e := range edges {
		if e.Pred == "gd-c" {
			if !e.PlannedFanOut {
				t.Errorf("gd-c edge should be PlannedFanOut")
			}
			cLabels = append(cLabels, e.PlannedLabel)
		}
	}
	if len(cLabels) != 2 {
		t.Errorf("gd-c planned labels = %v, want 2", cLabels)
	}
}

func TestSuppressSpawnedPlannedEdges(t *testing.T) {
	// gd-a declares 2 heirs but has already materialized 1 (realHeirCount=1);
	// only the 2nd (unspawned) planned edge survives. gd-b declares 1 heir,
	// none materialized → its planned edge survives.
	planned := []mapsChainEdge{
		{Pred: "gd-a", Heir: "planned:gd-a:0", Planned: true, PlannedLabel: "impl-a"},
		{Pred: "gd-a", Heir: "planned:gd-a:1", Planned: true, PlannedLabel: "impl-b"},
		{Pred: "gd-b", Heir: "planned:gd-b:0", Planned: true, PlannedLabel: "verify"},
	}
	real := map[string]int{"gd-a": 1}
	got := suppressSpawnedPlannedEdges(planned, real)
	if len(got) != 2 {
		t.Fatalf("survivors = %d, want 2", len(got))
	}
	// The surviving gd-a edge is the SECOND (impl-b), the unspawned remainder.
	var aLabel string
	for _, e := range got {
		if e.Pred == "gd-a" {
			aLabel = e.PlannedLabel
		}
	}
	if aLabel != "impl-b" {
		t.Errorf("surviving gd-a planned = %q, want impl-b (unspawned remainder)", aLabel)
	}
}

func TestSuppressSpawnedPlannedEdges_FullySpawnedDropsAll(t *testing.T) {
	planned := []mapsChainEdge{
		{Pred: "gd-a", Heir: "planned:gd-a:0", Planned: true},
	}
	// gd-a already has 1 real heir == its declared count → no pending node.
	got := suppressSpawnedPlannedEdges(planned, map[string]int{"gd-a": 1})
	if len(got) != 0 {
		t.Fatalf("fully-spawned should drop all planned edges, got %d", len(got))
	}
}

// A planned heir renders as a PENDING node (not a broken dangling ghost).
func TestBuildMapsWorkflows_PlannedHeirIsPending(t *testing.T) {
	cards := cardsByIDFrom(mkChainCard("gd-a", "research", ""))
	edges := []mapsChainEdge{
		{Pred: "gd-a", Heir: "planned:gd-a:0", Planned: true, PlannedLabel: "impl", PlannedFanOut: false},
	}
	wfs, standalone := buildMapsWorkflows(cards, edges)
	if len(standalone) != 0 {
		t.Fatalf("gd-a is chained (planned), want no standalone; got %v", standalone)
	}
	if len(wfs) != 1 {
		t.Fatalf("want 1 workflow, got %d", len(wfs))
	}
	wf := wfs[0]
	if !wf.Pending {
		t.Errorf("workflow should be Pending")
	}
	if wf.Dangling {
		t.Errorf("a planned heir must NOT read as dangling/broken")
	}
	// The pending node is present with its label; the real gardener is counted.
	if wf.GardenerCount != 1 {
		t.Errorf("GardenerCount = %d, want 1 (the real predecessor only)", wf.GardenerCount)
	}
	var foundPending bool
	for _, c := range wf.Cards {
		if c.PendingHeir {
			foundPending = true
			if c.PendingLabel != "impl" {
				t.Errorf("PendingLabel = %q, want impl", c.PendingLabel)
			}
			if c.DanglingHeir {
				t.Errorf("pending node must not also be DanglingHeir")
			}
		}
	}
	if !foundPending {
		t.Errorf("no pending heir node rendered; cards=%+v", wf.Cards)
	}
}

// A materialized chain PLUS a still-declared planned heir on the SAME
// predecessor must not double-render: real edge + a suppressed planned edge.
func TestBuildMapsWorkflows_MixedRealAndPending(t *testing.T) {
	cards := cardsByIDFrom(
		mkChainCard("gd-a", "research", "achieved"),
		mkChainCard("gd-b", "impl", ""),
	)
	// gd-a→gd-b is materialized; gd-a also declares a second (unspawned) heir.
	edges := []mapsChainEdge{
		{Pred: "gd-a", Heir: "gd-b"},
		{Pred: "gd-a", Heir: "planned:gd-a:1", Planned: true, PlannedLabel: "verify", PlannedFanOut: true},
	}
	wfs, _ := buildMapsWorkflows(cards, edges)
	if len(wfs) != 1 {
		t.Fatalf("want 1 workflow, got %d", len(wfs))
	}
	wf := wfs[0]
	if !wf.Pending {
		t.Errorf("workflow should be Pending (has an unspawned heir)")
	}
	// Two real gardeners + one pending node.
	if wf.GardenerCount != 2 {
		t.Errorf("GardenerCount = %d, want 2", wf.GardenerCount)
	}
	pendCount := 0
	for _, c := range wf.Cards {
		if c.PendingHeir {
			pendCount++
		}
	}
	if pendCount != 1 {
		t.Errorf("pending nodes = %d, want 1", pendCount)
	}
}
