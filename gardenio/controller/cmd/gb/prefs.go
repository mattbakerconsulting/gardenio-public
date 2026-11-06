package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
)

var prefsCmd = &cobra.Command{
	Use:   "prefs",
	Short: "Manage per-user preferences stored on user beads",
	Long: `Read and write user preferences stored in the preferences JSON field
on user identity beads. Preferences are key-value pairs (e.g. timezone,
default_project, notification_style).

The target user is resolved from --user (Slack ID or username) or the
SLACK_USER_ID environment variable.

Known keys:
  vibe_mode   Boolean (true/false). false or absent (the default) gives the
              simpler surface: minimal beads-viewer navigation and terse Slack
              notifications. true opts into the full experience: the complete
              beads-viewer nav and verbose Slack updates.
  dm_project  Project name. The project this user's DMs with the bot spawn
              agents into when the message carries no project:<name> /
              --project override. Outranks the bridge's DEFAULT_DM_PROJECT
              and any project bead's dm_default. Must name an existing
              project bead or the bridge ignores it (warn-logged).
              Example: gb prefs set --user U0C8VAG1V9T dm_project claimlayer`,
	GroupID: "orchestration",
}

// resolveUserBead finds the user bead for a given identifier (Slack ID, email,
// GitLab username, etc.) by searching all user beads.
func resolveUserBead(cmd *cobra.Command, userFlag string) (*beadsapi.BeadDetail, error) {
	user := userFlag
	if user == "" {
		user = os.Getenv("SLACK_USER_ID")
	}
	if user == "" {
		return nil, fmt.Errorf("no user specified: use --user or set SLACK_USER_ID")
	}

	result, err := daemon.ListBeadsFiltered(cmd.Context(), beadsapi.ListBeadsQuery{
		Types:    []string{"user"},
		Kinds:    []string{"config"},
		Statuses: []string{"open", "in_progress"},
		Limit:    500,
	})
	if err != nil {
		return nil, fmt.Errorf("listing user beads: %w", err)
	}

	for _, b := range result.Beads {
		if b.Fields["slack_id"] == user ||
			b.Fields["email"] == user ||
			strings.EqualFold(b.Fields["gitlab_username"], user) ||
			strings.EqualFold(b.Fields["github_username"], user) ||
			b.Fields["jira_user"] == user ||
			b.ID == user {
			return b, nil
		}
	}
	return nil, fmt.Errorf("no user bead found for %q", user)
}

// getPreferencesMap extracts the preferences JSON map from a user bead.
func getPreferencesMap(bead *beadsapi.BeadDetail) (map[string]string, error) {
	raw := bead.Fields["preferences"]
	if raw == "" || raw == "null" {
		return map[string]string{}, nil
	}
	var prefs map[string]string
	if err := json.Unmarshal([]byte(raw), &prefs); err != nil {
		return nil, fmt.Errorf("parsing preferences: %w", err)
	}
	return prefs, nil
}

// ── prefs get ──────────────────────────────────────────────────────

var prefsGetCmd = &cobra.Command{
	Use:   "get <key>",
	Short: "Get a user preference value",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		userFlag, _ := cmd.Flags().GetString("user")
		bead, err := resolveUserBead(cmd, userFlag)
		if err != nil {
			return err
		}

		prefs, err := getPreferencesMap(bead)
		if err != nil {
			return err
		}

		key := args[0]
		val, ok := prefs[key]
		if !ok {
			if jsonOutput {
				printJSON(map[string]any{"key": key, "found": false})
				return nil
			}
			return fmt.Errorf("preference %q not set", key)
		}

		if jsonOutput {
			printJSON(map[string]any{"key": key, "value": val, "found": true})
		} else {
			fmt.Println(val)
		}
		return nil
	},
}

// ── typed preference validators ────────────────────────────────────

// boolPrefKeys are preference keys that must hold a boolean. gb prefs stores
// every value as a string (the whole `preferences` blob round-trips through
// map[string]string), so the canonical stored form is the STRING "true" /
// "false" — the shape beads-viewer's flexBool and the map[string]string
// readers in gb prime / quick_spawn all agree on. Keys not listed here stay on
// the untyped generic path.
var boolPrefKeys = map[string]bool{
	// vibe_mode gates the full ("vibe") beads-viewer + Slack experience; absent
	// or false ⇒ the simpler default surface (gb-ux epic ka-0000005059).
	"vibe_mode": true,
}

// validatePrefValue normalizes and validates a preference value for known typed
// keys. For boolean keys it accepts any spelling strconv.ParseBool understands
// (true/false/1/0/t/f/…) — the same grammar beads-viewer's flexBool parses —
// and returns the canonical string form "true"/"false". Unknown keys pass
// through unchanged. Rejecting here (rather than storing e.g. "yes") avoids a
// silent footgun: the viewer coerces unparseable strings to false, so a typo'd
// value would quietly read as off with no error.
func validatePrefValue(key, value string) (string, error) {
	if boolPrefKeys[key] {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return "", fmt.Errorf("preference %q must be a boolean (true or false), got %q", key, value)
		}
		return strconv.FormatBool(parsed), nil
	}
	return value, nil
}

// ── prefs set ──────────────────────────────────────────────────────

var prefsSetCmd = &cobra.Command{
	Use:   "set <key> <value>",
	Short: "Set a user preference",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		key, value := args[0], args[1]
		value, err := validatePrefValue(key, value)
		if err != nil {
			return err
		}

		userFlag, _ := cmd.Flags().GetString("user")
		bead, err := resolveUserBead(cmd, userFlag)
		if err != nil {
			return err
		}

		prefs, err := getPreferencesMap(bead)
		if err != nil {
			return err
		}

		prefs[key] = value

		prefsJSON, err := json.Marshal(prefs)
		if err != nil {
			return fmt.Errorf("marshalling preferences: %w", err)
		}

		if err := daemon.UpdateBeadFieldsTyped(cmd.Context(), bead.ID, map[string]any{
			"preferences": json.RawMessage(prefsJSON),
		}); err != nil {
			return fmt.Errorf("updating preferences: %w", err)
		}

		if jsonOutput {
			printJSON(map[string]any{"key": key, "value": value, "user": bead.ID})
		} else {
			fmt.Printf("Set %s=%s on %s (%s)\n", key, value, bead.Title, bead.ID)
		}
		return nil
	},
}

// ── prefs list ─────────────────────────────────────────────────────

var prefsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all preferences for a user",
	RunE: func(cmd *cobra.Command, args []string) error {
		userFlag, _ := cmd.Flags().GetString("user")
		bead, err := resolveUserBead(cmd, userFlag)
		if err != nil {
			return err
		}

		prefs, err := getPreferencesMap(bead)
		if err != nil {
			return err
		}

		if jsonOutput {
			printJSON(map[string]any{"user": bead.ID, "display_name": bead.Title, "preferences": prefs})
			return nil
		}

		if len(prefs) == 0 {
			fmt.Printf("No preferences set for %s (%s)\n", bead.Title, bead.ID)
			return nil
		}

		keys := make([]string, 0, len(prefs))
		for k := range prefs {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintf(tw, "KEY\tVALUE\n")
		for _, k := range keys {
			fmt.Fprintf(tw, "%s\t%s\n", k, prefs[k])
		}
		tw.Flush()
		fmt.Printf("\n%d preference(s) for %s\n", len(prefs), bead.Title)
		return nil
	},
}

// ── prefs delete ───────────────────────────────────────────────────

var prefsDeleteCmd = &cobra.Command{
	Use:   "delete <key>",
	Short: "Delete a user preference",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		userFlag, _ := cmd.Flags().GetString("user")
		bead, err := resolveUserBead(cmd, userFlag)
		if err != nil {
			return err
		}

		prefs, err := getPreferencesMap(bead)
		if err != nil {
			return err
		}

		key := args[0]
		if _, ok := prefs[key]; !ok {
			return fmt.Errorf("preference %q not set", key)
		}
		delete(prefs, key)

		prefsJSON, err := json.Marshal(prefs)
		if err != nil {
			return fmt.Errorf("marshalling preferences: %w", err)
		}

		if err := daemon.UpdateBeadFieldsTyped(cmd.Context(), bead.ID, map[string]any{
			"preferences": json.RawMessage(prefsJSON),
		}); err != nil {
			return fmt.Errorf("updating preferences: %w", err)
		}

		if jsonOutput {
			printJSON(map[string]any{"key": key, "deleted": true, "user": bead.ID})
		} else {
			fmt.Printf("Deleted %s from %s (%s)\n", key, bead.Title, bead.ID)
		}
		return nil
	},
}

func init() {
	prefsCmd.PersistentFlags().String("user", "", "user identifier (Slack ID, email, username, or bead ID)")

	prefsCmd.AddCommand(prefsGetCmd)
	prefsCmd.AddCommand(prefsSetCmd)
	prefsCmd.AddCommand(prefsListCmd)
	prefsCmd.AddCommand(prefsDeleteCmd)
}
