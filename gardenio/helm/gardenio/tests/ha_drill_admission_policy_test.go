package tests

import (
	"strings"
	"testing"
)

// Regression guards for gd-JKXEwmGns8: the ValidatingAdmissionPolicy
// (templates/controller/ha-drill-admission-policy.yaml) that narrows the
// gardenio-ha-drill-operator SA's node UPDATE verb (granted by
// templates/controller/ha-drill-rbac.yaml) down to spec.unschedulable-only
// changes on spot worker nodes. RBAC alone cannot express this scoping
// (raised by fimba-40 on gd-JCLx5RLG1I, decision gd-KmJX79f3ff) — these
// tests pin the CEL shape so a future edit cannot silently widen the SA's
// blast radius back out to arbitrary node labels/annotations/taints, or to
// on-demand/control-plane nodes.

const haDrillVAPTemplate = "templates/controller/ha-drill-admission-policy.yaml"

func renderHaDrillVAP(t *testing.T, extraFlags ...string) string {
	t.Helper()
	flags := append([]string{
		"agents.enabled=true",
		"agents.haDrillOperator.enabled=true",
		"agents.haDrillOperator.admissionPolicy.enabled=true",
	}, extraFlags...)
	return helmTemplateNS(t, haDrillRelNS, haDrillVAPTemplate, flags...)
}

func TestHaDrillVAP_OmittedWhenHaDrillOperatorDisabled(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, haDrillVAPTemplate,
		"agents.enabled=true",
		"agents.haDrillOperator.enabled=false",
	)
	if err == nil && strings.Contains(out, "kind: ValidatingAdmissionPolicy") {
		t.Errorf("VAP must NOT render when haDrillOperator.enabled=false; got:\n%s", out)
	}
}

func TestHaDrillVAP_OmittedWhenPolicyDisabled(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, haDrillVAPTemplate,
		"agents.enabled=true",
		"agents.haDrillOperator.enabled=true",
		"agents.haDrillOperator.admissionPolicy.enabled=false",
	)
	if err == nil && strings.Contains(out, "kind: ValidatingAdmissionPolicy") {
		t.Errorf("VAP must NOT render when admissionPolicy.enabled=false; got:\n%s", out)
	}
}

func TestHaDrillVAP_EnabledByDefaultOnceHaDrillOperatorIsOn(t *testing.T) {
	t.Parallel()
	// admissionPolicy.enabled defaults to true — an operator turning on
	// haDrillOperator without an explicit override still gets the VAP.
	out := helmTemplateNS(t, haDrillRelNS, haDrillVAPTemplate,
		"agents.enabled=true",
		"agents.haDrillOperator.enabled=true",
	)
	if !strings.Contains(out, "kind: ValidatingAdmissionPolicy") {
		t.Fatalf("VAP must render by default once haDrillOperator.enabled=true; got:\n%s", out)
	}
}

func TestHaDrillVAP_ScopedToNodeUpdatesOnly(t *testing.T) {
	t.Parallel()
	out := renderHaDrillVAP(t)
	idx := strings.Index(out, "kind: ValidatingAdmissionPolicy\n")
	if idx < 0 {
		t.Fatalf("no ValidatingAdmissionPolicy found in:\n%s", out)
	}
	policy := out[idx:]
	if !strings.Contains(policy, `resources: ["nodes"]`) {
		t.Errorf("VAP must scope to nodes; got:\n%s", policy)
	}
	if !strings.Contains(policy, `operations: ["UPDATE"]`) {
		t.Errorf("VAP must scope to UPDATE only; got:\n%s", policy)
	}
	if !strings.Contains(policy, "failurePolicy: Fail") {
		t.Errorf("VAP must fail closed; got:\n%s", policy)
	}
}

func TestHaDrillVAP_MatchConditionTargetsOnlyTheDedicatedSA(t *testing.T) {
	t.Parallel()
	out := renderHaDrillVAP(t)
	if !strings.Contains(out, "system:serviceaccount:"+haDrillRelNS+":gardenio-ha-drill-operator") {
		t.Errorf("VAP matchCondition must target the dedicated ha-drill-operator SA; got:\n%s", out)
	}
}

func TestHaDrillVAP_RejectsLabelAnnotationTaintChanges(t *testing.T) {
	t.Parallel()
	out := renderHaDrillVAP(t)
	for _, want := range []string{
		"object.metadata.labels == oldObject.metadata.labels",
		"object.metadata.annotations == oldObject.metadata.annotations",
		"object.spec.taints == oldObject.spec.taints",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("VAP must compare %q between old and new node objects; got:\n%s", want, out)
		}
	}
}

// gd-JKXEwmGns8 merge-desk send-back #2: the field-scope check must reject
// an attempted ownerReference or finalizer mutation, not just
// labels/annotations/taints -- a dangling ownerReference lets the garbage
// collector delete the Node (a delete this SA is never granted, bypassing
// PDBs), and an added finalizer can wedge Node deletion during a real spot
// reclaim. Both are reachable through this SA's `nodes patch` verb unless
// the CEL pins them.
func TestHaDrillVAP_RejectsOwnerReferenceAndFinalizerChanges(t *testing.T) {
	t.Parallel()
	out := renderHaDrillVAP(t)
	for _, want := range []string{
		"object.metadata.ownerReferences == oldObject.metadata.ownerReferences",
		"object.metadata.finalizers == oldObject.metadata.finalizers",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("VAP must compare %q between old and new node objects; got:\n%s", want, out)
		}
	}
}

// gd-JKXEwmGns8 merge-desk send-back #2: the field-scope check must pin
// every NodeSpec field other than unschedulable, not just taints -- a
// change to podCIDR, podCIDRs, or providerID was previously unrestricted.
func TestHaDrillVAP_RejectsOtherSpecFieldChanges(t *testing.T) {
	t.Parallel()
	out := renderHaDrillVAP(t)
	for _, want := range []string{
		"object.spec.podCIDR == oldObject.spec.podCIDR",
		"object.spec.podCIDRs == oldObject.spec.podCIDRs",
		"object.spec.providerID == oldObject.spec.providerID",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("VAP must compare %q between old and new node objects; got:\n%s", want, out)
		}
	}
}

func TestHaDrillVAP_RestrictsToSpotNonControlPlaneNodes(t *testing.T) {
	t.Parallel()
	out := renderHaDrillVAP(t)
	if !strings.Contains(out, "oldObject.metadata.labels['gardenio.io/lifecycle'] == 'spot'") {
		t.Errorf("VAP must require the target node be labeled gardenio.io/lifecycle=spot; got:\n%s", out)
	}
	if !strings.Contains(out, "!('node-role.kubernetes.io/control-plane' in oldObject.metadata.labels)") {
		t.Errorf("VAP must reject control-plane-labeled nodes; got:\n%s", out)
	}
}

func TestHaDrillVAP_BindingDenies(t *testing.T) {
	t.Parallel()
	out := renderHaDrillVAP(t)
	if !strings.Contains(out, "kind: ValidatingAdmissionPolicyBinding") {
		t.Fatalf("expected a ValidatingAdmissionPolicyBinding; got:\n%s", out)
	}
	if !strings.Contains(out, `validationActions: ["Deny"]`) {
		t.Errorf("binding must enforce Deny, not just Audit; got:\n%s", out)
	}
}

func TestHaDrillVAP_NameOverrideFlowsThroughMatchCondition(t *testing.T) {
	t.Parallel()
	out := renderHaDrillVAP(t, "agents.haDrillOperator.name=custom-ha-drill-sa")
	if !strings.Contains(out, "system:serviceaccount:"+haDrillRelNS+":custom-ha-drill-sa") {
		t.Errorf("VAP matchCondition must use the overridden SA name; got:\n%s", out)
	}
	if strings.Contains(out, "system:serviceaccount:"+haDrillRelNS+":gardenio-ha-drill-operator") {
		t.Errorf("default SA name must not leak through when overridden; got:\n%s", out)
	}
}
