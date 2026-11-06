package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

const (
	testDeviceTokenA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testDeviceTokenB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// deviceRequest builds a request as the RBAC middleware would hand it to the
// handler for an OIDC caller with the given roles.
func deviceRequest(method, target, email string, roles []string, body any) *http.Request {
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(method, target, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	ctx := withAuthMethod(req.Context(), authMethodOIDC)
	ctx = WithAuthIdentity(ctx, AuthIdentity{Type: "oidc", Subject: email})
	return req.WithContext(WithRoles(ctx, roles))
}

// pushBridgeDeviceRequest is the push bridge's shape: its own Keycloak
// service account, machine:gardenio-push-bridge.
func pushBridgeDeviceRequest(method, target string) *http.Request {
	return httptest.NewRequest(method, target, nil).WithContext(machineCtx("gardenio-push-bridge"))
}

func seedDevice(ms *mockStore, token, email string) {
	_ = ms.UpsertPushDevice(context.Background(), &model.PushDevice{Token: token, Platform: "ios", UserEmail: email})
}

func decodeDevices(t *testing.T, rec *httptest.ResponseRecorder) []*model.PushDevice {
	t.Helper()
	var resp listPushDevicesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	return resp.Devices
}

func TestRegisterPushDevice_OwnerIsTheCaller(t *testing.T) {
	s, ms, _ := newTestServer()

	rec := httptest.NewRecorder()
	s.handleRegisterPushDevice(rec, deviceRequest(http.MethodPost, "/v1/devices", "Owner@Example.com", []string{RoleViewer},
		map[string]any{"token": strings.ToUpper(testDeviceTokenA), "platform": "iOS", "bundle_id": "com.gardenio.app",
			// A body owner is not part of the contract and must be ignored.
			"user_email": "someone-else@example.com"}))
	requireStatus(t, rec, http.StatusOK)

	d := ms.pushDevices[testDeviceTokenA]
	if d == nil {
		t.Fatalf("device not stored under the lowercased token: %v", ms.pushDevices)
	}
	if d.UserEmail != "owner@example.com" || d.Platform != "ios" || d.BundleID != "com.gardenio.app" {
		t.Fatalf("stored = %+v", d)
	}
}

func TestRegisterPushDevice_ReRegisterMovesToNewOwner(t *testing.T) {
	s, ms, _ := newTestServer()
	seedDevice(ms, testDeviceTokenA, "old@example.com")

	rec := httptest.NewRecorder()
	s.handleRegisterPushDevice(rec, deviceRequest(http.MethodPost, "/v1/devices", "new@example.com", []string{RoleViewer},
		map[string]any{"token": testDeviceTokenA}))
	requireStatus(t, rec, http.StatusOK)
	if got := ms.pushDevices[testDeviceTokenA].UserEmail; got != "new@example.com" {
		t.Fatalf("owner = %q, want the latest sign-in", got)
	}
}

func TestRegisterPushDevice_RefusesNonOIDC(t *testing.T) {
	s, ms, _ := newTestServer()

	raw, _ := json.Marshal(map[string]any{"token": testDeviceTokenA})
	req := httptest.NewRequest(http.MethodPost, "/v1/devices", bytes.NewReader(raw))
	req = req.WithContext(WithAuthIdentity(withAuthMethod(req.Context(), authMethodStatic), AuthIdentity{Type: "static", Subject: "static"}))
	rec := httptest.NewRecorder()
	s.handleRegisterPushDevice(rec, req)
	requireStatus(t, rec, http.StatusForbidden)
	if len(ms.pushDevices) != 0 {
		t.Fatalf("a non-OIDC caller must not register: %v", ms.pushDevices)
	}
}

func TestRegisterPushDevice_Validation(t *testing.T) {
	s, _, _ := newTestServer()
	cases := map[string]map[string]any{
		"missing token":   {},
		"short token":     {"token": "abc"},
		"non-hex token":   {"token": strings.Repeat("z", 64)},
		"other platform":  {"token": testDeviceTokenA, "platform": "android"},
		"spaced bundle":   {"token": testDeviceTokenA, "bundle_id": "com gardenio"},
		"oversize bundle": {"token": testDeviceTokenA, "bundle_id": strings.Repeat("a", pushDeviceBundleIDMax+1)},
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			s.handleRegisterPushDevice(rec, deviceRequest(http.MethodPost, "/v1/devices", "owner@example.com", []string{RoleViewer}, body))
			requireStatus(t, rec, http.StatusBadRequest)
		})
	}
}

func TestListPushDevices_NonAdminSeesOnlyOwn(t *testing.T) {
	s, ms, _ := newTestServer()
	seedDevice(ms, testDeviceTokenA, "owner@example.com")
	seedDevice(ms, testDeviceTokenB, "teammate@example.com")

	rec := httptest.NewRecorder()
	s.handleListPushDevices(rec, deviceRequest(http.MethodGet, "/v1/devices", "owner@example.com", []string{RoleOperator}, nil))
	requireStatus(t, rec, http.StatusOK)
	got := decodeDevices(t, rec)
	if len(got) != 1 || got[0].Token != testDeviceTokenA {
		t.Fatalf("non-admin list = %+v, want only their own device", got)
	}

	rec = httptest.NewRecorder()
	s.handleListPushDevices(rec, deviceRequest(http.MethodGet, "/v1/devices?user_email=teammate@example.com", "owner@example.com", []string{RoleOperator}, nil))
	requireStatus(t, rec, http.StatusForbidden)
}

func TestListPushDevices_BridgeResolvesRecipients(t *testing.T) {
	s, ms, _ := newTestServer()
	seedDevice(ms, testDeviceTokenA, "owner@example.com")
	seedDevice(ms, testDeviceTokenB, "teammate@example.com")
	seedDevice(ms, strings.Repeat("c", 64), "stranger@example.com")

	rec := httptest.NewRecorder()
	s.handleListPushDevices(rec, pushBridgeDeviceRequest(http.MethodGet,
		"/v1/devices?user_email=Owner@example.com,teammate@example.com&platform=ios"))
	requireStatus(t, rec, http.StatusOK)
	got := decodeDevices(t, rec)
	if len(got) != 2 || got[0].UserEmail != "owner@example.com" || got[1].UserEmail != "teammate@example.com" {
		t.Fatalf("recipient list = %+v", got)
	}

	// No filter is every device: admin/machine inventory.
	rec = httptest.NewRecorder()
	s.handleListPushDevices(rec, pushBridgeDeviceRequest(http.MethodGet, "/v1/devices"))
	requireStatus(t, rec, http.StatusOK)
	if got := decodeDevices(t, rec); len(got) != 3 {
		t.Fatalf("unfiltered list = %d devices, want 3", len(got))
	}
}

func TestListPushDevices_AdminSeesAny(t *testing.T) {
	s, ms, _ := newTestServer()
	seedDevice(ms, testDeviceTokenB, "teammate@example.com")

	rec := httptest.NewRecorder()
	s.handleListPushDevices(rec, deviceRequest(http.MethodGet, "/v1/devices?user_email=teammate@example.com", "owner@example.com", []string{RoleAdmin}, nil))
	requireStatus(t, rec, http.StatusOK)
	if got := decodeDevices(t, rec); len(got) != 1 {
		t.Fatalf("admin list = %+v", got)
	}
}

func TestDeletePushDevice_NonAdminCannotDeleteOthers(t *testing.T) {
	s, ms, _ := newTestServer()
	seedDevice(ms, testDeviceTokenA, "owner@example.com")
	seedDevice(ms, testDeviceTokenB, "teammate@example.com")

	req := deviceRequest(http.MethodDelete, "/v1/devices/"+testDeviceTokenB, "owner@example.com", []string{RoleOperator}, nil)
	req.SetPathValue("token", testDeviceTokenB)
	rec := httptest.NewRecorder()
	s.handleDeletePushDevice(rec, req)
	// 204 like a miss, so the answer never says whose the token is.
	requireStatus(t, rec, http.StatusNoContent)
	if ms.pushDevices[testDeviceTokenB] == nil {
		t.Fatal("another person's device was deleted")
	}

	req = deviceRequest(http.MethodDelete, "/v1/devices/"+testDeviceTokenA, "owner@example.com", []string{RoleOperator}, nil)
	req.SetPathValue("token", testDeviceTokenA)
	rec = httptest.NewRecorder()
	s.handleDeletePushDevice(rec, req)
	requireStatus(t, rec, http.StatusNoContent)
	if ms.pushDevices[testDeviceTokenA] != nil {
		t.Fatal("own device was not deleted")
	}
}

func TestDeletePushDevice_BridgePrunesAny(t *testing.T) {
	s, ms, _ := newTestServer()
	seedDevice(ms, testDeviceTokenB, "teammate@example.com")

	req := pushBridgeDeviceRequest(http.MethodDelete, "/v1/devices/"+testDeviceTokenB)
	req.SetPathValue("token", testDeviceTokenB)
	rec := httptest.NewRecorder()
	s.handleDeletePushDevice(rec, req)
	requireStatus(t, rec, http.StatusNoContent)
	if ms.pushDevices[testDeviceTokenB] != nil {
		t.Fatal("the push bridge must be able to prune a dead token")
	}
}

// TestPushDeviceRoutes_Wired drives the real mux, so a dropped route
// registration fails here.
func TestPushDeviceRoutes_Wired(t *testing.T) {
	_, ms, h := newTestServer()

	rec := doJSON(t, h, http.MethodPost, "/v1/devices", map[string]any{"token": testDeviceTokenA})
	requireStatus(t, rec, http.StatusOK)
	if ms.pushDevices[testDeviceTokenA] == nil {
		t.Fatalf("POST /v1/devices did not register: %s", rec.Body.String())
	}

	rec = doJSON(t, h, http.MethodGet, "/v1/devices", nil)
	requireStatus(t, rec, http.StatusOK)
	if got := decodeDevices(t, rec); len(got) != 1 {
		t.Fatalf("GET /v1/devices = %+v", got)
	}

	rec = doJSON(t, h, http.MethodDelete, "/v1/devices/"+testDeviceTokenA, nil)
	requireStatus(t, rec, http.StatusNoContent)
	if len(ms.pushDevices) != 0 {
		t.Fatalf("DELETE /v1/devices/{token} did not delete: %v", ms.pushDevices)
	}
}

// TestPushDevices_NonOIDCRefusedExceptPushBridge pins the security review on
// PR #580: not being OIDC is not trust. A per-agent token, a fleet token, the
// shared static token and any machine but the push bridge get 403 on all three
// verbs and touch no row. It also exercises the machine_gates_test.go rows for
// http_devices.go.
func TestPushDevices_NonOIDCRefusedExceptPushBridge(t *testing.T) {
	refused := map[string]context.Context{
		"per-agent token":     gateAgentCtx(),
		"fleet token":         gateFleetCtx(),
		"static token":        gateStaticCtx(),
		"other machine":       gateMachineCtx(),
		"agent-named machine": machineCtx("agent-gardenio-push-bridge"),
		"edge":                withAuthMethod(context.Background(), authMethodEdge),
		"auth disabled":       withAuthMethod(context.Background(), authMethodNone),
	}
	for name, ctx := range refused {
		t.Run(name, func(t *testing.T) {
			s, ms, _ := newTestServer()
			seedDevice(ms, testDeviceTokenA, "owner@example.com")

			raw, _ := json.Marshal(map[string]any{"token": testDeviceTokenB})
			rec := httptest.NewRecorder()
			s.handleRegisterPushDevice(rec, httptest.NewRequest(http.MethodPost, "/v1/devices", bytes.NewReader(raw)).WithContext(ctx))
			requireStatus(t, rec, http.StatusForbidden)

			for _, target := range []string{"/v1/devices", "/v1/devices?user_email=owner@example.com"} {
				rec = httptest.NewRecorder()
				s.handleListPushDevices(rec, httptest.NewRequest(http.MethodGet, target, nil).WithContext(ctx))
				requireStatus(t, rec, http.StatusForbidden)
				if strings.Contains(rec.Body.String(), testDeviceTokenA) {
					t.Fatalf("GET %s leaked a device token: %s", target, rec.Body.String())
				}
			}

			req := httptest.NewRequest(http.MethodDelete, "/v1/devices/"+testDeviceTokenA, nil).WithContext(ctx)
			req.SetPathValue("token", testDeviceTokenA)
			rec = httptest.NewRecorder()
			s.handleDeletePushDevice(rec, req)
			requireStatus(t, rec, http.StatusForbidden)

			if len(ms.pushDevices) != 1 || ms.pushDevices[testDeviceTokenA] == nil {
				t.Fatalf("a refused caller changed the registry: %v", ms.pushDevices)
			}
		})
	}
}

// TestPushDevices_PushBridgeCannotRegister: the bridge reads and prunes, but
// a device is only ever enrolled by its owner's own OIDC token.
func TestPushDevices_PushBridgeCannotRegister(t *testing.T) {
	s, ms, _ := newTestServer()
	raw, _ := json.Marshal(map[string]any{"token": testDeviceTokenB})
	req := httptest.NewRequest(http.MethodPost, "/v1/devices", bytes.NewReader(raw)).WithContext(machineCtx("gardenio-push-bridge"))
	rec := httptest.NewRecorder()
	s.handleRegisterPushDevice(rec, req)
	requireStatus(t, rec, http.StatusForbidden)
	if len(ms.pushDevices) != 0 {
		t.Fatalf("the push bridge registered a device: %v", ms.pushDevices)
	}
}

// TestPushDeviceRoutes_PerAgentTokenRefused drives the real mux with a
// per-agent fleet token, so the route wrapper and the scoped-route class
// cannot let one through either.
func TestPushDeviceRoutes_PerAgentTokenRefused(t *testing.T) {
	s, ms, _ := newTestServer()
	seedDevice(ms, testDeviceTokenA, "owner@example.com")
	fleet := NewFleetConfig("default", nil)
	fleet.RegisterTokenWithProject("crew-agent-token", "agent-gd-agent-1", "gardenio")
	h := s.NewHTTPHandler(nil, fleet)

	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/v1/devices"},
		{http.MethodGet, "/v1/devices"},
		{http.MethodGet, "/v1/devices?user_email=owner@example.com"},
		{http.MethodDelete, "/v1/devices/" + testDeviceTokenA},
	} {
		var body []byte
		if tc.method == http.MethodPost {
			body, _ = json.Marshal(map[string]any{"token": testDeviceTokenB})
		}
		req := httptest.NewRequest(tc.method, tc.path, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer crew-agent-token")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		requireStatus(t, rec, http.StatusForbidden)
	}
	if len(ms.pushDevices) != 1 || ms.pushDevices[testDeviceTokenA] == nil {
		t.Fatalf("a per-agent token changed the registry: %v", ms.pushDevices)
	}
}
