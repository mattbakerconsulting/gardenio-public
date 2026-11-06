package main

import (
	"context"
	"encoding/json"
	"fmt"

	"gardenio/kbeads/internal/client"
	"github.com/spf13/cobra"
)

var adviceAddCmd = &cobra.Command{
	Use:   "add <text>",
	Short: "Create an advice bead",
	Long: `Create an advice bead with optional targeting labels and hook commands.

If no targeting flags are provided, the advice defaults to the "global" label.

Examples:
  kd advice add "Always run lint before committing"
  kd advice add "Use bun instead of npm" --project=beads --priority=1
  kd advice add "Check test coverage" --hook-command="make coverage" --hook-trigger=before-commit`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		text := args[0]

		title, _ := cmd.Flags().GetString("title")
		description, _ := cmd.Flags().GetString("description")
		priority, _ := cmd.Flags().GetInt("priority")
		labels, _ := cmd.Flags().GetStringSlice("label")

		// Targeting shorthands → labels.
		project, _ := cmd.Flags().GetString("project")
		rig, _ := cmd.Flags().GetString("rig")
		role, _ := cmd.Flags().GetString("role")
		agent, _ := cmd.Flags().GetString("agent")
		if rig != "" {
			return fmt.Errorf("--rig has been removed; use --project=%s instead", rig)
		}
		if project != "" {
			labels = append(labels, "project:"+project)
		}
		if role != "" {
			labels = append(labels, "role:"+role)
		}
		if agent != "" {
			labels = append(labels, "agent:"+agent)
		}

		// Default to global if no targeting labels.
		if !hasTargetingLabel(labels) {
			labels = append(labels, "global")
		}

		// Build fields JSON for advice-specific data.
		fields := make(map[string]any)

		hookCmd, _ := cmd.Flags().GetString("hook-command")
		hookTrigger, _ := cmd.Flags().GetString("hook-trigger")
		hookTimeout, _ := cmd.Flags().GetInt("hook-timeout")
		hookOnFailure, _ := cmd.Flags().GetString("hook-on-failure")

		if hookCmd != "" {
			fields["hook_command"] = hookCmd
		}
		if hookTrigger != "" {
			fields["hook_trigger"] = hookTrigger
		}
		if hookTimeout != 0 {
			fields["hook_timeout"] = hookTimeout
		}
		if hookOnFailure != "" {
			fields["hook_on_failure"] = hookOnFailure
		}

		subs, _ := cmd.Flags().GetStringSlice("subscribe")
		if len(subs) > 0 {
			fields["subscriptions"] = subs
		}
		subsExclude, _ := cmd.Flags().GetStringSlice("exclude")
		if len(subsExclude) > 0 {
			fields["subscriptions_exclude"] = subsExclude
		}

		var fieldsJSON json.RawMessage
		if len(fields) > 0 {
			b, err := json.Marshal(fields)
			if err != nil {
				return fmt.Errorf("encoding fields: %w", err)
			}
			fieldsJSON = b
		}

		if title == "" {
			title = text
		}
		if description == "" {
			description = text
		}

		req := &client.CreateBeadRequest{
			Title:       title,
			Description: description,
			Type:        "advice",
			Priority:    priority,
			Labels:      labels,
			CreatedBy:   actor,
			Fields:      fieldsJSON,
		}

		bead, err := beadsClient.CreateBead(context.Background(), req)
		if err != nil {
			return fmt.Errorf("creating advice: %w", err)
		}

		if jsonOutput {
			printBeadJSON(bead)
		} else {
			fmt.Printf("Created advice %s: %s\n", bead.ID, bead.Title)
			if len(bead.Labels) > 0 {
				fmt.Printf("  Labels: %v\n", bead.Labels)
			}
		}
		return nil
	},
}

// hasTargetingLabel returns true if the label list contains at least one
// targeting label (global, project:*, role:*, agent:*).
func hasTargetingLabel(labels []string) bool {
	for _, l := range labels {
		switch {
		case l == "global":
			return true
		case len(l) > 8 && l[:8] == "project:":
			return true
		case len(l) > 5 && l[:5] == "role:":
			return true
		case len(l) > 6 && l[:6] == "agent:":
			return true
		}
	}
	return false
}

func init() {
	adviceAddCmd.Flags().StringP("title", "t", "", "override title (defaults to advice text)")
	adviceAddCmd.Flags().StringP("description", "d", "", "detailed description")
	adviceAddCmd.Flags().IntP("priority", "p", 2, "priority (0-4)")
	adviceAddCmd.Flags().StringSliceP("label", "l", nil, "labels (repeatable)")
	adviceAddCmd.Flags().String("project", "", "shorthand for --label project:<value>")
	adviceAddCmd.Flags().String("rig", "", "deprecated: use --project instead")
	adviceAddCmd.Flags().Lookup("rig").Hidden = true
	adviceAddCmd.Flags().String("role", "", "shorthand for --label role:<value>")
	adviceAddCmd.Flags().String("agent", "", "shorthand for --label agent:<value>")
	adviceAddCmd.Flags().String("hook-command", "", "shell command to execute")
	adviceAddCmd.Flags().String("hook-trigger", "", "when to run: session-end|before-commit|before-push|before-handoff")
	adviceAddCmd.Flags().Int("hook-timeout", 0, "hook timeout in seconds (0-300)")
	adviceAddCmd.Flags().String("hook-on-failure", "", "failure mode: block|warn|ignore")
	adviceAddCmd.Flags().StringSlice("subscribe", nil, "additional subscription labels")
	adviceAddCmd.Flags().StringSlice("exclude", nil, "subscription exclusion labels")
	// Accept --labels as an alias of --label (ka-0000000987).
	aliasLabelFlags(adviceAddCmd)
}
