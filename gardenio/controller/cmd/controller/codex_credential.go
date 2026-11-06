package main

import (
	"fmt"
	"strings"

	"gardenio/controller/internal/config"
	"gardenio/controller/internal/podmanager"
)

// Codex ChatGPT-subscription credential (ADR 0004 §C, contract gd-3gTWiNEhg1).
//
// An account bead with provider=openai carries credential
// secret:<name>#auth.json (e.g. gardenio-codex-auth). Two Secrets exist per
// login: <name> in the controller namespace holds the real auth.json including
// the single-use refresh token and is read only by the controller; the derived
// <name>-session Secret in the agents namespace holds the same document with a
// placeholder refresh_token and is the ONLY one an agent pod ever mounts. The
// refresh keeper (a separate step) is the sole writer of both.

const (
	// providerOpenAI is the account provider for the Codex harness.
	providerOpenAI = "openai"

	// codexSessionSecretSuffix is appended to the credential Secret name to derive
	// the pod-facing Secret: gardenio-codex-auth -> gardenio-codex-auth-session.
	codexSessionSecretSuffix = "-session"

	// envCodexHome is where the codex CLI keeps config.toml and auth.json. It
	// matches the codex harness state mount (podmanager.MountCodexState).
	envCodexHome = "CODEX_HOME"
	// codexHomeDir is CODEX_HOME inside the agent pod.
	codexHomeDir = "/home/agent/.codex"
	// envCodexModel carries the account's default Codex model.
	envCodexModel = "CODEX_MODEL"
)

// isOpenAIAccount reports whether an account is a Codex subscription login
// (provider=openai). Comparison is case- and whitespace-insensitive, like the
// other provider checks.
func isOpenAIAccount(acct config.AccountCacheEntry) bool {
	return strings.EqualFold(strings.TrimSpace(acct.Provider), providerOpenAI)
}

// codexAuthSecretName parses an account credential of the form
// secret:<name>[#auth.json] into the controller-side Secret name holding the real
// auth.json. Any other scheme, an empty name, or a key other than auth.json is an
// error: a Codex account must never fall back to a Claude-style token bake.
func codexAuthSecretName(credential string) (string, error) {
	scheme, target, ok := strings.Cut(strings.TrimSpace(credential), ":")
	if !ok || scheme != "secret" || target == "" {
		return "", fmt.Errorf("credential %q: a provider=openai account needs secret:<name>#%s", credential, podmanager.CodexAuthFileName)
	}
	name, key, hasKey := strings.Cut(target, "#")
	if name == "" {
		return "", fmt.Errorf("credential %q: empty secret name", credential)
	}
	if hasKey && key != podmanager.CodexAuthFileName {
		return "", fmt.Errorf("credential %q: key %q is not %q", credential, key, podmanager.CodexAuthFileName)
	}
	return name, nil
}

// codexSessionSecretName derives the pod-facing -session Secret name from an
// account credential (see codexAuthSecretName).
func codexSessionSecretName(credential string) (string, error) {
	name, err := codexAuthSecretName(credential)
	if err != nil {
		return "", err
	}
	return name + codexSessionSecretSuffix, nil
}

// applyCodexAccountConfig credentials a pod against a provider=openai account.
// It mounts the derived -session Secret read-only (placeholder refresh token) and
// sets CODEX_HOME. It deliberately emits NO CLAUDE_CODE_OAUTH_TOKEN,
// ANTHROPIC_* or CLAUDE_MODEL, and no LiteLLM headers: this is subscription auth,
// not an API key, and LiteLLM is not in the path.
//
// A malformed credential blocks the spawn (transient, like the other
// credential-less refusals) rather than baking a loginless pod.
func applyCodexAccountConfig(spec *podmanager.AgentPodSpec, accountName string, acct config.AccountCacheEntry) {
	session, err := codexSessionSecretName(acct.Credential)
	if err != nil {
		spec.SpawnBlocked = true
		spec.SpawnBlockReason = fmt.Sprintf("codex account %q is misconfigured: %v", accountName, err)
		return
	}
	spec.CodexAuthSecret = session
	spec.Env[envCodexHome] = codexHomeDir
	if acct.Model != "" {
		spec.Env[envCodexModel] = acct.Model
	}
}

// accountMatchesHarness reports whether an account's provider can credential a
// pod running the given harness (ADR 0004 §4, ADR 0005 §5 B), per the
// harnessProviders table: codex needs provider=openai, gemini needs
// provider=gemini, claude (or "" before the harness is resolved) needs any
// provider the others do not claim. It is the provider half of account
// selection, applied before health and load scoring so a pod is never handed
// another harness's credential.
func accountMatchesHarness(acct config.AccountCacheEntry, harness string) bool {
	return harnessAcceptsAccount(acct, harness)
}

// isHealthyAuthorizedFor is isHealthyAuthorized restricted to the harness's
// providers: an account of the wrong provider is never fit for the pod, however
// healthy it is.
func isHealthyAuthorizedFor(acct config.AccountCacheEntry, harness string) bool {
	return accountMatchesHarness(acct, harness) && isHealthyAuthorized(acct)
}

// resolveSpendAccountIsOpenAI reports whether the session's project/role resolves
// (role pin, then project account) to a provider=openai account. Used by the
// spend reconciler to skip subscription accounts, which carry no LiteLLM spend.
func resolveSpendAccountIsOpenAI(cfg *config.Config, project, role string) bool {
	account := resolveSpendAccount(cfg, project, role)
	if account == "" {
		return false
	}
	cfg.ProjectCacheMu.RLock()
	defer cfg.ProjectCacheMu.RUnlock()
	acct, ok := cfg.AccountCache[account]
	return ok && isOpenAIAccount(acct)
}
