package bridge

import "testing"

// TestAlertRuleType_Registered guards the W3a alert_rule bead type registration
// (epic ka-0000006776). The schema must keep matching kbeads' builtin
// type:alert_rule; existing alert_rule beads depend on these fields.
func TestAlertRuleType_Registered(t *testing.T) {
	cfg, ok := configs()["type:alert_rule"].(TypeConfig)
	if !ok {
		t.Fatal("configs() missing type:alert_rule entry")
	}
	registered := map[string]bool{}
	for _, f := range cfg.Fields {
		registered[f.Name] = true
	}
	for _, name := range []string{
		"rule_kind",
		"rule_expr",
		"rule_for",
		"rule_severity",
		"rule_group",
		"rule_target",
		"rule_labels",
		"rule_annotations",
	} {
		if !registered[name] {
			t.Errorf("type:alert_rule schema missing field %q", name)
		}
	}
}

// TestTriggerSchema_HasAlertFields guards the alert-source fields added to
// type:trigger for W3a alerts-in. WebhookTriggerSource reads allow_unsigned and
// beads-viewer shows alert_route; the alert source value gates which source enum
// the trigger validates against. If kbeads rejects these, alert trigger beads
// fail to create.
func TestTriggerSchema_HasAlertFields(t *testing.T) {
	cfg, ok := configs()["type:trigger"].(TypeConfig)
	if !ok {
		t.Fatal("configs() missing type:trigger entry")
	}
	var sourceField *FieldDef
	registered := map[string]bool{}
	for i := range cfg.Fields {
		f := cfg.Fields[i]
		registered[f.Name] = true
		if f.Name == "source" {
			sourceField = &cfg.Fields[i]
		}
	}
	for _, name := range []string{"alert_route", "allow_unsigned"} {
		if !registered[name] {
			t.Errorf("type:trigger schema missing alert field %q", name)
		}
	}
	if sourceField == nil {
		t.Fatal("type:trigger missing source field")
	}
	hasAlert := false
	for _, v := range sourceField.Values {
		if v == "alert" {
			hasAlert = true
		}
	}
	if !hasAlert {
		t.Errorf("type:trigger source enum missing %q value: %v", "alert", sourceField.Values)
	}
}
