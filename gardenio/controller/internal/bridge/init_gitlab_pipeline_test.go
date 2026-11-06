package bridge

import "testing"

// TestTriggerSchema_HasPipelineRefPattern guards the pipeline_ref_pattern field
// (gd-zndckdlsyb) on the controller-side type:trigger schema. The GitLab
// pipeline trigger source reads pipeline_ref_pattern to suppress non-train ref
// firings; if this field is not registered here, kbeads 400s the whole PATCH
// on any trigger bead that sets it — the same closed-schema "unknown field"
// failure family documented on gardener_evaluator_test.go. The kbeads builtin
// (internal/server/config.go) must list it too; both are required because a
// stored type:trigger config row on prod shadows the builtin (advice
// ka-0000000061), and only the controller-side EnsureConfigs upsert reliably
// rolls the new field forward.
func TestTriggerSchema_HasPipelineRefPattern(t *testing.T) {
	cfg, ok := configs()["type:trigger"].(TypeConfig)
	if !ok {
		t.Fatal("configs() missing type:trigger entry")
	}
	registered := map[string]bool{}
	for _, f := range cfg.Fields {
		registered[f.Name] = true
	}
	// pipeline_max_age is guarded alongside pipeline_ref_pattern: both are
	// pipeline-source fields the kbeads builtin already lists, and the
	// controller schema must not drift from it.
	for _, name := range []string{"pipeline_ref", "pipeline_ref_pattern", "pipeline_status", "pipeline_max_age", "min_duration", "ignore_failed_statuses"} {
		if !registered[name] {
			t.Errorf("type:trigger schema missing pipeline field %q", name)
		}
	}
}
