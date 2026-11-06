package server

import (
	"encoding/json"
	"maps"
	"testing"

	"gardenio/kbeads/internal/model"
)

// TestTypeTriggerAdmitsGardenerBoundsFields is the bounded-stop schema gate
// (ka-0000006744): a gardener trigger bead carrying the bounded-stop fields
// — run_budget (integer), max_runs (integer), terminal_goal (string), and a
// `stop` blob whose hard_stop slot holds a wall_clock — must validate cleanly
// against the built-in type:trigger schema. If any field were missing from the
// schema, ValidateTypeFields would reject it as an "unknown field" (the
// dead-on-arrival 400 captured in the gardener-stop-conditions review), and the
// guardrail gardeners could never be written.
//
// type:schedule is asserted alongside type:trigger because the schedule-mirror
// copies the gardener loop spec between the two gardener-carrying types — both
// must admit the fields or the mirror 400s on write.
func TestTypeTriggerAdmitsGardenerBoundsFields(t *testing.T) {
	srv, _, ctx := testCtx(t)

	stopBlob := `{"loop_end":{"any":[{"kind":"max_runs"}]},` +
		`"hard_stop":{"any":[{"kind":"wall_clock","duration":"168h"},` +
		`{"kind":"consecutive_failures","value":"5"}]},"on_loop_end":"close"}`

	// The load-bearing bounds fields, common to both gardener-carrying types.
	// gardener_spend_usd / gardener_spend_synced_at are the per-gardener agent
	// spend roll-up the controller stamps (gardenerSpendSync, feat 8a3bc9504);
	// they were shipped without a matching schema field, so the roll-up 400'd
	// on every write in prod until declared here (P4.4, 2026-07-05). Kept in
	// this gate so a future drop is caught by CI rather than in production.
	common := map[string]any{
		"role":                     "gardener",
		"project":                  "gardenio",
		"prompt":                   "gardener",
		"enabled":                  true,
		"run_budget":               20,
		"max_runs":                 20,
		"terminal_goal":            "All children closed.",
		"stop":                     stopBlob,
		"progress":                 `{"surface":{"in_wrapup":true}}`,
		"gardener_spend_usd":       "1.23",
		"gardener_spend_synced_at": "2026-07-05T16:00:00Z",
	}

	for _, typeName := range []model.BeadType{"trigger", "schedule"} {
		fieldsMap := map[string]any{}
		maps.Copy(fieldsMap, common)
		// action is a type:trigger-only field (schedules fire on cron, not an
		// explicit action). Only set it on the trigger case.
		if typeName == "trigger" {
			fieldsMap["action"] = "spawn_agent"
		}
		fields, err := json.Marshal(fieldsMap)
		if err != nil {
			t.Fatalf("marshal fields: %v", err)
		}

		tc, err := srv.resolveTypeConfig(ctx, typeName)
		if err != nil {
			t.Fatalf("resolveTypeConfig(%q): %v", typeName, err)
		}
		if tc == nil {
			t.Fatalf("type:%s config missing from builtins", typeName)
		}
		if err := model.ValidateTypeFields(fields, tc); err != nil {
			t.Errorf("type:%s rejected gardener bounds fields (would 400 on create): %v", typeName, err)
		}
	}
}

// TestTypeAdmitsSecondLifeFields is the second-life schema gate (bead
// ka-0000004010): both gardener-carrying types must admit max_second_lives and
// second_life_count (integer) or the second-life grant path 400s when it
// reopens a gardener and persists the bumped second_life_count. Mirrors the
// bounds-fields gate above; second-life is the coarse post-closure analogue of
// the resume budget those bounds fields drive.
func TestTypeAdmitsSecondLifeFields(t *testing.T) {
	srv, _, ctx := testCtx(t)

	common := map[string]any{
		"role":              "gardener",
		"project":           "gardenio",
		"prompt":            "gardener",
		"enabled":           true,
		"max_second_lives":  5,
		"second_life_count": 2,
		// The second-life GRANT path (gardenerGrantSecondLife) also clears the
		// terminate markers via the field-validating PATCH endpoint, so these
		// must be admitted on BOTH gardener-carrying types or the revive 400s
		// after the bead has already been reopened (inconsistent state). The
		// escalate path's gardener_stuck_escalated marker was previously only
		// writable via the validation-bypassing /close endpoint; registering it
		// here closes that latent gap.
		"gardener_terminated_slot": "",
		"gardener_terminated_kind": "",
		"gardener_stuck_escalated": "",
	}

	for _, typeName := range []model.BeadType{"trigger", "schedule"} {
		fieldsMap := map[string]any{}
		maps.Copy(fieldsMap, common)
		if typeName == "trigger" {
			fieldsMap["action"] = "spawn_agent"
		}
		fields, err := json.Marshal(fieldsMap)
		if err != nil {
			t.Fatalf("marshal fields: %v", err)
		}

		tc, err := srv.resolveTypeConfig(ctx, typeName)
		if err != nil {
			t.Fatalf("resolveTypeConfig(%q): %v", typeName, err)
		}
		if tc == nil {
			t.Fatalf("type:%s config missing from builtins", typeName)
		}
		if err := model.ValidateTypeFields(fields, tc); err != nil {
			t.Errorf("type:%s rejected second-life fields (would 400 on grant): %v", typeName, err)
		}

		// Both fields must be declared integer so the cumulative cap arithmetic
		// (second_life_count >= max_second_lives) persists as numbers.
		want := map[string]string{"max_second_lives": "integer", "second_life_count": "integer"}
		got := map[string]string{}
		for _, f := range tc.Fields {
			if _, ok := want[f.Name]; ok {
				got[f.Name] = string(f.Type)
			}
		}
		for name, wantType := range want {
			if got[name] != wantType {
				t.Errorf("type:%s field %q type=%q, want %q", typeName, name, got[name], wantType)
			}
		}
	}
}

// TestTypeTriggerBoundsFieldTypes pins the declared TYPES of the load-bearing
// bounds fields: run_budget/max_runs must be integer (so a positive-int
// semantic check is meaningful) and stop/progress must be json (so a structured
// blob persists rather than being string-coerced).
func TestTypeTriggerBoundsFieldTypes(t *testing.T) {
	srv, _, ctx := testCtx(t)

	want := map[string]string{
		"run_budget":    "integer",
		"max_runs":      "integer",
		"terminal_goal": "string",
		"stop":          "json",
		"progress":      "json",
	}

	for _, typeName := range []model.BeadType{"trigger", "schedule"} {
		tc, err := srv.resolveTypeConfig(ctx, typeName)
		if err != nil {
			t.Fatalf("resolveTypeConfig(%q): %v", typeName, err)
		}
		got := map[string]string{}
		for _, f := range tc.Fields {
			if _, ok := want[f.Name]; ok {
				got[f.Name] = string(f.Type)
			}
		}
		for name, wantType := range want {
			if got[name] != wantType {
				t.Errorf("type:%s field %q type=%q, want %q", typeName, name, got[name], wantType)
			}
		}
	}
}

// TestTypeAdmitsFormulaStepFields is the formula-pour schema gate
// (ka-0000002559): the pour paths (server http_formula_pour.go and CLI
// cmd/kd/formula_apply.go) stamp a stepFields map onto each step bead that
// includes source_formula/source_step_id plus — for steps that declare them —
// waits_for and a nested on_complete blob; cmd/kd/formula_expand.go stamps
// expansion_var on each expanded step; and the output-threading convention has
// a host step write a dwf_result blob. A step defaults to type=task, and a
// decision-gate step becomes type=decision, so BOTH schemas must admit these
// fields or the pour 400s mid-molecule ("on_complete: unknown field") and every
// deferred expand 400s the same way — the dead-on-arrival break this bug fixed.
// Mirrors the gardener-bounds gate above; kept in CI so a future drop is caught
// here rather than on the live daemon.
func TestTypeAdmitsFormulaStepFields(t *testing.T) {
	srv, _, ctx := testCtx(t)

	// The exact stepFields the pour/expand paths marshal onto a step bead.
	// on_complete is a nested map (json); dwf_result is the host-written output
	// blob (json); waits_for and expansion_var are scalars (string).
	common := map[string]any{
		"source_formula": "gd-someFormula",
		"source_step_id": "drive-reproduction",
		"waits_for":      "all-children",
		"on_complete": map[string]any{
			"for_each":    "{{scan.output.components}}",
			"var":         "component",
			"expand":      "gd-componentProcess",
			"expand_vars": map[string]any{"name": "{{component}}"},
		},
		"expansion_var": "FSM-403",
		"dwf_result":    map[string]any{"schema": "dwf.result.v1", "succeeded": []string{"a", "b"}},
	}

	for _, typeName := range []model.BeadType{"task", "decision"} {
		fieldsMap := map[string]any{}
		maps.Copy(fieldsMap, common)
		// A decision-gate step also carries the gate prompt (required on
		// type:decision). Set it so the case exercises a realistic gate step.
		if typeName == "decision" {
			fieldsMap["prompt"] = "Proceed?"
		}
		fields, err := json.Marshal(fieldsMap)
		if err != nil {
			t.Fatalf("marshal fields: %v", err)
		}

		tc, err := srv.resolveTypeConfig(ctx, typeName)
		if err != nil {
			t.Fatalf("resolveTypeConfig(%q): %v", typeName, err)
		}
		if tc == nil {
			t.Fatalf("type:%s config missing from builtins", typeName)
		}
		if err := model.ValidateTypeFields(fields, tc); err != nil {
			t.Errorf("type:%s rejected formula step fields (would 400 the pour mid-molecule): %v", typeName, err)
		}
	}
}
