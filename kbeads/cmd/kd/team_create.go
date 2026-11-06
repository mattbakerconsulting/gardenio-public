package main

import (
	"context"
	"encoding/json"
	"fmt"

	"gardenio/kbeads/internal/client"
	"github.com/spf13/cobra"
)

var teamCreateCmd = &cobra.Command{
	Use:   "create <name>",
	Short: "Create a team",
	Long: `Create a type:team config bead (ADR docs/adr/taints.md §8).

Team write policy requires the gardenio-admin role; a non-admin caller is
rejected server-side with a clear 403 error — this command does not check
the caller's role itself.

Bare emails/usernames passed to --member/--owner are resolved through
operator_aliases to an operators(id) before being stored; an alias with no
operator record yet is stored as a pending "kind:subject" reference instead
and resolves automatically once that person authenticates.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		ctx := context.Background()

		display, _ := cmd.Flags().GetString("display")
		memberRefs, _ := cmd.Flags().GetStringSlice("member")
		ownerRefs, _ := cmd.Flags().GetStringSlice("owner")
		oidcGroups, _ := cmd.Flags().GetStringSlice("oidc-group")
		projects, _ := cmd.Flags().GetStringSlice("project")

		fields := map[string]any{"team": name}
		if display != "" {
			fields["display"] = display
		}
		if len(oidcGroups) > 0 {
			fields["oidc_groups"] = oidcGroups
		}
		if len(projects) > 0 {
			fields["projects"] = projects
		}
		if len(memberRefs) > 0 {
			resolved, err := resolveOperatorRefs(ctx, memberRefs)
			if err != nil {
				return err
			}
			fields["members"] = resolved
		}
		if len(ownerRefs) > 0 {
			resolved, err := resolveOperatorRefs(ctx, ownerRefs)
			if err != nil {
				return err
			}
			fields["owners"] = resolved
		}

		fieldsJSON, err := json.Marshal(fields)
		if err != nil {
			return fmt.Errorf("encoding fields: %w", err)
		}

		title := name
		if display != "" {
			title = display
		}

		bead, err := beadsClient.CreateBead(ctx, &client.CreateBeadRequest{
			Title:     title,
			Type:      "team",
			CreatedBy: actor,
			Fields:    fieldsJSON,
		})
		if err != nil {
			return fmt.Errorf("creating team: %w", err)
		}

		if jsonOutput {
			printBeadJSON(bead)
		} else {
			fmt.Printf("Created team %s (%s)\n", name, bead.ID)
		}
		return nil
	},
}

func init() {
	teamCreateCmd.Flags().String("display", "", "display name for the team")
	teamCreateCmd.Flags().StringSlice("member", nil, "member operator id/email/alias (repeatable)")
	teamCreateCmd.Flags().StringSlice("oidc-group", nil, "OIDC group name granting membership (repeatable)")
	teamCreateCmd.Flags().StringSlice("owner", nil, "owner operator id/email/alias (repeatable)")
	teamCreateCmd.Flags().StringSlice("project", nil, "informational project association (repeatable)")
}
