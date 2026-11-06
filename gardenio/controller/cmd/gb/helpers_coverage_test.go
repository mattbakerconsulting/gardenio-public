package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// ── workspace helpers ────────────────────────────────────────────────────

func TestDeriveBranchName(t *testing.T) {
	if got := deriveBranchName("Fix [PE-6916] bug", "gd-A1"); got != "fix/pe-6916" {
		t.Errorf("pe-6916: %q", got)
	}
	// Bead IDs are case-sensitive — the fallback must preserve case so the
	// branch round-trips back to the exact bead. See fix ka-0000004161.
	if got := deriveBranchName("no issue here", "gd-XYZ"); got != "fix/gd-XYZ" {
		t.Errorf("fallback: %q", got)
	}
	if got := deriveBranchName("mixed case id", "gd-AbCdEf"); got != "fix/gd-AbCdEf" {
		t.Errorf("mixed-case: %q", got)
	}
}

func TestWorkspaceFlagOrCwd(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.Flags().String("workspace", "", "")

	// empty → cwd
	cwd, _ := os.Getwd()
	if got := workspaceFlagOrCwd(cmd); got != cwd {
		t.Errorf("cwd: got %q", got)
	}

	// set flag
	_ = cmd.Flags().Set("workspace", "/tmp/foo")
	if got := workspaceFlagOrCwd(cmd); got != "/tmp/foo" {
		t.Errorf("flag: got %q", got)
	}
}

func TestIsDirtyWorktree(t *testing.T) {
	// non-existent path → false
	if isDirtyWorktree("/definitely/does/not/exist/xyz") {
		t.Error("missing path should be clean")
	}
}

// ── bus_emit ─────────────────────────────────────────────────────────────

func TestResolvePrimeAgentFromEnv(t *testing.T) {
	t.Setenv("KD_ACTOR", "alice")
	t.Setenv("KD_AGENT_ID", "")
	if got := resolvePrimeAgentFromEnv(""); got != "alice" {
		t.Errorf("kd_actor: %q", got)
	}

	t.Setenv("KD_ACTOR", "")
	t.Setenv("KD_AGENT_ID", "gd-A1")
	if got := resolvePrimeAgentFromEnv(""); got != "gd-A1" {
		t.Errorf("kd_agent_id: %q", got)
	}

	t.Setenv("KD_ACTOR", "")
	t.Setenv("KD_AGENT_ID", "")
	if got := resolvePrimeAgentFromEnv("global"); got != "global" {
		t.Errorf("global: %q", got)
	}

	if got := resolvePrimeAgentFromEnv("unknown"); got != "" {
		t.Errorf("unknown should be empty: %q", got)
	}

	if got := resolvePrimeAgentFromEnv(""); got != "" {
		t.Errorf("empty: %q", got)
	}
}

// ── agent_system_prompt ──────────────────────────────────────────────────

func TestCurrentBinary(t *testing.T) {
	got := currentBinary()
	if got == "" {
		t.Error("expected non-empty")
	}
}

// ── attachments ──────────────────────────────────────────────────────────

func TestResolveJiraKey_DirectMatch(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	got, err := resolveJiraKey(cmd, "PE-123")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "PE-123" {
		t.Errorf("got %q", got)
	}
}

func TestResolveJiraKey_FromBead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/v1/beads/gd-B1") {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "gd-B1", "title": "t", "type": "bug",
				"fields": json.RawMessage(`{"jira_key":"PE-999"}`),
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	got, err := resolveJiraKey(cmd, "gd-B1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "PE-999" {
		t.Errorf("got %q", got)
	}
}

func TestResolveJiraKey_NoJiraKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "gd-B1", "title": "t", "type": "bug",
			"fields": json.RawMessage(`{}`),
		})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if _, err := resolveJiraKey(cmd, "gd-B1"); err == nil {
		t.Error("expected error")
	}
}

func TestResolveJiraKey_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if _, err := resolveJiraKey(cmd, "gd-B1"); err == nil {
		t.Error("expected error")
	}
}

// ── prime output ─────────────────────────────────────────────────────────

func TestOutputPrewarmedStandby(t *testing.T) {
	var buf bytes.Buffer
	outputPrewarmedStandby(&buf, "gd-A1")
	out := buf.String()
	if !strings.Contains(out, "Prewarmed Agent") {
		t.Errorf("missing header: %q", out)
	}
	if !strings.Contains(out, "gd-A1") {
		t.Errorf("missing agent: %q", out)
	}

	// no agent ID
	buf.Reset()
	outputPrewarmedStandby(&buf, "")
	if strings.Contains(buf.String(), "Agent: ") {
		t.Errorf("should not include agent line: %q", buf.String())
	}
}

func TestPrimeRole(t *testing.T) {
	t.Setenv("BOAT_ROLE", "thread")
	if got := primeRole(); got != "thread" {
		t.Errorf("got %q", got)
	}

	t.Setenv("BOAT_ROLE", "")
	if got := primeRole(); got != "" {
		t.Errorf("empty: %q", got)
	}
}

// ── hook.go: checkWrapUpGate ─────────────────────────────────────────────

func TestCheckWrapUpGate_GetBeadError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	blocked, _ := checkWrapUpGate(context.Background(), "gd-A1")
	if blocked {
		t.Error("should not block on GetBead error")
	}
}

func TestCheckWrapUpGate_NoStopRequested(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "gd-A1", "title": "t", "type": "agent",
			"fields": json.RawMessage(`{}`),
		})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	blocked, _ := checkWrapUpGate(context.Background(), "gd-A1")
	if blocked {
		t.Error("should not block when stop_requested missing")
	}
}

func TestCheckWrapUpGate_AlreadyHasWrapup(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "gd-A1", "title": "t", "type": "agent",
			"fields": json.RawMessage(`{"stop_requested":"true","wrapup":"{\"accomplishments\":\"done\"}"}`),
		})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	blocked, _ := checkWrapUpGate(context.Background(), "gd-A1")
	if blocked {
		t.Error("should not block when wrapup present")
	}
}

// ── user.go: findUserBead ────────────────────────────────────────────────

func mkUserCmd() *cobra.Command {
	c := &cobra.Command{}
	c.SetContext(context.Background())
	return c
}


func TestFindUserBead_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	_, err := findUserBead(mkUserCmd(), "alice")
	if err == nil {
		t.Error("expected not found error")
	}
}

func TestFindUserBead_Found(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"beads": []map[string]any{
				{
					"id": "gd-U1", "title": "alice", "type": "user", "kind": "config",
					"status": "open",
					"fields": json.RawMessage(`{"slack_id":"U1","email":"a@x.com"}`),
				},
			},
		})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	b, err := findUserBead(mkUserCmd(), "alice")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if b == nil || b.ID != "gd-U1" {
		t.Errorf("got %+v", b)
	}
}

func TestFindUserBead_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	if _, err := findUserBead(mkUserCmd(), "alice"); err == nil {
		t.Error("expected error")
	}
}
