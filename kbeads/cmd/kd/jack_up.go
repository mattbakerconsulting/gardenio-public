package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/model"
	"github.com/spf13/cobra"
)

var jackUpCmd = &cobra.Command{
	Use:   "up <target>",
	Short: "Create a new jack (infrastructure change permit)",
	Long: `Create a jack BEFORE making infrastructure changes outside CI/CD.

Examples:
  kd jack up pod/my-app --reason="Debug logging" --revert-plan="Restore config" --ttl=30m
  kd jack up deployment/api --reason="Emergency failover" --revert-plan="Remove manual label" --priority=0 --labels=jack:failover`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		target := args[0]

		reason, _ := cmd.Flags().GetString("reason")
		revertPlan, _ := cmd.Flags().GetString("revert-plan")
		ttlStr, _ := cmd.Flags().GetString("ttl")
		blocks, _ := cmd.Flags().GetString("blocks")
		labels, _ := cmd.Flags().GetStringSlice("labels")
		priority, _ := cmd.Flags().GetInt("priority")

		if reason == "" {
			return fmt.Errorf("--reason is required")
		}
		if revertPlan == "" {
			return fmt.Errorf("--revert-plan is required")
		}

		ttl, err := time.ParseDuration(ttlStr)
		if err != nil {
			return fmt.Errorf("invalid TTL %q: %w", ttlStr, err)
		}
		if ttl > model.JackMaxTTL {
			return fmt.Errorf("TTL %v exceeds maximum %v", ttl, model.JackMaxTTL)
		}

		// Ensure at least one jack: label.
		hasJackLabel := false
		for _, l := range labels {
			if strings.HasPrefix(l, model.JackLabelPrefix) {
				hasJackLabel = true
				break
			}
		}
		if !hasJackLabel {
			labels = append(labels, model.LabelJackGeneral)
		}

		now := time.Now().UTC()
		expiresAt := now.Add(ttl)

		fields := map[string]any{
			"jack_target":          target,
			"jack_reason":          reason,
			"jack_revert_plan":     revertPlan,
			"jack_ttl":             ttlStr,
			"jack_expires_at":      expiresAt.Format(time.RFC3339),
			"jack_extension_count": 0,
			"jack_cumulative_ttl":  ttlStr,
			"jack_reverted":        false,
			"jack_escalated":       false,
			"jack_changes":         []any{},
		}

		fieldsJSON, err := json.Marshal(fields)
		if err != nil {
			return fmt.Errorf("encoding fields: %w", err)
		}

		title := fmt.Sprintf("Jack: %s", target)
		inProgress := "in_progress"

		req := &client.CreateBeadRequest{
			Title:       title,
			Description: reason,
			Type:        "jack",
			Priority:    priority,
			Labels:      labels,
			CreatedBy:   actor,
			Fields:      fieldsJSON,
		}

		bead, err := beadsClient.CreateBead(context.Background(), req)
		if err != nil {
			return fmt.Errorf("creating jack: %w", err)
		}

		// Set status to in_progress (jacks start active).
		_, err = beadsClient.UpdateBead(context.Background(), bead.ID, &client.UpdateBeadRequest{
			Status: &inProgress,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: created jack but failed to set in_progress: %v\n", err)
		}

		// Add dependency if --blocks specified.
		if blocks != "" {
			_, err = beadsClient.AddDependency(context.Background(), &client.AddDependencyRequest{
				BeadID:      blocks,
				DependsOnID: bead.ID,
				Type:        "blocks",
				CreatedBy:   actor,
			})
			if err != nil {
				fmt.Fprintf(os.Stderr, "Warning: jack created but failed to add blocks dependency: %v\n", err)
			}
		}

		if jsonOutput {
			printBeadJSON(bead)
		} else {
			fmt.Printf("Jack activated: %s\n", bead.ID)
			fmt.Printf("  Target:  %s\n", target)
			fmt.Printf("  TTL:     %s (expires %s)\n", ttlStr, expiresAt.Format("15:04:05"))
			fmt.Printf("  Revert:  %s\n", revertPlan)
		}
		return nil
	},
}

func init() {
	jackUpCmd.Flags().StringP("reason", "r", "", "why this change is needed (required)")
	jackUpCmd.Flags().String("revert-plan", "", "how to undo the change (required)")
	jackUpCmd.Flags().String("ttl", "1h", "time-to-live duration")
	jackUpCmd.Flags().String("blocks", "", "bead ID this jack blocks")
	jackUpCmd.Flags().StringSlice("labels", nil, "labels (repeatable)")
	jackUpCmd.Flags().IntP("priority", "p", 2, "priority (0-4)")
}
