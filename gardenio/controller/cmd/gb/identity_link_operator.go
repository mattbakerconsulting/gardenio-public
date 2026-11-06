package main

import (
	"context"
	"fmt"
	"strings"

	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
)

// identityLinkOperatorCmd links the caller's OIDC operator row to their
// Slack operator row by attaching the slack alias to the OIDC operator. The
// daemon only accepts an alias recorded for the caller's email on an
// admin-written user bead (403 otherwise) and never merges: if the alias
// already belongs to a separate (legacy) operator row it answers 409 and a
// gardenio-admin merges via POST /v1/admin/operators/link-aliases.
var identityLinkOperatorCmd = &cobra.Command{
	Use:   "link-operator",
	Short: "Link the caller's OIDC operator to their Slack operator",
	Long: `Attach the matching Slack alias to your OIDC operator row so the inbox
and /active?operated_by=me find agents you spawned from Slack.

The command:
  1. Calls /v1/whoami to resolve your OIDC email.
  2. Looks up the user-identity bead matching that email and reads slack_id.
  3. Calls /v1/operators/me/aliases to attach (kind=slack, subject=<slack_id>)
     to your OIDC operator. The daemon accepts it only when an admin-written
     user bead for your email carries that alias (403 otherwise). It never
     merges: if the alias already belongs to another operator row it answers
     409 and a gardenio-admin must merge the rows.

The command is idempotent — replaying it after the link is in place returns
AlreadyLinked=true without writing.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		slackOverride, _ := cmd.Flags().GetString("slack-id")
		emailOverride, _ := cmd.Flags().GetString("email")

		who, err := daemon.Whoami(ctx)
		if err != nil {
			return fmt.Errorf("whoami: %w", err)
		}
		if who.Source != "auth" || who.OperatorID == "" {
			return fmt.Errorf("identity link-operator requires OIDC authentication (whoami source=%q)", who.Source)
		}
		email := strings.TrimSpace(emailOverride)
		if email == "" {
			email = who.OperatorID
		}

		slackID := strings.TrimSpace(slackOverride)
		if slackID == "" {
			slackID, err = lookupSlackIDForEmail(ctx, email)
			if err != nil {
				return err
			}
		}
		if err := validateSlackUserID("--slack-id", slackID); err != nil {
			return err
		}

		resp, err := daemon.LinkOperatorAlias(ctx, beadsapi.LinkOperatorAliasRequest{
			Kind:    "slack",
			Subject: slackID,
			Source:  "identity-link",
		})
		if err != nil {
			return fmt.Errorf("linking operator alias: %w", err)
		}

		if jsonOutput {
			printJSON(resp)
			return nil
		}

		switch {
		case resp.AlreadyLinked:
			fmt.Printf("Already linked: operator %d ← slack:%s\n", resp.OperatorID, resp.Subject)
		case resp.Merged:
			fmt.Printf("Linked operator %d ← slack:%s (merged operator %d, %d rows repointed)\n",
				resp.OperatorID, resp.Subject, resp.MergedFromID, resp.RowsRepointed)
		default:
			fmt.Printf("Linked operator %d ← slack:%s\n", resp.OperatorID, resp.Subject)
		}
		return nil
	},
}

// lookupSlackIDForEmail finds the user-identity bead matching email and
// returns its slack_id field. Returns an error if no bead is found or the
// match has no slack_id.
func lookupSlackIDForEmail(ctx context.Context, email string) (string, error) {
	result, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"user"},
		Kinds:    []string{"config"},
		Statuses: []string{"open", "in_progress"},
		Limit:    500,
	})
	if err != nil {
		return "", fmt.Errorf("listing user identities: %w", err)
	}
	for _, b := range result.Beads {
		if strings.EqualFold(b.Fields["email"], email) {
			slackID := strings.TrimSpace(b.Fields["slack_id"])
			if slackID == "" {
				return "", fmt.Errorf("user identity %s (email=%s) has no slack_id", b.ID, email)
			}
			return slackID, nil
		}
	}
	return "", fmt.Errorf("no user identity bead found for email %q (pass --slack-id to override)", email)
}

func init() {
	identityLinkOperatorCmd.Flags().String("slack-id", "", "override slack user ID (skip user-bead lookup)")
	identityLinkOperatorCmd.Flags().String("email", "", "override email used for the user-bead lookup (defaults to whoami)")
	identityCmd.AddCommand(identityLinkOperatorCmd)
}
