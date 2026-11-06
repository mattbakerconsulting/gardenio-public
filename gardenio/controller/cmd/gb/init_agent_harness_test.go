package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestBuildAgentCommand_Harness(t *testing.T) {
	t.Setenv("BOAT_COMMAND", "")
	t.Setenv("GARDENIO_CHANNEL", "")
	t.Setenv("CLAUDE_MODEL", "claude-opus")
	t.Setenv("CODEX_MODEL", "gpt-x")

	t.Setenv("GARDENIO_HARNESS", "")
	if got := buildAgentCommand(); got != "claude --dangerously-skip-permissions --model claude-opus" {
		t.Errorf("claude default command = %q", got)
	}

	t.Setenv("GARDENIO_HARNESS", "codex")
	got := buildAgentCommand()
	if got != "codex -m gpt-x app-server" {
		t.Errorf("codex command = %q", got)
	}
	for _, claudeOnly := range []string{"--model", "--dangerously", "claude-opus"} {
		if strings.Contains(got, claudeOnly) {
			t.Errorf("codex command %q carries Claude-only %q", got, claudeOnly)
		}
	}

	// GARDENIO_CHANNEL is Claude-only: even when the channel script exists
	// (it does in agent images) the codex command must not grow the flag.
	t.Setenv("GARDENIO_CHANNEL", "1")
	if got := buildAgentCommand(); strings.Contains(got, "development-channels") {
		t.Errorf("codex command %q must ignore GARDENIO_CHANNEL", got)
	}

	// An explicit BOAT_COMMAND still wins over the table.
	t.Setenv("BOAT_COMMAND", "codex exec --yolo")
	if got := buildAgentCommand(); got != "codex exec -m gpt-x --yolo" {
		t.Errorf("codex exec override = %q", got)
	}
}

func TestBuildCoopEnv_CodexAllowlist(t *testing.T) {
	t.Setenv("KD_AGENT_ID", "")
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	t.Setenv("COOP_S3_BUCKET", "")
	t.Setenv("CODEX_HOME", "/home/agent/.codex")
	t.Setenv("CODEX_MODEL", "gpt-x")
	t.Setenv("CODEX_UNSAFE_FLAG", "1")
	t.Setenv("OPENAI_API_KEY", "sk-leak")
	t.Setenv("OPENAI_BASE_URL", "http://leak")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "tok")

	codex := buildCoopEnv(initConfig{role: "crew", harness: "codex"})
	for _, want := range []string{"CODEX_HOME=/home/agent/.codex", "CODEX_MODEL=gpt-x"} {
		if !envContains(codex, want) {
			t.Errorf("codex env missing %q", want)
		}
	}
	for _, key := range []string{"CODEX_UNSAFE_FLAG", "OPENAI_API_KEY", "OPENAI_BASE_URL"} {
		if envHasKey(codex, key) {
			t.Errorf("codex env must strip %s", key)
		}
	}

	// Claude pods keep their env: no CODEX_*/OPENAI_* filtering.
	claude := buildCoopEnv(initConfig{role: "crew", harness: "claude"})
	for _, key := range []string{"CODEX_UNSAFE_FLAG", "OPENAI_API_KEY"} {
		if !envHasKey(claude, key) {
			t.Errorf("claude env must keep %s", key)
		}
	}
}

func TestBuildInitConfig_HarnessFromEnv(t *testing.T) {
	t.Setenv("GARDENIO_HARNESS", "codex")
	if cfg := buildInitConfig(false, false); cfg.harness != "codex" || cfg.command != "codex app-server" {
		t.Errorf("cfg.harness=%q command=%q", cfg.harness, cfg.command)
	}
	t.Setenv("GARDENIO_HARNESS", "")
	if cfg := buildInitConfig(false, false); cfg.harness != "claude" {
		t.Errorf("default harness = %q", cfg.harness)
	}
}

func TestBypassStartupForHarness_CodexIsNoop(t *testing.T) {
	// A nil coop client would panic if bypassStartup ran; codex must skip it.
	if err := bypassStartupForHarness(t.Context(), nil, "codex", "codex app-server", false); err != nil {
		t.Fatalf("codex bypass should be a no-op, got %v", err)
	}
}

// A claudeless mock on a codex pod gets coop's Claude driver, so it still needs
// the Claude startup prompts answered: bypassStartup must run (a nil coop client
// panics if it does) and the login-banner screen check must not be skipped.
func TestBypassStartupForHarness_CodexPodWithClaudelessMockStillBypasses(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("bypassStartup was skipped for a claudeless mock on a codex pod")
		}
	}()
	_ = bypassStartupForHarness(t.Context(), nil, "codex", "claudeless --scenario x", false)
}

func TestRunAgentStartK8s_RejectsUnknownHarness(t *testing.T) {
	t.Setenv("GARDENIO_HARNESS", "codx")
	err := runAgentStartK8s(&cobra.Command{}, nil)
	if err == nil || !strings.Contains(err.Error(), "GARDENIO_HARNESS") {
		t.Fatalf("runAgentStartK8s with a typo'd harness = %v, want a GARDENIO_HARNESS error", err)
	}
}

func TestLoadSwitchCredentials_CodexPinReassertsCodexModel(t *testing.T) {
	t.Setenv("GARDENIO_HARNESS", "codex")
	t.Setenv("BOAT_MODEL_PINNED", "gpt-pinned")
	t.Setenv("CODEX_MODEL", "gpt-account-default")
	t.Setenv("CLAUDE_MODEL", "")
	data := `{"credentials":{"CODEX_MODEL":"gpt-account-default"}}`
	// credentialSwitchFile is a const, so write the real path (as the sibling
	// TestLoadSwitchCredentials_AppliesEnvVars does) and clean up.
	if err := os.WriteFile(credentialSwitchFile, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(credentialSwitchFile)

	loadSwitchCredentials()

	if got := os.Getenv("CODEX_MODEL"); got != "gpt-pinned" {
		t.Errorf("CODEX_MODEL = %q, want pinned", got)
	}
	if got := os.Getenv("CLAUDE_MODEL"); got != "" {
		t.Errorf("CLAUDE_MODEL = %q, a codex pin must not set Claude vars", got)
	}
}

// TestPrepareAgentPrompt_CodexInstallsAgentsMD pins desk item 2 on PR #400 and
// M2 of gd-kyGoabBaE5: the codex launch paths call prepareAgentPrompt, which
// installs $CODEX_HOME/AGENTS.md itself (not as a side effect of
// writeSystemPromptFile) and returns no --append-system-prompt-file flag.
func TestPrepareAgentPrompt_CodexInstallsAgentsMD(t *testing.T) {
	codexHome := filepath.Join(t.TempDir(), "podcodex")
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv(envGardenioHarness, "codex")
	codexConfigBeadServer(t, nil)
	old := runPrimeSystemPrompt
	runPrimeSystemPrompt = func() ([]byte, error) { return []byte("prime body\n"), nil }
	t.Cleanup(func() { runPrimeSystemPrompt = old })

	if path, ok := prepareAgentPrompt("codex", t.TempDir(), func(string, ...any) {}); ok || path != "" {
		t.Fatalf("codex must not get a prompt-file flag, got (%q, %v)", path, ok)
	}
	md, err := os.ReadFile(filepath.Join(codexHome, "AGENTS.md"))
	if err != nil {
		t.Fatalf("codex pod got no AGENTS.md: %v", err)
	}
	if !strings.Contains(string(md), "prime body") {
		t.Errorf("AGENTS.md = %q", md)
	}
}

// writeSystemPromptFile is Claude-only: it must not install AGENTS.md.
func TestWriteSystemPromptFile_DoesNotInstallAgentsMD(t *testing.T) {
	codexHome := filepath.Join(t.TempDir(), "podcodex")
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv(envGardenioHarness, "codex")
	old := runPrimeSystemPrompt
	runPrimeSystemPrompt = func() ([]byte, error) { return []byte("prime body\n"), nil }
	t.Cleanup(func() { runPrimeSystemPrompt = old })

	if _, ok := writeSystemPromptFile(t.TempDir(), func(string, ...any) {}); !ok {
		t.Fatal("writeSystemPromptFile failed")
	}
	if _, err := os.Stat(filepath.Join(codexHome, "AGENTS.md")); err == nil {
		t.Error("writeSystemPromptFile installed AGENTS.md")
	}
}

func TestIsCodexHarness_UsesHarnessPackage(t *testing.T) {
	for env, want := range map[string]bool{"": false, "claude": false, "codex": true, "bogus": false} {
		t.Setenv(envGardenioHarness, env)
		if got := isCodexHarness(); got != want {
			t.Errorf("GARDENIO_HARNESS=%q: isCodexHarness = %v, want %v", env, got, want)
		}
	}
}
