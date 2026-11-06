package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gardenio/kbeads/internal/client"
	"github.com/spf13/cobra"
)

const (
	// defaultReextractBatchSize is the client default beads-per-page. The
	// server enforces a ~30s per-request deadline, and a 500-bead page (which
	// also re-reads every comment on those beads) measured 25–30s on prod —
	// sitting just under the ceiling, so any heavier page tipped over and the
	// whole walk died with an opaque HTTP 500 (ka-0000001700). 100 completes
	// cleanly with headroom; the auto-halving retry below covers the rest.
	defaultReextractBatchSize = 100
	// minReextractBatchSize is the floor the auto-halving retry will not go
	// below. If even this page times out the walk aborts with the real cause
	// rather than shrinking forever.
	minReextractBatchSize = 25
)

// adminReextractReferencesCmd drives the bead-references backfill loop. It
// repeatedly calls POST /v1/admin/reextract-references with a cursor
// (after_id) until the server reports Done=true. Idempotent. Safe to re-run
// after a transient failure: the cursor advances only on successful pages.
//
// See kbeads/docs/design-bead-references.md § 4.
var adminReextractReferencesCmd = &cobra.Command{
	Use:   "reextract-references",
	Short: "Backfill bead_references by re-running reference extraction over every bead",
	Long: `Walks every bead (and every comment), re-runs the ka-id reference
extractor on title/description/notes/comment text, and replaces the
matching rows in bead_references.

The backfill is idempotent: per-bead writes happen inside a transaction,
so re-running after a transient failure is safe. Pagination uses
keyset cursors (id > after_id ORDER BY id ASC), which is stable under
concurrent inserts during the backfill.

Run once after deploying the bead_references migration. Subsequent
inserts/updates keep references in sync via the store-side hooks.

--type narrows the walk to a single bead type. Use it to reindex
type=decision FIRST (seconds, not hours): the parked_on_decision badge
resolves parking purely through open decisions' references, and a full
~677k-bead backfill orders decision ids near the END of the keyset, so the
badge renders silent false negatives on parked rows for hours while an
unfiltered run climbs toward them (bug ka-0000005898). Run the targeted pass,
then the full unfiltered pass to finish the rest:

    kd admin reextract-references --type decision   # badge sound in seconds
    kd admin reextract-references                    # finish the long tail`,
	RunE: runAdminReextractReferences,
}

func init() {
	adminReextractReferencesCmd.Flags().Int("batch-size", defaultReextractBatchSize, "beads per page; the walk halves this and retries a page that hits the server's ~30s deadline")
	adminReextractReferencesCmd.Flags().String("after", "", "resume cursor: skip beads with id <= this value")
	adminReextractReferencesCmd.Flags().String("type", "", "narrow the backfill to a single bead type (e.g. decision); empty = all types")
	adminCmd.AddCommand(adminReextractReferencesCmd)
}

func runAdminReextractReferences(cmd *cobra.Command, _ []string) error {
	batchSize, _ := cmd.Flags().GetInt("batch-size")
	if batchSize <= 0 {
		batchSize = defaultReextractBatchSize
	}
	afterID, _ := cmd.Flags().GetString("after")
	beadType, _ := cmd.Flags().GetString("type")

	ctx := context.Background()
	start := time.Now()
	var totalBeads, totalComments, totalRefs int
	var pages int

	for {
		resp, err := beadsClient.ReextractReferences(ctx, &client.ReextractReferencesRequest{
			AfterID:   afterID,
			BatchSize: batchSize,
			Type:      beadType,
		})
		if err != nil {
			// A page that overran the server's ~30s deadline is recoverable:
			// the walk is idempotent and the cursor has not advanced, so shrink
			// the page and retry the SAME cursor rather than aborting and making
			// the operator hand-copy it out of the error string (ka-0000001700).
			if isReextractTimeout(err) && batchSize > minReextractBatchSize {
				next := batchSize / 2
				if next < minReextractBatchSize {
					next = minReextractBatchSize
				}
				fmt.Printf("page timed out at batch-size %d (after=%q); retrying at %d\n",
					batchSize, afterID, next)
				batchSize = next
				continue
			}
			return fmt.Errorf("reextract-references page (after=%q, batch-size=%d): %w", afterID, batchSize, err)
		}
		pages++
		totalBeads += resp.BeadsScanned
		totalComments += resp.CommentsScanned
		totalRefs += resp.ReferencesWritten

		fmt.Printf("page %d: beads=%d comments=%d refs=%d cursor=%s\n",
			pages, resp.BeadsScanned, resp.CommentsScanned, resp.ReferencesWritten, resp.LastID)

		if resp.Done {
			fmt.Printf("\ndone in %s: pages=%d beads_scanned=%d comments_scanned=%d references_written=%d total_references=%d\n",
				time.Since(start).Round(time.Millisecond),
				pages, totalBeads, totalComments, totalRefs, resp.TotalReferences)
			return nil
		}

		// An empty page that isn't the final one would loop forever — guard
		// defensively. The server should never return BeadsScanned=0 with
		// Done=false for a non-empty table, but if it does we abort rather
		// than spin.
		if resp.BeadsScanned == 0 {
			return fmt.Errorf("server returned an empty page with done=false (cursor=%q); aborting", afterID)
		}
		afterID = resp.LastID
	}
}

// isReextractTimeout reports whether err is the server's per-request deadline
// firing on an over-large page. The server discards the underlying
// `context deadline exceeded` cause behind a generic HTTP 500 body (it does not
// distinguish a timeout from any other extraction failure), so the client can
// only recognise the timeout by its status code + message shape. This is
// deliberately conservative: a genuine data-corruption 500 would also match,
// but retrying it at a smaller batch is harmless — it re-fails on the same bead
// and the walk aborts with the real cause once the floor is hit.
func isReextractTimeout(err error) bool {
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.StatusCode == 500
}
