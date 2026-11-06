package main

import (
	"context"
	"encoding/json"
	"fmt"

	"gardenio/kbeads/internal/client"
	"github.com/spf13/cobra"
)

var teamAddCmd = &cobra.Command{
	Use:   "add <name>",
	Short: "Add a member or owner to a team",
	Long: `Add one or more members/owners to a type:team bead's members/owners
array. A bare email/username is resolved through operator_aliases to an
operators(id) before being stored (falling back to a pending alias
reference if no operator record exists yet). Rejected server-side with a
403 if the caller lacks the required role or ownership.`,
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
		added := 0

		if len(memberRefs) > 0 {
			for _, ref := range memberRefs {
				resolved, err := resolveOperatorRef(ctx, ref)
				if err != nil {
					return err
				}
				if !containsRawJSON(tf.Members, resolved) {
					tf.Members = append(tf.Members, resolved)
					added++
				}
			}
			patch["members"] = tf.Members
		}
		if len(ownerRefs) > 0 {
			for _, ref := range ownerRefs {
				resolved, err := resolveOperatorRef(ctx, ref)
				if err != nil {
					return err
				}
				if !containsRawJSON(tf.Owners, resolved) {
					tf.Owners = append(tf.Owners, resolved)
					added++
				}
			}
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
			fmt.Printf("Updated team %s (%s): %d entr%s added\n", name, bead.ID, added, pluralY(added))
		}
		return nil
	},
}

func pluralY(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

func init() {
	teamAddCmd.Flags().StringSlice("member", nil, "member operator id/email/alias to add (repeatable)")
	teamAddCmd.Flags().StringSlice("owner", nil, "owner operator id/email/alias to add (repeatable)")
}
