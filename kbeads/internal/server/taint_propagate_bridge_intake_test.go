package server

import (
	"encoding/json"
	"testing"

	"gardenio/kbeads/internal/model"
)

// TestCreateBead_JiraBridgeShapedRequest_InheritsProjectIntakeTaints pins
// gd-VnWbYkGT59 (ADR docs/adr/taints.md §10.3 / §14 phase 4): a bead created
// with the EXACT request shape the jira bridge sends —
// gardenio/controller/internal/bridge/jira_poller.go's createBeadFromIssue,
// CreatedBy "jira-bridge", a "project:<name>" label alongside "source:jira"
// and "jira:<KEY>" — carries its project's intake_taints from creation, with
// no bridge-side taint code involved: resolveCreateTaints (the same hook
// every createBead call goes through) reads the project:<name> label off
// this create-time request exactly as it would any other caller's.
//
// This is deliberately NOT a retest of
// TestCreateBead_UnderProjectIntake_InheritsProjectIntakeTaints (which pins
// the generic mechanism with a bare Labels: []string{"project:gardenio"}
// request) — it pins that the bridge's own request shape satisfies that
// mechanism's input contract, so a future edit to jira_poller.go that
// dropped or renamed the project label would be caught here even though the
// generic-mechanism test would still pass untouched.
func TestCreateBead_JiraBridgeShapedRequest_InheritsProjectIntakeTaints(t *testing.T) {
	s, ms, _ := newTestServer()
	ms.taintRegistry = map[string]*model.Taint{"jira-prod": {ID: 11, Name: "jira-prod"}}

	project, err := s.createBead(ctxAsStaticAdmin(), createBeadInput{
		Title:  "acme",
		Type:   "project",
		Fields: json.RawMessage(`{"intake_taints":["jira-prod"]}`),
	})
	if err != nil {
		t.Fatalf("creating project bead: %v", err)
	}

	bead, err := s.createBead(ctxAsTestSubject(), createBeadInput{
		Title:       "[PE-7001] Ticket ingested from JIRA",
		Type:        "task",
		Kind:        "issue",
		Description: "issue body",
		Labels:      []string{"source:jira", "jira:PE-7001", "project:acme"},
		Priority:    2,
		CreatedBy:   "jira-bridge",
		Fields:      json.RawMessage(`{"jira_key":"PE-7001"}`),
	})
	if err != nil {
		t.Fatalf("creating jira-bridge-shaped bead: %v", err)
	}

	rows := ms.beadTaints[bead.ID]
	row, ok := beadTaintByID(rows, 11)
	if !ok {
		t.Fatalf("expected the jira-bridge-shaped bead to inherit project intake taint 11, got rows %v", rows)
	}
	if row.Source != model.TaintSourceInheritProjectIntake {
		t.Errorf("Source = %q; want %q", row.Source, model.TaintSourceInheritProjectIntake)
	}
	if row.SourceBeadID != project.ID {
		t.Errorf("SourceBeadID = %q; want project bead id %q", row.SourceBeadID, project.ID)
	}
}

// TestCreateBead_GitlabBridgeShapedRequest_InheritsProjectIntakeTaints is
// the gitlab-bridge counterpart, mirroring
// gardenio/controller/internal/bridge/gitlab_mentions.go's mention-created
// task bead: CreatedBy "gitlab-bridge", labels
// []string{"project:<name>", "gitlab-mention"}.
func TestCreateBead_GitlabBridgeShapedRequest_InheritsProjectIntakeTaints(t *testing.T) {
	s, ms, _ := newTestServer()
	ms.taintRegistry = map[string]*model.Taint{"acme-prod": {ID: 12, Name: "acme-prod"}}

	project, err := s.createBead(ctxAsStaticAdmin(), createBeadInput{
		Title:  "acme",
		Type:   "project",
		Fields: json.RawMessage(`{"intake_taints":["acme-prod"]}`),
	})
	if err != nil {
		t.Fatalf("creating project bead: %v", err)
	}

	bead, err := s.createBead(ctxAsTestSubject(), createBeadInput{
		Title:       "@gb please look at this",
		Type:        "task",
		Description: "mentioned in a GitLab MR discussion",
		Labels:      []string{"project:acme", "gitlab-mention"},
		Priority:    2,
		CreatedBy:   "gitlab-bridge",
	})
	if err != nil {
		t.Fatalf("creating gitlab-bridge-shaped bead: %v", err)
	}

	rows := ms.beadTaints[bead.ID]
	row, ok := beadTaintByID(rows, 12)
	if !ok {
		t.Fatalf("expected the gitlab-bridge-shaped bead to inherit project intake taint 12, got rows %v", rows)
	}
	if row.Source != model.TaintSourceInheritProjectIntake {
		t.Errorf("Source = %q; want %q", row.Source, model.TaintSourceInheritProjectIntake)
	}
	if row.SourceBeadID != project.ID {
		t.Errorf("SourceBeadID = %q; want project bead id %q", row.SourceBeadID, project.ID)
	}
}

// TestCreateBead_BridgeShapedRequest_NoIntakeTaints_Unaffected proves AC3: a
// ticket ingested (jira-bridge-shaped request) under a project with no
// intake_taints arrives untainted, same as the direct-create case
// (TestCreateBead_UnderProjectNoIntake_Unaffected).
func TestCreateBead_BridgeShapedRequest_NoIntakeTaints_Unaffected(t *testing.T) {
	s, ms, _ := newTestServer()
	// Registry non-empty (another taint exists) so the zero-config
	// short-circuit doesn't mask a real bug in the no-intake-declared path.
	ms.taintRegistry = map[string]*model.Taint{"jira-prod": {ID: 13, Name: "jira-prod"}}

	if _, err := s.createBead(ctxAsStaticAdmin(), createBeadInput{
		Title: "untainted-project",
		Type:  "project",
	}); err != nil {
		t.Fatalf("creating project bead: %v", err)
	}

	bead, err := s.createBead(ctxAsTestSubject(), createBeadInput{
		Title:     "[UP-1] Ticket ingested from JIRA",
		Type:      "task",
		Kind:      "issue",
		Labels:    []string{"source:jira", "jira:UP-1", "project:untainted-project"},
		CreatedBy: "jira-bridge",
	})
	if err != nil {
		t.Fatalf("creating jira-bridge-shaped bead: %v", err)
	}

	if rows := ms.beadTaints[bead.ID]; len(rows) != 0 {
		t.Fatalf("expected no taint rows for a project with no intake_taints, got %v", rows)
	}
}
