package main

import (
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/steward"
)

func TestStewardKillLand(t *testing.T) {
	for _, c := range []struct {
		name, role, project, targetProject, targetRole string
		ok                                             bool
	}{
		{"steward kills its gardener", "steward", "tomato-patch", "tomato-patch", "lands-gardener", true},
		{"steward kills any agent of its land", "steward", "tomato-patch", "tomato-patch", "crew", true},
		{"not another land's", "steward", "tomato-patch", "other-land", "lands-gardener", false},
		{"not a steward", "steward", "tomato-patch", "tomato-patch", "steward", false},
		{"a gardener is no steward", "lands-gardener", "tomato-patch", "tomato-patch", "lands-gardener", false},
		{"no land", "steward", "", "", "lands-gardener", false},
	} {
		t.Setenv("BOAT_ROLE", c.role)
		t.Setenv("BOAT_PROJECT", c.project)
		land, ok := stewardKillLand(c.targetProject, c.targetRole)
		if ok != c.ok || (ok && land != c.project) {
			t.Errorf("%s: got %q, %v; want %v", c.name, land, ok, c.ok)
		}
	}
}

func TestStewardKillLine_NamesTheGrant(t *testing.T) {
	a := &beadsapi.AgentBead{AgentName: "tp-gardener-1", Metadata: map[string]string{"task_id": "tp-step-2"}}
	line := stewardKillLine(a, "no change in 50 minutes and no answer", steward.AuthorityMergeOnGreen)
	for _, want := range []string{"tp-gardener-1", "tp-step-2", "no answer", "under grant merge-on-green"} {
		if !strings.Contains(line, want) {
			t.Errorf("line %q lacks %q", line, want)
		}
	}
}
