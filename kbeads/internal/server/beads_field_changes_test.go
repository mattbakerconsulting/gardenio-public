package server

import (
	"encoding/json"
	"testing"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// TestPerFieldChanges exercises the pure diff helper that powers the
// per-field-change overlay on BeadUpdated events (bug B3, ka-0000004185).
func TestPerFieldChanges(t *testing.T) {
	tests := []struct {
		name     string
		existing map[string]any
		patch    map[string]any
		want     map[string]any
	}{
		{
			name:     "new key counts as a change",
			existing: map[string]any{},
			patch:    map[string]any{"enabled": "false"},
			want:     map[string]any{"enabled": "false"},
		},
		{
			name:     "rotated value surfaces",
			existing: map[string]any{"enabled": "true"},
			patch:    map[string]any{"enabled": "false"},
			want:     map[string]any{"enabled": "false"},
		},
		{
			name:     "no-op rewrite is omitted",
			existing: map[string]any{"enabled": "false"},
			patch:    map[string]any{"enabled": "false"},
			want:     nil,
		},
		{
			name:     "only changed keys, untouched keys dropped",
			existing: map[string]any{"enabled": "true", "label": "x"},
			patch:    map[string]any{"enabled": "false", "label": "x"},
			want:     map[string]any{"enabled": "false"},
		},
		{
			name:     "empty patch yields nil",
			existing: map[string]any{"enabled": "true"},
			patch:    map[string]any{},
			want:     nil,
		},
		{
			name:     "bool transition (typed) surfaces",
			existing: map[string]any{"flag": true},
			patch:    map[string]any{"flag": false},
			want:     map[string]any{"flag": false},
		},
		{
			name:     "delete of present key surfaces as nil transition",
			existing: map[string]any{"model": "claude"},
			patch:    map[string]any{"model": nil},
			want:     map[string]any{"model": nil},
		},
		{
			name:     "unset of already-absent key is a no-op",
			existing: map[string]any{"other": "x"},
			patch:    map[string]any{"model": nil},
			want:     nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := perFieldChanges(tt.existing, tt.patch)
			if len(got) != len(tt.want) {
				t.Fatalf("perFieldChanges() = %v, want %v", got, tt.want)
			}
			for k, v := range tt.want {
				gv, ok := got[k]
				if !ok {
					t.Errorf("missing key %q in %v", k, got)
					continue
				}
				if gv != v {
					t.Errorf("key %q = %v, want %v", k, gv, v)
				}
			}
		})
	}
}

// TestUpdateBead_EmitsPerFieldChange is the B3 end-to-end assertion: a PATCH
// that flips a custom field (`enabled`) publishes a BeadUpdated event whose
// Changes map carries the BARE field key, so a declarative
// `enabled changed_to false` trigger can match it. Pre-fix the daemon only
// ever emitted the opaque "fields" key (ka-0000004185 / ka-0000000194).
func TestUpdateBead_EmitsPerFieldChange(t *testing.T) {
	srv, ms, ctx := testCtx(t)
	const id = "gd-cap1"
	// Seed `enabled` so the field-merge has an old value to diff against and
	// so type-config validation grandfathers the key on the subsequent PATCH.
	ms.beads[id] = &model.Bead{
		ID:        id,
		Title:     "janitor gardener",
		Kind:      model.KindIssue,
		Type:      model.TypeTask,
		Status:    model.StatusOpen,
		CreatedBy: defaultTestSubject,
		Fields:    json.RawMessage(`{"enabled":"true"}`),
	}

	patch := json.RawMessage(`{"enabled":"false"}`)
	if _, err := srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{Id: id, Fields: patch}); err != nil {
		t.Fatalf("UpdateBead: %v", err)
	}

	changes := lastUpdatedChanges(t, ms)
	// The per-field key is present with the new value...
	if got, ok := changes["enabled"]; !ok || got != "false" {
		t.Errorf("changes[enabled] = %v (present=%v), want \"false\"", changes["enabled"], ok)
	}
	// ...alongside the back-compat opaque "fields" key.
	if _, ok := changes["fields"]; !ok {
		t.Errorf("expected opaque \"fields\" key to remain in changes, got %v", changes)
	}
}

// TestUpdateBead_CustomFieldNeverClobbersReservedKey guards the collision case:
// a type-config field that happens to share a reserved top-level name (e.g.
// "status") must not overwrite the real column transition in the published
// changes map.
func TestUpdateBead_CustomFieldNeverClobbersReservedKey(t *testing.T) {
	srv, ms, ctx := testCtx(t)
	const id = "gd-cap2"
	ms.beads[id] = &model.Bead{
		ID:        id,
		Title:     "bead with shadowing field",
		Kind:      model.KindIssue,
		Type:      model.TypeTask,
		Status:    model.StatusOpen,
		Assignee:  "agent-x",
		CreatedBy: defaultTestSubject,
		Fields:    json.RawMessage(`{"status":"warm"}`),
	}

	// Flip the real status column AND the shadowing custom field in one PATCH.
	newStatus := "in_progress"
	patch := json.RawMessage(`{"status":"hot"}`)
	if _, err := srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{Id: id, Status: &newStatus, Fields: patch}); err != nil {
		t.Fatalf("UpdateBead: %v", err)
	}

	changes := lastUpdatedChanges(t, ms)
	// The reserved column transition wins the bare "status" key.
	if got := changes["status"]; got != string(model.StatusInProgress) {
		t.Errorf("changes[status] = %v, want the column transition %q", got, model.StatusInProgress)
	}
}

// lastUpdatedChanges decodes the most recently recorded beads.bead.updated
// event payload and returns its changes map.
func lastUpdatedChanges(t *testing.T, ms *mockStore) map[string]any {
	t.Helper()
	for i := len(ms.events) - 1; i >= 0; i-- {
		if ms.events[i].Topic != events.TopicBeadUpdated {
			continue
		}
		var payload struct {
			Changes map[string]any `json:"changes"`
		}
		if err := json.Unmarshal(ms.events[i].Payload, &payload); err != nil {
			t.Fatalf("unmarshal updated payload: %v", err)
		}
		return payload.Changes
	}
	t.Fatalf("no %s event recorded among %d events", events.TopicBeadUpdated, len(ms.events))
	return nil
}

// TestUpdateBead_EmitsActor: the published BeadUpdated event carries the
// server-resolved actor (Plans v1 S3, ka-0000002473) so live SSE subscribers
// — e.g. the beads-viewer plan-narrative stream — can attribute an edit to
// the acting agent without a second lookup. The actor is derived from the
// authenticated identity (actorFromContext), never client-supplied.
func TestUpdateBead_EmitsActor(t *testing.T) {
	srv, ms, ctx := testCtxAs(t, "gardenio-plan-asst-1")
	const id = "gd-doc9"
	ms.beads[id] = &model.Bead{
		ID:     id,
		Title:  "plan narrative doc",
		Kind:   model.KindData,
		Type:   "doc",
		Status: model.StatusOpen,
		// The assistant edits a doc it created (ownership gate).
		CreatedBy: "gardenio-plan-asst-1",
		Fields:    json.RawMessage(`{"format":"md","content":"old"}`),
	}

	patch := json.RawMessage(`{"content":"new"}`)
	if _, err := srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{Id: id, Fields: patch}); err != nil {
		t.Fatalf("UpdateBead: %v", err)
	}

	for i := len(ms.events) - 1; i >= 0; i-- {
		if ms.events[i].Topic != events.TopicBeadUpdated {
			continue
		}
		var payload struct {
			Actor   string         `json:"actor"`
			Changes map[string]any `json:"changes"`
		}
		if err := json.Unmarshal(ms.events[i].Payload, &payload); err != nil {
			t.Fatalf("unmarshal updated payload: %v", err)
		}
		if payload.Actor != "gardenio-plan-asst-1" {
			t.Errorf("payload actor = %q, want the authenticated subject", payload.Actor)
		}
		// The per-field overlay still carries the bare content key alongside.
		if _, ok := payload.Changes["content"]; !ok {
			t.Errorf("expected bare per-field 'content' key in changes, got %v", payload.Changes)
		}
		return
	}
	t.Fatalf("no %s event recorded", events.TopicBeadUpdated)
}
