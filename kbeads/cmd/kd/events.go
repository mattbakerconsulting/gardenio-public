package main

import (
	"github.com/spf13/cobra"
)

// eventsCmd is the parent for events-table maintenance/replay subcommands.
// Today it hosts only `backfill` (E4b, ka-0000006538).
var eventsCmd = &cobra.Command{
	Use:   "events",
	Short: "Inspect and replay the kbeads events table",
	Long: `Subcommands that operate on the durable events table.

  backfill   replay historical events into VictoriaMetrics as
             gardenio_beads_events_total samples timestamped at events.created_at`,
}

func init() {
	rootCmd.AddCommand(eventsCmd)
}
