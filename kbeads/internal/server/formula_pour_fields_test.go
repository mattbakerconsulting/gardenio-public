package server

import (
	"encoding/json"
	"testing"

	"gardenio/kbeads/internal/model"
)

// TestPourPayloadValidatesEndToEnd runs the EXACT field payloads that the
// three step-machinery writers build through the REAL field validator, for
// every step-bead type.
//
// TestBuiltinFormulaStepFieldsDeclared (builtin_types_test.go) asserts the
// field NAMES are declared; this asserts the values the writers actually send
// are ACCEPTED. That distinction is the whole bug: ka-0000002741 /
// ka-0000001148 shipped writers plus client-side tests that only checked the
// map the CLI/bridge built, never round-tripping it through validation — so CI
// stayed green while every real pour 400'd at its first step, and no fix-and-mr
// / fix-proof / publish-fix molecule could be poured at all.
//
// Reverting any declaration in config.go makes the relevant case fail with the
// verbatim production error, which a name-only assertion cannot reproduce. It
// also pins the TYPE: suggest_new_agent is a real Go bool, so declaring it
// "string" would pass the unknown-field check and then fail "must be a boolean"
// — the 400 would just move.
//
// The three writers, each modeled by a payload below:
//   - cmd/kd/formula_apply.go — CLI pour (suggest_new_agent + step_role).
//   - bridge/formula_instantiate.go — bridge pour (Slack /formula pour + the
//     apply_formula trigger); stamps molecule_id UNCONDITIONALLY plus
//     step_project, so registering only the CLI's two fields still 400s every
//     bridge pour on step 1.
//   - bridge/formula_step_dispatch.go — the dispatcher's pre-spawn
//     UpdateBeadFields stamp (dispatched_agent + dispatched_by); a 400 here
//     makes the dispatcher decline-and-return forever, never spawning.
func TestPourPayloadValidatesEndToEnd(t *testing.T) {
	// A step defaults to type=task; a decision-gate step becomes type=decision
	// (formula_apply.go:429) BEFORE the SuggestNewAgent writer runs at :450, so
	// an opted-in gated step lands on that schema and both must accept the
	// payload. type:decision additionally requires prompt, which the gate path
	// supplies alongside these fields.
	//
	// Each type is checked against several payloads: the CLI pour, the fuller
	// bridge pour (which adds molecule_id/step_project), and the dispatcher's
	// stamp. Every one must be accepted or the corresponding live write 400s.
	type payload struct {
		writer string
		raw    string
	}
	cases := map[string][]payload{
		"type:task": {
			{"cli pour (formula_apply.go)", `{"suggest_new_agent":true,"step_role":"monorepo-fix","source_formula":"gd-fhHZ046edw","source_step_id":"verify-inputs"}`},
			{"bridge pour (formula_instantiate.go)", `{"suggest_new_agent":true,"step_role":"monorepo-fix","molecule_id":"gd-MOL","step_project":"monorepo","source_formula":"gd-fhHZ046edw","source_step_id":"verify-inputs"}`},
			// The dispatcher stamp is a PATCH (UpdateBeadFields) that merges
			// dispatched_agent/dispatched_by onto the already-stored bridge-pour
			// fields; the validator re-checks the whole merged blob, so model
			// the merged result, not the two stamp keys in isolation.
			{"dispatcher stamp (formula_step_dispatch.go)", `{"suggest_new_agent":true,"step_role":"monorepo-fix","molecule_id":"gd-MOL","step_project":"monorepo","source_formula":"gd-fhHZ046edw","source_step_id":"verify-inputs","dispatched_agent":"gardenio-foo","dispatched_by":"formula-step-dispatcher"}`},
			// A manual/sync gate step stamps gate_type (+ timeout) but does NOT
			// flip typ to "decision" (only a decision gate does), so it lands on
			// type:task. Its absence 400'd "gate_type: unknown field" and aborted
			// the pour mid-molecule, dropping precisely the gate step
			// (ka-0000006567). The PE-ticket-to-completion `gardener-signoff` step
			// is exactly this: {"type":"manual","timeout":"72h"}.
			{"manual gate step (both pour paths)", `{"gate_type":"manual","timeout":"72h","molecule_id":"gd-MOL","source_formula":"ka-0000007107","source_step_id":"gardener-signoff"}`},
			{"sync gate step (both pour paths)", `{"gate_type":"sync","molecule_id":"gd-MOL","source_formula":"gd-fhHZ046edw","source_step_id":"fanout-join"}`},
		},
		"type:decision": {
			{"cli pour (formula_apply.go)", `{"suggest_new_agent":true,"step_role":"monorepo-fix","prompt":"gate?","source_formula":"gd-fhHZ046edw"}`},
			{"bridge pour (formula_instantiate.go)", `{"suggest_new_agent":true,"step_role":"monorepo-fix","molecule_id":"gd-MOL","step_project":"monorepo","prompt":"gate?","source_formula":"gd-fhHZ046edw"}`},
			// A decision step requires prompt; the merged blob still carries it
			// (patch merge preserves stored fields), so the stamp must validate
			// against the full blob including prompt.
			{"dispatcher stamp (formula_step_dispatch.go)", `{"suggest_new_agent":true,"step_role":"monorepo-fix","molecule_id":"gd-MOL","step_project":"monorepo","prompt":"gate?","source_formula":"gd-fhHZ046edw","dispatched_agent":"gardenio-foo","dispatched_by":"formula-step-dispatcher"}`},
		},
	}
	for typeName, payloads := range cases {
		c, ok := builtinConfigs[typeName]
		if !ok {
			t.Fatalf("builtinConfigs missing %q", typeName)
		}
		var tc model.TypeConfig
		if err := json.Unmarshal(c.Value, &tc); err != nil {
			t.Fatalf("%s: unmarshal config value: %v", typeName, err)
		}
		for _, p := range payloads {
			if err := model.ValidateTypeFields(json.RawMessage(p.raw), &tc); err != nil {
				t.Errorf("%s: %s payload rejected, so that write 400s at this step: %v", typeName, p.writer, err)
			}
		}
	}
}
