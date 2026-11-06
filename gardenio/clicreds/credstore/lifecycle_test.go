package credstore

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

// Integration tests for the full OAuth credential lifecycle:
// login → store → use → refresh → logout → re-login (d40c84a8)

func TestLifecycle_LoginStoreUseRefreshLogoutRelogin(t *testing.T) {
	// Mock GitLab token endpoint: handles initial exchange and refresh.
	var refreshCount int
	gitlab := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" {
			http.NotFound(w, r)
			return
		}
		grantType := r.FormValue("grant_type")
		switch grantType {
		case "refresh_token":
			refreshCount++
			if r.FormValue("refresh_token") == "" {
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{
				"access_token":  "refreshed-at-" + r.FormValue("refresh_token"),
				"refresh_token": "new-rt-2",
				"id_token":      "refreshed-id-token",
				"expires_in":    3600,
			})
		default:
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "unsupported_grant_type"})
		}
	}))
	defer gitlab.Close()

	path := filepath.Join(t.TempDir(), "creds.json")

	// === Phase 1: Login (simulate storing tokens after OAuth completes) ===
	store, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	store.SetContext("default", &Context{
		Server:       "https://beads.test.example.com",
		GitLabURL:    gitlab.URL,
		ClientID:     "test-client-id",
		AccessToken:  "initial-at",
		IDToken:      "initial-id-token",
		RefreshToken: "initial-rt",
		ExpiresAt:    time.Now().Add(time.Hour).Format(time.RFC3339),
		User: &User{
			Email:    "dev@example.com",
			Username: "developer",
			GitLabID: 100,
			Name:     "Test Developer",
		},
	})
	if err := store.Save(); err != nil {
		t.Fatalf("Save after login: %v", err)
	}

	// === Phase 2: Use stored credentials ===
	ts := NewTokenSource(store)
	tok, err := ts.Token(context.Background())
	if err != nil {
		t.Fatalf("Token (valid): %v", err)
	}
	// Should return id_token (preferred over access_token).
	if tok != "initial-id-token" {
		t.Errorf("expected id_token 'initial-id-token', got %q", tok)
	}

	// Verify persistence: reload from disk.
	store2, err := Load(path)
	if err != nil {
		t.Fatalf("Load (verify): %v", err)
	}
	cur := store2.Current()
	if cur == nil {
		t.Fatal("expected non-nil context after reload")
	}
	if cur.Server != "https://beads.test.example.com" {
		t.Errorf("server = %q", cur.Server)
	}
	if cur.User.Email != "dev@example.com" {
		t.Errorf("user email = %q", cur.User.Email)
	}

	// === Phase 3: Token refresh (expire the token, then call Token()) ===
	store.SetContext("default", &Context{
		Server:       "https://beads.test.example.com",
		GitLabURL:    gitlab.URL,
		ClientID:     "test-client-id",
		AccessToken:  "expired-at",
		IDToken:      "expired-id-token",
		RefreshToken: "initial-rt",
		ExpiresAt:    time.Now().Add(-time.Hour).Format(time.RFC3339), // expired
		User: &User{
			Email:    "dev@example.com",
			Username: "developer",
		},
	})

	tok, err = ts.Token(context.Background())
	if err != nil {
		t.Fatalf("Token (refresh): %v", err)
	}
	// After refresh, should return the new id_token.
	if tok != "refreshed-id-token" {
		t.Errorf("expected refreshed id_token, got %q", tok)
	}
	if refreshCount != 1 {
		t.Errorf("expected 1 refresh call, got %d", refreshCount)
	}

	// Verify refresh persisted: new tokens should be on disk.
	store3, err := Load(path)
	if err != nil {
		t.Fatalf("Load (post-refresh): %v", err)
	}
	cur3 := store3.Current()
	if cur3.RefreshToken != "new-rt-2" {
		t.Errorf("refresh_token not updated on disk: got %q", cur3.RefreshToken)
	}
	if cur3.IDToken != "refreshed-id-token" {
		t.Errorf("id_token not updated on disk: got %q", cur3.IDToken)
	}

	// === Phase 4: Logout (delete context and save) ===
	store.DeleteContext("default")
	if err := store.Save(); err != nil {
		t.Fatalf("Save after logout: %v", err)
	}

	// Token should fail after logout.
	_, err = ts.Token(context.Background())
	if err == nil {
		t.Fatal("expected error after logout, got nil")
	}

	// Verify logout persisted: reload from disk.
	store4, err := Load(path)
	if err != nil {
		t.Fatalf("Load (post-logout): %v", err)
	}
	if store4.Current() != nil {
		t.Error("expected nil context after logout reload")
	}

	// === Phase 5: Re-login after logout ===
	store.SetContext("default", &Context{
		Server:       "https://beads.new-server.example.com",
		GitLabURL:    gitlab.URL,
		ClientID:     "test-client-id",
		AccessToken:  "relogin-at",
		IDToken:      "relogin-id-token",
		RefreshToken: "relogin-rt",
		ExpiresAt:    time.Now().Add(time.Hour).Format(time.RFC3339),
		User: &User{
			Email:    "dev@example.com",
			Username: "developer",
		},
	})
	if err := store.Save(); err != nil {
		t.Fatalf("Save after re-login: %v", err)
	}

	tok, err = ts.Token(context.Background())
	if err != nil {
		t.Fatalf("Token (re-login): %v", err)
	}
	if tok != "relogin-id-token" {
		t.Errorf("expected relogin id_token, got %q", tok)
	}
}

func TestLifecycle_IDTokenPreferredOverAccessToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "creds.json")
	store, _ := Load(path)
	store.SetContext("default", &Context{
		AccessToken: "opaque-at",
		IDToken:     "jwt-id-token",
		ExpiresAt:   time.Now().Add(time.Hour).Format(time.RFC3339),
	})

	ts := NewTokenSource(store)
	tok, err := ts.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tok != "jwt-id-token" {
		t.Errorf("expected id_token, got %q", tok)
	}
}

func TestLifecycle_FallsBackToAccessTokenWhenNoIDToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "creds.json")
	store, _ := Load(path)
	store.SetContext("default", &Context{
		AccessToken: "opaque-at",
		// IDToken intentionally empty.
		ExpiresAt: time.Now().Add(time.Hour).Format(time.RFC3339),
	})

	ts := NewTokenSource(store)
	tok, err := ts.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tok != "opaque-at" {
		t.Errorf("expected access_token fallback, got %q", tok)
	}
}

func TestLifecycle_RefreshWithNoRefreshToken_Errors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "creds.json")
	store, _ := Load(path)
	store.SetContext("default", &Context{
		AccessToken: "expired-at",
		ExpiresAt:   time.Now().Add(-time.Hour).Format(time.RFC3339),
		// RefreshToken intentionally empty.
	})

	ts := NewTokenSource(store)
	_, err := ts.Token(context.Background())
	if err == nil {
		t.Fatal("expected error when token expired and no refresh token")
	}
}

func TestLifecycle_RefreshEndpointFails_Errors(t *testing.T) {
	gitlab := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer gitlab.Close()

	path := filepath.Join(t.TempDir(), "creds.json")
	store, _ := Load(path)
	store.SetContext("default", &Context{
		GitLabURL:    gitlab.URL,
		ClientID:     "client-1",
		AccessToken:  "expired-at",
		RefreshToken: "bad-rt",
		ExpiresAt:    time.Now().Add(-time.Hour).Format(time.RFC3339),
	})

	ts := NewTokenSource(store)
	_, err := ts.Token(context.Background())
	if err == nil {
		t.Fatal("expected error when refresh endpoint returns 401")
	}
}

func TestLifecycle_ContextSwitching(t *testing.T) {
	path := filepath.Join(t.TempDir(), "creds.json")
	store, _ := Load(path)

	// Store two contexts with different servers.
	store.SetContext("dev", &Context{
		Server:      "http://localhost:8080",
		AccessToken: "dev-at",
		IDToken:     "dev-id",
		ExpiresAt:   time.Now().Add(time.Hour).Format(time.RFC3339),
	})
	store.SetContext("prod", &Context{
		Server:      "https://beads.prod.example.com",
		AccessToken: "prod-at",
		IDToken:     "prod-id",
		ExpiresAt:   time.Now().Add(time.Hour).Format(time.RFC3339),
	})

	// Start on dev.
	store.SetCurrentContext("dev")
	ts := NewTokenSource(store)
	tok, _ := ts.Token(context.Background())
	if tok != "dev-id" {
		t.Errorf("expected dev id_token, got %q", tok)
	}

	// Switch to prod.
	store.SetCurrentContext("prod")
	tok, _ = ts.Token(context.Background())
	if tok != "prod-id" {
		t.Errorf("expected prod id_token, got %q", tok)
	}

	// Verify persistence across save/load.
	store.Save()
	store2, _ := Load(path)
	if store2.CurrentContext() != "prod" {
		t.Errorf("expected prod after reload, got %q", store2.CurrentContext())
	}
}

func TestLifecycle_RefreshUpdatesRotatedRefreshToken(t *testing.T) {
	// GitLab rotates refresh tokens — verify the new one is saved.
	callNum := 0
	gitlab := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callNum++
		json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "at-" + r.FormValue("refresh_token"),
			"refresh_token": "rotated-rt-" + string(rune('0'+callNum)),
			"id_token":      "id-" + r.FormValue("refresh_token"),
			"expires_in":    3600,
		})
	}))
	defer gitlab.Close()

	path := filepath.Join(t.TempDir(), "creds.json")
	store, _ := Load(path)
	store.SetContext("default", &Context{
		GitLabURL:    gitlab.URL,
		ClientID:     "client-1",
		AccessToken:  "expired",
		RefreshToken: "rt-original",
		ExpiresAt:    time.Now().Add(-time.Hour).Format(time.RFC3339),
	})

	ts := NewTokenSource(store)

	// First refresh.
	tok, err := ts.Token(context.Background())
	if err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	if tok != "id-rt-original" {
		t.Errorf("first token = %q", tok)
	}

	// Check rotated refresh token was saved.
	reloaded, _ := Load(path)
	if reloaded.Current().RefreshToken != "rotated-rt-1" {
		t.Errorf("refresh_token after first refresh = %q", reloaded.Current().RefreshToken)
	}

	// Expire the token again to force second refresh.
	cur := store.Current()
	cur.ExpiresAt = time.Now().Add(-time.Hour).Format(time.RFC3339)

	tok, err = ts.Token(context.Background())
	if err != nil {
		t.Fatalf("second refresh: %v", err)
	}
	// Second refresh should use the rotated token.
	if tok != "id-rotated-rt-1" {
		t.Errorf("second token = %q (should use rotated refresh token)", tok)
	}
}
