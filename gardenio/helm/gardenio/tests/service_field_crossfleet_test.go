package tests

import (
	"strings"
	"testing"
)

// Cross-environment cooldown-starvation guard (ka-0000006237, epic
// ka-0000005107). The chart wires three pieces:
//   - BEADS_SERVICE_FIELD_FLEETS on the beads daemon (writer-fleet allowlist
//     for the trigger/schedule service-managed-field ownership bypass),
//   - JIRA_TRIGGER_PROJECTS on the jira-bridge (trigger-bead load scoping),
//   - the ServiceFieldForeignFleetWrite PrometheusRule (Alert A).

// TestBeadsServiceFieldFleetsRenders pins that the beads daemon receives the
// writer-fleet allowlist and that it defaults to this deployment's own bridge
// fleets — the sandbox fleet (gardenio-jira-sandbox) is NOT on it, which is
// what denies the foreign last_checked stamp at merge time with no operator
// action (ka-0000006237).
func TestBeadsServiceFieldFleetsRenders(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads/deployment.yaml")
	block := blockAfter(out, "BEADS_SERVICE_FIELD_FLEETS")
	if block == "" {
		t.Fatal("expected BEADS_SERVICE_FIELD_FLEETS env var in rendered beads deployment")
	}
	if !strings.Contains(block, "slack-bridge,jira-bridge,gitlab-bridge") {
		t.Errorf("BEADS_SERVICE_FIELD_FLEETS must default to the deployment's own "+
			"bridge fleets so a foreign-env bridge fleet is denied the "+
			"trigger/schedule last_checked bypass (ka-0000006237); got:\n%s", block)
	}
}

// TestJiraBridgeTriggerProjectsOptIn pins that JIRA_TRIGGER_PROJECTS only
// renders when set (back-compat) and carries the configured value.
func TestJiraBridgeTriggerProjectsOptIn(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/jira-bridge/deployment.yaml",
		"jiraBridge.enabled=true",
	)
	if strings.Contains(out, "JIRA_TRIGGER_PROJECTS") {
		t.Errorf("JIRA_TRIGGER_PROJECTS must not render when triggerProjects is empty (back-compat)")
	}

	out = helmTemplate(t, "templates/jira-bridge/deployment.yaml",
		"jiraBridge.enabled=true",
		"jiraBridge.jira.triggerProjects=monorepo",
	)
	block := blockAfter(out, "JIRA_TRIGGER_PROJECTS")
	if block == "" {
		t.Fatal("expected JIRA_TRIGGER_PROJECTS env var when triggerProjects is set")
	}
	if !strings.Contains(block, "monorepo") {
		t.Errorf("JIRA_TRIGGER_PROJECTS must carry the configured projects; got:\n%s", block)
	}
}

func TestServiceFieldCrossFleetAlert_DefaultDisabled(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "templates/beads/service-field-crossfleet-alert.yaml")
	if err == nil {
		if strings.Contains(out, "kind: PrometheusRule") {
			t.Errorf("PrometheusRule must NOT render when serviceFieldCrossFleet.enabled=false, got:\n%s", out)
		}
		return
	}
	if !strings.Contains(err.Error(), "could not find template") &&
		!strings.Contains(out, "could not find template") {
		t.Errorf("unexpected helm failure: %v\n%s", err, out)
	}
}

func TestServiceFieldCrossFleetAlert_EnabledRendersRule(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads/service-field-crossfleet-alert.yaml",
		"agents.metrics.prometheusRule.serviceFieldCrossFleet.enabled=true",
	)
	if !strings.Contains(out, "kind: PrometheusRule") {
		t.Fatalf("PrometheusRule must render when enabled=true, got:\n%s", out)
	}
	if !strings.Contains(out, "ServiceFieldForeignFleetWrite") {
		t.Errorf("expected ServiceFieldForeignFleetWrite alert, got:\n%s", out)
	}
	// The expr must EXCLUDE the expected writer set — healthy prod polls emit
	// the crossfleet metric too (trigger beads are fleet=default while the
	// prod bridge writes as jira-bridge), so a naive rate()>0 would page on
	// every healthy cycle.
	if !strings.Contains(out, `writer_fleet!~"slack-bridge|jira-bridge|gitlab-bridge|default"`) {
		t.Errorf("alert expr must filter writer_fleet to values outside the expected set, got:\n%s", out)
	}
}
