package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"gardenio/controller/internal/scheduler"
)

func TestParseBeadQueryCondition_Defaults(t *testing.T) {
	got, err := parseBeadQueryCondition("labels=needs-review,statuses=open")
	if err != nil {
		t.Fatalf("parseBeadQueryCondition: %v", err)
	}
	want := scheduleConditionJSON{
		Type:     "bead_query",
		Op:       ">",
		Value:    "0",
		Labels:   []string{"needs-review"},
		Statuses: []string{"open"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

func TestParseBeadQueryCondition_FullMatrix(t *testing.T) {
	got, err := parseBeadQueryCondition("types=task|bug,labels=a|b,exclude_labels=skip-me|opt-out,statuses=open|in_progress,project=gardenio,role=crew,op=>=,value=5")
	if err != nil {
		t.Fatalf("parseBeadQueryCondition: %v", err)
	}
	want := scheduleConditionJSON{
		Type:          "bead_query",
		Op:            ">=",
		Value:         "5",
		Project:       "gardenio",
		Role:          "crew",
		Labels:        []string{"a", "b"},
		ExcludeLabels: []string{"skip-me", "opt-out"},
		Types:         []string{"task", "bug"},
		Statuses:      []string{"open", "in_progress"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

func TestParseBeadQueryCondition_ProjectAutoLabels(t *testing.T) {
	// When `project=` is given but no `labels=`, the project is added as a
	// project:<name> label so the count query stays scoped.
	got, err := parseBeadQueryCondition("project=gardenio,op=>,value=0")
	if err != nil {
		t.Fatalf("parseBeadQueryCondition: %v", err)
	}
	if !reflect.DeepEqual(got.Labels, []string{"project:gardenio"}) {
		t.Errorf("expected auto-label [project:gardenio], got %v", got.Labels)
	}
	if got.Project != "gardenio" {
		t.Errorf("project = %q, want gardenio", got.Project)
	}

	// When labels= is also explicit, do NOT clobber it.
	got2, err := parseBeadQueryCondition("project=gardenio,labels=needs-review")
	if err != nil {
		t.Fatalf("parseBeadQueryCondition: %v", err)
	}
	if !reflect.DeepEqual(got2.Labels, []string{"needs-review"}) {
		t.Errorf("expected explicit labels to win, got %v", got2.Labels)
	}
}

func TestParseBeadQueryCondition_ShortTitleMissing(t *testing.T) {
	got, err := parseBeadQueryCondition("project=gardenio,types=task|bug,statuses=open|closed,short_title_missing=true,op=>,value=0")
	if err != nil {
		t.Fatalf("parseBeadQueryCondition: %v", err)
	}
	if !got.ShortTitleMissing {
		t.Errorf("ShortTitleMissing = false, want true")
	}
	j, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(j), `"short_title_missing":true`) {
		t.Errorf("marshalled condition lacks short_title_missing: %s", j)
	}

	off, err := parseBeadQueryCondition("types=task,short_title_missing=false")
	if err != nil {
		t.Fatalf("parseBeadQueryCondition: %v", err)
	}
	if off.ShortTitleMissing {
		t.Errorf("short_title_missing=false parsed as true")
	}

	if _, err := parseBeadQueryCondition("types=task,short_title_missing=yes"); err == nil {
		t.Errorf("short_title_missing=yes: expected error")
	}
}

// TestParseBeadQueryCondition_ShortTitleGroomer pins the exact --bead-query the
// short-title groomer trigger is created with (gardenio/docs/groomers/
// short-title-groomer.md) to the condition the scheduler gates on.
func TestParseBeadQueryCondition_ShortTitleGroomer(t *testing.T) {
	got, err := parseBeadQueryCondition("project=gardenio,types=task|bug|feature|epic|escalation,statuses=open|in_progress|blocked,exclude_labels=source:schedule,short_title_missing=true,op=>,value=0")
	if err != nil {
		t.Fatalf("parseBeadQueryCondition: %v", err)
	}
	want := scheduleConditionJSON{
		Type:              "bead_query",
		Op:                ">",
		Value:             "0",
		Project:           "gardenio",
		Labels:            []string{"project:gardenio"},
		ExcludeLabels:     []string{"source:schedule"},
		Types:             []string{"task", "bug", "feature", "epic", "escalation"},
		Statuses:          []string{"open", "in_progress", "blocked"},
		ShortTitleMissing: true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

func TestParseBeadQueryCondition_TypeOverride(t *testing.T) {
	got, err := parseBeadQueryCondition("type=ready_queue,project=gardenio,op=>,value=2")
	if err != nil {
		t.Fatalf("parseBeadQueryCondition: %v", err)
	}
	if got.Type != "ready_queue" {
		t.Errorf("type = %q, want ready_queue", got.Type)
	}
	// ready_queue does not get the bead_query default statuses or label injection.
	if got.Statuses != nil {
		t.Errorf("expected no auto statuses for ready_queue, got %v", got.Statuses)
	}
	if got.Labels != nil {
		t.Errorf("expected no auto labels for ready_queue, got %v", got.Labels)
	}
}

func TestParseBeadQueryCondition_Errors(t *testing.T) {
	cases := []string{
		"",               // empty
		"   ",            // whitespace only
		"no-equals",      // no equals
		"unknownkey=foo", // unknown key
		"op=>,nokey",     // mixed valid + bad segment
	}
	for _, c := range cases {
		if _, err := parseBeadQueryCondition(c); err == nil {
			t.Errorf("parseBeadQueryCondition(%q): expected error", c)
		}
	}
}

func TestBuildScheduleFields(t *testing.T) {
	opts := commonTriggerOpts{
		action:        "spawn_agent",
		role:          "triage",
		agentProject:  "gardenio",
		cooldown:      "10m",
		maxConcurrent: 1,
		enabled:       true,
	}
	fields, labels, err := buildScheduleFields(opts,
		"*/30 9-17 * * 1-5",
		"America/Los_Angeles",
		[]string{"project=gardenio,types=task,statuses=open,op=>,value=0"},
	)
	if err != nil {
		t.Fatalf("buildScheduleFields: %v", err)
	}
	if fields["cron"] != "*/30 9-17 * * 1-5" {
		t.Errorf("cron = %v", fields["cron"])
	}
	if fields["timezone"] != "America/Los_Angeles" {
		t.Errorf("timezone = %v", fields["timezone"])
	}
	if fields["action"] != "spawn_agent" {
		t.Errorf("action = %v", fields["action"])
	}
	if fields["role"] != "triage" {
		t.Errorf("role = %v", fields["role"])
	}
	condStr, _ := fields["conditions"].(string)
	var got []scheduleConditionJSON
	if err := json.Unmarshal([]byte(condStr), &got); err != nil {
		t.Fatalf("conditions JSON: %v (raw: %s)", err, condStr)
	}
	want := []scheduleConditionJSON{
		{
			Type:     "bead_query",
			Op:       ">",
			Value:    "0",
			Project:  "gardenio",
			Types:    []string{"task"},
			Statuses: []string{"open"},
			Labels:   []string{"project:gardenio"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("conditions = %#v, want %#v", got, want)
	}
	if !containsString(labels, "trigger:schedule") {
		t.Errorf("labels missing trigger:schedule: %v", labels)
	}
}

func TestBuildScheduleFields_OmitsTimezoneAndConditionsWhenEmpty(t *testing.T) {
	opts := commonTriggerOpts{action: "notify", cooldown: "0", maxConcurrent: 1, enabled: true}
	fields, _, err := buildScheduleFields(opts, "0 9 * * *", "", nil)
	if err != nil {
		t.Fatalf("buildScheduleFields: %v", err)
	}
	if _, present := fields["timezone"]; present {
		t.Errorf("expected timezone to be omitted when --tz is empty, got fields=%v", fields)
	}
	if _, present := fields["conditions"]; present {
		t.Errorf("expected conditions to be omitted when --bead-query is empty, got fields=%v", fields)
	}
}

func TestDefaultScheduleCron(t *testing.T) {
	// An omitted --cron defaults to a 30-minute cadence. Guard both the value
	// (the standing rule) and that it is a parseable cron expression.
	if defaultScheduleCron != "*/30 * * * *" {
		t.Fatalf("defaultScheduleCron = %q, want %q", defaultScheduleCron, "*/30 * * * *")
	}
	if _, err := scheduler.ParseCron(defaultScheduleCron); err != nil {
		t.Fatalf("defaultScheduleCron %q must be a valid cron: %v", defaultScheduleCron, err)
	}
}

func TestRunTriggerCreateSchedule_RejectsInvalidCron(t *testing.T) {
	t.Cleanup(func() {
		scheduleCron = ""
		scheduleTimezone = ""
		scheduleBeadQuery = nil
		scheduleOpts = commonTriggerOpts{}
	})
	scheduleCron = "not a cron expression"
	err := runTriggerCreateSchedule(triggerCreateScheduleCmd, nil)
	if err == nil || !strings.Contains(err.Error(), "invalid --cron") {
		t.Fatalf("expected invalid cron error, got %v", err)
	}
}

func TestSplitPipe(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"a", []string{"a"}},
		{"a|b|c", []string{"a", "b", "c"}},
		{"a||b", []string{"a", "b"}},
		{" a | b ", []string{"a", "b"}},
		{"a|a|b", []string{"a", "b"}}, // dedupe
	}
	for _, c := range cases {
		got := splitPipe(c.in)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("splitPipe(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
