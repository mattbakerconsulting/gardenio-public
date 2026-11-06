package main

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

// startEmbeddedNATS spins up an in-process NATS server on a random port and
// returns its ClientURL. Shut down via t.Cleanup.
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
// enabled (file store in a temp dir) and creates the MESSAGE_EVENTS stream that
// the hub backfills from. Returns the ClientURL.
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
		Name:     thinkingStreamName,
		Subjects: []string{"coop.events.messages.>"},
		Storage:  nats.FileStorage,
		MaxMsgs:  10000,
	}); err != nil {
		t.Fatalf("add stream: %v", err)
	}
	return url
}

func TestActorFromSubject(t *testing.T) {
	cases := []struct {
		subject string
		want    string
	}{
		{"coop.events.messages.gardenio-foo-123.thinking", "gardenio-foo-123"},
		{"coop.events.messages.a.thinking", "a"},
		{"coop.events.messages.gardenio-foo.bar-123.thinking", "gardenio-foo.bar-123"},
		{"coop.events.messages.x.assistant_text", ""}, // wrong suffix
		{"hooks.x.thinking", ""},                       // wrong prefix
		{"coop.events.messages..thinking", ""},         // empty actor (too short)
		{"", ""},
	}
	for _, c := range cases {
		if got := actorFromSubject(c.subject); got != c.want {
			t.Errorf("actorFromSubject(%q) = %q, want %q", c.subject, got, c.want)
		}
	}
}

func TestNewThinkingHub_NilWhenNoURL(t *testing.T) {
	if h := NewThinkingHub("", "tok", "", nil); h != nil {
		t.Fatalf("expected nil hub when NATS_URL empty, got %v", h)
	}
}

func TestThinkingHub_NilSafe(t *testing.T) {
	var h *ThinkingHub // nil

	if err := h.Start(); err != nil {
		t.Errorf("nil Start() returned error: %v", err)
	}
	h.Close() // must not panic

	if got := h.Recent(); got != nil {
		t.Errorf("nil Recent() = %v, want nil", got)
	}

	ch, cancel := h.Subscribe(context.Background())
	cancel() // must not panic
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("nil Subscribe() channel should be closed (no events)")
		}
	case <-time.After(time.Second):
		t.Error("nil Subscribe() channel should be closed, not blocking")
	}
}

// newTestHub builds a hub with the internal maps initialized but no NATS
// connection, so broadcast/Subscribe can be exercised directly.
func newTestHub() *ThinkingHub {
	return &ThinkingHub{
		clients: make(map[int]chan ThinkingEvent),
	}
}

// TestThinkingHub_SubscribeBackgroundCtx_NoWatcherLeak mirrors the
// BeadFeedHub regression guard for bug ka-0000005687: Subscribe with a
// non-cancellable context (Done() == nil) must not park a watcher goroutine
// on a nil channel, and explicit unsubscribe must release the watcher on the
// cancellable path too.
func TestThinkingHub_SubscribeBackgroundCtx_NoWatcherLeak(t *testing.T) {
	h := newTestHub()

	before := runtime.NumGoroutine()
	const n = 50
	for range n {
		_, unsub := h.Subscribe(context.Background())
		unsub()
	}
	ctx, ctxCancel := context.WithCancel(context.Background())
	defer ctxCancel()
	for range n {
		_, unsub := h.Subscribe(ctx)
		unsub()
	}
	deadline := time.After(2 * time.Second)
	for {
		if runtime.NumGoroutine() <= before+2 {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("goroutines leaked by Subscribe: before=%d after=%d", before, runtime.NumGoroutine())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestThinkingHub_BroadcastFanOut(t *testing.T) {
	h := newTestHub()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	chA, cancelA := h.Subscribe(ctx)
	defer cancelA()
	chB, cancelB := h.Subscribe(ctx)
	defer cancelB()

	h.broadcast(ThinkingEvent{Agent: "agent-1", Text: "hello"})

	for name, ch := range map[string]<-chan ThinkingEvent{"A": chA, "B": chB} {
		select {
		case evt := <-ch:
			if evt.Agent != "agent-1" || evt.Text != "hello" {
				t.Errorf("client %s got %+v", name, evt)
			}
		case <-time.After(time.Second):
			t.Errorf("client %s did not receive event", name)
		}
	}
}

func TestThinkingHub_Backfill(t *testing.T) {
	h := newTestHub()
	for i := 0; i < 5; i++ {
		h.broadcast(ThinkingEvent{Agent: "a", Text: fmt.Sprintf("t%d", i)})
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, unsub := h.Subscribe(ctx)
	defer unsub()

	// A freshly-connected client should be backfilled with the ring, oldest
	// first, before any live events.
	for i := 0; i < 5; i++ {
		select {
		case evt := <-ch:
			if evt.Text != fmt.Sprintf("t%d", i) {
				t.Errorf("backfill[%d] = %q, want t%d", i, evt.Text, i)
			}
		case <-time.After(time.Second):
			t.Fatalf("missing backfill event %d", i)
		}
	}
}

func TestThinkingHub_RingBounded(t *testing.T) {
	h := newTestHub()
	for i := 0; i < thinkingRingCapacity+50; i++ {
		h.broadcast(ThinkingEvent{Agent: "a", Text: fmt.Sprintf("t%d", i)})
	}
	got := h.Recent()
	if len(got) != thinkingRingCapacity {
		t.Fatalf("ring size = %d, want %d", len(got), thinkingRingCapacity)
	}
	// Oldest retained event should be t50 (first 50 dropped).
	if got[0].Text != "t50" {
		t.Errorf("oldest retained = %q, want t50", got[0].Text)
	}
	if got[len(got)-1].Text != fmt.Sprintf("t%d", thinkingRingCapacity+49) {
		t.Errorf("newest retained = %q", got[len(got)-1].Text)
	}
}

func TestThinkingHub_SlowClientDropsOldest(t *testing.T) {
	h := newTestHub()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, unsub := h.Subscribe(ctx)
	defer unsub()

	// Publish more than the per-client buffer without draining. The hub must
	// never block; the client ends up with the most-recent thinkingClientBuffer
	// events.
	total := thinkingClientBuffer + 100
	for i := 0; i < total; i++ {
		h.broadcast(ThinkingEvent{Agent: "a", Text: fmt.Sprintf("t%d", i)})
	}

	// Drain what's buffered. The first received event must be newer than the
	// earliest published (drop-oldest happened), and the very last published
	// event must be present.
	var received []string
	drain := true
	for drain {
		select {
		case evt := <-ch:
			received = append(received, evt.Text)
		default:
			drain = false
		}
	}
	if len(received) == 0 {
		t.Fatal("expected buffered events, got none")
	}
	if len(received) > thinkingClientBuffer {
		t.Fatalf("buffered %d events, exceeds cap %d", len(received), thinkingClientBuffer)
	}
	last := received[len(received)-1]
	if last != fmt.Sprintf("t%d", total-1) {
		t.Errorf("last buffered = %q, want t%d (newest must survive)", last, total-1)
	}
}

func TestThinkingHub_UnsubscribeStopsDelivery(t *testing.T) {
	h := newTestHub()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, unsub := h.Subscribe(ctx)

	unsub()
	// Channel must be closed.
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("channel not closed after unsubscribe")
		}
	case <-time.After(time.Second):
		t.Error("channel should be closed after unsubscribe")
	}

	// Broadcast after unsubscribe must not panic (no send on closed channel).
	h.broadcast(ThinkingEvent{Agent: "a", Text: "after"})

	h.mu.Lock()
	n := len(h.clients)
	h.mu.Unlock()
	if n != 0 {
		t.Errorf("clients map size = %d after unsubscribe, want 0", n)
	}
}

func TestThinkingHub_ContextCancelUnsubscribes(t *testing.T) {
	h := newTestHub()
	ctx, cancel := context.WithCancel(context.Background())
	_, _ = h.Subscribe(ctx)

	cancel()

	// The auto-unsubscribe goroutine should drop the client shortly.
	deadline := time.After(2 * time.Second)
	for {
		h.mu.Lock()
		n := len(h.clients)
		h.mu.Unlock()
		if n == 0 {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("client not removed after context cancel (still %d)", n)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestThinkingHub_HandleMessageParsing(t *testing.T) {
	h := newTestHub()

	// Full payload with actor + k8s + metadata.
	payload := map[string]any{
		"event_type":    "thinking",
		"actor":         "gardenio-foo-123",
		"agent":         "gardenio-foo-123",
		"text":          "I should check the logs",
		"full_length":   1234,
		"session_id":    "sess-abc",
		"project":       "gardenio",
		"role":          "crew",
		"agent_bead_id": "gd-AGENT1",
		"source":        "litellm-callback",
		"ts":            int64(1700000000000),
		"k8s":           map[string]any{"pod": "gardenio-foo-123-pod"},
	}
	data, _ := json.Marshal(payload)
	h.handleMessage(&nats.Msg{Subject: "coop.events.messages.gardenio-foo-123.thinking", Data: data})

	got := h.Recent()
	if len(got) != 1 {
		t.Fatalf("expected 1 event, got %d", len(got))
	}
	e := got[0]
	if e.Agent != "gardenio-foo-123" {
		t.Errorf("Agent = %q", e.Agent)
	}
	if e.Text != "I should check the logs" {
		t.Errorf("Text = %q", e.Text)
	}
	if e.FullLength != 1234 {
		t.Errorf("FullLength = %d", e.FullLength)
	}
	if e.Project != "gardenio" || e.Role != "crew" {
		t.Errorf("Project/Role = %q/%q", e.Project, e.Role)
	}
	if e.AgentBeadID != "gd-AGENT1" {
		t.Errorf("AgentBeadID = %q", e.AgentBeadID)
	}
	if e.PodName != "gardenio-foo-123-pod" {
		t.Errorf("PodName = %q", e.PodName)
	}
	if e.Source != "litellm-callback" {
		t.Errorf("Source = %q, want litellm-callback", e.Source)
	}
	if e.TS != 1700000000000 {
		t.Errorf("TS = %d", e.TS)
	}
	if e.ReceivedAt.IsZero() {
		t.Error("ReceivedAt not set")
	}
}

func TestThinkingHub_HandleMessageSkipsEmptyAndMalformed(t *testing.T) {
	h := newTestHub()

	// Empty text — coop forwards signature-only thinking blocks; skip them.
	empty, _ := json.Marshal(map[string]any{"actor": "a", "text": ""})
	h.handleMessage(&nats.Msg{Subject: "coop.events.messages.a.thinking", Data: empty})

	// Malformed JSON.
	h.handleMessage(&nats.Msg{Subject: "coop.events.messages.a.thinking", Data: []byte("{not json")})

	if got := h.Recent(); len(got) != 0 {
		t.Fatalf("expected 0 events (empty+malformed skipped), got %d: %+v", len(got), got)
	}
}

func TestThinkingHub_HandleMessageActorFallback(t *testing.T) {
	h := newTestHub()

	// No actor/agent in payload — fall back to the subject's actor segment.
	data, _ := json.Marshal(map[string]any{"text": "thinking hard"})
	h.handleMessage(&nats.Msg{Subject: "coop.events.messages.gardenio-fallback-9.thinking", Data: data})

	got := h.Recent()
	if len(got) != 1 {
		t.Fatalf("expected 1 event, got %d", len(got))
	}
	if got[0].Agent != "gardenio-fallback-9" {
		t.Errorf("Agent = %q, want fallback from subject", got[0].Agent)
	}
}

// TestThinkingHub_EndToEndNATS exercises the full path: a real embedded NATS
// server, a published thinking event on the wildcard subject, and delivery to
// a subscribed in-process client.
func TestThinkingHub_EndToEndNATS(t *testing.T) {
	url := startEmbeddedNATS(t)

	h := NewThinkingHub(url, "", "", nil)
	if h == nil {
		t.Fatal("expected non-nil hub")
	}
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, unsub := h.Subscribe(ctx)
	defer unsub()

	pub, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("publisher connect: %v", err)
	}
	defer pub.Close()

	payload, _ := json.Marshal(map[string]any{
		"event_type": "thinking",
		"actor":      "gardenio-e2e-1",
		"text":       "let me verify the build",
		"project":    "gardenio",
	})
	if err := pub.Publish("coop.events.messages.gardenio-e2e-1.thinking", payload); err != nil {
		t.Fatalf("publish: %v", err)
	}
	_ = pub.Flush()

	select {
	case evt := <-ch:
		if evt.Agent != "gardenio-e2e-1" {
			t.Errorf("Agent = %q", evt.Agent)
		}
		if evt.Text != "let me verify the build" {
			t.Errorf("Text = %q", evt.Text)
		}
		if evt.Project != "gardenio" {
			t.Errorf("Project = %q", evt.Project)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not receive thinking event end-to-end within 5s")
	}
}

// TestThinkingHub_EndToEndIgnoresNonThinking confirms the wildcard subscription
// is scoped to *.thinking and never sees assistant_text/usage subjects.
func TestThinkingHub_EndToEndIgnoresNonThinking(t *testing.T) {
	url := startEmbeddedNATS(t)
	h := NewThinkingHub(url, "", "", nil)
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, unsub := h.Subscribe(ctx)
	defer unsub()

	pub, _ := nats.Connect(url)
	defer pub.Close()

	// An assistant_text event must not arrive on the thinking feed.
	at, _ := json.Marshal(map[string]any{"actor": "a", "text": "visible text"})
	_ = pub.Publish("coop.events.messages.a.assistant_text", at)
	// A thinking event to confirm the subscription is live.
	th, _ := json.Marshal(map[string]any{"actor": "a", "text": "real thinking"})
	_ = pub.Publish("coop.events.messages.a.thinking", th)
	_ = pub.Flush()

	select {
	case evt := <-ch:
		if evt.Text != "real thinking" {
			t.Errorf("received unexpected event %+v (assistant_text leaked?)", evt)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not receive the thinking event")
	}

	// Nothing else should be queued.
	select {
	case evt := <-ch:
		t.Errorf("unexpected second event: %+v", evt)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestThinkingHub_BackfillFromJetStream verifies that a freshly-started hub
// replays retained thinking events from the MESSAGE_EVENTS JetStream stream
// into its ring, so a (re)started viewer always has something to paint even
// before any new live event arrives. This is the core "always populate w
// something across restarts" guarantee (ka-0000005574).
func TestThinkingHub_BackfillFromJetStream(t *testing.T) {
	url := startEmbeddedJetStreamNATS(t)

	// Pre-publish retained thinking events to the stream BEFORE the hub starts.
	pub, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	js, err := pub.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	for _, a := range []string{"alpha", "bravo"} {
		b, _ := json.Marshal(map[string]any{"actor": a, "text": "retained thought from " + a})
		if _, err := js.Publish("coop.events.messages."+a+".thinking", b); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}
	// A non-thinking event on the same stream must NOT be backfilled.
	at, _ := json.Marshal(map[string]any{"actor": "alpha", "text": "assistant text"})
	if _, err := js.Publish("coop.events.messages.alpha.assistant_text", at); err != nil {
		t.Fatalf("publish: %v", err)
	}
	pub.Close()

	// Start the hub — backfill runs inside Start.
	h := NewThinkingHub(url, "", "", nil)
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Close()

	recent := h.Recent()
	if len(recent) != 2 {
		t.Fatalf("Recent() = %d events, want 2 (one thinking per agent); got %+v", len(recent), recent)
	}
	seen := map[string]string{}
	for _, e := range recent {
		seen[e.Agent] = e.Text
	}
	if seen["alpha"] != "retained thought from alpha" {
		t.Errorf("alpha backfill = %q", seen["alpha"])
	}
	if seen["bravo"] != "retained thought from bravo" {
		t.Errorf("bravo backfill = %q", seen["bravo"])
	}

	// A new subscriber must be seeded with the backfilled ring immediately.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, unsub := h.Subscribe(ctx)
	defer unsub()
	got := 0
	for got < 2 {
		select {
		case <-ch:
			got++
		case <-time.After(2 * time.Second):
			t.Fatalf("subscriber only received %d backfilled events, want 2", got)
		}
	}
}

// TestThinkingHub_BackfillNoStreamIsLiveOnly verifies the backfill degrades
// gracefully when JetStream / the stream is absent: Start succeeds and the
// live feed still works.
// An assistant text block on the say subject reaches OnSay with its agent bead and text, and is neither ringed nor on the
// telepathy feed; one with no agent bead or no text is dropped (gd-HHGjxt59Mr).
func TestThinkingHub_SayReachesOnSay(t *testing.T) {
	url := startEmbeddedNATS(t)
	h := NewThinkingHub(url, "", "", nil)
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Close()
	said := make(chan [2]string, 4)
	h.OnSay(func(id, text string) { said <- [2]string{id, text} })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	feed, unsub := h.Subscribe(ctx)
	defer unsub()

	pub, _ := nats.Connect(url)
	defer pub.Close()
	for _, m := range []map[string]any{
		{"actor": "a", "agent_bead_id": "", "text": "no bead"},
		{"actor": "a", "agent_bead_id": "gd-a", "text": ""},
		{"actor": "a", "agent_bead_id": "gd-a", "text": "Pushed the fix."},
	} {
		b, _ := json.Marshal(m)
		_ = pub.Publish("coop.events.messages.a.assistant_text", b)
	}
	_ = pub.Flush()

	select {
	case got := <-said:
		if got != [2]string{"gd-a", "Pushed the fix."} {
			t.Errorf("OnSay got %v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OnSay was not told")
	}
	select {
	case got := <-said:
		t.Errorf("a say with no bead or text reached OnSay: %v", got)
	case evt := <-feed:
		t.Errorf("a say reached the telepathy feed: %+v", evt)
	case <-time.After(200 * time.Millisecond):
	}
	if got := h.Recent(); len(got) != 0 {
		t.Errorf("a say was ringed: %+v", got)
	}
}

func TestThinkingHub_BackfillNoStreamIsLiveOnly(t *testing.T) {
	url := startEmbeddedNATS(t) // no JetStream
	h := NewThinkingHub(url, "", "", nil)
	if err := h.Start(); err != nil {
		t.Fatalf("Start should succeed without JetStream: %v", err)
	}
	defer h.Close()

	if got := h.Recent(); len(got) != 0 {
		t.Fatalf("Recent() = %d, want 0 (nothing to backfill)", len(got))
	}

	// Live feed still works.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, unsub := h.Subscribe(ctx)
	defer unsub()

	pub, _ := nats.Connect(url)
	defer pub.Close()
	th, _ := json.Marshal(map[string]any{"actor": "a", "text": "live thinking"})
	_ = pub.Publish("coop.events.messages.a.thinking", th)
	_ = pub.Flush()

	select {
	case evt := <-ch:
		if evt.Text != "live thinking" {
			t.Errorf("got %+v", evt)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not receive live thinking event")
	}
}
