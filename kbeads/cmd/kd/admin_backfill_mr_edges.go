package main

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"text/tabwriter"
	"time"

	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/model"
	"github.com/spf13/cobra"
)

// adminBackfillMREdgesCmd repairs the dependency edges around existing
// type=merge-request beads (epic ka-0000007780; audit ka-0000002269).
//
// Background: a gitlab-bridge defect (root-cause ka-0000003609, fixed by
// ka-0000004161) caused handleUpdated() to re-enter on every MR-bead update
// and call upsertMRBeadAndLink(taskBeadID = the-MR-bead). The result on each
// historical MR bead is:
//
//   - bogus `produces(MR→MR)` and `triggered-by(MR→MR)` self-loops, and
//   - the genuine `produces(task→MR)` forward edge was never written.
//
// `mr-beads` already projects produces edges *from task beads that carry
// mr_url*; this command instead walks the MR beads themselves so it can also
//
//  1. remove the self-loop edges the bug left behind, and
//  2. resolve the producing task for an MR bead whose task does NOT carry
//     mr_url (the task-mr_url-missing bucket) — by, in priority order:
//     a. an existing cross `triggered-by(MR→task)` edge;
//     b. a task bead that carries this MR's mr_url (field filter);
//     c. the case-preserving ka-id parsed from the MR's source branch.
//
// and then ensure the canonical pair `produces(task→MR)` +
// `triggered-by(MR→task)` exists.
//
// Dry-run by default (--apply commits). Idempotent — re-running on already-
// healed data only reports `exists`/`noop`.
var adminBackfillMREdgesCmd = &cobra.Command{
	Use:   "mr-edges",
	Short: "Repair produces/triggered-by edges on existing merge-request beads",
	Long: `Walks type=merge-request beads and repairs their dependency edges:

  - removes bogus self-loop edges (produces/triggered-by where the MR bead
    depends on itself — the ka-0000003609 self-loop bug), and
  - ensures the canonical pair ` + "`produces(task→MR)`" + ` +
    ` + "`triggered-by(MR→task)`" + ` exists, resolving the producing task from
    (in order) an existing reverse triggered-by edge, a task carrying the
    MR's mr_url, or the case-preserving ka-id in the source branch.

Default mode is --dry-run. Pass --apply to commit. Re-runs are idempotent.`,
	RunE: runAdminBackfillMREdges,
}

func init() {
	adminBackfillMREdgesCmd.Flags().Bool("dry-run", true, "report changes without writing (default true)")
	adminBackfillMREdgesCmd.Flags().Bool("apply", false, "commit changes (overrides --dry-run=true)")
	adminBackfillMREdgesCmd.Flags().Int("page-size", 200, "MR beads per ListBeads page")
	adminBackfillMREdgesCmd.Flags().Int("limit", 0, "stop after N MR beads scanned (0 = unbounded)")
	adminBackfillMREdgesCmd.Flags().Bool("clean-self-loops", true, "remove produces/triggered-by self-loops on MR beads")
	adminBackfillCmd.AddCommand(adminBackfillMREdgesCmd)
}

// mrEdgesBeadStatuses scans every status — closed MR beads are the common
// case (merged/closed MRs) and still need their edges repaired.
var mrEdgesBeadStatuses = []string{"open", "in_progress", "blocked", "deferred", "closed"}

// mrBranchKDIDPattern extracts the first ka-id from a source branch
// (e.g. "fix/ka-AbCdEf12-foo" → "ka-AbCdEf12"). kd IDs are case-sensitive
// base62, so the matched substring is used verbatim — never lowercased.
var mrBranchKDIDPattern = regexp.MustCompile(`(?:gd|ka)-[0-9A-Za-z]{6,}`)

// mrEdgesPlan describes the proposed edge repairs for one MR bead.
type mrEdgesPlan struct {
	MRBeadID string
	MRURL    string
	// TaskBeadID is the resolved producing task, or "" when unresolved.
	TaskBeadID string
	// TaskSource explains how TaskBeadID was resolved: "triggered-by",
	// "mr_url-carrier", "source-branch", or "" (unresolved).
	TaskSource string
	// SelfLoops is the count of self-loop deps removed (or to remove).
	SelfLoops int
	// ProducesAction / TriggeredByAction is "add", "exists", or "noop".
	ProducesAction    string
	TriggeredByAction string
	// Note carries a non-fatal explanation (e.g. unresolved task).
	Note string
}

func runAdminBackfillMREdges(cmd *cobra.Command, _ []string) error {
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
	cleanSelfLoops, _ := cmd.Flags().GetBool("clean-self-loops")

	ctx := context.Background()
	start := time.Now()

	// taskCache memoizes (task-id → exists) so repeated branch/url
	// resolutions for the same task share one daemon lookup.
	taskCache := map[string]bool{}

	var (
		scanned       int
		selfLoopsGone int
		producesAdded int
		producesExist int
		trigAdded     int
		trigExist     int
		unresolved    int
		failed        int
		plans         []mrEdgesPlan
		offset        int
	)

pageLoop:
	for {
		resp, err := beadsClient.ListBeads(ctx, &client.ListBeadsRequest{
			Type:   []string{"merge-request"},
			Status: mrEdgesBeadStatuses,
			Limit:  pageSize,
			Offset: offset,
			Sort:   "created_at",
		})
		if err != nil {
			return fmt.Errorf("listing MR beads (offset=%d): %w", offset, err)
		}
		if len(resp.Beads) == 0 {
			break
		}

		for _, mr := range resp.Beads {
			scanned++

			plan, err := planMREdges(ctx, mr, cleanSelfLoops, taskCache)
			if err != nil {
				fmt.Fprintf(os.Stderr, "  !! %s: plan failed: %v\n", mr.ID, err)
				failed++
				continue
			}
			plans = append(plans, plan)

			if !dryRun {
				if err := applyMREdges(ctx, &plan, cleanSelfLoops); err != nil {
					fmt.Fprintf(os.Stderr, "  !! %s: apply failed: %v\n", mr.ID, err)
					failed++
					continue
				}
			}

			selfLoopsGone += plan.SelfLoops
			switch plan.ProducesAction {
			case "add":
				producesAdded++
			case "exists":
				producesExist++
			}
			switch plan.TriggeredByAction {
			case "add":
				trigAdded++
			case "exists":
				trigExist++
			}
			if plan.TaskBeadID == "" {
				unresolved++
			}

			if limit > 0 && scanned >= limit {
				break pageLoop
			}
		}

		offset += len(resp.Beads)
		if len(resp.Beads) < pageSize {
			break
		}
	}

	printMREdgesPlans(plans)

	verb := "would"
	if !dryRun {
		verb = "did"
	}
	fmt.Printf("\nMR beads scanned:          %d\n", scanned)
	fmt.Printf("self-loops %s remove:     %d\n", verb, selfLoopsGone)
	fmt.Printf("produces deps %s add:     %d\n", verb, producesAdded)
	fmt.Printf("produces deps existed:     %d\n", producesExist)
	fmt.Printf("triggered-by deps %s add: %d\n", verb, trigAdded)
	fmt.Printf("triggered-by deps existed: %d\n", trigExist)
	fmt.Printf("unresolved (no task):      %d\n", unresolved)
	if failed > 0 {
		fmt.Printf("failed:                    %d\n", failed)
	}
	fmt.Printf("elapsed:                   %s\n", time.Since(start).Round(time.Millisecond))

	if dryRun {
		fmt.Fprintln(os.Stderr, "\nDry-run only. Re-run with --apply to commit.")
	}
	if failed > 0 {
		return fmt.Errorf("%d MR bead(s) failed during edge backfill", failed)
	}
	return nil
}

// planMREdges inspects one MR bead's dependencies and the daemon to decide
// which self-loops to drop and which canonical edges to add. It performs no
// writes.
func planMREdges(ctx context.Context, mr *model.Bead, cleanSelfLoops bool, taskCache map[string]bool) (mrEdgesPlan, error) {
	fields := decodeFields(mr.Fields)
	mrURL, _ := fields["mr_url"].(string)
	plan := mrEdgesPlan{MRBeadID: mr.ID, MRURL: mrURL}

	deps, err := beadsClient.GetDependencies(ctx, mr.ID)
	if err != nil {
		return plan, fmt.Errorf("get deps: %w", err)
	}

	// Inventory the MR bead's own edges: self-loops to drop, and any existing
	// cross triggered-by(MR→task) we can reuse to resolve the producing task.
	var crossTriggeredTask string
	for _, d := range deps {
		self := d.DependsOnID == mr.ID
		switch {
		case self && (d.Type == "produces" || d.Type == "triggered-by"):
			if cleanSelfLoops {
				plan.SelfLoops++
			}
		case !self && d.Type == "triggered-by":
			// MR → triggered-by → task: the canonical reverse edge.
			if crossTriggeredTask == "" {
				crossTriggeredTask = d.DependsOnID
			}
		}
	}

	// Resolve the producing task in priority order.
	switch {
	case crossTriggeredTask != "":
		plan.TaskBeadID = crossTriggeredTask
		plan.TaskSource = "triggered-by"
	default:
		if mrURL != "" {
			if t, err := taskCarryingMRURL(ctx, mrURL, mr.ID); err != nil {
				return plan, err
			} else if t != "" {
				plan.TaskBeadID = t
				plan.TaskSource = "mr_url-carrier"
			}
		}
		if plan.TaskBeadID == "" {
			if t := resolveTaskFromBranch(ctx, fields, taskCache); t != "" {
				plan.TaskBeadID = t
				plan.TaskSource = "source-branch"
			}
		}
	}

	if plan.TaskBeadID == "" {
		plan.Note = "no producing task resolvable"
		plan.ProducesAction = "noop"
		plan.TriggeredByAction = "noop"
		return plan, nil
	}

	// Decide produces(task→MR): live on the task bead.
	taskDeps, err := beadsClient.GetDependencies(ctx, plan.TaskBeadID)
	if err != nil {
		return plan, fmt.Errorf("get task deps: %w", err)
	}
	plan.ProducesAction = "add"
	for _, d := range taskDeps {
		if d.DependsOnID == mr.ID && d.Type == "produces" {
			plan.ProducesAction = "exists"
			break
		}
	}

	// Decide triggered-by(MR→task): live on the MR bead.
	plan.TriggeredByAction = "add"
	for _, d := range deps {
		if d.DependsOnID == plan.TaskBeadID && d.Type == "triggered-by" {
			plan.TriggeredByAction = "exists"
			break
		}
	}

	return plan, nil
}

// applyMREdges performs the writes described by plan: removes self-loops and
// adds the missing canonical edges. Best-effort per write; returns the first
// hard error.
func applyMREdges(ctx context.Context, plan *mrEdgesPlan, cleanSelfLoops bool) error {
	if cleanSelfLoops && plan.SelfLoops > 0 {
		for _, t := range []string{"produces", "triggered-by"} {
			// RemoveDependency is a no-op when the edge is absent, so removing
			// both candidate self-loop types unconditionally is safe — at most
			// one of the two types is actually present, so the rows-affected
			// signal is deliberately discarded here.
			if _, err := beadsClient.RemoveDependency(ctx, plan.MRBeadID, plan.MRBeadID, t); err != nil {
				return fmt.Errorf("remove self-loop %s on %s: %w", t, plan.MRBeadID, err)
			}
		}
	}
	if plan.TaskBeadID == "" {
		return nil
	}
	// Attribute both edges to the invoking human, matching every other
	// AddDependency caller (create/dep/reparent). The server ignores this
	// field anyway (AUDIT §C3: created_by is derived from the auth identity,
	// operated_by from resolveAuditOperatorRequired), so the old hardcoded
	// "system:ka-admin-backfill-mr-edges" was both dead and misleading — it
	// read as a system-operator stamp the daemon never honors (ka-0000007735
	// / epic ka-0000004835).
	if plan.ProducesAction == "add" {
		if _, err := beadsClient.AddDependency(ctx, &client.AddDependencyRequest{
			BeadID:      plan.TaskBeadID,
			DependsOnID: plan.MRBeadID,
			Type:        "produces",
			CreatedBy:   actor,
		}); err != nil {
			return fmt.Errorf("add produces %s → %s: %w", plan.TaskBeadID, plan.MRBeadID, err)
		}
	}
	if plan.TriggeredByAction == "add" {
		if _, err := beadsClient.AddDependency(ctx, &client.AddDependencyRequest{
			BeadID:      plan.MRBeadID,
			DependsOnID: plan.TaskBeadID,
			Type:        "triggered-by",
			CreatedBy:   actor,
		}); err != nil {
			return fmt.Errorf("add triggered-by %s → %s: %w", plan.MRBeadID, plan.TaskBeadID, err)
		}
	}
	return nil
}

// taskCarryingMRURL returns the id of a non-MR bead whose mr_url field equals
// mrURL, or "" if none. Excludes the MR bead itself (which also carries
// mr_url).
func taskCarryingMRURL(ctx context.Context, mrURL, mrBeadID string) (string, error) {
	resp, err := beadsClient.ListBeads(ctx, &client.ListBeadsRequest{
		Status:       mrEdgesBeadStatuses,
		FieldFilters: map[string]string{"mr_url": mrURL},
		Limit:        20,
	})
	if err != nil {
		return "", fmt.Errorf("list beads by mr_url: %w", err)
	}
	for _, b := range resp.Beads {
		if b.ID == mrBeadID || b.Type == "merge-request" {
			continue
		}
		return b.ID, nil
	}
	return "", nil
}

// resolveTaskFromBranch extracts a case-preserved ka-id from the MR bead's
// source-branch field and returns it if that bead exists. kd IDs are
// case-sensitive base62, so the branch substring is used verbatim.
func resolveTaskFromBranch(ctx context.Context, fields map[string]any, taskCache map[string]bool) string {
	for _, key := range []string{"source_branch", "gitlab_mr_source_branch"} {
		br, _ := fields[key].(string)
		if br == "" {
			continue
		}
		id := mrBranchKDIDPattern.FindString(br)
		if id == "" {
			continue
		}
		if exists, ok := taskCache[id]; ok {
			if exists {
				return id
			}
			continue
		}
		b, err := beadsClient.GetBead(ctx, id)
		exists := err == nil && b != nil
		taskCache[id] = exists
		if exists {
			return id
		}
	}
	return ""
}

// printMREdgesPlans renders the per-MR repair plan as an aligned table.
func printMREdgesPlans(plans []mrEdgesPlan) {
	if len(plans) == 0 {
		return
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "MR_BEAD\tTASK\tVIA\tSELFLOOPS\tPRODUCES\tTRIGGERED_BY\tNOTE")
	for _, p := range plans {
		task := p.TaskBeadID
		if task == "" {
			task = "—"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\t%s\n",
			p.MRBeadID, task, p.TaskSource, p.SelfLoops,
			p.ProducesAction, p.TriggeredByAction, p.Note)
	}
	_ = w.Flush()
}
