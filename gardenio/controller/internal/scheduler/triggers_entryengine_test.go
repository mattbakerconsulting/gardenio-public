package scheduler

import (
	"context"
	"testing"
	"time"

	"gardenio/controller/internal/scheduler/conditions"
)

// quietLogger is defined in orphan_sweep_test.go (same package).

// --- matchesEventType ---

func TestMatchesEventType(t *testing.T) {
	tests := []struct {
		name   string
		filter []string
		topic  string
		want   bool
	}{
		{"empty filter matches all", nil, "closed", true},
		{"exact short match", []string{"closed"}, "closed", true},
		{"short matches fully-qualified topic", []string{"closed"}, "beads.bead.closed", true},
		{"fully-qualified filter matches", []string{"beads.bead.closed"}, "beads.bead.closed", true},
		{"no match", []string{"created", "updated"}, "closed", false},
		{"topic without dot", []string{"mail_sent"}, "mail_sent", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := matchesEventType(tt.filter, tt.topic); got != tt.want {
				t.Errorf("matchesEventType(%v, %q) = %v, want %v", tt.filter, tt.topic, got, tt.want)
			}
		})
	}
}

// --- withLabels ---

func TestWithLabels(t *testing.T) {
	payload := map[string]any{"a": "1", "b": 2}
	out := withLabels(payload, []string{"x", "y"})
	// Original payload must be untouched (out-of-place copy).
	if _, ok := payload["labels"]; ok {
		t.Error("withLabels mutated the source payload")
	}
	if out["a"] != "1" || out["b"] != 2 {
		t.Errorf("copied keys lost: %+v", out)
	}
	labels, ok := out["labels"].([]any)
	if !ok {
		t.Fatalf("labels key type = %T, want []any", out["labels"])
	}
	if len(labels) != 2 || labels[0] != "x" || labels[1] != "y" {
		t.Errorf("labels = %v, want [x y]", labels)
	}
}

func TestWithLabels_EmptyLabels(t *testing.T) {
	out := withLabels(map[string]any{"k": "v"}, nil)
	labels, ok := out["labels"].([]any)
	if !ok || len(labels) != 0 {
		t.Errorf("empty labels should give empty []any, got %T %v", out["labels"], out["labels"])
	}
}

// --- eventCause ---

func TestEventCause(t *testing.T) {
	bead := TriggerEvent{EventType: "closed", BeadID: "gd-9"}
	if got := eventCause(TriggerEntry{Kind: TriggerKindBeadEvent}, bead); got != "bead_event closed gd-9" {
		t.Errorf("bead cause = %q", got)
	}
	dec := TriggerEvent{BeadID: "dec-1"}
	if got := eventCause(TriggerEntry{Kind: TriggerKindDecisionResolved}, dec); got != "decision_resolved dec-1" {
		t.Errorf("decision cause = %q", got)
	}
	// Unknown kind falls back to the kind string.
	if got := eventCause(TriggerEntry{Kind: "weird"}, bead); got != "weird" {
		t.Errorf("fallback cause = %q", got)
	}
}

// --- exported wrappers ---

func TestExportedWrappers(t *testing.T) {
	e := NewEntryEngine()
	now := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)

	// RecordFire + InThrottleWindow round-trip.
	e.RecordFire("sched", 0, now)
	if !e.InThrottleWindow("sched", 0, time.Minute, now.Add(10*time.Second)) {
		t.Error("InThrottleWindow should report throttled inside window")
	}

	// SnapshotEvents wrapper mirrors the private method.
	e.OnBeadEvent("closed", "gd-1", nil, nil)
	if len(e.SnapshotEvents()) != 1 {
		t.Errorf("SnapshotEvents len = %d, want 1", len(e.SnapshotEvents()))
	}

	// TickDebounce + DrainSettledDebounce wrappers.
	evt := TriggerEvent{Kind: TriggerKindBeadEvent, BeadID: "gd-2", Time: now}
	if fire, _ := e.TickDebounce("sched", 1, 5*time.Second, evt, now); fire {
		t.Error("TickDebounce should open window without firing")
	}
	if _, ok := e.DrainSettledDebounce("sched", 1, now.Add(time.Second)); ok {
		t.Error("DrainSettledDebounce before deadline should be false")
	}
	if _, ok := e.DrainSettledDebounce("sched", 1, now.Add(10*time.Second)); !ok {
		t.Error("DrainSettledDebounce after deadline should fire")
	}
}

// --- EvaluateCronEntry ---

func TestEvaluateCronEntry_Fires(t *testing.T) {
	// now sits exactly on a 9am UTC boundary; LastRun zero (never fired).
	now := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	tctx := TriggerEvalContext{TriggerID: "s", Timezone: "UTC"}
	entry := TriggerEntry{Kind: TriggerKindCron, Cron: "0 9 * * *"}
	dec := EvaluateCronEntry(tctx, entry, 0, now, quietLogger())
	if !dec.Fire {
		t.Fatal("expected cron entry to fire at 9:00 UTC")
	}
	if dec.Kind != TriggerKindCron || dec.EntryIndex != 0 {
		t.Errorf("dec = %+v", dec)
	}
	if dec.Cause != "cron 0 9 * * *" {
		t.Errorf("cause = %q", dec.Cause)
	}
}

func TestEvaluateCronEntry_MissingExpression(t *testing.T) {
	dec := EvaluateCronEntry(TriggerEvalContext{TriggerID: "s"}, TriggerEntry{Kind: TriggerKindCron}, 0, time.Now(), quietLogger())
	if dec.Fire {
		t.Error("missing cron expression must not fire")
	}
}

func TestEvaluateCronEntry_InvalidCron(t *testing.T) {
	dec := EvaluateCronEntry(TriggerEvalContext{TriggerID: "s"}, TriggerEntry{Kind: TriggerKindCron, Cron: "not a cron"}, 0, time.Now(), quietLogger())
	if dec.Fire {
		t.Error("invalid cron must not fire")
	}
}

func TestEvaluateCronEntry_InvalidTimezoneFallsBackToUTC(t *testing.T) {
	now := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	tctx := TriggerEvalContext{TriggerID: "s"}
	entry := TriggerEntry{Kind: TriggerKindCron, Cron: "0 9 * * *", Timezone: "Not/AZone"}
	dec := EvaluateCronEntry(tctx, entry, 0, now, quietLogger())
	// Invalid tz logs a warning and falls back to UTC, where 9:00 fires.
	if !dec.Fire {
		t.Error("invalid tz should fall back to UTC and still fire at 9:00")
	}
}

func TestEvaluateCronEntry_SuppressedByLastRun(t *testing.T) {
	now := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	tctx := TriggerEvalContext{
		TriggerID: "s",
		Timezone:  "UTC",
		// LastRun at or after the computed previous-fire boundary suppresses.
		LastRun: now,
	}
	entry := TriggerEntry{Kind: TriggerKindCron, Cron: "0 9 * * *"}
	dec := EvaluateCronEntry(tctx, entry, 0, now, quietLogger())
	if dec.Fire {
		t.Error("a cron whose boundary was already covered by LastRun must not re-fire")
	}
}

func TestEvaluateCronEntry_OutsideWindow(t *testing.T) {
	// 10:00 is outside the hour window of "0 9 * * *". InWindow checks the
	// hour (not the minute — see CronExpr.InWindow), so a different hour
	// must not fire.
	now := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	tctx := TriggerEvalContext{TriggerID: "s", Timezone: "UTC"}
	entry := TriggerEntry{Kind: TriggerKindCron, Cron: "0 9 * * *"}
	if EvaluateCronEntry(tctx, entry, 0, now, quietLogger()).Fire {
		t.Error("outside the cron hour window must not fire")
	}
}

// --- HasDueCronEntry ---

// HasDueCronEntry backs the bridge engine's cron-due cooldown bypass (bug
// ka-0000004307): a long per-trigger cooldown that divides 24h evenly
// phase-locks the evaluation slots, and when none of them lands inside the
// cron's active window the trigger never fires again. The bypass needs a
// read-only "would a cron entry fire right now?" probe — these tests pin its
// due / not-due / throttled / read-only semantics.
func TestHasDueCronEntry(t *testing.T) {
	inWindow := time.Date(2026, 5, 1, 9, 30, 0, 0, time.UTC) // past the 9:00 boundary
	entries := []TriggerEntry{
		{Kind: TriggerKindBeadEvent, Events: []string{"closed"}}, // non-cron entries are skipped
		{Kind: TriggerKindCron, Cron: "0 9 * * *"},
	}

	t.Run("due when in-window and past last_run", func(t *testing.T) {
		tctx := TriggerEvalContext{TriggerID: "s", Timezone: "UTC", Triggers: entries}
		if !HasDueCronEntry(NewEntryEngine(), quietLogger(), tctx, inWindow) {
			t.Error("cron past its 9:00 boundary with zero LastRun must be due")
		}
	})

	t.Run("not due outside the window", func(t *testing.T) {
		outside := time.Date(2026, 5, 1, 10, 30, 0, 0, time.UTC)
		tctx := TriggerEvalContext{TriggerID: "s", Timezone: "UTC", Triggers: entries}
		if HasDueCronEntry(NewEntryEngine(), quietLogger(), tctx, outside) {
			t.Error("outside the cron hour window must not be due")
		}
	})

	t.Run("not due when last_run already covers the boundary", func(t *testing.T) {
		tctx := TriggerEvalContext{TriggerID: "s", Timezone: "UTC", Triggers: entries, LastRun: inWindow}
		if HasDueCronEntry(NewEntryEngine(), quietLogger(), tctx, inWindow) {
			t.Error("a window already consumed by LastRun must not be due")
		}
	})

	t.Run("per-entry throttle suppresses due", func(t *testing.T) {
		throttled := []TriggerEntry{{Kind: TriggerKindCron, Cron: "0 9 * * *", Throttle: "1h"}}
		engine := NewEntryEngine()
		engine.RecordFire("s", 0, inWindow.Add(-10*time.Minute))
		tctx := TriggerEvalContext{TriggerID: "s", Timezone: "UTC", Triggers: throttled}
		if HasDueCronEntry(engine, quietLogger(), tctx, inWindow) {
			t.Error("an entry inside its throttle window must not hold the bypass open")
		}
	})

	t.Run("read-only: repeated probes stay due", func(t *testing.T) {
		engine := NewEntryEngine()
		tctx := TriggerEvalContext{TriggerID: "s", Timezone: "UTC", Triggers: entries}
		for i := 0; i < 3; i++ {
			if !HasDueCronEntry(engine, quietLogger(), tctx, inWindow) {
				t.Fatalf("probe %d: HasDueCronEntry consumed state — it must be read-only", i)
			}
		}
	})

	t.Run("nil guards", func(t *testing.T) {
		tctx := TriggerEvalContext{TriggerID: "s", Timezone: "UTC", Triggers: entries}
		if HasDueCronEntry(nil, quietLogger(), tctx, inWindow) {
			t.Error("nil engine must not be due")
		}
		if HasDueCronEntry(NewEntryEngine(), nil, tctx, inWindow) {
			t.Error("nil logger must not be due")
		}
	})
}

// --- EvaluateEventEntry ---

func TestEvaluateEventEntry_BeadEventNoMatchExprFires(t *testing.T) {
	e := NewEntryEngine()
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	evt := TriggerEvent{Kind: TriggerKindBeadEvent, EventType: "closed", BeadID: "gd-1", Time: now}
	events := []TriggerEvent{evt}
	entry := TriggerEntry{Kind: TriggerKindBeadEvent, Events: []string{"closed"}}
	tctx := TriggerEvalContext{TriggerID: "s"}

	dec := EvaluateEventEntry(context.Background(), e, nil, quietLogger(), tctx, entry, 0, events, 0, now)
	if !dec.Fire {
		t.Fatal("matching bead_event with no match-expr should fire")
	}
	if dec.Event == nil || dec.Event.BeadID != "gd-1" {
		t.Errorf("fired event = %+v", dec.Event)
	}
	if dec.Cause != "bead_event closed gd-1" {
		t.Errorf("cause = %q", dec.Cause)
	}
}

func TestEvaluateEventEntry_EventTypeFilteredOut(t *testing.T) {
	e := NewEntryEngine()
	now := time.Now()
	events := []TriggerEvent{{Kind: TriggerKindBeadEvent, EventType: "created", BeadID: "gd-1", Time: now}}
	entry := TriggerEntry{Kind: TriggerKindBeadEvent, Events: []string{"closed"}}
	dec := EvaluateEventEntry(context.Background(), e, nil, quietLogger(), TriggerEvalContext{TriggerID: "s"}, entry, 0, events, 0, now)
	if dec.Fire {
		t.Error("created event should not match a closed-only entry")
	}
}

func TestEvaluateEventEntry_KindMismatch(t *testing.T) {
	e := NewEntryEngine()
	now := time.Now()
	// A decision event in the buffer must not satisfy a bead_event entry.
	events := []TriggerEvent{{Kind: TriggerKindDecisionResolved, EventType: "resolved", BeadID: "dec-1", Time: now}}
	entry := TriggerEntry{Kind: TriggerKindBeadEvent}
	if EvaluateEventEntry(context.Background(), e, nil, quietLogger(), TriggerEvalContext{TriggerID: "s"}, entry, 0, events, 0, now).Fire {
		t.Error("kind mismatch must not fire")
	}
}

func TestEvaluateEventEntry_MatchExpressionGates(t *testing.T) {
	e := NewEntryEngine()
	now := time.Now()
	match := &conditions.Expression{Leaf: conditions.Leaf{Field: "watch_project", Op: "==", Value: "gardenio"}}
	entry := TriggerEntry{Kind: TriggerKindBeadEvent, Events: []string{"closed"}, Match: match}
	tctx := TriggerEvalContext{TriggerID: "s"}

	// Payload that satisfies the match → fires.
	pass := []TriggerEvent{{Kind: TriggerKindBeadEvent, EventType: "closed", BeadID: "gd-1", Payload: map[string]any{"watch_project": "gardenio"}, Time: now}}
	if !EvaluateEventEntry(context.Background(), e, nil, quietLogger(), tctx, entry, 0, pass, 0, now).Fire {
		t.Error("payload satisfying match should fire")
	}
	// Payload that fails the match → no fire.
	fail := []TriggerEvent{{Kind: TriggerKindBeadEvent, EventType: "closed", BeadID: "gd-2", Payload: map[string]any{"watch_project": "other"}, Time: now}}
	if EvaluateEventEntry(context.Background(), e, nil, quietLogger(), tctx, entry, 0, fail, 0, now).Fire {
		t.Error("payload failing match should not fire")
	}
}

func TestEvaluateEventEntry_MatchUsesLabels(t *testing.T) {
	e := NewEntryEngine()
	now := time.Now()
	match := &conditions.Expression{Leaf: conditions.Leaf{Field: "label:urgent", Op: "present"}}
	entry := TriggerEntry{Kind: TriggerKindBeadEvent, Events: []string{"closed"}, Match: match}
	events := []TriggerEvent{{Kind: TriggerKindBeadEvent, EventType: "closed", BeadID: "gd-1", Labels: []string{"urgent"}, Time: now}}
	if !EvaluateEventEntry(context.Background(), e, nil, quietLogger(), TriggerEvalContext{TriggerID: "s"}, entry, 0, events, 0, now).Fire {
		t.Error("label present in event should satisfy label:<name> match")
	}
}

func TestEvaluateEventEntry_DebounceDefersThenSettles(t *testing.T) {
	e := NewEntryEngine()
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	entry := TriggerEntry{Kind: TriggerKindBeadEvent, Events: []string{"closed"}}
	tctx := TriggerEvalContext{TriggerID: "s"}
	events := []TriggerEvent{{Kind: TriggerKindBeadEvent, EventType: "closed", BeadID: "gd-1", Time: now}}

	// First pass with debounce: defers (opens window), does not fire.
	if EvaluateEventEntry(context.Background(), e, nil, quietLogger(), tctx, entry, 0, events, 5*time.Second, now).Fire {
		t.Error("first debounced pass should not fire")
	}
	// Second pass after the deadline: the settled-debounce drain fires.
	dec := EvaluateEventEntry(context.Background(), e, nil, quietLogger(), tctx, entry, 0, nil, 5*time.Second, now.Add(10*time.Second))
	if !dec.Fire {
		t.Fatal("settled debounce should fire on a later pass")
	}
	if dec.Event == nil || dec.Event.BeadID != "gd-1" {
		t.Errorf("settled event = %+v", dec.Event)
	}
}

func TestEvaluateEventEntry_MatchErrorDropsEvent(t *testing.T) {
	e := NewEntryEngine()
	now := time.Now()
	// An unknown field op over a present field surfaces an evaluation error;
	// EvaluateEventEntry logs a warning and drops the event (no fire).
	match := &conditions.Expression{Leaf: conditions.Leaf{Field: "watch_project", Op: "bogus_op", Value: "x"}}
	entry := TriggerEntry{Kind: TriggerKindBeadEvent, Events: []string{"closed"}, Match: match}
	events := []TriggerEvent{{Kind: TriggerKindBeadEvent, EventType: "closed", BeadID: "gd-1", Payload: map[string]any{"watch_project": "gardenio"}, Time: now}}
	if EvaluateEventEntry(context.Background(), e, nil, quietLogger(), TriggerEvalContext{TriggerID: "s"}, entry, 0, events, 0, now).Fire {
		t.Error("a match-expression evaluation error should drop the event, not fire")
	}
}

func TestEvaluateEventEntry_NoEventsNoFire(t *testing.T) {
	e := NewEntryEngine()
	dec := EvaluateEventEntry(context.Background(), e, nil, quietLogger(), TriggerEvalContext{TriggerID: "s"},
		TriggerEntry{Kind: TriggerKindBeadEvent}, 0, nil, 0, time.Now())
	if dec.Fire {
		t.Error("no buffered events must not fire")
	}
}

// --- EvaluateEventEntryAll (multi-decision, ka-0000000952) ---

// TestEvaluateEventEntryAll_AllMatchingEventsFire is the regression lock for
// ka-0000000952: two distinct matching events buffered in one cycle must each
// produce a FireDecision, not collapse to the last-arrived one.
func TestEvaluateEventEntryAll_AllMatchingEventsFire(t *testing.T) {
	e := NewEntryEngine()
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	events := []TriggerEvent{
		{Kind: TriggerKindBeadEvent, EventType: "closed", BeadID: "gd-A", Time: now},
		{Kind: TriggerKindBeadEvent, EventType: "closed", BeadID: "gd-B", Time: now.Add(time.Second)},
	}
	entry := TriggerEntry{Kind: TriggerKindBeadEvent, Events: []string{"closed"}}
	tctx := TriggerEvalContext{TriggerID: "s"}

	decs := EvaluateEventEntryAll(context.Background(), e, nil, quietLogger(), tctx, entry, 0, events, 0, now)
	if len(decs) != 2 {
		t.Fatalf("expected one decision per matching event, got %d", len(decs))
	}
	got := map[string]bool{}
	for _, d := range decs {
		if !d.Fire || d.Event == nil {
			t.Fatalf("decision not firing / missing event: %+v", d)
		}
		got[d.Event.BeadID] = true
	}
	if !got["gd-A"] || !got["gd-B"] {
		t.Errorf("both events must fire, got %v", got)
	}

	// The legacy single-decision wrapper still returns the first firing event.
	if dec := EvaluateEventEntry(context.Background(), e, nil, quietLogger(), tctx, entry, 0, events, 0, now); !dec.Fire {
		t.Error("single-decision wrapper should still fire")
	}
}

// TestEvaluateEventEntryAll_NonMatchingFilteredOut verifies the multi-decision
// path applies the same per-event match predicate, so only matching events
// yield decisions.
func TestEvaluateEventEntryAll_NonMatchingFilteredOut(t *testing.T) {
	e := NewEntryEngine()
	now := time.Now()
	events := []TriggerEvent{
		{Kind: TriggerKindBeadEvent, EventType: "closed", BeadID: "gd-A", Time: now},
		{Kind: TriggerKindBeadEvent, EventType: "created", BeadID: "gd-B", Time: now},
	}
	entry := TriggerEntry{Kind: TriggerKindBeadEvent, Events: []string{"closed"}}
	decs := EvaluateEventEntryAll(context.Background(), e, nil, quietLogger(), TriggerEvalContext{TriggerID: "s"}, entry, 0, events, 0, now)
	if len(decs) != 1 || decs[0].Event.BeadID != "gd-A" {
		t.Fatalf("only the closed event should fire, got %+v", decs)
	}
}

// TestEvaluateEventEntryAll_DebounceCollapsesToOne verifies a live debounce
// window still collapses a burst (no decision now; fires once on settle).
func TestEvaluateEventEntryAll_DebounceCollapsesToOne(t *testing.T) {
	e := NewEntryEngine()
	now := time.Now()
	events := []TriggerEvent{
		{Kind: TriggerKindBeadEvent, EventType: "closed", BeadID: "gd-A", Time: now},
		{Kind: TriggerKindBeadEvent, EventType: "closed", BeadID: "gd-B", Time: now.Add(time.Second)},
	}
	entry := TriggerEntry{Kind: TriggerKindBeadEvent, Events: []string{"closed"}}
	tctx := TriggerEvalContext{TriggerID: "s"}
	if decs := EvaluateEventEntryAll(context.Background(), e, nil, quietLogger(), tctx, entry, 0, events, 5*time.Second, now); len(decs) != 0 {
		t.Fatalf("debounce should defer all fires, got %d", len(decs))
	}
	settled := EvaluateEventEntryAll(context.Background(), e, nil, quietLogger(), tctx, entry, 0, nil, 5*time.Second, now.Add(10*time.Second))
	if len(settled) != 1 {
		t.Fatalf("settled debounce should yield exactly one decision, got %d", len(settled))
	}
	// Most-recent matching event carries the settled window.
	if settled[0].Event.BeadID != "gd-B" {
		t.Errorf("settled decision should carry latest event gd-B, got %+v", settled[0].Event)
	}
}

// TestEvaluateTriggersAll_FanInAllEvents verifies the fan-in wrapper returns
// every matching event's decision from the first firing entry.
func TestEvaluateTriggersAll_FanInAllEvents(t *testing.T) {
	e := NewEntryEngine()
	now := time.Now()
	e.OnBeadEvent("closed", "gd-A", nil, nil)
	e.OnBeadEvent("closed", "gd-B", nil, nil)
	tctx := TriggerEvalContext{
		TriggerID: "s",
		Triggers:  []TriggerEntry{{Kind: TriggerKindBeadEvent, Events: []string{"closed"}}},
	}
	decs := EvaluateTriggersAll(context.Background(), e, nil, quietLogger(), tctx, now)
	if len(decs) != 2 {
		t.Fatalf("fan-in should return one decision per event, got %d", len(decs))
	}
}

// TestEvaluateTriggersAll_NilGuards mirrors the single-decision nil guards (ka-0000000952).
func TestEvaluateTriggersAll_NilGuards(t *testing.T) {
	if EvaluateTriggersAll(context.Background(), nil, nil, quietLogger(), TriggerEvalContext{}, time.Now()) != nil {
		t.Error("nil engine should yield no decisions")
	}
	if EvaluateTriggersAll(context.Background(), NewEntryEngine(), nil, nil, TriggerEvalContext{}, time.Now()) != nil {
		t.Error("nil logger should yield no decisions")
	}
}

// --- EvaluateTriggers (fan-in) ---

func TestEvaluateTriggers_NilGuards(t *testing.T) {
	if EvaluateTriggers(context.Background(), nil, nil, quietLogger(), TriggerEvalContext{}, time.Now()).Fire {
		t.Error("nil engine should yield non-firing decision")
	}
	if EvaluateTriggers(context.Background(), NewEntryEngine(), nil, nil, TriggerEvalContext{}, time.Now()).Fire {
		t.Error("nil logger should yield non-firing decision")
	}
}

func TestEvaluateTriggers_ManualKindNeverFires(t *testing.T) {
	e := NewEntryEngine()
	tctx := TriggerEvalContext{TriggerID: "s", Triggers: []TriggerEntry{{Kind: TriggerKindManual}}}
	if EvaluateTriggers(context.Background(), e, nil, quietLogger(), tctx, time.Now()).Fire {
		t.Error("manual trigger must never fire from the evaluator")
	}
}

func TestEvaluateTriggers_UnknownKindSkipped(t *testing.T) {
	e := NewEntryEngine()
	tctx := TriggerEvalContext{TriggerID: "s", Triggers: []TriggerEntry{{Kind: "mystery"}}}
	if EvaluateTriggers(context.Background(), e, nil, quietLogger(), tctx, time.Now()).Fire {
		t.Error("unknown kind must be skipped, not fired")
	}
}

func TestEvaluateTriggers_FirstFiringEntryWins(t *testing.T) {
	e := NewEntryEngine()
	now := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	e.OnBeadEvent("closed", "gd-1", nil, nil)
	tctx := TriggerEvalContext{
		TriggerID: "s",
		Timezone:  "UTC",
		Triggers: []TriggerEntry{
			{Kind: TriggerKindManual},                  // skipped
			{Kind: TriggerKindCron, Cron: "0 9 * * *"}, // fires first
			{Kind: TriggerKindBeadEvent, Events: []string{"closed"}},
		},
	}
	dec := EvaluateTriggers(context.Background(), e, nil, quietLogger(), tctx, now)
	if !dec.Fire {
		t.Fatal("expected a firing decision")
	}
	if dec.Kind != TriggerKindCron || dec.EntryIndex != 1 {
		t.Errorf("first-firing entry = %+v, want cron at index 1", dec)
	}
}

func TestEvaluateTriggers_ThrottleSkipsEntry(t *testing.T) {
	e := NewEntryEngine()
	now := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	// Record a recent fire so the throttle window gates entry 0.
	e.recordFire("s", 0, now)
	tctx := TriggerEvalContext{
		TriggerID: "s",
		Timezone:  "UTC",
		Triggers: []TriggerEntry{
			{Kind: TriggerKindCron, Cron: "0 9 * * *", Throttle: "1h"},
		},
	}
	if EvaluateTriggers(context.Background(), e, nil, quietLogger(), tctx, now.Add(time.Minute)).Fire {
		t.Error("throttled entry should be skipped even when cron window matches")
	}
}

func TestEvaluateTriggers_EventEntryFires(t *testing.T) {
	e := NewEntryEngine()
	now := time.Now()
	e.OnBeadEvent("closed", "gd-7", nil, nil)
	tctx := TriggerEvalContext{
		TriggerID: "s",
		Triggers:  []TriggerEntry{{Kind: TriggerKindBeadEvent, Events: []string{"closed"}}},
	}
	dec := EvaluateTriggers(context.Background(), e, nil, quietLogger(), tctx, now)
	if !dec.Fire || dec.Kind != TriggerKindBeadEvent {
		t.Errorf("expected bead_event fire, got %+v", dec)
	}
}
