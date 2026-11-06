package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"gardenio/controller/internal/config"
)

func writeTokenFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "token")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	return p
}

// TestRevokeZitiLease_NoopWhenUnconfigured: with no broker URL / no token file /
// revoke disabled, the call is a clean no-op (returns nil, makes no request).
func TestRevokeZitiLease_NoopWhenUnconfigured(t *testing.T) {
	cases := []*config.Config{
		{ZitiAccessBrokerURL: "", ZitiAccessRevokeOnExit: true, ZitiAccessControllerTokenFile: "/x"},
		{ZitiAccessBrokerURL: "http://b", ZitiAccessRevokeOnExit: false, ZitiAccessControllerTokenFile: "/x"},
		{ZitiAccessBrokerURL: "http://b", ZitiAccessRevokeOnExit: true, ZitiAccessControllerTokenFile: ""},
	}
	for i, cfg := range cases {
		if err := revokeZitiLease(context.Background(), slog.Default(), cfg, "gd-1"); err != nil {
			t.Errorf("case %d: expected nil no-op, got %v", i, err)
		}
	}
}

// TestRevokeZitiLease_NoopEmptyTask: empty task id is a no-op even when fully
// configured (nothing to revoke).
func TestRevokeZitiLease_NoopEmptyTask(t *testing.T) {
	cfg := &config.Config{
		ZitiAccessBrokerURL:           "http://b",
		ZitiAccessRevokeOnExit:        true,
		ZitiAccessControllerTokenFile: writeTokenFile(t, "tok"),
	}
	if err := revokeZitiLease(context.Background(), slog.Default(), cfg, "  "); err != nil {
		t.Errorf("empty task should be a no-op, got %v", err)
	}
}

// TestRevokeZitiLease_Success: a configured revoke issues DELETE
// /lease?task_id=<id> with the controller's bearer token and treats 204 as
// success.
func TestRevokeZitiLease_Success(t *testing.T) {
	var gotMethod, gotPath, gotQuery, gotAuth string
	var called atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Add(1)
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotQuery = r.URL.Query().Get("task_id")
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	cfg := &config.Config{
		ZitiAccessBrokerURL:           srv.URL,
		ZitiAccessRevokeOnExit:        true,
		ZitiAccessControllerTokenFile: writeTokenFile(t, "ctrl-token-xyz\n"),
	}
	if err := revokeZitiLease(context.Background(), slog.Default(), cfg, "gd-task-9"); err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if called.Load() != 1 {
		t.Fatalf("broker called %d times, want 1", called.Load())
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("method = %q, want DELETE", gotMethod)
	}
	if gotPath != "/lease" {
		t.Errorf("path = %q, want /lease", gotPath)
	}
	if gotQuery != "gd-task-9" {
		t.Errorf("task_id query = %q, want gd-task-9", gotQuery)
	}
	if gotAuth != "Bearer ctrl-token-xyz" {
		t.Errorf("auth = %q, want trimmed bearer token", gotAuth)
	}
}

// TestRevokeZitiLease_Non204IsError: a 403 (controller not a revoker subject)
// surfaces as an error so it is logged, but the helper still does not panic.
func TestRevokeZitiLease_Non204IsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	cfg := &config.Config{
		ZitiAccessBrokerURL:           srv.URL,
		ZitiAccessRevokeOnExit:        true,
		ZitiAccessControllerTokenFile: writeTokenFile(t, "tok"),
	}
	err := revokeZitiLease(context.Background(), slog.Default(), cfg, "gd-1")
	if err == nil {
		t.Fatal("expected error on 403")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error should mention status 403, got %v", err)
	}
}

// TestRevokeZitiLease_MissingTokenFile: a configured-but-unreadable token file
// returns an error and makes no request.
func TestRevokeZitiLease_MissingTokenFile(t *testing.T) {
	cfg := &config.Config{
		ZitiAccessBrokerURL:           "http://127.0.0.1:0",
		ZitiAccessRevokeOnExit:        true,
		ZitiAccessControllerTokenFile: "/nonexistent/token/file",
	}
	if err := revokeZitiLease(context.Background(), slog.Default(), cfg, "gd-1"); err == nil {
		t.Error("expected error when token file is unreadable")
	}
}
