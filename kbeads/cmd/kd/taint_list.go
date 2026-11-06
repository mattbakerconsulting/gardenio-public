package main

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

var taintListCmd = &cobra.Command{
	Use:   "list",
	Short: "List taints",
	Long: `List every taint in the registry (ADR docs/adr/taints.md §9.1). Every
viewer sees name, owner team, description and timestamps; scrub_policy is
included only for taints the caller owns (or when the caller is an admin).`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := context.Background()

		resp, err := beadsClient.ListTaints(ctx)
		if err != nil {
			return fmt.Errorf("listing taints: %w", err)
		}

		if jsonOutput {
			emitJSON(resp.Taints)
			return nil
		}

		tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tOWNER TEAM\tDESCRIPTION\tRETIRED")
		for _, t := range resp.Taints {
			retired := ""
			if t.RetiredAt != nil {
				retired = t.RetiredAt.Format("2006-01-02")
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", t.Name, t.OwnerTeam, t.Description, retired)
		}
		return tw.Flush()
	},
}
