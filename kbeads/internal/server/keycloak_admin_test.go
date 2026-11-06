package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newMockKeycloak(t *testing.T) (*httptest.Server, *KeycloakAdminClient) {
	t.Helper()

	mux := http.NewServeMux()

	// Token endpoint
	mux.HandleFunc("POST /realms/gardenio/protocol/openid-connect/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "admin-tok-123",
			"expires_in":   300,
		})
	})

	// Find user by email
	mux.HandleFunc("GET /admin/realms/gardenio/users", func(w http.ResponseWriter, r *http.Request) {
		email := r.URL.Query().Get("email")
		username := r.URL.Query().Get("username")

		if email == "alice@example.com" || username == "alice" {
			json.NewEncoder(w).Encode([]keycloakUser{{
				ID:       "user-abc-123",
				Username: "alice",
				Email:    "alice@example.com",
				Enabled:  true,
			}})
			return
		}
		json.NewEncoder(w).Encode([]keycloakUser{})
	})

	// List user sessions
	mux.HandleFunc("GET /admin/realms/gardenio/users/{id}/sessions", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]keycloakSession{
			{ID: "sess-1", Username: "alice", IPAddress: "10.0.0.1"},
			{ID: "sess-2", Username: "alice", IPAddress: "10.0.0.2"},
		})
	})

	// Logout user (revoke sessions)
	mux.HandleFunc("DELETE /admin/realms/gardenio/users/{id}/sessions", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	// Disable user
	mux.HandleFunc("PUT /admin/realms/gardenio/users/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	srv := httptest.NewServer(mux)

	client := NewKeycloakAdminClient(KeycloakAdminConfig{
		BaseURL:      srv.URL,
		Realm:        "gardenio",
		ClientID:     "admin-client",
		ClientSecret: "admin-secret",
	})

	return srv, client
}

func TestKeycloakAdminClient_FindUserByEmail(t *testing.T) {
	srv, client := newMockKeycloak(t)
	defer srv.Close()

	t.Run("Found", func(t *testing.T) {
		user, err := client.FindUserByEmail(context.Background(), "alice@example.com")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if user == nil {
			t.Fatal("expected user, got nil")
		}
		if user.ID != "user-abc-123" {
			t.Fatalf("expected user ID 'user-abc-123', got %q", user.ID)
		}
		if user.Email != "alice@example.com" {
			t.Fatalf("expected email 'alice@example.com', got %q", user.Email)
		}
	})

	t.Run("NotFound", func(t *testing.T) {
		user, err := client.FindUserByEmail(context.Background(), "nobody@example.com")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if user != nil {
			t.Fatalf("expected nil, got user %+v", user)
		}
	})
}

func TestKeycloakAdminClient_FindUserByUsername(t *testing.T) {
	srv, client := newMockKeycloak(t)
	defer srv.Close()

	user, err := client.FindUserByUsername(context.Background(), "alice")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if user == nil || user.Username != "alice" {
		t.Fatalf("expected user 'alice', got %+v", user)
	}
}

func TestKeycloakAdminClient_ListUserSessions(t *testing.T) {
	srv, client := newMockKeycloak(t)
	defer srv.Close()

	sessions, err := client.ListUserSessions(context.Background(), "user-abc-123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(sessions))
	}
}

func TestKeycloakAdminClient_RevokeUser(t *testing.T) {
	srv, client := newMockKeycloak(t)
	defer srv.Close()

	t.Run("RevokeByEmail", func(t *testing.T) {
		result, err := client.RevokeUser(context.Background(), "alice@example.com", "", false)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result.UserID != "user-abc-123" {
			t.Fatalf("expected user ID 'user-abc-123', got %q", result.UserID)
		}
		if result.SessionsRevoked != 2 {
			t.Fatalf("expected 2 sessions revoked, got %d", result.SessionsRevoked)
		}
		if result.UserDisabled {
			t.Fatal("expected user not disabled")
		}
	})

	t.Run("RevokeAndDisable", func(t *testing.T) {
		result, err := client.RevokeUser(context.Background(), "alice@example.com", "", true)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !result.UserDisabled {
			t.Fatal("expected user to be disabled")
		}
	})

	t.Run("UserNotFound", func(t *testing.T) {
		_, err := client.RevokeUser(context.Background(), "nobody@example.com", "", false)
		if err == nil {
			t.Fatal("expected error for nonexistent user")
		}
		if err.Error() != "user not found" {
			t.Fatalf("expected 'user not found', got %q", err.Error())
		}
	})

	t.Run("NoIdentifier", func(t *testing.T) {
		_, err := client.RevokeUser(context.Background(), "", "", false)
		if err == nil {
			t.Fatal("expected error when no email or username provided")
		}
	})
}

func TestKeycloakAdminConfig_IsConfigured(t *testing.T) {
	t.Run("AllSet", func(t *testing.T) {
		cfg := KeycloakAdminConfig{
			BaseURL:      "https://kc.example.com",
			Realm:        "test",
			ClientID:     "admin",
			ClientSecret: "secret",
		}
		if !cfg.IsConfigured() {
			t.Fatal("expected IsConfigured=true")
		}
	})

	t.Run("MissingURL", func(t *testing.T) {
		cfg := KeycloakAdminConfig{
			Realm:        "test",
			ClientID:     "admin",
			ClientSecret: "secret",
		}
		if cfg.IsConfigured() {
			t.Fatal("expected IsConfigured=false when URL missing")
		}
	})

	t.Run("AllEmpty", func(t *testing.T) {
		cfg := KeycloakAdminConfig{}
		if cfg.IsConfigured() {
			t.Fatal("expected IsConfigured=false when all empty")
		}
	})
}
