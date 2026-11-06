package tests

import (
	"strings"
	"testing"
)

// The beads-viewer template sets BOAT_PROJECT from
// `.Values.beadsViewer.project | default .Values.global.project`. That value
// feeds TWO consumers: the gardenio_operator_activity_total metric label
// (ka-0000006752) and the /claude-config effective-config resolution scope.
// Until this task, global.project was never declared in values.yaml, so a
// default render silently emitted no BOAT_PROJECT at all.

func TestBeadsViewer_Project_DefaultEmitsNoBoatProject(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads-viewer/deployment.yaml",
		"beadsViewer.enabled=true",
		"beadsViewer.oidc.issuerURL=https://auth.example.com/realms/x",
		"beadsViewer.oidc.clientID=viewer",
	)
	if strings.Contains(out, "BOAT_PROJECT") {
		t.Errorf("a default render must not emit BOAT_PROJECT (no project configured), got:\n%s", out)
	}
}

func TestBeadsViewer_Project_GlobalProjectEmitsEnv(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads-viewer/deployment.yaml",
		"beadsViewer.enabled=true",
		"beadsViewer.oidc.issuerURL=https://auth.example.com/realms/x",
		"beadsViewer.oidc.clientID=viewer",
		"global.project=gardenio",
	)
	if !strings.Contains(out, "BOAT_PROJECT") {
		t.Fatalf("global.project must render BOAT_PROJECT on beads-viewer, got:\n%s", out)
	}
	if !strings.Contains(out, `value: "gardenio"`) {
		t.Errorf("BOAT_PROJECT should carry the global.project value, got:\n%s", out)
	}
}

func TestBeadsViewer_Project_ViewerOverrideWins(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads-viewer/deployment.yaml",
		"beadsViewer.enabled=true",
		"beadsViewer.oidc.issuerURL=https://auth.example.com/realms/x",
		"beadsViewer.oidc.clientID=viewer",
		"global.project=fleetwide",
		"beadsViewer.project=viewerscoped",
	)
	if !strings.Contains(out, `value: "viewerscoped"`) {
		t.Errorf("beadsViewer.project must win over global.project, got:\n%s", out)
	}
	if strings.Contains(out, `value: "fleetwide"`) {
		t.Errorf("global.project must not leak through when the viewer override is set, got:\n%s", out)
	}
}

// The live k3s deployment must render BOAT_PROJECT=gardenio so /claude-config
// resolves the project-scoped claude-settings beads pods actually receive.
func TestBeadsViewer_Project_DeployValuesK3sSetsGardenio(t *testing.T) {
	t.Parallel()
	out := helmTemplateShowOnlyWithValues(t,
		"templates/beads-viewer/deployment.yaml",
		[]string{"../../../deploy-values/gardenio-k3s.yaml"},
		"beadsViewer.enabled=true",
		"beadsViewer.oidc.issuerURL=https://auth.example.com/realms/x",
		"beadsViewer.oidc.clientID=viewer",
	)
	if !strings.Contains(out, "BOAT_PROJECT") {
		t.Fatalf("deploy-values/gardenio-k3s.yaml must set a viewer project, got:\n%s", out)
	}
	if !strings.Contains(out, `value: "gardenio"`) {
		t.Errorf("the k3s deployment's viewer project must be gardenio, got:\n%s", out)
	}
}
