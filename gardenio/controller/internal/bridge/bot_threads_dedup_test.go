package bridge

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/slack-go/slack"
)

// newThreadAPIWithFakeSlack wires a SlackThreadAPI to a fake Slack server so
// handlePostReply's PostMessage call succeeds, and returns both the API mux
// and the counter of actual chat.postMessage calls that reached "Slack".
func newThreadAPIWithFakeSlack(t *testing.T) (*http.ServeMux, *atomic.Int32, *SlackThreadAPI) {
	t.Helper()
	var postCount atomic.Int32
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postMessage" {
			postCount.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "1234.5678"})
	}))
	t.Cleanup(slackSrv.Close)

	client := slack.New("xoxb-test", slack.OptionAPIURL(slackSrv.URL+"/"))
	api := NewSlackThreadAPI(client, slog.Default())
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)
	return mux, &postCount, api
}

func postReply(t *testing.T, mux *http.ServeMux, req replyRequest) (int, replyResponse) {
	t.Helper()
	body, _ := json.Marshal(req)
	httpReq := httptest.NewRequest(http.MethodPost, "/api/slack/threads/reply", bytes.NewReader(body))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httpReq)
	var resp replyResponse
	_ = json.NewDecoder(w.Body).Decode(&resp)
	return w.Code, resp
}

// TestPostReply_CollapsesIdenticalRepliesInWindow is the bridge-side backstop
// for ka-0000002080: N identical (channel, thread_ts, text) replies arriving
// inside the dedup window must produce exactly ONE Slack post. The duplicates
// still get 200/ok (deduped=true) so best-effort callers proceed normally.
func TestPostReply_CollapsesIdenticalRepliesInWindow(t *testing.T) {
	mux, postCount, _ := newThreadAPIWithFakeSlack(t)

	req := replyRequest{Channel: "C42", ThreadTS: "111.222", Text: ":warning: I was interrupted mid-response"}

	code, first := postReply(t, mux, req)
	if code != http.StatusOK || !first.OK {
		t.Fatalf("first reply: code=%d ok=%v, want 200/true", code, first.OK)
	}
	if first.Deduped {
		t.Errorf("first reply must not be marked deduped")
	}
	if first.Timestamp != "1234.5678" {
		t.Errorf("first reply ts = %q, want %q", first.Timestamp, "1234.5678")
	}

	// The observed incident was bursts of 4 — replay 11 more duplicates.
	for range 11 {
		code, resp := postReply(t, mux, req)
		if code != http.StatusOK || !resp.OK {
			t.Fatalf("duplicate reply: code=%d ok=%v, want 200/true", code, resp.OK)
		}
		if !resp.Deduped {
			t.Errorf("duplicate reply must be marked deduped")
		}
		if resp.Timestamp != "1234.5678" {
			t.Errorf("duplicate reply ts = %q, want the winner's %q", resp.Timestamp, "1234.5678")
		}
	}

	if n := postCount.Load(); n != 1 {
		t.Fatalf("expected exactly 1 chat.postMessage across 12 identical replies, got %d", n)
	}
}

// TestPostReply_DistinctRepliesNotCollapsed verifies dedup keys on the full
// (channel, thread_ts, text) triple — different text, thread, or channel all
// post normally.
func TestPostReply_DistinctRepliesNotCollapsed(t *testing.T) {
	mux, postCount, _ := newThreadAPIWithFakeSlack(t)

	reqs := []replyRequest{
		{Channel: "C42", ThreadTS: "111.222", Text: "hello"},
		{Channel: "C42", ThreadTS: "111.222", Text: "different text"},
		{Channel: "C42", ThreadTS: "333.444", Text: "hello"},
		{Channel: "C99", ThreadTS: "111.222", Text: "hello"},
	}
	for _, req := range reqs {
		code, resp := postReply(t, mux, req)
		if code != http.StatusOK || !resp.OK || resp.Deduped {
			t.Errorf("distinct reply %+v: code=%d ok=%v deduped=%v, want 200/true/false",
				req, code, resp.OK, resp.Deduped)
		}
	}
	if n := postCount.Load(); n != int32(len(reqs)) {
		t.Fatalf("expected %d Slack posts for %d distinct replies, got %d", len(reqs), len(reqs), n)
	}
}

// TestPostReply_ConcurrentIdenticalRepliesPostOnce is the actual race from the
// incident: 4 near-simultaneous teardowns all POST the same breadcrumb within
// ~1s. The reservation in begin() is taken before the Slack call, so exactly
// one must reach Slack no matter the interleaving.
func TestPostReply_ConcurrentIdenticalRepliesPostOnce(t *testing.T) {
	mux, postCount, _ := newThreadAPIWithFakeSlack(t)

	req := replyRequest{Channel: "C42", ThreadTS: "111.222", Text: ":warning: I was interrupted mid-response"}
	payload, _ := json.Marshal(req)

	const racers = 4
	var wg sync.WaitGroup
	codes := make([]int, racers)
	for i := range racers {
		wg.Go(func() {
			httpReq := httptest.NewRequest(http.MethodPost, "/api/slack/threads/reply", bytes.NewReader(payload))
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httpReq)
			codes[i] = w.Code
		})
	}
	wg.Wait()

	for i, code := range codes {
		if code != http.StatusOK {
			t.Errorf("racer %d: code=%d, want 200", i, code)
		}
	}
	if n := postCount.Load(); n != 1 {
		t.Fatalf("expected exactly 1 chat.postMessage across %d concurrent identical replies, got %d", racers, n)
	}
}

// TestPostReply_FailedPostReleasesReservation verifies abort(): when the Slack
// post fails, the reservation is released so an immediate retry of the same
// reply is not swallowed by a slot that produced no message.
func TestPostReply_FailedPostReleasesReservation(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	var postCount atomic.Int32
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if fail.Load() {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "fatal_error"})
			return
		}
		postCount.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "1234.5678"})
	}))
	t.Cleanup(slackSrv.Close)

	client := slack.New("xoxb-test", slack.OptionAPIURL(slackSrv.URL+"/"))
	api := NewSlackThreadAPI(client, slog.Default())
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	req := replyRequest{Channel: "C42", ThreadTS: "111.222", Text: "retry me"}

	if code, _ := postReply(t, mux, req); code != http.StatusInternalServerError {
		t.Fatalf("failed post: code=%d, want 500", code)
	}

	fail.Store(false)
	code, resp := postReply(t, mux, req)
	if code != http.StatusOK || !resp.OK || resp.Deduped {
		t.Fatalf("retry after failure: code=%d ok=%v deduped=%v, want 200/true/false", code, resp.OK, resp.Deduped)
	}
	if n := postCount.Load(); n != 1 {
		t.Fatalf("expected the retry to reach Slack exactly once, got %d", n)
	}
}

// TestThreadReplyDeduper_WindowExpiry verifies an identical reply posts again
// once the window has elapsed — the dedup is a burst collapse, not a permanent
// gag on repeated messages.
func TestThreadReplyDeduper_WindowExpiry(t *testing.T) {
	d := newThreadReplyDeduper(5 * time.Minute)
	current := time.Unix(1_700_000_000, 0)
	d.now = func() time.Time { return current }

	key, dup, _ := d.begin("C42", "111.222", "hello")
	if dup {
		t.Fatalf("first begin must not be a dup")
	}
	d.commit(key, "1.2")

	// Inside the window: collapsed, and the winner's ts is surfaced.
	current = current.Add(4 * time.Minute)
	if _, dup, prevTS := d.begin("C42", "111.222", "hello"); !dup || prevTS != "1.2" {
		t.Fatalf("inside window: dup=%v prevTS=%q, want true/%q", dup, prevTS, "1.2")
	}

	// Past the window (measured from the reservation): posts again.
	current = current.Add(2 * time.Minute)
	if _, dup, _ := d.begin("C42", "111.222", "hello"); dup {
		t.Fatalf("past window: identical reply must be allowed to post again")
	}
}

// TestThreadReplyDeduper_LazyPruneBoundsMap verifies expired entries are
// dropped on subsequent reservations so the map does not grow without bound.
func TestThreadReplyDeduper_LazyPruneBoundsMap(t *testing.T) {
	d := newThreadReplyDeduper(5 * time.Minute)
	current := time.Unix(1_700_000_000, 0)
	d.now = func() time.Time { return current }

	for i := range 100 {
		key, _, _ := d.begin("C42", "111.222", string(rune('a'+i%26))+string(rune('0'+i/26)))
		d.commit(key, "1.2")
	}

	current = current.Add(6 * time.Minute)
	d.begin("C42", "111.222", "fresh")

	d.mu.Lock()
	n := len(d.entries)
	d.mu.Unlock()
	if n != 1 {
		t.Fatalf("expected lazy prune to drop all 100 expired entries (1 fresh left), got %d", n)
	}
}
