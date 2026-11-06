package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// stubOIDCVerifier implements TokenVerifier, IdentityVerifier, and RoleExtractor
// for testing the full OIDC auth flow through AuthMiddleware → spawn authz.
type stubOIDCVerifier struct {
	email string
	roles []string
}

func (s *stubOIDCVerifier) Verify(_ context.Context, _ string) error { return nil }

func (s *stubOIDCVerifier) VerifyAndIdentify(_ context.Context, _ string) (string, error) {
	return s.email, nil
}

func (s *stubOIDCVerifier) ExtractRoles(_ context.Context, _ string) ([]string, error) {
	return s.roles, nil
}

// newSpawnAuthzTestServer creates an HTTP handler with OIDC auth configured.
// The static admin token is "admin-token"; OIDC requests use the provided
// stubOIDCVerifier. The UserProvisioner is disabled (set to nil) so tests
// can control user bead state explicitly.
func newSpawnAuthzTestServer(verifier TokenVerifier) (http.Handler, *mockStore) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	s.userProvisioner = nil // disable auto-provisioning for explicit control
	h := s.NewHTTPHandler(verifier, withTestToken(nil, "admin-token"))
	return h, ms
}

// spawnAuthzFleetTok is a fleet-scoped token distinct from the primary
// static admin token, used to prove the privileged-role gate cannot be
// bypassed from the fleet-token side either (gd-HWWEVbCy8N).
const spawnAuthzFleetTok = "tok-spawn-authz-fleet"

// newSpawnAuthzTestServerWithFleet is newSpawnAuthzTestServer plus a fleet
// token mapped to a non-default fleet, so a caller can authenticate as
// AuthIdentity{Type:"fleet"} rather than the primary static token.
func newSpawnAuthzTestServerWithFleet(verifier TokenVerifier) (http.Handler, *mockStore) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	s.userProvisioner = nil
	fc := NewFleetConfig("gardenio", map[string]string{spawnAuthzFleetTok: "some-bridge"})
	h := s.NewHTTPHandler(verifier, withTestToken(fc, "admin-token"))
	return h, ms
}

// oidcReq creates an HTTP request authenticated as an OIDC JWT bearer.
func oidcReq(t *testing.T, method, path string, body any) *http.Request {
	t.Helper()
	var req *http.Request
	var err error
	if body != nil {
		b, _ := json.Marshal(body)
		req, err = http.NewRequest(method, path, bytes.NewReader(b))
		if err != nil {
			t.Fatalf("creating request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
	} else {
		req, err = http.NewRequest(method, path, nil)
		if err != nil {
			t.Fatalf("creating request: %v", err)
		}
	}
	// Use a non-static token so AuthMiddleware falls through to OIDC verification.
	req.Header.Set("Authorization", "Bearer oidc-jwt-token")
	return req
}

func doOIDC(t *testing.T, h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	req := oidcReq(t, method, path, body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func agentCreateBody(project, role string) map[string]any {
	return map[string]any{
		"title": "test-agent",
		"type":  "agent",
		"fields": map[string]string{
			"project": project,
			"role":    role,
			"agent":   "test-agent",
		},
	}
}

// --- E2E Spawn Authorization Tests ---

// TestSpawnAuthzE2E_AuthenticatedUserCreatesAgent verifies that an OIDC-
// authenticated user with a user bead containing the right spawn permissions
// can successfully create an agent bead through the full HTTP stack.
func TestSpawnAuthzE2E_AuthenticatedUserCreatesAgent(t *testing.T) {
	verifier := &stubOIDCVerifier{
		email: "alice@example.com",
		roles: []string{RoleOperator},
	}
	h, ms := newSpawnAuthzTestServer(verifier)
	makeUserBead(ms, "gd-user-alice", "alice@example.com", "gardenio", "crew")

	rec := doOIDC(t, h, "POST", "/v1/beads", agentCreateBody("gardenio", "crew"))
	requireStatus(t, rec, 201)

	var bead model.Bead
	decodeJSON(t, rec, &bead)
	if bead.Type != "agent" {
		t.Fatalf("expected type=agent, got %q", bead.Type)
	}
}

// TestSpawnAuthzE2E_ViewerDenied verifies that a viewer-role OIDC user gets 403
// from the RBAC middleware before reaching spawn authorization (POST /v1/beads
// requires operator role).
func TestSpawnAuthzE2E_ViewerDenied(t *testing.T) {
	verifier := &stubOIDCVerifier{
		email: "viewer@example.com",
		roles: []string{RoleViewer},
	}
	h, _ := newSpawnAuthzTestServer(verifier)

	rec := doOIDC(t, h, "POST", "/v1/beads", agentCreateBody("gardenio", "crew"))
	requireStatus(t, rec, 403)

	body := rec.Body.String()
	if !strings.Contains(body, "insufficient permissions") {
		t.Fatalf("expected 'insufficient permissions' in response, got: %s", body)
	}
}

// TestSpawnAuthzE2E_NoUserBead verifies that an operator without a user bead
// is denied agent creation. The agent type has write_policy.project_scoped=true,
// so authorizeWrite fires first and returns a 403 AuthzDenial (not 400) with
// advisory text including escalation instructions.
func TestSpawnAuthzE2E_NoUserBead(t *testing.T) {
	verifier := &stubOIDCVerifier{
		email: "unknown@example.com",
		roles: []string{RoleOperator},
	}
	h, _ := newSpawnAuthzTestServer(verifier)

	rec := doOIDC(t, h, "POST", "/v1/beads", agentCreateBody("gardenio", "crew"))
	requireStatus(t, rec, 403)

	var denial AuthzDenial
	decodeJSON(t, rec, &denial)
	if !strings.Contains(denial.Message, "not authorized") {
		t.Fatalf("expected 'not authorized' error, got: %s", denial.Message)
	}
	if denial.AdvisoryText == "" {
		t.Fatal("denial should include advisory text")
	}
	if !denial.Escalatable {
		t.Fatal("denial should be escalatable per agent type write policy")
	}
}

// TestSpawnAuthzE2E_ProjectDenied verifies that an operator whose user bead
// does not include the target project is denied agent creation with a 403
// and advisory text including escalation instructions (write_policy on the
// agent type is project_scoped + escalatable).
func TestSpawnAuthzE2E_ProjectDenied(t *testing.T) {
	verifier := &stubOIDCVerifier{
		email: "bob@example.com",
		roles: []string{RoleOperator},
	}
	h, ms := newSpawnAuthzTestServer(verifier)

	// Bob only has permission for "monorepo", not "gardenio".
	makeUserBead(ms, "gd-user-bob", "bob@example.com", "monorepo", "crew")

	rec := doOIDC(t, h, "POST", "/v1/beads", agentCreateBody("gardenio", "crew"))
	requireStatus(t, rec, 403)

	var denial AuthzDenial
	decodeJSON(t, rec, &denial)
	if !strings.Contains(denial.Message, "not authorized for project") {
		t.Fatalf("expected project denial message, got: %s", denial.Message)
	}
	if !denial.Escalatable {
		t.Fatal("agent type write policy should be escalatable")
	}
	if denial.AdvisoryText == "" {
		t.Fatal("denial response should include advisory text")
	}
	if !strings.Contains(denial.AdvisoryText, "--escalate") {
		t.Fatal("advisory text should include escalation instructions")
	}
}

// TestSpawnAuthzE2E_RoleDenied verifies that an operator whose user bead
// does not include the target agent role is denied agent creation.
func TestSpawnAuthzE2E_RoleDenied(t *testing.T) {
	verifier := &stubOIDCVerifier{
		email: "carol@example.com",
		roles: []string{RoleOperator},
	}
	h, ms := newSpawnAuthzTestServer(verifier)

	// Carol has "thread" role, not "crew".
	makeUserBead(ms, "gd-user-carol", "carol@example.com", "gardenio", "thread")

	rec := doOIDC(t, h, "POST", "/v1/beads", agentCreateBody("gardenio", "crew"))
	requireStatus(t, rec, 400)

	body := rec.Body.String()
	if !strings.Contains(body, "not authorized for role") {
		t.Fatalf("expected 'not authorized for role' error, got: %s", body)
	}
}

// TestSpawnAuthzE2E_WildcardProject verifies that a user bead with
// allowed_projects="*" can spawn agents for any project.
func TestSpawnAuthzE2E_WildcardProject(t *testing.T) {
	verifier := &stubOIDCVerifier{
		email: "dave@example.com",
		roles: []string{RoleOperator},
	}
	h, ms := newSpawnAuthzTestServer(verifier)
	makeUserBead(ms, "gd-user-dave", "dave@example.com", "*", "crew,gardener")

	rec := doOIDC(t, h, "POST", "/v1/beads", agentCreateBody("any-project", "crew"))
	requireStatus(t, rec, 201)

	var bead model.Bead
	decodeJSON(t, rec, &bead)
	if bead.Type != "agent" {
		t.Fatalf("expected type=agent, got %q", bead.Type)
	}
}

// TestSpawnAuthzE2E_SpecificProjectLimitsSpawn verifies that a user bead
// with specific projects only allows those projects and denies others.
func TestSpawnAuthzE2E_SpecificProjectLimitsSpawn(t *testing.T) {
	verifier := &stubOIDCVerifier{
		email: "eve@example.com",
		roles: []string{RoleOperator},
	}
	h, ms := newSpawnAuthzTestServer(verifier)
	makeUserBead(ms, "gd-user-eve", "eve@example.com", "gardenio,monorepo", "crew")

	// Should succeed for an allowed project.
	rec := doOIDC(t, h, "POST", "/v1/beads", agentCreateBody("gardenio", "crew"))
	requireStatus(t, rec, 201)

	// Should fail for a non-allowed project (403 from write_policy project_scoped check).
	rec = doOIDC(t, h, "POST", "/v1/beads", agentCreateBody("secret-project", "crew"))
	requireStatus(t, rec, 403)

	var denial AuthzDenial
	decodeJSON(t, rec, &denial)
	if !strings.Contains(denial.Message, "not authorized for project") {
		t.Fatalf("expected project denial for 'secret-project', got: %s", denial.Message)
	}
}

// TestSpawnAuthzE2E_AdminBypass verifies that an admin-role OIDC user
// bypasses all spawn authorization checks, even without a user bead.
func TestSpawnAuthzE2E_AdminBypass(t *testing.T) {
	verifier := &stubOIDCVerifier{
		email: "admin@example.com",
		roles: []string{RoleAdmin},
	}
	h, _ := newSpawnAuthzTestServer(verifier)

	rec := doOIDC(t, h, "POST", "/v1/beads", agentCreateBody("any-project", "any-role"))
	requireStatus(t, rec, 201)
}

// TestSpawnAuthzE2E_StaticTokenBypass verifies that a static token request
// bypasses all spawn authorization checks (service-to-service).
func TestSpawnAuthzE2E_StaticTokenBypass(t *testing.T) {
	verifier := &stubOIDCVerifier{}
	h, _ := newSpawnAuthzTestServer(verifier)

	body, _ := json.Marshal(agentCreateBody("gardenio", "crew"))
	req, _ := http.NewRequest("POST", "/v1/beads", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer admin-token")
	req.Header.Set("X-Operated-By", "slack:U0OPERATOR2")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	requireStatus(t, rec, 201)
}

// TestSpawnAuthzE2E_EmptyPermissionsAllowAll verifies that a user bead
// with empty allowed_projects and allowed_roles permits all spawns.
func TestSpawnAuthzE2E_EmptyPermissionsAllowAll(t *testing.T) {
	verifier := &stubOIDCVerifier{
		email: "frank@example.com",
		roles: []string{RoleOperator},
	}
	h, ms := newSpawnAuthzTestServer(verifier)
	makeUserBead(ms, "gd-user-frank", "frank@example.com", "", "")

	rec := doOIDC(t, h, "POST", "/v1/beads", agentCreateBody("any-project", "any-role"))
	requireStatus(t, rec, 201)
}

// TestSpawnAuthzE2E_DeniedResponseContainsAdvisoryText verifies that the
// denial response body contains advisory text with escalation instructions
// (the agent type write_policy has escalatable=true).
func TestSpawnAuthzE2E_DeniedResponseContainsAdvisoryText(t *testing.T) {
	verifier := &stubOIDCVerifier{
		email: "denied@example.com",
		roles: []string{RoleOperator},
	}
	h, ms := newSpawnAuthzTestServer(verifier)
	makeUserBead(ms, "gd-user-denied", "denied@example.com", "internal", "crew")

	rec := doOIDC(t, h, "POST", "/v1/beads", agentCreateBody("gardenio", "crew"))
	requireStatus(t, rec, 403)

	var denial AuthzDenial
	decodeJSON(t, rec, &denial)

	// Advisory text should be populated.
	if denial.AdvisoryText == "" {
		t.Fatal("denial response should include advisory text")
	}
	// Should mention the denied action.
	if !strings.Contains(denial.AdvisoryText, "create") {
		t.Fatalf("advisory text should mention the denied action 'create', got: %s", denial.AdvisoryText)
	}
	// Should mention the bead type.
	if !strings.Contains(denial.AdvisoryText, "agent") {
		t.Fatalf("advisory text should mention the bead type 'agent', got: %s", denial.AdvisoryText)
	}
	// Should include escalation instructions since agent type is escalatable.
	if !strings.Contains(denial.AdvisoryText, "--escalate") {
		t.Fatalf("advisory text should include escalation instructions, got: %s", denial.AdvisoryText)
	}
	// Should mention the denied project for actionability.
	if !strings.Contains(denial.AdvisoryText, "gardenio") {
		t.Fatalf("advisory text should mention the denied project 'gardenio', got: %s", denial.AdvisoryText)
	}
}

// TestSpawnAuthzE2E_EscalationViaHeader verifies that a denied spawn can be
// escalated by setting the X-Escalate: true header, creating an escalation bead.
func TestSpawnAuthzE2E_EscalationViaHeader(t *testing.T) {
	verifier := &stubOIDCVerifier{
		email: "escalator@example.com",
		roles: []string{RoleOperator},
	}
	h, ms := newSpawnAuthzTestServer(verifier)
	makeUserBead(ms, "gd-user-esc", "escalator@example.com", "internal", "crew")

	body, _ := json.Marshal(agentCreateBody("gardenio", "crew"))
	req, _ := http.NewRequest("POST", "/v1/beads", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer oidc-jwt-token")
	req.Header.Set("X-Escalate", "true")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	requireStatus(t, rec, 202)

	var resp EscalationResponse
	decodeJSON(t, rec, &resp)
	if resp.EscalationID == "" {
		t.Fatal("expected escalation bead ID in response")
	}
	if resp.DeniedAction != "create" {
		t.Fatalf("expected denied_action=create, got %q", resp.DeniedAction)
	}

	// Verify the escalation bead was created in the store.
	esc, _ := ms.GetBead(context.Background(), resp.EscalationID)
	if esc == nil {
		t.Fatal("escalation bead should exist in store")
	}
	if esc.Type != "escalation" {
		t.Fatalf("expected type=escalation, got %q", esc.Type)
	}
}

// TestSpawnAuthzE2E_StaticTokenPrivilegedRoleDenied verifies that a static-
// token spawn of a privileged role (type:role privileged=true) is denied with
// a 403 and escalatable advisory text, per the spawn gate (gd-AWTT7AroK4,
// design doc gd-sGW5Y1B8sg §2).
func TestSpawnAuthzE2E_StaticTokenPrivilegedRoleDenied(t *testing.T) {
	verifier := &stubOIDCVerifier{}
	h, ms := newSpawnAuthzTestServer(verifier)
	makeRoleBead(ms, "gd-role-k8s-live", "k8s-live", true)

	body, _ := json.Marshal(agentCreateBody("gardenio", "k8s-live"))
	req, _ := http.NewRequest("POST", "/v1/beads", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer admin-token")
	req.Header.Set("X-Operated-By", "slack:U0OPERATOR2")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	requireStatus(t, rec, 403)

	var denial AuthzDenial
	decodeJSON(t, rec, &denial)
	if !denial.Escalatable {
		t.Fatal("privileged-role spawn denial should be escalatable")
	}
	if denial.AdvisoryText == "" || !strings.Contains(denial.AdvisoryText, "--escalate") {
		t.Fatalf("advisory text should point at --escalate, got: %s", denial.AdvisoryText)
	}
}

// TestSpawnAuthzE2E_StaticTokenPrivilegedRoleEscalates verifies that retrying
// a denied privileged-role spawn with X-Escalate: true produces a 202 and an
// escalation bead carrying operation_type=create, spawn_role=k8s-live, and
// requesting_agent set (gd-AWTT7AroK4 acceptance criteria).
func TestSpawnAuthzE2E_StaticTokenPrivilegedRoleEscalates(t *testing.T) {
	verifier := &stubOIDCVerifier{}
	h, ms := newSpawnAuthzTestServer(verifier)
	makeRoleBead(ms, "gd-role-k8s-live", "k8s-live", true)

	body, _ := json.Marshal(agentCreateBody("gardenio", "k8s-live"))
	req, _ := http.NewRequest("POST", "/v1/beads", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer admin-token")
	req.Header.Set("X-Operated-By", "slack:U0OPERATOR2")
	req.Header.Set("X-Escalate", "true")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	requireStatus(t, rec, 202)

	var resp EscalationResponse
	decodeJSON(t, rec, &resp)
	if resp.EscalationID == "" {
		t.Fatal("expected escalation bead ID in response")
	}

	esc, _ := ms.GetBead(context.Background(), resp.EscalationID)
	if esc == nil {
		t.Fatal("escalation bead should exist in store")
	}
	var fields struct {
		OperationType   string `json:"operation_type"`
		SpawnRole       string `json:"spawn_role"`
		RequestingAgent string `json:"requesting_agent"`
	}
	if err := json.Unmarshal(esc.Fields, &fields); err != nil {
		t.Fatalf("unmarshal escalation fields: %v", err)
	}
	if fields.OperationType != "create" {
		t.Fatalf("expected operation_type=create, got %q", fields.OperationType)
	}
	if fields.SpawnRole != "k8s-live" {
		t.Fatalf("expected spawn_role=k8s-live, got %q", fields.SpawnRole)
	}
	if fields.RequestingAgent == "" {
		t.Fatal("expected requesting_agent to be set")
	}
}

// TestSpawnAuthzE2E_OIDCAdminSpawnsPrivilegedRoleDirectly verifies that an
// OIDC admin can spawn a privileged role directly, with no escalation needed.
func TestSpawnAuthzE2E_OIDCAdminSpawnsPrivilegedRoleDirectly(t *testing.T) {
	verifier := &stubOIDCVerifier{
		email: "admin@example.com",
		roles: []string{RoleAdmin},
	}
	h, ms := newSpawnAuthzTestServer(verifier)
	makeRoleBead(ms, "gd-role-k8s-live", "k8s-live", true)

	rec := doOIDC(t, h, "POST", "/v1/beads", agentCreateBody("gardenio", "k8s-live"))
	requireStatus(t, rec, 201)
}

// TestSpawnAuthzE2E_StaticTokenNonPrivilegedRoleUnaffected verifies that a
// static-token spawn of a role with no privileged type:role bead (or an
// explicitly non-privileged one) is unaffected by the new gate.
func TestSpawnAuthzE2E_StaticTokenNonPrivilegedRoleUnaffected(t *testing.T) {
	verifier := &stubOIDCVerifier{}
	h, ms := newSpawnAuthzTestServer(verifier)
	makeRoleBead(ms, "gd-role-crew", "crew", false)

	body, _ := json.Marshal(agentCreateBody("gardenio", "crew"))
	req, _ := http.NewRequest("POST", "/v1/beads", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer admin-token")
	req.Header.Set("X-Operated-By", "slack:U0OPERATOR2")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	requireStatus(t, rec, 201)
}

// TestSpawnAuthzE2E_SpawnPrivilegedOverrideHeaderIgnored verifies that the
// removed X-Spawn-Privileged-Override header no longer bypasses the
// privileged-role spawn gate for any non-OIDC token kind (gd-HWWEVbCy8N):
// every agent pod holds the same primary static token as the caller this
// gate exists to constrain, so honouring a header on that token was itself
// the bypass. A human who needs a privileged spawn now goes through the
// OIDC escalation-approve step (gd-731gBeGgl7) like everyone else.
func TestSpawnAuthzE2E_SpawnPrivilegedOverrideHeaderIgnored(t *testing.T) {
	t.Run("PrimaryStaticToken", func(t *testing.T) {
		verifier := &stubOIDCVerifier{}
		h, ms := newSpawnAuthzTestServer(verifier)
		makeRoleBead(ms, "gd-role-k8s-live", "k8s-live", true)

		body, _ := json.Marshal(agentCreateBody("gardenio", "k8s-live"))
		req, _ := http.NewRequest("POST", "/v1/beads", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer admin-token")
		req.Header.Set("X-Operated-By", "slack:U0OPERATOR2")
		req.Header.Set("X-Spawn-Privileged-Override", "true")

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		requireStatus(t, rec, 403)

		var denial AuthzDenial
		decodeJSON(t, rec, &denial)
		if !denial.Escalatable {
			t.Fatal("privileged-role spawn denial should still be escalatable")
		}
	})

	t.Run("FleetToken", func(t *testing.T) {
		verifier := &stubOIDCVerifier{}
		h, ms := newSpawnAuthzTestServerWithFleet(verifier)
		makeRoleBead(ms, "gd-role-k8s-live", "k8s-live", true)

		body, _ := json.Marshal(agentCreateBody("gardenio", "k8s-live"))
		req, _ := http.NewRequest("POST", "/v1/beads", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+spawnAuthzFleetTok)
		req.Header.Set("X-Spawn-Privileged-Override", "true")

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		requireStatus(t, rec, 403)

		var denial AuthzDenial
		decodeJSON(t, rec, &denial)
		if !denial.Escalatable {
			t.Fatal("privileged-role spawn denial should still be escalatable")
		}
	})
}

// TestSpawnAuthzE2E_NonAgentBeadSkipsSpawnAuthz verifies that creating a
// non-agent bead type (e.g., task) does not trigger spawn authorization
// even for OIDC users without a user bead.
func TestSpawnAuthzE2E_NonAgentBeadSkipsSpawnAuthz(t *testing.T) {
	verifier := &stubOIDCVerifier{
		email: "nouserbead@example.com",
		roles: []string{RoleOperator},
	}
	h, _ := newSpawnAuthzTestServer(verifier)

	rec := doOIDC(t, h, "POST", "/v1/beads", map[string]any{
		"title": "test-task",
		"type":  "task",
	})
	requireStatus(t, rec, 201)
}
