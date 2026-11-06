package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"gardenio/kbeads/internal/client"
	"github.com/spf13/cobra"
)

var taintCreateCmd = &cobra.Command{
	Use:   "create <name>",
	Short: "Create or update a taint",
	Long: `Register a named taint in the taints registry (ADR docs/adr/taints.md
§3, §7). Admin-only server-side (RoleAdmin) — a role check, not a check on
--owner team membership.

Creating a taint implicitly grants its owner team full read access to
beads it carries (§10.1: "ownership implies the grant"); no separate grant
is needed for the owner team.

--preset and --policy are mutually exclusive. --preset resolves to one of
the starting-point policies §10.2 documents (production, internal,
scratch); the result is an ordinary scrub_policy the owner can edit
afterward like any other — nothing remembers that it came from a preset.
--policy loads a scrub_policy JSON document from a file (§3/§6.2 shape).
With neither flag, the taint gets the §10.1 default: human-four-eyes with
the owner team as sole approver.

--effect (repeatable) sets the taint's effect set (§15.2): gate, restrict,
evict, or sink. Only "gate" is enforced today; the others are reserved.
Omitted entirely, the taint defaults to {gate} — today's only behavior.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		ctx := context.Background()

		description, _ := cmd.Flags().GetString("description")
		ownerRef, _ := cmd.Flags().GetString("owner")
		presetFlag, _ := cmd.Flags().GetString("preset")
		policyFile, _ := cmd.Flags().GetString("policy")
		effects, _ := cmd.Flags().GetStringArray("effect")

		if ownerRef == "" {
			return fmt.Errorf("--owner is required")
		}
		if presetFlag != "" && policyFile != "" {
			return fmt.Errorf("--preset and --policy are mutually exclusive")
		}

		ownerTeam, err := resolveTaintOwnerTeam(ctx, ownerRef)
		if err != nil {
			return err
		}

		var policy json.RawMessage
		if policyFile != "" {
			data, err := os.ReadFile(policyFile)
			if err != nil {
				return fmt.Errorf("reading --policy file: %w", err)
			}
			policy = json.RawMessage(data)
		}

		req := &client.CreateTaintRequest{
			Name:        name,
			Description: description,
			OwnerTeam:   ownerTeam,
			Preset:      presetFlag,
			ScrubPolicy: policy,
			Effects:     effects,
		}
		taint, err := beadsClient.CreateTaint(ctx, req)
		if err != nil {
			return fmt.Errorf("creating taint: %w", err)
		}

		if jsonOutput {
			emitJSON(taint)
		} else {
			fmt.Printf("Created taint %s (owner: %s)\n", taint.Name, taint.OwnerTeam)
		}
		return nil
	},
}

func init() {
	taintCreateCmd.Flags().String("description", "", "human-readable description")
	taintCreateCmd.Flags().String("owner", "", "owner team, by name or bead id (required)")
	taintCreateCmd.Flags().String("preset", "", "starting-point policy: production, internal, or scratch")
	taintCreateCmd.Flags().String("policy", "", "path to a scrub_policy JSON file")
	taintCreateCmd.Flags().StringArray("effect", nil, "taint effect (repeatable): gate, restrict, evict, sink (default: gate)")
}
