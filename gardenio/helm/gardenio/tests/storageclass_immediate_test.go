package tests

import (
	"strings"
	"testing"
)

// Regression guards for ka-0000004683 / ka-0000002370 — the deadlock-safe
// Immediate-binding StorageClass (templates/storageclass-immediate.yaml).
//
// Background: the shared cluster-scoped `gardenio-gp3` class is
// volumeBindingMode: WaitForFirstConsumer (WFC). A BRAND-NEW WFC PVC does not
// bind until a consumer pod is scheduled, but the prod deploy runs the chart
// through an atomic helm_release with --wait, which waits for every PVC to bind
// BEFORE it schedules the consumer Deployments. So the first slack-bridge PVC
// stayed Pending, --wait errored after ~60s, and the atomic rollback deleted
// the fresh PVC — every retry started over (prod stranded on 2026.209.6 ~11h;
// MR !6257 disabled slack-bridge persistence to unblock).
//
// The deadlock-safe fix: a dedicated `gardenio-gp3-immediate` class with
// volumeBindingMode: Immediate, which provisions AND binds the volume at
// PVC-creation time, so the PVC is Bound before --wait inspects the Deployment.
// These tests pin the load-bearing properties so the fix cannot silently
// regress: (1) off by default so ephemeral releases never claim the
// cluster-scoped object, (2) Immediate binding (the whole point), and (3) the
// keep resource-policy so orphaning (not deletion) is the failure mode.

func TestStorageClassImmediate_DefaultDisabled(t *testing.T) {
	t.Parallel()
	// Base chart defaults (storageClassImmediate.create=false) must render
	// nothing — a staging/ephemeral release using the base default can never
	// apply the cluster-scoped SC and hijack its ownership annotations, exactly
	// like the gardenio-gp3 guard (TestStorageClass_DefaultDisabled).
	out, err := helmTemplateAllowFail(t, "templates/storageclass-immediate.yaml")
	rendersNothing(t, out, err)
}

func TestStorageClassImmediate_RendersWhenCreateTrue(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/storageclass-immediate.yaml",
		"storageClassImmediate.create=true",
	)
	if !strings.Contains(out, "kind: StorageClass") {
		t.Fatalf("expected a StorageClass when create=true; got:\n%s", out)
	}
	if !strings.Contains(out, "name: gardenio-gp3-immediate") {
		t.Errorf("expected the gardenio-gp3-immediate name; got:\n%s", out)
	}
	if !strings.Contains(out, "provisioner: ebs.csi.aws.com") {
		t.Errorf("expected the EBS CSI provisioner; got:\n%s", out)
	}
	// The whole point of this class: Immediate binding, so a brand-new PVC binds
	// at creation time instead of deadlocking WaitForFirstConsumer inside --wait.
	if !strings.Contains(out, "volumeBindingMode: Immediate") {
		t.Errorf("expected volumeBindingMode: Immediate (the deadlock-safe property); got:\n%s", out)
	}
	// The rendered directive must NOT be WFC (that is the deadlock). Match the
	// actual `volumeBindingMode:` line — the explanatory comment legitimately
	// mentions WaitForFirstConsumer, so a bare substring check would false-fail.
	if strings.Contains(out, "volumeBindingMode: WaitForFirstConsumer") {
		t.Errorf("this class must NOT be WaitForFirstConsumer — that is the deadlock; got:\n%s", out)
	}
	// keep annotation: a release that later stops templating this cluster-scoped
	// SC orphans it instead of deleting other releases' storage backing.
	if !strings.Contains(out, `"helm.sh/resource-policy": keep`) {
		t.Errorf("expected helm.sh/resource-policy: keep on the cluster-scoped SC; got:\n%s", out)
	}
}

func TestStorageClassImmediate_TagsRenderAsTagSpecifications(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/storageclass-immediate.yaml",
		"storageClassImmediate.create=true",
		"storageClassImmediate.tags[0].key=environment",
		"storageClassImmediate.tags[0].value=aws-poc",
	)
	if !strings.Contains(out, `tagSpecification_1: "environment=aws-poc"`) {
		t.Errorf("expected tag rendered as tagSpecification_1; got:\n%s", out)
	}
}

func TestStorageClassImmediate_AllowedTopologiesOptional(t *testing.T) {
	t.Parallel()
	// Default: no allowedTopologies stanza (EBS CSI may provision in any AZ with
	// schedulable nodes).
	base := helmTemplate(t, "templates/storageclass-immediate.yaml",
		"storageClassImmediate.create=true",
	)
	if strings.Contains(base, "allowedTopologies:") {
		t.Errorf("allowedTopologies must be absent by default; got:\n%s", base)
	}
	// When set, it renders (AZ-pin escape hatch for a capacity edge).
	pinned := helmTemplate(t, "templates/storageclass-immediate.yaml",
		"storageClassImmediate.create=true",
		"storageClassImmediate.allowedTopologies[0].matchLabelExpressions[0].key=topology.ebs.csi.aws.com/zone",
		"storageClassImmediate.allowedTopologies[0].matchLabelExpressions[0].values[0]=us-east-1a",
	)
	if !strings.Contains(pinned, "allowedTopologies:") ||
		!strings.Contains(pinned, "topology.ebs.csi.aws.com/zone") ||
		!strings.Contains(pinned, "us-east-1a") {
		t.Errorf("expected allowedTopologies to render when set; got:\n%s", pinned)
	}
}
