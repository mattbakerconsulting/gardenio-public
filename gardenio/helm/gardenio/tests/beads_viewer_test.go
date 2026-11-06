package tests

import (
	"strings"
	"testing"
)

// --- startupProbe: cold-start grace so liveness does not SIGTERM mid-startup ---
//
// ka-0000004213: beads-viewer's main() does blocking startup work (OIDC provider
// discovery with ~31s of backoff when Keycloak is not yet serving, config-watcher
// first load, S3 client build, NATS connect) BEFORE the /healthz HTTP server starts
// listening. With only a livenessProbe (initialDelay 5s, period 15s, default
// failureThreshold 3) and no startupProbe, the kubelet kills the container ~35-50s
// in if /healthz has not yet answered — exit, lost startup log, CrashLoopBackOff,
// and helm --wait --timeout 15m trips "context deadline exceeded", failing
// the deploy. The startupProbe suspends liveness until /healthz first answers.
func TestBeadsViewer_StartupProbe_Present(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads-viewer/deployment.yaml",
		"beadsViewer.enabled=true",
		"beadsViewer.oidc.issuerURL=https://auth.example.com/realms/x",
		"beadsViewer.oidc.clientID=viewer",
	)
	if !strings.Contains(out, "startupProbe:") {
		t.Fatalf("beads-viewer must declare a startupProbe so a slow cold start does not get SIGTERM'd by the liveness probe (ka-0000004213), got:\n%s", out)
	}
	// The startupProbe must grant a generous grace window — failureThreshold*period
	// well past the worst-case blocking startup (~31s OIDC backoff + dependency
	// cold start under CPU throttling). 60 * 5s = 300s mirrors the litellm idiom.
	if !strings.Contains(out, "failureThreshold: 60") {
		t.Errorf("beads-viewer startupProbe should allow a generous startup grace (failureThreshold: 60), got:\n%s", out)
	}
}

// --- beadsViewer.devAuthBypass: Ziti-only OIDC bypass env plumbing (ka-0000002068) ---
//
// When devAuthBypass is true the deployment must render
// BEADS_VIEWER_DEV_AUTH_BYPASS=1 (which disables OIDC in the binary) AND must
// suppress the OIDC_* env block so the rendered Deployment carries no
// misleading issuer. Critically it must NOT emit
// BEADS_VIEWER_DEV_AUTH_BYPASS_ACK_AGENT_POD — that flag would force
// LISTEN_ADDR to loopback, making the viewer unreachable over the Ziti
// overlay. The bypass is reachable on the pod network precisely because the
// pod sets automountServiceAccountToken:false, so the dev_auth.go SA-token
// guard never trips.

func TestBeadsViewer_DevAuthBypass_RendersEnvAndSuppressesOIDC(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads-viewer/deployment.yaml",
		"beadsViewer.enabled=true",
		"beadsViewer.devAuthBypass=true",
		"beadsViewer.oidc.enabled=true",
		"beadsViewer.oidc.issuerURL=https://auth.example.com/realms/x",
		"beadsViewer.oidc.clientID=viewer",
	)
	if !strings.Contains(out, "BEADS_VIEWER_DEV_AUTH_BYPASS") {
		t.Fatalf("BEADS_VIEWER_DEV_AUTH_BYPASS env must be present when beadsViewer.devAuthBypass=true, got:\n%s", out)
	}
	if strings.Contains(out, "OIDC_ISSUER_URL") || strings.Contains(out, "OIDC_CLIENT_ID") {
		t.Errorf("OIDC_* env must be suppressed when devAuthBypass is on (no misleading issuer), got:\n%s", out)
	}
	// Must NOT force loopback — the ACK env would do that and strand the viewer over Ziti.
	if strings.Contains(out, "BEADS_VIEWER_DEV_AUTH_BYPASS_ACK_AGENT_POD") {
		t.Errorf("the chart toggle must NOT set BEADS_VIEWER_DEV_AUTH_BYPASS_ACK_AGENT_POD (would force LISTEN_ADDR to loopback), got:\n%s", out)
	}
	// LISTEN_ADDR stays bound to the service port (all interfaces), reachable over Ziti.
	if !strings.Contains(out, `value: ":8095"`) {
		t.Errorf("LISTEN_ADDR should remain :8095 (bind-all) under devAuthBypass, got:\n%s", out)
	}
}

// gd-kRyMsWiDNZ: with the bypass on, the edge shared secret reaches the viewer
// as a required secretKeyRef (no optional: a missing Secret must fail the pod,
// not silently fall back to trusting the raw header), and is absent by default.
func TestBeadsViewer_DevAuthProxySecret_RendersSecretKeyRef(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads-viewer/deployment.yaml",
		"beadsViewer.enabled=true",
		"beadsViewer.devAuthBypass=true",
		"beadsViewer.devAuthProxySecret.secretName=viewer-authproxy-secret",
	)
	for _, want := range []string{"BEADS_VIEWER_DEV_AUTH_PROXY_SECRET", `name: "viewer-authproxy-secret"`, `key: "secret"`} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered Deployment missing %q, got:\n%s", want, out)
		}
	}
	i := strings.Index(out, "BEADS_VIEWER_DEV_AUTH_PROXY_SECRET")
	if i >= 0 && strings.Contains(out[i:min(len(out), i+300)], "optional") {
		t.Errorf("the proxy-secret secretKeyRef must not be optional, got:\n%s", out[i:min(len(out), i+300)])
	}
}

func TestBeadsViewer_DevAuthProxySecret_OffByDefault(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads-viewer/deployment.yaml",
		"beadsViewer.enabled=true",
		"beadsViewer.devAuthBypass=true",
	)
	if strings.Contains(out, "BEADS_VIEWER_DEV_AUTH_PROXY_SECRET") {
		t.Errorf("proxy-secret env must be absent unless devAuthProxySecret.secretName is set, got:\n%s", out)
	}
}

func TestBeadsViewer_DevAuthBypass_DefaultOffKeepsOIDC(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads-viewer/deployment.yaml",
		"beadsViewer.enabled=true",
		"beadsViewer.oidc.issuerURL=https://auth.example.com/realms/x",
		"beadsViewer.oidc.clientID=viewer",
	)
	if strings.Contains(out, "BEADS_VIEWER_DEV_AUTH_BYPASS") {
		t.Errorf("BEADS_VIEWER_DEV_AUTH_BYPASS must NOT be present by default (devAuthBypass=false), got:\n%s", out)
	}
	if !strings.Contains(out, "OIDC_ISSUER_URL") {
		t.Errorf("OIDC_ISSUER_URL must still render when devAuthBypass is off, got:\n%s", out)
	}
}

// --- beadsViewer.mattEmail: GARDENIO_VIEWER_MATT_EMAIL env var plumbing ---

func TestBeadsViewer_MattEmail_DefaultNotPresent(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads-viewer/deployment.yaml",
		"beadsViewer.enabled=true",
		"beadsViewer.oidc.issuerURL=https://auth.example.com/realms/x",
		"beadsViewer.oidc.clientID=viewer",
	)
	if strings.Contains(out, "GARDENIO_VIEWER_MATT_EMAIL") {
		t.Errorf("GARDENIO_VIEWER_MATT_EMAIL should NOT be present when beadsViewer.mattEmail is unset, got:\n%s", out)
	}
}

func TestBeadsViewer_MattEmail_SetApplied(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads-viewer/deployment.yaml",
		"beadsViewer.enabled=true",
		"beadsViewer.oidc.issuerURL=https://auth.example.com/realms/x",
		"beadsViewer.oidc.clientID=viewer",
		"beadsViewer.mattEmail=matt@acme.example",
	)
	if !strings.Contains(out, "GARDENIO_VIEWER_MATT_EMAIL") {
		t.Fatalf("GARDENIO_VIEWER_MATT_EMAIL env var should be present when beadsViewer.mattEmail is set, got:\n%s", out)
	}
	if !strings.Contains(out, `value: "matt@acme.example"`) {
		t.Errorf("GARDENIO_VIEWER_MATT_EMAIL should reflect the configured value, got:\n%s", out)
	}
}

// --- beadsViewer legacy host retired (ka-0000004107) ---
//
// The legacy beads-viewer host (beads-viewer.gardenio.app.<env>.acme.example) and its
// 301-redirect-to-AVA IngressRoute (templates/beads-viewer/ingressroute.yaml)
// were removed after the Epic A adoption window (parent epic ka-0000005702).
// The canonical AVA-gated route (ava-ingressroute.yaml) and the Gardenio-in-a-Box
// forwardauth route (forwardauth-ingressroute.yaml) are the only public
// beads-viewer routes the chart renders now. The earlier X-Beads-Viewer-Bypass
// directRoute escape hatch was already retired in ka-0000000153.

// --- ka-0000003642: /image/{key...} proxy needs IRSA + S3 env on beads-viewer ---
//
// Without these the renderer's rewriteSessionImageURL returns "" (imageBucket
// empty) and the bundle image falls through to the link card. The handler
// also returns 503 with no S3 client wired. Both must be present for inline
// images to render in the viewer.

func TestBeadsViewer_S3Env_ForwardedFromAgentsConfig(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads-viewer/deployment.yaml",
		"beadsViewer.enabled=true",
		"beadsViewer.oidc.issuerURL=https://auth.example.com/realms/x",
		"beadsViewer.oidc.clientID=viewer",
		"agents.s3.bucket=gardenio-sessions",
		"agents.s3.prefix=coop/sessions",
		"agents.s3.region=us-east-1",
		// In-box object store (ka-0000004955): endpoint override forwarded
		// so the /image proxy resolves against the same in-cluster store.
		"agents.s3.endpoint=http://minio.gardenio.svc.cluster.local:9000",
	)
	for _, want := range []string{
		"name: COOP_S3_BUCKET",
		"value: gardenio-sessions",
		"name: COOP_S3_PREFIX",
		"value: coop/sessions",
		"name: COOP_S3_REGION",
		"value: us-east-1",
		"name: COOP_S3_ENDPOINT",
		"value: http://minio.gardenio.svc.cluster.local:9000",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in beads-viewer deployment, got:\n%s", want, out)
		}
	}
}

func TestBeadsViewer_S3Endpoint_OmittedWhenUnset(t *testing.T) {
	t.Parallel()
	// In-box object store (ka-0000004955): with no endpoint override the
	// viewer must not emit COOP_S3_ENDPOINT, so it falls back to AWS S3.
	out := helmTemplate(t, "templates/beads-viewer/deployment.yaml",
		"beadsViewer.enabled=true",
		"beadsViewer.oidc.issuerURL=https://auth.example.com/realms/x",
		"beadsViewer.oidc.clientID=viewer",
		"agents.s3.bucket=gardenio-sessions",
		setIRSA("agents.serviceAccount"),
		setIRSA("agents.agentServiceAccount"),
	)
	if strings.Contains(out, "COOP_S3_ENDPOINT") {
		t.Errorf("expected COOP_S3_ENDPOINT to be absent when agents.s3.endpoint is unset, got:\n%s", out)
	}
}

func TestBeadsViewer_S3Env_OmittedWhenBucketUnset(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads-viewer/deployment.yaml",
		"beadsViewer.enabled=true",
		"beadsViewer.oidc.issuerURL=https://auth.example.com/realms/x",
		"beadsViewer.oidc.clientID=viewer",
	)
	for _, off := range []string{"COOP_S3_BUCKET", "COOP_S3_PREFIX", "COOP_S3_REGION"} {
		if strings.Contains(out, off) {
			t.Errorf("expected %q to be absent when agents.s3.bucket is unset, got:\n%s", off, out)
		}
	}
}

func TestBeadsViewer_ServiceAccountName_ReusesControllerSA(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads-viewer/deployment.yaml",
		"beadsViewer.enabled=true",
		"beadsViewer.oidc.issuerURL=https://auth.example.com/realms/x",
		"beadsViewer.oidc.clientID=viewer",
	)
	if !strings.Contains(out, "serviceAccountName:") {
		t.Fatalf("beads-viewer deployment must set serviceAccountName so IRSA is available for the /image/* proxy (ka-0000003642), got:\n%s", out)
	}
	if !strings.Contains(out, "automountServiceAccountToken: false") {
		t.Errorf("beads-viewer must keep automountServiceAccountToken: false (IRSA projects its own token); got:\n%s", out)
	}
}

// agentsSARendered reports whether the agents ServiceAccount (release
// "test" => "test-agents-sa") appears as a rendered ServiceAccount doc in a
// full chart render. Uses a full render (not --show-only) because --show-only
// errors when the template produces no output.
func agentsSARendered(out string) bool {
	for _, d := range strings.Split(out, "\n---") {
		if strings.Contains(d, "kind: ServiceAccount") && strings.Contains(d, "name: test-agents-sa") {
			return true
		}
	}
	return false
}

// ka-0000005282: on the Gardenio-in-a-Box profile agents.enabled=false but
// beadsViewer.enabled=true. beads-viewer reuses the agents ServiceAccount
// (gardenio.agents.serviceAccountName), so the SA must still be created — the
// gate is (agents.enabled OR beadsViewer.enabled) AND serviceAccount.create.
// Before the fix the SA was gated on agents.enabled alone, so the beads-viewer
// ReplicaSet FailedCreate with "serviceaccount ... not found" and the login
// gate never got a backend.
func TestBeadsViewer_ServiceAccountRendersWhenAgentsDisabled(t *testing.T) {
	t.Parallel()
	out := helmTemplateFull(t, nil,
		"agents.enabled=false",
		"beadsViewer.enabled=true",
	)
	if !agentsSARendered(out) {
		t.Fatalf("agents ServiceAccount (test-agents-sa) must render when beadsViewer.enabled even if agents.enabled=false (ka-0000005282)")
	}
}

// Both features off => no SA (nothing references it).
func TestBeadsViewer_ServiceAccountAbsentWhenBothDisabled(t *testing.T) {
	t.Parallel()
	out := helmTemplateFull(t, nil,
		"agents.enabled=false",
		"beadsViewer.enabled=false",
	)
	if agentsSARendered(out) {
		t.Errorf("agents SA must NOT render when both agents and beadsViewer are disabled")
	}
}

// serviceAccount.create=false must suppress the SA even when beadsViewer is on
// (operator supplies their own SA out-of-band).
func TestBeadsViewer_ServiceAccountRespectsCreateFalse(t *testing.T) {
	t.Parallel()
	out := helmTemplateFull(t, nil,
		"agents.enabled=false",
		"beadsViewer.enabled=true",
		"agents.serviceAccount.create=false",
	)
	if agentsSARendered(out) {
		t.Errorf("agents SA must NOT render when serviceAccount.create=false")
	}
}

// --- gd-64RUGKGPEF / ka-0000002146: AVA default-role (blanket viewer) ---
//
// Acceptance: when avaIngress.enabled=true and trustSigner is set, the deployment
// MUST render the AVA_DEFAULT_ROLE env var. Its default value is "gardenio-viewer"
// (decision ka-0000002146: blanket viewer). Setting defaultRole="" must omit the
// env entirely (escape hatch / pre-decision behavior).

func TestBeadsViewer_AvaDefaultRole_DefaultsToViewer(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads-viewer/deployment.yaml",
		"beadsViewer.enabled=true",
		"beadsViewer.oidc.issuerURL=https://auth.example.com/realms/x",
		"beadsViewer.oidc.clientID=viewer",
		"beadsViewer.avaIngress.enabled=true",
		"beadsViewer.avaIngress.host=beads-viewer.app.e2e.dev.acme.example",
		"beadsViewer.avaIngress.trustSigner=arn:aws:ec2:us-east-1:234567890123:verified-access-instance/vai-test",
	)
	if !strings.Contains(out, "name: AVA_DEFAULT_ROLE") {
		t.Fatalf("AVA_DEFAULT_ROLE env var must be present when avaIngress is enabled (gd-64RUGKGPEF), got:\n%s", out)
	}
	if !strings.Contains(out, `value: "gardenio-viewer"`) {
		t.Errorf("AVA_DEFAULT_ROLE must default to gardenio-viewer (ka-0000002146: blanket viewer), got:\n%s", out)
	}
}

func TestBeadsViewer_AvaDefaultRole_OmittedWhenEmpty(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads-viewer/deployment.yaml",
		"beadsViewer.enabled=true",
		"beadsViewer.oidc.issuerURL=https://auth.example.com/realms/x",
		"beadsViewer.oidc.clientID=viewer",
		"beadsViewer.avaIngress.enabled=true",
		"beadsViewer.avaIngress.host=beads-viewer.app.e2e.dev.acme.example",
		"beadsViewer.avaIngress.trustSigner=arn:aws:ec2:us-east-1:234567890123:verified-access-instance/vai-test",
		"beadsViewer.avaIngress.defaultRole=",
	)
	if strings.Contains(out, "AVA_DEFAULT_ROLE") {
		t.Errorf("AVA_DEFAULT_ROLE must be absent when defaultRole is explicitly empty, got:\n%s", out)
	}
}

func TestBeadsViewer_AvaDefaultRole_OmittedWhenAvaDisabled(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads-viewer/deployment.yaml",
		"beadsViewer.enabled=true",
		"beadsViewer.oidc.issuerURL=https://auth.example.com/realms/x",
		"beadsViewer.oidc.clientID=viewer",
	)
	if strings.Contains(out, "AVA_DEFAULT_ROLE") {
		t.Errorf("AVA_DEFAULT_ROLE must be absent when avaIngress is disabled, got:\n%s", out)
	}
}

// --- beadsViewer GRAFANA_BASE_URL: "Open in Grafana ↗" link on /charts ---
// (task ka-0000006706, epic ka-0000007895)

// Default chart (no grafana, no unified host) must NOT set GRAFANA_BASE_URL —
// otherwise /charts would render a dead link.
func TestBeadsViewer_GrafanaBaseURL_DefaultAbsent(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads-viewer/deployment.yaml",
		"beadsViewer.enabled=true",
		"beadsViewer.oidc.issuerURL=https://auth.example.com/realms/x",
		"beadsViewer.oidc.clientID=viewer",
	)
	if strings.Contains(out, "GRAFANA_BASE_URL") {
		t.Errorf("GRAFANA_BASE_URL must be absent by default (no grafana/unified host), got:\n%s", out)
	}
}

// Explicit override always wins, even without the unified host.
func TestBeadsViewer_GrafanaBaseURL_ExplicitOverride(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads-viewer/deployment.yaml",
		"beadsViewer.enabled=true",
		"beadsViewer.oidc.issuerURL=https://auth.example.com/realms/x",
		"beadsViewer.oidc.clientID=viewer",
		"beadsViewer.grafanaBaseURL=https://grafana.example.com",
	)
	if !strings.Contains(out, "GRAFANA_BASE_URL") {
		t.Fatalf("GRAFANA_BASE_URL should be present when beadsViewer.grafanaBaseURL is set, got:\n%s", out)
	}
	if !strings.Contains(out, `value: "https://grafana.example.com"`) {
		t.Errorf("GRAFANA_BASE_URL should reflect the explicit override, got:\n%s", out)
	}
}

// When grafana is served under the unified host's /grafana subpath, the URL
// auto-derives to https://<host><path> with no explicit override.
func TestBeadsViewer_GrafanaBaseURL_DerivedFromUnifiedHost(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads-viewer/deployment.yaml",
		"beadsViewer.enabled=true",
		"beadsViewer.oidc.issuerURL=https://auth.example.com/realms/x",
		"beadsViewer.oidc.clientID=viewer",
		"grafana.enabled=true",
		"unifiedHost.enabled=true",
		"unifiedHost.host=gardenio.app.e2e.dev.acme.example",
		"unifiedHost.paths.grafana=/grafana",
	)
	if !strings.Contains(out, "GRAFANA_BASE_URL") {
		t.Fatalf("GRAFANA_BASE_URL should derive when grafana is on the unified host, got:\n%s", out)
	}
	if !strings.Contains(out, `value: "https://gardenio.app.e2e.dev.acme.example/grafana"`) {
		t.Errorf("GRAFANA_BASE_URL should derive to host+path, got:\n%s", out)
	}
}

// Grafana enabled but NOT fanned out on the unified host (empty path) → no
// derivation, so the link stays hidden rather than pointing at a dead route.
func TestBeadsViewer_GrafanaBaseURL_OmittedWhenNoUnifiedPath(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads-viewer/deployment.yaml",
		"beadsViewer.enabled=true",
		"beadsViewer.oidc.issuerURL=https://auth.example.com/realms/x",
		"beadsViewer.oidc.clientID=viewer",
		"grafana.enabled=true",
		"unifiedHost.enabled=true",
		"unifiedHost.host=gardenio.app.e2e.dev.acme.example",
	)
	if strings.Contains(out, "GRAFANA_BASE_URL") {
		t.Errorf("GRAFANA_BASE_URL must be absent when unifiedHost.paths.grafana is empty, got:\n%s", out)
	}
}

// --- beadsViewer ESCALATION_REVIEW_URL: escalations "Review →" deep link ---
// (task ka-0000003740, epic ka-0000006413; R3 design §8.2)

// Default chart (no override, no unified host) must NOT set ESCALATION_REVIEW_URL
// — this preserves today's behaviour, where the deep link is silently disabled.
func TestBeadsViewer_EscalationReviewURL_DefaultAbsent(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads-viewer/deployment.yaml",
		"beadsViewer.enabled=true",
		"beadsViewer.oidc.issuerURL=https://auth.example.com/realms/x",
		"beadsViewer.oidc.clientID=viewer",
	)
	if strings.Contains(out, "ESCALATION_REVIEW_URL") {
		t.Errorf("ESCALATION_REVIEW_URL must be absent by default (no override/unified host), got:\n%s", out)
	}
}

// Explicit override always wins, even without the unified host (tier-1).
func TestBeadsViewer_EscalationReviewURL_ExplicitOverride(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads-viewer/deployment.yaml",
		"beadsViewer.enabled=true",
		"beadsViewer.oidc.issuerURL=https://auth.example.com/realms/x",
		"beadsViewer.oidc.clientID=viewer",
		"beadsViewer.escalationReviewURL=https://bridge.example.com/escalations",
	)
	if !strings.Contains(out, "ESCALATION_REVIEW_URL") {
		t.Fatalf("ESCALATION_REVIEW_URL should be present when beadsViewer.escalationReviewURL is set, got:\n%s", out)
	}
	if !strings.Contains(out, `value: "https://bridge.example.com/escalations"`) {
		t.Errorf("ESCALATION_REVIEW_URL should reflect the explicit override, got:\n%s", out)
	}
}

// When the unified host fans out an /escalations subpath, the URL auto-derives
// to https://<host><path> with no explicit override (tier-2).
func TestBeadsViewer_EscalationReviewURL_DerivedFromUnifiedHost(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads-viewer/deployment.yaml",
		"beadsViewer.enabled=true",
		"beadsViewer.oidc.issuerURL=https://auth.example.com/realms/x",
		"beadsViewer.oidc.clientID=viewer",
		"unifiedHost.enabled=true",
		"unifiedHost.host=gardenio.app.e2e.dev.acme.example",
		"unifiedHost.paths.escalations=/escalations",
	)
	if !strings.Contains(out, "ESCALATION_REVIEW_URL") {
		t.Fatalf("ESCALATION_REVIEW_URL should derive when /escalations is on the unified host, got:\n%s", out)
	}
	if !strings.Contains(out, `value: "https://gardenio.app.e2e.dev.acme.example/escalations"`) {
		t.Errorf("ESCALATION_REVIEW_URL should derive to host+path, got:\n%s", out)
	}
}

// Unified host enabled but the /escalations route NOT fanned out (empty path) →
// no derivation, so the deep link stays hidden rather than pointing nowhere.
// This is the SAFE guard: tier-2 only activates once the route is confirmed.
func TestBeadsViewer_EscalationReviewURL_OmittedWhenNoUnifiedPath(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads-viewer/deployment.yaml",
		"beadsViewer.enabled=true",
		"beadsViewer.oidc.issuerURL=https://auth.example.com/realms/x",
		"beadsViewer.oidc.clientID=viewer",
		"unifiedHost.enabled=true",
		"unifiedHost.host=gardenio.app.e2e.dev.acme.example",
	)
	if strings.Contains(out, "ESCALATION_REVIEW_URL") {
		t.Errorf("ESCALATION_REVIEW_URL must be absent when unifiedHost.paths.escalations is empty, got:\n%s", out)
	}
}

// Explicit override must beat the unified-host derivation (tier-1 > tier-2).
func TestBeadsViewer_EscalationReviewURL_OverrideBeatsUnified(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads-viewer/deployment.yaml",
		"beadsViewer.enabled=true",
		"beadsViewer.oidc.issuerURL=https://auth.example.com/realms/x",
		"beadsViewer.oidc.clientID=viewer",
		"beadsViewer.escalationReviewURL=https://bridge.example.com/escalations",
		"unifiedHost.enabled=true",
		"unifiedHost.host=gardenio.app.e2e.dev.acme.example",
		"unifiedHost.paths.escalations=/escalations",
	)
	if !strings.Contains(out, `value: "https://bridge.example.com/escalations"`) {
		t.Errorf("explicit escalationReviewURL override should win over the unified-host derivation, got:\n%s", out)
	}
	if strings.Contains(out, `value: "https://gardenio.app.e2e.dev.acme.example/escalations"`) {
		t.Errorf("unified-host derivation must not appear when an explicit override is set, got:\n%s", out)
	}
}

// --- ka-0000000593: AVA host must strip the BASE_PATH prefix ---
//
// When unifiedHost is enabled the viewer pod's BASE_PATH resolves to
// paths.beads (e.g. "/beads"), so every template link renders prefixed
// (/beads/decisions, /beads/assets/static/...). The unified host strips that
// prefix in its own IngressRoute; the AVA route forwards the path verbatim,
// so without a matching stripPrefix middleware the prefixed links 404 on the
// AVA host (the user-visible P0: top-level links sent users to
// /beads/decisions instead of /decisions). The AVA route must attach a
// stripPrefix middleware mirroring the unified host.

func TestBeadsViewer_AvaIngress_StripsBasePathWhenUnifiedHostOn(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads-viewer/ava-ingressroute.yaml",
		"beadsViewer.enabled=true",
		"beadsViewer.oidc.issuerURL=https://auth.example.com/realms/x",
		"beadsViewer.oidc.clientID=viewer",
		"beadsViewer.avaIngress.enabled=true",
		"beadsViewer.avaIngress.host=beads-viewer.app.e2e.dev.acme.example",
		"unifiedHost.enabled=true",
		"unifiedHost.host=gardenio.app.e2e.dev.acme.example",
		"unifiedHost.paths.beads=/beads",
	)
	if !strings.Contains(out, "kind: Middleware") || !strings.Contains(out, "stripPrefix") {
		t.Fatalf("AVA route must render a stripPrefix Middleware when BASE_PATH is non-empty, got:\n%s", out)
	}
	if !strings.Contains(out, "- /beads") {
		t.Errorf("stripPrefix middleware must strip the /beads prefix the viewer emits, got:\n%s", out)
	}
	if !strings.Contains(out, "name: test-beads-viewer-ava-stripprefix") {
		t.Errorf("AVA IngressRoute must reference the stripPrefix middleware, got:\n%s", out)
	}
}

// When unifiedHost is off, BASE_PATH (and ingress.pathPrefix) are empty, so
// the viewer emits root-relative links and no stripPrefix is needed. The AVA
// route must NOT attach a middleware in that case — stripping a nonexistent
// prefix is a no-op at best and a footgun if the prefix is later set blank.
func TestBeadsViewer_AvaIngress_NoStripPrefixWhenBasePathEmpty(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads-viewer/ava-ingressroute.yaml",
		"beadsViewer.enabled=true",
		"beadsViewer.oidc.issuerURL=https://auth.example.com/realms/x",
		"beadsViewer.oidc.clientID=viewer",
		"beadsViewer.avaIngress.enabled=true",
		"beadsViewer.avaIngress.host=beads-viewer.app.e2e.dev.acme.example",
	)
	if strings.Contains(out, "stripPrefix") || strings.Contains(out, "ava-stripprefix") {
		t.Errorf("AVA route must NOT render a stripPrefix middleware when BASE_PATH is empty, got:\n%s", out)
	}
}

// --- GRAFANA_UPSTREAM_URL: the AVA-authenticated /grafana reverse proxy wiring
// (Option C, decision ka-0000004242, epic ka-0000000746). beads-viewer fronts
// the in-cluster Grafana under its own same-origin /grafana subpath; the env var
// is what turns the proxy on. See grafana_proxy.go. ---

func TestBeadsViewer_GrafanaUpstream_AbsentByDefault(t *testing.T) {
	t.Parallel()
	// grafana.enabled defaults false, so no proxy is fronted.
	out := helmTemplate(t, "templates/beads-viewer/deployment.yaml",
		"beadsViewer.enabled=true",
		"beadsViewer.oidc.issuerURL=https://auth.example.com/realms/x",
		"beadsViewer.oidc.clientID=viewer",
	)
	if strings.Contains(out, "GRAFANA_UPSTREAM_URL") {
		t.Errorf("GRAFANA_UPSTREAM_URL should NOT render when grafana is disabled, got:\n%s", out)
	}
}

func TestBeadsViewer_GrafanaUpstream_NotSetWhenAuthProxyOff(t *testing.T) {
	t.Parallel()
	// grafana.enabled but auth-proxy OFF: fronting a Grafana that runs its own
	// login form would bounce every proxied request, so the upstream must NOT
	// auto-derive. Only the explicit override (next test) can force it.
	out := helmTemplate(t, "templates/beads-viewer/deployment.yaml",
		"beadsViewer.enabled=true",
		"beadsViewer.oidc.issuerURL=https://auth.example.com/realms/x",
		"beadsViewer.oidc.clientID=viewer",
		"grafana.enabled=true",
	)
	if strings.Contains(out, "GRAFANA_UPSTREAM_URL") {
		t.Errorf("GRAFANA_UPSTREAM_URL should NOT auto-derive without grafana.auth.proxy.enabled, got:\n%s", out)
	}
}

func TestBeadsViewer_GrafanaUpstream_AutoDerivesWhenAuthProxyOn(t *testing.T) {
	t.Parallel()
	// grafana.enabled + auth.proxy.enabled: the upstream auto-derives to the
	// in-cluster Grafana Service so the /grafana subpath comes up without
	// hand-wiring a URL.
	out := helmTemplate(t, "templates/beads-viewer/deployment.yaml",
		"beadsViewer.enabled=true",
		"beadsViewer.oidc.issuerURL=https://auth.example.com/realms/x",
		"beadsViewer.oidc.clientID=viewer",
		"grafana.enabled=true",
		"grafana.auth.proxy.enabled=true",
	)
	if !strings.Contains(out, "GRAFANA_UPSTREAM_URL") {
		t.Fatalf("GRAFANA_UPSTREAM_URL should render when grafana + auth.proxy are on, got:\n%s", out)
	}
	// Root Service URL (no /grafana suffix) — Grafana serves the subpath itself.
	if !strings.Contains(out, "http://test-grafana:3000") {
		t.Errorf("GRAFANA_UPSTREAM_URL should point at the in-cluster Grafana Service root (http://<release>-grafana:3000), got:\n%s", out)
	}
	if strings.Contains(out, "grafana:3000/grafana") {
		t.Errorf("GRAFANA_UPSTREAM_URL must be the Service ROOT — Grafana owns the /grafana subpath via serveFromSubPath, got:\n%s", out)
	}
}

func TestBeadsViewer_GrafanaUpstream_ExplicitOverrideWins(t *testing.T) {
	t.Parallel()
	// An explicit beadsViewer.grafanaUpstreamURL wins even with auth-proxy off —
	// the operator has taken responsibility for the Grafana-side config.
	out := helmTemplate(t, "templates/beads-viewer/deployment.yaml",
		"beadsViewer.enabled=true",
		"beadsViewer.oidc.issuerURL=https://auth.example.com/realms/x",
		"beadsViewer.oidc.clientID=viewer",
		"beadsViewer.grafanaUpstreamURL=http://custom-grafana:3000",
	)
	if !strings.Contains(out, "http://custom-grafana:3000") {
		t.Fatalf("explicit beadsViewer.grafanaUpstreamURL should render as GRAFANA_UPSTREAM_URL, got:\n%s", out)
	}
}

// gd-lHJot24kJu: the proxy must stamp the header Grafana's auth-proxy reads
// (GF_AUTH_PROXY_HEADER_NAME = grafana.auth.proxy.headerName), so the chart
// passes it as GRAFANA_AUTH_PROXY_HEADER whenever auth.proxy is on.
func TestBeadsViewer_GrafanaAuthProxyHeader_FollowsHeaderName(t *testing.T) {
	t.Parallel()
	base := []string{
		"beadsViewer.enabled=true",
		"beadsViewer.oidc.issuerURL=https://auth.example.com/realms/x",
		"beadsViewer.oidc.clientID=viewer",
		"grafana.enabled=true",
		"grafana.auth.proxy.enabled=true",
	}
	out := helmTemplate(t, "templates/beads-viewer/deployment.yaml", base...)
	if !strings.Contains(out, "- name: GRAFANA_AUTH_PROXY_HEADER\n              value: \"X-Auth-Request-Email\"") {
		t.Errorf("GRAFANA_AUTH_PROXY_HEADER should default to grafana.auth.proxy.headerName (X-Auth-Request-Email), got:\n%s", out)
	}

	custom := helmTemplate(t, "templates/beads-viewer/deployment.yaml",
		append(base, "grafana.auth.proxy.headerName=X-Custom-User")...)
	if !strings.Contains(custom, "- name: GRAFANA_AUTH_PROXY_HEADER\n              value: \"X-Custom-User\"") {
		t.Errorf("GRAFANA_AUTH_PROXY_HEADER should follow grafana.auth.proxy.headerName, got:\n%s", custom)
	}
}

func TestBeadsViewer_GrafanaAuthProxyHeader_AbsentWhenAuthProxyOff(t *testing.T) {
	t.Parallel()
	// Explicit upstream with auth.proxy off: the chart does not configure
	// Grafana's header name, so the proxy keeps its X-WEBAUTH-USER default.
	out := helmTemplate(t, "templates/beads-viewer/deployment.yaml",
		"beadsViewer.enabled=true",
		"beadsViewer.oidc.issuerURL=https://auth.example.com/realms/x",
		"beadsViewer.oidc.clientID=viewer",
		"grafana.enabled=true",
		"beadsViewer.grafanaUpstreamURL=http://custom-grafana:3000",
	)
	if strings.Contains(out, "GRAFANA_AUTH_PROXY_HEADER") {
		t.Errorf("GRAFANA_AUTH_PROXY_HEADER should NOT render when grafana.auth.proxy is off, got:\n%s", out)
	}
}

// gd-cTobdw0kAL: beadsViewer.nodeAffinity is a separate lever from the full
// beadsViewer.affinity override — it must layer onto the default soft
// podAntiAffinity spread (both render together at replicas>1, the deployed
// shape today) rather than replacing it, since the two express independent
// concerns (which nodes vs. spreading across them).
func TestBeadsViewer_NodeAffinity_LayersOntoPodAntiAffinity(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads-viewer/deployment.yaml",
		"beadsViewer.enabled=true",
		"beadsViewer.oidc.issuerURL=https://auth.example.com/realms/x",
		"beadsViewer.oidc.clientID=viewer",
		"beadsViewer.replicaCount=2",
		"beadsViewer.nodeAffinity.preferredDuringSchedulingIgnoredDuringExecution[0].weight=100",
		"beadsViewer.nodeAffinity.preferredDuringSchedulingIgnoredDuringExecution[0].preference.matchExpressions[0].key=gardenio.io/lifecycle",
		"beadsViewer.nodeAffinity.preferredDuringSchedulingIgnoredDuringExecution[0].preference.matchExpressions[0].operator=In",
		"beadsViewer.nodeAffinity.preferredDuringSchedulingIgnoredDuringExecution[0].preference.matchExpressions[0].values[0]=spot",
	)
	if !strings.Contains(out, "nodeAffinity:") || !strings.Contains(out, "gardenio.io/lifecycle") {
		t.Errorf("beadsViewer.nodeAffinity must render; got:\n%s", out)
	}
	if !strings.Contains(out, "podAntiAffinity:") {
		t.Errorf("beadsViewer.nodeAffinity must NOT suppress the default HA podAntiAffinity spread; got:\n%s", out)
	}

	// The full beadsViewer.affinity override still wins outright and
	// suppresses nodeAffinity entirely.
	override := helmTemplate(t, "templates/beads-viewer/deployment.yaml",
		"beadsViewer.enabled=true",
		"beadsViewer.oidc.issuerURL=https://auth.example.com/realms/x",
		"beadsViewer.oidc.clientID=viewer",
		"beadsViewer.replicaCount=2",
		"beadsViewer.nodeAffinity.preferredDuringSchedulingIgnoredDuringExecution[0].weight=100",
		"beadsViewer.nodeAffinity.preferredDuringSchedulingIgnoredDuringExecution[0].preference.matchExpressions[0].key=gardenio.io/lifecycle",
		"beadsViewer.nodeAffinity.preferredDuringSchedulingIgnoredDuringExecution[0].preference.matchExpressions[0].operator=In",
		"beadsViewer.nodeAffinity.preferredDuringSchedulingIgnoredDuringExecution[0].preference.matchExpressions[0].values[0]=spot",
		"beadsViewer.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[0].key=kubernetes.io/os",
		"beadsViewer.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[0].operator=In",
		"beadsViewer.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[0].values[0]=linux",
	)
	if strings.Contains(override, "gardenio.io/lifecycle") {
		t.Errorf("explicit beadsViewer.affinity must WIN outright and suppress beadsViewer.nodeAffinity too; got:\n%s", override)
	}
	if !strings.Contains(override, "kubernetes.io/os") {
		t.Errorf("explicit beadsViewer.affinity must still render; got:\n%s", override)
	}
}

// --- beadsViewer.theme: BEADS_VIEWER_THEME env plumbing (S1j, gd-cxU8eyiGyR) ---
//
// The Dawn Meadow look is the binary's default, so the chart renders no env
// unless the operator sets a value; "plain" is the runtime kill switch that
// restores the pre-theme look without an image change.

func TestBeadsViewer_Theme_DefaultRendersNoEnv(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads-viewer/deployment.yaml",
		"beadsViewer.enabled=true",
	)
	if strings.Contains(out, "BEADS_VIEWER_THEME") {
		t.Errorf("BEADS_VIEWER_THEME must NOT render by default (the binary defaults to the meadow), got:\n%s", out)
	}
}

func TestBeadsViewer_Theme_PlainRendersKillSwitch(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads-viewer/deployment.yaml",
		"beadsViewer.enabled=true",
		"beadsViewer.theme=plain",
	)
	if !strings.Contains(out, "- name: BEADS_VIEWER_THEME\n              value: \"plain\"") {
		t.Errorf("beadsViewer.theme=plain must render BEADS_VIEWER_THEME=\"plain\", got:\n%s", out)
	}
}

// --- beadsViewer.requireUserBearerForWrites: on in every deployment (gd-eXNRKIIyzD) ---
//
// Lands boundary step 1 (rec gd-uYNbyW4a9Z §6): a viewer write without the
// signed-in user's own bearer is refused, so the static daemon token is a read
// credential only. The chart default is on; false is the rollback.

func TestBeadsViewer_RequireUserBearerForWrites_OnByDefault(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads-viewer/deployment.yaml",
		"beadsViewer.enabled=true",
		"beadsViewer.oidc.issuerURL=https://auth.example.com/realms/x",
		"beadsViewer.oidc.clientID=viewer",
	)
	if !strings.Contains(out, "name: BEADS_VIEWER_REQUIRE_USER_BEARER_WRITES\n              value: \"1\"") {
		t.Errorf("BEADS_VIEWER_REQUIRE_USER_BEARER_WRITES=1 must render by default, got:\n%s", out)
	}
}

func TestBeadsViewer_RequireUserBearerForWrites_FalseIsRollback(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads-viewer/deployment.yaml",
		"beadsViewer.enabled=true",
		"beadsViewer.oidc.issuerURL=https://auth.example.com/realms/x",
		"beadsViewer.oidc.clientID=viewer",
		"beadsViewer.requireUserBearerForWrites=false",
	)
	if strings.Contains(out, "BEADS_VIEWER_REQUIRE_USER_BEARER_WRITES") {
		t.Errorf("BEADS_VIEWER_REQUIRE_USER_BEARER_WRITES must not render with requireUserBearerForWrites=false, got:\n%s", out)
	}
}
