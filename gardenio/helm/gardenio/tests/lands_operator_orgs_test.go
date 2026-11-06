package tests

import (
	"strings"
	"testing"
)

// Operator GitHub orgs (gd-72m3STiqya): one list, agents.lands.operatorGithubOrgs,
// feeds the beads daemon's pin-time refusal and the controller's mint-time
// refusal. The daemon gets it by default; the controller gets it with the
// Lands App (the provisioner block), so a default controller still renders no
// LANDS_* env (TestLands_DefaultRendersNoEnvOrVolumes).

func TestLandsOperatorOrgs_BeadsDefault(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads/deployment.yaml")
	if got := envValue(t, out, "BEADS_LANDS_OPERATOR_GITHUB_ORGS"); got != "mattbakerconsulting,joyousaversion" {
		t.Errorf("BEADS_LANDS_OPERATOR_GITHUB_ORGS = %q, want the chart's operator orgs", got)
	}
}

func TestLandsOperatorOrgs_Override(t *testing.T) {
	t.Parallel()
	set := "agents.lands.operatorGithubOrgs={mattbakerconsulting,claimlayer-org}"
	if got := envValue(t, helmTemplate(t, "templates/beads/deployment.yaml", set), "BEADS_LANDS_OPERATOR_GITHUB_ORGS"); got != "mattbakerconsulting,claimlayer-org" {
		t.Errorf("beads: BEADS_LANDS_OPERATOR_GITHUB_ORGS = %q", got)
	}
	_, d := renderControllerPodSpec(t, landsProvOn+","+set)
	got := ""
	for _, e := range d.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "LANDS_OPERATOR_GITHUB_ORGS" {
			got = e.Value
		}
	}
	if got != "mattbakerconsulting,claimlayer-org" {
		t.Errorf("controller: LANDS_OPERATOR_GITHUB_ORGS = %q, want the same list as the daemon", got)
	}
}

func TestLandsOperatorOrgs_EmptyRendersNothing(t *testing.T) {
	t.Parallel()
	set := "agents.lands.operatorGithubOrgs=null"
	if out := helmTemplate(t, "templates/beads/deployment.yaml", set); strings.Contains(out, "BEADS_LANDS_OPERATOR_GITHUB_ORGS") {
		t.Error("an empty list must render no BEADS_LANDS_OPERATOR_GITHUB_ORGS")
	}
	if out, _ := renderControllerPodSpec(t, landsProvOn+","+set); strings.Contains(out, "LANDS_OPERATOR_GITHUB_ORGS") {
		t.Error("an empty list must render no LANDS_OPERATOR_GITHUB_ORGS")
	}
}

func TestLandsOperatorOrgs_ControllerRidesWithLandsApp(t *testing.T) {
	t.Parallel()
	out, _ := renderControllerPodSpec(t, landsProvOn)
	if got := envValue(t, out, "LANDS_OPERATOR_GITHUB_ORGS"); got != "mattbakerconsulting,joyousaversion" {
		t.Errorf("LANDS_OPERATOR_GITHUB_ORGS = %q, want the chart's operator orgs", got)
	}
}
