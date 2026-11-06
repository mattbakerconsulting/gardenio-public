package tests

import (
	"strings"
	"testing"
)

// Regression guards for ka-0000008151 — the cluster-scoped gardenio-gp3
// StorageClass (templates/storageclass.yaml).
//
// Background: a staging/ephemeral release (the RWX-era gardenio-rwx) once rendered &
// applied this cluster-scoped SC, stamping it with
// meta.helm.sh/release-name=gardenio-rwx. That made the prod `gardenio` release's
// `helm upgrade` refuse to adopt it ("invalid ownership metadata ... must equal
// gardenio"), GATING every prod deploy (prod stuck on .18 while goldens piled
// up).
//
// The durable fix (b7582cdfd) is twofold and these tests pin both halves so the
// gating regression cannot silently return:
//   1. create:false by default — only the prod overlay (deploy-values/gardenio.yaml)
//      flips create:true, so staging/ephemeral releases never claim ownership.
//   2. helm.sh/resource-policy: keep — a release that stops templating the SC
//      (e.g. staging with create:false, or an uninstall) ORPHANS it rather than
//      deleting the SC that other releases' PVCs depend on.

func TestStorageClass_DefaultDisabled(t *testing.T) {
	t.Parallel()
	// Base chart defaults (values.yaml: storageClass.create=false) must render
	// nothing — so a staging/ephemeral release using the base default can never
	// apply the cluster-scoped SC and hijack its ownership annotations.
	out, err := helmTemplateAllowFail(t, "templates/storageclass.yaml")
	rendersNothing(t, out, err)
}

func TestStorageClass_RendersWhenCreateTrue(t *testing.T) {
	t.Parallel()
	// The prod overlay sets create:true. Pin the rendered shape: the named
	// cluster-scoped SC with the keep policy.
	out := helmTemplate(t, "templates/storageclass.yaml",
		"storageClass.create=true",
	)
	if !strings.Contains(out, "kind: StorageClass") {
		t.Fatalf("expected a StorageClass when create=true; got:\n%s", out)
	}
	if !strings.Contains(out, "name: gardenio-gp3") {
		t.Errorf("expected the gardenio-gp3 name; got:\n%s", out)
	}
	if !strings.Contains(out, "provisioner: ebs.csi.aws.com") {
		t.Errorf("expected the EBS CSI provisioner; got:\n%s", out)
	}
	// keep annotation: a release that later stops templating this cluster-scoped
	// SC orphans it instead of deleting other releases' storage backing.
	if !strings.Contains(out, `"helm.sh/resource-policy": keep`) {
		t.Errorf("expected helm.sh/resource-policy: keep on the cluster-scoped SC; got:\n%s", out)
	}
}
