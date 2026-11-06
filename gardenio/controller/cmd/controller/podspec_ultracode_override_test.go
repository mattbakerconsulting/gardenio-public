package main

import (
	"testing"

	"gardenio/controller/internal/config"
	"gardenio/controller/internal/subscriber"
)

// The per-spawn --ultracode override (Slack flag → bead field "ultracode" →
// event.Metadata["ultracode"]) must be mapped to the BOAT_ULTRACODE pod env var
// in BOTH spec builders, mirroring applyAgentModelOverride, so a reconcile or
// restart never strips it.

func newUltracodeCfg() *config.Config {
	return &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{
			"gardenio": {Account: "gardenio"},
		},
		RoleCache: map[string]config.RoleCacheEntry{
			"crew": {},
		},
		AccountCache: map[string]config.AccountCacheEntry{
			"gardenio": {Provider: "anthropic", Model: "account-default-model", LiteLLMKey: "sk"},
		},
	}
}

func eventForUltracode(v string) subscriber.Event {
	meta := map[string]string{}
	if v != "" {
		meta["ultracode"] = v
	}
	return subscriber.Event{Project: "gardenio", Role: "crew", AgentName: "agent1", Metadata: meta}
}

// TestUltracodeOverride_EventsPath: buildAgentPodSpec bakes BOAT_ULTRACODE for
// on/off and leaves it unset for absent/unrecognized.
func TestUltracodeOverride_EventsPath(t *testing.T) {
	cfg := newUltracodeCfg()

	tests := []struct {
		name string
		meta string
		want string // "" means the env key must be absent
	}{
		{"on", "on", "on"},
		{"off", "off", "off"},
		{"absent", "", ""},
		{"unrecognized", "maybe", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := buildAgentPodSpec(cfg, eventForUltracode(tc.meta))
			got, present := spec.Env[EnvUltracode]
			if tc.want == "" {
				if present {
					t.Errorf("expected BOAT_ULTRACODE unset, got %q", got)
				}
				return
			}
			if got != tc.want {
				t.Errorf("BOAT_ULTRACODE: got %q (present=%v), want %q", got, present, tc.want)
			}
		})
	}
}

// TestUltracodeOverride_ReconcilerPath: BuildSpecFromBeadInfo (restart/reconcile)
// must apply the same override, and be idempotent across repeated calls, so a
// per-spawn ultracode is never dropped mid-run.
func TestUltracodeOverride_ReconcilerPath(t *testing.T) {
	cfg := newUltracodeCfg()

	spec := BuildSpecFromBeadInfo(cfg, "gardenio", "crew", "crew", "agent1", map[string]string{"ultracode": "off"})
	if spec.Env[EnvUltracode] != "off" {
		t.Errorf("reconcile path: BOAT_ULTRACODE got %q, want off", spec.Env[EnvUltracode])
	}

	// Idempotent: a second reconcile with the same metadata keeps it stable.
	spec2 := BuildSpecFromBeadInfo(cfg, "gardenio", "crew", "crew", "agent1", map[string]string{"ultracode": "off"})
	if spec2.Env[EnvUltracode] != "off" {
		t.Errorf("reconcile path (2nd): BOAT_ULTRACODE got %q, want off", spec2.Env[EnvUltracode])
	}

	// Absent metadata leaves it unset on the reconcile path too.
	spec3 := BuildSpecFromBeadInfo(cfg, "gardenio", "crew", "crew", "agent1", map[string]string{})
	if got, present := spec3.Env[EnvUltracode]; present {
		t.Errorf("reconcile path: expected BOAT_ULTRACODE unset with no metadata, got %q", got)
	}
}

// boolPtr is a tiny helper for the tri-state *bool config layers.
func boolPtr(b bool) *bool { return &b }

// TestResolveUltracode_Precedence exercises the project < role < agent
// precedence chain added in ka-0000001714. Ultracode is tri-state, so an unset
// layer must be SKIPPED (never read as off) and a more-specific layer's explicit
// choice always wins.
func TestResolveUltracode_Precedence(t *testing.T) {
	tests := []struct {
		name      string
		projectUC *bool  // ProjectCacheEntry.Ultracode
		roleUC    *bool  // RoleCacheEntry.Ultracode
		agentMeta string // metadata["ultracode"] ("on"/"off"/"" etc.)
		want      string // "" means BOAT_ULTRACODE must be absent
	}{
		{"all unset", nil, nil, "", ""},
		{"project on", boolPtr(true), nil, "", "on"},
		{"project off", boolPtr(false), nil, "", "off"},
		{"role on beats unset project", nil, boolPtr(true), "", "on"},
		// The ka-0000001714 motivating case: a role opts its default agents in
		// even though the project is silent.
		{"role on, project unset (microvms role)", nil, boolPtr(true), "", "on"},
		{"role overrides project (off over on)", boolPtr(true), boolPtr(false), "", "off"},
		{"role overrides project (on over off)", boolPtr(false), boolPtr(true), "", "on"},
		// Unset role must NOT clobber the project layer.
		{"unset role keeps project on", boolPtr(true), nil, "", "on"},
		// Agent metadata (--ultracode flag) is highest precedence.
		{"agent on beats role off", nil, boolPtr(false), "on", "on"},
		{"agent off beats role on", nil, boolPtr(true), "off", "off"},
		{"agent on beats project off", boolPtr(false), nil, "on", "on"},
		// Unrecognized agent metadata falls through to the resolved layers.
		{"unrecognized agent meta keeps role on", nil, boolPtr(true), "maybe", "on"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{
				ProjectCache: map[string]config.ProjectCacheEntry{
					"gardenio": {Account: "gardenio", Ultracode: tc.projectUC},
				},
				RoleCache: map[string]config.RoleCacheEntry{
					"crew": {Ultracode: tc.roleUC},
				},
				AccountCache: map[string]config.AccountCacheEntry{
					"gardenio": {Provider: "anthropic", Model: "account-default-model", LiteLLMKey: "sk"},
				},
			}
			meta := map[string]string{}
			if tc.agentMeta != "" {
				meta["ultracode"] = tc.agentMeta
			}
			// Exercise via the full events-path builder so the wiring (call site +
			// lock discipline + env bake) is covered end-to-end, not just resolve.
			spec := buildAgentPodSpec(cfg, subscriber.Event{
				Project: "gardenio", Role: "crew", AgentName: "agent1", Metadata: meta,
			})
			got, present := spec.Env[EnvUltracode]
			if tc.want == "" {
				if present {
					t.Errorf("expected BOAT_ULTRACODE unset, got %q", got)
				}
				return
			}
			if got != tc.want {
				t.Errorf("BOAT_ULTRACODE: got %q (present=%v), want %q", got, present, tc.want)
			}
		})
	}
}

// TestResolveUltracode_MultiRoleFirstWins mirrors resolveExecutionTarget's
// multi-role iteration: the first comma-separated role that sets a value wins.
func TestResolveUltracode_MultiRoleFirstWins(t *testing.T) {
	cfg := &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{
			"gardenio": {Account: "gardenio"},
		},
		RoleCache: map[string]config.RoleCacheEntry{
			"crew":     {}, // unset — skipped
			"microvms": {Ultracode: boolPtr(true)},
		},
		AccountCache: map[string]config.AccountCacheEntry{
			"gardenio": {Provider: "anthropic", Model: "account-default-model", LiteLLMKey: "sk"},
		},
	}
	spec := buildAgentPodSpec(cfg, subscriber.Event{
		Project: "gardenio", Role: "crew,microvms", AgentName: "agent1", Metadata: map[string]string{},
	})
	if spec.Env[EnvUltracode] != "on" {
		t.Errorf("multi-role: BOAT_ULTRACODE got %q, want on", spec.Env[EnvUltracode])
	}
}
