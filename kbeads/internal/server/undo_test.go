package server

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// newUndoFixture creates a BeadsServer + mockStore with one bead and one
// recorded update mutation matching the bead's current value of `field`.
// Returns the seeded mutation id so tests can drive undoMutation.
func newUndoFixture(t *testing.T, beadID string, b *model.Bead, field, oldVal, newVal string) (*BeadsServer, *mockStore, int64) {
	t.Helper()
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	ms.beads[beadID] = b
	mut := &model.MutationRow{
		BeadID:       beadID,
		Actor:        "alice@example.com",
		Field:        field,
		OldValue:     oldVal,
		NewValue:     newVal,
		MutationType: model.MutationTypeUpdate,
		CreatedAt:    time.Now(),
	}
	if err := ms.RecordMutation(context.Background(), mut); err != nil {
		t.Fatalf("seeding mutation: %v", err)
	}
	// RecordMutation copies the row and assigns id; pull the persisted id.
	muts, _ := ms.ListMutations(context.Background(), beadID)
	if len(muts) == 0 {
		t.Fatalf("expected one seeded mutation")
	}
	return s, ms, muts[0].ID
}

func TestUndoMutation_HappyPath_Title(t *testing.T) {
	bead := &model.Bead{ID: "gd-undo-1", Title: "New Title", Type: "task", Kind: model.KindIssue, Status: model.StatusOpen}
	s, _, mutID := newUndoFixture(t, "gd-undo-1", bead, "title", "Old Title", "New Title")

	updated, undone, err := s.undoMutation(context.Background(), mutID, DefaultUndoRetentionDays)
	if err != nil {
		t.Fatalf("undoMutation: %v", err)
	}
	if updated.Title != "Old Title" {
		t.Errorf("title=%q, want %q", updated.Title, "Old Title")
	}
	if undone.ID != mutID {
		t.Errorf("undone.ID=%d, want %d", undone.ID, mutID)
	}
	if undone.Field != "title" {
		t.Errorf("undone.Field=%q, want title", undone.Field)
	}
}

func TestUndoMutation_HappyPath_Priority(t *testing.T) {
	bead := &model.Bead{ID: "gd-undo-prio", Title: "x", Type: "task", Kind: model.KindIssue, Priority: 3, Status: model.StatusOpen}
	s, _, mutID := newUndoFixture(t, "gd-undo-prio", bead, "priority", "1", "3")

	updated, _, err := s.undoMutation(context.Background(), mutID, DefaultUndoRetentionDays)
	if err != nil {
		t.Fatalf("undoMutation: %v", err)
	}
	if updated.Priority != 1 {
		t.Errorf("priority=%d, want 1", updated.Priority)
	}
}

func TestUndoMutation_NotFound(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})

	_, _, err := s.undoMutation(context.Background(), 999, DefaultUndoRetentionDays)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("err=%v, want sql.ErrNoRows", err)
	}
}

func TestUndoMutation_Conflict_FieldChanged(t *testing.T) {
	// Bead's current title is "Drift" but the mutation's recorded new_value
	// is "New Title" — refuse to undo, since something has changed since.
	bead := &model.Bead{ID: "gd-conflict", Title: "Drift", Type: "task", Status: model.StatusOpen}
	s, _, mutID := newUndoFixture(t, "gd-conflict", bead, "title", "Old Title", "New Title")

	_, _, err := s.undoMutation(context.Background(), mutID, DefaultUndoRetentionDays)
	if !errors.Is(err, ErrUndoConflict) {
		t.Fatalf("err=%v, want ErrUndoConflict", err)
	}
}

func TestUndoMutation_UnsupportedMutationType(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	ms.beads["gd-unsup"] = &model.Bead{ID: "gd-unsup", Type: "task", Status: model.StatusOpen}
	mut := &model.MutationRow{
		BeadID:       "gd-unsup",
		Field:        "*",
		MutationType: model.MutationTypeCreate,
		CreatedAt:    time.Now(),
	}
	_ = ms.RecordMutation(context.Background(), mut)
	muts, _ := ms.ListMutations(context.Background(), "gd-unsup")

	_, _, err := s.undoMutation(context.Background(), muts[0].ID, DefaultUndoRetentionDays)
	if !errors.Is(err, ErrUndoUnsupportedMutation) {
		t.Fatalf("err=%v, want ErrUndoUnsupportedMutation", err)
	}
}

func TestUndoMutation_UnsupportedField(t *testing.T) {
	bead := &model.Bead{ID: "gd-uf", Type: "task", Status: model.StatusOpen}
	s, _, mutID := newUndoFixture(t, "gd-uf", bead, "labels", "old", "new")

	_, _, err := s.undoMutation(context.Background(), mutID, DefaultUndoRetentionDays)
	if !errors.Is(err, ErrUndoUnsupportedField) {
		t.Fatalf("err=%v, want ErrUndoUnsupportedField", err)
	}
}

// TestUndoMutation_ScrubActorRefused covers gd-fVAy32nWDf acceptance
// criterion 6: a mutation written by ScrubTaint (actor
// model.SystemActorTaintScrub) is refused via ErrUndoUnsupportedMutation
// even though it is an ordinary "update" mutation type that would otherwise
// pass the type check just above it in undoMutation.
func TestUndoMutation_ScrubActorRefused(t *testing.T) {
	bead := &model.Bead{ID: "gd-scrubbed", Title: "[purged]", Type: "task", Status: model.StatusOpen}
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	ms.beads[bead.ID] = bead
	mut := &model.MutationRow{
		BeadID:       bead.ID,
		Actor:        model.SystemActorTaintScrub,
		Field:        "title",
		OldValue:     "sensitive title",
		NewValue:     "[purged]",
		MutationType: model.MutationTypeUpdate,
		CreatedAt:    time.Now(),
	}
	if err := ms.RecordMutation(context.Background(), mut); err != nil {
		t.Fatalf("seeding mutation: %v", err)
	}
	muts, _ := ms.ListMutations(context.Background(), bead.ID)

	_, _, err := s.undoMutation(context.Background(), muts[0].ID, DefaultUndoRetentionDays)
	if !errors.Is(err, ErrUndoUnsupportedMutation) {
		t.Fatalf("err=%v, want ErrUndoUnsupportedMutation", err)
	}
}

// TestUndoMutation_NonScrubUpdateOnSameBeadStillUndoable is the control for
// the test above: a bead that was scrubbed can still have an ordinary,
// non-scrub mutation on it undone normally — the refusal above keys off the
// mutation's own actor, not the bead.
func TestUndoMutation_NonScrubUpdateOnSameBeadStillUndoable(t *testing.T) {
	bead := &model.Bead{ID: "gd-scrubbed-2", Title: "New Title", Type: "task", Kind: model.KindIssue, Status: model.StatusOpen}
	s, _, mutID := newUndoFixture(t, "gd-scrubbed-2", bead, "title", "Old Title", "New Title")

	updated, _, err := s.undoMutation(context.Background(), mutID, DefaultUndoRetentionDays)
	if err != nil {
		t.Fatalf("undoMutation: %v", err)
	}
	if updated.Title != "Old Title" {
		t.Errorf("title=%q, want %q", updated.Title, "Old Title")
	}
}

func TestUndoMutation_OutOfWindow(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	ms.beads["gd-old"] = &model.Bead{ID: "gd-old", Title: "Now", Type: "task", Status: model.StatusOpen}
	mut := &model.MutationRow{
		BeadID:       "gd-old",
		Field:        "title",
		OldValue:     "Then",
		NewValue:     "Now",
		MutationType: model.MutationTypeUpdate,
		CreatedAt:    time.Now().Add(-60 * 24 * time.Hour), // 60 days ago
	}
	_ = ms.RecordMutation(context.Background(), mut)
	// RecordMutation overwrites CreatedAt only when zero; ours is set, so it stays.
	muts, _ := ms.ListMutations(context.Background(), "gd-old")

	_, _, err := s.undoMutation(context.Background(), muts[0].ID, DefaultUndoRetentionDays)
	if !errors.Is(err, ErrUndoOutOfWindow) {
		t.Fatalf("err=%v, want ErrUndoOutOfWindow", err)
	}
}
