package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// ── defaultGBProject ─────────────────────────────────────────────────────

// ── resolvePrimeAgentIdentity ────────────────────────────────────────────

func TestResolvePrimeAgentIdentity(t *testing.T) {
	old := primeForAgent
	t.Cleanup(func() { primeForAgent = old })

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())

	// --for flag takes priority
	primeForAgent = "gd-F1"
	t.Setenv("KD_ACTOR", "alice")
	if got := resolvePrimeAgentIdentity(cmd); got != "gd-F1" {
		t.Errorf("for flag: %q", got)
	}

	primeForAgent = ""
	if got := resolvePrimeAgentIdentity(cmd); got != "alice" {
		t.Errorf("actor: %q", got)
	}

	t.Setenv("KD_ACTOR", "")
	t.Setenv("KD_AGENT_ID", "gd-A1")
	if got := resolvePrimeAgentIdentity(cmd); got != "gd-A1" {
		t.Errorf("agent_id: %q", got)
	}

	t.Setenv("KD_AGENT_ID", "")
	oldActor := actor
	actor = "globalactor"
	t.Cleanup(func() { actor = oldActor })
	if got := resolvePrimeAgentIdentity(cmd); got != "globalactor" {
		t.Errorf("global: %q", got)
	}

	actor = "unknown"
	if got := resolvePrimeAgentIdentity(cmd); got != "" {
		t.Errorf("unknown: %q", got)
	}
}

// ── outputOperatorIdentity ───────────────────────────────────────────────

func TestOutputOperatorIdentity_NoAgentEnv(t *testing.T) {
	t.Setenv("KD_AGENT_ID", "")
	var buf bytes.Buffer
	outputOperatorIdentity(&buf)
	if buf.Len() != 0 {
		t.Errorf("expected empty: %q", buf.String())
	}
}

func TestOutputOperatorIdentity_NoUserBead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "gd-A1", "title": "agent", "type": "agent",
			"fields": json.RawMessage(`{}`),
		})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	t.Setenv("KD_AGENT_ID", "gd-A1")
	var buf bytes.Buffer
	outputOperatorIdentity(&buf)
	if buf.Len() != 0 {
		t.Errorf("expected empty when no user bead: %q", buf.String())
	}
}

func TestOutputOperatorIdentity_WithPronouns(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/beads/gd-A1" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "gd-A1", "title": "agent", "type": "agent",
				"fields": json.RawMessage(`{"user_bead_id":"gd-U1"}`),
			})
			return
		}
		if r.URL.Path == "/v1/beads/gd-U1" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "gd-U1", "title": "alice", "type": "user",
				"fields": json.RawMessage(`{"display_name":"Alice Smith","pronouns":"she/her"}`),
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	t.Setenv("KD_AGENT_ID", "gd-A1")
	var buf bytes.Buffer
	outputOperatorIdentity(&buf)
	out := buf.String()
	if !strings.Contains(out, "Alice Smith") || !strings.Contains(out, "she/her") {
		t.Errorf("expected operator name + pronouns, got: %q", out)
	}
	if !strings.Contains(out, "## Operator") {
		t.Errorf("expected ## Operator header, got: %q", out)
	}
}

func TestOutputOperatorIdentity_NoPronounsFallsBackToTitle(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/beads/gd-A1" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "gd-A1", "title": "agent", "type": "agent",
				"fields": json.RawMessage(`{"user_bead_id":"gd-U1"}`),
			})
			return
		}
		// No display_name field, no pronouns — name should fall back to title.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "gd-U1", "title": "Bob", "type": "user",
			"fields": json.RawMessage(`{}`),
		})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	t.Setenv("KD_AGENT_ID", "gd-A1")
	var buf bytes.Buffer
	outputOperatorIdentity(&buf)
	out := buf.String()
	if !strings.Contains(out, "Bob") {
		t.Errorf("expected fallback to title name, got: %q", out)
	}
	if strings.Contains(out, "(") {
		t.Errorf("expected no pronoun parens when pronouns absent, got: %q", out)
	}
}

// ── outputUserPreferences ────────────────────────────────────────────────

func TestOutputUserPreferences_NoAgentEnv(t *testing.T) {
	t.Setenv("KD_AGENT_ID", "")
	var buf bytes.Buffer
	outputUserPreferences(&buf)
	if buf.Len() != 0 {
		t.Errorf("expected empty: %q", buf.String())
	}
}

func TestOutputUserPreferences_GetBeadError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	t.Setenv("KD_AGENT_ID", "gd-A1")
	var buf bytes.Buffer
	outputUserPreferences(&buf)
	if buf.Len() != 0 {
		t.Errorf("expected empty on error: %q", buf.String())
	}
}

func TestOutputUserPreferences_NoUserBeadID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "gd-A1", "title": "agent", "type": "agent",
			"fields": json.RawMessage(`{}`),
		})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	t.Setenv("KD_AGENT_ID", "gd-A1")
	var buf bytes.Buffer
	outputUserPreferences(&buf)
	if buf.Len() != 0 {
		t.Errorf("expected empty: %q", buf.String())
	}
}

func TestOutputUserPreferences_WithPrefs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if path == "/v1/beads/gd-A1" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "gd-A1", "title": "agent", "type": "agent",
				"fields": json.RawMessage(`{"user_bead_id":"gd-U1"}`),
			})
			return
		}
		if path == "/v1/beads/gd-U1" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "gd-U1", "title": "alice", "type": "user",
				"fields": json.RawMessage(`{"preferences":"{\"tz\":\"UTC\",\"lang\":\"en\"}"}`),
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	t.Setenv("KD_AGENT_ID", "gd-A1")
	var buf bytes.Buffer
	outputUserPreferences(&buf)
	out := buf.String()
	if out == "" {
		t.Error("expected output")
	}
}

func TestOutputUserPreferences_BadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if path == "/v1/beads/gd-A1" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "gd-A1", "type": "agent",
				"fields": json.RawMessage(`{"user_bead_id":"gd-U1"}`),
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "gd-U1", "type": "user",
			"fields": json.RawMessage(`{"preferences":"{bad json"}`),
		})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	t.Setenv("KD_AGENT_ID", "gd-A1")
	var buf bytes.Buffer
	outputUserPreferences(&buf)
	if buf.Len() != 0 {
		t.Errorf("expected empty on bad json: %q", buf.String())
	}
}

// ── outputJackSection (empty cases) ──────────────────────────────────────

func TestOutputJackSection_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	var buf bytes.Buffer
	outputJackSection(&buf)
	if buf.Len() != 0 {
		t.Errorf("expected empty: %q", buf.String())
	}
}

func TestOutputJackSection_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	var buf bytes.Buffer
	outputJackSection(&buf)
	if buf.Len() != 0 {
		t.Errorf("expected empty: %q", buf.String())
	}
}

// ── outputRosterSection ──────────────────────────────────────────────────

func TestOutputRosterSection_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	var buf bytes.Buffer
	outputRosterSection(&buf, "")
	if buf.Len() != 0 {
		t.Errorf("expected empty: %q", buf.String())
	}
}

// ── outputAutoAssign ─────────────────────────────────────────────────────

func TestOutputAutoAssign_BoatTaskID(t *testing.T) {
	t.Setenv("BOAT_TASK_ID", "gd-T1")
	var buf bytes.Buffer
	outputAutoAssign(&buf, "gd-A1")
	if buf.Len() != 0 {
		t.Errorf("expected empty: %q", buf.String())
	}
}

func TestOutputAutoAssign_NoProject(t *testing.T) {
	t.Setenv("BOAT_TASK_ID", "")
	t.Setenv("KD_PROJECT", "")
	t.Setenv("BOAT_PROJECT", "")
	var buf bytes.Buffer
	outputAutoAssign(&buf, "gd-A1")
	if buf.Len() != 0 {
		t.Errorf("expected empty: %q", buf.String())
	}
}

// ── isAutoAssignEnabled ──────────────────────────────────────────────────

func TestIsAutoAssignEnabled_AgentOverride(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "gd-A1", "title": "t", "type": "agent",
			"fields": json.RawMessage(`{"auto_assign":"true"}`),
		})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	t.Setenv("KD_AGENT_ID", "gd-A1")
	if !isAutoAssignEnabled(context.Background(), "proj") {
		t.Error("expected true")
	}
}

func TestIsAutoAssignEnabled_Default(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	t.Setenv("KD_AGENT_ID", "")
	if isAutoAssignEnabled(context.Background(), "") {
		t.Error("default should be false")
	}
}

func TestIsAutoAssignEnabled_ProjectEnabled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// agent bead lookup returns empty fields
		if r.URL.Path == "/v1/beads/gd-A1" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "gd-A1", "type": "agent",
				"fields": json.RawMessage(`{}`),
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"beads": []map[string]any{
				{
					"id": "gd-P1", "title": "Project: myp", "type": "project",
					"status": "open",
					"fields": json.RawMessage(`{"auto_assign":"true"}`),
				},
			},
		})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	t.Setenv("KD_AGENT_ID", "gd-A1")
	if !isAutoAssignEnabled(context.Background(), "myp") {
		t.Error("expected true when project enabled")
	}
}
