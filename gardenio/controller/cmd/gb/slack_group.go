package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// This file adds the group-DM (mpim) start command to `gb slack`:
//   gb slack group open <user...> [--project X] [--role Y] [--message ...] [--prompt ...]
//
// It backs POST /api/slack/groups/open on the slack-bridge (Phase 2 of epic
// ka-0000003709). The command opens a Slack group DM with the given members
// and, when --project or --role is set (or --spawn is passed), spawns an agent
// bound to that group DM.
//
// Users may be Slack user IDs (U…/W…) or email addresses; emails are resolved
// bridge-side via users.lookupByEmail, mirroring `gb slack invite`.

// slackGroupCmd is the `gb slack group` command group.
var slackGroupCmd = &cobra.Command{
	Use:   "group",
	Short: "Manage gardenio group DMs (mpim)",
	Long: `Start and manage Slack group DMs (mpim) that gardenio participates in.

Subcommands:
  open    Open a new group DM, optionally spawning a bound agent.
  invite  Add user(s) to a bot-started group DM (re-opens a superset mpim).

In a group DM gardenio responds only when explicitly @mentioned (an mpim has
several human members, so — unlike a 1:1 DM — the bot must be addressed by name).
The first @mention spawns an agent bound to the channel; later turns route to it.

Slack has NO in-place mpim invite, so 'invite' re-opens a superset conversation
(a new channel id, no shared history) and migrates the bound agent — see
'gb slack group invite --help'. Full runbook (participate/start/invite semantics,
the no-in-place-invite constraint, project/role selection): gardenio/docs/group-dm.md.`,
}

// slackGroupOpenCmd is `gb slack group open`.
var slackGroupOpenCmd = &cobra.Command{
	Use:   "open <user>...",
	Short: "Open a group DM, optionally spawning a bound agent",
	Long: `Opens a Slack group DM (mpim) with the given members and posts an intro
message. When --project or --role is supplied (or --spawn is passed), an agent
is spawned and bound to the group DM so replies route to it.

Users may be Slack user IDs (U…/W…) or email addresses; emails are resolved to
IDs bridge-side. At least one user is required (Slack opens the mpim with the
bot plus the members).

Examples:
  gb slack group open U0123ABCD U0456DEFG
  gb slack group open alice@x.com bob@x.com --project gardenio --role crew
  gb slack group open U0123ABCD --message "Kicking off the incident bridge" --prompt "Investigate the pager"`,
	Args: cobra.MinimumNArgs(1),
	RunE: runSlackGroupOpen,
}

func runSlackGroupOpen(cmd *cobra.Command, args []string) error {
	bridgeURL := resolveBridgeURL()
	if bridgeURL == "" {
		return fmt.Errorf("SLACK_BRIDGE_URL is not set — cannot reach the Slack bridge")
	}

	project, _ := cmd.Flags().GetString("project")
	role, _ := cmd.Flags().GetString("role")
	message, _ := cmd.Flags().GetString("message")
	prompt, _ := cmd.Flags().GetString("prompt")
	spawn, _ := cmd.Flags().GetBool("spawn")

	// --project, --role, or --prompt each imply spawning an agent; --spawn forces
	// it explicitly (e.g. to spawn on defaults with no other flag).
	if project != "" || role != "" || prompt != "" {
		spawn = true
	}

	body := map[string]any{
		"users": args,
		"spawn": spawn,
	}
	if project != "" {
		body["project"] = project
	}
	if role != "" {
		body["role"] = role
	}
	if message != "" {
		body["message"] = message
	}
	if prompt != "" {
		body["prompt"] = prompt
	}

	var result slackGroupOpenResponse
	if err := postSlackJSON(bridgeURL, "/api/slack/groups/open", body, &result); err != nil {
		return err
	}

	if jsonOutput {
		printJSON(result)
		return nil
	}
	if !result.OK {
		return fmt.Errorf("group open failed: %s", result.Error)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Opened group DM %s", result.Channel)
	if result.Agent != "" {
		fmt.Fprintf(&b, " (agent %s bound)", result.Agent)
	}
	fmt.Println(b.String())
	return nil
}

// slackGroupOpenResponse mirrors the bridge's groupOpenResponse.
type slackGroupOpenResponse struct {
	OK      bool   `json:"ok"`
	Channel string `json:"channel,omitempty"`
	Agent   string `json:"agent,omitempty"`
	AgentID string `json:"agent_id,omitempty"`
	TS      string `json:"ts,omitempty"`
	Error   string `json:"error,omitempty"`
}

func init() {
	slackGroupOpenCmd.Flags().String("project", "", "project to scope the spawned agent (implies --spawn)")
	slackGroupOpenCmd.Flags().String("role", "", "role/mode for the spawned agent (default crew; implies --spawn)")
	slackGroupOpenCmd.Flags().String("message", "", "intro message to post in the group DM")
	slackGroupOpenCmd.Flags().String("prompt", "", "custom prompt for the spawned agent (implies --spawn)")
	slackGroupOpenCmd.Flags().Bool("spawn", false, "spawn and bind an agent even with no other flag")

	slackGroupCmd.AddCommand(slackGroupOpenCmd)
	slackCmd.AddCommand(slackGroupCmd)
}
