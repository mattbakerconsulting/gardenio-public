// Package bridge provides the bead activity watcher.
//
// BeadActivity subscribes to kbeads SSE event stream for bead lifecycle events
// and posts notifications in agent Slack threads when agents create, claim, or
// close beads. This gives operators real-time visibility into agent activity
// without checking kd list.
//
// Only work-item bead types (task, bug, feature, epic) are tracked. Infrastructure
// beads (agent, decision, mail, project, etc.) are excluded to avoid noise.
package bridge

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// defaultCreateClaimWindow is how long to buffer a "created" event before
// posting, to allow a subsequent "claimed" event to merge into a single
// message. Overridable per-instance via BeadActivityConfig.CreateClaimWindow
// so tests do not have to spend the production window in wall-clock sleeps:
// five tests waited window+500ms apiece, which was 17.5s of this package's
// 78s runtime and the single largest cost in it. Injected rather than made a
// mutable package var so that tests remain safe to parallelise.
const defaultCreateClaimWindow = 3 * time.Second

// beadCreatedNotifyTimeout bounds the deferred "Created" notification once its
// buffer window elapses. The AfterFunc runs on a context detached from the SSE
// handler ctx (which is cancelled the moment handleCreated returns), so it
// carries its own deadline for the vibe-mode lookup + Slack post.
const beadCreatedNotifyTimeout = 60 * time.Second

// workItemTypes is the set of bead types that represent real work items.
// Infrastructure types (agent, decision, mail, project, etc.) are excluded.
var workItemTypes = map[string]bool{
	"task":    true,
	"bug":     true,
	"feature": true,
	"epic":    true,
}

// BeadActivityNotifier posts bead lifecycle notifications to agent Slack threads.
type BeadActivityNotifier interface {
	// NotifyBeadCreated is called when an agent creates a work-item bead.
	NotifyBeadCreated(ctx context.Context, bead BeadEvent)
	// NotifyBeadClaimed is called when an agent claims a bead (status → in_progress).
	NotifyBeadClaimed(ctx context.Context, bead BeadEvent)
	// NotifyBeadCreatedAndClaimed is called when an agent creates and then
	// immediately claims a bead, aggregating both into a single notification.
	NotifyBeadCreatedAndClaimed(ctx context.Context, bead BeadEvent)
	// NotifyBeadClosed is called when a bead assigned to an agent is closed.
	NotifyBeadClosed(ctx context.Context, bead BeadEvent)
}

// BeadActivityConfig holds configuration for the BeadActivity watcher.
type BeadActivityConfig struct {
	Notifier BeadActivityNotifier // nil = no notifications
	Logger   *slog.Logger

	// CreateClaimWindow overrides how long a "created" event is buffered
	// awaiting a merge with its "claimed" event. Zero means
	// defaultCreateClaimWindow. Tests set it short; production leaves it unset.
	CreateClaimWindow time.Duration
}

// pendingCreate tracks a buffered "created" event awaiting possible merge with claim.
type pendingCreate struct {
	bead  BeadEvent
	timer *time.Timer
}

// BeadActivity watches the kbeads SSE event stream for bead lifecycle events
// and dispatches notifications to agent Slack threads.
type BeadActivity struct {
	notifier BeadActivityNotifier
	logger   *slog.Logger

	// seen dedups create/claim/close notifications per (bead, action) across
	// SSE reconnects. TTL/size-bounded so it cannot leak for the process
	// lifetime (ka-0000008059); carries its own lock.
	seen *seenSet

	// createClaimWindow is the buffer window for this instance; see
	// defaultCreateClaimWindow. Set once at construction, never mutated.
	createClaimWindow time.Duration

	mu      sync.Mutex
	pending map[string]*pendingCreate // bead ID → buffered create event
}

// NewBeadActivity creates a new bead activity watcher.
func NewBeadActivity(cfg BeadActivityConfig) *BeadActivity {
	window := cfg.CreateClaimWindow
	if window <= 0 {
		window = defaultCreateClaimWindow
	}
	return &BeadActivity{
		notifier:          cfg.Notifier,
		logger:            cfg.Logger,
		seen:              newSeenSet(seenTTL, seenMaxEntries),
		pending:           make(map[string]*pendingCreate),
		createClaimWindow: window,
	}
}

// Run starts the background dedup-map GC loop. It blocks until ctx is
// cancelled; callers run it in a goroutine. Mirrors Agents.Run.
func (ba *BeadActivity) Run(ctx context.Context) {
	ticker := time.NewTicker(seenGCInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ba.seen.gc()
		}
	}
}

// RegisterHandlers registers SSE event handlers on the given stream.
func (ba *BeadActivity) RegisterHandlers(stream *SSEStream) {
	stream.On("beads.bead.created", ba.handleCreated)
	stream.On("beads.bead.closed", ba.handleClosed)
	stream.On("beads.bead.updated", ba.handleUpdated)
	ba.logger.Info("bead activity watcher registered SSE handlers",
		"topics", []string{"beads.bead.created", "beads.bead.closed", "beads.bead.updated"})
}

func (ba *BeadActivity) handleCreated(ctx context.Context, data []byte) {
	bead := ParseBeadEvent(data)
	if bead == nil || !workItemTypes[bead.Type] {
		return
	}
	if bead.CreatedBy == "" {
		return
	}

	if ba.alreadySeen(bead.ID, "created") {
		return
	}

	ba.logger.Debug("bead created by agent",
		"id", bead.ID, "type", bead.Type, "title", bead.Title, "created_by", bead.CreatedBy)

	if ba.notifier == nil {
		return
	}

	// Buffer the created event briefly — if the agent claims the same bead
	// within createClaimWindow, we merge into a single notification.
	//
	// The AfterFunc fires ~createClaimWindow AFTER handleCreated returns, but
	// the SSE dispatcher (sse.go) cancels the per-handler ctx via `defer
	// cancel()` the instant handleCreated returns. Capturing that ctx directly
	// would hand NotifyBeadCreated an already-cancelled context: its first hop
	// (vibeModeForAgent -> daemon.FindAgentBead, a network call) fails closed
	// on a dead context and silently suppresses the "Created" notification for
	// every un-claimed create, and the downstream Slack post would run on a
	// cancelled context too. Detach cancellation with WithoutCancel (preserving
	// request-scoped values such as the operator stamp) under a fresh bounded
	// timeout, mirroring the bare_id_preview detached-goroutine pattern.
	beadCopy := *bead
	detachedCtx := context.WithoutCancel(ctx)
	timer := time.AfterFunc(ba.createClaimWindow, func() {
		ba.mu.Lock()
		_, stillPending := ba.pending[beadCopy.ID]
		if stillPending {
			delete(ba.pending, beadCopy.ID)
		}
		ba.mu.Unlock()
		if stillPending {
			notifyCtx, cancel := context.WithTimeout(detachedCtx, beadCreatedNotifyTimeout)
			defer cancel()
			ba.notifier.NotifyBeadCreated(notifyCtx, beadCopy)
		}
	})

	ba.mu.Lock()
	ba.pending[beadCopy.ID] = &pendingCreate{bead: beadCopy, timer: timer}
	ba.mu.Unlock()
}

func (ba *BeadActivity) handleClosed(ctx context.Context, data []byte) {
	bead := ParseBeadEvent(data)
	if bead == nil || !workItemTypes[bead.Type] {
		return
	}
	if bead.Assignee == "" {
		return
	}

	if ba.alreadySeen(bead.ID, "closed") {
		return
	}

	ba.logger.Debug("bead closed for agent",
		"id", bead.ID, "type", bead.Type, "title", bead.Title, "assignee", bead.Assignee)

	if ba.notifier != nil {
		ba.notifier.NotifyBeadClosed(ctx, *bead)
	}
}

func (ba *BeadActivity) handleUpdated(ctx context.Context, data []byte) {
	bead := ParseBeadEvent(data)
	if bead == nil || !workItemTypes[bead.Type] {
		return
	}

	// Detect claim: bead transitions to in_progress with an assignee.
	if bead.Status == "in_progress" && bead.Assignee != "" {
		if ba.alreadySeen(bead.ID, "claimed") {
			return
		}

		ba.logger.Debug("bead claimed by agent",
			"id", bead.ID, "type", bead.Type, "title", bead.Title, "assignee", bead.Assignee)

		if ba.notifier == nil {
			return
		}

		// Check if there's a pending "created" event for this bead —
		// if so, merge into a single "created & claimed" notification.
		ba.mu.Lock()
		pc, hasPending := ba.pending[bead.ID]
		if hasPending {
			pc.timer.Stop()
			delete(ba.pending, bead.ID)
		}
		ba.mu.Unlock()

		if hasPending {
			ba.logger.Debug("merging create+claim into single notification",
				"id", bead.ID, "type", bead.Type, "title", bead.Title)
			ba.notifier.NotifyBeadCreatedAndClaimed(ctx, *bead)
		} else {
			ba.notifier.NotifyBeadClaimed(ctx, *bead)
		}
	}
}

// alreadySeen returns true if this (beadID, action) pair has already been
// notified, and records it if not. Prevents duplicates on SSE reconnect.
func (ba *BeadActivity) alreadySeen(beadID, action string) bool {
	return ba.seen.markSeen(beadID + ":" + action)
}
