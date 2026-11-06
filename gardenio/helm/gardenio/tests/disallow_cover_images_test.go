package tests

import (
	"strings"
	"testing"
)

// Kyverno ClusterPolicy that denies coverage-instrumented (*-cover) images
// outside the E2E namespaces — W2 §6.2 (ka-0000003442), coupled with the
// -cover image OCI label + promote refusal (ka-0000001728).
//
// DEFAULT OFF: the policy is a Kyverno CRD and prod-gardenio-eks does not run
// Kyverno, so an unconditional render would break `helm upgrade` on a missing
// CRD. Nothing renders unless global.coverage.disallowCoverImages.enabled=true.

const disallowCoverTemplate = "templates/kyverno/disallow-cover-images.yaml"

func TestDisallowCoverImages_DefaultDisabled(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, disallowCoverTemplate)
	if err == nil {
		if strings.Contains(out, "kind: ClusterPolicy") {
			t.Errorf("ClusterPolicy must NOT render when disallowCoverImages.enabled=false, got:\n%s", out)
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

func TestDisallowCoverImages_EnabledRendersPolicy(t *testing.T) {
	t.Parallel()
	out := helmTemplateNS(t, "gardenio", disallowCoverTemplate,
		"global.coverage.disallowCoverImages.enabled=true",
	)
	if !strings.Contains(out, "kind: ClusterPolicy") {
		t.Fatalf("ClusterPolicy must render when enabled=true, got:\n%s", out)
	}
	if !strings.Contains(out, "apiVersion: kyverno.io/v1") {
		t.Errorf("policy must be a kyverno.io/v1 resource, got:\n%s", out)
	}
	// W2 §6.2 mandates hard Enforce by default.
	if !strings.Contains(out, "validationFailureAction: Enforce") {
		t.Errorf("policy must default to validationFailureAction: Enforce, got:\n%s", out)
	}
	// The isolation regex that matches any -cover repo suffix.
	if !strings.Contains(out, "-cover(:[^/@]+)?") {
		t.Errorf("policy must carry the -cover image regex, got:\n%s", out)
	}
}

// The load-bearing bit: the allowlist MUST exempt BOTH the singleton E2E
// namespace AND its per-instance variants (gardenio-e2e-<id>, including the
// gardenio-e2e-0/1 lane pool). Missing the `-*` globs would make the policy wrongly DENY
// the very pods the E2E coverage shard needs to run (ka-0000001728).
func TestDisallowCoverImages_AllowlistCoversEphemeralNamespaces(t *testing.T) {
	t.Parallel()
	out := helmTemplateNS(t, "gardenio", disallowCoverTemplate,
		"global.coverage.disallowCoverImages.enabled=true",
	)
	for _, ns := range []string{
		"gardenio-e2e",
		"gardenio-e2e-*",
	} {
		if !strings.Contains(out, "- "+ns+"\n") {
			t.Errorf("allowlist must exempt namespace %q (ephemeral E2E coverage namespaces), got:\n%s", ns, out)
		}
	}
	if strings.Contains(out, "gardenio-rwx") {
		t.Errorf("allowlist must not exempt the retired gardenio-rwx namespaces, got:\n%s", out)
	}
}

// The action is overridable for a dry-run (Audit) rollout.
func TestDisallowCoverImages_ActionOverridable(t *testing.T) {
	t.Parallel()
	out := helmTemplateNS(t, "gardenio", disallowCoverTemplate,
		"global.coverage.disallowCoverImages.enabled=true",
		"global.coverage.disallowCoverImages.validationFailureAction=Audit",
	)
	if !strings.Contains(out, "validationFailureAction: Audit") {
		t.Errorf("validationFailureAction must be overridable to Audit, got:\n%s", out)
	}
}
