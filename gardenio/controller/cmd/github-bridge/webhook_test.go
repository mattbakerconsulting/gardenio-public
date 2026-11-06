package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

var testSecret = []byte("It's a Secret to Everybody")

func sign(secret, body []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write(body)
	return signaturePrefix + hex.EncodeToString(m.Sum(nil))
}

type recSink struct {
	mu   sync.Mutex
	evs  []Event
	fail error
}

func (s *recSink) Handle(_ context.Context, ev Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return s.fail
	}
	s.evs = append(s.evs, ev)
	return nil
}

func (s *recSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.evs)
}

type harness struct {
	h        http.Handler
	sink     *recSink
	replay   *replayCache
	outcomes []string
	now      time.Time
}

func newHarness(secret []byte, cacheMax int) *harness {
	hn := &harness{sink: &recSink{}, now: time.Unix(1_700_000_000, 0)}
	hn.replay = newReplayCache(time.Hour, cacheMax, func() time.Time { return hn.now })
	hn.h = newWebhookHandler(webhookConfig{
		Secret: secret, Sink: hn.sink, Replay: hn.replay,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Observe: func(o string) { hn.outcomes = append(hn.outcomes, o) },
	})
	return hn
}

func (hn *harness) post(body, sig, delivery, event string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if sig != "" {
		r.Header.Set(headerSignature, sig)
	}
	if delivery != "" {
		r.Header.Set(headerDelivery, delivery)
	}
	if event != "" {
		r.Header.Set(headerEvent, event)
	}
	w := httptest.NewRecorder()
	hn.h.ServeHTTP(w, r)
	return w
}

func (hn *harness) last() string { return hn.outcomes[len(hn.outcomes)-1] }

const prBody = `{"action":"opened","repository":{"full_name":"joyousaversion/gardenio"},"installation":{"id":170295668},"sender":{"login":"octocat"}}`

func TestVerifySignature_GitHubDocumentedVector(t *testing.T) {
	// The test vector from GitHub's "Validating webhook deliveries" docs.
	const want = "sha256=757107ea0eb2509fc211221cce984b8a37570b6d7586c22c46f4379c8b043e17"
	if !verifySignature(testSecret, []byte("Hello, World!"), want) {
		t.Fatal("documented GitHub vector must verify")
	}
}

func TestVerifySignature_Rejects(t *testing.T) {
	body := []byte(prBody)
	good := sign(testSecret, body)
	cases := map[string]struct {
		secret []byte
		body   []byte
		header string
	}{
		"empty header":       {testSecret, body, ""},
		"no prefix":          {testSecret, body, strings.TrimPrefix(good, signaturePrefix)},
		"sha1 prefix":        {testSecret, body, "sha1=" + strings.TrimPrefix(good, signaturePrefix)},
		"not hex":            {testSecret, body, signaturePrefix + "zz"},
		"truncated mac":      {testSecret, body, good[:len(good)-2]},
		"wrong secret":       {[]byte("other"), body, good},
		"tampered body":      {testSecret, []byte(prBody + " "), good},
		"empty secret":       {nil, body, sign(nil, body)},
		"uppercase prefix":   {testSecret, body, "SHA256=" + strings.TrimPrefix(good, signaturePrefix)},
		"extra trailing hex": {testSecret, body, good + "00"},
	}
	for name, c := range cases {
		if verifySignature(c.secret, c.body, c.header) {
			t.Errorf("%s: must not verify", name)
		}
	}
}

func TestWebhook_AcceptsValidSignature(t *testing.T) {
	hn := newHarness(testSecret, 10)
	w := hn.post(prBody, sign(testSecret, []byte(prBody)), "d-1", "pull_request")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d", w.Code)
	}
	if hn.sink.count() != 1 {
		t.Fatalf("sink got %d events", hn.sink.count())
	}
	ev := hn.sink.evs[0]
	if ev.Type != "pull_request" || ev.Action != "opened" || ev.Repo != "joyousaversion/gardenio" ||
		ev.InstallationID != 170295668 || ev.Sender != "octocat" || ev.DeliveryID != "d-1" {
		t.Errorf("envelope = %+v", ev)
	}
	if hn.last() != outcomeAccepted {
		t.Errorf("outcome = %s", hn.last())
	}
}

func TestWebhook_RejectsBadSignature(t *testing.T) {
	hn := newHarness(testSecret, 10)
	for name, sig := range map[string]string{
		"missing": "",
		"wrong":   sign([]byte("not the secret"), []byte(prBody)),
		"garbage": "sha256=nothex",
	} {
		w := hn.post(prBody, sig, "d-"+name, "pull_request")
		if w.Code != http.StatusForbidden {
			t.Errorf("%s: status = %d, want 403", name, w.Code)
		}
		if hn.last() != outcomeBadSignature {
			t.Errorf("%s: outcome = %s", name, hn.last())
		}
	}
	if hn.sink.count() != 0 {
		t.Error("unauthenticated request reached the sink")
	}
	if hn.replay.size() != 0 {
		t.Error("unauthenticated request polluted the replay cache")
	}
}

func TestWebhook_FailsClosedOnEmptySecret(t *testing.T) {
	hn := newHarness(nil, 10)
	// Even a "valid" signature computed with the empty key must be refused.
	w := hn.post(prBody, sign(nil, []byte(prBody)), "d-1", "pull_request")
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	if hn.last() != outcomeNoSecret || hn.sink.count() != 0 {
		t.Errorf("outcome=%s sink=%d", hn.last(), hn.sink.count())
	}
}

func TestWebhook_ReplayIsIgnored(t *testing.T) {
	hn := newHarness(testSecret, 10)
	sig := sign(testSecret, []byte(prBody))
	if w := hn.post(prBody, sig, "d-1", "pull_request"); w.Code != http.StatusAccepted {
		t.Fatalf("first = %d", w.Code)
	}
	w := hn.post(prBody, sig, "d-1", "pull_request")
	if w.Code != http.StatusOK {
		t.Errorf("replay status = %d, want 200 (so GitHub does not mark it failed)", w.Code)
	}
	if hn.sink.count() != 1 {
		t.Errorf("replay reached the sink: %d events", hn.sink.count())
	}
	if hn.last() != outcomeDuplicate {
		t.Errorf("outcome = %s", hn.last())
	}
}

func TestWebhook_ReplayExpiresAfterTTL(t *testing.T) {
	hn := newHarness(testSecret, 10)
	sig := sign(testSecret, []byte(prBody))
	hn.post(prBody, sig, "d-1", "pull_request")
	hn.now = hn.now.Add(time.Hour + time.Second)
	if w := hn.post(prBody, sig, "d-1", "pull_request"); w.Code != http.StatusAccepted {
		t.Fatalf("post-TTL delivery = %d, want 202", w.Code)
	}
}

func TestWebhook_FailedHandlerAllowsRedelivery(t *testing.T) {
	hn := newHarness(testSecret, 10)
	sig := sign(testSecret, []byte(prBody))
	hn.sink.fail = errors.New("boom")
	if w := hn.post(prBody, sig, "d-1", "pull_request"); w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", w.Code)
	}
	hn.sink.fail = nil
	if w := hn.post(prBody, sig, "d-1", "pull_request"); w.Code != http.StatusAccepted {
		t.Fatalf("redelivery after failure = %d, want 202 (must not be swallowed as a replay)", w.Code)
	}
	if hn.sink.count() != 1 {
		t.Errorf("sink events = %d", hn.sink.count())
	}
}

func TestWebhook_CacheFullShedsInsteadOfEvicting(t *testing.T) {
	hn := newHarness(testSecret, 2)
	sig := sign(testSecret, []byte(prBody))
	hn.post(prBody, sig, "d-1", "pull_request")
	hn.post(prBody, sig, "d-2", "pull_request")
	if w := hn.post(prBody, sig, "d-3", "pull_request"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	// d-1 must still be remembered: eviction would reopen its replay window.
	if w := hn.post(prBody, sig, "d-1", "pull_request"); w.Code != http.StatusOK {
		t.Errorf("d-1 replay = %d, want 200 duplicate", w.Code)
	}
	// Once entries expire, capacity frees up.
	hn.now = hn.now.Add(2 * time.Hour)
	if w := hn.post(prBody, sig, "d-3", "pull_request"); w.Code != http.StatusAccepted {
		t.Errorf("after expiry = %d, want 202", w.Code)
	}
}

func TestWebhook_PingDoesNotReachSink(t *testing.T) {
	hn := newHarness(testSecret, 10)
	body := `{"zen":"Keep it logically awesome."}`
	if w := hn.post(body, sign(testSecret, []byte(body)), "d-ping", "ping"); w.Code != http.StatusOK {
		t.Fatalf("ping = %d", w.Code)
	}
	if hn.sink.count() != 0 || hn.last() != outcomePing {
		t.Errorf("sink=%d outcome=%s", hn.sink.count(), hn.last())
	}
}

func TestWebhook_MalformedRequests(t *testing.T) {
	hn := newHarness(testSecret, 10)
	sig := sign(testSecret, []byte(prBody))
	if w := hn.post(prBody, sig, "", "pull_request"); w.Code != http.StatusBadRequest {
		t.Errorf("missing delivery id = %d", w.Code)
	}
	if w := hn.post(prBody, sig, "d-1", ""); w.Code != http.StatusBadRequest {
		t.Errorf("missing event = %d", w.Code)
	}
	r := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(prBody))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set(headerSignature, sig)
	r.Header.Set(headerDelivery, "d-2")
	r.Header.Set(headerEvent, "pull_request")
	w := httptest.NewRecorder()
	hn.h.ServeHTTP(w, r)
	if w.Code != http.StatusUnsupportedMediaType {
		t.Errorf("form content type = %d", w.Code)
	}
	g := httptest.NewRecorder()
	hn.h.ServeHTTP(g, httptest.NewRequest(http.MethodGet, "/webhook", nil))
	if g.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET = %d", g.Code)
	}
	if hn.sink.count() != 0 {
		t.Error("malformed request reached the sink")
	}
}

func TestWebhook_OversizedBody(t *testing.T) {
	hn := newHarness(testSecret, 10)
	big := strings.Repeat("a", maxBodyBytes+1)
	if w := hn.post(big, sign(testSecret, []byte(big)), "d-1", "push"); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", w.Code)
	}
}

func TestWebhook_RejectionsLeakNothing(t *testing.T) {
	hn := newHarness(testSecret, 10)
	w := hn.post(prBody, "sha256=00", "d-1", "pull_request")
	if strings.Contains(w.Body.String(), string(testSecret)) || strings.TrimSpace(w.Body.String()) != "Forbidden" {
		t.Errorf("rejection body = %q", w.Body.String())
	}
}

// size is the number of delivery ids the cache holds; only tests read it.
func (c *replayCache) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.seen)
}
