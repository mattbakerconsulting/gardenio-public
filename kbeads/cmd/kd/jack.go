package main

import "github.com/spf13/cobra"

var jackCmd = &cobra.Command{
	Use:   "jack",
	Short: "Manage infrastructure modification jacks",
	Long:  "Jacks are temporary, time-limited change permits with audit trail and automatic expiry. Create a jack BEFORE making infrastructure changes outside CI/CD.",
}

func init() {
	jackCmd.AddCommand(jackUpCmd)
	jackCmd.AddCommand(jackDownCmd)
	jackCmd.AddCommand(jackListCmd)
	jackCmd.AddCommand(jackShowCmd)
	jackCmd.AddCommand(jackExtendCmd)
	jackCmd.AddCommand(jackLogCmd)
	jackCmd.AddCommand(jackCheckCmd)
}
