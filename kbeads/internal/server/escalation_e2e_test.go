package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// escOIDCMock simulates an OIDC issuer for escalation tests. Implements
// TokenVerifier, IdentityVerifier, and RoleExtractor.
type escOIDCMock struct {
	tokens map[string]escTokenInfo
}

type escTokenInfo struct {
	email string
	roles []string
}

func (m *escOIDCMock) Verify(_ context.Context, rawToken string) error {
	if _, ok := m.tokens[rawToken]; ok {
		return nil
	}
	return fmt.Errorf("unknown token")
}

func (m *escOIDCMock) VerifyAndIdentify(_ context.Context, rawToken string) (string, error) {
	if info, ok := m.tokens[rawToken]; ok {
		return info.email, nil
	}
	return "", fmt.Errorf("unknown token")
}

func (m *escOIDCMock) ExtractRoles(_ context.Context, rawToken string) ([]string, error) {
	if info, ok := m.tokens[rawToken]; ok {
		return info.roles, nil
	}
	return nil, fmt.Errorf("unknown token")
}

func (m *escOIDCMock) ExtractClientID(_ context.Context, rawToken string) (string, error) {
	if _, ok := m.tokens[rawToken]; ok {
		return testBrowserClient, nil
	}
	return "", fmt.Errorf("unknown token")
}

// ExtractAudiences implements AudienceExtractor: no token here is minted for
// a land (land_audience.go), as for a real Keycloak verifier.
func (m *escOIDCMock) ExtractAudiences(_ context.Context, rawToken string) ([]string, error) {
	if _, ok := m.tokens[rawToken]; ok {
		return nil, nil
	}
	return nil, fmt.Errorf("unknown token")
}

// newEscalationE2EServer creates a full-stack test server with OIDC mock
// and AuthMiddleware. Requests go through: AuthMiddleware → UserProvisioner →
// requireRole → handler, exercising the complete write-authz + escalation pipeline.
func newEscalationE2EServer(t *testing.T, adminToken string, fc *FleetConfig, verifier TokenVerifier) (*httptest.Server, *BeadsServer, *mockStore) {
	t.Helper()
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	srv.SetFleetAuth(fc)
	srv.SetForwardAuthClients([]string{testBrowserClient})
	handler := srv.NewHTTPHandler(verifier, withTestToken(fc, adminToken))
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return ts, srv, ms
}

// escRequest issues an HTTP request with optional JSON body, Bearer token,
// and extra headers against an httptest.Server.
func escRequest(t *testing.T, ts *httptest.Server, method, path, token string, body any, headers map[string]string) *http.Response {
	t.Helper()
	var bodyReader io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		bodyReader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, ts.URL+path, bodyReader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

// escDecode reads and decodes an HTTP response body.
func escDecode(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}

// TestEscalationOnDenyE2E is the full integration test for the escalation-on-403
// pipeline flowing through AuthMiddleware. Covers: OIDC user denied by project-scoped
// write policy → 403 → X-Escalate → 202 → admin approve → auto-replay creates bead
// → escalation auto-closed.
func TestEscalationOnDenyE2E(t *testing.T) {
	const (
		adminToken   = "admin-esc-e2e"
		operatorTok  = "oidc-operator-esc"
		adminOIDCTok = "oidc-admin-esc"
		fleetName    = "esc-fleet"
	)

	oidcMock := &escOIDCMock{
		tokens: map[string]escTokenInfo{
			operatorTok:  {email: "alice@example.com", roles: []string{RoleOperator}},
			adminOIDCTok: {email: "admin@example.com", roles: []string{RoleOperator}},
		},
	}

	fc := NewFleetConfig(fleetName, nil)
	ts, _, ms := newEscalationE2EServer(t, adminToken, fc, oidcMock)

	// Create a user bead for alice with authorized_projects = "monorepo" only.
	// She does NOT have access to "gardenio".
	makeUserBead(ms, "gd-alice", "alice@example.com", "monorepo", "crew")

	// ------------------------------------------------------------------
	// Phase 1: OIDC operator creates agent bead in project:gardenio → 403
	// The agent type has write_policy: {project_scoped: true, escalatable: true}.
	// alice only has access to "monorepo", so "gardenio" is denied.
	// ------------------------------------------------------------------
	t.Run("Phase1_DenyWithoutEscalate", func(t *testing.T) {
		resp := escRequest(t, ts, "POST", "/v1/beads", operatorTok, map[string]any{
			"title":  "Deploy agent",
			"type":   "agent",
			"labels": []string{"project:gardenio"},
			"fields": map[string]any{
				"agent":   "test-agent",
				"role":    "crew",
				"project": "gardenio",
			},
		}, nil)
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusForbidden {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 403, got %d: %s", resp.StatusCode, body)
		}

		var denial AuthzDenial
		escDecode(t, resp, &denial)

		if denial.Action != "create" {
			t.Errorf("denial.Action=%q, want create", denial.Action)
		}
		if denial.BeadType != "agent" {
			t.Errorf("denial.BeadType=%q, want agent", denial.BeadType)
		}
		if !denial.Escalatable {
			t.Error("agent type should be escalatable")
		}
		if denial.AdvisoryText == "" {
			t.Error("denial should include advisory text")
		}
		if !strings.Contains(denial.AdvisoryText, "--escalate") {
			t.Errorf("escalatable denial should mention --escalate, got: %s", denial.AdvisoryText)
		}
		if denial.RequiredScope != "gardenio" {
			t.Errorf("denial.RequiredScope=%q, want gardenio", denial.RequiredScope)
		}
	})

	// ------------------------------------------------------------------
	// Phase 2: Same request with X-Escalate: true → 202 with escalation bead
	// ------------------------------------------------------------------
	var escalationID string
	t.Run("Phase2_EscalateCreatesEscalationBead", func(t *testing.T) {
		resp := escRequest(t, ts, "POST", "/v1/beads", operatorTok, map[string]any{
			"title":  "Deploy agent",
			"type":   "agent",
			"labels": []string{"project:gardenio"},
			"fields": map[string]any{
				"agent":   "test-agent",
				"role":    "crew",
				"project": "gardenio",
			},
		}, map[string]string{"X-Escalate": "true"})
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusAccepted {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 202, got %d: %s", resp.StatusCode, body)
		}

		var escResp EscalationResponse
		escDecode(t, resp, &escResp)

		if escResp.EscalationID == "" {
			t.Fatal("expected non-empty escalation_id")
		}
		if escResp.DeniedAction != "create" {
			t.Errorf("denied_action=%q, want create", escResp.DeniedAction)
		}
		if escResp.BeadType != "agent" {
			t.Errorf("bead_type=%q, want agent", escResp.BeadType)
		}
		if !strings.Contains(escResp.Message, "escalation") {
			t.Errorf("message should mention escalation, got: %s", escResp.Message)
		}

		escalationID = escResp.EscalationID

		// Verify the escalation bead was stored correctly.
		escBead, err := ms.GetBead(context.Background(), escalationID)
		if err != nil || escBead == nil {
			t.Fatalf("escalation bead not found: %v", err)
		}
		if escBead.Type != model.TypeEscalation {
			t.Errorf("escalation bead type=%q, want escalation", escBead.Type)
		}
		if escBead.Status != model.StatusOpen {
			t.Errorf("escalation status=%q, want open", escBead.Status)
		}

		// Verify escalation fields contain operation_spec.
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(escBead.Fields, &fields); err != nil {
			t.Fatalf("unmarshal escalation fields: %v", err)
		}
		if _, ok := fields["operation_type"]; !ok {
			t.Error("escalation fields missing operation_type")
		}
		if _, ok := fields["operation_spec"]; !ok {
			t.Error("escalation fields missing operation_spec")
		}
		var opType string
		json.Unmarshal(fields["operation_type"], &opType)
		if opType != "create" {
			t.Errorf("operation_type=%q, want create", opType)
		}
	})

	// ------------------------------------------------------------------
	// Phase 3: Admin approves the escalation → auto-replay creates agent bead
	// ------------------------------------------------------------------
	t.Run("Phase3_ApproveAndReplay", func(t *testing.T) {
		if escalationID == "" {
			t.Skip("no escalation ID from Phase 2")
		}

		// Approval requires a genuine OIDC-derived identity (gd-731gBeGgl7):
		// gd-ZjiSd9Kzio round 3 removed the X-Operated-By relay over the
		// static admin token that beads-viewer used to rely on (any holder
		// of the shared static token could forge the header), so the human
		// admin now authenticates with their own OIDC token directly.
		// adminOIDCTok holds gardenio-operator, not admin: approve the
		// escalation as one filed before routing (unrouteEscalation).
		unrouteEscalation(t, ms, escalationID)
		resp := escRequest(t, ts, "POST", "/v1/escalations/"+escalationID+"/approve", adminOIDCTok, map[string]any{
			"approved_by":     "admin@example.com",
			"approval_method": "api_token",
		}, nil)
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
		}

		var approveResp map[string]any
		escDecode(t, resp, &approveResp)

		// Verify replay result.
		replay, ok := approveResp["replay"].(map[string]any)
		if !ok {
			t.Fatalf("expected replay field in response, got: %v", approveResp)
		}
		if replay["status"] != "success" {
			t.Errorf("replay status=%v, want success", replay["status"])
		}
		if replay["replayed"] != true {
			t.Error("replay.replayed should be true")
		}

		// The replayed operation should have created a new agent bead.
		replayedBeadID, _ := replay["bead_id"].(string)
		if replayedBeadID == "" {
			t.Fatal("replay should return bead_id of the created bead")
		}

		// Verify the agent bead exists and is correct.
		agentBead, err := ms.GetBead(context.Background(), replayedBeadID)
		if err != nil || agentBead == nil {
			t.Fatalf("replayed agent bead not found: %v", err)
		}
		if agentBead.Type != "agent" {
			t.Errorf("replayed bead type=%q, want agent", agentBead.Type)
		}
		if agentBead.Title != "Deploy agent" {
			t.Errorf("replayed bead title=%q, want 'Deploy agent'", agentBead.Title)
		}

		// Verify escalation was auto-closed on success.
		escBead, err := ms.GetBead(context.Background(), escalationID)
		if err != nil {
			t.Fatalf("get escalation bead: %v", err)
		}
		if escBead.Status != model.StatusClosed {
			t.Errorf("escalation status=%q, want closed (auto-closed on replay success)", escBead.Status)
		}

		// Verify approval fields were recorded with the server-derived
		// approver (from the caller's own OIDC identity), not the spoofable
		// body value.
		var fields map[string]json.RawMessage
		json.Unmarshal(escBead.Fields, &fields)
		var approvedBy string
		json.Unmarshal(fields["approved_by"], &approvedBy)
		if approvedBy != "admin@example.com" {
			t.Errorf("approved_by=%q, want admin@example.com", approvedBy)
		}
		var resultStatus string
		json.Unmarshal(fields["result_status"], &resultStatus)
		if resultStatus != "success" {
			t.Errorf("result_status=%q, want success", resultStatus)
		}
	})

	// ------------------------------------------------------------------
	// Phase 4: X-Escalate without escalatable type → plain 403
	// The user type has min_role: admin, NOT escalatable.
	// ------------------------------------------------------------------
	t.Run("Phase4_NonEscalatableTypeGets403", func(t *testing.T) {
		resp := escRequest(t, ts, "POST", "/v1/beads", operatorTok, map[string]any{
			"title":      "Create user",
			"type":       "user",
			"created_by": "alice@example.com",
		}, map[string]string{"X-Escalate": "true"})
		defer resp.Body.Close()

		// User type has min_role=admin. Operator should get 403.
		// The type is NOT escalatable, so X-Escalate header is ignored.
		if resp.StatusCode != http.StatusForbidden {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 403 for non-escalatable type, got %d: %s", resp.StatusCode, body)
		}

		var denial AuthzDenial
		escDecode(t, resp, &denial)
		if denial.Escalatable {
			t.Error("user type should not be escalatable")
		}
	})
}

// TestEscalationRejectE2E tests the rejection path: create escalation → reject →
// escalation closed with result_status=failure.
func TestEscalationRejectE2E(t *testing.T) {
	const (
		adminToken   = "admin-reject-e2e"
		operatorTok  = "oidc-reject-op"
		oidcAdminTok = "oidc-reject-op-admin"
		fleetName    = "reject-fleet"
	)

	oidcMock := &escOIDCMock{
		tokens: map[string]escTokenInfo{
			operatorTok:  {email: "bob@example.com", roles: []string{RoleOperator}},
			oidcAdminTok: {email: "owner@example.com", roles: []string{RoleAdmin}},
		},
	}

	fc := NewFleetConfig(fleetName, nil)
	ts, _, ms := newEscalationE2EServer(t, adminToken, fc, oidcMock)
	makeUserBead(ms, "gd-bob", "bob@example.com", "monorepo", "crew")

	// Create escalation via X-Escalate.
	resp := escRequest(t, ts, "POST", "/v1/beads", operatorTok, map[string]any{
		"title":  "Deploy agent rejected",
		"type":   "agent",
		"labels": []string{"project:gardenio"},
		"fields": map[string]any{
			"agent":   "test-agent",
			"role":    "crew",
			"project": "gardenio",
		},
	}, map[string]string{"X-Escalate": "true"})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 202, got %d: %s", resp.StatusCode, body)
	}

	var escResp EscalationResponse
	escDecode(t, resp, &escResp)
	escalationID := escResp.EscalationID

	// Reject the escalation. P7 Task 9: reject needs an operator identity, so
	// an OIDC admin rejects, not the static token.
	rejectResp := escRequest(t, ts, "POST", "/v1/escalations/"+escalationID+"/reject", oidcAdminTok, map[string]any{
		"rejected_by": "admin@example.com",
		"reason":      "not authorized for gardenio project",
	}, nil)
	defer rejectResp.Body.Close()

	if rejectResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(rejectResp.Body)
		t.Fatalf("expected 200 on reject, got %d: %s", rejectResp.StatusCode, body)
	}

	// Verify escalation is closed with failure status.
	escBead, err := ms.GetBead(context.Background(), escalationID)
	if err != nil {
		t.Fatalf("get escalation: %v", err)
	}
	if escBead.Status != model.StatusClosed {
		t.Errorf("escalation status=%q, want closed", escBead.Status)
	}

	var fields map[string]json.RawMessage
	json.Unmarshal(escBead.Fields, &fields)
	var resultStatus string
	json.Unmarshal(fields["result_status"], &resultStatus)
	if resultStatus != "failure" {
		t.Errorf("result_status=%q, want failure", resultStatus)
	}
}

// TestEscalationListAndGetE2E tests that escalation beads created through the
// authz pipeline are visible via the escalation list/get endpoints.
func TestEscalationListAndGetE2E(t *testing.T) {
	const (
		adminToken  = "admin-list-e2e"
		operatorTok = "oidc-list-op"
		fleetName   = "list-fleet"
	)

	oidcMock := &escOIDCMock{
		tokens: map[string]escTokenInfo{
			operatorTok: {email: "carol@example.com", roles: []string{RoleOperator}},
		},
	}

	fc := NewFleetConfig(fleetName, nil)
	ts, _, ms := newEscalationE2EServer(t, adminToken, fc, oidcMock)
	makeUserBead(ms, "gd-carol", "carol@example.com", "monorepo", "crew")

	// Create escalation.
	resp := escRequest(t, ts, "POST", "/v1/beads", operatorTok, map[string]any{
		"title":  "Deploy listing test",
		"type":   "agent",
		"labels": []string{"project:gardenio"},
		"fields": map[string]any{
			"agent":   "list-agent",
			"role":    "crew",
			"project": "gardenio",
		},
	}, map[string]string{"X-Escalate": "true"})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 202, got %d: %s", resp.StatusCode, body)
	}

	var escResp EscalationResponse
	escDecode(t, resp, &escResp)
	escalationID := escResp.EscalationID

	// GET /v1/escalations/{id} — should find the escalation.
	t.Run("GetEscalation", func(t *testing.T) {
		getResp := escRequest(t, ts, "GET", "/v1/escalations/"+escalationID, adminToken, nil, nil)
		defer getResp.Body.Close()

		if getResp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(getResp.Body)
			t.Fatalf("expected 200, got %d: %s", getResp.StatusCode, body)
		}

		var result map[string]any
		escDecode(t, getResp, &result)
		esc, _ := result["escalation"].(map[string]any)
		if esc["operation_type"] != "create" {
			t.Errorf("escalation operation_type=%v, want create", esc["operation_type"])
		}
		if esc["requesting_agent"] != "alice@example.com" && esc["requesting_agent"] != "carol@example.com" {
			// requesting_agent comes from the OIDC identity subject
			t.Logf("requesting_agent=%v (from OIDC identity)", esc["requesting_agent"])
		}
	})

	// GET /v1/escalations — should include the escalation in the list.
	t.Run("ListEscalations", func(t *testing.T) {
		listResp := escRequest(t, ts, "GET", "/v1/escalations", adminToken, nil, nil)
		defer listResp.Body.Close()

		if listResp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(listResp.Body)
			t.Fatalf("expected 200, got %d: %s", listResp.StatusCode, body)
		}

		var result struct {
			Escalations []map[string]any `json:"escalations"`
		}
		escDecode(t, listResp, &result)

		found := false
		for _, esc := range result.Escalations {
			issue, _ := esc["issue"].(map[string]any)
			if issue["id"] == escalationID {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("escalation %s not found in list response", escalationID)
		}
	})
}

// TestEscalationCaptureResultE2E tests the external result capture path.
func TestEscalationCaptureResultE2E(t *testing.T) {
	const (
		adminToken   = "admin-capture-e2e"
		operatorTok  = "oidc-capture-op"
		oidcAdminTok = "oidc-capture-op-admin"
		fleetName    = "capture-fleet"
	)

	oidcMock := &escOIDCMock{
		tokens: map[string]escTokenInfo{
			operatorTok:  {email: "dave@example.com", roles: []string{RoleOperator}},
			oidcAdminTok: {email: "owner@example.com", roles: []string{RoleAdmin}},
		},
	}

	fc := NewFleetConfig(fleetName, nil)
	ts, _, ms := newEscalationE2EServer(t, adminToken, fc, oidcMock)
	makeUserBead(ms, "gd-dave", "dave@example.com", "monorepo", "crew")

	// Create escalation.
	resp := escRequest(t, ts, "POST", "/v1/beads", operatorTok, map[string]any{
		"title":  "Deploy capture test",
		"type":   "agent",
		"labels": []string{"project:gardenio"},
		"fields": map[string]any{
			"agent":   "capture-agent",
			"role":    "crew",
			"project": "gardenio",
		},
	}, map[string]string{"X-Escalate": "true"})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 202, got %d: %s", resp.StatusCode, body)
	}

	var escResp EscalationResponse
	escDecode(t, resp, &escResp)
	escalationID := escResp.EscalationID

	// Capture external result on the escalation. P7 Task 9: the static token
	// may not post a result, so an OIDC admin does.
	captureResp := escRequest(t, ts, "POST", "/v1/escalations/"+escalationID+"/result", oidcAdminTok, map[string]any{
		"executor":         "external-system",
		"execution_target": "kubernetes",
		"result_status":    "success",
		"result":           map[string]string{"pod": "agent-xyz-123"},
	}, nil)
	defer captureResp.Body.Close()

	if captureResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(captureResp.Body)
		t.Fatalf("expected 200 on capture, got %d: %s", captureResp.StatusCode, body)
	}

	// Verify escalation is closed with captured result.
	escBead, err := ms.GetBead(context.Background(), escalationID)
	if err != nil {
		t.Fatalf("get escalation: %v", err)
	}
	if escBead.Status != model.StatusClosed {
		t.Errorf("status=%q, want closed", escBead.Status)
	}

	var fields map[string]json.RawMessage
	json.Unmarshal(escBead.Fields, &fields)
	var executor string
	json.Unmarshal(fields["executor"], &executor)
	// P7 Task 10: the body's executor and execution_target are ignored.
	if executor != "owner@example.com" {
		t.Errorf("executor=%q, want owner@example.com (the authenticated caller)", executor)
	}
	if _, ok := fields["execution_target"]; ok {
		t.Errorf("execution_target=%s, want the body value ignored", fields["execution_target"])
	}
	var resultStatus string
	json.Unmarshal(fields["result_status"], &resultStatus)
	if resultStatus != "success" {
		t.Errorf("result_status=%q, want success", resultStatus)
	}
}

// TestEscalationAdvisoryTextE2E verifies that advisory text with escalation hint
// flows through the full HTTP pipeline.
func TestEscalationAdvisoryTextE2E(t *testing.T) {
	const (
		adminToken  = "admin-advisory-e2e"
		operatorTok = "oidc-advisory-op"
		fleetName   = "advisory-fleet"
	)

	oidcMock := &escOIDCMock{
		tokens: map[string]escTokenInfo{
			operatorTok: {email: "eve@example.com", roles: []string{RoleOperator}},
		},
	}

	fc := NewFleetConfig(fleetName, nil)
	ts, _, ms := newEscalationE2EServer(t, adminToken, fc, oidcMock)
	makeUserBead(ms, "gd-eve", "eve@example.com", "monorepo", "crew")

	// Test default advisory text on project-scoped denial.
	t.Run("DefaultAdvisoryText", func(t *testing.T) {
		resp := escRequest(t, ts, "POST", "/v1/beads", operatorTok, map[string]any{
			"title":  "Advisory test agent",
			"type":   "agent",
			"labels": []string{"project:gardenio"},
			"fields": map[string]any{
				"agent":   "adv-agent",
				"role":    "crew",
				"project": "gardenio",
			},
		}, nil)
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("expected 403, got %d", resp.StatusCode)
		}

		var denial AuthzDenial
		escDecode(t, resp, &denial)

		// Advisory text should contain: action, type, escalate hint.
		if !strings.Contains(denial.AdvisoryText, "create") {
			t.Errorf("advisory should mention 'create', got: %s", denial.AdvisoryText)
		}
		if !strings.Contains(denial.AdvisoryText, "agent") {
			t.Errorf("advisory should mention 'agent', got: %s", denial.AdvisoryText)
		}
		if !strings.Contains(denial.AdvisoryText, "--escalate") {
			t.Errorf("advisory should mention '--escalate', got: %s", denial.AdvisoryText)
		}
	})

	// Test custom advisory text template via config bead.
	t.Run("CustomAdvisoryText", func(t *testing.T) {
		ms.configs["authz-denied-text"] = &model.Config{
			Key:   "authz-denied-text",
			Value: json.RawMessage(`{"template":"BLOCKED: {action} on {type}. {reason} {escalate_hint}"}`),
		}
		defer delete(ms.configs, "authz-denied-text")

		resp := escRequest(t, ts, "POST", "/v1/beads", operatorTok, map[string]any{
			"title":  "Custom advisory test",
			"type":   "agent",
			"labels": []string{"project:gardenio"},
			"fields": map[string]any{
				"agent":   "custom-agent",
				"role":    "crew",
				"project": "gardenio",
			},
		}, nil)
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("expected 403, got %d", resp.StatusCode)
		}

		var denial AuthzDenial
		escDecode(t, resp, &denial)
		if !strings.HasPrefix(denial.AdvisoryText, "BLOCKED:") {
			t.Errorf("expected custom template prefix 'BLOCKED:', got: %s", denial.AdvisoryText)
		}
	})
}

// TestEscalationStaticTokenBypassE2E verifies that static-token (admin) requests
// bypass write policy entirely — no denial, no escalation.
func TestEscalationStaticTokenBypassE2E(t *testing.T) {
	const (
		adminToken = "admin-bypass-e2e"
		fleetName  = "bypass-fleet"
	)

	fc := NewFleetConfig(fleetName, nil)
	ts, _, _ := newEscalationE2EServer(t, adminToken, fc, nil)

	// Admin creates agent bead in any project — no write policy check.
	// Post-R2 (ka-0000005772): admin still needs an operator attribution,
	// supplied via X-Operated-By since the static token has no inherent
	// human identity.
	resp := escRequest(t, ts, "POST", "/v1/beads", adminToken, map[string]any{
		"title":  "Admin agent bypass",
		"type":   "agent",
		"labels": []string{"project:gardenio"},
		"fields": map[string]any{
			"agent":   "bypass-agent",
			"role":    "crew",
			"project": "gardenio",
		},
	}, map[string]string{"X-Operated-By": "slack:U0OPERATOR2"})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 201 for admin (static token bypass), got %d: %s", resp.StatusCode, body)
	}
}
