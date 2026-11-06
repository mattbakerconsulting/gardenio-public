// Package bridge provides the claimed bead update watcher.
//
// Claimed watches the kbeads SSE event stream for bead update and closed
// events and nudges the assignee when a bead they have claimed changes. This
// helps agents notice changes to their in-progress work without polling.
//
// On update: nudge with a review prompt so the agent can react to the change.
// On close: nudge with stop-first guidance consistent with the Stop Gate
// Contract — if all work is done the agent should wrap up and call `gb stop`,
// and only create a `gb decision` when genuinely blocked on a human choice.
// (Beads close on every completed step, so a close is not by itself a signal
// that a decision checkpoint is warranted.) A decision that is ALREADY filed
// and merely awaiting the operator is also a `gb stop` case — the decision
// persists as the durable re-entry handle and auto-respawns the agent on
// resolution (respawnAgentForDecision), so re-`gb yield`ing across an
// indefinite operator wait only burns a live pod (ka-0000003244).
//
// Update events carry the mutation actor; updates originated by the assignee
// themselves (e.g. their own `kd claim`) are not nudged — there is no news
// for the actor. When the actor is empty (auth disabled / anonymous caller)
// the update is treated as external and nudged as before.
// Rate limiting (5 minutes per bead) prevents notification spam.
package bridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// claimedNudgeTTL is the minimum interval between nudges for the same bead.
const claimedNudgeTTL = 5 * time.Minute

// skipClaimedTypes are bead types that should not trigger claimed-update nudges.
// These are system/infrastructure beads rather than actionable work.
var skipClaimedTypes = map[string]bool{
	"agent":    true,
	"decision": true,
	"mail":     true,
	"project":  true,
	"report":   true,
}

// ClaimedConfig holds configuration for the Claimed watcher.
type ClaimedConfig struct {
	Daemon BeadClient
	Logger *slog.Logger
}

// Claimed watches the kbeads SSE event stream for bead update events and
// nudges the claiming agent when a bead they own is updated.
type Claimed struct {
	daemon BeadClient
	logger *slog.Logger

	nudgedMu sync.Mutex
	nudged   map[string]time.Time // bead ID → last nudge time
}

// NewClaimed creates a new claimed bead update watcher.
func NewClaimed(cfg ClaimedConfig) *Claimed {
	return &Claimed{
		daemon: cfg.Daemon,
		logger: cfg.Logger,
		nudged: make(map[string]time.Time),
	}
}

// RegisterHandlers registers SSE event handlers on the given stream for
// bead updated and closed events.
func (c *Claimed) RegisterHandlers(stream *SSEStream) {
	stream.On("beads.bead.updated", c.handleUpdated)
	stream.On("beads.bead.closed", c.handleClosed)
	c.logger.Info("claimed watcher registered SSE handlers",
		"topics", []string{"beads.bead.updated", "beads.bead.closed"})
}

func (c *Claimed) handleUpdated(ctx context.Context, data []byte) {
	bead := ParseBeadEvent(data)
	if bead == nil {
		c.logger.Debug("skipping malformed bead updated event")
		return
	}

	// Only nudge for claimed beads (assignee present).
	if bead.Assignee == "" {
		return
	}

	// Skip infrastructure/system bead types.
	if skipClaimedTypes[bead.Type] {
		return
	}

	// Skip self-originated updates: the assignee already knows about their
	// own mutation (e.g. their own `kd claim`). Empty actor means auth is
	// disabled or the caller is anonymous — treat as external and nudge.
	if bead.Actor != "" && strings.EqualFold(bead.Actor, bead.Assignee) {
		c.logger.Debug("skipping claimed-update nudge for self-originated update",
			"id", bead.ID, "actor", bead.Actor)
		return
	}

	// Rate-limit: skip if we nudged for this bead recently.
	if !c.shouldNudge(bead.ID) {
		return
	}

	c.logger.Info("claimed bead updated, nudging assignee",
		"id", bead.ID,
		"title", bead.Title,
		"assignee", bead.Assignee,
		"type", bead.Type)

	c.nudgeAgent(ctx, *bead)
}

func (c *Claimed) handleClosed(ctx context.Context, data []byte) {
	bead := ParseBeadEvent(data)
	if bead == nil {
		c.logger.Debug("skipping malformed bead closed event")
		return
	}

	// Only nudge for claimed beads (assignee present).
	if bead.Assignee == "" {
		return
	}

	// Skip infrastructure/system bead types.
	if skipClaimedTypes[bead.Type] {
		return
	}

	// Rate-limit: skip if we nudged for this bead recently.
	if !c.shouldNudge(bead.ID) {
		return
	}

	c.logger.Info("claimed bead closed, nudging assignee to checkpoint",
		"id", bead.ID,
		"title", bead.Title,
		"assignee", bead.Assignee,
		"type", bead.Type)

	message := fmt.Sprintf("Your claimed bead %s %q was closed — if all work is done, wrap up and call gb stop; only create a gb decision if you are blocked on a human choice. If you ALREADY filed a decision and are only waiting on the operator, gb stop too — the decision persists as your re-entry handle and auto-respawns you when it resolves; keep gb yield for a decision you need answered to finish THIS turn",
		bead.ID, bead.Title)

	if err := NudgeAgent(ctx, c.daemon, c.logger, bead.Assignee, message); err != nil {
		if errors.Is(err, beadsapi.ErrAgentNotFound) || errors.Is(err, ErrAgentNotNudgeable) {
			c.logger.Warn("agent gone or terminated, skipping claimed-close nudge",
				"agent", bead.Assignee, "bead", bead.ID)
		} else {
			c.logger.Error("failed to nudge agent for claimed bead close",
				"agent", bead.Assignee, "bead", bead.ID, "error", err)
		}
		return
	}

	c.logger.Info("nudged agent for claimed bead close",
		"agent", bead.Assignee, "bead", bead.ID)
}

// shouldNudge returns true if a nudge should be sent for the given bead ID.
// Enforces a rate limit of one nudge per bead per claimedNudgeTTL window.
// Cleans up expired entries on each call.
func (c *Claimed) shouldNudge(beadID string) bool {
	c.nudgedMu.Lock()
	defer c.nudgedMu.Unlock()

	now := time.Now()

	// Clean up expired entries.
	for id, t := range c.nudged {
		if now.Sub(t) > claimedNudgeTTL {
			delete(c.nudged, id)
		}
	}

	if last, seen := c.nudged[beadID]; seen && now.Sub(last) <= claimedNudgeTTL {
		return false
	}

	c.nudged[beadID] = now
	return true
}

// nudgeAgent delivers a claimed-bead-update nudge to the assigned agent with retry.
func (c *Claimed) nudgeAgent(ctx context.Context, bead BeadEvent) {
	message := fmt.Sprintf("Your claimed bead %s %q was updated — run 'kd show %s' to review",
		bead.ID, bead.Title, bead.ID)

	if err := NudgeAgent(ctx, c.daemon, c.logger, bead.Assignee, message); err != nil {
		switch {
		case errors.Is(err, beadsapi.ErrAgentNotFound) || errors.Is(err, ErrAgentNotNudgeable):
			c.logger.Warn("agent gone or terminated, skipping claimed-update nudge",
				"agent", bead.Assignee, "bead", bead.ID)
		default:
			c.logger.Error("failed to nudge agent for claimed bead update",
				"agent", bead.Assignee, "bead", bead.ID, "error", err)
		}
		return
	}

	c.logger.Info("nudged agent for claimed bead",
		"agent", bead.Assignee, "bead", bead.ID)
}
