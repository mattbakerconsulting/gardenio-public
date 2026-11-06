package tests

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types/ref"
	"google.golang.org/protobuf/types/known/structpb"
	"sigs.k8s.io/yaml"
)

// P9 (gd-IYY1dxukfR, plan gd-xiV5hyhPrG): the first per-project infra role,
// claimlayer-infra. Design: gardenio/docs/project-infra-role-design.md.
//
// What these tests pin, against deploy-values/gardenio-k3s.yaml:
//
//   - the default render has no claimlayer-infra object at all;
//   - the overlay renders exactly the SA, Role, RoleBinding, ResourceQuota,
//     LimitRange, two NetworkPolicies and the ValidatingAdmissionPolicy set;
//   - the Role has no secrets, pods/exec, pods/attach, pods/portforward, rbac or
//     wildcard entry, and the checker that says so is proven able to FAIL
//     (positive control: the same overlay plus a `secrets` rule is flagged);
//   - nothing binds claimlayer-infra at cluster scope;
//   - none of the project's hand-applied objects is rendered;
//   - the VAP's CEL, evaluated for real, admits a clean workload FIRST and then
//     denies each Secret-reaching variation.

const (
	infraName       = "claimlayer-infra"
	infraNS         = "claimlayer-demo"
	infraSAUsername = "system:serviceaccount:gardenio:claimlayer-infra"
)

type k8sObj struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name      string            `json:"name"`
		Namespace string            `json:"namespace"`
		Labels    map[string]string `json:"labels"`
	} `json:"metadata"`
	Raw map[string]any `json:"-"`
}

func (o k8sObj) id() string {
	return fmt.Sprintf("%s %s/%s", o.Kind, o.Metadata.Namespace, o.Metadata.Name)
}

// renderAllObjects renders the whole chart (namespace gardenio, release name
// gardenio, as in prod) and decodes every document.
func renderAllObjects(t *testing.T, valuesFiles []string, setFlags ...string) []k8sObj {
	t.Helper()
	args := []string{"template", "gardenio", chartDir(t), "--namespace", "gardenio"}
	for _, f := range valuesFiles {
		args = append(args, "-f", f)
	}
	for _, s := range setFlags {
		args = append(args, "--set", s)
	}
	out, err := exec.Command("helm", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template failed: %v\n%s", err, out)
	}
	var objs []k8sObj
	for _, d := range yamlDocs(string(out)) {
		var o k8sObj
		if err := yaml.Unmarshal([]byte(d), &o); err != nil || o.Kind == "" {
			continue
		}
		_ = yaml.Unmarshal([]byte(d), &o.Raw)
		objs = append(objs, o)
	}
	return objs
}

// infraObjects is every rendered object that belongs to the claimlayer-infra
// role: by name prefix, or by the gardenio.io/project label in the project ns.
func infraObjects(objs []k8sObj) []k8sObj {
	var out []k8sObj
	for _, o := range objs {
		// The release-deployer's own Role/RoleBinding in the project ns are
		// helm's access to these objects, not part of the infra role; they are
		// pinned by TestProjectInfraRole_ReleaseDeployerHoldsTheNamespace.
		if strings.HasSuffix(o.Metadata.Name, "-release-deployer") {
			continue
		}
		if strings.HasPrefix(o.Metadata.Name, infraName) || o.Metadata.Namespace == infraNS {
			out = append(out, o)
		}
	}
	return out
}

func TestProjectInfraRole_ReleaseDeployerHoldsTheNamespace(t *testing.T) {
	t.Parallel()
	// helm reads every object it owns on every upgrade, and RBAC escalation
	// prevention lets the deployer write the infra Role only if it holds it.
	// Without this Role every autonomous roll fails (see release_deployer_test.go).
	var role, binding bool
	for _, o := range renderAllObjects(t, []string{k3sOverlayPath(t)}) {
		if o.Metadata.Namespace == infraNS && o.Metadata.Name == "gardenio-release-deployer" {
			role = role || o.Kind == "Role"
			binding = binding || o.Kind == "RoleBinding"
		}
	}
	if !role || !binding {
		t.Errorf("release-deployer Role/RoleBinding in %s missing (role=%v binding=%v)", infraNS, role, binding)
	}
	// And only with the grant: the default render has none in that namespace.
	for _, o := range renderAllObjects(t, nil) {
		if o.Metadata.Namespace == infraNS {
			t.Errorf("default render must not touch %s: %s", infraNS, o.id())
		}
	}
}

func kindsOf(objs []k8sObj) map[string]int {
	m := map[string]int{}
	for _, o := range objs {
		m[o.Kind]++
	}
	return m
}

func TestProjectInfraRole_DefaultRenderHasNoClaimlayerInfra(t *testing.T) {
	t.Parallel()
	objs := renderAllObjects(t, nil)
	if got := infraObjects(objs); len(got) != 0 {
		var ids []string
		for _, o := range got {
			ids = append(ids, o.id())
		}
		t.Errorf("default render must have no claimlayer-infra objects; got %v", ids)
	}
	for _, o := range objs {
		if strings.Contains(o.Metadata.Name, "no-secrets") && strings.HasPrefix(o.Kind, "ValidatingAdmissionPolicy") {
			t.Errorf("default render must not carry the infra VAP set: %s", o.id())
		}
	}
}

func TestProjectInfraRole_OverlayRendersExactlyTheExpectedObjects(t *testing.T) {
	t.Parallel()
	objs := renderAllObjects(t, []string{k3sOverlayPath(t)})

	// Positive control for the selector: it must find something, or every
	// "exactly" below is about an empty set.
	got := infraObjects(objs)
	if len(got) == 0 {
		t.Fatal("overlay rendered no claimlayer-infra objects at all; the grant is not wired in")
	}

	want := map[string]int{
		"ServiceAccount":                   1,
		"Role":                             1,
		"RoleBinding":                      1,
		"ResourceQuota":                    1,
		"LimitRange":                       1,
		"NetworkPolicy":                    2,
		"ValidatingAdmissionPolicy":        4,
		"ValidatingAdmissionPolicyBinding": 4,
	}
	if gotKinds := kindsOf(got); !reflect.DeepEqual(gotKinds, want) {
		t.Errorf("claimlayer-infra object kinds = %v, want %v", gotKinds, want)
	}

	for _, o := range got {
		switch o.Kind {
		case "ServiceAccount":
			if o.Metadata.Namespace != "gardenio" || o.Metadata.Name != infraName {
				t.Errorf("SA must be gardenio/%s, got %s", infraName, o.id())
			}
			if v, ok := o.Raw["automountServiceAccountToken"].(bool); !ok || v {
				t.Errorf("SA automountServiceAccountToken must be false, got %v", o.Raw["automountServiceAccountToken"])
			}
		case "Role", "RoleBinding", "ResourceQuota", "LimitRange", "NetworkPolicy":
			if o.Metadata.Namespace != infraNS {
				t.Errorf("%s must be in %s", o.id(), infraNS)
			}
			if o.Metadata.Labels["gardenio.io/project"] != "claimlayer" {
				t.Errorf("%s must carry gardenio.io/project=claimlayer", o.id())
			}
		}
	}

	// The RoleBinding names the SA in ns gardenio and the Role we render.
	for _, o := range got {
		if o.Kind != "RoleBinding" {
			continue
		}
		subs, _ := o.Raw["subjects"].([]any)
		if len(subs) != 1 {
			t.Fatalf("RoleBinding must have exactly one subject, got %v", subs)
		}
		s := subs[0].(map[string]any)
		if s["kind"] != "ServiceAccount" || s["name"] != infraName || s["namespace"] != "gardenio" {
			t.Errorf("RoleBinding subject = %v, want ServiceAccount gardenio/%s", s, infraName)
		}
		if ref, _ := o.Raw["roleRef"].(map[string]any); ref["kind"] != "Role" || ref["name"] != infraName {
			t.Errorf("RoleBinding roleRef = %v", ref)
		}
	}
}

func TestProjectInfraRole_DoesNotRenderProjectsOwnObjects(t *testing.T) {
	t.Parallel()
	// The project's hand-applied ingress policies and the ecr-token-refresh
	// Role/RoleBinding already exist live; rendering an object with the same
	// name would collide on the first helm upgrade.
	theirs := []string{"claimlayer-demo-from-oauth2-proxy", "oauth2-proxy-from-public-edge", "ecr-token-refresh"}
	objs := renderAllObjects(t, []string{k3sOverlayPath(t)})
	if len(infraObjects(objs)) == 0 {
		t.Fatal("positive control: no infra objects rendered")
	}
	for _, o := range objs {
		if o.Metadata.Namespace != infraNS {
			continue
		}
		if slices.Contains(theirs, o.Metadata.Name) {
			t.Errorf("must not render the project's own object %s", o.id())
		}
		// Nothing in the project ns may be an ingress policy.
		if o.Kind == "NetworkPolicy" {
			if types, _ := o.Raw["spec"].(map[string]any)["policyTypes"].([]any); !slices.Contains(types, any("Egress")) || slices.Contains(types, any("Ingress")) {
				t.Errorf("%s must be egress-only, policyTypes=%v", o.id(), types)
			}
		}
	}
}

func TestProjectInfraRole_NoClusterRoleBindingNamesTheSA(t *testing.T) {
	t.Parallel()
	objs := renderAllObjects(t, []string{k3sOverlayPath(t)})
	seen := 0
	for _, o := range objs {
		if o.Kind != "ClusterRoleBinding" {
			continue
		}
		seen++
		subs, _ := o.Raw["subjects"].([]any)
		for _, s := range subs {
			if m, _ := s.(map[string]any); m["name"] == infraName {
				t.Errorf("%s binds %s at cluster scope", o.id(), infraName)
			}
		}
		if o.Metadata.Name == infraName {
			t.Errorf("a ClusterRoleBinding is named %s", infraName)
		}
	}
	// Positive control: the chart does render ClusterRoleBindings, so the loop
	// above looked at something.
	if seen == 0 {
		t.Fatal("positive control: overlay render contains no ClusterRoleBinding, the check is vacuous")
	}
}

// roleViolations lists every rule entry the infra Role must never contain.
func roleViolations(role map[string]any) []string {
	var bad []string
	forbiddenResources := []string{"secrets", "pods/exec", "pods/attach", "pods/portforward", "*", "serviceaccounts/token"}
	rules, _ := role["rules"].([]any)
	for _, r := range rules {
		rule := r.(map[string]any)
		groups := anyStrings(rule["apiGroups"])
		resources := anyStrings(rule["resources"])
		verbs := anyStrings(rule["verbs"])
		if slices.Contains(groups, "rbac.authorization.k8s.io") {
			bad = append(bad, "apiGroup rbac.authorization.k8s.io")
		}
		if slices.Contains(groups, "*") {
			bad = append(bad, "apiGroup *")
		}
		for _, res := range resources {
			if slices.Contains(forbiddenResources, res) {
				bad = append(bad, "resource "+res)
			}
		}
		for _, v := range verbs {
			if v == "*" || v == "escalate" || v == "bind" || v == "impersonate" {
				bad = append(bad, "verb "+v)
			}
		}
	}
	sort.Strings(bad)
	return slices.Compact(bad)
}

func anyStrings(v any) []string {
	var out []string
	list, _ := v.([]any)
	for _, e := range list {
		out = append(out, fmt.Sprint(e))
	}
	return out
}

func infraRole(t *testing.T, objs []k8sObj) map[string]any {
	t.Helper()
	for _, o := range objs {
		if o.Kind == "Role" && o.Metadata.Name == infraName {
			return o.Raw
		}
	}
	t.Fatal("no Role claimlayer-infra in the render")
	return nil
}

func TestProjectInfraRole_RoleHasNoForbiddenEntries(t *testing.T) {
	t.Parallel()
	role := infraRole(t, renderAllObjects(t, []string{k3sOverlayPath(t)}))
	if bad := roleViolations(role); len(bad) != 0 {
		t.Errorf("claimlayer-infra Role contains forbidden entries: %v", bad)
	}
	// Sanity: the Role really is the useful one (rollout + logs).
	var flat []string
	for _, r := range role["rules"].([]any) {
		flat = append(flat, anyStrings(r.(map[string]any)["resources"])...)
	}
	for _, must := range []string{"deployments", "jobs", "pods", "pods/log"} {
		if !slices.Contains(flat, must) {
			t.Errorf("Role must grant %q, resources = %v", must, flat)
		}
	}
}

// writeOverlay writes a values overlay that appends one extra rule to the
// claimlayer-infra grant, so the checker can be shown to fail on the real
// render pipeline (not only on a hand-written fixture).
func writeOverlayWithExtraRule(t *testing.T, apiGroup, resource string) string {
	t.Helper()
	raw, err := os.ReadFile(k3sOverlayPath(t))
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := yaml.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	g := v["projectNamespaceRbac"].(map[string]any)["grants"].(map[string]any)[infraName].(map[string]any)
	g["rules"] = append(g["rules"].([]any), map[string]any{
		"apiGroups": []any{apiGroup},
		"resources": []any{resource},
		"verbs":     []any{"get"},
	})
	out, err := yaml.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "overlay.yaml")
	if err := os.WriteFile(p, out, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestProjectInfraRole_ForbiddenEntryCheckerCanFail(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ group, resource, want string }{
		{"", "secrets", "resource secrets"},
		{"", "pods/exec", "resource pods/exec"},
		{"", "pods/attach", "resource pods/attach"},
		{"", "pods/portforward", "resource pods/portforward"},
		{"rbac.authorization.k8s.io", "roles", "apiGroup rbac.authorization.k8s.io"},
		{"*", "configmaps", "apiGroup *"},
		{"", "*", "resource *"},
	} {
		t.Run(c.want, func(t *testing.T) {
			overlay := writeOverlayWithExtraRule(t, c.group, c.resource)
			role := infraRole(t, renderAllObjects(t, []string{k3sOverlayPath(t), overlay}))
			bad := roleViolations(role)
			if !slices.Contains(bad, c.want) {
				t.Errorf("a rule adding %q/%q must be flagged %q; checker returned %v", c.group, c.resource, c.want, bad)
			}
		})
	}
}

// --- guards -----------------------------------------------------------------

// infraGrantOverlay returns the k3s overlay with a mutation applied to the
// claimlayer-infra grant entry.
func infraGrantOverlay(t *testing.T, mutate func(g map[string]any)) string {
	t.Helper()
	raw, err := os.ReadFile(k3sOverlayPath(t))
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := yaml.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	mutate(v["projectNamespaceRbac"].(map[string]any)["grants"].(map[string]any)[infraName].(map[string]any))
	out, err := yaml.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "overlay.yaml")
	if err := os.WriteFile(p, out, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func renderFails(t *testing.T, valuesFiles ...string) string {
	t.Helper()
	args := []string{"template", "gardenio", chartDir(t), "--namespace", "gardenio"}
	for _, f := range valuesFiles {
		args = append(args, "-f", f)
	}
	out, err := exec.Command("helm", args...).CombinedOutput()
	if err == nil {
		t.Fatal("expected the render to FAIL, but it succeeded")
	}
	return string(out)
}

func TestProjectInfraRole_HalfConfiguredGrantFailsTheRender(t *testing.T) {
	t.Parallel()
	// Positive control first: the unmodified overlay renders.
	renderAllObjects(t, []string{k3sOverlayPath(t)})

	for _, c := range []struct {
		name   string
		mutate func(g map[string]any)
		want   string
	}{
		{"no quota", func(g map[string]any) { delete(g, "resourceQuota") }, "resourceQuota.hard"},
		{"no limitRange", func(g map[string]any) { delete(g, "limitRange") }, "limitRange.limits"},
		{"no project", func(g map[string]any) { delete(g, "project") }, "no `project`"},
		{"vap off while pods are writable", func(g map[string]any) {
			g["agentInfra"].(map[string]any)["admissionPolicy"] = map[string]any{"enabled": false}
		}, "admissionPolicy.enabled"},
		{"vap absent while pods are writable", func(g map[string]any) {
			delete(g["agentInfra"].(map[string]any), "admissionPolicy")
		}, "admissionPolicy.enabled"},
	} {
		t.Run(c.name, func(t *testing.T) {
			// The mutated overlay is a full copy of the k3s overlay: layering it over
			// the original would merge the deleted key straight back in.
			out := renderFails(t, infraGrantOverlay(t, c.mutate))
			if !strings.Contains(out, c.want) {
				t.Errorf("failure should mention %q; got:\n%s", c.want, out)
			}
		})
	}
}

func TestProjectInfraRole_ServiceWriterNeedsTheServicesVAP(t *testing.T) {
	t.Parallel()
	svcRules := func(verbs ...any) []any {
		return []any{map[string]any{"apiGroups": []any{""}, "resources": []any{"services"}, "verbs": verbs}}
	}
	withVAP := func(on bool) func(g map[string]any) {
		return func(g map[string]any) {
			g["agentInfra"].(map[string]any)["admissionPolicy"] = map[string]any{"enabled": on}
		}
	}
	// Positive control: a Services-only writer with the VAP on renders the
	// Services VAP and its binding.
	on := infraGrantOverlay(t, func(g map[string]any) {
		g["rules"] = svcRules("get", "create", "patch")
		withVAP(true)(g)
	})
	var found int
	for _, o := range infraObjects(renderAllObjects(t, []string{on})) {
		if o.Kind == "ValidatingAdmissionPolicy" && o.Metadata.Name == infraName+"-service-exposure" {
			found++
		}
	}
	if found != 1 {
		t.Fatalf("a Services writer with the VAP on must render %s-service-exposure once, got %d", infraName, found)
	}

	// The VAP off while the Role can write Services must fail the render, for
	// every write verb: the Services VAP shares the admissionPolicy block.
	for _, verb := range []string{"create", "update", "patch", "*"} {
		t.Run("writer "+verb, func(t *testing.T) {
			out := renderFails(t, infraGrantOverlay(t, func(g map[string]any) {
				g["rules"] = svcRules("get", verb)
				withVAP(false)(g)
			}))
			if !strings.Contains(out, "create or change Services") {
				t.Errorf("failure should name the Services guard; got:\n%s", out)
			}
		})
	}

	// Control: a Services reader needs no VAP, so the guard must not over-fire.
	ro := infraGrantOverlay(t, func(g map[string]any) {
		g["rules"] = svcRules("get", "list")
		withVAP(false)(g)
	})
	renderAllObjects(t, []string{ro})
}

func TestProjectInfraRole_ReadOnlyGrantNeedsNoVAP(t *testing.T) {
	t.Parallel()
	// The guard is about pod-bearing WRITES. A grant that only reads pods does
	// not need the VAP, so the guard must not over-fire.
	overlay := infraGrantOverlay(t, func(g map[string]any) {
		g["rules"] = []any{map[string]any{"apiGroups": []any{""}, "resources": []any{"pods", "pods/log"}, "verbs": []any{"get", "list"}}}
		g["agentInfra"].(map[string]any)["admissionPolicy"] = map[string]any{"enabled": false}
	})
	objs := renderAllObjects(t, []string{overlay})
	if n := kindsOf(infraObjects(objs))["ValidatingAdmissionPolicy"]; n != 0 {
		t.Errorf("admissionPolicy.enabled=false must render no VAP, got %d", n)
	}
}

// --- NetworkPolicies ----------------------------------------------------------

func TestProjectInfraRole_EgressPolicies(t *testing.T) {
	t.Parallel()
	var internal, https map[string]any
	for _, o := range infraObjects(renderAllObjects(t, []string{k3sOverlayPath(t)})) {
		switch o.Metadata.Name {
		case infraName + "-egress-internal":
			internal = o.Raw
		case infraName + "-egress-https":
			https = o.Raw
		}
	}
	if internal == nil || https == nil {
		t.Fatalf("both egress policies must render (internal=%v https=%v)", internal != nil, https != nil)
	}
	flat := func(m map[string]any) string { b, _ := yaml.Marshal(m["spec"]); return string(b) }

	// internal: DNS to kube-dns, Keycloak runtime :8080 in ns gardenio, same-ns pods.
	is := flat(internal)
	for _, want := range []string{"k8s-app: kube-dns", "app.kubernetes.io/component: keycloak-runtime",
		"app.kubernetes.io/instance: gardenio", "kubernetes.io/metadata.name: gardenio", "port: 8080", "port: 53"} {
		if !strings.Contains(is, want) {
			t.Errorf("internal egress policy missing %q:\n%s", want, is)
		}
	}
	// https: 443 only, with private ranges and IMDS excluded; nothing else.
	hs := flat(https)
	for _, want := range []string{"cidr: 0.0.0.0/0", "169.254.0.0/16", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "port: 443"} {
		if !strings.Contains(hs, want) {
			t.Errorf("https egress policy missing %q:\n%s", want, hs)
		}
	}
	// No policy may allow an ipBlock that contains IMDS or the cluster CIDRs
	// without excluding them: the only ipBlock is the excepted 0.0.0.0/0.
	if strings.Contains(is, "ipBlock") {
		t.Errorf("internal policy must not use ipBlock:\n%s", is)
	}
	for _, o := range []map[string]any{internal, https} {
		spec := o["spec"].(map[string]any)
		if sel, _ := spec["podSelector"].(map[string]any); len(sel) != 0 {
			t.Errorf("policy must select every pod in the namespace, podSelector=%v", sel)
		}
	}
}

// --- ValidatingAdmissionPolicy: render shape + CEL evaluation -------------------

type vapPolicy struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		FailurePolicy    string `json:"failurePolicy"`
		MatchConstraints struct {
			ResourceRules []struct {
				APIGroups  []string `json:"apiGroups"`
				Operations []string `json:"operations"`
				Resources  []string `json:"resources"`
			} `json:"resourceRules"`
		} `json:"matchConstraints"`
		MatchConditions []struct {
			Name       string `json:"name"`
			Expression string `json:"expression"`
		} `json:"matchConditions"`
		Variables []struct {
			Name       string `json:"name"`
			Expression string `json:"expression"`
		} `json:"variables"`
		Validations []struct {
			Expression string `json:"expression"`
			Message    string `json:"message"`
		} `json:"validations"`
	} `json:"spec"`
}

type vapBinding struct {
	Spec struct {
		PolicyName        string   `json:"policyName"`
		ValidationActions []string `json:"validationActions"`
		MatchResources    struct {
			NamespaceSelector struct {
				MatchLabels map[string]string `json:"matchLabels"`
			} `json:"namespaceSelector"`
		} `json:"matchResources"`
	} `json:"spec"`
}

func renderVAPs(t *testing.T, valuesFiles ...string) (map[string]vapPolicy, map[string]vapBinding) {
	t.Helper()
	pols, binds := map[string]vapPolicy{}, map[string]vapBinding{}
	for _, o := range infraObjects(renderAllObjects(t, valuesFiles)) {
		b, _ := yaml.Marshal(o.Raw)
		switch o.Kind {
		case "ValidatingAdmissionPolicy":
			var p vapPolicy
			if err := yaml.Unmarshal(b, &p); err != nil {
				t.Fatal(err)
			}
			pols[p.Metadata.Name] = p
		case "ValidatingAdmissionPolicyBinding":
			var bd vapBinding
			if err := yaml.Unmarshal(b, &bd); err != nil {
				t.Fatal(err)
			}
			binds[bd.Spec.PolicyName] = bd
		}
	}
	return pols, binds
}

func TestProjectInfraRole_VAPRenderShape(t *testing.T) {
	t.Parallel()
	pols, binds := renderVAPs(t, k3sOverlayPath(t))
	if len(pols) != 4 {
		t.Fatalf("want 4 policies (pod, workload, cronjob, service), got %d", len(pols))
	}
	for name, p := range pols {
		if p.Spec.FailurePolicy != "Fail" {
			t.Errorf("%s failurePolicy = %q, want Fail", name, p.Spec.FailurePolicy)
		}
		if len(p.Spec.MatchConditions) != 1 || !strings.Contains(p.Spec.MatchConditions[0].Expression, "'"+infraSAUsername+"'") {
			t.Errorf("%s must match exactly the username %s, got %+v", name, infraSAUsername, p.Spec.MatchConditions)
		}
		b, ok := binds[name]
		if !ok {
			t.Errorf("%s has no binding", name)
			continue
		}
		if !slices.Equal(b.Spec.ValidationActions, []string{"Deny"}) {
			t.Errorf("%s validationActions = %v, want [Deny]", name, b.Spec.ValidationActions)
		}
		ml := b.Spec.MatchResources.NamespaceSelector.MatchLabels
		if len(ml) != 1 || ml["kubernetes.io/metadata.name"] != infraNS {
			t.Errorf("%s binding must select ns %s by the automatic label, got %v", name, infraNS, ml)
		}
	}
}

// podSpecShape wraps a pod spec the way each policy's `ps` variable reads it.
func podSpecShape(shape string, ps map[string]any) map[string]any {
	switch shape {
	case "pod":
		return map[string]any{"spec": ps}
	case "workload":
		return map[string]any{"spec": map[string]any{"template": map[string]any{"spec": ps}}}
	case "cronjob":
		return map[string]any{"spec": map[string]any{"jobTemplate": map[string]any{"spec": map[string]any{"template": map[string]any{"spec": ps}}}}}
	}
	panic(shape)
}

// celNative turns a CEL value back into plain Go data so it can be handed to
// the next variable expression as part of the `variables` map.
func celNative(t *testing.T, v ref.Val) any {
	t.Helper()
	pb, err := v.ConvertToNative(reflect.TypeOf(&structpb.Value{}))
	if err != nil {
		t.Fatalf("convert CEL value: %v", err)
	}
	return pb.(*structpb.Value).AsInterface()
}

// evalVAP evaluates one rendered policy against an object and a requesting
// user, mirroring the apiserver: matchConditions (all must be true or the
// policy does not apply), then variables in order, then every validation.
// It returns applied=false when a matchCondition was false, and the messages
// of the validations that returned false.
func evalVAP(t *testing.T, p vapPolicy, object map[string]any, username string) (applied bool, denied []string) {
	t.Helper()
	env, err := cel.NewEnv(
		cel.OptionalTypes(),
		cel.Variable("object", cel.DynType),
		cel.Variable("request", cel.DynType),
		cel.Variable("variables", cel.DynType),
	)
	if err != nil {
		t.Fatal(err)
	}
	eval := func(expr string, vars map[string]any) ref.Val {
		ast, iss := env.Compile(expr)
		if iss.Err() != nil {
			t.Fatalf("CEL compile error in %q: %v", expr, iss.Err())
		}
		prg, err := env.Program(ast)
		if err != nil {
			t.Fatalf("CEL program %q: %v", expr, err)
		}
		out, _, err := prg.Eval(map[string]any{
			"object":    object,
			"request":   map[string]any{"userInfo": map[string]any{"username": username}},
			"variables": vars,
		})
		if err != nil {
			t.Fatalf("CEL eval %q: %v", expr, err)
		}
		return out
	}
	for _, mc := range p.Spec.MatchConditions {
		if eval(mc.Expression, map[string]any{}).Value() != true {
			return false, nil
		}
	}
	vars := map[string]any{}
	for _, v := range p.Spec.Variables {
		vars[v.Name] = celNative(t, eval(v.Expression, vars))
	}
	for _, v := range p.Spec.Validations {
		if eval(v.Expression, vars).Value() != true {
			denied = append(denied, strings.Join(strings.Fields(v.Message), " "))
		}
	}
	return true, denied
}

// cleanPodSpec is the workload the agent is SUPPOSED to be able to run: an
// emptyDir, no Secret reference, the default SA, no token.
func cleanPodSpec() map[string]any {
	return map[string]any{
		"serviceAccountName":           "default",
		"automountServiceAccountToken": false,
		"containers": []any{map[string]any{
			"name": "c", "image": "busybox",
			"env": []any{map[string]any{"name": "A", "value": "b"}},
		}},
		"volumes": []any{map[string]any{"name": "tmp", "emptyDir": map[string]any{}}},
	}
}

func container0(ps map[string]any) map[string]any {
	return ps["containers"].([]any)[0].(map[string]any)
}

func TestProjectInfraRole_VAPCEL(t *testing.T) {
	t.Parallel()
	pols, _ := renderVAPs(t, k3sOverlayPath(t))
	shapes := map[string]string{
		"pod":      infraName + "-no-secrets-pod",
		"workload": infraName + "-no-secrets-workload",
		"cronjob":  infraName + "-no-secrets-cronjob",
	}

	type tc struct {
		name   string
		mutate func(ps map[string]any)
		deny   string // message fragment of the rule that must fire; "" = admitted
		shapes []string
	}
	all := []string{"pod", "workload", "cronjob"}
	cases := []tc{
		// Admitted first: without these the denials below prove nothing.
		{"clean", func(ps map[string]any) {}, "", all},
		{"configMap volume, configMap envFrom, imagePullSecrets", func(ps map[string]any) {
			ps["volumes"] = []any{map[string]any{"name": "cm", "configMap": map[string]any{"name": "x"}}}
			container0(ps)["envFrom"] = []any{map[string]any{"configMapRef": map[string]any{"name": "x"}}}
			ps["imagePullSecrets"] = []any{map[string]any{"name": "ecr"}}
		}, "", all},
		{"projected configMap + downwardAPI", func(ps map[string]any) {
			ps["volumes"] = []any{map[string]any{"name": "p", "projected": map[string]any{"sources": []any{
				map[string]any{"configMap": map[string]any{"name": "x"}},
				map[string]any{"downwardAPI": map[string]any{}},
			}}}}
		}, "", all},
		{"no volumes at all", func(ps map[string]any) { delete(ps, "volumes") }, "", all},
		{"serviceAccountName unset (defaults)", func(ps map[string]any) { delete(ps, "serviceAccountName") }, "", all},

		// Denied: one change from clean each.
		{"env secretKeyRef", func(ps map[string]any) {
			container0(ps)["env"] = []any{map[string]any{"name": "S", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": "s", "key": "k"}}}}
		}, "may not reference a Secret from a container", all},
		{"envFrom secretRef", func(ps map[string]any) {
			container0(ps)["envFrom"] = []any{map[string]any{"secretRef": map[string]any{"name": "s"}}}
		}, "may not reference a Secret from a container", all},
		{"initContainer secretKeyRef", func(ps map[string]any) {
			ps["initContainers"] = []any{map[string]any{"name": "i", "image": "busybox",
				"env": []any{map[string]any{"name": "S", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": "s", "key": "k"}}}}}}
		}, "may not reference a Secret from a container", all},
		{"ephemeralContainer secretKeyRef", func(ps map[string]any) {
			ps["ephemeralContainers"] = []any{map[string]any{"name": "e", "image": "busybox",
				"env": []any{map[string]any{"name": "S", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": "s", "key": "k"}}}}}}
		}, "ephemeral container", []string{"pod"}},
		{"secret volume", func(ps map[string]any) {
			ps["volumes"] = []any{map[string]any{"name": "s", "secret": map[string]any{"secretName": "s"}}}
		}, "may only use emptyDir", all},
		{"projected secret", func(ps map[string]any) {
			ps["volumes"] = []any{map[string]any{"name": "p", "projected": map[string]any{"sources": []any{map[string]any{"secret": map[string]any{"name": "s"}}}}}}
		}, "may only use emptyDir", all},
		{"projected serviceAccountToken", func(ps map[string]any) {
			ps["volumes"] = []any{map[string]any{"name": "p", "projected": map[string]any{"sources": []any{map[string]any{"serviceAccountToken": map[string]any{"path": "t"}}}}}}
		}, "may only use emptyDir", all},
		{"projected mixing configMap and serviceAccountToken", func(ps map[string]any) {
			ps["volumes"] = []any{map[string]any{"name": "p", "projected": map[string]any{"sources": []any{
				map[string]any{"configMap": map[string]any{"name": "x"}},
				map[string]any{"serviceAccountToken": map[string]any{"path": "t"}},
			}}}}
		}, "may only use emptyDir", all},
		{"csi volume", func(ps map[string]any) {
			ps["volumes"] = []any{map[string]any{"name": "c", "csi": map[string]any{"driver": "secrets-store.csi.k8s.io"}}}
		}, "may only use emptyDir", all},
		{"persistentVolumeClaim volume", func(ps map[string]any) {
			ps["volumes"] = []any{map[string]any{"name": "v", "persistentVolumeClaim": map[string]any{"claimName": "x"}}}
		}, "may only use emptyDir", all},
		{"hostPath volume", func(ps map[string]any) {
			ps["volumes"] = []any{map[string]any{"name": "h", "hostPath": map[string]any{"path": "/"}}}
		}, "may only use emptyDir", all},
		{"one clean volume and one secret volume", func(ps map[string]any) {
			ps["volumes"] = append(ps["volumes"].([]any), map[string]any{"name": "s", "secret": map[string]any{"secretName": "s"}})
		}, "may only use emptyDir", all},
		{"serviceAccountName ecr-token-refresh", func(ps map[string]any) { ps["serviceAccountName"] = "ecr-token-refresh" }, "may only run workloads as", all},
		{"deprecated serviceAccount differs", func(ps map[string]any) { ps["serviceAccount"] = "ecr-token-refresh" }, "may only run workloads as", all},
		{"automount unset", func(ps map[string]any) { delete(ps, "automountServiceAccountToken") }, "automountServiceAccountToken: false", all},
		{"automount true", func(ps map[string]any) { ps["automountServiceAccountToken"] = true }, "automountServiceAccountToken: false", all},
	}

	for _, shapeID := range all {
		pol, ok := pols[shapes[shapeID]]
		if !ok {
			t.Fatalf("policy %s not rendered", shapes[shapeID])
		}
		for _, c := range cases {
			if !slices.Contains(c.shapes, shapeID) {
				continue
			}
			t.Run(shapeID+"/"+c.name, func(t *testing.T) {
				ps := cleanPodSpec()
				c.mutate(ps)
				applied, denied := evalVAP(t, pol, podSpecShape(shapeID, ps), infraSAUsername)
				if !applied {
					t.Fatal("policy did not apply to the infra SA")
				}
				switch {
				case c.deny == "" && len(denied) != 0:
					t.Errorf("must be admitted, denied by: %v", denied)
				case c.deny != "" && len(denied) == 0:
					t.Errorf("must be denied (%s), was admitted", c.deny)
				case c.deny != "":
					found := false
					for _, d := range denied {
						found = found || strings.Contains(d, c.deny)
					}
					if !found {
						t.Errorf("denied for the wrong reason: want fragment %q, got %v", c.deny, denied)
					}
				}
			})
		}

		// Scope: the same secret-bearing workload from any other identity is
		// not touched by the policy.
		t.Run(shapeID+"/other user skips the policy", func(t *testing.T) {
			ps := cleanPodSpec()
			container0(ps)["envFrom"] = []any{map[string]any{"secretRef": map[string]any{"name": "s"}}}
			for _, user := range []string{"system:serviceaccount:gardenio:infra-admin", "system:serviceaccount:claimlayer-demo:default", "oidc:someone@example.com"} {
				if applied, _ := evalVAP(t, pol, podSpecShape(shapeID, ps), user); applied {
					t.Errorf("policy must not apply to %q", user)
				}
			}
			// Control: it DOES apply to the infra SA with the same object.
			if applied, denied := evalVAP(t, pol, podSpecShape(shapeID, ps), infraSAUsername); !applied || len(denied) == 0 {
				t.Errorf("control: infra SA must be matched and denied, applied=%v denied=%v", applied, denied)
			}
		})
	}
}

// CVE-2020-8554: a Service with spec.externalIPs that selects the agent's own
// pod hijacks that IP cluster-wide (kube-proxy on every node routes it), and
// the agent reads the traffic through pods/log. type LoadBalancer/NodePort open
// host ports on the public nodes. The Role allows services create/update/patch,
// so the fence is a VAP on services for this SA only.
func TestProjectInfraRole_ServiceVAPCEL(t *testing.T) {
	t.Parallel()
	pols, binds := renderVAPs(t, k3sOverlayPath(t))
	name := infraName + "-service-exposure"
	pol, ok := pols[name]
	if !ok {
		t.Fatalf("policy %s not rendered; have %v", name, pols)
	}

	// Render shape: CREATE and UPDATE on services only, this SA only.
	if pol.Spec.FailurePolicy != "Fail" {
		t.Errorf("failurePolicy = %q, want Fail", pol.Spec.FailurePolicy)
	}
	if len(pol.Spec.MatchConditions) != 1 || !strings.Contains(pol.Spec.MatchConditions[0].Expression, "'"+infraSAUsername+"'") {
		t.Errorf("must match exactly the username %s, got %+v", infraSAUsername, pol.Spec.MatchConditions)
	}
	b, ok := binds[name]
	if !ok {
		t.Fatalf("%s has no binding", name)
	}
	if !slices.Equal(b.Spec.ValidationActions, []string{"Deny"}) {
		t.Errorf("validationActions = %v, want [Deny]", b.Spec.ValidationActions)
	}
	if ml := b.Spec.MatchResources.NamespaceSelector.MatchLabels; len(ml) != 1 || ml["kubernetes.io/metadata.name"] != infraNS {
		t.Errorf("binding must select ns %s by the automatic label, got %v", infraNS, ml)
	}
	if len(pol.Spec.MatchConstraints.ResourceRules) != 1 {
		t.Fatalf("want one resourceRule, got %+v", pol.Spec.MatchConstraints.ResourceRules)
	}
	rr := pol.Spec.MatchConstraints.ResourceRules[0]
	if !slices.Equal(rr.Resources, []string{"services"}) || !slices.Equal(rr.Operations, []string{"CREATE", "UPDATE"}) {
		t.Errorf("rule must cover services CREATE+UPDATE only, got %+v", rr)
	}

	svc := func(spec map[string]any) map[string]any { return map[string]any{"spec": spec} }
	cases := []struct {
		name string
		spec map[string]any
		deny string // message fragment; "" = admitted
	}{
		// Admitted first: without these the denials prove nothing.
		{"plain ClusterIP", map[string]any{"type": "ClusterIP", "selector": map[string]any{"a": "b"}, "ports": []any{map[string]any{"port": 80}}}, ""},
		{"type unset (defaults to ClusterIP)", map[string]any{"selector": map[string]any{"a": "b"}}, ""},
		{"headless", map[string]any{"clusterIP": "None", "selector": map[string]any{"a": "b"}}, ""},
		{"empty externalIPs list", map[string]any{"type": "ClusterIP", "externalIPs": []any{}}, ""},

		{"externalIPs (CVE-2020-8554)", map[string]any{"type": "ClusterIP", "externalIPs": []any{"169.254.169.254"}, "selector": map[string]any{"a": "b"}}, "externalIPs"},
		{"externalIPs without a type", map[string]any{"externalIPs": []any{"10.43.0.10"}}, "externalIPs"},
		{"LoadBalancer", map[string]any{"type": "LoadBalancer"}, "type ClusterIP"},
		{"NodePort", map[string]any{"type": "NodePort"}, "type ClusterIP"},
		{"ExternalName", map[string]any{"type": "ExternalName", "externalName": "kbeads.gardenio.svc"}, "externalName"},
		{"externalName on a ClusterIP", map[string]any{"type": "ClusterIP", "externalName": "x.example.com"}, "externalName"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			applied, denied := evalVAP(t, pol, svc(c.spec), infraSAUsername)
			if !applied {
				t.Fatal("policy did not apply to the infra SA")
			}
			switch {
			case c.deny == "" && len(denied) != 0:
				t.Errorf("must be admitted, denied by: %v", denied)
			case c.deny != "" && len(denied) == 0:
				t.Errorf("must be denied (%s), was admitted", c.deny)
			case c.deny != "":
				found := false
				for _, d := range denied {
					found = found || strings.Contains(d, c.deny)
				}
				if !found {
					t.Errorf("denied for the wrong reason: want fragment %q, got %v", c.deny, denied)
				}
			}
		})
	}

	// Scope: the same externalIPs Service from any other identity is not
	// touched by the policy; control: it IS denied for the infra SA.
	bad := svc(map[string]any{"externalIPs": []any{"169.254.169.254"}})
	for _, user := range []string{"system:serviceaccount:gardenio:infra-admin", "system:serviceaccount:claimlayer-demo:default", "oidc:someone@example.com"} {
		if applied, _ := evalVAP(t, pol, bad, user); applied {
			t.Errorf("policy must not apply to %q", user)
		}
	}
	if applied, denied := evalVAP(t, pol, bad, infraSAUsername); !applied || len(denied) == 0 {
		t.Errorf("control: infra SA must be matched and denied, applied=%v denied=%v", applied, denied)
	}
}

// N2 (operator review of #346): the release-deployer's Role in the project
// namespace must not be able to CREATE a Role or RoleBinding. `create` cannot
// be name-restricted, so it would let a compromised deployer bind the infra
// rules (which the pod VAP does not fence for that identity) to any subject.
// The first install is an operator roll; later rolls update the named objects.
func TestProjectInfraRole_DeployerCannotCreateRolesOrBindings(t *testing.T) {
	t.Parallel()
	var role map[string]any
	for _, o := range renderAllObjects(t, []string{k3sOverlayPath(t)}) {
		if o.Kind == "Role" && o.Metadata.Namespace == infraNS && strings.HasSuffix(o.Metadata.Name, "-release-deployer") {
			role = o.Raw
		}
	}
	if role == nil {
		t.Fatalf("no release-deployer Role in %s", infraNS)
	}
	var createsOnPolicies, namedWriteOnRBAC bool
	for _, r := range role["rules"].([]any) {
		rule := r.(map[string]any)
		verbs, resources := anyStrings(rule["verbs"]), anyStrings(rule["resources"])
		hasCreate := slices.Contains(verbs, "create") || slices.Contains(verbs, "*")
		onRBAC := slices.Contains(resources, "roles") || slices.Contains(resources, "rolebindings") || slices.Contains(resources, "*")
		if hasCreate && slices.Contains(resources, "networkpolicies") {
			createsOnPolicies = true
		}
		if slices.Contains(verbs, "patch") && onRBAC && len(anyStrings(rule["resourceNames"])) > 0 {
			namedWriteOnRBAC = true
		}
		if hasCreate && onRBAC && len(anyStrings(rule["resourceNames"])) == 0 {
			// The infra rules are copied verbatim into this Role; none of them
			// is on RBAC objects, so any hit here is the narrowing regressing.
			t.Errorf("release-deployer Role in %s can create roles/rolebindings: %v", infraNS, rule)
		}
	}
	// Controls: the checker sees `create` rules (networkpolicies still has
	// one) and the named update path on RBAC objects survives.
	if !createsOnPolicies {
		t.Error("control: expected the deployer to keep create on networkpolicies; the create scan is not seeing the rules")
	}
	if !namedWriteOnRBAC {
		t.Error("control: expected named get/update/patch/delete on the infra Role and RoleBinding to remain")
	}
}
