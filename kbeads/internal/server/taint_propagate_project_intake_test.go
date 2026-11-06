package server

import (
	"encoding/json"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

// TestCreateBead_ProjectIntakeTaintsUnknownNameRejected pins decision
// gd-H9L64SF3Bk: intake_taints is validated at project write time, not a
// silent forward reference — a name that does not resolve to an existing
// taint is a hard reject (HTTP 400), and the project bead is never persisted.
func TestCreateBead_ProjectIntakeTaintsUnknownNameRejected(t *testing.T) {
	s, ms, _ := newTestServer()
	_, err := s.createBead(ctxAsStaticAdmin(), createBeadInput{
		Title:  "gardenio",
		Type:   "project",
		Fields: json.RawMessage(`{"intake_taints":["jira-prod"]}`),
	})
	if err == nil {
		t.Fatal("expected inputError for intake_taints naming an unknown taint, got nil")
	}
	if _, ok := err.(inputError); !ok {
		t.Fatalf("expected inputError (→HTTP 400), got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "jira-prod") {
		t.Errorf("error should name the unresolved taint: %v", err)
	}
	for id, b := range ms.beads {
		if b.Type == "project" {
			t.Fatalf("rejected project bead %s must NOT be persisted", id)
		}
	}
}

// TestCreateBead_ProjectIntakeTaintsValidPersisted proves the opposite: an
// intake_taints entry naming a taint that already exists is accepted.
func TestCreateBead_ProjectIntakeTaintsValidPersisted(t *testing.T) {
	s, ms, _ := newTestServer()
	ms.taintRegistry = map[string]*model.Taint{"jira-prod": {ID: 1, Name: "jira-prod"}}

	bead, err := s.createBead(ctxAsStaticAdmin(), createBeadInput{
		Title:  "gardenio",
		Type:   "project",
		Fields: json.RawMessage(`{"intake_taints":["jira-prod"]}`),
	})
	if err != nil {
		t.Fatalf("valid intake_taints should be accepted: %v", err)
	}
	if ms.beads[bead.ID] == nil {
		t.Fatalf("project bead %s not persisted", bead.ID)
	}
}

// TestCreateBead_UnderProjectIntake_InheritsProjectIntakeTaints pins ADR
// docs/adr/taints.md §10.3: a bead created under a project (via the
// project:<name> label) that declares intake_taints carries those taints
// from creation, source=inherit:project-intake, source_bead_id=the project
// bead — distinguishable from inherit:create/inherit:parent per the
// propagation-source UI acceptance criterion.
func TestCreateBead_UnderProjectIntake_InheritsProjectIntakeTaints(t *testing.T) {
	s, ms, _ := newTestServer()
	ms.taintRegistry = map[string]*model.Taint{"jira-prod": {ID: 7, Name: "jira-prod"}}

	project, err := s.createBead(ctxAsStaticAdmin(), createBeadInput{
		Title:  "gardenio",
		Type:   "project",
		Fields: json.RawMessage(`{"intake_taints":["jira-prod"]}`),
	})
	if err != nil {
		t.Fatalf("creating project bead: %v", err)
	}

	bead, err := s.createBead(ctxAsTestSubject(), createBeadInput{
		Title:  "ticket from jira",
		Type:   "task",
		Labels: []string{"project:gardenio"},
	})
	if err != nil {
		t.Fatalf("creating bead under project: %v", err)
	}

	rows := ms.beadTaints[bead.ID]
	row, ok := beadTaintByID(rows, 7)
	if !ok {
		t.Fatalf("expected bead to inherit project intake taint 7, got rows %v", rows)
	}
	if row.Source != model.TaintSourceInheritProjectIntake {
		t.Errorf("Source = %q; want %q", row.Source, model.TaintSourceInheritProjectIntake)
	}
	if row.SourceBeadID != project.ID {
		t.Errorf("SourceBeadID = %q; want project bead id %q", row.SourceBeadID, project.ID)
	}
}

// TestCreateBead_UnderProjectIntake_FieldsProjectFallback proves the
// project:<name> label is not the only way to scope a bead to a project: the
// fields.project fallback (mirroring resolveDedupProject's precedent) also
// resolves intake taints.
func TestCreateBead_UnderProjectIntake_FieldsProjectFallback(t *testing.T) {
	s, ms, _ := newTestServer()
	ms.taintRegistry = map[string]*model.Taint{"jira-prod": {ID: 3, Name: "jira-prod"}}

	project, err := s.createBead(ctxAsStaticAdmin(), createBeadInput{
		Title:  "gardenio",
		Type:   "project",
		Fields: json.RawMessage(`{"intake_taints":["jira-prod"]}`),
	})
	if err != nil {
		t.Fatalf("creating project bead: %v", err)
	}

	bead, err := s.createBead(ctxAsTestSubject(), createBeadInput{
		Title:  "ticket from jira",
		Type:   "event",
		Fields: json.RawMessage(`{"project":"gardenio"}`),
	})
	if err != nil {
		t.Fatalf("creating bead under project via fields.project: %v", err)
	}

	rows := ms.beadTaints[bead.ID]
	if _, ok := beadTaintByID(rows, 3); !ok {
		t.Fatalf("expected bead to inherit project intake taint 3 via fields.project, got rows %v", rows)
	}
	_ = project
}

// TestCreateBead_UnderProjectNoIntake_Unaffected proves a bead created under
// a project with no intake_taints (or no project at all) carries no
// project-intake taint rows — the zero-config default of §10.1.
func TestCreateBead_UnderProjectNoIntake_Unaffected(t *testing.T) {
	s, ms, _ := newTestServer()

	_, err := s.createBead(ctxAsStaticAdmin(), createBeadInput{
		Title: "gardenio",
		Type:  "project",
	})
	if err != nil {
		t.Fatalf("creating project bead: %v", err)
	}

	bead, err := s.createBead(ctxAsTestSubject(), createBeadInput{
		Title:  "unrelated task",
		Type:   "task",
		Labels: []string{"project:gardenio"},
	})
	if err != nil {
		t.Fatalf("creating bead under project: %v", err)
	}

	if rows := ms.beadTaints[bead.ID]; len(rows) != 0 {
		t.Fatalf("expected no taint rows, got %v", rows)
	}
}

// TestUpdateBead_ProjectIntakeTaintsUnchangedNotWedged mirrors the
// grandfather semantics of the sibling shape validators: a legacy project
// bead whose intake_taints already names a since-deleted taint is not
// wedged against an unrelated-field PATCH.
func TestUpdateBead_ProjectIntakeTaintsUnchangedNotWedged(t *testing.T) {
	s, ms, _ := newTestServer()
	const id = "gd-legacyintake"
	seedProjectBead(ms, id, "gardenio", `{"intake_taints":["deleted-taint"]}`)

	_, err := s.updateBead(ctxAsStaticAdmin(), id, updateBeadInput{
		Fields: json.RawMessage(`{"model":"claude-opus-4-8"}`),
	})
	if err != nil {
		t.Fatalf("unrelated-field PATCH on a legacy bead with a dangling intake_taints must not be wedged, got: %v", err)
	}
}

// TestCreateBead_ProjectIntakeTaints_EmptyRegistryShortCircuit pins ADR
// §10.1's zero-configuration short-circuit for the project-intake path: on
// an install with no taints created yet (empty registry), creating a bead
// under a project bead must not issue a single FindProjectByName store
// query — resolveProjectIntakeTaints returns early on
// taintRegistryNonEmpty, before ever resolving the project. This is the
// same hot-path guarantee beadTaintQueryCalls pins for the read gate,
// applied to the create-time apply path (send-back on PR #64).
func TestCreateBead_ProjectIntakeTaints_EmptyRegistryShortCircuit(t *testing.T) {
	s, ms, _ := newTestServer()

	_, err := s.createBead(ctxAsStaticAdmin(), createBeadInput{
		Title: "gardenio",
		Type:  "project",
	})
	if err != nil {
		t.Fatalf("creating project bead: %v", err)
	}
	ms.findProjectByNameCalls = 0 // reset: the create above may itself resolve a project

	_, err = s.createBead(ctxAsTestSubject(), createBeadInput{
		Title:  "unrelated task",
		Type:   "task",
		Labels: []string{"project:gardenio"},
	})
	if err != nil {
		t.Fatalf("creating bead under project: %v", err)
	}

	if ms.findProjectByNameCalls != 0 {
		t.Errorf("findProjectByNameCalls = %d; want 0 (taintRegistryNonEmpty must short-circuit before FindProjectByName on an empty registry)", ms.findProjectByNameCalls)
	}
}

// TestUpdateBead_ProjectIntakeTaintsChangedUnknownRejected proves the
// create-time guarantee still holds on update: introducing or changing
// intake_taints to name an unknown taint is hard-rejected.
func TestUpdateBead_ProjectIntakeTaintsChangedUnknownRejected(t *testing.T) {
	s, ms, _ := newTestServer()
	const id = "gd-cleanintake"
	seedProjectBead(ms, id, "gardenio", `{}`)

	_, err := s.updateBead(ctxAsStaticAdmin(), id, updateBeadInput{
		Fields: json.RawMessage(`{"intake_taints":["jira-prod"]}`),
	})
	if err == nil {
		t.Fatal("newly-introduced intake_taints naming an unknown taint must be rejected")
	}
	if _, ok := err.(inputError); !ok {
		t.Fatalf("expected inputError (→HTTP 400), got %T: %v", err, err)
	}
}
