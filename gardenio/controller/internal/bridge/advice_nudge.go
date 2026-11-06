// Package bridge provides the advice update nudge watcher.
//
// AdviceNudge watches the kbeads SSE event stream for advice bead update
// events. When an advice bead is updated, it finds all running agents whose
// label subscriptions match the advice's targeting labels and nudges them
// with the updated content so they can incorporate the new guidance.
package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/advice"
	"gardenio/controller/internal/beadsapi"
)

// adviceNudgeTTL is the minimum interval between nudges for the same advice bead.
const adviceNudgeTTL = 10 * time.Minute

// AdviceNudgeConfig holds configuration for the AdviceNudge watcher.
type AdviceNudgeConfig struct {
	Daemon BeadClient
	Logger *slog.Logger
}

// AdviceNudge watches the kbeads SSE event stream for advice bead update
// events and nudges matching agents with the updated content.
type AdviceNudge struct {
	daemon BeadClient
	logger *slog.Logger

	nudgedMu sync.Mutex
	nudged   map[string]time.Time // advice bead ID → last nudge time
}

// NewAdviceNudge creates a new advice update nudge watcher.
func NewAdviceNudge(cfg AdviceNudgeConfig) *AdviceNudge {
	return &AdviceNudge{
		daemon: cfg.Daemon,
		logger: cfg.Logger,
		nudged: make(map[string]time.Time),
	}
}

// RegisterHandlers registers SSE event handlers on the given stream for
// advice bead updated events.
func (a *AdviceNudge) RegisterHandlers(stream *SSEStream) {
	stream.On("beads.bead.updated", a.handleUpdated)
	a.logger.Info("advice nudge watcher registered SSE handlers",
		"topics", []string{"beads.bead.updated"})
}

func (a *AdviceNudge) handleUpdated(ctx context.Context, data []byte) {
	bead := ParseBeadEvent(data)
	if bead == nil {
		return
	}
	if bead.Type != "advice" {
		return
	}

	// Only nudge for open advice beads.
	if bead.Status == "closed" {
		return
	}

	// Rate-limit: skip if we nudged for this advice bead recently.
	if !a.shouldNudge(bead.ID) {
		a.logger.Debug("advice nudge rate-limited", "id", bead.ID)
		return
	}

	// Denied bare-project broadcasts never reach agents via gb prime, so
	// nudging agents to refresh for one would be misleading (and noisy).
	if advice.IsDeniedBroadcastLabels(bead.Labels) {
		a.logger.Warn("advice nudge skipped: bare-project broadcast without "+advice.BroadcastOptInLabel,
			"id", bead.ID, "labels", bead.Labels)
		return
	}

	a.logger.Info("advice bead updated, finding matching agents",
		"id", bead.ID, "title", bead.Title, "labels", bead.Labels)

	a.nudgeMatchingAgents(ctx, *bead)
}

// shouldNudge returns true if a nudge should be sent for the given advice bead ID.
func (a *AdviceNudge) shouldNudge(beadID string) bool {
	a.nudgedMu.Lock()
	defer a.nudgedMu.Unlock()

	now := time.Now()

	// Clean up expired entries.
	for id, t := range a.nudged {
		if now.Sub(t) > adviceNudgeTTL {
			delete(a.nudged, id)
		}
	}

	if last, seen := a.nudged[beadID]; seen && now.Sub(last) <= adviceNudgeTTL {
		return false
	}

	a.nudged[beadID] = now
	return true
}

// nudgeMatchingAgents finds all running agents that match the updated advice
// bead's labels and sends each a nudge with the updated content.
func (a *AdviceNudge) nudgeMatchingAgents(ctx context.Context, bead BeadEvent) {
	agents, err := a.daemon.ListAgentBeads(ctx)
	if err != nil {
		a.logger.Error("failed to list agent beads for advice nudge",
			"advice", bead.ID, "error", err)
		return
	}

	// Filter to running agents only.
	var running []beadsapi.AgentBead
	for _, agent := range agents {
		if agent.AgentState == "working" || agent.AgentState == "spawning" || agent.AgentState == "idle" {
			running = append(running, agent)
		}
	}

	if len(running) == 0 {
		a.logger.Debug("no running agents to nudge for advice update", "advice", bead.ID)
		return
	}

	// Build a truncated preview of the advice content.
	preview := bead.Title
	if len(preview) > 100 {
		preview = preview[:97] + "..."
	}

	nudged := 0
	for _, agent := range running {
		// Build the agent's subscriptions using the same logic as gb prime.
		agentID := agent.Project + "/" + agent.Role + "/" + agent.AgentName
		subs := advice.BuildAgentSubscriptions(agentID, nil)
		// Add label-derived subscriptions from the agent bead.
		if agent.Project != "" {
			subs = appendUnique(subs, "project:"+agent.Project)
		}
		if agent.Role != "" {
			subs = appendUnique(subs, "role:"+agent.Role)
		}

		if !advice.MatchesSubscriptions(bead.Labels, subs) {
			continue
		}

		message := fmt.Sprintf("Advice updated (%s): %s\n\nRun 'gb prime' to refresh your advice context, or read the full update with 'kd show %s'.",
			bead.ID, preview, bead.ID)

		if err := NudgeAgent(ctx, a.daemon, a.logger, agent.AgentName, message); err != nil {
			a.logger.Warn("failed to nudge agent for advice update",
				"agent", agent.AgentName, "advice", bead.ID, "error", err)
			continue
		}
		nudged++
	}

	a.logger.Info("nudged agents for advice update",
		"advice", bead.ID,
		"title", bead.Title,
		"agents_running", len(running),
		"agents_nudged", nudged)
}

// appendUnique adds a string to a slice only if it's not already present.
func appendUnique(slice []string, item string) []string {
	for _, s := range slice {
		if strings.EqualFold(s, item) {
			return slice
		}
	}
	return append(slice, item)
}
