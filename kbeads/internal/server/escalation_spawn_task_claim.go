// Spawn-task follow-ups for a replayed agent create (gd-G5PmhWXsF9).
//
// A direct spawn (beadsapi.Client.SpawnAgentResult) does two things after its
// CreateBead succeeds, both with the spawner's own token: it adds the agent
// bead -> task 'assigned' dependency, and, for a single-claim spawn (`gb spawn
// --task`), it pre-claims the task (status=in_progress, assignee=<agent>,
// ka-0000002756). A `gb spawn --escalate` create is denied, so the client never
// runs either step; the approve replay creates the agent bead server-side and,
// before this file, stopped there. The spawned agent then held a task nobody
// had assigned to it, and on its own per-agent token `kd claim` / `kd close` of
// that task were refused by the ownership gate (gd-v6rYa4Lk0o, 2026-10-30).
// This is the same gap #1498 closed for the project:/role: labels
// (withDerivedAgentLabels).
//
// The replay now does what the client would have done. It widens no authz:
// the pre-claim is the requester's own write, run through updateBead as the
// requesting agent, so the ordinary ownership rules decide it. A requester that
// could not have claimed the task itself gets no pre-claim, and the spawned
// agent is in the same position it was in before.
//
// The opt-in: the client strips single_claim_task before the create, so the
// denied body (operation_spec) never carries it. Every replayed agent create
// whose fields name a task_id is treated as single-claim. The only spawner that
// files these escalations is `gb spawn --escalate`, and `gb spawn --task`
// always sets single_claim_task, so this matches what the client would have
// done for every replay that reaches here.
package server

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strings"
	"time"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// Outcomes of claimReplayedSpawnTask, reported on the replay result as
// task_claim so an approver (and the test) can see what happened to the task.
const (
	spawnTaskClaimed         = "claimed"
	spawnTaskAlreadyAssigned = "already_assigned"
	spawnTaskLiveAssignee    = "skipped_live_assignee"
	spawnTaskClosed          = "skipped_closed"
	spawnTaskUnverified      = "skipped_unverified_requester"
	spawnTaskRequesterDenied = "skipped_requester_not_owner"
	spawnTaskNotFound        = "skipped_task_not_found"
	spawnTaskClaimFailed     = "failed"
)

const assignedDependencyType model.DependencyType = "assigned"

// claimReplayedSpawnTask runs the spawn client's two post-create task steps
// for an agent bead the approve replay just created: the 'assigned' dependency
// and the pre-claim. It returns "" when the agent names no task, else one of
// the spawnTask* outcomes. Best-effort, like the client: every failure logs and
// the agent bead stands. A spawn that pre-claims and whose pod then never runs
// self-heals: the stale reaper reopens a task whose assignee agent is gone.
func (s *BeadsServer) claimReplayedSpawnTask(ctx context.Context, escalation, agent *model.Bead) string {
	if agent == nil || agent.Type != model.BeadType("agent") {
		return ""
	}
	af := parseFieldsToStringMap(agent.Fields)
	taskID := strings.TrimSpace(af["task_id"])
	if taskID == "" {
		return ""
	}
	agentName := af["agent"]
	if agentName == "" {
		agentName = agent.Title
	}
	task, err := s.store.GetBead(ctx, taskID)
	if err != nil || task == nil {
		slog.Warn("spawn replay: task named by the agent's task_id not found",
			"escalation", escalation.ID, "agent", agent.ID, "task", taskID, "error", err, "bead", "gd-G5PmhWXsF9")
		return spawnTaskNotFound
	}

	// The 'assigned' dependency confers no authority (nothing in the daemon
	// authorizes on it); it is the graph link the client always adds, single
	// claim or not, and the one gb prime and the viewers read.
	//
	// deps.operated_by_id is NOT NULL (migrations 017/022), so the dep
	// inherits the operator FK of the agent bead it hangs off, as
	// maybeAddAutoParent does (ka-0000007722). createBead fails closed on a
	// missing FK, so a replayed agent bead always carries one; the nil check
	// only keeps a malformed bead from a doomed insert.
	dep := &model.Dependency{
		BeadID:       agent.ID,
		DependsOnID:  taskID,
		Type:         assignedDependencyType,
		CreatedAt:    time.Now().UTC(),
		CreatedBy:    agentName,
		OperatedBy:   agent.OperatedBy,
		OperatedByID: agent.OperatedByID,
	}
	if dep.OperatedByID == nil {
		slog.Warn("spawn replay: agent bead has no operator FK, skipping the assigned dependency",
			"escalation", escalation.ID, "agent", agent.ID, "task", taskID, "bead", "gd-G5PmhWXsF9")
	} else if err := s.store.AddDependency(ctx, dep); err != nil {
		slog.Warn("spawn replay: failed to add the agent's assigned dependency",
			"escalation", escalation.ID, "agent", agent.ID, "task", taskID, "error", err, "bead", "gd-G5PmhWXsF9")
	} else {
		s.recordAndPublish(ctx, events.TopicDependencyAdded, agent.ID, agentName, events.DependencyAdded{
			Dependency: dep,
			Fleet:      FleetFromContext(ctx),
		})
	}

	switch {
	case task.Status == model.StatusClosed:
		return spawnTaskClosed
	case task.Assignee == agentName:
		return spawnTaskAlreadyAssigned
	case task.Assignee != "" && !s.assigneeIsGoneAgent(ctx, task.Assignee):
		// A live agent, a human, or a name that is no agent at all: never
		// clobber it. The client never reaches its stamp with a live
		// incumbent either (its single-claim guard returns that agent first).
		return spawnTaskLiveAssignee
	}

	reqCtx, ok := s.spawnRequesterContext(ctx, escalation)
	if !ok {
		// requesting_agent is a claim unless the server derived it from the
		// filer's own per-agent token (requesting_agent_verified), and the
		// token's project must have been stamped with it. Without both there
		// is no credential whose ownership and scope the pre-claim could be
		// held to, so none is made.
		return spawnTaskUnverified
	}
	inProgress := string(model.StatusInProgress)
	if _, err := s.updateBead(reqCtx, taskID, updateBeadInput{Status: &inProgress, Assignee: &agentName}); err != nil {
		var denial *AuthzDenial
		if errors.As(err, &denial) {
			slog.Info("spawn replay: requester may not claim the spawn task, no pre-claim",
				"escalation", escalation.ID, "agent", agent.ID, "task", taskID, "reason", denial.Message, "bead", "gd-G5PmhWXsF9")
			return spawnTaskRequesterDenied
		}
		slog.Warn("spawn replay: failed to pre-claim the spawn task",
			"escalation", escalation.ID, "agent", agent.ID, "task", taskID, "error", err, "bead", "gd-G5PmhWXsF9")
		return spawnTaskClaimFailed
	}
	return spawnTaskClaimed
}

// spawnRequesterContext returns a context that authenticates as the agent that
// filed the spawn escalation, on its own per-agent token, the way the auth
// middleware builds one (fleet agent-<bead id>, subject = agent name, the
// token's project). It is built on a fresh context so nothing of the approver
// (OIDC roles, auth method, machine-write marker) carries into the write.
//
// Every part comes from the filer's credential, never from a bead the filer can
// edit. The requester's agent bead is config-kind, so authorizeOwnership lets
// the agent PATCH its own fields.agent and fields.project; read from there, an
// agent could name another crew and pre-claim that crew's task. Instead:
//
//   - fleet: requesting_agent, the bead id the create guard took from the
//     filer's fleet name, and only when requesting_agent_verified says so;
//   - subject: the escalation's created_by, which createBead sets from the
//     token subject and no PATCH changes;
//   - project: requesting_agent_project, stamped from the token's project
//     scope at filing. Without it the pre-claim is refused, never run
//     unscoped: no per-agent token is.
//
// All three stamps are immutable after create.
func (s *BeadsServer) spawnRequesterContext(ctx context.Context, escalation *model.Bead) (context.Context, bool) {
	ef := parseFieldsToStringMap(escalation.Fields)
	if ef[escalationRequestingAgentVerifiedField] != "true" {
		return nil, false
	}
	beadID := strings.TrimSpace(ef["requesting_agent"])
	project := strings.TrimSpace(ef[escalationRequestingAgentProjectField])
	name := strings.TrimSpace(escalation.CreatedBy)
	if beadID == "" || project == "" || name == "" || validateAgentFleetRegistration(agentFleetPrefix+beadID, name) != nil {
		return nil, false
	}
	requester, err := s.store.GetBead(ctx, beadID)
	if err != nil || requester == nil || requester.Type != model.BeadType("agent") {
		return nil, false
	}
	rctx := withAuthMethod(context.Background(), authMethodStatic)
	rctx = WithAuthIdentity(rctx, AuthIdentity{Type: "fleet", Subject: name})
	rctx = WithFleet(rctx, agentFleetPrefix+requester.ID)
	rctx = WithTokenProject(rctx, project)
	return rctx, true
}

// assigneeIsGoneAgent reports whether name is an agent with a closed bead and
// no live one: the stranded-claim case the stale reaper reopens. A human
// (email), an unknown name or any store error is not gone, so callers that use
// this to decide whether to overwrite an assignee fail closed.
func (s *BeadsServer) assigneeIsGoneAgent(ctx context.Context, name string) bool {
	if name == "" || strings.Contains(name, "@") {
		return false
	}
	active, err := s.store.FindAgentByName(ctx, name, activeAgentStatuses)
	if err == nil && active != nil {
		return false
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false
	}
	closed, err := s.store.FindAgentByName(ctx, name, []model.Status{model.StatusClosed})
	return err == nil && closed != nil
}
