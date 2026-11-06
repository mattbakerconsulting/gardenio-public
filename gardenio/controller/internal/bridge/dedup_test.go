package bridge

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

func TestDedup_Seen(t *testing.T) {
	d := NewDedup(slog.Default())

	// First call: not seen.
	if d.Seen("created:dec-1") {
		t.Fatal("expected first call to return false")
	}

	// Second call: already seen.
	if !d.Seen("created:dec-1") {
		t.Fatal("expected second call to return true")
	}
}

func TestDedup_Mark(t *testing.T) {
	d := NewDedup(slog.Default())

	d.Mark("resolved:dec-2")

	// Now Seen should return true.
	if !d.Seen("resolved:dec-2") {
		t.Fatal("expected Seen to return true after Mark")
	}
}

func TestDedup_DifferentKeys(t *testing.T) {
	d := NewDedup(slog.Default())

	// Different prefixes for same bead should be independent.
	if d.Seen("created:dec-3") {
		t.Fatal("expected created key to be unseen")
	}
	if d.Seen("resolved:dec-3") {
		t.Fatal("expected resolved key to be unseen (different prefix)")
	}

	// Original key should now be seen.
	if !d.Seen("created:dec-3") {
		t.Fatal("expected created key to be seen after first Seen call")
	}
}

func TestDedup_CatchUpDecisions_Empty(t *testing.T) {
	d := NewDedup(slog.Default())
	daemon := newMockDaemon()
	notif := &mockNotifier{}

	d.CatchUpDecisions(context.Background(), daemon, notif, slog.Default())

	// No decisions to catch up.
	if len(notif.getCreated()) != 0 {
		t.Fatal("expected no notifications for empty daemon")
	}
}

func TestDedup_CatchUpDecisions_SkipsResolved(t *testing.T) {
	d := NewDedup(slog.Default())
	daemon := newMockDaemon()
	daemon.beads["dec-resolved"] = &beadsapi.BeadDetail{
		ID:   "dec-resolved",
		Type: "decision",
		Fields: map[string]string{
			"question": "Deploy?",
			"chosen":   "yes",
		},
	}
	notif := &mockNotifier{}

	d.CatchUpDecisions(context.Background(), daemon, notif, slog.Default())

	// Resolved decisions should not be notified.
	if len(notif.getCreated()) != 0 {
		t.Fatal("expected no notifications for resolved decisions")
	}

	// But should be marked as seen.
	if !d.Seen("resolved:dec-resolved") {
		t.Fatal("expected resolved decision to be marked")
	}
}

func TestDedup_CatchUpDecisions_NilDaemon(t *testing.T) {
	d := NewDedup(slog.Default())

	// Should not panic with nil daemon.
	d.CatchUpDecisions(context.Background(), nil, nil, slog.Default())
}

func TestDedup_CatchUpAgents_MarksActiveAgents(t *testing.T) {
	d := NewDedup(slog.Default())
	daemon := newMockDaemon()
	daemon.beads["agent-active-1"] = &beadsapi.BeadDetail{
		ID:     "agent-active-1",
		Type:   "agent",
		Fields: map[string]string{"agent": "worker-1", "agent_state": "working"},
	}
	daemon.beads["agent-active-2"] = &beadsapi.BeadDetail{
		ID:     "agent-active-2",
		Type:   "agent",
		Fields: map[string]string{"agent": "worker-2", "agent_state": "spawning"},
	}

	d.CatchUpAgents(context.Background(), daemon, slog.Default())

	// Both active agents should be marked as seen for created events.
	if !d.Seen("beads.bead.created:agent-active-1") {
		t.Fatal("expected agent-active-1 created event to be marked")
	}
	if !d.Seen("beads.bead.created:agent-active-2") {
		t.Fatal("expected agent-active-2 created event to be marked")
	}
}

func TestDedup_CatchUpAgents_NilDaemon(t *testing.T) {
	d := NewDedup(slog.Default())

	// Should not panic with nil daemon.
	d.CatchUpAgents(context.Background(), nil, slog.Default())
}

func TestDedup_CatchUpAgents_Empty(t *testing.T) {
	d := NewDedup(slog.Default())
	daemon := newMockDaemon()

	d.CatchUpAgents(context.Background(), daemon, slog.Default())

	// No agents to mark — a previously unseen key should still be unseen.
	if d.Seen("beads.bead.created:nonexistent") {
		t.Fatal("expected no keys to be pre-marked for empty daemon")
	}
}

func TestDedup_CatchUpDecisions_SkipsClosedAgentDecisions(t *testing.T) {
	d := NewDedup(slog.Default())
	daemon := newMockDaemon()

	// Add a done agent.
	daemon.beads["agent-done"] = &beadsapi.BeadDetail{
		ID:   "agent-done",
		Type: "agent",
		Fields: map[string]string{
			"agent":       "worker-done",
			"agent_state": "done",
		},
	}
	// Add a working agent.
	daemon.beads["agent-active"] = &beadsapi.BeadDetail{
		ID:   "agent-active",
		Type: "agent",
		Fields: map[string]string{
			"agent":       "worker-active",
			"agent_state": "working",
		},
	}

	// Decision assigned to the done agent — should be skipped.
	daemon.beads["dec-stale"] = &beadsapi.BeadDetail{
		ID:       "dec-stale",
		Type:     "decision",
		Assignee: "worker-done",
		Fields:   map[string]string{"question": "Stale?"},
	}
	// Decision assigned to the active agent — should be notified.
	daemon.beads["dec-active"] = &beadsapi.BeadDetail{
		ID:       "dec-active",
		Type:     "decision",
		Assignee: "worker-active",
		Fields:   map[string]string{"question": "Active?"},
	}

	notif := &mockNotifier{}
	d.CatchUpDecisions(context.Background(), daemon, notif, slog.Default())

	created := notif.getCreated()
	if len(created) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(created))
	}
	if created[0].ID != "dec-active" {
		t.Fatalf("expected dec-active to be notified, got %s", created[0].ID)
	}

	// Stale decision should be marked as seen so SSE replay skips it too.
	if !d.Seen("created:dec-stale") {
		t.Fatal("expected stale decision to be marked as seen")
	}
}

func TestDedup_CatchUpDecisions_PrePopulatesDedup(t *testing.T) {
	d := NewDedup(slog.Default())
	daemon := newMockDaemon()
	daemon.beads["dec-pending"] = &beadsapi.BeadDetail{
		ID:   "dec-pending",
		Type: "decision",
		Fields: map[string]string{
			"question": "Deploy?",
		},
	}

	// Catch up with nil notifier (just mark as seen).
	d.CatchUpDecisions(context.Background(), daemon, nil, slog.Default())

	// Should be marked as seen.
	if !d.Seen("created:dec-pending") {
		t.Fatal("expected pending decision to be marked after catch-up")
	}
}

func TestDedup_CatchUpDecisions_SkipsOldDecisions(t *testing.T) {
	d := NewDedup(slog.Default())
	daemon := newMockDaemon()

	// Old decision (2 hours ago) — should be skipped.
	daemon.beads["dec-old"] = &beadsapi.BeadDetail{
		ID:        "dec-old",
		Type:      "decision",
		CreatedAt: time.Now().Add(-2 * time.Hour),
		Fields:    map[string]string{"question": "Old?"},
	}
	// Recent decision (5 minutes ago) — should be notified.
	daemon.beads["dec-recent"] = &beadsapi.BeadDetail{
		ID:        "dec-recent",
		Type:      "decision",
		CreatedAt: time.Now().Add(-5 * time.Minute),
		Fields:    map[string]string{"question": "Recent?"},
	}

	notif := &mockNotifier{}
	d.CatchUpDecisions(context.Background(), daemon, notif, slog.Default())

	created := notif.getCreated()
	if len(created) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(created))
	}
	if created[0].ID != "dec-recent" {
		t.Fatalf("expected dec-recent to be notified, got %s", created[0].ID)
	}

	// Old decision should still be marked as seen (dedup).
	if !d.Seen("created:dec-old") {
		t.Fatal("expected old decision to be marked as seen")
	}
}

func TestDedup_CatchUpDecisions_SkipsAlreadyPosted(t *testing.T) {
	d := NewDedup(slog.Default())
	daemon := newMockDaemon()

	// Decision already posted to Slack (has slack_ts) — should be skipped.
	daemon.beads["dec-posted"] = &beadsapi.BeadDetail{
		ID:        "dec-posted",
		Type:      "decision",
		CreatedAt: time.Now().Add(-5 * time.Minute),
		Fields:    map[string]string{"question": "Posted?", "slack_ts": "1234567890.123456"},
	}
	// Decision not yet posted — should be notified.
	daemon.beads["dec-new"] = &beadsapi.BeadDetail{
		ID:        "dec-new",
		Type:      "decision",
		CreatedAt: time.Now().Add(-5 * time.Minute),
		Fields:    map[string]string{"question": "New?"},
	}

	notif := &mockNotifier{}
	d.CatchUpDecisions(context.Background(), daemon, notif, slog.Default())

	created := notif.getCreated()
	if len(created) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(created))
	}
	if created[0].ID != "dec-new" {
		t.Fatalf("expected dec-new to be notified, got %s", created[0].ID)
	}

	// Posted decision should be marked as seen.
	if !d.Seen("created:dec-posted") {
		t.Fatal("expected posted decision to be marked as seen")
	}
}

func TestDedup_CatchUpDecisions_ZeroCreatedAtNotSkipped(t *testing.T) {
	d := NewDedup(slog.Default())
	daemon := newMockDaemon()

	// Decision with zero CreatedAt (e.g. missing from API response) — should
	// NOT be skipped by age filter (treat as unknown age = recent).
	daemon.beads["dec-notime"] = &beadsapi.BeadDetail{
		ID:     "dec-notime",
		Type:   "decision",
		Fields: map[string]string{"question": "NoTime?"},
	}

	notif := &mockNotifier{}
	d.CatchUpDecisions(context.Background(), daemon, notif, slog.Default())

	created := notif.getCreated()
	if len(created) != 1 {
		t.Fatalf("expected 1 notification for zero-time decision, got %d", len(created))
	}
}
