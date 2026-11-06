package main

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"gardenio/kbeads/internal/client"
	"github.com/spf13/cobra"
)

// adminBackfillProjectLabelsCmd gives the beads written before the daemon's
// project-label invariant (gd-YdsEpnN3Qo) the project: label it now requires:
// a project-bearing bead (agent, session, trigger, ...) with fields.project and
// no project label gets project:<fields.project>, and a message with none takes
// its thread head's. Without the label a bead is invisible to its own
// project's agent tokens, so an agent cannot run this; it needs a
// gardenio-admin bearer.
var adminBackfillProjectLabelsCmd = &cobra.Command{
	Use:   "backfill-project-labels",
	Short: "Label the beads that carry fields.project (or a thread's) but no project: label",
	Long: `Counts, and with --apply labels, the beads that break the project-label
invariant: a bead of a project-bearing type (one whose type config declares a
project field, plot_application excepted) with fields.project and no project:
label, and a message with no project: label whose thread head carries exactly
one. Each page adds up to --batch-size labels through the normal AddLabel path
in its own transaction. Dry run by default (prints the counts); --apply writes
until none remain. Idempotent: a re-run finds nothing. The same count is the
gardenio_beads_project_label_missing gauge.`,
	RunE: runAdminBackfillProjectLabels,
}

func init() {
	adminBackfillProjectLabelsCmd.Flags().Bool("apply", false, "write the labels (default: print the counts)")
	adminBackfillProjectLabelsCmd.Flags().Int("batch-size", 500, "labels per page (server caps at 1000)")
	adminCmd.AddCommand(adminBackfillProjectLabelsCmd)
}

// formatTypeCounts renders {"agent":3,"message":1} as "agent=3 message=1".
func formatTypeCounts(counts map[string]int) string {
	if len(counts) == 0 {
		return "none"
	}
	var parts []string
	for _, t := range slices.Sorted(maps.Keys(counts)) {
		parts = append(parts, fmt.Sprintf("%s=%d", t, counts[t]))
	}
	return strings.Join(parts, " ")
}

func runAdminBackfillProjectLabels(cmd *cobra.Command, _ []string) error {
	apply, _ := cmd.Flags().GetBool("apply")
	batchSize, _ := cmd.Flags().GetInt("batch-size")
	ctx := context.Background()

	if !apply {
		resp, err := beadsClient.BackfillProjectLabels(ctx, &client.BackfillProjectLabelsRequest{DryRun: true})
		if err != nil {
			return fmt.Errorf("counting beads missing a project label: %w", err)
		}
		fmt.Printf("missing a project label: %s (dry run; --apply to label them)\n", formatTypeCounts(resp.Missing))
		return nil
	}

	total := map[string]int{}
	for page := 1; ; page++ {
		resp, err := beadsClient.BackfillProjectLabels(ctx, &client.BackfillProjectLabelsRequest{BatchSize: batchSize})
		if err != nil {
			return fmt.Errorf("backfill-project-labels page %d: %w", page, err)
		}
		n := 0
		for t, c := range resp.Labelled {
			total[t] += c
			n += c
		}
		fmt.Printf("page %d: labelled %s\n", page, formatTypeCounts(resp.Labelled))
		if resp.Done || n == 0 {
			break
		}
	}
	fmt.Printf("labelled %s\n", formatTypeCounts(total))
	return nil
}
