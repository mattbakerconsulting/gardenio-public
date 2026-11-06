package tests

import (
	"strings"
	"testing"
)

// T2 (gd-YA22Xm2j6m, epic ka-0000004118): coopmux's JWT verifier issuer/audience
// (COOP_MUX_OIDC_*) must be flippable from the Keycloak realm broker to a
// non-Keycloak OIDC provider (GitLab) via a values override, WITHOUT breaking
// the historical Keycloak-derived wiring for in-box + ephemeral CI installs.

// TestCoopmuxOIDC_GitLabOverrideWins verifies an explicit coopmux.oidc.issuerUrl
// override drives BOTH env vars — even when Keycloak is still enabled (the
// cutover window). This proves we switched issuers, not merely added one:
// a Keycloak-issued JWT now fails the verifier's issuer check.
func TestCoopmuxOIDC_GitLabOverrideWins(t *testing.T) {
	t.Parallel()
	out := helmTemplateFull(t, nil,
		"coopmux.enabled=true",
		"keycloak.enabled=true",
		"keycloak.runtime.enabled=true",
		"keycloak.runtime.host=auth.example.com",
		"keycloak.realm.name=myrealm",
		"coopmux.oidc.issuerUrl=https://gitlab.com",
		"coopmux.oidc.clientId=57ab1f8b",
	)
	if !issuerValueAfter(out, "COOP_MUX_OIDC_ISSUER_URL", "https://gitlab.com") {
		t.Errorf("override: COOP_MUX_OIDC_ISSUER_URL should be the GitLab issuer, not the Keycloak realm")
	}
	if !issuerValueAfter(out, "COOP_MUX_OIDC_CLIENT_ID", "57ab1f8b") {
		t.Errorf("override: COOP_MUX_OIDC_CLIENT_ID should be the GitLab OAuth app client id")
	}
	// The Keycloak realm issuer must NOT leak through when the override is set.
	if strings.Contains(out, "auth.example.com/auth/realms/myrealm") &&
		issuerValueAfter(out, "COOP_MUX_OIDC_ISSUER_URL", "auth.example.com") {
		t.Errorf("override: Keycloak realm issuer must not be emitted for COOP_MUX_OIDC_ISSUER_URL")
	}
}

// TestCoopmuxOIDC_KeycloakFallback verifies the default (override empty) path is
// unchanged: coopmux still derives issuer from the Keycloak runtime and client
// id from the coopmux-api client. This is the in-box + ephemeral-CI contract.
func TestCoopmuxOIDC_KeycloakFallback(t *testing.T) {
	t.Parallel()
	out := helmTemplateFull(t, nil,
		"coopmux.enabled=true",
		"keycloak.enabled=true",
		"keycloak.runtime.enabled=true",
		"keycloak.runtime.host=auth.example.com",
		"keycloak.realm.name=myrealm",
		"keycloak.clients[0].name=coopmux-api",
		"keycloak.clients[0].clientId=gardenio-coopmux",
	)
	if !issuerValueAfter(out, "COOP_MUX_OIDC_ISSUER_URL", "https://auth.example.com/auth/realms/myrealm") {
		t.Errorf("fallback: expected Keycloak-derived issuer for COOP_MUX_OIDC_ISSUER_URL")
	}
	if !issuerValueAfter(out, "COOP_MUX_OIDC_CLIENT_ID", "gardenio-coopmux") {
		t.Errorf("fallback: expected coopmux-api clientId for COOP_MUX_OIDC_CLIENT_ID")
	}
}

// TestCoopmuxOIDC_OverrideIssuerOnlyOmitsClientID guards a partial override: if
// only issuerUrl is set (no clientId), the issuer is emitted and no stale
// Keycloak client id leaks in — the verifier would then have no audience, which
// is a loud misconfig rather than a silent accept of the wrong audience.
func TestCoopmuxOIDC_OverrideIssuerOnlyOmitsClientID(t *testing.T) {
	t.Parallel()
	out := helmTemplateFull(t, nil,
		"coopmux.enabled=true",
		"keycloak.enabled=true",
		"keycloak.runtime.enabled=true",
		"keycloak.runtime.host=auth.example.com",
		"keycloak.realm.name=myrealm",
		"keycloak.clients[0].name=coopmux-api",
		"keycloak.clients[0].clientId=gardenio-coopmux",
		"coopmux.oidc.issuerUrl=https://gitlab.com",
	)
	if !issuerValueAfter(out, "COOP_MUX_OIDC_ISSUER_URL", "https://gitlab.com") {
		t.Errorf("issuer-only override: expected GitLab issuer")
	}
	if issuerValueAfter(out, "COOP_MUX_OIDC_CLIENT_ID", "gardenio-coopmux") {
		t.Errorf("issuer-only override: Keycloak coopmux-api clientId must not leak when override is active")
	}
}
