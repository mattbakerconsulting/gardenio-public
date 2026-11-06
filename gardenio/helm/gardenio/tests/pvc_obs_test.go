package tests

import (
	"strings"
	"testing"
)

// ka-0000002737: the controller emits gardenio_orphaned_pending_pvcs via the
// pvcobs collector. The chart contract enforced here:
//   - agents.pvcObs is OFF by default → neither the persistentvolumeclaims:list
//     + pods:list ClusterRole (templates/controller/pvc-obs-rbac.yaml) nor the
//     PVC_OBS_INTERVAL controller env render (collector stays disabled, SA gains
//     no cluster PVC/pod access),
//   - flipping agents.pvcObs.enabled renders BOTH in lockstep,
//   - the ClusterRole grants ONLY list on persistentvolumeclaims + pods (minimal
//     blast radius, never get/watch/create/delete — the collector is read-only),
//   - the ClusterRoleBinding subject is the controller SA,
//   - PVC_OBS_THRESHOLD renders only when agents.pvcObs.threshold is set.

func TestPVCObs_DisabledByDefault_NoRBAC(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "", "agents.enabled=true")
	if err != nil {
		t.Fatalf("helm template (full chart) failed: %v\n%s", err, out)
	}
	if strings.Contains(out, "pvc-obs") {
		t.Errorf("pvc-obs RBAC must NOT render when agents.pvcObs is disabled; got:\n%s", out)
	}
}

func TestPVCObs_DisabledByDefault_NoEnv(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/controller/deployment.yaml",
		"agents.enabled=true",
	)
	if strings.Contains(out, "PVC_OBS_INTERVAL") {
		t.Errorf("PVC_OBS_INTERVAL must NOT be set when agents.pvcObs is disabled; got env in:\n%s", out)
	}
}

func TestPVCObs_EnabledRendersRBAC(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/controller/pvc-obs-rbac.yaml",
		"agents.enabled=true",
		"agents.pvcObs.enabled=true",
	)
	if !strings.Contains(out, "kind: ClusterRole") {
		t.Fatalf("expected a ClusterRole when pvcObs enabled; got:\n%s", out)
	}
	if !strings.Contains(out, "kind: ClusterRoleBinding") {
		t.Fatalf("expected a ClusterRoleBinding when pvcObs enabled; got:\n%s", out)
	}
	if !strings.Contains(out, `resources: ["persistentvolumeclaims", "pods"]`) {
		t.Errorf("ClusterRole must grant the persistentvolumeclaims + pods resources; got:\n%s", out)
	}
	// Minimal blast radius: every `verbs:` line must be exactly ["list"] — no
	// mutating or broader read verbs. Scanned per-verbs-line so prose in
	// comments does not trip the check.
	for line := range strings.SplitSeq(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "verbs:") && trimmed != `verbs: ["list"]` {
			t.Errorf("every verbs line must be exactly [\"list\"], got %q in:\n%s", trimmed, out)
		}
	}
	// Bound to the controller SA (default name <release>-agents-sa).
	if !strings.Contains(out, "test-agents-sa") {
		t.Errorf("ClusterRoleBinding subject must be the controller SA (test-agents-sa); got:\n%s", out)
	}
}

func TestPVCObs_EnabledRendersEnv(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/controller/deployment.yaml",
		"agents.enabled=true",
		"agents.pvcObs.enabled=true",
	)
	if !strings.Contains(out, "PVC_OBS_INTERVAL") {
		t.Fatalf("PVC_OBS_INTERVAL must be set when pvcObs enabled; got:\n%s", out)
	}
	// Default interval from values.yaml.
	if !strings.Contains(out, `value: "15m"`) {
		t.Errorf("expected default interval 15m in the env; got:\n%s", out)
	}
	// Threshold is empty by default → PVC_OBS_THRESHOLD must NOT render (the
	// collector falls back to its built-in 7d default).
	if strings.Contains(out, "PVC_OBS_THRESHOLD") {
		t.Errorf("PVC_OBS_THRESHOLD must NOT render when threshold is empty; got:\n%s", out)
	}
}

func TestPVCObs_CustomThresholdRendersEnv(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/controller/deployment.yaml",
		"agents.enabled=true",
		"agents.pvcObs.enabled=true",
		"agents.pvcObs.threshold=48h",
	)
	if !strings.Contains(out, "PVC_OBS_THRESHOLD") {
		t.Fatalf("PVC_OBS_THRESHOLD must be set when threshold provided; got:\n%s", out)
	}
	if !strings.Contains(out, `value: "48h"`) {
		t.Errorf("expected custom threshold 48h; got:\n%s", out)
	}
}

// The OrphanedPendingPVCs vmalert rule renders only when the blocked-gap toggle
// vmalert.gapAlerts.pvcObs.enabled is on (and a notifier is wired). Its series
// prereq is the collector (agents.pvcObs.enabled), documented in prereqBead.
func TestPVCObs_AlertRuleRendersWhenEnabled(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmalert/configmap.yaml",
		append(gapAlertsNotifierArgs, "vmalert.gapAlerts.pvcObs.enabled=true")...)
	if !strings.Contains(out, "OrphanedPendingPVCs") {
		t.Fatalf("expected the OrphanedPendingPVCs alert when pvcObs gap alert enabled; got:\n%s", out)
	}
	if !strings.Contains(out, "gardenio_orphaned_pending_pvcs >") {
		t.Errorf("alert expr must reference the gardenio_orphaned_pending_pvcs gauge; got:\n%s", out)
	}
}

func TestPVCObs_AlertRuleAbsentByDefault(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmalert/configmap.yaml", gapAlertsNotifierArgs...)
	if strings.Contains(out, "OrphanedPendingPVCs") {
		t.Errorf("OrphanedPendingPVCs alert must NOT render when pvcObs gap alert is disabled; got:\n%s", out)
	}
}
