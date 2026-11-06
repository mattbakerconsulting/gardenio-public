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

// adminBackfillEmptyOwnerCmd drives ka-0000007029: the three-phase chunked
// backfill of beads whose `owner` text column is still empty (`”` or
// NULL) after the ka-0000000269 server-side default landed. This is the
// complement of `kd admin backfill-owner` (which rebinds an already-set,
// wrong owner). Source: research ka-0000000391, report
// gardenio/docs/research/ownership-defaults.md §Recommended backfill
// sequence.
var adminBackfillEmptyOwnerCmd = &cobra.Command{
	Use:   "backfill-empty-owner",
	Short: "Fill empty owner from operated_by / created_by / sentinel (three phases)",
	Long: `Fills the owner column on beads where it is still empty ('' or NULL),
in three sequential chunked phases:

  Phase 1 — owner <- resolved operator identity (operators.kind:subject
            via beads.operated_by_id — the operated_by TEXT column was
            dropped in migration 024; skips the legacy
            "system:legacy-pre-attribution" sentinel; covers ~85-95%)
  Phase 2 — owner <- created_by   (legacy-sentinel rows with a usable
            created_by; skips pure system/bridge created_by values —
            see --phase2-exclude)
  Phase 3 — owner <- --unowned-sentinel (default "system:unowned") for
            whatever residual remains

Each phase is a chunked UPDATE ... LIMIT --batch-size loop that runs until
zero rows change; each batch is its own transaction (per kbeads/CLAUDE.md
Migration policy). Every UPDATE writes one mutation_history row per
affected bead.

Idempotent: every UPDATE filters COALESCE(owner,'') = '', so a re-run on a
fully-drained set is a no-op. --dry-run partitions the current owner=''
set into per-phase would-be counts without writing.

Output is a JSON manifest with per-phase counts — capture stdout for the
audit trail.`,
	RunE: runAdminBackfillEmptyOwner,
}

func init() {
	adminBackfillEmptyOwnerCmd.Flags().Int("batch-size", 5000, "rows per UPDATE (each batch is its own tx)")
	adminBackfillEmptyOwnerCmd.Flags().Int("max-batches", 0, "cap on batch iterations PER PHASE (0 = drain each phase)")
	adminBackfillEmptyOwnerCmd.Flags().String("unowned-sentinel", "system:unowned", "Phase-3 residual owner value")
	adminBackfillEmptyOwnerCmd.Flags().String("legacy-sentinel", "system:legacy-pre-attribution", "resolved operator identity Phase 1 excludes")
	adminBackfillEmptyOwnerCmd.Flags().StringSlice("phase2-exclude", nil, "created_by values Phase 2 skips (default: jira-bridge, system:scheduler, static)")
	adminBackfillEmptyOwnerCmd.Flags().Bool("dry-run", false, "print per-phase would-be counts without writing")
	adminCmd.AddCommand(adminBackfillEmptyOwnerCmd)
}

func runAdminBackfillEmptyOwner(cmd *cobra.Command, _ []string) error {
	batchSize, _ := cmd.Flags().GetInt("batch-size")
	maxBatches, _ := cmd.Flags().GetInt("max-batches")
	unowned, _ := cmd.Flags().GetString("unowned-sentinel")
	legacy, _ := cmd.Flags().GetString("legacy-sentinel")
	phase2Exclude, _ := cmd.Flags().GetStringSlice("phase2-exclude")
	dryRun, _ := cmd.Flags().GetBool("dry-run")

	ctx := context.Background()
	start := time.Now()

	resp, err := beadsClient.BackfillEmptyOwner(ctx, &client.BackfillEmptyOwnerRequest{
		BatchSize:                batchSize,
		MaxBatches:               maxBatches,
		UnownedSentinel:          unowned,
		LegacyOperatedBySentinel: legacy,
		Phase2Exclusions:         phase2Exclude,
		DryRun:                   dryRun,
	})
	if err != nil {
		return fmt.Errorf("backfill-empty-owner: %w", err)
	}

	manifest := map[string]any{
		"dry_run":            resp.DryRun,
		"before_empty":       resp.BeforeEmpty,
		"after_empty":        resp.AfterEmpty,
		"phase1_filled":      resp.Phase1Filled,
		"phase2_filled":      resp.Phase2Filled,
		"phase3_filled":      resp.Phase3Filled,
		"phase1_batches":     resp.Phase1Batches,
		"phase2_batches":     resp.Phase2Batches,
		"phase3_batches":     resp.Phase3Batches,
		"batches_capped":     resp.BatchesCapped,
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
