package runnerreg

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSaveLoad_TokenFileIs0600AndNeverInJSON(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GARDENIO_HOME", dir)
	r := Runner{RunnerID: "gd-r1", Hostname: "laptop", Owner: "oidc:a@example.com", DaemonURL: "https://runners.example.com", Projects: []string{"pet"}}
	if err := Save(r, "gr_secret"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"runner.token", "runner.json"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s must be mode 0600: %v %v", name, info, err)
		}
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "runner.json"))
	if string(raw) == "" || !json.Valid(raw) || strings.Contains(string(raw), "gr_secret") {
		t.Fatalf("runner.json must be valid JSON without the token: %s", raw)
	}
	got, tok, err := Load()
	if err != nil || tok != "gr_secret" || got.RunnerID != "gd-r1" {
		t.Fatalf("Load: %+v %q %v", got, tok, err)
	}
}

func TestSave_TightensAnExistingTokenFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GARDENIO_HOME", dir)
	p := filepath.Join(dir, "runner.token")
	if err := os.WriteFile(p, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Save(Runner{RunnerID: "gd-r1"}, "gr_new"); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(p); info.Mode().Perm() != 0o600 {
		t.Fatalf("re-registering must leave runner.token 0600, got %v", info.Mode().Perm())
	}
}

func TestDir_RefusesWithoutHome(t *testing.T) {
	t.Setenv("GARDENIO_HOME", "")
	t.Setenv("HOME", "")
	if d, err := Dir(); !errors.Is(err, ErrNoHome) {
		t.Fatalf("Dir() = %q, %v; want ErrNoHome, not a temp-dir fallback", d, err)
	}
	if err := Save(Runner{RunnerID: "gd-r1"}, "gr_x"); !errors.Is(err, ErrNoHome) {
		t.Fatalf("Save must refuse without a home: %v", err)
	}
	if _, _, err := Load(); !errors.Is(err, ErrNoHome) {
		t.Fatalf("Load must refuse without a home: %v", err)
	}
}

func TestLoad_RefusesAnEmptyRunnerID(t *testing.T) {
	t.Setenv("GARDENIO_HOME", t.TempDir())
	if err := Save(Runner{RunnerID: " ", DaemonURL: "https://runners.example.com"}, "gr_x"); err != nil {
		t.Fatal(err)
	}
	if _, tok, err := Load(); err == nil || !strings.Contains(err.Error(), "no runner_id") || tok != "" {
		t.Fatalf("Load must refuse an empty runner_id without returning the token: %q %v", tok, err)
	}
}

func TestMarkLeft_LeftSince(t *testing.T) {
	t.Setenv("GARDENIO_HOME", t.TempDir())
	before := time.Now().Add(-time.Second)
	if LeftSince(before) {
		t.Fatal("no marker yet must read as not left")
	}
	if err := MarkLeft(); err != nil {
		t.Fatal(err)
	}
	if !LeftSince(before) {
		t.Fatal("a leave after t must read as left")
	}
	if LeftSince(time.Now().Add(time.Hour)) {
		t.Fatal("a join started after the leave must not read it as left")
	}
}

func TestLoad_NotRegistered(t *testing.T) {
	t.Setenv("GARDENIO_HOME", t.TempDir())
	if _, _, err := Load(); !errors.Is(err, ErrNotRegistered) {
		t.Fatalf("expected ErrNotRegistered, got %v", err)
	}
}

func TestClient_Heartbeat401IsTokenRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer gr_x" {
			t.Errorf("bearer not sent: %q", r.Header.Get("Authorization"))
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	c := Client{BaseURL: srv.URL, Token: "gr_x"}
	if err := c.Heartbeat(context.Background(), "gd-r1", ""); !errors.Is(err, ErrTokenRejected) {
		t.Fatalf("expected ErrTokenRejected, got %v", err)
	}
}

func TestClient_Heartbeat409RevokedIsTokenRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
	}))
	defer srv.Close()
	if err := (Client{BaseURL: srv.URL, Token: "gr_x"}).Heartbeat(context.Background(), "gd-r1", ""); !errors.Is(err, ErrTokenRejected) {
		t.Fatalf("expected ErrTokenRejected, got %v", err)
	}
}

func TestClient_HeartbeatAwaySendsState(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/runners/gd-r1/heartbeat" {
			t.Errorf("wrong route: %s %s", r.Method, r.URL.Path)
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["state"] != "away" {
			t.Errorf("state not sent: %v", body)
		}
		_, _ = w.Write([]byte(`{"state":"away"}`))
	}))
	defer srv.Close()
	if err := (Client{BaseURL: srv.URL + "/", Token: "gr_x"}).Heartbeat(context.Background(), "gd-r1", "away"); err != nil {
		t.Fatal(err)
	}
}

func TestClient_OtherErrorsCarryTheDaemonMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"runner may only heartbeat itself"}`))
	}))
	defer srv.Close()
	err := (Client{BaseURL: srv.URL, Token: "gr_x"}).Heartbeat(context.Background(), "gd-r2", "")
	if err == nil || errors.Is(err, ErrTokenRejected) || !strings.Contains(err.Error(), "only heartbeat itself") {
		t.Fatalf("got %v", err)
	}
}

func TestClient_Register_SendsHostAndReturnsTokenOnce(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("register must not send a bearer: %q", r.Header.Get("Authorization"))
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["invite_code"] != "GRI-A" || body["hostname"] != "laptop" || body["os_arch"] != "linux/amd64" {
			t.Errorf("body wrong: %v", body)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"runner_id": "gd-r1", "token": "gr_new", "owner": "oidc:a@example.com", "projects": []string{"pet"}})
	}))
	defer srv.Close()
	r, tok, err := Client{BaseURL: srv.URL}.Register(context.Background(), "GRI-A", "laptop", "linux/amd64", nil)
	if err != nil || tok != "gr_new" || r.RunnerID != "gd-r1" || r.DaemonURL != srv.URL || r.Owner != "oidc:a@example.com" {
		t.Fatalf("Register: %+v %q %v", r, tok, err)
	}
}

func TestClient_Register_UnknownInviteIsNotATokenError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"invite not found, expired, or already used"}`))
	}))
	defer srv.Close()
	_, _, err := Client{BaseURL: srv.URL}.Register(context.Background(), "GRI-X", "laptop", "linux/amd64", nil)
	if err == nil || errors.Is(err, ErrTokenRejected) || !strings.Contains(err.Error(), "invite not found") {
		t.Fatalf("got %v", err)
	}
}

func TestClient_Get(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/runners/gd-r1" {
			t.Errorf("wrong route: %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"id":"gd-r1","fields":{"state":"online"}}`))
	}))
	defer srv.Close()
	got, err := Client{BaseURL: srv.URL, Token: "gr_x"}.Get(context.Background(), "gd-r1")
	if err != nil || got["id"] != "gd-r1" {
		t.Fatalf("Get: %v %v", got, err)
	}
}
