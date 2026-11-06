package main

import (
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

func mkMentionBead(id, title, createdBy string, fields map[string]string, t time.Time) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID:        id,
		Title:     title,
		CreatedBy: createdBy,
		Fields:    fields,
		CreatedAt: t,
		UpdatedAt: t,
	}
}

func TestComputeInboxRecentMentions_BasicAgentAndDecision(t *testing.T) {
	now := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	older := now.Add(-30 * time.Minute)
	newer := now.Add(-5 * time.Minute)

	agent := mkMentionBead("gd-A1", "agent fallback title", "",
		map[string]string{"spawned_by": "alice", "task_title": "Implement widget"}, older)
	decision := mkMentionBead("gd-D1", "decision fallback title", "creator-id",
		map[string]string{"requested_by": "bob", "prompt": "Pick a path"}, newer)

	rows := computeInboxRecentMentions(
		[]*beadsapi.BeadDetail{agent},
		nil,
		[]*beadsapi.BeadDetail{decision},
		"/v",
		now,
	)
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d: %+v", len(rows), rows)
	}
	if rows[0].ID != "gd-D1" || rows[0].Source != "decision" {
		t.Errorf("expected newest (decision) first, got %+v", rows[0])
	}
	if rows[0].Actor != "bob" || rows[0].Context != "Pick a path" {
		t.Errorf("decision actor/context wrong: %+v", rows[0])
	}
	if rows[0].Href != "/v/gd-D1" {
		t.Errorf("href wrong: %q", rows[0].Href)
	}
	if rows[1].ID != "gd-A1" || rows[1].Source != "agent" {
		t.Errorf("expected agent second, got %+v", rows[1])
	}
	if rows[1].Actor != "alice" || rows[1].Context != "Implement widget" {
		t.Errorf("agent actor/context wrong: %+v", rows[1])
	}
	if rows[1].Ago == "" {
		t.Errorf("expected Ago to be populated for agent row")
	}
}

func TestComputeInboxRecentMentions_SkipsAgentWithoutSpawner(t *testing.T) {
	now := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	selfSpawned := mkMentionBead("gd-A1", "self", "",
		map[string]string{"task_title": "no spawner"}, now)
	rows := computeInboxRecentMentions(
		[]*beadsapi.BeadDetail{selfSpawned},
		nil,
		nil,
		"/v",
		now,
	)
	if len(rows) != 0 {
		t.Fatalf("expected 0 rows, got %d: %+v", len(rows), rows)
	}
}

func TestComputeInboxRecentMentions_DecisionActorFallsBackToCreatedBy(t *testing.T) {
	now := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	d := mkMentionBead("gd-D1", "title", "creator-x",
		map[string]string{"prompt": "p"}, now)
	rows := computeInboxRecentMentions(nil, nil, []*beadsapi.BeadDetail{d}, "/v", now)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if rows[0].Actor != "creator-x" {
		t.Errorf("expected actor=creator-x, got %q", rows[0].Actor)
	}
}

func TestComputeInboxRecentMentions_DecisionSkippedIfNoActor(t *testing.T) {
	now := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	d := mkMentionBead("gd-D1", "title", "", map[string]string{"prompt": "p"}, now)
	rows := computeInboxRecentMentions(nil, nil, []*beadsapi.BeadDetail{d}, "/v", now)
	if len(rows) != 0 {
		t.Fatalf("expected 0 rows (no actor), got %d", len(rows))
	}
}

func TestComputeInboxRecentMentions_ContextFallsBackToTitle(t *testing.T) {
	now := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	agent := mkMentionBead("gd-A1", "agent-bead-title", "",
		map[string]string{"spawned_by": "alice"}, now)
	d := mkMentionBead("gd-D1", "decision-bead-title", "",
		map[string]string{"requested_by": "bob"}, now)
	rows := computeInboxRecentMentions(
		[]*beadsapi.BeadDetail{agent}, nil,
		[]*beadsapi.BeadDetail{d}, "/v", now,
	)
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}
	var agentCtx, decisionCtx string
	for _, r := range rows {
		if r.Source == "agent" {
			agentCtx = r.Context
		}
		if r.Source == "decision" {
			decisionCtx = r.Context
		}
	}
	if agentCtx != "agent-bead-title" {
		t.Errorf("agent context fallback wrong: %q", agentCtx)
	}
	if decisionCtx != "decision-bead-title" {
		t.Errorf("decision context fallback wrong: %q", decisionCtx)
	}
}

func TestComputeInboxRecentMentions_ContextTrimmedToBudget(t *testing.T) {
	now := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	long := strings.Repeat("x", 200)
	agent := mkMentionBead("gd-A1", "fallback", "",
		map[string]string{"spawned_by": "a", "task_title": long}, now)
	rows := computeInboxRecentMentions([]*beadsapi.BeadDetail{agent}, nil, nil, "/v", now)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row")
	}
	// snippet() appends an ellipsis when truncating, so the budget+1 is the
	// expected ceiling.
	gotRunes := len([]rune(rows[0].Context))
	if gotRunes > inboxMentionContextLimit+1 {
		t.Errorf("context not trimmed: len=%d (budget=%d)", gotRunes, inboxMentionContextLimit)
	}
	if gotRunes < inboxMentionContextLimit {
		t.Errorf("context truncated too aggressively: len=%d", gotRunes)
	}
}

func TestComputeInboxRecentMentions_SortAndCap(t *testing.T) {
	now := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	beads := make([]*beadsapi.BeadDetail, 0, 8)
	for i := range 8 {
		ts := now.Add(time.Duration(-i) * time.Minute)
		beads = append(beads, mkMentionBead(
			"gd-A"+string(rune('0'+i)),
			"t",
			"",
			map[string]string{"spawned_by": "u", "task_title": "ctx"},
			ts,
		))
	}
	rows := computeInboxRecentMentions(beads, nil, nil, "/v", now)
	if len(rows) != inboxRecentMentionsLimit {
		t.Fatalf("expected cap=%d rows, got %d", inboxRecentMentionsLimit, len(rows))
	}
	for i := 1; i < len(rows); i++ {
		if rows[i].ActivityAt.After(rows[i-1].ActivityAt) {
			t.Errorf("rows not DESC by activity at index %d", i)
		}
	}
	if rows[0].ID != "gd-A0" {
		t.Errorf("newest first wrong: %q", rows[0].ID)
	}
}

func TestComputeInboxRecentMentions_DedupActiveAndRecentOverlap(t *testing.T) {
	now := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	a := mkMentionBead("gd-A1", "t", "",
		map[string]string{"spawned_by": "u", "task_title": "ctx"}, now)
	dup := mkMentionBead("gd-A1", "t", "",
		map[string]string{"spawned_by": "u", "task_title": "ctx"}, now)
	rows := computeInboxRecentMentions(
		[]*beadsapi.BeadDetail{a},
		[]*beadsapi.BeadDetail{dup},
		nil, "/v", now,
	)
	if len(rows) != 1 {
		t.Fatalf("expected dedupe to 1 row, got %d", len(rows))
	}
}

func TestComputeInboxRecentMentions_NilSafe(t *testing.T) {
	now := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	rows := computeInboxRecentMentions(
		[]*beadsapi.BeadDetail{nil},
		[]*beadsapi.BeadDetail{nil},
		[]*beadsapi.BeadDetail{nil},
		"/v", now,
	)
	if len(rows) != 0 {
		t.Fatalf("expected 0 rows for all-nil input, got %d", len(rows))
	}
}

func TestSortInboxMentions_TieBreakerByID(t *testing.T) {
	t1 := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	rows := []inboxMentionRow{
		{ID: "gd-B", ActivityAt: t1},
		{ID: "gd-A", ActivityAt: t1},
		{ID: "gd-C", ActivityAt: t1},
	}
	sortInboxMentions(rows)
	if rows[0].ID != "gd-A" || rows[1].ID != "gd-B" || rows[2].ID != "gd-C" {
		t.Errorf("tie-breaker order wrong: %+v", rows)
	}
}
