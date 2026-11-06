package tests

import (
	"os/exec"
	"strings"
	"testing"
)

// gd-l1Jh9aFEXY: the beads daemon's OIDC issuer must be byte-equal to the iss
// Keycloak stamps, and its discovery fetch must dial the Service's REAL scheme
// and port. The old derivation produced https://<svc-fqdn>/auth/realms/<r>
// (port 443) against a Service that only serves http on 8080, so the verifier
// was disabled from boot.

func beadsOIDCRender(t *testing.T, show string, extra ...string) string {
	t.Helper()
	args := []string{
		"template", "test", chartDir(t), "--namespace", "gardenio",
		"--show-only", show,
		"--set", "postgres.enabled=true",
		"--set", "keycloak.enabled=true",
		"--set", "keycloak.runtime.enabled=true",
		"--set", "keycloak.runtime.db.autoWire=true",
		"--set", "keycloak.runtime.host=gardenio-keycloak-runtime.gardenio.svc.cluster.local",
		"--set", "keycloak.realm.name=gardenio",
		"--set", "keycloak.oidcAudience=gardenio-traefik-forwardauth",
	}
	for _, s := range extra {
		args = append(args, "--set", s)
	}
	out, err := exec.Command("helm", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template %s: %v\n%s", show, err, out)
	}
	return string(out)
}

func TestBeadsOIDC_IssuerFollowsHostnameURL(t *testing.T) {
	t.Parallel()
	extra := []string{
		"keycloak.runtime.hostnameUrl=https://auth.example.com/auth",
		"keycloak.runtime.hostnameBackchannelDynamic=true",
	}
	beads := beadsOIDCRender(t, "templates/beads/deployment.yaml", extra...)
	sts := beadsOIDCRender(t, "templates/keycloak-runtime/statefulset.yaml", extra...)
	svc := beadsOIDCRender(t, "templates/keycloak-runtime/service.yaml", extra...)

	// Issuer == KC_HOSTNAME + /realms/<realm>: what Keycloak stamps as iss.
	kcHost, ok := kcEnvValue(t, sts, "KC_HOSTNAME")
	if !ok {
		t.Fatal("KC_HOSTNAME not rendered")
	}
	issuer, ok := kcEnvValue(t, beads, "BEADS_OIDC_ISSUER_URL")
	if !ok {
		t.Fatal("BEADS_OIDC_ISSUER_URL not rendered")
	}
	if want := kcHost + "/realms/gardenio"; issuer != want {
		t.Errorf("BEADS_OIDC_ISSUER_URL = %q, want %q (KC_HOSTNAME + realm)", issuer, want)
	}

	// Discovery is fetched over the Service: http, and the port the Service exposes.
	disc, ok := kcEnvValue(t, beads, "BEADS_OIDC_DISCOVERY_URL")
	if !ok {
		t.Fatal("BEADS_OIDC_DISCOVERY_URL not rendered")
	}
	const want = "http://test-keycloak-runtime.gardenio.svc:8080/auth/realms/gardenio"
	if disc != want {
		t.Errorf("BEADS_OIDC_DISCOVERY_URL = %q, want %q", disc, want)
	}
	if !strings.Contains(svc, "name: test-keycloak-runtime\n") && !strings.Contains(svc, "name: test-gardenio-keycloak-runtime\n") {
		t.Logf("service render:\n%s", svc)
	}
	if !strings.Contains(svc, "port: 8080\n      targetPort: http") {
		t.Errorf("Service no longer exposes http on 8080; update the discovery URL derivation:\n%s", svc)
	}
	// The discovery host must be the rendered Service's own name.
	host := strings.TrimPrefix(disc, "http://")
	host = host[:strings.Index(host, ".gardenio.svc")]
	if !strings.Contains(svc, "name: "+host+"\n") {
		t.Errorf("discovery host %q is not the rendered Service name:\n%s", host, svc)
	}
}

// Without hostnameUrl the historical derivation is unchanged and no discovery
// override is emitted.
func TestBeadsOIDC_NoHostnameURLNoDiscoveryOverride(t *testing.T) {
	t.Parallel()
	beads := beadsOIDCRender(t, "templates/beads/deployment.yaml")
	if v, ok := kcEnvValue(t, beads, "BEADS_OIDC_DISCOVERY_URL"); ok {
		t.Errorf("BEADS_OIDC_DISCOVERY_URL = %q rendered without hostnameUrl", v)
	}
	if v, _ := kcEnvValue(t, beads, "BEADS_OIDC_ISSUER_URL"); v != "https://gardenio-keycloak-runtime.gardenio.svc.cluster.local/auth/realms/gardenio" {
		t.Errorf("BEADS_OIDC_ISSUER_URL = %q, historical derivation changed", v)
	}
}

// An explicit beads.oidc.issuerUrl override still wins and takes no discovery override.
func TestBeadsOIDC_OverrideSkipsDiscoveryURL(t *testing.T) {
	t.Parallel()
	beads := beadsOIDCRender(t, "templates/beads/deployment.yaml",
		"keycloak.runtime.hostnameUrl=https://auth.example.com/auth",
		"beads.oidc.issuerUrl=https://gitlab.com", "beads.oidc.audience=abc")
	if _, ok := kcEnvValue(t, beads, "BEADS_OIDC_DISCOVERY_URL"); ok {
		t.Error("BEADS_OIDC_DISCOVERY_URL must not be set with a beads.oidc.issuerUrl override")
	}
}
