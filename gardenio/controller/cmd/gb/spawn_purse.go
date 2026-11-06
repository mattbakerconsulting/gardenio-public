package main

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/steward"

	"github.com/spf13/cobra"
)

// Spawning a lands-gardener within the purse (Lands Track B step 9.5). Before
// gb spawn creates a lands-gardener on a tenant land it asks
// steward.CheckStepSpawn: purse at the wall or unknown, the land's
// max_live_agents full, or the step estimate more than remains above the
// margin refuse it before anything is created. The last is the steward's ask
// instead of spawn: it tells the owner what the step needs, and an owner's
// yes comes back as --owner-approved <decision>, which lifts only that one
// refusal. The controller's spawn wall still decides the spawn after this.

// ownerApprovedField records on the agent bead the decision that let a step
// spawn past its estimate.
const ownerApprovedField = "owner_approved_decision"

// stepSpawnReader is the slice of the daemon client the check reads through.
type stepSpawnReader interface {
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
	GetBead(ctx context.Context, id string) (*beadsapi.BeadDetail, error)
}

// isLandsGardenerSpawn reports whether role puts the agent in lands-gardener.
func isLandsGardenerSpawn(role string) bool {
	return slices.Contains(parseRoleElems(role), steward.GardenerRole)
}

func parseRoleElems(role string) []string {
	var out []string
	for r := range strings.SplitSeq(role, ",") {
		if r = strings.TrimSpace(r); r != "" {
			out = append(out, r)
		}
	}
	return out
}

// checkOwnerApproval reads the decision passed as --owner-approved and asks
// steward.CheckOwnerApproval whether it is the owner's yes for this step.
func checkOwnerApproval(ctx context.Context, d stepSpawnReader, land, id, step string) error {
	b, err := d.GetBead(ctx, id)
	if err != nil {
		return fmt.Errorf("--owner-approved %s: %w", id, err)
	}
	if err := steward.CheckOwnerApproval(ctx, d, b, land, step, actor); err != nil {
		return fmt.Errorf("--owner-approved %s: %w", id, err)
	}
	return nil
}

// gateStepSpawn is the pre-create check. It returns the fields to stamp on
// the agent bead (the owner's approval, when one let it through) or an error
// that refuses the spawn, printed for the steward to act on.
func gateStepSpawn(cmd *cobra.Command, d stepSpawnReader, project, role, taskID string, now time.Time) (map[string]string, error) {
	if d == nil || !isLandsGardenerSpawn(role) {
		return nil, nil
	}
	ctx := cmd.Context()
	verdict := steward.CheckStepSpawn(ctx, d, project, now)
	approved, _ := cmd.Flags().GetString("owner-approved")
	approved = strings.TrimSpace(approved)
	if verdict.Code == steward.SpawnAskOwner && approved != "" {
		if err := checkOwnerApproval(ctx, d, project, approved, taskID); err != nil {
			return nil, err
		}
		return map[string]string{ownerApprovedField: approved}, nil
	}
	if !verdict.Refused() {
		return nil, nil
	}
	if jsonOutput {
		out := map[string]any{"status": "refused", "project": project, "role": role, "reason": verdict.Code,
			"detail": verdict.Detail, "step_estimate_credits": verdict.Estimate.Credits,
			"above_margin_credits": verdict.AboveMarginCredits, "live_gardeners": verdict.LiveGardeners,
			"max_live_agents": verdict.MaxLiveAgents}
		if taskID != "" {
			out["task"] = taskID
		}
		printJSON(out)
	} else {
		fmt.Printf("NOT SPAWNED project=%s role=%s reason=%s\n", project, role, verdict.Code)
		fmt.Printf("  %s.\n", verdict.Detail)
		if verdict.Code == steward.SpawnAskOwner {
			fmt.Printf("  Ask the owner with a decision filed for the step (--requesting-task %s) with options %q and %q;\n", taskOr(taskID), steward.AskSpawn, steward.AskWait)
			fmt.Printf("  when they answer %s, spawn again with --owner-approved <decision-id>.\n", steward.AskSpawn)
		}
		fmt.Printf("  Nothing was created.\n")
	}
	return nil, fmt.Errorf("lands-gardener spawn in %s not made (%s)", project, verdict.Code)
}

// taskOr is the step id, or a placeholder when none was passed.
func taskOr(taskID string) string {
	if taskID == "" {
		return "<step-id>"
	}
	return taskID
}

// stepSpawnDaemon is the global daemon client as a stepSpawnReader, nil when
// there is none.
func stepSpawnDaemon() stepSpawnReader {
	if daemon == nil {
		return nil
	}
	return daemon
}
