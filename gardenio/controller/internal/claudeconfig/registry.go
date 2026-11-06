// Package claudeconfig is the shared source of truth for resolving and merging
// gardenio "config beads" — the type=config beads (claude-settings, claude-hooks,
// claude-mcp, prerun, …) that layer global → project → role → agent overrides on
// top of the hardcoded fleet defaults.
//
// It was factored out of cmd/gb (task ka-0000006351, plan ka-0000005440) so that
// TWO consumers can share ONE implementation:
//
//   - cmd/gb "setup claude" — writes each agent's ~/.claude/settings.json at
//     SessionStart from the resolved+merged layers (the write path).
//   - cmd/beads-viewer "/claude-config" — renders the read-only role×knob matrix
//     of effective per-role Claude Code settings (the view path).
//
// Keeping the merge in one place is what makes the viewer's matrix TRUTHFUL:
// the effective ultracode/effort/workflows/model it shows for a role is computed
// by the exact same code that writes the pod's settings.json — including the
// load-bearing "global effort pin blocks ultracode's xhigh" interaction
// (clearEffortForUltracode, settings.go) that operators kept tripping over.
//
// cmd/gb retains thin delegating wrappers/aliases over this package so its own
// public-looking API (ResolveConfigBeads, MergeLayers, defaultUserSettings, …)
// and its test suite are unchanged.
package claudeconfig

import (
	"encoding/json"
	"maps"
)

// MergeStrategy defines how config layers are combined when multiple beads match
// an agent's context (e.g. global + role-specific).
type MergeStrategy int

const (
	// MergeOverride uses shallow key override: later (more specific) layers
	// replace earlier values for the same top-level key. Sub-maps merge sub-key
	// by sub-key (mergeSimpleLayers), so a role bead adding env.FOO does not drop
	// a global bead's env.BAR. Used for: claude-settings, claude-mcp, type,
	// context, view, and the remaining scalar categories.
	MergeOverride MergeStrategy = iota

	// MergeConcat concatenates array values within the "hooks" key rather than
	// replacing them. Non-hook keys still use override semantics.
	// Used for: claude-hooks.
	MergeConcat

	// MergeConcatList concatenates every top-level array-valued key across layers
	// (global < project < role < agent), so more-specific layers ADD entries
	// rather than replacing them. Non-array keys use override semantics.
	// Used for: prerun (a per-role list of boot commands whose results are
	// injected into the system prompt).
	MergeConcatList
)

// ConfigCategory describes a known config bead category: its name, how layers
// merge, and a human-readable description. Output paths are not stored here
// because they depend on runtime context (workspace dir, home dir) and are
// handled by the cmd/gb setup command.
type ConfigCategory struct {
	// Name is the config category identifier, used as the bead title and the
	// config KV namespace. E.g. "claude-settings".
	Name string

	// Strategy controls how multiple matching layers are combined.
	Strategy MergeStrategy

	// Description is a human-readable summary of this category's purpose.
	Description string
}

// configCategories is the authoritative list of config bead categories that gb
// knows how to resolve and merge.
var configCategories = []ConfigCategory{
	{
		Name:        "claude-settings",
		Strategy:    MergeOverride,
		Description: "User-level Claude Code settings (model, permissions, plugins, thinking mode)",
	},
	{
		Name:        "claude-hooks",
		Strategy:    MergeConcat,
		Description: "Claude Code hooks (SessionStart, PreCompact, PostCompact, UserPromptSubmit, Stop)",
	},
	{
		Name:        "claude-mcp",
		Strategy:    MergeOverride,
		Description: "MCP server configuration (mcpServers key/value pairs)",
	},
	{
		Name:        "type",
		Strategy:    MergeOverride,
		Description: "Bead type system definitions (task, agent, epic schemas)",
	},
	{
		Name:        "context",
		Strategy:    MergeOverride,
		Description: "Contextual data for agent sessions",
	},
	{
		Name:        "view",
		Strategy:    MergeOverride,
		Description: "View definitions for bead listing and display",
	},
	{
		Name:        "claude-instructions",
		Strategy:    MergeOverride,
		Description: "Agent instruction text (workflow context, lifecycle, stop gate) — consumed by gb prime",
	},
	{
		Name:        "codex-config",
		Strategy:    MergeOverride,
		Description: "Codex CLI config.toml overrides for the codex harness (JSON keys deep-merged into config.toml; optional \"toml\" string key holds a raw TOML fragment)",
	},
	{
		Name:        "codex-instructions",
		Strategy:    MergeOverride,
		Description: "Codex-only agent instruction text (agents_md) appended to $CODEX_HOME/AGENTS.md after the gb prime output",
	},
	{
		Name:        "gemini-config",
		Strategy:    MergeOverride,
		Description: "Gemini CLI settings.json overrides for the gemini harness (allowlisted keys merged over the pinned defaults)",
	},
	{
		Name:        "gemini-instructions",
		Strategy:    MergeOverride,
		Description: "Gemini-only agent instruction text (gemini_md) appended to GEMINI.md after the gb prime output",
	},
	{
		Name:        "wrapup-config",
		Strategy:    MergeOverride,
		Description: "Wrap-up message requirements for gb stop (required fields, enforcement level, custom fields)",
	},
	{
		Name:        "human-survey-config",
		Strategy:    MergeOverride,
		Description: "Human exit-survey config for gb stop (enabled flag, rating scale, role allowlist, session-duration filter, expire hours)",
	},
	{
		Name:        "nudge-prompts",
		Strategy:    MergeOverride,
		Description: "Nudge prompt templates for agent startup (thread, adhoc, default, prewarmed)",
	},
	{
		Name:        "bridge-notifications",
		Strategy:    MergeOverride,
		Description: "Slack bridge notification settings — controls which agent notifications are emitted (suppression flags)",
	},
	{
		Name:        "idle-ttl",
		Strategy:    MergeOverride,
		Description: "Warm-keepalive idle-TTL for thread/DM agents (enabled flag, ttl_minutes) — keeps a parked-idle pod warm for low-latency follow-ups, then despawns cleanly",
	},
	{
		Name:        "prerun",
		Strategy:    MergeConcatList,
		Description: "Per-role boot commands whose (read-only) output is pre-executed and injected into the system prompt as a '## Prerun results' section, so gardeners/plan-gardeners don't spend a turn running `kd show <assigned>` + `gb news` at boot (ka-0000007894)",
	},
}

// configCategoryMap provides O(1) lookup by category name.
var configCategoryMap map[string]*ConfigCategory

func init() {
	configCategoryMap = make(map[string]*ConfigCategory, len(configCategories))
	for i := range configCategories {
		configCategoryMap[configCategories[i].Name] = &configCategories[i]
	}
}

// Categories returns a copy of the authoritative config category list.
func Categories() []ConfigCategory {
	out := make([]ConfigCategory, len(configCategories))
	copy(out, configCategories)
	return out
}

// ConfigCategoryNames returns the names of all known config categories.
func ConfigCategoryNames() []string {
	names := make([]string, len(configCategories))
	for i, c := range configCategories {
		names[i] = c.Name
	}
	return names
}

// LookupCategory returns the ConfigCategory for a given name, or nil.
func LookupCategory(name string) *ConfigCategory {
	return configCategoryMap[name]
}

// MergeLayers combines multiple JSON config layers according to the given
// strategy. Layers are applied in order (first = lowest priority, last =
// highest priority / most specific).
func MergeLayers(strategy MergeStrategy, layers []json.RawMessage) map[string]any {
	switch strategy {
	case MergeConcat:
		return mergeHookLayers(layers)
	case MergeConcatList:
		return mergeConcatListLayers(layers)
	default:
		return mergeSimpleLayers(layers)
	}
}

// mergeSimpleLayers merges JSON config layers with one-level-deep map merge.
// Top-level scalar values are overridden by later layers. Top-level map values
// are merged: individual sub-keys from later layers override earlier ones, but
// sub-keys not present in the later layer survive from the earlier layer.
//
// This is critical for mcpServers: a role-scoped bead that overrides
// server-alpha should not drop server-beta from the global bead.
func mergeSimpleLayers(layers []json.RawMessage) map[string]any {
	result := make(map[string]any)
	for _, raw := range layers {
		var layer map[string]any
		if err := json.Unmarshal(raw, &layer); err != nil {
			continue
		}
		for k, v := range layer {
			if existMap, ok := result[k].(map[string]any); ok {
				if vMap, ok := v.(map[string]any); ok {
					maps.Copy(existMap, vMap)
					continue
				}
			}
			result[k] = v
		}
	}
	return result
}

func mergeHookLayers(layers []json.RawMessage) map[string]any {
	result := make(map[string]any)
	for _, raw := range layers {
		var layer map[string]any
		if err := json.Unmarshal(raw, &layer); err != nil {
			continue
		}
		mergeSettingsLayer(result, layer)
	}
	return result
}

// mergeConcatListLayers merges JSON config layers where every top-level
// array-valued key is CONCATENATED across layers (earlier = lower priority,
// appended first) rather than replaced. Non-array keys use override semantics
// (later layer wins). Used for the `prerun` category, whose value is a list of
// boot-command entries that global < project < role < agent layers ADD to.
//
// De-duplication by entry `id` is handled downstream in resolvePrerunEntries so
// a role layer can still override a global entry by id.
func mergeConcatListLayers(layers []json.RawMessage) map[string]any {
	result := make(map[string]any)
	for _, raw := range layers {
		var layer map[string]any
		if err := json.Unmarshal(raw, &layer); err != nil {
			continue
		}
		for k, v := range layer {
			srcArr, isArr := v.([]any)
			if !isArr {
				result[k] = v
				continue
			}
			if dstArr, ok := result[k].([]any); ok {
				result[k] = append(dstArr, srcArr...)
			} else {
				// Copy so we never alias the caller's slice.
				result[k] = append([]any{}, srcArr...)
			}
		}
	}
	return result
}

func mergeSettingsLayer(dst, src map[string]any) {
	for k, v := range src {
		if k == "hooks" {
			srcHooks, ok := v.(map[string]any)
			if !ok {
				dst[k] = v
				continue
			}
			dstHooks, ok := dst[k].(map[string]any)
			if !ok {
				dstHooks = make(map[string]any)
				dst[k] = dstHooks
			}
			mergeHooksField(dstHooks, srcHooks)
		} else {
			dst[k] = v
		}
	}
}

func mergeHooksField(dst, src map[string]any) {
	for hookType, srcVal := range src {
		srcArr, ok := srcVal.([]any)
		if !ok {
			dst[hookType] = srcVal
			continue
		}
		dstArr, ok := dst[hookType].([]any)
		if !ok {
			dst[hookType] = srcArr
			continue
		}
		dst[hookType] = append(dstArr, srcArr...)
	}
}
