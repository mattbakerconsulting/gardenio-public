package tests

import (
	"strings"
	"testing"
)

// The fleet-manager ValidatingAdmissionPolicy scopes the controller SA's
// namespaced-resource CREATE verbs (SA / Secret / RBAC / ExternalSecret) to
// namespaces stamped managed-by=gardenio-fleet, exempting only the controller's
// own substrate namespaces via the not-controller-home-namespace-create
// matchCondition.
//
// The gVisor namespace is created by Helm (managed-by=Helm), NOT by
// `gb fleet create`, yet the secretreconciler mirrors every project/account
// ExternalSecret into it (extraNamespaces, ka-0000007922). Without exempting it
// from the VAP, every mirror CREATE is denied and any runsc-routed agent defers
// forever on "secret(s) not yet synced" — i.e. fails to spawn (ka-0000000416).
// These tests pin the exemption so a future refactor cannot silently re-block
// the gVisor mirror.

const fleetVAPBase = "agents.enabled=true,agents.fleetManager.enabled=true,agents.fleetManager.admissionPolicy.enabled=true"

// vapMatchExpr returns the not-controller-home-namespace-create matchCondition
// expression text from the rendered ValidatingAdmissionPolicy.
func vapMatchExpr(t *testing.T, rendered string) string {
	t.Helper()
	const marker = "name: not-controller-home-namespace-create"
	idx := strings.Index(rendered, marker)
	if idx < 0 {
		t.Fatalf("matchCondition not-controller-home-namespace-create not found in:\n%s", rendered)
	}
	// Grab a slice large enough to span the expression block that follows.
	tail := rendered[idx:]
	if len(tail) > 600 {
		tail = tail[:600]
	}
	return tail
}

func TestFleetManagerVAP_ExemptsGvisorNamespace(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/controller/fleet-manager-admission-policy.yaml",
		fleetVAPBase,
		"gvisor.enabled=true",
		"gvisor.namespace.name=gardenio-gvisor",
	)
	expr := vapMatchExpr(t, out)
	if !strings.Contains(expr, "request.namespace != 'gardenio-gvisor'") {
		t.Errorf("VAP must exempt the gVisor namespace from fleet-manager CREATE checks, got:\n%s", expr)
	}
}

func TestFleetManagerVAP_NoGvisorClauseWhenUnset(t *testing.T) {
	t.Parallel()
	// With no gVisor namespace configured the expression must reduce to the
	// original home-namespace-only form — no dangling/empty namespace clause.
	// The release namespace is pinned: an unpinned render inherits whatever
	// namespace the ambient environment supplies (kube context / in-cluster
	// SA namespace), which made this assertion environment-dependent
	// (ka-0000003495).
	out := helmTemplateNS(t, "gardenio", "templates/controller/fleet-manager-admission-policy.yaml",
		fleetVAPBase,
	)
	expr := vapMatchExpr(t, out)
	if strings.Contains(expr, "gardenio-gvisor") {
		t.Errorf("VAP must not reference the gVisor namespace when it is unset, got:\n%s", expr)
	}
	if !strings.Contains(expr, "request.namespace != 'gardenio'") {
		t.Errorf("VAP must still exempt the release namespace, got:\n%s", expr)
	}
}
