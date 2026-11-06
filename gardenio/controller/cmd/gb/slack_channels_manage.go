package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/spf13/cobra"
)

// This file adds the channel-management write commands to `gb slack`:
// channel-create, channel-archive, channel-unarchive, and invite. They call
// the slack-bridge write endpoints registered by SlackChannelAPI.RegisterManageRoutes.
//
// "destroy" maps to Slack's archive (reversible) — Slack has no hard channel
// delete in its public API.

// ── slack channel-create ───────────────────────────────────────────

var slackChannelCreateCmd = &cobra.Command{
	Use:   "channel-create <name>",
	Short: "Create a public or private Slack channel",
	Long: `Creates a Slack channel and optionally sets its topic/purpose and
invites an initial set of members.

The name is normalized to Slack's rules (lowercase, no spaces). Use --private
for a private channel. Invitees may be Slack user IDs (U…) or email addresses.

Examples:
  gb slack channel-create proj-falcon --topic "Falcon launch"
  gb slack channel-create incident-1234 --private --invite alice@x.com,U0123ABCD`,
	Args: cobra.ExactArgs(1),
	RunE: runSlackChannelCreate,
}

func runSlackChannelCreate(cmd *cobra.Command, args []string) error {
	bridgeURL := resolveBridgeURL()
	if bridgeURL == "" {
		return fmt.Errorf("SLACK_BRIDGE_URL is not set — cannot reach the Slack bridge")
	}

	private, _ := cmd.Flags().GetBool("private")
	topic, _ := cmd.Flags().GetString("topic")
	purpose, _ := cmd.Flags().GetString("purpose")
	inviteSpec, _ := cmd.Flags().GetString("invite")

	body := map[string]any{
		"name":    args[0],
		"private": private,
	}
	if topic != "" {
		body["topic"] = topic
	}
	if purpose != "" {
		body["purpose"] = purpose
	}
	if invitees := splitCSV(inviteSpec); len(invitees) > 0 {
		body["invite"] = invitees
	}

	var result slackCreateChannelResponse
	if err := postSlackJSON(bridgeURL, "/api/slack/channels/create", body, &result); err != nil {
		return err
	}

	if jsonOutput {
		printJSON(result)
		return nil
	}

	if !result.OK {
		return fmt.Errorf("create failed: %s", result.Error)
	}
	kind := "public"
	if result.Private {
		kind = "private"
	}
	fmt.Printf("Created %s channel #%s (%s)\n", kind, result.Name, result.ID)
	if len(result.Invited) > 0 {
		fmt.Printf("Invited %d member(s): %s\n", len(result.Invited), strings.Join(result.Invited, ", "))
	}
	if result.Warning != "" {
		fmt.Printf("Warning: %s\n", result.Warning)
	}
	return nil
}

// ── slack channel-archive / channel-unarchive ──────────────────────

var slackChannelArchiveCmd = &cobra.Command{
	Use:   "channel-archive <channel>",
	Short: "Archive (destroy) a Slack channel",
	Long: `Archives a Slack channel. Archiving is Slack's reversible form of
"destroy" — there is no hard channel delete in the public API. Use
channel-unarchive to restore. Channel may be an ID (C…/G…) or a name.`,
	Args: cobra.ExactArgs(1),
	RunE: runSlackChannelArchive,
}

func runSlackChannelArchive(cmd *cobra.Command, args []string) error {
	return runChannelAction(cmd, args[0], "/api/slack/channels/archive", "Archived")
}

var slackChannelUnarchiveCmd = &cobra.Command{
	Use:   "channel-unarchive <channel>",
	Short: "Restore a previously archived Slack channel",
	Args:  cobra.ExactArgs(1),
	RunE:  runSlackChannelUnarchive,
}

func runSlackChannelUnarchive(cmd *cobra.Command, args []string) error {
	return runChannelAction(cmd, args[0], "/api/slack/channels/unarchive", "Unarchived")
}

func runChannelAction(_ *cobra.Command, channelArg, path, verb string) error {
	bridgeURL := resolveBridgeURL()
	if bridgeURL == "" {
		return fmt.Errorf("SLACK_BRIDGE_URL is not set — cannot reach the Slack bridge")
	}

	channelID, err := resolveChannelArg(bridgeURL, channelArg)
	if err != nil {
		return err
	}

	var result slackChannelActionResponse
	if err := postSlackJSON(bridgeURL, path, map[string]any{"channel": channelID}, &result); err != nil {
		return err
	}

	if jsonOutput {
		printJSON(result)
		return nil
	}
	if !result.OK {
		return fmt.Errorf("%s failed: %s", strings.ToLower(verb), result.Error)
	}
	fmt.Printf("%s channel %s\n", verb, channelID)
	return nil
}

// ── slack invite ───────────────────────────────────────────────────

var slackInviteCmd = &cobra.Command{
	Use:   "invite <channel> <user>...",
	Short: "Invite users to a Slack channel",
	Long: `Invites one or more users to a Slack channel for collaboration.
Users may be Slack user IDs (U…) or email addresses. Channel may be an ID
(C…/G…) or a name.

Examples:
  gb slack invite proj-falcon alice@x.com bob@x.com
  gb slack invite C0123ABCD U0456DEFG`,
	Args: cobra.MinimumNArgs(2),
	RunE: runSlackInvite,
}

func runSlackInvite(cmd *cobra.Command, args []string) error {
	bridgeURL := resolveBridgeURL()
	if bridgeURL == "" {
		return fmt.Errorf("SLACK_BRIDGE_URL is not set — cannot reach the Slack bridge")
	}

	channelID, err := resolveChannelArg(bridgeURL, args[0])
	if err != nil {
		return err
	}

	var result slackInviteResponse
	body := map[string]any{"channel": channelID, "users": args[1:]}
	if err := postSlackJSON(bridgeURL, "/api/slack/channels/invite", body, &result); err != nil {
		return err
	}

	if jsonOutput {
		printJSON(result)
		return nil
	}
	if !result.OK {
		return fmt.Errorf("invite failed: %s", result.Error)
	}
	fmt.Printf("Invited %d member(s) to %s: %s\n", len(result.Invited), channelID, strings.Join(result.Invited, ", "))
	if result.Error != "" {
		fmt.Printf("Note: %s\n", result.Error)
	}
	return nil
}

// ── helpers ────────────────────────────────────────────────────────

// resolveChannelArg resolves a channel name to an ID, passing through values
// that already look like channel IDs (C…/G…).
func resolveChannelArg(bridgeURL, arg string) (string, error) {
	arg = strings.TrimPrefix(arg, "#")
	if strings.HasPrefix(arg, "C") || strings.HasPrefix(arg, "G") {
		return arg, nil
	}
	return resolveChannelName(bridgeURL, arg)
}

// splitCSV splits a comma-separated flag value into trimmed, non-empty parts.
func splitCSV(s string) []string {
	var out []string
	for part := range strings.SplitSeq(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// postSlackJSON POSTs a JSON body to a bridge endpoint and decodes the response.
func postSlackJSON(bridgeURL, path string, body any, out any) error {
	u := strings.TrimRight(bridgeURL, "/") + path
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshaling request: %w", err)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, u, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := slackHTTPClient().Do(req)
	if err != nil {
		return fmt.Errorf("calling bridge: %w", err)
	}
	defer resp.Body.Close()

	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decoding response (HTTP %d): %w", resp.StatusCode, err)
	}
	return nil
}

// ── response types ─────────────────────────────────────────────────

type slackCreateChannelResponse struct {
	OK      bool     `json:"ok"`
	ID      string   `json:"id,omitempty"`
	Name    string   `json:"name,omitempty"`
	Private bool     `json:"private,omitempty"`
	Invited []string `json:"invited,omitempty"`
	Warning string   `json:"warning,omitempty"`
	Error   string   `json:"error,omitempty"`
}

type slackChannelActionResponse struct {
	OK      bool   `json:"ok"`
	Channel string `json:"channel,omitempty"`
	Error   string `json:"error,omitempty"`
}

type slackInviteResponse struct {
	OK      bool     `json:"ok"`
	Channel string   `json:"channel,omitempty"`
	Invited []string `json:"invited,omitempty"`
	Error   string   `json:"error,omitempty"`
}

func init() {
	slackChannelCreateCmd.Flags().Bool("private", false, "create a private channel")
	slackChannelCreateCmd.Flags().String("topic", "", "set the channel topic")
	slackChannelCreateCmd.Flags().String("purpose", "", "set the channel purpose/description")
	slackChannelCreateCmd.Flags().String("invite", "", "comma-separated user IDs or emails to invite on creation")

	slackCmd.AddCommand(slackChannelCreateCmd)
	slackCmd.AddCommand(slackChannelArchiveCmd)
	slackCmd.AddCommand(slackChannelUnarchiveCmd)
	slackCmd.AddCommand(slackInviteCmd)
}
