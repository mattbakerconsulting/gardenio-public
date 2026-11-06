package main

import "github.com/spf13/cobra"

var molCmd = &cobra.Command{
	Use:     "mol",
	Short:   "Manage instantiated formula molecules",
	Long:    "Molecules are work item sets created by pouring a formula.\nEach molecule is an epic-like bead with child beads for each step.",
	GroupID: "beads",
}

func init() {
	molCmd.AddCommand(molListCmd)
	molCmd.AddCommand(molShowCmd)
	molCmd.AddCommand(newPourCmd())
	molCmd.AddCommand(newWispCmd())
	molCmd.AddCommand(newBurnCmd())
	molCmd.AddCommand(newSquashCmd())
	molCmd.AddCommand(newBondCmd())
	molCmd.AddCommand(newGCCmd())
}
