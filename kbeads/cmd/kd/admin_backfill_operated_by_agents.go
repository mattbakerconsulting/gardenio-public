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

// adminBackfillOperatedByAgentsCmd drives sub-item #1 of ka-0000006817
// (per-user homepage M2): backfill operated_by_id on `type=agent` beads
// whose value is unset (NULL on legacy envs, or bound to the
// `system:legacy-pre-attribution` sentinel post-migration 022). The
// per-user homepage filters agent beads by operated_by_id; legacy rows
// that never got stamped at write time stay invisible until this
// backfill stamps them.
//
// Per kbeads/CLAUDE.md §Migration policy this is a cold-path admin
// command, not an eager in-migration backfill. Each scan page is its
// own transaction; UPDATEs are idempotent (filter on `IS NULL OR =
// sentinel`), so re-runs on already-stamped data are no-ops.
var adminBackfillOperatedByAgentsCmd = &cobra.Command{
	Use:   "backfill-operated-by-agents",
	Short: "Backfill operated_by_id on agent beads (ka-0000006817 sub-item #1)",
	Long: `Scans type=agent beads with no operated_by_id binding and resolves
the owning human via the agent bead's own identity fields, in priority
order:

  1. fields.slack_user_id     → operator_aliases('slack', uid)
  2. fields.gitlab_username   → operator_aliases('gitlab', uname)
  3. fields.jira_user_id      → operator_aliases('jira', jid)
  4. fields.operated_by       → split "<kind>:<subject>", look up alias

Rows where none of the four fields resolves are skipped (counted, not
bound to any fallback). With --include-sentinel (default true), rows
currently bound to the system:legacy-pre-attribution sentinel are also
scanned — production envs land legacy rows there via migration 022.

The scan is keyset-paginated by id; each batch is its own transaction.
Every UPDATE writes one mutation_history row with mutation_type=
'backfill-operated-by-agents'. Re-runs on already-cleaned data are
no-ops because the WHERE clause filters by IS NULL / sentinel.

Output is a JSON manifest with processed/updated/skipped counts and a
per-signal histogram of resolutions — capture stdout for the audit
trail. Use --dry-run to preview without writing.`,
	RunE: runAdminBackfillOperatedByAgents,
}

func init() {
	adminBackfillOperatedByAgentsCmd.Flags().Int("batch-size", 500, "rows per scan page (each page is its own tx)")
	adminBackfillOperatedByAgentsCmd.Flags().Int("max-batches", 0, "cap on number of scan pages (0 = no cap)")
	adminBackfillOperatedByAgentsCmd.Flags().Bool("include-sentinel", true, "also scan rows bound to system:legacy-pre-attribution")
	adminBackfillOperatedByAgentsCmd.Flags().Bool("dry-run", false, "print would-be updates without writing")
	adminCmd.AddCommand(adminBackfillOperatedByAgentsCmd)
}

func runAdminBackfillOperatedByAgents(cmd *cobra.Command, _ []string) error {
	batchSize, _ := cmd.Flags().GetInt("batch-size")
	maxBatches, _ := cmd.Flags().GetInt("max-batches")
	includeSentinel, _ := cmd.Flags().GetBool("include-sentinel")
	dryRun, _ := cmd.Flags().GetBool("dry-run")

	ctx := context.Background()
	start := time.Now()

	resp, err := beadsClient.BackfillOperatedByAgents(ctx, &client.BackfillOperatedByAgentsRequest{
		BatchSize:       batchSize,
		MaxBatches:      maxBatches,
		IncludeSentinel: includeSentinel,
		DryRun:          dryRun,
	})
	if err != nil {
		return fmt.Errorf("backfill-operated-by-agents: %w", err)
	}

	manifest := map[string]any{
		"dry_run":            resp.DryRun,
		"processed":          resp.Processed,
		"updated":            resp.Updated,
		"skipped":            resp.Skipped,
		"rebind_by_signal":   resp.RebindBySignal,
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
