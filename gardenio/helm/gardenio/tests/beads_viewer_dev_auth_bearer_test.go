package tests

import (
	"strings"
	"testing"
)

// --- beadsViewer.devAuthBearerRoles (gd-BmYmugDc4x) ---
//
// Behind the viewer-authproxy edge the dev-bypass viewer verifies the Keycloak
// access token the edge forwards and attaches its realm roles. The chart hands
// it the issuer and audience kbeads verifies the same token against, and the
// in-cluster discovery URL + CA when the issuer is the derived one.

var bearerRolesBase = []string{
	"beadsViewer.enabled=true",
	"beadsViewer.devAuthBypass=true",
	"beadsViewer.devAuthProxySecret.secretName=viewer-authproxy-secret",
	"keycloak.enabled=true",
	"keycloak.realm.issuerURL=https://auth.example.com/realms/gardenio",
	"keycloak.oidcAudience=gardenio-traefik-forwardauth",
}

func TestBeadsViewer_DevAuthBearerRoles_Renders(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads-viewer/deployment.yaml", bearerRolesBase...)
	for _, want := range []string{
		"name: BEADS_VIEWER_DEV_AUTH_BEARER_ISSUER_URL\n              value: \"https://auth.example.com/realms/gardenio\"",
		"name: BEADS_VIEWER_DEV_AUTH_BEARER_AUDIENCE\n              value: \"gardenio-traefik-forwardauth\"",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered Deployment missing %q, got:\n%s", want, out)
		}
	}
	// An explicit realm issuer is dialled directly: no in-cluster discovery.
	if strings.Contains(out, "BEADS_VIEWER_DEV_AUTH_BEARER_DISCOVERY_URL") {
		t.Errorf("discovery URL must not render for an explicit realm issuer, got:\n%s", out)
	}
	// It must never turn on the OIDC middleware the bypass replaces.
	if strings.Contains(out, "OIDC_ISSUER_URL\n") || strings.Contains(out, "name: OIDC_CLIENT_ID") {
		t.Errorf("OIDC_* must stay off under the bypass, got:\n%s", out)
	}
}

func TestBeadsViewer_DevAuthBearerRoles_Overrides(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads-viewer/deployment.yaml", append(bearerRolesBase,
		"beadsViewer.devAuthBearerRoles.issuerURL=https://other.example.com/realms/r",
		"beadsViewer.devAuthBearerRoles.audience=other-client",
	)...)
	for _, want := range []string{`value: "https://other.example.com/realms/r"`, `value: "other-client"`} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered Deployment missing %q, got:\n%s", want, out)
		}
	}
}

func TestBeadsViewer_DevAuthBearerRoles_Off(t *testing.T) {
	t.Parallel()
	cases := map[string][]string{
		"no edge secret": {
			"beadsViewer.enabled=true", "beadsViewer.devAuthBypass=true",
			"keycloak.enabled=true", "keycloak.realm.issuerURL=https://auth.example.com/realms/gardenio",
			"keycloak.oidcAudience=gardenio-traefik-forwardauth",
		},
		"no audience":  append(bearerRolesBase[:5:5], "keycloak.oidcAudience="),
		"disabled":     append(bearerRolesBase, "beadsViewer.devAuthBearerRoles.enabled=false"),
		"keycloak off": append(bearerRolesBase, "keycloak.enabled=false"),
	}
	for name, flags := range cases {
		t.Run(name, func(t *testing.T) {
			out := helmTemplate(t, "templates/beads-viewer/deployment.yaml", flags...)
			if strings.Contains(out, "BEADS_VIEWER_DEV_AUTH_BEARER_") {
				t.Errorf("bearer-roles env must not render, got:\n%s", out)
			}
		})
	}
}

// On k3s the issuer is the derived public one and Keycloak's clients are on
// the https port: discovery goes over the Service and trusts its CA, exactly
// as the beads daemon's does, and the CA is mounted.
func TestBeadsViewer_DevAuthBearerRoles_K3s(t *testing.T) {
	t.Parallel()
	out := helmTemplateShowOnlyWithValues(t, "templates/beads-viewer/deployment.yaml", []string{"../../../deploy-values/gardenio-k3s.yaml"})
	for _, want := range []string{
		"BEADS_VIEWER_DEV_AUTH_BEARER_ISSUER_URL",
		"name: BEADS_VIEWER_DEV_AUTH_BEARER_AUDIENCE\n              value: \"gardenio-traefik-forwardauth\"",
		"BEADS_VIEWER_DEV_AUTH_BEARER_DISCOVERY_URL",
		"name: BEADS_VIEWER_DEV_AUTH_BEARER_CA_FILE\n              value: \"/etc/gardenio/keycloak-runtime-ca/ca.crt\"",
		"name: keycloak-runtime-ca\n              mountPath:",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("k3s viewer Deployment missing %q, got:\n%s", want, out)
		}
	}
}
