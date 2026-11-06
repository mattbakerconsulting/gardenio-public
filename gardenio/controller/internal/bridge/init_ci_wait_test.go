package bridge

import (
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// TestEnsureConfigs_AgentDeclaresCIWaitFields pins every ci_wait_* key
// (`gb wait ci`, plan wake-on-ci-completion.md §2.1, task gd-AwuRDezONo) on the
// bridge's type:agent copy as a plain string. The kbeads builtin carries the same
// keys (TestBuiltinAgent_DeclaresCIWaitFields there), and the kbeads subset guard
// (TestBridgeTypeConfig_SubsetOfBuiltin) fails if this copy ever gets ahead of it.
func TestEnsureConfigs_AgentDeclaresCIWaitFields(t *testing.T) {
	cfg, ok := configs()["type:agent"].(TypeConfig)
	if !ok {
		t.Fatal("configs() missing type:agent entry")
	}
	byName := make(map[string]FieldDef, len(cfg.Fields))
	for _, f := range cfg.Fields {
		byName[f.Name] = f
	}
	for _, name := range append(append([]string(nil), beadsapi.CIWaitFields...), beadsapi.FieldCIWaitLedger) {
		f, ok := byName[name]
		if !ok {
			t.Errorf("type:agent omits %q — `gb wait ci`, the stop gate and the resolver write it", name)
			continue
		}
		if f.Type != "string" {
			t.Errorf("type:agent %q has type %q, want string", name, f.Type)
		}
	}
}
