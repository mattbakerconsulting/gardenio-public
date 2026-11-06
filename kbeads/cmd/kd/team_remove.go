package main

import (
	"context"
	"encoding/json"
	"fmt"

	"gardenio/kbeads/internal/client"
	"github.com/spf13/cobra"
)

var teamRemoveCmd = &cobra.Command{
	Use:   "remove <name>",
	Short: "Remove a member or owner from a team",
	Long: `Remove one or more members/owners from a type:team bead's
members/owners array. A ref matches an existing entry after resolving both
sides through operator_aliases, so a Slack alias stored in the array can be
removed by passing the person's email (and vice versa). Rejected
server-side with a 403 if the caller lacks the required role or ownership.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		ctx := context.Background()

		memberRefs, _ := cmd.Flags().GetStringSlice("member")
		ownerRefs, _ := cmd.Flags().GetStringSlice("owner")
		if len(memberRefs) == 0 && len(ownerRefs) == 0 {
			return fmt.Errorf("specify at least one --member or --owner")
		}

		bead, err := findTeamByName(ctx, name)
		if err != nil {
			return err
		}
		tf, err := parseTeamFields(bead.Fields)
		if err != nil {
			return err
		}

		patch := map[string]any{}
		removed := 0

		if len(memberRefs) > 0 {
			kept := make([]json.RawMessage, 0, len(tf.Members))
			for _, entry := range tf.Members {
				matched := false
				for _, ref := range memberRefs {
					if operatorRefMatchesEntry(ctx, ref, entry) {
						matched = true
						break
					}
				}
				if matched {
					removed++
					continue
				}
				kept = append(kept, entry)
			}
			tf.Members = kept
			patch["members"] = tf.Members
		}
		if len(ownerRefs) > 0 {
			kept := make([]json.RawMessage, 0, len(tf.Owners))
			for _, entry := range tf.Owners {
				matched := false
				for _, ref := range ownerRefs {
					if operatorRefMatchesEntry(ctx, ref, entry) {
						matched = true
						break
					}
				}
				if matched {
					removed++
					continue
				}
				kept = append(kept, entry)
			}
			tf.Owners = kept
			patch["owners"] = tf.Owners
		}

		patchJSON, err := json.Marshal(patch)
		if err != nil {
			return fmt.Errorf("encoding fields patch: %w", err)
		}

		updated, err := beadsClient.UpdateBead(ctx, bead.ID, &client.UpdateBeadRequest{Fields: patchJSON})
		if err != nil {
			return fmt.Errorf("updating team: %w", err)
		}

		if jsonOutput {
			printBeadJSON(updated)
		} else {
			fmt.Printf("Updated team %s (%s): %d entr%s removed\n", name, bead.ID, removed, pluralY(removed))
		}
		return nil
	},
}

func init() {
	teamRemoveCmd.Flags().StringSlice("member", nil, "member operator id/email/alias to remove (repeatable)")
	teamRemoveCmd.Flags().StringSlice("owner", nil, "owner operator id/email/alias to remove (repeatable)")
}
