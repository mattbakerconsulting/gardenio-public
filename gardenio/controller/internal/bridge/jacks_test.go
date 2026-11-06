package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// mockJackNotifier records calls to jack notification methods.
type mockJackNotifier struct {
	mu          sync.Mutex
	raised      []BeadEvent
	batched     [][]BeadEvent
	batchCtxErr error // ctx.Err() observed on the most recent batch call
	lowered     []BeadEvent
	expired     []BeadEvent
}

func (m *mockJackNotifier) NotifyJackOn(_ context.Context, bead BeadEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.raised = append(m.raised, bead)
	return nil
}

func (m *mockJackNotifier) NotifyJackOnBatch(ctx context.Context, beads []BeadEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.batched = append(m.batched, beads)
	m.batchCtxErr = ctx.Err()
	return nil
}

func (m *mockJackNotifier) NotifyJackOff(_ context.Context, bead BeadEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lowered = append(m.lowered, bead)
	return nil
}

func (m *mockJackNotifier) NotifyJackExpired(_ context.Context, bead BeadEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expired = append(m.expired, bead)
	return nil
}

func (m *mockJackNotifier) getBatched() [][]BeadEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([][]BeadEvent{}, m.batched...)
}

func (m *mockJackNotifier) getBatchCtxErr() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.batchCtxErr
}

func (m *mockJackNotifier) getRaised() []BeadEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]BeadEvent{}, m.raised...)
}

func (m *mockJackNotifier) getLowered() []BeadEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]BeadEvent{}, m.lowered...)
}

func (m *mockJackNotifier) getExpired() []BeadEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]BeadEvent{}, m.expired...)
}

func TestJacks_HandleCreated_RaisesNotification(t *testing.T) {
	notif := &mockJackNotifier{}
	j := NewJacks(JacksConfig{
		Notifier: notif,
		Logger:   slog.Default(),
	})

	// Non-jack bead should be ignored.
	nonJack := marshalSSEBeadPayload(BeadEvent{
		ID:   "dec-1",
		Type: "decision",
	})
	j.handleCreated(context.Background(), nonJack)
	if len(notif.getRaised()) != 0 {
		t.Fatal("non-jack bead should not trigger notification")
	}

	// Jack bead should trigger notification.
	jack := marshalSSEBeadPayload(BeadEvent{
		ID:       "hq-abc123",
		Type:     "jack",
		Assignee: "gardenio/crew/ops",
		Fields: map[string]string{
			"target": "pod/bd-daemon-xyz",
			"ttl":    "30m",
			"reason": "Debug logging for NATS timeout",
		},
	})
	j.handleCreated(context.Background(), jack)

	raised := notif.getRaised()
	if len(raised) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(raised))
	}
	if raised[0].ID != "hq-abc123" {
		t.Errorf("expected jack ID hq-abc123, got %s", raised[0].ID)
	}
	if raised[0].Fields["target"] != "pod/bd-daemon-xyz" {
		t.Errorf("expected target pod/bd-daemon-xyz, got %s", raised[0].Fields["target"])
	}
}

func TestJacks_HandleClosed_LowersNotification(t *testing.T) {
	notif := &mockJackNotifier{}
	j := NewJacks(JacksConfig{
		Notifier: notif,
		Logger:   slog.Default(),
	})

	jack := marshalSSEBeadPayload(BeadEvent{
		ID:       "hq-def456",
		Type:     "jack",
		Assignee: "gardenio/crew/ops",
		Fields: map[string]string{
			"target": "deployment/api",
			"reason": "Fix verified, reverting",
		},
	})
	j.handleClosed(context.Background(), jack)

	lowered := notif.getLowered()
	if len(lowered) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(lowered))
	}
	if lowered[0].ID != "hq-def456" {
		t.Errorf("expected jack ID hq-def456, got %s", lowered[0].ID)
	}
}

func TestJacks_HandleClosed_Dedup(t *testing.T) {
	notif := &mockJackNotifier{}
	j := NewJacks(JacksConfig{
		Notifier: notif,
		Logger:   slog.Default(),
	})

	jack := marshalSSEBeadPayload(BeadEvent{
		ID:   "hq-dedup1",
		Type: "jack",
		Fields: map[string]string{
			"target": "pod/test",
		},
	})

	// First call: should notify.
	j.handleClosed(context.Background(), jack)
	// Second call: should be deduplicated.
	j.handleClosed(context.Background(), jack)

	lowered := notif.getLowered()
	if len(lowered) != 1 {
		t.Fatalf("expected exactly 1 notification (dedup), got %d", len(lowered))
	}
}

func TestJacks_HandleUpdated_Expired(t *testing.T) {
	notif := &mockJackNotifier{}
	j := NewJacks(JacksConfig{
		Notifier: notif,
		Logger:   slog.Default(),
	})

	// Jack without expired label should be ignored.
	normalUpdate := marshalSSEBeadPayload(BeadEvent{
		ID:   "hq-normal",
		Type: "jack",
		Fields: map[string]string{
			"target": "pod/test",
		},
	})
	j.handleUpdated(context.Background(), normalUpdate)
	if len(notif.getExpired()) != 0 {
		t.Fatal("jack without expired label should not trigger notification")
	}

	// Jack with expired label should trigger notification.
	expiredJack := marshalSSEBeadPayload(BeadEvent{
		ID:     "hq-expired1",
		Type:   "jack",
		Labels: []string{"expired"},
		Fields: map[string]string{
			"target": "pod/bd-daemon-abc",
			"reason": "Debug logging",
		},
	})
	j.handleUpdated(context.Background(), expiredJack)

	expired := notif.getExpired()
	if len(expired) != 1 {
		t.Fatalf("expected 1 expired notification, got %d", len(expired))
	}
	if expired[0].ID != "hq-expired1" {
		t.Errorf("expected jack ID hq-expired1, got %s", expired[0].ID)
	}
}

func TestJacks_HandleUpdated_ExpiredDedup(t *testing.T) {
	notif := &mockJackNotifier{}
	j := NewJacks(JacksConfig{
		Notifier: notif,
		Logger:   slog.Default(),
	})

	expiredJack := marshalSSEBeadPayload(BeadEvent{
		ID:     "hq-expired2",
		Type:   "jack",
		Labels: []string{"expired"},
		Fields: map[string]string{
			"target": "pod/test",
		},
	})

	// First call: should notify.
	j.handleUpdated(context.Background(), expiredJack)
	// Second call within 6h: should be deduplicated.
	j.handleUpdated(context.Background(), expiredJack)

	expired := notif.getExpired()
	if len(expired) != 1 {
		t.Fatalf("expected exactly 1 expired notification (dedup), got %d", len(expired))
	}
}

// TestJacks_DedupMapsBounded proves the per-jack dedup maps do not grow for the
// process lifetime: distinct jack IDs accumulate entries, but a gc sweep past
// the TTL horizon evicts them. Before the fix (ka-0000004753) offSeen/expiredSeen
// were raw maps with no delete/Run, leaking one entry per jack ever seen on the
// always-on bridge process.
func TestJacks_DedupMapsBounded(t *testing.T) {
	j := NewJacks(JacksConfig{
		Notifier: &mockJackNotifier{},
		Logger:   slog.Default(),
	})

	for i := 0; i < 500; i++ {
		id := fmt.Sprintf("hq-bound-%d", i)
		j.handleClosed(context.Background(), marshalSSEBeadPayload(BeadEvent{
			ID: id, Type: "jack", Fields: map[string]string{"target": "pod/x"},
		}))
		j.handleUpdated(context.Background(), marshalSSEBeadPayload(BeadEvent{
			ID: id, Type: "jack", Labels: []string{"expired"},
			Fields: map[string]string{"target": "pod/x"},
		}))
	}
	if got := j.offSeen.len(); got != 500 {
		t.Fatalf("offSeen: expected 500 tracked entries, got %d", got)
	}
	if got := j.expiredSeen.len(); got != 500 {
		t.Fatalf("expiredSeen: expected 500 tracked entries, got %d", got)
	}

	// Age every entry past its TTL, then sweep — both maps must drain to empty,
	// demonstrating the leak is bounded (this is exactly what Run() calls).
	past := time.Now().Add(-2 * seenTTL)
	j.offSeen.mu.Lock()
	for k := range j.offSeen.at {
		j.offSeen.at[k] = past
	}
	j.offSeen.mu.Unlock()
	j.expiredSeen.mu.Lock()
	for k := range j.expiredSeen.at {
		j.expiredSeen.at[k] = past
	}
	j.expiredSeen.mu.Unlock()

	j.offSeen.gc()
	j.expiredSeen.gc()

	if got := j.offSeen.len(); got != 0 {
		t.Fatalf("offSeen: expected 0 entries after TTL sweep, got %d", got)
	}
	if got := j.expiredSeen.len(); got != 0 {
		t.Fatalf("expiredSeen: expected 0 entries after TTL sweep, got %d", got)
	}
}

// TestJacks_ExpiredRenotifiesAfterTTL proves the 6h re-notify semantics survive
// the seenSet migration: an expired jack re-announced after its TTL window
// notifies a second time (whereas a repeat within the window is deduped, covered
// by TestJacks_HandleUpdated_ExpiredDedup).
func TestJacks_ExpiredRenotifiesAfterTTL(t *testing.T) {
	notif := &mockJackNotifier{}
	j := NewJacks(JacksConfig{Notifier: notif, Logger: slog.Default()})

	expiredJack := marshalSSEBeadPayload(BeadEvent{
		ID: "hq-renotify", Type: "jack", Labels: []string{"expired"},
		Fields: map[string]string{"target": "pod/test"},
	})

	j.handleUpdated(context.Background(), expiredJack)
	// Age the entry past the 6h window so the next observation re-notifies.
	j.expiredSeen.mu.Lock()
	for k := range j.expiredSeen.at {
		j.expiredSeen.at[k] = time.Now().Add(-2 * jackExpiredRenotifyTTL)
	}
	j.expiredSeen.mu.Unlock()
	j.handleUpdated(context.Background(), expiredJack)

	if got := len(notif.getExpired()); got != 2 {
		t.Fatalf("expected 2 expired notifications after TTL elapsed, got %d", got)
	}
}

func TestJacks_NilNotifier(t *testing.T) {
	j := NewJacks(JacksConfig{
		Notifier: nil,
		Logger:   slog.Default(),
	})

	jack := marshalSSEBeadPayload(BeadEvent{
		ID:   "hq-nil",
		Type: "jack",
		Fields: map[string]string{
			"target": "pod/test",
		},
	})

	// Should not panic with nil notifier.
	j.handleCreated(context.Background(), jack)
	j.handleClosed(context.Background(), jack)

	expiredJack := marshalSSEBeadPayload(BeadEvent{
		ID:     "hq-nil2",
		Type:   "jack",
		Labels: []string{"expired"},
		Fields: map[string]string{
			"target": "pod/test",
		},
	})
	j.handleUpdated(context.Background(), expiredJack)
}

// TestJacks_FlushBatch_DetachesSSECtx is the regression test for gd-CIZA6CMxmN.
// The batch timer must fire the overflow summary on a context detached from the
// SSE handler ctx. It fault-injects the SSE lifecycle — cancel the handler ctx
// the instant handleCreated returns (as the sse.go `defer cancel()` does) — then
// drives flushBatch and asserts NotifyJackOnBatch runs on a LIVE context. On main
// the cached SSE ctx is cancelled by then, so this fails with context.Canceled.
func TestJacks_FlushBatch_DetachesSSECtx(t *testing.T) {
	notif := &mockJackNotifier{}
	j := NewJacks(JacksConfig{
		Notifier: notif,
		Logger:   slog.Default(),
	})

	// Simulate the SSE dispatcher: a per-handler ctx cancelled as soon as the
	// handler returns. Raise batchThreshold+1 jacks to trip overflow and arm
	// the batch timer, then cancel to mimic `defer cancel()`.
	ctx, cancel := context.WithCancel(context.Background())
	for i := 0; i <= batchThreshold; i++ {
		jack := marshalSSEBeadPayload(BeadEvent{
			ID:       "hq-batch-" + string(rune('a'+i)),
			Type:     "jack",
			Assignee: "gardenio/crew/ops",
			Fields:   map[string]string{"target": "pod/x"},
		})
		j.handleCreated(ctx, jack)
	}
	cancel()

	// Prevent the real AfterFunc from firing during/after the test.
	j.batchMu.Lock()
	if j.batchTimer != nil {
		j.batchTimer.Stop()
	}
	j.batchMu.Unlock()

	// Drive the flush directly (as the timer would).
	j.flushBatch()

	batched := notif.getBatched()
	if len(batched) != 1 {
		t.Fatalf("expected 1 batch summary, got %d", len(batched))
	}
	if len(batched[0]) != 1 {
		t.Errorf("expected 1 overflow jack, got %d", len(batched[0]))
	}
	if err := notif.getBatchCtxErr(); err != nil {
		t.Errorf("NotifyJackOnBatch ran on a cancelled context: %v (want live ctx)", err)
	}
}
