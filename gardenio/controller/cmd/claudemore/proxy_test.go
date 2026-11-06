package main

import (
	"bufio"
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestNewProxy_RejectsBadURL(t *testing.T) {
	cases := []string{"", "   ", "not-a-url", "://missing-scheme", "http://"}
	for _, c := range cases {
		if _, err := newProxy(c, nil, testLogger()); err == nil {
			t.Errorf("newProxy(%q): expected error, got nil", c)
		}
	}
}

// TestProxy_PassthroughTransparent is the A1 transparency property: the request
// reaches the upstream unchanged (method, path, query, body, auth + litellm
// headers) and the response is returned byte-for-byte.
func TestProxy_PassthroughTransparent(t *testing.T) {
	var (
		gotMethod, gotPath, gotQuery, gotBody string
		gotAuth, gotLiteLLMKey, gotTags       string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		gotAuth = r.Header.Get("Authorization")
		gotLiteLLMKey = r.Header.Get("x-litellm-api-key")
		gotTags = r.Header.Get("x-litellm-tags")
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Upstream-Marker", "litellm")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant"}`))
	}))
	defer upstream.Close()

	p, err := newProxy(upstream.URL, nil, testLogger())
	if err != nil {
		t.Fatalf("newProxy: %v", err)
	}
	srv := httptest.NewServer(p)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages?beta=true",
		strings.NewReader(`{"model":"anthropic-claude","messages":[]}`))
	req.Header.Set("Authorization", "Bearer sk-ant-oat-xyz")
	req.Header.Set("x-litellm-api-key", "Bearer sk-proj-key")
	req.Header.Set("x-litellm-tags", "agent:test-agent")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if gotMethod != http.MethodPost {
		t.Errorf("method: got %q want POST", gotMethod)
	}
	if gotPath != "/v1/messages" {
		t.Errorf("path: got %q want /v1/messages", gotPath)
	}
	if gotQuery != "beta=true" {
		t.Errorf("query: got %q want beta=true", gotQuery)
	}
	if gotBody != `{"model":"anthropic-claude","messages":[]}` {
		t.Errorf("body not forwarded verbatim: got %q", gotBody)
	}
	// litellm's spend tagging + OAuth forwarding depend on these headers
	// surviving the hop unchanged (cost fence §5.2).
	if gotAuth != "Bearer sk-ant-oat-xyz" {
		t.Errorf("Authorization not forwarded: got %q", gotAuth)
	}
	if gotLiteLLMKey != "Bearer sk-proj-key" {
		t.Errorf("x-litellm-api-key not forwarded: got %q", gotLiteLLMKey)
	}
	if gotTags != "agent:test-agent" {
		t.Errorf("x-litellm-tags not forwarded: got %q", gotTags)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: got %d want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Upstream-Marker"); got != "litellm" {
		t.Errorf("upstream response header dropped: got %q", got)
	}
	if string(respBody) != `{"id":"msg_1","type":"message","role":"assistant"}` {
		t.Errorf("response body not byte-for-byte: got %q", respBody)
	}
}

// TestProxy_UpstreamErrorReturnsBadGateway verifies a dead upstream produces a
// clean Anthropic-shaped error rather than a hang or a 500 leak.
func TestProxy_UpstreamErrorReturnsBadGateway(t *testing.T) {
	// Point at a closed port: connection refused on the dial.
	p, err := newProxy("http://127.0.0.1:1", nil, testLogger())
	if err != nil {
		t.Fatalf("newProxy: %v", err)
	}
	srv := httptest.NewServer(p)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status: got %d want 502", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), `"type":"error"`) {
		t.Errorf("error body not Anthropic-shaped: got %q", b)
	}
}

// TestProxy_StreamingFlushesIncrementally proves SSE deltas are flushed as they
// arrive (FlushInterval -1) rather than buffered — the property the real
// `claude` binary's stream parser depends on.
func TestProxy_StreamingFlushesIncrementally(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("upstream ResponseWriter is not a Flusher")
			return
		}
		_, _ = io.WriteString(w, "event: message_start\ndata: {}\n\n")
		fl.Flush()
		<-release // hold the stream open until the client has seen chunk 1
		_, _ = io.WriteString(w, "event: message_stop\ndata: {}\n\n")
		fl.Flush()
	}))
	defer upstream.Close()

	p, err := newProxy(upstream.URL, nil, testLogger())
	if err != nil {
		t.Fatalf("newProxy: %v", err)
	}
	srv := httptest.NewServer(p)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/messages")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	br := bufio.NewReader(resp.Body)
	// First chunk must arrive before we release the upstream — i.e. it was
	// flushed, not buffered to end-of-stream.
	type readResult struct {
		line string
		err  error
	}
	ch := make(chan readResult, 1)
	go func() {
		line, err := br.ReadString('\n')
		ch <- readResult{line, err}
	}()
	select {
	case rr := <-ch:
		if rr.err != nil {
			t.Fatalf("reading first chunk: %v", rr.err)
		}
		if !strings.Contains(rr.line, "message_start") {
			t.Errorf("first flushed chunk: got %q", rr.line)
		}
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("first SSE chunk not flushed within 2s — response is being buffered")
	}
	close(release)
}

// recordingRecorder captures exchanges for the seam test.
type recordingRecorder struct {
	mu  sync.Mutex
	exs []Exchange
}

func (r *recordingRecorder) RecordExchange(ex Exchange) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.exs = append(r.exs, ex)
}

// len returns the number of exchanges observed so far, under the lock.
func (r *recordingRecorder) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.exs)
}

// waitForExchanges blocks until the recorder has observed at least n exchanges,
// or the deadline elapses, and reports whether the count was reached. The
// recorder seam fires from the ReverseProxy's response-body onDone callback
// (proxy.modifyResponse), which runs on the proxy's server goroutine AFTER the
// body is fully streamed and closed — there is no happens-before edge from that
// callback to the client's http.Post returning. So asserting the exchange count
// IMMEDIATELY after the client read races the recorder goroutine: under load
// (-parallel with atomic-coverage instrumentation) the assertion can run
// first and see 0, reddening CI on a correct code path (gd-3d0yv8g3Dz). A short
// bounded poll closes that race without weakening the assertion — the exchange
// still must arrive, just not synchronously with the client read.
func (r *recordingRecorder) waitForExchanges(n int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if r.len() >= n {
			return true
		}
		if time.Now().After(deadline) {
			return r.len() >= n
		}
		time.Sleep(time.Millisecond)
	}
}

// TestProxy_RecorderSeamObservesWithoutMutating verifies an attached recorder
// sees the exchange but does NOT alter the client-visible response — the
// "augments, never changes behavior" invariant.
func TestProxy_RecorderSeamObservesWithoutMutating(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("upstream-body"))
	}))
	defer upstream.Close()

	rec := &recordingRecorder{}
	p, err := newProxy(upstream.URL, rec, testLogger())
	if err != nil {
		t.Fatalf("newProxy: %v", err)
	}
	srv := httptest.NewServer(p)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusCreated {
		t.Errorf("recorder mutated status: got %d want 201", resp.StatusCode)
	}
	if string(body) != "upstream-body" {
		t.Errorf("recorder mutated body: got %q", body)
	}
	// The recorder observes the exchange from the response-body onDone callback,
	// which is NOT synchronized with the client read above — wait (bounded) for it
	// to land rather than racing it (gd-3d0yv8g3Dz). 2s is generously past the
	// microseconds the in-process callback actually needs, so a real regression
	// (the recorder never fires) still fails fast, while CI load jitter no longer
	// reds a correct path.
	if !rec.waitForExchanges(1, 2*time.Second) {
		t.Fatalf("recorder saw %d exchanges after wait, want 1", rec.len())
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.exs) != 1 {
		t.Fatalf("recorder saw %d exchanges, want 1", len(rec.exs))
	}
	if rec.exs[0].StatusCode != http.StatusCreated || rec.exs[0].Path != "/v1/messages" {
		t.Errorf("recorded exchange wrong: %+v", rec.exs[0])
	}
}

// serverBody mimics the server's inbound request body: reads after Close fail
// with http.ErrBodyReadAfterClose.
type serverBody struct {
	r      io.Reader
	closed bool
}

func (b *serverBody) Read(p []byte) (int, error) {
	if b.closed {
		return 0, http.ErrBodyReadAfterClose
	}
	return b.r.Read(p)
}

func (b *serverBody) Close() error { b.closed = true; return nil }

// TestTeeReadCloser_StickyEOFAfterServerDrain pins the gd-0A9m1x6uFQ race fix:
// after the tee has returned EOF, the server may close the inbound body (its
// post-handler drain runs when the response header is written), and the
// Transport's confirming read must still see io.EOF, not ErrBodyReadAfterClose
// (which would make it close the upstream connection mid-response).
func TestTeeReadCloser_StickyEOFAfterServerDrain(t *testing.T) {
	in := &serverBody{r: strings.NewReader(`{"model":"m"}`)}
	var captured bytes.Buffer
	tee := &teeReadCloser{r: io.TeeReader(in, &captured), closer: in}
	got, err := io.ReadAll(tee)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != `{"model":"m"}` || captured.String() != `{"model":"m"}` {
		t.Fatalf("forwarded %q, captured %q", got, captured.String())
	}
	_ = in.Close() // the server drains and closes the body
	if n, err := tee.Read(make([]byte, 8)); n != 0 || err != io.EOF {
		t.Fatalf("read after drain: n=%d err=%v, want 0, io.EOF", n, err)
	}
}
