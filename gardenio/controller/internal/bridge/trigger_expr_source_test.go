package bridge

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// TestBeadStateTriggerSource_ExprCondition drives an expr condition end-to-end
// through Evaluate: only the events whose bead satisfies the CEL predicate
// should produce matches.
func TestBeadStateTriggerSource_ExprCondition(t *testing.T) {
	src := NewBeadStateTriggerSource(slog.Default())

	src.mu.Lock()
	src.pending = []beadStateEvent{
		{Topic: "beads.bead.created", Bead: &BeadEvent{ID: "gd-bug0", Type: "bug", Status: "open", Priority: 0}},
		{Topic: "beads.bead.created", Bead: &BeadEvent{ID: "gd-bug2", Type: "bug", Status: "open", Priority: 2}},
		{Topic: "beads.bead.created", Bead: &BeadEvent{ID: "gd-task", Type: "task", Status: "open", Priority: 0}},
	}
	src.mu.Unlock()

	trig := TriggerDef{
		ID:            "p0-bug-assert",
		Source:        "bead",
		Action:        TriggerActionNotify,
		MaxConcurrent: 10,
		SourceData: beadTriggerData{
			EventTypes: []string{"created"},
			Conditions: []beadCondition{
				{Expr: `bead.type == "bug" && bead.priority == 0`},
			},
		},
	}

	matches, err := src.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	matched := map[string]bool{}
	for _, m := range matches {
		matched[m.Fields["source_bead_id"]] = true
	}
	if !matched["gd-bug0"] {
		t.Errorf("gd-bug0 (bug, P0) should match the expr; matched=%v", matched)
	}
	if matched["gd-bug2"] {
		t.Errorf("gd-bug2 (bug, P2) must NOT match; matched=%v", matched)
	}
	if matched["gd-task"] {
		t.Errorf("gd-task (task, P0) must NOT match; matched=%v", matched)
	}
	if len(matches) != 1 {
		t.Errorf("expected exactly 1 match, got %d (%v)", len(matches), matched)
	}
}

// TestBeadStateTriggerSource_ExprAndsWithFieldCondition verifies an expr
// condition ANDs with a legacy field/op/value condition.
func TestBeadStateTriggerSource_ExprAndsWithFieldCondition(t *testing.T) {
	src := NewBeadStateTriggerSource(slog.Default())
	src.mu.Lock()
	src.pending = []beadStateEvent{
		{Topic: "beads.bead.updated", Bead: &BeadEvent{ID: "gd-1", Type: "task", Status: "in_progress", Priority: 0}},
		{Topic: "beads.bead.updated", Bead: &BeadEvent{ID: "gd-2", Type: "task", Status: "open", Priority: 0}},
	}
	src.mu.Unlock()

	trig := TriggerDef{
		ID: "expr-plus-field", Source: "bead", Action: TriggerActionNotify, MaxConcurrent: 10,
		SourceData: beadTriggerData{
			Conditions: []beadCondition{
				{Field: "status", Op: "==", Value: "in_progress"},
				{Expr: `bead.priority == 0`},
			},
		},
	}
	matches, err := src.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(matches) != 1 || matches[0].Fields["source_bead_id"] != "gd-1" {
		t.Errorf("only gd-1 (in_progress AND P0) should match, got %+v", matches)
	}
}

// TestBeadStateTriggerSource_ExprFailClosed verifies that a runtime-erroring
// expression does NOT fire the trigger (fail-closed).
func TestBeadStateTriggerSource_ExprFailClosed(t *testing.T) {
	src := NewBeadStateTriggerSource(slog.Default())
	src.mu.Lock()
	src.pending = []beadStateEvent{
		{Topic: "beads.bead.updated", Bead: &BeadEvent{ID: "gd-1", Type: "task", Status: "open"}},
	}
	src.mu.Unlock()

	trig := TriggerDef{
		ID: "broken-assert", Source: "bead", Action: TriggerActionNotify, MaxConcurrent: 10,
		SourceData: beadTriggerData{
			// Unguarded missing-key access errors at runtime → fail closed.
			Conditions: []beadCondition{{Expr: `bead.nonexistent == "x"`}},
		},
	}
	matches, err := src.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(matches) != 0 {
		t.Errorf("a runtime-erroring expr must NOT fire (fail-closed); got %d matches", len(matches))
	}
}

// TestBeadStateTriggerSource_ExprWatched drives an expr that quantifies over
// the watched list, with a real WatchedResolver backed by fakes.
func TestBeadStateTriggerSource_ExprWatched(t *testing.T) {
	sg := NewSubgraphResolver(&fakeSubgraphClient{
		fwd: map[string][]beadsapi.Dependency{
			"gd-epic": {dep("gd-epic", "gd-a", "parent-child"), dep("gd-epic", "gd-b", "parent-child")},
		},
	}, SubgraphResolverConfig{})
	hyd := &fakeHydrator{beads: map[string]*beadsapi.BeadDetail{
		"gd-epic": detail("gd-epic", "open"),
		"gd-a":    detail("gd-a", "closed"),
		"gd-b":    detail("gd-b", "in_progress"), // one child still open
	}}

	src := NewBeadStateTriggerSource(slog.Default())
	src.SetWatchedResolver(NewWatchedResolver(sg, hyd, WatchedResolverConfig{}))
	src.mu.Lock()
	src.pending = []beadStateEvent{
		{Topic: "beads.bead.updated", Bead: &BeadEvent{ID: "gd-epic", Type: "epic", Status: "open"}},
	}
	src.mu.Unlock()

	trig := TriggerDef{
		ID: "epic-has-open-child", Source: "bead", Action: TriggerActionNotify, MaxConcurrent: 10,
		SourceData: beadTriggerData{
			Conditions: []beadCondition{
				{Expr: `bead.type == "epic" && watched.exists(w, w.status != "closed")`},
			},
		},
	}
	matches, err := src.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("epic with an open child should match watched.exists, got %d matches", len(matches))
	}

	// Flip the open child to closed: now all children are closed → no match.
	hyd.beads["gd-b"] = detail("gd-b", "closed")
	// New resolver to bypass the TTL cache from the previous resolve.
	src.SetWatchedResolver(NewWatchedResolver(sg2(), &fakeHydrator{beads: map[string]*beadsapi.BeadDetail{
		"gd-epic": detail("gd-epic", "open"),
		"gd-a":    detail("gd-a", "closed"),
		"gd-b":    detail("gd-b", "closed"),
	}}, WatchedResolverConfig{}))
	src.mu.Lock()
	src.pending = []beadStateEvent{
		{Topic: "beads.bead.updated", Bead: &BeadEvent{ID: "gd-epic", Type: "epic", Status: "open"}},
	}
	src.mu.Unlock()
	matches, err = src.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate(2): %v", err)
	}
	if len(matches) != 0 {
		t.Errorf("epic with all children closed should NOT match watched.exists, got %d", len(matches))
	}
}

// sg2 builds a fresh subgraph resolver with the all-closed graph for the
// second leg of TestBeadStateTriggerSource_ExprWatched.
func sg2() *SubgraphResolver {
	return NewSubgraphResolver(&fakeSubgraphClient{
		fwd: map[string][]beadsapi.Dependency{
			"gd-epic": {dep("gd-epic", "gd-a", "parent-child"), dep("gd-epic", "gd-b", "parent-child")},
		},
	}, SubgraphResolverConfig{})
}

// TestBeadStateTriggerSource_LoadRejectsBadExpr verifies compile-on-load:
// a trigger whose conditions carry a malformed CEL expression is skipped.
func TestBeadStateTriggerSource_LoadRejectsBadExpr(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: triggerBeads(
			triggerBead("good", "Valid expr", "trigger:bead", map[string]string{
				"action":     "notify",
				"conditions": `[{"expr":"bead.status == \"open\""}]`,
			}),
			triggerBead("bad", "Malformed expr", "trigger:bead", map[string]string{
				"action":     "notify",
				"conditions": `[{"expr":"bead.status =="}]`,
			}),
			triggerBead("nonbool", "Non-bool expr", "trigger:bead", map[string]string{
				"action":     "notify",
				"conditions": `[{"expr":"bead.priority"}]`,
			}),
		),
	}
	src := NewBeadStateTriggerSource(slog.Default())
	triggers, err := src.LoadTriggers(context.Background(), daemon)
	if err != nil {
		t.Fatalf("LoadTriggers: %v", err)
	}
	ids := map[string]bool{}
	for _, tr := range triggers {
		ids[tr.ID] = true
	}
	if !ids["good"] {
		t.Errorf("valid-expr trigger should load; got %v", ids)
	}
	if ids["bad"] {
		t.Errorf("malformed-expr trigger must be skipped (compile-on-load); got %v", ids)
	}
	if ids["nonbool"] {
		t.Errorf("non-bool-expr trigger must be skipped (compile-on-load); got %v", ids)
	}
}

// TestBeadStateTriggerSource_ExprNowConsistent verifies `now` is threaded into
// the expression and reflects the timeNow clock.
func TestBeadStateTriggerSource_ExprNowConsistent(t *testing.T) {
	orig := timeNow
	defer func() { timeNow = orig }()
	fixed := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	timeNow = func() time.Time { return fixed }

	src := NewBeadStateTriggerSource(slog.Default())
	src.mu.Lock()
	src.pending = []beadStateEvent{
		{Topic: "beads.bead.updated", Bead: &BeadEvent{
			ID: "gd-stale", Type: "task", Status: "in_progress",
			Fields: map[string]string{"last_checked": fixed.Add(-3 * time.Hour).Format(time.RFC3339)},
		}},
		{Topic: "beads.bead.updated", Bead: &BeadEvent{
			ID: "gd-fresh", Type: "task", Status: "in_progress",
			Fields: map[string]string{"last_checked": fixed.Add(-10 * time.Minute).Format(time.RFC3339)},
		}},
	}
	src.mu.Unlock()

	trig := TriggerDef{
		ID: "stale-assert", Source: "bead", Action: TriggerActionNotify, MaxConcurrent: 10,
		SourceData: beadTriggerData{
			Conditions: []beadCondition{
				{Expr: `has(bead.last_checked) && now - timestamp(bead.last_checked) > duration("2h")`},
			},
		},
	}
	matches, err := src.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(matches) != 1 || matches[0].Fields["source_bead_id"] != "gd-stale" {
		t.Errorf("only gd-stale (>2h) should match against fixed now; got %+v", matches)
	}
}
