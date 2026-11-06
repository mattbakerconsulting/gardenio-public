package bridge

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/scheduler"
)

// mirrorTriggerBead constructs a `type=trigger` mirror bead with the canonical
// shape: label `schedule-mirror` + per-mirror linker `schedule:<sid>`, plus a
// non-empty `triggers` array (single cron entry by default).
func mirrorTriggerBead(id, sid, cron string, fields map[string]string) *beadsapi.BeadDetail {
	f := map[string]string{
		"schedule_id":  sid,
		"source":       "schedule-mirror",
		"action":       "spawn_agent",
		"enabled":      "true",
		"triggers":     `[{"kind":"cron","cron":"` + cron + `"}]`,
		"last_checked": time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339),
	}
	maps.Copy(f, fields)
	return &beadsapi.BeadDetail{
		ID:     id,
		Type:   "trigger",
		Status: "open",
		Title:  "schedule-mirror: " + id,
		Labels: []string{scheduleMirrorMarkerLabel, "schedule:" + sid},
		Fields: f,
	}
}

func newMirrorSource(now time.Time) *ScheduleMirrorTriggerSource {
	s := NewScheduleMirrorTriggerSource(slog.Default(), nil)
	s.nowFunc = func() time.Time { return now }
	return s
}

func TestScheduleMirrorTriggerSource_Name(t *testing.T) {
	s := NewScheduleMirrorTriggerSource(nil, nil)
	if s.Name() != "schedule-mirror" {
		t.Errorf("Name() = %q, want %q", s.Name(), "schedule-mirror")
	}
}

func TestScheduleMirrorTriggerSource_SkipRuntimeBootstrap(t *testing.T) {
	s := NewScheduleMirrorTriggerSource(nil, nil)
	if !s.SkipRuntimeBootstrap() {
		t.Error("expected SkipRuntimeBootstrap to return true")
	}
}

// TestScheduleMirrorTriggerSource_LoadTriggers_FiresAllEnabledMirrors confirms
// that after Phase 3b/D10 removed type=schedule firing, every enabled mirror
// with a non-empty triggers[] is loaded — there is no longer any "is the
// source schedule still open?" gate to skip mirrors against.
func TestScheduleMirrorTriggerSource_LoadTriggers_FiresAllEnabledMirrors(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			mirrorTriggerBead("gd-mirror-a", "gd-src-a", "*/30 * * * *", nil),
			mirrorTriggerBead("gd-mirror-b", "gd-src-b", "*/30 * * * *", nil),
		},
	}

	src := NewScheduleMirrorTriggerSource(slog.Default(), nil)
	triggers, err := src.LoadTriggers(context.Background(), daemon)
	if err != nil {
		t.Fatalf("LoadTriggers: %v", err)
	}
	if len(triggers) != 2 {
		t.Fatalf("expected 2 triggers (both mirrors), got %d (ids=%v)",
			len(triggers), mirrorIDs(triggers))
	}
}

func TestScheduleMirrorTriggerSource_LoadTriggers_SkipsDisabledAndEmpty(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			// Disabled — skip.
			mirrorTriggerBead("gd-m1", "gd-s1", "*/30 * * * *", map[string]string{"enabled": "false"}),
			// Empty triggers[] — skip.
			mirrorTriggerBead("gd-m2", "gd-s2", "", map[string]string{"triggers": ""}),
			// Valid.
			mirrorTriggerBead("gd-m3", "gd-s3", "*/30 * * * *", nil),
		},
	}
	src := NewScheduleMirrorTriggerSource(slog.Default(), nil)
	triggers, err := src.LoadTriggers(context.Background(), daemon)
	if err != nil {
		t.Fatalf("LoadTriggers: %v", err)
	}
	if len(triggers) != 1 {
		t.Fatalf("expected 1 trigger, got %d (ids=%v)", len(triggers), mirrorIDs(triggers))
	}
	if triggers[0].ID != "gd-m3" {
		t.Errorf("loaded mirror %q, want gd-m3", triggers[0].ID)
	}
}

// TestScheduleMirrorTriggerSource_Evaluate_CronFires is the core
// regression test for ka-0000008057: a mirror must fire on its cron tick.
func TestScheduleMirrorTriggerSource_Evaluate_CronFires(t *testing.T) {
	// Set "now" to 10:05 UTC, cron is "0 * * * *" — most-recent fire at 10:00.
	now := time.Date(2026, 4, 12, 10, 5, 0, 0, time.UTC)
	src := newMirrorSource(now)

	trig := TriggerDef{
		ID:     "gd-mirror-x",
		Title:  "schedule-mirror: Reviver",
		Source: "schedule-mirror",
		Action: TriggerActionSpawnAgent,
		Triggers: []scheduler.TriggerEntry{{
			Kind: scheduler.TriggerKindCron,
			Cron: "0 * * * *",
		}},
		SourceData: scheduleMirrorData{
			SourceScheduleID: "gd-src-closed",
		},
	}

	matches, err := src.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected 1 match, got %d", len(matches))
	}
	m := matches[0]
	if m.Key != "0:0:2026-04-12T10:00" {
		t.Errorf("dedup key = %q, want 0:0:2026-04-12T10:00", m.Key)
	}
	if m.Title != "ScheduleMirror[cron]: schedule-mirror: Reviver" {
		t.Errorf("match title = %q", m.Title)
	}
	if m.Fields["mirror_fire_key"] != m.Key {
		t.Errorf("mirror_fire_key = %q, want %q", m.Fields["mirror_fire_key"], m.Key)
	}
	if m.Fields["source_schedule_id"] != "gd-src-closed" {
		t.Errorf("source_schedule_id = %q", m.Fields["source_schedule_id"])
	}
	// Must surface the source:schedule-mirror label so downstream graph
	// filters can distinguish mirror fires from multi-source fires.
	if !slices.Contains(m.Labels, "source:schedule-mirror") {
		t.Errorf("missing source:schedule-mirror label; got %v", m.Labels)
	}
}

// TestScheduleMirrorTriggerSource_Evaluate_AllBufferedEventsFire is the
// mirror-side regression lock for ka-0000000952: two distinct matching bead
// events buffered in one cycle must each produce a TriggerMatch with its own
// per-event dedup key rather than collapsing to the last-arrived event.
func TestScheduleMirrorTriggerSource_Evaluate_AllBufferedEventsFire(t *testing.T) {
	now := time.Date(2026, 4, 12, 10, 5, 0, 0, time.UTC)
	src := newMirrorSource(now)

	src.engine.OnBeadEvent("closed", "gd-A", map[string]any{"id": "gd-A", "status": "closed"}, nil)
	src.engine.OnBeadEvent("closed", "gd-B", map[string]any{"id": "gd-B", "status": "closed"}, nil)

	trig := TriggerDef{
		ID:     "gd-mirror-all",
		Title:  "schedule-mirror: per-event",
		Source: "schedule-mirror",
		Action: TriggerActionSpawnAgent,
		Triggers: []scheduler.TriggerEntry{{
			Kind:   scheduler.TriggerKindBeadEvent,
			Events: []string{"closed"},
		}},
		SourceData: scheduleMirrorData{SourceScheduleID: "gd-src-closed"},
	}

	matches, err := src.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(matches) != 2 {
		t.Fatalf("expected one match per distinct event, got %d", len(matches))
	}
	keys := map[string]bool{}
	for _, m := range matches {
		keys[m.Key] = true
	}
	if !keys["0:0:closed:gd-A"] || !keys["0:0:closed:gd-B"] {
		t.Errorf("both per-event dedup keys expected, got %v", keys)
	}
}

func TestScheduleMirrorTriggerSource_Evaluate_AlreadyRan(t *testing.T) {
	now := time.Date(2026, 4, 12, 10, 5, 0, 0, time.UTC)
	src := newMirrorSource(now)

	trig := TriggerDef{
		ID:     "gd-mirror-x",
		Source: "schedule-mirror",
		Triggers: []scheduler.TriggerEntry{{
			Kind: scheduler.TriggerKindCron,
			Cron: "0 * * * *",
		}},
		SourceData: scheduleMirrorData{
			LastRun: time.Date(2026, 4, 12, 10, 1, 0, 0, time.UTC),
		},
	}
	matches, err := src.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(matches) != 0 {
		t.Errorf("expected 0 matches (already ran past last fire), got %d", len(matches))
	}
}

// CronDue backs the bridge engine's cron-due cooldown bypass (bug
// ka-0000004307): it must mirror Evaluate's due/not-due decision read-only.
func TestScheduleMirrorTriggerSource_CronDue(t *testing.T) {
	// 10:05 UTC with cron "0 * * * *": inside the window, past the 10:00 boundary.
	inWindow := time.Date(2026, 4, 12, 10, 5, 0, 0, time.UTC)
	cronEntries := []scheduler.TriggerEntry{{
		Kind: scheduler.TriggerKindCron,
		Cron: "0 * * * *",
	}}

	t.Run("due when in-window and past last_run", func(t *testing.T) {
		src := newMirrorSource(inWindow)
		trig := TriggerDef{
			ID:         "gd-mirror-x",
			Source:     "schedule-mirror",
			Triggers:   cronEntries,
			SourceData: scheduleMirrorData{},
		}
		if !src.CronDue(context.Background(), trig) {
			t.Error("cron past its 10:00 boundary with zero LastRun must be due")
		}
	})

	t.Run("not due when last_run covers the boundary", func(t *testing.T) {
		src := newMirrorSource(inWindow)
		trig := TriggerDef{
			ID:       "gd-mirror-x",
			Source:   "schedule-mirror",
			Triggers: cronEntries,
			SourceData: scheduleMirrorData{
				LastRun: time.Date(2026, 4, 12, 10, 1, 0, 0, time.UTC),
			},
		}
		if src.CronDue(context.Background(), trig) {
			t.Error("a window already consumed by LastRun must not be due")
		}
	})

	t.Run("no entries is not due", func(t *testing.T) {
		src := newMirrorSource(inWindow)
		trig := TriggerDef{ID: "gd-mirror-x", Source: "schedule-mirror"}
		if src.CronDue(context.Background(), trig) {
			t.Error("a mirror with no trigger entries must not be due")
		}
	})
}

// TestScheduleMirrorTriggerSource_CatchUpKeys_ReadsMirrorFireKey verifies
// the dedup-key field name matches the field written in Evaluate so the
// engine's seen-set is correctly seeded on restart.
func TestScheduleMirrorTriggerSource_CatchUpKeys_ReadsMirrorFireKey(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{
				ID:     "gd-task-1",
				Kind:   "issue",
				Status: "closed",
				Labels: []string{"trigger:gd-mirror-x"},
				Fields: map[string]string{"mirror_fire_key": "0:0:2026-04-12T10:00"},
			},
			{
				ID:     "gd-task-2",
				Kind:   "issue",
				Status: "open",
				Labels: []string{"trigger:gd-mirror-x"},
				Fields: map[string]string{"mirror_fire_key": "0:0:2026-04-12T11:00"},
			},
		},
	}
	src := NewScheduleMirrorTriggerSource(slog.Default(), nil)
	keys, err := src.CatchUpKeys(context.Background(), daemon, "gd-mirror-x")
	if err != nil {
		t.Fatalf("CatchUpKeys: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("expected 2 catch-up keys, got %d", len(keys))
	}
}

// TestScheduleMirrorTriggerSource_EventReady is the schedule-mirror analogue of
// the multi-source EventReady test — the regression guard for ka-0000004937. A
// schedule-mirror fires event-kind entries off the SHARED EntryEngine buffer, so
// without EventReady a matching event that arrives mid-cooldown is buffered,
// cooldown-skips the mirror before Evaluate runs, then drains-and-loses at cycle
// end. EventReady lets the bridge cooldown gate bypass cooldown for a pending
// matching event. It must return true for a matching buffered event and false
// for a non-matching one (the gate that proves the match actually discriminates).
func TestScheduleMirrorTriggerSource_EventReady(t *testing.T) {
	now := time.Date(2026, 4, 12, 12, 0, 0, 0, time.UTC)

	entries, err := scheduler.ParseTriggers(
		`[{"kind":"bead_event","events":["created"],"match":{"field":"label:smer-match","op":"present"}}]`)
	if err != nil {
		t.Fatalf("ParseTriggers: %v", err)
	}
	trig := TriggerDef{
		ID:         "gd-mirror-event",
		Title:      "mirror event",
		Triggers:   entries,
		SourceData: scheduleMirrorData{},
	}

	// Empty triggers[] short-circuits to false.
	t.Run("empty triggers is not ready", func(t *testing.T) {
		src := newMirrorSource(now)
		if src.EventReady(context.Background(), TriggerDef{ID: "gd-empty"}) {
			t.Fatal("EventReady = true for a mirror with no triggers[]")
		}
	})

	// A MATCHING buffered event makes the mirror event-ready — the injected
	// event carries the match label onto the shared engine buffer, exactly as a
	// live SSE bead frame would.
	t.Run("matching event is ready", func(t *testing.T) {
		src := newMirrorSource(now)
		src.engine.OnBeadEvent("created", "gd-source-1",
			beadEventToPayload(&BeadEvent{
				ID:     "gd-source-1",
				Type:   "task",
				Title:  "probe bead",
				Status: "open",
				Labels: []string{"smer-match", "project:gardenio"},
			}),
			[]string{"smer-match", "project:gardenio"})
		if !src.EventReady(context.Background(), trig) {
			t.Fatal("EventReady = false; the matching buffered event should make the mirror event-ready")
		}
	})

	// A NON-matching event (missing the match label) must NOT make the mirror
	// event-ready — the negative control proving the match gates.
	t.Run("non-matching event is not ready", func(t *testing.T) {
		src := newMirrorSource(now)
		src.engine.OnBeadEvent("created", "gd-source-2",
			beadEventToPayload(&BeadEvent{
				ID:     "gd-source-2",
				Type:   "task",
				Status: "open",
				Labels: []string{"some-other-label"},
			}),
			[]string{"some-other-label"})
		if src.EventReady(context.Background(), trig) {
			t.Fatal("EventReady = true for a non-matching event; the label:present match should have gated it")
		}
	})
}

func mirrorIDs(defs []TriggerDef) []string {
	out := make([]string, len(defs))
	for i, d := range defs {
		out[i] = d.ID
	}
	return out
}
