package server

import (
	"testing"

	"gardenio/kbeads/internal/model"
)

// Tests for the `triggered-by` dep type as a first-class lineage edge — the
// "Option E substrate" landed under auto-link epic ka-0000000933, M1
// (gd-8E0ffMCTfu). Companion to the unit-test plan ka-0000004136 § 15. These
// tests pin the server-side semantics that downstream milestones (M2-M5)
// build on:
//
//   - dep-type validator accepts the new verb (15.1)
//   - GET /v1/beads/{id}/dependencies?type=triggered-by filters correctly (15.2)
//   - /v1/graph emits triggered-by edges with dep_type="triggered-by" (15.3)
//   - ready-queue blocker walk ignores triggered-by edges (15.4)

// TestDepTypeRegistry_TriggeredBy_Accepted verifies the validator treats
// `triggered-by` as a valid dependency type alongside the existing closed-core
// verbs. The model accepts any non-empty <=50 char string (extensible by
// design); this test pins the closed-core constant so a future refactor that
// switches to a fixed allow-list still lets `triggered-by` through.
func TestDepTypeRegistry_TriggeredBy_Accepted(t *testing.T) {
	for _, dt := range []model.DependencyType{
		model.DepBlocks,
		model.DepParentChild,
		model.DepRelated,
		model.DepDuplicates,
		model.DepSupersedes,
		model.DepTriggeredBy,
	} {
		if !dt.IsValid() {
			t.Errorf("DependencyType(%q).IsValid() = false, want true", dt)
		}
	}
	if string(model.DepTriggeredBy) != "triggered-by" {
		t.Fatalf("DepTriggeredBy = %q, want %q", model.DepTriggeredBy, "triggered-by")
	}
}

// TestListDependencies_Filter_TriggeredBy exercises the HTTP `?type=` filter
// added to GET /v1/beads/{id}/dependencies. With a mixed-type dep set, the
// triggered-by filter must return only triggered-by rows; the bare endpoint
// (no filter) must still return everything.
func TestListDependencies_Filter_TriggeredBy(t *testing.T) {
	_, ms, h := newTestServer()
	ms.deps["gd-child"] = []*model.Dependency{
		{BeadID: "gd-child", DependsOnID: "gd-parent", Type: model.DepParentChild},
		{BeadID: "gd-child", DependsOnID: "gd-blocker", Type: model.DepBlocks},
		{BeadID: "gd-child", DependsOnID: "gd-trigger", Type: model.DepTriggeredBy},
	}

	// No filter: all three rows.
	rec := doJSON(t, h, "GET", "/v1/beads/gd-child/dependencies", nil)
	requireStatus(t, rec, 200)
	var unfiltered struct {
		Dependencies []model.Dependency `json:"dependencies"`
	}
	decodeJSON(t, rec, &unfiltered)
	if len(unfiltered.Dependencies) != 3 {
		t.Fatalf("no-filter: want 3 deps, got %d (%+v)", len(unfiltered.Dependencies), unfiltered.Dependencies)
	}

	// type=triggered-by: only the triggered-by row.
	rec = doJSON(t, h, "GET", "/v1/beads/gd-child/dependencies?type=triggered-by", nil)
	requireStatus(t, rec, 200)
	var filtered struct {
		Dependencies []model.Dependency `json:"dependencies"`
	}
	decodeJSON(t, rec, &filtered)
	if len(filtered.Dependencies) != 1 {
		t.Fatalf("triggered-by filter: want 1 dep, got %d (%+v)", len(filtered.Dependencies), filtered.Dependencies)
	}
	got := filtered.Dependencies[0]
	if got.Type != model.DepTriggeredBy {
		t.Errorf("filtered dep.Type = %q, want %q", got.Type, model.DepTriggeredBy)
	}
	if got.DependsOnID != "gd-trigger" {
		t.Errorf("filtered dep.DependsOnID = %q, want %q", got.DependsOnID, "gd-trigger")
	}

	// type=blocks: only the blocks row (regression check that the filter
	// still works for the existing verbs).
	rec = doJSON(t, h, "GET", "/v1/beads/gd-child/dependencies?type=blocks", nil)
	requireStatus(t, rec, 200)
	var blocksOnly struct {
		Dependencies []model.Dependency `json:"dependencies"`
	}
	decodeJSON(t, rec, &blocksOnly)
	if len(blocksOnly.Dependencies) != 1 || blocksOnly.Dependencies[0].Type != model.DepBlocks {
		t.Fatalf("blocks filter: want 1 blocks dep, got %+v", blocksOnly.Dependencies)
	}
}

// TestGraphAPI_TriggeredByEdge_DistinctFromParentChild verifies that /v1/graph
// emits a triggered-by edge as a first-class edge with Type=DepType=
// "triggered-by" and metadata.source="dep" — the same shape every other
// deps-table-derived edge uses. Crucially, the triggered-by edge must NOT
// collapse into the parent-child edge or vice versa: both must surface as
// separate edges so renderers can style them independently (beads-viewer
// follows parent-child for its tree traversal but treats triggered-by as
// lineage provenance).
func TestGraphAPI_TriggeredByEdge_DistinctFromParentChild(t *testing.T) {
	_, ms, h := newTestServer()
	ms.beads["gd-src"] = &model.Bead{ID: "gd-src", Title: "Source", Status: model.StatusOpen, Kind: model.KindIssue, Type: model.TypeTask}
	ms.beads["gd-pa"] = &model.Bead{ID: "gd-pa", Title: "Parent", Status: model.StatusOpen, Kind: model.KindIssue, Type: model.TypeTask}
	ms.beads["gd-tr"] = &model.Bead{ID: "gd-tr", Title: "Trigger", Status: model.StatusOpen, Kind: model.KindIssue, Type: model.TypeTask}
	ms.deps["gd-src"] = []*model.Dependency{
		{BeadID: "gd-src", DependsOnID: "gd-pa", Type: model.DepParentChild},
		{BeadID: "gd-src", DependsOnID: "gd-tr", Type: model.DepTriggeredBy},
	}

	rec := doJSON(t, h, "POST", "/v1/graph", map[string]any{})
	requireStatus(t, rec, 200)
	var result model.GraphResponse
	decodeJSON(t, rec, &result)

	var triggered, parent *model.GraphEdge
	for _, e := range result.Edges {
		if e.Source != "gd-src" {
			continue
		}
		switch e.DepType {
		case "triggered-by":
			triggered = e
		case "parent-child":
			parent = e
		}
	}
	if triggered == nil {
		t.Fatalf("missing triggered-by edge; got edges=%+v", result.Edges)
	}
	if parent == nil {
		t.Fatalf("missing parent-child edge; got edges=%+v", result.Edges)
	}
	if triggered.Type != "triggered-by" || triggered.DepType != "triggered-by" {
		t.Errorf("triggered-by edge Type=%q DepType=%q, want both %q", triggered.Type, triggered.DepType, "triggered-by")
	}
	if triggered.Target != "gd-tr" {
		t.Errorf("triggered-by edge Target=%q, want %q", triggered.Target, "gd-tr")
	}
	if triggered.Metadata == nil || triggered.Metadata["source"] != "dep" {
		t.Errorf("triggered-by edge metadata.source=%v, want %q", triggered.Metadata, "dep")
	}
	if parent.Target != "gd-pa" {
		t.Errorf("parent-child edge Target=%q, want %q", parent.Target, "gd-pa")
	}
}

// TestReadyQueue_IgnoresTriggeredBy asserts that the /v1/ready blocker walk
// only considers `blocks` edges. A bead whose only outgoing dep is a
// triggered-by edge to an OPEN trigger source must still surface as ready —
// otherwise every trigger-spawned task would be permanently blocked on the
// schedule / source bead that spawned it.
func TestReadyQueue_IgnoresTriggeredBy(t *testing.T) {
	_, ms, h := newTestServer()
	ms.beads["gd-work"] = &model.Bead{ID: "gd-work", Title: "Triggered work", Status: model.StatusOpen}
	ms.beads["gd-src"] = &model.Bead{ID: "gd-src", Title: "Source bead", Status: model.StatusOpen}
	ms.beads["gd-blocked"] = &model.Bead{ID: "gd-blocked", Title: "Real blocked", Status: model.StatusOpen}
	ms.beads["gd-blocker"] = &model.Bead{ID: "gd-blocker", Title: "Real blocker", Status: model.StatusOpen}
	ms.deps["gd-work"] = []*model.Dependency{
		{BeadID: "gd-work", DependsOnID: "gd-src", Type: model.DepTriggeredBy},
	}
	ms.deps["gd-blocked"] = []*model.Dependency{
		{BeadID: "gd-blocked", DependsOnID: "gd-blocker", Type: model.DepBlocks},
	}

	rec := doJSON(t, h, "GET", "/v1/ready", nil)
	requireStatus(t, rec, 200)
	var result struct {
		Beads []*model.Bead `json:"beads"`
		Total int           `json:"total"`
	}
	decodeJSON(t, rec, &result)

	ready := make(map[string]bool, len(result.Beads))
	for _, b := range result.Beads {
		ready[b.ID] = true
	}
	if !ready["gd-work"] {
		t.Errorf("gd-work (triggered-by an open source) should be ready, got beads=%v", ready)
	}
	if !ready["gd-src"] || !ready["gd-blocker"] {
		t.Errorf("trigger source / blocker should be ready, got beads=%v", ready)
	}
	if ready["gd-blocked"] {
		t.Errorf("gd-blocked (blocked by open gd-blocker) must NOT be ready, got beads=%v", ready)
	}
}
