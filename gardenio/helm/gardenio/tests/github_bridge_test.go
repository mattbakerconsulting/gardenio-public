package tests

import (
	"strings"
	"testing"
)

// github-bridge (task gd-GreWIwME20, plan gd-j5OxXb3ySp): the GitHub App
// webhook receiver. Invariants locked here:
//   - default OFF: nothing github-bridge renders.
//   - the webhook secret and the App key are mounted as FILES, never env vars.
//   - the Deployment is Recreate (the replay cache is in memory).
//   - only /webhook is routed publicly; /metrics and /healthz are not.
//   - the secret ExternalSecret pulls the SSM path the register script writes.
//   - NetworkPolicies exist (the namespace is default-deny).

const (
	ghbOn        = "githubBridge.enabled=true"
	ghbAppFlags  = "githubBridge.githubApp.appId=5029022"
	ghbSecretESO = "githubBridge.webhookSecret.externalSecret.enabled=true"
	ghbStoreName = "githubBridge.webhookSecret.externalSecret.secretStoreName=ssm"
)

func TestGithubBridge_DisabledByDefault(t *testing.T) {
	t.Parallel()
	// The realm role gardenio-machine-github-bridge is declared whether or not
	// the bridge runs (keycloak.realmRoles, like every machine role); it is a
	// name, not a bridge resource.
	out := strings.ReplaceAll(helmTemplate(t, ""), "gardenio-machine-github-bridge", "")
	if strings.Contains(out, "github-bridge") {
		t.Error("nothing github-bridge may render when githubBridge.enabled is false")
	}
}

func TestGithubBridge_DeploymentMountsSecretsAsFilesNotEnv(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/github-bridge/deployment.yaml", ghbOn, ghbAppFlags, "githubBridge.githubApp.installationId=170295668")
	for _, want := range []string{
		"name: GITHUB_WEBHOOK_SECRET_FILE",
		`value: "/etc/gardenio/github-webhook/webhook-secret"`,
		"secretName: gardenio-github-webhook-secret",
		"name: GITHUB_APP_ID",
		`value: "5029022"`,
		"name: GITHUB_APP_PRIVATE_KEY_FILE",
		`value: "/etc/gardenio/github-app/private-key"`,
		"secretName: gardenio-github-app-private-key",
		"name: GITHUB_APP_INSTALLATION_ID",
		"type: Recreate",
		"readOnlyRootFilesystem: true",
		"automountServiceAccountToken: false",
		"containerPort: 8101",
		"image: \"ghcr.io/mattbakerconsulting/gardenio/github-bridge:",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("github-bridge Deployment missing %q", want)
		}
	}
	// The webhook secret and App key may not ride env: the only secretKeyRef is
	// the beads daemon token, and there is no PEM.
	if strings.Count(out, "secretKeyRef") != 1 || strings.Contains(out, "BEGIN") {
		t.Error("github-bridge secrets must be mounted files, never env vars (beads token excepted)")
	}
	// Pipeline events are written onto MR beads, so the bridge reaches the daemon.
	for _, want := range []string{"name: BEADS_HTTP_ADDR", "name: BEADS_DAEMON_TOKEN"} {
		if !strings.Contains(out, want) {
			t.Errorf("github-bridge Deployment missing %q", want)
		}
	}
}

func TestGithubBridge_BeadsTokenSecretOverride(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/github-bridge/deployment.yaml", ghbOn,
		"githubBridge.beads.tokenSecret=bridge-tokens")
	if !strings.Contains(out, "name: bridge-tokens") || !strings.Contains(out, "key: github-bridge") {
		t.Errorf("per-bridge token override not rendered:\n%s", out)
	}
}

// draftAgentPRs (plan gd-WFMvg9sDKE task 1.4) renders nothing by default, so
// the default chart is unchanged, and the env var only when switched on.
func TestGithubBridge_DraftAgentPRsFlag(t *testing.T) {
	t.Parallel()
	const env = "name: GITHUB_BRIDGE_DRAFT_AGENT_PRS"
	if out := helmTemplate(t, "templates/github-bridge/deployment.yaml", ghbOn, ghbAppFlags); strings.Contains(out, env) {
		t.Errorf("default must not render %q", env)
	}
	out := helmTemplate(t, "templates/github-bridge/deployment.yaml", ghbOn, ghbAppFlags, "githubBridge.draftAgentPRs=true")
	if !strings.Contains(out, env+"\n              value: \"true\"") {
		t.Errorf("githubBridge.draftAgentPRs=true did not render %s=true:\n%s", env, out)
	}
}

func TestGithubBridge_AppBlockOmittedWithoutAppID(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/github-bridge/deployment.yaml", ghbOn)
	for _, forbidden := range []string{"GITHUB_APP_", "github-app-private-key"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("without an appId the bridge must not mount or reference %q", forbidden)
		}
	}
}

func TestGithubBridge_RefusesEmptyWebhookSecretName(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "templates/github-bridge/deployment.yaml", ghbOn, "githubBridge.webhookSecret.secretName=")
	if err == nil || !strings.Contains(out, "fails closed without a webhook secret") {
		t.Errorf("an empty webhook secret name must fail the render, err=%v out=%s", err, out)
	}
}

func TestGithubBridge_AppIDRequiresKeySecret(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "templates/github-bridge/deployment.yaml", ghbOn, ghbAppFlags, "githubBridge.githubApp.privateKeySecret=")
	if err == nil || !strings.Contains(out, "privateKeySecret is empty") {
		t.Errorf("appId without a key secret must fail the render, err=%v out=%s", err, out)
	}
}

func TestGithubBridge_ExternalSecretUsesRegisterScriptPath(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/github-bridge/webhook-secret-external-secret.yaml", ghbOn, ghbSecretESO, ghbStoreName)
	for _, want := range []string{
		"kind: ExternalSecret",
		"name: gardenio-github-webhook-secret",
		"secretKey: webhook-secret",
		`key: "/gardenio/github-app/agents/webhook-secret"`,
		"name: ssm",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("webhook ExternalSecret missing %q, got:\n%s", want, out)
		}
	}
	if _, err := helmTemplateAllowFail(t, "templates/github-bridge/webhook-secret-external-secret.yaml", ghbOn, ghbSecretESO); err == nil {
		t.Error("the ExternalSecret must require a secretStoreName")
	}
}

func TestGithubBridge_ExternalSecretOffByDefault(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "", ghbOn)
	if strings.Contains(out, "kind: ExternalSecret\nmetadata:\n  name: gardenio-github-webhook-secret") ||
		strings.Contains(out, "name: test-gardenio-github-webhook-secret") {
		t.Error("the webhook ExternalSecret must not render unless externalSecret.enabled")
	}
}

func TestGithubBridge_ServiceExposesHTTPAndMetrics(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/github-bridge/service.yaml", ghbOn)
	for _, want := range []string{"port: 8101", "port: 9101", "app.kubernetes.io/component: github-bridge"} {
		if !strings.Contains(out, want) {
			t.Errorf("Service missing %q", want)
		}
	}
}

func TestGithubBridge_IngressRoutesOnlyWebhookPath(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/github-bridge/ingressroute.yaml", ghbOn,
		"traefik.enabled=true", "githubBridge.ingress.enabled=true", "githubBridge.ingress.host=github-webhook.example.com",
		"githubBridge.ingress.entryPoints={webhooks}")
	if !strings.Contains(out, "Host(`github-webhook.example.com`) && Path(`/webhook`)") {
		t.Errorf("IngressRoute must match exactly Host && Path(/webhook), got:\n%s", out)
	}
	for _, leaked := range []string{"PathPrefix", "/metrics", "/healthz"} {
		if strings.Contains(out, leaked) {
			t.Errorf("IngressRoute must not route %q publicly", leaked)
		}
	}
	for _, want := range []string{"- webhooks", "kind: Middleware", "rateLimit:"} {
		if !strings.Contains(out, want) {
			t.Errorf("IngressRoute missing %q", want)
		}
	}
}

func TestGithubBridge_IngressGuards(t *testing.T) {
	t.Parallel()
	if out, err := helmTemplateAllowFail(t, "templates/github-bridge/ingressroute.yaml", ghbOn,
		"traefik.enabled=true", "githubBridge.ingress.enabled=true"); err == nil || !strings.Contains(out, "githubBridge.ingress.host is empty") {
		t.Errorf("an ingress without a host must fail the render, err=%v", err)
	}
	if out, err := helmTemplateAllowFail(t, "templates/github-bridge/ingressroute.yaml", ghbOn,
		"traefik.enabled=true", "githubBridge.ingress.enabled=true", "githubBridge.ingress.host=h.example.com",
		"githubBridge.ingress.ipWhitelist.enabled=true"); err == nil || !strings.Contains(out, "sourceRange is empty") {
		t.Errorf("an enabled whitelist with no ranges must fail the render, err=%v", err)
	}
}

func TestGithubBridge_NetworkPolicies(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/networkpolicies/networkpolicies.yaml", ghbOn, "networkPolicies.enabled=true")
	for _, want := range []string{"test-github-bridge-ingress", "test-github-bridge-egress"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing NetworkPolicy %q (the namespace is default-deny)", want)
		}
	}
	// The egress policy reaches beads: the namespace is default-deny and both
	// pull_request and pipeline events write bead fields.
	found := false
	for doc := range strings.SplitSeq(out, "\n---") {
		if strings.Contains(doc, "test-github-bridge-egress") {
			found = true
			if !strings.Contains(doc, "component: beads") {
				t.Error("github-bridge egress must allow beads (HTTP)")
			}
		}
	}
	if !found {
		t.Error("github-bridge egress policy not found")
	}
}

// conflictScanner (gd-xrDytQgUfR): off by default, so the default chart is
// unchanged. With a repo it renders the env and an emptyDir mounted one level
// above CONFLICT_SCAN_DIR, because git refuses a repo whose top directory is
// the root-owned mount point.
func TestGithubBridge_ConflictScanner(t *testing.T) {
	t.Parallel()
	const tmpl = "templates/github-bridge/deployment.yaml"
	if out := helmTemplate(t, tmpl, ghbOn, ghbAppFlags); strings.Contains(out, "CONFLICT_SCAN") || strings.Contains(out, "conflict-mirror") {
		t.Errorf("default must not render the conflict scanner:\n%s", out)
	}
	out := helmTemplate(t, tmpl, ghbOn, ghbAppFlags, "githubBridge.conflictScanner.repo=o/r")
	for _, want := range []string{
		"name: CONFLICT_SCAN_REPO\n              value: \"o/r\"",
		"name: CONFLICT_SCAN_DIR\n              value: /var/lib/github-bridge/mirror",
		"- name: conflict-mirror\n              mountPath: /var/lib/github-bridge\n",
		"emptyDir:\n            sizeLimit: 2Gi",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("conflictScanner.repo set: missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "CONFLICT_SCAN_INTERVAL") {
		t.Error("an empty interval must leave CONFLICT_SCAN_INTERVAL to the binary default")
	}
	errOut, err := helmTemplateAllowFail(t, tmpl, ghbOn, "githubBridge.conflictScanner.repo=o/r")
	if err == nil || !strings.Contains(errOut, "conflictScanner.repo is set but githubBridge.githubApp.appId is empty") {
		t.Errorf("a repo without an appId must fail the render, err=%v out=%s", err, errOut)
	}
}
