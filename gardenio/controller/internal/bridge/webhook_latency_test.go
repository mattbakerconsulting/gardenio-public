package bridge

// Coverage for the inbound-webhook handling-latency histogram (ka-0000000565).
//
// The bug this instrumentation fixes was NOT "a number was wrong" — it was "the
// series a histogram_quantile() panel needs does not exist on the wire", which
// renders as an empty panel, not as an error. So the load-bearing assertions
// here are made against the SERVED Prometheus text exposition (the same surface
// the scraper reads) rather than against the in-process registry: testutil
// reads would stay green even if the HistogramVec never marshalled to _bucket
// lines, which is precisely the failure mode being guarded.

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// servedExposition renders the default registry through the real promhttp
// handler over HTTP — the exact bytes a Prometheus/VictoriaMetrics scrape sees.
func servedExposition(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(promhttp.HandlerFor(prometheus.DefaultGatherer, promhttp.HandlerOpts{}))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /metrics body: %v", err)
	}
	return string(body)
}

// servedHistogramSample returns the value of one sample line of the handling
// -latency histogram, selected by its rendered label set. suffix is "_bucket",
// "_sum" or "_count"; extra is any additional label text that must appear on the
// line (e.g. `le="0.5"`). found=false when no such line was marshalled.
func servedHistogramSample(text, suffix, source, outcome, extra string) (value float64, found bool) {
	name := "gardenio_webhook_handling_duration_seconds" + suffix + "{"
	for line := range strings.SplitSeq(text, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, name) {
			continue
		}
		if !strings.Contains(line, `source="`+source+`"`) || !strings.Contains(line, `outcome="`+outcome+`"`) {
			continue
		}
		if extra != "" && !strings.Contains(line, extra) {
			continue
		}
		idx := strings.LastIndex(line, " ")
		if idx < 0 {
			return 0, false
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(line[idx+1:]), 64)
		if err != nil {
			return 0, false
		}
		return f, true
	}
	return 0, false
}

// resetWebhookHandlingDuration clears every child of the histogram so a test can
// assert absolute counts. The vec lives on the package-global default registry
// shared with the rest of this package's tests, so each leg resets first.
func resetWebhookHandlingDuration() { webhookHandlingDuration.Reset() }

// TestWebhookHandlingDuration_ServedBucketsBackAQuantilePanel is the leg that
// pins the actual deliverable: after a real inbound delivery through the real
// handler, the served exposition carries `_bucket` samples with an `le` label,
// plus the `_sum`/`_count` pair — the complete set histogram_quantile() needs.
// Before this instrumentation the webhook path exported counters only, so the
// webhook-liveness board could not carry an honest distribution panel and the
// richness lane dropped it rather than ship one that renders empty.
func TestWebhookHandlingDuration_ServedBucketsBackAQuantilePanel(t *testing.T) {
	resetWebhookHandlingDuration()

	const secret = "the-real-secret"
	handler := GitLabWebhookHandlerWithConfig(GitLabWebhookConfig{
		Daemon:        newE2EDaemon(),
		WebhookSecret: secret,
		Logger:        slog.Default(),
	})

	// A wrong-token delivery: a genuine terminal path through the whole handler,
	// with no upstream dependency needed to reach it.
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("X-Gitlab-Token", "wrong")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("wrong token: got %d, want 403", w.Code)
	}

	served := servedExposition(t)

	// _bucket with an le= label is the series histogram_quantile() reads. Its
	// absence is exactly the "renders empty" failure this metric exists to fix.
	for _, le := range []string{`le="0.001"`, `le="30"`, `le="+Inf"`} {
		if _, found := servedHistogramSample(served, "_bucket", webhookSourceGitLab, gitlabWebhookOutcomeTokenFailed, le); !found {
			t.Errorf("gardenio_webhook_handling_duration_seconds_bucket{source=%q,outcome=%q,%s} absent from the served exposition — a histogram_quantile panel over this metric would render empty:\n%s",
				webhookSourceGitLab, gitlabWebhookOutcomeTokenFailed, le, served)
		}
	}

	// _count and _sum complete the histogram contract (avg-latency panels and
	// the counter-reconciliation check below both read them).
	count, found := servedHistogramSample(served, "_count", webhookSourceGitLab, gitlabWebhookOutcomeTokenFailed, "")
	if !found {
		t.Fatalf("gardenio_webhook_handling_duration_seconds_count absent from the served exposition:\n%s", served)
	}
	if count != 1 {
		t.Errorf("_count{source=gitlab,outcome=token_failed} = %v, want 1 (one delivery, one observation)", count)
	}
	if _, found := servedHistogramSample(served, "_sum", webhookSourceGitLab, gitlabWebhookOutcomeTokenFailed, ""); !found {
		t.Errorf("gardenio_webhook_handling_duration_seconds_sum absent from the served exposition:\n%s", served)
	}
}

// TestWebhookHandlingDuration_ObservesEveryTerminalPath proves the histogram
// counts the SAME population as the deliveries counter it sits beside. That
// equality is the whole reason the observation is threaded through the
// record*WebhookOutcome funnels instead of a defer: every terminal path already
// calls the funnel exactly once, so a newly-added early return cannot ship a
// counted-but-untimed delivery. A dashboard can assert the same identity live
// (sum(rate(..._count)) == sum(rate(..._total))).
func TestWebhookHandlingDuration_ObservesEveryTerminalPath(t *testing.T) {
	resetWebhookHandlingDuration()
	gitlabWebhookDeliveries.Reset()

	const secret = "secret"
	handler := GitLabWebhookHandlerWithConfig(GitLabWebhookConfig{
		Daemon:        newE2EDaemon(),
		WebhookSecret: secret,
		BotUsername:   "group_1_bot_x",
		Logger:        slog.Default(),
	})

	post := func(token string, payload map[string]any) {
		body, _ := json.Marshal(payload)
		req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
		req.Header.Set("X-Gitlab-Token", token)
		handler.ServeHTTP(httptest.NewRecorder(), req)
	}

	// Three deliveries down three DIFFERENT terminal paths: perimeter reject,
	// undecodable body, and an authenticated-but-unhandled object_kind.
	post("wrong", map[string]any{})                                    // token_failed
	post(secret, map[string]any{})                                     // bad_request (no object_kind match / decode)
	post(secret, map[string]any{"object_kind": "no_such_kind_at_all"}) // ignored

	served := servedExposition(t)

	// Every outcome the counter recorded must have an equal histogram _count.
	// Reading both off the same served text keeps the comparison honest.
	for _, outcome := range []string{
		gitlabWebhookOutcomeTokenFailed,
		gitlabWebhookOutcomeBadRequest,
		gitlabWebhookOutcomeIgnored,
		gitlabWebhookOutcomeProcessed,
		gitlabWebhookOutcomeError,
	} {
		counter, counterFound := servedDeliveriesValue(served, outcome)
		if !counterFound {
			continue // outcome not exercised by this leg
		}
		hist, histFound := servedHistogramSample(served, "_count", webhookSourceGitLab, outcome, "")
		if !histFound {
			t.Errorf("outcome %q was counted (%v) but has no histogram _count series — a terminal path increments the counter without timing the delivery", outcome, counter)
			continue
		}
		if hist != counter {
			t.Errorf("outcome %q: histogram _count = %v but deliveries_total = %v — the two must observe the same population", outcome, hist, counter)
		}
	}
}

// TestWebhookHandlingDuration_JiraDoorIsLabelledSeparately proves the Jira door
// emits under its own source= label. Both doors share one metric name so a
// single panel can chart them, which only works if the label actually
// distinguishes them — otherwise the two bridges' distributions silently merge.
func TestWebhookHandlingDuration_JiraDoorIsLabelledSeparately(t *testing.T) {
	resetWebhookHandlingDuration()

	handler := JiraWebhookHandler(JiraWebhookConfig{
		DevMode: true,
		Logger:  slog.Default(),
	})

	body, _ := json.Marshal(map[string]any{"webhookEvent": "no_such_event"})
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("unhandled event: got %d, want 200", w.Code)
	}

	served := servedExposition(t)

	if got, found := servedHistogramSample(served, "_count", webhookSourceJira, webhookOutcomeIgnored, ""); !found || got != 1 {
		t.Errorf("_count{source=%q,outcome=%q} = %v (found=%v), want 1 — the Jira door must time its deliveries under its own source label:\n%s",
			webhookSourceJira, webhookOutcomeIgnored, got, found, served)
	}
	// The GitLab door must not have absorbed the Jira delivery.
	if _, found := servedHistogramSample(served, "_count", webhookSourceGitLab, webhookOutcomeIgnored, ""); found {
		t.Errorf("a Jira delivery produced a source=%q series — the two doors are not separated", webhookSourceGitLab)
	}
}

// TestWebhookHandlingDuration_SeededBeforeFirstDelivery proves a cold pod serves
// the histogram's series from its first scrape, before any webhook arrives. An
// unseeded histogram is why a freshly-rolled bridge on a quiet hook would render
// an empty latency panel for hours — the same "renders empty" failure, arriving
// by a different route.
func TestWebhookHandlingDuration_SeededBeforeFirstDelivery(t *testing.T) {
	resetWebhookHandlingDuration()
	InitGitLabWebhookMetrics()

	served := servedExposition(t)

	for _, outcome := range []string{
		gitlabWebhookOutcomeProcessed,
		gitlabWebhookOutcomeIgnored,
		gitlabWebhookOutcomeTokenFailed,
		gitlabWebhookOutcomeBadRequest,
		gitlabWebhookOutcomeError,
	} {
		got, found := servedHistogramSample(served, "_count", webhookSourceGitLab, outcome, "")
		if !found {
			t.Errorf("_count{source=gitlab,outcome=%q} absent from a cold pod's exposition — the latency panel renders empty until the first delivery:\n%s", outcome, served)
			continue
		}
		if got != 0 {
			t.Errorf("_count{source=gitlab,outcome=%q} = %v on a cold pod, want 0", outcome, got)
		}
	}
}

// TestWebhookHandlingDuration_ZeroStartTimeIsNotObserved guards the funnel's
// escape hatch. Callers outside a real request (the deadman unit tests drive
// record*WebhookOutcome directly to move the liveness clock) pass the zero Time;
// observing it would add ~57 years to _sum and drag every quantile to the top
// bucket forever — a silently poisoned distribution that no counter would reveal.
func TestWebhookHandlingDuration_ZeroStartTimeIsNotObserved(t *testing.T) {
	resetWebhookHandlingDuration()

	observeWebhookHandling(webhookSourceGitLab, gitlabWebhookOutcomeProcessed, time.Time{})

	served := servedExposition(t)
	if got, found := servedHistogramSample(served, "_count", webhookSourceGitLab, gitlabWebhookOutcomeProcessed, ""); found && got != 0 {
		t.Errorf("_count after a zero-start-time call = %v, want no observation", got)
	}
}

// TestWebhookHandlingBuckets_MonotonicAndSpanTheObservedRange pins the bucket
// ladder's two structural properties. Prometheus requires strictly increasing
// bucket bounds (an out-of-order ladder panics at registration). The range
// bounds are asserted because they encode the design: a 1ms floor so the no-I/O
// reject paths are resolvable rather than all piling into the first bucket, and
// a 30s ceiling covering the pathological upstream stall past which the sender
// has already abandoned the delivery.
func TestWebhookHandlingBuckets_MonotonicAndSpanTheObservedRange(t *testing.T) {
	if len(webhookHandlingBuckets) == 0 {
		t.Fatal("webhookHandlingBuckets is empty")
	}
	for i := 1; i < len(webhookHandlingBuckets); i++ {
		if webhookHandlingBuckets[i] <= webhookHandlingBuckets[i-1] {
			t.Fatalf("buckets must be strictly increasing; [%d]=%v <= [%d]=%v",
				i, webhookHandlingBuckets[i], i-1, webhookHandlingBuckets[i-1])
		}
	}
	if first := webhookHandlingBuckets[0]; first > 0.001 {
		t.Errorf("bucket floor = %v, want <= 0.001s — the reject paths do no I/O and land sub-millisecond", first)
	}
	if last := webhookHandlingBuckets[len(webhookHandlingBuckets)-1]; last < 30 {
		t.Errorf("bucket ceiling = %v, want >= 30s — a stalled upstream must be distinguishable from a merely slow one", last)
	}
}
