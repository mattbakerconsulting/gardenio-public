package main

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestNormalizeBeadEventTypes(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want string
	}{
		{"empty", nil, ""},
		{"single", []string{"created"}, "created"},
		{"csv", []string{"created,updated"}, "created,updated"},
		{"repeated and csv mixed", []string{"created", "updated,closed"}, "created,updated,closed"},
		{"dedupe", []string{"created", "created,updated"}, "created,updated"},
		{"trim whitespace", []string{" created , updated "}, "created,updated"},
		{"drop empties", []string{",,created,,"}, "created"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := normalizeBeadEventTypes(c.in); got != c.want {
				t.Errorf("normalizeBeadEventTypes(%v) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestParseBeadConditions(t *testing.T) {
	got, err := parseBeadConditions([]string{
		"type:==:task",
		"label:needs-review:present",
		"status:in:open,in_progress",
	})
	if err != nil {
		t.Fatalf("parseBeadConditions: %v", err)
	}
	want := []beadConditionJSON{
		{Field: "type", Op: "==", Value: "task"},
		{Field: "label:needs-review", Op: "present", Value: ""},
		{Field: "status", Op: "in", Value: "open,in_progress"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseBeadConditions = %#v, want %#v", got, want)
	}
}

func TestParseBeadConditions_DiffOps(t *testing.T) {
	got, err := parseBeadConditions([]string{
		"status:field_changed",
		"status:changed_to:in_progress",
		"label:project:gardenio:field_changed", // label as field name still parses
	})
	if err != nil {
		t.Fatalf("parseBeadConditions: %v", err)
	}
	want := []beadConditionJSON{
		{Field: "status", Op: "field_changed", Value: ""},
		{Field: "status", Op: "changed_to", Value: "in_progress"},
		{Field: "label:project:gardenio", Op: "field_changed", Value: ""},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseBeadConditions = %#v, want %#v", got, want)
	}
}

func TestParseBeadConditions_TimeRelativeOps(t *testing.T) {
	got, err := parseBeadConditions([]string{
		"last_checked:older_than:2h",
		"last_checked:newer_than:90m",
	})
	if err != nil {
		t.Fatalf("parseBeadConditions: %v", err)
	}
	want := []beadConditionJSON{
		{Field: "last_checked", Op: "older_than", Value: "2h"},
		{Field: "last_checked", Op: "newer_than", Value: "90m"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseBeadConditions = %#v, want %#v", got, want)
	}
}

func TestParseBeadConditions_TimeRelativeRequiresValue(t *testing.T) {
	// older_than / newer_than are not unary — a missing duration must error.
	if _, err := parseBeadConditions([]string{"last_checked:older_than"}); err == nil {
		t.Errorf("parseBeadConditions(last_checked:older_than): expected error (operator requires a value)")
	}
}

func TestParseBeadConditions_ChangedToRequiresValue(t *testing.T) {
	// changed_to is not unary — missing value should error.
	if _, err := parseBeadConditions([]string{"status:changed_to"}); err == nil {
		t.Errorf("parseBeadConditions(status:changed_to): expected error (operator requires a value)")
	}
}

func TestParseBeadConditions_InvoluntaryDeath(t *testing.T) {
	// is_involuntary_death is unary (value optional): bare form matches any
	// family; a value narrows to one family. Both must parse (ka-0000005447).
	got, err := parseBeadConditions([]string{
		"close_reason:is_involuntary_death",
		"close_reason:is_involuntary_death:stop_gate_cooldown",
	})
	if err != nil {
		t.Fatalf("parseBeadConditions: %v", err)
	}
	want := []beadConditionJSON{
		{Field: "close_reason", Op: "is_involuntary_death", Value: ""},
		{Field: "close_reason", Op: "is_involuntary_death", Value: "stop_gate_cooldown"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseBeadConditions = %#v, want %#v", got, want)
	}
}

func TestParseBeadConditions_Errors(t *testing.T) {
	cases := []string{
		"missingop",   // no colon
		":op:value",   // empty field
		"path::value", // empty op
	}
	for _, c := range cases {
		if _, err := parseBeadConditions([]string{c}); err == nil {
			t.Errorf("parseBeadConditions(%q): expected error", c)
		}
	}
}

func TestBuildBeadFields(t *testing.T) {
	opts := commonTriggerOpts{
		action:        "spawn_agent",
		role:          "crew",
		agentProject:  "monorepo",
		cooldown:      "5m",
		maxConcurrent: 1,
		enabled:       true,
	}
	fields, labels, err := buildBeadFields(opts,
		[]string{"created,updated"},
		[]string{"type:==:task", "label:project:gardenio:present"},
	)
	if err != nil {
		t.Fatalf("buildBeadFields: %v", err)
	}
	if fields["event_types"] != "created,updated" {
		t.Errorf("event_types = %v, want created,updated", fields["event_types"])
	}
	if fields["action"] != "spawn_agent" {
		t.Errorf("action = %v", fields["action"])
	}
	if fields["role"] != "crew" {
		t.Errorf("role = %v", fields["role"])
	}
	condStr, _ := fields["conditions"].(string)
	var got []beadConditionJSON
	if err := json.Unmarshal([]byte(condStr), &got); err != nil {
		t.Fatalf("conditions JSON: %v (raw: %s)", err, condStr)
	}
	want := []beadConditionJSON{
		{Field: "type", Op: "==", Value: "task"},
		{Field: "label:project:gardenio", Op: "present", Value: ""},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("conditions = %#v, want %#v", got, want)
	}
	if !containsString(labels, "trigger:bead") {
		t.Errorf("labels missing trigger:bead: %v", labels)
	}
}

func TestBuildBeadFields_OmitsEmptyEventTypesAndConditions(t *testing.T) {
	opts := commonTriggerOpts{action: "notify", cooldown: "0", maxConcurrent: 1, enabled: true}
	fields, _, err := buildBeadFields(opts, nil, nil)
	if err != nil {
		t.Fatalf("buildBeadFields: %v", err)
	}
	if _, present := fields["event_types"]; present {
		t.Errorf("expected event_types to be omitted when no --event-types are given, got fields=%v", fields)
	}
	if _, present := fields["conditions"]; present {
		t.Errorf("expected conditions to be omitted when no --condition is given, got fields=%v", fields)
	}
}
