package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/model"
	"github.com/spf13/cobra"
)

// adminMigrateMRBeadDedupCmd reconciles the existing duplicate
// type=merge-request bead sets keyed on fields.mr_url (task ka-0000005686,
// HALF-2 of ka-0000002762).
//
// HALF-1 (MR !7506) moved the per-mr_url dedup INTO the daemon so NEW
// duplicates can no longer form, and staged a NON-UNIQUE partial index
// (migration 041). But it deliberately left the existing population untouched:
// a full census (2026-08-18) measured 396 mr_urls carrying >1 canonical MR bead
// (up to 7 each), 282 disagreeing on mr_state. This command reconciles those
// sets into one surviving bead each, so migration 042 can promote the index to
// UNIQUE without hitting 23505.
//
// For each mr_url carrying >1 merge-request bead:
//
//  1. Pick the SURVIVOR with the same precedence gb mr list uses to resolve the
//     ambiguity at read time (ka-0000001223 pickCanonicalMRBead), so the sweep
//     and the live read agree on which bead speaks for the URL:
//     a. a terminal mr_outcome (merged/closed) beats a non-terminal/absent one
//     — mr_outcome is the enum the type:merge-request schema stamps at close
//     and is the strongest "this MR is done" signal the data plane holds;
//     b. else a terminal mr_state (merged/closed) beats a non-terminal one —
//     GitLab never walks an MR back to opened, so terminal is absorbing;
//     c. else the more recently updated bead wins (recency ≈ GitLab truth);
//     d. ties fall to the larger bead ID, purely for determinism.
//  2. Re-point produces / triggered-by dep edges from each loser onto the
//     survivor (both the forward edges the loser OWNS and the reverse edges that
//     POINT AT the loser), idempotently — the daemon's ON CONFLICT DO NOTHING
//     makes an already-present edge a no-op.
//  3. CLOSE each loser (if not already closed) with marker fields
//     merged_into=<survivor id> + dedup_reason, preserving its comments and any
//     deps that could not be re-pointed. Losers are NOT hard-deleted.
//
// Dry-run by default (--apply commits). Idempotent — a re-run on already-
// reconciled data finds every url at count 1 and reports 0 changes.
var adminMigrateMRBeadDedupCmd = &cobra.Command{
	Use:   "mr-bead-dedup",
	Short: "Reconcile duplicate merge-request beads per mr_url before the UNIQUE index (migration 042)",
	Long: `Deduplicate existing type=merge-request beads that share an mr_url.

For each mr_url carrying more than one merge-request bead, picks a single
survivor (terminal mr_outcome > terminal mr_state > most-recently-updated >
larger id — the same precedence gb mr list uses at read time), re-points
produces/triggered-by dep edges from the losers onto the survivor, and closes
the losers with marker fields merged_into=<survivor> + dedup_reason. Losers are
closed, never hard-deleted, so their comments and un-repointable deps survive.

This MUST run (with --apply) before migration 042 promotes the mr_url index to
UNIQUE: CREATE UNIQUE INDEX raises 23505 while duplicate sets live.

Default mode is --dry-run. Pass --apply to commit. Re-runs are idempotent.`,
	RunE: runAdminMigrateMRBeadDedup,
}

func init() {
	adminMigrateMRBeadDedupCmd.Flags().Bool("dry-run", true, "report changes without writing (default true)")
	adminMigrateMRBeadDedupCmd.Flags().Bool("apply", false, "commit changes (overrides --dry-run=true)")
	adminMigrateMRBeadDedupCmd.Flags().Int("page-size", 200, "MR beads per ListBeads page")
	adminMigrateMRBeadDedupCmd.Flags().Int("limit", 0, "stop after reconciling N duplicate mr_urls (0 = unbounded)")
	adminMigrateCmd.AddCommand(adminMigrateMRBeadDedupCmd)
}

// mrDedupStatuses scans every status. A merged MR's canonical bead is
// status=closed (type:merge-request config closes on merge/close), so the
// duplicate set for a merged MR is predominantly closed beads — they MUST be in
// scope or the unique index promotion (migration 042, which is status-blind:
// its predicate is type='merge-request' AND fields ? 'mr_url') would still find
// duplicates the reconciler skipped.
var mrDedupStatuses = []string{"open", "in_progress", "blocked", "deferred", "closed"}

// terminalMROutcomes are the mr_outcome enum values the type:merge-request
// schema stamps at close (config.go: enum {merged, closed}). Presence of either
// is the strongest "this MR is done" signal a bead carries.
var terminalMROutcomes = map[string]bool{"merged": true, "closed": true}

// terminalMRStates mirrors gb mr list's terminalMRStates (ka-0000001223): the
// GitLab MR states from which an MR never returns.
var terminalMRDedupStates = map[string]bool{"merged": true, "closed": true}

// mrDedupReason is the marker stamped on each loser's dedup_reason field.
const mrDedupReason = "duplicate merge-request bead reconciled to survivor per mr_url (ka-0000005686 HALF-2)"

// mrDedupPlan describes the reconciliation of one mr_url's duplicate set.
type mrDedupPlan struct {
	MRURL      string
	SurvivorID string
	// Losers holds the per-loser edge-repoint + close actions.
	Losers []mrDedupLoser
}

// mrDedupLoser describes what happens to one non-survivor bead.
type mrDedupLoser struct {
	LoserID string
	// AlreadyClosed is true when the loser was already status=closed (only the
	// marker fields are stamped; no status transition).
	AlreadyClosed bool
	// RepointEdges is the list of edges to re-point from loser → survivor.
	RepointEdges []mrDedupEdge
}

// mrDedupEdge is one produces/triggered-by edge to move off a loser.
type mrDedupEdge struct {
	// Forward is true for an edge the LOSER owns (loser --type--> other);
	// false for a REVERSE edge that points AT the loser (other --type--> loser).
	Forward bool
	// Other is the non-loser endpoint of the edge.
	Other string
	Type  string
}

func runAdminMigrateMRBeadDedup(cmd *cobra.Command, _ []string) error {
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

	ctx := context.Background()
	start := time.Now()

	// Enumerate all merge-request beads (paged) and group by mr_url.
	byURL, scanned, err := groupMRBeadsByURL(ctx, pageSize)
	if err != nil {
		return err
	}

	// Build a plan per duplicate url, in a stable order (sorted by url) so the
	// dry-run output and the --limit slice are deterministic across runs.
	urls := make([]string, 0, len(byURL))
	for u, beads := range byURL {
		if len(beads) > 1 {
			urls = append(urls, u)
		}
	}
	sort.Strings(urls)

	var (
		plans       []mrDedupPlan
		urlsDone    int
		losersTotal int
	)
	for _, u := range urls {
		plan, err := planMRDedup(ctx, u, byURL[u])
		if err != nil {
			return fmt.Errorf("planning %s: %w", u, err)
		}
		plans = append(plans, plan)
		losersTotal += len(plan.Losers)
		urlsDone++
		if limit > 0 && urlsDone >= limit {
			break
		}
	}

	printMRDedupPlans(plans)

	var (
		closed    int
		repointed int
		failed    int
	)
	if !dryRun {
		for _, p := range plans {
			c, r, err := applyMRDedup(ctx, &p)
			closed += c
			repointed += r
			if err != nil {
				fmt.Fprintf(os.Stderr, "  !! %s: apply failed: %v\n", p.MRURL, err)
				failed++
			}
		}
	}

	verb := "would"
	if !dryRun {
		verb = "did"
	}
	fmt.Printf("\nMR beads scanned:           %d\n", scanned)
	fmt.Printf("duplicate mr_urls:          %d\n", len(urls))
	fmt.Printf("mr_urls planned:            %d\n", len(plans))
	fmt.Printf("losers %s close:          %d\n", verb, losersTotal)
	if !dryRun {
		fmt.Printf("losers closed:              %d\n", closed)
		fmt.Printf("edges re-pointed:           %d\n", repointed)
	}
	if failed > 0 {
		fmt.Printf("failed:                     %d\n", failed)
	}
	fmt.Printf("elapsed:                    %s\n", time.Since(start).Round(time.Millisecond))

	if dryRun {
		fmt.Fprintln(os.Stderr, "\nDry-run only. Re-run with --apply to commit.")
	}
	if failed > 0 {
		return fmt.Errorf("%d mr_url(s) failed during dedup", failed)
	}
	return nil
}

// groupMRBeadsByURL pages every merge-request bead and buckets them by
// fields.mr_url. Beads with no mr_url are skipped (they cannot collide on the
// unique index, whose predicate requires fields ? 'mr_url').
func groupMRBeadsByURL(ctx context.Context, pageSize int) (map[string][]*model.Bead, int, error) {
	byURL := map[string][]*model.Bead{}
	scanned := 0
	offset := 0
	for {
		resp, err := beadsClient.ListBeads(ctx, &client.ListBeadsRequest{
			Type:   []string{"merge-request"},
			Status: mrDedupStatuses,
			Limit:  pageSize,
			Offset: offset,
			Sort:   "created_at",
		})
		if err != nil {
			return nil, scanned, fmt.Errorf("listing MR beads (offset=%d): %w", offset, err)
		}
		if len(resp.Beads) == 0 {
			break
		}
		for _, b := range resp.Beads {
			scanned++
			if u := mrURLOf(b); u != "" {
				byURL[u] = append(byURL[u], b)
			}
		}
		offset += len(resp.Beads)
		if len(resp.Beads) < pageSize {
			break
		}
	}
	return byURL, scanned, nil
}

// planMRDedup selects the survivor for one duplicate set and inventories the
// edge-repoints + closes for every loser. Performs no writes.
func planMRDedup(ctx context.Context, mrURL string, beads []*model.Bead) (mrDedupPlan, error) {
	survivor := pickSurvivorMRBead(beads)
	plan := mrDedupPlan{MRURL: mrURL, SurvivorID: survivor.ID}

	for _, b := range beads {
		if b.ID == survivor.ID {
			continue
		}
		loser := mrDedupLoser{
			LoserID:       b.ID,
			AlreadyClosed: b.Status == model.StatusClosed,
		}

		// Forward edges the loser owns (loser --produces/triggered-by--> X).
		fwd, err := beadsClient.GetDependencies(ctx, b.ID)
		if err != nil {
			return plan, fmt.Errorf("get deps of loser %s: %w", b.ID, err)
		}
		for _, d := range fwd {
			if !isMRDedupEdgeType(d.Type) {
				continue
			}
			// Drop a self-loop and any edge that would re-point onto the
			// survivor itself (it already speaks for the url); the daemon's ON
			// CONFLICT makes a duplicate a no-op, but skipping keeps the plan
			// honest and the counts meaningful.
			if d.DependsOnID == b.ID || d.DependsOnID == survivor.ID {
				continue
			}
			loser.RepointEdges = append(loser.RepointEdges, mrDedupEdge{
				Forward: true,
				Other:   d.DependsOnID,
				Type:    string(d.Type),
			})
		}

		// Reverse edges that point AT the loser (X --produces/triggered-by-->
		// loser). The common shape is a producing task's produces(task→MR) and
		// the MR's own triggered-by(MR→task) reverse — both must follow the
		// survivor.
		rev, err := beadsClient.GetReverseDependencies(ctx, b.ID)
		if err != nil {
			return plan, fmt.Errorf("get reverse deps of loser %s: %w", b.ID, err)
		}
		for _, d := range rev {
			if !isMRDedupEdgeType(d.Type) {
				continue
			}
			if d.BeadID == b.ID || d.BeadID == survivor.ID {
				continue
			}
			loser.RepointEdges = append(loser.RepointEdges, mrDedupEdge{
				Forward: false,
				Other:   d.BeadID,
				Type:    string(d.Type),
			})
		}

		plan.Losers = append(plan.Losers, loser)
	}
	return plan, nil
}

// applyMRDedup executes a plan: re-points every loser's edges onto the survivor,
// then stamps marker fields + closes each loser. Returns (losersClosed,
// edgesRepointed). Best-effort per write; the first hard error is returned but
// the successful writes stay committed (the command is idempotent, so a re-run
// finishes the rest).
func applyMRDedup(ctx context.Context, plan *mrDedupPlan) (int, int, error) {
	closed := 0
	repointed := 0
	for _, loser := range plan.Losers {
		// 1. Re-point edges onto the survivor. Add the survivor edge first
		//    (idempotent ON CONFLICT), then remove the loser edge, so a crash
		//    between the two never orphans the relationship.
		for _, e := range loser.RepointEdges {
			var addReq *client.AddDependencyRequest
			if e.Forward {
				addReq = &client.AddDependencyRequest{
					BeadID:      plan.SurvivorID,
					DependsOnID: e.Other,
					Type:        e.Type,
					CreatedBy:   actor,
				}
			} else {
				addReq = &client.AddDependencyRequest{
					BeadID:      e.Other,
					DependsOnID: plan.SurvivorID,
					Type:        e.Type,
					CreatedBy:   actor,
				}
			}
			if _, err := beadsClient.AddDependency(ctx, addReq); err != nil {
				return closed, repointed, fmt.Errorf("re-point %s edge %s (loser %s → survivor %s): %w",
					e.Type, e.Other, loser.LoserID, plan.SurvivorID, err)
			}
			// Remove the old edge off the loser.
			if e.Forward {
				if _, err := beadsClient.RemoveDependency(ctx, loser.LoserID, e.Other, e.Type); err != nil {
					return closed, repointed, fmt.Errorf("remove forward %s edge on loser %s: %w", e.Type, loser.LoserID, err)
				}
			} else {
				if _, err := beadsClient.RemoveDependency(ctx, e.Other, loser.LoserID, e.Type); err != nil {
					return closed, repointed, fmt.Errorf("remove reverse %s edge onto loser %s: %w", e.Type, loser.LoserID, err)
				}
			}
			repointed++
		}

		// 2. Stamp marker fields AND clear mr_url off the loser, in one patch.
		//
		//    merged_into + dedup_reason survive on a closed bead (they are not
		//    transient-closed keys), so this works whether the loser is open or
		//    already closed. dedup_former_mr_url preserves the URL the loser
		//    carried so the reconcile stays auditable after mr_url is cleared.
		//
		//    Clearing mr_url (delete-on-null) is what actually lets migration
		//    042 build the UNIQUE index. That index is `WHERE type =
		//    'merge-request' AND fields ? 'mr_url'` — it is NOT status-scoped
		//    (by design: a merged MR's canonical bead is itself status=closed,
		//    so the daemon dedup lookup must keep closed beads in scope, see
		//    beads_mr_dedup.go lookupMergeRequestByURL). Closing a loser
		//    therefore does NOT remove it from the index population; only
		//    removing its mr_url does. Leaving mr_url on the losers made 042
		//    hard-fail 23505 even after a "successful" reconcile
		//    (ka-0000000451). The survivor keeps the URL and answers every
		//    lookup; the loser's merged_into pointer preserves the link.
		markerFields, err := json.Marshal(map[string]any{
			"merged_into":         plan.SurvivorID,
			"dedup_reason":        mrDedupReason,
			"dedup_former_mr_url": plan.MRURL,
			"mr_url":              nil,
		})
		if err != nil {
			return closed, repointed, fmt.Errorf("marshal marker fields for loser %s: %w", loser.LoserID, err)
		}
		if _, err := beadsClient.UpdateBead(ctx, loser.LoserID, &client.UpdateBeadRequest{
			Fields: markerFields,
		}); err != nil {
			return closed, repointed, fmt.Errorf("stamp marker fields on loser %s: %w", loser.LoserID, err)
		}

		// 3. Close the loser if it is still open. Already-closed losers keep
		//    their existing closed_at/closed_by; only the marker fields above are
		//    added.
		if !loser.AlreadyClosed {
			if _, err := beadsClient.CloseBead(ctx, loser.LoserID, actor); err != nil {
				return closed, repointed, fmt.Errorf("close loser %s: %w", loser.LoserID, err)
			}
			closed++
		}
	}
	return closed, repointed, nil
}

// pickSurvivorMRBead returns the most authoritative bead in a duplicate set,
// using the precedence documented on the command. The set is never empty (the
// caller only builds plans for len > 1).
func pickSurvivorMRBead(beads []*model.Bead) *model.Bead {
	best := beads[0]
	for _, b := range beads[1:] {
		if moreAuthoritativeMRBead(b, best) {
			best = b
		}
	}
	return best
}

// moreAuthoritativeMRBead reports whether a should outrank b as the survivor.
// Precedence: terminal mr_outcome > terminal mr_state > more-recent update >
// larger id. Mirrors gb mr list's pickCanonicalMRBead (ka-0000001223) with an
// added mr_outcome tier (the sweep has the loser's full fields; the read path
// keys off mr_state), so a merged MR whose bead carries mr_outcome=merged always
// survives.
func moreAuthoritativeMRBead(a, b *model.Bead) bool {
	af := decodeFields(a.Fields)
	bf := decodeFields(b.Fields)

	aOutcome := terminalMROutcomes[strField(af, "mr_outcome")]
	bOutcome := terminalMROutcomes[strField(bf, "mr_outcome")]
	if aOutcome != bOutcome {
		return aOutcome // terminal outcome wins
	}

	aState := terminalMRDedupStates[strField(af, "mr_state")]
	bState := terminalMRDedupStates[strField(bf, "mr_state")]
	if aState != bState {
		return aState // terminal state wins
	}

	if !a.UpdatedAt.Equal(b.UpdatedAt) {
		return a.UpdatedAt.After(b.UpdatedAt) // fresher wins
	}
	return a.ID > b.ID // deterministic tie-break
}

// mrURLOf extracts fields.mr_url from a bead, or "" when absent/blank.
func mrURLOf(b *model.Bead) string {
	return strField(decodeFields(b.Fields), "mr_url")
}

func strField(f map[string]any, key string) string {
	if v, ok := f[key].(string); ok {
		return v
	}
	return ""
}

// isMRDedupEdgeType reports whether a dep type is one the sweep re-points. The
// `produces` edge has no model constant (it is a raw type string, as in
// admin_backfill_mr_edges.go); `triggered-by` is model.DepTriggeredBy.
func isMRDedupEdgeType(t model.DependencyType) bool {
	return t == "produces" || t == model.DepTriggeredBy
}

// printMRDedupPlans renders the per-url reconciliation plan as an aligned table.
func printMRDedupPlans(plans []mrDedupPlan) {
	if len(plans) == 0 {
		fmt.Println("0 duplicate mr_urls — nothing to reconcile.")
		return
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "MR_URL\tSURVIVOR\tLOSERS\tEDGES_REPOINTED")
	for _, p := range plans {
		edges := 0
		losers := make([]string, 0, len(p.Losers))
		for _, l := range p.Losers {
			edges += len(l.RepointEdges)
			tag := l.LoserID
			if l.AlreadyClosed {
				tag += "(closed)"
			}
			losers = append(losers, tag)
		}
		fmt.Fprintf(w, "%s\t%s\t%v\t%d\n", p.MRURL, p.SurvivorID, losers, edges)
	}
	_ = w.Flush()
}
