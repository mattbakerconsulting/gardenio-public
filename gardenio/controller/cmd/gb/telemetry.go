package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
)

var telemetryCmd = &cobra.Command{
	Use:     "telemetry",
	Short:   "Inspect UI telemetry events",
	GroupID: "orchestration",
}

var telemetryTailCmd = &cobra.Command{
	Use:   "tail",
	Short: "Show recent UI telemetry events from the daemon sink",
	Long: `Show recent UI telemetry events from the daemon's ui_telemetry sink.

The Phase 9 incident-intel surfaces (inbox rich-report rows, rich-report
back-link) post click events to POST /dev/incident-intel/telemetry, which
forwards them to the daemon's ui_telemetry table. This command reads that
sink so an operator can verify clicks land.

Newest first.`,
	RunE: runTelemetryTail,
}

func init() {
	telemetryTailCmd.Flags().String("event", "", "filter by event name (e.g. inbox-rich-report-click)")
	telemetryTailCmd.Flags().Duration("since", 0, "only show events newer than this duration ago (e.g. 15m, 1h)")
	telemetryTailCmd.Flags().IntP("limit", "n", 20, "maximum rows to return (max 1000)")
	telemetryTailCmd.Flags().Bool("json", false, "emit one JSON object per line instead of a table")

	telemetryCmd.AddCommand(telemetryTailCmd)
}

func runTelemetryTail(cmd *cobra.Command, args []string) error {
	event, _ := cmd.Flags().GetString("event")
	since, _ := cmd.Flags().GetDuration("since")
	limit, _ := cmd.Flags().GetInt("limit")
	asJSON, _ := cmd.Flags().GetBool("json")

	filter := beadsapi.UITelemetryFilter{
		Event: event,
		Limit: limit,
	}
	if since > 0 {
		filter.Since = time.Now().Add(-since)
	}

	rows, err := daemon.ListUITelemetry(cmd.Context(), filter)
	if err != nil {
		return fmt.Errorf("listing telemetry: %w", err)
	}

	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		for _, r := range rows {
			if err := enc.Encode(r); err != nil {
				return err
			}
		}
		return nil
	}

	if len(rows) == 0 {
		fmt.Fprintln(os.Stderr, "no telemetry events match")
		return nil
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "WHEN\tEVENT\tTARGET\tFROM\tOPERATOR")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			r.CreatedAt.Local().Format(time.RFC3339),
			r.Event,
			truncForTable(r.Target, 24),
			truncForTable(r.From, 32),
			r.OperatorID,
		)
	}
	return tw.Flush()
}

func truncForTable(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
