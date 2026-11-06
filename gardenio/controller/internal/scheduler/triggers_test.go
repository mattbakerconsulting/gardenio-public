package scheduler

import (
	"testing"
	"time"
)

// --- ParseTriggers ---

func TestParseTriggers_Empty(t *testing.T) {
	for _, in := range []string{"", "  ", "\t\n"} {
		got, err := ParseTriggers(in)
		if err != nil {
			t.Fatalf("ParseTriggers(%q) unexpected error: %v", in, err)
		}
		if got != nil {
			t.Errorf("ParseTriggers(%q) = %v, want nil", in, got)
		}
	}
}

func TestParseTriggers_MixedKinds(t *testing.T) {
	raw := `[
		{"kind":"cron","cron":"0 9 * * *","timezone":"America/Los_Angeles","throttle":"5m"},
		{"kind":"bead_event","events":["closed"],"debounce":"30s","label":"close-watch"},
		{"kind":"decision_resolved"}
	]`
	entries, err := ParseTriggers(raw)
	if err != nil {
		t.Fatalf("ParseTriggers: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("entries len = %d, want 3", len(entries))
	}
	if entries[0].Kind != TriggerKindCron || entries[0].Cron != "0 9 * * *" || entries[0].Timezone != "America/Los_Angeles" || entries[0].Throttle != "5m" {
		t.Errorf("entry[0] = %+v", entries[0])
	}
	if entries[1].Kind != TriggerKindBeadEvent || len(entries[1].Events) != 1 || entries[1].Events[0] != "closed" || entries[1].Debounce != "30s" || entries[1].Label != "close-watch" {
		t.Errorf("entry[1] = %+v", entries[1])
	}
	if entries[2].Kind != TriggerKindDecisionResolved {
		t.Errorf("entry[2] = %+v", entries[2])
	}
}

func TestParseTriggers_InvalidJSON(t *testing.T) {
	_, err := ParseTriggers(`{not json`)
	if err == nil {
		t.Error("expected error on malformed JSON")
	}
}

// --- EntryEngine throttle ---

func TestEntryEngine_ThrottleWindow(t *testing.T) {
	e := NewEntryEngine()
	now := time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC)

	// No prior fire: never throttled.
	if e.inThrottleWindow("s", 0, time.Minute, now) {
		t.Error("inThrottleWindow without prior fire should be false")
	}
	// Throttle disabled (zero / negative): never throttled.
	e.recordFire("s", 0, now)
	if e.inThrottleWindow("s", 0, 0, now.Add(time.Second)) {
		t.Error("zero throttle should never gate")
	}
	// Inside the window: throttled.
	if !e.inThrottleWindow("s", 0, time.Minute, now.Add(30*time.Second)) {
		t.Error("expected throttle inside window")
	}
	// Outside the window: not throttled.
	if e.inThrottleWindow("s", 0, time.Minute, now.Add(2*time.Minute)) {
		t.Error("expected no throttle outside window")
	}
	// Different entry index → independent state.
	if e.inThrottleWindow("s", 1, time.Minute, now.Add(30*time.Second)) {
		t.Error("entry 1 should be independent from entry 0")
	}
}

// --- EntryEngine debounce ---

func TestEntryEngine_DebounceCoalesces(t *testing.T) {
	e := NewEntryEngine()
	now := time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC)

	first := TriggerEvent{Kind: TriggerKindBeadEvent, EventType: "closed", BeadID: "gd-1", Time: now}
	second := TriggerEvent{Kind: TriggerKindBeadEvent, EventType: "closed", BeadID: "gd-2", Time: now.Add(time.Second)}
	third := TriggerEvent{Kind: TriggerKindBeadEvent, EventType: "closed", BeadID: "gd-3", Time: now.Add(2 * time.Second)}

	// Open the window.
	if fire, _ := e.tickDebounce("s", 0, 5*time.Second, first, now); fire {
		t.Error("first event with debounce should not fire immediately")
	}
	// Continue the burst — slides the deadline forward, last-write-wins payload.
	if fire, _ := e.tickDebounce("s", 0, 5*time.Second, second, now.Add(time.Second)); fire {
		t.Error("second event in burst should not fire")
	}
	if fire, _ := e.tickDebounce("s", 0, 5*time.Second, third, now.Add(2*time.Second)); fire {
		t.Error("third event in burst should not fire")
	}

	// Still inside the (extended) window: drainSettledDebounce returns false.
	if _, ok := e.drainSettledDebounce("s", 0, now.Add(5*time.Second)); ok {
		t.Error("drainSettledDebounce should not fire before deadline")
	}

	// After deadline passes: drainSettledDebounce yields the most recent payload.
	settled, ok := e.drainSettledDebounce("s", 0, now.Add(10*time.Second))
	if !ok {
		t.Fatal("drainSettledDebounce should fire once burst settles")
	}
	if settled.BeadID != "gd-3" {
		t.Errorf("debounced payload bead = %q, want gd-3 (last-write-wins)", settled.BeadID)
	}
	// Once drained, the window is gone — second drain returns false.
	if _, ok := e.drainSettledDebounce("s", 0, now.Add(20*time.Second)); ok {
		t.Error("second drain after settle should return false")
	}

	// Zero debounce: tickDebounce fires immediately with the event payload.
	if fire, payload := e.tickDebounce("s", 0, 0, first, now); !fire || payload.BeadID != "gd-1" {
		t.Errorf("zero debounce should fire immediately, got fire=%v payload=%+v", fire, payload)
	}
}

// --- ingest + drain ---

func TestEntryEngine_IngestAndDrainEvents(t *testing.T) {
	e := NewEntryEngine()
	if e.PendingEventCount() != 0 {
		t.Fatal("fresh engine should have 0 pending events")
	}
	e.OnBeadEvent("closed", "gd-1", map[string]any{"x": "y"}, []string{"foo"})
	e.OnDecisionResolved("dec-1", map[string]any{"approve": true}, nil)
	if got := e.PendingEventCount(); got != 2 {
		t.Errorf("after ingest, pending = %d, want 2", got)
	}
	snap := e.snapshotEvents()
	if len(snap) != 2 {
		t.Errorf("snapshot len = %d, want 2", len(snap))
	}
	if snap[0].Kind != TriggerKindBeadEvent || snap[1].Kind != TriggerKindDecisionResolved {
		t.Errorf("snapshot kinds = %q,%q", snap[0].Kind, snap[1].Kind)
	}
	// Snapshot must NOT drain.
	if got := e.PendingEventCount(); got != 2 {
		t.Errorf("after snapshot, pending = %d, want 2 (snapshot must not drain)", got)
	}
	e.DrainEvents()
	if got := e.PendingEventCount(); got != 0 {
		t.Errorf("after DrainEvents, pending = %d, want 0", got)
	}
}

// TestEntryEngine_FreezeKeepsMidPassEvents: an event buffered after
// FreezeEvents is invisible to that pass and survives its DrainEvents, so
// the next pass sees it (gd-IQcMk16PkH: the escalation-coordinator's 'filed'
// event landed mid-pass and was drained unseen).
func TestEntryEngine_FreezeKeepsMidPassEvents(t *testing.T) {
	e := NewEntryEngine()
	e.OnBeadEvent("created", "gd-before", nil, nil)
	e.FreezeEvents()
	e.OnBeadEvent("created", "gd-mid", nil, nil)

	snap := e.snapshotEvents()
	if len(snap) != 1 || snap[0].BeadID != "gd-before" {
		t.Fatalf("frozen pass sees %+v, want only gd-before", snap)
	}
	e.DrainEvents()

	snap = e.snapshotEvents()
	if len(snap) != 1 || snap[0].BeadID != "gd-mid" {
		t.Fatalf("next pass sees %+v, want only gd-mid", snap)
	}
	e.DrainEvents()
	if got := e.PendingEventCount(); got != 0 {
		t.Errorf("after an unfrozen DrainEvents, pending = %d, want 0", got)
	}
}
