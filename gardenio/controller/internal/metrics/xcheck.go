package metrics

import (
	"math"

	"github.com/prometheus/client_golang/prometheus"
)

// XCheckMetrics is the cost-audit cross-check surface (C2, plan ka-0000003747,
// bundle ka-0000004761). It implements the operator's "double-check principle":
// if you can add up the same quantity from two independent data sources and they
// are close, you are probably measuring something real; a single-source number is
// only ever spot-checkable by hand.
//
// The periodic cost-audit sweep (cmd/controller/xcheck_sweep.go) computes a set
// of conservation PAIRS — the same quantity derived two independent ways — and
// publishes each as three series:
//
//	gardenio_xcheck_value{pair,side="a",env}     — side A's number (for graphing)
//	gardenio_xcheck_value{pair,side="b",env}     — side B's number (for graphing)
//	gardenio_xcheck_agreement{pair,env}          — 1 − |A−B| / max(A,B,ε), the
//	                                              corroboration score in [0,1]
//
// The agreement score is the headline: 1.0 means the two derivations agree
// exactly, and it falls toward 0 as they diverge. A per-pair CostXCheckDrift
// alert (C5) fires when a pair's agreement stays below its threshold, and the
// beads-viewer /costs trust strip (C6) badges each number with the agreement of
// the pair(s) that qualify it.
//
// These are GAUGES, not counters: each is an absolute corroboration snapshot the
// sweep re-Sets every cycle (a pair whose sources vanish is Reset away so it does
// not linger stale — the poller-gauge idiom, not the counter anti-pattern). The
// env label disambiguates prod / gardenio-next / e2e the same way the
// account-cost and tag-cost gauges do.
type XCheckMetrics struct {
	// env is baked in at construction (GARDENIO_ENV), stamped on every series so
	// a shared vmsingle can distinguish estates. An empty env still emits.
	env string

	// value carries both sides of every pair so a Grafana panel can overlay
	// side A against side B (and, for ratio-style pairs like P7 whose agreement
	// is expected < 1 by design, chart the raw ratio B/A). Labels {pair,side,env};
	// side ∈ {"a","b"}.
	value *prometheus.GaugeVec

	// agreement is the corroboration score in [0,1] per pair (labels {pair,env}).
	// The alert-bearing series: sustained low agreement means two independent
	// derivations of one number disagree — a real measurement bug, not a vibe.
	agreement *prometheus.GaugeVec
}

// XCheck side label values.
const (
	XCheckSideA = "a"
	XCheckSideB = "b"
)

// xcheckAgreementEpsilon is the ε floor in the agreement denominator. It keeps
// the score defined (and equal to 1.0) when both sides are 0 — two derivations
// that both legitimately sum to nothing DO agree — and prevents a divide-by-zero
// or an absurd score when both sides are sub-cent noise.
const xcheckAgreementEpsilon = 1e-9

// NewXCheckMetrics constructs the cross-check gauge vectors. env is stamped on
// every series. Register() must be called to expose them on the controller
// :9092 /metrics endpoint.
func NewXCheckMetrics(env string) *XCheckMetrics {
	return &XCheckMetrics{
		env: env,
		value: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "xcheck_value",
			Help:      "One side of a cost-audit conservation pair: the same quantity derived independently as side a or side b (cmd/controller/xcheck_sweep.go). Labels {pair,side,env}; side ∈ {a,b}. Graph side a against side b, or their ratio, per pair. The paired corroboration score is gardenio_xcheck_agreement{pair}.",
		}, []string{"pair", "side", "env"}),
		agreement: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "xcheck_agreement",
			Help:      "Corroboration score in [0,1] for a cost-audit conservation pair: 1 − |A−B| / max(A,B,ε), where A and B are the pair's two independently-derived values (cmd/controller/xcheck_sweep.go). 1.0 = the two derivations agree; falls toward 0 as they diverge. Labels {pair,env}. The CostXCheckDrift alert fires on sustained low agreement.",
		}, []string{"pair", "env"}),
	}
}

// Register registers the cross-check gauges with reg, served alongside the other
// controller metrics on the :9092 /metrics endpoint.
func (x *XCheckMetrics) Register(reg prometheus.Registerer) error {
	if err := reg.Register(x.value); err != nil {
		return err
	}
	return reg.Register(x.agreement)
}

// XCheckAgreement computes the corroboration score 1 − |a−b| / max(a,b,ε) for
// two independently-derived values, clamped to [0,1]. Exported (and pure) so the
// sweep, its tests, and the beads-viewer trust strip all score a pair the same
// way. Negative inputs are clamped to 0 first (both sides of every pair are sums
// of non-negative terms, so a negative is a caller/source bug that must not
// inflate the score by widening the denominator).
func XCheckAgreement(a, b float64) float64 {
	a = math.Max(a, 0)
	b = math.Max(b, 0)
	denom := math.Max(math.Max(a, b), xcheckAgreementEpsilon)
	score := 1 - math.Abs(a-b)/denom
	if score < 0 {
		return 0
	}
	if score > 1 {
		return 1
	}
	return score
}

// SetPair publishes one conservation pair: both value sides and the derived
// agreement score, all stamped with the construction env. Call it only when BOTH
// sides were successfully derived this cycle — a pair missing a side must be left
// absent (the sweep skips it), so the /costs strip and the alert read "no data"
// rather than a fabricated agreement. No-op on a nil receiver so the sweep runs
// metrics-less in tests.
func (x *XCheckMetrics) SetPair(pair string, a, b float64) {
	if x == nil {
		return
	}
	x.value.WithLabelValues(pair, XCheckSideA, x.env).Set(a)
	x.value.WithLabelValues(pair, XCheckSideB, x.env).Set(b)
	x.agreement.WithLabelValues(pair, x.env).Set(XCheckAgreement(a, b))
}

// ClearPair removes a pair's three series (both value sides + agreement) for the
// construction env. The sweep calls it when a pair that previously published can
// no longer derive both sides, so a stale corroboration score does not linger and
// silently reassure while the underlying source is broken. No-op on a nil receiver.
func (x *XCheckMetrics) ClearPair(pair string) {
	if x == nil {
		return
	}
	x.value.DeleteLabelValues(pair, XCheckSideA, x.env)
	x.value.DeleteLabelValues(pair, XCheckSideB, x.env)
	x.agreement.DeleteLabelValues(pair, x.env)
}

// ValueGauge exposes gardenio_xcheck_value for test assertions via
// prometheus/testutil. Read-only for callers (use SetPair to mutate).
func (x *XCheckMetrics) ValueGauge() *prometheus.GaugeVec {
	if x == nil {
		return nil
	}
	return x.value
}

// AgreementGauge exposes gardenio_xcheck_agreement for test assertions via
// prometheus/testutil. Read-only for callers (use SetPair to mutate).
func (x *XCheckMetrics) AgreementGauge() *prometheus.GaugeVec {
	if x == nil {
		return nil
	}
	return x.agreement
}
