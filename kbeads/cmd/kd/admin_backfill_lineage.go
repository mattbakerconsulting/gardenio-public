package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"text/tabwriter"

	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/model"
	"github.com/spf13/cobra"
)

// adminBackfillLineageCmd stamps prior_agent_id + lineage_root_id onto
// HISTORICAL agent beads that predate task-shared succession stamping
// (laurels/lineage epic ka-0000006664, task ka-0000007863; this is the P3
// optional backfill ka-0000001708 hanging off research epic ka-0000006566).
//
// WHAT IT CLOSES. The spawn choke point (SpawnAgentResult →
// stampTaskPredecessorLineage, gardenio/controller/internal/beadsapi/
// task_predecessor.go) now stamps every NEW agent bead whose task_id a closed
// predecessor already worked. But the beads created BEFORE that landed carry no
// pointer — measured 770 crew agents arrived at a bead a predecessor had
// already worked and NOT ONE carried prior_agent_id (ka-0000001141 §1.3). This
// command replays the same rule over the existing corpus so a
// lineage-consuming surface (a viewer lineage strip, a `gb prime` handoff on a
// resumed old bead) has a chain to walk, not just for spawns going forward.
//
// THE RULE — identical to the spawn path. Group agent beads by task_id. Within
// a task, walk in creation order; each bead's predecessor is the most-recently
// CREATED, currently-CLOSED occupant that came before it. prior_agent_id points
// at that predecessor; lineage_root_id inherits the predecessor's root when it
// has one, else the predecessor itself becomes the root. So a whole task chain
// roots at its first closed occupant, exactly as a live chain does.
//
// WHY "CLOSED only" matches the live path: a predecessor is an occupant that
// FINISHED (has a terminal wrap-up worth reading). An open/in_progress sibling
// is either the live incumbent or a concurrent fan-out peer — not a
// predecessor. The reconstruction is necessarily approximate for history (it
// reads CURRENT status, not status-at-spawn) but it is the same positional fact
// the pointer always encoded: this bead was worked before, by that bead.
//
// BATCHED / CHECKPOINTED (the P3 framing: agent beads >> work beads). Writes go
// out in --batch-size chunks; each UpdateBead commits independently and a
// checkpoint line prints after every flush. There is no separate cursor store —
// the idempotency guard IS the checkpoint: a bead that already carries
// prior_agent_id is skipped, so a killed run resumes simply by re-running (the
// already-written batches skip, the rest proceed). --dry-run writes nothing and
// re-running after an apply yields an all-SKIP_IDEMPOTENT plan.
var adminBackfillLineageCmd = &cobra.Command{
	Use:   "lineage",
	Short: "Backfill prior_agent_id + lineage_root_id onto historical agent beads (task-shared succession)",
	Long: `Backfill task-shared succession pointers onto historical agent beads
(laurels/lineage epic ka-0000006664; P3 optional backfill ka-0000001708).

The spawn choke point (stampTaskPredecessorLineage) stamps every NEW agent bead
whose task a closed predecessor already worked. This command replays the same
rule over agent beads created before that landed, so a lineage-consuming surface
has a chain to walk over the existing corpus.

Rule (identical to the spawn path): group type=agent beads by task_id; within a
task, walk in creation order and point each bead's prior_agent_id at the
most-recently-created, currently-closed occupant before it. lineage_root_id
inherits the predecessor's root when set, else the predecessor becomes the root,
so a task chain roots at its first closed occupant.

Batched/checkpointed: writes go out in --batch-size chunks (each UpdateBead
commits independently, a checkpoint prints per flush). The idempotency guard is
the checkpoint — a bead already carrying prior_agent_id is skipped, so a killed
run resumes by re-running. --dry-run writes nothing; a re-run after an apply is
all-SKIP_IDEMPOTENT.`,
	RunE: runAdminBackfillLineage,
}

func init() {
	adminBackfillLineageCmd.Flags().Bool("dry-run", false, "show proposed changes without writing")
	adminBackfillLineageCmd.Flags().Int("limit", 0, "cap the number of agent beads scanned (0 = no cap)")
	adminBackfillLineageCmd.Flags().Int("page-size", 200, "agent beads to fetch per list page")
	adminBackfillLineageCmd.Flags().Int("batch-size", 200, "agent beads to write per checkpointed flush")
	adminBackfillCmd.AddCommand(adminBackfillLineageCmd)
}

// lineageStatus is the STATUS column value for one planned row.
type lineageStatus string

const (
	lineageWouldUpdate       lineageStatus = "WOULD_UPDATE"
	lineageUpdated           lineageStatus = "UPDATED"
	lineageSkipIdempotent    lineageStatus = "SKIP_IDEMPOTENT"
	lineageSkipNoPredecessor lineageStatus = "SKIP_NO_PREDECESSOR"
	lineageError             lineageStatus = "ERROR"
)

// lineageRow is one planned change for a single agent bead. A row with a
// non-WOULD_UPDATE status carries no patch; the patch (when present) merges the
// two flat fields via UpdateBead.
type lineageRow struct {
	BeadID      string
	TaskID      string
	PriorAgent  string // proposed prior_agent_id, "—" for skip rows
	LineageRoot string // proposed lineage_root_id, "—" for skip rows
	Status      lineageStatus
	patch       json.RawMessage // nil unless Status == WOULD_UPDATE
}

func runAdminBackfillLineage(cmd *cobra.Command, _ []string) error {
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	limit, _ := cmd.Flags().GetInt("limit")
	pageSize, _ := cmd.Flags().GetInt("page-size")
	batchSize, _ := cmd.Flags().GetInt("batch-size")
	if pageSize <= 0 {
		pageSize = 200
	}
	if batchSize <= 0 {
		batchSize = 200
	}

	ctx := context.Background()

	// Scan every agent bead that carries a task_id. The "*" field filter means
	// "present" (same shape as the gardener-owner backfill's schedule_id scan);
	// beads with no task_id can have no task-shared predecessor and are skipped
	// server-side.
	agents, err := listAllBeads(ctx, &client.ListBeadsRequest{
		Type:         []string{"agent"},
		FieldFilters: map[string]string{"task_id": "*"},
	}, pageSize, limit)
	if err != nil {
		return fmt.Errorf("listing agent beads: %w", err)
	}

	groups := groupAgentsByTask(agents)
	// Deterministic task iteration order so the printed plan (and the write
	// order) is reproducible across runs.
	taskIDs := make([]string, 0, len(groups))
	for taskID := range groups {
		taskIDs = append(taskIDs, taskID)
	}
	sort.Strings(taskIDs)

	var rows []lineageRow
	for _, taskID := range taskIDs {
		rows = append(rows, planLineageForTask(taskID, groups[taskID])...)
	}

	printLineageRows(rows, dryRun)

	if dryRun {
		printLineageSummary(rows, len(agents), len(groups), 0, 0, dryRun)
		fmt.Fprintln(os.Stderr, "\nDry-run only. Re-run without --dry-run to write prior_agent_id / lineage_root_id.")
		return nil
	}

	applied, failed := applyLineagePatches(ctx, rows, batchSize)
	printLineageSummary(rows, len(agents), len(groups), applied, failed, dryRun)
	if failed > 0 {
		return fmt.Errorf("%d update(s) failed", failed)
	}
	return nil
}

// groupAgentsByTask buckets agent beads by their task_id field. Beads with no
// task_id (defensive — the server filter guarantees presence) are dropped.
func groupAgentsByTask(agents []*model.Bead) map[string][]*model.Bead {
	groups := make(map[string][]*model.Bead)
	for _, b := range agents {
		if b == nil || b.ID == "" {
			continue
		}
		taskID, _ := decodeFields(b.Fields)["task_id"].(string)
		if taskID == "" {
			continue
		}
		groups[taskID] = append(groups[taskID], b)
	}
	return groups
}

// planLineageForTask plans the backfill for one task's agent beads. It sorts
// them in creation order (tie-break bead id ascending so the choice is stable),
// walks the chain, and returns one row per bead. Only beads that both lack a
// prior_agent_id AND have a closed predecessor before them produce a
// WOULD_UPDATE row carrying a patch; every other bead is a SKIP row.
//
// lineage_root_id propagation mirrors the spawn path: a bead's effective root
// is its own existing root, else its predecessor's effective root, else (no
// predecessor) itself — so a later bead using it as a predecessor inherits the
// chain's true root rather than a mid-chain link.
func planLineageForTask(taskID string, beads []*model.Bead) []lineageRow {
	ordered := append([]*model.Bead(nil), beads...)
	sort.SliceStable(ordered, func(i, j int) bool {
		ti, tj := ordered[i].CreatedAt, ordered[j].CreatedAt
		if !ti.Equal(tj) {
			return ti.Before(tj)
		}
		return ordered[i].ID < ordered[j].ID
	})

	// effectiveRoot[id] is the lineage-root value a downstream bead would
	// inherit if it pointed at id as its predecessor.
	effectiveRoot := make(map[string]string, len(ordered))
	var lastClosed *model.Bead // most-recently-created closed bead seen so far

	rows := make([]lineageRow, 0, len(ordered))
	for _, b := range ordered {
		fields := decodeFields(b.Fields)
		existingPrior, _ := fields["prior_agent_id"].(string)
		existingRoot, _ := fields["lineage_root_id"].(string)

		// Record this bead's effective root for any successor that points at it.
		switch {
		case existingRoot != "":
			effectiveRoot[b.ID] = existingRoot
		case lastClosed != nil:
			effectiveRoot[b.ID] = effectiveRoot[lastClosed.ID]
		default:
			effectiveRoot[b.ID] = b.ID
		}

		row := lineageRow{BeadID: b.ID, TaskID: taskID, PriorAgent: "—", LineageRoot: "—"}
		switch {
		case existingPrior != "":
			// Already stamped (prior_agent_id + lineage_root_id are written
			// together at spawn) — idempotent skip.
			row.Status = lineageSkipIdempotent
		case lastClosed == nil:
			// First occupant, or every earlier occupant is still open — no
			// finished predecessor to point at.
			row.Status = lineageSkipNoPredecessor
		default:
			priorID := lastClosed.ID
			rootID := effectiveRoot[lastClosed.ID]
			patch, err := json.Marshal(map[string]string{
				"prior_agent_id":  priorID,
				"lineage_root_id": rootID,
			})
			if err != nil {
				row.Status = lineageError
				row.PriorAgent = priorID
				row.LineageRoot = rootID
				break
			}
			row.Status = lineageWouldUpdate
			row.PriorAgent = priorID
			row.LineageRoot = rootID
			row.patch = patch
		}
		rows = append(rows, row)

		// Advance the predecessor pointer: a closed bead becomes the newest
		// finished occupant for everything created after it.
		if b.Status == model.StatusClosed {
			lastClosed = b
		}
	}
	return rows
}

// applyLineagePatches writes the WOULD_UPDATE rows via UpdateBead's field merge,
// flushing a checkpoint line every batchSize writes so a long run reports
// progress and an interrupted run has durably committed everything up to the
// last checkpoint. Returns the applied and failed counts.
func applyLineagePatches(ctx context.Context, rows []lineageRow, batchSize int) (applied, failed int) {
	pending := 0
	flush := func() {
		if pending == 0 {
			return
		}
		fmt.Printf("  checkpoint: %d applied, %d failed\n", applied, failed)
		pending = 0
	}
	for _, r := range rows {
		if r.Status != lineageWouldUpdate || len(r.patch) == 0 {
			continue
		}
		if _, err := beadsClient.UpdateBead(ctx, r.BeadID, &client.UpdateBeadRequest{
			Fields: r.patch,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "  !! %s: update failed: %v\n", r.BeadID, err)
			failed++
		} else {
			applied++
		}
		pending++
		if pending >= batchSize {
			flush()
		}
	}
	flush()
	return applied, failed
}

func countLineageStatus(rows []lineageRow, want lineageStatus) int {
	n := 0
	for _, r := range rows {
		if r.Status == want {
			n++
		}
	}
	return n
}

func printLineageRows(rows []lineageRow, dryRun bool) {
	if len(rows) == 0 {
		fmt.Println("no agent beads with a task_id found")
		return
	}
	title := "PROPOSED CHANGES"
	if !dryRun {
		title = "APPLYING CHANGES"
	}
	fmt.Printf("\n%s (%d rows)\n\n", title, len(rows))
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "BEAD_ID\tTASK_ID\tPRIOR_AGENT\tLINEAGE_ROOT\tSTATUS")
	for _, r := range rows {
		status := r.Status
		// When applying, a planned WOULD_UPDATE that we are about to write is
		// reported as UPDATED in the table.
		if !dryRun && status == lineageWouldUpdate {
			status = lineageUpdated
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", r.BeadID, r.TaskID, r.PriorAgent, r.LineageRoot, status)
	}
	w.Flush()
}

func printLineageSummary(rows []lineageRow, agents, tasks, applied, failed int, dryRun bool) {
	verb := "would update"
	if !dryRun {
		verb = "updated"
	}
	fmt.Printf("\nagent beads scanned:    %d\n", agents)
	fmt.Printf("tasks grouped:          %d\n", tasks)
	fmt.Printf("%s (rows):     %d\n", verb, countLineageStatus(rows, lineageWouldUpdate))
	fmt.Printf("skip idempotent:        %d\n", countLineageStatus(rows, lineageSkipIdempotent))
	fmt.Printf("skip no predecessor:    %d\n", countLineageStatus(rows, lineageSkipNoPredecessor))
	fmt.Printf("errors:                 %d\n", countLineageStatus(rows, lineageError))
	if !dryRun {
		fmt.Printf("applied:                %d\n", applied)
		fmt.Printf("failed:                 %d\n", failed)
	}
}
