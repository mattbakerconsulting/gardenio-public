package server

import (
	"context"
	"log/slog"
	"time"

	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/store"
)

// auditRetentionAdvisoryLockKey is the Postgres advisory-lock key
// serializing the audit-retention sweep across replicas (Phase 2 2-replica
// spike, gd-XxrObXvc8r). CloseBead has no `WHERE status='open'` guard, so a
// second concurrent close of the same already-closed bead is a harmless
// idempotent re-write rather than a correctness bug — this lock is applied
// for consistency with decision-aging and to avoid duplicate
// TopicBeadClosed events on the fleet bus, not because of a data-loss risk.
// Distinct from decisionAgingAdvisoryLockKey — the two sweeps must never
// contend for the same key.
const auditRetentionAdvisoryLockKey int64 = 0x6265_6164_0002

// StartAuditRetention launches a background goroutine that periodically closes
// audit beads older than retentionDays and deletes closed ones past
// retentionDays plus auditRetentionDeleteGrace. The goroutine runs once per
// day and stops when ctx is cancelled.
func (s *BeadsServer) StartAuditRetention(ctx context.Context, retentionDays int) {
	go s.runAuditRetention(ctx, retentionDays, 24*time.Hour)
}

func (s *BeadsServer) runAuditRetention(ctx context.Context, retentionDays int, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Run once immediately on startup.
	s.sweepAuditBeads(ctx, retentionDays)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sweepAuditBeads(ctx, retentionDays)
		}
	}
}

// auditRetentionDeleteGrace is how long a closed audit bead is kept past the
// retention window before the sweep hard-deletes it (gd-GJzCfUmbQC, operator
// decision gd-Z6lkvI37gE: delete_only).
const auditRetentionDeleteGrace = 30 * 24 * time.Hour

// auditRetentionPageSize is the ListBeads page each sweep pass works through
// before it lists again.
const auditRetentionPageSize = 500

// auditRetentionTickBudget bounds how long one daily tick may hold the sweep
// lock. The server writes about 14k audit beads a day, so a single 500-bead
// page per tick (the old behavior) could never catch up; the sweep now pages
// until drained or until this budget runs out, and the next tick resumes.
var auditRetentionTickBudget = 10 * time.Minute

// sweepAuditBeads closes open audit beads older than retentionDays and
// hard-deletes closed ones older than retentionDays plus
// auditRetentionDeleteGrace. Serialized across replicas by
// auditRetentionAdvisoryLockKey, same pattern as sweepStaleDecisions: a
// replica that loses the lock race skips this tick and retries on the next.
func (s *BeadsServer) sweepAuditBeads(ctx context.Context, retentionDays int) {
	acquired, err := s.store.TryAdvisoryLock(ctx, auditRetentionAdvisoryLockKey, func(ctx context.Context) error {
		s.sweepAuditBeadsLocked(ctx, retentionDays, time.Now().Add(auditRetentionTickBudget))
		return nil
	})
	if err != nil {
		slog.Warn("audit retention: advisory lock error", "error", err)
		return
	}
	if !acquired {
		slog.Info("audit retention: skipping tick, another replica holds the sweep lock")
	}
}

// sweepAuditBeadsLocked is the sweep body, run only while
// auditRetentionAdvisoryLockKey is held by this replica. The delete pass runs
// first, so a bead this tick closes is never deleted in the same tick. Both
// passes stop at deadline; whatever is left waits for the next tick.
func (s *BeadsServer) sweepAuditBeadsLocked(ctx context.Context, retentionDays int, deadline time.Time) {
	now := time.Now()
	closeCutoff := now.Add(-time.Duration(retentionDays) * 24 * time.Hour)
	deleteCutoff := closeCutoff.Add(-auditRetentionDeleteGrace)

	deleted := s.deleteExpiredAuditBeads(ctx, deleteCutoff, deadline)
	closed := s.closeExpiredAuditBeads(ctx, closeCutoff, deadline)

	if closed > 0 || deleted > 0 {
		slog.Info("audit retention: sweep complete",
			"closed", closed, "deleted", deleted,
			"close_cutoff", closeCutoff.Format(time.RFC3339),
			"delete_cutoff", deleteCutoff.Format(time.RFC3339))
	}
}

// closeExpiredAuditBeads closes open audit beads created before cutoff, a page
// at a time. A closed bead leaves the open set, so every page is listed from
// the head again; offset only skips the beads that failed to close, so a bead
// that keeps failing cannot spin the loop.
func (s *BeadsServer) closeExpiredAuditBeads(ctx context.Context, cutoff, deadline time.Time) int {
	closed, failed := 0, 0
	for time.Now().Before(deadline) && ctx.Err() == nil {
		beads, _, err := s.store.ListBeads(ctx, model.BeadFilter{
			Type:          []model.BeadType{"audit"},
			Status:        []model.Status{model.StatusOpen},
			CreatedBefore: &cutoff,
			Sort:          "created_at",
			Limit:         auditRetentionPageSize,
			Offset:        failed,
			NoTotal:       true,
		})
		if err != nil {
			slog.Warn("audit retention: failed to list expired beads", "error", err)
			break
		}
		for _, b := range beads {
			if _, err := s.store.CloseBead(ctx, b.ID, "system:audit-retention"); err != nil {
				slog.Warn("audit retention: failed to close bead", "id", b.ID, "error", err)
				failed++
				continue
			}
			closed++
			auditRetentionClosedTotal.Inc()
		}
		if len(beads) < auditRetentionPageSize {
			break
		}
	}
	return closed
}

// deleteExpiredAuditBeads hard-deletes closed audit beads created before
// cutoff, a page at a time, through PurgeBeads (which also drops their events
// and mutation history). Only type=audit with status closed is ever listed, so
// an open or non-audit bead can never be selected.
func (s *BeadsServer) deleteExpiredAuditBeads(ctx context.Context, cutoff, deadline time.Time) int {
	deleted := 0
	for time.Now().Before(deadline) && ctx.Err() == nil {
		beads, _, err := s.store.ListBeads(ctx, model.BeadFilter{
			Type:          []model.BeadType{"audit"},
			Status:        []model.Status{model.StatusClosed},
			CreatedBefore: &cutoff,
			Sort:          "created_at",
			Limit:         auditRetentionPageSize,
			NoTotal:       true,
		})
		if err != nil {
			slog.Warn("audit retention: failed to list closed beads to delete", "error", err)
			break
		}
		if len(beads) == 0 {
			break
		}
		ids := make([]string, 0, len(beads))
		for _, b := range beads {
			ids = append(ids, b.ID)
		}
		purged, err := s.store.PurgeBeads(ctx, store.PurgeRequest{IDs: ids})
		if err != nil {
			slog.Warn("audit retention: failed to delete closed beads", "count", len(ids), "error", err)
			break
		}
		deleted += len(purged)
		auditRetentionDeletedTotal.Add(float64(len(purged)))
		if len(beads) < auditRetentionPageSize || len(purged) == 0 {
			break
		}
	}
	return deleted
}
