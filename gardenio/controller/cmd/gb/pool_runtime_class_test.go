package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// poolMuxServerWithRoles extends poolMuxServer to also serve type=role beads,
// so the runtime-class-aware assign path (which resolves the effective runtime
// class from project < role config beads) can be exercised end-to-end.
func poolMuxServerWithRoles(t *testing.T, projects, agents, roles []map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		typeParam := r.URL.Query().Get("type")
		switch {
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/v1/beads/"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		case strings.Contains(typeParam, "project"):
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": projects})
		case strings.Contains(typeParam, "role"):
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": roles})
		case strings.Contains(typeParam, "agent"):
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": agents})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}})
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func setPoolAssignRole(t *testing.T, role string) {
	t.Helper()
	old := poolAssignRole
	poolAssignRole = role
	t.Cleanup(func() { poolAssignRole = old })
}

func setPoolAssignRuntimeClass(t *testing.T, rc string) {
	t.Helper()
	old := poolAssignRuntimeClass
	poolAssignRuntimeClass = rc
	t.Cleanup(func() { poolAssignRuntimeClass = old })
}

// poolAgentBead is a small helper to build an agent bead map for the test mux.
func poolAgentBead(id, name, project, role, state string) map[string]any {
	fields := map[string]string{
		"project":     project,
		"agent":       name,
		"agent_state": state,
		"role":        role,
	}
	raw, _ := json.Marshal(fields)
	return map[string]any{
		"id": id, "title": name, "type": "agent", "status": "open",
		"fields": json.RawMessage(raw),
	}
}

// poolRoleBead builds a type=role config bead pinning a runtime_class.
func poolRoleBead(id, name, runtimeClass string) map[string]any {
	fields := map[string]string{}
	if runtimeClass != "" {
		fields["runtime_class"] = runtimeClass
	}
	raw, _ := json.Marshal(fields)
	return map[string]any{
		"id": id, "title": name, "type": "role", "status": "open",
		"fields": json.RawMessage(raw),
	}
}

// TestRunPoolAssign_RuntimeClassMatch verifies a runsc-required assign claims a
// prewarmed agent whose role resolves to runtime_class=runsc, skipping the runc
// candidate. This is the WS3 proposal-A fast path: a first-spawn runsc channel
// grafts onto a warm runsc pod instead of cold-provisioning a gVisor node.
func TestRunPoolAssign_RuntimeClassMatch(t *testing.T) {
	agents := []map[string]any{
		// runc candidate first (would be picked by the legacy project-only filter).
		poolAgentBead("gd-A1", "warm-runc", "myproj", "thread", "prewarmed"),
		// runsc candidate second.
		poolAgentBead("gd-A2", "warm-runsc", "myproj", "gvisor-poc", "prewarmed"),
	}
	roles := []map[string]any{
		poolRoleBead("gd-R1", "thread", ""),
		poolRoleBead("gd-R2", "gvisor-poc", "runsc"),
	}
	srv := poolMuxServerWithRoles(t, nil, agents, roles)
	swapDaemon(t, srv.URL)
	setPoolProject(t, "myproj")
	setPoolAssignRole(t, "")
	setPoolAssignRuntimeClass(t, "runsc")

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runPoolAssign(cmd, nil); err != nil {
		t.Errorf("expected runsc candidate to be assigned, got err: %v", err)
	}
}

// TestRunPoolAssign_RuntimeClassNoMatch verifies a runsc-required assign returns
// an error (no cold fallback in the CLI) when only runc prewarmed agents exist —
// so a runsc channel never claims+abandons a runc warm pod.
func TestRunPoolAssign_RuntimeClassNoMatch(t *testing.T) {
	agents := []map[string]any{
		poolAgentBead("gd-A1", "warm-runc", "myproj", "thread", "prewarmed"),
	}
	roles := []map[string]any{
		poolRoleBead("gd-R1", "thread", ""),
	}
	srv := poolMuxServerWithRoles(t, nil, agents, roles)
	swapDaemon(t, srv.URL)
	setPoolProject(t, "myproj")
	setPoolAssignRole(t, "")
	setPoolAssignRuntimeClass(t, "runsc")

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runPoolAssign(cmd, nil); err == nil {
		t.Error("expected error: no runsc prewarmed agent available")
	}
}

// TestRunPoolAssign_RuntimeClassFromProject verifies the project-level
// runtime_class is the base of the layering: a role with no override inherits
// the project's runsc pin and matches a runsc-required assign.
func TestRunPoolAssign_RuntimeClassFromProject(t *testing.T) {
	projects := []map[string]any{
		{
			"id": "gd-P1", "title": "myproj", "type": "project", "status": "open",
			"fields": json.RawMessage(`{"prefix":"myp","runtime_class":"runsc"}`),
		},
	}
	agents := []map[string]any{
		poolAgentBead("gd-A1", "warm", "myproj", "thread", "prewarmed"),
	}
	roles := []map[string]any{
		poolRoleBead("gd-R1", "thread", ""), // no role override → inherit project
	}
	srv := poolMuxServerWithRoles(t, projects, agents, roles)
	swapDaemon(t, srv.URL)
	setPoolProject(t, "myproj")
	setPoolAssignRole(t, "")
	setPoolAssignRuntimeClass(t, "runsc")

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runPoolAssign(cmd, nil); err != nil {
		t.Errorf("expected project-inherited runsc candidate to be assigned, got err: %v", err)
	}
}

// TestRunPoolAssign_RoleFilter verifies the --role flag scopes the assign to a
// single role's pool, independent of runtime class.
func TestRunPoolAssign_RoleFilter(t *testing.T) {
	agents := []map[string]any{
		poolAgentBead("gd-A1", "warm-thread", "myproj", "thread", "prewarmed"),
		poolAgentBead("gd-A2", "warm-crew", "myproj", "crew", "prewarmed"),
	}
	srv := poolMuxServerWithRoles(t, nil, agents, nil)
	swapDaemon(t, srv.URL)
	setPoolProject(t, "myproj")
	setPoolAssignRole(t, "crew")
	setPoolAssignRuntimeClass(t, "")

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runPoolAssign(cmd, nil); err != nil {
		t.Errorf("expected crew candidate to be assigned, got err: %v", err)
	}
}

// TestRunPoolAssign_RoleNoMatch verifies a --role with no matching prewarmed
// agent returns an error.
func TestRunPoolAssign_RoleNoMatch(t *testing.T) {
	agents := []map[string]any{
		poolAgentBead("gd-A1", "warm-thread", "myproj", "thread", "prewarmed"),
	}
	srv := poolMuxServerWithRoles(t, nil, agents, nil)
	swapDaemon(t, srv.URL)
	setPoolProject(t, "myproj")
	setPoolAssignRole(t, "crew")
	setPoolAssignRuntimeClass(t, "")

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runPoolAssign(cmd, nil); err == nil {
		t.Error("expected error: no crew prewarmed agent available")
	}
}
