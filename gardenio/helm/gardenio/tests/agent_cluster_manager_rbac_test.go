package tests

import (
	"strings"
	"testing"
)

// Regression guards for ka-0000005117.
//
// The -cluster-manager ClusterRole grants WRITE verbs (create/update/patch/
// delete) on the cluster-admin ClusterRoleBinding so the DEPLOYER SA can manage
// that helm-owned binding during upgrades. Historically the binding also listed
// the spawned-agent-pod SA (`<release>-agent`) as a subject, so every agent pod
// held those write verbs — including `delete`.
//
// An operator measured (2026-08-25) that `delete` is NOT gated by k8s RBAC
// escalation-prevention (delete is not a grant), so any agent pod could delete
// the cluster-admin ClusterRoleBinding outright: a fleet-wide availability
// hazard with no admission check in the way. The fix binds the WRITE role to
// the deployer SA alone and gives agent pods a read-only (`get`) role instead.
//
// These tests pin that split so a future chart edit cannot silently re-grant
// agent pods write on the cluster-admin binding.

// splitClusterManagerDoc returns the two ClusterRoleBinding YAML documents
// (manager, reader) from the rendered cluster-rbac.yaml, keyed by binding name.
func renderClusterRBAC(t *testing.T) string {
	t.Helper()
	return helmTemplate(t, "templates/controller/cluster-rbac.yaml",
		"agents.enabled=true",
		"agents.clusterAdmin.enabled=true",
	)
}

// bindingSubjects extracts the subjects block text of the named
// ClusterRoleBinding from a rendered multi-doc YAML string. It returns the
// slice of the doc from the binding's roleRef/subjects onward up to the next
// document separator, which is enough to assert on the SA names present.
func bindingSubjects(t *testing.T, rendered, bindingName string) string {
	t.Helper()
	for d := range strings.SplitSeq(rendered, "\n---") {
		if !strings.Contains(d, "kind: ClusterRoleBinding") {
			continue
		}
		if !strings.Contains(d, "name: "+bindingName+"\n") {
			continue
		}
		idx := strings.Index(d, "subjects:")
		if idx < 0 {
			t.Fatalf("ClusterRoleBinding %q has no subjects block:\n%s", bindingName, d)
		}
		return d[idx:]
	}
	t.Fatalf("ClusterRoleBinding %q not found in rendered output:\n%s", bindingName, rendered)
	return ""
}

// The WRITE binding must bind ONLY the deployer SA (<release>-agents-sa) and
// must NOT bind the spawned-agent-pod SA (<release>-agent).
func TestClusterManager_WriteBindingExcludesAgentPodSA(t *testing.T) {
	t.Parallel()
	out := renderClusterRBAC(t)
	subs := bindingSubjects(t, out, "test-agents-cluster-manager")

	if !strings.Contains(subs, "name: test-agents-sa") {
		t.Errorf("cluster-manager (write) binding must bind the deployer SA test-agents-sa; got:\n%s", subs)
	}
	// The agent-pod SA is `test-agent`. Match on a full line so it does not
	// spuriously match the substring inside `test-agents-sa`.
	if strings.Contains(subs, "name: test-agent\n") {
		t.Errorf("cluster-manager (write) binding must NOT bind the agent-pod SA test-agent "+
			"(ka-0000005117: agent pods must not hold write/delete on the cluster-admin CRB); got:\n%s", subs)
	}
}

// The write ClusterRole must still carry the write verbs (the deployer needs
// them to manage the helm-owned binding) — the fix narrows WHO is bound, not
// what the deployer can do.
func TestClusterManager_WriteRoleStillHasWriteVerbs(t *testing.T) {
	t.Parallel()
	out := renderClusterRBAC(t)
	if !strings.Contains(out, `verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]`) {
		t.Errorf("cluster-manager ClusterRole must retain write verbs for the deployer SA; got:\n%s", out)
	}
}

// Agent pods get a read-only binding: it binds the agent-pod SA and grants
// ONLY `get` on the named clusterrolebinding — no write verbs.
func TestClusterReader_BindsAgentPodSAReadOnly(t *testing.T) {
	t.Parallel()
	out := renderClusterRBAC(t)
	subs := bindingSubjects(t, out, "test-agents-cluster-reader")
	if !strings.Contains(subs, "name: test-agent\n") {
		t.Errorf("cluster-reader binding must bind the agent-pod SA test-agent; got:\n%s", subs)
	}

	// Isolate the reader ClusterRole document and assert it has no write verb.
	var readerRole string
	for d := range strings.SplitSeq(out, "\n---") {
		if strings.Contains(d, "kind: ClusterRole\n") && strings.Contains(d, "name: test-agents-cluster-reader\n") {
			readerRole = d
			break
		}
	}
	if readerRole == "" {
		t.Fatalf("cluster-reader ClusterRole not found; got:\n%s", out)
	}
	for _, verb := range []string{"create", "update", "patch", "delete"} {
		if strings.Contains(readerRole, verb) {
			t.Errorf("cluster-reader ClusterRole must be read-only but contains %q verb (ka-0000005117); got:\n%s", verb, readerRole)
		}
	}
}

// Nothing renders when clusterAdmin is disabled (the whole template is gated).
func TestClusterManager_OmittedWhenClusterAdminDisabled(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "templates/controller/cluster-rbac.yaml",
		"agents.enabled=true",
		"agents.clusterAdmin.enabled=false",
	)
	// Either the template errors as empty, or it renders nothing meaningful.
	if err == nil && strings.Contains(out, "kind: ClusterRoleBinding") {
		t.Errorf("cluster-rbac must render nothing when clusterAdmin.enabled=false; got:\n%s", out)
	}
}
