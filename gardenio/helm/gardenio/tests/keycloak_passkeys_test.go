package tests

import (
	"strings"
	"testing"
)

// Passkeys (plan gd-QXq1fjYuRD, step S3a, gd-h9M9eTTun8). The KeycloakRealm CRD
// cannot express WebAuthn policy, so the first-admin hook applies it with kcadm,
// gated on keycloak.runtime.realmHardening.passkeys.enabled (default false).
// The shipped commands are also run against a real Keycloak by the
// keycloak-theme harness (gardenio/tests/keycloak-theme/passkey-policy.sh).

const (
	pkOn   = "keycloak.runtime.realmHardening.passkeys.enabled=true"
	pkURL  = "keycloak.runtime.hostnameUrl=https://auth.example.com/auth"
	pkBase = "keycloak.runtime.realmHardening.passkeys."
)

func pkRender(t *testing.T, extra ...string) string {
	t.Helper()
	return helmTemplateShowOnlyWithValues(t, firstAdminJob, nil,
		append([]string{"keycloak.enabled=true", "keycloak.runtime.enabled=true"}, extra...)...)
}

func pkFail(t *testing.T, extra ...string) string {
	t.Helper()
	args := []string{"--set", "keycloak.enabled=true", "--set", "keycloak.runtime.enabled=true"}
	for _, e := range extra {
		args = append(args, "--set", e)
	}
	return helmTemplateExpectError(t, firstAdminJob, args...)
}

// pkBlock returns the text between the passkeys:begin/end marker comments.
func pkBlock(t *testing.T, out string) string {
	t.Helper()
	b := strings.Index(out, "# --- passkeys:begin")
	e := strings.Index(out, "# --- passkeys:end")
	if b < 0 || e < b {
		t.Fatalf("passkey block markers missing (begin=%d end=%d):\n%s", b, e, out)
	}
	return out[b:e]
}

func TestKeycloakPasskeys_OffByDefault(t *testing.T) {
	t.Parallel()
	out := pkRender(t)
	if !strings.Contains(out, "[first-admin] DONE") {
		t.Fatalf("first-admin hook did not render:\n%s", out)
	}
	for _, s := range []string{"passkeys:begin", "webAuthnPolicy", "passkey"} {
		if strings.Contains(out, s) {
			t.Errorf("passkeys are off by default: script must not contain %q", s)
		}
	}
}

func TestKeycloakPasskeys_EnabledSetsEveryAttribute(t *testing.T) {
	t.Parallel()
	out := pkRender(t, pkOn, pkURL)
	block := pkBlock(t, out)
	for _, w := range []string{
		`kcadm update "realms/${KC_REALM}"`,
		"-s webAuthnPolicyPasswordlessPasskeysEnabled=true",
		"-s webAuthnPolicyPasswordlessRpId=auth.example.com",
		"-s webAuthnPolicyPasswordlessRpEntityName='gardenio'",
		"-s webAuthnPolicyPasswordlessUserVerificationRequirement=required",
		"-s webAuthnPolicyPasswordlessResidentKey=required",
		"-s webAuthnPolicyPasswordlessAttestationConveyancePreference=none",
		"-s webAuthnPolicyPasswordlessAuthenticatorAttachment='not specified'",
		`-s 'webAuthnPolicyPasswordlessSignatureAlgorithms=["ES256","RS256"]'`,
		"-s webAuthnPolicyPasswordlessAvoidSameAuthenticatorRegister=true",
	} {
		if !strings.Contains(block, w) {
			t.Errorf("passkeys on: block missing %q\n%s", w, block)
		}
	}
	// The deprecated attribute stays untouched (it reads "not specified" on 26.7).
	if strings.Contains(block, "-s webAuthnPolicyPasswordlessRequireResidentKey") {
		t.Error("must set webAuthnPolicyPasswordlessResidentKey, not the deprecated ...RequireResidentKey")
	}
	// Non-fatal like bruteForce: warn and continue so the admin bootstrap runs.
	if !strings.Contains(block, "WARNING: could not apply the passkey policy") {
		t.Error("a failed passkey update must warn, not die")
	}
	// Ordered after the realm wait and before the group wait.
	realm := strings.Index(out, "realm ${KC_REALM} present")
	pk := strings.Index(out, "# --- passkeys:begin")
	grp := strings.Index(out, "# 3) Wait for the target group")
	if !(realm >= 0 && realm < pk && pk < grp) {
		t.Errorf("passkey step must sit between the realm wait and the group wait (realm=%d pk=%d group=%d)", realm, pk, grp)
	}
}

func TestKeycloakPasskeys_ValuesOverride(t *testing.T) {
	t.Parallel()
	out := pkRender(t, pkOn, pkURL,
		pkBase+"rpName=My Garden",
		pkBase+"userVerification=preferred",
		pkBase+"requireResidentKey=No",
		pkBase+"attestation=direct",
		pkBase+"authenticatorAttachment=platform",
		pkBase+"signatureAlgorithms={ES256}",
		pkBase+"avoidSameAuthenticatorRegister=false")
	block := pkBlock(t, out)
	for _, w := range []string{
		"RpEntityName='My Garden'",
		"UserVerificationRequirement=preferred",
		"ResidentKey=discouraged",
		"AttestationConveyancePreference=direct",
		"AuthenticatorAttachment='platform'",
		`SignatureAlgorithms=["ES256"]'`,
		"AvoidSameAuthenticatorRegister=false",
	} {
		if !strings.Contains(block, w) {
			t.Errorf("override missing %q\n%s", w, block)
		}
	}
}

func TestKeycloakPasskeys_RPIDDefaultsToIssuerHost(t *testing.T) {
	t.Parallel()
	// A port and a mixed-case host: the RP ID is the bare, lower-case host.
	block := pkBlock(t, pkRender(t, pkOn, "keycloak.runtime.hostnameUrl=https://Auth.Example.com:8443/auth"))
	if !strings.Contains(block, "RpId=auth.example.com ") {
		t.Errorf("RP ID must be the host of hostnameUrl:\n%s", block)
	}
}

func TestKeycloakPasskeys_RPIDExplicitParentDomainAllowed(t *testing.T) {
	t.Parallel()
	block := pkBlock(t, pkRender(t, pkOn, pkURL, pkBase+"rpId=example.com"))
	if !strings.Contains(block, "RpId=example.com ") {
		t.Errorf("a parent domain of the issuer host is a valid RP ID:\n%s", block)
	}
}

func TestKeycloakPasskeys_RPIDMustBeSuffixOfIssuerHost(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{"other.com", "uth.example.com", "evil-example.com", "auth.example.com.evil.com"} {
		out := pkFail(t, pkOn, pkURL, pkBase+"rpId="+bad)
		if !strings.Contains(out, "is not the issuer host") || !strings.Contains(out, "auth.example.com") {
			t.Errorf("rpId %q: want a clear suffix error, got:\n%s", bad, out)
		}
	}
}

func TestKeycloakPasskeys_RPIDMustBeBareHost(t *testing.T) {
	t.Parallel()
	out := pkFail(t, pkOn, pkURL, pkBase+"rpId=https://auth.example.com")
	if !strings.Contains(out, "bare host name") {
		t.Errorf("want a bare-host error, got:\n%s", out)
	}
}

func TestKeycloakPasskeys_NeedsAnRPID(t *testing.T) {
	t.Parallel()
	// No hostnameUrl and no rpId: the render must fail, never guess.
	out := pkFail(t, pkOn)
	if !strings.Contains(out, "needs an RP ID") {
		t.Errorf("want a missing-RP-ID error, got:\n%s", out)
	}
	// An explicit rpId without hostnameUrl is accepted (nothing to compare it to).
	block := pkBlock(t, pkRender(t, pkOn, pkBase+"rpId=auth.example.com"))
	if !strings.Contains(block, "RpId=auth.example.com ") {
		t.Errorf("explicit rpId not rendered:\n%s", block)
	}
}

func TestKeycloakPasskeys_RejectsBadEnums(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ set, want string }{
		{pkBase + "userVerification=maybe", "userVerification must be"},
		{pkBase + "requireResidentKey=required", "requireResidentKey must be"},
		{pkBase + "attestation=enterprise", "attestation must be"},
		{pkBase + "authenticatorAttachment=usb", "authenticatorAttachment must be"},
		{pkBase + "signatureAlgorithms={ES999}", "unknown algorithm"},
		{pkBase + "signatureAlgorithms=null", "at least one algorithm"},
		{pkBase + "rpName=a'b", "rpName must be"},
	} {
		out := pkFail(t, pkOn, pkURL, tc.set)
		if !strings.Contains(out, tc.want) {
			t.Errorf("%s: want %q, got:\n%s", tc.set, tc.want, out)
		}
	}
}

// The k3s overlay turns the policy on (its live factor is passkey).
func TestKeycloakPasskeys_K3sOverlay(t *testing.T) {
	t.Parallel()
	out := helmTemplateShowOnlyWithValues(t, firstAdminJob, []string{k3sOverlay})
	block := pkBlock(t, out)
	if !strings.Contains(block, "RpId=auth.clonk.tv ") {
		t.Errorf("k3s RP ID must be the host of hostnameUrl (auth.clonk.tv):\n%s", block)
	}
}
