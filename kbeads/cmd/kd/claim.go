package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"

	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/model"
	"github.com/spf13/cobra"
)

var claimForce bool

var claimCmd = &cobra.Command{
	Use:     "claim <id>",
	Short:   "Claim a bead by assigning it to yourself",
	GroupID: "workflow",
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id := args[0]
		ctx := context.Background()

		if !claimForce {
			// Enforce single-claim limit: agent must not already have a claimed bead.
			// The target id is passed so a molecule STEP the agent must drive
			// sequentially is exempted when its molecule descends from (or was
			// triggered/sourced by) the held work item — see checkNoClaimed.
			if err := checkNoClaimed(ctx, id); err != nil {
				return err
			}

			// Pre-claim validation: check type, status, and current assignee.
			if err := checkClaimable(ctx, id); err != nil {
				return err
			}

			// Enforce project-scoped claiming: bead must belong to agent's project.
			if err := checkProjectMatch(ctx, id); err != nil {
				return err
			}
		}

		// Auto-link M3 (ka-0000003164): if the controller spawned this agent
		// with BOAT_TRIGGER_BEAD_ID set, propagate it as X-Trigger-Bead on the
		// claim so the server can write a triggered-by dep when no parent
		// dep exists yet. Best-effort — header validation lives server-side.
		if tb := strings.TrimSpace(os.Getenv("BOAT_TRIGGER_BEAD_ID")); tb != "" && tb != id {
			ctx = client.WithTriggerBead(ctx, tb)
		}

		inProgress := "in_progress"
		bead, err := beadsClient.UpdateBead(ctx, id, &client.UpdateBeadRequest{
			Assignee: &actor,
			Status:   &inProgress,
		})
		if err != nil {
			return fmt.Errorf("claiming bead %s: %w", id, err)
		}

		if jsonOutput {
			printBeadJSON(bead)
		} else {
			// Terse single-line success by default (ka-0000005647): a full
			// bead-table dump on every claim taught agents to defensively pipe
			// `kd claim` through `2>/dev/null`/`tail`, the same error-masking
			// habit that silently ate a glab failure in the audited session.
			// Use --json for the structured form. The advice diff below still
			// prints to stderr, so `2>/dev/null` is no longer needed to quiet
			// the table — and no longer eats the advice.
			fmt.Printf("Claimed %s %s\n", bead.ID, truncate(bead.Title, 40))
		}

		// Print advice diff: show any new advice items that become relevant
		// because of the claimed bead's labels (e.g., role-specific advice).
		printAdviceDiff(ctx, bead)

		return nil
	},
}

func init() {
	claimCmd.Flags().BoolVar(&claimForce, "force", false, "bypass project-scope and single-claim checks")
}

// checkNoClaimed returns an error if the actor already has an in-progress bead
// claimed. Agents should claim at most one actionable bead at a time to keep
// their focus clear and avoid work-item confusion.
//
// targetID is the bead being claimed. It is exempted from the guard when it is
// a molecule STEP the agent must drive sequentially — i.e. its molecule
// descends from (parent-child) or was triggered/sourced by (triggered-by) the
// held work item. This lets an agent spawned holding its assignment/iteration
// bead use the natural `kd claim` verb on the molecule steps poured for that
// assignment, instead of the `kd update --assignee --status=in_progress`
// workaround (formula ka-0000002084 / task ka-0000006622). If lineage can't be
// established, the guard error points at that workaround.
func checkNoClaimed(ctx context.Context, targetID string) error {
	resp, err := beadsClient.ListBeads(ctx, &client.ListBeadsRequest{
		Status:   []string{"in_progress"},
		Assignee: actor,
		Limit:    5,
	})
	if err != nil {
		// Fail open: don't block claiming if we can't check.
		return nil
	}
	// A lock bead is not work: a run takes it while holding its task, and the
	// daemon refuses the take while a live agent holds it (gd-SlLPKtUGWZ).
	if target, err := beadsClient.GetBead(ctx, targetID); err == nil && isLockBead(target) {
		return nil
	}
	for _, b := range resp.Beads {
		// Skip non-issue beads — only actionable work (kind=issue) counts.
		if b.Kind != "issue" {
			continue
		}
		// A lock this run holds is not a claimed work item either.
		if isLockBead(b) {
			continue
		}
		// Exempt a molecule step whose molecule traces back to the held bead:
		// the agent is legitimately driving the steps of a molecule poured for
		// the work it already holds.
		if isMoleculeStepOf(ctx, targetID, b.ID) {
			return nil
		}
		return fmt.Errorf("you already have claimed bead %s %q — unclaim it first with `kd unclaim %s`, or use --force to override.\n"+
			"If %s is a molecule step you must drive while holding %s, set it in progress directly instead:\n"+
			"  kd update %s --assignee %s --status=in_progress", b.ID, b.Title, b.ID, targetID, b.ID, targetID, actor)
	}
	return nil
}

// isMoleculeStepOf reports whether targetID is a step of a molecule whose
// lineage traces back to heldID. The step→molecule→held chain is:
//
//	step  --parent-child-->  molecule  --parent-child / triggered-by-->  held
//
// and the molecule also records its origin work item(s) in the
// source_bead_ids / trigger_bead_ids fields (formula_apply.go). We walk the
// target's parent-child parents to find its molecule, then check whether that
// molecule points at heldID via any of those edges/fields. Fails closed
// (returns false) on any fetch error — the guard then applies as before.
func isMoleculeStepOf(ctx context.Context, targetID, heldID string) bool {
	if targetID == "" || heldID == "" || targetID == heldID {
		return false
	}
	target, err := beadsClient.GetBead(ctx, targetID)
	if err != nil || target == nil {
		return false
	}
	// Find the target's parent-child parent(s). The molecule is the parent of
	// its step beads; a nested step's parent is another step, so walk up a
	// bounded number of hops until we reach a molecule.
	cur := target
	for range 8 {
		parentID := parentChildParent(cur)
		if parentID == "" {
			return false
		}
		parent, err := beadsClient.GetBead(ctx, parentID)
		if err != nil || parent == nil {
			return false
		}
		if parent.Type == model.TypeMolecule {
			return moleculeTracesTo(parent, heldID)
		}
		cur = parent
	}
	return false
}

// parentChildParent returns the depends_on_id of the first parent-child edge on
// b (b is the child), or "" if b has no parent-child parent. GetBead populates
// b.Dependencies with the bead's outgoing (child-first) edges.
func parentChildParent(b *model.Bead) string {
	for _, d := range b.Dependencies {
		if d == nil {
			continue
		}
		if d.BeadID == b.ID && model.IsParentChildDepType(string(d.Type)) {
			return d.DependsOnID
		}
	}
	return ""
}

// moleculeTracesTo reports whether molecule mol points at heldID via a
// parent-child or triggered-by dependency, or records heldID in its
// source_bead_ids / trigger_bead_ids provenance fields.
func moleculeTracesTo(mol *model.Bead, heldID string) bool {
	for _, d := range mol.Dependencies {
		if d == nil || d.BeadID != mol.ID || d.DependsOnID != heldID {
			continue
		}
		if model.IsParentChildDepType(string(d.Type)) || d.Type == model.DepTriggeredBy {
			return true
		}
	}
	for _, field := range []string{"source_bead_ids", "trigger_bead_ids"} {
		if slices.Contains(beadFieldStringSlice(mol.Fields, field), heldID) {
			return true
		}
	}
	return false
}

// beadFieldStringSlice extracts fields[key] from a bead's raw Fields JSON as a
// []string, tolerating both a JSON array and a single string value. Returns nil
// on any parse miss — callers treat that as "no such provenance".
func beadFieldStringSlice(raw json.RawMessage, key string) []string {
	if len(raw) == 0 {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil
	}
	v, ok := fields[key]
	if !ok {
		return nil
	}
	var slice []string
	if err := json.Unmarshal(v, &slice); err == nil {
		return slice
	}
	var single string
	if err := json.Unmarshal(v, &single); err == nil && single != "" {
		return []string{single}
	}
	return nil
}

// checkClaimable validates that the target bead can be claimed: not an epic,
// not closed/deferred, and not already claimed by a different actor.
func checkClaimable(ctx context.Context, beadID string) error {
	bead, err := beadsClient.GetBead(ctx, beadID)
	if err != nil {
		return nil // Fail open if we can't fetch the bead.
	}
	if bead.Type == "epic" {
		return fmt.Errorf("cannot claim an epic — epics are planning containers, not actionable work.\nClaim an individual task, feature, or bug from this epic instead.\nUse `kd dep list %s` to see the child work items, or `--force` to override.", beadID)
	}
	if bead.Status == "closed" || bead.Status == "deferred" {
		return fmt.Errorf("cannot claim bead %s — status is %q. Only open or in_progress beads can be claimed, or use --force to override.", beadID, bead.Status)
	}
	// A held lock's holder may be dead; the daemon decides whether it is
	// takeable and refuses a live holder (internal/server/lock_bead.go).
	if bead.Assignee != "" && bead.Assignee != actor && !isLockBead(bead) {
		return fmt.Errorf("bead %s is already claimed by %q — unclaim it first with `kd unclaim %s`, or use --force to override", beadID, bead.Assignee, beadID)
	}
	return nil
}

// checkProjectMatch verifies that the target bead belongs to the same project
// as the actor. The agent's project is read from the BOAT_PROJECT env var.
// Beads carry their project in a "project:<name>" label.
// If no project is configured for the agent, the check is skipped.
func checkProjectMatch(ctx context.Context, beadID string) error {
	agentProject := agentProject()
	if agentProject == "" {
		// A laptop operator legitimately runs kd with no project, so this is a
		// skip rather than an error — but say so, because the guard silently
		// doing nothing is how a cross-project claim slips through unnoticed.
		fmt.Fprintln(os.Stderr, "kd claim: no project configured (KD_PROJECT/BOAT_PROJECT); cross-project check skipped")
		return nil
	}

	bead, err := beadsClient.GetBead(ctx, beadID)
	if err != nil {
		// Fail CLOSED: a daemon fault is not evidence that the bead is in our
		// project. Use --force to claim anyway.
		return fmt.Errorf("cannot verify the project of bead %s (%w) — retry, or use --force to override", beadID, err)
	}

	// Find project:<name> label on the target bead.
	beadProject := ""
	for _, label := range bead.Labels {
		if strings.HasPrefix(label, "project:") {
			beadProject = strings.TrimPrefix(label, "project:")
			break
		}
	}

	if beadProject == "" {
		return nil // Bead has no project label — allow claiming.
	}

	if beadProject != agentProject {
		return fmt.Errorf("bead %s belongs to project %q, but you are in project %q — only claim beads in your own project, or use --force to override", beadID, beadProject, agentProject)
	}
	return nil
}

// isLockBead reports whether b is a lock bead: labelled `lock`, taken with
// kd claim and released with kd unclaim. Mirrors the daemon's isLockBead.
func isLockBead(b *model.Bead) bool {
	return b != nil && slices.Contains(b.Labels, "lock")
}
