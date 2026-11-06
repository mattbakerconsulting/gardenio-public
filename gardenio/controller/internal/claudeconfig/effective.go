package claudeconfig

import (
	"context"
	"encoding/json"
	"slices"
)

// The high-signal knobs surfaced by the beads-viewer /claude-config matrix. These
// are the settings operators actually reason about per role; the full resolved
// settings.json is available in the role detail view.
const (
	KnobUltracode = "ultracode"
	KnobEffort    = "effort"    // env.CLAUDE_CODE_EFFORT_LEVEL
	KnobWorkflows = "workflows" // env.CLAUDE_CODE_WORKFLOWS presence
	KnobModel     = "model"
)

// KnobOrder is the stable left-to-right column order for the matrix.
var KnobOrder = []string{KnobUltracode, KnobEffort, KnobWorkflows, KnobModel}

// KnobValue is one effective knob cell: the resolved value plus the provenance
// of where it came from (which merge layer last set it).
type KnobValue struct {
	// Value is the human-readable effective value of the knob, e.g. "true",
	// "xhigh", "on", "claude-sonnet-5", or "" when unset/default.
	Value string
	// Set reports whether the knob has a meaningful (non-default) resolved value.
	// A false Set renders as an em-dash "—" default cell.
	Set bool
	// Provenance is the scope that last set this knob: "default", "global",
	// "project", "role", "agent", or "override" (per-spawn ultracode). Empty
	// when Set is false.
	Provenance string
	// ProvenanceTarget is the scope target (project/role/agent name) when the
	// provenance is a targeted layer; empty for default/global/override.
	ProvenanceTarget string
	// BeadID is the source config bead id when the value came from a config
	// bead layer (not default/override/inline). Empty otherwise.
	BeadID string
}

// EffectiveConfig is the resolved per-role Claude Code configuration: the fully
// merged settings.json plus the per-knob provenance the matrix renders.
type EffectiveConfig struct {
	// Subscriptions is the ordered subscription list this config resolved for
	// (["global", "role:crew", "project:gardenio"]).
	Subscriptions []string
	// Settings is the fully merged settings.json map (defaults + config-bead
	// layers), the exact document `gb setup claude` would write for a pod with
	// these subscriptions (before the per-spawn ultracode override, which is a
	// pod-time flag not a stored config).
	Settings map[string]any
	// LayerCount is the number of claude-settings config-bead layers that
	// matched (0 means only defaults apply).
	LayerCount int
	// Knobs is the effective value + provenance for each high-signal knob,
	// keyed by KnobUltracode/KnobEffort/KnobWorkflows/KnobModel.
	Knobs map[string]KnobValue
	// EffortTrap is true when ultracode is ON but a non-role effort pin (global
	// or project) is blocking ultracode's xhigh — the exact interaction the
	// matrix exists to surface (plan ka-0000005440). When true, Knobs[effort]
	// reflects the pinned level and ProvenanceTarget names the blocking layer.
	EffortTrap bool
}

// ResolveEffectiveConfig computes the effective Claude Code settings for a set of
// subscriptions, using the SAME resolve+merge path as `gb setup claude`. It
// returns the merged settings.json plus per-knob provenance for the matrix.
//
// It resolves claude-settings config beads for the subscriptions, merges them
// onto DefaultUserSettings() (which applies clearEffortForUltracode — the
// ultracode/effort interaction), then walks the sorted layers a second time to
// attribute each high-signal knob to the layer that last set it.
//
// A daemon listing failure returns (nil, err). On success EffectiveConfig is
// always non-nil, even when no config bead matched (defaults only).
func ResolveEffectiveConfig(ctx context.Context, lister BeadLister, subscriptions []string, extraLayers ...Layer) (*EffectiveConfig, error) {
	layers, err := ResolveConfigLayers(ctx, lister, "claude-settings", subscriptions, extraLayers...)
	if err != nil {
		return nil, err
	}

	// Merge onto defaults exactly as the write path does.
	settings := DefaultUserSettings()
	if len(layers) > 0 {
		raws := make([]json.RawMessage, len(layers))
		for i, l := range layers {
			raws[i] = l.value
		}
		merged := MergeLayers(MergeOverride, raws)
		settings = MergeSettingsOntoDefaults(settings, merged)
	}

	ec := &EffectiveConfig{
		Subscriptions: subscriptions,
		Settings:      settings,
		LayerCount:    len(layers),
		Knobs:         make(map[string]KnobValue, len(KnobOrder)),
	}

	// Attribute each knob. Provenance walks the sorted layers (lowest → highest
	// priority) and keeps the LAST layer that set the knob — mirroring the merge.
	ec.Knobs[KnobUltracode] = attributeUltracode(settings, layers)
	ec.Knobs[KnobWorkflows] = attributeWorkflows(settings, layers)
	ec.Knobs[KnobModel] = attributeScalar(settings, layers, "model")
	effort, trap := attributeEffort(settings, layers)
	ec.Knobs[KnobEffort] = effort
	ec.EffortTrap = trap

	return ec, nil
}

// layerScalar reads a top-level scalar key from a layer's JSON, returning its
// value and whether the key is present.
func layerScalar(l Layer, key string) (any, bool) {
	var m map[string]any
	if err := json.Unmarshal(l.value, &m); err != nil {
		return nil, false
	}
	v, ok := m[key]
	return v, ok
}

// layerEnv reads an env sub-key from a layer's JSON.
func layerEnv(l Layer, envKey string) (any, bool) {
	var m map[string]any
	if err := json.Unmarshal(l.value, &m); err != nil {
		return nil, false
	}
	env, ok := m["env"].(map[string]any)
	if !ok {
		return nil, false
	}
	v, ok := env[envKey]
	return v, ok
}

// provenanceOf builds the scope/target/beadID triple for a layer.
func provenanceOf(l Layer) (scope, target, beadID string) {
	scope = l.scope
	if scope == "" {
		// Inline (role/project claude_config) layers carry no labels; classify
		// by their specificity prefix so provenance is still meaningful.
		switch {
		case l.specificity == ProjectInlineSpecificity:
			scope = "project"
		case len(l.specificity) >= 2 && l.specificity[:2] == "2^":
			scope = "role"
		default:
			scope = "config"
		}
	}
	return scope, l.target, l.beadID
}

// attributeScalar finds the last layer that set a top-level scalar key and
// reports the effective value + provenance. Falls back to the merged settings
// value with "default" provenance when no layer set it.
func attributeScalar(settings map[string]any, layers []Layer, key string) KnobValue {
	kv := KnobValue{}
	if v, ok := settings[key]; ok {
		kv.Value = toDisplay(v)
		kv.Set = kv.Value != ""
	}
	// Walk layers high→low priority; first hit wins (it's the effective one).
	for _, l := range slices.Backward(layers) {
		if v, ok := layerScalar(l, key); ok {
			kv.Value = toDisplay(v)
			kv.Set = kv.Value != ""
			kv.Provenance, kv.ProvenanceTarget, kv.BeadID = provenanceOf(l)
			return kv
		}
	}
	if kv.Set {
		kv.Provenance = "default"
	}
	return kv
}

// attributeUltracode reports the effective ultracode flag + provenance.
func attributeUltracode(settings map[string]any, layers []Layer) KnobValue {
	kv := KnobValue{}
	uc, _ := settings["ultracode"].(bool)
	kv.Value = boolWord(uc)
	kv.Set = uc // only surface when ON — OFF is the fleet default, rendered "—"
	for _, l := range slices.Backward(layers) {
		if v, ok := layerScalar(l, "ultracode"); ok {
			b, _ := v.(bool)
			kv.Value = boolWord(b)
			kv.Set = b
			kv.Provenance, kv.ProvenanceTarget, kv.BeadID = provenanceOf(l)
			return kv
		}
	}
	if kv.Set {
		kv.Provenance = "default"
	}
	return kv
}

// attributeWorkflows reports whether CLAUDE_CODE_WORKFLOWS is present (the
// presence-driven enable knob), with provenance.
func attributeWorkflows(settings map[string]any, layers []Layer) KnobValue {
	kv := KnobValue{}
	if env, ok := settings["env"].(map[string]any); ok {
		if _, present := env["CLAUDE_CODE_WORKFLOWS"]; present {
			kv.Value = "on"
			kv.Set = true
		}
	}
	if !kv.Set {
		return kv
	}
	for _, l := range slices.Backward(layers) {
		if _, ok := layerEnv(l, "CLAUDE_CODE_WORKFLOWS"); ok {
			kv.Provenance, kv.ProvenanceTarget, kv.BeadID = provenanceOf(l)
			return kv
		}
		// ultracode:true in a layer implies the workflows knob too (added at
		// pod-time), so attribute to that layer when no explicit env key exists.
		if v, ok := layerScalar(l, "ultracode"); ok {
			if b, _ := v.(bool); b {
				kv.Provenance, kv.ProvenanceTarget, kv.BeadID = provenanceOf(l)
				return kv
			}
		}
	}
	kv.Provenance = "default"
	return kv
}

// attributeEffort reports the effective CLAUDE_CODE_EFFORT_LEVEL + provenance,
// and whether the "global/project effort pin blocks ultracode's xhigh" trap is
// active. When ultracode is ON and effort is unset in the merged settings,
// ultracode's xhigh applies (the effort key was cleared) — value "xhigh",
// provenance the ultracode-setting layer. When ultracode is ON but effort is
// still pinned by a non-role layer, that's the trap.
func attributeEffort(settings map[string]any, layers []Layer) (KnobValue, bool) {
	kv := KnobValue{}
	uc, _ := settings["ultracode"].(bool)

	effortSet := false
	var effortVal string
	if env, ok := settings["env"].(map[string]any); ok {
		if v, present := env["CLAUDE_CODE_EFFORT_LEVEL"]; present {
			effortVal = toDisplay(v)
			effortSet = true
		}
	}

	if uc && !effortSet {
		// Effort key was cleared by clearEffortForUltracode → ultracode's xhigh
		// governs. Attribute to whichever layer turned ultracode on.
		kv.Value = "xhigh"
		kv.Set = true
		for _, l := range slices.Backward(layers) {
			if v, ok := layerScalar(l, "ultracode"); ok {
				if b, _ := v.(bool); b {
					kv.Provenance, kv.ProvenanceTarget, kv.BeadID = provenanceOf(l)
					return kv, false
				}
			}
		}
		kv.Provenance = "default"
		return kv, false
	}

	// Effort is explicitly pinned (or ultracode off). Find the layer that set it.
	kv.Value = effortVal
	kv.Set = effortSet
	pinnedScope := ""
	for _, l := range slices.Backward(layers) {
		if _, ok := layerEnv(l, "CLAUDE_CODE_EFFORT_LEVEL"); ok {
			kv.Provenance, kv.ProvenanceTarget, kv.BeadID = provenanceOf(l)
			pinnedScope = kv.Provenance
			break
		}
	}
	if kv.Provenance == "" && effortSet {
		kv.Provenance = "default" // the DefaultUserSettings high pin
		pinnedScope = "default"
	}

	// The trap: ultracode ON, effort still pinned by a layer that is NOT the
	// role's own explicit pin (global/project/default). A role that pins its own
	// effort alongside ultracode is a deliberate operator choice, not the trap.
	trap := uc && effortSet && pinnedScope != "role"
	return kv, trap
}

// toDisplay renders a JSON scalar as a compact display string.
func toDisplay(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case bool:
		return boolWord(x)
	case float64:
		// JSON numbers decode as float64; render integers without a decimal.
		if x == float64(int64(x)) {
			return itoa(int64(x))
		}
		b, _ := json.Marshal(x)
		return string(b)
	case nil:
		return ""
	default:
		b, _ := json.Marshal(x)
		return string(b)
	}
}

func boolWord(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// RoleScope is one (project, role) pair the /claude-config matrix renders as a
// row. A Project of "" means the role's only claude-settings beads carry no
// project: label, so it resolves identically in every project.
type RoleScope struct {
	Project string
	Role    string
}

// DiscoverRoleScopes returns every (project, role) pair that has at least one
// open claude-settings bead, sorted by project then role. When project is ""
// every project contributes; otherwise only beads compatible with that project
// do (projectCompatible).
//
// This exists because a single viewer-wide project cannot describe a
// multi-project daemon: the same role name resolves to DIFFERENT effective
// settings in different projects, and resolving every row against one project
// (or against none) misreports all but one of them.
func DiscoverRoleScopes(ctx context.Context, lister BeadLister, project string) ([]RoleScope, error) {
	return listSettingsRoleScopes(ctx, lister, project)
}
