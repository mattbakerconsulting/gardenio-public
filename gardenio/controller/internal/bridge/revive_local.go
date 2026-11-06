package bridge

import (
	"context"
	"log/slog"

	"gardenio/controller/internal/beadsapi"
)

// reviveLocalAgentByMail is the one rule every revival path shares
// (local-agents spec §7, "Revival paths obey the runtime"): a laptop agent
// (execution_target=laptop, see isLaptopAgent) is never respawned as a pod.
// The event that would have respawned it is written as one interrupt mail to
// the agent instead; its runner delivers it when the agent is running, and
// the mail waits for the next session under that name otherwise (D5). The
// mail skips NudgeAgentMail's liveness gate: the caller is here because that
// nudge already failed, and a stopped laptop agent's bead is closed.
//
// Returns true when the caller must stop (the agent is a laptop agent, and
// the mail was created or its failure was logged); false otherwise. Nothing
// is spawned and no bead is closed. execution_target=local is not a laptop
// agent: the escalation bridge stamps it on fleet pods, which still respawn
// (gd-SzPJDNTBXS).
func reviveLocalAgentByMail(ctx context.Context, daemon NudgeBeadClient, logger *slog.Logger, agentBead *beadsapi.BeadDetail, reason, message string) bool {
	if !isLaptopAgent(agentBead) {
		return false
	}
	name := agentBead.Fields["agent"]
	if name == "" {
		name = agentBead.Title
	}
	if name == "" {
		logger.Warn("local agent revival: laptop agent bead has no agent name; nothing spawned, no mail sent",
			"agent_bead", agentBead.ID)
		return true
	}
	mailID, err := createInterruptMail(ctx, daemon, logger, name, reason+"\n\n"+message)
	if err != nil {
		logger.Warn("local agent revival: mail not created; the event is lost until the next one",
			"agent", name, "agent_bead", agentBead.ID, "error", err)
		return true
	}
	logger.Info("local agent revival: sent mail instead of spawning a pod",
		"agent", name, "agent_bead", agentBead.ID, "mail", mailID)
	return true
}
