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

// adminBackfillParentChildCmd drives ka-0000006775's one-shot
// reconstruction of parent-child edges over agent-authored beads. Before
// server-side auto-parent inheritance (ka-0000007224) shipped, every bead
// an agent created was orphaned by default; beads-viewer cannot draw
// "what this gardener did" / "what this crew produced" without the edge.
//
// The server scans candidate beads (created_by present, created within
// --since OR — when --include-closed — currently closed), joins to the
// author's agent bead to read fields.task_id, and inserts a parent-child
// dep when no edge exists. Each batch is its own transaction so the run
// is resumable; --dry-run reports the count of edges that would be added
// grouped by author. See gardenio/docs/research/attribution-chain-spawn-paths.md
// (Follow-up beads § P3) for context.
var adminBackfillParentChildCmd = &cobra.Command{
	Use:   "backfill-parent-child-deps",
	Short: "Reconstruct parent-child deps for agent-orphan beads",
	Long: `Adds the missing parent-child edge for agent-authored beads that
were orphaned before server-side auto-parent inheritance shipped. For each
candidate bead, the server joins to the
author's agent bead and reads fields.task_id; when set and no
parent-child edge already exists, it inserts one.

Scope (default): beads with created_by present, not soft-deleted, and
either created within --since (default 6 months) or — when
--include-closed=true (default) — currently closed regardless of age.

Skipped (counted, never written):
  - description begins with "parent(gd-…)" (already intentionally parented)
  - any outbound parent-child dep already exists
  - author has no matching agent bead (slug mismatch or alias rotation)
  - agent bead has no fields.task_id (e.g. gardener-driven role)
  - task_id refers to the bead itself (would form a self-loop)
  - referenced task bead is soft-deleted or missing

Each batch (--batch-size) is its own transaction so a mid-run failure
leaves a coherent, resumable state — re-run to pick up where the last
run left off. INSERT uses ON CONFLICT … DO NOTHING so re-running on
already-linked data is a no-op.

Output is a JSON manifest with would-be / actual edge counts grouped
by author, skip-reason tallies, and timing — capture stdout for the
audit trail. Use --dry-run to preview without writing.`,
	RunE: runAdminBackfillParentChild,
}

func init() {
	adminBackfillParentChildCmd.Flags().Duration("since", 180*24*time.Hour, "limit scan to beads created within this duration")
	adminBackfillParentChildCmd.Flags().Bool("include-closed", true, "include all closed beads regardless of age")
	adminBackfillParentChildCmd.Flags().Int("batch-size", 500, "rows per page; each batch is its own tx")
	adminBackfillParentChildCmd.Flags().Int("max-batches", 0, "cap on number of batches (0 = drain queue)")
	adminBackfillParentChildCmd.Flags().Bool("dry-run", false, "print would-be edges without writing")
	adminCmd.AddCommand(adminBackfillParentChildCmd)
}

func runAdminBackfillParentChild(cmd *cobra.Command, _ []string) error {
	since, _ := cmd.Flags().GetDuration("since")
	includeClosed, _ := cmd.Flags().GetBool("include-closed")
	batchSize, _ := cmd.Flags().GetInt("batch-size")
	maxBatches, _ := cmd.Flags().GetInt("max-batches")
	dryRun, _ := cmd.Flags().GetBool("dry-run")

	ctx := context.Background()
	start := time.Now()

	resp, err := beadsClient.BackfillParentChildDeps(ctx, &client.BackfillParentChildDepsRequest{
		SinceSeconds:  int64(since / time.Second),
		IncludeClosed: includeClosed,
		BatchSize:     batchSize,
		MaxBatches:    maxBatches,
		DryRun:        dryRun,
	})
	if err != nil {
		return fmt.Errorf("backfill-parent-child-deps: %w", err)
	}

	manifest := map[string]any{
		"dry_run":                resp.DryRun,
		"candidate_beads":        resp.CandidateBeads,
		"edges_added":            resp.EdgesAdded,
		"edges_by_author":        resp.EdgesByAuthor,
		"skipped_no_agent":       resp.SkippedNoAgent,
		"skipped_no_task_id":     resp.SkippedNoTaskID,
		"skipped_self_parent":    resp.SkippedSelfParent,
		"skipped_task_missing":   resp.SkippedTaskMissing,
		"skipped_existing_dep":   resp.SkippedExistingDep,
		"skipped_parent_mention": resp.SkippedParentMention,
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
