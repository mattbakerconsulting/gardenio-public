package server

import (
	"context"
	"log/slog"
	"time"
)

// slowToolCallsRetentionAdvisoryLockKey serializes the slow_tool_calls
// retention sweep across replicas. Postgres advisory locks share one 64-bit
// keyspace per database, so it must differ from auditRetentionAdvisoryLockKey
// and decisionAgingAdvisoryLockKey. A DELETE of already-deleted rows is a
// harmless no-op, so the lock only avoids two replicas doing the same work.
const slowToolCallsRetentionAdvisoryLockKey int64 = 0x6265_6164_0003

// slowToolCallsRetentionBatch bounds each DELETE; the sweep loops until a
// batch comes back short so a large backlog drains without one huge statement.
const slowToolCallsRetentionBatch = 1000

// StartSlowToolCallsRetention launches a background goroutine that
// periodically deletes slow_tool_calls rows whose ts is older than
// retentionDays (gd-4psiVnwXdj). Runs once at startup then daily, and stops
// when ctx is cancelled.
func (s *BeadsServer) StartSlowToolCallsRetention(ctx context.Context, retentionDays int) {
	go s.runSlowToolCallsRetention(ctx, retentionDays, 24*time.Hour)
}

func (s *BeadsServer) runSlowToolCallsRetention(ctx context.Context, retentionDays int, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	s.sweepSlowToolCalls(ctx, retentionDays)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sweepSlowToolCalls(ctx, retentionDays)
		}
	}
}

// sweepSlowToolCalls deletes expired ledger rows under the cross-replica
// advisory lock; a replica that loses the race skips this tick.
func (s *BeadsServer) sweepSlowToolCalls(ctx context.Context, retentionDays int) {
	acquired, err := s.store.TryAdvisoryLock(ctx, slowToolCallsRetentionAdvisoryLockKey, func(ctx context.Context) error {
		s.sweepSlowToolCallsLocked(ctx, retentionDays)
		return nil
	})
	if err != nil {
		slog.Warn("slow tool calls retention: advisory lock error", "error", err)
		return
	}
	if !acquired {
		slog.Info("slow tool calls retention: skipping tick, another replica holds the sweep lock")
	}
}

func (s *BeadsServer) sweepSlowToolCallsLocked(ctx context.Context, retentionDays int) {
	cutoff := time.Now().Add(-time.Duration(retentionDays) * 24 * time.Hour)
	var total int64
	for ctx.Err() == nil {
		n, err := s.store.DeleteSlowToolCallsBefore(ctx, cutoff, slowToolCallsRetentionBatch)
		if err != nil {
			slog.Warn("slow tool calls retention: delete failed", "error", err, "deleted_so_far", total)
			return
		}
		total += n
		if n < slowToolCallsRetentionBatch {
			break
		}
	}
	if total > 0 {
		slog.Info("slow tool calls retention: sweep complete", "deleted", total, "cutoff", cutoff.Format(time.RFC3339))
	}
}
