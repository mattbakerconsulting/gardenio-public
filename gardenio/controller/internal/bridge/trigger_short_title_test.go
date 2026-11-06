package bridge

import (
	"context"
	"log/slog"
	"testing"
	"unicode/utf8"

	"gardenio/controller/internal/shorttitle"
)

// TestCreateTaskBead_ScheduleRunShortTitle: a schedule run authors its short
// title as `<trigger title, clipped> HH:MM` from the fireKey instead of leaving
// the bridge to guess it from the `[…] Schedule: …` title (plan gd-YZATAIOhfW
// Task 6).
func TestCreateTaskBead_ScheduleRunShortTitle(t *testing.T) {
	daemon := &mockTriggerDaemon{}
	engine := NewTriggerEngine(TriggerEngineConfig{Daemon: daemon, Logger: slog.Default()})

	trig := TriggerDef{
		ID:      "gd-sched-1",
		Title:   "cicd watchdog: 20-min GitHub Actions stall",
		Source:  "schedule",
		Project: "gardenio",
	}
	match := TriggerMatch{
		Key:   "2026-10-11T15:07",
		Title: "Schedule: cicd watchdog: 20-min GitHub Actions stall",
	}
	if _, err := engine.createTaskBead(context.Background(), trig, match); err != nil {
		t.Fatalf("createTaskBead: %v", err)
	}
	if len(daemon.createdBeads) != 1 {
		t.Fatalf("expected 1 bead, got %d", len(daemon.createdBeads))
	}
	got := daemon.createdBeads[0].ShortTitle
	if want := "cicd watchdog: 20-min GitHub… 15:07"; got != want {
		t.Errorf("ShortTitle = %q, want %q", got, want)
	}
	if n := utf8.RuneCountInString(got); n > shorttitle.Max {
		t.Errorf("ShortTitle %q is %d runes, over %d", got, n, shorttitle.Max)
	}
}

// TestCreateTaskBead_NonScheduleLeavesShortTitleToChokePoint: a non-schedule
// source authors nothing here; beadsapi.CreateBeadDetail derives it.
func TestCreateTaskBead_NonScheduleLeavesShortTitleToChokePoint(t *testing.T) {
	daemon := &mockTriggerDaemon{}
	engine := NewTriggerEngine(TriggerEngineConfig{Daemon: daemon, Logger: slog.Default()})

	trig := TriggerDef{ID: "t1", Title: "Jira intake", Source: "jira", Project: "gardenio"}
	match := TriggerMatch{Key: "JIRA-123", Title: "Test issue"}
	if _, err := engine.createTaskBead(context.Background(), trig, match); err != nil {
		t.Fatalf("createTaskBead: %v", err)
	}
	if got := daemon.createdBeads[0].ShortTitle; got != "" {
		t.Errorf("ShortTitle = %q, want empty (choke point derives it)", got)
	}
}
