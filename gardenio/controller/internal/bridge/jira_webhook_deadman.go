package bridge

// Inbound-webhook receive-path liveness metrics + deadman monitor for the
// jira-bridge (bug ka-0000000426, parent epic ka-0000007401).
//
// THE DETECTION GAP THIS CLOSES. The jira-bridge has a poller-stall alert
// (jira_poller_stall.go, gardenio_jira_poller_stalled) guarding INGESTION and a
// trigger-starved alert (trigger_starve.go, gardenio_trigger_starved) guarding
// TRIGGERS. The inbound WEBHOOK receive path — @gardenio mentions, follow-up
// nudges, F3 re-spawn, comment mirroring — had NO metric and NO alert, so
// "receiving nothing, ever" was indistinguishable from "healthy and quiet."
// The Jira webhook was in fact unregistered in prod for 8 days while 67 real
// comments were created on PE/DEVOPS and every dashboard stayed green (root
// cause is operator-only escalation ka-0000004344; this file is the DETECTION,
// not the registration). The webhook receive path is the third leg with no
// guard — this adds it.
//
// WHY A MONITOR AND NOT A HANDLER-COMPUTED GAUGE. The ingestion poller runs a
// loop, so recordPollOutcome can fold its verdict per-cycle. The webhook path
// is event-driven and — in exactly the failure state we are detecting — SILENT:
// no request arrives to run any handler code, so the verdict cannot be computed
// there. JiraWebhookMonitor is a periodic evaluator (mirroring the JQL-count
// collector's Run loop) that folds the "0 authenticated deliveries in the
// window WHILE upstream activity exists" verdict into a single set-per-cycle
// gauge, gardenio_jira_webhook_dark.
//
// THE UNFIREABLE-ALERT TRAP (see jira_poller_stall.go for the sibling lesson).
// The alert must stay QUIET in the genuinely-quiet-upstream state — no comments
// in the window => no page — and fire only on "matter existed upstream and we
// received nothing." So the verdict has two arms: (a) no authenticated,
// well-formed delivery received within the deadman window, AND (b) an
// independent approximate-count of the webhook JQL scope (project in (PE,DEVOPS)
// updated in the window) still reports activity. Arm (a) keys on the last
// PROCESSED-or-IGNORED delivery, i.e. one that passed HMAC and decoded — NOT on
// total request count. That is deliberate: a wrong Secret in the Jira console
// looks like a working hook from Jira's side but only ever produces hmac_failed
// here, so keying "alive" on authenticated deliveries makes the wrong-Secret
// case page rather than read as healthy (bead ACCEPTANCE clause).
//
// FOLDED, NOT JOINED. The whole verdict lives in the gauge (a trivial ==1 in the
// rule) rather than a multi-term PromQL join, because an absent series inside an
// `and` silently suppresses the page — the exact failure the sibling
// trigger-starved Helm test guards against. A companion heartbeat counter
// (gardenio_jira_webhook_evaluations_total) backs a deadman leg that catches the
// OTHER failure shape the gauge alone cannot see: the monitor not running at all
// (pod down / hung goroutine), which would otherwise freeze the dark gauge at
// its last value and silently suppress the page.
//
// Registered globally against the default registerer (promauto), matching the
// other bridge metrics. The jira-bridge serves /metrics from
// prometheus.DefaultGatherer on the internal :9093 server, so these series are
// scrapeable; in the controller binary the monitor never runs, so the families
// stay at their zero baseline. The paging PrometheusRule lives in
// helm/gardenio/templates/jira-bridge/webhook-deadman-alert.yaml.

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Terminal outcomes for gardenio_jira_webhook_deliveries_total. Every receive
// path in JiraWebhookHandler records exactly one, so a still-arriving-but-
// rejected stream (hmac_failed / bad_request) is distinguishable from silence.
const (
	webhookOutcomeProcessed  = "processed"   // event dispatched to a handler (200 processed)
	webhookOutcomeIgnored    = "ignored"     // authenticated, decoded, but unhandled event type (200 ignored)
	webhookOutcomeHMACFailed = "hmac_failed" // signature verification failed / secret not configured (403)
	webhookOutcomeBadRequest = "bad_request" // wrong method, unreadable body, or undecodable payload (400/405)
)

// Heartbeat labels for gardenio_jira_webhook_evaluations_total — the deadman
// signal for the monitor loop itself. Incremented on EVERY evaluation cycle so
// a live-but-erroring monitor keeps the deadman quiet while a dead/absent
// monitor fires.
const (
	webhookEvalOK         = "ok"          // evaluation completed (verdict set)
	webhookEvalCountError = "count_error" // approximate-count failed; verdict left cleared this cycle
)

// defaultWebhookDeadmanWindow is how long the receive path may go without an
// authenticated delivery before it is judged dark (given upstream activity
// exists). 2h is long enough that a healthy but momentarily quiet hook is never
// flagged, while the real dark case (deregistered / wrong-secret) persists for
// hours and pages. Overridable via JIRA_WEBHOOK_DEADMAN_WINDOW.
const defaultWebhookDeadmanWindow = 2 * time.Hour

// defaultWebhookDeadmanInterval is the monitor's evaluation cadence. Also the
// heartbeat cadence the deadman leg reads. Overridable via
// JIRA_WEBHOOK_DEADMAN_INTERVAL.
const defaultWebhookDeadmanInterval = 5 * time.Minute

var (
	// gardenio_jira_webhook_deliveries_total counts inbound webhook deliveries by
	// terminal outcome. This is BOTH the observability surface (a rejected
	// stream shows as hmac_failed/bad_request instead of silence) and the raw
	// receive-liveness signal. Note it CANNOT be the deadman heartbeat: in the
	// fully-dark case (deregistered hook) nothing increments it at all — that is
	// exactly the state being detected — so the monitor keeps its own heartbeat
	// counter below.
	jiraWebhookDeliveries = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gardenio_jira_webhook_deliveries_total",
		Help: "Inbound JIRA webhook deliveries by terminal outcome (processed|ignored|hmac_failed|bad_request); the receive-path liveness signal.",
	}, []string{"outcome"})

	// gardenio_jira_webhook_dark is the Go-computed dark verdict: 1 while no
	// authenticated, well-formed inbound delivery has been received within the
	// deadman window AND an independent approximate-count of the webhook JQL
	// scope still reports upstream activity — matter existed upstream and we
	// received nothing. 0 in every healthy state (a delivery arrived recently)
	// and in the genuinely-quiet-upstream state (no activity => no page). The
	// verdict is folded entirely into this gauge (not a multi-term PromQL join)
	// so the alert expr is a trivial ==1 that an absent series cannot suppress.
	jiraWebhookDark = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "gardenio_jira_webhook_dark",
		Help: "1 while no authenticated well-formed inbound JIRA webhook delivery was received within the deadman window while upstream activity exists on the webhook JQL scope; 0 when healthy or genuinely quiet upstream.",
	})

	// gardenio_jira_webhook_evaluations_total counts completed monitor evaluation
	// cycles by result. The deadman leg reads this: increase over the window ==
	// 0 (or the series absent) means the monitor is not evaluating at all — a
	// dead/hung monitor or a down pod that would otherwise freeze the dark gauge
	// and silently suppress the page.
	jiraWebhookEvaluations = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gardenio_jira_webhook_evaluations_total",
		Help: "JIRA webhook deadman monitor evaluation cycles by result (ok|count_error); the deadman signal for the monitor loop.",
	}, []string{"result"})

	// gardenio_jira_webhook_upstream_matches is the most recent approximate-count
	// of the webhook JQL scope observed on a dark evaluation — the upstream side
	// of the verdict, exposed as context so an operator can see how much activity
	// was arriving upstream while the receive path stayed silent. Populated only
	// on the (rare) no-delivery branch; not paged on directly.
	jiraWebhookUpstreamMatches = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "gardenio_jira_webhook_upstream_matches",
		Help: "Approximate-count of recent activity on the webhook JQL scope observed on the most recent dark evaluation (context for the dark gauge).",
	})

	// lastWebhookReceivedNano is the wall-clock (UnixNano) of the most recent
	// authenticated, well-formed delivery (processed|ignored). Written by
	// recordWebhookOutcome; read by the monitor. Seeded to pod startup (only if
	// still unset) so a fresh pod gets one deadman window of grace before it can
	// be judged dark — a restart must not immediately page.
	lastWebhookReceivedNano atomic.Int64
)

// recordWebhookOutcome accounts one terminal receive-path outcome and, for the
// two authenticated-and-decoded outcomes (processed|ignored), stamps the
// receive-liveness clock. hmac_failed and bad_request deliberately do NOT stamp
// it: a delivery that failed HMAC (wrong Secret) or was malformed does not prove
// the webhook is registered and working, so it must not keep the deadman quiet.
//
// started is when the handler began serving this delivery; it feeds the handling
// -latency histogram (webhook_latency.go). It is threaded through this funnel
// rather than observed by a defer in the handler precisely because every
// terminal path already calls here exactly once — that makes the histogram's
// population identical to the counter's by construction. Pass the zero Time
// when accounting an outcome outside a real request (tests driving the deadman
// clock); the observation is then skipped.
func recordWebhookOutcome(outcome string, started time.Time) {
	jiraWebhookDeliveries.WithLabelValues(outcome).Inc()
	observeWebhookHandling(webhookSourceJira, outcome, started)
	if outcome == webhookOutcomeProcessed || outcome == webhookOutcomeIgnored {
		lastWebhookReceivedNano.Store(time.Now().UnixNano())
	}
}

// initWebhookMetrics touches every webhook-metric series to its zero baseline so
// increase()/absent() have a sample the instant the pod is first scraped —
// following the initPollerMetrics() precedent (jira_poller_stall.go), which
// exists for exactly this reason. It also seeds the receive-liveness clock to
// startup (only if no delivery has already stamped it), giving a fresh pod one
// deadman window of grace. Called once at the top of the monitor's Run.
func initWebhookMetrics(now time.Time) {
	jiraWebhookDeliveries.WithLabelValues(webhookOutcomeProcessed).Add(0)
	jiraWebhookDeliveries.WithLabelValues(webhookOutcomeIgnored).Add(0)
	jiraWebhookDeliveries.WithLabelValues(webhookOutcomeHMACFailed).Add(0)
	jiraWebhookDeliveries.WithLabelValues(webhookOutcomeBadRequest).Add(0)
	initWebhookHandlingMetrics(webhookSourceJira,
		webhookOutcomeProcessed, webhookOutcomeIgnored,
		webhookOutcomeHMACFailed, webhookOutcomeBadRequest)
	jiraWebhookEvaluations.WithLabelValues(webhookEvalOK).Add(0)
	jiraWebhookEvaluations.WithLabelValues(webhookEvalCountError).Add(0)
	jiraWebhookDark.Set(0)
	jiraWebhookUpstreamMatches.Set(0)
	lastWebhookReceivedNano.CompareAndSwap(0, now.UnixNano())
}

// buildWebhookScopeJQL constructs the confirmation-arm JQL: recent activity on
// the webhook's project scope. `updated >= -Nm` bumps on new comments and issue
// edits — exactly the events the webhook delivers — so a non-zero count means a
// webhook-worthy event happened upstream in the window. No ORDER BY is appended
// (approximate-count 400s on one), so no stripOrderBy is needed here.
func buildWebhookScopeJQL(projects []string, window time.Duration) string {
	minutes := max(int(window.Minutes()), 1)
	return fmt.Sprintf("project in (%s) AND updated >= -%dm", quoteJQL(projects), minutes)
}

// JiraWebhookMonitorConfig configures the inbound-webhook deadman monitor.
type JiraWebhookMonitorConfig struct {
	Jira     jiraCounter      // ApproximateCount for the upstream-activity confirmation arm
	Projects []string         // webhook project scope (e.g. ["PE","DEVOPS"])
	Window   time.Duration    // deadman window; 0 uses defaultWebhookDeadmanWindow
	Interval time.Duration    // evaluation cadence; 0 uses defaultWebhookDeadmanInterval
	Logger   *slog.Logger     // default slog.Default()
	Clock    func() time.Time // injectable clock; default time.Now
}

// JiraWebhookMonitor periodically folds the inbound-webhook dark verdict into
// gardenio_jira_webhook_dark. It is the receive-path analog of the ingestion
// poller's per-cycle stall computation — necessary because the receive path is
// event-driven and silent in exactly the failure state being detected.
type JiraWebhookMonitor struct {
	counter  jiraCounter
	jql      string
	window   time.Duration
	interval time.Duration
	logger   *slog.Logger
	clock    func() time.Time
}

// NewJiraWebhookMonitor builds the monitor. Defaults: Window 2h, Interval 5m,
// Logger slog.Default(), Clock time.Now.
func NewJiraWebhookMonitor(cfg JiraWebhookMonitorConfig) *JiraWebhookMonitor {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	window := cfg.Window
	if window <= 0 {
		window = defaultWebhookDeadmanWindow
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = defaultWebhookDeadmanInterval
	}
	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}
	return &JiraWebhookMonitor{
		counter:  cfg.Jira,
		jql:      buildWebhookScopeJQL(cfg.Projects, window),
		window:   window,
		interval: interval,
		logger:   logger,
		clock:    clock,
	}
}

// Run baselines the metrics, evaluates once immediately, then on every tick
// until ctx is cancelled. Mirrors JiraMetricsCollector.Run / the poller loop.
func (m *JiraWebhookMonitor) Run(ctx context.Context) error {
	initWebhookMetrics(m.clock())
	m.evaluate(ctx)
	t := time.NewTicker(m.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			m.evaluate(ctx)
		}
	}
}

// evaluate (re)computes the dark verdict for one cycle and beats the heartbeat.
//
//   - An authenticated delivery within the window → healthy; verdict cleared to
//     0. No approximate-count call (the cheap early-out for the common case).
//   - Otherwise confirm whether matter actually exists upstream via the cheap
//     approximate-count endpoint. count > 0 with no delivery in the window is
//     the dark signature → 1. count == 0 (genuinely quiet upstream) → 0. A count
//     ERROR clears the verdict to 0 and logs: a single transient count failure
//     is not evidence of darkness, a real dark state persists to the next cycle,
//     and firing on an unconfirmed guess would false-page (same discipline as
//     recordPollOutcome).
func (m *JiraWebhookMonitor) evaluate(ctx context.Context) {
	now := m.clock()
	last := time.Unix(0, lastWebhookReceivedNano.Load())
	if now.Sub(last) < m.window {
		jiraWebhookEvaluations.WithLabelValues(webhookEvalOK).Inc()
		jiraWebhookDark.Set(0)
		return
	}

	count, err := m.counter.ApproximateCount(ctx, m.jql)
	if err != nil {
		jiraWebhookEvaluations.WithLabelValues(webhookEvalCountError).Inc()
		m.logger.Warn("jira-webhook deadman: approximate-count failed; not treating the receive path as dark",
			"jql", m.jql, "error", err)
		jiraWebhookDark.Set(0)
		return
	}

	jiraWebhookEvaluations.WithLabelValues(webhookEvalOK).Inc()
	jiraWebhookUpstreamMatches.Set(float64(count))
	if count > 0 {
		jiraWebhookDark.Set(1)
		m.logger.Warn("jira-webhook DARK: no authenticated inbound webhook delivery received within the deadman window while upstream activity exists — the inbound webhook path is receiving nothing (runbook: bead ka-0000000426; likely root cause: unregistered/mis-secreted Jira webhook, escalation ka-0000004344)",
			"jql", m.jql, "approximate_count", count, "window", m.window, "last_received", last)
		return
	}
	jiraWebhookDark.Set(0)
}
