package bridge

import (
	"context"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// mockSLANudgeNotifier records each NotifySLANudge call, mirroring
// mockDigestNotifier (escalation_staleness_digest_test.go).
type mockSLANudgeNotifier struct {
	mu    sync.Mutex
	calls []mockSLANudgeCall
	err   error
}

type mockSLANudgeCall struct {
	bead BeadEvent
	rung SLARung
	age  time.Duration
}

func (m *mockSLANudgeNotifier) NotifySLANudge(_ context.Context, bead BeadEvent, rung SLARung, age time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.calls = append(m.calls, mockSLANudgeCall{bead, rung, age})
	return nil
}

func (m *mockSLANudgeNotifier) lastCall() (mockSLANudgeCall, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.calls) == 0 {
		return mockSLANudgeCall{}, false
	}
	return m.calls[len(m.calls)-1], true
}

// newSLASweepForTest builds a sweep with quiet hours disabled (nil), so tests
// exercising the ladder/dedupe/notify logic don't flake depending on the
// wall-clock time of day the test happens to run at. Quiet-hours behavior
// itself is covered separately by TestSLASweep_QuietHours_DefersWithoutStamping.
func newSLASweepForTest(client BeadClient, notifier SLANudgeNotifier, now time.Time) *SLANudgeSweep {
	policy := DefaultSLAPolicy()
	policy.QuietHours = nil
	s := NewSLANudgeSweep(SLANudgeSweepConfig{
		Daemon:   client,
		Notifier: notifier,
		Policy:   policy,
	})
	s.now = func() time.Time { return now }
	return s
}

// addOpenDecision seeds an open decision bead whose stall clock (created_at,
// since decisions have no notified_at) sits createdAgo in the past.
func addOpenDecision(client *mockEscalationBeadClient, id string, priority int, createdAgo time.Duration, now time.Time, fields map[string]string) {
	f := map[string]string{}
	for k, v := range fields {
		f[k] = v
	}
	client.beads[id] = &beadsapi.BeadDetail{
		ID:        id,
		Type:      "decision",
		Status:    "open",
		Title:     "decision " + id,
		Priority:  priority,
		Fields:    f,
		CreatedAt: now.Add(-createdAgo),
	}
}

func TestSLASweep_Rung1_Escalation(t *testing.T) {
	now := time.Now()
	client := newMockEscalationBeadClient()
	addStaleEscalation(client, "gd-e1", 1, 65*time.Minute, now, nil)

	s := newSLASweepForTest(client, nil, now)
	result := s.Sweep(context.Background())

	if result.NudgesSent != 1 {
		t.Fatalf("NudgesSent = %d, want 1", result.NudgesSent)
	}

	req, ok := client.updateBeadRequests["gd-e1"]
	if !ok || req.Priority == nil || *req.Priority != 0 {
		t.Fatalf("expected priority bump to P0, got %+v (ok=%v)", req.Priority, ok)
	}

	fields := client.updatedFields["gd-e1"]
	for _, key := range []string{"reflag_count", "reflagged_at", "reflagged_by", "reflag_reason",
		"pingback_requested_at", "pingback_count", "pingback_by", "pingback_source", "pingback_reason"} {
		if fields[key] == "" {
			t.Errorf("expected field %q to be stamped, fields=%+v", key, fields)
		}
	}
	if fields["reflagged_by"] != slaNudgeActor {
		t.Errorf("reflagged_by = %q, want %q", fields["reflagged_by"], slaNudgeActor)
	}

	foundLabel := false
	for _, lc := range client.labelCalls {
		if lc.beadID == "gd-e1" && lc.label == "escalated" {
			foundLabel = true
		}
	}
	if !foundLabel {
		t.Errorf("expected 'escalated' label on gd-e1, got %+v", client.labelCalls)
	}

	if len(client.comments) != 1 || client.comments[0].beadID != "gd-e1" || client.comments[0].author != slaNudgeActor {
		t.Errorf("expected one audit comment from %q, got %+v", slaNudgeActor, client.comments)
	}
}

func TestSLASweep_Rung1_Dedupe_NoRefire(t *testing.T) {
	now := time.Now()
	client := newMockEscalationBeadClient()
	// reflagged 30m ago; P1's rung1 fraction is 1h, so this is not due yet.
	addStaleEscalation(client, "gd-e2", 1, 65*time.Minute, now, map[string]string{
		"reflagged_at": now.Add(-30 * time.Minute).UTC().Format(time.RFC3339),
	})

	s := newSLASweepForTest(client, nil, now)
	result := s.Sweep(context.Background())

	if result.NudgesSent != 0 {
		t.Fatalf("NudgesSent = %d, want 0 (deduped)", result.NudgesSent)
	}
	if _, ok := client.updateBeadRequests["gd-e2"]; ok {
		t.Errorf("expected no priority update while deduped, got %+v", client.updateBeadRequests["gd-e2"])
	}
	if len(client.comments) != 0 {
		t.Errorf("expected no audit comment while deduped, got %+v", client.comments)
	}
}

// applyPersistedPatch merges a Sweep's recorded UpdateBead/UpdateBeadFields
// calls into the mock's stored bead, mimicking what a real daemon write +
// the next sweep's re-list would observe. The mock intentionally keeps
// mutations separate from client.beads (so tests can assert on exactly what
// was written) — multi-sweep regression tests need to replay that write
// themselves between Sweep() calls.
func applyPersistedPatch(client *mockEscalationBeadClient, id string) {
	if req, ok := client.updateBeadRequests[id]; ok && req.Priority != nil {
		client.beads[id].Priority = *req.Priority
	}
	for k, v := range client.updatedFields[id] {
		client.beads[id].Fields[k] = v
	}
}

// TestSLASweep_Rung1PriorityBump_AnchorsSLAToOriginalPriority is the
// merge-desk-requested regression test for PR #173 finding 1: rung 1 bumps
// priority (e.g. P1->P0), and a later sweep must keep measuring age against
// the bead's ORIGINAL priority's SLA (P1, 4h), not the live post-bump
// priority (P0, 1h) — otherwise a P1 escalation nudged to P0 at ~1h gets
// falsely marked overdue at ~1h05m instead of its real 4h SLA.
func TestSLASweep_Rung1PriorityBump_AnchorsSLAToOriginalPriority(t *testing.T) {
	now := time.Now()
	client := newMockEscalationBeadClient()
	// P1 SLA=4h, rung1 fires at 25%=1h. Seed just past rung1.
	addStaleEscalation(client, "gd-anchor1", 1, 65*time.Minute, now, nil)

	s := newSLASweepForTest(client, nil, now)
	result := s.Sweep(context.Background())
	if result.NudgesSent != 1 {
		t.Fatalf("sweep1: NudgesSent = %d, want 1", result.NudgesSent)
	}

	req, ok := client.updateBeadRequests["gd-anchor1"]
	if !ok || req.Priority == nil || *req.Priority != 0 {
		t.Fatalf("expected priority bumped to P0, got %+v (ok=%v)", req, ok)
	}
	applyPersistedPatch(client, "gd-anchor1")
	if got := client.beads["gd-anchor1"].Fields[slaBasePriorityField]; got != "1" {
		t.Fatalf("sla_base_priority = %q, want %q (anchored to the original P1)", got, "1")
	}

	// Second sweep bringing total age (since the anchor) to ~2h15m: the live
	// priority is now P0 (1h SLA), so the bug being fixed here would falsely
	// fire rung 3 (overdue) here. Anchored to the original P1 (4h SLA, rung2
	// at 2.4h), nothing is due yet.
	now2 := now.Add(70 * time.Minute)
	s.now = func() time.Time { return now2 }
	result2 := s.Sweep(context.Background())
	if result2.NudgesSent != 0 {
		t.Fatalf("sweep2: NudgesSent = %d, want 0 (SLA must stay anchored to the original P1 4h window)", result2.NudgesSent)
	}
	if result2.OverdueCount["escalation"] != 0 {
		t.Fatalf("sweep2: OverdueCount = %d, want 0 (must not breach P0's 1h SLA via the bumped live priority)", result2.OverdueCount["escalation"])
	}
}

// TestSLASweep_Rung1_NoRefireAcrossSweeps is the merge-desk-requested
// regression test for PR #173 finding 2: rung 1 must fire at most once per
// bead. Previously the dedupe compared reflagged_at's age against the
// rung-1 SLA fraction, so it re-fired (and double-bumped priority) every
// Rung1Fraction*SLA interval after the first fire.
func TestSLASweep_Rung1_NoRefireAcrossSweeps(t *testing.T) {
	now := time.Now()
	client := newMockEscalationBeadClient()
	// P2 SLA=24h, rung1 at 25%=6h. Seed just past rung1.
	addStaleEscalation(client, "gd-norefire1", 2, 6*time.Hour+5*time.Minute, now, nil)

	s := newSLASweepForTest(client, nil, now)
	result := s.Sweep(context.Background())
	if result.NudgesSent != 1 {
		t.Fatalf("sweep1: NudgesSent = %d, want 1", result.NudgesSent)
	}
	if got := client.updatedFields["gd-norefire1"]["reflag_count"]; got != "1" {
		t.Fatalf("reflag_count = %q, want %q", got, "1")
	}
	applyPersistedPatch(client, "gd-norefire1")
	if got := client.beads["gd-norefire1"].Priority; got != 1 {
		t.Fatalf("sweep1: priority = P%d, want P1", got)
	}

	// Second sweep at ~12h10m TOTAL age since the anchor (notified_at):
	// exactly the merge-desk repro — another Rung1Fraction*SLA (6h) has
	// elapsed since the rung-1 fire at ~6h05m, but still short of rung 2's
	// 14.4h threshold, so this isolates the rung-1-specific refire bug from
	// a legitimate rung-2 escalation.
	now2 := now.Add(6*time.Hour + 5*time.Minute)
	s.now = func() time.Time { return now2 }
	result2 := s.Sweep(context.Background())
	if result2.NudgesSent != 0 {
		t.Fatalf("sweep2: NudgesSent = %d, want 0 (rung 1 fires at most once per bead)", result2.NudgesSent)
	}
	if got := client.beads["gd-norefire1"].Priority; got != 1 {
		t.Fatalf("sweep2: priority = P%d, want still P1 (must not double-bump)", got)
	}
}

func TestSLASweep_Rung2_Escalation_StampsQueueAlertOnly(t *testing.T) {
	now := time.Now()
	client := newMockEscalationBeadClient()
	// P1: rung2 at 2.4h (144m); 150m is past rung2, short of rung3 (4h).
	addStaleEscalation(client, "gd-e3", 1, 150*time.Minute, now, nil)

	notifier := &mockSLANudgeNotifier{}
	s := newSLASweepForTest(client, notifier, now)
	result := s.Sweep(context.Background())

	if result.NudgesSent != 1 {
		t.Fatalf("NudgesSent = %d, want 1", result.NudgesSent)
	}
	fields := client.updatedFields["gd-e3"]
	if len(fields) != 1 || fields["queue_alert_requested_at"] == "" {
		t.Errorf("expected only queue_alert_requested_at stamped, got %+v", fields)
	}
	if len(notifier.calls) != 0 {
		t.Errorf("escalation rung2 should not call the direct notifier, got %+v", notifier.calls)
	}
}

func TestSLASweep_Rung2_Decision_NotifiesAndStamps(t *testing.T) {
	now := time.Now()
	client := newMockEscalationBeadClient()
	addOpenDecision(client, "gd-d1", 1, 150*time.Minute, now, nil)

	notifier := &mockSLANudgeNotifier{}
	s := newSLASweepForTest(client, notifier, now)
	result := s.Sweep(context.Background())

	if result.NudgesSent != 1 {
		t.Fatalf("NudgesSent = %d, want 1", result.NudgesSent)
	}
	call, ok := notifier.lastCall()
	if !ok {
		t.Fatal("expected a notifier call for decision rung2")
	}
	if call.bead.ID != "gd-d1" || call.rung != SLARung2 {
		t.Errorf("notifier call = %+v, want bead gd-d1 rung2", call)
	}

	fields := client.updatedFields["gd-d1"]
	if fields["decision_pingback_requested_at"] == "" || fields["decision_pingback_count"] == "" {
		t.Errorf("expected decision_pingback_* fields stamped, got %+v", fields)
	}
	if fields["decision_pingback_source"] != "sla-nudge" {
		t.Errorf("decision_pingback_source = %q, want sla-nudge", fields["decision_pingback_source"])
	}
}

func TestSLASweep_Rung3_Overdue_BothTypes(t *testing.T) {
	now := time.Now()
	client := newMockEscalationBeadClient()
	// P1 SLA is 4h; 5h is past it for both bead types.
	addStaleEscalation(client, "gd-e4", 1, 5*time.Hour, now, nil)
	addOpenDecision(client, "gd-d2", 1, 5*time.Hour, now, nil)

	notifier := &mockSLANudgeNotifier{}
	s := newSLASweepForTest(client, notifier, now)
	result := s.Sweep(context.Background())

	if result.NudgesSent != 2 {
		t.Fatalf("NudgesSent = %d, want 2", result.NudgesSent)
	}
	if result.OverdueCount["escalation"] != 1 || result.OverdueCount["decision"] != 1 {
		t.Fatalf("OverdueCount = %+v, want 1/1", result.OverdueCount)
	}

	for _, id := range []string{"gd-e4", "gd-d2"} {
		if client.updatedFields[id]["sla_breached"] != "true" {
			t.Errorf("%s: sla_breached not stamped true, got %+v", id, client.updatedFields[id])
		}
		found := false
		for _, lc := range client.labelCalls {
			if lc.beadID == id && lc.label == "overdue" {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: expected 'overdue' label, got %+v", id, client.labelCalls)
		}
	}

	if len(notifier.calls) != 2 {
		t.Fatalf("expected 2 notifier calls, got %d: %+v", len(notifier.calls), notifier.calls)
	}
	for _, c := range notifier.calls {
		if c.rung != SLARung3 {
			t.Errorf("notifier call rung = %v, want SLARung3", c.rung)
		}
	}
}

func TestSLASweep_Rung3_AlreadyBreached_NoRefire_StillCounted(t *testing.T) {
	now := time.Now()
	client := newMockEscalationBeadClient()
	addStaleEscalation(client, "gd-e5", 1, 5*time.Hour, now, map[string]string{"sla_breached": "true"})

	notifier := &mockSLANudgeNotifier{}
	s := newSLASweepForTest(client, notifier, now)
	result := s.Sweep(context.Background())

	if result.NudgesSent != 0 {
		t.Fatalf("NudgesSent = %d, want 0 (already breached)", result.NudgesSent)
	}
	if result.OverdueCount["escalation"] != 1 {
		t.Fatalf("OverdueCount[escalation] = %d, want 1 (standing gauge counts regardless of refire)", result.OverdueCount["escalation"])
	}
	if len(notifier.calls) != 0 {
		t.Errorf("expected no notifier call for an already-breached bead, got %+v", notifier.calls)
	}
}

func TestSLASweep_QuietHours_DefersWithoutStamping(t *testing.T) {
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Skipf("tzdata not available: %v", err)
	}
	// 22:00 PT is inside the default 21:00-08:00 quiet window.
	now := time.Date(2026, 1, 15, 22, 0, 0, 0, loc)

	client := newMockEscalationBeadClient()
	addStaleEscalation(client, "gd-e6", 1, 65*time.Minute, now, nil)

	// Uses DefaultSLAPolicy() directly (quiet hours ON), unlike
	// newSLASweepForTest's other callers.
	s := NewSLANudgeSweep(SLANudgeSweepConfig{Daemon: client, Policy: DefaultSLAPolicy()})
	s.now = func() time.Time { return now }
	result := s.Sweep(context.Background())

	if result.NudgesSent != 0 {
		t.Fatalf("NudgesSent = %d, want 0 during quiet hours", result.NudgesSent)
	}
	if _, ok := client.updatedFields["gd-e6"]; ok {
		t.Errorf("expected no fields stamped during quiet hours, got %+v", client.updatedFields["gd-e6"])
	}
	if len(client.comments) != 0 {
		t.Errorf("expected no audit comment during quiet hours, got %+v", client.comments)
	}
}

func TestSLASweep_ListsOnlyOpenAndInProgress(t *testing.T) {
	now := time.Now()
	client := newMockEscalationBeadClient()
	addStaleEscalation(client, "gd-e7", 1, 65*time.Minute, now, nil)
	addOpenDecision(client, "gd-d3", 1, 65*time.Minute, now, nil)

	s := newSLASweepForTest(client, nil, now)
	s.Sweep(context.Background())

	if len(client.listQueries) != 2 {
		t.Fatalf("expected 2 list queries (escalation + decision), got %d", len(client.listQueries))
	}
	wantStatuses := []string{"open", "in_progress"}
	for _, q := range client.listQueries {
		if len(q.Statuses) != len(wantStatuses) || q.Statuses[0] != wantStatuses[0] || q.Statuses[1] != wantStatuses[1] {
			t.Errorf("query Statuses = %v, want %v (excludes status=deferred — the snooze mechanism)", q.Statuses, wantStatuses)
		}
	}
}
