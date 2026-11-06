package main

import (
	"testing"

	"gardenio/controller/internal/config"
	"gardenio/controller/internal/podmanager"
	"gardenio/controller/internal/subscriber"
)

// Model-override precedence: global < project < role < agent (epic
// ka-0000005488). The account's default model is the base; a project, role, or
// per-agent override supersedes it so a single account (one OAuth cred, one
// LiteLLM key allowing N models) can back fable/opus/sonnet without cloning
// account beads.

// newModelCfg builds a config with one account ("gardenio") whose default model
// is the account base, plus optional project/role model overrides.
func newModelCfg(projectModel, roleModel string) *config.Config {
	cfg := &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{
			"gardenio": {Account: "gardenio", Model: projectModel},
		},
		RoleCache: map[string]config.RoleCacheEntry{
			"crew": {Model: roleModel},
		},
		AccountCache: map[string]config.AccountCacheEntry{
			"gardenio": {
				Provider:   "anthropic",
				BaseURL:    "http://litellm:4000",
				Credential: "secret:gardenio-oauth",
				Model:      "account-default-model",
				LiteLLMKey: "sk-key",
				Labels:     []string{"project:gardenio"},
			},
		},
	}
	return cfg
}

// assertModel checks both ANTHROPIC_MODEL and CLAUDE_MODEL equal want, and that
// the BOAT_MODEL_PINNED pin is present iff an override (non-account) is in play.
func assertModel(t *testing.T, spec podmanager.AgentPodSpec, want string, wantPinned bool) {
	t.Helper()
	if spec.Env["ANTHROPIC_MODEL"] != want {
		t.Errorf("ANTHROPIC_MODEL: got %q, want %q", spec.Env["ANTHROPIC_MODEL"], want)
	}
	if spec.Env["CLAUDE_MODEL"] != want {
		t.Errorf("CLAUDE_MODEL: got %q, want %q", spec.Env["CLAUDE_MODEL"], want)
	}
	pin, hasPin := spec.Env[EnvModelPinned]
	if wantPinned {
		if !hasPin || pin != want {
			t.Errorf("BOAT_MODEL_PINNED: got %q (present=%v), want %q", pin, hasPin, want)
		}
		if spec.ResolvedModel != want {
			t.Errorf("ResolvedModel: got %q, want %q", spec.ResolvedModel, want)
		}
	} else {
		if hasPin {
			t.Errorf("BOAT_MODEL_PINNED: expected unset, got %q", pin)
		}
		if spec.ResolvedModel != "" {
			t.Errorf("ResolvedModel: expected empty, got %q", spec.ResolvedModel)
		}
	}
}

func eventForModel(meta map[string]string) subscriber.Event {
	if meta == nil {
		meta = map[string]string{}
	}
	return subscriber.Event{
		Project:   "gardenio",
		Role:      "crew",
		AgentName: "agent1",
		Metadata:  meta,
	}
}

// TestModelOverride_AccountOnly: no project/role/agent override → the account
// default model is used and no pin is stamped (rotation may freely re-derive it).
func TestModelOverride_AccountOnly(t *testing.T) {
	cfg := newModelCfg("", "")
	spec := buildAgentPodSpec(cfg, eventForModel(nil))
	assertModel(t, spec, "account-default-model", false)
}

// TestModelOverride_Project: project model supersedes the account default.
func TestModelOverride_Project(t *testing.T) {
	cfg := newModelCfg("project-model", "")
	spec := buildAgentPodSpec(cfg, eventForModel(nil))
	assertModel(t, spec, "project-model", true)
}

// TestModelOverride_Role: role model supersedes both project and account.
func TestModelOverride_Role(t *testing.T) {
	cfg := newModelCfg("project-model", "role-model")
	spec := buildAgentPodSpec(cfg, eventForModel(nil))
	assertModel(t, spec, "role-model", true)
}

// TestModelOverride_AccountBoundRole is the podspec-level guard for bug
// ka-0000004910: a role that sets BOTH a model override AND pins an account
// (account default = opus) must resolve to the role's model, with the pin set,
// not to the account default. applyCommonConfig runs applyAccountConfig (which
// stamps ANTHROPIC_MODEL from the pinned account) THEN applyModelOverride, so
// the role model must win end-to-end. This is the invariant the shadowed
// haiku-pinned gardener-delivery bead should have satisfied once un-shadowed.
func TestModelOverride_AccountBoundRole(t *testing.T) {
	cfg := &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{
			"gardenio": {Account: "gardenio"},
		},
		RoleCache: map[string]config.RoleCacheEntry{
			// The gardener-delivery shape: an account-bound role that also pins a
			// model. The account it pins defaults to opus; the role model is haiku.
			"gardener-delivery": {Account: "opus-account", Model: "role-haiku-model"},
		},
		AccountCache: map[string]config.AccountCacheEntry{
			"gardenio":     {Provider: "anthropic", Model: "account-default-model", LiteLLMKey: "sk"},
			"opus-account": {Provider: "anthropic", Model: "opus-account-default", LiteLLMKey: "sk"},
		},
	}
	ev := subscriber.Event{Project: "gardenio", Role: "gardener-delivery", AgentName: "cd1", Metadata: map[string]string{}}
	spec := buildAgentPodSpec(cfg, ev)
	// The role model wins over the pinned account's opus default, and the pin is
	// stamped so a coopmux credential reload can't revert it to the account default.
	assertModel(t, spec, "role-haiku-model", true)

	// Same invariant via the reconciler/restart path.
	spec = BuildSpecFromBeadInfo(cfg, "gardenio", "gardener-delivery", "gardener-delivery", "cd1", map[string]string{})
	assertModel(t, spec, "role-haiku-model", true)
}

// TestModelOverride_Agent: per-agent metadata model wins over everything.
func TestModelOverride_Agent(t *testing.T) {
	cfg := newModelCfg("project-model", "role-model")
	spec := buildAgentPodSpec(cfg, eventForModel(map[string]string{"model": "agent-model"}))
	assertModel(t, spec, "agent-model", true)
}

// TestModelOverride_AgentBeatsAccountWithoutRoleProject: agent override applies
// even when no project/role override exists, still beating the account default.
func TestModelOverride_AgentBeatsAccountWithoutRoleProject(t *testing.T) {
	cfg := newModelCfg("", "")
	spec := buildAgentPodSpec(cfg, eventForModel(map[string]string{"model": "agent-model"}))
	assertModel(t, spec, "agent-model", true)
}

// TestModelOverride_ReconcilerPath: BuildSpecFromBeadInfo (the reconciler/restart
// path) must apply the same precedence so a restart never reverts the override.
func TestModelOverride_ReconcilerPath(t *testing.T) {
	cfg := newModelCfg("project-model", "role-model")

	// Role override via the reconciler path.
	spec := BuildSpecFromBeadInfo(cfg, "gardenio", "crew", "crew", "agent1", map[string]string{})
	assertModel(t, spec, "role-model", true)

	// Agent override via the reconciler path.
	spec = BuildSpecFromBeadInfo(cfg, "gardenio", "crew", "crew", "agent1", map[string]string{"model": "agent-model"})
	assertModel(t, spec, "agent-model", true)
}

// TestModelOverride_MultiRoleFirstMatch: for a multi-role agent ("a,b"), the
// first role in the list with a non-empty model wins (mirrors resolveRoleAccount).
func TestModelOverride_MultiRoleFirstMatch(t *testing.T) {
	cfg := &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{
			"gardenio": {Account: "gardenio"},
		},
		RoleCache: map[string]config.RoleCacheEntry{
			"thread": {}, // no model
			"crew":   {Model: "crew-model"},
		},
		AccountCache: map[string]config.AccountCacheEntry{
			"gardenio": {Provider: "anthropic", Model: "account-default-model", LiteLLMKey: "sk", Labels: []string{"project:gardenio"}},
		},
	}
	ev := subscriber.Event{Project: "gardenio", Role: "thread,crew", AgentName: "a", Metadata: map[string]string{}}
	spec := buildAgentPodSpec(cfg, ev)
	assertModel(t, spec, "crew-model", true)
}

// TestResolveModelOverride_Precedence unit-tests the resolver directly.
func TestResolveModelOverride_Precedence(t *testing.T) {
	tests := []struct {
		name         string
		projectModel string
		roleModel    string
		want         string
	}{
		{"neither", "", "", ""},
		{"project only", "p", "", "p"},
		{"role only", "", "r", "r"},
		{"role beats project", "p", "r", "r"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := newModelCfg(tc.projectModel, tc.roleModel)
			got := resolveModelOverride(cfg, "gardenio", "crew")
			if got != tc.want {
				t.Errorf("resolveModelOverride: got %q, want %q", got, tc.want)
			}
		})
	}
}
