package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
)

func TestClearedIfZero(t *testing.T) {
	now := time.Now()
	zero := time.Time{}
	if got := clearedIfZero(nil); got != nil {
		t.Errorf("clearedIfZero(nil) = %v, want nil", got)
	}
	if got := clearedIfZero(&zero); got != nil {
		t.Errorf("clearedIfZero(zero) = %v, want nil", got)
	}
	if got := clearedIfZero(&now); got != &now {
		t.Errorf("clearedIfZero(now) = %v, want the same pointer", got)
	}
}

func TestUpdateBeadInputScrubProse(t *testing.T) {
	in := updateBeadInput{Heraldry: strPtr("none"), ShortTitle: strPtr("  fix   it  ")}
	if err := in.scrubProse(); err != nil {
		t.Fatalf("scrubProse: %v", err)
	}
	if *in.Heraldry != "" {
		t.Errorf("Heraldry = %q, want \"\" for none", *in.Heraldry)
	}
	if want := model.NormalizeShortTitle("  fix   it  "); *in.ShortTitle != want {
		t.Errorf("ShortTitle = %q, want %q", *in.ShortTitle, want)
	}

	bad := updateBeadInput{Heraldry: strPtr("not arms")}
	if err := bad.scrubProse(); err == nil {
		t.Error("scrubProse with unparseable arms: want an error")
	}
}

func TestNewBeadUpdateSnapshotsAndTakesResealFlag(t *testing.T) {
	bead := &model.Bead{ID: "b1", Type: model.TypeTask, Status: model.StatusClosed}
	in := &updateBeadInput{Fields: json.RawMessage(`{"a":1}`)}
	u := newBeadUpdate(bead, in)
	if !u.wasClosed || u.priorStatus != model.StatusClosed {
		t.Errorf("wasClosed=%v priorStatus=%q, want true/closed", u.wasClosed, u.priorStatus)
	}
	if u.in != in || u.bead != bead {
		t.Error("beadUpdate must hold the caller's input and bead, not copies")
	}
	if u.changes == nil || u.oldValues == nil {
		t.Error("change maps must be initialised")
	}
}

func TestBeadUpdateDropClosedStrayWrites(t *testing.T) {
	tests := []struct {
		name       string
		status     model.Status
		in         updateBeadInput
		wantEmpty  bool
		wantFields string // "" means nil
	}{
		{
			name:      "closed bead, only transient fields",
			status:    model.StatusClosed,
			in:        updateBeadInput{Fields: json.RawMessage(`{"agent_state":"working"}`), Notes: strPtr("n")},
			wantEmpty: true,
		},
		{
			name:       "closed bead, transient and real fields",
			status:     model.StatusClosed,
			in:         updateBeadInput{Fields: json.RawMessage(`{"agent_state":"working","x":1}`)},
			wantFields: `{"x":1}`,
		},
		{
			name:       "closed bead reopened in the same patch keeps transient fields",
			status:     model.StatusClosed,
			in:         updateBeadInput{Status: strPtr("open"), Fields: json.RawMessage(`{"agent_state":""}`)},
			wantFields: `{"agent_state":""}`,
		},
		{
			name:       "open bead untouched",
			status:     model.StatusOpen,
			in:         updateBeadInput{Fields: json.RawMessage(`{"agent_state":"working"}`)},
			wantFields: `{"agent_state":"working"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := tt.in
			u := &beadUpdate{in: &in, bead: &model.Bead{Status: tt.status}}
			if got := u.dropClosedStrayWrites(); got != tt.wantEmpty {
				t.Errorf("empty = %v, want %v", got, tt.wantEmpty)
			}
			if got := string(in.Fields); got != tt.wantFields {
				t.Errorf("Fields = %q, want %q", got, tt.wantFields)
			}
		})
	}
}

func TestBeadUpdateApplyColumns(t *testing.T) {
	zero := time.Time{}
	due := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	bead := &model.Bead{
		Title: "old", ShortTitle: "authored", Heraldry: "🔥", Status: model.StatusOpen,
		Assignee: "alice", Labels: []string{"project:x"}, DueAt: &due,
	}
	in := &updateBeadInput{
		Title:                 strPtr("new"),
		ShortTitle:            strPtr("groomed"),
		OnlyIfShortTitleEmpty: true,
		Heraldry:              strPtr("🔥"),
		Assignee:              strPtr("bob"),
		DueAt:                 &zero,
		dueAtSet:              true,
	}
	u := newBeadUpdate(bead, in)
	u.applyColumns()

	if bead.Title != "new" || u.oldValues["title"] != "old" || u.changes["title"] != "new" {
		t.Errorf("title: bead=%q old=%v new=%v", bead.Title, u.oldValues["title"], u.changes["title"])
	}
	if bead.ShortTitle != "authored" {
		t.Errorf("an authored short_title must win over a fill, got %q", bead.ShortTitle)
	}
	if _, ok := u.changes["short_title"]; ok {
		t.Error("a refused short_title fill must not be a change")
	}
	if _, ok := u.changes["heraldry"]; ok {
		t.Error("unchanged arms must not be a change")
	}
	if bead.DueAt != nil || u.oldValues["due_at"] != &due {
		t.Errorf("zero due_at must clear it: got %v, old %v", bead.DueAt, u.oldValues["due_at"])
	}
	if u.priorAssignee != "alice" || bead.Assignee != "bob" {
		t.Errorf("priorAssignee=%q assignee=%q, want alice/bob", u.priorAssignee, bead.Assignee)
	}
	if len(u.priorLabels) != 1 || u.priorLabels[0] != "project:x" {
		t.Errorf("priorLabels = %v", u.priorLabels)
	}
	if _, ok := u.changes["status"]; ok {
		t.Error("status not in the patch must not be a change")
	}
}

func TestBeadUpdateMergeFields(t *testing.T) {
	bead := &model.Bead{Type: model.TypeTask, Fields: json.RawMessage(`{"keep":1,"drop":"x","same":"s"}`)}
	in := &updateBeadInput{Fields: json.RawMessage(`{"drop":null,"same":"s","add":true}`)}
	u := newBeadUpdate(bead, in)
	if err := u.mergeFields(); err != nil {
		t.Fatalf("mergeFields: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(bead.Fields, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["drop"]; ok {
		t.Error("a null in the patch must delete the key")
	}
	if got["keep"] != float64(1) || got["add"] != true || got["same"] != "s" {
		t.Errorf("merged fields = %v", got)
	}
	if u.oldValues["fields"] != `{"keep":1,"drop":"x","same":"s"}` {
		t.Errorf("oldValues[fields] = %v", u.oldValues["fields"])
	}
	if _, ok := u.changes["fields"]; !ok {
		t.Error("changes must carry fields")
	}
	if _, ok := u.fieldChanges["same"]; ok {
		t.Error("an unchanged key must not be a per-field change")
	}
	if v, ok := u.fieldChanges["drop"]; !ok || v != nil {
		t.Errorf("fieldChanges[drop] = %v, %v; want nil, true", v, ok)
	}
}

func TestBeadUpdateReconcileClosedAt(t *testing.T) {
	closed := &beadUpdate{bead: &model.Bead{Status: model.StatusClosed}, changes: map[string]any{}, oldValues: map[string]any{}}
	closed.reconcileClosedAt()
	if closed.bead.ClosedAt == nil {
		t.Error("a closed bead must get a ClosedAt")
	}
	if _, ok := closed.changes["closed_at"]; !ok {
		t.Error("stamping ClosedAt must be a change")
	}

	now := time.Now()
	reopened := &beadUpdate{bead: &model.Bead{Status: model.StatusOpen, ClosedAt: &now}, changes: map[string]any{}, oldValues: map[string]any{}}
	reopened.reconcileClosedAt()
	if reopened.bead.ClosedAt != nil {
		t.Error("an open bead must lose its ClosedAt")
	}
	if reopened.oldValues["closed_at"] != &now {
		t.Errorf("oldValues[closed_at] = %v", reopened.oldValues["closed_at"])
	}

	open := &beadUpdate{bead: &model.Bead{Status: model.StatusOpen}, changes: map[string]any{}, oldValues: map[string]any{}}
	open.reconcileClosedAt()
	if len(open.changes) != 0 {
		t.Errorf("no ClosedAt change expected, got %v", open.changes)
	}
}

func TestBeadUpdatePublishChanges(t *testing.T) {
	u := &beadUpdate{changes: map[string]any{"status": model.StatusClosed, "fields": "blob"}}
	if got := u.publishChanges(); len(got) != 2 {
		t.Errorf("without field changes the change set is published as is, got %v", got)
	}

	u.fieldChanges = map[string]any{"status": "custom", "enabled": false}
	got := u.publishChanges()
	if got["status"] != model.StatusClosed {
		t.Errorf("a reserved column key must win over a custom field, got %v", got["status"])
	}
	if got["enabled"] != false || got["fields"] != "blob" {
		t.Errorf("published = %v", got)
	}
	if _, ok := u.changes["enabled"]; ok {
		t.Error("the overlay must not leak into the recorded change set")
	}
}

func TestBeadUpdateValidateDocPayloadUpdateSkipsWithoutDescriptionChange(t *testing.T) {
	u := &beadUpdate{bead: &model.Bead{Type: "doc"}, changes: map[string]any{"title": "x"}}
	if err := u.validateDocPayloadUpdate(); err != nil {
		t.Errorf("a patch that does not write the description must pass, got %v", err)
	}
}

func TestCheckUpdatedConversationReleaseIsNeverNil(t *testing.T) {
	s := &BeadsServer{}
	u := &beadUpdate{
		in:        &updateBeadInput{},
		bead:      &model.Bead{Type: model.TypeTask},
		changes:   map[string]any{"title": "x"},
		oldValues: map[string]any{},
	}
	release, err := s.checkUpdatedConversation(context.Background(), u)
	if err != nil {
		t.Fatalf("checkUpdatedConversation: %v", err)
	}
	if release == nil {
		t.Fatal("release must never be nil")
	}
	release()
	if !s.channelWriteMu.TryLock() || !s.cardWriteMu.TryLock() {
		t.Error("a task patch must not leave a write lock held")
	}
}
