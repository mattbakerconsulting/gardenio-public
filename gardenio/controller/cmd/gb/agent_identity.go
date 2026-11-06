package main

import (
	"context"
	"fmt"
	"os"

	"gardenio/controller/internal/beadsapi"
)

// resolveAgentID returns the agent identity for the current session.
// Priority: KD_AGENT_ID env > error.
//
// In K8s agent pods, KD_AGENT_ID is always set by the controller (via podspec).
// For local development, the user must set it explicitly or use --agent-id.
func resolveAgentID(flagValue string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	if id := os.Getenv("KD_AGENT_ID"); id != "" {
		return id, nil
	}
	return "", fmt.Errorf("agent ID not set: use --agent-id or set KD_AGENT_ID")
}

// resolveAgentByActor looks up an open agent bead by the actor's assignee name.
// Returns empty string if not found or on error.
func resolveAgentByActor(ctx context.Context, actorName string) string {
	if actorName == "" || actorName == "unknown" {
		return ""
	}
	result, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"agent"},
		Assignee: actorName,
		Statuses: []string{"open", "in_progress"},
		Sort:     "-created_at",
		Limit:    1,
	})
	if err != nil || len(result.Beads) == 0 {
		return ""
	}
	return result.Beads[0].ID
}

// resolveAgentIDWithFallback resolves agent bead ID with actor fallback.
// Priority: flagValue > KD_AGENT_ID env > actor-based lookup.
func resolveAgentIDWithFallback(ctx context.Context, flagValue string) (string, error) {
	id, err := resolveAgentID(flagValue)
	if err == nil {
		return id, nil
	}
	agentID := resolveAgentByActor(ctx, actor)
	if agentID == "" {
		return "", fmt.Errorf("no agent identity found (set KD_AGENT_ID, use --agent-id, or set KD_ACTOR)")
	}
	return agentID, nil
}

// deriveRequestingTask returns the caller-supplied requesting task unchanged
// when set; otherwise it auto-derives it from the current agent's bead
// fields.task_id (Layer 3 source-prevention, ka-0000002745 / design
// ka-0000006833 §3). A decision/escalation created without --requesting-task
// then still runs the existing blocks-edge + requesting_plan homing path off
// the agent's task, instead of orphaning off every /plans surface.
//
// Best-effort by design: any resolution failure (no agent identity, GetBead
// error, no task_id field) returns "" so creation is never blocked — the
// caller's no-homing warning fires instead. Verified agent beads carry
// fields.task_id (e.g. the spawning gardener stamps task_id at spawn time).
//
// CAVEAT (design §2 Option 1): for a scheduled gardener the derived task is the
// short-lived CYCLE bead, so the blocks-edge this enables still goes stale in
// ~30m. This is hygiene that shrinks the orphan population, not the fix —
// Layer 1's /plans unhomed backstop is the safety net.
func deriveRequestingTask(ctx context.Context, requestingTask string) string {
	if requestingTask != "" {
		return requestingTask
	}
	agentID, err := resolveAgentIDWithFallback(ctx, "")
	if err != nil || agentID == "" {
		return ""
	}
	bead, err := daemon.GetBead(ctx, agentID)
	if err != nil || bead == nil {
		return ""
	}
	return bead.Fields["task_id"]
}
