package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"gardenio/controller/internal/claudeconfig"
)

// envVarPattern matches ${VAR_NAME} placeholders for environment variable expansion.
var envVarPattern = regexp.MustCompile(`\$\{[A-Za-z_][A-Za-z0-9_]*\}`)

// hookEntry creates a single hook entry for a Claude Code settings.local.json hooks array.
func hookEntry(command string) map[string]any {
	return map[string]any{
		"matcher": "",
		"hooks": []any{
			map[string]any{
				"type":    "command",
				"command": command,
			},
		},
	}
}

// defaultUserSettings returns the hardcoded user-level Claude Code settings.
// The fleet baseline now lives in internal/claudeconfig (shared with the
// beads-viewer /claude-config matrix, task ka-0000006351); this is a thin
// delegating wrapper so cmd/gb call sites and tests are unchanged.
func defaultUserSettings() map[string]any {
	return claudeconfig.DefaultUserSettings()
}

// writeMCPConfig writes project-level MCP config to {workspace}/.mcp.json.
// If the file already exists (e.g. from a cloned repo), mcpServers entries
// are merged without overwriting existing servers.
//
// Environment variable placeholders (${VAR}) in config values are expanded
// from the pod environment before writing. This allows config beads to
// reference secrets injected via K8s (e.g. ${MEZMO_SERVICE_KEY}).
func writeMCPConfig(workspace string, config map[string]any) error {
	outPath := filepath.Join(workspace, ".mcp.json")

	servers, _ := config["mcpServers"].(map[string]any)
	removals, _ := config["removeServers"].([]any)
	if len(servers) == 0 && len(removals) == 0 {
		fmt.Fprintf(os.Stderr, "[setup] no MCP servers to configure\n")
		return nil
	}

	// Merge with existing .mcp.json if present.
	existing := make(map[string]any)
	if data, err := os.ReadFile(outPath); err == nil {
		_ = json.Unmarshal(data, &existing)
	}

	existingServers := finalizeMCPServers(workspace, existing, config)

	existing["mcpServers"] = existingServers

	data, err := json.MarshalIndent(existing, "", "  ")
	if err != nil {
		return fmt.Errorf("marshalling MCP config: %w", err)
	}

	// Expand ${VAR} placeholders from the environment.
	expanded := expandEnvVars(string(data))
	out := []byte(expanded)
	out = append(out, '\n')

	if err := os.WriteFile(outPath, out, 0644); err != nil {
		return fmt.Errorf("writing MCP config: %w", err)
	}

	fmt.Fprintf(os.Stderr, "[setup] wrote MCP config to %s\n", outPath)
	return nil
}

// finalizeMCPServers returns the final MCP server map for a workspace: the
// servers already in existing["mcpServers"] (a repo .mcp.json) filled in with
// the config bead's servers, then the removeServers strip and the playwright /
// slack / gardenio-channel fixups. It is the single source of truth for both
// the Claude .mcp.json (writeMCPConfig) and the Codex [mcp_servers] tables
// (cmd/gb/setup_codex.go), so the two harnesses can never disagree on which
// servers an agent gets. ${VAR} placeholders are NOT expanded here; each
// writer expands them in its own output format.
func finalizeMCPServers(workspace string, existing, config map[string]any) map[string]any {
	servers, _ := config["mcpServers"].(map[string]any)
	existingServers, _ := existing["mcpServers"].(map[string]any)
	if existingServers == nil {
		existingServers = make(map[string]any)
	}

	// New servers fill gaps; existing entries are preserved.
	for name, cfg := range servers {
		if _, ok := existingServers[name]; !ok {
			existingServers[name] = cfg
		}
	}

	// Role-scoped removal: a claude-mcp config bead may declare a top-level
	// "removeServers": ["playwright", "mezmo"] to STRIP servers from the final
	// .mcp.json. writeMCPConfig is otherwise add-only (a bead can add or
	// override a server, never delete one), so a non-browser role could not
	// shed the playwright+mezmo tool-defs (~17k boot tokens re-read every turn)
	// that ride in the global claude-mcp bead. removeServers is that lever
	// (epic ka-0000003733): a belt-and-suspenders source-side trim complementing
	// the fleet-wide ENABLE_TOOL_SEARCH default, for roles that should not even
	// DECLARE a server.
	//
	// It runs AFTER the add-only merge (so it deletes what the global bead just
	// inserted) and BEFORE the fixPlaywright*/ensureGardenioChannel passes (those
	// gate on server presence and would otherwise re-touch a just-removed
	// playwright). Removal wins over any add/override in the same resolved
	// config. removeServers uses override (not concat) semantics across config
	// layers (claudeconfig.MergeLayers), so a role bead must list every server it
	// wants gone; it does not inherit the global bead's list.
	removeServers(existingServers, config["removeServers"])

	// Ensure playwright MCP always has --browser and PLAYWRIGHT_BROWSERS_PATH.
	// Without --browser, playwright-mcp defaults to "chrome" which requires
	// system-installed Google Chrome. Add --browser chromium as a fallback
	// to guarantee browser availability.
	fixPlaywrightBrowser(existingServers)
	fixPlaywrightEnv(existingServers)
	fixPlaywrightViewport(existingServers)
	fixPlaywrightVideo(existingServers)
	ensureSlackPlaywright(existingServers)
	ensureGardenioChannel(workspace, existingServers)

	return existingServers
}

// removeServers deletes every named server from the servers map. names is the
// raw value of a claude-mcp bead's top-level "removeServers" field: expected to
// be a []any of strings (["playwright", "mezmo"]). Non-list values and non-string
// elements are ignored, so a malformed bead is a harmless no-op rather than an
// error. Deleting a server that is not present is also a no-op.
func removeServers(servers map[string]any, names any) {
	list, ok := names.([]any)
	if !ok {
		return
	}
	for _, n := range list {
		name, ok := n.(string)
		if !ok || name == "" {
			continue
		}
		if _, present := servers[name]; present {
			delete(servers, name)
			fmt.Fprintf(os.Stderr, "[setup] MCP: removed server %q (removeServers directive)\n", name)
		}
	}
}

// expandEnvVars expands ${VAR} placeholders in s from the environment.
// Unresolved placeholders (empty env var) are replaced with an empty string
// and a warning is logged.
func expandEnvVars(s string) string {
	return envVarPattern.ReplaceAllStringFunc(s, func(match string) string {
		varName := match[2 : len(match)-1] // strip ${ and }
		val := os.Getenv(varName)
		if val == "" {
			fmt.Fprintf(os.Stderr, "[setup] warning: env var %s not set (placeholder unresolved)\n", varName)
		}
		return val
	})
}

// defaultMCPConfig returns a fallback MCP config with Playwright if the
// playwright-mcp binary is on PATH. Returns nil if not found.
// Uses --browser=chromium to ensure the bundled Chromium browser is used,
// which is always available in the agent image.
func defaultMCPConfig() map[string]any {
	if !pathExists("playwright-mcp") {
		return nil
	}
	return map[string]any{
		"mcpServers": map[string]any{
			"playwright": map[string]any{
				"command": "playwright-mcp",
				"args":    []any{"--browser", "chromium", "--headless", "--no-sandbox", "--viewport-size", "1280,720"},
				"env": map[string]any{
					"PLAYWRIGHT_BROWSERS_PATH": "/ms-playwright",
				},
			},
		},
	}
}

// fixPlaywrightBrowser ensures the playwright MCP server config includes
// a --browser flag. Without it, playwright-mcp defaults to "chrome" which
// requires system-installed Google Chrome — if Chrome is missing, the MCP
// server fails with "Chromium distribution 'chrome' is not found".
// This adds "--browser chromium" as a fallback when no --browser is specified.
func fixPlaywrightBrowser(servers map[string]any) {
	pw, ok := servers["playwright"]
	if !ok {
		return
	}
	cfg, ok := pw.(map[string]any)
	if !ok {
		return
	}
	args, ok := cfg["args"].([]any)
	if !ok {
		return
	}
	for _, a := range args {
		if s, ok := a.(string); ok && s == "--browser" {
			return // already has --browser flag
		}
	}
	// Prepend --browser chromium so the bundled Chromium is used as fallback.
	cfg["args"] = append([]any{"--browser", "chromium"}, args...)
	fmt.Fprintf(os.Stderr, "[setup] playwright MCP: added --browser chromium (no --browser flag found)\n")
}

// fixPlaywrightEnv ensures the playwright MCP server config includes
// PLAYWRIGHT_BROWSERS_PATH in its env block. Without it, the MCP server
// cannot find Chromium installed at /ms-playwright/ even though the
// Dockerfile sets the env var — config beads or user-provided .mcp.json
// may override the default config, losing the env section.
func fixPlaywrightEnv(servers map[string]any) {
	pw, ok := servers["playwright"]
	if !ok {
		return
	}
	cfg, ok := pw.(map[string]any)
	if !ok {
		return
	}
	env, ok := cfg["env"].(map[string]any)
	if !ok {
		env = make(map[string]any)
		cfg["env"] = env
	}
	if _, ok := env["PLAYWRIGHT_BROWSERS_PATH"]; !ok {
		env["PLAYWRIGHT_BROWSERS_PATH"] = "/ms-playwright"
		fmt.Fprintf(os.Stderr, "[setup] playwright MCP: added PLAYWRIGHT_BROWSERS_PATH=/ms-playwright\n")
	}
}

// fixPlaywrightViewport ensures the playwright MCP server config includes
// a --viewport-size flag. Without it, headless Chromium starts with a null
// viewport, causing screenshots to be 0x0 or blank on heavy SPAs.
func fixPlaywrightViewport(servers map[string]any) {
	pw, ok := servers["playwright"]
	if !ok {
		return
	}
	cfg, ok := pw.(map[string]any)
	if !ok {
		return
	}
	args, ok := cfg["args"].([]any)
	if !ok {
		return
	}
	for _, a := range args {
		if s, ok := a.(string); ok && s == "--viewport-size" {
			return // already has --viewport-size flag
		}
	}
	cfg["args"] = append(args, "--viewport-size", "1280,720")
	fmt.Fprintf(os.Stderr, "[setup] playwright MCP: added --viewport-size 1280,720\n")
}

// fixPlaywrightVideo ensures that when --save-video is present in the
// playwright MCP server args, --output-dir is also set. Without --output-dir,
// playwright-mcp has no writable directory for video files. This does NOT
// add --save-video itself — video recording is opt-in via config beads.
func fixPlaywrightVideo(servers map[string]any) {
	pw, ok := servers["playwright"]
	if !ok {
		return
	}
	cfg, ok := pw.(map[string]any)
	if !ok {
		return
	}
	args, ok := cfg["args"].([]any)
	if !ok {
		return
	}
	hasSaveVideo := false
	hasOutputDir := false
	for _, a := range args {
		s, ok := a.(string)
		if !ok {
			continue
		}
		if s == "--save-video" {
			hasSaveVideo = true
		}
		if s == "--output-dir" {
			hasOutputDir = true
		}
	}
	if !hasSaveVideo {
		return
	}
	if !hasOutputDir {
		cfg["args"] = append(args, "--output-dir", "/tmp/playwright-videos")
		fmt.Fprintf(os.Stderr, "[setup] playwright MCP: added --output-dir /tmp/playwright-videos (--save-video requires output dir)\n")
	}
}

// appendDetectedPlugins carries the settings' enabledPlugins map forward and,
// when detectLSP is true, enables the LSP plugin for each language server found
// on PATH.
//
// writeUserSettings passes detectLSP only in local (laptop) mode. In an agent
// pod, Claude Code starts gopls the first time the agent edits a .go file, and
// gopls then holds the go.work workspace in memory for the rest of the session:
// 1.4-1.9Gi RSS measured on a 3Gi crew pod. Add a big-package go test compile
// (1.8-2.3Gi) and the pod is OOM-killed. In the gd-zBUZ9bwCKl sweep, all 9 crew
// sessions killed this way had edited .go files, and none had used the LSP
// tool. rust-analyzer is left off for the same reason.
func appendDetectedPlugins(settings map[string]any, detectLSP bool) {
	plugins := make(map[string]any)
	if existing, ok := settings["enabledPlugins"].(map[string]any); ok {
		for k, v := range existing {
			plugins[k] = v
		}
	}

	if detectLSP && pathExists("gopls") {
		plugins["gopls-lsp@claude-plugins-official"] = true
	}
	if detectLSP && pathExists("rust-analyzer") {
		plugins["rust-analyzer-lsp@claude-plugins-official"] = true
	}

	settings["enabledPlugins"] = plugins
}

// teamsEnvDisabledRoles is the allowlist of PROVEN pure-orchestrator roles that
// shed the Agent Teams feature flag (and with it the ~2.2 KB agent_listing_delta
// pre-prompt block). These roles are structurally read→filter→squawk→stop: they
// spawn crew via `gb spawn`, never invoke the Agent tool, and never ToolSearch —
// so the harness built-in agent types are dead weight for every one of their
// sessions (design ka-0000004160#5 / ka-0000003275 Option D, operator decision
// ka-0000003631=rollout-tiered).
//
// This is a conservative allowlist of DISTINCT role labels, NOT a blanket off:
// every role not listed here keeps the Agent tool. The bare `role:gardener` label
// is deliberately EXCLUDED — it is too broad to target safely (it would hit
// every gardener sub-role, including any that legitimately spawn subagents). When
// generic cron gardeners (e.g. triage-intake) gain a distinct role label, add it
// here (design ka-0000004160 item 3).
var teamsEnvDisabledRoles = map[string]bool{
	"plan-gardener":          true,
	"gardener-delivery":      true,
	"gardener-bugfix":        true,
	"gardener-release-owner": true,
}

// roleShedsTeamsEnv reports whether a role (or, for a multi-role agent, EVERY one
// of its roles) is a proven pure orchestrator that can shed the Agent Teams flag.
// A multi-role agent keeps the flag unless all of its roles are allowlisted, so a
// composite role that includes e.g. crew is never silently stripped of the Agent
// tool.
func roleShedsTeamsEnv(role string) bool {
	roles := claudeconfig.SplitRoles(role)
	if len(roles) == 0 {
		return false
	}
	for _, r := range roles {
		if !teamsEnvDisabledRoles[r] {
			return false
		}
	}
	return true
}

// injectTeamsEnv adds the Claude Code Agent Teams feature flag to the settings
// env block when CLAUDE_TEAMS_ENABLED is set in the environment — unless the
// agent's role is a proven pure orchestrator (see teamsEnvDisabledRoles), in
// which case the flag (and its ~2.2 KB agent_listing_delta pre-prompt block) is
// shed. This runs AFTER the claude-settings bead merge and is the only
// pre-prompt dead-weight surface with no config-bead lever, so the gate lives
// here in the spawn path (design ka-0000004160#5).
func injectTeamsEnv(settings map[string]any, role string) {
	if v := os.Getenv("CLAUDE_TEAMS_ENABLED"); v != "true" && v != "1" {
		return
	}
	if roleShedsTeamsEnv(role) {
		fmt.Fprintf(os.Stderr, "[setup] Agent Teams disabled for pure-orchestrator role %q (shedding agent_listing)\n", role)
		return
	}
	env, ok := settings["env"].(map[string]any)
	if !ok {
		env = make(map[string]any)
	}
	env["CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS"] = "1"
	settings["env"] = env
	fmt.Fprintf(os.Stderr, "[setup] Agent Teams enabled (CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS=1)\n")
}

// lookPath is the function used to check if an executable exists on PATH.
// Defaults to exec.LookPath; overridden in tests for deterministic behavior.
var lookPath = exec.LookPath

// pathExists checks if an executable exists on PATH.
func pathExists(name string) bool {
	_, err := lookPath(name)
	return err == nil
}

func defaultHookSettings() map[string]any {
	// SessionStart: relay proof-of-life to NATS + prime context + check mail +
	// audit worktrees. The relay is FIRST and intentionally so: SessionStart is
	// the earliest proof an agent is alive (Claude has booted), and the Slack
	// bridge self-heal (ka-0000003479) promotes a stale "spawning" agent card to
	// "working" the instant it receives this event — before any tool fires.
	// Without this relay the card sits at "starting" through the entire boot +
	// prime window even though the agent is clearly working (ka-0000004260).
	sessionStart := []any{
		hookEntry("gb hook relay 2>/dev/null || true"),
		hookEntry("gb hook prime 2>/dev/null || true"),
		hookEntry("gb hook load-state 2>/dev/null || true"),
		hookEntry("gb hook check-mail 2>/dev/null || true"),
		hookEntry("gb workspace audit 2>/dev/null || true"),
	}

	// PreCompact: re-prime after compaction.
	preCompact := []any{
		hookEntry("gb hook prime --enqueue 2>/dev/null || true"),
		hookEntry("gb hook load-state 2>/dev/null || true"),
	}

	// Stop: gate check.
	stop := []any{
		hookEntry("gb hook stop-gate"),
	}

	return map[string]any{
		"hooks": map[string]any{
			"SessionStart": sessionStart,
			"PreCompact":   preCompact,
			// UserPromptSubmit: check mail on every human message.
			"UserPromptSubmit": []any{
				hookEntry("gb hook check-mail 2>/dev/null || true"),
			},
			"Stop": stop,
			// PreToolUse: relay tool events to NATS for live streaming status.
			"PreToolUse": []any{
				hookEntry("gb hook relay 2>/dev/null || true"),
			},
			// PostToolUse: drain the inject queue written by PreCompact --enqueue,
			// then relay tool events to NATS for live streaming status.
			// Runs after every tool call but drain is a no-op (--quiet) when no
			// queue file exists, so overhead is negligible.
			"PostToolUse": []any{
				hookEntry("/hooks/drain-queue.sh --quiet 2>/dev/null || true"),
				hookEntry("gb hook relay 2>/dev/null || true"),
			},
			// PostToolUseFailure: relay tool errors to NATS for debugging visibility.
			"PostToolUseFailure": []any{
				hookEntry("gb hook relay 2>/dev/null || true"),
			},
			// StopFailure: relay stop errors to NATS for debugging visibility.
			"StopFailure": []any{
				hookEntry("gb hook relay 2>/dev/null || true"),
			},
			// Subagent events: relay to NATS for streaming visibility.
			"SubagentStart": []any{
				hookEntry("gb hook relay 2>/dev/null || true"),
			},
			"SubagentStop": []any{
				hookEntry("gb hook relay 2>/dev/null || true"),
			},
			// Agent Teams: relay teammate and task events to NATS for visibility.
			"TaskCompleted": []any{
				hookEntry("gb hook relay 2>/dev/null || true"),
			},
			"TaskCreated": []any{
				hookEntry("gb hook relay 2>/dev/null || true"),
			},
			"TeammateIdle": []any{
				hookEntry("gb hook relay 2>/dev/null || true"),
			},
			// Notification, environment, and file events: relay for observability.
			"Notification": []any{
				hookEntry("gb hook relay 2>/dev/null || true"),
			},
			"CwdChanged": []any{
				hookEntry("gb hook relay 2>/dev/null || true"),
			},
			"FileChanged": []any{
				hookEntry("gb hook relay 2>/dev/null || true"),
			},
			// Config and instruction events: relay for debugging visibility.
			"ConfigChange": []any{
				hookEntry("gb hook relay 2>/dev/null || true"),
			},
			"InstructionsLoaded": []any{
				hookEntry("gb hook relay 2>/dev/null || true"),
			},
			// MCP elicitation events: relay for observability.
			"Elicitation": []any{
				hookEntry("gb hook relay 2>/dev/null || true"),
			},
			"ElicitationResult": []any{
				hookEntry("gb hook relay 2>/dev/null || true"),
			},
			// Worktree events: relay for observability.
			"WorktreeCreate": []any{
				hookEntry("gb hook relay 2>/dev/null || true"),
			},
			"WorktreeRemove": []any{
				hookEntry("gb hook relay 2>/dev/null || true"),
			},
			// Setup and permission events: relay for observability.
			"Setup": []any{
				hookEntry("gb hook relay 2>/dev/null || true"),
			},
			"PermissionRequest": []any{
				hookEntry("gb hook relay 2>/dev/null || true"),
			},
		},
	}
}

// jiraEnabled returns true if JIRA credentials are configured in the environment.
func jiraEnabled() bool {
	return os.Getenv("JIRA_BASE_URL") != "" &&
		os.Getenv("JIRA_EMAIL") != "" &&
		os.Getenv("JIRA_API_TOKEN") != ""
}

// appendJIRAStatusHooks adds JIRA status preservation hooks when JIRA
// credentials are available. These hooks prevent the JIRA-GitLab Automation
// for Jira integration from auto-transitioning ticket statuses when gardenio
// creates branches or MRs:
//
//   - PreToolUse (Bash): saves ticket status before glab/git push commands
//   - PostToolUse (Bash): restores ticket status after the automation fires
func appendJIRAStatusHooks(settings map[string]any) {
	if !jiraEnabled() {
		return
	}

	hooks, ok := settings["hooks"].(map[string]any)
	if !ok {
		hooks = make(map[string]any)
		settings["hooks"] = hooks
	}

	// PreToolUse: save JIRA ticket status before glab mr create / git push.
	saveHook := map[string]any{
		"matcher": "Bash",
		"hooks": []any{
			map[string]any{
				"type":    "command",
				"command": "/hooks/jira-status-save.sh 2>/dev/null || true",
			},
		},
	}
	preToolUse, ok := hooks["PreToolUse"].([]any)
	if !ok {
		preToolUse = []any{}
	}
	hooks["PreToolUse"] = append(preToolUse, saveHook)

	// PostToolUse: restore JIRA ticket status after command completes.
	resetHook := map[string]any{
		"matcher": "Bash",
		"hooks": []any{
			map[string]any{
				"type":    "command",
				"command": "/hooks/jira-status-reset.sh 2>/dev/null || true",
			},
		},
	}
	postToolUse, ok := hooks["PostToolUse"].([]any)
	if !ok {
		postToolUse = []any{}
	}
	hooks["PostToolUse"] = append(postToolUse, resetHook)

	fmt.Fprintf(os.Stderr, "[setup] JIRA status preservation hooks enabled\n")
}

// defaultClaudeMD returns a fallback CLAUDE.md template with role-specific
// context. This is used when no claude-instructions config beads are available.
func defaultClaudeMD(role string) string {
	if role == "" {
		role = "crew"
	}
	project := os.Getenv("BOAT_PROJECT")
	agent := os.Getenv("BOAT_AGENT")

	projectSuffix := ""
	if project != "" {
		projectSuffix = " (project: " + project + ")"
	}
	agentLine := ""
	if agent != "" {
		agentLine = "Agent name: " + agent + "\n"
	}
	projectRef := "your project"
	if project != "" {
		projectRef = project
	}

	r := strings.NewReplacer(
		"{{ROLE}}", role,
		"{{PROJECT_SUFFIX}}", projectSuffix,
		"{{AGENT_LINE}}", agentLine,
		"{{PROJECT_REF}}", projectRef,
	)
	return r.Replace(defaultClaudeMDTemplate)
}

// The interactive/crew tool deny lists and the permissions merge now live in
// internal/claudeconfig (task ka-0000006351). These thin wrappers keep cmd/gb
// call sites and its test suite unchanged.

// ensureInteractiveToolsDenied merges the interactive tool deny list into the
// settings permissions. Config beads may define their own deny list, so this
// appends without duplicating entries.
func ensureInteractiveToolsDenied(settings map[string]any) {
	claudeconfig.EnsureInteractiveToolsDenied(settings)
}

// ensureRoleToolsDenied merges any role-scoped deny list into the settings
// permissions. Currently only the crew role has extra denies (the in-session
// task/todo tools). Roles with no extra denies are a no-op.
func ensureRoleToolsDenied(settings map[string]any, role string) {
	claudeconfig.EnsureRoleToolsDenied(settings, role)
}

// writeUserSettings writes user-level Claude Code settings.
//
// role is the agent's role (comma-separated for multi-role agents); it selects
// any role-scoped tool deny list (e.g. crew's in-session task/todo tools). Pass
// "" for role-agnostic writes.
//
// When local is false (the K8s agent-pod default) settings are written to
// ~/.claude/settings.json — the pod's HOME is the agent's home, so a global
// write is the correct behaviour.
//
// When local is true (`gb agent start --local` on a developer laptop) the
// settings are merged into <workspace>/.claude/settings.local.json, leaving
// the operator's own ~/.claude/ untouched. They never go to
// <workspace>/.claude/settings.json: repos commit that file, and overwriting
// it dropped the gardenio repo's own PreCompact hooks and left the tracked
// file modified (gd-wKXxD4qXp6). Claude Code merges the project's
// settings.json, settings.local.json and user-level settings, so the repo's
// hooks still run alongside the agent persona. writeWorkspaceHooks later
// merges the hooks into the same settings.local.json.
func writeUserSettings(settings map[string]any, workspace, role string, local bool) error {
	appendDetectedPlugins(settings, local)
	injectTeamsEnv(settings, role)
	ensureInteractiveToolsDenied(settings)
	ensureRoleToolsDenied(settings, role)

	if local {
		if workspace == "" {
			return fmt.Errorf("local mode requires non-empty workspace")
		}
		outPath := filepath.Join(workspace, ".claude", "settings.local.json")
		if err := mergeIntoSettingsFile(outPath, settings); err != nil {
			return fmt.Errorf("writing user settings: %w", err)
		}
		fmt.Fprintf(os.Stderr, "[setup] merged user-level settings into %s (local mode: .claude/settings.json left untouched)\n", outPath)
		return nil
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("determining home dir: %w", err)
	}
	claudeDir := filepath.Join(homeDir, ".claude")
	if err := os.MkdirAll(claudeDir, 0755); err != nil {
		return fmt.Errorf("creating %s: %w", claudeDir, err)
	}

	outPath := filepath.Join(claudeDir, "settings.json")
	if err := writeSettingsFile(outPath, settings); err != nil {
		return fmt.Errorf("writing user settings: %w", err)
	}
	fmt.Fprintf(os.Stderr, "[setup] wrote user settings to %s\n", outPath)
	return nil
}

// writeWorkspaceHooks writes the resolved hooks config to
// <workspace>/.claude/settings.local.json and returns the path written.
//
// In the K8s pod that file is gb's alone and is replaced wholesale. In local
// mode it also carries the user-level layer writeUserSettings merged in (and
// whatever the operator keeps there), so the hooks are merged in by top-level
// key instead of replacing the file.
func writeWorkspaceHooks(workspace string, hooks map[string]any, local bool) (string, error) {
	outPath := filepath.Join(workspace, ".claude", "settings.local.json")
	if !local {
		if err := os.MkdirAll(filepath.Dir(outPath), 0755); err != nil {
			return "", fmt.Errorf("creating .claude dir: %w", err)
		}
		return outPath, writeSettingsFile(outPath, hooks)
	}
	if gitTracked(outPath) {
		fmt.Fprintf(os.Stderr, "[setup] warning: %s is tracked by git, so this local-agent write shows in git status; do not commit it (git update-index --skip-worktree keeps it out)\n", outPath)
	}
	return outPath, mergeIntoSettingsFile(outPath, hooks)
}

// mergeIntoSettingsFile overlays layer's top-level keys onto the JSON object
// already at path and writes the result, creating the file and its directory
// if needed. Keys the layer does not set survive, so local mode's user-level
// layer and hooks layer share one settings.local.json. An unreadable existing
// file is replaced with a warning, as the pre-merge wholesale write did.
func mergeIntoSettingsFile(path string, layer map[string]any) error {
	var merged map[string]any
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &merged); err != nil {
			fmt.Fprintf(os.Stderr, "[setup] warning: replacing unparseable %s: %v\n", path, err)
			merged = nil
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	if merged == nil {
		merged = map[string]any{}
	}
	for k, v := range layer {
		merged[k] = v
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	return writeSettingsFile(path, merged)
}

// writeSettingsFile writes settings as indented JSON with a trailing newline.
func writeSettingsFile(path string, settings map[string]any) error {
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("marshalling settings: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// gitTracked reports whether path is tracked by the git work tree that
// contains it. It is false when git is absent or path is outside a repo.
func gitTracked(path string) bool {
	cmd := exec.Command("git", "-C", filepath.Dir(path), "ls-files", "--error-unmatch", "--", filepath.Base(path))
	return cmd.Run() == nil
}

// gitIgnored reports whether path is ignored by the git work tree that
// contains it. It is false when git is absent or path is outside a repo.
func gitIgnored(path string) bool {
	cmd := exec.Command("git", "-C", filepath.Dir(path), "check-ignore", "-q", "--", filepath.Base(path))
	return cmd.Run() == nil
}

// instructionsPath returns where the claude_md instructions go, or "" to skip
// the write.
//
// In the K8s pod that is always <workspace>/CLAUDE.md. In local mode the
// workspace is the operator's own checkout, whose CLAUDE.md is usually
// committed, so a git-tracked CLAUDE.md is left alone and the instructions go
// to CLAUDE.local.md, which Claude Code also loads (gd-owzMVxfY1q).
func instructionsPath(workspace string, local bool) string {
	path := filepath.Join(workspace, "CLAUDE.md")
	if !local || !gitTracked(path) {
		return path
	}
	localPath := filepath.Join(workspace, "CLAUDE.local.md")
	if gitTracked(localPath) {
		fmt.Fprintf(os.Stderr, "[setup] warning: %s and %s are both tracked by git; not writing agent instructions\n", path, localPath)
		return ""
	}
	fmt.Fprintf(os.Stderr, "[setup] %s is tracked by git, so agent instructions go to %s instead\n", path, localPath)
	if !gitIgnored(localPath) {
		fmt.Fprintf(os.Stderr, "[setup] warning: %s is not gitignored, so it shows in git status; do not commit it (add CLAUDE.local.md to .git/info/exclude to hide it)\n", localPath)
	}
	return localPath
}

// writeInstructionFiles writes CLAUDE.md and stop-gate-text.md from the
// resolved claude-instructions config.
//
// CLAUDE.md is written under workspace, except that local mode writes
// CLAUDE.local.md instead of overwriting a git-tracked CLAUDE.md (see
// instructionsPath). The stop-gate-text.md file is written under ~/.claude/
// in the default (K8s pod) mode and under <workspace>/.claude/ in local mode,
// so that local invocations never touch the operator's global Claude Code
// home.
func writeInstructionFiles(workspace string, config map[string]any, local bool) {
	if claudeMD, ok := config["claude_md"].(string); ok && claudeMD != "" {
		if path := instructionsPath(workspace, local); path != "" {
			if err := os.WriteFile(path, []byte(claudeMD), 0644); err != nil {
				fmt.Fprintf(os.Stderr, "[setup] warning: failed to write %s: %v\n", filepath.Base(path), err)
			} else {
				fmt.Fprintf(os.Stderr, "[setup] wrote %s from claude-instructions config\n", path)
			}
		}
	}

	if stopGate, ok := config["stop_gate_blocked"].(string); ok && stopGate != "" {
		var claudeDir string
		if local {
			if workspace == "" {
				fmt.Fprintf(os.Stderr, "[setup] warning: local mode requires non-empty workspace; skipping stop-gate-text.md\n")
				return
			}
			claudeDir = filepath.Join(workspace, ".claude")
		} else {
			homeDir, err := os.UserHomeDir()
			if err != nil {
				fmt.Fprintf(os.Stderr, "[setup] warning: cannot determine home dir for stop-gate-text.md: %v\n", err)
				return
			}
			claudeDir = filepath.Join(homeDir, ".claude")
		}
		if err := os.MkdirAll(claudeDir, 0755); err != nil {
			fmt.Fprintf(os.Stderr, "[setup] warning: failed to create %s: %v\n", claudeDir, err)
			return
		}
		path := filepath.Join(claudeDir, "stop-gate-text.md")
		if err := os.WriteFile(path, []byte(stopGate), 0644); err != nil {
			fmt.Fprintf(os.Stderr, "[setup] warning: failed to write stop-gate-text.md: %v\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "[setup] wrote %s from claude-instructions config\n", path)
		}
	}
}

// The claude-settings merge, ultracode override, and low-level layer merge now
// live in internal/claudeconfig (task ka-0000006351) so the beads-viewer
// /claude-config matrix computes effective per-role settings with the exact same
// code that writes each pod's settings.json. These thin wrappers keep cmd/gb
// call sites and its test suite unchanged.

func mergeSettingsOntoDefaults(base, override map[string]any) map[string]any {
	return claudeconfig.MergeSettingsOntoDefaults(base, override)
}

func applyUltracodeOverride(settings map[string]any, raw string) {
	claudeconfig.ApplyUltracodeOverride(settings, raw)
}

// splitRoles splits a comma-separated role string into individual roles.
func splitRoles(roles string) []string {
	return claudeconfig.SplitRoles(roles)
}

// slackViewStorageStateEnv names the env var holding the path to the Slack test
// user's Playwright storageState file (the `d`/`d-s` session cookies), exposed to
// agent pods via ESO from the slack-e2e/test-user-session secret.
const slackViewStorageStateEnv = "SLACK_VIEW_STORAGE_STATE"

// ensureSlackPlaywright registers a second Playwright MCP server,
// "playwright-slack", for interactive click-through (threads, modals, buttons)
// of the Slack web client as the test user. It is separate from the default
// "playwright" server so ordinary browsing never carries the Slack session.
//
// It is added only when SLACK_VIEW_STORAGE_STATE points at an existing file, so
// pods without the test-user session (the common case) declare nothing extra.
// Uses --browser chrome (the system Google Chrome baked into the agent image,
// closer to what users run than bundled Chromium) and --isolated, which
// --storage-state requires: the profile stays in memory and the cookies are
// loaded fresh into each session. An existing "playwright-slack" entry (e.g.
// from a config bead) is left alone.
func ensureSlackPlaywright(servers map[string]any) {
	statePath := os.Getenv(slackViewStorageStateEnv)
	if statePath == "" {
		return
	}
	if _, ok := servers["playwright-slack"]; ok {
		return // already configured (e.g. via config bead)
	}
	if _, err := os.Stat(statePath); err != nil {
		fmt.Fprintf(os.Stderr, "[setup] %s=%s not readable (%v); skipping playwright-slack MCP server\n",
			slackViewStorageStateEnv, statePath, err)
		return
	}
	servers["playwright-slack"] = map[string]any{
		"command": "playwright-mcp",
		"args": []any{
			"--browser", "chrome",
			"--headless", "--no-sandbox",
			"--viewport-size", "1280,720",
			"--isolated",
			"--storage-state", statePath,
		},
		"env": map[string]any{
			"PLAYWRIGHT_BROWSERS_PATH": "/ms-playwright",
		},
	}
	fmt.Fprintf(os.Stderr, "[setup] added playwright-slack MCP server (storage state %s)\n", statePath)
}

// ensureGardenioChannel adds the gardenio-channel MCP server to the server map
// when GARDENIO_CHANNEL=1 and the channel source file exists in the workspace.
// This enables reliable MCP-based message delivery to replace PTY nudges.
func ensureGardenioChannel(workspace string, servers map[string]any) {
	if os.Getenv("GARDENIO_CHANNEL") != "1" {
		return
	}
	if _, ok := servers["gardenio-channel"]; ok {
		return // already configured (e.g. via config bead)
	}
	channelScript := filepath.Join(workspace, "channel", "gardenio-channel.ts")
	if _, err := os.Stat(channelScript); err != nil {
		return // channel script not present in workspace
	}
	servers["gardenio-channel"] = map[string]any{
		"command": "bun",
		"args":    []any{channelScript},
		"env": map[string]any{
			"GARDENIO_CHANNEL_PORT":         os.Getenv("GARDENIO_CHANNEL_PORT"),
			"GARDENIO_CHANNEL_TOKEN":        os.Getenv("GARDENIO_CHANNEL_TOKEN"),
			"GARDENIO_CHANNEL_CALLBACK_URL": os.Getenv("GARDENIO_CHANNEL_CALLBACK_URL"),
			"COOP_BROKER_TOKEN":             os.Getenv("COOP_BROKER_TOKEN"),
		},
	}
	fmt.Fprintf(os.Stderr, "[setup] added gardenio-channel MCP server (port %s)\n",
		os.Getenv("GARDENIO_CHANNEL_PORT"))
}

// buildSubscriptions computes the agent subscription labels for config resolution.
// Supports comma-separated roles (e.g. "thread,crew") — adds a role: subscription
// for each role. Sources project via resolveProject (flag > KD_PROJECT >
// BOAT_PROJECT) and agent (BOAT_AGENT) from the pod environment, then delegates
// the label assembly to claudeconfig.BuildSubscriptions (shared with the
// viewer's per-role path).
func buildSubscriptions(roles string) []string {
	project := resolveProject("")
	if project == "" {
		fmt.Fprintf(os.Stderr, "[setup] warning: no project in env (KD_PROJECT/BOAT_PROJECT unset); project-scoped config beads will not apply\n")
	}
	return claudeconfig.BuildSubscriptions(roles, project, os.Getenv("BOAT_AGENT"))
}
