package tests

import (
	"strings"
	"testing"
)

// MFA hardening for in-box Keycloak (MFA epic ka-0000001366, task ka-0000000050).
// The gating spike ka-0000006878 disproved the KeycloakRealmRequiredAction CRD
// approach; these tests pin the validated alternative: a custom browser flow
// built from KeycloakAuthFlow CRs (conditional subflow gated on the admin role)
// bound to the realm via KeycloakRealm.browserFlow.

// TestKeycloakMFA_DefaultOff is the customer-poc / no-MFA guard: with the
// default values (keycloak.mfa.enabled=false) the chart renders ZERO
// KeycloakAuthFlow CRs and does NOT set browserFlow on the realm.
func TestKeycloakMFA_DefaultOff(t *testing.T) {
	t.Parallel()
	out := helmTemplateFull(t, nil, "keycloak.enabled=true")
	if c := countKinds(out, "KeycloakAuthFlow")["KeycloakAuthFlow"]; c != 0 {
		t.Errorf("mfa off by default: expected 0 KeycloakAuthFlow CRs, got %d", c)
	}
	if strings.Contains(out, "browserFlow:") {
		t.Errorf("mfa off by default: realm must not set browserFlow")
	}
}

// TestKeycloakMFA_EnabledEmitsConditionalFlow verifies the three-CR flow chain
// and that the conditional subflow is gated on the admin role with a REQUIRED
// second factor, and that the realm binds the custom flow as its browserFlow.
func TestKeycloakMFA_EnabledEmitsConditionalFlow(t *testing.T) {
	t.Parallel()
	out := helmTemplateFull(t, nil,
		"keycloak.enabled=true",
		"keycloak.mfa.enabled=true",
	)

	if c := countKinds(out, "KeycloakAuthFlow")["KeycloakAuthFlow"]; c != 3 {
		t.Fatalf("expected 3 KeycloakAuthFlow CRs (browser + forms + conditional), got %d", c)
	}

	wants := []string{
		// top-level browser flow + the two child subflows wired by parentName
		`alias: "gardenio-mfa-browser"`,
		`alias: "gardenio-mfa-browser-forms"`,
		`alias: "gardenio-mfa-browser-conditional-mfa"`,
		`parentName: "gardenio-mfa-browser"`,
		`parentName: "gardenio-mfa-browser-forms"`,
		// the conditional subflow is CONDITIONAL and gated on the admin role
		"childRequirement: CONDITIONAL",
		"authenticator: conditional-user-role",
		`condUserRole: "gardenio-admin"`,
		// realm binds the custom flow
		`browserFlow: "gardenio-mfa-browser"`,
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("mfa enabled: rendered output missing %q", w)
		}
	}
}

// TestKeycloakMFA_FactorWebauthnDefault verifies the default second factor is
// WebAuthn (phishing-resistant, preferred per research §5).
func TestKeycloakMFA_FactorWebauthnDefault(t *testing.T) {
	t.Parallel()
	out := helmTemplateFull(t, nil,
		"keycloak.enabled=true",
		"keycloak.mfa.enabled=true",
	)
	if !strings.Contains(out, "authenticator: webauthn-authenticator") {
		t.Errorf("default factor should be webauthn (webauthn-authenticator)")
	}
	if strings.Contains(out, "authenticator: auth-otp-form") {
		t.Errorf("default factor webauthn must not also emit auth-otp-form")
	}
}

// TestKeycloakMFA_FactorTotp verifies factor=totp swaps the second factor to
// the TOTP authenticator.
func TestKeycloakMFA_FactorTotp(t *testing.T) {
	t.Parallel()
	out := helmTemplateFull(t, nil,
		"keycloak.enabled=true",
		"keycloak.mfa.enabled=true",
		"keycloak.mfa.factor=totp",
	)
	if !strings.Contains(out, "authenticator: auth-otp-form") {
		t.Errorf("factor=totp should emit auth-otp-form")
	}
	if strings.Contains(out, "authenticator: webauthn-authenticator") {
		t.Errorf("factor=totp must not emit webauthn-authenticator")
	}
}

// TestKeycloakMFA_FactorPasskey pins the passkey browser flow of the design
// (doc section 4.1): the conditional subflow holds, in order,
// conditional-user-role, conditional-credential (skipped when the primary
// credential was a passkey) and the passwordless WebAuthn authenticator. The
// aliases and the realm browserFlow binding do not change, and the TOTP and
// non-passwordless authenticators are not emitted.
func TestKeycloakMFA_FactorPasskey(t *testing.T) {
	t.Parallel()
	out := helmTemplateFull(t, nil,
		"keycloak.enabled=true",
		"keycloak.mfa.enabled=true",
		"keycloak.mfa.factor=passkey",
	)
	if c := countKinds(out, "KeycloakAuthFlow")["KeycloakAuthFlow"]; c != 3 {
		t.Fatalf("factor=passkey: expected 3 KeycloakAuthFlow CRs, got %d", c)
	}
	for _, w := range []string{
		`alias: "gardenio-mfa-browser"`,
		`alias: "gardenio-mfa-browser-forms"`,
		`alias: "gardenio-mfa-browser-conditional-mfa"`,
		`browserFlow: "gardenio-mfa-browser"`,
		"authenticator: conditional-user-role",
		"authenticator: conditional-credential",
		`credentials: "webauthn-passwordless"`,
		`included: "false"`,
		"authenticator: webauthn-authenticator-passwordless",
	} {
		if !strings.Contains(out, w) {
			t.Errorf("factor=passkey: rendered output missing %q", w)
		}
	}
	for _, bad := range []string{"authenticator: auth-otp-form", "authenticator: webauthn-authenticator\n"} {
		if strings.Contains(out, bad) {
			t.Errorf("factor=passkey must not emit %q", strings.TrimSpace(bad))
		}
	}

	// Execution order inside the conditional subflow.
	i := strings.Index(out, "authenticator: conditional-user-role")
	j := strings.Index(out, "authenticator: conditional-credential")
	k := strings.Index(out, "authenticator: webauthn-authenticator-passwordless")
	if !(i >= 0 && i < j && j < k) {
		t.Errorf("factor=passkey: want conditional-user-role < conditional-credential < webauthn-authenticator-passwordless, got offsets %d, %d, %d", i, j, k)
	}
	// All three are REQUIRED.
	for _, name := range []string{"conditional-user-role", "conditional-credential", "webauthn-authenticator-passwordless"} {
		line := "authenticator: " + name + "\n"
		idx := strings.Index(out, line)
		if idx < 0 {
			t.Errorf("factor=passkey: no execution %q", name)
			continue
		}
		if !strings.HasPrefix(out[idx+len(line):], "      requirement: REQUIRED\n") {
			t.Errorf("factor=passkey: %q must be REQUIRED", name)
		}
	}
}

// TestKeycloakMFA_ConditionalCredentialOnlyForPasskey: totp and webauthn
// render the exact two-step subflow they always did.
func TestKeycloakMFA_ConditionalCredentialOnlyForPasskey(t *testing.T) {
	t.Parallel()
	for _, f := range []string{"totp", "webauthn"} {
		out := helmTemplateFull(t, nil,
			"keycloak.enabled=true",
			"keycloak.mfa.enabled=true",
			"keycloak.mfa.factor="+f,
		)
		for _, bad := range []string{"conditional-credential", "webauthn-authenticator-passwordless", "webauthn-passwordless"} {
			if strings.Contains(out, bad) {
				t.Errorf("factor=%s must not render %q", f, bad)
			}
		}
	}
}

// TestKeycloakMFA_FactorInvalid: a typo in the factor fails the render and the
// message lists every valid value.
func TestKeycloakMFA_FactorInvalid(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "",
		"keycloak.enabled=true",
		"keycloak.mfa.enabled=true",
		"keycloak.mfa.factor=passkeys",
	)
	if err == nil {
		t.Fatalf("factor=passkeys (typo) must fail the render; got success")
	}
	for _, w := range []string{"totp", "webauthn", "passkey"} {
		if !strings.Contains(out, w) {
			t.Errorf("error should list %q as a valid factor; got:\n%s", w, out)
		}
	}
}

// TestKeycloakMFA_RoleConfigurable verifies the gated role follows
// keycloak.mfa.adminRole.
func TestKeycloakMFA_RoleConfigurable(t *testing.T) {
	t.Parallel()
	out := helmTemplateFull(t, nil,
		"keycloak.enabled=true",
		"keycloak.mfa.enabled=true",
		"keycloak.mfa.adminRole=gardenio-operator",
	)
	if !strings.Contains(out, `condUserRole: "gardenio-operator"`) {
		t.Errorf("adminRole override should set condUserRole to gardenio-operator")
	}
}

// TestKeycloakAuthHostRateLimit verifies the Traefik per-IP rateLimit Middleware
// is emitted for the in-box auth host and attached to the runtime IngressRoute.
// This is the front-line credential-stuffing mitigation, since the EDP
// KeycloakRealm CRD cannot express realm brute-force protection (spike
// ka-0000006878).
func TestKeycloakAuthHostRateLimit(t *testing.T) {
	t.Parallel()
	out := helmTemplateFull(t, nil,
		"keycloak.enabled=true",
		"keycloak.runtime.enabled=true",
	)
	for _, w := range []string{
		"name: test-keycloak-runtime-ratelimit",
		"rateLimit:",
		"sourceCriterion:",
		"ipStrategy:",
	} {
		if !strings.Contains(out, w) {
			t.Errorf("auth-host rate limit: rendered output missing %q", w)
		}
	}
}

// TestKeycloakAuthHostRateLimit_Disabled verifies the rateLimit can be turned
// off without affecting the IngressRoute.
func TestKeycloakAuthHostRateLimit_Disabled(t *testing.T) {
	t.Parallel()
	out := helmTemplateFull(t, nil,
		"keycloak.enabled=true",
		"keycloak.runtime.enabled=true",
		"keycloak.runtime.ingress.rateLimit.enabled=false",
	)
	if strings.Contains(out, "keycloak-runtime-ratelimit") {
		t.Errorf("rateLimit disabled: no ratelimit Middleware should render")
	}
	// The runtime IngressRoute itself must still render.
	if !strings.Contains(out, "name: test-keycloak-runtime\n") {
		t.Errorf("rateLimit disabled: runtime IngressRoute should still render")
	}
}
