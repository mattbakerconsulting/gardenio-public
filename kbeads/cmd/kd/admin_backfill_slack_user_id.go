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

// adminBackfillSlackUserIDCmd drives ka-0000001582's backfill of the
// missing `fields.slack_user_id` on thread-derived legacy beads. Pass-1 A
// of `backfill-operated-by-legacy` resolves a thread agent's real operator
// from `fields.slack_user_id`; investigation ka-0000000959 found 122 of 127
// thread-derived candidates had that field missing, so the heuristic caught
// zero. This command recovers the field.
//
// The originating slack_user_id is NOT re-fetched from Slack — it already
// lives on the root "Slack thread:" bead the slack-bridge creates at
// thread-open time. Each candidate references that root two ways, tried in
// order: (1) its `fields.thread_id` bead-id FK, (2) the timestamp parsed
// from `created_by = "thread-<sanitized-ts>"` matched against a root bead's
// `fields.slack_thread_ts`.
var adminBackfillSlackUserIDCmd = &cobra.Command{
	Use:   "backfill-slack-user-id",
	Short: "Backfill missing fields.slack_user_id on thread-derived legacy beads",
	Long: `Populates the missing fields.slack_user_id on thread-derived legacy
beads (created_by LIKE 'thread-%') by resolving the originating Slack
thread's user id from the root "Slack thread:" bead the slack-bridge
created — no Slack API call is made. Resolution tries, per bead:

  1. fields.thread_id  → look up that bead's fields.slack_user_id
  2. created_by ts     → match a slack-bridge root bead by
                         fields.slack_thread_ts

Each batch (--batch-size) runs in its own transaction per the migration
policy; every write records one mutation_history row
(field='slack_user_id', mutation_type='backfill-slack-user-id'). The
UPDATE re-checks the field is still empty, so re-runs are no-ops.

This unblocks Pass-1 A of backfill-operated-by-legacy (see investigation
ka-0000000959). Output is a JSON manifest with before/after missing counts
and per-strategy tallies — capture stdout for the audit trail. Use --dry-run
to preview.`,
	RunE: runAdminBackfillSlackUserID,
}

func init() {
	adminBackfillSlackUserIDCmd.Flags().StringSlice("statuses", []string{"open"}, "bead statuses to scan")
	adminBackfillSlackUserIDCmd.Flags().Int("batch-size", 200, "rows per batch (each batch is its own tx)")
	adminBackfillSlackUserIDCmd.Flags().Bool("dry-run", false, "print would-be writes without mutating")
	adminCmd.AddCommand(adminBackfillSlackUserIDCmd)
}

func runAdminBackfillSlackUserID(cmd *cobra.Command, _ []string) error {
	statuses, _ := cmd.Flags().GetStringSlice("statuses")
	batchSize, _ := cmd.Flags().GetInt("batch-size")
	dryRun, _ := cmd.Flags().GetBool("dry-run")

	ctx := context.Background()
	start := time.Now()

	resp, err := beadsClient.BackfillSlackUserID(ctx, &client.BackfillSlackUserIDRequest{
		Statuses:  statuses,
		BatchSize: batchSize,
		DryRun:    dryRun,
	})
	if err != nil {
		return fmt.Errorf("backfill-slack-user-id: %w", err)
	}

	manifest := map[string]any{
		"dry_run":                 resp.DryRun,
		"before_missing":          resp.BeforeMissing,
		"after_missing":           resp.AfterMissing,
		"updated":                 resp.Updated,
		"resolved_via_thread_id":  resp.ResolvedViaThreadID,
		"resolved_via_created_by": resp.ResolvedViaCreatedBy,
		"unresolved":              resp.Unresolved,
		"server_duration_ms":      resp.DurationMs,
		"client_wall_ms":          time.Since(start).Milliseconds(),
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(manifest); err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	return nil
}
