package main

import (
	"context"
	"log/slog"

	"gardenio/controller/internal/statusreporter"
	"gardenio/controller/internal/subscriber"
)

// internalStuckPublisher adapts statusreporter.EventPublisher to the
// controller's main event channel. The reporter detects coop seq stagnation
// (ka-0000001882) and synthesises an AgentStuck event so the existing
// handler in handleEvent can delete and recreate the pod.
type internalStuckPublisher struct {
	ch     chan<- subscriber.Event
	logger *slog.Logger
}

// PublishStuck enqueues an AgentStuck subscriber.Event derived from the
// reporter's StuckEvent. Drops the event (with a warning) if the buffered
// channel is full so SyncAll never blocks on a saturated handler.
func (p *internalStuckPublisher) PublishStuck(_ context.Context, ev statusreporter.StuckEvent) {
	out := subscriber.Event{
		Type:      subscriber.AgentStuck,
		Project:   ev.Project,
		Mode:      ev.Mode,
		Role:      ev.Role,
		AgentName: ev.AgentName,
		BeadID:    ev.BeadID,
		Metadata: map[string]string{
			"reason": ev.Reason,
			"source": "stuck-heuristic",
		},
	}
	select {
	case p.ch <- out:
	default:
		p.logger.Warn("stuck event channel full, dropping event",
			"bead", ev.BeadID, "agent", ev.AgentName)
	}
}
