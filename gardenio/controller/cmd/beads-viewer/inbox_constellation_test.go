package main

import (
	"reflect"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// TestParentTargets covers the forward-ring helper: given the bead's outgoing
// deps, return parents reachable via canonical (`parent-child`) and inverse
// (`child-of`) types only. Anything else — including the parent-stored
// `parent-of` shape — is ignored here; that one feeds the reverse ring.
func TestParentTargets(t *testing.T) {
	cases := []struct {
		name string
		deps []beadsapi.Dependency
		want []string
	}{
		{
			name: "empty",
			deps: nil,
			want: []string{},
		},
		{
			name: "parent-child rows return parents",
			deps: []beadsapi.Dependency{
				{BeadID: "gd-self", DependsOnID: "gd-parent1", Type: "parent-child"},
				{BeadID: "gd-self", DependsOnID: "gd-parent2", Type: "parent-child"},
			},
			want: []string{"gd-parent1", "gd-parent2"},
		},
		{
			name: "child-of alias also returns parents",
			deps: []beadsapi.Dependency{
				{BeadID: "gd-self", DependsOnID: "gd-parent1", Type: "child-of"},
			},
			want: []string{"gd-parent1"},
		},
		{
			name: "parent-of rows are skipped (those are descendants, not parents)",
			deps: []beadsapi.Dependency{
				{BeadID: "gd-self", DependsOnID: "gd-child1", Type: "parent-of"},
			},
			want: []string{},
		},
		{
			name: "blocks/related rows are skipped",
			deps: []beadsapi.Dependency{
				{BeadID: "gd-self", DependsOnID: "gd-other", Type: "blocks"},
				{BeadID: "gd-self", DependsOnID: "gd-other2", Type: "related"},
			},
			want: []string{},
		},
		{
			name: "dedup repeated parent ids",
			deps: []beadsapi.Dependency{
				{BeadID: "gd-self", DependsOnID: "gd-parent1", Type: "parent-child"},
				{BeadID: "gd-self", DependsOnID: "gd-parent1", Type: "child-of"},
			},
			want: []string{"gd-parent1"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parentTargets(tc.deps)
			if got == nil {
				got = []string{}
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parentTargets(%v) = %v, want %v", tc.deps, got, tc.want)
			}
		})
	}
}

// TestChildTargets covers the reverse-ring helper. Children show up in two
// storage shapes: canonical (child-stored `parent-child`/`child-of` rows in
// `revDeps`) and inverse (parent-stored `parent-of` rows in `deps`). Both
// shapes contribute to the same ring, deduped by id.
func TestChildTargets(t *testing.T) {
	cases := []struct {
		name    string
		deps    []beadsapi.Dependency
		revDeps []beadsapi.Dependency
		want    []string
	}{
		{
			name: "canonical: revDeps with parent-child returns child ids",
			revDeps: []beadsapi.Dependency{
				{BeadID: "gd-child1", DependsOnID: "gd-self", Type: "parent-child"},
				{BeadID: "gd-child2", DependsOnID: "gd-self", Type: "parent-child"},
			},
			want: []string{"gd-child1", "gd-child2"},
		},
		{
			name: "child-of in revDeps is also accepted",
			revDeps: []beadsapi.Dependency{
				{BeadID: "gd-child1", DependsOnID: "gd-self", Type: "child-of"},
			},
			want: []string{"gd-child1"},
		},
		{
			name: "inverse: deps rows of type parent-of return DependsOnID as child id",
			deps: []beadsapi.Dependency{
				{BeadID: "gd-self", DependsOnID: "gd-child1", Type: "parent-of"},
				{BeadID: "gd-self", DependsOnID: "gd-child2", Type: "parent-of"},
			},
			want: []string{"gd-child1", "gd-child2"},
		},
		{
			name: "mixed shapes: both contribute, deduped",
			deps: []beadsapi.Dependency{
				{BeadID: "gd-self", DependsOnID: "gd-child1", Type: "parent-of"},
				{BeadID: "gd-self", DependsOnID: "gd-child3", Type: "parent-of"},
			},
			revDeps: []beadsapi.Dependency{
				{BeadID: "gd-child1", DependsOnID: "gd-self", Type: "parent-child"}, // dup
				{BeadID: "gd-child2", DependsOnID: "gd-self", Type: "parent-child"},
			},
			want: []string{"gd-child1", "gd-child2", "gd-child3"},
		},
		{
			name: "non-structural types ignored",
			deps: []beadsapi.Dependency{
				{BeadID: "gd-self", DependsOnID: "gd-other", Type: "blocks"},
			},
			revDeps: []beadsapi.Dependency{
				{BeadID: "gd-other", DependsOnID: "gd-self", Type: "related"},
			},
			want: []string{},
		},
		{
			name: "parent-child in `deps` slot is ignored (those are parents, not children)",
			deps: []beadsapi.Dependency{
				{BeadID: "gd-self", DependsOnID: "gd-parent1", Type: "parent-child"},
			},
			want: []string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := childTargets(tc.deps, tc.revDeps)
			if got == nil {
				got = []string{}
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("childTargets(deps=%v, revDeps=%v) = %v, want %v",
					tc.deps, tc.revDeps, got, tc.want)
			}
		})
	}
}
