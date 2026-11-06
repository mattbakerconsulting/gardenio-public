package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
)

var userCmd = &cobra.Command{
	Use:   "user",
	Short: "Manage user beads (identities, preferences, and cross-platform mappings)",
	Long: `User beads store cross-platform identities and per-user preferences.

Each user bead links a person's accounts across Slack, GitLab, GitHub, JIRA,
and email — enabling automatic resolution for DM routing, task assignment,
MR reviewer tagging, and notification delivery.

Subcommands:
  add       Create a new user bead
  list      List all users
  show      Show user details by ID or username
  update    Update fields on an existing user
  remove    Close or delete a user bead`,
	GroupID: "orchestration",
}

// ── user add ────────────────────────────────────────────────────────

var userAddCmd = &cobra.Command{
	Use:   "add <display-name>",
	Short: "Create a new user bead",
	Long: `Create a user bead linking a person's accounts across platforms.

At least one of --slack-id, --gitlab, --github, or --email is required.

Examples:
  gb user add "Alice Smith" --slack-id U0123ABC --gitlab alice-gl --email alice@example.com
  gb user add "Bob" --gitlab bob-dev --github bob-gh --team platform --jira bob.jones`,
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
		gitlabID, _ := cmd.Flags().GetString("gitlab-id")
		githubID, _ := cmd.Flags().GetString("github-id")
		if err := validateForgeUserID("--gitlab-id", gitlabID); err != nil {
			return err
		}
		if err := validateForgeUserID("--github-id", githubID); err != nil {
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
		if gitlabID != "" {
			fields["gitlab_user_id"] = gitlabID
		}
		if githubID != "" {
			fields["github_user_id"] = githubID
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
			return fmt.Errorf("creating user: %w", err)
		}

		if jsonOutput {
			printJSON(map[string]string{"id": id})
		} else {
			fmt.Printf("Created user %s: %s\n", id, displayName)
		}
		return nil
	},
}

// ── user list ───────────────────────────────────────────────────────

var userListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all users",
	RunE: func(cmd *cobra.Command, args []string) error {
		result, err := daemon.ListBeadsFiltered(cmd.Context(), beadsapi.ListBeadsQuery{
			Types:    []string{"user"},
			Kinds:    []string{"config"},
			Statuses: []string{"open", "in_progress"},
			Limit:    500,
		})
		if err != nil {
			return fmt.Errorf("listing users: %w", err)
		}

		search, _ := cmd.Flags().GetString("search")
		team, _ := cmd.Flags().GetString("team")

		beads := result.Beads
		if search != "" {
			beads = filterIdentities(beads, search)
		}
		if team != "" {
			beads = filterByTeam(beads, team)
		}

		if jsonOutput {
			printJSON(beads)
			return nil
		}

		if len(beads) == 0 {
			fmt.Println("No users found.")
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
		fmt.Printf("\n%d user(s)\n", len(beads))
		return nil
	},
}

// ── user show ───────────────────────────────────────────────────────

var userShowCmd = &cobra.Command{
	Use:   "show <id-or-username>",
	Short: "Show user details",
	Long: `Show a user by bead ID (gd-...) or by GitLab/GitHub username, email,
Slack ID, or JIRA username.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		bead, err := findUserBead(cmd, args[0])
		if err != nil {
			return err
		}

		if jsonOutput {
			printJSON(bead)
			return nil
		}

		printUserDetail(bead)
		return nil
	},
}

// ── user update ─────────────────────────────────────────────────────

var userUpdateCmd = &cobra.Command{
	Use:   "update <id-or-username>",
	Short: "Update fields on an existing user",
	Long: `Update one or more fields on a user bead. Only flags that are explicitly
provided will be updated — omitted flags are left unchanged.

Use --clear-<field> to remove a field value.

Examples:
  gb user update alice-gl --team platform --pronouns "she/her"
  gb user update ka-abc123 --email alice@newdomain.com
  gb user update U0123ABC --gitlab alice-new --clear-github`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		bead, err := findUserBead(cmd, args[0])
		if err != nil {
			return err
		}

		fields := make(map[string]string)
		titleUpdate := ""

		// Collect changed flags.
		if cmd.Flags().Changed("name") {
			name, _ := cmd.Flags().GetString("name")
			titleUpdate = name
			fields["display_name"] = name
		}
		if cmd.Flags().Changed("slack-id") {
			v, _ := cmd.Flags().GetString("slack-id")
			if err := validateSlackUserID("--slack-id", v); err != nil {
				return err
			}
			fields["slack_id"] = v
		}
		if cmd.Flags().Changed("slack-channel-id") {
			v, _ := cmd.Flags().GetString("slack-channel-id")
			fields["slack_channel_id"] = v
		}
		if cmd.Flags().Changed("gitlab") {
			v, _ := cmd.Flags().GetString("gitlab")
			fields["gitlab_username"] = v
		}
		if cmd.Flags().Changed("github") {
			v, _ := cmd.Flags().GetString("github")
			fields["github_username"] = v
		}
		for _, f := range []struct{ flag, field string }{
			{"gitlab-id", "gitlab_user_id"},
			{"github-id", "github_user_id"},
		} {
			if cmd.Flags().Changed(f.flag) {
				v, _ := cmd.Flags().GetString(f.flag)
				if err := validateForgeUserID("--"+f.flag, v); err != nil {
					return err
				}
				fields[f.field] = v
			}
		}
		if cmd.Flags().Changed("email") {
			v, _ := cmd.Flags().GetString("email")
			fields["email"] = v
		}
		if cmd.Flags().Changed("team") {
			v, _ := cmd.Flags().GetString("team")
			fields["team"] = v
		}
		if cmd.Flags().Changed("jira") {
			v, _ := cmd.Flags().GetString("jira")
			fields["jira_user"] = v
		}
		if cmd.Flags().Changed("pronouns") {
			v, _ := cmd.Flags().GetString("pronouns")
			fields["pronouns"] = v
		}

		// Handle --clear-* flags (set to empty string to clear).
		clearFlags := []struct {
			flag  string
			field string
		}{
			{"clear-slack-id", "slack_id"},
			{"clear-slack-channel-id", "slack_channel_id"},
			{"clear-gitlab", "gitlab_username"},
			{"clear-github", "github_username"},
			{"clear-gitlab-id", "gitlab_user_id"},
			{"clear-github-id", "github_user_id"},
			{"clear-email", "email"},
			{"clear-team", "team"},
			{"clear-jira", "jira_user"},
			{"clear-pronouns", "pronouns"},
		}
		for _, cf := range clearFlags {
			if v, _ := cmd.Flags().GetBool(cf.flag); v {
				fields[cf.field] = ""
			}
		}

		if len(fields) == 0 && titleUpdate == "" {
			return fmt.Errorf("no fields to update — provide at least one flag")
		}

		// Update title if display name changed.
		if titleUpdate != "" {
			if err := daemon.UpdateBead(cmd.Context(), bead.ID, beadsapi.UpdateBeadRequest{
				Title: &titleUpdate,
			}); err != nil {
				return fmt.Errorf("updating title: %w", err)
			}
		}

		// Update fields.
		if len(fields) > 0 {
			if err := daemon.UpdateBeadFields(cmd.Context(), bead.ID, fields); err != nil {
				return fmt.Errorf("updating user fields: %w", err)
			}
		}

		if jsonOutput {
			// Re-fetch to show updated state.
			updated, err := daemon.GetBead(cmd.Context(), bead.ID)
			if err != nil {
				return fmt.Errorf("fetching updated user: %w", err)
			}
			printJSON(updated)
		} else {
			fmt.Printf("Updated user %s (%s)\n", bead.Title, bead.ID)
		}
		return nil
	},
}

// ── user remove ─────────────────────────────────────────────────────

var userRemoveCmd = &cobra.Command{
	Use:   "remove <id-or-username>",
	Short: "Remove a user bead",
	Long: `Close (soft-delete) or permanently delete a user bead.

By default the bead is closed (status set to closed). Use --hard to
permanently delete it from the database.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		bead, err := findUserBead(cmd, args[0])
		if err != nil {
			return err
		}

		hard, _ := cmd.Flags().GetBool("hard")

		if hard {
			if err := daemon.DeleteBead(cmd.Context(), bead.ID); err != nil {
				return fmt.Errorf("deleting user %s: %w", bead.ID, err)
			}
			if jsonOutput {
				printJSON(map[string]any{"id": bead.ID, "deleted": true})
			} else {
				fmt.Printf("Deleted user %s (%s)\n", bead.Title, bead.ID)
			}
		} else {
			if err := daemon.CloseBead(cmd.Context(), bead.ID, nil); err != nil {
				return fmt.Errorf("closing user %s: %w", bead.ID, err)
			}
			if jsonOutput {
				printJSON(map[string]any{"id": bead.ID, "closed": true})
			} else {
				fmt.Printf("Closed user %s (%s)\n", bead.Title, bead.ID)
			}
		}
		return nil
	},
}

// ── helpers ─────────────────────────────────────────────────────────

// findUserBead resolves a user bead by ID, username, email, or Slack ID.
func findUserBead(cmd *cobra.Command, query string) (*beadsapi.BeadDetail, error) {
	// Direct bead ID lookup.
	if (strings.HasPrefix(query, "gd-") || strings.HasPrefix(query, "ka-")) {
		bead, err := daemon.GetBead(cmd.Context(), query)
		if err != nil {
			return nil, fmt.Errorf("getting user %s: %w", query, err)
		}
		return bead, nil
	}

	// Search by identity field.
	result, err := daemon.ListBeadsFiltered(cmd.Context(), beadsapi.ListBeadsQuery{
		Types:    []string{"user"},
		Kinds:    []string{"config"},
		Statuses: []string{"open", "in_progress"},
		Limit:    500,
	})
	if err != nil {
		return nil, fmt.Errorf("searching users: %w", err)
	}
	lower := strings.ToLower(query)
	for _, b := range result.Beads {
		if strings.EqualFold(b.Fields["gitlab_username"], lower) ||
			strings.EqualFold(b.Fields["github_username"], lower) ||
			strings.EqualFold(b.Fields["slack_id"], query) ||
			strings.EqualFold(b.Fields["slack_channel_id"], query) ||
			strings.EqualFold(b.Fields["email"], lower) ||
			strings.EqualFold(b.Fields["jira_user"], lower) ||
			strings.EqualFold(b.Title, lower) {
			return b, nil
		}
	}
	return nil, fmt.Errorf("no user found matching %q", query)
}

// validateForgeUserID checks a --gitlab-id / --github-id value: the forge's
// numeric user id, which (unlike the username) survives a rename and is never
// reissued (gd-yrnMQbmLgp). Empty is allowed (flag unset).
func validateForgeUserID(flagDesc, value string) error {
	if value == "" {
		return nil
	}
	if n, err := strconv.ParseInt(value, 10, 64); err != nil || n <= 0 {
		return fmt.Errorf("%s %q is not a numeric user id (find it on the profile, or via the forge API /users?username=<name>)", flagDesc, value)
	}
	return nil
}

// filterByTeam filters user beads by team field.
func filterByTeam(beads []*beadsapi.BeadDetail, team string) []*beadsapi.BeadDetail {
	lower := strings.ToLower(team)
	var out []*beadsapi.BeadDetail
	for _, b := range beads {
		if strings.EqualFold(b.Fields["team"], lower) {
			out = append(out, b)
		}
	}
	return out
}

// printUserDetail prints a formatted view of a user bead.
func printUserDetail(bead *beadsapi.BeadDetail) {
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
	if v := bead.Fields["gitlab_user_id"]; v != "" {
		fmt.Printf("GitLab #: %s\n", v)
	}
	if v := bead.Fields["github_user_id"]; v != "" {
		fmt.Printf("GitHub #: %s\n", v)
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
	if v := bead.Fields["capabilities"]; v != "" && v != "null" {
		fmt.Printf("Caps:     %s\n", v)
	}
	if v := bead.Fields["allowed_projects"]; v != "" && v != "null" {
		fmt.Printf("Projects: %s\n", v)
	}
	if v := bead.Fields["allowed_roles"]; v != "" && v != "null" {
		fmt.Printf("Roles:    %s\n", v)
	}
	fmt.Printf("Status:   %s\n", bead.Status)
}

// ── init ────────────────────────────────────────────────────────────

func init() {
	userCmd.AddCommand(userAddCmd)
	userCmd.AddCommand(userListCmd)
	userCmd.AddCommand(userShowCmd)
	userCmd.AddCommand(userUpdateCmd)
	userCmd.AddCommand(userRemoveCmd)

	// user add flags
	userAddCmd.Flags().String("slack-id", "", "Slack user ID (e.g. U0123ABC)")
	userAddCmd.Flags().String("slack-channel-id", "", "Slack DM channel ID (e.g. D0AG8CC037H)")
	userAddCmd.Flags().String("gitlab", "", "GitLab username")
	userAddCmd.Flags().String("github", "", "GitHub username")
	userAddCmd.Flags().String("gitlab-id", "", "GitLab numeric user id (pins the person; outranks the username)")
	userAddCmd.Flags().String("github-id", "", "GitHub numeric user id (pins the person; outranks the username)")
	userAddCmd.Flags().String("email", "", "email address")
	userAddCmd.Flags().String("team", "", "team name")
	userAddCmd.Flags().String("jira", "", "JIRA account ID or username")
	userAddCmd.Flags().String("pronouns", "", "pronouns (e.g. he/him, she/her, they/them)")

	// user list flags
	userListCmd.Flags().String("search", "", "filter by name, username, or email")
	userListCmd.Flags().String("team", "", "filter by team name")

	// user update flags
	userUpdateCmd.Flags().String("name", "", "update display name")
	userUpdateCmd.Flags().String("slack-id", "", "Slack user ID")
	userUpdateCmd.Flags().String("slack-channel-id", "", "Slack DM channel ID")
	userUpdateCmd.Flags().String("gitlab", "", "GitLab username")
	userUpdateCmd.Flags().String("github", "", "GitHub username")
	userUpdateCmd.Flags().String("gitlab-id", "", "GitLab numeric user id (pins the person; outranks the username)")
	userUpdateCmd.Flags().String("github-id", "", "GitHub numeric user id (pins the person; outranks the username)")
	userUpdateCmd.Flags().String("email", "", "email address")
	userUpdateCmd.Flags().String("team", "", "team name")
	userUpdateCmd.Flags().String("jira", "", "JIRA account ID or username")
	userUpdateCmd.Flags().String("pronouns", "", "pronouns")
	userUpdateCmd.Flags().Bool("clear-slack-id", false, "clear Slack user ID")
	userUpdateCmd.Flags().Bool("clear-slack-channel-id", false, "clear Slack DM channel ID")
	userUpdateCmd.Flags().Bool("clear-gitlab", false, "clear GitLab username")
	userUpdateCmd.Flags().Bool("clear-github", false, "clear GitHub username")
	userUpdateCmd.Flags().Bool("clear-gitlab-id", false, "clear GitLab numeric user id")
	userUpdateCmd.Flags().Bool("clear-github-id", false, "clear GitHub numeric user id")
	userUpdateCmd.Flags().Bool("clear-email", false, "clear email address")
	userUpdateCmd.Flags().Bool("clear-team", false, "clear team name")
	userUpdateCmd.Flags().Bool("clear-jira", false, "clear JIRA username")
	userUpdateCmd.Flags().Bool("clear-pronouns", false, "clear pronouns")

	// user remove flags
	userRemoveCmd.Flags().Bool("hard", false, "permanently delete instead of closing")
}
