package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/model"
	"github.com/spf13/cobra"
)

// adminBackfillTriggerEdgesCmd is the D9 one-shot backfill (task ka-0000004639,
// formulas epic ka-0000007507; audit ka-0000000419). It writes the trigger-
// lineage edge that historical molecules never received — the edge the D2/D3/D4
// forward-fix now stamps on every NEW molecule but which the ~700 molecules
// poured before that landed still lack, so they render as disconnected islands
// in beads-viewer.
//
// WHAT IT REPLAYS. The command derives, for each molecule with no trigger edge,
// the SAME edge the live pour path now writes, so a backfilled historical
// molecule is indistinguishable from a freshly-poured one:
//
//   - a molecule whose spawning agent carries schedule_id  → triggered-by → schedule
//     (matches the daemon's scheduled-pour convention, e.g. ka-0000005551), and
//   - a molecule whose spawning agent carries task_id       → parent-child → gardener task
//     (matches the agent-session-pour convention, e.g. ka-0000000223).
//
// THE PIVOT is deterministic, not heuristic: molecule.created_by is the agent
// NAME; that name resolves to a type=agent bead (fields.agent), which carries
// the factual schedule_id / task_id the molecule inherited. schedule_id wins
// when both are present — a scheduled gardener's canonical trigger is its
// schedule, not the "Scheduled: …" wrapper task.
//
// SCOPED TO HIGH CONFIDENCE. Molecules whose created_by does NOT resolve to an
// agent bead (human-direct CLI invocations like "Matthew.Baker" or a slugified
// prompt, "static" daemon pours, one-off test names) get NO edge — we skip
// rather than guess, so misattribution risk is zero.
//
// IDEMPOTENT + REVERSIBLE. A molecule that already has any triggered-by /
// parent-child edge is left alone (EXISTS), so re-runs and molecules the
// forward-fix already wired are no-ops. The rollback handle is the edge's OWN
// created_by (= the run's --actor, plumbed to the X-Actor header the server
// records on the dependency row) — NOT a molecule field stamp. The molecule
// type schema (kd fields molecule) permits only formula_id / applied_vars, so a
// stamping UpdateBead 400s AFTER the edge is written, leaving a partial commit
// (that is exactly how the first D9 apply half-failed). --rollback therefore
// enumerates molecules, reads their deps, and removes any triggered-by /
// parent-child edge whose created_by matches --marker-actor. Zero writes to the
// molecule bead itself.
//
// A parent-child edge onto an already-CLOSED gardener task uses the
// X-Force-Parent-Edge override (client.WithForceParentEdge) — the closed-parent
// guard (ka-0000005756) rightly blocks grafting live work onto a dead epic, but
// here we are reconstructing the lineage of an already-closed molecule, so the
// force is correct. Nearly every historical gardener task is closed, so without
// this every parent-child backfill 409s.
//
// Dry-run by default (--apply commits). Run procedure per the D9 plan
// (/bundle/ka-0000004171): dry-run, review the per-class counts, then --apply.
var adminBackfillTriggerEdgesCmd = &cobra.Command{
	Use:   "trigger-edges",
	Short: "Backfill triggered-by/parent-child trigger edges onto historical molecules (D9)",
	Long: `Backfill trigger-lineage edges onto historical molecules that predate the
D2/D3/D4 forward-fix (D9, task ka-0000004639; audit ka-0000000419).

For each molecule with no triggered-by / parent-child edge, it resolves the
molecule's spawning agent (created_by → type=agent bead) and replays the same
edge the live pour path now writes:

  - agent carries schedule_id → triggered-by(molecule → schedule)
  - agent carries task_id      → parent-child(molecule → gardener task)

schedule_id wins when both are present. Molecules whose created_by does not
resolve to an agent bead (human-direct, static, one-off) are skipped — no guess.

A molecule that already has any trigger edge is a no-op (EXISTS). Parent-child
edges onto closed gardener tasks use X-Force-Parent-Edge (the closed-parent guard
is correct to block live work, but this is closed-molecule lineage). Rollback
identity is the edge's created_by (= --actor), not a molecule field stamp — the
molecule schema rejects extra fields.

Default mode is --dry-run. Pass --apply to commit. Pass --rollback to remove the
trigger edges whose created_by matches --marker-actor (defaults to --actor).`,
	RunE: runAdminBackfillTriggerEdges,
}

func init() {
	adminBackfillTriggerEdgesCmd.Flags().Bool("dry-run", true, "report changes without writing (default true)")
	adminBackfillTriggerEdgesCmd.Flags().Bool("apply", false, "commit changes (overrides --dry-run=true)")
	adminBackfillTriggerEdgesCmd.Flags().Bool("rollback", false, "remove trigger edges whose created_by matches --marker-actor")
	adminBackfillTriggerEdgesCmd.Flags().String("marker-actor", "", "--rollback selector: remove edges with this created_by (default: the effective --actor)")
	adminBackfillTriggerEdgesCmd.Flags().Int("page-size", 200, "beads per ListBeads page")
	adminBackfillTriggerEdgesCmd.Flags().Int("limit", 0, "stop after N molecules scanned (0 = unbounded)")
	adminBackfillCmd.AddCommand(adminBackfillTriggerEdgesCmd)
}

// triggerEdgesMoleculeStatuses scans every status — the historical gap is
// overwhelmingly closed molecules (finished scheduled/agent work).
var triggerEdgesMoleculeStatuses = []string{"open", "in_progress", "blocked", "deferred", "closed"}

// triggerEdgeStatus is the STATUS column value for one planned molecule.
type triggerEdgeStatus string

const (
	triggerWouldAdd       triggerEdgeStatus = "WOULD_ADD"
	triggerAdded          triggerEdgeStatus = "ADDED"
	triggerExists         triggerEdgeStatus = "EXISTS"          // already has a trigger edge
	triggerSkipUnresolved triggerEdgeStatus = "SKIP_UNRESOLVED" // created_by → no agent / no trigger field
	triggerSkipTargetGone triggerEdgeStatus = "SKIP_TARGET_GONE"
	triggerError          triggerEdgeStatus = "ERROR"
	triggerRolledBack     triggerEdgeStatus = "ROLLED_BACK"
)

// triggerEdgePlan is one molecule's resolved backfill.
type triggerEdgePlan struct {
	MoleculeID string
	CreatedBy  string
	// TargetID / EdgeType are the resolved trigger edge, "" when unresolved.
	TargetID string
	EdgeType string // "triggered-by" or "parent-child"
	// Class explains the inference pivot, e.g. "sched-via-agent.schedule_id".
	Class  string
	Status triggerEdgeStatus
	Note   string
}

func runAdminBackfillTriggerEdges(cmd *cobra.Command, _ []string) error {
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	apply, _ := cmd.Flags().GetBool("apply")
	if apply {
		dryRun = false
	}
	rollback, _ := cmd.Flags().GetBool("rollback")
	markerActor, _ := cmd.Flags().GetString("marker-actor")
	if markerActor == "" {
		markerActor = actor // the effective --actor is the created_by we write
	}
	pageSize, _ := cmd.Flags().GetInt("page-size")
	if pageSize <= 0 {
		pageSize = 200
	}
	limit, _ := cmd.Flags().GetInt("limit")

	ctx := context.Background()

	if rollback {
		return runTriggerEdgesRollback(ctx, markerActor, pageSize, limit, dryRun)
	}

	start := time.Now()

	// Fetch every molecule and every agent bead once, then resolve in-process —
	// no per-molecule daemon lookup for the pivot. The agent index maps an agent
	// NAME (fields.agent) to its bead(s); a name can recur across sessions, so we
	// keep the slice and pick per molecule by creation time.
	molecules, err := listAllBeads(ctx, &client.ListBeadsRequest{
		Type:   []string{"molecule"},
		Status: triggerEdgesMoleculeStatuses,
		Sort:   "created_at",
	}, pageSize, limit)
	if err != nil {
		return fmt.Errorf("listing molecules: %w", err)
	}
	agents, err := listAllBeads(ctx, &client.ListBeadsRequest{
		Type: []string{"agent"},
	}, pageSize, 0)
	if err != nil {
		return fmt.Errorf("listing agent beads: %w", err)
	}
	index := buildAgentTriggerIndex(agents)

	// targetExists memoizes GetBead so repeated schedule/task lookups share one
	// round-trip; a resolved-but-missing target is skipped (no dangling edge).
	targetExists := map[string]bool{}
	beadExists := func(id string) bool {
		if v, ok := targetExists[id]; ok {
			return v
		}
		b, err := beadsClient.GetBead(ctx, id)
		exists := err == nil && b != nil && b.ID != ""
		targetExists[id] = exists
		return exists
	}

	var (
		plans  []triggerEdgePlan
		added  int
		exists int
		failed int
	)
	for _, mol := range molecules {
		plan := resolveMoleculeTrigger(mol, index)

		// Resolved candidates need the daemon to decide EXISTS vs (WOULD_)ADD;
		// this read runs in dry-run too so the plan is accurate.
		if plan.Status == triggerWouldAdd {
			if hasTriggerEdge, derr := moleculeHasTriggerEdge(ctx, mol.ID); derr != nil {
				plan.Status = triggerError
				plan.Note = derr.Error()
			} else if hasTriggerEdge {
				plan.Status = triggerExists
			} else if !beadExists(plan.TargetID) {
				plan.Status = triggerSkipTargetGone
			}
		}

		if !dryRun && plan.Status == triggerWouldAdd {
			if err := applyTriggerEdge(ctx, &plan); err != nil {
				fmt.Fprintf(os.Stderr, "  !! %s: apply failed: %v\n", mol.ID, err)
				plan.Status = triggerError
				plan.Note = err.Error()
				failed++
			} else {
				plan.Status = triggerAdded
			}
		}

		switch plan.Status {
		case triggerWouldAdd, triggerAdded:
			added++
		case triggerExists:
			exists++
		}
		plans = append(plans, plan)
	}

	printTriggerEdgePlans(plans)
	printTriggerEdgeSummary(plans, len(molecules), len(index), added, exists, failed, dryRun)
	fmt.Printf("elapsed: %s\n", time.Since(start).Round(time.Millisecond))

	if dryRun {
		fmt.Fprintln(os.Stderr, "\nDry-run only. Re-run with --apply to write the edges.")
	}
	if failed > 0 {
		return fmt.Errorf("%d molecule(s) failed during trigger-edge backfill", failed)
	}
	return nil
}

// triggerAgentRef is the trigger-relevant slice of one agent bead.
type triggerAgentRef struct {
	ID         string
	CreatedAt  time.Time
	ScheduleID string
	TaskID     string
}

// buildAgentTriggerIndex maps an agent NAME (fields.agent) to the agent beads
// carrying it, keeping only beads that actually hold a schedule_id or task_id
// (the only ones a molecule can pivot through).
func buildAgentTriggerIndex(agents []*model.Bead) map[string][]triggerAgentRef {
	index := make(map[string][]triggerAgentRef)
	for _, a := range agents {
		if a == nil || a.ID == "" {
			continue
		}
		fields := decodeFields(a.Fields)
		name, _ := fields["agent"].(string)
		if name == "" {
			continue
		}
		schedID, _ := fields["schedule_id"].(string)
		taskID, _ := fields["task_id"].(string)
		if schedID == "" && taskID == "" {
			continue
		}
		index[name] = append(index[name], triggerAgentRef{
			ID:         a.ID,
			CreatedAt:  a.CreatedAt,
			ScheduleID: schedID,
			TaskID:     taskID,
		})
	}
	return index
}

// resolveMoleculeTrigger derives the trigger edge for one molecule, purely from
// the molecule and the agent index (no client calls). A returned status of
// triggerWouldAdd carries a TargetID/EdgeType/Class the caller then confirms
// against the daemon; every other status is terminal.
func resolveMoleculeTrigger(mol *model.Bead, index map[string][]triggerAgentRef) triggerEdgePlan {
	plan := triggerEdgePlan{MoleculeID: mol.ID, CreatedBy: mol.CreatedBy}

	name := mol.CreatedBy
	if name == "" || name == "static" {
		plan.Status = triggerSkipUnresolved
		plan.Note = "no spawning agent (static/empty created_by)"
		return plan
	}
	refs := index[name]
	if len(refs) == 0 {
		plan.Status = triggerSkipUnresolved
		plan.Note = "created_by resolves to no agent bead"
		return plan
	}

	ref := pickAgentRef(refs, mol.CreatedAt)

	// schedule_id is the canonical trigger of a scheduled pour; it wins over the
	// "Scheduled: …" wrapper task_id the same agent also carries.
	switch {
	case ref.ScheduleID != "" && ref.ScheduleID != mol.ID:
		plan.TargetID = ref.ScheduleID
		plan.EdgeType = "triggered-by"
		plan.Class = "sched-via-agent.schedule_id"
		plan.Status = triggerWouldAdd
	case ref.TaskID != "" && ref.TaskID != mol.ID:
		plan.TargetID = ref.TaskID
		plan.EdgeType = "parent-child"
		plan.Class = "agent-spawn-via-agent.task_id"
		plan.Status = triggerWouldAdd
	default:
		plan.Status = triggerSkipUnresolved
		plan.Note = "agent bead carries no usable schedule_id/task_id"
	}
	return plan
}

// pickAgentRef selects the agent bead that most narrowly PRECEDES the molecule's
// creation (the actual spawner). If none precede — reconstruction from current
// data is necessarily approximate — it falls back to the earliest bead, which
// still yields the right schedule_id for the stable scheduled-agent case.
func pickAgentRef(refs []triggerAgentRef, molCreated time.Time) triggerAgentRef {
	ordered := append([]triggerAgentRef(nil), refs...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if !ordered[i].CreatedAt.Equal(ordered[j].CreatedAt) {
			return ordered[i].CreatedAt.Before(ordered[j].CreatedAt)
		}
		return ordered[i].ID < ordered[j].ID
	})
	var chosen *triggerAgentRef
	for i := range ordered {
		if ordered[i].CreatedAt.After(molCreated) {
			break
		}
		chosen = &ordered[i]
	}
	if chosen == nil {
		return ordered[0]
	}
	return *chosen
}

// moleculeHasTriggerEdge reports whether the molecule already carries any
// outgoing triggered-by / parent-child edge (forward-fix or a prior run). Such
// molecules are left untouched — we never add a second trigger edge.
func moleculeHasTriggerEdge(ctx context.Context, molID string) (bool, error) {
	deps, err := beadsClient.GetDependencies(ctx, molID)
	if err != nil {
		return false, fmt.Errorf("get deps: %w", err)
	}
	for _, d := range deps {
		if d.Type == "triggered-by" || d.Type == "parent-child" {
			return true, nil
		}
	}
	return false, nil
}

// applyTriggerEdge writes the resolved edge. The edge's created_by (= --actor,
// set on the X-Actor header at client init) is the rollback handle — there is no
// molecule field stamp, because the molecule schema rejects extra fields. A
// parent-child edge uses X-Force-Parent-Edge to override the closed-parent guard
// (ka-0000005756): historical gardener tasks are overwhelmingly closed, and this
// is closed-molecule lineage reconstruction, not live re-parenting. On success
// the plan status becomes triggerAdded.
func applyTriggerEdge(ctx context.Context, plan *triggerEdgePlan) error {
	addCtx := ctx
	if plan.EdgeType == "parent-child" {
		addCtx = client.WithForceParentEdge(ctx)
	}
	if _, err := beadsClient.AddDependency(addCtx, &client.AddDependencyRequest{
		BeadID:      plan.MoleculeID,
		DependsOnID: plan.TargetID,
		Type:        plan.EdgeType,
		CreatedBy:   actor,
	}); err != nil {
		return fmt.Errorf("add %s %s → %s: %w", plan.EdgeType, plan.MoleculeID, plan.TargetID, err)
	}
	return nil
}

// runTriggerEdgesRollback reverses a prior run by edge PROVENANCE, not a molecule
// stamp: it enumerates every molecule, reads its dependencies, and removes each
// triggered-by / parent-child edge whose created_by == markerActor. This mirrors
// how apply records provenance (the edge's own created_by = --actor), so a run
// applied under --actor X is fully reversible with --rollback --marker-actor X
// (or the same --actor) and never touches an edge some other actor authored.
func runTriggerEdgesRollback(ctx context.Context, markerActor string, pageSize, limit int, dryRun bool) error {
	start := time.Now()
	if markerActor == "" {
		return fmt.Errorf("rollback needs a created_by selector: pass --marker-actor or --actor")
	}
	molecules, err := listAllBeads(ctx, &client.ListBeadsRequest{
		Type:   []string{"molecule"},
		Status: triggerEdgesMoleculeStatuses,
		Sort:   "created_at",
	}, pageSize, limit)
	if err != nil {
		return fmt.Errorf("listing molecules: %w", err)
	}

	var plans []triggerEdgePlan
	var removed, failed int
	for _, mol := range molecules {
		deps, err := beadsClient.GetDependencies(ctx, mol.ID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  !! %s: get deps failed: %v\n", mol.ID, err)
			plans = append(plans, triggerEdgePlan{MoleculeID: mol.ID, CreatedBy: mol.CreatedBy, Class: "rollback", Status: triggerError, Note: err.Error()})
			failed++
			continue
		}
		for _, d := range deps {
			if d.CreatedBy != markerActor {
				continue
			}
			if d.Type != model.DepTriggeredBy && d.Type != model.DepParentChild {
				continue
			}
			plan := triggerEdgePlan{
				MoleculeID: mol.ID,
				CreatedBy:  mol.CreatedBy,
				EdgeType:   string(d.Type),
				TargetID:   d.DependsOnID,
				Class:      "rollback",
				Status:     triggerRolledBack,
			}
			if !dryRun {
				if _, err := beadsClient.RemoveDependency(ctx, mol.ID, d.DependsOnID, string(d.Type)); err != nil {
					fmt.Fprintf(os.Stderr, "  !! %s: remove edge failed: %v\n", mol.ID, err)
					plan.Status = triggerError
					plan.Note = err.Error()
					failed++
					plans = append(plans, plan)
					continue
				}
			}
			removed++
			plans = append(plans, plan)
		}
	}

	printTriggerEdgePlans(plans)
	verb := "would remove"
	if !dryRun {
		verb = "removed"
	}
	fmt.Printf("\nmolecules scanned: %d\n", len(molecules))
	fmt.Printf("marker actor:      %s\n", markerActor)
	fmt.Printf("%s (edges):  %d\n", verb, removed)
	if failed > 0 {
		fmt.Printf("failed:            %d\n", failed)
	}
	fmt.Printf("elapsed:           %s\n", time.Since(start).Round(time.Millisecond))
	if dryRun {
		fmt.Fprintln(os.Stderr, "\nDry-run only. Re-run with --apply --rollback to commit the reversal.")
	}
	if failed > 0 {
		return fmt.Errorf("%d molecule(s) failed during rollback", failed)
	}
	return nil
}

func countTriggerStatus(plans []triggerEdgePlan, want triggerEdgeStatus) int {
	n := 0
	for _, p := range plans {
		if p.Status == want {
			n++
		}
	}
	return n
}

func printTriggerEdgePlans(plans []triggerEdgePlan) {
	if len(plans) == 0 {
		fmt.Println("no molecules found")
		return
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "MOLECULE\tCREATED_BY\tEDGE\tTARGET\tCLASS\tSTATUS\tNOTE")
	for _, p := range plans {
		target := p.TargetID
		if target == "" {
			target = "—"
		}
		edge := p.EdgeType
		if edge == "" {
			edge = "—"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			p.MoleculeID, orUnset(p.CreatedBy), edge, target, orUnset(p.Class), p.Status, p.Note)
	}
	_ = w.Flush()
}

func printTriggerEdgeSummary(plans []triggerEdgePlan, molecules, agentNames, added, exists, failed int, dryRun bool) {
	verb := "would add"
	if !dryRun {
		verb = "added"
	}
	// Per-class breakdown of the (would-)add rows — the review signal the D9
	// plan calls for before an --apply.
	byClass := map[string]int{}
	for _, p := range plans {
		if p.Status == triggerWouldAdd || p.Status == triggerAdded {
			byClass[p.Class]++
		}
	}
	fmt.Printf("\nmolecules scanned:      %d\n", molecules)
	fmt.Printf("agent names indexed:    %d\n", agentNames)
	fmt.Printf("%s (edges):        %d\n", verb, added)
	for _, class := range sortedClassKeys(byClass) {
		fmt.Printf("    %-34s %d\n", class, byClass[class])
	}
	fmt.Printf("already had edge:       %d\n", exists)
	fmt.Printf("skip unresolved:        %d\n", countTriggerStatus(plans, triggerSkipUnresolved))
	fmt.Printf("skip target gone:       %d\n", countTriggerStatus(plans, triggerSkipTargetGone))
	fmt.Printf("errors:                 %d\n", countTriggerStatus(plans, triggerError))
	if !dryRun {
		fmt.Printf("failed:                 %d\n", failed)
	}
}

func sortedClassKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
