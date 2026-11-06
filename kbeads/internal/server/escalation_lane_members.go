package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"gardenio/kbeads/internal/model"
)

// SetEscalationCoordinatorClients sets BEADS_ESCALATION_COORDINATOR_CLIENTS:
// the Keycloak client ids that may act in a coordinator lane through a
// machine:<id> principal (D4, defence in depth). Empty means none.
func (s *BeadsServer) SetEscalationCoordinatorClients(clients []string) {
	m := make(map[string]struct{}, len(clients))
	for _, c := range clients {
		if c = strings.TrimSpace(c); c != "" {
			m[c] = struct{}{}
		}
	}
	s.coordinatorClients = m
}

func (s *BeadsServer) coordinatorClientAllowed(client string) bool {
	_, ok := s.coordinatorClients[client]
	return ok && client != ""
}

// tenantLandBead returns the project bead for land when it is a tenant land
// (land_kind == "tenant", CONTRACT ISSUE 1), else nil. The lookup reads
// through the store, never the caller's scope.
func (s *BeadsServer) tenantLandBead(ctx context.Context, land string) (*model.Bead, error) {
	if land == "" {
		return nil, nil
	}
	p, err := s.store.FindProjectByName(ctx, land)
	switch {
	case errors.Is(err, sql.ErrNoRows) || (err == nil && p == nil):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("project lookup %q: %w", land, err)
	}
	kind, _, _ := exactFieldString(p.Fields, "land_kind")
	if kind != landKindTenant {
		return nil, nil
	}
	return p, nil
}

func (s *BeadsServer) isTenantLand(ctx context.Context, land string) (bool, error) {
	p, err := s.tenantLandBead(ctx, land)
	return p != nil, err
}

// landDelegatesToSteward reports whether land's owner delegated opType to the
// land's steward (D8: project field land_steward_approves; absent reads as
// [], and the daemon stamps defaultLandStewardApproves on a tenant land's
// create).
// Task 11 declares the field and the steward_delegate action, and calls this
// helper rather than re-implementing it.
func (s *BeadsServer) landDelegatesToSteward(ctx context.Context, land, opType string) (bool, error) {
	p, err := s.tenantLandBead(ctx, land)
	if err != nil || p == nil {
		return false, err
	}
	var f struct {
		Approves []string `json:"land_steward_approves"`
	}
	if len(p.Fields) > 0 {
		if err := json.Unmarshal(p.Fields, &f); err != nil {
			return false, fmt.Errorf("land %q land_steward_approves: %w", land, err)
		}
	}
	return policyListHas(f.Approves, normalizeOperationType(opType)), nil
}

// laneStaffed reports whether a lane could act on esc right now (D5: the
// create-time held_by is the first staffed approver lane). It asks about the
// lane, not about a caller. A presence-tracked machine coordinator staffs its
// lane only while present (gd-t41kYFbLUt); a route or hand-up target is
// chosen with it.
func (s *BeadsServer) laneStaffed(ctx context.Context, lane laneRef, esc *model.Bead, p escalationPolicy) (bool, error) {
	return s.laneStaffedBy(ctx, lane, esc, p, true)
}

// laneMayHold is laneStaffed without presence: the lane has a member that
// could act once it is back. A lane keeps executing and holding what it holds
// while it is away, so an offline laptop never passes its work to the owner.
func (s *BeadsServer) laneMayHold(ctx context.Context, lane laneRef, esc *model.Bead, p escalationPolicy) bool {
	ok, err := s.laneStaffedBy(ctx, lane, esc, p, false)
	return err == nil && ok
}

func (s *BeadsServer) laneStaffedBy(ctx context.Context, lane laneRef, esc *model.Bead, p escalationPolicy, presence bool) (bool, error) {
	switch lane.Kind {
	case laneOperator:
		return true, nil
	case laneCoordinator:
		c, ok := p.Coordinators[lane.Name]
		if !ok {
			return false, nil
		}
		var perr error
		for _, pr := range c.Principals {
			kind, name, _ := strings.Cut(pr, ":")
			if kind == "role" {
				return true, nil
			}
			if kind != "machine" || !s.coordinatorClientAllowed(name) {
				continue
			}
			if !presence {
				return true, nil
			}
			present, err := s.machinePresent(ctx, name)
			if err == nil && present {
				return true, nil
			}
			perr = err
		}
		return false, perr
	case laneSteward:
		if home, err := s.homeStewardAdviceLaneStaffed(ctx, lane.Name, esc); err != nil || home {
			return home, err
		}
		return s.landDelegatesToSteward(ctx, lane.Name, parseSpawnEscalationFields(esc).OperationType)
	case laneLandOwner:
		return s.isTenantLand(ctx, lane.Name)
	case laneRequester:
		v, _, _ := exactFieldString(esc.Fields, escalationRequestingAgentVerifiedField)
		return v == "true", nil
	}
	// server never approves; legacy entries are not routable lanes.
	return false, nil
}

// firstStaffedLane returns the first staffed lane among approvers, else
// "operator". Unknown refs and lookup errors are skipped with a WARN, so they
// can only push routing toward the operator. A coordinator lane that is away
// (laneMayHold, not present) is passed over for a later agent lane, never for
// an owner lane: it holds rather than add to the owner's asks (gd-t41kYFbLUt).
func (s *BeadsServer) firstStaffedLane(ctx context.Context, esc *model.Bead, approvers []string, p escalationPolicy) string {
	away := ""
	for _, a := range approvers {
		lane, err := parseLaneRef(a)
		if err != nil {
			slog.Warn("escalation routing: unknown lane ref in approvers; skipped", "escalation", esc.ID, "ref", a, "error", err)
			continue
		}
		staffed, err := s.laneStaffed(ctx, lane, esc, p)
		if err != nil {
			slog.Warn("escalation routing: could not resolve lane staffing; skipped", "escalation", esc.ID, "lane", lane.String(), "error", err)
			continue
		}
		if staffed && away != "" && (lane.Kind == laneOperator || lane.Kind == laneLandOwner) {
			return away
		}
		if staffed {
			return lane.String()
		}
		if away == "" && lane.Kind == laneCoordinator && s.laneMayHold(ctx, lane, esc, p) {
			away = lane.String()
		}
	}
	if away != "" {
		return away
	}
	return string(laneOperator)
}

const approvalMethodCoordinator = "coordinator"

// callerInLane reports whether the authenticated caller is a member of lane
// for esc (D2). The shared static token and X-Operated-By relays are members
// of no lane. A legacy ref is never a lane: humanSatisfiesApprovers matches it.
func (s *BeadsServer) callerInLane(ctx context.Context, lane laneRef, esc *model.Bead, p escalationPolicy) (bool, error) {
	switch lane.Kind {
	case laneOperator:
		_, human := s.escalationApprover(ctx)
		return human && callerMayApproveAsAdmin(ctx), nil
	case laneCoordinator:
		c, ok := p.Coordinators[lane.Name]
		if !ok {
			return false, nil
		}
		for _, pr := range c.Principals {
			in, err := s.callerIsPrincipal(ctx, pr)
			if err != nil || in {
				return in, err
			}
		}
		return false, nil
	case laneSteward:
		agent, err := s.callerAgentBead(ctx)
		if err != nil || agent == nil {
			return false, err
		}
		if !slices.Contains(parseRoleList(agentRole(agent)), "steward") || projectFromFields(agent.Fields) != lane.Name {
			return false, nil
		}
		// The home garden's steward, for an advice ask (escalation_steward_advice.go).
		if home, err := s.homeStewardAdviceLaneMember(ctx, lane.Name, esc); err != nil || home {
			return home, err
		}
		return s.landDelegatesToSteward(ctx, lane.Name, parseSpawnEscalationFields(esc).OperationType)
	case laneLandOwner:
		if _, human := s.escalationApprover(ctx); !human {
			return false, nil
		}
		if groupsToLandRoles(GroupsFromContext(ctx))[lane.Name] != landRoleOwner {
			return false, nil
		}
		return s.isTenantLand(ctx, lane.Name)
	case laneRequester:
		beadID, ok := agentBeadIDFromContext(ctx)
		if !ok {
			return false, nil
		}
		req, _, _ := exactFieldString(esc.Fields, "requesting_agent")
		verified, _, _ := exactFieldString(esc.Fields, escalationRequestingAgentVerifiedField)
		return verified == "true" && req != "" && req == beadID, nil
	case laneServer:
		return isMachineWrite(ctx), nil
	}
	return false, nil
}

// callerIsPrincipal matches one coordinator principal: machine:<client> (a
// machine principal whose client is also in BEADS_ESCALATION_COORDINATOR_CLIENTS)
// or role:<role> (an agent on its own per-agent token whose open agent bead
// carries the role).
func (s *BeadsServer) callerIsPrincipal(ctx context.Context, principal string) (bool, error) {
	kind, name, _ := strings.Cut(principal, ":")
	switch kind {
	case "machine":
		if authMethodFromContext(ctx) != authMethodMachine {
			return false, nil
		}
		return AuthIdentityFromContext(ctx).ClientID == name && s.coordinatorClientAllowed(name), nil
	case "role":
		agent, err := s.callerAgentBead(ctx)
		if err != nil || agent == nil {
			return false, err
		}
		return slices.Contains(parseRoleList(agentRole(agent)), name), nil
	}
	return false, nil
}

// callerAgentBead returns the caller's own open agent bead when it is on a
// per-agent token, else nil. The static token's X-Actor is a claim and never
// reaches here (agentBeadIDFromContext).
func (s *BeadsServer) callerAgentBead(ctx context.Context) (*model.Bead, error) {
	beadID, ok := agentBeadIDFromContext(ctx)
	if !ok {
		return nil, nil
	}
	b, err := s.store.GetBead(ctx, beadID)
	switch {
	case errors.Is(err, sql.ErrNoRows) || (err == nil && b == nil):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("agent bead %s: %w", beadID, err)
	}
	if b.Type != model.BeadType("agent") || b.Status == model.StatusClosed {
		return nil, nil
	}
	return b, nil
}

func agentRole(b *model.Bead) string {
	r, _, _ := exactFieldString(b.Fields, agentRoleField)
	return r
}

// callerIsRequester is D4's self-approval check: the caller's agent bead is
// the requesting agent, or its principal subject is the creator or the
// recorded requester (a machine principal filing its own escalation).
func (s *BeadsServer) callerIsRequester(ctx context.Context, esc *model.Bead) bool {
	req, _, _ := exactFieldString(esc.Fields, "requesting_agent")
	if beadID, ok := agentBeadIDFromContext(ctx); ok && req != "" && beadID == req {
		return true
	}
	subj := AuthIdentityFromContext(ctx).Subject
	return subj != "" && (subj == esc.CreatedBy || subj == req)
}

// escalationOperatorOnly re-derives D3 at decision time as well as reading the
// stamp, so a missing or stale operator_only never opens a floor escalation.
// The floor is escalationForcedReason, the same one #756's audience and the
// create stamp use; only its floor reasons are operator-only (a named person
// or a spawn keeps its own approvers).
func (s *BeadsServer) escalationOperatorOnly(ctx context.Context, esc *model.Bead) bool {
	if isEscalationFloorReason(s.escalationForcedReason(ctx, esc)) {
		return true
	}
	for _, v := range escalationFieldValues(esc.Fields, []string{"operator_only"}) {
		if b, ok := v.(bool); ok && b {
			return true
		}
	}
	return false
}

// approveCheck is checkMayApprove's verdict. Method is what the approval would
// be stamped with ("web_ui" for a human, "coordinator"); As is the lane it acts
// in; Reason is the refusal text.
type approveCheck struct {
	Allowed bool
	Method  string
	As      string
	Reason  string
}

// checkMayApprove answers "may this caller approve esc?" with the same checks
// handleApproveEscalation runs. POST .../can-approve returns it, so the viewer
// shows buttons only when the click would succeed.
func (s *BeadsServer) checkMayApprove(ctx context.Context, esc *model.Bead) approveCheck {
	if esc == nil || esc.Type != model.TypeEscalation {
		return approveCheck{Reason: "not an escalation"}
	}
	if esc.Status != model.StatusOpen && esc.Status != model.StatusInProgress {
		return approveCheck{Reason: "escalation is already " + string(esc.Status)}
	}
	if allowed, reason := s.scopedApproverAllowed(ctx, esc); !allowed {
		return approveCheck{Reason: reason}
	}
	if approvedBy, human := s.escalationApprover(ctx); human {
		return s.checkHumanMayApprove(ctx, esc, approvedBy)
	}
	return s.checkCoordinatorMayApprove(ctx, esc)
}

// checkHumanMayApprove is the unchanged human path: four-eyes, approvers,
// operator-only, spawn policy, replay write policy, scoped replay. The
// refusal texts are the handler's 403 messages.
func (s *BeadsServer) checkHumanMayApprove(ctx context.Context, esc *model.Bead, approvedBy string) approveCheck {
	if escalationRequiresFourEyes(esc) && s.escalationFourEyesViolation(ctx, esc, approvedBy) {
		return approveCheck{Reason: "four-eyes required: approver must differ from the requester"}
	}
	approvers, err := escalationApprovers(esc)
	if err != nil {
		return approveCheck{Reason: "escalation approvers field is malformed; refusing to approve"}
	}
	as := ""
	if len(approvers) > 0 {
		matched, ok, err := s.humanSatisfiesApprovers(ctx, esc, approvedBy, approvers)
		if err != nil {
			return approveCheck{Reason: "failed to resolve approver's teams"}
		}
		if !ok {
			return approveCheck{Reason: "approval requires being named in, or a member of a team named in, this escalation's approvers"}
		}
		as = matched
	}
	if s.escalationOperatorOnly(ctx, esc) && as != string(laneOperator) {
		if in, _ := s.callerInLane(ctx, laneRef{Kind: laneOperator, Raw: string(laneOperator)}, esc, escalationPolicy{}); !in {
			return approveCheck{Reason: "this escalation is operator-only (secrets, credentials or approval rights)"}
		}
		as = string(laneOperator)
	}
	for _, gate := range []func(context.Context, *model.Bead) (string, error){
		func(ctx context.Context, b *model.Bead) (string, error) {
			return s.spawnApprovalDenial(ctx, b, approvedBy)
		},
		s.replayWritePolicyDenial,
		s.replayKillSwitchDenial, // #762
		s.scopedReplayDenial,
	} {
		denial, err := gate(ctx, esc)
		if err != nil {
			return approveCheck{Reason: "could not resolve who may approve this escalation; refusing to approve"}
		}
		if denial != "" {
			return approveCheck{Reason: denial}
		}
	}
	return approveCheck{Allowed: true, Method: "web_ui", As: as}
}

// humanSatisfiesApprovers matches a human approver against an approvers list
// that may mix lane refs and legacy entries (CONTRACT ISSUE 5). operator and
// land-owner:<land> go to callerInLane. Coordinator, steward, requester and
// server are never human lanes. Everything else, unparseable entries
// included, keeps today's escalationApproverListed matching. as is the lane
// matched ("" for a legacy entry).
func (s *BeadsServer) humanSatisfiesApprovers(ctx context.Context, esc *model.Bead, approvedBy string, approvers []string) (as string, ok bool, err error) {
	var legacy []string
	for _, a := range approvers {
		lane, perr := parseLaneRef(a)
		if perr != nil || lane.Kind == laneLegacy {
			legacy = append(legacy, a)
			continue
		}
		if lane.Kind != laneOperator && lane.Kind != laneLandOwner {
			continue
		}
		in, err := s.callerInLane(ctx, lane, esc, escalationPolicy{})
		if err != nil {
			return "", false, err
		}
		if in {
			return lane.String(), true, nil
		}
	}
	if len(legacy) == 0 {
		return "", false, nil
	}
	ok, err = s.escalationApproverListed(ctx, approvedBy, legacy)
	return "", ok, err
}

// checkCoordinatorMayApprove is D4: not operator-only, the caller in the
// coordinator or steward lane that holds the escalation, that lane listed in
// approvers, and not the requester. An unstamped escalation has no held_by,
// so it is refused (fail closed).
func (s *BeadsServer) checkCoordinatorMayApprove(ctx context.Context, esc *model.Bead) approveCheck {
	if s.escalationOperatorOnly(ctx, esc) {
		return approveCheck{Reason: "this escalation is operator-only (secrets, credentials or approval rights); a coordinator may not approve it"}
	}
	approvers, err := escalationApprovers(esc)
	if err != nil {
		return approveCheck{Reason: "escalation approvers field is malformed; refusing to approve"}
	}
	if s.callerIsRequester(ctx, esc) {
		return approveCheck{Reason: "a coordinator may not approve an escalation it requested"}
	}
	p, err := s.activeEscalationPolicy(ctx)
	if err != nil {
		slog.Warn("escalation approve: policy unreadable; refusing coordinator approval", "escalation", esc.ID, "error", err)
		return approveCheck{Reason: "could not read the escalation policy; refusing coordinator approval"}
	}
	heldRef, _, _ := exactFieldString(esc.Fields, "held_by")
	held, herr := parseLaneRef(strings.TrimSpace(heldRef))
	if herr != nil || (held.Kind != laneCoordinator && held.Kind != laneSteward) {
		return approveCheck{Reason: "a coordinator approves only an escalation its lane holds; this one is held by " + strconvQuote(heldRef)}
	}
	for _, a := range approvers {
		lane, err := parseLaneRef(a)
		if err != nil {
			slog.Warn("escalation approve: unknown lane ref in approvers; skipped", "escalation", esc.ID, "ref", a, "error", err)
			continue
		}
		if lane.String() != held.String() {
			continue // D4: only the holding lane, and only if it is an approver
		}
		in, err := s.callerInLane(ctx, lane, esc, p)
		if err != nil {
			slog.Warn("escalation approve: lane membership unresolvable; refusing", "escalation", esc.ID, "lane", lane.String(), "error", err)
			return approveCheck{Reason: "could not resolve lane membership; refusing to approve"}
		}
		if in {
			denial, err := s.coordinatorReplayDenial(ctx, esc)
			if err != nil {
				slog.Warn("escalation approve: coordinator replay gate unresolvable; refusing", "escalation", esc.ID, "error", err)
				return approveCheck{Reason: "could not resolve the write policy for this escalation's operation; refusing to approve"}
			}
			if denial != "" {
				return approveCheck{Reason: denial}
			}
			return approveCheck{Allowed: true, Method: approvalMethodCoordinator, As: lane.String()}
		}
	}
	return approveCheck{Reason: "caller is not in the approver lane that holds this escalation (" + held.String() + "); ask an operator to route it (gb escalation route)"}
}

// coordinatorRefusedOperations are never approvable by a coordinator: a taint
// grant widens access fleet-wide and its replay has no write-policy check
// (replayWritePolicyDenial's default arm).
var coordinatorRefusedOperations = map[string]bool{"grant": true}

// coordinatorReplayDenial is the coordinator's version of the human
// spawn-policy and replay gates (CONTRACT ISSUE 12). It refuses grants and
// privileged spawns outright, then checks the replayed write against the
// coordinator's own roles. A non-OIDC principal has no ladder role, so any
// min_role type is refused.
func (s *BeadsServer) coordinatorReplayDenial(ctx context.Context, esc *model.Bead) (string, error) {
	f := parseSpawnEscalationFields(esc)
	if coordinatorRefusedOperations[normalizeOperationType(f.OperationType)] {
		return "a coordinator may not approve a " + strconvQuote(f.OperationType) + " escalation", nil
	}
	role, isSpawn, rerr := escalationSpawnTarget(f)
	if isSpawn {
		if rerr != nil {
			return "a coordinator may not approve a spawn whose role cannot be resolved", nil
		}
		privileged := isNeverAutoApproveRole(role)
		if !privileged {
			var err error
			if privileged, err = roleIsPrivileged(ctx, s.store, role); err != nil {
				return "", err
			}
		}
		if privileged {
			return "a coordinator may not approve a spawn of privileged role " + strconvQuote(role), nil
		}
	}
	if denial, err := s.replayWritePolicyDenial(ctx, esc); err != nil || denial != "" {
		return denial, err
	}
	// #762: a coordinator may not throw a land kill switch by approving.
	if denial, err := s.replayKillSwitchDenial(ctx, esc); err != nil || denial != "" {
		return denial, err
	}
	return s.scopedReplayDenial(ctx, esc)
}
