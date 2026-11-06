package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func readClaudeJSON(t *testing.T, home string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		t.Fatalf("read .claude.json: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("unmarshal .claude.json: %v", err)
	}
	return cfg
}

func projectTrusted(t *testing.T, cfg map[string]any, dir string) bool {
	t.Helper()
	projects, ok := cfg["projects"].(map[string]any)
	if !ok {
		return false
	}
	entry, ok := projects[dir].(map[string]any)
	if !ok {
		return false
	}
	return entry["hasTrustDialogAccepted"] == true
}

// On a fresh pod ~/.claude.json does not exist; writeOnboardingSkip must create
// it with the global onboarding keys AND pre-trust every passed directory so
// Claude Code's per-project trust dialog never appears (ka-0000005485).
func TestWriteOnboardingSkipPreTrustsWorkspaceFromScratch(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	workdir := "/home/agent/bot/gardenio/work"
	scaffold := "/home/agent/workspace"
	writeOnboardingSkip(workdir, scaffold)

	cfg := readClaudeJSON(t, home)
	if cfg["hasCompletedOnboarding"] != true {
		t.Errorf("hasCompletedOnboarding not set: %v", cfg["hasCompletedOnboarding"])
	}
	if cfg["bypassPermissionsModeAccepted"] != true {
		t.Errorf("bypassPermissionsModeAccepted not set: %v", cfg["bypassPermissionsModeAccepted"])
	}
	if !projectTrusted(t, cfg, workdir) {
		t.Errorf("coop workdir %q not pre-trusted", workdir)
	}
	if !projectTrusted(t, cfg, scaffold) {
		t.Errorf("scaffold workspace %q not pre-trusted", scaffold)
	}
}

// On restart Claude Code may have already written ~/.claude.json with rich
// per-project state (allowedTools, mcpServers, etc.). writeOnboardingSkip must
// MERGE — not clobber — preserving existing keys while ensuring trust is set.
func TestWriteOnboardingSkipMergesExistingState(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	workdir := "/home/agent/bot/gardenio/work"
	existing := map[string]any{
		"userID":      "abc123",
		"preferredTheme": "light",
		"projects": map[string]any{
			workdir: map[string]any{
				"allowedTools":           []any{"Bash"},
				"hasTrustDialogAccepted": false,
			},
			"/some/other/dir": map[string]any{
				"allowedTools": []any{"Read"},
			},
		},
	}
	data, _ := json.Marshal(existing)
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), data, 0o600); err != nil {
		t.Fatalf("seed .claude.json: %v", err)
	}

	writeOnboardingSkip(workdir)

	cfg := readClaudeJSON(t, home)
	if cfg["userID"] != "abc123" {
		t.Errorf("userID clobbered: %v", cfg["userID"])
	}
	if !projectTrusted(t, cfg, workdir) {
		t.Errorf("workdir trust not flipped to true")
	}

	projects := cfg["projects"].(map[string]any)
	workEntry := projects[workdir].(map[string]any)
	if tools, ok := workEntry["allowedTools"].([]any); !ok || len(tools) != 1 || tools[0] != "Bash" {
		t.Errorf("existing allowedTools for workdir lost: %v", workEntry["allowedTools"])
	}
	if _, ok := projects["/some/other/dir"]; !ok {
		t.Errorf("unrelated project entry dropped")
	}
}

// A corrupt ~/.claude.json must not wedge startup: writeOnboardingSkip rewrites
// from scratch with trust set rather than propagating a parse error.
func TestWriteOnboardingSkipRecoversFromCorruptFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("seed corrupt .claude.json: %v", err)
	}

	workdir := "/home/agent/bot/gardenio/work"
	writeOnboardingSkip(workdir)

	cfg := readClaudeJSON(t, home)
	if cfg["hasCompletedOnboarding"] != true {
		t.Errorf("did not recover global keys from corrupt file")
	}
	if !projectTrusted(t, cfg, workdir) {
		t.Errorf("did not pre-trust workdir after corrupt-file recovery")
	}
}

// Empty trust dirs (defensive: unresolved workspace) must be skipped, not
// written as a "" project key.
func TestWriteOnboardingSkipSkipsEmptyDirs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	writeOnboardingSkip("", "/home/agent/workspace", "")

	cfg := readClaudeJSON(t, home)
	projects := cfg["projects"].(map[string]any)
	if _, ok := projects[""]; ok {
		t.Errorf("empty dir written as project key")
	}
	if !projectTrusted(t, cfg, "/home/agent/workspace") {
		t.Errorf("real dir not trusted")
	}
}

// lastOnboardingVersion must track the installed Claude Code version rather
// than a stale hardcoded string. The CLAUDE_CODE_VERSION env var is the
// preferred source so the stamped value follows the image (ka-0000005485).
func TestWriteOnboardingSkipStampsInstalledVersion(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CODE_VERSION", "9.9.99")

	writeOnboardingSkip("/home/agent/workspace")

	cfg := readClaudeJSON(t, home)
	if cfg["lastOnboardingVersion"] != "9.9.99" {
		t.Errorf("lastOnboardingVersion = %v, want 9.9.99 (from CLAUDE_CODE_VERSION)", cfg["lastOnboardingVersion"])
	}
}

func TestParseClaudeVersion(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"2.1.154 (Claude Code)", "2.1.154"},
		{"  2.1.37 (Claude Code)\n", "2.1.37"},
		{"1.0.0", "1.0.0"},
		{"", ""},
		{"Claude Code", ""},      // leading token has no dot
		{"v2.1.0 (foo)", ""},     // 'v' prefix is not all-digits-and-dots
		{"2.1.154-rc1 (x)", ""},  // suffix breaks the digit/dot invariant
	}
	for _, c := range cases {
		if got := parseClaudeVersion(c.in); got != c.want {
			t.Errorf("parseClaudeVersion(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// When neither the env var nor the binary resolves a version, the static floor
// is used so the field is never empty (which would re-arm onboarding UI).
func TestClaudeOnboardingVersionFallback(t *testing.T) {
	t.Setenv("CLAUDE_CODE_VERSION", "")
	t.Setenv("PATH", t.TempDir()) // no `claude` binary discoverable
	if got := claudeOnboardingVersion(); got != onboardingVersionFallback {
		t.Errorf("claudeOnboardingVersion() = %q, want fallback %q", got, onboardingVersionFallback)
	}
}
