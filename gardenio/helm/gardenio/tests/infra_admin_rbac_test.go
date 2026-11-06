package tests

import (
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// Regression guards for gd-sAMar9tn5B (plan gd-xiV5hyhPrG P6): the admin infra
// identity (templates/controller/infra-admin-rbac.yaml). It binds the built-in
// cluster-admin ClusterRole to ONE dedicated ServiceAccount, off by default,
// and must stay separate from agents.clusterAdmin (whose template also grants
// the deployer SA clusterrolebinding create). Design:
// gardenio/docs/infra-admin-role-design.md.

const infraAdminNS = "gardenio"

type infraAdminObject struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name      string            `json:"name"`
		Namespace string            `json:"namespace"`
		Labels    map[string]string `json:"labels"`
	} `json:"metadata"`
	AutomountServiceAccountToken *bool `json:"automountServiceAccountToken"`
	RoleRef                      struct {
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

// renderInfraAdminChart renders the WHOLE chart (not just the one template) so
// the assertions also cover any other template that might emit these objects.
func renderInfraAdminChart(t *testing.T, flags ...string) []infraAdminObject {
	t.Helper()
	out := helmTemplateFullNamespaced(t, infraAdminNS, nil, append([]string{"agents.enabled=true"}, flags...)...)
	var objs []infraAdminObject
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
		var o infraAdminObject
		if err := yaml.Unmarshal([]byte(doc), &o); err != nil {
			t.Fatalf("decode rendered %s: %v\n%s", k[1], err, doc)
		}
		objs = append(objs, o)
	}
	return objs
}

func filterInfraAdmin(objs []infraAdminObject, kind, name string) []infraAdminObject {
	var out []infraAdminObject
	for _, o := range objs {
		if o.Kind == kind && o.Metadata.Name == name {
			out = append(out, o)
		}
	}
	return out
}

// The positive control for the "absent" assertions below: the same filter must
// find the neighbouring infra-reader objects, so an always-empty filter (wrong
// kind string, wrong name) cannot make the absence checks pass.
func TestInfraAdmin_FilterFindsObjectsItShould(t *testing.T) {
	t.Parallel()
	objs := renderInfraAdminChart(t, "agents.infraReader.enabled=true")
	if got := len(filterInfraAdmin(objs, "ServiceAccount", "gardenio-infra-reader")); got != 1 {
		t.Fatalf("control: expected exactly 1 gardenio-infra-reader ServiceAccount, got %d", got)
	}
	if got := len(filterInfraAdmin(objs, "ClusterRoleBinding", "gardenio-infra-reader")); got != 1 {
		t.Fatalf("control: expected exactly 1 gardenio-infra-reader ClusterRoleBinding, got %d", got)
	}
}

func TestInfraAdmin_AbsentByDefault(t *testing.T) {
	t.Parallel()
	for _, o := range renderInfraAdminChart(t) {
		if o.Metadata.Name == "gardenio-infra-admin" {
			t.Errorf("default render must not contain %s %q", o.Kind, o.Metadata.Name)
		}
		if o.Kind == "ClusterRoleBinding" && o.RoleRef.Name == "cluster-admin" && o.Metadata.Name == "gardenio-infra-admin" {
			t.Errorf("default render must not bind cluster-admin to the infra-admin identity")
		}
	}
}

func TestInfraAdmin_EnabledRendersOneSAAndOneClusterAdminBinding(t *testing.T) {
	t.Parallel()
	objs := renderInfraAdminChart(t, "agents.infraAdmin.enabled=true")

	sas := filterInfraAdmin(objs, "ServiceAccount", "gardenio-infra-admin")
	if len(sas) != 1 {
		t.Fatalf("expected exactly 1 ServiceAccount gardenio-infra-admin, got %d", len(sas))
	}
	sa := sas[0]
	if sa.Metadata.Namespace != infraAdminNS {
		t.Errorf("ServiceAccount namespace = %q, want %q", sa.Metadata.Namespace, infraAdminNS)
	}
	if sa.AutomountServiceAccountToken == nil || *sa.AutomountServiceAccountToken {
		t.Errorf("ServiceAccount must set automountServiceAccountToken: false explicitly, got %v", sa.AutomountServiceAccountToken)
	}
	if got := sa.Metadata.Labels["gardenio.io/access-boundary"]; got != "infra-admin" {
		t.Errorf("ServiceAccount label gardenio.io/access-boundary = %q, want infra-admin", got)
	}

	crbs := filterInfraAdmin(objs, "ClusterRoleBinding", "gardenio-infra-admin")
	if len(crbs) != 1 {
		t.Fatalf("expected exactly 1 ClusterRoleBinding gardenio-infra-admin, got %d", len(crbs))
	}
	crb := crbs[0]
	if crb.RoleRef.Kind != "ClusterRole" || crb.RoleRef.Name != "cluster-admin" || crb.RoleRef.APIGroup != "rbac.authorization.k8s.io" {
		t.Errorf("roleRef = %+v, want rbac.authorization.k8s.io ClusterRole/cluster-admin", crb.RoleRef)
	}
	if len(crb.Subjects) != 1 {
		t.Fatalf("expected exactly 1 subject, got %+v", crb.Subjects)
	}
	if s := crb.Subjects[0]; s.Kind != "ServiceAccount" || s.Name != "gardenio-infra-admin" || s.Namespace != infraAdminNS {
		t.Errorf("subject = %+v, want ServiceAccount gardenio/gardenio-infra-admin", s)
	}
	if got := crb.Metadata.Labels["gardenio.io/access-boundary"]; got != "infra-admin" {
		t.Errorf("ClusterRoleBinding label gardenio.io/access-boundary = %q, want infra-admin", got)
	}

	// The grant is the built-in role: no chart-authored ClusterRole of this
	// name, and nothing else bound to cluster-admin by this feature.
	if got := len(filterInfraAdmin(objs, "ClusterRole", "gardenio-infra-admin")); got != 0 {
		t.Errorf("expected no chart-authored ClusterRole gardenio-infra-admin, got %d", got)
	}
	for _, o := range objs {
		if o.Metadata.Name != "gardenio-infra-admin" {
			continue
		}
		if o.Kind != "ServiceAccount" && o.Kind != "ClusterRoleBinding" {
			t.Errorf("unexpected object %s %q rendered for the infra-admin identity", o.Kind, o.Metadata.Name)
		}
	}
}

// Enabling infraAdmin must not switch on agents.clusterAdmin: that path also
// renders the -cluster-manager objects, which grant the deployer SA
// clusterrolebinding create. Checked in both renders, with a positive control
// that the objects DO appear when clusterAdmin is enabled.
func TestInfraAdmin_DoesNotRenderClusterAdminObjects(t *testing.T) {
	t.Parallel()
	hasClusterManager := func(objs []infraAdminObject) bool {
		for _, o := range objs {
			if strings.HasSuffix(o.Metadata.Name, "-cluster-manager") {
				return true
			}
		}
		return false
	}
	if hasClusterManager(renderInfraAdminChart(t)) {
		t.Error("default render must not contain -cluster-manager objects")
	}
	if hasClusterManager(renderInfraAdminChart(t, "agents.infraAdmin.enabled=true")) {
		t.Error("agents.infraAdmin.enabled must not render -cluster-manager objects (agents.clusterAdmin stays off)")
	}
	if !hasClusterManager(renderInfraAdminChart(t, "agents.clusterAdmin.enabled=true")) {
		t.Fatal("control: agents.clusterAdmin.enabled=true should render -cluster-manager objects; the filter is broken")
	}
}

func TestInfraAdmin_RequiresAgentsEnabled(t *testing.T) {
	t.Parallel()
	out := helmTemplateFullNamespaced(t, infraAdminNS, nil, "agents.enabled=false", "agents.infraAdmin.enabled=true")
	if strings.Contains(out, "gardenio-infra-admin") {
		t.Error("agents.enabled=false must suppress the infra-admin objects")
	}
}

func TestInfraAdmin_NameOverride(t *testing.T) {
	t.Parallel()
	objs := renderInfraAdminChart(t, "agents.infraAdmin.enabled=true", "agents.infraAdmin.name=custom-infra-admin")
	if len(filterInfraAdmin(objs, "ServiceAccount", "custom-infra-admin")) != 1 ||
		len(filterInfraAdmin(objs, "ClusterRoleBinding", "custom-infra-admin")) != 1 {
		t.Error("expected the overridden name on the ServiceAccount and the ClusterRoleBinding")
	}
	if len(filterInfraAdmin(objs, "ServiceAccount", "gardenio-infra-admin")) != 0 {
		t.Error("default name must not leak through when overridden")
	}
}

// The release-deployer must be able to read the cluster-admin binding on every
// upgrade but must NOT be able to write one: no impersonate verb, no wildcard
// verb or resource, and bind/escalate only by name and never on cluster-admin
// (the land-namespace roles, TestReleaseDeployer_LandNamespaceRoles).
// Otherwise the autonomous roll path could grant itself cluster-admin.
func TestInfraAdmin_ReleaseDeployerCannotBindClusterAdmin(t *testing.T) {
	t.Parallel()
	out := helmTemplateFullNamespaced(t, infraAdminNS, releaseDeployerRepoRootValues(),
		"releaseDeployer.enabled=true", "agents.infraAdmin.enabled=true")

	var deployerRole, infraAdminCRB *rbacObject
	objs := parseRBACObjects(t, out)
	for i := range objs {
		o := &objs[i]
		if o.Kind == "ClusterRole" && strings.HasSuffix(o.Metadata.Name, "-release-deployer") {
			deployerRole = o
		}
		if o.Kind == "ClusterRoleBinding" && o.Metadata.Name == "gardenio-infra-admin" {
			infraAdminCRB = o
		}
	}
	if infraAdminCRB == nil || deployerRole == nil || len(deployerRole.Rules) == 0 {
		t.Fatalf("control: expected the rendered infra-admin binding (%v) and release-deployer ClusterRole (%v) with rules", infraAdminCRB != nil, deployerRole != nil)
	}

	grantsGet := false
	for _, r := range deployerRole.Rules {
		for _, v := range r.Verbs {
			switch v {
			case "impersonate", "*":
				t.Errorf("release-deployer ClusterRole must not carry verb %q; rule: %+v", v, r)
			case "bind", "escalate":
				if len(r.ResourceNames) == 0 || slices.Contains(r.ResourceNames, "cluster-admin") || slices.Contains(r.ResourceNames, "gardenio-infra-admin") {
					t.Errorf("release-deployer ClusterRole may carry %q only by name, and never on cluster-admin; rule: %+v", v, r)
				}
			}
		}
		for _, res := range r.Resources {
			if res == "*" {
				t.Errorf("release-deployer ClusterRole must not carry resource \"*\"; rule: %+v", r)
			}
		}
		if slices.Contains(r.Resources, "clusterrolebindings") && slices.Contains(r.ResourceNames, "gardenio-infra-admin") && slices.Contains(r.Verbs, "get") {
			grantsGet = true
		}
	}
	if !grantsGet {
		t.Error("release-deployer must hold get on clusterrolebinding gardenio-infra-admin so helm can read it on upgrade")
	}
}

// helmTemplatePackaged renders the chart packaged at a given chart version, so
// a version bump is the only difference between two renders.
func helmTemplatePackaged(t *testing.T, version string, setFlags ...string) string {
	t.Helper()
	dest := t.TempDir()
	if out, err := exec.Command("helm", "package", chartDir(t), "--version", version, "--app-version", version, "--destination", dest).CombinedOutput(); err != nil {
		t.Fatalf("helm package %s failed: %v\n%s", version, err, out)
	}
	args := []string{"template", "test", filepath.Join(dest, "gardenio-"+version+".tgz"), "--namespace", infraAdminNS}
	for _, s := range setFlags {
		args = append(args, "--set", s)
	}
	out, err := exec.Command("helm", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template %s failed: %v\n%s", version, err, out)
	}
	return string(out)
}

// renderedDoc returns the single rendered YAML document of the given kind and
// name, verbatim.
func renderedDoc(t *testing.T, rendered, kind, name string) string {
	t.Helper()
	var found []string
	for doc := range strings.SplitSeq(rendered, "\n---") {
		k := kindLineRE.FindStringSubmatch(doc)
		if k == nil || k[1] != kind {
			continue
		}
		if strings.Contains(doc, "\n  name: "+name+"\n") {
			found = append(found, doc)
		}
	}
	if len(found) != 1 {
		t.Fatalf("expected exactly 1 %s %q in the render, got %d", kind, name, len(found))
	}
	return found[0]
}

// Helm patches any object whose rendered manifest changed between releases.
// The release-deployer cannot write a cluster-admin binding (no `bind`), so the
// binding must render byte-identically for every chart version or the first
// autonomous release after a version bump fails and --atomic rolls it back.
// The ServiceAccount, which does carry helm.sh/chart, is the positive control:
// the same comparison must see it differ, so a comparison that can never fail
// (wrong document, version not applied) cannot pass this test.
func TestInfraAdmin_ClusterRoleBindingIsStaticAcrossChartVersions(t *testing.T) {
	t.Parallel()
	flags := []string{"agents.enabled=true", "agents.infraAdmin.enabled=true"}
	v1 := helmTemplatePackaged(t, "2026.100.1", flags...)
	v2 := helmTemplatePackaged(t, "2026.200.9", flags...)

	saV1 := renderedDoc(t, v1, "ServiceAccount", "gardenio-infra-admin")
	saV2 := renderedDoc(t, v2, "ServiceAccount", "gardenio-infra-admin")
	if saV1 == saV2 {
		t.Fatal("control: the ServiceAccount carries helm.sh/chart and must differ between chart versions; the version bump is not reaching the render")
	}
	if !strings.Contains(saV1, "2026.100.1") || !strings.Contains(saV2, "2026.200.9") {
		t.Fatal("control: expected each ServiceAccount to carry its own chart version in helm.sh/chart")
	}

	crbV1 := renderedDoc(t, v1, "ClusterRoleBinding", "gardenio-infra-admin")
	crbV2 := renderedDoc(t, v2, "ClusterRoleBinding", "gardenio-infra-admin")
	if crbV1 != crbV2 {
		t.Errorf("ClusterRoleBinding gardenio-infra-admin must be byte-identical across chart versions.\n--- 2026.100.1 ---\n%s\n--- 2026.200.9 ---\n%s", crbV1, crbV2)
	}
	if strings.Contains(crbV1, "helm.sh/chart") || strings.Contains(crbV1, "2026.100.1") {
		t.Errorf("ClusterRoleBinding must not carry helm.sh/chart or any chart version:\n%s", crbV1)
	}
}
