package main

import "github.com/spf13/cobra"

var adviceCmd = &cobra.Command{
	Use:        "advice",
	Short:      "Manage advice beads",
	Long:       "Advice beads are persistent guidance delivered to agents based on label matching. They can optionally run hook commands on session events.",
	Deprecated: "use 'gb advice' instead",
}

func init() {
	adviceCmd.AddCommand(adviceAddCmd)
	adviceCmd.AddCommand(adviceListCmd)
	adviceCmd.AddCommand(adviceShowCmd)
	adviceCmd.AddCommand(adviceRemoveCmd)
}
