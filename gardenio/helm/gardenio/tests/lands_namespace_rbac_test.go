package tests

import (
	"strings"
	"testing"

	"github.com/google/cel-go/cel"
	"sigs.k8s.io/yaml"
)

// Lands land namespaces (Track B spec §5.2, step 10.2 gd-NH44mohWlL): the
// controller RBAC in templates/controller/lands-namespace-rbac.yaml and the
// env in the controller deployment.

const landNamespaceRBAC = "templates/controller/lands-namespace-rbac.yaml"

func renderLandNamespaceRBAC(t *testing.T, setFlags ...string) string {
	t.Helper()
	return helmTemplateNS(t, "gardenio", landNamespaceRBAC,
		append([]string{"agents.enabled=true", "landNamespaces.enabled=true"}, setFlags...)...)
}

// landDoc returns the rendered document of kind and name.
func landDoc(t *testing.T, out, kind, name string) string {
	t.Helper()
	for d := range strings.SplitSeq(out, "\n---") {
		if strings.Contains(d, "kind: "+kind+"\n") && strings.Contains(d, "\n  name: "+name+"\n") {
			return d
		}
	}
	t.Fatalf("%s %s not rendered:\n%s", kind, name, out)
	return ""
}

func TestLandNamespaces_OffByDefault(t *testing.T) {
	t.Parallel()
	out := helmTemplateNS(t, "gardenio", "", "agents.enabled=true")
	for _, s := range []string{"gardenio-land-agent-manager", "lands-namespaces", "LANDS_NAMESPACES_ENABLED"} {
		if strings.Contains(out, s) {
			t.Errorf("default render contains %q", s)
		}
	}
}

func TestLandNamespaces_AgentManagerRoleIsNeverBoundClusterWide(t *testing.T) {
	t.Parallel()
	out := renderLandNamespaceRBAC(t)
	role := landDoc(t, out, "ClusterRole", "gardenio-land-agent-manager")
	for _, want := range []string{`resources: ["pods"]`, `resources: ["secrets", "configmaps", "persistentvolumeclaims"]`, `resources: ["events"]`} {
		if !strings.Contains(role, want) {
			t.Errorf("gardenio-land-agent-manager lacks %s:\n%s", want, role)
		}
	}
	for _, never := range []string{"namespaces", "rolebindings", "clusterroles", "nodes", "\"*\""} {
		if strings.Contains(role, never) {
			t.Errorf("gardenio-land-agent-manager grants %s:\n%s", never, role)
		}
	}
	// Only per-land RoleBindings (made by the provisioner) bind it.
	for d := range strings.SplitSeq(out, "\n---") {
		if strings.Contains(d, "kind: ClusterRoleBinding") && strings.Contains(d, "name: gardenio-land-agent-manager") {
			t.Errorf("a ClusterRoleBinding binds gardenio-land-agent-manager cluster-wide:\n%s", d)
		}
	}
}

func TestLandNamespaces_ProvisionerMayBindOnlyTheAgentManager(t *testing.T) {
	t.Parallel()
	out := renderLandNamespaceRBAC(t)
	role := landDoc(t, out, "ClusterRole", "test-agents-lands-namespaces")
	if strings.Count(role, `verbs: ["bind"]`) != 1 || !strings.Contains(role, `resourceNames: ["gardenio-land-agent-manager"]`) {
		t.Errorf("bind is not limited to gardenio-land-agent-manager:\n%s", role)
	}
	for _, never := range []string{"escalate", "impersonate", "clusterrolebindings", "\"*\"", "pods", "secrets"} {
		if strings.Contains(role, never) {
			t.Errorf("the provisioner role grants %s:\n%s", never, role)
		}
	}
	binding := landDoc(t, out, "ClusterRoleBinding", "test-agents-lands-namespaces")
	if !strings.Contains(binding, "name: test-agents-sa\n    namespace: gardenio") {
		t.Errorf("binding subject is not the controller ServiceAccount:\n%s", binding)
	}
}

func TestLandNamespaces_AdmissionPolicyHoldsWritesToLandNamespaces(t *testing.T) {
	t.Parallel()
	out := renderLandNamespaceRBAC(t)
	vap := landDoc(t, out, "ValidatingAdmissionPolicy", "test-agents-lands-namespaces")
	for _, want := range []string{
		"'system:serviceaccount:gardenio:test-agents-sa'",
		"variables.obj.metadata.name.startsWith('land-')",
		"variables.obj.metadata.name == 'land-' + variables.labels['gardenio.io/land']",
		"request.namespace.startsWith('land-')",
		"object.roleRef.name == 'gardenio-land-agent-manager'",
		"failurePolicy: Fail",
	} {
		if !strings.Contains(vap, want) {
			t.Errorf("admission policy lacks %s:\n%s", want, vap)
		}
	}
	// The policies are what make the cluster-wide grant narrow, so they render
	// with landNamespaces and have no switch of their own.
	for _, name := range []string{"test-agents-lands-namespaces", "test-agents-land-agent-manager-binding", "test-agents-land-pods"} {
		landDoc(t, out, "ValidatingAdmissionPolicy", name)
		b := landDoc(t, out, "ValidatingAdmissionPolicyBinding", name)
		if !strings.Contains(b, "policyName: "+name+"\n") || !strings.Contains(b, `validationActions: ["Deny"]`) {
			t.Errorf("binding %s does not deny under its policy:\n%s", name, b)
		}
	}
	if strings.Contains(out, "admissionPolicy") {
		t.Error("the land admission policies still have an off switch")
	}
}

// landPolicy renders the named land admission policy.
func landPolicy(t *testing.T, name string, setFlags ...string) vapPolicy {
	t.Helper()
	var p vapPolicy
	if err := yaml.Unmarshal([]byte(landDoc(t, renderLandNamespaceRBAC(t, setFlags...), "ValidatingAdmissionPolicy", name)), &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// The controller ServiceAccount's `bind` on gardenio-land-agent-manager is
// cluster-wide and it can create ClusterRoleBindings (cluster-rbac.yaml), so
// the binding policy is what keeps the role out of every non-land namespace,
// and, for every principal, bound to nobody but the controller
// (gd-pXik57wZA9).
func TestLandNamespaces_AgentManagerBindingPolicyCEL(t *testing.T) {
	t.Parallel()
	p := landPolicy(t, "test-agents-land-agent-manager-binding")
	if p.Spec.FailurePolicy != "Fail" {
		t.Errorf("failurePolicy = %q, want Fail", p.Spec.FailurePolicy)
	}
	const ctrl = "system:serviceaccount:gardenio:test-agents-sa"
	sa := func(name, ns string) map[string]any {
		return map[string]any{"kind": "ServiceAccount", "name": name, "namespace": ns}
	}
	controllerSA := sa("test-agents-sa", "gardenio")
	rbTo := func(kind, name string, subjects ...any) map[string]any {
		return map[string]any{"metadata": map[string]any{"name": "b"}, "roleRef": map[string]any{"kind": kind, "name": name}, "subjects": subjects}
	}
	rb := func(kind, name string) map[string]any { return rbTo(kind, name, controllerSA) }
	agentManagerTo := func(subjects ...any) map[string]any {
		return rbTo("ClusterRole", "gardenio-land-agent-manager", subjects...)
	}
	noSubjects := map[string]any{"metadata": map[string]any{"name": "b"}, "roleRef": map[string]any{"kind": "ClusterRole", "name": "gardenio-land-agent-manager"}}
	cases := []landVAPRequest{
		{name: "rolebinding in a land namespace", user: ctrl, op: "CREATE", resource: "rolebindings", ns: "land-acme", object: rb("ClusterRole", "gardenio-land-agent-manager"), applied: true, wantAllowed: true},
		{name: "bound to the land-agent ServiceAccount", user: ctrl, op: "CREATE", resource: "rolebindings", ns: "land-acme", object: agentManagerTo(sa("land-agent", "land-acme")), applied: true},
		{name: "controller name in another namespace", user: ctrl, op: "UPDATE", resource: "rolebindings", ns: "land-acme", object: agentManagerTo(sa("test-agents-sa", "land-acme")), applied: true},
		{name: "bound to a user", user: "kubernetes-admin", op: "CREATE", resource: "rolebindings", ns: "land-acme",
			object: agentManagerTo(map[string]any{"kind": "User", "name": "test-agents-sa"}), applied: true},
		{name: "a second subject beside the controller", user: ctrl, op: "UPDATE", resource: "rolebindings", ns: "land-acme",
			object: agentManagerTo(controllerSA, map[string]any{"kind": "Group", "name": "system:authenticated"}), applied: true},
		{name: "no subjects", user: ctrl, op: "CREATE", resource: "rolebindings", ns: "land-acme", object: noSubjects, applied: true},
		{name: "clusterrolebinding by the controller", user: ctrl, op: "CREATE", resource: "clusterrolebindings", object: rb("ClusterRole", "gardenio-land-agent-manager"), applied: true},
		{name: "clusterrolebinding by anyone", user: "kubernetes-admin", op: "CREATE", resource: "clusterrolebindings", object: rb("ClusterRole", "gardenio-land-agent-manager"), applied: true},
		{name: "rolebinding in the release namespace", user: ctrl, op: "CREATE", resource: "rolebindings", ns: "gardenio", object: rb("ClusterRole", "gardenio-land-agent-manager"), applied: true},
		{name: "rolebinding in kube-system by anyone", user: "kubernetes-admin", op: "UPDATE", resource: "rolebindings", ns: "kube-system", object: rb("ClusterRole", "gardenio-land-agent-manager"), applied: true},
		{name: "the cluster-admin binding helm manages", user: ctrl, op: "CREATE", resource: "clusterrolebindings", object: rb("ClusterRole", "cluster-admin")},
		{name: "a Role of the same name", user: ctrl, op: "CREATE", resource: "rolebindings", ns: "gardenio", object: rb("Role", "gardenio-land-agent-manager")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			applied, denied := evalLandVAP(t, p, c)
			if applied != c.applied {
				t.Fatalf("applied = %v, want %v", applied, c.applied)
			}
			if applied && (len(denied) == 0) != c.wantAllowed {
				t.Errorf("denied = %v, want allowed %v", denied, c.wantAllowed)
			}
		})
	}
}

// Tenant pods run as land-agent with no token and no hostPath at admission,
// not only because the controller's land fence builds them so. With
// landNamespaces.runtimeClassName empty (the default, gd-iyZom5u11A) they name
// no RuntimeClass; with runsc they must run under it.
func TestLandNamespaces_LandPodPolicyCEL(t *testing.T) {
	t.Parallel()
	t.Run("default runtime", func(t *testing.T) {
		t.Parallel()
		landPodPolicyCEL(t, "", []landPodRC{
			{"no runtime class", nil, true},
			{"empty runtime class", "", true},
			{"runsc", "runsc", false},
			{"another runtime class", "kata", false},
		})
	})
	t.Run("runsc", func(t *testing.T) {
		t.Parallel()
		landPodPolicyCEL(t, "runsc", []landPodRC{
			{"runsc", "runsc", true},
			{"runc", "runc", false},
			{"no runtime class", nil, false},
			{"empty runtime class", "", false},
		})
	})
}

// landPodRC is a tenant pod's runtimeClassName (nil: unset) and whether the
// land pod policy admits it.
type landPodRC struct {
	name string
	rc   any
	ok   bool
}

func landPodPolicyCEL(t *testing.T, runtimeClass string, rcCases []landPodRC) {
	t.Helper()
	set := "landNamespaces.runtimeClassName=" + runtimeClass
	p := landPolicy(t, "test-agents-land-pods", set)
	doc := landDoc(t, renderLandNamespaceRBAC(t, set), "ValidatingAdmissionPolicy", "test-agents-land-pods")
	for _, want := range []string{"failurePolicy: Fail", "key: gardenio.io/land\n          operator: Exists", `resources: ["pods"]`, `operations: ["CREATE", "UPDATE"]`} {
		if !strings.Contains(doc, want) {
			t.Errorf("land pod policy lacks %s:\n%s", want, doc)
		}
	}
	if len(p.Spec.MatchConditions) != 0 {
		t.Errorf("land pod policy is limited to some principals: %+v", p.Spec.MatchConditions)
	}
	fenced := func(mut func(spec map[string]any)) map[string]any {
		spec := map[string]any{
			"serviceAccountName": "land-agent", "automountServiceAccountToken": false,
			"volumes": []any{map[string]any{"name": "workspace", "persistentVolumeClaim": map[string]any{"claimName": "w"}}},
		}
		if runtimeClass != "" {
			spec["runtimeClassName"] = runtimeClass
		}
		if mut != nil {
			mut(spec)
		}
		return map[string]any{"metadata": map[string]any{"name": "p"}, "spec": spec}
	}
	const ctrl = "system:serviceaccount:gardenio:test-agents-sa"
	pod := func(name string, object map[string]any, ok bool) landVAPRequest {
		return landVAPRequest{name: name, user: ctrl, op: "CREATE", resource: "pods", ns: "land-acme", object: object, applied: true, wantAllowed: ok}
	}
	cases := []landVAPRequest{
		pod("fenced tenant pod", fenced(nil), true),
		pod("no volumes", fenced(func(s map[string]any) { delete(s, "volumes") }), true),
		pod("another service account", fenced(func(s map[string]any) { s["serviceAccountName"] = "default" }), false),
		pod("no service account named", fenced(func(s map[string]any) { delete(s, "serviceAccountName") }), false),
		pod("token mounted", fenced(func(s map[string]any) { s["automountServiceAccountToken"] = true }), false),
		pod("token left to the default", fenced(func(s map[string]any) { delete(s, "automountServiceAccountToken") }), false),
		pod("hostPath", fenced(func(s map[string]any) {
			s["volumes"] = []any{map[string]any{"name": "thaw", "hostPath": map[string]any{"path": "/var/lib"}}}
		}), false),
		// A projected token mounts whatever automountServiceAccountToken says
		// (gd-pXik57wZA9).
		pod("projected serviceAccountToken", fenced(func(s map[string]any) {
			s["volumes"] = []any{map[string]any{"name": "ziti", "projected": map[string]any{"sources": []any{
				map[string]any{"configMap": map[string]any{"name": "kube-root-ca.crt"}},
				map[string]any{"serviceAccountToken": map[string]any{"path": "token", "audience": "ziti"}},
			}}}}
		}), false),
		pod("projected configMap only", fenced(func(s map[string]any) {
			s["volumes"] = []any{map[string]any{"name": "ca", "projected": map[string]any{"sources": []any{
				map[string]any{"configMap": map[string]any{"name": "kube-root-ca.crt"}},
			}}}}
		}), true),
	}
	// UPDATE holds the same fence. The runtime class is immutable and is
	// checked on CREATE only, so a changed landNamespaces.runtimeClassName
	// never wedges the controller's patches to the pods already running.
	update := func(name string, object map[string]any, ok bool) landVAPRequest {
		r := pod(name, object, ok)
		r.op = "UPDATE"
		return r
	}
	cases = append(cases,
		update("update of a fenced pod", fenced(nil), true),
		update("update of a pod under another runtime class", fenced(func(s map[string]any) { s["runtimeClassName"] = "kata" }), true),
		update("update with a token mounted", fenced(func(s map[string]any) { s["automountServiceAccountToken"] = true }), false),
		update("update with a projected token", fenced(func(s map[string]any) {
			s["volumes"] = []any{map[string]any{"name": "t", "projected": map[string]any{"sources": []any{
				map[string]any{"serviceAccountToken": map[string]any{"path": "token"}},
			}}}}
		}), false),
	)
	for _, rc := range rcCases {
		cases = append(cases, pod(rc.name, fenced(func(s map[string]any) {
			if rc.rc == nil {
				delete(s, "runtimeClassName")
			} else {
				s["runtimeClassName"] = rc.rc
			}
		}), rc.ok))
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			applied, denied := evalLandVAP(t, p, c)
			if applied != c.applied {
				t.Fatalf("applied = %v, want %v", applied, c.applied)
			}
			if (len(denied) == 0) != c.wantAllowed {
				t.Errorf("denied = %v, want allowed %v", denied, c.wantAllowed)
			}
		})
	}
}

func TestLandNamespaces_ControllerEnv(t *testing.T) {
	t.Parallel()
	out := helmTemplateNS(t, "gardenio", "templates/controller/deployment.yaml",
		"agents.enabled=true", "landNamespaces.enabled=true",
		"landNamespaces.extraExceptCIDRs={fd00::/8,203.0.113.0/24}")
	for _, want := range []string{
		"- name: LANDS_NAMESPACES_ENABLED\n              value: \"true\"",
		"- name: LANDS_CONTROLLER_SERVICE_ACCOUNT\n              value: \"test-agents-sa\"",
		"- name: LANDS_NAMESPACE_EXTRA_EXCEPT_CIDRS\n              value: \"fd00::/8,203.0.113.0/24\"",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("controller env lacks %q", want)
		}
	}
	if strings.Contains(out, "LANDS_NAMESPACE_EGRESS_PEERS") {
		t.Error("egress peers env rendered with no peers set")
	}
	// No land runtime class by default: tenant pods run on the node's
	// default runtime (gd-iyZom5u11A).
	if strings.Contains(out, "LANDS_RUNTIME_CLASS") {
		t.Error("LANDS_RUNTIME_CLASS rendered with landNamespaces.runtimeClassName unset")
	}
	out = helmTemplateNS(t, "gardenio", "templates/controller/deployment.yaml",
		"agents.enabled=true", "landNamespaces.enabled=true", "landNamespaces.runtimeClassName=runsc")
	if want := "- name: LANDS_RUNTIME_CLASS\n              value: \"runsc\""; !strings.Contains(out, want) {
		t.Errorf("controller env lacks %q", want)
	}
}

// landVAPRequest is one admission request to the land namespace policy.
type landVAPRequest struct {
	name                 string
	user                 string
	op, resource, ns     string
	object, oldObject    map[string]any
	applied, wantAllowed bool
}

// evalLandVAP evaluates the rendered policy as the apiserver would:
// matchConditions, then variables in order, then every validation. It
// reports whether the policy applied and the messages of failed validations.
func evalLandVAP(t *testing.T, p vapPolicy, r landVAPRequest) (bool, []string) {
	t.Helper()
	env, err := cel.NewEnv(cel.OptionalTypes(),
		cel.Variable("object", cel.DynType), cel.Variable("oldObject", cel.DynType),
		cel.Variable("request", cel.DynType), cel.Variable("variables", cel.DynType))
	if err != nil {
		t.Fatal(err)
	}
	request := map[string]any{
		"userInfo": map[string]any{"username": r.user}, "operation": r.op,
		"resource": map[string]any{"resource": r.resource}, "namespace": r.ns,
	}
	var object, oldObject any = r.object, r.oldObject
	if r.object == nil {
		object = nil
	}
	if r.oldObject == nil {
		oldObject = nil
	}
	eval := func(expr string, vars map[string]any) any {
		ast, iss := env.Compile(expr)
		if iss.Err() != nil {
			t.Fatalf("CEL compile error in %q: %v", expr, iss.Err())
		}
		prg, err := env.Program(ast)
		if err != nil {
			t.Fatal(err)
		}
		out, _, err := prg.Eval(map[string]any{"object": object, "oldObject": oldObject, "request": request, "variables": vars})
		if err != nil {
			t.Fatalf("CEL eval %q: %v", expr, err)
		}
		return celNative(t, out)
	}
	for _, mc := range p.Spec.MatchConditions {
		if eval(mc.Expression, map[string]any{}) != true {
			return false, nil
		}
	}
	vars := map[string]any{}
	for _, v := range p.Spec.Variables {
		vars[v.Name] = eval(v.Expression, vars)
	}
	var denied []string
	for _, v := range p.Spec.Validations {
		if eval(v.Expression, vars) != true {
			denied = append(denied, strings.Join(strings.Fields(v.Message), " "))
		}
	}
	return true, denied
}

func TestLandNamespaces_AdmissionPolicyCEL(t *testing.T) {
	t.Parallel()
	p := landPolicy(t, "test-agents-lands-namespaces")
	const ctrl = "system:serviceaccount:gardenio:test-agents-sa"
	ns := func(name string, labels map[string]any) map[string]any {
		return map[string]any{"metadata": map[string]any{"name": name, "labels": labels}}
	}
	ours := map[string]any{"gardenio.io/land": "acme", "app.kubernetes.io/managed-by": "gardenio-controller"}
	obj := map[string]any{"metadata": map[string]any{"name": "x"}}
	rb := func(kind, name string) map[string]any {
		return map[string]any{"metadata": map[string]any{"name": "b"}, "roleRef": map[string]any{"kind": kind, "name": name}}
	}

	cases := []landVAPRequest{
		{name: "create land namespace", user: ctrl, op: "CREATE", resource: "namespaces", object: ns("land-acme", ours), applied: true, wantAllowed: true},
		{name: "delete land namespace", user: ctrl, op: "DELETE", resource: "namespaces", oldObject: ns("land-acme", ours), applied: true, wantAllowed: true},
		{name: "delete kube-system", user: ctrl, op: "DELETE", resource: "namespaces", oldObject: ns("kube-system", map[string]any{}), applied: true},
		{name: "delete unlabelled land-x", user: ctrl, op: "DELETE", resource: "namespaces", oldObject: ns("land-acme", nil), applied: true},
		{name: "create land-acme labelled for another land", user: ctrl, op: "CREATE", resource: "namespaces",
			object: ns("land-acme", map[string]any{"gardenio.io/land": "beta", "app.kubernetes.io/managed-by": "gardenio-controller"}), applied: true},
		{name: "quota in a land namespace", user: ctrl, op: "CREATE", resource: "resourcequotas", ns: "land-acme", object: obj, applied: true, wantAllowed: true},
		{name: "network policy in the release namespace", user: ctrl, op: "DELETE", resource: "networkpolicies", ns: "gardenio", oldObject: obj, applied: true},
		{name: "service account in kube-system", user: ctrl, op: "CREATE", resource: "serviceaccounts", ns: "kube-system", object: obj, applied: true},
		{name: "bind the agent manager", user: ctrl, op: "CREATE", resource: "rolebindings", ns: "land-acme", object: rb("ClusterRole", "gardenio-land-agent-manager"), applied: true, wantAllowed: true},
		{name: "bind cluster-admin", user: ctrl, op: "CREATE", resource: "rolebindings", ns: "land-acme", object: rb("ClusterRole", "cluster-admin"), applied: true},
		{name: "bind a Role of the same name", user: ctrl, op: "UPDATE", resource: "rolebindings", ns: "land-acme", object: rb("Role", "gardenio-land-agent-manager"), applied: true},
		{name: "another principal", user: "system:serviceaccount:kube-system:namespace-controller", op: "DELETE", resource: "namespaces", oldObject: ns("kube-system", nil)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			applied, denied := evalLandVAP(t, p, c)
			if applied != c.applied {
				t.Fatalf("applied = %v, want %v", applied, c.applied)
			}
			if applied && (len(denied) == 0) != c.wantAllowed {
				t.Errorf("denied = %v, want allowed %v", denied, c.wantAllowed)
			}
		})
	}
}

// Tenant land pods reach the slack-bridge only on its land listener, which
// serves the per-agent-token routes gb say and gb thread use; the main port
// serves the bridge's whole mux (gd-pXik57wZA9). The controller's land
// namespace egress peer (landns.BridgeLandPort) and tenant pods'
// SLACK_BRIDGE_URL (LANDS_BRIDGE_URL) name the same port the chart opens.
func TestLandNamespaces_BridgeLandListener(t *testing.T) {
	t.Parallel()
	on := []string{"agents.enabled=true", "slackBridge.enabled=true", "networkPolicies.enabled=true", "landNamespaces.enabled=true"}
	render := func(tmpl string, flags []string) string {
		return helmTemplateNS(t, "gardenio", tmpl, flags...)
	}
	checks := []struct {
		tmpl, want string
	}{
		{"templates/slack-bridge/deployment.yaml", "- name: land\n              containerPort: 8091"},
		{"templates/slack-bridge/deployment.yaml", "- name: SLACK_BRIDGE_LAND_LISTEN_ADDR\n              value: \":8091\""},
		{"templates/slack-bridge/service.yaml", "- port: 8091\n      targetPort: land"},
		{"templates/controller/deployment.yaml", "- name: LANDS_BRIDGE_URL\n              value: \"http://test-slack-bridge:8091\""},
		{"templates/networkpolicies/networkpolicies.yaml", "- key: gardenio.io/land\n                operator: Exists\n      ports:\n        - protocol: TCP\n          port: 8091"},
	}
	for _, c := range checks {
		if out := render(c.tmpl, on); !strings.Contains(out, c.want) {
			t.Errorf("%s lacks %q", c.tmpl, c.want)
		}
	}
	off := on[:3]
	for _, c := range checks {
		if out := render(c.tmpl, off); strings.Contains(out, c.want) {
			t.Errorf("%s renders %q with landNamespaces off", c.tmpl, c.want)
		}
	}
}
