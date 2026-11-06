package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"google.golang.org/grpc/codes"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// type:permission writes (gd-RuI4Ze6vAg): the bead records the verdict the
// bridge delivered, so only the bridge's machine principal (its
// gardenio-machine-slack-dm-delivery grant) and an OIDC admin may create,
// update or close one. Every fleet token, a privileged one included, is
// refused, an agent's own per-agent token included, even on the permission
// bead its own harness raised.

const (
	permAgentToken  = "agent-secret"
	permBridgeToken = "bridge-secret"
	permAgentFleet  = agentFleetPrefix + "gd-agent1"
	permBridgeRoles = "gardenio-machine-slack-dm-delivery"
)

func permissionFields() json.RawMessage {
	return json.RawMessage(`{"agent":"crew-a","request_id":"req-1","tool_name":"Bash"}`)
}

// permissionBead is an open permission request raised by crew-a's harness and
// owned by its per-agent token, so the ownership gate alone would admit that
// agent: what refuses it is the write_policy.
func permissionBead(id string) *model.Bead {
	return &model.Bead{
		ID: id, Title: "Permission: Bash for crew-a", Type: typePermission, Kind: model.KindData,
		Status: model.StatusOpen, Labels: []string{"project:gardenio"},
		CreatedBy: permAgentFleet, Assignee: permAgentFleet, Fields: permissionFields(),
	}
}

func TestPermissionType_AdminGated(t *testing.T) {
	srv, _, _ := testCtx(t)
	tc, err := srv.resolveTypeConfig(context.Background(), typePermission)
	if err != nil || tc == nil || tc.WritePolicy == nil || tc.WritePolicy.MinRole != RoleAdmin {
		t.Fatalf("type:permission write_policy = %+v (err %v), want min_role %s", tc, err, RoleAdmin)
	}
	if !minRoleFloorTypes["type:permission"] {
		t.Fatal("type:permission must be a min_role floor type: a stored seed must not lower it")
	}
	for _, action := range []string{"create", "update", "close"} {
		if isMachineAllowedWrite(action, typePermission) {
			t.Errorf("%s/permission must not be in machineAllowedWrites (static and fleet tokens)", action)
		}
	}
}

// A stored type:permission row (the controller seeds one) with a weaker policy
// keeps the builtin min_role.
func TestPermissionType_StoredPolicyCannotLowerFloor(t *testing.T) {
	srv, ms, _ := testCtx(t)
	ms.configs["type:permission"] = &model.Config{Key: "type:permission", Value: json.RawMessage(
		`{"kind":"data","write_policy":{"project_scoped":true},"fields":[]}`)}
	tc, err := srv.resolveTypeConfig(context.Background(), typePermission)
	if err != nil || tc.WritePolicy == nil || tc.WritePolicy.MinRole != RoleAdmin {
		t.Fatalf("stored policy lowered the floor: %+v (err %v)", tc, err)
	}
}

// deniedPermissionCtxs are the non-admin credentials an agent pod can hold, plus
// the bridge's own fleet token and machine principals without the grant.
func deniedPermissionCtxs() map[string]context.Context {
	return map[string]context.Context{
		"privileged fleet":          WithOperatedByHeader(WithAuthIdentity(staticCtx(), AuthIdentity{Type: "fleet", Subject: testBearerSubject, Privileged: true}), "oidc:owner@example.com"),
		"privileged agent fleet":    WithOperatedByHeader(withTestPrivilegedAgent(staticCtx(), permAgentFleet), "oidc:owner@example.com"),
		"per-agent fleet (own)":     fleetCtxWithFleet(permAgentFleet, permAgentFleet),
		"bridge fleet token":        fleetCtxWithFleet("slack-bridge", "slack-bridge"),
		"oidc operator":             oidcCtx("op@example.com", []string{RoleOperator}),
		"slack-bridge config-seed":  machineCtx("gardenio-slack-bridge", "gardenio-operator", "gardenio-machine-slack-config-seed"),
		"controller config-seed":    machineCtx("gardenio-controller", "gardenio-machine-controller-config-seed", "gardenio-machine-controller-ownership"),
		"viewer lands-gate":         machineCtx("gardenio-beads-viewer", "gardenio-machine-lands-gate"),
		"machine carrying gd-admin": machineCtx("gardenio-slack-bridge", RoleAdmin),
	}
}

func TestPermissionWrites_GRPC_Denied(t *testing.T) {
	for name, ctx := range deniedPermissionCtxs() {
		t.Run(name, func(t *testing.T) {
			srv, ms, _ := testCtx(t)
			ms.beads["gd-perm1"] = permissionBead("gd-perm1")

			_, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{
				Title: "Permission: Bash for crew-a", Type: "permission", Kind: "data",
				Labels: []string{"project:gardenio"}, Fields: permissionFields(),
			})
			requireCode(t, err, codes.PermissionDenied)

			answer := []byte(`{"agent":"crew-a","request_id":"req-1","tool_name":"Bash","behavior":"allow","answered_by":"oidc:owner@example.com"}`)
			_, err = srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{Id: "gd-perm1", Fields: answer})
			requireCode(t, err, codes.PermissionDenied)

			_, err = srv.CloseBead(ctx, &beadsv1.CloseBeadRequest{Id: "gd-perm1"})
			requireCode(t, err, codes.PermissionDenied)

			b := ms.beads["gd-perm1"]
			if b.Status != model.StatusOpen || string(b.Fields) != string(permissionFields()) {
				t.Fatalf("refused writes changed the bead: status=%s fields=%s", b.Status, b.Fields)
			}
			for _, other := range ms.beads {
				if other.ID != "gd-perm1" && other.Type == typePermission {
					t.Fatalf("a refused create stored %s", other.ID)
				}
			}
		})
	}
}

func TestPermissionWrites_GRPC_BridgeMachineAndAdmin(t *testing.T) {
	for name, ctx := range map[string]context.Context{
		"slack-bridge machine": machineCtx("gardenio-slack-bridge", "gardenio-operator", "gardenio-machine-slack-config-seed", permBridgeRoles),
		"oidc admin":           oidcCtx("owner@example.com", []string{RoleAdmin}),
	} {
		t.Run(name, func(t *testing.T) {
			srv, ms, _ := testCtx(t)
			// Filed on the bridge's fleet token before the move to the machine
			// principal: the ownership grant lets the bridge close it.
			ms.beads["gd-perm1"] = permissionBead("gd-perm1")
			ms.beads["gd-perm1"].CreatedBy, ms.beads["gd-perm1"].Assignee = "slack-bridge", ""

			if _, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{
				Title: "Permission: Bash for crew-a", Type: "permission", Kind: "data",
				Labels: []string{"project:gardenio"}, Fields: permissionFields(),
			}); err != nil {
				t.Fatalf("create: %v", err)
			}
			answer := []byte(`{"agent":"crew-a","request_id":"req-1","tool_name":"Bash","behavior":"allow","answered_by":"slack:U1"}`)
			if _, err := srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{Id: "gd-perm1", Fields: answer}); err != nil {
				t.Fatalf("update: %v", err)
			}
			if _, err := srv.CloseBead(ctx, &beadsv1.CloseBeadRequest{Id: "gd-perm1"}); err != nil {
				t.Fatalf("close: %v", err)
			}
		})
	}
}

// The grant is typed to permission and dm: the bridge role opens no other
// admin-gated type.
func TestPermissionGrant_Scoped(t *testing.T) {
	srv, _, _ := testCtx(t)
	bridge := machineCtx("gardenio-slack-bridge", permBridgeRoles)
	for _, typ := range []model.BeadType{"account", "advice", "config", typePlotApplication, "credit_grant", "user"} {
		if d := srv.authorizeWrite(bridge, "create", typ, nil); d == nil {
			t.Errorf("slack-dm-delivery must not create %s", typ)
		}
	}
	if d := srv.authorizeWrite(bridge, "delete", typePermission, nil); d == nil {
		t.Error("slack-dm-delivery must not delete a permission bead")
	}
}

// permissionHTTP is the full HTTP stack: the machine harness, a per-agent
// fleet token and the bridge's hand-made fleet-map token.
func permissionHTTP(t *testing.T) (*machineHarness, http.Handler, *mockStore) {
	t.Helper()
	h := newMachineHarness(t, []string{"gardenio-slack-bridge", "gardenio-controller"})
	h.fc.tokenFleets[permAgentToken] = bridgeTokenInfo{Fleet: permAgentFleet}
	h.fc.tokenFleets[permBridgeToken] = bridgeTokenInfo{Fleet: "slack-bridge"}
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	srv.userProvisioner = nil
	srv.SetFleetAuth(h.fc)
	ms.beads["gd-perm1"] = permissionBead("gd-perm1")
	return h, srv.NewHTTPHandler(h.verifier, withTestToken(h.fc, machineStaticToken)), ms
}

func TestPermissionWrites_HTTP_Denied(t *testing.T) {
	h, handler, ms := permissionHTTP(t)
	answer := map[string]any{"behavior": "allow", "answered_by": "oidc:owner@example.com"}
	create := map[string]any{
		"title": "Permission: Bash for crew-a", "type": "permission", "kind": "data",
		"labels": []string{"project:gardenio"}, "fields": permissionFields(),
	}
	for name, tok := range map[string]string{
		"privileged fleet":         machineStaticToken,
		"per-agent fleet (own)":    permAgentToken,
		"bridge fleet token":       permBridgeToken,
		"controller machine":       h.saToken(t, "gardenio-controller", "gardenio-operator", "gardenio-machine-controller-ownership"),
		"slack-bridge config-seed": h.saToken(t, "gardenio-slack-bridge", "gardenio-operator", "gardenio-machine-slack-config-seed"),
	} {
		for _, req := range []struct {
			method, path string
			body         any
		}{
			{http.MethodPost, "/v1/beads", create},
			{http.MethodPatch, "/v1/beads/gd-perm1", map[string]any{"fields": answer}},
			{http.MethodPost, "/v1/beads/gd-perm1/close", answer},
			{http.MethodPost, "/v1/beads/gd-perm1/fields/answered_by/append", map[string]any{"element": "oidc:owner@example.com"}},
			{http.MethodPost, "/v1/beads/gd-perm1/increment", map[string]float64{"behavior": 1}},
		} {
			rec := permissionDo(t, handler, tok, req.method, req.path, req.body)
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s %s %s: status=%d, want 403; body=%s", name, req.method, req.path, rec.Code, rec.Body.String())
			}
		}
	}
	b := ms.beads["gd-perm1"]
	if b.Status != model.StatusOpen || string(b.Fields) != string(permissionFields()) {
		t.Fatalf("refused writes changed the bead: status=%s fields=%s", b.Status, b.Fields)
	}
	for _, other := range ms.beads {
		if other.ID != "gd-perm1" && other.Type == typePermission {
			t.Fatalf("a refused create stored %s", other.ID)
		}
	}
}

// permissionDo is machineDo relaying a human in X-Operated-By, so a static
// token's create reaches the write_policy instead of the operator refusal.
func permissionDo(t *testing.T, handler http.Handler, tok, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	r := httptest.NewRequest(method, path, bytes.NewReader(b))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+tok)
	r.Header.Set("X-Operated-By", "oidc:owner@example.com")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, r)
	return rec
}

// The bridge's machine principal files the request bead and closes it with the
// verdict, as writePermissionBead / closePermissionBead do.
func TestPermissionWrites_HTTP_BridgeMachine(t *testing.T) {
	h, handler, _ := permissionHTTP(t)
	tok := h.saToken(t, "gardenio-slack-bridge", "gardenio-operator", "gardenio-machine-slack-config-seed", permBridgeRoles)
	rec := machineDo(t, handler, tok, http.MethodPost, "/v1/beads", map[string]any{
		"title": "Permission: Bash for crew-a", "type": "permission", "kind": "data",
		"labels": []string{"project:gardenio"}, "fields": permissionFields(),
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("bridge create: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var b model.Bead
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatal(err)
	}
	if b.CreatedBy != "machine:gardenio-slack-bridge" {
		t.Errorf("created_by = %q, want the bridge's machine subject", b.CreatedBy)
	}
	closed := machineDo(t, handler, tok, http.MethodPost, "/v1/beads/"+b.ID+"/close", map[string]any{"behavior": "allow", "answered_by": "slack:U1"})
	if closed.Code != http.StatusOK {
		t.Fatalf("bridge close: status=%d body=%s", closed.Code, closed.Body.String())
	}
	// A bead someone else filed (the agent's, or one from the fleet-token era).
	other := machineDo(t, handler, tok, http.MethodPost, "/v1/beads/gd-perm1/close", map[string]any{"behavior": "deny", "answered_by": "slack:U1"})
	if other.Code != http.StatusOK {
		t.Fatalf("bridge close of a foreign permission bead: status=%d body=%s", other.Code, other.Body.String())
	}
}
