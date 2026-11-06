package main

// Edit-form parity tests for the scheduling knobs the config-save handler
// omitted before I2.2 (task ka-0000006347 under epic ka-0000001611 "Gardener
// Configuration Surface"). Before this the edit form exposed only the goal /
// budget / stop knobs, so opening a full-power gardener and hitting Save
// silently flattened away its cadence (cron), its per-cycle fire caps
// (cooldown_minutes, max_concurrent), its pre-flight gate (conditions[]), and
// its progress spec — the exact "surface exposes a sliver of the model"
// failure the epic exists to fix.
//
// These drive the REAL POST /gardener/{id}/config handler end-to-end so they
// lock behaviour at the HTTP surface an operator hits. The four scheduling
// fields are TOP-LEVEL trigger-bead fields (not part of the `gardener` blob);
// progress lives INSIDE the blob (GardenerSpec.Progress).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestGardenerConfigSave_PersistsSchedulingKnobs confirms a save writes the four
// top-level scheduling fields verbatim alongside the gardener blob.
func TestGardenerConfigSave_PersistsSchedulingKnobs(t *testing.T) {
	f := seedArchetypeGardener(t, fsmDrainArchetype().spec)
	edits := baseConfigEdits()
	edits.Set("cron", "0 9 * * 1-5")
	edits.Set("cooldown_minutes", "15")
	edits.Set("max_concurrent", "2")
	edits.Set("conditions", `[{"kind":"bead_query","parent":"gd-WiKagFODjX","op":">","value":"0"}]`)

	mux := mkServer(t, f)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, opsReq("POST", "/gardener/gd-archcap/config", edits))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), `role="alert"`) {
		t.Fatalf("valid scheduling knobs were rejected; body=%s", w.Body.String())
	}

	last := f.updateFieldsLastVal
	if got := last["cron"]; got != "0 9 * * 1-5" {
		t.Errorf("cron = %#v, want %q", got, "0 9 * * 1-5")
	}
	if got := last["cooldown_minutes"]; got != "15" {
		t.Errorf("cooldown_minutes = %#v, want %q", got, "15")
	}
	if got := last["max_concurrent"]; got != "2" {
		t.Errorf("max_concurrent = %#v, want %q", got, "2")
	}
	if got, ok := last["conditions"].(string); !ok || !strings.Contains(got, "bead_query") {
		t.Errorf("conditions = %#v, want the submitted JSON array", last["conditions"])
	}
	// The gardener blob is still written in the same update.
	if _, ok := last["gardener"].(string); !ok {
		t.Errorf("gardener blob not persisted alongside scheduling knobs; got %v", last)
	}
}

// TestGardenerConfigSave_RejectsBadSchedulingKnobs proves each scheduling knob is
// validated through the engine's own parser, so a value the engine would choke
// on (or silently ignore) reds the save with an attributed inline error instead
// of being persisted.
func TestGardenerConfigSave_RejectsBadSchedulingKnobs(t *testing.T) {
	cases := []struct {
		name    string
		field   string
		value   string
		wantSub string
	}{
		{"unparseable cron", "cron", "not a cron", "Cron expression is invalid"},
		{"cron wrong field count", "cron", "* * *", "Cron expression is invalid"},
		{"negative cooldown", "cooldown_minutes", "-1", "Cooldown must be"},
		{"non-numeric cooldown", "cooldown_minutes", "soon", "Cooldown must be"},
		{"negative max_concurrent", "max_concurrent", "-3", "Max concurrent must be"},
		{"conditions not JSON", "conditions", "{not json", "Conditions must be a valid JSON array"},
		{"conditions bad shape", "conditions", `{"kind":"bead_query"}`, "Conditions are invalid"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			f := seedArchetypeGardener(t, fsmDrainArchetype().spec)
			edits := baseConfigEdits()
			edits.Set(tc.field, tc.value)

			mux := mkServer(t, f)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, opsReq("POST", "/gardener/gd-archcap/config", edits))
			if w.Code != http.StatusOK {
				t.Fatalf("expected 200 (inline error), got %d body=%s", w.Code, w.Body.String())
			}
			body := w.Body.String()
			if !strings.Contains(body, `role="alert"`) {
				t.Errorf("expected an inline error alert, got body=%s", body)
			}
			if !strings.Contains(body, tc.wantSub) {
				t.Errorf("error should name the problem (%q); body=%s", tc.wantSub, body)
			}
			if _, persisted := f.updateFieldsLastVal["gardener"]; persisted {
				t.Errorf("an invalid save was persisted: %v", f.updateFieldsLastVal)
			}
		})
	}
}

// TestGardenerConfigSave_ProgressSpecRoundTrips confirms the progress textarea is
// validated through progress.ParseSpec and, when valid, persisted inside the
// gardener blob — the missing sibling of the stop-spec round-trip.
func TestGardenerConfigSave_ProgressSpecRoundTrips(t *testing.T) {
	f := seedArchetypeGardener(t, fsmDrainArchetype().spec)
	edits := baseConfigEdits()
	edits.Set("progress_json", mustCompactJSON(map[string]any{
		"events": []any{map[string]any{"kind": "child_closed", "parent": "gd-WiKagFODjX"}},
		"windows": []any{
			map[string]any{"name": "last_2", "scope": "last_n_runs", "n": float64(2)},
		},
	}))

	mux := mkServer(t, f)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, opsReq("POST", "/gardener/gd-archcap/config", edits))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), `role="alert"`) {
		t.Fatalf("a valid progress spec was rejected; body=%s", w.Body.String())
	}
	persisted := f.updateFieldsLastVal["gardener"].(string)
	var got map[string]any
	if err := json.Unmarshal([]byte(persisted), &got); err != nil {
		t.Fatalf("persisted spec not JSON: %v", err)
	}
	prog, ok := got["progress"].(map[string]any)
	if !ok {
		t.Fatalf("persisted progress spec missing/not an object: %#v", got["progress"])
	}
	if _, ok := prog["windows"]; !ok {
		t.Errorf("persisted progress spec lost its windows: %#v", prog)
	}
}

// TestGardenerConfigSave_RejectsBadProgressSpec proves a progress spec the engine
// would drop (bad window scope) reds the save with an attributed error.
func TestGardenerConfigSave_RejectsBadProgressSpec(t *testing.T) {
	f := seedArchetypeGardener(t, fsmDrainArchetype().spec)
	edits := baseConfigEdits()
	edits.Set("progress_json", `{"windows":[{"name":"w","scope":"since_the_dawn_of_time"}]}`)

	mux := mkServer(t, f)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, opsReq("POST", "/gardener/gd-archcap/config", edits))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (inline error), got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "Progress spec is invalid") {
		t.Errorf("error should be attributed to the progress spec; body=%s", body)
	}
	if _, persisted := f.updateFieldsLastVal["gardener"]; persisted {
		t.Errorf("an invalid progress spec was persisted: %v", f.updateFieldsLastVal)
	}
}

// TestGardenerConfigSave_DoesNotIntroduceEmptySchedulingFields locks the
// legacy-schedule guard: when a scheduling knob is left empty AND the bead does
// not already carry that field, the save must NOT add it to the patch (a PATCH
// that introduces an unknown field is 400-rejected on a bead whose type-schema
// does not declare it). An emptied knob that DID exist is still cleared.
func TestGardenerConfigSave_DoesNotIntroduceEmptySchedulingFields(t *testing.T) {
	f := seedArchetypeGardener(t, fsmDrainArchetype().spec)
	// The seeded bead carries a cron but no conditions/cooldown/max_concurrent.
	f.beads["gd-archcap"].Fields["cron"] = "*/30 * * * *"

	edits := baseConfigEdits()
	// Operator clears the cron (it exists → should be cleared to "") and leaves
	// the three never-set knobs blank (must NOT be introduced).
	edits.Set("cron", "")

	mux := mkServer(t, f)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, opsReq("POST", "/gardener/gd-archcap/config", edits))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	last := f.updateFieldsLastVal
	if got, ok := last["cron"]; !ok || got != "" {
		t.Errorf("existing cron should be cleared to \"\"; got %#v (present=%v)", got, ok)
	}
	for _, k := range []string{"conditions", "cooldown_minutes", "max_concurrent"} {
		if _, present := last[k]; present {
			t.Errorf("empty never-set knob %q should NOT be added to the patch; patch=%v", k, last)
		}
	}
}

// TestGardenerConfigViewFromBead_LoadsSchedulingKnobs confirms the edit form is
// SEEDED with the gardener's current top-level scheduling fields, so opening the
// form shows the live cadence/gate rather than blanks (which a save would then
// clear).
func TestGardenerConfigViewFromBead_LoadsSchedulingKnobs(t *testing.T) {
	f := seedArchetypeGardener(t, fsmDrainArchetype().spec)
	b := f.beads["gd-archcap"]
	b.Fields["cron"] = "*/15 * * * *"
	b.Fields["cooldown_minutes"] = "10"
	b.Fields["max_concurrent"] = "4"
	b.Fields["conditions"] = `[{"kind":"bead_query","parent":"gd-x","op":">","value":"0"}]`

	v := gardenerConfigViewFromBead(b)
	if v.Cron != "*/15 * * * *" {
		t.Errorf("Cron = %q, want %q", v.Cron, "*/15 * * * *")
	}
	if v.Cooldown != "10" {
		t.Errorf("Cooldown = %q, want %q", v.Cooldown, "10")
	}
	if v.MaxConcurrent != "4" {
		t.Errorf("MaxConcurrent = %q, want %q", v.MaxConcurrent, "4")
	}
	if !strings.Contains(v.Conditions, "bead_query") {
		t.Errorf("Conditions = %q, want the seeded gate", v.Conditions)
	}
	// Progress from the gardener blob is surfaced too (fsm-drain has one).
	if !strings.Contains(v.ProgressJSON, "child_closed") {
		t.Errorf("ProgressJSON = %q, want the gardener's progress spec", v.ProgressJSON)
	}
}
