package bridge

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Inbound-webhook HANDLING-LATENCY distribution (ka-0000000565).
//
// WHY THIS EXISTS. The non-AVA webhook carve-out (ka-0000005632, see
// gardenio/docs/security/non-ava-webhook-carveout.md) is compensated by logs and
// metrics, and both bridge doors already export receive-path COUNTERS
// (gardenio_{jira,gitlab}_webhook_deliveries_total{outcome}). Counters answer
// "is it arriving, and is it being rejected?" — they cannot answer "how long
// does a delivery take to handle?". With no _bucket series in the exposition,
// the webhook-liveness board could not carry an honest histogram_quantile
// panel: the GRADE richness lane reached for quantile_over_time() over an
// existing gauge, then DROPPED the proposed latency panel rather than ship one
// that renders empty. This histogram is the missing primitive.
//
// WHY IT MATTERS OPERATIONALLY. A webhook POST is synchronous from the sender's
// side: GitLab and Jira both time out and retry (or silently drop) a hook that
// answers too slowly, so a handler creeping from 200ms to 10s degrades into
// lost deliveries WITHOUT ever moving the deliveries counter — a rising p95 is
// the only early warning, and until now nothing recorded it.

// Webhook source label values for gardenio_webhook_handling_duration_seconds.
// One binary owns each door (cmd/jira-bridge, cmd/gitlab-bridge), so in prod a
// given pod only ever emits its own source — the label exists so a single
// Grafana panel can chart both doors from one metric name, the same reason the
// two outcome vocabularies were deliberately kept parallel.
const (
	webhookSourceJira   = "jira"
	webhookSourceGitLab = "gitlab"
)

// webhookHandlingBuckets spans the observed range of the two handlers' work.
//
// The reject paths (token_failed/hmac_failed/bad_request) do no I/O at all —
// a constant-time secret compare or a failed json.Unmarshal — and land in the
// sub-millisecond floor, so the ladder starts at 1ms rather than at the
// client_golang default 5ms; without that floor every rejected delivery would
// pile into the first bucket and a reject-path regression would be invisible.
// The processed paths make at least one kbeads daemon round-trip and usually
// one or more GitLab/Jira REST calls (fetch MR, post note, nudge or spawn an
// agent), which puts the interesting mass in the tens-of-ms to low-seconds
// range — hence the dense 10ms..1s rungs where p50/p95 actually live. The 30s
// top rung is the pathological upstream-stall case: past that the sender has
// already given up on the delivery, so finer resolution beyond it would buy
// nothing.
var webhookHandlingBuckets = []float64{
	0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30,
}

// gardenio_webhook_handling_duration_seconds is the wall-clock time each inbound
// webhook delivery spent inside the bridge handler, from the first statement of
// the HTTP handler to the terminal outcome that ends it.
//
// The outcome label is NOT decoration: the reject paths are three orders of
// magnitude faster than the processed paths, so an unlabelled histogram would
// have its quantiles dragged around by the arrival MIX (a burst of token_failed
// probes would "improve" p95 while real handling got slower). Split by outcome,
// a panel can chart the honest thing — the latency of deliveries the bridge
// actually did work for — and still sum over outcomes when the whole-door view
// is wanted.
var webhookHandlingDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "gardenio_webhook_handling_duration_seconds",
	Help:    "Inbound webhook handling latency in seconds by bridge door and terminal outcome — the distribution behind gardenio_{jira,gitlab}_webhook_deliveries_total (ka-0000000565).",
	Buckets: webhookHandlingBuckets,
}, []string{"source", "outcome"})

// observeWebhookHandling records one delivery's handling latency. Called only
// from the two record*WebhookOutcome funnels, which every terminal path in both
// handlers already traverses exactly once — so the histogram's count is the
// same population as the deliveries counter by construction, and the two must
// reconcile (sum(rate(..._duration_seconds_count)) == sum(rate(..._total))).
// That equality is the cheapest possible check that this instrumentation is
// wired to every path, and a dashboard can assert it directly.
func observeWebhookHandling(source, outcome string, started time.Time) {
	// A zero start time means a caller (today: only unit tests exercising the
	// deadman clock) is accounting an outcome outside a real request. Skip the
	// observation rather than poison the distribution with the seconds since
	// the Unix epoch.
	if started.IsZero() {
		return
	}
	webhookHandlingDuration.WithLabelValues(source, outcome).Observe(time.Since(started).Seconds())
}

// initWebhookHandlingMetrics touches every (source, outcome) child of the
// histogram for one door to its zero baseline, so rate()/histogram_quantile()
// have a series to work with the instant the pod is first scraped instead of
// only after the first delivery — the same reason initWebhookMetrics() and
// InitGitLabWebhookMetrics() seed their counters. An unseeded histogram is why
// a freshly-rolled bridge would render an empty latency panel for hours on a
// quiet hook, which is exactly the "renders empty" failure this metric exists
// to fix.
func initWebhookHandlingMetrics(source string, outcomes ...string) {
	for _, outcome := range outcomes {
		webhookHandlingDuration.WithLabelValues(source, outcome)
	}
}
