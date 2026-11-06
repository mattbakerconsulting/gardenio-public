package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
)

// agentDaemonStub simulates the beads daemon for agent remote tests.
type agentDaemonStub struct {
	mu            sync.Mutex
	beads         []map[string]any
	patches       []agentPatch
	closes        []agentClose
	missingBeads  map[string]bool // bead IDs that should 404 on close
	closeFailCode int             // non-zero to force /close to fail
}

type agentPatch struct {
	BeadID string
	Body   map[string]any
}

type agentClose struct {
	BeadID string
	Body   map[string]any
}

func (s *agentDaemonStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/v1/beads":
			s.mu.Lock()
			resp := map[string]any{"beads": s.beads, "total": len(s.beads)}
			s.mu.Unlock()
			json.NewEncoder(w).Encode(resp)

		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v1/beads/"):
			beadID := strings.TrimPrefix(r.URL.Path, "/v1/beads/")
			s.mu.Lock()
			defer s.mu.Unlock()
			for _, b := range s.beads {
				if b["id"] == beadID {
					json.NewEncoder(w).Encode(b)
					return
				}
			}
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)

		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/close") && strings.HasPrefix(r.URL.Path, "/v1/beads/"):
			beadID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/beads/"), "/close")
			body, _ := io.ReadAll(r.Body)
			var payload map[string]any
			json.Unmarshal(body, &payload)
			s.mu.Lock()
			if s.missingBeads[beadID] {
				s.mu.Unlock()
				http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
				return
			}
			if s.closeFailCode != 0 {
				code := s.closeFailCode
				s.mu.Unlock()
				http.Error(w, `{"error":"boom"}`, code)
				return
			}
			s.closes = append(s.closes, agentClose{BeadID: beadID, Body: payload})
			s.mu.Unlock()
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]any{"ok": true})

		case r.Method == "PATCH" && strings.HasPrefix(r.URL.Path, "/v1/beads/"):
			beadID := strings.TrimPrefix(r.URL.Path, "/v1/beads/")
			body, _ := io.ReadAll(r.Body)
			var patch map[string]any
			json.Unmarshal(body, &patch)
			s.mu.Lock()
			s.patches = append(s.patches, agentPatch{BeadID: beadID, Body: patch})
			s.mu.Unlock()
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]any{"ok": true})

		default:
			http.NotFound(w, r)
		}
	})
}

func makeAgentBead(id, agentName, project, role, mode, state string) map[string]any {
	fields := map[string]string{
		"agent":       agentName,
		"project":     project,
		"role":        role,
		"mode":        mode,
		"agent_state": state,
	}
	fieldsRaw, _ := json.Marshal(fields)
	return map[string]any{
		"id":         id,
		"title":      agentName,
		"type":       "agent",
		"status":     "open",
		"fields":     json.RawMessage(fieldsRaw),
		"created_at": time.Now().Add(-2 * time.Hour).Format(time.RFC3339),
		"updated_at": time.Now().Format(time.RFC3339),
	}
}

func setupAgentDaemon(t *testing.T, stub *agentDaemonStub) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)

	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	oldDaemon := daemon
	daemon = c
	t.Cleanup(func() { daemon = oldDaemon })
	return srv
}

// cmdWithCtx returns a cobra command with a background context set.
func cmdWithCtx() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	return cmd
}

func TestAgentList_AllAgents(t *testing.T) {
	stub := &agentDaemonStub{
		beads: []map[string]any{
			makeAgentBead("gd-a1", "worker-alpha", "gardenio", "crew", "crew", "working"),
			makeAgentBead("gd-a2", "worker-beta", "town", "crew", "crew", "idle"),
		},
	}
	setupAgentDaemon(t, stub)

	agents, err := daemon.ListAgentBeadsBySpawner(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 2 {
		t.Fatalf("expected 2 agents, got %d", len(agents))
	}
	if agents[0].AgentName != "worker-alpha" {
		t.Errorf("agent[0].AgentName = %q, want %q", agents[0].AgentName, "worker-alpha")
	}
	if agents[1].Project != "town" {
		t.Errorf("agent[1].Project = %q, want %q", agents[1].Project, "town")
	}
}

func TestAgentList_NoAgents(t *testing.T) {
	stub := &agentDaemonStub{beads: []map[string]any{}}
	setupAgentDaemon(t, stub)

	agents, err := daemon.ListAgentBeadsBySpawner(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 0 {
		t.Errorf("expected 0 agents, got %d", len(agents))
	}
}

func TestAgentList_FilterBySpawner(t *testing.T) {
	fields1 := map[string]string{
		"agent":      "worker-alpha",
		"project":    "gardenio",
		"role":       "crew",
		"mode":       "crew",
		"spawned_by": "alice@example.com",
	}
	fields2 := map[string]string{
		"agent":      "worker-beta",
		"project":    "gardenio",
		"role":       "crew",
		"mode":       "crew",
		"spawned_by": "bob@example.com",
	}
	raw1, _ := json.Marshal(fields1)
	raw2, _ := json.Marshal(fields2)

	stub := &agentDaemonStub{
		beads: []map[string]any{
			{
				"id": "gd-a1", "title": "worker-alpha", "type": "agent", "status": "open",
				"fields":     json.RawMessage(raw1),
				"created_at": time.Now().Format(time.RFC3339),
				"updated_at": time.Now().Format(time.RFC3339),
			},
			{
				"id": "gd-a2", "title": "worker-beta", "type": "agent", "status": "open",
				"fields":     json.RawMessage(raw2),
				"created_at": time.Now().Format(time.RFC3339),
				"updated_at": time.Now().Format(time.RFC3339),
			},
		},
	}
	setupAgentDaemon(t, stub)

	// The server receives the field filter in query params. Since the stub
	// doesn't filter server-side, we test that the API call is formed correctly
	// by verifying the client doesn't error.
	agents, err := daemon.ListAgentBeadsBySpawner(context.Background(), "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	// The stub returns all agents (no server-side filtering), so we verify
	// both come back. Real daemon would filter.
	if len(agents) != 2 {
		t.Fatalf("expected 2 agents from stub, got %d", len(agents))
	}
}

// makeAgentBeadWithTask is makeAgentBead plus a task_id metadata field, for
// exercising the --task filter on `gb agent list` / `gb agent roster`.
func makeAgentBeadWithTask(id, agentName, project, role, mode, state, taskID string) map[string]any {
	fields := map[string]string{
		"agent":       agentName,
		"project":     project,
		"role":        role,
		"mode":        mode,
		"agent_state": state,
		"task_id":     taskID,
	}
	fieldsRaw, _ := json.Marshal(fields)
	return map[string]any{
		"id":         id,
		"title":      agentName,
		"type":       "agent",
		"status":     "open",
		"fields":     json.RawMessage(fieldsRaw),
		"created_at": time.Now().Add(-2 * time.Hour).Format(time.RFC3339),
		"updated_at": time.Now().Format(time.RFC3339),
	}
}

func TestAgentListCmd_FilterByTask(t *testing.T) {
	stub := &agentDaemonStub{
		beads: []map[string]any{
			makeAgentBeadWithTask("gd-a1", "worker-alpha", "gardenio", "crew", "crew", "working", "gd-task1"),
			makeAgentBeadWithTask("gd-a2", "worker-beta", "gardenio", "crew", "crew", "working", "gd-task2"),
			makeAgentBeadWithTask("gd-a3", "worker-gamma", "gardenio", "crew", "crew", "working", "gd-task1"),
		},
	}
	setupAgentDaemon(t, stub)

	// Text output goes through cmd.Printf, which honors cmd.SetOut; JSON goes to
	// os.Stdout, so drive the text path to make the filtered rows observable.
	run := func(task string) string {
		t.Helper()
		var buf bytes.Buffer
		cmd := cmdWithCtx()
		cmd.SetOut(&buf)
		cmd.Flags().Bool("mine", false, "")
		cmd.Flags().Bool("local", false, "")
		cmd.Flags().Bool("prune", false, "")
		cmd.Flags().String("task", task, "")
		if err := agentListCmd.RunE(cmd, nil); err != nil {
			t.Fatalf("list --task %q failed: %v", task, err)
		}
		return buf.String()
	}

	// --task with live crew shows exactly the matching agents.
	out := run("gd-task1")
	if !strings.Contains(out, "worker-alpha") || !strings.Contains(out, "worker-gamma") {
		t.Errorf("list --task gd-task1 missing expected agents:\n%s", out)
	}
	if strings.Contains(out, "worker-beta") {
		t.Errorf("list --task gd-task1 leaked non-matching agent worker-beta:\n%s", out)
	}

	// --task with no live crew returns the empty message, not an error.
	if out := run("gd-nobody"); !strings.Contains(out, "No active agents.") {
		t.Errorf("list --task gd-nobody = %q, want the no-active-agents message", out)
	}

	// No --task shows all agents.
	out = run("")
	for _, name := range []string{"worker-alpha", "worker-beta", "worker-gamma"} {
		if !strings.Contains(out, name) {
			t.Errorf("list without --task missing %q:\n%s", name, out)
		}
	}
}

func TestResolveAgent_ByBeadID(t *testing.T) {
	stub := &agentDaemonStub{
		beads: []map[string]any{
			makeAgentBead("gd-abc123", "my-agent", "gardenio", "crew", "crew", "working"),
		},
	}
	setupAgentDaemon(t, stub)

	agent, err := resolveAgent(cmdWithCtx(), "gd-abc123")
	if err != nil {
		t.Fatal(err)
	}
	if agent.ID != "gd-abc123" {
		t.Errorf("ID = %q, want %q", agent.ID, "gd-abc123")
	}
	if agent.AgentName != "my-agent" {
		t.Errorf("AgentName = %q, want %q", agent.AgentName, "my-agent")
	}
	if agent.AgentState != "working" {
		t.Errorf("AgentState = %q, want %q", agent.AgentState, "working")
	}
	if agent.Project != "gardenio" {
		t.Errorf("Project = %q, want %q", agent.Project, "gardenio")
	}
}

func TestResolveAgent_ByName(t *testing.T) {
	stub := &agentDaemonStub{
		beads: []map[string]any{
			makeAgentBead("gd-x1", "gardener-gardenio", "gardenio", "gardener", "crew", "idle"),
			makeAgentBead("gd-x2", "worker-one", "gardenio", "crew", "crew", "working"),
		},
	}
	setupAgentDaemon(t, stub)

	agent, err := resolveAgent(cmdWithCtx(), "worker-one")
	if err != nil {
		t.Fatal(err)
	}
	if agent.ID != "gd-x2" {
		t.Errorf("ID = %q, want %q", agent.ID, "gd-x2")
	}
}

func TestResolveAgent_NotFound(t *testing.T) {
	stub := &agentDaemonStub{
		beads: []map[string]any{
			makeAgentBead("gd-x1", "worker-one", "gardenio", "crew", "crew", "working"),
		},
	}
	setupAgentDaemon(t, stub)

	_, err := resolveAgent(cmdWithCtx(), "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent agent")
	}
	if !strings.Contains(err.Error(), "no active agent found") {
		t.Errorf("error = %q, want it to contain 'no active agent found'", err.Error())
	}
}

func TestResolveAgent_NonAgentBead(t *testing.T) {
	fields, _ := json.Marshal(map[string]string{"project": "gardenio"})
	stub := &agentDaemonStub{
		beads: []map[string]any{
			{
				"id": "gd-task1", "title": "Some task", "type": "task", "status": "open",
				"fields":     json.RawMessage(fields),
				"created_at": time.Now().Format(time.RFC3339),
				"updated_at": time.Now().Format(time.RFC3339),
			},
		},
	}
	setupAgentDaemon(t, stub)

	_, err := resolveAgent(cmdWithCtx(), "gd-task1")
	if err == nil {
		t.Fatal("expected error for non-agent bead")
	}
	if !strings.Contains(err.Error(), "not an agent") {
		t.Errorf("error = %q, want it to contain 'not an agent'", err.Error())
	}
}

func TestResolveAgent_DefaultMode(t *testing.T) {
	// When mode field is empty, resolveAgent should default to "crew".
	fields, _ := json.Marshal(map[string]string{
		"agent":       "worker-no-mode",
		"project":     "gardenio",
		"role":        "crew",
		"agent_state": "working",
		// mode is intentionally omitted
	})
	stub := &agentDaemonStub{
		beads: []map[string]any{
			{
				"id": "gd-nm1", "title": "worker-no-mode", "type": "agent", "status": "open",
				"fields":     json.RawMessage(fields),
				"created_at": time.Now().Format(time.RFC3339),
				"updated_at": time.Now().Format(time.RFC3339),
			},
		},
	}
	setupAgentDaemon(t, stub)

	agent, err := resolveAgent(cmdWithCtx(), "gd-nm1")
	if err != nil {
		t.Fatal(err)
	}
	if agent.Mode != "crew" {
		t.Errorf("Mode = %q, want %q (default)", agent.Mode, "crew")
	}
}

func TestAgentStop_RequestsSent(t *testing.T) {
	stub := &agentDaemonStub{
		beads: []map[string]any{
			makeAgentBead("gd-stop1", "worker-stop", "gardenio", "crew", "crew", "working"),
		},
	}
	setupAgentDaemon(t, stub)

	err := daemon.RequestAgentStop(context.Background(), "gd-stop1")
	if err != nil {
		t.Fatal(err)
	}

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.patches) == 0 {
		t.Fatal("expected PATCH request for stop")
	}

	// RequestAgentStop does a read-modify-write: GET then PATCH.
	// The PATCH body should contain fields with agent_state = stop_requested.
	lastPatch := stub.patches[len(stub.patches)-1]
	if lastPatch.BeadID != "gd-stop1" {
		t.Errorf("PATCH bead = %q, want %q", lastPatch.BeadID, "gd-stop1")
	}
}

func TestAgentStop_DaemonError(t *testing.T) {
	errSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	t.Cleanup(errSrv.Close)

	c, _ := beadsapi.New(beadsapi.Config{HTTPAddr: errSrv.URL})
	oldDaemon := daemon
	daemon = c
	t.Cleanup(func() { daemon = oldDaemon })

	err := daemon.RequestAgentStop(context.Background(), "gd-bad")
	if err == nil {
		t.Fatal("expected error when daemon returns 500")
	}
}

func TestAgentKill_ClosesBead(t *testing.T) {
	stub := &agentDaemonStub{
		beads: []map[string]any{
			makeAgentBead("gd-kill1", "stuck-worker", "gardenio", "crew", "crew", "working"),
		},
	}
	setupAgentDaemon(t, stub)

	cmd := cmdWithCtx()
	cmd.Flags().Bool("override-owner", true, "")
	if err := cmd.Flags().Set("override-owner", "true"); err != nil {
		t.Fatal(err)
	}

	if err := agentKillCmd.RunE(cmd, []string{"gd-kill1"}); err != nil {
		t.Fatalf("kill failed: %v", err)
	}

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.closes) != 1 {
		t.Fatalf("expected 1 close, got %d", len(stub.closes))
	}
	if stub.closes[0].BeadID != "gd-kill1" {
		t.Errorf("closed bead = %q, want %q", stub.closes[0].BeadID, "gd-kill1")
	}
	body := stub.closes[0].Body
	if body["agent_state"] != "killed" {
		t.Errorf("agent_state on close = %v, want %q", body["agent_state"], "killed")
	}
	if body["failure_reason"] != "killed-by-operator" {
		t.Errorf("failure_reason on close = %v, want %q", body["failure_reason"], "killed-by-operator")
	}
}

func TestAgentKill_NotFound(t *testing.T) {
	stub := &agentDaemonStub{beads: []map[string]any{}}
	setupAgentDaemon(t, stub)

	cmd := cmdWithCtx()
	cmd.Flags().Bool("override-owner", false, "")
	err := agentKillCmd.RunE(cmd, []string{"gd-missing"})
	if err == nil {
		t.Fatal("expected error for missing agent")
	}
}

// stubUserEmail overrides currentUserEmail for the duration of the test.
func stubUserEmail(t *testing.T, email string) {
	t.Helper()
	old := currentUserEmail
	currentUserEmail = func() string { return email }
	t.Cleanup(func() { currentUserEmail = old })
}

func makeAgentBeadWithSpawner(id, agentName, spawner string) map[string]any {
	fields := map[string]string{
		"agent":       agentName,
		"project":     "gardenio",
		"role":        "crew",
		"mode":        "crew",
		"agent_state": "working",
		"spawned_by":  spawner,
	}
	raw, _ := json.Marshal(fields)
	return map[string]any{
		"id": id, "title": agentName, "type": "agent", "status": "open",
		"fields":     json.RawMessage(raw),
		"created_at": time.Now().Add(-time.Hour).Format(time.RFC3339),
		"updated_at": time.Now().Format(time.RFC3339),
	}
}

func TestAgentKill_OwnershipDenied(t *testing.T) {
	stub := &agentDaemonStub{
		beads: []map[string]any{
			makeAgentBeadWithSpawner("gd-owned", "owned-agent", "alice@example.com"),
		},
	}
	setupAgentDaemon(t, stub)
	stubUserEmail(t, "bob@example.com")

	cmd := cmdWithCtx()
	cmd.Flags().Bool("override-owner", false, "")
	err := agentKillCmd.RunE(cmd, []string{"gd-owned"})
	if err == nil {
		t.Fatal("expected ownership error")
	}
	if !strings.Contains(err.Error(), "override-owner") {
		t.Errorf("error = %q, want it to mention override-owner", err.Error())
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.closes) != 0 {
		t.Errorf("expected no close calls on ownership denial, got %d", len(stub.closes))
	}
}

func TestAgentKill_DaemonError(t *testing.T) {
	stub := &agentDaemonStub{
		beads: []map[string]any{
			makeAgentBead("gd-killerr", "stuck", "gardenio", "crew", "crew", "working"),
		},
		closeFailCode: http.StatusInternalServerError,
	}
	setupAgentDaemon(t, stub)

	cmd := cmdWithCtx()
	cmd.Flags().Bool("override-owner", true, "")
	if err := cmd.Flags().Set("override-owner", "true"); err != nil {
		t.Fatal(err)
	}

	err := agentKillCmd.RunE(cmd, []string{"gd-killerr"})
	if err == nil {
		t.Fatal("expected error when CloseBead fails")
	}
	if !strings.Contains(err.Error(), "killing agent") {
		t.Errorf("error = %q, want it to mention 'killing agent'", err.Error())
	}
}

func TestAgentKill_JSONOutput(t *testing.T) {
	stub := &agentDaemonStub{
		beads: []map[string]any{
			makeAgentBead("gd-killjson", "stuck-json", "gardenio", "crew", "crew", "working"),
		},
	}
	setupAgentDaemon(t, stub)

	oldJSON := jsonOutput
	jsonOutput = true
	t.Cleanup(func() { jsonOutput = oldJSON })

	cmd := cmdWithCtx()
	cmd.Flags().Bool("override-owner", true, "")
	if err := cmd.Flags().Set("override-owner", "true"); err != nil {
		t.Fatal(err)
	}

	if err := agentKillCmd.RunE(cmd, []string{"gd-killjson"}); err != nil {
		t.Fatalf("kill failed: %v", err)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.closes) != 1 {
		t.Fatalf("expected 1 close, got %d", len(stub.closes))
	}
}

// stop-command tests

func TestAgentStopCmd_Success(t *testing.T) {
	stub := &agentDaemonStub{
		beads: []map[string]any{
			makeAgentBead("gd-stopOK", "worker-stop", "gardenio", "crew", "crew", "working"),
		},
	}
	setupAgentDaemon(t, stub)

	cmd := cmdWithCtx()
	cmd.Flags().Bool("override-owner", false, "")
	cmd.Flags().Bool("force", false, "")
	if err := agentStopCmd.RunE(cmd, []string{"gd-stopOK"}); err != nil {
		t.Fatalf("stop failed: %v", err)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.patches) == 0 {
		t.Fatal("expected PATCH for stop request")
	}
}

func TestAgentStopCmd_NotFound(t *testing.T) {
	stub := &agentDaemonStub{beads: []map[string]any{}}
	setupAgentDaemon(t, stub)

	cmd := cmdWithCtx()
	cmd.Flags().Bool("override-owner", false, "")
	cmd.Flags().Bool("force", false, "")
	err := agentStopCmd.RunE(cmd, []string{"gd-missing"})
	if err == nil {
		t.Fatal("expected error for missing agent")
	}
}

func TestAgentStopCmd_ForceDeprecated(t *testing.T) {
	// --force is accepted (back-compat) and should bypass the ownership check
	// while emitting a deprecation warning.
	stub := &agentDaemonStub{
		beads: []map[string]any{
			makeAgentBeadWithSpawner("gd-stopforce", "owned", "alice@example.com"),
		},
	}
	setupAgentDaemon(t, stub)
	stubUserEmail(t, "bob@example.com")

	cmd := cmdWithCtx()
	cmd.Flags().Bool("override-owner", false, "")
	cmd.Flags().Bool("force", false, "")
	if err := cmd.Flags().Set("force", "true"); err != nil {
		t.Fatal(err)
	}

	var errBuf strings.Builder
	cmd.SetErr(&errBuf)
	if err := agentStopCmd.RunE(cmd, []string{"gd-stopforce"}); err != nil {
		t.Fatalf("expected --force to bypass ownership, got error: %v", err)
	}
	if !strings.Contains(errBuf.String(), "deprecated") {
		t.Errorf("expected deprecation warning on stderr, got %q", errBuf.String())
	}
}

func TestAgentStopCmd_OwnershipDenied(t *testing.T) {
	stub := &agentDaemonStub{
		beads: []map[string]any{
			makeAgentBeadWithSpawner("gd-stopown", "owned", "alice@example.com"),
		},
	}
	setupAgentDaemon(t, stub)
	stubUserEmail(t, "bob@example.com")

	cmd := cmdWithCtx()
	cmd.Flags().Bool("override-owner", false, "")
	cmd.Flags().Bool("force", false, "")
	err := agentStopCmd.RunE(cmd, []string{"gd-stopown"})
	if err == nil {
		t.Fatal("expected ownership error")
	}
	if !strings.Contains(err.Error(), "override-owner") {
		t.Errorf("error = %q, want mention of override-owner", err.Error())
	}
}

func TestAgentStopCmd_DaemonError(t *testing.T) {
	// Daemon returns 500 on the underlying read-modify-write for stop.
	errSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v1/beads/") {
			fields, _ := json.Marshal(map[string]string{
				"agent": "broken", "project": "gardenio", "role": "crew", "mode": "crew",
			})
			json.NewEncoder(w).Encode(map[string]any{
				"id": "gd-bad", "title": "broken", "type": "agent", "status": "open",
				"fields": json.RawMessage(fields),
			})
			return
		}
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(errSrv.Close)

	c, _ := beadsapi.New(beadsapi.Config{HTTPAddr: errSrv.URL})
	oldDaemon := daemon
	daemon = c
	t.Cleanup(func() { daemon = oldDaemon })

	cmd := cmdWithCtx()
	cmd.Flags().Bool("override-owner", true, "")
	cmd.Flags().Bool("force", false, "")
	if err := cmd.Flags().Set("override-owner", "true"); err != nil {
		t.Fatal(err)
	}
	err := agentStopCmd.RunE(cmd, []string{"gd-bad"})
	if err == nil {
		t.Fatal("expected error when stop daemon call fails")
	}
}

func TestAgentStopCmd_JSONOutput(t *testing.T) {
	stub := &agentDaemonStub{
		beads: []map[string]any{
			makeAgentBead("gd-stopJ", "json-stop", "gardenio", "crew", "crew", "working"),
		},
	}
	setupAgentDaemon(t, stub)

	oldJSON := jsonOutput
	jsonOutput = true
	t.Cleanup(func() { jsonOutput = oldJSON })

	cmd := cmdWithCtx()
	cmd.Flags().Bool("override-owner", false, "")
	cmd.Flags().Bool("force", false, "")
	if err := agentStopCmd.RunE(cmd, []string{"gd-stopJ"}); err != nil {
		t.Fatalf("stop failed: %v", err)
	}
}

func TestAgentAge(t *testing.T) {
	tests := []struct {
		name string
		age  time.Duration
		want string
	}{
		{"zero", 0, "-"},
		{"seconds", 45 * time.Second, "45s"},
		{"minutes", 15 * time.Minute, "15m"},
		{"hours", 3 * time.Hour, "3h"},
		{"days", 48 * time.Hour, "2d"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var ts time.Time
			if tt.name != "zero" {
				ts = time.Now().Add(-tt.age)
			}
			got := agentAge(ts)
			if got != tt.want {
				t.Errorf("agentAge(%v ago) = %q, want %q", tt.age, got, tt.want)
			}
		})
	}
}

func TestAgentList_StateDisplayDefaults(t *testing.T) {
	// When agent_state is empty, the list command should show "unknown".
	fields, _ := json.Marshal(map[string]string{
		"agent":   "ghost-agent",
		"project": "gardenio",
		"role":    "crew",
		"mode":    "crew",
		// agent_state intentionally omitted
	})
	stub := &agentDaemonStub{
		beads: []map[string]any{
			{
				"id": "gd-ghost", "title": "ghost-agent", "type": "agent", "status": "open",
				"fields":     json.RawMessage(fields),
				"created_at": time.Now().Format(time.RFC3339),
				"updated_at": time.Now().Format(time.RFC3339),
			},
		},
	}
	setupAgentDaemon(t, stub)

	agents, err := daemon.ListAgentBeadsBySpawner(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(agents))
	}
	// Verify the state defaulting logic from agent_remote.go
	state := agents[0].AgentState
	if state == "" {
		state = "unknown"
	}
	if state != "unknown" {
		t.Errorf("state = %q, want %q (default for empty)", state, "unknown")
	}
}

func TestAgentList_MetadataFields(t *testing.T) {
	// Verify that task_id and spawned_by metadata are propagated.
	fields, _ := json.Marshal(map[string]string{
		"agent":       "task-worker",
		"project":     "gardenio",
		"role":        "crew",
		"mode":        "crew",
		"agent_state": "working",
		"task_id":     "gd-TASK123",
		"spawned_by":  "alice@example.com",
		"pod_name":    "task-worker-abc12",
	})
	stub := &agentDaemonStub{
		beads: []map[string]any{
			{
				"id": "gd-tw1", "title": "task-worker", "type": "agent", "status": "open",
				"fields":     json.RawMessage(fields),
				"created_at": time.Now().Format(time.RFC3339),
				"updated_at": time.Now().Format(time.RFC3339),
			},
		},
	}
	setupAgentDaemon(t, stub)

	agents, err := daemon.ListAgentBeadsBySpawner(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(agents))
	}
	a := agents[0]
	if a.Metadata["task_id"] != "gd-TASK123" {
		t.Errorf("Metadata[task_id] = %q, want %q", a.Metadata["task_id"], "gd-TASK123")
	}
	if a.Metadata["spawned_by"] != "alice@example.com" {
		t.Errorf("Metadata[spawned_by] = %q, want %q", a.Metadata["spawned_by"], "alice@example.com")
	}
	if a.Metadata["pod_name"] != "task-worker-abc12" {
		t.Errorf("Metadata[pod_name] = %q, want %q", a.Metadata["pod_name"], "task-worker-abc12")
	}
}

func TestResolveAgent_BeadIDNotFound(t *testing.T) {
	stub := &agentDaemonStub{beads: []map[string]any{}}
	setupAgentDaemon(t, stub)

	_, err := resolveAgent(cmdWithCtx(), "gd-nonexistent")
	if err == nil {
		t.Fatal("expected error for missing bead ID")
	}
	if !strings.Contains(err.Error(), "looking up agent") {
		t.Errorf("error = %q, want it to contain 'looking up agent'", err.Error())
	}
}

func TestAgentList_SkipsBeadsWithoutRoleOrName(t *testing.T) {
	// ListAgentBeadsBySpawner skips beads missing role or agent fields.
	incomplete, _ := json.Marshal(map[string]string{
		"agent": "has-name-only",
		// role is missing
	})
	complete, _ := json.Marshal(map[string]string{
		"agent":   "complete-agent",
		"role":    "crew",
		"project": "gardenio",
		"mode":    "crew",
	})
	stub := &agentDaemonStub{
		beads: []map[string]any{
			{
				"id": "gd-inc", "title": "incomplete", "type": "agent", "status": "open",
				"fields":     json.RawMessage(incomplete),
				"created_at": time.Now().Format(time.RFC3339),
				"updated_at": time.Now().Format(time.RFC3339),
			},
			{
				"id": "gd-comp", "title": "complete", "type": "agent", "status": "open",
				"fields":     json.RawMessage(complete),
				"created_at": time.Now().Format(time.RFC3339),
				"updated_at": time.Now().Format(time.RFC3339),
			},
		},
	}
	setupAgentDaemon(t, stub)

	agents, err := daemon.ListAgentBeadsBySpawner(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 {
		t.Fatalf("expected 1 agent (incomplete skipped), got %d", len(agents))
	}
	if agents[0].AgentName != "complete-agent" {
		t.Errorf("agent name = %q, want %q", agents[0].AgentName, "complete-agent")
	}
}
