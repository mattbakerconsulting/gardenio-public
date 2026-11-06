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

// adminBackfillOperatorSlugsCmd drives ka-0000001149's op-<id> → readable
// slug backfill. One POST per run; the server pages through operators still
// on the opaque `op-<id>` slug and, for each that now carries a real
// (non-sentinel) email, recomputes a readable slug from the email local-part
// (matthew.baker, al.kent, …), preserving the old op-<id> slug as a
// retired operator_slug_aliases row so existing /u/op-NNN links still resolve.
//
// Run AFTER the operator-cluster merge cleanup (ka-0000006421) so it slugs the
// human survivors rather than soon-to-be-merged duplicates. Machine / sentinel
// rows (system:*, fleet:*, un-resynced slack) have no derivable readable slug
// and are left on op-<id>. Idempotent — every rename is predicate-gated on the
// current slug still being op-<id>, so a re-run is a no-op.
var adminBackfillOperatorSlugsCmd = &cobra.Command{
	Use:   "backfill-operator-slugs",
	Short: "Recompute readable slugs for op-<id> operators that now have a real email",
	Long: `Recomputes readable slugs for operators still on the opaque op-<id>
form that now carry a real (non-sentinel) email — the human survivors of the
operator-cluster merge cleanup (task ka-0000006421). For each candidate the slug is
derived from the email local-part (matthew.baker, al.kent, …); the old
op-<id> slug is preserved as a RETIRED operator_slug_aliases row so existing
/u/op-NNN links keep resolving to the same operator.

Sequence this AFTER the merge cleanup so it slugs survivors, not duplicates.

Machine / sentinel rows (system:*, fleet:*, un-resynced slack) have no
derivable readable slug and are left on op-<id>. A candidate whose readable
slug is already claimed by another operator is left on op-<id> and counted
under skipped_collision — a residual collision should not happen after the
merge cleanup and is worth surfacing.

Each candidate's rename runs in its own transaction per the migration policy;
the op-<id> predicate makes re-runs no-ops. Output is a JSON manifest with
before/after op-<id> counts and per-outcome tallies — capture stdout for the
audit trail. Use --dry-run to preview.`,
	RunE: runAdminBackfillOperatorSlugs,
}

func init() {
	adminBackfillOperatorSlugsCmd.Flags().Int("batch-size", 200, "rows per batch (each candidate's rename is its own tx)")
	adminBackfillOperatorSlugsCmd.Flags().Bool("dry-run", false, "print would-be renames without mutating")
	adminCmd.AddCommand(adminBackfillOperatorSlugsCmd)
}

func runAdminBackfillOperatorSlugs(cmd *cobra.Command, _ []string) error {
	batchSize, _ := cmd.Flags().GetInt("batch-size")
	dryRun, _ := cmd.Flags().GetBool("dry-run")

	ctx := context.Background()
	start := time.Now()

	resp, err := beadsClient.BackfillOperatorSlugs(ctx, &client.BackfillOperatorSlugsRequest{
		BatchSize: batchSize,
		DryRun:    dryRun,
	})
	if err != nil {
		return fmt.Errorf("backfill-operator-slugs: %w", err)
	}

	manifest := map[string]any{
		"dry_run":            resp.DryRun,
		"before_opaque":      resp.BeforeOpaque,
		"after_opaque":       resp.AfterOpaque,
		"renamed":            resp.Renamed,
		"skipped_collision":  resp.SkippedCollision,
		"machine":            resp.Machine,
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
