package main

import (
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/config"
	"gardenio/controller/internal/podmanager"
	"gardenio/controller/internal/subscriber"
)

func TestOperatedByFromMetadata(t *testing.T) {
	tests := []struct {
		name string
		meta map[string]string
		want string
	}{
		{"empty", map[string]string{}, ""},
		{"slack only", map[string]string{"slack_user_id": "U123"}, "slack:U123"},
		{"slack with whitespace", map[string]string{"slack_user_id": "  U123  "}, "slack:U123"},
		{"explicit operated_by wins", map[string]string{"operated_by": "oidc:user@x.com", "slack_user_id": "U123"}, "oidc:user@x.com"},
		{"explicit only", map[string]string{"operated_by": "slack:U999"}, "slack:U999"},
		{"empty values ignored", map[string]string{"operated_by": "", "slack_user_id": "  "}, ""},
		// ka-0000005841: sentinel-class operators must not flow into BEADS_OPERATED_BY.
		// The server rejects them as header_non_human; falling through to slack_user_id
		// or empty lets server-side fallback / chain-walk find a real human.
		{"sentinel system:* dropped, slack fallback", map[string]string{"operated_by": "system:legacy-pre-attribution", "slack_user_id": "U123"}, "slack:U123"},
		{"sentinel system:* dropped, no slack", map[string]string{"operated_by": "system:legacy-pre-attribution"}, ""},
		{"sentinel system:scheduler dropped", map[string]string{"operated_by": "system:scheduler"}, ""},
		{"sentinel fleet:* dropped, slack fallback", map[string]string{"operated_by": "fleet:jira-bridge", "slack_user_id": "U999"}, "slack:U999"},
		{"sentinel fleet:* dropped, no slack", map[string]string{"operated_by": "fleet:jira-bridge"}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := operatedByFromMetadata(tc.meta)
			if got != tc.want {
				t.Errorf("operatedByFromMetadata(%v) = %q, want %q", tc.meta, got, tc.want)
			}
		})
	}
}

func TestModeForRole(t *testing.T) {
	tests := []struct {
		mode string
		role string
		want string
	}{
		{"", "gardener", "crew"},
		{"", "crew", "crew"},
		{"", "job", "job"},
		{"", "polecat", "job"},
		{"", "unknown", "crew"},
		{"crew", "polecat", "crew"},     // explicit mode takes precedence
		{"job", "crew", "job"},          // explicit mode takes precedence
		{"", "thread,crew", "crew"},     // multi-role: first role determines mode
		{"", "job,crew", "job"},         // multi-role: first role is job
		{"", "gardener,thread", "crew"}, // multi-role: first role is gardener
		{"crew", "job,polecat", "crew"}, // explicit mode takes precedence over multi-role
	}
	for _, tc := range tests {
		got := modeForRole(tc.mode, tc.role)
		if got != tc.want {
			t.Errorf("modeForRole(%q, %q) = %q, want %q", tc.mode, tc.role, got, tc.want)
		}
	}
}

func TestOverrideOrAppendSecretEnv_OverridesExisting(t *testing.T) {
	envs := []podmanager.SecretEnvSource{
		{EnvName: "GITHUB_TOKEN", SecretName: "global-gh", SecretKey: "token"},
		{EnvName: "OTHER_SECRET", SecretName: "other", SecretKey: "key"},
	}
	src := podmanager.SecretEnvSource{
		EnvName: "GITHUB_TOKEN", SecretName: "project-gh", SecretKey: "my-token",
	}
	overrideOrAppendSecretEnv(&envs, src)

	if len(envs) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(envs))
	}
	if envs[0].SecretName != "project-gh" {
		t.Errorf("expected SecretName project-gh, got %s", envs[0].SecretName)
	}
	if envs[0].SecretKey != "my-token" {
		t.Errorf("expected SecretKey my-token, got %s", envs[0].SecretKey)
	}
}

func TestOverrideOrAppendSecretEnv_AppendsNew(t *testing.T) {
	envs := []podmanager.SecretEnvSource{
		{EnvName: "GITHUB_TOKEN", SecretName: "global-gh", SecretKey: "token"},
	}
	src := podmanager.SecretEnvSource{
		EnvName: "JIRA_API_TOKEN", SecretName: "proj-jira", SecretKey: "api-token",
	}
	overrideOrAppendSecretEnv(&envs, src)

	if len(envs) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(envs))
	}
	if envs[1].EnvName != "JIRA_API_TOKEN" {
		t.Errorf("expected JIRA_API_TOKEN, got %s", envs[1].EnvName)
	}
}

func TestRepoNameFromURL(t *testing.T) {
	tests := []struct {
		url  string
		want string
	}{
		{"https://github.com/org/my-repo.git", "my-repo"},
		{"https://github.com/org/my-repo", "my-repo"},
		{"https://gitlab.com/Acme/Platform/monorepo", "monorepo"},
		{"https://gitlab.com/Acme/Platform/monorepo.git", "monorepo"},
		{"repo", "repo"},
	}
	for _, tc := range tests {
		got := repoNameFromURL(tc.url)
		if got != tc.want {
			t.Errorf("repoNameFromURL(%q) = %q, want %q", tc.url, got, tc.want)
		}
	}
}

func TestApplyCommonConfig_PerProjectSecrets(t *testing.T) {
	cfg := &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{
			"myproject": {
				Secrets: []beadsapi.SecretEntry{
					{Env: "GITHUB_TOKEN", Secret: "myproject-gh-token", Key: "my-token"},
				},
			},
		},
	}
	spec := &podmanager.AgentPodSpec{
		Project: "myproject",
		Env:     map[string]string{},
	}
	applyCommonConfig(cfg, spec)

	found := false
	for _, se := range spec.SecretEnv {
		if se.EnvName == "GITHUB_TOKEN" {
			found = true
			if se.SecretName != "myproject-gh-token" {
				t.Errorf("expected SecretName myproject-gh-token, got %s", se.SecretName)
			}
			if se.SecretKey != "my-token" {
				t.Errorf("expected SecretKey my-token, got %s", se.SecretKey)
			}
		}
	}
	if !found {
		t.Error("GITHUB_TOKEN not found in SecretEnv")
	}
}

func TestApplyCommonConfig_PerProjectMultipleSecrets(t *testing.T) {
	cfg := &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{
			"myproject": {
				Secrets: []beadsapi.SecretEntry{
					{Env: "GITHUB_TOKEN", Secret: "myproject-gh-token", Key: "token"},
					{Env: "JIRA_API_TOKEN", Secret: "myproject-jira", Key: "api-token"},
				},
			},
		},
	}
	spec := &podmanager.AgentPodSpec{
		Project: "myproject",
		Env:     map[string]string{},
	}
	applyCommonConfig(cfg, spec)

	envNames := map[string]bool{}
	for _, se := range spec.SecretEnv {
		envNames[se.EnvName] = true
	}
	if !envNames["GITHUB_TOKEN"] {
		t.Error("expected GITHUB_TOKEN from project config")
	}
	if !envNames["JIRA_API_TOKEN"] {
		t.Error("expected JIRA_API_TOKEN from project config")
	}
}

func TestApplyCommonConfig_GitCredentialFromProjectBead(t *testing.T) {
	cfg := &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{
			"myproject": {
				Secrets: []beadsapi.SecretEntry{
					{Env: "GIT_TOKEN", Secret: "myproject-git-creds", Key: "token"},
				},
			},
		},
	}
	spec := &podmanager.AgentPodSpec{
		Project: "myproject",
		Env:     map[string]string{},
	}
	applyCommonConfig(cfg, spec)

	if spec.GitCredentialsSecret != "myproject-git-creds" {
		t.Errorf("expected GitCredentialsSecret myproject-git-creds, got %s", spec.GitCredentialsSecret)
	}
}

func TestApplyCommonConfig_NoProjectSecrets(t *testing.T) {
	cfg := &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{},
	}
	spec := &podmanager.AgentPodSpec{
		Project: "myproject",
		Env:     map[string]string{},
	}
	applyCommonConfig(cfg, spec)

	if len(spec.SecretEnv) != 0 {
		t.Errorf("expected 0 SecretEnv, got %d", len(spec.SecretEnv))
	}
}

func TestApplyCommonConfig_MultiRepo(t *testing.T) {
	cfg := &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{
			"myproject": {
				Repos: []beadsapi.RepoEntry{
					{URL: "https://github.com/org/main-repo.git", Branch: "develop", Role: "primary"},
					{URL: "https://github.com/org/shared-lib.git", Role: "reference", Name: "shared-lib"},
					{URL: "https://github.com/org/other.git", Role: "reference"},
				},
			},
		},
	}
	spec := &podmanager.AgentPodSpec{
		Project: "myproject",
		Env:     map[string]string{},
	}
	applyCommonConfig(cfg, spec)

	if spec.GitURL != "https://github.com/org/main-repo.git" {
		t.Errorf("expected primary GitURL, got %s", spec.GitURL)
	}
	if spec.GitDefaultBranch != "develop" {
		t.Errorf("expected develop branch, got %s", spec.GitDefaultBranch)
	}
	if len(spec.ReferenceRepos) != 2 {
		t.Fatalf("expected 2 reference repos, got %d", len(spec.ReferenceRepos))
	}
	if spec.ReferenceRepos[0].Name != "shared-lib" {
		t.Errorf("expected shared-lib, got %s", spec.ReferenceRepos[0].Name)
	}
	if spec.ReferenceRepos[1].Name != "other" {
		t.Errorf("expected other (derived from URL), got %s", spec.ReferenceRepos[1].Name)
	}

	refRepos := spec.Env["BOAT_REFERENCE_REPOS"]
	if refRepos == "" {
		t.Fatal("expected BOAT_REFERENCE_REPOS to be set")
	}
}

func TestApplyCommonConfig_LegacySingleRepo(t *testing.T) {
	cfg := &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{
			"myproject": {
				GitURL:        "https://github.com/org/legacy.git",
				DefaultBranch: "master",
			},
		},
	}
	spec := &podmanager.AgentPodSpec{
		Project: "myproject",
		Env:     map[string]string{},
	}
	applyCommonConfig(cfg, spec)

	if spec.GitURL != "https://github.com/org/legacy.git" {
		t.Errorf("expected legacy GitURL, got %s", spec.GitURL)
	}
	if spec.GitDefaultBranch != "master" {
		t.Errorf("expected master branch, got %s", spec.GitDefaultBranch)
	}
	if len(spec.ReferenceRepos) != 0 {
		t.Errorf("expected no reference repos, got %d", len(spec.ReferenceRepos))
	}
}

func TestApplyCommonConfig_RejectsSecretWithWrongPrefix(t *testing.T) {
	cfg := &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{
			"myproject": {
				Secrets: []beadsapi.SecretEntry{
					{Env: "VALID_TOKEN", Secret: "myproject-creds", Key: "token"},
					{Env: "INVALID_TOKEN", Secret: "acme-jira", Key: "api-token"},
					{Env: "BAD_SECRET", Secret: "shared-secret", Key: "key"},
				},
			},
		},
	}
	spec := &podmanager.AgentPodSpec{
		Project: "myproject",
		Env:     map[string]string{},
	}
	applyCommonConfig(cfg, spec)

	envNames := map[string]bool{}
	for _, se := range spec.SecretEnv {
		envNames[se.EnvName] = true
	}
	if !envNames["VALID_TOKEN"] {
		t.Error("expected VALID_TOKEN to be present (valid prefix)")
	}
	if envNames["INVALID_TOKEN"] {
		t.Error("expected INVALID_TOKEN to be skipped (wrong prefix)")
	}
	if envNames["BAD_SECRET"] {
		t.Error("expected BAD_SECRET to be skipped (wrong prefix)")
	}
}

func TestApplyCommonConfig_ReferenceOnlyRepos(t *testing.T) {
	cfg := &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{
			"myproject": {
				Repos: []beadsapi.RepoEntry{
					{URL: "https://github.com/org/ref1.git", Role: "reference", Name: "ref1"},
					{URL: "https://github.com/org/ref2.git", Role: "reference", Name: "ref2"},
				},
			},
		},
	}
	spec := &podmanager.AgentPodSpec{
		Project: "myproject",
		Env:     map[string]string{},
	}
	applyCommonConfig(cfg, spec)

	if spec.GitURL != "" {
		t.Errorf("expected empty GitURL, got %s", spec.GitURL)
	}
	if len(spec.ReferenceRepos) != 2 {
		t.Fatalf("expected 2 reference repos, got %d", len(spec.ReferenceRepos))
	}
}

func TestApplyCommonConfig_SlackBridgeURL(t *testing.T) {
	cfg := &config.Config{
		SlackBridgeURL: "http://gardenio-slack-bridge:8090",
		ProjectCache:   map[string]config.ProjectCacheEntry{},
	}
	spec := &podmanager.AgentPodSpec{
		Project: "myproject",
		Env:     map[string]string{},
	}
	applyCommonConfig(cfg, spec)

	if spec.Env["SLACK_BRIDGE_URL"] != "http://gardenio-slack-bridge:8090" {
		t.Errorf("expected SLACK_BRIDGE_URL, got %q", spec.Env["SLACK_BRIDGE_URL"])
	}
}

func TestApplyCommonConfig_BeadsViewerInternalURL(t *testing.T) {
	const internal = "http://gardenio-beads-viewer.gardenio-system.svc.cluster.local:8095"
	cfg := &config.Config{
		BeadsViewerInternalURL: internal,
		ProjectCache:           map[string]config.ProjectCacheEntry{},
	}
	spec := &podmanager.AgentPodSpec{
		Project: "myproject",
		Env:     map[string]string{},
	}
	applyCommonConfig(cfg, spec)

	if spec.Env["BEADS_VIEWER_INTERNAL_URL"] != internal {
		t.Errorf("expected BEADS_VIEWER_INTERNAL_URL=%q, got %q", internal, spec.Env["BEADS_VIEWER_INTERNAL_URL"])
	}
}

func TestApplyCommonConfig_BeadsViewerInternalURL_OmittedWhenUnset(t *testing.T) {
	cfg := &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{},
	}
	spec := &podmanager.AgentPodSpec{
		Project: "myproject",
		Env:     map[string]string{},
	}
	applyCommonConfig(cfg, spec)

	if _, present := spec.Env["BEADS_VIEWER_INTERNAL_URL"]; present {
		t.Errorf("expected BEADS_VIEWER_INTERNAL_URL to be omitted when unset, got %q", spec.Env["BEADS_VIEWER_INTERNAL_URL"])
	}
}

// A role bead's env (applyRoleSecrets, called after the common injection) must
// still win over the universal default so a role can point at a non-derivable
// viewer layout (ka-0000000379).
func TestApplyCommonConfig_BeadsViewerInternalURL_RoleOverrides(t *testing.T) {
	const universal = "http://gardenio-beads-viewer.gardenio-system.svc.cluster.local:8095"
	const roleOverride = "http://custom-viewer.other-ns.svc.cluster.local:8095"
	cfg := &config.Config{
		BeadsViewerInternalURL: universal,
		ProjectCache:           map[string]config.ProjectCacheEntry{},
		RoleCache: map[string]config.RoleCacheEntry{
			"plan-gardener": {
				EnvVars: []beadsapi.EnvEntry{
					{Name: "BEADS_VIEWER_INTERNAL_URL", Value: roleOverride},
				},
			},
		},
	}
	spec := &podmanager.AgentPodSpec{
		Project: "myproject",
		Role:    "plan-gardener",
		Env:     map[string]string{},
	}
	applyCommonConfig(cfg, spec)

	if spec.Env["BEADS_VIEWER_INTERNAL_URL"] != roleOverride {
		t.Errorf("expected role env to override, got %q", spec.Env["BEADS_VIEWER_INTERNAL_URL"])
	}
}

func TestApplyCommonConfig_GardenioChannel(t *testing.T) {
	cfg := &config.Config{
		GardenioChannel: true,
		ProjectCache:    map[string]config.ProjectCacheEntry{},
	}
	spec := &podmanager.AgentPodSpec{
		Project: "myproject",
		Env:     map[string]string{},
	}
	applyCommonConfig(cfg, spec)

	if spec.Env["GARDENIO_CHANNEL"] != "1" {
		t.Errorf("expected GARDENIO_CHANNEL=1, got %q", spec.Env["GARDENIO_CHANNEL"])
	}
}

func TestApplyCommonConfig_GardenioChannelDisabled(t *testing.T) {
	cfg := &config.Config{
		GardenioChannel: false,
		ProjectCache:    map[string]config.ProjectCacheEntry{},
	}
	spec := &podmanager.AgentPodSpec{
		Project: "myproject",
		Env:     map[string]string{},
	}
	applyCommonConfig(cfg, spec)

	if _, ok := spec.Env["GARDENIO_CHANNEL"]; ok {
		t.Error("expected GARDENIO_CHANNEL to not be set when disabled")
	}
}

// TestApplyCommonConfig_NatsForwardInjected guards the load-bearing precondition
// for the plan-chat transcript pipeline (task 2a + ka-0000007237 durable replay):
// coop only publishes assistant_text/thinking/usage to NATS when COOP_NATS_FORWARD
// is set, and the AssistantTurnMinter mints plan turns from exactly those
// assistant_text events. This env is set for ALL agents (plan-gardeners included —
// they are NOT special-cased out) whenever a NATS URL is configured, so the whole
// turn-persistence + full-tail-replay feature silently no-ops if this regresses.
func TestApplyCommonConfig_NatsForwardInjected(t *testing.T) {
	cfg := &config.Config{
		NatsURL:      "nats://gardenio-nats:4222",
		ProjectCache: map[string]config.ProjectCacheEntry{},
	}
	spec := &podmanager.AgentPodSpec{
		Project: "gardenio",
		Role:    "plan-gardener", // the role that mints plan-chat turns
		Env:     map[string]string{},
	}
	applyCommonConfig(cfg, spec)

	if spec.Env["COOP_NATS_FORWARD"] != "1" {
		t.Errorf("plan-gardener pods MUST get COOP_NATS_FORWARD=1 (else no assistant_text "+
			"events are published and the transcript minter never fires); got %q", spec.Env["COOP_NATS_FORWARD"])
	}
	if spec.Env["COOP_NATS_URL"] != "nats://gardenio-nats:4222" {
		t.Errorf("expected COOP_NATS_URL to be wired; got %q", spec.Env["COOP_NATS_URL"])
	}
	if spec.Env["BEADS_NATS_URL"] != "nats://gardenio-nats:4222" {
		t.Errorf("expected BEADS_NATS_URL to be wired; got %q", spec.Env["BEADS_NATS_URL"])
	}
}

// TestApplyCommonConfig_NatsForwardOmittedWithoutURL is the paired negative: when
// no NATS URL is configured, the forward env is NOT set (coop stays quiet rather
// than publishing to a nonexistent broker).
func TestApplyCommonConfig_NatsForwardOmittedWithoutURL(t *testing.T) {
	cfg := &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{},
	}
	spec := &podmanager.AgentPodSpec{
		Project: "gardenio",
		Role:    "plan-gardener",
		Env:     map[string]string{},
	}
	applyCommonConfig(cfg, spec)

	if _, ok := spec.Env["COOP_NATS_FORWARD"]; ok {
		t.Error("COOP_NATS_FORWARD must NOT be set when no NATS URL is configured")
	}
}

func TestBuildSpecFromBeadInfo_SlackThreadMetadata(t *testing.T) {
	cfg := &config.Config{
		Namespace:    "test",
		CoopImage:    "agent:latest",
		ProjectCache: map[string]config.ProjectCacheEntry{},
	}
	metadata := map[string]string{
		"slack_thread_channel": "C-help",
		"slack_thread_ts":      "9999.0001",
	}
	spec := BuildSpecFromBeadInfo(cfg, "gardenio", "crew", "crew", "agent1", metadata)

	if spec.Env["SLACK_THREAD_CHANNEL"] != "C-help" {
		t.Errorf("expected SLACK_THREAD_CHANNEL=C-help, got %q", spec.Env["SLACK_THREAD_CHANNEL"])
	}
	if spec.Env["SLACK_THREAD_TS"] != "9999.0001" {
		t.Errorf("expected SLACK_THREAD_TS=9999.0001, got %q", spec.Env["SLACK_THREAD_TS"])
	}
}

func TestApplyAccountConfig_FullAccount(t *testing.T) {
	cfg := &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{
			"myproject": {Account: "claude-max-litellm"},
		},
		AccountCache: map[string]config.AccountCacheEntry{
			"claude-max-litellm": {
				Provider:   "anthropic",
				BaseURL:    "http://litellm.devops:4000",
				Credential: "coopmux:claude-max",
				Model:      "anthropic-claude",
				LiteLLMKey: "sk-proj-12345", // gitleaks:allow: fake key
				Labels:     []string{"project:myproject"},
			},
		},
	}
	spec := &podmanager.AgentPodSpec{
		Project: "myproject",
		Env:     map[string]string{},
	}
	applyAccountConfig(cfg, spec)

	if spec.Env["ANTHROPIC_BASE_URL"] != "http://litellm.devops:4000" {
		t.Errorf("ANTHROPIC_BASE_URL: got %q, want %q", spec.Env["ANTHROPIC_BASE_URL"], "http://litellm.devops:4000")
	}
	if spec.Env["ANTHROPIC_MODEL"] != "anthropic-claude" {
		t.Errorf("ANTHROPIC_MODEL: got %q, want %q", spec.Env["ANTHROPIC_MODEL"], "anthropic-claude")
	}
	// The header carries the api-key plus the attribution tags. This spec has no
	// AgentName/Role, so only project: and model: (from acct.Model) ride along
	// (ka-0000000478).
	if got, want := spec.Env["ANTHROPIC_CUSTOM_HEADERS"], "x-litellm-api-key: Bearer sk-proj-12345\nx-litellm-tags: project:myproject,model:anthropic-claude"; got != want {
		t.Errorf("ANTHROPIC_CUSTOM_HEADERS:\n got  %q\n want %q", got, want)
	}
	// BOAT_ACCOUNT must name the account whose credential we baked in, so coop
	// can report the account it is actually using (ka-0000002509).
	if spec.Env["BOAT_ACCOUNT"] != "claude-max-litellm" {
		t.Errorf("BOAT_ACCOUNT: got %q, want %q", spec.Env["BOAT_ACCOUNT"], "claude-max-litellm")
	}
	// And it must equal the authoritative resolved account recorded on the spec.
	if spec.Env["BOAT_ACCOUNT"] != spec.ResolvedAccount {
		t.Errorf("BOAT_ACCOUNT %q != spec.ResolvedAccount %q", spec.Env["BOAT_ACCOUNT"], spec.ResolvedAccount)
	}
}

func TestApplyAccountConfig_TagsRequestWithAgentName(t *testing.T) {
	cfg := &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{
			"myproject": {Account: "claude-max"},
		},
		AccountCache: map[string]config.AccountCacheEntry{
			"claude-max": {
				Provider:   "anthropic",
				BaseURL:    "http://litellm.devops:4000",
				LiteLLMKey: "sk-proj-12345", // gitleaks:allow: fake key
				Labels:     []string{"project:myproject"},
			},
		},
	}
	spec := &podmanager.AgentPodSpec{
		Project:   "myproject",
		AgentName: "gardenio-fix-session-cost-bfi",
		Env:       map[string]string{},
	}
	applyAccountConfig(cfg, spec)

	// The agent identity must ride along as an x-litellm-tags header so LiteLLM
	// attributes per-agent spend (read back from /spend/tags at session close).
	// Multiple headers are newline-separated. This account has no Model and the
	// cfg has no ClaudeModel, so no model: tag is emitted; the spec has no Role,
	// so no role: tag — only agent: and project: ride along (ka-0000000478).
	want := "x-litellm-api-key: Bearer sk-proj-12345\nx-litellm-tags: agent:gardenio-fix-session-cost-bfi,project:myproject"
	if spec.Env["ANTHROPIC_CUSTOM_HEADERS"] != want {
		t.Errorf("ANTHROPIC_CUSTOM_HEADERS:\n got  %q\n want %q", spec.Env["ANTHROPIC_CUSTOM_HEADERS"], want)
	}
}

func TestApplyAccountConfig_TagsAllDimensions(t *testing.T) {
	// A full agent identity (agent + role + project + account model) emits all
	// four attribution tags, in canonical order agent,role,project,model, so the
	// real-bill tag gauge can break spend down by role/project/model (ka-0000000478).
	cfg := &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{
			"monorepo": {Account: "claude-max"},
		},
		AccountCache: map[string]config.AccountCacheEntry{
			"claude-max": {
				Provider:   "anthropic",
				BaseURL:    "http://litellm.devops:4000",
				Model:      "claude-opus-4-8",
				LiteLLMKey: "sk-proj-12345", // gitleaks:allow: fake key
				Labels:     []string{"project:monorepo"},
			},
		},
	}
	// Multi-role value must be comma-sanitized (commas are the tag delimiter).
	spec := &podmanager.AgentPodSpec{
		Project:   "monorepo",
		Role:      "crew,thread",
		AgentName: "monorepo-gardener-x",
		Env:       map[string]string{},
	}
	applyAccountConfig(cfg, spec)

	want := "x-litellm-api-key: Bearer sk-proj-12345\nx-litellm-tags: agent:monorepo-gardener-x,role:crew-thread,project:monorepo,model:claude-opus-4-8"
	if got := spec.Env["ANTHROPIC_CUSTOM_HEADERS"]; got != want {
		t.Errorf("ANTHROPIC_CUSTOM_HEADERS:\n got  %q\n want %q", got, want)
	}
}

func TestApplyAccountConfig_TagsCommaSanitized(t *testing.T) {
	// Commas delimit x-litellm-tags entries, so a comma in ANY value (here a
	// contrived project and model) must be stripped to a dash, or it would split
	// into a spurious tag and corrupt the per-dimension attribution (ka-0000000478).
	cfg := &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{
			"a,b": {Account: "acct"},
		},
		AccountCache: map[string]config.AccountCacheEntry{
			"acct": {Provider: "anthropic", LiteLLMKey: "sk-1", BaseURL: "http://l:4000", Model: "m,x", Labels: []string{"project:a,b"}},
		},
	}
	spec := &podmanager.AgentPodSpec{Project: "a,b", Role: "crew,thread", AgentName: "ag1", Env: map[string]string{}}
	applyAccountConfig(cfg, spec)

	got := spec.Env["ANTHROPIC_CUSTOM_HEADERS"]
	want := "x-litellm-api-key: Bearer sk-1\nx-litellm-tags: agent:ag1,role:crew-thread,project:a-b,model:m-x"
	if got != want {
		t.Errorf("comma sanitization:\n got  %q\n want %q", got, want)
	}
	// Exactly 4 tags after the header line — no value introduced a 5th.
	tagline := got[strings.Index(got, "x-litellm-tags: ")+len("x-litellm-tags: "):]
	if n := strings.Count(tagline, ",") + 1; n != 4 {
		t.Errorf("expected 4 tags, got %d: %q", n, tagline)
	}
}

func TestApplyAccountConfig_ModelTagFallsBackToClaudeModel(t *testing.T) {
	// When the account has no Model, the model: tag falls back to the
	// controller-global ClaudeModel — matching the agent-spend counter's model
	// label resolution (ka-0000000478).
	cfg := &config.Config{
		ClaudeModel: "claude-opus-4-8",
		ProjectCache: map[string]config.ProjectCacheEntry{
			"p": {Account: "a"},
		},
		AccountCache: map[string]config.AccountCacheEntry{
			"a": {Provider: "anthropic", LiteLLMKey: "sk-1", BaseURL: "http://l:4000", Labels: []string{"project:p"}},
		},
	}
	spec := &podmanager.AgentPodSpec{Project: "p", Role: "crew", AgentName: "p-foo", Env: map[string]string{}}
	applyAccountConfig(cfg, spec)

	want := "x-litellm-api-key: Bearer sk-1\nx-litellm-tags: agent:p-foo,role:crew,project:p,model:claude-opus-4-8"
	if got := spec.Env["ANTHROPIC_CUSTOM_HEADERS"]; got != want {
		t.Errorf("ANTHROPIC_CUSTOM_HEADERS:\n got  %q\n want %q", got, want)
	}
}

func TestApplyAccountConfig_FallbackToLiteLLMURL(t *testing.T) {
	cfg := &config.Config{
		LiteLLMURL: "http://litellm-fallback:4000",
		ProjectCache: map[string]config.ProjectCacheEntry{
			"myproject": {Account: "claude-max"},
		},
		AccountCache: map[string]config.AccountCacheEntry{
			"claude-max": {
				Provider:   "anthropic",
				Credential: "coopmux:claude-max",
				Model:      "anthropic-claude",
				Labels:     []string{"project:myproject"},
				// BaseURL intentionally empty — should fall back to cfg.LiteLLMURL
			},
		},
	}
	spec := &podmanager.AgentPodSpec{
		Project: "myproject",
		Env:     map[string]string{},
	}
	applyAccountConfig(cfg, spec)

	if spec.Env["ANTHROPIC_BASE_URL"] != "http://litellm-fallback:4000" {
		t.Errorf("expected fallback LITELLM_URL, got %q", spec.Env["ANTHROPIC_BASE_URL"])
	}
}

// TestApplyAccountConfig_ProxyURLOverridesBaseURL is the claudemore wiring (A2):
// when an account bead sets proxy_url, ANTHROPIC_BASE_URL points at the proxy
// (claudemore) instead of base_url (litellm). litellm-tagging headers are
// unchanged — claudemore forwards them, so spend tracking still works.
func TestApplyAccountConfig_ProxyURLOverridesBaseURL(t *testing.T) {
	cfg := &config.Config{
		LiteLLMURL: "http://litellm-fallback:4000",
		ProjectCache: map[string]config.ProjectCacheEntry{
			"myproject": {Account: "claude-max"},
		},
		AccountCache: map[string]config.AccountCacheEntry{
			"claude-max": {
				Provider:   "anthropic",
				BaseURL:    "http://gardenio-litellm:4000",
				ProxyURL:   "http://gardenio-claudemore:8100",
				Credential: "coopmux:claude-max",
				Model:      "anthropic-claude",
				LiteLLMKey: "sk-proj-key",
				Labels:     []string{"project:myproject"},
			},
		},
	}
	spec := &podmanager.AgentPodSpec{
		Project:   "myproject",
		AgentName: "agent-1",
		Env:       map[string]string{},
	}
	applyAccountConfig(cfg, spec)

	if got := spec.Env["ANTHROPIC_BASE_URL"]; got != "http://gardenio-claudemore:8100" {
		t.Errorf("ANTHROPIC_BASE_URL: got %q, want claudemore proxy URL", got)
	}
	// The litellm virtual-key header must still be set — claudemore forwards it,
	// so per-agent spend tracking is unchanged (cost fence §5.2).
	if hdrs := spec.Env["ANTHROPIC_CUSTOM_HEADERS"]; !strings.Contains(hdrs, "sk-proj-key") {
		t.Errorf("expected litellm key header preserved, got %q", hdrs)
	}
}

// TestApplyAccountConfig_NoProxyURLUsesBaseURL is the A2 rollback property:
// clearing proxy_url reverts ANTHROPIC_BASE_URL to base_url (litellm) on the
// next pod spawn — no code change, no redeploy.
func TestApplyAccountConfig_NoProxyURLUsesBaseURL(t *testing.T) {
	cfg := &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{
			"myproject": {Account: "claude-max"},
		},
		AccountCache: map[string]config.AccountCacheEntry{
			"claude-max": {
				Provider: "anthropic",
				BaseURL:  "http://gardenio-litellm:4000",
				// ProxyURL intentionally empty — rolled back to litellm-direct.
				Model:      "anthropic-claude",
				LiteLLMKey: "sk-rollback",
				Labels:     []string{"project:myproject"},
			},
		},
	}
	spec := &podmanager.AgentPodSpec{
		Project: "myproject",
		Env:     map[string]string{},
	}
	applyAccountConfig(cfg, spec)

	if got := spec.Env["ANTHROPIC_BASE_URL"]; got != "http://gardenio-litellm:4000" {
		t.Errorf("ANTHROPIC_BASE_URL after rollback: got %q, want litellm base_url", got)
	}
}

func TestApplyAccountConfig_NoAccount(t *testing.T) {
	cfg := &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{
			"myproject": {}, // no account field
		},
		AccountCache: map[string]config.AccountCacheEntry{},
	}
	spec := &podmanager.AgentPodSpec{
		Project: "myproject",
		Env:     map[string]string{},
	}
	applyAccountConfig(cfg, spec)

	if _, ok := spec.Env["ANTHROPIC_BASE_URL"]; ok {
		t.Error("expected no ANTHROPIC_BASE_URL when project has no account")
	}
}

func TestApplyAccountConfig_UnknownAccount(t *testing.T) {
	// Post-Phase-2 (ka-0000004094) the only way to resolve an account name that is
	// absent from the cache is a role pin (the project-cache fall-through is
	// gone). Pin one so the unknown-account terminal branch is still exercised.
	cfg := &config.Config{
		RoleCache: map[string]config.RoleCacheEntry{
			"crew": {Account: "nonexistent"},
		},
		AccountCache: map[string]config.AccountCacheEntry{},
	}
	spec := &podmanager.AgentPodSpec{
		Project: "myproject",
		Role:    "crew",
		Env:     map[string]string{},
	}
	applyAccountConfig(cfg, spec)

	// Should not set any env vars when account is not found.
	if _, ok := spec.Env["ANTHROPIC_BASE_URL"]; ok {
		t.Error("expected no ANTHROPIC_BASE_URL when account is unknown")
	}
}

func TestApplyAccountConfig_NoLiteLLMKey(t *testing.T) {
	// A keyless-but-base_url account is excluded from the spread pool
	// (!isHealthyAuthorized), so under the Phase 2 model (ka-0000004094) it is
	// only ever baked when a ROLE pins it directly — the spread health gate is
	// bypassed for a role pin exactly as before. Pin it so the "no key => no
	// x-litellm-api-key header" baking logic is still exercised.
	cfg := &config.Config{
		RoleCache: map[string]config.RoleCacheEntry{
			"crew": {Account: "claude-direct"},
		},
		AccountCache: map[string]config.AccountCacheEntry{
			"claude-direct": {
				Provider:   "anthropic",
				BaseURL:    "http://litellm:4000",
				Credential: "coopmux:claude-max",
				// No LiteLLMKey — should not set ANTHROPIC_CUSTOM_HEADERS
			},
		},
	}
	spec := &podmanager.AgentPodSpec{
		Project: "myproject",
		Role:    "crew",
		Env:     map[string]string{},
	}
	applyAccountConfig(cfg, spec)

	if spec.Env["ANTHROPIC_BASE_URL"] != "http://litellm:4000" {
		t.Errorf("expected ANTHROPIC_BASE_URL, got %q", spec.Env["ANTHROPIC_BASE_URL"])
	}
	if _, ok := spec.Env["ANTHROPIC_CUSTOM_HEADERS"]; ok {
		t.Error("expected no ANTHROPIC_CUSTOM_HEADERS when litellm_key is empty")
	}
}

func TestApplyAccountConfig_RoleOverridesProject(t *testing.T) {
	// Role-level account should take precedence over project-level account.
	// Use case: fsm-triage role pinned to litellm-bedrock regardless of which
	// project it runs in.
	cfg := &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{
			"monorepo": {Account: "matthewbaker-acme"},
		},
		RoleCache: map[string]config.RoleCacheEntry{
			"fsm-triage": {Account: "litellm-bedrock"},
		},
		AccountCache: map[string]config.AccountCacheEntry{
			"matthewbaker-acme": {
				Provider: "anthropic", BaseURL: "http://litellm:4000",
				Model: "anthropic-claude", LiteLLMKey: "sk-project",
			},
			"litellm-bedrock": {
				Provider: "anthropic", BaseURL: "http://litellm:4000",
				Model: "bedrock-claude", LiteLLMKey: "sk-bedrock",
			},
		},
	}
	spec := &podmanager.AgentPodSpec{
		Project: "monorepo",
		Role:    "fsm-triage",
		Env:     map[string]string{},
	}
	applyAccountConfig(cfg, spec)

	if spec.Env["ANTHROPIC_MODEL"] != "bedrock-claude" {
		t.Errorf("expected role account model 'bedrock-claude', got %q — role account must override project account",
			spec.Env["ANTHROPIC_MODEL"])
	}
	if got := liteLLMKeyFromHeader(spec.Env["ANTHROPIC_CUSTOM_HEADERS"]); got != "sk-bedrock" {
		t.Errorf("expected bedrock litellm key sk-bedrock, got %q (header %q)", got, spec.Env["ANTHROPIC_CUSTOM_HEADERS"])
	}
}

func TestApplyAccountConfig_RoleAccountFallsBackToProject(t *testing.T) {
	// When the agent's role has no account pin, fall back to project account.
	cfg := &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{
			"monorepo": {Account: "matthewbaker-acme"},
		},
		RoleCache: map[string]config.RoleCacheEntry{
			"crew": {}, // no Account
		},
		AccountCache: map[string]config.AccountCacheEntry{
			"matthewbaker-acme": {
				Provider: "anthropic", BaseURL: "http://litellm:4000",
				Model: "anthropic-claude", LiteLLMKey: "sk-mb", Labels: []string{"project:monorepo"},
			},
		},
	}
	spec := &podmanager.AgentPodSpec{
		Project: "monorepo",
		Role:    "crew",
		Env:     map[string]string{},
	}
	applyAccountConfig(cfg, spec)

	if spec.Env["ANTHROPIC_MODEL"] != "anthropic-claude" {
		t.Errorf("expected project account to apply when role has no account, got %q",
			spec.Env["ANTHROPIC_MODEL"])
	}
}

func TestApplyAccountConfig_MultiRoleFirstWithAccountWins(t *testing.T) {
	// For comma-separated roles, first role with a non-empty Account wins.
	cfg := &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{
			"monorepo": {Account: "project-acct"},
		},
		RoleCache: map[string]config.RoleCacheEntry{
			"thread":     {}, // no account
			"fsm-triage": {Account: "litellm-bedrock"},
		},
		AccountCache: map[string]config.AccountCacheEntry{
			"project-acct":    {Provider: "anthropic", Model: "proj-model"},
			"litellm-bedrock": {Provider: "anthropic", Model: "bedrock-model"},
		},
	}
	spec := &podmanager.AgentPodSpec{
		Project: "monorepo",
		Role:    "thread,fsm-triage",
		Env:     map[string]string{},
	}
	applyAccountConfig(cfg, spec)

	if spec.Env["ANTHROPIC_MODEL"] != "bedrock-model" {
		t.Errorf("expected bedrock-model (fsm-triage role pin), got %q", spec.Env["ANTHROPIC_MODEL"])
	}
}

func TestApplyAccountConfig_Phase0HaikuRolePin(t *testing.T) {
	// WS-1 (ka-0000001778, epic ka-0000004375, design §9.2): the Haiku phase-0
	// shake-out pins the meta-scorer gardener (ka-0000001271, role=phase0-haiku)
	// to a Haiku-backed account via the SAME role->account mechanism as
	// fsm-triage — config only, NO engine change. This test locks the
	// resolution chain so the pin cannot silently regress while the phase-0
	// window is live: role:phase0-haiku -> account:first-mate-haiku ->
	// model:claude-haiku-4-5. The bounded/reversible kill-switch is documented
	// in gardenio/docs/haiku-phase0-shakeout-runbook.md (revert = repoint the
	// gardener's role field back to the default Opus-backed role; one kd update).
	cfg := &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{
			// Project default would resolve to an Opus-backed account — the
			// role pin must override it.
			"gardenio": {Account: "gardenio-opus"},
		},
		RoleCache: map[string]config.RoleCacheEntry{
			"phase0-haiku": {Account: "first-mate-haiku"},
		},
		AccountCache: map[string]config.AccountCacheEntry{
			"gardenio-opus": {
				Provider: "anthropic", BaseURL: "https://litellm.app.devops.acme.example",
				Model: "claude-opus-4-8", LiteLLMKey: "sk-opus",
			},
			"first-mate-haiku": {
				Provider: "anthropic", BaseURL: "https://litellm.app.devops.acme.example",
				Model: "claude-haiku-4-5", LiteLLMKey: "sk-haiku",
			},
		},
	}
	spec := &podmanager.AgentPodSpec{
		Project: "gardenio",
		Role:    "phase0-haiku",
		Env:     map[string]string{},
	}
	applyAccountConfig(cfg, spec)

	if spec.Env["ANTHROPIC_MODEL"] != "claude-haiku-4-5" {
		t.Errorf("expected Haiku model 'claude-haiku-4-5' (phase0-haiku role pin must override project Opus account), got %q",
			spec.Env["ANTHROPIC_MODEL"])
	}
	if got := liteLLMKeyFromHeader(spec.Env["ANTHROPIC_CUSTOM_HEADERS"]); got != "sk-haiku" {
		t.Errorf("expected first-mate-haiku litellm key sk-haiku, got %q (header %q)", got, spec.Env["ANTHROPIC_CUSTOM_HEADERS"])
	}
}

func TestRoleMatchesExclusive(t *testing.T) {
	cases := []struct {
		role          string
		exclusiveRole string
		want          bool
	}{
		{"fsm-triage", "fsm-triage", true},
		{"thread,fsm-triage", "fsm-triage", true},
		{"fsm-triage,crew", "fsm-triage", true},
		{" fsm-triage ", "fsm-triage", true},
		{"crew", "fsm-triage", false},
		{"gardener", "fsm-triage", false},
		{"thread", "fsm-triage", false},
		{"", "fsm-triage", false},
		{"fsm-reproduce", "fsm-triage", false},
		// An account with no exclusive_to_role matches no role.
		{"fsm-triage", "", false},
		{"anything", "", false},
		// A different exclusivity owner (e.g. a new exclusive account).
		{"vip-role", "vip-role", true},
		{"crew,vip-role", "vip-role", true},
	}
	for _, c := range cases {
		if got := roleMatchesExclusive(c.role, c.exclusiveRole); got != c.want {
			t.Errorf("roleMatchesExclusive(%q, %q) = %v, want %v", c.role, c.exclusiveRole, got, c.want)
		}
	}
}

func TestRoleOwnsExclusiveAccount(t *testing.T) {
	cfg := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{
			"litellm-bedrock": {Provider: "anthropic", ExclusiveToRole: "fsm-triage"},
			"vip-account":     {Provider: "anthropic", ExclusiveToRole: "vip-role"},
			"gardenio":        {Provider: "anthropic"}, // not exclusive
		},
	}
	cases := []struct {
		role string
		want bool
	}{
		{"fsm-triage", true},
		{"thread,fsm-triage", true},
		{"vip-role", true},
		{"crew", false},
		{"", false},
		{"hema-analyst", false}, // pinned to bedrock but NOT its exclusive owner
	}
	for _, c := range cases {
		if got := roleOwnsExclusiveAccount(cfg, c.role); got != c.want {
			t.Errorf("roleOwnsExclusiveAccount(%q) = %v, want %v", c.role, got, c.want)
		}
	}
	// No exclusive accounts at all → never an owner.
	empty := &config.Config{AccountCache: map[string]config.AccountCacheEntry{"gardenio": {Provider: "anthropic"}}}
	if roleOwnsExclusiveAccount(empty, "fsm-triage") {
		t.Errorf("roleOwnsExclusiveAccount with no exclusive accounts must be false")
	}
}

func TestApplyAccountConfig_IntegrationWithApplyCommonConfig(t *testing.T) {
	// Verify that applyCommonConfig calls applyAccountConfig properly.
	cfg := &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{
			"myproject": {Account: "litellm-proxy"},
		},
		AccountCache: map[string]config.AccountCacheEntry{
			"litellm-proxy": {
				Provider:   "anthropic",
				BaseURL:    "http://litellm:4000",
				Credential: "coopmux:claude-max",
				Model:      "anthropic-claude",
				LiteLLMKey: "sk-virtual-123",
				Labels:     []string{"project:myproject"},
			},
		},
	}
	spec := &podmanager.AgentPodSpec{
		Project: "myproject",
		Env:     map[string]string{},
	}
	applyCommonConfig(cfg, spec)

	if spec.Env["ANTHROPIC_BASE_URL"] != "http://litellm:4000" {
		t.Errorf("ANTHROPIC_BASE_URL: got %q, want %q", spec.Env["ANTHROPIC_BASE_URL"], "http://litellm:4000")
	}
	if spec.Env["ANTHROPIC_MODEL"] != "anthropic-claude" {
		t.Errorf("ANTHROPIC_MODEL: got %q, want %q", spec.Env["ANTHROPIC_MODEL"], "anthropic-claude")
	}
	if got := liteLLMKeyFromHeader(spec.Env["ANTHROPIC_CUSTOM_HEADERS"]); got != "sk-virtual-123" {
		t.Errorf("litellm key: got %q, want sk-virtual-123 (header %q)", got, spec.Env["ANTHROPIC_CUSTOM_HEADERS"])
	}
}

// --- Exhausted-account guard tests (ka-0000000313) ---
//
// Phase 2 (ka-0000004094, account-routing.md §7) removed the DefaultAccount /
// per-account fallback-chain redirect from the spawn path. An exhausted account
// no longer redirects to a DefaultAccount or a chain successor; instead a healthy
// account is chosen up front by the spread pool (selectAccount), or — when
// the whole pool is exhausted — the spawn is blocked (see
// account_pool_exhaust_test.go). The former
// TestApplyAccountConfig_ExhaustedFallsBackToDefaultAccount was deleted with the
// behavior it covered.

// TestApplyAccountConfig_BedrockPartitionedByRole verifies the data-driven
// account-exclusivity partition end-to-end through applyAccountConfig
// (ka-0000005223, policy ka-0000002660):
//   - fsm-triage gets Bedrock via its role pin (resolveRoleAccount), and is NOT
//     redirected onto a Claude Max account even when Bedrock is exhausted.
//   - non-owner roles NEVER receive Bedrock, even when every Claude Max account
//     is exhausted and Bedrock is the only healthy account.
//
// The partition is expressed purely by exclusive_to_role=fsm-triage on the
// litellm-bedrock account bead — no hardcoded role/account constant.
func TestApplyAccountConfig_BedrockPartitionedByRole(t *testing.T) {
	newCfg := func() *config.Config {
		return &config.Config{
			ProjectCache: map[string]config.ProjectCacheEntry{
				"monorepo": {Account: "gardenio"},
			},
			RoleCache: map[string]config.RoleCacheEntry{
				"fsm-triage": {Account: "litellm-bedrock"},
			},
			AccountCache: map[string]config.AccountCacheEntry{
				"gardenio": {
					Provider: "anthropic", BaseURL: "http://litellm:4000",
					LiteLLMKey: "sk-exhausted-1", Status: "exhausted",
					Labels: []string{"project:monorepo"},
				},
				"gardenio2": {
					Provider: "anthropic", BaseURL: "http://litellm:4000",
					LiteLLMKey: "sk-exhausted-2", Status: "exhausted",
					Labels: []string{"project:monorepo"},
				},
				"litellm-bedrock": {
					Provider:        "anthropic",
					BaseURL:         "http://litellm:4000",
					Credential:      "secret:bedrock-token",
					Model:           "bedrock-claude",
					LiteLLMKey:      "sk-bedrock-healthy",
					Status:          "active",
					ExclusiveToRole: "fsm-triage",
				},
			},
		}
	}

	// fsm-triage: pinned to Bedrock via its role bead.
	triageSpec := &podmanager.AgentPodSpec{
		Project: "monorepo",
		Role:    "fsm-triage",
		Env:     map[string]string{},
	}
	applyAccountConfig(newCfg(), triageSpec)
	if got := liteLLMKeyFromHeader(triageSpec.Env["ANTHROPIC_CUSTOM_HEADERS"]); got != "sk-bedrock-healthy" {
		t.Errorf("fsm-triage: expected litellm-bedrock key (role pin) sk-bedrock-healthy, got %q", got)
	}
	if got := triageSpec.Env["ANTHROPIC_MODEL"]; got != "bedrock-claude" {
		t.Errorf("fsm-triage: expected bedrock-claude model, got %q", got)
	}

	// crew: every Claude Max account is exhausted and Bedrock is the only healthy
	// account — but crew must NOT receive it.
	crewSpec := &podmanager.AgentPodSpec{
		Project: "monorepo",
		Role:    "crew",
		Env:     map[string]string{},
	}
	applyAccountConfig(newCfg(), crewSpec)
	if got := crewSpec.Env["ANTHROPIC_CUSTOM_HEADERS"]; got == "x-litellm-api-key: Bearer sk-bedrock-healthy" {
		t.Errorf("crew: must NOT receive litellm-bedrock, got %q", got)
	}
	if got := crewSpec.Env["ANTHROPIC_MODEL"]; got == "bedrock-claude" {
		t.Errorf("crew: must NOT use bedrock-claude model, got %q", got)
	}
}

// TestApplyAccountConfig_FSMTriageStaysOnExhaustedBedrock verifies that a
// fsm-triage agent is NOT redirected to a healthy Claude Max account when its
// exclusive Bedrock account is exhausted (ka-0000005223) — an exclusive account
// never spills.
func TestApplyAccountConfig_FSMTriageStaysOnExhaustedBedrock(t *testing.T) {
	cfg := &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{
			"monorepo": {Account: "gardenio2"},
		},
		RoleCache: map[string]config.RoleCacheEntry{
			"fsm-triage": {Account: "litellm-bedrock"},
		},
		AccountCache: map[string]config.AccountCacheEntry{
			"gardenio2": {
				Provider: "anthropic", BaseURL: "http://litellm:4000",
				LiteLLMKey: "sk-healthy-max", Model: "anthropic-claude", Status: "active",
			},
			"litellm-bedrock": {
				Provider: "anthropic", BaseURL: "http://litellm:4000",
				LiteLLMKey: "sk-bedrock", Model: "bedrock-claude", Status: "exhausted",
				ExclusiveToRole: "fsm-triage",
			},
		},
	}
	spec := &podmanager.AgentPodSpec{
		Project: "monorepo",
		Role:    "fsm-triage",
		Env:     map[string]string{},
	}
	applyAccountConfig(cfg, spec)
	if got := spec.Env["ANTHROPIC_CUSTOM_HEADERS"]; got == "x-litellm-api-key: Bearer sk-healthy-max" {
		t.Errorf("fsm-triage must NOT be redirected to a Claude Max account, got %q", got)
	}
	if got := spec.Env["ANTHROPIC_MODEL"]; got != "bedrock-claude" {
		t.Errorf("fsm-triage must stay on bedrock-claude model even when exhausted, got %q", got)
	}
}

// TestApplyAccountConfig_HealthyAccountNoChange: healthy accounts must not be
// rewritten by the guard — pod env carries the originally-resolved account's
// credentials.
func TestApplyAccountConfig_HealthyAccountNoChange(t *testing.T) {
	cfg := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{
			"gardenio2": {
				Provider:   "anthropic",
				BaseURL:    "http://litellm:4000",
				Credential: "secret:gardenio2-token",
				Model:      "anthropic-claude",
				LiteLLMKey: "sk-healthy-key",
				Status:     "active",
				Labels:     []string{"project:monorepo"},
			},
		},
	}
	spec := &podmanager.AgentPodSpec{
		Project: "monorepo",
		Env:     map[string]string{},
	}
	applyAccountConfig(cfg, spec)

	if got := liteLLMKeyFromHeader(spec.Env["ANTHROPIC_CUSTOM_HEADERS"]); got != "sk-healthy-key" {
		t.Errorf("healthy account must not be rewritten; got key %q", got)
	}
	if got := spec.Env["ANTHROPIC_MODEL"]; got != "anthropic-claude" {
		t.Errorf("expected anthropic-claude model (no fallback), got %q", got)
	}
}

// TestApplyAccountConfig_ExhaustedEmptyStatusTreatedAsHealthy: an account with
// empty Status must NOT be treated as exhausted — the guard only triggers on
// explicit status="exhausted" (mirrors buildProjectAccountOverride semantics).
func TestApplyAccountConfig_ExhaustedEmptyStatusTreatedAsHealthy(t *testing.T) {
	cfg := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{
			"gardenio": {
				Provider:   "anthropic",
				BaseURL:    "http://litellm:4000",
				LiteLLMKey: "sk-empty-status",
				Labels:     []string{"project:monorepo"},
				// Status: "" (empty) — treated as active.
			},
		},
	}
	spec := &podmanager.AgentPodSpec{
		Project: "monorepo",
		Env:     map[string]string{},
	}
	applyAccountConfig(cfg, spec)

	if got := liteLLMKeyFromHeader(spec.Env["ANTHROPIC_CUSTOM_HEADERS"]); got != "sk-empty-status" {
		t.Errorf("empty status must be treated as healthy; got key %q", got)
	}
}

// TestApplyAccountConfig_RolePinnedExhaustedBlocksSpawn locks the Phase 2
// (ka-0000004094, account-routing.md §7) behavior for a role-pinned exhausted
// account. Pre-Phase-2 a ROLE-pinned exhausted account (gardener-delivery →
// gardenio) flowed through resolveNonExhaustedAccount and hopped down a per-account
// fallback chain to the first healthy account. Phase 2 deleted that chain walk:
// a role pin resolves exactly one account, and if it is capacity-exhausted (and
// not exclusive) the pool-exhaustion escape hatch blocks the spawn rather than
// baking a known-dead key. The bead is not failed and no escalation is filed —
// the reconciler re-attempts once the account's window resets. This guards
// against a regression that would silently bake the exhausted role-pinned key.
//
// NOTE: the project pool here holds ONLY the exhausted pinned account (no healthy
// sibling), so the role-pin exhaustion spill (ka-0000001307) finds nowhere to
// spill and this remains a genuine block. The spill's healthy-sibling case is
// covered in account_role_pin_spill_test.go.
func TestApplyAccountConfig_RolePinnedExhaustedBlocksSpawn(t *testing.T) {
	cfg := &config.Config{
		RoleCache: map[string]config.RoleCacheEntry{
			// gardener-delivery pins the (now-exhausted) gardenio account.
			"gardener-delivery": {Account: "gardenio"},
		},
		AccountCache: map[string]config.AccountCacheEntry{
			"gardenio": {
				Provider: "anthropic", BaseURL: "http://litellm:4000",
				Credential: "secret:gardenio-token", Model: "anthropic/claude-haiku-4-5",
				LiteLLMKey: "sk-gardenio-exhausted", Status: "exhausted",
				ThrottleResetAt: "2026-06-29T22:00:00Z",
			},
		},
	}
	spec := &podmanager.AgentPodSpec{
		Project: "gardenio",
		Role:    "gardener-delivery",
		Env:     map[string]string{},
	}
	applyAccountConfig(cfg, spec)

	if !spec.SpawnBlocked {
		t.Fatalf("role-pinned exhausted account must block the spawn (no chain failover in Phase 2), got SpawnBlocked=false")
	}
	if spec.SpawnBlockReason == "" {
		t.Error("expected a non-empty SpawnBlockReason")
	}
	if spec.ResolvedAccount != "" {
		t.Errorf("must not record a ResolvedAccount for a blocked spawn, got %q", spec.ResolvedAccount)
	}
	if _, ok := spec.Env["ANTHROPIC_CUSTOM_HEADERS"]; ok {
		t.Error("must NOT bake the exhausted role-pinned key")
	}
	if _, ok := spec.Env["BOAT_ACCOUNT"]; ok {
		t.Error("must NOT set BOAT_ACCOUNT for a blocked spawn")
	}
}

// --- Agent-to-agent network isolation tests ---

func TestApplyCommonConfig_NatsAgentIsolation(t *testing.T) {
	// W5 (ka-0000006585): the NATS agent token is projected as a tmpfs file,
	// not an env-var. When NatsAgentUserSecret is set, agent pods should
	// reference the agent-specific secret via NatsTokenSecret and have
	// COOP_NATS_TOKEN_FILE pointing at the projected file. The secret must
	// NOT appear in SecretEnv (env-var injection) anymore.
	cfg := &config.Config{
		NatsURL:             "nats://nats:4222",
		NatsAgentUserSecret: "gardenio-nats-agent-creds",
		NatsTokenSecret:     "gardenio-nats-shared-token",
		ProjectCache:        map[string]config.ProjectCacheEntry{},
	}
	spec := &podmanager.AgentPodSpec{
		Project: "myproject",
		Env:     map[string]string{},
	}
	applyCommonConfig(cfg, spec)

	if spec.NatsTokenSecret != "gardenio-nats-agent-creds" {
		t.Errorf("NatsTokenSecret: expected %q, got %q",
			"gardenio-nats-agent-creds", spec.NatsTokenSecret)
	}
	wantPath := podmanager.MountNatsToken + "/" + podmanager.NatsTokenFileName
	if got := spec.Env["COOP_NATS_TOKEN_FILE"]; got != wantPath {
		t.Errorf("COOP_NATS_TOKEN_FILE: expected %q, got %q", wantPath, got)
	}
	for _, s := range spec.SecretEnv {
		if s.EnvName == "COOP_NATS_TOKEN" {
			t.Errorf("COOP_NATS_TOKEN must not be injected as env-var (W5); found SecretEnv entry from %q", s.SecretName)
		}
	}
}

func TestApplyCommonConfig_NatsLegacyToken(t *testing.T) {
	// When only NatsTokenSecret is set (no subject isolation), agent pods
	// reference the shared secret via NatsTokenSecret (file mount).
	cfg := &config.Config{
		NatsURL:         "nats://nats:4222",
		NatsTokenSecret: "gardenio-nats-shared-token",
		ProjectCache:    map[string]config.ProjectCacheEntry{},
	}
	spec := &podmanager.AgentPodSpec{
		Project: "myproject",
		Env:     map[string]string{},
	}
	applyCommonConfig(cfg, spec)

	if spec.NatsTokenSecret != "gardenio-nats-shared-token" {
		t.Errorf("NatsTokenSecret: expected %q, got %q",
			"gardenio-nats-shared-token", spec.NatsTokenSecret)
	}
	if spec.Env["COOP_NATS_TOKEN_FILE"] == "" {
		t.Error("expected COOP_NATS_TOKEN_FILE to be set")
	}
	for _, s := range spec.SecretEnv {
		if s.EnvName == "COOP_NATS_TOKEN" {
			t.Errorf("COOP_NATS_TOKEN must not be injected as env-var (W5)")
		}
	}
}

func TestApplyCommonConfig_NatsAgentSecretTakesPrecedence(t *testing.T) {
	// Agent secret takes precedence over the shared legacy token — the
	// file-mounted NatsTokenSecret must point at the agent-specific secret
	// when both are configured.
	cfg := &config.Config{
		NatsURL:             "nats://nats:4222",
		NatsAgentUserSecret: "agent-creds",
		NatsTokenSecret:     "shared-token",
		ProjectCache:        map[string]config.ProjectCacheEntry{},
	}
	spec := &podmanager.AgentPodSpec{
		Project: "myproject",
		Env:     map[string]string{},
	}
	applyCommonConfig(cfg, spec)

	if spec.NatsTokenSecret != "agent-creds" {
		t.Errorf("NatsTokenSecret: expected agent-creds, got %q", spec.NatsTokenSecret)
	}
}

// TestApplyCommonConfig_NatsTokenFileMount verifies W5 (ka-0000006585): the
// NATS token is not present in spec.Env (the controller-supplied env map)
// and only the *path* to the projected file is exposed. The token bytes
// themselves live in the file produced by the K8s Secret volume.
func TestApplyCommonConfig_NatsTokenFileMount(t *testing.T) {
	cfg := &config.Config{
		NatsURL:             "nats://nats:4222",
		NatsAgentUserSecret: "agent-creds",
		ProjectCache:        map[string]config.ProjectCacheEntry{},
	}
	spec := &podmanager.AgentPodSpec{
		Project: "myproject",
		Env:     map[string]string{},
	}
	applyCommonConfig(cfg, spec)

	if _, set := spec.Env["COOP_NATS_TOKEN"]; set {
		t.Error("COOP_NATS_TOKEN must not be set in spec.Env (W5: env-var injection removed)")
	}
	if spec.Env["COOP_NATS_TOKEN_FILE"] == "" {
		t.Error("COOP_NATS_TOKEN_FILE must be set so consumers find the projected token")
	}
}

// --- Object-store credentials (gd-W8OwRoHLLn) ---

// TestApplyCommonConfig_ObjectStoreCredentials_EndpointMode verifies that
// when an object-store endpoint (in-box MinIO) and a credentials secret are
// both configured, the pod spec carries the Secret name (for podmanager to
// project as a tmpfs volume, mirroring the NATS-token pattern) and the two
// COOP_S3_*_FILE env vars point at the projected file paths — never the
// credential value itself.
func TestApplyCommonConfig_ObjectStoreCredentials_EndpointMode(t *testing.T) {
	cfg := &config.Config{
		CoopS3Bucket:            "gardenio-sessions",
		CoopS3Prefix:            "coop/sessions",
		CoopS3Endpoint:          "http://minio.gardenio.svc:9000",
		CoopS3CredentialsSecret: "gardenio-minio-agent-credentials",
		ProjectCache:            map[string]config.ProjectCacheEntry{},
	}
	spec := &podmanager.AgentPodSpec{
		Project: "myproject",
		Env:     map[string]string{},
	}
	applyCommonConfig(cfg, spec)

	if spec.ObjectStoreCredentialsSecret != "gardenio-minio-agent-credentials" {
		t.Errorf("ObjectStoreCredentialsSecret: expected %q, got %q",
			"gardenio-minio-agent-credentials", spec.ObjectStoreCredentialsSecret)
	}
	wantAccessKeyPath := podmanager.MountObjectStoreCreds + "/" + podmanager.ObjectStoreAccessKeyFileName
	if got := spec.Env["COOP_S3_ACCESS_KEY_ID_FILE"]; got != wantAccessKeyPath {
		t.Errorf("COOP_S3_ACCESS_KEY_ID_FILE: expected %q, got %q", wantAccessKeyPath, got)
	}
	wantSecretKeyPath := podmanager.MountObjectStoreCreds + "/" + podmanager.ObjectStoreSecretKeyFileName
	if got := spec.Env["COOP_S3_SECRET_ACCESS_KEY_FILE"]; got != wantSecretKeyPath {
		t.Errorf("COOP_S3_SECRET_ACCESS_KEY_FILE: expected %q, got %q", wantSecretKeyPath, got)
	}
	for k, v := range spec.Env {
		if k == "COOP_S3_ACCESS_KEY_ID_FILE" || k == "COOP_S3_SECRET_ACCESS_KEY_FILE" {
			continue
		}
		if v == "gardenio-minio-agent-credentials" {
			t.Errorf("credential secret name leaked into unrelated env var %q", k)
		}
	}
}

// TestApplyCommonConfig_ObjectStoreCredentials_RootSecretRefused pins
// gd-urSiLLwPIz: the MinIO root Secret (`<release>-minio-credentials`) can
// read, overwrite and delete every bucket. If COOP_S3_CREDENTIALS_SECRET names
// it, agent pods get no object-store volume and no COOP_S3_*_FILE env at all,
// rather than the root keys.
func TestApplyCommonConfig_ObjectStoreCredentials_RootSecretRefused(t *testing.T) {
	cfg := &config.Config{
		CoopS3Bucket:            "gardenio-sessions",
		CoopS3Prefix:            "coop/sessions",
		CoopS3Endpoint:          "http://minio.gardenio.svc:9000",
		CoopS3CredentialsSecret: "gardenio-minio-credentials",
		ProjectCache:            map[string]config.ProjectCacheEntry{},
	}
	spec := &podmanager.AgentPodSpec{
		Project: "myproject",
		Env:     map[string]string{},
	}
	applyCommonConfig(cfg, spec)

	if spec.ObjectStoreCredentialsSecret != "" {
		t.Errorf("the MinIO root Secret must never be projected into agent pods, got %q",
			spec.ObjectStoreCredentialsSecret)
	}
	for _, k := range []string{"COOP_S3_ACCESS_KEY_ID_FILE", "COOP_S3_SECRET_ACCESS_KEY_FILE"} {
		if _, set := spec.Env[k]; set {
			t.Errorf("%s must not be set when the credentials Secret is refused", k)
		}
	}
	if spec.Env["COOP_S3_ENDPOINT"] != "http://minio.gardenio.svc:9000" {
		t.Errorf("COOP_S3_ENDPOINT should still be wired, got %q", spec.Env["COOP_S3_ENDPOINT"])
	}
}

// TestForbiddenPodSecret_MinIORootVsAgent pins the marker split: the root
// Secret (and any project-prefixed variant a project bead might declare) is
// denied, while the least-privilege agent Secret the chart wires is not.
func TestForbiddenPodSecret_MinIORootVsAgent(t *testing.T) {
	for _, name := range []string{"gardenio-minio-credentials", "myproject-minio-credentials", "Gardenio-MinIO-Credentials"} {
		if !isForbiddenPodSecret(name) {
			t.Errorf("isForbiddenPodSecret(%q) = false, want true (MinIO root keys)", name)
		}
	}
	if isForbiddenPodSecret("gardenio-minio-agent-credentials") {
		t.Error("isForbiddenPodSecret(gardenio-minio-agent-credentials) = true, want false (least-privilege agent user)")
	}
}

// TestApplyCommonConfig_ObjectStoreCredentials_AWSMode is the regression test
// the acceptance criteria call out explicitly: when CoopS3Endpoint is unset
// (AWS/IRSA mode), the pod spec must be byte-for-byte unchanged by this
// feature — no ObjectStoreCredentialsSecret, no COOP_S3_*_FILE env vars —
// even if a credentials secret name happens to be configured (it must be
// ignored outside endpoint mode, since IRSA supplies credentials instead).
func TestApplyCommonConfig_ObjectStoreCredentials_AWSMode(t *testing.T) {
	cfg := &config.Config{
		CoopS3Bucket:            "gardenio-sessions",
		CoopS3Prefix:            "coop/sessions",
		CoopS3Region:            "us-east-1",
		CoopS3CredentialsSecret: "gardenio-minio-credentials", // must be ignored: endpoint unset
		ProjectCache:            map[string]config.ProjectCacheEntry{},
	}
	spec := &podmanager.AgentPodSpec{
		Project: "myproject",
		Env:     map[string]string{},
	}
	applyCommonConfig(cfg, spec)

	if spec.ObjectStoreCredentialsSecret != "" {
		t.Errorf("ObjectStoreCredentialsSecret must stay unset in AWS/IRSA mode, got %q",
			spec.ObjectStoreCredentialsSecret)
	}
	if _, set := spec.Env["COOP_S3_ACCESS_KEY_ID_FILE"]; set {
		t.Error("COOP_S3_ACCESS_KEY_ID_FILE must not be set in AWS/IRSA mode")
	}
	if _, set := spec.Env["COOP_S3_SECRET_ACCESS_KEY_FILE"]; set {
		t.Error("COOP_S3_SECRET_ACCESS_KEY_FILE must not be set in AWS/IRSA mode")
	}
	if _, set := spec.Env["COOP_S3_ENDPOINT"]; set {
		t.Error("COOP_S3_ENDPOINT must not be set when CoopS3Endpoint is empty")
	}
	// The rest of the S3 wiring (bucket/prefix/region) is unaffected by this
	// feature and should be present as before.
	if spec.Env["COOP_S3_BUCKET"] != "gardenio-sessions" {
		t.Errorf("COOP_S3_BUCKET: expected %q, got %q", "gardenio-sessions", spec.Env["COOP_S3_BUCKET"])
	}
	if spec.Env["COOP_S3_REGION"] != "us-east-1" {
		t.Errorf("COOP_S3_REGION: expected %q, got %q", "us-east-1", spec.Env["COOP_S3_REGION"])
	}
}

// TestApplyCommonConfig_ObjectStoreCredentials_EndpointSetNoSecret verifies
// that endpoint mode without a credentials secret configured (e.g. a MinIO
// deployment that hasn't wired COOP_S3_CREDENTIALS_SECRET yet) leaves the
// credential plumbing untouched while still setting COOP_S3_ENDPOINT.
func TestApplyCommonConfig_ObjectStoreCredentials_EndpointSetNoSecret(t *testing.T) {
	cfg := &config.Config{
		CoopS3Bucket:   "gardenio-sessions",
		CoopS3Endpoint: "http://minio.gardenio.svc:9000",
		ProjectCache:   map[string]config.ProjectCacheEntry{},
	}
	spec := &podmanager.AgentPodSpec{
		Project: "myproject",
		Env:     map[string]string{},
	}
	applyCommonConfig(cfg, spec)

	if spec.Env["COOP_S3_ENDPOINT"] != "http://minio.gardenio.svc:9000" {
		t.Errorf("COOP_S3_ENDPOINT: expected endpoint to be set, got %q", spec.Env["COOP_S3_ENDPOINT"])
	}
	if spec.ObjectStoreCredentialsSecret != "" {
		t.Errorf("ObjectStoreCredentialsSecret must stay unset without a configured secret, got %q",
			spec.ObjectStoreCredentialsSecret)
	}
	if _, set := spec.Env["COOP_S3_ACCESS_KEY_ID_FILE"]; set {
		t.Error("COOP_S3_ACCESS_KEY_ID_FILE must not be set without a configured secret")
	}
}

// TestApplyCommonConfig_SetsCoopHost verifies that the controller explicitly
// sets COOP_HOST=0.0.0.0 on agent pods. The coop CLI default is 127.0.0.1
// (safe-bind for laptops/dev); cluster-reachable callers like the
// statusreporter need the agent to bind to all interfaces. See ka-0000003486.
func TestApplyCommonConfig_SetsCoopHost(t *testing.T) {
	cfg := &config.Config{}
	spec := &podmanager.AgentPodSpec{
		Project: "myproject",
		Env:     map[string]string{},
	}
	applyCommonConfig(cfg, spec)

	got, ok := spec.Env["COOP_HOST"]
	if !ok {
		t.Fatal("COOP_HOST not set in spec.Env")
	}
	if got != "0.0.0.0" {
		t.Errorf("COOP_HOST = %q, want %q", got, "0.0.0.0")
	}
}

// TestApplyCommonConfig_CoopHostSetEvenWithoutCoopmux ensures COOP_HOST is set
// regardless of whether COOP_BROKER_URL/COOPMUX_URL is wired — bind behavior
// must not depend on coopmux configuration.
func TestApplyCommonConfig_CoopHostSetEvenWithoutCoopmux(t *testing.T) {
	cfg := &config.Config{
		// CoopmuxURL deliberately empty.
	}
	spec := &podmanager.AgentPodSpec{
		Project: "myproject",
		Env:     map[string]string{},
	}
	applyCommonConfig(cfg, spec)

	if _, ok := spec.Env["COOP_BROKER_URL"]; ok {
		t.Error("did not expect COOP_BROKER_URL to be set when CoopmuxURL is empty")
	}
	if got := spec.Env["COOP_HOST"]; got != "0.0.0.0" {
		t.Errorf("COOP_HOST = %q, want %q", got, "0.0.0.0")
	}
}

// TestBuildSpecFromBeadInfo_SetsCoopHost verifies COOP_HOST=0.0.0.0 ends up
// on the final spec produced by BuildSpecFromBeadInfo (the entry point used
// by the reconciler/subscriber).
func TestBuildSpecFromBeadInfo_SetsCoopHost(t *testing.T) {
	cfg := &config.Config{
		Namespace: "gardenio",
	}
	spec := BuildSpecFromBeadInfo(cfg, "gardenio", "crew", "crew", "agent1", map[string]string{})

	if got := spec.Env["COOP_HOST"]; got != "0.0.0.0" {
		t.Errorf("COOP_HOST = %q, want %q", got, "0.0.0.0")
	}
}

// Env-side Grafana Editor token bundle (task ka-0000002337, design §1).

func TestApplyCommonConfig_GrafanaEnvBundle_InjectsTokensAndURLs(t *testing.T) {
	cfg := &config.Config{
		GrafanaEnvAPIKeySecret: "grafana-api-key",
		GrafanaEnvList:         []string{"devel", "perf", "prod"},
		GrafanaEnvBaseURLs: map[string]string{
			"devel": "https://lens.devel.acme.net/grafana",
			"prod":  "https://lens.acme.net/grafana",
		},
		ProjectCache: map[string]config.ProjectCacheEntry{},
	}
	spec := &podmanager.AgentPodSpec{Project: "myproject", Env: map[string]string{}}
	applyCommonConfig(cfg, spec)

	want := map[string]string{
		"GRAFANA_API_KEY_DEVEL": "token-devel",
		"GRAFANA_API_KEY_PERF":  "token-perf",
		"GRAFANA_API_KEY_PROD":  "token-prod",
	}
	got := map[string]string{}
	for _, se := range spec.SecretEnv {
		if _, ok := want[se.EnvName]; ok {
			got[se.EnvName] = se.SecretKey
			if se.SecretName != "grafana-api-key" {
				t.Errorf("%s: SecretName = %q, want grafana-api-key", se.EnvName, se.SecretName)
			}
			if !se.Optional {
				t.Errorf("%s: expected Optional=true for incremental provisioning", se.EnvName)
			}
		}
	}
	for env, key := range want {
		if got[env] != key {
			t.Errorf("missing SecretEnv %s (key %s); got %v", env, key, got)
		}
	}

	if spec.Env["GRAFANA_BASE_URL_DEVEL"] != "https://lens.devel.acme.net/grafana" {
		t.Errorf("GRAFANA_BASE_URL_DEVEL = %q", spec.Env["GRAFANA_BASE_URL_DEVEL"])
	}
	if spec.Env["GRAFANA_BASE_URL_PROD"] != "https://lens.acme.net/grafana" {
		t.Errorf("GRAFANA_BASE_URL_PROD = %q", spec.Env["GRAFANA_BASE_URL_PROD"])
	}
	if _, ok := spec.Env["GRAFANA_BASE_URL_PERF"]; ok {
		t.Error("GRAFANA_BASE_URL_PERF should be omitted when no URL is set")
	}
}

func TestApplyCommonConfig_GrafanaEnvBundle_DisabledByDefault(t *testing.T) {
	cfg := &config.Config{ProjectCache: map[string]config.ProjectCacheEntry{}}
	spec := &podmanager.AgentPodSpec{Project: "myproject", Env: map[string]string{}}
	applyCommonConfig(cfg, spec)

	for _, se := range spec.SecretEnv {
		if strings.HasPrefix(se.EnvName, "GRAFANA_API_KEY_") {
			t.Errorf("unexpected grafana token SecretEnv %s when bundle unset", se.EnvName)
		}
	}
	for k := range spec.Env {
		if strings.HasPrefix(k, "GRAFANA_BASE_URL_") {
			t.Errorf("unexpected %s when bundle unset", k)
		}
	}
}

// TestApplyCommonConfig_LogsURL_InjectedWhenSet is the guard for the `gb logs`
// read-side affordance (design ka-0000005579, task ka-0000005209): when the
// controller resolves an in-cluster VictoriaLogs base (GARDENIO_LOGS_URL), it
// forwards it onto every agent pod as GARDENIO_LOGS_URL — the same env-injection
// shape as GRAFANA_URL — so `gb logs <component>` resolves the store without the
// agent hand-discovering the svc. Empty = not injected (gb logs then falls back
// to the well-known Service name).
func TestApplyCommonConfig_LogsURL_InjectedWhenSet(t *testing.T) {
	cfg := &config.Config{
		LogsURL:      "http://gardenio-victoria-logs:9428",
		ProjectCache: map[string]config.ProjectCacheEntry{},
	}
	spec := &podmanager.AgentPodSpec{Project: "myproject", Env: map[string]string{}}
	applyCommonConfig(cfg, spec)
	if spec.Env["GARDENIO_LOGS_URL"] != "http://gardenio-victoria-logs:9428" {
		t.Errorf("GARDENIO_LOGS_URL = %q, want the in-cluster VictoriaLogs base", spec.Env["GARDENIO_LOGS_URL"])
	}
}

func TestApplyCommonConfig_LogsURL_OmittedWhenUnset(t *testing.T) {
	cfg := &config.Config{ProjectCache: map[string]config.ProjectCacheEntry{}}
	spec := &podmanager.AgentPodSpec{Project: "myproject", Env: map[string]string{}}
	applyCommonConfig(cfg, spec)
	if _, ok := spec.Env["GARDENIO_LOGS_URL"]; ok {
		t.Errorf("GARDENIO_LOGS_URL should not be injected when LogsURL is empty; got %q", spec.Env["GARDENIO_LOGS_URL"])
	}
}

// TestApplyAccountConfig_WhitelistFailClosedOnFallback is the ka-0000003061
// regression guard for the final whitelist gate. A role with an account_whitelist
// whose only listed account is NOT in the project pool must bake NO credential —
// not silently fall back to the project/default account. This is the path
// selectAccount's pool filter alone does not cover: the project-cache and
// DefaultAccount fallbacks (and the exhaustion redirect) bypass spread selection.
func TestApplyAccountConfig_WhitelistFailClosedOnFallback(t *testing.T) {
	cfg := &config.Config{
		RoleCache: map[string]config.RoleCacheEntry{
			// The role is PINNED to project-default, but its whitelist names a
			// different account. resolveRoleAccount returns the pinned account, and
			// the final fail-closed whitelist gate in applyAccountConfig must refuse
			// to bake a credential for the forbidden account (ka-0000003061). This is
			// the surviving reachable path for the gate post-Phase-2 (ka-0000004094):
			// the spread pool already filters non-whitelisted candidates, so only a
			// role pin can present a forbidden account to the terminal gate.
			"scoped": {Account: "project-default", AccountWhitelist: []string{"only-this-one"}},
		},
		AccountCache: map[string]config.AccountCacheEntry{
			"project-default": {
				Provider: "anthropic", BaseURL: "http://litellm:4000",
				Model: "anthropic-claude", LiteLLMKey: "sk-default",
			},
		},
	}
	spec := &podmanager.AgentPodSpec{
		Project: "gardenio",
		Role:    "scoped",
		Env:     map[string]string{},
	}
	applyAccountConfig(cfg, spec)

	if spec.ResolvedAccount != "" {
		t.Errorf("fail-closed: ResolvedAccount must be empty, got %q", spec.ResolvedAccount)
	}
	if v, ok := spec.Env["ANTHROPIC_MODEL"]; ok {
		t.Errorf("fail-closed: no credential env must be baked, got ANTHROPIC_MODEL=%q", v)
	}
	if v, ok := spec.Env["BOAT_ACCOUNT"]; ok {
		t.Errorf("fail-closed: BOAT_ACCOUNT must stay unset, got %q", v)
	}
}

// TestApplyAccountConfig_WhitelistAllowsListedAccount verifies the gate is not
// over-eager: when the resolved account IS in the role whitelist, credentials
// bake normally.
func TestApplyAccountConfig_WhitelistAllowsListedAccount(t *testing.T) {
	cfg := &config.Config{
		RoleCache: map[string]config.RoleCacheEntry{
			"scoped": {Account: "allowed-acct", AccountWhitelist: []string{"allowed-acct"}},
		},
		AccountCache: map[string]config.AccountCacheEntry{
			"allowed-acct": {
				Provider: "anthropic", BaseURL: "http://litellm:4000",
				Model: "anthropic-claude", LiteLLMKey: "sk-allowed",
			},
		},
	}
	spec := &podmanager.AgentPodSpec{
		Project: "gardenio",
		Role:    "scoped",
		Env:     map[string]string{},
	}
	applyAccountConfig(cfg, spec)

	if spec.ResolvedAccount != "allowed-acct" {
		t.Errorf("whitelisted account must resolve normally, got ResolvedAccount=%q", spec.ResolvedAccount)
	}
	if spec.Env["ANTHROPIC_MODEL"] != "anthropic-claude" {
		t.Errorf("expected model baked for whitelisted account, got %q", spec.Env["ANTHROPIC_MODEL"])
	}
}

// --- Budget-headroom spawn gate + per-account fallback chain (ka-0000005505) ---

// TestAccountOverBudget covers the opt-in budget-headroom predicate that gates
// spawning below the provider's hard 429 ceiling.
func TestAccountOverBudget(t *testing.T) {
	cases := []struct {
		name string
		acct config.AccountCacheEntry
		want bool
	}{
		{
			name: "no max_budget set is never over budget (opt-in, fleet default)",
			acct: config.AccountCacheEntry{TotalCostUSD: "9999"},
			want: false,
		},
		{
			name: "spend below budget is fine",
			acct: config.AccountCacheEntry{MaxBudget: "100", TotalCostUSD: "40"},
			want: false,
		},
		{
			name: "spend equal to budget redirects (next request would exceed)",
			acct: config.AccountCacheEntry{MaxBudget: "100", TotalCostUSD: "100"},
			want: true,
		},
		{
			name: "spend above budget redirects",
			acct: config.AccountCacheEntry{MaxBudget: "100", TotalCostUSD: "150"},
			want: true,
		},
		{
			name: "since-reset is total minus spend_at_last_reset (under budget)",
			acct: config.AccountCacheEntry{MaxBudget: "100", TotalCostUSD: "180", SpendAtLastReset: "120"},
			want: false, // 180-120 = 60 < 100
		},
		{
			name: "since-reset is total minus spend_at_last_reset (over budget)",
			acct: config.AccountCacheEntry{MaxBudget: "100", TotalCostUSD: "300", SpendAtLastReset: "120"},
			want: true, // 300-120 = 180 >= 100
		},
		{
			name: "zero max_budget is treated as unset",
			acct: config.AccountCacheEntry{MaxBudget: "0", TotalCostUSD: "500"},
			want: false,
		},
		{
			name: "malformed max_budget is fail-open (not over budget)",
			acct: config.AccountCacheEntry{MaxBudget: "lots", TotalCostUSD: "500"},
			want: false,
		},
		{
			name: "malformed total_cost_usd is treated as zero spend",
			acct: config.AccountCacheEntry{MaxBudget: "100", TotalCostUSD: "n/a"},
			want: false,
		},
		{
			name: "negative since-reset (reset snapshot above total) clamps to zero",
			acct: config.AccountCacheEntry{MaxBudget: "100", TotalCostUSD: "10", SpendAtLastReset: "50"},
			want: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := accountOverBudget(c.acct); got != c.want {
				t.Errorf("accountOverBudget(%+v) = %v, want %v", c.acct, got, c.want)
			}
		})
	}
}

// TestIsHealthyAuthorized_BudgetGate verifies the budget gate is wired into the
// health predicate without disturbing the existing triggers.
func TestIsHealthyAuthorized_BudgetGate(t *testing.T) {
	// Over budget but otherwise healthy (has key, active, throttle-allowed): must be unfit.
	over := config.AccountCacheEntry{
		Provider: "anthropic", BaseURL: "http://litellm:4000",
		LiteLLMKey: "sk-live", Status: "active",
		MaxBudget: "100", TotalCostUSD: "150",
	}
	if isHealthyAuthorized(over) {
		t.Error("over-budget healthy account should be unfit")
	}
	// Under budget healthy account: must be fit.
	under := over
	under.TotalCostUSD = "10"
	if !isHealthyAuthorized(under) {
		t.Error("under-budget healthy account should be fit")
	}
	// No budget configured (fleet default): must be fit on budget grounds.
	nobudget := over
	nobudget.MaxBudget = ""
	if !isHealthyAuthorized(nobudget) {
		t.Error("account with no max_budget should be fit on budget grounds")
	}
}

// TestIsHealthyAuthorized_ProbeAuthGate pins the probe-gate (ka-0000002085): an
// account that is healthy on every rate-limit/budget axis but whose read-only
// probe returned 401/403 (probe_auth_status="rejected") must be unfit — it can
// never authenticate, no matter how healthy the pool reads.
func TestIsHealthyAuthorized_ProbeAuthGate(t *testing.T) {
	healthy := config.AccountCacheEntry{
		Provider: "anthropic", BaseURL: "http://litellm:4000",
		LiteLLMKey: "sk-live", Status: "active", ThrottleStatus: "allowed",
	}
	if !isHealthyAuthorized(healthy) {
		t.Fatal("baseline healthy account should be fit")
	}
	authDead := healthy
	authDead.ProbeAuthStatus = "rejected"
	if isHealthyAuthorized(authDead) {
		t.Error("probe-auth-rejected account should be unfit even when pool-healthy")
	}
	// probe_auth_status="ok" is explicitly fit.
	authOK := healthy
	authOK.ProbeAuthStatus = "ok"
	if !isHealthyAuthorized(authOK) {
		t.Error("probe_auth_status=ok account should be fit")
	}
}

func TestApplySlackViewSession(t *testing.T) {
	cfg := &config.Config{SlackViewSessionSecret: "slack-e2e-session", SlackViewSessionRoles: "thread, crew"}
	for _, tc := range []struct {
		name, role string
		cfg        *config.Config
		want       string
	}{
		{"crew mounts", "crew", cfg, "slack-e2e-session"},
		{"thread mounts", "thread", cfg, "slack-e2e-session"},
		{"multi-role with crew mounts", "devops,crew", cfg, "slack-e2e-session"},
		{"other role skipped", "infra", cfg, ""},
		{"empty role skipped", "", cfg, ""},
		{"no secret configured", "crew", &config.Config{SlackViewSessionRoles: "crew"}, ""},
		{"empty role list", "crew", &config.Config{SlackViewSessionSecret: "s"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := &podmanager.AgentPodSpec{Role: tc.role, Env: map[string]string{}}
			applyCommonConfig(tc.cfg, spec)
			if spec.SlackViewSessionSecret != tc.want {
				t.Errorf("SlackViewSessionSecret = %q, want %q", spec.SlackViewSessionSecret, tc.want)
			}
			// The entrypoint exports the env var only when the file exists, so the
			// controller must never set it statically.
			if _, ok := spec.Env["SLACK_VIEW_STORAGE_STATE"]; ok {
				t.Error("controller must not set SLACK_VIEW_STORAGE_STATE")
			}
		})
	}
}

// TestK8sAPIRolesUseDedicatedServiceAccount is the guard for gd-KRHGbBJqRN: a
// role with needs_k8s_api=true gets a mounted API token, so it must resolve to
// a dedicated ServiceAccount and never the fleet-shared gardenio-agent (or the
// controller's default SA, or no SA at all). Checked on both the reconciler path
// (BuildSpecFromBeadInfo) and the events path (buildAgentPodSpec).
func TestK8sAPIRolesUseDedicatedServiceAccount(t *testing.T) {
	tests := []struct {
		name       string
		role       config.RoleCacheEntry
		metadataSA string
		wantShared bool
	}{
		{"dedicated SA on role", config.RoleCacheEntry{NeedsK8sAPI: true, ServiceAccount: "gc-k8s-live"}, "", false},
		{"no SA resolves to the default", config.RoleCacheEntry{NeedsK8sAPI: true}, "", true},
		{"role pins gardenio-agent", config.RoleCacheEntry{NeedsK8sAPI: true, ServiceAccount: "gardenio-agent"}, "", true},
		{"role pins the controller default SA", config.RoleCacheEntry{NeedsK8sAPI: true, ServiceAccount: "release-agent"}, "", true},
		{"agent-level pin back to gardenio-agent is ignored, role SA wins", config.RoleCacheEntry{NeedsK8sAPI: true, ServiceAccount: "gc-k8s-live"}, "gardenio-agent", false},
		{"no token mounted, shared SA is fine", config.RoleCacheEntry{}, "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{
				Namespace:          "test",
				CoopImage:          "agent:latest",
				CoopServiceAccount: "release-agent",
				ProjectCache:       map[string]config.ProjectCacheEntry{},
				RoleCache:          map[string]config.RoleCacheEntry{"k8s-live": tc.role},
			}
			metadata := map[string]string{}
			if tc.metadataSA != "" {
				metadata["service_account"] = tc.metadataSA
			}
			spec := BuildSpecFromBeadInfo(cfg, "gascity", "crew", "k8s-live", "a1", metadata)
			if got := k8sAPIOnSharedServiceAccount(cfg, &spec); got != tc.wantShared {
				t.Errorf("reconciler path: shared=%v want %v (sa=%q needsK8sAPI=%v)", got, tc.wantShared, spec.ServiceAccountName, spec.NeedsK8sAPI)
			}
			event := subscriber.Event{Project: "gascity", Role: "k8s-live", AgentName: "a1", Metadata: metadata}
			espec := buildAgentPodSpec(cfg, event)
			if got := k8sAPIOnSharedServiceAccount(cfg, &espec); got != tc.wantShared {
				t.Errorf("events path: shared=%v want %v (sa=%q needsK8sAPI=%v)", got, tc.wantShared, espec.ServiceAccountName, espec.NeedsK8sAPI)
			}
		})
	}
}
