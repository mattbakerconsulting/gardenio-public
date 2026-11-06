package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// Tests for the grant-request flow ADR docs/adr/taints.md §9.2/§9.4 adds on
// top of the taint write gate (gd-KT7mKtvYwt): the structured AuthzDenial
// body a taint-gated write returns, filing a `grant` escalation either via
// the X-Escalate retry or `kd taint grant-request` directly, and approving
// that escalation to write a taint_grants row.

// --- structured denial body ---------------------------------------------

// TestBuildTaintDenialAdvisoryText_ExactShape pins the exact ADR §9.4
// worked-example wording for the single-taint/single-team case.
func TestBuildTaintDenialAdvisoryText_ExactShape(t *testing.T) {
	got := buildTaintDenialAdvisoryText("update", "gd-abc", []string{"acme-production"}, []string{"acme-prod-owners"})
	want := `Write denied: update on gd-abc carries taint "acme-production" that you do not hold.
Ask team acme-prod-owners for a grant, or retry with --escalate to file a grant request.`
	if got != want {
		t.Errorf("advisory text mismatch:\ngot:  %q\nwant: %q", got, want)
	}
}

// TestHandleUpdateBead_TaintDenial_ResponseBody drives a real taint-gated
// write denial through the HTTP handler and asserts the response body
// matches the AuthzDenial shape: denied_reason=taint, taint_names,
// taint_owner_teams, and the advisory_text template, not a generic 403.
func TestHandleUpdateBead_TaintDenial_ResponseBody(t *testing.T) {
	_, ms, h := newTestServer()
	bead := &model.Bead{ID: "gd-tainted", Kind: model.KindIssue, Type: model.TypeTask, Title: "t", Status: model.StatusOpen}
	ms.beads["gd-tainted"] = bead
	seedTaint(ms, 1, "acme-production", "acme-prod-owners")
	if _, err := ms.ApplyTaint(context.Background(), &model.BeadTaint{
		BeadID: "gd-tainted", TaintID: 1, Source: model.TaintSourceExplicit, AppliedBy: "9",
	}); err != nil {
		t.Fatalf("apply taint: %v", err)
	}

	body, _ := json.Marshal(map[string]any{"title": "changed"})
	req := httptest.NewRequest(http.MethodPatch, "/v1/beads/gd-tainted", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer outsider@example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d; body: %s", rec.Code, rec.Body.String())
	}

	var denial AuthzDenial
	if err := json.Unmarshal(rec.Body.Bytes(), &denial); err != nil {
		t.Fatalf("unmarshal AuthzDenial: %v; body: %s", err, rec.Body.String())
	}
	if denial.DeniedReason != reasonTaint {
		t.Errorf("denied_reason = %q; want %q", denial.DeniedReason, reasonTaint)
	}
	if len(denial.TaintNames) != 1 || denial.TaintNames[0] != "acme-production" {
		t.Errorf("taint_names = %v; want [acme-production]", denial.TaintNames)
	}
	if len(denial.TaintOwnerTeams) != 1 || denial.TaintOwnerTeams[0] != "acme-prod-owners" {
		t.Errorf("taint_owner_teams = %v; want [acme-prod-owners]", denial.TaintOwnerTeams)
	}
	wantAdvisory := `Write denied: update on gd-tainted carries taint "acme-production" that you do not hold.
Ask team acme-prod-owners for a grant, or retry with --escalate to file a grant request.`
	if denial.AdvisoryText != wantAdvisory {
		t.Errorf("advisory_text = %q; want %q", denial.AdvisoryText, wantAdvisory)
	}
	if !denial.Escalatable {
		t.Error("expected Escalatable=true so the client knows --escalate is available")
	}
}

// --- X-Escalate on a taint-denied write ----------------------------------

// TestHandleUpdateBead_TaintDenial_XEscalate_CreatesGrantEscalation proves a
// taint-denied write retried with X-Escalate: true creates a `grant`
// escalation addressed to the taint's actual owner team, not a generic admin
// queue escalation (which would carry operation_type "update", not "grant").
func TestHandleUpdateBead_TaintDenial_XEscalate_CreatesGrantEscalation(t *testing.T) {
	_, ms, h := newTestServer()
	bead := &model.Bead{ID: "gd-tainted", Kind: model.KindIssue, Type: model.TypeTask, Title: "t", Status: model.StatusOpen}
	ms.beads["gd-tainted"] = bead
	seedTaint(ms, 1, "acme-production", "acme-prod-owners")
	ms.operatorAliases = map[string]int64{"oidc\x00requester@example.com": 42}
	if _, err := ms.ApplyTaint(context.Background(), &model.BeadTaint{
		BeadID: "gd-tainted", TaintID: 1, Source: model.TaintSourceExplicit, AppliedBy: "9",
	}); err != nil {
		t.Fatalf("apply taint: %v", err)
	}

	body, _ := json.Marshal(map[string]any{"title": "changed"})
	req := httptest.NewRequest(http.MethodPatch, "/v1/beads/gd-tainted", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer requester@example.com")
	req.Header.Set("X-Escalate", "true")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var resp EscalationResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.EscalationID == "" {
		t.Fatal("expected an escalation_id")
	}

	esc, ok := ms.beads[resp.EscalationID]
	if !ok {
		t.Fatalf("escalation bead %s not found", resp.EscalationID)
	}
	var fields struct {
		OperationType   string   `json:"operation_type"`
		TaintOwnerTeams []string `json:"taint_owner_teams"`
		TaintNames      []string `json:"taint_names"`
	}
	if err := json.Unmarshal(esc.Fields, &fields); err != nil {
		t.Fatalf("unmarshal escalation fields: %v", err)
	}
	if fields.OperationType != "grant" {
		t.Errorf("operation_type = %q; want %q (not a generic write-denial escalation)", fields.OperationType, "grant")
	}
	if len(fields.TaintOwnerTeams) != 1 || fields.TaintOwnerTeams[0] != "acme-prod-owners" {
		t.Errorf("taint_owner_teams = %v; want [acme-prod-owners] (the taint's real owner, not an admin queue)", fields.TaintOwnerTeams)
	}
	if len(fields.TaintNames) != 1 || fields.TaintNames[0] != "acme-production" {
		t.Errorf("taint_names = %v; want [acme-production]", fields.TaintNames)
	}

	// The bead itself must still be unchanged — X-Escalate files a request,
	// it does not perform the write.
	if bead.Title == "changed" {
		t.Error("X-Escalate must not perform the originally denied write")
	}
}

// --- kd taint grant-request (direct, no --owner) --------------------------

// TestHandleGrantTaintRequest_NoOwnerNeeded proves POST
// /v1/taints/{name}/grant-requests resolves the owner team itself, so a
// caller need not (and cannot) pass one — the same escalation shape as the
// X-Escalate path above.
func TestHandleGrantTaintRequest_NoOwnerNeeded(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedTaint(ms, 1, "acme-production", "acme-prod-owners")
	ms.operatorAliases = map[string]int64{"oidc\x00requester@example.com": 42}

	body, _ := json.Marshal(map[string]any{"reason": "need to patch prod config"})
	req := httptest.NewRequest(http.MethodPost, "/v1/taints/acme-production/grant-requests", bytes.NewReader(body))
	req.SetPathValue("name", "acme-production")
	req = req.WithContext(asOIDC(req.Context(), "requester@example.com"))
	rec := httptest.NewRecorder()

	s.handleGrantTaintRequest(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var resp EscalationResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	esc := ms.beads[resp.EscalationID]
	if esc == nil {
		t.Fatalf("escalation bead %s not found", resp.EscalationID)
	}
	var fields struct {
		OperationType   string   `json:"operation_type"`
		TaintOwnerTeams []string `json:"taint_owner_teams"`
		Reason          string   `json:"reason"`
	}
	if err := json.Unmarshal(esc.Fields, &fields); err != nil {
		t.Fatalf("unmarshal escalation fields: %v", err)
	}
	if fields.OperationType != "grant" {
		t.Errorf("operation_type = %q; want %q", fields.OperationType, "grant")
	}
	if len(fields.TaintOwnerTeams) != 1 || fields.TaintOwnerTeams[0] != "acme-prod-owners" {
		t.Errorf("taint_owner_teams = %v; want [acme-prod-owners] — resolved without the caller supplying --owner", fields.TaintOwnerTeams)
	}
	if fields.Reason != "need to patch prod config" {
		t.Errorf("reason = %q; want the caller's --reason text preserved", fields.Reason)
	}
}

// --- approving a grant escalation -----------------------------------------

// TestReplayEscalation_Grant_DefaultsToSevenDays proves approving a `grant`
// escalation with no approver-chosen expiry writes a taint_grants row whose
// granted_by is the APPROVER and whose expires_at defaults to ~7 days out
// (ADR §10.4).
func TestReplayEscalation_Grant_DefaultsToSevenDays(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedTaint(ms, 1, "acme-production", "acme-prod-owners")
	seedGrantApproverTeam(ms)
	ms.operatorAliases = map[string]int64{"oidc\x00approver@example.com": 7}

	fields, _ := json.Marshal(map[string]any{
		"operation_type": "grant",
		"operation_spec": map[string]any{
			"taint_names":    []string{"acme-production"},
			"principal_kind": model.PrincipalAgent,
			"principal":      "gd-requester",
		},
		"requesting_agent":          "gd-requester",
		"requesting_agent_verified": "true",
	})
	escalation := &model.Bead{ID: "gd-esc-grant-1", Fields: fields}
	ms.beads[escalation.ID] = escalation // ClaimEscalationReplay needs the bead in the store, as in production

	approverCtx := asOIDC(context.Background(), "approver@example.com")
	before := time.Now()
	result := s.replayEscalation(approverCtx, escalation, "approver@example.com", nil)
	after := time.Now()

	if result == nil || result["status"] != "success" {
		t.Fatalf("expected successful replay, got %v", result)
	}

	if len(ms.taintGrants) != 1 {
		t.Fatalf("expected exactly one taint_grants row, got %d", len(ms.taintGrants))
	}
	grant := firstTaintGrant(ms)
	if grant.GrantedBy != 7 {
		t.Errorf("granted_by = %d; want 7 (the approver, not the requester)", grant.GrantedBy)
	}
	if grant.PrincipalKind != model.PrincipalAgent || grant.Principal != "gd-requester" {
		t.Errorf("principal = %s:%s; want %s:gd-requester", grant.PrincipalKind, grant.Principal, model.PrincipalAgent)
	}
	if grant.ExpiresAt == nil {
		t.Fatal("expected non-nil expires_at")
	}
	wantMin := before.Add(7 * 24 * time.Hour)
	wantMax := after.Add(7 * 24 * time.Hour)
	if grant.ExpiresAt.Before(wantMin) || grant.ExpiresAt.After(wantMax) {
		t.Errorf("expires_at = %v; want between %v and %v (7-day default)", grant.ExpiresAt, wantMin, wantMax)
	}
}

// TestReplayEscalation_Grant_HonorsApproverChosenExpiry proves an
// approver-supplied grant_until overrides the 7-day default.
func TestReplayEscalation_Grant_HonorsApproverChosenExpiry(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedTaint(ms, 1, "acme-production", "acme-prod-owners")
	seedGrantApproverTeam(ms)
	ms.operatorAliases = map[string]int64{"oidc\x00approver@example.com": 7, "oidc\x00requester@example.com": 9}

	fields, _ := json.Marshal(map[string]any{
		"operation_type": "grant",
		"operation_spec": map[string]any{
			"taint_names":    []string{"acme-production"},
			"principal_kind": model.PrincipalOperator,
			"principal":      "9",
		},
		"requesting_agent": "requester@example.com",
	})
	escalation := &model.Bead{ID: "gd-esc-grant-2", Fields: fields}
	ms.beads[escalation.ID] = escalation // ClaimEscalationReplay needs the bead in the store, as in production

	chosen := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	approverCtx := asOIDC(context.Background(), "approver@example.com")
	result := s.replayEscalation(approverCtx, escalation, "approver@example.com", &chosen)

	if result == nil || result["status"] != "success" {
		t.Fatalf("expected successful replay, got %v", result)
	}
	if len(ms.taintGrants) != 1 {
		t.Fatalf("expected exactly one taint_grants row, got %d", len(ms.taintGrants))
	}
	grant := firstTaintGrant(ms)
	if grant.ExpiresAt == nil || !grant.ExpiresAt.Equal(chosen) {
		t.Errorf("expires_at = %v; want %v (approver-chosen)", grant.ExpiresAt, chosen)
	}
}

// firstTaintGrant returns the sole entry of a mockStore's taintGrants map,
// for tests that have already asserted len == 1 and just want the value.
func firstTaintGrant(ms *mockStore) *model.TaintGrant {
	for _, g := range ms.taintGrants {
		return g
	}
	return nil
}

// TestReplayEscalation_Grant_ResultIncludesGrantedTaints proves the replay
// result surfaces the granted taint names (merged into the escalation's
// persisted result, mirroring how a create/update replay surfaces bead_id).
func TestReplayEscalation_Grant_ResultIncludesGrantedTaints(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedTaint(ms, 1, "acme-production", "acme-prod-owners")
	seedGrantApproverTeam(ms)
	ms.operatorAliases = map[string]int64{"oidc\x00approver@example.com": 7}

	fields, _ := json.Marshal(map[string]any{
		"operation_type": "grant",
		"operation_spec": map[string]any{
			"taint_names":    []string{"acme-production"},
			"principal_kind": model.PrincipalAgent,
			"principal":      "gd-requester",
		},
		"requesting_agent":          "gd-requester",
		"requesting_agent_verified": "true",
	})
	escalation := &model.Bead{ID: "gd-esc-grant-3", Fields: fields}
	ms.beads[escalation.ID] = escalation // ClaimEscalationReplay needs the bead in the store, as in production

	approverCtx := asOIDC(context.Background(), "approver@example.com")
	result := s.replayEscalation(approverCtx, escalation, "approver@example.com", nil)

	granted, _ := result["granted_taints"].([]string)
	if len(granted) != 1 || granted[0] != "acme-production" {
		t.Errorf("granted_taints = %v; want [acme-production]", granted)
	}
}

// TestReplayEscalation_Grant_MissingSpec_Errors proves a malformed/missing
// operation_spec fails the replay cleanly rather than writing a partial or
// wrongly-scoped grant.
func TestReplayEscalation_Grant_MissingSpec_Errors(t *testing.T) {
	s, ms := newTaintGrantsTestServer()

	fields, _ := json.Marshal(map[string]any{
		"operation_type":            "grant",
		"operation_spec":            map[string]any{},
		"requesting_agent":          "gd-requester",
		"requesting_agent_verified": "true",
	})
	escalation := &model.Bead{ID: "gd-esc-grant-bad", Fields: fields}
	ms.beads[escalation.ID] = escalation // ClaimEscalationReplay needs the bead in the store, as in production

	approverCtx := asOIDC(context.Background(), "approver@example.com")
	result := s.replayEscalation(approverCtx, escalation, "approver@example.com", nil)

	if result == nil || result["status"] == "success" {
		t.Fatalf("expected a failed replay for an incomplete grant spec, got %v", result)
	}
	if len(ms.taintGrants) != 0 {
		t.Fatalf("expected no taint_grants row written, got %v", ms.taintGrants)
	}
}

// seedGrantApproverTeam makes approver@example.com a member of
// acme-prod-owners, the owner team the grant replay tests seed their taint
// with.
func seedGrantApproverTeam(ms *mockStore) {
	seedTeamBead(ms, "acme-prod-owners", `{"team":"owner","members":["oidc:approver@example.com"]}`)
}

// replayGrantFor replays a grant escalation for acme-production to
// kind:principal, filed by the verified requesting agent gd-requester and
// approved under approverCtx.
func replayGrantFor(t *testing.T, s *BeadsServer, ms *mockStore, approverCtx context.Context, kind, principal string) map[string]any {
	t.Helper()
	fields, _ := json.Marshal(map[string]any{
		"operation_type": "grant",
		"operation_spec": map[string]any{
			"taint_names":    []string{"acme-production"},
			"principal_kind": kind,
			"principal":      principal,
		},
		"requesting_agent":          "gd-requester",
		"requesting_agent_verified": "true",
	})
	escalation := &model.Bead{ID: "gd-esc-grant-" + kind, Fields: fields}
	ms.beads[escalation.ID] = escalation
	return s.replayEscalation(approverCtx, escalation, "approver@example.com", nil)
}

// TestReplayEscalation_Grant_ApproverOffOwnerTeam_Refused proves gd-URd8GBIXvL:
// approving a grant escalation needs the rule the direct grant route applies
// (owner team or admin), so a human who may not grant the taint directly
// cannot grant it by approving a card.
func TestReplayEscalation_Grant_ApproverOffOwnerTeam_Refused(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedTaint(ms, 1, "acme-production", "acme-prod-owners")
	seedTeamBead(ms, "acme-prod-owners", `{"team":"owner","members":["oidc:someone-else@example.com"]}`)
	ms.operatorAliases = map[string]int64{"oidc\x00approver@example.com": 7}

	result := replayGrantFor(t, s, ms, asOIDC(context.Background(), "approver@example.com"), model.PrincipalAgent, "gd-requester")

	if result == nil || result["status"] == "success" {
		t.Fatalf("expected a refused replay for an approver off the owner team, got %v", result)
	}
	if len(ms.taintGrants) != 0 {
		t.Fatalf("expected no taint_grants row, got %v", ms.taintGrants)
	}
}

// TestReplayEscalation_Grant_AdminApprover_Allowed is the control for the
// owner-team check: an admin off the owner team may still approve.
func TestReplayEscalation_Grant_AdminApprover_Allowed(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedTaint(ms, 1, "acme-production", "acme-prod-owners")
	ms.operatorAliases = map[string]int64{"oidc\x00approver@example.com": 7}
	approverCtx := WithRoles(asOIDC(context.Background(), "approver@example.com"), []string{RoleAdmin})

	result := replayGrantFor(t, s, ms, approverCtx, model.PrincipalAgent, "gd-requester")

	if result == nil || result["status"] != "success" {
		t.Fatalf("expected an admin's approval to replay, got %v", result)
	}
	if len(ms.taintGrants) != 1 {
		t.Fatalf("expected one taint_grants row, got %d", len(ms.taintGrants))
	}
}

// TestReplayEscalation_Grant_PrincipalNotRequester_Refused proves the replay
// grants only to the requester: a team principal, or an agent other than the
// verified requesting agent, is refused even when the approver is on the
// owner team (gd-URd8GBIXvL).
func TestReplayEscalation_Grant_PrincipalNotRequester_Refused(t *testing.T) {
	for _, tc := range []struct{ kind, principal string }{
		{model.PrincipalTeam, "everyone"},
		{model.PrincipalAgent, "gd-someone-else"},
		{model.PrincipalFleet, "gardenio"},
		{model.PrincipalOperator, "9"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			s, ms := newTaintGrantsTestServer()
			seedTaint(ms, 1, "acme-production", "acme-prod-owners")
			seedGrantApproverTeam(ms)
			ms.operatorAliases = map[string]int64{"oidc\x00approver@example.com": 7}

			result := replayGrantFor(t, s, ms, asOIDC(context.Background(), "approver@example.com"), tc.kind, tc.principal)

			if result == nil || result["status"] == "success" {
				t.Fatalf("expected a refused replay for principal %s:%s, got %v", tc.kind, tc.principal, result)
			}
			if len(ms.taintGrants) != 0 {
				t.Fatalf("expected no taint_grants row, got %v", ms.taintGrants)
			}
		})
	}
}

// TestEscalationCreate_GrantPrincipalPinnedToFiler proves gd-URd8GBIXvL's
// filing half: an agent filing a grant escalation directly may name only
// itself as the principal, never its fleet or another agent.
func TestEscalationCreate_GrantPrincipalPinnedToFiler(t *testing.T) {
	for _, tc := range []struct {
		name, kind, principal string
		want                  int
	}{
		{"fleet principal refused", model.PrincipalFleet, "gardenio", http.StatusBadRequest},
		{"other agent refused", model.PrincipalAgent, "gd-other-agent", http.StatusBadRequest},
		{"own agent bead allowed", model.PrincipalAgent, "gd-filer-agent", http.StatusCreated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ms := newMockStore()
			s := NewBeadsServer(ms, &events.NoopPublisher{})
			h := xactorAgentHandler(s, "primary-token", nil)
			putAgentBead(ms, "gd-filer-agent", "gd-filer-agent", model.StatusInProgress, time.Now())

			body, _ := json.Marshal(map[string]any{
				"title": "Grant request: untrusted-input",
				"type":  "escalation",
				"kind":  "data",
				"fields": map[string]any{
					"operation_type": "grant",
					"operation_spec": map[string]any{
						"taint_names":    []string{"untrusted-input"},
						"principal_kind": tc.kind,
						"principal":      tc.principal,
					},
				},
			})
			req := httptest.NewRequest(http.MethodPost, "/v1/beads", bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer primary-token")
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Actor", "gd-filer-agent")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d; body: %s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}
