package server

import (
	"context"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// historyRetentionAdvisoryLockKey serializes the history-retention sweep
// across replicas. Distinct from every other sweep's key (see
// TestAdvisoryLockKeysAreUnique). A DELETE of already-deleted rows is a no-op,
// so the lock only avoids two replicas doing the same work.
const historyRetentionAdvisoryLockKey int64 = 0x6265_6164_0006

// historyRetentionBatch bounds each DELETE, and historyRetentionPause spaces
// the batches, so the sweep never holds one long statement or saturates the
// database while it drains a backlog.
const historyRetentionBatch = 500

var historyRetentionPause = 200 * time.Millisecond

// historyRetentionTickBudget bounds how long one tick may hold the sweep lock.
// The first sweep after rollout meets months of trigger and agent history;
// it drains a budget's worth per tick and the next tick resumes.
var historyRetentionTickBudget = 10 * time.Minute

// historyRetentionInterval is the sweep cadence. Hourly keeps each
// steady-state tick to an hour's worth of expired rows.
const historyRetentionInterval = time.Hour

// gardenio_history_retention_deleted_total counts what the history-retention
// sweep deletes, by table (events or mutation_history). Registered on
// /metrics in MetricsHandler (http_metrics.go).
var historyRetentionDeletedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "gardenio_history_retention_deleted_total",
	Help: "Rows the history-retention sweep deleted: beads.bead.updated events and field='fields' mutation_history rows of trigger and agent beads past the retention window.",
}, []string{"table"})

// StartHistoryRetention launches a background goroutine that deletes
// beads.bead.updated events and field='fields' mutation_history rows older
// than retentionDays on historySlimTypes beads (gd-1F8R0SQtiQ). Create,
// close, status, assignee and every other history row is kept for every type.
// Runs once at startup, then hourly, and stops when ctx is cancelled.
func (s *BeadsServer) StartHistoryRetention(ctx context.Context, retentionDays int) {
	go s.runHistoryRetention(ctx, retentionDays, historyRetentionInterval)
}

func (s *BeadsServer) runHistoryRetention(ctx context.Context, retentionDays int, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	s.sweepHistory(ctx, retentionDays)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sweepHistory(ctx, retentionDays)
		}
	}
}

// sweepHistory runs one sweep under the cross-replica advisory lock; a replica
// that loses the race skips this tick.
func (s *BeadsServer) sweepHistory(ctx context.Context, retentionDays int) {
	acquired, err := s.store.TryAdvisoryLock(ctx, historyRetentionAdvisoryLockKey, func(ctx context.Context) error {
		s.sweepHistoryLocked(ctx, retentionDays, time.Now().Add(historyRetentionTickBudget))
		return nil
	})
	if err != nil {
		slog.Warn("history retention: advisory lock error", "error", err)
		return
	}
	if !acquired {
		slog.Info("history retention: skipping tick, another replica holds the sweep lock")
	}
}

func (s *BeadsServer) sweepHistoryLocked(ctx context.Context, retentionDays int, deadline time.Time) {
	cutoff := time.Now().Add(-time.Duration(retentionDays) * 24 * time.Hour)
	types := make([]string, len(historySlimTypes))
	for i, t := range historySlimTypes {
		types[i] = string(t)
	}
	evDeleted := s.drainHistory(ctx, "events", deadline, func(ctx context.Context) (int64, error) {
		return s.store.DeleteBeadUpdatedEventsBefore(ctx, types, cutoff, historyRetentionBatch)
	})
	mutDeleted := s.drainHistory(ctx, "mutation_history", deadline, func(ctx context.Context) (int64, error) {
		return s.store.DeleteFieldsMutationsBefore(ctx, types, cutoff, historyRetentionBatch)
	})
	if evDeleted > 0 || mutDeleted > 0 {
		slog.Info("history retention: sweep complete",
			"events_deleted", evDeleted, "mutations_deleted", mutDeleted,
			"cutoff", cutoff.Format(time.RFC3339))
	}
}

// drainHistory calls del until a batch comes back short, the deadline passes
// or ctx ends, pausing between batches. It returns the rows deleted.
func (s *BeadsServer) drainHistory(ctx context.Context, table string, deadline time.Time, del func(context.Context) (int64, error)) int64 {
	var total int64
	for ctx.Err() == nil && time.Now().Before(deadline) {
		n, err := del(ctx)
		if err != nil {
			slog.Warn("history retention: delete failed", "table", table, "error", err, "deleted_so_far", total)
			return total
		}
		total += n
		historyRetentionDeletedTotal.WithLabelValues(table).Add(float64(n))
		if n < historyRetentionBatch {
			return total
		}
		select {
		case <-ctx.Done():
		case <-time.After(historyRetentionPause):
		}
	}
	return total
}
