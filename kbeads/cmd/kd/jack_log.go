package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/model"
	"github.com/spf13/cobra"
)

var jackLogCmd = &cobra.Command{
	Use:   "log <jack-id>",
	Short: "Record a change made under a jack",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id := args[0]
		action, _ := cmd.Flags().GetString("action")
		target, _ := cmd.Flags().GetString("target")
		before, _ := cmd.Flags().GetString("before")
		after, _ := cmd.Flags().GetString("after")
		cmdStr, _ := cmd.Flags().GetString("cmd")

		if action == "" {
			return fmt.Errorf("--action is required")
		}
		if !model.IsValidJackAction(action) {
			return fmt.Errorf("invalid action %q (must be one of %v)", action, model.ValidJackActions)
		}

		// Truncate oversized fields.
		if len(before) > model.JackMaxChangeFieldSize {
			before = before[:model.JackMaxChangeFieldSize]
		}
		if len(after) > model.JackMaxChangeFieldSize {
			after = after[:model.JackMaxChangeFieldSize]
		}

		// Fetch current jack.
		bead, err := beadsClient.GetBead(context.Background(), id)
		if err != nil {
			return fmt.Errorf("getting jack %s: %w", id, err)
		}
		if string(bead.Type) != "jack" {
			return fmt.Errorf("%s is not a jack", id)
		}
		if string(bead.Status) == "closed" {
			return fmt.Errorf("jack %s is already closed", id)
		}

		var fields map[string]any
		if len(bead.Fields) > 0 {
			_ = json.Unmarshal(bead.Fields, &fields)
		}
		if fields == nil {
			fields = make(map[string]any)
		}

		// Get existing changes.
		var changes []any
		if v, ok := fields["jack_changes"]; ok {
			if arr, ok := v.([]any); ok {
				changes = arr
			}
		}

		if len(changes) >= model.JackMaxChanges {
			return fmt.Errorf("jack has reached maximum %d change records", model.JackMaxChanges)
		}

		// Default target to jack's target.
		if target == "" {
			if jt, ok := fields["jack_target"].(string); ok {
				target = jt
			}
		}

		change := model.JackChange{
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Action:    action,
			Target:    target,
			Before:    before,
			After:     after,
			Cmd:       cmdStr,
			Agent:     actor,
		}

		changes = append(changes, change)
		fields["jack_changes"] = changes

		fieldsJSON, _ := json.Marshal(fields)

		_, err = beadsClient.UpdateBead(context.Background(), id, &client.UpdateBeadRequest{
			Fields: fieldsJSON,
		})
		if err != nil {
			return fmt.Errorf("updating jack %s: %w", id, err)
		}

		if jsonOutput {
			data, _ := json.MarshalIndent(change, "", "  ")
			fmt.Println(string(data))
		} else {
			fmt.Printf("Logged change #%d on %s\n", len(changes), id)
			fmt.Printf("  Action: %s\n", action)
			if target != "" {
				fmt.Printf("  Target: %s\n", target)
			}
			if len(changes) >= model.JackMaxChanges*80/100 {
				fmt.Fprintf(os.Stderr, "Warning: %d/%d change records used\n", len(changes), model.JackMaxChanges)
			}
		}
		return nil
	},
}

func init() {
	jackLogCmd.Flags().String("action", "", "change action: edit|exec|patch|delete|create (required)")
	jackLogCmd.Flags().String("target", "", "specific resource affected (defaults to jack target)")
	jackLogCmd.Flags().String("before", "", "state before change")
	jackLogCmd.Flags().String("after", "", "state after change")
	jackLogCmd.Flags().String("cmd", "", "command executed")
}
