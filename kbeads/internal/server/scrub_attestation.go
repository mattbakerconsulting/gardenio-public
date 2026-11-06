// Attestation (docs/adr/taints.md §6.4, gd-eRKKim4OJc): for a detaint
// scrub-request whose taint policy resolves to a mode requiring
// `attested: true`, the scrub formula's `review` step — an agent task —
// reads every bead in resolved_bead_ids and writes onto the scrub-request
// (never the original beads):
//
//	attestation: {
//	  agent_bead_id: <the reviewing agent's own bead id>,
//	  content_shas:  {bead_id: sha256(bead content at review time)},
//	  clean_content: {bead_id: {title, description, notes, fields}},
//	}
//
// This file is the privileged write surface for that step
// (POST /v1/scrub-requests/{id}/attest, handleAttestScrubRequest) plus the
// content-hash function both this endpoint and the §6.3 approve path
// (checkAttestationDrift, called from handleApproveScrubRequest in
// http_scrub_requests.go) share — computeBeadContentSHA is the single
// definition of "what a bead's content hash means", the same "true by
// construction" pattern scrub_policy_eval.go's doc comment describes for
// EvaluateScrubPolicy/ResolveScrubMode: preview and authorize differ only in
// WHEN they call the same function, never in what it computes.
//
// Why a dedicated endpoint rather than a plain PATCH of `attestation`:
// content_shas must reflect THIS SERVER's own read of each bead's current
// content, not whatever hash a client claims — trusting a client-supplied
// hash would make the drift check (acceptance criterion 2) meaningless. It
// also means the attesting agent's read of every scoped bead genuinely
// happens here, so inherit:read propagation (taint_propagate_read.go) fires
// exactly as it would for a manual GET of each bead (acceptance criterion
// 4), rather than being asserted after the fact. `attestation` is therefore
// folded into scrubRequestCreateForbiddenFields (scrub_request.go) — rejected
// at create by applyScrubRequestGuard and on plain PATCH by
// checkScrubRequestUpdateGuard — and this endpoint is the only writer,
// via mergeBeadFields, the same direct store.UpdateBead bypass approve/reject
// already use to stamp their own privileged fields.
//
// clean_content's shape is mode-independent and WHERE it lands is out of
// scope here (p2-scrub-execute, gd-fVAy32nWDf, reads it later — fork mode to
// CreateBead a successor, strip mode to overwrite the original). This
// endpoint only validates that clean_content covers every resolved bead
// (acceptance criterion 3) and never itself reads or writes the ORIGINAL
// bead's own rows beyond the ordinary authorized read used to compute its
// content hash (acceptance criterion 5) — it writes exclusively onto the
// scrub-request bead.
package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"

	"gardenio/kbeads/internal/model"
)

// computeBeadContentSHA is the single, shared definition of a bead's content
// hash for attestation purposes (file doc comment above) — a thin alias for
// model.BeadContentSHA, which is also called from
// internal/store/postgres/scrub_taint.go's in-transaction drift re-check
// (gd-JnACECPcvy), so both layers compute "content changed" the same way
// without server importing postgres or vice versa.
func computeBeadContentSHA(b *model.Bead) string {
	return model.BeadContentSHA(b)
}

// attestScrubRequestBody is the JSON body for POST
// /v1/scrub-requests/{id}/attest: clean_content only. agent_bead_id is
// server-derived (resolveTaintAgentBeadID) and content_shas is server-
// computed from each named bead's current content — neither is
// client-suppliable, for the reasons in the file doc comment above.
type attestScrubRequestBody struct {
	CleanContent map[string]json.RawMessage `json:"clean_content"`
}

// handleAttestScrubRequest handles POST /v1/scrub-requests/{id}/attest: the
// scrub formula's `review` step (docs/adr/taints.md §6.4/§6.6) calls this
// once, after preparing a clean replacement for every bead named in
// resolved_bead_ids, to record the attestation the §6.3 approve path later
// verifies (checkAttestationDrift in http_scrub_requests.go).
func (s *BeadsServer) handleAttestScrubRequest(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}

	var req attestScrubRequestBody
	if !decodeJSONBody(w, r, &req) {
		return
	}

	targetBead, ok := s.authorizeResolutionTarget(w, r, id, "scrub-request", "handleAttestScrubRequest")
	if !ok {
		return
	}
	if targetBead.Type != model.TypeScrubRequest {
		writeError(w, http.StatusNotFound, "scrub-request not found")
		return
	}

	fields, err := parseScrubRequestApprovalFields(targetBead.Fields)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Attestation exists only for detaint — purge destroys content outright
	// and has no review/clean_content step to attest (§6.6).
	if fields.Kind != "detaint" {
		writeError(w, http.StatusBadRequest, "scrub-request: attestation only applies to kind=detaint")
		return
	}

	ctx := r.Context()

	agentBeadID, ok := resolveTaintAgentBeadID(ctx)
	if !ok {
		writeError(w, http.StatusForbidden, "scrub-request attestation requires an agent identity")
		return
	}

	taint, err := s.store.GetTaint(ctx, fields.Taint)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("scrub-request: unknown taint %q", fields.Taint))
		return
	}
	if err != nil {
		writeServerError(ctx, w, "handleAttestScrubRequest", err, "failed to get taint")
		return
	}

	// Same grant-holder gate applyScrubRequestGuard enforces at create — the
	// review step must itself be authorized to work this taint, not merely
	// authenticated as some agent.
	grants, err := s.grantsOf(ctx)
	if err != nil {
		writeServerError(ctx, w, "handleAttestScrubRequest", err, "failed to resolve grants")
		return
	}
	if !containsTaintID(grants, taint.ID) {
		writeError(w, http.StatusForbidden, fmt.Sprintf("scrub-request: attesting agent holds no grant for taint %q", fields.Taint))
		return
	}

	resolvedIDs := extractResolvedBeadIDs(targetBead.Fields)
	if len(resolvedIDs) == 0 {
		writeError(w, http.StatusBadRequest, "scrub-request: resolved_bead_ids is empty; nothing to attest")
		return
	}

	if err := validateCleanContentCoverage(req.CleanContent, resolvedIDs); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Read every scoped bead through the ordinary authorized-read path — the
	// same one GET /v1/beads/{id} uses — so the attesting agent's inherit:read
	// acquisition (ADR §4.2 row 3) fires exactly as it would for a manual
	// read, and content_shas is computed from THIS read's content, never a
	// client-supplied value (file doc comment above).
	contentSHAs := make(map[string]string, len(resolvedIDs))
	for _, beadID := range resolvedIDs {
		bead, err := s.readBeadForAttestation(ctx, beadID)
		if err != nil {
			var ie inputError
			if errors.As(err, &ie) {
				writeError(w, http.StatusBadRequest, ie.Error())
			} else {
				writeServerError(ctx, w, "handleAttestScrubRequest", err, "failed to read bead for attestation")
			}
			return
		}
		contentSHAs[beadID] = computeBeadContentSHA(bead)
	}

	attestation := map[string]any{
		"agent_bead_id": agentBeadID,
		"content_shas":  contentSHAs,
		"clean_content": req.CleanContent,
	}
	if err := s.mergeBeadFields(ctx, id, map[string]any{"attestation": attestation}); err != nil {
		writeServerError(ctx, w, "handleAttestScrubRequest", err, "failed to record attestation")
		return
	}

	bead, err := s.store.GetBead(ctx, id)
	if errors.Is(err, sql.ErrNoRows) || bead == nil {
		writeError(w, http.StatusNotFound, "scrub-request not found")
		return
	}
	if err != nil {
		writeServerError(ctx, w, "handleAttestScrubRequest", err, "failed to get scrub-request")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"scrub_request": extractScrubRequestFields(bead),
		"issue":         bead,
	})
}

// readBeadForAttestation fetches beadID under the SAME two gates
// handleGetBead applies (project scope, then the taint read gate) and, for a
// caller not exempt from the taint gate, propagates inherit:read exactly as
// a normal GET would (ADR §4.2 row 3) — this is how the attesting agent's
// own bead comes to carry the taint (acceptance criterion 4). A missing or
// unreadable bead is reported as an inputError (400) naming the offending
// id, rather than silently dropping it from the attestation, since
// resolved_bead_ids is the frozen blast radius the whole request is scoped
// to.
func (s *BeadsServer) readBeadForAttestation(ctx context.Context, beadID string) (*model.Bead, error) {
	bead, err := s.getBeadFiltered(ctx, beadID)
	switch {
	case err == nil:
		// fall through to the taint gate below
	case errors.Is(err, errReadTokenDenied), errors.Is(err, errReadDenied):
		return nil, inputError(fmt.Sprintf("scrub-request: attesting agent cannot read bead %s: %s", beadID, err.Error()))
	case errors.Is(err, sql.ErrNoRows) || err.Error() == "bead not found":
		return nil, inputError(fmt.Sprintf("scrub-request: resolved bead %s no longer exists", beadID))
	default:
		return nil, err
	}

	taintGate, err := s.resolveBeadTaintGateState(ctx)
	if err != nil {
		return nil, err
	}
	if !taintGate.exempt {
		taintIDs, err := s.store.GetTaintsForBead(ctx, bead.ID)
		if err != nil {
			return nil, err
		}
		if !taintGate.allows(taintIDs) {
			return nil, inputError(fmt.Sprintf("scrub-request: attesting agent cannot read bead %s", beadID))
		}
		if err := s.propagateReadTaint(ctx, bead.ID); err != nil {
			return nil, err
		}
	}
	return bead, nil
}

// extractResolvedBeadIDs decodes resolved_bead_ids off a scrub-request
// bead's Fields JSON. It never errors: a missing/malformed value simply
// yields nil, matching applyScrubRequestGuard's own writer (setFieldValue
// with a []string) — anything else means the bead's Fields are corrupt, and
// callers here already treat "no resolved beads" as a hard validation
// failure on its own.
func extractResolvedBeadIDs(fields json.RawMessage) []string {
	var f struct {
		ResolvedBeadIDs []string `json:"resolved_bead_ids"`
	}
	if err := json.Unmarshal(fields, &f); err != nil {
		return nil
	}
	return f.ResolvedBeadIDs
}

// scrubAttestation is the parsed shape of a scrub-request's `attestation`
// field, as written by handleAttestScrubRequest and read back by
// checkAttestationDrift at approve.
type scrubAttestation struct {
	AgentBeadID  string                     `json:"agent_bead_id"`
	ContentSHAs  map[string]string          `json:"content_shas"`
	CleanContent map[string]json.RawMessage `json:"clean_content"`
}

// checkAttestationDrift is the §6.3 approve-time twin of
// handleAttestScrubRequest's write: it verifies an existing attestation
// still fully and accurately covers resolvedIDs before approve is allowed to
// finalize (acceptance criteria 1-3). rawAttestation is a scrub-request's raw
// `attestation` Fields value; an absent/null attestation is a no-op here —
// whether one was REQUIRED is the policy evaluator's job
// (resolveScrubRuleForApproval), not this function's.
//
// This is a fail-fast pre-check only — it runs on an unlocked read, before
// approval even commits, purely so a stale attestation surfaces as a 400
// instead of a silently-failed execution later. It is NOT the last word: a
// bead can still drift in the gap between this check and ScrubTaint's own
// transaction (approval commits, then execution runs, possibly much later),
// so ScrubTaint re-verifies content_shas itself under the per-bead FOR
// UPDATE lock its cascade writes under (verifyAttestationNotDrifted in
// internal/store/postgres/scrub_taint.go) — that re-check, not this one, is
// what actually closes the TOCTOU window (gd-JnACECPcvy).
//
// Uses the exact same computeBeadContentSHA function
// handleAttestScrubRequest used to write content_shas, so "match" and
// "drift" mean the same thing at both call sites by construction.
func (s *BeadsServer) checkAttestationDrift(ctx context.Context, resolvedIDs []string, rawAttestation json.RawMessage) error {
	if len(rawAttestation) == 0 || isJSONNull(rawAttestation) {
		return nil
	}
	var att scrubAttestation
	if err := json.Unmarshal(rawAttestation, &att); err != nil {
		return inputError("scrub-request: attestation is malformed: " + err.Error())
	}

	if err := validateCleanContentCoverage(att.CleanContent, resolvedIDs); err != nil {
		return err
	}

	var missing []string
	for _, id := range resolvedIDs {
		if sha, ok := att.ContentSHAs[id]; !ok || sha == "" {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return inputError(fmt.Sprintf(
			"scrub-request: attestation is missing content_shas for resolved bead(s): %v", missing))
	}

	var drifted []string
	for _, id := range resolvedIDs {
		bead, err := s.store.GetBead(ctx, id)
		if err != nil {
			return err
		}
		if bead == nil {
			drifted = append(drifted, id+" (no longer exists)")
			continue
		}
		if computeBeadContentSHA(bead) != att.ContentSHAs[id] {
			drifted = append(drifted, id)
		}
	}
	if len(drifted) > 0 {
		sort.Strings(drifted)
		return inputError(fmt.Sprintf(
			"scrub-request: content changed after attestation for bead(s) %v — re-attest before approving", drifted))
	}
	return nil
}

// validateCleanContentCoverage enforces acceptance criterion 3: clean_content
// must be present for EVERY entry in resolvedIDs, no more and no less. An
// entry missing clean_content fails validation the same way a missing
// content_shas entry does at approve (checkAttestationDrift) — both are
// "the attestation does not actually cover this bead". Extra entries naming
// a bead outside resolved_bead_ids are also rejected: resolved_bead_ids is
// the frozen blast radius (scrub_request.go), and clean_content silently
// covering more than that would let an attestation smuggle a rewrite for a
// bead the request never scoped.
func validateCleanContentCoverage(cleanContent map[string]json.RawMessage, resolvedIDs []string) error {
	want := make(map[string]struct{}, len(resolvedIDs))
	for _, id := range resolvedIDs {
		want[id] = struct{}{}
	}

	var missing []string
	for _, id := range resolvedIDs {
		if raw, ok := cleanContent[id]; !ok || isJSONNull(raw) {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return inputError(fmt.Sprintf(
			"scrub-request: clean_content is missing an entry for resolved bead(s): %v", missing))
	}

	var extra []string
	for id := range cleanContent {
		if _, ok := want[id]; !ok {
			extra = append(extra, id)
		}
	}
	if len(extra) > 0 {
		sort.Strings(extra)
		return inputError(fmt.Sprintf(
			"scrub-request: clean_content names bead(s) outside resolved_bead_ids: %v", extra))
	}
	return nil
}
