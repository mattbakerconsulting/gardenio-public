package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"gardenio/controller/internal/beadsapi"

	"github.com/slack-go/slack"
	"github.com/spf13/cobra"
)

var identitySeedCmd = &cobra.Command{
	Use:   "seed",
	Short: "Seed user beads from Slack workspace members",
	Long: `Populate user identity beads from the Slack workspace member list.

For each Slack user (excluding bots and deactivated accounts), creates
a user bead with slack_id, display_name, and email. Skips users that
already have a user bead (matched by slack_id).

Requires SLACK_BOT_TOKEN or --token with a Slack bot token that has
the users:read scope.

Requires an OIDC gardenio-admin on the beads daemon: the user beads carry
identity fields (slack_id, email), and the daemon answers 403 to the shared
static token and to fleet tokens. See
gardenio/docs/security/user-identity-fields.md.

Examples:
  gb identity seed                         # seed from SLACK_BOT_TOKEN
  gb identity seed --token xoxb-...        # explicit token
  gb identity seed --dry-run               # preview without creating beads
  gb identity seed --include-bots          # include bot users`,
	RunE: runIdentitySeed,
}

var (
	seedToken       string
	seedDryRun      bool
	seedIncludeBots bool
)

func init() {
	identitySeedCmd.Flags().StringVar(&seedToken, "token", "", "Slack bot token (default: SLACK_BOT_TOKEN env)")
	identitySeedCmd.Flags().BoolVar(&seedDryRun, "dry-run", false, "preview changes without creating beads")
	identitySeedCmd.Flags().BoolVar(&seedIncludeBots, "include-bots", false, "include bot users")
}

func runIdentitySeed(cmd *cobra.Command, args []string) error {
	token := seedToken
	if token == "" {
		token = os.Getenv("SLACK_BOT_TOKEN")
	}
	if token == "" {
		return fmt.Errorf("no Slack token: set SLACK_BOT_TOKEN or use --token")
	}

	ctx := cmd.Context()

	// 1. Fetch existing user beads to deduplicate.
	existing, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"user"},
		Kinds:    []string{"config"},
		Statuses: []string{"open", "in_progress"},
		Limit:    1000,
	})
	if err != nil {
		return fmt.Errorf("listing existing user beads: %w", err)
	}

	existingBySlack := make(map[string]string) // slack_id -> bead ID
	for _, b := range existing.Beads {
		if sid := b.Fields["slack_id"]; sid != "" {
			existingBySlack[sid] = b.ID
		}
	}

	// 2. Fetch Slack workspace members (pagination handled automatically).
	api := slack.New(token)
	allUsers, err := api.GetUsersContext(ctx)
	if err != nil {
		return fmt.Errorf("fetching Slack users: %w", err)
	}

	// 3. Filter and create beads.
	var created, skippedExisting, skippedBot, skippedDeleted int
	for _, u := range allUsers {
		if u.Deleted {
			skippedDeleted++
			continue
		}
		if u.IsBot || u.ID == "USLACKBOT" {
			if !seedIncludeBots {
				skippedBot++
				continue
			}
		}

		if _, exists := existingBySlack[u.ID]; exists {
			skippedExisting++
			continue
		}

		displayName := u.Profile.RealName
		if displayName == "" {
			displayName = u.Name
		}

		fields := map[string]string{
			"slack_id":     u.ID,
			"display_name": displayName,
		}
		if u.Profile.Email != "" {
			fields["email"] = u.Profile.Email
		}
		if u.Profile.Pronouns != "" {
			fields["pronouns"] = u.Profile.Pronouns
		}
		if u.IsBot {
			fields["team"] = "bot"
		}

		if seedDryRun {
			fmt.Printf("[dry-run] Would create: %s (%s) %s\n", displayName, u.ID, u.Profile.Email)
			created++
			continue
		}

		fieldsJSON, err := json.Marshal(fields)
		if err != nil {
			fmt.Fprintf(os.Stderr, "WARN: skip %s: %v\n", u.ID, err)
			continue
		}

		id, err := daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
			Title:     displayName,
			Type:      "user",
			Kind:      "config",
			Fields:    json.RawMessage(fieldsJSON),
			CreatedBy: actor,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "WARN: failed to create bead for %s (%s): %v\n", displayName, u.ID, err)
			continue
		}

		created++
		if !jsonOutput {
			fmt.Printf("Created %s: %s (%s)\n", id, displayName, u.ID)
		}
	}

	// 4. Summary.
	if jsonOutput {
		printJSON(map[string]any{
			"total_slack_users":  len(allUsers),
			"created":           created,
			"skipped_existing":  skippedExisting,
			"skipped_bot":       skippedBot,
			"skipped_deleted":   skippedDeleted,
			"dry_run":           seedDryRun,
			"existing_user_beads": len(existingBySlack),
		})
	} else {
		action := "Created"
		if seedDryRun {
			action = "Would create"
		}
		var parts []string
		parts = append(parts, fmt.Sprintf("%s %d user bead(s)", action, created))
		if skippedExisting > 0 {
			parts = append(parts, fmt.Sprintf("skipped %d existing", skippedExisting))
		}
		if skippedBot > 0 {
			parts = append(parts, fmt.Sprintf("skipped %d bots", skippedBot))
		}
		if skippedDeleted > 0 {
			parts = append(parts, fmt.Sprintf("skipped %d deleted", skippedDeleted))
		}
		fmt.Printf("\n%s (from %d Slack members)\n", strings.Join(parts, ", "), len(allUsers))
	}

	return nil
}
