package assistantturnminter

// assistantturnminter_replay_test.go — the DURABLE FULL-TAIL REPLAY hardening
// layer: the DeliverAll (not DeliverLastPerSubject) swap so a MULTI-turn host
// outage is recovered, the background/off-the-boot-path replay, the
// deterministic LastSeq/NumPending end-of-tail signal, the hydration-invariant
// turn_id, and the bounded goroutine shutdown. The end-to-end cases use an
// embedded JetStream NATS server to exercise the real DeliverAll consumer.

import (
	"context"
	"encoding/json"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/coopmuxapi"
	"gardenio/controller/internal/plantranscript"
)

// startEmbeddedNATS spins up an in-process NATS server (no JetStream) on a
// random port and returns its ClientURL. Shut down via t.Cleanup.
func startEmbeddedNATS(t *testing.T) string {
	t.Helper()
	opts := &natsserver.Options{Host: "127.0.0.1", Port: -1}
	ns, err := natsserver.NewServer(opts)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		ns.Shutdown()
		t.Fatal("embedded NATS not ready within 5s")
	}
	t.Cleanup(func() { ns.Shutdown() })
	return ns.ClientURL()
}

// startEmbeddedJetStreamNATS spins up an in-process NATS server with JetStream
// enabled and creates the MESSAGE_EVENTS stream the minter backfills from.
func startEmbeddedJetStreamNATS(t *testing.T) string {
	t.Helper()
	opts := &natsserver.Options{
		Host:      "127.0.0.1",
		Port:      -1,
		JetStream: true,
		StoreDir:  t.TempDir(),
	}
	ns, err := natsserver.NewServer(opts)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		ns.Shutdown()
		t.Fatal("embedded JetStream NATS not ready within 5s")
	}
	t.Cleanup(func() { ns.Shutdown() })

	url := ns.ClientURL()
	conn, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close()
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if _, err := js.AddStream(&nats.StreamConfig{
		Name:     assistantTurnStreamName,
		Subjects: []string{"coop.events.messages.>"},
		Storage:  nats.FileStorage,
		MaxMsgs:  10000,
	}); err != nil {
		t.Fatalf("add stream: %v", err)
	}
	return url
}

// publishAssistantJS publishes an assistant_text event to the MESSAGE_EVENTS
// JetStream stream so a subsequently-started minter replays it on backfill.
func publishAssistantJS(t *testing.T, js nats.JetStreamContext, actor, role, agentBeadID, sessionID string, ts int64, text string, fullLength int) {
	t.Helper()
	data := assistantEvent(actor, role, agentBeadID, sessionID, ts, text, fullLength).Data
	if _, err := js.Publish("coop.events.messages."+actor+".assistant_text", data); err != nil {
		t.Fatalf("publish: %v", err)
	}
}

// publishToolUseJS publishes a tool_use event to the MESSAGE_EVENTS JetStream
// stream so a subsequently-started minter replays it on backfill (data slice
// ka-0000004690).
func publishToolUseJS(t *testing.T, js nats.JetStreamContext, actor, role, agentBeadID, sessionID string, ts int64, toolName, argsSummary string) {
	t.Helper()
	data := toolUseEvent(actor, role, agentBeadID, sessionID, ts, toolName, argsSummary).Data
	if _, err := js.Publish("coop.events.messages."+actor+".tool_use", data); err != nil {
		t.Fatalf("publish tool_use: %v", err)
	}
}

// waitForMintCount polls the message-create count until it reaches want or the
// deadline elapses, so an async background replay can be asserted without a
// fixed sleep.
func waitForMintCount(t *testing.T, f *fakeDaemon, want int, within time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		got := f.createCallsOfType("message")
		if got >= want || time.Now().After(deadline) {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestMultiTurnOutageRecovery is THE gap this design closes: a host down for
// MULTIPLE turns of one plan replays and mints EVERY missed turn, not just the
// newest per subject. DeliverLastPerSubject would mint only 1; DeliverAll mints
// all N.
func TestMultiTurnOutageRecovery(t *testing.T) {
	url := startEmbeddedJetStreamNATS(t)

	pub, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	js, err := pub.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	const n = 5
	for i := range n {
		publishAssistantJS(t, js, "gardenio-plan-asst-1", plantranscript.PlanGardenerRole, "gd-asst1", "sess-9",
			int64(1700000000000+i), "missed turn "+strconv.Itoa(i), 0)
	}
	pub.Close()

	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	seedAssistantBead(f, "gd-asst1", "gardenio-plan-asst-1", "gd-plan1", "working", testNow)

	m := New(url, "", "", f, &fakeScreenClient{}, nil)
	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Close()

	if got := waitForMintCount(t, f, n, 10*time.Second); got != n {
		t.Fatalf("multi-turn outage must replay ALL %d missed turns (DeliverAll), got %d "+
			"(DeliverLastPerSubject would have minted only 1)", n, got)
	}

	if keys := distinctMessageKeys(f); len(keys) != n {
		t.Errorf("expected %d distinct turn IdempotencyKeys, got %d", n, len(keys))
	}
}

// TestToolUseReplayMintsToolTurns proves the full-tail JetStream replay recovers
// TOOL turns too: a mix of retained assistant_text and tool_use events, replayed
// on a fresh host, mints both an assistant prose turn AND a tool turn (the
// shared subscription is coop.events.messages.*.*, and handleMessageCtx routes
// each by event_type). This is the replay-path E2E for the data slice.
func TestToolUseReplayMintsToolTurns(t *testing.T) {
	url := startEmbeddedJetStreamNATS(t)

	pub, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	js, err := pub.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	// A realistic turn sequence: the plan agent replies, then calls two tools.
	publishAssistantJS(t, js, "gardenio-plan-asst-1", plantranscript.PlanGardenerRole, "gd-asst1", "sess-9", 1700000000000, "On it — dispatching e2e.", 0)
	publishToolUseJS(t, js, "gardenio-plan-asst-1", plantranscript.PlanGardenerRole, "gd-asst1", "sess-9", 1700000000001, "Bash", "gh workflow run e2e.yml")
	publishToolUseJS(t, js, "gardenio-plan-asst-1", plantranscript.PlanGardenerRole, "gd-asst1", "sess-9", 1700000000002, "Read", "plan.md")
	pub.Close()

	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	seedAssistantBead(f, "gd-asst1", "gardenio-plan-asst-1", "gd-plan1", "working", testNow)

	m := New(url, "", "", f, &fakeScreenClient{}, nil)
	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Close()

	if got := waitForMintCount(t, f, 3, 10*time.Second); got != 3 {
		t.Fatalf("replay must mint all 3 turns (1 assistant + 2 tool), got %d", got)
	}

	// Verify the turn_role mix: exactly one assistant prose turn and two tool turns.
	assistant, tool := 0, 0
	f.mintMu.Lock()
	for _, req := range f.createReqs {
		if req.Type != "message" {
			continue
		}
		fields := map[string]string{}
		_ = json.Unmarshal(req.Fields, &fields)
		switch fields[plantranscript.TurnRoleField] {
		case plantranscript.TurnRoleAssistant:
			assistant++
		case plantranscript.TurnRoleTool:
			tool++
		}
	}
	f.mintMu.Unlock()
	if assistant != 1 || tool != 2 {
		t.Errorf("replay turn-role mix = %d assistant / %d tool, want 1 / 2", assistant, tool)
	}
}

// TestBackfillDoesNotBlockStart proves the replay is OFF the boot critical path:
// Start() returns promptly even when the daemon's CreateBead blocks.
func TestBackfillDoesNotBlockStart(t *testing.T) {
	url := startEmbeddedJetStreamNATS(t)
	pub, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	js, err := pub.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	publishAssistantJS(t, js, "gardenio-plan-asst-1", plantranscript.PlanGardenerRole, "gd-asst1", "sess-9", 1, "slow turn", 0)
	pub.Close()

	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	seedAssistantBead(f, "gd-asst1", "gardenio-plan-asst-1", "gd-plan1", "working", testNow)
	release := make(chan struct{})
	var blocked atomic.Bool
	f.createOnSuccess = func(_ *fakeDaemon, _ string, _ beadsapi.CreateBeadRequest) {
		blocked.Store(true)
		<-release
	}

	m := New(url, "", "", f, &fakeScreenClient{}, nil)
	start := time.Now()
	if err := m.Start(); err != nil {
		close(release)
		t.Fatalf("Start: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		close(release)
		t.Fatalf("Start() must not block on the backfill mint; took %v", elapsed)
	}
	for i := 0; i < 200 && !blocked.Load(); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if !blocked.Load() {
		close(release)
		t.Fatal("expected the background replay to reach the (blocked) mint")
	}
	close(release)
	m.Close()
}

// TestOversizedReplayConvergesOnOneKey is the load-bearing dedup fix for the
// full-tail replay: an oversized (>256KB) reply observed by the LIVE path (full
// body hydrated) and later re-observed by the REPLAY path (clipped copy kept,
// truncated) must mint against the SAME IdempotencyKey, because turn_id is
// derived from the hydration-INVARIANT wire fields.
func TestOversizedReplayConvergesOnOneKey(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	seedAssistantBead(f, "gd-asst1", "gardenio-plan-asst-1", "gd-plan1", "working", testNow)

	evt := assistantEvent("gardenio-plan-asst-1", plantranscript.PlanGardenerRole, "gd-asst1", "sess-9", 1,
		"clipped prefix of the reply...", 999999)

	full := "clipped prefix of the reply followed by the rest of a very long body"
	cmLive := &fakeScreenClient{agentState: &coopmuxapi.AgentState{LastMessage: full}}
	mLive := mkMinter(f, cmLive, testNow)
	mLive.handleMessage(evt)

	cmReplay := &fakeScreenClient{agentState: &coopmuxapi.AgentState{LastMessage: "a completely different newer reply"}}
	mReplay := mkMinter(f, cmReplay, testNow)
	mReplay.handleMessage(evt)

	if keys := distinctMessageKeys(f); len(keys) != 1 {
		t.Errorf("oversized live(full)+replay(clipped) of the SAME event must converge on ONE "+
			"IdempotencyKey (hydration-invariant turn_id); got %d: %v", len(keys), keys)
	}
}

// TestAssistantTurnID_HydrationInvariant proves turn_id depends only on the wire
// fields — the same wire event always yields the same id regardless of any
// post-hydration body — while distinct wire texts, or the same prefix with a
// different true length, yield distinct ids.
func TestAssistantTurnID_HydrationInvariant(t *testing.T) {
	base := assistantTurnID("sess-9", 1, "clip...", 999999)

	if again := assistantTurnID("sess-9", 1, "clip...", 999999); again != base {
		t.Errorf("same wire fields must yield same turn_id; got %q vs %q", again, base)
	}
	if diff := assistantTurnID("sess-9", 1, "clip...", 12345); diff == base {
		t.Errorf("different full_length must yield a distinct turn_id; both %q", base)
	}
	if diff := assistantTurnID("sess-9", 2, "clip...", 999999); diff == base {
		t.Errorf("different ts must yield a distinct turn_id; both %q", base)
	}
	if diff := assistantTurnID("sess-8", 1, "clip...", 999999); diff == base {
		t.Errorf("different session must yield a distinct turn_id; both %q", base)
	}
}

// TestTailReached covers the deterministic end-of-tail decision.
func TestTailReached(t *testing.T) {
	cases := []struct {
		name                        string
		numPending, streamSeq, last uint64
		want                        bool
	}{
		{"pending zero → tail", 0, 50, 100, true},
		{"pending>0, seq below last → not tail", 3, 50, 100, false},
		{"pending>0, seq reached last → tail", 3, 100, 100, true},
		{"pending>0, seq past last → tail", 3, 101, 100, true},
		{"pending>0, seq one below last → not tail", 1, 99, 100, false},
	}
	for _, c := range cases {
		if got := tailReached(c.numPending, c.streamSeq, c.last); got != c.want {
			t.Errorf("%s: tailReached(%d,%d,%d)=%v want %v", c.name, c.numPending, c.streamSeq, c.last, got, c.want)
		}
	}
}

// TestBackfillReachedTail_NoMetadata proves a message without JetStream metadata
// does NOT falsely signal end-of-tail.
func TestBackfillReachedTail_NoMetadata(t *testing.T) {
	plain := &nats.Msg{Subject: "coop.events.messages.a.assistant_text"}
	if backfillReachedTail(plain, 100) {
		t.Error("a message without JetStream metadata must NOT signal end-of-tail")
	}
}

// TestCloseJoinsBackfillGoroutine proves Close() cancels the background replay
// and joins the goroutine promptly even when the daemon mint is blocked
// mid-replay — no goroutine leak, no hang past the shutdown grace period.
func TestCloseJoinsBackfillGoroutine(t *testing.T) {
	url := startEmbeddedJetStreamNATS(t)
	pub, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	js, err := pub.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	for i := range 10 {
		publishAssistantJS(t, js, "gardenio-plan-asst-1", plantranscript.PlanGardenerRole, "gd-asst1", "sess-9", int64(i+1), "turn", 0)
	}
	pub.Close()

	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	seedAssistantBead(f, "gd-asst1", "gardenio-plan-asst-1", "gd-plan1", "working", testNow)
	reached := make(chan struct{}, 1)
	var once sync.Once
	f.createOnSuccess = func(_ *fakeDaemon, _ string, _ beadsapi.CreateBeadRequest) {
		once.Do(func() { close(reached) })
		time.Sleep(50 * time.Millisecond)
	}

	m := New(url, "", "", f, &fakeScreenClient{}, nil)
	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		m.Close()
		t.Fatal("background replay never reached the mint")
	}

	done := make(chan struct{})
	go func() { m.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(backfillJoinTimeout + 2*time.Second):
		t.Fatal("Close() did not return within the bounded join window — goroutine hang")
	}
}

// TestConcurrentLiveAndReplayDedup drives the live path and the replay path on
// TWO goroutines for the SAME event and asserts they converge on exactly ONE
// IdempotencyKey. Guards the fakeDaemon mint mutex under `go test -race`.
func TestConcurrentLiveAndReplayDedup(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	seedAssistantBead(f, "gd-asst1", "gardenio-plan-asst-1", "gd-plan1", "working", testNow)
	m := mkMinter(f, &fakeScreenClient{}, testNow)

	live := assistantEvent("gardenio-plan-asst-1", plantranscript.PlanGardenerRole, "gd-asst1", "sess-9", 1700000000000, "the reply", 0)
	replay := assistantEvent("gardenio-plan-asst-1", plantranscript.PlanGardenerRole, "gd-asst1", "sess-9", 1700000000000, "the reply", 0)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); m.handleMessage(live) }()
	go func() { defer wg.Done(); m.handleMessageCtx(context.Background(), replay) }()
	wg.Wait()

	if keys := distinctMessageKeys(f); len(keys) != 1 {
		t.Errorf("concurrent live+replay of the same event must converge on ONE IdempotencyKey; got %d: %v", len(keys), keys)
	}
}

// TestReplayNoStreamIsLiveOnly proves the background replay degrades gracefully
// when JetStream / the stream is absent: Start() succeeds, the live subscription
// still works, and no panic/leak on Close().
func TestReplayNoStreamIsLiveOnly(t *testing.T) {
	url := startEmbeddedNATS(t)
	f := newFake()
	m := New(url, "", "", f, &fakeScreenClient{}, nil)
	if err := m.Start(); err != nil {
		t.Fatalf("Start should succeed without JetStream: %v", err)
	}
	m.Close()
}
