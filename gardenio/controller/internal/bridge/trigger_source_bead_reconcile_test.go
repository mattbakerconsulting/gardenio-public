package bridge

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// fakeReconcileSource captures InjectBeadEvent calls for assertions.
type fakeReconcileSource struct {
	mu     sync.Mutex
	events []struct {
		Topic string
		Bead  *BeadEvent
	}
}

func (f *fakeReconcileSource) InjectBeadEvent(topic string, bead *BeadEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, struct {
		Topic string
		Bead  *BeadEvent
	}{Topic: topic, Bead: bead})
}

func (f *fakeReconcileSource) snapshot() []struct {
	Topic string
	Bead  *BeadEvent
} {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]struct {
		Topic string
		Bead  *BeadEvent
	}, len(f.events))
	copy(out, f.events)
	return out
}

// fakeWatermarkStore is an in-memory ReconcileWatermarkStore.
type fakeWatermarkStore struct {
	mu sync.Mutex
	t  time.Time
}

func (f *fakeWatermarkStore) GetLastReconcileAt() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeWatermarkStore) SetLastReconcileAt(t time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = t
	return nil
}

// fakeReconcileDaemon returns a queued slice of bead lists.
type fakeReconcileDaemon struct {
	mu       sync.Mutex
	calls    []beadsapi.ListBeadsQuery
	results  [][]*beadsapi.BeadDetail
	errOnce  error
	errAlways bool
}

func (f *fakeReconcileDaemon) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, q)
	if f.errAlways {
		return nil, errors.New("daemon offline")
	}
	if f.errOnce != nil {
		err := f.errOnce
		f.errOnce = nil
		return nil, err
	}
	if len(f.results) == 0 {
		return &beadsapi.ListBeadsResult{Beads: nil}, nil
	}
	res := f.results[0]
	f.results = f.results[1:]
	return &beadsapi.ListBeadsResult{Beads: res, Total: len(res)}, nil
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{}))
}

func TestReconciler_BootstrapsWatermark(t *testing.T) {
	src := &fakeReconcileSource{}
	state := &fakeWatermarkStore{}
	daemon := &fakeReconcileDaemon{}

	r := NewBeadStateReconciler(ReconcilerConfig{
		Source: src, State: state, Daemon: daemon,
		Interval: time.Hour, Logger: quietLogger(),
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.Start(ctx)
		close(done)
	}()
	// Let bootstrap + first tick run, then cancel.
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	if state.GetLastReconcileAt().IsZero() {
		t.Fatalf("expected watermark to be seeded, got zero")
	}
}

func TestReconciler_InjectsNewBeads(t *testing.T) {
	src := &fakeReconcileSource{}
	now := time.Now().UTC()
	state := &fakeWatermarkStore{t: now.Add(-time.Minute)}
	daemon := &fakeReconcileDaemon{
		results: [][]*beadsapi.BeadDetail{
			{
				{ID: "gd-A", Type: "task", Title: "newer", Status: "open", UpdatedAt: now},
				{ID: "gd-B", Type: "task", Title: "skip", Status: "open", UpdatedAt: now.Add(-2 * time.Minute)}, // before cursor
			},
		},
	}

	r := NewBeadStateReconciler(ReconcilerConfig{
		Source: src, State: state, Daemon: daemon,
		Interval: time.Hour, Logger: quietLogger(),
	})
	r.lastReconcile = state.GetLastReconcileAt()
	r.runOnce(context.Background())

	got := src.snapshot()
	if len(got) != 1 {
		t.Fatalf("expected 1 injected event (gd-A only), got %d: %+v", len(got), got)
	}
	if got[0].Topic != reconcileTopic {
		t.Errorf("expected topic %q, got %q", reconcileTopic, got[0].Topic)
	}
	if got[0].Bead.ID != "gd-A" {
		t.Errorf("expected bead gd-A, got %q", got[0].Bead.ID)
	}
	if !state.GetLastReconcileAt().After(now.Add(-time.Minute)) {
		t.Errorf("watermark did not advance: got %v", state.GetLastReconcileAt())
	}
}

func TestReconciler_AdvancesWatermarkOnEmptyTick(t *testing.T) {
	src := &fakeReconcileSource{}
	old := time.Now().UTC().Add(-time.Hour)
	state := &fakeWatermarkStore{t: old}
	daemon := &fakeReconcileDaemon{} // empty results

	r := NewBeadStateReconciler(ReconcilerConfig{
		Source: src, State: state, Daemon: daemon,
		Interval: time.Hour, Logger: quietLogger(),
	})
	r.lastReconcile = state.GetLastReconcileAt()
	r.runOnce(context.Background())

	if len(src.snapshot()) != 0 {
		t.Errorf("expected no injections on empty tick, got %d", len(src.snapshot()))
	}
	if !state.GetLastReconcileAt().After(old) {
		t.Errorf("watermark should advance even when no beads match; got %v want > %v",
			state.GetLastReconcileAt(), old)
	}
}

func TestReconciler_PreservesWatermarkOnDaemonError(t *testing.T) {
	src := &fakeReconcileSource{}
	original := time.Now().UTC().Add(-time.Hour)
	state := &fakeWatermarkStore{t: original}
	daemon := &fakeReconcileDaemon{errAlways: true}

	r := NewBeadStateReconciler(ReconcilerConfig{
		Source: src, State: state, Daemon: daemon,
		Interval: time.Hour, Logger: quietLogger(),
	})
	r.lastReconcile = state.GetLastReconcileAt()
	r.runOnce(context.Background())

	if got := state.GetLastReconcileAt(); !got.Equal(original) {
		t.Errorf("watermark should not advance on daemon error; got %v want %v", got, original)
	}
	if r.consecutiveFail != 1 {
		t.Errorf("expected consecutiveFail=1, got %d", r.consecutiveFail)
	}
}

func TestReconciler_OverlapAppliedToQuery(t *testing.T) {
	src := &fakeReconcileSource{}
	cursor := time.Now().UTC().Truncate(time.Second)
	state := &fakeWatermarkStore{t: cursor}
	daemon := &fakeReconcileDaemon{}

	r := NewBeadStateReconciler(ReconcilerConfig{
		Source: src, State: state, Daemon: daemon,
		Interval: time.Hour, Overlap: 5 * time.Second, Logger: quietLogger(),
	})
	r.lastReconcile = state.GetLastReconcileAt()
	r.runOnce(context.Background())

	if len(daemon.calls) != 1 {
		t.Fatalf("expected 1 daemon call, got %d", len(daemon.calls))
	}
	want := cursor.Add(-5 * time.Second)
	if !daemon.calls[0].UpdatedAfter.Equal(want) {
		t.Errorf("expected UpdatedAfter=%v, got %v", want, daemon.calls[0].UpdatedAfter)
	}
}

func TestReconciler_SkipsDuplicatesWithinOverlap(t *testing.T) {
	src := &fakeReconcileSource{}
	now := time.Now().UTC()
	state := &fakeWatermarkStore{t: now}
	// Daemon returns a bead with UpdatedAt == cursor (i.e. inside the
	// overlap rewind but at-or-before the previous watermark). The
	// defence-in-depth check in runOnce should skip it.
	daemon := &fakeReconcileDaemon{
		results: [][]*beadsapi.BeadDetail{
			{{ID: "gd-old", UpdatedAt: now}},
		},
	}

	r := NewBeadStateReconciler(ReconcilerConfig{
		Source: src, State: state, Daemon: daemon,
		Interval: time.Hour, Logger: quietLogger(),
	})
	r.lastReconcile = state.GetLastReconcileAt()
	r.runOnce(context.Background())

	if got := len(src.snapshot()); got != 0 {
		t.Errorf("expected duplicate to be skipped, got %d injections", got)
	}
}

func TestBeadDetailToEvent(t *testing.T) {
	in := &beadsapi.BeadDetail{
		ID:       "gd-X",
		Type:     "task",
		Title:    "t",
		Status:   "open",
		Assignee: "alice",
		Labels:   []string{"project:gardenio"},
		Fields:   map[string]string{"k": "v"},
		Priority: 2,
	}
	out := beadDetailToEvent(in)
	if out == nil || out.ID != "gd-X" || out.Fields["k"] != "v" {
		t.Fatalf("unexpected projection: %+v", out)
	}
	// Mutating output must not leak into input.
	out.Fields["k"] = "MUTATED"
	if in.Fields["k"] != "v" {
		t.Errorf("Fields map shared between input and output")
	}
	if beadDetailToEvent(nil) != nil {
		t.Errorf("nil input should project to nil")
	}
	if beadDetailToEvent(&beadsapi.BeadDetail{}) != nil {
		t.Errorf("empty ID should project to nil")
	}
}

func TestNewBeadStateReconciler_PanicsOnMissingDeps(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Errorf("expected panic when Source is nil")
		}
	}()
	NewBeadStateReconciler(ReconcilerConfig{
		State:  &fakeWatermarkStore{},
		Daemon: &fakeReconcileDaemon{},
	})
}
