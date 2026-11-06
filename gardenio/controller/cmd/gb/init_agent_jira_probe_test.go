package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// withJiraEnv sets JIRA env vars for the duration of the test.
func withJiraEnv(t *testing.T, baseURL, email, token, ticket string) {
	t.Helper()
	t.Setenv("JIRA_BASE_URL", baseURL)
	t.Setenv("JIRA_EMAIL", email)
	t.Setenv("JIRA_API_TOKEN", token)
	t.Setenv("JIRA_SMOKE_TICKET", ticket)
}

func TestProbeJIRA_CredsUnset_NoOp(t *testing.T) {
	// All three env vars empty.
	t.Setenv("JIRA_BASE_URL", "")
	t.Setenv("JIRA_EMAIL", "")
	t.Setenv("JIRA_API_TOKEN", "")

	// Hijack http client to a server that records hits — must NOT be called.
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	prevClient := jiraProbeHTTPClient
	jiraProbeHTTPClient = &http.Client{Timeout: 2 * time.Second}
	defer func() { jiraProbeHTTPClient = prevClient }()

	probeJIRA(context.Background())

	if atomic.LoadInt32(&hits) != 0 {
		t.Fatalf("expected no HTTP calls when creds unset, got %d", hits)
	}
}

// fakeDaemonRoundTrip stubs the beadsapi daemon HTTP backend so we can assert
// that the probe creates+closes a squawk bead on permission failure.
type fakeDaemonHandler struct {
	createCount int32
	closeCount  int32
	lastCreate  beadsapi.CreateBeadRequest
}

func (h *fakeDaemonHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/beads":
		atomic.AddInt32(&h.createCount, 1)
		_ = json.NewDecoder(r.Body).Decode(&h.lastCreate)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "fake-1"})
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/close"):
		atomic.AddInt32(&h.closeCount, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	default:
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}
}

// installFakeDaemon swaps the package-level `daemon` for a client pointed at
// the given handler, restoring on cleanup.
func installFakeDaemon(t *testing.T, handler http.Handler) (*httptest.Server, func()) {
	t.Helper()
	srv := httptest.NewServer(handler)
	prev := daemon
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		srv.Close()
		t.Fatalf("beadsapi.New: %v", err)
	}
	daemon = c
	cleanup := func() {
		daemon = prev
		c.Close()
		srv.Close()
	}
	return srv, cleanup
}

func TestProbeJIRA_PermissionFailure_Squawks(t *testing.T) {
	for _, status := range []int{
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusNotFound,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			jiraSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"errorMessages":["nope"]}`))
			}))
			defer jiraSrv.Close()

			withJiraEnv(t, jiraSrv.URL, "svc@example.com", "tok", "PE-1")
			prevClient := jiraProbeHTTPClient
			jiraProbeHTTPClient = &http.Client{Timeout: 2 * time.Second}
			defer func() { jiraProbeHTTPClient = prevClient }()

			handler := &fakeDaemonHandler{}
			_, cleanup := installFakeDaemon(t, handler)
			defer cleanup()

			probeJIRA(context.Background())

			if got := atomic.LoadInt32(&handler.createCount); got != 1 {
				t.Fatalf("expected exactly 1 squawk-create call for HTTP %d, got %d", status, got)
			}
			if got := atomic.LoadInt32(&handler.closeCount); got != 1 {
				t.Fatalf("expected exactly 1 close call for HTTP %d, got %d", status, got)
			}
			if handler.lastCreate.Type != "message" {
				t.Errorf("squawk bead Type = %q, want %q", handler.lastCreate.Type, "message")
			}
			if !strings.Contains(handler.lastCreate.Title, "JIRA smoke probe FAILED") {
				t.Errorf("squawk title missing FAILED marker: %q", handler.lastCreate.Title)
			}
			if !slices.Contains(handler.lastCreate.Labels, "jira-smoke") {
				t.Errorf("squawk labels missing jira-smoke: %v", handler.lastCreate.Labels)
			}
		})
	}
}

func TestProbeJIRA_Success_NoSquawk(t *testing.T) {
	jiraSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"key":"PE-1","fields":{"summary":"ok"}}`))
	}))
	defer jiraSrv.Close()

	withJiraEnv(t, jiraSrv.URL, "svc@example.com", "tok", "PE-1")
	prevClient := jiraProbeHTTPClient
	jiraProbeHTTPClient = &http.Client{Timeout: 2 * time.Second}
	defer func() { jiraProbeHTTPClient = prevClient }()

	handler := &fakeDaemonHandler{}
	_, cleanup := installFakeDaemon(t, handler)
	defer cleanup()

	probeJIRA(context.Background())

	if got := atomic.LoadInt32(&handler.createCount); got != 0 {
		t.Fatalf("expected no squawk on success, got %d create calls", got)
	}
}

func TestProbeJIRA_TransientFailure_NoSquawk(t *testing.T) {
	// 5xx is a transient/server-side failure, not a permission regression.
	jiraSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer jiraSrv.Close()

	withJiraEnv(t, jiraSrv.URL, "svc@example.com", "tok", "PE-1")
	prevClient := jiraProbeHTTPClient
	jiraProbeHTTPClient = &http.Client{Timeout: 2 * time.Second}
	defer func() { jiraProbeHTTPClient = prevClient }()

	handler := &fakeDaemonHandler{}
	_, cleanup := installFakeDaemon(t, handler)
	defer cleanup()

	probeJIRA(context.Background())

	if got := atomic.LoadInt32(&handler.createCount); got != 0 {
		t.Fatalf("expected no squawk on 5xx, got %d create calls", got)
	}
}

func TestProbeJIRA_DefaultCanaryTicket(t *testing.T) {
	var gotPath string
	jiraSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer jiraSrv.Close()

	t.Setenv("JIRA_BASE_URL", jiraSrv.URL)
	t.Setenv("JIRA_EMAIL", "svc@example.com")
	t.Setenv("JIRA_API_TOKEN", "tok")
	t.Setenv("JIRA_SMOKE_TICKET", "") // unset → default

	prevClient := jiraProbeHTTPClient
	jiraProbeHTTPClient = &http.Client{Timeout: 2 * time.Second}
	defer func() { jiraProbeHTTPClient = prevClient }()

	probeJIRA(context.Background())

	if !strings.HasSuffix(gotPath, "/issue/"+jiraSmokeTicketDefault) {
		t.Errorf("default canary not used; got path %q want suffix /issue/%s", gotPath, jiraSmokeTicketDefault)
	}
}
