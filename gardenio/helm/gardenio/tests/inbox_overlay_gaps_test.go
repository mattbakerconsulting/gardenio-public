package tests

import (
	"strings"
	"testing"
)

// sliceDoc returns the YAML document (split on the doc separator) that contains
// the given substring, or "" if none. Used to scope Contains checks to a single
// rendered object.
func sliceDoc(out, needle string) string {
	for doc := range strings.SplitSeq(out, "\n---\n") {
		if strings.Contains(doc, needle) {
			return doc
		}
	}
	return ""
}

// Regression suite for ka-0000000567 — the residual hand-applied workarounds
// from the 2nd live Gardenio-in-a-Box run, converted into chart/overlay defaults
// so the install is hands-off. Epic gd-Xnq8Ldq2ct. Each test pins one gap.
//
// The in-box overlay normally requires tls.hosts (perHost cert), which the box
// supplies via --set at bootstrap. Tests that don't exercise TLS pass
// tls.mode=disabled (mirroring box_networkpolicy_test.go) to skip that gate.

// ── G2 (ka-0000003741): forwardauth + edge-auth HMAC secrets auto-seeded ──────
//
// The cloud path sources these via ExternalSecrets only; in-box
// (externalSecrets.enabled=false) the chart fail-closed and the Secrets had to
// be created by hand. templates/forwardauth/secrets.yaml now owns them
// (generate-if-missing, helm.sh/resource-policy: keep).

func TestInboxG2_SeedsForwardAuthAndHMACSecrets(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/forwardauth/secrets.yaml",
		"externalSecrets.enabled=false",
		"keycloak.enabled=true",
		"keycloak.forwardAuth.enabled=true",
		"edgeAuth.enabled=true",
		"edgeAuth.dryRun=true",
	)
	if !strings.Contains(out, "name: \"gardenio-forwardauth-secrets\"") {
		t.Error("G2: in-box must self-seed the gardenio-forwardauth-secrets Secret")
	}
	if !strings.Contains(out, "client-secret:") || !strings.Contains(out, "cookie-secret:") {
		t.Error("G2: forwardauth Secret must carry client-secret + cookie-secret keys")
	}
	if !strings.Contains(out, "-edge-auth-hmac\"") {
		t.Error("G2: in-box must self-seed the <release>-edge-auth-hmac Secret")
	}
	if !strings.Contains(out, "hmac-key:") {
		t.Error("G2: edge-auth Secret must carry the hmac-key")
	}
	// Survive helm uninstall / value churn across upgrades.
	if !strings.Contains(out, `"helm.sh/resource-policy": keep`) {
		t.Error("G2: self-seeded Secrets must carry helm.sh/resource-policy: keep")
	}
}

// The cloud path (ExternalSecrets owns the values) must render NOTHING from this
// template — otherwise the chart would fight ESO for ownership of the Secret.
// The template is fully gated off there, so --show-only errors; render the full
// chart and assert the self-seeded Secret names are absent.
func TestInboxG2_NoSeedWhenExternalSecretsEnabled(t *testing.T) {
	t.Parallel()
	out := helmTemplateFull(t, nil,
		"externalSecrets.enabled=true",
		"externalSecrets.secretStoreName=dummy",
		"keycloak.enabled=true",
		"keycloak.forwardAuth.enabled=true",
		"edgeAuth.enabled=true",
		"edgeAuth.dryRun=true",
	)
	// The self-seed template would set resource-policy: keep on these Secret
	// names; the ExternalSecret path does not. Assert neither self-seeded Secret
	// is chart-owned here.
	if doc := sliceDoc(out, "name: \"gardenio-forwardauth-secrets\""); doc != "" &&
		strings.Contains(doc, "kind: Secret") && strings.Contains(doc, "resource-policy") {
		t.Error("G2: forwardauth Secret must not be chart-self-seeded when externalSecrets.enabled=true (ESO owns it)")
	}
	if doc := sliceDoc(out, "-edge-auth-hmac\""); doc != "" &&
		strings.Contains(doc, "kind: Secret") && strings.Contains(doc, "resource-policy") {
		t.Error("G2: edge-auth HMAC Secret must not be chart-self-seeded when externalSecrets.enabled=true (ESO owns it)")
	}
}

// An operator-supplied HMAC Secret (edgeAuth.hmacKey.secretName) must suppress
// only the HMAC seed, not the forwardauth one.
func TestInboxG2_OperatorHMACSecretSuppressesOnlyHMAC(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/forwardauth/secrets.yaml",
		"externalSecrets.enabled=false",
		"keycloak.enabled=true",
		"keycloak.forwardAuth.enabled=true",
		"edgeAuth.enabled=true",
		"edgeAuth.dryRun=true",
		"edgeAuth.hmacKey.secretName=my-own-hmac",
	)
	if !strings.Contains(out, "gardenio-forwardauth-secrets") {
		t.Error("G2: forwardauth Secret must still seed when only the HMAC is operator-supplied")
	}
	if strings.Contains(out, "-edge-auth-hmac\"") {
		t.Error("G2: HMAC Secret must NOT seed when edgeAuth.hmacKey.secretName is set")
	}
}

// ── G8 (ka-0000000554): keycloak-runtime ingress allows the EDP operator ns ──
//
// Without this the EDP keycloak-operator (in the operators ns) cannot reach
// keycloak:8080, so its ClusterKeycloak stays connected:false.

// The allow rule must NOT render when no operator namespace is configured
// (the cloud default — binds to the external ClusterKeycloak).
func TestInboxG8_NoOperatorRuleByDefault(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/networkpolicies/networkpolicies.yaml",
		"networkPolicies.enabled=true",
		"keycloak.enabled=true",
		"keycloak.runtime.enabled=true",
		"edgeAuth.dryRun=true",
	)
	seg := sliceNetPol(out, "keycloak-runtime-ingress")
	if seg != "" && strings.Contains(seg, "name: operators") {
		t.Error("G8: operator-namespace allow must not render without networkPolicies.keycloakOperator.namespaceSelector")
	}
}

// ── G9 (ka-0000001782): forwardauth-gated beads-viewer IngressRoute + netpols ─
//
// The chart's other beads-viewer routes don't cover the box edge, so no gated
// public route rendered and one had to be hand-applied.

func TestInboxG9_ForwardAuthBeadsViewerRoute(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads-viewer/forwardauth-ingressroute.yaml",
		"beadsViewer.enabled=true",
		"beadsViewer.ingress.forwardAuthIngress.enabled=true",
		"beadsViewer.ingress.host=beads-viewer.x.sslip.io",
		"keycloak.enabled=true",
		"keycloak.forwardAuth.enabled=true",
		"edgeAuth.dryRun=true",
	)
	if !strings.Contains(out, "kind: IngressRoute") {
		t.Fatal("G9: forwardauth beads-viewer IngressRoute must render when enabled")
	}
	if !strings.Contains(out, "Host(`beads-viewer.x.sslip.io`)") {
		t.Error("G9: route must match the configured beads-viewer host")
	}
	// The whole point: the in-cluster oauth2-proxy forwardauth middleware gates
	// it (release-prefixed name, e.g. <release>-forwardauth).
	if !strings.Contains(out, "-forwardauth\n") {
		t.Error("G9: route must attach the <release>-forwardauth middleware")
	}
}

// Default OFF: the new route must not render in the base chart. The whole
// template is gated off, so --show-only errors; render the full chart and
// assert the gate label is absent (mirrors edge_middlewares_test.go).
func TestInboxG9_ForwardAuthRouteDefaultOff(t *testing.T) {
	t.Parallel()
	out := helmTemplateFull(t, nil,
		"beadsViewer.enabled=true",
		"beadsViewer.ingress.host=beads-viewer.x.sslip.io",
		"keycloak.enabled=true",
		"keycloak.forwardAuth.enabled=true",
	)
	if strings.Contains(out, "app.kubernetes.io/gate: forwardauth") {
		t.Error("G9: forwardauth beads-viewer route must default OFF (forwardAuthIngress.enabled=false)")
	}
}
