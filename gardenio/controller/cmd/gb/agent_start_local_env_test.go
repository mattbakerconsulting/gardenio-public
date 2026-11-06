package main

import (
	"strings"
	"testing"
)

// gd-6Nrazv7Waz: an operator launching from inside Claude Code must not hand
// that session's identity to the local agent, or the agent's claude registers
// as a child of the operator's session.
func TestLocalAgentEnv_ScrubsLaunchingClaudeSession(t *testing.T) {
	scrubbed := map[string]string{
		"CLAUDECODE":                    "1",
		"AI_AGENT":                      "claude-code",
		"CLAUDE_CODE_SESSION_ID":        "600ae862-parent",
		"CLAUDE_CODE_MESSAGING_SOCKET":  "/tmp/claude-1000/parent.sock",
		"CLAUDE_CODE_BRIDGE_SESSION_ID": "bridge-parent",
		"CLAUDE_CODE_CHILD_SESSION":     "1",
		"CLAUDE_CODE_ENTRYPOINT":        "cli",
		"CLAUDE_CODE_SSE_PORT":          "41234",
	}
	kept := map[string]string{
		"PATH":                    "/usr/bin:/bin",
		"HOME":                    "/home/operator",
		"TMUX":                    "/tmp/tmux-1000/default,1,0",
		"CLAUDE_CONFIG_DIR":       "/home/operator/.claude",
		"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat-operator",
		"CLAUDE_CODE_USE_BEDROCK": "1",
	}
	for k, v := range scrubbed {
		t.Setenv(k, v)
	}
	for k, v := range kept {
		t.Setenv(k, v)
	}

	env := localAgentEnv("agent", "gd-bead", "crew", "/tmp", "http://127.0.0.1:8080", "", localCreds{HTTPURL: "http://localhost:8080"})

	for _, e := range env {
		key, _, _ := strings.Cut(e, "=")
		if _, bad := scrubbed[key]; bad {
			t.Errorf("child env carries launching-session var %s", e)
		}
	}
	for k, want := range kept {
		if got := findEnv(env, k); got != want {
			t.Errorf("%s = %q, want %q kept", k, got, want)
		}
	}
	if got := findEnv(env, "KD_ACTOR"); got != "agent" {
		t.Errorf("KD_ACTOR = %q, want agent override still applied", got)
	}
}
