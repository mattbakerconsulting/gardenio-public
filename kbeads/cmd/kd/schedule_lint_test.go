package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestRejectScheduleCreate_Schedule(t *testing.T) {
	err := rejectScheduleCreate("schedule")
	if err == nil {
		t.Fatalf("expected error for type=schedule, got nil")
	}
	if !errors.Is(err, errScheduleCreateRejected) {
		t.Errorf("expected errScheduleCreateRejected, got %v", err)
	}
	msg := err.Error()
	for _, want := range []string{"no longer supported", "Phase 3b/D10", "type=trigger", "triggers.md"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message missing %q, got: %s", want, msg)
		}
	}
}

func TestRejectScheduleCreate_OtherTypes(t *testing.T) {
	for _, bt := range []string{"task", "trigger", "agent", "epic", "feature", ""} {
		if err := rejectScheduleCreate(bt); err != nil {
			t.Errorf("type=%q should not be rejected, got: %v", bt, err)
		}
	}
}

// The TestLintScheduleFields_* tests guard ka-0000006296: touching a
// type=schedule bead with legacy wake-spec fields and no triggers: array
// warns, since those fields no longer fire.
func TestLintScheduleFields_NotSchedule(t *testing.T) {
	var buf bytes.Buffer
	n := lintScheduleFields(&buf, "task", []string{"cron=*/5 * * * *"})
	if n != 0 || buf.Len() != 0 {
		t.Fatalf("non-schedule should not warn, got n=%d output=%q", n, buf.String())
	}
}

func TestLintScheduleFields_LegacyFieldsWithoutTriggers_Warn(t *testing.T) {
	var buf bytes.Buffer
	n := lintScheduleFields(&buf, "schedule", []string{
		"cron=*/10 * * * *",
		"trigger_mode=cron",
	})
	if n != 1 {
		t.Fatalf("expected 1 warning, got %d:\n%s", n, buf.String())
	}
	out := buf.String()
	for _, want := range []string{
		"deprecated schedule fields",
		"cron",
		"trigger_mode",
		"D10",
		"type=trigger",
		"triggers.md",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("warning missing %q, got:\n%s", want, out)
		}
	}
}

func TestLintScheduleFields_LegacyFieldsWithTriggers_NoWarn(t *testing.T) {
	var buf bytes.Buffer
	n := lintScheduleFields(&buf, "schedule", []string{
		"cron=*/10 * * * *",
		"triggers=[{\"kind\":\"cron\",\"cron\":\"*/10 * * * *\"}]",
	})
	if n != 0 {
		t.Fatalf("legacy fields alongside triggers should not warn, got:\n%s", buf.String())
	}
}

func TestLintScheduleFields_WatchFields_Warn(t *testing.T) {
	var buf bytes.Buffer
	n := lintScheduleFields(&buf, "schedule", []string{
		"watch_project=gardenio",
		"watch_labels=foo",
	})
	if n != 1 {
		t.Fatalf("expected 1 warning, got %d:\n%s", n, buf.String())
	}
	out := buf.String()
	if !strings.Contains(out, "watch_project") || !strings.Contains(out, "watch_labels") {
		t.Errorf("warning should list watch_* fields, got:\n%s", out)
	}
}

func TestLintScheduleFields_EmptyValuesIgnored(t *testing.T) {
	var buf bytes.Buffer
	n := lintScheduleFields(&buf, "schedule", []string{
		"cron=",
		"trigger_mode=",
	})
	if n != 0 {
		t.Fatalf("empty values should not count as legacy-set, got %d:\n%s", n, buf.String())
	}
}

func TestLintScheduleFields_NoLegacyFields_NoWarn(t *testing.T) {
	var buf bytes.Buffer
	n := lintScheduleFields(&buf, "schedule", []string{
		"slack_channel=#foo",
		"project=gardenio",
	})
	if n != 0 {
		t.Fatalf("unrelated field updates should not warn, got %d:\n%s", n, buf.String())
	}
}
