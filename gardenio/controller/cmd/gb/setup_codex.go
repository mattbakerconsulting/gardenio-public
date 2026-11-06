package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/spf13/cobra"

	"gardenio/controller/internal/harness"
)

// Codex harness config injection (ADR 0004 §E, task gd-kmduHGcH5z).
//
// A harness=codex pod gets the same three things a Claude pod gets, written in
// Codex's formats:
//
//	~/.claude/settings.json + .mcp.json   →  $CODEX_HOME/config.toml
//	--append-system-prompt-file           →  $CODEX_HOME/AGENTS.md
//	<ws>/CLAUDE.md                        →  unchanged (read via project_doc_fallback_filenames)
//
// The .mcp.json servers come from finalizeMCPServers, the same function that
// builds the Claude file, so the harnesses cannot drift. Hook wiring
// ($CODEX_HOME/hooks.json and the seeded [hooks.state] trust, ADR §E.1) lives in
// setup_codex_hooks.go and runs AFTER writeCodexConfig, because that writer
// regenerates config.toml wholesale.

const (
	// envGardenioHarness is set by the controller on codex pods
	// (cmd/controller.EnvHarness). Unset or anything else means claude.
	envGardenioHarness = "GARDENIO_HARNESS"

	envCodexHome            = "CODEX_HOME"
	envCodexModel           = "CODEX_MODEL"
	envCodexReasoningEffort = "CODEX_REASONING_EFFORT"
	// envCodexLiteLLMURL opts a pod into the LiteLLM-routed provider. The v1
	// default is ChatGPT-subscription auth straight to OpenAI (ADR §C), so the
	// provider block is only written for a future metered API-key account.
	envCodexLiteLLMURL  = "CODEX_LITELLM_URL"
	envCodexLiteLLMTags = "CODEX_LITELLM_TAGS"

	// codexProjectDocMaxBytes is the AGENTS.md/CLAUDE.md byte budget. Codex's
	// default (32 KiB) truncates this repo's CLAUDE.md plus nested files; 256 KiB
	// leaves headroom without letting a runaway file eat the context window.
	codexProjectDocMaxBytes = 262144

	codexConfigFile = "config.toml"
	codexAgentsFile = "AGENTS.md"
)

// codexReasoningEfforts are the model_reasoning_effort values Codex accepts.
var codexReasoningEfforts = map[string]bool{
	"minimal": true, "low": true, "medium": true, "high": true, "xhigh": true,
}

// isCodexHarness reports whether this process is materializing config for
// Codex. It reads internal/harness so it agrees with gb init / gb agent start.
func isCodexHarness() bool { return harness.IsCodex(harness.MustFromEnv()) }

// codexHomePath resolves where config.toml and AGENTS.md go: <ws>/.codex in
// local mode (never touching the operator's ~/.codex, like .claude/ in
// writeUserSettings), else $CODEX_HOME, else ~/.codex.
func codexHomePath(workspace string, local bool) (string, error) {
	if local {
		if workspace == "" {
			return "", fmt.Errorf("local mode requires non-empty workspace")
		}
		return filepath.Join(workspace, ".codex"), nil
	}
	if h := os.Getenv(envCodexHome); h != "" {
		return h, nil
	}
	return filepath.Join(homeDir(), ".codex"), nil
}

// codexConfigInput is everything buildCodexConfig needs; keeping it a plain
// struct (no env reads inside the builder) is what makes the golden test exact.
type codexConfigInput struct {
	Workspace       string
	Model           string
	ReasoningEffort string
	LiteLLMURL      string
	LiteLLMTags     string
	// MCPServers is the finalized Claude-shaped server map (finalizeMCPServers),
	// with ${VAR} placeholders still unexpanded.
	MCPServers map[string]any
	// Fragment is the resolved codex-config bead, merged over the defaults.
	Fragment map[string]any
}

// codexConfigInputFromEnv fills the env-sourced fields.
func codexConfigInputFromEnv(workspace string) codexConfigInput {
	return codexConfigInput{
		Workspace:       workspace,
		Model:           strings.TrimSpace(os.Getenv(envCodexModel)),
		ReasoningEffort: strings.ToLower(strings.TrimSpace(os.Getenv(envCodexReasoningEffort))),
		LiteLLMURL:      strings.TrimRight(strings.TrimSpace(os.Getenv(envCodexLiteLLMURL)), "/"),
		LiteLLMTags:     strings.TrimSpace(os.Getenv(envCodexLiteLLMTags)),
	}
}

// buildCodexConfig assembles the config.toml document as a generic map. The
// returned warnings are non-fatal (an MCP server Codex cannot express, a bad
// codex-config fragment); the caller logs them.
func buildCodexConfig(in codexConfigInput) (map[string]any, []string) {
	var warnings []string
	cfg := map[string]any{
		"approval_policy":                "never",
		"sandbox_mode":                   "danger-full-access",
		"project_doc_fallback_filenames": []string{"CLAUDE.md"},
		"project_doc_max_bytes":          int64(codexProjectDocMaxBytes),
		"check_for_update_on_startup":    false,
		"history":                        map[string]any{"persistence": "save-all"},
		"features":                       map[string]any{"hooks": true},
		"shell_environment_policy":       map[string]any{"inherit": "all"},
	}
	if in.Workspace != "" {
		cfg["projects"] = map[string]any{
			in.Workspace: map[string]any{"trust_level": "trusted"},
		}
	}
	if in.Model != "" {
		cfg["model"] = in.Model
	}
	if in.ReasoningEffort != "" {
		if codexReasoningEfforts[in.ReasoningEffort] {
			cfg["model_reasoning_effort"] = in.ReasoningEffort
		} else {
			warnings = append(warnings, fmt.Sprintf("ignoring unknown %s=%q", envCodexReasoningEffort, in.ReasoningEffort))
		}
	}
	if in.LiteLLMURL != "" {
		provider := map[string]any{
			"name":     "LiteLLM",
			"base_url": in.LiteLLMURL + "/v1",
			"env_key":  "LITELLM_API_KEY",
			// Codex only speaks the Responses API for custom providers.
			"wire_api": "responses",
		}
		if in.LiteLLMTags != "" {
			// The spend reconciler (reconcile_session_spend.go) keys on this header.
			provider["http_headers"] = map[string]any{"x-litellm-tags": in.LiteLLMTags}
		}
		cfg["model_provider"] = "litellm"
		cfg["model_providers"] = map[string]any{"litellm": provider}
	}

	mcp, mcpWarnings := translateMCPServersForCodex(in.MCPServers)
	warnings = append(warnings, mcpWarnings...)
	if len(mcp) > 0 {
		cfg["mcp_servers"] = mcp
	}

	frag, fragWarnings := codexFragmentToMap(in.Fragment)
	warnings = append(warnings, fragWarnings...)
	deepMergeInto(cfg, frag)
	return cfg, warnings
}

// bearerHeaderPattern matches an Authorization header whose whole value is
// "Bearer ${VAR}", which Codex expresses as bearer_token_env_var so the token
// never lands in config.toml.
var bearerHeaderPattern = regexp.MustCompile(`^Bearer \$\{([A-Za-z_][A-Za-z0-9_]*)\}$`)

// translateMCPServersForCodex maps Claude-shaped mcpServers entries to Codex
// [mcp_servers.<name>] tables: stdio (command/args/env) and streamable-HTTP
// (url + bearer_token_env_var / http_headers). Servers Codex cannot express
// (SSE, malformed) are dropped with a warning. gardenio-channel is dropped
// silently: nudges reach Codex through the driver, not a channel (ADR §E).
func translateMCPServersForCodex(servers map[string]any) (map[string]any, []string) {
	var warnings []string
	out := map[string]any{}
	names := make([]string, 0, len(servers))
	for n := range servers {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, name := range names {
		if name == "gardenio-channel" {
			continue
		}
		src, ok := servers[name].(map[string]any)
		if !ok {
			warnings = append(warnings, fmt.Sprintf("mcp server %q: not an object, skipped", name))
			continue
		}
		typ, _ := src["type"].(string)
		command, _ := src["command"].(string)
		url, _ := src["url"].(string)

		switch {
		case command != "" && (typ == "" || typ == "stdio"):
			dst := map[string]any{"command": expandEnvVars(command)}
			if args := stringList(src["args"]); len(args) > 0 {
				for i := range args {
					args[i] = expandEnvVars(args[i])
				}
				dst["args"] = args
			}
			if env := stringMap(src["env"]); len(env) > 0 {
				for k, v := range env {
					env[k] = expandEnvVars(v)
				}
				dst["env"] = env
			}
			out[name] = dst
		case url != "" && (typ == "" || typ == "http" || typ == "streamable-http"):
			dst := map[string]any{"url": expandEnvVars(url)}
			headers := map[string]any{}
			for k, v := range stringMap(src["headers"]) {
				if m := bearerHeaderPattern.FindStringSubmatch(v); m != nil && strings.EqualFold(k, "authorization") {
					dst["bearer_token_env_var"] = m[1]
					continue
				}
				headers[k] = expandEnvVars(v)
			}
			if len(headers) > 0 {
				dst["http_headers"] = headers
			}
			out[name] = dst
		default:
			warnings = append(warnings, fmt.Sprintf("mcp server %q (type %q): Codex has no equivalent, skipped", name, typ))
		}
	}
	return out, warnings
}

func stringList(v any) []string {
	var out []string
	switch l := v.(type) {
	case []any:
		for _, e := range l {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
	case []string:
		out = append(out, l...)
	}
	return out
}

func stringMap(v any) map[string]string {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, e := range m {
		if s, ok := e.(string); ok {
			out[k] = s
		} else if e != nil {
			out[k] = fmt.Sprint(e)
		}
	}
	return out
}

// codexFragmentToMap turns the resolved codex-config bead into config.toml
// keys. Config beads store JSON, so every key except "toml" is taken as a
// config.toml key as-is; the optional "toml" string key carries a raw TOML
// fragment and is applied on top of them.
func codexFragmentToMap(fragment map[string]any) (map[string]any, []string) {
	if len(fragment) == 0 {
		return nil, nil
	}
	var warnings []string
	out := map[string]any{}
	for k, v := range fragment {
		if k == "toml" {
			continue
		}
		out[k] = normalizeJSONNumbers(v)
	}
	if raw, ok := fragment["toml"].(string); ok && strings.TrimSpace(raw) != "" {
		var parsed map[string]any
		if _, err := toml.Decode(raw, &parsed); err != nil {
			warnings = append(warnings, fmt.Sprintf("codex-config toml fragment is not valid TOML, ignored: %v", err))
		} else {
			deepMergeInto(out, parsed)
		}
	}
	for _, k := range stripDeniedCodexKeys(out) {
		warnings = append(warnings, fmt.Sprintf("codex-config key %q is not allowed in a config bead (it runs a program or redirects requests), stripped", k))
	}
	return out, warnings
}

// codexDeniedKeys are the top-level config.toml keys a codex-config bead may
// not set. Any agent's shared token can write config beads, so the fragment is
// untrusted: these keys either make Codex run a program (notify, hooks,
// mcp_servers.*.command, profile-selected variants of the same), redirect
// requests or credentials (model_provider(s), oss_provider, *_base_url,
// experimental_realtime_ws_base_url, mcp_oauth_callback_url, otel exporters,
// mcp_servers.*.url), load code from elsewhere (plugins, marketplaces), or widen what a child process inherits
// (shell_environment_policy). mcp_servers is denied whole, which subsumes
// stripping its per-server command/args/env/cwd/url/headers keys. gardenio generates mcp_servers and the LiteLLM
// provider itself, from trusted sources, before the fragment is merged.
var codexDeniedKeys = map[string]bool{
	"model_provider":                    true,
	"model_providers":                   true,
	"oss_provider":                      true,
	"chatgpt_base_url":                  true,
	"openai_base_url":                   true,
	"experimental_realtime_ws_base_url": true,
	"mcp_oauth_callback_url":            true,
	"plugins":                           true,
	"marketplaces":                      true,
	"notify":                            true,
	"mcp_servers":                       true,
	"hooks":                             true,
	"profile":                           true,
	"profiles":                          true,
	"shell_environment_policy":          true,
	"otel":                              true,
	"experimental_instructions_file":    true,
	"model_instructions_file":           true,
	"cli_auth_credentials_store":        true,
	"experimental_bearer_token":         true,
}

// stripDeniedCodexKeys deletes codexDeniedKeys from fragment (top level, where
// Codex reads them) and returns the stripped keys, sorted, for logging.
func stripDeniedCodexKeys(fragment map[string]any) []string {
	var stripped []string
	for k := range fragment {
		if codexDeniedKeys[k] {
			stripped = append(stripped, k)
			delete(fragment, k)
		}
	}
	sort.Strings(stripped)
	return stripped
}

// normalizeJSONNumbers converts whole-number float64s (how encoding/json
// decodes every number) to int64 so the TOML encoder writes 4096, not 4096.0.
func normalizeJSONNumbers(v any) any {
	switch t := v.(type) {
	case float64:
		if t == float64(int64(t)) {
			return int64(t)
		}
		return t
	case map[string]any:
		for k, e := range t {
			t[k] = normalizeJSONNumbers(e)
		}
		return t
	case []any:
		for i, e := range t {
			t[i] = normalizeJSONNumbers(e)
		}
		return t
	}
	return v
}

// deepMergeInto merges src over dst: maps merge key by key, everything else
// (scalars, arrays) in src replaces dst.
func deepMergeInto(dst, src map[string]any) {
	for k, sv := range src {
		if sm, ok := sv.(map[string]any); ok {
			if dm, ok := dst[k].(map[string]any); ok {
				deepMergeInto(dm, sm)
				continue
			}
		}
		dst[k] = sv
	}
}

// renderCodexTOML encodes the config. BurntSushi/toml writes plain keys before
// sub-tables and sorts both, which keeps top-level keys such as
// project_doc_fallback_filenames ahead of every [table] (a TOML requirement)
// and the output deterministic for the golden test.
func renderCodexTOML(cfg map[string]any) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString("# Generated by `gb setup codex` at pod start. Edit the codex-config config bead, not this file.\n")
	enc := toml.NewEncoder(&buf)
	enc.Indent = ""
	if err := enc.Encode(cfg); err != nil {
		return nil, fmt.Errorf("encoding codex config.toml: %w", err)
	}
	return buf.Bytes(), nil
}

// writeHarnessFileAtomic writes via a temp file and rename so a Codex or Gemini process starting
// mid-write never reads a torn config.
func writeHarnessFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// writeCodexConfig renders and writes <codexHome>/config.toml (0600: MCP env
// values may carry expanded secrets).
func writeCodexConfig(codexHome string, in codexConfigInput) error {
	cfg, warnings := buildCodexConfig(in)
	for _, w := range warnings {
		fmt.Fprintf(os.Stderr, "[setup] warning: codex config: %s\n", w)
	}
	data, err := renderCodexTOML(cfg)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(codexHome, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", codexHome, err)
	}
	path := filepath.Join(codexHome, codexConfigFile)
	if err := writeHarnessFileAtomic(path, data, 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	fmt.Fprintf(os.Stderr, "[setup] wrote codex config to %s\n", path)
	return nil
}

// composeAgentsMD is the AGENTS.md body: the `gb prime --system-prompt` output
// (the same content Claude gets via --append-system-prompt-file) followed by
// the optional codex-instructions agents_md section.
func composeAgentsMD(prime []byte, extra string) []byte {
	out := bytes.TrimRight(prime, "\n")
	extra = strings.TrimSpace(extra)
	if extra != "" {
		out = append(append(out, '\n', '\n'), extra...)
	}
	return append(out, '\n')
}

// writeCodexAgentsMD writes <codexHome>/AGENTS.md.
func writeCodexAgentsMD(codexHome string, prime []byte, extra string) error {
	if err := os.MkdirAll(codexHome, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", codexHome, err)
	}
	path := filepath.Join(codexHome, codexAgentsFile)
	if err := writeHarnessFileAtomic(path, composeAgentsMD(prime, extra), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	fmt.Fprintf(os.Stderr, "[setup] wrote %s\n", path)
	return nil
}

// codexInstructionsExtra resolves the codex-instructions config bead and
// returns its agents_md text. Best-effort: a daemon failure yields "" so a
// Codex agent still boots with the plain prime output.
func codexInstructionsExtra(ctx context.Context, role string) string {
	cfg, _, err := resolveConfigBeadsWithRetry(ctx, daemon, "codex-instructions", buildSubscriptions(role))
	if err != nil {
		fmt.Fprintf(os.Stderr, "[setup] warning: resolving codex-instructions: %v\n", err)
		return ""
	}
	s, _ := cfg["agents_md"].(string)
	return s
}

// runPrimeSystemPrompt runs `gb prime --system-prompt` and returns its output.
// A var so tests can stub the subprocess.
var runPrimeSystemPrompt = func() ([]byte, error) {
	gbBin, err := exec.LookPath("gb")
	if err != nil {
		gbBin = currentBinary()
	}
	cmd := exec.Command(gbBin, "prime", "--system-prompt")
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = io.Discard
	cmd.Env = os.Environ()
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("gb prime --system-prompt: %w", err)
	}
	if buf.Len() == 0 {
		return nil, fmt.Errorf("gb prime --system-prompt produced empty output")
	}
	return buf.Bytes(), nil
}

// installCodexAgentsMDFromPrime runs `gb prime --system-prompt` and installs the
// result as AGENTS.md. Best-effort: a failed prime is logged and the agent still
// launches (without refreshed advice).
func installCodexAgentsMDFromPrime(logf func(string, ...any)) {
	prime, err := runPrimeSystemPrompt()
	if err != nil {
		logf("warning: codex AGENTS.md: %v", err)
		return
	}
	installCodexAgentsMD(prime, logf)
}

// installCodexAgentsMD is the Codex counterpart of passing the system-prompt
// file to --append-system-prompt-file: it writes AGENTS.md under CODEX_HOME.
// Called (via prepareAgentPrompt) on every coop session start so the file
// tracks the agent's current advice/identity. Best-effort; failure is logged.
func installCodexAgentsMD(prime []byte, logf func(string, ...any)) {
	home, err := codexHomePath("", false)
	if err != nil {
		logf("warning: codex AGENTS.md: %v", err)
		return
	}
	extra := codexInstructionsExtra(context.Background(), os.Getenv("BOAT_ROLE"))
	if err := writeCodexAgentsMD(home, prime, extra); err != nil {
		logf("warning: codex AGENTS.md: %v", err)
	}
}

// writeClaudeMD writes <workspace>/CLAUDE.md from the claude-instructions
// config, so a Codex or Gemini agent (which reads it via
// project_doc_fallback_filenames / context.fileName) sees the same
// instructions file as a Claude agent. A missing claude_md key leaves any repo
// CLAUDE.md alone.
//
// In local mode the workspace is the operator's own checkout, so a git-tracked
// CLAUDE.md is left alone (gd-ySqO7lAPoV). Codex and Gemini do not read
// CLAUDE.local.md, so instead of the Claude redirect (instructionsPath) the
// claude_md text is returned for the caller to fold into the local
// instructions file it writes, named by foldInto. Otherwise it returns "".
func writeClaudeMD(workspace string, config map[string]any, local bool, foldInto string) string {
	claudeMD, ok := config["claude_md"].(string)
	if !ok || claudeMD == "" {
		return ""
	}
	path := filepath.Join(workspace, "CLAUDE.md")
	if local && gitTracked(path) {
		fmt.Fprintf(os.Stderr, "[setup] %s is tracked by git, so agent instructions go to %s instead\n", path, foldInto)
		return claudeMD
	}
	if err := os.WriteFile(path, []byte(claudeMD), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "[setup] warning: failed to write CLAUDE.md: %v\n", err)
		return ""
	}
	fmt.Fprintf(os.Stderr, "[setup] wrote %s from claude-instructions config\n", path)
	return ""
}

// joinInstructions joins non-empty instruction sections with a blank line.
func joinInstructions(sections ...string) string {
	var out []string
	for _, s := range sections {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return strings.Join(out, "\n\n")
}

var setupCodexCmd = &cobra.Command{
	Use:   "codex",
	Short: "Materialize Codex config.toml and AGENTS.md from config beads",
	Long: `Writes the Codex counterpart of "gb setup claude" for harness=codex agents:

  $CODEX_HOME/config.toml — model, approval/sandbox policy, workspace trust,
                            CLAUDE.md fallback, mcp_servers.* (from the same
                            claude-mcp source as .mcp.json), plus the merged
                            codex-config bead
  $CODEX_HOME/AGENTS.md   — gb prime --system-prompt output plus the merged
                            codex-instructions agents_md text
  {workspace}/CLAUDE.md   — from claude-instructions, shared with Claude agents

CODEX_HOME defaults to ~/.codex. With --local everything is written under
{workspace}/.codex/ instead and the operator's ~/.codex is never touched; a
git-tracked {workspace}/CLAUDE.md is left alone and the claude-instructions
text goes into that AGENTS.md instead.

Model and effort come from the pod env: CODEX_MODEL, CODEX_REASONING_EFFORT.
CODEX_LITELLM_URL (+ CODEX_LITELLM_TAGS) opts into a LiteLLM-routed provider;
the default is ChatGPT-subscription auth direct to OpenAI.

Config bead keys (global → role, later overrides earlier):
  codex-config:<scope>        — JSON keys deep-merged into config.toml; an
                                optional "toml" string key holds a raw fragment
  codex-instructions:<scope>  — {"agents_md": "..."} appended to AGENTS.md

Flags:
  --defaults   No daemon: write the built-in config only`,
	RunE: func(cmd *cobra.Command, args []string) error {
		workspace, _ := cmd.Flags().GetString("workspace")
		if workspace == "" {
			workspace, _ = os.Getwd()
		}
		role, _ := cmd.Flags().GetString("role")
		if role == "" {
			role = os.Getenv("BOAT_ROLE")
		}
		if role == "" {
			role = os.Getenv("KD_ROLE")
		}
		local, _ := cmd.Flags().GetBool("local")
		defaults, _ := cmd.Flags().GetBool("defaults")
		if defaults {
			return runSetupCodexDefaults(workspace, role, local)
		}
		return runSetupCodex(cmd.Context(), workspace, role, local)
	},
}

func init() {
	setupCodexCmd.Flags().String("workspace", os.Getenv("KD_WORKSPACE"), "workspace directory")
	setupCodexCmd.Flags().String("role", "", "agent role (default: $BOAT_ROLE or $KD_ROLE)")
	setupCodexCmd.Flags().Bool("defaults", false, "write built-in config only (no daemon needed)")
	setupCodexCmd.Flags().Bool("local", false, "local mode: confine writes to <workspace>/.codex/ (never touches ~/.codex/)")
	setupCmd.AddCommand(setupCodexCmd)
}

// runSetupCodexDefaults writes the built-in config without a daemon: the
// fallback when config beads are unreachable, mirroring runSetupClaudeDefaults.
func runSetupCodexDefaults(workspace, role string, local bool) error {
	home, err := codexHomePath(workspace, local)
	if err != nil {
		return err
	}
	in := codexConfigInputFromEnv(workspace)
	if mcp := defaultMCPConfig(); mcp != nil {
		in.MCPServers = finalizeMCPServers(workspace, map[string]any{}, mcp)
	}
	if err := writeCodexConfig(home, in); err != nil {
		return err
	}
	// After config.toml: the trust seeding rewrites it (see setup_codex_hooks.go).
	logCodexHooksInstall(context.Background(), home, workspace, harnessHooksSource(nil))
	// No daemon, so no role toggle: fail open to every repo skill, as
	// fetchRoleExtensionFilters does when it cannot read the role beads.
	symlinkCodexSkills(home, extensionFilter{})
	return writeCodexLocalAgentsMD(home, role, local, "")
}

// runSetupCodex resolves config beads and writes config.toml (and, in local
// mode, AGENTS.md) plus the shared CLAUDE.md. In pods AGENTS.md is written at
// coop launch by installCodexAgentsMD, once the prime output is current.
func runSetupCodex(ctx context.Context, workspace, role string, local bool) error {
	home, err := codexHomePath(workspace, local)
	if err != nil {
		return err
	}
	subs := buildSubscriptions(role)

	// Same bounded-retry resolution as runSetupClaude: a transient daemon blip
	// must not fail the SessionStart boot path.
	mcpConfig, _, err := resolveConfigBeadsWithRetry(ctx, daemon, "claude-mcp", subs)
	if err != nil {
		return fmt.Errorf("resolving claude-mcp (daemon unreachable?): %w", err)
	}
	if mcpConfig == nil {
		mcpConfig = defaultMCPConfig()
	}
	fragment, _, err := resolveConfigBeadsWithRetry(ctx, daemon, "codex-config", subs)
	if err != nil {
		return fmt.Errorf("resolving codex-config (daemon unreachable?): %w", err)
	}
	instructions, _, err := resolveConfigBeadsWithRetry(ctx, daemon, "claude-instructions", subs)
	if err != nil {
		return fmt.Errorf("resolving claude-instructions (daemon unreachable?): %w", err)
	}
	codexInstr, _, err := resolveConfigBeadsWithRetry(ctx, daemon, "codex-instructions", subs)
	if err != nil {
		return fmt.Errorf("resolving codex-instructions (daemon unreachable?): %w", err)
	}
	// Hooks come from the same claude-hooks bead as Claude agents. A missing
	// bead is not an error here: the built-in defaults apply.
	hooksConfig, _, err := resolveConfigBeadsWithRetry(ctx, daemon, "claude-hooks", subs)
	if err != nil {
		return fmt.Errorf("resolving claude-hooks (daemon unreachable?): %w", err)
	}

	in := codexConfigInputFromEnv(workspace)
	in.Fragment = fragment
	if mcpConfig != nil {
		in.MCPServers = finalizeMCPServers(workspace, map[string]any{}, mcpConfig)
	}
	if err := writeCodexConfig(home, in); err != nil {
		return err
	}
	// After config.toml: the trust seeding rewrites it (see setup_codex_hooks.go).
	logCodexHooksInstall(ctx, home, workspace, harnessHooksSource(hooksConfig))
	// Same role skill toggle the Claude path applies (docs/agent-skill-toggle.md).
	symlinkCodexSkills(home, fetchRoleExtensionFilters(ctx, daemon, role).skills)
	folded := writeClaudeMD(workspace, instructions, local, codexAgentsFile)
	extra, _ := codexInstr["agents_md"].(string)
	return writeCodexLocalAgentsMD(home, role, local, joinInstructions(folded, extra))
}

// writeCodexLocalAgentsMD writes AGENTS.md at setup time for local mode, where
// no coop launch follows. In pod mode it is a no-op (see runSetupCodex).
func writeCodexLocalAgentsMD(home, role string, local bool, extra string) error {
	if !local {
		return nil
	}
	prime, err := runPrimeSystemPrompt()
	if err != nil {
		// Local agents may run without a daemon; config.toml is already
		// written, so this is a warning, not a failure.
		fmt.Fprintf(os.Stderr, "[setup] warning: skipping AGENTS.md: %v\n", err)
		return nil
	}
	return writeCodexAgentsMD(home, prime, extra)
}

// setupCodexSettings is the codex-harness counterpart of setupClaudeSettings.
func setupCodexSettings(coopWorkdir, role string, mockMode bool) {
	if mockMode {
		logInit("mock mode — skipping Codex settings")
		home, err := codexHomePath("", false)
		if err == nil && os.MkdirAll(home, 0o755) == nil {
			_ = os.WriteFile(filepath.Join(home, codexConfigFile), []byte("# mock mode\n"), 0o600)
		}
		return
	}
	if err := runSetupCodex(context.Background(), coopWorkdir, role, false); err != nil {
		logInit("config beads not found, installing codex defaults: %v", err)
		if err2 := runSetupCodexDefaults(coopWorkdir, role, false); err2 != nil {
			logInit("warning: could not write codex config: %v", err2)
		}
	}
	// Independent of config beads: codex 401s every turn without auth.json.
	if home, err := codexHomePath("", false); err == nil {
		installCodexAuthLogged(home)
	}
}
