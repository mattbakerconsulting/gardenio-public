// Package bridge provides the jack lifecycle watcher.
//
// Jacks subscribes to kbeads SSE event stream for bead create/close/update events,
// filters for type=jack beads, and notifies Slack with:
//   - Jack raised: wrench emoji + target + TTL + reason
//   - Jack lowered: checkmark + target + reason
//   - Jack expired: warning + time past TTL + suggested action
//   - Batch summary: >10 jacks/minute collapsed into single message
package bridge

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// JackNotifier sends jack lifecycle notifications to an external system.
type JackNotifier interface {
	// NotifyJackOn is called when a new jack bead is created.
	NotifyJackOn(ctx context.Context, bead BeadEvent) error
	// NotifyJackOnBatch is called when >10 jacks arrive within 1 minute.
	NotifyJackOnBatch(ctx context.Context, beads []BeadEvent) error
	// NotifyJackOff is called when a jack bead is closed (lowered).
	NotifyJackOff(ctx context.Context, bead BeadEvent) error
	// NotifyJackExpired is called when a jack bead expires past TTL.
	NotifyJackExpired(ctx context.Context, bead BeadEvent) error
}

// jackExpiredRenotifyTTL is the re-notify horizon for a jack expiry: an expired
// jack is announced at most once per this window (a lingering-past-TTL jack
// keeps emitting `updated` events, and we do not want to spam Slack on each).
const jackExpiredRenotifyTTL = 6 * time.Hour

// Jacks watches the kbeads SSE event stream for jack bead lifecycle events.
type Jacks struct {
	notifier JackNotifier
	logger   *slog.Logger

	// offSeen / expiredSeen dedup jack-lowered and jack-expired notifications
	// per jack bead. They are TTL/size-bounded (ka-0000008059): the bridge is a
	// process-lifetime handler, so a plain map[string]... of every jack ID ever
	// observed would grow monotonically into a slow heap leak. Each carries its
	// own lock. Run() sweeps them on a ticker.
	offSeen     *seenSet // jack ID → lowered-notified (24h dedup)
	expiredSeen *seenSet // jack ID → expired-notified (6h re-notify window)

	batchMu    sync.Mutex
	batch      []BeadEvent
	batchTimer *time.Timer
	batchCtx   context.Context
}

// JacksConfig holds configuration for the Jacks watcher.
type JacksConfig struct {
	Notifier JackNotifier
	Logger   *slog.Logger
}

// NewJacks creates a new jack lifecycle watcher.
func NewJacks(cfg JacksConfig) *Jacks {
	return &Jacks{
		notifier:    cfg.Notifier,
		logger:      cfg.Logger,
		offSeen:     newSeenSet(seenTTL, seenMaxEntries),
		expiredSeen: newSeenSet(jackExpiredRenotifyTTL, seenMaxEntries),
	}
}

// Run starts the background dedup-map GC loop. It blocks until ctx is cancelled;
// callers run it in a goroutine. Without it the seen sets would only self-evict
// lazily on re-observation, so idle keys would linger for the process lifetime.
// Mirrors Agents.Run / BeadActivity.Run.
func (j *Jacks) Run(ctx context.Context) {
	ticker := time.NewTicker(seenGCInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			j.offSeen.gc()
			j.expiredSeen.gc()
		}
	}
}

// RegisterHandlers registers SSE event handlers on the given stream for
// jack bead created, closed, and updated events.
func (j *Jacks) RegisterHandlers(stream *SSEStream) {
	stream.On("beads.bead.created", j.handleCreated)
	stream.On("beads.bead.closed", j.handleClosed)
	stream.On("beads.bead.updated", j.handleUpdated)
	j.logger.Info("jacks watcher registered SSE handlers",
		"topics", []string{"beads.bead.created", "beads.bead.closed", "beads.bead.updated"})
}

// batchWindow is the duration to accumulate jack-on events before flushing.
const batchWindow = 1 * time.Minute

// batchThreshold is the number of individual notifications before batching.
const batchThreshold = 10

// batchNotifyTimeout bounds the batch-summary notification once its timer fires
// on a context detached from the (already-cancelled) SSE handler ctx.
const batchNotifyTimeout = 60 * time.Second

func (j *Jacks) handleCreated(ctx context.Context, data []byte) {
	bead := ParseBeadEvent(data)
	if bead == nil || bead.Type != "jack" {
		return
	}

	j.logger.Info("jack raised",
		"id", bead.ID,
		"target", bead.Fields["target"],
		"ttl", bead.Fields["ttl"],
		"agent", bead.Assignee)

	if j.notifier == nil {
		return
	}

	j.batchMu.Lock()
	j.batch = append(j.batch, *bead)
	batchLen := len(j.batch)

	// Send individual notifications for first N.
	if batchLen <= batchThreshold {
		j.batchMu.Unlock()
		if err := j.notifier.NotifyJackOn(ctx, *bead); err != nil {
			j.logger.Error("failed to notify jack on", "id", bead.ID, "error", err)
		}
		return
	}

	// Start batch timer on first overflow.
	//
	// flushBatch fires batchWindow AFTER handleCreated returns, but the SSE
	// dispatcher (sse.go) cancels the per-handler ctx via `defer cancel()` the
	// instant handleCreated returns — and even absent that early return, the
	// SSE handler timeout equals batchWindow, so the ctx is dead by the time
	// flushBatch runs regardless. Capturing that ctx directly would hand
	// NotifyJackOnBatch an already-cancelled context: its first hop
	// (anyVibeAmongJacks -> vibeModeForAgent -> daemon.FindAgentBead, a network
	// call) fails closed on a dead context and silently suppresses the overflow
	// summary on every fire, and the downstream Slack post would run on a
	// cancelled context too. Detach cancellation with WithoutCancel (preserving
	// request-scoped values such as the operator stamp), mirroring the
	// beadactivity / bare_id_preview detached-timer pattern.
	if j.batchTimer == nil {
		j.batchCtx = context.WithoutCancel(ctx)
		j.batchTimer = time.AfterFunc(batchWindow, j.flushBatch)
	}
	j.batchMu.Unlock()
}

// flushBatch sends a batch summary for overflow jacks.
func (j *Jacks) flushBatch() {
	j.batchMu.Lock()
	batch := j.batch
	j.batch = nil
	j.batchTimer = nil
	ctx := j.batchCtx
	j.batchMu.Unlock()

	if len(batch) <= batchThreshold {
		return // All were sent individually
	}

	// The first batchThreshold were already notified individually.
	overflow := batch[batchThreshold:]
	if j.notifier != nil {
		if ctx == nil {
			ctx = context.Background()
		}
		notifyCtx, cancel := context.WithTimeout(ctx, batchNotifyTimeout)
		defer cancel()
		if err := j.notifier.NotifyJackOnBatch(notifyCtx, overflow); err != nil {
			j.logger.Error("failed to notify jack batch", "count", len(overflow), "error", err)
		}
	}
}

func (j *Jacks) handleClosed(ctx context.Context, data []byte) {
	bead := ParseBeadEvent(data)
	if bead == nil || bead.Type != "jack" {
		return
	}

	j.logger.Info("jack lowered",
		"id", bead.ID,
		"target", bead.Fields["target"],
		"agent", bead.Assignee)

	// Dedup: only notify once per jack close.
	if j.offSeen.markSeen(bead.ID) {
		return
	}

	if j.notifier != nil {
		if err := j.notifier.NotifyJackOff(ctx, *bead); err != nil {
			j.logger.Error("failed to notify jack off", "id", bead.ID, "error", err)
		}
	}
}

func (j *Jacks) handleUpdated(ctx context.Context, data []byte) {
	bead := ParseBeadEvent(data)
	if bead == nil || bead.Type != "jack" {
		return
	}

	// Detect expiry: jack updated with "expired" label or past TTL.
	isExpired := false
	for _, label := range bead.Labels {
		if label == "expired" {
			isExpired = true
			break
		}
	}
	if !isExpired {
		return
	}

	j.logger.Info("jack expired",
		"id", bead.ID,
		"target", bead.Fields["target"],
		"agent", bead.Assignee)

	// Dedup: only notify once per 6 hours per jack. markSeen returns true when a
	// still-live entry (within the 6h TTL) exists, and refreshes the timestamp
	// on an expired one — the same re-notify-after-6h semantics as before, now
	// TTL/size-bounded so the map cannot leak for the process lifetime.
	if j.expiredSeen.markSeen(bead.ID) {
		return
	}

	if j.notifier != nil {
		if err := j.notifier.NotifyJackExpired(ctx, *bead); err != nil {
			j.logger.Error("failed to notify jack expired", "id", bead.ID, "error", err)
		}
	}
}
