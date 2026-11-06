package main

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"gardenio/controller/internal/config"
	"gardenio/controller/internal/subscriber"
)

func TestHarnessProviders_Table(t *testing.T) {
	tests := []struct {
		provider, harness string
		want              bool
	}{
		{"anthropic", "claude", true},
		{"bedrock", "claude", true},
		{"", "claude", true},
		{"", "", true}, // unresolved harness is claude
		{"openai", "claude", false},
		{"gemini", "claude", false},
		{" Gemini ", "claude", false},
		{"openai", "codex", true},
		{" OpenAI ", "codex", true},
		{"anthropic", "codex", false},
		{"gemini", "codex", false},
		{"gemini", "gemini", true},
		{" Gemini ", "gemini", true},
		{"openai", "gemini", false},
		{"anthropic", "gemini", false},
		{"", "gemini", false},
	}
	for _, tc := range tests {
		got := accountMatchesHarness(config.AccountCacheEntry{Provider: tc.provider}, tc.harness)
		if got != tc.want {
			t.Errorf("accountMatchesHarness(provider=%q, harness=%q) = %v, want %v", tc.provider, tc.harness, got, tc.want)
		}
	}
	if got := harnessNeedsProvider("gemini"); got != "gemini" {
		t.Errorf("harnessNeedsProvider(gemini) = %q", got)
	}
	if got := harnessNeedsProvider("claude"); got != "" {
		t.Errorf("harnessNeedsProvider(claude) = %q, want empty", got)
	}
}

// geminiCfg is the harness test config with a gemini API-key account labeled
// for the project and the gemini harness switched on or off.
func geminiCfg(roleHarness string, enabled bool) *config.Config {
	cfg := newHarnessCfg("", "", roleHarness)
	cfg.GeminiHarnessEnabled = enabled
	cfg.AccountCache["gemini-acct"] = config.AccountCacheEntry{
		Provider: "gemini", Credential: "secret:gardenio-gemini-key#api-key",
		Model: "gemini-2.5-pro", Labels: []string{"project:gardenio"},
	}
	return cfg
}

// The gemini harness is refused at spawn until the flag is on, whatever the
// accounts look like, and the refusal is a permanent harness refusal.
func TestHarness_GeminiGatedByFlag(t *testing.T) {
	cfg := pinRoleAccount(geminiCfg("gemini", false), "gemini-acct")
	spec := buildAgentPodSpec(cfg, subscriber.Event{Project: "gardenio", Role: "crew", AgentName: "a", Metadata: map[string]string{}})
	if !strings.Contains(spec.HarnessError, "GEMINI_HARNESS_ENABLED") {
		t.Fatalf("HarnessError = %q, want the flag named", spec.HarnessError)
	}
	r := evaluateSpawnCredentials(context.Background(), nil, slog.Default(), spec)
	if !r.refuse || !r.permanent || !r.harness {
		t.Errorf("refusal = %+v, want refuse+permanent+harness", r)
	}

	cfg.GeminiHarnessEnabled = true
	spec = buildAgentPodSpec(cfg, subscriber.Event{Project: "gardenio", Role: "crew", AgentName: "a", Metadata: map[string]string{}})
	if spec.HarnessError != "" || spec.Harness != "gemini" || spec.ResolvedAccount != "gemini-acct" {
		t.Errorf("Harness=%q err=%q account=%q, want gemini on gemini-acct", spec.Harness, spec.HarnessError, spec.ResolvedAccount)
	}
}

// A gemini spec with a mismatched or missing account is refused naming both
// values; it never falls back to another account.
func TestHarness_GeminiProviderMismatchRefused(t *testing.T) {
	tests := []struct {
		name, account, role, wantInErr string
	}{
		{"gemini on anthropic account", "gardenio", "gemini", `needs an account with provider "gemini", but account "gardenio" has provider "anthropic"`},
		{"gemini on openai account", "codex-acct", "gemini", `needs an account with provider "gemini"`},
		{"claude on gemini account", "gemini-acct", "claude", `cannot use account "gemini-acct" (provider "gemini"); set harness=gemini`},
		{"codex on gemini account", "gemini-acct", "codex", `needs an account with provider "openai", but account "gemini-acct" has provider "gemini"`},
		{"gemini with no account resolved", "", "gemini", `needs an account with provider "gemini", but no account resolved`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := geminiCfg(tc.role, true)
			cfg.AccountCache = map[string]config.AccountCacheEntry{}
			for _, n := range []string{"gardenio", "codex-acct", "gemini-acct"} {
				if n == tc.account {
					cfg.AccountCache[n] = geminiCfg(tc.role, true).AccountCache[n]
				}
			}
			if tc.account != "" {
				pinRoleAccount(cfg, tc.account)
			}
			spec := buildAgentPodSpec(cfg, subscriber.Event{Project: "gardenio", Role: "crew", AgentName: "a", Metadata: map[string]string{}})
			if !strings.Contains(spec.HarnessError, tc.wantInErr) {
				t.Fatalf("HarnessError = %q, want containing %q", spec.HarnessError, tc.wantInErr)
			}
			r := evaluateSpawnCredentials(context.Background(), nil, slog.Default(), spec)
			if !r.refuse || !r.permanent || !r.harness {
				t.Errorf("refusal = %+v, want refuse+permanent+harness", r)
			}
		})
	}
}

// selectAccount's pre-scoring filter: each harness spreads only over its own
// providers, so a Claude spec never lands on the gemini account and a gemini
// spec lands on it with the claude and openai accounts in the same pool.
func TestHarness_SpreadFilterIsTableDriven(t *testing.T) {
	for harness, want := range map[string]string{"gemini": "gemini-acct", "codex": "codex-acct", "claude": "gardenio"} {
		cfg := geminiCfg(harness, true)
		spec := buildAgentPodSpec(cfg, subscriber.Event{Project: "gardenio", Role: "crew", AgentName: "a", Metadata: map[string]string{}})
		if spec.HarnessError != "" {
			t.Fatalf("%s: unexpected HarnessError %q", harness, spec.HarnessError)
		}
		if spec.ResolvedAccount != want {
			t.Errorf("%s: ResolvedAccount = %q, want %q", harness, spec.ResolvedAccount, want)
		}
	}
}
