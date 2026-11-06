package main

// Unit + integration tests for the GardenerStopSpecBuilder component (task
// ka-0000000897 / I2.3). Three concerns:
//
//   1. Round-trip: a stop spec → guided-row view → form values → parsed spec
//      reconstructs the original (no field dropped, no slot mis-routed).
//   2. Validation: the no-silent-no-op checks reject exactly the combos the
//      engine treats as never-firing (max_runs with 0 budget, wall_clock with
//      a garbage duration, bead_query with a bad op, ...).
//   3. Summary: the human-readable rendering names each condition legibly.
//   4. Handler integration: POST /gardener/{id}/config with guided rows persists
//      a canonical spec, and a no-op combo re-renders with the error (no save).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/scheduler"
)

// --- Round-trip -------------------------------------------------------------

func TestStopBuilder_RoundTripsFullSpec(t *testing.T) {
	specJSON := `{
	  "loop_end": {"any": [
	    {"kind": "max_runs"},
	    {"kind": "bead_query", "op": "==", "value": "0", "parent": "gd-epic", "types": ["task","bug"], "statuses": ["open"], "exclude_blocked_by_open_types": ["escalation","decision"]},
	    {"kind": "no_progress_for", "runs": 3},
	    {"kind": "goal_satisfied"}
	  ]},
	  "hard_stop": {"any": [
	    {"kind": "wall_clock", "duration": "720h"},
	    {"kind": "consecutive_failures", "value": "5"}
	  ]},
	  "run_end": {"any": [
	    {"kind": "specific_bead_closed", "bead_id": "gd-target"}
	  ]},
	  "on_loop_end": "notify_slack"
	}`
	spec, err := scheduler.ParseStopSpec(specJSON)
	if err != nil {
		t.Fatalf("parse seed: %v", err)
	}

	// spec -> view -> form values.
	view := buildStopSpecBuilderView(spec, 100)
	form := formFromBuilderView(view)

	// form -> spec.
	got := parseStopSpecFromForm(form)

	assertSpecEqual(t, got, spec)
	if got.OnLoopEnd != "notify_slack" {
		t.Errorf("on_loop_end round-trip = %q, want notify_slack", got.OnLoopEnd)
	}
}

// formFromBuilderView serialises a rendered builder view back into the form
// field names the template emits, so the round-trip test exercises the exact
// wire format parseStopSpecFromForm reads.
func formFromBuilderView(v stopSpecBuilderView) url.Values {
	form := url.Values{}
	form.Set(stopBuilderMarker, "1")
	form.Set("on_loop_end", v.OnLoopEnd)
	for _, slot := range v.Slots {
		for _, row := range slot.Rows {
			prefix := "stop_" + row.Slot + "_" + itoa(row.Index) + "_"
			form.Set(prefix+"kind", row.Kind)
			for _, f := range row.Fields {
				if f.Value != "" {
					form.Set(prefix+f.Name, f.Value)
				}
			}
		}
	}
	return form
}

func assertSpecEqual(t *testing.T, got, want scheduler.StopSpec) {
	t.Helper()
	gj, _ := json.Marshal(got)
	wj, _ := json.Marshal(want)
	if string(gj) != string(wj) {
		t.Errorf("spec mismatch:\n got=%s\nwant=%s", gj, wj)
	}
}

func TestStopBuilder_KindSwitchDropsStaleFields(t *testing.T) {
	// A row that carries fields from a prior kind must not leak them: the
	// parser only reads fields the CURRENT kind declares.
	form := url.Values{}
	form.Set(stopBuilderMarker, "1")
	form.Set("stop_loop_end_0_kind", "goal_satisfied")
	// Stale leftovers from when the row was a wall_clock / bead_query:
	form.Set("stop_loop_end_0_duration", "720h")
	form.Set("stop_loop_end_0_op", "==")
	form.Set("stop_loop_end_0_value", "5")

	spec := parseStopSpecFromForm(form)
	if len(spec.LoopEnd.Any) != 1 {
		t.Fatalf("want 1 entry, got %d", len(spec.LoopEnd.Any))
	}
	e := spec.LoopEnd.Any[0]
	if e.Kind != "goal_satisfied" || e.Duration != "" || e.Op != "" || e.Value != "" {
		t.Errorf("stale fields leaked: %+v", e)
	}
}

func TestStopBuilder_GapToleranceInRowIndices(t *testing.T) {
	// The client removes a middle row without renumbering; the server must
	// still collect rows 0 and 2.
	form := url.Values{}
	form.Set(stopBuilderMarker, "1")
	form.Set("stop_loop_end_0_kind", "goal_satisfied")
	form.Set("stop_loop_end_2_kind", "max_runs")

	spec := parseStopSpecFromForm(form)
	if len(spec.LoopEnd.Any) != 2 {
		t.Fatalf("gap tolerance: want 2 entries, got %d (%+v)", len(spec.LoopEnd.Any), spec.LoopEnd.Any)
	}
	if spec.LoopEnd.Any[0].Kind != "goal_satisfied" || spec.LoopEnd.Any[1].Kind != "max_runs" {
		t.Errorf("gap tolerance: wrong order/kinds: %+v", spec.LoopEnd.Any)
	}
}

// --- Validation (the core "no silent no-ops" acceptance) --------------------

func TestValidateStopSpec_MaxRunsZeroBudgetIsBlocked(t *testing.T) {
	spec := scheduler.StopSpec{
		LoopEnd: scheduler.StopSlot{Any: []scheduler.StopEntry{{Kind: scheduler.StopKindMaxRuns}}},
	}
	// 0 budget → the canonical silent no-op.
	if v := validateStopSpec(spec, 0); v.OK() {
		t.Errorf("max_runs with 0 budget must be blocked; got OK")
	}
	// Positive budget → fine.
	if v := validateStopSpec(spec, 20); !v.OK() {
		t.Errorf("max_runs with positive budget must pass; got errors %v", v.Errors)
	}
}

func TestValidateStopSpec_NoOpCombos(t *testing.T) {
	cases := []struct {
		name  string
		entry scheduler.StopEntry
		slot  string
		block bool
	}{
		{"wall_clock empty duration", scheduler.StopEntry{Kind: scheduler.StopKindWallClock}, scheduler.StopSlotHardStop, true},
		{"wall_clock garbage duration", scheduler.StopEntry{Kind: scheduler.StopKindWallClock, Duration: "soon"}, scheduler.StopSlotHardStop, true},
		{"wall_clock valid", scheduler.StopEntry{Kind: scheduler.StopKindWallClock, Duration: "720h"}, scheduler.StopSlotHardStop, false},
		{"no_progress_for zero runs", scheduler.StopEntry{Kind: scheduler.StopKindNoProgressFor}, scheduler.StopSlotLoopEnd, true},
		{"no_progress_for valid", scheduler.StopEntry{Kind: scheduler.StopKindNoProgressFor, Runs: 3}, scheduler.StopSlotLoopEnd, false},
		{"consecutive_failures zero", scheduler.StopEntry{Kind: scheduler.StopKindConsecutiveFailures, Value: "0"}, scheduler.StopSlotHardStop, true},
		{"consecutive_failures garbage", scheduler.StopEntry{Kind: scheduler.StopKindConsecutiveFailures, Value: "lots"}, scheduler.StopSlotHardStop, true},
		{"consecutive_failures valid", scheduler.StopEntry{Kind: scheduler.StopKindConsecutiveFailures, Value: "5"}, scheduler.StopSlotHardStop, false},
		{"bead_query bad op", scheduler.StopEntry{Kind: scheduler.StopKindBeadQuery, Op: "~=", Value: "0", Parent: "gd-x"}, scheduler.StopSlotLoopEnd, true},
		{"bead_query bad value", scheduler.StopEntry{Kind: scheduler.StopKindBeadQuery, Op: "==", Value: "none", Parent: "gd-x"}, scheduler.StopSlotLoopEnd, true},
		{"bead_query valid", scheduler.StopEntry{Kind: scheduler.StopKindBeadQuery, Op: "==", Value: "0", Parent: "gd-x"}, scheduler.StopSlotLoopEnd, false},
		{"specific_bead_closed empty", scheduler.StopEntry{Kind: scheduler.StopKindSpecificBeadClosed}, scheduler.StopSlotRunEnd, true},
		{"gate_present empty", scheduler.StopEntry{Kind: scheduler.StopKindGatePresent}, scheduler.StopSlotLoopEnd, true},
		{"mr_merged in hard_stop", scheduler.StopEntry{Kind: scheduler.StopKindMRMerged}, scheduler.StopSlotHardStop, true},
		{"mr_merged in loop_end", scheduler.StopEntry{Kind: scheduler.StopKindMRMerged}, scheduler.StopSlotLoopEnd, false},
		{"goal_satisfied always fine", scheduler.StopEntry{Kind: scheduler.StopKindGoalSatisfied}, scheduler.StopSlotLoopEnd, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := specWithEntry(tc.slot, tc.entry)
			v := validateStopSpec(spec, 50)
			if tc.block && v.OK() {
				t.Errorf("expected block, got OK (advisories=%v)", v.Advisories)
			}
			if !tc.block && !v.OK() {
				t.Errorf("expected OK, got errors %v", v.Errors)
			}
		})
	}
}

func TestValidateStopSpec_UnscopedBeadQueryIsAdvisoryNotError(t *testing.T) {
	spec := specWithEntry(scheduler.StopSlotLoopEnd,
		scheduler.StopEntry{Kind: scheduler.StopKindBeadQuery, Op: "==", Value: "0"})
	v := validateStopSpec(spec, 50)
	if !v.OK() {
		t.Errorf("unscoped bead_query should not be a blocking error; got %v", v.Errors)
	}
	if len(v.Advisories) == 0 {
		t.Errorf("unscoped bead_query should surface an advisory")
	}
}

func specWithEntry(slot string, e scheduler.StopEntry) scheduler.StopSpec {
	var spec scheduler.StopSpec
	switch slot {
	case scheduler.StopSlotLoopEnd:
		spec.LoopEnd.Any = []scheduler.StopEntry{e}
	case scheduler.StopSlotHardStop:
		spec.HardStop.Any = []scheduler.StopEntry{e}
	case scheduler.StopSlotRunEnd:
		spec.RunEnd.Any = []scheduler.StopEntry{e}
	}
	return spec
}

// --- Summary ----------------------------------------------------------------

func TestSummarizeStopSpec_HumanReadable(t *testing.T) {
	spec, _ := scheduler.ParseStopSpec(`{
	  "loop_end": {"any": [
	    {"kind": "max_runs"},
	    {"kind": "bead_query", "op": "==", "value": "0", "parent": "gd-epic", "statuses": ["open"]},
	    {"kind": "goal_satisfied"}
	  ]},
	  "hard_stop": {"any": [{"kind": "wall_clock", "duration": "720h"}]}
	}`)
	lines := summarizeStopSpec(spec)
	joined := strings.Join(lines, "\n")
	for _, want := range []string{
		"End the loop when the run budget is exhausted.",
		"End the loop when the count of open beads under gd-epic == 0",
		"End the loop when the gardener declares its goal satisfied.",
		"Kill the pod after 720h of wall-clock time.",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("summary missing %q; got:\n%s", want, joined)
		}
	}
}

func TestSummarizeStopSpec_EmptyIsNil(t *testing.T) {
	if s := summarizeStopSpec(scheduler.StopSpec{}); s != nil {
		t.Errorf("empty spec summary should be nil, got %v", s)
	}
}

// --- JSON encoding ----------------------------------------------------------

func TestStopSpecToJSON_EmptyIsNil(t *testing.T) {
	raw, err := stopSpecToJSON(scheduler.StopSpec{})
	if err != nil {
		t.Fatal(err)
	}
	if raw != nil {
		t.Errorf("empty spec should encode to nil (omit field), got %s", raw)
	}
}

// --- Handler integration ----------------------------------------------------

// seedGardenerForStopBuilder builds a gardener with a run budget so max_runs is
// valid, plus a live runtime counter that a save must preserve.
func seedGardenerForStopBuilder(t *testing.T) *fakeDaemon {
	t.Helper()
	f := newFake()
	spec := map[string]any{
		"name":          "sb",
		"terminal_goal": "drain",
		"run_budget":    float64(20),
		"max_runs":      float64(20),
		"runs_spawned":  float64(4),
		"last_agent_id": "gd-run4",
	}
	raw, _ := json.Marshal(spec)
	f.beads["gd-sb"] = &beadsapi.BeadDetail{
		ID:        "gd-sb",
		Title:     "SB gardener",
		Type:      "trigger",
		Status:    "open",
		Labels:    []string{"trigger:schedule", "role:gardener", "project:gardenio"},
		Fields:    map[string]string{"role": "gardener", "gardener": string(raw)},
		UpdatedAt: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
	}
	return f
}

func TestGardenerConfigSave_GuidedRowsPersistCanonicalSpec(t *testing.T) {
	f := seedGardenerForStopBuilder(t)
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("run_budget", "20")
	form.Set("max_runs", "20")
	form.Set(stopBuilderMarker, "1")
	form.Set("on_loop_end", "close")
	// loop_end: max_runs + goal_satisfied
	form.Set("stop_loop_end_0_kind", "max_runs")
	form.Set("stop_loop_end_1_kind", "goal_satisfied")
	// hard_stop: wall_clock 720h
	form.Set("stop_hard_stop_0_kind", "wall_clock")
	form.Set("stop_hard_stop_0_duration", "720h")

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, opsReqSB("POST", "/gardener/gd-sb/config", form))
	if w.Code != http.StatusOK {
		t.Fatalf("save: got %d body=%s", w.Code, w.Body.String())
	}
	persisted, ok := f.updateFieldsLastVal["gardener"].(string)
	if !ok {
		t.Fatalf("spec not persisted; got %v", f.updateFieldsLastVal)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(persisted), &got); err != nil {
		t.Fatal(err)
	}
	// Runtime preserved.
	if got["runs_spawned"] != float64(4) || got["last_agent_id"] != "gd-run4" {
		t.Errorf("runtime clobbered: runs_spawned=%v last_agent_id=%v", got["runs_spawned"], got["last_agent_id"])
	}
	// Stop spec round-tripped through the canonical shape.
	stopRaw, _ := json.Marshal(got["stop"])
	stop, err := scheduler.ParseStopSpec(string(stopRaw))
	if err != nil {
		t.Fatalf("persisted stop not parseable: %v (%s)", err, stopRaw)
	}
	if len(stop.LoopEnd.Any) != 2 || len(stop.HardStop.Any) != 1 {
		t.Fatalf("persisted stop shape wrong: %+v", stop)
	}
	if stop.HardStop.Any[0].Kind != "wall_clock" || stop.HardStop.Any[0].Duration != "720h" {
		t.Errorf("wall_clock not persisted: %+v", stop.HardStop.Any[0])
	}
}

func TestGardenerConfigSave_GuidedRowsRejectSilentNoOp(t *testing.T) {
	f := seedGardenerForStopBuilder(t)
	mux := mkServer(t, f)

	form := url.Values{}
	// Budget 0 makes the max_runs loop_end a silent no-op.
	form.Set("run_budget", "0")
	form.Set("max_runs", "0")
	form.Set(stopBuilderMarker, "1")
	form.Set("stop_loop_end_0_kind", "max_runs")

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, opsReqSB("POST", "/gardener/gd-sb/config", form))
	if w.Code != http.StatusOK {
		t.Fatalf("save (no-op): got %d", w.Code)
	}
	if f.updateFieldsCalls != 0 {
		t.Errorf("no-op combo must not persist; got %d updates", f.updateFieldsCalls)
	}
	body := w.Body.String()
	if !strings.Contains(body, "never fire") {
		t.Errorf("expected a never-fire validation error in the re-rendered form; body=%s", body)
	}
}

func TestGardenerConfigSave_GuidedRowsRejectBadWallClock(t *testing.T) {
	f := seedGardenerForStopBuilder(t)
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("run_budget", "20")
	form.Set("max_runs", "20")
	form.Set(stopBuilderMarker, "1")
	form.Set("stop_hard_stop_0_kind", "wall_clock")
	form.Set("stop_hard_stop_0_duration", "whenever")

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, opsReqSB("POST", "/gardener/gd-sb/config", form))
	if f.updateFieldsCalls != 0 {
		t.Errorf("bad wall_clock must not persist; got %d updates", f.updateFieldsCalls)
	}
	if !strings.Contains(w.Body.String(), "never fire") {
		t.Errorf("expected wall_clock validation error; body=%s", w.Body.String())
	}
}

func TestGardenerEditForm_RendersGuidedBuilder(t *testing.T) {
	f := seedGardenerForStopBuilder(t)
	// Give it a real stop spec to render as guided rows.
	spec := map[string]any{
		"name":       "sb",
		"run_budget": float64(20),
		"stop": map[string]any{
			"loop_end":    map[string]any{"any": []any{map[string]any{"kind": "goal_satisfied"}}},
			"hard_stop":   map[string]any{"any": []any{map[string]any{"kind": "wall_clock", "duration": "720h"}}},
			"on_loop_end": "close",
		},
	}
	raw, _ := json.Marshal(spec)
	f.beads["gd-sb"].Fields["gardener"] = string(raw)

	mux := mkServer(t, f)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, opsReqSB("GET", "/gardener/gd-sb/edit", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("edit form: got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`name="stop_builder"`,             // the marker
		`data-stop-builder`,               // the fieldset
		`name="stop_loop_end_0_kind"`,     // a rendered guided row
		"720h",                            // the wall_clock duration pre-filled
		"Stop conditions",                 // the legend
		`data-kind-template="wall_clock"`, // a client add-row template
	} {
		if !strings.Contains(body, want) {
			t.Errorf("edit form missing %q", want)
		}
	}
}

// opsReqSB is opsReq with the SB fixture's operator email.
func opsReqSB(method, path string, form url.Values) *http.Request {
	return opsReq(method, path, form)
}
