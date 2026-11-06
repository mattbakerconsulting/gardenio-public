package tests

import (
	"strings"
	"testing"
)

// Agent-pod Secret allow-list and the gVisor mirror guards for controller-only
// Secrets (gd-q8nBnOP65d).
//
// Invariants locked here:
//   - agents.secretAllowlist renders AGENT_SECRET_ALLOWLIST (comma-joined) on
//     the controller, and nothing when empty (the default).
//   - The Lands App key mirror guard keys on the configured Secret name, so a
//     name without "github-app" in it is still refused.
//   - The controller's Keycloak credentials (gardenio-machine-gardenio-*,
//     keycloak-runtime-admin, keycloak-first-admin) are never mirrored.

func TestAgentSecretAllowlist_DefaultRendersNoEnv(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/controller/deployment.yaml", "agents.enabled=true")
	if strings.Contains(out, "AGENT_SECRET_ALLOWLIST") {
		t.Error("AGENT_SECRET_ALLOWLIST must not render when agents.secretAllowlist is empty")
	}
}

func TestAgentSecretAllowlist_RendersCommaJoined(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/controller/deployment.yaml",
		"agents.enabled=true,agents.secretAllowlist={*-git-credentials,infra-aws-readonly}")
	if got, want := envValue(t, out, "AGENT_SECRET_ALLOWLIST"), "*-git-credentials,infra-aws-readonly"; got != want {
		t.Errorf("AGENT_SECRET_ALLOWLIST = %q, want %q", got, want)
	}
}

func TestGvisorMirror_LandsAppKeyGuardKeysOnConfiguredName(t *testing.T) {
	t.Parallel()
	out := helmTemplateExpectError(t, "templates/gvisor-namespace.yaml",
		"--set", gvisorMirrorBase,
		"--set", "agents.lands.provisioner.githubApp.privateKeySecret=tenant-repo-signer",
		"--set", "gvisor.namespace.secretMirrors[0].name=Tenant-Repo-Signer",
		"--set", "gvisor.namespace.secretMirrors[0].remoteRef=gardenio/x")
	if !strings.Contains(out, "the Lands App private key must never be mirrored") {
		t.Errorf("expected the Lands App key guard to fire, got:\n%s", out)
	}
}

func TestGvisorMirror_ControllerKeycloakSecretsRefused(t *testing.T) {
	t.Parallel()
	for _, mirror := range []string{
		"gvisor.namespace.secretMirrors[0].name=gardenio-machine-gardenio-controller,gvisor.namespace.secretMirrors[0].remoteRef=gardenio/x",
		"gvisor.namespace.secretMirrors[0].name=gardenio-keycloak-runtime-admin,gvisor.namespace.secretMirrors[0].remoteRef=gardenio/x",
		"gvisor.namespace.secretMirrors[0].name=kc,gvisor.namespace.secretMirrors[0].remoteRef=gardenio/gardenio-keycloak-first-admin",
	} {
		t.Run(mirror, func(t *testing.T) {
			out := helmTemplateExpectError(t, "templates/gvisor-namespace.yaml",
				"--set", gvisorMirrorBase, "--set", mirror)
			if !strings.Contains(out, "Keycloak credentials must never be mirrored") {
				t.Errorf("expected the Keycloak guard to fire, got:\n%s", out)
			}
		})
	}
}
