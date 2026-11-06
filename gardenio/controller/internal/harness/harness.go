// Package harness is the single source of truth for how a pod launches its
// agent CLI per harness (claude | codex, ADR 0004 §4): the default launch
// command, the coop --agent driver name, the model env var, the CLI state dir,
// and the per-harness env allowlist. gb init and gb agent start both read it so
// the two launch paths cannot drift.
package harness

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// EnvHarness is the pod env var the controller stamps for a non-default
// harness (cmd/controller EnvHarness; the two must stay equal).
const EnvHarness = "GARDENIO_HARNESS"

// Default launch commands. Codex runs as `codex app-server`, which coop drives
// over JSON-RPC (ADR 0004 §3); approval policy and sandbox are sent by coop on
// thread/start, so there is no --yolo here.
const (
	ClaudeCommand = "claude --dangerously-skip-permissions"
	CodexCommand  = "codex app-server"
	// GeminiCommand runs the Gemini CLI as an ACP agent (JSON-RPC over stdio),
	// which coop drives without a TUI (ADR 0005 §11.1). Approvals are answered
	// by coop over the protocol, so there is no --approval-mode yolo here.
	GeminiCommand = "gemini --acp"
)

// FromEnv returns the harness named by GARDENIO_HARNESS. Unset means claude; an
// unknown value is an error, never a silent fallback to claude (ADR 0004 §4).
func FromEnv() (string, error) {
	h, err := beadsapi.ParseHarness(os.Getenv(EnvHarness))
	if err != nil {
		return "", fmt.Errorf("%s: %w", EnvHarness, err)
	}
	if h == "" {
		h = beadsapi.DefaultHarness
	}
	return h, nil
}

// MustFromEnv is FromEnv for call sites that cannot return an error: gb init
// validates the env once at startup, so a bad value here (tests only) degrades
// to claude rather than panicking.
func MustFromEnv() string {
	h, err := FromEnv()
	if err != nil {
		return beadsapi.DefaultHarness
	}
	return h
}

// IsCodex reports whether h is the codex harness.
func IsCodex(h string) bool { return h == beadsapi.HarnessCodex }

// IsClaude reports whether h is the claude harness (the default).
func IsClaude(h string) bool { return h == beadsapi.HarnessClaude || h == "" }

// IsGemini reports whether h is the gemini harness.
func IsGemini(h string) bool { return h == beadsapi.HarnessGemini }

// DefaultCommand is the launch command used when BOAT_COMMAND is unset.
func DefaultCommand(h string) string {
	switch h {
	case beadsapi.HarnessCodex:
		return CodexCommand
	case beadsapi.HarnessGemini:
		return GeminiCommand
	}
	return ClaudeCommand
}

// IsMockCommand reports whether cmd runs the claudeless mock CLI. The mock
// speaks Claude's protocol whatever harness the pod resolved, so it keeps the
// Claude driver and skips the model flag.
func IsMockCommand(cmd string) bool { return strings.Contains(cmd, "claudeless") }

// CoopAgent is the value for coop's --agent flag.
func CoopAgent(h, cmd string) string {
	if IsMockCommand(cmd) {
		return beadsapi.HarnessClaude
	}
	switch h {
	case beadsapi.HarnessCodex, beadsapi.HarnessGemini:
		return h
	}
	return beadsapi.HarnessClaude
}

// ClaudeDriver reports whether coop drives the pod's agent with the Claude
// driver (TUI screen, startup prompts, login banner): every claude pod, plus a
// claudeless mock on a codex pod.
func ClaudeDriver(h, cmd string) bool { return CoopAgent(h, cmd) == beadsapi.HarnessClaude }

// ModelEnv names the env var carrying the resolved model for h.
func ModelEnv(h string) string {
	switch h {
	case beadsapi.HarnessCodex:
		return "CODEX_MODEL"
	case beadsapi.HarnessGemini:
		return "GEMINI_MODEL"
	}
	return "CLAUDE_MODEL"
}

// WithModel adds the model flag for h to cmd. A no-op for an empty model, a
// mock command, or a command that already pins a model. Claude appends
// `--model`; Codex takes `-m` as a root option placed right after the binary
// (`codex -m M app-server`), or after `exec` for the per-turn fallback; Gemini
// takes `--model` as a global option, appended unless the command already pins
// a model (`gemini --acp` becomes `gemini --acp --model M`).
func WithModel(h, cmd, model string) string {
	if model == "" || IsMockCommand(cmd) {
		return cmd
	}
	if IsGemini(h) {
		for _, f := range strings.Fields(cmd) {
			if f == "-m" || f == "--model" || strings.HasPrefix(f, "--model=") {
				return cmd
			}
		}
		return cmd + " --model " + model
	}
	if !IsCodex(h) {
		return cmd + " --model " + model
	}
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return cmd
	}
	for _, f := range fields {
		if f == "-m" || f == "--model" || strings.HasPrefix(f, "--model=") {
			return cmd
		}
	}
	at := 1
	for i, f := range fields[1:] {
		if f == "exec" || f == "e" {
			at = i + 2
			break
		}
		if !strings.HasPrefix(f, "-") {
			break
		}
	}
	out := append([]string{}, fields[:at]...)
	out = append(out, "-m", model)
	return strings.Join(append(out, fields[at:]...), " ")
}

// GeminiHome is the Gemini CLI's settings and session directory: the CLI
// relocates it with GEMINI_CLI_HOME (it then uses $GEMINI_CLI_HOME/.gemini),
// otherwise <home>/.gemini. home is the user's home directory.
func GeminiHome(home string) string {
	if base := os.Getenv("GEMINI_CLI_HOME"); base != "" {
		return filepath.Join(base, ".gemini")
	}
	return filepath.Join(home, ".gemini")
}

// codexEnvAllowlist names the CODEX_* / OPENAI_* env vars a codex pod's coop
// child may inherit. Everything else with those prefixes is stripped, the same
// reason as the CLAUDE_CODE_* allowlist: those vars are read directly by the
// CLI, so a stray pod-spec value would change behaviour with no trace. There is
// deliberately no OPENAI_API_KEY / OPENAI_BASE_URL: v1 authenticates with the
// ChatGPT-subscription auth.json (ADR 0004 §5), and an inherited API key would
// silently switch Codex to per-token billing.
var codexEnvAllowlist = map[string]struct{}{
	"CODEX_HOME":           {}, // per-pod writable state dir
	"CODEX_MODEL":          {}, // resolved model, set by the controller
	"CODEX_CA_CERTIFICATE": {}, // custom CA bundle for the ChatGPT backend
}

// geminiEnvAllowlist names the GEMINI_* / GOOGLE_* env vars a gemini pod's coop
// child may inherit. Everything else with those prefixes is stripped, for the
// reason ADR 0005 §5 C gives: the CLI reads them directly, so a stray pod-spec
// value would silently redirect requests (GOOGLE_GEMINI_BASE_URL), switch the
// auth path (GOOGLE_APPLICATION_CREDENTIALS, GOOGLE_API_KEY) or change the
// model. GEMINI_API_KEY is deliberately NOT here: the key arrives through
// coop's credential push, and an inherited key from a pod spec must not win.
var geminiEnvAllowlist = map[string]struct{}{
	"GEMINI_MODEL":               {}, // resolved model, set by the controller
	"GEMINI_CLI_HOME":            {}, // per-pod writable settings and session root
	"GEMINI_CLI_TRUST_WORKSPACE": {}, // clears the untrusted-directory gate (exit 55)
	"GEMINI_AUTH_MODE":           {}, // "oauth" = Google-account login, set by the controller
}

// IsDeniedEnv reports whether kv ("KEY=val") must be stripped from the coop
// env for harness h. Codex filters CODEX_*/OPENAI_*, gemini filters
// GEMINI_*/GOOGLE_*; claude pods keep their env byte-for-byte.
func IsDeniedEnv(h, kv string) bool {
	var allow map[string]struct{}
	var prefixes []string
	switch h {
	case beadsapi.HarnessCodex:
		allow, prefixes = codexEnvAllowlist, []string{"CODEX_", "OPENAI_"}
	case beadsapi.HarnessGemini:
		allow, prefixes = geminiEnvAllowlist, []string{"GEMINI_", "GOOGLE_"}
	default:
		return false
	}
	if !hasAnyPrefix(kv, prefixes) {
		return false
	}
	key, _, _ := strings.Cut(kv, "=")
	_, ok := allow[key]
	return !ok
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}
