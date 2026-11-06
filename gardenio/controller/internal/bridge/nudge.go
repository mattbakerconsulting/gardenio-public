package bridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/mailproto"
)

// ErrAgentNotNudgeable is returned when the agent is in a terminal state
// (done, failed, stopping, budget_exceeded) and cannot receive nudges.
var ErrAgentNotNudgeable = errors.New("agent not nudgeable")

// deadAgentStates are agent states where the agent cannot receive nudges.
var deadAgentStates = map[string]bool{
	"done":            true,
	"failed":          true,
	"stopping":        true,
	"budget_exceeded": true,
}

// agentLooksDead is the liveness test that replaced "has no coop_url": the
// bead is gone or closed, its agent_state is terminal, or it asked to stop.
// Nothing dials the pod any more, so the bead is all there is to go on.
func agentLooksDead(bead *beadsapi.BeadDetail) bool {
	if bead == nil || bead.Status == "closed" {
		return true
	}
	return deadAgentStates[bead.Fields["agent_state"]] || bead.Fields["stop_requested"] == "true"
}

// isAgentTerminal returns true if the agent bead is in a terminal state where
// it can no longer receive nudges — its pod is gone or shutting down. Callers
// resolving a thread/card agent use this to choose a cold respawn (session
// resume) over a nudge that would fail with [ErrAgentNotNudgeable]. Nudging a
// terminal agent yields the misleading "will pick it up on its next cycle"
// message even though a done/failed agent has no next cycle (ka-0000002089).
func isAgentTerminal(state string) bool {
	return deadAgentStates[state]
}

// NudgeBeadClient is the narrow subset of [BeadClient] that [NudgeAgent]
// depends on. Split out so bridge-v2 (which holds a narrower beads client
// than v1) can reuse the same nudge delivery code without pulling in the
// fat [BeadClient] surface.
type NudgeBeadClient interface {
	FindAgentBead(ctx context.Context, agentName string) (*beadsapi.BeadDetail, error)
	CreateBead(ctx context.Context, req beadsapi.CreateBeadRequest) (string, error)
}

// NudgeSender is the from: label stamped on mail this process sends. Every
// other binary that calls NudgeAgent (gitlab-bridge, jira-bridge, controller,
// beads-viewer) sets its own name at the top of main.
var NudgeSender = "slack-bridge"

// NudgeAgentMail delivers message to agentName as a delivery:interrupt mail
// bead and returns its id, for callers that watch delivery
// (Bot.watchMailDelivery) or must close the mail once it no longer applies
// (the controller's drain nudge). The agent's own mail subscriber (internal/mailsub)
// picks it up and hands it to coop on loopback, so nothing dials in to the
// pod. This is the single entry point for all agent nudge delivery in the
// bridge package; [NudgeAgent] is it without the id.
func NudgeAgentMail(ctx context.Context, daemon NudgeBeadClient, logger *slog.Logger, agentName, message string) (string, error) {
	if err := checkNudgeable(ctx, daemon, agentName); err != nil {
		return "", err
	}
	return createInterruptMail(ctx, daemon, logger, agentName, message)
}

// checkNudgeable refuses mail to an agent that has no bead, or whose pod is
// gone or shutting down ([ErrAgentNotNudgeable]): such mail would be written
// for a session that never reads it and reported as sent.
func checkNudgeable(ctx context.Context, daemon NudgeBeadClient, agentName string) error {
	if agentName == "" {
		return fmt.Errorf("empty agent name")
	}

	agentBead, err := daemon.FindAgentBead(ctx, agentName)
	if err != nil {
		return fmt.Errorf("find agent bead %q: %w", agentName, err)
	}

	// Skip nudge for agents in terminal states — the pod is gone or shutting down.
	if state := agentBead.Fields["agent_state"]; deadAgentStates[state] {
		return fmt.Errorf("%w: agent %q is in state %q", ErrAgentNotNudgeable, agentName, state)
	}

	// Skip nudge for agents that have requested stop but whose agent_state has
	// not yet flipped to a terminal value — the "lame-duck" shutdown window.
	//
	// `gb stop` sets stop_requested="true" as its FIRST step (cmd/gb/stop.go),
	// well before the bead's agent_state flips working→done (that flip lags,
	// driven by the statusreporter / coop exit). In the interval
	// [stop_requested=true, agent_state still "working"] the pod is tearing
	// down and its turn is OVER, so without this check the mail is written
	// for a session that will never process it, and reported as success. The bridge then believes
	// delivery worked and never respawns, silently dropping the message (a
	// respond-then-stop thread agent has no "next cycle" to pick it up).
	//
	// Treating stop_requested as not-nudgeable classifies the target as
	// pod-gone (ErrAgentNotNudgeable), which routes the mention path through
	// respawnThreadForGoneNudge → a cold session-
	// resume respawn that re-injects the message — exactly how the NEXT message
	// (after the working→done flip) already succeeds, just ~tens of seconds
	// earlier via the stop_requested signal. This is the mirror of the boot-race
	// (pod not up yet); see ka-0000002972 and epic ka-0000000292.
	if agentBead.Fields["stop_requested"] == "true" {
		return fmt.Errorf("%w: agent %q has stop_requested=true (lame-duck shutdown)", ErrAgentNotNudgeable, agentName)
	}
	return nil
}

// createInterruptMail writes message as a delivery:interrupt mail bead
// assigned to agentName, with no liveness check: the mail waits open until a
// session under that name drains it. [NudgeAgentMail] gates it on a live
// agent; reviveLocalAgentByMail calls it for a laptop agent that is not
// running, whose next session picks the mail up.
func createInterruptMail(ctx context.Context, daemon NudgeBeadClient, logger *slog.Logger, agentName, message string) (string, error) {
	id, err := daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Title:       nudgeTitle(message),
		Type:        "mail",
		Kind:        "data",
		Description: message,
		Assignee:    agentName,
		Labels:      []string{mailproto.FromLabel(NudgeSender), mailproto.LabelDeliveryInterrupt},
		CreatedBy:   NudgeSender,
	})
	if err != nil {
		logger.Warn("nudge mail create failed", "agent", agentName, "error", err)
		return "", fmt.Errorf("create nudge mail for %q: %w", agentName, err)
	}
	logger.Debug("nudge delivered as mail", "agent", agentName, "mail", id)
	return id, nil
}

// NudgeAgent is [NudgeAgentMail] without the mail id.
func NudgeAgent(ctx context.Context, daemon NudgeBeadClient, logger *slog.Logger, agentName, message string) error {
	_, err := NudgeAgentMail(ctx, daemon, logger, agentName, message)
	return err
}

// nudgeTitle is the first line of the message, capped at 120 runes.
func nudgeTitle(message string) string {
	first := message
	if i := strings.IndexByte(message, '\n'); i >= 0 {
		first = message[:i]
	}
	first = strings.TrimSpace(first)
	if r := []rune(first); len(r) > 120 {
		return string(r[:117]) + "..."
	}
	if first == "" {
		return "Message from " + NudgeSender
	}
	return first
}

// startingUpStates are agent states where the agent is still booting; its
// mail waits for the subscriber, which drains unread mail when it starts.
var startingUpStates = map[string]bool{
	"spawning":  true, // pod starting
	"assigning": true, // pool agent being assigned work
}

// isAgentStartingUp returns true if the agent state indicates the agent is
// still booting.
func isAgentStartingUp(state string) bool {
	return startingUpStates[state]
}

// NudgeAgentWithBootRetry is NudgeAgent. Mail needs no boot retry: the
// subscriber drains unread mail when it starts, so a nudge sent before the
// pod is up is delivered once it is. Kept so callers compile.
func NudgeAgentWithBootRetry(ctx context.Context, daemon NudgeBeadClient, logger *slog.Logger, agentName, message string) error {
	return NudgeAgent(ctx, daemon, logger, agentName, message)
}

// PermissionRequest is the payload the gardenio-channel server sends when
// Claude Code hits a tool-approval dialog (Bash, Write, Edit, etc.).
type PermissionRequest struct {
	Type         string `json:"type"`          // "permission_request"
	RequestID    string `json:"request_id"`    // five-letter ID from Claude Code
	ToolName     string `json:"tool_name"`     // e.g. "Bash", "Write"
	Description  string `json:"description"`   // human-readable summary
	InputPreview string `json:"input_preview"` // tool args truncated to ~200 chars
}
