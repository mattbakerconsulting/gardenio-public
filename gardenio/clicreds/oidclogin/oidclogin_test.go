package oidclogin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

// A refreshed land session keeps its land: it is saved back to the land's own
// file, and the ordinary session is left alone.
func TestRefreshKeepsLand(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oidc-login.json")
	old := FilePath
	FilePath = func() (string, error) { return path, nil }
	t.Cleanup(func() { FilePath = old })

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]string{"issuer": srv.URL, "token_endpoint": srv.URL + "/token"})
		case "/token":
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "new", "expires_in": 300})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	plain := &Session{Issuer: srv.URL, Daemon: "api.example.com", AccessToken: "plain", Expiry: time.Now().Add(time.Hour)}
	if err := Save(plain); err != nil {
		t.Fatal(err)
	}
	s := &Session{Issuer: srv.URL, ClientID: "c", Daemon: "api.example.com", Land: "acme", AccessToken: "old", RefreshToken: "r1", Expiry: time.Now().Add(-time.Minute)}
	if err := Refresh(context.Background(), srv.Client(), s, time.Now()); err != nil {
		t.Fatal(err)
	}
	if s.Land != "acme" || s.Daemon != "api.example.com" || s.AccessToken != "new" || s.RefreshToken != "r1" {
		t.Fatalf("refreshed session = %+v", s)
	}
	if err := Save(s); err != nil {
		t.Fatal(err)
	}
	if got, err := LoadFor("acme"); err != nil || got == nil || got.AccessToken != "new" {
		t.Fatalf("land session after save = %+v, %v", got, err)
	}
	if got, err := LoadFor(""); err != nil || got == nil || got.AccessToken != "plain" {
		t.Fatalf("ordinary session after save = %+v, %v; a land refresh must not replace it", got, err)
	}
}

// With NoSessionEnv=1 a fresh session for the very land and host asked for is
// not sent: a land agent sharing its person's home runs on its own token.
func TestBearerNoSessionEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oidc-login.json")
	old := FilePath
	FilePath = func() (string, error) { return path, nil }
	t.Cleanup(func() { FilePath = old })
	s := &Session{Daemon: "api.example.com", Land: "acme", AccessToken: "person", Expiry: time.Now().Add(time.Hour)}
	if err := Save(s); err != nil {
		t.Fatal(err)
	}
	target := "https://api.example.com/land/acme"
	if got := Bearer(context.Background(), http.DefaultClient, time.Now(), target, "gb", io.Discard); got != "person" {
		t.Fatalf("Bearer without %s = %q, want the session", NoSessionEnv, got)
	}
	t.Setenv(NoSessionEnv, "1")
	if got := Bearer(context.Background(), http.DefaultClient, time.Now(), target, "gb", io.Discard); got != "" {
		t.Fatalf("Bearer with %s=1 = %q, want none", NoSessionEnv, got)
	}
}
