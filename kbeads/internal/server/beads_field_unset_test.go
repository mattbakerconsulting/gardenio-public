package server

import (
	"encoding/json"
	"testing"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/model"
)

// fieldsMapOf decodes a bead's Fields blob into a generic map for assertions.
func fieldsMapOf(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	m := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("unmarshal fields %q: %v", raw, err)
		}
	}
	return m
}

// TestUpdateBead_NullDeletesField_GRPC verifies RFC 7386 merge-patch semantics
// on the gRPC transport: a top-level JSON null in the fields patch DELETES the
// key from the stored fields object, leaving sibling keys untouched. This is
// the fix for ka-0000007246 — restoring a field to "absent", distinct from an
// empty string or a stored null.
func TestUpdateBead_NullDeletesField_GRPC(t *testing.T) {
	srv, ms, ctx := testCtx(t)
	const id = "gd-unset-grpc"
	ms.beads[id] = &model.Bead{
		ID:        id,
		Title:     "role bead",
		Kind:      model.KindIssue,
		Type:      model.TypeTask,
		Status:    model.StatusOpen,
		CreatedBy: defaultTestSubject,
		Fields:    json.RawMessage(`{"model":"claude","account":"acct-1"}`),
	}

	patch := json.RawMessage(`{"model":null}`)
	if _, err := srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{Id: id, Fields: patch}); err != nil {
		t.Fatalf("UpdateBead: %v", err)
	}

	fields := fieldsMapOf(t, ms.beads[id].Fields)
	if _, present := fields["model"]; present {
		t.Errorf("model key should be DELETED, still present: %v", fields)
	}
	if got := fields["account"]; got != "acct-1" {
		t.Errorf("sibling account = %v, want acct-1 (must be untouched)", got)
	}
}

// TestUpdateBead_NullDeletesField_HTTP pins the same delete-on-null behaviour
// on the HTTP/JSON transport. Both transports funnel through updateBead(); this
// guards against a future divergence.
func TestUpdateBead_NullDeletesField_HTTP(t *testing.T) {
	_, ms, h := newTestServer()
	const id = "gd-unset-http"
	ms.beads[id] = &model.Bead{
		ID:        id,
		Title:     "role bead",
		Kind:      model.KindIssue,
		Type:      model.TypeTask,
		Status:    model.StatusOpen,
		CreatedBy: defaultTestSubject,
		Fields:    json.RawMessage(`{"model":"claude","account":"acct-1"}`),
	}

	rec := doJSON(t, h, "PATCH", "/v1/beads/"+id, map[string]any{
		"fields": map[string]any{"model": nil},
	})
	requireStatus(t, rec, 200)

	fields := fieldsMapOf(t, ms.beads[id].Fields)
	if _, present := fields["model"]; present {
		t.Errorf("model key should be DELETED, still present: %v", fields)
	}
	if got := fields["account"]; got != "acct-1" {
		t.Errorf("sibling account = %v, want acct-1 (must be untouched)", got)
	}
}

// TestUpdateBead_StoredNullCannotRegress is the incident regression guard for
// ka-0000001419: a `-f model=null` (which marshals to {"model":null}) must
// leave the stored blob with NO model key at all — neither a literal null nor
// an empty string. The stored-null → re-marshaled-"null" → every-pod-401 chain
// can never recur because the daemon deletes the key at the source.
func TestUpdateBead_StoredNullCannotRegress(t *testing.T) {
	srv, ms, ctx := testCtx(t)
	const id = "gd-null-regress"
	ms.beads[id] = &model.Bead{
		ID:        id,
		Title:     "role bead",
		Kind:      model.KindIssue,
		Type:      model.TypeTask,
		Status:    model.StatusOpen,
		CreatedBy: defaultTestSubject,
		Fields:    json.RawMessage(`{"model":"claude-opus"}`),
	}

	patch := json.RawMessage(`{"model":null}`)
	if _, err := srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{Id: id, Fields: patch}); err != nil {
		t.Fatalf("UpdateBead: %v", err)
	}

	stored := string(ms.beads[id].Fields)
	fields := fieldsMapOf(t, ms.beads[id].Fields)
	if v, present := fields["model"]; present {
		t.Fatalf("model key must be absent after unset, got present with value %v (stored=%q)", v, stored)
	}
}

// TestUpdateBead_UnsetMissingKeyIsNoOp verifies that unsetting a key that was
// never present is a genuine no-op: the stored fields are unchanged and no
// phantom per-field transition is published (perFieldChanges must not emit a
// nil->nil change for an already-absent key).
func TestUpdateBead_UnsetMissingKeyIsNoOp(t *testing.T) {
	srv, ms, ctx := testCtx(t)
	const id = "gd-unset-missing"
	ms.beads[id] = &model.Bead{
		ID:        id,
		Title:     "bead",
		Kind:      model.KindIssue,
		Type:      model.TypeTask,
		Status:    model.StatusOpen,
		CreatedBy: defaultTestSubject,
		Fields:    json.RawMessage(`{"keep":"yes"}`),
	}

	patch := json.RawMessage(`{"absent":null}`)
	if _, err := srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{Id: id, Fields: patch}); err != nil {
		t.Fatalf("UpdateBead: %v", err)
	}

	fields := fieldsMapOf(t, ms.beads[id].Fields)
	if got := fields["keep"]; got != "yes" {
		t.Errorf("keep = %v, want yes (unrelated field must survive)", got)
	}
	if _, present := fields["absent"]; present {
		t.Errorf("absent key should not have been created, got %v", fields)
	}
	// The published per-field overlay must not carry a phantom "absent" key.
	changes := lastUpdatedChanges(t, ms)
	if _, ok := changes["absent"]; ok {
		t.Errorf("phantom nil->nil transition published for already-absent key: %v", changes)
	}
}

// TestUpdateBead_DeleteEmitsPerFieldChange verifies that deleting a PRESENT
// field surfaces the key in the published BeadUpdated.Changes map (with a nil
// value), so a declarative `field_changed` watcher can observe a gardener field
// being cleared.
func TestUpdateBead_DeleteEmitsPerFieldChange(t *testing.T) {
	srv, ms, ctx := testCtx(t)
	const id = "gd-del-change"
	ms.beads[id] = &model.Bead{
		ID:        id,
		Title:     "gardener",
		Kind:      model.KindIssue,
		Type:      model.TypeTask,
		Status:    model.StatusOpen,
		CreatedBy: defaultTestSubject,
		Fields:    json.RawMessage(`{"model":"claude"}`),
	}

	patch := json.RawMessage(`{"model":null}`)
	if _, err := srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{Id: id, Fields: patch}); err != nil {
		t.Fatalf("UpdateBead: %v", err)
	}

	changes := lastUpdatedChanges(t, ms)
	v, ok := changes["model"]
	if !ok {
		t.Fatalf("expected bare 'model' key in changes for a delete, got %v", changes)
	}
	if v != nil {
		t.Errorf("changes[model] = %v, want nil (delete transition)", v)
	}
}
