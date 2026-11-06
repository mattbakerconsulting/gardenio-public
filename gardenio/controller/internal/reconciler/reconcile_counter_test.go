package reconciler

import (
	"context"
	"errors"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// fakeReconcileRecorder records RecordReconcile calls for assertions.
type fakeReconcileRecorder struct {
	calls []recordedReconcile
}

type recordedReconcile struct {
	controller string
	ok         bool
}

func (f *fakeReconcileRecorder) RecordReconcile(controller string, ok bool) {
	f.calls = append(f.calls, recordedReconcile{controller: controller, ok: ok})
}

// TestReconcile_RecordsOkOutcome verifies that a successful reconcile pass
// records result=ok against the agent controller (ka-0000001213).
func TestReconcile_RecordsOkOutcome(t *testing.T) {
	lister := &mockLister{
		beads: []beadsapi.AgentBead{
			{ID: "bd-1", Project: "proj", Mode: "crew", Role: "dev", AgentName: "alpha"},
		},
	}
	mgr := &mockManager{pods: nil}
	rec := &fakeReconcileRecorder{}

	r := New(lister, mgr, testConfig("ns"), testLogger(), simpleSpecBuilder("img:v1"))
	r.SetReconcileRecorder(rec)

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(rec.calls) != 1 {
		t.Fatalf("expected exactly 1 reconcile record, got %d: %+v", len(rec.calls), rec.calls)
	}
	got := rec.calls[0]
	if got.controller != reconcilerControllerName {
		t.Errorf("expected controller %q, got %q", reconcilerControllerName, got.controller)
	}
	if !got.ok {
		t.Errorf("expected ok=true on a successful reconcile, got ok=false")
	}
}

// TestReconcile_RecordsErrorOutcome verifies that a reconcile pass which
// returns an error records result=error. A lister error makes Reconcile return
// early with a wrapped error (ka-0000001213).
func TestReconcile_RecordsErrorOutcome(t *testing.T) {
	lister := &mockLister{err: errors.New("daemon unreachable")}
	mgr := &mockManager{pods: nil}
	rec := &fakeReconcileRecorder{}

	r := New(lister, mgr, testConfig("ns"), testLogger(), simpleSpecBuilder("img:v1"))
	r.SetReconcileRecorder(rec)

	if err := r.Reconcile(context.Background()); err == nil {
		t.Fatal("expected an error from Reconcile, got nil")
	}

	if len(rec.calls) != 1 {
		t.Fatalf("expected exactly 1 reconcile record, got %d: %+v", len(rec.calls), rec.calls)
	}
	got := rec.calls[0]
	if got.controller != reconcilerControllerName {
		t.Errorf("expected controller %q, got %q", reconcilerControllerName, got.controller)
	}
	if got.ok {
		t.Errorf("expected ok=false on a failed reconcile, got ok=true")
	}
}

// TestReconcile_NoRecorderIsSafe verifies that Reconcile does not panic when no
// recorder is installed (the recorder is optional).
func TestReconcile_NoRecorderIsSafe(t *testing.T) {
	lister := &mockLister{beads: nil}
	mgr := &mockManager{pods: nil}

	r := New(lister, mgr, testConfig("ns"), testLogger(), simpleSpecBuilder("img:v1"))
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
