package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"gardenio/controller/internal/beadsapi"
)

//go:embed claudemd_default.md
var defaultClaudeMDTemplate string

var setupCmd = &cobra.Command{
	Use:     "setup",
	Short:   "Setup commands for agent environment",
	GroupID: "session",
}

// gbHookPrefixes are command prefixes that indicate gb hook installations.
// Both the legacy gb bus emit style and the newer gb hook style are recognised.
var gbHookPrefixes = []string{"gb hook ", "gb bus emit --hook="}

var setupClaudeCmd = &cobra.Command{
	Use:   "claude",
	Short: "Materialize Claude Code settings and hooks from config beads",
	Long: `Fetches claude-settings, claude-mcp, and claude-hooks config beads
from the daemon, merges them by specificity (global → role), and writes:

  ~/.claude/settings.json                 — user-level (permissions, plugins)
  {workspace}/.mcp.json                   — project-level MCP server config
  {workspace}/.claude/settings.local.json — workspace-level (hooks)

Config bead keys (checked in order, later overrides earlier):
  claude-settings:global — base user-level settings for all agents
  claude-settings:<role> — role-specific settings overrides
  claude-mcp:global      — base MCP server config for all agents
  claude-mcp:<role>      — role-specific MCP server overrides
  claude-hooks:global    — base hooks for all agents
  claude-hooks:<role>    — role-specific hook overrides

Settings and MCP merge use one-level-deep key override: a top-level
role key replaces the corresponding global value, but sub-maps (env,
permissions, mcpServers) merge sub-key by sub-key.

claude-settings beads merge ON TOP of the hardcoded default settings,
so a role bead can flip a single knob (e.g. {"ultracode": true} plus
{"env": {"CLAUDE_CODE_WORKFLOWS": "1"}} to enable ultracode for that
role) without restating permissions/thinking-mode/effort. When no
claude-settings bead matches, the defaults are written unchanged.

Hooks merge uses array concatenation: role hooks are appended to (not
replacing) global hooks for each hook type.

MCP: if no claude-mcp beads exist, no .mcp.json is written (existing
repo .mcp.json files are unaffected). When beads exist, new servers fill
gaps without overwriting existing entries.

Flags:
  --defaults   Install hardcoded settings and hooks (no server needed)
  --check      Verify hooks are installed, exit 1 if missing
  --remove     Remove gb hooks from settings.local.json`,
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

		check, _ := cmd.Flags().GetBool("check")
		if check {
			return runSetupClaudeCheck(workspace)
		}

		remove, _ := cmd.Flags().GetBool("remove")
		if remove {
			return runSetupClaudeRemove(workspace)
		}

		local, _ := cmd.Flags().GetBool("local")

		defaults, _ := cmd.Flags().GetBool("defaults")
		if defaults {
			return runSetupClaudeDefaults(workspace, role, local)
		}

		return runSetupClaude(cmd.Context(), workspace, role, local)
	},
}

func init() {
	setupClaudeCmd.Flags().String("workspace", os.Getenv("KD_WORKSPACE"), "workspace directory")
	setupClaudeCmd.Flags().String("role", "", "agent role (default: $BOAT_ROLE or $KD_ROLE)")
	setupClaudeCmd.Flags().Bool("defaults", false, "install hardcoded default hooks (no server needed)")
	setupClaudeCmd.Flags().Bool("check", false, "verify hooks are installed (exit 1 if missing)")
	setupClaudeCmd.Flags().Bool("remove", false, "remove gb hooks from settings.local.json")
	setupClaudeCmd.Flags().Bool("local", false, "local mode: confine writes to <workspace>/.claude/ and <workspace>/.mcp.json (never touches ~/.claude/)")
	setupCmd.AddCommand(setupClaudeCmd)
}

func runSetupClaudeDefaults(workspace, role string, local bool) error {
	// Honor a per-spawn --ultracode override even on the daemon-less defaults
	// path (which never merges claude-settings beads). The ON/OFF transforms are
	// self-contained, so they compose correctly on top of the bare defaults.
	defaultSettings := defaultUserSettings()
	applyUltracodeOverride(defaultSettings, os.Getenv("BOAT_ULTRACODE"))
	if err := writeUserSettings(defaultSettings, workspace, role, local); err != nil {
		return fmt.Errorf("writing user settings: %w", err)
	}

	if mcpConfig := defaultMCPConfig(); mcpConfig != nil {
		if err := writeMCPConfig(workspace, mcpConfig); err != nil {
			fmt.Fprintf(os.Stderr, "[setup] warning: failed to write default MCP config: %v\n", err)
		}
	}

	settings := defaultHookSettings()
	appendJIRAStatusHooks(settings)

	outPath, err := writeWorkspaceHooks(workspace, settings, local)
	if err != nil {
		return fmt.Errorf("writing settings: %w", err)
	}

	fmt.Fprintf(os.Stderr, "[setup] wrote default hooks to %s\n", outPath)

	claudeMDPath := filepath.Join(workspace, "CLAUDE.md")
	if _, err := os.Stat(claudeMDPath); os.IsNotExist(err) {
		content := defaultClaudeMD(role)
		if err := os.WriteFile(claudeMDPath, []byte(content), 0644); err != nil {
			fmt.Fprintf(os.Stderr, "[setup] warning: failed to write fallback CLAUDE.md: %v\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "[setup] wrote fallback CLAUDE.md to %s\n", claudeMDPath)
		}
	}

	return nil
}

func runSetupClaudeCheck(workspace string) error {
	settingsPath := filepath.Join(workspace, ".claude", "settings.local.json")
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		return fmt.Errorf("hooks not installed: %s not found", settingsPath)
	}

	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		return fmt.Errorf("hooks not installed: invalid JSON in %s", settingsPath)
	}

	hooks, ok := settings["hooks"].(map[string]any)
	if !ok {
		return fmt.Errorf("hooks not installed: no hooks key in %s", settingsPath)
	}

	required := []string{"SessionStart", "Stop"}
	for _, ht := range required {
		if !hookContainsGB(hooks, ht) {
			return fmt.Errorf("hooks not installed: missing %s hook with gb command", ht)
		}
	}

	fmt.Fprintf(os.Stderr, "hooks OK: gb hooks installed in %s\n", settingsPath)
	return nil
}

// isGBHookCommand returns true if the command string is a gb hook command.
func isGBHookCommand(cmd string) bool {
	for _, prefix := range gbHookPrefixes {
		if strings.HasPrefix(cmd, prefix) {
			return true
		}
	}
	return false
}

func hookContainsGB(hooks map[string]any, hookType string) bool {
	arr, ok := hooks[hookType].([]any)
	if !ok {
		return false
	}
	for _, entry := range arr {
		entryMap, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		innerHooks, ok := entryMap["hooks"].([]any)
		if !ok {
			continue
		}
		for _, h := range innerHooks {
			hMap, ok := h.(map[string]any)
			if !ok {
				continue
			}
			cmd, _ := hMap["command"].(string)
			if isGBHookCommand(cmd) {
				return true
			}
		}
	}
	return false
}

func runSetupClaudeRemove(workspace string) error {
	settingsPath := filepath.Join(workspace, ".claude", "settings.local.json")
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		return fmt.Errorf("no settings file: %w", err)
	}

	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}

	hooks, ok := settings["hooks"].(map[string]any)
	if !ok {
		fmt.Fprintf(os.Stderr, "[setup] no hooks to remove\n")
		return nil
	}

	removed := 0
	for hookType, val := range hooks {
		arr, ok := val.([]any)
		if !ok {
			continue
		}
		var filtered []any
		for _, entry := range arr {
			entryMap, ok := entry.(map[string]any)
			if !ok {
				filtered = append(filtered, entry)
				continue
			}
			innerHooks, ok := entryMap["hooks"].([]any)
			if !ok {
				filtered = append(filtered, entry)
				continue
			}
			hasGB := false
			for _, h := range innerHooks {
				hMap, ok := h.(map[string]any)
				if !ok {
					continue
				}
				cmd, _ := hMap["command"].(string)
				if isGBHookCommand(cmd) {
					hasGB = true
					break
				}
			}
			if !hasGB {
				filtered = append(filtered, entry)
			} else {
				removed++
			}
		}
		if len(filtered) == 0 {
			delete(hooks, hookType)
		} else {
			hooks[hookType] = filtered
		}
	}

	if removed == 0 {
		fmt.Fprintf(os.Stderr, "[setup] no gb hooks found to remove\n")
		return nil
	}

	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("marshalling: %w", err)
	}
	out = append(out, '\n')

	if err := os.WriteFile(settingsPath, out, 0600); err != nil {
		return fmt.Errorf("writing: %w", err)
	}

	fmt.Fprintf(os.Stderr, "[setup] removed %d gb hook(s) from %s\n", removed, settingsPath)
	return nil
}

func runSetupClaude(ctx context.Context, workspace, role string, local bool) error {
	subs := buildSubscriptions(role)

	// ── Inline config overrides ──────────────────────────────────────────
	// Role bead claude_config: specificity 2^: (after role config beads, before project inline).
	// Project bead claude_config: specificity 2~: (after role inline, before agent).
	roleExtras := fetchRoleClaudeConfig(ctx, role)
	projectExtras := fetchProjectClaudeConfig(ctx)
	inlineExtras := mergeExtraLayers(roleExtras, projectExtras)

	// ── User-level settings (claude-settings) ───────────────────────────
	// Each of the four config-bead resolutions below is a separate daemon
	// round-trip with no transport-level retry, and they run synchronously on
	// the SessionStart boot path. A transient blip on any ONE of them fails the
	// whole "gb setup claude" — historically AFTER an earlier file was already
	// written, producing the config-merge-override E2E flake (correct output on
	// disk, non-zero exit; ka-0000006617). resolveConfigBeadsWithRetry absorbs
	// the transient with bounded backoff while still surfacing a persistent
	// failure. See its doc comment for the full rationale.
	settings, _, settingsErr := resolveConfigBeadsWithRetry(ctx, daemon, "claude-settings", subs, inlineExtras["claude-settings"]...)
	if settingsErr != nil {
		return fmt.Errorf("resolving claude-settings (daemon unreachable?): %w", settingsErr)
	}
	// Resolved claude-settings beads merge ON TOP of the hardcoded defaults
	// rather than replacing them wholesale. This keeps per-role settings beads
	// additive: a role bead that only flips {"ultracode": true} inherits the
	// default permissions/env/thinking-mode instead of dropping them. When no
	// bead matches, settings is nil and the defaults are written unchanged.
	finalSettings := defaultUserSettings()
	if settings != nil {
		finalSettings = mergeSettingsOntoDefaults(finalSettings, settings)
	}
	// Per-spawn ultracode override (--ultracode=on|off Slack flag → BOAT_ULTRACODE).
	// Applied LAST, after the bead merge, so it is authoritative over resolved
	// claude-settings beads. No-op when the env var is empty/unrecognized.
	applyUltracodeOverride(finalSettings, os.Getenv("BOAT_ULTRACODE"))
	if err := writeUserSettings(finalSettings, workspace, role, local); err != nil {
		fmt.Fprintf(os.Stderr, "[setup] warning: failed to write user settings: %v\n", err)
	}

	// ── Project-level MCP config (claude-mcp) ───────────────────────────
	mcpConfig, _, mcpErr := resolveConfigBeadsWithRetry(ctx, daemon, "claude-mcp", subs, inlineExtras["claude-mcp"]...)
	if mcpErr != nil {
		return fmt.Errorf("resolving claude-mcp (daemon unreachable?): %w", mcpErr)
	}
	if mcpConfig == nil {
		mcpConfig = defaultMCPConfig()
	}
	if mcpConfig != nil {
		if err := writeMCPConfig(workspace, mcpConfig); err != nil {
			fmt.Fprintf(os.Stderr, "[setup] warning: failed to write MCP config: %v\n", err)
		}
	}

	// ── Workspace-level hooks (claude-hooks) ────────────────────────────
	hooks, _, hooksErr := resolveConfigBeadsWithRetry(ctx, daemon, "claude-hooks", subs, inlineExtras["claude-hooks"]...)
	if hooksErr != nil {
		return fmt.Errorf("resolving claude-hooks (daemon unreachable?): %w", hooksErr)
	}
	if hooks == nil {
		return fmt.Errorf("no claude-hooks config found")
	}

	appendJIRAStatusHooks(hooks)

	outPath, err := writeWorkspaceHooks(workspace, hooks, local)
	if err != nil {
		return fmt.Errorf("writing settings: %w", err)
	}

	fmt.Fprintf(os.Stderr, "[setup] wrote %s\n", outPath)

	// ── Agent instructions (claude-instructions) ────────────────────────
	// Fail hard on daemon-unreachable here: the whole point of "gb setup
	// claude" is to materialize CLAUDE.md from config beads, and silently
	// exiting 0 with no CLAUDE.md written makes e2e config-instructions
	// pass even when the daemon is down. See ka-0000004806.
	instructions, _, instrErr := resolveConfigBeadsWithRetry(ctx, daemon, "claude-instructions", subs, inlineExtras["claude-instructions"]...)
	if instrErr != nil {
		return fmt.Errorf("resolving claude-instructions (daemon unreachable?): %w", instrErr)
	}
	if instructions != nil {
		writeInstructionFiles(workspace, instructions, local)
	}

	// ── Claude extensions (agents, skills, commands) ─────────────────────
	// The role bead's enabled_skills/disabled_skills gate which skills are
	// linked (Adam feedback #1, epic ka-0000004989) and enabled_agents/
	// disabled_agents gate which sub-agents are linked (ka-0000006908);
	// commands always link.
	symlinkClaudeExtensions(workspace, fetchRoleExtensionFilters(ctx, daemon, role))

	return nil
}

// roleBeadLister is the daemon surface fetchRoleExtensionFilters needs — just
// the role registry. Declared as an interface (satisfied by *beadsapi.Client)
// so the toggle resolution stays unit-testable with a fake, mirroring role.go's
// roleDataSource pattern.
type roleBeadLister interface {
	ListRoleBeads(ctx context.Context) (map[string]beadsapi.RoleInfo, error)
}

// fetchRoleExtensionFilters builds the per-subdir extension filters for the
// agent's role(s): the skills filter from the role beads' enabled_skills /
// disabled_skills fields and the agents filter from enabled_agents /
// disabled_agents. Both toggles share the multi-role union semantics of
// unionExtensionFilter; one daemon round-trip serves both.
//
// Best-effort: a nil lister or a daemon error yields empty (allow-all)
// filters — the same default as before the toggles existed.
func fetchRoleExtensionFilters(ctx context.Context, lister roleBeadLister, roleStr string) extensionFilters {
	filters := extensionFilters{
		skills: extensionFilter{enabled: map[string]bool{}, disabled: map[string]bool{}},
		agents: extensionFilter{enabled: map[string]bool{}, disabled: map[string]bool{}},
	}

	roles := splitRoles(roleStr)
	if len(roles) == 0 || lister == nil {
		return filters
	}

	roleBeads, err := lister.ListRoleBeads(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[setup] warning: failed to list role beads for skill/agent toggle: %v\n", err)
		return filters
	}

	filters.skills = unionExtensionFilter(roles, roleBeads, func(info beadsapi.RoleInfo) ([]string, []string) {
		return info.EnabledSkills, info.DisabledSkills
	})
	filters.agents = unionExtensionFilter(roles, roleBeads, func(info beadsapi.RoleInfo) ([]string, []string) {
		return info.EnabledAgents, info.DisabledAgents
	})

	if filters.skills.active() {
		fmt.Fprintf(os.Stderr, "[setup] role skill toggle: %d enabled-allow, %d disabled\n",
			len(filters.skills.enabled), len(filters.skills.disabled))
	}
	if filters.agents.active() {
		fmt.Fprintf(os.Stderr, "[setup] role agent toggle: %d enabled-allow, %d disabled\n",
			len(filters.agents.enabled), len(filters.agents.disabled))
	}
	return filters
}

// unionExtensionFilter builds one subdir's extensionFilter by unioning the
// participating roles' EFFECTIVE entry sets — an agent that wears N hats
// (comma-separated BOAT_ROLE) gets what any one of those hats would grant on
// its own. lists extracts the (enabled, disabled) pair for the subdir from a
// role bead.
//
//   - DENY side: an entry is disabled when ANY role disables it (deny wins
//     across roles, mirroring deny-wins within a single filter).
//   - ALLOW side: a role with NO allow-list (empty enabled list, or no role
//     bead at all) grants EVERY entry, so its effective allow-set is the
//     universe. The union with any universe is the universe — therefore if any
//     participating role is unrestricted, the combined allow-list is cleared
//     and only the deny-list gates. Otherwise the allow-list is the union of
//     every restricting role's enabled list.
//
// The account_whitelist multi-role convention (resolveRoleAccountWhitelist)
// also unions the literal entries across roles — the union direction is the
// same on both sides. Where the two DIVERGE is what a non-empty list then
// means: a non-empty account whitelist stays fail-closed
// (accountAllowedByRoleWhitelist permits ONLY the listed accounts), which is
// right for a credential boundary. The extension toggles are a curation
// feature, so they do the opposite — an unrestricted co-role collapses the
// allow-list to allow-all, failing toward GRANTING the capability an operator
// explicitly gave a co-role rather than silently stripping it when an
// unrelated restrictive role is added to the same agent.
func unionExtensionFilter(roles []string, roleBeads map[string]beadsapi.RoleInfo, lists func(beadsapi.RoleInfo) ([]string, []string)) extensionFilter {
	filter := extensionFilter{enabled: map[string]bool{}, disabled: map[string]bool{}}

	// anyRoleUnrestricted tracks whether some participating role grants every
	// entry (no bead, or a bead with an empty allow-list). If so, the union of
	// effective allow-sets is the universe and the allow-list must not restrict.
	anyRoleUnrestricted := false
	for _, roleName := range roles {
		info, ok := roleBeads[roleName]
		enabled, disabled := lists(info)
		if !ok || len(enabled) == 0 {
			anyRoleUnrestricted = true
		}
		if !ok {
			continue
		}
		for _, s := range enabled {
			filter.enabled[s] = true
		}
		for _, s := range disabled {
			filter.disabled[s] = true
		}
	}
	// A universe in the union collapses the allow-list; the deny-list still gates.
	if anyRoleUnrestricted {
		filter.enabled = map[string]bool{}
	}
	return filter
}

// roleInlineSpecificity / projectInlineSpecificity now live in config_resolve.go
// as thin wrappers over internal/claudeconfig (task ka-0000006351).

// fetchRoleClaudeConfig returns per-category extra layers from role beads'
// claude_config fields. For multi-role agents, layers from all matching roles
// are included with distinct specificity keys (first role sorts last = highest
// precedence, matching ResolveConfigBeads role ordering).
func fetchRoleClaudeConfig(ctx context.Context, roleStr string) map[string][]resolvedConfig {
	result := make(map[string][]resolvedConfig)

	roles := splitRoles(roleStr)
	if len(roles) == 0 {
		return result
	}

	// Fetch all role beads.
	resp, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"role"},
		Statuses: []string{"open"},
		Limit:    100,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "[setup] warning: failed to list role beads for claude_config: %v\n", err)
		return result
	}

	// Index role beads by title (role name).
	roleBeads := make(map[string]*beadsapi.BeadDetail)
	for _, b := range resp.Beads {
		if b.Title != "" {
			roleBeads[b.Title] = b
		}
	}

	for _, roleName := range roles {
		bead, ok := roleBeads[roleName]
		if !ok {
			continue
		}
		raw := bead.Fields["claude_config"]
		if raw == "" {
			continue
		}
		var cfg map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
			fmt.Fprintf(os.Stderr, "[setup] warning: invalid claude_config JSON in role bead %q: %v\n", roleName, err)
			continue
		}
		spec := roleInlineSpecificity(roleName)
		for category, val := range cfg {
			result[category] = append(result[category], resolvedConfig{
				value:       val,
				specificity: spec,
			})
		}
		fmt.Fprintf(os.Stderr, "[setup] loaded %d claude_config categories from role bead %q\n", len(cfg), roleName)
	}

	return result
}

// mergeExtraLayers combines multiple extra-layer maps into a single map.
// Used to merge role-inline and project-inline layers before passing to
// ResolveConfigBeads.
func mergeExtraLayers(maps ...map[string][]resolvedConfig) map[string][]resolvedConfig {
	result := make(map[string][]resolvedConfig)
	for _, m := range maps {
		for k, v := range m {
			result[k] = append(result[k], v...)
		}
	}
	return result
}

// fetchProjectClaudeConfig returns per-category extra layers from the
// current project bead's claude_config field. Returns an empty map (safe
// to index) when no project is set or the field is absent.
func fetchProjectClaudeConfig(ctx context.Context) map[string][]resolvedConfig {
	result := make(map[string][]resolvedConfig)

	project := os.Getenv("BOAT_PROJECT")
	if project == "" {
		return result
	}

	projects, err := daemon.ListProjectBeads(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[setup] warning: failed to list projects for claude_config: %v\n", err)
		return result
	}

	info, ok := projects[project]
	if !ok || info.ClaudeConfig == nil {
		return result
	}

	for category, raw := range info.ClaudeConfig {
		result[category] = []resolvedConfig{{
			value:       raw,
			specificity: projectInlineSpecificity,
		}}
	}

	fmt.Fprintf(os.Stderr, "[setup] loaded %d claude_config categories from project bead %q\n", len(result), project)
	return result
}
