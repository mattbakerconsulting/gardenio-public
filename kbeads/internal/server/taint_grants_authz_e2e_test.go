package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"gardenio/kbeads/internal/model"
)

// End-to-end authorization tests for the taint grant/revoke management
// surface (docs/adr/taints.md §7/§8.2), gd-sIMJB8eY3q. Mirrors the shape of
// spawn_authz_e2e_test.go: requests go through the full HTTP stack
// (AuthMiddleware, real X-Operated-By sanitization) via newSpawnAuthzTestServer,
// rather than direct context injection — this is what actually proves the
// bare primary static token is forbidden and that the viewer-relay header
// path works end to end.

// staticReq builds a request authenticated with the shared primary static
// token ("admin-token", as configured by newSpawnAuthzTestServer) — the
// identity every agent and coopmux holds.
func staticReq(method, path string, body any) *http.Request {
	var req *http.Request
	if body != nil {
		b, _ := json.Marshal(body)
		req, _ = http.NewRequest(method, path, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req, _ = http.NewRequest(method, path, nil)
	}
	req.Header.Set("Authorization", "Bearer admin-token")
	return req
}

func TestTaintGrantsAuthzE2E_PrimaryStaticTokenForbidden_Grant(t *testing.T) {
	verifier := &stubOIDCVerifier{}
	h, ms := newSpawnAuthzTestServer(verifier)
	seedTaint(ms, 1, "acme-production", "gd-owner-team")

	req := staticReq(http.MethodPost, "/v1/taints/acme-production/grants", grantTaintRequest{Principal: "fleet:gardenio"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	requireStatus(t, rec, http.StatusForbidden)

	if len(ms.taintGrants) != 0 {
		t.Fatalf("expected no grant written, got %v", ms.taintGrants)
	}
}

func TestTaintGrantsAuthzE2E_PrimaryStaticTokenForbidden_Revoke(t *testing.T) {
	verifier := &stubOIDCVerifier{}
	h, ms := newSpawnAuthzTestServer(verifier)
	seedTaint(ms, 1, "acme-production", "gd-owner-team")
	ms.taintGrants = map[string]*model.TaintGrant{
		taintGrantKey(1, "fleet", "gardenio"): {TaintID: 1, PrincipalKind: "fleet", Principal: "gardenio", GrantedBy: 3},
	}

	req := staticReq(http.MethodDelete, "/v1/taints/acme-production/grants/fleet:gardenio", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	requireStatus(t, rec, http.StatusForbidden)

	if _, ok := ms.taintGrants[taintGrantKey(1, "fleet", "gardenio")]; !ok {
		t.Fatal("expected grant to remain, forbidden revoke must not mutate state")
	}
}

// TestTaintGrantsAuthzE2E_StaticRelayOwnerTeamMember_Forbidden: the shared
// static token relaying an owner-team member via X-Operated-By no longer
// grants. P7 Task 3: the static token's relay is attribution only (every pod
// without a per-agent token holds it); beads-viewer writes with the user's
// own bearer (gd-0ORn0Mhr4I). The bridge relay is covered by
// TestHandleGrantTaint_BridgeRelayOwnerTeamMember_Allowed.
func TestTaintGrantsAuthzE2E_StaticRelayOwnerTeamMember_Forbidden(t *testing.T) {
	verifier := &stubOIDCVerifier{}
	h, ms := newSpawnAuthzTestServer(verifier)
	seedTaint(ms, 1, "acme-production", "gd-owner-team")
	seedTeamBead(ms, "gd-owner-team", `{"team":"owner","members":["oidc:alice@example.com"]}`)
	ms.operatorAliases = map[string]int64{"oidc\x00alice@example.com": 7}

	req := staticReq(http.MethodPost, "/v1/taints/acme-production/grants", grantTaintRequest{Principal: "fleet:gardenio"})
	req.Header.Set("X-Operated-By", "oidc:alice@example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	requireStatus(t, rec, http.StatusForbidden)
	if len(ms.taintGrants) != 0 {
		t.Fatalf("expected no grant written, got %v", ms.taintGrants)
	}
}

// TestTaintGrantsAuthzE2E_ViewerRelayNonMember_Forbidden proves the viewer
// path does not become a blanket bypass: a relayed human who is NOT a member
// of the owner team (and not admin) still gets 403.
func TestTaintGrantsAuthzE2E_ViewerRelayNonMember_Forbidden(t *testing.T) {
	verifier := &stubOIDCVerifier{}
	h, ms := newSpawnAuthzTestServer(verifier)
	seedTaint(ms, 1, "acme-production", "gd-owner-team")
	seedTeamBead(ms, "gd-owner-team", `{"team":"owner","members":["oidc:alice@example.com"]}`)
	ms.operatorAliases = map[string]int64{"oidc\x00mallory@example.com": 9}

	req := staticReq(http.MethodPost, "/v1/taints/acme-production/grants", grantTaintRequest{Principal: "fleet:gardenio"})
	req.Header.Set("X-Operated-By", "oidc:mallory@example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	requireStatus(t, rec, http.StatusForbidden)

	if len(ms.taintGrants) != 0 {
		t.Fatalf("expected no grant written, got %v", ms.taintGrants)
	}
}

// TestTaintGrantsAuthzE2E_AdminBypass_Grant proves an OIDC caller with the
// admin role can grant regardless of team membership.
func TestTaintGrantsAuthzE2E_AdminBypass_Grant(t *testing.T) {
	verifier := &stubOIDCVerifier{email: "admin@example.com", roles: []string{RoleAdmin}}
	h, ms := newSpawnAuthzTestServer(verifier)
	seedTaint(ms, 1, "acme-production", "gd-owner-team")
	ms.operatorAliases = map[string]int64{"oidc\x00admin@example.com": 3}

	rec := doOIDC(t, h, http.MethodPost, "/v1/taints/acme-production/grants", grantTaintRequest{Principal: "fleet:gardenio"})
	requireStatus(t, rec, http.StatusCreated)
}
