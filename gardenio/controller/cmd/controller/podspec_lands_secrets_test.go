package main

// Regression guards for gd-ej7Iwq8Fuf: the Lands provisioner's two credentials
// (the Lands App private key and the Keycloak client secret that manages every
// land's groups) are controller-only. Both names carry the gardenio- prefix, so
// the {project}- check alone would let a project bead mount them into an agent
// pod. If these go red, do not relax the test; keep the markers in
// forbiddenPodSecretMarkers.

import (
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/config"
	"gardenio/controller/internal/podmanager"
)

func TestLandsSecretsAreForbiddenPodSecrets(t *testing.T) {
	for _, name := range []string{
		"gardenio-lands-github-app",
		"gardenio-lands-provisioner",
		"Gardenio-Lands-GitHub-App",
		"GARDENIO-LANDS-PROVISIONER",
		"crew-lands-github-app-private-key",
		"gardenio/lands-provisioner",
	} {
		if !isForbiddenPodSecret(name) {
			t.Errorf("isForbiddenPodSecret(%q) = false, want true (Lands provisioner credential must be denied)", name)
		}
	}

	// Secrets agent pods do mount must stay mountable. (The kbeads machine
	// role gardenio-machine-lands-provisioner contains the marker but is never
	// a Secret name, so it does not belong here.)
	for _, name := range []string{
		"gardenio-git-credentials",
		"agent-github-token-gd-b",
		"gardenio-github-token",
		"gardenio-claude-oauth-gardenio2",
		"gardenio-lands-contract",
	} {
		if isForbiddenPodSecret(name) {
			t.Errorf("isForbiddenPodSecret(%q) = true, want false (legitimate secret wrongly denied)", name)
		}
	}
}

// TestApplyCommonConfig_ProjectLandsSecretForbidden is the injection-path
// check: a gardenio project bead naming either Lands secret (any casing) gets
// it dropped, while a benign secret beside it still mounts.
func TestApplyCommonConfig_ProjectLandsSecretForbidden(t *testing.T) {
	for _, secret := range []string{
		"gardenio-lands-github-app",
		"gardenio-lands-provisioner",
		"gardenio-Lands-GitHub-App",
		"gardenio-LANDS-PROVISIONER",
	} {
		t.Run(secret, func(t *testing.T) {
			cfg := &config.Config{
				ProjectCache: map[string]config.ProjectCacheEntry{
					"gardenio": {
						Secrets: []beadsapi.SecretEntry{
							{Env: "GIT_TOKEN", Secret: "gardenio-git-credentials", Key: "token"},
							{Env: "LANDS_SECRET", Secret: secret, Key: "private-key"},
						},
					},
				},
			}
			spec := &podmanager.AgentPodSpec{Project: "gardenio", Env: map[string]string{}}
			applyCommonConfig(cfg, spec)

			benign := false
			for _, e := range spec.SecretEnv {
				if e.SecretName == secret {
					t.Fatalf("SECURITY REGRESSION: Lands secret %q (env %q) reached the agent pod spec", e.SecretName, e.EnvName)
				}
				if e.SecretName == "gardenio-git-credentials" {
					benign = true
				}
			}
			if !benign {
				t.Errorf("expected benign gardenio-git-credentials to survive; SecretEnv=%v", secretEnvSecretNames(spec))
			}
		})
	}
}

// TestApplyRoleSecrets_LandsSecretForbidden covers the role path: a role named
// "gardenio" would satisfy the {role}- prefix too.
func TestApplyRoleSecrets_LandsSecretForbidden(t *testing.T) {
	cfg := &config.Config{
		RoleCache: map[string]config.RoleCacheEntry{
			"gardenio": {
				Secrets: []beadsapi.SecretEntry{
					{Env: "LANDS_APP_KEY", Secret: "gardenio-lands-github-app", Key: "private-key"},
					{Env: "LANDS_KC_SECRET", Secret: "gardenio-lands-provisioner", Key: "clientSecret"},
				},
			},
		},
	}
	spec := &podmanager.AgentPodSpec{Role: "gardenio", Env: map[string]string{}}
	applyRoleSecrets(cfg, spec)
	if len(spec.SecretEnv) != 0 {
		t.Errorf("expected no secrets mounted, got %v", secretEnvSecretNames(spec))
	}
}
