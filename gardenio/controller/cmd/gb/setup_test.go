package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gardenio/controller/internal/claudeconfig"
)

func TestDefaultUserSettings(t *testing.T) {
	s := defaultUserSettings()

	perms, ok := s["permissions"].(map[string]any)
	if !ok {
		t.Fatal("expected permissions key")
	}

	allow, ok := perms["allow"].([]any)
	if !ok || len(allow) == 0 {
		t.Fatal("expected non-empty allow list")
	}

	if s["alwaysThinkingEnabled"] != true {
		t.Error("expected alwaysThinkingEnabled=true")
	}

	// ToolSearch must be enabled explicitly: the fleet runs behind the LiteLLM
	// proxy (a non-first-party ANTHROPIC_BASE_URL), where Claude Code
	// auto-disables tool search unless the env var forces it on. Without this
	// the full MCP tool-def blob loads eagerly into every turn's boot context
	// (~+31k tok measured). See epic ka-0000003733.
	env, ok := s["env"].(map[string]any)
	if !ok {
		t.Fatal("expected env key")
	}
	if env["ENABLE_TOOL_SEARCH"] != "true" {
		t.Errorf("expected default ENABLE_TOOL_SEARCH=true, got %v", env["ENABLE_TOOL_SEARCH"])
	}

	// The Bash-tool timeout ceilings must be raised above Claude Code's 120s
	// default so a legitimate multi-minute foreground blocking wait (chiefly
	// `gb yield` parking on a human decision) is not chopped by a SIGINT, whose
	// interrupted, un-satisfied turn-end then tripped the stop-gate cooldown
	// close (ka-0000002844). These are default/max ceilings only — a per-call
	// timeout still wins — so they must be present and comfortably above 120000.
	if env["BASH_DEFAULT_TIMEOUT_MS"] != "1800000" {
		t.Errorf("expected default BASH_DEFAULT_TIMEOUT_MS=1800000 (30m), got %v", env["BASH_DEFAULT_TIMEOUT_MS"])
	}
	if env["BASH_MAX_TIMEOUT_MS"] != "86400000" {
		t.Errorf("expected default BASH_MAX_TIMEOUT_MS=86400000 (24h, yield's own ceiling), got %v", env["BASH_MAX_TIMEOUT_MS"])
	}

	// Project-scoped .mcp.json servers (chiefly `playwright`) must be
	// auto-approved, or Claude Code never spawns the stdio server and the
	// mcp__playwright__* tools silently never appear — the exact failure that
	// cost two agents their per-component browser evidence on FSM-475
	// (ka-0000001691). `--dangerously-skip-permissions` does NOT cover this:
	// MCP server approval is a separate workspace-trust flow. The key lives in
	// user settings because that location is honored regardless of folder-trust
	// state (Claude Code >= 2.1.196 ignores an enable key in a checked-in
	// .claude/settings.json when the folder is untrusted).
	if s["enableAllProjectMcpServers"] != true {
		t.Errorf("expected default enableAllProjectMcpServers=true so .mcp.json servers "+
			"(playwright) actually spawn (ka-0000001691); got %v", s["enableAllProjectMcpServers"])
	}
}

// TestEnableAllProjectMcpServers_SurvivesWriteUserSettings guards that the MCP
// auto-approval key is not dropped by the writeUserSettings pipeline (plugins,
// marketplace, teams env, deny-list merges) and reaches the rendered
// ~/.claude/settings.json — the file Claude Code actually reads. A key that is
// present in defaultUserSettings() but stripped downstream would re-open
// ka-0000001691 silently, so assert on the on-disk artifact.
func TestEnableAllProjectMcpServers_SurvivesWriteUserSettings(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	if err := writeUserSettings(defaultUserSettings(), "", "crew", false); err != nil {
		t.Fatalf("writeUserSettings: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(tmpDir, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("reading rendered settings.json: %v", err)
	}
	var rendered map[string]any
	if err := json.Unmarshal(data, &rendered); err != nil {
		t.Fatalf("invalid rendered settings JSON: %v", err)
	}
	if rendered["enableAllProjectMcpServers"] != true {
		t.Errorf("rendered ~/.claude/settings.json must keep enableAllProjectMcpServers=true "+
			"(ka-0000001691); got %v", rendered["enableAllProjectMcpServers"])
	}
}

func TestDefaultUserSettings_DeniesInteractiveTools(t *testing.T) {
	s := defaultUserSettings()

	perms := s["permissions"].(map[string]any)
	deny, ok := perms["deny"].([]any)
	if !ok {
		t.Fatal("expected deny list in permissions")
	}

	denySet := make(map[string]bool)
	for _, d := range deny {
		if s, ok := d.(string); ok {
			denySet[s] = true
		}
	}

	expected := []string{"EnterPlanMode", "ExitPlanMode", "AskUserQuestion", "EnterWorktree", "ExitWorktree"}
	for _, tool := range expected {
		if !denySet[tool] {
			t.Errorf("expected %q in deny list", tool)
		}
	}
}

func TestEnsureInteractiveToolsDenied_MergesWithExisting(t *testing.T) {
	settings := map[string]any{
		"permissions": map[string]any{
			"allow": []any{"Bash(*)"},
			"deny":  []any{"NotebookEdit(*)"},
		},
	}

	ensureInteractiveToolsDenied(settings)

	perms := settings["permissions"].(map[string]any)
	deny := perms["deny"].([]any)

	denySet := make(map[string]bool)
	for _, d := range deny {
		if s, ok := d.(string); ok {
			denySet[s] = true
		}
	}

	// Original entry preserved.
	if !denySet["NotebookEdit(*)"] {
		t.Error("expected original NotebookEdit(*) preserved in deny list")
	}
	// Interactive tools added.
	if !denySet["EnterPlanMode"] {
		t.Error("expected EnterPlanMode in deny list")
	}
	if !denySet["AskUserQuestion"] {
		t.Error("expected AskUserQuestion in deny list")
	}
}

func TestEnsureInteractiveToolsDenied_NoDuplicates(t *testing.T) {
	settings := map[string]any{
		"permissions": map[string]any{
			"deny": []any{"EnterPlanMode", "AskUserQuestion"},
		},
	}

	ensureInteractiveToolsDenied(settings)

	perms := settings["permissions"].(map[string]any)
	deny := perms["deny"].([]any)

	// Count occurrences of EnterPlanMode.
	count := 0
	for _, d := range deny {
		if d == "EnterPlanMode" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 EnterPlanMode in deny list, got %d", count)
	}
}

func TestEnsureInteractiveToolsDenied_CreatesPermissionsIfMissing(t *testing.T) {
	settings := map[string]any{
		"model": "opus",
	}

	ensureInteractiveToolsDenied(settings)

	perms, ok := settings["permissions"].(map[string]any)
	if !ok {
		t.Fatal("expected permissions to be created")
	}

	deny, ok := perms["deny"].([]any)
	if !ok || len(deny) == 0 {
		t.Fatal("expected non-empty deny list")
	}
}

func TestMergeSettingsOntoDefaults_AdditiveUltracode(t *testing.T) {
	// A per-role claude-settings bead that only flips ultracode + adds the
	// workflows env knob must keep the default permissions, thinking mode, and
	// the pre-existing env entries — EXCEPT the inherited CLAUDE_CODE_EFFORT_LEVEL,
	// which is dropped so ultracode can apply xhigh (gd-OZRS4qhG4c).
	base := defaultUserSettings()
	override := map[string]any{
		"ultracode": true,
		"env": map[string]any{
			"CLAUDE_CODE_WORKFLOWS": "1",
		},
	}

	merged := mergeSettingsOntoDefaults(base, override)

	if merged["ultracode"] != true {
		t.Errorf("expected ultracode=true from override, got %v", merged["ultracode"])
	}
	// Default thinking mode survives.
	if merged["alwaysThinkingEnabled"] != true {
		t.Error("expected alwaysThinkingEnabled=true to survive from defaults")
	}
	// Default permissions survive untouched.
	if merged["permissions"] == nil {
		t.Error("expected permissions to survive from defaults")
	}
	// env is sub-key merged: the new workflows knob is added, other default env
	// entries survive, but the inherited effort level is cleared for ultracode.
	env, ok := merged["env"].(map[string]any)
	if !ok {
		t.Fatal("expected env map in merged settings")
	}
	if env["CLAUDE_CODE_WORKFLOWS"] != "1" {
		t.Errorf("expected CLAUDE_CODE_WORKFLOWS=1 from override, got %v", env["CLAUDE_CODE_WORKFLOWS"])
	}
	if env["ENABLE_PROMPT_CACHING_1H"] != "true" {
		t.Errorf("expected default ENABLE_PROMPT_CACHING_1H=true to survive, got %v", env["ENABLE_PROMPT_CACHING_1H"])
	}
	// The key fix: ultracode must NOT inherit the default effort pin, or
	// CLAUDE_CODE_EFFORT_LEVEL=high overrides ultracode's xhigh.
	if _, present := env["CLAUDE_CODE_EFFORT_LEVEL"]; present {
		t.Errorf("expected CLAUDE_CODE_EFFORT_LEVEL to be cleared under ultracode, got %v", env["CLAUDE_CODE_EFFORT_LEVEL"])
	}
}

func TestMergeSettingsOntoDefaults_UltracodeRespectsExplicitEffort(t *testing.T) {
	// If a bead explicitly pins an effort level alongside ultracode, that pin
	// wins and is preserved — the operator opted into it deliberately.
	base := defaultUserSettings()
	override := map[string]any{
		"ultracode": true,
		"env": map[string]any{
			"CLAUDE_CODE_EFFORT_LEVEL": "medium",
		},
	}

	merged := mergeSettingsOntoDefaults(base, override)
	env := merged["env"].(map[string]any)
	if env["CLAUDE_CODE_EFFORT_LEVEL"] != "medium" {
		t.Errorf("expected explicit CLAUDE_CODE_EFFORT_LEVEL=medium to survive, got %v", env["CLAUDE_CODE_EFFORT_LEVEL"])
	}
}

func TestApplyUltracodeOverride_OnForcesUltracodeOverBead(t *testing.T) {
	// A resolved bead disabled ultracode (ultracode:false, no workflows). A
	// per-spawn --ultracode=on must force it back on, authoritatively.
	settings := map[string]any{
		"ultracode": false,
		"env": map[string]any{
			"CLAUDE_CODE_EFFORT_LEVEL": "high",
			"ENABLE_PROMPT_CACHING_1H": "true",
		},
	}
	applyUltracodeOverride(settings, "on")

	if settings["ultracode"] != true {
		t.Errorf("expected ultracode=true after --ultracode=on, got %v", settings["ultracode"])
	}
	env := settings["env"].(map[string]any)
	if env["CLAUDE_CODE_WORKFLOWS"] != "1" {
		t.Errorf("expected CLAUDE_CODE_WORKFLOWS=1, got %v", env["CLAUDE_CODE_WORKFLOWS"])
	}
	// Effort must be cleared so ultracode's xhigh applies.
	if _, present := env["CLAUDE_CODE_EFFORT_LEVEL"]; present {
		t.Errorf("expected CLAUDE_CODE_EFFORT_LEVEL cleared under --ultracode=on, got %v", env["CLAUDE_CODE_EFFORT_LEVEL"])
	}
	// Unrelated env survives.
	if env["ENABLE_PROMPT_CACHING_1H"] != "true" {
		t.Errorf("expected ENABLE_PROMPT_CACHING_1H to survive, got %v", env["ENABLE_PROMPT_CACHING_1H"])
	}
}

func TestApplyUltracodeOverride_OffDisablesBeadForcedUltracode(t *testing.T) {
	// Simulate the state AFTER a global/project bead forced ultracode:true +
	// WORKFLOWS=1 and clearEffortForUltracode deleted the effort key. --ultracode=off
	// must fully tear that down: ultracode:false, WORKFLOWS gone, effort restored.
	settings := map[string]any{
		"ultracode": true,
		"env": map[string]any{
			"CLAUDE_CODE_WORKFLOWS":    "1",
			"ENABLE_PROMPT_CACHING_1H": "true",
			// note: CLAUDE_CODE_EFFORT_LEVEL absent, as clearEffortForUltracode left it.
		},
	}
	applyUltracodeOverride(settings, "off")

	if settings["ultracode"] != false {
		t.Errorf("expected ultracode=false after --ultracode=off, got %v", settings["ultracode"])
	}
	env := settings["env"].(map[string]any)
	if _, present := env["CLAUDE_CODE_WORKFLOWS"]; present {
		t.Errorf("expected CLAUDE_CODE_WORKFLOWS removed under --ultracode=off, got %v", env["CLAUDE_CODE_WORKFLOWS"])
	}
	// The load-bearing restore: without it the session downshifts to Claude's
	// built-in default effort instead of the gardenio baseline.
	if env["CLAUDE_CODE_EFFORT_LEVEL"] != "high" {
		t.Errorf("expected CLAUDE_CODE_EFFORT_LEVEL restored to high under --ultracode=off, got %v", env["CLAUDE_CODE_EFFORT_LEVEL"])
	}
	if env["ENABLE_PROMPT_CACHING_1H"] != "true" {
		t.Errorf("expected ENABLE_PROMPT_CACHING_1H to survive, got %v", env["ENABLE_PROMPT_CACHING_1H"])
	}
}

func TestApplyUltracodeOverride_EmptyIsNoOp(t *testing.T) {
	// Unset/empty/unrecognized BOAT_ULTRACODE must not touch the resolved settings.
	for _, raw := range []string{"", "  ", "maybe", "true"} {
		settings := map[string]any{
			"ultracode": true,
			"env": map[string]any{
				"CLAUDE_CODE_WORKFLOWS": "1",
			},
		}
		applyUltracodeOverride(settings, raw)
		if settings["ultracode"] != true {
			t.Errorf("raw=%q: expected ultracode untouched (true), got %v", raw, settings["ultracode"])
		}
		env := settings["env"].(map[string]any)
		if env["CLAUDE_CODE_WORKFLOWS"] != "1" {
			t.Errorf("raw=%q: expected CLAUDE_CODE_WORKFLOWS untouched, got %v", raw, env["CLAUDE_CODE_WORKFLOWS"])
		}
	}
}

func TestApplyUltracodeOverride_CaseInsensitive(t *testing.T) {
	// A hand-edited bead field could carry "ON"/" Off "; normalize defensively.
	settings := map[string]any{"env": map[string]any{}}
	applyUltracodeOverride(settings, " ON ")
	if settings["ultracode"] != true {
		t.Errorf("expected ultracode=true for ' ON ', got %v", settings["ultracode"])
	}
}

func TestApplyUltracodeOverride_MissingEnvMap(t *testing.T) {
	// A malformed settings doc could lack an env map; the override must create it
	// (writeback) rather than panic or silently drop the mutation.
	settings := map[string]any{}
	applyUltracodeOverride(settings, "on")
	env, ok := settings["env"].(map[string]any)
	if !ok {
		t.Fatal("expected env map to be created")
	}
	if env["CLAUDE_CODE_WORKFLOWS"] != "1" {
		t.Errorf("expected CLAUDE_CODE_WORKFLOWS=1 in created env, got %v", env["CLAUDE_CODE_WORKFLOWS"])
	}
}

func TestMergeSettingsOntoDefaults_NoUltracodeKeepsEffort(t *testing.T) {
	// Without ultracode, the default effort pin is untouched.
	base := defaultUserSettings()
	override := map[string]any{
		"env": map[string]any{
			"CLAUDE_CODE_WORKFLOWS": "1",
		},
	}

	merged := mergeSettingsOntoDefaults(base, override)
	env := merged["env"].(map[string]any)
	if env["CLAUDE_CODE_EFFORT_LEVEL"] != "high" {
		t.Errorf("expected default CLAUDE_CODE_EFFORT_LEVEL=high to survive without ultracode, got %v", env["CLAUDE_CODE_EFFORT_LEVEL"])
	}
}

func TestMergeSettingsOntoDefaults_NilOverrideKeepsDefaults(t *testing.T) {
	base := defaultUserSettings()
	merged := mergeSettingsOntoDefaults(base, map[string]any{})

	if merged["alwaysThinkingEnabled"] != true {
		t.Error("expected defaults to be preserved when override is empty")
	}
	if merged["permissions"] == nil {
		t.Error("expected permissions to be preserved when override is empty")
	}
}

func TestMergeSimpleLayers(t *testing.T) {
	global := json.RawMessage(`{"model":"sonnet","permissions":{"allow":["Bash(*)"]}}`)
	role := json.RawMessage(`{"model":"opus"}`)

	merged := claudeconfig.MergeLayers(MergeOverride, []json.RawMessage{global, role})

	if merged["model"] != "opus" {
		t.Errorf("expected model=opus (role override), got %v", merged["model"])
	}

	// Permissions should be from global (role didn't override it).
	if merged["permissions"] == nil {
		t.Error("expected permissions key from global layer")
	}
}

func TestMergeSimpleLayers_RoleOverridesGlobal(t *testing.T) {
	global := json.RawMessage(`{"alwaysThinkingEnabled":true,"model":"sonnet"}`)
	role := json.RawMessage(`{"alwaysThinkingEnabled":false}`)

	merged := claudeconfig.MergeLayers(MergeOverride, []json.RawMessage{global, role})

	if merged["alwaysThinkingEnabled"] != false {
		t.Errorf("expected alwaysThinkingEnabled=false (role override), got %v", merged["alwaysThinkingEnabled"])
	}
	if merged["model"] != "sonnet" {
		t.Errorf("expected model=sonnet (from global), got %v", merged["model"])
	}
}

func TestMergeSimpleLayers_DeepMergeNestedMaps(t *testing.T) {
	// Role-scoped MCP bead overrides server-alpha but server-beta from global survives.
	global := json.RawMessage(`{"mcpServers":{"server-alpha":{"command":"alpha-cmd"},"server-beta":{"command":"beta-cmd"}}}`)
	role := json.RawMessage(`{"mcpServers":{"server-alpha":{"command":"crew-alpha-cmd"}}}`)

	merged := claudeconfig.MergeLayers(MergeOverride, []json.RawMessage{global, role})

	servers, ok := merged["mcpServers"].(map[string]any)
	if !ok {
		t.Fatal("expected mcpServers key")
	}

	// server-alpha should be overridden by role.
	alpha, ok := servers["server-alpha"].(map[string]any)
	if !ok {
		t.Fatal("expected server-alpha")
	}
	if alpha["command"] != "crew-alpha-cmd" {
		t.Errorf("expected server-alpha command=crew-alpha-cmd, got %v", alpha["command"])
	}

	// server-beta from global should survive.
	beta, ok := servers["server-beta"].(map[string]any)
	if !ok {
		t.Fatal("expected server-beta from global to survive")
	}
	if beta["command"] != "beta-cmd" {
		t.Errorf("expected server-beta command=beta-cmd, got %v", beta["command"])
	}
}

func TestWriteUserSettings(t *testing.T) {
	// Override HOME to a temp dir so we don't write to the real home.
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	settings := map[string]any{
		"model":                 "opus",
		"alwaysThinkingEnabled": true,
	}

	if err := writeUserSettings(settings, "", "", false); err != nil {
		t.Fatalf("writeUserSettings: %v", err)
	}

	outPath := filepath.Join(tmpDir, ".claude", "settings.json")
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("reading settings: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("parsing settings JSON: %v", err)
	}

	if result["model"] != "opus" {
		t.Errorf("expected model=opus, got %v", result["model"])
	}
	if result["alwaysThinkingEnabled"] != true {
		t.Errorf("expected alwaysThinkingEnabled=true, got %v", result["alwaysThinkingEnabled"])
	}
}

func TestWriteUserSettings_ModelPreservedThroughPipeline(t *testing.T) {
	// Verifies that model set by per-role config survives the full
	// writeUserSettings pipeline (plugins, marketplace, teams env, deny list).
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	// Disable teams env injection for this test.
	t.Setenv("CLAUDE_TEAMS_ENABLED", "")

	// Simulate merged settings from crew-lite role (sonnet).
	settings := map[string]any{
		"model":                 "claude-sonnet-4-6",
		"alwaysThinkingEnabled": true,
		"permissions": map[string]any{
			"allow": []any{"Bash(*)", "Read(*)"},
		},
	}

	// Override lookPath to avoid depending on actual binaries.
	origLookPath := lookPath
	lookPath = func(name string) (string, error) { return "", fmt.Errorf("not found") }
	defer func() { lookPath = origLookPath }()

	if err := writeUserSettings(settings, "", "", false); err != nil {
		t.Fatalf("writeUserSettings: %v", err)
	}

	outPath := filepath.Join(tmpDir, ".claude", "settings.json")
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("reading settings: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("parsing settings JSON: %v", err)
	}

	// Model must survive through plugins/marketplace/teams/deny injection.
	if result["model"] != "claude-sonnet-4-6" {
		t.Errorf("expected model=claude-sonnet-4-6, got %v", result["model"])
	}

	// Permissions deny list must include interactive tools.
	perms, ok := result["permissions"].(map[string]any)
	if !ok {
		t.Fatal("expected permissions key")
	}
	deny, ok := perms["deny"].([]any)
	if !ok {
		t.Fatal("expected deny list")
	}
	denySet := make(map[string]bool)
	for _, d := range deny {
		if s, ok := d.(string); ok {
			denySet[s] = true
		}
	}
	if !denySet["EnterPlanMode"] {
		t.Error("expected EnterPlanMode in deny list")
	}
}

// denySetFromSettings extracts settings.permissions.deny as a string set.
func denySetFromSettings(t *testing.T, settings map[string]any) map[string]bool {
	t.Helper()
	perms, ok := settings["permissions"].(map[string]any)
	if !ok {
		t.Fatal("expected permissions key")
	}
	deny, ok := perms["deny"].([]any)
	if !ok {
		t.Fatal("expected deny list")
	}
	set := make(map[string]bool)
	for _, d := range deny {
		if s, ok := d.(string); ok {
			set[s] = true
		}
	}
	return set
}

// TestEnsureRoleToolsDenied_Crew verifies the crew role gets the in-session
// task/todo tools appended to its deny list (gd-C6wzy8qLWa): the soft prompt
// prohibition becomes a hard, permission-level block.
func TestEnsureRoleToolsDenied_Crew(t *testing.T) {
	settings := defaultUserSettings()
	ensureRoleToolsDenied(settings, "crew")

	deny := denySetFromSettings(t, settings)
	for _, tool := range []string{"TodoWrite", "TaskCreate", "TaskUpdate"} {
		if !deny[tool] {
			t.Errorf("expected crew deny list to contain %q", tool)
		}
	}
	// The fleet-wide interactive denies from defaultUserSettings() must remain.
	if !deny["EnterPlanMode"] {
		t.Error("crew deny must preserve the fleet-wide interactive denies")
	}
}

// TestEnsureRoleToolsDenied_NonCrewUnaffected verifies the crew-scoped deny does
// NOT leak into other roles — the deny must stay role-scoped to bound blast
// radius (task gd-YJXQZa27Nh scope decision).
func TestEnsureRoleToolsDenied_NonCrewUnaffected(t *testing.T) {
	for _, role := range []string{"gardener", "thread", "first-mate", ""} {
		settings := defaultUserSettings()
		ensureRoleToolsDenied(settings, role)
		deny := denySetFromSettings(t, settings)
		for _, tool := range []string{"TodoWrite", "TaskCreate", "TaskUpdate"} {
			if deny[tool] {
				t.Errorf("role %q must NOT deny %q (crew-scoped only)", role, tool)
			}
		}
	}
}

// TestEnsureRoleToolsDenied_MultiRoleWithCrew verifies a comma-separated
// multi-role agent that includes crew still gets the crew denies.
func TestEnsureRoleToolsDenied_MultiRoleWithCrew(t *testing.T) {
	settings := defaultUserSettings()
	ensureRoleToolsDenied(settings, "gardener,crew")

	deny := denySetFromSettings(t, settings)
	if !deny["TodoWrite"] {
		t.Error("expected multi-role agent including crew to deny TodoWrite")
	}
}

// TestEnsureRoleToolsDenied_PreservesConfigBeadDeny verifies a role's own
// pre-existing deny entries (e.g. from a config bead) are preserved and not
// duplicated when the crew denies are appended.
func TestEnsureRoleToolsDenied_PreservesConfigBeadDeny(t *testing.T) {
	settings := map[string]any{
		"permissions": map[string]any{
			"deny": []any{"CustomTool", "TodoWrite"},
		},
	}
	ensureRoleToolsDenied(settings, "crew")

	perms := settings["permissions"].(map[string]any)
	deny := perms["deny"].([]any)
	count := make(map[string]int)
	for _, d := range deny {
		count[d.(string)]++
	}
	if count["CustomTool"] != 1 {
		t.Errorf("expected CustomTool preserved exactly once, got %d", count["CustomTool"])
	}
	if count["TodoWrite"] != 1 {
		t.Errorf("expected TodoWrite deduplicated to exactly one entry, got %d", count["TodoWrite"])
	}
	if count["TaskCreate"] != 1 || count["TaskUpdate"] != 1 {
		t.Errorf("expected TaskCreate/TaskUpdate each appended once, got %d/%d",
			count["TaskCreate"], count["TaskUpdate"])
	}
}

// TestWriteUserSettings_CrewDeniesTaskTools verifies the crew role's deny list
// reaches the rendered settings.json through the full writeUserSettings
// pipeline — the end-to-end guarantee that a crew pod's Claude Code rejects the
// in-session task/todo tools at the permission level.
func TestWriteUserSettings_CrewDeniesTaskTools(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("CLAUDE_TEAMS_ENABLED", "")

	origLookPath := lookPath
	lookPath = func(name string) (string, error) { return "", fmt.Errorf("not found") }
	defer func() { lookPath = origLookPath }()

	if err := writeUserSettings(defaultUserSettings(), "", "crew", false); err != nil {
		t.Fatalf("writeUserSettings: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(tmpDir, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("reading settings: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("parsing settings JSON: %v", err)
	}

	deny := denySetFromSettings(t, result)
	for _, tool := range []string{"TodoWrite", "TaskCreate", "TaskUpdate"} {
		if !deny[tool] {
			t.Errorf("expected rendered crew settings.json deny list to contain %q", tool)
		}
	}
}

// TestWriteUserSettings_NonCrewDoesNotDenyTaskTools is the negative end-to-end
// guard: a non-crew role's rendered settings.json must NOT deny the task tools.
func TestWriteUserSettings_NonCrewDoesNotDenyTaskTools(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("CLAUDE_TEAMS_ENABLED", "")

	origLookPath := lookPath
	lookPath = func(name string) (string, error) { return "", fmt.Errorf("not found") }
	defer func() { lookPath = origLookPath }()

	if err := writeUserSettings(defaultUserSettings(), "", "gardener", false); err != nil {
		t.Fatalf("writeUserSettings: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(tmpDir, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("reading settings: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("parsing settings JSON: %v", err)
	}

	deny := denySetFromSettings(t, result)
	if deny["TodoWrite"] {
		t.Error("gardener role must NOT deny TodoWrite (crew-scoped only)")
	}
	// The fleet-wide interactive denies must still be present for every role.
	if !deny["EnterPlanMode"] {
		t.Error("expected fleet-wide interactive denies for gardener role")
	}
}

func TestAppendDetectedPlugins(t *testing.T) {
	tests := []struct {
		name        string
		existing    map[string]any // initial enabledPlugins (nil = no key)
		goplsFound  bool
		rustFound   bool
		pod         bool            // agent-pod mode: no LSP plugins (gd-zBUZ9bwCKl)
		wantPlugins map[string]bool // expected plugin keys and values
	}{
		{
			name:        "nothing detected, no existing",
			existing:    nil,
			goplsFound:  false,
			rustFound:   false,
			wantPlugins: map[string]bool{},
		},
		{
			name:       "nothing detected, preserves existing",
			existing:   map[string]any{"custom-plugin": true},
			goplsFound: false,
			rustFound:  false,
			wantPlugins: map[string]bool{
				"custom-plugin": true,
			},
		},
		{
			name:       "gopls only",
			existing:   nil,
			goplsFound: true,
			rustFound:  false,
			wantPlugins: map[string]bool{
				"gopls-lsp@claude-plugins-official": true,
			},
		},
		{
			name:       "rust-analyzer only",
			existing:   nil,
			goplsFound: false,
			rustFound:  true,
			wantPlugins: map[string]bool{
				"rust-analyzer-lsp@claude-plugins-official": true,
			},
		},
		{
			name:       "both detected with existing",
			existing:   map[string]any{"custom-plugin": true},
			goplsFound: true,
			rustFound:  true,
			wantPlugins: map[string]bool{
				"custom-plugin":                             true,
				"gopls-lsp@claude-plugins-official":         true,
				"rust-analyzer-lsp@claude-plugins-official": true,
			},
		},
		{
			name:       "agent pod skips detected LSPs, keeps existing",
			existing:   map[string]any{"custom-plugin": true},
			goplsFound: true,
			rustFound:  true,
			pod:        true,
			wantPlugins: map[string]bool{
				"custom-plugin": true,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			orig := lookPath
			t.Cleanup(func() { lookPath = orig })

			lookPath = func(name string) (string, error) {
				switch name {
				case "gopls":
					if tt.goplsFound {
						return "/usr/bin/gopls", nil
					}
				case "rust-analyzer":
					if tt.rustFound {
						return "/usr/bin/rust-analyzer", nil
					}
				}
				return "", fmt.Errorf("not found: %s", name)
			}

			settings := map[string]any{}
			if tt.existing != nil {
				ep := make(map[string]any, len(tt.existing))
				for k, v := range tt.existing {
					ep[k] = v
				}
				settings["enabledPlugins"] = ep
			}

			appendDetectedPlugins(settings, !tt.pod)

			plugins, hasKey := settings["enabledPlugins"].(map[string]any)
			if !hasKey {
				t.Fatal("expected enabledPlugins key")
			}
			if len(plugins) != len(tt.wantPlugins) {
				t.Errorf("expected %d plugins, got %d: %v", len(tt.wantPlugins), len(plugins), plugins)
			}
			for k, want := range tt.wantPlugins {
				if got, ok := plugins[k].(bool); !ok || got != want {
					t.Errorf("plugin %q: want %v, got %v", k, want, plugins[k])
				}
			}
		})
	}
}

func TestWriteMCPConfig_NewFile(t *testing.T) {
	workspace := t.TempDir()
	config := map[string]any{
		"mcpServers": map[string]any{
			"playwright": map[string]any{
				"command": "playwright-mcp",
				"args":    []any{"--headless"},
			},
		},
	}

	if err := writeMCPConfig(workspace, config); err != nil {
		t.Fatalf("writeMCPConfig: %v", err)
	}

	outPath := filepath.Join(workspace, ".mcp.json")
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("reading .mcp.json: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("parsing JSON: %v", err)
	}

	servers, ok := result["mcpServers"].(map[string]any)
	if !ok {
		t.Fatal("expected mcpServers key")
	}
	if servers["playwright"] == nil {
		t.Error("expected playwright server")
	}
}

func TestWriteMCPConfig_PreservesExisting(t *testing.T) {
	workspace := t.TempDir()

	// Write an existing .mcp.json with a mezmo server.
	existing := `{"mcpServers":{"mezmo":{"type":"http","url":"https://mcp.mezmo.com"}}}`
	if err := os.WriteFile(filepath.Join(workspace, ".mcp.json"), []byte(existing), 0644); err != nil {
		t.Fatal(err)
	}

	// Write new config with playwright — should NOT overwrite mezmo.
	config := map[string]any{
		"mcpServers": map[string]any{
			"playwright": map[string]any{
				"command": "playwright-mcp",
			},
		},
	}

	if err := writeMCPConfig(workspace, config); err != nil {
		t.Fatalf("writeMCPConfig: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(workspace, ".mcp.json"))
	if err != nil {
		t.Fatal(err)
	}

	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}

	servers := result["mcpServers"].(map[string]any)
	if servers["mezmo"] == nil {
		t.Error("expected existing mezmo server to be preserved")
	}
	if servers["playwright"] == nil {
		t.Error("expected new playwright server to be added")
	}
}

func TestWriteMCPConfig_DoesNotOverrideExistingServer(t *testing.T) {
	workspace := t.TempDir()

	// Existing .mcp.json has playwright with custom args.
	existing := `{"mcpServers":{"playwright":{"command":"playwright-mcp","args":["--custom"]}}}`
	if err := os.WriteFile(filepath.Join(workspace, ".mcp.json"), []byte(existing), 0644); err != nil {
		t.Fatal(err)
	}

	// New config tries to set playwright with different args.
	config := map[string]any{
		"mcpServers": map[string]any{
			"playwright": map[string]any{
				"command": "playwright-mcp",
				"args":    []any{"--browser", "chromium", "--headless", "--no-sandbox"},
			},
		},
	}

	if err := writeMCPConfig(workspace, config); err != nil {
		t.Fatalf("writeMCPConfig: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(workspace, ".mcp.json"))
	if err != nil {
		t.Fatal(err)
	}

	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}

	servers := result["mcpServers"].(map[string]any)
	pw := servers["playwright"].(map[string]any)
	args := pw["args"].([]any)
	// Existing server is preserved (not overwritten by new config).
	// fixPlaywrightBrowser adds --browser chromium since the existing
	// config doesn't specify --browser. fixPlaywrightViewport adds
	// --viewport-size 1280,720.
	if len(args) != 5 || args[0] != "--browser" || args[1] != "chromium" || args[2] != "--custom" ||
		args[3] != "--viewport-size" || args[4] != "1280,720" {
		t.Errorf("expected [--browser chromium --custom --viewport-size 1280,720], got args=%v", args)
	}
	// fixPlaywrightEnv adds PLAYWRIGHT_BROWSERS_PATH.
	env, ok := pw["env"].(map[string]any)
	if !ok {
		t.Fatal("expected env to be added by fixPlaywrightEnv")
	}
	if env["PLAYWRIGHT_BROWSERS_PATH"] != "/ms-playwright" {
		t.Errorf("expected PLAYWRIGHT_BROWSERS_PATH=/ms-playwright, got %v", env["PLAYWRIGHT_BROWSERS_PATH"])
	}
}

// TestWriteMCPConfig_RemoveServers verifies the role-scoped removeServers lever
// (epic ka-0000003733): a config bead can strip a server that the on-disk
// .mcp.json (or the global bead) would otherwise carry, so a non-browser role
// sheds the playwright+mezmo tool-defs. Removal wins over the add-only merge and
// over the fixPlaywright* re-touch passes.
func TestWriteMCPConfig_RemoveServers(t *testing.T) {
	workspace := t.TempDir()

	// Existing .mcp.json carries both playwright and mezmo.
	existing := `{"mcpServers":{"playwright":{"command":"playwright-mcp"},"mezmo":{"type":"http","url":"https://mcp.mezmo.com"}}}`
	if err := os.WriteFile(filepath.Join(workspace, ".mcp.json"), []byte(existing), 0644); err != nil {
		t.Fatal(err)
	}

	// A non-browser role bead: declares no servers, removes both.
	config := map[string]any{
		"mcpServers":    map[string]any{},
		"removeServers": []any{"playwright", "mezmo"},
	}

	if err := writeMCPConfig(workspace, config); err != nil {
		t.Fatalf("writeMCPConfig: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(workspace, ".mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	servers, _ := result["mcpServers"].(map[string]any)
	if _, ok := servers["playwright"]; ok {
		t.Error("expected playwright to be removed by removeServers")
	}
	if _, ok := servers["mezmo"]; ok {
		t.Error("expected mezmo to be removed by removeServers")
	}
}

// TestWriteMCPConfig_RemoveWinsOverAdd verifies removal beats a same-config add:
// even if the resolved config re-adds a server via mcpServers, listing it in
// removeServers strips it from the final file (removal runs after the add-only
// merge and before the fixPlaywright* passes, so playwright is not re-touched).
func TestWriteMCPConfig_RemoveWinsOverAdd(t *testing.T) {
	workspace := t.TempDir()

	config := map[string]any{
		"mcpServers": map[string]any{
			"playwright": map[string]any{"command": "playwright-mcp"},
			"mezmo":      map[string]any{"type": "http", "url": "https://mcp.mezmo.com"},
		},
		"removeServers": []any{"playwright"},
	}

	if err := writeMCPConfig(workspace, config); err != nil {
		t.Fatalf("writeMCPConfig: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(workspace, ".mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	servers, _ := result["mcpServers"].(map[string]any)
	if _, ok := servers["playwright"]; ok {
		t.Error("expected playwright removed even though it was also added in the same config")
	}
	if _, ok := servers["mezmo"]; !ok {
		t.Error("expected mezmo (not in removeServers) to be preserved")
	}
}

// TestWriteMCPConfig_RemoveOnlyStripsExisting verifies a config that declares
// ONLY removeServers (empty mcpServers) still strips servers from the on-disk
// file rather than short-circuiting on the "no servers to configure" guard.
func TestWriteMCPConfig_RemoveOnlyStripsExisting(t *testing.T) {
	workspace := t.TempDir()

	existing := `{"mcpServers":{"playwright":{"command":"playwright-mcp"},"gardenio-channel":{"command":"bun"}}}`
	if err := os.WriteFile(filepath.Join(workspace, ".mcp.json"), []byte(existing), 0644); err != nil {
		t.Fatal(err)
	}

	config := map[string]any{
		"removeServers": []any{"playwright"},
	}

	if err := writeMCPConfig(workspace, config); err != nil {
		t.Fatalf("writeMCPConfig: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(workspace, ".mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	servers, _ := result["mcpServers"].(map[string]any)
	if _, ok := servers["playwright"]; ok {
		t.Error("expected playwright removed by removeServers-only config")
	}
	if _, ok := servers["gardenio-channel"]; !ok {
		t.Error("expected gardenio-channel preserved")
	}
}

func TestWriteMCPConfig_EmptyServers(t *testing.T) {
	workspace := t.TempDir()
	config := map[string]any{
		"mcpServers": map[string]any{},
	}

	if err := writeMCPConfig(workspace, config); err != nil {
		t.Fatalf("writeMCPConfig: %v", err)
	}

	// Should not create the file when there are no servers.
	outPath := filepath.Join(workspace, ".mcp.json")
	if _, err := os.Stat(outPath); !os.IsNotExist(err) {
		t.Error("expected no .mcp.json file when servers are empty")
	}
}

func TestWriteUserSettings_FilePermissions(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	if err := writeUserSettings(map[string]any{"model": "test"}, "", "", false); err != nil {
		t.Fatalf("writeUserSettings: %v", err)
	}

	outPath := filepath.Join(tmpDir, ".claude", "settings.json")
	info, err := os.Stat(outPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("expected file permissions 0600, got %o", perm)
	}
}

func TestWriteInstructionFiles_ClaudeMD(t *testing.T) {
	workspace := t.TempDir()
	config := map[string]any{
		"claude_md": "# Test Agent\n\nYou are a test agent.",
	}

	writeInstructionFiles(workspace, config, false)

	data, err := os.ReadFile(filepath.Join(workspace, "CLAUDE.md"))
	if err != nil {
		t.Fatalf("expected CLAUDE.md: %v", err)
	}
	if string(data) != "# Test Agent\n\nYou are a test agent." {
		t.Errorf("unexpected CLAUDE.md content: %s", data)
	}
}

func TestWriteInstructionFiles_StopGate(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	config := map[string]any{
		"stop_gate_blocked": "<system-reminder>STOP BLOCKED</system-reminder>",
	}

	writeInstructionFiles("/nonexistent", config, false)

	data, err := os.ReadFile(filepath.Join(tmpDir, ".claude", "stop-gate-text.md"))
	if err != nil {
		t.Fatalf("expected stop-gate-text.md: %v", err)
	}
	if string(data) != "<system-reminder>STOP BLOCKED</system-reminder>" {
		t.Errorf("unexpected stop-gate-text.md content: %s", data)
	}
}

func TestWriteInstructionFiles_EmptyConfig(t *testing.T) {
	workspace := t.TempDir()
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	// Neither key present — should not create any files.
	writeInstructionFiles(workspace, map[string]any{"other_key": "value"}, false)

	if _, err := os.Stat(filepath.Join(workspace, "CLAUDE.md")); !os.IsNotExist(err) {
		t.Error("expected no CLAUDE.md when key is absent")
	}
	if _, err := os.Stat(filepath.Join(tmpDir, ".claude", "stop-gate-text.md")); !os.IsNotExist(err) {
		t.Error("expected no stop-gate-text.md when key is absent")
	}
}

// TestWriteUserSettings_LocalModeWritesToWorkspace verifies that
// writeUserSettings in --local mode confines its write to
// <workspace>/.claude/settings.local.json and leaves $HOME/.claude/ untouched.
// ka-0000002300: gb setup claude --local must not write under ~/.claude/.
func TestWriteUserSettings_LocalModeWritesToWorkspace(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	t.Setenv("HOME", home)

	if err := writeUserSettings(map[string]any{"model": "test"}, workspace, "", true); err != nil {
		t.Fatalf("writeUserSettings: %v", err)
	}

	wsPath := filepath.Join(workspace, ".claude", "settings.local.json")
	if got := readJSONFile(t, wsPath)["model"]; got != "test" {
		t.Fatalf("expected model=test in %s, got %v", wsPath, got)
	}
	if _, err := os.Stat(filepath.Join(workspace, ".claude", "settings.json")); !os.IsNotExist(err) {
		t.Errorf("expected no workspace settings.json in local mode, got err=%v", err)
	}

	// $HOME/.claude/ must not be touched at all in local mode.
	if _, err := os.Stat(filepath.Join(home, ".claude")); !os.IsNotExist(err) {
		t.Errorf("expected no ~/.claude/ directory in local mode, got err=%v", err)
	}
}

// TestWriteUserSettings_LocalModeRequiresWorkspace verifies that
// writeUserSettings(..., "", true) fails fast rather than silently
// falling back to the home directory.
func TestWriteUserSettings_LocalModeRequiresWorkspace(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	err := writeUserSettings(map[string]any{"model": "test"}, "", "", true)
	if err == nil {
		t.Fatal("expected error when local=true and workspace is empty")
	}
	if _, err := os.Stat(filepath.Join(home, ".claude")); !os.IsNotExist(err) {
		t.Errorf("expected no ~/.claude/ created on error, got err=%v", err)
	}
}

// TestWriteInstructionFiles_LocalModeStopGate verifies that
// writeInstructionFiles writes stop-gate-text.md under <workspace>/.claude/
// in local mode and never touches ~/.claude/.
func TestWriteInstructionFiles_LocalModeStopGate(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	t.Setenv("HOME", home)

	config := map[string]any{
		"stop_gate_blocked": "<system-reminder>STOP BLOCKED</system-reminder>",
	}

	writeInstructionFiles(workspace, config, true)

	data, err := os.ReadFile(filepath.Join(workspace, ".claude", "stop-gate-text.md"))
	if err != nil {
		t.Fatalf("expected stop-gate-text.md under workspace: %v", err)
	}
	if string(data) != "<system-reminder>STOP BLOCKED</system-reminder>" {
		t.Errorf("unexpected stop-gate-text.md content: %s", data)
	}

	if _, err := os.Stat(filepath.Join(home, ".claude")); !os.IsNotExist(err) {
		t.Errorf("expected no ~/.claude/ touched in local mode, got err=%v", err)
	}
}

// TestRunSetupClaudeDefaults_LocalMode end-to-ends the local-mode setup:
// user settings, workspace hooks, and CLAUDE.md must all land under
// <workspace>/ and nothing must be written under $HOME/.claude/.
func TestRunSetupClaudeDefaults_LocalMode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	orig := lookPath
	t.Cleanup(func() { lookPath = orig })
	lookPath = func(name string) (string, error) {
		return "", fmt.Errorf("not found: %s", name)
	}

	workspace := filepath.Join(t.TempDir(), "workspace")
	if err := os.MkdirAll(workspace, 0755); err != nil {
		t.Fatal(err)
	}

	if err := runSetupClaudeDefaults(workspace, "crew", true); err != nil {
		t.Fatalf("runSetupClaudeDefaults: %v", err)
	}

	// Local mode never writes the repo-committed settings.json (gd-wKXxD4qXp6).
	if _, err := os.Stat(filepath.Join(workspace, ".claude", "settings.json")); !os.IsNotExist(err) {
		t.Errorf("expected no <workspace>/.claude/settings.json in local mode, got err=%v", err)
	}
	// settings.local.json carries both the user-level layer and the hooks.
	local := readJSONFile(t, filepath.Join(workspace, ".claude", "settings.local.json"))
	if _, ok := local["hooks"].(map[string]any); !ok {
		t.Errorf("expected hooks in settings.local.json, got %v", local["hooks"])
	}
	if local["alwaysThinkingEnabled"] != true {
		t.Errorf("expected user-level alwaysThinkingEnabled in settings.local.json, got %v", local["alwaysThinkingEnabled"])
	}
	// CLAUDE.md must exist in workspace.
	if _, err := os.Stat(filepath.Join(workspace, "CLAUDE.md")); err != nil {
		t.Errorf("expected <workspace>/CLAUDE.md: %v", err)
	}

	// Nothing must have been written under ~/.claude/.
	if _, err := os.Stat(filepath.Join(home, ".claude")); !os.IsNotExist(err) {
		t.Errorf("expected no ~/.claude/ in local mode, got err=%v", err)
	}
}

func TestRunSetupClaudeDefaults_WritesBothFiles(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	// Mock lookPath so playwright-mcp is not found (tests the no-MCP path).
	orig := lookPath
	t.Cleanup(func() { lookPath = orig })
	lookPath = func(name string) (string, error) {
		return "", fmt.Errorf("not found: %s", name)
	}

	workspace := filepath.Join(tmpDir, "workspace")
	if err := os.MkdirAll(workspace, 0755); err != nil {
		t.Fatal(err)
	}

	if err := runSetupClaudeDefaults(workspace, "crew", false); err != nil {
		t.Fatalf("runSetupClaudeDefaults: %v", err)
	}

	// User-level settings should exist.
	userPath := filepath.Join(tmpDir, ".claude", "settings.json")
	userData, err := os.ReadFile(userPath)
	if err != nil {
		t.Fatalf("expected user settings at %s: %v", userPath, err)
	}
	var userSettings map[string]any
	if err := json.Unmarshal(userData, &userSettings); err != nil {
		t.Fatalf("invalid user settings JSON: %v", err)
	}
	if userSettings["permissions"] == nil {
		t.Error("expected permissions in user settings")
	}

	// No .mcp.json should be created without config beads or playwright-mcp.
	mcpPath := filepath.Join(workspace, ".mcp.json")
	if _, err := os.Stat(mcpPath); !os.IsNotExist(err) {
		t.Error("expected no .mcp.json when playwright-mcp is not on PATH")
	}

	// Workspace-level hooks should exist.
	wsPath := filepath.Join(workspace, ".claude", "settings.local.json")
	wsData, err := os.ReadFile(wsPath)
	if err != nil {
		t.Fatalf("expected workspace settings at %s: %v", wsPath, err)
	}
	var wsSettings map[string]any
	if err := json.Unmarshal(wsData, &wsSettings); err != nil {
		t.Fatalf("invalid workspace settings JSON: %v", err)
	}
	if wsSettings["hooks"] == nil {
		t.Error("expected hooks in workspace settings")
	}

	// CLAUDE.md should be written as fallback.
	claudeMDPath := filepath.Join(workspace, "CLAUDE.md")
	claudeData, err := os.ReadFile(claudeMDPath)
	if err != nil {
		t.Fatalf("expected CLAUDE.md at %s: %v", claudeMDPath, err)
	}
	content := string(claudeData)
	if !strings.Contains(content, "# Gardenio Agent: crew") {
		t.Error("CLAUDE.md should contain role header")
	}
	if !strings.Contains(content, "## Claim Protocol") {
		t.Error("CLAUDE.md should contain Claim Protocol section")
	}
	if !strings.Contains(content, "## Development Tools") {
		t.Error("CLAUDE.md should contain Development Tools section")
	}
}

func TestRunSetupClaudeDefaults_DoesNotOverwriteExistingClaudeMD(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	workspace := filepath.Join(tmpDir, "workspace")
	if err := os.MkdirAll(workspace, 0755); err != nil {
		t.Fatal(err)
	}

	// Pre-create CLAUDE.md with custom content.
	claudeMDPath := filepath.Join(workspace, "CLAUDE.md")
	existing := "# Custom CLAUDE.md\nDo not overwrite me."
	if err := os.WriteFile(claudeMDPath, []byte(existing), 0644); err != nil {
		t.Fatal(err)
	}

	if err := runSetupClaudeDefaults(workspace, "gardener", false); err != nil {
		t.Fatalf("runSetupClaudeDefaults: %v", err)
	}

	// CLAUDE.md should NOT be overwritten.
	data, err := os.ReadFile(claudeMDPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != existing {
		t.Errorf("CLAUDE.md was overwritten; got %q, want %q", string(data), existing)
	}
}

func TestDefaultClaudeMD_SubstitutesRole(t *testing.T) {
	content := defaultClaudeMD("gardener")
	if !strings.Contains(content, "# Gardenio Agent: gardener") {
		t.Error("expected role in header")
	}
	if !strings.Contains(content, "**gardener** agent") {
		t.Error("expected role in body")
	}
}

func TestDefaultClaudeMD_IncludesProject(t *testing.T) {
	t.Setenv("BOAT_PROJECT", "testproject")
	t.Setenv("BOAT_AGENT", "test-agent")
	content := defaultClaudeMD("crew")
	if !strings.Contains(content, "(project: testproject)") {
		t.Error("expected project in output")
	}
	if !strings.Contains(content, "Agent name: test-agent") {
		t.Error("expected agent name in output")
	}
}

// TestDefaultClaudeMD_ResearchBeforeTests pins the operator policy from Slack
// thread gd-UJiGnNn7Mc in the fallback CLAUDE.md: research CI logs and state a
// hypothesis before running tests, run only the minimum subset, and escalate
// for permanent redress of a missing capability (gd-WL5Rq7g47y).
func TestDefaultClaudeMD_ResearchBeforeTests(t *testing.T) {
	content := defaultClaudeMD("crew")
	for _, want := range []string{
		"## Research Is Cheap, Running Tests Is Expensive",
		"--log-failed",
		"State a hypothesis",
		"minimum subset",
		"## Missing Access or Capability? Escalate for Permanent Redress",
		"gb escalation create",
		"successors",
		"security case",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("fallback CLAUDE.md should contain %q", want)
		}
	}
}

func TestInjectTeamsEnv_Enabled(t *testing.T) {
	t.Setenv("CLAUDE_TEAMS_ENABLED", "true")

	settings := map[string]any{"model": "opus"}
	injectTeamsEnv(settings, "crew")

	env, ok := settings["env"].(map[string]any)
	if !ok {
		t.Fatal("expected env key in settings")
	}
	if env["CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS"] != "1" {
		t.Errorf("expected CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS=1, got %v", env["CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS"])
	}
}

func TestInjectTeamsEnv_Disabled(t *testing.T) {
	t.Setenv("CLAUDE_TEAMS_ENABLED", "false")

	settings := map[string]any{"model": "opus"}
	injectTeamsEnv(settings, "crew")

	if _, ok := settings["env"]; ok {
		t.Error("expected no env key when teams is disabled")
	}
}

func TestInjectTeamsEnv_PreservesExistingEnv(t *testing.T) {
	t.Setenv("CLAUDE_TEAMS_ENABLED", "1")

	settings := map[string]any{
		"env": map[string]any{"EXISTING_VAR": "keep"},
	}
	injectTeamsEnv(settings, "crew")

	env := settings["env"].(map[string]any)
	if env["EXISTING_VAR"] != "keep" {
		t.Error("expected existing env var to be preserved")
	}
	if env["CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS"] != "1" {
		t.Error("expected teams flag to be injected")
	}
}

// TestInjectTeamsEnv_PureOrchestratorRolesShed verifies that the proven
// pure-orchestrator roles shed the teams flag even when CLAUDE_TEAMS_ENABLED is
// set (gd-q3iWgPXXsZ / design ka-0000004160#5, Option D).
func TestInjectTeamsEnv_PureOrchestratorRolesShed(t *testing.T) {
	t.Setenv("CLAUDE_TEAMS_ENABLED", "1")

	for _, role := range []string{"plan-gardener", "gardener-delivery", "gardener-bugfix", "gardener-release-owner"} {
		t.Run(role, func(t *testing.T) {
			settings := map[string]any{"model": "opus"}
			injectTeamsEnv(settings, role)
			if _, ok := settings["env"]; ok {
				t.Errorf("expected no env key for pure-orchestrator role %q, got %v", role, settings["env"])
			}
		})
	}
}

// TestInjectTeamsEnv_NonAllowlistedRolesKeepFlag verifies the gate is a
// conservative allowlist: every role NOT proven pure-orchestrator (including the
// too-broad bare `gardener` label) keeps the Agent Teams flag.
func TestInjectTeamsEnv_NonAllowlistedRolesKeepFlag(t *testing.T) {
	t.Setenv("CLAUDE_TEAMS_ENABLED", "1")

	for _, role := range []string{"crew", "thread", "gardener", "gardener-intake", "auto-inspector", ""} {
		t.Run("role="+role, func(t *testing.T) {
			settings := map[string]any{"model": "opus"}
			injectTeamsEnv(settings, role)
			env, ok := settings["env"].(map[string]any)
			if !ok {
				t.Fatalf("expected env key for role %q", role)
			}
			if env["CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS"] != "1" {
				t.Errorf("expected teams flag kept for role %q, got %v", role, env["CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS"])
			}
		})
	}
}

// TestInjectTeamsEnv_MultiRoleKeepsFlagUnlessAllShed verifies a multi-role agent
// only sheds the flag when EVERY one of its roles is allowlisted — a composite
// role that includes e.g. crew is never silently stripped of the Agent tool.
func TestInjectTeamsEnv_MultiRoleKeepsFlagUnlessAllShed(t *testing.T) {
	t.Setenv("CLAUDE_TEAMS_ENABLED", "1")

	// Mixed: one pure-orchestrator + one general role → keep the flag.
	settings := map[string]any{"model": "opus"}
	injectTeamsEnv(settings, "gardener-delivery,crew")
	if _, ok := settings["env"]; !ok {
		t.Error("expected teams flag kept for mixed role gardener-delivery,crew")
	}

	// All pure-orchestrator → shed the flag.
	settings = map[string]any{"model": "opus"}
	injectTeamsEnv(settings, "gardener-delivery,plan-gardener")
	if _, ok := settings["env"]; ok {
		t.Error("expected teams flag shed for all-orchestrator roles gardener-delivery,plan-gardener")
	}
}

func TestDefaultHookSettings_IncludesTeamHooks(t *testing.T) {
	settings := defaultHookSettings()
	hooks := settings["hooks"].(map[string]any)

	for _, hookType := range []string{"TaskCompleted", "TeammateIdle"} {
		arr, ok := hooks[hookType].([]any)
		if !ok || len(arr) == 0 {
			t.Errorf("expected %s hook in default settings", hookType)
		}
	}
}

func TestDefaultHookSettings_IncludesErrorEventRelays(t *testing.T) {
	settings := defaultHookSettings()
	hooks := settings["hooks"].(map[string]any)

	for _, hookType := range []string{"PostToolUseFailure", "StopFailure"} {
		arr, ok := hooks[hookType].([]any)
		if !ok || len(arr) == 0 {
			t.Errorf("expected %s hook in default settings", hookType)
			continue
		}
		entryMap := arr[0].(map[string]any)
		innerHooks := entryMap["hooks"].([]any)
		hMap := innerHooks[0].(map[string]any)
		cmd := hMap["command"].(string)
		if !strings.Contains(cmd, "gb hook relay") {
			t.Errorf("expected %s hook to contain 'gb hook relay', got %s", hookType, cmd)
		}
	}
}

func TestDefaultHookSettings_IncludesRemainingEventRelays(t *testing.T) {
	settings := defaultHookSettings()
	hooks := settings["hooks"].(map[string]any)

	for _, hookType := range []string{
		"TaskCreated", "Notification", "CwdChanged", "FileChanged",
		"ConfigChange", "InstructionsLoaded", "Elicitation", "ElicitationResult",
		"WorktreeCreate", "WorktreeRemove", "Setup", "PermissionRequest",
	} {
		arr, ok := hooks[hookType].([]any)
		if !ok || len(arr) == 0 {
			t.Errorf("expected %s hook in default settings", hookType)
			continue
		}
		entryMap := arr[0].(map[string]any)
		innerHooks := entryMap["hooks"].([]any)
		hMap := innerHooks[0].(map[string]any)
		cmd := hMap["command"].(string)
		if !strings.Contains(cmd, "gb hook relay") {
			t.Errorf("expected %s hook to contain 'gb hook relay', got %s", hookType, cmd)
		}
	}
}

func TestDefaultHookSettings_IncludesDrainQueueHook(t *testing.T) {
	settings := defaultHookSettings()
	hooks := settings["hooks"].(map[string]any)

	postToolUse, ok := hooks["PostToolUse"].([]any)
	if !ok || len(postToolUse) == 0 {
		t.Fatal("expected PostToolUse hooks with drain-queue.sh")
	}

	found := false
	for _, entry := range postToolUse {
		entryMap := entry.(map[string]any)
		innerHooks := entryMap["hooks"].([]any)
		for _, h := range innerHooks {
			hMap := h.(map[string]any)
			cmd := hMap["command"].(string)
			if strings.Contains(cmd, "drain-queue.sh") {
				found = true
			}
		}
	}
	if !found {
		t.Error("expected drain-queue.sh in PostToolUse hooks")
	}
}

func TestDefaultMCPConfig_PlaywrightFound(t *testing.T) {
	orig := lookPath
	t.Cleanup(func() { lookPath = orig })

	lookPath = func(name string) (string, error) {
		if name == "playwright-mcp" {
			return "/usr/bin/playwright-mcp", nil
		}
		return "", fmt.Errorf("not found: %s", name)
	}

	config := defaultMCPConfig()
	if config == nil {
		t.Fatal("expected non-nil MCP config when playwright-mcp is on PATH")
	}

	servers, ok := config["mcpServers"].(map[string]any)
	if !ok {
		t.Fatal("expected mcpServers key")
	}
	if servers["playwright"] == nil {
		t.Error("expected playwright server entry")
	}
	pw := servers["playwright"].(map[string]any)
	env, ok := pw["env"].(map[string]any)
	if !ok {
		t.Fatal("expected env key in playwright config")
	}
	if env["PLAYWRIGHT_BROWSERS_PATH"] != "/ms-playwright" {
		t.Errorf("expected PLAYWRIGHT_BROWSERS_PATH=/ms-playwright, got %v", env["PLAYWRIGHT_BROWSERS_PATH"])
	}
}

func TestDefaultMCPConfig_PlaywrightNotFound(t *testing.T) {
	orig := lookPath
	t.Cleanup(func() { lookPath = orig })

	lookPath = func(name string) (string, error) {
		return "", fmt.Errorf("not found: %s", name)
	}

	config := defaultMCPConfig()
	if config != nil {
		t.Errorf("expected nil MCP config when playwright-mcp is not on PATH, got %v", config)
	}
}

func TestFixPlaywrightBrowser_AddsBrowserWhenMissing(t *testing.T) {
	servers := map[string]any{
		"playwright": map[string]any{
			"command": "playwright-mcp",
			"args":    []any{"--headless", "--no-sandbox"},
		},
	}

	fixPlaywrightBrowser(servers)

	cfg := servers["playwright"].(map[string]any)
	args := cfg["args"].([]any)
	if len(args) != 4 {
		t.Fatalf("expected 4 args, got %d: %v", len(args), args)
	}
	if args[0] != "--browser" || args[1] != "chromium" {
		t.Errorf("expected --browser chromium prepended, got %v %v", args[0], args[1])
	}
}

func TestFixPlaywrightBrowser_SkipsWhenBrowserPresent(t *testing.T) {
	servers := map[string]any{
		"playwright": map[string]any{
			"command": "playwright-mcp",
			"args":    []any{"--browser", "chrome", "--headless"},
		},
	}

	fixPlaywrightBrowser(servers)

	cfg := servers["playwright"].(map[string]any)
	args := cfg["args"].([]any)
	if len(args) != 3 {
		t.Fatalf("expected 3 args (unchanged), got %d: %v", len(args), args)
	}
	if args[1] != "chrome" {
		t.Errorf("expected browser=chrome to be preserved, got %v", args[1])
	}
}

func TestFixPlaywrightBrowser_NoPlaywright(t *testing.T) {
	servers := map[string]any{
		"other-server": map[string]any{"command": "foo"},
	}

	// Should not panic or modify anything
	fixPlaywrightBrowser(servers)

	if _, ok := servers["playwright"]; ok {
		t.Error("should not create playwright entry")
	}
}

func TestFixPlaywrightEnv_AddsWhenMissing(t *testing.T) {
	servers := map[string]any{
		"playwright": map[string]any{
			"command": "playwright-mcp",
			"args":    []any{"--browser", "chromium"},
		},
	}

	fixPlaywrightEnv(servers)

	cfg := servers["playwright"].(map[string]any)
	env, ok := cfg["env"].(map[string]any)
	if !ok {
		t.Fatal("expected env to be added")
	}
	if env["PLAYWRIGHT_BROWSERS_PATH"] != "/ms-playwright" {
		t.Errorf("expected PLAYWRIGHT_BROWSERS_PATH=/ms-playwright, got %v", env["PLAYWRIGHT_BROWSERS_PATH"])
	}
}

func TestFixPlaywrightEnv_SkipsWhenPresent(t *testing.T) {
	servers := map[string]any{
		"playwright": map[string]any{
			"command": "playwright-mcp",
			"env": map[string]any{
				"PLAYWRIGHT_BROWSERS_PATH": "/custom/path",
			},
		},
	}

	fixPlaywrightEnv(servers)

	cfg := servers["playwright"].(map[string]any)
	env := cfg["env"].(map[string]any)
	if env["PLAYWRIGHT_BROWSERS_PATH"] != "/custom/path" {
		t.Errorf("expected custom path to be preserved, got %v", env["PLAYWRIGHT_BROWSERS_PATH"])
	}
}

func TestFixPlaywrightEnv_NoPlaywright(t *testing.T) {
	servers := map[string]any{
		"other-server": map[string]any{"command": "foo"},
	}

	fixPlaywrightEnv(servers)

	if _, ok := servers["playwright"]; ok {
		t.Error("should not create playwright entry")
	}
}

func TestFixPlaywrightViewport_AddsWhenMissing(t *testing.T) {
	servers := map[string]any{
		"playwright": map[string]any{
			"command": "playwright-mcp",
			"args":    []any{"--browser", "chromium", "--headless"},
		},
	}

	fixPlaywrightViewport(servers)

	cfg := servers["playwright"].(map[string]any)
	args := cfg["args"].([]any)
	if len(args) != 5 {
		t.Fatalf("expected 5 args, got %d: %v", len(args), args)
	}
	if args[3] != "--viewport-size" || args[4] != "1280,720" {
		t.Errorf("expected --viewport-size 1280,720 appended, got %v %v", args[3], args[4])
	}
}

func TestFixPlaywrightViewport_SkipsWhenPresent(t *testing.T) {
	servers := map[string]any{
		"playwright": map[string]any{
			"command": "playwright-mcp",
			"args":    []any{"--viewport-size", "1920,1080"},
		},
	}

	fixPlaywrightViewport(servers)

	cfg := servers["playwright"].(map[string]any)
	args := cfg["args"].([]any)
	if len(args) != 2 {
		t.Fatalf("expected 2 args (unchanged), got %d: %v", len(args), args)
	}
}

func TestFixPlaywrightVideo_AddsOutputDirWhenSaveVideoPresent(t *testing.T) {
	servers := map[string]any{
		"playwright": map[string]any{
			"command": "playwright-mcp",
			"args":    []any{"--browser", "chromium", "--headless", "--save-video"},
		},
	}

	fixPlaywrightVideo(servers)

	cfg := servers["playwright"].(map[string]any)
	args := cfg["args"].([]any)
	if len(args) != 6 {
		t.Fatalf("expected 6 args, got %d: %v", len(args), args)
	}
	if args[4] != "--output-dir" || args[5] != "/tmp/playwright-videos" {
		t.Errorf("expected --output-dir /tmp/playwright-videos appended, got %v %v", args[4], args[5])
	}
}

func TestFixPlaywrightVideo_SkipsWhenNoSaveVideo(t *testing.T) {
	servers := map[string]any{
		"playwright": map[string]any{
			"command": "playwright-mcp",
			"args":    []any{"--browser", "chromium", "--headless"},
		},
	}

	fixPlaywrightVideo(servers)

	cfg := servers["playwright"].(map[string]any)
	args := cfg["args"].([]any)
	if len(args) != 3 {
		t.Fatalf("expected 3 args (unchanged), got %d: %v", len(args), args)
	}
}

func TestFixPlaywrightVideo_SkipsWhenOutputDirAlreadyPresent(t *testing.T) {
	servers := map[string]any{
		"playwright": map[string]any{
			"command": "playwright-mcp",
			"args":    []any{"--save-video", "--output-dir", "/custom/video/path"},
		},
	}

	fixPlaywrightVideo(servers)

	cfg := servers["playwright"].(map[string]any)
	args := cfg["args"].([]any)
	if len(args) != 3 {
		t.Fatalf("expected 3 args (unchanged), got %d: %v", len(args), args)
	}
	if args[2] != "/custom/video/path" {
		t.Errorf("expected custom output dir preserved, got %v", args[2])
	}
}

func TestFixPlaywrightVideo_NoPlaywright(t *testing.T) {
	servers := map[string]any{
		"other-server": map[string]any{"command": "foo"},
	}

	fixPlaywrightVideo(servers)

	if _, ok := servers["playwright"]; ok {
		t.Error("should not create playwright entry")
	}
}

func TestEnsureGardenioChannel_AddsWhenEnabled(t *testing.T) {
	// Create a temp dir with the channel script.
	workspace := t.TempDir()
	channelDir := filepath.Join(workspace, "channel")
	if err := os.MkdirAll(channelDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(channelDir, "gardenio-channel.ts"), []byte("// test"), 0644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GARDENIO_CHANNEL", "1")
	t.Setenv("GARDENIO_CHANNEL_PORT", "9999")

	servers := map[string]any{}
	ensureGardenioChannel(workspace, servers)

	entry, ok := servers["gardenio-channel"]
	if !ok {
		t.Fatal("expected gardenio-channel server to be added")
	}
	cfg := entry.(map[string]any)
	if cfg["command"] != "bun" {
		t.Errorf("expected command 'bun', got %v", cfg["command"])
	}
}

func TestEnsureGardenioChannel_SkipsWhenDisabled(t *testing.T) {
	t.Setenv("GARDENIO_CHANNEL", "0")
	servers := map[string]any{}
	ensureGardenioChannel(t.TempDir(), servers)

	if _, ok := servers["gardenio-channel"]; ok {
		t.Error("should not add gardenio-channel when GARDENIO_CHANNEL=0")
	}
}

func TestEnsureGardenioChannel_SkipsWhenAlreadyConfigured(t *testing.T) {
	t.Setenv("GARDENIO_CHANNEL", "1")
	servers := map[string]any{
		"gardenio-channel": map[string]any{"command": "custom"},
	}
	ensureGardenioChannel(t.TempDir(), servers)

	cfg := servers["gardenio-channel"].(map[string]any)
	if cfg["command"] != "custom" {
		t.Error("should not overwrite existing gardenio-channel config")
	}
}

func TestEnsureSlackPlaywright_AddsWhenStateFileExists(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(state, []byte(`{"cookies":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SLACK_VIEW_STORAGE_STATE", state)

	servers := map[string]any{}
	ensureSlackPlaywright(servers)

	cfg, ok := servers["playwright-slack"].(map[string]any)
	if !ok {
		t.Fatal("expected playwright-slack server to be added")
	}
	if cfg["command"] != "playwright-mcp" {
		t.Errorf("expected command playwright-mcp, got %v", cfg["command"])
	}
	args := cfg["args"].([]any)
	has := func(flag, val string) bool {
		for i, a := range args {
			if a == flag && (val == "" || (i+1 < len(args) && args[i+1] == val)) {
				return true
			}
		}
		return false
	}
	for _, c := range [][2]string{{"--browser", "chrome"}, {"--isolated", ""}, {"--storage-state", state}, {"--no-sandbox", ""}} {
		if !has(c[0], c[1]) {
			t.Errorf("expected args to contain %s %s, got %v", c[0], c[1], args)
		}
	}
	// The default playwright server is untouched.
	if _, ok := servers["playwright"]; ok {
		t.Error("must not add the default playwright server")
	}
}

func TestEnsureSlackPlaywright_SkipsWhenEnvUnset(t *testing.T) {
	t.Setenv("SLACK_VIEW_STORAGE_STATE", "")
	servers := map[string]any{}
	ensureSlackPlaywright(servers)
	if len(servers) != 0 {
		t.Errorf("expected no servers, got %v", servers)
	}
}

func TestEnsureSlackPlaywright_SkipsWhenFileMissing(t *testing.T) {
	t.Setenv("SLACK_VIEW_STORAGE_STATE", filepath.Join(t.TempDir(), "nope.json"))
	servers := map[string]any{}
	ensureSlackPlaywright(servers)
	if len(servers) != 0 {
		t.Errorf("expected no servers, got %v", servers)
	}
}

func TestEnsureSlackPlaywright_SkipsWhenAlreadyConfigured(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(state, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SLACK_VIEW_STORAGE_STATE", state)
	servers := map[string]any{"playwright-slack": map[string]any{"command": "custom"}}
	ensureSlackPlaywright(servers)
	if servers["playwright-slack"].(map[string]any)["command"] != "custom" {
		t.Error("should not overwrite existing playwright-slack config")
	}
}

func TestExpandEnvVars(t *testing.T) {
	tests := []struct {
		name  string
		input string
		envs  map[string]string
		want  string
	}{
		{
			name:  "single var",
			input: `Bearer ${MEZMO_SERVICE_KEY}`,
			envs:  map[string]string{"MEZMO_SERVICE_KEY": "sts_abc123"},
			want:  `Bearer sts_abc123`,
		},
		{
			name:  "multiple vars",
			input: `${FOO} and ${BAR}`,
			envs:  map[string]string{"FOO": "hello", "BAR": "world"},
			want:  `hello and world`,
		},
		{
			name:  "unset var becomes empty",
			input: `Bearer ${MISSING_KEY}`,
			envs:  map[string]string{},
			want:  `Bearer `,
		},
		{
			name:  "no placeholders",
			input: `plain text with $dollars`,
			envs:  map[string]string{},
			want:  `plain text with $dollars`,
		},
		{
			name:  "bare $VAR not expanded",
			input: `$NOT_EXPANDED`,
			envs:  map[string]string{"NOT_EXPANDED": "oops"},
			want:  `$NOT_EXPANDED`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range tt.envs {
				t.Setenv(k, v)
			}
			got := expandEnvVars(tt.input)
			if got != tt.want {
				t.Errorf("expandEnvVars(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestWriteMCPConfig_ExpandsEnvVars(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv("MEZMO_SERVICE_KEY", "sts_test_key_123")

	config := map[string]any{
		"mcpServers": map[string]any{
			"mezmo": map[string]any{
				"type": "http",
				"url":  "https://mcp.mezmo.com/mcp",
				"headers": map[string]any{
					"Authorization": "Bearer ${MEZMO_SERVICE_KEY}",
				},
			},
		},
	}

	if err := writeMCPConfig(workspace, config); err != nil {
		t.Fatalf("writeMCPConfig: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(workspace, ".mcp.json"))
	if err != nil {
		t.Fatal(err)
	}

	content := string(data)
	if strings.Contains(content, "${MEZMO_SERVICE_KEY}") {
		t.Error("expected ${MEZMO_SERVICE_KEY} to be expanded")
	}
	if !strings.Contains(content, "sts_test_key_123") {
		t.Error("expected expanded value sts_test_key_123 in output")
	}
}

func TestJIRAEnabled_AllVarsSet(t *testing.T) {
	t.Setenv("JIRA_BASE_URL", "https://example.atlassian.net")
	t.Setenv("JIRA_EMAIL", "test@example.com")
	t.Setenv("JIRA_API_TOKEN", "token123")

	if !jiraEnabled() {
		t.Error("expected jiraEnabled() to return true when all vars are set")
	}
}

func TestJIRAEnabled_MissingVars(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		email   string
		token   string
	}{
		{"missing base URL", "", "test@example.com", "token123"},
		{"missing email", "https://example.atlassian.net", "", "token123"},
		{"missing token", "https://example.atlassian.net", "test@example.com", ""},
		{"all missing", "", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("JIRA_BASE_URL", tt.baseURL)
			t.Setenv("JIRA_EMAIL", tt.email)
			t.Setenv("JIRA_API_TOKEN", tt.token)

			if jiraEnabled() {
				t.Error("expected jiraEnabled() to return false")
			}
		})
	}
}

func TestAppendJIRAStatusHooks_WhenEnabled(t *testing.T) {
	t.Setenv("JIRA_BASE_URL", "https://example.atlassian.net")
	t.Setenv("JIRA_EMAIL", "test@example.com")
	t.Setenv("JIRA_API_TOKEN", "token123")

	settings := defaultHookSettings()
	appendJIRAStatusHooks(settings)

	hooks := settings["hooks"].(map[string]any)

	// Check PreToolUse hook was added
	preToolUse, ok := hooks["PreToolUse"].([]any)
	if !ok || len(preToolUse) == 0 {
		t.Fatal("expected PreToolUse hooks to be added")
	}
	found := false
	for _, entry := range preToolUse {
		entryMap := entry.(map[string]any)
		if entryMap["matcher"] == "Bash" {
			innerHooks := entryMap["hooks"].([]any)
			for _, h := range innerHooks {
				hMap := h.(map[string]any)
				cmd := hMap["command"].(string)
				if strings.Contains(cmd, "jira-status-save.sh") {
					found = true
				}
			}
		}
	}
	if !found {
		t.Error("expected jira-status-save.sh in PreToolUse hooks")
	}

	// Check PostToolUse hook was added
	postToolUse, ok := hooks["PostToolUse"].([]any)
	if !ok || len(postToolUse) == 0 {
		t.Fatal("expected PostToolUse hooks")
	}
	found = false
	for _, entry := range postToolUse {
		entryMap := entry.(map[string]any)
		if matcher, ok := entryMap["matcher"].(string); ok && matcher == "Bash" {
			innerHooks := entryMap["hooks"].([]any)
			for _, h := range innerHooks {
				hMap := h.(map[string]any)
				cmd := hMap["command"].(string)
				if strings.Contains(cmd, "jira-status-reset.sh") {
					found = true
				}
			}
		}
	}
	if !found {
		t.Error("expected jira-status-reset.sh in PostToolUse hooks")
	}
}

func TestAppendJIRAStatusHooks_WhenDisabled(t *testing.T) {
	t.Setenv("JIRA_BASE_URL", "")
	t.Setenv("JIRA_EMAIL", "")
	t.Setenv("JIRA_API_TOKEN", "")

	settings := defaultHookSettings()
	appendJIRAStatusHooks(settings)

	hooks := settings["hooks"].(map[string]any)

	// PreToolUse should only have the hook relay (no JIRA hooks, no RTK hooks)
	preToolUse, ok := hooks["PreToolUse"].([]any)
	if !ok {
		t.Fatal("expected PreToolUse hooks (hook relay should be present)")
	}
	for _, entry := range preToolUse {
		entryMap := entry.(map[string]any)
		if matcher, ok := entryMap["matcher"].(string); ok && matcher == "Bash" {
			t.Error("expected no JIRA Bash-matcher hooks in PreToolUse when JIRA is disabled")
		}
		_ = entryMap
	}
}

func TestRunSetupClaudeDefaults_PlaywrightFallback(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	orig := lookPath
	t.Cleanup(func() { lookPath = orig })
	lookPath = func(name string) (string, error) {
		if name == "playwright-mcp" {
			return "/usr/bin/playwright-mcp", nil
		}
		return "", fmt.Errorf("not found: %s", name)
	}

	workspace := filepath.Join(tmpDir, "workspace")
	if err := os.MkdirAll(workspace, 0755); err != nil {
		t.Fatal(err)
	}

	if err := runSetupClaudeDefaults(workspace, "crew", false); err != nil {
		t.Fatalf("runSetupClaudeDefaults: %v", err)
	}

	// .mcp.json should be created with Playwright server.
	data, err := os.ReadFile(filepath.Join(workspace, ".mcp.json"))
	if err != nil {
		t.Fatalf("expected .mcp.json: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("parsing .mcp.json: %v", err)
	}

	servers, ok := result["mcpServers"].(map[string]any)
	if !ok {
		t.Fatal("expected mcpServers key")
	}
	if servers["playwright"] == nil {
		t.Error("expected playwright server in fallback .mcp.json")
	}
}

// readJSONFile reads and decodes a JSON object, failing the test on error.
func readJSONFile(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	return m
}

// TestRunSetupClaudeDefaults_LocalLeavesRepoSettingsAlone is the
// gd-wKXxD4qXp6 repro: a repo commits .claude/settings.json with its own
// PreCompact hooks, and `gb agent start --local` used to overwrite it with the
// agent's user-level settings. The committed file must come out byte-identical,
// and settings.local.json must keep the operator's own keys next to the
// agent's settings and hooks.
func TestRunSetupClaudeDefaults_LocalLeavesRepoSettingsAlone(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	orig := lookPath
	t.Cleanup(func() { lookPath = orig })
	lookPath = func(name string) (string, error) {
		return "", fmt.Errorf("not found: %s", name)
	}

	workspace := t.TempDir()
	claudeDir := filepath.Join(workspace, ".claude")
	if err := os.MkdirAll(claudeDir, 0755); err != nil {
		t.Fatal(err)
	}
	repoSettings := []byte(`{"hooks":{"PreCompact":[{"matcher":"","hooks":[{"type":"command","command":"gb hook load-state"}]}]}}` + "\n")
	repoPath := filepath.Join(claudeDir, "settings.json")
	if err := os.WriteFile(repoPath, repoSettings, 0644); err != nil {
		t.Fatal(err)
	}
	localPath := filepath.Join(claudeDir, "settings.local.json")
	if err := os.WriteFile(localPath, []byte(`{"operatorKey":"kept"}`), 0600); err != nil {
		t.Fatal(err)
	}

	if err := runSetupClaudeDefaults(workspace, "crew", true); err != nil {
		t.Fatalf("runSetupClaudeDefaults: %v", err)
	}

	got, err := os.ReadFile(repoPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(repoSettings) {
		t.Errorf("repo settings.json was rewritten:\n%s", got)
	}

	local := readJSONFile(t, localPath)
	if local["operatorKey"] != "kept" {
		t.Errorf("operator key dropped from settings.local.json: %v", local)
	}
	if local["alwaysThinkingEnabled"] != true {
		t.Errorf("user-level settings missing from settings.local.json: %v", local)
	}
	if hooks, ok := local["hooks"].(map[string]any); !ok || !hookContainsGB(hooks, "SessionStart") {
		t.Errorf("gb hooks missing from settings.local.json: %v", local["hooks"])
	}

	// A second run is idempotent: same keys, nothing duplicated.
	if err := runSetupClaudeDefaults(workspace, "crew", true); err != nil {
		t.Fatalf("second runSetupClaudeDefaults: %v", err)
	}
	if again := readJSONFile(t, localPath); fmt.Sprint(again) != fmt.Sprint(local) {
		t.Errorf("second local run changed settings.local.json:\nfirst:  %v\nsecond: %v", local, again)
	}
}

// TestMergeIntoSettingsFile_ReplacesUnparseable verifies a corrupt existing
// file is replaced rather than failing the setup.
func TestMergeIntoSettingsFile_ReplacesUnparseable(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".claude", "settings.local.json")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := mergeIntoSettingsFile(path, map[string]any{"model": "test"}); err != nil {
		t.Fatalf("mergeIntoSettingsFile: %v", err)
	}
	if got := readJSONFile(t, path); len(got) != 1 || got["model"] != "test" {
		t.Errorf("expected only model=test, got %v", got)
	}
}

// TestGitTracked distinguishes a committed file from an untracked one and
// from a directory outside any repo.
func TestGitTracked(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	tracked := filepath.Join(repo, "tracked.json")
	untracked := filepath.Join(repo, "untracked.json")
	for _, p := range []string{tracked, untracked} {
		if err := os.WriteFile(p, []byte("{}"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	run("add", "tracked.json")

	if !gitTracked(tracked) {
		t.Error("expected tracked.json to be tracked")
	}
	if gitTracked(untracked) {
		t.Error("expected untracked.json not to be tracked")
	}
	if gitTracked(filepath.Join(t.TempDir(), "x.json")) {
		t.Error("expected a path outside any repo not to be tracked")
	}
}

// TestWriteInstructionFiles_LocalTrackedClaudeMD covers gd-owzMVxfY1q: local
// mode must never overwrite a git-tracked CLAUDE.md. A tracked one is left
// alone and the instructions go to CLAUDE.local.md; an untracked or absent
// one is written as before, and the pod path still writes CLAUDE.md even when
// it is tracked.
func TestWriteInstructionFiles_LocalTrackedClaudeMD(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("HOME", t.TempDir())
	const repoMD = "# Repo instructions\n"
	const agentMD = "# Agent instructions\n"
	config := map[string]any{"claude_md": agentMD}

	newRepo := func(t *testing.T, claudeMD string, track ...string) string {
		t.Helper()
		repo := t.TempDir()
		run := func(args ...string) {
			t.Helper()
			cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v\n%s", args, err, out)
			}
		}
		run("init", "-q")
		if claudeMD != "" {
			if err := os.WriteFile(filepath.Join(repo, "CLAUDE.md"), []byte(claudeMD), 0644); err != nil {
				t.Fatal(err)
			}
		}
		for _, name := range track {
			if _, err := os.Stat(filepath.Join(repo, name)); os.IsNotExist(err) {
				if err := os.WriteFile(filepath.Join(repo, name), []byte(repoMD), 0644); err != nil {
					t.Fatal(err)
				}
			}
			run("add", name)
		}
		return repo
	}
	read := func(t *testing.T, path string) string {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		return string(data)
	}
	absent := func(t *testing.T, path string) {
		t.Helper()
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("expected no %s, got err=%v", path, err)
		}
	}

	t.Run("tracked", func(t *testing.T) {
		repo := newRepo(t, repoMD, "CLAUDE.md")
		writeInstructionFiles(repo, config, true)
		if got := read(t, filepath.Join(repo, "CLAUDE.md")); got != repoMD {
			t.Errorf("tracked CLAUDE.md was overwritten: %q", got)
		}
		if got := read(t, filepath.Join(repo, "CLAUDE.local.md")); got != agentMD {
			t.Errorf("CLAUDE.local.md = %q, want %q", got, agentMD)
		}
	})

	t.Run("untracked", func(t *testing.T) {
		repo := newRepo(t, repoMD)
		writeInstructionFiles(repo, config, true)
		if got := read(t, filepath.Join(repo, "CLAUDE.md")); got != agentMD {
			t.Errorf("CLAUDE.md = %q, want %q", got, agentMD)
		}
		absent(t, filepath.Join(repo, "CLAUDE.local.md"))
	})

	t.Run("absent", func(t *testing.T) {
		repo := newRepo(t, "")
		writeInstructionFiles(repo, config, true)
		if got := read(t, filepath.Join(repo, "CLAUDE.md")); got != agentMD {
			t.Errorf("CLAUDE.md = %q, want %q", got, agentMD)
		}
		absent(t, filepath.Join(repo, "CLAUDE.local.md"))
	})

	t.Run("tracked CLAUDE.local.md too", func(t *testing.T) {
		repo := newRepo(t, repoMD, "CLAUDE.md", "CLAUDE.local.md")
		writeInstructionFiles(repo, config, true)
		for _, name := range []string{"CLAUDE.md", "CLAUDE.local.md"} {
			if got := read(t, filepath.Join(repo, name)); got != repoMD {
				t.Errorf("tracked %s was overwritten: %q", name, got)
			}
		}
	})

	t.Run("pod mode still writes tracked CLAUDE.md", func(t *testing.T) {
		repo := newRepo(t, repoMD, "CLAUDE.md")
		writeInstructionFiles(repo, config, false)
		if got := read(t, filepath.Join(repo, "CLAUDE.md")); got != agentMD {
			t.Errorf("CLAUDE.md = %q, want %q", got, agentMD)
		}
		absent(t, filepath.Join(repo, "CLAUDE.local.md"))
	})
}
