package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// ── ladder math ──────────────────────────────────────────────────────────

func TestAutoPingbackThreshold_Ladder(t *testing.T) {
	want := []time.Duration{
		12 * time.Hour,
		24 * time.Hour,
		48 * time.Hour,
		96 * time.Hour,
		192 * time.Hour,
	}
	for i, w := range want {
		if got := autoPingbackThreshold(i + 1); got != w {
			t.Errorf("autoPingbackThreshold(%d) = %v, want %v", i+1, got, w)
		}
	}
}

func TestAutoPingbackStepsPassed(t *testing.T) {
	cases := []struct {
		age  time.Duration
		want int
	}{
		{0, 0},
		{11 * time.Hour, 0},
		{12 * time.Hour, 1},
		{23 * time.Hour, 1},
		{24 * time.Hour, 2},
		{47 * time.Hour, 2},
		{48 * time.Hour, 3},
		{95 * time.Hour, 3},
		{96 * time.Hour, 4},
		{100000 * time.Hour, maxAutoPingbacks},
	}
	for _, c := range cases {
		if got := autoPingbackStepsPassed(c.age); got != c.want {
			t.Errorf("autoPingbackStepsPassed(%v) = %d, want %d", c.age, got, c.want)
		}
	}
}

// ── sweep behavior ───────────────────────────────────────────────────────

func newPingbackLoopForTest(client BeadClient, now time.Time) *EscalationPingbackLoop {
	l := NewEscalationPingbackLoop(EscalationPingbackLoopConfig{
		Daemon: client,
		Logger: slog.Default(),
	})
	l.now = func() time.Time { return now }
	return l
}

// addOpenEscalation adds an open escalation notified notifiedAgo. It is owner
// audience unless fields say otherwise: the ladder warms only the owner's
// (escalation routing T9), and an untyped escalation's default audience is not
// the owner.
func addOpenEscalation(client *mockEscalationBeadClient, id string, notifiedAgo time.Duration, now time.Time, fields map[string]string) {
	f := map[string]string{
		"notified_at": now.Add(-notifiedAgo).UTC().Format(time.RFC3339),
		"audience":    "owner",
	}
	for k, v := range fields {
		f[k] = v
	}
	client.beads[id] = &beadsapi.BeadDetail{
		ID:        id,
		Type:      "escalation",
		Status:    "open",
		Title:     "escalation " + id,
		Fields:    f,
		CreatedAt: now.Add(-notifiedAgo),
	}
}

func TestPingbackSweep_FreshEscalationUntouched(t *testing.T) {
	now := time.Now()
	client := newMockEscalationBeadClient()
	addOpenEscalation(client, "gd-s1", 2*time.Hour, now, nil)

	newPingbackLoopForTest(client, now).sweepOnce(context.Background())

	if _, ok := client.updatedFields["gd-s1"]; ok {
		t.Errorf("2h-old escalation must not be pinged; got %v", client.updatedFields["gd-s1"])
	}
}

func TestPingbackSweep_FirstStepAt12h(t *testing.T) {
	now := time.Now()
	client := newMockEscalationBeadClient()
	addOpenEscalation(client, "gd-s2", 13*time.Hour, now, nil)

	newPingbackLoopForTest(client, now).sweepOnce(context.Background())

	got := client.updatedFields["gd-s2"]
	if got == nil {
		t.Fatal("13h-old escalation was not pinged")
	}
	if got["auto_pingback_count"] != "1" {
		t.Errorf("auto_pingback_count = %q, want \"1\"", got["auto_pingback_count"])
	}
	if got["pingback_requested_at"] == "" {
		t.Error("pingback_requested_at not stamped")
	}
	if got["pingback_source"] != "auto" {
		t.Errorf("pingback_source = %q, want auto", got["pingback_source"])
	}
	if got["pingback_count"] != "1" {
		t.Errorf("pingback_count = %q, want \"1\"", got["pingback_count"])
	}
	if _, ok := got["queue_alert_requested_at"]; ok {
		t.Error("queue alert must not fire at 13h")
	}
}

func TestPingbackSweep_NoRefireWhenStepAlreadyTaken(t *testing.T) {
	now := time.Now()
	client := newMockEscalationBeadClient()
	addOpenEscalation(client, "gd-s3", 13*time.Hour, now, map[string]string{
		"auto_pingback_count": "1",
		"pingback_count":      "1",
	})

	newPingbackLoopForTest(client, now).sweepOnce(context.Background())

	if _, ok := client.updatedFields["gd-s3"]; ok {
		t.Errorf("step 1 already taken at 13h; must not re-fire: %v", client.updatedFields["gd-s3"])
	}
}

func TestPingbackSweep_SecondStepAt24h(t *testing.T) {
	now := time.Now()
	client := newMockEscalationBeadClient()
	addOpenEscalation(client, "gd-s4", 25*time.Hour, now, map[string]string{
		"auto_pingback_count": "1",
		"pingback_count":      "3", // 1 auto + 2 manual
	})

	newPingbackLoopForTest(client, now).sweepOnce(context.Background())

	got := client.updatedFields["gd-s4"]
	if got == nil {
		t.Fatal("25h-old escalation at step 1 was not advanced")
	}
	if got["auto_pingback_count"] != "2" {
		t.Errorf("auto_pingback_count = %q, want \"2\"", got["auto_pingback_count"])
	}
	if got["pingback_count"] != "4" {
		t.Errorf("pingback_count = %q, want \"4\" (3 + 1)", got["pingback_count"])
	}
}

func TestPingbackSweep_QueueAlertAt48h(t *testing.T) {
	now := time.Now()
	client := newMockEscalationBeadClient()
	addOpenEscalation(client, "gd-s5", 49*time.Hour, now, map[string]string{
		"auto_pingback_count": "2",
		"pingback_count":      "2",
	})

	newPingbackLoopForTest(client, now).sweepOnce(context.Background())

	got := client.updatedFields["gd-s5"]
	if got == nil {
		t.Fatal("49h-old escalation was not advanced")
	}
	if got["auto_pingback_count"] != "3" {
		t.Errorf("auto_pingback_count = %q, want \"3\"", got["auto_pingback_count"])
	}
	if got["queue_alert_requested_at"] == "" {
		t.Error("queue_alert_requested_at must be stamped past 48h")
	}
}

func TestPingbackSweep_QueueAlertOnlyOnce(t *testing.T) {
	now := time.Now()
	client := newMockEscalationBeadClient()
	addOpenEscalation(client, "gd-s6", 97*time.Hour, now, map[string]string{
		"auto_pingback_count":      "3",
		"pingback_count":           "3",
		"queue_alert_requested_at": now.Add(-49 * time.Hour).UTC().Format(time.RFC3339),
	})

	newPingbackLoopForTest(client, now).sweepOnce(context.Background())

	got := client.updatedFields["gd-s6"]
	if got == nil {
		t.Fatal("97h-old escalation was not advanced to step 4")
	}
	if got["auto_pingback_count"] != "4" {
		t.Errorf("auto_pingback_count = %q, want \"4\"", got["auto_pingback_count"])
	}
	if _, ok := got["queue_alert_requested_at"]; ok {
		t.Error("queue_alert_requested_at must not be re-stamped once set")
	}
}

func TestPingbackSweep_CatchUpFiresOneStepNotBurst(t *testing.T) {
	// After bridge downtime a 50h-old untouched escalation jumps straight to
	// step 3 with ONE pingback request, not three.
	now := time.Now()
	client := newMockEscalationBeadClient()
	addOpenEscalation(client, "gd-s7", 50*time.Hour, now, nil)

	newPingbackLoopForTest(client, now).sweepOnce(context.Background())

	got := client.updatedFields["gd-s7"]
	if got == nil {
		t.Fatal("50h-old escalation was not pinged")
	}
	if got["auto_pingback_count"] != "3" {
		t.Errorf("auto_pingback_count = %q, want \"3\" (catch-up)", got["auto_pingback_count"])
	}
	if got["pingback_count"] != "1" {
		t.Errorf("pingback_count = %q, want \"1\" — catch-up must post once, not burst", got["pingback_count"])
	}
	if got["queue_alert_requested_at"] == "" {
		t.Error("queue alert must fire on catch-up past 48h")
	}
}

func TestPingbackSweep_SkipsApproved(t *testing.T) {
	now := time.Now()
	client := newMockEscalationBeadClient()
	addOpenEscalation(client, "gd-s8", 30*time.Hour, now, map[string]string{
		"approved_by": "matthew.baker",
	})

	newPingbackLoopForTest(client, now).sweepOnce(context.Background())

	if _, ok := client.updatedFields["gd-s8"]; ok {
		t.Error("approved escalation must not be pinged")
	}
}

func TestPingbackSweep_SkipsE2EFixtures(t *testing.T) {
	now := time.Now()
	client := newMockEscalationBeadClient()
	addOpenEscalation(client, "gd-s9", 30*time.Hour, now, nil)
	client.beads["gd-s9"].CreatedBy = "e2e-v3-fixture"
	addOpenEscalation(client, "gd-s10", 30*time.Hour, now, nil)
	client.beads["gd-s10"].Labels = []string{"e2e:true"}

	newPingbackLoopForTest(client, now).sweepOnce(context.Background())

	if _, ok := client.updatedFields["gd-s9"]; ok {
		t.Error("e2e-created escalation must not be pinged")
	}
	if _, ok := client.updatedFields["gd-s10"]; ok {
		t.Error("e2e-labeled escalation must not be pinged")
	}
}

func TestPingbackSweep_FallsBackToCreatedAt(t *testing.T) {
	// Escalations that predate notified_at stamping anchor on CreatedAt.
	now := time.Now()
	client := newMockEscalationBeadClient()
	client.beads["gd-s11"] = &beadsapi.BeadDetail{
		ID:        "gd-s11",
		Type:      "escalation",
		Status:    "open",
		Fields:    map[string]string{"audience": "owner"},
		CreatedAt: now.Add(-13 * time.Hour),
	}

	newPingbackLoopForTest(client, now).sweepOnce(context.Background())

	got := client.updatedFields["gd-s11"]
	if got == nil {
		t.Fatal("no-notified_at escalation was not pinged from CreatedAt anchor")
	}
	if got["auto_pingback_count"] != "1" {
		t.Errorf("auto_pingback_count = %q, want \"1\"", got["auto_pingback_count"])
	}
}

// ── watcher pingback handshake ───────────────────────────────────────────

// mockEscalationNotifier records notifier calls for handshake tests.
type mockEscalationNotifier struct {
	mu            sync.Mutex
	pingbacks     []string // bead IDs
	queueAlerts   []string // bead IDs
	resultCalls   []string // bead IDs passed to NotifyEscalationResult (green summary)
	dismissCalls  []string // bead IDs passed to DismissEscalation (red "Rejected" banner)
	approvedCalls []string // bead IDs passed to UpdateEscalationApproved
	offline       []string // bead IDs passed to NotifyEscalationHolderOffline
}

func (m *mockEscalationNotifier) NotifyNewEscalation(context.Context, BeadEvent) error { return nil }
func (m *mockEscalationNotifier) UpdateEscalationApproved(_ context.Context, bead BeadEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.approvedCalls = append(m.approvedCalls, bead.ID)
	return nil
}
func (m *mockEscalationNotifier) NotifyEscalationResult(_ context.Context, bead BeadEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.resultCalls = append(m.resultCalls, bead.ID)
	return nil
}
func (m *mockEscalationNotifier) DismissEscalation(_ context.Context, beadID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dismissCalls = append(m.dismissCalls, beadID)
	return nil
}
func (m *mockEscalationNotifier) NotifyEscalationPingback(_ context.Context, bead BeadEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pingbacks = append(m.pingbacks, bead.ID)
	return nil
}
func (m *mockEscalationNotifier) NotifyEscalationQueueAlert(_ context.Context, bead BeadEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.queueAlerts = append(m.queueAlerts, bead.ID)
	return nil
}
func (m *mockEscalationNotifier) NotifyEscalationHolderOffline(_ context.Context, bead BeadEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.offline = append(m.offline, bead.ID)
	return nil
}

func pingbackUpdateEvent(t *testing.T, bead BeadEvent) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{"bead": bead})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestHandleUpdated_PingbackRequestPostsOnceAndStamps(t *testing.T) {
	client := newMockEscalationBeadClient()
	notifier := &mockEscalationNotifier{}
	esc := NewEscalations(EscalationsConfig{
		Daemon:             client,
		EscalationNotifier: notifier,
		Logger:             slog.Default(),
	})

	requested := "2026-07-18T12:00:00Z"
	bead := BeadEvent{
		ID:     "gd-h1",
		Type:   "escalation",
		Status: "open",
		Fields: map[string]string{
			"pingback_requested_at": requested,
			"pingback_source":       "manual",
			"audience":              "owner", // only the owner's are re-surfaced (T9)
		},
	}
	data := pingbackUpdateEvent(t, bead)

	esc.handleUpdated(context.Background(), data)
	// Redelivered event before the notified_at stamp lands: in-memory guard.
	esc.handleUpdated(context.Background(), data)

	if len(notifier.pingbacks) != 1 {
		t.Fatalf("got %d pingback posts, want 1", len(notifier.pingbacks))
	}
	if got := client.updatedFields["gd-h1"]["pingback_notified_at"]; got != requested {
		t.Errorf("pingback_notified_at = %q, want %q", got, requested)
	}
}

func TestHandleUpdated_PingbackAlreadyNotifiedIsNoop(t *testing.T) {
	client := newMockEscalationBeadClient()
	notifier := &mockEscalationNotifier{}
	esc := NewEscalations(EscalationsConfig{
		Daemon:             client,
		EscalationNotifier: notifier,
		Logger:             slog.Default(),
	})

	ts := "2026-07-18T12:00:00Z"
	bead := BeadEvent{
		ID:     "gd-h2",
		Type:   "escalation",
		Status: "open",
		Fields: map[string]string{
			"pingback_requested_at": ts,
			"pingback_notified_at":  ts,
		},
	}
	esc.handleUpdated(context.Background(), pingbackUpdateEvent(t, bead))

	if len(notifier.pingbacks) != 0 {
		t.Errorf("requested == notified must be a no-op; got %d posts", len(notifier.pingbacks))
	}
}

func TestHandleUpdated_QueueAlertPostsAndStamps(t *testing.T) {
	client := newMockEscalationBeadClient()
	notifier := &mockEscalationNotifier{}
	esc := NewEscalations(EscalationsConfig{
		Daemon:             client,
		EscalationNotifier: notifier,
		Logger:             slog.Default(),
	})

	requested := "2026-07-18T13:00:00Z"
	bead := BeadEvent{
		ID:     "gd-h3",
		Type:   "escalation",
		Status: "open",
		Fields: map[string]string{
			"queue_alert_requested_at": requested,
			"audience":                 "owner", // only the owner's are re-surfaced (T9)
		},
	}
	data := pingbackUpdateEvent(t, bead)
	esc.handleUpdated(context.Background(), data)
	esc.handleUpdated(context.Background(), data)

	if len(notifier.queueAlerts) != 1 {
		t.Fatalf("got %d queue alerts, want 1", len(notifier.queueAlerts))
	}
	if got := client.updatedFields["gd-h3"]["queue_alert_notified_at"]; got != requested {
		t.Errorf("queue_alert_notified_at = %q, want %q", got, requested)
	}
}

func TestHandleUpdated_ApprovedEscalationSkipsPingback(t *testing.T) {
	client := newMockEscalationBeadClient()
	client.beads["gd-h4"] = &beadsapi.BeadDetail{ID: "gd-h4", Type: "escalation", Status: "open"}
	notifier := &mockEscalationNotifier{}
	esc := NewEscalations(EscalationsConfig{
		Daemon:             client,
		EscalationNotifier: notifier,
		Logger:             slog.Default(),
	})

	bead := BeadEvent{
		ID:     "gd-h4",
		Type:   "escalation",
		Status: "open",
		Fields: map[string]string{
			"approved_by":           "matthew.baker",
			"pingback_requested_at": "2026-07-18T14:00:00Z",
		},
	}
	esc.handleUpdated(context.Background(), pingbackUpdateEvent(t, bead))

	if len(notifier.pingbacks) != 0 {
		t.Errorf("approved escalation must not re-post a pingback; got %d", len(notifier.pingbacks))
	}
}

// TestPingbackSweep_ExhaustsPastOnePage is the regression guard for
// ka-0000005723: the sweep must page to exhaustion, not fetch a single capped
// window.
//
// Before the fix the sweep issued one ListBeadsFiltered with Limit:200 and no
// offset walk, so every escalation past the cap was silently skipped. That
// truncation is worst-case harmful here: /v1/beads returns newest-first by
// default, so the rows dropped are the OLDEST — exactly the long-stalled ones
// the backoff ladder exists to keep warm.
//
// The mock holds one more escalation than a single page, each stalled long
// enough to earn a pingback. A capped sweep pings pageSize of them; an
// exhaustive sweep pings all of them.
func TestPingbackSweep_ExhaustsPastOnePage(t *testing.T) {
	now := time.Now()
	client := newMockEscalationBeadClient()

	const total = pingbackSweepPageSize + 1
	for i := range total {
		// 100h stalled → well past the 48h step, so every one is ladder-eligible.
		addOpenEscalation(client, fmt.Sprintf("gd-p%04d", i), 100*time.Hour, now, nil)
	}

	newPingbackLoopForTest(client, now).sweepOnce(context.Background())

	if got := len(client.updatedFields); got != total {
		t.Errorf("sweep pinged %d escalations, want all %d — a single capped fetch "+
			"stops at %d and silently drops the tail", got, total, pingbackSweepPageSize)
	}
	// The bead that only a second page can reach must have been evaluated.
	last := fmt.Sprintf("gd-p%04d", total-1)
	if _, ok := client.updatedFields[last]; !ok {
		t.Errorf("escalation %s (past the first page boundary) was never evaluated", last)
	}
}

// TestPingbackSweep_PagingDoesNotDoubleEvaluate guards the walk's own
// bookkeeping. Paging introduces a hazard the single capped fetch did not have:
// a bead can appear on two pages when the underlying set shifts, and evaluating
// it twice in one sweep would advance the ladder two steps and double-post to
// Slack — the exact spam the backoff exists to prevent. The `seen` set in
// sweepOnce is what stops that.
//
// The set spans a page boundary so the multi-page path is the one under test.
// A 100h stall has crossed four ladder rungs (12h, 24h, 48h, 96h), so a
// correctly-once-evaluated bead lands on step 4.
func TestPingbackSweep_PagingDoesNotDoubleEvaluate(t *testing.T) {
	now := time.Now()
	client := newMockEscalationBeadClient()

	const total = pingbackSweepPageSize + 1
	for i := range total {
		addOpenEscalation(client, fmt.Sprintf("gd-d%04d", i), 100*time.Hour, now, nil)
	}

	newPingbackLoopForTest(client, now).sweepOnce(context.Background())

	// Check both a first-page bead and the one only the second page reaches.
	for _, id := range []string{"gd-d0000", fmt.Sprintf("gd-d%04d", total-1)} {
		f := client.updatedFields[id]
		if f == nil {
			t.Errorf("%s was never pinged", id)
			continue
		}
		if got := f["auto_pingback_count"]; got != "4" {
			t.Errorf("%s auto_pingback_count = %q, want %q — a bead evaluated twice "+
				"in one sweep advances the ladder twice", id, got, "4")
		}
	}
}

// In-hand escalations are a coordinator's queue: the ladder warms only the
// owner's (#756 audience). The owner-audience escalation in the same sweep is
// the positive control.
func TestPingbackSweep_SkipsInHand(t *testing.T) {
	now := time.Now()
	client := newMockEscalationBeadClient()
	addOpenEscalation(client, "gd-hb1", 30*time.Hour, now, map[string]string{"held_by": "coordinator:laptop", "audience": "operator"})
	addOpenEscalation(client, "gd-hb2", 30*time.Hour, now, map[string]string{"audience": "owner"})

	newPingbackLoopForTest(client, now).sweepOnce(context.Background())

	if _, ok := client.updatedFields["gd-hb1"]; ok {
		t.Error("coordinator-held escalation was pinged")
	}
	if _, ok := client.updatedFields["gd-hb2"]; !ok {
		t.Error("owner-audience escalation was not pinged")
	}
}

// After a hand-up the ladder restarts from handed_up_at, so the operator gets
// the hand-up's own pingback, not an immediate catch-up burst for the hours a
// coordinator held it.
func TestPingbackAnchor_PrefersLaterHandUp(t *testing.T) {
	created := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	f := map[string]string{"notified_at": "2026-10-19T00:00:00Z", "handed_up_at": "2026-10-20T05:00:00Z"}
	if got := pingbackAnchor(f, created); !got.Equal(time.Date(2026, 10, 20, 5, 0, 0, 0, time.UTC)) {
		t.Errorf("anchor = %v, want handed_up_at", got)
	}
}
