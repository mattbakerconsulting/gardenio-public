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

// mattDefaultDeployDate is the UTC timestamp at which ka-0000000269 landed
// on main (commit 96ec332ca). Every bead created at-or-after this instant
// that carries owner=slack:U0OPERATOR1 is a candidate for rebind unless its
// spawn-chain root genuinely resolves to Matt.
const mattDefaultDeployDate = "2026-05-13T18:14:39Z"

// mattDefaultTarget is the owner sentinel that ka-0000000269 stamped onto
// every spawn-path bead — Matt Baker's Slack identity.
const mattDefaultTarget = "slack:U0OPERATOR1"

// adminBackfillOwnerCmd drives ka-0000006961 (C4 of epic ka-0000004247):
// reverse the ka-0000000269 damage by re-attributing every bead whose
// `owner` text column equals the verbatim Matt default to whichever human
// the spawn-chain root actually resolves to — or to `system:unowned` when
// the walk yields nothing. Each scan page (--batch-size) runs in its own
// transaction; UPDATEs filter `WHERE owner = $target` so re-runs are
// no-ops. Output is a JSON manifest with per-bucket histogram of rebinds.
var adminBackfillOwnerCmd = &cobra.Command{
	Use:   "backfill-owner",
	Short: "Rebind beads stamped with the Matt-default owner",
	Long: `Re-attributes beads currently carrying owner=<--target> (default
slack:U0OPERATOR1 — Matt) to their real human owner using the C1
spawn-chain-root resolver:

  - resolve the bead's creator (created_by + thread-spawn fallback)
  - walk fields.spawned_by until a human identity is reached
  - if that human == --target: leave alone (KeptMatt bucket)
  - else: rebind owner to that human
  - if no human is found: rebind to --unowned-sentinel (default
    "system:unowned")

The scan is keyset-paginated by id; each batch is its own transaction.
Every UPDATE writes one mutation_history row per affected bead with
mutation_type='backfill-owner-matt-default'. Re-runs on already-cleaned
data are no-ops because the WHERE clause filters by the original target.

Output is a JSON manifest with before/after counts and per-(new-owner)
rebind tallies — capture stdout for the audit trail. Use --dry-run to
preview without writing.`,
	RunE: runAdminBackfillOwner,
}

func init() {
	adminBackfillOwnerCmd.Flags().String("target", mattDefaultTarget, "owner sentinel to rebind from (e.g. slack:U0OPERATOR1)")
	adminBackfillOwnerCmd.Flags().String("since", mattDefaultDeployDate, "only scan beads created at-or-after this RFC3339 timestamp (ka-0000000269 deploy date)")
	adminBackfillOwnerCmd.Flags().Int("batch-size", 5000, "rows per scan page (each page is its own tx)")
	adminBackfillOwnerCmd.Flags().Int("max-batches", 0, "cap on number of scan pages (0 = no cap)")
	adminBackfillOwnerCmd.Flags().String("unowned-sentinel", "system:unowned", "owner value when spawn-chain walk finds no human")
	adminBackfillOwnerCmd.Flags().Bool("rebind-matt-default", false, "pin --target to slack:U0OPERATOR1 and --since to the ka-0000000269 deploy date")
	adminBackfillOwnerCmd.Flags().Bool("dry-run", false, "print would-be rebinds without writing")
	adminCmd.AddCommand(adminBackfillOwnerCmd)
}

func runAdminBackfillOwner(cmd *cobra.Command, _ []string) error {
	target, _ := cmd.Flags().GetString("target")
	sinceStr, _ := cmd.Flags().GetString("since")
	batchSize, _ := cmd.Flags().GetInt("batch-size")
	maxBatches, _ := cmd.Flags().GetInt("max-batches")
	unowned, _ := cmd.Flags().GetString("unowned-sentinel")
	rebindMatt, _ := cmd.Flags().GetBool("rebind-matt-default")
	dryRun, _ := cmd.Flags().GetBool("dry-run")

	if rebindMatt {
		target = mattDefaultTarget
		sinceStr = mattDefaultDeployDate
	}

	var since time.Time
	if sinceStr != "" {
		t, err := time.Parse(time.RFC3339, sinceStr)
		if err != nil {
			return fmt.Errorf("--since: parse %q as RFC3339: %w", sinceStr, err)
		}
		since = t
	}

	ctx := context.Background()
	start := time.Now()

	resp, err := beadsClient.BackfillOwner(ctx, &client.BackfillOwnerRequest{
		Target:          target,
		Since:           since,
		BatchSize:       batchSize,
		MaxBatches:      maxBatches,
		UnownedSentinel: unowned,
		DryRun:          dryRun,
	})
	if err != nil {
		return fmt.Errorf("backfill-owner: %w", err)
	}

	manifest := map[string]any{
		"dry_run":            resp.DryRun,
		"target":             resp.Target,
		"since":              resp.Since,
		"before_candidates":  resp.BeforeCandidates,
		"after_candidates":   resp.AfterCandidates,
		"candidate_beads":    resp.CandidateBeads,
		"rebind_count":       resp.RebindCount,
		"kept_matt_count":    resp.KeptMattCount,
		"unresolved_count":   resp.UnresolvedCount,
		"batches":            resp.Batches,
		"batches_capped":     resp.BatchesCapped,
		"last_id":            resp.LastID,
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
