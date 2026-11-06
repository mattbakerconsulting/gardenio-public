package main

import (
	"encoding/json"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

func mustWrapup(t *testing.T, wu WrapUp) string {
	t.Helper()
	b, err := json.Marshal(wu)
	if err != nil {
		t.Fatalf("marshal wrapup: %v", err)
	}
	return string(b)
}

func TestSummarizeCycle_WithWrapupNotFailed(t *testing.T) {
	cycleStart := time.Now().Add(-30 * time.Minute)
	ab := &beadsapi.BeadDetail{
		ID:        "gd-cycle1",
		Status:    "closed",
		Assignee:  "sched-intake-01",
		CreatedAt: cycleStart,
		UpdatedAt: time.Now(),
		ClosedAt:  time.Now().Format(time.RFC3339),
		Fields: map[string]string{
			"agent":       "sched-intake-01",
			"role":        "gardener-intake",
			"schedule_id": "gd-sched-1",
			"wrapup": mustWrapup(t, WrapUp{
				Accomplishments: "Readied 3 beads, reaped 1 stale",
				PullRequests:    []string{"https://gitlab.com/x/y/-/merge_requests/1"},
			}),
		},
	}

	cycle := summarizeCycle(ab, nil)
	if cycle.Failed {
		t.Fatalf("cycle should not be failed: %+v", cycle)
	}
	if !cycle.WrapupPresent {
		t.Fatal("wrapup should be present")
	}
	if cycle.AccomplishmentsChars == 0 {
		t.Fatal("accomplishments_chars should be > 0")
	}
	if cycle.PullRequests != 1 {
		t.Errorf("expected 1 PR, got %d", cycle.PullRequests)
	}
	if cycle.Role != "gardener-intake" || cycle.ScheduleID != "gd-sched-1" {
		t.Errorf("role/schedule mismatch: %+v", cycle)
	}
	if cycle.Duration == "" {
		t.Error("duration should be populated when created_at and closed_at are set")
	}
}

func TestSummarizeCycle_CarriesWrapupTextAndCloseFields(t *testing.T) {
	// The audit JSON must expose the wrap-up TEXT (not just its length) plus
	// agent_state/close_reason, so a downstream no-op classifier does not need
	// a second bulk bead fetch to recover them (ka-0000008261 item 3).
	ab := &beadsapi.BeadDetail{
		ID:        "gd-cycle-text",
		Status:    "closed",
		Assignee:  "sched-release-99",
		CreatedAt: time.Now().Add(-20 * time.Minute),
		ClosedAt:  time.Now().Format(time.RFC3339),
		Fields: map[string]string{
			"agent":        "sched-release-99",
			"role":         "gardener-release-owner",
			"agent_state":  "done",
			"close_reason": "cycle complete",
			"wrapup": mustWrapup(t, WrapUp{
				TLDR:            "KPI snapshot only — main +0 since golden, no cut available",
				Accomplishments: "SKIP: golden tag 2026.226.12 is 24min old (gate=25min)",
				Blockers:        "waiting on golden age",
				HandoffNotes:    "next cut eligible in ~1min",
			}),
		},
	}

	cycle := summarizeCycle(ab, nil)
	if cycle.TLDR != "KPI snapshot only — main +0 since golden, no cut available" {
		t.Errorf("tldr not carried through: %q", cycle.TLDR)
	}
	if cycle.Accomplishments != "SKIP: golden tag 2026.226.12 is 24min old (gate=25min)" {
		t.Errorf("accomplishments text not carried through: %q", cycle.Accomplishments)
	}
	if cycle.Blockers != "waiting on golden age" {
		t.Errorf("blockers not carried through: %q", cycle.Blockers)
	}
	if cycle.HandoffNotes != "next cut eligible in ~1min" {
		t.Errorf("handoff_notes not carried through: %q", cycle.HandoffNotes)
	}
	if cycle.AgentState != "done" {
		t.Errorf("agent_state = %q, want done", cycle.AgentState)
	}
	if cycle.CloseReason != "cycle complete" {
		t.Errorf("close_reason = %q, want 'cycle complete'", cycle.CloseReason)
	}
	// A cycle with a genuine wrap-up must still not be flagged failed.
	if cycle.Failed {
		t.Errorf("cycle with wrap-up text should not be FAILED: %+v", cycle)
	}
}

func TestSummarizeCycle_NoWrapupOmitsTextFields(t *testing.T) {
	// With no wrap-up, the text fields stay empty so `omitempty` drops them
	// from JSON — they never carry stale content from a prior cycle.
	ab := &beadsapi.BeadDetail{
		ID:        "gd-cycle-notext",
		Status:    "closed",
		CreatedAt: time.Now().Add(-5 * time.Minute),
		Fields: map[string]string{
			"agent":       "sched-x",
			"role":        "gardener-delivery",
			"agent_state": "working",
		},
	}
	cycle := summarizeCycle(ab, nil)
	if cycle.TLDR != "" || cycle.Accomplishments != "" || cycle.Blockers != "" || cycle.HandoffNotes != "" {
		t.Errorf("text fields should be empty with no wrap-up: %+v", cycle)
	}
	if cycle.AgentState != "working" {
		t.Errorf("agent_state should still be exposed: %q", cycle.AgentState)
	}
}

func TestSummarizeCycle_NoWrapupIsFailed(t *testing.T) {
	ab := &beadsapi.BeadDetail{
		ID:        "gd-cycle2",
		Status:    "closed",
		Assignee:  "sched-delivery-02",
		CreatedAt: time.Now().Add(-10 * time.Minute),
		Fields: map[string]string{
			"agent": "sched-delivery-02",
			"role":  "gardener-delivery",
			// wrapup intentionally absent
		},
	}

	cycle := summarizeCycle(ab, nil)
	if !cycle.Failed {
		t.Fatal("cycle with no wrapup must be flagged FAILED")
	}
	if cycle.FailedReason != "no wrapup" {
		t.Errorf("unexpected failed reason: %q", cycle.FailedReason)
	}
	if cycle.WrapupPresent {
		t.Error("wrapup_present should be false")
	}
}

func TestSummarizeCycle_EmptyAccomplishmentsIsFailed(t *testing.T) {
	ab := &beadsapi.BeadDetail{
		ID:        "gd-cycle3",
		Status:    "closed",
		Assignee:  "sched-release-03",
		CreatedAt: time.Now().Add(-5 * time.Minute),
		Fields: map[string]string{
			"agent": "sched-release-03",
			"role":  "gardener-release",
			"wrapup": mustWrapup(t, WrapUp{
				Accomplishments: "   ", // whitespace only
				Blockers:        "stuck on decision",
			}),
		},
	}

	cycle := summarizeCycle(ab, nil)
	if !cycle.Failed {
		t.Fatal("empty accomplishments should fail")
	}
	if cycle.FailedReason != "empty accomplishments" {
		t.Errorf("unexpected failed reason: %q", cycle.FailedReason)
	}
	if !cycle.WrapupPresent {
		t.Error("wrapup_present should still be true — the field was set, just empty")
	}
}

func TestSummarizeCycle_DecisionsScopedToCycleWindowAndAgent(t *testing.T) {
	cycleStart := time.Now().Add(-1 * time.Hour)
	cycleEnd := time.Now().Add(-10 * time.Minute)
	ab := &beadsapi.BeadDetail{
		ID:        "gd-cycle4",
		Status:    "closed",
		Assignee:  "sched-intake-04",
		CreatedAt: cycleStart,
		ClosedAt:  cycleEnd.Format(time.RFC3339),
		Fields: map[string]string{
			"agent":  "sched-intake-04",
			"role":   "gardener-intake",
			"wrapup": mustWrapup(t, WrapUp{Accomplishments: "ok"}),
		},
	}

	decisions := []*beadsapi.BeadDetail{
		// In-window, this agent, filed — resolved (chosen set).
		{
			ID:        "gd-dec-in-resolved",
			CreatedBy: "sched-intake-04",
			CreatedAt: cycleStart.Add(5 * time.Minute),
			Fields:    map[string]string{"chosen": "a"},
		},
		// In-window, this agent, filed — unresolved.
		{
			ID:        "gd-dec-in-open",
			CreatedBy: "sched-intake-04",
			CreatedAt: cycleStart.Add(30 * time.Minute),
			Fields:    map[string]string{},
		},
		// Out of window (after close).
		{
			ID:        "gd-dec-after",
			CreatedBy: "sched-intake-04",
			CreatedAt: cycleEnd.Add(5 * time.Minute),
			Fields:    map[string]string{"chosen": "b"},
		},
		// In window but different agent.
		{
			ID:        "gd-dec-other",
			CreatedBy: "sched-other",
			CreatedAt: cycleStart.Add(10 * time.Minute),
			Fields:    map[string]string{"chosen": "a"},
		},
	}

	cycle := summarizeCycle(ab, decisions)
	if cycle.DecisionsFiled != 2 {
		t.Errorf("expected 2 decisions filed, got %d", cycle.DecisionsFiled)
	}
	if cycle.DecisionsResolved != 1 {
		t.Errorf("expected 1 resolved decision, got %d", cycle.DecisionsResolved)
	}
}

func TestSummarizeCycle_UnparseableWrapupIsFailed(t *testing.T) {
	ab := &beadsapi.BeadDetail{
		ID:        "gd-cycle5",
		Status:    "closed",
		CreatedAt: time.Now().Add(-3 * time.Minute),
		Fields: map[string]string{
			"agent":  "x",
			"role":   "gardener-delivery",
			"wrapup": "{not json",
		},
	}
	cycle := summarizeCycle(ab, nil)
	if !cycle.Failed {
		t.Fatal("unparseable wrapup must fail — treat as 'no wrapup'")
	}
	if cycle.WrapupPresent {
		t.Fatal("unparseable wrapup should not be present")
	}
}
