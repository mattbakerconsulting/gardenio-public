package tests

import (
	"strings"
	"testing"
)

// ka-0000000726: edgeAuth.dryRun=true must truly gate the image+secret
// dependencies so the chart can land before the operator provisions
// gardenio-<env>-edge-auth-hmac and before the edge-auth image is published.
//
//   - edge-auth Deployment: skipped under dryRun (no image required).
//   - DAEMON_HMAC_KEY / SLACK_BRIDGE_EDGE_AUTH_HMAC_KEY env source on
//     beads, coopmux, slack-bridge: marked optional:true under dryRun so
//     pods do not hit CreateContainerConfigError on the absent secret.
//
// With dryRun=false (default) the existing behaviour is preserved.

func TestEdgeAuth_DryRunTrue_SkipsDeployment(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "templates/edge-auth/deployment.yaml",
		"edgeAuth.enabled=true",
		"edgeAuth.dryRun=true",
		"edgeAuth.host=cli.test",
	)
	if err == nil {
		if strings.Contains(out, "kind: Deployment") {
			t.Fatalf("edge-auth Deployment must NOT render when edgeAuth.dryRun=true, got:\n%s", out)
		}
		return
	}
	if !strings.Contains(out, "could not find template") {
		t.Fatalf("unexpected helm failure: %v\n%s", err, out)
	}
}

func TestEdgeAuth_DryRunFalse_RendersDeployment(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/edge-auth/deployment.yaml",
		"edgeAuth.enabled=true",
		"edgeAuth.dryRun=false",
		"edgeAuth.host=cli.test",
		// dryRun=false requires an HMAC key source or the edge-auth deployment
		// template fail-fasts (edge-auth/deployment.yaml:32, ka-0000001247).
		// Wire the dev/e2e path so the gate passes (gd-Wu2JEv8b4H).
		"edgeAuth.hmacKey.secretName=gardenio-edge-auth-hmac",
	)
	if !strings.Contains(out, "kind: Deployment") {
		t.Fatalf("edge-auth Deployment must render with edgeAuth.dryRun=false, got:\n%s", out)
	}
	if !strings.Contains(out, "name: test-edge-auth") {
		t.Errorf("expected edge-auth Deployment name in render, got:\n%s", out)
	}
}

func TestBeads_DaemonHmacKey_OptionalUnderDryRun(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads/deployment.yaml",
		"edgeAuth.enabled=true",
		"edgeAuth.dryRun=true",
		"edgeAuth.host=cli.test",
	)
	if !containsDaemonHmacKeyOptional(out, true) {
		t.Errorf("beads DAEMON_HMAC_KEY secretKeyRef must be optional:true under edgeAuth.dryRun=true, got:\n%s",
			extractDaemonHmacKeyBlock(out))
	}
}

func TestBeads_DaemonHmacKey_RequiredWhenDryRunOff(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads/deployment.yaml",
		"edgeAuth.enabled=true",
		"edgeAuth.dryRun=false",
		"edgeAuth.host=cli.test",
		// dryRun=false requires an HMAC key source or the edge-auth deployment
		// template fail-fasts (edge-auth/deployment.yaml:32, ka-0000001247).
		// Wire the dev/e2e path so the gate passes (gd-Wu2JEv8b4H).
		"edgeAuth.hmacKey.secretName=gardenio-edge-auth-hmac",
	)
	if !containsDaemonHmacKeyOptional(out, false) {
		t.Errorf("beads DAEMON_HMAC_KEY secretKeyRef must be optional:false under edgeAuth.dryRun=false, got:\n%s",
			extractDaemonHmacKeyBlock(out))
	}
}

func TestCoopmux_DaemonHmacKey_OptionalUnderDryRun(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/coopmux/deployment.yaml",
		"coopmux.enabled=true",
		"edgeAuth.enabled=true",
		"edgeAuth.dryRun=true",
		"edgeAuth.host=cli.test",
	)
	if !containsDaemonHmacKeyOptional(out, true) {
		t.Errorf("coopmux DAEMON_HMAC_KEY secretKeyRef must be optional:true under edgeAuth.dryRun=true, got:\n%s",
			extractDaemonHmacKeyBlock(out))
	}
}

func TestCoopmux_DaemonHmacKey_RequiredWhenDryRunOff(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/coopmux/deployment.yaml",
		"coopmux.enabled=true",
		"edgeAuth.enabled=true",
		"edgeAuth.dryRun=false",
		"edgeAuth.host=cli.test",
		// dryRun=false requires an HMAC key source or the edge-auth deployment
		// template fail-fasts (edge-auth/deployment.yaml:32, ka-0000001247).
		// Wire the dev/e2e path so the gate passes (gd-Wu2JEv8b4H).
		"edgeAuth.hmacKey.secretName=gardenio-edge-auth-hmac",
	)
	if !containsDaemonHmacKeyOptional(out, false) {
		t.Errorf("coopmux DAEMON_HMAC_KEY secretKeyRef must be optional:false under edgeAuth.dryRun=false, got:\n%s",
			extractDaemonHmacKeyBlock(out))
	}
}

func TestSlackBridge_EdgeAuthHmacKey_OptionalUnderDryRun(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/slack-bridge/deployment.yaml",
		"slackBridge.enabled=true",
		"edgeAuth.enabled=true",
		"edgeAuth.dryRun=true",
		"edgeAuth.host=cli.test",
	)
	block := extractEnvBlock(out, "SLACK_BRIDGE_EDGE_AUTH_HMAC_KEY")
	if !strings.Contains(block, "optional: true") {
		t.Errorf("slack-bridge SLACK_BRIDGE_EDGE_AUTH_HMAC_KEY secretKeyRef must be optional:true under edgeAuth.dryRun=true, got:\n%s", block)
	}
}

func TestSlackBridge_EdgeAuthHmacKey_RequiredWhenDryRunOff(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/slack-bridge/deployment.yaml",
		"slackBridge.enabled=true",
		"edgeAuth.enabled=true",
		"edgeAuth.dryRun=false",
		"edgeAuth.host=cli.test",
		// dryRun=false requires an HMAC key source or the edge-auth deployment
		// template fail-fasts (edge-auth/deployment.yaml:32, ka-0000001247).
		// Wire the dev/e2e path so the gate passes (gd-Wu2JEv8b4H).
		"edgeAuth.hmacKey.secretName=gardenio-edge-auth-hmac",
	)
	block := extractEnvBlock(out, "SLACK_BRIDGE_EDGE_AUTH_HMAC_KEY")
	if !strings.Contains(block, "optional: false") {
		t.Errorf("slack-bridge SLACK_BRIDGE_EDGE_AUTH_HMAC_KEY secretKeyRef must be optional:false under edgeAuth.dryRun=false, got:\n%s", block)
	}
}

// containsDaemonHmacKeyOptional returns true when the DAEMON_HMAC_KEY env
// block in `out` declares optional: <want>.
func containsDaemonHmacKeyOptional(out string, want bool) bool {
	block := extractEnvBlock(out, "DAEMON_HMAC_KEY")
	if want {
		return strings.Contains(block, "optional: true")
	}
	return strings.Contains(block, "optional: false")
}

// extractEnvBlock pulls out the ~8 lines following a `- name: <envName>` env
// declaration. Good enough for assertions without a YAML parser.
func extractEnvBlock(out, envName string) string {
	needle := "- name: " + envName
	idx := strings.Index(out, needle)
	if idx < 0 {
		return ""
	}
	rest := out[idx:]
	lines := strings.SplitN(rest, "\n", 9)
	return strings.Join(lines, "\n")
}

func extractDaemonHmacKeyBlock(out string) string {
	return extractEnvBlock(out, "DAEMON_HMAC_KEY")
}
