package bridge

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/scheduler"
)

// --- Test helpers ---

// newScheduleSource creates a ScheduleTriggerSource with a fixed clock for testing.
func newScheduleSource(now time.Time) *ScheduleTriggerSource {
	s := NewScheduleTriggerSource(slog.Default())
	s.nowFunc = func() time.Time { return now }
	return s
}

// scheduleTriggerBead creates a trigger bead configured as a schedule trigger.
//
// Defaults `last_checked` to 1 hour ago so the engine's runtime-bootstrap
// guard does not suppress first-evaluation firing in these tests.
func scheduleTriggerBead(id, cron string, fields map[string]string) *beadsapi.BeadDetail {
	f := map[string]string{
		"cron":         cron,
		"enabled":      "true",
		"action":       "notify",
		"last_checked": time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339),
	}
	for k, v := range fields {
		f[k] = v
	}
	return &beadsapi.BeadDetail{
		ID:     id,
		Type:   "trigger",
		Status: "open",
		Title:  "Test Schedule " + id,
		Labels: []string{"trigger:schedule"},
		Fields: f,
	}
}

// --- Unit tests ---

func TestScheduleTriggerSource_Name(t *testing.T) {
	s := NewScheduleTriggerSource(nil)
	if s.Name() != "schedule" {
		t.Errorf("Name() = %q, want %q", s.Name(), "schedule")
	}
}

func TestScheduleTriggerSource_LoadTriggers(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			scheduleTriggerBead("t1", "*/10 * * * *", nil),
			scheduleTriggerBead("t2", "", nil),                                            // missing cron, should skip
			scheduleTriggerBead("t3", "0 9 * * *", map[string]string{"enabled": "false"}), // disabled
			scheduleTriggerBead("t4", "0 12 * * 1-5", map[string]string{
				"timezone":   "America/Los_Angeles",
				"conditions": `[{"type":"ready_queue","op":">","value":"0"}]`,
			}),
		},
	}

	source := NewScheduleTriggerSource(slog.Default())
	triggers, err := source.LoadTriggers(context.Background(), daemon)
	if err != nil {
		t.Fatalf("LoadTriggers() error: %v", err)
	}

	// Should load t1 and t4 (t2 skipped for missing cron, t3 disabled).
	if len(triggers) != 2 {
		t.Fatalf("expected 2 triggers, got %d", len(triggers))
	}
	if triggers[0].ID != "t1" {
		t.Errorf("triggers[0].ID = %q", triggers[0].ID)
	}
	if triggers[1].ID != "t4" {
		t.Errorf("triggers[1].ID = %q", triggers[1].ID)
	}

	// Check t4 has conditions parsed.
	data, ok := triggers[1].SourceData.(scheduleTriggerData)
	if !ok {
		t.Fatal("triggers[1].SourceData type assertion failed")
	}
	if data.Timezone != "America/Los_Angeles" {
		t.Errorf("timezone = %q", data.Timezone)
	}
	if len(data.Conditions) != 1 {
		t.Fatalf("expected 1 condition, got %d", len(data.Conditions))
	}
	if data.Conditions[0].Type != "ready_queue" {
		t.Errorf("condition type = %q", data.Conditions[0].Type)
	}
}

// TestScheduleTriggerSource_LoadTriggers_SkipsE2EFixture pins ka-0000005435:
// a trigger:schedule bead seeded by the beads-viewer-e2e suite (carrying a
// `fixture:e2e:<key>` label) must NOT be returned as a fireable TriggerDef even
// when enabled=true, so the shared prod scheduler never spawns a live agent for
// it. The fixture's enabled field is deliberately left untouched so it still
// renders in beads-viewer.
func TestScheduleTriggerSource_LoadTriggers_SkipsE2EFixture(t *testing.T) {
	fixture := scheduleTriggerBead("fixture", "@hourly", nil)
	fixture.Labels = append(fixture.Labels, "fixture:e2e:schedule_hourly")

	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			fixture, // E2E fixture, enabled=true — must be skipped
			scheduleTriggerBead("real", "0 9 * * *", nil), // a real schedule trigger — must load
		},
	}

	source := NewScheduleTriggerSource(slog.Default())
	triggers, err := source.LoadTriggers(context.Background(), daemon)
	if err != nil {
		t.Fatalf("LoadTriggers() error: %v", err)
	}

	if len(triggers) != 1 {
		t.Fatalf("expected 1 fireable trigger (fixture skipped), got %d: %+v", len(triggers), triggers)
	}
	if triggers[0].ID != "real" {
		t.Errorf("expected the real trigger to load, got %q", triggers[0].ID)
	}
	for _, tr := range triggers {
		if tr.ID == "fixture" {
			t.Errorf("E2E fixture %q must not be returned as a fireable trigger", tr.ID)
		}
	}

	// Sanity: the fixture's enabled field is untouched — it still parses as
	// enabled so beads-viewer renders it. (We only suppress scheduler firing.)
	if fixture.Fields["enabled"] != "true" {
		t.Errorf("fixture enabled field must remain true (viewer still renders it), got %q", fixture.Fields["enabled"])
	}
}

// TestIsE2EFixture covers the shared helper used by every spawn_agent firing
// source to skip beads-viewer-e2e fixtures (ka-0000005435).
func TestIsE2EFixture(t *testing.T) {
	cases := []struct {
		name   string
		labels []string
		want   bool
	}{
		{"hourly fixture", []string{"trigger:schedule", "fixture:e2e:schedule_hourly"}, true},
		{"bare prefix only", []string{"fixture:e2e:"}, true},
		{"no fixture label", []string{"trigger:schedule", "project:gardenio"}, false},
		{"similar but not prefix", []string{"e2e:fixture:schedule"}, false},
		{"empty labels", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bead := &beadsapi.BeadDetail{Labels: tc.labels}
			if got := isE2EFixture(bead); got != tc.want {
				t.Errorf("isE2EFixture(%v) = %v, want %v", tc.labels, got, tc.want)
			}
		})
	}
}

func TestScheduleTriggerSource_LoadTriggers_InvalidConditionsJSON(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			scheduleTriggerBead("t1", "0 9 * * *", map[string]string{
				"conditions": "not valid json",
			}),
		},
	}

	source := NewScheduleTriggerSource(slog.Default())
	triggers, err := source.LoadTriggers(context.Background(), daemon)
	if err != nil {
		t.Fatalf("LoadTriggers() error: %v", err)
	}

	// Should skip the trigger with invalid conditions.
	if len(triggers) != 0 {
		t.Errorf("expected 0 triggers (invalid conditions), got %d", len(triggers))
	}
}

// TestScheduleTriggerSource_LoadTriggers_WarnsOnTriggersArray verifies the
// mis-config guardrail (ka-0000002562): a trigger:schedule bead that carries a
// non-empty triggers[] array has that array silently dropped by this source
// (only trigger:multi / schedule-mirror fan it in). LoadTriggers must emit a
// WARN so the mis-label is discoverable. Two shapes are checked:
//   - cron + triggers[]: still loads (cron fires), but warns.
//   - no cron + triggers[]: warns AND skips (it would fire from nothing).
func TestScheduleTriggerSource_LoadTriggers_WarnsOnTriggersArray(t *testing.T) {
	triggersJSON := `[{"type":"bead_event","topics":["beads.bead.updated"]}]`

	withCron := scheduleTriggerBead("has-cron", "0 * * * *", map[string]string{
		"triggers": triggersJSON,
	})
	noCron := scheduleTriggerBead("no-cron", "", map[string]string{
		"triggers": triggersJSON,
	})

	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{withCron, noCron},
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	source := NewScheduleTriggerSource(logger)

	triggers, err := source.LoadTriggers(context.Background(), daemon)
	if err != nil {
		t.Fatalf("LoadTriggers() error: %v", err)
	}

	// Only the cron-bearing bead is loaded; the no-cron bead is skipped.
	if len(triggers) != 1 {
		t.Fatalf("expected 1 trigger (has-cron only), got %d", len(triggers))
	}
	if triggers[0].ID != "has-cron" {
		t.Errorf("loaded trigger ID = %q, want has-cron", triggers[0].ID)
	}

	// Both beads must produce the triggers[]-ignored WARN.
	logs := buf.String()
	for _, id := range []string{"has-cron", "no-cron"} {
		if !strings.Contains(logs, "triggers[] array ignored") || !strings.Contains(logs, id) {
			t.Errorf("expected triggers[]-ignored warning for %q, logs:\n%s", id, logs)
		}
	}
}

func TestScheduleTriggerSource_LoadTriggers_ParsesLastRun(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			scheduleTriggerBead("t1", "0 * * * *", map[string]string{
				"last_run": "2026-04-12T10:00:00Z",
			}),
		},
	}

	source := NewScheduleTriggerSource(slog.Default())
	triggers, err := source.LoadTriggers(context.Background(), daemon)
	if err != nil {
		t.Fatal(err)
	}

	if len(triggers) != 1 {
		t.Fatalf("expected 1 trigger, got %d", len(triggers))
	}
	data := triggers[0].SourceData.(scheduleTriggerData)
	if data.LastRun.IsZero() {
		t.Error("expected last_run to be parsed")
	}
	if data.LastRun.Hour() != 10 {
		t.Errorf("last_run hour = %d, want 10", data.LastRun.Hour())
	}
}

func TestScheduleTriggerSource_Evaluate_CronFires(t *testing.T) {
	// Scenario: cron "0 * * * *" (every hour on the hour).
	// Current time: 10:05 UTC. Last run: never.
	// Expected: fires for the 10:00 fire time.
	now := time.Date(2026, 4, 12, 10, 5, 0, 0, time.UTC)
	source := newScheduleSource(now)
	source.daemon = &mockTriggerDaemon{} // no conditions

	trig := TriggerDef{
		ID:            "t1",
		Title:         "Hourly Check",
		Source:        "schedule",
		Action:        TriggerActionNotify,
		MaxConcurrent: 10,
		SourceData: scheduleTriggerData{
			Cron: "0 * * * *",
		},
	}

	matches, err := source.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected 1 match, got %d", len(matches))
	}
	if matches[0].Key != "2026-04-12T10:00" {
		t.Errorf("match key = %q", matches[0].Key)
	}
	if matches[0].Title != "Schedule: Hourly Check" {
		t.Errorf("match title = %q", matches[0].Title)
	}
}

func TestScheduleTriggerSource_Evaluate_AlreadyRan(t *testing.T) {
	// Scenario: cron "0 * * * *", last_run is after the most recent fire time.
	now := time.Date(2026, 4, 12, 10, 5, 0, 0, time.UTC)
	source := newScheduleSource(now)

	trig := TriggerDef{
		ID:     "t1",
		Source: "schedule",
		SourceData: scheduleTriggerData{
			Cron:    "0 * * * *",
			LastRun: time.Date(2026, 4, 12, 10, 1, 0, 0, time.UTC), // ran after 10:00
		},
	}

	matches, err := source.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Errorf("expected 0 matches (already ran), got %d", len(matches))
	}
}

func TestScheduleTriggerSource_Evaluate_OutsideWindow(t *testing.T) {
	// Scenario: cron "0 9-17 * * 1-5" (weekdays 9am-5pm).
	// Current time: Saturday 14:00.
	now := time.Date(2026, 4, 11, 14, 0, 0, 0, time.UTC) // Saturday
	source := newScheduleSource(now)

	trig := TriggerDef{
		ID:     "t1",
		Source: "schedule",
		SourceData: scheduleTriggerData{
			Cron: "0 9-17 * * 1-5",
		},
	}

	matches, err := source.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Errorf("expected 0 matches (outside window), got %d", len(matches))
	}
}

func TestScheduleTriggerSource_Evaluate_WithTimezone(t *testing.T) {
	// Scenario: cron "0 9 * * *" (9am daily) in America/Los_Angeles.
	// Current time: 16:05 UTC = 9:05 AM PDT. Should fire.
	now := time.Date(2026, 4, 12, 16, 5, 0, 0, time.UTC)
	source := newScheduleSource(now)
	source.daemon = &mockTriggerDaemon{}

	trig := TriggerDef{
		ID:            "t1",
		Title:         "Morning Check",
		Source:        "schedule",
		Action:        TriggerActionNotify,
		MaxConcurrent: 10,
		SourceData: scheduleTriggerData{
			Cron:     "0 9 * * *",
			Timezone: "America/Los_Angeles",
		},
	}

	matches, err := source.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected 1 match (9am PDT = 16:00 UTC), got %d", len(matches))
	}

	// The fire_time_local template var should be in Pacific time.
	if v := matches[0].TemplateVars["{{timezone}}"]; v != "America/Los_Angeles" {
		t.Errorf("timezone var = %q", v)
	}
}

func TestScheduleTriggerSource_Evaluate_InvalidCron(t *testing.T) {
	now := time.Date(2026, 4, 12, 10, 0, 0, 0, time.UTC)
	source := newScheduleSource(now)

	trig := TriggerDef{
		ID:     "t1",
		Source: "schedule",
		SourceData: scheduleTriggerData{
			Cron: "not a cron",
		},
	}

	_, err := source.Evaluate(context.Background(), trig)
	if err == nil {
		t.Error("expected error for invalid cron")
	}
}

// CronDue backs the bridge engine's cron-due cooldown bypass (bug
// ka-0000004307): it must mirror Evaluate's due/not-due decision without
// touching state or evaluating pre-flight conditions.
func TestScheduleTriggerSource_CronDue(t *testing.T) {
	// 10:05 UTC with cron "0 * * * *": inside the window, past the 10:00 boundary.
	inWindow := time.Date(2026, 4, 12, 10, 5, 0, 0, time.UTC)

	trigWith := func(data scheduleTriggerData) TriggerDef {
		return TriggerDef{ID: "t1", Source: "schedule", SourceData: data}
	}

	t.Run("due when in-window and past last_run", func(t *testing.T) {
		source := newScheduleSource(inWindow)
		if !source.CronDue(context.Background(), trigWith(scheduleTriggerData{Cron: "0 * * * *"})) {
			t.Error("cron past its 10:00 boundary with zero LastRun must be due")
		}
	})

	t.Run("not due when last_run covers the boundary", func(t *testing.T) {
		source := newScheduleSource(inWindow)
		data := scheduleTriggerData{
			Cron:    "0 * * * *",
			LastRun: time.Date(2026, 4, 12, 10, 1, 0, 0, time.UTC),
		}
		if source.CronDue(context.Background(), trigWith(data)) {
			t.Error("a window already consumed by LastRun must not be due")
		}
	})

	t.Run("not due outside the window", func(t *testing.T) {
		// Saturday 14:00 with a weekday 9-17 cron.
		source := newScheduleSource(time.Date(2026, 4, 11, 14, 0, 0, 0, time.UTC))
		if source.CronDue(context.Background(), trigWith(scheduleTriggerData{Cron: "0 9-17 * * 1-5"})) {
			t.Error("outside the cron window must not be due")
		}
	})

	t.Run("timezone respected", func(t *testing.T) {
		// 16:05 UTC = 9:05 PDT: due for a 9am Pacific cron.
		source := newScheduleSource(time.Date(2026, 4, 12, 16, 5, 0, 0, time.UTC))
		data := scheduleTriggerData{Cron: "0 9 * * *", Timezone: "America/Los_Angeles"}
		if !source.CronDue(context.Background(), trigWith(data)) {
			t.Error("9:05 AM PDT must be due for a 9am Pacific cron")
		}
	})

	t.Run("invalid cron is not due", func(t *testing.T) {
		source := newScheduleSource(inWindow)
		if source.CronDue(context.Background(), trigWith(scheduleTriggerData{Cron: "not a cron"})) {
			t.Error("invalid cron must not hold the bypass open")
		}
	})

	t.Run("bad source data is not due", func(t *testing.T) {
		source := newScheduleSource(inWindow)
		trig := TriggerDef{ID: "t1", SourceData: "not scheduleTriggerData"}
		if source.CronDue(context.Background(), trig) {
			t.Error("non-schedule source data must not be due")
		}
	})
}

func TestScheduleTriggerSource_Evaluate_BadSourceData(t *testing.T) {
	source := NewScheduleTriggerSource(nil)
	trig := TriggerDef{
		ID:         "t1",
		SourceData: "not scheduleTriggerData",
	}

	matches, err := source.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Errorf("expected 0 matches for bad source data, got %d", len(matches))
	}
}

func TestScheduleTriggerSource_CatchUpKeys(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{
				ID:     "gd-task-1",
				Type:   "task",
				Status: "closed",
				Labels: []string{"trigger:t1"},
				Fields: map[string]string{"schedule_fire_key": "2026-04-12T10:00"},
			},
			{
				ID:     "gd-task-2",
				Type:   "task",
				Status: "open",
				Labels: []string{"trigger:t1"},
				Fields: map[string]string{"schedule_fire_key": "2026-04-12T11:00"},
			},
		},
	}

	source := NewScheduleTriggerSource(nil)
	keys, err := source.CatchUpKeys(context.Background(), daemon, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 {
		t.Fatalf("expected 2 catch-up keys, got %d", len(keys))
	}
}

// --- Condition tests ---

func TestScheduleCondition_ReadyQueue(t *testing.T) {
	// Daemon has 5 open issue beads.
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{ID: "b1", Type: "task", Status: "open", Labels: []string{"project:test"}},
			{ID: "b2", Type: "task", Status: "open", Labels: []string{"project:test"}},
			{ID: "b3", Type: "task", Status: "open", Labels: []string{"project:test"}},
			{ID: "b4", Type: "task", Status: "open", Labels: []string{"project:test"}},
			{ID: "b5", Type: "task", Status: "open", Labels: []string{"project:test"}},
		},
	}

	now := time.Date(2026, 4, 12, 10, 5, 0, 0, time.UTC)
	source := newScheduleSource(now)
	source.daemon = daemon

	// Condition: ready_queue > 2 for project "test".
	trig := TriggerDef{
		ID:            "t1",
		Title:         "Queue Check",
		Source:        "schedule",
		Project:       "test",
		Action:        TriggerActionNotify,
		MaxConcurrent: 10,
		SourceData: scheduleTriggerData{
			Cron: "0 * * * *",
			Conditions: []scheduler.Condition{
				{Type: "ready_queue", Op: ">", Value: "2", Project: "test"},
			},
		},
	}

	matches, err := source.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Errorf("expected 1 match (queue > 2, has 5), got %d", len(matches))
	}

	// Change condition to > 10: should not fire.
	trig.SourceData = scheduleTriggerData{
		Cron: "0 * * * *",
		Conditions: []scheduler.Condition{
			{Type: "ready_queue", Op: ">", Value: "10"},
		},
	}
	matches, err = source.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Errorf("expected 0 matches (queue 5 > 10 is false), got %d", len(matches))
	}
}

func TestScheduleCondition_ActiveAgents(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{ID: "a1", Type: "agent", Status: "open", Labels: []string{"project:test", "role:crew"}},
			{ID: "a2", Type: "agent", Status: "open", Labels: []string{"project:test", "role:crew"}},
		},
	}

	now := time.Date(2026, 4, 12, 10, 5, 0, 0, time.UTC)
	source := newScheduleSource(now)
	source.daemon = daemon

	// Condition: active_agents < 5 for crew role.
	trig := TriggerDef{
		ID:            "t1",
		Title:         "Agent Check",
		Source:        "schedule",
		Project:       "test",
		Action:        TriggerActionNotify,
		MaxConcurrent: 10,
		SourceData: scheduleTriggerData{
			Cron: "0 * * * *",
			Conditions: []scheduler.Condition{
				{Type: "active_agents", Op: "<", Value: "5", Role: "crew"},
			},
		},
	}

	matches, err := source.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Errorf("expected 1 match (2 agents < 5), got %d", len(matches))
	}

	// Condition: active_agents == 0 (should fail, there are 2).
	trig.SourceData = scheduleTriggerData{
		Cron: "0 * * * *",
		Conditions: []scheduler.Condition{
			{Type: "active_agents", Op: "==", Value: "0", Role: "crew"},
		},
	}
	matches, err = source.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Errorf("expected 0 matches (2 agents != 0), got %d", len(matches))
	}
}

func TestScheduleCondition_Gate_Absent(t *testing.T) {
	// No gate bead exists — condition should pass.
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{},
	}

	now := time.Date(2026, 4, 12, 10, 5, 0, 0, time.UTC)
	source := newScheduleSource(now)
	source.daemon = daemon

	trig := TriggerDef{
		ID:            "t1",
		Title:         "Gate Check",
		Source:        "schedule",
		Action:        TriggerActionNotify,
		MaxConcurrent: 10,
		SourceData: scheduleTriggerData{
			Cron: "0 * * * *",
			Conditions: []scheduler.Condition{
				{Type: "gate", Op: "absent", Value: "scheduler-pause"},
			},
		},
	}

	matches, err := source.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Errorf("expected 1 match (gate absent), got %d", len(matches))
	}
}

func TestScheduleCondition_Gate_Present(t *testing.T) {
	// Gate bead exists — "absent" condition should fail.
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{ID: "g1", Type: "gate", Status: "open", Title: "scheduler-pause"},
		},
	}

	now := time.Date(2026, 4, 12, 10, 5, 0, 0, time.UTC)
	source := newScheduleSource(now)
	source.daemon = daemon

	trig := TriggerDef{
		ID:            "t1",
		Title:         "Gate Check",
		Source:        "schedule",
		Action:        TriggerActionNotify,
		MaxConcurrent: 10,
		SourceData: scheduleTriggerData{
			Cron: "0 * * * *",
			Conditions: []scheduler.Condition{
				{Type: "gate", Op: "absent", Value: "scheduler-pause"},
			},
		},
	}

	matches, err := source.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Errorf("expected 0 matches (gate present, wanted absent), got %d", len(matches))
	}
}

func TestScheduleCondition_Gate_DefaultOp(t *testing.T) {
	// Empty op defaults to "absent".
	daemon := &mockTriggerDaemon{beads: []*beadsapi.BeadDetail{}}

	now := time.Date(2026, 4, 12, 10, 5, 0, 0, time.UTC)
	source := newScheduleSource(now)
	source.daemon = daemon

	trig := TriggerDef{
		ID:            "t1",
		Title:         "Gate Default",
		Source:        "schedule",
		Action:        TriggerActionNotify,
		MaxConcurrent: 10,
		SourceData: scheduleTriggerData{
			Cron: "0 * * * *",
			Conditions: []scheduler.Condition{
				{Type: "gate", Value: "maintenance-mode"}, // no op specified
			},
		},
	}

	matches, err := source.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Errorf("expected 1 match (gate absent, default op), got %d", len(matches))
	}
}

func TestScheduleCondition_Gate_EmptyValue(t *testing.T) {
	// Empty gate value means no gate to check — always passes.
	daemon := &mockTriggerDaemon{beads: []*beadsapi.BeadDetail{}}

	now := time.Date(2026, 4, 12, 10, 5, 0, 0, time.UTC)
	source := newScheduleSource(now)
	source.daemon = daemon

	trig := TriggerDef{
		ID:            "t1",
		Title:         "No Gate",
		Source:        "schedule",
		Action:        TriggerActionNotify,
		MaxConcurrent: 10,
		SourceData: scheduleTriggerData{
			Cron: "0 * * * *",
			Conditions: []scheduler.Condition{
				{Type: "gate", Op: "absent", Value: ""},
			},
		},
	}

	matches, err := source.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Errorf("expected 1 match (empty gate value = no check), got %d", len(matches))
	}
}

func TestScheduleCondition_BeadQuery(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{ID: "b1", Type: "bug", Status: "open", Labels: []string{"severity:critical"}},
			{ID: "b2", Type: "bug", Status: "open", Labels: []string{"severity:critical"}},
		},
	}

	now := time.Date(2026, 4, 12, 10, 5, 0, 0, time.UTC)
	source := newScheduleSource(now)
	source.daemon = daemon

	trig := TriggerDef{
		ID:            "t1",
		Title:         "Bead Query",
		Source:        "schedule",
		Action:        TriggerActionNotify,
		MaxConcurrent: 10,
		SourceData: scheduleTriggerData{
			Cron: "0 * * * *",
			Conditions: []scheduler.Condition{
				{
					Type:   "bead_query",
					Op:     ">=",
					Value:  "1",
					Types:  []string{"bug"},
					Labels: []string{"severity:critical"},
				},
			},
		},
	}

	matches, err := source.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Errorf("expected 1 match (2 bugs >= 1), got %d", len(matches))
	}
}

func TestScheduleCondition_MultipleConditions(t *testing.T) {
	// All conditions must pass.
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			// Ready queue items.
			{ID: "b1", Type: "task", Status: "open", Labels: []string{"project:test"}},
			// Active agents.
			{ID: "a1", Type: "agent", Status: "open", Labels: []string{"project:test", "role:crew"}},
		},
	}

	now := time.Date(2026, 4, 12, 10, 5, 0, 0, time.UTC)
	source := newScheduleSource(now)
	source.daemon = daemon

	trig := TriggerDef{
		ID:            "t1",
		Title:         "Multi Condition",
		Source:        "schedule",
		Project:       "test",
		Action:        TriggerActionNotify,
		MaxConcurrent: 10,
		SourceData: scheduleTriggerData{
			Cron: "0 * * * *",
			Conditions: []scheduler.Condition{
				{Type: "ready_queue", Op: ">", Value: "0"},   // 1 > 0 = true
				{Type: "active_agents", Op: "<", Value: "5"}, // 1 < 5 = true
			},
		},
	}

	matches, err := source.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Errorf("expected 1 match (both conditions pass), got %d", len(matches))
	}

	// Add a failing condition.
	trig.SourceData = scheduleTriggerData{
		Cron: "0 * * * *",
		Conditions: []scheduler.Condition{
			{Type: "ready_queue", Op: ">", Value: "0"},    // true
			{Type: "active_agents", Op: "==", Value: "0"}, // false (there is 1)
		},
	}

	matches, err = source.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Errorf("expected 0 matches (second condition fails), got %d", len(matches))
	}
}

func TestScheduleCondition_UnknownType(t *testing.T) {
	daemon := &mockTriggerDaemon{beads: []*beadsapi.BeadDetail{}}

	now := time.Date(2026, 4, 12, 10, 5, 0, 0, time.UTC)
	source := newScheduleSource(now)
	source.daemon = daemon

	trig := TriggerDef{
		ID:            "t1",
		Title:         "Unknown Condition",
		Source:        "schedule",
		Action:        TriggerActionNotify,
		MaxConcurrent: 10,
		SourceData: scheduleTriggerData{
			Cron: "0 * * * *",
			Conditions: []scheduler.Condition{
				{Type: "nonexistent", Op: "==", Value: "1"},
			},
		},
	}

	matches, err := source.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Errorf("expected 0 matches (unknown condition type), got %d", len(matches))
	}
}

// --- scheduler.CompareCount tests ---

func TestCompareCount(t *testing.T) {
	tests := []struct {
		count    int
		op       string
		value    string
		expected bool
	}{
		{5, ">", "3", true},
		{5, ">", "5", false},
		{5, ">=", "5", true},
		{5, ">=", "6", false},
		{3, "<", "5", true},
		{5, "<", "5", false},
		{5, "<=", "5", true},
		{6, "<=", "5", false},
		{5, "==", "5", true},
		{5, "==", "6", false},
		{5, "eq", "5", true},
		{5, "!=", "5", false},
		{5, "!=", "6", true},
		{5, "ne", "6", true},
		{5, "invalid_op", "5", false},
		{5, ">", "not_a_number", false},
	}

	for _, tt := range tests {
		name := fmt.Sprintf("%d_%s_%s", tt.count, tt.op, tt.value)
		t.Run(name, func(t *testing.T) {
			got := scheduler.CompareCount(tt.count, tt.op, tt.value)
			if got != tt.expected {
				t.Errorf("scheduler.CompareCount(%d, %q, %q) = %v, want %v",
					tt.count, tt.op, tt.value, got, tt.expected)
			}
		})
	}
}

// --- Engine integration tests ---

func TestScheduleTriggerSource_EngineIntegration_Notify(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			scheduleTriggerBead("t1", "0 * * * *", map[string]string{
				"prompt": "Hourly check at {{fire_time}}",
			}),
		},
	}

	now := time.Date(2026, 4, 12, 10, 5, 0, 0, time.UTC)
	source := newScheduleSource(now)

	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})

	var (
		mu        sync.Mutex
		notified  bool
		notifyMsg string
	)
	engine.notifier = func(_ context.Context, _, msg string, _ string) {
		mu.Lock()
		notified = true
		notifyMsg = msg
		mu.Unlock()
	}

	engine.RegisterSource(source)
	engine.Evaluate(context.Background())

	mu.Lock()
	if !notified {
		t.Error("expected notification")
	}
	if notifyMsg == "" {
		t.Error("expected non-empty notification message")
	}
	mu.Unlock()
}

func TestScheduleTriggerSource_EngineIntegration_CreateBead(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			scheduleTriggerBead("t1", "0 9 * * *", map[string]string{
				"action":  "create_bead",
				"project": "gardenio",
			}),
		},
	}

	now := time.Date(2026, 4, 12, 9, 5, 0, 0, time.UTC)
	source := newScheduleSource(now)

	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})
	engine.RegisterSource(source)
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	if len(daemon.createdBeads) != 1 {
		t.Fatalf("expected 1 created bead, got %d", len(daemon.createdBeads))
	}
	daemon.mu.Unlock()
}

func TestScheduleTriggerSource_EngineIntegration_SpawnAgent(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			scheduleTriggerBead("t1", "0 9 * * *", map[string]string{
				"action":  "spawn_agent",
				"role":    "gardener",
				"project": "gardenio",
				"prompt":  "Gardener duties at {{fire_time}}",
			}),
		},
	}

	now := time.Date(2026, 4, 12, 9, 5, 0, 0, time.UTC)
	source := newScheduleSource(now)

	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})
	engine.RegisterSource(source)
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()

	if len(daemon.spawnedNames) != 1 {
		t.Fatalf("expected 1 spawned agent, got %d", len(daemon.spawnedNames))
	}
	if daemon.spawnedRoles[0] != "gardener" {
		t.Errorf("agent role = %q, want %q", daemon.spawnedRoles[0], "gardener")
	}
	if daemon.lastPrompt == "" {
		t.Error("expected non-empty prompt")
	}
}

// TestScheduleTriggerSource_SpawnedTask_DependsOnTriggerBead drives a full
// cron-fire → engine → actionSpawnAgent path and asserts that the spawned
// tracking task carries a `triggered-by` dep to the trigger definition bead.
// Auto-link M2a (ka-0000007272, epic ka-0000000933).
func TestScheduleTriggerSource_SpawnedTask_DependsOnTriggerBead(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			scheduleTriggerBead("sched-m2a", "0 9 * * *", map[string]string{
				"action":  "spawn_agent",
				"role":    "gardener",
				"project": "gardenio",
				"prompt":  "Daily run at {{fire_time}}",
			}),
		},
	}

	now := time.Date(2026, 4, 12, 9, 5, 0, 0, time.UTC)
	source := newScheduleSource(now)

	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})
	engine.RegisterSource(source)
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()

	if len(daemon.spawnedNames) != 1 {
		t.Fatalf("expected 1 spawned agent, got %d", len(daemon.spawnedNames))
	}

	var sawTriggerDep bool
	for _, d := range daemon.addedDeps {
		if d.DepType != "triggered-by" {
			t.Errorf("unexpected dep type %q (dep=%+v)", d.DepType, d)
			continue
		}
		if d.DependsOnID == "sched-m2a" {
			sawTriggerDep = true
		}
	}
	if !sawTriggerDep {
		t.Errorf("missing triggered-by dep to trigger bead sched-m2a (deps=%+v)", daemon.addedDeps)
	}
}

func TestScheduleTriggerSource_EngineIntegration_Dedup(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			scheduleTriggerBead("t1", "0 * * * *", nil),
		},
	}

	now := time.Date(2026, 4, 12, 10, 5, 0, 0, time.UTC)
	source := newScheduleSource(now)

	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})
	engine.notifier = func(_ context.Context, _, _ string, _ string) {}
	engine.RegisterSource(source)

	// First eval: fires.
	engine.Evaluate(context.Background())

	// Second eval: deduped (same fire key).
	engine.Evaluate(context.Background())

	seen := engine.SeenKeys("t1")
	if !seen["2026-04-12T10:00"] {
		t.Error("expected fire key in seen set")
	}
}

func TestScheduleTriggerSource_EngineIntegration_CatchUp(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			scheduleTriggerBead("t1", "0 * * * *", nil),
			// Existing task from previous fire.
			{
				ID:     "gd-prev-task",
				Type:   "task",
				Status: "closed",
				Labels: []string{"trigger:t1"},
				Fields: map[string]string{"schedule_fire_key": "2026-04-12T09:00"},
			},
		},
	}

	now := time.Date(2026, 4, 12, 10, 5, 0, 0, time.UTC)
	source := newScheduleSource(now)

	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})
	engine.RegisterSource(source)

	// CatchUp should populate the seen set.
	engine.CatchUp(context.Background())

	seen := engine.SeenKeys("t1")
	if !seen["2026-04-12T09:00"] {
		t.Error("expected catch-up key in seen set")
	}
}

// TestScheduleTriggerSource_Restart_DoesNotRefireLastMatch reproduces
// gd-pLf5LSHDEh: a trigger-engine restart wipes the in-memory seen set, and
// for an AssignBead-pinned gardener (or any spawn_agent trigger with no
// per-fire task bead) CatchUpKeys finds nothing to re-seed it with — the ONLY
// durable record of "this cron match already fired" is last_match, persisted
// on the trigger bead in the same write as last_run (stampLastRun). This
// simulates a real restart: fire once on engine1, hand-apply the persisted
// field write the mock doesn't auto-reflect (mirroring what the real daemon
// does), then build a brand-new engine2 — fresh in-memory seen/state, exactly
// what a restarted pod gets — on the SAME daemon and assert the identical
// cron match does NOT spawn a second agent.
func TestScheduleTriggerSource_Restart_DoesNotRefireLastMatch(t *testing.T) {
	trig := scheduleTriggerBead("t1", "5 * * * *", map[string]string{
		"action":  "spawn_agent",
		"role":    "crew",
		"project": "gardenio",
		"prompt":  "Work at {{fire_time}}",
	})
	daemon := &mockTriggerDaemon{beads: []*beadsapi.BeadDetail{trig}}

	// Matches the incident's own timestamps: cron '5 * * * *' fires 08:05,
	// engine1 evaluates shortly after (08:11).
	now := time.Date(2026, 9, 21, 8, 11, 0, 0, time.UTC)

	engine1 := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})
	engine1.RegisterSource(newScheduleSource(now))
	engine1.Evaluate(context.Background())

	daemon.mu.Lock()
	if len(daemon.spawnedNames) != 1 {
		daemon.mu.Unlock()
		t.Fatalf("expected 1 spawn on first fire, got %d", len(daemon.spawnedNames))
	}
	// UpdateBeadFields calls accumulate: stampLastRun's write (last_run +
	// last_match) fires first, then stampCheckedOutcome's end-of-cycle
	// last_checked stamp fires after it — so the LAST record for this
	// trigger is not necessarily stampLastRun's. The mock records every call
	// but — unlike UpdateBead — does not reflect any of them back onto the
	// fixture bead, so replay every update for this trigger ID in order by
	// hand, mirroring the real daemon's cumulative read-after-write.
	for i, id := range daemon.updatedIDs {
		if id != trig.ID {
			continue
		}
		for k, v := range daemon.updatedFields[i] {
			trig.Fields[k] = v
		}
	}
	daemon.mu.Unlock()

	if trig.Fields["last_match"] != "2026-09-21T08:05" {
		t.Fatalf("expected last_match persisted as 2026-09-21T08:05, got %q", trig.Fields["last_match"])
	}

	// Simulate the pod restart 12 minutes later: a brand-new engine (fresh
	// in-memory seen map) evaluating the SAME still-unfired-per-cron match
	// (next fire isn't until 09:05), on the same daemon.
	restartNow := time.Date(2026, 9, 21, 8, 23, 0, 0, time.UTC)
	engine2 := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})
	engine2.RegisterSource(newScheduleSource(restartNow))
	engine2.CatchUp(context.Background())
	engine2.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.spawnedNames) != 1 {
		t.Errorf("post-restart re-evaluation of an already-fired match spawned again: total spawns = %d, want 1", len(daemon.spawnedNames))
	}
}

func TestScheduleTriggerSource_EngineIntegration_ConditionBlocksFire(t *testing.T) {
	// Gate is present — condition says gate must be absent — should not fire.
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			scheduleTriggerBead("t1", "0 * * * *", map[string]string{
				"conditions": `[{"type":"gate","op":"absent","value":"scheduler-pause"}]`,
			}),
			{ID: "g1", Type: "gate", Status: "open", Title: "scheduler-pause"},
		},
	}

	now := time.Date(2026, 4, 12, 10, 5, 0, 0, time.UTC)
	source := newScheduleSource(now)

	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})

	var notified bool
	engine.notifier = func(_ context.Context, _, _ string, _ string) {
		notified = true
	}

	engine.RegisterSource(source)
	engine.Evaluate(context.Background())

	if notified {
		t.Error("expected no notification (gate condition blocked)")
	}
}

func TestScheduleTriggerSource_NoConditions(t *testing.T) {
	// No conditions = unconditional fire when cron matches.
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			scheduleTriggerBead("t1", "0 * * * *", nil),
		},
	}

	now := time.Date(2026, 4, 12, 10, 5, 0, 0, time.UTC)
	source := newScheduleSource(now)

	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})

	var notified bool
	engine.notifier = func(_ context.Context, _, _ string, _ string) { notified = true }
	engine.RegisterSource(source)
	engine.Evaluate(context.Background())

	if !notified {
		t.Error("expected notification (no conditions = always fire)")
	}
}

func TestScheduleTriggerSource_TemplateVars(t *testing.T) {
	now := time.Date(2026, 4, 12, 10, 5, 0, 0, time.UTC)
	source := newScheduleSource(now)
	source.daemon = &mockTriggerDaemon{}

	trig := TriggerDef{
		ID:            "t1",
		Title:         "Daily Check",
		Source:        "schedule",
		Action:        TriggerActionNotify,
		MaxConcurrent: 10,
		SourceData: scheduleTriggerData{
			Cron:     "0 * * * *",
			Timezone: "UTC",
		},
	}

	matches, err := source.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected 1 match, got %d", len(matches))
	}

	m := matches[0]
	if m.TemplateVars["{{trigger_id}}"] != "t1" {
		t.Errorf("trigger_id = %q", m.TemplateVars["{{trigger_id}}"])
	}
	if m.TemplateVars["{{trigger_title}}"] != "Daily Check" {
		t.Errorf("trigger_title = %q", m.TemplateVars["{{trigger_title}}"])
	}
	if m.TemplateVars["{{cron}}"] != "0 * * * *" {
		t.Errorf("cron = %q", m.TemplateVars["{{cron}}"])
	}
	if m.TemplateVars["{{timezone}}"] != "UTC" {
		t.Errorf("timezone = %q", m.TemplateVars["{{timezone}}"])
	}
	if m.TemplateVars["{{fire_time}}"] == "" {
		t.Error("fire_time should not be empty")
	}

	// Check fields on the match.
	if m.Fields["schedule_fire_key"] == "" {
		t.Error("schedule_fire_key should not be empty")
	}
	if m.Fields["schedule_cron"] != "0 * * * *" {
		t.Errorf("schedule_cron = %q", m.Fields["schedule_cron"])
	}

	// Check labels.
	hasLabel := false
	for _, l := range m.Labels {
		if l == "source:schedule" {
			hasLabel = true
		}
	}
	if !hasLabel {
		t.Error("expected source:schedule label")
	}
}

// --- Wedged-trigger self-heal (ka-0000006681) ---

// wedgedTriggerBead builds a schedule trigger bead stuck in status=in_progress
// with the given last_checked, simulating a gardener whose spawned agent claimed
// the trigger bead itself and died without releasing it.
func wedgedTriggerBead(id, lastChecked string) *beadsapi.BeadDetail {
	b := scheduleTriggerBead(id, "*/30 * * * *", map[string]string{
		"role":   "gardener",
		"action": "spawn_agent",
	})
	b.Status = "in_progress"
	b.Assignee = "gardenio-dead-pod-xyz"
	b.Fields["last_checked"] = lastChecked
	return b
}

// TestScheduleTriggerSource_HealsWedgedTrigger verifies the core fix: a schedule
// trigger stuck in_progress with a stale last_checked is released back to open
// and then re-enters the normal load on the same pass.
func TestScheduleTriggerSource_HealsWedgedTrigger(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	stale := now.Add(-3 * time.Hour).Format(time.RFC3339)

	wedged := wedgedTriggerBead("gd-janitor", stale)
	daemon := &mockTriggerDaemon{beads: []*beadsapi.BeadDetail{wedged}}

	source := newScheduleSource(now)
	triggers, err := source.LoadTriggers(context.Background(), daemon)
	if err != nil {
		t.Fatalf("LoadTriggers() error: %v", err)
	}

	// The bead was released to open.
	if len(daemon.updatedBeadIDs) != 1 || daemon.updatedBeadIDs[0] != "gd-janitor" {
		t.Fatalf("expected UpdateBead on gd-janitor, got %v", daemon.updatedBeadIDs)
	}
	req := daemon.updatedBeadReqs[0]
	if req.Status == nil || *req.Status != "open" {
		t.Errorf("expected status reset to open, got %v", req.Status)
	}
	if req.Assignee == nil || *req.Assignee != "" {
		t.Errorf("expected assignee cleared, got %v", req.Assignee)
	}

	// And the now-open trigger re-enters the rotation on the same pass.
	if len(triggers) != 1 || triggers[0].ID != "gd-janitor" {
		t.Fatalf("expected healed trigger to load, got %d: %+v", len(triggers), triggers)
	}

	// An audit comment was stamped on the bead.
	found := false
	for _, id := range daemon.commentedIDs {
		if id == "gd-janitor" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected an auto-heal audit comment on gd-janitor, got %v", daemon.commentedIDs)
	}
}

// TestScheduleTriggerSource_DoesNotHealFreshInProgress verifies the grace
// window: an in_progress trigger whose last_checked is recent is left alone so
// the heal never races a legitimate transient claim.
func TestScheduleTriggerSource_DoesNotHealFreshInProgress(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	fresh := now.Add(-2 * time.Minute).Format(time.RFC3339)

	daemon := &mockTriggerDaemon{beads: []*beadsapi.BeadDetail{wedgedTriggerBead("gd-fresh", fresh)}}

	source := newScheduleSource(now)
	if _, err := source.LoadTriggers(context.Background(), daemon); err != nil {
		t.Fatalf("LoadTriggers() error: %v", err)
	}

	if len(daemon.updatedBeadIDs) != 0 {
		t.Errorf("expected no heal for fresh in_progress trigger, got %v", daemon.updatedBeadIDs)
	}
}

// TestScheduleTriggerSource_DoesNotHealDisabledTrigger verifies a disabled
// (operator-paused) in_progress trigger is left exactly as parked.
func TestScheduleTriggerSource_DoesNotHealDisabledTrigger(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	stale := now.Add(-5 * time.Hour).Format(time.RFC3339)

	wedged := wedgedTriggerBead("gd-paused", stale)
	wedged.Fields["enabled"] = "false"
	daemon := &mockTriggerDaemon{beads: []*beadsapi.BeadDetail{wedged}}

	source := newScheduleSource(now)
	if _, err := source.LoadTriggers(context.Background(), daemon); err != nil {
		t.Fatalf("LoadTriggers() error: %v", err)
	}

	if len(daemon.updatedBeadIDs) != 0 {
		t.Errorf("expected no heal for disabled trigger, got %v", daemon.updatedBeadIDs)
	}
}

// TestScheduleTriggerSource_HealsMissingLastChecked verifies an in_progress
// trigger with no last_checked at all is treated as long-abandoned and healed.
func TestScheduleTriggerSource_HealsMissingLastChecked(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)

	// Represent "no last_checked" as an explicit empty string rather than
	// deleting the key. The mock daemon's ListBeadsFiltered runs
	// ensurePastLastChecked on every trigger, which re-injects a real
	// wall-clock timestamp (now-1h) whenever the key is *absent* — that fresh
	// value would make the staleness gate defer the heal and flake this test.
	// An explicit "" is preserved by the mock and is exactly what the real
	// daemon surfaces for a missing field (map zero value), so the heal gate
	// still evaluates wedgedTriggerIsStale("", pinnedNow) == true. See
	// ensurePastLastChecked's doc comment in jira_triggers_test.go.
	wedged := wedgedTriggerBead("gd-nolc", "")
	wedged.Fields["last_checked"] = ""
	daemon := &mockTriggerDaemon{beads: []*beadsapi.BeadDetail{wedged}}

	source := newScheduleSource(now)
	if _, err := source.LoadTriggers(context.Background(), daemon); err != nil {
		t.Fatalf("LoadTriggers() error: %v", err)
	}

	if len(daemon.updatedBeadIDs) != 1 || daemon.updatedBeadIDs[0] != "gd-nolc" {
		t.Fatalf("expected heal for missing-last_checked trigger, got %v", daemon.updatedBeadIDs)
	}
}

// stalledOpenGardenerBead builds an OPEN + enabled schedule gardener trigger stuck
// in the no_progress_for bare-reconcile deadlock (ka-0000003265): its gardener
// block carries last_resume_reason="no_progress_for" and runs_spawned (0) ≤
// last_resume_runs_spawned (0), the exact fingerprint healStalledOpenTriggers
// targets. last_checked is set by the caller to control staleness.
func stalledOpenGardenerBead(id, lastChecked string) *beadsapi.BeadDetail {
	capJSON := `{"run_budget":10,"runs_spawned":0,"resumes_without_progress":1,` +
		`"last_resume_reason":"no_progress_for","last_resume_runs_spawned":0,` +
		`"progress":{"surface":{"in_prime":true}},` +
		`"progress_state":{"runs":[{"agent_id":"gd-old-a","progress_score":0},` +
		`{"agent_id":"gd-old-b","progress_score":0},{"agent_id":"gd-old-c","progress_score":0}]}}`
	b := scheduleTriggerBead(id, "7,37 * * * *", map[string]string{
		"role":     "gardener",
		"action":   "spawn_agent",
		"gardener": capJSON,
	})
	b.Status = "open"
	b.Fields["last_checked"] = lastChecked
	return b
}

// TestScheduleTriggerSource_HealsStalledOpenTrigger is the core acceptance test
// for ka-0000003265: an open + enabled gardener trigger frozen in the
// no_progress_for deadlock is durably reset so it can fire a fresh window.
func TestScheduleTriggerSource_HealsStalledOpenTrigger(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	// cron "7,37 * * * *" → 30m cadence; stale threshold = 6×30m = 3h (floored at
	// 1h). 47h ago is unambiguously stale (mirrors the live repro on gd-BiZLvj0e1P).
	stale := now.Add(-47 * time.Hour).Format(time.RFC3339)

	daemon := &mockTriggerDaemon{beads: []*beadsapi.BeadDetail{stalledOpenGardenerBead("gd-frozen", stale)}}
	source := newScheduleSource(now)

	if _, err := source.LoadTriggers(context.Background(), daemon); err != nil {
		t.Fatalf("LoadTriggers() error: %v", err)
	}

	// A gardener UpdateBeadFields write recovered the trigger.
	cap := gardenerSpecFromUpdate(t, daemon, "gd-frozen")

	// The durable reset: ProgressState cleared and RunsFinalizedAfter stamped so
	// gardenerFinalizeRunsFromIterations cannot rebuild the stale zero tail.
	if len(cap.ProgressState) != 0 && string(cap.ProgressState) != "null" {
		t.Errorf("expected ProgressState cleared, got %s", cap.ProgressState)
	}
	if cap.RunsFinalizedAfter.IsZero() {
		t.Errorf("expected RunsFinalizedAfter stamped, got zero")
	}
	if !cap.RunsFinalizedAfter.Equal(now) {
		t.Errorf("expected RunsFinalizedAfter=%v, got %v", now, cap.RunsFinalizedAfter)
	}
	// Resume bookkeeping cleared so the fingerprint no longer matches (idempotent).
	if cap.LastResumeReason != "" {
		t.Errorf("expected LastResumeReason cleared, got %q", cap.LastResumeReason)
	}
	if cap.ResumesWithoutProgress != 0 {
		t.Errorf("expected ResumesWithoutProgress reset, got %d", cap.ResumesWithoutProgress)
	}

	// last_checked was re-stamped fresh so the liveness sweep re-arms.
	found := false
	daemon.mu.Lock()
	for i := range daemon.updatedIDs {
		if daemon.updatedIDs[i] == "gd-frozen" {
			if lc := daemon.updatedFields[i]["last_checked"]; lc == now.UTC().Format(time.RFC3339) {
				found = true
			}
		}
	}
	daemon.mu.Unlock()
	if !found {
		t.Errorf("expected last_checked re-stamped to now on gd-frozen")
	}

	// An audit comment records the recovery.
	commented := false
	for _, id := range daemon.commentedIDs {
		if id == "gd-frozen" {
			commented = true
		}
	}
	if !commented {
		t.Errorf("expected an auto-heal audit comment on gd-frozen, got %v", daemon.commentedIDs)
	}
}

// TestScheduleTriggerSource_LoadReusesHealList pins gd-6PXLuR3juD: the load
// reuses the stalled-open heal's open trigger:schedule list when the heal
// wrote nothing, and lists afresh after a heal so it sees the reset gardener.
func TestScheduleTriggerSource_LoadReusesHealList(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name        string
		lastChecked string
		wantLists   int
	}{
		{"nothing healed", now.Add(-2 * time.Minute).Format(time.RFC3339), 1},
		{"gardener healed", now.Add(-47 * time.Hour).Format(time.RFC3339), 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			daemon := &mockTriggerDaemon{beads: []*beadsapi.BeadDetail{stalledOpenGardenerBead("gd-g", tc.lastChecked)}}
			lists := 0
			daemon.listBeadsHook = func(_ int, q beadsapi.ListBeadsQuery) error {
				if slices.Equal(q.Labels, []string{"trigger:schedule"}) && slices.Equal(q.Statuses, []string{"open"}) {
					lists++
				}
				return nil
			}
			triggers, err := newScheduleSource(now).LoadTriggers(context.Background(), daemon)
			if err != nil {
				t.Fatalf("LoadTriggers() error: %v", err)
			}
			if len(triggers) != 1 {
				t.Fatalf("want the gardener loaded, got %d triggers", len(triggers))
			}
			if lists != tc.wantLists {
				t.Errorf("open trigger:schedule lists = %d, want %d", lists, tc.wantLists)
			}
		})
	}
}

// TestScheduleTriggerSource_StalledHeal_Idempotent verifies a gardener that was
// already healed (LastResumeReason cleared by the reset) no longer matches the
// deadlock fingerprint, so a second pass performs no further writes/comments.
func TestScheduleTriggerSource_StalledHeal_Idempotent(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	healed := stalledOpenGardenerBead("gd-healed", now.Add(-47*time.Hour).Format(time.RFC3339))
	// Simulate a prior heal: fingerprint fields cleared, last_checked still frozen.
	healed.Fields["gardener"] = `{"run_budget":10,"runs_spawned":0,"last_resume_reason":"",` +
		`"progress":{"surface":{"in_prime":true}}}`

	daemon := &mockTriggerDaemon{beads: []*beadsapi.BeadDetail{healed}}
	source := newScheduleSource(now)
	if _, err := source.LoadTriggers(context.Background(), daemon); err != nil {
		t.Fatalf("LoadTriggers() error: %v", err)
	}

	for _, id := range daemon.updatedIDs {
		if id == "gd-healed" {
			t.Errorf("expected no re-heal of an already-reset gardener, got update on %s", id)
		}
	}
}

// TestScheduleTriggerSource_StalledHeal_SkipsNonDeadlocked verifies the heal
// leaves alone open gardeners that do NOT match the deadlock fingerprint: a fresh
// last_checked, a different resume reason, or a gardener that has fired since the
// re-arm (runs_spawned advanced — e.g. one_spawn_at_a_time waiting on a live run).
func TestScheduleTriggerSource_StalledHeal_SkipsNonDeadlocked(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	stale := now.Add(-47 * time.Hour).Format(time.RFC3339)

	fresh := stalledOpenGardenerBead("gd-fresh", now.Add(-2*time.Minute).Format(time.RFC3339))

	otherReason := stalledOpenGardenerBead("gd-maxruns", stale)
	otherReason.Fields["gardener"] = `{"run_budget":10,"runs_spawned":0,"last_resume_reason":"max_runs",` +
		`"last_resume_runs_spawned":0,"progress":{"surface":{"in_prime":true}}}`

	fired := stalledOpenGardenerBead("gd-fired", stale)
	// runs_spawned (5) > last_resume_runs_spawned (2): the gardener fired after the
	// re-arm, so it is not deadlocked (this is the one_spawn_at_a_time shape).
	fired.Fields["gardener"] = `{"run_budget":10,"runs_spawned":5,"last_resume_reason":"no_progress_for",` +
		`"last_resume_runs_spawned":2,"progress":{"surface":{"in_prime":true}}}`

	daemon := &mockTriggerDaemon{beads: []*beadsapi.BeadDetail{fresh, otherReason, fired}}
	source := newScheduleSource(now)
	if _, err := source.LoadTriggers(context.Background(), daemon); err != nil {
		t.Fatalf("LoadTriggers() error: %v", err)
	}

	for _, id := range daemon.updatedBeadIDs {
		t.Errorf("unexpected UpdateBead (status change) on %s", id)
	}
	for _, id := range daemon.updatedIDs {
		t.Errorf("expected no gardener heal write, got update on %s", id)
	}
}

// TestScheduleTriggerSource_StalledHeal_SkipsDisabledDormantNonGardener verifies
// the guardrails: operator-paused (enabled=false), dormant-on-escalation
// (frozen by design), and non-gardener triggers are never touched even when stale.
func TestScheduleTriggerSource_StalledHeal_SkipsDisabledDormantNonGardener(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	stale := now.Add(-47 * time.Hour).Format(time.RFC3339)

	disabled := stalledOpenGardenerBead("gd-disabled", stale)
	disabled.Fields["enabled"] = "false"

	dormant := stalledOpenGardenerBead("gd-dormant", stale)
	dormant.Fields["dormant_on_escalation"] = "true"

	// A stale open source-only trigger (no gardener block) must be ignored — it
	// fires reactively and may legitimately sit idle for days.
	plain := scheduleTriggerBead("gd-plain", "7,37 * * * *", map[string]string{"action": "notify"})
	plain.Fields["last_checked"] = stale

	daemon := &mockTriggerDaemon{beads: []*beadsapi.BeadDetail{disabled, dormant, plain}}
	source := newScheduleSource(now)
	if _, err := source.LoadTriggers(context.Background(), daemon); err != nil {
		t.Fatalf("LoadTriggers() error: %v", err)
	}

	for _, id := range daemon.updatedIDs {
		t.Errorf("expected no heal for disabled/dormant/non-gardener, got update on %s", id)
	}
}

// TestScheduleTriggerSource_StalledHeal_SkipsBlockedByOpenDecision is the
// blocking-review guardrail for ka-0000003265. A gardener that re-armed on
// no_progress_for and THEN became blocked_by an open decision keeps the FULL
// deadlock fingerprint indefinitely (LastResumeReason=="no_progress_for",
// RunsSpawned<=LastResumeRunsSpawned, last_checked frozen — the blocked_by gate
// in triggers.go returns before the pre-fire check, so nothing advances
// RunsSpawned or clears the marker). But it is NOT dark — it is healthily waiting
// on a human. The heal must skip it exactly as the read-only liveness sweep does
// (gardener_liveness.go:236-256): NO gardener UpdateBeadFields write, NO audit
// comment. Mirrors the live hazard the reviewer flagged before merge.
func TestScheduleTriggerSource_StalledHeal_SkipsBlockedByOpenDecision(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	stale := now.Add(-47 * time.Hour).Format(time.RFC3339)

	// Full deadlock fingerprint + stale last_checked — would be healed if the
	// blocked_by skip were absent.
	blocked := stalledOpenGardenerBead("gd-blocked", stale)
	blocked.Fields["blocked_by"] = "gd-open-decision"

	// The referenced decision is still OPEN, so the gardener is legitimately parked.
	decision := &beadsapi.BeadDetail{
		ID:     "gd-open-decision",
		Type:   "decision",
		Status: "open",
		Title:  "operator: pick session strategy",
	}

	daemon := &mockTriggerDaemon{beads: []*beadsapi.BeadDetail{blocked, decision}}
	source := newScheduleSource(now)
	if _, err := source.LoadTriggers(context.Background(), daemon); err != nil {
		t.Fatalf("LoadTriggers() error: %v", err)
	}

	// No gardener heal write of any kind.
	for _, id := range daemon.updatedIDs {
		if id == "gd-blocked" {
			t.Errorf("expected NO heal write on a blocked_by gardener, got UpdateBeadFields on %s", id)
		}
	}
	// No audit comment either.
	for _, id := range daemon.commentedIDs {
		if id == "gd-blocked" {
			t.Errorf("expected NO auto-heal comment on a blocked_by gardener, got comment on %s", id)
		}
	}
}

// TestScheduleTriggerSource_StalledHeal_HealsWhenBlockerClosed is the companion
// to the guardrail above: once the referenced decision is resolved (closed), the
// blocked_by skip no longer applies, and a gardener still carrying the deadlock
// fingerprint with a frozen last_checked IS recovered. This proves the skip keys
// off the LIVE status of the blocker (mirroring triggerBlockedBy/isOpenStatus),
// not merely the presence of a blocked_by field.
func TestScheduleTriggerSource_StalledHeal_HealsWhenBlockerClosed(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	stale := now.Add(-47 * time.Hour).Format(time.RFC3339)

	blocked := stalledOpenGardenerBead("gd-was-blocked", stale)
	blocked.Fields["blocked_by"] = "gd-closed-decision"

	// The referenced decision is CLOSED, so it no longer blocks — the deadlock
	// fingerprint is genuine and the heal must fire.
	decision := &beadsapi.BeadDetail{
		ID:     "gd-closed-decision",
		Type:   "decision",
		Status: "closed",
		Title:  "operator: resolved",
	}

	daemon := &mockTriggerDaemon{beads: []*beadsapi.BeadDetail{blocked, decision}}
	source := newScheduleSource(now)
	if _, err := source.LoadTriggers(context.Background(), daemon); err != nil {
		t.Fatalf("LoadTriggers() error: %v", err)
	}

	cap := gardenerSpecFromUpdate(t, daemon, "gd-was-blocked")
	if cap.RunsFinalizedAfter.IsZero() {
		t.Errorf("expected RunsFinalizedAfter stamped (blocker closed ⇒ genuine deadlock), got zero")
	}
	if cap.LastResumeReason != "" {
		t.Errorf("expected LastResumeReason cleared, got %q", cap.LastResumeReason)
	}
}

// TestScheduleTriggerSource_StalledHeal_SkipsOneSpawnWithLiveAgent verifies the
// second no-stamp gate: a one_spawn_at_a_time gardener whose prior LastAgentID is
// still running freezes last_checked (the one_spawn pre-fire gate returns without
// stamping) but is not dark. Even when it carries the no_progress_for fingerprint
// and a stale last_checked, the heal must leave it untouched — mirroring
// gardener_liveness.go's gardenerAgentStillRunning skip.
func TestScheduleTriggerSource_StalledHeal_SkipsOneSpawnWithLiveAgent(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	stale := now.Add(-47 * time.Hour).Format(time.RFC3339)

	// Deadlock fingerprint (runs_spawned 0 ≤ last_resume_runs_spawned 0) PLUS
	// one_spawn_at_a_time with a live prior agent.
	oneSpawn := stalledOpenGardenerBead("gd-onespawn", stale)
	oneSpawn.Fields["gardener"] = `{"run_budget":10,"runs_spawned":0,"resumes_without_progress":1,` +
		`"last_resume_reason":"no_progress_for","last_resume_runs_spawned":0,` +
		`"one_spawn_at_a_time":true,"last_agent_id":"gd-live-agent",` +
		`"progress":{"surface":{"in_prime":true}}}`

	liveAgent := &beadsapi.BeadDetail{
		ID:     "gd-live-agent",
		Type:   "agent",
		Status: "in_progress",
		Title:  "still running",
	}

	daemon := &mockTriggerDaemon{beads: []*beadsapi.BeadDetail{oneSpawn, liveAgent}}
	source := newScheduleSource(now)
	if _, err := source.LoadTriggers(context.Background(), daemon); err != nil {
		t.Fatalf("LoadTriggers() error: %v", err)
	}

	for _, id := range daemon.updatedIDs {
		if id == "gd-onespawn" {
			t.Errorf("expected NO heal write on a one_spawn gardener with a live agent, got update on %s", id)
		}
	}
	for _, id := range daemon.commentedIDs {
		if id == "gd-onespawn" {
			t.Errorf("expected NO auto-heal comment on a one_spawn gardener with a live agent, got comment on %s", id)
		}
	}
}

// TestScheduleTriggerSource_HealsWeeklyCronStalledGardener is the regression
// test for ka-0000005387: with the 6× factor unbounded, a WEEKLY cron's stale
// threshold was 42 days, so the live victim (ka-0000005781, cron "11 8 * * 1")
// sat in the ka-0000003265 deadlock 19 days — two missed Monday fires — and
// the heal declined to act. The stalledOpenTriggerMaxStale cap (14d) must make
// that gardener heal.
func TestScheduleTriggerSource_HealsWeeklyCronStalledGardener(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	frozen := stalledOpenGardenerBead("gd-weekly", now.Add(-19*24*time.Hour).Format(time.RFC3339))
	frozen.Fields["cron"] = "11 8 * * 1" // weekly: 6×cadence = 42d without the cap

	daemon := &mockTriggerDaemon{beads: []*beadsapi.BeadDetail{frozen}}
	source := newScheduleSource(now)
	if _, err := source.LoadTriggers(context.Background(), daemon); err != nil {
		t.Fatalf("LoadTriggers() error: %v", err)
	}

	cap := gardenerSpecFromUpdate(t, daemon, "gd-weekly")
	if cap.LastResumeReason != "" {
		t.Errorf("expected LastResumeReason cleared by the heal, got %q", cap.LastResumeReason)
	}
	if !cap.RunsFinalizedAfter.Equal(now) {
		t.Errorf("expected RunsFinalizedAfter=%v, got %v", now, cap.RunsFinalizedAfter)
	}
}

// TestScheduleTriggerSource_WeeklyCronFresherThanCapNotHealed verifies the cap
// does not make the heal aggressive: a weekly gardener frozen for LESS than
// stalledOpenTriggerMaxStale is still left alone.
func TestScheduleTriggerSource_WeeklyCronFresherThanCapNotHealed(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	frozen := stalledOpenGardenerBead("gd-weekly-fresh", now.Add(-13*24*time.Hour).Format(time.RFC3339))
	frozen.Fields["cron"] = "11 8 * * 1"

	daemon := &mockTriggerDaemon{beads: []*beadsapi.BeadDetail{frozen}}
	source := newScheduleSource(now)
	if _, err := source.LoadTriggers(context.Background(), daemon); err != nil {
		t.Fatalf("LoadTriggers() error: %v", err)
	}

	for _, id := range daemon.updatedIDs {
		if id == "gd-weekly-fresh" {
			t.Errorf("expected no heal below the 14d cap, got update on %s", id)
		}
	}
}

// TestStalledOpenTriggerIsStale exercises the threshold math directly: the 6×
// cadence factor, the 1h floor for tight crons, and the 14d cap for slow crons
// (ka-0000005387).
func TestStalledOpenTriggerIsStale(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		cron string
		age  time.Duration
		want bool
	}{
		// 30m cadence → threshold 6×30m = 3h (cap irrelevant).
		{"tight cron between ticks is fresh", "7,37 * * * *", 2 * time.Hour, false},
		{"tight cron past 6x cadence is stale", "7,37 * * * *", 4 * time.Hour, true},
		// Weekly cadence → 6×7d = 42d uncapped; the 14d cap governs.
		{"weekly cron below cap is fresh", "11 8 * * 1", 13 * 24 * time.Hour, false},
		{"weekly cron at cap is stale", "11 8 * * 1", 14 * 24 * time.Hour, true},
		{"weekly cron live victim (19d) is stale", "11 8 * * 1", 19 * 24 * time.Hour, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bead := scheduleTriggerBead("gd-thresh", tc.cron, nil)
			lastChecked := now.Add(-tc.age).Format(time.RFC3339)
			if got := stalledOpenTriggerIsStale(lastChecked, bead, now); got != tc.want {
				t.Errorf("stalledOpenTriggerIsStale(cron=%q, age=%v) = %v, want %v", tc.cron, tc.age, got, tc.want)
			}
		})
	}
}

func TestWedgedTriggerIsStale(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name        string
		lastChecked string
		want        bool
	}{
		{"empty is stale", "", true},
		{"unparseable is stale", "not-a-time", true},
		{"old is stale", now.Add(-time.Hour).Format(time.RFC3339), true},
		{"exactly threshold is stale", now.Add(-wedgedTriggerStaleAfter).Format(time.RFC3339), true},
		{"recent is fresh", now.Add(-time.Minute).Format(time.RFC3339), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := wedgedTriggerIsStale(tc.lastChecked, now); got != tc.want {
				t.Errorf("wedgedTriggerIsStale(%q) = %v, want %v", tc.lastChecked, got, tc.want)
			}
		})
	}
}
