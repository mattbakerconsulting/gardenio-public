package main

// Regression guards for the two-account Jira split (epic ka-0000003475, task
// ka-0000003053). Two invariants are enforced here BY CONSTRUCTION:
//
//  1. Agent pods carry the browse-only gardenio@acme.example Jira identity — the
//     monorepo project injects JIRA_API_TOKEN from `monorepo-jira-api-token`
//     plus JIRA_EMAIL=gardenio@ / a PROD JIRA_BASE_URL.
//  2. The privileged FSM-writer gardenio2@ token (`jira2-*`, e.g.
//     `gardenio/jira2-api-token`) can NEVER be mounted into any agent pod, for
//     ANY project or role config. The jira-bridge deployment is its only home.
//
// The `{project}-` / `{role}-` prefix guard is necessary but not sufficient:
// a secret named `monorepo-jira2-api-token` or `gardenio-jira2-api-token` would
// satisfy the prefix yet carry the privileged token. isForbiddenPodSecret +
// scrubForbiddenSecretEnv close that gap and FAIL CLOSED. If any of these tests
// go red, a change has re-opened the path for the privileged Jira identity to
// reach an agent pod — do not "fix" the test; fix the wiring.

import (
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/config"
	"gardenio/controller/internal/podmanager"
)

// secretEnvNames returns the K8s Secret names referenced by a spec's SecretEnv.
func secretEnvSecretNames(spec *podmanager.AgentPodSpec) []string {
	names := make([]string, 0, len(spec.SecretEnv))
	for _, e := range spec.SecretEnv {
		names = append(names, e.SecretName)
	}
	return names
}

// assertNoJira2 fails if any mounted secret carries the `jira2` marker.
func assertNoJira2(t *testing.T, spec *podmanager.AgentPodSpec) {
	t.Helper()
	for _, e := range spec.SecretEnv {
		if strings.Contains(strings.ToLower(e.SecretName), "jira2") {
			t.Fatalf("SECURITY REGRESSION: privileged jira2 secret %q (env %q) reached the agent pod spec; it must never be mounted",
				e.SecretName, e.EnvName)
		}
	}
}

// --- Unit: the deny-list predicate itself -----------------------------------

func TestIsForbiddenPodSecret(t *testing.T) {
	forbidden := []string{
		"jira2-api-token",
		"gardenio/jira2-api-token",
		"monorepo-jira2-api-token",
		"gardenio-jira2-api-token",
		"JIRA2-API-TOKEN", // case-insensitive
	}
	for _, name := range forbidden {
		if !isForbiddenPodSecret(name) {
			t.Errorf("isForbiddenPodSecret(%q) = false, want true (privileged Jira writer must be denied)", name)
		}
	}

	// Allowed: the agent-pod gardenio@ Jira token, the LLM `gardenio2` account
	// OAuth credential (unrelated to Jira), and ordinary secrets. Critically,
	// `gardenio-claude-oauth-gardenio2` must NOT be denied — it is the Anthropic
	// OAuth token for the LLM account named "gardenio2", not the Jira writer.
	allowed := []string{
		"monorepo-jira-api-token",
		"monorepo-jira-config",
		"gardenio-claude-oauth-gardenio2",
		"monorepo-git-credentials",
		"",
	}
	for _, name := range allowed {
		if isForbiddenPodSecret(name) {
			t.Errorf("isForbiddenPodSecret(%q) = true, want false (legitimate secret wrongly denied)", name)
		}
	}
}

// TestProdBridgeSecretNameIsForbidden pins the DEPLOY-CONFIG choice to the code
// guard. The prod overlay (deploy-values/gardenio.yaml, Phase 2.1 ka-0000003274)
// binds the jira-bridge to the privileged gardenio2@ writer via K8s Secret
// `gardenio-jira2-api-token` (materialized from SM key gardenio/jira-prod-api-token).
// That secret name MUST stay on the isForbiddenPodSecret deny-list so the
// privileged writer can never be mounted into an agent pod — the bridge
// Deployment is its only home. If a future rename drops the `jira2` marker from
// jiraBridge.jira.secretName, this test goes red: do NOT relax the guard; keep
// the marker in the overlay's secret name (or extend forbiddenPodSecretMarkers).
func TestProdBridgeSecretNameIsForbidden(t *testing.T) {
	const prodBridgeJiraSecret = "gardenio-jira2-api-token" // = deploy-values/gardenio.yaml jiraBridge.jira.secretName
	if !isForbiddenPodSecret(prodBridgeJiraSecret) {
		t.Fatalf("prod jira-bridge secret %q is NOT on the agent-pod deny-list; the privileged gardenio2@ writer could reach an agent pod. Keep the `jira2` marker in jiraBridge.jira.secretName (deploy-values/gardenio.yaml).", prodBridgeJiraSecret)
	}
}

// --- Project-secret injection path ------------------------------------------

// TestApplyCommonConfig_MonorepoGetsGardenioJiraIdentity is the positive case:
// the monorepo project injects the gardenio@ Jira token + PROD env, mirroring the
// live config (JIRA_API_TOKEN <- monorepo-jira-api-token, JIRA_EMAIL=gardenio@,
// JIRA_BASE_URL=https://acme.atlassian.net).
func TestApplyCommonConfig_MonorepoGetsGardenioJiraIdentity(t *testing.T) {
	cfg := &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{
			"monorepo": {
				Secrets: []beadsapi.SecretEntry{
					{Env: "JIRA_API_TOKEN", Secret: "monorepo-jira-api-token", Key: "api-token"},
					{Env: "JIRA_EMAIL", Secret: "monorepo-jira-config", Key: "email"},
					{Env: "JIRA_BASE_URL", Secret: "monorepo-jira-config", Key: "base-url"},
				},
			},
		},
	}
	spec := &podmanager.AgentPodSpec{Project: "monorepo", Env: map[string]string{}}
	applyCommonConfig(cfg, spec)

	var gotToken *podmanager.SecretEnvSource
	for i := range spec.SecretEnv {
		if spec.SecretEnv[i].EnvName == "JIRA_API_TOKEN" {
			gotToken = &spec.SecretEnv[i]
		}
	}
	if gotToken == nil {
		t.Fatalf("expected JIRA_API_TOKEN to be injected; SecretEnv=%v", secretEnvSecretNames(spec))
	}
	if gotToken.SecretName != "monorepo-jira-api-token" {
		t.Errorf("JIRA_API_TOKEN secret: got %q, want monorepo-jira-api-token (gardenio@ identity)", gotToken.SecretName)
	}
	assertNoJira2(t, spec)
}

// TestApplyCommonConfig_ProjectJira2SecretRefused proves a project config that
// tries to mount a jira2-marked secret — even with a valid {project}- prefix —
// is refused. This is the core HARD CONSTRAINT.
func TestApplyCommonConfig_ProjectJira2SecretRefused(t *testing.T) {
	cases := []struct {
		name   string
		secret string
	}{
		{"prefixed jira2 token", "monorepo-jira2-api-token"},
		{"gardenio-prefixed jira2 token", "gardenio-jira2-api-token"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			project, _, _ := strings.Cut(tc.secret, "-")
			cfg := &config.Config{
				ProjectCache: map[string]config.ProjectCacheEntry{
					project: {
						Secrets: []beadsapi.SecretEntry{
							// A benign secret alongside the forbidden one to prove
							// only the forbidden entry is dropped.
							{Env: "JIRA_API_TOKEN", Secret: project + "-jira-api-token", Key: "api-token"},
							{Env: "JIRA_API_TOKEN", Secret: tc.secret, Key: "api-token"},
						},
					},
				},
			}
			spec := &podmanager.AgentPodSpec{Project: project, Env: map[string]string{}}
			applyCommonConfig(cfg, spec)
			assertNoJira2(t, spec)
			// The benign gardenio@ token survives.
			found := false
			for _, e := range spec.SecretEnv {
				if e.SecretName == project+"-jira-api-token" {
					found = true
				}
			}
			if !found {
				t.Errorf("expected benign %q to survive; SecretEnv=%v", project+"-jira-api-token", secretEnvSecretNames(spec))
			}
		})
	}
}

// --- Role-secret injection path ---------------------------------------------

// TestApplyRoleSecrets_Jira2SecretRefused proves a role config cannot smuggle in
// the privileged token via a role-prefixed name either.
func TestApplyRoleSecrets_Jira2SecretRefused(t *testing.T) {
	cfg := &config.Config{
		RoleCache: map[string]config.RoleCacheEntry{
			"fsm-triage": {
				Secrets: []beadsapi.SecretEntry{
					{Env: "JIRA_API_TOKEN", Secret: "fsm-triage-jira2-api-token", Key: "api-token"},
				},
			},
		},
	}
	spec := &podmanager.AgentPodSpec{Role: "fsm-triage", Env: map[string]string{}}
	applyRoleSecrets(cfg, spec)
	assertNoJira2(t, spec)
	if len(spec.SecretEnv) != 0 {
		t.Errorf("expected no secrets mounted, got %v", secretEnvSecretNames(spec))
	}
}

// --- Final catch-all scrub (injection-site agnostic) ------------------------

// TestScrubForbiddenSecretEnv proves the last-line sweep removes a forbidden
// secret regardless of which layer added it (e.g. a future injection site or
// the account-credential path), while leaving legitimate secrets — including
// the LLM `gardenio2` OAuth credential — untouched.
func TestScrubForbiddenSecretEnv(t *testing.T) {
	spec := &podmanager.AgentPodSpec{
		Project: "monorepo",
		Role:    "crew",
		SecretEnv: []podmanager.SecretEnvSource{
			{EnvName: "JIRA_API_TOKEN", SecretName: "monorepo-jira-api-token", SecretKey: "api-token"},
			{EnvName: "CLAUDE_CODE_OAUTH_TOKEN", SecretName: "gardenio-claude-oauth-gardenio2", SecretKey: "token"},
			{EnvName: "JIRA_API_TOKEN", SecretName: "gardenio/jira2-api-token", SecretKey: "api-token"},
		},
	}
	scrubForbiddenSecretEnv(spec)
	assertNoJira2(t, spec)

	kept := map[string]bool{}
	for _, e := range spec.SecretEnv {
		kept[e.SecretName] = true
	}
	if !kept["monorepo-jira-api-token"] {
		t.Error("scrub dropped the legitimate gardenio@ Jira token")
	}
	if !kept["gardenio-claude-oauth-gardenio2"] {
		t.Error("scrub wrongly dropped the LLM gardenio2 OAuth credential (it is not the Jira writer)")
	}
	if len(spec.SecretEnv) != 2 {
		t.Errorf("expected 2 surviving secrets, got %d: %v", len(spec.SecretEnv), secretEnvSecretNames(spec))
	}
}

// TestApplyAccountConfig_Jira2AccountCredentialScrubbed proves the end-to-end
// account path cannot bake a jira2 secret into the pod: even if an account
// bead's credential field pointed at `secret:...jira2...`, the final scrub in
// applyCommonConfig removes it. (The account path normally carries an Anthropic
// OAuth token; this is a defense-in-depth assertion that the choke point holds
// no matter the source.)
func TestApplyAccountConfig_Jira2AccountCredentialScrubbed(t *testing.T) {
	cfg := &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{
			"monorepo": {Account: "rogue"},
		},
		AccountCache: map[string]config.AccountCacheEntry{
			"rogue": {
				Provider:   "anthropic",
				BaseURL:    "http://litellm:4000",
				Credential: "secret:gardenio-jira2-api-token",
				LiteLLMKey: "sk-key",
				Status:     "active",
				Labels:     []string{"project:monorepo"},
			},
		},
	}
	spec := &podmanager.AgentPodSpec{Project: "monorepo", Env: map[string]string{}}
	applyCommonConfig(cfg, spec)
	assertNoJira2(t, spec)
}

// TestBuildSpecFromBeadInfo_MonorepoNoJira2 exercises the full public spec
// builder (reconciler path) for the monorepo project and asserts no jira2
// secret is ever present end-to-end.
func TestBuildSpecFromBeadInfo_MonorepoNoJira2(t *testing.T) {
	cfg := &config.Config{
		Namespace: "test",
		CoopImage: "agent:latest",
		ProjectCache: map[string]config.ProjectCacheEntry{
			"monorepo": {
				Secrets: []beadsapi.SecretEntry{
					{Env: "JIRA_API_TOKEN", Secret: "monorepo-jira-api-token", Key: "api-token"},
				},
				EnvVars: []beadsapi.EnvEntry{
					{Name: "JIRA_EMAIL", Value: "gardenio@acme.example"},
					{Name: "JIRA_BASE_URL", Value: "https://acme.atlassian.net"},
				},
			},
		},
	}
	spec := BuildSpecFromBeadInfo(cfg, "monorepo", "crew", "crew", "agent1", map[string]string{})
	assertNoJira2(t, &spec)

	if spec.Env["JIRA_EMAIL"] != "gardenio@acme.example" {
		t.Errorf("JIRA_EMAIL: got %q, want gardenio@acme.example", spec.Env["JIRA_EMAIL"])
	}
	if spec.Env["JIRA_BASE_URL"] != "https://acme.atlassian.net" {
		t.Errorf("JIRA_BASE_URL: got %q, want the PROD Jira URL", spec.Env["JIRA_BASE_URL"])
	}
}
