package tests

import (
	"strings"
	"testing"
)

// ka-0000007080: the `externalSecrets.beadsToken` ExternalSecret
// (templates/external-secrets.yaml) renders `kd-beads-token` into the release
// namespace once its remoteRef is set. A stack may already create that same
// object OUTSIDE the chart (the EKS-era terraform did).
// A helm upgrade that flips the gate must be able to ADOPT that pre-existing
// object rather than collide with it — the collision is the outage class that
// wedged the prod release train ~16h on jira-webhook-secret (ka-0000001012):
//
//	cannot be imported into the current release: invalid ownership metadata;
//	missing key "app.kubernetes.io/managed-by": must be set to "Helm"
//
// Adoption works only when the LIVE object already carries Helm ownership
// metadata. Two independent surfaces must therefore agree on that metadata:
//
//  1. the chart-rendered ExternalSecret (this test), which carries
//     app.kubernetes.io/managed-by=Helm via the gardenio.labels helper and
//     renders into the release namespace, and
//  2. the terraform manifest, which now stamps the matching managed-by label +
//     meta.helm.sh/release-name/-namespace annotations so the live object is
//     already Helm-owned before any gate-flip (see the terraform fmt/validate
//     guard and the values gate-flip notes).
//
// This test pins surface (1): if a future edit drops the ownership label from
// the rendered beadsToken ExternalSecret, or moves it out of the release
// namespace, a gate-flip would stop being adoptable and re-arm the outage.
func TestBeadsTokenExternalSecret_CarriesHelmOwnership(t *testing.T) {
	t.Parallel()
	// Gate flipped, release namespace pinned (helmTemplateNS — the assertion is
	// namespace-sensitive, so it must not depend on the ambient namespace,
	// ka-0000003495).
	out := helmTemplateNS(t, "gardenio-system", "templates/external-secrets.yaml",
		"externalSecrets.enabled=true",
		"externalSecrets.secretStoreName=aws-secrets-manager",
		"externalSecrets.beadsToken.remoteRef=gardenio/kd-beads-token",
	)

	block := beadsTokenESBlock(t, out)

	// The Helm ownership label is what lets a live object be adopted (patched)
	// into the release instead of colliding on upgrade.
	if !strings.Contains(block, "app.kubernetes.io/managed-by: Helm") {
		t.Errorf("beadsToken ExternalSecret must carry app.kubernetes.io/managed-by=Helm so a gate-flip adopts the terraform-created twin cleanly (ka-0000007080), got:\n%s", block)
	}
	// It must render into the release namespace — the terraform twin lives in
	// gardenio-system, so a namespace mismatch would mean two DIFFERENT objects
	// (no adoption, and a second uncontrolled ExternalSecret).
	if !strings.Contains(block, "namespace: gardenio-system") {
		t.Errorf("beadsToken ExternalSecret must render into the release namespace (gardenio-system) to match the terraform-created twin, got:\n%s", block)
	}
	// Sanity: it is the kd-beads-token object.
	if !strings.Contains(block, "name: kd-beads-token") {
		t.Errorf("beadsToken ExternalSecret must target the kd-beads-token Secret, got:\n%s", block)
	}
}

// TestBeadsTokenExternalSecret_DormantWhenRemoteRefEmpty pins the gate itself:
// with no remoteRef the ExternalSecret must NOT render, so on a stack where
// something else creates it (gate off) that owner stays the sole owner of kd-beads-token and helm
// never touches it. This is the "disarmed" posture the fix relies on.
func TestBeadsTokenExternalSecret_DormantWhenRemoteRefEmpty(t *testing.T) {
	t.Parallel()
	// Render the whole chart (not --show-only): helm errors "could not find
	// template" when a --show-only target renders empty, which would mask the
	// assertion.
	out := helmTemplateNS(t, "gardenio-system", "",
		"externalSecrets.enabled=true",
		"externalSecrets.secretStoreName=aws-secrets-manager",
	)
	if beadsTokenESCount(out) != 0 {
		t.Fatalf("beadsToken ExternalSecret must NOT render when remoteRef is empty (gate off), got:\n%s", out)
	}
}

// beadsTokenESBlock returns the single ExternalSecret document that targets the
// kd-beads-token Secret, failing if there is not exactly one.
func beadsTokenESBlock(t *testing.T, rendered string) string {
	t.Helper()
	var found string
	n := 0
	for _, doc := range strings.Split(rendered, "\n---") {
		if strings.Contains(doc, "kind: ExternalSecret") &&
			strings.Contains(doc, "name: kd-beads-token") {
			found = doc
			n++
		}
	}
	if n != 1 {
		t.Fatalf("expected exactly 1 kd-beads-token ExternalSecret, got %d in:\n%s", n, rendered)
	}
	return found
}

// beadsTokenESCount counts ExternalSecret manifests targeting the kd-beads-token
// Secret in a full-chart render.
func beadsTokenESCount(rendered string) int {
	n := 0
	for _, doc := range strings.Split(rendered, "\n---") {
		if strings.Contains(doc, "kind: ExternalSecret") &&
			strings.Contains(doc, "name: kd-beads-token") {
			n++
		}
	}
	return n
}
