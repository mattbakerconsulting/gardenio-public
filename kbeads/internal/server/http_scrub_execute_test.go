package server

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
)

// seedScrubRequestBeadWithAttestation is seedScrubRequestBead plus an
// attestation.clean_content blob, for the execution-wiring tests below —
// the plain seedScrubRequestBead fixture (http_scrub_requests_test.go) has
// no attestation, which is fine for the approval-authz tests it serves but
// not for exercising executeScrubRequest's parsing of it. An attestation is
// only seeded when cleanContent is non-nil — the tests that pass nil have
// no review step to attest and must reach approve as an unattested request
// (checkAttestationDrift is a no-op then). When cleanContent is given,
// content_shas is computed the same way handleAttestScrubRequest computes
// it — over each target bead's CURRENT content, creating the target in the
// mock store first if it doesn't already exist — so checkAttestationDrift's
// drift comparison at approve time matches by construction instead of
// failing closed on a missing/stale hash (#150's drift check).
func seedScrubRequestBeadWithAttestation(ms *mockStore, id, taintName, kind, requestedBy string, beadIDs []string, cleanContent map[string]model.ScrubCleanContent) {
	fieldsMap := map[string]any{
		"taint":             taintName,
		"kind":              kind,
		"requested_by":      requestedBy,
		"resolved_bead_ids": beadIDs,
		"approval_status":   "pending",
	}
	if cleanContent != nil {
		contentSHAs := make(map[string]string, len(beadIDs))
		for _, beadID := range beadIDs {
			target, ok := ms.beads[beadID]
			if !ok {
				target = &model.Bead{ID: beadID, Kind: model.KindIssue, Type: "task", Status: model.StatusOpen}
				ms.beads[beadID] = target
			}
			contentSHAs[beadID] = computeBeadContentSHA(target)
		}
		attestation, _ := json.Marshal(map[string]any{
			"content_shas":  contentSHAs,
			"clean_content": cleanContent,
		})
		fieldsMap["attestation"] = json.RawMessage(attestation)
	}
	fields, _ := json.Marshal(fieldsMap)
	ms.beads[id] = &model.Bead{
		ID: id, Kind: model.KindData, Type: model.TypeScrubRequest,
		Title: "scrub " + id, Status: model.StatusOpen, Fields: fields,
	}
}

// TestApproveScrubRequest_ExecutesScrubAndStampsResult covers the
// gd-fVAy32nWDf wiring: a successful approve builds the right
// model.ScrubTaintRequest from the bead's own Fields and calls
// store.ScrubTaint, then stamps executed_at/result/artifact_sha back onto
// the scrub-request bead.
func TestApproveScrubRequest_ExecutesScrubAndStampsResult(t *testing.T) {
	_, ms, h := newTestServer()

	seedScrubTaint(t, ms, 5, "t5", model.ScrubPolicy{Default: model.ScrubModeAuto})
	seedScrubRequestBeadWithAttestation(ms, "gd-scrub-exec", "t5", "purge", "someone@example.com",
		[]string{"gd-target1", "gd-target2"}, nil)

	rec := doJSON(t, h, "POST", "/v1/scrub-requests/gd-scrub-exec/approve", map[string]any{})
	requireStatus(t, rec, 200)

	if len(ms.scrubTaintCalls) != 1 {
		t.Fatalf("scrubTaintCalls = %d, want 1", len(ms.scrubTaintCalls))
	}
	got := ms.scrubTaintCalls[0]
	if got.Kind != "purge" || got.TaintID != 5 {
		t.Errorf("req = %+v, want Kind=purge TaintID=5", got)
	}
	if len(got.BeadIDs) != 2 || got.BeadIDs[0] != "gd-target1" || got.BeadIDs[1] != "gd-target2" {
		t.Errorf("BeadIDs = %v, want [gd-target1 gd-target2]", got.BeadIDs)
	}
	if got.Actor != defaultTestSubject {
		t.Errorf("Actor = %q, want %q", got.Actor, defaultTestSubject)
	}

	fields := scrubRequestFields(t, ms, "gd-scrub-exec")
	if fields["executed_at"] == nil || fields["executed_at"] == "" {
		t.Errorf("executed_at not stamped: %v", fields["executed_at"])
	}
	if fields["artifact_sha"] == nil || fields["artifact_sha"] == "" {
		t.Errorf("artifact_sha not stamped: %v", fields["artifact_sha"])
	}
	result, ok := fields["result"].(map[string]any)
	if !ok || result["status"] != "success" {
		t.Errorf("result = %v, want status=success", fields["result"])
	}

	// gd-qn6yEdlOmx: a successfully-executed scrub-request must not stay
	// in_progress forever — it is terminal, so it closes, the same way a
	// rejected request closes.
	bead := ms.beads["gd-scrub-exec"]
	if bead.Status != model.StatusClosed {
		t.Errorf("bead.Status = %s, want closed after successful execution", bead.Status)
	}
}

// TestApproveScrubRequest_PassesAttestedCleanContent confirms the
// attestation.clean_content blob on the bead reaches
// model.ScrubTaintRequest.CleanContent unchanged.
func TestApproveScrubRequest_PassesAttestedCleanContent(t *testing.T) {
	_, ms, h := newTestServer()

	seedScrubTaint(t, ms, 6, "t6", model.ScrubPolicy{Default: model.ScrubModeAuto, DetaintMode: model.DetaintModeStrip})
	clean := map[string]model.ScrubCleanContent{
		"gd-target1": {Title: "Clean Title", Description: "Clean Desc"},
	}
	seedScrubRequestBeadWithAttestation(ms, "gd-scrub-exec2", "t6", "detaint", "someone@example.com",
		[]string{"gd-target1"}, clean)

	rec := doJSON(t, h, "POST", "/v1/scrub-requests/gd-scrub-exec2/approve", map[string]any{})
	requireStatus(t, rec, 200)

	if len(ms.scrubTaintCalls) != 1 {
		t.Fatalf("scrubTaintCalls = %d, want 1", len(ms.scrubTaintCalls))
	}
	got := ms.scrubTaintCalls[0]
	if got.DetaintMode != model.DetaintModeStrip {
		t.Errorf("DetaintMode = %q, want strip", got.DetaintMode)
	}
	cc, ok := got.CleanContent["gd-target1"]
	if !ok || cc.Title != "Clean Title" || cc.Description != "Clean Desc" {
		t.Errorf("CleanContent[gd-target1] = %+v, want the attested clean content", cc)
	}
}

// TestApproveScrubRequest_RejectsInvalidCleanContentFields pins gd-9rjfQsmQeD:
// clean_content.fields must be validated against the target bead's own type
// schema before ScrubTaint ever runs — previously it was written straight
// through by strip/fork with no check, so an attester's bad payload
// (here, an unknown field name gd-target1's type=task schema doesn't
// declare) would only surface later on some unrelated PATCH. store.ScrubTaint
// must never be called, and the failure must be stamped the same way a
// ScrubTaint-returned error is (approve itself still succeeds — see
// TestApproveScrubRequest_ExecutionFailureDoesNotFailApproval).
func TestApproveScrubRequest_RejectsInvalidCleanContentFields(t *testing.T) {
	_, ms, h := newTestServer()

	seedScrubTaint(t, ms, 10, "t10", model.ScrubPolicy{Default: model.ScrubModeAuto, DetaintMode: model.DetaintModeStrip})
	clean := map[string]model.ScrubCleanContent{
		"gd-target1": {Title: "Clean Title", Fields: json.RawMessage(`{"not_a_real_field": "x"}`)},
	}
	seedScrubRequestBeadWithAttestation(ms, "gd-scrub-badfields", "t10", "detaint", "someone@example.com",
		[]string{"gd-target1"}, clean)

	rec := doJSON(t, h, "POST", "/v1/scrub-requests/gd-scrub-badfields/approve", map[string]any{})
	requireStatus(t, rec, 200)

	if len(ms.scrubTaintCalls) != 0 {
		t.Fatalf("scrubTaintCalls = %d, want 0 — invalid clean_content.fields must block execution", len(ms.scrubTaintCalls))
	}

	fields := scrubRequestFields(t, ms, "gd-scrub-badfields")
	result, ok := fields["result"].(map[string]any)
	if !ok || result["status"] != "failure" {
		t.Fatalf("result = %v, want status=failure", fields["result"])
	}
	if errMsg, _ := result["error"].(string); !strings.Contains(errMsg, "gd-target1") {
		t.Errorf("result.error = %q, want it to name the offending bead gd-target1", errMsg)
	}

	bead := ms.beads["gd-scrub-badfields"]
	if bead.Status != model.StatusClosed {
		t.Errorf("bead.Status = %s, want closed after terminal execution failure", bead.Status)
	}
}

// TestApproveScrubRequest_CleanContentValidationFailsClosedOnStoreError
// pins the merge desk send-back on PR #179 (gd-9rjfQsmQeD): a GetBead-class
// error OTHER than sql.ErrNoRows (a transient store failure, not "this bead
// doesn't exist") must fail the validation closed, not silently skip it and
// let strip write an unvalidated clean_content.fields payload through.
func TestApproveScrubRequest_CleanContentValidationFailsClosedOnStoreError(t *testing.T) {
	_, ms, h := newTestServer()

	seedScrubTaint(t, ms, 11, "t11", model.ScrubPolicy{Default: model.ScrubModeAuto, DetaintMode: model.DetaintModeStrip})
	clean := map[string]model.ScrubCleanContent{
		"gd-target1": {Title: "Clean Title", Description: "Clean Desc"},
	}
	seedScrubRequestBeadWithAttestation(ms, "gd-scrub-storeerr", "t11", "detaint", "someone@example.com",
		[]string{"gd-target1"}, clean)
	ms.getBeadIncludingDeletedErr = errTestScrubExecFailure

	rec := doJSON(t, h, "POST", "/v1/scrub-requests/gd-scrub-storeerr/approve", map[string]any{})
	requireStatus(t, rec, 200)

	if len(ms.scrubTaintCalls) != 0 {
		t.Fatalf("scrubTaintCalls = %d, want 0 — a store error resolving the target bead must block execution", len(ms.scrubTaintCalls))
	}

	fields := scrubRequestFields(t, ms, "gd-scrub-storeerr")
	result, ok := fields["result"].(map[string]any)
	if !ok || result["status"] != "failure" {
		t.Fatalf("result = %v, want status=failure", fields["result"])
	}
}

// TestApproveScrubRequest_RejectsInvalidCleanContentFieldsOnSoftDeletedStripTarget
// pins the merge desk send-back on PR #179 (gd-9rjfQsmQeD): PR #176 made
// purge and detaint/strip scrub a soft-deleted bead in place instead of
// skipping it (it's restorable via `kd restore`), so clean_content.fields
// validation must reach a soft-deleted strip target too — validating only
// through GetBead (which hides soft-deleted rows) would let unvalidated
// fields land on a bead `kd restore` can later revive.
//
// Exercises executeScrubRequest directly rather than the HTTP approve
// endpoint: checkAttestationDrift (the approve-time drift pre-check) itself
// calls plain GetBead, so it 400s on a resolved bead that's already
// soft-deleted before its own, separate concern is reached — the same
// direct-call pattern TestApproveScrubRequest_ExecutionIsIdempotent uses to
// reach executeScrubRequest's own guards in isolation.
func TestApproveScrubRequest_RejectsInvalidCleanContentFieldsOnSoftDeletedStripTarget(t *testing.T) {
	s, ms, _ := newTestServer()

	seedScrubTaint(t, ms, 12, "t12", model.ScrubPolicy{Default: model.ScrubModeAuto, DetaintMode: model.DetaintModeStrip})
	deletedAt := time.Now().UTC()
	ms.beads["gd-target1"] = &model.Bead{
		ID: "gd-target1", Kind: model.KindIssue, Type: "task", Status: model.StatusOpen,
		DeletedAt: &deletedAt,
	}
	clean := map[string]model.ScrubCleanContent{
		"gd-target1": {Title: "Clean Title", Fields: json.RawMessage(`{"not_a_real_field": "x"}`)},
	}
	seedScrubRequestBeadWithAttestation(ms, "gd-scrub-softdel", "t12", "detaint", "someone@example.com",
		[]string{"gd-target1"}, clean)

	taint, err := ms.GetTaint(t.Context(), "t12")
	if err != nil {
		t.Fatalf("GetTaint: %v", err)
	}
	fields, err := parseScrubRequestApprovalFields(ms.beads["gd-scrub-softdel"].Fields)
	if err != nil {
		t.Fatalf("parseScrubRequestApprovalFields: %v", err)
	}
	s.executeScrubRequest(t.Context(), "gd-scrub-softdel", taint, fields, model.DetaintModeStrip, model.ScrubModeAuto, defaultTestSubject)

	if len(ms.scrubTaintCalls) != 0 {
		t.Fatalf("scrubTaintCalls = %d, want 0 — invalid clean_content.fields on a soft-deleted strip target must still block execution", len(ms.scrubTaintCalls))
	}

	requestFields := scrubRequestFields(t, ms, "gd-scrub-softdel")
	result, ok := requestFields["result"].(map[string]any)
	if !ok || result["status"] != "failure" {
		t.Fatalf("result = %v, want status=failure", requestFields["result"])
	}
	if errMsg, _ := result["error"].(string); !strings.Contains(errMsg, "gd-target1") {
		t.Errorf("result.error = %q, want it to name the offending bead gd-target1", errMsg)
	}
}

// TestApproveScrubRequest_SkipsCleanContentValidationForForkOnSoftDeletedOriginal
// pins the other half of the merge desk send-back on PR #179 (gd-9rjfQsmQeD):
// the soft-delete carve-out is scoped to detaint/fork ONLY — scrubDetaintForkBead
// skips a soft-deleted original outright (nothing live to fork from), so an
// invalid clean_content.fields payload for it must not block execution the
// way it would for strip (see
// TestApproveScrubRequest_RejectsInvalidCleanContentFieldsOnSoftDeletedStripTarget).
// Exercises executeScrubRequest directly for the same reason given there.
func TestApproveScrubRequest_SkipsCleanContentValidationForForkOnSoftDeletedOriginal(t *testing.T) {
	s, ms, _ := newTestServer()

	seedScrubTaint(t, ms, 13, "t13", model.ScrubPolicy{Default: model.ScrubModeAuto, DetaintMode: model.DetaintModeFork})
	deletedAt := time.Now().UTC()
	ms.beads["gd-target1"] = &model.Bead{
		ID: "gd-target1", Kind: model.KindIssue, Type: "task", Status: model.StatusOpen,
		DeletedAt: &deletedAt,
	}
	clean := map[string]model.ScrubCleanContent{
		"gd-target1": {Title: "Clean Title", Fields: json.RawMessage(`{"not_a_real_field": "x"}`)},
	}
	seedScrubRequestBeadWithAttestation(ms, "gd-scrub-forksoftdel", "t13", "detaint", "someone@example.com",
		[]string{"gd-target1"}, clean)

	taint, err := ms.GetTaint(t.Context(), "t13")
	if err != nil {
		t.Fatalf("GetTaint: %v", err)
	}
	fields, err := parseScrubRequestApprovalFields(ms.beads["gd-scrub-forksoftdel"].Fields)
	if err != nil {
		t.Fatalf("parseScrubRequestApprovalFields: %v", err)
	}
	s.executeScrubRequest(t.Context(), "gd-scrub-forksoftdel", taint, fields, model.DetaintModeFork, model.ScrubModeAuto, defaultTestSubject)

	if len(ms.scrubTaintCalls) != 1 {
		t.Fatalf("scrubTaintCalls = %d, want 1 — invalid fields on a soft-deleted fork original must not block execution (fork skips it downstream instead)", len(ms.scrubTaintCalls))
	}
}

// TestApproveScrubRequest_PassesAttestedContentSHAs pins the gd-JnACECPcvy
// fix: attestation.content_shas must reach model.ScrubTaintRequest.ContentSHAs
// alongside CleanContent, so ScrubTaint can re-verify "no drift since
// attestation" itself, under its own transaction's row lock, instead of the
// caller trusting checkAttestationDrift's earlier, unlocked read to still
// hold by the time execution runs.
func TestApproveScrubRequest_PassesAttestedContentSHAs(t *testing.T) {
	_, ms, h := newTestServer()

	seedScrubTaint(t, ms, 9, "t9", model.ScrubPolicy{Default: model.ScrubModeAuto, DetaintMode: model.DetaintModeStrip})
	ms.beads["gd-target1"] = &model.Bead{ID: "gd-target1", Kind: model.KindIssue, Type: "task", Status: model.StatusOpen}
	wantSHA := computeBeadContentSHA(ms.beads["gd-target1"])
	clean := map[string]model.ScrubCleanContent{
		"gd-target1": {Title: "Clean Title", Description: "Clean Desc"},
	}
	seedScrubRequestBeadWithAttestation(ms, "gd-scrub-exec3", "t9", "detaint", "someone@example.com",
		[]string{"gd-target1"}, clean)

	rec := doJSON(t, h, "POST", "/v1/scrub-requests/gd-scrub-exec3/approve", map[string]any{})
	requireStatus(t, rec, 200)

	if len(ms.scrubTaintCalls) != 1 {
		t.Fatalf("scrubTaintCalls = %d, want 1", len(ms.scrubTaintCalls))
	}
	got := ms.scrubTaintCalls[0]
	if sha, ok := got.ContentSHAs["gd-target1"]; !ok || sha != wantSHA {
		t.Errorf("ContentSHAs[gd-target1] = %q (ok=%v), want %q", sha, ok, wantSHA)
	}
}

// TestApproveScrubRequest_ExecutionFailureDoesNotFailApproval covers the
// "approval already committed" guarantee: a ScrubTaint failure must not
// turn a successful approve into an error response, and is instead stamped
// onto the bead's Fields.result for the operator to see.
func TestApproveScrubRequest_ExecutionFailureDoesNotFailApproval(t *testing.T) {
	_, ms, h := newTestServer()

	seedScrubTaint(t, ms, 7, "t7", model.ScrubPolicy{Default: model.ScrubModeAuto})
	seedScrubRequestBeadWithAttestation(ms, "gd-scrub-fail", "t7", "purge", "someone@example.com",
		[]string{"gd-target1"}, nil)
	ms.scrubTaintErr = errTestScrubExecFailure

	rec := doJSON(t, h, "POST", "/v1/scrub-requests/gd-scrub-fail/approve", map[string]any{})
	requireStatus(t, rec, 200)

	fields := scrubRequestFields(t, ms, "gd-scrub-fail")
	if fields["approval_status"] != "approved" {
		t.Errorf("approval_status = %v, want approved even though execution failed", fields["approval_status"])
	}
	if _, ok := fields["executed_at"]; ok {
		t.Errorf("executed_at = %v, want unset on execution failure", fields["executed_at"])
	}
	result, ok := fields["result"].(map[string]any)
	if !ok || result["status"] != "failure" {
		t.Errorf("result = %v, want status=failure", fields["result"])
	}

	// gd-qn6yEdlOmx: a terminal execution failure also closes the
	// scrub-request — it will not be retried in place (a fresh scrub-request
	// must be filed), so leaving it in_progress would strand it the same way
	// the success path used to.
	bead := ms.beads["gd-scrub-fail"]
	if bead.Status != model.StatusClosed {
		t.Errorf("bead.Status = %s, want closed after terminal execution failure", bead.Status)
	}
}

// TestApproveScrubRequest_ExecutionIsIdempotent covers the idempotency
// guard: a second approve call (e.g. a retry) after executed_at is already
// set must not call store.ScrubTaint again.
func TestApproveScrubRequest_ExecutionIsIdempotent(t *testing.T) {
	s, ms, h := newTestServer()

	seedScrubTaint(t, ms, 8, "t8", model.ScrubPolicy{Default: model.ScrubModeAuto})
	seedScrubRequestBeadWithAttestation(ms, "gd-scrub-idem", "t8", "purge", "someone@example.com",
		[]string{"gd-target1"}, nil)

	rec := doJSON(t, h, "POST", "/v1/scrub-requests/gd-scrub-idem/approve", map[string]any{})
	requireStatus(t, rec, 200)
	if len(ms.scrubTaintCalls) != 1 {
		t.Fatalf("after first approve: scrubTaintCalls = %d, want 1", len(ms.scrubTaintCalls))
	}

	// The bead's own status flip to in_progress on the first call means a
	// literal second HTTP approve would be rejected earlier in the handler
	// (authorizeResolutionTarget's target-status checks) before ever
	// reaching execution — so exercise the idempotency guard directly, the
	// same way a replay/retry of executeScrubRequest itself would.
	taint, err := ms.GetTaint(t.Context(), "t8")
	if err != nil {
		t.Fatalf("GetTaint: %v", err)
	}
	fields, err := parseScrubRequestApprovalFields(ms.beads["gd-scrub-idem"].Fields)
	if err != nil {
		t.Fatalf("parseScrubRequestApprovalFields: %v", err)
	}
	s.executeScrubRequest(t.Context(), "gd-scrub-idem", taint, fields, "", model.ScrubModeAuto, defaultTestSubject)

	if len(ms.scrubTaintCalls) != 1 {
		t.Errorf("after second (idempotent) call: scrubTaintCalls = %d, want still 1", len(ms.scrubTaintCalls))
	}
}

// TestApproveScrubRequest_ConcurrentExecutionClaimsExactlyOnce covers
// gd-P6uCMzQqU9: two concurrent executeScrubRequest calls racing the same
// fork-mode scrub-request (the shape of two concurrent /approve calls both
// passing the pending check before either write lands) must result in
// exactly one store.ScrubTaint call, not two successor beads. Before the
// fix, executeScrubRequest's idempotency guard was a plain executed_at
// field check with no exclusive claim, so both callers could observe
// executed_at=="" and both proceed.
func TestApproveScrubRequest_ConcurrentExecutionClaimsExactlyOnce(t *testing.T) {
	s, ms, _ := newTestServer()

	seedScrubTaint(t, ms, 9, "t9", model.ScrubPolicy{Default: model.ScrubModeAuto})
	seedScrubRequestBeadWithAttestation(ms, "gd-scrub-race", "t9", "purge", "someone@example.com",
		[]string{"gd-target1"}, nil)

	taint, err := ms.GetTaint(t.Context(), "t9")
	if err != nil {
		t.Fatalf("GetTaint: %v", err)
	}
	fields, err := parseScrubRequestApprovalFields(ms.beads["gd-scrub-race"].Fields)
	if err != nil {
		t.Fatalf("parseScrubRequestApprovalFields: %v", err)
	}

	const concurrency = 10
	var wg sync.WaitGroup
	wg.Add(concurrency)
	for range concurrency {
		go func() {
			defer wg.Done()
			s.executeScrubRequest(t.Context(), "gd-scrub-race", taint, fields, "", model.ScrubModeAuto, defaultTestSubject)
		}()
	}
	wg.Wait()

	if len(ms.scrubTaintCalls) != 1 {
		t.Errorf("scrubTaintCalls = %d, want exactly 1 across %d concurrent executeScrubRequest calls",
			len(ms.scrubTaintCalls), concurrency)
	}
}

// TestApproveScrubRequest_FieldsMergeDoesNotClobberInFlightClaim is the
// merge desk's required interleaving regression for gd-P6uCMzQqU9 (send-back
// on PR #169, https://github.com/joyousaversion/gardenio/pull/169#issuecomment-5807750051):
// TestApproveScrubRequest_ConcurrentExecutionClaimsExactlyOnce only raced
// executeScrubRequest against itself, so it never exercised the actual gap
// — scrub_execution_claimed lives in the same `fields` JSONB the approve
// handler's own approval-fields write (s.mergeBeadFields) touches. Under the
// old Get+UpdateBead read-modify-write, a second concurrent approve's fields
// merge landing AFTER a first approve's claim, but built from a snapshot
// read BEFORE it, would silently overwrite scrub_execution_claimed back to
// absent — undoing the claim without ever calling ClaimScrubExecution
// itself. This pins that scrub_execution_claimed survives a fields merge
// that lands strictly after the claim, proving mergeBeadFields' atomic `||`
// UPDATE (store.Store.MergeBeadFields) never touches keys it wasn't asked
// to set, regardless of ordering.
func TestApproveScrubRequest_FieldsMergeDoesNotClobberInFlightClaim(t *testing.T) {
	s, ms, _ := newTestServer()

	seedScrubTaint(t, ms, 10, "t10", model.ScrubPolicy{Default: model.ScrubModeAuto})
	seedScrubRequestBeadWithAttestation(ms, "gd-scrub-clobber", "t10", "purge", "someone@example.com",
		[]string{"gd-target1"}, nil)

	// Simulate concurrent approve A: it wins the exclusive execution claim
	// first.
	claimed, err := ms.ClaimScrubExecution(t.Context(), "gd-scrub-clobber")
	if err != nil {
		t.Fatalf("ClaimScrubExecution: %v", err)
	}
	if !claimed {
		t.Fatalf("claimed = false, want true (first claim on an unclaimed bead)")
	}

	// Simulate concurrent approve B's approval-fields merge landing AFTER
	// A's claim — the exact interleaving merge desk flagged: a stale
	// read-modify-write here would silently drop scrub_execution_claimed
	// because B's snapshot predates the claim.
	if err := s.mergeBeadFields(t.Context(), "gd-scrub-clobber", map[string]any{
		"approval_status": "approved",
		"approved_by":     "someone-else@example.com",
		"approved_at":     "2026-09-24T00:00:00Z",
		"approval_mode":   "auto",
	}); err != nil {
		t.Fatalf("mergeBeadFields: %v", err)
	}

	fields := scrubRequestFields(t, ms, "gd-scrub-clobber")
	if fields["approval_status"] != "approved" {
		t.Errorf("approval_status = %v, want approved — the merge itself must still land", fields["approval_status"])
	}

	// The claim must have survived the fields merge — a subsequent claim
	// attempt (mirroring B's own executeScrubRequest) must lose the race,
	// exactly like TestClaimScrubExecution_AlreadyClaimedIsNotAnError.
	claimedAgain, err := ms.ClaimScrubExecution(t.Context(), "gd-scrub-clobber")
	if err != nil {
		t.Fatalf("ClaimScrubExecution (second): %v", err)
	}
	if claimedAgain {
		t.Error("claimedAgain = true, want false — the fields merge must not have cleared scrub_execution_claimed")
	}
}

// TestApproveScrubRequest_StatusTransitionDoesNotClobberInFlightClaim is the
// merge desk's third required interleaving regression for gd-P6uCMzQqU9
// (send-back on PR #169,
// https://github.com/joyousaversion/gardenio/pull/169#issuecomment-5808385888):
// handleApproveScrubRequest's own open->in_progress status transition
// (GetBead, then `if open { bead.Status = in_progress; UpdateBead(bead) }`)
// was a SECOND read-modify-write on the same row
// TestApproveScrubRequest_FieldsMergeDoesNotClobberInFlightClaim already
// pinned for mergeBeadFields — UpdateBead(bead) writes the bead's WHOLE row,
// including bead.Fields exactly as GetBead returned it moments earlier. A
// concurrent approve's claim + execution + close landing strictly after
// that GetBead but before this UpdateBead would be silently erased:
// scrub_execution_claimed and executed_at wiped, and worse, a bead the
// concurrent request had already closed would be blindly reopened to
// in_progress. Fixed by store.CASBeadStatus, which touches only
// status/updated_at and only succeeds while the bead is still open.
func TestApproveScrubRequest_StatusTransitionDoesNotClobberInFlightClaim(t *testing.T) {
	s, ms, _ := newTestServer()

	seedScrubTaint(t, ms, 11, "t11", model.ScrubPolicy{Default: model.ScrubModeAuto})
	seedScrubRequestBeadWithAttestation(ms, "gd-scrub-statusrace", "t11", "purge", "someone@example.com",
		[]string{"gd-target1"}, nil)

	// Simulate approve A reaching the point in handleApproveScrubRequest
	// just after its own mergeBeadFields call, where it re-reads the bead
	// snapshot the pre-fix code would later write straight back via
	// UpdateBead(bead).
	if err := s.mergeBeadFields(t.Context(), "gd-scrub-statusrace", map[string]any{
		"approval_status": "approved",
		"approved_by":     "a@example.com",
		"approved_at":     "2026-09-24T00:00:00Z",
		"approval_mode":   "auto",
	}); err != nil {
		t.Fatalf("mergeBeadFields (A): %v", err)
	}
	if _, err := ms.GetBead(t.Context(), "gd-scrub-statusrace"); err != nil {
		t.Fatalf("GetBead (A snapshot): %v", err)
	}

	// Simulate a full concurrent approve B racing ahead of A: claim,
	// execute, stamp executed_at, and close the bead — all landing strictly
	// after A took its snapshot above.
	taint, err := ms.GetTaint(t.Context(), "t11")
	if err != nil {
		t.Fatalf("GetTaint: %v", err)
	}
	fields, err := parseScrubRequestApprovalFields(ms.beads["gd-scrub-statusrace"].Fields)
	if err != nil {
		t.Fatalf("parseScrubRequestApprovalFields: %v", err)
	}
	s.executeScrubRequest(t.Context(), "gd-scrub-statusrace", taint, fields, "", model.ScrubModeAuto, "b@example.com")

	if len(ms.scrubTaintCalls) != 1 {
		t.Fatalf("scrubTaintCalls = %d, want exactly 1 after B's execution", len(ms.scrubTaintCalls))
	}
	if ms.beads["gd-scrub-statusrace"].Status != model.StatusClosed {
		t.Fatalf("bead status = %s, want closed after B's execution completes", ms.beads["gd-scrub-statusrace"].Status)
	}

	// A now performs its (fixed) status transition. Under the old
	// GetBead+UpdateBead(bead) code this would unconditionally reopen the
	// bead to in_progress and clobber B's fields with A's stale snapshot.
	// CASBeadStatus must instead no-op: the bead's current status is
	// closed, not open, so the guarded UPDATE matches no row.
	changed, err := s.store.CASBeadStatus(t.Context(), "gd-scrub-statusrace", model.StatusOpen, model.StatusInProgress)
	if err != nil {
		t.Fatalf("CASBeadStatus (A): %v", err)
	}
	if changed {
		t.Error("changed = true, want false — the bead was already closed by B, A's stale transition must not win")
	}

	bead := ms.beads["gd-scrub-statusrace"]
	if bead.Status != model.StatusClosed {
		t.Errorf("bead status = %s, want closed — A's status transition must not reopen a bead B already closed", bead.Status)
	}

	fields2 := scrubRequestFields(t, ms, "gd-scrub-statusrace")
	if v, ok := fields2["executed_at"].(string); !ok || v == "" {
		t.Errorf("executed_at = %v, want non-empty — B's execution result must survive A's status transition", fields2["executed_at"])
	}

	// A second claim attempt must still lose the race — scrub_execution_claimed
	// must not have been wiped by A's status transition.
	claimedAgain, err := ms.ClaimScrubExecution(t.Context(), "gd-scrub-statusrace")
	if err != nil {
		t.Fatalf("ClaimScrubExecution (second): %v", err)
	}
	if claimedAgain {
		t.Error("claimedAgain = true, want false — A's status transition must not have cleared scrub_execution_claimed")
	}
}

// errTestScrubExecFailure is a fixed sentinel error for the failure-path
// test above.
var errTestScrubExecFailure = &testScrubExecError{}

type testScrubExecError struct{}

func (*testScrubExecError) Error() string { return "simulated scrub execution failure" }

// approveScrubResponseBody mirrors handleApproveScrubRequest's response
// envelope, narrowed to the "artifact" key the gd-FJUw7nG1DV audit-artifact
// tests below care about.
type approveScrubResponseBody struct {
	Artifact *model.ScrubAuditArtifact `json:"artifact"`
}

// cascadeTablesPlusAudit is the tables_touched shape purge and detaint/strip
// share (docs/adr/taints.md §6.7, gd-FJUw7nG1DV): the content-cascade list
// (mirroring internal/store/postgres/scrub_cascade_tables.go's
// cascadeContentTables) plus bead_taints/taint_audit, which the cascade
// helper touches after returning that list.
var cascadeTablesPlusAudit = []string{
	"beads", "comments", "events", "mutation_history", "bead_embeddings",
	"deps", "session_gates", "bead_taints", "taint_audit",
}

func strSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestApproveScrubRequest_ArtifactShape_Purge pins acceptance criterion (b):
// for kind=purge, the response's artifact.beads[i].tables_touched lists
// exactly the purge cascade tables, no successor_bead_id, and the top-level
// detaint_mode field is entirely absent (criterion f).
func TestApproveScrubRequest_ArtifactShape_Purge(t *testing.T) {
	_, ms, h := newTestServer()

	seedScrubTaint(t, ms, 20, "t20", model.ScrubPolicy{Default: model.ScrubModeAuto})
	seedScrubRequestBeadWithAttestation(ms, "gd-scrub-artifact-purge", "t20", "purge", "someone@example.com",
		[]string{"gd-target1", "gd-target2"}, nil)

	cascade := []string{"beads", "comments", "events", "mutation_history", "bead_embeddings", "deps", "session_gates"}
	ms.scrubTaintResult = &model.ScrubTaintResult{Beads: []model.ScrubTaintBeadResult{
		{BeadID: "gd-target1", ContentSHA: "sha-1", TablesTouched: cascade},
		{BeadID: "gd-target2", ContentSHA: "sha-2", TablesTouched: cascade},
	}}

	rec := doJSON(t, h, "POST", "/v1/scrub-requests/gd-scrub-artifact-purge/approve", map[string]any{})
	requireStatus(t, rec, 200)

	if !strings.Contains(rec.Body.String(), `"artifact"`) {
		t.Fatalf("response missing artifact key: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"detaint_mode"`) {
		t.Errorf("purge artifact must omit detaint_mode entirely, got: %s", rec.Body.String())
	}

	var body approveScrubResponseBody
	decodeJSON(t, rec, &body)
	a := body.Artifact
	if a == nil {
		t.Fatal("artifact = nil")
	}
	if a.RequestID != "gd-scrub-artifact-purge" || a.Taint != "t20" || a.Kind != "purge" {
		t.Errorf("artifact = %+v, want request_id/taint/kind for the purge request", a)
	}
	if a.ApprovalMode != model.ScrubModeAuto {
		t.Errorf("ApprovalMode = %q, want auto", a.ApprovalMode)
	}
	if a.DetaintMode != "" {
		t.Errorf("DetaintMode = %q, want empty for purge", a.DetaintMode)
	}
	if len(a.Beads) != 2 {
		t.Fatalf("len(Beads) = %d, want 2 (one per resolved_bead_ids member)", len(a.Beads))
	}
	for _, br := range a.Beads {
		if br.SuccessorBeadID != "" {
			t.Errorf("bead %s: SuccessorBeadID = %q, want empty for purge", br.ID, br.SuccessorBeadID)
		}
		if !strSlicesEqual(br.TablesTouched, cascadeTablesPlusAudit) {
			t.Errorf("bead %s: TablesTouched = %v, want %v", br.ID, br.TablesTouched, cascadeTablesPlusAudit)
		}
	}
}

// TestApproveScrubRequest_ArtifactShape_DetaintStrip pins acceptance
// criterion (d): detaint/strip's tables_touched matches purge's cascade
// exactly, and no entry carries successor_bead_id.
func TestApproveScrubRequest_ArtifactShape_DetaintStrip(t *testing.T) {
	_, ms, h := newTestServer()

	seedScrubTaint(t, ms, 21, "t21", model.ScrubPolicy{Default: model.ScrubModeAuto, DetaintMode: model.DetaintModeStrip})
	clean := map[string]model.ScrubCleanContent{"gd-target1": {Title: "Clean", Description: "Clean"}}
	seedScrubRequestBeadWithAttestation(ms, "gd-scrub-artifact-strip", "t21", "detaint", "someone@example.com",
		[]string{"gd-target1"}, clean)

	cascade := []string{"beads", "comments", "events", "mutation_history", "bead_embeddings", "deps", "session_gates"}
	ms.scrubTaintResult = &model.ScrubTaintResult{Beads: []model.ScrubTaintBeadResult{
		{BeadID: "gd-target1", ContentSHA: "sha-1", TablesTouched: cascade},
	}}

	rec := doJSON(t, h, "POST", "/v1/scrub-requests/gd-scrub-artifact-strip/approve", map[string]any{})
	requireStatus(t, rec, 200)

	var body approveScrubResponseBody
	decodeJSON(t, rec, &body)
	a := body.Artifact
	if a == nil {
		t.Fatal("artifact = nil")
	}
	if a.DetaintMode != model.DetaintModeStrip {
		t.Errorf("DetaintMode = %q, want strip", a.DetaintMode)
	}
	if len(a.Beads) != 1 {
		t.Fatalf("len(Beads) = %d, want 1", len(a.Beads))
	}
	br := a.Beads[0]
	if br.SuccessorBeadID != "" {
		t.Errorf("SuccessorBeadID = %q, want empty for strip", br.SuccessorBeadID)
	}
	if !strSlicesEqual(br.TablesTouched, cascadeTablesPlusAudit) {
		t.Errorf("TablesTouched = %v, want %v", br.TablesTouched, cascadeTablesPlusAudit)
	}
}

// TestApproveScrubRequest_ArtifactShape_DetaintFork pins acceptance
// criterion (c): detaint/fork's tables_touched is exactly beads/taint_audit,
// and every entry carries successor_bead_id.
func TestApproveScrubRequest_ArtifactShape_DetaintFork(t *testing.T) {
	_, ms, h := newTestServer()

	seedScrubTaint(t, ms, 22, "t22", model.ScrubPolicy{Default: model.ScrubModeAuto, DetaintMode: model.DetaintModeFork})
	clean := map[string]model.ScrubCleanContent{"gd-target1": {Title: "Clean", Description: "Clean"}}
	seedScrubRequestBeadWithAttestation(ms, "gd-scrub-artifact-fork", "t22", "detaint", "someone@example.com",
		[]string{"gd-target1"}, clean)

	ms.scrubTaintResult = &model.ScrubTaintResult{Beads: []model.ScrubTaintBeadResult{
		{BeadID: "gd-target1", ContentSHA: "sha-1", SuccessorBeadID: "gd-successor1"},
	}}

	rec := doJSON(t, h, "POST", "/v1/scrub-requests/gd-scrub-artifact-fork/approve", map[string]any{})
	requireStatus(t, rec, 200)

	var body approveScrubResponseBody
	decodeJSON(t, rec, &body)
	a := body.Artifact
	if a == nil {
		t.Fatal("artifact = nil")
	}
	if a.DetaintMode != model.DetaintModeFork {
		t.Errorf("DetaintMode = %q, want fork", a.DetaintMode)
	}
	if len(a.Beads) != 1 {
		t.Fatalf("len(Beads) = %d, want 1", len(a.Beads))
	}
	br := a.Beads[0]
	if br.SuccessorBeadID != "gd-successor1" {
		t.Errorf("SuccessorBeadID = %q, want gd-successor1", br.SuccessorBeadID)
	}
	want := []string{"beads", "taint_audit"}
	if !strSlicesEqual(br.TablesTouched, want) {
		t.Errorf("TablesTouched = %v, want %v", br.TablesTouched, want)
	}
}

// TestApproveScrubRequest_ArtifactShape_DetaintLift pins acceptance
// criterion (e): detaint/lift's tables_touched is exactly
// bead_taints/taint_audit and nothing else, no successor_bead_id, and the
// top-level detaint_mode is present (criterion f covers "including lift").
func TestApproveScrubRequest_ArtifactShape_DetaintLift(t *testing.T) {
	_, ms, h := newTestServer()

	seedScrubTaint(t, ms, 23, "t23", model.ScrubPolicy{Default: model.ScrubModeAuto, DetaintMode: model.DetaintModeLift})
	seedScrubRequestBeadWithAttestation(ms, "gd-scrub-artifact-lift", "t23", "detaint", "someone@example.com",
		[]string{"gd-target1"}, nil)

	ms.scrubTaintResult = &model.ScrubTaintResult{Beads: []model.ScrubTaintBeadResult{
		{BeadID: "gd-target1", ContentSHA: "sha-1"},
	}}

	rec := doJSON(t, h, "POST", "/v1/scrub-requests/gd-scrub-artifact-lift/approve", map[string]any{})
	requireStatus(t, rec, 200)

	var body approveScrubResponseBody
	decodeJSON(t, rec, &body)
	a := body.Artifact
	if a == nil {
		t.Fatal("artifact = nil")
	}
	if a.DetaintMode != model.DetaintModeLift {
		t.Errorf("DetaintMode = %q, want lift", a.DetaintMode)
	}
	if len(a.Beads) != 1 {
		t.Fatalf("len(Beads) = %d, want 1", len(a.Beads))
	}
	br := a.Beads[0]
	if br.SuccessorBeadID != "" {
		t.Errorf("SuccessorBeadID = %q, want empty for lift", br.SuccessorBeadID)
	}
	want := []string{"bead_taints", "taint_audit"}
	if !strSlicesEqual(br.TablesTouched, want) {
		t.Errorf("TablesTouched = %v, want %v", br.TablesTouched, want)
	}
}

// TestApproveScrubRequest_ArtifactSkippedBead covers the stale-taint skip
// path (gd-0fQDgy18lo): a skipped bead still gets one artifact entry (one
// per resolved_bead_ids member, criterion a), but with no content_sha,
// tables_touched, or successor_bead_id — nothing was touched for it.
func TestApproveScrubRequest_ArtifactSkippedBead(t *testing.T) {
	_, ms, h := newTestServer()

	seedScrubTaint(t, ms, 24, "t24", model.ScrubPolicy{Default: model.ScrubModeAuto})
	seedScrubRequestBeadWithAttestation(ms, "gd-scrub-artifact-skip", "t24", "purge", "someone@example.com",
		[]string{"gd-target1", "gd-target2"}, nil)

	ms.scrubTaintResult = &model.ScrubTaintResult{Beads: []model.ScrubTaintBeadResult{
		{BeadID: "gd-target1", ContentSHA: "sha-1", TablesTouched: []string{"beads", "comments", "events", "mutation_history", "bead_embeddings", "deps", "session_gates"}},
		{BeadID: "gd-target2", Skipped: true},
	}}

	rec := doJSON(t, h, "POST", "/v1/scrub-requests/gd-scrub-artifact-skip/approve", map[string]any{})
	requireStatus(t, rec, 200)

	var body approveScrubResponseBody
	decodeJSON(t, rec, &body)
	a := body.Artifact
	if a == nil {
		t.Fatal("artifact = nil")
	}
	if len(a.Beads) != 2 {
		t.Fatalf("len(Beads) = %d, want 2", len(a.Beads))
	}
	skipped := a.Beads[1]
	if skipped.ID != "gd-target2" {
		t.Fatalf("Beads[1].ID = %q, want gd-target2", skipped.ID)
	}
	if skipped.ContentSHA != "" || len(skipped.TablesTouched) != 0 || skipped.SuccessorBeadID != "" {
		t.Errorf("skipped bead entry = %+v, want only ID populated", skipped)
	}
}
