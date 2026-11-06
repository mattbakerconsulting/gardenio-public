package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// crossPourMockDaemon is a recording formulaInstantiator that captures the
// ORDER of writes, so the cross-pour tests can prove the invariant that makes
// the race mitigation (constraint 5, ka-0000007823) work: the cross-pour blocks
// edge lands BEFORE the deferred suggest_new_agent stamp. A plain
// map-of-final-state mock cannot see ordering; the whole point of deferring the
// opt-in is temporal, so the test must observe time.
//
// Bead IDs are derived from each step's source_step_id field so assertions can
// name a step by its formula ID rather than an opaque generated ID.
type crossPourMockDaemon struct {
	mu     sync.Mutex
	beads  map[string]*beadsapi.BeadDetail
	nextID int

	// events is the append-only log of mutating calls, in call order. Each entry
	// is "<verb> <beadID> <detail>", e.g. "dep step-1->gd-upstream blocks" or
	// "typed step-1 suggest_new_agent=true".
	events []string

	// addDepErr / typedErr, when set, make the corresponding call fail so the
	// fail-loud paths can be exercised.
	addDepErr error
	typedErr  error
}

func newCrossPourMockDaemon() *crossPourMockDaemon {
	return &crossPourMockDaemon{beads: make(map[string]*beadsapi.BeadDetail)}
}

func (m *crossPourMockDaemon) CreateBead(_ context.Context, req beadsapi.CreateBeadRequest) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var fields map[string]string
	if len(req.Fields) > 0 {
		var raw map[string]any
		if err := json.Unmarshal(req.Fields, &raw); err == nil {
			fields = make(map[string]string, len(raw))
			for k, v := range raw {
				fields[k] = fmt.Sprintf("%v", v)
			}
		}
	}

	// Prefer a stable, readable ID: molecule -> "mol", step -> its source_step_id.
	id := fields["source_step_id"]
	if req.Type == "molecule" {
		id = "mol"
	}
	if id == "" || m.beads[id] != nil {
		m.nextID++
		id = fmt.Sprintf("bd-%d", m.nextID)
	}

	m.beads[id] = &beadsapi.BeadDetail{
		ID:     id,
		Title:  req.Title,
		Type:   req.Type,
		Status: "open",
		Labels: req.Labels,
		Fields: fields,
	}
	m.events = append(m.events, "create "+id+" "+req.Type)
	return id, nil
}

func (m *crossPourMockDaemon) AddDependency(_ context.Context, beadID, dependsOnID, depType, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, fmt.Sprintf("dep %s->%s %s", beadID, dependsOnID, depType))
	return m.addDepErr
}

func (m *crossPourMockDaemon) UpdateBeadFields(_ context.Context, beadID string, fields map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, fmt.Sprintf("fields %s %v", beadID, fields))
	if b := m.beads[beadID]; b != nil {
		if b.Fields == nil {
			b.Fields = map[string]string{}
		}
		maps.Copy(b.Fields, fields)
	}
	return nil
}

func (m *crossPourMockDaemon) UpdateBeadFieldsTyped(_ context.Context, beadID string, fields map[string]any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.typedErr != nil {
		return m.typedErr
	}
	for k, v := range fields {
		m.events = append(m.events, fmt.Sprintf("typed %s %s=%v", beadID, k, v))
	}
	if b := m.beads[beadID]; b != nil {
		if b.Fields == nil {
			b.Fields = map[string]string{}
		}
		for k, v := range fields {
			b.Fields[k] = fmt.Sprintf("%v", v)
		}
	}
	return nil
}

// eventIndex returns the position of the first logged event containing sub, or
// -1 if none. Used to assert relative ordering of two writes.
func (m *crossPourMockDaemon) eventIndex(sub string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, e := range m.events {
		if strings.Contains(e, sub) {
			return i
		}
	}
	return -1
}

// The canonical cross-pour shape: a downstream molecule whose step 1 (an opted-in
// entry step) must chain onto an upstream molecule's emit step. This is the
// FSM §7→§8 fix-proof handoff (ka-0000001755).
func crossPourSteps() []formulaStep {
	return []formulaStep{
		{ID: "capture-before", Title: "Capture before-state", Roles: []string{"monorepo-fix"}, SuggestNewAgent: true},
		{ID: "prove-fix", Title: "Prove the fix", DependsOn: []string{"capture-before"}, Roles: []string{"monorepo-fix"}},
	}
}

// TestInstantiateFormula_CrossPourEdgeWritten is the core acceptance: with an
// upstream anchor, the poured molecule's entry step gets a `blocks` edge onto
// the upstream emit step, oriented (entry-step blocked, upstream blocker).
func TestInstantiateFormula_CrossPourEdgeWritten(t *testing.T) {
	daemon := newCrossPourMockDaemon()
	formula := &beadsapi.BeadDetail{ID: "gd-fix-proof", Title: "Fix-proof"}

	_, _, err := instantiateFormula(
		context.Background(), daemon, nil, formula, crossPourSteps(), nil, false,
		"monorepo", "trigger:t1", "gd-upstream-emit")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The entry step ("capture-before", no depends_on) must carry a blocks edge
	// onto the upstream emit step. Its ID is its source_step_id in the mock.
	want := "dep capture-before->gd-upstream-emit blocks"
	if daemon.eventIndex(want) < 0 {
		t.Fatalf("want a cross-pour edge %q; got events:\n%s", want, strings.Join(daemon.events, "\n"))
	}

	// The NON-entry step ("prove-fix", depends_on capture-before) must NOT chain
	// to the upstream — it blocks on its intra-molecule predecessor only.
	if daemon.eventIndex("dep prove-fix->gd-upstream-emit") >= 0 {
		t.Errorf("non-entry step must not get a cross-pour edge; got events:\n%s", strings.Join(daemon.events, "\n"))
	}
}

// TestInstantiateFormula_CrossPourRaceMitigation is the constraint-5 guard: an
// opted-in ENTRY step must NOT carry suggest_new_agent at create time, and the
// stamp must land only AFTER the cross-pour blocking edge exists. Otherwise the
// backfill sweep (≤2m) can dispatch step 1 in the gap before its blocker, which
// is the exact window that would fire the seam early and defeat the gate.
func TestInstantiateFormula_CrossPourRaceMitigation(t *testing.T) {
	daemon := newCrossPourMockDaemon()
	formula := &beadsapi.BeadDetail{ID: "gd-fix-proof", Title: "Fix-proof"}

	_, _, err := instantiateFormula(
		context.Background(), daemon, nil, formula, crossPourSteps(), nil, false,
		"monorepo", "trigger:t1", "gd-upstream-emit")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// 1. The entry step must NOT have been created carrying suggest_new_agent.
	step := daemon.beads["capture-before"]
	if step == nil {
		t.Fatal("entry step bead was not created")
	}
	// After the full pour it SHOULD carry the opt-in (stamped post-edge)…
	if step.Fields["suggest_new_agent"] != "true" {
		t.Errorf("entry step should carry suggest_new_agent=true after pour, got %q", step.Fields["suggest_new_agent"])
	}

	// 2. …but the opt-in stamp (typed write) must come AFTER the cross-pour edge.
	edgeAt := daemon.eventIndex("dep capture-before->gd-upstream-emit blocks")
	stampAt := daemon.eventIndex("typed capture-before suggest_new_agent=true")
	if edgeAt < 0 {
		t.Fatalf("no cross-pour edge logged; events:\n%s", strings.Join(daemon.events, "\n"))
	}
	if stampAt < 0 {
		t.Fatalf("no deferred opt-in stamp logged; events:\n%s", strings.Join(daemon.events, "\n"))
	}
	if stampAt < edgeAt {
		t.Errorf("suggest_new_agent stamp (idx %d) must come AFTER the cross-pour edge (idx %d) — the backfill race is open; events:\n%s",
			stampAt, edgeAt, strings.Join(daemon.events, "\n"))
	}

	// 3. The create for the entry step must NOT itself have set suggest_new_agent
	// (the whole point of deferral). The create event precedes any typed stamp;
	// assert the created bead's fields at create time by checking the typed stamp
	// is what supplied the opt-in, i.e. there was a create then a later typed set.
	createAt := daemon.eventIndex("create capture-before task")
	if createAt < 0 {
		t.Fatalf("no create event for entry step; events:\n%s", strings.Join(daemon.events, "\n"))
	}
	if stampAt <= createAt {
		t.Errorf("opt-in must be stamped after create, not at create; create idx %d, stamp idx %d", createAt, stampAt)
	}
}

// TestInstantiateFormula_NoAnchorNoCrossPour is the regression guard: without an
// upstream anchor (the Slack pour and every non-chained trigger), behavior is
// unchanged — no cross-pour edge, and an opted-in step carries suggest_new_agent
// at CREATE time exactly as before (no deferral, no extra typed write).
func TestInstantiateFormula_NoAnchorNoCrossPour(t *testing.T) {
	daemon := newCrossPourMockDaemon()
	formula := &beadsapi.BeadDetail{ID: "gd-fix-proof", Title: "Fix-proof"}

	_, _, err := instantiateFormula(
		context.Background(), daemon, nil, formula, crossPourSteps(), nil, false,
		"monorepo", "trigger:t1", "") // empty anchor
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// No edge to any upstream, and no deferred typed stamp at all.
	for _, e := range daemon.events {
		if strings.Contains(e, "gd-upstream") {
			t.Errorf("no anchor, so no cross-pour edge expected; got %q", e)
		}
		if strings.HasPrefix(e, "typed ") {
			t.Errorf("no anchor, so no deferred opt-in stamp expected; got %q", e)
		}
	}

	// The opted-in entry step must carry suggest_new_agent from create (the
	// unchanged, non-deferred path). The mock stringifies create fields, so the
	// bead reflects create-time state since nothing stamped it afterward.
	step := daemon.beads["capture-before"]
	if step == nil || step.Fields["suggest_new_agent"] != "true" {
		t.Errorf("without an anchor, opted-in step should carry suggest_new_agent at create; got %v",
			mustFields(step))
	}
}

// TestInstantiateFormula_CrossPourEdgeFailIsLoud proves a failed cross-pour edge
// does NOT abort the pour (a childless molecule is worse than one needing a
// nudge) and is logged at error level so a human can see the seam won't fire.
func TestInstantiateFormula_CrossPourEdgeFailIsLoud(t *testing.T) {
	daemon := newCrossPourMockDaemon()
	daemon.addDepErr = fmt.Errorf("boom")

	var buf bytes.Buffer
	logger := captureLogger(&buf)
	formula := &beadsapi.BeadDetail{ID: "gd-fix-proof", Title: "Fix-proof"}

	_, n, err := instantiateFormula(
		context.Background(), daemon, logger, formula, crossPourSteps(), nil, false,
		"monorepo", "trigger:t1", "gd-upstream-emit")
	// The pour itself must succeed — both step beads were created.
	if err != nil {
		t.Fatalf("cross-pour edge failure must not abort the pour, got err: %v", err)
	}
	if n != 2 {
		t.Errorf("expected 2 steps created despite edge failure, got %d", n)
	}
	if !strings.Contains(buf.String(), "failed to add cross-pour blocks edge") {
		t.Errorf("want a loud error about the failed cross-pour edge; got logs:\n%s", buf.String())
	}
	// And because the edge failed, the opt-in must NOT be stamped — an opted-in
	// entry step with no blocker is precisely the early-dispatch footgun.
	if daemon.eventIndex("typed capture-before suggest_new_agent") >= 0 {
		t.Errorf("opt-in must not be stamped when the cross-pour edge failed; events:\n%s",
			strings.Join(daemon.events, "\n"))
	}
}

func mustFields(b *beadsapi.BeadDetail) map[string]string {
	if b == nil {
		return nil
	}
	return b.Fields
}

// TestE2E_ApplyFormula_CrossPourEdge drives the FULL SSE-event →
// BeadStateTriggerSource → engine → actionApplyFormula path with a trigger that
// declares cross_pour_upstream_step="{{bead_id}}". It proves the wiring the unit
// tests can't: the trigger field is parsed, template-expanded against the firing
// bead, and threaded into instantiateFormula so the poured molecule's step 1
// gets a `blocks` edge onto the upstream emit step (the firing bead), plus the
// deferred opt-in stamp. This is the cross-molecule half of the FSM §7→§8 seam
// (decision ka-0000007596, exit criterion ka-0000007685).
func TestE2E_ApplyFormula_CrossPourEdge(t *testing.T) {
	const (
		triggerID = "gd-trig-crosspour-1"
		sourceID  = "gd-repro-emit-1" // the upstream emit step that fired the trigger
		project   = "gardenio"
	)

	// A fix-proof formula whose step 1 opts into its own agent — the canonical
	// downstream molecule of the §7→§8 handoff.
	formula := &beadsapi.BeadDetail{
		ID:    "gd-formula-fixproof",
		Type:  "formula",
		Title: "Fix-proof for {{BEAD}}",
		Fields: map[string]string{
			"vars":  `[{"name":"BEAD"}]`,
			"steps": `[{"id":"capture-before","title":"Capture before-state for {{BEAD}}","roles":["monorepo-fix"],"suggest_new_agent":true},{"id":"prove-fix","title":"Prove the fix","depends_on":["capture-before"],"roles":["monorepo-fix"]}]`,
		},
	}

	daemon := &mockTriggerDaemon{beads: []*beadsapi.BeadDetail{formula}}
	engine := NewTriggerEngine(TriggerEngineConfig{Daemon: daemon, Logger: slog.Default()})

	src := NewBeadStateTriggerSource(slog.Default())
	engine.RegisterSource(src)
	src.mu.Lock()
	src.pending = []beadStateEvent{{
		Topic: "beads.bead.closed",
		Bead: &BeadEvent{
			ID:     sourceID,
			Type:   "task",
			Title:  "repro emit step",
			Status: "closed",
			Labels: []string{"project:" + project},
		},
	}}
	src.mu.Unlock()

	fields := map[string]string{
		"action":                   "apply_formula",
		"event_types":              "closed",
		"conditions":               `[{"field":"type","op":"==","value":"task"}]`,
		"formula_id":               formula.ID,
		"formula_vars":             `{"BEAD":"{{bead_id}}"}`,
		"cross_pour_upstream_step": "{{bead_id}}",
		"project":                  project,
	}
	daemon.mu.Lock()
	daemon.beads = append(daemon.beads, triggerBeads(
		triggerBead(triggerID, "Pour fix-proof on repro emit close", "trigger:bead", fields),
	)...)
	daemon.mu.Unlock()

	engine.EvaluateSource(context.Background(), "bead")

	// The molecule is created first (gd-trigger-bead-1), then step 1
	// (gd-trigger-bead-2), then step 2 (gd-trigger-bead-3). The cross-pour edge
	// must run from step 1 onto the upstream emit step (the firing bead).
	daemon.mu.Lock()
	defer daemon.mu.Unlock()

	const step1BeadID = "gd-trigger-bead-2"
	foundEdge := false
	for _, d := range daemon.addedDeps {
		if d.DepType == "blocks" && d.BeadID == step1BeadID && d.DependsOnID == sourceID {
			foundEdge = true
		}
		// The non-entry step (step 2) must NOT chain to the upstream.
		if d.DepType == "blocks" && d.DependsOnID == sourceID && d.BeadID != step1BeadID {
			t.Errorf("only the entry step may chain to the upstream emit; got edge from %q", d.BeadID)
		}
	}
	if !foundEdge {
		t.Fatalf("want a cross-pour blocks edge %s->%s; got deps:\n%+v", step1BeadID, sourceID, daemon.addedDeps)
	}

	// The deferred opt-in must have been stamped (typed) onto step 1.
	stampedOptIn := false
	for i, id := range daemon.updatedIDs {
		if id == step1BeadID && daemon.updatedFields[i]["suggest_new_agent"] == "true" {
			stampedOptIn = true
		}
	}
	if !stampedOptIn {
		t.Errorf("want suggest_new_agent stamped on step 1 (%s) after the edge; got updates ids=%v fields=%v",
			step1BeadID, daemon.updatedIDs, daemon.updatedFields)
	}
}

// TestE2E_ApplyFormula_CrossPourUnresolvedAnchorRejected proves the guard: a
// cross_pour_upstream_step whose placeholder the firing source never published
// must fail the pour LOUDLY rather than writing an edge to a bead ID that is a
// literal "{{...}}" string (which would silently never dispatch).
func TestE2E_ApplyFormula_CrossPourUnresolvedAnchorRejected(t *testing.T) {
	const (
		triggerID = "gd-trig-crosspour-bad"
		sourceID  = "gd-repro-emit-2"
		project   = "gardenio"
	)
	formula := testFormulaBead("gd-formula-plain2", "BEAD")

	daemon := &mockTriggerDaemon{beads: []*beadsapi.BeadDetail{formula}}
	engine := NewTriggerEngine(TriggerEngineConfig{Daemon: daemon, Logger: slog.Default()})
	src := NewBeadStateTriggerSource(slog.Default())
	engine.RegisterSource(src)
	src.mu.Lock()
	src.pending = []beadStateEvent{{
		Topic: "beads.bead.created",
		Bead:  &BeadEvent{ID: sourceID, Type: "task", Title: "x", Status: "open", Labels: []string{"project:" + project}},
	}}
	src.mu.Unlock()

	fields := map[string]string{
		"action":       "apply_formula",
		"event_types":  "created",
		"conditions":   `[{"field":"type","op":"==","value":"task"}]`,
		"formula_id":   formula.ID,
		"formula_vars": `{"BEAD":"{{bead_id}}"}`,
		// {{no_such_var}} is never published by the bead source → stays literal.
		"cross_pour_upstream_step": "{{no_such_var}}",
		"project":                  project,
	}
	daemon.mu.Lock()
	daemon.beads = append(daemon.beads, triggerBeads(
		triggerBead(triggerID, "bad anchor", "trigger:bead", fields),
	)...)
	daemon.mu.Unlock()

	engine.EvaluateSource(context.Background(), "bead")

	// The pour must have been refused before creating any molecule/step beads —
	// no partial pour, no edge, no spawn.
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.createdBeads) != 0 {
		t.Errorf("unresolved cross-pour anchor must abort the pour before creating beads; got %d created", len(daemon.createdBeads))
	}
	if len(daemon.spawnedNames) != 0 {
		t.Errorf("unresolved cross-pour anchor must not spawn a driver; got %d spawns", len(daemon.spawnedNames))
	}
}
