package main

import (
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
)

func TestDiffBeads_InitialPoll(t *testing.T) {
	seen := make(map[string]time.Time)
	now := time.Now()
	beads := []*model.Bead{
		{ID: "a", UpdatedAt: now},
		{ID: "b", UpdatedAt: now.Add(time.Second)},
	}

	changed := diffBeads(beads, seen)
	if len(changed) != 2 {
		t.Fatalf("got %d changed, want 2", len(changed))
	}
	if len(seen) != 2 {
		t.Fatalf("got %d seen, want 2", len(seen))
	}
}

func TestDiffBeads_NoChanges(t *testing.T) {
	now := time.Now()
	seen := map[string]time.Time{
		"a": now,
		"b": now.Add(time.Second),
	}
	beads := []*model.Bead{
		{ID: "a", UpdatedAt: now},
		{ID: "b", UpdatedAt: now.Add(time.Second)},
	}

	changed := diffBeads(beads, seen)
	if len(changed) != 0 {
		t.Fatalf("got %d changed, want 0", len(changed))
	}
}

func TestDiffBeads_NewBead(t *testing.T) {
	now := time.Now()
	seen := map[string]time.Time{
		"a": now,
	}
	beads := []*model.Bead{
		{ID: "a", UpdatedAt: now},
		{ID: "b", UpdatedAt: now},
	}

	changed := diffBeads(beads, seen)
	if len(changed) != 1 {
		t.Fatalf("got %d changed, want 1", len(changed))
	}
	if changed[0].ID != "b" {
		t.Errorf("got changed[0].ID=%q, want %q", changed[0].ID, "b")
	}
}

func TestDiffBeads_UpdatedBead(t *testing.T) {
	now := time.Now()
	seen := map[string]time.Time{
		"a": now,
		"b": now,
	}
	beads := []*model.Bead{
		{ID: "a", UpdatedAt: now},
		{ID: "b", UpdatedAt: now.Add(5 * time.Second)},
	}

	changed := diffBeads(beads, seen)
	if len(changed) != 1 {
		t.Fatalf("got %d changed, want 1", len(changed))
	}
	if changed[0].ID != "b" {
		t.Errorf("got changed[0].ID=%q, want %q", changed[0].ID, "b")
	}
	// Verify seen map was updated.
	if !seen["b"].Equal(now.Add(5 * time.Second)) {
		t.Error("seen map was not updated for bead b")
	}
}

func TestDiffBeads_ZeroUpdatedAt(t *testing.T) {
	seen := make(map[string]time.Time)
	beads := []*model.Bead{
		{ID: "a"}, // zero UpdatedAt
	}

	changed := diffBeads(beads, seen)
	if len(changed) != 1 {
		t.Fatalf("got %d changed, want 1", len(changed))
	}

	// Second call with same zero UpdatedAt should not diff.
	changed = diffBeads(beads, seen)
	if len(changed) != 0 {
		t.Fatalf("got %d changed on second call, want 0", len(changed))
	}
}
