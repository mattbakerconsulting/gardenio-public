// Prometheus metrics for the Claude status-page poller (claude_status.go),
// leg B of the Claude-outage detector (task ka-0000003022, epic ka-0000000487).
//
// These metrics are built WITHOUT promauto and are NOT registered at package
// var-init time. They register ONLY when a caller passes the struct to
// Register(reg) — the controller does so against its own promRegistry, the one
// its :9092 /metrics endpoint serves (cmd/controller/main.go). This mirrors leg
// A (internal/metrics/events.go: explicit NewGaugeVec + Register(promRegistry)).
//
// Why NOT promauto (the bug this file was rewritten to fix — ka-0000008266):
// promauto registers against prometheus.DefaultRegisterer at package var-init,
// which runs BEFORE controller/main.go swaps DefaultRegisterer to promRegistry.
// The series therefore landed on the ORIGINAL default registry, which :9092
// never serves — leg B was genuinely dark in prod. Worse, the four bridge
// binaries import this package for unrelated reasons, so the same var-init fired
// in THEM, registering gardenio_claude_status_* on their DefaultRegisterer (which
// each bridge DOES serve). They never run the poller, so they exported phantom
// zero series with last_check=0 (epoch 1970), making ClaudeStatusPollerStale
// fire forever. Struct-scoped registration keeps a bare import side-effect-free:
// importing the package registers nothing.
//
// The controller's in-chart vmagent already scrapes :9092 via the pod
// annotations, so no new scrape config is needed once the series live there —
// which is deliberate: the alert rules that read these series must be fireable
// the moment they ship.
//
// These are leg B (the provider's own claim about its health). They deliberately
// do NOT fire the page on their own — a status-page blip that never touches our
// traffic (claude.ai down while our API path is fine) must not wake anyone.
// They corroborate leg A (gardenio_agent_upstream_error_active, the agents that
// are actually failing) so the Slack message can self-diagnose "us" vs "them".
package bridge

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Ordinal severity encodings. Both ladders are ordered least→most severe so a
// PromQL rule is a simple threshold comparison (`>= 2`) rather than a regex over
// one-hot label values — the alert expressions stay readable and a newly-added
// upstream status slots into the ladder instead of silently matching nothing.
const (
	claudeIndicatorNone     = 0
	claudeIndicatorMinor    = 1
	claudeIndicatorMajor    = 2
	claudeIndicatorCritical = 3
)

const (
	claudeComponentOperational     = 0
	claudeComponentDegraded        = 1
	claudeComponentPartialOutage   = 2
	claudeComponentMajorOutage     = 3
	claudeComponentUnderMaintence  = 4
	claudeComponentUnknownFallback = -1
)

// ClaudeStatusMetrics holds the leg-B poller's Prometheus series. Built by
// NewClaudeStatusMetrics and registered on the caller's registry via Register —
// nothing here touches the global default registry, so importing this package
// (as the bridge binaries do) exports no phantom series.
type ClaudeStatusMetrics struct {
	indicator         prometheus.Gauge
	component         *prometheus.GaugeVec
	openIncidents     *prometheus.GaugeVec
	lastCheckTS       prometheus.Gauge
	checkErrors       prometheus.Counter
	checkErrorsReason *prometheus.CounterVec
}

// NewClaudeStatusMetrics builds the leg-B series without registering them. Call
// Register to attach them to a registry.
func NewClaudeStatusMetrics() *ClaudeStatusMetrics {
	return &ClaudeStatusMetrics{
		indicator: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Name: "gardenio_claude_status_indicator",
				Help: "Claude status-page overall indicator, ordinal severity: 0=none 1=minor 2=major 3=critical. Leg B of the Claude-outage detector (ka-0000000487) — corroborating context for ClaudeUpstreamOutage, NOT a pager on its own.",
			},
		),
		component: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "gardenio_claude_status_component",
				Help: "Per-component Claude status-page state, ordinal severity: 0=operational 1=degraded_performance 2=partial_outage 3=major_outage 4=under_maintenance. Labels {component} use stable gardenio slugs (claude_api/claude_code/claude_ai/claude_console/claude_cowork/claude_gov), NOT the opaque Statuspage component ids. `claude_api` is the one our agent traffic actually depends on.",
			},
			[]string{"component"},
		),
		openIncidents: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "gardenio_claude_status_open_incidents",
				Help: "Count of unresolved Claude status-page incidents by {impact} (none/minor/major/critical). Incident ids and titles are deliberately NOT labels — they are unbounded and would churn cardinality on every new incident; the poller exposes them as annotation text instead.",
			},
			[]string{"impact"},
		),
		lastCheckTS: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Name: "gardenio_claude_status_last_check_timestamp_seconds",
				Help: "Unix timestamp of the poller's last SUCCESSFUL fetch+parse. Deadman signal for the poller itself: a stale value means the detector is blind, which must be alertable or leg B silently rots.",
			},
		),
		checkErrors: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "gardenio_claude_status_check_errors_total",
				Help: "Fetch/parse failures against the Claude status page. On failure the poller leaves prior gauges STALE rather than zeroing them — writing 'all operational' on a failed fetch would let a network partition actively suppress the outage alert.",
			},
		),
		checkErrorsReason: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "gardenio_claude_status_check_errors_by_reason_total",
				Help: "Fetch/parse failures broken out by {reason} (tls/dns/connect/http_status/parse/empty/build_request/read_body/other). Lets ClaudeStatusPollerStale route the oncall correctly — a `tls` streak is a PROVIDER cert or a MITM, NOT our pod egress, so it should not send anyone chasing a network partition. Sums to gardenio_claude_status_check_errors_total.",
			},
			[]string{"reason"},
		),
	}
}

// Register attaches the leg-B series to reg. The controller calls this with its
// promRegistry (the one :9092 serves) ONLY when the poller is enabled, so a
// disabled poller exports nothing and ClaudeStatusPollerStale's absent() clause
// — not a phantom stale last_check — is what reports leg B as off.
func (m *ClaudeStatusMetrics) Register(reg prometheus.Registerer) error {
	collectors := []prometheus.Collector{
		m.indicator,
		m.component,
		m.openIncidents,
		m.lastCheckTS,
		m.checkErrors,
		m.checkErrorsReason,
	}
	for _, col := range collectors {
		if err := reg.Register(col); err != nil {
			return err
		}
	}
	return nil
}
