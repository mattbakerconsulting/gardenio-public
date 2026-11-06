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

// adminCloseTriggerAuditCmd drives ka-0000006341's one-shot close
// sweep over legacy type=event beads labeled `auto:trigger-audit` whose
// status is still open. The forward-fix in ka-0000004100 makes new
// audit beads land status=closed, but ~226k legacy rows from before the
// fix are still open and inflate the beads-viewer open-beads count.
//
// Each batch is its own transaction (per kbeads/CLAUDE.md migration
// policy — no eager full-table updates) and the UPDATE filters
// status != 'closed' so re-runs are no-ops. --dry-run reports the
// would-be-closed count without writing.
var adminCloseTriggerAuditCmd = &cobra.Command{
	Use:   "close-trigger-audit-beads",
	Short: "Close legacy trigger-audit ledger beads",
	Long: `Closes legacy type=event beads labeled 'auto:trigger-audit'
whose status is still open. These are append-only ledger records
written by recordTriggerAudit before ka-0000004100's forward-fix made
them land status=closed.

Each batch (--batch-size) is its own transaction so a mid-run failure
leaves a coherent, resumable state — re-run to pick up where the last
run left off. The UPDATE filters status != 'closed' so re-running on
already-closed data is a no-op.

Output is a JSON manifest with candidate/closed counts and timing —
capture stdout for the audit trail. Use --dry-run to preview without
writing.`,
	RunE: runAdminCloseTriggerAudit,
}

func init() {
	adminCloseTriggerAuditCmd.Flags().Int("batch-size", 500, "rows per page; each batch is its own tx")
	adminCloseTriggerAuditCmd.Flags().Int("max-batches", 0, "cap on number of batches (0 = drain queue)")
	adminCloseTriggerAuditCmd.Flags().Bool("dry-run", false, "count would-be closes without writing")
	adminCmd.AddCommand(adminCloseTriggerAuditCmd)
}

func runAdminCloseTriggerAudit(cmd *cobra.Command, _ []string) error {
	batchSize, _ := cmd.Flags().GetInt("batch-size")
	maxBatches, _ := cmd.Flags().GetInt("max-batches")
	dryRun, _ := cmd.Flags().GetBool("dry-run")

	ctx := context.Background()
	start := time.Now()

	resp, err := beadsClient.CloseTriggerAuditBeads(ctx, &client.CloseTriggerAuditBeadsRequest{
		BatchSize:  batchSize,
		MaxBatches: maxBatches,
		DryRun:     dryRun,
	})
	if err != nil {
		return fmt.Errorf("close-trigger-audit-beads: %w", err)
	}

	manifest := map[string]any{
		"dry_run":            resp.DryRun,
		"candidate_beads":    resp.CandidateBeads,
		"beads_closed":       resp.BeadsClosed,
		"batches":            resp.Batches,
		"batches_capped":     resp.BatchesCapped,
		"last_id":            resp.LastID,
		"server_duration_ms": resp.DurationMs,
		"client_wall_ms":     time.Since(start).Milliseconds(),
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(manifest); err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	return nil
}
