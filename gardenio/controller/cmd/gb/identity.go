package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
)

var identityCmd = &cobra.Command{
	Use:   "identity",
	Short: "Manage user identity mappings (Slack ↔ GitLab ↔ GitHub ↔ email)",
	Long: `User identity beads map cross-platform identities so the system can
automatically resolve between Slack user IDs, GitLab usernames, GitHub
usernames, and email addresses.

Used by gb dm review to auto-resolve --reviewer by GitLab username,
and by the bridge for task notification routing.`,
	GroupID: "orchestration",
}

// ── identity add ────────────────────────────────────────────────────

var identityAddCmd = &cobra.Command{
	Use:   "add <display-name>",
	Short: "Register a user identity mapping",
	Long: `Create a user identity bead linking a person's accounts across platforms.

At least one of --slack-id, --gitlab, --github, or --email is required.

Requires an OIDC gardenio-admin. Identity fields (email, slack_id, gitlab/
github/jira usernames, ...) decide which operator an identity resolves to, so
the daemon answers 403 to the shared static token and to fleet tokens — which
is what an agent pod holds. See gardenio/docs/security/user-identity-fields.md.

Examples:
  gb identity add "Alice Smith" --slack-id U0123ABC --gitlab alice-gl --email alice@example.com
  gb identity add "Bob" --gitlab bob-dev --github bob-gh --team platform --jira bob.jones
  gb identity add "Dana" --slack-id U999XYZ --pronouns "she/her"`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		displayName := args[0]

		slackID, _ := cmd.Flags().GetString("slack-id")
		slackChannelID, _ := cmd.Flags().GetString("slack-channel-id")
		gitlab, _ := cmd.Flags().GetString("gitlab")
		github, _ := cmd.Flags().GetString("github")
		email, _ := cmd.Flags().GetString("email")
		team, _ := cmd.Flags().GetString("team")
		jiraUser, _ := cmd.Flags().GetString("jira")
		pronouns, _ := cmd.Flags().GetString("pronouns")

		if slackID == "" && gitlab == "" && github == "" && email == "" {
			return fmt.Errorf("at least one of --slack-id, --gitlab, --github, or --email is required")
		}

		if err := validateSlackUserID("--slack-id", slackID); err != nil {
			return err
		}

		fields := map[string]string{
			"display_name": displayName,
		}
		if slackID != "" {
			fields["slack_id"] = slackID
		}
		if slackChannelID != "" {
			fields["slack_channel_id"] = slackChannelID
		}
		if gitlab != "" {
			fields["gitlab_username"] = gitlab
		}
		if github != "" {
			fields["github_username"] = github
		}
		if email != "" {
			fields["email"] = email
		}
		if team != "" {
			fields["team"] = team
		}
		if jiraUser != "" {
			fields["jira_user"] = jiraUser
		}
		if pronouns != "" {
			fields["pronouns"] = pronouns
		}

		fieldsJSON, err := json.Marshal(fields)
		if err != nil {
			return fmt.Errorf("encoding fields: %w", err)
		}

		id, err := daemon.CreateBead(cmd.Context(), beadsapi.CreateBeadRequest{
			Title:     displayName,
			Type:      "user",
			Kind:      "config",
			Fields:    json.RawMessage(fieldsJSON),
			CreatedBy: actor,
		})
		if err != nil {
			return fmt.Errorf("creating identity: %w", err)
		}

		if jsonOutput {
			printJSON(map[string]string{"id": id})
		} else {
			fmt.Printf("Created identity %s: %s\n", id, displayName)
		}
		return nil
	},
}

// ── identity list ────────────────────────────────────────────────────

var identityListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all user identity mappings",
	RunE: func(cmd *cobra.Command, args []string) error {
		result, err := daemon.ListBeadsFiltered(cmd.Context(), beadsapi.ListBeadsQuery{
			Types:    []string{"user"},
			Kinds:    []string{"config"},
			Statuses: []string{"open", "in_progress"},
			Limit:    500,
		})
		if err != nil {
			return fmt.Errorf("listing identities: %w", err)
		}

		search, _ := cmd.Flags().GetString("search")

		if jsonOutput {
			if search != "" {
				filtered := filterIdentities(result.Beads, search)
				printJSON(filtered)
			} else {
				printJSON(result.Beads)
			}
			return nil
		}

		beads := result.Beads
		if search != "" {
			beads = filterIdentities(beads, search)
		}

		if len(beads) == 0 {
			fmt.Println("No identity mappings found.")
			return nil
		}

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tNAME\tSLACK\tGITLAB\tGITHUB\tEMAIL\tTEAM")
		for _, b := range beads {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				b.ID,
				b.Title,
				b.Fields["slack_id"],
				b.Fields["gitlab_username"],
				b.Fields["github_username"],
				b.Fields["email"],
				b.Fields["team"],
			)
		}
		w.Flush()
		fmt.Printf("\n%d identity mapping(s)\n", len(beads))
		return nil
	},
}

// ── identity show ────────────────────────────────────────────────────

var identityShowCmd = &cobra.Command{
	Use:   "show <id-or-username>",
	Short: "Show details of a user identity",
	Long: `Show a user identity by bead ID (gd-...) or by GitLab/GitHub username.
When a non-ID argument is given, the command searches by GitLab username first,
then GitHub username.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		query := args[0]

		var bead *beadsapi.BeadDetail

		// If it looks like a bead ID, fetch directly.
		if (strings.HasPrefix(query, "gd-") || strings.HasPrefix(query, "ka-")) {
			var err error
			bead, err = daemon.GetBead(cmd.Context(), query)
			if err != nil {
				return fmt.Errorf("getting identity %s: %w", query, err)
			}
		} else {
			// Search by username across all user beads.
			result, err := daemon.ListBeadsFiltered(cmd.Context(), beadsapi.ListBeadsQuery{
				Types:    []string{"user"},
				Kinds:    []string{"config"},
				Statuses: []string{"open", "in_progress"},
				Limit:    500,
			})
			if err != nil {
				return fmt.Errorf("searching identities: %w", err)
			}
			lower := strings.ToLower(query)
			for _, b := range result.Beads {
				if strings.EqualFold(b.Fields["gitlab_username"], lower) ||
					strings.EqualFold(b.Fields["github_username"], lower) ||
					strings.EqualFold(b.Fields["slack_id"], query) ||
					strings.EqualFold(b.Fields["slack_channel_id"], query) ||
					strings.EqualFold(b.Fields["email"], lower) ||
					strings.EqualFold(b.Fields["jira_user"], lower) {
					bead = b
					break
				}
			}
			if bead == nil {
				return fmt.Errorf("no identity found matching %q", query)
			}
		}

		if jsonOutput {
			printJSON(bead)
			return nil
		}

		fmt.Printf("ID:       %s\n", bead.ID)
		fmt.Printf("Name:     %s\n", bead.Title)
		if v := bead.Fields["slack_id"]; v != "" {
			fmt.Printf("Slack:    %s\n", v)
		}
		if v := bead.Fields["slack_channel_id"]; v != "" {
			fmt.Printf("Slack DM: %s\n", v)
		}
		if v := bead.Fields["gitlab_username"]; v != "" {
			fmt.Printf("GitLab:   %s\n", v)
		}
		if v := bead.Fields["github_username"]; v != "" {
			fmt.Printf("GitHub:   %s\n", v)
		}
		if v := bead.Fields["email"]; v != "" {
			fmt.Printf("Email:    %s\n", v)
		}
		if v := bead.Fields["jira_user"]; v != "" {
			fmt.Printf("JIRA:     %s\n", v)
		}
		if v := bead.Fields["pronouns"]; v != "" {
			fmt.Printf("Pronouns: %s\n", v)
		}
		if v := bead.Fields["team"]; v != "" {
			fmt.Printf("Team:     %s\n", v)
		}
		fmt.Printf("Status:   %s\n", bead.Status)
		return nil
	},
}

// ── identity remove ──────────────────────────────────────────────────

var identityRemoveCmd = &cobra.Command{
	Use:   "remove <id>",
	Short: "Remove a user identity mapping",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id := args[0]
		hard, _ := cmd.Flags().GetBool("hard")

		if hard {
			if err := daemon.DeleteBead(cmd.Context(), id); err != nil {
				return fmt.Errorf("deleting identity %s: %w", id, err)
			}
			fmt.Printf("Deleted identity %s\n", id)
		} else {
			if err := daemon.CloseBead(cmd.Context(), id, nil); err != nil {
				return fmt.Errorf("closing identity %s: %w", id, err)
			}
			fmt.Printf("Closed identity %s\n", id)
		}
		return nil
	},
}

// ── helpers ──────────────────────────────────────────────────────────

func filterIdentities(beads []*beadsapi.BeadDetail, search string) []*beadsapi.BeadDetail {
	lower := strings.ToLower(search)
	var out []*beadsapi.BeadDetail
	for _, b := range beads {
		if strings.Contains(strings.ToLower(b.Title), lower) ||
			strings.Contains(strings.ToLower(b.Fields["gitlab_username"]), lower) ||
			strings.Contains(strings.ToLower(b.Fields["github_username"]), lower) ||
			strings.Contains(strings.ToLower(b.Fields["email"]), lower) ||
			strings.Contains(strings.ToLower(b.Fields["slack_id"]), lower) ||
			strings.Contains(strings.ToLower(b.Fields["slack_channel_id"]), lower) ||
			strings.Contains(strings.ToLower(b.Fields["jira_user"]), lower) {
			out = append(out, b)
		}
	}
	return out
}

// ── identity resolve ─────────────────────────────────────────────────

var identityResolveCmd = &cobra.Command{
	Use:   "resolve <value>",
	Short: "Resolve any identity field to a user bead",
	Long: `Resolve a Slack user ID, email, GitLab/GitHub username, JIRA user,
or Slack channel ID to the matching user identity bead.

Searches all identity fields and returns the first match. Useful for
programmatic lookups in bridge event handlers and automation scripts.

Examples:
  gb identity resolve U0123ABC          # by Slack user ID
  gb identity resolve alice@example.com # by email
  gb identity resolve alice-gl          # by GitLab username
  gb identity resolve grace.hopper      # by JIRA user
  gb identity resolve D0AG8CC037H       # by Slack DM channel ID`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		query := args[0]
		field, _ := cmd.Flags().GetString("field")

		result, err := daemon.ListBeadsFiltered(cmd.Context(), beadsapi.ListBeadsQuery{
			Types:    []string{"user"},
			Kinds:    []string{"config"},
			Statuses: []string{"open", "in_progress"},
			Limit:    500,
		})
		if err != nil {
			return fmt.Errorf("searching identities: %w", err)
		}

		var bead *beadsapi.BeadDetail
		lower := strings.ToLower(query)

		for _, b := range result.Beads {
			if field != "" {
				// Search only the specified field.
				if strings.EqualFold(b.Fields[field], lower) {
					bead = b
					break
				}
				continue
			}
			// Search all identity fields.
			if strings.EqualFold(b.Fields["slack_id"], query) ||
				strings.EqualFold(b.Fields["slack_channel_id"], query) ||
				strings.EqualFold(b.Fields["gitlab_username"], lower) ||
				strings.EqualFold(b.Fields["github_username"], lower) ||
				strings.EqualFold(b.Fields["email"], lower) ||
				strings.EqualFold(b.Fields["jira_user"], lower) {
				bead = b
				break
			}
		}

		if bead == nil {
			if jsonOutput {
				printJSON(map[string]any{"found": false, "query": query})
				return nil
			}
			return fmt.Errorf("no identity found matching %q", query)
		}

		if jsonOutput {
			printJSON(map[string]any{
				"found":            true,
				"id":               bead.ID,
				"display_name":     bead.Title,
				"slack_id":         bead.Fields["slack_id"],
				"slack_channel_id": bead.Fields["slack_channel_id"],
				"gitlab_username":  bead.Fields["gitlab_username"],
				"github_username":  bead.Fields["github_username"],
				"email":            bead.Fields["email"],
				"jira_user":        bead.Fields["jira_user"],
				"pronouns":         bead.Fields["pronouns"],
				"team":             bead.Fields["team"],
			})
			return nil
		}

		fmt.Printf("%s  %s\n", bead.ID, bead.Title)
		return nil
	},
}

func init() {
	identityCmd.AddCommand(identityAddCmd)
	identityCmd.AddCommand(identityListCmd)
	identityCmd.AddCommand(identityShowCmd)
	identityCmd.AddCommand(identityRemoveCmd)
	identityCmd.AddCommand(identityResolveCmd)
	identityCmd.AddCommand(identitySeedCmd)

	identityAddCmd.Flags().String("slack-id", "", "Slack user ID (e.g. U0123ABC)")
	identityAddCmd.Flags().String("slack-channel-id", "", "Slack DM channel ID (e.g. D0AG8CC037H)")
	identityAddCmd.Flags().String("gitlab", "", "GitLab username")
	identityAddCmd.Flags().String("github", "", "GitHub username")
	identityAddCmd.Flags().String("email", "", "email address")
	identityAddCmd.Flags().String("team", "", "team name")
	identityAddCmd.Flags().String("jira", "", "JIRA account ID or username")
	identityAddCmd.Flags().String("pronouns", "", "pronouns (e.g. he/him, she/her, they/them)")

	identityListCmd.Flags().String("search", "", "filter by name, username, or email")

	identityRemoveCmd.Flags().Bool("hard", false, "permanently delete instead of closing")

	identityResolveCmd.Flags().String("field", "", "search only this specific field (e.g. slack_id, jira_user)")
}
