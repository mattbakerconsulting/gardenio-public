package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// fakeKeycloakServer simulates the Keycloak Admin API endpoints needed by
// KeycloakRoleChecker: token endpoint, user lookup by email, and realm
// role mappings.
type fakeKeycloakServer struct {
	srv *httptest.Server

	// Configurable state.
	users map[string]fakeKCUser // email → user
	roles map[string][]string   // userID → role names

	tokenCalls atomic.Int64
}

type fakeKCUser struct {
	ID    string
	Email string
}

func newFakeKeycloakServer() *fakeKeycloakServer {
	fk := &fakeKeycloakServer{
		users: make(map[string]fakeKCUser),
		roles: make(map[string][]string),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/realms/gardenio/protocol/openid-connect/token", fk.handleToken)
	mux.HandleFunc("/admin/realms/gardenio/users", fk.handleUsers)
	mux.HandleFunc("/admin/realms/gardenio/users/", fk.handleUserRoles)
	fk.srv = httptest.NewServer(mux)
	return fk
}

func (fk *fakeKeycloakServer) Close() { fk.srv.Close() }
func (fk *fakeKeycloakServer) URL() string { return fk.srv.URL }

func (fk *fakeKeycloakServer) handleToken(w http.ResponseWriter, _ *http.Request) {
	fk.tokenCalls.Add(1)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"access_token": "fake-token",
		"expires_in":   300,
		"token_type":   "Bearer",
	})
}

func (fk *fakeKeycloakServer) handleUsers(w http.ResponseWriter, r *http.Request) {
	email := r.URL.Query().Get("email")
	w.Header().Set("Content-Type", "application/json")
	for _, u := range fk.users {
		if u.Email == email {
			json.NewEncoder(w).Encode([]map[string]string{{"id": u.ID}})
			return
		}
	}
	json.NewEncoder(w).Encode([]map[string]string{})
}

func (fk *fakeKeycloakServer) handleUserRoles(w http.ResponseWriter, r *http.Request) {
	// Path: /admin/realms/gardenio/users/{userID}/role-mappings/realm
	// Extract userID from path.
	var userID string
	fmt.Sscanf(r.URL.Path, "/admin/realms/gardenio/users/%s/role-mappings/realm", &userID)
	// Remove trailing path components from userID.
	if i := len("/admin/realms/gardenio/users/"); i < len(r.URL.Path) {
		rest := r.URL.Path[i:]
		// rest = "userID/role-mappings/realm"
		for j := 0; j < len(rest); j++ {
			if rest[j] == '/' {
				userID = rest[:j]
				break
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	roleNames := fk.roles[userID]
	var roles []map[string]string
	for _, name := range roleNames {
		roles = append(roles, map[string]string{"name": name})
	}
	if roles == nil {
		roles = []map[string]string{}
	}
	json.NewEncoder(w).Encode(roles)
}

func TestKeycloakRoleChecker_HasRealmRole(t *testing.T) {
	fk := newFakeKeycloakServer()
	defer fk.Close()

	fk.users["alice@example.com"] = fakeKCUser{ID: "kc-alice", Email: "alice@example.com"}
	fk.roles["kc-alice"] = []string{"gardenio-mention", "gardenio-viewer"}

	checker := NewKeycloakRoleChecker(KeycloakConfig{
		BaseURL:      fk.URL(),
		Realm:        "gardenio",
		ClientID:     "test-client",
		ClientSecret: "test-secret",
	}, slog.Default())

	hasRole, err := checker.HasRealmRole(context.Background(), "alice@example.com", "gardenio-mention")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !hasRole {
		t.Error("expected alice to have gardenio-mention role")
	}
}

func TestKeycloakRoleChecker_MissingRole(t *testing.T) {
	fk := newFakeKeycloakServer()
	defer fk.Close()

	fk.users["bob@example.com"] = fakeKCUser{ID: "kc-bob", Email: "bob@example.com"}
	fk.roles["kc-bob"] = []string{"gardenio-viewer"}

	checker := NewKeycloakRoleChecker(KeycloakConfig{
		BaseURL:      fk.URL(),
		Realm:        "gardenio",
		ClientID:     "test-client",
		ClientSecret: "test-secret",
	}, slog.Default())

	hasRole, err := checker.HasRealmRole(context.Background(), "bob@example.com", "gardenio-mention")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hasRole {
		t.Error("expected bob to NOT have gardenio-mention role")
	}
}

func TestKeycloakRoleChecker_UserNotFound(t *testing.T) {
	fk := newFakeKeycloakServer()
	defer fk.Close()

	checker := NewKeycloakRoleChecker(KeycloakConfig{
		BaseURL:      fk.URL(),
		Realm:        "gardenio",
		ClientID:     "test-client",
		ClientSecret: "test-secret",
	}, slog.Default())

	hasRole, err := checker.HasRealmRole(context.Background(), "nobody@example.com", "gardenio-mention")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hasRole {
		t.Error("expected false for non-existent user")
	}
}

func TestKeycloakRoleChecker_CachesResults(t *testing.T) {
	fk := newFakeKeycloakServer()
	defer fk.Close()

	fk.users["cached@example.com"] = fakeKCUser{ID: "kc-cached", Email: "cached@example.com"}
	fk.roles["kc-cached"] = []string{"gardenio-mention"}

	checker := NewKeycloakRoleChecker(KeycloakConfig{
		BaseURL:      fk.URL(),
		Realm:        "gardenio",
		ClientID:     "test-client",
		ClientSecret: "test-secret",
	}, slog.Default())

	// First call: hits the API.
	hasRole, err := checker.HasRealmRole(context.Background(), "cached@example.com", "gardenio-mention")
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if !hasRole {
		t.Fatal("expected true on first call")
	}
	firstTokenCalls := fk.tokenCalls.Load()

	// Second call: should use cache (token call count should not increase).
	hasRole, err = checker.HasRealmRole(context.Background(), "cached@example.com", "gardenio-mention")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if !hasRole {
		t.Fatal("expected true on cached call")
	}
	if fk.tokenCalls.Load() != firstTokenCalls {
		t.Error("expected cached result — token endpoint should not have been called again")
	}
}

func TestKeycloakConfig_IsConfigured(t *testing.T) {
	tests := []struct {
		name string
		cfg  KeycloakConfig
		want bool
	}{
		{"all set", KeycloakConfig{"https://kc", "realm", "id", "secret"}, true},
		{"missing url", KeycloakConfig{"", "realm", "id", "secret"}, false},
		{"missing realm", KeycloakConfig{"https://kc", "", "id", "secret"}, false},
		{"missing client id", KeycloakConfig{"https://kc", "realm", "", "secret"}, false},
		{"missing secret", KeycloakConfig{"https://kc", "realm", "id", ""}, false},
		{"all empty", KeycloakConfig{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.IsConfigured(); got != tt.want {
				t.Errorf("IsConfigured() = %v, want %v", got, tt.want)
			}
		})
	}
}
