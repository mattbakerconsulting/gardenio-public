package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// drainEngine wires a TriggerEngine with the closed-enabled-drain sweep enabled,
// a recording notifier, and a MUTABLE clock (via the returned pointer) so the
// once-per-hour throttle can be exercised deterministically.
func drainEngine(t *testing.T, beads ...*beadsapi.BeadDetail) (*TriggerEngine, *mockTriggerDaemon, *recordingNotifier, *time.Time) {
	t.Helper()
	daemon := &mockTriggerDaemon{beads: beads}
	rec := &recordingNotifier{}
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	clk := &now
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon:                    daemon,
		Notifier:                  rec.notify,
		Logger:                    slog.Default(),
		ClosedEnabledDrainEnabled: true,
		ClosedEnabledDrainNotify:  true,
		Clock:                     func() time.Time { return *clk },
	})
	return engine, daemon, rec, clk
}

// closedEnabledTrigger builds a closed+enabled trigger fixture with the given
// labels and extra fields.
func closedEnabledTrigger(id, title string, labels []string, extra map[string]string) *beadsapi.BeadDetail {
	fields := map[string]string{
		"enabled": "true",
		"project": "gardenio",
	}
	for k, v := range extra {
		fields[k] = v
	}
	return &beadsapi.BeadDetail{
		ID:     id,
		Type:   "trigger",
		Status: "closed",
		Title:  title,
		Labels: labels,
		Fields: fields,
	}
}

// drainEpic builds an epic bead of the given status.
func drainEpic(id, status string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID:     id,
		Type:   "epic",
		Status: status,
		Fields: map[string]string{},
	}
}

// drainDecision builds a grouped-decision fixture as this sweep would have
// filed it: type=decision carrying the marker label and the candidate id list.
func drainDecision(id, status, chosen, responseText string, candidateIDs []string, extra map[string]string) *beadsapi.BeadDetail {
	idsJSON, _ := json.Marshal(candidateIDs)
	fields := map[string]string{
		drainCandidateIDsField: string(idsJSON),
	}
	if chosen != "" {
		fields["chosen"] = chosen
	}
	if responseText != "" {
		fields["response_text"] = responseText
	}
	for k, v := range extra {
		fields[k] = v
	}
	return &beadsapi.BeadDetail{
		ID:     id,
		Type:   "decision",
		Status: status,
		Title:  "grouped drain decision",
		Labels: []string{"project:gardenio", closedEnabledDrainMarkerLabel},
		Fields: fields,
	}
}

// drainFieldUpdatesFor returns every field-map written to beadID via
// UpdateBeadFields / UpdateBeadFieldsTyped, in call order.
func drainFieldUpdatesFor(m *mockTriggerDaemon, beadID string) []map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []map[string]string
	for i, id := range m.updatedIDs {
		if id == beadID {
			out = append(out, m.updatedFields[i])
		}
	}
	return out
}

// drainWasArchived reports whether beadID received an enabled=false write.
func drainWasArchived(m *mockTriggerDaemon, beadID string) bool {
	for _, f := range drainFieldUpdatesFor(m, beadID) {
		if f["enabled"] == "false" {
			return true
		}
	}
	return false
}

// drainDisarmWasTypedBool reports whether beadID received an enabled=false write
// whose value was a real JSON bool (not the string "false"). enabled is
// boolean-typed on the type:trigger schema, so a string 400s the whole PATCH
// (ka-0000003627); the mock records raw JSON-typed values from
// UpdateBeadFieldsTyped into updatedTyped.
func drainDisarmWasTypedBool(m *mockTriggerDaemon, beadID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, id := range m.updatedIDs {
		if id != beadID || i >= len(m.updatedTyped) {
			continue
		}
		if v, ok := m.updatedTyped[i]["enabled"]; ok {
			if b, isBool := v.(bool); isBool && !b {
				return true
			}
		}
	}
	return false
}

// drainCreatedDecision returns the first created type=decision bead carrying the
// marker label, or nil.
func drainCreatedDecision(m *mockTriggerDaemon) *beadsapi.CreateBeadRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.createdBeads {
		req := &m.createdBeads[i]
		if req.Type != "decision" {
			continue
		}
		for _, l := range req.Labels {
			if l == closedEnabledDrainMarkerLabel {
				return req
			}
		}
	}
	return nil
}

// ── Fix 1: autonomous archive of the CLOSED-epic cohort ────────────────────

func TestClosedEnabledDrain_AutoArchivesClosedEpicCohort(t *testing.T) {
	trig := closedEnabledTrigger("gd-trig-1", "retired gardener", []string{"trigger:schedule", "gardener-epic:gd-epic-closed"}, nil)
	epic := drainEpic("gd-epic-closed", "closed")
	engine, daemon, rec, _ := drainEngine(t, trig, epic)

	engine.SweepClosedEnabledDrain(context.Background())

	if !drainWasArchived(daemon, "gd-trig-1") {
		t.Fatalf("expected closed-epic trigger to be auto-archived (enabled=false)")
	}
	// Audit comment stamped on the trigger.
	commented := false
	for _, id := range daemon.commentedIDs {
		if id == "gd-trig-1" {
			commented = true
		}
	}
	if !commented {
		t.Errorf("expected an audit comment on the auto-archived trigger")
	}
	// NEVER spawns.
	if len(daemon.spawnedNames) != 0 {
		t.Errorf("drain sweep must not spawn agents, spawned %d", len(daemon.spawnedNames))
	}
	// A grouped decision must NOT be filed for a pool that had only a
	// gardener-epic-anchored candidate.
	if d := drainCreatedDecision(daemon); d != nil {
		t.Errorf("did not expect a grouped decision for the closed-epic-only pool, got %q", d.Title)
	}
	if rec.count() == 0 {
		t.Errorf("expected a notify summary for a run that archived")
	}
}

func TestClosedEnabledDrain_SkipsOpenEpicLane(t *testing.T) {
	// A closed+enabled trigger anchored to an OPEN epic is a possibly-uncovered
	// lane: it must be left ALONE (neither auto-archived nor batched).
	trig := closedEnabledTrigger("gd-trig-open", "uncovered lane", []string{"trigger:schedule", "gardener-epic:gd-epic-open"}, nil)
	epic := drainEpic("gd-epic-open", "open")
	engine, daemon, _, _ := drainEngine(t, trig, epic)

	engine.SweepClosedEnabledDrain(context.Background())

	if drainWasArchived(daemon, "gd-trig-open") {
		t.Fatalf("open-epic-anchored trigger must NOT be auto-archived")
	}
	if d := drainCreatedDecision(daemon); d != nil {
		t.Errorf("open-epic-anchored trigger must NOT be batched into a decision, got %q", d.Title)
	}
}

func TestClosedEnabledDrain_MissingEpicIsConservativelySkipped(t *testing.T) {
	// Anchor epic not present (GetBead fails) → treat as NOT closed → skip.
	trig := closedEnabledTrigger("gd-trig-missing", "orphan anchor", []string{"gardener-epic:gd-epic-gone"}, nil)
	engine, daemon, _, _ := drainEngine(t, trig)

	engine.SweepClosedEnabledDrain(context.Background())

	if drainWasArchived(daemon, "gd-trig-missing") {
		t.Fatalf("trigger with an unresolvable anchor epic must not be auto-archived")
	}
}

func TestClosedEnabledDrain_RespectsPerRunArchiveCap(t *testing.T) {
	var beads []*beadsapi.BeadDetail
	beads = append(beads, drainEpic("gd-epic-c", "closed"))
	total := closedEnabledDrainArchiveCap + 5
	for i := 0; i < total; i++ {
		beads = append(beads, closedEnabledTrigger(
			"gd-cap-"+itoa(i), "retired", []string{"gardener-epic:gd-epic-c"},
			map[string]string{"last_checked": time.Now().Add(-time.Duration(i) * time.Hour).Format(time.RFC3339)}))
	}
	engine, daemon, _, _ := drainEngine(t, beads...)

	engine.SweepClosedEnabledDrain(context.Background())

	archived := 0
	for i := 0; i < total; i++ {
		if drainWasArchived(daemon, "gd-cap-"+itoa(i)) {
			archived++
		}
	}
	if archived != closedEnabledDrainArchiveCap {
		t.Fatalf("expected exactly %d archived (per-run cap), got %d", closedEnabledDrainArchiveCap, archived)
	}
}

func TestClosedEnabledDrain_SkipsManualAndFixture(t *testing.T) {
	manual := closedEnabledTrigger("gd-manual", "manual", []string{"gardener-epic:gd-epic-c"}, map[string]string{"trigger_mode": "manual"})
	fixture := closedEnabledTrigger("gd-fixture", "e2e", []string{"gardener-epic:gd-epic-c", e2eFixtureLabelPrefix + "smoke"}, nil)
	epic := drainEpic("gd-epic-c", "closed")
	engine, daemon, _, _ := drainEngine(t, manual, fixture, epic)

	engine.SweepClosedEnabledDrain(context.Background())

	if drainWasArchived(daemon, "gd-manual") {
		t.Errorf("manual trigger must not be auto-archived")
	}
	if drainWasArchived(daemon, "gd-fixture") {
		t.Errorf("e2e-fixture trigger must not be auto-archived")
	}
}

// ── Fix 3: autonomous disarm of the ENGINE-RETIRED cohort (ka-0000001423) ──

// TestClosedEnabledDrain_DisablesTerminatedKindCohort proves the backfill half
// of the ka-0000001423 fix: a closed+enabled trigger carrying a
// gardener_terminated_kind marker (the engine's own record that it retired the
// gardener on an author-declared stop) is auto-disabled regardless of anchor-epic
// status — the phantom class the closed-epic cohort (fix 1) otherwise strands.
func TestClosedEnabledDrain_DisablesTerminatedKindCohort(t *testing.T) {
	// Anchored to an OPEN epic — fix 1 would DELIBERATELY skip this. Fix 3 must
	// still disable it because the engine's terminate marker is unambiguous.
	trig := closedEnabledTrigger("gd-retired", "engine-retired gardener",
		[]string{"trigger:schedule", "gardener-epic:gd-epic-open"},
		map[string]string{"gardener_terminated_slot": "loop_end", "gardener_terminated_kind": "bead_query"})
	epic := drainEpic("gd-epic-open", "open")
	engine, daemon, rec, _ := drainEngine(t, trig, epic)

	engine.SweepClosedEnabledDrain(context.Background())

	if !drainWasArchived(daemon, "gd-retired") {
		t.Fatalf("expected engine-retired trigger (gardener_terminated_kind set) to be disabled, even under an OPEN epic")
	}
	// The disarm write MUST be a real JSON bool, not the string "false": enabled
	// is boolean-typed and a string 400s the whole PATCH (ka-0000003627 / ka-0000007976).
	if !drainDisarmWasTypedBool(daemon, "gd-retired") {
		t.Errorf("enabled=false must be written as a JSON bool via UpdateBeadFieldsTyped, not a string")
	}
	commented := false
	for _, id := range daemon.commentedIDs {
		if id == "gd-retired" {
			commented = true
		}
	}
	if !commented {
		t.Errorf("expected an audit comment on the disabled engine-retired trigger")
	}
	// No grouped decision — the terminate marker is a deterministic signal, not
	// an ambiguous one needing operator judgement.
	if d := drainCreatedDecision(daemon); d != nil {
		t.Errorf("engine-retired cohort must not file a grouped decision, got %q", d.Title)
	}
	if rec.count() == 0 {
		t.Errorf("expected a notify summary for a run that disabled an engine-retired trigger")
	}
}

// TestClosedEnabledDrain_TerminatedKindWinsOverNoLabel proves the terminate
// marker is classified BEFORE the epic-label branch: an engine-retired trigger
// with NO gardener-epic label is disabled by fix 3, never routed into the fix-2
// grouped-decision cohort (which would waste an operator click on a lane the
// engine already declared retired). This is the worked-example shape:
// gd-ARlDFR1z3P (loop_end/bead_query, no epic label).
func TestClosedEnabledDrain_TerminatedKindWinsOverNoLabel(t *testing.T) {
	trig := closedEnabledTrigger("gd-qox-like", "retired, no epic label",
		[]string{"trigger:schedule"},
		map[string]string{"gardener_terminated_slot": "loop_end", "gardener_terminated_kind": "bead_query"})
	engine, daemon, _, _ := drainEngine(t, trig)

	engine.SweepClosedEnabledDrain(context.Background())

	if !drainWasArchived(daemon, "gd-qox-like") {
		t.Fatalf("expected unanchored engine-retired trigger to be disabled by fix 3")
	}
	if d := drainCreatedDecision(daemon); d != nil {
		t.Errorf("engine-retired trigger must be disabled, not batched into a grouped decision, got %q", d.Title)
	}
}

// TestClosedEnabledDrain_TerminatedKindRespectsCap proves fix 3 honors the same
// per-run archive cap as fix 1, so a large backlog drains over several runs
// rather than in one irreversible sweep.
func TestClosedEnabledDrain_TerminatedKindRespectsCap(t *testing.T) {
	var beads []*beadsapi.BeadDetail
	total := closedEnabledDrainArchiveCap + 5
	for i := 0; i < total; i++ {
		beads = append(beads, closedEnabledTrigger(
			"gd-rt-"+itoa(i), "engine-retired", []string{"trigger:schedule"},
			map[string]string{
				"gardener_terminated_kind": "no_progress_for",
				"last_checked":             time.Now().Add(-time.Duration(i) * time.Hour).Format(time.RFC3339),
			}))
	}
	engine, daemon, _, _ := drainEngine(t, beads...)

	engine.SweepClosedEnabledDrain(context.Background())

	archived := 0
	for i := 0; i < total; i++ {
		if drainWasArchived(daemon, "gd-rt-"+itoa(i)) {
			archived++
		}
	}
	if archived != closedEnabledDrainArchiveCap {
		t.Fatalf("expected exactly %d disabled (per-run cap), got %d", closedEnabledDrainArchiveCap, archived)
	}
}

// TestClosedEnabledDrain_TerminatedKindSkipsManualAndFixture proves the same
// manual/E2E-fixture guard the other cohorts honor also protects fix 3.
func TestClosedEnabledDrain_TerminatedKindSkipsManualAndFixture(t *testing.T) {
	manual := closedEnabledTrigger("gd-rt-manual", "manual retired", []string{"trigger:schedule"},
		map[string]string{"gardener_terminated_kind": "max_runs", "trigger_mode": "manual"})
	fixture := closedEnabledTrigger("gd-rt-fixture", "e2e retired",
		[]string{"trigger:schedule", e2eFixtureLabelPrefix + "smoke"},
		map[string]string{"gardener_terminated_kind": "wall_clock"})
	engine, daemon, _, _ := drainEngine(t, manual, fixture)

	engine.SweepClosedEnabledDrain(context.Background())

	if drainWasArchived(daemon, "gd-rt-manual") {
		t.Errorf("manual engine-retired trigger must not be auto-disabled")
	}
	if drainWasArchived(daemon, "gd-rt-fixture") {
		t.Errorf("e2e-fixture engine-retired trigger must not be auto-disabled")
	}
}

// ── Fix 2: grouped decision for the unanchored cohort ──────────────────────

func TestClosedEnabledDrain_FilesGroupedDecisionForNoLabelCohort(t *testing.T) {
	a := closedEnabledTrigger("gd-nolabel-a", "probe A", []string{"trigger:multi"}, nil)
	b := closedEnabledTrigger("gd-nolabel-b", "probe B", []string{"trigger:schedule"}, nil)
	engine, daemon, _, _ := drainEngine(t, a, b)

	engine.SweepClosedEnabledDrain(context.Background())

	dec := drainCreatedDecision(daemon)
	if dec == nil {
		t.Fatalf("expected a grouped archive decision for the no-label cohort")
	}
	var fields map[string]string
	if err := json.Unmarshal(dec.Fields, &fields); err != nil {
		t.Fatalf("decision fields not JSON: %v", err)
	}
	// Options carry archive-all + keep-all.
	if !strings.Contains(fields["options"], drainOptionArchiveAll) || !strings.Contains(fields["options"], drainOptionKeepAll) {
		t.Errorf("decision options missing archive-all/keep-all: %s", fields["options"])
	}
	// Candidate ids stashed for deterministic execution.
	var ids []string
	if err := json.Unmarshal([]byte(fields[drainCandidateIDsField]), &ids); err != nil {
		t.Fatalf("candidate ids not JSON: %v", err)
	}
	if len(ids) != 2 {
		t.Errorf("expected 2 candidate ids, got %d (%v)", len(ids), ids)
	}
	// Nothing auto-archived (no-label cohort waits for the operator).
	if drainWasArchived(daemon, "gd-nolabel-a") || drainWasArchived(daemon, "gd-nolabel-b") {
		t.Errorf("no-label triggers must not be archived before the operator resolves the batch")
	}
}

func TestClosedEnabledDrain_DedupsWhenOpenDecisionExists(t *testing.T) {
	a := closedEnabledTrigger("gd-nolabel-a", "probe A", nil, nil)
	openDec := drainDecision("gd-dec-open", "open", "", "", []string{"gd-old"}, nil)
	engine, daemon, _, _ := drainEngine(t, a, openDec)

	engine.SweepClosedEnabledDrain(context.Background())

	if d := drainCreatedDecision(daemon); d != nil {
		t.Fatalf("must NOT file a second grouped decision while one is open — the anti-flood guard")
	}
}

func TestClosedEnabledDrain_SkipsCandidatesInCooldown(t *testing.T) {
	recent := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	a := closedEnabledTrigger("gd-cooling", "recently kept", nil, map[string]string{drainReviewedAtField: recent})
	engine, daemon, _, _ := drainEngine(t, a)

	engine.SweepClosedEnabledDrain(context.Background())

	if d := drainCreatedDecision(daemon); d != nil {
		t.Fatalf("a candidate inside its keep-cooldown must not be re-proposed")
	}
}

// ── Fix 2 completion: execute resolved grouped decisions ───────────────────

func TestClosedEnabledDrain_ExecutesResolvedArchiveAll(t *testing.T) {
	c1 := closedEnabledTrigger("gd-c1", "cand 1", nil, nil)
	c2 := closedEnabledTrigger("gd-c2", "cand 2", nil, nil)
	dec := drainDecision("gd-dec-1", "closed", drainOptionArchiveAll, "", []string{"gd-c1", "gd-c2"}, nil)
	engine, daemon, _, _ := drainEngine(t, c1, c2, dec)

	engine.SweepClosedEnabledDrain(context.Background())

	if !drainWasArchived(daemon, "gd-c1") || !drainWasArchived(daemon, "gd-c2") {
		t.Fatalf("archive-all must archive every candidate; c1=%v c2=%v",
			drainWasArchived(daemon, "gd-c1"), drainWasArchived(daemon, "gd-c2"))
	}
	// Decision marked executed exactly once (idempotency marker).
	execMarked := false
	for _, f := range drainFieldUpdatesFor(daemon, "gd-dec-1") {
		if f[drainExecutedAtField] != "" {
			execMarked = true
		}
	}
	if !execMarked {
		t.Errorf("resolved decision must be stamped %s after execution", drainExecutedAtField)
	}
}

func TestClosedEnabledDrain_ExecutesResolvedArchiveAllViaLabel(t *testing.T) {
	// The web-UI resolve path can store the human "Archive all" label rather than
	// the option id; execution must still recognise it.
	c1 := closedEnabledTrigger("gd-l1", "cand", nil, nil)
	dec := drainDecision("gd-dec-label", "closed", "Archive all", "", []string{"gd-l1"}, nil)
	engine, daemon, _, _ := drainEngine(t, c1, dec)

	engine.SweepClosedEnabledDrain(context.Background())

	if !drainWasArchived(daemon, "gd-l1") {
		t.Fatalf("archive via the human 'Archive all' label must archive the candidate")
	}
}

func TestClosedEnabledDrain_ExecutesResolvedKeepAll(t *testing.T) {
	c1 := closedEnabledTrigger("gd-k1", "cand 1", nil, nil)
	dec := drainDecision("gd-dec-keep", "closed", drainOptionKeepAll, "", []string{"gd-k1"}, nil)
	engine, daemon, _, _ := drainEngine(t, c1, dec)

	engine.SweepClosedEnabledDrain(context.Background())

	if drainWasArchived(daemon, "gd-k1") {
		t.Fatalf("keep-all must NOT archive any candidate")
	}
	// Kept candidate gets a cooldown stamp so it is not re-proposed immediately.
	cooled := false
	for _, f := range drainFieldUpdatesFor(daemon, "gd-k1") {
		if f[drainReviewedAtField] != "" {
			cooled = true
		}
	}
	if !cooled {
		t.Errorf("kept candidate must be stamped %s (re-review cooldown)", drainReviewedAtField)
	}
}

func TestClosedEnabledDrain_OptOutListKeepsNamedIDs(t *testing.T) {
	c1 := closedEnabledTrigger("gd-o1", "archive me", nil, nil)
	c2 := closedEnabledTrigger("gd-o2", "keep me", nil, nil)
	// Free-text "Other" response: archive except gd-o2.
	dec := drainDecision("gd-dec-opt", "closed", "Other", "archive all except gd-o2", []string{"gd-o1", "gd-o2"}, nil)
	engine, daemon, _, _ := drainEngine(t, c1, c2, dec)

	engine.SweepClosedEnabledDrain(context.Background())

	if !drainWasArchived(daemon, "gd-o1") {
		t.Errorf("gd-o1 (not opted out) should be archived")
	}
	if drainWasArchived(daemon, "gd-o2") {
		t.Errorf("gd-o2 (named in opt-out list) must be KEPT")
	}
}

func TestClosedEnabledDrain_FreeTextWithoutArchiveIntentKeepsAll(t *testing.T) {
	c1 := closedEnabledTrigger("gd-f1", "cand", nil, nil)
	// "Other" response naming no archive intent → conservative keep-all.
	dec := drainDecision("gd-dec-free", "closed", "Other", "let me think about these later", []string{"gd-f1"}, nil)
	engine, daemon, _, _ := drainEngine(t, c1, dec)

	engine.SweepClosedEnabledDrain(context.Background())

	if drainWasArchived(daemon, "gd-f1") {
		t.Fatalf("a free-text response with no archive intent must archive nothing")
	}
}

func TestClosedEnabledDrain_ReValidationSkipsReopenedCandidate(t *testing.T) {
	// A candidate that was REOPENED (status=open) since the decision was filed
	// must not be clobbered by the archive.
	reopened := closedEnabledTrigger("gd-reopened", "came back", nil, nil)
	reopened.Status = "open"
	dec := drainDecision("gd-dec-rv", "closed", drainOptionArchiveAll, "", []string{"gd-reopened"}, nil)
	engine, daemon, _, _ := drainEngine(t, reopened, dec)

	engine.SweepClosedEnabledDrain(context.Background())

	if drainWasArchived(daemon, "gd-reopened") {
		t.Fatalf("a reopened candidate must be skipped by execution re-validation")
	}
}

func TestClosedEnabledDrain_DoesNotReexecuteMarkedDecision(t *testing.T) {
	c1 := closedEnabledTrigger("gd-x1", "cand", nil, nil)
	dec := drainDecision("gd-dec-done", "closed", drainOptionArchiveAll, "",
		[]string{"gd-x1"}, map[string]string{drainExecutedAtField: "2026-07-28T00:00:00Z"})
	engine, daemon, _, _ := drainEngine(t, c1, dec)

	engine.SweepClosedEnabledDrain(context.Background())

	if drainWasArchived(daemon, "gd-x1") {
		t.Fatalf("an already-executed decision must not archive its candidates again")
	}
}

// ── Cross-cutting guards ───────────────────────────────────────────────────

func TestClosedEnabledDrain_DisabledIsNoOp(t *testing.T) {
	trig := closedEnabledTrigger("gd-trig-1", "retired", []string{"gardener-epic:gd-epic-c"}, nil)
	epic := drainEpic("gd-epic-c", "closed")
	daemon := &mockTriggerDaemon{beads: []*beadsapi.BeadDetail{trig, epic}}
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon:                    daemon,
		Logger:                    slog.Default(),
		ClosedEnabledDrainEnabled: false, // OFF
	})

	engine.SweepClosedEnabledDrain(context.Background())

	if drainWasArchived(daemon, "gd-trig-1") {
		t.Fatalf("a disabled sweep must be a complete no-op")
	}
	if len(daemon.createdBeads) != 0 {
		t.Fatalf("a disabled sweep must not create beads")
	}
}

func TestClosedEnabledDrain_Throttle(t *testing.T) {
	trig := closedEnabledTrigger("gd-trig-1", "retired", []string{"gardener-epic:gd-epic-c"}, nil)
	epic := drainEpic("gd-epic-c", "closed")
	engine, daemon, _, clk := drainEngine(t, trig, epic)

	engine.SweepClosedEnabledDrain(context.Background())
	firstUpdates := len(daemon.updatedIDs)
	if firstUpdates == 0 {
		t.Fatalf("first run should have done work")
	}

	// Within the throttle window: a no-op (no new writes).
	*clk = clk.Add(closedEnabledDrainThrottle - time.Minute)
	engine.SweepClosedEnabledDrain(context.Background())
	if len(daemon.updatedIDs) != firstUpdates {
		t.Fatalf("second run inside throttle window must be a no-op; updates %d→%d", firstUpdates, len(daemon.updatedIDs))
	}

	// Past the throttle window: runs again (re-lists; the mock does not persist
	// enabled=false, so it re-archives — proving the throttle, not idempotency,
	// gated the middle call).
	*clk = clk.Add(2 * closedEnabledDrainThrottle)
	engine.SweepClosedEnabledDrain(context.Background())
	if len(daemon.updatedIDs) <= firstUpdates {
		t.Fatalf("run past throttle window should do work again; updates stuck at %d", len(daemon.updatedIDs))
	}
}

func TestClosedEnabledDrain_PausedIsNoOp(t *testing.T) {
	trig := closedEnabledTrigger("gd-trig-1", "retired", []string{"gardener-epic:gd-epic-c"}, nil)
	epic := drainEpic("gd-epic-c", "closed")
	// An open triggers-pause gate bead engages the global pause.
	gate := &beadsapi.BeadDetail{
		ID:     "gd-gate",
		Type:   "gate",
		Status: "open",
		Title:  triggersPauseTitle,
		Fields: map[string]string{},
	}
	engine, daemon, _, _ := drainEngine(t, trig, epic, gate)

	engine.SweepClosedEnabledDrain(context.Background())

	if drainWasArchived(daemon, "gd-trig-1") {
		t.Fatalf("under triggers-pause the sweep must not mutate any trigger")
	}
}

// ── Failure 2: machine-operator attribution (ka-0000007257) ────────────────
//
// The trigger-engine daemon client sends no operator header, so every
// operator-bearing write the sweep makes (CreateBead for the grouped decision,
// AddComment for each per-archive audit line) failed the daemon's fail-closed
// audit-operator resolver with HTTP 400 "operator could not be resolved". The
// fix stamps X-System-Operator on the sweep's context (WithSystemOperator) so
// the daemon attributes these true machine-origin cron writes to the machine
// sink. These tests assert the header is present on both write kinds.

// drainDecisionSystemOperator returns the SystemOperatorFromContext snapshot the
// mock captured for the grouped-decision CreateBead, or "".
func drainDecisionSystemOperator(m *mockTriggerDaemon) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.createdBeads {
		if m.createdBeads[i].Type != "decision" {
			continue
		}
		if i < len(m.createSystemOperators) {
			return m.createSystemOperators[i]
		}
	}
	return ""
}

func TestClosedEnabledDrain_GroupedDecisionCarriesMachineOperator(t *testing.T) {
	// The unanchored no-label cohort files a grouped decision. Without the
	// machine-operator attribution the daemon 400s CreateBead and the whole arm
	// is inert (the prod symptom). Assert the decision CreateBead carries the
	// system-operator header so the daemon can resolve it.
	a := closedEnabledTrigger("gd-nolabel-a", "probe A", []string{"trigger:multi"}, nil)
	engine, daemon, _, _ := drainEngine(t, a)

	engine.SweepClosedEnabledDrain(context.Background())

	if drainCreatedDecision(daemon) == nil {
		t.Fatalf("expected a grouped decision for the no-label cohort")
	}
	if got := drainDecisionSystemOperator(daemon); got != closedEnabledDrainSystemOperator {
		t.Errorf("grouped-decision CreateBead system-operator = %q, want %q — without it the daemon 400s the create (Failure 2)", got, closedEnabledDrainSystemOperator)
	}
}

func TestClosedEnabledDrain_ArchiveAuditCommentCarriesMachineOperator(t *testing.T) {
	// Every archive stamps an audit comment. AddComment ALSO calls the daemon's
	// fail-closed operator resolver, so the audit leg was latently broken by the
	// same missing-operator root cause as the decision arm. Assert the comment
	// write carries the system-operator header.
	trig := closedEnabledTrigger("gd-retired", "engine-retired gardener",
		[]string{"trigger:schedule"},
		map[string]string{"gardener_terminated_kind": "no_progress_for"})
	engine, daemon, _, _ := drainEngine(t, trig)

	engine.SweepClosedEnabledDrain(context.Background())

	if !drainWasArchived(daemon, "gd-retired") {
		t.Fatalf("expected the engine-retired trigger to be archived")
	}
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.commentSystemOperators) == 0 {
		t.Fatalf("expected at least one audit comment")
	}
	for i, id := range daemon.commentedIDs {
		if id != "gd-retired" {
			continue
		}
		if i >= len(daemon.commentSystemOperators) || daemon.commentSystemOperators[i] != closedEnabledDrainSystemOperator {
			t.Errorf("archive audit comment system-operator = %q, want %q — without it AddComment 400s (Failure 2)",
				daemon.commentSystemOperators[i], closedEnabledDrainSystemOperator)
		}
	}
}

// ── Failure 4: silent-zero observability guard (advice ka-0000003555) ───────

// drainEngineWithLog wires a drain-enabled engine whose logger writes to the
// returned buffer, so a test can assert the silent-zero WARN fired (or didn't).
func drainEngineWithLog(t *testing.T, beads ...*beadsapi.BeadDetail) (*TriggerEngine, *mockTriggerDaemon, *bytes.Buffer) {
	t.Helper()
	daemon := &mockTriggerDaemon{beads: beads}
	var buf bytes.Buffer
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon:                    daemon,
		Logger:                    slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})),
		ClosedEnabledDrainEnabled: true,
		Clock:                     func() time.Time { return now },
	})
	return engine, daemon, &buf
}

func TestClosedEnabledDrain_WarnsWhenTerminatedCohortArchivesZero(t *testing.T) {
	// A non-empty terminated-kind cohort that archives 0 (every write failing —
	// the exact prod symptom that hid this for two days) must WARN, not read as
	// "nothing to do". Inject an archive (UpdateBeadFieldsTyped) error so the
	// archive fails, mirroring the "operator could not be resolved" 400.
	trig := closedEnabledTrigger("gd-retired", "engine-retired",
		[]string{"trigger:schedule"},
		map[string]string{"gardener_terminated_kind": "no_progress_for"})
	engine, daemon, buf := drainEngineWithLog(t, trig)
	daemon.updateFieldsTypedErr = fmt.Errorf("HTTP 400: operator could not be resolved")

	engine.SweepClosedEnabledDrain(context.Background())

	if drainWasArchived(daemon, "gd-retired") {
		t.Fatalf("archive should have failed (injected error)")
	}
	if logs := buf.String(); !strings.Contains(logs, "terminated-kind cohort non-empty but archived 0") {
		t.Errorf("expected a WARN when a non-empty cohort archives 0; logs:\n%s", logs)
	}
}

func TestClosedEnabledDrain_NoSilentZeroWarnWhenCohortEmpty(t *testing.T) {
	// An empty pool is a legitimate zero — no false WARN.
	engine, _, buf := drainEngineWithLog(t)

	engine.SweepClosedEnabledDrain(context.Background())

	if logs := buf.String(); strings.Contains(logs, "non-empty but archived 0") || strings.Contains(logs, "non-empty but no decision filed") {
		t.Errorf("empty pool must not emit a silent-zero WARN; logs:\n%s", logs)
	}
}
