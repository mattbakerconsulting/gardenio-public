package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/gardenermode"
	"gardenio/controller/internal/planspend"
	"gardenio/controller/internal/scheduler"
	"gardenio/controller/internal/scheduler/conditions"
)

// newMultiSource constructs a MultiTriggerSource with a fixed clock.
func newMultiSource(now time.Time) *MultiTriggerSource {
	s := NewMultiTriggerSource(slog.Default(), nil)
	s.nowFunc = func() time.Time { return now }
	return s
}

// multiTriggerBead builds a trigger:multi-labelled trigger bead with the
// given JSON-encoded triggers[] array.
func multiTriggerBead(id, triggersJSON string, fields map[string]string) *beadsapi.BeadDetail {
	f := map[string]string{
		"enabled":      "true",
		"action":       "notify",
		"triggers":     triggersJSON,
		"last_checked": time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339),
	}
	maps.Copy(f, fields)
	return &beadsapi.BeadDetail{
		ID:     id,
		Type:   "trigger",
		Status: "open",
		Title:  "Multi " + id,
		Labels: []string{"trigger:multi"},
		Fields: f,
	}
}

// --- Unit tests ---

func TestMultiTriggerSource_Name(t *testing.T) {
	s := NewMultiTriggerSource(nil, nil)
	if s.Name() != "multi" {
		t.Errorf("Name() = %q, want %q", s.Name(), "multi")
	}
}

func TestMultiTriggerSource_SkipRuntimeBootstrap(t *testing.T) {
	s := NewMultiTriggerSource(nil, nil)
	if !s.SkipRuntimeBootstrap() {
		t.Errorf("SkipRuntimeBootstrap() = false, want true")
	}
}

func TestMultiTriggerSource_LoadTriggers(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			multiTriggerBead("t-cron", `[{"kind":"cron","cron":"*/10 * * * *"}]`, nil),
			multiTriggerBead("t-empty", "", nil),             // empty triggers — skip
			multiTriggerBead("t-bad", "not valid json", nil), // unparseable — skip
			multiTriggerBead("t-off", `[{"kind":"cron","cron":"0 9 * * *"}]`,
				map[string]string{"enabled": "false"}), // disabled — skip
			multiTriggerBead("t-mix", `[{"kind":"cron","cron":"0 9 * * *"},{"kind":"bead_event","events":["closed"]}]`,
				map[string]string{"timezone": "America/Los_Angeles", "last_run": "2026-04-12T10:00:00Z"}),
		},
	}

	src := NewMultiTriggerSource(slog.Default(), nil)
	triggers, err := src.LoadTriggers(context.Background(), daemon)
	if err != nil {
		t.Fatalf("LoadTriggers error: %v", err)
	}
	if len(triggers) != 2 {
		t.Fatalf("expected 2 triggers (t-cron, t-mix), got %d", len(triggers))
	}
	if triggers[0].ID != "t-cron" {
		t.Errorf("triggers[0].ID = %q", triggers[0].ID)
	}
	if triggers[1].ID != "t-mix" {
		t.Errorf("triggers[1].ID = %q", triggers[1].ID)
	}
	if len(triggers[1].Triggers) != 2 {
		t.Errorf("t-mix should have 2 entries, got %d", len(triggers[1].Triggers))
	}

	data, ok := triggers[1].SourceData.(multiTriggerData)
	if !ok {
		t.Fatalf("t-mix SourceData type assertion failed: %T", triggers[1].SourceData)
	}
	if data.Timezone != "America/Los_Angeles" {
		t.Errorf("timezone = %q", data.Timezone)
	}
	want, _ := time.Parse(time.RFC3339, "2026-04-12T10:00:00Z")
	if !data.LastRun.Equal(want) {
		t.Errorf("LastRun = %v, want %v", data.LastRun, want)
	}
}

// TestMultiTriggerSource_Evaluate_CronEntry: a cron entry whose Prev fire
// time has elapsed since LastRun fires and produces a dedup key that
// includes the entry index, the entry label (or fallback to index), and the
// minute-granularity fire time.
func TestMultiTriggerSource_Evaluate_CronEntry(t *testing.T) {
	// Fire at 09:00 UTC on a known day — well past LastRun at 08:00 UTC.
	now := time.Date(2026, 4, 12, 9, 1, 0, 0, time.UTC)
	src := newMultiSource(now)

	entries := []scheduler.TriggerEntry{
		{Kind: scheduler.TriggerKindCron, Cron: "0 9 * * *", Label: "morning"},
	}
	trig := TriggerDef{
		ID:       "gd-multi-1",
		Title:    "morning kickoff",
		Source:   "multi",
		Triggers: entries,
		SourceData: multiTriggerData{
			LastRun: time.Date(2026, 4, 12, 8, 0, 0, 0, time.UTC),
		},
	}

	matches, err := src.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate error: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected 1 match, got %d", len(matches))
	}
	m := matches[0]
	wantKey := "0:morning:2026-04-12T09:00"
	if m.Key != wantKey {
		t.Errorf("Key = %q, want %q", m.Key, wantKey)
	}
	if m.Fields["multi_entry_idx"] != "0" {
		t.Errorf("multi_entry_idx = %q, want 0", m.Fields["multi_entry_idx"])
	}
	if m.Fields["multi_entry_kind"] != scheduler.TriggerKindCron {
		t.Errorf("multi_entry_kind = %q", m.Fields["multi_entry_kind"])
	}
	if m.Fields["multi_entry_label"] != "morning" {
		t.Errorf("multi_entry_label = %q", m.Fields["multi_entry_label"])
	}
	if m.Fields["multi_fire_key"] != wantKey {
		t.Errorf("multi_fire_key field = %q, want %q", m.Fields["multi_fire_key"], wantKey)
	}
	if got := m.TemplateVars["{{cause}}"]; got != "cron 0 9 * * *" {
		t.Errorf("cause = %q", got)
	}
}

// TestMultiTriggerSource_Evaluate_NoFire: a cron entry whose LastRun is at
// or after Prev(now) does not fire.
func TestMultiTriggerSource_Evaluate_NoFire(t *testing.T) {
	now := time.Date(2026, 4, 12, 9, 5, 0, 0, time.UTC)
	src := newMultiSource(now)

	trig := TriggerDef{
		ID: "gd-multi-2",
		Triggers: []scheduler.TriggerEntry{
			{Kind: scheduler.TriggerKindCron, Cron: "0 9 * * *"},
		},
		SourceData: multiTriggerData{
			// Already fired for this 9am tick.
			LastRun: time.Date(2026, 4, 12, 9, 0, 0, 0, time.UTC),
		},
	}

	matches, err := src.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate error: %v", err)
	}
	if len(matches) != 0 {
		t.Errorf("expected 0 matches, got %d", len(matches))
	}
}

// TestMultiTriggerSource_Evaluate_BeadEventEntry: an event-kind entry fires
// when a matching event has been buffered into the underlying engine.
func TestMultiTriggerSource_Evaluate_BeadEventEntry(t *testing.T) {
	now := time.Date(2026, 4, 12, 12, 0, 0, 0, time.UTC)
	src := newMultiSource(now)

	// Buffer a bead.closed event for gd-source-1.
	src.engine.OnBeadEventAt("closed", "gd-source-1", map[string]any{
		"id":     "gd-source-1",
		"type":   "task",
		"status": "closed",
	}, []string{"project:gardenio"}, now)

	trig := TriggerDef{
		ID:    "gd-multi-3",
		Title: "fan-in event",
		Triggers: []scheduler.TriggerEntry{
			{Kind: scheduler.TriggerKindBeadEvent, Events: []string{"closed"}},
		},
		SourceData: multiTriggerData{},
	}

	matches, err := src.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate error: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected 1 match, got %d", len(matches))
	}
	wantKey := "0:0:closed:gd-source-1@2026-04-12T12:00:00Z"
	if matches[0].Key != wantKey {
		t.Errorf("Key = %q, want %q", matches[0].Key, wantKey)
	}
	if got := matches[0].TemplateVars["{{event_bead_id}}"]; got != "gd-source-1" {
		t.Errorf("event_bead_id = %q", got)
	}
	if got := matches[0].TemplateVars["{{event_type}}"]; got != "closed" {
		t.Errorf("event_type = %q", got)
	}
}

// TestMultiTriggerSource_Evaluate_AllBufferedEventsFire is the source-level
// regression lock for ka-0000000952: two distinct matching bead events buffered
// in one reconcile cycle must each produce a TriggerMatch with its own per-event
// dedup key, not collapse to a single fire for the last-arrived event.
func TestMultiTriggerSource_Evaluate_AllBufferedEventsFire(t *testing.T) {
	now := time.Date(2026, 4, 12, 12, 0, 0, 0, time.UTC)
	src := newMultiSource(now)

	src.engine.OnBeadEventAt("closed", "gd-A", map[string]any{"id": "gd-A", "status": "closed"}, []string{"project:gardenio"}, now)
	src.engine.OnBeadEventAt("closed", "gd-B", map[string]any{"id": "gd-B", "status": "closed"}, []string{"project:gardenio"}, now)

	trig := TriggerDef{
		ID:    "gd-multi-all",
		Title: "per-event fan-in",
		Triggers: []scheduler.TriggerEntry{
			{Kind: scheduler.TriggerKindBeadEvent, Events: []string{"closed"}},
		},
		SourceData: multiTriggerData{},
	}

	matches, err := src.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate error: %v", err)
	}
	if len(matches) != 2 {
		t.Fatalf("expected one match per distinct event, got %d", len(matches))
	}
	keys := map[string]bool{}
	for _, m := range matches {
		keys[m.Key] = true
	}
	if !keys["0:0:closed:gd-A@2026-04-12T12:00:00Z"] || !keys["0:0:closed:gd-B@2026-04-12T12:00:00Z"] {
		t.Errorf("both per-event dedup keys expected, got %v", keys)
	}
}

// TestMultiTriggerSource_InjectBeadEvent drives the runtime-injection seam
// cmd/trigger-probe --source=multi uses (ka-0000000405). It asserts the injected
// event buffers exactly as a live SSE frame would — same event-type derivation
// from the full topic, same beadEventToPayload columns, same labels folded in —
// by matching a bead_event entry whose Match is a label:present leaf that only
// the buffered event's labels satisfy. This is the unit-level guarantee that the
// e2e's synthetic event is not a shape prod never emits (fixture fidelity,
// ka-0000001521): InjectBeadEvent shares injectBeadEventFromTopic with RegisterSSE.
func TestMultiTriggerSource_InjectBeadEvent(t *testing.T) {
	now := time.Date(2026, 4, 12, 12, 0, 0, 0, time.UTC)

	entries, err := scheduler.ParseTriggers(
		`[{"kind":"bead_event","events":["created"],"match":{"field":"label:mcer-match","op":"present"}}]`)
	if err != nil {
		t.Fatalf("ParseTriggers: %v", err)
	}
	trig := TriggerDef{
		ID:         "gd-multi-inject",
		Title:      "inject seam",
		Triggers:   entries,
		SourceData: multiTriggerData{},
	}

	// nil bead / empty topic are no-ops (guarded like BeadStateTriggerSource).
	t.Run("nil bead and empty topic are no-ops", func(t *testing.T) {
		src := newMultiSource(now)
		src.InjectBeadEvent("beads.bead.created", nil)
		src.InjectBeadEvent("", &BeadEvent{ID: "gd-x"})
		if n := src.PendingCount(); n != 0 {
			t.Fatalf("expected no buffered events, got %d", n)
		}
	})

	// A MATCHING injected created-event makes the trigger event-ready AND fires
	// through Evaluate — proving the created-topic -> "created" derivation and
	// the label fold-through both work on the injected path.
	t.Run("matching created event is ready and fires", func(t *testing.T) {
		src := newMultiSource(now)
		src.InjectBeadEvent("beads.bead.created", &BeadEvent{
			ID:     "gd-source-1",
			Type:   "task",
			Title:  "probe bead",
			Status: "open",
			Labels: []string{"mcer-match", "project:gardenio"},
		})
		if n := src.PendingCount(); n != 1 {
			t.Fatalf("expected 1 buffered event, got %d", n)
		}
		if !src.EventReady(context.Background(), trig) {
			t.Fatalf("EventReady = false; the injected matching event should make the trigger event-ready")
		}
		matches, err := src.Evaluate(context.Background(), trig)
		if err != nil {
			t.Fatalf("Evaluate error: %v", err)
		}
		if len(matches) != 1 {
			t.Fatalf("expected 1 match, got %d", len(matches))
		}
		if got := matches[0].TemplateVars["{{event_type}}"]; got != "created" {
			t.Errorf("event_type = %q, want \"created\" (topic-strip derivation)", got)
		}
		if got := matches[0].TemplateVars["{{event_bead_id}}"]; got != "gd-source-1" {
			t.Errorf("event_bead_id = %q, want \"gd-source-1\"", got)
		}
	})

	// A NON-matching event (missing the match label) must NOT make the trigger
	// event-ready — the negative control that proves the match actually gates,
	// mirroring the e2e's negative leg.
	t.Run("non-matching event is not ready", func(t *testing.T) {
		src := newMultiSource(now)
		src.InjectBeadEvent("beads.bead.created", &BeadEvent{
			ID:     "gd-source-2",
			Type:   "task",
			Status: "open",
			Labels: []string{"some-other-label"},
		})
		if src.EventReady(context.Background(), trig) {
			t.Fatalf("EventReady = true for a non-matching event; the label:present match should have gated it")
		}
	})
}

// TestMultiTriggerSource_FreezeKeepsMidPassEvent replays gd-IQcMk16PkH: the
// escalation-coordinator trigger was visited (cooldown skip) early in a pass,
// the 'filed' escalation's created frame landed before the pass's drain, and
// no pass ever saw it. Frozen, the frame waits for the next pass and makes the
// trigger event-ready there.
func TestMultiTriggerSource_FreezeKeepsMidPassEvent(t *testing.T) {
	now := time.Date(2026, 11, 4, 5, 8, 41, 0, time.UTC)
	entries, err := scheduler.ParseTriggers(`[{"kind":"bead_event","label":"filed","events":["created"],` +
		`"match":{"all":[{"field":"type","op":"==","value":"escalation"},{"field":"held_by","op":"==","value":"coordinator:cluster"}]}}]`)
	if err != nil {
		t.Fatalf("ParseTriggers: %v", err)
	}
	trig := TriggerDef{ID: "gd-coordinator", Title: "coordinator", Project: "gardenio", Triggers: entries, SourceData: multiTriggerData{}}
	src := newMultiSource(now)

	src.FreezePending()
	if src.EventReady(context.Background(), trig) {
		t.Fatal("EventReady = true before any event")
	}
	src.InjectBeadEvent("beads.bead.created", &BeadEvent{
		ID:     "gd-esc",
		Type:   "escalation",
		Status: "open",
		Fields: map[string]string{"held_by": "coordinator:cluster"},
	})
	src.DrainPending()

	if !src.EventReady(context.Background(), trig) {
		t.Fatal("EventReady = false on the next pass; the mid-pass event was drained unseen")
	}
	matches, err := src.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(matches) != 1 || matches[0].TemplateVars["{{event_bead_id}}"] != "gd-esc" {
		t.Fatalf("matches = %+v, want one fire for gd-esc", matches)
	}
}

// TestMultiTriggerSource_Evaluate_FirstEntryWins: when multiple entries
// would fire, the first one in the array wins.
func TestMultiTriggerSource_Evaluate_FirstEntryWins(t *testing.T) {
	now := time.Date(2026, 4, 12, 9, 1, 0, 0, time.UTC)
	src := newMultiSource(now)

	src.engine.OnBeadEvent("closed", "gd-evt", nil, nil)

	trig := TriggerDef{
		ID: "gd-multi-4",
		Triggers: []scheduler.TriggerEntry{
			{Kind: scheduler.TriggerKindCron, Cron: "0 9 * * *", Label: "morning"},
			{Kind: scheduler.TriggerKindBeadEvent, Events: []string{"closed"}, Label: "any-close"},
		},
		SourceData: multiTriggerData{
			LastRun: time.Date(2026, 4, 12, 8, 0, 0, 0, time.UTC),
		},
	}

	matches, err := src.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate error: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected 1 match, got %d", len(matches))
	}
	if matches[0].Fields["multi_entry_idx"] != "0" {
		t.Errorf("expected entry index 0 (cron) to win, got %q", matches[0].Fields["multi_entry_idx"])
	}
}

// TestMultiTriggerSource_Evaluate_LegacyFallback: a TriggerDef with an empty
// Triggers slice returns no matches — the legacy single-source path stays
// owned by the per-source loader (jira/gitlab/etc.).
func TestMultiTriggerSource_Evaluate_LegacyFallback(t *testing.T) {
	src := newMultiSource(time.Now())
	trig := TriggerDef{ID: "gd-legacy", Triggers: nil}
	matches, err := src.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate error: %v", err)
	}
	if len(matches) != 0 {
		t.Errorf("expected 0 matches for empty Triggers, got %d", len(matches))
	}
}

// TestMultiTriggerSource_DedupKey_IndexFallback: when an entry has no Label,
// the dedup key uses the entry index as the second segment.
func TestMultiTriggerSource_DedupKey_IndexFallback(t *testing.T) {
	now := time.Date(2026, 4, 12, 9, 1, 0, 0, time.UTC)
	src := newMultiSource(now)

	trig := TriggerDef{
		ID: "gd-multi-5",
		Triggers: []scheduler.TriggerEntry{
			{Kind: scheduler.TriggerKindCron, Cron: "0 9 * * *"}, // no label
		},
		SourceData: multiTriggerData{LastRun: time.Date(2026, 4, 12, 8, 0, 0, 0, time.UTC)},
	}
	matches, err := src.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate error: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected 1 match, got %d", len(matches))
	}
	wantKey := "0:0:2026-04-12T09:00"
	if matches[0].Key != wantKey {
		t.Errorf("Key = %q, want %q", matches[0].Key, wantKey)
	}
}

// TestMultiSourceMatchKey_CommentPerOccurrence: two steward nudges on one plan
// get distinct keys, so the second is not swallowed as already fired; so do
// two updates of one bead (gd-wRullKPlWJ).
func TestMultiSourceMatchKey_CommentPerOccurrence(t *testing.T) {
	nudge := func(at time.Time) scheduler.FireDecision {
		return scheduler.FireDecision{
			Kind:  scheduler.TriggerKindBeadEvent,
			Time:  at,
			Event: &scheduler.TriggerEvent{Kind: scheduler.TriggerKindBeadEvent, EventType: "comment.added", BeadID: "gd-plan", Time: at},
		}
	}
	first := multiSourceMatchKey(nudge(time.Date(2026, 11, 3, 0, 58, 48, 0, time.UTC)))
	second := multiSourceMatchKey(nudge(time.Date(2026, 11, 3, 8, 58, 21, 0, time.UTC)))
	if first != "comment.added:gd-plan@2026-11-03T00:58:48Z" {
		t.Errorf("first nudge key = %q", first)
	}
	if first == second {
		t.Errorf("two nudges share key %q", first)
	}

	update := func(at time.Time) string {
		return multiSourceMatchKey(scheduler.FireDecision{
			Kind:  scheduler.TriggerKindBeadEvent,
			Time:  at,
			Event: &scheduler.TriggerEvent{Kind: scheduler.TriggerKindBeadEvent, EventType: "updated", BeadID: "gd-step", Time: at},
		})
	}
	firstUpdate := update(time.Date(2026, 11, 3, 1, 0, 0, 0, time.UTC))
	if firstUpdate != "updated:gd-step@2026-11-03T01:00:00Z" {
		t.Errorf("update key = %q", firstUpdate)
	}
	if again := update(time.Date(2026, 11, 3, 3, 30, 0, 0, time.UTC)); again == firstUpdate {
		t.Errorf("two updates of one bead share key %q", again)
	}
}

// TestMultiTriggerSource_RecordsFireOnEngine: a successful fire records the
// fire time on the underlying engine so a same-tick second Evaluate is
// suppressed by the engine's throttle (when configured).
func TestMultiTriggerSource_RecordsFireOnEngine(t *testing.T) {
	now := time.Date(2026, 4, 12, 9, 1, 0, 0, time.UTC)
	src := newMultiSource(now)

	trig := TriggerDef{
		ID: "gd-multi-throttle",
		Triggers: []scheduler.TriggerEntry{
			{Kind: scheduler.TriggerKindCron, Cron: "0 9 * * *", Throttle: "10m"},
		},
		SourceData: multiTriggerData{LastRun: time.Date(2026, 4, 12, 8, 0, 0, 0, time.UTC)},
	}

	if matches, err := src.Evaluate(context.Background(), trig); err != nil || len(matches) != 1 {
		t.Fatalf("first Evaluate: matches=%d err=%v", len(matches), err)
	}

	// Second evaluate at the same now must be throttled (no fire).
	matches, err := src.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("second Evaluate error: %v", err)
	}
	if len(matches) != 0 {
		t.Errorf("expected throttle to suppress second fire, got %d matches", len(matches))
	}
}

// TestMultiTriggerSource_CooldownEventRace is the regression test for bug
// ka-0000005988: a bead_event that lands while a multi-source trigger is in its
// per-trigger cooldown window must still be consumed, not silently dropped.
//
// Repro (from the bead description): a trigger:multi with cooldown_minutes=20
// and a bead_event entry, with an event fired ~1m into the cooldown window.
// Before the fix, evalOne's cooldown gate returned BEFORE source.Evaluate ran,
// so the buffered event was never consumed and got drained at cycle end. With
// the fix, the engine's cooldown gate calls source.EventReady and bypasses
// cooldown when a matching event is pending, so the event fires this cycle.
//
// This test fails without the event-eligible bypass in evalOne (cooldown skip
// → fired == 0) and passes with it (fired == 1). The control sub-case proves
// cooldown still throttles cron/idle re-eval when no event is pending.
func TestMultiTriggerSource_CooldownEventRace(t *testing.T) {
	entries := `[{"kind":"bead_event","events":["closed"]}]`

	newEngineWithMulti := func(src *MultiTriggerSource) (*TriggerEngine, *int) {
		daemon := &mockTriggerDaemon{}
		engine := NewTriggerEngine(TriggerEngineConfig{Daemon: daemon, Logger: slog.Default()})
		engine.RegisterSource(src)
		fired := 0
		engine.notifier = func(_ context.Context, _, _ string, _ string) { fired++ }
		return engine, &fired
	}

	// midCooldownTrigger builds a TriggerDef whose LastChecked is 1 minute ago
	// with a 20-minute cooldown → squarely inside the cooldown window.
	midCooldownTrigger := func() TriggerDef {
		parsed, err := scheduler.ParseTriggers(entries)
		if err != nil {
			t.Fatalf("ParseTriggers: %v", err)
		}
		return TriggerDef{
			ID:              "gd-cooldown-race",
			Title:           "cooldown race",
			Source:          "multi",
			Action:          TriggerActionNotify,
			MaxConcurrent:   5,
			CooldownMinutes: 20,
			Triggers:        parsed,
			LastChecked:     time.Now().Add(-1 * time.Minute),
			SourceData:      multiTriggerData{},
		}
	}

	t.Run("event mid-cooldown is consumed, not drained", func(t *testing.T) {
		src := newMultiSource(time.Now())
		engine, fired := newEngineWithMulti(src)

		// A matching bead.closed event arrives 1m into the cooldown window.
		src.engine.OnBeadEvent("closed", "gd-source-1", map[string]any{
			"id":     "gd-source-1",
			"type":   "task",
			"status": "closed",
		}, []string{"project:gardenio"})

		trig := midCooldownTrigger()

		// Sanity: the trigger really is mid-cooldown.
		if time.Since(trig.LastChecked) >= effectiveCooldown(trig) {
			t.Fatalf("test setup: trigger is not in cooldown window")
		}

		engine.evalOne(context.Background(), src, trig)

		if *fired != 1 {
			t.Errorf("expected the mid-cooldown event to fire exactly once, fired %d "+
				"(without the cooldown/event-race fix the event is cooldown-skipped and dropped)", *fired)
		}
		if !slices.ContainsFunc(slices.Collect(maps.Keys(engine.SeenKeys(trig.ID))), func(k string) bool {
			return strings.HasPrefix(k, "0:0:closed:gd-source-1@")
		}) {
			t.Errorf("expected the consumed event to be recorded in the seen set, got %v",
				engine.SeenKeys(trig.ID))
		}
	})

	t.Run("no event mid-cooldown still honors cooldown", func(t *testing.T) {
		src := newMultiSource(time.Now())
		engine, fired := newEngineWithMulti(src)

		// No buffered event: cron/idle re-eval must remain cooldown-gated.
		trig := midCooldownTrigger()
		engine.evalOne(context.Background(), src, trig)

		if *fired != 0 {
			t.Errorf("with no pending event, cooldown must suppress re-eval, fired %d", *fired)
		}
	})
}

// ka-0000004307: a due cron window must bypass the per-trigger cooldown.
//
// Repro (from the bead): gardener ka-0000005204 ran cron "11 */12 * * *"
// (America/Los_Angeles → two 1-hour windows/day) with cooldown_minutes=240.
// The engine stamps last_checked on EVERY evaluation, so evaluations recur at
// fixed ~4h offsets — and 240m divides 24h, so the offsets are phase-locked.
// After a redeploy re-phased them outside both cron windows, every evaluation
// found InWindow=false, fired nothing, re-stamped last_checked, and re-locked
// the phase: zero fires for 6 days while the trigger looked alive.
//
// This test drops an evaluation INSIDE the cron window but mid-cooldown.
// Without the cron-due bypass the cooldown gate skips it (fired == 0 — the
// aliasing wedge); with the bypass the due window fires (fired == 1). The
// control sub-case proves cooldown still throttles when no cron is due.
func TestMultiTriggerSource_CooldownCronAliasing(t *testing.T) {
	entries := `[{"kind":"cron","cron":"11 */12 * * *","timezone":"UTC"}]`
	// 12:30 UTC: inside the hour-12 window, past the 12:11 boundary.
	inWindow := time.Date(2026, 7, 18, 12, 30, 0, 0, time.UTC)

	newEngineWithMulti := func(src *MultiTriggerSource) (*TriggerEngine, *int) {
		daemon := &mockTriggerDaemon{}
		engine := NewTriggerEngine(TriggerEngineConfig{Daemon: daemon, Logger: slog.Default()})
		engine.RegisterSource(src)
		fired := 0
		engine.notifier = func(_ context.Context, _, _ string, _ string) { fired++ }
		return engine, &fired
	}

	midCooldownTrigger := func(lastRun time.Time) TriggerDef {
		parsed, err := scheduler.ParseTriggers(entries)
		if err != nil {
			t.Fatalf("ParseTriggers: %v", err)
		}
		return TriggerDef{
			ID:              "gd-cron-aliasing",
			Title:           "cron aliasing",
			Source:          "multi",
			Action:          TriggerActionNotify,
			MaxConcurrent:   5,
			CooldownMinutes: 240,
			Triggers:        parsed,
			// last_checked 40m ago → squarely inside the 240m cooldown, the
			// state a phase-locked evaluation slot leaves behind. Relative to
			// the REAL clock because evalOne's cooldown gate uses time.Since;
			// the cron evaluation itself runs on the source's fixed nowFunc.
			LastChecked: time.Now().Add(-40 * time.Minute),
			SourceData:  multiTriggerData{LastRun: lastRun},
		}
	}

	t.Run("due cron window mid-cooldown fires", func(t *testing.T) {
		src := newMultiSource(inWindow)
		engine, fired := newEngineWithMulti(src)

		// LastRun at the PREVIOUS window (00:11) → the 12:11 boundary is due.
		trig := midCooldownTrigger(time.Date(2026, 7, 18, 0, 11, 0, 0, time.UTC))

		if time.Since(trig.LastChecked) >= effectiveCooldown(trig) {
			t.Fatalf("test setup: trigger is not in cooldown window")
		}

		engine.evalOne(context.Background(), src, trig)

		if *fired != 1 {
			t.Errorf("expected the due cron window to bypass cooldown and fire exactly once, fired %d "+
				"(without the cron-due bypass the window is cooldown-skipped — the ka-0000004307 aliasing wedge)", *fired)
		}
	})

	t.Run("no due cron mid-cooldown still honors cooldown", func(t *testing.T) {
		src := newMultiSource(inWindow)
		engine, fired := newEngineWithMulti(src)

		// LastRun already covers the 12:11 boundary → nothing due.
		trig := midCooldownTrigger(time.Date(2026, 7, 18, 12, 11, 0, 0, time.UTC))
		engine.evalOne(context.Background(), src, trig)

		if *fired != 0 {
			t.Errorf("with no due cron window, cooldown must suppress re-eval, fired %d", *fired)
		}
	})
}

func TestMultiTriggerSource_CatchUpKeys(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{
				ID:     "gd-fired-1",
				Type:   "task",
				Status: "open",
				Labels: []string{"trigger:gd-multi-1"},
				Fields: map[string]string{"multi_fire_key": "0:morning:2026-04-12T09:00"},
			},
			{
				ID:     "gd-fired-2",
				Type:   "task",
				Status: "closed",
				Labels: []string{"trigger:gd-multi-1"},
				Fields: map[string]string{"multi_fire_key": "1:any-close:closed:gd-x"},
			},
			{
				ID:     "gd-other",
				Type:   "task",
				Status: "open",
				Labels: []string{"trigger:gd-multi-1"},
				Fields: map[string]string{}, // no multi_fire_key — ignored
			},
		},
	}

	src := NewMultiTriggerSource(slog.Default(), nil)
	keys, err := src.CatchUpKeys(context.Background(), daemon, "gd-multi-1")
	if err != nil {
		t.Fatalf("CatchUpKeys error: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("expected 2 keys, got %d: %v", len(keys), keys)
	}
}

func TestMultiTriggerSource_DrainPending(t *testing.T) {
	src := newMultiSource(time.Now())
	src.engine.OnBeadEvent("closed", "gd-x", nil, nil)
	src.engine.OnBeadEvent("created", "gd-y", nil, nil)
	if got := src.PendingCount(); got != 2 {
		t.Fatalf("PendingCount before drain = %d, want 2", got)
	}
	src.DrainPending()
	if got := src.PendingCount(); got != 0 {
		t.Errorf("PendingCount after drain = %d, want 0", got)
	}
}

// TestMultiTriggerSource_HealsStalledOpenGardener is the acceptance test for
// bug ka-0000003498: the perpetual release-owner + delivery gardeners are
// trigger:multi, so the no_progress_for deadlock heal — which previously ran
// only on the trigger:schedule source — never saw them and they went dark for
// ~24h with last_checked frozen. MultiTriggerSource.LoadTriggers must run the
// same durable reset via healStalledOpenGardeners(…, "trigger:multi").
func TestMultiTriggerSource_HealsStalledOpenGardener(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	// cron "13,43 * * * *" → 30m cadence; 47h ago is unambiguously stale
	// (mirrors the live release-gardener repro on ka-0000006579).
	stale := now.Add(-47 * time.Hour).Format(time.RFC3339)
	capJSON := `{"run_budget":10,"runs_spawned":0,"resumes_without_progress":1,` +
		`"last_resume_reason":"no_progress_for","last_resume_runs_spawned":0,` +
		`"progress":{"surface":{"in_prime":true}},` +
		`"progress_state":{"runs":[{"agent_id":"gd-old-a","progress_score":0},` +
		`{"agent_id":"gd-old-b","progress_score":0},{"agent_id":"gd-old-c","progress_score":0}]}}`
	bead := multiTriggerBead("gd-multi-frozen",
		`[{"kind":"cron","cron":"13,43 * * * *"}]`,
		map[string]string{
			"role":         "gardener",
			"action":       "spawn_agent",
			"gardener":     capJSON,
			"last_checked": stale,
		})

	daemon := &mockTriggerDaemon{beads: []*beadsapi.BeadDetail{bead}}
	src := newMultiSource(now)

	if _, err := src.LoadTriggers(context.Background(), daemon); err != nil {
		t.Fatalf("LoadTriggers() error: %v", err)
	}

	cap := gardenerSpecFromUpdate(t, daemon, "gd-multi-frozen")
	if len(cap.ProgressState) != 0 && string(cap.ProgressState) != "null" {
		t.Errorf("expected ProgressState cleared, got %s", cap.ProgressState)
	}
	if !cap.RunsFinalizedAfter.Equal(now) {
		t.Errorf("expected RunsFinalizedAfter=%v, got %v", now, cap.RunsFinalizedAfter)
	}
	if cap.LastResumeReason != "" {
		t.Errorf("expected LastResumeReason cleared, got %q", cap.LastResumeReason)
	}
	if cap.ResumesWithoutProgress != 0 {
		t.Errorf("expected ResumesWithoutProgress reset, got %d", cap.ResumesWithoutProgress)
	}

	// last_checked re-stamped fresh.
	found := false
	daemon.mu.Lock()
	for i := range daemon.updatedIDs {
		if daemon.updatedIDs[i] == "gd-multi-frozen" &&
			daemon.updatedFields[i]["last_checked"] == now.UTC().Format(time.RFC3339) {
			found = true
		}
	}
	daemon.mu.Unlock()
	if !found {
		t.Errorf("expected last_checked re-stamped to now on gd-multi-frozen")
	}

	// Audit comment records the recovery.
	commented := false
	for _, id := range daemon.commentedIDs {
		if id == "gd-multi-frozen" {
			commented = true
		}
	}
	if !commented {
		t.Errorf("expected an auto-heal audit comment on gd-multi-frozen, got %v", daemon.commentedIDs)
	}
}

// gardenermodeRecorder is a local gardenermode.Writer that records the field map
// gardenermode.Set writes plus the label add/remove/comment calls in order. It
// is the Task-4 fakeWriter reduced to what this test needs: we only read back
// w.fields (to build the flipped trigger bead) and w.labelsAdded/Removed (to
// prove the schedule loader can no longer claim the bead).
type gardenermodeRecorder struct {
	fields       map[string]any
	labelsAdded  []string
	labelsRemove []string
}

func (r *gardenermodeRecorder) UpdateBeadFieldsTyped(_ context.Context, _ string, fl map[string]any) error {
	r.fields = fl
	return nil
}

func (r *gardenermodeRecorder) AddLabel(_ context.Context, _, l string) error {
	r.labelsAdded = append(r.labelsAdded, l)
	return nil
}

func (r *gardenermodeRecorder) RemoveLabel(_ context.Context, _, l string) error {
	r.labelsRemove = append(r.labelsRemove, l)
	return nil
}
func (r *gardenermodeRecorder) AddComment(_ context.Context, _, _, _ string) error { return nil }

// subtreeDaemon is a scheduler.TriggerDaemon (conditions.BeadClient +
// AncestorClient) whose IsAncestor answer is driven by a fixed child→parent
// map, so the in_subtree_of leaf on the flipped gardener's Match resolves
// exactly the "inside the epic" vs "outside" cases. Mirrors edgeHydrDaemon's
// ancestry walk; kept separate so the two tests don't fight over one fixture.
type subtreeDaemon struct {
	parent map[string]string // child id -> parent id
}

func (d *subtreeDaemon) ListBeadsFiltered(_ context.Context, _ beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	return &beadsapi.ListBeadsResult{}, nil
}

func (d *subtreeDaemon) IsAncestor(_ context.Context, root, child string) (bool, error) {
	cur := child
	for {
		p, ok := d.parent[cur]
		if !ok || p == "" {
			return false, nil
		}
		if p == root {
			return true, nil
		}
		cur = p
	}
}

// TestMultiSource_FlippedPlanGardener_FiresOnSubtreeEvent pins the contract
// between gardenermode.Set's emitted triggers[] and this loader: a gardener
// flipped to event mode fires when a bead INSIDE its epic subtree closes,
// does not fire for an outside bead, and its trigger:schedule label is gone
// so the schedule loader cannot double-claim it.
func TestMultiSource_FlippedPlanGardener_FiresOnSubtreeEvent(t *testing.T) {
	// 1. Produce the flipped bead exactly the way prod will: run the real
	//    gardenermode.Set against a recorder so the triggers[] JSON and the
	//    rewritten label set come from the producer, not a hand-built fixture.
	orig := &beadsapi.BeadDetail{
		ID:     "gd-TRIG",
		Labels: []string{"trigger:schedule", "gardener-epic:gd-EPIC"},
		Fields: map[string]string{"cron": "7,37 * * * *"},
	}
	w := &gardenermodeRecorder{}
	if _, err := gardenermode.Set(context.Background(), w, orig, gardenermode.ModeEvent,
		gardenermode.Opts{Operator: "test"}); err != nil {
		t.Fatalf("gardenermode.Set: %v", err)
	}

	// 2. Build the multi-source trigger bead fixture from the recorder's
	//    writes: the flip clears cron and writes triggers[], swaps
	//    trigger:schedule → trigger:multi, and keeps gardener-epic.
	triggersJSON, ok := w.fields["triggers"].(string)
	if !ok || triggersJSON == "" {
		t.Fatalf("Set did not write a triggers JSON string, fields=%+v", w.fields)
	}
	rewritten := rewriteLabels(orig.Labels, w.labelsAdded, w.labelsRemove)
	if slices.Contains(rewritten, "trigger:schedule") {
		t.Fatalf("flip must drop trigger:schedule so the schedule loader cannot "+
			"double-claim the gardener; rewritten labels = %v", rewritten)
	}
	if !slices.Contains(rewritten, "trigger:multi") {
		t.Fatalf("flip must add trigger:multi; rewritten labels = %v", rewritten)
	}

	flippedBead := multiTriggerBead("gd-TRIG", triggersJSON, nil)
	flippedBead.Labels = rewritten
	trig := ParseTriggerDef(flippedBead, "multi")
	if len(trig.Triggers) != 2 || trig.Triggers[0].Kind != scheduler.TriggerKindBeadEvent {
		t.Fatalf("parsed triggers[] wrong shape: %+v", trig.Triggers)
	}

	// The event entry debounces 10m; drive the engine with a fixed clock so
	// the settled-debounce fire is deterministic. Pin SourceData.LastRun to
	// t0 so the entry-1 fallback watchdog cron never fires in either case —
	// this test is about the event entry, not the watchdog.
	t0 := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	trig.SourceData = multiTriggerData{LastRun: t0}

	newSrc := func() *MultiTriggerSource {
		s := newMultiSource(t0)
		s.daemon = &subtreeDaemon{parent: map[string]string{
			"gd-CHILD": "gd-EPIC", // inside the watched subtree
			// gd-OUTSIDE has no ancestry → IsAncestor(gd-EPIC, gd-OUTSIDE) = false
		}}
		return s
	}

	fireOnce := func(t *testing.T, s *MultiTriggerSource, childID string) []TriggerMatch {
		t.Helper()
		// A subtree bead closes.
		s.engine.OnBeadEvent("closed", childID, map[string]any{
			"id":     childID,
			"type":   "task",
			"status": "closed",
		}, []string{"project:gardenio"})

		// First Evaluate at t0 only ARMS the 10m debounce window (no fire).
		if matches, err := s.Evaluate(context.Background(), trig); err != nil {
			t.Fatalf("Evaluate (arm): %v", err)
		} else if len(matches) != 0 {
			t.Fatalf("expected debounce to arm without firing, got %d matches", len(matches))
		}
		// Advance past the 10m debounce deadline; the settled burst fires now.
		s.nowFunc = func() time.Time { return t0.Add(11 * time.Minute) }
		matches, err := s.Evaluate(context.Background(), trig)
		if err != nil {
			t.Fatalf("Evaluate (fire): %v", err)
		}
		return matches
	}

	// 3. A close INSIDE the epic subtree fires exactly once, off the
	//    bead_event entry (index 0), carrying the closing bead's id.
	t.Run("inside subtree fires", func(t *testing.T) {
		matches := fireOnce(t, newSrc(), "gd-CHILD")
		if len(matches) != 1 {
			t.Fatalf("expected exactly one fire for an in-subtree close, got %d", len(matches))
		}
		if got := matches[0].Fields["multi_entry_idx"]; got != "0" {
			t.Errorf("expected the bead_event entry (index 0) to fire, got entry %q", got)
		}
		if got := matches[0].Fields["multi_entry_kind"]; got != scheduler.TriggerKindBeadEvent {
			t.Errorf("multi_entry_kind = %q, want %q", got, scheduler.TriggerKindBeadEvent)
		}
		if got := matches[0].TemplateVars["{{event_bead_id}}"]; got != "gd-CHILD" {
			t.Errorf("event_bead_id = %q, want gd-CHILD", got)
		}
	})

	// 4. A close OUTSIDE the subtree does not fire (in_subtree_of is false and
	//    the fallback watchdog cron is suppressed by LastRun==t0).
	t.Run("outside subtree does not fire", func(t *testing.T) {
		matches := fireOnce(t, newSrc(), "gd-OUTSIDE")
		if len(matches) != 0 {
			t.Fatalf("expected no fire for an out-of-subtree close, got %d matches: %+v",
				len(matches), matches)
		}
	})
}

// rewriteLabels applies gardenermode.Set's label add/remove writes to a starting
// label set, reproducing the bead's on-disk label state after the flip.
func rewriteLabels(start, added, removed []string) []string {
	out := append([]string(nil), start...)
	for _, r := range removed {
		out = slices.DeleteFunc(out, func(l string) bool { return l == r })
	}
	for _, a := range added {
		if !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	return out
}

// openChildrenDaemon answers a parent-scoped bead_query with a fixed open
// count, so a first-fire gate reads "the plan has open steps" or not.
type openChildrenDaemon struct{ open int }

func (d *openChildrenDaemon) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	if q.Parent == "" {
		return &beadsapi.ListBeadsResult{}, nil
	}
	return &beadsapi.ListBeadsResult{Total: d.open}, nil
}

func (d *openChildrenDaemon) IsAncestor(context.Context, string, string) (bool, error) {
	return false, nil
}

// firstFirePlanGardener is an event-mode plan gardener as plangardener.Bind
// authors it (the gardenermode.EventTriggers shape with an open-children
// preflight on the watchdog), sealed to an admin and never fired.
func firstFirePlanGardener(t *testing.T, id string, fields map[string]string) *beadsapi.BeadDetail {
	t.Helper()
	gate := &conditions.Expression{Leaf: conditions.Leaf{
		Type: "bead_query", Op: ">", Value: "0", Parent: "gd-PLAN",
		Types: []string{"task", "bug", "epic"}, Statuses: []string{"open", "in_progress", "blocked"},
	}}
	raw, err := json.Marshal(gardenermode.EventTriggers("gd-PLAN", "", "17 4 * * *", "America/Los_Angeles", gate))
	if err != nil {
		t.Fatal(err)
	}
	f := map[string]string{
		"action":        "spawn_agent",
		"role":          "plan-gardener",
		"spawn_lineage": `{"v":1,"author":"oidc:owner@example.com","digest":"d","iat":1}`,
	}
	maps.Copy(f, fields)
	b := multiTriggerBead(id, string(raw), f)
	b.Labels = append(b.Labels, "role:gardener", "plan-gardener", "gardener-epic:gd-PLAN", "project:gardenio")
	return b
}

// TestMultiTriggerSource_FirstFireQualification pins which plan gardeners the
// engine fires once on load (gd-Vk2ADepJZ2): only a sealed, never-fired,
// not-failing event-mode plan gardener. A re-enabled gardener that already
// ran, an unsealed one, one whose last fire failed, and a non-plan gardener do
// not qualify.
func TestMultiTriggerSource_FirstFireQualification(t *testing.T) {
	cases := []struct {
		name string
		bead *beadsapi.BeadDetail
		want bool
	}{
		{"sealed and never fired", firstFirePlanGardener(t, "gd-new", nil), true},
		{"unsealed", firstFirePlanGardener(t, "gd-unsealed", map[string]string{"spawn_lineage": ""}), false},
		{"sealed by a non-oidc author", firstFirePlanGardener(t, "gd-tok", map[string]string{"spawn_lineage": `{"author":"token:x"}`}), false},
		{"already fired (last_run)", firstFirePlanGardener(t, "gd-ran", map[string]string{"last_run": "2026-10-31T10:00:00Z"}), false},
		{"already fired (runs_spawned)", firstFirePlanGardener(t, "gd-runs", map[string]string{"gardener": `{"assign_bead":"gd-PLAN","runs_spawned":2}`}), false},
		{"has a prior agent", firstFirePlanGardener(t, "gd-prior", map[string]string{"last_agent_id": "gd-agent1"}), false},
		{"last fire failed", firstFirePlanGardener(t, "gd-failed", map[string]string{"last_fire_error": "HTTP 403: spawn denied"}), false},
	}
	notPlan := firstFirePlanGardener(t, "gd-notplan", nil)
	notPlan.Labels = slices.DeleteFunc(notPlan.Labels, func(l string) bool { return l == "plan-gardener" })
	cases = append(cases, struct {
		name string
		bead *beadsapi.BeadDetail
		want bool
	}{"not a plan gardener", notPlan, false})

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := NewMultiTriggerSource(slog.Default(), nil)
			triggers, err := src.LoadTriggers(context.Background(), &mockTriggerDaemon{beads: []*beadsapi.BeadDetail{tc.bead}})
			if err != nil || len(triggers) != 1 {
				t.Fatalf("LoadTriggers: %d triggers, err %v", len(triggers), err)
			}
			data, _ := triggers[0].SourceData.(multiTriggerData)
			if data.FirstFire != tc.want {
				t.Errorf("FirstFire = %v, want %v", data.FirstFire, tc.want)
			}
			if tc.want && data.FirstFireGate == nil {
				t.Error("a first fire must carry the watchdog's open-children gate")
			}
		})
	}
}

// TestMultiTriggerSource_FirstFireEvaluate: with no event pending and the cron
// out of window, a first-fire plan gardener fires once off its plan-activity
// entry, with a fixed dedup key the engine's seen set holds; with no open
// steps under the plan the gate holds it back.
func TestMultiTriggerSource_FirstFireEvaluate(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	load := func(t *testing.T, open int) (*MultiTriggerSource, TriggerDef) {
		t.Helper()
		src := newMultiSource(now)
		triggers, err := src.LoadTriggers(context.Background(), &mockTriggerDaemon{beads: []*beadsapi.BeadDetail{firstFirePlanGardener(t, "gd-new", nil)}})
		if err != nil || len(triggers) != 1 {
			t.Fatalf("LoadTriggers: %d triggers, err %v", len(triggers), err)
		}
		// The watchdog fires at 04:17 daily; pin last_run-equivalent state so
		// only the first fire can produce a match at noon.
		data := triggers[0].SourceData.(multiTriggerData)
		data.LastRun = now.Add(-time.Hour)
		triggers[0].SourceData = data
		src.daemon = &openChildrenDaemon{open: open}
		return src, triggers[0]
	}

	t.Run("open steps: fires once", func(t *testing.T) {
		src, trig := load(t, 2)
		matches, err := src.Evaluate(context.Background(), trig)
		if err != nil || len(matches) != 1 {
			t.Fatalf("Evaluate: %d matches, err %v", len(matches), err)
		}
		m := matches[0]
		if m.Key != "0:plan-activity:first-fire" {
			t.Errorf("key = %q, want the fixed first-fire key", m.Key)
		}
		if m.Fields["multi_entry_kind"] != multiFirstFireKind || m.Fields["multi_entry_idx"] != "0" {
			t.Errorf("fields = %v, want the first_fire kind on entry 0", m.Fields)
		}
		again, _ := src.Evaluate(context.Background(), trig)
		if len(again) != 1 || again[0].Key != m.Key {
			t.Errorf("a repeat evaluation must offer the SAME key so the engine's seen set dedups it, got %+v", again)
		}
	})

	t.Run("no open steps: gate holds it back", func(t *testing.T) {
		src, trig := load(t, 0)
		if matches, err := src.Evaluate(context.Background(), trig); err != nil || len(matches) != 0 {
			t.Fatalf("Evaluate: %d matches, err %v; want none with no open steps", len(matches), err)
		}
	})
}

// wedgedMultiGardenerBead is an event-mode plan gardener (trigger:multi) that
// an erroneous claim left in_progress, last checked at lastChecked.
func wedgedMultiGardenerBead(t *testing.T, id, lastChecked string) *beadsapi.BeadDetail {
	t.Helper()
	b := firstFirePlanGardener(t, id, map[string]string{"last_checked": lastChecked, "last_run": "2026-10-31T10:00:00Z"})
	b.Status = "in_progress"
	b.Assignee = "gardenio-dead-pod-xyz"
	return b
}

// TestMultiTriggerSource_HealsWedgedEventGardener pins gd-7XjRN5uCUW: the multi
// loader runs the schedule source's wedged-trigger heal over trigger:multi, so
// a stale in_progress event-mode gardener is released to open, commented, and
// loaded on the same pass instead of dropping out of the engine silently.
func TestMultiTriggerSource_HealsWedgedEventGardener(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	stale := now.Add(-3 * time.Hour).Format(time.RFC3339)
	daemon := &mockTriggerDaemon{beads: []*beadsapi.BeadDetail{wedgedMultiGardenerBead(t, "gd-wedged", stale)}}

	triggers, err := newMultiSource(now).LoadTriggers(context.Background(), daemon)
	if err != nil {
		t.Fatalf("LoadTriggers() error: %v", err)
	}
	if len(daemon.updatedBeadIDs) != 1 || daemon.updatedBeadIDs[0] != "gd-wedged" {
		t.Fatalf("expected one UpdateBead on gd-wedged, got %v", daemon.updatedBeadIDs)
	}
	req := daemon.updatedBeadReqs[0]
	if req.Status == nil || *req.Status != "open" {
		t.Errorf("expected status reset to open, got %v", req.Status)
	}
	if req.Assignee == nil || *req.Assignee != "" {
		t.Errorf("expected assignee cleared, got %v", req.Assignee)
	}
	if len(triggers) != 1 || triggers[0].ID != "gd-wedged" {
		t.Fatalf("expected the healed gardener to load on the same pass, got %d: %+v", len(triggers), triggers)
	}
	if !slices.Contains(daemon.commentedIDs, "gd-wedged") {
		t.Errorf("expected an auto-heal audit comment on gd-wedged, got %v", daemon.commentedIDs)
	}
}

// TestMultiTriggerSource_WedgedHealKeepsGuardrails: the multi heal keeps the
// schedule heal's guardrails exactly. A fresh in_progress claim (inside
// wedgedTriggerStaleAfter) and an operator-disabled one are left alone, and
// the multi loader only touches trigger:multi beads.
func TestMultiTriggerSource_WedgedHealKeepsGuardrails(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	stale := now.Add(-3 * time.Hour).Format(time.RFC3339)

	fresh := wedgedMultiGardenerBead(t, "gd-fresh", now.Add(-2*time.Minute).Format(time.RFC3339))
	disabled := wedgedMultiGardenerBead(t, "gd-disabled", stale)
	disabled.Fields["enabled"] = "false"
	scheduleBead := wedgedTriggerBead("gd-sched", stale) // trigger:schedule: the schedule source's to heal

	daemon := &mockTriggerDaemon{beads: []*beadsapi.BeadDetail{fresh, disabled, scheduleBead}}
	if _, err := newMultiSource(now).LoadTriggers(context.Background(), daemon); err != nil {
		t.Fatalf("LoadTriggers() error: %v", err)
	}
	if len(daemon.updatedBeadIDs) != 0 {
		t.Errorf("expected no release, got %v", daemon.updatedBeadIDs)
	}
	if len(daemon.commentedIDs) != 0 {
		t.Errorf("expected no heal comment, got %v", daemon.commentedIDs)
	}
}

// TestMultiTriggerSource_FirstFireAfterReseal pins gd-wT7Q0kFTbT: a refused
// first fire (last_fire_error) disqualifies a plan gardener only while the
// error is not older than its seal. A reseal after the refusal makes it
// eligible again; a refusal after that reseal stamps a newer error and stops
// it, so each seal gets one try and there is no retry loop. A missing or
// unreadable timestamp keeps the old behaviour (disqualified).
func TestMultiTriggerSource_FirstFireAfterReseal(t *testing.T) {
	refusedAt := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	seal := func(iat time.Time) string {
		return fmt.Sprintf(`{"v":1,"author":"oidc:owner@example.com","digest":"d","iat":%d}`, iat.Unix())
	}
	failed := func(sealed time.Time, errAt string) map[string]string {
		return map[string]string{
			"spawn_lineage":      seal(sealed),
			"last_fire_error":    "HTTP 403: spawn denied: spawn_lineage seal is stale",
			"last_fire_error_at": errAt,
		}
	}
	at := refusedAt.Format(time.RFC3339)
	for _, tc := range []struct {
		name   string
		fields map[string]string
		want   bool
	}{
		{"resealed after the refusal", failed(refusedAt.Add(2*time.Hour), at), true},
		{"refused again after the reseal", failed(refusedAt.Add(-time.Hour), at), false},
		{"refused in the second the seal was stamped", failed(refusedAt, at), false},
		{"reseal inside the clock margin", failed(refusedAt.Add(firstFireResealMargin), at), false},
		{"no last_fire_error_at", failed(refusedAt.Add(2*time.Hour), ""), false},
		{"unreadable last_fire_error_at", failed(refusedAt.Add(2*time.Hour), "yesterday"), false},
		{"seal without iat", map[string]string{
			"spawn_lineage":      `{"v":1,"author":"oidc:owner@example.com","digest":"d"}`,
			"last_fire_error":    "HTTP 403: spawn denied",
			"last_fire_error_at": at,
		}, false},
		{"resealed but already fired", func() map[string]string {
			f := failed(refusedAt.Add(2*time.Hour), at)
			f["last_run"] = "2026-11-01T10:00:00Z"
			return f
		}(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bead := firstFirePlanGardener(t, "gd-reseal", tc.fields)
			triggers, err := NewMultiTriggerSource(slog.Default(), nil).LoadTriggers(context.Background(), &mockTriggerDaemon{beads: []*beadsapi.BeadDetail{bead}})
			if err != nil || len(triggers) != 1 {
				t.Fatalf("LoadTriggers: %d triggers, err %v", len(triggers), err)
			}
			if data, _ := triggers[0].SourceData.(multiTriggerData); data.FirstFire != tc.want {
				t.Errorf("FirstFire = %v, want %v", data.FirstFire, tc.want)
			}
		})
	}
}

// TestMultiTriggerSource_TriggerBookkeepingUpdateIsDropped: an update to a
// trigger bead that only stamps the engine's bookkeeping fields buffers no
// event, so a plan gardener whose subtree holds a trigger is not woken on each
// of its check ticks (gd-OvY1D7G5Il). A real change to that trigger, or the
// same field names on another bead type, still buffers.
func TestMultiTriggerSource_TriggerBookkeepingUpdateIsDropped(t *testing.T) {
	now := time.Date(2026, 11, 4, 19, 19, 24, 0, time.UTC)
	cases := []struct {
		name    string
		bead    *BeadEvent
		pending int
	}{
		{"check stamp on a trigger", &BeadEvent{ID: "gd-trig", Type: "trigger", Changes: map[string]any{
			"fields": map[string]any{}, "last_checked": "x", "last_outcome": "no match", "last_outcome_at": "x",
		}}, 0},
		{"fire stamp on a trigger", &BeadEvent{ID: "gd-trig", Type: "trigger", Changes: map[string]any{
			"fields": map[string]any{}, "last_run": "x", "last_agent_id": "gd-agent", "last_fire_error": "",
		}}, 0},
		{"real change on a trigger", &BeadEvent{ID: "gd-trig", Type: "trigger", Changes: map[string]any{
			"fields": map[string]any{}, "last_checked": "x", "enabled": false,
		}}, 1},
		{"status change on a trigger", &BeadEvent{ID: "gd-trig", Type: "trigger", Changes: map[string]any{"status": "closed"}}, 1},
		{"fields blob alone", &BeadEvent{ID: "gd-trig", Type: "trigger", Changes: map[string]any{"fields": map[string]any{}}}, 1},
		{"same fields on a task", &BeadEvent{ID: "gd-task", Type: "task", Changes: map[string]any{
			"fields": map[string]any{}, "last_checked": "x",
		}}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := newMultiSource(now)
			src.InjectBeadEvent("beads.bead.updated", tc.bead)
			if n := src.PendingCount(); n != tc.pending {
				t.Fatalf("buffered events = %d, want %d", n, tc.pending)
			}
		})
	}
}

// TestMultiTriggerSource_SpendRollupUpdateIsDropped: an update that only
// rewrites a spend rollup's figure buffers no event, so a crew's cost landing
// after its close does not wake the plan gardener a second time
// (gd-yeakVgR34c). The plan rollup is dropped on any bead, the gardener rollup
// only on a trigger; a rollup beside a real change, or a frame naming no
// per-field key, still buffers.
func TestMultiTriggerSource_SpendRollupUpdateIsDropped(t *testing.T) {
	now := time.Date(2026, 11, 4, 23, 25, 9, 0, time.UTC)
	cases := []struct {
		name    string
		bead    *BeadEvent
		pending int
	}{
		{"plan spend on an epic", &BeadEvent{ID: "gd-plan", Type: "epic", Changes: map[string]any{
			"fields": map[string]any{}, planspend.FieldUSD: "1.2345", planspend.FieldSyncedAt: "x",
			planspend.FieldCrew: "3", planspend.FieldLive: "1",
		}}, 0},
		{"plan spend and status on an epic", &BeadEvent{ID: "gd-plan", Type: "epic", Changes: map[string]any{
			"fields": map[string]any{}, planspend.FieldUSD: "1.2345", "status": "closed",
		}}, 1},
		{"fields blob alone on an epic", &BeadEvent{ID: "gd-plan", Type: "epic", Changes: map[string]any{"fields": map[string]any{}}}, 1},
		{"gardener spend on a trigger", &BeadEvent{ID: "gd-trig", Type: "trigger", Changes: map[string]any{
			"fields": map[string]any{}, planspend.GardenerFieldUSD: "2.0000", planspend.GardenerFieldSyncedAt: "x",
		}}, 0},
		{"gardener spend and check stamp on a trigger", &BeadEvent{ID: "gd-trig", Type: "trigger", Changes: map[string]any{
			"fields": map[string]any{}, planspend.GardenerFieldUSD: "2.0000", "last_checked": "x",
		}}, 0},
		{"gardener spend and real change on a trigger", &BeadEvent{ID: "gd-trig", Type: "trigger", Changes: map[string]any{
			"fields": map[string]any{}, planspend.GardenerFieldUSD: "2.0000", "enabled": false,
		}}, 1},
		{"gardener spend on an epic", &BeadEvent{ID: "gd-plan", Type: "epic", Changes: map[string]any{
			"fields": map[string]any{}, planspend.GardenerFieldUSD: "2.0000",
		}}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := newMultiSource(now)
			src.InjectBeadEvent("beads.bead.updated", tc.bead)
			if n := src.PendingCount(); n != tc.pending {
				t.Fatalf("buffered events = %d, want %d", n, tc.pending)
			}
		})
	}
}
