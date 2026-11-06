package main

import (
	"testing"

	"gardenio/controller/internal/beadsapi"
)

func TestBuildDecisionProjectFacets_CountsAndOrder(t *testing.T) {
	beads := []*beadsapi.BeadDetail{
		{ID: "a", Labels: []string{"project:gardenio"}},
		{ID: "b", Labels: []string{"project:gardenio"}},
		{ID: "c", Labels: []string{"project:gardenio"}},
		{ID: "d", Labels: []string{"project:monorepo"}},
		{ID: "e", Labels: []string{"project:cs"}},
		{ID: "f", Labels: []string{"project:cs"}},
		{ID: "g"}, // unlabeled — counts toward All only
		nil,       // tolerated
	}
	got := buildDecisionProjectFacets(beads, "", false, "/decisions")

	// All + 3 projects, All first, then count-desc.
	wantLabels := []string{"All", "gardenio", "cs", "monorepo"}
	if len(got) != len(wantLabels) {
		t.Fatalf("facets = %d (%v), want %d", len(got), got, len(wantLabels))
	}
	for i, want := range wantLabels {
		if got[i].Label != want {
			t.Errorf("facet[%d].Label = %q, want %q (full: %v)", i, got[i].Label, want, got)
		}
	}
	// All counts every non-nil bead including the unlabeled one.
	if got[0].Count != 7 {
		t.Errorf("All count = %d, want 7", got[0].Count)
	}
	if got[1].Count != 3 {
		t.Errorf("gardenio count = %d, want 3", got[1].Count)
	}
	if !got[0].Active {
		t.Error("All must be active with no project filter")
	}
	if got[0].URL != "/decisions" {
		t.Errorf("All URL = %q, want bare /decisions", got[0].URL)
	}
	if got[1].URL != "/decisions?project=gardenio" {
		t.Errorf("gardenio URL = %q", got[1].URL)
	}
}

// Equal counts must tie-break by name so the row order is stable across
// renders rather than following Go's randomized map iteration.
func TestBuildDecisionProjectFacets_TieBreaksByName(t *testing.T) {
	beads := []*beadsapi.BeadDetail{
		{Labels: []string{"project:zeta"}},
		{Labels: []string{"project:alpha"}},
		{Labels: []string{"project:mid"}},
	}
	for i := 0; i < 20; i++ {
		got := buildDecisionProjectFacets(beads, "", false, "/decisions")
		if got[1].Label != "alpha" || got[2].Label != "mid" || got[3].Label != "zeta" {
			t.Fatalf("unstable order on iteration %d: %v", i, got)
		}
	}
}

func TestBuildDecisionProjectFacets_ActiveAndShowAll(t *testing.T) {
	beads := []*beadsapi.BeadDetail{{Labels: []string{"project:gardenio"}}}
	got := buildDecisionProjectFacets(beads, "gardenio", true, "/b/decisions")

	if got[0].URL != "/b/decisions?all=1" {
		t.Errorf("All URL = %q, want the ?all=1 scope preserved", got[0].URL)
	}
	if got[0].Active {
		t.Error("All must not be active when a project is selected")
	}
	if got[1].Label != "gardenio" || !got[1].Active {
		t.Errorf("selected project chip not active: %v", got[1])
	}
	if got[1].URL != "/b/decisions?all=1&project=gardenio" {
		t.Errorf("project URL = %q", got[1].URL)
	}
}

// An active project with zero open decisions must still get a (zero-count)
// chip, so the pinned scope stays visible and clickable-off.
func TestBuildDecisionProjectFacets_ActiveWithNoMatches(t *testing.T) {
	got := buildDecisionProjectFacets([]*beadsapi.BeadDetail{
		{Labels: []string{"project:gardenio"}},
	}, "ghost", false, "/decisions")

	var found bool
	for _, f := range got {
		if f.Label == "ghost" {
			found = true
			if !f.Active {
				t.Error("ghost chip must be active")
			}
			if f.Count != 0 {
				t.Errorf("ghost count = %d, want 0", f.Count)
			}
		}
	}
	if !found {
		t.Errorf("active project with no matches lost its chip: %v", got)
	}
}

// Empty input still yields the All chip so the template's len>2 guard has a
// well-defined value instead of a nil slice index panic.
func TestBuildDecisionProjectFacets_EmptyYieldsAllOnly(t *testing.T) {
	got := buildDecisionProjectFacets(nil, "", false, "/decisions")
	if len(got) != 1 || got[0].Label != "All" || got[0].Count != 0 {
		t.Fatalf("facets = %v, want just a zero-count All chip", got)
	}
}
