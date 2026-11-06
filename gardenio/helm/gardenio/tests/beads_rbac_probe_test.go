package tests

import (
	"strings"
	"testing"
)

// beads.rbacProbe.enabled (gd-g9zzRfy1WL) gives the beads daemon a read-only
// RBAC ServiceAccount for the spawn permission snapshot probe (PR #1200,
// gd-SyEYkqe601). The grant must stay exactly get/list on the four RBAC kinds:
// an impersonate grant would hand the daemon pods/exec wherever gc-k8s-live
// reaches.

const beadsRBACTemplate = "templates/beads/rbac.yaml"

func TestBeadsRBACProbe_DefaultOff(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateGardenio(t, beadsRBACTemplate)
	rendersNothing(t, out, err)

	dep := helmBeadsDeployment(t)
	if !strings.Contains(dep, "automountServiceAccountToken: false") {
		t.Errorf("default beads pod must keep automountServiceAccountToken: false")
	}
	for _, s := range []string{"serviceAccountName:", "POD_NAMESPACE"} {
		if strings.Contains(dep, s) {
			t.Errorf("default beads pod must not render %q", s)
		}
	}
}

func TestBeadsRBACProbe_On_ReadOnlyClusterRole(t *testing.T) {
	t.Parallel()
	out := helmTemplateNS(t, "gardenio", beadsRBACTemplate, "beads.rbacProbe.enabled=true")
	for _, want := range []string{
		"kind: ServiceAccount\nmetadata:\n  name: test-beads\n",
		"automountServiceAccountToken: false",
		"kind: ClusterRole\nmetadata:\n  name: test-beads-rbac-reader\n",
		`- apiGroups: ["rbac.authorization.k8s.io"]`,
		`resources: ["roles", "rolebindings", "clusterroles", "clusterrolebindings"]`,
		`verbs: ["get", "list"]`,
		"kind: ClusterRoleBinding",
		"    name: test-beads\n    namespace: gardenio\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rbac render missing %q:\n%s", want, out)
		}
	}
	if n := strings.Count(out, "apiGroups:"); n != 1 {
		t.Errorf("ClusterRole must carry exactly one rule, got %d", n)
	}
	var spec []string
	for l := range strings.SplitSeq(out, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(l), "#") {
			spec = append(spec, l)
		}
	}
	for _, banned := range []string{"impersonate", "secrets", "watch", "escalate", `"bind"`, `"*"`} {
		if strings.Contains(strings.Join(spec, "\n"), banned) {
			t.Errorf("rbac render must not mention %q:\n%s", banned, out)
		}
	}
}

func TestBeadsRBACProbe_On_DeploymentMountsToken(t *testing.T) {
	t.Parallel()
	dep := helmBeadsDeployment(t, "--set", "beads.rbacProbe.enabled=true")
	for _, want := range []string{
		"serviceAccountName: test-beads\n      automountServiceAccountToken: true",
		"- name: POD_NAMESPACE",
		"fieldPath: metadata.namespace",
	} {
		if !strings.Contains(dep, want) {
			t.Errorf("beads Deployment missing %q", want)
		}
	}
	if strings.Contains(dep, "automountServiceAccountToken: false") {
		t.Errorf("probe-on beads pod must not also render automountServiceAccountToken: false")
	}
}

func TestBeadsRBACProbe_EgressAllowsAPIServerOnlyWhenOn(t *testing.T) {
	t.Parallel()
	base := []string{
		"networkPolicies.enabled=true",
		"networkPolicies.defaultDenyEgress=true",
		"networkPolicies.apiServerPostDNAT=true",
		"networkPolicies.controller.kubeApiServer.cidrs={10.100.0.1/32}",
	}
	beadsEgress := func(out string) string {
		for doc := range strings.SplitSeq(out, "\n---\n") {
			if strings.Contains(doc, "name: test-beads-egress\n") {
				return doc
			}
		}
		t.Fatalf("beads-egress NetworkPolicy not rendered:\n%s", out)
		return ""
	}

	off := beadsEgress(helmTemplate(t, "templates/networkpolicies/default-deny-egress.yaml", base...))
	if strings.Contains(off, "10.100.0.1/32") || strings.Contains(off, "port: 6443") {
		t.Errorf("probe-off beads egress must not reach the API server:\n%s", off)
	}

	on := beadsEgress(helmTemplate(t, "templates/networkpolicies/default-deny-egress.yaml",
		append(base, "beads.rbacProbe.enabled=true")...))
	for _, want := range []string{"cidr: 10.100.0.1/32", "port: 6443"} {
		if !strings.Contains(on, want) {
			t.Errorf("probe-on beads egress missing %q:\n%s", want, on)
		}
	}
	if strings.Contains(on, "0.0.0.0/0") {
		t.Errorf("beads egress must stay internet-free:\n%s", on)
	}
}
