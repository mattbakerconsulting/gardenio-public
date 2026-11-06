package bridge

// T1 — invariant test coverage for the scriptable-triggers + gardener-liveness
// correction (task gd-BWRdfbMLJu; research epic ka-0000007837; cross-referenced
// from the trigger-coverage epic ka-0000004568).
//
// The five invariants B1/B2/B3/F4/F5 were each shipped with their implementation
// MRs (ka-0000002275 / ka-0000001541 / ka-0000004185 / ka-0000008047 /
// ka-0000008157) and already carry broad unit coverage. This file adds the
// dedicated, invariant-named contracts T1 asks for — each test states the ONE
// property the invariant must hold — and closes three concrete gaps the shipped
// suites left open:
//
//   B1  The CANONICAL liveness signal (last_checked), not just the last_run
//       convenience mirror, is freshly stamped after a gardener SPAWNS a run —
//       the property the whole liveness architecture rests on. The existing
//       B1 tests (gardener_evaluator_test.go) assert only the runs_spawned /
//       last_run top-level mirrors; none asserts last_checked-after-spawn.
//
//   B3  The enabled→false transition is expressible as a declarative
//       `enabled changed_to false` bead-source trigger that fires exactly once
//       for the disable and is silent on an unrelated update — the operator-
//       facing shape B3 promises ("once the event exists a declarative trigger
//       can watch enabled changed_to false directly") — plus the roster-count
//       drop that a whole-source (cascading) disable produces, expressible as a
//       single CEL set-assertion over the watched roster (the B4/F4+F5 upgrade
//       path B3's scope names for the cascading case).
//
//   F4  The cost budget actually TRIPS and fails CLOSED on an over-budget
//       program. trigger_expr_test.go's TestEvalExpr_CostLimit is a soft no-op
//       (it skips or ignores the result) and its own comment defers the hard
//       case to a "TestEvalExpr_CostLimitTrips below" that was never written.
//       This is that test, built from a program with a deliberately tiny
//       CostLimit so the trip is deterministic.
//
// B2 and F5 already have exhaustive, correctly-shaped coverage
// (gardener_liveness*_test.go, trigger_assert_source_test.go); the tests here for
// those two are thin invariant restatements that pin the single acceptance
// property from the task's Scope, cross-referenced to the deep suites rather
// than duplicating them.

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/cel-go/cel"
)

// ---------------------------------------------------------------------------
// B1 — a gardener that spawns a run has a fresh CANONICAL liveness timestamp
//      afterward (ka-0000002275).
// ---------------------------------------------------------------------------

// TestT1_B1_GardenerSpawnStampsFreshLastChecked is the B1 invariant in its
// canonical-signal form: after a gardener fires (spawns an agent) in an Evaluate
// cycle, the trigger bead's `last_checked` — the ONE field the whole liveness
// architecture treats as the dead-man's switch (gardener_liveness.go header,
// "last_checked is the canonical liveness signal") — is stamped to ~now.
//
// This is distinct from TestGardener_PostFire_MirrorsCountersAndStampsLastRun,
// which asserts only the runs_spawned/last_run convenience mirrors. B1's whole
// point is that last_run is UNTRUSTWORTHY on the unified type=trigger path and
// last_checked replaced it; so the invariant that must be tested is that a fire
// leaves last_checked fresh, not merely that last_run is non-null.
func TestT1_B1_GardenerSpawnStampsFreshLastChecked(t *testing.T) {
	daemon, engine, _, _ := gardenerTrigger(t, GardenerSpec{RunBudget: 5})

	engine.Evaluate(context.Background())

	// The gardener must have actually spawned — otherwise the stamp we assert
	// below is the no-match path, not the post-spawn path.
	daemon.mu.Lock()
	spawned := len(daemon.spawnedNames)
	daemon.mu.Unlock()
	if spawned != 1 {
		t.Fatalf("precondition: gardener should spawn exactly one agent, spawned %d", spawned)
	}

	fu := findFieldUpdate(daemon, "gd-cap-1")
	if fu == nil || fu["last_checked"] == "" {
		t.Fatalf("a gardener that spawned a run must have last_checked stamped, got %v", fu)
	}
	ts, err := time.Parse(time.RFC3339, fu["last_checked"])
	if err != nil {
		t.Fatalf("last_checked must be RFC3339, got %q: %v", fu["last_checked"], err)
	}
	if age := time.Since(ts); age < 0 || age > time.Minute {
		t.Errorf("last_checked must be freshly stamped (~now) after a spawn, got %s ago", age)
	}
	// And the run counter advanced — a fresh timestamp with no run would be a
	// heartbeat, not a spawn. Both facts together are the B1 invariant.
	cap := gardenerSpecFromUpdate(t, daemon, "gd-cap-1")
	if cap.RunsSpawned != 1 {
		t.Errorf("a spawned run must advance RunsSpawned to 1, got %d", cap.RunsSpawned)
	}
}

// TestT1_B1_NoReaderTrustsLastRunForLiveness is the negative half of B1: the
// engine-side liveness sweep must key off last_checked, NOT last_run. A gardener
// with a FRESH last_checked but a stale/empty last_run is alive and must not be
// flagged dark — proving no liveness reader fell back to last_run (the exact
// untrustworthy field B1 deprecated).
func TestT1_B1_NoReaderTrustsLastRunForLiveness(t *testing.T) {
	// Fresh last_checked (1m), but last_run is ancient AND runs_spawned is 0 —
	// the "looks never-run" shape that made last_run useless for liveness.
	bead := gardenerBead("gd-b1-freshcheck", "fresh gardener, stale last_run", rfc3339Ago(1*time.Minute),
		map[string]string{
			"last_run":     rfc3339Ago(30 * 24 * time.Hour),
			"runs_spawned": "0",
		})
	engine, _, rec := livenessEngine(t, true, false, bead)

	engine.SweepGardenerLiveness(context.Background())

	if rec.count() != 0 {
		t.Fatalf("a gardener fresh by last_checked must be alive regardless of last_run; "+
			"a reader trusting last_run would false-alarm, got %d alerts", rec.count())
	}
}

// ---------------------------------------------------------------------------
// B2 — a stale/dark watched gardener trips the engine heartbeat liveness event
//      (no agent spawn) (ka-0000001541).
// ---------------------------------------------------------------------------

// TestT1_B2_DarkGardenerHeartbeatFiresWithoutSpawn pins the B2 acceptance
// property: an OPEN+enabled gardener whose last_checked is stale fires the
// liveness heartbeat alert with ZERO agent spawns. The exhaustive per-gate B2
// coverage (dedup, cron cadence, dormant/blocked/one-spawn skips, closed/
// disabled scope boundary) lives in gardener_liveness_test.go; this is the
// single invariant restatement T1's Scope item 2 asks for.
func TestT1_B2_DarkGardenerHeartbeatFiresWithoutSpawn(t *testing.T) {
	// Default cadence 2h, stale factor 3 ⇒ 6h threshold; 10h is dark.
	bead := gardenerBead("gd-b2-dark", "dark janitor", rfc3339Ago(10*time.Hour), nil)
	engine, daemon, rec := livenessEngine(t, true, false, bead)

	engine.SweepGardenerLiveness(context.Background())

	if rec.count() != 1 {
		t.Fatalf("dark gardener must fire exactly one heartbeat alert, got %d", rec.count())
	}
	if len(daemon.spawnedNames) != 0 {
		t.Fatalf("B2 heartbeat must NOT spawn an agent (the whole point), spawned %d", len(daemon.spawnedNames))
	}
}

// ---------------------------------------------------------------------------
// B3 — the enabled→false transition emits a matchable lifecycle event, and a
//      roster-count drop (cascading disable) is detectable (ka-0000004185).
// ---------------------------------------------------------------------------

// TestT1_B3_EnabledChangedToFalseFiresOnce is the per-bead half of B3: once the
// daemon emits `enabled` in a BeadUpdated's Changes (the kbeads fix in MR
// !4506), a declarative bead-source trigger watching `enabled changed_to false`
// fires exactly once for the disable transition and stays silent on an
// unrelated update that does not carry the enabled change. This is the
// operator-facing detection B3 promises ("a declarative trigger can watch
// enabled changed_to false directly").
func TestT1_B3_EnabledChangedToFalseFiresOnce(t *testing.T) {
	src := NewBeadStateTriggerSource(slog.Default())
	src.mu.Lock()
	src.pending = []beadStateEvent{
		// The disable transition: enabled went to false this update.
		{Topic: "beads.bead.updated", Bead: &BeadEvent{
			ID: "gd-cap-disabled", Type: "trigger", Title: "janitor gardener", Status: "open",
			Changes: map[string]any{"enabled": false},
		}},
		// An unrelated later update on the same gardener (a status touch) that
		// does NOT carry the enabled change — must not re-fire the disable watch.
		{Topic: "beads.bead.updated", Bead: &BeadEvent{
			ID: "gd-cap-other", Type: "trigger", Title: "other gardener", Status: "open",
			Changes: map[string]any{"last_checked": "2026-06-17T00:00:00Z"},
		}},
	}
	src.mu.Unlock()

	trig := TriggerDef{
		ID: "watch-gardener-disable", Source: "bead", Action: TriggerActionNotify, MaxConcurrent: 10,
		SourceData: beadTriggerData{
			EventTypes: []string{"updated"},
			Conditions: []beadCondition{{Field: "enabled", Op: "changed_to", Value: "false"}},
		},
	}

	matches, err := src.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("enabled changed_to false must fire exactly once (only the disable transition), got %d: %+v",
			len(matches), matches)
	}
	if matches[0].Fields["source_bead_id"] != "gd-cap-disabled" {
		t.Errorf("the match must be the disabled gardener, got %q", matches[0].Fields["source_bead_id"])
	}
}

// TestT1_B3_RosterCountDropDetectableViaAssert is the cascading-disable half of
// B3: a whole-source disable produces no single per-bead transition, so B3's
// scope specifies a roster-count invariant, upgradeable to a CEL set-assertion
// under F4/F5. Here the invariant "the enabled janitor roster is at full
// strength (== expected size)" is expressed as a single CEL assertion over the
// resolved `watched` set. When one gardener is silently disabled the count drops
// and the assertion is VIOLATED → fires; at full strength it is silent.
//
// This exercises the exact B4-upgrade path B3's scope names, using the shipped
// F4 evaluator directly (the count assertion the JSON-DSL bead_query stopgap
// could only express as a daemon count query).
func TestT1_B3_RosterCountDropDetectableViaAssert(t *testing.T) {
	// The invariant: exactly 2 janitor gardeners are enabled (full roster).
	const rosterAssert = `watched.filter(w, w.enabled == "true").size() == 2`
	compiled, err := CompileExpr(rosterAssert)
	if err != nil {
		t.Fatalf("roster-count assertion must compile: %v", err)
	}

	rootBead := &BeadEvent{ID: "gd-roster-root", Type: "epic", Status: "open"}

	// Full strength: both janitors enabled → invariant holds → silent.
	full := []*BeadEvent{
		{ID: "gd-jan-a", Fields: map[string]string{"enabled": "true"}},
		{ID: "gd-jan-b", Fields: map[string]string{"enabled": "true"}},
	}
	if res := compiled.Eval(context.Background(), rootBead, "updated", timeNow(), full); res.Err != nil {
		t.Fatalf("full-roster eval errored: %v", res.Err)
	} else if !res.Matched {
		t.Errorf("full roster (2 enabled) must satisfy the invariant, got matched=false")
	}

	// Cascading disable: one janitor silently dropped to enabled=false → count
	// falls to 1 → invariant no longer holds → an assert trigger would fire.
	dropped := []*BeadEvent{
		{ID: "gd-jan-a", Fields: map[string]string{"enabled": "true"}},
		{ID: "gd-jan-b", Fields: map[string]string{"enabled": "false"}},
	}
	if res := compiled.Eval(context.Background(), rootBead, "updated", timeNow(), dropped); res.Err != nil {
		t.Fatalf("dropped-roster eval errored: %v", res.Err)
	} else if res.Matched {
		t.Errorf("a roster-count drop (1 of 2 enabled) must break the invariant so the assert fires, got matched=true")
	}
}

// ---------------------------------------------------------------------------
// F4 — evalExpr happy path + fail-closed + cost budget + compile-on-write
//      (ka-0000008047).
// ---------------------------------------------------------------------------

// TestT1_F4_EvalExprHappyPath pins the F4 happy-path invariant: a well-formed
// boolean CEL predicate over the typed context evaluates to its expected value
// with no error. (Broad field/event/time/watched coverage: trigger_expr_test.go.)
func TestT1_F4_EvalExprHappyPath(t *testing.T) {
	bead := &BeadEvent{ID: "gd-1", Type: "bug", Status: "open", Priority: 0}
	compiled, err := CompileExpr(`bead.type == "bug" && bead.priority == 0 && bead.status == "open"`)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	res := compiled.Eval(context.Background(), bead, "created", time.Now(), nil)
	if res.Err != nil {
		t.Fatalf("happy-path eval must not error: %v", res.Err)
	}
	if !res.Matched {
		t.Errorf("expected the predicate to hold for a P0 open bug, got matched=false")
	}
}

// TestT1_F4_FailsClosedOnEvalError pins the fail-closed invariant on the
// EVALUATION path: an expression that type-checks but errors at runtime
// (unguarded missing-key access) returns Err != nil and Matched=false — a
// broken assertion must never silently pass.
func TestT1_F4_FailsClosedOnEvalError(t *testing.T) {
	compiled, err := CompileExpr(`bead.t1_missing_field == "x"`)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	res := compiled.Eval(context.Background(), &BeadEvent{ID: "gd-1", Status: "open"}, "updated", time.Now(), nil)
	if res.Err == nil {
		t.Fatalf("unguarded missing-key access must error (fail-closed), got matched=%v", res.Matched)
	}
	if res.Matched {
		t.Errorf("fail-closed violated: Matched=true on the error path")
	}
}

// TestT1_F4_CompileOnWriteRejectsMalformed pins the compile-on-write invariant:
// a malformed or non-bool expression is rejected at authoring time (CompileExpr
// returns an error), never deferred to the hot path.
func TestT1_F4_CompileOnWriteRejectsMalformed(t *testing.T) {
	for _, src := range []string{
		`bead.status ==`,         // syntax error
		`(bead.status == "open"`, // unbalanced paren
		`no_such_var == 1`,       // unknown variable
		`bead.priority`,          // non-bool result
		``,                       // empty
	} {
		if _, err := CompileExpr(src); err == nil {
			t.Errorf("CompileExpr(%q) must be rejected at authoring time, got nil error", src)
		}
	}
}

// TestT1_F4_CostBudgetTripsFailClosed is the cost-enforcement invariant the
// shipped suite left open. trigger_expr_test.go's TestEvalExpr_CostLimit is a
// soft no-op whose own comment defers the hard case to a
// "TestEvalExpr_CostLimitTrips below with a custom program" that never existed.
// This is that test: a program built with a deliberately tiny CostLimit so an
// otherwise-cheap comprehension deterministically blows the budget, proving the
// budget is ENFORCED and the overrun fails CLOSED (Err != nil, Matched=false)
// rather than running unbounded or silently passing.
func TestT1_F4_CostBudgetTripsFailClosed(t *testing.T) {
	env, err := buildExprEnv()
	if err != nil {
		t.Fatalf("build env: %v", err)
	}
	// A comprehension whose runtime cost exceeds the tiny limit below.
	ast, iss := env.Compile(`[1,2,3,4,5,6,7,8,9,10].all(i, i > 0)`)
	if iss != nil && iss.Err() != nil {
		t.Fatalf("compile: %v", iss.Err())
	}
	// CostLimit(1) is far below any comprehension's cost, so evaluation must
	// abort on the cost budget. This mirrors trigger_expr.go's Program build
	// but with a limit chosen to trip deterministically.
	program, err := env.Program(ast, cel.CostLimit(1), cel.InterruptCheckFrequency(exprInterruptCheckFreq))
	if err != nil {
		t.Fatalf("program: %v", err)
	}
	compiled := &CompiledExpr{src: "cost-trip", program: program}

	res := compiled.Eval(context.Background(), &BeadEvent{ID: "gd-1"}, "updated", time.Now(), nil)
	if res.Err == nil {
		t.Fatalf("an over-budget program must trip the cost limit and error (fail-closed), got matched=%v", res.Matched)
	}
	if res.Matched {
		t.Errorf("fail-closed violated: Matched=true on the cost-overrun path")
	}
}

// ---------------------------------------------------------------------------
// F5 — assert action: predicate flips false → alert fires once (dedup);
//      flips true → silent (ka-0000008157).
// ---------------------------------------------------------------------------

// TestT1_F5_PredicateFlipFalseFiresOnceThenTrueSilent is the F5 acceptance
// walk from the task VERIFY line, driven through the full engine evalOne path:
//
//  1. predicate VIOLATED (a P0 bug with no assignee) → exactly one notify alert,
//     no agent spawn (assert bypasses SpawnGuard);
//  2. re-evaluating the SAME violation → deduped (still one alert);
//  3. predicate now SATISFIED (the bug gains an assignee) → silent.
//
// The per-source and on_violation=both mechanics are covered in
// trigger_assert_source_test.go; this restates the exact flip-sequence the T1
// Scope names as one contiguous invariant.
func TestT1_F5_PredicateFlipFalseFiresOnceThenTrueSilent(t *testing.T) {
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
		ID: "t1-f5-assert", Title: "P0 bugs must be assigned", Source: "bead",
		Action: TriggerActionAssert, OnViolation: "notify", MaxConcurrent: 10,
		LastChecked: timeNow().Add(-time.Hour), // bypass runtime-bootstrap guard
		Prompt:      "VIOLATION on {{bead_id}}: {{violated}} | {{values}}",
		SourceData: beadTriggerData{
			Assert: []beadCondition{{Expr: `bead.assignee != ""`}},
		},
	}

	// Step 1: predicate false → fires once, no spawn.
	src.mu.Lock()
	src.pending = []beadStateEvent{
		{Topic: "beads.bead.updated", Bead: &BeadEvent{ID: "gd-p0", Type: "bug", Status: "open", Priority: 0}},
	}
	src.mu.Unlock()
	engine.evalOne(context.Background(), src, trig)
	if len(alerts) != 1 {
		t.Fatalf("violated predicate must fire exactly one alert, got %d: %v", len(alerts), alerts)
	}
	if !strings.Contains(alerts[0], "gd-p0") || !strings.Contains(alerts[0], `bead.assignee != ""`) {
		t.Errorf("alert must name the bead and the violated clause, got %q", alerts[0])
	}
	if len(daemon.spawnedNames) != 0 {
		t.Errorf("assert action must NOT spawn an agent, spawned %v", daemon.spawnedNames)
	}

	// Step 2: same violation again → deduped.
	src.mu.Lock()
	src.pending = []beadStateEvent{
		{Topic: "beads.bead.updated", Bead: &BeadEvent{ID: "gd-p0", Type: "bug", Status: "open", Priority: 0}},
	}
	src.mu.Unlock()
	engine.evalOne(context.Background(), src, trig)
	if len(alerts) != 1 {
		t.Errorf("re-evaluating the same violation must dedup (still 1 alert), got %d", len(alerts))
	}

	// Step 3: predicate now satisfied → silent.
	src.mu.Lock()
	src.pending = []beadStateEvent{
		{Topic: "beads.bead.updated", Bead: &BeadEvent{ID: "gd-ok", Type: "bug", Status: "open", Priority: 0, Assignee: "alice"}},
	}
	src.mu.Unlock()
	engine.evalOne(context.Background(), src, trig)
	if len(alerts) != 1 {
		t.Errorf("a satisfied predicate must stay silent, alerts grew to %d: %v", len(alerts), alerts)
	}
}

// TestT1_F5_AssertFailsClosedToViolation pins the F5 fail-closed inversion: on
// the assert path (unlike the suppress-gate condition path) a clause that errors
// at runtime counts as a VIOLATION and fires — a broken invariant must not
// silently pass. Uses the source Evaluate directly for a focused assertion.
func TestT1_F5_AssertFailsClosedToViolation(t *testing.T) {
	src := NewBeadStateTriggerSource(slog.Default())
	src.mu.Lock()
	src.pending = []beadStateEvent{
		{Topic: "beads.bead.updated", Bead: &BeadEvent{ID: "gd-1", Type: "task", Status: "open"}},
	}
	src.mu.Unlock()

	trig := TriggerDef{
		ID: "t1-f5-broken", Source: "bead", Action: TriggerActionAssert, MaxConcurrent: 10,
		SourceData: beadTriggerData{
			// Unguarded missing-key access errors at runtime → violation (fires).
			Assert: []beadCondition{{Expr: `bead.t1_no_such_field == "x"`}},
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
