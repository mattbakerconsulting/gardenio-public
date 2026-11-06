package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

func TestSplitRoles(t *testing.T) {
	tests := []struct {
		input string
		want  []string
	}{
		{"", nil},
		{"crew", []string{"crew"}},
		{"thread,crew", []string{"thread", "crew"}},
		{"thread, crew", []string{"thread", "crew"}},
		{" thread , crew , gardener ", []string{"thread", "crew", "gardener"}},
		{"crew,,gardener", []string{"crew", "gardener"}},
	}
	for _, tc := range tests {
		got := splitRoles(tc.input)
		if len(got) != len(tc.want) {
			t.Errorf("splitRoles(%q) = %v, want %v", tc.input, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("splitRoles(%q)[%d] = %q, want %q", tc.input, i, got[i], tc.want[i])
			}
		}
	}
}

func TestBuildSubscriptions_SingleRole(t *testing.T) {
	t.Setenv("KD_PROJECT", "")
	t.Setenv("BOAT_PROJECT", "")
	t.Setenv("BOAT_RIG", "")
	t.Setenv("BOAT_AGENT", "")
	subs := buildSubscriptions("crew")
	has := make(map[string]bool)
	for _, s := range subs {
		has[s] = true
	}
	if !has["global"] {
		t.Error("missing global subscription")
	}
	if !has["role:crew"] {
		t.Error("missing role:crew subscription")
	}
	if len(subs) != 2 {
		t.Errorf("expected 2 subscriptions, got %d: %v", len(subs), subs)
	}
}

func TestBuildSubscriptions_MultiRole(t *testing.T) {
	t.Setenv("KD_PROJECT", "")
	t.Setenv("BOAT_PROJECT", "")
	t.Setenv("BOAT_RIG", "")
	t.Setenv("BOAT_AGENT", "")
	subs := buildSubscriptions("thread,crew")
	has := make(map[string]bool)
	for _, s := range subs {
		has[s] = true
	}
	if !has["global"] {
		t.Error("missing global subscription")
	}
	if !has["role:thread"] {
		t.Error("missing role:thread subscription")
	}
	if !has["role:crew"] {
		t.Error("missing role:crew subscription")
	}
	if len(subs) != 3 {
		t.Errorf("expected 3 subscriptions, got %d: %v", len(subs), subs)
	}
}

func TestBuildSubscriptions_Empty(t *testing.T) {
	t.Setenv("KD_PROJECT", "")
	t.Setenv("BOAT_PROJECT", "")
	t.Setenv("BOAT_RIG", "")
	t.Setenv("BOAT_AGENT", "")
	subs := buildSubscriptions("")
	if len(subs) != 1 || subs[0] != "global" {
		t.Errorf("expected [global] for empty role, got %v", subs)
	}
}

func TestBuildSubscriptions_WithProject(t *testing.T) {
	t.Setenv("KD_PROJECT", "")
	t.Setenv("BOAT_PROJECT", "myproject")
	t.Setenv("BOAT_RIG", "")
	t.Setenv("BOAT_AGENT", "")
	subs := buildSubscriptions("thread,crew")
	has := make(map[string]bool)
	for _, s := range subs {
		has[s] = true
	}
	if !has["project:myproject"] {
		t.Error("missing project:myproject subscription")
	}
	if !has["role:thread"] {
		t.Error("missing role:thread subscription")
	}
	if !has["role:crew"] {
		t.Error("missing role:crew subscription")
	}
}

func TestBuildSubscriptions_WithAgent(t *testing.T) {
	t.Setenv("KD_PROJECT", "")
	t.Setenv("BOAT_PROJECT", "myproject")
	t.Setenv("BOAT_RIG", "")
	t.Setenv("BOAT_AGENT", "my-agent-pod")
	subs := buildSubscriptions("crew")
	has := make(map[string]bool)
	for _, s := range subs {
		has[s] = true
	}
	if !has["global"] {
		t.Error("missing global subscription")
	}
	if !has["role:crew"] {
		t.Error("missing role:crew subscription")
	}
	if !has["project:myproject"] {
		t.Error("missing project:myproject subscription")
	}
	if !has["agent:my-agent-pod"] {
		t.Error("missing agent:my-agent-pod subscription")
	}
	if len(subs) != 4 {
		t.Errorf("expected 4 subscriptions, got %d: %v", len(subs), subs)
	}
}

func TestBuildSubscriptions_HonoursKDProject(t *testing.T) {
	t.Setenv("KD_PROJECT", "gardenio")
	t.Setenv("BOAT_PROJECT", "")
	t.Setenv("BOAT_RIG", "")
	got := buildSubscriptions("crew")
	if !slices.Contains(got, "project:gardenio") {
		t.Errorf("KD_PROJECT must contribute a project subscription, got %v", got)
	}
}

func TestBuildSubscriptions_BoatRigIsGone(t *testing.T) {
	t.Setenv("KD_PROJECT", "")
	t.Setenv("BOAT_PROJECT", "")
	t.Setenv("BOAT_RIG", "legacyrig")
	got := buildSubscriptions("crew")
	for _, s := range got {
		if strings.HasPrefix(s, "project:") {
			t.Errorf("BOAT_RIG must no longer contribute a project subscription, got %v", got)
		}
	}
}

func TestRoleInlineSpecificity_SortOrder(t *testing.T) {
	// Role inline (2^:) must sort between role config beads (2:) and project inline (2~:).
	keys := []string{
		projectInlineSpecificity,      // 2~:project-inline
		roleInlineSpecificity("crew"), // 2^:role-inline:crew
		"2:99:crew",                   // role config bead
		"0:global",                    // global
	}
	sort.Strings(keys)
	expected := []string{
		"0:global",
		"2:99:crew",
		"2^:role-inline:crew",
		projectInlineSpecificity,
	}
	for i, k := range keys {
		if k != expected[i] {
			t.Errorf("position %d: got %q, want %q", i, k, expected[i])
		}
	}
}

func TestMergeExtraLayers(t *testing.T) {
	roleExtras := map[string][]resolvedConfig{
		"claude-settings": {{value: json.RawMessage(`{"model":"opus"}`), specificity: "2^:role-inline:crew"}},
		"claude-hooks":    {{value: json.RawMessage(`{}`), specificity: "2^:role-inline:crew"}},
	}
	projectExtras := map[string][]resolvedConfig{
		"claude-settings": {{value: json.RawMessage(`{"model":"sonnet"}`), specificity: projectInlineSpecificity}},
		"claude-mcp":      {{value: json.RawMessage(`{}`), specificity: projectInlineSpecificity}},
	}

	merged := mergeExtraLayers(roleExtras, projectExtras)

	// claude-settings should have both role and project layers.
	if len(merged["claude-settings"]) != 2 {
		t.Errorf("claude-settings: expected 2 layers, got %d", len(merged["claude-settings"]))
	}
	// claude-hooks should have 1 (role only).
	if len(merged["claude-hooks"]) != 1 {
		t.Errorf("claude-hooks: expected 1 layer, got %d", len(merged["claude-hooks"]))
	}
	// claude-mcp should have 1 (project only).
	if len(merged["claude-mcp"]) != 1 {
		t.Errorf("claude-mcp: expected 1 layer, got %d", len(merged["claude-mcp"]))
	}
}

// setupRoleDaemon creates an httptest server that returns role beads and sets
// the package-level daemon. Returns a cleanup function.
func setupRoleDaemon(t *testing.T, roleBeads []mockBead) func() {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" && r.URL.Path == "/v1/beads" {
			resp := mockListResponse{Beads: roleBeads, Total: len(roleBeads)}
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		http.Error(w, "unexpected", http.StatusBadRequest)
	}))

	oldDaemon := daemon
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("failed to create test client: %v", err)
	}
	daemon = c

	return func() {
		daemon = oldDaemon
		srv.Close()
	}
}

func TestFetchRoleClaudeConfig_SingleRole(t *testing.T) {
	cleanup := setupRoleDaemon(t, []mockBead{
		{
			ID:     "role-1",
			Title:  "crew",
			Type:   "role",
			Status: "open",
			Fields: map[string]string{
				"claude_config": `{"claude-settings":{"model":"opus"},"claude-mcp":{"mcpServers":{}}}`,
			},
		},
	})
	defer cleanup()

	result := fetchRoleClaudeConfig(context.Background(), "crew")

	if len(result["claude-settings"]) != 1 {
		t.Fatalf("expected 1 claude-settings layer, got %d", len(result["claude-settings"]))
	}
	if result["claude-settings"][0].specificity != roleInlineSpecificity("crew") {
		t.Errorf("specificity: got %q, want %q", result["claude-settings"][0].specificity, roleInlineSpecificity("crew"))
	}
	if len(result["claude-mcp"]) != 1 {
		t.Errorf("expected 1 claude-mcp layer, got %d", len(result["claude-mcp"]))
	}
}

func TestFetchRoleClaudeConfig_NoClaudeConfig(t *testing.T) {
	cleanup := setupRoleDaemon(t, []mockBead{
		{
			ID:     "role-1",
			Title:  "crew",
			Type:   "role",
			Status: "open",
			Fields: map[string]string{},
		},
	})
	defer cleanup()

	result := fetchRoleClaudeConfig(context.Background(), "crew")
	if len(result) != 0 {
		t.Errorf("expected empty result for role without claude_config, got %d categories", len(result))
	}
}

func TestFetchRoleClaudeConfig_EmptyRole(t *testing.T) {
	result := fetchRoleClaudeConfig(context.Background(), "")
	if len(result) != 0 {
		t.Errorf("expected empty result for empty role, got %d categories", len(result))
	}
}

// extractHookCommands returns the list of command strings from a hook type's entry array.
func extractHookCommands(settings map[string]any, hookType string) []string {
	hooks, ok := settings["hooks"].(map[string]any)
	if !ok {
		return nil
	}
	entries, ok := hooks[hookType].([]any)
	if !ok {
		return nil
	}
	var cmds []string
	for _, e := range entries {
		entry, ok := e.(map[string]any)
		if !ok {
			continue
		}
		hArr, ok := entry["hooks"].([]any)
		if !ok || len(hArr) == 0 {
			continue
		}
		h, ok := hArr[0].(map[string]any)
		if !ok {
			continue
		}
		if cmd, ok := h["command"].(string); ok {
			cmds = append(cmds, cmd)
		}
	}
	return cmds
}

// containsCmd checks if a command substring appears in any of the commands.
func containsCmd(cmds []string, substr string) bool {
	for _, c := range cmds {
		if strings.Contains(c, substr) {
			return true
		}
	}
	return false
}

func TestDefaultHookSettings_NoHippoReferences(t *testing.T) {
	settings := defaultHookSettings()

	for _, phase := range []string{"SessionStart", "PreCompact", "Stop", "UserPromptSubmit", "PreToolUse"} {
		cmds := extractHookCommands(settings, phase)
		for _, c := range cmds {
			if strings.Contains(c, "hippo") {
				t.Errorf("%s hook still references hippo: %q", phase, c)
			}
		}
	}

	// Stop should still have stop-gate.
	stop := extractHookCommands(settings, "Stop")
	if !containsCmd(stop, "stop-gate") {
		t.Error("Stop missing stop-gate")
	}
}

// TestDefaultHookSettings_SessionStartRelays guards the fix for ka-0000004260:
// the SessionStart hook MUST publish to NATS via "gb hook relay". SessionStart
// is the earliest proof-of-life the Slack bridge uses to self-heal a "spawning"
// agent card to "working" (ka-0000003479); without the relay the card stays
// stuck at "starting" through the whole boot + prime window while the agent is
// clearly working.
func TestDefaultHookSettings_SessionStartRelays(t *testing.T) {
	settings := defaultHookSettings()

	sessionStart := extractHookCommands(settings, "SessionStart")
	if !containsCmd(sessionStart, "gb hook relay") {
		t.Errorf("SessionStart hook missing 'gb hook relay' — agent cards will sit at 'starting' "+
			"because the bridge never receives the proof-of-life event (ka-0000004260); got %v", sessionStart)
	}
	// SessionStart must still prime context after relaying.
	if !containsCmd(sessionStart, "gb hook prime") {
		t.Errorf("SessionStart hook missing 'gb hook prime'; got %v", sessionStart)
	}
}

func TestFetchRoleClaudeConfig_MultiRole(t *testing.T) {
	cleanup := setupRoleDaemon(t, []mockBead{
		{
			ID:     "role-1",
			Title:  "thread",
			Type:   "role",
			Status: "open",
			Fields: map[string]string{
				"claude_config": `{"claude-settings":{"model":"sonnet"}}`,
			},
		},
		{
			ID:     "role-2",
			Title:  "crew",
			Type:   "role",
			Status: "open",
			Fields: map[string]string{
				"claude_config": `{"claude-settings":{"model":"opus"}}`,
			},
		},
	})
	defer cleanup()

	result := fetchRoleClaudeConfig(context.Background(), "thread,crew")

	// Both roles contribute layers to claude-settings.
	if len(result["claude-settings"]) != 2 {
		t.Fatalf("expected 2 claude-settings layers, got %d", len(result["claude-settings"]))
	}
	// Verify they have different specificity keys.
	specs := map[string]bool{}
	for _, layer := range result["claude-settings"] {
		specs[layer.specificity] = true
	}
	if !specs[roleInlineSpecificity("thread")] {
		t.Error("missing thread role-inline specificity")
	}
	if !specs[roleInlineSpecificity("crew")] {
		t.Error("missing crew role-inline specificity")
	}
}
