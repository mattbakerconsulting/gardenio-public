package main

import (
	"context"
	"fmt"
	"time"

	"gardenio/kbeads/internal/client"
	"github.com/spf13/cobra"
)

// adminBackfillCategoryCmd drives the beads.category materialization backfill
// (ka-0000007274, follow-up to migration 043). It repeatedly calls
// POST /v1/admin/backfill-category with a cursor (after_id) until the server
// reports Done=true. Idempotent and resumable: the cursor advances only on a
// successful page, and each page's guarded UPDATE skips rows already carrying
// the correct category.
//
// Run once after deploying migration 043_beads_category_artifact. New and
// updated beads are stamped by the migration-020 trigger; this sweeps the
// legacy rows whose stored category pre-dates the migration-043 rule change —
// chiefly the deliverable/artifact beads (bundle/dashboard/rich-report/mr/
// merge-request) written between migration 020 and 043 that carry a stale
// `intermediate`. Each page is a single UPDATE in its own transaction (no
// table-locking eager backfill — see kbeads/CLAUDE.md "Migration policy").
var adminBackfillCategoryCmd = &cobra.Command{
	Use:   "backfill-category",
	Short: "Backfill the beads.category materialized column (migration 043)",
	Long: `Walks beads in keyset-paginated pages and re-stamps the beads.category
materialized column on legacy rows whose stored category disagrees with the
current derive_bead_category rule table. Each page is a single UPDATE in its own
transaction (no table-locking eager backfill). Idempotent and resumable: the
cursor advances only on success and rows already carrying the correct category
are skipped, so re-running after an interruption is safe.

Run once after deploying migration 043_beads_category_artifact. New and updated
beads are kept current by the migration's trigger; this sweeps the pre-existing
rows — mainly deliverable beads (bundle/dashboard/rich-report/mr/merge-request)
stamped 'intermediate' before the 'artifact' rule landed.`,
	RunE: runAdminBackfillCategory,
}

func init() {
	adminBackfillCategoryCmd.Flags().Int("batch-size", 500, "beads per page (server caps internally)")
	adminBackfillCategoryCmd.Flags().String("after", "", "resume cursor: skip beads with id <= this value")
	adminCmd.AddCommand(adminBackfillCategoryCmd)
}

func runAdminBackfillCategory(cmd *cobra.Command, _ []string) error {
	batchSize, _ := cmd.Flags().GetInt("batch-size")
	if batchSize <= 0 {
		batchSize = 500
	}
	afterID, _ := cmd.Flags().GetString("after")

	ctx := context.Background()
	start := time.Now()
	var totalBeads, pages int

	for {
		resp, err := beadsClient.BackfillCategory(ctx, &client.BackfillCategoryRequest{
			AfterID:   afterID,
			BatchSize: batchSize,
		})
		if err != nil {
			return fmt.Errorf("backfill-category page (after=%q): %w", afterID, err)
		}
		pages++
		totalBeads += resp.BeadsScanned

		fmt.Printf("page %d: restamped=%d cursor=%s\n", pages, resp.BeadsScanned, resp.LastID)

		if resp.Done {
			fmt.Printf("\ndone in %s: pages=%d beads_restamped=%d\n",
				time.Since(start).Round(time.Millisecond), pages, totalBeads)
			return nil
		}

		// A non-final page that restamped nothing AND did not advance the
		// cursor would loop forever — guard defensively. (resp.LastID is empty
		// only when the page was empty, which the server reports as Done=true,
		// so this is belt-and-suspenders.)
		if resp.LastID == "" {
			return fmt.Errorf("server returned a non-final page with no cursor (after=%q); aborting", afterID)
		}
		afterID = resp.LastID
	}
}
