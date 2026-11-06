package tests

import (
	"strings"
	"testing"
)

// gd-5QYwXtV7IA: the shared static beads token (kd-beads-token) is being
// retired from every holder but kbeads. These tests pin the per-holder steps
// that are already done in the chart so a later edit cannot quietly put the
// token back.

// With the machine principal on, trigger-engine, grafana-bridge, the controller
// and github-bridge authenticate every daemon call and the SSE stream as their
// service account, so the shared token is not injected at all. With it off (the
// rollback) the token returns.
func TestMachinePrincipalOn_DropsSharedStaticToken(t *testing.T) {
	t.Parallel()
	overlay := []string{k3sOverlayPath(t)}
	for _, tc := range []struct {
		name, template, enableKey string
		// set are the --set flags the overlay render needs for the
		// Deployment to exist at all.
		set []string
	}{
		{"trigger-engine", "templates/trigger-engine/deployment.yaml", "triggerEngine.machinePrincipal.enabled", nil},
		{"grafana-bridge", "templates/grafana-bridge/deployment.yaml", "grafanaBridge.machinePrincipal.enabled", nil},
		// gd-BDirD4F7Iq: the controller's default client (reads, SSE, typed
		// ownership, config seed) is the machine principal too.
		{"controller", "templates/controller/deployment.yaml", "agents.machinePrincipal.enabled", nil},
		// gd-tCMXlS9fXW: the github-bridge's whole client, not only its
		// release writes. The k3s overlay does not itself enable the bridge.
		{"github-bridge", "templates/github-bridge/deployment.yaml", "githubBridge.machinePrincipal.enabled", []string{"githubBridge.enabled=true"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			on := renderDeployment(t, tc.template, overlay, append([]string{tc.enableKey + "=true"}, tc.set...)...)
			if _, ok := on.env("", "BEADS_DAEMON_TOKEN"); ok {
				t.Errorf("%s still injects BEADS_DAEMON_TOKEN with the machine principal on", tc.template)
			}
			if _, ok := on.env("", "BEADS_OIDC_CLIENT_ID"); !ok {
				t.Errorf("%s must render BEADS_OIDC_CLIENT_ID with the machine principal on", tc.template)
			}
			off := renderDeployment(t, tc.template, overlay, append([]string{tc.enableKey + "=false"}, tc.set...)...)
			if _, ok := off.env("", "BEADS_DAEMON_TOKEN"); !ok {
				t.Errorf("%s must fall back to BEADS_DAEMON_TOKEN with the machine principal off (rollback)", tc.template)
			}
		})
	}
}

// gd-j9uB0cduHA (Lands boundary §6 step 4): the viewer has no rollback path
// to the shared token. Machine principal on or off, overlay or chart
// defaults, with beads.tokenSecret set, BEADS_DAEMON_TOKEN never renders, so
// there is no shared credential a viewer bug could lend to a tenant.
func TestBeadsViewer_NeverInjectsSharedStaticToken(t *testing.T) {
	t.Parallel()
	const tpl = "templates/beads-viewer/deployment.yaml"
	overlay := []string{k3sOverlayPath(t)}
	for _, tc := range []struct {
		name    string
		overlay []string
		set     []string
	}{
		{"k3s overlay", overlay, nil},
		// Rollback turns the Lands Gate off too: it files applications only as the viewer's machine principal.
		{"k3s overlay, principal off", overlay, []string{"beadsViewer.machinePrincipal.enabled=false", "beadsViewer.landsApply.enabled=false"}},
		{"chart defaults", nil, []string{"beadsViewer.enabled=true", "beads.tokenSecret=kd-beads-token"}},
		{"chart defaults, principal on", nil, []string{"beadsViewer.enabled=true", "beads.tokenSecret=kd-beads-token", "beadsViewer.machinePrincipal.enabled=true"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := helmRenderFiles(t, tpl, tc.overlay, tc.set...)
			if out == "" {
				t.Fatalf("%s renders nothing; the check proves nothing", tpl)
			}
			if strings.Contains(out, "BEADS_DAEMON_TOKEN") {
				t.Errorf("%s injects BEADS_DAEMON_TOKEN", tpl)
			}
			if strings.Contains(out, "kd-beads-token") {
				t.Errorf("%s references the kd-beads-token Secret", tpl)
			}
		})
	}
}

// NATS is its own credential domain: an infra client never sends the beads
// token or the agent-facing coopmux token to NATS. Since gd-XieCvZHhKN the
// k3s overlay ships nats.auth + subjectIsolation ON with operator-created
// creds Secrets (existingSecrets), so coopmux and trigger-engine read
// <fullname>-nats-infra-creds. The rollback (auth off) sends no token at all.
func TestNatsClientToken_NeverTheBeadsOrCoopmuxToken(t *testing.T) {
	t.Parallel()
	overlay := []string{k3sOverlayPath(t)}
	const infraCreds = "test-nats-infra-creds"

	natsTokenEnv := func(mux, te k8sDeployment) map[string]func() (k8sEnvVar, bool) {
		return map[string]func() (k8sEnvVar, bool){
			"coopmux":        func() (k8sEnvVar, bool) { return mux.env("", "COOP_MUX_NATS_TOKEN") },
			"trigger-engine": func() (k8sEnvVar, bool) { return te.env("", "NATS_TOKEN") },
		}
	}
	wantSecret := func(t *testing.T, mux, te k8sDeployment, want, when string) {
		t.Helper()
		for who, env := range natsTokenEnv(mux, te) {
			e, ok := env()
			if !ok {
				t.Errorf("%s must render its NATS token env %s", who, when)
				continue
			}
			ref, _ := e.ValueFrom["secretKeyRef"].(map[string]any)
			if ref["name"] != want {
				t.Errorf("%s NATS token comes from %v %s, want %s", who, ref["name"], when, want)
			}
		}
	}

	// The overlay as shipped: subjectIsolation on, operator-created Secrets.
	// Both infra clients read exactly the infra-creds Secret, the coopmux
	// --nats-token arg finds its env var defined, and neither the beads
	// token nor the agent-facing NATS creds is anywhere near an infra client.
	mux := renderDeployment(t, "templates/coopmux/deployment.yaml", overlay)
	te := renderDeployment(t, "templates/trigger-engine/deployment.yaml", overlay)
	wantSecret(t, mux, te, infraCreds, "with the overlay as shipped (subjectIsolation on)")
	muxOut := helmRenderFiles(t, "templates/coopmux/deployment.yaml", overlay)
	if !strings.Contains(muxOut, "$(COOP_MUX_NATS_TOKEN)") {
		t.Errorf("coopmux must pass --nats-token $(COOP_MUX_NATS_TOKEN) with subjectIsolation on")
	}
	teOut := helmRenderFiles(t, "templates/trigger-engine/deployment.yaml", overlay)
	for _, forbidden := range []string{"kd-beads-token", "test-nats-agent-creds"} {
		for who, out := range map[string]string{"coopmux": muxOut, "trigger-engine": teOut} {
			if strings.Contains(out, forbidden) {
				t.Errorf("%s references %s with the overlay as shipped", who, forbidden)
			}
		}
	}

	// The rollback: auth and subject isolation off -> no NATS token env/args
	// at all. Both keys, because gardenio.nats.clientTokenSecretName keys off
	// subjectIsolation alone: with auth off and isolation left on the clients
	// would still send the infra token to an unauthenticated server.
	off := []string{"nats.auth.enabled=false", "nats.auth.subjectIsolation.enabled=false"}
	mux = renderDeployment(t, "templates/coopmux/deployment.yaml", overlay, off...)
	for _, n := range []string{"COOP_MUX_NATS_TOKEN", "COOP_MUX_NATS_RELAY_TOKEN"} {
		if e, ok := mux.env("", n); ok {
			t.Errorf("coopmux renders %s (%v) with nats.auth disabled", n, e.ValueFrom)
		}
	}
	muxOut = helmRenderFiles(t, "templates/coopmux/deployment.yaml", overlay, off...)
	if strings.Contains(muxOut, "$(COOP_MUX_NATS_TOKEN)") {
		t.Errorf("coopmux args reference $(COOP_MUX_NATS_TOKEN) with nats.auth disabled but the env var is not defined (it would be passed literally)")
	}
	te = renderDeployment(t, "templates/trigger-engine/deployment.yaml", overlay, off...)
	if e, ok := te.env("", "NATS_TOKEN"); ok {
		t.Errorf("trigger-engine renders NATS_TOKEN (%v) with nats.auth disabled", e.ValueFrom)
	}

	// With a dedicated secret and no subject isolation, both read exactly
	// that Secret (subjectIsolation takes precedence when on, so pin it off).
	set := []string{"nats.auth.clientTokenSecret=nats-client-token", "nats.auth.enabled=true", "nats.auth.subjectIsolation.enabled=false"}
	mux = renderDeployment(t, "templates/coopmux/deployment.yaml", overlay, set...)
	te = renderDeployment(t, "templates/trigger-engine/deployment.yaml", overlay, set...)
	wantSecret(t, mux, te, "nats-client-token", "with nats.auth.clientTokenSecret set and subjectIsolation off")
}

// The release-deployer reports as its own machine principal when enabled: no
// KD_TOKEN / X-Operated-By env, the secret is an optional read-only file mount.
func TestReleaseDeployer_MachinePrincipal(t *testing.T) {
	t.Parallel()
	overlay := []string{k3sOverlayPath(t)}
	const tpl = "templates/release-deployer/cronjob.yaml"

	on := helmRenderFiles(t, tpl, overlay, "releaseDeployer.machinePrincipal.enabled=true")
	for _, want := range []string{
		"name: KD_OIDC_TOKEN_URL",
		"name: KD_OIDC_CLIENT_ID",
		"value: \"gardenio-release-deployer\"",
		"name: KD_OIDC_CLIENT_SECRET_FILE",
		"value: /var/run/secrets/gardenio-machine/clientSecret",
		"secretName: \"gardenio-machine-gardenio-release-deployer\"",
		"optional: true",
	} {
		if !strings.Contains(on, want) {
			t.Errorf("release-deployer (principal on) missing %q:\n%s", want, on)
		}
	}
	for _, bad := range []string{"name: KD_TOKEN", "kd-beads-token", "name: KD_OPERATED_BY"} {
		if strings.Contains(on, bad) {
			t.Errorf("release-deployer (principal on) still renders %q", bad)
		}
	}
	if !strings.Contains(on, "http://") || !strings.Contains(on, "/protocol/openid-connect/token") || strings.Contains(on, "auth.clonk.tv") {
		t.Errorf("release-deployer token URL must be the in-cluster Keycloak token endpoint:\n%s", on)
	}

	off := helmRenderFiles(t, tpl, overlay, "releaseDeployer.machinePrincipal.enabled=false")
	if !strings.Contains(off, "name: KD_TOKEN") {
		t.Errorf("release-deployer (principal off) must keep the legacy KD_TOKEN path")
	}
	for _, bad := range []string{"KD_OIDC_TOKEN_URL", "gardenio-machine-gardenio-release-deployer"} {
		if strings.Contains(off, bad) {
			t.Errorf("release-deployer (principal off) renders %q", bad)
		}
	}
}
