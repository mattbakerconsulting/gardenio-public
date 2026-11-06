package sync

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
)

// mockDestination records calls to Write.
type mockDestination struct {
	writes atomic.Int64
	last   atomic.Value // []byte
}

func (d *mockDestination) Write(_ context.Context, data []byte) error {
	d.writes.Add(1)
	cp := make([]byte, len(data))
	copy(cp, data)
	d.last.Store(cp)
	return nil
}

func TestSchedulerStartStop(t *testing.T) {
	ms := newMockStore()
	now := time.Now().UTC()
	ms.beads["gd-1"] = &model.Bead{ID: "gd-1", Kind: model.KindIssue, Type: model.TypeTask, Title: "T1", Status: model.StatusOpen, CreatedAt: now, UpdatedAt: now}
	ms.configs["view:inbox"] = &model.Config{Key: "view:inbox", Value: json.RawMessage(`{}`), CreatedAt: now, UpdatedAt: now}

	dest := &mockDestination{}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	sched := NewScheduler(ms, []Destination{dest}, 50*time.Millisecond, logger)
	sched.Start()

	// Wait for at least the initial sync + one tick.
	time.Sleep(120 * time.Millisecond)
	sched.Stop()

	if writes := dest.writes.Load(); writes < 2 {
		t.Fatalf("expected at least 2 writes, got %d", writes)
	}

	// Verify last written data is valid JSONL.
	data, ok := dest.last.Load().([]byte)
	if !ok || len(data) == 0 {
		t.Fatal("expected non-empty data")
	}

	lines := nonEmptyLines(string(data))
	// 1 header + 1 bead + 1 config = 3
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d", len(lines))
	}
}

func TestSchedulerStop_NoStart(t *testing.T) {
	ms := newMockStore()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	sched := NewScheduler(ms, nil, time.Minute, logger)
	// Stop without Start should not panic.
	sched.Stop()
}

func TestSchedulerMultipleDestinations(t *testing.T) {
	ms := newMockStore()
	dest1 := &mockDestination{}
	dest2 := &mockDestination{}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	sched := NewScheduler(ms, []Destination{dest1, dest2}, time.Second, logger)
	sched.Start()

	// Wait for the initial sync.
	time.Sleep(50 * time.Millisecond)
	sched.Stop()

	if dest1.writes.Load() < 1 {
		t.Fatal("dest1 expected at least 1 write")
	}
	if dest2.writes.Load() < 1 {
		t.Fatal("dest2 expected at least 1 write")
	}
}
