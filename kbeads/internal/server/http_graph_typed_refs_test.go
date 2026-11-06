package server

import (
	"testing"

	"gardenio/kbeads/internal/model"
)

// Tests for typed-mention emission on /v1/graph (epic ka-0000003840, T5).
// Contract per docs/research-typed-mention-syntax.md § 8 + Decision 4:
//
//   - bead_reference rows with relationship_type set emit edges with
//     Type=DepType=<relationship_type> (so renderers achieve dep-style
//     parity automatically).
//   - bead_reference rows with NULL relationship_type keep the legacy
//     "mentions" edge shape and DepType="mentions".
//   - Every text-derived edge carries metadata["source"]="text".
//   - Every deps-derived edge carries metadata["source"]="dep".

func strPtr(s string) *string { return &s }

func TestHandleGraph_TypedMentions_RelationshipTypeBecomesEdgeType(t *testing.T) {
	ms, _, doGraph := graphWithMentions(t)
	ms.references = []*model.BeadReference{
		{
			SourceBeadID:     "gd-src1",
			TargetBeadID:     "gd-tgt1",
			SourceField:      "description",
			RelationshipType: strPtr("blocks"),
		},
	}

	result := doGraph(map[string]any{})

	var typed []*model.GraphEdge
	for _, e := range result.Edges {
		if e.Source == "gd-src1" && e.Target == "gd-tgt1" {
			typed = append(typed, e)
		}
	}
	if len(typed) != 1 {
		t.Fatalf("expected 1 typed-mention edge, got %d (all edges: %+v)", len(typed), result.Edges)
	}
	e := typed[0]
	if e.Type != "blocks" {
		t.Fatalf("Type: want %q, got %q", "blocks", e.Type)
	}
	if e.DepType != "blocks" {
		t.Fatalf("DepType: want %q, got %q", "blocks", e.DepType)
	}
	if e.Metadata == nil || e.Metadata["source"] != "text" {
		t.Fatalf("metadata.source: want %q, got %+v", "text", e.Metadata)
	}
	if e.Metadata["source_field"] != "description" {
		t.Fatalf("metadata.source_field: want %q, got %q", "description", e.Metadata["source_field"])
	}
}

func TestHandleGraph_TypedMentions_UntypedKeepsMentionsType(t *testing.T) {
	// NULL relationship_type → still "mentions" (backward compatible).
	ms, _, doGraph := graphWithMentions(t)
	ms.references = []*model.BeadReference{
		{SourceBeadID: "gd-src1", TargetBeadID: "gd-tgt1", SourceField: "description"},
	}

	result := doGraph(map[string]any{})

	var found *model.GraphEdge
	for _, e := range result.Edges {
		if e.Source == "gd-src1" && e.Target == "gd-tgt1" {
			found = e
			break
		}
	}
	if found == nil {
		t.Fatalf("expected mentions edge, got none (edges: %+v)", result.Edges)
	}
	if found.Type != "mentions" {
		t.Fatalf("Type: want %q, got %q", "mentions", found.Type)
	}
	if found.DepType != "mentions" {
		t.Fatalf("DepType: want %q, got %q", "mentions", found.DepType)
	}
	if found.Metadata == nil || found.Metadata["source"] != "text" {
		t.Fatalf("metadata.source: want %q, got %+v", "text", found.Metadata)
	}
}

func TestHandleGraph_TypedMentions_TypedAndUntypedCoexist(t *testing.T) {
	// Same (src, tgt) pair with both a typed (blocks) and an untyped
	// reference must coexist as two distinct edges. The bead_references
	// PK was extended in migration 013 specifically to allow this.
	ms, _, doGraph := graphWithMentions(t)
	ms.references = []*model.BeadReference{
		{SourceBeadID: "gd-src1", TargetBeadID: "gd-tgt1", SourceField: "description"},
		{
			SourceBeadID:     "gd-src1",
			TargetBeadID:     "gd-tgt1",
			SourceField:      "description",
			RelationshipType: strPtr("blocks"),
		},
	}

	result := doGraph(map[string]any{})

	types := make(map[string]int)
	for _, e := range result.Edges {
		if e.Source == "gd-src1" && e.Target == "gd-tgt1" {
			types[e.Type]++
		}
	}
	if types["mentions"] != 1 || types["blocks"] != 1 {
		t.Fatalf("expected one mentions edge and one blocks edge, got %+v (all: %+v)", types, result.Edges)
	}
}

func TestHandleGraph_TypedMentions_DistinctRelationshipTypesProduceDistinctEdges(t *testing.T) {
	// Two typed mentions of the same target with different verbs both render.
	ms, _, doGraph := graphWithMentions(t)
	ms.references = []*model.BeadReference{
		{
			SourceBeadID:     "gd-src1",
			TargetBeadID:     "gd-tgt1",
			SourceField:      "description",
			RelationshipType: strPtr("blocks"),
		},
		{
			SourceBeadID:     "gd-src1",
			TargetBeadID:     "gd-tgt1",
			SourceField:      "description",
			RelationshipType: strPtr("parent-child"),
		},
	}

	result := doGraph(map[string]any{})

	types := make(map[string]bool)
	for _, e := range result.Edges {
		if e.Source == "gd-src1" && e.Target == "gd-tgt1" {
			if e.Metadata["source"] != "text" {
				t.Fatalf("text edge missing metadata.source=text: %+v", e)
			}
			types[e.Type] = true
		}
	}
	if !types["blocks"] || !types["parent-child"] {
		t.Fatalf("expected both blocks and parent-child edges, got %+v", types)
	}
}

func TestHandleGraph_DepEdges_CarrySourceDep(t *testing.T) {
	// Golden: every deps-derived edge must carry metadata["source"]="dep"
	// and DepType mirroring Type, so consumers can split by edge provenance.
	_, ms, h := newTestServer()
	ms.beads["gd-A"] = &model.Bead{ID: "gd-A", Title: "A", Status: model.StatusOpen, Kind: model.KindIssue, Type: model.TypeTask}
	ms.beads["gd-B"] = &model.Bead{ID: "gd-B", Title: "B", Status: model.StatusOpen, Kind: model.KindIssue, Type: model.TypeTask}
	ms.deps["gd-A"] = []*model.Dependency{
		{BeadID: "gd-A", DependsOnID: "gd-B", Type: model.DepBlocks},
	}

	rec := doJSON(t, h, "POST", "/v1/graph", map[string]any{})
	requireStatus(t, rec, 200)
	var result model.GraphResponse
	decodeJSON(t, rec, &result)

	var found *model.GraphEdge
	for _, e := range result.Edges {
		if e.Source == "gd-A" && e.Target == "gd-B" {
			found = e
			break
		}
	}
	if found == nil {
		t.Fatalf("dep edge missing (edges: %+v)", result.Edges)
	}
	if found.Type != "blocks" {
		t.Fatalf("Type: want %q, got %q", "blocks", found.Type)
	}
	if found.DepType != "blocks" {
		t.Fatalf("DepType: want %q, got %q", "blocks", found.DepType)
	}
	if found.Metadata == nil || found.Metadata["source"] != "dep" {
		t.Fatalf("metadata.source: want %q, got %+v", "dep", found.Metadata)
	}
}
