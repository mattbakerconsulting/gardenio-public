package main

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
)

// This file adds the group-DM (mpim) invite command to `gb slack group`:
//   gb slack group invite <channel> <new-user>... [--message ...]
//
// It backs POST /api/slack/groups/{channel}/invite on the slack-bridge (Phase 3
// of epic ka-0000003709, task ka-0000006522).
//
// IMPORTANT — there is NO in-place mpim invite in Slack. conversations.invite is
// channels-only; conversations.open with a new member set yields a DISTINCT
// conversation with a distinct channel ID and no shared history. So `invite`
// re-opens a SUPERSET mpim (existing members + the new user), migrates the bound
// agent to the new channel, and posts a context hand-off in both the old and new
// conversations. Expect a NEW channel id in the output when the invitee was not
// already a member.
//
// Users may be Slack user IDs (U…/W…) or email addresses; emails are resolved
// bridge-side via users.lookupByEmail, mirroring `gb slack group open`.

// slackGroupInviteCmd is `gb slack group invite`.
var slackGroupInviteCmd = &cobra.Command{
	Use:   "invite <channel> <new-user>...",
	Short: "Invite user(s) into a group DM (re-opens a superset mpim — see note)",
	Long: `Invite one or more users into an existing group DM (mpim).

Slack has NO in-place mpim invite: conversations.invite is channels-only, and
conversations.open with a new member set yields a DISTINCT conversation with a
new channel id and no shared history. So this command re-opens a SUPERSET mpim
(the existing members plus the new user(s)), migrates any bound agent to the new
channel, and posts a one-line context hand-off in BOTH the old and new group DMs.

Because a genuinely new invitee produces a NEW conversation, the command prints
the new channel id (which differs from <channel>). Inviting a user who is already
a member is an idempotent no-op that returns the same channel.

Users may be Slack user IDs (U…/W…) or email addresses; emails are resolved to
IDs bridge-side.

Examples:
  gb slack group invite G0123MPIM U0999NEWUSER
  gb slack group invite G0123MPIM carol@x.com --message "Carol, welcome — here's the context"`,
	Args: cobra.MinimumNArgs(2),
	RunE: runSlackGroupInvite,
}

func runSlackGroupInvite(cmd *cobra.Command, args []string) error {
	bridgeURL := resolveBridgeURL()
	if bridgeURL == "" {
		return fmt.Errorf("SLACK_BRIDGE_URL is not set — cannot reach the Slack bridge")
	}

	channel := strings.TrimSpace(args[0])
	if channel == "" {
		return fmt.Errorf("channel (mpim id) is required")
	}
	users := args[1:]
	message, _ := cmd.Flags().GetString("message")

	body := map[string]any{"users": users}
	if message != "" {
		body["message"] = message
	}

	path := "/api/slack/groups/" + url.PathEscape(channel) + "/invite"
	var result slackGroupInviteResponse
	if err := postSlackJSON(bridgeURL, path, body, &result); err != nil {
		return err
	}

	if jsonOutput {
		printJSON(result)
		return nil
	}
	if !result.OK {
		return fmt.Errorf("group invite failed: %s", result.Error)
	}

	var b strings.Builder
	if result.Moved {
		fmt.Fprintf(&b, "Invited into a new group DM %s (was %s)", result.Channel, result.OldChannel)
	} else {
		fmt.Fprintf(&b, "User(s) already in group DM %s — no new conversation created", result.Channel)
	}
	if result.Agent != "" {
		fmt.Fprintf(&b, "; agent %s carried over", result.Agent)
	}
	fmt.Println(b.String())
	return nil
}

// slackGroupInviteResponse mirrors the bridge's groupInviteResponse.
type slackGroupInviteResponse struct {
	OK         bool   `json:"ok"`
	OldChannel string `json:"old_channel,omitempty"`
	Channel    string `json:"channel,omitempty"`
	Moved      bool   `json:"moved"`
	Agent      string `json:"agent,omitempty"`
	Members    int    `json:"members,omitempty"`
	Error      string `json:"error,omitempty"`
}

func init() {
	slackGroupInviteCmd.Flags().String("message", "", "hand-off message posted in the new group DM")
	slackGroupCmd.AddCommand(slackGroupInviteCmd)
}
