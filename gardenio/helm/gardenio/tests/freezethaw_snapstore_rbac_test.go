package tests

import (
	"strings"
	"testing"
)

// Regression guard for ka-0000001694 — the durable ConfigMap-backed snapshot
// store. When gvisor.freezeThaw.enabled=true the controller's in-namespace Role
// must grant configmaps verbs so ConfigMapSnapshotStore can persist/read the
// per-snapshot ConfigMaps (name prefix "gardenio-ftsnap-") that make
// HasSnapshot()/shouldThaw() survive a controller restart. When disabled
// (default), the rule MUST be omitted so the controller's blast radius stays
// minimal.

func TestFreezeThawSnapStoreRBAC_DisabledByDefault(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/controller/rbac.yaml",
		"agents.enabled=true",
	)
	if strings.Contains(out, `resources: ["configmaps"]`) {
		t.Errorf("controller Role must NOT grant configmaps verbs when freeze/thaw is disabled; got:\n%s", out)
	}
}

func TestFreezeThawSnapStoreRBAC_EnabledGrantsConfigMaps(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/controller/rbac.yaml",
		"agents.enabled=true",
		"gvisor.freezeThaw.enabled=true",
	)
	if !strings.Contains(out, `resources: ["configmaps"]`) {
		t.Errorf("controller Role must grant configmaps when freeze/thaw is enabled; got:\n%s", out)
	}
	if !strings.Contains(out, `verbs: ["get", "list", "create", "update", "delete"]`) {
		t.Errorf("controller Role must grant the full configmaps verb set for the snapshot store; got:\n%s", out)
	}
}
