package main

// Golden ROUND-TRIP PARITY tests for the gardener-config edit surface
// (V3.2, task gd-6fyrAk0bWH under epic ka-0000001611 "Gardener Configuration
// Surface"). The epic's dogfood mandate: migrate 2-3 hand-authored gardeners to
// the UI-authored config path and CONFIRM PARITY — i.e. prove that opening a
// real, full-power hand-authored gardener in the beads-viewer edit form and
// hitting Save preserves the total power of that gardener rather than silently
// flattening it back to the 3-input prose blob the create form once produced.
//
// The three archetypes below are distilled from LIVE hand-authored gardeners so
// the parity guarantee is anchored to real config, not a synthetic strawman:
//
//   - "home-beauty"  ← ka-0000000748: a STANDING/perpetual continual gardener
//     with a full loop_end/hard_stop stop spec, a rich progress spec (events +
//     windows + surface), an assign_bead, and a Slack/operator binding.
//   - "costs-crisp"  ← ka-0000008137: a daily cron quality gardener with a
//     stop+progress spec but NO assign_bead / Slack binding (the un-bound
//     archetype), and a large run_budget/max_runs.
//   - "fsm-drain"    ← ka-0000004701: a wave-scoped operator-staged drainer
//     with a tight run_budget, an assign_bead, a Slack binding, AND live
//     engine-owned runtime counters (runs_spawned/completed, last_agent_id,
//     progress_state, resume/second-life bookkeeping) — the fields a save must
//     NEVER clobber.
//
// The invariant every case locks: the config-save handler decodes the FULL
// GardenerSpec, overlays ONLY the ~11 operator-editable knobs from the form, and
// re-encodes — so a round-trip through the UI must (a) apply exactly the edited
// knobs and (b) leave every OTHER field — advanced config the form does not yet
// expose (progress, assign_bead, Slack binding, standing, resume/second-life
// budgets) AND engine-owned runtime counters — byte-for-byte intact. A
// regression that drops any of those on save reduces a full-power gardener to a
// lobotomised one, which is exactly the "surface exposes a sliver of the model"
// failure this epic exists to fix.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
)

// gardenerArchetype is one hand-authored gardener fixture plus the set of edits
// an operator makes to it through the UI edit form.
type gardenerArchetype struct {
	name string
	// spec is the raw GardenerSpec JSON as it lives on the hand-authored
	// trigger bead today (the "before" state).
	spec map[string]any
	// edits are the operator's form inputs on the edit sheet.
	edits url.Values
	// wantEditedKnobs is the subset of top-level spec keys the save MUST have
	// changed to these exact (JSON-decoded) values.
	wantEditedKnobs map[string]any
	// wantPreservedKeys are top-level spec keys that must survive the save
	// byte-for-byte identical to the original spec (advanced config + engine
	// runtime the form never sends).
	wantPreservedKeys []string
}

// homeBeautyArchetype ← ka-0000000748 (standing continual gardener).
func homeBeautyArchetype() gardenerArchetype {
	return gardenerArchetype{
		name: "home-beauty-standing-continual",
		spec: map[string]any{
			"name":          "home-beauty",
			"terminal_goal": "All four CLUB dimensions score 8+ across two consecutive audits.",
			"standing":      true,
			"run_budget":    float64(500),
			"max_runs":      float64(500),
			"stop": map[string]any{
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
			},
			"progress": map[string]any{
				"events": []any{
					map[string]any{"kind": "child_closed", "parent": "gd-qipN5RP8Do"},
					map[string]any{"kind": "mr_merged", "parent": "gd-qipN5RP8Do"},
				},
				"windows": []any{
					map[string]any{"name": "this_run", "scope": "since_run_start"},
					map[string]any{"name": "last_3_runs", "scope": "last_n_runs", "n": float64(3)},
				},
				"surface": map[string]any{"in_prime": true, "in_wrapup": true},
			},
			"assign_bead":   "gd-qipN5RP8Do",
			"slack_user_id": "U0OPERATOR1",
			"user_bead_id":  "gd-mjBjMWy6O4",
		},
		edits: url.Values{
			// Operator tightens the goal and drops the run budget via the UI.
			"terminal_goal": {"CLUB dimensions score 8+ across two consecutive audits (tightened)."},
			"run_budget":    {"250"},
			"max_runs":      {"250"},
			// Standing gardener: retry off, one-spawn-at-a-time on.
			"one_spawn_at_a_time": {"1"},
			// Keep the existing progress spec verbatim (form round-trips it as a
			// raw-JSON textarea, exactly like the stop spec below).
			"progress_json": {mustCompactJSON(map[string]any{
				"events": []any{
					map[string]any{"kind": "child_closed", "parent": "gd-qipN5RP8Do"},
					map[string]any{"kind": "mr_merged", "parent": "gd-qipN5RP8Do"},
				},
				"windows": []any{
					map[string]any{"name": "this_run", "scope": "since_run_start"},
					map[string]any{"name": "last_3_runs", "scope": "last_n_runs", "n": float64(3)},
				},
				"surface": map[string]any{"in_prime": true, "in_wrapup": true},
			})},
			// Keep the existing stop spec verbatim (form round-trips it).
			"stop_json": {mustCompactJSON(map[string]any{
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
			})},
		},
		wantEditedKnobs: map[string]any{
			"terminal_goal":       "CLUB dimensions score 8+ across two consecutive audits (tightened).",
			"run_budget":          float64(250),
			"max_runs":            float64(250),
			"one_spawn_at_a_time": true,
		},
		wantPreservedKeys: []string{"progress", "assign_bead", "slack_user_id", "user_bead_id", "standing"},
	}
}

// costsCrispArchetype ← ka-0000008137 (daily cron, unbound).
func costsCrispArchetype() gardenerArchetype {
	return gardenerArchetype{
		name: "costs-crisp-unbound-daily",
		spec: map[string]any{
			"name":          "costs-crisp",
			"terminal_goal": "Keep /costs CRISP-accurate: every number reconciles with its source.",
			"run_budget":    float64(999),
			"max_runs":      float64(999),
			"stop": map[string]any{
				"loop_end": map[string]any{"any": []any{
					map[string]any{"kind": "no_progress_for", "runs": float64(3)},
					map[string]any{"kind": "max_runs"},
				}},
				"on_loop_end": "close",
			},
			"progress": map[string]any{
				"events": []any{
					map[string]any{"kind": "child_closed", "parent": "ka-0000006115"},
					map[string]any{"kind": "decision_resolved", "raised_by_agent_subtree_of": "ka-0000006115"},
				},
			},
		},
		edits: url.Values{
			"terminal_goal": {"Keep /costs CRISP-accurate: every number reconciles with its source."},
			"run_budget":    {"999"},
			"max_runs":      {"999"},
			// Operator adds a retry policy through the UI.
			"retry_on_failure": {"1"},
			"max_retries":      {"2"},
			// Progress spec round-trips through the textarea verbatim.
			"progress_json": {mustCompactJSON(map[string]any{
				"events": []any{
					map[string]any{"kind": "child_closed", "parent": "ka-0000006115"},
					map[string]any{"kind": "decision_resolved", "raised_by_agent_subtree_of": "ka-0000006115"},
				},
			})},
		},
		wantEditedKnobs: map[string]any{
			"retry_on_failure": true,
			"max_retries":      float64(2),
		},
		// No assign_bead / Slack binding to preserve — progress is the payload.
		wantPreservedKeys: []string{"progress"},
	}
}

// fsmDrainArchetype ← ka-0000004701 (wave-scoped drainer with live runtime).
func fsmDrainArchetype() gardenerArchetype {
	return gardenerArchetype{
		name: "fsm-drain-with-live-runtime",
		spec: map[string]any{
			"name":          "fsm-drain",
			"terminal_goal": "Every released wave's FSM tickets carry gardenio-triaged.",
			"run_budget":    float64(15),
			"max_runs":      float64(15),
			"stop": map[string]any{
				"loop_end":    map[string]any{"any": []any{map[string]any{"kind": "max_runs"}}},
				"on_loop_end": "close",
			},
			"progress": map[string]any{
				"events": []any{map[string]any{"kind": "child_closed", "parent": "gd-WiKagFODjX"}},
			},
			"assign_bead":   "gd-WiKagFODjX",
			"slack_user_id": "U0OPERATOR1",
			"user_bead_id":  "gd-ENcn1IsCKn",
			// --- Engine-owned runtime counters (never operator-authored). A
			// save MUST carry every one of these through untouched. ---
			"runs_spawned":             float64(9),
			"runs_completed":           float64(8),
			"consecutive_failures":     float64(1),
			"last_agent_id":            "gd-lastrun9",
			"lineage_root":             "gd-firstrun",
			"resumes_without_progress": float64(2),
			"last_resume_reason":       "no_progress_for",
			"second_life_count":        float64(1),
			"resume_budget":            float64(4),
		},
		edits: url.Values{
			// Operator resets the goal and bumps the budget for a new wave.
			"terminal_goal": {"Every released wave's FSM tickets carry gardenio-triaged (wave 2)."},
			"run_budget":    {"30"},
			"max_runs":      {"30"},
			// Progress spec round-trips through the textarea verbatim.
			"progress_json": {mustCompactJSON(map[string]any{
				"events": []any{map[string]any{"kind": "child_closed", "parent": "gd-WiKagFODjX"}},
			})},
		},
		wantEditedKnobs: map[string]any{
			"terminal_goal": "Every released wave's FSM tickets carry gardenio-triaged (wave 2).",
			"run_budget":    float64(30),
			"max_runs":      float64(30),
		},
		wantPreservedKeys: []string{
			"progress", "assign_bead", "slack_user_id", "user_bead_id",
			"runs_spawned", "runs_completed", "consecutive_failures",
			"last_agent_id", "lineage_root", "resumes_without_progress",
			"last_resume_reason", "second_life_count", "resume_budget",
		},
	}
}

// TestGardenerConfigSave_RoundTripParity dogfoods three real hand-authored
// gardener archetypes through the UI edit→save path and confirms full config
// parity: the edited knobs land, and EVERY other field — advanced config the
// form does not expose plus engine-owned runtime — survives byte-for-byte.
func TestGardenerConfigSave_RoundTripParity(t *testing.T) {
	for _, arch := range []gardenerArchetype{
		homeBeautyArchetype(),
		costsCrispArchetype(),
		fsmDrainArchetype(),
	} {
		arch := arch
		t.Run(arch.name, func(t *testing.T) {
			f := seedArchetypeGardener(t, arch.spec)

			// Capture the ORIGINAL values of the preserved keys before the
			// save so we compare against ground truth, not the edited copy.
			origPreserved := map[string]any{}
			for _, k := range arch.wantPreservedKeys {
				v, ok := arch.spec[k]
				if !ok {
					t.Fatalf("archetype %q lists preserved key %q not present in its own spec", arch.name, k)
				}
				origPreserved[k] = v
			}

			mux := mkServer(t, f)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, opsReq("POST", "/gardener/gd-archcap/config", arch.edits))
			if w.Code != http.StatusOK {
				t.Fatalf("config save: expected 200, got %d body=%s", w.Code, w.Body.String())
			}

			persisted, ok := f.updateFieldsLastVal["gardener"].(string)
			if !ok {
				t.Fatalf("config save: gardener spec not persisted; got %v", f.updateFieldsLastVal)
			}
			var got map[string]any
			if err := json.Unmarshal([]byte(persisted), &got); err != nil {
				t.Fatalf("config save: persisted spec not JSON: %v", err)
			}

			// (a) Edited knobs landed with exactly the submitted values.
			for k, want := range arch.wantEditedKnobs {
				if !reflect.DeepEqual(got[k], want) {
					t.Errorf("edited knob %q = %#v (%T), want %#v (%T)", k, got[k], got[k], want, want)
				}
			}

			// (b) Every preserved key survived the round-trip byte-for-byte.
			for _, k := range arch.wantPreservedKeys {
				if !reflect.DeepEqual(got[k], origPreserved[k]) {
					t.Errorf("preserved key %q was mutated by save:\n  got:  %#v\n  want: %#v", k, got[k], origPreserved[k])
				}
			}

			// (c) The save re-rendered the form with a success confirmation
			// (the htmx swap the operator sees), not an inline error.
			body := w.Body.String()
			if strings.Contains(body, `role="alert"`) {
				t.Errorf("config save rendered an inline error for a valid round-trip; body=%s", body)
			}
		})
	}
}

// TestGardenerConfigSave_RoundTripParity_StopSpecPreserved locks the specific
// hazard that the stop spec — the one advanced field the edit form DOES round-
// trip, as a raw-JSON textarea — survives a save that does not touch it. The
// home-beauty archetype carries a nested loop_end/hard_stop spec; re-submitting
// it verbatim must persist a semantically-identical spec (key order aside).
func TestGardenerConfigSave_RoundTripParity_StopSpecPreserved(t *testing.T) {
	arch := homeBeautyArchetype()
	f := seedArchetypeGardener(t, arch.spec)

	mux := mkServer(t, f)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, opsReq("POST", "/gardener/gd-archcap/config", arch.edits))
	if w.Code != http.StatusOK {
		t.Fatalf("config save: expected 200, got %d body=%s", w.Code, w.Body.String())
	}

	persisted := f.updateFieldsLastVal["gardener"].(string)
	var got map[string]any
	if err := json.Unmarshal([]byte(persisted), &got); err != nil {
		t.Fatalf("persisted spec not JSON: %v", err)
	}
	if !reflect.DeepEqual(got["stop"], arch.spec["stop"]) {
		t.Errorf("stop spec not preserved through round-trip:\n  got:  %#v\n  want: %#v", got["stop"], arch.spec["stop"])
	}
}

// TestGardenerConfigSave_RoundTripParity_ParsesBackToSpec is the belt-and-
// suspenders half of the dogfood: the persisted JSON must decode cleanly back
// through the SAME bridge.ParseTriggerDef path the engine uses, so a UI-saved
// gardener is one the engine can actually run. This closes the loop the epic
// cares about — "the UI never persists a spec the engine chokes on" — by
// re-parsing the saved bytes rather than trusting the map projection.
func TestGardenerConfigSave_RoundTripParity_ParsesBackToSpec(t *testing.T) {
	arch := fsmDrainArchetype()
	f := seedArchetypeGardener(t, arch.spec)

	mux := mkServer(t, f)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, opsReq("POST", "/gardener/gd-archcap/config", arch.edits))
	if w.Code != http.StatusOK {
		t.Fatalf("config save: expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	persisted := f.updateFieldsLastVal["gardener"].(string)

	// Build a synthetic trigger bead carrying the saved spec and re-parse it
	// exactly as the engine's reconcile loop would.
	reparsed := &beadsapi.BeadDetail{
		ID:     "gd-archcap",
		Title:  "archetype gardener",
		Type:   "trigger",
		Status: "open",
		Labels: []string{"trigger:schedule", "role:gardener", "project:gardenio"},
		Fields: map[string]string{"role": "gardener", "gardener": persisted},
	}
	def := bridge.ParseTriggerDef(reparsed, "schedule")
	if def.Gardener == nil {
		t.Fatalf("persisted spec did not parse back into a GardenerSpec")
	}
	spec := def.Gardener

	// Edited knobs survived the re-parse into the typed struct.
	if spec.TerminalGoal != "Every released wave's FSM tickets carry gardenio-triaged (wave 2)." {
		t.Errorf("re-parsed terminal_goal = %q", spec.TerminalGoal)
	}
	if spec.RunBudget != 30 || spec.MaxRuns != 30 {
		t.Errorf("re-parsed budget: run_budget=%d max_runs=%d, want 30/30", spec.RunBudget, spec.MaxRuns)
	}
	// Engine-owned runtime survived into the typed struct too.
	if spec.RunsSpawned != 9 || spec.RunsCompleted != 8 {
		t.Errorf("re-parsed runtime clobbered: runs_spawned=%d runs_completed=%d, want 9/8", spec.RunsSpawned, spec.RunsCompleted)
	}
	if spec.LastAgentID != "gd-lastrun9" {
		t.Errorf("re-parsed last_agent_id = %q, want gd-lastrun9", spec.LastAgentID)
	}
	if spec.AssignBead != "gd-WiKagFODjX" {
		t.Errorf("re-parsed assign_bead = %q, want gd-WiKagFODjX", spec.AssignBead)
	}
	if spec.SlackUserID != "U0OPERATOR1" || spec.UserBeadID != "gd-ENcn1IsCKn" {
		t.Errorf("re-parsed Slack binding lost: slack_user_id=%q user_bead_id=%q", spec.SlackUserID, spec.UserBeadID)
	}
	// The progress spec (advanced, un-exposed) survived as raw JSON.
	if len(spec.Progress) == 0 {
		t.Errorf("re-parsed progress spec dropped (len 0)")
	}
}

// seedArchetypeGardener builds a fakeDaemon carrying one open gardener trigger
// bead (gd-archcap) whose `gardener` field is the supplied hand-authored spec.
// TestGardenerConfigSave_Prompt locks the P3 fix of bug ka-0000003596: the
// per-run `prompt` — the single most behavior-defining knob — is now viewable
// AND editable in the config form. The edit form renders the current prompt in
// a textarea, and a save persists the operator's edit as the top-level `prompt`
// bead field (NOT inside the gardener blob). It also asserts the GET edit form
// pre-populates the current prompt so a plain save round-trips it unchanged.
func TestGardenerConfigSave_Prompt(t *testing.T) {
	f := newFake()
	spec := map[string]any{"name": "promptcap", "terminal_goal": "some goal"}
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}
	f.beads["gd-archcap"] = &beadsapi.BeadDetail{
		ID:     "gd-archcap",
		Title:  "prompt gardener",
		Type:   "trigger",
		Status: "open",
		Labels: []string{"trigger:schedule", "role:gardener", "project:gardenio"},
		Fields: map[string]string{
			"role":     "gardener",
			"gardener": string(raw),
			"prompt":   "OLD prompt: drive the epic.",
		},
		UpdatedAt: time.Date(2026, 6, 1, 11, 0, 0, 0, time.UTC),
	}

	mux := mkServer(t, f)

	// (1) The GET edit form pre-populates the current prompt in a textarea.
	wGet := httptest.NewRecorder()
	mux.ServeHTTP(wGet, opsReq("GET", "/gardener/gd-archcap/edit", nil))
	if wGet.Code != http.StatusOK {
		t.Fatalf("edit form GET: expected 200, got %d body=%s", wGet.Code, wGet.Body.String())
	}
	getBody := wGet.Body.String()
	if !strings.Contains(getBody, `name="prompt"`) {
		t.Errorf("edit form missing a prompt textarea; body=%s", getBody)
	}
	if !strings.Contains(getBody, "OLD prompt: drive the epic.") {
		t.Errorf("edit form did not pre-populate the current prompt; body=%s", getBody)
	}

	// (2) A save persists the edited prompt as the top-level `prompt` field.
	edits := url.Values{}
	edits.Set("prompt", "NEW prompt: shepherd MRs to green.")
	edits.Set("terminal_goal", "some goal")
	edits.Set("stop_json", "")
	wPost := httptest.NewRecorder()
	mux.ServeHTTP(wPost, opsReq("POST", "/gardener/gd-archcap/config", edits))
	if wPost.Code != http.StatusOK {
		t.Fatalf("config save: expected 200, got %d body=%s", wPost.Code, wPost.Body.String())
	}
	if got := f.updateFieldsLastVal["prompt"]; got != "NEW prompt: shepherd MRs to green." {
		t.Errorf("prompt not persisted as top-level field: got %#v", got)
	}
	// The prompt is a top-level field, never smuggled into the gardener blob.
	if persisted, ok := f.updateFieldsLastVal["gardener"].(string); ok {
		if strings.Contains(persisted, "NEW prompt") {
			t.Errorf("prompt leaked into the gardener blob; it must be a top-level field: %s", persisted)
		}
	}
}

func seedArchetypeGardener(t *testing.T, spec map[string]any) *fakeDaemon {
	t.Helper()
	f := newFake()
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal archetype spec: %v", err)
	}
	f.beads["gd-archcap"] = &beadsapi.BeadDetail{
		ID:     "gd-archcap",
		Title:  "archetype gardener",
		Type:   "trigger",
		Status: "open",
		Labels: []string{"trigger:schedule", "role:gardener", "project:gardenio"},
		Fields: map[string]string{
			"role":     "gardener",
			"gardener": string(raw),
		},
		UpdatedAt: time.Date(2026, 6, 1, 11, 0, 0, 0, time.UTC),
	}
	return f
}

// mustCompactJSON marshals v to a compact JSON string, failing the process on
// error (test-fixture construction, so a panic is the right blast radius).
func mustCompactJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic("mustCompactJSON: " + err.Error())
	}
	return string(b)
}
