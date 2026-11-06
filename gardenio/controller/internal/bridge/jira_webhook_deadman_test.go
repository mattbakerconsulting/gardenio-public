package bridge

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// webhookFakeCounter is an ApproximateCount stub for the webhook monitor tests.
// It records the JQL it was asked and returns a fixed count/error. (Distinct
// from the map-keyed fakeCounter in jira_metrics_test.go — the monitor issues a
// single fixed JQL, so a scalar stub is clearer here.)
type webhookFakeCounter struct {
	count  int64
	err    error
	gotJQL string
	calls  int
}

func (f *webhookFakeCounter) ApproximateCount(_ context.Context, jql string) (int64, error) {
	f.calls++
	f.gotJQL = jql
	return f.count, f.err
}

// resetWebhookMetrics clears every webhook series so each test starts from a
// known baseline (the promauto families are process-global).
func resetWebhookMetrics() {
	jiraWebhookDeliveries.Reset()
	jiraWebhookEvaluations.Reset()
	jiraWebhookDark.Set(0)
	jiraWebhookUpstreamMatches.Set(0)
	lastWebhookReceivedNano.Store(0)
}

func testMonitor(counter jiraCounter, window time.Duration, now time.Time) *JiraWebhookMonitor {
	return NewJiraWebhookMonitor(JiraWebhookMonitorConfig{
		Jira:     counter,
		Projects: []string{"PE", "DEVOPS"},
		Window:   window,
		Logger:   slog.New(slog.NewTextHandler(nopWriter{}, nil)),
		Clock:    func() time.Time { return now },
	})
}

// recordWebhookOutcome stamps the receive-liveness clock ONLY for authenticated,
// well-formed deliveries (processed|ignored). hmac_failed and bad_request must
// NOT stamp it — a wrong-Secret or malformed delivery does not prove the hook is
// working, so it cannot keep the deadman quiet.
func TestRecordWebhookOutcome_OnlyAuthenticatedStampsClock(t *testing.T) {
	for _, outcome := range []string{webhookOutcomeProcessed, webhookOutcomeIgnored} {
		resetWebhookMetrics()
		recordWebhookOutcome(outcome, time.Time{})
		if lastWebhookReceivedNano.Load() == 0 {
			t.Errorf("outcome %q must stamp the receive-liveness clock", outcome)
		}
		if got := testutil.ToFloat64(jiraWebhookDeliveries.WithLabelValues(outcome)); got != 1 {
			t.Errorf("deliveries{outcome=%q} = %v, want 1", outcome, got)
		}
	}
	for _, outcome := range []string{webhookOutcomeHMACFailed, webhookOutcomeBadRequest} {
		resetWebhookMetrics()
		recordWebhookOutcome(outcome, time.Time{})
		if lastWebhookReceivedNano.Load() != 0 {
			t.Errorf("outcome %q must NOT stamp the receive-liveness clock (rejected traffic is not proof of a working hook)", outcome)
		}
		if got := testutil.ToFloat64(jiraWebhookDeliveries.WithLabelValues(outcome)); got != 1 {
			t.Errorf("deliveries{outcome=%q} = %v, want 1", outcome, got)
		}
	}
}

// A recent authenticated delivery is the healthy state: the verdict clears to 0
// and no approximate-count call is made (cheap early-out for the common case).
func TestEvaluate_RecentDelivery_NotDark(t *testing.T) {
	resetWebhookMetrics()
	now := time.Unix(1_700_000_000, 0)
	lastWebhookReceivedNano.Store(now.Add(-30 * time.Minute).UnixNano())
	counter := &webhookFakeCounter{count: 999}
	m := testMonitor(counter, 2*time.Hour, now)

	m.evaluate(context.Background())

	if got := testutil.ToFloat64(jiraWebhookDark); got != 0 {
		t.Errorf("dark gauge = %v, want 0 (recent delivery)", got)
	}
	if counter.calls != 0 {
		t.Errorf("approximate-count must NOT be called when a delivery is recent, got %d calls", counter.calls)
	}
	if got := testutil.ToFloat64(jiraWebhookEvaluations.WithLabelValues(webhookEvalOK)); got != 1 {
		t.Errorf("ok evaluations = %v, want 1", got)
	}
}

// No delivery in the window AND upstream activity exists is the dark signature —
// matter arrived upstream while the receive path stayed silent. The gauge trips
// to 1 and the confirmation JQL is scoped to the webhook projects + window.
func TestEvaluate_NoDeliveryWithUpstreamActivity_Dark(t *testing.T) {
	resetWebhookMetrics()
	now := time.Unix(1_700_000_000, 0)
	lastWebhookReceivedNano.Store(now.Add(-3 * time.Hour).UnixNano())
	counter := &webhookFakeCounter{count: 67}
	m := testMonitor(counter, 2*time.Hour, now)

	m.evaluate(context.Background())

	if got := testutil.ToFloat64(jiraWebhookDark); got != 1 {
		t.Errorf("dark gauge = %v, want 1 (no delivery in window, upstream activity)", got)
	}
	if got := testutil.ToFloat64(jiraWebhookUpstreamMatches); got != 67 {
		t.Errorf("upstream_matches = %v, want 67", got)
	}
	if counter.gotJQL != `project in ("PE","DEVOPS") AND updated >= -120m` {
		t.Errorf("confirmation JQL = %q, want webhook-scope + window", counter.gotJQL)
	}
}

// The wrong-Secret case: the hook is registered and Jira is delivering, but the
// Secret is wrong, so every delivery is hmac_failed and NONE stamps the clock.
// With upstream activity present, this must page — not read as healthy. (Bead
// ACCEPTANCE clause.)
func TestEvaluate_WrongSecretHMACFailuresOnly_Dark(t *testing.T) {
	resetWebhookMetrics()
	now := time.Unix(1_700_000_000, 0)
	// Seed the startup grace to well before the window so the pod is past grace.
	lastWebhookReceivedNano.Store(now.Add(-3 * time.Hour).UnixNano())
	// A stream of HMAC failures arrives — these increment the counter but never
	// stamp the liveness clock.
	for range 20 {
		recordWebhookOutcome(webhookOutcomeHMACFailed, time.Time{})
	}
	counter := &webhookFakeCounter{count: 5}
	m := testMonitor(counter, 2*time.Hour, now)

	m.evaluate(context.Background())

	if got := testutil.ToFloat64(jiraWebhookDark); got != 1 {
		t.Errorf("dark gauge = %v, want 1 (HMAC-failures-only must page, not read as healthy)", got)
	}
}

// No delivery in the window but genuinely quiet upstream (count == 0) is the
// healthy-quiet state the unfireable-alert trap warns about: no comments =>
// no page. The gauge stays 0.
func TestEvaluate_NoDeliveryQuietUpstream_NotDark(t *testing.T) {
	resetWebhookMetrics()
	now := time.Unix(1_700_000_000, 0)
	lastWebhookReceivedNano.Store(now.Add(-3 * time.Hour).UnixNano())
	counter := &webhookFakeCounter{count: 0}
	m := testMonitor(counter, 2*time.Hour, now)

	m.evaluate(context.Background())

	if got := testutil.ToFloat64(jiraWebhookDark); got != 0 {
		t.Errorf("dark gauge = %v, want 0 (quiet upstream must not page)", got)
	}
}

// A count-endpoint error on a no-delivery cycle must NOT page: darkness cannot
// be confirmed, a real dark state persists to the next cycle, and firing on an
// unconfirmed guess would false-page. The verdict clears to 0 and the cycle is
// counted as count_error (so the deadman leg still sees a heartbeat).
func TestEvaluate_CountError_NotDark(t *testing.T) {
	resetWebhookMetrics()
	now := time.Unix(1_700_000_000, 0)
	lastWebhookReceivedNano.Store(now.Add(-3 * time.Hour).UnixNano())
	jiraWebhookDark.Set(1) // must be cleared even from a prior tripped state
	counter := &webhookFakeCounter{err: errors.New("boom")}
	m := testMonitor(counter, 2*time.Hour, now)

	m.evaluate(context.Background())

	if got := testutil.ToFloat64(jiraWebhookDark); got != 0 {
		t.Errorf("dark gauge = %v, want 0 (unconfirmed count error must not page)", got)
	}
	if got := testutil.ToFloat64(jiraWebhookEvaluations.WithLabelValues(webhookEvalCountError)); got != 1 {
		t.Errorf("count_error evaluations = %v, want 1 (heartbeat must still beat)", got)
	}
}

// initWebhookMetrics touches every series to zero and seeds the liveness clock
// to startup (only if unset), so increase()/absent() have a baseline the instant
// the pod is first scraped and a fresh pod gets one window of grace.
func TestInitWebhookMetrics_BaselinesAndSeedsGrace(t *testing.T) {
	resetWebhookMetrics()
	now := time.Unix(1_700_000_000, 0)
	initWebhookMetrics(now)

	for _, o := range []string{webhookOutcomeProcessed, webhookOutcomeIgnored, webhookOutcomeHMACFailed, webhookOutcomeBadRequest} {
		if got := testutil.ToFloat64(jiraWebhookDeliveries.WithLabelValues(o)); got != 0 {
			t.Errorf("deliveries{outcome=%q} baseline = %v, want 0 (series must exist)", o, got)
		}
	}
	for _, r := range []string{webhookEvalOK, webhookEvalCountError} {
		if got := testutil.ToFloat64(jiraWebhookEvaluations.WithLabelValues(r)); got != 0 {
			t.Errorf("evaluations{result=%q} baseline = %v, want 0", r, got)
		}
	}
	if lastWebhookReceivedNano.Load() != now.UnixNano() {
		t.Errorf("liveness clock = %v, want seeded to startup %v", lastWebhookReceivedNano.Load(), now.UnixNano())
	}

	// A subsequent init must NOT clobber a clock a real delivery already stamped.
	later := now.Add(time.Hour)
	recordWebhookOutcome(webhookOutcomeProcessed, time.Time{})
	stamped := lastWebhookReceivedNano.Load()
	initWebhookMetrics(later)
	if lastWebhookReceivedNano.Load() != stamped {
		t.Errorf("init must not overwrite a real delivery's clock: got %v, want %v", lastWebhookReceivedNano.Load(), stamped)
	}
}

// A fresh pod (clock seeded to startup, no delivery yet) must be within grace on
// the first evaluation — a restart must never immediately page.
func TestEvaluate_FreshPodWithinGrace_NotDark(t *testing.T) {
	resetWebhookMetrics()
	start := time.Unix(1_700_000_000, 0)
	counter := &webhookFakeCounter{count: 999}
	m := testMonitor(counter, 2*time.Hour, start)
	initWebhookMetrics(start) // seeds clock to start

	m.evaluate(context.Background()) // evaluated at the same instant

	if got := testutil.ToFloat64(jiraWebhookDark); got != 0 {
		t.Errorf("dark gauge = %v, want 0 (fresh pod is within its grace window)", got)
	}
	if counter.calls != 0 {
		t.Errorf("a within-grace pod must not probe upstream, got %d calls", counter.calls)
	}
}

// serveJiraWebhook posts one request through a handler and returns the recorder.
// devMode / secret / method / signature are the knobs the outcome tests vary.
func serveJiraWebhook(t *testing.T, cfg JiraWebhookConfig, method string, body []byte, sig string) *httptest.ResponseRecorder {
	t.Helper()
	handler := JiraWebhookHandler(cfg)
	req := httptest.NewRequest(method, "/webhook", bytes.NewReader(body))
	if sig != "" {
		req.Header.Set("X-Hub-Signature", sig)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

// The receive-path counter must tag each terminal outcome so a
// still-arriving-but-rejected stream is distinguishable from silence, and the
// authenticated outcomes (processed|ignored) must stamp the liveness clock while
// the rejected ones (hmac_failed|bad_request) must not. This is the wiring that
// makes the wrong-Secret case page instead of reading as healthy.
func TestJiraWebhookHandler_RecordsOutcomes(t *testing.T) {
	secret := "s3cr3t"

	t.Run("processed stamps clock", func(t *testing.T) {
		resetWebhookMetrics()
		cfg := JiraWebhookConfig{Daemon: newMockJiraWebhookDaemon(), DevMode: true, Logger: slog.Default()}
		body := makeJiraWebhookPayload("comment_created", "PE-1", "hi", "User")
		w := serveJiraWebhook(t, cfg, http.MethodPost, body, "")
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200", w.Code)
		}
		if got := testutil.ToFloat64(jiraWebhookDeliveries.WithLabelValues(webhookOutcomeProcessed)); got != 1 {
			t.Errorf("processed count = %v, want 1", got)
		}
		if lastWebhookReceivedNano.Load() == 0 {
			t.Error("processed delivery must stamp the liveness clock")
		}
	})

	t.Run("ignored (unhandled event) stamps clock", func(t *testing.T) {
		resetWebhookMetrics()
		cfg := JiraWebhookConfig{Daemon: newMockJiraWebhookDaemon(), DevMode: true, Logger: slog.Default()}
		body := makeJiraWebhookPayload("issue_updated", "PE-1", "", "")
		if w := serveJiraWebhook(t, cfg, http.MethodPost, body, ""); w.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200", w.Code)
		}
		if got := testutil.ToFloat64(jiraWebhookDeliveries.WithLabelValues(webhookOutcomeIgnored)); got != 1 {
			t.Errorf("ignored count = %v, want 1", got)
		}
		if lastWebhookReceivedNano.Load() == 0 {
			t.Error("an authenticated-but-ignored delivery still proves the hook is live — it must stamp the clock")
		}
	})

	t.Run("hmac failure does not stamp clock", func(t *testing.T) {
		resetWebhookMetrics()
		cfg := JiraWebhookConfig{Daemon: newMockJiraWebhookDaemon(), WebhookSecret: secret, Logger: slog.Default()}
		body := makeJiraWebhookPayload("comment_created", "PE-1", "hi", "User")
		w := serveJiraWebhook(t, cfg, http.MethodPost, body, "sha256=deadbeef")
		if w.Code != http.StatusForbidden {
			t.Fatalf("code = %d, want 403", w.Code)
		}
		if got := testutil.ToFloat64(jiraWebhookDeliveries.WithLabelValues(webhookOutcomeHMACFailed)); got != 1 {
			t.Errorf("hmac_failed count = %v, want 1", got)
		}
		if lastWebhookReceivedNano.Load() != 0 {
			t.Error("an HMAC failure must NOT stamp the clock (wrong Secret must not read as a live hook)")
		}
	})

	t.Run("missing secret records hmac failure", func(t *testing.T) {
		resetWebhookMetrics()
		cfg := JiraWebhookConfig{Daemon: newMockJiraWebhookDaemon(), Logger: slog.Default()} // no secret, no dev mode
		body := makeJiraWebhookPayload("comment_created", "PE-1", "hi", "User")
		w := serveJiraWebhook(t, cfg, http.MethodPost, body, "")
		if w.Code != http.StatusForbidden {
			t.Fatalf("code = %d, want 403", w.Code)
		}
		if got := testutil.ToFloat64(jiraWebhookDeliveries.WithLabelValues(webhookOutcomeHMACFailed)); got != 1 {
			t.Errorf("hmac_failed count = %v, want 1", got)
		}
	})

	t.Run("wrong method records bad_request", func(t *testing.T) {
		resetWebhookMetrics()
		cfg := JiraWebhookConfig{Daemon: newMockJiraWebhookDaemon(), DevMode: true, Logger: slog.Default()}
		w := serveJiraWebhook(t, cfg, http.MethodGet, nil, "")
		if w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("code = %d, want 405", w.Code)
		}
		if got := testutil.ToFloat64(jiraWebhookDeliveries.WithLabelValues(webhookOutcomeBadRequest)); got != 1 {
			t.Errorf("bad_request count = %v, want 1", got)
		}
		if lastWebhookReceivedNano.Load() != 0 {
			t.Error("a bad request must NOT stamp the clock")
		}
	})

	t.Run("undecodable body records bad_request", func(t *testing.T) {
		resetWebhookMetrics()
		cfg := JiraWebhookConfig{Daemon: newMockJiraWebhookDaemon(), DevMode: true, Logger: slog.Default()}
		w := serveJiraWebhook(t, cfg, http.MethodPost, []byte("{not json"), "")
		if w.Code != http.StatusBadRequest {
			t.Fatalf("code = %d, want 400", w.Code)
		}
		if got := testutil.ToFloat64(jiraWebhookDeliveries.WithLabelValues(webhookOutcomeBadRequest)); got != 1 {
			t.Errorf("bad_request count = %v, want 1", got)
		}
	})

	t.Run("valid signature is processed and stamps clock", func(t *testing.T) {
		resetWebhookMetrics()
		cfg := JiraWebhookConfig{Daemon: newMockJiraWebhookDaemon(), WebhookSecret: secret, Logger: slog.Default()}
		body := makeJiraWebhookPayload("comment_created", "PE-1", "hi", "User")
		w := serveJiraWebhook(t, cfg, http.MethodPost, body, signPayload(body, secret))
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200", w.Code)
		}
		if got := testutil.ToFloat64(jiraWebhookDeliveries.WithLabelValues(webhookOutcomeProcessed)); got != 1 {
			t.Errorf("processed count = %v, want 1", got)
		}
		if lastWebhookReceivedNano.Load() == 0 {
			t.Error("a validly-signed delivery must stamp the liveness clock")
		}
	})
}

func TestBuildWebhookScopeJQL(t *testing.T) {
	got := buildWebhookScopeJQL([]string{"PE", "DEVOPS"}, 90*time.Minute)
	want := `project in ("PE","DEVOPS") AND updated >= -90m`
	if got != want {
		t.Errorf("buildWebhookScopeJQL = %q, want %q", got, want)
	}
	// A sub-minute window floors to 1m so the JQL is always well-formed.
	if got := buildWebhookScopeJQL([]string{"PE"}, 10*time.Second); got != `project in ("PE") AND updated >= -1m` {
		t.Errorf("sub-minute window must floor to -1m, got %q", got)
	}
}
