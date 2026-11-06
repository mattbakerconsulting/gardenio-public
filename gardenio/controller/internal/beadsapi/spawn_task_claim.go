package beadsapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
)

// Spawn-task claim at pod create (gd-Yo28zKgkGx).
//
// SpawnAgentResult pre-claims a single-claim spawn's task (ka-0000002756), but
// it writes with the SPAWNER's token, and a spawner that does not own the task
// gets a 403 there that is logged and lost. The crew then boots on a task every
// `kd claim` / `kd update` / `kd close` of which the ownership gate refuses.
//
// ClaimSpawnTask is the dispatch-side repair. The controller calls it on its
// machine principal just before it creates the agent's pod, so the claim is in
// place before the crew boots. The write is the controller's own, held to
// gardenio-machine-controller-ownership (kbeads machine_grants.go): Ownership
// on task; ReclaimOwnership on bug/feature/chore/epic only while the assignee
// is a gone agent, or is empty with ReapedAssigneeField naming an agent that
// has died (gd-8Irb2vDFf7); and DispatchClaim on an open, unassigned
// bug/feature/chore for the single-claim agent dispatched onto it
// (gd-GrJRP7kLBY). It never takes a task from a live agent or a human.
//
// Three gates keep it to what the spawner's own pre-claim would have done:
//   - Opt-in: only a spawn that set single_claim_task (persisted as
//     FieldClaimTaskAtSpawn, since SpawnAgent strips the control flag). Fan-out
//     and AssignBead-pinned gardeners, which share a long-lived task, are left
//     alone, as SpawnAgent leaves them.
//   - Once per agent bead: the outcome is stamped as FieldSpawnTaskClaim, so a
//     recreate (spot loss, stuck restart, reconciler create-missing) never
//     re-claims a task the crew or a send-back has since handed back. Every
//     in-place wake clears the stamp (agentwake.EventWakeDormant and
//     ReviveAgentFields, sent by the timer wake, `gb wake` and the garden), since
//     a wake is a fresh dispatch onto the task.
//   - Spawner scope: the task must be in the agent's project (the project the
//     daemon's spawn authz already let the spawner spawn into) or created by
//     the spawner. A spawn naming another project's task leaves it unclaimed.

const (
	// FieldClaimTaskAtSpawn marks an agent bead whose spawn opted into
	// single-claim (single_claim_task), so the controller may claim its
	// task_id at pod create.
	FieldClaimTaskAtSpawn = "claim_task_at_spawn"
	// FieldSpawnTaskClaim records the outcome of the controller's claim at the
	// agent bead's first pod create; its presence skips the claim on recreate.
	FieldSpawnTaskClaim = "spawn_task_claim"
)

// Outcomes of ClaimSpawnTask, logged by the callers so an operator can see
// what happened to an agent's task at pod create.
const (
	SpawnTaskClaimed         = "claimed"
	SpawnTaskAlreadyAssigned = "already_assigned"
	SpawnTaskLiveAssignee    = "skipped_live_assignee"
	SpawnTaskClosed          = "skipped_closed"
	SpawnTaskUnclaimableType = "skipped_type"
	SpawnTaskNotSingleClaim  = "skipped_not_single_claim"
	SpawnTaskRecreate        = "skipped_recreate"
	SpawnTaskOutOfScope      = "skipped_out_of_spawner_scope"
)

// spawnTaskClaimableTypes are the issue types a crew is dispatched onto and
// claims. An epic is a planning container `kd claim` refuses, and its assignee
// is the plan's owner (plan_scope_authz.go), so it is never stamped here.
var spawnTaskClaimableTypes = map[string]bool{"task": true, "bug": true, "feature": true, "chore": true}

// ClaimSpawnTask makes the agent of agentBeadID the assignee of the task its
// bead names, writing what `kd claim` writes (status=in_progress, assignee),
// when the three gates above pass, the task is claimable and nobody live holds
// it. It returns one of the SpawnTask* outcomes, or "" when the agent names no
// task. An error means a read or the claim write failed; callers log it and
// still create the pod, since the crew can work without the claim.
//
// The assignee states it can meet:
//   - the agent already: nothing to do (the spawner's own pre-claim landed).
//   - empty: claimed. kbeads admits a bug/feature/chore through DispatchClaim
//     (or, when a reopen, park or half-reap clear stamped ReapedAssigneeField,
//     through the reaped reclaim).
//   - a gone agent (its newest agent bead is closed): claimed. This is the
//     stale reaper's reclaim test (assigneeIsGoneAgent in stalereaper.go).
//   - anything else (a live agent, a human, a name with no agent bead, or a
//     lookup error): left alone.
func (c *Client) ClaimSpawnTask(ctx context.Context, agentBeadID string) (string, error) {
	agent, err := c.GetBead(ctx, agentBeadID)
	if err != nil {
		return "", err
	}
	taskID := strings.TrimSpace(agent.Fields["task_id"])
	agentName := agent.Fields["agent"]
	if taskID == "" || agentName == "" {
		return "", nil
	}
	switch {
	case !fieldTrue(agent.Fields[FieldClaimTaskAtSpawn]):
		return SpawnTaskNotSingleClaim, nil
	case agent.Fields[FieldSpawnTaskClaim] != "":
		return SpawnTaskRecreate, nil
	}
	outcome, err := c.claimTaskFor(ctx, agent, agentName, taskID)
	if err != nil {
		return "", err
	}
	// Best-effort: a failed stamp only means a later recreate reads the task
	// again, and finds it already assigned to this agent.
	if err := c.UpdateBeadFields(ctx, agentBeadID, map[string]string{FieldSpawnTaskClaim: outcome}); err != nil {
		slog.Warn("failed to stamp spawn task claim outcome on agent bead",
			"bead", agentBeadID, "task", taskID, "outcome", outcome, "error", err)
	}
	return outcome, nil
}

// claimTaskFor decides and, if it may, writes the claim of taskID for the
// agent bead agent, named agentName.
func (c *Client) claimTaskFor(ctx context.Context, agent *BeadDetail, agentName, taskID string) (string, error) {
	task, err := c.GetBead(ctx, taskID)
	if err != nil {
		return "", err
	}
	switch {
	case task.Status == "closed" || task.Status == "deferred":
		return SpawnTaskClosed, nil
	case !spawnTaskClaimableTypes[task.Type]:
		return SpawnTaskUnclaimableType, nil
	case task.Assignee == agentName:
		return SpawnTaskAlreadyAssigned, nil
	case !inSpawnerScope(task, agent):
		return SpawnTaskOutOfScope, nil
	case task.Assignee != "":
		gone, err := c.agentIsGone(ctx, task.Assignee)
		if err != nil {
			return "", err
		}
		if !gone {
			return SpawnTaskLiveAssignee, nil
		}
	}
	inProgress := "in_progress"
	req := UpdateBeadRequest{Status: &inProgress, Assignee: &agentName}
	if task.Fields[ReapedAssigneeField] != "" {
		// The claim consumes the reap record, so it cannot admit a later claim.
		req.Fields = map[string]any{ReapedAssigneeField: nil}
	}
	if err := c.UpdateBead(ctx, taskID, req); err != nil {
		return "", fmt.Errorf("claiming task %s for %s: %w", taskID, agentName, err)
	}
	return SpawnTaskClaimed, nil
}

// inSpawnerScope reports whether the agent's spawner could have dispatched
// onto task: the task carries the agent's project label, or the spawner (the
// agent bead's creator) created the task.
func inSpawnerScope(task, agent *BeadDetail) bool {
	if agent.CreatedBy != "" && task.CreatedBy == agent.CreatedBy {
		return true
	}
	project := agent.Fields["project"]
	if project == "" {
		return false
	}
	for _, l := range task.Labels {
		if l == "project:"+project {
			return true
		}
	}
	return false
}

// agentIsGone reports whether name is an agent whose newest bead is closed. A
// human (an email) or a name with no agent bead is not gone; a lookup error is
// returned so the caller leaves the assignee alone.
func (c *Client) agentIsGone(ctx context.Context, name string) (bool, error) {
	if strings.Contains(name, "@") {
		return false, nil
	}
	agent, err := c.FindMostRecentAgentBead(ctx, name)
	if errors.Is(err, ErrAgentNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return agent.Status == "closed", nil
}

// markClaimTaskAtSpawn persists a single-claim spawn's opt-in on the fields
// SpawnAgent creates, for the controller to read at pod create when the
// spawner's own pre-claim is refused.
func markClaimTaskAtSpawn(fields map[string]string, taskID string, extraFields []map[string]string) {
	if taskID != "" && extraFieldTrue(extraFields, "single_claim_task") {
		fields[FieldClaimTaskAtSpawn] = "true"
	}
}

func fieldTrue(v string) bool {
	return v == "1" || v == "true"
}
