package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/clicreds/avareauth"
	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/coopapi"
	"github.com/spf13/cobra"
)

// ── shouldTerminateOnAuthFailure ─────────────────────────────────────────

func TestShouldTerminateOnAuthFailure_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	client := coopapi.New(srv.URL)

	if !shouldTerminateOnAuthFailure(context.Background(), client) {
		t.Error("error → should terminate")
	}
}

func TestShouldTerminateOnAuthFailure_Working(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(coopapi.AgentState{State: "working"})
	}))
	defer srv.Close()
	client := coopapi.New(srv.URL)

	if shouldTerminateOnAuthFailure(context.Background(), client) {
		t.Error("working → should NOT terminate")
	}
}

func TestShouldTerminateOnAuthFailure_Idle(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(coopapi.AgentState{State: "idle"})
	}))
	defer srv.Close()
	client := coopapi.New(srv.URL)

	if shouldTerminateOnAuthFailure(context.Background(), client) {
		t.Error("idle → should NOT terminate")
	}
}

func TestShouldTerminateOnAuthFailure_Exited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(coopapi.AgentState{State: "exited"})
	}))
	defer srv.Close()
	client := coopapi.New(srv.URL)

	if !shouldTerminateOnAuthFailure(context.Background(), client) {
		t.Error("exited → should terminate")
	}
}

// ── requestCoopShutdown ──────────────────────────────────────────────────

func TestRequestCoopShutdown_Error(t *testing.T) {
	t.Setenv("COOP_URL", "http://127.0.0.1:1") // unreachable
	// Should not panic; prints a note to stderr.
	requestCoopShutdown()
}

func TestRequestCoopShutdown_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/shutdown" {
			t.Errorf("path: %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	t.Setenv("COOP_URL", srv.URL)
	requestCoopShutdown()
}

func TestRequestCoopShutdown_NonOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	t.Setenv("COOP_URL", srv.URL)
	requestCoopShutdown()
}

// ── requireDecisionBead ──────────────────────────────────────────────────

func TestRequireDecisionBead_NoDaemon(t *testing.T) {
	oldD := daemon
	daemon = nil
	t.Cleanup(func() { daemon = oldD })
	if err := requireDecisionBead(context.Background(), "alice"); err != nil {
		t.Errorf("nil daemon should return nil: %v", err)
	}
}

func TestRequireDecisionBead_EmptyActor(t *testing.T) {
	// Even with a daemon, empty actor returns nil.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	if err := requireDecisionBead(context.Background(), ""); err != nil {
		t.Errorf("empty actor should return nil: %v", err)
	}
}

func TestRequireDecisionBead_ListError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	// List error is non-fatal.
	if err := requireDecisionBead(context.Background(), "alice"); err != nil {
		t.Errorf("list error should be non-fatal: %v", err)
	}
}

func TestRequireDecisionBead_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"beads": []map[string]any{{
				"id": "gd-D1", "type": "decision", "created_by": "bob",
				"status": "open",
			}},
		})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	if err := requireDecisionBead(context.Background(), "alice"); err == nil {
		t.Error("expected error when alice has no decision beads")
	}
}

func TestRequireDecisionBead_Found(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"beads": []map[string]any{{
				"id": "gd-D1", "type": "decision", "created_by": "alice",
				"status": "open",
			}},
		})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	if err := requireDecisionBead(context.Background(), "alice"); err != nil {
		t.Errorf("found → nil: %v", err)
	}
}

// ── emitHookWithRetry ────────────────────────────────────────────────────

func TestEmitHookWithRetry_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(beadsapi.EmitHookResponse{})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	resp, err := emitHookWithRetry(context.Background(), beadsapi.EmitHookRequest{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp == nil {
		t.Error("expected resp")
	}
}

// TestEmitHookWithRetry_ReauthFailsFast verifies that an expired-AVA-cookie
// bounce — the daemon edge answering with a Cloudflare challenge / sign-in page
// instead of JSON — is NOT retried 5x. The hook must bail on the first response
// with avareauth.ErrReauthRequired so callers can fail open with "run gb login"
// instead of hammering the wall and dumping HTML (the bug behind the
// non-graceful Stop-hook error).
func TestEmitHookWithRetry_ReauthFailsFast(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		// Mimic the AVA edge's Cloudflare-walled IdP bounce: HTML challenge body.
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`<!DOCTYPE html><html><head><title>Just a moment...</title>` +
			`<script src="/cdn-cgi/challenge-platform/h/g/orchestrate/chl_page/v1"></script></head></html>`))
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	_, err := emitHookWithRetry(context.Background(), beadsapi.EmitHookRequest{})
	if err == nil {
		t.Fatal("expected a re-auth error")
	}
	if !errors.Is(err, avareauth.ErrReauthRequired) {
		t.Errorf("expected ErrReauthRequired, got: %v", err)
	}
	if hits != 1 {
		t.Errorf("expected exactly 1 request (fail-fast, no retries), got %d", hits)
	}
}

func TestEmitHookWithRetry_ContextCancelled(t *testing.T) {
	// Server always errors → retry loop kicks in. Cancel context to exit fast.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := emitHookWithRetry(ctx, beadsapi.EmitHookRequest{})
	if err == nil {
		t.Error("expected error")
	}
}

// ── resolveGateAgentID ────────────────────────────────────────────────────

func TestResolveGateAgentID_FromFlag(t *testing.T) {
	old := gateAgentID
	gateAgentID = "gd-G1"
	t.Cleanup(func() { gateAgentID = old })

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	got, err := resolveGateAgentID(cmd)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "gd-G1" {
		t.Errorf("got %q", got)
	}
}

// ── outputRosterSection: populated ──────────────────────────────────────

func TestOutputRosterSection_Populated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(beadsapi.AgentRosterResponse{
			Actors: []beadsapi.RosterActor{
				{Actor: "me", TaskID: "gd-T1", TaskTitle: "t1", IdleSecs: 30},
				{Actor: "other", IdleSecs: 60, ToolName: "Bash"},
				{Actor: "stale-one", IdleSecs: 700}, // > staleThresholdSecs
				{Actor: "crashed", Reaped: true, TaskID: "gd-T2", TaskTitle: "t2"},
				{Actor: "stopped", LastEvent: "Stop", IdleSecs: 120}, // skipped
			},
		})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	var buf bytes.Buffer
	outputRosterSection(&buf, "me")
	out := buf.String()
	if !strings.Contains(out, "Active Agents") {
		t.Errorf("missing header: %q", out)
	}
	if !strings.Contains(out, "<- you") {
		t.Errorf("missing self marker: %q", out)
	}
	if !strings.Contains(out, "Crashed") {
		t.Errorf("missing crashed: %q", out)
	}
}

// ── outputAutoAssign: project set but disabled ───────────────────────────

func TestOutputAutoAssign_DisabledOnProject(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/beads/gd-A1" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "gd-A1", "type": "agent",
				"fields": json.RawMessage(`{}`),
			})
			return
		}
		// project list → no matching project bead
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	t.Setenv("BOAT_TASK_ID", "")
	t.Setenv("KD_PROJECT", "myp")
	t.Setenv("KD_AGENT_ID", "gd-A1")

	var buf bytes.Buffer
	outputAutoAssign(&buf, "gd-A1")
	if buf.Len() != 0 {
		t.Errorf("expected empty when disabled: %q", buf.String())
	}
}

// ── outputJackSection: populated ─────────────────────────────────────────

func TestOutputJackSection_Populated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"beads": []map[string]any{
				{
					"id": "gd-J1", "title": "active jack", "type": "jack",
					"status":    "in_progress",
					"assignee":  "alice",
					"due_at":    "2099-01-01T00:00:00Z", // future
					"fields":    json.RawMessage(`{"jack_target":"prod"}`),
					"createdBy": "alice",
				},
				{
					"id": "gd-J2", "title": "expired jack", "type": "jack",
					"status":     "in_progress",
					"assignee":   "bob",
					"due_at":     "2000-01-01T00:00:00Z", // past
					"fields":     json.RawMessage(`{"jack_target":"test"}`),
					"created_by": "bob",
				},
			},
		})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	var buf bytes.Buffer
	outputJackSection(&buf)
	out := buf.String()
	if !strings.Contains(out, "Active Jacks") {
		t.Errorf("missing header: %q", out)
	}
	if !strings.Contains(out, "EXPIRED") {
		t.Errorf("missing expired marker: %q", out)
	}
}
