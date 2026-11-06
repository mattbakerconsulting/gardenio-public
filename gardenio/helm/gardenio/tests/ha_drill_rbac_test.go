package tests

import (
	"strings"
	"testing"
)

// Regression guards for gd-JCLx5RLG1I: the HA-drill node/pod mutation
// identity (templates/controller/ha-drill-rbac.yaml). This is a NEW,
// dedicated ServiceAccount for the scripted cordon+drain proof
// (gardenio/scripts/ha-drill-drain.sh) — deliberately NOT the default agent
// SA and NOT the gc-dogfood-scoped k8s-live SA, whose policy of record bounds
// it to gc-dogfood only. These tests pin the narrow verb/resource set so a
// future chart edit cannot silently widen this SA's blast radius.

const haDrillRelNS = "gardenio"

func renderHaDrillRBAC(t *testing.T, extraFlags ...string) string {
	t.Helper()
	flags := append([]string{
		"agents.enabled=true",
		"agents.haDrillOperator.enabled=true",
	}, extraFlags...)
	return helmTemplateNS(t, haDrillRelNS, "templates/controller/ha-drill-rbac.yaml", flags...)
}

func TestHaDrillOperator_OmittedWhenDisabled(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "templates/controller/ha-drill-rbac.yaml",
		"agents.enabled=true",
		"agents.haDrillOperator.enabled=false",
	)
	if err == nil && strings.TrimSpace(out) != "" && !strings.Contains(out, "no matches") {
		t.Errorf("ha-drill-rbac must render nothing when haDrillOperator.enabled=false; got:\n%s", out)
	}
}

func TestHaDrillOperator_ServiceAccountHasNoAutomount(t *testing.T) {
	t.Parallel()
	out := renderHaDrillRBAC(t)
	if !strings.Contains(out, "kind: ServiceAccount") {
		t.Fatalf("expected a ServiceAccount; got:\n%s", out)
	}
	if !strings.Contains(out, "name: gardenio-ha-drill-operator") {
		t.Errorf("expected SA name gardenio-ha-drill-operator; got:\n%s", out)
	}
	if !strings.Contains(out, "automountServiceAccountToken: false") {
		t.Errorf("SA must not automount a token at the SA level; got:\n%s", out)
	}
}

// extractDoc returns the single YAML document (by kind + name) from a
// multi-doc rendered string.
func extractDoc(t *testing.T, rendered, kind, name string) string {
	t.Helper()
	for d := range strings.SplitSeq(rendered, "\n---") {
		if strings.Contains(d, "kind: "+kind+"\n") && strings.Contains(d, "name: "+name+"\n") {
			return d
		}
	}
	t.Fatalf("%s %q not found in rendered output:\n%s", kind, name, rendered)
	return ""
}

func TestHaDrillOperator_ClusterRoleIsNodesOnlyNoDelete(t *testing.T) {
	t.Parallel()
	out := renderHaDrillRBAC(t)
	role := extractDoc(t, out, "ClusterRole", "gardenio-ha-drill-operator")

	if !strings.Contains(role, `resources: ["nodes"]`) {
		t.Errorf("ClusterRole must scope to nodes only; got:\n%s", role)
	}
	if !strings.Contains(role, `verbs: ["get", "list", "watch", "patch"]`) {
		t.Errorf("ClusterRole must grant exactly get/list/watch/patch on nodes; got:\n%s", role)
	}
	for _, forbidden := range []string{"delete", "create", "\"pods\"", "secrets", "exec"} {
		if strings.Contains(role, forbidden) {
			t.Errorf("ClusterRole must not contain %q (nodes-only, no delete/create); got:\n%s", forbidden, role)
		}
	}
}

func TestHaDrillOperator_NamespacedRoleScopedToPodsAndEviction(t *testing.T) {
	t.Parallel()
	out := renderHaDrillRBAC(t)
	role := extractDoc(t, out, "Role", "gardenio-ha-drill-operator")

	if !strings.Contains(role, "namespace: "+haDrillRelNS+"\n") {
		t.Errorf("Role must be scoped to the release namespace %q; got:\n%s", haDrillRelNS, role)
	}
	if !strings.Contains(role, `resources: ["pods"]`) || !strings.Contains(role, `verbs: ["get", "list"]`) {
		t.Errorf("Role must grant get/list on pods; got:\n%s", role)
	}
	if !strings.Contains(role, `resources: ["pods/eviction"]`) || !strings.Contains(role, `verbs: ["create"]`) {
		t.Errorf("Role must grant create on pods/eviction; got:\n%s", role)
	}
	for _, forbidden := range []string{"delete", "\"exec\"", "attach", "portforward", "\"log\"", "secrets"} {
		if strings.Contains(role, forbidden) {
			t.Errorf("Role must not contain %q; got:\n%s", forbidden, role)
		}
	}
}

func TestHaDrillOperator_BindingsUseDedicatedSAOnly(t *testing.T) {
	t.Parallel()
	out := renderHaDrillRBAC(t)

	crb := extractDoc(t, out, "ClusterRoleBinding", "gardenio-ha-drill-operator")
	idx := strings.Index(crb, "subjects:")
	if idx < 0 {
		t.Fatalf("ClusterRoleBinding has no subjects block:\n%s", crb)
	}
	crbSubjects := crb[idx:]
	if !strings.Contains(crbSubjects, "name: gardenio-ha-drill-operator\n") {
		t.Errorf("ClusterRoleBinding must bind the dedicated SA; got:\n%s", crbSubjects)
	}
	if strings.Contains(crbSubjects, "name: gardenio-agent\n") {
		t.Errorf("ClusterRoleBinding must NOT bind the default agent SA; got:\n%s", crbSubjects)
	}

	rb := extractDoc(t, out, "RoleBinding", "gardenio-ha-drill-operator")
	idx = strings.Index(rb, "subjects:")
	if idx < 0 {
		t.Fatalf("RoleBinding has no subjects block:\n%s", rb)
	}
	rbSubjects := rb[idx:]
	if !strings.Contains(rbSubjects, "name: gardenio-ha-drill-operator\n") {
		t.Errorf("RoleBinding must bind the dedicated SA; got:\n%s", rbSubjects)
	}
	if strings.Contains(rbSubjects, "name: gardenio-agent\n") {
		t.Errorf("RoleBinding must NOT bind the default agent SA; got:\n%s", rbSubjects)
	}
}

func TestHaDrillOperator_NameOverride(t *testing.T) {
	t.Parallel()
	out := renderHaDrillRBAC(t, "agents.haDrillOperator.name=custom-ha-drill-sa")
	if !strings.Contains(out, "name: custom-ha-drill-sa") {
		t.Errorf("expected overridden SA name custom-ha-drill-sa; got:\n%s", out)
	}
	if strings.Contains(out, "gardenio-ha-drill-operator") {
		t.Errorf("default name must not leak through when overridden; got:\n%s", out)
	}
}
