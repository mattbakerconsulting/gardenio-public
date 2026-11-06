package tests

import (
	"strings"
	"testing"
)

// Plan ziti-to-keycloak S2a (gd-hRltljHGon): the oauth2-proxy behind the
// public edge serves many hosts with HOST-ONLY cookies. An empty
// keycloak.forwardAuth.cookieDomain must drop --cookie-domain and the
// domain-wide --whitelist-domain=.<domain>; whitelistDomains adds exact hosts.

const forwardAuthDeployment = "templates/forwardauth/deployment.yaml"

func forwardAuthBase() []string {
	return []string{
		"keycloak.enabled=true",
		"keycloak.forwardAuth.enabled=true",
		"keycloak.forwardAuth.oidcIssuerUrl=https://auth.example.test/auth/realms/gardenio",
	}
}

// Positive control: the chart default (domain-wide cookie) is unchanged.
func TestForwardAuth_DefaultCookieDomainUnchanged(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, forwardAuthDeployment, forwardAuthBase()...)
	for _, want := range []string{
		"- --cookie-domain=.example.com",
		"- --whitelist-domain=.example.com",
		"- --whitelist-domain=auth.example.test",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("default render must contain %q:\n%s", want, out)
		}
	}
}

func TestForwardAuth_EmptyCookieDomainIsHostOnly(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, forwardAuthDeployment,
		append(forwardAuthBase(), `keycloak.forwardAuth.cookieDomain=`)...)
	if strings.Contains(out, "--cookie-domain") {
		t.Errorf("empty cookieDomain must render no --cookie-domain:\n%s", out)
	}
	for line := range strings.SplitSeq(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "- --whitelist-domain=.") {
			t.Errorf("empty cookieDomain must not render a domain-wide --whitelist-domain=. flag: %q", line)
		}
	}
	// The issuer host stays whitelisted so sign_out can reach Keycloak.
	if !strings.Contains(out, "- --whitelist-domain=auth.example.test") {
		t.Errorf("issuer host must stay whitelisted:\n%s", out)
	}
}

func TestForwardAuth_WhitelistDomainsRendered(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, forwardAuthDeployment,
		append(forwardAuthBase(),
			"keycloak.forwardAuth.cookieDomain=",
			"keycloak.forwardAuth.whitelistDomains={grafana.clonk.tv,gardenio.clonk.tv}",
		)...)
	for _, want := range []string{
		"- --whitelist-domain=grafana.clonk.tv",
		"- --whitelist-domain=gardenio.clonk.tv",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

// Without ingress.enabled there is no --redirect-url, so oauth2-proxy derives
// each host's callback from X-Forwarded-Host. Keep it that way.
func TestForwardAuth_NoRedirectURLWithoutIngress(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, forwardAuthDeployment,
		append(forwardAuthBase(), "keycloak.forwardAuth.ingress.enabled=false")...)
	if strings.Contains(out, "--redirect-url") {
		t.Errorf("--redirect-url must not render when ingress.enabled=false:\n%s", out)
	}
}
