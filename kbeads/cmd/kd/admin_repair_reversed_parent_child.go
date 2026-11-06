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

// adminRepairReversedParentChildCmd drives ka-0000006880's one-shot
// audit + repair of REVERSED parent-child edges. Some epics have
// parent-child dep rows stored backwards (bead_id=epic,
// depends_on_id=child) instead of canonical (bead_id=CHILD,
// depends_on_id=PARENT). The ParentSubtreeID recursive CTE seeds the
// subtree from `depends_on_id = <root>`, so a reversed row is invisible:
// the subtree count comes back 0 and every gardener `bead_query`
// loop_end ("open children == 0") fires immediately, killing the
// gardener DOA.
//
// The server scans the deps table for parent-child rows whose bead_id
// resolves to a type=epic bead and whose depends_on_id resolves to a
// non-epic bead, reports them, and — unless --dry-run — flips each to
// the canonical direction. Each batch is its own transaction so the run
// is resumable; the flip is idempotent (a re-run finds nothing because
// no epic remains on the bead_id side). Dry-run is the default.
var adminRepairReversedParentChildCmd = &cobra.Command{
	Use:   "repair-reversed-parent-child-deps",
	Short: "Audit/repair reversed parent-child deps (epic on child side)",
	Long: `Scans the deps table for parent-child rows stored in the REVERSED
direction — bead_id resolves to a type=epic bead while depends_on_id
resolves to a NON-epic bead — and (unless --dry-run) flips each to the
canonical direction (bead_id=CHILD, depends_on_id=PARENT).

Why this matters: the ParentSubtreeID recursive CTE seeds the subtree
from 'SELECT bead_id FROM deps WHERE depends_on_id = <root>'. A reversed
row never matches, so the subtree count comes back 0 and every gardener
bead_query loop_end ('open children == 0') fires immediately — the
gardener is DOA in ~30s with 0 runs.

Scope: deps of type='parent-child' where the bead_id bead is type=epic
and the depends_on_id bead exists and is NOT type=epic. epic->epic edges
(legitimate sub-epics) and rows with a missing/soft-deleted endpoint are
left untouched.

Repair: for each reversed row, the canonical row (child -> epic) is
inserted carrying the original created_at / created_by / metadata /
operated_by_id, then the reversed row is deleted. The insert uses
ON CONFLICT DO NOTHING; when the canonical twin already exists the
reversed row is still removed (counted under skipped_canonical_exists).

Each batch (--batch-size) is its own transaction so the run is
resumable and idempotent. --dry-run (the DEFAULT) reports the reversed
rows without writing; pass --dry-run=false to actually flip them.

Output is a JSON manifest with the reversed-row list, flip/skip tallies,
and timing — capture stdout for the audit trail.`,
	RunE: runAdminRepairReversedParentChild,
}

func init() {
	adminRepairReversedParentChildCmd.Flags().Int("batch-size", 500, "rows per page; each batch is its own tx")
	adminRepairReversedParentChildCmd.Flags().Int("max-batches", 0, "cap on number of batches (0 = drain queue)")
	adminRepairReversedParentChildCmd.Flags().Bool("dry-run", true, "report reversed rows without flipping them (default true; pass --dry-run=false to repair)")
	adminCmd.AddCommand(adminRepairReversedParentChildCmd)
}

func runAdminRepairReversedParentChild(cmd *cobra.Command, _ []string) error {
	batchSize, _ := cmd.Flags().GetInt("batch-size")
	maxBatches, _ := cmd.Flags().GetInt("max-batches")
	dryRun, _ := cmd.Flags().GetBool("dry-run")

	ctx := context.Background()
	start := time.Now()

	resp, err := beadsClient.RepairReversedParentChildDeps(ctx, &client.RepairReversedParentChildDepsRequest{
		BatchSize:  batchSize,
		MaxBatches: maxBatches,
		DryRun:     dryRun,
	})
	if err != nil {
		return fmt.Errorf("repair-reversed-parent-child-deps: %w", err)
	}

	manifest := map[string]any{
		"dry_run":                  resp.DryRun,
		"reversed_rows":            resp.ReversedRows,
		"edges_flipped":            resp.EdgesFlipped,
		"skipped_canonical_exists": resp.SkippedCanonicalExists,
		"edges":                    resp.Edges,
		"batches":                  resp.Batches,
		"batches_capped":           resp.BatchesCapped,
		"server_duration_ms":       resp.DurationMs,
		"client_wall_ms":           time.Since(start).Milliseconds(),
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(manifest); err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	return nil
}
