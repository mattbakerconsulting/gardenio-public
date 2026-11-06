package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

// ProbeMetrics holds the liveness signals for the rate-limit probe loop
// (ka-0000001889). The 2026-07-05 incident was a goroutine-scoped freeze of
// the probe RunLoop: it stopped ticking for ~24h while the controller process
// stayed alive, silently disabling BOTH exhausted-account auto-recovery paths
// (SweepStaleExhausted and reconcileLegacyStatusFields) with nothing detecting
// or recovering from it.
//
// The headline series is probe_loop_ticks_total: it MUST advance every tick
// (~1/min). A flatlined counter is the visible signature of a dead loop — the
// alertable "probe loop dead" signal that was missing during the incident.
// probe_loop_seconds_since_last_tick is the watchdog's freshness gauge (how
// stale the last tick is); a value climbing past the stall threshold means the
// loop has wedged. panics_recovered and stalls_detected count the two failure
// modes the hardening now survives rather than dies on.
type ProbeMetrics struct {
	// env is baked in at construction (GARDENIO_ENV), the cross-estate join key
	// every gardenio series carries.
	env string

	ticks             *prometheus.CounterVec
	panicsRecovered   *prometheus.CounterVec
	stallsDetected    *prometheus.CounterVec
	secsSinceLastTick *prometheus.GaugeVec
}

// NewProbeMetrics constructs the rate-limit probe liveness metrics. env is
// attached to every series as the cross-estate join key. Register() must be
// called before the metrics are served.
func NewProbeMetrics(env string) *ProbeMetrics {
	return &ProbeMetrics{
		env: env,
		ticks: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "probe_loop_ticks_total",
			Help:      "Count of rate-limit probe RunLoop ticks completed (~1/min). MUST advance while the controller is alive; a flatlined series is the alertable signature of a wedged/dead probe loop — the silent-freeze failure mode from the 2026-07-05 idle-account incident (ka-0000001889). Label {env}.",
		}, []string{"env"}),
		panicsRecovered: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "probe_loop_panics_recovered_total",
			Help:      "Count of panics recovered inside the rate-limit probe loop (per-account probe or the probeAll scaffolding) that were logged and swallowed so a single panic can no longer kill RunLoop (ka-0000001889). Non-zero means a probe iteration is faulting; inspect the WARN log + stack. Label {env}.",
		}, []string{"env"}),
		stallsDetected: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "probe_loop_stalls_detected_total",
			Help:      "Count of times the probe watchdog observed the loop's last tick go stale beyond the stall threshold (default 10m) and raised a WARN + NATS alert (ka-0000001889). Non-zero means the probe loop wedged; the freeze that went undetected for ~24h on 2026-07-05 would show here. Label {env}.",
		}, []string{"env"}),
		secsSinceLastTick: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "probe_loop_seconds_since_last_tick",
			Help:      "Seconds elapsed since the rate-limit probe loop last completed a tick, sampled by the watchdog (ka-0000001889). Steady-state hovers near the tick interval (~60s); a value climbing without bound is a wedged loop. Alert on this crossing the stall threshold. Label {env}.",
		}, []string{"env"}),
	}
}

// Register registers the probe liveness metrics with reg. Served alongside the
// other controller metrics on the :9092 /metrics endpoint. A nil receiver is a
// no-op.
func (m *ProbeMetrics) Register(reg prometheus.Registerer) error {
	if m == nil {
		return nil
	}
	if err := reg.Register(m.ticks); err != nil {
		return err
	}
	if err := reg.Register(m.panicsRecovered); err != nil {
		return err
	}
	if err := reg.Register(m.stallsDetected); err != nil {
		return err
	}
	return reg.Register(m.secsSinceLastTick)
}

// RecordTick records that the probe loop completed a tick. Nil receiver is a
// no-op. It also resets the staleness gauge to 0 — a fresh tick means zero
// seconds since the last tick.
func (m *ProbeMetrics) RecordTick() {
	if m == nil {
		return
	}
	m.ticks.WithLabelValues(m.env).Inc()
	m.secsSinceLastTick.WithLabelValues(m.env).Set(0)
}

// RecordPanicRecovered records that a panic was recovered inside the probe
// loop. Nil receiver is a no-op.
func (m *ProbeMetrics) RecordPanicRecovered() {
	if m == nil {
		return
	}
	m.panicsRecovered.WithLabelValues(m.env).Inc()
}

// RecordStallDetected records that the watchdog observed a stalled probe loop.
// Nil receiver is a no-op.
func (m *ProbeMetrics) RecordStallDetected() {
	if m == nil {
		return
	}
	m.stallsDetected.WithLabelValues(m.env).Inc()
}

// SetSecondsSinceLastTick publishes the watchdog's freshness observation. Nil
// receiver or a negative value is a no-op (a negative delta implies a clock
// skew we don't want to expose as a metric).
func (m *ProbeMetrics) SetSecondsSinceLastTick(secs float64) {
	if m == nil || secs < 0 {
		return
	}
	m.secsSinceLastTick.WithLabelValues(m.env).Set(secs)
}
