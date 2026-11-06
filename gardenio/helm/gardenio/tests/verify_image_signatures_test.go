package tests

import (
	"strings"
	"testing"
)

// Kyverno verifyImages ClusterPolicy that REQUIRES a valid cosign keyless
// signature on every gardenio release image before admission — the admission
// half of image signing (ka-0000007951). Signing runs in the images.yml
// workflow on calver tag pushes (cosign keyless, by digest, with the job's
// GitHub OIDC identity, gd-oHQrFnsQ39); this policy is the enforcement that
// denies unsigned/mis-signed release images at admission time, closing
// attacker path A6.
//
// DEFAULT OFF: the policy is a Kyverno CRD AND needs the Kyverno admission
// controller installed. prod-gardenio-eks does not run Kyverno today, so an
// unconditional render would break `helm upgrade` on a missing CRD. Nothing
// renders unless global.imageSigning.verify.enabled=true.

const verifyImagesTemplate = "templates/kyverno/verify-image-signatures.yaml"

// The default signer: the images.yml workflow at a calver tag ref. [.] keeps
// the regex free of backslashes.
const verifySignerRegExp = "^https://github[.]com/mattbakerconsulting/gardenio/[.]github/workflows/images[.]yml@refs/tags/20.*$"

func TestVerifyImageSignatures_DefaultDisabled(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, verifyImagesTemplate)
	if err == nil {
		if strings.Contains(out, "kind: ClusterPolicy") {
			t.Errorf("ClusterPolicy must NOT render when imageSigning.verify.enabled=false, got:\n%s", out)
		}
		return
	}
	// helm exits non-zero with "could not find template" when --show-only
	// matches a template that renders to nothing (the gated-off case).
	if !strings.Contains(err.Error(), "could not find template") &&
		!strings.Contains(out, "could not find template") {
		t.Errorf("unexpected helm failure: %v\n%s", err, out)
	}
}

func TestVerifyImageSignatures_EnabledRendersPolicy(t *testing.T) {
	t.Parallel()
	out := helmTemplateNS(t, "gardenio", verifyImagesTemplate,
		"global.imageSigning.verify.enabled=true",
	)
	if !strings.Contains(out, "kind: ClusterPolicy") {
		t.Fatalf("ClusterPolicy must render when enabled=true, got:\n%s", out)
	}
	if !strings.Contains(out, "apiVersion: kyverno.io/v1") {
		t.Errorf("policy must be a kyverno.io/v1 resource, got:\n%s", out)
	}
	// The load-bearing verifyImages rule + keyless attestor.
	if !strings.Contains(out, "verifyImages:") {
		t.Errorf("policy must carry a verifyImages rule, got:\n%s", out)
	}
	if !strings.Contains(out, "keyless:") {
		t.Errorf("policy must use a keyless attestor (Fulcio/Rekor), got:\n%s", out)
	}
	// The signing identity ships as the default: the images.yml workflow's
	// GitHub OIDC identity at a calver tag ref.
	if !strings.Contains(out, "issuer: \"https://token.actions.githubusercontent.com\"") {
		t.Errorf("keyless attestor must pin issuer=https://token.actions.githubusercontent.com, got:\n%s", out)
	}
	if !strings.Contains(out, `subjectRegExp: "`+verifySignerRegExp+`"`) {
		t.Errorf("keyless attestor must match the images.yml calver-tag subject, got:\n%s", out)
	}
	// Security-hardening defaults.
	if !strings.Contains(out, "validationFailureAction: Enforce") {
		t.Errorf("policy must default to validationFailureAction: Enforce, got:\n%s", out)
	}
	if !strings.Contains(out, "failurePolicy: Fail") {
		t.Errorf("policy must fail CLOSED (failurePolicy: Fail) by default, got:\n%s", out)
	}
	if !strings.Contains(out, "verifyDigest: true") {
		t.Errorf("policy must require verifyDigest by default, got:\n%s", out)
	}
}

// The policy must gate ONLY our release repos — third-party images (postgres,
// nats, kyverno) are not signed by our CI and would be wrongly denied if the
// imageReferences glob were too broad.
// The registry defaults to global.imageRegistry, where images.yml publishes.
func TestVerifyImageSignatures_ScopedToReleaseRegistry(t *testing.T) {
	t.Parallel()
	out := helmTemplateNS(t, "gardenio", verifyImagesTemplate,
		"global.imageSigning.verify.enabled=true",
		"global.imageRegistry=123456789012.dkr.ecr.us-east-1.amazonaws.com/gardenio",
	)
	if !strings.Contains(out, `- "123456789012.dkr.ecr.us-east-1.amazonaws.com/gardenio/*"`) {
		t.Errorf("imageReferences must default to the global.imageRegistry glob, got:\n%s", out)
	}
}

// The pull-through mirror repo (<registry>/mirror/*) co-locates under our
// registry but is NEVER signed by our CI (postgres, nats, vmagent, victoria-*,
// …). Kyverno image globs treat "*" as matching "/", so the imageReferences
// default (<registry>/*) ALSO catches <registry>/mirror/*; without a
// skipImageReferences entry an Enforce rollout would DENY every mirrored
// dependency running in gardenio-system and take prod down (ka-0000008191).
// cache/* (build caches, the controller-base stage) is never signed either.
func TestVerifyImageSignatures_SkipsMirrorRepo(t *testing.T) {
	t.Parallel()
	out := helmTemplateNS(t, "gardenio", verifyImagesTemplate,
		"global.imageSigning.verify.enabled=true",
	)
	if !strings.Contains(out, "skipImageReferences:") {
		t.Fatalf("policy must carry skipImageReferences (mirror/* would be DENIED), got:\n%s", out)
	}
	for _, repo := range []string{"mirror", "cache"} {
		if !strings.Contains(out, "ghcr.io/mattbakerconsulting/gardenio/"+repo+"/*") {
			t.Errorf("skipImageReferences must exempt the <registry>/%s/* repo, got:\n%s", repo, out)
		}
	}
}

// The E2E coverage namespaces (which legitimately run unsigned -cover variants)
// must be excluded, or this policy would deny the very pods the coverage shard
// needs — mirrors the disallow-cover-images allowlist.
func TestVerifyImageSignatures_ExcludesE2ENamespaces(t *testing.T) {
	t.Parallel()
	out := helmTemplateNS(t, "gardenio", verifyImagesTemplate,
		"global.imageSigning.verify.enabled=true",
	)
	for _, ns := range []string{
		"gardenio-e2e",
		"gardenio-e2e-*",
	} {
		if !strings.Contains(out, "- "+ns+"\n") {
			t.Errorf("exclude list must exempt namespace %q (E2E unsigned/-cover pods), got:\n%s", ns, out)
		}
	}
	if strings.Contains(out, "gardenio-rwx") {
		t.Errorf("exclude list must not exempt the retired gardenio-rwx namespaces, got:\n%s", out)
	}
}

// The action is overridable for a dry-run (Audit) rollout, and the identity is
// pinnable to an exact SAN.
func TestVerifyImageSignatures_Overridable(t *testing.T) {
	t.Parallel()
	out := helmTemplateNS(t, "gardenio", verifyImagesTemplate,
		"global.imageSigning.verify.enabled=true",
		"global.imageSigning.verify.validationFailureAction=Audit",
		"global.imageSigning.verify.certificateIdentity=https://github.com/mattbakerconsulting/gardenio/.github/workflows/images.yml@refs/tags/2026.216.1",
	)
	if !strings.Contains(out, "validationFailureAction: Audit") {
		t.Errorf("validationFailureAction must be overridable to Audit, got:\n%s", out)
	}
	// An exact certificateIdentity switches subject → subjectRegExp off.
	if !strings.Contains(out, "subject: \"https://github.com/mattbakerconsulting/gardenio/.github/workflows/images.yml@refs/tags/2026.216.1\"") {
		t.Errorf("exact certificateIdentity must render as subject:, got:\n%s", out)
	}
	if strings.Contains(out, "subjectRegExp:") {
		t.Errorf("exact certificateIdentity must NOT also render subjectRegExp, got:\n%s", out)
	}
}

// Blanking the signer identity must fail the render rather than admit images
// against an empty subject.
func TestVerifyImageSignatures_EnabledWithoutIdentityFails(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, verifyImagesTemplate,
		"global.imageSigning.verify.enabled=true",
		"global.imageSigning.verify.certificateIdentityRegExp=",
	)
	if err == nil {
		t.Fatalf("enabled=true with no certificateIdentity/certificateIdentityRegExp must fail, got:\n%s", out)
	}
	if !strings.Contains(out, "no signer identity is set") {
		t.Errorf("failure must name the missing signer identity, got: %v\n%s", err, out)
	}
}
