package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// loadBeadForWrite fetches the target bead and enforces token-project scope and
// write-policy authorization — including the ownership gate — before a relation
// mutation (label/dep/comment-delete/comment-trim/follower). On any failure
// (not found, forbidden, or policy denial) it writes an HTTP response and
// returns nil; the caller should simply return.
//
// bodyBytes is the raw request body, forwarded to handleEscalation so an
// X-Escalate:true caller can convert a denial into an escalation bead.
func (s *BeadsServer) loadBeadForWrite(w http.ResponseWriter, r *http.Request, beadID, action string, bodyBytes []byte) *model.Bead {
	return s.loadBeadForWriteCore(w, r, beadID, action, bodyBytes, true)
}

// loadBeadForComment is the comment-create variant of loadBeadForWrite. It
// applies every check loadBeadForWrite does EXCEPT the bead-mutation ownership
// gate (ka-0000005884): comments are append-only annotations, not mutations of
// the bead's own fields, so any authenticated identity authorized to write to
// the bead's type/project may comment regardless of created_by/assignee/owner.
// Token-project scope and type-level write-policy (role/project/token-scope)
// still apply, and the commenter identity is stamped on the comment by the
// caller for attribution. Ownership gating remains in force for comment
// delete/trim and for all real field mutations.
func (s *BeadsServer) loadBeadForComment(w http.ResponseWriter, r *http.Request, beadID string, bodyBytes []byte) *model.Bead {
	return s.loadBeadForWriteCore(w, r, beadID, "comment", bodyBytes, false)
}

// loadBeadForWriteCore is the shared implementation behind loadBeadForWrite and
// loadBeadForComment. When enforceOwnership is true it runs the full
// authorizeWriteForBead (write-policy + ownership gate); when false it runs only
// the type-level write-policy via authorizeWriteTitled, decoupling comment-create from
// the ownership gate.
func (s *BeadsServer) loadBeadForWriteCore(w http.ResponseWriter, r *http.Request, beadID, action string, bodyBytes []byte, enforceOwnership bool) *model.Bead {
	bead, err := s.store.GetBead(r.Context(), beadID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "bead not found")
		} else {
			writeServerError(r.Context(), w, "loadBeadForWrite", err, "failed to fetch bead")
		}
		return nil
	}
	if bead == nil {
		writeError(w, http.StatusNotFound, "bead not found")
		return nil
	}
	// Each refusal below answers a bead the caller cannot read as missing.
	if !beadMatchesTokenProject(r.Context(), bead.Labels) {
		if !s.refuseHidden(w, r, bead) {
			writeError(w, http.StatusForbidden, "token not authorized for this bead's project")
		}
		return nil
	}
	var denial *AuthzDenial
	if enforceOwnership {
		denial = s.authorizeWriteForBead(r.Context(), action, bead)
	} else {
		denial = s.authorizeWriteTitled(r.Context(), action, bead.Type, bead.Labels, bead.Title)
		if denial == nil {
			denial = runnerProjectDenial(r.Context(), action, nil, bead.Fields)
		}
	}
	if denial != nil {
		if s.beadHiddenFromCaller(r.Context(), bead) {
			s.auditDenial(r, denial, beadID)
			writeError(w, http.StatusNotFound, "bead not found")
			return nil
		}
		if s.handleEscalation(w, r, denial, bodyBytes, beadID) {
			return nil
		}
		s.auditDenial(r, denial, beadID)
		writeAuthzDenial(w, denial)
		return nil
	}
	return bead
}

// handleGetDependencies handles GET /v1/beads/{id}/dependencies.
//
// Optional query parameter `?type=<dep-type>` filters the response to a single
// relationship type (e.g. "blocks", "parent-child", "triggered-by"). The
// filter is applied in-memory after the store call so it works against every
// backend without a new store method.
func (s *BeadsServer) handleGetDependencies(w http.ResponseWriter, r *http.Request) {
	beadID := r.PathValue("id")
	if beadID == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	if !s.authorizeSubresourceRead(w, r, beadID, map[string]any{"dependencies": []*model.Dependency{}}) {
		return
	}

	deps, err := s.store.GetDependencies(r.Context(), beadID)
	if err != nil {
		writeServerError(r.Context(), w, "handleGetDependencies", err, "failed to get dependencies")
		return
	}

	deps = filterDepsByType(deps, r.URL.Query().Get("type"))

	if deps == nil {
		deps = []*model.Dependency{}
	}

	writeJSON(w, http.StatusOK, map[string]any{"dependencies": deps})
}

// handleGetReverseDependencies handles GET /v1/beads/{id}/dependents.
//
// Supports the same optional `?type=` filter as handleGetDependencies.
func (s *BeadsServer) handleGetReverseDependencies(w http.ResponseWriter, r *http.Request) {
	beadID := r.PathValue("id")
	if beadID == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	if !s.authorizeSubresourceRead(w, r, beadID, map[string]any{"dependencies": []*model.Dependency{}}) {
		return
	}

	deps, err := s.store.GetReverseDependencies(r.Context(), beadID)
	if err != nil {
		writeServerError(r.Context(), w, "handleGetReverseDependencies", err, "failed to get reverse dependencies")
		return
	}

	deps = filterDepsByType(deps, r.URL.Query().Get("type"))

	if deps == nil {
		deps = []*model.Dependency{}
	}

	writeJSON(w, http.StatusOK, map[string]any{"dependencies": deps})
}

// filterDepsByType returns deps whose Type matches the given type string. An
// empty wantType passes everything through unchanged. Used by the
// dependencies / dependents HTTP endpoints to support `?type=` filtering.
func filterDepsByType(deps []*model.Dependency, wantType string) []*model.Dependency {
	if wantType == "" {
		return deps
	}
	out := make([]*model.Dependency, 0, len(deps))
	for _, d := range deps {
		if string(d.Type) == wantType {
			out = append(out, d)
		}
	}
	return out
}

// addDependencyRequest is the JSON body for POST /v1/beads/{id}/dependencies.
type addDependencyRequest struct {
	DependsOnID string `json:"depends_on_id"`
	Type        string `json:"type"`
	CreatedBy   string `json:"created_by"`
}

// handleAddDependency handles POST /v1/beads/{id}/dependencies.
func (s *BeadsServer) handleAddDependency(w http.ResponseWriter, r *http.Request) {
	beadID := r.PathValue("id")
	if beadID == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}

	bodyBytes, ok := readBoundedBody(w, r)
	if !ok {
		return
	}
	var req addDependencyRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.DependsOnID == "" {
		writeError(w, http.StatusBadRequest, "depends_on_id is required")
		return
	}

	src := s.loadBeadForWrite(w, r, beadID, "update", bodyBytes)
	if src == nil {
		return
	}
	if s.runnerDependencySourceHidden(r.Context(), src) {
		writeError(w, http.StatusNotFound, "bead not found")
		return
	}
	hidden, denial := s.scopedDependencyTargetDenial(r.Context(), req.DependsOnID)
	if hidden {
		writeError(w, http.StatusNotFound, "depends_on bead not found")
		return
	}
	if denial != nil {
		s.auditDenial(r, denial, beadID)
		writeAuthzDenial(w, denial)
		return
	}

	// SECURITY (AUDIT §C3): ignore req.CreatedBy — derive from auth identity.
	// Post-cutover (017): deps.operated_by_id is NOT NULL. R2 (ka-0000005772)
	// made the resolver fail-closed: unattributable requests get 400 so
	// misconfigured callers see the error instead of silently writing under
	// the system:legacy-pre-attribution sentinel.
	now := time.Now().UTC()
	operatedBy, operatedByID, refusalSource, ok := s.resolveAuditOperatorRequired(r.Context())
	if !ok {
		writeOperatorRefusal(w, refusalSource)
		return
	}
	dep := &model.Dependency{
		BeadID:       beadID,
		DependsOnID:  req.DependsOnID,
		Type:         model.DependencyType(req.Type),
		CreatedAt:    now,
		CreatedBy:    actorFromContext(r.Context()),
		OperatedBy:   operatedBy,
		OperatedByID: &operatedByID,
	}

	// Data-hygiene guard (ka-0000006880): auto-canonicalize a reversed
	// parent-child edge (epic as bead_id / child side) before the insert,
	// so the subtree CTE and gardener preflights see the row.
	s.canonicalizeParentChildDep(r.Context(), dep)

	// Cycle guard (ka-0000006899): reject a parent-child edge that would
	// close a cycle. A cycle makes an open epic its own descendant, wedging
	// subtree-drain gardener dormancy preflights awake forever.
	if s.wouldCreateParentChildCycle(r.Context(), dep) {
		slog.Warn("rejecting parent-child dep that would create a cycle",
			"bead_id", dep.BeadID,
			"depends_on_id", dep.DependsOnID,
			"created_by", dep.CreatedBy,
			"bead", "ka-0000006899",
		)
		writeError(w, http.StatusConflict,
			fmt.Sprintf("parent-child dependency %s -> %s would create a cycle", dep.BeadID, dep.DependsOnID))
		return
	}

	// Closed-parent guard (ka-0000005756): reject a parent-child edge whose
	// PARENT is already closed. Such a child enters `gb ready` as a claimable
	// orphan the auto-close cascade can never drain (auto_close.go skips closed
	// parents). REJECT loudly — the author named the parent deliberately — and
	// point them at a live parent. X-Force-Parent-Edge (`kd dep add --force`)
	// overrides for legitimate re-parenting. Checked after canonicalization so
	// dep.DependsOnID is the true parent.
	if !forceParentEdgeFromContext(r.Context()) {
		if parent := s.closedParentSide(r.Context(), dep); parent != nil {
			slog.Warn("rejecting parent-child dep onto a CLOSED parent",
				"bead_id", dep.BeadID,
				"closed_parent", parent.ID,
				"parent_closed_at", closedAtString(parent),
				"created_by", dep.CreatedBy,
				"bead", "ka-0000005756",
			)
			writeError(w, http.StatusConflict,
				fmt.Sprintf("parent-child dependency: parent %s is closed; pick a live parent or pass --force to re-parent onto it", dep.DependsOnID))
			return
		}
	}

	// Cycle guard (ka-0000007984): reject a blocks edge that would close a cycle
	// in the blocks graph. A blocks cycle pins every bead in and downstream of
	// it as permanently undispatchable, and nothing else surfaces the condition.
	if s.wouldCreateBlocksCycle(r.Context(), dep) {
		rejectBlocksCycleLog(dep)
		writeError(w, http.StatusConflict,
			fmt.Sprintf("blocks dependency %s -> %s would create a cycle", dep.BeadID, dep.DependsOnID))
		return
	}

	if err := s.addDependencyWithTaintPropagation(r.Context(), dep); err != nil {
		writeServerError(r.Context(), w, "handleAddDependency", err, "failed to add dependency")
		return
	}

	// Taint propagation on write (ADR docs/adr/taints.md §4.2 row 4): the
	// dependent bead also gets the WRITER's own taints, independent of the
	// triggered-by/derived-from target-taint union addDependencyWithTaintPropagation
	// already applied above.
	if err := s.propagateWriteTaint(r.Context(), dep.BeadID, dep.CreatedBy); err != nil {
		writeServerError(r.Context(), w, "handleAddDependency", err, "failed to propagate write taint")
		return
	}

	s.recordAndPublish(r.Context(), events.TopicDependencyAdded, dep.BeadID, dep.CreatedBy, events.DependencyAdded{Dependency: dep, Fleet: FleetFromContext(r.Context())})
	// Proposal C (ka-0000007756): audit the edge add so `kd audit bead` can
	// answer "why did this edge change" post-hoc. Uses the canonicalized dep.
	s.recordDependencyMutation(r.Context(), dep.BeadID, dep.DependsOnID, string(dep.Type), dep.CreatedBy, model.MutationTypeCreate)
	s.inheritParentProjectLabel(r.Context(), dep)

	writeJSON(w, http.StatusCreated, dep)
}

// handleRemoveDependency handles DELETE /v1/beads/{id}/dependencies.
// depends_on_id and type are taken from query parameters.
func (s *BeadsServer) handleRemoveDependency(w http.ResponseWriter, r *http.Request) {
	beadID := r.PathValue("id")
	if beadID == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}

	q := r.URL.Query()
	dependsOnID := q.Get("depends_on_id")
	if dependsOnID == "" {
		writeError(w, http.StatusBadRequest, "depends_on_id query parameter is required")
		return
	}
	depType := q.Get("type")

	src := s.loadBeadForWrite(w, r, beadID, "update", nil)
	if src == nil {
		return
	}
	if s.runnerDependencySourceHidden(r.Context(), src) {
		writeError(w, http.StatusNotFound, "bead not found")
		return
	}
	// Both ends, as on add: a scoped principal may not unlink a bead outside
	// its projects. A target the caller cannot see unlinks as a missing one
	// does: nothing removed.
	hidden, denial := s.scopedDependencyTargetDenial(r.Context(), dependsOnID)
	if hidden {
		writeJSON(w, http.StatusOK, map[string]any{"removed": false})
		return
	}
	if denial != nil {
		s.auditDenial(r, denial, beadID)
		writeAuthzDenial(w, denial)
		return
	}

	// Shared with the gRPC transport so the "only a matched delete is a
	// mutation" invariant has exactly one implementation (see
	// removeDependencyEdge — the phantom event/audit row this fixes was
	// duplicated across both transports).
	removed, err := s.removeDependencyEdge(r.Context(), beadID, dependsOnID, depType)
	if err != nil {
		writeServerError(r.Context(), w, "handleRemoveDependency", err, "failed to remove dependency")
		return
	}

	// BOTH outcomes stay 2xx — a no-op remove is not an HTTP error, and every
	// existing client (kd's HTTPClient, controller beadsapi, `gb bundle`) treats
	// any non-2xx as a hard failure, which would break the idempotent callers.
	// The rows-affected signal therefore rides in the BODY, which is why this is
	// now 200-with-body rather than the previous bodiless 204: carrying it in
	// the status code is precisely what we can't do. Both clients' decodeResponse
	// special-cases 204 only as "no body to parse" and otherwise unmarshals just
	// when the caller passed a result, so a 200 + JSON body is a no-op for
	// callers that ignore it. `removed` is always explicitly present so a new
	// client can tell it from an old daemon that omits the key (see
	// RemoveDependencyResponse in beads.proto).
	writeJSON(w, http.StatusOK, map[string]any{"removed": removed})
}

// handleGetLabels handles GET /v1/beads/{id}/labels.
func (s *BeadsServer) handleGetLabels(w http.ResponseWriter, r *http.Request) {
	beadID := r.PathValue("id")
	if beadID == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	if !s.authorizeSubresourceRead(w, r, beadID, map[string]any{"labels": []string{}}) {
		return
	}

	labels, err := s.store.GetLabels(r.Context(), beadID)
	if err != nil {
		writeServerError(r.Context(), w, "handleGetLabels", err, "failed to get labels")
		return
	}

	if labels == nil {
		labels = []string{}
	}

	writeJSON(w, http.StatusOK, map[string]any{"labels": labels})
}

// addLabelRequest is the JSON body for POST /v1/beads/{id}/labels.
type addLabelRequest struct {
	Label string `json:"label"`
}

// handleAddLabel handles POST /v1/beads/{id}/labels.
func (s *BeadsServer) handleAddLabel(w http.ResponseWriter, r *http.Request) {
	beadID := r.PathValue("id")
	if beadID == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}

	bodyBytes, ok := readBoundedBody(w, r)
	if !ok {
		return
	}
	var req addLabelRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Label == "" {
		writeError(w, http.StatusBadRequest, "label is required")
		return
	}

	target := s.loadBeadForWrite(w, r, beadID, "update", bodyBytes)
	if target == nil {
		return
	}
	if denial := scopedLabelChangeDenial(r.Context(), s.store, target.Type, req.Label, true); denial != nil {
		s.auditDenial(r, denial, beadID)
		writeAuthzDenial(w, denial)
		return
	}
	if denial := s.tenantAgentLabelDenial(r.Context(), "update", target.Type, []string{req.Label}); denial != nil {
		s.auditDenial(r, denial, beadID)
		writeAuthzDenial(w, denial)
		return
	}
	if denial := commonsMessageLockDenial(r.Context(), "update", "change its labels", target); denial != nil {
		s.auditDenial(r, denial, beadID)
		writeAuthzDenial(w, denial)
		return
	}
	if err := conversationLabelChangeError(r.Context(), s.store, target, req.Label, true); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := plotApplicationLabelChangeError(target, req.Label, true); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.projectLabelChangeError(r.Context(), target, req.Label, true); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := creditGrantLabelChangeError(target, req.Label, true); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := landInviteLabelChangeError(target, req.Label, true); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.heraldryLabelChangeError(r.Context(), target, req.Label, true); err != nil {
		writeHeraldryCheckError(w, r, "handleAddLabel", err)
		return
	}
	if denial := s.singleLabelChangeDenial(r.Context(), target, req.Label); denial != nil {
		s.auditDenial(r, denial, beadID)
		writeAuthzDenial(w, denial)
		return
	}

	if err := s.store.AddLabel(r.Context(), beadID, req.Label); err != nil {
		writeServerError(r.Context(), w, "handleAddLabel", err, "failed to add label")
		return
	}

	// Taint propagation on write (ADR docs/adr/taints.md §4.2 row 4).
	if err := s.propagateWriteTaint(r.Context(), beadID, actorFromContext(r.Context())); err != nil {
		writeServerError(r.Context(), w, "handleAddLabel", err, "failed to propagate write taint")
		return
	}

	s.recordAndPublish(r.Context(), events.TopicLabelAdded, beadID, "", events.LabelAdded{
		BeadID: beadID,
		Label:  req.Label,
		Fleet:  FleetFromContext(r.Context()),
	})

	// Fetch the updated bead to return.
	bead, err := s.store.GetBead(r.Context(), beadID)
	if err != nil {
		writeServerError(r.Context(), w, "handleAddLabel", err, "failed to get bead after adding label")
		return
	}
	if bead == nil {
		writeError(w, http.StatusNotFound, "bead not found")
		return
	}

	writeJSON(w, http.StatusCreated, bead)
}

// handleRemoveLabel handles DELETE /v1/beads/{id}/labels/{label}.
func (s *BeadsServer) handleRemoveLabel(w http.ResponseWriter, r *http.Request) {
	beadID := r.PathValue("id")
	if beadID == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	label := r.PathValue("label")
	if label == "" {
		writeError(w, http.StatusBadRequest, "label is required")
		return
	}

	target := s.loadBeadForWrite(w, r, beadID, "update", nil)
	if target == nil {
		return
	}
	if denial := scopedLabelChangeDenial(r.Context(), s.store, target.Type, label, false); denial != nil {
		s.auditDenial(r, denial, beadID)
		writeAuthzDenial(w, denial)
		return
	}
	if denial := frozenAgentLabelDenial(r.Context(), s.store, target, labelsWithout(target.Labels, label)); denial != nil {
		s.auditDenial(r, denial, beadID)
		writeAuthzDenial(w, denial)
		return
	}
	if denial := s.tenantAgentProjectLabelDenial(r.Context(), target.Type, target.Labels, labelsWithout(target.Labels, label)); denial != nil {
		s.auditDenial(r, denial, beadID)
		writeAuthzDenial(w, denial)
		return
	}
	if denial := commonsMessageLockDenial(r.Context(), "update", "change its labels", target); denial != nil {
		s.auditDenial(r, denial, beadID)
		writeAuthzDenial(w, denial)
		return
	}
	if err := conversationLabelChangeError(r.Context(), s.store, target, label, false); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := plotApplicationLabelChangeError(target, label, false); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.projectLabelChangeError(r.Context(), target, label, false); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := creditGrantLabelChangeError(target, label, false); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := landInviteLabelChangeError(target, label, false); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.heraldryLabelChangeError(r.Context(), target, label, false); err != nil {
		writeHeraldryCheckError(w, r, "handleRemoveLabel", err)
		return
	}
	if denial := s.singleLabelChangeDenial(r.Context(), target, label); denial != nil {
		s.auditDenial(r, denial, beadID)
		writeAuthzDenial(w, denial)
		return
	}

	// Shared with the gRPC transport so the "only a matched delete is a
	// mutation" invariant has exactly one implementation (see removeLabelEdge —
	// the phantom label-removed event this fixes would otherwise be duplicated
	// across both transports).
	removed, err := s.removeLabelEdge(r.Context(), beadID, label)
	if err != nil {
		writeServerError(r.Context(), w, "handleRemoveLabel", err, "failed to remove label")
		return
	}

	// Taint propagation on write (ADR docs/adr/taints.md §4.2 row 4).
	if err := s.propagateWriteTaint(r.Context(), beadID, actorFromContext(r.Context())); err != nil {
		writeServerError(r.Context(), w, "handleRemoveLabel", err, "failed to propagate write taint")
		return
	}

	// BOTH outcomes stay 2xx — a no-op remove is not an HTTP error, and every
	// existing client (kd's HTTPClient, controller beadsapi) treats any non-2xx
	// as a hard failure, which would break the idempotent callers. The
	// rows-affected signal therefore rides in the BODY, which is why this is now
	// 200-with-body rather than the previous bodiless 204: carrying it in the
	// status code is precisely what we can't do. Both clients' decodeResponse
	// special-cases 204 only as "no body to parse" and otherwise unmarshals just
	// when the caller passed a result, so a 200 + JSON body is a no-op for
	// callers that ignore it. `removed` is always explicitly present so a new
	// client can tell it from an old daemon that omits the key (see
	// RemoveLabelResponse in beads.proto).
	writeJSON(w, http.StatusOK, map[string]any{"removed": removed})
}

// handleGetComments handles GET /v1/beads/{id}/comments.
func (s *BeadsServer) handleGetComments(w http.ResponseWriter, r *http.Request) {
	beadID := r.PathValue("id")
	if beadID == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	if !s.authorizeSubresourceRead(w, r, beadID, map[string]any{"comments": []*model.Comment{}}) {
		return
	}

	comments, err := s.store.GetComments(r.Context(), beadID)
	if err != nil {
		writeServerError(r.Context(), w, "handleGetComments", err, "failed to get comments")
		return
	}

	if comments == nil {
		comments = []*model.Comment{}
	}

	writeJSON(w, http.StatusOK, map[string]any{"comments": comments})
}

// addCommentRequest is the JSON body for POST /v1/beads/{id}/comments.
type addCommentRequest struct {
	Author string `json:"author"`
	Text   string `json:"text"`
	// ImpersonatedBy is never taken from a client: present at all, the
	// request is refused (impersonation.go).
	ImpersonatedBy *string `json:"impersonated_by"`
}

// handleAddComment handles POST /v1/beads/{id}/comments.
func (s *BeadsServer) handleAddComment(w http.ResponseWriter, r *http.Request) {
	beadID := r.PathValue("id")
	if beadID == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}

	bodyBytes, ok := readBoundedBody(w, r)
	if !ok {
		return
	}
	var req addCommentRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Text == "" {
		writeError(w, http.StatusBadRequest, "text is required")
		return
	}

	// Comment-create is intentionally NOT gated by bead ownership
	// (ka-0000005884): any authenticated identity authorized to write to this
	// bead's type/project may comment, regardless of created_by/assignee/owner.
	// The commenter identity is stamped below (Author/OperatedBy) for
	// attribution. Ownership gating remains in force for delete/trim and field
	// mutations.
	bead := s.loadBeadForComment(w, r, beadID, bodyBytes)
	if bead == nil {
		return
	}
	if req.ImpersonatedBy != nil {
		denial := impersonatedByDeny("comment", bead.Type)
		s.auditDenial(r, denial, beadID)
		writeAuthzDenial(w, denial)
		return
	}

	// SECURITY (AUDIT §C3): ignore req.Author — derive from auth identity.
	// Post-cutover (017): comments.operated_by_id is NOT NULL. R2
	// (ka-0000005772) made the resolver fail-closed: unattributable requests
	// get 400 so misconfigured callers see the error instead of silently
	// writing under the system:legacy-pre-attribution sentinel.
	now := time.Now().UTC()
	operatedBy, operatedByID, refusalSource, ok := s.resolveAuditOperatorRequired(r.Context())
	if !ok {
		writeOperatorRefusal(w, refusalSource)
		return
	}
	comment := &model.Comment{
		BeadID:       beadID,
		Author:       actorFromContext(r.Context()),
		Text:         redactCredShapes(req.Text), // F17 (R4-SEC-4): strip bare credential shapes
		CreatedAt:    now,
		OperatedBy:   operatedBy,
		OperatedByID: &operatedByID,
		// Stamped from the verified token, never from the request
		// (impersonation.go).
		ImpersonatedBy: impersonatedByFromContext(r.Context()),
	}

	if err := s.store.AddComment(r.Context(), comment); err != nil {
		writeServerError(r.Context(), w, "handleAddComment", err, "failed to add comment")
		return
	}

	// Taint propagation on write (ADR docs/adr/taints.md §4.2 row 4). Runs
	// post-commit, so a failure here 500s a comment that already persisted —
	// a client retry would duplicate it; accepted as a known hazard on this
	// path only.
	if err := s.propagateWriteTaint(r.Context(), comment.BeadID, comment.Author); err != nil {
		writeServerError(r.Context(), w, "handleAddComment", err, "failed to propagate write taint")
		return
	}

	s.recordAndPublish(r.Context(), events.TopicCommentAdded, comment.BeadID, comment.Author, events.CommentAdded{Comment: comment, Fleet: FleetFromContext(r.Context())})

	writeJSON(w, http.StatusCreated, comment)
}

// handleDeleteComment handles DELETE /v1/beads/{id}/comments/{comment_id}.
// Idempotent: the response body always reports `deleted` as a boolean — false
// signals "the row was already gone" so the scheduler can call this in a
// retry without distinguishing 404 from success.
func (s *BeadsServer) handleDeleteComment(w http.ResponseWriter, r *http.Request) {
	beadID := r.PathValue("id")
	if beadID == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	commentIDStr := r.PathValue("comment_id")
	commentID, err := strconv.ParseInt(commentIDStr, 10, 64)
	if err != nil || commentID <= 0 {
		writeError(w, http.StatusBadRequest, "comment_id must be a positive integer")
		return
	}

	if s.loadBeadForWrite(w, r, beadID, "update", nil) == nil {
		return
	}

	deleted, err := s.store.DeleteComment(r.Context(), beadID, commentID)
	if err != nil {
		writeServerError(r.Context(), w, "handleDeleteComment", err, "failed to delete comment")
		return
	}
	if deleted == nil {
		writeJSON(w, http.StatusOK, map[string]any{"deleted": false})
		return
	}

	s.recordAndPublish(r.Context(), events.TopicCommentDeleted, deleted.BeadID, actorFromContext(r.Context()), events.CommentDeleted{
		BeadID:    deleted.BeadID,
		CommentID: deleted.ID,
		Author:    deleted.Author,
		Reason:    "delete",
		Fleet:     FleetFromContext(r.Context()),
	})

	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

// trimCommentsRequest is the JSON body for POST /v1/beads/{id}/comments/trim.
type trimCommentsRequest struct {
	KeepLast int    `json:"keep_last"`
	Author   string `json:"author"`
}

// handleTrimComments handles POST /v1/beads/{id}/comments/trim.
func (s *BeadsServer) handleTrimComments(w http.ResponseWriter, r *http.Request) {
	beadID := r.PathValue("id")
	if beadID == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}

	bodyBytes, ok := readBoundedBody(w, r)
	if !ok {
		return
	}
	var req trimCommentsRequest
	if len(bodyBytes) > 0 {
		if err := json.Unmarshal(bodyBytes, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
	}
	if req.KeepLast < 0 {
		writeError(w, http.StatusBadRequest, "keep_last must be >= 0")
		return
	}

	if s.loadBeadForWrite(w, r, beadID, "update", bodyBytes) == nil {
		return
	}

	deleted, err := s.store.TrimComments(r.Context(), beadID, req.KeepLast, req.Author)
	if err != nil {
		writeServerError(r.Context(), w, "handleTrimComments", err, "failed to trim comments")
		return
	}

	deletedIDs := make([]int64, 0, len(deleted))
	actor := actorFromContext(r.Context())
	fleet := FleetFromContext(r.Context())
	for _, c := range deleted {
		deletedIDs = append(deletedIDs, c.ID)
		s.recordAndPublish(r.Context(), events.TopicCommentDeleted, c.BeadID, actor, events.CommentDeleted{
			BeadID:    c.BeadID,
			CommentID: c.ID,
			Author:    c.Author,
			Reason:    "trim",
			Fleet:     fleet,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{"deleted_ids": deletedIDs})
}

// handleGetEvents handles GET /v1/beads/{id}/events. With ?limit=N (1..10000)
// returns only the most-recent N events (still ordered ASC by created_at) so
// callers can render activity feeds without loading the full history. With
// ?topic=T it returns only events whose topic is T (the most-recent N of
// them when ?limit is also set), so a caller after one signal does not ship
// the whole history to find it (gd-cP9tODid8j). Both are opt-in: without
// them the unbounded, every-topic default is preserved for the gRPC path and
// legacy HTTP callers.
func (s *BeadsServer) handleGetEvents(w http.ResponseWriter, r *http.Request) {
	beadID := r.PathValue("id")
	if beadID == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	if !s.authorizeSubresourceRead(w, r, beadID, map[string]any{"events": []*model.Event{}}) {
		return
	}

	limit, ok := parseEventsLimit(r.URL.Query().Get("limit"))
	if !ok {
		writeError(w, http.StatusBadRequest, "limit must be an integer between 1 and 10000")
		return
	}

	var (
		evts []*model.Event
		err  error
	)
	if topic := r.URL.Query().Get("topic"); topic != "" {
		evts, err = s.store.GetEventsByTopic(r.Context(), beadID, topic, limit)
	} else if limit > 0 {
		evts, err = s.store.GetRecentEvents(r.Context(), beadID, limit)
	} else {
		evts, err = s.store.GetEvents(r.Context(), beadID)
	}
	if err != nil {
		writeServerError(r.Context(), w, "handleGetEvents", err, "failed to get events")
		return
	}

	if evts == nil {
		evts = []*model.Event{}
	}

	writeJSON(w, http.StatusOK, map[string]any{"events": evts})
}

// parseEventsLimit parses the ?limit= query parameter. Empty string returns
// (0, true) meaning "unlimited / use legacy GetEvents". Non-empty values must
// parse as an integer in [1, 10000]; anything else is invalid.
func parseEventsLimit(raw string) (int, bool) {
	if raw == "" {
		return 0, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > 10000 {
		return 0, false
	}
	return n, true
}
