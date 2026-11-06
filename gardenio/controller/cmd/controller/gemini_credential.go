package main

import (
	"fmt"
	"strings"

	"gardenio/controller/internal/config"
	"gardenio/controller/internal/podmanager"
)

const (
	// envGeminiAuthMode tells coop's ACP driver and `gb setup gemini` that the
	// pod authenticates with a Google-account login instead of GEMINI_API_KEY.
	envGeminiAuthMode = "GEMINI_AUTH_MODE"
	// geminiAuthModeOAuth is the only non-default value of envGeminiAuthMode.
	geminiAuthModeOAuth = "oauth"
)

// isGeminiAccount reports whether an account's provider is gemini.
func isGeminiAccount(acct config.AccountCacheEntry) bool {
	return strings.EqualFold(strings.TrimSpace(acct.Provider), providerGemini)
}

// geminiOAuthSecretName parses a provider=gemini account credential. It returns
// the Secret name and oauth=true for secret:<name>#oauth_creds.json (a Google
// account login: free tier or a Gemini subscription). Any other credential,
// including secret:<name>#api-key, is the metered API-key path and returns
// oauth=false with no error. A bare "#oauth_creds.json" with no name is an error.
func geminiOAuthSecretName(credential string) (name string, oauth bool, err error) {
	scheme, target, ok := strings.Cut(strings.TrimSpace(credential), ":")
	if !ok || scheme != "secret" {
		return "", false, nil
	}
	name, key, hasKey := strings.Cut(target, "#")
	if !hasKey || key != podmanager.GeminiOAuthFileName {
		return "", false, nil
	}
	if name == "" {
		return "", true, fmt.Errorf("credential %q: empty secret name", credential)
	}
	return name, true, nil
}

// applyGeminiAccountConfig credentials a pod against a provider=gemini account
// that carries a Google-account login (see geminiOAuthSecretName). It mounts the
// login Secret read-only and sets GEMINI_AUTH_MODE=oauth. It returns false for an
// API-key account, which keeps the existing flow untouched. A malformed
// credential blocks the spawn (transient) rather than baking a loginless pod.
func applyGeminiAccountConfig(spec *podmanager.AgentPodSpec, accountName string, acct config.AccountCacheEntry) bool {
	if !isGeminiAccount(acct) {
		return false
	}
	secret, oauth, err := geminiOAuthSecretName(acct.Credential)
	if !oauth {
		return false
	}
	if err != nil {
		spec.SpawnBlocked = true
		spec.SpawnBlockReason = fmt.Sprintf("gemini account %q is misconfigured: %v", accountName, err)
		return true
	}
	spec.GeminiOAuthSecret = secret
	spec.Env[envGeminiAuthMode] = geminiAuthModeOAuth
	if acct.Model != "" {
		// shapeHarnessEnv lets a later role/project/agent model override win by
		// copying ANTHROPIC_MODEL over this.
		spec.Env["GEMINI_MODEL"] = acct.Model
	}
	return true
}
