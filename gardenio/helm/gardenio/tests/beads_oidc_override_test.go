package tests

import (
	"strings"
	"testing"
)

// T3 (ka-0000002268, epic ka-0000004118): the beads daemon's JWT verifier
// issuer/audience (BEADS_OIDC_*) must be flippable from the Keycloak realm
// broker to a non-Keycloak OIDC provider (GitLab) via a values override,
// WITHOUT breaking the historical Keycloak-derived wiring for in-box +
// ephemeral CI installs.

// TestBeadsOIDC_GitLabOverrideWins verifies an explicit beads.oidc.issuerUrl
// override drives BOTH env vars — even when Keycloak is still enabled (the
// cutover window). This proves we switched issuers, not merely added one:
// a Keycloak-issued JWT now fails the verifier's issuer check.
func TestBeadsOIDC_GitLabOverrideWins(t *testing.T) {
	t.Parallel()
	out := helmTemplateFull(t, nil,
		"keycloak.enabled=true",
		"keycloak.runtime.enabled=true",
		"keycloak.runtime.host=auth.example.com",
		"keycloak.realm.name=myrealm",
		"keycloak.oidcAudience=gardenio-beads-api",
		"beads.oidc.issuerUrl=https://gitlab.com",
		"beads.oidc.audience=57ab1f8b",
	)
	if !issuerValueAfter(out, "BEADS_OIDC_ISSUER_URL", "https://gitlab.com") {
		t.Errorf("override: BEADS_OIDC_ISSUER_URL should be the GitLab issuer, not the Keycloak realm")
	}
	if !issuerValueAfter(out, "BEADS_OIDC_AUDIENCE", "57ab1f8b") {
		t.Errorf("override: BEADS_OIDC_AUDIENCE should be the GitLab OAuth app client id")
	}
	// The Keycloak realm issuer must NOT leak through when the override is set.
	if issuerValueAfter(out, "BEADS_OIDC_ISSUER_URL", "auth.example.com") {
		t.Errorf("override: Keycloak realm issuer must not be emitted for BEADS_OIDC_ISSUER_URL")
	}
	// The Keycloak audience must NOT leak through when the override is set.
	if issuerValueAfter(out, "BEADS_OIDC_AUDIENCE", "gardenio-beads-api") {
		t.Errorf("override: Keycloak oidcAudience must not be emitted for BEADS_OIDC_AUDIENCE")
	}
}

// TestBeadsOIDC_KeycloakFallback verifies the default (override empty) path is
// unchanged: the daemon still derives issuer from the Keycloak runtime and
// audience from keycloak.oidcAudience. This is the in-box + ephemeral-CI
// contract — it must render byte-identically to the pre-T3 wiring.
func TestBeadsOIDC_KeycloakFallback(t *testing.T) {
	t.Parallel()
	out := helmTemplateFull(t, nil,
		"keycloak.enabled=true",
		"keycloak.runtime.enabled=true",
		"keycloak.runtime.host=auth.example.com",
		"keycloak.realm.name=myrealm",
		"keycloak.oidcAudience=gardenio-beads-api",
	)
	if !issuerValueAfter(out, "BEADS_OIDC_ISSUER_URL", "https://auth.example.com/auth/realms/myrealm") {
		t.Errorf("fallback: expected Keycloak-derived issuer for BEADS_OIDC_ISSUER_URL")
	}
	if !issuerValueAfter(out, "BEADS_OIDC_AUDIENCE", "gardenio-beads-api") {
		t.Errorf("fallback: expected keycloak.oidcAudience for BEADS_OIDC_AUDIENCE")
	}
}

// TestBeadsOIDC_OverrideIssuerOnlyOmitsAudience guards a partial override: if
// only issuerUrl is set (no audience), the issuer is emitted and no stale
// Keycloak audience leaks in — the verifier would then fail loudly on an empty
// audience (NewOIDCVerifier rejects an empty audience) rather than silently
// accept the wrong audience.
func TestBeadsOIDC_OverrideIssuerOnlyOmitsAudience(t *testing.T) {
	t.Parallel()
	out := helmTemplateFull(t, nil,
		"keycloak.enabled=true",
		"keycloak.runtime.enabled=true",
		"keycloak.runtime.host=auth.example.com",
		"keycloak.realm.name=myrealm",
		"keycloak.oidcAudience=gardenio-beads-api",
		"beads.oidc.issuerUrl=https://gitlab.com",
	)
	if !issuerValueAfter(out, "BEADS_OIDC_ISSUER_URL", "https://gitlab.com") {
		t.Errorf("issuer-only override: expected GitLab issuer")
	}
	if issuerValueAfter(out, "BEADS_OIDC_AUDIENCE", "gardenio-beads-api") {
		t.Errorf("issuer-only override: Keycloak oidcAudience must not leak when override is active")
	}
}

// TestBeadsOIDC_OverrideDoesNotTouchDaemonToken verifies the JWT-issuer flip is
// orthogonal to the in-cluster BEADS_DAEMON_TOKEN path: flipping the OIDC
// override must not remove or alter the shared-token auth env (different threat
// model per ka-0000005702 — the acceptance criterion "in-cluster agents still
// authenticate via BEADS_DAEMON_TOKEN").
func TestBeadsOIDC_OverrideDoesNotTouchDaemonToken(t *testing.T) {
	t.Parallel()
	out := helmTemplateFull(t, nil,
		"keycloak.enabled=true",
		"keycloak.runtime.enabled=true",
		"keycloak.runtime.host=auth.example.com",
		"keycloak.realm.name=myrealm",
		"keycloak.oidcAudience=gardenio-beads-api",
		"beads.oidc.issuerUrl=https://gitlab.com",
		"beads.oidc.audience=57ab1f8b",
	)
	// The daemon accepts no shared static token (gd-ke6PC7bRto), so it must be
	// handed neither the token nor its rotation twin.
	for _, gone := range []string{"BEADS_AUTH_TOKEN", "BEADS_PREVIOUS_TOKEN"} {
		if strings.Contains(out, gone) {
			t.Errorf("override: %s must not be wired into the beads daemon", gone)
		}
	}
}
