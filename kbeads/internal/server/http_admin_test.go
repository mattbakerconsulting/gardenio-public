package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"gardenio/kbeads/internal/events"
)

func TestHandleRevokeUser(t *testing.T) {
	// Set up a mock Keycloak server and wire it into the beads server.
	kcSrv, kcClient := newMockKeycloak(t)
	defer kcSrv.Close()

	fc := NewFleetConfig("gardenio", nil)
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	srv.SetFleetAuth(fc)
	srv.SetKeycloakAdmin(kcClient)

	t.Run("RevokeByEmail", func(t *testing.T) {
		body, _ := json.Marshal(revokeUserRequest{
			Email:  "alice@example.com",
			Reason: "offboarding",
		})
		req := httptest.NewRequest(http.MethodPost, "/v1/admin/revoke-user", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer admin-secret")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()

		srv.handleRevokeUser(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
		}

		var resp revokeUserResponse
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if resp.UserID != "user-abc-123" {
			t.Fatalf("expected user_id 'user-abc-123', got %q", resp.UserID)
		}
		if resp.SessionsRevoked != 2 {
			t.Fatalf("expected 2 sessions revoked, got %d", resp.SessionsRevoked)
		}
		if resp.UserDisabled {
			t.Fatal("expected user not disabled")
		}
	})

	t.Run("RevokeAndDisable", func(t *testing.T) {
		body, _ := json.Marshal(revokeUserRequest{
			Email:   "alice@example.com",
			Disable: true,
			Reason:  "terminated",
		})
		req := httptest.NewRequest(http.MethodPost, "/v1/admin/revoke-user", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer admin-secret")
		rec := httptest.NewRecorder()

		srv.handleRevokeUser(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
		}

		var resp revokeUserResponse
		json.NewDecoder(rec.Body).Decode(&resp)
		if !resp.UserDisabled {
			t.Fatal("expected user to be disabled")
		}
	})

	t.Run("DryRun", func(t *testing.T) {
		body, _ := json.Marshal(revokeUserRequest{
			Email:  "alice@example.com",
			DryRun: true,
		})
		req := httptest.NewRequest(http.MethodPost, "/v1/admin/revoke-user", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer admin-secret")
		rec := httptest.NewRecorder()

		srv.handleRevokeUser(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
		}

		var resp revokeUserResponse
		json.NewDecoder(rec.Body).Decode(&resp)
		if !resp.DryRun {
			t.Fatal("expected dry_run=true in response")
		}
		if resp.SessionsRevoked != 2 {
			t.Fatalf("expected 2 sessions in preview, got %d", resp.SessionsRevoked)
		}
	})

	t.Run("UserNotFound", func(t *testing.T) {
		body, _ := json.Marshal(revokeUserRequest{
			Email: "nobody@example.com",
		})
		req := httptest.NewRequest(http.MethodPost, "/v1/admin/revoke-user", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer admin-secret")
		rec := httptest.NewRecorder()

		srv.handleRevokeUser(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d; body: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("MissingEmailAndUsername", func(t *testing.T) {
		body, _ := json.Marshal(revokeUserRequest{})
		req := httptest.NewRequest(http.MethodPost, "/v1/admin/revoke-user", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer admin-secret")
		rec := httptest.NewRecorder()

		srv.handleRevokeUser(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", rec.Code)
		}
	})

	t.Run("KeycloakNotConfigured", func(t *testing.T) {
		noKcMs := newMockStore()
		noKcSrv := NewBeadsServer(noKcMs, &events.NoopPublisher{})
		noKcSrv.SetFleetAuth(fc)
		// Don't set keycloak admin client

		body, _ := json.Marshal(revokeUserRequest{Email: "alice@example.com"})
		req := httptest.NewRequest(http.MethodPost, "/v1/admin/revoke-user", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer admin-secret")
		rec := httptest.NewRecorder()

		noKcSrv.handleRevokeUser(rec, req)

		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("expected 503, got %d; body: %s", rec.Code, rec.Body.String())
		}
	})
}
