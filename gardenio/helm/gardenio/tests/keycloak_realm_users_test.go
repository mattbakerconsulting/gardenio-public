package tests

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const realmUsersTpl = "templates/keycloak/realm-users.yaml"

// With the default (empty) keycloak.realmUsers the template renders nothing,
// so existing consumers see no new CR (and need no new CRD).
func TestKeycloakRealmUsers_DefaultRendersNothing(t *testing.T) {
	t.Parallel()
	// Full render (not --show-only): it must succeed, so a template error
	// cannot masquerade as "renders nothing".
	out := helmTemplate(t, "")
	if strings.Contains(out, "KeycloakRealmUser") {
		t.Fatalf("default render must emit no KeycloakRealmUser, got:\n%s", out)
	}
}

func TestKeycloakRealmUsers_RendersAdminMember(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateGardenio(t, realmUsersTpl,
		"keycloak.realmUsers[0].username=owner@example.com",
		"keycloak.realmUsers[0].groups[0]=gardenio-admins",
	)
	if err != nil {
		t.Fatalf("helm template failed: %v\n%s", err, out)
	}
	for _, want := range []string{
		"kind: KeycloakRealmUser",
		`username: "owner@example.com"`,
		`email: "owner@example.com"`,
		"emailVerified: true",
		"enabled: true",
		"- gardenio-admins",
		`reconciliationStrategy: "addOnly"`,
		"kind: KeycloakRealm",
		"-user-owner-at-example-com",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q:\n%s", want, out)
		}
	}
	// Unset names must not render (an empty name would blank the live user's).
	for _, bad := range []string{"firstName", "lastName"} {
		if strings.Contains(out, bad) {
			t.Errorf("render must not carry %q when unset:\n%s", bad, out)
		}
	}
	// Never a credential in git.
	for _, bad := range []string{"password", "Password"} {
		if strings.Contains(out, bad) {
			t.Errorf("render must not carry %q:\n%s", bad, out)
		}
	}
}

func TestKeycloakRealmUsers_KeycloakOffRendersNothing(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateGardenio(t, "",
		"keycloak.enabled=false",
		"keycloak.realmUsers[0].username=owner@example.com",
	)
	if err != nil {
		t.Fatalf("helm template failed: %v\n%s", err, out)
	}
	if strings.Contains(out, "KeycloakRealmUser") {
		t.Fatalf("keycloak.enabled=false must emit no KeycloakRealmUser, got:\n%s", out)
	}
}

// The operator defaults to `full`, which strips unlisted groups and direct
// realm roles from an adopted user; the default must stay addOnly and an
// entry may still opt in explicitly.
func TestKeycloakRealmUsers_ReconciliationStrategy(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateGardenio(t, realmUsersTpl,
		"keycloak.realmUsers[0].username=owner@example.com",
		"keycloak.realmUsers[0].reconciliationStrategy=full",
	)
	if err != nil {
		t.Fatalf("helm template failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, `reconciliationStrategy: "full"`) || strings.Contains(out, "addOnly") {
		t.Errorf("explicit strategy not honoured:\n%s", out)
	}
}

func TestKeycloakRealmUsers_RendersNamesWhenSet(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateGardenio(t, realmUsersTpl,
		"keycloak.realmUsers[0].username=owner@example.com",
		"keycloak.realmUsers[0].firstName=Matthew",
		"keycloak.realmUsers[0].lastName=Baker",
	)
	if err != nil {
		t.Fatalf("helm template failed: %v\n%s", err, out)
	}
	for _, want := range []string{`firstName: "Matthew"`, `lastName: "Baker"`} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q:\n%s", want, out)
		}
	}
}

// The prod overlay's owner entry renders addOnly, the names, the
// preserve-on-deletion annotation, and no credential.
func TestKeycloakRealmUsers_ProdOverlayOwner(t *testing.T) {
	t.Parallel()
	overlay := filepath.Join(chartDir(t), "..", "..", "..", "deploy-values", "gardenio-k3s.yaml")
	cmd := exec.Command("helm", "template", "gardenio", chartDir(t),
		"--show-only", realmUsersTpl, "-f", overlay)
	raw, err := cmd.CombinedOutput()
	out := string(raw)
	if err != nil {
		t.Fatalf("helm template with prod overlay failed: %v\n%s", err, out)
	}
	for _, want := range []string{
		`username: "matthewbaker1110@gmail.com"`,
		`firstName: "Matthew"`,
		`lastName: "Baker"`,
		`reconciliationStrategy: "addOnly"`,
		`edp.epam.com/preserve-resources-on-deletion: "true"`,
		"- gardenio-admins",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("prod render missing %q:\n%s", want, out)
		}
	}
	// No credential in git: only the land-door verifier's test owner names a
	// Secret (gd-Hq1PnUVOUC), and nobody carries a literal password.
	for doc := range strings.SplitSeq(out, "\n---") {
		if strings.Contains(doc, "\n  password:") {
			t.Errorf("prod render carries a literal password:\n%s", doc)
		}
		if strings.Contains(doc, "passwordSecret") && !strings.Contains(doc, `username: "land-verifier-scopetest@lands.clonk.tv"`) {
			t.Errorf("only the land-door verifier may name a passwordSecret:\n%s", doc)
		}
	}
	_, verifier, ok := strings.Cut(out, `username: "land-verifier-scopetest@lands.clonk.tv"`)
	if !ok {
		t.Fatalf("prod render has no land-door verifier user:\n%s", out)
	}
	if i := strings.Index(verifier, "\n---"); i >= 0 {
		verifier = verifier[:i]
	}
	for _, want := range []string{"passwordSecret:\n    name: \"crew-land-verifier\"\n    key: \"password\"\n    temporary: false", `reconciliationStrategy: "addOnly"`} {
		if !strings.Contains(verifier, want) {
			t.Errorf("verifier user missing %q:\n%s", want, verifier)
		}
	}
	// Its owners membership is set by path, never by the operator's bare-name
	// group match, which could pick another land's /owners.
	if strings.Contains(verifier, "groups:") {
		t.Errorf("verifier user must list no groups:\n%s", verifier)
	}
}

// A machine test identity names its password's Secret, never the value, and
// both name and key are required.
func TestKeycloakRealmUsers_PasswordSecret(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateGardenio(t, realmUsersTpl,
		"keycloak.realmUsers[0].username=bot@example.com",
		"keycloak.realmUsers[0].passwordSecret.name=crew-bot",
		"keycloak.realmUsers[0].passwordSecret.key=password",
	)
	if err != nil {
		t.Fatalf("helm template failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "passwordSecret:\n    name: \"crew-bot\"\n    key: \"password\"\n    temporary: false") {
		t.Errorf("passwordSecret not rendered:\n%s", out)
	}
	if out, err := helmTemplateGardenio(t, realmUsersTpl,
		"keycloak.realmUsers[0].username=bot@example.com",
		"keycloak.realmUsers[0].passwordSecret.name=crew-bot",
	); err == nil {
		t.Errorf("passwordSecret without key rendered, want a fail:\n%s", out)
	}
}

// The prod overlay declares the public device-grant client behind `kd login`
// (gd-bFFquoScHy): device grant only, and tokens that the kbeads verifier and
// RBAC accept (audience = keycloak.oidcAudience, roles in realm_roles).
func TestKeycloakClients_ProdOverlayCLIDeviceGrant(t *testing.T) {
	t.Parallel()
	overlay := filepath.Join(chartDir(t), "..", "..", "..", "deploy-values", "gardenio-k3s.yaml")
	cmd := exec.Command("helm", "template", "gardenio", chartDir(t),
		"--show-only", "templates/keycloak/clients.yaml", "-f", overlay)
	raw, err := cmd.CombinedOutput()
	out := string(raw)
	if err != nil {
		t.Fatalf("helm template with prod overlay failed: %v\n%s", err, out)
	}
	_, doc, ok := strings.Cut(out, "clientId: \"gardenio-cli\"")
	if !ok {
		t.Fatalf("prod overlay renders no gardenio-cli KeycloakClient:\n%s", out)
	}
	if i := strings.Index(doc, "\n---"); i >= 0 {
		doc = doc[:i]
	}
	for _, want := range []string{
		"public: true",
		"standardFlowEnabled: false",
		"directAccess: false",
		`oauth2.device.authorization.grant.enabled: "true"`,
		"claim.name: realm_roles",
		"included.client.audience: gardenio-traefik-forwardauth",
		// Bounded session: a code the owner confirmed for a pod's `kd login`
		// must lapse (gd-bFFquoScHy phishing mitigation). 8h idle / 7d total
		// per owner decision gd-lpQFvBIWZ0.
		`access.token.lifespan: "300"`,
		`client.session.idle.timeout: "28800"`,
		`client.session.max.lifespan: "604800"`,
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("gardenio-cli client missing %q:\n%s", want, doc)
		}
	}
	// A public client must never carry a secret or redirect URIs it has no use for.
	for _, bad := range []string{"secret:", "redirectUris:"} {
		if strings.Contains(doc, bad) {
			t.Errorf("gardenio-cli client must not render %q:\n%s", bad, doc)
		}
	}
}
