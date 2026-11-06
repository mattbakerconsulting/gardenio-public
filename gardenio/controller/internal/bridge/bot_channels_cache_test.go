package bridge

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/slack-go/slack"
)

// fakeConversationsListServer serves conversations.list from a fixed set of
// channels, counting how many times the endpoint is actually hit. When
// rateLimited is non-nil and set, it responds 429 with the given Retry-After
// (seconds) instead — simulating the Tier-2 rate limit that motivated the cache
// (ka-0000001584).
func fakeConversationsListServer(t *testing.T, channels []map[string]any, listCalls *int32, rateLimited *int32, retryAfter string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if strings.Contains(r.URL.Path, "conversations.list") {
			atomic.AddInt32(listCalls, 1)
			if rateLimited != nil && atomic.LoadInt32(rateLimited) == 1 {
				w.Header().Set("Retry-After", retryAfter)
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":                true,
				"channels":          channels,
				"response_metadata": map[string]any{"next_cursor": ""},
			})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
}

func memberChannel(id, name string) map[string]any {
	return map[string]any{"id": id, "name": name, "is_member": true, "is_channel": true}
}

func newCacheTestAPI(srv *httptest.Server, ttl time.Duration, clock func() time.Time) *SlackChannelAPI {
	api := slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/"))
	a := NewSlackChannelAPI(api, slog.Default())
	a.cacheTTL = ttl
	if clock != nil {
		a.now = clock
	}
	return a
}

// TestListChannels_ServesFromCacheWithinTTL verifies conversations.list is hit
// only once for repeated calls inside the TTL window — the core fix for the
// Tier-2 rate-limit burn.
func TestListChannels_ServesFromCacheWithinTTL(t *testing.T) {
	chans := []map[string]any{
		memberChannel("C001", "general"),
		memberChannel("C002", "acme-devops"),
	}
	var listCalls int32
	srv := fakeConversationsListServer(t, chans, &listCalls, nil, "")
	defer srv.Close()

	a := newCacheTestAPI(srv, 5*time.Minute, nil)
	mux := http.NewServeMux()
	a.RegisterRoutes(mux)

	for i := range 5 {
		req := httptest.NewRequest(http.MethodGet, "/api/slack/channels", nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("call %d: status=%d body=%s", i, w.Code, w.Body.String())
		}
		var got []ChannelInfo
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("call %d: decode: %v", i, err)
		}
		if len(got) != 2 {
			t.Fatalf("call %d: got %d channels, want 2", i, len(got))
		}
	}

	if n := atomic.LoadInt32(&listCalls); n != 1 {
		t.Errorf("conversations.list called %d times, want 1 (cache should absorb the rest)", n)
	}
}

// TestListChannels_RefreshesAfterTTL verifies the cache expires and refetches
// once the TTL elapses.
func TestListChannels_RefreshesAfterTTL(t *testing.T) {
	chans := []map[string]any{memberChannel("C001", "general")}
	var listCalls int32
	srv := fakeConversationsListServer(t, chans, &listCalls, nil, "")
	defer srv.Close()

	now := time.Unix(1_700_000_000, 0)
	a := newCacheTestAPI(srv, 1*time.Minute, func() time.Time { return now })
	mux := http.NewServeMux()
	a.RegisterRoutes(mux)

	do := func() {
		req := httptest.NewRequest(http.MethodGet, "/api/slack/channels", nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
	}

	do() // fetch #1
	do() // cached
	now = now.Add(2 * time.Minute)
	do() // TTL elapsed -> fetch #2

	if n := atomic.LoadInt32(&listCalls); n != 2 {
		t.Errorf("conversations.list called %d times, want 2 (one per TTL window)", n)
	}
}

// TestListChannels_RateLimitReturns429 verifies a Slack 429 on the first fetch
// is surfaced as HTTP 429 with a Retry-After header and a distinct error body,
// not collapsed into a generic 500.
func TestListChannels_RateLimitReturns429(t *testing.T) {
	var listCalls int32
	rl := int32(1)
	srv := fakeConversationsListServer(t, nil, &listCalls, &rl, "30")
	defer srv.Close()

	a := newCacheTestAPI(srv, 5*time.Minute, nil)
	mux := http.NewServeMux()
	a.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/slack/channels", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d, want 429; body=%s", w.Code, w.Body.String())
	}
	if ra := w.Header().Get("Retry-After"); ra != "30" {
		t.Errorf("Retry-After=%q, want 30", ra)
	}
	if !strings.Contains(w.Body.String(), "rate limited") {
		t.Errorf("body %q should mention rate limiting", w.Body.String())
	}
}

// TestListChannels_ServesStaleOnRateLimit verifies that once a good result is
// cached, a subsequent refresh that hits a 429 falls back to the stale result
// instead of failing — name resolution keeps working through a rate-limit blip.
func TestListChannels_ServesStaleOnRateLimit(t *testing.T) {
	chans := []map[string]any{memberChannel("C001", "general")}
	var listCalls int32
	rl := int32(0)
	srv := fakeConversationsListServer(t, chans, &listCalls, &rl, "30")
	defer srv.Close()

	now := time.Unix(1_700_000_000, 0)
	a := newCacheTestAPI(srv, 1*time.Minute, func() time.Time { return now })
	mux := http.NewServeMux()
	a.RegisterRoutes(mux)

	do := func() (int, []ChannelInfo) {
		req := httptest.NewRequest(http.MethodGet, "/api/slack/channels", nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		var got []ChannelInfo
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		return w.Code, got
	}

	// Prime the cache with a good result.
	if code, got := do(); code != http.StatusOK || len(got) != 1 {
		t.Fatalf("prime: code=%d n=%d, want 200/1", code, len(got))
	}

	// Expire the cache and make the next fetch rate-limited.
	now = now.Add(2 * time.Minute)
	atomic.StoreInt32(&rl, 1)

	code, got := do()
	if code != http.StatusOK {
		t.Fatalf("stale-serve: code=%d, want 200 (should fall back to stale cache)", code)
	}
	if len(got) != 1 || got[0].ID != "C001" {
		t.Errorf("stale-serve: got %+v, want the primed channel C001", got)
	}
}
