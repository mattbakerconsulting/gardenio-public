package tests

import (
	"strings"
	"testing"
)

// Bead-scoped S3 session-credential vending env wiring for the lambda_microvm
// backend (S4 ka-0000006612; blocker ka-0000001013). The chart contract:
//   - sessionVendorRoleARN unset renders no MICROVM_SESSION_VENDOR_ROLE_ARN, so
//     the controller leaves microvmSessionVendor=nil and satellites get no S3
//     session credential (the MINIMAL execution role grants none) — which is
//     exactly the state that made multi-turn resume DEAD in prod
//     (ka-0000001013: every transcript upload/restore failed "no EC2 IMDS role").
//   - sessionVendorRoleARN set renders the env so the controller vends
//     bead-scoped creds and coop can read/write the sessions bucket.
//   - sessionCredentialDurationSeconds renders only when non-zero.

// TestMicrovmSessionVendor_DefaultOff: enabling the microvm backend without a
// session vendor role must render no MICROVM_SESSION_VENDOR_ROLE_ARN.
func TestMicrovmSessionVendor_DefaultOff(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, microvmTpl, microvmBaseFlags()...)
	for _, forbidden := range []string{
		"MICROVM_SESSION_VENDOR_ROLE_ARN",
		"MICROVM_SESSION_CREDENTIAL_DURATION_SECONDS",
	} {
		if strings.Contains(out, forbidden) {
			t.Errorf("%s must NOT render without agents.microvm.sessionVendorRoleARN; got:\n%s", forbidden, out)
		}
	}
}

// TestMicrovmSessionVendor_EnabledRendersARN: setting the vendor role renders the
// env var carrying it verbatim (the fix for ka-0000001013).
func TestMicrovmSessionVendor_EnabledRendersARN(t *testing.T) {
	t.Parallel()
	const arn = "arn:aws:iam::123456789012:role/gardenio-prod-microvm-session-vendor"
	out := helmTemplate(t, microvmTpl, microvmBaseFlags(
		"agents.microvm.sessionVendorRoleARN="+arn,
	)...)
	if !strings.Contains(out, "name: MICROVM_SESSION_VENDOR_ROLE_ARN") {
		t.Errorf("sessionVendorRoleARN must render MICROVM_SESSION_VENDOR_ROLE_ARN; got:\n%s", out)
	}
	if !strings.Contains(out, arn) {
		t.Errorf("MICROVM_SESSION_VENDOR_ROLE_ARN must carry the ARN %q; got:\n%s", arn, out)
	}
	// Credential-duration env stays unset unless explicitly configured.
	if strings.Contains(out, "MICROVM_SESSION_CREDENTIAL_DURATION_SECONDS") {
		t.Errorf("MICROVM_SESSION_CREDENTIAL_DURATION_SECONDS must NOT render at default 0; got:\n%s", out)
	}
}

// TestMicrovmSessionVendor_CredentialDuration: a non-zero duration renders its env.
func TestMicrovmSessionVendor_CredentialDuration(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, microvmTpl, microvmBaseFlags(
		"agents.microvm.sessionVendorRoleARN=arn:aws:iam::1:role/vendor",
		"agents.microvm.sessionCredentialDurationSeconds=3600",
	)...)
	for _, want := range []string{
		"name: MICROVM_SESSION_CREDENTIAL_DURATION_SECONDS",
		`value: "3600"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("sessionCredentialDurationSeconds must render %q; got:\n%s", want, out)
		}
	}
}
