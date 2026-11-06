package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"gardenio/kbeads/internal/model"
)

// Wire-compatibility tests for the fleet→bridge token rename (ka-0000000310).
// The bridge-named noun is canonical; the fleet-named noun is a dual-accept
// alias. These tests pin: (1) the canonical `bridge` request field registers a
// token, (2) responses mirror both `bridge` and `fleet`, (3) new rows persist
// under bridge_token:*, and (4) `bridge` is preferred when both fields are set.

func TestRegisterBridgeToken_CanonicalField(t *testing.T) {
	fc := NewFleetConfig("gardenio", nil)
	srv, _ := newTestServerWithFleetAuth(t, "admin-secret", fc)

	body, _ := json.Marshal(map[string]any{
		"token":  "bridge-tok-abc",
		"bridge": "slack-bridge",
	})
	// slack-bridge is a reserved service name: only a token-mint machine
	// principal may register it (gd-ua4DGNtZdG).
	req := httptest.NewRequest(http.MethodPost, "/v1/bridge-tokens", bytes.NewReader(body)).
		WithContext(machineCtx("gardenio-controller", "gardenio-machine-token-mint"))
	req.Header.Set("Authorization", "Bearer machine-jwt")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.handleRegisterBridgeToken(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d; body: %s", rec.Code, rec.Body.String())
	}

	// Identity registered in memory under the unchanged internal value.
	if got := fc.ResolveFleet("bridge-tok-abc"); got != "slack-bridge" {
		t.Fatalf("expected identity 'slack-bridge', got %q", got)
	}

	// Response mirrors both nouns with the identical value.
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["bridge"] != "slack-bridge" {
		t.Fatalf("response bridge = %v, want slack-bridge", resp["bridge"])
	}
	if resp["fleet"] != "slack-bridge" {
		t.Fatalf("response fleet (mirror) = %v, want slack-bridge", resp["fleet"])
	}

	// New row persists under the canonical key only.
	if _, err := srv.store.GetConfig(req.Context(), "bridge_token:slack-bridge"); err != nil {
		t.Fatalf("expected canonical bridge_token row, got error: %v", err)
	}
	if _, err := srv.store.GetConfig(req.Context(), "fleet_token:slack-bridge"); err == nil {
		t.Fatalf("did not expect a legacy fleet_token row to be written")
	}
}

func TestRegisterBridgeToken_BridgePreferredOverFleet(t *testing.T) {
	fc := NewFleetConfig("gardenio", nil)
	srv, _ := newTestServerWithFleetAuth(t, "admin-secret", fc)

	// Both fields present — `bridge` wins.
	body, _ := json.Marshal(map[string]any{
		"token":  "bridge-tok-both",
		"bridge": "canonical-name",
		"fleet":  "legacy-name",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/bridge-tokens", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin-secret")
	req = asTokenMinter(req)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.handleRegisterBridgeToken(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if got := fc.ResolveFleet("bridge-tok-both"); got != "canonical-name" {
		t.Fatalf("expected identity 'canonical-name' (bridge preferred), got %q", got)
	}
	if _, err := srv.store.GetConfig(req.Context(), "bridge_token:canonical-name"); err != nil {
		t.Fatalf("expected bridge_token:canonical-name persisted, got error: %v", err)
	}
}

func TestRegisterBridgeToken_LegacyFleetAliasStillAccepted(t *testing.T) {
	fc := NewFleetConfig("gardenio", nil)
	srv, _ := newTestServerWithFleetAuth(t, "admin-secret", fc)

	// Old client sends only `fleet` — still accepted (dual-accept window).
	body, _ := json.Marshal(map[string]any{
		"token": "fleet-tok-legacy",
		"fleet": "old-client",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/fleet-tokens", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin-secret")
	req = asTokenMinter(req)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.handleRegisterBridgeToken(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if got := fc.ResolveFleet("fleet-tok-legacy"); got != "old-client" {
		t.Fatalf("expected identity 'old-client', got %q", got)
	}
	// Even via the legacy alias, the row persists under the canonical key.
	if _, err := srv.store.GetConfig(req.Context(), "bridge_token:old-client"); err != nil {
		t.Fatalf("expected canonical bridge_token row, got error: %v", err)
	}
}

func TestRegisterBridgeToken_MissingNameRejected(t *testing.T) {
	fc := NewFleetConfig("gardenio", nil)
	srv, _ := newTestServerWithFleetAuth(t, "admin-secret", fc)

	body, _ := json.Marshal(map[string]any{"token": "t"})
	req := httptest.NewRequest(http.MethodPost, "/v1/bridge-tokens", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin-secret")
	req = asTokenMinter(req)
	rec := httptest.NewRecorder()

	srv.handleRegisterBridgeToken(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestListBridgeTokens_MirrorsBothFields(t *testing.T) {
	fc := NewFleetConfig("gardenio", nil)
	fc.RegisterToken("tok-a", "bridge-a")
	fc.RegisterToken("tok-b", "bridge-b")
	srv, _ := newTestServerWithFleetAuth(t, "admin-secret", fc)

	req := httptest.NewRequest(http.MethodGet, "/v1/bridge-tokens", nil)
	req.Header.Set("Authorization", "Bearer admin-secret")
	req = asTokenMinter(req)
	rec := httptest.NewRecorder()

	srv.handleListBridgeTokens(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Bridges []string `json:"bridges"`
		Fleets  []string `json:"fleets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Bridges) != 2 {
		t.Fatalf("expected 2 bridges, got %v", resp.Bridges)
	}
	if len(resp.Fleets) != len(resp.Bridges) {
		t.Fatalf("fleets mirror mismatch: bridges=%v fleets=%v", resp.Bridges, resp.Fleets)
	}
}

func TestRevokeBridgeToken_DeletesBothKeys(t *testing.T) {
	fc := NewFleetConfig("gardenio", nil)
	fc.RegisterToken("tok-x", "to-revoke")
	srv, ms := newTestServerWithFleetAuth(t, "admin-secret", fc)

	// Seed a legacy row to prove the revoke clears both shapes.
	legacy := &model.Config{Key: "fleet_token:to-revoke", Value: []byte(`"tok-x"`)}
	if err := ms.SetConfig(context.Background(), legacy); err != nil {
		t.Fatalf("seed legacy config: %v", err)
	}
	canon := &model.Config{Key: "bridge_token:to-revoke", Value: []byte(`{"token":"tok-x"}`)}
	if err := ms.SetConfig(context.Background(), canon); err != nil {
		t.Fatalf("seed canonical config: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/v1/bridge-tokens/to-revoke", nil)
	req.Header.Set("Authorization", "Bearer admin-secret")
	req = asTokenMinter(req)
	req.SetPathValue("bridge", "to-revoke")
	rec := httptest.NewRecorder()

	srv.handleRevokeBridgeToken(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if _, err := ms.GetConfig(context.Background(), "bridge_token:to-revoke"); err == nil {
		t.Fatalf("expected canonical row deleted")
	}
	if _, err := ms.GetConfig(context.Background(), "fleet_token:to-revoke"); err == nil {
		t.Fatalf("expected legacy row deleted")
	}
}

func TestRevokeBridgeToken_LegacyPathParam(t *testing.T) {
	fc := NewFleetConfig("gardenio", nil)
	fc.RegisterToken("tok-y", "legacy-route")
	srv, _ := newTestServerWithFleetAuth(t, "admin-secret", fc)

	// Old clients hit DELETE /v1/fleet-tokens/{fleet}; the handler reads the
	// {fleet} path value when {bridge} is empty.
	req := httptest.NewRequest(http.MethodDelete, "/v1/fleet-tokens/legacy-route", nil)
	req.Header.Set("Authorization", "Bearer admin-secret")
	req = asTokenMinter(req)
	req.SetPathValue("fleet", "legacy-route")
	rec := httptest.NewRecorder()

	srv.handleRevokeBridgeToken(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if got := fc.ResolveFleet("tok-y"); got == "legacy-route" {
		t.Fatalf("expected token revoked from memory, still resolves to %q", got)
	}
}
