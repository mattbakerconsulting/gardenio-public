package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"gardenio/controller/internal/harness"
)

// Gemini harness config injection (ADR 0005 §5 E, task gd-xdnGt0w9tW).
//
// A harness=gemini pod gets the same three things a Claude pod gets, written in
// the Gemini CLI's formats, at USER level (~/.gemini). The system-level file
// (GEMINI_CLI_SYSTEM_SETTINGS_PATH) is silently skipped in a non-root pod
// (spike R3), so it is not used:
//
//	~/.claude/settings.json + .mcp.json   →  ~/.gemini/settings.json (hooks, mcpServers, pins)
//	--append-system-prompt-file           →  ~/.gemini/GEMINI.md  (lands in systemInstruction)
//	<ws>/CLAUDE.md                        →  unchanged (context.fileName lists it after GEMINI.md)
//
// The mcpServers come from finalizeMCPServers, the same function that builds
// the Claude and Codex files, so the harnesses cannot drift. Hook translation
// is in setup_gemini_hooks.go. GEMINI_CLI_HOME relocates ~/.gemini.

const (
	envGeminiModel = "GEMINI_MODEL"
	// envGeminiAuthMode is "oauth" when the controller credentialed the pod with
	// a Google-account login (a provider=gemini account whose credential is
	// secret:<name>#oauth_creds.json) instead of GEMINI_API_KEY.
	envGeminiAuthMode = "GEMINI_AUTH_MODE"
	// envGeminiAuthDir overrides geminiAuthDir (tests).
	envGeminiAuthDir = "GARDENIO_GEMINI_AUTH_DIR"
	// geminiAuthDir is where the controller mounts the login Secret read-only
	// (podmanager.MountGeminiAuth).
	geminiAuthDir = "/var/run/gardenio/gemini"
	// geminiOAuthFile is the login file the CLI reads and rewrites in ~/.gemini.
	geminiOAuthFile = "oauth_creds.json"

	geminiAuthTypeAPIKey = "gemini-api-key"
	geminiAuthTypeOAuth  = "oauth-personal"

	geminiSettingsFile = "settings.json"
	geminiContextFile  = "GEMINI.md"
)

// isGeminiHarness reports whether this process is materializing config for
// Gemini. It reads internal/harness so it agrees with gb init / gb agent start.
func isGeminiHarness() bool { return harness.IsGemini(harness.MustFromEnv()) }

// geminiHomePath resolves where settings.json and GEMINI.md go: <ws>/.gemini in
// local mode (never touching the operator's ~/.gemini; gb agent start points
// GEMINI_CLI_HOME at the workspace), else $GEMINI_CLI_HOME/.gemini, else
// ~/.gemini.
func geminiHomePath(workspace string, local bool) (string, error) {
	if local {
		if workspace == "" {
			return "", fmt.Errorf("local mode requires non-empty workspace")
		}
		return filepath.Join(workspace, ".gemini"), nil
	}
	return harness.GeminiHome(homeDir()), nil
}

// geminiSettingsInput is everything buildGeminiSettings needs; keeping it a
// plain struct (no env reads inside the builder) is what makes the golden test
// exact.
type geminiSettingsInput struct {
	Model string
	// OAuth selects the Google-account login (oauth-personal) over the API key.
	OAuth bool
	// MCPServers is the finalized Claude-shaped server map (finalizeMCPServers),
	// with ${VAR} placeholders still unexpanded.
	MCPServers map[string]any
	// Hooks is the translated {"hooks": {...}} document (buildGeminiHooks).
	Hooks map[string]any
	// Fragment is the resolved gemini-config bead, merged over the defaults.
	Fragment map[string]any
}

// geminiSettingsInputFromEnv fills the env-sourced fields.
func geminiSettingsInputFromEnv() geminiSettingsInput {
	return geminiSettingsInput{
		Model: strings.TrimSpace(os.Getenv(envGeminiModel)),
		OAuth: strings.TrimSpace(os.Getenv(envGeminiAuthMode)) == "oauth",
	}
}

// buildGeminiSettings assembles the settings.json document. The pinned keys are
// the three first-run gates of spike R7 (auth type, usage statistics, update
// checks) plus the model pin that avoids the extra utility_router call (R3).
// The returned warnings are non-fatal; the caller logs them.
func buildGeminiSettings(in geminiSettingsInput) (map[string]any, []string) {
	var warnings []string
	authType := geminiAuthTypeAPIKey
	if in.OAuth {
		authType = geminiAuthTypeOAuth
	}
	cfg := map[string]any{
		// Without selectedType the CLI exits 41 even with GEMINI_API_KEY set.
		"security": map[string]any{"auth": map[string]any{"selectedType": authType}},
		"general": map[string]any{
			"enableAutoUpdate":             false,
			"enableAutoUpdateNotification": false,
		},
		"privacy": map[string]any{"usageStatisticsEnabled": false},
		// CLAUDE.md is the shared instructions file; GEMINI.md (the prime output
		// at user level) stays first.
		"context": map[string]any{"fileName": []any{geminiContextFile, "CLAUDE.md"}},
	}
	if in.Model != "" {
		cfg["model"] = map[string]any{"name": in.Model}
	}

	mcp, mcpWarnings := translateMCPServersForGemini(in.MCPServers)
	warnings = append(warnings, mcpWarnings...)
	if len(mcp) > 0 {
		cfg["mcpServers"] = mcp
	}
	if hooks, _ := in.Hooks["hooks"].(map[string]any); len(hooks) > 0 {
		cfg["hooks"] = hooks
	}

	frag, fragWarnings := geminiFragmentToMap(in.Fragment)
	warnings = append(warnings, fragWarnings...)
	deepMergeInto(cfg, frag)
	return cfg, warnings
}

// translateMCPServersForGemini maps Claude-shaped mcpServers entries to the
// Gemini settings shape: stdio (command/args/env), streamable HTTP (httpUrl +
// headers) and SSE (url + headers). Every server is marked trusted: the pod is
// the sandbox and approval is "never ask" (ADR 0005 §8), so a per-tool prompt
// would only stall a turn. gardenio-channel is dropped silently: nudges reach
// Gemini through the driver, not a channel.
func translateMCPServersForGemini(servers map[string]any) (map[string]any, []string) {
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

		dst := map[string]any{"trust": true}
		switch {
		case command != "" && (typ == "" || typ == "stdio"):
			dst["command"] = expandEnvVars(command)
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
		case url != "" && (typ == "" || typ == "http" || typ == "streamable-http" || typ == "sse"):
			key := "httpUrl"
			if typ == "sse" {
				key = "url"
			}
			dst[key] = expandEnvVars(url)
			if headers := stringMap(src["headers"]); len(headers) > 0 {
				for k, v := range headers {
					headers[k] = expandEnvVars(v)
				}
				dst["headers"] = headers
			}
		default:
			warnings = append(warnings, fmt.Sprintf("mcp server %q (type %q): no Gemini equivalent, skipped", name, typ))
			continue
		}
		out[name] = dst
	}
	return out, warnings
}

// geminiAllowedKeys are the top-level settings.json keys a gemini-config bead
// may set. Any agent's shared token can write config beads, so the fragment is
// untrusted, and this is an allowlist rather than a denylist because the
// Gemini settings surface is large and most of it runs a program or redirects
// requests: hooks, mcpServers and tools (discoveryCommand/callCommand/sandbox)
// run programs, security (auth type, folder trust), proxy and telemetry send
// credentials or data elsewhere, privacy and general carry the update and
// statistics pins, and extensions/experimental/advanced load or widen code.
// gardenio generates hooks and mcpServers itself from trusted sources.
var geminiAllowedKeys = map[string]bool{
	"model":   true,
	"context": true,
	"ui":      true,
	"output":  true,
}

// geminiFragmentToMap turns the resolved gemini-config bead into settings.json
// keys. Config beads store JSON, so every key is taken as-is when allowlisted.
func geminiFragmentToMap(fragment map[string]any) (map[string]any, []string) {
	if len(fragment) == 0 {
		return nil, nil
	}
	var warnings []string
	out := map[string]any{}
	var stripped []string
	for k, v := range fragment {
		if !geminiAllowedKeys[k] {
			stripped = append(stripped, k)
			continue
		}
		out[k] = v
	}
	sort.Strings(stripped)
	for _, k := range stripped {
		warnings = append(warnings, fmt.Sprintf("gemini-config key %q is not allowed in a config bead (it runs a program, redirects requests or unpins a first-run gate), stripped", k))
	}
	return out, warnings
}

// renderGeminiSettings encodes the settings document (map keys sort, so the
// output is deterministic for the golden test).
func renderGeminiSettings(cfg map[string]any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // keep "2>/dev/null" readable in the file
	enc.SetIndent("", "  ")
	if err := enc.Encode(cfg); err != nil {
		return nil, fmt.Errorf("encoding gemini settings.json: %w", err)
	}
	return buf.Bytes(), nil
}

// writeGeminiSettings renders and writes <geminiHome>/settings.json (0600: MCP
// env values and headers may carry expanded secrets). It returns the dropped
// Claude hook events, the allowlist-rejected hook commands and the adjusted
// timeouts, for the caller to log.
func writeGeminiSettings(geminiHome string, in geminiSettingsInput, hookSrc map[string]any) error {
	doc, dropped, skipped, rejected, badTimeouts := buildGeminiHooks(hookSrc)
	in.Hooks = doc
	cfg, warnings := buildGeminiSettings(in)
	for _, w := range warnings {
		fmt.Fprintf(os.Stderr, "[setup] warning: gemini config: %s\n", w)
	}
	for _, b := range badTimeouts {
		fmt.Fprintf(os.Stderr, "[setup] warning: gemini hook timeout adjusted (%s)\n", b)
	}
	for _, r := range rejected {
		fmt.Fprintf(os.Stderr, "[setup] warning: dropped non-allowlisted gemini hook (%s)\n", r)
	}
	data, err := renderGeminiSettings(cfg)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(geminiHome, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", geminiHome, err)
	}
	path := filepath.Join(geminiHome, geminiSettingsFile)
	if err := writeHarnessFileAtomic(path, data, 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if in.OAuth {
		if err := installGeminiOAuthCreds(geminiHome); err != nil {
			return err
		}
	}
	fmt.Fprintf(os.Stderr, "[setup] wrote gemini settings to %s", path)
	if len(dropped) > 0 {
		fmt.Fprintf(os.Stderr, " (no Gemini event for: %s)", strings.Join(dropped, ", "))
	}
	if len(skipped) > 0 {
		fmt.Fprintf(os.Stderr, " (skipped, stdout does not reach the model: %s)", strings.Join(skipped, ", "))
	}
	fmt.Fprintln(os.Stderr)
	return nil
}

// installGeminiOAuthCreds copies the mounted Google-account login to
// <geminiHome>/oauth_creds.json (0600). The mount is read-only but the CLI
// rewrites the file when it refreshes the access token, so it needs its own
// writable copy. The Secret's refresh token is long-lived and not rotated, so
// restarting a pod from it always works, even when the PVC copy is newer.
// A missing source is an error: starting an oauth pod without a login would
// only fail later, inside the CLI, with an opaque auth error.
func installGeminiOAuthCreds(geminiHome string) error {
	dir := os.Getenv(envGeminiAuthDir)
	if dir == "" {
		dir = geminiAuthDir
	}
	src := filepath.Join(dir, geminiOAuthFile)
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("%s=oauth but the Gemini login is not mounted: %w", envGeminiAuthMode, err)
	}
	if !json.Valid(data) {
		return fmt.Errorf("%s is not valid JSON; re-export ~/.gemini/oauth_creds.json from a logged-in gemini CLI", src)
	}
	dst := filepath.Join(geminiHome, geminiOAuthFile)
	if err := writeHarnessFileAtomic(dst, data, 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", dst, err)
	}
	fmt.Fprintf(os.Stderr, "[setup] installed gemini login at %s\n", dst)
	return nil
}

// writeGeminiContextMD writes <geminiHome>/GEMINI.md: the `gb prime
// --system-prompt` output (the same content Claude gets via
// --append-system-prompt-file) plus the optional gemini-instructions gemini_md
// text. The CLI injects the user-level file into systemInstruction (spike R3).
func writeGeminiContextMD(geminiHome string, prime []byte, extra string) error {
	if err := os.MkdirAll(geminiHome, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", geminiHome, err)
	}
	path := filepath.Join(geminiHome, geminiContextFile)
	if err := writeHarnessFileAtomic(path, composeAgentsMD(prime, extra), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	fmt.Fprintf(os.Stderr, "[setup] wrote %s\n", path)
	return nil
}

// geminiInstructionsExtra resolves the gemini-instructions config bead and
// returns its gemini_md text. Best-effort: a daemon failure yields "" so a
// Gemini agent still boots with the plain prime output.
func geminiInstructionsExtra(ctx context.Context, role string) string {
	cfg, _, err := resolveConfigBeadsWithRetry(ctx, daemon, "gemini-instructions", buildSubscriptions(role))
	if err != nil {
		fmt.Fprintf(os.Stderr, "[setup] warning: resolving gemini-instructions: %v\n", err)
		return ""
	}
	s, _ := cfg["gemini_md"].(string)
	return s
}

// installGeminiMDFromPrime runs `gb prime --system-prompt` and installs the
// result as GEMINI.md. Best-effort: a failed prime is logged and the agent still
// launches (without refreshed advice).
func installGeminiMDFromPrime(logf func(string, ...any)) {
	prime, err := runPrimeSystemPrompt()
	if err != nil {
		logf("warning: gemini GEMINI.md: %v", err)
		return
	}
	installGeminiMD(prime, logf)
}

// installGeminiMD is the Gemini counterpart of passing the system-prompt file
// to --append-system-prompt-file. Called (via prepareAgentPrompt) on every coop
// session start so the file tracks the agent's current advice/identity.
func installGeminiMD(prime []byte, logf func(string, ...any)) {
	home, err := geminiHomePath("", false)
	if err != nil {
		logf("warning: gemini GEMINI.md: %v", err)
		return
	}
	extra := geminiInstructionsExtra(context.Background(), os.Getenv("BOAT_ROLE"))
	if err := writeGeminiContextMD(home, prime, extra); err != nil {
		logf("warning: gemini GEMINI.md: %v", err)
	}
}

var setupGeminiCmd = &cobra.Command{
	Use:   "gemini",
	Short: "Materialize Gemini settings.json and GEMINI.md from config beads",
	Long: `Writes the Gemini counterpart of "gb setup claude" for harness=gemini agents:

  $GEMINI_CLI_HOME/.gemini/settings.json — auth/update/statistics pins, model,
                            CLAUDE.md context fallback, mcpServers (from the same
                            claude-mcp source as .mcp.json), hooks (the claude-hooks
                            set translated to Gemini events) plus the merged
                            gemini-config bead
  $GEMINI_CLI_HOME/.gemini/GEMINI.md     — gb prime --system-prompt output plus the
                            merged gemini-instructions gemini_md text
  {workspace}/CLAUDE.md   — from claude-instructions, shared with Claude agents

GEMINI_CLI_HOME defaults to $HOME. With --local everything is written under
{workspace}/.gemini/ instead and the operator's ~/.gemini is never touched; a
git-tracked {workspace}/CLAUDE.md is left alone and the claude-instructions
text goes into that GEMINI.md instead.

The model comes from the pod env: GEMINI_MODEL.

Config bead keys (global → role, later overrides earlier):
  gemini-config:<scope>        — JSON keys deep-merged into settings.json; only
                                 model, context, ui and output are honoured
  gemini-instructions:<scope>  — {"gemini_md": "..."} appended to GEMINI.md

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
			return runSetupGeminiDefaults(workspace, role, local)
		}
		return runSetupGemini(cmd.Context(), workspace, role, local)
	},
}

func init() {
	setupGeminiCmd.Flags().String("workspace", os.Getenv("KD_WORKSPACE"), "workspace directory")
	setupGeminiCmd.Flags().String("role", "", "agent role (default: $BOAT_ROLE or $KD_ROLE)")
	setupGeminiCmd.Flags().Bool("defaults", false, "write built-in config only (no daemon needed)")
	setupGeminiCmd.Flags().Bool("local", false, "local mode: confine writes to <workspace>/.gemini/ (never touches ~/.gemini/)")
	setupCmd.AddCommand(setupGeminiCmd)
}

// runSetupGeminiDefaults writes the built-in config without a daemon: the
// fallback when config beads are unreachable, mirroring runSetupCodexDefaults.
func runSetupGeminiDefaults(workspace, role string, local bool) error {
	home, err := geminiHomePath(workspace, local)
	if err != nil {
		return err
	}
	in := geminiSettingsInputFromEnv()
	if mcp := defaultMCPConfig(); mcp != nil {
		in.MCPServers = finalizeMCPServers(workspace, map[string]any{}, mcp)
	}
	if err := writeGeminiSettings(home, in, harnessHooksSource(nil)); err != nil {
		return err
	}
	return writeGeminiLocalContextMD(home, local, "")
}

// runSetupGemini resolves config beads and writes settings.json (and, in local
// mode, GEMINI.md) plus the shared CLAUDE.md. In pods GEMINI.md is written at
// coop launch by installGeminiMD, once the prime output is current.
func runSetupGemini(ctx context.Context, workspace, role string, local bool) error {
	home, err := geminiHomePath(workspace, local)
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
	fragment, _, err := resolveConfigBeadsWithRetry(ctx, daemon, "gemini-config", subs)
	if err != nil {
		return fmt.Errorf("resolving gemini-config (daemon unreachable?): %w", err)
	}
	instructions, _, err := resolveConfigBeadsWithRetry(ctx, daemon, "claude-instructions", subs)
	if err != nil {
		return fmt.Errorf("resolving claude-instructions (daemon unreachable?): %w", err)
	}
	geminiInstr, _, err := resolveConfigBeadsWithRetry(ctx, daemon, "gemini-instructions", subs)
	if err != nil {
		return fmt.Errorf("resolving gemini-instructions (daemon unreachable?): %w", err)
	}
	// Hooks come from the same claude-hooks bead as Claude agents. A missing
	// bead is not an error here: the built-in defaults apply.
	hooksConfig, _, err := resolveConfigBeadsWithRetry(ctx, daemon, "claude-hooks", subs)
	if err != nil {
		return fmt.Errorf("resolving claude-hooks (daemon unreachable?): %w", err)
	}

	in := geminiSettingsInputFromEnv()
	in.Fragment = fragment
	if mcpConfig != nil {
		in.MCPServers = finalizeMCPServers(workspace, map[string]any{}, mcpConfig)
	}
	if err := writeGeminiSettings(home, in, harnessHooksSource(hooksConfig)); err != nil {
		return err
	}
	folded := writeClaudeMD(workspace, instructions, local, geminiContextFile)
	extra, _ := geminiInstr["gemini_md"].(string)
	return writeGeminiLocalContextMD(home, local, joinInstructions(folded, extra))
}

// writeGeminiLocalContextMD writes GEMINI.md at setup time for local mode, where
// no coop launch follows. In pod mode it is a no-op (see runSetupGemini).
func writeGeminiLocalContextMD(home string, local bool, extra string) error {
	if !local {
		return nil
	}
	prime, err := runPrimeSystemPrompt()
	if err != nil {
		// Local agents may run without a daemon; settings.json is already
		// written, so this is a warning, not a failure.
		fmt.Fprintf(os.Stderr, "[setup] warning: skipping GEMINI.md: %v\n", err)
		return nil
	}
	return writeGeminiContextMD(home, prime, extra)
}

// setupGeminiSettings is the gemini-harness counterpart of setupClaudeSettings.
// There is no credential step for an API-key account: the key arrives through
// coop's credential push as GEMINI_API_KEY, never through a file. A
// Google-account login is the exception: it is a file the CLI owns, installed
// from the mounted Secret by writeGeminiSettings.
func setupGeminiSettings(coopWorkdir, role string, mockMode bool) {
	if mockMode {
		logInit("mock mode — skipping Gemini settings")
		home, err := geminiHomePath("", false)
		if err == nil && os.MkdirAll(home, 0o755) == nil {
			_ = os.WriteFile(filepath.Join(home, geminiSettingsFile), []byte("{}\n"), 0o600)
		}
		return
	}
	if err := runSetupGemini(context.Background(), coopWorkdir, role, false); err != nil {
		logInit("config beads not found, installing gemini defaults: %v", err)
		if err2 := runSetupGeminiDefaults(coopWorkdir, role, false); err2 != nil {
			logInit("warning: could not write gemini config: %v", err2)
		}
	}
}
