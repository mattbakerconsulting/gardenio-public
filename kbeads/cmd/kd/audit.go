package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
)

var auditCmd = &cobra.Command{
	Use:     "audit",
	Short:   "Inspect bead audit history",
	GroupID: "beads",
}

var auditBeadCmd = &cobra.Command{
	Use:   "bead <bead-id>",
	Short: "Show append-only mutation history for a bead",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		beadID := args[0]

		muts, err := beadsClient.GetMutations(context.Background(), beadID)
		if err != nil {
			return fmt.Errorf("listing mutations: %w", err)
		}

		if jsonOutput {
			data, err := json.MarshalIndent(muts, "", "  ")
			if err != nil {
				return fmt.Errorf("marshaling JSON: %w", err)
			}
			fmt.Println(string(data))
			return nil
		}

		if len(muts) == 0 {
			fmt.Println("No mutations recorded.")
			return nil
		}
		for _, m := range muts {
			ts := ""
			if !m.CreatedAt.IsZero() {
				ts = m.CreatedAt.Format("2006-01-02 15:04:05")
			}
			actor := m.Actor
			if actor == "" {
				actor = "<unknown>"
			}
			fmt.Printf("[%s] %s %s %s: %q -> %q\n", ts, actor, m.MutationType, m.Field, m.OldValue, m.NewValue)
		}
		return nil
	},
}

func init() {
	auditCmd.AddCommand(auditBeadCmd)
}
