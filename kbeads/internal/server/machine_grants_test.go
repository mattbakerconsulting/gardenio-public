package server

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

var updateMachineGolden = flag.Bool("update", false, "rewrite testdata/machine_grants.golden.json and testdata/ownership_grants.golden.json")

// machineCtx is a classified machine principal's context, built the way
// AuthMiddleware builds it.
func machineCtx(client string, roles ...string) context.Context {
	return machineContext(context.Background(), NewFleetConfig("default", nil), machinePrincipal{ClientID: client, Roles: roles})
}

func TestAuthorizeWrite_Machine_DeniedForWrongPrincipal(t *testing.T) {
	srv, _, _ := testCtx(t)

	controller := machineCtx("gardenio-controller", "gardenio-machine-accounts")
	if d := srv.authorizeWrite(controller, "update", "account", nil); d != nil {
		t.Fatalf("controller holding gardenio-machine-accounts must update accounts, got %+v", d)
	}

	canary := machineCtx("gardenio-authz-canary")
	d := srv.authorizeWrite(canary, "update", "account", nil)
	if d == nil {
		t.Fatal("canary with no capability role must be denied update/account")
	}
	if d.RequiredRole != RoleAdmin {
		t.Errorf("RequiredRole=%q want %q", d.RequiredRole, RoleAdmin)
	}
	if d.CallerSubject != "machine:gardenio-authz-canary" {
		t.Errorf("CallerSubject=%q want machine:gardenio-authz-canary", d.CallerSubject)
	}
}

// The machine does not get the global machineAllowedWrites list: the controller
// has update/account but update/config (allow-listed for static/fleet) is denied.
func TestAuthorizeWrite_Machine_GlobalAllowListNotConsulted(t *testing.T) {
	srv, _, _ := testCtx(t)
	if !isMachineAllowedWrite("update", "config") {
		t.Fatal("precondition: update/config is in machineAllowedWrites")
	}
	controller := machineCtx("gardenio-controller", "gardenio-machine-accounts")
	if d := srv.authorizeWrite(controller, "update", "config", nil); d == nil {
		t.Fatal("machine must be denied update/config: the global list is not consulted for machines")
	}
	// Control: the static token on update/config is allowed, as today.
	if d := srv.authorizeWrite(staticCtx(), "update", "config", nil); d != nil {
		t.Fatalf("static token on update/config must stay allowed, got %+v", d)
	}
	// The static token on update/account stays denied (narrow list unchanged).
	if d := srv.authorizeWrite(staticCtx(), "update", "account", nil); d == nil {
		t.Fatal("static token on update/account must stay denied")
	}
}

// A machine token that carries gardenio-admin gets no admin anywhere.
func TestAuthorizeWrite_Machine_AdminRoleGrantsNothing(t *testing.T) {
	srv, _, _ := testCtx(t)
	ctx := machineCtx("gardenio-controller", RoleAdmin)
	if d := srv.authorizeWrite(ctx, "update", "account", nil); d == nil {
		t.Fatal("gardenio-admin on a machine token must not bypass write policy")
	}
	if d := srv.authorizeWrite(ctx, "create", "team", nil); d == nil {
		t.Fatal("gardenio-admin on a machine token must not create admin-gated types")
	}
}

// An unknown gardenio-machine-* role grants nothing.
func TestAuthorizeWrite_Machine_UnknownRoleGrantsNothing(t *testing.T) {
	srv, _, _ := testCtx(t)
	ctx := machineCtx("gardenio-controller", "gardenio-machine-not-in-the-table")
	if d := srv.authorizeWrite(ctx, "update", "account", nil); d == nil {
		t.Fatal("a role absent from the grants table must grant nothing")
	}
}

// machineGrantsServer is the full HTTP stack with a registered controller and
// canary and a real account bead.
func machineGrantsServer(t *testing.T) (*machineHarness, http.Handler, *mockStore) {
	t.Helper()
	h := newMachineHarness(t, []string{"gardenio-controller", "gardenio-authz-canary"})
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	srv.userProvisioner = nil
	srv.SetFleetAuth(h.fc)
	ms.beads["gd-acct1"] = &model.Bead{
		ID: "gd-acct1", Title: "acct", Type: "account", Kind: "config",
		Status: model.StatusOpen, Fields: json.RawMessage(`{"provider":"p","credential":"c"}`),
	}
	return h, srv.NewHTTPHandler(h.verifier, withTestToken(h.fc, machineStaticToken)), ms
}

func machineDo(t *testing.T, handler http.Handler, tok, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != nil {
		b, _ := json.Marshal(body)
		r = httptest.NewRequest(method, path, bytes.NewReader(b))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	r.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, r)
	return rec
}

// The visible-403 acceptance test: the denial reaches the wire naming the
// machine, and the denials counter moves by exactly 1 for the canary, 0 for
// the controller.
func TestMachineGrants_UpdateAccount_HTTP_DeniedForWrongPrincipal(t *testing.T) {
	h, handler, _ := machineGrantsServer(t)
	counter := authzDenialsAcrossPrincipals("update", "insufficient_role", "account")
	patch := map[string]any{"title": "renamed"}

	before := testutil.ToFloat64(counter)
	rec := machineDo(t, handler, h.saToken(t, "gardenio-controller", "gardenio-machine-accounts"), http.MethodPatch, "/v1/beads/gd-acct1", patch)
	if rec.Code != http.StatusOK {
		t.Fatalf("controller: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if d := testutil.ToFloat64(counter) - before; d != 0 {
		t.Fatalf("controller moved the denials counter by %v; want 0", d)
	}

	before = testutil.ToFloat64(counter)
	rec = machineDo(t, handler, h.saToken(t, "gardenio-authz-canary"), http.MethodPatch, "/v1/beads/gd-acct1", patch)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("canary: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body AuthzDenial
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.RequiredRole != RoleAdmin || body.CallerSubject != "machine:gardenio-authz-canary" {
		t.Fatalf("denial body = %+v; want required_role=%s caller_subject=machine:gardenio-authz-canary", body, RoleAdmin)
	}
	if d := testutil.ToFloat64(counter) - before; d != 1 {
		t.Fatalf("canary moved the denials counter by %v; want exactly 1", d)
	}
}

func TestMachineGrants_AdminRoleDoesNotOpenAdminRoutes(t *testing.T) {
	h, handler, _ := machineGrantsServer(t)
	tok := h.saToken(t, "gardenio-controller", RoleAdmin, "gardenio-machine-accounts")
	if rec := machineDo(t, handler, tok, http.MethodDelete, "/v1/beads/gd-acct1", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("DELETE /v1/beads/{id} with a machine token carrying gardenio-admin: status=%d, want 403", rec.Code)
	}
}

func TestMachineGrants_TokenMint(t *testing.T) {
	h, handler, _ := machineGrantsServer(t)
	mint := h.saToken(t, "gardenio-controller", "gardenio-machine-token-mint")
	plain := h.saToken(t, "gardenio-authz-canary")
	reg := map[string]any{"token": "fleet-tok-abc123", "bridge": "qa-bridge"} // gitleaks:allow: fake token

	if rec := machineDo(t, handler, plain, http.MethodPost, "/v1/bridge-tokens", reg); rec.Code != http.StatusForbidden {
		t.Fatalf("canary POST /v1/bridge-tokens: status=%d, want 403", rec.Code)
	}
	if rec := machineDo(t, handler, plain, http.MethodGet, "/v1/bridge-tokens", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("canary GET /v1/bridge-tokens: status=%d, want 403", rec.Code)
	}

	if rec := machineDo(t, handler, mint, http.MethodPost, "/v1/bridge-tokens", reg); rec.Code != http.StatusCreated {
		t.Fatalf("minter POST /v1/bridge-tokens: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := machineDo(t, handler, mint, http.MethodGet, "/v1/bridge-tokens", nil); rec.Code != http.StatusOK {
		t.Fatalf("minter GET /v1/bridge-tokens: status=%d body=%s", rec.Code, rec.Body.String())
	}
	// The legacy alias is the same handler.
	if rec := machineDo(t, handler, mint, http.MethodGet, "/v1/fleet-tokens", nil); rec.Code != http.StatusOK {
		t.Fatalf("minter GET /v1/fleet-tokens: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := machineDo(t, handler, mint, http.MethodDelete, "/v1/bridge-tokens/qa-bridge", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("minter DELETE /v1/bridge-tokens/qa-bridge: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// The capability is scoped to the token routes: embeddings stay admin-only.
	if rec := machineDo(t, handler, mint, http.MethodPost, "/v1/embeddings/sync-batch", map[string]any{}); rec.Code != http.StatusForbidden {
		t.Fatalf("minter POST /v1/embeddings/sync-batch: status=%d, want 403", rec.Code)
	}
}

func TestMachineHasCapability_OnlyForMachineMethod(t *testing.T) {
	// An OIDC human whose context somehow carries the machine-roles key still gets nothing.
	ctx := WithMachineRoles(withAuthMethod(context.Background(), authMethodOIDC), []string{"gardenio-machine-token-mint"})
	if machineHasCapability(ctx, capTokenMint) {
		t.Fatal("non-machine auth method must not hold machine capabilities")
	}
	if machineGrantsWrite(ctx, "update", "account") {
		t.Fatal("non-machine auth method must not hold machine write grants")
	}
	if !machineHasCapability(machineCtx("c", "gardenio-machine-token-mint"), capTokenMint) {
		t.Fatal("machine with token-mint role must hold the capability")
	}
}

// Every grants entry cites its call sites; the table must not contain a role
// outside the gardenio-machine- namespace.
func TestMachineCapabilityGrants_Namespace(t *testing.T) {
	for role := range machineCapabilityGrants {
		if !strings.HasPrefix(role, machineRolePrefix) {
			t.Errorf("role %q is outside the %s namespace", role, machineRolePrefix)
		}
	}
}

func TestMachineCapabilityGrants_Golden(t *testing.T) {
	got, err := marshalMachineGrantsGolden()
	if err != nil {
		t.Fatal(err)
	}
	const path = "testdata/machine_grants.golden.json"
	if *updateMachineGolden {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (regenerate with `go test -run TestMachineCapabilityGrants_Golden -update`): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("machineCapabilityGrants differs from %s (regenerate with -update and update the chart cross-check):\n got: %s\nwant: %s", path, got, want)
	}
}

// --- typed ownership (gd-Hc34EENbO9, decision gd-ZsfxvXRiyv "tight") -------

// The beads-viewer principal holding gardenio-machine-plan-reactor may update a
// plan EPIC it did not create (the step reactor's last_nudge_at stamp). The
// same role grants nothing on any other type, and the canary / controller get
// the ordinary ownership denial on the same bead.
func TestAuthorizeOwnership_Machine_TypedGrant(t *testing.T) {
	srv, _, _ := testCtx(t)
	epic := &model.Bead{ID: "gd-plan1", Type: model.TypeEpic, Kind: model.KindIssue, CreatedBy: "oidc:owner@example.com"}
	task := &model.Bead{ID: "gd-step1", Type: model.TypeTask, Kind: model.KindIssue, CreatedBy: "oidc:owner@example.com"}

	viewer := machineCtx("gardenio-beads-viewer", "gardenio-operator", "gardenio-machine-plan-reactor")
	if d := srv.authorizeOwnership(viewer, "update", epic); d != nil {
		t.Fatalf("viewer holding gardenio-machine-plan-reactor must update a foreign epic, got %+v", d)
	}
	d := srv.authorizeOwnership(viewer, "update", task)
	if d == nil {
		t.Fatal("the plan-reactor grant is typed to epic: a foreign task must still be denied")
	}
	if d.CallerSubject != "machine:gardenio-beads-viewer" || !d.Escalatable {
		t.Errorf("task denial = %+v; want caller_subject=machine:gardenio-beads-viewer escalatable", d)
	}

	for _, c := range []struct{ client, role string }{
		{"gardenio-authz-canary", ""},
		{"gardenio-controller", "gardenio-machine-accounts"},
	} {
		var ctx context.Context
		if c.role == "" {
			ctx = machineCtx(c.client)
		} else {
			ctx = machineCtx(c.client, c.role)
		}
		d := srv.authorizeOwnership(ctx, "update", epic)
		if d == nil {
			t.Fatalf("%s must be ownership-denied on a foreign epic", c.client)
		}
		if d.CallerSubject != "machine:"+c.client {
			t.Errorf("%s: CallerSubject=%q", c.client, d.CallerSubject)
		}
	}

	// Self-made beads need no grant: created_by match wins first.
	own := &model.Bead{ID: "gd-own", Type: model.TypeTask, Kind: model.KindIssue, CreatedBy: "machine:gardenio-authz-canary"}
	if d := srv.authorizeOwnership(machineCtx("gardenio-authz-canary"), "update", own); d != nil {
		t.Fatalf("a machine must still own what it created, got %+v", d)
	}
}

// A non-machine auth method never picks up an ownership grant, even with the
// machine-roles context key present.
func TestMachineGrantsOwnership_OnlyForMachineMethod(t *testing.T) {
	ctx := WithMachineRoles(withAuthMethod(context.Background(), authMethodOIDC), []string{"gardenio-machine-plan-reactor"})
	ctx = WithAuthIdentity(ctx, AuthIdentity{Type: "machine", Subject: "machine:v"})
	if machineTypedOwnership(ctx, model.TypeEpic) {
		t.Fatal("non-machine auth method must not hold machine ownership grants")
	}
	if !machineTypedOwnership(machineCtx("v", "gardenio-machine-plan-reactor"), model.TypeEpic) {
		t.Fatal("machine with plan-reactor role must own epics")
	}
	if machineTypedOwnership(machineCtx("v", "gardenio-machine-plan-reactor"), model.BeadType("trigger")) {
		t.Fatal("plan-reactor role must not extend to triggers")
	}
}

// machineTypedOwnership reports whether a machine-derived typed Ownership row
// (machine:<role>/ownership) admits an update by ctx on a bead of type bt that
// someone else created.
func machineTypedOwnership(ctx context.Context, bt model.BeadType) bool {
	b := &model.Bead{ID: "gd-x", Type: bt, Kind: model.KindIssue, Status: model.StatusOpen, CreatedBy: "someone-else@example.com"}
	adm, ok := (&BeadsServer{store: newMockStore()}).evaluateOwnershipGrants(ctx, "update", b)
	return ok && strings.HasPrefix(adm.grant, "machine:") && strings.HasSuffix(adm.grant, "/ownership")
}

// machineOwnershipServer is the full HTTP stack with the viewer, controller
// and canary registered and a plan epic a HUMAN created.
func machineOwnershipServer(t *testing.T) (*machineHarness, http.Handler) {
	t.Helper()
	h := newMachineHarness(t, []string{"gardenio-beads-viewer", "gardenio-controller", "gardenio-authz-canary"})
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	srv.userProvisioner = nil
	srv.SetFleetAuth(h.fc)
	ms.beads["gd-plan1"] = &model.Bead{
		ID: "gd-plan1", Title: "Ship the widget", Type: model.TypeEpic, Kind: model.KindIssue,
		Status: model.StatusOpen, CreatedBy: "oidc:owner@example.com", OperatedBy: "oidc:owner@example.com",
		Labels: []string{"project:gardenio", "plan"},
		Fields: json.RawMessage(`{}`),
	}
	return h, srv.NewHTTPHandler(h.verifier, withTestToken(h.fc, machineStaticToken))
}

// The visible-403 acceptance test for the viewer's grant: the viewer stamps
// last_nudge_at on a human's plan epic; the canary and the controller are
// refused with the ownership denial naming them, and the denials counter moves
// by exactly 1 per refused principal and 0 for the viewer.
func TestMachineGrants_PlanReactorOwnership_HTTP_DeniedForWrongPrincipal(t *testing.T) {
	h, handler := machineOwnershipServer(t)
	// An ownership denial is Escalatable, so it lands under escalation_required.
	counter := authzDenialsAcrossPrincipals("update", reasonEscalationRequired, "epic")
	patch := map[string]any{"fields": map[string]any{"last_nudge_at": "2026-10-14T00:00:00Z"}}

	before := testutil.ToFloat64(counter)
	rec := machineDo(t, handler, h.saToken(t, "gardenio-beads-viewer", "gardenio-operator", "gardenio-machine-plan-reactor"), http.MethodPatch, "/v1/beads/gd-plan1", patch)
	if rec.Code != http.StatusOK {
		t.Fatalf("viewer: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if d := testutil.ToFloat64(counter) - before; d != 0 {
		t.Fatalf("viewer moved the denials counter by %v; want 0", d)
	}

	for _, c := range []struct {
		client string
		roles  []string
	}{
		{"gardenio-authz-canary", []string{"gardenio-operator"}},
		{"gardenio-controller", []string{"gardenio-operator", "gardenio-machine-accounts", "gardenio-machine-token-mint"}},
	} {
		before = testutil.ToFloat64(counter)
		rec = machineDo(t, handler, h.saToken(t, c.client, c.roles...), http.MethodPatch, "/v1/beads/gd-plan1", patch)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s: status=%d body=%s", c.client, rec.Code, rec.Body.String())
		}
		var body AuthzDenial
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if body.CallerSubject != "machine:"+c.client || body.ResourceCreatedBy != "oidc:owner@example.com" {
			t.Fatalf("%s denial body = %+v; want caller_subject=machine:%s naming the epic's creator", c.client, body, c.client)
		}
		if d := testutil.ToFloat64(counter) - before; d != 1 {
			t.Fatalf("%s moved the denials counter by %v; want exactly 1", c.client, d)
		}
	}
}

// The controller's plan spend sync stamps plan_spend_* on a plan epic a human
// filed (gd-Hk70sS1XuS). Its FieldOwnership grant admits exactly that patch, on
// open and closed plans, and nothing else: another epic field, a column, a
// close, or the same patch on a task stays an ownership 403. Before the grant
// every stamp was refused and the sync re-sent it each 10-minute pass.
func TestMachineGrants_ControllerPlanSpendStamp_HTTP(t *testing.T) {
	h, srv := machineOwnershipServer(t)
	controller := h.saToken(t, "gardenio-controller", "gardenio-operator", "gardenio-machine-controller-ownership")
	counter := authzDenialsAcrossPrincipals("update", reasonEscalationRequired, "epic")
	stamp := map[string]any{"fields": map[string]any{
		"plan_spend_usd": "12.34", "plan_spend_synced_at": "2026-10-30T17:00:00Z", "plan_spend_crew": 3, "plan_spend_live": 1,
	}}

	before := testutil.ToFloat64(counter)
	rec := machineDo(t, srv, controller, http.MethodPatch, "/v1/beads/gd-plan1", stamp)
	if rec.Code != http.StatusOK {
		t.Fatalf("plan spend stamp: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if d := testutil.ToFloat64(counter) - before; d != 0 {
		t.Fatalf("the admitted stamp moved the denials counter by %v", d)
	}

	// A closing PATCH is authorized (and counted) as action "close".
	for _, c := range []struct {
		name, action string
		patch        map[string]any
	}{
		{"another epic field", "update", map[string]any{"fields": map[string]any{"plan_spend_usd": "99.00", "last_nudge_at": "2026-10-30T17:00:00Z"}}},
		{"a column", "update", map[string]any{"title": "Renamed", "fields": map[string]any{"plan_spend_usd": "99.00"}}},
		{"a close", "close", map[string]any{"status": "closed", "fields": map[string]any{"plan_spend_usd": "99.00"}}},
	} {
		denials := authzDenialsAcrossPrincipals(c.action, reasonEscalationRequired, "epic")
		before = testutil.ToFloat64(denials)
		rec = machineDo(t, srv, controller, http.MethodPatch, "/v1/beads/gd-plan1", c.patch)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s: status=%d body=%s; want 403", c.name, rec.Code, rec.Body.String())
		}
		if d := testutil.ToFloat64(denials) - before; d != 1 {
			t.Fatalf("%s moved the %s denials counter by %v; want 1", c.name, c.action, d)
		}
	}

	// A principal without the role is still refused the same stamp.
	canary := h.saToken(t, "gardenio-authz-canary", "gardenio-operator")
	if rec = machineDo(t, srv, canary, http.MethodPatch, "/v1/beads/gd-plan1", stamp); rec.Code != http.StatusForbidden {
		t.Fatalf("canary: status=%d body=%s; want 403", rec.Code, rec.Body.String())
	}
}

// The grant is typed to epics: the same field patch on a bead of another type
// the controller holds no ownership of is still refused.
func TestMachineMayStampFields_TypedToEpic(t *testing.T) {
	controller := machineCtx("gardenio-controller", "gardenio-machine-controller-ownership")
	in := &updateBeadInput{Fields: json.RawMessage(`{"plan_spend_usd":"1.00"}`)}
	if !machineMayStampFields(controller, &model.Bead{Type: model.TypeEpic, Fields: json.RawMessage(`{}`)}, in) {
		t.Fatal("controller must stamp plan_spend_usd on an epic")
	}
	if machineMayStampFields(controller, &model.Bead{Type: model.TypeBug, Fields: json.RawMessage(`{}`)}, in) {
		t.Fatal("the plan spend grant must not extend to bugs")
	}
	if machineMayStampFields(machineCtx("gardenio-controller", "gardenio-machine-accounts"), &model.Bead{Type: model.TypeEpic, Fields: json.RawMessage(`{}`)}, in) {
		t.Fatal("a role without FieldOwnership must not stamp")
	}
	// A no-op patch buys nothing.
	if machineMayStampFields(controller, &model.Bead{Type: model.TypeEpic, Fields: json.RawMessage(`{"plan_spend_usd":"1.00"}`)}, in) {
		t.Fatal("an unchanged stamp must not qualify")
	}
}

// The step reactor's wake of a PARKED plan gardener (planagent Engine.Wake:
// PATCH status=in_progress + prompt/session_resume on the agent bead) needs no
// grant beyond the plan-reactor role's epic ownership: type:agent is
// kind=config, which authorizeOwnership exempts, and its write policy has no
// min_role, so authorizeWrite admits the machine. This pins that the viewer's
// machine principal still wakes an agent bead a human created (and that the
// wake is not counted as a denial), so a future type:agent policy tightening
// that would silently break parked-plan wakes shows up here.
func TestMachineGrants_PlanReactorWake_HTTP_ParkedAgent(t *testing.T) {
	h := newMachineHarness(t, []string{"gardenio-beads-viewer"})
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	srv.userProvisioner = nil
	srv.SetFleetAuth(h.fc)
	ms.beads["gd-asst1"] = &model.Bead{
		ID: "gd-asst1", Title: "gardenio-plan-asst-1", Type: model.BeadType("agent"), Kind: model.KindConfig,
		Status: model.StatusClosed, CreatedBy: "oidc:owner@example.com", OperatedBy: "oidc:owner@example.com",
		Labels: []string{"project:gardenio"},
		Fields: json.RawMessage(`{"agent":"gardenio-plan-asst-1","role":"crew","project":"gardenio","agent_state":"done"}`),
	}
	handler := srv.NewHTTPHandler(h.verifier, withTestToken(h.fc, machineStaticToken))
	escCounter := authzDenialsAcrossPrincipals("update", reasonEscalationRequired, "agent")
	roleCounter := authzDenialsAcrossPrincipals("update", "insufficient_role", "agent")
	escBefore, roleBefore := testutil.ToFloat64(escCounter), testutil.ToFloat64(roleCounter)

	wake := map[string]any{
		"status": "in_progress",
		"fields": map[string]any{"prompt": "I added a new step", "session_resume": "true", "stop_requested": "", "agent_state": nil},
	}
	rec := machineDo(t, handler, h.saToken(t, "gardenio-beads-viewer", "gardenio-operator", "gardenio-machine-plan-reactor"), http.MethodPatch, "/v1/beads/gd-asst1", wake)
	if rec.Code != http.StatusOK {
		t.Fatalf("viewer wake: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := ms.beads["gd-asst1"].Status; got != model.StatusInProgress {
		t.Fatalf("wake did not reopen the agent bead: status=%q", got)
	}
	if testutil.ToFloat64(escCounter) != escBefore || testutil.ToFloat64(roleCounter) != roleBefore {
		t.Fatal("the wake was counted as an authz denial")
	}
}

// --- grafana-bridge (gd-Hc34EENbO9 component 3) -------------------------------

// config-seed: the grafana-bridge principal creates/updates config beads
// (EnsureConfigs); the canary and the viewer's principal are denied, and the
// grant stops at config (no other admin-gated type).
func TestAuthorizeWrite_Machine_ConfigSeed(t *testing.T) {
	srv, _, _ := testCtx(t)
	grafana := machineCtx("gardenio-grafana-bridge", "gardenio-operator", "gardenio-machine-config-seed", "gardenio-machine-dashboard-sync")
	for _, action := range []string{"create", "update"} {
		for _, title := range []string{"type:dashboard"} {
			if d := srv.authorizeWriteTitled(grafana, action, "config", nil, title); d != nil {
				t.Fatalf("grafana-bridge holding config-seed must %s config %q, got %+v", action, title, d)
			}
		}
		// Typed to the titles the bridge seeds: the configs that put commands,
		// settings or credentials into every agent pod are out of reach.
		for _, title := range []string{
			// prerun runs via `sh -c` in every agent pod; context (labels
			// role:<r>) and claude-instructions are any role's system prompt;
			// nudge-prompts is every agent's nudge text. Seeded by the other
			// bridges on their own tokens, never by grafana-bridge.
			"prerun", "context", "claude-instructions", "nudge-prompts",
			"claude-hooks", "claude-settings", "claude-mcp", "context:x", "auth",
			// Other bridges' types and views.
			"type:agent", "type:task", "type:view", "view:agents:active", "view:x",
			"type:", "type:dashboard2", "view", "",
		} {
			d := srv.authorizeWriteTitled(grafana, action, "config", nil, title)
			if d == nil {
				t.Fatalf("config-seed must not %s config %q", action, title)
			}
			if d.CallerSubject != "machine:gardenio-grafana-bridge" {
				t.Errorf("%q: CallerSubject=%q", title, d.CallerSubject)
			}
		}
	}
	// An untitled caller (authorizeWrite) gets no config grant: fail closed.
	if d := srv.authorizeWrite(grafana, "update", "config", nil); d == nil {
		t.Fatal("authorizeWrite without a title must not admit a machine config write")
	}
	if d := srv.authorizeWriteTitled(grafana, "close", "config", nil, "type:dashboard"); d == nil {
		t.Fatal("config-seed must not close a config")
	}
	if d := srv.authorizeWrite(grafana, "update", "account", nil); d == nil {
		t.Fatal("config-seed must not reach update/account")
	}
	if d := srv.authorizeWrite(grafana, "create", "advice", nil); d == nil {
		t.Fatal("config-seed must not reach create/advice")
	}
	for _, c := range [][]string{
		{"gardenio-authz-canary"},
		{"gardenio-beads-viewer", "gardenio-machine-plan-reactor"},
		{"gardenio-controller", "gardenio-machine-accounts"},
	} {
		d := srv.authorizeWriteTitled(machineCtx(c[0], c[1:]...), "update", "config", nil, "nudge-prompts")
		if d == nil {
			t.Fatalf("%s must be denied update/config", c[0])
		}
		if d.CallerSubject != "machine:"+c[0] {
			t.Errorf("%s: CallerSubject=%q", c[0], d.CallerSubject)
		}
	}
}

// dashboard-sync: typed ownership of dashboard and doc beads, nothing else.
func TestAuthorizeOwnership_Machine_DashboardSync(t *testing.T) {
	srv, _, _ := testCtx(t)
	grafana := machineCtx("gardenio-grafana-bridge", "gardenio-machine-config-seed", "gardenio-machine-dashboard-sync")
	dash := &model.Bead{ID: "gd-x", Type: "dashboard", Kind: model.KindIssue, CreatedBy: "oidc:owner@example.com"}
	if d := srv.authorizeOwnership(grafana, "update", dash); d != nil {
		t.Fatalf("grafana-bridge must update a foreign dashboard, got %+v", d)
	}
	if d := srv.authorizeOwnership(machineCtx("gardenio-authz-canary"), "update", dash); d == nil {
		t.Fatal("canary must be ownership-denied on a foreign dashboard")
	}
	// doc ownership is tied to the dashboard-model label and to update only.
	legacy := &model.Bead{ID: "gd-doc", Type: "doc", Kind: model.KindIssue, CreatedBy: "static", Labels: []string{"grafana:dashboard-model"}}
	if d := srv.authorizeOwnership(grafana, "update", legacy); d != nil {
		t.Fatalf("a model doc created under the old static identity must stay writable, got %+v", d)
	}
	if d := srv.authorizeOwnership(machineCtx("gardenio-authz-canary"), "update", legacy); d == nil {
		t.Fatal("canary must be ownership-denied on a model doc")
	}
	if d := srv.authorizeOwnership(grafana, "close", legacy); d == nil {
		t.Fatal("dashboard-sync must not close a model doc")
	}
	for _, labels := range [][]string{nil, {"plan"}, {"grafana:dashboard-conflict"}} {
		other := &model.Bead{ID: "gd-plan-doc", Type: "doc", Kind: model.KindIssue, CreatedBy: "oidc:owner@example.com", Labels: labels}
		if d := srv.authorizeOwnership(grafana, "update", other); d == nil {
			t.Fatalf("dashboard-sync must not update a doc labeled %v", labels)
		}
	}
	epic := &model.Bead{ID: "gd-plan", Type: model.TypeEpic, Kind: model.KindIssue, CreatedBy: "oidc:owner@example.com"}
	if d := srv.authorizeOwnership(grafana, "update", epic); d == nil {
		t.Fatal("dashboard-sync must not extend to epics")
	}
}

// HTTP acceptance for config-seed: PUT-less path — a PATCH on an existing
// config bead by the grafana-bridge principal succeeds; the canary gets the
// insufficient-role 403 naming it, and the counter moves for the canary only.
func TestMachineGrants_ConfigSeed_HTTP_DeniedForWrongPrincipal(t *testing.T) {
	h := newMachineHarness(t, []string{"gardenio-grafana-bridge", "gardenio-authz-canary"})
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	srv.userProvisioner = nil
	srv.SetFleetAuth(h.fc)
	ms.beads["gd-cfg1"] = &model.Bead{
		ID: "gd-cfg1", Title: "type:dashboard", Type: "config", Kind: "config",
		Status: model.StatusOpen, CreatedBy: "static", Fields: json.RawMessage(`{}`),
	}
	handler := srv.NewHTTPHandler(h.verifier, withTestToken(h.fc, machineStaticToken))
	counter := authzDenialsAcrossPrincipals("update", reasonInsufficientRole, "custom")
	patch := map[string]any{"description": "refreshed"}

	before := testutil.ToFloat64(counter)
	rec := machineDo(t, handler, h.saToken(t, "gardenio-grafana-bridge", "gardenio-operator", "gardenio-machine-config-seed"), http.MethodPatch, "/v1/beads/gd-cfg1", patch)
	if rec.Code != http.StatusOK {
		t.Fatalf("grafana-bridge: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if d := testutil.ToFloat64(counter) - before; d != 0 {
		t.Fatalf("grafana-bridge moved the denials counter by %v; want 0", d)
	}
	before = testutil.ToFloat64(counter)
	rec = machineDo(t, handler, h.saToken(t, "gardenio-authz-canary", "gardenio-operator"), http.MethodPatch, "/v1/beads/gd-cfg1", patch)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("canary: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body AuthzDenial
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.CallerSubject != "machine:gardenio-authz-canary" || body.RequiredRole != RoleAdmin {
		t.Fatalf("denial body = %+v; want caller_subject=machine:gardenio-authz-canary required_role=%s", body, RoleAdmin)
	}
	if d := testutil.ToFloat64(counter) - before; d != 1 {
		t.Fatalf("canary moved the denials counter by %v; want exactly 1", d)
	}

	// The grant is typed to the bridge's own config titles: under the SAME
	// principal, writes to the configs that reach every agent pod are 403.
	for _, title := range []string{"prerun", "context", "claude-instructions", "nudge-prompts", "claude-hooks", "context:x", "auth"} {
		// context/claude-instructions are role-labelled in the store; the
		// title alone must be enough to refuse them.
		ms.beads["gd-"+title] = &model.Bead{
			ID: "gd-" + title, Title: title, Type: "config", Kind: "config",
			Labels: []string{"role:infra"},
			Status: model.StatusOpen, CreatedBy: "static", Fields: json.RawMessage(`{}`),
		}
		tok := h.saToken(t, "gardenio-grafana-bridge", "gardenio-operator", "gardenio-machine-config-seed", "gardenio-machine-dashboard-sync")
		rec = machineDo(t, handler, tok, http.MethodPatch, "/v1/beads/gd-"+title, patch)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("PATCH config %q: status=%d body=%s; want 403", title, rec.Code, rec.Body.String())
		}
		body = AuthzDenial{}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if body.CallerSubject != "machine:gardenio-grafana-bridge" {
			t.Fatalf("PATCH config %q: caller_subject=%q", title, body.CallerSubject)
		}
		rec = machineDo(t, handler, tok, http.MethodPost, "/v1/beads", map[string]any{"title": title, "type": "config", "kind": "config", "description": "{}", "labels": []string{"role:infra"}})
		if rec.Code != http.StatusForbidden {
			t.Fatalf("create config %q: status=%d body=%s; want 403", title, rec.Code, rec.Body.String())
		}
	}
	// Renaming a permitted config into a forbidden title is refused too.
	for _, to := range []string{"claude-hooks", "prerun"} {
		rec = machineDo(t, handler, h.saToken(t, "gardenio-grafana-bridge", "gardenio-operator", "gardenio-machine-config-seed"), http.MethodPatch, "/v1/beads/gd-cfg1", map[string]any{"title": to})
		if rec.Code != http.StatusForbidden {
			t.Fatalf("rename to %s: status=%d body=%s; want 403", to, rec.Code, rec.Body.String())
		}
	}
}

// --- trigger-engine (gd-Hc34EENbO9 component 2, decisions gd-ZsfxvXRiyv
// "tight" + gd-dI7PfsFlvr "lineage") ------------------------------------------

// gardenio-machine-trigger-ops owns trigger beads and the run work items
// (task/bug/feature/chore) others authored, and may UPDATE the one rollup doc
// it marks; it grants nothing on an epic, an unmarked doc, or a close of that
// doc, and no other machine principal gets any of it.
func TestAuthorizeOwnership_Machine_TriggerOps(t *testing.T) {
	srv, _, _ := testCtx(t)
	engine := machineCtx("gardenio-trigger-engine", "gardenio-operator", "gardenio-machine-trigger-ops")
	foreign := func(id string, typ model.BeadType, kind model.Kind, labels ...string) *model.Bead {
		return &model.Bead{ID: id, Type: typ, Kind: kind, CreatedBy: "oidc:owner@example.com", Labels: labels}
	}

	allowed := []struct {
		action string
		bead   *model.Bead
	}{
		{"update", foreign("gd-t1", "trigger", model.KindData)},
		{"close", foreign("gd-t1", "trigger", model.KindData)},
		{"update", foreign("gd-run1", model.TypeTask, model.KindIssue)},
		{"close", foreign("gd-run2", model.TypeBug, model.KindIssue)},
		{"close", foreign("gd-run3", model.TypeFeature, model.KindIssue)},
		{"close", foreign("gd-run4", model.TypeChore, model.KindIssue)},
		{"update", foreign("gd-rollup", "doc", model.KindData, "project:gardenio", "uncovered-open-lane-rollup")},
	}
	for _, c := range allowed {
		if d := srv.authorizeOwnership(engine, c.action, c.bead); d != nil {
			t.Errorf("trigger-engine must %s a foreign %s, got %+v", c.action, c.bead.Type, d)
		}
	}
	denied := []struct {
		action string
		bead   *model.Bead
	}{
		{"update", foreign("gd-epic", model.TypeEpic, model.KindIssue)},
		{"update", foreign("gd-doc", "doc", model.KindData, "project:gardenio")},
		{"close", foreign("gd-rollup", "doc", model.KindData, "uncovered-open-lane-rollup")},
		{"update", foreign("gd-dash", "dashboard", model.KindData)},
	}
	for _, c := range denied {
		d := srv.authorizeOwnership(engine, c.action, c.bead)
		if d == nil {
			t.Errorf("trigger-engine must be denied %s on a foreign %s %v", c.action, c.bead.Type, c.bead.Labels)
			continue
		}
		if d.CallerSubject != "machine:gardenio-trigger-engine" || !d.Escalatable {
			t.Errorf("%s %s denial = %+v", c.action, c.bead.Type, d)
		}
	}

	trig := foreign("gd-t1", "trigger", model.KindData)
	for _, c := range []struct {
		client string
		roles  []string
	}{
		{"gardenio-authz-canary", nil},
		{"gardenio-controller", []string{"gardenio-machine-accounts", "gardenio-machine-token-mint"}},
		{"gardenio-grafana-bridge", []string{"gardenio-machine-config-seed", "gardenio-machine-dashboard-sync"}},
	} {
		d := srv.authorizeOwnership(machineCtx(c.client, c.roles...), "update", trig)
		if d == nil {
			t.Fatalf("%s must be ownership-denied on a foreign trigger", c.client)
		}
		if d.CallerSubject != "machine:"+c.client {
			t.Errorf("%s: CallerSubject=%q", c.client, d.CallerSubject)
		}
	}
	// The role grants no admin-gated write: it is ownership + lineage only.
	if d := srv.authorizeWrite(engine, "update", "account", nil); d == nil {
		t.Fatal("trigger-ops must not grant update/account")
	}
	if d := srv.authorizeWrite(engine, "create", "config", nil); d == nil {
		t.Fatal("trigger-ops must not grant create/config")
	}
}

// The visible-403 acceptance test for the trigger-engine's grant (the live
// canary check in the escalation): the engine stamps a gardener field on a
// human's trigger bead; the canary and the controller are refused with the
// ownership denial naming them, and the denials counter moves by exactly 1
// per refused principal and 0 for the engine.
func TestMachineGrants_TriggerOps_HTTP_DeniedForWrongPrincipal(t *testing.T) {
	h := newMachineHarness(t, []string{"gardenio-trigger-engine", "gardenio-controller", "gardenio-authz-canary"})
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	srv.userProvisioner = nil
	srv.SetFleetAuth(h.fc)
	ms.beads["gd-trig1"] = &model.Bead{
		ID: "gd-trig1", Title: "nightly sweep", Type: "trigger", Kind: model.KindData,
		Status: model.StatusOpen, CreatedBy: "oidc:owner@example.com", OperatedBy: "oidc:owner@example.com",
		Labels: []string{"project:gardenio"},
		Fields: json.RawMessage(`{"action":"spawn_agent","source":"schedule"}`),
	}
	handler := srv.NewHTTPHandler(h.verifier, withTestToken(h.fc, machineStaticToken))
	counter := authzDenialsAcrossPrincipals("update", reasonEscalationRequired, "trigger")
	// consecutive_failures is NOT a service-managed field, so the patch goes
	// through the ownership gate (IB/triggers.go:2418 stampFireError).
	patch := map[string]any{"fields": map[string]any{"action": "spawn_agent", "source": "schedule", "consecutive_failures": 0}}

	before := testutil.ToFloat64(counter)
	rec := machineDo(t, handler, h.saToken(t, "gardenio-trigger-engine", "gardenio-operator", "gardenio-machine-trigger-ops"), http.MethodPatch, "/v1/beads/gd-trig1", patch)
	if rec.Code != http.StatusOK {
		t.Fatalf("trigger-engine: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if d := testutil.ToFloat64(counter) - before; d != 0 {
		t.Fatalf("trigger-engine moved the denials counter by %v; want 0", d)
	}

	for _, c := range []struct {
		client string
		roles  []string
	}{
		{"gardenio-authz-canary", []string{"gardenio-operator"}},
		{"gardenio-controller", []string{"gardenio-operator", "gardenio-machine-accounts", "gardenio-machine-token-mint"}},
	} {
		before = testutil.ToFloat64(counter)
		rec = machineDo(t, handler, h.saToken(t, c.client, c.roles...), http.MethodPatch, "/v1/beads/gd-trig1", patch)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s: status=%d body=%s", c.client, rec.Code, rec.Body.String())
		}
		var body AuthzDenial
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if body.CallerSubject != "machine:"+c.client || body.ResourceCreatedBy != "oidc:owner@example.com" {
			t.Fatalf("%s denial body = %+v; want caller_subject=machine:%s naming the trigger's creator", c.client, body, c.client)
		}
		if d := testutil.ToFloat64(counter) - before; d != 1 {
			t.Fatalf("%s moved the denials counter by %v; want exactly 1", c.client, d)
		}
	}
}

// --- slack-bridge (gd-Hc34EENbO9 component 4, decision gd-ZsfxvXRiyv "tight,
// amended": the bridge's own background writes only, no relay) -----------------

// slack-config-seed: the slack-bridge principal creates/updates exactly the
// config titles it seeds; every other title (the agent-facing configs and the
// write-policy types) is out of reach, and so is every other machine principal.
func TestAuthorizeWrite_Machine_SlackConfigSeed(t *testing.T) {
	srv, _, _ := testCtx(t)
	slack := machineCtx("gardenio-slack-bridge", "gardenio-operator", "gardenio-machine-slack-config-seed", "gardenio-machine-slack-dm-delivery")
	for _, action := range []string{"create", "update"} {
		for _, title := range machineSlackConfigSeedTitles {
			if d := srv.authorizeWriteTitled(slack, action, "config", nil, title); d != nil {
				t.Fatalf("slack-bridge holding slack-config-seed must %s config %q, got %+v", action, title, d)
			}
		}
		for _, title := range []string{
			"prerun", "context", "claude-instructions", "nudge-prompts",
			"claude-hooks", "claude-settings", "claude-mcp", "context:x", "auth",
			// Write-policy and identity types: weakening one weakens every gate.
			"type:role", "type:agent", "type:project", "type:user", "type:account",
			"type:trigger", "type:schedule", "type:task", "type:view", "type:dashboard",
			"view:agents:active", "view:x", "type:", "type:dm2", "view", "",
		} {
			d := srv.authorizeWriteTitled(slack, action, "config", nil, title)
			if d == nil {
				t.Fatalf("slack-config-seed must not %s config %q", action, title)
			}
			if d.CallerSubject != "machine:gardenio-slack-bridge" {
				t.Errorf("%q: CallerSubject=%q", title, d.CallerSubject)
			}
		}
	}
	if d := srv.authorizeWriteTitled(slack, "close", "config", nil, "type:dm"); d == nil {
		t.Fatal("slack-config-seed must not close a config")
	}
	for _, typ := range []model.BeadType{"account", "advice", "project", "user"} {
		if d := srv.authorizeWrite(slack, "update", typ, nil); d == nil {
			t.Fatalf("the slack roles must not reach update/%s", typ)
		}
	}
	for _, c := range [][]string{
		{"gardenio-authz-canary"},
		{"gardenio-grafana-bridge", "gardenio-machine-config-seed", "gardenio-machine-dashboard-sync"},
		{"gardenio-trigger-engine", "gardenio-machine-trigger-ops"},
		{"gardenio-controller", "gardenio-machine-accounts"},
	} {
		d := srv.authorizeWriteTitled(machineCtx(c[0], c[1:]...), "update", "config", nil, "type:dm")
		if d == nil {
			t.Fatalf("%s must be denied update/config type:dm", c[0])
		}
		if d.CallerSubject != "machine:"+c[0] {
			t.Errorf("%s: CallerSubject=%q", c[0], d.CallerSubject)
		}
	}
}

// slack-dm-delivery: ownership typed to dm, the machine counterpart of the
// fleet-only dm carve-out. Nothing else: not a thread task, not an epic, and no
// other machine principal.
func TestAuthorizeOwnership_Machine_SlackDMDelivery(t *testing.T) {
	srv, _, _ := testCtx(t)
	slack := machineCtx("gardenio-slack-bridge", "gardenio-operator", "gardenio-machine-slack-config-seed", "gardenio-machine-slack-dm-delivery")
	foreign := func(id string, typ model.BeadType) *model.Bead {
		return &model.Bead{ID: id, Type: typ, Kind: model.KindIssue, CreatedBy: "agent:someone"}
	}
	for _, action := range []string{"update", "close"} {
		if d := srv.authorizeOwnership(slack, action, foreign("gd-dm1", "dm")); d != nil {
			t.Errorf("slack-bridge must %s a foreign dm, got %+v", action, d)
		}
	}
	for _, typ := range []model.BeadType{model.TypeTask, model.TypeEpic, model.TypeBug, "trigger", "dashboard", "message", "mail", "agent-feedback"} {
		d := srv.authorizeOwnership(slack, "update", foreign("gd-x", typ))
		if d == nil {
			t.Errorf("slack-dm-delivery must not extend to a foreign %s", typ)
			continue
		}
		if d.CallerSubject != "machine:gardenio-slack-bridge" {
			t.Errorf("%s: CallerSubject=%q", typ, d.CallerSubject)
		}
	}
	// Config alone (no dm role) does not grant dm ownership.
	if d := srv.authorizeOwnership(machineCtx("gardenio-slack-bridge", "gardenio-machine-slack-config-seed"), "update", foreign("gd-dm1", "dm")); d == nil {
		t.Fatal("slack-config-seed must not grant dm ownership")
	}
	for _, c := range [][]string{
		{"gardenio-authz-canary"},
		{"gardenio-controller", "gardenio-machine-accounts", "gardenio-machine-token-mint"},
		{"gardenio-beads-viewer", "gardenio-machine-plan-reactor"},
		{"gardenio-trigger-engine", "gardenio-machine-trigger-ops"},
	} {
		d := srv.authorizeOwnership(machineCtx(c[0], c[1:]...), "update", foreign("gd-dm1", "dm"))
		if d == nil {
			t.Fatalf("%s must be ownership-denied on a foreign dm", c[0])
		}
		if d.CallerSubject != "machine:"+c[0] {
			t.Errorf("%s: CallerSubject=%q", c[0], d.CallerSubject)
		}
	}
	// The role carries no relay: the grant table has no header-trust capability
	// for the bridge (decision gd-ZsfxvXRiyv, "NO relay capability"). Its only
	// admin-gated writes are the permission records (gd-RuI4Ze6vAg).
	for _, role := range []string{"gardenio-machine-slack-config-seed", "gardenio-machine-slack-dm-delivery"} {
		g := machineCapabilityGrants[role]
		if g.TriggerLineage || g.TokenMint {
			t.Errorf("%s grants more than config titles / delivery records: %+v", role, g)
		}
		for _, w := range g.Writes {
			if w.BeadType != typePermission {
				t.Errorf("%s grants %s/%s; only permission writes belong here", role, w.Action, w.BeadType)
			}
		}
	}
}

// The visible-403 acceptance test for the slack-bridge's grants (the live
// canary check in the escalation): the bridge records a reply on an
// agent-created dm bead; the canary and the controller are refused with the
// ownership denial naming them, and the denials counter moves by exactly 1 per
// refused principal and 0 for the bridge.
func TestMachineGrants_SlackDMDelivery_HTTP_DeniedForWrongPrincipal(t *testing.T) {
	h := newMachineHarness(t, []string{"gardenio-slack-bridge", "gardenio-controller", "gardenio-authz-canary"})
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	srv.userProvisioner = nil
	srv.SetFleetAuth(h.fc)
	ms.beads["gd-dm1"] = &model.Bead{
		ID: "gd-dm1", Title: "question for you", Type: "dm", Kind: model.KindIssue,
		Status: model.StatusOpen, CreatedBy: "agent:someone", OperatedBy: "oidc:owner@example.com",
		Fields: json.RawMessage(`{"target_user":"U123"}`),
	}
	handler := srv.NewHTTPHandler(h.verifier, withTestToken(h.fc, machineStaticToken))
	counter := authzDenialsAcrossPrincipals("update", reasonEscalationRequired, authzBeadTypeLabel("dm"))
	patch := map[string]any{"fields": map[string]any{"target_user": "U123", "dm_channel": "D1", "dm_ts": "1.2"}}

	before := testutil.ToFloat64(counter)
	rec := machineDo(t, handler, h.saToken(t, "gardenio-slack-bridge", "gardenio-operator", "gardenio-machine-slack-dm-delivery"), http.MethodPatch, "/v1/beads/gd-dm1", patch)
	if rec.Code != http.StatusOK {
		t.Fatalf("slack-bridge: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if d := testutil.ToFloat64(counter) - before; d != 0 {
		t.Fatalf("slack-bridge moved the denials counter by %v; want 0", d)
	}

	for _, c := range []struct {
		client string
		roles  []string
	}{
		{"gardenio-authz-canary", []string{"gardenio-operator"}},
		{"gardenio-controller", []string{"gardenio-operator", "gardenio-machine-accounts", "gardenio-machine-token-mint"}},
	} {
		before = testutil.ToFloat64(counter)
		rec = machineDo(t, handler, h.saToken(t, c.client, c.roles...), http.MethodPatch, "/v1/beads/gd-dm1", patch)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s: status=%d body=%s", c.client, rec.Code, rec.Body.String())
		}
		var body AuthzDenial
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if body.CallerSubject != "machine:"+c.client || body.ResourceCreatedBy != "agent:someone" {
			t.Fatalf("%s denial body = %+v; want caller_subject=machine:%s naming the dm's creator", c.client, body, c.client)
		}
		if d := testutil.ToFloat64(counter) - before; d != 1 {
			t.Fatalf("%s moved the denials counter by %v; want exactly 1", c.client, d)
		}
	}
}

// --- controller default client (gd-BDirD4F7Iq, stage 3a of gd-5QYwXtV7IA) -----

// gardenio-machine-controller-ownership replaces the static-token ownership
// bypass for the controller's reaper/sweeper/settle writes, typed to
// task/advice/session/trigger, and grants close/advice as a write. It grants
// nothing on an epic, bug or dm, no account/config write, and no other
// machine principal gets any of it.
func TestAuthorizeOwnership_Machine_ControllerOwnership(t *testing.T) {
	srv, _, _ := testCtx(t)
	ctrl := machineCtx("gardenio-controller", "gardenio-operator", "gardenio-machine-controller-ownership")
	foreign := func(id string, typ model.BeadType, kind model.Kind) *model.Bead {
		return &model.Bead{ID: id, Type: typ, Kind: kind, CreatedBy: "static"}
	}

	for _, c := range []struct {
		action string
		bead   *model.Bead
	}{
		{"close", foreign("gd-t1", model.TypeTask, model.KindIssue)},
		{"update", foreign("gd-t2", model.TypeTask, model.KindIssue)},
		{"close", foreign("gd-a1", "advice", model.KindData)},
		{"update", foreign("gd-s1", "session", model.KindData)},
		{"update", foreign("gd-tr1", "trigger", model.KindData)},
	} {
		if d := srv.authorizeOwnership(ctrl, c.action, c.bead); d != nil {
			t.Errorf("controller must %s a foreign %s, got %+v", c.action, c.bead.Type, d)
		}
	}
	for _, c := range []struct {
		action string
		bead   *model.Bead
	}{
		{"update", foreign("gd-e1", model.TypeEpic, model.KindIssue)},
		{"close", foreign("gd-b1", model.TypeBug, model.KindIssue)},
		{"update", foreign("gd-dm1", "dm", model.KindData)},
	} {
		d := srv.authorizeOwnership(ctrl, c.action, c.bead)
		if d == nil {
			t.Errorf("controller must be denied %s on a foreign %s", c.action, c.bead.Type)
			continue
		}
		if d.CallerSubject != "machine:gardenio-controller" {
			t.Errorf("%s %s denial CallerSubject=%q", c.action, c.bead.Type, d.CallerSubject)
		}
	}

	// close/advice is a Writes grant: advice is admin-gated and machine
	// principals do not consult machineAllowedWrites. Nothing else is.
	if d := srv.authorizeWrite(ctrl, "close", "advice", nil); d != nil {
		t.Errorf("controller must close advice, got %+v", d)
	}
	for _, c := range []struct{ action, typ string }{
		{"create", "advice"}, {"update", "advice"}, {"update", "account"}, {"create", "project"},
	} {
		if d := srv.authorizeWrite(ctrl, c.action, model.BeadType(c.typ), nil); d == nil {
			t.Errorf("controller-ownership must not grant %s/%s", c.action, c.typ)
		}
	}

	// A machine client without the role gets none of it.
	for _, client := range []string{"gardenio-trigger-engine", "gardenio-authz-canary"} {
		if d := srv.authorizeOwnership(machineCtx(client, "gardenio-operator"), "close", foreign("gd-t1", model.TypeTask, model.KindIssue)); d == nil {
			t.Errorf("%s must be ownership-denied on a foreign task", client)
		}
	}
}

// gardenio-machine-controller-config-seed is title-typed: the controller may
// upsert the config beads configs() defines, never the legacy `auth` bead
// (credential routing) or the agent-facing hook / settings / mcp configs the
// controller does not seed.
func TestMachineGrants_ControllerConfigSeed_Titles(t *testing.T) {
	ctrl := machineCtx("gardenio-controller", "gardenio-operator", "gardenio-machine-controller-config-seed")
	for _, title := range []string{"prerun", "context", "claude-instructions", "nudge-prompts", "type:agent", "view:agents:active"} {
		for _, action := range []string{"create", "update"} {
			if !machineGrantsConfigWrite(ctrl, action, title) {
				t.Errorf("controller must %s config %q", action, title)
			}
		}
	}
	for _, title := range []string{"auth", "claude-hooks", "claude-settings", "claude-mcp", "authz-denied-text", ""} {
		if machineGrantsConfigWrite(ctrl, "update", title) {
			t.Errorf("controller must not update config %q", title)
		}
	}
	if machineGrantsConfigWrite(ctrl, "close", "prerun") {
		t.Error("config-seed grants create/update only")
	}
	for _, e := range machineControllerConfigSeedTitles {
		if strings.HasSuffix(e, "*") {
			t.Errorf("config-seed lists the prefix %q; exact titles only", e)
		}
	}
	other := machineCtx("gardenio-grafana-bridge", "gardenio-operator", "gardenio-machine-config-seed")
	if machineGrantsConfigWrite(other, "update", "prerun") {
		t.Error("grafana-bridge's config-seed must not cover prerun")
	}
}

// The Lands Gate (step 7.7): the viewer holding gardenio-machine-lands-gate may
// create and update a plot_application; the same viewer without it, the
// plan-reactor role alone and the canary are refused, and the grant opens no
// other admin-gated type.
func TestAuthorizeWrite_Machine_LandsGate(t *testing.T) {
	srv, _, _ := testCtx(t)
	gate := machineCtx("gardenio-beads-viewer", "gardenio-operator", "gardenio-machine-plan-reactor", "gardenio-machine-lands-gate")
	for _, action := range []string{"create", "update"} {
		if d := srv.authorizeWrite(gate, action, typePlotApplication, []string{"project:lands-intake"}); d != nil {
			t.Fatalf("lands gate %s/plot_application denied: %+v", action, d)
		}
	}
	if d := srv.authorizeWrite(gate, "create", "credit_grant", nil); d == nil {
		t.Fatal("the lands-gate grant must not extend to credit_grant")
	}
	if d := srv.authorizeWrite(gate, "update", "account", nil); d == nil {
		t.Fatal("the lands-gate grant must not extend to account")
	}
	for _, ctx := range []context.Context{
		machineCtx("gardenio-beads-viewer", "gardenio-operator", "gardenio-machine-plan-reactor"),
		machineCtx("gardenio-authz-canary"),
	} {
		if d := srv.authorizeWrite(ctx, "create", typePlotApplication, []string{"project:lands-intake"}); d == nil {
			t.Fatal("a machine without gardenio-machine-lands-gate created a plot_application")
		}
	}
}

// Over HTTP: the Gate files an application and moves it, the plan-reactor-only
// viewer is refused before anything is stored.
func TestMachineGrants_LandsGate_HTTP(t *testing.T) {
	h := newMachineHarness(t, []string{"gardenio-beads-viewer"})
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	srv.userProvisioner = nil
	srv.SetFleetAuth(h.fc)
	handler := srv.NewHTTPHandler(h.verifier, withTestToken(h.fc, machineStaticToken))

	body := plotApplicationBody(validPlotApplicationFields())
	refused := machineDo(t, handler, h.saToken(t, "gardenio-beads-viewer", "gardenio-operator", "gardenio-machine-plan-reactor"), http.MethodPost, "/v1/beads", body)
	if refused.Code != http.StatusForbidden {
		t.Fatalf("viewer without the lands-gate role: status=%d body=%s", refused.Code, refused.Body.String())
	}
	for _, b := range ms.beads {
		if b.Type == typePlotApplication {
			t.Fatalf("a refused create stored application %s", b.ID)
		}
	}

	tok := h.saToken(t, "gardenio-beads-viewer", "gardenio-operator", "gardenio-machine-lands-gate")
	rec := machineDo(t, handler, tok, http.MethodPost, "/v1/beads", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("lands gate create: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var b model.Bead
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatal(err)
	}
	if b.CreatedBy != "machine:gardenio-beads-viewer" {
		t.Errorf("created_by = %q, want the viewer's machine subject", b.CreatedBy)
	}
	upd := machineDo(t, handler, tok, http.MethodPatch, "/v1/beads/"+b.ID, map[string]any{"fields": map[string]any{"state": "withdrawn"}})
	if upd.Code != http.StatusOK {
		t.Fatalf("lands gate update: status=%d body=%s", upd.Code, upd.Body.String())
	}
}

// github-bridge's release recorder (gd-305kriuXdT): its role passes the
// release writer check for create and update, close is not granted, and
// another machine principal (or the release-recorder role held under a
// different action) is refused the same write.
func TestReleaseWriterDenial_Machine_ReleaseRecorder(t *testing.T) {
	srv, _, _ := testCtx(t)
	recorder := machineCtx("gardenio-github-bridge", "gardenio-machine-release-recorder")
	for _, action := range []string{"create", "update", releaseDeployAction} {
		if d := srv.releaseWriterDenial(recorder, action); d != nil {
			t.Errorf("release recorder %s: denied: %s", action, d.Message)
		}
	}
	if d := srv.releaseWriterDenial(recorder, "close"); d == nil {
		t.Error("release recorder close: admitted, want 403 (close is not granted)")
	}
	other := machineCtx("gardenio-grafana-bridge", "gardenio-operator", "gardenio-machine-config-seed", "gardenio-machine-dashboard-sync")
	if d := srv.releaseWriterDenial(other, "create"); d == nil {
		t.Error("grafana-bridge principal create release: admitted, want 403")
	} else if d.CallerSubject != "machine:gardenio-grafana-bridge" {
		t.Errorf("denial subject = %q, want machine:gardenio-grafana-bridge", d.CallerSubject)
	}
	if !machineTypedOwnership(recorder, model.TypeRelease) {
		t.Error("release recorder: no typed ownership of a release another writer created")
	}
	if machineTypedOwnership(other, model.TypeRelease) {
		t.Error("grafana-bridge principal: typed ownership of a release, want none")
	}
}

// Over HTTP, through the real mux and requireRole(RoleOperator) (gd-ySWUWrbfRR):
// github-bridge holding only gardenio-machine-release-recorder, no
// gardenio-operator, records a release and its deployment. A machine principal
// is not roleBearing, so the route's RoleOperator gate admits it and the
// release writer check is what decides; another registered client without the
// recorder role is refused both writes.
func TestMachineGrants_ReleaseRecorder_HTTP(t *testing.T) {
	h := newMachineHarness(t, []string{"gardenio-github-bridge", "gardenio-grafana-bridge"})
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	srv.userProvisioner = nil
	srv.SetFleetAuth(withTestToken(h.fc, machineStaticToken))
	handler := srv.NewHTTPHandler(h.verifier, h.fc)
	seedReleaseProject(ms, "proj-gardenio", "gardenio", testGardenioReleaseSource)

	release := map[string]any{"project": "gardenio", "tag": "2026.304.1", "sha": "abc123"}
	other := h.saToken(t, "gardenio-grafana-bridge", "gardenio-machine-config-seed")
	if rec := machineDo(t, handler, other, http.MethodPost, "/v1/releases", release); rec.Code != http.StatusForbidden {
		t.Fatalf("registered client without the recorder role, create: status=%d body=%s", rec.Code, rec.Body.String())
	}
	for _, b := range ms.beads {
		if b.Type == model.TypeRelease {
			t.Fatalf("a refused create stored release %s", b.ID)
		}
	}

	recorder := h.saToken(t, "gardenio-github-bridge", "gardenio-machine-release-recorder")
	rec := machineDo(t, handler, recorder, http.MethodPost, "/v1/releases", release)
	if rec.Code/100 != 2 {
		t.Fatalf("release recorder create: status=%d body=%s", rec.Code, rec.Body.String())
	}
	rel := decodeRelease(t, rec.Body.Bytes())

	deployPath := "/v1/releases/" + rel.ID + "/deployments"
	if rec := machineDo(t, handler, other, http.MethodPost, deployPath, map[string]any{"target": "prod"}); rec.Code != http.StatusForbidden {
		t.Fatalf("registered client without the recorder role, deploy: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if releaseViewOf(ms.beads[rel.ID]).Status != releaseStatusCut {
		t.Error("a refused deploy changed the release")
	}
	if rec := machineDo(t, handler, recorder, http.MethodPost, deployPath, map[string]any{"target": "prod"}); rec.Code/100 != 2 {
		t.Fatalf("release recorder deploy: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := releaseViewOf(ms.beads[rel.ID]).Status; got != releaseStatusLive {
		t.Errorf("release status after deploy = %q, want %q", got, releaseStatusLive)
	}
}

// gardenio-machine-release-deploy (plan gd-UZKZ9H25UD step 4) is deploy-only:
// the release writer check admits it for deploy and refuses create, update
// and close, so it records where a release runs and edits no release.
func TestReleaseWriterDenial_Machine_ReleaseDeploy(t *testing.T) {
	srv, _, _ := testCtx(t)
	deployer := machineCtx("gardenio-release-deployer", "gardenio-operator", "gardenio-machine-release-deploy")
	if d := srv.releaseWriterDenial(deployer, releaseDeployAction); d != nil {
		t.Errorf("release deploy role, deploy: denied: %s", d.Message)
	}
	for _, action := range []string{"create", "update", "close"} {
		if d := srv.releaseWriterDenial(deployer, action); d == nil {
			t.Errorf("release deploy role, %s: admitted, want 403", action)
		}
	}
	if machineTypedOwnership(deployer, model.TypeRelease) {
		t.Error("release deploy role: typed ownership of a release, want none")
	}
}

// Over HTTP on an auth-enabled server (real mux, OIDC verifier, machine
// registry, privileged fleet and per-agent tokens), the merge desk's refusal
// cases for gardenio-machine-release-deploy (PR #1623):
//
//	(a) the role records a deployment, and is refused creating a release and
//	    PATCHing its sha or images (the recorder, which holds update, may);
//	(b) a registered machine without the role is refused the deployment;
//	(c) a privileged fleet token and a crew agent's own token are refused it;
//	(d) a helm_deploy escalation closed success by a caller who is not a
//	    release writer records no deployment: a crew agent (as the recorded
//	    executor, spec §5.2) and a machine without the role close it and get
//	    an error note. (A privileged fleet token is refused the close itself
//	    by the /result caller-class gate, so it is not a case here; the
//	    retired static token gets 401 everywhere.)
func TestMachineGrants_ReleaseDeploy_HTTP(t *testing.T) {
	const crewToken = "crew-agent-tok"
	h := newMachineHarness(t, []string{"gardenio-github-bridge", "gardenio-release-deployer", "gardenio-controller"})
	h.fc.RegisterTokenFull(crewToken, "agent-gd-crew1", "gardenio", nil, nil, time.Time{}, "crew-1", false)
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	srv.userProvisioner = nil
	srv.SetFleetAuth(withTestToken(h.fc, machineStaticToken))
	handler := srv.NewHTTPHandler(h.verifier, h.fc)
	seedReleaseProject(ms, "proj-gardenio", "gardenio", testGardenioReleaseSource)

	recorder := h.saToken(t, "gardenio-github-bridge", "gardenio-machine-release-recorder")
	deployer := h.saToken(t, "gardenio-release-deployer", "gardenio-operator", "gardenio-machine-release-deploy")
	noRole := h.saToken(t, "gardenio-controller", "gardenio-operator", "gardenio-machine-accounts", "gardenio-machine-controller-ownership")

	rec := machineDo(t, handler, recorder, http.MethodPost, "/v1/releases", map[string]any{"project": "gardenio", "tag": "2026.304.1", "sha": "abc123"})
	if rec.Code/100 != 2 {
		t.Fatalf("release recorder create: status=%d body=%s", rec.Code, rec.Body.String())
	}
	rel := decodeRelease(t, rec.Body.Bytes())
	deployPath := "/v1/releases/" + rel.ID + "/deployments"
	deployments := func() int { return len(releaseViewOf(ms.beads[rel.ID]).Deployments) }
	sha := func() string { return releaseString(releaseFieldMap(ms.beads[rel.ID].Fields), "sha") }

	// (a) create and PATCH are refused; the release is unchanged.
	if rec := machineDo(t, handler, deployer, http.MethodPost, "/v1/releases", map[string]any{"project": "gardenio", "tag": "2026.305.1"}); rec.Code != http.StatusForbidden {
		t.Errorf("release deploy role, create: status=%d body=%s, want 403", rec.Code, rec.Body.String())
	}
	for _, b := range ms.beads {
		if b.Type == model.TypeRelease && b.ID != rel.ID {
			t.Errorf("a refused create stored release %s", b.ID)
		}
	}
	for _, fields := range []map[string]any{
		{"sha": "evil0001"},
		{"images": []map[string]any{{"name": "controller", "digest": "sha256:evil"}}},
	} {
		if rec := machineDo(t, handler, deployer, http.MethodPatch, "/v1/beads/"+rel.ID, map[string]any{"fields": fields}); rec.Code != http.StatusForbidden {
			t.Errorf("release deploy role, PATCH %v: status=%d body=%s, want 403", fields, rec.Code, rec.Body.String())
		}
	}
	if got := sha(); got != "abc123" {
		t.Errorf("sha after refused PATCHes = %q, want abc123", got)
	}
	if _, ok := releaseFieldMap(ms.beads[rel.ID].Fields)["images"]; ok {
		t.Error("images written by a refused PATCH")
	}

	// (b) and (c): the deployment route refuses every non-writer.
	for name, tok := range map[string]string{
		"machine without the role": noRole,
		"privileged fleet token":   machineStaticToken,
		"crew agent token":         crewToken,
	} {
		if rec := machineDo(t, handler, tok, http.MethodPost, deployPath, map[string]any{"target": "prod", "revision": "999"}); rec.Code != http.StatusForbidden {
			t.Errorf("%s, deploy: status=%d body=%s, want 403", name, rec.Code, rec.Body.String())
		}
	}
	if n := deployments(); n != 0 {
		t.Fatalf("refused deploys wrote %d rows", n)
	}

	// (a) the role records the deployment.
	if rec := machineDo(t, handler, deployer, http.MethodPost, deployPath, map[string]any{"target": "prod", "revision": "431"}); rec.Code/100 != 2 {
		t.Fatalf("release deploy role, deploy: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if v := releaseViewOf(ms.beads[rel.ID]); v.Status != releaseStatusLive || len(v.Deployments) != 1 || v.Deployments[0].Revision != "431" {
		t.Fatalf("release after deploy = %s %+v, want live at revision 431", v.Status, v.Deployments)
	}

	// Positive control for (a): the recorder, which holds update, may PATCH.
	if rec := machineDo(t, handler, recorder, http.MethodPatch, "/v1/beads/"+rel.ID, map[string]any{"fields": map[string]any{"sha": "def456"}}); rec.Code != http.StatusOK {
		t.Fatalf("release recorder PATCH sha: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := sha(); got != "def456" {
		t.Errorf("sha after the recorder's PATCH = %q, want def456", got)
	}

	// (d) a helm_deploy close by a non-writer records nothing.
	for i, tok := range []string{crewToken, noRole} {
		id := fmt.Sprintf("gd-esc-hd%d", i)
		// An agent may post a non-verdict result only as the executor the
		// dispatcher recorded (escalation_resolver.go); the body's executor
		// is ignored, so the crew close is seeded as dispatched to crew-1.
		fields := `{"operation_type":"helm_deploy","context":"next window"}`
		if tok == crewToken {
			fields = `{"operation_type":"helm_deploy","context":"next window","executor":"crew-1"}`
		}
		ms.beads[id] = &model.Bead{
			ID: id, Title: "ship it", Type: model.TypeEscalation, Status: model.StatusOpen, Fleet: "default",
			Fields: json.RawMessage(fields),
		}
		ms.labels[id] = []string{"project:gardenio"}
		rec := machineDo(t, handler, tok, http.MethodPost, "/v1/escalations/"+id+"/result",
			map[string]any{"result_status": "success", "executor": "crew-1", "result": map[string]any{"tag": "2026.304.1", "revision": 500 + i}})
		if rec.Code != http.StatusOK {
			t.Fatalf("close %s: status=%d body=%s", id, rec.Code, rec.Body.String())
		}
		var resp map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		if dep, _ := resp["deployment"].(map[string]any); dep == nil || dep["error"] == nil {
			t.Errorf("close %s by a non-writer: deployment = %v, want an error note", id, resp["deployment"])
		}
	}
	if v := releaseViewOf(ms.beads[rel.ID]); len(v.Deployments) != 1 || v.Deployments[0].Revision != "431" || v.Deployments[0].Until != "" {
		t.Errorf("deployments after non-writer helm_deploy closes = %+v, want only the open revision 431 row", v.Deployments)
	}
}

// machineGrantsGolden is the JSON shape of testdata/machine_grants.golden.json.
type machineGrantsGolden struct {
	Writes    []string `json:"writes"`
	TokenMint bool     `json:"token_mint"`
	Ownership []string `json:"ownership"`
	// ConfigTitles and LabeledOwnership are omitted when empty so roles that
	// do not use them keep their golden entry unchanged.
	ConfigTitles     []string `json:"config_titles,omitempty"`
	LabeledOwnership []string `json:"labeled_ownership,omitempty"`
	ReclaimOwnership []string `json:"reclaim_ownership,omitempty"`
	FieldOwnership   []string `json:"field_ownership,omitempty"`
	DispatchClaim    []string `json:"dispatch_claim,omitempty"`
	TriggerLineage   bool     `json:"trigger_lineage,omitempty"`
	LandPurge        bool     `json:"land_purge,omitempty"`
	PlanScope        bool     `json:"plan_scope,omitempty"`
	// EscalationExecutor is omitted when false, like the flags above it.
	EscalationExecutor bool `json:"escalation_executor,omitempty"`
}

// marshalMachineGrantsGolden renders machineCapabilityGrants as the golden
// file: {"<role>": {"writes": ["update/account"], "token_mint": false,
// "ownership": ["epic"]}}, with roles, writes and ownership types sorted
// (json.Marshal sorts map keys).
func marshalMachineGrantsGolden() ([]byte, error) {
	out := make(map[string]machineGrantsGolden, len(machineCapabilityGrants))
	for role, g := range machineCapabilityGrants {
		writes := make([]string, 0, len(g.Writes))
		for _, w := range g.Writes {
			writes = append(writes, w.Action+"/"+string(w.BeadType))
		}
		sort.Strings(writes)
		ownership := make([]string, 0, len(g.Ownership))
		for _, t := range g.Ownership {
			ownership = append(ownership, string(t))
		}
		sort.Strings(ownership)
		titles := append([]string(nil), g.ConfigTitles...)
		sort.Strings(titles)
		labeled := make([]string, 0, len(g.LabeledOwnership))
		for _, l := range g.LabeledOwnership {
			labeled = append(labeled, strings.Join(l.Actions, ",")+"/"+string(l.BeadType)+"@"+l.Label)
		}
		sort.Strings(labeled)
		var reclaim []string
		for _, t := range g.ReclaimOwnership {
			reclaim = append(reclaim, string(t))
		}
		sort.Strings(reclaim)
		var fieldOwnership []string
		for _, f := range g.FieldOwnership {
			for _, field := range f.Fields {
				fieldOwnership = append(fieldOwnership, string(f.BeadType)+"."+field)
			}
		}
		sort.Strings(fieldOwnership)
		var dispatch []string
		for _, t := range g.DispatchClaim {
			dispatch = append(dispatch, string(t))
		}
		sort.Strings(dispatch)
		out[role] = machineGrantsGolden{Writes: writes, TokenMint: g.TokenMint, Ownership: ownership, ConfigTitles: titles, LabeledOwnership: labeled, ReclaimOwnership: reclaim, FieldOwnership: fieldOwnership, DispatchClaim: dispatch, TriggerLineage: g.TriggerLineage, LandPurge: g.LandPurge, PlanScope: g.PlanScope, EscalationExecutor: g.EscalationExecutor}
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
