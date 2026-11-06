package bridge

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/spawnguard"
)

// TestBeadStateTriggerSource_AssertFiresOnViolation verifies the F5
// fire-on-violation semantic: an assert trigger emits a match ONLY when its
// invariant is violated (a clause evaluates FALSE), and stays silent when the
// invariant holds. This is the inverse of the legacy fire-on-match condition
// path. (ka-0000008157)
func TestBeadStateTriggerSource_AssertFiresOnViolation(t *testing.T) {
	src := NewBeadStateTriggerSource(slog.Default())
	src.mu.Lock()
	src.pending = []beadStateEvent{
		// Invariant: P0 bugs must be assigned. gd-bad violates it (no assignee).
		{Topic: "beads.bead.updated", Bead: &BeadEvent{ID: "gd-bad", Type: "bug", Status: "open", Priority: 0, Assignee: ""}},
		// gd-ok satisfies the invariant (assigned) → silent.
		{Topic: "beads.bead.updated", Bead: &BeadEvent{ID: "gd-ok", Type: "bug", Status: "open", Priority: 0, Assignee: "alice"}},
	}
	src.mu.Unlock()

	trig := TriggerDef{
		ID: "p0-must-be-assigned", Source: "bead", Action: TriggerActionAssert, MaxConcurrent: 10,
		SourceData: beadTriggerData{
			// Pre-filter: only bugs.
			Conditions: []beadCondition{
				{Field: "type", Op: "==", Value: "bug"},
			},
			// Invariant expected to hold: a bug has an assignee.
			Assert: []beadCondition{{Expr: `bead.assignee != ""`}},
		},
	}

	matches, err := src.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected exactly 1 match (the violation), got %d: %+v", len(matches), matches)
	}
	m := matches[0]
	if m.Fields["source_bead_id"] != "gd-bad" {
		t.Errorf("violation should be on gd-bad (unassigned P0 bug), got %q", m.Fields["source_bead_id"])
	}
	if got := m.TemplateVars["{{violated}}"]; got != `bead.assignee != ""` {
		t.Errorf("{{violated}} should name the failed clause, got %q", got)
	}
	if got := m.TemplateVars["{{values}}"]; !strings.Contains(got, "assignee=") {
		t.Errorf("{{values}} should surface observed field values, got %q", got)
	}
}

// TestBeadStateTriggerSource_AssertSilentWhenSatisfied verifies that when the
// invariant holds for every pre-filtered event, the assert trigger produces no
// matches at all. (ka-0000008157)
func TestBeadStateTriggerSource_AssertSilentWhenSatisfied(t *testing.T) {
	src := NewBeadStateTriggerSource(slog.Default())
	src.mu.Lock()
	src.pending = []beadStateEvent{
		{Topic: "beads.bead.updated", Bead: &BeadEvent{ID: "gd-ok1", Type: "bug", Status: "open", Priority: 0, Assignee: "alice"}},
		{Topic: "beads.bead.updated", Bead: &BeadEvent{ID: "gd-ok2", Type: "bug", Status: "open", Priority: 0, Assignee: "bob"}},
	}
	src.mu.Unlock()

	trig := TriggerDef{
		ID: "p0-must-be-assigned", Source: "bead", Action: TriggerActionAssert, MaxConcurrent: 10,
		SourceData: beadTriggerData{
			Conditions: []beadCondition{{Field: "type", Op: "==", Value: "bug"}},
			Assert:     []beadCondition{{Expr: `bead.assignee != ""`}},
		},
	}
	matches, err := src.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(matches) != 0 {
		t.Errorf("invariant holds for all events → assert must stay silent, got %d matches: %+v", len(matches), matches)
	}
}

// TestBeadStateTriggerSource_AssertFailClosed verifies the fail-closed stance
// for assertions: an invariant clause that errors at runtime (e.g. unguarded
// missing-key access) counts as a VIOLATION and fires — a broken assertion must
// not silently pass. This is the opposite of the suppress-gate condition path,
// where a fail-closed error means do-not-fire. (ka-0000008157)
func TestBeadStateTriggerSource_AssertFailClosed(t *testing.T) {
	src := NewBeadStateTriggerSource(slog.Default())
	src.mu.Lock()
	src.pending = []beadStateEvent{
		{Topic: "beads.bead.updated", Bead: &BeadEvent{ID: "gd-1", Type: "task", Status: "open"}},
	}
	src.mu.Unlock()

	trig := TriggerDef{
		ID: "broken-invariant", Source: "bead", Action: TriggerActionAssert, MaxConcurrent: 10,
		SourceData: beadTriggerData{
			// Unguarded missing-key access errors at runtime → treated as a
			// violation (fires), not a silent pass.
			Assert: []beadCondition{{Expr: `bead.nonexistent == "x"`}},
		},
	}
	matches, err := src.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(matches) != 1 {
		t.Errorf("a broken invariant must fire (fail-closed → violation), got %d matches", len(matches))
	}
}

// TestTriggerEngine_AssertAlertDedupAndBypassGuard drives the assert action
// through the full engine: a violated predicate fires a notify alert exactly
// ONCE (dedup), bypasses the SpawnGuard (no agent spawned), and a satisfied
// predicate fires nothing. (ka-0000008157 acceptance + VERIFY)
func TestTriggerEngine_AssertAlertDedupAndBypassGuard(t *testing.T) {
	daemon := &mockTriggerDaemon{}
	// A SpawnGuard with a single rate slot that we immediately consume, so any
	// spawn attempt from here on would be denied. The assert action must never
	// consult the guard (it spawns no agent), so it proceeds regardless.
	guard := spawnguard.New(spawnguard.Config{RateMax: 1, RateWindow: time.Hour})
	if ok, _ := guard.Allow(); !ok {
		t.Fatal("precondition: first Allow() should succeed")
	}
	if ok, _ := guard.Allow(); ok {
		t.Fatal("precondition: guard should now reject (slot exhausted)")
	}

	var alerts []string
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
		Guard:  guard,
		Notifier: func(_ context.Context, _, msg string, _ string) {
			alerts = append(alerts, msg)
		},
	})

	src := NewBeadStateTriggerSource(slog.Default())
	engine.RegisterSource(src)

	trig := TriggerDef{
		ID: "assert-trig", Title: "P0 must be assigned", Source: "bead",
		Action: TriggerActionAssert, OnViolation: "notify", MaxConcurrent: 10,
		// Non-zero LastChecked bypasses the runtime-bootstrap guard.
		LastChecked: timeNow().Add(-time.Hour),
		Prompt:      "VIOLATION on {{bead_id}}: {{violated}} | {{values}}",
		SourceData: beadTriggerData{
			Assert: []beadCondition{{Expr: `bead.assignee != ""`}},
		},
	}
	// Wire the trigger into the source via the mock daemon so evalOne picks it up.
	src.mu.Lock()
	src.pending = []beadStateEvent{
		{Topic: "beads.bead.updated", Bead: &BeadEvent{ID: "gd-bad", Type: "bug", Status: "open", Priority: 0}},
	}
	src.mu.Unlock()

	// Directly fire via the source+engine evalOne path.
	engine.evalOne(context.Background(), src, trig)

	if len(alerts) != 1 {
		t.Fatalf("violation should fire exactly one alert, got %d: %v", len(alerts), alerts)
	}
	if !strings.Contains(alerts[0], "gd-bad") || !strings.Contains(alerts[0], `bead.assignee != ""`) {
		t.Errorf("alert should name the bead and the violated clause, got %q", alerts[0])
	}
	if len(daemon.spawnedNames) != 0 {
		t.Errorf("assert action must NOT spawn an agent, spawned: %v", daemon.spawnedNames)
	}
	if len(daemon.createdBeads) != 0 {
		t.Errorf("on_violation=notify must NOT create a bead, created: %d", len(daemon.createdBeads))
	}

	// Second evaluation over the SAME pending event must dedup → no new alert.
	src.mu.Lock()
	src.pending = []beadStateEvent{
		{Topic: "beads.bead.updated", Bead: &BeadEvent{ID: "gd-bad", Type: "bug", Status: "open", Priority: 0}},
	}
	src.mu.Unlock()
	engine.evalOne(context.Background(), src, trig)
	if len(alerts) != 1 {
		t.Errorf("re-evaluating the same violation must dedup (still 1 alert), got %d: %v", len(alerts), alerts)
	}

	// A now-satisfied predicate fires nothing.
	src.mu.Lock()
	src.pending = []beadStateEvent{
		{Topic: "beads.bead.updated", Bead: &BeadEvent{ID: "gd-ok", Type: "bug", Status: "open", Priority: 0, Assignee: "alice"}},
	}
	src.mu.Unlock()
	engine.evalOne(context.Background(), src, trig)
	if len(alerts) != 1 {
		t.Errorf("satisfied predicate must stay silent, alerts grew to %d: %v", len(alerts), alerts)
	}
}

// TestBeadAssertViolationToMatch_KeyIsPollStable is the direct repro for
// ka-0000006401: the violation dedup key must depend ONLY on the bead ID and the
// violated clause, NOT on the changed-field VALUES of the event. Before the fix
// the key layered on beadEventToMatch's base key, which folds a fingerprint of
// the Changes map (for a watched trigger: last_checked / last_outcome_at,
// restamped every poll) into the key — so two polls of the SAME continuous
// violation produced two DIFFERENT keys and the seen-set never deduped them (36
// dup alerts in 7h). Same bead + same clause + different changed values must now
// collapse to one key; a different clause must still differ.
func TestBeadAssertViolationToMatch_KeyIsPollStable(t *testing.T) {
	clause := `bead.status == "open" && has(bead.last_run)`

	// Two polls of the SAME continuous violation on the SAME bead: identical
	// except the value-bearing Changes map (last_checked/last_outcome_at) the
	// engine restamps every ~5min. This is exactly the prod shape from the bug.
	poll1 := beadStateEvent{Topic: "beads.bead.updated", Bead: &BeadEvent{
		ID: "gd-dark", Type: "trigger", Status: "open",
		Changes: map[string]any{
			"last_checked":    "2026-08-27T21:12:21Z",
			"last_outcome_at": "2026-08-27T21:12:21Z",
		},
	}}
	poll2 := beadStateEvent{Topic: "beads.bead.updated", Bead: &BeadEvent{
		ID: "gd-dark", Type: "trigger", Status: "open",
		Changes: map[string]any{
			"last_checked":    "2026-08-27T21:17:34Z",
			"last_outcome_at": "2026-08-27T21:17:34Z",
		},
	}}

	k1 := beadAssertViolationToMatch(poll1, clause, "status=open").Key
	k2 := beadAssertViolationToMatch(poll2, clause, "status=open").Key
	if k1 != k2 {
		t.Errorf("continuous violation on the same bead+clause must produce ONE stable key across polls;\n poll1 key = %q\n poll2 key = %q", k1, k2)
	}
	if want := "gd-dark:violated:"; !strings.HasPrefix(k1, want) {
		t.Errorf("violation key should be <beadID>:violated:<clauseFP>, got %q", k1)
	}
	// The changed-field values must NOT appear in the key at all.
	for _, leak := range []string{"last_checked", "last_outcome_at", "21:12:21", "21:17:34"} {
		if strings.Contains(k1, leak) {
			t.Errorf("dedup key must not encode changed-field values, but %q contains %q", k1, leak)
		}
	}

	// A DIFFERENT clause on the SAME bead+event must still get a distinct key
	// (acceptance: multi-clause violations alert separately).
	kOther := beadAssertViolationToMatch(poll1, `bead.enabled == "true"`, "enabled=false").Key
	if kOther == k1 {
		t.Errorf("distinct clauses must produce distinct keys, both were %q", k1)
	}
}

// TestTriggerEngine_AssertEpisodeDedupAndRearm is the paired flood + recovery
// acceptance test for ka-0000006401. A watched bead in CONTINUOUS violation
// across many polls (each restamping last_checked, so each event carries a fresh
// Changes map) must fire exactly ONE alert; after the invariant RECOVERS the
// alert must re-arm so a subsequent genuine violation fires again — once per
// episode, matching the SweepGardenerLiveness dedup.
func TestTriggerEngine_AssertEpisodeDedupAndRearm(t *testing.T) {
	daemon := &mockTriggerDaemon{}
	var alerts []string
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon:   daemon,
		Logger:   slog.Default(),
		Notifier: func(_ context.Context, _, msg string, _ string) { alerts = append(alerts, msg) },
	})
	src := NewBeadStateTriggerSource(slog.Default())
	engine.RegisterSource(src)

	trig := TriggerDef{
		ID: "fleet-liveness", Title: "gardener liveness", Source: "bead",
		Action: TriggerActionAssert, OnViolation: "notify", MaxConcurrent: 10,
		LastChecked: timeNow().Add(-time.Hour), // bypass runtime-bootstrap guard
		Prompt:      "DARK: {{bead_id}} violated {{violated}}",
		SourceData: beadTriggerData{
			// Invariant: a live gardener must be enabled. gd-dark violates it.
			Assert: []beadCondition{{Expr: `bead.enabled == "true"`}},
		},
	}

	// darkPoll builds one poll of the continuous violation with a fresh
	// (value-bearing) Changes map — the exact per-poll restamp that used to mint a
	// new key every cycle.
	darkPoll := func(stamp string) {
		src.mu.Lock()
		src.pending = []beadStateEvent{{Topic: "beads.bead.updated", Bead: &BeadEvent{
			ID: "gd-dark", Type: "trigger", Status: "open",
			Fields:  map[string]string{"enabled": "false"},
			Changes: map[string]any{"last_checked": stamp, "last_outcome_at": stamp},
		}}}
		src.mu.Unlock()
		engine.evalOne(context.Background(), src, trig)
	}

	// 5 polls of a single continuous DARK episode, each restamping last_checked.
	for _, stamp := range []string{
		"2026-08-27T21:00:00Z", "2026-08-27T21:05:00Z", "2026-08-27T21:10:00Z",
		"2026-08-27T21:15:00Z", "2026-08-27T21:20:00Z",
	} {
		darkPoll(stamp)
	}
	if len(alerts) != 1 {
		t.Fatalf("continuous violation across 5 polls must fire exactly ONE alert, got %d: %v", len(alerts), alerts)
	}

	// Recovery: the gardener is re-enabled → invariant holds → no alert, and the
	// engine re-arms the episode key.
	src.mu.Lock()
	src.pending = []beadStateEvent{{Topic: "beads.bead.updated", Bead: &BeadEvent{
		ID: "gd-dark", Type: "trigger", Status: "open",
		Fields:  map[string]string{"enabled": "true"},
		Changes: map[string]any{"last_checked": "2026-08-27T21:25:00Z", "last_outcome_at": "2026-08-27T21:25:00Z"},
	}}}
	src.mu.Unlock()
	engine.evalOne(context.Background(), src, trig)
	if len(alerts) != 1 {
		t.Fatalf("recovery must stay silent, alerts grew to %d: %v", len(alerts), alerts)
	}

	// A NEW dark episode after recovery must alert again (re-armed).
	darkPoll("2026-08-27T21:30:00Z")
	if len(alerts) != 2 {
		t.Fatalf("a new violation episode after recovery must re-alert (expected 2), got %d: %v", len(alerts), alerts)
	}

	// And that new episode must itself collapse across polls (still one new alert).
	darkPoll("2026-08-27T21:35:00Z")
	if len(alerts) != 2 {
		t.Errorf("the re-armed episode must dedup across its own polls too, alerts grew to %d: %v", len(alerts), alerts)
	}
}

// TestTriggerEngine_AssertOnViolationBoth verifies on_violation=both fires both
// a notify alert AND creates a bead. (ka-0000008157)
func TestTriggerEngine_AssertOnViolationBoth(t *testing.T) {
	daemon := &mockTriggerDaemon{}
	var alerts []string
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon:   daemon,
		Logger:   slog.Default(),
		Notifier: func(_ context.Context, _, msg string, _ string) { alerts = append(alerts, msg) },
	})
	src := NewBeadStateTriggerSource(slog.Default())
	engine.RegisterSource(src)

	trig := TriggerDef{
		ID: "assert-both", Title: "assert both", Source: "bead",
		Action: TriggerActionAssert, OnViolation: "both", MaxConcurrent: 10,
		LastChecked: timeNow().Add(-time.Hour),
		Prompt:      "violation: {{violated}}",
		SourceData: beadTriggerData{
			Assert: []beadCondition{{Expr: `bead.status == "closed"`}},
		},
	}
	src.mu.Lock()
	src.pending = []beadStateEvent{
		{Topic: "beads.bead.updated", Bead: &BeadEvent{ID: "gd-open", Type: "task", Status: "open"}},
	}
	src.mu.Unlock()

	engine.evalOne(context.Background(), src, trig)

	if len(alerts) != 1 {
		t.Errorf("on_violation=both should send 1 notify, got %d", len(alerts))
	}
	if len(daemon.createdBeads) != 1 {
		t.Errorf("on_violation=both should create 1 bead, got %d", len(daemon.createdBeads))
	}
}
