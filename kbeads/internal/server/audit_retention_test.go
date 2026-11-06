package server

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"gardenio/kbeads/internal/model"
)

func TestSweepAuditBeads_ClosesExpired(t *testing.T) {
	srv, ms, _ := newTestServer()

	// Create audit beads with different ages.
	now := time.Now().UTC()
	srv.recordAuditEntry(context.Background(), auditEntry{
		Actor: "old-actor", Action: "create", Outcome: "deny",
	})
	srv.recordAuditEntry(context.Background(), auditEntry{
		Actor: "recent-actor", Action: "update", Outcome: "allow",
	})

	// Make the first audit bead old (100 days) and keep the second recent.
	auditCount := 0
	for _, b := range ms.beads {
		if b.Type != "audit" {
			continue
		}
		if auditCount == 0 {
			b.CreatedAt = now.Add(-100 * 24 * time.Hour)
		} else {
			b.CreatedAt = now.Add(-10 * 24 * time.Hour)
		}
		auditCount++
	}

	// Sweep with 90-day retention.
	srv.sweepAuditBeads(context.Background(), 90)

	// Count open vs closed audit beads.
	var openCount, closedCount int
	for _, b := range ms.beads {
		if b.Type != "audit" {
			continue
		}
		if b.Status == model.StatusClosed {
			closedCount++
		} else {
			openCount++
		}
	}

	if closedCount != 1 {
		t.Errorf("closed=%d, want 1 (the 100-day-old bead)", closedCount)
	}
	if openCount != 1 {
		t.Errorf("open=%d, want 1 (the 10-day-old bead)", openCount)
	}
}

func TestSweepAuditBeads_NoExpired(t *testing.T) {
	srv, ms, _ := newTestServer()

	srv.recordAuditEntry(context.Background(), auditEntry{
		Actor: "recent", Action: "create", Outcome: "allow",
	})

	// All beads are recent — nothing should be closed.
	srv.sweepAuditBeads(context.Background(), 90)

	for _, b := range ms.beads {
		if b.Type == "audit" && b.Status == model.StatusClosed {
			t.Error("no audit beads should be closed")
		}
	}
}

func TestSweepAuditBeads_SkipsAlreadyClosed(t *testing.T) {
	srv, ms, _ := newTestServer()

	now := time.Now().UTC()
	srv.recordAuditEntry(context.Background(), auditEntry{
		Actor: "old", Action: "delete", Outcome: "deny",
	})

	// Make it old and pre-close it.
	for _, b := range ms.beads {
		if b.Type == "audit" {
			b.CreatedAt = now.Add(-100 * 24 * time.Hour)
			b.Status = model.StatusClosed
		}
	}

	// Sweep should find nothing to close (already closed).
	srv.sweepAuditBeads(context.Background(), 90)

	// Verify it's still closed (no error from double-close attempt).
	for _, b := range ms.beads {
		if b.Type == "audit" && b.Status != model.StatusClosed {
			t.Error("bead should remain closed")
		}
	}
}

// TestSweepAuditBeads_SkipsWhenAnotherReplicaHoldsTheLock pins the
// belt-and-suspenders cross-replica serialization from the Phase 2 2-replica
// spike (gd-XxrObXvc8r): with auditRetentionAdvisoryLockKey already held
// (standing in for a concurrent tick on another replica), this sweep must
// skip its body entirely.
func TestSweepAuditBeads_SkipsWhenAnotherReplicaHoldsTheLock(t *testing.T) {
	srv, ms, _ := newTestServer()

	now := time.Now().UTC()
	srv.recordAuditEntry(context.Background(), auditEntry{
		Actor: "old-actor", Action: "create", Outcome: "deny",
	})
	for _, b := range ms.beads {
		if b.Type == "audit" {
			b.CreatedAt = now.Add(-100 * 24 * time.Hour)
		}
	}

	ms.advisoryLocksMu.Lock()
	if ms.advisoryLocks == nil {
		ms.advisoryLocks = map[int64]bool{}
	}
	ms.advisoryLocks[auditRetentionAdvisoryLockKey] = true
	ms.advisoryLocksMu.Unlock()

	srv.sweepAuditBeads(context.Background(), 90)

	for _, b := range ms.beads {
		if b.Type == "audit" && b.Status == model.StatusClosed {
			t.Error("sweep ran despite the advisory lock being held")
		}
	}
}

func TestRunAuditRetention_StopsOnCancel(t *testing.T) {
	srv, _, _ := newTestServer()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		srv.runAuditRetention(ctx, 90, 10*time.Millisecond)
		close(done)
	}()

	// Let it tick a couple times.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// Good — goroutine exited.
	case <-time.After(2 * time.Second):
		t.Fatal("runAuditRetention did not stop after context cancellation")
	}
}

func TestSweepAuditBeads_ClosedBySystem(t *testing.T) {
	srv, ms, _ := newTestServer()

	now := time.Now().UTC()
	srv.recordAuditEntry(context.Background(), auditEntry{
		Actor: "old-actor", Action: "create", Outcome: "deny",
	})

	for _, b := range ms.beads {
		if b.Type == "audit" {
			b.CreatedAt = now.Add(-100 * 24 * time.Hour)
		}
	}

	srv.sweepAuditBeads(context.Background(), 90)

	for _, b := range ms.beads {
		if b.Type == "audit" && b.Status == model.StatusClosed {
			if b.ClosedBy != "system:audit-retention" {
				t.Errorf("closed_by=%q, want system:audit-retention", b.ClosedBy)
			}
		}
	}
}

// seedRetentionBead puts a bead of the given type, status and age straight
// into the mock store.
func seedRetentionBead(ms *mockStore, id string, typ model.BeadType, status model.Status, age time.Duration) {
	created := time.Now().UTC().Add(-age)
	b := &model.Bead{ID: id, Title: id, Type: typ, Kind: "data", Status: status, Fleet: "default", CreatedAt: created, UpdatedAt: created}
	if status == model.StatusClosed {
		closedAt := created.Add(time.Hour)
		b.ClosedAt = &closedAt
	}
	ms.beads[id] = b
}

const retentionDay = 24 * time.Hour

// TestSweepAuditBeads_DeletesOnlyClosedAuditPastGrace pins delete_only
// (gd-GJzCfUmbQC, decision gd-Z6lkvI37gE): a CLOSED type=audit bead older than
// retention+30d is hard-deleted; an open one that old, a young closed audit
// bead and a non-audit bead all survive.
func TestSweepAuditBeads_DeletesOnlyClosedAuditPastGrace(t *testing.T) {
	srv, ms, _ := newTestServer()
	seedRetentionBead(ms, "gd-aud-closed-old", "audit", model.StatusClosed, 130*retentionDay)
	seedRetentionBead(ms, "gd-aud-open-old", "audit", model.StatusOpen, 130*retentionDay)
	seedRetentionBead(ms, "gd-aud-closed-young", "audit", model.StatusClosed, 100*retentionDay)
	seedRetentionBead(ms, "gd-aud-closed-recent", "audit", model.StatusClosed, 10*retentionDay)
	seedRetentionBead(ms, "gd-task-closed-old", model.TypeTask, model.StatusClosed, 200*retentionDay)

	before := testutil.ToFloat64(auditRetentionDeletedTotal)
	srv.sweepAuditBeads(context.Background(), 90)

	if _, ok := ms.beads["gd-aud-closed-old"]; ok {
		t.Error("closed audit bead past retention+30d must be deleted")
	}
	for _, id := range []string{"gd-aud-open-old", "gd-aud-closed-young", "gd-aud-closed-recent", "gd-task-closed-old"} {
		if _, ok := ms.beads[id]; !ok {
			t.Errorf("%s must survive the sweep", id)
		}
	}
	// The open one is closed this tick, and only deleted on a later tick.
	if b := ms.beads["gd-aud-open-old"]; b == nil || b.Status != model.StatusClosed {
		t.Error("open audit bead past retention must be closed, not deleted")
	}
	if got := testutil.ToFloat64(auditRetentionDeletedTotal) - before; got != 1 {
		t.Errorf("deleted counter delta = %v, want 1", got)
	}
}

// TestSweepAuditBeads_DrainsPastPageSizeInOneTick pins the paging fix: the
// old sweep listed one 500-bead page per daily tick, so it could never keep up
// with ~14k audit writes a day. One tick now drains both passes.
func TestSweepAuditBeads_DrainsPastPageSizeInOneTick(t *testing.T) {
	srv, ms, _ := newTestServer()
	const nOpen, nClosed = 1234, 1111
	for i := range nOpen {
		seedRetentionBead(ms, fmt.Sprintf("gd-aud-open-%04d", i), "audit", model.StatusOpen, 100*retentionDay)
	}
	for i := range nClosed {
		seedRetentionBead(ms, fmt.Sprintf("gd-aud-closed-%04d", i), "audit", model.StatusClosed, 130*retentionDay)
	}

	closedBefore := testutil.ToFloat64(auditRetentionClosedTotal)
	deletedBefore := testutil.ToFloat64(auditRetentionDeletedTotal)
	srv.sweepAuditBeads(context.Background(), 90)

	var open, closed int
	for _, b := range ms.beads {
		if b.Type != "audit" {
			continue
		}
		if b.Status == model.StatusOpen {
			open++
		} else {
			closed++
		}
	}
	if open != 0 || closed != nOpen {
		t.Errorf("after one tick: open=%d closed=%d, want open=0 closed=%d", open, closed, nOpen)
	}
	if got := testutil.ToFloat64(auditRetentionClosedTotal) - closedBefore; got != nOpen {
		t.Errorf("closed counter delta = %v, want %d", got, nOpen)
	}
	if got := testutil.ToFloat64(auditRetentionDeletedTotal) - deletedBefore; got != nClosed {
		t.Errorf("deleted counter delta = %v, want %d", got, nClosed)
	}
}

// TestSweepAuditBeads_StopsAtDeadline pins the per-tick budget: a pass whose
// deadline has passed touches nothing, leaving the work for the next tick.
func TestSweepAuditBeads_StopsAtDeadline(t *testing.T) {
	srv, ms, _ := newTestServer()
	seedRetentionBead(ms, "gd-aud-open-old", "audit", model.StatusOpen, 100*retentionDay)
	seedRetentionBead(ms, "gd-aud-closed-old", "audit", model.StatusClosed, 130*retentionDay)

	srv.sweepAuditBeadsLocked(context.Background(), 90, time.Now().Add(-time.Second))

	if b := ms.beads["gd-aud-open-old"]; b == nil || b.Status != model.StatusOpen {
		t.Error("an expired budget must not close anything")
	}
	if _, ok := ms.beads["gd-aud-closed-old"]; !ok {
		t.Error("an expired budget must not delete anything")
	}
}
