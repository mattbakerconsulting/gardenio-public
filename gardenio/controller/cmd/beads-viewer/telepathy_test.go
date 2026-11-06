package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fixedReceivedAt is a stable display timestamp for row-render assertions.
func fixedReceivedAt(t *testing.T) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, "2026-06-11T21:30:05Z")
	if err != nil {
		t.Fatalf("parse fixed ts: %v", err)
	}
	return ts
}

func TestRenderThinkingRow_FullEvent(t *testing.T) {
	evt := ThinkingEvent{
		Agent:       "gardenio-telepathy-crew-q5t",
		Text:        "I should claim the bead before <editing>",
		FullLength:  80,
		Role:        "crew",
		Project:     "gardenio",
		AgentBeadID: "gd-abc123",
		PodName:     "gardenio-telepathy-crew-q5t-abc",
		Source:      "litellm-callback",
		ReceivedAt:  fixedReceivedAt(t),
	}
	row := renderThinkingRow("/base", evt)

	for _, want := range []string{
		`class="telepathy-event"`,
		`data-agent="gardenio-telepathy-crew-q5t"`,
		`--agent-hue:`,
		"2026-06-11T21:30:05",
		"21:30:05",
		// AgentBeadID present → deep-link to the agent bead, not /agents.
		`href="/base/gd-abc123"`,
		">crew<",
		">gardenio<",
		// Text is HTML-escaped — angle brackets must not survive raw.
		"I should claim the bead before &lt;editing&gt;",
	} {
		if !strings.Contains(row, want) {
			t.Errorf("row missing %q\nrow=%s", want, row)
		}
	}
	if strings.Contains(row, "<editing>") {
		t.Errorf("unescaped angle brackets leaked into row: %s", row)
	}
	// FullLength (80) > len(Text) → a clip hint is shown.
	if !strings.Contains(row, "telepathy-clip") {
		t.Errorf("expected clip hint for clipped trace, row=%s", row)
	}
}

func TestRenderThinkingRow_NoBeadLinkFallsBackToAgents(t *testing.T) {
	row := renderThinkingRow("/base", ThinkingEvent{Agent: "agent-x", Text: "hi", ReceivedAt: fixedReceivedAt(t)})
	if !strings.Contains(row, `href="/base/agents"`) {
		t.Errorf("expected /agents fallback when no AgentBeadID, row=%s", row)
	}
}

func TestRenderThinkingRow_EmptyTextFallback(t *testing.T) {
	row := renderThinkingRow("", ThinkingEvent{Agent: "agent-x", ReceivedAt: fixedReceivedAt(t)})
	if !strings.Contains(row, "telepathy-empty") {
		t.Errorf("empty text should render the empty-state em, row=%s", row)
	}
	if strings.Contains(row, "telepathy-clip") {
		t.Errorf("no clip hint expected for empty text, row=%s", row)
	}
}

func TestRenderThinkingRow_NameFallbackUnknown(t *testing.T) {
	row := renderThinkingRow("", ThinkingEvent{ReceivedAt: fixedReceivedAt(t)})
	if !strings.Contains(row, `data-agent="unknown"`) {
		t.Errorf("expected unknown fallback, row=%s", row)
	}
}

func TestThinkingEventTime_Precedence(t *testing.T) {
	rcv := fixedReceivedAt(t)
	// ReceivedAt wins when set.
	if got := thinkingEventTime(ThinkingEvent{ReceivedAt: rcv, TS: 1}); !got.Equal(rcv) {
		t.Errorf("expected ReceivedAt, got %v", got)
	}
	// Falls back to producer TS (unix millis) when ReceivedAt is zero.
	ms := rcv.UnixMilli()
	if got := thinkingEventTime(ThinkingEvent{TS: ms}); got.UnixMilli() != ms {
		t.Errorf("expected TS-derived time %d, got %d", ms, got.UnixMilli())
	}
}

func TestAgentHue_StableAndBounded(t *testing.T) {
	a := agentHue("gardenio-foo")
	b := agentHue("gardenio-foo")
	if a != b {
		t.Errorf("agentHue not stable: %d != %d", a, b)
	}
	if a < 0 || a >= 360 {
		t.Errorf("agentHue out of range: %d", a)
	}
	if agentHue("alpha") == agentHue("beta") && agentHue("gamma") == agentHue("delta") {
		t.Errorf("agentHue collapsed distinct names onto one hue")
	}
}

func TestHandleTelepathy_PageEnabled(t *testing.T) {
	f := newFake()
	srv := NewServer(f, testLogger(), "", "", "", &fakeScreenClient{})
	srv.SetThinkingHub(newTestHub())
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/telepathy", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`class="telepathy-page"`,
		`data-events-url="/telepathy/events"`,
		"telepathy.js",
		"auto-scroll",
		// Demo polish (gd-QeufU35VlI): a pause control to freeze the feed.
		`class="telepathy-pause"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q", want)
		}
	}
	if strings.Contains(body, "Thinking feed is disabled") {
		t.Errorf("did not expect disabled banner when feed is wired")
	}
}

func TestHandleTelepathy_PageDisabledBanner(t *testing.T) {
	f := newFake()
	srv := NewServer(f, testLogger(), "", "", "", &fakeScreenClient{})
	// No SetThinkingHub → nil hub.
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/telepathy", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Thinking feed is disabled") {
		t.Errorf("expected disabled banner when hub is nil")
	}
}

func TestHandleTelepathyEvents_StreamsBacklogAndLive(t *testing.T) {
	f := newFake()
	hub := newTestHub()
	// Seed the ring so a freshly-connected client is backfilled.
	hub.broadcast(ThinkingEvent{Agent: "backlog-agent", Text: "old thought"})

	srv := NewServer(f, testLogger(), "", "", "", &fakeScreenClient{})
	srv.SetThinkingHub(hub)
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/telepathy/events", nil)
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("GET /telepathy/events: %v", err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}

	// Push a live event after the client connected, then close the hub so the
	// handler unwinds and the body read completes.
	go func() {
		time.Sleep(150 * time.Millisecond)
		hub.broadcast(ThinkingEvent{Agent: "live-agent", Text: "new thought"})
		time.Sleep(150 * time.Millisecond)
		hub.Close()
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	text := string(body)
	for _, want := range []string{
		"event: thinking",
		"backlog-agent",
		"old thought",
		"live-agent",
		"new thought",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("stream missing %q\nstream=%s", want, text)
		}
	}
}

// TestWriteThinkingEvent_MultiLineNotFlattened guards the core promise of this
// feature: the ENTIRE thought reaches the browser without truncation OR loss of
// its line structure. A multi-line thought lands as raw newlines inside the
// rendered row (html.EscapeString does not touch "\n"). The SSE writer must
// emit those as repeated `data:` fields — the spec's multi-line idiom that the
// browser EventSource rejoins with "\n" — rather than collapsing them to one
// run-on line. We assert both: (a) every line survives verbatim, and (b) the
// payload uses one `data:` field per source line.
func TestWriteThinkingEvent_MultiLineNotFlattened(t *testing.T) {
	srv := NewServer(newFake(), testLogger(), "", "", "", &fakeScreenClient{})

	thought := "First I will read the file.\nThen I will edit it.\nFinally I will run the tests."
	evt := ThinkingEvent{Agent: "deep-thinker", Text: thought, ReceivedAt: fixedReceivedAt(t)}

	w := httptest.NewRecorder()
	srv.writeThinkingEvent(w, w, evt)
	out := w.Body.String()

	// Every line of the thought must be present verbatim — nothing clipped.
	for line := range strings.SplitSeq(thought, "\n") {
		if !strings.Contains(out, line) {
			t.Errorf("thought line missing from SSE payload: %q\nout=%s", line, out)
		}
	}

	// Reconstruct the message body per the SSE spec: strip the leading
	// "data: " from each data field and join with "\n". The rendered row must
	// then contain the full multi-line thought, proving newlines round-tripped.
	var dataLines []string
	for l := range strings.SplitSeq(strings.TrimRight(out, "\n"), "\n") {
		if rest, ok := strings.CutPrefix(l, "data: "); ok {
			dataLines = append(dataLines, rest)
		}
	}
	if len(dataLines) < 3 {
		t.Errorf("expected the multi-line thought to span >=3 data: fields, got %d\nout=%s", len(dataLines), out)
	}
	row := strings.Join(dataLines, "\n")
	if !strings.Contains(row, thought) {
		t.Errorf("reconstructed row lost the thought's newline structure\nrow=%s", row)
	}
}

func TestHandleTelepathyEvents_DisabledFeedComment(t *testing.T) {
	f := newFake()
	srv := NewServer(f, testLogger(), "", "", "", &fakeScreenClient{})
	// nil hub.
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/telepathy/events", nil)
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("GET /telepathy/events: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "feed disabled") {
		t.Errorf("expected 'feed disabled' comment, got %q", string(body))
	}
}
