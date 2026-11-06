package main

import (
	"context"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// fakeGateClient is a purpose-built daemon fake for the gate probe. It serves
// bead_query list results (honoring the daemon-side exclude_labels filter itself,
// exactly as the real daemon does) plus the batch dep/bead lookups the
// exclude_blocked_by_open_types path needs. It is the smallest client that lets
// buildGateEval run the REAL evaluator (scheduler.EvaluateConditions +
// conditions.EvaluateBeadQueryCount) end-to-end, so the test exercises the fire
// GATE itself, not a reimplementation of it.
type fakeGateClient struct {
	// rows is the full label-matched set BEFORE the exclude_labels subtraction.
	rows []*beadsapi.BeadDetail
	// depsByChild / beadsByID drive the exclude_blocked_by_open_types walk.
	depsByChild map[string][]beadsapi.Dependency
	beadsByID   map[string]*beadsapi.BeadDetail
}

func (f *fakeGateClient) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	// Apply the daemon-side exclude_labels filter ourselves — this is the exact
	// subtraction the real daemon performs for the effective query, and its
	// absence on the raw query is what makes raw and effective diverge.
	excl := make(map[string]struct{}, len(q.ExcludeLabels))
	for _, l := range q.ExcludeLabels {
		excl[l] = struct{}{}
	}
	var kept []*beadsapi.BeadDetail
	for _, b := range f.rows {
		drop := false
		for _, lab := range b.Labels {
			if _, bad := excl[lab]; bad {
				drop = true
				break
			}
		}
		if !drop {
			kept = append(kept, b)
		}
	}
	return &beadsapi.ListBeadsResult{Beads: kept, Total: len(kept)}, nil
}

func (f *fakeGateClient) BatchGetDependencies(_ context.Context, ids []string, _ string) (map[string][]beadsapi.Dependency, error) {
	out := make(map[string][]beadsapi.Dependency, len(ids))
	for _, id := range ids {
		out[id] = f.depsByChild[id]
	}
	return out, nil
}

func (f *fakeGateClient) BatchGetBeads(_ context.Context, ids []string) (map[string]*beadsapi.BeadDetail, error) {
	out := make(map[string]*beadsapi.BeadDetail, len(ids))
	for _, id := range ids {
		if b, ok := f.beadsByID[id]; ok {
			out[id] = b
		}
	}
	return out, nil
}

func bead(id string, labels ...string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{ID: id, Type: "task", Status: "open", Labels: labels}
}

// TestBuildGateEval_IntakeGroomer_ExcludeLabelsFloorsLaneToZero is the live-fire
// regression for bug ka-0000000876. The intake-groomer trigger (ka-0000006210)
// fires a bead_query on gardenio needs-intake beads but subtracts
// source:multi/source:slack/thread/... via exclude_labels. In production every
// queued needs-intake bead carried one of those labels, so the effective count
// was 0 and the trigger CORRECTLY declined to fire — yet the unconditional
// fire-diff reported it as 300+ "missed" fires and the reconciler mis-filed it as
// habitual drift. The gate probe must now surface would_fire_now=false with a
// leaf whose raw>0, effective=0, subtraction_explains_miss=true — the
// deterministic signal that the misses are gated, not drift.
func TestBuildGateEval_IntakeGroomer_ExcludeLabelsFloorsLaneToZero(t *testing.T) {
	const conds = `[{"type":"bead_query","op":">","value":"0","project":"gardenio",` +
		`"labels":["needs-intake"],"types":["task","bug","feature","epic"],"statuses":["open"],` +
		`"exclude_labels":["source:multi","source:schedule","thread","source:slack","slack-thread"]}]`

	fc := &fakeGateClient{
		rows: []*beadsapi.BeadDetail{
			bead("gd-a", "needs-intake", "project:gardenio", "source:multi"),
			bead("gd-b", "needs-intake", "project:gardenio", "source:slack", "thread"),
			bead("gd-c", "needs-intake", "project:gardenio", "source:multi"),
		},
	}

	g := buildGateEval(context.Background(), fc, conds, "gardenio")
	if g == nil {
		t.Fatal("expected a gate eval for a conditioned trigger, got nil")
	}
	if !g.Evaluated {
		t.Fatalf("gate not evaluated: %s", g.Error)
	}
	if g.WouldFireNow {
		t.Fatal("would_fire_now must be FALSE: every needs-intake bead carries an excluded label")
	}
	if len(g.Leaves) != 1 {
		t.Fatalf("expected 1 exclude-bearing leaf, got %d", len(g.Leaves))
	}
	l := g.Leaves[0]
	if l.RawCount != 3 {
		t.Errorf("raw_count: want 3 (all needs-intake beads), got %d", l.RawCount)
	}
	if l.EffectiveCount != 0 {
		t.Errorf("effective_count: want 0 (all excluded), got %d", l.EffectiveCount)
	}
	if l.EffectiveWouldFire {
		t.Error("effective_would_fire must be false (0 is not > 0)")
	}
	if !l.SubtractionExplainsMiss {
		t.Fatal("subtraction_explains_miss must be TRUE: raw>0 would fire, effective=0 does not — the misses are gated")
	}
}

// TestBuildGateEval_Rescope_ExcludeBlockedByOpenDecisionFloorsLaneToZero is the
// live-fire regression for the sibling bug ka-0000005279. The gardener-rescope
// trigger (ka-0000007515) fires on gardenio needs-rescope beads but subtracts any
// bead blocked_by an OPEN decision/escalation. In production all three queued
// needs-rescope beads were each blocked by an open decision, so the effective
// count was 0 and the no-fire was correct — same measurement artifact, DIFFERENT
// exclude mechanism (blocked_by, not labels). The gate probe must reach the same
// verdict via the transitive-blocked walk.
func TestBuildGateEval_Rescope_ExcludeBlockedByOpenDecisionFloorsLaneToZero(t *testing.T) {
	const conds = `[{"type":"bead_query","op":">","value":"0","project":"gardenio",` +
		`"labels":["needs-rescope"],"types":["task","bug","feature","epic"],"statuses":["open"],` +
		`"exclude_blocked_by_open_types":["decision","escalation"]}]`

	fc := &fakeGateClient{
		rows: []*beadsapi.BeadDetail{
			bead("gd-r1", "needs-rescope", "project:gardenio"),
			bead("gd-r2", "needs-rescope", "project:gardenio"),
			bead("gd-r3", "needs-rescope", "project:gardenio"),
		},
		depsByChild: map[string][]beadsapi.Dependency{
			"gd-r1": {{BeadID: "gd-r1", DependsOnID: "gd-d1", Type: "blocks"}},
			"gd-r2": {{BeadID: "gd-r2", DependsOnID: "gd-d2", Type: "blocks"}},
			"gd-r3": {{BeadID: "gd-r3", DependsOnID: "gd-d3", Type: "blocks"}},
		},
		beadsByID: map[string]*beadsapi.BeadDetail{
			"gd-d1": {ID: "gd-d1", Type: "decision", Status: "open"},
			"gd-d2": {ID: "gd-d2", Type: "decision", Status: "open"},
			"gd-d3": {ID: "gd-d3", Type: "decision", Status: "open"},
		},
	}

	g := buildGateEval(context.Background(), fc, conds, "gardenio")
	if g == nil {
		t.Fatal("expected a gate eval, got nil")
	}
	if !g.Evaluated {
		t.Fatalf("gate not evaluated: %s", g.Error)
	}
	if g.WouldFireNow {
		t.Fatal("would_fire_now must be FALSE: every needs-rescope bead is blocked by an open decision")
	}
	if len(g.Leaves) != 1 {
		t.Fatalf("expected 1 exclude-bearing leaf, got %d", len(g.Leaves))
	}
	l := g.Leaves[0]
	if l.RawCount != 3 || l.EffectiveCount != 0 {
		t.Errorf("want raw=3 effective=0, got raw=%d effective=%d", l.RawCount, l.EffectiveCount)
	}
	if !l.SubtractionExplainsMiss {
		t.Fatal("subtraction_explains_miss must be TRUE: all three beads are operator-decision-gated")
	}
}

// TestBuildGateEval_UnblockedLaneWouldFire proves the probe does NOT cry
// "gated" when the lane really has actionable work: an unblocked needs-rescope
// bead survives every subtraction, so would_fire_now=true and
// subtraction_explains_miss=false. This is the guard against the fix hiding a
// GENUINE dark trigger — a real miss must still read as a real miss.
func TestBuildGateEval_UnblockedLaneWouldFire(t *testing.T) {
	const conds = `[{"type":"bead_query","op":">","value":"0","project":"gardenio",` +
		`"labels":["needs-rescope"],"types":["task","bug","feature","epic"],"statuses":["open"],` +
		`"exclude_blocked_by_open_types":["decision","escalation"]}]`

	fc := &fakeGateClient{
		rows: []*beadsapi.BeadDetail{
			bead("gd-r1", "needs-rescope", "project:gardenio"), // no blocker → actionable
		},
	}

	g := buildGateEval(context.Background(), fc, conds, "gardenio")
	if g == nil || !g.Evaluated {
		t.Fatalf("expected an evaluated gate, got %+v", g)
	}
	if !g.WouldFireNow {
		t.Fatal("would_fire_now must be TRUE: an unblocked needs-rescope bead is real work")
	}
	if len(g.Leaves) != 1 || g.Leaves[0].SubtractionExplainsMiss {
		t.Fatalf("subtraction must NOT explain a miss when the lane fires: %+v", g.Leaves)
	}
}

// TestBuildGateEval_NoConditionsReturnsNil confirms an unconditional trigger
// (no conditions field) gets no gate block — its misses are genuine, and there
// is no subtraction to explain them.
func TestBuildGateEval_NoConditionsReturnsNil(t *testing.T) {
	if g := buildGateEval(context.Background(), &fakeGateClient{}, "", "gardenio"); g != nil {
		t.Fatalf("expected nil gate for an unconditional trigger, got %+v", g)
	}
}

// TestBuildGateEval_EventPayloadGateNotStoreEvaluable is the regression for
// impossible-drift false positive ka-0000001670. The fix-proof pour trigger
// ka-0000002857 is EVENT-driven: it fires when a bead CLOSE event carries
// field-leaf conditions ({"field":"source_step_id","op":"==","value":"emit..."},
// {"field":"gate_passed","op":"==","value":"true"}, ...). Those match an
// in-process event payload, not a store query. The legacy scheduler.Condition
// shape has no `field` member, so it silently drops the field key and collapses
// every such condition to a no-op "no constraint" leaf that evaluates TRUE —
// producing a phantom would_fire_now=true that misclassified this event-only
// trigger as impossible drift. The gate probe must now recognize the field
// leaves and report the gate as un-evaluable (Evaluated=false,
// EventPayloadGate=true) instead of emitting a bogus would_fire_now.
func TestBuildGateEval_EventPayloadGateNotStoreEvaluable(t *testing.T) {
	raw := `[{"field":"type","op":"==","value":"task"},` +
		`{"field":"source_formula","op":"==","value":"ka-0000002084"},` +
		`{"field":"source_step_id","op":"==","value":"emit-repro-harness"},` +
		`{"field":"gate_passed","op":"==","value":"true"}]`

	g := buildGateEval(context.Background(), &fakeGateClient{}, raw, "monorepo")
	if g == nil {
		t.Fatal("expected a non-nil gate for a conditions-bearing trigger")
	}
	if !g.EventPayloadGate {
		t.Fatalf("expected EventPayloadGate=true for a field-leaf gate, got %+v", g)
	}
	if g.Evaluated {
		t.Fatal("an event-payload gate is not store-evaluable: Evaluated must be false")
	}
	if g.WouldFireNow {
		t.Fatal("event-payload gate must NOT report would_fire_now=true (the phantom that filed ka-0000001670)")
	}
	if g.Error == "" {
		t.Fatal("expected an explanatory Error string on an un-evaluable event-payload gate")
	}
}

// TestBuildGateEval_MixedFieldAndDaemonLeavesIsEventPayload guards the boundary:
// a gate that mixes a store-evaluable daemon leaf with even ONE event-payload
// field leaf still cannot be evaluated as a point-in-time store snapshot (the
// field leaf has no payload to match), so it is treated as an event-payload gate.
func TestBuildGateEval_MixedFieldAndDaemonLeavesIsEventPayload(t *testing.T) {
	raw := `[{"type":"bead_query","op":">","value":"0"},` +
		`{"field":"gate_passed","op":"==","value":"true"}]`

	g := buildGateEval(context.Background(), &fakeGateClient{}, raw, "monorepo")
	if g == nil {
		t.Fatal("expected a non-nil gate")
	}
	if !g.EventPayloadGate || g.Evaluated || g.WouldFireNow {
		t.Fatalf("a mixed gate with a field leaf must be un-evaluable event-payload gate, got %+v", g)
	}
}
