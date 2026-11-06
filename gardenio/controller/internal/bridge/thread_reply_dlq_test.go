package bridge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"

	"github.com/slack-go/slack/slackevents"
)

// newDLQTestBot builds a Bot with a temp-file-backed StateManager and a fake
// Slack server, plus a pre-seeded thread head so getOrCreateThreadBead resolves
// without a create. Returns the bot and the mock daemon.
func newDLQTestBot(t *testing.T, daemon *mockDaemon) *Bot {
	t.Helper()
	slackSrv := newFakeSlackServer(t)
	t.Cleanup(slackSrv.Close)
	b := newTestBot(daemon, slackSrv)
	b.botUserID = "U-BOT"
	b.lastThreadNudge = make(map[string]time.Time)
	state, err := NewStateManager(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("NewStateManager: %v", err)
	}
	b.state = state
	return b
}

// seedThreadHead pre-populates a resolvable wish head so the message-create stage
// is the one under test (not the head resolve).
func seedThreadHead(daemon *mockDaemon, channel, threadTS string) {
	daemon.beads["wish-"+channel+"-"+threadTS] = &beadsapi.BeadDetail{
		ID:     "wish-" + channel + "-" + threadTS,
		Title:  "wish",
		Type:   "task",
		Labels: []string{threadBeadLabel(channel, threadTS), threadKindLabel},
		Fields: map[string]string{
			"slack_thread_channel": channel,
			"slack_thread_ts":      threadTS,
		},
	}
}

func replyInput(channel, threadTS, messageTS, text string) unifiedThreadReplyInput {
	return unifiedThreadReplyInput{
		Channel:    channel,
		ThreadTS:   threadTS,
		MessageTS:  messageTS,
		Text:       text,
		SlackUser:  "U-HUMAN",
		AgentName:  "some-agent",
		ExtraLabel: "slack-thread-reply",
		LogPrefix:  "thread-forward",
	}
}

func countMessageBeads(daemon *mockDaemon) int {
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	n := 0
	for _, bead := range daemon.beads {
		if bead.Type == "message" && hasLabel(bead.Labels, "slack-thread-message") {
			n++
		}
	}
	return n
}

// TestRecordUnifiedThreadReply_RetriesTransientCreate proves the fix for the
// drop path: a transient (5xx) CreateBead failure on the message create is
// retried and the bubble ultimately persists — where before it was Warn-logged
// and dropped. The first create call (the thread head) is left alone; only the
// message create (idempotency-key prefix "thread-reply:") is failed once.
func TestRecordUnifiedThreadReply_RetriesTransientCreate(t *testing.T) {
	daemon := newMockDaemon()
	seedThreadHead(daemon, "C1", "111.1")
	b := newDLQTestBot(t, daemon)

	failedOnce := false
	daemon.createBeadHook = func(_ int, req beadsapi.CreateBeadRequest) error {
		if !failedOnce && req.IdempotencyKey == threadReplyIdempotencyKey("C1", "111.2") {
			failedOnce = true
			return &beadsapi.APIError{StatusCode: http.StatusInternalServerError, Message: "boom"}
		}
		return nil
	}

	beadID := b.recordUnifiedThreadReply(context.Background(), replyInput("C1", "111.1", "111.2", "hello agent"))
	if beadID == "" {
		t.Fatal("expected the bubble to persist after a transient 5xx retry; got empty bead id (dropped)")
	}
	if !failedOnce {
		t.Fatal("expected the injected transient failure to fire")
	}
	if got := countMessageBeads(daemon); got != 1 {
		t.Fatalf("expected exactly 1 message bead after retry, got %d", got)
	}
	if b.state.ThreadReplyDLQDepth() != 0 {
		t.Fatalf("a retried-then-succeeded reply must not enter the DLQ; depth=%d", b.state.ThreadReplyDLQDepth())
	}
}

// TestRecordUnifiedThreadReply_TerminalCreateEnqueuesDLQ proves that when the
// message create keeps failing with a transient error past the retry budget, the
// reply is NOT silently dropped — it is enqueued to the durable DLQ.
func TestRecordUnifiedThreadReply_TerminalCreateEnqueuesDLQ(t *testing.T) {
	daemon := newMockDaemon()
	seedThreadHead(daemon, "C2", "222.1")
	b := newDLQTestBot(t, daemon)

	daemon.createBeadHook = func(_ int, req beadsapi.CreateBeadRequest) error {
		if req.IdempotencyKey == threadReplyIdempotencyKey("C2", "222.2") {
			return &beadsapi.APIError{StatusCode: http.StatusServiceUnavailable, Message: "down"}
		}
		return nil
	}

	beadID := b.recordUnifiedThreadReply(context.Background(), replyInput("C2", "222.1", "222.2", "please don't drop me"))
	if beadID != "" {
		t.Fatalf("expected empty bead id on terminal failure, got %q", beadID)
	}
	if b.state.ThreadReplyDLQDepth() != 1 {
		t.Fatalf("terminal create failure must enqueue to DLQ; depth=%d want 1", b.state.ThreadReplyDLQDepth())
	}
	entries := b.state.AllThreadReplyDLQ()
	if len(entries) != 1 || entries[0].Text != "please don't drop me" {
		t.Fatalf("DLQ entry mismatch: %+v", entries)
	}
}

// TestRecordUnifiedThreadReply_NonTransientNotRetriedNotQueued proves that a
// deterministic 4xx (e.g. a validation rejection) is neither retried nor queued —
// a replay would fail identically, so queuing it would just churn the DLQ.
func TestRecordUnifiedThreadReply_NonTransientNotRetriedNotQueued(t *testing.T) {
	daemon := newMockDaemon()
	seedThreadHead(daemon, "C3", "333.1")
	b := newDLQTestBot(t, daemon)

	calls := 0
	daemon.createBeadHook = func(_ int, req beadsapi.CreateBeadRequest) error {
		if req.IdempotencyKey == threadReplyIdempotencyKey("C3", "333.2") {
			calls++
			return &beadsapi.APIError{StatusCode: http.StatusBadRequest, Message: "unknown field"}
		}
		return nil
	}

	beadID := b.recordUnifiedThreadReply(context.Background(), replyInput("C3", "333.1", "333.2", "bad reply"))
	if beadID != "" {
		t.Fatalf("expected empty bead id, got %q", beadID)
	}
	if calls != 1 {
		t.Fatalf("a non-transient 4xx must NOT be retried; create called %d times, want 1", calls)
	}
	// A deterministic 4xx would fail identically on replay, so it is dropped
	// (logged + counted), NOT queued — queuing it would only churn the DLQ.
	if b.state.ThreadReplyDLQDepth() != 0 {
		t.Fatalf("a non-transient failure must not enter the DLQ; depth=%d want 0", b.state.ThreadReplyDLQDepth())
	}
}

// TestDrainThreadReplyDLQ_ReplaysAndDequeues proves the drain loop replays a
// queued reply once the daemon recovers, persists the bubble, and removes the
// entry.
func TestDrainThreadReplyDLQ_ReplaysAndDequeues(t *testing.T) {
	daemon := newMockDaemon()
	seedThreadHead(daemon, "C4", "444.1")
	b := newDLQTestBot(t, daemon)

	// Fail the message create so the reply lands in the DLQ.
	down := true
	daemon.createBeadHook = func(_ int, req beadsapi.CreateBeadRequest) error {
		if down && req.IdempotencyKey == threadReplyIdempotencyKey("C4", "444.2") {
			return &beadsapi.APIError{StatusCode: http.StatusBadGateway, Message: "502"}
		}
		return nil
	}
	b.recordUnifiedThreadReply(context.Background(), replyInput("C4", "444.1", "444.2", "queued reply"))
	if b.state.ThreadReplyDLQDepth() != 1 {
		t.Fatalf("precondition: expected 1 queued reply, got %d", b.state.ThreadReplyDLQDepth())
	}

	// Daemon recovers; drain replays it.
	down = false
	b.drainThreadReplyDLQ(context.Background())

	if b.state.ThreadReplyDLQDepth() != 0 {
		t.Fatalf("drain should dequeue the replayed reply; depth=%d", b.state.ThreadReplyDLQDepth())
	}
	if got := countMessageBeads(daemon); got != 1 {
		t.Fatalf("drain should have persisted the bubble; message beads=%d want 1", got)
	}
}

// TestDrainThreadReplyDLQ_IdempotentReplay proves that replaying a reply whose
// original create ACTUALLY LANDED (a lost-response false-negative) does not
// double-post: the idempotency key collapses the replay to the existing bead.
func TestDrainThreadReplyDLQ_IdempotentReplay(t *testing.T) {
	daemon := newMockDaemon()
	seedThreadHead(daemon, "C5", "555.1")
	b := newDLQTestBot(t, daemon)

	// Simulate a lost response: the create SUCCEEDS at the daemon (bead recorded)
	// but the client sees a transient error, so the reply is queued for replay.
	in := replyInput("C5", "555.1", "555.2", "landed but lost response")
	if _, _, err := b.writeThreadConversationBead(context.Background(), in, "landed but lost response", nil); err != nil {
		t.Fatalf("seed write failed: %v", err)
	}
	if countMessageBeads(daemon) != 1 {
		t.Fatalf("precondition: expected 1 bead from seed write, got %d", countMessageBeads(daemon))
	}
	// Now enqueue the same reply as if the client had not observed that success.
	b.enqueueThreadReplyDLQ(in, "landed but lost response")
	if b.state.ThreadReplyDLQDepth() != 1 {
		t.Fatalf("precondition: expected 1 queued reply, got %d", b.state.ThreadReplyDLQDepth())
	}

	b.drainThreadReplyDLQ(context.Background())

	if got := countMessageBeads(daemon); got != 1 {
		t.Fatalf("idempotent replay must NOT double-post; message beads=%d want 1", got)
	}
	if b.state.ThreadReplyDLQDepth() != 0 {
		t.Fatalf("replay should have dequeued the entry; depth=%d", b.state.ThreadReplyDLQDepth())
	}
}

// TestEnqueueThreadReplyDLQ_CoalescesByKey proves a re-enqueue of the same reply
// does not grow the queue and preserves the original enqueue time.
func TestEnqueueThreadReplyDLQ_CoalescesByKey(t *testing.T) {
	daemon := newMockDaemon()
	b := newDLQTestBot(t, daemon)
	in := replyInput("C6", "666.1", "666.2", "dup reply")

	b.enqueueThreadReplyDLQ(in, "dup reply")
	first := b.state.AllThreadReplyDLQ()
	if len(first) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(first))
	}
	b.enqueueThreadReplyDLQ(in, "dup reply")
	second := b.state.AllThreadReplyDLQ()
	if len(second) != 1 {
		t.Fatalf("re-enqueue of same key must coalesce; got %d entries", len(second))
	}
	if !second[0].EnqueuedAt.Equal(first[0].EnqueuedAt) {
		t.Errorf("coalesced entry should preserve original enqueue time; %v != %v", second[0].EnqueuedAt, first[0].EnqueuedAt)
	}
}

// TestDrainThreadReplyDLQ_DropsPoisonedEntry proves a reply that can never
// replay (persistent deterministic failure) is eventually dropped rather than
// wedging the queue forever.
func TestDrainThreadReplyDLQ_DropsPoisonedEntry(t *testing.T) {
	daemon := newMockDaemon()
	seedThreadHead(daemon, "C7", "777.1")
	b := newDLQTestBot(t, daemon)

	// Always fail the message create.
	daemon.createBeadHook = func(_ int, req beadsapi.CreateBeadRequest) error {
		if req.IdempotencyKey == threadReplyIdempotencyKey("C7", "777.2") {
			return &beadsapi.APIError{StatusCode: http.StatusBadRequest, Message: "permanently bad"}
		}
		return nil
	}
	in := replyInput("C7", "777.1", "777.2", "poison")
	b.enqueueThreadReplyDLQ(in, "poison")

	// Drain up to the attempt budget; the entry should be dropped by the end.
	for i := 0; i < threadReplyDLQMaxAttempts+1 && b.state.ThreadReplyDLQDepth() > 0; i++ {
		b.drainThreadReplyDLQ(context.Background())
	}
	if b.state.ThreadReplyDLQDepth() != 0 {
		t.Fatalf("poisoned entry should be dropped after exhausting attempts; depth=%d", b.state.ThreadReplyDLQDepth())
	}
}

func TestIsTransientDaemonError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"500", &beadsapi.APIError{StatusCode: http.StatusInternalServerError}, true},
		{"502", &beadsapi.APIError{StatusCode: http.StatusBadGateway}, true},
		{"503", &beadsapi.APIError{StatusCode: http.StatusServiceUnavailable}, true},
		{"429", &beadsapi.APIError{StatusCode: http.StatusTooManyRequests}, true},
		{"400", &beadsapi.APIError{StatusCode: http.StatusBadRequest}, false},
		{"404", &beadsapi.APIError{StatusCode: http.StatusNotFound}, false},
		{"deadline", context.DeadlineExceeded, true},
		{"conn reset", errors.New("read tcp: connection reset by peer"), true},
		{"conn refused", errors.New("dial tcp: connection refused"), true},
		{"plain", errors.New("some validation problem"), false},
		{"wrapped 503", fmt.Errorf("creating bead: %w", &beadsapi.APIError{StatusCode: http.StatusServiceUnavailable}), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTransientDaemonError(tc.err); got != tc.want {
				t.Errorf("isTransientDaemonError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestHandleThreadForward_TransientCreateFailure_ReplyQueuedNotDropped is the
// end-to-end regression for ka-0000000612: a human thread reply arriving while
// the daemon is returning transient 5xx must NOT vanish from the /threads lane —
// it lands in the DLQ for replay, and the agent is still nudged.
func TestHandleThreadForward_TransientCreateFailure_ReplyQueuedNotDropped(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["thread-agent"] = &beadsapi.BeadDetail{
		ID:    "bd-thread-agent",
		Title: "thread-agent",
		Type:  "agent",
		Fields: map[string]string{
			"agent":                "thread-agent",
			"project":              "gardenio",
			"slack_thread_channel": "C-e2e",
			"slack_thread_ts":      "888.1",
		},
	}
	seedThreadHead(daemon, "C-e2e", "888.1")
	b := newDLQTestBot(t, daemon)
	_ = b.state.SetThreadAgent("C-e2e", "888.1", "thread-agent")

	// The daemon is down for the message create.
	daemon.createBeadHook = func(_ int, req beadsapi.CreateBeadRequest) error {
		if req.IdempotencyKey == threadReplyIdempotencyKey("C-e2e", "888.2") {
			return &beadsapi.APIError{StatusCode: http.StatusServiceUnavailable, Message: "daemon restarting"}
		}
		return nil
	}

	ev := &slackevents.MessageEvent{
		User:            "U-HUMAN",
		Channel:         "C-e2e",
		Text:            "critical follow-up during an outage",
		TimeStamp:       "888.2",
		ThreadTimeStamp: "888.1",
	}
	b.handleThreadForward(context.Background(), slackReplyInbound(ev, ""), "thread-agent")

	if b.state.ThreadReplyDLQDepth() != 1 {
		t.Fatalf("REGRESSION (ka-0000000612): a reply that failed to persist during a "+
			"daemon outage must be queued for replay, not dropped; DLQ depth=%d", b.state.ThreadReplyDLQDepth())
	}

	// Recovery: drain replays the queued reply into a real bubble.
	daemon.createBeadHook = nil
	b.drainThreadReplyDLQ(context.Background())
	if countMessageBeads(daemon) != 1 {
		t.Fatalf("expected the queued reply to be replayed into a bubble; message beads=%d", countMessageBeads(daemon))
	}
	if b.state.ThreadReplyDLQDepth() != 0 {
		t.Fatalf("DLQ should be empty after successful replay; depth=%d", b.state.ThreadReplyDLQDepth())
	}
}
