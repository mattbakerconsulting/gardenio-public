package main

import (
	"testing"

	"gardenio/controller/internal/beadsapi"
)

func strptr(s string) *string { return &s }

// mkRef builds a forward-mention summary with an optional typed relationship.
func mkRef(targetID string, relType *string) beadsapi.BeadReferenceSummary {
	return beadsapi.BeadReferenceSummary{ID: targetID, RelationshipType: relType}
}

// TestChainEdgeFromMention covers direction recovery from the surface verb:
// chains_to/chains-to are forward (src=predecessor), succeeds is reverse
// (src=heir). Non-chain verbs and self/empty edges are rejected.
func TestChainEdgeFromMention(t *testing.T) {
	cases := []struct {
		name           string
		src, rel, tgt  string
		wantOK         bool
		wantPred, want string
	}{
		{"chains_to forward", "gd-a", "chains_to", "gd-b", true, "gd-a", "gd-b"},
		{"chains-to hyphen alias", "gd-a", "chains-to", "gd-b", true, "gd-a", "gd-b"},
		{"succeeds reverse", "gd-b", "succeeds", "gd-a", true, "gd-a", "gd-b"},
		{"case-insensitive verb", "gd-a", "Chains_To", "gd-b", true, "gd-a", "gd-b"},
		{"non-chain verb dropped", "gd-a", "parent", "gd-b", false, "", ""},
		{"self edge dropped", "gd-a", "chains_to", "gd-a", false, "", ""},
		{"empty target dropped", "gd-a", "chains_to", "", false, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, ok := chainEdgeFromMention(tc.src, tc.rel, tc.tgt)
			if ok != tc.wantOK {
				t.Fatalf("ok=%v want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if e.Pred != tc.wantPred || e.Heir != tc.want {
				t.Errorf("edge=%+v want pred=%s heir=%s", e, tc.wantPred, tc.want)
			}
		})
	}
}

// TestChainEdgesFromMentions: only typed chain verbs become edges; untyped and
// non-chain mentions are ignored.
func TestChainEdgesFromMentions(t *testing.T) {
	fwd := []beadsapi.BeadReferenceSummary{
		mkRef("gd-b", strptr("chains_to")),
		mkRef("gd-c", strptr("parent")), // not a chain verb
		mkRef("gd-d", nil),              // untyped mention
		mkRef("gd-e", strptr("succeeds")),
	}
	edges := chainEdgesFromMentions("gd-a", fwd)
	if len(edges) != 2 {
		t.Fatalf("expected 2 chain edges, got %d: %+v", len(edges), edges)
	}
	// gd-a chains_to gd-b → (a,b); gd-a succeeds gd-e → (e,a).
	want := map[mapsChainEdge]bool{
		{Pred: "gd-a", Heir: "gd-b"}: true,
		{Pred: "gd-e", Heir: "gd-a"}: true,
	}
	for _, e := range edges {
		if !want[e] {
			t.Errorf("unexpected edge %+v", e)
		}
	}
}
