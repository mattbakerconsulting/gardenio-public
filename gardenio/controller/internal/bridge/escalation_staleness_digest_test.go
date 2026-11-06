package bridge

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// mockDigestNotifier records each PostStaleEscalationDigest call.
type mockDigestNotifier struct {
	mu        sync.Mutex
	calls     [][]StaleEscalationDigestEntry
	overflows []int
	err       error
}

func (m *mockDigestNotifier) PostStaleEscalationDigest(_ context.Context, entries []StaleEscalationDigestEntry, overflow int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	cp := append([]StaleEscalationDigestEntry(nil), entries...)
	m.calls = append(m.calls, cp)
	m.overflows = append(m.overflows, overflow)
	return nil
}

func (m *mockDigestNotifier) lastCall() ([]StaleEscalationDigestEntry, int, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.calls) == 0 {
		return nil, 0, false
	}
	return m.calls[len(m.calls)-1], m.overflows[len(m.overflows)-1], true
}

func newDigestLoopForTest(client BeadClient, notifier StaleDigestNotifier, now time.Time) *EscalationStalenessDigestLoop {
	l := NewEscalationStalenessDigestLoop(EscalationStalenessDigestConfig{
		Daemon:   client,
		Notifier: notifier,
		Logger:   slog.Default(),
	})
	l.now = func() time.Time { return now }
	return l
}

// addStaleEscalation seeds an open escalation of the given priority whose stall
// clock (notified_at + created_at) sits `stalledAgo` in the past.
func addStaleEscalation(client *mockEscalationBeadClient, id string, priority int, stalledAgo time.Duration, now time.Time, fields map[string]string) {
	addOpenEscalation(client, id, stalledAgo, now, fields)
	client.beads[id].Priority = priority
}

// ── threshold logic ────────────────────────────────────────────────────────

func TestDigestStaleThreshold(t *testing.T) {
	l := newDigestLoopForTest(newMockEscalationBeadClient(), &mockDigestNotifier{}, time.Now())
	cases := []struct {
		priority int
		want     time.Duration
		ok       bool
	}{
		{0, defaultP0StaleThreshold, true},
		{1, defaultP1StaleThreshold, true},
		{2, 0, false},
		{3, 0, false},
	}
	for _, c := range cases {
		got, ok := l.staleThreshold(c.priority)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("staleThreshold(P%d) = (%v,%v), want (%v,%v)", c.priority, got, ok, c.want, c.ok)
		}
	}
}

func TestNewDigestLoop_DisabledWithoutDeps(t *testing.T) {
	if l := NewEscalationStalenessDigestLoop(EscalationStalenessDigestConfig{Notifier: &mockDigestNotifier{}}); l != nil {
		t.Error("nil Daemon must disable the loop (return nil)")
	}
	if l := NewEscalationStalenessDigestLoop(EscalationStalenessDigestConfig{Daemon: newMockEscalationBeadClient()}); l != nil {
		t.Error("nil Notifier must disable the loop (return nil)")
	}
	// Run on a nil loop must not panic.
	var nilLoop *EscalationStalenessDigestLoop
	nilLoop.Run(context.Background())
}

// ── sweep inclusion / exclusion ────────────────────────────────────────────

func TestDigestSweep_ThresholdsByPriority(t *testing.T) {
	now := time.Now()
	client := newMockEscalationBeadClient()
	addStaleEscalation(client, "gd-p0-fresh", 0, 1*time.Hour, now, nil)  // < 2h → out
	addStaleEscalation(client, "gd-p0-stale", 0, 3*time.Hour, now, nil)  // > 2h → in
	addStaleEscalation(client, "gd-p1-fresh", 1, 5*time.Hour, now, nil)  // < 8h → out
	addStaleEscalation(client, "gd-p1-stale", 1, 9*time.Hour, now, nil)  // > 8h → in
	addStaleEscalation(client, "gd-p2-old", 2, 99*time.Hour, now, nil)   // P2 → never

	notifier := &mockDigestNotifier{}
	newDigestLoopForTest(client, notifier, now).sweepOnce(context.Background())

	entries, _, ok := notifier.lastCall()
	if !ok {
		t.Fatal("expected a digest to be posted")
	}
	got := map[string]bool{}
	for _, e := range entries {
		got[e.ID] = true
	}
	wantIn := []string{"gd-p0-stale", "gd-p1-stale"}
	wantOut := []string{"gd-p0-fresh", "gd-p1-fresh", "gd-p2-old"}
	for _, id := range wantIn {
		if !got[id] {
			t.Errorf("%s should be in the digest", id)
		}
	}
	for _, id := range wantOut {
		if got[id] {
			t.Errorf("%s should NOT be in the digest", id)
		}
	}
	// Included escalations must be stamped; excluded must not.
	for _, id := range wantIn {
		if client.updatedFields[id]["last_digested_at"] == "" {
			t.Errorf("%s not stamped with last_digested_at", id)
		}
	}
	for _, id := range wantOut {
		if _, stamped := client.updatedFields[id]; stamped {
			t.Errorf("%s must not be stamped", id)
		}
	}
}

func TestDigestSweep_SkipsApprovedReviewFlaggedAndE2E(t *testing.T) {
	now := time.Now()
	client := newMockEscalationBeadClient()
	addStaleEscalation(client, "gd-approved", 0, 5*time.Hour, now, map[string]string{"approved_by": "oidc:matt"})
	addStaleEscalation(client, "gd-mxdwvqtrlg", 0, 5*time.Hour, now, map[string]string{mrLandedField: "true"})
	addStaleEscalation(client, "gd-e2e", 0, 5*time.Hour, now, nil)
	client.beads["gd-e2e"].CreatedBy = "e2e-agent-v2"
	addStaleEscalation(client, "gd-real", 0, 5*time.Hour, now, nil)

	notifier := &mockDigestNotifier{}
	newDigestLoopForTest(client, notifier, now).sweepOnce(context.Background())

	entries, _, ok := notifier.lastCall()
	if !ok {
		t.Fatal("expected a digest with the one real escalation")
	}
	if len(entries) != 1 || entries[0].ID != "gd-real" {
		t.Fatalf("digest = %v, want only gd-real", entries)
	}
}

func TestDigestSweep_NoDuePostsNothing(t *testing.T) {
	now := time.Now()
	client := newMockEscalationBeadClient()
	addStaleEscalation(client, "gd-fresh", 0, 30*time.Minute, now, nil)

	notifier := &mockDigestNotifier{}
	newDigestLoopForTest(client, notifier, now).sweepOnce(context.Background())

	if _, _, ok := notifier.lastCall(); ok {
		t.Error("no stale escalations → no digest should be posted")
	}
}

// ── restart-durable per-bead re-warm dedup ─────────────────────────────────

func TestDigestSweep_ReWarmDedup(t *testing.T) {
	now := time.Now()
	client := newMockEscalationBeadClient()
	// Digested 1h ago (< 6h re-warm) → skipped.
	addStaleEscalation(client, "gd-recent", 0, 5*time.Hour, now, map[string]string{
		"last_digested_at": now.Add(-1 * time.Hour).UTC().Format(time.RFC3339),
	})
	// Digested 7h ago (> 6h re-warm) → re-appears.
	addStaleEscalation(client, "gd-cold", 0, 5*time.Hour, now, map[string]string{
		"last_digested_at": now.Add(-7 * time.Hour).UTC().Format(time.RFC3339),
	})

	notifier := &mockDigestNotifier{}
	newDigestLoopForTest(client, notifier, now).sweepOnce(context.Background())

	entries, _, ok := notifier.lastCall()
	if !ok {
		t.Fatal("expected a digest containing the re-warm-due escalation")
	}
	if len(entries) != 1 || entries[0].ID != "gd-cold" {
		t.Fatalf("digest = %v, want only gd-cold (gd-recent still inside re-warm window)", entries)
	}
}

// ── post failure must not stamp (retried next sweep) ───────────────────────

func TestDigestSweep_PostFailureLeavesUnstamped(t *testing.T) {
	now := time.Now()
	client := newMockEscalationBeadClient()
	addStaleEscalation(client, "gd-x", 0, 5*time.Hour, now, nil)

	notifier := &mockDigestNotifier{err: errors.New("slack down")}
	newDigestLoopForTest(client, notifier, now).sweepOnce(context.Background())

	if _, stamped := client.updatedFields["gd-x"]; stamped {
		t.Error("post failure must NOT stamp last_digested_at (so the next sweep retries)")
	}
}

// ── overflow cap ───────────────────────────────────────────────────────────

func TestDigestSweep_OverflowCapAndOrdering(t *testing.T) {
	now := time.Now()
	client := newMockEscalationBeadClient()
	// One very old P1 plus more than digestMaxEntries P0s.
	addStaleEscalation(client, "gd-p1old", 1, 40*time.Hour, now, nil)
	total := digestMaxEntries + 5
	for i := 0; i < total; i++ {
		id := "gd-p0-" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		addStaleEscalation(client, id, 0, time.Duration(3+i)*time.Hour, now, nil)
	}

	notifier := &mockDigestNotifier{}
	newDigestLoopForTest(client, notifier, now).sweepOnce(context.Background())

	entries, overflow, ok := notifier.lastCall()
	if !ok {
		t.Fatal("expected a digest")
	}
	if len(entries) != digestMaxEntries {
		t.Errorf("digest listed %d entries, want cap %d", len(entries), digestMaxEntries)
	}
	// P0 sorts before P1, so the capped list is all P0 and the P1 is dropped.
	if entries[0].Priority != 0 {
		t.Errorf("digest head priority = P%d, want P0 (P0 before P1)", entries[0].Priority)
	}
	// overflow = (total P0 + 1 P1) - cap.
	wantOverflow := (total + 1) - digestMaxEntries
	if overflow != wantOverflow {
		t.Errorf("overflow = %d, want %d", overflow, wantOverflow)
	}
	// The dropped tail must be left unstamped so it surfaces next sweep.
	if _, stamped := client.updatedFields["gd-p1old"]; stamped {
		t.Error("overflowed P1 must not be stamped (it was not listed)")
	}
}

// ── age is measured from the stall anchor ──────────────────────────────────

func TestDigestSweep_AgeFromAnchor(t *testing.T) {
	now := time.Now()
	client := newMockEscalationBeadClient()
	addStaleEscalation(client, "gd-a", 0, 4*time.Hour, now, nil)

	notifier := &mockDigestNotifier{}
	newDigestLoopForTest(client, notifier, now).sweepOnce(context.Background())

	entries, _, ok := notifier.lastCall()
	if !ok || len(entries) != 1 {
		t.Fatalf("expected one entry, got %v", entries)
	}
	if d := entries[0].Age - 4*time.Hour; d < -time.Minute || d > time.Minute {
		t.Errorf("entry age = %v, want ~4h", entries[0].Age)
	}
}

// compile-time: mockDigestNotifier satisfies the interface.
var _ StaleDigestNotifier = (*mockDigestNotifier)(nil)

// silence unused import if beadsapi drifts out of this file.
var _ = beadsapi.BeadDetail{}
