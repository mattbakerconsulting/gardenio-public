package tests

import (
	"strings"
	"testing"
)

// Real-user Slack E2E session ExternalSecret (epic ka-0000006991, task
// ka-0000002727). templates/external-secrets.yaml syncs the captured Playwright
// storageState for the dedicated TEST Slack human user from AWS Secrets Manager
// into a k8s Secret (default `slack-e2e-session`, key `storage-state.json`).
//
// The block is OFF by default (empty remoteRef) so it stays dormant on every
// stack that does not run the Slack UI tests; it lights up only when the E2E
// harness opts in by setting externalSecrets.slackE2ESession.remoteRef.

const slackE2EExtSecret = "templates/external-secrets.yaml"

func TestSlackE2ESession_ExternalSecret_DefaultDisabled(t *testing.T) {
	t.Parallel()
	// externalSecrets.enabled alone (no slackE2ESession.remoteRef) must NOT
	// render the slack-e2e session ExternalSecret.
	out, _ := helmTemplateAllowFail(t, slackE2EExtSecret,
		"externalSecrets.enabled=true",
		"externalSecrets.secretStoreName=aws-secretsmanager",
	)
	if strings.Contains(out, "app.kubernetes.io/component: slack-e2e") {
		t.Errorf("slack-e2e session ExternalSecret must NOT render without a remoteRef; got:\n%s", out)
	}
	if strings.Contains(out, "name: slack-e2e-session") {
		t.Errorf("slack-e2e session Secret target must NOT render without a remoteRef; got:\n%s", out)
	}
}

func TestSlackE2ESession_ExternalSecret_RendersWithRemoteRef(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, slackE2EExtSecret,
		"externalSecrets.enabled=true",
		"externalSecrets.secretStoreName=aws-secretsmanager",
		"externalSecrets.slackE2ESession.remoteRef=gardenio/prod/slack-e2e/test-user-session",
	)
	if !strings.Contains(out, "app.kubernetes.io/component: slack-e2e") {
		t.Fatalf("slack-e2e session ExternalSecret must render when remoteRef is set; got:\n%s", out)
	}
	for _, want := range []string{
		"kind: ExternalSecret",
		"name: slack-e2e-session",
		`key: "gardenio/prod/slack-e2e/test-user-session"`,
		"secretKey: storage_state",
		`property: "storage-state"`,
		// The k8s Secret key the Playwright module mounts as its storageState.
		"storage-state.json:",
		// Rotation class: oauth ExternalSecrets carry a 1h refreshInterval so a
		// re-captured session propagates without a redeploy (Part B refresh).
		"name: aws-secretsmanager",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered slack-e2e ExternalSecret missing %q; got:\n%s", want, out)
		}
	}
}

func TestSlackE2ESession_ExternalSecret_RespectsOverrides(t *testing.T) {
	t.Parallel()
	// A custom secretName + sessionProperty must both flow through.
	out := helmTemplate(t, slackE2EExtSecret,
		"externalSecrets.enabled=true",
		"externalSecrets.secretStoreName=aws-secretsmanager",
		"externalSecrets.slackE2ESession.remoteRef=gardenio/prod/slack-e2e/test-user-session",
		"externalSecrets.slackE2ESession.secretName=custom-slack-session",
		"externalSecrets.slackE2ESession.sessionProperty=state-blob",
	)
	if !strings.Contains(out, "name: custom-slack-session") {
		t.Errorf("custom secretName should render; got:\n%s", out)
	}
	if !strings.Contains(out, `property: "state-blob"`) {
		t.Errorf("custom sessionProperty should render; got:\n%s", out)
	}
}

// Disabling externalSecrets entirely must suppress the block even with a
// remoteRef set (the whole family is gated on externalSecrets.enabled).
func TestSlackE2ESession_ExternalSecret_GatedOnExternalSecretsEnabled(t *testing.T) {
	t.Parallel()
	out, _ := helmTemplateAllowFail(t, slackE2EExtSecret,
		"externalSecrets.enabled=false",
		"externalSecrets.slackE2ESession.remoteRef=gardenio/prod/slack-e2e/test-user-session",
	)
	if strings.Contains(out, "app.kubernetes.io/component: slack-e2e") {
		t.Errorf("slack-e2e session ExternalSecret must NOT render when externalSecrets.enabled=false; got:\n%s", out)
	}
}
