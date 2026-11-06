package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/kbeads/internal/eventbus"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

// --- handleBusStatus tests ---

func TestBusStatusNoBus(t *testing.T) {
	_, _, handler := newTestServer()
	rec := doJSON(t, handler, "GET", "/v1/bus/status", nil)
	requireStatus(t, rec, 200)

	var resp busStatusResponse
	decodeJSON(t, rec, &resp)

	if resp.JetStreamEnabled {
		t.Error("expected jetstream_enabled=false when bus is nil")
	}
	if resp.HandlerCount != 0 {
		t.Errorf("handler_count = %d, want 0", resp.HandlerCount)
	}
	if len(resp.Streams) != len(eventbus.StreamNames) {
		t.Errorf("streams count = %d, want %d", len(resp.Streams), len(eventbus.StreamNames))
	}
	if len(resp.Handlers) != 0 {
		t.Error("handlers should be empty when bus is nil")
	}
}

func TestBusStatusWithBusNoJetStream(t *testing.T) {
	s, _, handler := newTestServer()
	bus := eventbus.New()
	s.SetBus(bus)

	rec := doJSON(t, handler, "GET", "/v1/bus/status", nil)
	requireStatus(t, rec, 200)

	var resp busStatusResponse
	decodeJSON(t, rec, &resp)

	if resp.JetStreamEnabled {
		t.Error("expected jetstream_enabled=false when JetStream not set")
	}
	if resp.HandlerCount != 0 {
		t.Errorf("handler_count = %d, want 0", resp.HandlerCount)
	}
}

func TestBusStatusWithHandlers(t *testing.T) {
	s, _, handler := newTestServer()
	bus := eventbus.New()
	bus.Register(&busStubHandler{id: "handler-a"})
	bus.Register(&busStubHandler{id: "handler-b"})
	s.SetBus(bus)

	rec := doJSON(t, handler, "GET", "/v1/bus/status", nil)
	requireStatus(t, rec, 200)

	var resp busStatusResponse
	decodeJSON(t, rec, &resp)

	if resp.HandlerCount != 2 {
		t.Errorf("handler_count = %d, want 2", resp.HandlerCount)
	}
	if len(resp.Handlers) != 2 {
		t.Errorf("handlers len = %d, want 2", len(resp.Handlers))
	}

	ids := map[string]bool{}
	for _, h := range resp.Handlers {
		ids[h] = true
	}
	if !ids["handler-a"] || !ids["handler-b"] {
		t.Errorf("handlers = %v, want handler-a and handler-b", resp.Handlers)
	}
}

func TestBusStatusStreamsAlwaysPresent(t *testing.T) {
	_, _, handler := newTestServer()
	rec := doJSON(t, handler, "GET", "/v1/bus/status", nil)
	requireStatus(t, rec, 200)

	var resp busStatusResponse
	decodeJSON(t, rec, &resp)

	expected := map[string]bool{
		"hooks": true, "decisions": true, "agents": true,
		"mail": true, "mutations": true, "config": true,
		"gate": true, "inbox": true, "jack": true, "messages": true,
		"plan": true,
	}
	for _, s := range resp.Streams {
		if !expected[s] {
			t.Errorf("unexpected stream name %q", s)
		}
		delete(expected, s)
	}
	for s := range expected {
		t.Errorf("missing stream name %q", s)
	}
}

// --- handleBusEvents error path tests ---

func TestBusEventsNoBus(t *testing.T) {
	_, _, handler := newTestServer()
	rec := doJSON(t, handler, "GET", "/v1/bus/events", nil)
	requireStatus(t, rec, 503)

	var resp map[string]string
	decodeJSON(t, rec, &resp)
	if !strings.Contains(resp["error"], "JetStream not configured") {
		t.Errorf("error = %q, want JetStream not configured message", resp["error"])
	}
}

func TestBusEventsNoJetStream(t *testing.T) {
	s, _, handler := newTestServer()
	s.SetBus(eventbus.New()) // bus without JetStream

	rec := doJSON(t, handler, "GET", "/v1/bus/events", nil)
	requireStatus(t, rec, 503)
}

func TestBusEventsInvalidStream(t *testing.T) {
	ns, js := startTestNATS(t)
	_ = ns
	s, _, handler := newTestServer()
	bus := eventbus.New()
	bus.SetJetStream(js)
	s.SetBus(bus)

	rec := doJSON(t, handler, "GET", "/v1/bus/events?stream=bogus,invalid", nil)
	requireStatus(t, rec, 400)

	var resp map[string]string
	decodeJSON(t, rec, &resp)
	if !strings.Contains(resp["error"], "no valid stream names") {
		t.Errorf("error = %q, want 'no valid stream names'", resp["error"])
	}
}

// --- handleBusEvents streaming tests (require embedded NATS) ---

func TestBusEventsStreamHookEvent(t *testing.T) {
	w := newSSEWriter()
	js, stop := serveBusSSE(t, "/v1/bus/events?stream=hooks", w, 1)

	// Publish an event to the hooks stream.
	_, err := js.Publish("hooks.test-agent.SessionStart", []byte(`{"type":"SessionStart"}`))
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}

	w.waitForEvents(1)

	stop()

	body := w.body()
	if !strings.Contains(body, "event:hooks") {
		t.Errorf("SSE body should contain event:hooks, got:\n%s", body)
	}
	if !strings.Contains(body, `"stream":"hooks"`) {
		t.Errorf("SSE body should contain stream:hooks, got:\n%s", body)
	}
	if !strings.Contains(body, `"type":"SessionStart"`) {
		t.Errorf("SSE body should contain type:SessionStart, got:\n%s", body)
	}
}

func TestBusEventsStreamFilter(t *testing.T) {
	w := newSSEWriter()
	js, stop := serveBusSSE(t, "/v1/bus/events?stream=hooks&filter=Stop", w, 1)

	// Publish two events: one that matches the filter, one that doesn't.
	_, _ = js.Publish("hooks.agent1.SessionStart", []byte(`{"type":"SessionStart"}`))
	_, _ = js.Publish("hooks.agent1.Stop", []byte(`{"type":"Stop"}`))

	w.waitForEvents(1)

	stop()

	body := w.body()
	if !strings.Contains(body, `"type":"Stop"`) {
		t.Errorf("SSE body should contain filtered event Stop, got:\n%s", body)
	}
	if strings.Contains(body, `"type":"SessionStart"`) {
		t.Errorf("SSE body should NOT contain SessionStart (filtered out), got:\n%s", body)
	}
}

func TestBusEventsMultipleStreams(t *testing.T) {
	w := newSSEWriter()
	js, stop := serveBusSSE(t, "/v1/bus/events?stream=hooks,decisions", w, 2)

	_, _ = js.Publish("hooks.agent1.SessionStart", []byte(`{"type":"SessionStart"}`))
	_, _ = js.Publish("decisions.agent1.DecisionCreated", []byte(`{"type":"DecisionCreated"}`))

	w.waitForEvents(2)

	stop()

	body := w.body()
	if !strings.Contains(body, "event:hooks") {
		t.Errorf("SSE body should contain event:hooks, got:\n%s", body)
	}
	if !strings.Contains(body, "event:decisions") {
		t.Errorf("SSE body should contain event:decisions, got:\n%s", body)
	}
}

func TestBusEventsSSEHeaders(t *testing.T) {
	ns, js := startTestNATS(t)
	_ = ns
	if err := eventbus.EnsureStreams(js); err != nil {
		t.Fatalf("EnsureStreams: %v", err)
	}

	s, _, handler := newTestServer()
	bus := eventbus.New()
	bus.SetJetStream(js)
	s.SetBus(bus)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("GET", "/v1/bus/events?stream=hooks", nil)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(rec, req)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", cc)
	}
	if xab := rec.Header().Get("X-Accel-Buffering"); xab != "no" {
		t.Errorf("X-Accel-Buffering = %q, want no", xab)
	}
}

func TestBusEventsSSEFormat(t *testing.T) {
	w := newSSEWriter()
	js, stop := serveBusSSE(t, "/v1/bus/events?stream=hooks", w, 1)

	_, _ = js.Publish("hooks.agent1.Stop", []byte(`{"stopped":true}`))

	w.waitForEvents(1)
	stop()

	body := w.body()

	// Verify SSE format: id:<n>\nevent:<stream>\ndata:<json>\n\n
	if !strings.Contains(body, "id:1\n") {
		t.Errorf("SSE body should contain id:1, got:\n%s", body)
	}
	if !strings.Contains(body, "event:hooks\n") {
		t.Errorf("SSE body should contain event:hooks, got:\n%s", body)
	}
	if !strings.Contains(body, "data:") {
		t.Errorf("SSE body should contain data: prefix, got:\n%s", body)
	}

	// Extract and verify the JSON data payload.
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "data:") {
			var evt busSSEEvent
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data:")), &evt); err != nil {
				t.Fatalf("failed to parse SSE data payload: %v", err)
			}
			if evt.Stream != "hooks" {
				t.Errorf("payload stream = %q, want hooks", evt.Stream)
			}
			if evt.Type != "Stop" {
				t.Errorf("payload type = %q, want Stop", evt.Type)
			}
			if evt.Subject != "hooks.agent1.Stop" {
				t.Errorf("payload subject = %q, want hooks.agent1.Stop", evt.Subject)
			}
			return
		}
	}
	t.Error("no data: line found in SSE body")
}

func TestBusEventsStreamMessageEvents(t *testing.T) {
	w := newSSEWriter()
	js, stop := serveBusSSE(t, "/v1/bus/events?stream=messages", w, 1)

	// Publish a message event to the messages stream (coop.events.messages.>).
	_, err := js.Publish("coop.events.messages.bright-hog.assistant_text", []byte(`{"type":"assistant_text","text":"hello world","agent":"bright-hog"}`))
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}

	w.waitForEvents(1)
	stop()

	body := w.body()
	if !strings.Contains(body, "event:messages") {
		t.Errorf("SSE body should contain event:messages, got:\n%s", body)
	}
	if !strings.Contains(body, `"type":"assistant_text"`) {
		t.Errorf("SSE body should contain type:assistant_text, got:\n%s", body)
	}
	if !strings.Contains(body, `"stream":"messages"`) {
		t.Errorf("SSE body should contain stream:messages, got:\n%s", body)
	}
}

func TestBusEventsStreamAllDefault(t *testing.T) {
	// No stream param = all streams.
	w := newSSEWriter()
	js, stop := serveBusSSE(t, "/v1/bus/events", w, len(eventbus.StreamNames))

	// Publish to mutations stream (should be subscribed by default).
	_, _ = js.Publish("mutations.MutationCreate", []byte(`{"issue_id":"gd-123"}`))

	w.waitForEvents(1)
	stop()

	body := w.body()
	if !strings.Contains(body, "event:mutations") {
		t.Errorf("SSE body should contain event:mutations when subscribing to all, got:\n%s", body)
	}
}

// TestBusEventsReplayDeliversEveryEvent is the regression for gd-P7jMtvwJCk:
// a since= replay of a backlog larger than the handler's 128-slot channel,
// read by a client that stalls, must deliver every event rather than drop
// whatever arrives while the channel is full.
func TestBusEventsReplayDeliversEveryEvent(t *testing.T) {
	_, js := startTestNATS(t)
	if err := eventbus.EnsureStreams(js); err != nil {
		t.Fatalf("EnsureStreams: %v", err)
	}

	s, _, handler := newTestServer()
	bus := eventbus.New()
	bus.SetJetStream(js)
	s.SetBus(bus)

	start := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	const n = 1000
	for i := 0; i < n; i++ {
		if _, err := js.Publish(fmt.Sprintf("hooks.replay-agent.Event%d", i), []byte(`{}`)); err != nil {
			t.Fatalf("Publish %d: %v", i, err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest("GET", "/v1/bus/events?stream=hooks&since="+start, nil)
	req = req.WithContext(ctx)
	w := newGatedSSEWriter()

	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(w, req)
	}()

	// Hold the client still long enough for the whole backlog to reach the
	// subscription while the handler is stuck writing the first event.
	time.Sleep(500 * time.Millisecond)
	w.open()

	deadline := time.Now().Add(10 * time.Second)
	for w.events() < n && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done

	if got := w.events(); got != n {
		t.Fatalf("replay delivered %d events, want %d", got, n)
	}
	if d := w.dropped(); d != 0 {
		t.Errorf("replay reported %d dropped events, want 0", d)
	}
	// Ordered delivery: the replay emits the stream in sequence order.
	body := w.body()
	if i0, i1 := strings.Index(body, `"Event0"`), strings.Index(body, `"Event999"`); i0 < 0 || i1 < 0 || i0 > i1 {
		t.Errorf("replay out of order or incomplete: Event0 at %d, Event999 at %d", i0, i1)
	}
}

// TestBusEventsLiveReportsDrops: a live tail may still drop events a stalled
// client cannot keep up with, but it says so with ":dropped N" comments, and
// delivered plus reported-dropped accounts for every published event.
func TestBusEventsLiveReportsDrops(t *testing.T) {
	w := newGatedSSEWriter()
	js, stop := serveBusSSE(t, "/v1/bus/events?stream=hooks", w, 1)
	t.Cleanup(w.open) // runs before stop, which would block on the gate

	const n = 1000
	for i := 0; i < n; i++ {
		if _, err := js.Publish(fmt.Sprintf("hooks.live-agent.Event%d", i), []byte(`{}`)); err != nil {
			t.Fatalf("Publish %d: %v", i, err)
		}
	}
	time.Sleep(500 * time.Millisecond)
	w.open()

	deadline := time.Now().Add(10 * time.Second)
	for w.events()+w.dropped() < n && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	stop()

	events, dropped := w.events(), w.dropped()
	if dropped == 0 {
		t.Errorf("stalled live tail reported no drops (delivered %d of %d)", events, n)
	}
	if events+dropped != n {
		t.Errorf("delivered %d + dropped %d = %d, want %d", events, dropped, events+dropped, n)
	}
}

// gatedSSEWriter is a concurrency-safe SSE ResponseWriter whose body writes
// block until open is called, standing in for a client that stalls.
type gatedSSEWriter struct {
	header http.Header
	gate   chan struct{}
	once   sync.Once
	mu     sync.Mutex
	buf    strings.Builder
}

func newGatedSSEWriter() *gatedSSEWriter {
	return &gatedSSEWriter{header: http.Header{}, gate: make(chan struct{})}
}

func (g *gatedSSEWriter) Header() http.Header { return g.header }
func (g *gatedSSEWriter) WriteHeader(int)     {}
func (g *gatedSSEWriter) Flush()              {}

func (g *gatedSSEWriter) Write(p []byte) (int, error) {
	<-g.gate
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.buf.Write(p)
}

func (g *gatedSSEWriter) open() { g.once.Do(func() { close(g.gate) }) }

func (g *gatedSSEWriter) body() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.buf.String()
}

// newSSEWriter is a concurrency-safe SSE ResponseWriter that never stalls,
// so a test can poll the body while the handler is still writing it.
func newSSEWriter() *gatedSSEWriter {
	g := newGatedSSEWriter()
	g.open()
	return g
}

// waitForEvents polls until at least n events are written or 10s pass; the
// caller's assertions report a shortfall with the body.
func (g *gatedSSEWriter) waitForEvents(n int) {
	deadline := time.Now().Add(10 * time.Second)
	for g.events() < n && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
}

// events counts SSE data lines written so far.
func (g *gatedSSEWriter) events() int {
	return strings.Count(g.body(), "\ndata:")
}

// dropped sums the ":dropped N" comments written so far.
func (g *gatedSSEWriter) dropped() int {
	total := 0
	for _, line := range strings.Split(g.body(), "\n") {
		if rest, ok := strings.CutPrefix(line, ":dropped "); ok {
			n, _ := strconv.Atoi(rest)
			total += n
		}
	}
	return total
}

// --- helpers ---

// startTestNATS creates an embedded NATS server with JetStream for testing.
func startTestNATS(t *testing.T) (*server.Server, nats.JetStreamContext) {
	t.Helper()
	opts := &server.Options{
		Host:      "127.0.0.1",
		Port:      -1,
		JetStream: true,
		StoreDir:  t.TempDir(),
	}
	ns, err := server.NewServer(opts)
	if err != nil {
		t.Fatalf("failed to create NATS server: %v", err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		t.Fatal("NATS server not ready")
	}
	t.Cleanup(func() { ns.Shutdown() })

	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("failed to connect to NATS: %v", err)
	}
	t.Cleanup(func() { nc.Close() })

	js, err := nc.JetStream()
	if err != nil {
		t.Fatalf("failed to create JetStream context: %v", err)
	}
	return ns, js
}

// serveBusSSE serves path from the bus SSE handler into w, backed by a fresh
// embedded JetStream, and returns once the handler has made consumers
// JetStream consumers. stop cancels the request and waits for the handler.
func serveBusSSE(t *testing.T, path string, w http.ResponseWriter, consumers int) (nats.JetStreamContext, func()) {
	t.Helper()
	_, js := startTestNATS(t)
	if err := eventbus.EnsureStreams(js); err != nil {
		t.Fatalf("EnsureStreams: %v", err)
	}
	s, _, handler := newTestServer()
	bus := eventbus.New()
	bus.SetJetStream(js)
	s.SetBus(bus)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(w, httptest.NewRequest("GET", path, nil).WithContext(ctx))
	}()
	stop := func() { cancel(); <-done }
	t.Cleanup(stop)
	waitForBusConsumers(t, js, consumers)
	return js, stop
}

// waitForBusConsumers blocks until want JetStream consumers exist across all
// streams, i.e. the bus SSE handler has made its subscriptions. They are
// DeliverNew, so an event published before its consumer exists never arrives
// (gd-Wx3iAlGkVe). EnsureStreams makes no consumers, so the count starts at 0.
func waitForBusConsumers(t *testing.T, js nats.JetStreamContext, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got := 0
		for info := range js.Streams() {
			got += info.State.Consumers
		}
		if got >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("bus SSE handler made %d JetStream consumers in 10s, want %d", got, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// busStubHandler is a minimal eventbus.Handler for testing bus status.
type busStubHandler struct {
	id string
}

func (h *busStubHandler) ID() string                    { return h.id }
func (h *busStubHandler) Handles() []eventbus.EventType { return nil }
func (h *busStubHandler) Priority() int                 { return 0 }
func (h *busStubHandler) Handle(_ context.Context, _ *eventbus.Event, _ *eventbus.Result) error {
	return fmt.Errorf("not implemented")
}
