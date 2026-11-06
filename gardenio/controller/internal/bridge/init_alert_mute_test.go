package bridge

import "testing"

// TestAlertMuteType_Registered guards the W3b alert_mute bead type registration
// (epic ka-0000006776, task ka-0000008199). The schema must keep matching
// kbeads' builtin type:alert_mute; existing alert_mute beads depend on these
// fields.
func TestAlertMuteType_Registered(t *testing.T) {
	cfg, ok := configs()["type:alert_mute"].(TypeConfig)
	if !ok {
		t.Fatal("configs() missing type:alert_mute entry")
	}
	registered := map[string]bool{}
	for _, f := range cfg.Fields {
		registered[f.Name] = true
	}
	for _, name := range []string{"mute_matchers", "mute_until", "mute_reason"} {
		if !registered[name] {
			t.Errorf("type:alert_mute schema missing field %q", name)
		}
	}
}
