package server

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"gardenio/kbeads/internal/model"
)

// escalationResultRequiresApproval reads BEADS_ESCALATION_RESULT_REQUIRES_APPROVAL
// (default false; Task 14 flips it). Read per request like spawnPolicyEnabled,
// so the flip is a deploy-values roll with no code change.
func escalationResultRequiresApproval() bool {
	v, err := strconv.ParseBool(strings.TrimSpace(os.Getenv("BEADS_ESCALATION_RESULT_REQUIRES_APPROVAL")))
	return err == nil && v
}

// SetEscalationResultMachines sets BEADS_ESCALATION_RESULT_MACHINES: machine
// client ids whose results are accepted without an approval (D6(d), a merge
// reaper that witnessed the merge). Empty at rollout.
func (s *BeadsServer) SetEscalationResultMachines(clients []string) {
	m := make(map[string]struct{}, len(clients))
	for _, c := range clients {
		if c = strings.TrimSpace(c); c != "" {
			m[c] = struct{}{}
		}
	}
	s.resultMachines = m
}

func (s *BeadsServer) resultMachineAllowed(client string) bool {
	_, ok := s.resultMachines[client]
	return ok && client != ""
}

// escalationResultDenial is the D6 gate on POST /v1/escalations/{id}/result.
// It returns "" when the result is accepted, else the 403 text. With the flag
// off it accepts everything (today's behaviour). Accepted when any of:
//
//	(d) the caller is a machine principal in BEADS_ESCALATION_RESULT_MACHINES;
//	(c) the caller may withdraw (callerMayWithdraw) and result_status=failure;
//	(f) result_status=failure on an unapproved, non-merge escalation, by a
//	    member of a coordinator lane of its stamped policy rule (policyLaneMayReject);
//	(e) result_status=failure on a merge escalation, by a member of its
//	    coordinator execute_by lane while that lane holds it or it was handed
//	    up from there (laneMaySendBack);
//	(b) execute_by=server and the caller is the daemon itself (isMachineWrite);
//	(a) a server-stamped approval exists and the caller has execute_by's
//	    authority (callerHasLaneAuthority; an empty execute_by is read as
//	    operator);
//	(g) a server-stamped approval exists, a hand-up left esc with the
//	    operator, and the caller is the operator (operatorFinishesHandUp).
func (s *BeadsServer) escalationResultDenial(ctx context.Context, esc *model.Bead, resultStatus string) string {
	if !escalationResultRequiresApproval() {
		return ""
	}
	if authMethodFromContext(ctx) == authMethodMachine && s.resultMachineAllowed(AuthIdentityFromContext(ctx).ClientID) {
		return ""
	}
	p, err := s.activeEscalationPolicy(ctx)
	if err != nil {
		slog.Warn("escalation result: policy unreadable; refusing", "escalation", esc.ID, "error", err)
		return "could not read the escalation policy; refusing the result"
	}
	if resultStatus == "failure" && (s.callerMayWithdraw(ctx, esc) || s.policyLaneMayReject(ctx, esc)) {
		return ""
	}
	exec, _, _ := exactFieldString(esc.Fields, "execute_by")
	if exec = strings.TrimSpace(exec); exec == "" {
		exec = string(laneOperator)
	}
	lane, err := parseLaneRef(exec)
	if err != nil {
		slog.Warn("escalation result: unparseable execute_by; treating as operator", "escalation", esc.ID, "execute_by", exec, "error", err)
		lane = laneRef{Kind: laneOperator, Raw: string(laneOperator)}
	}
	if lane.Kind == laneServer {
		if isMachineWrite(ctx) {
			return ""
		}
		return "this escalation is executed by the server's own replay; a result cannot be posted for it"
	}
	if resultStatus == "failure" && s.laneMaySendBack(ctx, esc, lane, p) {
		return ""
	}
	if !escalationHasServerApproval(esc) {
		return "this escalation has no approval yet; approve it before recording a result, or reject it"
	}
	in, err := s.callerHasLaneAuthority(ctx, lane, esc, p)
	if err != nil {
		slog.Warn("escalation result: lane membership unresolvable; refusing", "escalation", esc.ID, "lane", lane.String(), "error", err)
		return "could not resolve lane membership; refusing the result"
	}
	if !in && !operatorFinishesHandUp(ctx, esc) {
		return "only a member of " + lane.String() + " (execute_by) may record this escalation's result"
	}
	return ""
}

// operatorFinishesHandUp is D6(g) (gd-UNcBYJ9TZP): an approved escalation
// handed up to the operator is the operator's to finish. A manual hand-up
// keeps execute_by (applyHandUp), so a lane that hands up for approval keeps
// executing; but a lane that hands up what is already approved is saying it
// cannot execute it, and execute_by is fixed at filing, so without this nobody
// could record the result (five escalations coordinator:cluster handed up at
// 08:01Z 11-05). The caller must pass the approval check first.
func operatorFinishesHandUp(ctx context.Context, esc *model.Bead) bool {
	return escalationHeldBy(esc) == string(laneOperator) && heldByHandUp(esc) && callerMayApproveAsAdmin(ctx)
}

// callerHasLaneAuthority is D6(a)'s membership test: a result needs
// authority, not presence (design gd-B2kAow1r1O step A). Presence was proven
// at approve, and the result records it with result_client. For the human
// lanes it drops escalationApprover's client check: operator is gardenio-admin
// on any OIDC identity (any client, or a verified v2 edge envelope), and
// land-owner:<land> is that land's owners group on a tenant land. Every other
// lane is callerInLane. Never use it for approve: callerInLane(laneOperator)
// also gates the D3 floor and the approvers match.
func (s *BeadsServer) callerHasLaneAuthority(ctx context.Context, lane laneRef, esc *model.Bead, p escalationPolicy) (bool, error) {
	switch lane.Kind {
	case laneOperator:
		return callerMayApproveAsAdmin(ctx), nil
	case laneLandOwner:
		if authMethodFromContext(ctx) == authMethodOIDC && groupsToLandRoles(GroupsFromContext(ctx))[lane.Name] == landRoleOwner {
			return s.isTenantLand(ctx, lane.Name)
		}
		return false, nil
	}
	return s.callerInLane(ctx, lane, esc, p)
}

// laneMaySendBack is D6(e) (gd-fs9lGoSLDd): the merge desk sends a PR back by
// recording a failure on its unapproved merge escalation, since approving it
// would mean merging. A failure executes nothing, so no approval is needed.
// True when esc is a merge, execute_by is a coordinator lane, the caller is a
// member of it, and either that lane holds esc or esc's holder last changed
// by a hand-up (heldByHandUp). A hand-up for approval leaves a merge's
// execute_by with the desk (handUpExecuteBy), so the desk may still send back
// a PR closed unmerged meanwhile (gd-0v8Ftju0z0). An escalation routed
// elsewhere on purpose, e.g. to the operator, is no longer the desk's to close.
func (s *BeadsServer) laneMaySendBack(ctx context.Context, esc *model.Bead, lane laneRef, p escalationPolicy) bool {
	if lane.Kind != laneCoordinator || operationClassOf(parseSpawnEscalationFields(esc).OperationType) != "merge" {
		return false
	}
	if held, err := parseLaneRef(escalationHeldBy(esc)); (err != nil || held.String() != lane.String()) && !heldByHandUp(esc) {
		return false
	}
	in, err := s.callerInLane(ctx, lane, esc, p)
	return err == nil && in
}

// heldByHandUp reports whether esc's last held_by write was a hand-up:
// applyHandUp stamps held_at and handed_up_at with one time, and every other
// held_by write (route, approve, reroute) stamps held_at alone.
func heldByHandUp(esc *model.Bead) bool {
	at, _, _ := exactFieldString(esc.Fields, "handed_up_at")
	held, _, _ := exactFieldString(esc.Fields, "held_at")
	at = strings.TrimSpace(at)
	return at != "" && at == strings.TrimSpace(held)
}

// callerMayWithdraw reports whether the caller may withdraw esc, i.e. post
// result_status=failure without an approval (D6(c), gd-iCUeCw6UWP). True for:
//
//   - the verified requester (callerInLane(laneRequester));
//   - an agent on its own per-agent token that is the current assignee of the
//     open requesting_task, when the verified requester's own agent bead was
//     spawned on that same task (its task_id). Crews are short-lived, so this
//     is the successor on the requester's task;
//   - a later occupant of the verified requester's seat, while esc has no
//     approval (callerOccupiesRequesterSeat, gd-mlVNJxZ1yC). Every
//     plan-gardener run is a new identity on one seat, so without this only
//     the owner could retire a predecessor's moot ask.
//
// The task_id anchor is there because requesting_task is free text anyone on
// the queue may edit (not in escalationImmutableFields); without it an agent
// could point any escalation at its own task. Never widen
// callerInLane(laneRequester) itself: requester is also an execute_by lane, so
// that would admit success, the executor grant and routing too. Predecessor
// lineage (prior_agent_id) is not consulted: it is client-written.
func (s *BeadsServer) callerMayWithdraw(ctx context.Context, esc *model.Bead) bool {
	if in, _ := s.callerInLane(ctx, laneRef{Kind: laneRequester, Raw: string(laneRequester)}, esc, escalationPolicy{}); in {
		return true
	}
	if s.callerOccupiesRequesterSeat(ctx, esc) {
		return true
	}
	beadID, ok := agentBeadIDFromContext(ctx)
	if !ok {
		return false
	}
	name, _ := agentNameFromContext(ctx)
	f := escalationStringFields(esc)
	req, taskID := f["requesting_agent"], strings.TrimSpace(f["requesting_task"])
	if f[escalationRequestingAgentVerifiedField] != "true" || req == "" || taskID == "" {
		return false
	}
	reqBead, err := s.store.GetBead(ctx, req)
	if err != nil || reqBead == nil || strings.TrimSpace(escalationStringFields(reqBead)["task_id"]) != taskID {
		return false
	}
	task, err := s.store.GetBead(ctx, taskID)
	if err != nil || task == nil || task.Status == model.StatusClosed {
		return false
	}
	a := strings.TrimSpace(task.Assignee)
	return a != "" && (a == beadID || (name != "" && a == name))
}

// callerOccupiesRequesterSeat reports whether the caller is a later occupant
// of the seat that filed esc, on the identity binding of the seat reflag
// (reflag_seat.go): the verified requester's agent bead and the caller's open
// agent bead carry the same seat_key, AND the token's subject occupies that
// seat. Only before an approval: once approved, the ask is its executor's.
func (s *BeadsServer) callerOccupiesRequesterSeat(ctx context.Context, esc *model.Bead) bool {
	f := escalationStringFields(esc)
	req := f["requesting_agent"]
	if f[escalationRequestingAgentVerifiedField] != "true" || req == "" || escalationHasServerApproval(esc) {
		return false
	}
	agent, err := s.callerAgentBead(ctx)
	if err != nil || agent == nil {
		return false
	}
	seat, _, _ := exactFieldString(agent.Fields, "seat_key")
	if seat == "" || !subjectOccupiesSeat(AuthIdentityFromContext(ctx).Subject, seat) {
		return false
	}
	reqBead, err := s.store.GetBead(ctx, req)
	if err != nil || reqBead == nil || reqBead.Type != model.BeadType("agent") {
		return false
	}
	reqSeat, _, _ := exactFieldString(reqBead.Fields, "seat_key")
	return reqSeat == seat
}
