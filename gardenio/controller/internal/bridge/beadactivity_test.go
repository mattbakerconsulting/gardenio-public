package bridge

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// mockBeadActivityNotifier records which notification methods were called.
type mockBeadActivityNotifier struct {
	mu             sync.Mutex
	created        []BeadEvent
	claimed        []BeadEvent
	createdClaimed []BeadEvent
	closed         []BeadEvent
	// createdCtxErr records ctx.Err() observed by NotifyBeadCreated at call
	// time — the deferred (AfterFunc) create path must NOT receive a
	// cancelled context (see TestBeadActivity_CreateWithoutClaim_ContextNotCancelled).
	createdCtxErr []error
}

func (m *mockBeadActivityNotifier) NotifyBeadCreated(ctx context.Context, bead BeadEvent) {
	m.mu.Lock()
	m.created = append(m.created, bead)
	m.createdCtxErr = append(m.createdCtxErr, ctx.Err())
	m.mu.Unlock()
}

func (m *mockBeadActivityNotifier) NotifyBeadClaimed(_ context.Context, bead BeadEvent) {
	m.mu.Lock()
	m.claimed = append(m.claimed, bead)
	m.mu.Unlock()
}

func (m *mockBeadActivityNotifier) NotifyBeadCreatedAndClaimed(_ context.Context, bead BeadEvent) {
	m.mu.Lock()
	m.createdClaimed = append(m.createdClaimed, bead)
	m.mu.Unlock()
}

func (m *mockBeadActivityNotifier) NotifyBeadClosed(_ context.Context, bead BeadEvent) {
	m.mu.Lock()
	m.closed = append(m.closed, bead)
	m.mu.Unlock()
}

func (m *mockBeadActivityNotifier) getCreated() []BeadEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]BeadEvent{}, m.created...)
}

func (m *mockBeadActivityNotifier) getCreatedCtxErrs() []error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]error{}, m.createdCtxErr...)
}

func (m *mockBeadActivityNotifier) getClaimed() []BeadEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]BeadEvent{}, m.claimed...)
}

func (m *mockBeadActivityNotifier) getCreatedClaimed() []BeadEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]BeadEvent{}, m.createdClaimed...)
}

func (m *mockBeadActivityNotifier) getClosed() []BeadEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]BeadEvent{}, m.closed...)
}

// testCreateClaimWindow replaces the 3s production create/claim merge window.
// The five tests here each wait window+500ms, so the production value cost
// 17.5s -- 22% of this package's 78s runtime, and this package bounds the
// controller `test` step's wall clock (go test already runs packages at -p=4,
// so the slowest single package is the floor).
//
// 500ms keeps a wide margin: the merge only requires handleUpdated to be
// reached before the window expires, which takes microseconds here, and the
// events are delivered in-process. The +500ms slack on each wait below is
// deliberately left untouched, so the tolerance for a slow AfterFunc is
// exactly what it was before.
//
// FOUR of the five use this. The fifth deliberately does not -- see
// newProdWindowBeadActivity.
const testCreateClaimWindow = 500 * time.Millisecond

func newTestBeadActivity(notifier *mockBeadActivityNotifier) *BeadActivity {
	return NewBeadActivity(BeadActivityConfig{
		Notifier:          notifier,
		Logger:            slog.Default(),
		CreateClaimWindow: testCreateClaimWindow,
	})
}

// newProdWindowBeadActivity omits CreateClaimWindow, so the instance runs on
// the real defaultCreateClaimWindow. Exactly one test uses it, on purpose.
//
// Shortening all five waits would have banked ~3s more and left a hole that
// nothing green can show you (caught by fimba-a6): with every test at 500ms,
// no test exercises the production duration at all, so a bug whose margin sits
// between 500ms and 3s -- boundary arithmetic on the window, a timer that fires
// systematically late -- would pass the suite. `-count=5` does not help; it
// re-runs the shortened path five times.
//
// Keeping one test on the default also makes this the only coverage of the
// zero-value branch in NewBeadActivity: if the `window <= 0` fallback were ever
// dropped, every other test would still pass, because they all pass an explicit
// window. This one would wait 3.5s for a notification fired at ~0s and still
// see it -- so it does NOT catch that alone, which is why it asserts the
// effective window directly below rather than relying on the sleep.
func newProdWindowBeadActivity(notifier *mockBeadActivityNotifier) *BeadActivity {
	return NewBeadActivity(BeadActivityConfig{
		Notifier: notifier,
		Logger:   slog.Default(),
	})
}

func TestBeadActivity_CreateThenClaim_MergedNotification(t *testing.T) {
	mock := &mockBeadActivityNotifier{}
	ba := newTestBeadActivity(mock)

	created := marshalSSEBeadPayload(BeadEvent{
		ID:        "gd-merge1",
		Type:      "task",
		Title:     "Test task",
		CreatedBy: "test-agent",
	})
	ba.handleCreated(context.Background(), created)

	// Claim within the window.
	claimed := marshalSSEBeadPayload(BeadEvent{
		ID:       "gd-merge1",
		Type:     "task",
		Title:    "Test task",
		Status:   "in_progress",
		Assignee: "test-agent",
	})
	ba.handleUpdated(context.Background(), claimed)

	// Wait for any pending timers to fire.
	time.Sleep(testCreateClaimWindow + 500*time.Millisecond)

	if got := mock.getCreated(); len(got) != 0 {
		t.Errorf("expected 0 separate created notifications, got %d", len(got))
	}
	if got := mock.getClaimed(); len(got) != 0 {
		t.Errorf("expected 0 separate claimed notifications, got %d", len(got))
	}
	if got := mock.getCreatedClaimed(); len(got) != 1 {
		t.Fatalf("expected 1 merged created+claimed notification, got %d", len(got))
	}
	if got := mock.getCreatedClaimed()[0].ID; got != "gd-merge1" {
		t.Errorf("expected bead ID gd-merge1, got %s", got)
	}
}

// TestBeadActivity_CreateWithoutClaim_SeparateNotification is the one test
// left running at the real production window; see newProdWindowBeadActivity
// for why one has to be. It is the right one to keep slow: it is the only path
// where the window's DURATION is what is under test -- the AfterFunc has to
// actually fire, on a detached context, after the full wait -- so a timer that
// fires late or a mis-scaled window shows up here and nowhere else.
//
// Cost: 3.5s rather than 1.0s. That gives up ~3s of the 17.5s saving and keeps
// the remaining ~14.5s.
func TestBeadActivity_CreateWithoutClaim_SeparateNotification(t *testing.T) {
	mock := &mockBeadActivityNotifier{}
	ba := newProdWindowBeadActivity(mock)

	// Assert the zero-value fallback resolved to the production constant. The
	// sleep below cannot catch a broken fallback on its own (a window of 0
	// fires immediately and the notification is still there 3.5s later), so
	// check the effective value directly.
	if ba.createClaimWindow != defaultCreateClaimWindow {
		t.Fatalf("zero CreateClaimWindow should fall back to %v, got %v",
			defaultCreateClaimWindow, ba.createClaimWindow)
	}

	created := marshalSSEBeadPayload(BeadEvent{
		ID:        "gd-noclaim",
		Type:      "task",
		Title:     "Unclaimed task",
		CreatedBy: "test-agent",
	})
	ba.handleCreated(context.Background(), created)

	// Nothing may be delivered before the window elapses. Sampling here is what
	// makes this test sensitive to a window that is too SHORT; the wait below
	// only proves one that is not too long.
	time.Sleep(defaultCreateClaimWindow / 2)
	if got := mock.getCreated(); len(got) != 0 {
		t.Fatalf("created notification fired %v into a %v window, got %d",
			defaultCreateClaimWindow/2, defaultCreateClaimWindow, len(got))
	}

	// Wait out the rest of the create window without claiming.
	time.Sleep(defaultCreateClaimWindow/2 + 500*time.Millisecond)

	if got := mock.getCreated(); len(got) != 1 {
		t.Fatalf("expected 1 created notification after window expired, got %d", len(got))
	}
	if got := mock.getCreatedClaimed(); len(got) != 0 {
		t.Errorf("expected 0 merged notifications, got %d", len(got))
	}
}

// TestBeadActivity_CreateWithoutClaim_ContextNotCancelled guards the detached
// context on the deferred (AfterFunc) create-notification path. The real SSE
// dispatcher (sse.go) cancels the per-handler ctx via `defer cancel()` the
// instant handleCreated returns — but the AfterFunc fires ~createClaimWindow
// LATER. If the AfterFunc captured that handler ctx directly, NotifyBeadCreated
// would run on an already-cancelled context: vibeModeForAgent's daemon lookup
// fails closed on a dead ctx and silently drops the "Created" notification, and
// the downstream Slack post runs cancelled too. This test reproduces the SSE
// cancel-on-return and asserts the notifier receives a LIVE context.
//
// Fault injection: before the fix (AfterFunc captured the handler ctx), the
// recorded ctx.Err() is context.Canceled and this test fails.
func TestBeadActivity_CreateWithoutClaim_ContextNotCancelled(t *testing.T) {
	mock := &mockBeadActivityNotifier{}
	ba := newTestBeadActivity(mock)

	created := marshalSSEBeadPayload(BeadEvent{
		ID:        "gd-ctxcancel",
		Type:      "task",
		Title:     "Unclaimed task",
		CreatedBy: "test-agent",
	})

	// Mirror the SSE dispatcher: a per-handler context cancelled the moment
	// handleCreated returns (sse.go `defer cancel()`).
	handlerCtx, cancel := context.WithCancel(context.Background())
	ba.handleCreated(handlerCtx, created)
	cancel()

	// Wait for the buffered create to fire after the window.
	time.Sleep(testCreateClaimWindow + 500*time.Millisecond)

	if got := mock.getCreated(); len(got) != 1 {
		t.Fatalf("expected 1 created notification after window expired, got %d", len(got))
	}
	errs := mock.getCreatedCtxErrs()
	if len(errs) != 1 {
		t.Fatalf("expected 1 recorded ctx error, got %d", len(errs))
	}
	if errs[0] != nil {
		t.Errorf("deferred NotifyBeadCreated must receive a live (non-cancelled) context; got ctx.Err()=%v", errs[0])
	}
}

func TestBeadActivity_ClaimWithoutCreate_SeparateNotification(t *testing.T) {
	mock := &mockBeadActivityNotifier{}
	ba := newTestBeadActivity(mock)

	// Claim event with no preceding create (agent claims someone else's bead).
	claimed := marshalSSEBeadPayload(BeadEvent{
		ID:       "gd-claimonly",
		Type:     "bug",
		Title:    "Existing bug",
		Status:   "in_progress",
		Assignee: "test-agent",
	})
	ba.handleUpdated(context.Background(), claimed)

	if got := mock.getClaimed(); len(got) != 1 {
		t.Fatalf("expected 1 claimed notification, got %d", len(got))
	}
	if got := mock.getCreatedClaimed(); len(got) != 0 {
		t.Errorf("expected 0 merged notifications, got %d", len(got))
	}
}

func TestBeadActivity_NonWorkItemType_Ignored(t *testing.T) {
	mock := &mockBeadActivityNotifier{}
	ba := newTestBeadActivity(mock)

	for _, typ := range []string{"agent", "decision", "mail", "project", "config"} {
		data := marshalSSEBeadPayload(BeadEvent{
			ID:        "gd-infra",
			Type:      typ,
			CreatedBy: "test-agent",
		})
		ba.handleCreated(context.Background(), data)
	}

	time.Sleep(testCreateClaimWindow + 500*time.Millisecond)

	if got := mock.getCreated(); len(got) != 0 {
		t.Errorf("expected 0 notifications for non-work-item types, got %d", len(got))
	}
}

func TestBeadActivity_ClosedBead_Notifies(t *testing.T) {
	mock := &mockBeadActivityNotifier{}
	ba := newTestBeadActivity(mock)

	data := marshalSSEBeadPayload(BeadEvent{
		ID:       "gd-closed1",
		Type:     "task",
		Title:    "Done task",
		Assignee: "test-agent",
	})
	ba.handleClosed(context.Background(), data)

	if got := mock.getClosed(); len(got) != 1 {
		t.Fatalf("expected 1 closed notification, got %d", len(got))
	}
}

func TestBeadActivity_ClosedBead_NoAssignee_Ignored(t *testing.T) {
	mock := &mockBeadActivityNotifier{}
	ba := newTestBeadActivity(mock)

	data := marshalSSEBeadPayload(BeadEvent{
		ID:   "gd-closed-noassignee",
		Type: "task",
	})
	ba.handleClosed(context.Background(), data)

	if got := mock.getClosed(); len(got) != 0 {
		t.Errorf("expected 0 closed notifications for unassigned bead, got %d", len(got))
	}
}

func TestBeadActivity_DuplicateCreate_Deduped(t *testing.T) {
	mock := &mockBeadActivityNotifier{}
	ba := newTestBeadActivity(mock)

	data := marshalSSEBeadPayload(BeadEvent{
		ID:        "gd-dup",
		Type:      "feature",
		Title:     "Duplicate test",
		CreatedBy: "test-agent",
	})

	// Fire twice (simulates SSE reconnect replay).
	ba.handleCreated(context.Background(), data)
	ba.handleCreated(context.Background(), data)

	time.Sleep(testCreateClaimWindow + 500*time.Millisecond)

	if got := mock.getCreated(); len(got) != 1 {
		t.Errorf("expected 1 created notification (deduped), got %d", len(got))
	}
}

func TestBeadActivity_MalformedPayload_NoPanic(t *testing.T) {
	mock := &mockBeadActivityNotifier{}
	ba := newTestBeadActivity(mock)

	ba.handleCreated(context.Background(), []byte("not-json"))
	ba.handleUpdated(context.Background(), []byte("not-json"))
	ba.handleClosed(context.Background(), []byte("not-json"))
	// No panic = pass.
}

func TestBeadActivity_UpdateNonClaim_Ignored(t *testing.T) {
	mock := &mockBeadActivityNotifier{}
	ba := newTestBeadActivity(mock)

	// Update event that is NOT a claim (status != in_progress).
	data := marshalSSEBeadPayload(BeadEvent{
		ID:       "gd-nonclaim",
		Type:     "task",
		Title:    "Updated task",
		Status:   "open",
		Assignee: "test-agent",
	})
	ba.handleUpdated(context.Background(), data)

	if got := mock.getClaimed(); len(got) != 0 {
		t.Errorf("expected 0 claimed notifications for non-claim update, got %d", len(got))
	}
}

// TestNotifyBeadClaimed_ThreadBoundAgent_PostsAndStreams verifies the fix for
// gd-ziHiL448Pw: a thread-bound agent with an active streaming session must
// still receive bead lifecycle notifications in its bound thread, AND a
// task_update chunk in the streaming UI. Previously, an active stream
// suppressed the thread post entirely, so operators monitoring the thread had
// no visibility into the agent's bead activity.
func TestNotifyBeadClaimed_ThreadBoundAgent_PostsAndStreams(t *testing.T) {
	var postCount atomic.Int32
	var lastThreadTS atomic.Value
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postMessage" {
			postCount.Add(1)
			_ = r.ParseForm()
			lastThreadTS.Store(r.FormValue("thread_ts"))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "channel": "C-THREAD", "ts": "9999.0002",
		})
	}))
	defer slackSrv.Close()

	daemon := newMockDaemon()
	daemon.beads["thread-9999-0001"] = &beadsapi.BeadDetail{
		ID: "gd-thread-bound", Type: "agent", Status: "open",
		Title: "thread-9999-0001",
		Fields: map[string]string{
			"agent":                "thread-9999-0001",
			"slack_thread_channel": "C-THREAD",
			"slack_thread_ts":      "9999.0001",
		},
	}

	mockStream := &mockStreamClient{}
	mgr := NewAgentStreamManager(mockStream, slog.Default())

	b := newTestBot(daemon, slackSrv)
	b.beadMsgs = make(map[string]MessageRef)
	b.streamMgr = mgr
	enableVibeForAgent(daemon, "thread-9999-0001") // stamps operated_by on the seeded agent bead

	ctx := context.Background()
	mgr.StartSession(ctx, "thread-9999-0001", "C-THREAD", "9999.0001", "U12345")

	b.NotifyBeadClaimed(ctx, BeadEvent{
		ID:       "gd-test1",
		Type:     "task",
		Title:    "Test task",
		Assignee: "thread-9999-0001",
	})

	if got := postCount.Load(); got < 1 {
		t.Errorf("expected at least 1 chat.postMessage call to bound thread, got %d", got)
	}
	if got, _ := lastThreadTS.Load().(string); got != "9999.0001" {
		t.Errorf("expected thread_ts=9999.0001, got %q", got)
	}

	chunkCount := 0
	for _, c := range mockStream.getCalls() {
		if c.method == "appendChunks" {
			chunkCount++
		}
	}
	if chunkCount != 1 {
		t.Errorf("expected 1 stream appendChunks (task_update), got %d", chunkCount)
	}

	b.mu.Lock()
	ref, has := b.beadMsgs[beadMsgKey("thread-9999-0001", "gd-test1")]
	b.mu.Unlock()
	if !has {
		t.Fatal("expected beadMsgs entry to be tracked for in-place updates")
	}
	if ref.ThreadTS != "9999.0001" {
		t.Errorf("expected stored ThreadTS=9999.0001 (for restoreThreadStatus on update), got %q", ref.ThreadTS)
	}
}

func TestBeadMsgKey(t *testing.T) {
	key := beadMsgKey("my-agent", "gd-abc123")
	if key != "my-agent:gd-abc123" {
		t.Errorf("expected 'my-agent:gd-abc123', got %q", key)
	}
}

func TestPostOrUpdateBeadMessage_StoresRef(t *testing.T) {
	// Verify that beadMsgs map management works correctly.
	b := &Bot{
		daemon:          newMockDaemon(),
		logger:          slog.Default(),
		beadMsgs:        make(map[string]MessageRef),
		threadSpawnMsgs: make(map[string]MessageRef),
		agentCards:      map[string]MessageRef{},
		agentSeen:       map[string]time.Time{},
	}

	// With nil api, postBeadThreadMessage will try resolveAgentThread (no-op)
	// then check agentThreadingEnabled (false by default), so it drops the
	// notification. That's fine — we test the map cleanup via NotifyBeadClosed.
	b.NotifyBeadClosed(context.Background(), BeadEvent{
		ID:       "gd-test1",
		Type:     "task",
		Title:    "Test",
		Assignee: "test-agent",
	})

	// After close, the beadMsgs entry should be cleaned up.
	b.mu.Lock()
	_, has := b.beadMsgs[beadMsgKey("test-agent", "gd-test1")]
	b.mu.Unlock()
	if has {
		t.Error("expected beadMsgs entry to be cleaned up after close")
	}
}
