package main

import (
	"context"
	"fmt"

	"gardenio/kbeads/internal/client"
	"github.com/spf13/cobra"
)

var teamListCmd = &cobra.Command{
	Use:   "list",
	Short: "List teams",
	RunE: func(cmd *cobra.Command, args []string) error {
		limit, _ := cmd.Flags().GetInt("limit")
		offset, _ := cmd.Flags().GetInt("offset")

		resp, err := beadsClient.ListBeads(context.Background(), &client.ListBeadsRequest{
			Type:   []string{"team"},
			Kind:   []string{"config"},
			Limit:  limit,
			Offset: offset,
		})
		if err != nil {
			return fmt.Errorf("listing teams: %w", err)
		}

		if jsonOutput {
			printBeadListJSONWithTotal(resp.Beads, resp.Total, jsonOutput)
		} else {
			printBeadListTable(resp.Beads, resp.Total)
		}
		return nil
	},
}

func init() {
	teamListCmd.Flags().Int("limit", 20, "maximum results to return")
	teamListCmd.Flags().Int("offset", 0, "pagination offset")
}
