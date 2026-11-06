package scheduler

import (
	"context"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// fakeWakeDaemon implements TriggerDaemon (BeadClient + AncestorClient) for the
// gardener-wake migration regression locks. `total` is the bead_query result
// count used by cron-floor preflight; `ancestor` is the IsAncestor answer used
// by `in_subtree_of` leaves in decision_resolved matches.
type fakeWakeDaemon struct {
	total    int
	ancestor bool
}

func (f *fakeWakeDaemon) ListBeadsFiltered(_ context.Context, _ beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	return &beadsapi.ListBeadsResult{Total: f.total}, nil
}

func (f *fakeWakeDaemon) IsAncestor(_ context.Context, _, _ string) (bool, error) {
	return f.ancestor, nil
}

// The exact triggers[] JSON applied to each live gardener by WS-A
// (ka-0000001383). These string literals are the source of truth this test
// locks: if the production bead field drifts from these, re-sync both. The
// goal is the runbook §8.6 hybrid shape — an event entry for fast response
// plus a cron floor as a slow catch-up — verified to actually fire on the
// intended event via the real ParseTriggers + EvaluateTriggers path.
const (
	// ka-0000007578 — PE-10116 epic-driver, epic ka-0000002626.
	wakeAX = `[{"kind":"bead_event","events":["closed","updated"],"match":{"all":[{"field":"parent","op":"==","value":"ka-0000002626"}]},"throttle":"60s"},{"kind":"decision_resolved","match":{"all":[{"field":"watch_bead_id","op":"in_subtree_of","value":"ka-0000002626"}]}},{"kind":"cron","cron":"7 */6 * * *","timezone":"America/Los_Angeles","preflight":{"all":[{"type":"bead_query","op":">","value":"0","parent":"ka-0000002626","types":["task","bug","epic"],"statuses":["open","in_progress","blocked"]}]}},{"kind":"manual","label":"operator-fire"}]`

	// ka-0000008247 — deploy-mr login/data validation, epic ka-0000006260.
	// Unconditional cron floor: validates a sliding window of live envs each
	// cycle even with an empty backlog (runbook §5.4 reason #3).
	wakeKR = `[{"kind":"bead_event","events":["closed","updated"],"match":{"all":[{"field":"parent","op":"==","value":"ka-0000006260"}]},"throttle":"120s"},{"kind":"cron","cron":"13 */4 * * *","timezone":"America/Los_Angeles"},{"kind":"manual","label":"operator-fire"}]`

	// ka-0000007624 — Resumer Watchdog. Wakes the moment the engine flags a
	// stuck gardener via the gardener-stuck-escalate label; slow cron floor.
	wakeNR = `[{"kind":"bead_event","events":["label.added"],"match":{"all":[{"field":"label:gardener-stuck-escalate","op":"present"}]},"throttle":"60s","debounce":"5s"},{"kind":"cron","cron":"17 */4 * * *","timezone":"America/Los_Angeles"},{"kind":"manual","label":"operator-fire"}]`

	// ka-0000006911 — perf-metrics pipe driver, epic ka-0000005085. Two-mode:
	// epic-driver events + unconditional daily freshness-guard cron floor.
	wakeOO = `[{"kind":"bead_event","events":["closed","updated"],"match":{"all":[{"field":"parent","op":"==","value":"ka-0000005085"}]},"throttle":"60s"},{"kind":"decision_resolved","match":{"all":[{"field":"watch_bead_id","op":"in_subtree_of","value":"ka-0000005085"}]}},{"kind":"cron","cron":"37 8 * * *","timezone":"UTC"},{"kind":"manual","label":"operator-fire"}]`
)

// mustParseWake parses a gardener's triggers[] literal and fails the test on a
// parse error or empty result (which would make the bridge silently fall back
// to the legacy single cron entry — the exact failure mode this test guards).
func mustParseWake(t *testing.T, raw string, wantLen int) []TriggerEntry {
	t.Helper()
	entries, err := ParseTriggers(raw)
	if err != nil {
		t.Fatalf("ParseTriggers: %v", err)
	}
	if len(entries) != wantLen {
		t.Fatalf("entries len = %d, want %d (empty/short array => bridge falls back to legacy cron)", len(entries), wantLen)
	}
	return entries
}

// fireOnEvent buffers one bead event and reports the resulting FireDecision.
func fireOnEvent(t *testing.T, d TriggerDaemon, entries []TriggerEntry, project, eventType, beadID string, payload map[string]any, labels []string, now time.Time) FireDecision {
	t.Helper()
	e := NewEntryEngine()
	e.OnBeadEvent(eventType, beadID, payload, labels)
	tctx := TriggerEvalContext{TriggerID: "cap", Project: project, Triggers: entries}
	return EvaluateTriggers(context.Background(), e, d, quietLogger(), tctx, now)
}

// fireOnDecision buffers one decision_resolved event and reports the result.
func fireOnDecision(t *testing.T, d TriggerDaemon, entries []TriggerEntry, project, decisionID string, payload map[string]any, now time.Time) FireDecision {
	t.Helper()
	e := NewEntryEngine()
	e.OnDecisionResolved(decisionID, payload, nil)
	tctx := TriggerEvalContext{TriggerID: "cap", Project: project, Triggers: entries}
	return EvaluateTriggers(context.Background(), e, d, quietLogger(), tctx, now)
}

// Per-gardener clocks. eventNow is chosen OUTSIDE every cron window (in the
// entry's timezone) so a negative event-match test cannot be masked by the
// cron floor firing; cronNow lands inside the cron window with the minute
// matched so the floor entry fires. (InWindow checks month/day/hour in the
// entry tz; the cron floor fires whenever in-window with a zero LastRun.)
var (
	// AX/KR/NR are America/Los_Angeles (UTC-7 in June). 10:00 UTC = 03:00 PDT,
	// outside every */4 and */6 hour window.
	axEventNow = time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC)  // 03:00 PDT
	axCronNow  = time.Date(2026, 6, 11, 7, 7, 0, 0, time.UTC)   // 00:07 PDT, matches "7 */6 * * *"
	krEventNow = time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC)  // 03:00 PDT
	krCronNow  = time.Date(2026, 6, 11, 15, 13, 0, 0, time.UTC) // 08:13 PDT, matches "13 */4 * * *"
	nrEventNow = time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC)  // 03:00 PDT
	nrCronNow  = time.Date(2026, 6, 11, 15, 17, 0, 0, time.UTC) // 08:17 PDT, matches "17 */4 * * *"
	// OO is UTC. 03:00 UTC is outside the single hour-8 window.
	ooEventNow = time.Date(2026, 6, 11, 3, 0, 0, 0, time.UTC)
	ooCronNow  = time.Date(2026, 6, 11, 8, 37, 0, 0, time.UTC) // matches "37 8 * * *"
)

// TestGardenerWakeMigration_AX9XM2WLKE locks the PE-10116 epic-driver shape:
// wakes on a direct child close/update, on an in-subtree decision resolution,
// and via the cron floor only while the epic has open children.
func TestGardenerWakeMigration_AX9XM2WLKE(t *testing.T) {
	entries := mustParseWake(t, wakeAX, 4)
	d := &fakeWakeDaemon{total: 3, ancestor: true}

	t.Run("child_close_fires", func(t *testing.T) {
		dec := fireOnEvent(t, d, entries, "monorepo", "closed", "gd-child",
			map[string]any{"parent": "ka-0000002626"}, nil, axEventNow)
		if !dec.Fire || dec.Kind != TriggerKindBeadEvent {
			t.Fatalf("expected bead_event fire on direct-child close, got %+v", dec)
		}
	})

	t.Run("foreign_child_does_not_fire", func(t *testing.T) {
		dec := fireOnEvent(t, d, entries, "monorepo", "closed", "gd-other",
			map[string]any{"parent": "gd-SomeoneElse"}, nil, axEventNow)
		if dec.Fire {
			t.Fatalf("must not wake on a child of a different epic, got %+v", dec)
		}
	})

	t.Run("decision_in_subtree_fires", func(t *testing.T) {
		dec := fireOnDecision(t, d, entries, "monorepo", "gd-dec",
			map[string]any{"watch_bead_id": "gd-child"}, axEventNow)
		if !dec.Fire || dec.Kind != TriggerKindDecisionResolved {
			t.Fatalf("expected decision_resolved fire for in-subtree decision, got %+v", dec)
		}
	})

	t.Run("cron_floor_gated_by_open_children", func(t *testing.T) {
		// preflight holds (open children) -> the cron floor fires.
		e := NewEntryEngine()
		tctx := TriggerEvalContext{TriggerID: "cap", Project: "monorepo", Triggers: entries}
		if dec := EvaluateTriggers(context.Background(), e, &fakeWakeDaemon{total: 2}, quietLogger(), tctx, axCronNow); !dec.Fire || dec.Kind != TriggerKindCron {
			t.Fatalf("cron floor should fire while epic has open children, got %+v", dec)
		}
		// preflight fails (no open children) -> the cron floor no-ops.
		e2 := NewEntryEngine()
		if dec := EvaluateTriggers(context.Background(), e2, &fakeWakeDaemon{total: 0}, quietLogger(), tctx, axCronNow); dec.Fire {
			t.Fatalf("cron floor must no-op when epic has no open children, got %+v", dec)
		}
	})
}

// TestGardenerWakeMigration_kr7VL3Rqgh locks the deploy-mr validation shape:
// wakes on a child fix close/update, plus an UNCONDITIONAL cron floor (the
// sliding-window validation must run each cycle even with an empty backlog).
func TestGardenerWakeMigration_kr7VL3Rqgh(t *testing.T) {
	entries := mustParseWake(t, wakeKR, 3)
	d := &fakeWakeDaemon{}

	t.Run("child_update_fires", func(t *testing.T) {
		dec := fireOnEvent(t, d, entries, "monorepo", "updated", "gd-fix",
			map[string]any{"parent": "ka-0000006260"}, nil, krEventNow)
		if !dec.Fire || dec.Kind != TriggerKindBeadEvent {
			t.Fatalf("expected bead_event fire on epic-child update, got %+v", dec)
		}
	})

	t.Run("unconditional_cron_floor_fires", func(t *testing.T) {
		// No preflight on this floor, and total=0: it must still fire so the
		// gardener re-validates live envs even when the backlog is empty.
		e := NewEntryEngine()
		tctx := TriggerEvalContext{TriggerID: "cap", Project: "monorepo", Triggers: entries}
		if dec := EvaluateTriggers(context.Background(), e, &fakeWakeDaemon{total: 0}, quietLogger(), tctx, krCronNow); !dec.Fire || dec.Kind != TriggerKindCron {
			t.Fatalf("unconditional cron floor must fire regardless of backlog, got %+v", dec)
		}
	})
}

// TestGardenerWakeMigration_nr6v7a90CU locks the Resumer Watchdog shape: wakes
// the moment a gardener-stuck-escalate label lands (edge event), plus a slow
// cron floor. The match is on the label, delivered via the SSE edge-hydrated
// payload (runbook §8.6b); here we exercise the engine path directly.
func TestGardenerWakeMigration_nr6v7a90CU(t *testing.T) {
	entries := mustParseWake(t, wakeNR, 3)
	d := &fakeWakeDaemon{}

	t.Run("stuck_escalate_label_fires", func(t *testing.T) {
		// debounce is set, so the first observation arms the timer rather than
		// firing immediately; a second evaluation after the window settles it.
		// Use an out-of-cron-window clock so the fire is unambiguously the
		// label event, not the cron floor.
		e := NewEntryEngine()
		e.OnBeadEvent("label.added", "gd-stuck", map[string]any{"id": "gd-stuck"},
			[]string{"gardener-stuck-escalate", "role:gardener"})
		tctx := TriggerEvalContext{TriggerID: "cap", Project: "gardenio", Triggers: entries}
		// First pass: debounce arms (no fire yet).
		_ = EvaluateTriggers(context.Background(), e, d, quietLogger(), tctx, nrEventNow)
		// After the 5s debounce window: the settled event fires.
		settled := nrEventNow.Add(6 * time.Second)
		if dec := EvaluateTriggers(context.Background(), e, d, quietLogger(), tctx, settled); !dec.Fire || dec.Kind != TriggerKindBeadEvent {
			t.Fatalf("expected wake when gardener-stuck-escalate label lands, got %+v", dec)
		}
	})

	t.Run("unrelated_label_does_not_fire", func(t *testing.T) {
		dec := fireOnEvent(t, d, entries, "gardenio", "label.added", "gd-x",
			map[string]any{"id": "gd-x"}, []string{"some-other-label"}, nrEventNow)
		if dec.Fire {
			t.Fatalf("must not wake on an unrelated label, got %+v", dec)
		}
	})

	t.Run("cron_floor_fires", func(t *testing.T) {
		e := NewEntryEngine()
		tctx := TriggerEvalContext{TriggerID: "cap", Project: "gardenio", Triggers: entries}
		if dec := EvaluateTriggers(context.Background(), e, d, quietLogger(), tctx, nrCronNow); !dec.Fire || dec.Kind != TriggerKindCron {
			t.Fatalf("slow cron floor should fire at its window, got %+v", dec)
		}
	})
}

// TestGardenerWakeMigration_OOxQl4q1wM locks the perf-metrics pipe-driver shape:
// epic-child events + in-subtree decision + an UNCONDITIONAL daily cron floor
// (the freshness guard must run daily even post-backlog).
func TestGardenerWakeMigration_OOxQl4q1wM(t *testing.T) {
	entries := mustParseWake(t, wakeOO, 4)
	d := &fakeWakeDaemon{ancestor: true}

	t.Run("child_close_fires", func(t *testing.T) {
		dec := fireOnEvent(t, d, entries, "gardenio", "closed", "gd-child",
			map[string]any{"parent": "ka-0000005085"}, nil, ooEventNow)
		if !dec.Fire || dec.Kind != TriggerKindBeadEvent {
			t.Fatalf("expected bead_event fire on direct-child close, got %+v", dec)
		}
	})

	t.Run("decision_in_subtree_fires", func(t *testing.T) {
		dec := fireOnDecision(t, d, entries, "gardenio", "gd-dec",
			map[string]any{"watch_bead_id": "gd-child"}, ooEventNow)
		if !dec.Fire || dec.Kind != TriggerKindDecisionResolved {
			t.Fatalf("expected decision_resolved fire for in-subtree decision, got %+v", dec)
		}
	})

	t.Run("unconditional_daily_floor_fires", func(t *testing.T) {
		e := NewEntryEngine()
		tctx := TriggerEvalContext{TriggerID: "cap", Project: "gardenio", Triggers: entries}
		if dec := EvaluateTriggers(context.Background(), e, &fakeWakeDaemon{total: 0}, quietLogger(), tctx, ooCronNow); !dec.Fire || dec.Kind != TriggerKindCron {
			t.Fatalf("unconditional daily freshness floor must fire, got %+v", dec)
		}
	})
}
