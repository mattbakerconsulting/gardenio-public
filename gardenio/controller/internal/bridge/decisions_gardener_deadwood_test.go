package bridge

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// hasLabel reports whether the seeded subject bead carries label after a
// handleClosed run. Reads the mock's bead map directly (single-goroutine test).
func (m *mockDaemon) hasLabel(beadID, label string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	bead, ok := m.beads[beadID]
	if !ok {
		return false
	}
	for _, l := range bead.Labels {
		if l == label {
			return true
		}
	}
	return false
}

func newDeadWoodDecisions(daemon *mockDaemon, notif *mockNotifier) *Decisions {
	return &Decisions{
		daemon:    daemon,
		notifier:  notif,
		logger:    slog.Default(),
		escalated: make(map[string]time.Time),
	}
}

// TestDecisions_Dismissed_StampsDeadWoodOnSubject is the happy path: an operator
// DISMISSES a revival-review decision carrying subject_trigger_id (C1's machine-
// readable subject), so the reactor stamps gardener-retired-dead-wood on the
// subject trigger bead and posts an audit comment naming the dismissing decision.
func TestDecisions_Dismissed_StampsDeadWoodOnSubject(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["trig-1"] = &beadsapi.BeadDetail{ID: "trig-1", Type: "trigger"}
	notif := &mockNotifier{}
	d := newDeadWoodDecisions(daemon, notif)

	dismissedEvent := marshalSSEBeadPayload(BeadEvent{
		ID:   "dec-dw-1",
		Type: "decision",
		Fields: map[string]string{
			"chosen":             "dismissed",
			"subject_trigger_id": "trig-1",
		},
	})
	d.handleClosed(context.Background(), dismissedEvent)

	// Slack message still dismissed (reactor must not disturb existing handling).
	if got := notif.getDismissed(); len(got) != 1 || got[0] != "dec-dw-1" {
		t.Fatalf("expected 1 dismiss call for dec-dw-1, got %v", got)
	}

	// Label stamped on the subject.
	if !daemon.hasLabel("trig-1", gardenerRetiredDeadWoodLabel) {
		t.Fatalf("expected label %q on trig-1, got %v", gardenerRetiredDeadWoodLabel, daemon.beads["trig-1"].Labels)
	}

	// Audit comment posted on the subject, naming the dismissing decision.
	comments := daemon.getComments()
	var found bool
	for _, c := range comments {
		if c.BeadID == "trig-1" && strings.Contains(c.Text, "dec-dw-1") {
			found = true
			if c.Author != gardenerRetiredDeadWoodActor {
				t.Errorf("audit comment author = %q, want %q", c.Author, gardenerRetiredDeadWoodActor)
			}
		}
	}
	if !found {
		t.Errorf("expected audit comment on trig-1 naming dec-dw-1, got %+v", comments)
	}
}

// TestDecisions_Expired_DoesNotStampDeadWood is the critical guard rail: _expired
// shares the dismissal branch but is a timeout, NOT an operator judgement — it
// must never stamp, even when subject_trigger_id is present.
func TestDecisions_Expired_DoesNotStampDeadWood(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["trig-2"] = &beadsapi.BeadDetail{ID: "trig-2", Type: "trigger"}
	notif := &mockNotifier{}
	d := newDeadWoodDecisions(daemon, notif)

	expiredEvent := marshalSSEBeadPayload(BeadEvent{
		ID:   "dec-dw-2",
		Type: "decision",
		Fields: map[string]string{
			"chosen":             "_expired",
			"subject_trigger_id": "trig-2",
		},
	})
	d.handleClosed(context.Background(), expiredEvent)

	if daemon.hasLabel("trig-2", gardenerRetiredDeadWoodLabel) {
		t.Errorf("_expired must NOT stamp dead-wood label; trig-2 labels = %v", daemon.beads["trig-2"].Labels)
	}
	if got := daemon.getComments(); len(got) != 0 {
		t.Errorf("_expired must NOT post an audit comment, got %+v", got)
	}
}

// TestDecisions_Dismissed_NoSubject_Inert covers the pre-C1 / non-revival path:
// a dismissed decision with no subject_trigger_id must be inert — no label, no
// comment, and no stray write.
func TestDecisions_Dismissed_NoSubject_Inert(t *testing.T) {
	daemon := newMockDaemon()
	notif := &mockNotifier{}
	d := newDeadWoodDecisions(daemon, notif)

	dismissedEvent := marshalSSEBeadPayload(BeadEvent{
		ID:   "dec-dw-3",
		Type: "decision",
		Fields: map[string]string{
			"chosen": "dismissed",
		},
	})
	d.handleClosed(context.Background(), dismissedEvent)

	// Still dismisses the Slack message — the reactor is a pure addition.
	if got := notif.getDismissed(); len(got) != 1 {
		t.Fatalf("expected 1 dismiss call, got %v", got)
	}
	if got := daemon.getComments(); len(got) != 0 {
		t.Errorf("no-subject dismissal must not post a comment, got %+v", got)
	}
}

// TestDecisions_Dismissed_Idempotent verifies re-processing the same dismissal
// (label already present) is a no-op that never errors and never breaks handling.
func TestDecisions_Dismissed_Idempotent(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["trig-4"] = &beadsapi.BeadDetail{
		ID:     "trig-4",
		Type:   "trigger",
		Labels: []string{gardenerRetiredDeadWoodLabel},
	}
	notif := &mockNotifier{}
	d := newDeadWoodDecisions(daemon, notif)

	dismissedEvent := marshalSSEBeadPayload(BeadEvent{
		ID:   "dec-dw-4",
		Type: "decision",
		Fields: map[string]string{
			"chosen":             "dismissed",
			"subject_trigger_id": "trig-4",
		},
	})
	d.handleClosed(context.Background(), dismissedEvent)

	// Label present exactly once (no duplicate append).
	var count int
	for _, l := range daemon.beads["trig-4"].Labels {
		if l == gardenerRetiredDeadWoodLabel {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected label present exactly once, got %d (%v)", count, daemon.beads["trig-4"].Labels)
	}
}

// TestDecisions_Dismissed_StampFailureIsBestEffort verifies a stamp failure
// (subject bead missing → mock AddLabel errors) is swallowed: the dismissal
// still completes and no panic escapes.
func TestDecisions_Dismissed_StampFailureIsBestEffort(t *testing.T) {
	daemon := newMockDaemon() // trig-missing intentionally not seeded → AddLabel errors
	notif := &mockNotifier{}
	d := newDeadWoodDecisions(daemon, notif)

	dismissedEvent := marshalSSEBeadPayload(BeadEvent{
		ID:   "dec-dw-5",
		Type: "decision",
		Fields: map[string]string{
			"chosen":             "dismissed",
			"subject_trigger_id": "trig-missing",
		},
	})
	d.handleClosed(context.Background(), dismissedEvent)

	// Dismissal still happened despite the label-add failure.
	if got := notif.getDismissed(); len(got) != 1 {
		t.Fatalf("stamp failure must not break dismissal; dismiss calls = %v", got)
	}
	// Label add failed, so no audit comment either (short-circuit after label).
	if got := daemon.getComments(); len(got) != 0 {
		t.Errorf("expected no comment when label add fails, got %+v", got)
	}
}
