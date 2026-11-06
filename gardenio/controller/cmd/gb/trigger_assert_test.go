package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildBeadFieldsWithExprs_CompilesAndStores(t *testing.T) {
	opts := commonTriggerOpts{action: "notify", cooldown: "5m", maxConcurrent: 1, enabled: true}
	fields, labels, err := buildBeadFieldsWithExprs(opts,
		[]string{"created"},
		[]string{"type:==:bug"},
		[]string{`bead.priority == 0`},
	)
	if err != nil {
		t.Fatalf("buildBeadFieldsWithExprs: %v", err)
	}
	condStr, _ := fields["conditions"].(string)
	var got []beadConditionJSON
	if err := json.Unmarshal([]byte(condStr), &got); err != nil {
		t.Fatalf("conditions JSON: %v (raw: %s)", err, condStr)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 conditions (1 field + 1 expr), got %d: %#v", len(got), got)
	}
	// Field condition first, expr condition second.
	if got[0].Field != "type" || got[0].Op != "==" || got[0].Value != "bug" {
		t.Errorf("field condition wrong: %#v", got[0])
	}
	if got[1].Expr != `bead.priority == 0` {
		t.Errorf("expr condition wrong: %#v", got[1])
	}
	if got[1].Field != "" || got[1].Op != "" {
		t.Errorf("expr condition should not carry field/op: %#v", got[1])
	}
	if !containsString(labels, "trigger:bead") {
		t.Errorf("labels missing trigger:bead: %v", labels)
	}
}

func TestBuildBeadFieldsWithExprs_RejectsMalformed(t *testing.T) {
	opts := commonTriggerOpts{action: "notify", cooldown: "5m", maxConcurrent: 1, enabled: true}
	cases := []string{
		`bead.status ==`,        // syntax error
		`bead.priority`,         // non-bool
		`nonexistent.field > 0`, // unknown variable
	}
	for _, expr := range cases {
		t.Run(expr, func(t *testing.T) {
			_, _, err := buildBeadFieldsWithExprs(opts, nil, nil, []string{expr})
			if err == nil {
				t.Errorf("expected compile-on-write rejection for %q", expr)
			}
			if err != nil && !strings.Contains(err.Error(), "invalid CEL expression") {
				t.Errorf("error should mention invalid CEL expression, got: %v", err)
			}
		})
	}
}

func TestBuildBeadFieldsWithExprs_EmptyExprSkipped(t *testing.T) {
	opts := commonTriggerOpts{action: "notify", cooldown: "5m", maxConcurrent: 1, enabled: true}
	// A whitespace-only expr is dropped; with no other conditions, the
	// conditions field is omitted entirely.
	fields, _, err := buildBeadFieldsWithExprs(opts, nil, nil, []string{"   "})
	if err != nil {
		t.Fatalf("buildBeadFieldsWithExprs: %v", err)
	}
	if _, present := fields["conditions"]; present {
		t.Errorf("expected conditions omitted when only blank exprs given, got %v", fields["conditions"])
	}
}

func TestBuildAssertViolationFields(t *testing.T) {
	opts := commonTriggerOpts{action: "assert", cooldown: "5m", maxConcurrent: 1, enabled: true}
	fields, labels, err := buildAssertViolationFields(opts,
		[]string{"updated"},
		[]string{"type:==:bug"},
		[]string{`bead.assignee != ""`},
		"notify",
	)
	if err != nil {
		t.Fatalf("buildAssertViolationFields: %v", err)
	}
	// action stamped as assert.
	if fields["action"] != "assert" {
		t.Errorf("action should be assert, got %v", fields["action"])
	}
	// on_violation persisted.
	if fields["on_violation"] != "notify" {
		t.Errorf("on_violation should be notify, got %v", fields["on_violation"])
	}
	// Invariant in the `assert` field, pre-filter in `conditions`.
	var asserts []beadConditionJSON
	if err := json.Unmarshal([]byte(fields["assert"].(string)), &asserts); err != nil {
		t.Fatalf("assert JSON: %v (raw %v)", err, fields["assert"])
	}
	if len(asserts) != 1 || asserts[0].Expr != `bead.assignee != ""` {
		t.Errorf("assert clauses wrong: %#v", asserts)
	}
	var conds []beadConditionJSON
	if err := json.Unmarshal([]byte(fields["conditions"].(string)), &conds); err != nil {
		t.Fatalf("conditions JSON: %v", err)
	}
	if len(conds) != 1 || conds[0].Field != "type" || conds[0].Op != "==" || conds[0].Value != "bug" {
		t.Errorf("pre-filter conditions wrong: %#v", conds)
	}
	if !containsString(labels, "trigger:bead") {
		t.Errorf("labels missing trigger:bead: %v", labels)
	}
}

func TestBuildAssertViolationFields_RejectsMalformed(t *testing.T) {
	opts := commonTriggerOpts{action: "assert", cooldown: "5m", maxConcurrent: 1, enabled: true}
	_, _, err := buildAssertViolationFields(opts, nil, nil, []string{`bead.status ==`}, "notify")
	if err == nil || !strings.Contains(err.Error(), "invalid assert CEL expression") {
		t.Errorf("expected compile-on-write rejection, got %v", err)
	}
	// Empty exprs rejected.
	if _, _, err := buildAssertViolationFields(opts, nil, nil, nil, "notify"); err == nil {
		t.Errorf("expected error for no --expr")
	}
}

func TestParseTriggerConditions(t *testing.T) {
	conds, err := parseTriggerConditions(`[{"field":"type","op":"==","value":"bug"},{"expr":"bead.priority == 0"}]`)
	if err != nil {
		t.Fatalf("parseTriggerConditions: %v", err)
	}
	if len(conds) != 2 {
		t.Fatalf("expected 2 conditions, got %d", len(conds))
	}
	if conds[1].Expr != "bead.priority == 0" {
		t.Errorf("expr condition not parsed: %#v", conds[1])
	}
	// Empty input yields no conditions, no error.
	empty, err := parseTriggerConditions("")
	if err != nil || empty != nil {
		t.Errorf("empty conditions = (%v, %v), want (nil, nil)", empty, err)
	}
}

// TestApplyCommonFields_CreateBeadShaping verifies the create_bead action
// fields (ka-0000002642, C1) are stamped onto the trigger fields, validated,
// and — crucially — that P0 (0) is distinguishable from "unset". These fields
// are read by ParseTriggerDef/createTaskBead to shape the filed bead.
func TestApplyCommonFields_CreateBeadShaping(t *testing.T) {
	p1 := 1
	p0 := 0
	cases := []struct {
		name      string
		opts      commonTriggerOpts
		wantType  any // value or nil if field should be absent
		wantPrio  any
		wantPar   any
		wantLabel any
	}{
		{
			name:      "all set, priority 1",
			opts:      commonTriggerOpts{action: "create_bead", cooldown: "0", maxConcurrent: 1, enabled: true, beadType: "bug", beadPriority: &p1, parentBead: "gd-EPIC", beadLabels: []string{"needs-intake", "liveness"}},
			wantType:  "bug",
			wantPrio:  1,
			wantPar:   "gd-EPIC",
			wantLabel: "needs-intake,liveness",
		},
		{
			name:     "priority P0 explicitly set is stamped (not treated as unset)",
			opts:     commonTriggerOpts{action: "create_bead", cooldown: "0", maxConcurrent: 1, enabled: true, beadPriority: &p0},
			wantPrio: 0,
		},
		{
			name: "nothing set leaves all create_bead fields absent",
			opts: commonTriggerOpts{action: "notify", cooldown: "0", maxConcurrent: 1, enabled: true},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := map[string]any{}
			if err := applyCommonFields(c.opts, f); err != nil {
				t.Fatalf("applyCommonFields: %v", err)
			}
			checkField(t, f, "bead_type", c.wantType)
			checkField(t, f, "bead_priority", c.wantPrio)
			checkField(t, f, "parent", c.wantPar)
			checkField(t, f, "extra_labels", c.wantLabel)
		})
	}
}

func checkField(t *testing.T, f map[string]any, key string, want any) {
	t.Helper()
	got, present := f[key]
	if want == nil {
		if present {
			t.Errorf("field %q should be absent, got %v", key, got)
		}
		return
	}
	if !present {
		t.Errorf("field %q should be present (want %v), absent", key, want)
		return
	}
	if got != want {
		t.Errorf("field %q = %v, want %v", key, got, want)
	}
}

// TestApplyCommonFields_CreateBeadValidation rejects malformed create_bead
// shaping at authoring time (not silently at fire time).
func TestApplyCommonFields_CreateBeadValidation(t *testing.T) {
	badPrio := 9
	negPrio := -1
	cases := []struct {
		name string
		opts commonTriggerOpts
		want string
	}{
		{"bad bead_type", commonTriggerOpts{action: "create_bead", cooldown: "0", beadType: "molecule"}, "invalid --bead-type"},
		{"priority too high", commonTriggerOpts{action: "create_bead", cooldown: "0", beadPriority: &badPrio}, "--bead-priority must be 0-4"},
		{"priority negative", commonTriggerOpts{action: "create_bead", cooldown: "0", beadPriority: &negPrio}, "--bead-priority must be 0-4"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := applyCommonFields(c.opts, map[string]any{})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("expected error containing %q, got %v", c.want, err)
			}
		})
	}
}
