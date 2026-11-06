package main

import (
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func mkItem(id, status, successor string, createdAgo time.Duration, now time.Time, extra map[string]string) *beadsapi.BeadDetail {
	fields := map[string]string{}
	if successor != "" {
		fields["successor"] = successor
	}
	for k, v := range extra {
		fields[k] = v
	}
	return &beadsapi.BeadDetail{
		ID:        id,
		Type:      "decision",
		Status:    status,
		Fields:    fields,
		CreatedAt: now.Add(-createdAgo),
	}
}

func TestResolveSuccessors_RoutesIdleItemWithSuccessor(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	beads := []*beadsapi.BeadDetail{
		mkItem("gd-idle", "open", "gd-user1", 2*time.Hour, now, nil),
	}
	got := resolveSuccessors(beads, successorIdleThreshold, now)
	if len(got) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(got))
	}
	if got[0].Bead.ID != "gd-idle" || got[0].Successor != "gd-user1" {
		t.Fatalf("unexpected candidate: %+v", got[0])
	}
	if got[0].IdleFor < 2*time.Hour-time.Minute {
		t.Fatalf("idle window wrong: %s", got[0].IdleFor)
	}
}

func TestResolveSuccessors_SkipsFreshItem(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	beads := []*beadsapi.BeadDetail{
		mkItem("gd-fresh", "open", "gd-user1", 30*time.Minute, now, nil),
	}
	if got := resolveSuccessors(beads, successorIdleThreshold, now); len(got) != 0 {
		t.Fatalf("expected 0 candidates for a fresh item, got %d", len(got))
	}
}

func TestResolveSuccessors_SkipsItemWithoutSuccessor(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	beads := []*beadsapi.BeadDetail{
		mkItem("gd-nosucc", "open", "", 5*time.Hour, now, nil),
	}
	if got := resolveSuccessors(beads, successorIdleThreshold, now); len(got) != 0 {
		t.Fatalf("expected 0 candidates without a successor, got %d", len(got))
	}
}

func TestResolveSuccessors_SkipsClosedItem(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	beads := []*beadsapi.BeadDetail{
		mkItem("gd-closed", "closed", "gd-user1", 5*time.Hour, now, nil),
	}
	if got := resolveSuccessors(beads, successorIdleThreshold, now); len(got) != 0 {
		t.Fatalf("expected 0 candidates for a closed item, got %d", len(got))
	}
}

func TestResolveSuccessors_SkipsAnsweredDecision(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	beads := []*beadsapi.BeadDetail{
		// Old, has a successor, but the operator already chose an option — the
		// close just lagged. Must NOT be re-routed.
		mkItem("gd-answered", "open", "gd-user1", 5*time.Hour, now, map[string]string{"chosen": "a"}),
		mkItem("gd-responded", "open", "gd-user1", 5*time.Hour, now, map[string]string{"response_text": "go ahead"}),
	}
	if got := resolveSuccessors(beads, successorIdleThreshold, now); len(got) != 0 {
		t.Fatalf("expected 0 candidates for answered items, got %d", len(got))
	}
}

func TestResolveSuccessors_RecentPingbackResetsIdleClock(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	// Created 5h ago but re-surfaced 20m ago: the owner-facing clock is fresh,
	// so it is NOT yet eligible for successor routing.
	beads := []*beadsapi.BeadDetail{
		mkItem("gd-recent-ping", "open", "gd-user1", 5*time.Hour, now, map[string]string{
			"pingback_requested_at": rfc3339(now.Add(-20 * time.Minute)),
		}),
	}
	if got := resolveSuccessors(beads, successorIdleThreshold, now); len(got) != 0 {
		t.Fatalf("expected 0 candidates when pingback is recent, got %d", len(got))
	}

	// A pingback OLDER than the threshold does not save it.
	stale := []*beadsapi.BeadDetail{
		mkItem("gd-stale-ping", "open", "gd-user1", 5*time.Hour, now, map[string]string{
			"pingback_requested_at": rfc3339(now.Add(-3 * time.Hour)),
		}),
	}
	if got := resolveSuccessors(stale, successorIdleThreshold, now); len(got) != 1 {
		t.Fatalf("expected 1 candidate for a stale pingback, got %d", len(got))
	}
}

func TestResolveSuccessors_SortsMostIdleFirst(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	beads := []*beadsapi.BeadDetail{
		mkItem("gd-2h", "open", "gd-user1", 2*time.Hour, now, nil),
		mkItem("gd-9h", "open", "gd-user2", 9*time.Hour, now, nil),
		mkItem("gd-4h", "open", "gd-user3", 4*time.Hour, now, nil),
	}
	got := resolveSuccessors(beads, successorIdleThreshold, now)
	if len(got) != 3 {
		t.Fatalf("expected 3 candidates, got %d", len(got))
	}
	if got[0].Bead.ID != "gd-9h" || got[1].Bead.ID != "gd-4h" || got[2].Bead.ID != "gd-2h" {
		t.Fatalf("wrong sort order: %s, %s, %s", got[0].Bead.ID, got[1].Bead.ID, got[2].Bead.ID)
	}
}

func TestResolveSuccessors_CustomThreshold(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	beads := []*beadsapi.BeadDetail{
		mkItem("gd-45m", "open", "gd-user1", 45*time.Minute, now, nil),
	}
	// Default 1h threshold: not eligible.
	if got := resolveSuccessors(beads, successorIdleThreshold, now); len(got) != 0 {
		t.Fatalf("expected 0 candidates at 1h threshold, got %d", len(got))
	}
	// Tighter 30m threshold: eligible.
	if got := resolveSuccessors(beads, 30*time.Minute, now); len(got) != 1 {
		t.Fatalf("expected 1 candidate at 30m threshold, got %d", len(got))
	}
}

func TestResolveSuccessors_ToleratesNilBead(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	beads := []*beadsapi.BeadDetail{
		nil,
		mkItem("gd-ok", "open", "gd-user1", 2*time.Hour, now, nil),
		nil,
	}
	if got := resolveSuccessors(beads, successorIdleThreshold, now); len(got) != 1 {
		t.Fatalf("expected 1 candidate skipping nils, got %d", len(got))
	}
}

func TestSuccessorLastTouch_UsesFreshestStamp(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	b := mkItem("gd-x", "open", "gd-user1", 10*time.Hour, now, map[string]string{
		"reflagged_at":          rfc3339(now.Add(-6 * time.Hour)),
		"pingback_requested_at": rfc3339(now.Add(-1 * time.Hour)),
	})
	got := successorLastTouch(b)
	want := now.Add(-1 * time.Hour)
	if !got.Equal(want) {
		t.Fatalf("expected freshest touch %s, got %s", want, got)
	}
}
