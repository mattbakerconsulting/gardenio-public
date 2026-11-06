package server

import (
	"testing"

	"gardenio/kbeads/internal/model"
)

// Tests for the include_text_references flag on /v1/graph (ka-0000002463).
// Contract: emit edges with type "mentions" and source_field metadata for
// gd-id references parsed out of bead text. See docs/design-bead-references.md
// § 5b.

func graphWithMentions(t *testing.T) (*mockStore, *model.GraphResponse, func(args map[string]any) *model.GraphResponse) {
	t.Helper()
	_, ms, h := newTestServer()
	ms.beads["gd-src1"] = &model.Bead{ID: "gd-src1", Title: "Source", Status: model.StatusOpen, Kind: model.KindIssue, Type: model.TypeTask}
	ms.beads["gd-tgt1"] = &model.Bead{ID: "gd-tgt1", Title: "Target", Status: model.StatusOpen, Kind: model.KindIssue, Type: model.TypeTask}

	doGraph := func(args map[string]any) *model.GraphResponse {
		t.Helper()
		rec := doJSON(t, h, "POST", "/v1/graph", args)
		requireStatus(t, rec, 200)
		var result model.GraphResponse
		decodeJSON(t, rec, &result)
		return &result
	}
	return ms, doGraph(map[string]any{}), doGraph
}

func TestHandleGraph_TextReferences_DefaultEmitsMentionsEdges(t *testing.T) {
	ms, _, doGraph := graphWithMentions(t)
	ms.references = []*model.BeadReference{
		{SourceBeadID: "gd-src1", TargetBeadID: "gd-tgt1", SourceField: "description"},
	}

	result := doGraph(map[string]any{})

	// Default include_text_references=true: a mentions edge should appear.
	var mentionsEdges []*model.GraphEdge
	for _, e := range result.Edges {
		if e.Type == "mentions" {
			mentionsEdges = append(mentionsEdges, e)
		}
	}
	if len(mentionsEdges) != 1 {
		t.Fatalf("expected 1 mentions edge by default, got %d (all edges: %+v)", len(mentionsEdges), result.Edges)
	}
	e := mentionsEdges[0]
	if e.Source != "gd-src1" || e.Target != "gd-tgt1" {
		t.Fatalf("unexpected mentions edge endpoints: %+v", e)
	}
	if e.Metadata == nil || e.Metadata["source_field"] != "description" {
		t.Fatalf("expected source_field=description in metadata, got %+v", e.Metadata)
	}
}

func TestHandleGraph_TextReferences_FlagOffOmitsExtraEdges(t *testing.T) {
	ms, _, doGraph := graphWithMentions(t)
	ms.references = []*model.BeadReference{
		{SourceBeadID: "gd-src1", TargetBeadID: "gd-tgt1", SourceField: "description"},
	}

	result := doGraph(map[string]any{
		"include_text_references": false,
	})

	for _, e := range result.Edges {
		if e.Type == "mentions" {
			t.Fatalf("expected no mentions edges when include_text_references=false, got %+v", e)
		}
	}
}

func TestHandleGraph_TextReferences_DanglingTargetOmitted(t *testing.T) {
	// Source is in window, target is NOT — the mentions edge must be
	// omitted (do not surface degenerate nodes per § 5b).
	ms, _, doGraph := graphWithMentions(t)
	ms.references = []*model.BeadReference{
		{SourceBeadID: "gd-src1", TargetBeadID: "gd-MVGLXH3KPX", SourceField: "description"},
	}

	result := doGraph(map[string]any{})

	for _, e := range result.Edges {
		if e.Type == "mentions" {
			t.Fatalf("expected no mentions edge for dangling target, got %+v", e)
		}
	}
	for _, n := range result.Nodes {
		if n.ID == "gd-MVGLXH3KPX" {
			t.Fatalf("dangling target should not surface as a node, got %+v", n)
		}
	}
}

func TestHandleGraph_TextReferences_MultipleSourceFieldsCollapsed(t *testing.T) {
	// Multiple rows for the same (source, target) pair (e.g. one in
	// description and one in a comment) should collapse to a single
	// mentions edge with both source_fields surfaced in metadata.
	ms, _, doGraph := graphWithMentions(t)
	ms.references = []*model.BeadReference{
		{SourceBeadID: "gd-src1", TargetBeadID: "gd-tgt1", SourceField: "description"},
		{SourceBeadID: "gd-src1", TargetBeadID: "gd-tgt1", SourceField: "comment:42"},
	}

	result := doGraph(map[string]any{})

	var mentionsEdges []*model.GraphEdge
	for _, e := range result.Edges {
		if e.Type == "mentions" {
			mentionsEdges = append(mentionsEdges, e)
		}
	}
	if len(mentionsEdges) != 1 {
		t.Fatalf("expected 1 collapsed mentions edge, got %d", len(mentionsEdges))
	}
	got := mentionsEdges[0].Metadata["source_field"]
	// Sorted, comma-joined: "comment:42,description".
	want := "comment:42,description"
	if got != want {
		t.Fatalf("expected source_field=%q, got %q", want, got)
	}
}

func TestHandleGraph_TextReferences_QueryParamOptOut(t *testing.T) {
	// GET /v1/graph?include_text_references=false also opts out.
	_, ms, h := newTestServer()
	ms.beads["gd-q1"] = &model.Bead{ID: "gd-q1", Title: "A", Status: model.StatusOpen, Kind: model.KindIssue, Type: model.TypeTask}
	ms.beads["gd-q2"] = &model.Bead{ID: "gd-q2", Title: "B", Status: model.StatusOpen, Kind: model.KindIssue, Type: model.TypeTask}
	ms.references = []*model.BeadReference{
		{SourceBeadID: "gd-q1", TargetBeadID: "gd-q2", SourceField: "notes"},
	}

	rec := doJSON(t, h, "GET", "/v1/graph?include_text_references=false", nil)
	requireStatus(t, rec, 200)
	var result model.GraphResponse
	decodeJSON(t, rec, &result)
	for _, e := range result.Edges {
		if e.Type == "mentions" {
			t.Fatalf("query opt-out failed, got mentions edge %+v", e)
		}
	}
}
