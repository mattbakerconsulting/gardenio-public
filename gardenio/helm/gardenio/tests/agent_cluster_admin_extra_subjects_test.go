package tests

import (
	"strings"
	"testing"
)

// Regression guards for gd-kfkeH73SBt (operator escalation ka-0000004109).
//
// agents.clusterAdmin.extraSubjects appends additional subjects to the
// cluster-admin ClusterRoleBinding. This lets operators bind the *default*
// agent SA (`<release>-agent`) — the SA normal agent pods run as — to
// cluster-admin without changing which SA pods select, so agents can read
// pods/pods-log/events cluster-wide (incl. the privileged gardenio-gvisor
// namespace). The dedicated cluster-admin SA is always bound regardless.

func TestClusterAdminExtraSubjects_DefaultEmpty(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/controller/agent-cluster-rbac.yaml",
		"agents.enabled=true",
		"agents.clusterAdmin.enabled=true",
	)
	// The dedicated cluster-admin SA must always be bound.
	if !strings.Contains(out, "test-agent-cluster-admin") {
		t.Fatalf("cluster-admin ClusterRoleBinding must bind the dedicated cluster-admin SA; got:\n%s", out)
	}
	// With no extraSubjects, the default agent SA must NOT appear.
	if strings.Contains(out, "name: test-agent\n") {
		t.Errorf("default agent SA must NOT be bound when extraSubjects is empty; got:\n%s", out)
	}
}

func TestClusterAdminExtraSubjects_AppendsAgentSA(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/controller/agent-cluster-rbac.yaml",
		"agents.enabled=true",
		"agents.clusterAdmin.enabled=true",
		"agents.clusterAdmin.extraSubjects[0].kind=ServiceAccount",
		"agents.clusterAdmin.extraSubjects[0].name=test-agent",
		"agents.clusterAdmin.extraSubjects[0].namespace=gardenio",
	)
	// The dedicated cluster-admin SA must still be bound.
	if !strings.Contains(out, "test-agent-cluster-admin") {
		t.Fatalf("cluster-admin ClusterRoleBinding must still bind the dedicated cluster-admin SA; got:\n%s", out)
	}
	// The extra subject (default agent SA) must now appear in the binding.
	if !strings.Contains(out, "name: test-agent") {
		t.Errorf("extraSubjects must render the default agent SA into the cluster-admin ClusterRoleBinding; got:\n%s", out)
	}
	if !strings.Contains(out, "namespace: gardenio") {
		t.Errorf("extraSubjects subject namespace must render; got:\n%s", out)
	}
}

func TestClusterAdminExtraSubjects_OmittedWhenClusterAdminDisabled(t *testing.T) {
	t.Parallel()
	// Render the full chart (no --show-only): the whole agent-cluster-rbac
	// template is gated on clusterAdmin.enabled, so with it false the
	// cluster-admin ClusterRoleBinding must not appear even if extraSubjects
	// is set.
	out := helmTemplate(t, "",
		"agents.enabled=true",
		"agents.clusterAdmin.enabled=false",
		"agents.clusterAdmin.extraSubjects[0].kind=ServiceAccount",
		"agents.clusterAdmin.extraSubjects[0].name=test-agent",
		"agents.clusterAdmin.extraSubjects[0].namespace=gardenio",
	)
	if strings.Contains(out, "name: test-agent-cluster-admin") {
		t.Errorf("no cluster-admin ClusterRoleBinding should render when clusterAdmin.enabled=false; got match in:\n%s", out)
	}
}
