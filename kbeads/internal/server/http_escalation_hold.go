package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// heldByMe in a held_by filter means "every lane the caller is a member of",
// resolved per escalation by callerInLane (CONTRACT ISSUE 20).
const heldByMe = "@me"

// escalationRouteSweepActor is who an idle hand-up is recorded as.
const escalationRouteSweepActor = "system:escalation-route"

// errNothingAbove is a hand-up from the top of the chain: the operator, or a
// tenant land's owner (CONTRACT ISSUE 25).
var errNothingAbove = errors.New("nothing above this lane")

// escalationHeldBy reads held_by. An escalation filed before routing shipped
// has none and is held by the operator (Global Constraint 7).
func escalationHeldBy(b *model.Bead) string {
	v, _, _ := exactFieldString(b.Fields, "held_by")
	if v = strings.TrimSpace(v); v == "" {
		return string(laneOperator)
	}
	return v
}

// laneActor is who a claim or hand-up is recorded as: the agent bead id on a
// per-agent token (stable across renames), else the principal subject.
func laneActor(ctx context.Context) string {
	if beadID, ok := agentBeadIDFromContext(ctx); ok {
		return beadID
	}
	return AuthIdentityFromContext(ctx).Subject
}

// callerHoldsEscalation reports whether the caller is a member of esc's
// held_by lane. An unparseable held_by is read as operator (fail closed).
func (s *BeadsServer) callerHoldsEscalation(ctx context.Context, esc *model.Bead) (laneRef, bool, error) {
	lane, err := parseLaneRef(escalationHeldBy(esc))
	if err != nil {
		slog.Warn("escalation routing: unparseable held_by; treating as operator", "escalation", esc.ID, "held_by", escalationHeldBy(esc), "error", err)
		lane = laneRef{Kind: laneOperator, Raw: string(laneOperator)}
	}
	p, err := s.activeEscalationPolicy(ctx)
	if err != nil {
		return lane, false, err
	}
	in, err := s.callerInLane(ctx, lane, esc, p)
	return lane, in, err
}

// handUpTarget is D5's hand-up destination: land-owner:<land> for a tenant
// land's escalation, else operator. Already at the top is errNothingAbove.
func (s *BeadsServer) handUpTarget(ctx context.Context, esc *model.Bead, from laneRef) (string, error) {
	if f := s.escalationFactsFor(ctx, esc); f.Land != "" {
		owner := string(laneLandOwner) + ":" + f.Land
		if from.String() == owner {
			return "", errNothingAbove
		}
		return owner, nil
	}
	if from.Kind == laneOperator {
		return "", errNothingAbove
	}
	return string(laneOperator), nil
}

// idleHandUpTarget is P10.F2: where the idle sweep hands esc up from the lane
// that idled. An escalation off the floor and still awaiting approval goes to
// the next staffed coordinator or steward lane after from in its approvers,
// so the chain only moves forward and ends at operator. Otherwise (a land, a
// floor or operator-only item, an approved one, or from not among the
// approvers) it is handUpTarget, as for the manual verb. sideways reports a
// lane other than handUpTarget's.
func (s *BeadsServer) idleHandUpTarget(ctx context.Context, esc *model.Bead, from laneRef, p escalationPolicy) (target string, sideways bool, err error) {
	if s.escalationFactsFor(ctx, esc).Land == "" && !escalationHasServerApproval(esc) &&
		s.escalationForcedReason(ctx, esc) == "" && !s.escalationOperatorOnly(ctx, esc) {
		approvers, _ := escalationApprovers(esc)
		at := slices.IndexFunc(approvers, func(a string) bool {
			lane, err := parseLaneRef(a)
			return err == nil && lane.String() == from.String()
		})
		for i := at + 1; at >= 0 && i < len(approvers); i++ {
			lane, err := parseLaneRef(approvers[i])
			if err != nil || (lane.Kind != laneCoordinator && lane.Kind != laneSteward) {
				continue
			}
			if staffed, err := s.laneStaffed(ctx, lane, esc, p); err == nil && staffed {
				return lane.String(), true, nil
			}
		}
	}
	target, err = s.handUpTarget(ctx, esc, from)
	return target, false, err
}

// clusterCoordinatorLane executes approved bead-only escalations
// (escalation routing Task 15).
const clusterCoordinatorLane = "coordinator:cluster"

// handUpExecuteBy is P7.1: the lane that executes esc once the idle sweep has
// handed it up to the operator from the lane that idled. Only a coordinator
// execute_by is moved, since server, requester and the owner lanes do not
// idle, and never a verdict record's (a merge): nothing executes it, and its
// lane is the one that records the verdict. A bead-only escalation off the
// floor goes to coordinator:cluster when the policy has that lane, it is
// staffed, and it is not the lane that idled; anything else goes to operator.
// "" keeps execute_by as it is.
func (s *BeadsServer) handUpExecuteBy(ctx context.Context, esc *model.Bead, from laneRef, p escalationPolicy) string {
	exec, _, _ := exactFieldString(esc.Fields, "execute_by")
	op := normalizeOperationType(parseSpawnEscalationFields(esc).OperationType)
	if lane, err := parseLaneRef(exec); err != nil || lane.Kind != laneCoordinator || isVerdictOperation(op) {
		return ""
	}
	return s.executionLane(ctx, esc, from, p)
}

// executionLane is the first staffed lane that may execute esc, other than
// from: coordinator:cluster for a bead-only escalation off the floor when the
// policy has that lane, else operator. P7.1's hand-up and P7.3's refused
// executor pod (refuseExecutorPod) share it.
func (s *BeadsServer) executionLane(ctx context.Context, esc *model.Bead, from laneRef, p escalationPolicy) string {
	op := normalizeOperationType(parseSpawnEscalationFields(esc).OperationType)
	cluster := laneRef{Kind: laneCoordinator, Name: "cluster", Raw: clusterCoordinatorLane}
	if from.String() == clusterCoordinatorLane || !isBeadOnlyOperation(op) ||
		s.escalationForcedReason(ctx, esc) != "" || s.escalationOperatorOnly(ctx, esc) {
		return string(laneOperator)
	}
	if staffed, err := s.laneStaffed(ctx, cluster, esc, p); err != nil || !staffed {
		return string(laneOperator)
	}
	return clusterCoordinatorLane
}

// retargetExecution applies P7.1 to fields for esc, which is leaving the lane
// from: execute_by moves to handUpExecuteBy's lane, and an approved
// escalation is then held by its new executor, as approval holds it (D5), so
// that lane can claim it. fields carries held_by on entry. It returns the
// note for the reason, or "" when execute_by stays.
func (s *BeadsServer) retargetExecution(ctx context.Context, esc *model.Bead, from laneRef, p escalationPolicy, fields map[string]any) string {
	exec := s.handUpExecuteBy(ctx, esc, from, p)
	was, _, _ := exactFieldString(esc.Fields, "execute_by")
	if was = strings.TrimSpace(was); exec == "" || exec == was {
		return ""
	}
	// A lane that idled on an unapproved item idled on its hand-up for
	// approval, which says nothing about whether it can execute. A staffed
	// lane keeps executing it, and approval hands it back (D5), rather than
	// a person executing what a machine lane could (gd-YnsSlh9239). Moving
	// to another machine lane is still P7.1's.
	if exec == string(laneOperator) && was == from.String() && !escalationHasServerApproval(esc) &&
		s.laneMayHold(ctx, from, esc, p) {
		return ""
	}
	fields["execute_by"] = exec
	if escalationHasServerApproval(esc) {
		fields["held_by"] = exec
	}
	held, _ := fields["held_by"].(string)
	fields[escalationAudienceField] = laneAudience(held, p) // D11
	return "execution moved from " + was + " to " + exec
}

// operatorClassFloorStamp is the floor a PR merge moves onto when its holder
// names an operator class (P10.F4, gd-QiDj0JvkFr): the routing
// escalationRouteFieldsFor gives a floor reason, with execute_by kept, since
// the desk still merges once the owner approves.
func operatorClassFloorStamp(class string) map[string]any {
	return map[string]any{
		"approvers": []string{string(laneOperator)}, "operator_only": true,
		"policy_rule": "floor:" + operatorClassMergeReason, operatorClassField: class,
	}
}

// applyHandUp moves held_by up one level (the sweep: idleHandUpTarget's lane),
// with its audience (D11), and stamps
// the hand-up plus a pingback, so the bridge re-notifies the new holder. It clears any claim and
// publishes beads.bead.updated. The manual verb and the idle sweep share it;
// only the sweep passes retarget (P7.1, retargetExecution), so a lane that
// hands up for approval keeps executing. A non-nil floor
// (operatorClassFloorStamp) is written with it and sends the escalation to
// operator, whose lane the floor approvers name.
func (s *BeadsServer) applyHandUp(ctx context.Context, esc *model.Bead, from laneRef, actor, reason string, retarget bool, floor map[string]any) (*model.Bead, error) {
	p, err := s.activeEscalationPolicy(ctx)
	if err != nil {
		p = defaultEscalationPolicy() // the hand-up targets are operator or land-owner, both owner in any policy
	}
	target, sideways := "", false
	switch {
	case floor != nil:
		target = string(laneOperator)
	case retarget:
		target, sideways, err = s.idleHandUpTarget(ctx, esc, from, p)
	default:
		target, err = s.handUpTarget(ctx, esc, from)
	}
	if err != nil {
		return nil, err
	}
	if sideways {
		reason += "; to " + target
	}
	now := time.Now().UTC().Format(time.RFC3339)
	count := 0
	if v, _, _ := exactFieldString(esc.Fields, "pingback_count"); v != "" {
		count, _ = strconv.Atoi(v)
	}
	extra := map[string]any{"held_by": target, escalationAudienceField: laneAudience(target, p)} // D11
	if retarget && (sideways || target == string(laneOperator)) {
		if note := s.retargetExecution(ctx, esc, from, p, extra); note != "" {
			reason += "; " + note
		}
	}
	s.noteOfflineHolder(ctx, esc, extra, p)
	for k, v := range floor {
		extra[k] = v
	}
	for k, v := range map[string]any{
		"held_at": now, "handed_up_by": actor, "handed_up_at": now, "hand_up_reason": reason,
		"claimed_by": "", "claimed_at": "",
		"pingback_requested_at": now, "pingback_count": strconv.Itoa(count + 1),
		"pingback_by": actor, "pingback_source": "hand-up", "pingback_reason": reason,
	} {
		extra[k] = v
	}
	if err := s.mergeBeadFields(ctx, esc.ID, extra); err != nil {
		return nil, err
	}
	bead, err := s.store.GetBead(ctx, esc.ID)
	if err != nil || bead == nil {
		return nil, fmt.Errorf("re-read escalation %s after hand-up: %w", esc.ID, err)
	}
	changes := map[string]any{"held_by": extra["held_by"], "handed_up_by": actor}
	if exec, ok := extra["execute_by"]; ok {
		changes["execute_by"] = exec
	}
	if rule, ok := floor["policy_rule"]; ok {
		changes["policy_rule"] = rule
	}
	s.recordAndPublish(ctx, events.TopicBeadUpdated, bead.ID, actor, events.BeadUpdated{
		Bead:    bead,
		Changes: changes,
	})
	return bead, nil
}

func openEscalation(w http.ResponseWriter, esc *model.Bead) bool {
	if esc.Status != model.StatusOpen && esc.Status != model.StatusInProgress {
		writeError(w, http.StatusConflict, "escalation is already "+string(esc.Status))
		return false
	}
	return true
}

// handleClaimEscalation handles POST /v1/escalations/{id}/claim (D5). Only a
// member of held_by may claim. Status is left alone (gardenhub reads
// in_progress as approved), and so is held_by.
func (s *BeadsServer) handleClaimEscalation(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	esc, ok := s.fetchResolutionTarget(w, r, id, "escalation", "handleClaimEscalation")
	if !ok || !openEscalation(w, esc) {
		return
	}
	lane, in, err := s.callerHoldsEscalation(r.Context(), esc)
	if err != nil {
		slog.Warn("handleClaimEscalation: lane membership unresolvable; refusing", "escalation", id, "error", err)
		writeError(w, http.StatusForbidden, "could not resolve lane membership; refusing")
		return
	}
	if !in {
		writeError(w, http.StatusForbidden, "only a member of "+lane.String()+" (held_by) may claim this escalation")
		return
	}
	actor := laneActor(r.Context())
	if prev, _, _ := exactFieldString(esc.Fields, "claimed_by"); prev != "" && prev != actor {
		// An agent of the caller's machine role holds it (claim-for): the
		// caller holds it, and the row keeps naming that agent.
		if held, err := s.claimHeldByCaller(r.Context(), prev); err != nil || !held {
			writeError(w, http.StatusConflict, "already claimed by "+prev)
		} else {
			writeJSON(w, http.StatusOK, esc)
		}
		return
	}
	if err := s.mergeBeadFields(r.Context(), id, map[string]any{
		"claimed_by": actor,
		"claimed_at": time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		writeServerError(r.Context(), w, "handleClaimEscalation", err, "failed to claim escalation")
		return
	}
	bead, err := s.store.GetBead(r.Context(), id)
	if err != nil || bead == nil {
		writeServerError(r.Context(), w, "handleClaimEscalation", err, "failed to re-read escalation")
		return
	}
	s.recordAndPublish(r.Context(), events.TopicBeadUpdated, id, actor, events.BeadUpdated{
		Bead: bead, Changes: map[string]any{"claimed_by": actor},
	})
	writeJSON(w, http.StatusOK, bead)
}

type handUpEscalationRequest struct {
	Reason string `json:"reason"`
	// OperatorClass is the merge desk's OPERATOR-ONLY class, "1"-"3", on a
	// PR merge it holds: the hand-up then puts the merge on the owner floor
	// (operatorClassFloorStamp).
	OperatorClass string `json:"operator_class,omitempty"`
}

const maxHandUpReason = 2000

// handleHandUpEscalation handles POST /v1/escalations/{id}/hand-up (D5). Only
// a member of held_by may hand up. An approved, unclaimed escalation whose
// operation is already satisfied is closed as a success instead, and the
// closed bead returned (gd-Jzu7QMQnZS). With operator_class, only an
// unapproved PR merge may be handed up, and it lands on the owner floor
// (P10.F4, gd-QiDj0JvkFr): approvers operator alone, operator_only, policy_rule
// floor:operator_class_merge. It never widens who may approve.
func (s *BeadsServer) handleHandUpEscalation(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	var req handUpEscalationRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" || len(reason) > maxHandUpReason {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("reason is required (at most %d bytes)", maxHandUpReason))
		return
	}
	class := strings.TrimSpace(req.OperatorClass)
	if class != "" && !validOperatorClass(class) {
		writeError(w, http.StatusBadRequest, "operator_class must be 1, 2 or 3 (docs/merge-desk.md, Sensitive paths)")
		return
	}
	esc, ok := s.fetchResolutionTarget(w, r, id, "escalation", "handleHandUpEscalation")
	if !ok || !openEscalation(w, esc) {
		return
	}
	lane, in, err := s.callerHoldsEscalation(r.Context(), esc)
	if err != nil {
		slog.Warn("handleHandUpEscalation: lane membership unresolvable; refusing", "escalation", id, "error", err)
		writeError(w, http.StatusForbidden, "could not resolve lane membership; refusing")
		return
	}
	if !in {
		writeError(w, http.StatusForbidden, "only a member of "+lane.String()+" (held_by) may hand this escalation up")
		return
	}
	var floor map[string]any
	if class != "" {
		switch {
		case !isDeskMergeOperation(parseSpawnEscalationFields(esc).OperationType):
			writeError(w, http.StatusBadRequest, "operator_class applies only to a PR merge")
			return
		case escalationHasServerApproval(esc):
			writeError(w, http.StatusConflict, "this merge is already approved; an operator class can only be named before approval")
			return
		}
		floor = operatorClassFloorStamp(class)
	}
	if floor == nil && s.closeSatisfiedEscalation(r.Context(), esc, laneActor(r.Context())) {
		closed, err := s.store.GetBead(r.Context(), id)
		if err != nil {
			writeServerError(r.Context(), w, "handleHandUpEscalation", err, "failed to read the closed escalation")
			return
		}
		writeJSON(w, http.StatusOK, closed)
		return
	}
	bead, err := s.applyHandUp(r.Context(), esc, lane, laneActor(r.Context()), reason, false, floor)
	switch {
	case errors.Is(err, errNothingAbove):
		writeError(w, http.StatusConflict, "nothing above "+lane.String())
		return
	case err != nil:
		writeServerError(r.Context(), w, "handleHandUpEscalation", err, "failed to hand up escalation")
		return
	}
	writeJSON(w, http.StatusOK, bead)
}

// filterEscalationsHeldBy keeps the escalations whose held_by (empty =
// operator) is one of refs, or, for "@me", a lane the caller is in. A bad ref
// is a 400.
func (s *BeadsServer) filterEscalationsHeldBy(ctx context.Context, beads []*model.Bead, refs []string) ([]*model.Bead, error) {
	want := map[string]bool{}
	me := false
	for _, r := range refs {
		r = strings.TrimSpace(r)
		if r == heldByMe {
			me = true
			continue
		}
		lane, err := parseLaneRef(r)
		if err != nil || lane.Kind == laneLegacy {
			return nil, inputError("held_by: " + strconvQuote(r) + " is not a lane ref")
		}
		want[lane.String()] = true
	}
	var p escalationPolicy
	if me {
		var err error
		if p, err = s.activeEscalationPolicy(ctx); err != nil {
			return nil, err
		}
	}
	out := make([]*model.Bead, 0, len(beads))
	for _, b := range beads {
		lane, err := parseLaneRef(escalationHeldBy(b))
		if err != nil {
			lane = laneRef{Kind: laneOperator, Raw: string(laneOperator)}
		}
		if want[lane.String()] {
			out = append(out, b)
			continue
		}
		if !me {
			continue
		}
		in, err := s.callerInLane(ctx, lane, b, p)
		if err != nil {
			slog.Warn("held_by=@me: lane membership unresolvable; escalation omitted", "escalation", b.ID, "error", err)
			continue
		}
		if in {
			out = append(out, b)
		}
	}
	return out, nil
}

// escalationLaneIdleHandUp is BEADS_ESCALATION_LANE_IDLE_HANDUP: how long a
// coordinator or steward lane may hold an unclaimed escalation before the
// sweep hands it up. Default 6h; 0 disables; an unparseable value disables
// it, with a WARN.
func escalationLaneIdleHandUp() time.Duration {
	v := strings.TrimSpace(os.Getenv("BEADS_ESCALATION_LANE_IDLE_HANDUP"))
	if v == "" {
		return 6 * time.Hour
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		slog.Warn("BEADS_ESCALATION_LANE_IDLE_HANDUP unparseable; idle hand-up disabled", "value", v, "error", err)
		return 0
	}
	return d
}

// maybeIdleHandUp hands up b when a coordinator or steward lane has held it,
// unclaimed, for longer than idle. A missing or unparseable held_at starts the
// clock instead. An approved b whose operation is already satisfied is closed
// rather than handed up or re-notified (closeSatisfiedEscalation). It reports
// whether b was handed up.
func (s *BeadsServer) maybeIdleHandUp(ctx context.Context, b *model.Bead, idle time.Duration) bool {
	if idle <= 0 {
		return false
	}
	lane, err := parseLaneRef(escalationHeldBy(b))
	if err != nil || (lane.Kind != laneCoordinator && lane.Kind != laneSteward) {
		return false
	}
	if c, _, _ := exactFieldString(b.Fields, "claimed_by"); strings.TrimSpace(c) != "" {
		return false
	}
	at, _, _ := exactFieldString(b.Fields, "held_at")
	heldAt, err := time.Parse(time.RFC3339, strings.TrimSpace(at))
	if err != nil {
		if err := s.mergeBeadFields(ctx, b.ID, map[string]any{"held_at": time.Now().UTC().Format(time.RFC3339)}); err != nil {
			slog.Warn("escalation routing: failed to start the held_at clock", "escalation", b.ID, "error", err)
		}
		return false
	}
	if time.Since(heldAt) < idle {
		return false
	}
	if s.closeSatisfiedEscalation(ctx, b, escalationRouteSweepActor) {
		return false
	}
	reason := fmt.Sprintf("no %s activity for %s", lane.String(), idle)
	if s.idleExecutorKeeps(ctx, b, lane) {
		s.renotifyIdleExecutor(ctx, b, lane, reason)
		return false
	}
	if _, err := s.applyHandUp(ctx, b, lane, escalationRouteSweepActor, reason, true, nil); err != nil {
		if !errors.Is(err, errNothingAbove) {
			slog.Warn("escalation routing: idle hand-up failed", "escalation", b.ID, "error", err)
		}
		return false
	}
	slog.Info("escalation routing: idle hand-up", "escalation", b.ID, "from", lane.String(), "idle", idle)
	return true
}

// idleExecutorKeeps reports whether the idle sweep leaves b with lane: b is
// approved, lane is the staffed lane that executes it, and no other machine
// lane may take it over (P7.1 would only find operator, or keeps a verdict's
// desk). Handing it up would ask a person for an approval b already has, and
// leave a machine lane's work with that person: on 11-04, 35 approved
// escalations sat with operator this way, unclaimable by the lanes that run
// them (gd-YnsSlh9239). The lane is re-notified instead, every idle period,
// so an offline coordinator still cannot hide an escalation.
func (s *BeadsServer) idleExecutorKeeps(ctx context.Context, b *model.Bead, lane laneRef) bool {
	if !escalationHasServerApproval(b) {
		return false
	}
	if exec, _, _ := exactFieldString(b.Fields, "execute_by"); strings.TrimSpace(exec) != lane.String() {
		return false
	}
	p, err := s.activeEscalationPolicy(ctx)
	if err != nil {
		return false
	}
	if !s.laneMayHold(ctx, lane, b, p) { // an away executor keeps its work (gd-t41kYFbLUt)
		return false
	}
	next := s.handUpExecuteBy(ctx, b, lane, p)
	return next == "" || next == string(laneOperator)
}

// renotifyIdleExecutor restarts b's held_at clock and stamps a pingback, so
// the bridge re-notifies lane and the beads.bead.updated event wakes its
// trigger, without moving held_by or execute_by.
func (s *BeadsServer) renotifyIdleExecutor(ctx context.Context, b *model.Bead, lane laneRef, reason string) {
	now := time.Now().UTC().Format(time.RFC3339)
	count := 0
	if v, _, _ := exactFieldString(b.Fields, "pingback_count"); v != "" {
		count, _ = strconv.Atoi(v)
	}
	reason += "; approved and unexecuted, so it stays with " + lane.String() + " (re-notified)"
	fields := map[string]any{
		"held_at": now, "pingback_requested_at": now, "pingback_count": strconv.Itoa(count + 1),
		"pingback_by": escalationRouteSweepActor, "pingback_source": "idle", "pingback_reason": reason,
	}
	if err := s.mergeBeadFields(ctx, b.ID, fields); err != nil {
		slog.Warn("escalation routing: idle re-notify failed", "escalation", b.ID, "error", err)
		return
	}
	bead, err := s.store.GetBead(ctx, b.ID)
	if err != nil || bead == nil {
		return
	}
	s.recordAndPublish(ctx, events.TopicBeadUpdated, bead.ID, escalationRouteSweepActor, events.BeadUpdated{
		Bead:    bead,
		Changes: map[string]any{"pingback_source": "idle"},
	})
	slog.Info("escalation routing: idle executor re-notified", "escalation", b.ID, "lane", lane.String())
}
