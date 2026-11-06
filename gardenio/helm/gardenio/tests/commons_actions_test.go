package tests

import (
	"strings"
	"testing"
)

// gd-na5unRTCaP: Commons card actions (Commons II step 5, gd-amX5uMx4KM). The
// viewer's POST /garden/c/action|submit go to the bridge's
// /api/conversation/inbound on the signed-in person's own bearer, which the
// bridge verifies with OIDC. slackBridge.commonsActions.enabled renders the
// viewer's SLACK_BRIDGE_URL, the bridge's OIDC_ISSUER_URL + OIDC_CLIENT_ID and
// the one NetworkPolicy hop (viewer egress, bridge ingress), plus
// SLACK_BRIDGE_OIDC_COMMONS_ONLY so that OIDC verifies the inbound route alone
// and every other bridge /api/* web route stays closed. Off by default.

func commonsActionsFlags(extra ...string) []string {
	return append([]string{
		"networkPolicies.enabled=true",
		"slackBridge.enabled=true",
		"beadsViewer.enabled=true",
		"keycloak.enabled=true",
		"keycloak.realm.issuerURL=https://auth.example.test/auth/realms/gardenio",
		"keycloak.oidcAudience=gardenio-traefik-forwardauth",
	}, extra...)
}

func commonsActionsOn(extra ...string) []string {
	return commonsActionsFlags(append([]string{"slackBridge.commonsActions.enabled=true"}, extra...)...)
}

// viewerToBridgeRule is the beads-viewer egress rule that reaches the bridge.
func viewerToBridgeRule(egress string) string {
	for r := range strings.SplitSeq(egress, "\n    - to:") {
		if strings.Contains(r, "app.kubernetes.io/component: slack-bridge") {
			return r
		}
	}
	return ""
}

// bridgeFromViewerRule is the slack-bridge ingress rule that admits the viewer.
func bridgeFromViewerRule(ingress string) string {
	for _, r := range ingressRules(ingress) {
		if strings.Contains(r, "app.kubernetes.io/component: beads-viewer") {
			return r
		}
	}
	return ""
}

func TestCommonsActions_DefaultRendersNothing(t *testing.T) {
	t.Parallel()
	docs := splitDocs(helmTemplate(t, "", commonsActionsFlags()...))
	if v, ok := envValueAfter(docs["Deployment/test-beads-viewer"], "SLACK_BRIDGE_URL"); ok {
		t.Errorf("beads-viewer has SLACK_BRIDGE_URL=%q with commonsActions off", v)
	}
	bridge := docs["Deployment/test-slack-bridge"]
	if bridge == "" {
		t.Fatal("slack-bridge Deployment did not render")
	}
	for _, key := range []string{"OIDC_ISSUER_URL", "OIDC_CLIENT_ID", "SLACK_BRIDGE_OIDC_COMMONS_ONLY"} {
		if v, ok := envValueAfter(bridge, key); ok {
			t.Errorf("slack-bridge has %s=%q with commonsActions off: its /api/* must stay closed", key, v)
		}
	}
	if r := viewerToBridgeRule(docs["NetworkPolicy/test-beads-viewer-egress"]); r != "" {
		t.Errorf("beads-viewer egress reaches slack-bridge with commonsActions off:\n%s", r)
	}
	if r := bridgeFromViewerRule(docs["NetworkPolicy/test-slack-bridge-ingress"]); r != "" {
		t.Errorf("slack-bridge ingress admits beads-viewer with commonsActions off:\n%s", r)
	}
}

func TestCommonsActions_ViewerGetsTheBridgeURL(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads-viewer/deployment.yaml", commonsActionsOn("slackBridge.service.port=9190")...)
	if v, ok := envValueAfter(out, "SLACK_BRIDGE_URL"); !ok || v != "http://test-slack-bridge:9190" {
		t.Errorf("SLACK_BRIDGE_URL = %q (set %v), want http://test-slack-bridge:9190", v, ok)
	}
}

func TestCommonsActions_BridgeVerifiesTheKbeadsIssuerAndAudience(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/slack-bridge/deployment.yaml", commonsActionsOn()...)
	if v, _ := envValueAfter(out, "OIDC_ISSUER_URL"); v != "https://auth.example.test/auth/realms/gardenio" {
		t.Errorf("OIDC_ISSUER_URL = %q, want the derived realm issuer", v)
	}
	if v, _ := envValueAfter(out, "OIDC_CLIENT_ID"); v != "gardenio-traefik-forwardauth" {
		t.Errorf("OIDC_CLIENT_ID = %q, want keycloak.oidcAudience", v)
	}
}

// The bridge's OIDC is scoped to the Commons inbound route whenever it renders,
// including with explicit overrides: it must never open the other /api/* web
// routes (decisions, escalations, jira, the agent routes' admin fallback).
func TestCommonsActions_BridgeOIDCIsScopedToInbound(t *testing.T) {
	t.Parallel()
	for _, extra := range [][]string{nil, {
		"slackBridge.commonsActions.oidc.issuerURL=https://idp.example.test/realms/other",
		"slackBridge.commonsActions.oidc.clientID=other-aud",
	}} {
		out := helmTemplate(t, "templates/slack-bridge/deployment.yaml", commonsActionsOn(extra...)...)
		if v, ok := envValueAfter(out, "SLACK_BRIDGE_OIDC_COMMONS_ONLY"); !ok || v != "true" {
			t.Errorf("overrides %v: SLACK_BRIDGE_OIDC_COMMONS_ONLY = %q (set %v), want \"true\" beside OIDC_ISSUER_URL", extra, v, ok)
		}
	}
}

func TestCommonsActions_ExplicitOIDCOverridesWin(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/slack-bridge/deployment.yaml", commonsActionsOn(
		"slackBridge.commonsActions.oidc.issuerURL=https://idp.example.test/realms/other",
		"slackBridge.commonsActions.oidc.clientID=other-aud",
	)...)
	if v, _ := envValueAfter(out, "OIDC_ISSUER_URL"); v != "https://idp.example.test/realms/other" {
		t.Errorf("OIDC_ISSUER_URL = %q, want the explicit override", v)
	}
	if v, _ := envValueAfter(out, "OIDC_CLIENT_ID"); v != "other-aud" {
		t.Errorf("OIDC_CLIENT_ID = %q, want the explicit override", v)
	}
}

// Enabled with no issuer or audience to verify against fails the render
// rather than shipping a bridge that 503s every Commons action.
func TestCommonsActions_RequiresIssuerAndAudience(t *testing.T) {
	t.Parallel()
	out := helmTemplateExpectError(t, "templates/slack-bridge/deployment.yaml",
		"--set", "slackBridge.enabled=true", "--set", "beadsViewer.enabled=true",
		"--set", "slackBridge.commonsActions.enabled=true")
	if !strings.Contains(out, "slackBridge.commonsActions.oidc.issuerURL is required") {
		t.Errorf("missing issuer: want the required error, got:\n%s", out)
	}
	out = helmTemplateExpectError(t, "templates/slack-bridge/deployment.yaml",
		"--set", "slackBridge.enabled=true", "--set", "beadsViewer.enabled=true",
		"--set", "slackBridge.commonsActions.enabled=true",
		"--set", "slackBridge.commonsActions.oidc.issuerURL=https://idp.example.test/realms/g",
		"--set", "keycloak.oidcAudience=")
	if !strings.Contains(out, "slackBridge.commonsActions.oidc.clientID (or keycloak.oidcAudience) is required") {
		t.Errorf("missing audience: want the required error, got:\n%s", out)
	}
}

func TestCommonsActions_NetworkPolicyHopIsViewerToBridgePortOnly(t *testing.T) {
	t.Parallel()
	docs := splitDocs(helmTemplate(t, "templates/networkpolicies/networkpolicies.yaml",
		commonsActionsOn("slackBridge.service.port=9190")...))

	egress := docs["NetworkPolicy/test-beads-viewer-egress"]
	rule := viewerToBridgeRule(egress)
	if rule == "" {
		t.Fatalf("beads-viewer egress has no rule to slack-bridge:\n%s", egress)
	}
	if !strings.Contains(rule, "port: 9190") || strings.Count(rule, "port:") != 1 {
		t.Errorf("the viewer's bridge rule must allow exactly the bridge's http port 9190:\n%s", rule)
	}

	ingress := docs["NetworkPolicy/test-slack-bridge-ingress"]
	rule = bridgeFromViewerRule(ingress)
	if rule == "" {
		t.Fatalf("slack-bridge ingress has no rule from beads-viewer:\n%s", ingress)
	}
	if !strings.Contains(rule, "port: 9190") || strings.Count(rule, "port:") != 1 {
		t.Errorf("the bridge's viewer rule must admit exactly the bridge's http port 9190:\n%s", rule)
	}
	for _, r := range []string{viewerToBridgeRule(egress), rule} {
		for _, bad := range []string{"namespaceSelector", "ipBlock"} {
			if strings.Contains(r, bad) {
				t.Errorf("the viewer<->bridge rule must name the pod only, found %q:\n%s", bad, r)
			}
		}
	}
}

// The hop and the env go with their components: no viewer, no bridge OIDC and
// no bridge ingress from it; no bridge, no SLACK_BRIDGE_URL and no egress.
func TestCommonsActions_GoesWithItsComponents(t *testing.T) {
	t.Parallel()
	docs := splitDocs(helmTemplate(t, "", commonsActionsOn("beadsViewer.enabled=false")...))
	if _, ok := envValueAfter(docs["Deployment/test-slack-bridge"], "OIDC_ISSUER_URL"); ok {
		t.Error("slack-bridge OIDC_ISSUER_URL renders with the viewer disabled")
	}
	if r := bridgeFromViewerRule(docs["NetworkPolicy/test-slack-bridge-ingress"]); r != "" {
		t.Errorf("slack-bridge ingress admits beads-viewer with the viewer disabled:\n%s", r)
	}

	docs = splitDocs(helmTemplate(t, "", commonsActionsOn("slackBridge.enabled=false")...))
	if _, ok := envValueAfter(docs["Deployment/test-beads-viewer"], "SLACK_BRIDGE_URL"); ok {
		t.Error("beads-viewer SLACK_BRIDGE_URL renders with the bridge disabled")
	}
	if r := viewerToBridgeRule(docs["NetworkPolicy/test-beads-viewer-egress"]); r != "" {
		t.Errorf("beads-viewer egress reaches slack-bridge with the bridge disabled:\n%s", r)
	}
}
