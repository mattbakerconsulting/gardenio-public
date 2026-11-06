// Prometheus metrics for the bridge fleet-token rotator (task ka-0000002353,
// plan item 3 — observability + "rotation overdue" alert). The rotator core
// (rotator.go) previously emitted only slog lines, so there was NO series a
// vmalert rule could fire on: a rotator that silently wedged (bad token, RBAC
// forbidden on the Secret Update, daemon 5xx) would let each fleet token walk
// straight into its TTL cliff with zero paging. These series close that gap.
//
// Built WITHOUT promauto and registered ONLY when the caller passes the struct
// to Register(reg) — mirroring internal/bridge/claude_status_metrics.go and
// internal/metrics/events.go. The controller registers them against its own
// promRegistry (the one :9092 serves, cmd/controller/main.go); the bridge
// binaries import neither this package nor register anything, so a bare import
// stays side-effect-free and no phantom zero series leak onto a bridge's own
// default registry (the exact bug ka-0000008266 was rewritten to avoid).
//
// The load-bearing series is gardenio_bridge_token_expires_at_timestamp_seconds:
// the rotator writes each configured fleet's token expiry (Unix seconds) on
// EVERY successful check tick, whether or not that fleet is a rotation
// candidate. The BridgeTokenRotationOverdue alert reads it directly —
// `expires_at - time() < <overdue>` — so it fires on the user-visible fact (a
// bridge token is about to expire) regardless of WHY the rotator fell behind.
// The rotator's in-chart vmagent already scrapes the controller :9092 via pod
// annotations, so no new scrape config is needed for the series to be fireable.
package bridgetokenrotator

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Metrics holds the rotator's Prometheus series. Built by NewMetrics and
// attached to a registry via Register — nothing here touches the global
// default registry, so importing this package exports no phantom series.
type Metrics struct {
	// expiresAt is the current token expiry, Unix seconds, per {fleet}. Set on
	// every successful check tick for every configured fleet. This is the
	// series BridgeTokenRotationOverdue evaluates: remaining TTL is
	// `expiresAt - time()`. A legacy/seed token with no daemon-reported expiry
	// is written as 0 (epoch), which reads as maximally-overdue — deliberate:
	// an un-TTL'd token is exactly the migrate-onto-rails case the alert should
	// surface, not hide.
	expiresAt *prometheus.GaugeVec

	// remainingSeconds mirrors expiresAt as a directly-readable "seconds until
	// expiry" gauge per {fleet}, for dashboards/eyeballing without an inline
	// `- time()`. The alert keys on expiresAt (absolute), which is immune to
	// clock skew between vmalert and the controller; this is convenience only.
	remainingSeconds *prometheus.GaugeVec

	// lastCheckTS is the Unix timestamp of the rotator's last SUCCESSFUL check
	// (daemon list parsed OK). Deadman for the loop itself: a stale value means
	// the rotator is blind and the expiry gauges below are frozen, which
	// BridgeTokenRotationOverdue's absent()/staleness clause must catch or the
	// rotator silently rots.
	lastCheckTS prometheus.Gauge

	// rotations counts rotation attempts per {fleet,result}. result is
	// success|dry_run|error. dry_run is emitted while DryRun=true so the
	// preprod observe window can confirm the loop is exercising the code path
	// without a single real write.
	rotations *prometheus.CounterVec

	// checkErrors counts failed check ticks (daemon list/parse failed). On a
	// failed check the rotator leaves the prior expiry gauges STALE rather than
	// zeroing them — writing 0 on a transient daemon 5xx would let a blip
	// actively trip the overdue alert.
	checkErrors prometheus.Counter
}

// Rotation result label values.
const (
	rotationResultSuccess = "success"
	rotationResultDryRun  = "dry_run"
	rotationResultError   = "error"
)

// NewMetrics builds the rotator series without registering them. Call Register
// to attach them to a registry.
func NewMetrics() *Metrics {
	return &Metrics{
		expiresAt: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "gardenio_bridge_token_expires_at_timestamp_seconds",
				Help: "Unix timestamp at which a bridge fleet token expires, per {fleet}. Written on every successful rotator check tick. 0 means the daemon reports no expiry (legacy/seed token) — read as maximally overdue. Backs BridgeTokenRotationOverdue.",
			},
			[]string{"fleet"},
		),
		remainingSeconds: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "gardenio_bridge_token_remaining_seconds",
				Help: "Seconds until a bridge fleet token expires, per {fleet} (expires_at - now at check time). Convenience gauge for dashboards; can go negative once expired. The alert keys on the absolute expires_at series, which is clock-skew-immune.",
			},
			[]string{"fleet"},
		),
		lastCheckTS: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Name: "gardenio_bridge_token_rotator_last_check_timestamp_seconds",
				Help: "Unix timestamp of the rotator's last SUCCESSFUL check (daemon token list parsed). Deadman for the rotation loop: a stale value means the expiry gauges are frozen and the rotator is blind.",
			},
		),
		rotations: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "gardenio_bridge_token_rotations_total",
				Help: "Bridge fleet-token rotation attempts by {fleet,result}. result: success|dry_run|error. dry_run counts the observe-mode code path so preprod can confirm the loop fires without a real write.",
			},
			[]string{"fleet", "result"},
		),
		checkErrors: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "gardenio_bridge_token_rotator_check_errors_total",
				Help: "Failed rotator check ticks (daemon list/parse failed). On failure the prior expiry gauges are left STALE, not zeroed, so a transient daemon blip cannot trip BridgeTokenRotationOverdue.",
			},
		),
	}
}

// Register attaches the rotator series to reg. The controller calls this with
// its promRegistry (the one :9092 serves) ONLY when the rotator is enabled, so
// a disabled rotator exports nothing and the alert's absent() clause — not a
// phantom stale gauge — reports the rotator as off.
func (m *Metrics) Register(reg prometheus.Registerer) error {
	collectors := []prometheus.Collector{
		m.expiresAt,
		m.remainingSeconds,
		m.lastCheckTS,
		m.rotations,
		m.checkErrors,
	}
	for _, col := range collectors {
		if err := reg.Register(col); err != nil {
			return err
		}
	}
	return nil
}

// observeExpiry records a fleet's token expiry (Unix seconds) and the derived
// remaining-seconds gauge. expiresAtUnix==0 (no daemon expiry) is written
// as-is so the overdue alert treats an un-TTL'd token as maximally overdue.
func (m *Metrics) observeExpiry(fleet string, expiresAtUnix, remainingSeconds float64) {
	if m == nil {
		return
	}
	m.expiresAt.WithLabelValues(fleet).Set(expiresAtUnix)
	m.remainingSeconds.WithLabelValues(fleet).Set(remainingSeconds)
}

// observeCheckOK stamps the deadman timestamp after a successful check tick.
func (m *Metrics) observeCheckOK(nowUnix float64) {
	if m == nil {
		return
	}
	m.lastCheckTS.Set(nowUnix)
}

// observeCheckError increments the check-error counter. The caller leaves the
// expiry gauges untouched (stale) on error.
func (m *Metrics) observeCheckError() {
	if m == nil {
		return
	}
	m.checkErrors.Inc()
}

// observeRotation increments the rotation counter for a fleet+result.
func (m *Metrics) observeRotation(fleet, result string) {
	if m == nil {
		return
	}
	m.rotations.WithLabelValues(fleet, result).Inc()
}
