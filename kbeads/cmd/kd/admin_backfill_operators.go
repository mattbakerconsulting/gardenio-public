package main

import (
	"context"
	"fmt"
	"time"

	"gardenio/kbeads/internal/client"
	"github.com/spf13/cobra"
)

// adminBackfillOperatorsCmd drives the P3 operators-FK backfill.
// One POST per run; the server does the full job inline:
//
//	Pass 1 — UNION DISTINCT operated_by across beads/comments/deps/
//	         events/mutation_history; prefix-decode on the first ':';
//	         upsert operators + operator_aliases (source='migration');
//	         quarantine non-splittable values for review.
//	Pass 2 — batched UPDATE setting operated_by_id from the alias join
//	         on each audit table (deps single-shot due to small size);
//	         empty-string rows on beads/comments/deps bind to the
//	         system:legacy-pre-attribution sentinel; events &
//	         mutation_history keep operated_by_id NULL on empties.
//	Verifier — count rows where operated_by <> '' AND operated_by_id IS
//	           NULL per table — must be zero on success.
//
// Idempotent — every UPDATE filters on operated_by_id IS NULL.
// See docs/adr/human-attribution-identity-v2.md §4.3 / §4.5 / §4.6.
var adminBackfillOperatorsCmd = &cobra.Command{
	Use:   "backfill-operators",
	Short: "Rebind audit rows to operator_id (P3 of human-attribution-identity-v2)",
	Long: `Pass 1 walks DISTINCT operated_by across beads/comments/deps/events/
mutation_history, splits each value on the first ':' into (kind, subject),
and upserts operators + operator_aliases (source='migration'). Values
without a ':' are quarantined for manual review.

Pass 2 issues batched UPDATEs setting operated_by_id from the alias join,
per table. Empty-string rows on live-state tables (beads, comments, deps)
are bound to the system:legacy-pre-attribution sentinel operator (created
by migration 016). History-only tables (events, mutation_history) keep
operated_by_id NULL when operated_by was empty — we do not know who
operated those rows.

The backfill is idempotent: every UPDATE carries WHERE operated_by_id IS
NULL. Server batches internally with brief sleeps between batches per
ADR §4.6.

Use --dry-run for cost estimation (counts only, no writes). The verifier
output reports per-table counts of rows where operated_by TEXT is
populated but operated_by_id is still NULL — these must be zero on a
real run modulo any quarantined operated_by values.`,
	RunE: runAdminBackfillOperators,
}

func init() {
	adminBackfillOperatorsCmd.Flags().Bool("dry-run", false, "count rows that would be updated, but do not write")
	adminBackfillOperatorsCmd.Flags().Int("batch-size", 50000, "rows per UPDATE chunk (server clamps)")
	adminCmd.AddCommand(adminBackfillOperatorsCmd)
}

func runAdminBackfillOperators(cmd *cobra.Command, _ []string) error {
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	batchSize, _ := cmd.Flags().GetInt("batch-size")
	if batchSize <= 0 {
		batchSize = 50000
	}

	ctx := context.Background()
	start := time.Now()

	if dryRun {
		fmt.Println("dry-run: counting rows that would be updated (no writes)")
	} else {
		fmt.Printf("running operators-FK backfill (batch_size=%d) — this may take several minutes on prod-sized data\n", batchSize)
	}

	resp, err := beadsClient.BackfillOperators(ctx, &client.BackfillOperatorsRequest{
		DryRun:    dryRun,
		BatchSize: batchSize,
	})
	if err != nil {
		return fmt.Errorf("backfill-operators: %w", err)
	}

	verb := "updated"
	if resp.DryRun {
		verb = "would update"
	}

	fmt.Printf("\ndistinct operated_by values: %d\n", resp.DistinctOperatedBy)
	fmt.Printf("operators upserted:          %d\n", resp.OperatorsUpserted)
	fmt.Printf("operator_aliases upserted:   %d\n", resp.AliasesUpserted)

	if len(resp.QuarantinedOperatedBy) > 0 {
		fmt.Printf("\nquarantined (not splittable on ':'):\n")
		for _, q := range resp.QuarantinedOperatedBy {
			fmt.Printf("  %q\n", q)
		}
	}

	fmt.Printf("\nrows %s (TEXT-populated → FK):\n", verb)
	fmt.Printf("  beads:            %d\n", resp.BeadsUpdated)
	fmt.Printf("  comments:         %d\n", resp.CommentsUpdated)
	fmt.Printf("  deps:             %d\n", resp.DepsUpdated)
	fmt.Printf("  events:           %d\n", resp.EventsUpdated)
	fmt.Printf("  mutation_history: %d\n", resp.MutationsUpdated)
	totalText := resp.BeadsUpdated + resp.CommentsUpdated + resp.DepsUpdated + resp.EventsUpdated + resp.MutationsUpdated
	fmt.Printf("  ─────────────────\n")
	fmt.Printf("  total:            %d\n", totalText)

	fmt.Printf("\nrows %s (empty-string → legacy-pre-attribution sentinel):\n", verb)
	fmt.Printf("  beads:            %d\n", resp.BeadsEmptySentinel)
	fmt.Printf("  comments:         %d\n", resp.CommentsEmptySentinel)
	fmt.Printf("  deps:             %d\n", resp.DepsEmptySentinel)
	totalEmpty := resp.BeadsEmptySentinel + resp.CommentsEmptySentinel + resp.DepsEmptySentinel
	fmt.Printf("  ─────────────────\n")
	fmt.Printf("  total:            %d\n", totalEmpty)

	fmt.Printf("\nverifier (rows where operated_by <> '' AND operated_by_id IS NULL):\n")
	fmt.Printf("  beads:            %d\n", resp.VerifierBeads)
	fmt.Printf("  comments:         %d\n", resp.VerifierComments)
	fmt.Printf("  deps:             %d\n", resp.VerifierDeps)
	fmt.Printf("  events:           %d\n", resp.VerifierEvents)
	fmt.Printf("  mutation_history: %d\n", resp.VerifierMutations)
	verifTotal := resp.VerifierBeads + resp.VerifierComments + resp.VerifierDeps + resp.VerifierEvents + resp.VerifierMutations
	if !resp.DryRun {
		if verifTotal == 0 {
			fmt.Printf("  ✓ verifier clean — all TEXT-populated rows have FK bindings\n")
		} else {
			fmt.Printf("  ! verifier nonzero — %d straggler rows remain (check quarantined values above)\n", verifTotal)
		}
	}

	fmt.Printf("\nserver duration: %s  (wall: %s)\n",
		time.Duration(resp.DurationMs)*time.Millisecond,
		time.Since(start).Round(time.Millisecond))
	return nil
}
