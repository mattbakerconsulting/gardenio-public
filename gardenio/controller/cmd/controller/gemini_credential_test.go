package main

import (
	"strings"
	"testing"

	"gardenio/controller/internal/config"
	"gardenio/controller/internal/podmanager"
)

func geminiOAuthAcct() config.AccountCacheEntry {
	return config.AccountCacheEntry{
		Provider:    "gemini",
		Credential:  "secret:gardenio-gemini-login#oauth_creds.json",
		BillingType: "subscription",
		Model:       "gemini-2.5-pro",
		Status:      "active",
		Labels:      []string{"project:gardenio"},
	}
}

func TestGeminiOAuthSecretName(t *testing.T) {
	tests := []struct {
		cred      string
		wantName  string
		wantOAuth bool
		wantErr   bool
	}{
		{"secret:gardenio-gemini-login#oauth_creds.json", "gardenio-gemini-login", true, false},
		{"secret:gardenio-gemini-key#api-key", "", false, false},
		{"secret:gardenio-gemini-key", "", false, false},
		{"env:GEMINI_API_KEY", "", false, false},
		{"", "", false, false},
		{"secret:#oauth_creds.json", "", true, true},
	}
	for _, tt := range tests {
		name, oauth, err := geminiOAuthSecretName(tt.cred)
		if name != tt.wantName || oauth != tt.wantOAuth || (err != nil) != tt.wantErr {
			t.Errorf("geminiOAuthSecretName(%q) = (%q, %v, %v), want (%q, %v, err=%v)",
				tt.cred, name, oauth, err, tt.wantName, tt.wantOAuth, tt.wantErr)
		}
	}
}

// A role-pinned provider=gemini account with an oauth_creds.json credential
// mounts the login Secret and flips the pod to oauth mode, with none of the
// Claude/LiteLLM baking.
func TestApplyAccountConfig_GeminiOAuth(t *testing.T) {
	cfg := &config.Config{
		RoleCache:    map[string]config.RoleCacheEntry{"gemini-crew": {Account: "gardenio-gemini"}},
		AccountCache: map[string]config.AccountCacheEntry{"gardenio-gemini": geminiOAuthAcct()},
		LiteLLMURL:   "http://litellm.example",
		ClaudeModel:  "claude-opus",
	}
	spec := &podmanager.AgentPodSpec{Project: "gardenio", Role: "gemini-crew", AgentName: "a"}
	applyAccountConfig(cfg, spec)

	if spec.SpawnBlocked {
		t.Fatalf("unexpected SpawnBlocked: %s", spec.SpawnBlockReason)
	}
	if spec.GeminiOAuthSecret != "gardenio-gemini-login" {
		t.Errorf("GeminiOAuthSecret = %q", spec.GeminiOAuthSecret)
	}
	if spec.Env["GEMINI_AUTH_MODE"] != "oauth" {
		t.Errorf("GEMINI_AUTH_MODE = %q", spec.Env["GEMINI_AUTH_MODE"])
	}
	if spec.Env["GEMINI_MODEL"] != "gemini-2.5-pro" {
		t.Errorf("GEMINI_MODEL = %q", spec.Env["GEMINI_MODEL"])
	}
	for _, k := range []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_MODEL", "CLAUDE_MODEL", "ANTHROPIC_CUSTOM_HEADERS"} {
		if v, ok := spec.Env[k]; ok {
			t.Errorf("oauth gemini pod must not carry %s (got %q)", k, v)
		}
	}
	if len(spec.SecretEnv) != 0 {
		t.Errorf("SecretEnv = %+v, want none (no CLAUDE_CODE_OAUTH_TOKEN)", spec.SecretEnv)
	}
	if name, kind, ok := credentialSecretRef(*spec); !ok || name != "gardenio-gemini-login" || kind != credentialKindGeminiLogin {
		t.Errorf("credentialSecretRef = (%q, %q, %v): the guard must probe the login Secret", name, kind, ok)
	}
}

func TestApplyAccountConfig_GeminiOAuthMalformedBlocksSpawn(t *testing.T) {
	acct := geminiOAuthAcct()
	acct.Credential = "secret:#oauth_creds.json"
	cfg := &config.Config{
		RoleCache:    map[string]config.RoleCacheEntry{"gemini-crew": {Account: "gardenio-gemini"}},
		AccountCache: map[string]config.AccountCacheEntry{"gardenio-gemini": acct},
	}
	spec := &podmanager.AgentPodSpec{Project: "gardenio", Role: "gemini-crew", AgentName: "a"}
	applyAccountConfig(cfg, spec)
	if !spec.SpawnBlocked || !strings.Contains(spec.SpawnBlockReason, "gardenio-gemini") {
		t.Errorf("want SpawnBlocked naming the account, got blocked=%v reason=%q", spec.SpawnBlocked, spec.SpawnBlockReason)
	}
	if spec.GeminiOAuthSecret != "" {
		t.Errorf("GeminiOAuthSecret = %q, want empty", spec.GeminiOAuthSecret)
	}
}

// An API-key gemini account is not touched by the oauth path.
func TestApplyGeminiAccountConfig_APIKeyAccountFallsThrough(t *testing.T) {
	acct := geminiOAuthAcct()
	acct.Credential = "secret:gardenio-gemini-key#api-key"
	spec := &podmanager.AgentPodSpec{Env: map[string]string{}}
	if applyGeminiAccountConfig(spec, "k", acct) {
		t.Fatal("API-key account must fall through to the existing flow")
	}
	if spec.GeminiOAuthSecret != "" || spec.Env["GEMINI_AUTH_MODE"] != "" {
		t.Errorf("API-key account got oauth wiring: %+v", spec)
	}
}
