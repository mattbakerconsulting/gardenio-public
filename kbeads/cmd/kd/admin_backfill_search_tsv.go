package main

import (
	"context"
	"fmt"
	"time"

	"gardenio/kbeads/internal/client"
	"github.com/spf13/cobra"
)

// adminBackfillSearchTSVCmd drives the search_tsv FTS backfill loop
// (ka-0000003832 / ka-search relevance Option 2; fields extension
// ka-0000005839). It repeatedly calls POST /v1/admin/backfill-search-tsv with a
// cursor (after_id) until the server reports Done=true. Idempotent. Safe to
// re-run after a transient failure: the cursor advances only on a successful
// page, and each page's guarded UPDATE skips rows that are already correct.
//
// Default mode: run once after deploying migration 039_beads_search_tsv. New
// and updated beads are filled by the migration's trigger; this command sweeps
// the legacy rows that pre-date the migration (which the no-eager-backfill
// policy keeps out of the migration transaction — see kbeads/CLAUDE.md
// "Migration policy").
//
// --reindex mode: run once after deploying migration 040_beads_search_tsv_fields.
// 040 extends the vector to index a decision's context/options free text, but
// the already-filled rows carry the old title+description-only vector; this
// refreshes the context/options-bearing rows so their vectors pick up the new
// fields.
var adminBackfillSearchTSVCmd = &cobra.Command{
	Use:   "backfill-search-tsv",
	Short: "Backfill/reindex the search_tsv FTS column (migrations 039/040)",
	Long: `Walks beads in keyset-paginated pages and maintains the search_tsv FTS
column so ka-search FTS ranking covers them. Each page is a single UPDATE in
its own transaction (no table-locking eager backfill). Idempotent and
resumable: the cursor advances only on success and rows that are already
correct are skipped, so re-running after an interruption is safe.

Default: fills beads whose search_tsv is NULL (legacy rows pre-dating migration
039) from title + description. New and updated beads are kept current by the
migration's trigger; this only fills the pre-existing rows.

--reindex: refreshes the vector on beads carrying a context/options field so
they pick up migration 040's fields extension — decision
context + option label/short text. Those rows are already non-NULL, so the
default NULL-fill never touches them; run this once after deploying 040.`,
	RunE: runAdminBackfillSearchTSV,
}

func init() {
	adminBackfillSearchTSVCmd.Flags().Int("batch-size", 500, "beads per page (server caps internally)")
	adminBackfillSearchTSVCmd.Flags().String("after", "", "resume cursor: skip beads with id <= this value")
	adminBackfillSearchTSVCmd.Flags().Bool("reindex", false, "refresh context/options-bearing rows for migration 040 (instead of filling NULL legacy rows)")
	adminCmd.AddCommand(adminBackfillSearchTSVCmd)
}

func runAdminBackfillSearchTSV(cmd *cobra.Command, _ []string) error {
	batchSize, _ := cmd.Flags().GetInt("batch-size")
	if batchSize <= 0 {
		batchSize = 500
	}
	afterID, _ := cmd.Flags().GetString("after")
	reindex, _ := cmd.Flags().GetBool("reindex")

	ctx := context.Background()
	start := time.Now()
	var totalBeads, pages int

	for {
		resp, err := beadsClient.BackfillSearchTSV(ctx, &client.BackfillSearchTSVRequest{
			AfterID:   afterID,
			BatchSize: batchSize,
			Reindex:   reindex,
		})
		if err != nil {
			return fmt.Errorf("backfill-search-tsv page (after=%q): %w", afterID, err)
		}
		pages++
		totalBeads += resp.BeadsScanned

		// "filled" (NULL legacy rows) vs "reindexed" (040 fields refresh).
		verb := "filled"
		if reindex {
			verb = "reindexed"
		}
		fmt.Printf("page %d: %s=%d cursor=%s\n", pages, verb, resp.BeadsScanned, resp.LastID)

		if resp.Done {
			if reindex {
				fmt.Printf("\ndone in %s: pages=%d beads_reindexed=%d\n",
					time.Since(start).Round(time.Millisecond), pages, totalBeads)
			} else {
				fmt.Printf("\ndone in %s: pages=%d beads_filled=%d remaining_null=%d\n",
					time.Since(start).Round(time.Millisecond),
					pages, totalBeads, resp.Remaining)
			}
			return nil
		}

		// A non-final page that filled nothing AND did not advance the cursor
		// would loop forever — guard defensively. (resp.LastID is empty only
		// when the page was empty, which the server reports as Done=true, so
		// this is belt-and-suspenders.)
		if resp.LastID == "" {
			return fmt.Errorf("server returned a non-final page with no cursor (after=%q); aborting", afterID)
		}
		afterID = resp.LastID
	}
}
