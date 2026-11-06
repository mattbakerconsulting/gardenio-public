package main

import (
	"context"
	"fmt"

	"gardenio/kbeads/internal/model"
	"github.com/spf13/cobra"
)

// teamShowResult wraps a type:team bead with its derived membership count
// for `kd team show` (ADR docs/adr/taints.md §9.2 acceptance: "renders all
// fields including a derived membership count").
type teamShowResult struct {
	*model.Bead
	MemberCount int `json:"member_count"`
}

var teamShowCmd = &cobra.Command{
	Use:   "show <name>",
	Short: "Show team details",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		ctx := context.Background()

		bead, err := findTeamByName(ctx, name)
		if err != nil {
			return err
		}
		tf, err := parseTeamFields(bead.Fields)
		if err != nil {
			return err
		}
		memberCount := len(tf.Members)

		if jsonOutput {
			emitJSON(teamShowResult{Bead: bead, MemberCount: memberCount})
		} else {
			printBeadTable(bead)
			fmt.Printf("Membership Count: %d\n", memberCount)
		}
		return nil
	},
}
