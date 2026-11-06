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

// Handler tests for the taint_grants/bead_taints write surface
// (docs/adr/taints.md §7/§5.2/§8.2/§10.4/§4.2), gd-sIMJB8eY3q. Every
// acceptance criterion on the task is covered here.

func newTaintGrantsTestServer() (*BeadsServer, *mockStore) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	s.userProvisioner = nil
	return s, ms
}

func seedTaint(ms *mockStore, id int64, name, ownerTeam string) *model.Taint {
	t := &model.Taint{ID: id, Name: name, OwnerTeam: ownerTeam}
	if ms.taintRegistry == nil {
		ms.taintRegistry = make(map[string]*model.Taint)
	}
	ms.taintRegistry[name] = t
	return t
}

func asOIDC(ctx context.Context, email string) context.Context {
	ctx = WithAuthIdentity(ctx, AuthIdentity{Type: "oidc", Subject: email})
	return withAuthMethod(ctx, authMethodOIDC)
}

// --- grant: authorization ---------------------------------------------

func TestHandleGrantTaint_NonOwnerNonAdmin_Forbidden(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedTaint(ms, 1, "acme-production", "gd-owner-team")
	ms.operatorAliases = map[string]int64{"oidc\x00outsider@example.com": 1}

	body, _ := json.Marshal(grantTaintRequest{Principal: "operator:2"})
	req := httptest.NewRequest(http.MethodPost, "/v1/taints/acme-production/grants", bytes.NewReader(body))
	req.SetPathValue("name", "acme-production")
	req = req.WithContext(asOIDC(req.Context(), "outsider@example.com"))
	rec := httptest.NewRecorder()

	s.handleGrantTaint(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if len(ms.taintGrants) != 0 {
		t.Fatalf("expected no grant written, got %v", ms.taintGrants)
	}
}

func TestHandleGrantTaint_OwnerTeamMember_Allowed(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedTaint(ms, 1, "acme-production", "gd-owner-team")
	seedTeamBead(ms, "gd-owner-team", `{"team":"owner","members":["oidc:alice@example.com"]}`)
	ms.operatorAliases = map[string]int64{
		"oidc\x00alice@example.com": 7,
		"oidc\x00bob@example.com":   9,
	}

	body, _ := json.Marshal(grantTaintRequest{Principal: "operator:bob@example.com"})
	req := httptest.NewRequest(http.MethodPost, "/v1/taints/acme-production/grants", bytes.NewReader(body))
	req.SetPathValue("name", "acme-production")
	req = req.WithContext(asOIDC(req.Context(), "alice@example.com"))
	rec := httptest.NewRecorder()

	s.handleGrantTaint(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var got model.TaintGrant
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.GrantedBy != 7 {
		t.Fatalf("expected granted_by=7 (alice), got %d", got.GrantedBy)
	}
	if got.Principal != "9" {
		t.Fatalf("expected resolved operator principal \"9\", got %q", got.Principal)
	}
	if len(ms.taintAudit) != 1 {
		t.Fatalf("expected 1 taint_audit row, got %d", len(ms.taintAudit))
	}
	if a := ms.taintAudit[0]; a.Action != model.TaintAuditGrant || a.TaintID != 1 || a.Actor != "alice@example.com" {
		t.Fatalf("unexpected taint_audit row: %+v", a)
	}
}

func TestHandleGrantTaint_Admin_Allowed(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedTaint(ms, 1, "acme-production", "gd-owner-team")
	ms.operatorAliases = map[string]int64{"oidc\x00admin@example.com": 3}

	body, _ := json.Marshal(grantTaintRequest{Principal: "fleet:gardenio"})
	req := httptest.NewRequest(http.MethodPost, "/v1/taints/acme-production/grants", bytes.NewReader(body))
	req.SetPathValue("name", "acme-production")
	ctx := asOIDC(req.Context(), "admin@example.com")
	ctx = WithRoles(ctx, []string{RoleAdmin})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	s.handleGrantTaint(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

// TestHandleGrantTaint_PrimaryStaticToken_Forbidden proves the bare primary
// static token (Type=static, Subject=static — the identity shared by every
// agent and coopmux) can no longer grant a taint on its own: it carries no
// admin role and resolves to no team membership, so callerMayManageTaint
// must deny it. See also the end-to-end variant in
// taint_grants_authz_e2e_test.go, which exercises the same property through
// the real HTTP stack and header sanitization.
func TestHandleGrantTaint_PrimaryStaticToken_Forbidden(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedTaint(ms, 1, "acme-production", "gd-owner-team")

	body, _ := json.Marshal(grantTaintRequest{Principal: "fleet:gardenio"})
	req := httptest.NewRequest(http.MethodPost, "/v1/taints/acme-production/grants", bytes.NewReader(body))
	req.SetPathValue("name", "acme-production")
	req = req.WithContext(withTestPrivilegedAgent(req.Context(), "static"))
	rec := httptest.NewRecorder()

	s.handleGrantTaint(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if len(ms.taintGrants) != 0 {
		t.Fatalf("expected no grant written, got %v", ms.taintGrants)
	}
}

// TestHandleGrantTaint_BridgeRelayOwnerTeamMember_Allowed proves the relay
// path still works after the static-token shortcut removal: a bridge fleet
// caller relaying an authenticated human via a pre-sanitized X-Operated-By
// context value (as the auth interceptor would install it — see
// WithOperatedByHeader) succeeds when that human is a member of the taint's
// owner team. P7 Task 3: only a bridge's relay counts, so this runs as
// slack-bridge, not the static token.
func TestHandleGrantTaint_BridgeRelayOwnerTeamMember_Allowed(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedTaint(ms, 1, "acme-production", "gd-owner-team")
	seedTeamBead(ms, "gd-owner-team", `{"team":"owner","members":["oidc:alice@example.com"]}`)
	ms.operatorAliases = map[string]int64{"oidc\x00alice@example.com": 7}

	body, _ := json.Marshal(grantTaintRequest{Principal: "fleet:gardenio"})
	req := httptest.NewRequest(http.MethodPost, "/v1/taints/acme-production/grants", bytes.NewReader(body))
	req.SetPathValue("name", "acme-production")
	ctx := WithAuthIdentity(req.Context(), AuthIdentity{Type: "fleet", Subject: "slack-bridge"})
	ctx = WithFleet(ctx, "slack-bridge")
	ctx = WithOperatedByHeader(ctx, "oidc:alice@example.com")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	s.handleGrantTaint(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var got model.TaintGrant
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.GrantedBy != 7 {
		t.Fatalf("expected granted_by=7 (alice, relayed via X-Operated-By), got %d", got.GrantedBy)
	}
}

// --- grant: expiry defaults ---------------------------------------------

func TestHandleGrantTaint_AgentPrincipal_AlwaysBounded_IgnoresUntil(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedTaint(ms, 1, "acme-production", "gd-owner-team")
	ms.beads["gd-agent1"] = &model.Bead{ID: "gd-agent1", Type: "agent", Kind: model.KindIssue}

	farFuture := time.Now().Add(365 * 24 * time.Hour)
	body, _ := json.Marshal(grantTaintRequest{Principal: "agent:gd-agent1", Until: &farFuture})
	req := httptest.NewRequest(http.MethodPost, "/v1/taints/acme-production/grants", bytes.NewReader(body))
	req.SetPathValue("name", "acme-production")
	ctx := asOIDC(req.Context(), "admin@example.com")
	ctx = WithRoles(ctx, []string{RoleAdmin})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	s.handleGrantTaint(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var got model.TaintGrant
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ExpiresAt == nil {
		t.Fatal("expected non-nil expires_at for an agent grant")
	}
	if got.ExpiresAt.Equal(farFuture) || got.ExpiresAt.After(farFuture) {
		t.Fatalf("expected client Until to be ignored; got expires_at=%v, client Until=%v", got.ExpiresAt, farFuture)
	}
}

func TestHandleGrantTaint_OperatorPrincipal_DefaultNinetyDays(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedTaint(ms, 1, "acme-production", "gd-owner-team")
	ms.operatorAliases = map[string]int64{"oidc\x00bob@example.com": 9}

	body, _ := json.Marshal(grantTaintRequest{Principal: "operator:bob@example.com"})
	req := httptest.NewRequest(http.MethodPost, "/v1/taints/acme-production/grants", bytes.NewReader(body))
	req.SetPathValue("name", "acme-production")
	ctx := asOIDC(req.Context(), "admin@example.com")
	ctx = WithRoles(ctx, []string{RoleAdmin})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	before := time.Now()
	s.handleGrantTaint(rec, req)
	after := time.Now()

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var got model.TaintGrant
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ExpiresAt == nil {
		t.Fatal("expected non-nil expires_at for an operator grant with no --until")
	}
	wantMin := before.Add(90 * 24 * time.Hour)
	wantMax := after.Add(90 * 24 * time.Hour)
	if got.ExpiresAt.Before(wantMin) || got.ExpiresAt.After(wantMax) {
		t.Fatalf("expected expires_at ~90d out, got %v (want between %v and %v)", got.ExpiresAt, wantMin, wantMax)
	}
}

func TestHandleGrantTaint_TeamAndFleetPrincipal_DefaultNull(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedTaint(ms, 1, "acme-production", "gd-owner-team")
	seedTeamBead(ms, "gd-other-team", `{"team":"other"}`)

	for _, principal := range []string{"team:gd-other-team", "fleet:gardenio"} {
		body, _ := json.Marshal(grantTaintRequest{Principal: principal})
		req := httptest.NewRequest(http.MethodPost, "/v1/taints/acme-production/grants", bytes.NewReader(body))
		req.SetPathValue("name", "acme-production")
		ctx := asOIDC(req.Context(), "admin@example.com")
		ctx = WithRoles(ctx, []string{RoleAdmin})
		req = req.WithContext(ctx)
		rec := httptest.NewRecorder()

		s.handleGrantTaint(rec, req)

		if rec.Code != http.StatusCreated {
			t.Fatalf("principal %q: expected 201, got %d; body: %s", principal, rec.Code, rec.Body.String())
		}
		var got model.TaintGrant
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got.ExpiresAt != nil {
			t.Fatalf("principal %q: expected nil expires_at, got %v", principal, got.ExpiresAt)
		}
	}
}

func TestHandleGrantTaint_TeamPrincipal_UntilHonored(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedTaint(ms, 1, "acme-production", "gd-owner-team")
	seedTeamBead(ms, "gd-other-team", `{"team":"other"}`)

	until := time.Now().Add(30 * 24 * time.Hour)
	body, _ := json.Marshal(grantTaintRequest{Principal: "team:gd-other-team", Until: &until})
	req := httptest.NewRequest(http.MethodPost, "/v1/taints/acme-production/grants", bytes.NewReader(body))
	req.SetPathValue("name", "acme-production")
	ctx := asOIDC(req.Context(), "admin@example.com")
	ctx = WithRoles(ctx, []string{RoleAdmin})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	s.handleGrantTaint(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var got model.TaintGrant
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(until.UTC()) {
		t.Fatalf("expected expires_at=%v, got %v", until.UTC(), got.ExpiresAt)
	}
}

// --- grant: granted_by is never client-supplied --------------------------

func TestHandleGrantTaint_GrantedByServerDerived(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedTaint(ms, 1, "acme-production", "gd-owner-team")
	ms.operatorAliases = map[string]int64{
		"oidc\x00alice@example.com": 7,
		"oidc\x00bob@example.com":   9,
	}

	// grantTaintRequest has no client-settable GrantedBy field at all — the
	// wire shape itself makes forging it impossible. Assert the persisted
	// value is the resolved caller's operator id regardless.
	body, _ := json.Marshal(grantTaintRequest{Principal: "operator:bob@example.com"})
	req := httptest.NewRequest(http.MethodPost, "/v1/taints/acme-production/grants", bytes.NewReader(body))
	req.SetPathValue("name", "acme-production")
	ctx := asOIDC(req.Context(), "alice@example.com")
	ctx = WithRoles(ctx, []string{RoleAdmin})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	s.handleGrantTaint(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d; body: %s", rec.Code, rec.Body.String())
	}
	key := taintGrantKey(1, "operator", "9")
	stored, ok := ms.taintGrants[key]
	if !ok {
		t.Fatal("expected grant to be stored")
	}
	if stored.GrantedBy != 7 {
		t.Fatalf("expected granted_by=7 (alice, the caller), got %d", stored.GrantedBy)
	}
}

// --- grant: not found / bad request --------------------------------------

func TestHandleGrantTaint_UnknownTaint_NotFound(t *testing.T) {
	s, _ := newTaintGrantsTestServer()

	body, _ := json.Marshal(grantTaintRequest{Principal: "fleet:gardenio"})
	req := httptest.NewRequest(http.MethodPost, "/v1/taints/ghost/grants", bytes.NewReader(body))
	req.SetPathValue("name", "ghost")
	req = req.WithContext(withTestPrivilegedAgent(req.Context(), "static"))
	rec := httptest.NewRecorder()

	s.handleGrantTaint(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

// --- revoke ---------------------------------------------------------------

func TestHandleRevokeTaint_RemovesGrant(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedTaint(ms, 1, "acme-production", "gd-owner-team")
	ms.taintGrants = map[string]*model.TaintGrant{
		taintGrantKey(1, "fleet", "gardenio"): {TaintID: 1, PrincipalKind: "fleet", Principal: "gardenio", GrantedBy: 3},
	}

	req := httptest.NewRequest(http.MethodDelete, "/v1/taints/acme-production/grants/fleet:gardenio", nil)
	req.SetPathValue("name", "acme-production")
	req.SetPathValue("principal", "fleet:gardenio")
	ctx := asOIDC(req.Context(), "admin@example.com")
	ctx = WithRoles(ctx, []string{RoleAdmin})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	s.handleRevokeTaint(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var got map[string]bool
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got["removed"] {
		t.Fatal("expected removed=true")
	}
	if _, ok := ms.taintGrants[taintGrantKey(1, "fleet", "gardenio")]; ok {
		t.Fatal("expected grant to be deleted")
	}
	if len(ms.taintAudit) != 1 {
		t.Fatalf("expected 1 taint_audit row, got %d", len(ms.taintAudit))
	}
	if a := ms.taintAudit[0]; a.Action != model.TaintAuditRevoke || a.TaintID != 1 {
		t.Fatalf("unexpected taint_audit row: %+v", a)
	}
}

func TestHandleRevokeTaint_IdempotentWhenNoMatch(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedTaint(ms, 1, "acme-production", "gd-owner-team")

	req := httptest.NewRequest(http.MethodDelete, "/v1/taints/acme-production/grants/fleet:gardenio", nil)
	req.SetPathValue("name", "acme-production")
	req.SetPathValue("principal", "fleet:gardenio")
	ctx := asOIDC(req.Context(), "admin@example.com")
	ctx = WithRoles(ctx, []string{RoleAdmin})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	s.handleRevokeTaint(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var got map[string]bool
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["removed"] {
		t.Fatal("expected removed=false")
	}
	if len(ms.taintAudit) != 0 {
		t.Fatalf("expected no taint_audit row for a no-op revoke, got %d", len(ms.taintAudit))
	}
}

// TestHandleRevokeTaint_PrimaryStaticToken_Forbidden proves the bare primary
// static token can no longer revoke a grant on its own — the revoke mirror
// of TestHandleGrantTaint_PrimaryStaticToken_Forbidden.
func TestHandleRevokeTaint_PrimaryStaticToken_Forbidden(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedTaint(ms, 1, "acme-production", "gd-owner-team")
	ms.taintGrants = map[string]*model.TaintGrant{
		taintGrantKey(1, "fleet", "gardenio"): {TaintID: 1, PrincipalKind: "fleet", Principal: "gardenio", GrantedBy: 3},
	}

	req := httptest.NewRequest(http.MethodDelete, "/v1/taints/acme-production/grants/fleet:gardenio", nil)
	req.SetPathValue("name", "acme-production")
	req.SetPathValue("principal", "fleet:gardenio")
	req = req.WithContext(withTestPrivilegedAgent(req.Context(), "static"))
	rec := httptest.NewRecorder()

	s.handleRevokeTaint(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if _, ok := ms.taintGrants[taintGrantKey(1, "fleet", "gardenio")]; !ok {
		t.Fatal("expected grant to remain, forbidden revoke must not mutate state")
	}
}

func TestHandleRevokeTaint_NonOwnerNonAdmin_Forbidden(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedTaint(ms, 1, "acme-production", "gd-owner-team")
	ms.operatorAliases = map[string]int64{"oidc\x00outsider@example.com": 1}
	ms.taintGrants = map[string]*model.TaintGrant{
		taintGrantKey(1, "fleet", "gardenio"): {TaintID: 1, PrincipalKind: "fleet", Principal: "gardenio", GrantedBy: 3},
	}

	req := httptest.NewRequest(http.MethodDelete, "/v1/taints/acme-production/grants/fleet:gardenio", nil)
	req.SetPathValue("name", "acme-production")
	req.SetPathValue("principal", "fleet:gardenio")
	req = req.WithContext(asOIDC(req.Context(), "outsider@example.com"))
	rec := httptest.NewRecorder()

	s.handleRevokeTaint(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if _, ok := ms.taintGrants[taintGrantKey(1, "fleet", "gardenio")]; !ok {
		t.Fatal("expected grant to remain, unauthorized revoke must not mutate state")
	}
}

// --- apply ------------------------------------------------------------

func TestHandleApplyTaint_NoGrant_Forbidden(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedTaint(ms, 1, "acme-production", "gd-owner-team")
	ms.beads["gd-bead1"] = &model.Bead{ID: "gd-bead1", Type: "task", Kind: model.KindIssue}
	ms.operatorAliases = map[string]int64{"oidc\x00outsider@example.com": 1}

	body, _ := json.Marshal(applyTaintRequest{Name: "acme-production"})
	req := httptest.NewRequest(http.MethodPost, "/v1/beads/gd-bead1/taints", bytes.NewReader(body))
	req.SetPathValue("id", "gd-bead1")
	req = req.WithContext(asOIDC(req.Context(), "outsider@example.com"))
	rec := httptest.NewRecorder()

	s.handleApplyTaint(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if len(ms.beadTaints["gd-bead1"]) != 0 {
		t.Fatalf("expected no taint applied, got %v", ms.beadTaints["gd-bead1"])
	}
}

func TestHandleApplyTaint_GrantHolder_WritesExplicitSource(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedTaint(ms, 1, "acme-production", "gd-owner-team")
	ms.beads["gd-bead1"] = &model.Bead{ID: "gd-bead1", Type: "task", Kind: model.KindIssue}
	ms.operatorAliases = map[string]int64{"oidc\x00alice@example.com": 7}
	ms.taintGrants = map[string]*model.TaintGrant{
		taintGrantKey(1, "operator", "7"): {TaintID: 1, PrincipalKind: "operator", Principal: "7", GrantedBy: 3},
	}

	body, _ := json.Marshal(applyTaintRequest{Name: "acme-production"})
	req := httptest.NewRequest(http.MethodPost, "/v1/beads/gd-bead1/taints", bytes.NewReader(body))
	req.SetPathValue("id", "gd-bead1")
	req = req.WithContext(asOIDC(req.Context(), "alice@example.com"))
	rec := httptest.NewRecorder()

	s.handleApplyTaint(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var got model.BeadTaint
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Source != model.TaintSourceExplicit {
		t.Fatalf("expected source=explicit, got %q", got.Source)
	}
	if got.AppliedBy != "alice@example.com" {
		t.Fatalf("expected applied_by=alice@example.com, got %q", got.AppliedBy)
	}
	if len(ms.taintAudit) != 1 {
		t.Fatalf("expected 1 taint_audit row, got %d", len(ms.taintAudit))
	}
	if a := ms.taintAudit[0]; a.Action != model.TaintAuditApply || a.BeadID != "gd-bead1" || a.Actor != "alice@example.com" {
		t.Fatalf("unexpected taint_audit row: %+v", a)
	}
}

func TestHandleApplyTaint_OwnerTeamImplicitGrant_Allowed(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedTaint(ms, 1, "acme-production", "gd-owner-team")
	seedTeamBead(ms, "gd-owner-team", `{"team":"owner","members":["oidc:alice@example.com"]}`)
	ms.beads["gd-bead1"] = &model.Bead{ID: "gd-bead1", Type: "task", Kind: model.KindIssue}
	ms.operatorAliases = map[string]int64{"oidc\x00alice@example.com": 7}

	body, _ := json.Marshal(applyTaintRequest{Name: "acme-production"})
	req := httptest.NewRequest(http.MethodPost, "/v1/beads/gd-bead1/taints", bytes.NewReader(body))
	req.SetPathValue("id", "gd-bead1")
	req = req.WithContext(asOIDC(req.Context(), "alice@example.com"))
	rec := httptest.NewRecorder()

	s.handleApplyTaint(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 via owner-team implicit grant, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleApplyTaint_Idempotent_PreservesOriginalProvenance(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedTaint(ms, 1, "acme-production", "gd-owner-team")
	ms.beads["gd-bead1"] = &model.Bead{ID: "gd-bead1", Type: "task", Kind: model.KindIssue}
	ms.operatorAliases = map[string]int64{
		"oidc\x00alice@example.com": 7,
		"oidc\x00carol@example.com": 8,
	}
	ms.taintGrants = map[string]*model.TaintGrant{
		taintGrantKey(1, "operator", "7"): {TaintID: 1, PrincipalKind: "operator", Principal: "7", GrantedBy: 3},
		taintGrantKey(1, "operator", "8"): {TaintID: 1, PrincipalKind: "operator", Principal: "8", GrantedBy: 3},
	}

	body, _ := json.Marshal(applyTaintRequest{Name: "acme-production"})

	req1 := httptest.NewRequest(http.MethodPost, "/v1/beads/gd-bead1/taints", bytes.NewReader(body))
	req1.SetPathValue("id", "gd-bead1")
	req1 = req1.WithContext(asOIDC(req1.Context(), "alice@example.com"))
	rec1 := httptest.NewRecorder()
	s.handleApplyTaint(rec1, req1)
	if rec1.Code != http.StatusCreated {
		t.Fatalf("first apply: expected 201, got %d; body: %s", rec1.Code, rec1.Body.String())
	}

	req2 := httptest.NewRequest(http.MethodPost, "/v1/beads/gd-bead1/taints", bytes.NewReader(body))
	req2.SetPathValue("id", "gd-bead1")
	req2 = req2.WithContext(asOIDC(req2.Context(), "carol@example.com"))
	rec2 := httptest.NewRecorder()
	s.handleApplyTaint(rec2, req2)
	if rec2.Code != http.StatusCreated {
		t.Fatalf("second apply: expected 201, got %d; body: %s", rec2.Code, rec2.Body.String())
	}

	var got model.BeadTaint
	if err := json.Unmarshal(rec2.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.AppliedBy != "alice@example.com" {
		t.Fatalf("expected original applied_by=alice@example.com preserved, got %q", got.AppliedBy)
	}
	if len(ms.beadTaints["gd-bead1"]) != 1 {
		t.Fatalf("expected exactly one bead_taints row, got %d", len(ms.beadTaints["gd-bead1"]))
	}
	if len(ms.taintAudit) != 1 {
		t.Fatalf("expected exactly one taint_audit row (idempotent re-apply must not duplicate), got %d", len(ms.taintAudit))
	}
}

// --- show -------------------------------------------------------------

func TestHandleGetBeadTaints_ListsSourceAndSourceBeadID(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	ms.beads["gd-bead1"] = &model.Bead{ID: "gd-bead1", Type: "task", Kind: model.KindIssue}
	ms.beadTaints = map[string][]*model.BeadTaint{
		"gd-bead1": {
			{BeadID: "gd-bead1", TaintID: 1, TaintName: "acme-production", Source: model.TaintSourceExplicit, AppliedBy: "alice@example.com"},
			{BeadID: "gd-bead1", TaintID: 2, TaintName: "jira-prod", Source: model.TaintSourceInheritCreate, SourceBeadID: "gd-parent", AppliedBy: "system:machine"},
		},
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/beads/gd-bead1/taints", nil)
	req.SetPathValue("id", "gd-bead1")
	req = req.WithContext(withTestPrivilegedAgent(req.Context(), "static"))
	rec := httptest.NewRecorder()

	s.handleGetBeadTaints(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Taints []*model.BeadTaint `json:"taints"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Taints) != 2 {
		t.Fatalf("expected 2 taints, got %d", len(got.Taints))
	}
	if got.Taints[0].Source != model.TaintSourceExplicit || got.Taints[0].SourceBeadID != "" {
		t.Fatalf("unexpected first taint: %+v", got.Taints[0])
	}
	if got.Taints[1].SourceBeadID != "gd-parent" {
		t.Fatalf("unexpected second taint source_bead_id: %+v", got.Taints[1])
	}
}

func TestHandleGetBeadTaints_ReachableByAnyReaderNotJustGrantHolder(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	// Bead carries no project label; caller has no grant on any taint and is
	// not the owner team — GetBeadTaints must still succeed, since read
	// access is gated on bead-read, not grant-holding.
	ms.beads["gd-bead1"] = &model.Bead{ID: "gd-bead1", Type: "task", Kind: model.KindIssue}
	ms.beadTaints = map[string][]*model.BeadTaint{
		"gd-bead1": {
			{BeadID: "gd-bead1", TaintID: 1, TaintName: "acme-production", Source: model.TaintSourceExplicit, AppliedBy: "alice@example.com"},
		},
	}
	ms.operatorAliases = map[string]int64{"oidc\x00outsider@example.com": 1}

	req := httptest.NewRequest(http.MethodGet, "/v1/beads/gd-bead1/taints", nil)
	req.SetPathValue("id", "gd-bead1")
	req = req.WithContext(asOIDC(req.Context(), "outsider@example.com"))
	rec := httptest.NewRecorder()

	s.handleGetBeadTaints(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for a bead-reader with no grant, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Taints []*model.BeadTaint `json:"taints"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Taints) != 1 {
		t.Fatalf("expected 1 taint, got %d", len(got.Taints))
	}
}
