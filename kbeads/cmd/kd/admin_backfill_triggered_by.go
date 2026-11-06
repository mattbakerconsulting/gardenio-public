package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"gardenio/kbeads/internal/client"
	"github.com/spf13/cobra"
)

// adminBackfillTriggeredByCmd drives the M4 auto-link backfill scanner
// from epic ka-0000000933 / task ka-0000000004. Before the spawn-time
// triggered-by writers landed (M2/M3), beads spawned from a Slack thread
// or a trigger fired event carried the origin in their fields
// (slack_thread_bead_id / trigger_id) but never got the triggered-by
// dep edge. This scanner walks recent beads and fills in the edge when
// a single unambiguous origin signal exists.
//
// Conservative by design: append-only, never modifies existing deps,
// skips ambiguous and self-referential candidates, and emits per-skip
// tallies so the audit trail explains why each candidate was or was not
// linked. See bead ka-0000000827 for the full design.
var adminBackfillTriggeredByCmd = &cobra.Command{
	Use:   "backfill-triggered-by-deps",
	Short: "Reconstruct triggered-by deps for unparented spawn-origin beads",
	Long: `Adds the missing triggered-by edge for beads whose fields carry
an unambiguous spawn-origin signal but never got linked at creation
time (M4 auto-link backfill scanner; epic ka-0000000933).

Recognized signal fields on the candidate bead:
  - fields.slack_thread_bead_id — slack-thread spawn (ka-0000005137 / M2)
  - fields.trigger_id           — trigger-engine spawn (ka-0000007272 / M3)

Scope (default): beads created within --since (default 24h) that carry
at least one of the signal fields and have no parent-child or
triggered-by dep already.

Skipped (counted, never written):
  - already has a parent-child or triggered-by edge
  - signal field points at the candidate bead itself
  - signal field points at a missing or soft-deleted bead
  - multiple signal fields disagree on the target (ambiguous)
  - no surviving signal after the above filters

Each batch (--batch-size) is its own transaction so a mid-run failure
leaves a coherent, resumable state. INSERT uses ON CONFLICT … DO NOTHING
so re-running on already-linked data is a no-op.

Output is a JSON manifest with edge counts grouped by signal field,
skip-reason tallies, and timing — capture stdout for the audit trail.
Use --dry-run to preview without writing.`,
	RunE: runAdminBackfillTriggeredBy,
}

func init() {
	adminBackfillTriggeredByCmd.Flags().Duration("since", 24*time.Hour, "limit scan to beads created within this duration")
	adminBackfillTriggeredByCmd.Flags().Int("batch-size", 500, "rows per page; each batch is its own tx")
	adminBackfillTriggeredByCmd.Flags().Int("max-batches", 0, "cap on number of batches (0 = drain queue)")
	adminBackfillTriggeredByCmd.Flags().Bool("dry-run", false, "print would-be edges without writing")
	adminCmd.AddCommand(adminBackfillTriggeredByCmd)
}

func runAdminBackfillTriggeredBy(cmd *cobra.Command, _ []string) error {
	since, _ := cmd.Flags().GetDuration("since")
	batchSize, _ := cmd.Flags().GetInt("batch-size")
	maxBatches, _ := cmd.Flags().GetInt("max-batches")
	dryRun, _ := cmd.Flags().GetBool("dry-run")

	ctx := context.Background()
	start := time.Now()

	resp, err := beadsClient.BackfillTriggeredByDeps(ctx, &client.BackfillTriggeredByDepsRequest{
		SinceSeconds: int64(since / time.Second),
		BatchSize:    batchSize,
		MaxBatches:   maxBatches,
		DryRun:       dryRun,
	})
	if err != nil {
		return fmt.Errorf("backfill-triggered-by-deps: %w", err)
	}

	manifest := map[string]any{
		"dry_run":                resp.DryRun,
		"candidate_beads":        resp.CandidateBeads,
		"edges_added":            resp.EdgesAdded,
		"edges_by_signal":        resp.EdgesBySignal,
		"skipped_existing_dep":   resp.SkippedExistingDep,
		"skipped_no_signal":      resp.SkippedNoSignal,
		"skipped_ambiguous":      resp.SkippedAmbiguous,
		"skipped_self_reference": resp.SkippedSelfReference,
		"skipped_target_missing": resp.SkippedTargetMissing,
		"batches":                resp.Batches,
		"batches_capped":         resp.BatchesCapped,
		"last_id":                resp.LastID,
		"server_duration_ms":     resp.DurationMs,
		"client_wall_ms":         time.Since(start).Milliseconds(),
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(manifest); err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	return nil
}
