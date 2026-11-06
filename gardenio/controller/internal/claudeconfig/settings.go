package claudeconfig

import (
	"encoding/json"
	"strings"
)

// DefaultUserSettings returns the hardcoded user-level Claude Code settings
// (permissions, thinking mode, dangerous-mode bypass, fleet env). This is the
// base onto which resolved claude-settings config beads merge, and the fallback
// when no bead matches.
//
// It is the single source of truth for the fleet baseline: cmd/gb "setup claude"
// writes it to each pod's ~/.claude/settings.json, and beads-viewer merges the
// same base to compute the effective per-role knobs it renders. Any change here
// changes BOTH the pod behavior and what the /claude-config matrix reports.
func DefaultUserSettings() map[string]any {
	return map[string]any{
		"permissions": map[string]any{
			"allow": []any{
				"Bash(*)", "Read(*)", "Write(*)", "Edit(*)",
				"Glob(*)", "Grep(*)", "WebFetch(*)", "WebSearch(*)",
			},
			"deny": InteractiveToolDenyList(),
		},
		"env": map[string]any{
			"CLAUDE_CODE_EFFORT_LEVEL": "high",
			"ENABLE_PROMPT_CACHING_1H": "true",
			// Lazy-load MCP tool schemas via ToolSearch instead of paying the
			// full tool-def blob in every turn's boot context. Claude Code
			// auto-DISABLES tool search when ANTHROPIC_BASE_URL is a
			// non-first-party host (our fleet runs behind the LiteLLM proxy),
			// so it must be set EXPLICITLY or the schemas load eagerly.
			// Overridable per-role via a claude-settings bead. See epic
			// ka-0000003733.
			"ENABLE_TOOL_SEARCH": "true",
			// ka-0000002844: raise the Bash-tool timeout so a legitimate
			// multi-minute FOREGROUND blocking wait (chiefly `gb yield`, which
			// parks the agent on a human decision) is not chopped at Claude
			// Code's 120s default. These are only DEFAULT/MAX ceilings — a
			// per-call `timeout` still wins. Overridable per-role via a
			// claude-settings bead. 30m default / 24h max (yield's own cap).
			"BASH_DEFAULT_TIMEOUT_MS": "1800000",
			"BASH_MAX_TIMEOUT_MS":     "86400000",
		},
		"alwaysThinkingEnabled":             true,
		"skipDangerousModePermissionPrompt": true,
		// Auto-approve project-scoped .mcp.json servers so the stdio servers
		// declared there (chiefly `playwright`) actually spawn headless. Without
		// this key, Claude Code treats every .mcp.json server as pending manual
		// approval and NEVER launches it (ka-0000001691). Overridable per-role
		// via a claude-settings bead.
		"enableAllProjectMcpServers": true,
	}
}

// MergeSettingsOntoDefaults merges resolved claude-settings (from config beads)
// on top of the DefaultUserSettings() base, using the same one-level-deep map
// merge as MergeLayers(MergeOverride). This makes per-role settings beads
// ADDITIVE: a role bead that sets only {"ultracode": true} keeps the default
// permissions/env/thinking-mode, rather than replacing the entire settings
// document. Sub-maps (env, permissions) merge sub-key by sub-key.
func MergeSettingsOntoDefaults(base, override map[string]any) map[string]any {
	baseRaw, err := json.Marshal(base)
	if err != nil {
		// base is always DefaultUserSettings(); marshalling it cannot fail in
		// practice, but if it ever does, fall back to the override alone.
		return override
	}
	overrideRaw, err := json.Marshal(override)
	if err != nil {
		return base
	}
	merged := MergeLayers(MergeOverride, []json.RawMessage{baseRaw, overrideRaw})
	clearEffortForUltracode(merged, override)
	return merged
}

// clearEffortForUltracode drops the inherited env.CLAUDE_CODE_EFFORT_LEVEL when
// the override enables ultracode but does NOT itself pin an effort level.
//
// DefaultUserSettings() hardcodes env.CLAUDE_CODE_EFFORT_LEVEL=high. Because the
// env sub-map merges key-by-key, a role bead that flips only {"ultracode": true}
// keeps that inherited "high" — and Claude Code treats a set
// CLAUDE_CODE_EFFORT_LEVEL as authoritative, pinning effort and blocking
// ultracode from applying xhigh. Removing the key lets ultracode take over.
//
// If the bead explicitly sets env.CLAUDE_CODE_EFFORT_LEVEL, that wins and is left
// untouched — an operator can still pin a specific level alongside ultracode.
//
// This is the exact "global effort pin blocks ultracode's xhigh" trap the
// /claude-config matrix is built to make legible (plan ka-0000005440).
func clearEffortForUltracode(merged, override map[string]any) {
	if uc, _ := merged["ultracode"].(bool); !uc {
		return
	}
	if overrideEnv, ok := override["env"].(map[string]any); ok {
		if _, pinned := overrideEnv["CLAUDE_CODE_EFFORT_LEVEL"]; pinned {
			return
		}
	}
	if env, ok := merged["env"].(map[string]any); ok {
		delete(env, "CLAUDE_CODE_EFFORT_LEVEL")
	}
}

// ApplyUltracodeOverride applies a per-spawn ultracode override to the final
// user settings map, AUTHORITATIVELY over whatever claude-settings config beads
// resolved. It is driven by the BOAT_ULTRACODE pod env var, which carries
// "on" / "off" / "" (empty = no override, honor beads).
//
// Must be called AFTER the settings document is fully assembled (defaults +
// resolved beads + clearEffortForUltracode) so it is the last writer.
//
//	ON  → ultracode=true, env.CLAUDE_CODE_WORKFLOWS="1", drop
//	      env.CLAUDE_CODE_EFFORT_LEVEL so ultracode's xhigh applies.
//	OFF → ultracode=false, drop env.CLAUDE_CODE_WORKFLOWS, and restore
//	      env.CLAUDE_CODE_EFFORT_LEVEL="high" (the DefaultUserSettings baseline).
//
// A no-op for any value other than "on"/"off".
func ApplyUltracodeOverride(settings map[string]any, raw string) {
	v := strings.ToLower(strings.TrimSpace(raw))
	if v != "on" && v != "off" {
		return
	}
	env, ok := settings["env"].(map[string]any)
	if !ok {
		env = map[string]any{}
	}
	switch v {
	case "on":
		settings["ultracode"] = true
		env["CLAUDE_CODE_WORKFLOWS"] = "1"
		delete(env, "CLAUDE_CODE_EFFORT_LEVEL")
	case "off":
		settings["ultracode"] = false
		delete(env, "CLAUDE_CODE_WORKFLOWS")
		env["CLAUDE_CODE_EFFORT_LEVEL"] = "high"
	}
	// Reassign in case env was freshly created (a malformed bead round-trip
	// could leave settings["env"] absent or a non-map).
	settings["env"] = env
}

// InteractiveToolDenyList returns the fleet-wide interactive tool deny list.
// These tools block a headless agent waiting for human input it can't provide.
func InteractiveToolDenyList() []any {
	return []any{
		"EnterPlanMode",
		"ExitPlanMode",
		"AskUserQuestion",
		"EnterWorktree",
		"ExitWorktree",
	}
}

// CrewToolDenyList returns the extra tools denied for the crew role — the
// in-session task/todo tools, which the gardenio workflow prohibits in favor of
// beads. Denying them turns the soft prompt rule into a hard permission block.
func CrewToolDenyList() []any {
	return []any{
		"TodoWrite",
		"TaskCreate",
		"TaskUpdate",
	}
}

// EnsureInteractiveToolsDenied merges the interactive tool deny list into the
// settings permissions, appending without duplicating entries.
func EnsureInteractiveToolsDenied(settings map[string]any) {
	appendToolsDenied(settings, InteractiveToolDenyList())
}

// EnsureRoleToolsDenied merges any role-scoped deny list into the settings
// permissions. Currently only the crew role has extra denies. Roles with no
// extra denies are a no-op. Accepts a comma-separated role string.
func EnsureRoleToolsDenied(settings map[string]any, role string) {
	for _, r := range SplitRoles(role) {
		if r == "crew" {
			appendToolsDenied(settings, CrewToolDenyList())
		}
	}
}

// appendToolsDenied appends tools to settings.permissions.deny, creating the
// permissions map if needed and skipping entries already present.
func appendToolsDenied(settings map[string]any, tools []any) {
	perms, ok := settings["permissions"].(map[string]any)
	if !ok {
		perms = make(map[string]any)
		settings["permissions"] = perms
	}

	existing := make(map[string]bool)
	if deny, ok := perms["deny"].([]any); ok {
		for _, d := range deny {
			if s, ok := d.(string); ok {
				existing[s] = true
			}
		}
	}

	deny, _ := perms["deny"].([]any)
	for _, tool := range tools {
		if s, ok := tool.(string); ok && !existing[s] {
			deny = append(deny, tool)
			existing[s] = true
		}
	}
	perms["deny"] = deny
}

// SplitRoles splits a comma-separated role string into individual roles.
// Returns nil for empty input. Trims whitespace around each role.
func SplitRoles(roles string) []string {
	if roles == "" {
		return nil
	}
	parts := strings.Split(roles, ",")
	var result []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}

// BuildSubscriptions computes the agent subscription labels for config
// resolution from a comma-separated role string, an optional project, and an
// optional agent name. The order matters: earlier role: entries have higher
// precedence during multi-role resolution (buildRoleIndex).
//
//	["global", "role:<r1>", "role:<r2>", "project:<p>", "agent:<a>"]
//
// Empty project/agent are omitted. This is the shared subscription builder used
// by both the pod write path (cmd/gb, sourcing os env) and the viewer's
// per-role effective-config computation (sourcing the role name directly).
func BuildSubscriptions(roles, project, agent string) []string {
	subs := []string{"global"}
	for _, role := range SplitRoles(roles) {
		subs = append(subs, "role:"+role)
	}
	if project != "" {
		subs = append(subs, "project:"+project)
	}
	if agent != "" {
		subs = append(subs, "agent:"+agent)
	}
	return subs
}
