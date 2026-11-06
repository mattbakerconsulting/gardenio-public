package tests

import (
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// Regression guards for gd-3Xa9PZgybq (operator decision gd-lUsRL07kKO): the
// infra-reader identity's namespaced reads (templates/controller/
// infra-reader-rbac.yaml). get/list only on rolebindings, ingresses and
// networkpolicies in the release namespace, and on keycloakclients in the
// keycloak operator namespace; and (gd-6FEbhRnAkd, escalation gd-vdwY0sszvu)
// on the actions.github.com runner sets and runners in the ARC runner
// namespace, only when it is set. Nothing widens the cluster-wide ClusterRole,
// and secrets never appear.

const infraReaderNS = "gardenio"

type infraReaderObject struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name      string            `json:"name"`
		Namespace string            `json:"namespace"`
		Labels    map[string]string `json:"labels"`
	} `json:"metadata"`
	Rules   []rbacPolicyRule `json:"rules"`
	RoleRef struct {
		APIGroup string `json:"apiGroup"`
		Kind     string `json:"kind"`
		Name     string `json:"name"`
	} `json:"roleRef"`
	Subjects []struct {
		Kind      string `json:"kind"`
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"subjects"`
}

func renderInfraReaderChart(t *testing.T, flags ...string) []infraReaderObject {
	t.Helper()
	out := helmTemplateFullNamespaced(t, infraReaderNS, nil, append([]string{"agents.enabled=true"}, flags...)...)
	var objs []infraReaderObject
	for doc := range strings.SplitSeq(out, "\n---") {
		k := kindLineRE.FindStringSubmatch(doc)
		if k == nil {
			continue
		}
		switch k[1] {
		case "ServiceAccount", "ClusterRole", "ClusterRoleBinding", "Role", "RoleBinding":
		default:
			continue
		}
		var o infraReaderObject
		if err := yaml.Unmarshal([]byte(doc), &o); err != nil {
			t.Fatalf("decode rendered %s: %v\n%s", k[1], err, doc)
		}
		objs = append(objs, o)
	}
	return objs
}

// infraReaderObjects returns the objects of one kind named after the SA,
// keyed by namespace ("" for cluster-scoped).
func infraReaderObjects(t *testing.T, objs []infraReaderObject, kind, name string) map[string]infraReaderObject {
	t.Helper()
	out := map[string]infraReaderObject{}
	for _, o := range objs {
		if o.Kind != kind || o.Metadata.Name != name {
			continue
		}
		if _, dup := out[o.Metadata.Namespace]; dup {
			t.Fatalf("duplicate %s %s/%s", kind, o.Metadata.Namespace, name)
		}
		out[o.Metadata.Namespace] = o
	}
	return out
}

var (
	wantReleaseNSRules = []rbacPolicyRule{
		{APIGroups: []string{"rbac.authorization.k8s.io"}, Resources: []string{"rolebindings"}, Verbs: []string{"get", "list"}},
		{APIGroups: []string{"networking.k8s.io"}, Resources: []string{"ingresses", "networkpolicies"}, Verbs: []string{"get", "list"}},
	}
	wantKeycloakNSRules = []rbacPolicyRule{
		{APIGroups: []string{"v1.edp.epam.com"}, Resources: []string{"keycloakclients"}, Verbs: []string{"get", "list"}},
	}
	wantArcRunnersNSRules = []rbacPolicyRule{
		{APIGroups: []string{"actions.github.com"}, Resources: []string{"autoscalingrunnersets", "ephemeralrunnersets", "ephemeralrunners"}, Verbs: []string{"get", "list"}},
	}
)

func assertInfraReaderBinding(t *testing.T, rb infraReaderObject, ns string) {
	t.Helper()
	if rb.RoleRef.APIGroup != "rbac.authorization.k8s.io" || rb.RoleRef.Kind != "Role" || rb.RoleRef.Name != "gardenio-infra-reader" {
		t.Errorf("RoleBinding %s roleRef = %+v, want Role/gardenio-infra-reader", ns, rb.RoleRef)
	}
	if len(rb.Subjects) != 1 {
		t.Fatalf("RoleBinding %s: expected exactly 1 subject, got %+v", ns, rb.Subjects)
	}
	if s := rb.Subjects[0]; s.Kind != "ServiceAccount" || s.Name != "gardenio-infra-reader" || s.Namespace != infraReaderNS {
		t.Errorf("RoleBinding %s subject = %+v, want ServiceAccount %s/gardenio-infra-reader", ns, s, infraReaderNS)
	}
	if got := rb.Metadata.Labels["gardenio.io/access-boundary"]; got != "infra-reader" {
		t.Errorf("RoleBinding %s label gardenio.io/access-boundary = %q, want infra-reader", ns, got)
	}
}

func TestInfraReader_NamespacedRolesExactRules(t *testing.T) {
	t.Parallel()
	objs := renderInfraReaderChart(t, "agents.infraReader.enabled=true")

	roles := infraReaderObjects(t, objs, "Role", "gardenio-infra-reader")
	if len(roles) != 2 {
		t.Fatalf("expected Roles in exactly %s and operators, got namespaces %v", infraReaderNS, reflect.ValueOf(roles).MapKeys())
	}
	for ns, want := range map[string][]rbacPolicyRule{infraReaderNS: wantReleaseNSRules, "operators": wantKeycloakNSRules} {
		r, ok := roles[ns]
		if !ok {
			t.Errorf("missing Role gardenio-infra-reader in %s", ns)
			continue
		}
		if !reflect.DeepEqual(r.Rules, want) {
			t.Errorf("Role %s/gardenio-infra-reader rules =\n%+v\nwant\n%+v", ns, r.Rules, want)
		}
		if got := r.Metadata.Labels["gardenio.io/access-boundary"]; got != "infra-reader" {
			t.Errorf("Role %s label gardenio.io/access-boundary = %q, want infra-reader", ns, got)
		}
	}

	rbs := infraReaderObjects(t, objs, "RoleBinding", "gardenio-infra-reader")
	if len(rbs) != 2 {
		t.Fatalf("expected RoleBindings in exactly %s and operators, got %d", infraReaderNS, len(rbs))
	}
	for _, ns := range []string{infraReaderNS, "operators"} {
		rb, ok := rbs[ns]
		if !ok {
			t.Errorf("missing RoleBinding gardenio-infra-reader in %s", ns)
			continue
		}
		assertInfraReaderBinding(t, rb, ns)
	}
}

// Every rule the identity holds, namespaced or cluster-wide: no secrets, no
// write or watch verb on the new grants, no RBAC-definition objects, no
// wildcards.
func TestInfraReader_NeverSecretsWritesOrWildcards(t *testing.T) {
	t.Parallel()
	objs := renderInfraReaderChart(t, "agents.infraReader.enabled=true", "agents.infraReader.arcRunnersNamespace=arc-runners")
	seen := 0
	for _, o := range objs {
		if o.Metadata.Name != "gardenio-infra-reader" || (o.Kind != "Role" && o.Kind != "ClusterRole") {
			continue
		}
		seen++
		for _, r := range o.Rules {
			for _, res := range r.Resources {
				switch res {
				case "secrets", "roles", "clusterroles", "clusterrolebindings", "serviceaccounts/token", "*":
					t.Errorf("%s %s/%s must not grant resource %q: %+v", o.Kind, o.Metadata.Namespace, o.Metadata.Name, res, r)
				}
			}
			for _, g := range r.APIGroups {
				if g == "*" {
					t.Errorf("%s %s must not use apiGroup \"*\": %+v", o.Kind, o.Metadata.Namespace, r)
				}
			}
			for _, v := range r.Verbs {
				allowed := v == "get" || v == "list" || (o.Kind == "ClusterRole" && v == "watch")
				if !allowed {
					t.Errorf("%s %s must not carry verb %q: %+v", o.Kind, o.Metadata.Namespace, v, r)
				}
			}
		}
	}
	if seen != 4 {
		t.Fatalf("control: expected the ClusterRole and 3 Roles named gardenio-infra-reader, saw %d", seen)
	}
}

// The new reads are namespaced on purpose: the cluster-wide ClusterRole must
// not pick them up.
func TestInfraReader_ClusterRoleNotWidened(t *testing.T) {
	t.Parallel()
	crs := infraReaderObjects(t, renderInfraReaderChart(t, "agents.infraReader.enabled=true", "agents.infraReader.arcRunnersNamespace=arc-runners"), "ClusterRole", "gardenio-infra-reader")
	cr, ok := crs[""]
	if !ok || len(cr.Rules) == 0 {
		t.Fatal("control: expected the gardenio-infra-reader ClusterRole with rules")
	}
	for _, r := range cr.Rules {
		for _, res := range r.Resources {
			switch res {
			case "rolebindings", "ingresses", "networkpolicies", "keycloakclients",
				"autoscalingrunnersets", "ephemeralrunnersets", "ephemeralrunners":
				t.Errorf("ClusterRole must not grant %q cluster-wide: %+v", res, r)
			}
		}
	}
}

func TestInfraReader_RolesAbsentByDefault(t *testing.T) {
	t.Parallel()
	objs := renderInfraReaderChart(t)
	for _, kind := range []string{"Role", "RoleBinding"} {
		if got := infraReaderObjects(t, objs, kind, "gardenio-infra-reader"); len(got) != 0 {
			t.Errorf("default render must not contain any %s gardenio-infra-reader, got %d", kind, len(got))
		}
	}
}

func TestInfraReader_KeycloakNamespaceValue(t *testing.T) {
	t.Parallel()
	objs := renderInfraReaderChart(t, "agents.infraReader.enabled=true", "agents.infraReader.keycloakClientsNamespace=keycloak-ops")
	roles := infraReaderObjects(t, objs, "Role", "gardenio-infra-reader")
	if _, ok := roles["operators"]; ok {
		t.Error("overridden namespace must not leave a Role in operators")
	}
	if r, ok := roles["keycloak-ops"]; !ok || !reflect.DeepEqual(r.Rules, wantKeycloakNSRules) {
		t.Errorf("expected the keycloakclients Role in keycloak-ops, got %+v", roles)
	}
	if rb, ok := infraReaderObjects(t, objs, "RoleBinding", "gardenio-infra-reader")["keycloak-ops"]; !ok {
		t.Error("expected a RoleBinding in keycloak-ops")
	} else {
		assertInfraReaderBinding(t, rb, "keycloak-ops")
	}

	objs = renderInfraReaderChart(t, "agents.infraReader.enabled=true", "agents.infraReader.keycloakClientsNamespace=")
	roles = infraReaderObjects(t, objs, "Role", "gardenio-infra-reader")
	if len(roles) != 1 || !reflect.DeepEqual(roles[infraReaderNS].Rules, wantReleaseNSRules) {
		t.Errorf("empty keycloakClientsNamespace must render only the release-namespace Role, got %+v", roles)
	}
	if got := len(infraReaderObjects(t, objs, "RoleBinding", "gardenio-infra-reader")); got != 1 {
		t.Errorf("empty keycloakClientsNamespace must render only the release-namespace RoleBinding, got %d", got)
	}
}

func TestInfraReader_KeycloakNamespaceMustDifferFromRelease(t *testing.T) {
	t.Parallel()
	cmd := exec.Command("helm", "template", "test", chartDir(t), "--namespace", infraReaderNS,
		"--set", "agents.enabled=true", "--set", "agents.infraReader.enabled=true",
		"--set", "agents.infraReader.keycloakClientsNamespace="+infraReaderNS)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected helm template to fail when keycloakClientsNamespace equals the release namespace")
	}
	if !strings.Contains(string(out), "keycloakClientsNamespace must differ") {
		t.Errorf("unexpected failure output:\n%s", out)
	}
}

func TestInfraReader_ArcRunnersNamespaceValue(t *testing.T) {
	t.Parallel()
	objs := renderInfraReaderChart(t, "agents.infraReader.enabled=true")
	for _, kind := range []string{"Role", "RoleBinding"} {
		if got := infraReaderObjects(t, objs, kind, "gardenio-infra-reader"); len(got) != 2 {
			t.Errorf("empty arcRunnersNamespace must render only the release and keycloak %ss, got namespaces %v", kind, reflect.ValueOf(got).MapKeys())
		}
	}

	objs = renderInfraReaderChart(t, "agents.infraReader.enabled=true", "agents.infraReader.arcRunnersNamespace=arc-runners")
	if r, ok := infraReaderObjects(t, objs, "Role", "gardenio-infra-reader")["arc-runners"]; !ok {
		t.Error("expected the ARC runner Role in arc-runners")
	} else {
		if !reflect.DeepEqual(r.Rules, wantArcRunnersNSRules) {
			t.Errorf("Role arc-runners/gardenio-infra-reader rules =\n%+v\nwant\n%+v", r.Rules, wantArcRunnersNSRules)
		}
		if got := r.Metadata.Labels["gardenio.io/access-boundary"]; got != "infra-reader" {
			t.Errorf("Role arc-runners label gardenio.io/access-boundary = %q, want infra-reader", got)
		}
	}
	if rb, ok := infraReaderObjects(t, objs, "RoleBinding", "gardenio-infra-reader")["arc-runners"]; !ok {
		t.Error("expected a RoleBinding in arc-runners")
	} else {
		assertInfraReaderBinding(t, rb, "arc-runners")
	}
}

// Every Role is named after the SA: in the release namespace the ARC rule
// joins that Role, and sharing the keycloak namespace is refused.
func TestInfraReader_ArcRunnersNamespaceSharedWithOtherRoles(t *testing.T) {
	t.Parallel()
	objs := renderInfraReaderChart(t, "agents.infraReader.enabled=true", "agents.infraReader.arcRunnersNamespace="+infraReaderNS)
	roles := infraReaderObjects(t, objs, "Role", "gardenio-infra-reader")
	if len(roles) != 2 {
		t.Fatalf("expected Roles in exactly %s and operators, got namespaces %v", infraReaderNS, reflect.ValueOf(roles).MapKeys())
	}
	if want := append(append([]rbacPolicyRule{}, wantReleaseNSRules...), wantArcRunnersNSRules...); !reflect.DeepEqual(roles[infraReaderNS].Rules, want) {
		t.Errorf("Role %s/gardenio-infra-reader rules =\n%+v\nwant\n%+v", infraReaderNS, roles[infraReaderNS].Rules, want)
	}

	cmd := exec.Command("helm", "template", "test", chartDir(t), "--namespace", infraReaderNS,
		"--set", "agents.enabled=true", "--set", "agents.infraReader.enabled=true",
		"--set", "agents.infraReader.arcRunnersNamespace=operators")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected helm template to fail when arcRunnersNamespace equals keycloakClientsNamespace")
	}
	if !strings.Contains(string(out), "arcRunnersNamespace must differ") {
		t.Errorf("unexpected failure output:\n%s", out)
	}
}
