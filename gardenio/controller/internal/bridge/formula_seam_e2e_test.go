package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// This file proves the FSM §7→§8 seam END-TO-END as a single flow, which no
// existing test does. The two halves of the seam are each covered in isolation:
//
//   - The POUR half — TestE2E_ApplyFormula_CrossPourEdge
//     (formula_instantiate_crosspour_test.go) drives a real apply_formula
//     evaluation and asserts the poured molecule's entry step gets a cross-pour
//     `blocks` edge onto the upstream emit + a deferred suggest_new_agent stamp.
//     It STOPS there — it never runs a dispatch pass.
//
//   - The DISPATCH half — the backfill tests in formula_step_dispatch_test.go
//     (TestStepDispatch_BackfillDispatchesMissedStep et al.) prove
//     BackfillReadySteps spawns a role-matched agent for a ready step, but they
//     HAND-BUILD the step bead (readyStep + addBlocks) rather than obtain it
//     from a real pour, and they use a DIFFERENT fake daemon (fakeStepDaemon).
//
// The seam the plan (ka-0000005928) actually ships is the JOIN of the two: a
// repro-emit close pours the fix-proof molecule AND the dispatcher then spawns
// its step-1 agent — through ONE daemon, exactly as cmd/trigger-engine/main.go
// wires them (NewFormulaStepDispatcher(daemon, ...) shares the client the engine
// pours through). This test exercises that join: a real EvaluateSource pour
// whose emitted step beads + edges + opt-in stamp are then consumed by a real
// FormulaStepDispatcher.BackfillReadySteps, producing a role:monorepo-fix spawn
// stamped dispatched_by=formula-step-dispatcher. It is the CI-durable complement
// to the operator-gated LIVE run (ka-0000007685).

// seamDaemon is a single in-memory daemon that faithfully satisfies BOTH the
// trigger-engine's TriggerBeadClient (the pour half) and the dispatcher's
// stepDispatchDaemon (the dispatch half). The two production paths run against
// the SAME client, so the test does too — a step created by the pour is a step
// the dispatcher can hydrate, filter, and unblock.
//
// It materializes CreateBead into a queryable bead map (with fields decoded from
// the JSON blob), records dependency edges in both directions, and mirrors every
// field stamp onto the stored bead — the three things the shared mockTriggerDaemon
// deliberately does NOT do, which is exactly why no cross-half test could exist
// before. Kept local to this file so the 38 tests that share mockTriggerDaemon
// are untouched.
type seamDaemon struct {
	mu sync.Mutex

	beads   map[string]*beadsapi.BeadDetail  // id -> bead, materialized on CreateBead
	fwdDeps map[string][]beadsapi.Dependency // beadID -> forward deps ("beadID blocks-on X")
	revDeps map[string][]beadsapi.Dependency // dependsOnID -> reverse deps ("X blocks-on beadID")
	nextID  int

	// Recorded spawns (the dispatch half's observable output).
	spawnedNames   []string
	spawnedRoles   []string
	spawnedProject []string
	spawnedTaskIDs []string
	spawnedFields  []map[string]string
}

func newSeamDaemon(seed ...*beadsapi.BeadDetail) *seamDaemon {
	d := &seamDaemon{
		beads:   make(map[string]*beadsapi.BeadDetail),
		fwdDeps: make(map[string][]beadsapi.Dependency),
		revDeps: make(map[string][]beadsapi.Dependency),
	}
	for _, b := range seed {
		if b != nil {
			d.beads[b.ID] = b
		}
	}
	return d
}

// --- TriggerBeadClient (pour half) ---

func (d *seamDaemon) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []*beadsapi.BeadDetail
	for _, b := range d.beads {
		if !seamMatchTypes(b, q.Types) {
			continue
		}
		if !seamMatchStatuses(b, q.Statuses) {
			continue
		}
		if !seamMatchLabels(b, q.Labels) {
			continue
		}
		if !seamMatchFields(b, q.Fields) {
			continue
		}
		out = append(out, b)
	}
	return &beadsapi.ListBeadsResult{Beads: out, Total: len(out)}, nil
}

func (d *seamDaemon) CreateBead(_ context.Context, req beadsapi.CreateBeadRequest) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	// Decode the fields blob the pour writes (source_formula/source_step_id/
	// molecule_id/step_role/step_project/… and, for non-entry steps,
	// suggest_new_agent) into the map[string]string projection GetBead returns —
	// exactly the shape the daemon renders and the dispatcher reads.
	fields := seamDecodeFields(req.Fields)

	d.nextID++
	id := fmt.Sprintf("gd-seam-%d", d.nextID)
	d.beads[id] = &beadsapi.BeadDetail{
		ID:       id,
		Title:    req.Title,
		Type:     req.Type,
		Status:   "open",
		Assignee: req.Assignee,
		Labels:   append([]string(nil), req.Labels...),
		Fields:   fields,
	}
	return id, nil
}

func (d *seamDaemon) UpdateBeadFields(_ context.Context, beadID string, fields map[string]string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.applyFieldsLocked(beadID, fields)
	return nil
}

func (d *seamDaemon) UpdateBeadFieldsTyped(_ context.Context, beadID string, fields map[string]any) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	str := make(map[string]string, len(fields))
	for k, v := range fields {
		str[k] = fmt.Sprintf("%v", v)
	}
	d.applyFieldsLocked(beadID, str)
	return nil
}

func (d *seamDaemon) applyFieldsLocked(beadID string, fields map[string]string) {
	b := d.beads[beadID]
	if b == nil {
		return
	}
	if b.Fields == nil {
		b.Fields = map[string]string{}
	}
	maps.Copy(b.Fields, fields)
}

func (d *seamDaemon) AddDependency(_ context.Context, beadID, dependsOnID, depType, _ string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	dep := beadsapi.Dependency{BeadID: beadID, DependsOnID: dependsOnID, Type: depType}
	d.fwdDeps[beadID] = append(d.fwdDeps[beadID], dep)
	d.revDeps[dependsOnID] = append(d.revDeps[dependsOnID], dep)
	return nil
}

func (d *seamDaemon) SpawnAgent(_ context.Context, agentName, project, taskID, role, _ string, extraFields ...map[string]string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.spawnedNames = append(d.spawnedNames, agentName)
	d.spawnedRoles = append(d.spawnedRoles, role)
	d.spawnedProject = append(d.spawnedProject, project)
	d.spawnedTaskIDs = append(d.spawnedTaskIDs, taskID)
	merged := map[string]string{}
	for _, ef := range extraFields {
		maps.Copy(merged, ef)
	}
	d.spawnedFields = append(d.spawnedFields, merged)
	return "gd-agent-" + agentName, nil
}

func (d *seamDaemon) ListProjectBeads(_ context.Context) (map[string]beadsapi.ProjectInfo, error) {
	return map[string]beadsapi.ProjectInfo{}, nil
}

func (d *seamDaemon) GetBead(_ context.Context, beadID string) (*beadsapi.BeadDetail, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if b := d.beads[beadID]; b != nil {
		return b, nil
	}
	return nil, fmt.Errorf("bead %s not found", beadID)
}

func (d *seamDaemon) AddComment(_ context.Context, _, _, _ string) error { return nil }
func (d *seamDaemon) AddLabel(_ context.Context, _, _ string) error      { return nil }

func (d *seamDaemon) CloseBead(_ context.Context, beadID string, _ map[string]string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if b := d.beads[beadID]; b != nil {
		b.Status = "closed"
	}
	return nil
}

func (d *seamDaemon) UpdateBead(_ context.Context, beadID string, req beadsapi.UpdateBeadRequest) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if b := d.beads[beadID]; b != nil {
		if req.Status != nil {
			b.Status = *req.Status
		}
		if req.Assignee != nil {
			b.Assignee = *req.Assignee
		}
	}
	return nil
}

func (d *seamDaemon) MRBeadForTask(_ context.Context, _ string) (*beadsapi.BeadDetail, error) {
	return nil, nil
}

// --- dependency-graph reads shared by both halves ---

func (d *seamDaemon) GetDependencies(_ context.Context, beadID string) ([]beadsapi.Dependency, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]beadsapi.Dependency(nil), d.fwdDeps[beadID]...), nil
}

func (d *seamDaemon) GetReverseDependencies(_ context.Context, beadID string) ([]beadsapi.Dependency, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]beadsapi.Dependency(nil), d.revDeps[beadID]...), nil
}

func (d *seamDaemon) BatchGetDependencies(_ context.Context, ids []string, direction string) (map[string][]beadsapi.Dependency, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[string][]beadsapi.Dependency, len(ids))
	for _, id := range ids {
		if direction == "reverse" {
			out[id] = append([]beadsapi.Dependency(nil), d.revDeps[id]...)
		} else {
			out[id] = append([]beadsapi.Dependency(nil), d.fwdDeps[id]...)
		}
	}
	return out, nil
}

func (d *seamDaemon) BatchGetBeads(_ context.Context, ids []string) (map[string]*beadsapi.BeadDetail, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[string]*beadsapi.BeadDetail, len(ids))
	for _, id := range ids {
		if b := d.beads[id]; b != nil {
			out[id] = b
		}
	}
	return out, nil
}

// --- small local matchers (faithful subset of the daemon list filter) ---

func seamMatchTypes(b *beadsapi.BeadDetail, types []string) bool {
	return len(types) == 0 || slices.Contains(types, b.Type)
}

func seamMatchStatuses(b *beadsapi.BeadDetail, statuses []string) bool {
	return len(statuses) == 0 || b.Status == "" || slices.Contains(statuses, b.Status)
}

func seamMatchLabels(b *beadsapi.BeadDetail, labels []string) bool {
	for _, want := range labels {
		if !slices.Contains(b.Labels, want) {
			return false
		}
	}
	return true
}

func seamMatchFields(b *beadsapi.BeadDetail, fields map[string]string) bool {
	for k, v := range fields {
		if b.Fields[k] != v {
			return false
		}
	}
	return true
}

// seamDecodeFields mirrors the daemon's fields-blob → map[string]string
// projection: every value is stringified, so a JSON boolean true renders as
// "true" (the same shape stepDispatchFieldTrue and the list filter compare
// against).
func seamDecodeFields(blob []byte) map[string]string {
	if len(blob) == 0 {
		return map[string]string{}
	}
	var raw map[string]any
	if err := json.Unmarshal(blob, &raw); err != nil {
		return map[string]string{}
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		out[k] = fmt.Sprintf("%v", v)
	}
	return out
}

// seamFixProofFormula is the downstream molecule of the §7→§8 handoff: a
// fix-proof formula whose entry step opts into its own role:monorepo-fix agent.
// Mirrors the real fix-proof formula (ka-0000006208) shape the pour trigger
// ka-0000002857 targets: an opted-in entry step plus a dependent step that does
// NOT opt in (so the test also proves the dispatcher fires for step 1 only).
func seamFixProofFormula(id string) *beadsapi.BeadDetail {
	steps := `[` +
		`{"id":"extract-inputs-and-fix","title":"Extract inputs and prove fix for {{TICKET}}","roles":["monorepo-fix"],"suggest_new_agent":true},` +
		`{"id":"land-proof","title":"Land the proof for {{TICKET}}","depends_on":["extract-inputs-and-fix"],"roles":["monorepo-fix"]}` +
		`]`
	return &beadsapi.BeadDetail{
		ID:    id,
		Type:  "formula",
		Title: "Fix-proof for {{TICKET}}",
		Fields: map[string]string{
			"vars":  `[{"name":"TICKET"}]`,
			"steps": steps,
		},
	}
}

// TestE2E_Seam_PourThenDispatch is the end-to-end proof: a repro-emit CLOSE
// pours the fix-proof molecule (cross-pour edge + deferred opt-in), and the
// real backfill dispatcher then spawns a role:monorepo-fix agent for its entry
// step — the whole §7→§8 seam, through one daemon, with no live cluster.
func TestE2E_Seam_PourThenDispatch(t *testing.T) {
	const (
		formulaID = "gd-seam-fixproof-formula"
		triggerID = "gd-seam-pour-trigger"
		emitID    = "gd-seam-repro-emit" // the upstream repro-emit step that closes
		project   = "gardenio"
	)

	// Seed the CLOSED upstream emit bead itself: in production the repro-emit
	// step is a real bead that has already closed (its close is what fires the
	// pour), so stillBlocked can hydrate it and find it terminal. Without it the
	// dispatcher fails CLOSED on an unreadable blocker — the correct safety
	// behavior, but not the state we are proving here.
	emitBead := &beadsapi.BeadDetail{
		ID:     emitID,
		Type:   "task",
		Title:  "repro emit step (gate passed)",
		Status: "closed",
		Labels: []string{"project:" + project},
		Fields: map[string]string{
			"source_formula": "gd-repro-harness",
			"source_step_id": "emit-repro-harness",
			"gate_passed":    "true",
			"repro_ticket":   "FSM-472",
		},
	}
	daemon := newSeamDaemon(seamFixProofFormula(formulaID), emitBead)
	engine := NewTriggerEngine(TriggerEngineConfig{Daemon: daemon, Logger: quietLogger()})

	// --- POUR HALF: fire the real apply_formula trigger on a repro-emit close. ---
	src := NewBeadStateTriggerSource(quietLogger())
	engine.RegisterSource(src)
	src.mu.Lock()
	src.pending = []beadStateEvent{{
		Topic: "beads.bead.closed",
		Bead: &BeadEvent{
			ID:     emitID,
			Type:   "task",
			Title:  "repro emit step (gate passed)",
			Status: "closed",
			Labels: []string{"project:" + project},
			Fields: map[string]string{
				"source_formula": "gd-repro-harness",
				"source_step_id": "emit-repro-harness",
				"gate_passed":    "true",
				"repro_ticket":   "FSM-472",
			},
		},
	}}
	src.mu.Unlock()

	// The trigger mirrors the authored fix-proof pour trigger ka-0000002857:
	// apply_formula on a repro-emit close, cross_pour_upstream_step={{bead_id}}
	// so the poured entry step chains onto the closing emit.
	triggerFields := map[string]string{
		"action":                   "apply_formula",
		"event_types":              "closed",
		"conditions":               `[{"field":"type","op":"==","value":"task"},{"field":"gate_passed","op":"==","value":"true"}]`,
		"formula_id":               formulaID,
		"formula_vars":             `{"TICKET":"{{repro_ticket}}"}`,
		"cross_pour_upstream_step": "{{bead_id}}",
		"project":                  project,
	}
	daemon.mu.Lock()
	daemon.beads[triggerID] = triggerBead(triggerID, "Pour fix-proof on repro-emit close", "trigger:bead", triggerFields)
	daemon.mu.Unlock()

	engine.EvaluateSource(context.Background(), "bead")

	// The pour must have produced a fix-proof molecule with an opted-in entry
	// step carrying the dispatch metadata, chained to the upstream emit. Locate
	// the entry step by the fields the pour stamps.
	entry := daemon.findStep(t, func(b *beadsapi.BeadDetail) bool {
		return b.Fields["source_step_id"] == "extract-inputs-and-fix"
	})
	if entry.Fields["source_formula"] != formulaID {
		t.Errorf("entry step source_formula = %q, want %q", entry.Fields["source_formula"], formulaID)
	}
	if entry.Fields["step_role"] != "monorepo-fix" {
		t.Errorf("entry step step_role = %q, want monorepo-fix", entry.Fields["step_role"])
	}
	if entry.Fields["molecule_id"] == "" {
		t.Errorf("entry step missing molecule_id")
	}
	// The DEFERRED opt-in must have landed AFTER the cross-pour edge (the pour
	// withholds suggest_new_agent from an entry step until its blocker exists).
	if !stepDispatchFieldTrue(entry.Fields["suggest_new_agent"]) {
		t.Fatalf("entry step must carry suggest_new_agent=true after the cross-pour edge; fields=%v", entry.Fields)
	}
	// The cross-pour blocks edge must run from the entry step onto the emit.
	if !daemon.hasBlocksEdge(entry.ID, emitID) {
		t.Fatalf("want cross-pour blocks edge %s->%s; fwd=%v", entry.ID, emitID, daemon.fwdDeps[entry.ID])
	}

	// The entry step's only blocker (the emit) is ALREADY closed — its close is
	// what fired the pour — so the step is dispatch-ready the instant it is
	// poured. This is the FSM-472 shape precisely: the close that fires the pour
	// is the same close that clears the blocker, and because the emit is already
	// closed when the edge lands, an OnStepClosed event will never re-fire for
	// it. Only the time-driven backfill sweep can drive this step — which is
	// exactly why the seam depends on it, and why the isolated close-event
	// dispatch tests do not cover this path.

	// --- DISPATCH HALF: run the REAL backfill sweep against the SAME daemon. ---
	// Note the pour already spawned the molecule DRIVER (spawned_by=trigger:<id>);
	// the seam's dispatch half is the SEPARATE step-role spawn the dispatcher
	// makes (spawned_by=formula-step-dispatcher), which is what we assert on.
	dispatcher := NewFormulaStepDispatcher(daemon, nil, quietLogger())
	dispatcher.BackfillReadySteps(context.Background())

	// Exactly one dispatcher spawn: the opted-in entry step. The dependent step
	// (land-proof) did NOT opt in, so the dispatcher must not spawn for it.
	idx := daemon.dispatcherSpawnIdx()
	if len(idx) != 1 {
		t.Fatalf("want exactly one dispatcher spawn (the opted-in entry step), got %d (all roles=%v, all spawned_by=%v)",
			len(idx), daemon.spawnedRoles, seamSpawnedBy(daemon))
	}
	i := idx[0]
	if daemon.spawnedRoles[i] != "monorepo-fix" {
		t.Errorf("dispatched role = %q, want monorepo-fix", daemon.spawnedRoles[i])
	}
	// The dispatched role must be the one the pour resolved into the step_role
	// FIELD — the generalized dispatch target this epic added (formula_instantiate.go
	// stamps step_role=effectiveRole()), not merely a role: label the dispatcher
	// happens to fall back to. Tying the observed spawn to the field asserts the
	// field-based path is what fired. (The field-vs-label PRECEDENCE is isolated
	// separately by TestStepDispatch_StepRoleFieldWinsOverLabel.)
	if daemon.spawnedRoles[i] != entry.Fields["step_role"] {
		t.Errorf("dispatched role = %q, want the poured step_role field %q",
			daemon.spawnedRoles[i], entry.Fields["step_role"])
	}
	if daemon.spawnedTaskIDs[i] != entry.ID {
		t.Errorf("dispatched task = %q, want the entry step %q", daemon.spawnedTaskIDs[i], entry.ID)
	}
	if got := daemon.spawnedFields[i]["molecule_id"]; got != entry.Fields["molecule_id"] {
		t.Errorf("spawn molecule_id = %q, want the poured molecule %q", got, entry.Fields["molecule_id"])
	}
	// The durable dispatch stamp must be on the step bead — the field the
	// metrics/idempotency guard reads (dispatched_by=formula-step-dispatcher).
	if got := entry.Fields["dispatched_by"]; got != "formula-step-dispatcher" {
		t.Errorf("entry step dispatched_by = %q, want formula-step-dispatcher", got)
	}
	if strings.TrimSpace(entry.Fields["dispatched_agent"]) == "" {
		t.Errorf("entry step missing durable dispatched_agent stamp")
	}

	// A second sweep must be idempotent — the durable stamp is the guard.
	dispatcher.BackfillReadySteps(context.Background())
	if got := len(daemon.dispatcherSpawnIdx()); got != 1 {
		t.Fatalf("second sweep must not re-dispatch (durable stamp is the guard), got %d dispatcher spawns", got)
	}
}

// seamSpawnedBy projects the spawned_by field of every recorded spawn, for
// diagnostic output.
func seamSpawnedBy(d *seamDaemon) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, 0, len(d.spawnedFields))
	for _, f := range d.spawnedFields {
		out = append(out, f["spawned_by"])
	}
	return out
}

// TestE2E_Seam_NoDispatchWhileUpstreamOpen proves the gate the seam depends on:
// if the poured entry step's cross-pour blocker is NOT yet closed, the backfill
// sweep must NOT dispatch — the fix-proof agent only appears once the repro
// emit it proves has actually cleared. This is the negative half of the seam
// contract (constraint 5, ka-0000007823): an opted-in entry step whose upstream
// is still open must stay quiet.
func TestE2E_Seam_NoDispatchWhileUpstreamOpen(t *testing.T) {
	const (
		formulaID = "gd-seam-fixproof-formula-2"
		triggerID = "gd-seam-pour-trigger-2"
		emitID    = "gd-seam-repro-emit-2"
		project   = "gardenio"
	)

	daemon := newSeamDaemon(seamFixProofFormula(formulaID))
	engine := NewTriggerEngine(TriggerEngineConfig{Daemon: daemon, Logger: quietLogger()})

	// Fire the pour on a CREATED event (upstream still OPEN), so the cross-pour
	// edge lands onto a bead that has not cleared. The seam must pour the
	// molecule but the dispatcher must hold the entry step.
	src := NewBeadStateTriggerSource(quietLogger())
	engine.RegisterSource(src)
	src.mu.Lock()
	src.pending = []beadStateEvent{{
		Topic: "beads.bead.created",
		Bead: &BeadEvent{
			ID:     emitID,
			Type:   "task",
			Title:  "repro emit step (still open)",
			Status: "open",
			Labels: []string{"project:" + project},
			Fields: map[string]string{"repro_ticket": "FSM-999"},
		},
	}}
	src.mu.Unlock()

	// Seed the still-open upstream so stillBlocked can hydrate it.
	daemon.mu.Lock()
	daemon.beads[emitID] = &beadsapi.BeadDetail{
		ID: emitID, Type: "task", Status: "open",
		Labels: []string{"project:" + project},
		Fields: map[string]string{"repro_ticket": "FSM-999"},
	}
	daemon.beads[triggerID] = triggerBead(triggerID, "Pour fix-proof on create", "trigger:bead", map[string]string{
		"action":                   "apply_formula",
		"event_types":              "created",
		"conditions":               `[{"field":"type","op":"==","value":"task"}]`,
		"formula_id":               formulaID,
		"formula_vars":             `{"TICKET":"{{repro_ticket}}"}`,
		"cross_pour_upstream_step": "{{bead_id}}",
		"project":                  project,
	})
	daemon.mu.Unlock()

	engine.EvaluateSource(context.Background(), "bead")

	// The entry step was poured, opted in, and chained — but its blocker is open.
	entry := daemon.findStep(t, func(b *beadsapi.BeadDetail) bool {
		return b.Fields["source_step_id"] == "extract-inputs-and-fix"
	})
	if !stepDispatchFieldTrue(entry.Fields["suggest_new_agent"]) {
		t.Fatalf("entry step should be opted in after the edge; fields=%v", entry.Fields)
	}
	if !daemon.hasBlocksEdge(entry.ID, emitID) {
		t.Fatalf("want cross-pour blocks edge %s->%s", entry.ID, emitID)
	}

	dispatcher := NewFormulaStepDispatcher(daemon, nil, quietLogger())
	dispatcher.BackfillReadySteps(context.Background())

	// The pour spawned a molecule driver, but the DISPATCHER must stay silent
	// while the entry step's cross-pour blocker is still open.
	if got := len(daemon.dispatcherSpawnIdx()); got != 0 {
		t.Fatalf("no dispatcher spawn while the upstream emit is still open, got %d (spawned_by=%v)",
			got, seamSpawnedBy(daemon))
	}
	if strings.TrimSpace(entry.Fields["dispatched_by"]) != "" {
		t.Errorf("entry step must not be stamped dispatched while blocked; got %q", entry.Fields["dispatched_by"])
	}

	// Now close the upstream emit and re-sweep: the same step must dispatch,
	// proving the gate opens exactly when the blocker clears.
	daemon.mu.Lock()
	daemon.beads[emitID].Status = "closed"
	daemon.mu.Unlock()

	dispatcher.BackfillReadySteps(context.Background())
	idx := daemon.dispatcherSpawnIdx()
	if len(idx) != 1 {
		t.Fatalf("once the upstream emit closes the backfill must dispatch the entry step, got %d dispatcher spawns", len(idx))
	}
	if daemon.spawnedRoles[idx[0]] != "monorepo-fix" {
		t.Errorf("dispatched role = %q, want monorepo-fix", daemon.spawnedRoles[idx[0]])
	}
}

// TestE2E_Seam_DispatchesOnStepRoleFieldNotLabel isolates the GENERALIZED
// dispatch path this epic added: the dispatcher must resolve the target role
// from the step_role FIELD, not just a role: label. A real pour always stamps
// both consistently (step_role=effectiveRole() AND role:<r> labels), so a pour-
// derived step cannot tell the two apart — the label fallback would mask a
// broken step_role read. This test hands the dispatcher a poured-SHAPE step
// carrying step_role but NO role: label, so the ONLY way it can dispatch to the
// right role is by reading the field. If stepDispatchRole stopped reading
// step_role, the step would resolve to no role and decline — this reds.
func TestE2E_Seam_DispatchesOnStepRoleFieldNotLabel(t *testing.T) {
	const (
		emitID = "gd-seam-emit-fieldrole"
		stepID = "gd-seam-step-fieldrole"
	)

	daemon := newSeamDaemon()

	// A closed upstream emit (the cleared cross-pour blocker).
	daemon.beads[emitID] = &beadsapi.BeadDetail{ID: emitID, Type: "task", Status: "closed"}

	// A poured-shape fix-proof entry step: opted in, carrying the dispatch
	// metadata the pour writes — but with step_role as the ONLY role source (no
	// role: label). Project comes from step_project, mirroring a real pour.
	daemon.beads[stepID] = &beadsapi.BeadDetail{
		ID:     stepID,
		Type:   "task",
		Status: "open",
		Title:  "Extract inputs and prove fix",
		Labels: []string{"project:monorepo"}, // deliberately NO role: label
		Fields: map[string]string{
			"source_formula":    "gd-fixproof",
			"source_step_id":    "extract-inputs-and-fix",
			"molecule_id":       "gd-mol-fieldrole",
			"step_role":         "monorepo-fix",
			"step_project":      "monorepo",
			"suggest_new_agent": "true",
		},
	}
	daemon.AddDependency(context.Background(), stepID, emitID, "blocks", "test")

	dispatcher := NewFormulaStepDispatcher(daemon, nil, quietLogger())
	dispatcher.BackfillReadySteps(context.Background())

	idx := daemon.dispatcherSpawnIdx()
	if len(idx) != 1 {
		t.Fatalf("want one dispatcher spawn resolved from the step_role field, got %d", len(idx))
	}
	if got := daemon.spawnedRoles[idx[0]]; got != "monorepo-fix" {
		t.Errorf("dispatched role = %q, want monorepo-fix resolved from the step_role field (no role: label present)", got)
	}
}

// findStep returns the single materialized step bead matching pred, failing the
// test if there is not exactly one.
func (d *seamDaemon) findStep(t *testing.T, pred func(*beadsapi.BeadDetail) bool) *beadsapi.BeadDetail {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	var found []*beadsapi.BeadDetail
	for _, b := range d.beads {
		if b.Fields != nil && pred(b) {
			found = append(found, b)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly one matching step bead, got %d", len(found))
	}
	return found[0]
}

// dispatcherSpawnIdx returns the indices of spawns made by the step-role
// dispatcher (spawned_by=formula-step-dispatcher), as distinct from the molecule
// DRIVER the pour spawns (spawned_by=trigger:<id>). Both are legitimate outputs
// of a real fire; the seam's dispatch half is specifically the former.
func (d *seamDaemon) dispatcherSpawnIdx() []int {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []int
	for i, f := range d.spawnedFields {
		if f["spawned_by"] == "formula-step-dispatcher" {
			out = append(out, i)
		}
	}
	return out
}

// hasBlocksEdge reports whether a `blocks` edge (beadID depends on dependsOnID)
// was recorded.
func (d *seamDaemon) hasBlocksEdge(beadID, dependsOnID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, dep := range d.fwdDeps[beadID] {
		if dep.Type == "blocks" && dep.DependsOnID == dependsOnID {
			return true
		}
	}
	return false
}
