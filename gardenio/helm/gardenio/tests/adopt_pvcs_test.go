package tests

import (
	"strings"
	"testing"
)

// pre-upgrade-adopt-pvcs.yaml renders an ensure_pvc call per enabled PVC.
// Regression: gitlab-bridge wedged Pending in prod when its PVC was absent
// after a helm upgrade (ka-0000004481). The hook now create-if-missing in
// addition to adopting existing PVCs.

func TestAdoptPVCs_GitlabBridgePVCIncluded(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/pre-upgrade-adopt-pvcs.yaml",
		"gitlabBridge.enabled=true",
		"gitlabBridge.persistence.enabled=true",
		"gitlabBridge.persistence.size=200Mi",
		"gitlabBridge.persistence.storageClass=gp2",
	)
	if !strings.Contains(out, `ensure_pvc "test-gitlab-bridge-state" "200Mi" "gp2"`) {
		t.Errorf("expected ensure_pvc call for gitlab-bridge-state with size/sc; got:\n%s", out)
	}
}

func TestAdoptPVCs_SlackBridgePVCIncluded(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/pre-upgrade-adopt-pvcs.yaml",
		"slackBridge.enabled=true",
		"slackBridge.slack.secretName=foo",
		"slackBridge.persistence.enabled=true",
		"slackBridge.persistence.size=300Mi",
		"slackBridge.persistence.storageClass=gp3",
	)
	if !strings.Contains(out, `ensure_pvc "test-slack-bridge-state" "300Mi" "gp3"`) {
		t.Errorf("expected ensure_pvc call for slack-bridge-state with size/sc; got:\n%s", out)
	}
}

func TestAdoptPVCs_OmittedWhenPersistenceDisabled(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/pre-upgrade-adopt-pvcs.yaml",
		"gitlabBridge.enabled=true",
		"gitlabBridge.persistence.enabled=false",
	)
	if strings.Contains(out, "gitlab-bridge-state") {
		t.Errorf("PVC should NOT be templated when persistence.enabled=false; got:\n%s", out)
	}
}

func TestAdoptPVCs_StorageClassFallsBackToGlobal(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/pre-upgrade-adopt-pvcs.yaml",
		"gitlabBridge.enabled=true",
		"gitlabBridge.persistence.enabled=true",
		"global.storageClass=ebs-csi-gp3",
	)
	if !strings.Contains(out, `ensure_pvc "test-gitlab-bridge-state" "100Mi" "ebs-csi-gp3"`) {
		t.Errorf("expected global.storageClass fallback; got:\n%s", out)
	}
}

func TestAdoptPVCs_RBACIncludesCreateVerb(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/pre-upgrade-adopt-pvcs.yaml")
	// Role rules must include "create" so the Job can apply missing PVCs.
	roleSection := out
	if !strings.Contains(roleSection, `verbs: ["get", "list", "patch", "create"]`) {
		t.Errorf("Role must grant create on PVCs; got:\n%s", out)
	}
}
