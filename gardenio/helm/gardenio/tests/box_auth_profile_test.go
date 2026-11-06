package tests

import (
	"regexp"
	"strings"
	"testing"
)

// oauth2ProxyMiddlewareRE matches a Traefik middleware list entry that is
// EXACTLY the oauth2-proxy forwardauth gate (<release>-forwardauth) — the `$`
// end-of-line anchor excludes the sibling `-forwardauth-stripprefix` and
// `-forwardauth-errors` middlewares, and the leading `- name:` excludes an
// IngressRoute's own `name:` metadata line.
var oauth2ProxyMiddlewareRE = regexp.MustCompile(`(?m)^\s*- name: \S+-forwardauth$`)

// Box auth profile (ka-0000006087, doc §6): migrate the human + CLI edge off
// the GitLab-direct OIDC / AVA cloud paths onto the in-box Keycloak. These
// tests pin the chart wiring deploy-values/gardenio-inbox.yaml relies on:
//
//   1. forwardAuth issuer auto-derivation: with provider=oidc and an EMPTY
//      oidcIssuerUrl, the in-box runtime's realm issuer is derived from
//      runtime.host (so the overlay need not hand-copy it).
//   2. The cloud path is untouched: an explicit oidcIssuerUrl still wins.
//   3. The fail-fast still fires when nothing resolves to an https issuer.
//   4. edge-auth box profile: patDisabled omits the GitLab env + emits
//      EDGE_AUTH_PAT_DISABLED, while cookieAuth wires the oauth2-proxy subreq.

// 1. forwardAuth issuer auto-derives from the in-box runtime when empty.
func TestBoxAuth_ForwardAuthIssuerAutoDerives(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/forwardauth/deployment.yaml",
		"keycloak.enabled=true",
		"keycloak.forwardAuth.enabled=true",
		"keycloak.forwardAuth.provider=oidc",
		"keycloak.forwardAuth.oidcIssuerUrl=",
		"keycloak.runtime.enabled=true",
		"keycloak.runtime.host=auth.gardenio.app.e2e.dev.acme.example",
		"keycloak.realm.name=gardenio-inbox",
	)
	want := "--oidc-issuer-url=https://auth.gardenio.app.e2e.dev.acme.example/auth/realms/gardenio-inbox"
	if !strings.Contains(out, want) {
		t.Errorf("forwardAuth issuer should auto-derive to %q, not found in:\n%s", want, out)
	}
	if !strings.Contains(out, "--provider=oidc") {
		t.Error("forwardAuth provider should be oidc in the box profile")
	}
	// The issuer host must also be whitelisted for sign_out redirects.
	if !strings.Contains(out, "--whitelist-domain=auth.gardenio.app.e2e.dev.acme.example") {
		t.Error("derived issuer host should be whitelisted for sign_out")
	}
}

//  2. An explicit issuer (the cloud gitlab.com / external-Keycloak path) wins
//     over auto-derivation even when the in-box runtime is also enabled.
func TestBoxAuth_ExplicitIssuerWins(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/forwardauth/deployment.yaml",
		"keycloak.enabled=true",
		"keycloak.forwardAuth.enabled=true",
		"keycloak.forwardAuth.provider=gitlab",
		"keycloak.forwardAuth.oidcIssuerUrl=https://gitlab.com",
		"keycloak.runtime.enabled=true",
		"keycloak.runtime.host=auth.gardenio.app.e2e.dev.acme.example",
		"keycloak.realm.name=gardenio-inbox",
	)
	if !strings.Contains(out, "--oidc-issuer-url=https://gitlab.com") {
		t.Error("explicit forwardAuth.oidcIssuerUrl must win over runtime auto-derivation")
	}
	if strings.Contains(out, "/realms/gardenio-inbox") {
		t.Error("explicit issuer set: must NOT auto-derive the realm issuer")
	}
}

//  3. Empty issuer with no in-box runtime must fail fast (no plaintext / empty
//     issuer ever reaches oauth2-proxy).
func TestBoxAuth_NoIssuerFailsFast(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "templates/forwardauth/deployment.yaml",
		"keycloak.enabled=true",
		"keycloak.forwardAuth.enabled=true",
		"keycloak.forwardAuth.oidcIssuerUrl=",
		"keycloak.runtime.enabled=false",
	)
	if err == nil {
		t.Fatalf("expected fail-fast when no issuer resolves, got render:\n%s", out)
	}
	if !strings.Contains(out, "must resolve to an https:// issuer") {
		t.Errorf("expected the https issuer fail-fast message, got: %v\n%s", err, out)
	}
}

//  4. edge-auth box profile: patDisabled drops the GitLab PAT env and sets the
//     disable flag; cookieAuth wires the oauth2-proxy subrequest endpoint.
func TestBoxAuth_EdgeAuthPATDisabledCookieOn(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/edge-auth/deployment.yaml",
		"edgeAuth.enabled=true",
		"edgeAuth.host=cli.gardenio.app.e2e.dev.acme.example",
		"edgeAuth.hmacKey.secretName=gardenio-edge-auth-hmac",
		"edgeAuth.patDisabled=true",
		"edgeAuth.ava.trustSigner=",
		"edgeAuth.cookieAuth.enabled=true",
		"edgeAuth.cookieAuth.cookieName=_gardenio_oauth2",
	)
	if !strings.Contains(out, "name: EDGE_AUTH_PAT_DISABLED") {
		t.Error("patDisabled: EDGE_AUTH_PAT_DISABLED env must be set")
	}
	// The GitLab PAT envs must be omitted so no gitlab.com calls are possible.
	// Match the env declarations specifically (the explanatory comment names
	// them too).
	if strings.Contains(out, "name: EDGE_AUTH_GITLAB_GROUP") || strings.Contains(out, "name: GITLAB_BASE_URL") {
		t.Error("patDisabled: GitLab PAT env vars must be omitted from the pod")
	}
	// AVA off: no AVA trust-signer env.
	if strings.Contains(out, "name: EDGE_AUTH_AVA_TRUST_SIGNER") {
		t.Error("box profile: AVA must be off (no trust-signer env)")
	}
	// Cookie auth wired to the in-cluster oauth2-proxy.
	if !strings.Contains(out, "name: EDGE_AUTH_OAUTH2_PROXY_AUTH_URL") {
		t.Error("cookieAuth: oauth2-proxy auth URL env must be set")
	}
	if !strings.Contains(out, `value: "_gardenio_oauth2"`) {
		t.Error("cookieAuth: cookie name must be _gardenio_oauth2")
	}
	if !strings.Contains(out, "name: EDGE_AUTH_OAUTH2_PROXY_ROLE_SOURCE\n              value: \"provider-groups\"") {
		t.Error("cookieAuth: default role source must require exact provider group paths")
	}
}

// Default edge-auth (patDisabled false) keeps the cloud PAT env — back-compat.
func TestBoxAuth_EdgeAuthDefaultKeepsPATEnv(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/edge-auth/deployment.yaml",
		"edgeAuth.enabled=true",
		"edgeAuth.host=cli.test",
		"edgeAuth.hmacKey.secretName=gardenio-edge-auth-hmac",
	)
	if !strings.Contains(out, "name: EDGE_AUTH_GITLAB_GROUP") || !strings.Contains(out, "name: GITLAB_BASE_URL") {
		t.Error("default edge-auth must keep the GitLab PAT env (cloud back-compat)")
	}
	if strings.Contains(out, "name: EDGE_AUTH_PAT_DISABLED") {
		t.Error("default edge-auth must NOT set EDGE_AUTH_PAT_DISABLED")
	}
}

// ---------------------------------------------------------------------------
// P3.1 edge hardening (ka-0000006983; ingress research §G1/§G2/§8,
// ka-0000002581). The oauth2-proxy forwardAuth must front EVERY browser route,
// X-Forwarded-* must not be trusted on the directly-internet-facing box, and
// no AVA carve-out route may render.
// ---------------------------------------------------------------------------

//  5. G2: the forwardauth middleware trusts X-Forwarded-* by default (the
//     ALB-fronted prod/staging behaviour).
func TestBoxAuth_ForwardAuthTrustHeaderDefaultsTrue(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/forwardauth/middleware.yaml",
		"traefik.enabled=true",
		"keycloak.enabled=true",
		"keycloak.forwardAuth.enabled=true",
	)
	if !strings.Contains(out, "trustForwardHeader: true") {
		t.Errorf("forwardauth middleware should default trustForwardHeader: true, got:\n%s", out)
	}
}

//  6. G2: the box flips trustForwardHeader off so a spoofed X-Forwarded-* cannot
//     feed the auth decision (Traefik is directly internet-facing, no front-proxy).
func TestBoxAuth_ForwardAuthTrustHeaderOff(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/forwardauth/middleware.yaml",
		"traefik.enabled=true",
		"keycloak.enabled=true",
		"keycloak.forwardAuth.enabled=true",
		"keycloak.forwardAuth.trustForwardHeader=false",
	)
	if !strings.Contains(out, "trustForwardHeader: false") {
		t.Errorf("trustForwardHeader=false must render literally false, got:\n%s", out)
	}
}

//  11. PROD-SAFETY: with the base values only, NO KeycloakClientScope renders —
//     keycloak.clientScopes is empty by default, so the prod path (gitlab
//     provider) is untouched and the fix is additive.
func TestBoxAuth_BaseRendersNoClientScope(t *testing.T) {
	t.Parallel()
	out := helmTemplateFull(t, nil,
		"keycloak.enabled=true",
	)
	if strings.Contains(out, "kind: KeycloakClientScope") {
		t.Errorf("base values must NOT render any KeycloakClientScope (prod no-op), got:\n%s", out)
	}
}

// keycloakClientBlock returns the rendered spec of the KeycloakClient whose
// clientId is clientID (from a full multi-doc `helm template` output), or ""
// if not found. It slices from the client's `clientId:` line to the next `---`
// document separator so per-client field assertions don't bleed across clients.
func keycloakClientBlock(out, clientID string) string {
	marker := "clientId: \"" + clientID + "\""
	i := strings.Index(out, marker)
	if i < 0 {
		return ""
	}
	rest := out[i:]
	if j := strings.Index(rest, "\n---"); j >= 0 {
		return rest[:j]
	}
	return rest
}

//  13. PROD-SAFETY for the attach step: with keycloak.clientScopes empty (base +
//     every cloud overlay), NO client gets a chart-injected defaultClientScopes.
//     A client keeps ONLY what it declares itself, so the gitlab-provider front
//     doors are untouched and the fix stays strictly in-box.
func TestBoxAuth_BaseInjectsNoDefaultClientScopes(t *testing.T) {
	t.Parallel()
	out := helmTemplateFull(t, nil,
		"keycloak.enabled=true",
	)
	fa := keycloakClientBlock(out, "gardenio-traefik-forwardauth")
	if fa == "" {
		t.Fatal("base render must still render the gardenio-traefik-forwardauth KeycloakClient")
	}
	if strings.Contains(fa, "- groups") {
		t.Errorf("base render must NOT inject a `groups` scope onto any client (prod no-op); got:\n%s", fa)
	}
}

// ---------------------------------------------------------------------------
// Per-box auth host wiring (ka-0000000068). The base in-box overlay
// deploy-values/gardenio-inbox.yaml pins the auth host / cookie domain / callback
// to the CLOUD placeholder auth.gardenio.app.e2e.dev.acme.example — a real per-env
// value the operator overrides at install. On a from-scratch tarboat BYO sweep
// box nothing overrides it, so oauth2-proxy did OIDC discovery against the cloud
// host (which public DNS resolves to a foreign 404) and crash-looped, blocking
// helm --wait + the login gate. These tests pin the two halves of the fix:
//   A. a per-box host injection (as tarboat-box-remediate.sh now writes)
//      repoints issuer/callback/cookie at the box apex, and
//   B. the forwardauth KeycloakClient always whitelists the EXACT callback
//      oauth2-proxy redeems (derived from forwardAuth.ingress.host) so Keycloak
//      does not reject the code exchange after the host is fixed.
// ---------------------------------------------------------------------------

// 12B. The forwardauth KeycloakClient whitelists the callback oauth2-proxy
//
//	actually redeems on (derived from forwardAuth.ingress.host) so the code
//	exchange is not rejected with "Invalid parameter: redirect_uri".
func TestBoxAuth_ForwardAuthClientWhitelistsDerivedCallback(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/keycloak/clients.yaml",
		"keycloak.enabled=true",
		"keycloak.forwardAuth.enabled=true",
		"keycloak.forwardAuth.ingress.enabled=true",
		"keycloak.forwardAuth.ingress.host=1-2-3-4.sslip.io",
	)
	if !strings.Contains(out, "https://1-2-3-4.sslip.io/oauth2/callback") {
		t.Errorf("forwardauth client must whitelist the derived box callback, got:\n%s", out)
	}
}
