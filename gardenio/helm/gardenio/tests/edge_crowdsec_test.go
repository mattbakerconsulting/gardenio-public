package tests

import (
	"strings"
	"testing"
)

// P3.3 CrowdSec WAF-lite (ka-0000001941; ingress doc §5, ka-0000002581).
// CrowdSec agent (LAPI) Deployment+Service+Secret + the Traefik bouncer
// Middleware, gated DEFAULT OFF and flipped on by the in-box overlay. The
// bouncer runs in streaming mode and fails OPEN.

// 1. DEFAULT OFF: no CrowdSec objects render out of the box.
func TestCrowdSec_DefaultDisabled(t *testing.T) {
	t.Parallel()
	out := helmTemplateFull(t, []string{"values-test.yaml"})
	if strings.Contains(out, "edge-crowdsec") {
		t.Error("no CrowdSec objects should render when edge.crowdsec.enabled is false (default)")
	}
}

//  2. Enabled: agent Deployment + Service + bouncer Middleware render, and the
//     Middleware runs in stream mode + fails open.
func TestCrowdSec_EnabledRendersAgentAndMiddleware(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/edge-crowdsec.yaml",
		"traefik.enabled=true",
		"edge.crowdsec.enabled=true",
	)
	if !strings.Contains(out, "kind: Deployment") {
		t.Error("CrowdSec agent Deployment should render when enabled")
	}
	if !strings.Contains(out, "kind: Middleware") {
		t.Error("CrowdSec bouncer Middleware should render when enabled")
	}
	if !strings.Contains(out, "crowdsecMode: \"stream\"") {
		t.Errorf("bouncer must run in streaming mode, got:\n%s", out)
	}
	// fail-open => failureBlock false.
	if !strings.Contains(out, "failureBlock: false") {
		t.Errorf("bouncer must fail OPEN (failureBlock: false), got:\n%s", out)
	}
	// The chart generates a bouncer-key Secret when none is provided.
	if !strings.Contains(out, "bouncer-key:") {
		t.Error("a bouncer-key Secret should be generated when bouncerKeySecret is unset")
	}
	// Air-gapped startup (ka-0000000072): the agent runs under default-deny
	// egress on the box, so the entrypoint MUST NOT dial the CrowdSec Central
	// API or Hub. Without these the entrypoint's `set -e` `cscli capi register`
	// crashes on blocked egress -> CrashLoopBackOff.
	if !strings.Contains(out, "DISABLE_ONLINE_API") {
		t.Errorf("crowdsec agent must set DISABLE_ONLINE_API for air-gapped startup, got:\n%s", out)
	}
	if !strings.Contains(out, "NO_HUB_UPGRADE") {
		t.Errorf("crowdsec agent must set NO_HUB_UPGRADE to skip the network hub upgrade, got:\n%s", out)
	}
}

//  3. The crowdsec Middleware is attached to rendered routes via the
//     edgeMiddlewares helper (regression guard for the chain wiring).
func TestCrowdSec_AttachedToRoutes(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/landing/ingressroute.yaml",
		"traefik.enabled=true",
		"edge.crowdsec.enabled=true",
		"keycloak.enabled=true",
		"landing.enabled=true",
		"landing.ingress.enabled=true",
		"landing.ingress.host=x.test",
	)
	if !strings.Contains(out, "-edge-crowdsec") {
		t.Errorf("rendered routes must carry the crowdsec middleware, got:\n%s", out)
	}
}

//  5. Regression (gd-qYD8piysYM): an operator who wants fail-CLOSED behaviour
//     (block traffic when the CrowdSec LAPI is unreachable) sets
//     `edge.crowdsec.failOpen=false`. The template previously rendered
//     `failureBlock: {{ not ($cs.failOpen | default true) }}`; Sprig's `default`
//     treats boolean false as empty and clobbered it back to true, so the
//     render still produced `failureBlock: false` (fail-open) and the operator's
//     intent was silently discarded. The fix references `$cs.failOpen` directly.
func TestCrowdSec_FailClosedHonored(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/edge-crowdsec.yaml",
		"traefik.enabled=true",
		"edge.crowdsec.enabled=true",
		"edge.crowdsec.failOpen=false",
	)
	if !strings.Contains(out, "failureBlock: true") {
		t.Errorf("failOpen=false must render failureBlock: true (fail CLOSED), got:\n%s", out)
	}
	if strings.Contains(out, "failureBlock: false") {
		t.Errorf("failOpen=false must NOT render failureBlock: false (default clobbered the override), got:\n%s", out)
	}
}
