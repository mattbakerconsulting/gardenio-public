package main

import (
	"strings"
	"testing"

	"gardenio/controller/internal/config"
	"gardenio/controller/internal/podmanager"
)

func claudeAcct() config.AccountCacheEntry {
	return config.AccountCacheEntry{Provider: "anthropic", Credential: "secret:gardenio-claude-oauth", Status: "active", Labels: []string{"project:gardenio"}}
}

func TestAccountMatchesHarness(t *testing.T) {
	openai, anthropic := codexAcct(), claudeAcct()
	bedrock := config.AccountCacheEntry{Provider: "bedrock"}
	unset := config.AccountCacheEntry{}
	tests := []struct {
		name    string
		acct    config.AccountCacheEntry
		harness string
		want    bool
	}{
		{"codex wants openai", openai, "codex", true},
		{"codex rejects anthropic", anthropic, "codex", false},
		{"codex rejects bedrock", bedrock, "codex", false},
		{"codex rejects unset provider", unset, "codex", false},
		{"claude rejects openai", openai, "claude", false},
		{"claude takes anthropic", anthropic, "claude", true},
		{"claude takes bedrock", bedrock, "claude", true},
		{"claude takes unset provider", unset, "claude", true},
		{"unresolved harness is claude", openai, "", false},
		{"unresolved harness takes anthropic", anthropic, "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := accountMatchesHarness(tc.acct, tc.harness); got != tc.want {
				t.Errorf("accountMatchesHarness = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIsHealthyAuthorizedFor(t *testing.T) {
	healthy := codexAcct()
	if !isHealthyAuthorizedFor(healthy, "codex") {
		t.Error("healthy openai account should be fit for codex")
	}
	if isHealthyAuthorizedFor(healthy, "claude") {
		t.Error("openai account must not be fit for claude")
	}
	dead := codexAcct()
	dead.ProbeAuthStatus = "rejected"
	if isHealthyAuthorizedFor(dead, "codex") {
		t.Error("auth-rejected openai account must not be fit for codex")
	}
}

// A codex spec only ever sees provider=openai accounts, and picks the least
// loaded of them; a claude spec never sees one.
func TestSelectAccount_FiltersByHarness(t *testing.T) {
	codexB := codexAcct()
	cfg := &config.Config{AccountCache: map[string]config.AccountCacheEntry{
		"claude-a": claudeAcct(),
		"codex-a":  codexAcct(),
		"codex-b":  codexB,
	}}
	counts := map[string]int{"claude-a": 0, "codex-a": 9, "codex-b": 2}

	codexSpec := &podmanager.AgentPodSpec{Project: "gardenio", Role: "crew", Harness: "codex"}
	if got := selectAccount(cfg, "gardenio", codexSpec, counts); got != "codex-b" {
		t.Errorf("codex selectAccount = %q, want codex-b (least-loaded openai account, never claude-a)", got)
	}
	claudeSpec := &podmanager.AgentPodSpec{Project: "gardenio", Role: "crew", Harness: "claude"}
	if got := selectAccount(cfg, "gardenio", claudeSpec, counts); got != "claude-a" {
		t.Errorf("claude selectAccount = %q, want claude-a", got)
	}
	unsetSpec := &podmanager.AgentPodSpec{Project: "gardenio", Role: "crew"}
	if got := selectAccount(cfg, "gardenio", unsetSpec, counts); got != "claude-a" {
		t.Errorf("unresolved-harness selectAccount = %q, want claude-a", got)
	}
}

// Provider filtering happens before health: a codex spec with only unhealthy
// openai accounts gets nothing, never a healthy Claude account.
func TestSelectAccount_CodexNeverFallsBackToClaude(t *testing.T) {
	dead := codexAcct()
	dead.Status = "exhausted"
	cfg := &config.Config{AccountCache: map[string]config.AccountCacheEntry{
		"claude-a": claudeAcct(),
		"codex-a":  dead,
	}}
	spec := &podmanager.AgentPodSpec{Project: "gardenio", Role: "crew", Harness: "codex"}
	if got := selectAccount(cfg, "gardenio", spec, nil); got != "" {
		t.Errorf("selectAccount = %q, want empty (no healthy openai account)", got)
	}
	noOpenAI := &config.Config{AccountCache: map[string]config.AccountCacheEntry{"claude-a": claudeAcct()}}
	if got := selectAccount(noOpenAI, "gardenio", spec, nil); got != "" {
		t.Errorf("selectAccount = %q, want empty (project has no openai account)", got)
	}
}

// The served-model gate describes LiteLLM's models; a subscription-authed codex
// account is not routed through it and must not be excluded by it.
func TestSelectAccount_CodexIgnoresServedModelGate(t *testing.T) {
	cfg := &config.Config{
		LiteLLMURL:                   "http://litellm.example",
		SpreadServedModelGateEnabled: true,
		AccountCache:                 map[string]config.AccountCacheEntry{"codex-a": codexAcct()},
	}
	cfg.SetServedModels(map[string]bool{"some-other-model": true})
	spec := &podmanager.AgentPodSpec{Project: "gardenio", Role: "crew", Harness: "codex"}
	if got := selectAccount(cfg, "gardenio", spec, nil); got != "codex-a" {
		t.Errorf("selectAccount = %q, want codex-a", got)
	}
}

func TestCountCredentialedProjectAccounts_ByHarness(t *testing.T) {
	cfg := &config.Config{AccountCache: map[string]config.AccountCacheEntry{
		"claude-a": claudeAcct(), "claude-b": claudeAcct(), "codex-a": codexAcct(),
	}}
	for harness, want := range map[string]int{"": 2, "claude": 2, "codex": 1} {
		if got := countCredentialedProjectAccounts(cfg, "gardenio", harness); got != want {
			t.Errorf("harness %q: count = %d, want %d", harness, got, want)
		}
	}
}

// applyAccountConfig resolves the harness before selecting, so an unpinned codex
// role is credentialed from the openai pool and an unpinned claude role from the
// claude pool, from the same project.
func TestApplyAccountConfig_SelectionFollowsHarness(t *testing.T) {
	newCfg := func() *config.Config {
		return &config.Config{
			RoleCache: map[string]config.RoleCacheEntry{
				"codex-crew": {Harness: "codex"},
				"crew":       {},
			},
			AccountCache: map[string]config.AccountCacheEntry{"claude-a": claudeAcct(), "codex-a": codexAcct()},
		}
	}
	codexSpec := &podmanager.AgentPodSpec{Project: "gardenio", Role: "codex-crew", AgentName: "a"}
	applyAccountConfig(newCfg(), codexSpec)
	if codexSpec.Harness != "codex" || codexSpec.ResolvedAccount != "codex-a" {
		t.Errorf("codex role: Harness=%q ResolvedAccount=%q, want codex/codex-a", codexSpec.Harness, codexSpec.ResolvedAccount)
	}
	if codexSpec.CodexAuthSecret == "" {
		t.Error("codex role should mount the codex -session Secret")
	}

	claudeSpec := &podmanager.AgentPodSpec{Project: "gardenio", Role: "crew", AgentName: "a"}
	applyAccountConfig(newCfg(), claudeSpec)
	if claudeSpec.ResolvedAccount != "claude-a" || claudeSpec.CodexAuthSecret != "" {
		t.Errorf("claude role: ResolvedAccount=%q CodexAuthSecret=%q, want claude-a and none", claudeSpec.ResolvedAccount, claudeSpec.CodexAuthSecret)
	}
}

// An exhausted codex pin spills to another healthy openai account, never to a
// Claude one.
func TestApplyAccountConfig_CodexPinSpillsToOpenAIPool(t *testing.T) {
	pinned := codexAcct()
	pinned.Status = "exhausted"
	cfg := &config.Config{
		RoleCache: map[string]config.RoleCacheEntry{"codex-crew": {Account: "codex-pin", Harness: "codex"}},
		AccountCache: map[string]config.AccountCacheEntry{
			"codex-pin": pinned, "codex-b": codexAcct(), "claude-a": claudeAcct(),
		},
	}
	spec := &podmanager.AgentPodSpec{Project: "gardenio", Role: "codex-crew", AgentName: "a"}
	applyAccountConfig(cfg, spec)
	if spec.ResolvedAccount != "codex-b" {
		t.Errorf("ResolvedAccount = %q (blocked=%v %q), want spill to codex-b", spec.ResolvedAccount, spec.SpawnBlocked, spec.SpawnBlockReason)
	}
}

// A codex project whose openai accounts are all exhausted blocks the spawn
// transiently instead of baking a Claude credential.
func TestApplyAccountConfig_CodexAllExhaustedBlocks(t *testing.T) {
	dead := codexAcct()
	dead.Status = "exhausted"
	cfg := &config.Config{
		RoleCache:    map[string]config.RoleCacheEntry{"codex-crew": {Harness: "codex"}},
		AccountCache: map[string]config.AccountCacheEntry{"codex-a": dead, "claude-a": claudeAcct()},
	}
	spec := &podmanager.AgentPodSpec{Project: "gardenio", Role: "codex-crew", AgentName: "a"}
	applyAccountConfig(cfg, spec)
	if spec.ResolvedAccount == "claude-a" {
		t.Fatal("codex spec was credentialed with a Claude account")
	}
	if !spec.SpawnBlocked || !strings.Contains(spec.SpawnBlockReason, "credentialed account") {
		t.Errorf("want transient SpawnBlocked, got blocked=%v reason=%q", spec.SpawnBlocked, spec.SpawnBlockReason)
	}
}
