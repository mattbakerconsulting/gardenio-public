package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/lands/purse"
	"gardenio/controller/internal/lands/wall"
	"gardenio/controller/internal/steward"

	"github.com/spf13/cobra"
)

// The steward's kill switch (Lands step 9.5, steward amendment 2026-10-23
// §5): gb agent kill within the land always works for its steward, whatever
// the grant and whoever spawned the agent, and every kill is said in the
// land's #machines with the grant named. The daemon's authz (the land scope
// of the steward's token) still decides the close; this only lifts gb's own
// spawned-by-you check, which a steward cannot meet because each wake is a
// new run.

// stewardKillFailureReason marks an agent bead its steward killed.
const stewardKillFailureReason = "killed-by-steward"

// stewardKillLand reports the land when the caller is a steward
// (BOAT_ROLE) of the target's land (BOAT_PROJECT). A steward is never
// killed this way, itself included: stewards are retired by gb lands.
func stewardKillLand(targetProject, targetRole string) (string, bool) {
	land := strings.TrimSpace(os.Getenv("BOAT_PROJECT"))
	if land == "" || land != targetProject || !wall.IsSteward(os.Getenv("BOAT_ROLE")) || wall.IsSteward(targetRole) {
		return "", false
	}
	return land, true
}

// stewardKillLine is what #machines is told.
func stewardKillLine(agent *beadsapi.AgentBead, reason string, grant steward.Authority) string {
	step := ""
	if t := strings.TrimSpace(agent.Metadata["task_id"]); t != "" {
		step = " on " + t
	}
	return fmt.Sprintf("Killed %s%s: %s. (kill switch, under grant %s)", agent.AgentName, step, reason, grant)
}

func runStewardKill(cmd *cobra.Command, agent *beadsapi.AgentBead, land, reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return fmt.Errorf("a steward's kill needs --reason: it is said in #machines")
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	// agent_state failed: the type:agent enum has no "killed".
	if err := daemon.CloseBead(ctx, agent.ID, map[string]string{
		"agent_state": "failed", "failure_reason": stewardKillFailureReason, "kill_reason": reason, "stop_requested": "true",
	}); err != nil {
		return fmt.Errorf("killing agent %q: %w", agent.AgentName, err)
	}
	grant := steward.AuthorityAsk
	if proj, _, err := purse.Project(ctx, daemon, land); err == nil && proj != nil {
		grant = steward.ParseAuthority(proj.Fields[steward.AuthorityField])
	}
	line := stewardKillLine(agent, reason, grant)
	said := ""
	if bridgeURL := resolveBridgeURL(); bridgeURL != "" {
		req := newSayParams(line)
		req.Channel = steward.MachinesChannel
		if res, err := conversationSay(ctx, bridgeURL, req, nil); err != nil {
			fmt.Fprintf(os.Stderr, "WARNING: %s is killed, but #machines was not told: %v\nSay it yourself: gb say --channel %s %q\n", agent.AgentName, err, steward.MachinesChannel, line)
		} else if res != nil {
			said = res.ID
		}
	} else {
		fmt.Fprintf(os.Stderr, "WARNING: %s is killed, but SLACK_BRIDGE_URL is not set, so #machines was not told.\nSay it yourself: gb say --channel %s %q\n", agent.AgentName, steward.MachinesChannel, line)
	}
	if jsonOutput {
		printJSON(map[string]string{"id": agent.ID, "name": agent.AgentName, "state": "killed", "action": "bead_closed", "said": said})
		return nil
	}
	cmd.Printf("Killed agent %s (%s) — bead closed; reconciler will delete the pod within one tick.\n", agent.AgentName, agent.ID)
	return nil
}
