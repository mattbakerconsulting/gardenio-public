package server

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"gardenio/kbeads/internal/model"
)

// checkEscalationTargets enforces spec §5.3 and §8.1 at filing, and again for
// an approve-time execution_target, and refuses an executor for a replay-unfit
// type. It is the one create guard for where an escalation runs. Skipped for
// machine writes (the daemon's own creates).
//
// target_runner is a runner bead id (spec §8.1; widened from P7 Task 13's
// local agent bead by P4 Task 16): the machine, not one session on it. A
// target_runner that is not an open, unrevoked runner is refused whatever the
// execution_target, since approve may switch the target to local and the field
// is frozen once filed. execution_target=local must name target_runner or
// target_owner (that owner's one online runner is resolved at approval); one
// naming neither is refused unless BEADS_ESCALATION_LOCAL_TARGET=warn.
func (s *BeadsServer) checkEscalationTargets(ctx context.Context, bead *model.Bead) error {
	if bead.Type != model.TypeEscalation || isMachineWrite(ctx) {
		return nil
	}
	f := escalationStringFields(bead)
	if err := checkReplayUnfitExecutor(f); err != nil {
		return err
	}
	if runner := f["target_runner"]; runner != "" {
		if _, err := s.liveTargetRunner(ctx, runner); err != nil {
			return err
		}
	}
	switch f["execution_target"] {
	case "local":
		if f["target_runner"] != "" || f["target_owner"] != "" {
			return nil
		}
		if !escalationLocalTargetWarnOnly() {
			return inputError(escalationLocalTargetMissing)
		}
		slog.Warn("local escalation names no runner", "actor", actorFromContext(ctx), "mode", "warn")
		addWarning(ctx, escalationLocalTargetMissing+" (allowed: BEADS_ESCALATION_LOCAL_TARGET=warn)")
	case "service_account":
		role := f["spawn_role"]
		if role == "" || strings.TrimSpace(f["execution_script"]) == "" {
			return inputError("execution_target=service_account requires spawn_role (a privileged role) and a reviewed execution_script")
		}
		ok, err := roleIsPrivileged(ctx, s.store, role)
		if err != nil {
			return fmt.Errorf("check spawn_role %q: %w", role, err)
		}
		if !ok {
			return inputError(fmt.Sprintf("spawn_role %q is not a privileged role; service_account execution needs one", role))
		}
	}
	return nil
}

// startEscalationExecutor runs after the dispatch claim (Task 12).
func (s *BeadsServer) startEscalationExecutor(ctx context.Context, bead *model.Bead, dispatch, approvedBy string) error {
	f := escalationStringFields(bead)
	switch dispatch {
	case dispatchLocal:
		// The one writer of a local executor: the agent joined on the
		// escalation's runner (target_runner, else target_owner's one online
		// runner), stamped before the approval event so the runner's watcher
		// and POST /result both see it. No runner resolved, or none joined:
		// no executor, and the approved mail is parked for gb runner join.
		runner, err := s.resolveEscalationRunner(ctx, beadFieldMap(bead.Fields))
		if err != nil || runner == "" {
			return err
		}
		agent, err := s.localAgentOnRunner(ctx, runner)
		if err != nil || agent == "" {
			return err
		}
		return s.mergeBeadFields(ctx, bead.ID, map[string]any{"executor": agent})
	case dispatchServiceAccount:
		// Never a plain crew (spec Q3): one filed before these checks, with
		// no privileged spawn_role or script, spawns nothing and an operator
		// posts the result.
		if ok, err := roleIsPrivileged(ctx, s.store, f["spawn_role"]); err != nil {
			return fmt.Errorf("check spawn_role %q: %w", f["spawn_role"], err)
		} else if !ok || strings.TrimSpace(f["execution_script"]) == "" {
			slog.Warn("service_account escalation has no privileged spawn_role and script; spawning nothing",
				"escalation", bead.ID, "spawn_role", f["spawn_role"])
			return nil
		}
		name := "escalation-sa-" + bead.ID
		project := ""
		for _, l := range bead.Labels {
			if p, ok := strings.CutPrefix(l, "project:"); ok {
				project = p
				break
			}
		}
		prompt := fmt.Sprintf("You are the service-account executor for approved escalation %s (operation %s).\n"+
			"Run exactly this reviewed execution_script, nothing else:\n\n%s\n\n"+
			"Then report: gb escalation close %s --status success|failure|partial --result '<what happened>'",
			bead.ID, f["operation_type"], f["execution_script"], bead.ID)
		fields := s.requesterSlackRouting(ctx, f["requesting_agent"])
		for k, v := range map[string]string{
			"agent": name, "mode": "crew", "role": f["spawn_role"], "project": project,
			"prompt": prompt, "escalation_id": bead.ID, "execution_target": "service_account",
		} {
			fields[k] = v
		}
		raw, _ := json.Marshal(fields)
		replayCtx := withApprovedSpawnReplay(withMachineWrite(WithMachineOperator(ctx)), bead.ID, approvedBy)
		if _, err := s.createBead(replayCtx, createBeadInput{
			Title: name, Type: "agent", Kind: string(model.KindData), Labels: []string{"project:" + project},
			Fields: stampSpawnProvenance(raw, bead.ID, approvedBy),
		}); err != nil {
			return fmt.Errorf("spawn service-account executor: %w", err)
		}
		return s.mergeBeadFields(ctx, bead.ID, map[string]any{"executor": name})
	}
	return nil
}

// escalationExecutorRoutingFields are the requester agent-bead fields the
// bridge reads to place an agent's card (beadsapi.SlackRoutingFields, plus
// slack_user_id and the wish pointer), so a service-account executor posts in
// the requester's thread as the bridge-spawned executor did (gd-Q553TCVjN4).
var escalationExecutorRoutingFields = []string{
	"slack_spawn_channel",
	"slack_spawn_channel_inherited_from_role",
	"schedule_slack_channel",
	"slack_thread_channel",
	"slack_thread_ts",
	"slack_user_id",
	"thread_id",
	"slack_thread_bead_id",
}

// requesterSlackRouting returns the requesting agent's Slack routing fields,
// or an empty map when there is no readable requester. Best-effort: routing
// only decides where the executor's card lands.
func (s *BeadsServer) requesterSlackRouting(ctx context.Context, requester string) map[string]string {
	out := map[string]string{}
	if requester == "" {
		return out
	}
	rb, err := s.store.GetBead(ctx, requester)
	if err != nil || rb == nil || rb.Type != "agent" {
		return out
	}
	rf := escalationStringFields(rb)
	for _, k := range escalationExecutorRoutingFields {
		if v := rf[k]; v != "" {
			out[k] = v
		}
	}
	// The wish pointer is written as a pair (beadsapi.SetWishPointer).
	if head := cmp.Or(out["thread_id"], out["slack_thread_bead_id"]); head != "" {
		out["thread_id"], out["slack_thread_bead_id"] = head, head
	}
	return out
}
