package bridge

import (
	"context"
	"log/slog"
	"testing"
)

// gd-t41kYFbLUt: an escalation the offline laptop holds gets one notice in
// the operator thread, however often its create and update events arrive.
func TestEscalationHolderOffline_OneNoticePerEscalation(t *testing.T) {
	client := newMockEscalationBeadClient()
	notifier := &mockEscalationNotifier{}
	esc := NewEscalations(EscalationsConfig{Daemon: client, EscalationNotifier: notifier, Logger: slog.Default()})
	at := "2026-11-05T01:00:00Z"
	bead := BeadEvent{ID: "gd-off1", Type: "escalation", Status: "open", Fields: map[string]string{
		"held_by": "coordinator:laptop", "execute_by": "coordinator:laptop",
		"held_by_offline_at": at, "held_by_last_seen_at": "2026-11-04T22:06:00Z",
	}}
	data := pingbackUpdateEvent(t, bead)
	esc.handleCreated(context.Background(), data)
	esc.handleUpdated(context.Background(), data)
	esc.handleUpdated(context.Background(), data)
	if len(notifier.offline) != 1 {
		t.Fatalf("got %d offline notices, want 1", len(notifier.offline))
	}
	if got := client.updatedFields["gd-off1"]["held_by_offline_notified_at"]; got != at {
		t.Fatalf("held_by_offline_notified_at = %q, want %q", got, at)
	}

	// After a bridge restart the stamp is the dedupe.
	bead.Fields["held_by_offline_notified_at"] = at
	fresh := NewEscalations(EscalationsConfig{Daemon: client, EscalationNotifier: notifier, Logger: slog.Default()})
	fresh.handleUpdated(context.Background(), pingbackUpdateEvent(t, bead))
	// An item held by a lane that is online carries no stamp.
	fresh.handleCreated(context.Background(), pingbackUpdateEvent(t, BeadEvent{ID: "gd-on1", Type: "escalation", Status: "open",
		Fields: map[string]string{"held_by": "coordinator:laptop"}}))
	if len(notifier.offline) != 1 {
		t.Fatalf("got %d offline notices after restart, want still 1", len(notifier.offline))
	}
}
