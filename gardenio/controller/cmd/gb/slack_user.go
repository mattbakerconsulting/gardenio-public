package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
)

// slackUserInfo mirrors bridge.SlackUserInfo (/api/slack/users/lookup).
type slackUserInfo struct {
	ID          string `json:"id"`
	Handle      string `json:"handle"`
	DisplayName string `json:"display_name,omitempty"`
	RealName    string `json:"real_name,omitempty"`
	Email       string `json:"email,omitempty"`
	Title       string `json:"title,omitempty"`
	IsBot       bool   `json:"is_bot"`
}

var slackUserCmd = &cobra.Command{
	Use:   "user <@alias|U0123ABC|email|name>",
	Short: "Look up a Slack user's name and email from their alias",
	Long: `Resolve a Slack @alias, user ID, <@U..> mention, email or display name to the
person's name and email, live from the Slack workspace (no seeded user bead
needed). An ambiguous query prints every match. Requires SLACK_BRIDGE_URL.

Examples:
  gb slack user @alice            # by @alias (handle)
  gb slack user U0123ABC          # by Slack user ID
  gb slack user alice@example.com # by email
  gb slack user "Alice Smith"     # by display / real name
  gb slack user @alice --json`,
	Args: cobra.ExactArgs(1),
	RunE: runSlackUser,
}

func runSlackUser(cmd *cobra.Command, args []string) error {
	bridgeURL := resolveBridgeURL()
	if bridgeURL == "" {
		return fmt.Errorf("SLACK_BRIDGE_URL is not set — cannot reach the Slack bridge")
	}

	u := strings.TrimRight(bridgeURL, "/") + "/api/slack/users/lookup?q=" + url.QueryEscape(args[0])
	resp, err := slackHTTPClient().Get(u)
	if err != nil {
		return fmt.Errorf("looking up user: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusTooManyRequests:
		return fmt.Errorf("slack rate limited while looking up user — retry in %ss", resp.Header.Get("Retry-After"))
	default:
		return fmt.Errorf("bridge returned HTTP %d while looking up user", resp.StatusCode)
	}

	var users []slackUserInfo
	if err := json.NewDecoder(resp.Body).Decode(&users); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}

	if jsonOutput {
		printJSON(users)
		return nil
	}
	if len(users) == 0 {
		return fmt.Errorf("no Slack user found matching %q", args[0])
	}
	for _, usr := range users {
		name := usr.RealName
		if name == "" {
			name = usr.DisplayName
		}
		email := usr.Email
		if email == "" {
			email = "(no email visible)"
		}
		fmt.Printf("%s  @%s  %s  %s\n", usr.ID, usr.Handle, name, email)
	}
	return nil
}
