package main

import (
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/config"
)

// providerGemini is the account provider for the Gemini harness (a Gemini API
// key account, ADR 0005 §C). coop's credential broker already pushes it as
// GEMINI_API_KEY.
const providerGemini = "gemini"

// harnessProviders is the single harness -> account.provider table (ADR 0005
// §5 B). It replaces the codex/non-codex `if` that used to live in
// applyHarness, accountMatchesHarness and the spread pool filter, so adding a
// harness is one row here instead of another copy of the branch.
//
// The default harness (claude) deliberately has no row: it is credentialed by
// every provider no other harness claims (anthropic, bedrock, a LiteLLM-fronted
// model, an unset provider). That keeps the existing "anything but openai is
// Claude" behaviour for providers nobody has named, while guaranteeing a Claude
// pod is never handed a Codex or Gemini account and the reverse. Provider names
// are matched case- and whitespace-insensitively.
var harnessProviders = map[string][]string{
	beadsapi.HarnessCodex:  {providerOpenAI},
	beadsapi.HarnessGemini: {providerGemini},
}

// normalizeHarnessName maps the unresolved harness ("") onto the default.
func normalizeHarnessName(harness string) string {
	if h := strings.TrimSpace(harness); h != "" {
		return h
	}
	return beadsapi.DefaultHarness
}

// providerHarness returns the harness an account.provider credentials: the one
// whose harnessProviders row names it, else the default harness.
func providerHarness(provider string) string {
	p := strings.ToLower(strings.TrimSpace(provider))
	for h, providers := range harnessProviders {
		for _, want := range providers {
			if p == want {
				return h
			}
		}
	}
	return beadsapi.DefaultHarness
}

// harnessNeedsProvider reports the provider a harness must be credentialed with
// ("" for the default harness, which accepts any unclaimed provider).
func harnessNeedsProvider(harness string) string {
	if providers := harnessProviders[normalizeHarnessName(harness)]; len(providers) > 0 {
		return providers[0]
	}
	return ""
}

// harnessAcceptsAccount reports whether acct's provider can credential harness.
// For the claude harness, a provider=openai or provider=gemini account never
// can, so it must not make a project look "credentialed but exhausted" (which
// would block Claude spawns) nor enter the Claude spread pool.
func harnessAcceptsAccount(acct config.AccountCacheEntry, harness string) bool {
	return providerHarness(acct.Provider) == normalizeHarnessName(harness)
}
