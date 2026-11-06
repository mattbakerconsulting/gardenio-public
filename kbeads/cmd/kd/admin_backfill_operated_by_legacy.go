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

// adminBackfillOperatedByLegacyCmd drives ka-0000001880's cleanup of
// legacy `operated_by` attributions — beads whose operated_by_id still
// points at a human operator (e.g. slack:U0OPERATOR1 / Matt) because
// pre-attribution-v2 code paths recorded the human session owner instead
// of the actual agent / cron / parent task. See research note
// gardenio/docs/research/legacy-operated-by-matt-cleanup.md §3.
//
// The server runs three passes against the target operator:
//
//	Pass 1 — keyset-paginated batch scan over candidate beads; for each
//	         batch (own transaction) categorize rows A/B/C/D in Go and
//	         UPDATE A & B groups; write one mutation_history row per
//	         affected bead. C rows are deferred to pass 2.
//	Pass 2 — iterative parent-walk UPDATE against deps (parent-child)
//	         until no rows change or --max-iterations is hit. Each
//	         iteration is its own transaction.
//	Pass 3 — emit before/after candidate counts.
//
// All UPDATEs filter `WHERE operated_by_id = target` so re-runs on
// already-cleaned data are no-ops.
var adminBackfillOperatedByLegacyCmd = &cobra.Command{
	Use:   "backfill-operated-by-legacy",
	Short: "Re-categorize beads bound to a legacy operator",
	Long: `Rebinds beads currently attributed to --target=<kind>:<subject>
(typically slack:U0OPERATOR1 — Matt) to their correct operator using the
algorithm in gardenio/docs/research/legacy-operated-by-matt-cleanup.md §3.2:

  A) Slack-thread agents → resolve via fields.slack_user_id
  B) cron-spawned (created_by='static') → schedule.operated_by_id
  C) agent-spawned descendants → iterative parent-walk via deps
  D) genuinely the target operator → leave alone

Each batch in Pass 1 (--batch-size) and each parent-walk iteration in
Pass 2 (--max-iterations) runs in its own transaction. Every UPDATE
writes one mutation_history row per affected bead with
mutation_type='backfill-legacy-{A|B|C}'.

Output is a JSON manifest with before/after counts and per-(category,
operator) rebind tallies — capture stdout for the audit trail. Use
--dry-run to preview without writing.`,
	RunE: runAdminBackfillOperatedByLegacy,
}

func init() {
	adminBackfillOperatedByLegacyCmd.Flags().String("target", "", "legacy operator to clean up (e.g. slack:U0OPERATOR1)")
	adminBackfillOperatedByLegacyCmd.Flags().StringSlice("statuses", []string{"open"}, "bead statuses to scan")
	adminBackfillOperatedByLegacyCmd.Flags().Int("batch-size", 200, "rows per Pass-1 batch (each batch is its own tx)")
	adminBackfillOperatedByLegacyCmd.Flags().Int("max-iterations", 5, "cap on Pass-2 parent-walk iterations")
	adminBackfillOperatedByLegacyCmd.Flags().Bool("dry-run", false, "print would-be rebinds without writing")
	_ = adminBackfillOperatedByLegacyCmd.MarkFlagRequired("target")
	adminCmd.AddCommand(adminBackfillOperatedByLegacyCmd)
}

func runAdminBackfillOperatedByLegacy(cmd *cobra.Command, _ []string) error {
	target, _ := cmd.Flags().GetString("target")
	statuses, _ := cmd.Flags().GetStringSlice("statuses")
	batchSize, _ := cmd.Flags().GetInt("batch-size")
	maxIterations, _ := cmd.Flags().GetInt("max-iterations")
	dryRun, _ := cmd.Flags().GetBool("dry-run")

	ctx := context.Background()
	start := time.Now()

	resp, err := beadsClient.BackfillOperatedByLegacy(ctx, &client.BackfillOperatedByLegacyRequest{
		Target:        target,
		Statuses:      statuses,
		BatchSize:     batchSize,
		MaxIterations: maxIterations,
		DryRun:        dryRun,
	})
	if err != nil {
		return fmt.Errorf("backfill-operated-by-legacy: %w", err)
	}

	manifest := map[string]any{
		"dry_run":                resp.DryRun,
		"target":                 resp.Target,
		"target_operator_id":     resp.TargetOperatorID,
		"before_open_candidates": resp.BeforeOpenCandidates,
		"after_open_candidates":  resp.AfterOpenCandidates,
		"category_a":             resp.CategoryA,
		"category_b":             resp.CategoryB,
		"category_c":             resp.CategoryC,
		"category_d":             resp.CategoryD,
		"rebind_count":           resp.RebindCount,
		"iterations":             resp.Iterations,
		"iterations_capped":      resp.IterationsCapped,
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
