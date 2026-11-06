package tests

import (
	"os/exec"
	"strings"
	"testing"
)

// Regression guards for templates/project-namespace-rbac.yaml
// (task ka-0000007869, epic ka-0000005677).
//
// This template exists so per-project namespace grants are owned by the chart
// rather than hand-applied. Two properties are
// worth pinning, and they pull in opposite directions:
//
//  1. DEFAULT OFF. The chart's default render must be byte-identical to before
//     this file existed, or every non-prod install grows RBAC nobody asked for.
//  2. RULE FIDELITY for the cs fixture (testdata/, the grant crab-science ran
//     with on the EKS cluster) — its rbac roles/rolebindings verbs are what let a cs pod
//     `helm install` the crab-science chart (escalation ka-0000000617). A
//     narrowed rule set here is a silent regression: the render succeeds, the
//     upgrade succeeds, and cs helm installs start failing later.
//
// Plus the glm-lab property: the operator revoked a wildcard-on-wildcard
// namespace-admin Role on 2026-08-25 and it must not come back by accident.
// The guard is that a bare `enabled: true` flip FAILS the render rather than
// producing an empty-or-inherited Role.

const projectRbacTemplate = "templates/project-namespace-rbac.yaml"

// prodRbacOverlay is a fixture grant set (a cs grant plus an off glm-lab
// grant), relative to this directory because helmProjectRbac execs helm here.
// The live k3s grants are projectNamespaceRbac in deploy-values/gardenio-k3s.yaml.
const prodRbacOverlay = "testdata/project-namespace-rbac-values.yaml"

// helmProjectRbac renders the template with raw helm args (NOT --set-prefixed —
// the shared helmTemplate helper wraps every arg in "--set", which cannot express
// --values). Fails the test on a non-zero exit.
func helmProjectRbac(t *testing.T, args ...string) string {
	t.Helper()
	full := append([]string{"template", "test", chartDir(t), "--show-only", projectRbacTemplate}, args...)
	out, err := exec.Command("helm", full...).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template failed: %v\n%s", err, out)
	}
	return string(out)
}

// helmTemplateExpectError renders with the given flags and returns the combined
// output, requiring a NON-zero exit. Used for the `fail`-guard assertions, which
// helmTemplate cannot express (it t.Fatalf's on any error).
func helmTemplateExpectError(t *testing.T, showOnly string, args ...string) string {
	t.Helper()
	full := []string{"template", "test", chartDir(t)}
	if showOnly != "" {
		full = append(full, "--show-only", showOnly)
	}
	full = append(full, args...)
	out, err := exec.Command("helm", full...).CombinedOutput()
	if err == nil {
		t.Fatalf("expected helm template to FAIL, but it succeeded; got:\n%s", out)
	}
	return string(out)
}

func TestProjectNamespaceRbac_DefaultRendersNothing(t *testing.T) {
	t.Parallel()
	// --show-only on a template that renders nothing is an error in helm, so
	// assert on the whole-chart render instead: neither namespace may appear.
	out, err := exec.Command("helm", "template", "test", chartDir(t)).CombinedOutput()
	if err != nil {
		t.Fatalf("default chart render failed: %v\n%s", err, out)
	}
	for _, forbidden := range []string{"crab-science", "glm-lab"} {
		if strings.Contains(string(out), forbidden) {
			t.Errorf("default render must not contain %q — projectNamespaceRbac ships enabled=false + grants={}", forbidden)
		}
	}
}

func TestProjectNamespaceRbac_EnabledButNoGrantsRendersNothing(t *testing.T) {
	t.Parallel()
	// enabled=true with an empty grants map must still render nothing: the outer
	// gate is `and enabled grants`, so flipping the master flag alone is inert.
	out, err := exec.Command("helm", "template", "test", chartDir(t),
		"--set", "projectNamespaceRbac.enabled=true").CombinedOutput()
	if err != nil {
		t.Fatalf("render failed: %v\n%s", err, out)
	}
	if strings.Contains(string(out), "crab-science") {
		t.Errorf("enabled=true with grants={} must render nothing; got:\n%s", out)
	}
}

func TestProjectNamespaceRbac_MasterGateSuppressesRealGrants(t *testing.T) {
	t.Parallel()
	// The master flag must dominate populated grants, not just an empty map.
	// Mutation-verified: dropping `.enabled` from the outer `and` renders the cs
	// objects even with enabled=false, which this catches and the empty-grants
	// test above does not (an empty range renders nothing either way).
	out, err := exec.Command("helm", "template", "test", chartDir(t),
		"--values="+prodRbacOverlay,
		"--set", "projectNamespaceRbac.enabled=false").CombinedOutput()
	if err != nil {
		t.Fatalf("render failed: %v\n%s", err, out)
	}
	if strings.Contains(string(out), "crab-science-runner") {
		t.Errorf("enabled=false must suppress even fully-populated grants; got:\n%s", out)
	}
}

// csRules is the cs Role's rule set as it exists LIVE on prod-gardenio-eks
// (transcribed 2026-08-25 from the object's own last-applied-configuration, and
// verified equal to the render by kubectl diff — labels only, no rule delta).
//
// Each entry is (resource-marker, verb) pairs that MUST survive. This is
// deliberately spelled out rather than compared to a golden file: the point is
// that a reviewer reading a diff to this list sees a permission change as a
// permission change.
func TestProjectNamespaceRbac_CsRulesMatchLive(t *testing.T) {
	t.Parallel()
	out := helmProjectRbac(t, "--values="+prodRbacOverlay)

	role, binding := splitProjectRbacDocs(t, out)

	// The rbac roles/rolebindings rule is the ka-0000000617 grant — the one that
	// lets a cs pod helm-install a chart shipping its own per-release RBAC.
	// Losing it breaks cs installs, not this render, so it is pinned hardest.
	for _, want := range []string{
		"- rbac.authorization.k8s.io",
		"- roles",
		"- rolebindings",
	} {
		if !strings.Contains(role, want) {
			t.Errorf("cs Role must retain the ka-0000000617 rbac grant (%q missing); got:\n%s", want, role)
		}
	}

	// Every resource the live Role grants. A missing entry is a narrowing.
	for _, res := range []string{"jobs", "secrets", "configmaps", "pods", "pods/log", "events", "serviceaccounts"} {
		if !strings.Contains(role, "- "+res+"\n") {
			t.Errorf("cs Role must grant resource %q (live does); got:\n%s", res, role)
		}
	}

	// The live Role has exactly 7 rules. A count change means somebody merged,
	// split, or dropped a rule — all of which change effective permissions in
	// ways the per-resource checks above can miss.
	if got := strings.Count(role, "\n  - apiGroups:"); got != 7 {
		t.Errorf("cs Role must have exactly 7 rules (matching live); got %d:\n%s", got, role)
	}

	// pods are READ-ONLY live: no create/delete/exec on pods. This asserts the
	// absence of a widening, which the resource checks above cannot.
	if strings.Contains(role, "pods/exec") {
		t.Errorf("cs Role must NOT grant pods/exec — live does not; got:\n%s", role)
	}

	// The subject is the fleet's default spawned-agent SA in gardenio-system. A
	// cs pod reaches ACROSS into crab-science through this binding, so the
	// (name, namespace) pair is the actual security boundary.
	// Line-exact (trailing \n): "name: gardenio-agent" is a PREFIX of
	// "name: gardenio-agent-cluster-admin" and of any typo'd suffix, so a
	// substring check silently accepts the wrong subject — caught by mutation
	// test (renaming the subject passed an unanchored assertion).
	if !strings.Contains(binding, "name: gardenio-agent\n") || !strings.Contains(binding, "namespace: gardenio-system\n") {
		t.Errorf("cs RoleBinding subject must be ServiceAccount gardenio-system/gardenio-agent; got:\n%s", binding)
	}
	if !strings.Contains(binding, "kind: ServiceAccount") {
		t.Errorf("cs RoleBinding subject must be a ServiceAccount (not a Group); got:\n%s", binding)
	}
}

func TestProjectNamespaceRbac_CsObjectIdentityAndNamespace(t *testing.T) {
	t.Parallel()
	out := helmProjectRbac(t, "--values="+prodRbacOverlay)
	role, binding := splitProjectRbacDocs(t, out)

	// Adoption depends on name+namespace matching the live object EXACTLY. A
	// rename does not fail the render — it silently creates a second Role and
	// leaves the live one hand-applied and unowned.
	for _, doc := range []string{role, binding} {
		if !strings.Contains(doc, "name: crab-science-runner\n") {
			t.Errorf("cs objects must be named crab-science-runner to adopt the live ones; got:\n%s", doc)
		}
		if !strings.Contains(doc, "namespace: crab-science\n") {
			t.Errorf("cs objects must live in namespace crab-science; got:\n%s", doc)
		}
		// The inventory label. `kubectl get roles,rolebindings -A -l
		// gardenio.io/project` is the only way these objects are discoverable at
		// all — it is how the sweep that produced this epic found them.
		if !strings.Contains(doc, `gardenio.io/project: "cs"`) {
			t.Errorf("cs objects must carry gardenio.io/project=cs; got:\n%s", doc)
		}
		// Helm ownership metadata — required for `helm upgrade` to adopt the
		// live hand-applied object instead of erroring on ownership.
		if !strings.Contains(doc, "app.kubernetes.io/managed-by: Helm") {
			t.Errorf("cs objects must carry managed-by=Helm (the adoption requirement); got:\n%s", doc)
		}
	}

	if !strings.Contains(binding, "name: crab-science-runner\n") {
		t.Errorf("binding roleRef must point at the cs Role; got:\n%s", binding)
	}
}

func TestProjectNamespaceRbac_GlmLabOffByDefaultInProdOverlay(t *testing.T) {
	t.Parallel()
	// The operator revoked glm-lab's wildcard grant on 2026-08-25 and both live
	// objects are deleted. The overlay must represent it WITHOUT rendering it.
	out := helmProjectRbac(t, "--values="+prodRbacOverlay)

	if strings.Contains(out, "glm-lab") {
		t.Errorf("glm-lab must render nothing (revoked 2026-08-25, enabled:false); got:\n%s", out)
	}
	// And nothing wildcard leaked in from anywhere.
	if strings.Contains(out, `- "*"`) || strings.Contains(out, "- '*'") {
		t.Errorf("no rendered grant may use wildcard resources/verbs; got:\n%s", out)
	}
}

func TestProjectNamespaceRbac_GlmLabBareFlipFails(t *testing.T) {
	t.Parallel()
	// THE POINT OF THE PLACEHOLDER: flipping glm-lab on without stating rules
	// must FAIL the render, not quietly produce a Role. If this ever starts
	// succeeding, a one-line values flip silently re-grants something.
	out := helmTemplateExpectError(t, projectRbacTemplate,
		"--values="+prodRbacOverlay,
		"--set", "projectNamespaceRbac.grants.glm-lab-agent-admin.enabled=true")

	if !strings.Contains(out, "declares no `rules`") {
		t.Errorf("bare glm-lab flip must fail with the empty-rules guard; got:\n%s", out)
	}
}

func TestProjectNamespaceRbac_EnabledGrantWithoutNamespaceFails(t *testing.T) {
	t.Parallel()
	// Namespace creation is out of scope, so a grant with no namespace has
	// nowhere to land. Failing loudly beats rendering into .Release.Namespace,
	// which would put a project's grant in the control-plane namespace.
	out := helmTemplateExpectError(t, projectRbacTemplate,
		"--set", "projectNamespaceRbac.enabled=true",
		"--set", "projectNamespaceRbac.grants.orphan.enabled=true")

	if !strings.Contains(out, "has no `namespace`") {
		t.Errorf("namespace-less grant must fail the render; got:\n%s", out)
	}
}

func TestProjectNamespaceRbac_BindingWithoutSubjectsFails(t *testing.T) {
	t.Parallel()
	// A RoleBinding with no subjects binds a Role to nobody — an object that
	// looks like a working grant and grants nothing. Fail rather than render it.
	out := helmTemplateExpectError(t, projectRbacTemplate,
		"--set", "projectNamespaceRbac.enabled=true",
		"--set", "projectNamespaceRbac.grants.g.enabled=true",
		"--set", "projectNamespaceRbac.grants.g.namespace=somens",
		"--set", "projectNamespaceRbac.grants.g.rules[0].apiGroups[0]=",
		"--set", "projectNamespaceRbac.grants.g.rules[0].resources[0]=configmaps",
		"--set", "projectNamespaceRbac.grants.g.rules[0].verbs[0]=get")

	if !strings.Contains(out, "declares no `subjects`") {
		t.Errorf("subject-less RoleBinding must fail the render; got:\n%s", out)
	}
}

func TestProjectNamespaceRbac_PerObjectCreateFlags(t *testing.T) {
	t.Parallel()
	// The per-object create flags are what make adoption landable one object at
	// a time (gvisor-namespace.yaml precedent): an operator managing one half
	// elsewhere can opt just that half out without a flag day.
	base := []string{"--values=" + prodRbacOverlay}

	roleOnly := helmProjectRbac(t, append(base,
		"--set", "projectNamespaceRbac.grants.crab-science-runner.roleBinding.create=false")...)
	if strings.Contains(roleOnly, "kind: RoleBinding") {
		t.Errorf("roleBinding.create=false must suppress the RoleBinding; got:\n%s", roleOnly)
	}
	if !strings.Contains(roleOnly, "\nkind: Role\n") {
		t.Errorf("roleBinding.create=false must leave the Role rendering; got:\n%s", roleOnly)
	}

	bindingOnly := helmProjectRbac(t, append(base,
		"--set", "projectNamespaceRbac.grants.crab-science-runner.role.create=false")...)
	if strings.Contains(bindingOnly, "\nkind: Role\n") { // column-anchored: roleRef's indented "kind: Role" is not a Role doc
		t.Errorf("role.create=false must suppress the Role; got:\n%s", bindingOnly)
	}
	if !strings.Contains(bindingOnly, "kind: RoleBinding") {
		t.Errorf("role.create=false must leave the RoleBinding rendering; got:\n%s", bindingOnly)
	}
	// The suppressed Role's NAME must still be the roleRef target, or the
	// binding points at nothing.
	if !strings.Contains(bindingOnly, "name: crab-science-runner\n") {
		t.Errorf("roleRef must still name the (externally-managed) Role; got:\n%s", bindingOnly)
	}
}

// splitProjectRbacDocs returns the Role doc and the RoleBinding doc from a
// render. Asserting against the whole render would let a Role-only property
// pass on text that actually came from the RoleBinding.
func splitProjectRbacDocs(t *testing.T, out string) (role, binding string) {
	t.Helper()
	for _, doc := range strings.Split(out, "\n---\n") {
		// Scope to the cs objects: a second enabled grant renders more docs, and
		// "last one wins" would silently assert cs properties against another
		// project's Role.
		if !strings.Contains(doc, "namespace: crab-science\n") {
			continue
		}
		// Anchor to column 0: a RoleBinding's roleRef contains an INDENTED
		// "  kind: Role", which an unanchored match would misclassify as the
		// Role document.
		switch {
		case strings.Contains(doc, "\nkind: RoleBinding\n"):
			binding = doc
		case strings.Contains(doc, "\nkind: Role\n"):
			role = doc
		}
	}
	if role == "" {
		t.Fatalf("no Role rendered; got:\n%s", out)
	}
	if binding == "" {
		t.Fatalf("no RoleBinding rendered; got:\n%s", out)
	}
	return role, binding
}
