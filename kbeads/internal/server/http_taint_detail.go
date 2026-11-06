// This file backs the beads-viewer taint detail page /taints/{name} (ADR
// docs/adr/taints.md §9.1, task gd-nOnc1Q8Zcv): Policy, Grants, Beads,
// Scrubs, and Audit tabs. Each handler reuses an existing gate rather than
// inventing a new one — Policy/Grants/Audit reuse canSeeTaintDetail /
// callerMayManageTaint (owner team or admin, per §9.1: "policies and grants
// are visible only to the owner team and admins"); Beads and Scrubs reuse
// scopeListFilter, the same read gate every GET /v1/beads call goes through,
// so this page can never list a bead the caller could not otherwise read.
package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"gardenio/kbeads/internal/model"
)

// updateTaintPolicyRequest is the JSON body for PATCH /v1/taints/{name}: the
// Policy tab's save action. ScrubPolicy is the only field this endpoint
// writes — Description/OwnerTeam/Effects are preserved from the existing
// row, so a policy save can never smuggle in an ownership or effects change.
type updateTaintPolicyRequest struct {
	ScrubPolicy json.RawMessage `json:"scrub_policy"`
}

// handleUpdateTaintPolicy handles PATCH /v1/taints/{name}. Gated by
// callerMayManageTaintPolicy (owner team or admin, direct OIDC only — no
// X-Operated-By relay fallback, unlike grant/revoke's callerMayManageTaint;
// operator review 2026-09-23 on PR #149). Reuses store.SetTaint's existing
// validation (model.ValidateScrubPolicy) — the client mirrors that
// validation for immediate feedback, but this is the authoritative check
// (task gd-nOnc1Q8Zcv acceptance criteria: "matching p1-taint-store's
// server-side validation"). Records a "policy-update" taint_audit row;
// taint_audit.action is a plain TEXT column with no CHECK constraint (see
// migration 047_taint_registry), so this needed no schema change.
func (s *BeadsServer) handleUpdateTaintPolicy(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}

	var req updateTaintPolicyRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if len(req.ScrubPolicy) == 0 {
		writeError(w, http.StatusBadRequest, "scrub_policy is required")
		return
	}

	ctx := r.Context()
	t, err := s.store.GetTaint(ctx, name)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "taint not found")
		return
	}
	if err != nil {
		writeServerError(ctx, w, "handleUpdateTaintPolicy", err, "failed to get taint")
		return
	}

	may, err := s.callerMayManageTaintPolicy(ctx, t)
	if err != nil {
		writeServerError(ctx, w, "handleUpdateTaintPolicy", err, "failed to resolve team membership")
		return
	}
	if !may {
		writeError(w, http.StatusForbidden, "insufficient permissions: requires a direct OIDC caller who is the taint's owner team or an admin")
		return
	}

	t.ScrubPolicy = req.ScrubPolicy
	if err := s.store.SetTaint(ctx, t); err != nil {
		var ve *model.ValidationError
		if errors.As(err, &ve) {
			writeError(w, http.StatusBadRequest, ve.Error())
			return
		}
		writeServerError(ctx, w, "handleUpdateTaintPolicy", err, "failed to update taint policy")
		return
	}

	if err := s.store.RecordTaintAudit(ctx, &model.TaintAudit{
		TaintID: t.ID,
		Action:  model.TaintAuditPolicyUpdate,
		Actor:   actorFromContext(ctx),
	}); err != nil {
		writeServerError(ctx, w, "handleUpdateTaintPolicy", err, "failed to record taint audit")
		return
	}

	writeJSON(w, http.StatusOK, taintDetailFromModel(t))
}

// handleListTaintGrants handles GET /v1/taints/{name}/grants: the Grants
// tab. Gated by canSeeTaintDetail, same as scrub_policy visibility (§9.1).
// Lists every grant including expired ones — the tab renders expiry itself
// rather than hiding a lapsed row.
func (s *BeadsServer) handleListTaintGrants(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}

	ctx := r.Context()
	t, err := s.store.GetTaint(ctx, name)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "taint not found")
		return
	}
	if err != nil {
		writeServerError(ctx, w, "handleListTaintGrants", err, "failed to get taint")
		return
	}

	visible, err := s.canSeeTaintDetail(r, t.OwnerTeam)
	if err != nil {
		writeServerError(ctx, w, "handleListTaintGrants", err, "failed to resolve team membership")
		return
	}
	if !visible {
		writeError(w, http.StatusForbidden, "insufficient permissions: requires the taint's owner team or an admin")
		return
	}

	grants, err := s.store.ListTaintGrants(ctx, t.ID)
	if err != nil {
		writeServerError(ctx, w, "handleListTaintGrants", err, "failed to list taint grants")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"grants": grants})
}

// handleListTaintAudit handles GET /v1/taints/{name}/audit: the Audit tab.
// Gated by canSeeTaintDetail like the Grants tab. Query params: action,
// actor (exact-match filters), limit, offset. Never includes content — the
// response is model.TaintAudit as-is, which carries only ids, hashes,
// actors, and timestamps (§9.1 acceptance criteria).
func (s *BeadsServer) handleListTaintAudit(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}

	ctx := r.Context()
	t, err := s.store.GetTaint(ctx, name)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "taint not found")
		return
	}
	if err != nil {
		writeServerError(ctx, w, "handleListTaintAudit", err, "failed to get taint")
		return
	}

	visible, err := s.canSeeTaintDetail(r, t.OwnerTeam)
	if err != nil {
		writeServerError(ctx, w, "handleListTaintAudit", err, "failed to resolve team membership")
		return
	}
	if !visible {
		writeError(w, http.StatusForbidden, "insufficient permissions: requires the taint's owner team or an admin")
		return
	}

	q := r.URL.Query()
	filter := model.TaintAuditFilter{
		Action: q.Get("action"),
		Actor:  q.Get("actor"),
	}
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			filter.Limit = n
		}
	}
	filter.Limit = clampListBeadsLimit(filter.Limit, defaultListBeadsLimit)
	if v := q.Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			filter.Offset = n
		}
	}

	audit, total, err := s.store.ListTaintAudit(ctx, t.ID, filter)
	if err != nil {
		writeServerError(ctx, w, "handleListTaintAudit", err, "failed to list taint audit")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"audit": audit, "total": total})
}

// taintBeadEntry is one row of the Beads tab: an ordinary bead the caller is
// authorized to read (via scopeListFilter below) plus its propagation source
// for this specific taint.
type taintBeadEntry struct {
	*model.Bead
	Source       string `json:"source,omitempty"`
	SourceBeadID string `json:"source_bead_id,omitempty"`
}

// handleListTaintBeads handles GET /v1/taints/{name}/beads: the Beads tab.
// Unlike Grants/Audit, this is not owner/admin-restricted — every RoleViewer
// may call it, because scopeListFilter (via TaintID, the same predicate
// taintBeadCount uses) IS the authorization: a caller who holds no grant for
// this taint sees zero rows, never an error, matching every other bead list
// endpoint's "denials do not leak existence beyond the taint's own name"
// convention. This can never list a bead the caller could not otherwise
// read (task gd-nOnc1Q8Zcv acceptance criteria).
func (s *BeadsServer) handleListTaintBeads(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}

	ctx := r.Context()
	t, err := s.store.GetTaint(ctx, name)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "taint not found")
		return
	}
	if err != nil {
		writeServerError(ctx, w, "handleListTaintBeads", err, "failed to get taint")
		return
	}

	q := r.URL.Query()
	base := model.BeadFilter{TaintID: &t.ID, Sort: "-updated_at"}
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			base.Limit = n
		}
	}
	base.Limit = clampListBeadsLimit(base.Limit, defaultListBeadsLimit)
	if v := q.Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			base.Offset = n
		}
	}

	filter := s.scopeListFilter(ctx, base)
	beads, total, err := s.store.ListBeads(ctx, filter)
	if err != nil {
		writeServerError(ctx, w, "handleListTaintBeads", err, "failed to list beads")
		return
	}
	// These rows are, by construction, beads carrying the taint, returned
	// whole: inherit:read like every other bead list (gd-WdnPW8TLPq).
	if err := s.propagateReadTaintBeads(ctx, beadIDsOf(beads)); err != nil {
		writeServerError(ctx, w, "handleListTaintBeads", err, "failed to propagate read taint")
		return
	}

	sources, err := s.store.GetBeadTaintsForTaint(ctx, t.ID, beadIDsOf(beads))
	if err != nil {
		writeServerError(ctx, w, "handleListTaintBeads", err, "failed to resolve propagation sources")
		return
	}

	entries := make([]taintBeadEntry, len(beads))
	for i, b := range beads {
		entries[i] = taintBeadEntry{Bead: b}
		if bt, ok := sources[b.ID]; ok {
			entries[i].Source = bt.Source
			entries[i].SourceBeadID = bt.SourceBeadID
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"beads": entries, "total": total})
}

// taintScrubRequestEntry is one row of the Scrubs tab: an ordinary
// scrub-request bead the caller is authorized to read, plus the taint's
// CURRENT scrub_policy rule resolution for it — resolved fresh on every
// call, the same way handleApproveScrubRequest resolves it, never stored on
// the bead itself. ResolvedMode is shown in the "mode" column; CanApprove/
// CanReject gate the viewer's approve/reject buttons to callers in the
// resolved rule's approvers (task gd-nOnc1Q8Zcv acceptance criteria) without
// exposing the taint's full scrub_policy (owner/admin-only, §9.1) to a
// caller who is merely an approver.
type taintScrubRequestEntry struct {
	*model.Bead
	ResolvedMode string `json:"resolved_mode,omitempty"`
	CanApprove   bool   `json:"can_approve"`
	CanReject    bool   `json:"can_reject"`
}

// handleListTaintScrubRequests handles GET /v1/taints/{name}/scrub-requests:
// the Scrubs tab. Not owner/admin-restricted, same reasoning as
// handleListTaintBeads — scopeListFilter is the authorization. Approve/
// reject visibility mirrors handleApproveScrubRequest's own authorization
// (resolveScrubRuleForApproval + scrubApproverInApprovers) rather than
// re-deriving it, so a viewer never shows a button the API would actually
// refuse — including an over-cap "auto" rule, which resolveScrubRuleForApproval
// falls through the same way handleApproveScrubRequest does, rather than
// showing a request as auto/not-actionable when approve would actually
// route it to a human rule — except four-eyes' same-approver-as-requester
// check, which depends on which specific human clicks and so cannot be
// precomputed per row; the API enforces that at call time regardless of
// what this list shows.
func (s *BeadsServer) handleListTaintScrubRequests(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}

	ctx := r.Context()
	t, err := s.store.GetTaint(ctx, name)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "taint not found")
		return
	}
	if err != nil {
		writeServerError(ctx, w, "handleListTaintScrubRequests", err, "failed to get taint")
		return
	}

	var policy model.ScrubPolicy
	if len(t.ScrubPolicy) > 0 {
		if err := json.Unmarshal(t.ScrubPolicy, &policy); err != nil {
			writeServerError(ctx, w, "handleListTaintScrubRequests", err, "failed to parse scrub policy")
			return
		}
	}

	q := r.URL.Query()
	base := model.BeadFilter{
		Type:   []model.BeadType{model.TypeScrubRequest},
		Fields: map[string]string{"taint": name},
		Sort:   "-created_at",
	}
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			base.Limit = n
		}
	}
	base.Limit = clampListBeadsLimit(base.Limit, defaultListBeadsLimit)
	if v := q.Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			base.Offset = n
		}
	}

	filter := s.scopeListFilter(ctx, base)
	beads, total, err := s.store.ListBeads(ctx, filter)
	if err != nil {
		writeServerError(ctx, w, "handleListTaintScrubRequests", err, "failed to list scrub-requests")
		return
	}
	// A scrub request is a bead with its own text: inherit:read for each
	// returned row (gd-WdnPW8TLPq).
	if err := s.propagateReadTaintBeads(ctx, beadIDsOf(beads)); err != nil {
		writeServerError(ctx, w, "handleListTaintScrubRequests", err, "failed to propagate read taint")
		return
	}

	teams, err := s.TeamsOf(ctx)
	if err != nil {
		writeServerError(ctx, w, "handleListTaintScrubRequests", err, "failed to resolve caller's teams")
		return
	}
	_, isOIDCApprover := scrubApprover(ctx)

	// dailyCount caches this taint's today scrub-execute total across every
	// row below — all beads in this list share the same taint (base.Fields
	// filters on it), so one ListTaintAudit call covers the whole page
	// instead of one per row.
	dailyCount := make(map[int64]int)
	entries := make([]taintScrubRequestEntry, len(beads))
	for i, b := range beads {
		entries[i] = taintScrubRequestEntry{Bead: b}
		fields, err := parseScrubRequestApprovalFields(b.Fields)
		if err != nil {
			continue // malformed Fields: show the bead, no mode/approve annotation
		}
		mode, approvers, err := s.resolveScrubRuleForApproval(ctx, t.ID, &policy, scrubRequestPredicate(fields), len(fields.ResolvedBeadIDs), dailyCount)
		if err != nil {
			writeServerError(ctx, w, "handleListTaintScrubRequests", err, "failed to evaluate scrub policy caps")
			return
		}
		entries[i].ResolvedMode = string(mode)
		entries[i].CanApprove = isOIDCApprover && scrubApproverInApprovers(teams, approvers)
		entries[i].CanReject = entries[i].CanApprove
	}
	writeJSON(w, http.StatusOK, map[string]any{"scrub_requests": entries, "total": total})
}
