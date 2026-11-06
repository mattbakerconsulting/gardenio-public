package main

import "github.com/spf13/cobra"

// adminCmd groups rarely-used operations that modify large numbers of
// beads or rely on elevated privileges. The subcommands are intended
// for operators running out-of-band maintenance, not day-to-day work.
var adminCmd = &cobra.Command{
	Use:   "admin",
	Short: "Operator-only maintenance commands",
}

// adminMigrateCmd is the parent for one-shot migrations. Each
// migration is a discrete subcommand so the help text describes the
// exact rules and the dry-run flag is the default.
var adminMigrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "One-shot data migrations",
}

func init() {
	adminCmd.AddCommand(adminMigrateCmd)
	adminMigrateCmd.AddCommand(adminMigrateAdviceTargetingCmd)
}
