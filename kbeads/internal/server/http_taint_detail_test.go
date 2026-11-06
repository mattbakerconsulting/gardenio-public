package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
)

// Handler tests for the beads-viewer taint detail page /taints/{name}'s
// backend surface (docs/adr/taints.md §9.1, gd-nOnc1Q8Zcv): Policy, Grants,
// Beads, Scrubs, Audit tabs.

// --- PATCH /v1/taints/{name} (Policy tab save) --------------------------

func TestHandleUpdateTaintPolicy_RejectsAutoRuleWithNoCap(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedTaint(ms, 1, "acme-production", "gd-owner-team")
	seedTeamBead(ms, "gd-owner-team", `{"team":"owner","members":["oidc:alice@example.com"]}`)
	ms.operatorAliases = map[string]int64{"oidc\x00alice@example.com": 7}

	body, _ := json.Marshal(updateTaintPolicyRequest{
		ScrubPolicy: json.RawMessage(`{"default":"human","rules":[{"when":{"kind":"detaint"},"mode":"auto"}]}`),
	})
	req := httptest.NewRequest(http.MethodPatch, "/v1/taints/acme-production", bytes.NewReader(body))
	req.SetPathValue("name", "acme-production")
	req = req.WithContext(asOIDC(req.Context(), "alice@example.com"))
	rec := httptest.NewRecorder()

	s.handleUpdateTaintPolicy(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an auto rule with no cap, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if string(ms.taintRegistry["acme-production"].ScrubPolicy) != "" {
		t.Fatalf("expected the rejected policy to not be persisted, got %s", ms.taintRegistry["acme-production"].ScrubPolicy)
	}
}

func TestHandleUpdateTaintPolicy_OwnerTeamCanSave(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedTaint(ms, 1, "acme-production", "gd-owner-team")
	seedTeamBead(ms, "gd-owner-team", `{"team":"owner","members":["oidc:alice@example.com"]}`)
	ms.operatorAliases = map[string]int64{"oidc\x00alice@example.com": 7}

	newPolicy := `{"default":"human","approvers":["team:gd-owner-team"]}`
	body, _ := json.Marshal(updateTaintPolicyRequest{ScrubPolicy: json.RawMessage(newPolicy)})
	req := httptest.NewRequest(http.MethodPatch, "/v1/taints/acme-production", bytes.NewReader(body))
	req.SetPathValue("name", "acme-production")
	req = req.WithContext(asOIDC(req.Context(), "alice@example.com"))
	rec := httptest.NewRecorder()

	s.handleUpdateTaintPolicy(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	stored := ms.taintRegistry["acme-production"]
	var got, want map[string]any
	_ = json.Unmarshal(stored.ScrubPolicy, &got)
	_ = json.Unmarshal([]byte(newPolicy), &want)
	gotEnc, _ := json.Marshal(got)
	wantEnc, _ := json.Marshal(want)
	if string(gotEnc) != string(wantEnc) {
		t.Fatalf("expected persisted policy %s, got %s", wantEnc, gotEnc)
	}

	if len(ms.taintAudit) != 1 {
		t.Fatalf("expected 1 taint_audit row, got %d: %v", len(ms.taintAudit), ms.taintAudit)
	}
	audit := ms.taintAudit[0]
	if audit.Action != model.TaintAuditPolicyUpdate {
		t.Fatalf("expected audit action %q, got %q", model.TaintAuditPolicyUpdate, audit.Action)
	}
	if audit.TaintID != 1 {
		t.Fatalf("expected audit taint_id 1, got %d", audit.TaintID)
	}
}

func TestHandleUpdateTaintPolicy_ForbiddenForOutsider(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedTaint(ms, 1, "acme-production", "gd-owner-team")

	body, _ := json.Marshal(updateTaintPolicyRequest{ScrubPolicy: json.RawMessage(`{"default":"human"}`)})
	req := httptest.NewRequest(http.MethodPatch, "/v1/taints/acme-production", bytes.NewReader(body))
	req.SetPathValue("name", "acme-production")
	req = req.WithContext(asOIDC(req.Context(), "outsider@example.com"))
	rec := httptest.NewRecorder()

	s.handleUpdateTaintPolicy(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

// TestHandleUpdateTaintPolicy_RelayDeniedEvenForOwnerTeamMember pins Fix #1
// (operator review 2026-09-23 on PR #149): unlike
// TestHandleGrantTaint_ViewerRelayOwnerTeamMember_Allowed, a static/fleet
// caller relaying an authenticated owner-team member via X-Operated-By must
// NOT be able to save a scrub policy — policy writes require the caller's
// OWN authentication to be direct OIDC, with no relay fallback.
func TestHandleUpdateTaintPolicy_RelayDeniedEvenForOwnerTeamMember(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedTaint(ms, 1, "acme-production", "gd-owner-team")
	seedTeamBead(ms, "gd-owner-team", `{"team":"owner","members":["oidc:alice@example.com"]}`)
	ms.operatorAliases = map[string]int64{"oidc\x00alice@example.com": 7}

	body, _ := json.Marshal(updateTaintPolicyRequest{ScrubPolicy: json.RawMessage(`{"default":"human"}`)})
	req := httptest.NewRequest(http.MethodPatch, "/v1/taints/acme-production", bytes.NewReader(body))
	req.SetPathValue("name", "acme-production")
	ctx := withTestPrivilegedAgent(req.Context(), "static")
	ctx = WithOperatedByHeader(ctx, "oidc:alice@example.com")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	s.handleUpdateTaintPolicy(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 (relay must not authorize a policy write), got %d; body: %s", rec.Code, rec.Body.String())
	}
	if string(ms.taintRegistry["acme-production"].ScrubPolicy) != "" {
		t.Fatalf("expected the relay-authored policy to not be persisted, got %s", ms.taintRegistry["acme-production"].ScrubPolicy)
	}
}

func TestHandleUpdateTaintPolicy_NotFound(t *testing.T) {
	s, _ := newTaintGrantsTestServer()

	body, _ := json.Marshal(updateTaintPolicyRequest{ScrubPolicy: json.RawMessage(`{"default":"human"}`)})
	req := httptest.NewRequest(http.MethodPatch, "/v1/taints/does-not-exist", bytes.NewReader(body))
	req.SetPathValue("name", "does-not-exist")
	req = req.WithContext(asOIDC(req.Context(), "alice@example.com"))
	rec := httptest.NewRecorder()

	s.handleUpdateTaintPolicy(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

// --- GET /v1/taints/{name}/grants (Grants tab) ---------------------------

func TestHandleListTaintGrants_OwnerSeesGrantsIncludingExpired(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedTaint(ms, 1, "acme-production", "gd-owner-team")
	seedTeamBead(ms, "gd-owner-team", `{"team":"owner","members":["oidc:alice@example.com"]}`)
	ms.operatorAliases = map[string]int64{"oidc\x00alice@example.com": 7}
	past := time.Now().Add(-time.Hour)
	if err := ms.GrantTaint(context.Background(), &model.TaintGrant{TaintID: 1, PrincipalKind: model.PrincipalOperator, Principal: "9", GrantedBy: 7}); err != nil {
		t.Fatalf("GrantTaint: %v", err)
	}
	if err := ms.GrantTaint(context.Background(), &model.TaintGrant{TaintID: 1, PrincipalKind: model.PrincipalTeam, Principal: "gd-other-team", GrantedBy: 7, ExpiresAt: &past}); err != nil {
		t.Fatalf("GrantTaint: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/taints/acme-production/grants", nil)
	req.SetPathValue("name", "acme-production")
	req = req.WithContext(asOIDC(req.Context(), "alice@example.com"))
	rec := httptest.NewRecorder()

	s.handleListTaintGrants(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Grants []model.TaintGrant `json:"grants"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Grants) != 2 {
		t.Fatalf("expected 2 grants (including the expired one), got %d: %+v", len(resp.Grants), resp.Grants)
	}
}

func TestHandleListTaintGrants_ForbiddenForOutsider(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedTaint(ms, 1, "acme-production", "gd-owner-team")

	req := httptest.NewRequest(http.MethodGet, "/v1/taints/acme-production/grants", nil)
	req.SetPathValue("name", "acme-production")
	req = req.WithContext(asOIDC(req.Context(), "outsider@example.com"))
	rec := httptest.NewRecorder()

	s.handleListTaintGrants(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

// --- GET /v1/taints/{name}/audit (Audit tab) -----------------------------

func TestHandleListTaintAudit_FiltersByActionAndActor(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedTaint(ms, 1, "acme-production", "gd-owner-team")
	seedTeamBead(ms, "gd-owner-team", `{"team":"owner","members":["oidc:alice@example.com"]}`)
	ms.operatorAliases = map[string]int64{"oidc\x00alice@example.com": 7}
	if err := ms.RecordTaintAudit(context.Background(), &model.TaintAudit{TaintID: 1, Action: model.TaintAuditGrant, Actor: "gd-alice"}); err != nil {
		t.Fatalf("RecordTaintAudit: %v", err)
	}
	if err := ms.RecordTaintAudit(context.Background(), &model.TaintAudit{TaintID: 1, Action: model.TaintAuditRevoke, Actor: "gd-bob"}); err != nil {
		t.Fatalf("RecordTaintAudit: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/taints/acme-production/audit?action=revoke", nil)
	req.SetPathValue("name", "acme-production")
	req = req.WithContext(asOIDC(req.Context(), "alice@example.com"))
	rec := httptest.NewRecorder()

	s.handleListTaintAudit(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Audit []model.TaintAudit `json:"audit"`
		Total int                `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Audit) != 1 || resp.Audit[0].Actor != "gd-bob" {
		t.Fatalf("expected 1 filtered row for gd-bob's revoke, got %+v", resp.Audit)
	}
}

func TestHandleListTaintAudit_ForbiddenForOutsider(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedTaint(ms, 1, "acme-production", "gd-owner-team")

	req := httptest.NewRequest(http.MethodGet, "/v1/taints/acme-production/audit", nil)
	req.SetPathValue("name", "acme-production")
	req = req.WithContext(asOIDC(req.Context(), "outsider@example.com"))
	rec := httptest.NewRecorder()

	s.handleListTaintAudit(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

// --- GET /v1/taints/{name}/beads (Beads tab) -----------------------------

// TestHandleListTaintBeads_OnlyReturnsGrantedBeads pins the acceptance
// criterion that the Beads tab never lists a bead the viewer cannot read: it
// reuses scopeListFilter (the same gate as every GET /v1/beads call), so a
// caller with no grant for this taint sees zero rows, not an error.
func TestHandleListTaintBeads_OnlyReturnsGrantedBeads(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedTaint(ms, 1, "acme-production", "gd-owner-team")
	ms.beads["gd-bead1"] = &model.Bead{ID: "gd-bead1", Kind: model.KindIssue, Type: model.TypeTask, Title: "t1", Fleet: "gardenio"}
	ms.taints = map[string][]int64{"gd-bead1": {1}}

	req := httptest.NewRequest(http.MethodGet, "/v1/taints/acme-production/beads", nil)
	req.SetPathValue("name", "acme-production")
	req = req.WithContext(asOIDC(req.Context(), "outsider@example.com"))
	rec := httptest.NewRecorder()

	s.handleListTaintBeads(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Beads []map[string]any `json:"beads"`
		Total int              `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Beads) != 0 {
		t.Fatalf("expected 0 beads for a caller with no grant, got %+v", resp.Beads)
	}
}

func TestHandleListTaintBeads_IncludesPropagationSourceForGrantedCaller(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedTaint(ms, 1, "acme-production", "gd-owner-team")
	ms.operatorAliases = map[string]int64{"oidc\x00grantee@example.com": 7}
	ms.beads["gd-bead1"] = &model.Bead{ID: "gd-bead1", Kind: model.KindIssue, Type: model.TypeTask, Title: "t1", Fleet: "gardenio"}
	ms.taints = map[string][]int64{"gd-bead1": {1}}
	if _, err := ms.ApplyTaint(context.Background(), &model.BeadTaint{BeadID: "gd-bead1", TaintID: 1, Source: "inherit:create", SourceBeadID: "gd-parent", AppliedBy: "gd-system"}); err != nil {
		t.Fatalf("ApplyTaint: %v", err)
	}
	if err := ms.GrantTaint(context.Background(), &model.TaintGrant{TaintID: 1, PrincipalKind: model.PrincipalOperator, Principal: "7", GrantedBy: 7}); err != nil {
		t.Fatalf("GrantTaint: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/taints/acme-production/beads", nil)
	req.SetPathValue("name", "acme-production")
	req = req.WithContext(asOIDC(req.Context(), "grantee@example.com"))
	rec := httptest.NewRecorder()

	s.handleListTaintBeads(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Beads []map[string]any `json:"beads"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Beads) != 1 {
		t.Fatalf("expected 1 bead for the grant holder, got %d", len(resp.Beads))
	}
	row := resp.Beads[0]
	if row["source"] != "inherit:create" || row["source_bead_id"] != "gd-parent" {
		t.Fatalf("expected propagation source annotated, got %+v", row)
	}
}

// --- GET /v1/taints/{name}/scrub-requests (Scrubs tab) -------------------

func TestHandleListTaintScrubRequests_CanApproveOnlyForApprovers(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedScrubTaint(t, ms, 1, "acme-production", model.ScrubPolicy{
		Default:   model.ScrubModeHuman,
		Approvers: []string{"team:gd-approvers"},
	})
	seedTeamBead(ms, "gd-approvers", `{"team":"approvers","members":["oidc:approver@example.com"]}`)
	ms.operatorAliases = map[string]int64{"oidc\x00approver@example.com": 9}
	seedScrubRequestBead(ms, "gd-scrub1", "acme-production", "detaint", "requester@example.com")

	req := httptest.NewRequest(http.MethodGet, "/v1/taints/acme-production/scrub-requests", nil)
	req.SetPathValue("name", "acme-production")
	req = req.WithContext(asOIDC(req.Context(), "approver@example.com"))
	rec := httptest.NewRecorder()
	s.handleListTaintScrubRequests(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		ScrubRequests []map[string]any `json:"scrub_requests"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.ScrubRequests) != 1 {
		t.Fatalf("expected 1 scrub request, got %d", len(resp.ScrubRequests))
	}
	if resp.ScrubRequests[0]["can_approve"] != true || resp.ScrubRequests[0]["can_reject"] != true {
		t.Fatalf("expected approver to see can_approve/can_reject=true, got %+v", resp.ScrubRequests[0])
	}

	// A caller outside the approvers list must not see the actions.
	reqOutsider := httptest.NewRequest(http.MethodGet, "/v1/taints/acme-production/scrub-requests", nil)
	reqOutsider.SetPathValue("name", "acme-production")
	reqOutsider = reqOutsider.WithContext(asOIDC(reqOutsider.Context(), "outsider@example.com"))
	recOutsider := httptest.NewRecorder()
	s.handleListTaintScrubRequests(recOutsider, reqOutsider)

	var respOutsider struct {
		ScrubRequests []map[string]any `json:"scrub_requests"`
	}
	if err := json.Unmarshal(recOutsider.Body.Bytes(), &respOutsider); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(respOutsider.ScrubRequests) != 1 {
		t.Fatalf("expected the outsider to still see the bead (not owner/admin-gated), got %d", len(respOutsider.ScrubRequests))
	}
	if respOutsider.ScrubRequests[0]["can_approve"] == true || respOutsider.ScrubRequests[0]["can_reject"] == true {
		t.Fatalf("expected outsider to NOT see can_approve/can_reject, got %+v", respOutsider.ScrubRequests[0])
	}
}

// TestHandleListTaintScrubRequests_ResolvedModeReflectsCaps is the merge-desk
// fix for PR #182 (gd-d3ZZbjU9gH): this list must resolve mode through the
// same cap-aware evaluator handleApproveScrubRequest uses, not the uncapped
// resolveScrubRule — otherwise an over-cap "auto" request is shown as
// mode=auto/can_approve=false (the auto rule names no approvers) and never
// appears as actionable in the fallback human rule's own approvers' queue,
// even though approving it for real would fall through to that human rule.
func TestHandleListTaintScrubRequests_ResolvedModeReflectsCaps(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	maxBeads := 10
	maxPerDay := 2
	seedScrubTaint(t, ms, 1, "acme-production", model.ScrubPolicy{
		Rules: []model.ScrubRule{
			{When: json.RawMessage(`{"kind":"detaint"}`), Mode: model.ScrubModeAuto, MaxBeads: &maxBeads, MaxPerDay: &maxPerDay},
			{When: json.RawMessage(`{"kind":"detaint"}`), Mode: model.ScrubModeHuman, Approvers: []string{"team:gd-approvers"}},
		},
		Default: model.ScrubModeHumanFourEyes,
	})
	seedTeamBead(ms, "gd-approvers", `{"team":"approvers","members":["oidc:approver@example.com"]}`)
	ms.operatorAliases = map[string]int64{"oidc\x00approver@example.com": 9}
	// The taint has already hit its max_per_day of 2 today, so the auto
	// rule is over cap and must fall through to the human rule below it.
	seedTaintAuditScrubExecuteRows(ms, 1, 2)
	seedScrubRequestBead(ms, "gd-scrub-overcap-list", "acme-production", "detaint", "requester@example.com")

	req := httptest.NewRequest(http.MethodGet, "/v1/taints/acme-production/scrub-requests", nil)
	req.SetPathValue("name", "acme-production")
	req = req.WithContext(asOIDC(req.Context(), "approver@example.com"))
	rec := httptest.NewRecorder()
	s.handleListTaintScrubRequests(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		ScrubRequests []map[string]any `json:"scrub_requests"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.ScrubRequests) != 1 {
		t.Fatalf("expected 1 scrub request, got %d", len(resp.ScrubRequests))
	}
	entry := resp.ScrubRequests[0]
	if entry["resolved_mode"] != string(model.ScrubModeHuman) {
		t.Fatalf("expected resolved_mode=human (auto rule over max_per_day cap, fell through), got %v", entry["resolved_mode"])
	}
	if entry["can_approve"] != true || entry["can_reject"] != true {
		t.Fatalf("expected the fallback human rule's approver to see can_approve/can_reject=true, got %+v", entry)
	}
}
