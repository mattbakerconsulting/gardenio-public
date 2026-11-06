package main

// Server-side stop-spec VALIDATION tests for the gardener-config edit surface
// (I2.5, task ka-0000002260 under epic ka-0000001611 "Gardener Configuration
// Surface"). Companion to gardener_config_roundtrip_test.go: that file proves a
// save PRESERVES the full spec; this file proves a save REJECTS a stop spec the
// engine would silently ignore.
//
// The gap I2.5 closes: handleGardenerConfigSave used to guard the submitted
// stop-spec textarea with json.Valid ALONE. json.Valid accepts dead config — a
// typo'd kind ("max_run"), a wall_clock whose duration does not parse, a
// consecutive_failures with a non-numeric value, a bead_query with an unknown
// op. Every one decodes fine yet the engine's EvaluateGardenerStopEntry returns
// not-met (fail-open) for it, so the gardener never stops the way its author
// declared — a SILENT failure. The save handler now runs
// scheduler.ValidateStopSpec (which mirrors the evaluator's per-kind
// requirements) so the UI never persists a spec the engine chokes on.
//
// These tests drive the REAL POST /gardener/{id}/config handler end-to-end
// against the shared archetype fixture, so they lock the behavior at the HTTP
// surface an operator actually hits — not just the validator in isolation
// (that unit lives in internal/scheduler/stop_validate_test.go).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// baseConfigEdits returns the minimal valid form body for the fsm-drain
// archetype (gd-archcap) — the non-stop knobs a save always sends. Individual
// cases overlay a stop_json onto this.
func baseConfigEdits() url.Values {
	return url.Values{
		"terminal_goal": {"Every released wave's FSM tickets carry gardenio-triaged."},
		"run_budget":    {"30"},
		"max_runs":      {"30"},
	}
}

// TestGardenerConfigSave_RejectsEngineDeadStopSpec is the core I2.5 assertion:
// each submitted stop spec is valid JSON but semantically dead, and the save
// must reject it with an inline error naming the problem — NOT persist it.
func TestGardenerConfigSave_RejectsEngineDeadStopSpec(t *testing.T) {
	cases := []struct {
		name     string
		stopJSON string
		wantSub  string // substring the inline error must surface
	}{
		{
			"typo'd kind",
			`{"loop_end":{"any":[{"kind":"max_run"}]}}`,
			"unknown kind",
		},
		{
			"wall_clock unparseable duration",
			`{"hard_stop":{"any":[{"kind":"wall_clock","duration":"forever"}]}}`,
			"does not parse",
		},
		{
			"consecutive_failures non-numeric value",
			`{"hard_stop":{"any":[{"kind":"consecutive_failures","value":"lots"}]}}`,
			"integer",
		},
		{
			"no_progress_for missing runs",
			`{"loop_end":{"any":[{"kind":"no_progress_for"}]}}`,
			// `>` is HTML-escaped to &gt; in the rendered template body.
			"runs&gt;0",
		},
		{
			"bead_query unknown op",
			`{"loop_end":{"any":[{"kind":"bead_query","op":"=~","value":"0"}]}}`,
			"recognized comparison",
		},
		{
			"unknown on_loop_end action",
			`{"loop_end":{"any":[{"kind":"max_runs"}]},"on_loop_end":"burn_it_down"}`,
			"recognized action",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			f := seedArchetypeGardener(t, fsmDrainArchetype().spec)
			edits := baseConfigEdits()
			edits.Set("stop_json", tc.stopJSON)

			mux := mkServer(t, f)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, opsReq("POST", "/gardener/gd-archcap/config", edits))

			// Rendered at HTTP 200 with an inline alert (htmx swap contract —
			// renderGardenerConfigError, see ka-0000005181), NOT a 4xx.
			if w.Code != http.StatusOK {
				t.Fatalf("expected 200 (inline error), got %d body=%s", w.Code, w.Body.String())
			}
			body := w.Body.String()
			if !strings.Contains(body, `role="alert"`) {
				t.Errorf("expected an inline error alert, got body=%s", body)
			}
			if !strings.Contains(body, "Stop spec is invalid") {
				t.Errorf("error should be attributed to the stop spec; body=%s", body)
			}
			if !strings.Contains(body, tc.wantSub) {
				t.Errorf("error should name the problem (%q); body=%s", tc.wantSub, body)
			}
			// The dead spec must NOT have been persisted.
			if _, persisted := f.updateFieldsLastVal["gardener"]; persisted {
				t.Errorf("a semantically-invalid stop spec was persisted: %v", f.updateFieldsLastVal["gardener"])
			}
		})
	}
}

// TestGardenerConfigSave_AcceptsEngineValidStopSpec is the twin: a full,
// engine-valid stop spec (the home-beauty archetype's real loop_end/hard_stop
// blob) submitted through the same handler must persist cleanly with no error.
// This guards against the validator becoming over-strict and rejecting specs
// the engine CAN run — the round-trip-parity failure mode in the opposite
// direction.
func TestGardenerConfigSave_AcceptsEngineValidStopSpec(t *testing.T) {
	f := seedArchetypeGardener(t, fsmDrainArchetype().spec)
	edits := baseConfigEdits()
	edits.Set("stop_json", mustCompactJSON(map[string]any{
		"loop_end": map[string]any{
			"any": []any{
				map[string]any{"kind": "bead_query", "value": "0", "op": "==", "parent": "gd-qipN5RP8Do"},
				map[string]any{"kind": "max_runs"},
				map[string]any{"kind": "no_progress_for", "runs": float64(3)},
			},
		},
		"hard_stop": map[string]any{
			"any": []any{
				map[string]any{"kind": "consecutive_failures", "value": "5"},
				map[string]any{"kind": "wall_clock", "duration": "720h"},
			},
		},
		"on_loop_end": "close",
	}))

	mux := mkServer(t, f)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, opsReq("POST", "/gardener/gd-archcap/config", edits))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), `role="alert"`) {
		t.Fatalf("a valid stop spec was rejected; body=%s", w.Body.String())
	}
	persisted, ok := f.updateFieldsLastVal["gardener"].(string)
	if !ok {
		t.Fatalf("valid save did not persist a gardener spec; got %v", f.updateFieldsLastVal)
	}
	// Belt-and-suspenders: the persisted bytes must re-parse and carry the
	// submitted stop spec through, so a valid save is genuinely round-trippable.
	var got map[string]any
	if err := json.Unmarshal([]byte(persisted), &got); err != nil {
		t.Fatalf("persisted spec not JSON: %v", err)
	}
	stop, ok := got["stop"].(map[string]any)
	if !ok {
		t.Fatalf("persisted stop spec missing/not an object: %#v", got["stop"])
	}
	if _, ok := stop["hard_stop"]; !ok {
		t.Errorf("persisted stop spec lost its hard_stop slot: %#v", stop)
	}
}

// TestGardenerConfigSave_EmptyStopSpecStillValid confirms clearing the stop
// textarea (no constraints) remains a valid save — validation only bites a
// NON-empty dead spec, never an intentionally-empty one.
func TestGardenerConfigSave_EmptyStopSpecStillValid(t *testing.T) {
	f := seedArchetypeGardener(t, fsmDrainArchetype().spec)
	edits := baseConfigEdits()
	edits.Set("stop_json", "")

	mux := mkServer(t, f)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, opsReq("POST", "/gardener/gd-archcap/config", edits))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), `role="alert"`) {
		t.Fatalf("an empty stop spec was rejected; body=%s", w.Body.String())
	}
	if _, ok := f.updateFieldsLastVal["gardener"].(string); !ok {
		t.Fatalf("empty-stop save did not persist; got %v", f.updateFieldsLastVal)
	}
}
