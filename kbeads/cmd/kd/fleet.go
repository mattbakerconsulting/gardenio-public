package main

import (
	"context"
	"fmt"

	"gardenio/kbeads/internal/client"
	"github.com/spf13/cobra"
)

var fleetCmd = &cobra.Command{
	Use:     "fleet",
	Short:   "Fleet management commands",
	GroupID: "system",
}

var fleetListCmd = &cobra.Command{
	Use:   "list",
	Short: "List fleet beads",
	RunE: func(cmd *cobra.Command, args []string) error {
		resp, err := beadsClient.ListBeads(context.Background(), &client.ListBeadsRequest{
			Type: []string{"fleet"},
		})
		if err != nil {
			return fmt.Errorf("listing fleets: %w", err)
		}

		if jsonOutput {
			printBeadListJSONWithTotal(resp.Beads, resp.Total, jsonOutput)
		} else {
			printBeadListTable(resp.Beads, resp.Total)
		}
		return nil
	},
}

var fleetShowCmd = &cobra.Command{
	Use:   "show <fleet-name-or-id>",
	Short: "Show fleet details",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		bead, err := beadsClient.GetBead(context.Background(), args[0])
		if err != nil {
			return fmt.Errorf("getting fleet: %w", err)
		}

		if jsonOutput {
			printBeadJSON(bead)
		} else {
			printBeadTable(bead)
		}
		return nil
	},
}

var fleetTransferCmd = &cobra.Command{
	Use:   "transfer <bead-id> --to-fleet <fleet>",
	Short: "Transfer a bead to a different fleet",
	Long:  "Move a bead and all its related data (deps, labels, comments, events) to a target fleet. Requires a gardenio-admin OIDC login (kd login); the shared static token and fleet tokens are refused.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		toFleet, _ := cmd.Flags().GetString("to-fleet")
		if toFleet == "" {
			return fmt.Errorf("--to-fleet is required")
		}

		bead, err := beadsClient.TransferBead(context.Background(), args[0], toFleet)
		if err != nil {
			return fmt.Errorf("transferring bead: %w", err)
		}

		if jsonOutput {
			printBeadJSON(bead)
		} else {
			fmt.Printf("Transferred %s to fleet %q\n", bead.ID, bead.Fleet)
		}
		return nil
	},
}

func init() {
	fleetTransferCmd.Flags().String("to-fleet", "", "target fleet name (required)")
	_ = fleetTransferCmd.MarkFlagRequired("to-fleet")

	fleetCmd.AddCommand(fleetListCmd)
	fleetCmd.AddCommand(fleetShowCmd)
	fleetCmd.AddCommand(fleetTransferCmd)
}
