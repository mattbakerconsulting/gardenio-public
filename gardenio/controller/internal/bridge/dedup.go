// Package bridge provides SSE event deduplication for the slack-bridge.
//
// Dedup tracks seen events by prefixed keys to prevent duplicate Slack
// notifications. It is used by all watchers (decisions, agents, jacks) and
// handles both in-session dedup (same event replayed by SSE reconnect) and
// cross-session dedup (state persisted via StateManager).
package bridge

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// dedupTTL is the maximum age of entries in the dedup map before cleanup.
const dedupTTL = 2 * time.Hour

// dedupCleanupInterval is how often the dedup map is swept for expired entries.
const dedupCleanupInterval = 10 * time.Minute

// catchUpMaxAge is the maximum age of decisions to notify during catch-up.
// Decisions older than this are silently marked as seen to prevent flooding
// Slack on bridge restart or when pointed at a cloned DB.
const catchUpMaxAge = 1 * time.Hour

// Dedup provides event deduplication for the slack-bridge watchers.
type Dedup struct {
	mu   sync.Mutex
	seen map[string]time.Time // prefixed key → first-seen time

	logger *slog.Logger
}

// NewDedup creates a new event deduplicator.
func NewDedup(logger *slog.Logger) *Dedup {
	return &Dedup{
		seen:   make(map[string]time.Time),
		logger: logger,
	}
}

// StartCleanup runs a periodic goroutine that removes expired entries from the
// dedup map. It blocks until ctx is cancelled.
func (d *Dedup) StartCleanup(ctx context.Context) {
	ticker := time.NewTicker(dedupCleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.cleanup()
		}
	}
}

// cleanup removes entries older than dedupTTL.
func (d *Dedup) cleanup() {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now()
	expired := 0
	for key, t := range d.seen {
		if now.Sub(t) > dedupTTL {
			delete(d.seen, key)
			expired++
		}
	}
	if expired > 0 {
		d.logger.Debug("dedup cleanup", "expired", expired, "remaining", len(d.seen))
	}
}

// Len returns the number of entries in the dedup map (for testing).
func (d *Dedup) Len() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.seen)
}

// Seen returns true if the key has already been processed. If not, marks it as seen.
// Keys should be prefixed by event type, e.g., "created:dec-1", "resolved:dec-1".
func (d *Dedup) Seen(key string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.seen[key]; ok {
		return true
	}
	d.seen[key] = time.Now()
	return false
}

// Mark records a key as seen without checking.
func (d *Dedup) Mark(key string) {
	d.mu.Lock()
	d.seen[key] = time.Now()
	d.mu.Unlock()
}

// CatchUpDecisions fetches pending decisions from the daemon and pre-populates
// the dedup map. Decisions older than 1 hour are skipped to prevent flood on
// cloned DBs. New decisions are notified with rate limiting.
// Decisions assigned to closed/done/failed agents are skipped to prevent
// stale decisions from re-appearing on agent cards after a bridge restart.
func (d *Dedup) CatchUpDecisions(ctx context.Context, daemon BeadClient, notifier Notifier, logger *slog.Logger) {
	if daemon == nil {
		return
	}

	decisions, err := daemon.ListDecisionBeads(ctx)
	if err != nil {
		logger.Warn("catch-up: failed to list pending decisions", "error", err)
		return
	}

	// Build a set of active (non-terminal) agent names so we can skip
	// decisions assigned to agents that have already finished.
	activeAgents := make(map[string]bool)
	if agents, err := daemon.ListAgentBeads(ctx); err == nil {
		for _, a := range agents {
			if a.AgentState == "done" || a.AgentState == "failed" {
				continue
			}
			activeAgents[extractAgentName(a.AgentName)] = true
		}
	} else {
		logger.Warn("catch-up: failed to list agents, skipping agent filter", "error", err)
	}

	now := time.Now()
	notified := 0
	skippedSeen := 0
	skippedClosedAgent := 0
	skippedOld := 0
	skippedPosted := 0

	for _, dec := range decisions {
		key := "created:" + dec.ID
		if d.Seen(key) {
			skippedSeen++
			continue
		}

		// Skip decisions that already have a chosen value (resolved).
		if dec.Fields["chosen"] != "" {
			d.Mark("resolved:" + dec.ID)
			continue
		}

		// Skip decisions older than catchUpMaxAge to prevent flooding Slack
		// on bridge restart or when pointed at a cloned/migrated DB.
		if !dec.CreatedAt.IsZero() && now.Sub(dec.CreatedAt) > catchUpMaxAge {
			skippedOld++
			d.Mark(key)
			continue
		}

		// Skip decisions already posted to Slack (have a slack_ts field).
		if dec.Fields["slack_ts"] != "" {
			skippedPosted++
			d.Mark(key)
			continue
		}

		// Skip decisions assigned to agents that are closed/done/failed.
		agent := extractAgentName(dec.Assignee)
		if agent != "" && len(activeAgents) > 0 && !activeAgents[agent] {
			skippedClosedAgent++
			d.Mark(key) // Mark seen so SSE replay doesn't re-notify either.
			continue
		}

		// Notify if we have a notifier.
		if notifier != nil {
			if err := notifier.NotifyDecision(ctx, beadEventFromDetail(dec)); err != nil {
				logger.Error("catch-up: failed to notify decision", "id", dec.ID, "error", err)
			} else {
				notified++
			}
			// Rate limit: ~1 notification per second.
			time.Sleep(1100 * time.Millisecond)
		}
	}

	logger.Info("catch-up complete",
		"total", len(decisions),
		"notified", notified,
		"skipped_seen", skippedSeen,
		"skipped_old", skippedOld,
		"skipped_posted", skippedPosted,
		"skipped_closed_agent", skippedClosedAgent)
}

// CatchUpAgents fetches active agent beads from the daemon and pre-populates
// the dedup map so that SSE replay of stale created events is suppressed.
// This prevents agent card flicker (state reset to "spawning") on restart.
func (d *Dedup) CatchUpAgents(ctx context.Context, daemon BeadClient, logger *slog.Logger) {
	if daemon == nil {
		return
	}

	agents, err := daemon.ListAgentBeads(ctx)
	if err != nil {
		logger.Warn("catch-up agents: failed to list active agents", "error", err)
		return
	}

	for _, a := range agents {
		d.Mark("beads.bead.created:" + a.ID)
	}

	logger.Info("catch-up agents complete", "marked", len(agents))
}

// beadEventFromDetail converts a BeadDetail to a BeadEvent for notification.
func beadEventFromDetail(d *beadsapi.BeadDetail) BeadEvent {
	return BeadEvent{
		ID:        d.ID,
		Type:      d.Type,
		Title:     d.Title,
		Status:    d.Status,
		Assignee:  d.Assignee,
		CreatedBy: d.CreatedBy,
		Labels:    d.Labels,
		Fields:    d.Fields,
		Priority:  d.Priority,
	}
}
