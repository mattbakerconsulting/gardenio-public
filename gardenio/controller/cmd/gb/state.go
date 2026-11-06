package main

// gb state — Persist session-learned state across context compactions.
//
// Agents lose environment values, CLI patterns, and error resolutions when
// Claude Code compacts context. This command provides key-value storage that
// the PreCompact hook reads and re-injects so the agent retains knowledge.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// stateFile returns the path to the learned-state JSON file.
// Uses $KD_WORKSPACE/.state/learned.json if available, otherwise $HOME/.state/learned.json.
// When KD_WORKSPACE is unset, all beads on the same pod share a single state file.
func stateFile() string {
	base := os.Getenv("KD_WORKSPACE")
	if base == "" {
		base = os.Getenv("HOME")
	}
	return filepath.Join(base, ".state", "learned.json")
}

// readState reads the state file and returns the key-value map.
func readState() (map[string]string, error) {
	data, err := os.ReadFile(stateFile())
	if err != nil {
		if os.IsNotExist(err) {
			return make(map[string]string), nil
		}
		return nil, err
	}
	var state map[string]string
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("parsing state file: %w", err)
	}
	return state, nil
}

// writeState writes the key-value map to the state file atomically
// using a write-to-tmp-then-rename pattern to prevent torn writes.
func writeState(state map[string]string) error {
	path := stateFile()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// sortedKeys returns the keys of a string map in sorted order.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

var stateCmd = &cobra.Command{
	Use:     "state",
	Short:   "Persist session-learned state across context compactions",
	GroupID: "session",
}

var stateSetCmd = &cobra.Command{
	Use:   "set <key> <value>",
	Short: "Save a learned value (persists across compactions)",
	Long: `Save a key-value pair to the session state file.

The PreCompact hook reads this file and re-injects saved state into the
conversation so the agent retains learned knowledge after compaction.

Keys should be descriptive. Values can be any string (keep them short).

Examples:
  gb state set GITLAB_PROJECT_ID 79511414
  gb state set jira-auth "use curl -u $JIRA_EMAIL:$JIRA_API_TOKEN"
  gb state set kubectl-token "use: kubectl create token gardenio-agent-cluster-admin -n gardenio --duration=1h"
  gb state set shell-escaping "passwords with ! need single quotes, not double"`,
	Args: cobra.MinimumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		key := args[0]
		value := strings.Join(args[1:], " ")

		state, err := readState()
		if err != nil {
			return err
		}

		state[key] = value
		if err := writeState(state); err != nil {
			return err
		}

		fmt.Printf("Saved: %s = %s\n", key, value)
		return nil
	},
}

var stateGetCmd = &cobra.Command{
	Use:   "get <key>",
	Short: "Read a saved state value",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		state, err := readState()
		if err != nil {
			return err
		}

		val, ok := state[args[0]]
		if !ok {
			return fmt.Errorf("key %q not found", args[0])
		}

		fmt.Println(val)
		return nil
	},
}

var stateListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all saved state",
	RunE: func(cmd *cobra.Command, args []string) error {
		state, err := readState()
		if err != nil {
			return err
		}

		if len(state) == 0 {
			fmt.Println("No saved state.")
			return nil
		}

		for _, k := range sortedKeys(state) {
			fmt.Printf("%s = %s\n", k, state[k])
		}
		return nil
	},
}

var stateDeleteCmd = &cobra.Command{
	Use:   "delete <key>",
	Short: "Delete a saved state value",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		state, err := readState()
		if err != nil {
			return err
		}

		key := args[0]
		if _, ok := state[key]; !ok {
			return fmt.Errorf("key %q not found", key)
		}

		delete(state, key)
		if err := writeState(state); err != nil {
			return err
		}

		fmt.Printf("Deleted: %s\n", key)
		return nil
	},
}

var stateClearCmd = &cobra.Command{
	Use:   "clear",
	Short: "Clear all saved state",
	RunE: func(cmd *cobra.Command, args []string) error {
		path := stateFile()
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		fmt.Println("State cleared.")
		return nil
	},
}

// formatStateForInjection formats the state map as a human-readable block
// suitable for injection into the conversation context via system-reminder.
func formatStateForInjection(state map[string]string) string {
	if len(state) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("## Learned State\n\n")
	sb.WriteString("These values were saved during this session. Use them without re-discovering.\n\n")
	for _, k := range sortedKeys(state) {
		fmt.Fprintf(&sb, "- **%s**: %s\n", k, state[k])
	}
	return sb.String()
}

func init() {
	stateCmd.AddCommand(stateSetCmd)
	stateCmd.AddCommand(stateGetCmd)
	stateCmd.AddCommand(stateListCmd)
	stateCmd.AddCommand(stateDeleteCmd)
	stateCmd.AddCommand(stateClearCmd)
}
