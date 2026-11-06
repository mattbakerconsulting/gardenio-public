package credstore

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadEmpty(t *testing.T) {
	s, err := Load(filepath.Join(t.TempDir(), "creds.json"))
	if err != nil {
		t.Fatal(err)
	}
	if s.CurrentContext() != "default" {
		t.Errorf("expected default context, got %q", s.CurrentContext())
	}
	if cur := s.Current(); cur != nil {
		t.Errorf("expected nil current context, got %+v", cur)
	}
}

func TestSaveAndLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "creds.json")
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	s.SetContext("default", &Context{
		Server:       "https://beads.example.com",
		GitLabURL:    "https://gitlab.com",
		ClientID:     "test-client",
		AccessToken:  "at-123",
		RefreshToken: "rt-456",
		ExpiresAt:    time.Now().Add(time.Hour).Format(time.RFC3339),
		User: &User{
			Email:    "matt@example.com",
			Username: "matt.baker",
			GitLabID: 42,
		},
	})
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	// Verify file permissions.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("expected 0600 permissions, got %04o", perm)
	}
	// Reload and verify.
	s2, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cur := s2.Current()
	if cur == nil {
		t.Fatal("expected non-nil current context after reload")
	}
	if cur.Server != "https://beads.example.com" {
		t.Errorf("server = %q", cur.Server)
	}
	if cur.User.Email != "matt@example.com" {
		t.Errorf("email = %q", cur.User.Email)
	}
	if cur.User.GitLabID != 42 {
		t.Errorf("gitlab_id = %d", cur.User.GitLabID)
	}
}

func TestMultiContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "creds.json")
	s, _ := Load(path)
	s.SetContext("dev", &Context{Server: "http://localhost:8080"})
	s.SetContext("prod", &Context{Server: "https://beads.prod.example.com"})
	s.SetCurrentContext("prod")
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	s2, _ := Load(path)
	if s2.CurrentContext() != "prod" {
		t.Errorf("expected prod, got %q", s2.CurrentContext())
	}
	if s2.Current().Server != "https://beads.prod.example.com" {
		t.Errorf("server = %q", s2.Current().Server)
	}
	names := s2.ContextNames()
	if len(names) != 2 {
		t.Errorf("expected 2 contexts, got %d", len(names))
	}
}

func TestDeleteContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "creds.json")
	s, _ := Load(path)
	s.SetContext("old", &Context{Server: "http://old"})
	s.DeleteContext("old")
	if c := s.Context("old"); c != nil {
		t.Errorf("expected nil after delete, got %+v", c)
	}
}

func TestIsExpired(t *testing.T) {
	tests := []struct {
		name      string
		expiresAt string
		want      bool
	}{
		{"empty", "", true},
		{"invalid", "not-a-date", true},
		{"past", time.Now().Add(-time.Hour).Format(time.RFC3339), true},
		{"within grace", time.Now().Add(15 * time.Second).Format(time.RFC3339), true},
		{"valid", time.Now().Add(time.Hour).Format(time.RFC3339), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Context{ExpiresAt: tt.expiresAt}
			if got := c.IsExpired(); got != tt.want {
				t.Errorf("IsExpired() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestTokenSourceNotLoggedIn(t *testing.T) {
	s, _ := Load(filepath.Join(t.TempDir(), "creds.json"))
	ts := NewTokenSource(s)
	_, err := ts.Token(context.Background())
	if err == nil {
		t.Fatal("expected error for empty store")
	}
}

func TestTokenSourceValidToken(t *testing.T) {
	s, _ := Load(filepath.Join(t.TempDir(), "creds.json"))
	s.SetContext("default", &Context{
		AccessToken: "valid-token",
		ExpiresAt:   time.Now().Add(time.Hour).Format(time.RFC3339),
	})
	ts := NewTokenSource(s)
	tok, err := ts.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tok != "valid-token" {
		t.Errorf("token = %q, want valid-token", tok)
	}
}

func TestTokenSourceRefresh(t *testing.T) {
	// Set up a mock GitLab token endpoint.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.FormValue("grant_type") != "refresh_token" {
			t.Errorf("grant_type = %q", r.FormValue("grant_type"))
		}
		if r.FormValue("refresh_token") != "old-refresh" {
			t.Errorf("refresh_token = %q", r.FormValue("refresh_token"))
		}
		json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "new-access",
			"refresh_token": "new-refresh",
			"expires_in":    3600,
		})
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "creds.json")
	s, _ := Load(path)
	s.SetContext("default", &Context{
		GitLabURL:    srv.URL,
		ClientID:     "test-client",
		AccessToken:  "expired-token",
		RefreshToken: "old-refresh",
		ExpiresAt:    time.Now().Add(-time.Hour).Format(time.RFC3339),
	})
	ts := NewTokenSource(s)
	tok, err := ts.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tok != "new-access" {
		t.Errorf("token = %q, want new-access", tok)
	}
	// Verify refresh token was updated and saved.
	s2, _ := Load(path)
	cur := s2.Current()
	if cur.RefreshToken != "new-refresh" {
		t.Errorf("refresh_token = %q, want new-refresh", cur.RefreshToken)
	}
}

func TestDefaultPath(t *testing.T) {
	// Verify that DefaultPath returns a non-empty string.
	p := DefaultPath()
	if p == "" {
		t.Error("DefaultPath() returned empty string")
	}
	if filepath.Base(p) != "credentials.json" {
		t.Errorf("DefaultPath() = %q, want credentials.json basename", p)
	}
}

func TestXDGConfigHome(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	p := DefaultPath()
	want := filepath.Join(tmp, "gardenio", "credentials.json")
	if p != want {
		t.Errorf("DefaultPath() = %q, want %q", p, want)
	}
}
