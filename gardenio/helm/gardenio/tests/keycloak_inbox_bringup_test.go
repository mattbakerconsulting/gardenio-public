package tests

import (
	"strings"
	"testing"
)

// inboxOn renders the chart with the in-box Keycloak runtime flipped on and the
// in-cluster postgres available, matching deploy-values/gardenio-inbox.yaml.
func inboxOn(t *testing.T, extra ...string) string {
	t.Helper()
	base := []string{
		"postgres.enabled=true",
		"keycloak.enabled=true",
		"keycloak.runtime.enabled=true",
		"keycloak.runtime.db.autoWire=true",
		"keycloak.runtime.host=auth.example.com",
		"keycloak.realm.name=myrealm",
	}
	return helmTemplateFull(t, nil, append(base, extra...)...)
}

// TestInboxKeycloak_DBAutoWire verifies §7.1: when postgres.enabled and
// runtime.db.autoWire are both true the StatefulSet derives KC_DB_URL from the
// in-cluster postgres Service and a create-db init container is emitted, rather
// than projecting KC_DB_* from the out-of-band gardenio-keycloak-runtime-db Secret.
func TestInboxKeycloak_DBAutoWire(t *testing.T) {
	t.Parallel()
	out := inboxOn(t)

	wantURL := `value: "jdbc:postgresql://test-postgres.`
	if !strings.Contains(out, wantURL) {
		t.Errorf("autoWire: expected derived KC_DB_URL containing %q, not found", wantURL)
	}
	if !strings.Contains(out, "/keycloak?ApplicationName=keycloak\"") {
		t.Errorf("autoWire: KC_DB_URL should target the 'keycloak' logical database")
	}
	if !strings.Contains(out, "name: create-db") {
		t.Errorf("autoWire: expected the create-db init container to be rendered")
	}
	// In autowire mode the BYO db Secret must NOT be referenced.
	if strings.Contains(out, "gardenio-keycloak-runtime-db") {
		t.Errorf("autoWire: BYO db Secret gardenio-keycloak-runtime-db should not be referenced")
	}
}

// TestInboxKeycloak_DBByoPath verifies the default (autoWire off) still projects
// KC_DB_* from the pre-provisioned external Secret and emits no init container.
func TestInboxKeycloak_DBByoPath(t *testing.T) {
	t.Parallel()
	out := helmTemplateFull(t, nil,
		"keycloak.enabled=true",
		"keycloak.runtime.enabled=true",
	)
	if !strings.Contains(out, "gardenio-keycloak-runtime-db") {
		t.Errorf("BYO path: expected KC_DB_* to reference gardenio-keycloak-runtime-db Secret")
	}
	if strings.Contains(out, "name: create-db") {
		t.Errorf("BYO path: create-db init container must not render without autoWire")
	}
}

// TestKeycloakRuntime_PersistenceDefaultOff verifies gd-85rqxsPPoM: realm/
// client/user data lives in postgres (db.vendor defaults to "postgres", no
// H2 path exists in this chart), so /opt/keycloak/data holds nothing worth a
// PVC by default — it must render as a plain emptyDir with no
// volumeClaimTemplates, so a spot-node loss of gardenio-keycloak-runtime-0
// never strands the pod on a node-bound PV.
func TestKeycloakRuntime_PersistenceDefaultOff(t *testing.T) {
	t.Parallel()
	out := helmTemplateFull(t, nil,
		"keycloak.enabled=true",
		"keycloak.runtime.enabled=true",
	)
	if strings.Contains(out, "volumeClaimTemplates") {
		t.Errorf("keycloak-runtime: must NOT render volumeClaimTemplates by default (persistence.enabled default must be false), got:\n%s", out)
	}
	dataVol := dataVolumeBlock(out)
	if dataVol == "" {
		t.Fatalf("keycloak-runtime: no `- name: data` volume found — the /opt/keycloak/data mount would dangle:\n%s", out)
	}
	if !strings.Contains(dataVol, "emptyDir") {
		t.Errorf("keycloak-runtime: data volume must be backed by emptyDir by default, got:\n%s", dataVol)
	}
	if !strings.Contains(out, "mountPath: /opt/keycloak/data") {
		t.Errorf("keycloak-runtime: expected /opt/keycloak/data to remain mounted, not found in:\n%s", out)
	}
}

// TestKeycloakRuntime_PersistenceEnabledUsesPVC verifies the opt-in path still
// works: setting persistence.enabled=true must still render a real PVC via
// volumeClaimTemplates, not an emptyDir.
func TestKeycloakRuntime_PersistenceEnabledUsesPVC(t *testing.T) {
	t.Parallel()
	out := helmTemplateFull(t, nil,
		"keycloak.enabled=true",
		"keycloak.runtime.enabled=true",
		"keycloak.runtime.persistence.enabled=true",
		"keycloak.runtime.persistence.size=8Gi",
	)
	if !strings.Contains(out, "volumeClaimTemplates") {
		t.Errorf("keycloak-runtime: must render volumeClaimTemplates (PVC) when persistence.enabled=true, got:\n%s", out)
	}
	if dataVol := dataVolumeBlock(out); strings.Contains(dataVol, "emptyDir") {
		t.Errorf("keycloak-runtime: data volume must come from the PVC claim template, not emptyDir, when persistence is enabled; got:\n%s", dataVol)
	}
}

// TestInboxKeycloak_AdminSecretGenerated verifies §7.3: the chart generates the
// bootstrap-admin Secret with resource-policy keep when generate=true (default).
func TestInboxKeycloak_AdminSecretGenerated(t *testing.T) {
	t.Parallel()
	out := helmTemplateFull(t, nil,
		"keycloak.enabled=true",
		"keycloak.runtime.enabled=true",
	)
	sec := extractSecret(out, "gardenio-keycloak-runtime-admin")
	if sec == "" {
		t.Fatalf("expected generated Secret gardenio-keycloak-runtime-admin, none found")
	}
	if !strings.Contains(sec, `"helm.sh/resource-policy": keep`) {
		t.Errorf("admin Secret should carry helm.sh/resource-policy: keep for stable password")
	}
	if !strings.Contains(sec, "username:") || !strings.Contains(sec, "password:") {
		t.Errorf("admin Secret must contain username and password keys")
	}
}

// TestInboxKeycloak_AdminGenerateDisabled verifies generate=false suppresses the
// Secret (operator supplies it out-of-band).
func TestInboxKeycloak_AdminGenerateDisabled(t *testing.T) {
	t.Parallel()
	out := helmTemplateFull(t, nil,
		"keycloak.enabled=true",
		"keycloak.runtime.enabled=true",
		"keycloak.runtime.admin.generate=false",
	)
	if strings.Contains(out, "name: \"gardenio-keycloak-runtime-admin\"\n  namespace:") {
		t.Errorf("admin Secret should not be generated when generate=false")
	}
}

// TestInboxKeycloak_IssuerDerived verifies §7.4: with runtime on and an empty
// realm.issuerURL, BEADS/COOPMUX consumers get the derived https issuer URL.
func TestInboxKeycloak_IssuerDerived(t *testing.T) {
	t.Parallel()
	// coopmux is disabled by default; enable it so its deployment renders.
	out := inboxOn(t, "coopmux.enabled=true")
	want := `https://auth.example.com/auth/realms/myrealm`
	for _, env := range []string{"BEADS_OIDC_ISSUER_URL", "COOP_MUX_OIDC_ISSUER_URL"} {
		if !issuerValueAfter(out, env, want) {
			t.Errorf("%s: expected derived issuer %q", env, want)
		}
	}
}

// TestInboxKeycloak_IssuerExplicitWins verifies an explicit realm.issuerURL is
// emitted verbatim and is not overridden by the derivation.
func TestInboxKeycloak_IssuerExplicitWins(t *testing.T) {
	t.Parallel()
	out := inboxOn(t, `keycloak.realm.issuerURL=https://explicit.example/auth/realms/x`)
	if !issuerValueAfter(out, "BEADS_OIDC_ISSUER_URL", "https://explicit.example/auth/realms/x") {
		t.Errorf("explicit realm.issuerURL should win over derivation")
	}
}

// TestInboxKeycloak_IssuerNoopWhenRuntimeOff verifies the derivation is a true
// no-op for existing installs: runtime off + empty issuer => no issuer env var.
func TestInboxKeycloak_IssuerNoopWhenRuntimeOff(t *testing.T) {
	t.Parallel()
	out := helmTemplateFull(t, nil, "keycloak.enabled=true")
	if strings.Contains(out, "BEADS_OIDC_ISSUER_URL") {
		t.Errorf("runtime off + empty issuerURL must not emit BEADS_OIDC_ISSUER_URL")
	}
}

// TestInboxKeycloak_TLSExistingSecretSuppressesCertManager verifies §7.2: setting
// tls.existingSecretName skips the cert-manager Certificate and external-dns
// DNSEndpoint while the keycloak-runtime IngressRoute still references the secret.
func TestInboxKeycloak_TLSExistingSecretSuppressesCertManager(t *testing.T) {
	t.Parallel()
	out := inboxOn(t,
		"tls.enabled=true",
		"tls.existingSecretName=my-wildcard-tls",
	)
	if strings.Contains(out, "kind: Certificate") {
		t.Errorf("existingSecretName should suppress the cert-manager Certificate")
	}
	if strings.Contains(out, "kind: DNSEndpoint") {
		t.Errorf("existingSecretName should suppress the external-dns DNSEndpoint")
	}
	if !strings.Contains(out, "secretName: my-wildcard-tls") {
		t.Errorf("IngressRoute should reference the operator-supplied TLS secret")
	}
}

// TestInboxKeycloak_FrontendUrlOmittedByDefault is the no-regression / fail-loud
// guard for the conditional `frontendUrl` on the KeycloakRealm CR (added by
// 05f9263c4, gd-Ao27qM3NrR). With the default value ("") the realm CR must OMIT
// the `frontendUrl` key entirely — Keycloak then stamps its own issuer claims,
// which is correct on installs where KC_HOSTNAME is set. An accidental empty
// `frontendUrl: ""` would pin the realm to a blank frontend base and break OIDC
// issuer matching, so the KEY must be absent, not merely empty.
func TestInboxKeycloak_FrontendUrlOmittedByDefault(t *testing.T) {
	t.Parallel()
	out := helmTemplateFull(t, nil, "keycloak.enabled=true")
	realm := extractKeycloakRealmDoc(out)
	if realm == "" {
		t.Fatalf("expected a KeycloakRealm CR in the default render, none found")
	}
	if strings.Contains(realm, "frontendUrl:") {
		t.Errorf("default (keycloak.realm.frontendUrl=\"\") must OMIT the frontendUrl key from the realm CR; got:\n%s", realm)
	}
}

// TestInboxKeycloak_FrontendUrlRendersWhenSet verifies the override: setting
// keycloak.realm.frontendUrl stamps `spec.frontendUrl: "<value>"` onto the
// realm CR verbatim (base URL only, no realm path). This is what makes the
// shared Keycloak emit https:// issuer/token-service claims for THIS realm on
// installs with no KC_HOSTNAME and KC_PROXY_HEADERS=forwarded — otherwise OIDC
// clients reject the http:// issuer as a mismatch and auth fails closed.
func TestInboxKeycloak_FrontendUrlRendersWhenSet(t *testing.T) {
	t.Parallel()
	const want = "https://auth.example.com/auth"
	out := helmTemplateFull(t, nil,
		"keycloak.enabled=true",
		"keycloak.realm.frontendUrl="+want,
	)
	realm := extractKeycloakRealmDoc(out)
	if realm == "" {
		t.Fatalf("expected a KeycloakRealm CR with frontendUrl set, none found")
	}
	if !strings.Contains(realm, `frontendUrl: "`+want+`"`) {
		t.Errorf("keycloak.realm.frontendUrl=%q should render frontendUrl: %q on the realm CR; got:\n%s", want, want, realm)
	}
}

// extractKeycloakRealmDoc returns the YAML document for the top-level
// KeycloakRealm CR (the one whose `kind:` is a top-level field), skipping
// KeycloakRealmGroup/Role/IdentityProvider docs and subresources that only
// mention `kind: KeycloakRealm` inside ownerReferences.
func extractKeycloakRealmDoc(out string) string {
	for doc := range strings.SplitSeq(out, "\n---\n") {
		for line := range strings.SplitSeq(doc, "\n") {
			if strings.TrimSpace(line) == "kind: KeycloakRealm" && !strings.HasPrefix(line, " ") {
				return doc
			}
		}
	}
	return ""
}

// extractSecret returns the YAML document for a Secret with the given name.
func extractSecret(out, name string) string {
	for doc := range strings.SplitSeq(out, "\n---\n") {
		if strings.Contains(doc, "kind: Secret") &&
			strings.Contains(doc, "name: \""+name+"\"") {
			return doc
		}
	}
	return ""
}

// issuerValueAfter checks that the env var named `env` is followed by a
// `value: "<want>"` line within a few lines.
func issuerValueAfter(out, env, want string) bool {
	lines := strings.Split(out, "\n")
	for i, l := range lines {
		if strings.Contains(l, "name: "+env) {
			for j := i + 1; j < len(lines) && j < i+3; j++ {
				if strings.Contains(lines[j], "value:") && strings.Contains(lines[j], want) {
					return true
				}
			}
		}
	}
	return false
}
