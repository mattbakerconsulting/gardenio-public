package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
)

var healthCmd = &cobra.Command{
	Use:     "health",
	Short:   "Check the health of the beads service",
	GroupID: "system",
	RunE: func(cmd *cobra.Command, args []string) error {
		status, err := beadsClient.Health(context.Background())
		if err != nil {
			return fmt.Errorf("checking health: %w", err)
		}

		if jsonOutput {
			out := map[string]string{"status": status}
			data, err := json.MarshalIndent(out, "", "  ")
			if err != nil {
				return fmt.Errorf("marshaling JSON: %w", err)
			}
			fmt.Println(string(data))
		} else {
			fmt.Printf("Health: %s\n", status)
		}

		if status != "ok" {
			return fmt.Errorf("unhealthy: %s", status)
		}
		return nil
	},
}
