package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"gardenio/controller/internal/claudemore"
)

// fakeStore is an in-memory CassetteStore for REPLAY server tests. It keys
// exactly like the S3 store (model + hash) so a cassette Put under a canonical
// request is Get-able under the same request the replay server canonicalizes.
type fakeStore struct {
	mu   sync.Mutex
	objs map[string]claudemore.Cassette
	// getErr, when non-nil, is returned from Get to exercise the store-error
	// (non-miss) hard-failure path.
	getErr error
}

func newFakeStore() *fakeStore { return &fakeStore{objs: map[string]claudemore.Cassette{}} }

func storeKey(model, hash string) string { return model + "\x00" + hash }

func (f *fakeStore) Put(_ context.Context, c claudemore.Cassette) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objs[storeKey(c.Model, c.RequestHash)] = c
	return nil
}

func (f *fakeStore) Get(_ context.Context, model, hash string) (claudemore.Cassette, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return claudemore.Cassette{}, f.getErr
	}
	c, ok := f.objs[storeKey(model, hash)]
	if !ok {
		return claudemore.Cassette{}, claudemore.ErrCassetteNotFound
	}
	return c, nil
}

// recordCassette canonicalizes a request the way the LIVE recorder does and
// stores a cassette with the given response, returning the raw request body so a
// test can replay the IDENTICAL request.
func recordCassette(t *testing.T, store *fakeStore, reqBody string, resp claudemore.CassetteResponse) string {
	t.Helper()
	req, model, hash, err := claudemore.CanonicalizeRequest(claudemore.RequestIdentity{
		Method: http.MethodPost, Path: "/v1/messages",
		Body: []byte(reqBody),
	})
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	c := claudemore.NewCassette(req, model, hash, resp, 1700000000000)
	if err := store.Put(context.Background(), c); err != nil {
		t.Fatalf("put: %v", err)
	}
	return reqBody
}

func newReplayTestServer(t *testing.T, store claudemore.CassetteStore) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(newReplayServer(store, testLogger()))
	t.Cleanup(srv.Close)
	return srv
}

// TestReplay_HitServesCassetteByteForByte is the T1 core: a recorded request
// resolves to its cassette and the stored response is served byte-for-byte.
func TestReplay_HitServesCassetteByteForByte(t *testing.T) {
	store := newFakeStore()
	body := `{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hi"}]}`
	recordCassette(t, store, body, claudemore.CassetteResponse{
		StatusCode: 200,
		Headers:    map[string]string{"content-type": "application/json"},
		Body:       []byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"hello"}]}`),
	})
	srv := newReplayTestServer(t, store)

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: got %d want 200", resp.StatusCode)
	}
	if resp.Header.Get("X-Claudemore-Replay") != "hit" {
		t.Errorf("replay marker: got %q want hit", resp.Header.Get("X-Claudemore-Replay"))
	}
	if resp.Header.Get("Content-Type") != "application/json" {
		t.Errorf("content-type not replayed: got %q", resp.Header.Get("Content-Type"))
	}
	want := `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"hello"}]}`
	if string(got) != want {
		t.Errorf("body not byte-for-byte:\n got %q\nwant %q", got, want)
	}
}

// TestReplay_HashStableAcrossVolatileFields proves a re-issued request that
// differs ONLY in a volatile field (metadata) or key ordering still hits the
// same cassette — the canonicalization is what makes REPLAY robust to run-to-run
// noise (A3).
func TestReplay_HashStableAcrossVolatileFields(t *testing.T) {
	store := newFakeStore()
	recorded := `{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hi"}],"metadata":{"user_id":"rec-run"}}`
	recordCassette(t, store, recorded, claudemore.CassetteResponse{
		StatusCode: 200, Body: []byte("REPLAYED"),
	})
	srv := newReplayTestServer(t, store)

	// Same logical request, different metadata + reordered top-level keys.
	replayed := `{"messages":[{"role":"user","content":"hi"}],"metadata":{"user_id":"replay-run"},"model":"claude-sonnet-5"}`
	resp, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader(replayed))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(got) != "REPLAYED" {
		t.Errorf("volatile-field request did not hit same cassette: status=%d body=%q", resp.StatusCode, got)
	}
}

// TestReplay_MissFailsLoudly is the fail_on_miss property (R2 §5): an unrecorded
// request returns a distinctive 424 with an Anthropic-shaped error and never
// falls back to LIVE.
func TestReplay_MissFailsLoudly(t *testing.T) {
	store := newFakeStore() // empty — every request misses
	srv := newReplayTestServer(t, store)

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"unrecorded"}]}`))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusFailedDependency {
		t.Errorf("miss status: got %d want 424", resp.StatusCode)
	}
	if resp.Header.Get("X-Claudemore-Replay") != "miss" {
		t.Errorf("miss marker: got %q want miss", resp.Header.Get("X-Claudemore-Replay"))
	}
	if miss := resp.Header.Get("X-Claudemore-Replay-Miss"); !strings.Contains(miss, "claude-sonnet-5/") {
		t.Errorf("miss header should name model/hash: got %q", miss)
	}
	// Body must be a valid Anthropic error envelope.
	var env struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("miss body not JSON: %v (%q)", err, body)
	}
	if env.Type != "error" || !strings.Contains(env.Error.Message, "fail_on_miss") {
		t.Errorf("miss body not fail_on_miss error: %+v", env)
	}
}

// TestReplay_StoreErrorIsHardFailure proves a store error (not a miss) is ALSO a
// hard failure — REPLAY must never fall through to LIVE on a broken store.
func TestReplay_StoreErrorIsHardFailure(t *testing.T) {
	store := newFakeStore()
	store.getErr = io.ErrUnexpectedEOF // any non-notfound error
	srv := newReplayTestServer(t, store)

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"m","messages":[]}`))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("store-error status: got %d want 502", resp.StatusCode)
	}
	if resp.Header.Get("X-Claudemore-Replay") != "error" {
		t.Errorf("error marker: got %q want error", resp.Header.Get("X-Claudemore-Replay"))
	}
}

// TestReplay_StreamingCassetteServesEventStream proves a streaming cassette is
// served with its recorded Content-Type and flushed — the wire shape the real
// `claude` binary's SSE parser expects.
func TestReplay_StreamingCassetteServesEventStream(t *testing.T) {
	store := newFakeStore()
	body := `{"model":"claude-sonnet-5","stream":true,"messages":[{"role":"user","content":"go"}]}`
	sse := "event: message_start\ndata: {}\n\nevent: message_stop\ndata: {}\n\n"
	recordCassette(t, store, body, claudemore.CassetteResponse{
		StatusCode: 200,
		Headers:    map[string]string{"content-type": "text/event-stream"},
		Streaming:  true,
		Body:       []byte(sse),
	})
	srv := newReplayTestServer(t, store)

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("streaming content-type: got %q", ct)
	}
	if string(got) != sse {
		t.Errorf("streaming body not byte-for-byte: got %q", got)
	}
}

// TestReplay_NonJSONBodyMissesCleanly proves a non-JSON body does not crash the
// canonicalizer (it hashes over raw redacted bytes) and simply misses when no
// cassette matches — graceful degradation, not a 500.
func TestReplay_NonJSONBodyMissesCleanly(t *testing.T) {
	store := newFakeStore()
	srv := newReplayTestServer(t, store)
	resp, err := http.Post(srv.URL+"/v1/messages", "text/plain", strings.NewReader("not-json"))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFailedDependency {
		t.Errorf("non-json miss status: got %d want 424", resp.StatusCode)
	}
}

// errReader fails on the first Read so ServeHTTP's body-read-error branch runs.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, io.ErrClosedPipe }

// TestReplay_BodyReadErrorReturns400 exercises the request-body read-error path:
// a body that errors mid-read yields a clean 400, not a hang or 500.
func TestReplay_BodyReadErrorReturns400(t *testing.T) {
	store := newFakeStore()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", errReader{})
	newReplayServer(store, testLogger()).ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("body read error: got %d want 400", rec.Code)
	}
	if rec.Header().Get("X-Claudemore-Replay") != "error" {
		t.Errorf("read-error marker: got %q want error", rec.Header().Get("X-Claudemore-Replay"))
	}
}

// TestReplay_OverLimitBodyMisses proves a request body over the capture limit is
// treated as an unrecordable miss (the recorder never persists over-limit
// exchanges), not a crash.
func TestReplay_OverLimitBodyMisses(t *testing.T) {
	store := newFakeStore()
	rec := httptest.NewRecorder()
	big := strings.NewReader(strings.Repeat("x", replayReadLimit+10))
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", big)
	newReplayServer(store, testLogger()).ServeHTTP(rec, req)
	if rec.Code != http.StatusFailedDependency {
		t.Errorf("over-limit body: got %d want 424 (miss)", rec.Code)
	}
	if rec.Header().Get("X-Claudemore-Replay") != "miss" {
		t.Errorf("over-limit marker: got %q want miss", rec.Header().Get("X-Claudemore-Replay"))
	}
}

func TestHeaderSafe(t *testing.T) {
	cases := map[string]string{
		"claude-sonnet-5":    "claude-sonnet-5",
		"anthropic/claude-x": "anthropic/claude-x",
		"bad\r\nInjected":    "bad__Injected",
		"with space":         "with_space",
		"tab\tchar":          "tab_char",
	}
	for in, want := range cases {
		if got := headerSafe(in); got != want {
			t.Errorf("headerSafe(%q): got %q want %q", in, got, want)
		}
	}
}

func TestReadLimited(t *testing.T) {
	// nil reader.
	if b, over, err := readLimited(nil, 10); b != nil || over || err != nil {
		t.Errorf("nil reader: got b=%v over=%v err=%v", b, over, err)
	}
	// under limit.
	b, over, err := readLimited(strings.NewReader("abc"), 10)
	if err != nil || over || string(b) != "abc" {
		t.Errorf("under limit: b=%q over=%v err=%v", b, over, err)
	}
	// exactly at limit — NOT over.
	b, over, err = readLimited(strings.NewReader("abcde"), 5)
	if err != nil || over || string(b) != "abcde" {
		t.Errorf("at limit: b=%q over=%v err=%v", b, over, err)
	}
	// over limit — truncated to limit + flagged.
	b, over, err = readLimited(strings.NewReader("abcdefgh"), 5)
	if err != nil || !over || string(b) != "abcde" {
		t.Errorf("over limit: b=%q over=%v err=%v", b, over, err)
	}
}

// TestReplay_HeadPreflightIsNotAMiss proves the SDK's `HEAD /` connectivity
// preflight is answered 200 with a distinct "preflight" marker rather than
// routed to the cassette lookup as a fail_on_miss. The recorder never captures a
// HEAD, so treating it as a cassette miss would pollute the miss signal a hooks
// e2e asserts on (ka-0000004452).
func TestReplay_HeadPreflightIsNotAMiss(t *testing.T) {
	store := newFakeStore()
	srv := newReplayTestServer(t, store)

	req, err := http.NewRequest(http.MethodHead, srv.URL+"/", nil)
	if err != nil {
		t.Fatalf("new HEAD request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("HEAD request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("HEAD preflight status: got %d want 200 (must not be a fail_on_miss 424)", resp.StatusCode)
	}
	if marker := resp.Header.Get("X-Claudemore-Replay"); marker != "preflight" {
		t.Errorf("HEAD preflight marker: got %q want preflight", marker)
	}
}
