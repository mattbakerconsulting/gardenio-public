package bridge

import (
	"context"
	"log/slog"
	"maps"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// reconcileTopic is the synthetic SSE topic used by BeadStateReconciler when
// it replays poll-discovered beads through BeadStateTriggerSource. trigger:bead
// authors can subscribe to it by name ("reconciled") to opt in to floor-quality
// resilience when the live SSE path is broken — though leaving event_types
// unset (which matches every event) also picks it up automatically.
const reconcileTopic = "beads.bead.reconciled"

// ReconcileSource is the subset of BeadStateTriggerSource the reconciler
// needs. Extracted so tests can substitute a mock without spinning up the
// full bead-state pipeline.
type ReconcileSource interface {
	InjectBeadEvent(topic string, bead *BeadEvent)
}

// ReconcileWatermarkStore persists the high-water mark between bridge
// restarts. *StateManager satisfies it via GetLastReconcileAt /
// SetLastReconcileAt.
type ReconcileWatermarkStore interface {
	GetLastReconcileAt() time.Time
	SetLastReconcileAt(t time.Time) error
}

// ReconcileDaemonClient is the subset of TriggerBeadClient the reconciler
// needs. Tests can substitute a fake without standing up the full HTTP
// client.
type ReconcileDaemonClient interface {
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
}

// BeadStateReconciler is the persistent-event-buffer floor for the
// trigger:bead pipeline (GAP-2, ka-0000004769). It periodically asks the
// daemon for every bead whose updated_at exceeds the last watermark, then
// injects synthetic "reconciled" events into BeadStateTriggerSource so the
// regular condition / action plumbing fires once per missed mutation.
//
// The reconciler is intentionally simple: it owns no event ring buffer of
// its own, only a single monotonic timestamp persisted to the bridge state
// file. That is enough to survive (a) bridge crashes that discarded the
// in-memory pending slice and (b) postgres failovers that nuked the kbeads
// SSE ring buffer and invalidated Last-Event-ID. The SSE happy path stays
// the low-latency primary; this loop is the at-least-once backstop.
type BeadStateReconciler struct {
	source     ReconcileSource
	state      ReconcileWatermarkStore
	daemon     ReconcileDaemonClient
	interval   time.Duration
	pageLimit  int
	overlap    time.Duration
	logger     *slog.Logger

	mu              sync.Mutex
	lastReconcile   time.Time // in-memory mirror of the persisted watermark
	consecutiveFail int
}

// ReconcilerConfig holds construction parameters for BeadStateReconciler.
type ReconcilerConfig struct {
	// Source receives the synthetic events. Required.
	Source ReconcileSource
	// State persists the high-water mark across restarts. Required for
	// crash safety; tests may pass a no-op implementation.
	State ReconcileWatermarkStore
	// Daemon is the beads HTTP client. Required.
	Daemon ReconcileDaemonClient
	// Interval between reconcile ticks. Defaults to 60s when zero.
	Interval time.Duration
	// PageLimit caps the number of beads fetched per tick. Defaults to
	// 500. Larger ticks pay more daemon CPU; smaller ticks risk dropping
	// the tail of a burst.
	PageLimit int
	// Overlap rewinds the cursor by this duration when advancing the
	// watermark, so beads written within the same second as the previous
	// tick are not skipped due to second-precision timestamps. Defaults
	// to 2s. Re-fires within the overlap window are absorbed by the
	// trigger engine's dedup (CatchUpKeys + per-trigger seen map).
	Overlap time.Duration
	// Logger for diagnostics.
	Logger *slog.Logger
}

// NewBeadStateReconciler constructs a reconciler. Source, State, and Daemon
// must all be non-nil; missing dependencies trip a panic at construction
// rather than silently no-op'ing in production.
func NewBeadStateReconciler(cfg ReconcilerConfig) *BeadStateReconciler {
	if cfg.Source == nil || cfg.State == nil || cfg.Daemon == nil {
		// PANIC: misconfiguration at startup is a developer bug, not a
		// runtime condition — fail loud so deploys catch it.
		panic("BeadStateReconciler requires Source, State, and Daemon")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = 60 * time.Second
	}
	pageLimit := cfg.PageLimit
	if pageLimit <= 0 {
		pageLimit = 500
	}
	overlap := cfg.Overlap
	if overlap <= 0 {
		overlap = 2 * time.Second
	}
	return &BeadStateReconciler{
		source:    cfg.Source,
		state:     cfg.State,
		daemon:    cfg.Daemon,
		interval:  interval,
		pageLimit: pageLimit,
		overlap:   overlap,
		logger:    logger,
	}
}

// Start runs the reconcile loop until ctx is cancelled. The first tick fires
// immediately so a bridge that just came up from a crash catches up before
// waiting a full interval. Bootstraps the watermark from time.Now() on the
// very first run so we do not stampede the daemon with the entire bead
// history (the SSE path handles steady-state delivery from the moment the
// stream connects).
func (r *BeadStateReconciler) Start(ctx context.Context) {
	if existing := r.state.GetLastReconcileAt(); !existing.IsZero() {
		r.lastReconcile = existing
		r.logger.Info("trigger:bead reconciler resuming from persisted watermark",
			"watermark", existing.UTC().Format(time.RFC3339))
	} else {
		now := time.Now().UTC()
		r.lastReconcile = now
		if err := r.state.SetLastReconcileAt(now); err != nil {
			r.logger.Warn("trigger:bead reconciler failed to seed watermark",
				"error", err)
		} else {
			r.logger.Info("trigger:bead reconciler bootstrapped watermark", "watermark", now.Format(time.RFC3339))
		}
	}

	r.runOnce(ctx)

	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			r.logger.Info("trigger:bead reconciler stopping")
			return
		case <-ticker.C:
			r.runOnce(ctx)
		}
	}
}

// runOnce performs a single reconcile cycle. Exported (lowercase but callable
// from tests in the same package) so tests can drive the loop deterministically
// without sleeping a real interval.
func (r *BeadStateReconciler) runOnce(ctx context.Context) {
	r.mu.Lock()
	cursor := r.lastReconcile
	r.mu.Unlock()

	queryFrom := cursor.Add(-r.overlap)
	tickStart := time.Now().UTC()

	res, err := r.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		UpdatedAfter: queryFrom,
		Sort:         "updated_at",
		Limit:        r.pageLimit,
	})
	if err != nil {
		r.mu.Lock()
		r.consecutiveFail++
		streak := r.consecutiveFail
		r.mu.Unlock()
		r.logger.Warn("trigger:bead reconciler poll failed",
			"error", err, "from", queryFrom.Format(time.RFC3339),
			"consecutive_failures", streak)
		return
	}

	r.mu.Lock()
	r.consecutiveFail = 0
	r.mu.Unlock()

	injected := 0
	maxUpdated := cursor
	for _, b := range res.Beads {
		if b == nil {
			continue
		}
		// Defence-in-depth: the daemon already filters by updated_after,
		// but the overlap rewind plus clock skew can re-surface a bead
		// that was already processed last tick. Skip anything at-or-
		// before the previous watermark cursor (strict > matches the
		// daemon's updated_after semantics).
		if !b.UpdatedAt.After(cursor) {
			continue
		}
		evt := beadDetailToEvent(b)
		if evt == nil {
			continue
		}
		r.source.InjectBeadEvent(reconcileTopic, evt)
		injected++
		if b.UpdatedAt.After(maxUpdated) {
			maxUpdated = b.UpdatedAt
		}
	}

	// Advance the watermark even when no beads matched, so a quiet system
	// keeps queryFrom bounded to the overlap window rather than walking
	// the full table every tick. Use the tick-start time as the upper
	// bound so we never advance past events the daemon may still be
	// flushing within the same wall-clock instant.
	advance := tickStart
	if maxUpdated.After(advance) {
		advance = maxUpdated
	}
	if advance.After(cursor) {
		r.mu.Lock()
		r.lastReconcile = advance
		r.mu.Unlock()
		if err := r.state.SetLastReconcileAt(advance); err != nil {
			r.logger.Warn("trigger:bead reconciler failed to persist watermark",
				"error", err, "watermark", advance.Format(time.RFC3339))
		}
	}

	if injected > 0 || len(res.Beads) > 0 {
		r.logger.Info("trigger:bead reconciler tick",
			"fetched", len(res.Beads), "injected", injected,
			"from", queryFrom.Format(time.RFC3339),
			"advance", advance.Format(time.RFC3339))
	} else {
		r.logger.Debug("trigger:bead reconciler tick (no changes)",
			"from", queryFrom.Format(time.RFC3339))
	}
}

// beadDetailToEvent projects a beadsapi.BeadDetail (the poll response shape)
// into the bridge's internal BeadEvent (the SSE event shape) so reconciler
// output flows through the same beadEventToMatch / matchConditions path as
// live SSE. Returns nil for malformed inputs so callers can `if evt == nil
// { continue }`.
// BeadDetailToEvent is the exported projection of a daemon BeadDetail into a
// BeadEvent, used by `gb trigger test` to build the typed CEL context from a
// real bead fetched over the API. Thin wrapper over beadDetailToEvent.
func BeadDetailToEvent(b *beadsapi.BeadDetail) *BeadEvent {
	return beadDetailToEvent(b)
}

func beadDetailToEvent(b *beadsapi.BeadDetail) *BeadEvent {
	if b == nil || b.ID == "" {
		return nil
	}
	return &BeadEvent{
		ID:        b.ID,
		Type:      b.Type,
		Title:     b.Title,
		Status:    b.Status,
		Assignee:  b.Assignee,
		CreatedBy: b.CreatedBy,
		Labels:    append([]string(nil), b.Labels...),
		Fields:    copyStringMap(b.Fields),
		Priority:  b.Priority,
		ClosedBy:  b.ClosedBy,
	}
}

// copyStringMap returns a shallow clone so reconciler-side mutations on the
// projected Fields map cannot leak back into the beadsapi cache.
func copyStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	maps.Copy(out, in)
	return out
}
