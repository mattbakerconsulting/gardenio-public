package main

// Regression guards for gd-q8nBnOP65d: the controller's Keycloak credentials
// (gardenio-machine-gardenio-<client>, gardenio-keycloak-runtime-admin,
// gardenio-keycloak-first-admin) all pass the gardenio- prefix, so a gardenio
// project bead, a role named after their leading words, or an account
// credential could mount Keycloak admin into an agent pod. The markers in
// forbiddenPodSecretMarkers refuse them always; AGENT_SECRET_ALLOWLIST refuses
// every bead-supplied Secret it does not name. If these go red, do not relax
// the test.

import (
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/config"
	"gardenio/controller/internal/podmanager"
)

var controllerKeycloakSecrets = []string{
	"gardenio-machine-gardenio-controller",
	"gardenio-machine-gardenio-coopmux",
	"gardenio-machine-gardenio-slack-bridge",
	"gardenio-keycloak-runtime-admin",
	"gardenio-keycloak-first-admin",
	"Gardenio-Keycloak-Runtime-Admin",
}

func TestControllerKeycloakSecretsAreForbiddenPodSecrets(t *testing.T) {
	for _, name := range controllerKeycloakSecrets {
		if !isForbiddenPodSecret(name) {
			t.Errorf("isForbiddenPodSecret(%q) = false, want true (controller Keycloak credential)", name)
		}
	}
	for _, name := range []string{
		"gardenio-git-credentials",
		"infra-aws-readonly",
		"gardenio-claude-oauth-fimbaz",
		"claude-oauth",
	} {
		if isForbiddenPodSecret(name) {
			t.Errorf("isForbiddenPodSecret(%q) = true, want false (legitimate secret wrongly denied)", name)
		}
	}
}

// prodAllowlist mirrors the AGENT_SECRET_ALLOWLIST deploy-values/gardenio-k3s.yaml sets.
var prodAllowlist = []string{"*-git-credentials", "infra-aws-readonly", "gardenio-claude-oauth-*", "claude-oauth"}

func secretEnvHas(spec *podmanager.AgentPodSpec, secret string) bool {
	for _, e := range spec.SecretEnv {
		if e.SecretName == secret {
			return true
		}
	}
	return false
}

// TestApplyCommonConfig_ProjectControllerSecretsRefused is the project path,
// with and without an allow-list: the Keycloak secrets never mount, the git
// credentials beside them do.
func TestApplyCommonConfig_ProjectControllerSecretsRefused(t *testing.T) {
	for _, allow := range [][]string{nil, prodAllowlist} {
		for _, secret := range controllerKeycloakSecrets {
			cfg := &config.Config{
				AgentSecretAllowlist: allow,
				ProjectCache: map[string]config.ProjectCacheEntry{
					"gardenio": {Secrets: []beadsapi.SecretEntry{
						{Env: "GIT_TOKEN", Secret: "gardenio-git-credentials", Key: "token"},
						{Env: "KC_SECRET", Secret: secret, Key: "clientSecret"},
					}},
				},
			}
			spec := &podmanager.AgentPodSpec{Project: "gardenio", Env: map[string]string{}}
			applyCommonConfig(cfg, spec)
			if secretEnvHas(spec, secret) {
				t.Fatalf("SECURITY REGRESSION (allowlist=%v): %q reached the agent pod spec", allow, secret)
			}
			if !secretEnvHas(spec, "gardenio-git-credentials") {
				t.Errorf("allowlist=%v: gardenio-git-credentials should still mount; SecretEnv=%v", allow, secretEnvSecretNames(spec))
			}
		}
	}
}

// TestApplyRoleSecrets_ControllerSecretsRefused is the role path: roles named
// "gardenio-machine" and "gardenio-keycloak" pass the {role}- prefix for the
// Keycloak secrets.
func TestApplyRoleSecrets_ControllerSecretsRefused(t *testing.T) {
	for _, allow := range [][]string{nil, prodAllowlist} {
		cfg := &config.Config{
			AgentSecretAllowlist: allow,
			RoleCache: map[string]config.RoleCacheEntry{
				"gardenio-machine": {Secrets: []beadsapi.SecretEntry{
					{Env: "KC_SECRET", Secret: "gardenio-machine-gardenio-controller", Key: "clientSecret"},
				}},
				"gardenio-keycloak": {Secrets: []beadsapi.SecretEntry{
					{Env: "KC_ADMIN", Secret: "gardenio-keycloak-runtime-admin", Key: "password"},
					{Env: "KC_FIRST", Secret: "gardenio-keycloak-first-admin", Key: "password"},
				}},
				"infra": {Secrets: []beadsapi.SecretEntry{
					{Env: "AWS_ACCESS_KEY_ID", Secret: "infra-aws-readonly", Key: "access-key-id"},
				}},
			},
		}
		spec := &podmanager.AgentPodSpec{Role: "gardenio-machine,gardenio-keycloak,infra", Env: map[string]string{}}
		applyRoleSecrets(cfg, spec)
		if got := secretEnvSecretNames(spec); len(got) != 1 || got[0] != "infra-aws-readonly" {
			t.Errorf("allowlist=%v: SecretEnv=%v, want only infra-aws-readonly", allow, got)
		}
	}
}

func accountCredentialSpec(allow []string, credential string) *podmanager.AgentPodSpec {
	cfg := &config.Config{
		AgentSecretAllowlist: allow,
		ProjectCache: map[string]config.ProjectCacheEntry{
			"gardenio": {Account: "acct"},
		},
		AccountCache: map[string]config.AccountCacheEntry{
			"acct": {
				Provider:   "anthropic",
				BaseURL:    "http://litellm:4000",
				Credential: credential,
				LiteLLMKey: "sk-key",
				Status:     "active",
				Labels:     []string{"project:gardenio"},
			},
		},
	}
	spec := &podmanager.AgentPodSpec{Project: "gardenio", Env: map[string]string{}}
	applyCommonConfig(cfg, spec)
	return spec
}

// TestApplyAccountConfig_ControllerSecretCredentialRefused is the account
// path, which has no name prefix at all. The positive control proves the
// account was selected, so the refusal is not vacuous.
func TestApplyAccountConfig_ControllerSecretCredentialRefused(t *testing.T) {
	for _, allow := range [][]string{nil, prodAllowlist} {
		if spec := accountCredentialSpec(allow, "secret:gardenio-claude-oauth-fimbaz"); !secretEnvHas(spec, "gardenio-claude-oauth-fimbaz") {
			t.Fatalf("allowlist=%v: control: OAuth credential should mount; SecretEnv=%v", allow, secretEnvSecretNames(spec))
		}
		for _, secret := range controllerKeycloakSecrets {
			if spec := accountCredentialSpec(allow, "secret:"+secret); secretEnvHas(spec, secret) {
				t.Fatalf("SECURITY REGRESSION (allowlist=%v): account credential %q reached the agent pod spec", allow, secret)
			}
		}
	}
}

// TestAgentSecretAllowlist_RefusesUnlistedBeadSecrets: with an allow-list set,
// a controller-only Secret no marker names (gardenio-forwardauth-secrets, the
// oauth2-proxy client secret) is refused on every bead path, while the
// controller's own configured Secrets are untouched.
func TestAgentSecretAllowlist_RefusesUnlistedBeadSecrets(t *testing.T) {
	const unlisted = "gardenio-forwardauth-secrets"
	if isForbiddenPodSecret(unlisted) {
		t.Fatalf("%s is marker-forbidden; pick a name only the allow-list catches", unlisted)
	}
	cfg := &config.Config{
		AgentSecretAllowlist:   prodAllowlist,
		SlackBridgeTokenSecret: "gardenio-slack-bridge-agent-token",
		ProjectCache: map[string]config.ProjectCacheEntry{
			"gardenio": {Secrets: []beadsapi.SecretEntry{
				{Env: "GIT_TOKEN", Secret: "gardenio-git-credentials", Key: "token"},
				{Env: "FA", Secret: unlisted, Key: "client-secret"},
			}},
		},
		RoleCache: map[string]config.RoleCacheEntry{
			"gardenio": {Secrets: []beadsapi.SecretEntry{
				{Env: "FA2", Secret: unlisted, Key: "cookie-secret"},
			}},
		},
	}
	spec := &podmanager.AgentPodSpec{Project: "gardenio", Role: "gardenio", Env: map[string]string{}}
	applyCommonConfig(cfg, spec)
	if secretEnvHas(spec, unlisted) {
		t.Fatalf("%s reached the agent pod despite the allow-list; SecretEnv=%v", unlisted, secretEnvSecretNames(spec))
	}
	for _, want := range []string{"gardenio-git-credentials", "gardenio-slack-bridge-agent-token"} {
		if !secretEnvHas(spec, want) {
			t.Errorf("%s should mount; SecretEnv=%v", want, secretEnvSecretNames(spec))
		}
	}

	if spec := accountCredentialSpec(prodAllowlist, "secret:"+unlisted); secretEnvHas(spec, unlisted) {
		t.Fatalf("account credential %s reached the agent pod despite the allow-list", unlisted)
	}
	if spec := accountCredentialSpec(prodAllowlist, "secret:claude-oauth"); !secretEnvHas(spec, "claude-oauth") {
		t.Errorf("allow-listed account credential claude-oauth should mount; SecretEnv=%v", secretEnvSecretNames(spec))
	}
}
