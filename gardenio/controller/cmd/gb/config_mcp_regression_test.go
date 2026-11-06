package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// This file pins the E2E config-mcp failure modes that repeatedly blocked
// golden release promotion (epic gd-zwFkGfb8FJ, task gd-nDqfd6O1NI). Like its
// sibling config_instructions_regression_test.go, these are product-layer
// (deterministic, daemon-free) regressions for the exact resolve→write chain
// `gb setup claude` runs for the claude-mcp category:
//
//   ResolveConfigBeads("claude-mcp", subs, inline...) → writeMCPConfig(workspace)
//
// The bash E2E module (test-config-mcp.sh) exercises the same chain against a
// shared live daemon, where a leaked *global* claude-mcp bead from a sibling
// config-* module can contaminate merge resolution nondeterministically. These
// tests can never flake on that contamination — they construct the layers
// directly — so they catch a reintroduced merge/write regression on every
// unit-test run rather than only on a flaky release-tag E2E pipeline.
//
// Two scenarios had the thinnest direct product-layer coverage and are pinned
// here end-to-end (resolve → materialize .mcp.json):
//
//   Scenario 11: an unset ${VAR} placeholder resolves to an empty string with
//                NO literal placeholder left behind in .mcp.json.
//   Scenario 13: a role-scoped claude-mcp bead overrides a global one via the
//                MergeOverride specificity ordering (role 2: beats global 0:),
//                while unrelated global servers survive.

// resolveAndWriteMCP mirrors runSetupClaude's claude-mcp leg: resolve the
// merged config for the given subscriptions (plus any inline role layers), then
// materialize .mcp.json via writeMCPConfig. Returns the parsed .mcp.json (nil if
// the file was not written).
func resolveAndWriteMCP(t *testing.T, lister configBeadLister, subs []string, workspace string, inline ...resolvedConfig) map[string]any {
	t.Helper()
	merged, _, err := ResolveConfigBeads(context.Background(), lister, "claude-mcp", subs, inline...)
	if err != nil {
		t.Fatalf("ResolveConfigBeads: %v", err)
	}
	if merged != nil {
		if err := writeMCPConfig(workspace, merged); err != nil {
			t.Fatalf("writeMCPConfig: %v", err)
		}
	}
	path := filepath.Join(workspace, ".mcp.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read .mcp.json: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal .mcp.json: %v (content: %s)", err, data)
	}
	return out
}

func mcpServer(t *testing.T, root map[string]any, name string) map[string]any {
	t.Helper()
	servers, ok := root["mcpServers"].(map[string]any)
	if !ok {
		t.Fatalf("mcpServers missing or wrong type in %v", root)
	}
	srv, ok := servers[name].(map[string]any)
	if !ok {
		t.Fatalf("server %q missing or wrong type in %v", name, servers)
	}
	return srv
}

// TestConfigMCP_UnsetEnvVarResolvesToEmpty pins E2E scenario 11: a claude-mcp
// bead whose env value is an unset ${VAR} placeholder must materialize with the
// value resolved to an EMPTY STRING and, critically, with NO literal ${VAR}
// placeholder text left in .mcp.json. A regression that stopped expanding (or
// left the raw placeholder) would leak a literal "${...}" into the env the MCP
// server sees.
func TestConfigMCP_UnsetEnvVarResolvesToEmpty(t *testing.T) {
	const varName = "E2E_MCP_REGRESSION_DEFINITELY_UNSET_VAR"
	// Ensure the var is genuinely unset for the duration of this test.
	os.Unsetenv(varName)

	lister := &mockConfigBeadLister{
		beads: []*beadsapi.BeadDetail{
			{
				Title:       "claude-mcp",
				Labels:      []string{"global"},
				Description: `{"mcpServers":{"unset-test":{"command":"echo","env":{"MISSING_KEY":"${` + varName + `}"}}}}`,
			},
		},
	}

	workspace := t.TempDir()
	root := resolveAndWriteMCP(t, lister, []string{"global"}, workspace)
	if root == nil {
		t.Fatal(".mcp.json not written")
	}

	// Re-read raw bytes to assert on literal placeholder absence.
	raw, err := os.ReadFile(filepath.Join(workspace, ".mcp.json"))
	if err != nil {
		t.Fatalf("read .mcp.json: %v", err)
	}
	if strings.Contains(string(raw), "${"+varName+"}") {
		t.Errorf("literal placeholder ${%s} leaked into .mcp.json:\n%s", varName, raw)
	}

	srv := mcpServer(t, root, "unset-test")
	env, ok := srv["env"].(map[string]any)
	if !ok {
		t.Fatalf("unset-test env missing or wrong type: %v", srv)
	}
	if got := env["MISSING_KEY"]; got != "" {
		t.Errorf("MISSING_KEY: got %q, want empty string (unset var)", got)
	}
}

// TestConfigMCP_EnvVarExpandsFromEnvironment is the positive counterpart to the
// unset case: a set ${VAR} placeholder expands to its value end-to-end through
// the resolve→write chain (E2E scenario 10).
func TestConfigMCP_EnvVarExpandsFromEnvironment(t *testing.T) {
	const varName = "E2E_MCP_REGRESSION_SET_VAR"
	const want = "expanded-regression-value"
	t.Setenv(varName, want)

	lister := &mockConfigBeadLister{
		beads: []*beadsapi.BeadDetail{
			{
				Title:       "claude-mcp",
				Labels:      []string{"global"},
				Description: `{"mcpServers":{"env-test":{"command":"echo","env":{"API_KEY":"${` + varName + `}","STATIC":"literal-value"}}}}`,
			},
		},
	}

	workspace := t.TempDir()
	root := resolveAndWriteMCP(t, lister, []string{"global"}, workspace)
	if root == nil {
		t.Fatal(".mcp.json not written")
	}

	srv := mcpServer(t, root, "env-test")
	env, ok := srv["env"].(map[string]any)
	if !ok {
		t.Fatalf("env-test env missing or wrong type: %v", srv)
	}
	if got := env["API_KEY"]; got != want {
		t.Errorf("API_KEY: got %q, want %q", got, want)
	}
	if got := env["STATIC"]; got != "literal-value" {
		t.Errorf("STATIC: got %q, want literal-value (untouched)", got)
	}
}

// TestConfigMCP_RoleScopedOverridesGlobal pins E2E scenario 13: a role-scoped
// claude-mcp bead overrides a global one for the SAME server (role 2: beats
// global 0: under MergeOverride), while an unrelated server declared only at
// global scope survives into the final .mcp.json.
//
// This is the exact resolve→write chain gb setup claude runs when an agent with
// BOAT_ROLE=crew boots: the daemon returns both the global and role beads, the
// specificity sort orders role last (wins), and writeMCPConfig materializes the
// merged servers.
func TestConfigMCP_RoleScopedOverridesGlobal(t *testing.T) {
	lister := &mockConfigBeadLister{
		beads: []*beadsapi.BeadDetail{
			{
				Title:       "claude-mcp",
				Labels:      []string{"global"},
				Description: `{"mcpServers":{"server-alpha":{"command":"alpha-cmd","args":["--mode","fast"]},"server-beta":{"command":"beta-cmd","args":["--verbose"]}}}`,
			},
			{
				Title:       "claude-mcp",
				Labels:      []string{"role:crew"},
				Description: `{"mcpServers":{"server-alpha":{"command":"crew-alpha-cmd","args":["--crew-mode"]}}}`,
			},
		},
	}

	workspace := t.TempDir()
	root := resolveAndWriteMCP(t, lister, []string{"global", "role:crew"}, workspace)
	if root == nil {
		t.Fatal(".mcp.json not written")
	}

	// Role bead overrides server-alpha's command + args.
	alpha := mcpServer(t, root, "server-alpha")
	if got := alpha["command"]; got != "crew-alpha-cmd" {
		t.Errorf("server-alpha command: got %v, want crew-alpha-cmd (role override)", got)
	}
	if !argsContain(alpha["args"], "--crew-mode") {
		t.Errorf("server-alpha args: want --crew-mode from role bead, got %v", alpha["args"])
	}
	// The role bead replaces the whole server entry (override, not deep-merge),
	// so the global --mode/fast args must NOT survive.
	if argsContain(alpha["args"], "fast") {
		t.Errorf("server-alpha args: global 'fast' leaked through role override: %v", alpha["args"])
	}

	// Global-only server survives untouched.
	beta := mcpServer(t, root, "server-beta")
	if got := beta["command"]; got != "beta-cmd" {
		t.Errorf("server-beta command: got %v, want beta-cmd (global survives)", got)
	}
}

// argsContain reports whether the JSON args value (a []any of strings) contains
// the target string.
func argsContain(args any, target string) bool {
	list, ok := args.([]any)
	if !ok {
		return false
	}
	for _, a := range list {
		if s, ok := a.(string); ok && s == target {
			return true
		}
	}
	return false
}
