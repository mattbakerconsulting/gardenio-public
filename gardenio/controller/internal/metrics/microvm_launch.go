package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

// MicrovmLaunchMetrics holds the lambda_microvm launch-dispatch histogram
// emitted from the controller spawn door (task ka-0000003938, thread
// "plan gardeners on MicroVM"). It is the controller-owned slice of MicroVM
// spin-up: the wall-clock the spawn door spends turning a resolved agent spec
// into a live RunMicrovm handle.
//
// launch dispatch = t(backend.Launch returned a live handle) − t(spawn door
// began the MicroVM branch). It covers exactly the work gardenio owns and can
// regress: per-run credential resolution (beads/NATS/slack-bridge/coopmux/git
// token minting), the two Secrets Manager writes, the broker IssueLease, and the
// RunMicrovm accept. It does NOT cover the in-VM phases (image boot, Ziti
// enroll, gb init, coop start) — those run inside the Fabric-owned MicroVM and
// land in the launcher-account CloudWatch group, not the controller.
//
// This is the durable, in-process answer to "what is MicroVM spin-up now?" for
// the controller half of the launch. The end-to-end trigger→working-agent time
// (~27s cold after the coopmux-token fix, ka-0000006120 / gardenio!5687) is the
// sum of this dispatch term plus the in-VM phases; publishing the dispatch term
// as a live histogram lets a dashboard alert the moment credential-resolution
// or SM latency regresses the piece we control.
//
// Labels (env is the cross-estate join key; outcome is a HARD enum —
// RecordLaunch drops anything else so series cardinality stays bounded):
//   - outcome: "success" (handle live) | "failed" (backend.Launch errored,
//     rolled back). A failed dispatch still took wall-clock worth measuring —
//     a slow-then-failing launch (e.g. SM throttling) is exactly the regression
//     this metric must surface.
//   - project, role: the agent's project and functional role for drill-down.
//     Free-form (bounded in practice by the gardenio project/role set); an empty
//     value collapses to "unknown" so a missing label never spawns a distinct
//     empty series.
type MicrovmLaunchMetrics struct {
	// env is baked in at construction (GARDENIO_ENV), the cross-estate join key
	// every gardenio series carries.
	env string

	launchDuration *prometheus.HistogramVec
}

// Launch outcome enum — the only accepted `outcome` label values.
const (
	MicrovmLaunchSuccess = "success"
	MicrovmLaunchFailed  = "failed"
)

func microvmLaunchOutcomeValid(o string) bool {
	switch o {
	case MicrovmLaunchSuccess, MicrovmLaunchFailed:
		return true
	default:
		return false
	}
}

// NewMicrovmLaunchMetrics constructs the launch-dispatch histogram. env is
// attached to every series. Register() must be called before it is served.
//
// Buckets resolve the sub-second/low-second regime the dispatch normally lives
// in (measured ~1.2s: broker lease + 2 SM writes + RunMicrovm accept) while
// keeping a coarse tail out to a minute for the SM-throttle / broker-timeout
// regressions the metric exists to catch. Values are SECONDS (Prometheus
// convention) so rate()/histogram_quantile() compose with the rest of the
// gardenio metrics.
func NewMicrovmLaunchMetrics(env string) *MicrovmLaunchMetrics {
	return &MicrovmLaunchMetrics{
		env: env,
		launchDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "microvm_launch_duration_seconds",
			Help:      "lambda_microvm launch-dispatch latency: seconds from the controller spawn door entering the MicroVM branch to backend.Launch returning a live RunMicrovm handle. Covers the controller-owned launch work only (per-run credential resolution + Secrets Manager writes + broker IssueLease + RunMicrovm accept), NOT the in-VM boot/enroll/gb-init/coop phases. Labels {outcome,project,role,env}. The controller half of MicroVM spin-up (thread ka-0000003938).",
			// 0.25s..60s: dense in the ~1s dispatch regime, coarse tail for
			// SM-throttle / broker-timeout regressions.
			Buckets: []float64{0.25, 0.5, 0.75, 1, 1.5, 2, 3, 5, 10, 30, 60},
		}, []string{"outcome", "project", "role", "env"}),
	}
}

// Register registers the launch-dispatch histogram with reg. Served alongside
// the other event metrics on the controller :9092 /metrics endpoint.
func (m *MicrovmLaunchMetrics) Register(reg prometheus.Registerer) error {
	if m == nil {
		return nil
	}
	return reg.Register(m.launchDuration)
}

// RecordLaunch observes one launch dispatch in seconds. A negative duration
// (clock skew) or an out-of-enum outcome is DROPPED (returns false) so the
// histogram's cardinality and correctness stay bounded — a bad label never
// inflates series nor poisons a percentile. Empty project/role collapse to
// "unknown". A nil receiver (metric not constructed / MicroVM off) is a no-op.
func (m *MicrovmLaunchMetrics) RecordLaunch(seconds float64, outcome, project, role string) bool {
	if m == nil {
		return false
	}
	if seconds < 0 {
		return false
	}
	if !microvmLaunchOutcomeValid(outcome) {
		return false
	}
	if project == "" {
		project = "unknown"
	}
	if role == "" {
		role = "unknown"
	}
	m.launchDuration.WithLabelValues(outcome, project, role, m.env).Observe(seconds)
	return true
}
