package bridge

import (
	"context"
	"testing"
)

// TestJiraPoller_CreateBeadFromIssue_CarriesProjectLabelAndCreatedBy pins
// gd-VnWbYkGT59 (ADR docs/adr/taints.md §10.3 / §14 phase 4): a ticket
// ingested by the jira bridge arrives tainted with no bridge-side taint code
// at all — the daemon's generic resolveCreateTaints hook
// (kbeads/internal/server/taint_propagate_create.go) reads the new bead's
// own "project:<name>" label to resolve the project's intake_taints
// (kbeads/internal/server/taint_propagate_project_intake.go, proved against
// this exact request shape by
// TestCreateBead_JiraBridgeShapedRequest_InheritsProjectIntakeTaints).
//
// That server-side mechanism has exactly one input contract on the bridge
// side: the CreateBeadRequest must carry a "project:<name>" label naming the
// project bead that declares intake_taints. This test pins that contract at
// the source — createBeadFromIssue (jira_poller.go) — so a future refactor
// that dropped or renamed the label would fail here even though nothing
// downstream of the daemon boundary would notice (the mock daemon used by
// every other jira_poller test does not run taint logic).
func TestJiraPoller_CreateBeadFromIssue_CarriesProjectLabelAndCreatedBy(t *testing.T) {
	daemon := newMockJiraDaemon()
	poller := NewJiraPoller(newTestJiraClient("http://unused.invalid"), daemon, JiraPollerConfig{
		Projects:   []string{"PE"},
		ProjectMap: map[string]string{"PE": "acme"},
		Logger:     discardLogger(),
	})

	beadID, err := poller.createBeadFromIssue(context.Background(), JiraIssue{
		Key:    "PE-7001",
		Fields: JiraIssueFields{Summary: "ticket ingested from JIRA"},
	})
	if err != nil {
		t.Fatalf("createBeadFromIssue: %v", err)
	}

	bead := daemon.beads[beadID]
	if bead == nil {
		t.Fatalf("bead %s not found in mock daemon", beadID)
	}
	if bead.CreatedBy != "jira-bridge" {
		t.Errorf("CreatedBy = %q; want %q", bead.CreatedBy, "jira-bridge")
	}
	wantLabel := "project:acme"
	found := false
	for _, l := range bead.Labels {
		if l == wantLabel {
			found = true
		}
	}
	if !found {
		t.Errorf("expected label %q (the daemon's project-intake-taint lookup key), got labels: %v", wantLabel, bead.Labels)
	}
}

// TestJiraPoller_CreateBeadFromIssue_ProjectMapFallback pins the same
// contract for the unmapped-prefix fallback: with no ProjectMap entry for
// the JIRA prefix, createBeadFromIssue still emits a "project:<name>" label
// (the lowercased prefix) — so an intake_taints declared on a project bead
// named after the bare prefix still reaches the daemon's resolution.
func TestJiraPoller_CreateBeadFromIssue_ProjectMapFallback(t *testing.T) {
	daemon := newMockJiraDaemon()
	poller := NewJiraPoller(newTestJiraClient("http://unused.invalid"), daemon, JiraPollerConfig{
		Projects: []string{"UP"},
		Logger:   discardLogger(),
	})

	beadID, err := poller.createBeadFromIssue(context.Background(), JiraIssue{
		Key:    "UP-1",
		Fields: JiraIssueFields{Summary: "ticket ingested from JIRA, unmapped project"},
	})
	if err != nil {
		t.Fatalf("createBeadFromIssue: %v", err)
	}

	bead := daemon.beads[beadID]
	if bead == nil {
		t.Fatalf("bead %s not found in mock daemon", beadID)
	}
	wantLabel := "project:up"
	found := false
	for _, l := range bead.Labels {
		if l == wantLabel {
			found = true
		}
	}
	if !found {
		t.Errorf("expected fallback label %q, got labels: %v", wantLabel, bead.Labels)
	}
}
