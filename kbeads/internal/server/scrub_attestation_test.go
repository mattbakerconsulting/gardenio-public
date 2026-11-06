package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"gardenio/kbeads/internal/model"
)

// Tests for docs/adr/taints.md §6.4 (gd-eRKKim4OJc): the attest write path
// (handleAttestScrubRequest, scrub_attestation.go) and its approve-time twin
// (checkAttestationDrift, called from handleApproveScrubRequest). Reuses the
// seedScrubTaint/seedScrubRequestBead/scrubRequestFields helpers from
// http_scrub_requests_test.go and the newTaintGrantsTestServer/seedBeadTaints/
// beadTaintByID helpers from taint_propagate_read_test.go / taint_read_gate_test.go.

// attestScrubRequestAsAgent issues POST /v1/scrub-requests/{id}/attest
// directly against the handler (bypassing the mux, like getBeadAsAgent does
// for handleGetBead) under a static agent-bead identity.
func attestScrubRequestAsAgent(t *testing.T, s *BeadsServer, scrubRequestID, agentBeadID string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/scrub-requests/"+scrubRequestID+"/attest", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", scrubRequestID)
	ctx := withTestAgent(req.Context(), agentBeadID)
	ctx = WithOperatedByHeader(ctx, "slack:U_TEST")
	rec := httptest.NewRecorder()
	s.handleAttestScrubRequest(rec, req.WithContext(ctx))
	return rec
}

// seedAttestFixture seeds a taint, a pending detaint scrub-request naming
// gd-target1 as its sole resolved bead (seedScrubRequestBead's hardcoded
// shape), the gd-target1 bead itself carrying that taint, and a grant for
// agentBeadID — the minimal fixture every attest happy/failure-path test
// below builds on.
func seedAttestFixture(t *testing.T, ms *mockStore, taintID int64, taintName, scrubRequestID, agentBeadID string) {
	t.Helper()
	seedScrubTaint(t, ms, taintID, taintName, model.ScrubPolicy{Default: model.ScrubModeHuman})
	seedScrubRequestBead(ms, scrubRequestID, taintName, "detaint", "someone@example.com")
	ms.beads["gd-target1"] = &model.Bead{
		ID: "gd-target1", Kind: model.KindIssue, Type: model.TypeTask,
		Title: "secret content", Description: "desc", Status: model.StatusOpen,
	}
	seedBeadTaints(ms, "gd-target1", taintID)
	if err := ms.GrantTaint(t.Context(), &model.TaintGrant{
		TaintID: taintID, PrincipalKind: model.PrincipalAgent, Principal: agentBeadID, GrantedBy: 1,
	}); err != nil {
		t.Fatalf("seed grant: %v", err)
	}
}

// TestHandleAttestScrubRequest_HappyPath covers acceptance criteria 1, 3, 4
// and 5 together: a well-formed attest records server-computed content_shas
// and the supplied clean_content onto the scrub-request, the attesting
// agent's own bead acquires the taint via inherit:read, and the original
// resolved bead's own rows are left untouched.
func TestHandleAttestScrubRequest_HappyPath(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedAttestFixture(t, ms, 1, "t1", "gd-scrub-attest1", "gd-agent1")

	rec := attestScrubRequestAsAgent(t, s, "gd-scrub-attest1", "gd-agent1", map[string]any{
		"clean_content": map[string]any{
			"gd-target1": map[string]any{"title": "clean title", "description": "", "notes": "", "fields": map[string]any{}},
		},
	})
	requireStatus(t, rec, http.StatusOK)

	fields := scrubRequestFields(t, ms, "gd-scrub-attest1")
	att, ok := fields["attestation"].(map[string]any)
	if !ok {
		t.Fatalf("expected attestation map in scrub-request fields, got %v", fields["attestation"])
	}
	if att["agent_bead_id"] != "gd-agent1" {
		t.Errorf("agent_bead_id = %v, want gd-agent1", att["agent_bead_id"])
	}

	shas, ok := att["content_shas"].(map[string]any)
	if !ok {
		t.Fatalf("expected content_shas map, got %v", att["content_shas"])
	}
	wantSHA := computeBeadContentSHA(ms.beads["gd-target1"])
	if shas["gd-target1"] != wantSHA {
		t.Errorf("content_shas[gd-target1] = %v, want server-computed %v", shas["gd-target1"], wantSHA)
	}

	cc, ok := att["clean_content"].(map[string]any)
	if !ok || cc["gd-target1"] == nil {
		t.Fatalf("expected clean_content to carry an entry for gd-target1, got %v", att["clean_content"])
	}

	// Acceptance criterion 4: the attesting agent's own bead now carries the
	// taint, source inherit:read, from reading gd-target1 during attest.
	row, ok := beadTaintByID(ms.beadTaints["gd-agent1"], 1)
	if !ok {
		t.Fatalf("expected attesting agent to acquire taint 1 via inherit:read, got %v", ms.beadTaints["gd-agent1"])
	}
	if row.Source != model.TaintSourceInheritRead {
		t.Errorf("Source = %q; want %q", row.Source, model.TaintSourceInheritRead)
	}
	if row.SourceBeadID != "gd-target1" {
		t.Errorf("SourceBeadID = %q; want %q", row.SourceBeadID, "gd-target1")
	}

	// Acceptance criterion 5: attestation never touches the original bead's
	// own rows — only a read happened against it.
	target := ms.beads["gd-target1"]
	if target.Title != "secret content" || target.Description != "desc" {
		t.Errorf("attestation must not mutate the original bead's own rows, got title=%q description=%q", target.Title, target.Description)
	}
}

// TestHandleAttestScrubRequest_RejectsNonDetaintKind verifies attestation
// only applies to kind=detaint — purge has no review/clean_content step.
func TestHandleAttestScrubRequest_RejectsNonDetaintKind(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedScrubTaint(t, ms, 1, "t1", model.ScrubPolicy{Default: model.ScrubModeHuman})
	seedScrubRequestBead(ms, "gd-scrub-purge1", "t1", "purge", "someone@example.com")

	rec := attestScrubRequestAsAgent(t, s, "gd-scrub-purge1", "gd-agent1", map[string]any{
		"clean_content": map[string]any{},
	})
	requireStatus(t, rec, http.StatusBadRequest)
}

// TestHandleAttestScrubRequest_RejectsNonAgentIdentity verifies an
// OIDC-authenticated caller (not an agent bead's static identity) cannot
// attest — resolveTaintAgentBeadID must fail closed with 403.
func TestHandleAttestScrubRequest_RejectsNonAgentIdentity(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedAttestFixture(t, ms, 1, "t1", "gd-scrub-attest2", "gd-agent1")

	b, _ := json.Marshal(map[string]any{"clean_content": map[string]any{}})
	req := httptest.NewRequest(http.MethodPost, "/v1/scrub-requests/gd-scrub-attest2/attest", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", "gd-scrub-attest2")
	ctx := asOIDC(req.Context(), "someone@example.com")
	rec := httptest.NewRecorder()
	s.handleAttestScrubRequest(rec, req.WithContext(ctx))
	requireStatus(t, rec, http.StatusForbidden)

	fields := scrubRequestFields(t, ms, "gd-scrub-attest2")
	if _, ok := fields["attestation"]; ok {
		t.Errorf("expected no attestation to be recorded, got %v", fields["attestation"])
	}
}

// TestHandleAttestScrubRequest_RejectsNoGrant verifies the review step must
// itself be authorized to work the taint — an authenticated agent identity
// with no grant for the taint is refused, mirroring applyScrubRequestGuard's
// create-time grant-holder gate.
func TestHandleAttestScrubRequest_RejectsNoGrant(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedScrubTaint(t, ms, 1, "t1", model.ScrubPolicy{Default: model.ScrubModeHuman})
	seedScrubRequestBead(ms, "gd-scrub-attest3", "t1", "detaint", "someone@example.com")
	ms.beads["gd-target1"] = &model.Bead{ID: "gd-target1", Kind: model.KindIssue, Type: model.TypeTask, Title: "t"}
	seedBeadTaints(ms, "gd-target1", 1)
	// Deliberately no grant seeded for gd-agent-ungranted.

	rec := attestScrubRequestAsAgent(t, s, "gd-scrub-attest3", "gd-agent-ungranted", map[string]any{
		"clean_content": map[string]any{"gd-target1": map[string]any{}},
	})
	requireStatus(t, rec, http.StatusForbidden)
}

// TestHandleAttestScrubRequest_RejectsMissingCleanContent verifies
// acceptance criterion 3: clean_content missing an entry for a resolved
// bead fails validation, and no attestation is recorded.
func TestHandleAttestScrubRequest_RejectsMissingCleanContent(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedAttestFixture(t, ms, 1, "t1", "gd-scrub-attest4", "gd-agent1")

	rec := attestScrubRequestAsAgent(t, s, "gd-scrub-attest4", "gd-agent1", map[string]any{
		"clean_content": map[string]any{},
	})
	requireStatus(t, rec, http.StatusBadRequest)

	fields := scrubRequestFields(t, ms, "gd-scrub-attest4")
	if _, ok := fields["attestation"]; ok {
		t.Errorf("expected no attestation to be recorded on rejected attest, got %v", fields["attestation"])
	}
}

// TestHandleAttestScrubRequest_RejectsExtraCleanContent verifies
// clean_content naming a bead outside resolved_bead_ids is rejected —
// resolved_bead_ids is the frozen blast radius, and clean_content covering
// more than that would smuggle a rewrite the request never scoped.
func TestHandleAttestScrubRequest_RejectsExtraCleanContent(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedAttestFixture(t, ms, 1, "t1", "gd-scrub-attest5", "gd-agent1")

	rec := attestScrubRequestAsAgent(t, s, "gd-scrub-attest5", "gd-agent1", map[string]any{
		"clean_content": map[string]any{
			"gd-target1": map[string]any{},
			"gd-other":   map[string]any{},
		},
	})
	requireStatus(t, rec, http.StatusBadRequest)
}

// TestHandleAttestScrubRequest_RejectsEmptyResolvedBeadIDs verifies a
// scrub-request with an empty resolved_bead_ids has nothing to attest.
func TestHandleAttestScrubRequest_RejectsEmptyResolvedBeadIDs(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedScrubTaint(t, ms, 1, "t1", model.ScrubPolicy{Default: model.ScrubModeHuman})
	fields, _ := json.Marshal(map[string]any{
		"taint": "t1", "kind": "detaint", "requested_by": "someone@example.com",
		"resolved_bead_ids": []string{}, "approval_status": "pending",
	})
	ms.beads["gd-scrub-empty"] = &model.Bead{
		ID: "gd-scrub-empty", Kind: model.KindData, Type: model.TypeScrubRequest,
		Title: "scrub", Status: model.StatusOpen, Fields: fields,
	}
	if err := ms.GrantTaint(t.Context(), &model.TaintGrant{
		TaintID: 1, PrincipalKind: model.PrincipalAgent, Principal: "gd-agent1", GrantedBy: 1,
	}); err != nil {
		t.Fatalf("seed grant: %v", err)
	}

	rec := attestScrubRequestAsAgent(t, s, "gd-scrub-empty", "gd-agent1", map[string]any{
		"clean_content": map[string]any{},
	})
	requireStatus(t, rec, http.StatusBadRequest)
}

// seedScrubRequestWithAttestation seeds a pending detaint scrub-request
// carrying a pre-written attestation, the shape handleAttestScrubRequest
// itself writes — for exercising checkAttestationDrift's approve-time
// integration independently of the attest endpoint.
func seedScrubRequestWithAttestation(ms *mockStore, id, taintName, requestedBy, agentBeadID string, resolvedIDs []string, contentSHAs map[string]string, cleanContent map[string]any) {
	attestation := map[string]any{
		"agent_bead_id": agentBeadID,
		"content_shas":  contentSHAs,
		"clean_content": cleanContent,
	}
	fields, _ := json.Marshal(map[string]any{
		"taint": taintName, "kind": "detaint", "requested_by": requestedBy,
		"resolved_bead_ids": resolvedIDs, "approval_status": "pending",
		"attestation": attestation,
	})
	ms.beads[id] = &model.Bead{
		ID: id, Kind: model.KindData, Type: model.TypeScrubRequest,
		Title: "scrub " + id, Status: model.StatusOpen, Fields: fields,
	}
}

// TestApproveScrubRequest_AttestationMatchesApproveSucceeds is acceptance
// criterion 1: an attestation whose content_shas match every resolved
// bead's current content passes evaluator validation and approve succeeds.
func TestApproveScrubRequest_AttestationMatchesApproveSucceeds(t *testing.T) {
	_, ms, h := newTestServer()
	seedScrubTaint(t, ms, 1, "t1", model.ScrubPolicy{Default: model.ScrubModeHuman})
	ms.beads["gd-target1"] = &model.Bead{ID: "gd-target1", Kind: model.KindIssue, Type: model.TypeTask, Title: "clean already"}
	sha := computeBeadContentSHA(ms.beads["gd-target1"])
	seedScrubRequestWithAttestation(ms, "gd-scrub-att-ok", "t1", "someone@example.com", "gd-agent1",
		[]string{"gd-target1"}, map[string]string{"gd-target1": sha},
		map[string]any{"gd-target1": map[string]any{"title": "clean already"}})

	rec := doJSON(t, h, "POST", "/v1/scrub-requests/gd-scrub-att-ok/approve", map[string]any{})
	requireStatus(t, rec, 200)

	fields := scrubRequestFields(t, ms, "gd-scrub-att-ok")
	if fields["approval_status"] != "approved" {
		t.Errorf("expected approval_status=approved, got %v", fields["approval_status"])
	}
}

// TestApproveScrubRequest_AttestationDriftFailsClosed is acceptance
// criterion 2: a resolved bead edited after attestation but before approve
// causes approve to fail closed with a clear reason, not a silent pass.
func TestApproveScrubRequest_AttestationDriftFailsClosed(t *testing.T) {
	_, ms, h := newTestServer()
	seedScrubTaint(t, ms, 1, "t1", model.ScrubPolicy{Default: model.ScrubModeHuman})
	ms.beads["gd-target1"] = &model.Bead{ID: "gd-target1", Kind: model.KindIssue, Type: model.TypeTask, Title: "original"}
	sha := computeBeadContentSHA(ms.beads["gd-target1"])
	seedScrubRequestWithAttestation(ms, "gd-scrub-att-drift", "t1", "someone@example.com", "gd-agent1",
		[]string{"gd-target1"}, map[string]string{"gd-target1": sha},
		map[string]any{"gd-target1": map[string]any{"title": "clean"}})

	// Simulate someone editing the bead after attestation, before approve.
	ms.beads["gd-target1"].Title = "edited after attestation"

	rec := doJSON(t, h, "POST", "/v1/scrub-requests/gd-scrub-att-drift/approve", map[string]any{})
	requireStatus(t, rec, 400)
	if !bytes.Contains(rec.Body.Bytes(), []byte("gd-target1")) {
		t.Errorf("expected the 400 response to name the drifted bead, got body %s", rec.Body.String())
	}

	bead := ms.beads["gd-scrub-att-drift"]
	if bead.Status != model.StatusOpen {
		t.Errorf("expected scrub-request to remain open after drift-blocked approve, got %s", bead.Status)
	}
	fields := scrubRequestFields(t, ms, "gd-scrub-att-drift")
	if fields["approval_status"] != "pending" {
		t.Errorf("expected approval_status to remain pending, got %v", fields["approval_status"])
	}
}

// TestApproveScrubRequest_AttestationMissingContentSHAFailsClosed verifies
// an attestation missing a content_shas entry for a resolved bead fails
// approve closed, the same as a hash mismatch.
func TestApproveScrubRequest_AttestationMissingContentSHAFailsClosed(t *testing.T) {
	_, ms, h := newTestServer()
	seedScrubTaint(t, ms, 1, "t1", model.ScrubPolicy{Default: model.ScrubModeHuman})
	ms.beads["gd-target1"] = &model.Bead{ID: "gd-target1", Kind: model.KindIssue, Type: model.TypeTask, Title: "t"}
	seedScrubRequestWithAttestation(ms, "gd-scrub-att-missingsha", "t1", "someone@example.com", "gd-agent1",
		[]string{"gd-target1"}, map[string]string{},
		map[string]any{"gd-target1": map[string]any{"title": "t"}})

	rec := doJSON(t, h, "POST", "/v1/scrub-requests/gd-scrub-att-missingsha/approve", map[string]any{})
	requireStatus(t, rec, 400)

	bead := ms.beads["gd-scrub-att-missingsha"]
	if bead.Status != model.StatusOpen {
		t.Errorf("expected scrub-request to remain open, got %s", bead.Status)
	}
}

// TestApproveScrubRequest_AttestationMissingCleanContentFailsClosed is
// acceptance criterion 3 re-verified at approve (defense in depth): an
// attestation whose clean_content does not cover every resolved bead fails
// approve closed the same way a missing content_shas entry does.
func TestApproveScrubRequest_AttestationMissingCleanContentFailsClosed(t *testing.T) {
	_, ms, h := newTestServer()
	seedScrubTaint(t, ms, 1, "t1", model.ScrubPolicy{Default: model.ScrubModeHuman})
	ms.beads["gd-target1"] = &model.Bead{ID: "gd-target1", Kind: model.KindIssue, Type: model.TypeTask, Title: "t"}
	sha := computeBeadContentSHA(ms.beads["gd-target1"])
	seedScrubRequestWithAttestation(ms, "gd-scrub-att-missingcc", "t1", "someone@example.com", "gd-agent1",
		[]string{"gd-target1"}, map[string]string{"gd-target1": sha},
		map[string]any{})

	rec := doJSON(t, h, "POST", "/v1/scrub-requests/gd-scrub-att-missingcc/approve", map[string]any{})
	requireStatus(t, rec, 400)

	bead := ms.beads["gd-scrub-att-missingcc"]
	if bead.Status != model.StatusOpen {
		t.Errorf("expected scrub-request to remain open, got %s", bead.Status)
	}
}

// TestApproveScrubRequest_NoAttestationApproveUnaffected is the regression
// control: a request with no attestation at all is untouched by the drift
// check — approve proceeds exactly as it did before this task.
func TestApproveScrubRequest_NoAttestationApproveUnaffected(t *testing.T) {
	_, ms, h := newTestServer()
	seedScrubTaint(t, ms, 1, "t1", model.ScrubPolicy{Default: model.ScrubModeHuman})
	seedScrubRequestBead(ms, "gd-scrub-noatt", "t1", "detaint", "someone@example.com")

	rec := doJSON(t, h, "POST", "/v1/scrub-requests/gd-scrub-noatt/approve", map[string]any{})
	requireStatus(t, rec, 200)
}
