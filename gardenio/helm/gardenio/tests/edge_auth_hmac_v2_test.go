package tests

import (
	"strings"
	"testing"
)

// gd-h2tQrOwjGJ (gd-dMof1PW1oJ item 3): the v1 and v2 edge-auth envelope keys
// are split, so a holder of the v1 key alone (beads-viewer) cannot mint a v2
// admin envelope. edgeAuth.hmacKey.secretKeyV2 wires the v2 key into every v2
// signer (edge-auth, slack-bridge) and verifier (beads, coopmux) at once, and
// never into beads-viewer. Empty (the default) renders none of it, so each side
// keeps falling back to the shared key.

// v2Sets is the dryRun=false baseline with an operator-supplied Secret (the
// path that passes the edge-auth HMAC source gate) plus the v2 key.
var v2Sets = []string{
	"edgeAuth.enabled=true",
	"edgeAuth.dryRun=false",
	"edgeAuth.host=cli.test",
	"edgeAuth.hmacKey.secretName=gardenio-edge-auth-hmac",
	"edgeAuth.hmacKey.secretKeyV2=hmac-key-v2",
	"slackBridge.enabled=true",
	"coopmux.enabled=true",
	"beadsViewer.enabled=true",
}

func TestEdgeAuthHmacV2_DefaultRendersNoV2Env(t *testing.T) {
	t.Parallel()
	sets := []string{
		"edgeAuth.enabled=true",
		"edgeAuth.dryRun=false",
		"edgeAuth.host=cli.test",
		"edgeAuth.hmacKey.secretName=gardenio-edge-auth-hmac",
		"slackBridge.enabled=true",
		"coopmux.enabled=true",
	}
	for _, tmpl := range []string{
		"templates/edge-auth/deployment.yaml",
		"templates/beads/deployment.yaml",
		"templates/coopmux/deployment.yaml",
		"templates/slack-bridge/deployment.yaml",
	} {
		out := helmTemplate(t, tmpl, sets...)
		if strings.Contains(out, "HMAC_KEY_V2") {
			t.Errorf("%s: no *_HMAC_KEY_V2 env may render while edgeAuth.hmacKey.secretKeyV2 is empty, got:\n%s", tmpl, out)
		}
	}
}

func TestEdgeAuthHmacV2_WiresAllFourFromSameSecret(t *testing.T) {
	t.Parallel()
	cases := []struct {
		tmpl, env, optional string
	}{
		// edge-auth mirrors its v1 EDGE_AUTH_HMAC_KEY: always optional (the
		// proxy surface 503s without a key rather than the pod failing).
		{"templates/edge-auth/deployment.yaml", "EDGE_AUTH_HMAC_KEY_V2", "optional: true"},
		{"templates/slack-bridge/deployment.yaml", "SLACK_BRIDGE_EDGE_AUTH_HMAC_KEY_V2", "optional: false"},
		{"templates/beads/deployment.yaml", "DAEMON_HMAC_KEY_V2", "optional: false"},
		{"templates/coopmux/deployment.yaml", "DAEMON_HMAC_KEY_V2", "optional: false"},
	}
	for _, c := range cases {
		out := helmTemplate(t, c.tmpl, v2Sets...)
		block := extractEnvBlock(out, c.env)
		if block == "" {
			t.Errorf("%s: %s must render when edgeAuth.hmacKey.secretKeyV2 is set, got:\n%s", c.tmpl, c.env, out)
			continue
		}
		for _, want := range []string{"name: gardenio-edge-auth-hmac", "key: hmac-key-v2", c.optional} {
			if !strings.Contains(block, want) {
				t.Errorf("%s: %s secretKeyRef must contain %q, got:\n%s", c.tmpl, c.env, want, block)
			}
		}
	}
}

func TestEdgeAuthHmacV2_OptionalUnderDryRun(t *testing.T) {
	t.Parallel()
	sets := []string{
		"edgeAuth.enabled=true",
		"edgeAuth.dryRun=true",
		"edgeAuth.host=cli.test",
		"edgeAuth.hmacKey.secretKeyV2=hmac-key-v2",
		"slackBridge.enabled=true",
		"coopmux.enabled=true",
	}
	for tmpl, env := range map[string]string{
		"templates/slack-bridge/deployment.yaml": "SLACK_BRIDGE_EDGE_AUTH_HMAC_KEY_V2",
		"templates/beads/deployment.yaml":        "DAEMON_HMAC_KEY_V2",
		"templates/coopmux/deployment.yaml":      "DAEMON_HMAC_KEY_V2",
	} {
		block := extractEnvBlock(helmTemplate(t, tmpl, sets...), env)
		if !strings.Contains(block, "optional: true") {
			t.Errorf("%s: %s must be optional:true under edgeAuth.dryRun=true, got:\n%s", tmpl, env, block)
		}
	}
}

// The v2 key is the whole point of the split: beads-viewer holds the v1 key
// only, so it must never be handed the v2 one.
func TestEdgeAuthHmacV2_NeverWiredIntoBeadsViewer(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads-viewer/deployment.yaml",
		append([]string{"beadsViewer.edgeAuthSigning=true"}, v2Sets...)...)
	if !strings.Contains(out, "BEADS_VIEWER_EDGE_AUTH_HMAC_KEY") {
		t.Fatalf("precondition: beads-viewer should still get the v1 key, got:\n%s", out)
	}
	if strings.Contains(out, "hmac-key-v2") || strings.Contains(out, "HMAC_KEY_V2") {
		t.Errorf("beads-viewer must never be given the v2 envelope key, got:\n%s", out)
	}
}

func TestEdgeAuthHmacV2_InboxSeedsV2Key(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/forwardauth/secrets.yaml",
		"externalSecrets.enabled=false",
		"edgeAuth.enabled=true",
		"edgeAuth.dryRun=true",
		"edgeAuth.hmacKey.secretKeyV2=hmac-key-v2",
	)
	if !strings.Contains(out, "hmac-key:") || !strings.Contains(out, "hmac-key-v2:") {
		t.Errorf("in-box edge-auth HMAC Secret must seed both hmac-key and hmac-key-v2, got:\n%s", out)
	}
}

func TestEdgeAuthHmacV2_InboxDefaultSeedsNoV2Key(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/forwardauth/secrets.yaml",
		"externalSecrets.enabled=false",
		"edgeAuth.enabled=true",
		"edgeAuth.dryRun=true",
	)
	if strings.Contains(out, "hmac-key-v2") {
		t.Errorf("no v2 key may be seeded while edgeAuth.hmacKey.secretKeyV2 is empty, got:\n%s", out)
	}
}

func TestEdgeAuthHmacV2_ExternalSecretMapsV2RemoteRef(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/edge-auth/externalsecret.yaml",
		"edgeAuth.enabled=true",
		"edgeAuth.dryRun=false",
		"edgeAuth.host=cli.test",
		"externalSecrets.enabled=true",
		"externalSecrets.edgeAuthHMACKey.remoteRef=gardenio/edge-auth-hmac",
		"externalSecrets.edgeAuthHMACKeyV2.remoteRef=gardenio/edge-auth-hmac-v2",
		"edgeAuth.hmacKey.secretKeyV2=hmac-key-v2",
	)
	for _, want := range []string{
		"hmac-key-v2: '{{ .hmac_key_v2 }}'",
		"- secretKey: hmac_key_v2",
		`key: "gardenio/edge-auth-hmac-v2"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("ExternalSecret must contain %q, got:\n%s", want, out)
		}
	}
}

// On the ESO path the ExternalSecret owns the Secret, so a v2 key with no v2
// remoteRef would never land and the required mounts would stall the rollout.
func TestEdgeAuthHmacV2_ExternalSecretWithoutV2RemoteRefFails(t *testing.T) {
	t.Parallel()
	out := helmTemplateExpectError(t, "templates/edge-auth/deployment.yaml",
		"--set", "edgeAuth.enabled=true",
		"--set", "edgeAuth.dryRun=false",
		"--set", "edgeAuth.host=cli.test",
		"--set", "externalSecrets.enabled=true",
		"--set", "externalSecrets.edgeAuthHMACKey.remoteRef=gardenio/edge-auth-hmac",
		"--set", "edgeAuth.hmacKey.secretKeyV2=hmac-key-v2",
	)
	if !strings.Contains(out, "externalSecrets.edgeAuthHMACKeyV2.remoteRef is empty") {
		t.Errorf("expected the v2 remoteRef fail-fast, got:\n%s", out)
	}
}
