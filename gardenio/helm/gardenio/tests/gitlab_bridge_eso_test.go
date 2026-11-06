package tests

import (
	"strings"
	"testing"
)

// The gitlab-bridge consumes GITLAB_API_TOKEN (a REQUIRED secretKeyRef — the
// pod CrashLoops without it) and GITLAB_WEBHOOK_SECRET from K8s Secrets. Before
// gd-9uJXLG0T1i the chart had ExternalSecret blocks for beadsToken / postgres /
// litellm / forwardAuth / jira(api+webhook) / gitlabIdp — but NONE for the
// gitlab-bridge token or webhook secret, so on an ESO-backed env (e.g. new-prod
// prod-gardenio-eks) there was no mechanism to materialize them and
// gitlabBridge.enabled had to stay false. These tests pin the two new
// ExternalSecrets, mirroring the jira ESO pattern:
//   - dormant unless gitlabBridge.enabled (so the current dest deploy, which
//     runs the bridge OFF, is unaffected),
//   - emitting the SAME Secret name + key the deployment's secretKeyRef reads,
//     so a future edit that drifts the ESO target away from the env var would
//     silently reintroduce the CrashLoop.

func TestGitlabBridgeESO_DormantWhenDisabled(t *testing.T) {
	t.Parallel()
	// gitlabBridge OFF (the current new-prod posture): even with remoteRefs set,
	// the two gitlab-bridge ExternalSecrets must NOT render. Render the whole
	// chart (not --show-only) — helm errors "could not find template" when a
	// --show-only target renders empty, which would mask the assertion.
	out := helmTemplate(t, "",
		"externalSecrets.enabled=true",
		"externalSecrets.secretStoreName=aws-secrets-manager",
		"gitlabBridge.enabled=false",
		"externalSecrets.gitlabToken.remoteRef=gardenio/prod/gardenio-gitlab-token",
		"externalSecrets.gitlabWebhookSecret.remoteRef=gardenio/prod/gitlab-webhook-secret",
	)
	if gitlabBridgeExternalSecretCount(out) != 0 {
		t.Fatalf("gitlab-bridge ExternalSecrets must NOT render when gitlabBridge.enabled=false, got:\n%s", out)
	}
}

// gitlabBridgeExternalSecretCount counts ExternalSecret manifests that target a
// gitlab-bridge Secret (by the component label) in a full-chart render.
func gitlabBridgeExternalSecretCount(rendered string) int {
	n := 0
	for doc := range strings.SplitSeq(rendered, "\n---") {
		if strings.Contains(doc, "kind: ExternalSecret") &&
			strings.Contains(doc, "app.kubernetes.io/component: gitlab-bridge") {
			n++
		}
	}
	return n
}

func TestGitlabBridgeESO_RendersWhenEnabled(t *testing.T) {
	t.Parallel()
	// gitlabBridge ON + remoteRefs set: both ExternalSecrets render with the
	// SM key + property and the K8s Secret name + key the bridge consumes.
	out := helmTemplate(t, "templates/external-secrets.yaml",
		"externalSecrets.enabled=true",
		"externalSecrets.secretStoreName=aws-secrets-manager",
		"gitlabBridge.enabled=true",
		"gitlabBridge.gitlab.secretName=gardenio-gitlab-token",
		"gitlabBridge.gitlab.secretKey=token",
		"gitlabBridge.gitlab.webhookSecretName=gitlab-webhook-secret",
		"gitlabBridge.gitlab.webhookSecretKey=webhook-secret",
		"externalSecrets.gitlabToken.remoteRef=gardenio/prod/gardenio-gitlab-token",
		"externalSecrets.gitlabToken.property=token",
		"externalSecrets.gitlabWebhookSecret.remoteRef=gardenio/prod/gitlab-webhook-secret",
		"externalSecrets.gitlabWebhookSecret.property=webhook-secret",
	)

	// API token ExternalSecret: target Secret name + key, source SM key + property.
	if !strings.Contains(out, "name: gardenio-gitlab-token") {
		t.Errorf("API-token ExternalSecret must target the gardenio-gitlab-token Secret, got:\n%s", out)
	}
	if !strings.Contains(out, "key: \"gardenio/prod/gardenio-gitlab-token\"") {
		t.Errorf("API-token ExternalSecret must reference SM key gardenio/prod/gardenio-gitlab-token, got:\n%s", out)
	}
	// The target template key must match gitlabBridge.gitlab.secretKey (token),
	// which is exactly the deployment's GITLAB_API_TOKEN secretKeyRef key.
	if !strings.Contains(out, "token: '{{ .api_token }}'") {
		t.Errorf("API-token ExternalSecret must emit K8s Secret key `token` (matching the deployment secretKeyRef), got:\n%s", out)
	}

	// Webhook-secret ExternalSecret.
	if !strings.Contains(out, "name: gitlab-webhook-secret") {
		t.Errorf("webhook ExternalSecret must target the gitlab-webhook-secret Secret, got:\n%s", out)
	}
	if !strings.Contains(out, "key: \"gardenio/prod/gitlab-webhook-secret\"") {
		t.Errorf("webhook ExternalSecret must reference SM key gardenio/prod/gitlab-webhook-secret, got:\n%s", out)
	}
	if !strings.Contains(out, "webhook-secret: '{{ .webhook_secret }}'") {
		t.Errorf("webhook ExternalSecret must emit K8s Secret key `webhook-secret` (matching the deployment secretKeyRef), got:\n%s", out)
	}
}

func TestGitlabBridgeESO_SkippedWhenRemoteRefEmpty(t *testing.T) {
	t.Parallel()
	// gitlabBridge ON but remoteRefs empty (e.g. a non-ESO env that supplies the
	// Secrets out of band): the chart must skip the ExternalSecrets rather than
	// emit a broken one with an empty SM key.
	out := helmTemplate(t, "",
		"externalSecrets.enabled=true",
		"externalSecrets.secretStoreName=aws-secrets-manager",
		"gitlabBridge.enabled=true",
		"gitlabBridge.gitlab.secretName=gardenio-gitlab-token",
		"gitlabBridge.gitlab.webhookSecretName=gitlab-webhook-secret",
	)
	if gitlabBridgeExternalSecretCount(out) != 0 {
		t.Fatalf("gitlab-bridge ExternalSecrets must be skipped when remoteRef is empty, got:\n%s", out)
	}
}

// TestGitlabBridgeESO_KeyMatchesDeployment is the anti-drift guard: it renders
// BOTH the ExternalSecret and the deployment with the same values and asserts
// the deployment's GITLAB_API_TOKEN / GITLAB_WEBHOOK_SECRET secretKeyRef
// (name+key) is exactly what the ExternalSecret materializes. A future edit that
// changes one without the other reintroduces the CrashLoop (gd-9uJXLG0T1i).
func TestGitlabBridgeESO_KeyMatchesDeployment(t *testing.T) {
	t.Parallel()
	flags := []string{
		"externalSecrets.enabled=true",
		"externalSecrets.secretStoreName=aws-secrets-manager",
		"gitlabBridge.enabled=true",
		"gitlabBridge.gitlab.secretName=gardenio-gitlab-token",
		"gitlabBridge.gitlab.secretKey=token",
		"gitlabBridge.gitlab.webhookSecretName=gitlab-webhook-secret",
		"gitlabBridge.gitlab.webhookSecretKey=webhook-secret",
		"externalSecrets.gitlabToken.remoteRef=gardenio/prod/gardenio-gitlab-token",
		"externalSecrets.gitlabToken.property=token",
		"externalSecrets.gitlabWebhookSecret.remoteRef=gardenio/prod/gitlab-webhook-secret",
		"externalSecrets.gitlabWebhookSecret.property=webhook-secret",
	}
	dep := helmTemplate(t, "templates/gitlab-bridge/deployment.yaml", flags...)

	// The deployment must read GITLAB_API_TOKEN from the same Secret name+key the
	// ESO produces.
	if !strings.Contains(dep, "name: gardenio-gitlab-token") || !strings.Contains(dep, "key: token") {
		t.Errorf("deployment GITLAB_API_TOKEN secretKeyRef must be {name: gardenio-gitlab-token, key: token}, got:\n%s", dep)
	}
	if !strings.Contains(dep, "name: gitlab-webhook-secret") || !strings.Contains(dep, "key: webhook-secret") {
		t.Errorf("deployment GITLAB_WEBHOOK_SECRET secretKeyRef must be {name: gitlab-webhook-secret, key: webhook-secret}, got:\n%s", dep)
	}
}
