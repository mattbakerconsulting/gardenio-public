package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// handleGetEscalation handles GET /v1/escalations/{id}.
func (s *BeadsServer) handleGetEscalation(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}

	bead, err := s.store.GetBead(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) || bead == nil {
		writeError(w, http.StatusNotFound, "escalation not found")
		return
	}
	if err != nil {
		writeServerError(r.Context(), w, "handleGetEscalation", err, "failed to get escalation")
		return
	}
	// Enforce cross-project read scope (ka-0000008132) — the by-id twin of the
	// handleGetBead gate. Escalation beads carry the denied privileged-operation
	// payload (operation_spec, credential_requirements, execution_script), so an
	// out-of-scope read here is the highest-sensitivity leak in this family.
	if !s.authorizeBeadRead(w, r, bead, "escalation not found") {
		return
	}
	if err := s.propagateReadTaint(r.Context(), bead.ID); err != nil {
		writeServerError(r.Context(), w, "handleGetEscalation", err, "failed to propagate read taint")
		return
	}

	esc := extractEscalationFields(bead)
	if strings.HasPrefix(escalationHeldBy(bead), "coordinator:") {
		if p, err := s.activeEscalationPolicy(r.Context()); err == nil {
			s.escalationPresenceJSON(r.Context(), bead, esc, p)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"escalation": esc,
		"issue":      bead,
	})
}

// handleListEscalations handles GET /v1/escalations.
func (s *BeadsServer) handleListEscalations(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := model.BeadFilter{
		Type: []model.BeadType{model.TypeEscalation},
		Sort: "-created_at",
	}
	if v := q.Get("status"); v != "" {
		for _, st := range strings.Split(v, ",") {
			filter.Status = append(filter.Status, model.Status(st))
		}
	} else {
		filter.Status = []model.Status{model.StatusOpen, model.StatusInProgress}
	}
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			filter.Limit = n
		}
	}
	// Route through clampListBeadsLimit so this hand-built filter shares the
	// maxListBeadsLimit OOM choke point (ka-0000006889) like every other
	// ListBeads path — an unclamped limit here would let a RoleViewer
	// materialize the whole table in daemon heap.
	filter.Limit = clampListBeadsLimit(filter.Limit, 50)

	// label (all of) and exclude_label (any of) only ever narrow, so they
	// compose with the read-scope labels scopeListFilter appends below
	// (gd-CndLtkZ5h0: the merge desk lists by label instead of a kd show per
	// escalation).
	filter.Labels = append(filter.Labels, splitComma(q.Get("label"))...)
	filter.ExcludeLabels = append(filter.ExcludeLabels, splitComma(q.Get("exclude_label"))...)

	// held_by is filtered in Go after the scope filter: it needs empty →
	// operator and "@me" lane resolution, which BeadFilter.Fields cannot
	// express. So is an operation_type list of more than one: BeadFilter.Fields
	// matches one value per key. The open working set is small, so fetch to
	// the cap and cut to the requested limit afterwards.
	var heldBy []string
	var opTypes []string
	wantLimit := filter.Limit
	if v := strings.TrimSpace(q.Get("held_by")); v != "" {
		heldBy = strings.Split(v, ",")
		filter.Limit = clampListBeadsLimit(maxListBeadsLimit, 50)
	}
	switch ops := splitComma(q.Get("operation_type")); len(ops) {
	case 0:
	case 1:
		filter.Fields = map[string]string{"operation_type": ops[0]}
	default:
		opTypes = ops
		filter.Limit = clampListBeadsLimit(maxListBeadsLimit, 50)
	}

	// Enforce cross-project read scope (ka-0000008132): inject the fleet-token
	// project: label (AND) and the OIDC allowed-projects labels (OR), exactly
	// as handleListBeads does. This is the bulk-exposure twin of the by-id gate
	// above — operation_spec / credential_requirements across every escalation.
	filter = s.scopeListFilter(r.Context(), filter)

	beads, _, err := s.store.ListBeads(r.Context(), filter)
	if err != nil {
		writeServerError(r.Context(), w, "handleListEscalations", err, "failed to list escalations")
		return
	}
	if len(heldBy) > 0 {
		if beads, err = s.filterEscalationsHeldBy(r.Context(), beads, heldBy); err != nil {
			var ie inputError
			if errors.As(err, &ie) {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
			writeServerError(r.Context(), w, "handleListEscalations", err, "failed to resolve held_by")
			return
		}
	}
	if len(opTypes) > 0 {
		beads = filterEscalationsByOperationType(beads, opTypes)
	}
	if len(beads) > wantLimit {
		beads = beads[:wantLimit]
	}

	if err := s.propagateReadTaintBeads(r.Context(), beadIDsOf(beads)); err != nil {
		writeServerError(r.Context(), w, "handleListEscalations", err, "failed to propagate read taint")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"escalations": s.escalationListRows(r.Context(), beads)})
}

// escalationListRows is the list response's rows: each escalation's fields
// and bead, with lane presence on the coordinator-held ones.
func (s *BeadsServer) escalationListRows(ctx context.Context, beads []*model.Bead) []map[string]any {
	var policy *escalationPolicy // read once, for the first coordinator-held row
	escalations := make([]map[string]any, 0, len(beads))
	for _, b := range beads {
		esc := extractEscalationFields(b)
		if strings.HasPrefix(escalationHeldBy(b), "coordinator:") {
			if policy == nil {
				p, err := s.activeEscalationPolicy(ctx)
				if err != nil {
					p = escalationPolicy{} // no coordinators: no presence rows
				}
				policy = &p
			}
			s.escalationPresenceJSON(ctx, b, esc, *policy)
		}
		escalations = append(escalations, map[string]any{
			"escalation": esc,
			"issue":      b,
		})
	}
	return escalations
}

// filterEscalationsByOperationType keeps the beads whose operation_type field
// is one of ops.
func filterEscalationsByOperationType(beads []*model.Bead, ops []string) []*model.Bead {
	kept := beads[:0]
	for _, b := range beads {
		var f struct {
			OperationType string `json:"operation_type"`
		}
		if len(b.Fields) > 0 && json.Unmarshal(b.Fields, &f) == nil && slices.Contains(ops, f.OperationType) {
			kept = append(kept, b)
		}
	}
	return kept
}

// approveEscalationRequest is the JSON body for POST /v1/escalations/{id}/approve.
type approveEscalationRequest struct {
	ApprovedBy     string `json:"approved_by"`
	ApprovalMethod string `json:"approval_method"`

	// GrantUntil is the approver-chosen expires_at for a `grant`-type
	// escalation's taint_grants row (ADR docs/adr/taints.md §9.4/§10.4: "the
	// expiry the approver chooses, default 7 days"). Ignored by every other
	// operation_type. Nil defaults to now+7days in replayEscalation.
	GrantUntil *time.Time `json:"grant_until,omitempty"`

	// ExecutionTarget is the operator's choice of executor at approval (gb
	// escalation approve --target): "local" or "service_account". Written with
	// the approval, by the daemon, and only while no dispatch is claimed (P7
	// Task 12).
	ExecutionTarget string `json:"execution_target,omitempty"`
	// Comment is the approver's optional note ("approve with comment",
	// gd-yuOG6cX6VI): recorded as a comment on the escalation by the approver
	// and as approval_comment with the approval. At most
	// maxApprovalCommentLen runes.
	Comment string `json:"comment,omitempty"`
}

// handleApproveEscalation handles POST /v1/escalations/{id}/approve.
// Records the approval on the escalation bead, then automatically replays the
// denied operation if operation_spec is present. Closes the escalation with the
// result (success or failure).
func (s *BeadsServer) handleApproveEscalation(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}

	var req approveEscalationRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}

	switch req.ExecutionTarget {
	case "", dispatchLocal, dispatchServiceAccount:
	default:
		writeError(w, http.StatusBadRequest, "execution_target must be local or service_account")
		return
	}
	req.Comment = strings.TrimSpace(req.Comment)
	if utf8.RuneCountInString(req.Comment) > maxApprovalCommentLen {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("comment is too long (max %d characters)", maxApprovalCommentLen))
		return
	}

	// Enforce cross-project write-authz BEFORE any mutation (ka-0000002666).
	// requireRole(RoleOperator) short-circuits for non-OIDC callers, so a
	// project-bound fleet token would otherwise reach the merge/update/replay
	// below with no project-scope check — the same 403 GET /v1/escalations/{id}
	// already returns. Fetching here also corrects the prior merge-before-fetch
	// ordering (approval fields must not land on an out-of-scope bead).
	targetBead, ok := s.authorizeEscalationVerb(w, r, id, "handleApproveEscalation", "approve")
	if !ok {
		return
	}

	// taints ADR §6.3 correction 1+2: approved_by is SERVER-DERIVED from the
	// authenticated identity — never trusted from the request body — and any
	// caller with no OIDC-derived human identity behind it is refused
	// outright (AUDIT-2026-04-18 §C3; gd-CxfzKo5VOJ). A differing body value
	// is logged, never honored.
	approvedBy, ok := s.escalationApprover(r.Context())
	if !ok {
		if cid := AuthIdentityFromContext(r.Context()); cid.Type == "oidc" {
			slog.Warn("handleApproveEscalation: refused oidc token from non-allowlisted client",
				"bead", id, "azp", cid.ClientID)
			writeError(w, http.StatusForbidden, presenceRefusal(cid.ClientID, "approval"))
			return
		}
		// D4: a non-human approves only in a coordinator or steward lane listed
		// in approvers, never its own request, never operator-only, never a
		// grant or privileged spawn. The static token is in no lane.
		check := s.checkMayApprove(r.Context(), targetBead)
		if check.Allowed && check.Method == approvalMethodCoordinator {
			principal := AuthIdentityFromContext(r.Context()).Subject
			if req.ApprovedBy != "" && req.ApprovedBy != principal {
				slog.Warn("handleApproveEscalation: ignoring body approved_by, using the coordinator principal",
					"bead", id, "body_approved_by", req.ApprovedBy, "principal", principal)
			}
			extra := map[string]any{
				"approved_by":     principal,
				"approved_at":     time.Now().UTC().Format(time.RFC3339),
				"approval_method": approvalMethodCoordinator,
				"approved_as":     check.As,
			}
			if req.Comment != "" {
				extra["approval_comment"] = req.Comment
			}
			s.finishEscalationApproval(w, r, targetBead, extra, principal, check.As, req.ExecutionTarget, req.GrantUntil)
			return
		}
		msg := "escalation approval requires an OIDC-authenticated identity"
		if _, agent := agentBeadIDFromContext(r.Context()); (agent || authMethodFromContext(r.Context()) == authMethodMachine) && check.Reason != "" {
			msg = check.Reason
		}
		writeError(w, http.StatusForbidden, msg)
		return
	}
	if req.ApprovedBy != "" && req.ApprovedBy != approvedBy {
		slog.Warn("handleApproveEscalation: ignoring body approved_by, using derived identity",
			"bead", id, "body_approved_by", req.ApprovedBy, "derived_approved_by", approvedBy)
	}

	// Correction 3: four-eyes — the escalation's policy may demand the
	// approver differ from the requester after alias resolution.
	if escalationRequiresFourEyes(targetBead) && s.escalationFourEyesViolation(r.Context(), targetBead, approvedBy) {
		writeError(w, http.StatusForbidden, "four-eyes required: approver must differ from the requester")
		return
	}

	// taints ADR §8: when the escalation names an approvers list (team-based
	// gate, config.go type:escalation "approvers"), the approver must belong
	// to one of those teams. escalationApprover above already guarantees
	// approvedBy is a human OIDC identity, so this is inherently the "human
	// mode" check the spawn_policy design doc (gd-sGW5Y1B8sg) describes —
	// the auto-approval path (system:spawn-policy) never reaches this
	// handler, since it carries no OIDC identity for escalationApprover to
	// accept. An escalation with no approvers set is unrestricted, matching
	// today's behavior for every escalation that predates this field.
	//
	// This is an access-control gate, not attribution, so it resolves teams
	// via TeamsOfDirect — NOT TeamsOf — deliberately refusing the
	// X-Operated-By relay escalationApprover itself honors above: a caller
	// holding only the shared static/fleet token controls that header and
	// could otherwise name any team member's email to both pass as
	// approvedBy AND satisfy this membership check, fully spoofing the gate
	// (send-back on PR #147 round 3; mirrors scrubApprover's identical
	// no-relay stance for scrub-request approval, http_scrub_requests.go).
	approvers, err := escalationApprovers(targetBead)
	if err != nil {
		writeError(w, http.StatusForbidden, "escalation approvers field is malformed; refusing to approve")
		return
	}
	if len(approvers) > 0 {
		// Lane refs (operator, land-owner:<land>) match by lane membership,
		// legacy entries by escalationApproverListed (escalation_lane_members.go).
		_, ok, err := s.humanSatisfiesApprovers(r.Context(), targetBead, approvedBy, approvers)
		if err != nil {
			writeServerError(r.Context(), w, "handleApproveEscalation", err, "failed to resolve approver's teams")
			return
		}
		if !ok {
			writeError(w, http.StatusForbidden, "approval requires being named in, or a member of a team named in, this escalation's approvers")
			return
		}
	}

	// D3's floor is re-derived here as checkHumanMayApprove does, so a
	// missing or stale operator_only stamp never opens a floor escalation to
	// a human outside the operator lane.
	if s.escalationOperatorOnly(r.Context(), targetBead) {
		if in, _ := s.callerInLane(r.Context(), laneRef{Kind: laneOperator, Raw: string(laneOperator)}, targetBead, escalationPolicy{}); !in {
			writeError(w, http.StatusForbidden, "this escalation is operator-only (secrets, credentials or approval rights)")
			return
		}
	}

	// A privileged-agent-spawn escalation is also gated on the live
	// spawn_policy approvers / admin-only default, so a missing or patched
	// approvers field cannot open it to any operator (gd-5o6PxGfSa7).
	denial, err := s.spawnApprovalDenial(r.Context(), targetBead, approvedBy)
	if err != nil {
		slog.Warn("handleApproveEscalation: failed to resolve spawn approval gate; refusing", "bead", id, "error", err)
		writeError(w, http.StatusForbidden, "could not resolve who may approve this spawn; refusing to approve")
		return
	}
	if denial != "" {
		writeError(w, http.StatusForbidden, denial)
		return
	}

	// The replay runs as a machine write, skipping write policy, so vet the
	// write here as the approver (gd-pUyCv4FSv5).
	denial, err = s.replayWritePolicyDenial(r.Context(), targetBead)
	if err != nil {
		slog.Warn("handleApproveEscalation: failed to resolve replay write policy; refusing", "bead", id, "error", err)
		writeError(w, http.StatusForbidden, "could not resolve the write policy for this escalation's operation; refusing to approve")
		return
	}
	if denial != "" {
		writeError(w, http.StatusForbidden, denial)
		return
	}

	// Nor does the machine-write replay pass the Lands kill-switch gates, so
	// only an approver who could throw the switch directly may approve one
	// (land_switch_authz.go).
	denial, err = s.replayKillSwitchDenial(r.Context(), targetBead)
	if err != nil {
		slog.Warn("handleApproveEscalation: failed to vet the replay against the kill switches; refusing", "bead", id, "error", err)
		writeError(w, http.StatusForbidden, "could not check this escalation's operation against the Lands kill switches; refusing to approve")
		return
	}
	if denial != "" {
		writeError(w, http.StatusForbidden, denial)
		return
	}

	// A scoped approver's replay is not a machine write, so vet the write
	// against the scoped rules before anything is mutated.
	denial, err = s.scopedReplayDenial(r.Context(), targetBead)
	if err != nil {
		slog.Warn("handleApproveEscalation: failed to resolve scoped replay policy; refusing", "bead", id, "error", err)
		writeError(w, http.StatusForbidden, "could not resolve the write policy for this escalation's operation; refusing to approve")
		return
	}
	if denial != "" {
		writeError(w, http.StatusForbidden, denial)
		return
	}

	extra := map[string]any{
		"approved_by": approvedBy,
		"approved_at": time.Now().UTC().Format(time.RFC3339),
	}
	// approval_method is derived from the identity, never the body (P7 Task
	// 15, spec D12); clients still send it for older daemons.
	extra["approval_method"] = approvalMethodFor(r.Context())
	if req.ApprovalMethod != "" && req.ApprovalMethod != extra["approval_method"] {
		slog.Info("handleApproveEscalation: ignoring body approval_method",
			"bead", id, "body", req.ApprovalMethod, "derived", extra["approval_method"])
	}
	s.addSpawnPermissionSnapshot(r.Context(), targetBead, extra)
	if req.Comment != "" {
		extra["approval_comment"] = req.Comment
	}

	s.finishEscalationApproval(w, r, targetBead, extra, approvedBy, "", req.ExecutionTarget, req.GrantUntil)
}

// finishEscalationApproval is the shared tail of a human or coordinator
// approval: claim the one executor (P7 Task 12, spec D10), stamp the approval
// (plus held_by=execute_by and its audience per D5 and D11, clearing any
// claim), move open → in_progress, publish, audit, replay when the daemon is
// the executor, and respond. execTarget is the approver's optional
// execution_target override.
func (s *BeadsServer) finishEscalationApproval(w http.ResponseWriter, r *http.Request, esc *model.Bead, extra map[string]any, approvedBy, as, execTarget string, grantUntil *time.Time) {
	id := esc.ID

	// Exactly one executor, claimed before anything is written or run: a
	// second approve that would dispatch again is refused with no mutation.
	// The claim lands before the approval event below, so the event carries
	// execution_dispatch for the local watcher.
	if !s.applyApproveTimeTarget(w, r, esc, extra, execTarget) {
		return
	}
	target, want, refused, err := s.approvalDispatch(r.Context(), esc, execTarget, extra)
	if err != nil {
		writeServerError(r.Context(), w, "handleApproveEscalation", err, "failed to check the executor pod")
		return
	}

	// A local escalation naming a runner runs on that machine: re-check the
	// runner now and that the approver owns it, before anything is claimed.
	// startEscalationExecutor records the runner's agent as executor.
	toRunner := want == dispatchLocal && escalationNamesRunner(beadFieldMap(target.Fields))
	if toRunner && !s.checkRunnerApprover(w, r, target, approvedBy) {
		return
	}
	dispatch, proceed, err := s.claimEscalationDispatchAs(r.Context(), target, want)
	if err != nil {
		writeServerError(r.Context(), w, "handleApproveEscalation", err, "failed to claim escalation dispatch")
		return
	}
	if !proceed {
		writeError(w, http.StatusConflict, "escalation already dispatched to "+dispatch)
		return
	}
	if err := s.startEscalationExecutor(r.Context(), esc, dispatch, approvedBy); err != nil {
		slog.Error("escalation executor did not start", "bead", id, "dispatch", dispatch, "error", err)
		writeError(w, http.StatusBadGateway, "approved, but the executor did not start: "+err.Error())
		return
	}
	s.stampEscalationHolder(r.Context(), esc, extra)
	// The beads the first approval names, frozen from here on: the only beads
	// an escalation-executor may write for it (escalation_executor_grant.go).
	// A later approve of an approved escalation never re-stamps them.
	if !escalationHasServerApproval(esc) {
		if targets := s.escalationApprovedTargets(r.Context(), esc); len(targets) > 0 {
			extra[approvedTargetsField] = targets
		}
	}

	if err := s.mergeEscalationFieldsWithHistory(r.Context(), esc, approvedBy, extra); err != nil {
		slog.Warn("failed to merge escalation approval fields", "bead", id, "error", err)
	}
	s.addApprovalComment(r.Context(), id, approvedBy, extra)

	// Re-fetch to pick up the merged approval fields before the status
	// transition + replay (mergeBeadFields wrote a fresh fields blob).
	bead, err := s.store.GetBead(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) || bead == nil {
		writeError(w, http.StatusNotFound, "escalation not found")
		return
	}
	if err != nil {
		writeServerError(r.Context(), w, "handleApproveEscalation", err, "failed to get escalation")
		return
	}

	// Publish the approval itself, not only the status flip: the bridge wakes
	// the executor on this event's Changes and ignores every later update of
	// the approved bead (gd-TMYvBxNY6h). An escalation already in_progress when
	// approved emits no status change, so without approved_by here its
	// approval would be invisible on the stream.
	changes := map[string]any{"approved_by": approvedBy, "execution_dispatch": dispatch}
	if exec, ok := extra["execute_by"]; ok {
		changes["execute_by"], changes["held_by"] = exec, extra["held_by"]
	}
	if bead.Status == model.StatusOpen {
		bead.Status = model.StatusInProgress
		if err := s.store.UpdateBead(r.Context(), bead); err != nil {
			writeServerError(r.Context(), w, "handleApproveEscalation", err, "failed to update escalation status")
			return
		}
		changes["status"] = string(model.StatusInProgress)
	}
	s.recordAndPublish(r.Context(), events.TopicBeadUpdated, bead.ID, approvedBy, events.BeadUpdated{
		Bead:    bead,
		Changes: changes,
	})

	if as != "" {
		s.auditEscalationCoordinatorApproval(r.Context(), id, approvedBy, as)
	} else {
		s.auditEscalationApproval(r.Context(), id, approvedBy)
	}

	// Only the daemon's own dispatch replays here; a local or service_account
	// executor acts on the event above, and "none" waits for a posted result.
	var replayResult map[string]any
	if dispatch == dispatchServer {
		replayResult = s.replayEscalation(r.Context(), bead, approvedBy, grantUntil)
	}

	executorNotified := toRunner && s.notifyRunnerExecutor(r.Context(), bead)

	// Re-fetch the bead to include any fields updated by replay.
	bead, _ = s.store.GetBead(r.Context(), id)

	resp := map[string]any{
		"escalation": extractEscalationFields(bead),
		"issue":      bead,
	}
	if replayResult != nil {
		resp["replay"] = replayResult
	}
	if toRunner {
		resp["executor_notified"] = executorNotified
	}
	if refused != "" {
		resp["executor_refused"] = refused
	}

	writeJSON(w, http.StatusOK, resp)
}

// maxApprovalCommentLen bounds an approval's comment, as the viewer bounds a
// rejection reason.
const maxApprovalCommentLen = 4000

// addApprovalComment writes the approver's note (extra["approval_comment"]) as
// a comment on the escalation, by the approver, so it reads in the bead's
// thread next to the approval it came with (gd-yuOG6cX6VI). The comment's
// operator is the caller's when the identity resolves, else the bead's own
// (comments.operated_by_id is NOT NULL). A failure is logged: the approval
// already stands.
func (s *BeadsServer) addApprovalComment(ctx context.Context, id, approvedBy string, extra map[string]any) {
	text, _ := extra["approval_comment"].(string)
	if text == "" {
		return
	}
	c := &model.Comment{BeadID: id, Author: approvedBy, Text: redactCredShapes(text), CreatedAt: time.Now().UTC()}
	if operatedBy, operatedByID, _, ok := s.resolveAuditOperatorRequired(ctx); ok {
		c.OperatedBy, c.OperatedByID = operatedBy, &operatedByID
	} else {
		s.inheritCommentOperator(ctx, c)
	}
	if err := s.store.AddComment(ctx, c); err != nil {
		slog.Warn("escalation approved but its comment failed", "bead", id, "error", err)
	}
}

// applyApproveTimeTarget records the approver's execution_target override in
// extra. An approve-time target must meet the same §5.3 checks as one filed
// with it (P7 Task 13), before the claim makes it final. It writes the error
// response and returns false when the target is refused.
func (s *BeadsServer) applyApproveTimeTarget(w http.ResponseWriter, r *http.Request, esc *model.Bead, extra map[string]any, execTarget string) bool {
	if execTarget == "" || escalationStringFields(esc)["execution_dispatch"] != "" {
		return true
	}
	if err := s.checkEscalationTargets(r.Context(), withEscalationTarget(esc, execTarget)); err != nil {
		var ie inputError
		if errors.As(err, &ie) {
			writeError(w, http.StatusBadRequest, ie.Error())
		} else {
			writeServerError(r.Context(), w, "handleApproveEscalation", err, "failed to check execution_target")
		}
		return false
	}
	extra["execution_target"] = execTarget
	return true
}

// stampEscalationHolder sets held_by=execute_by in extra (D5), with its
// audience (D11: every write that sets held_by sets audience), and clears any
// claim. An execute_by already in extra (refuseExecutorPod) wins over the
// bead's. An unreadable policy maps an unknown coordinator to owner (fail
// toward the owner).
func (s *BeadsServer) stampEscalationHolder(ctx context.Context, esc *model.Bead, extra map[string]any) {
	exec, ok := extra["execute_by"].(string)
	if !ok {
		exec, _, _ = exactFieldString(esc.Fields, "execute_by")
	}
	exec = strings.TrimSpace(exec)
	if exec == "" {
		return
	}
	extra["held_by"] = exec
	p, err := s.activeEscalationPolicy(ctx)
	if err != nil {
		slog.Warn("escalation approve: policy unreadable; audience from the built-in default", "escalation", esc.ID, "error", err)
		p = defaultEscalationPolicy()
	}
	extra[escalationAudienceField] = laneAudience(exec, p)
	extra["held_at"] = time.Now().UTC().Format(time.RFC3339)
	extra["claimed_by"] = ""
	extra["claimed_at"] = ""
	s.noteOfflineHolder(ctx, esc, extra, p)
}

// handleCanApproveEscalation handles POST /v1/escalations/{id}/can-approve: a
// dry run of handleApproveEscalation's checks for the caller (D9). It never
// writes. The scoped-approver gate is inside checkMayApprove, so a scoped
// principal gets allowed=false with the reason, not a 403.
func (s *BeadsServer) handleCanApproveEscalation(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	bead, ok := s.fetchResolutionTarget(w, r, id, "escalation", "handleCanApproveEscalation")
	if !ok {
		return
	}
	c := s.checkMayApprove(r.Context(), bead)
	writeJSON(w, http.StatusOK, map[string]any{"allowed": c.Allowed, "method": c.Method, "as": c.As, "reason": c.Reason})
}

// roleFromFields extracts the "role" string from a bead's JSON fields blob
// (createBeadInput.Fields for an agent create) by its exact key, or "" if
// absent/unparseable. See exactFieldString.
func roleFromFields(fields json.RawMessage) string {
	role, _, _ := exactFieldString(fields, agentRoleField)
	return role
}

// escalationRejector is who a reject is attributed to: the approval-capable
// human identity when there is one (the identity approve records as
// approved_by), else whatever authenticated the call. The body's rejected_by is
// used only when the call carries no identity at all (auth disabled); a
// differing body value is logged and ignored, never honored.
func (s *BeadsServer) escalationRejector(ctx context.Context, id, bodyValue string) string {
	derived, ok := s.escalationApprover(ctx)
	if !ok {
		derived = actorFromContext(ctx)
	}
	if derived == "" {
		return bodyValue
	}
	if bodyValue != "" && bodyValue != derived {
		slog.Warn("handleRejectEscalation: ignoring body rejected_by, using derived identity",
			"bead", id, "body_rejected_by", bodyValue, "derived_rejected_by", derived)
	}
	return derived
}

// escalationOnThePage ends every presence refusal (P10.B gd-4mEAhIE01U): the
// one door for a person's approve or reject is the escalation's page.
const escalationOnThePage = "approve or reject it on its page in the browser or on a phone (kd and gl print the link)"

// presenceRefusal is the 403 message for a person's oidc token that is not the
// page session; noun is "approval" or "rejection".
func presenceRefusal(clientID, noun string) string {
	if clientID == DeviceGrantClientID {
		return "escalation " + noun + " is not available to a `kd login` session; " + escalationOnThePage
	}
	return "escalation " + noun + " requires the page session; " + escalationOnThePage
}

// rejectEscalationRequest is the JSON body for POST /v1/escalations/{id}/reject.
type rejectEscalationRequest struct {
	RejectedBy string `json:"rejected_by"`
	Reason     string `json:"reason"`
}

// handleRejectEscalation handles POST /v1/escalations/{id}/reject.
// Closes the escalation as rejected with an audit trail.
func (s *BeadsServer) handleRejectEscalation(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}

	var req rejectEscalationRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}

	// Enforce cross-project write-authz before mutating (ka-0000002666).
	target, ok := s.authorizeEscalationVerb(w, r, id, "handleRejectEscalation", "reject")
	if !ok {
		return
	}

	// A person rejects, as they approve, only on the page session, for every
	// escalation (gd-yRGb2j1s56). Machine, agent and coordinator rejects pass.
	if cid := AuthIdentityFromContext(r.Context()); cid.Type == "oidc" {
		if _, page := s.escalationApprover(r.Context()); !page {
			slog.Warn("handleRejectEscalation: refused oidc token from non-allowlisted client",
				"bead", id, "azp", cid.ClientID)
			writeError(w, http.StatusForbidden, presenceRefusal(cid.ClientID, "rejection"))
			return
		}
	}

	// P10.B: a floor escalation is rejected, like approved, only by the
	// operator lane on the page session; any other floor reject is a 403.
	if s.escalationOperatorOnly(r.Context(), target) {
		if in, _ := s.callerInLane(r.Context(), laneRef{Kind: laneOperator, Raw: string(laneOperator)}, target, escalationPolicy{}); !in {
			writeError(w, http.StatusForbidden, "this escalation is on the owner floor (secrets, roles, policy, money or deletion); "+escalationOnThePage)
			return
		}
	}

	// rejected_by is SERVER-DERIVED, as approved_by is on approve: the body
	// value is only a fallback when nothing authenticated the call. It lands
	// on the bead too (P7 Task 10), beside closed_by and the audit actor.
	rejectedBy := s.escalationRejector(r.Context(), id, req.RejectedBy)
	// Four-eyes covers reject as well as approve (spec D11, P7 Task 14).
	if escalationRequiresFourEyes(target) && s.escalationFourEyesViolation(r.Context(), target, rejectedBy) {
		writeError(w, http.StatusForbidden, "four-eyes required: the rejecter must differ from the requester")
		return
	}

	extra := map[string]any{
		"result_status": "failure",
	}
	if rejectedBy != "" {
		extra["rejected_by"] = rejectedBy
	}
	// rejected_client names the door, as result_client does (gd-EmqfpYQ3NZ).
	if c := callerClient(r.Context()); c != "" {
		extra["rejected_client"] = c
	}
	if req.Reason != "" {
		extra["result"] = map[string]string{"error": "rejected", "reason": req.Reason}
	}

	if err := s.mergeEscalationFieldsWithHistory(r.Context(), target, rejectedBy, extra); err != nil {
		slog.Warn("failed to merge escalation reject fields", "bead", id, "error", err)
	}

	s.auditEscalationRejection(r.Context(), id, rejectedBy, req.Reason)

	closedBy := rejectedBy
	bead, err := s.store.CloseBead(r.Context(), id, closedBy)
	if errors.Is(err, sql.ErrNoRows) || bead == nil {
		writeError(w, http.StatusNotFound, "escalation not found")
		return
	}
	if err != nil {
		writeServerError(r.Context(), w, "handleRejectEscalation", err, "failed to close escalation")
		return
	}

	s.recordAndPublish(r.Context(), events.TopicBeadClosed, bead.ID, closedBy, events.BeadClosed{
		Bead:     bead,
		ClosedBy: closedBy,
	})
	s.recordCloseClient(r.Context(), bead.ID, closedBy)

	writeJSON(w, http.StatusOK, map[string]any{
		"escalation": extractEscalationFields(bead),
		"issue":      bead,
	})
}

// captureResultRequest is the JSON body for POST /v1/escalations/{id}/result.
type captureResultRequest struct {
	// Executor and ExecutionTarget are ignored (P7 Task 10): the daemon stamps
	// executor and closed_by from the authenticated caller, and
	// execution_target is frozen at filing. Executor is honoured only with
	// auth disabled.
	Executor        string `json:"executor"`
	ExecutionTarget string `json:"execution_target"`
	Result          any    `json:"result"`
	ResultStatus    string `json:"result_status"`
	// Fields are extra top-level bead fields merged atomically with the result
	// record + close. Only a bridge may send them, and only the escalation
	// reaper's render and witness keys (resultRenderField: reaped,
	// reaped_by/at/reason/satisfier, the MR witness mr_*, approvals_*), in the
	// SAME operation as the close, so only a beads.bead.closed event fires
	// (never an updated event that would re-trigger execution of an
	// already-satisfied ask). Any other key is a 400.
	Fields map[string]string `json:"fields"`
	// KeepRequestingTask withdraws the escalation's close-on-execute opt-in in
	// the same write as the close (gd-MgicCcY2rj). A merge that does not finish
	// the requesting task (its fix rides another PR) used to be closed success
	// and then had the flag removed by a second write, which lost the race to
	// the auto-close by a second (gd-Ma2u0JfCFE).
	KeepRequestingTask bool `json:"keep_requesting_task"`
}

// handleCaptureEscalationResult handles POST /v1/escalations/{id}/result.
// Records the execution result and closes the escalation.
func (s *BeadsServer) handleCaptureEscalationResult(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}

	var req captureResultRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.ResultStatus == "" {
		writeError(w, http.StatusBadRequest, "result_status is required")
		return
	}

	// Validate result_status.
	switch req.ResultStatus {
	case "success", "failure", "partial":
		// valid
	default:
		writeError(w, http.StatusBadRequest, "invalid result_status: must be success, failure, or partial")
		return
	}

	// Enforce cross-project write-authz before mutating (ka-0000002666).
	resultTarget, ok := s.authorizeResolutionTarget(w, r, id, "escalation", "handleCaptureEscalationResult")
	if !ok {
		return
	}

	// P7 Task 10 (spec §5.2): who resolved is the authenticated caller, never
	// the body. An agent resolves only what resultResolver admits.
	resolver, refusal := s.resultResolver(r.Context(), resultTarget, req.ResultStatus)
	if refusal != "" {
		s.auditDenial(r, &AuthzDenial{Action: "close", BeadType: string(resultTarget.Type), Message: refusal}, id)
		writeError(w, http.StatusForbidden, refusal)
		return
	}
	class := s.callerClassFromContext(r.Context())
	resolver = captureResultResolver(r.Context(), class, id, resolver, &req)

	// D6 result gate (escalation_result_gate.go), behind
	// BEADS_ESCALATION_RESULT_REQUIRES_APPROVAL. Checked before any field is
	// merged, so a refused result leaves the bead untouched.
	if denial := s.escalationResultDenial(r.Context(), resultTarget, req.ResultStatus); denial != "" {
		s.auditDenial(r, &AuthzDenial{Action: "result", BeadType: string(resultTarget.Type), Message: denial}, id)
		writeError(w, http.StatusForbidden, denial)
		return
	}

	// req.Fields carries only the escalation reaper's render and witness trail
	// (reaped*, mr_*, approvals_*), and only from a bridge. Anything else, the
	// decision fields approved_by/executor/result_status included, is a 400:
	// the passthrough let a caller forge them (gd-pUyCv4FSv5, P7 Task 10).
	if msg := captureResultFieldsError(r.Context(), class, resultTarget, req.Fields); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}

	extra := captureResultExtra(r.Context(), resultTarget, &req, resolver)
	if err := s.mergeEscalationFieldsWithHistory(r.Context(), resultTarget, resolver, extra); err != nil {
		slog.Warn("failed to merge escalation result fields", "bead", id, "error", err)
		// Closing without the withdrawn opt-in would close the very task the
		// caller asked to keep open.
		if req.KeepRequestingTask {
			writeServerError(r.Context(), w, "handleCaptureEscalationResult", err, "failed to record keep_requesting_task; escalation left open")
			return
		}
	}

	closedBy := resolver
	bead, err := s.store.CloseBead(r.Context(), id, closedBy)
	if errors.Is(err, sql.ErrNoRows) || bead == nil {
		writeError(w, http.StatusNotFound, "escalation not found")
		return
	}
	if err != nil {
		writeServerError(r.Context(), w, "handleCaptureEscalationResult", err, "failed to close escalation")
		return
	}

	s.recordAndPublish(r.Context(), events.TopicBeadClosed, bead.ID, closedBy, events.BeadClosed{
		Bead:     bead,
		ClosedBy: closedBy,
	})

	writeJSON(w, http.StatusOK, s.afterEscalationResult(r.Context(), bead, &req))
}

// captureResultResolver applies the body's executor to the derived resolver:
// only with auth disabled (dev), where nothing authenticated the call. Any
// other body executor, and every body execution_target, is logged and ignored.
func captureResultResolver(ctx context.Context, class callerClass, id, resolver string, req *captureResultRequest) string {
	if class == classNone && actorFromContext(ctx) == "" && req.Executor != "" {
		resolver = req.Executor
	} else if req.Executor != "" && req.Executor != resolver {
		slog.Warn("handleCaptureEscalationResult: ignoring body executor, using derived identity",
			"bead", id, "body_executor", req.Executor, "resolver", resolver)
	}
	if req.ExecutionTarget != "" {
		slog.Warn("handleCaptureEscalationResult: ignoring body execution_target (frozen at filing)",
			"bead", id, "body_execution_target", req.ExecutionTarget)
	}
	return resolver
}

// captureResultFieldsError is the 400 message for a result's extra fields, or
// "" when they are acceptable: only a bridge may send them, and only
// resultRenderField keys the endpoint and freeze checks allow.
func captureResultFieldsError(ctx context.Context, class callerClass, target *model.Bead, fields map[string]string) string {
	if len(fields) > 0 && class != classBridge && class != classNone {
		return "result fields are accepted only from a bridge"
	}
	for k := range fields {
		if !resultRenderField(k) {
			return "result field " + k + " is not allowed (reaped, reaped_*, mr_*, approvals_* only)"
		}
		if err := checkEscalationFieldEndpointWrite(ctx, target, k); err != nil {
			return err.Error()
		}
		if err := checkEscalationFrozenFieldEndpoint(ctx, target, k); err != nil {
			return err.Error()
		}
	}
	return ""
}

// captureResultExtra is the field set a result merges into the escalation in
// the same write as its history entry.
func captureResultExtra(ctx context.Context, target *model.Bead, req *captureResultRequest, resolver string) map[string]any {
	extra := map[string]any{}
	for k, v := range req.Fields {
		if v != "" {
			extra[k] = v
		}
	}
	extra["result_status"] = req.ResultStatus
	// executor is the dispatched executor when one was recorded, else the
	// resolver; execution_target stays as filed.
	if escalationStringFields(target)["executor"] == "" && resolver != "" {
		extra["executor"] = resolver
	}
	if req.Result != nil {
		extra["result"] = req.Result
	}
	// result_client names the door the result came from (callerClient): an
	// agent on a person's CLI login may record one, callerHasLaneAuthority.
	if c := callerClient(ctx); c != "" {
		extra["result_client"] = c
	}
	if req.KeepRequestingTask {
		extra[closeRequestingTaskField] = false
	}
	return extra
}

// afterEscalationResult runs the success-only follow-ups of a closed
// escalation and returns the response body.
//
// Auto-close cascade (ka-0000001587): only a SUCCESSFUL execution releases
// the close-on-escalation-execute wrappers. A failure/partial result means
// the operator-privileged action did not land, so the wrapper's blocker is
// not actually cleared — leave it open. (Rejection routes through
// handleRejectEscalation, which never cascades, for the same reason.)
func (s *BeadsServer) afterEscalationResult(ctx context.Context, bead *model.Bead, req *captureResultRequest) map[string]any {
	resp := map[string]any{
		"escalation": extractEscalationFields(bead),
		"issue":      bead,
	}
	if req.ResultStatus == "success" {
		s.afterBeadClosed(ctx, bead)
		// A helm_deploy closed with the rolled tag records where that
		// release now runs (releases-design.md §4.3).
		if dep := s.recordEscalationDeployment(ctx, bead, req.Result, req.Executor); dep != nil {
			resp["deployment"] = dep
		}
	}
	return resp
}

// escalationResolveHint is the actionable message returned when a caller tries
// to close an escalation through the generic ownership-gated write path instead
// of its dedicated resolve surface (ka-0000005516).
const escalationResolveHint = "escalation %s must be resolved through its dedicated verbs, not the generic close path: " +
	"`gb escalation approve|reject|close <id>` (POST /v1/escalations/{id}/approve|reject|result). " +
	"A generic close would skip operation_spec replay and wrongly release close-on-escalation wrappers as if the operator action had run."

// escalationBlocksGenericClose reports whether the given bead is an open
// escalation whose close must be diverted away from the generic close path.
//
// Background (ka-0000005516): the shared-queue ownership carve-out
// (ka-0000002032) relaxed authorizeOwnership for type:escalation so any operator
// could close/reflag a teammate's escalation. That is correct for reflag/pingback
// (which are *updates*), but it also made a *generic close* succeed — and the
// generic close path does a plain close: it never replays the denied
// operation_spec (that lives only on the approve path, replayEscalation), never
// records result_status, and fires afterBeadClosed → maybeCloseEscalationWrappers
// UNCONDITIONALLY, releasing close-on-escalation-execute wrapper tasks as though
// the privileged operation had executed. Diverting the generic close to the
// dedicated approve/reject/result verbs keeps replay and wrapper-release correct.
//
// Only OPEN/in_progress escalations are diverted — an already-closed escalation
// stays on the idempotent-close fast path so racing cleanup writes are harmless.
// The dedicated approve/reject/result handlers close via s.store.CloseBead
// directly, never through this generic path, so they are unaffected.
func escalationBlocksGenericClose(bead *model.Bead) bool {
	return bead != nil &&
		bead.Type == model.TypeEscalation &&
		bead.Status != model.StatusClosed
}

// escalationDivertError is returned by updateBead when a status→closed PATCH
// would terminate an open escalation through the generic update path, bypassing
// the dedicated resolve verbs. The generic close paths (gRPC CloseBead, HTTP
// handleCloseBead) already consult escalationBlocksGenericClose and refuse; the
// PATCH update path (`kd update <id> --status closed`) did not — so it could
// terminate an escalation while skipping operation_spec replay + result_status
// recording AND (because updateBead only publishes TopicBeadUpdated, never
// TopicBeadClosed) emitting no closed event at all, blinding every closed-event
// watcher in the fleet. Refusing outright — rather than merely publishing
// TopicBeadClosed — is the correct parity fix: adding the event would not
// restore replay or result_status, whereas the refusal forces the caller
// through approve/reject/result, which do all three correctly (ka-0000006558).
//
// Transport layers map this to 409 (HTTP) / FailedPrecondition (gRPC), matching
// the two existing generic-close divert responses.
type escalationDivertError struct {
	id string
}

func (e *escalationDivertError) Error() string {
	return fmt.Sprintf(escalationResolveHint, e.id)
}

// extractEscalationFields extracts escalation-specific fields from a bead's Fields JSON.
func extractEscalationFields(b *model.Bead) map[string]any {
	esc := make(map[string]any)
	if len(b.Fields) == 0 {
		return esc
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b.Fields, &fields); err != nil {
		return esc
	}

	// String fields.
	stringKeys := []string{
		"operation_type", "context", "requesting_agent", "requesting_task",
		"requesting_plan", "implements_decision",
		"target_bead_id",
		"execution_script", "sensitivity", "audience",
		"approved_by", "approved_at", "approval_method", "approval_comment", "policy_rule",
		"approved_as", "held_by", "held_at", "execute_by",
		"claimed_by", "claimed_at", "handed_up_by", "handed_up_at", "hand_up_reason", approvedTargetsField,
		heldByOfflineAtField, "held_by_last_seen_at",
		"execution_target", "executor", "result_status",
		"redacted_summary", "snapshot_at",
	}
	for _, k := range stringKeys {
		if raw, ok := fields[k]; ok {
			var s string
			if json.Unmarshal(raw, &s) == nil {
				esc[k] = s
			} else {
				esc[k] = strings.TrimSpace(string(raw))
			}
		}
	}

	// JSON fields — pass through as-is.
	jsonKeys := []string{
		"operation_spec", "result",
		"credential_requirements", "data_requirements", "expected_outputs",
		"approvers", "permissions_at_approval",
	}
	for _, k := range jsonKeys {
		if raw, ok := fields[k]; ok {
			esc[k] = json.RawMessage(raw)
		}
	}

	// Boolean fields.
	if raw, ok := fields["requires_four_eyes"]; ok {
		var b bool
		if json.Unmarshal(raw, &b) == nil {
			esc["requires_four_eyes"] = b
		}
	}
	if raw, ok := fields["operator_only"]; ok {
		var b bool
		if json.Unmarshal(raw, &b) == nil {
			esc["operator_only"] = b
		}
	}

	return esc
}
