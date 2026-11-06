package harness

import (
	"testing"
)

func TestFromEnv(t *testing.T) {
	cases := []struct {
		env, want string
		wantErr   bool
	}{
		{"", "claude", false},
		{"claude", "claude", false},
		{"codex", "codex", false},
		{"Codex", "", true},
		{"gemini", "gemini", false},
		{"Gemini", "", true},
		{"gpt", "", true},
	}
	for _, c := range cases {
		t.Setenv(EnvHarness, c.env)
		got, err := FromEnv()
		if (err != nil) != c.wantErr || got != c.want {
			t.Errorf("FromEnv(%q) = %q, %v; want %q, err=%v", c.env, got, err, c.want, c.wantErr)
		}
	}
	t.Setenv(EnvHarness, "bogus")
	if got := MustFromEnv(); got != "claude" {
		t.Errorf("MustFromEnv on bad value = %q, want claude", got)
	}
}

func TestDefaultCommandAndCoopAgent(t *testing.T) {
	if got := DefaultCommand("claude"); got != "claude --dangerously-skip-permissions" {
		t.Errorf("claude default = %q", got)
	}
	if got := DefaultCommand("codex"); got != "codex app-server" {
		t.Errorf("codex default = %q", got)
	}
	if got := DefaultCommand("gemini"); got != "gemini --acp" {
		t.Errorf("gemini default = %q", got)
	}
	cases := []struct{ h, cmd, want string }{
		{"claude", ClaudeCommand, "claude"},
		{"codex", CodexCommand, "codex"},
		// claudeless speaks Claude's protocol whatever the harness resolved.
		{"codex", "claudeless --scenario /s.toml", "claude"},
		{"gemini", GeminiCommand, "gemini"},
		{"gemini", "claudeless --scenario /s.toml", "claude"},
	}
	for _, c := range cases {
		if got := CoopAgent(c.h, c.cmd); got != c.want {
			t.Errorf("CoopAgent(%q, %q) = %q, want %q", c.h, c.cmd, got, c.want)
		}
	}
}

func TestModelEnv(t *testing.T) {
	if ModelEnv("claude") != "CLAUDE_MODEL" || ModelEnv("codex") != "CODEX_MODEL" || ModelEnv("gemini") != "GEMINI_MODEL" {
		t.Errorf("ModelEnv mismatch: %q %q %q", ModelEnv("claude"), ModelEnv("codex"), ModelEnv("gemini"))
	}
}

func TestWithModel(t *testing.T) {
	cases := []struct{ name, h, cmd, model, want string }{
		{"claude appends", "claude", ClaudeCommand, "opus", ClaudeCommand + " --model opus"},
		{"empty model", "claude", ClaudeCommand, "", ClaudeCommand},
		{"mock skips", "claude", "claudeless --x", "opus", "claudeless --x"},
		{"codex app-server", "codex", "codex app-server", "gpt-x", "codex -m gpt-x app-server"},
		{"codex tui", "codex", "codex --yolo", "gpt-x", "codex -m gpt-x --yolo"},
		{"codex exec", "codex", "codex exec --yolo", "gpt-x", "codex exec -m gpt-x --yolo"},
		{"codex already pinned", "codex", "codex -m other app-server", "gpt-x", "codex -m other app-server"},
		{"codex empty model", "codex", "codex app-server", "", "codex app-server"},
		{"codex mock skips", "codex", "claudeless --x", "gpt-x", "claudeless --x"},
		{"gemini acp", "gemini", GeminiCommand, "gemini-2.5-pro", "gemini --acp --model gemini-2.5-pro"},
		{"gemini already pinned -m", "gemini", "gemini --acp -m other", "gemini-2.5-pro", "gemini --acp -m other"},
		{"gemini already pinned --model=", "gemini", "gemini --acp --model=other", "gemini-2.5-pro", "gemini --acp --model=other"},
		{"gemini empty model", "gemini", GeminiCommand, "", GeminiCommand},
		{"gemini mock skips", "gemini", "claudeless --x", "gemini-2.5-pro", "claudeless --x"},
	}
	for _, c := range cases {
		if got := WithModel(c.h, c.cmd, c.model); got != c.want {
			t.Errorf("%s: WithModel = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestStateDir(t *testing.T) {
	t.Setenv("HOME", "/home/agent")
	t.Setenv("CODEX_HOME", "")
	if got := StateDir("claude", "/x/.claude"); got != "/x/.claude" {
		t.Errorf("claude StateDir = %q", got)
	}
	if got := StateDir("codex", "/x/.claude"); got != "/home/agent/.codex" {
		t.Errorf("codex StateDir = %q", got)
	}
	t.Setenv("CODEX_HOME", "/data/codex")
	if got := StateDir("codex", "/x/.claude"); got != "/data/codex" {
		t.Errorf("codex StateDir with CODEX_HOME = %q", got)
	}
	t.Setenv("GEMINI_CLI_HOME", "")
	if got := StateDir("gemini", "/x/.claude"); got != "/home/agent/.gemini" {
		t.Errorf("gemini StateDir = %q", got)
	}
	t.Setenv("GEMINI_CLI_HOME", "/data/gem")
	if got := StateDir("gemini", "/x/.claude"); got != "/data/gem/.gemini" {
		t.Errorf("gemini StateDir with GEMINI_CLI_HOME = %q", got)
	}
}

func TestIsDeniedEnv(t *testing.T) {
	denied := []string{"CODEX_UNSAFE=1", "OPENAI_API_KEY=sk-x", "OPENAI_BASE_URL=http://x", "CODEX_SQLITE_HOME=/x"}
	allowed := []string{"CODEX_HOME=/x", "CODEX_MODEL=m", "CODEX_CA_CERTIFICATE=/ca", "PATH=/bin", "CLAUDE_CODE_OAUTH_TOKEN=t"}
	for _, kv := range denied {
		if !IsDeniedEnv("codex", kv) {
			t.Errorf("codex: %q should be denied", kv)
		}
		if IsDeniedEnv("claude", kv) {
			t.Errorf("claude: %q must pass through untouched", kv)
		}
	}
	for _, kv := range allowed {
		if IsDeniedEnv("codex", kv) {
			t.Errorf("codex: %q should be allowed", kv)
		}
	}
}

func TestIsDeniedEnvGemini(t *testing.T) {
	denied := []string{
		"GEMINI_API_KEY=k", "GOOGLE_GEMINI_BASE_URL=http://x", "GOOGLE_API_KEY=k",
		"GOOGLE_APPLICATION_CREDENTIALS=/c.json", "GEMINI_SANDBOX=docker",
	}
	allowed := []string{
		"GEMINI_MODEL=m", "GEMINI_CLI_HOME=/x", "GEMINI_CLI_TRUST_WORKSPACE=true", "GEMINI_AUTH_MODE=oauth",
		"PATH=/bin", "OPENAI_API_KEY=sk-x", "CODEX_HOME=/x", "COOP_GEMINI_APPROVAL=deny",
	}
	for _, kv := range denied {
		if !IsDeniedEnv("gemini", kv) {
			t.Errorf("gemini: %q should be denied", kv)
		}
		for _, other := range []string{"claude", "codex"} {
			if IsDeniedEnv(other, kv) {
				t.Errorf("%s: %q must pass through untouched", other, kv)
			}
		}
	}
	for _, kv := range allowed {
		if IsDeniedEnv("gemini", kv) {
			t.Errorf("gemini: %q should be allowed", kv)
		}
	}
}

func TestClaudeDriver(t *testing.T) {
	cases := []struct {
		h, cmd string
		want   bool
	}{
		{"claude", ClaudeCommand, true},
		{"claude", "claudeless --scenario x", true},
		{"codex", CodexCommand, false},
		{"codex", "claudeless --scenario x", true}, // mock keeps the Claude driver
		{"gemini", GeminiCommand, false},
		{"gemini", "claudeless --scenario x", true},
	}
	for _, c := range cases {
		if got := ClaudeDriver(c.h, c.cmd); got != c.want {
			t.Errorf("ClaudeDriver(%q, %q) = %v, want %v", c.h, c.cmd, got, c.want)
		}
	}
}
