package main

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/claudemore"
)

// memStore is an in-memory CassetteStore for the recorder e2e tests.
type memStore struct {
	mu    sync.Mutex
	saved []claudemore.Cassette
	err   error // if set, Put returns it
}

func (m *memStore) Put(_ context.Context, c claudemore.Cassette) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.saved = append(m.saved, c)
	return nil
}

func (m *memStore) Get(_ context.Context, _, _ string) (claudemore.Cassette, error) {
	return claudemore.Cassette{}, claudemore.ErrCassetteNotFound
}

func (m *memStore) all() []claudemore.Cassette {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]claudemore.Cassette, len(m.saved))
	copy(out, m.saved)
	return out
}

// newTestRecorder wires a recorder over a memStore with a synchronous
// store-completion signal so tests never sleep.
func newTestRecorder(t *testing.T, store claudemore.CassetteStore) (*cassetteRecorder, <-chan struct{}) {
	t.Helper()
	done := make(chan struct{}, 16)
	r := newCassetteRecorder(context.Background(), store, testLogger())
	r.onStored = func(error) { done <- struct{}{} }
	t.Cleanup(r.Close)
	return r, done
}

func waitStored(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for cassette store")
	}
}

// TestRecorder_CapturesExchangeToCassette is the A3 record-fidelity path end to
// end through the real proxy: a request flows through, the upstream responds, and
// a cassette lands in the store keyed by a stable hash.
func TestRecorder_CapturesExchangeToCassette(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"4"}]}`))
	}))
	defer upstream.Close()

	store := &memStore{}
	rec, done := newTestRecorder(t, store)
	p, err := newProxy(upstream.URL, rec, testLogger())
	if err != nil {
		t.Fatalf("newProxy: %v", err)
	}
	srv := httptest.NewServer(p)
	defer srv.Close()

	body := `{"model":"claude-sonnet-5","messages":[{"role":"user","content":"2+2"}]}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", strings.NewReader(body))
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("Authorization", "Bearer sk-ant-oat01-SHOULD-NOT-PERSIST-000")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	// Client still sees the upstream response byte-for-byte (A1).
	if !strings.Contains(string(respBody), `"text":"4"`) {
		t.Errorf("client response altered: %q", respBody)
	}

	waitStored(t, done)
	saved := store.all()
	if len(saved) != 1 {
		t.Fatalf("expected 1 cassette, got %d", len(saved))
	}
	c := saved[0]
	if c.Model != "claude-sonnet-5" {
		t.Errorf("model tag: got %q", c.Model)
	}
	if c.RequestHash == "" {
		t.Error("empty request hash")
	}
	if c.Response.StatusCode != 200 {
		t.Errorf("status: got %d", c.Response.StatusCode)
	}
	if !strings.Contains(string(c.Response.Body), `"text":"4"`) {
		t.Errorf("response body not captured: %q", c.Response.Body)
	}
	if c.Request.Method != "POST" || c.Request.Path != "/v1/messages" {
		t.Errorf("request identity wrong: %+v", c.Request)
	}
}

// TestRecorder_R4SEC3_NoSecretsInCassette is the A4 / R4-SEC-3 acceptance: on a
// deliberately key-echoing session (the agent pastes provider keys into the
// request AND the model echoes one back in the response), NO sk-ant-/sk-/vk-
// shaped secret survives anywhere in the persisted cassette.
func TestRecorder_R4SEC3_NoSecretsInCassette(t *testing.T) {
	// Upstream (model) echoes a key back in its completion — the exact leak
	// vector R4-SEC-3 describes for a real reasoning agent.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"here is the key you asked for: sk-ant-oat01-MODELECHO9999back"}]}`))
	}))
	defer upstream.Close()

	store := &memStore{}
	rec, done := newTestRecorder(t, store)
	p, _ := newProxy(upstream.URL, rec, testLogger())
	srv := httptest.NewServer(p)
	defer srv.Close()

	// The request body echoes several key shapes (a pasted transcript).
	reqBody := `{"model":"claude-sonnet-5","messages":[{"role":"user","content":` +
		`"my creds: sk-ant-oat01-REQUESTKEY1234abcd and vk-abcdEFGH01234567 and sk-1234567890abcdefABCDEF and ghp_aaaaBBBBccccDDDDeeee"}]}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", strings.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer sk-ant-oat01-HEADERKEY-secretsecret")
	req.Header.Set("x-litellm-api-key", "sk-9999888877776666aaaa")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	waitStored(t, done)
	saved := store.all()
	if len(saved) != 1 {
		t.Fatalf("expected 1 cassette, got %d", len(saved))
	}
	c := saved[0]

	// Check every DECODED surface a secret could hide in: the stored request
	// body (JSON, redaction visible), the raw response bytes (NOT base64 here),
	// and the header maps. Checking the marshaled cassette alone would be weak —
	// the response body base64-encodes and could mask a plaintext leak.
	surfaces := map[string][]byte{
		"request-body":  []byte(c.Request.Body),
		"response-body": c.Response.Body,
	}
	for k, v := range c.Request.Headers {
		surfaces["req-header:"+k] = []byte(v)
	}
	for k, v := range c.Response.Headers {
		surfaces["resp-header:"+k] = []byte(v)
	}

	leaks := []string{
		"REQUESTKEY1234abcd", "abcdEFGH01234567", "1234567890abcdefABCDEF",
		"aaaaBBBBccccDDDD", "HEADERKEY-secretsecret", "9999888877776666aaaa",
		"MODELECHO9999back",
	}
	for name, surface := range surfaces {
		for _, leak := range leaks {
			if strings.Contains(string(surface), leak) {
				t.Errorf("SECRET LEAKED into %s: %q\n%s", name, leak, surface)
			}
		}
		if claudemore.ContainsSecretShape(surface) {
			t.Errorf("%s still contains a bare credential shape:\n%s", name, surface)
		}
	}
	// Request and response bodies both echoed keys, so both must show markers.
	// The canonical request body is re-marshaled through encoding/json, which
	// HTML-escapes the marker's angle brackets (<redacted> -> <redacted>),
	// so match on the escape-invariant "redacted" substring.
	if !strings.Contains(string(c.Request.Body), "redacted") {
		t.Errorf("expected redaction marker in request body:\n%s", c.Request.Body)
	}
	if !strings.Contains(string(c.Response.Body), claudemore.RedactedValue) {
		t.Errorf("expected redaction marker in response body:\n%s", c.Response.Body)
	}
}

// TestRecorder_StreamingCapture proves an SSE response is captured (concatenated
// event stream) and flagged Streaming, while still flushing to the client.
func TestRecorder_StreamingCapture(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
		fl.Flush()
		_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		fl.Flush()
	}))
	defer upstream.Close()

	store := &memStore{}
	rec, done := newTestRecorder(t, store)
	p, _ := newProxy(upstream.URL, rec, testLogger())
	srv := httptest.NewServer(p)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"m","stream":true,"messages":[]}`))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	br := bufio.NewReader(resp.Body)
	got, _ := io.ReadAll(br)
	resp.Body.Close()
	if !strings.Contains(string(got), "message_start") || !strings.Contains(string(got), "message_stop") {
		t.Errorf("client did not receive full stream: %q", got)
	}

	waitStored(t, done)
	saved := store.all()
	if len(saved) != 1 {
		t.Fatalf("expected 1 cassette, got %d", len(saved))
	}
	c := saved[0]
	if !c.Response.Streaming {
		t.Error("cassette not flagged Streaming")
	}
	if !strings.Contains(string(c.Response.Body), "message_start") ||
		!strings.Contains(string(c.Response.Body), "message_stop") {
		t.Errorf("streaming body not fully captured: %q", c.Response.Body)
	}
}

// TestRecorder_SkipsNon2xx proves a non-success response (e.g. litellm budget
// 429) is NOT recorded — a 4xx/5xx is not a reusable cassette.
func TestRecorder_SkipsNon2xx(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error"}}`))
	}))
	defer upstream.Close()

	store := &memStore{}
	rec, done := newTestRecorder(t, store)
	_ = done
	p, _ := newProxy(upstream.URL, rec, testLogger())
	srv := httptest.NewServer(p)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader(`{"model":"m"}`))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// Give any (erroneous) async store a moment; none should occur.
	select {
	case <-done:
		t.Fatal("non-2xx response was recorded")
	case <-time.After(200 * time.Millisecond):
	}
	if len(store.all()) != 0 {
		t.Errorf("expected 0 cassettes for 429, got %d", len(store.all()))
	}
}

// TestRecorder_HashStableAcrossRerecord is the A3 re-record property at the
// integration level: sending the same logical request twice yields two cassettes
// with the same RequestHash.
func TestRecorder_HashStableAcrossRerecord(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	store := &memStore{}
	rec, done := newTestRecorder(t, store)
	p, _ := newProxy(upstream.URL, rec, testLogger())
	srv := httptest.NewServer(p)
	defer srv.Close()

	send := func() {
		resp, err := http.Post(srv.URL+"/v1/messages", "application/json",
			strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"same"}]}`))
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		waitStored(t, done)
	}
	send()
	send()

	saved := store.all()
	if len(saved) != 2 {
		t.Fatalf("expected 2 cassettes, got %d", len(saved))
	}
	if saved[0].RequestHash != saved[1].RequestHash {
		t.Errorf("re-record hash unstable: %s vs %s", saved[0].RequestHash, saved[1].RequestHash)
	}
}

// TestRecorder_DropsTruncatedExchange proves an over-limit capture is dropped
// (never persisted incomplete — an incomplete cassette is unsafe to serve
// byte-for-byte in REPLAY).
func TestRecorder_DropsTruncatedExchange(t *testing.T) {
	store := &memStore{}
	rec, done := newTestRecorder(t, store)
	rec.RecordExchange(Exchange{
		Method: "POST", Path: "/v1/messages", StatusCode: 200,
		ReqBody: []byte(`{"model":"m"}`), RespBody: []byte("partial"), Truncated: true,
	})
	select {
	case <-done:
		t.Fatal("truncated exchange was stored")
	case <-time.After(150 * time.Millisecond):
	}
	if len(store.all()) != 0 {
		t.Errorf("expected 0 cassettes for truncated exchange, got %d", len(store.all()))
	}
}

// TestRecorder_ClosedDropsWithoutPanic proves enqueueing after Close drops
// safely (no send-on-closed-channel panic) — the shutdown-race guard.
func TestRecorder_ClosedDropsWithoutPanic(t *testing.T) {
	store := &memStore{}
	r := newCassetteRecorder(context.Background(), store, testLogger())
	r.Close()
	r.Close() // idempotent
	// Must not panic; the cassette is simply dropped.
	r.RecordExchange(Exchange{
		Method: "POST", Path: "/v1/messages", StatusCode: 200,
		ReqBody: []byte(`{"model":"m"}`), RespBody: []byte("ok"),
	})
	if n := len(store.all()); n != 0 {
		t.Errorf("expected 0 stored after Close, got %d", n)
	}
}

// blockingStore gates its Put on a release channel so a test can prove Close()
// blocks until an in-flight/queued Put actually completes rather than returning
// while the writer is mid-write.
type blockingStore struct {
	release chan struct{} // Put blocks until this is closed
	mu      sync.Mutex
	puts    int
	done    int
}

func (b *blockingStore) Put(_ context.Context, _ claudemore.Cassette) error {
	b.mu.Lock()
	b.puts++
	b.mu.Unlock()
	<-b.release
	b.mu.Lock()
	b.done++
	b.mu.Unlock()
	return nil
}

func (b *blockingStore) Get(_ context.Context, _, _ string) (claudemore.Cassette, error) {
	return claudemore.Cassette{}, claudemore.ErrCassetteNotFound
}

func (b *blockingStore) counts() (puts, done int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.puts, b.done
}

// TestRecorder_CloseDrainsQueuedPuts is the shutdown-fidelity guard: Close() must
// BLOCK until the writeLoop has finished every queued Put, so a process closing
// the recorder on shutdown never loses a cassette to a writer goroutine killed
// mid-Put. A store whose Put is gated proves Close() does not return early.
func TestRecorder_CloseDrainsQueuedPuts(t *testing.T) {
	store := &blockingStore{release: make(chan struct{})}
	r := newCassetteRecorder(context.Background(), store, testLogger())
	r.RecordExchange(Exchange{
		Method: "POST", Path: "/v1/messages", StatusCode: 200,
		ReqBody:  []byte(`{"model":"m","messages":[{"role":"user","content":"drain"}]}`),
		RespBody: []byte(`{"ok":true}`),
	})

	// Close() in a goroutine — it must NOT return while the Put is still gated.
	closed := make(chan struct{})
	go func() { r.Close(); close(closed) }()

	// Wait until the writer has entered Put, then confirm Close is still blocked.
	deadline := time.After(3 * time.Second)
	for {
		if p, _ := store.counts(); p >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("writer never started the Put")
		case <-time.After(2 * time.Millisecond):
		}
	}
	select {
	case <-closed:
		t.Fatal("Close() returned before the queued Put completed")
	case <-time.After(50 * time.Millisecond):
	}

	// Release the Put; Close() must now return, and the Put must have completed.
	close(store.release)
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("Close() did not return after the Put was released")
	}
	if p, d := store.counts(); p != 1 || d != 1 {
		t.Errorf("expected exactly one completed Put after Close, got puts=%d done=%d", p, d)
	}
}

// TestRecorder_CloseIdempotentBlocksOnDrain proves a SECOND Close() (and one
// racing with the first) also waits for a fully-drained writer instead of
// returning early on the already-closed fast path.
func TestRecorder_CloseIdempotentBlocksOnDrain(t *testing.T) {
	store := &blockingStore{release: make(chan struct{})}
	r := newCassetteRecorder(context.Background(), store, testLogger())
	r.RecordExchange(Exchange{
		Method: "POST", Path: "/v1/messages", StatusCode: 200,
		ReqBody:  []byte(`{"model":"m","messages":[{"role":"user","content":"drain2"}]}`),
		RespBody: []byte(`{"ok":true}`),
	})

	first := make(chan struct{})
	second := make(chan struct{})
	go func() { r.Close(); close(first) }()
	// Give the first Close a moment to flip closed + close(work), then a second
	// concurrent Close takes the already-closed branch.
	go func() {
		time.Sleep(10 * time.Millisecond)
		r.Close()
		close(second)
	}()

	select {
	case <-second:
		t.Fatal("second Close() returned before the queued Put completed")
	case <-time.After(50 * time.Millisecond):
	}
	close(store.release)
	for _, ch := range []chan struct{}{first, second} {
		select {
		case <-ch:
		case <-time.After(3 * time.Second):
			t.Fatal("a Close() did not return after the Put was released")
		}
	}
	if _, d := store.counts(); d != 1 {
		t.Errorf("expected the queued Put to have completed, done=%d", d)
	}
}

// ctxHonoringStore models the S3 store's context behavior for the SIGTERM-drain
// regression (ka-0000000092): its Put fails with ctx.Err() whenever the context
// is cancelled — exactly what the AWS SDK does — so a Put whose parent is the
// cancelled signal context returns context.Canceled and stores NOTHING. The FS
// store masks the bug because its Put ignores ctx. `entered` signals that a Put
// is in-flight and `release` gates it, so a test can cancel the signal ctx while
// the Put is mid-flight (the real SIGTERM ordering) with no timing races.
type ctxHonoringStore struct {
	mu      sync.Mutex
	saved   []claudemore.Cassette
	entered chan struct{}
	release chan struct{}
}

func (s *ctxHonoringStore) Put(ctx context.Context, c claudemore.Cassette) error {
	s.entered <- struct{}{}
	select {
	case <-s.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	// Post-check mirrors the AWS SDK: a request whose context is cancelled fails
	// even if the wait above happened to observe `release` first.
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saved = append(s.saved, c)
	return nil
}

func (s *ctxHonoringStore) Get(_ context.Context, _, _ string) (claudemore.Cassette, error) {
	return claudemore.Cassette{}, claudemore.ErrCassetteNotFound
}

func (s *ctxHonoringStore) all() []claudemore.Cassette {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]claudemore.Cassette, len(s.saved))
	copy(out, s.saved)
	return out
}

// TestRecorder_CloseDrainsAfterSignalCtxCancelled is the regression guard for
// ka-0000000092. main.go threads the signal context (cancelled on SIGTERM) into
// the recorder as the Put parent, and the deferred recorder.Close() runs AFTER
// that context is already cancelled. Close() documents a hard drain guarantee —
// "no cassette is lost to a Put killed mid-flight" — so a queued cassette MUST
// land durably even though the signal ctx is cancelled. Against a ctx-honoring
// store (the S3 store honors ctx via the AWS SDK) the pre-fix recorder inherits
// the cancelled ctx, every Put returns context.Canceled, and the drain stores
// nothing. This test fails without the context.WithoutCancel decoupling.
func TestRecorder_CloseDrainsAfterSignalCtxCancelled(t *testing.T) {
	store := &ctxHonoringStore{
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	// Model signal.NotifyContext: a context cancelled when SIGTERM arrives.
	sigCtx, cancelSig := context.WithCancel(context.Background())
	r := newCassetteRecorder(sigCtx, store, testLogger())

	// A cassette is queued while the proxy is still live.
	r.RecordExchange(Exchange{
		Method: "POST", Path: "/v1/messages", StatusCode: 200,
		ReqBody:  []byte(`{"model":"m","messages":[{"role":"user","content":"drain-on-sigterm"}]}`),
		RespBody: []byte(`{"ok":true}`),
	})

	// Wait until the writer is mid-Put, then fire SIGTERM (cancel the signal ctx)
	// and let the store proceed — the exact shutdown ordering main.go produces.
	<-store.entered
	cancelSig()
	close(store.release)

	// Close() drains: the queued cassette must survive the cancelled signal ctx.
	r.Close()

	if n := len(store.all()); n != 1 {
		t.Fatalf("Close() dropped the queued cassette at SIGTERM: stored %d, want 1 "+
			"(the Put must not inherit the cancelled signal ctx)", n)
	}
}

// TestBodyCapture_IncompleteReadFlaggedTruncated proves a response body closed
// before EOF (client disconnect / upstream error) is flagged so the recorder
// drops it rather than persisting a partial 2xx as complete.
func TestBodyCapture_IncompleteReadFlaggedTruncated(t *testing.T) {
	var gotTrunc, gotIncomplete bool
	bc := &bodyCapture{
		rc:    io.NopCloser(strings.NewReader("partial-bytes-then-close")),
		limit: maxCaptureBytes,
		onDone: func(_ []byte, truncated, incomplete bool) {
			gotTrunc, gotIncomplete = truncated, incomplete
		},
	}
	// Read a few bytes but NOT to EOF, then Close (mid-stream teardown).
	buf := make([]byte, 4)
	if _, err := bc.Read(buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := bc.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if !gotIncomplete {
		t.Error("close before EOF should report incomplete=true")
	}
	_ = gotTrunc
}

// TestBodyCapture_FullReadNotIncomplete is the happy-path counterpart: reading
// to EOF marks the capture complete.
func TestBodyCapture_FullReadNotIncomplete(t *testing.T) {
	var gotIncomplete = true
	bc := &bodyCapture{
		rc:     io.NopCloser(strings.NewReader("all")),
		limit:  maxCaptureBytes,
		onDone: func(_ []byte, _, incomplete bool) { gotIncomplete = incomplete },
	}
	if _, err := io.ReadAll(bc); err != nil {
		t.Fatalf("readall: %v", err)
	}
	if err := bc.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if gotIncomplete {
		t.Error("full read to EOF should report incomplete=false")
	}
}

// TestRecorder_DirectRecordExchange exercises the transform + store on a
// hand-built Exchange (no HTTP), covering buildCassette directly.
func TestRecorder_DirectRecordExchange(t *testing.T) {
	store := &memStore{}
	rec, done := newTestRecorder(t, store)
	rec.RecordExchange(Exchange{
		Method:      "POST",
		Path:        "/v1/messages",
		StatusCode:  200,
		ReqHeaders:  http.Header{"Anthropic-Version": {"2023-06-01"}, "Authorization": {"Bearer sk-ant-oat01-XXXXXXXXXXXX"}},
		ReqBody:     []byte(`{"model":"claude-x","messages":[{"role":"user","content":"hi"}]}`),
		RespHeaders: http.Header{"Content-Type": {"application/json"}},
		RespBody:    []byte(`{"ok":true}`),
	})
	waitStored(t, done)
	saved := store.all()
	if len(saved) != 1 {
		t.Fatalf("expected 1 cassette, got %d", len(saved))
	}
	c := saved[0]
	if c.Model != "claude-x" {
		t.Errorf("model: got %q", c.Model)
	}
	if _, ok := c.Request.Headers["anthropic-version"]; !ok {
		t.Errorf("allowlisted header missing: %+v", c.Request.Headers)
	}
	if _, ok := c.Request.Headers["authorization"]; ok {
		t.Error("Authorization leaked into cassette headers")
	}
	if c.Response.Headers["content-type"] != "application/json" {
		t.Errorf("response content-type not captured: %+v", c.Response.Headers)
	}
}

// TestRecorder_StoreFailureDoesNotBlockClient proves a failing store never
// affects the client response (best-effort recording, cost-fence invariant).
func TestRecorder_StoreFailureDoesNotBlockClient(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("upstream-ok"))
	}))
	defer upstream.Close()

	store := &memStore{err: io.ErrClosedPipe}
	rec, done := newTestRecorder(t, store)
	p, _ := newProxy(upstream.URL, rec, testLogger())
	srv := httptest.NewServer(p)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader(`{"model":"m"}`))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(got) != "upstream-ok" {
		t.Errorf("store failure altered client response: %q", got)
	}
	waitStored(t, done) // the Put ran (and failed) without affecting the client
}
