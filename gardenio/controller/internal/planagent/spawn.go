package planagent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// Spawn creates (on first contact) or WAKES IN PLACE (when a prior agent
// already exists) the plan's agent in the fable+runsc-pinned plan-gardener role
// (the unified name of the ka-0000000081 merge), bound to the epic by
// fields.plan_id AND an `assigned` edge, with `message` as its spawn/wake
// prompt. Returns (beadID, agentName, error).
//
// Wake continuity depends on the agent bead id being STABLE across the
// stop/respawn cycle, because that id IS the session-resume key: the controller
// wires KD_AGENT_ID = <agent bead id> and coop composes its S3 transcript prefix
// as <bead-id>/<coop-uuid>. A wake that MINTED A NEW bead each time (the pre-fix
// behavior) therefore pointed the fresh pod at an empty S3 prefix and
// cold-started — silently discarding the operator's plan conversation (bug
// ka-0000002360). So when a prior agent exists we REVIVE THE SAME bead in place
// (refresh the prompt + session_resume field, then flip status back to
// in_progress) rather than create a new one: the daemon's "status →
// in_progress" transition re-emits AgentSpawn (subscriber/sse.go), which on a
// freeze-eligible env thaws the checkpointed pod and otherwise cold-respawns
// coop with --resume against the unchanged S3 key. The controller reads
// role/model/account/runtime_class off the plan-gardener role bead; this function
// does not set runtime_class itself (the role pins it), which keeps the freeze
// policy in one place.
func (e *Engine) Spawn(ctx context.Context, actor string, epic *beadsapi.BeadDetail, message string) (string, string, error) {
	epicID := epic.ID
	project := projectFromLabels(epic.Labels)

	// Backstop guard (ka-0000000753): a CLOSED plan is done — refuse to spawn a
	// fresh gardener OR wake a parked one for it. Every path that spawns or
	// re-drives the plan gardener (reactive add-step, per-step dispatch, index-row
	// dispatch, the explicit prompt box) funnels through here, so this one check
	// stops the observed no-op respawn loop regardless of the trigger: a closed
	// plan kept re-driving its gardener, each respawn replaying the stale original
	// add-step message (fields.prompt), until it burned cycles indefinitely. The
	// Wake flip to status=in_progress below is the exact write that re-emits
	// AgentSpawn and thaws/cold-respawns the pod; not doing it means a closed
	// plan's gardener stays parked. Callers map ErrPlanClosedNoWake to a graceful
	// no-op (Prompt) — no work is lost, the plan is closed.
	if !isPlanOpen(epic.Status) {
		e.logger.Info("plan_agent_spawn_skipped", "event", "plan_agent_spawn_skipped",
			"reason", "plan_closed", "epic", epicID, "plan_status", epic.Status, "actor", actor)
		return "", "", ErrPlanClosedNoWake
	}

	// Wake path: a prior agent bead exists → revive IT (stable id → stable S3
	// resume key) instead of minting a new bead.
	if prior := e.Resolve(ctx, epicID); prior != nil && strings.TrimSpace(prior.Title) != "" {
		return e.Wake(ctx, actor, prior, message)
	}

	agentName, err := e.nameGen(project, "plan gardener "+epicID)
	if err != nil {
		return "", "", fmt.Errorf("generating agent name: %w", err)
	}

	// Attribution invariant (ka-0000001801): the CreateBead + AddDependency writes
	// below are attributed solely from ctx. Surface a diagnostic if the caller
	// entered here without stamping an operator, so a downstream R2 fail-closed 400
	// traces to the missing stamp rather than a mystery upstream error.
	e.requireAuditAttribution(ctx, "spawn", epicID, actor)

	fields := map[string]string{
		"agent":     agentName,
		"mode":      "crew",
		"role":      SpawnRole,
		"project":   project,
		"prompt":    message,
		"task_id":   epicID,
		PlanIDField: epicID,
	}
	fieldsJSON, err := json.Marshal(fields)
	if err != nil {
		return "", "", fmt.Errorf("marshalling agent fields: %w", err)
	}

	desc := "Plan gardener for epic: " + epicID + "\n\n" + message
	id, err := e.daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Title:       agentName,
		Type:        "agent",
		Description: desc,
		CreatedBy:   actor,
		Fields:      fieldsJSON,
	})
	if err != nil {
		return "", "", fmt.Errorf("creating agent bead: %w", err)
	}
	// Best-effort labels + the load-bearing assigned edge.
	if project != "" {
		if lerr := e.daemon.AddLabel(ctx, id, "project:"+project); lerr != nil {
			e.logger.Warn("plan agent: add project label", "bead", id, "error", lerr)
		}
	}
	if lerr := e.daemon.AddLabel(ctx, id, RoleLabel); lerr != nil {
		e.logger.Warn("plan agent: add role label", "bead", id, "error", lerr)
	}
	if derr := e.daemon.AddDependency(ctx, id, epicID, "assigned", agentName); derr != nil {
		e.logger.Warn("plan agent: add assigned edge", "bead", id, "epic", epicID, "error", derr)
	}
	e.logger.Info("plan_agent_spawn", "event", "plan_agent_spawn", "epic", epicID,
		"agent", agentName, "actor", actor, "resume", false)
	return id, agentName, nil
}

// Wake revives an existing (parked / stopped / closed) plan agent bead IN PLACE
// so its bead id — and therefore the coop S3 session key (KD_AGENT_ID) — stays
// constant across the wake. This is what makes the operator's plan conversation
// continue instead of cold-starting (bug ka-0000002360); minting a fresh bead
// pointed the pod at an empty S3 prefix.
//
// ONE atomic write (ka-0000001583): flip status→in_progress AND refresh the wake
// fields in a single PATCH. The fields refreshed are:
//   - the new prompt (the pod reads fields.prompt into BOAT_PROMPT as the resumed
//     session's next message),
//   - session_resume=true,
//   - stop_requested cleared, so the entrypoint's restart loop does not
//     immediately re-close the revived bead,
//   - agent_state cleared (see below).
//
// The daemon's status→in_progress transition is exactly what subscriber/sse.go
// maps to AgentSpawn, driving the controller to thaw (freeze-eligible) or
// cold-respawn-with-resume.
//
// Why ONE write, not two (ka-0000001583): a prior version split this into a
// fields-only PATCH followed by a status-only PATCH. When the prior bead is
// CLOSED (an operator-`gb stop`ped plan gardener, the common case), the
// fields-only PATCH landed while the bead was still closed and the daemon's
// dropTransientClosedFields guard (kbeads beads.go, ka-0000005253) STRIPPED
// agent_state from it — so the clear was silently dropped and agent_state stayed
// "done". The status flip then reopened the bead with a stale terminal
// agent_state, and the reconciler leak-closer immediately re-reaped it as a
// podless_leak before the cold-start pod reached Running — an infinite
// reopen→reap loop in which the operator's message was never delivered. Sending
// status + fields together means the daemon sees a REOPEN (status leaving
// closed) and its guard steps aside for exactly this write, so the agent_state
// clear lands atomically with the reopen.
//
// Why agent_state MUST be cleared (bug ka-0000004492): the SSE watcher's
// mapBeadEvent (subscriber/sse.go) inspects the event's terminal agent_state
// BEFORE it checks for the status→in_progress re-spawn. If the prior session died
// as rate_limited / budget_exceeded (a very common way a plan agent parks — an
// operator says "it got rate limited"), that stale agent_state is still on the
// bead. mapBeadEvent then maps the wake's update to a BudgetExceeded event and
// RETURNS EARLY — the controller runs auth failover and `continue`s WITHOUT ever
// creating a pod (cmd/controller/main.go). The status→in_progress re-spawn branch
// is never reached, so the wake is silently swallowed. Because the clear now
// lands in the SAME event as the status flip, the single AgentSpawn event carries
// no terminal agent_state and neither the BudgetExceeded interceptor nor the
// leak-closer misfires. The status reporter re-stamps agent_state
// (spawning→working) once the fresh pod is actually running.
//
// Returns (beadID, agentName, error) with the PRIOR bead's id and name.
func (e *Engine) Wake(ctx context.Context, actor string, prior *beadsapi.BeadDetail, message string) (string, string, error) {
	id := prior.ID
	agentName := prior.Title

	// Attribution invariant (ka-0000001801): the atomic wake write below (the
	// status flip + field refresh that re-emits AgentSpawn) is attributed solely
	// from ctx. Surface a diagnostic if the caller entered the wake path without
	// stamping an operator — an unattributed wake fail-closes the same way a fresh
	// spawn does, and this makes the missing stamp visible at the write boundary.
	e.requireAuditAttribution(ctx, "wake", id, actor)

	inProgress := "in_progress"
	if err := e.daemon.UpdateBead(ctx, id, beadsapi.UpdateBeadRequest{
		Status: &inProgress,
		Fields: map[string]any{
			"prompt":         message,
			"session_resume": "true",
			"stop_requested": "",
			// DELETE agent_state (nil), do not set it to "". agent_state is an enum
			// field whose value set does NOT include the empty string, so an
			// UpdateBead carrying agent_state:"" 400s at the daemon's type-field
			// validator; RFC-7386 null-merge removes the key, which is the only legal
			// "cleared" state and is exactly what the SSE watcher + reconciler read as
			// "no terminal state" (ka-0000001583 / ka-0000004492).
			"agent_state": nil,
		},
	}); err != nil {
		return "", "", fmt.Errorf("reopening plan agent %s for wake: %w", id, err)
	}

	e.logger.Info("plan_agent_spawn", "event", "plan_agent_wake", "agent", id,
		"name", agentName, "actor", actor, "resume", true)
	return id, agentName, nil
}
