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

// This file implements the grant-request surface ADR docs/adr/taints.md
// §9.2/§9.4 adds on top of the taint write gate and the generic escalation
// replay path:
//
//   - POST /v1/taints/{name}/grant-requests   handleGrantTaintRequest
//
// A grant request is filed as an `operation_type: grant` escalation bead
// (createGrantEscalation), either directly here or via the X-Escalate retry
// on a taint write denial (handleTaintGrantEscalation, write_authz.go) — both
// call createGrantEscalation so the two surfaces produce the same escalation
// shape. Approving it does not replay the original write; replayEscalation's
// "grant" case (http_escalations.go) writes a taint_grants row instead.

// grantEscalationSpec is the operation_spec payload stamped on a
// `grant`-type escalation. replayEscalation's "grant" case reads this back
// on approval to know which taint(s) to grant and to whom — the requester's
// principal is resolved at FILING time (resolveRequesterPrincipal), since the
// requester's identity context is gone by the time an operator approves.
type grantEscalationSpec struct {
	TaintNames      []string `json:"taint_names"`
	TaintOwnerTeams []string `json:"taint_owner_teams,omitempty"`
	PrincipalKind   string   `json:"principal_kind"`
	Principal       string   `json:"principal"`
	Reason          string   `json:"reason,omitempty"`
}

// resolveRequesterPrincipal resolves the authenticated caller to the
// grantable principal (kind + taint_grants.principal value) a taint grant
// escalation should be filed for. Mirrors the identity branching
// callerHoldsTaintGrant (http_taint_grants.go) uses to check for an existing
// grant, but resolves the principal itself rather than testing membership.
// Team membership is deliberately NOT resolved here (unlike
// callerHoldsTaintGrant's implicit-grant check) — a grant *request* is filed
// for the requester as an individual (agent/operator/fleet), not for
// whichever team they happen to belong to.
func (s *BeadsServer) resolveRequesterPrincipal(ctx context.Context) (kind, principal string, ok bool) {
	id := AuthIdentityFromContext(ctx)
	if ref, ok := actingAgentRef(ctx); ok {
		return model.PrincipalAgent, ref, true
	}
	switch {
	case id.Type == "fleet" && id.Subject != "":
		return model.PrincipalFleet, id.Subject, true
	}
	if k, subject := operatorAliasForIdentity(id); k != "" {
		opID, err := s.store.LookupOperatorAlias(ctx, k, subject)
		if err == nil && opID != 0 {
			return model.PrincipalOperator, strconv.FormatInt(opID, 10), true
		}
	}
	return "", "", false
}

// decodeGrantEscalationSpec decodes a grant escalation's operation_spec,
// accepting the object itself or a JSON string holding it.
func decodeGrantEscalationSpec(raw string) (grantEscalationSpec, bool) {
	var spec grantEscalationSpec
	var s string
	if json.Unmarshal([]byte(raw), &s) == nil {
		raw = s
	}
	if json.Unmarshal([]byte(raw), &spec) != nil {
		return spec, false
	}
	return spec, true
}

// checkGrantEscalationPrincipal refuses a grant escalation whose
// operation_spec names any principal but the filer's own
// (resolveRequesterPrincipal). Approval writes the grant to that principal,
// so a filer free to name it could turn one approval into a grant for a
// whole fleet or team (gd-URd8GBIXvL). operation_spec is immutable after
// create, so this pins the principal the replay grants to.
func (s *BeadsServer) checkGrantEscalationPrincipal(ctx context.Context, bead *model.Bead) error {
	if bead == nil || bead.Type != model.TypeEscalation || normOp(escalationStringFields(bead)["operation_type"]) != "grant" {
		return nil
	}
	spec, ok := decodeGrantEscalationSpec(fieldRaw(bead, "operation_spec"))
	if !ok {
		return inputError("a grant escalation's operation_spec must be an object with taint_names, principal_kind and principal")
	}
	kind, principal, ok := s.resolveRequesterPrincipal(ctx)
	if !ok {
		return inputError("a grant escalation needs a caller that resolves to a grantable principal")
	}
	if spec.PrincipalKind != kind || spec.Principal != principal {
		return inputError(fmt.Sprintf("a grant escalation grants only to its filer: principal must be %s:%s, not %s:%s", kind, principal, spec.PrincipalKind, spec.Principal))
	}
	return nil
}

// checkGrantReplay refuses the replay of an approved grant escalation unless
// the approver may manage every named taint (callerMayManageTaint: owner team
// or admin, the rule the direct grant route applies) and the principal is the
// requester itself, as createGrantEscalation records it: an agent principal
// is the verified requesting_agent, a fleet principal is a requesting_agent
// filed without an agent credential, an operator principal is the operator
// that requesting_agent resolves to, and a team is never a requester.
// ctx is the approver's request context, before any replay override
// (gd-URd8GBIXvL).
func (s *BeadsServer) checkGrantReplay(ctx context.Context, escalation *model.Bead, spec grantEscalationSpec) error {
	f := escalationStringFields(escalation)
	requester, agentFiled := f["requesting_agent"], f[escalationRequestingAgentVerifiedField] == "true"
	var isRequester bool
	switch spec.PrincipalKind {
	case model.PrincipalAgent:
		isRequester = agentFiled && spec.Principal == requester
	case model.PrincipalFleet:
		isRequester = !agentFiled && spec.Principal == requester
	case model.PrincipalOperator:
		if k, sub := operatorAliasForIdentity(AuthIdentity{Type: "oidc", Subject: requester}); k != "" && !agentFiled {
			opID, err := s.store.LookupOperatorAlias(ctx, k, sub)
			isRequester = err == nil && opID != 0 && strconv.FormatInt(opID, 10) == spec.Principal
		}
	}
	if !isRequester {
		return fmt.Errorf("replay refused: grant principal %s:%s is not the requester %s", spec.PrincipalKind, spec.Principal, requester)
	}
	for _, name := range spec.TaintNames {
		taint, err := s.store.GetTaint(ctx, name)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return err
		}
		may, err := s.callerMayManageTaint(ctx, taint)
		if err != nil {
			return err
		}
		if !may {
			return fmt.Errorf("replay refused: approver is neither an admin nor on taint %s's owner team (%s)", taint.Name, taint.OwnerTeam)
		}
	}
	return nil
}

// createGrantEscalation creates a `grant`-type escalation bead (ADR
// docs/adr/taints.md §9.4/§9.2), shared by the X-Escalate retry on a taint
// write denial (handleTaintGrantEscalation, write_authz.go) and the direct
// kd taint grant-request endpoint (handleGrantTaintRequest below) — both must
// produce the same escalation shape so a developer filing one directly gets
// no different treatment than one who hit the denial first. denialContext is
// the denied write's AuthzDenial.Message, empty for a direct request;
// targetBeadID is the bead whose write was denied, empty for a direct
// request (a grant request is not itself a bead mutation).
func (s *BeadsServer) createGrantEscalation(ctx context.Context, identity AuthIdentity, taintNames, ownerTeams []string, kind, principal, reason, denialContext, targetBeadID string) (*model.Bead, error) {
	spec := grantEscalationSpec{
		TaintNames:      taintNames,
		TaintOwnerTeams: ownerTeams,
		PrincipalKind:   kind,
		Principal:       principal,
		Reason:          reason,
	}

	fields := map[string]any{
		"operation_type":    "grant",
		"requesting_agent":  identity.Subject,
		"operation_spec":    mustMarshal(spec),
		"taint_names":       taintNames,
		"taint_owner_teams": ownerTeams,
	}
	if denialContext != "" {
		fields["context"] = denialContext
	}
	if reason != "" {
		fields["reason"] = reason
	}
	if targetBeadID != "" {
		fields["target_bead_id"] = targetBeadID
	}

	return s.createBead(ctx, createBeadInput{
		Title: "Grant request: " + strings.Join(taintNames, ", "),
		// The daemon authors this escalation, so it supplies the short_title
		// the enforce-mode gate requires (gd-lYZUr0CqY0).
		ShortTitle: model.ClipShortTitle("grant " + strings.Join(taintNames, ", ")),
		Type:       "escalation",
		Labels: []string{
			"auto:authz-escalation",
			"auto:taint-grant-request",
		},
		CreatedBy: identity.Subject,
		Fields:    mustMarshal(fields),
	})
}

// grantTaintRequestRequest is the JSON body for
// POST /v1/taints/{name}/grant-requests. Reason is optional free text
// recorded on the escalation for the approving team's context.
type grantTaintRequestRequest struct {
	Reason string `json:"reason,omitempty"`
}

// handleGrantTaintRequest handles POST /v1/taints/{name}/grant-requests (ADR
// docs/adr/taints.md §9.2: "kd taint grant-request <name> [--reason] files a
// grant request to the owner team ... so a developer does not need to know
// who owns the taint"). Unlike handleGrantTaint, it does NOT require the
// caller to already be the owner team or an admin — the whole point is
// asking for access the caller does not have. RoleOperator-gated at the
// route (http.go), the same baseline every other write route uses; the
// escalation this creates still requires operator approval to take effect.
func (s *BeadsServer) handleGrantTaintRequest(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	bodyBytes, ok := readBoundedBody(w, r)
	if !ok {
		return
	}
	var req grantTaintRequestRequest
	if len(bodyBytes) > 0 {
		if err := json.Unmarshal(bodyBytes, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
	}

	ctx := r.Context()
	taint, err := s.store.GetTaint(ctx, name)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "taint not found")
		} else {
			writeServerError(ctx, w, "handleGrantTaintRequest", err, "failed to look up taint")
		}
		return
	}

	identity := AuthIdentityFromContext(ctx)
	kind, principal, ok := s.resolveRequesterPrincipal(ctx)
	if !ok {
		writeError(w, http.StatusForbidden, "caller identity cannot be resolved to a grantable principal")
		return
	}

	var ownerTeams []string
	if taint.OwnerTeam != "" {
		ownerTeams = []string{taint.OwnerTeam}
	}

	bead, err := s.createGrantEscalation(ctx, identity, []string{taint.Name}, ownerTeams, kind, principal, req.Reason, "", "")
	if err != nil {
		writeServerError(ctx, w, "handleGrantTaintRequest", err, "failed to create grant request")
		return
	}

	writeJSON(w, http.StatusAccepted, EscalationResponse{
		EscalationID: bead.ID,
		Message:      "grant request created: pending owner-team approval",
		DeniedAction: "grant",
		BeadType:     "taint",
	})
}

// grantTaintsForEscalation writes a taint_grants row for each named taint to
// the resolved requester principal, on approval of a `grant`-type escalation
// (ADR docs/adr/taints.md §9.4). Reuses the exact GrantTaint/RecordTaintAudit
// /TaintGranted sequence handleGrantTaint uses for a direct grant
// (http_taint_grants.go), so an escalation-approved grant is indistinguishable
// in the audit trail from an operator-issued one except for its granted_by.
// A taint name that no longer resolves (deleted between filing and approval)
// is skipped rather than failing the whole batch. Returns an error only when
// NOTHING could be granted.
func (s *BeadsServer) grantTaintsForEscalation(ctx context.Context, names []string, kind, principal string, granterID int64, expiresAt *time.Time) ([]string, error) {
	granted := make([]string, 0, len(names))
	for _, name := range names {
		taint, err := s.store.GetTaint(ctx, name)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return granted, err
		}

		g := &model.TaintGrant{
			TaintID:       taint.ID,
			PrincipalKind: kind,
			Principal:     principal,
			GrantedBy:     granterID,
			ExpiresAt:     expiresAt,
		}
		if err := s.store.GrantTaint(ctx, g); err != nil {
			return granted, err
		}
		if err := s.store.RecordTaintAudit(ctx, &model.TaintAudit{
			TaintID: taint.ID,
			Action:  model.TaintAuditGrant,
			Actor:   actorFromContext(ctx),
		}); err != nil {
			return granted, err
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

		granted = append(granted, name)
	}
	if len(granted) == 0 {
		return granted, fmt.Errorf("no valid taints to grant")
	}
	return granted, nil
}
