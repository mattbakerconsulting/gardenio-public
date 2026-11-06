package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// Claim on a spawned agent's behalf (gd-1yeWlbJUJi, decision gd-P7dxTFCWmt
// "equivalence"). When the trigger engine spawns an agent for an escalation
// event, it stamps claimed_by=<agent bead> on the escalation, so the row names
// its worker from the first second. A coordinator run calls the escalation
// verbs as its role's machine client (--as-machine), so /claim and the
// executor grant treat a claim by agent bead A as held by machine:<M> when one
// of A's role beads mounts machine_client M. A's pod holds M's secret, so the
// equivalence gives M nothing it did not already have.

type claimEscalationForRequest struct {
	Agent string `json:"agent"`
}

// handleClaimEscalationFor handles POST /v1/escalations/{id}/claim-for. Only
// machine:gardenio-trigger-engine holding capTriggerLineage may call it, and
// only for an open agent bead a trigger spawned (spawned_by trigger:<id>) that
// is a member of the escalation's held_by coordinator lane
// (agentInCoordinatorLane). A claim held by anyone else is a 409; a repeat for
// the same agent writes nothing.
func (s *BeadsServer) handleClaimEscalationFor(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !machineHasCapability(ctx, capTriggerLineage) || AuthIdentityFromContext(ctx).Subject != triggerEngineSubject {
		writeError(w, http.StatusForbidden, "only the trigger engine may claim on an agent's behalf")
		return
	}
	id := r.PathValue("id")
	var req claimEscalationForRequest
	if id == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Agent = strings.TrimSpace(req.Agent); req.Agent == "" {
		writeError(w, http.StatusBadRequest, "agent is required")
		return
	}
	esc, ok := s.fetchResolutionTarget(w, r, id, "escalation", "handleClaimEscalationFor")
	if !ok || !openEscalation(w, esc) {
		return
	}
	agent, err := s.store.GetBead(ctx, req.Agent)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		writeServerError(ctx, w, "handleClaimEscalationFor", err, "failed to read the agent bead")
		return
	}
	if agent == nil || agent.Type != model.BeadType("agent") || agent.Status == model.StatusClosed ||
		!strings.HasPrefix(fieldsStringValue(agent.Fields, "spawned_by"), "trigger:") {
		writeError(w, http.StatusForbidden, req.Agent+" is not an open agent a trigger spawned")
		return
	}
	lane, err := parseLaneRef(escalationHeldBy(esc))
	if err != nil || lane.Kind != laneCoordinator {
		writeError(w, http.StatusForbidden, "held_by "+escalationHeldBy(esc)+" is not a coordinator lane")
		return
	}
	in, err := s.agentInCoordinatorLane(ctx, agent, lane)
	if err != nil || !in {
		writeError(w, http.StatusForbidden, req.Agent+" is not a member of "+lane.String()+" (held_by)")
		return
	}
	if prev, _, _ := exactFieldString(esc.Fields, "claimed_by"); prev == agent.ID {
		writeJSON(w, http.StatusOK, esc)
		return
	} else if prev != "" {
		writeError(w, http.StatusConflict, "already claimed by "+prev)
		return
	}
	if err := s.mergeBeadFields(ctx, id, map[string]any{
		"claimed_by": agent.ID,
		"claimed_at": time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		writeServerError(ctx, w, "handleClaimEscalationFor", err, "failed to claim escalation")
		return
	}
	bead, err := s.store.GetBead(ctx, id)
	if err != nil || bead == nil {
		writeServerError(ctx, w, "handleClaimEscalationFor", err, "failed to re-read escalation")
		return
	}
	s.recordAndPublish(ctx, events.TopicBeadUpdated, id, triggerEngineSubject, events.BeadUpdated{
		Bead: bead, Changes: map[string]any{"claimed_by": agent.ID},
	})
	writeJSON(w, http.StatusOK, bead)
}

// agentInCoordinatorLane is callerInLane's coordinator arm for agent rather
// than the caller: a role:<R> principal matches a role on the agent bead, a
// machine:<M> principal an allowed coordinator client M that one of its open
// role beads mounts.
func (s *BeadsServer) agentInCoordinatorLane(ctx context.Context, agent *model.Bead, lane laneRef) (bool, error) {
	p, err := s.activeEscalationPolicy(ctx)
	if err != nil {
		return false, err
	}
	c, ok := p.Coordinators[lane.Name]
	if !ok {
		return false, nil
	}
	for _, pr := range c.Principals {
		kind, name, _ := strings.Cut(pr, ":")
		switch {
		case kind == "role" && slices.Contains(parseRoleList(agentRole(agent)), name):
			return true, nil
		case kind == "machine" && s.coordinatorClientAllowed(name):
			if in, err := s.agentMountsMachineClient(ctx, agent, name); err != nil || in {
				return in, err
			}
		}
	}
	return false, nil
}

// agentMountsMachineClient reports whether one of agent's open role beads
// names client as its machine_client: the client whose secret the controller
// mounts into the agent's pod (applyRoleMachineClient).
func (s *BeadsServer) agentMountsMachineClient(ctx context.Context, agent *model.Bead, client string) (bool, error) {
	roles := parseRoleList(agentRole(agent))
	if client == "" || len(roles) == 0 {
		return false, nil
	}
	rbs, err := roleBeadsByTitle(ctx, s.store, roles)
	if err != nil {
		return false, fmt.Errorf("role beads of %s: %w", agent.ID, err)
	}
	return slices.ContainsFunc(rbs, func(rb *model.Bead) bool {
		return rb.Status != model.StatusClosed && strings.TrimSpace(fieldsStringValue(rb.Fields, "machine_client")) == client
	}), nil
}

// claimHeldByCaller reports whether claimant, an escalation's claimed_by, is
// the caller: laneActor itself, or, for a machine caller, an agent bead whose
// role mounts the caller's client. A closed agent bead still counts: the
// equivalence is identity, not liveness.
func (s *BeadsServer) claimHeldByCaller(ctx context.Context, claimant string) (bool, error) {
	if claimant != "" && claimant == laneActor(ctx) {
		return true, nil
	}
	client := AuthIdentityFromContext(ctx).ClientID
	if authMethodFromContext(ctx) != authMethodMachine || client == "" || claimant == "" || strings.ContainsAny(claimant, ":@") {
		return false, nil
	}
	agent, err := s.store.GetBead(ctx, claimant)
	switch {
	case errors.Is(err, sql.ErrNoRows) || (err == nil && (agent == nil || agent.Type != model.BeadType("agent"))):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("claimant %s: %w", claimant, err)
	}
	return s.agentMountsMachineClient(ctx, agent, client)
}

// callerClaimCandidates lists the open escalations whose claim may be the
// caller's: those claimed by laneActor, and for a machine caller those
// executed by a coordinator lane naming its client, which an agent of its
// role may hold (claimHeldByCaller decides each).
func (s *BeadsServer) callerClaimCandidates(ctx context.Context, actor string) ([]*model.Bead, error) {
	list := func(field, value string) ([]*model.Bead, error) {
		escs, _, err := s.store.ListBeads(ctx, model.BeadFilter{
			Type: []model.BeadType{model.TypeEscalation}, Status: openEscalationStatuses,
			Fields: map[string]string{field: value},
		})
		return escs, err
	}
	out, err := list("claimed_by", actor)
	if err != nil || authMethodFromContext(ctx) != authMethodMachine {
		return out, err
	}
	p, err := s.activeEscalationPolicy(ctx)
	if err != nil {
		return nil, fmt.Errorf("escalation policy: %w", err)
	}
	principal := machineSubjectPrefix + AuthIdentityFromContext(ctx).ClientID
	for name, c := range p.Coordinators {
		if !slices.Contains(c.Principals, principal) {
			continue
		}
		escs, err := list("execute_by", string(laneCoordinator)+":"+name)
		if err != nil {
			return nil, err
		}
		for _, e := range escs {
			if !slices.ContainsFunc(out, func(o *model.Bead) bool { return o.ID == e.ID }) {
				out = append(out, e)
			}
		}
	}
	return out, nil
}
