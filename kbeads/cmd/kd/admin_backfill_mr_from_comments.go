package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"text/tabwriter"
	"time"

	"gardenio/kbeads/internal/client"
	"github.com/spf13/cobra"
)

// adminBackfillMRFromCommentsCmd is the historical companion to the live
// gitlab-bridge MRCommentLinker (task ka-0000003369). The bridge handler sets
// mr_url at source for *new* comments; this command sweeps *existing* beads
// whose MR URL only ever lived in a free-text comment ("MR opened: <url>") and
// never reached the mr_url field — so no type=merge-request bead was ever
// projected for them.
//
// Per matching bead the command:
//
//  1. Skips beads that already carry mr_url (nothing to recover).
//  2. Reads the bead's comments, oldest-first, and takes the first GitLab MR
//     URL found.
//  3. Field-merges mr_url onto the bead via UpdateBead.
//
// Setting mr_url is the whole job: the gitlab-bridge GitLabSync handler (on
// beads.bead.updated) then fetches the MR and projects the MR bead + edges,
// and `kd admin backfill mr-beads` can sweep any that pre-date that path.
//
// Dry-run by default (--apply commits). Idempotent — re-running on already-
// backfilled data is a no-op (beads now carrying mr_url are skipped).
var adminBackfillMRFromCommentsCmd = &cobra.Command{
	Use:   "mr-from-comments",
	Short: "Backfill mr_url on beads from MR URLs found in their comments",
	Long: `Walks beads lacking an mr_url field, scans their comments for a GitLab
merge-request URL (the pattern agents post as "MR opened: <url>"), and
field-merges the first match onto the bead's mr_url.

This is the historical companion to the live gitlab-bridge autolinker, which
sets mr_url at source for new comments. Once mr_url is set, the gitlab-bridge
projects the type=merge-request bead and produces/triggered-by edges, the same
as ` + "`gb mr link`" + `.

Default mode is --dry-run. Pass --apply to commit. Re-runs are idempotent:
beads that already carry mr_url are skipped.`,
	RunE: runAdminBackfillMRFromComments,
}

func init() {
	adminBackfillMRFromCommentsCmd.Flags().Bool("dry-run", true, "report changes without writing (default true)")
	adminBackfillMRFromCommentsCmd.Flags().Bool("apply", false, "commit changes (overrides --dry-run=true)")
	adminBackfillMRFromCommentsCmd.Flags().Int("page-size", 200, "beads per ListBeads page")
	adminBackfillMRFromCommentsCmd.Flags().Int("limit", 0, "stop after N beads with a recoverable mr_url (0 = unbounded)")
	adminBackfillMRFromCommentsCmd.Flags().StringSlice("type", []string{"task", "bug", "feature", "epic"}, "bead types to scan (comma-separated)")
	adminBackfillCmd.AddCommand(adminBackfillMRFromCommentsCmd)
}

// mrFromCommentsStatuses scans every status — a closed bead whose MR URL was
// lost to a comment still needs the MR bead projected (the merge/review may
// post-date the close).
var mrFromCommentsStatuses = []string{"open", "in_progress", "blocked", "deferred", "closed"}

// commentMRURLPattern mirrors gardenio/controller/internal/bridge.
// commentMRURLPattern: an UNanchored GitLab MR URL matcher that pulls a URL out
// of surrounding prose, excluding trailing punctuation / wrapping characters so
// the captured URL ends cleanly at the MR IID.
var commentMRURLPattern = regexp.MustCompile(`https?://[^\s<>"'` + "`" + `)\]}]+/-/merge_requests/\d+`)

// mrFromCommentsPlan describes the proposed change for one bead.
type mrFromCommentsPlan struct {
	BeadID    string
	MRURL     string
	CommentID int64
}

func runAdminBackfillMRFromComments(cmd *cobra.Command, _ []string) error {
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	apply, _ := cmd.Flags().GetBool("apply")
	if apply {
		dryRun = false
	}
	pageSize, _ := cmd.Flags().GetInt("page-size")
	if pageSize <= 0 {
		pageSize = 200
	}
	limit, _ := cmd.Flags().GetInt("limit")
	types, _ := cmd.Flags().GetStringSlice("type")
	if len(types) == 0 {
		types = []string{"task", "bug", "feature", "epic"}
	}

	ctx := context.Background()
	start := time.Now()

	var (
		scanned    int
		alreadySet int
		recovered  int
		applied    int
		failed     int
		plans      []mrFromCommentsPlan
		offset     int
	)

pageLoop:
	for {
		resp, err := beadsClient.ListBeads(ctx, &client.ListBeadsRequest{
			Type:   types,
			Status: mrFromCommentsStatuses,
			Limit:  pageSize,
			Offset: offset,
			Sort:   "created_at",
		})
		if err != nil {
			return fmt.Errorf("listing beads (offset=%d): %w", offset, err)
		}
		if len(resp.Beads) == 0 {
			break
		}

		for _, b := range resp.Beads {
			scanned++
			fields := decodeFields(b.Fields)
			if mrURL, _ := fields["mr_url"].(string); mrURL != "" {
				alreadySet++
				continue
			}

			mrURL, commentID, err := firstMRURLInComments(ctx, b.ID)
			if err != nil {
				fmt.Fprintf(os.Stderr, "  !! %s: reading comments failed: %v\n", b.ID, err)
				failed++
				continue
			}
			if mrURL == "" {
				continue
			}
			recovered++
			plan := mrFromCommentsPlan{BeadID: b.ID, MRURL: mrURL, CommentID: commentID}
			plans = append(plans, plan)

			if !dryRun {
				if err := applyMRFromComment(ctx, plan); err != nil {
					fmt.Fprintf(os.Stderr, "  !! %s: apply failed: %v\n", b.ID, err)
					failed++
					continue
				}
				applied++
			}

			if limit > 0 && recovered >= limit {
				break pageLoop
			}
		}

		offset += len(resp.Beads)
		if len(resp.Beads) < pageSize {
			break
		}
	}

	printMRFromCommentsPlans(plans)

	verb := "would set"
	if !dryRun {
		verb = "set"
	}
	fmt.Printf("\nbeads scanned:         %d\n", scanned)
	fmt.Printf("already had mr_url:    %d\n", alreadySet)
	fmt.Printf("mr_url recoverable:    %d\n", recovered)
	fmt.Printf("mr_url %s:        %d\n", verb, applied)
	if failed > 0 {
		fmt.Printf("failed:                %d\n", failed)
	}
	fmt.Printf("elapsed:               %s\n", time.Since(start).Round(time.Millisecond))

	if dryRun {
		fmt.Fprintln(os.Stderr, "\nDry-run only. Re-run with --apply to commit.")
	}
	if failed > 0 {
		return fmt.Errorf("%d bead(s) failed during mr-from-comments backfill", failed)
	}
	return nil
}

// firstMRURLInComments returns the first GitLab MR URL found across a bead's
// comments (oldest-first) plus the originating comment id. Returns an empty
// URL when no comment contains one.
func firstMRURLInComments(ctx context.Context, beadID string) (string, int64, error) {
	comments, err := beadsClient.GetComments(ctx, beadID)
	if err != nil {
		return "", 0, err
	}
	for _, c := range comments {
		if c == nil {
			continue
		}
		if mrURL := commentMRURLPattern.FindString(c.Text); mrURL != "" {
			// Validate via the strict, anchored parser so we never persist a
			// malformed URL.
			if mrURLPattern.MatchString(mrURL) {
				return mrURL, c.ID, nil
			}
		}
	}
	return "", 0, nil
}

// applyMRFromComment field-merges mr_url onto the bead. UpdateBead merges the
// Fields patch, so unrelated fields are preserved.
func applyMRFromComment(ctx context.Context, plan mrFromCommentsPlan) error {
	patch, err := json.Marshal(map[string]string{"mr_url": plan.MRURL})
	if err != nil {
		return err
	}
	if _, err := beadsClient.UpdateBead(ctx, plan.BeadID, &client.UpdateBeadRequest{
		Fields: patch,
	}); err != nil {
		return err
	}
	return nil
}

func printMRFromCommentsPlans(plans []mrFromCommentsPlan) {
	if len(plans) == 0 {
		fmt.Println("No beads with a recoverable mr_url in comments.")
		return
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "BEAD\tCOMMENT\tMR_URL")
	for _, p := range plans {
		fmt.Fprintf(w, "%s\t%d\t%s\n", p.BeadID, p.CommentID, p.MRURL)
	}
	_ = w.Flush()
}
