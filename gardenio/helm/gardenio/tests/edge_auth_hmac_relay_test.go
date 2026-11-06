package tests

import (
	"strings"
	"testing"
)

// gd-9ZGh01W5ub (relay-key): beads-viewer's Glass relay signs its coopmux /ws
// envelope with a key of its own, edgeAuth.hmacKey.secretKeyRelay. Only
// coopmux verifies it, so the viewer still holds no key the beads daemon
// trusts and the v1/v2 split (TestEdgeAuthHmacV2_NeverWiredIntoBeadsViewer)
// stands. Empty (the default) renders none of it. It is staged with edge-auth
// off too (ruling gd-P2Ybp03DPh, relay-only).

// relaySets is v2Sets plus the relay key: every key split in force at once.
var relaySets = append([]string{"edgeAuth.hmacKey.secretKeyRelay=hmac-key-relay"}, v2Sets...)

func TestEdgeAuthHmacRelay_DefaultRendersNoRelayEnv(t *testing.T) {
	t.Parallel()
	for _, tmpl := range []string{
		"templates/beads-viewer/deployment.yaml",
		"templates/coopmux/deployment.yaml",
		"templates/beads/deployment.yaml",
	} {
		if out := helmTemplate(t, tmpl, v2Sets...); strings.Contains(out, "RELAY") {
			t.Errorf("%s: no relay env may render while edgeAuth.hmacKey.secretKeyRelay is empty, got:\n%s", tmpl, out)
		}
	}
}

func TestEdgeAuthHmacRelay_WiresViewerAndCoopmuxFromSameSecret(t *testing.T) {
	t.Parallel()
	for tmpl, env := range map[string]string{
		"templates/beads-viewer/deployment.yaml": "COOPMUX_RELAY_HMAC_KEY",
		"templates/coopmux/deployment.yaml":      "DAEMON_HMAC_KEY_RELAY",
	} {
		out := helmTemplate(t, tmpl, relaySets...)
		block := extractEnvBlock(out, env)
		if block == "" {
			t.Errorf("%s: %s must render when edgeAuth.hmacKey.secretKeyRelay is set, got:\n%s", tmpl, env, out)
			continue
		}
		for _, want := range []string{"name: gardenio-edge-auth-hmac", "key: hmac-key-relay", "optional: false"} {
			if !strings.Contains(block, want) {
				t.Errorf("%s: %s secretKeyRef must contain %q, got:\n%s", tmpl, env, want, block)
			}
		}
	}
}

func TestEdgeAuthHmacRelay_OptionalUnderDryRun(t *testing.T) {
	t.Parallel()
	sets := []string{
		"edgeAuth.enabled=true",
		"edgeAuth.dryRun=true",
		"edgeAuth.host=cli.test",
		"edgeAuth.hmacKey.secretKeyRelay=hmac-key-relay",
		"coopmux.enabled=true",
		"beadsViewer.enabled=true",
	}
	for tmpl, env := range map[string]string{
		"templates/beads-viewer/deployment.yaml": "COOPMUX_RELAY_HMAC_KEY",
		"templates/coopmux/deployment.yaml":      "DAEMON_HMAC_KEY_RELAY",
	} {
		block := extractEnvBlock(helmTemplate(t, tmpl, sets...), env)
		if !strings.Contains(block, "optional: true") {
			t.Errorf("%s: %s must be optional:true under edgeAuth.dryRun=true, got:\n%s", tmpl, env, block)
		}
	}
}

// The viewer's copy rides coopmux.enabled: with no coopmux there is nothing
// to relay from.
func TestEdgeAuthHmacRelay_AbsentWithCoopmuxOff(t *testing.T) {
	t.Parallel()
	noMux := helmTemplate(t, "templates/beads-viewer/deployment.yaml",
		append(append([]string{}, relaySets...), "coopmux.enabled=false")...)
	if strings.Contains(noMux, "COOPMUX_RELAY_HMAC_KEY") {
		t.Errorf("beads-viewer: relay key must not render with coopmux off, got:\n%s", noMux)
	}
}

// relayOnlySets stages the relay key with edge-auth off (ruling
// gd-P2Ybp03DPh, relay-only): the k3s posture, in-box seeding, no shim.
var relayOnlySets = []string{
	"edgeAuth.enabled=false",
	"edgeAuth.hmacKey.secretKeyRelay=hmac-key-relay",
	"externalSecrets.enabled=false",
	"coopmux.enabled=true",
	"beadsViewer.enabled=true",
}

func TestEdgeAuthHmacRelay_RelayOnlyWiresViewerAndCoopmux(t *testing.T) {
	t.Parallel()
	for tmpl, env := range map[string]string{
		"templates/beads-viewer/deployment.yaml": "COOPMUX_RELAY_HMAC_KEY",
		"templates/coopmux/deployment.yaml":      "DAEMON_HMAC_KEY_RELAY",
	} {
		block := extractEnvBlock(helmTemplate(t, tmpl, relayOnlySets...), env)
		if block == "" {
			t.Errorf("%s: %s must render with edge-auth off and the relay key set", tmpl, env)
			continue
		}
		// No dryRun with edge-auth off: the Secret seeds in the same release.
		for _, want := range []string{"name: test-edge-auth-hmac", "key: hmac-key-relay", "optional: false"} {
			if !strings.Contains(block, want) {
				t.Errorf("%s: %s secretKeyRef must contain %q, got:\n%s", tmpl, env, want, block)
			}
		}
	}
}

// With edge-auth off, coopmux holds the relay key alone and the beads daemon
// holds nothing: no shared or v2 key is mounted anywhere.
func TestEdgeAuthHmacRelay_RelayOnlyMountsNoOtherKey(t *testing.T) {
	t.Parallel()
	sets := append([]string{"edgeAuth.hmacKey.secretKeyV2=hmac-key-v2"}, relayOnlySets...)
	if out := helmTemplate(t, "templates/coopmux/deployment.yaml", sets...); strings.Contains(out, "name: DAEMON_HMAC_KEY\n") ||
		strings.Contains(out, "DAEMON_HMAC_KEY_V2") {
		t.Errorf("coopmux: only DAEMON_HMAC_KEY_RELAY may render with edge-auth off, got:\n%s", out)
	}
	if out := helmTemplate(t, "templates/beads/deployment.yaml", sets...); strings.Contains(out, "HMAC_KEY") || strings.Contains(out, "hmac-key") {
		t.Errorf("beads: no edge-auth key may render with edge-auth off, got:\n%s", out)
	}
}

func TestEdgeAuthHmacRelay_RelayOnlySeedsOnlyTheRelayKey(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/forwardauth/secrets.yaml",
		append([]string{"edgeAuth.hmacKey.secretKeyV2=hmac-key-v2"}, relayOnlySets...)...)
	if !strings.Contains(out, `name: "test-edge-auth-hmac"`) || !strings.Contains(out, "hmac-key-relay:") ||
		!strings.Contains(out, `"helm.sh/resource-policy": keep`) {
		t.Errorf("relay-only must self-seed the relay key in the kept edge-auth HMAC Secret, got:\n%s", out)
	}
	if strings.Contains(out, "hmac-key:") || strings.Contains(out, "hmac-key-v2:") {
		t.Errorf("relay-only must not seed hmac-key or hmac-key-v2 with edge-auth off, got:\n%s", out)
	}
	if out := helmTemplate(t, "templates/forwardauth/secrets.yaml",
		append([]string{"edgeAuth.hmacKey.secretName=mine"}, relayOnlySets...)...); strings.Contains(out, "edge-auth-hmac") {
		t.Errorf("an operator-supplied secretName must not be seeded by the chart, got:\n%s", out)
	}
}

// Relay-only stages no shim: no edge-auth Deployment, Service or IngressRoute.
func TestEdgeAuthHmacRelay_RelayOnlyRendersNoShim(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "", relayOnlySets...)
	if strings.Contains(out, "# Source: gardenio/templates/edge-auth/") {
		t.Errorf("relay-only must render no edge-auth template; found one in the full render")
	}
}

func TestEdgeAuthHmacRelay_RelayOnlyRefusesADaemonTrustedKey(t *testing.T) {
	t.Parallel()
	var args []string
	for _, s := range append(append([]string{}, relayOnlySets...), "edgeAuth.hmacKey.secretKeyRelay=hmac-key") {
		args = append(args, "--set", s)
	}
	out := helmTemplateExpectError(t, "templates/beads-viewer/deployment.yaml", args...)
	if !strings.Contains(out, "secretKeyRelay must name its own key") {
		t.Errorf("relay-only secretKeyRelay=hmac-key: expected the own-key fail-fast, got:\n%s", out)
	}
}

// ExternalSecrets maps the relay key only with edge-auth on, so relay-only on
// that path needs an operator-supplied Secret or the pods would never start.
func TestEdgeAuthHmacRelay_RelayOnlyOnExternalSecretsNeedsSecretName(t *testing.T) {
	t.Parallel()
	var args []string
	for _, s := range append(append([]string{}, relayOnlySets...), "externalSecrets.enabled=true") {
		args = append(args, "--set", s)
	}
	out := helmTemplateExpectError(t, "templates/coopmux/deployment.yaml", args...)
	if !strings.Contains(out, "is seeded in-box only") {
		t.Errorf("expected the relay-only ExternalSecrets fail-fast, got:\n%s", out)
	}
	block := extractEnvBlock(helmTemplate(t, "templates/coopmux/deployment.yaml",
		append(append([]string{}, relayOnlySets...), "externalSecrets.enabled=true", "edgeAuth.hmacKey.secretName=mine")...),
		"DAEMON_HMAC_KEY_RELAY")
	if !strings.Contains(block, "name: mine") {
		t.Errorf("relay-only with secretName must read the relay key from it, got:\n%s", block)
	}
}

// The relay key is coopmux's alone: the beads daemon never verifies it, and
// the viewer still gets no v2 key with the relay key set.
func TestEdgeAuthHmacRelay_NeverWiredIntoBeads(t *testing.T) {
	t.Parallel()
	if out := helmTemplate(t, "templates/beads/deployment.yaml", relaySets...); strings.Contains(out, "RELAY") || strings.Contains(out, "hmac-key-relay") {
		t.Errorf("beads must never be given the relay key, got:\n%s", out)
	}
	out := helmTemplate(t, "templates/beads-viewer/deployment.yaml",
		append([]string{"beadsViewer.edgeAuthSigning=true"}, relaySets...)...)
	if strings.Contains(out, "hmac-key-v2") || strings.Contains(out, "HMAC_KEY_V2") {
		t.Errorf("beads-viewer must never be given the v2 envelope key, got:\n%s", out)
	}
}

// Naming the shared or the v2 key as the relay key would hand beads-viewer a
// key the daemon trusts, the hole the ruling keeps closed; the chart refuses.
func TestEdgeAuthHmacRelay_RefusesADaemonTrustedKey(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"hmac-key", "hmac-key-v2"} {
		args := []string{"--set", "edgeAuth.hmacKey.secretKeyRelay=" + key}
		for _, s := range v2Sets {
			args = append(args, "--set", s)
		}
		out := helmTemplateExpectError(t, "templates/beads-viewer/deployment.yaml", args...)
		if !strings.Contains(out, "secretKeyRelay must name its own key") {
			t.Errorf("secretKeyRelay=%s: expected the own-key fail-fast, got:\n%s", key, out)
		}
	}
}

func TestEdgeAuthHmacRelay_InboxSeedsRelayKey(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/forwardauth/secrets.yaml",
		"externalSecrets.enabled=false",
		"edgeAuth.enabled=true",
		"edgeAuth.dryRun=true",
		"edgeAuth.hmacKey.secretKeyRelay=hmac-key-relay",
	)
	if !strings.Contains(out, "hmac-key:") || !strings.Contains(out, "hmac-key-relay:") {
		t.Errorf("in-box edge-auth HMAC Secret must seed both hmac-key and hmac-key-relay, got:\n%s", out)
	}
}

func TestEdgeAuthHmacRelay_ExternalSecretMapsRelayRemoteRef(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/edge-auth/externalsecret.yaml",
		"edgeAuth.enabled=true",
		"edgeAuth.dryRun=false",
		"edgeAuth.host=cli.test",
		"externalSecrets.enabled=true",
		"externalSecrets.edgeAuthHMACKey.remoteRef=gardenio/edge-auth-hmac",
		"externalSecrets.edgeAuthHMACKeyRelay.remoteRef=gardenio/edge-auth-hmac-relay",
		"edgeAuth.hmacKey.secretKeyRelay=hmac-key-relay",
	)
	for _, want := range []string{
		"hmac-key-relay: '{{ .hmac_key_relay }}'",
		"- secretKey: hmac_key_relay",
		`key: "gardenio/edge-auth-hmac-relay"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("ExternalSecret must contain %q, got:\n%s", want, out)
		}
	}
}

func TestEdgeAuthHmacRelay_ExternalSecretWithoutRelayRemoteRefFails(t *testing.T) {
	t.Parallel()
	out := helmTemplateExpectError(t, "templates/edge-auth/deployment.yaml",
		"--set", "edgeAuth.enabled=true",
		"--set", "edgeAuth.dryRun=false",
		"--set", "edgeAuth.host=cli.test",
		"--set", "externalSecrets.enabled=true",
		"--set", "externalSecrets.edgeAuthHMACKey.remoteRef=gardenio/edge-auth-hmac",
		"--set", "edgeAuth.hmacKey.secretKeyRelay=hmac-key-relay",
	)
	if !strings.Contains(out, "externalSecrets.edgeAuthHMACKeyRelay.remoteRef is empty") {
		t.Errorf("expected the relay remoteRef fail-fast, got:\n%s", out)
	}
}
