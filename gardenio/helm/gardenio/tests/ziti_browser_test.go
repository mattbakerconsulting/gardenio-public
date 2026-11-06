package tests

import (
	"strings"
	"testing"
)

func zitiBrowserFlags() []string {
	return []string{
		"traefik.enabled=true",
		"traefik.ingressRoutes.enabled=true",
		"zitiBrowser.enabled=true",
		"zitiBrowser.host=prod.gardenio.acme.example",
		"zitiBrowser.oauth2Proxy.issuerURL=https://login.microsoftonline.com/tenant/v2.0",
		"zitiBrowser.oauth2Proxy.allowedRoles={gardenio-viewer,gardenio-operator,gardenio-admin}",
		"edgeAuth.enabled=true",
		"edgeAuth.dryRun=true",
		"beadsViewer.enabled=true",
		"coopmux.enabled=true",
	}
}

func zitiBrowserEntraFlags() []string {
	return append(zitiBrowserFlags(),
		"edgeAuth.entra.enabled=true",
		"edgeAuth.entra.issuerURL=https://login.microsoftonline.com/tenant/v2.0",
		"edgeAuth.entra.audience=gardenio-api-client",
		"edgeAuth.entra.tenantID=tenant",
		"edgeAuth.entra.requiredScope=user_impersonation",
		"edgeAuth.entra.requiredRole=gardenio-viewer",
		"edgeAuth.entra.allowedClientIDs={piid-client}",
	)
}

func TestZitiBrowser_IngressUsesOnlyPrivateEntrypoint(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/ziti-browser/ingressroute.yaml", zitiBrowserFlags()...)
	if !strings.Contains(out, "entryPoints:\n    - ziti") {
		t.Fatalf("private route must bind the ziti entrypoint:\n%s", out)
	}
	if strings.Contains(out, "- web\n") || strings.Contains(out, "- websecure\n") {
		t.Fatalf("private route must not bind public entrypoints:\n%s", out)
	}
	for _, path := range []string{"/oauth2/", "/beads", "/mux", "/kd"} {
		if !strings.Contains(out, path) {
			t.Errorf("private route missing %s", path)
		}
	}
}

func TestZitiBrowser_BeadsGetsIDTokenButShimDoesNot(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/ziti-browser/ingressroute.yaml", zitiBrowserFlags()...)
	clear := zitiMiddlewareDoc(t, out, "test-ziti-clear-auth")
	for _, header := range []string{
		`Authorization: ""`,
		`X-Auth-Request-Email: ""`,
		`X-Auth-Request-Access-Token: ""`,
	} {
		if !strings.Contains(clear, header) {
			t.Errorf("private route must clear forged identity header %q before auth:\n%s", header, clear)
		}
	}
	beads := zitiMiddlewareDoc(t, out, "test-ziti-forwardauth-beads")
	if !strings.Contains(beads, "- Authorization") {
		t.Fatalf("beads ForwardAuth must copy the signed Entra ID token:\n%s", beads)
	}
	session := zitiMiddlewareDoc(t, out, "test-ziti-forwardauth-session")
	if strings.Contains(session, "- Authorization") {
		t.Fatalf("mux/kd ForwardAuth must not inject Authorization into edge-auth:\n%s", session)
	}
	if strings.Contains(session, "- X-Auth-Request-Access-Token") {
		t.Fatalf("mux/kd ForwardAuth must not expose the Entra access token to edge-auth:\n%s", session)
	}
	for name, doc := range map[string]string{"beads": beads, "session": session} {
		if !strings.Contains(doc, "addAuthCookiesToResponse:\n      - _gardenio_ziti_oauth2") {
			t.Errorf("%s ForwardAuth must return the configured session cookie to the browser:\n%s", name, doc)
		}
	}
}

func TestZitiBrowser_DelegatedBearerRoutesPreserveTokenAndOutrankBrowser(t *testing.T) {
	t.Parallel()
	// Pin --namespace=default: the edge-auth ForwardAuth address below is
	// derived from .Release.Namespace, so an un-pinned render inherits the
	// ambient namespace (kube context on a laptop, in-cluster SA namespace in
	// an agent pod) and the ".default." assertion only passes where the
	// ambient namespace happens to be "default" (gd-8jT1GBQXuE / ka-0000003495).
	out := helmTemplateNS(t, "default", "templates/ziti-browser/ingressroute.yaml", zitiBrowserEntraFlags()...)

	if got := strings.Count(out, "HeaderRegexp(`Authorization`, `(?i)^Bearer[[:space:]]+.+$`)"); got != 3 {
		t.Fatalf("delegated bearer selector count=%d, want 3 for /beads, /mux, and /kd:\n%s", got, out)
	}
	if got := strings.Count(out, "priority: 250"); got != 3 {
		t.Fatalf("delegated bearer priority count=%d, want 3:\n%s", got, out)
	}
	if got := strings.Count(out, "priority: 200"); got != 3 {
		t.Fatalf("browser-session priority count=%d, want existing 3 routes:\n%s", got, out)
	}

	scrub := zitiMiddlewareDoc(t, out, "test-ziti-clear-spoofed-auth")
	if strings.Contains(scrub, `Authorization: ""`) {
		t.Fatalf("delegated-token middleware must preserve Authorization:\n%s", scrub)
	}
	for _, header := range []string{
		`X-Auth-Request-User: ""`,
		`X-Auth-Request-Email: ""`,
		`X-Auth-Request-Groups: ""`,
		`X-Auth-Request-Access-Token: ""`,
	} {
		if !strings.Contains(scrub, header) {
			t.Errorf("delegated-token middleware must scrub %s:\n%s", header, scrub)
		}
	}

	forwardauth := zitiMiddlewareDoc(t, out, "test-ziti-forwardauth-entra")
	if !strings.Contains(forwardauth, "address: http://test-edge-auth.default.svc.cluster.local:8096/forwardauth/entra") {
		t.Fatalf("beads bearer route must use the strict edge-auth Entra gate:\n%s", forwardauth)
	}
	if strings.Contains(forwardauth, "authResponseHeaders:") {
		t.Fatalf("Entra ForwardAuth must not replace the original Authorization header:\n%s", forwardauth)
	}

	// The legacy browser route is deliberately unchanged and continues to
	// clear a caller-supplied Authorization header before oauth2-proxy.
	clear := zitiMiddlewareDoc(t, out, "test-ziti-clear-auth")
	if !strings.Contains(clear, `Authorization: ""`) {
		t.Fatalf("browser route no longer clears caller Authorization:\n%s", clear)
	}
}

func TestZitiBrowser_DelegatedBearerRoutesStayDarkWhenEntraDisabled(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/ziti-browser/ingressroute.yaml", zitiBrowserFlags()...)
	for _, unexpected := range []string{"HeaderRegexp(`Authorization`", "ziti-clear-spoofed-auth", "ziti-forwardauth-entra"} {
		if strings.Contains(out, unexpected) {
			t.Errorf("Entra-disabled route rendered %q:\n%s", unexpected, out)
		}
	}
}

func TestEdgeAuth_EntraDeploymentContract(t *testing.T) {
	t.Parallel()
	flags := append(zitiBrowserEntraFlags(),
		"edgeAuth.dryRun=false",
		"edgeAuth.host=cli.gardenio.acme.example",
		"edgeAuth.hmacKey.secretName=test-edge-auth-hmac",
	)
	out := helmTemplate(t, "templates/edge-auth/deployment.yaml", flags...)
	for _, want := range []string{
		"name: EDGE_AUTH_ENTRA_ISSUER_URL\n              value: \"https://login.microsoftonline.com/tenant/v2.0\"",
		"name: EDGE_AUTH_ENTRA_AUDIENCE\n              value: \"gardenio-api-client\"",
		"name: EDGE_AUTH_ENTRA_TENANT_ID\n              value: \"tenant\"",
		"name: EDGE_AUTH_ENTRA_REQUIRED_SCOPE\n              value: \"user_impersonation\"",
		"name: EDGE_AUTH_ENTRA_REQUIRED_ROLE\n              value: \"gardenio-viewer\"",
		"name: EDGE_AUTH_ENTRA_ALLOWED_CLIENT_IDS\n              value: \"piid-client\"",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("edge-auth Entra deployment missing %q:\n%s", want, out)
		}
	}
}

func TestEdgeAuth_EntraDeploymentFailsWithoutAuthorizedClient(t *testing.T) {
	t.Parallel()
	flags := append(zitiBrowserFlags(),
		"edgeAuth.dryRun=false",
		"edgeAuth.host=cli.gardenio.acme.example",
		"edgeAuth.hmacKey.secretName=test-edge-auth-hmac",
		"edgeAuth.entra.enabled=true",
		"edgeAuth.entra.issuerURL=https://login.microsoftonline.com/tenant/v2.0",
		"edgeAuth.entra.audience=gardenio-api-client",
		"edgeAuth.entra.tenantID=tenant",
	)
	out, err := helmTemplateAllowFail(t, "templates/edge-auth/deployment.yaml", flags...)
	if err == nil {
		t.Fatalf("expected Entra deployment without allowedClientIDs to fail closed:\n%s", out)
	}
	if !strings.Contains(out, "edgeAuth.entra.allowedClientIDs is empty") {
		t.Fatalf("unexpected Entra fail-closed error:\n%s", out)
	}
}

func TestEdgeAuth_ZitiCookieUsesExplicitEntraAppRoles(t *testing.T) {
	t.Parallel()
	flags := append(zitiBrowserFlags(),
		"edgeAuth.dryRun=false",
		"edgeAuth.host=cli.gardenio.acme.example",
		"edgeAuth.hmacKey.secretName=test-edge-auth-hmac",
		"edgeAuth.cookieAuth.enabled=true",
		"edgeAuth.cookieAuth.authURL=http://gardenio-ziti-forwardauth:4180/oauth2/auth",
		"edgeAuth.cookieAuth.cookieName=_gardenio_ziti_oauth2",
		"edgeAuth.cookieAuth.roleSource=entra-app-roles",
	)
	out := helmTemplate(t, "templates/edge-auth/deployment.yaml", flags...)
	for _, want := range []string{
		"name: EDGE_AUTH_OAUTH2_PROXY_AUTH_URL\n              value: \"http://gardenio-ziti-forwardauth:4180/oauth2/auth\"",
		"name: EDGE_AUTH_OAUTH2_PROXY_COOKIE_NAME\n              value: \"_gardenio_ziti_oauth2\"",
		"name: EDGE_AUTH_OAUTH2_PROXY_ROLE_SOURCE\n              value: \"entra-app-roles\"",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("edge-auth Ziti cookie deployment missing %q:\n%s", want, out)
		}
	}
}

func TestEdgeAuth_CookieRoleSourceRejectsUnknownContract(t *testing.T) {
	t.Parallel()
	flags := append(zitiBrowserFlags(),
		"edgeAuth.dryRun=false",
		"edgeAuth.host=cli.gardenio.acme.example",
		"edgeAuth.hmacKey.secretName=test-edge-auth-hmac",
		"edgeAuth.cookieAuth.enabled=true",
		"edgeAuth.cookieAuth.roleSource=leaf-name",
	)
	out, err := helmTemplateAllowFail(t, "templates/edge-auth/deployment.yaml", flags...)
	if err == nil {
		t.Fatalf("expected unknown cookie role source to fail closed:\n%s", out)
	}
	if !strings.Contains(out, "edgeAuth.cookieAuth.roleSource must be provider-groups or entra-app-roles") {
		t.Fatalf("unexpected cookie role-source validation error:\n%s", out)
	}
}

func TestZitiBrowser_NetworkPoliciesPermitOnlyRequiredAuthPath(t *testing.T) {
	t.Parallel()
	flags := append(zitiBrowserFlags(),
		"networkPolicies.enabled=true",
		"networkPolicies.defaultDenyEgress=true",
	)

	ingress := helmTemplate(t, "templates/networkpolicies/networkpolicies.yaml", flags...)
	for _, want := range []string{
		"name: test-ziti-forwardauth-ingress",
		"app.kubernetes.io/component: edge-auth",
		"port: 4180",
		"name: test-edge-auth-egress",
		"app.kubernetes.io/name: gardenio-ziti-forwardauth",
	} {
		if !strings.Contains(ingress, want) {
			t.Errorf("private auth NetworkPolicy missing %q:\n%s", want, ingress)
		}
	}

	egress := helmTemplate(t, "templates/networkpolicies/default-deny-egress.yaml", flags...)
	for _, want := range []string{
		"name: test-ziti-forwardauth-egress",
		"app.kubernetes.io/name: gardenio-ziti-forwardauth",
		"port: 443",
	} {
		if !strings.Contains(egress, want) {
			t.Errorf("oauth2-proxy egress policy missing %q:\n%s", want, egress)
		}
	}
}

func zitiMiddlewareDoc(t *testing.T, out, name string) string {
	t.Helper()
	for _, doc := range strings.Split(out, "---") {
		if strings.Contains(doc, "kind: Middleware") && strings.Contains(doc, "name: "+name) {
			return doc
		}
	}
	t.Fatalf("middleware %s did not render:\n%s", name, out)
	return ""
}

func TestZitiBrowser_OAuth2ProxyEnforcesRoles(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/ziti-browser/deployment.yaml", zitiBrowserFlags()...)
	for _, role := range []string{"gardenio-viewer", "gardenio-operator", "gardenio-admin"} {
		if !strings.Contains(out, "--allowed-group="+role) {
			t.Errorf("oauth2-proxy missing allowed role %s", role)
		}
	}
	if !strings.Contains(out, "--oidc-groups-claim=roles") {
		t.Error("oauth2-proxy must read Entra app roles from the roles claim")
	}
	if !strings.Contains(out, "--set-authorization-header=true") {
		t.Error("oauth2-proxy must expose the signed ID token to the beads-only middleware")
	}
	if !strings.Contains(out, "--cookie-expire=50m") {
		t.Error("oauth2-proxy cookie must expire before the non-refreshing Entra ID token")
	}
	if strings.Contains(out, "--cookie-refresh=") {
		t.Error("oauth2-proxy must not advertise refresh without the offline_access scope")
	}
}

func TestZitiBrowser_ExternalSecretsUseSeparateSources(t *testing.T) {
	t.Parallel()
	flags := append(zitiBrowserFlags(),
		"externalSecrets.enabled=true",
		"externalSecrets.secretStoreName=aws-secrets-manager",
		"externalSecrets.zitiBrowser.client.remoteRef=gardenio/prod/beads-viewer/oauth-client",
		"externalSecrets.zitiBrowser.cookie.remoteRef=gardenio/prod/beads-viewer/oauth-cookie-secret",
	)
	out := helmTemplate(t, "templates/ziti-browser/external-secrets.yaml", flags...)
	for _, want := range []string{
		`key: "gardenio/prod/beads-viewer/oauth-client"`,
		`property: "client_id"`,
		`property: "client_secret"`,
		`key: "gardenio/prod/beads-viewer/oauth-cookie-secret"`,
		`property: "cookie_secret"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing ExternalSecret contract %q:\n%s", want, out)
		}
	}
}
