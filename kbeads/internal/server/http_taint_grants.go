package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// This file implements the taint_grants/bead_taints write surface, per ADR
// docs/adr/taints.md §7/§5.2/§8.2/§10.4/§4.2 (row: explicit apply):
//
//   - POST   /v1/taints/{name}/grants               handleGrantTaint
//   - DELETE /v1/taints/{name}/grants/{principal}    handleRevokeTaint
//   - POST   /v1/beads/{id}/taints                   handleApplyTaint
//   - GET    /v1/beads/{id}/taints                   handleGetBeadTaints
//
// Taint-domain actor fields (granted_by, applied_by) are ALWAYS server-
// derived, per ADR §11 invariant #1: never client input. applied_by
// (actorFromContext) derives purely from AuthIdentityFromContext.
// granted_by (granterOperatorID) additionally honors a human relayed via a
// sanitized X-Operated-By header — the same narrow "viewer path" precedent
// as decisionAttribution in http_decisions.go — because grant/revoke
// management requires a resolvable human operator (admin role or owner-team
// membership; see callerMayManageTaint) and the bare shared static/fleet
// token carries neither. X-Owner-Override and the broader resolveOperatedBy
// spawn-chain-walk fallback are still never consulted here.

// parsePrincipal splits a combined "<kind>:<value>" principal string, as
// accepted at the CLI/API boundary (docs/adr/taints.md §7). Both halves must
// be non-empty.
func parsePrincipal(raw string) (kind, value string, ok bool) {
	idx := strings.IndexByte(raw, ':')
	if idx <= 0 || idx == len(raw)-1 {
		return "", "", false
	}
	return raw[:idx], raw[idx+1:], true
}

// resolveTeamPrincipal resolves a team principal's value to a type:team
// bead id. Per the pinned coordinator guidance on this task, taint_grants.
// principal stores the bead id (the same id TeamsOf returns) — slug
// resolution belongs at the CLI/viewer boundary, not here, so an unknown or
// non-team bead id is refused rather than guessed at.
func (s *BeadsServer) resolveTeamPrincipal(ctx context.Context, value string) (string, error) {
	if value == "" {
		return "", fmt.Errorf("team principal value is required")
	}
	bead, err := s.store.GetBead(ctx, value)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("unknown team %q", value)
		}
		return "", err
	}
	if bead == nil || bead.Type != model.TypeTeam {
		return "", fmt.Errorf("%q is not a team", value)
	}
	return bead.ID, nil
}

// resolveOperatorPrincipal resolves an operator principal's value to an
// operators(id) rendered as text. A value that already parses as an integer
// is treated as an already-resolved operator id. Otherwise the value is
// parsed as a "<kind>:<subject>" operator-alias string (colon-less input
// defaults to "oidc:", mirroring operatorAliasForIdentity's own bare-email
// default) and looked up — a miss is refused rather than silently creating a
// new operator row, so a typo in a grant request fails closed.
func (s *BeadsServer) resolveOperatorPrincipal(ctx context.Context, value string) (string, error) {
	if value == "" {
		return "", fmt.Errorf("operator principal value is required")
	}
	if _, err := strconv.ParseInt(value, 10, 64); err == nil {
		return value, nil
	}
	kind, subject := splitOperator(value)
	if kind == "" {
		kind, subject = "oidc", value
	}
	opID, err := s.store.LookupOperatorAlias(ctx, kind, subject)
	if err != nil {
		return "", err
	}
	if opID == 0 {
		return "", fmt.Errorf("unknown operator %q", value)
	}
	return strconv.FormatInt(opID, 10), nil
}

// resolveAgentPrincipal resolves an agent principal's value to its agent
// bead. The full bead (not just the id) is returned so the caller can read
// ClosedAt to compute the grant's mandatory expiry without a second fetch.
func (s *BeadsServer) resolveAgentPrincipal(ctx context.Context, value string) (*model.Bead, error) {
	if value == "" {
		return nil, fmt.Errorf("agent principal value is required")
	}
	bead, err := s.store.GetBead(ctx, value)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("unknown agent %q", value)
		}
		return nil, err
	}
	if bead == nil || bead.Type != "agent" {
		return nil, fmt.Errorf("%q is not an agent", value)
	}
	return bead, nil
}

// resolvePrincipal parses and resolves a combined "<kind>:<value>" principal
// string to the kind and the bare principal value taint_grants.principal
// stores. agentBead is populated only for an agent principal (needed by the
// caller for expiry computation).
func (s *BeadsServer) resolvePrincipal(ctx context.Context, raw string) (kind, principal string, agentBead *model.Bead, err error) {
	k, v, ok := parsePrincipal(raw)
	if !ok {
		return "", "", nil, fmt.Errorf(`principal must be "<kind>:<value>"`)
	}
	if !model.IsValidPrincipalKind(k) {
		return "", "", nil, fmt.Errorf("unknown principal kind %q", k)
	}
	switch k {
	case model.PrincipalTeam:
		id, err := s.resolveTeamPrincipal(ctx, v)
		if err != nil {
			return "", "", nil, err
		}
		return k, id, nil, nil
	case model.PrincipalOperator:
		id, err := s.resolveOperatorPrincipal(ctx, v)
		if err != nil {
			return "", "", nil, err
		}
		return k, id, nil, nil
	case model.PrincipalAgent:
		bead, err := s.resolveAgentPrincipal(ctx, v)
		if err != nil {
			return "", "", nil, err
		}
		return k, bead.ID, bead, nil
	default: // model.PrincipalFleet
		if v == "" {
			return "", "", nil, fmt.Errorf("fleet principal value is required")
		}
		return k, v, nil, nil
	}
}

// defaultGrantExpiry returns the ADR §10.4 default expires_at for a grant
// with no client-supplied --until: nil (unbounded) for team/fleet, now+90d
// for operator, and the agent bead's bound (agentGrantExpiry) for agent.
func defaultGrantExpiry(kind string, agentBead *model.Bead, now time.Time) *time.Time {
	switch kind {
	case model.PrincipalOperator:
		t := now.Add(90 * 24 * time.Hour)
		return &t
	case model.PrincipalAgent:
		return agentGrantExpiry(agentBead, now)
	default: // team, fleet
		return nil
	}
}

// agentGrantExpiry always returns a non-nil bound for an agent-principal
// grant (ADR §5.2: "always bounded, expiring at the agent bead's close"),
// regardless of any client --until. A still-open agent bead has no known
// close time yet — this explicit management endpoint (unlike the automatic
// propagation hooks of a later task) cannot invent a future one, so it
// defaults to the most conservative bound: now.
func agentGrantExpiry(agentBead *model.Bead, now time.Time) *time.Time {
	if agentBead != nil && agentBead.ClosedAt != nil {
		t := *agentBead.ClosedAt
		return &t
	}
	t := now
	return &t
}

// granterOperatorID resolves the authenticated caller to an operators(id)
// for taint_grants.granted_by. Derives from AuthIdentityFromContext for an
// OIDC/edge human, or from the human a bridge relays via a sanitized
// X-Operated-By header: the relay counts only from a bridge fleet subject
// (relayTrusted, P7 Task 3); the static token's relay is attribution only
// (ADR §11 invariant #1, "never client input"). A static or non-bridge fleet
// token, or a bridge with no relayed human, resolves to nothing: it must not be able
// to self-attribute a grant (ADR invariant D1), so it gets no operator id
// here rather than falling back to a machine sentinel.
func (s *BeadsServer) granterOperatorID(ctx context.Context) (int64, bool) {
	id := AuthIdentityFromContext(ctx)
	if kind, subject := operatorAliasForIdentity(id); kind != "" {
		return s.resolveOperator(ctx, kind+":"+subject)
	}
	if op := s.trustedRelayOperator(ctx); op != "" {
		return s.resolveOperator(ctx, op)
	}
	return 0, false
}

// callerMayManageTaint reports whether the caller may grant/revoke taint
// (ADR §7: "taint owner or admin"), and — per this task's description — also
// carries the owner-team implicit grant to apply. An admin (OIDC role)
// always qualifies; otherwise the caller must belong to the taint's owner
// team via TeamsOf, which itself honors a bridge's relay (relayTrusted, P7
// Task 3); the static token's relay is attribution only. The bare
// primary static token held by every agent and coopmux carries no team
// membership of its own and gets no shortcut here (ADR invariant D1): it
// must not be able to grant itself a taint or revoke the owner team's grant.
func (s *BeadsServer) callerMayManageTaint(ctx context.Context, taint *model.Taint) (bool, error) {
	if hasMinimumRole(RolesFromContext(ctx), RoleAdmin) {
		return true, nil
	}
	teams, err := s.TeamsOf(ctx)
	if err != nil {
		return false, err
	}
	for _, t := range teams {
		if t == taint.OwnerTeam {
			return true, nil
		}
	}
	return false, nil
}

// callerMayManageTaintPolicy reports whether the caller may edit a taint's
// scrub_policy (ADR §7: owner team or admin) — the same authorization rule
// as callerMayManageTaint, but WITHOUT honoring the X-Operated-By relay.
// Policy writes always require a genuine direct OIDC (or cryptographically
// verified edge-v2) identity: operator review 2026-09-23 on PR #149 flagged
// that grant/revoke may keep the relay, but "on main only a direct OIDC
// admin sets scrub policy; do not let the static-token relay weaken that."
// A static/fleet-token caller relaying a real human via X-Operated-By would
// otherwise pass callerMayManageTaint's TeamsOf check (team_membership.go);
// this gate refuses that caller outright, mirroring callerMayCreateTaint's
// authMethodOIDC check (http_taints.go) for the same reason.
func (s *BeadsServer) callerMayManageTaintPolicy(ctx context.Context, taint *model.Taint) (bool, error) {
	if authMethodFromContext(ctx) != authMethodOIDC {
		return false, nil
	}
	return s.callerMayManageTaint(ctx, taint)
}

// hasUnexpiredGrant reports whether a currently-valid taint_grants row
// exists for (taintID, kind, principal).
func (s *BeadsServer) hasUnexpiredGrant(ctx context.Context, taintID int64, kind, principal string) (bool, error) {
	_, err := s.store.GetTaintGrant(ctx, taintID, kind, principal)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// callerHoldsTaintGrant reports whether the caller may apply taint (ADR §7:
// "grant holder"), checked against the taint_grants table for the caller's
// applicable principal(s) — agent, fleet, operator, or any team from
// TeamsOf — or the owner-team-or-admin implicit grant this task's
// description carries over from p1-taint-crud-api. Deliberately does NOT
// implement ADR §11 invariant #3's spawn-chain-root inheritance for agents —
// out of scope for this task.
func (s *BeadsServer) callerHoldsTaintGrant(ctx context.Context, taint *model.Taint) (bool, error) {
	may, err := s.callerMayManageTaint(ctx, taint)
	if err != nil {
		return false, err
	}
	if may {
		return true, nil
	}

	id := AuthIdentityFromContext(ctx)
	if ref, ok := actingAgentRef(ctx); ok {
		return s.hasUnexpiredGrant(ctx, taint.ID, model.PrincipalAgent, ref)
	}
	switch {
	case id.Type == "fleet" && id.Subject != "":
		if ok, err := s.hasUnexpiredGrant(ctx, taint.ID, model.PrincipalFleet, id.Subject); err != nil || ok {
			return ok, err
		}
	}

	if kind, subject := operatorAliasForIdentity(id); kind != "" {
		opID, err := s.store.LookupOperatorAlias(ctx, kind, subject)
		if err == nil && opID != 0 {
			ok, err := s.hasUnexpiredGrant(ctx, taint.ID, model.PrincipalOperator, strconv.FormatInt(opID, 10))
			if err != nil {
				return false, err
			}
			if ok {
				return true, nil
			}
		}
	}

	teams, err := s.TeamsOf(ctx)
	if err != nil {
		return false, err
	}
	for _, t := range teams {
		ok, err := s.hasUnexpiredGrant(ctx, taint.ID, model.PrincipalTeam, t)
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}

// grantTaintRequest is the JSON body for POST /v1/taints/{name}/grants.
type grantTaintRequest struct {
	Principal string     `json:"principal"`
	Until     *time.Time `json:"until,omitempty"`
}

// handleGrantTaint handles POST /v1/taints/{name}/grants. Restricted to the
// taint's owner team or an admin (callerMayManageTaint). granted_by is
// always server-derived (granterOperatorID); expires_at follows the ADR
// §10.4 default table unless the caller passes --until — except for an
// agent principal, whose --until is always ignored (ADR §5.2).
func (s *BeadsServer) handleGrantTaint(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	bodyBytes, ok := readBoundedBody(w, r)
	if !ok {
		return
	}
	var req grantTaintRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Principal == "" {
		writeError(w, http.StatusBadRequest, "principal is required")
		return
	}

	ctx := r.Context()
	taint, err := s.store.GetTaint(ctx, name)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "taint not found")
		} else {
			writeServerError(ctx, w, "handleGrantTaint", err, "failed to look up taint")
		}
		return
	}

	may, err := s.callerMayManageTaint(ctx, taint)
	if err != nil {
		writeServerError(ctx, w, "handleGrantTaint", err, "failed to authorize grant")
		return
	}
	if !may {
		writeError(w, http.StatusForbidden, "must be the taint's owner team or an admin to grant it")
		return
	}

	kind, principal, agentBead, verr := s.resolvePrincipal(ctx, req.Principal)
	if verr != nil {
		writeError(w, http.StatusBadRequest, verr.Error())
		return
	}

	granterID, ok := s.granterOperatorID(ctx)
	if !ok {
		writeError(w, http.StatusForbidden, "caller identity cannot be resolved to an operator")
		return
	}

	now := time.Now().UTC()
	var expiresAt *time.Time
	if req.Until != nil && kind != model.PrincipalAgent {
		t := req.Until.UTC()
		expiresAt = &t
	} else {
		expiresAt = defaultGrantExpiry(kind, agentBead, now)
	}

	g := &model.TaintGrant{
		TaintID:       taint.ID,
		PrincipalKind: kind,
		Principal:     principal,
		GrantedBy:     granterID,
		ExpiresAt:     expiresAt,
	}
	if err := s.store.GrantTaint(ctx, g); err != nil {
		var ve *model.ValidationError
		if errors.As(err, &ve) {
			writeError(w, http.StatusBadRequest, ve.Error())
			return
		}
		writeServerError(ctx, w, "handleGrantTaint", err, "failed to grant taint")
		return
	}

	if err := s.store.RecordTaintAudit(ctx, &model.TaintAudit{
		TaintID: taint.ID,
		Action:  model.TaintAuditGrant,
		Actor:   actorFromContext(ctx),
	}); err != nil {
		writeServerError(ctx, w, "handleGrantTaint", err, "failed to record taint audit")
		return
	}

	payload := events.TaintGranted{
		TaintID:       taint.ID,
		TaintName:     taint.Name,
		PrincipalKind: kind,
		Principal:     principal,
		GrantedBy:     granterID,
		Fleet:         FleetFromContext(ctx),
	}
	if expiresAt != nil {
		payload.ExpiresAt = expiresAt.Format(time.RFC3339)
	}
	s.recordAndPublish(ctx, events.TopicTaintGranted, "", actorFromContext(ctx), payload)

	writeJSON(w, http.StatusCreated, g)
}

// handleRevokeTaint handles DELETE /v1/taints/{name}/grants/{principal}.
// Restricted to the taint's owner team or an admin. Idempotent: revoking a
// grant that does not exist is not an error.
func (s *BeadsServer) handleRevokeTaint(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	principalRaw := r.PathValue("principal")
	if name == "" || principalRaw == "" {
		writeError(w, http.StatusBadRequest, "name and principal are required")
		return
	}
	ctx := r.Context()

	taint, err := s.store.GetTaint(ctx, name)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "taint not found")
		} else {
			writeServerError(ctx, w, "handleRevokeTaint", err, "failed to look up taint")
		}
		return
	}

	may, err := s.callerMayManageTaint(ctx, taint)
	if err != nil {
		writeServerError(ctx, w, "handleRevokeTaint", err, "failed to authorize revoke")
		return
	}
	if !may {
		writeError(w, http.StatusForbidden, "must be the taint's owner team or an admin to revoke it")
		return
	}

	kind, principal, _, verr := s.resolvePrincipal(ctx, principalRaw)
	if verr != nil {
		writeError(w, http.StatusBadRequest, verr.Error())
		return
	}

	removed, err := s.store.RevokeTaint(ctx, taint.ID, kind, principal)
	if err != nil {
		writeServerError(ctx, w, "handleRevokeTaint", err, "failed to revoke taint")
		return
	}

	if removed {
		if err := s.store.RecordTaintAudit(ctx, &model.TaintAudit{
			TaintID: taint.ID,
			Action:  model.TaintAuditRevoke,
			Actor:   actorFromContext(ctx),
		}); err != nil {
			writeServerError(ctx, w, "handleRevokeTaint", err, "failed to record taint audit")
			return
		}
		s.recordAndPublish(ctx, events.TopicTaintRevoked, "", actorFromContext(ctx), events.TaintRevoked{
			TaintID:       taint.ID,
			TaintName:     taint.Name,
			PrincipalKind: kind,
			Principal:     principal,
			Fleet:         FleetFromContext(ctx),
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{"removed": removed})
}

// applyTaintRequest is the JSON body for POST /v1/beads/{id}/taints.
type applyTaintRequest struct {
	Name string `json:"name"`
}

// handleApplyTaint handles POST /v1/beads/{id}/taints. Authorization is
// grant-holder-based (callerHoldsTaintGrant) and deliberately independent of
// bead ownership, so this does NOT route through loadBeadForWrite — it loads
// the bead directly and only enforces token-project scope. applied_by is
// always server-derived. Idempotent: applying an already-applied taint
// preserves the original source/applied_by/applied_at, and the response
// reflects whatever is actually persisted — a taint_audit row (ADR §3) is
// written only when ApplyTaint reports a genuinely new row, not on a no-op
// re-apply.
func (s *BeadsServer) handleApplyTaint(w http.ResponseWriter, r *http.Request) {
	beadID := r.PathValue("id")
	if beadID == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	bodyBytes, ok := readBoundedBody(w, r)
	if !ok {
		return
	}
	var req applyTaintRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}

	ctx := r.Context()
	taint, err := s.store.GetTaint(ctx, req.Name)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "taint not found")
		} else {
			writeServerError(ctx, w, "handleApplyTaint", err, "failed to look up taint")
		}
		return
	}

	bead, err := s.store.GetBead(ctx, beadID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "bead not found")
		} else {
			writeServerError(ctx, w, "handleApplyTaint", err, "failed to fetch bead")
		}
		return
	}
	if bead == nil {
		writeError(w, http.StatusNotFound, "bead not found")
		return
	}
	if !beadMatchesTokenProject(ctx, bead.Labels) {
		if !s.refuseHidden(w, r, bead) {
			writeError(w, http.StatusForbidden, "token not authorized for this bead's project")
		}
		return
	}

	holds, err := s.callerHoldsTaintGrant(ctx, taint)
	if err != nil {
		writeServerError(ctx, w, "handleApplyTaint", err, "failed to authorize apply")
		return
	}
	if !holds {
		writeError(w, http.StatusForbidden, "caller does not hold a grant for this taint")
		return
	}

	bt := &model.BeadTaint{
		BeadID:    beadID,
		TaintID:   taint.ID,
		Source:    model.TaintSourceExplicit,
		AppliedBy: actorFromContext(ctx),
	}
	inserted, err := s.store.ApplyTaint(ctx, bt)
	if err != nil {
		var ve *model.ValidationError
		if errors.As(err, &ve) {
			writeError(w, http.StatusBadRequest, ve.Error())
			return
		}
		writeServerError(ctx, w, "handleApplyTaint", err, "failed to apply taint")
		return
	}

	if inserted {
		if err := s.store.RecordTaintAudit(ctx, &model.TaintAudit{
			TaintID: taint.ID,
			Action:  model.TaintAuditApply,
			BeadID:  beadID,
			Actor:   bt.AppliedBy,
		}); err != nil {
			writeServerError(ctx, w, "handleApplyTaint", err, "failed to record taint audit")
			return
		}
	}

	s.recordAndPublish(ctx, events.TopicTaintApplied, beadID, actorFromContext(ctx), events.TaintApplied{
		BeadID:    beadID,
		TaintID:   taint.ID,
		TaintName: taint.Name,
		AppliedBy: bt.AppliedBy,
		Fleet:     FleetFromContext(ctx),
	})

	// Re-fetch so the response reflects whatever the ON CONFLICT DO NOTHING
	// write actually left persisted (the original grant on an idempotent
	// re-apply, not this call's own values).
	applied, err := s.store.GetBeadTaints(ctx, beadID)
	if err != nil {
		writeServerError(ctx, w, "handleApplyTaint", err, "failed to fetch applied taint")
		return
	}
	for _, t := range applied {
		if t.TaintID == taint.ID {
			writeJSON(w, http.StatusCreated, t)
			return
		}
	}
	writeJSON(w, http.StatusCreated, bt)
}

// handleGetBeadTaints handles GET /v1/beads/{id}/taints. Available to any
// principal that can already read the bead — authorizeSubresourceRead is the
// same gate every other per-bead subresource GET routes through, NOT
// callerHoldsTaintGrant (grant-holder is the apply gate, not the read gate).
func (s *BeadsServer) handleGetBeadTaints(w http.ResponseWriter, r *http.Request) {
	beadID := r.PathValue("id")
	if beadID == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	if !s.authorizeSubresourceRead(w, r, beadID, map[string]any{"taints": []*model.BeadTaint{}}) {
		return
	}

	taints, err := s.store.GetBeadTaints(r.Context(), beadID)
	if err != nil {
		writeServerError(r.Context(), w, "handleGetBeadTaints", err, "failed to get bead taints")
		return
	}
	if taints == nil {
		taints = []*model.BeadTaint{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"taints": taints})
}
