package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"time"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// This file implements the taints ADR §6.3 approve/reject surface for
// scrub-request beads (gd-DyWyWFxnzC):
//
//   POST /v1/scrub-requests/{id}/approve   handleApproveScrubRequest
//   POST /v1/scrub-requests/{id}/reject    handleRejectScrubRequest
//
// Both reuse handleApproveEscalation's shape (fetch+authz before mutation,
// server-derived actor, merge fields, close/transition, audit) with the
// three corrections §6.3 makes mandatory over today's escalation approve:
//
//  1. approved_by is SERVER-DERIVED — a body-supplied value is logged, never
//     applied.
//  2. OIDC only — the caller must be a DIRECTLY authenticated OIDC identity
//     (AuthIdentityFromContext(ctx).Type == "oidc" with a non-empty
//     Subject). This endpoint deliberately does NOT call
//     s.escalationApprover: that helper's second accept path trusts a
//     static/fleet token's relayed X-Operated-By header, which lets any
//     holder of the shared crew static token name an arbitrary
//     "oidc:someone@x" identity and pass as a server-derived approver —
//     defeating four-eyes on exactly the endpoint (destructive scrub)
//     where it has to be real (coordinator send-back on PR #87). See
//     scrubApprover below.
//  3. Four-eyes when the resolved mode is human-four-eyes: approved_by !=
//     requested_by after operator_aliases resolution, AND approved_by must
//     be a member of the rule's approvers (scrub_policy approvers entries
//     are "team:<team bead id>" — PINNED coordinator constraint on this
//     task, 2026-09-20: compare bead ids, never resolve slugs here).
//
// Reject has the same restrictions (server-derived + OIDC-only) but no
// four-eyes/approvers check — the ADR ties four-eyes to the approval mode
// specifically, not to reject.
//
// Deliberately NOT here: any replay/execution. Approve only flips
// approval_status; ScrubTaint execution is gd-fVAy32nWDf's job.

// approveScrubRequestBody is the JSON body for POST /v1/scrub-requests/{id}/approve.
type approveScrubRequestBody struct {
	ApprovedBy     string `json:"approved_by"`
	ApprovalReason string `json:"approval_reason"`
}

// rejectScrubRequestBody is the JSON body for POST /v1/scrub-requests/{id}/reject.
type rejectScrubRequestBody struct {
	Reason string `json:"reason"`
}

// handleApproveScrubRequest handles POST /v1/scrub-requests/{id}/approve.
func (s *BeadsServer) handleApproveScrubRequest(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}

	var req approveScrubRequestBody
	if !decodeJSONBody(w, r, &req) {
		return
	}

	targetBead, ok := s.authorizeResolutionTarget(w, r, id, "scrub-request", "handleApproveScrubRequest")
	if !ok {
		return
	}
	if targetBead.Type != model.TypeScrubRequest {
		writeError(w, http.StatusNotFound, "scrub-request not found")
		return
	}
	if targetBead.Status == model.StatusClosed {
		writeError(w, http.StatusConflict, "scrub-request is closed")
		return
	}

	// Corrections 1+2: server-derived, OIDC-only approver identity. NOT
	// s.escalationApprover — see the file doc comment above.
	approver, ok := scrubApprover(r.Context())
	if !ok {
		writeError(w, http.StatusForbidden, "scrub-request approval requires an OIDC-authenticated identity")
		return
	}
	if req.ApprovedBy != "" && req.ApprovedBy != approver {
		slog.Warn("handleApproveScrubRequest: ignoring body approved_by, using derived identity",
			"bead", id, "body_approved_by", req.ApprovedBy, "derived_approved_by", approver)
	}

	fields, err := parseScrubRequestApprovalFields(targetBead.Fields)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// A reject stamps approval_status=denied and closes the bead, but a
	// status-only PATCH (`kd update <id> --status open`) reopens it without
	// ever touching Fields — the closed-status check above only catches a
	// still-closed target. Check the denied verdict itself so a reopened,
	// previously-denied request can never be approved and executed.
	if fields.ApprovalStatus == "denied" {
		writeError(w, http.StatusConflict, "scrub-request was already denied")
		return
	}

	taint, err := s.store.GetTaint(r.Context(), fields.Taint)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("scrub-request: unknown taint %q", fields.Taint))
		return
	}
	if err != nil {
		writeServerError(r.Context(), w, "handleApproveScrubRequest", err, "failed to get taint")
		return
	}

	var policy model.ScrubPolicy
	if len(taint.ScrubPolicy) > 0 {
		if err := json.Unmarshal(taint.ScrubPolicy, &policy); err != nil {
			writeServerError(r.Context(), w, "handleApproveScrubRequest", err, "failed to parse scrub policy")
			return
		}
	}

	mode, approvers, err := s.resolveScrubRuleForApproval(r.Context(), taint.ID, &policy, scrubRequestPredicate(fields), len(fields.ResolvedBeadIDs), nil)
	if err != nil {
		writeServerError(r.Context(), w, "handleApproveScrubRequest", err, "failed to evaluate scrub policy caps")
		return
	}

	// Acceptance criteria 1+2 (gd-eRKKim4OJc, ADR §6.4): when an attestation
	// is present, approve must fail closed if it does not fully and
	// currently cover resolved_bead_ids — a bead edited (by anyone) after
	// attestation but before approve must not slip through as a silent
	// pass. A request with no attestation at all is untouched here: a
	// policy rule requiring attested:true simply never matched above
	// (scrubRequestPredicate only sets "attested" when one is present), so
	// resolveScrubRuleForApproval already fell through to a stricter
	// rule/default — this check only has something to verify once an
	// attestation exists.
	if err := s.checkAttestationDrift(r.Context(), fields.ResolvedBeadIDs, fields.Attestation); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Correction 3: four-eyes, only when the resolved mode demands it.
	if mode == model.ScrubModeHumanFourEyes {
		if s.scrubFourEyesViolation(r.Context(), fields.RequestedBy, approver) {
			writeError(w, http.StatusForbidden, "four-eyes required: approver must differ from the requester")
			return
		}
		teams, err := s.TeamsOf(r.Context())
		if err != nil {
			writeServerError(r.Context(), w, "handleApproveScrubRequest", err, "failed to resolve approver's teams")
			return
		}
		if !scrubApproverInApprovers(teams, approvers) {
			writeError(w, http.StatusForbidden, "four-eyes required: approver must be a member of the rule's approvers")
			return
		}
	}

	extra := map[string]any{
		"approval_status": "approved",
		"approved_by":     approver,
		"approved_at":     time.Now().UTC().Format(time.RFC3339),
		"approval_mode":   string(mode),
	}
	if req.ApprovalReason != "" {
		extra["approval_reason"] = req.ApprovalReason
	}
	// Fatal, not logged-and-continue: a failed write here must not let
	// execution reach ScrubTaint with approval_status still pending/stale
	// (gd-09omBt40fj, merge desk follow-up on PR #144). The scrub-request
	// bead is the only durable record that a human approved this
	// destructive operation — if that write doesn't land, the caller must
	// see a failure and retry, not a 200 that green-lights replay against
	// an unapproved-looking request.
	if err := s.mergeBeadFields(r.Context(), id, extra); err != nil {
		writeServerError(r.Context(), w, "handleApproveScrubRequest", err, "failed to merge scrub-request approval fields")
		return
	}

	bead, err := s.store.GetBead(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) || bead == nil {
		writeError(w, http.StatusNotFound, "scrub-request not found")
		return
	}
	if err != nil {
		writeServerError(r.Context(), w, "handleApproveScrubRequest", err, "failed to get scrub-request")
		return
	}

	if bead.Status == model.StatusOpen {
		// CASBeadStatus (not GetBead-mutate-UpdateBead): a whole-object
		// UpdateBead here would write back this bead.Fields snapshot, which
		// was read before this call and can race a concurrent approve's
		// scrub_execution_claimed claim landing in between — silently
		// clobbering it (gd-P6uCMzQqU9, second merge desk send-back on PR
		// #169). CASBeadStatus touches only status/updated_at.
		changed, err := s.store.CASBeadStatus(r.Context(), id, model.StatusOpen, model.StatusInProgress)
		if err != nil {
			writeServerError(r.Context(), w, "handleApproveScrubRequest", err, "failed to update scrub-request status")
			return
		}
		if changed {
			bead.Status = model.StatusInProgress
			s.recordAndPublish(r.Context(), events.TopicBeadUpdated, bead.ID, approver, events.BeadUpdated{
				Bead:    bead,
				Changes: map[string]any{"status": string(model.StatusInProgress)},
			})
		}
	}

	if err := s.store.RecordTaintAudit(r.Context(), &model.TaintAudit{
		TaintID:      taint.ID,
		Action:       model.TaintAuditScrubApprove,
		RequestID:    id,
		Actor:        approver,
		ApprovalMode: string(mode),
	}); err != nil {
		writeServerError(r.Context(), w, "handleApproveScrubRequest", err, "failed to record taint audit")
		return
	}

	// Execution (docs/adr/taints.md §6.5, gd-fVAy32nWDf) — never here on
	// error: a failed scrub execution does not unwind the approval above,
	// which has already committed. Failure is stamped onto the bead's own
	// Fields for the operator to see, not surfaced as a non-2xx here.
	// Runs detached from r.Context(): approval already committed above, so a
	// client disconnect/timeout past this point must not abort the in-flight
	// destructive scrub (gd-en3QF0P5zC).
	//
	// artifact is nil whenever executeScrubRequest didn't actually run
	// ScrubTaint this call (already executed by a prior/concurrent approve,
	// lost the exclusive claim, or failed) — the §6.7 artifact only exists
	// for the call that produced it, never reconstructed from stored state.
	artifact := s.executeScrubRequest(context.WithoutCancel(r.Context()), id, taint, fields, policy.DetaintMode, mode, approver)

	bead, err = s.store.GetBead(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) || bead == nil {
		writeError(w, http.StatusNotFound, "scrub-request not found")
		return
	}
	if err != nil {
		writeServerError(r.Context(), w, "handleApproveScrubRequest", err, "failed to reload scrub-request after execution")
		return
	}

	resp := map[string]any{
		"scrub_request": extractScrubRequestFields(bead),
		"issue":         bead,
	}
	if artifact != nil {
		resp["artifact"] = artifact
	}
	writeJSON(w, http.StatusOK, resp)
}

// executeScrubRequest runs the docs/adr/taints.md §6.5 execution step for an
// approved scrub-request: it builds a model.ScrubTaintRequest from the
// bead's own Fields (resolved_bead_ids, attestation.clean_content) and calls
// store.ScrubTaint, then stamps executed_at/result/artifact_sha back onto
// the bead via mergeBeadFields — the same store-layer write path used above
// for approval_status etc., which (unlike the generic PATCH/updateBead HTTP
// path) is NOT subject to checkScrubRequestUpdateGuard's blanket reject of
// these fields (scrub_request.go's doc comment names this as
// p2-scrub-execute's own carve-out, the same way authorizeScifApprovalStamp
// carves one out for investigation-request).
//
// Idempotency (gd-P6uCMzQqU9): store.ClaimScrubExecution takes an exclusive
// claim via a single guarded UPDATE before ScrubTaint runs, mirroring
// escalation replay's ClaimEscalationReplay — the prior guard here was just
// an in-Go `executed_at != ""` field check, a read-then-write race that let
// two concurrent /approve calls on the same fork-mode request both pass the
// check and both call ScrubTaint, creating two successor beads. The
// executed_at check below is kept as a cheap short-circuit for the common
// sequential case (skip the UPDATE round-trip for an already-finished
// request); the claim is what actually prevents the race.
//
// Never returns an error to the caller: approval has already committed by
// the time this runs, so a failed execution must not unwind it. Failure is
// stamped onto the bead's Fields for the operator to see instead.
//
// Returns the docs/adr/taints.md §6.7 audit artifact when THIS call is the
// one that actually ran ScrubTaint, nil in every other case (parse failure,
// already executed, lost the exclusive claim, or ScrubTaint itself failed)
// — mode is the resolved approval mode the caller's resolveScrubRuleForApproval
// already computed, carried through only to stamp the artifact
// (ScrubTaintRequest itself has no notion of approval mode).
func (s *BeadsServer) executeScrubRequest(ctx context.Context, id string, taint *model.Taint, fields scrubRequestApprovalFields, detaintMode model.DetaintMode, mode model.ScrubMode, approver string) *model.ScrubAuditArtifact {
	// docs/adr/taints.md §6.5: "runs under WithMachineOperator."
	ctx = WithMachineOperator(ctx)

	bead, err := s.store.GetBead(ctx, id)
	if err != nil || bead == nil {
		slog.Warn("executeScrubRequest: failed to reload scrub-request", "bead", id, "error", err)
		return nil
	}

	var parsed struct {
		ExecutedAt      string                  `json:"executed_at"`
		ResolvedBeadIDs []string                `json:"resolved_bead_ids"`
		Attestation     scrubRequestAttestation `json:"attestation"`
	}
	if err := json.Unmarshal(bead.Fields, &parsed); err != nil {
		s.recordScrubExecutionFailure(ctx, id, approver, fmt.Errorf("parsing scrub-request fields: %w", err))
		return nil
	}
	if parsed.ExecutedAt != "" {
		slog.Info("executeScrubRequest: already executed, skipping", "bead", id)
		return nil
	}
	if len(parsed.ResolvedBeadIDs) == 0 {
		s.recordScrubExecutionFailure(ctx, id, approver, errors.New("resolved_bead_ids is empty"))
		return nil
	}

	// gd-9rjfQsmQeD: the attested clean_content.fields payload strip/fork
	// consume (scrubDetaintStripBead's UPDATE, scrubDetaintForkBead's
	// queryCreateBead) is written straight through with no schema check —
	// only the NEXT unrelated PATCH would catch a missing required key or a
	// wrong-typed value, via ValidateTypeFieldsForUpdate, by which point the
	// scrub has already landed. Validate every attested fields blob against
	// its own bead's current type schema here, before ScrubTaint ever runs,
	// so a bad attestation fails the approve call instead of silently
	// corrupting the scrubbed bead. lift never reads clean_content (it only
	// deletes the bead_taints row — see scrubDetaintLiftBead), so it's
	// exempt; unset DetaintMode defaults to fork downstream (queryScrubTaint),
	// so it's treated as fork here too.
	if fields.Kind == "detaint" && detaintMode != model.DetaintModeLift {
		if err := s.validateScrubCleanContentFields(ctx, parsed.ResolvedBeadIDs, parsed.Attestation.CleanContent, detaintMode); err != nil {
			s.recordScrubExecutionFailure(ctx, id, approver, err)
			return nil
		}
	}

	// Exclusive claim (see doc comment above): only the caller whose UPDATE
	// actually flips scrub_execution_claimed from unset/false to true may
	// proceed to ScrubTaint. A concurrent caller that loses the race gets
	// claimed=false and returns without side effects.
	claimed, err := s.store.ClaimScrubExecution(ctx, id)
	if err != nil {
		slog.Warn("executeScrubRequest: failed to claim execution lock", "bead", id, "error", err)
		return nil
	}
	if !claimed {
		slog.Info("executeScrubRequest: skipped, already claimed by a concurrent execution", "bead", id)
		return nil
	}

	req := &model.ScrubTaintRequest{
		RequestID:       id,
		TaintID:         taint.ID,
		Kind:            fields.Kind,
		DetaintMode:     detaintMode,
		BeadIDs:         parsed.ResolvedBeadIDs,
		ContentSHAs:     parsed.Attestation.ContentSHAs,
		CleanContent:    parsed.Attestation.CleanContent,
		Actor:           approver,
		ActorOperatorID: s.resolveOperatorIDForIdentity(ctx, approver),
	}

	result, err := s.store.ScrubTaint(ctx, req)
	if err != nil {
		// Release the claim on failure so a retry (another approve call, or
		// an operator-triggered re-approve) can attempt execution again —
		// preserves the pre-existing retry-after-failure behavior. On
		// success the claim is deliberately left set: executed_at is stamped
		// below, so nothing will legitimately try to execute this request
		// again.
		if relErr := s.store.ReleaseScrubExecutionClaim(ctx, id); relErr != nil {
			slog.Warn("executeScrubRequest: failed to release execution claim after failure", "bead", id, "error", relErr)
		}
		s.recordScrubExecutionFailure(ctx, id, approver, err)
		return nil
	}

	artifact := model.BuildScrubAuditArtifact(req, taint.Name, mode, result)

	if err := s.mergeBeadFields(ctx, id, map[string]any{
		"executed_at":  time.Now().UTC().Format(time.RFC3339),
		"result":       map[string]any{"status": "success", "beads": result.Beads},
		"artifact_sha": scrubResultArtifactSHA(result),
	}); err != nil {
		slog.Warn("executeScrubRequest: failed to stamp execution result", "bead", id, "error", err)
	}

	// gd-qn6yEdlOmx: execution is terminal on success, the same way reject
	// closes the bead on terminal denial (handleRejectScrubRequest above) —
	// without this, a successfully-executed scrub-request stayed in_progress
	// forever, cluttering any query that filters on open work.
	s.closeScrubRequestBead(ctx, id, approver)

	return artifact
}

// closeScrubRequestBead closes a scrub-request bead once execution has
// reached a terminal outcome (success or terminal failure — see
// recordScrubExecutionFailure), mirroring handleRejectScrubRequest's
// CloseBead + TopicBeadClosed publish for its own terminal outcome (denial).
// Never surfaces an error to the caller: by the time execution runs,
// approval has already committed (see executeScrubRequest's doc comment), so
// a failure to flip status here must not unwind anything — it's logged for
// the operator instead, same treatment as the mergeBeadFields stamp calls
// around it.
func (s *BeadsServer) closeScrubRequestBead(ctx context.Context, id, closedBy string) {
	bead, err := s.store.CloseBead(ctx, id, closedBy)
	if err != nil {
		slog.Warn("closeScrubRequestBead: failed to close scrub-request", "bead", id, "error", err)
		return
	}
	s.recordAndPublish(ctx, events.TopicBeadClosed, bead.ID, closedBy, events.BeadClosed{
		Bead:     bead,
		ClosedBy: closedBy,
	})
}

// scrubRequestAttestation is the shape docs/adr/taints.md §6.4 defines for a
// scrub-request's attestation field that execution consumes. ContentSHAs
// rides along with CleanContent into model.ScrubTaintRequest so ScrubTaint
// can re-verify "no drift since attestation" itself, under the same
// per-bead FOR UPDATE lock its cascade writes under — the checkAttestationDrift
// call above only proves that held at approve time, before this transaction
// even opened (gd-JnACECPcvy); a concurrent edit landing in the gap between
// approve committing and this execution running would otherwise slip past
// unnoticed.
type scrubRequestAttestation struct {
	ContentSHAs  map[string]string                  `json:"content_shas"`
	CleanContent map[string]model.ScrubCleanContent `json:"clean_content"`
}

// validateScrubCleanContentFields checks every resolved bead's attested
// clean_content.Fields against that bead's OWN current type schema
// (model.ValidateTypeFields — the same create-time, full-replacement check
// beads.go's createBead runs, not the update-path grandfathering variant:
// strip/fork both write the attested fields as a whole replacement, never a
// partial patch, so nothing here should be grandfathered past validation).
// A bead missing its clean_content entry is left to fork/strip's own
// "no attested clean_content for bead" error — this only validates entries
// that ARE present.
//
// Fetches via GetBeadIncludingDeleted, not GetBead: scrubPurgeBead and
// scrubDetaintStripBead lock and scrub a soft-deleted bead in place exactly
// like a live one (gd-T5P13cSoBw, PR #176) — validating against GetBead's
// soft-delete-hiding view would skip the exact beads strip still writes to,
// leaving unvalidated fields on a bead `kd restore` can revive. Only
// detaint/fork (detaintMode "" or DetaintModeFork, its default) skips a
// soft-deleted original outright (scrubDetaintForkBead has nothing live to
// fork from), so a soft-deleted bead's attested fields are exempt in that
// mode alone.
//
// A resolved bead with no row AT ALL (sql.ErrNoRows, or the mock store's
// nil/nil convention) is skipped rather than failing the whole scrub on an
// unrelated lookup problem — ScrubTaint's own per-bead lock will surface
// that separately. Any OTHER store error fails closed: a transient error
// must not silently skip validation and let strip/fork write unvalidated
// fields through (merge desk send-back on PR #179).
func (s *BeadsServer) validateScrubCleanContentFields(ctx context.Context, resolvedIDs []string, cleanContent map[string]model.ScrubCleanContent, detaintMode model.DetaintMode) error {
	isFork := detaintMode == "" || detaintMode == model.DetaintModeFork
	for _, beadID := range resolvedIDs {
		clean, ok := cleanContent[beadID]
		if !ok {
			continue
		}
		bead, err := s.store.GetBeadIncludingDeleted(ctx, beadID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return fmt.Errorf("scrub-request: resolving bead %s for clean_content validation: %w", beadID, err)
		}
		if bead == nil {
			continue
		}
		if isFork && bead.DeletedAt != nil {
			continue
		}
		tc, err := s.resolveTypeConfig(ctx, bead.Type)
		if err != nil {
			return fmt.Errorf("scrub-request: resolving type config for bead %s: %w", beadID, err)
		}
		if err := model.ValidateTypeFields(clean.Fields, tc); err != nil {
			return fmt.Errorf("scrub-request: attested clean_content.fields for bead %s is invalid: %w", beadID, err)
		}
	}
	return nil
}

// recordScrubExecutionFailure stamps a failure result so the approve
// response (and any later `kd show`) surfaces why execution did not
// complete, without failing the approve call itself, then closes the
// scrub-request bead: a failure here is terminal (gd-qn6yEdlOmx) — the
// request is not retried in place, a fresh scrub-request must be filed —
// the same way handleRejectScrubRequest closes on denial.
func (s *BeadsServer) recordScrubExecutionFailure(ctx context.Context, id, closedBy string, err error) {
	slog.Warn("executeScrubRequest: execution failed", "bead", id, "error", err)
	if mergeErr := s.mergeBeadFields(ctx, id, map[string]any{
		"result": map[string]any{"status": "failure", "error": err.Error()},
	}); mergeErr != nil {
		slog.Warn("executeScrubRequest: failed to stamp failure result", "bead", id, "error", mergeErr)
	}
	s.closeScrubRequestBead(ctx, id, closedBy)
}

// scrubResultArtifactSHA hashes the ordered list of per-bead content_shas
// ScrubTaint returned into a single artifact_sha for the approve response —
// a checksum of exactly what was scrubbed, without storing any content
// server-side.
func scrubResultArtifactSHA(result *model.ScrubTaintResult) string {
	h := sha256.New()
	for _, br := range result.Beads {
		h.Write([]byte(br.BeadID))
		h.Write([]byte{0})
		h.Write([]byte(br.ContentSHA))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// handleRejectScrubRequest handles POST /v1/scrub-requests/{id}/reject.
func (s *BeadsServer) handleRejectScrubRequest(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}

	var req rejectScrubRequestBody
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		writeError(w, http.StatusBadRequest, "reason is required")
		return
	}

	targetBead, ok := s.authorizeResolutionTarget(w, r, id, "scrub-request", "handleRejectScrubRequest")
	if !ok {
		return
	}
	if targetBead.Type != model.TypeScrubRequest {
		writeError(w, http.StatusNotFound, "scrub-request not found")
		return
	}

	// Same restrictions as approve (server-derived, OIDC-only) — no
	// rejector-identity field is trusted from the body at all, stricter than
	// escalation reject's req.RejectedBy. NOT s.escalationApprover — see the
	// file doc comment above.
	rejector, ok := scrubApprover(r.Context())
	if !ok {
		writeError(w, http.StatusForbidden, "scrub-request rejection requires an OIDC-authenticated identity")
		return
	}

	fields, err := parseScrubRequestApprovalFields(targetBead.Fields)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	taint, err := s.store.GetTaint(r.Context(), fields.Taint)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		writeServerError(r.Context(), w, "handleRejectScrubRequest", err, "failed to get taint")
		return
	}

	extra := map[string]any{
		"approval_status": "denied",
		"approval_reason": req.Reason,
	}
	// Same fatal treatment as approve, for the same reason: a lost write
	// here leaves the request looking pending/unresolved forever even
	// though CloseBead below closes it, corrupting the audit trail.
	if err := s.mergeBeadFields(r.Context(), id, extra); err != nil {
		writeServerError(r.Context(), w, "handleRejectScrubRequest", err, "failed to merge scrub-request reject fields")
		return
	}

	bead, err := s.store.CloseBead(r.Context(), id, rejector)
	if errors.Is(err, sql.ErrNoRows) || bead == nil {
		writeError(w, http.StatusNotFound, "scrub-request not found")
		return
	}
	if err != nil {
		writeServerError(r.Context(), w, "handleRejectScrubRequest", err, "failed to close scrub-request")
		return
	}

	s.recordAndPublish(r.Context(), events.TopicBeadClosed, bead.ID, rejector, events.BeadClosed{
		Bead:     bead,
		ClosedBy: rejector,
	})

	// taint may be unresolvable (unknown taint name survives create only if
	// the taint was later deleted — no such path exists today, but GetTaint
	// erroring with anything other than ErrNoRows already returned above).
	// A missing taint is not fatal to reject: the request itself is still
	// closed as denied.
	if taint != nil {
		if err := s.store.RecordTaintAudit(r.Context(), &model.TaintAudit{
			TaintID:   taint.ID,
			Action:    model.TaintAuditScrubReject,
			RequestID: id,
			Actor:     rejector,
		}); err != nil {
			writeServerError(r.Context(), w, "handleRejectScrubRequest", err, "failed to record taint audit")
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"scrub_request": extractScrubRequestFields(bead),
		"issue":         bead,
	})
}

// scrubRequestApprovalFields is the subset of a scrub-request bead's Fields
// the approve/reject path reads: taint + kind (to resolve the policy rule),
// requested_by (for four-eyes), attestation (to build the predicate's
// "attested" key, see scrubRequestPredicate, and for checkAttestationDrift),
// and resolved_bead_ids (the frozen blast radius checkAttestationDrift
// validates the attestation against).
type scrubRequestApprovalFields struct {
	Taint           string          `json:"taint"`
	Kind            string          `json:"kind"`
	RequestedBy     string          `json:"requested_by"`
	ApprovalStatus  string          `json:"approval_status"`
	Attestation     json.RawMessage `json:"attestation"`
	ResolvedBeadIDs []string        `json:"resolved_bead_ids"`
}

// parseScrubRequestApprovalFields decodes the fields the approve/reject
// handlers need off a scrub-request bead. taint/kind are required by the
// create-time schema, so their absence here means the bead's Fields are
// malformed rather than a legitimate empty state.
func parseScrubRequestApprovalFields(raw json.RawMessage) (scrubRequestApprovalFields, error) {
	var f scrubRequestApprovalFields
	if err := json.Unmarshal(raw, &f); err != nil {
		return f, inputError("scrub-request: invalid fields: " + err.Error())
	}
	if f.Taint == "" || f.Kind == "" {
		return f, inputError("scrub-request: missing taint or kind")
	}
	return f, nil
}

// scrubRequestPredicate builds the minimal, conservative predicate
// resolveScrubRuleForApproval matches a policy rule's `when` against: only
// fields safely available on the request bead itself. A rule requiring a
// predicate key this never populates (older_than_days,
// requester_is_chain_root) simply never matches and falls through to the
// stricter default — this can never resolve to a WEAKER mode than intended,
// only stricter. This is the extension point for gd-u1zizVKGmz's full
// evaluator, which should reuse this predicate-building, not replace it.
func scrubRequestPredicate(f scrubRequestApprovalFields) map[string]any {
	predicate := map[string]any{"kind": f.Kind}
	if len(f.Attestation) > 0 && !isJSONNull(f.Attestation) {
		predicate["attested"] = true
	}
	return predicate
}

// scrubPolicyDefaultMaxPerDayMultiplier is docs/adr/taints.md §10.5's
// default for an "auto" rule that declares max_beads but omits
// max_per_day: 10x the per-request cap.
const scrubPolicyDefaultMaxPerDayMultiplier = 10

// resolveScrubRuleForApproval picks the first rule in policy.Rules whose
// When predicate is a subset-equality match of predicate, first-match per
// docs/adr/taints.md §3/§6.2 — used by every path that needs an
// authoritative (not merely a preview) mode resolution: the approve path
// (gd-d3ZZbjU9gH, docs/adr/taints.md §6.3/§10.5/§11 invariant 5) plus the
// list views that decide whether to show a request as auto/actionable
// (gd-dF7Sn12V0K). A bead matching no rule falls through to
// policy.Default/Approvers. A matching rule whose Mode is "auto" is
// additionally checked against its own max_beads/max_per_day caps. A match
// that is over cap is treated as a non-match — evaluation continues to the
// next rule (or policy.Default) — rather than auto-approving (or showing as
// auto) a truncated scope. requestBeadCount is the request's own
// len(resolved_bead_ids); taintID scopes the max_per_day count to this
// taint's own taint_audit history. dailyCountCache, when non-nil, is reused
// across calls for the same taintID (see scrubTaintDailyAuditCount) so a
// list endpoint resolving many requests against the same taint issues one
// ListTaintAudit query instead of one per request.
func (s *BeadsServer) resolveScrubRuleForApproval(ctx context.Context, taintID int64, policy *model.ScrubPolicy, predicate map[string]any, requestBeadCount int, dailyCountCache map[int64]int) (model.ScrubMode, []string, error) {
	for _, rule := range policy.Rules {
		if !scrubRuleWhenMatches(rule.When, predicate) {
			continue
		}
		if rule.Mode == model.ScrubModeAuto {
			within, err := s.scrubRuleWithinCaps(ctx, taintID, rule, requestBeadCount, dailyCountCache)
			if err != nil {
				return "", nil, err
			}
			if !within {
				continue
			}
		}
		return rule.Mode, rule.Approvers, nil
	}
	return policy.Default, policy.Approvers, nil
}

// scrubRuleWithinCaps reports whether an "auto" rule's caps allow approving
// a request of requestBeadCount beads against taintID: the request must not
// exceed rule.MaxBeads, and approving it must not push the taint's today
// total (already-executed scrub-execute rows plus this request's own
// beads) past max_per_day. Both counts are read from durable store state —
// never a client-supplied counter (§11 invariant 5) — via
// scrubTaintDailyAuditCount (mirrors spawnPolicyWithinCaps in
// spawn_policy.go).
//
// A rule with no positive MaxBeads is always over cap: ValidateScrubPolicy
// requires a positive max_beads on any written "auto" rule, so this can only
// happen via a validation bypass (e.g. a test helper) — fail closed rather
// than auto-approve an unbounded request.
func (s *BeadsServer) scrubRuleWithinCaps(ctx context.Context, taintID int64, rule model.ScrubRule, requestBeadCount int, dailyCountCache map[int64]int) (bool, error) {
	if rule.MaxBeads == nil || *rule.MaxBeads <= 0 {
		return false, nil
	}
	if requestBeadCount > *rule.MaxBeads {
		return false, nil
	}

	maxPerDay := *rule.MaxBeads * scrubPolicyDefaultMaxPerDayMultiplier
	if rule.MaxPerDay != nil && *rule.MaxPerDay > 0 {
		maxPerDay = *rule.MaxPerDay
	}

	dailyTotal, err := s.scrubTaintDailyAuditCount(ctx, taintID, dailyCountCache)
	if err != nil {
		return false, err
	}
	// This request's own beads count toward the cap it would land the
	// taint at, not just what already executed today — otherwise a
	// request landing exactly on the cap boundary (e.g. 28 executed + a
	// 3-bead request under max_per_day=30) auto-approves and overshoots
	// to 31.
	if dailyTotal+requestBeadCount > maxPerDay {
		return false, nil
	}
	return true, nil
}

// scrubTaintDailyAuditCount returns the count of taintID's scrub-execute
// taint_audit rows since the start of today (UTC), consulting cache first
// when one is provided and populating it on a miss. Passing the same cache
// across multiple resolveScrubRuleForApproval calls for the same taintID
// (e.g. resolving every row of a scrub-requests list) collapses what would
// otherwise be one ListTaintAudit query per request into one per taint.
func (s *BeadsServer) scrubTaintDailyAuditCount(ctx context.Context, taintID int64, cache map[int64]int) (int, error) {
	if cache != nil {
		if v, ok := cache[taintID]; ok {
			return v, nil
		}
	}
	startOfDay := time.Now().UTC().Truncate(24 * time.Hour)
	_, dailyTotal, err := s.store.ListTaintAudit(ctx, taintID, model.TaintAuditFilter{
		Action:       model.TaintAuditScrubExecute,
		CreatedAfter: &startOfDay,
		Limit:        1,
	})
	if err != nil {
		return 0, err
	}
	if cache != nil {
		cache[taintID] = dailyTotal
	}
	return dailyTotal, nil
}

// scrubRuleWhenMatches reports whether every key in a rule's `when` predicate
// is present in predicate with an equal value. An empty/malformed when never
// matches.
func scrubRuleWhenMatches(when json.RawMessage, predicate map[string]any) bool {
	if len(when) == 0 {
		return false
	}
	var want map[string]any
	if err := json.Unmarshal(when, &want); err != nil {
		return false
	}
	for k, v := range want {
		pv, ok := predicate[k]
		if !ok || !reflect.DeepEqual(pv, v) {
			return false
		}
	}
	return true
}

// scrubApprover resolves the directly-authenticated OIDC human identity
// permitted to approve or reject a scrub-request (correction 2).
//
// This intentionally does NOT reuse s.escalationApprover, though as of
// gd-ZjiSd9Kzio round 3 the two functions ended up structurally identical:
// escalationApprover ALSO no longer accepts the X-Operated-By relay path.
// Until round 3, escalationApprover had a second accept path
// (escalation_approval_authz.go) for a static/fleet token that relays a
// human's OIDC login via X-Operated-By — built for beads-viewer's approve
// button, which authenticates to the daemon with the primary static token
// and threads the operator's own OIDC session through the header. That was
// judged "correct for escalation approve" on the theory that any caller
// holding the shared static token could forge the header but the blast
// radius was bounded by requireRole — a theory round 3 disproved:
// handleApproveEscalation is requireRole(RoleOperator, …), which
// short-circuits with NO check for a non-OIDC caller (RoleOperator <
// RoleAdmin), so the "bound" never actually applied, and the header led
// straight into replayEscalation's withMachineWrite total bypass — the same
// severity scrub-requests' four-eyes-only protection has, which is why this
// function was already this strict for scrub-requests (coordinator
// send-back on PR #87). The two functions are kept separate rather than
// merged because scrubApprover additionally excludes "edge" (see below),
// which escalationApprover does not.
//
// id.Type == "edge" (gardenio-edge-auth proxy, ka-0000001641) is
// deliberately excluded. escalationApprover treats "edge" as OIDC-equivalent
// for escalations, but docs/adr/taints.md §6.3 says only "OIDC only" and is
// silent on edge; absent an explicit ADR call, this endpoint takes the
// strict reading rather than inheriting escalationApprover's inclusion of
// it. Widen this to accept "edge" only with a deliberate ADR amendment.
func scrubApprover(ctx context.Context) (string, bool) {
	id := AuthIdentityFromContext(ctx)
	if id.Type == "oidc" && id.Subject != "" {
		return id.Subject, true
	}
	return "", false
}

// scrubFourEyesViolation reports whether approver resolves to the same
// operator as requestedBy (a scrub-request's Fields.requested_by string,
// e.g. "oidc:alice@example.com" or "slack:U123"), after operator_aliases
// resolution. Adapts escalationFourEyesViolation's alias-resolution pattern
// (escalation_approval_authz.go) to a Fields-JSON string instead of
// bead.OperatedBy — a scrub-request's requester identity lives in Fields
// (server-stamped by applyScrubRequestGuard), not on the bead's own
// OperatedBy.
//
// Either side failing to resolve to any operator returns false rather than
// blocking: four-eyes exists to stop one human from being both requester and
// approver, and a side that never resolves to a human operator cannot BE
// that human, so there is nothing to enforce against.
func (s *BeadsServer) scrubFourEyesViolation(ctx context.Context, requestedBy, approver string) bool {
	if approver == "" || requestedBy == "" {
		return false
	}
	if requestedBy == approver {
		return true
	}

	approverOp := s.resolveOperatorIDForIdentity(ctx, approver)
	if approverOp == 0 {
		return false
	}
	requesterOp := s.resolveOperatorIDForIdentity(ctx, requestedBy)
	return requesterOp != 0 && requesterOp == approverOp
}

// resolveOperatorIDForIdentity resolves a "<kind>:<subject>" (or bare-email,
// defaulted to kind "oidc") identity string to an operators(id) via
// operator_aliases. Returns 0 when unresolvable.
func (s *BeadsServer) resolveOperatorIDForIdentity(ctx context.Context, identity string) int64 {
	kind, subject := splitOperator(identity)
	if kind == "" {
		kind, subject = "oidc", identity
	}
	op, err := s.store.LookupOperatorAlias(ctx, kind, subject)
	if err != nil {
		return 0
	}
	return op
}

// scrubApproverInApprovers reports whether the caller's own team set
// (teams, from s.TeamsOf) intersects a rule's approvers list. Per the
// PINNED coordinator constraint on this task (2026-09-20): scrub_policy
// approvers entries are stored as "team:<team bead id>" — the same id
// TeamsOf returns — so this compares bead ids directly and never resolves
// slugs.
func scrubApproverInApprovers(teams, approvers []string) bool {
	teamSet := make(map[string]bool, len(teams))
	for _, t := range teams {
		teamSet[t] = true
	}
	for _, a := range approvers {
		id, ok := strings.CutPrefix(a, "team:")
		if ok && teamSet[id] {
			return true
		}
	}
	return false
}

// extractScrubRequestFields extracts the scrub-request-specific fields from
// a bead's Fields JSON for the approve/reject response body, mirroring
// extractEscalationFields' shape (http_escalations.go).
func extractScrubRequestFields(b *model.Bead) map[string]any {
	sr := make(map[string]any)
	if b == nil || len(b.Fields) == 0 {
		return sr
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b.Fields, &fields); err != nil {
		return sr
	}

	stringKeys := []string{
		"taint", "kind", "requested_by",
		"approval_status", "approved_by", "approved_at", "approval_mode", "approval_reason",
		"executed_at", "artifact_sha",
	}
	for _, k := range stringKeys {
		if raw, ok := fields[k]; ok {
			var s string
			if json.Unmarshal(raw, &s) == nil {
				sr[k] = s
			} else {
				sr[k] = strings.TrimSpace(string(raw))
			}
		}
	}

	jsonKeys := []string{"scope", "resolved_bead_ids", "attestation", "result"}
	for _, k := range jsonKeys {
		if raw, ok := fields[k]; ok {
			sr[k] = json.RawMessage(raw)
		}
	}

	return sr
}
