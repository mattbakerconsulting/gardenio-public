package main

import "github.com/spf13/cobra"

var formulaCmd = &cobra.Command{
	Use:     "formula",
	Short:   "Manage reusable work formulas",
	Long:    "Formulas define reusable sets of work items (steps) with variable substitution.\nCreate a formula, then pour it to instantiate a molecule of beads.",
	GroupID: "beads",
}

func init() {
	formulaCmd.AddCommand(formulaCreateCmd)
	formulaCmd.AddCommand(formulaListCmd)
	formulaCmd.AddCommand(formulaShowCmd)
	formulaCmd.AddCommand(formulaDumpCmd)
	formulaCmd.AddCommand(formulaUpdateCmd)
	formulaCmd.AddCommand(formulaApplyCmd)
	formulaCmd.AddCommand(newPourCmd())
	formulaCmd.AddCommand(newWispCmd())
	formulaCmd.AddCommand(newCookCmd())
	formulaCmd.AddCommand(newExpandCmd())
}
