package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

// ResumeMetrics holds the resume-latency histogram (Phase 5 sub-second resume,
// epic ka-0000002590, task ka-0000003577). It is a shared type emitted from TWO
// processes, each covering the resume mechanisms it can observe first-hand:
//
//   - controller wake path (cmd/controller/main.go): the gVisor freeze/thaw
//     resume — path=thaw-warm (R4) / thaw-fresh (R3). "responsive" means
//     FreezeThawManager.Thaw returned, which already gates on the C3 rehydration
//     acks (gb re-credentialed + coop reconnected NATS — freezethaw_rehydrate.go),
//     so its return IS the first proof-of-life the bench plan defines
//     (sub-second-resume-integration.md §"Metric & instrumentation").
//   - slack-bridge (internal/bridge/agentstream.go): the common non-runsc COLD
//     resume — path=cold (R0). respawnThreadAgent re-mints a thread agent with
//     --resume, and the bridge already measures follow-up→first-proof-of-life for
//     it (gardenio_mention_cold_respawn_seconds); that same measurement is teed
//     here (task ka-0000004119 / C7). Before this tee the metric emitted ZERO
//     prod samples, because the controller's thaw call site is the sole emitter
//     and prod freeze/thaw is a hard-hold on the near-idle gvisor-poc canary.
//   - controller MicroVM resume path (cmd/controller/main.go handleMicrovmResume):
//     the AWS Lambda MicroVM native resume — path=microvm-resume (regime=microvm,
//     runtime=microvm). This is the native suspend/resume analog of the gVisor
//     thaw (plan ka-0000004518, step 6 ka-0000002861). Unlike a frozen-process
//     thaw, ResumeMicrovm restarts the ENTIRE in-VM userspace (the AWS `resume`
//     lifecycle hook re-reads secrets + re-enrolls Ziti + restarts the agent), so
//     "responsive" is the step-5 fail-closed PROOF-OF-LIFE: the resumed VM's own
//     egress heartbeat re-crosses the overlay to beads (AwaitRehydration returned
//     Released=true — microvm/rehydrate.go). Only a proof-confirmed resume emits;
//     a fail-closed rehydration (the VM was terminated + resume_failed) is NOT a
//     successful resume and records no latency sample, and the gate-off (step-3)
//     rollback has no proof signal to time, so it too records nothing.
//
// resume latency = t(agent responsive) − t(resume request accepted). The
// emitters are DISJOINT by construction — a frozen-awaiting-thaw respawn is
// recorded by the controller's thaw path and NOT teed by the bridge; a MicroVM
// resume is a distinct backend (path=microvm-resume) the other two never touch —
// so no wake is double-counted. A cold CreateAgentPod that is a first-spawn (not
// a resume) is not observed at all; only genuine resumes emit.
//
// The pool path (warm generic pool graft) is not yet emitted here — its hit/miss
// instrumentation is task ka-0000000550 (C8). The bench harness (bench-resume.sh)
// still writes the same-named, same-bucketed samples per-run to CSV / a
// Prometheus textfile for the E2E resume-latency gate, so all three sources
// (controller, bridge, harness) share this histogram's shape.
//
// Labels (all HARD enums — RecordResumeLatency drops anything else so series
// cardinality stays bounded):
//   - regime: R0..R4 (the cumulative gVisor benchmark regimes) or "microvm".
//     R0 = cold resume (bridge); R3 = thaw onto fresh-created Pod B; R4 = thaw
//     onto warm placeholder (both controller). "microvm" is the native
//     Lambda MicroVM resume — a DIFFERENT execution backend, not a point on the
//     gVisor R0..R4 cold→warm ladder, so it gets its own regime value rather
//     than being force-fit onto R0/R4.
//   - role:   the agent's functional role (crew/gardener/thread/…); free-form is
//     NOT allowed — unknown/empty roles collapse to "unknown" via RecordResumeLatency.
//   - runtime: "runsc" (gVisor), "runc", or "microvm". The thaw path is
//     runsc-only; the cold resume path is runc unless the resumed channel is
//     runsc-pinned; the MicroVM resume path is "microvm" (a Firecracker-class VM,
//     not an OCI runtime — kept honest rather than mislabelled runc).
//   - path:   cold | pool | thaw-fresh | thaw-warm | microvm-resume — the
//     mechanism that served the wake, matching the plan's path taxonomy.
type ResumeMetrics struct {
	// env is baked in at construction (GARDENIO_ENV), the cross-estate join key
	// every gardenio series carries.
	env string

	resumeLatency *prometheus.HistogramVec

	// microvmSuspends counts MicroVM VMs put into native standby on a graceful
	// stop (plan ka-0000004518, step 2 ka-0000007189; instrumented in step 6
	// ka-0000002861). A suspend is not a latency sample — it is a discrete
	// standby-entry event — so it gets its own counter rather than polluting the
	// resume-latency histogram. Labels {role,outcome,env}, outcome=suspended (the
	// VM was left SUSPENDED as the resume target) | fallback-terminated (the
	// suspend attempt failed and the teardown fell back to Terminate). Pairing
	// this counter with resume_latency_seconds{path="microvm-resume"} yields the
	// resume rate = resumed / suspended, the headline "does standby actually get
	// woken back up" signal for the cost model (docs/microvm-suspend-resume-cost-model.md).
	microvmSuspends *prometheus.CounterVec
}

// Resume regime enum — the cumulative gVisor benchmark regimes R0..R4, plus
// "microvm" for the native Lambda MicroVM resume (a distinct backend, not a
// point on the gVisor cold→warm ladder). Only these values are accepted as the
// `regime` label.
const (
	ResumeRegimeR0      = "R0"      // cold runsc spawn, cold node
	ResumeRegimeR1      = "R1"      // + node-image prewarm
	ResumeRegimeR2      = "R2"      // + warm generic agent-pod pool
	ResumeRegimeR3      = "R3"      // + thaw onto fresh-created Pod B
	ResumeRegimeR4      = "R4"      // + thaw onto warm placeholder Pod B (target <1s)
	ResumeRegimeMicrovm = "microvm" // native Lambda MicroVM ResumeMicrovm (step 6)
)

func resumeRegimeValid(r string) bool {
	switch r {
	case ResumeRegimeR0, ResumeRegimeR1, ResumeRegimeR2, ResumeRegimeR3, ResumeRegimeR4,
		ResumeRegimeMicrovm:
		return true
	default:
		return false
	}
}

// Resume path enum — the mechanism that served the wake. Mirrors the plan's
// path taxonomy (sub-second-resume-integration.md §"Metric & instrumentation").
const (
	ResumePathCold          = "cold"           // cold CreateAgentPod
	ResumePathPool          = "pool"           // warm generic agent-pod pool
	ResumePathThawFresh     = "thaw-fresh"     // thaw onto a fresh-created Pod B (R3)
	ResumePathThawWarm      = "thaw-warm"      // thaw onto a pre-Ready warm placeholder (R4)
	ResumePathMicrovmResume = "microvm-resume" // native Lambda MicroVM ResumeMicrovm (step 6)
)

func resumePathValid(p string) bool {
	switch p {
	case ResumePathCold, ResumePathPool, ResumePathThawFresh, ResumePathThawWarm,
		ResumePathMicrovmResume:
		return true
	default:
		return false
	}
}

// Resume runtime enum — the only accepted `runtime` label values. "microvm" is
// the native Lambda MicroVM (a Firecracker-class VM, not an OCI runtime); it is
// kept a distinct honest value rather than mislabelled as runc.
const (
	ResumeRuntimeRunsc   = "runsc"
	ResumeRuntimeRunc    = "runc"
	ResumeRuntimeMicrovm = "microvm"
)

func resumeRuntimeValid(rt string) bool {
	switch rt {
	case ResumeRuntimeRunsc, ResumeRuntimeRunc, ResumeRuntimeMicrovm:
		return true
	default:
		return false
	}
}

// MicroVM suspend-outcome enum — the only accepted `outcome` label values for
// the microvm_suspends_total counter.
const (
	// MicrovmSuspendOutcomeSuspended: the VM was left SUSPENDED as the resume
	// target (the bead stays open at agent_state=stopping).
	MicrovmSuspendOutcomeSuspended = "suspended"
	// MicrovmSuspendOutcomeFallbackTerminated: the suspend attempt failed and the
	// teardown fell back to Terminate (leak GC backstops the open bead).
	MicrovmSuspendOutcomeFallbackTerminated = "fallback-terminated"
)

func microvmSuspendOutcomeValid(o string) bool {
	switch o {
	case MicrovmSuspendOutcomeSuspended, MicrovmSuspendOutcomeFallbackTerminated:
		return true
	default:
		return false
	}
}

// NewResumeMetrics constructs the resume-latency histogram. env is attached to
// every series as the cross-estate join key. Register() must be called before
// the metric is served.
//
// Buckets are chosen to resolve the sub-second goal precisely while still
// covering the slow regimes: dense sub-second buckets (250/500/750ms, the R4
// p50<1s / p95<1.5s gate lives here), then a 1s..2s band for thaw-fresh, then a
// coarse tail out to 5 minutes for cold image-pull regimes. Values are SECONDS
// (Prometheus convention); the metric NAME is _ms-flavored in the plan but the
// histogram unit is seconds so rate()/histogram_quantile() compose with the
// rest of the gardenio metrics — callers pass a duration in seconds.
func NewResumeMetrics(env string) *ResumeMetrics {
	return &ResumeMetrics{
		env: env,
		resumeLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "resume_latency_seconds",
			Help:      "Agent resume latency: seconds from resume request accepted to agent responsive. path=thaw-warm/thaw-fresh emitted by the controller (thaw rehydration acked); path=cold emitted by the slack-bridge for a non-runsc thread-agent resume (respawn --resume -> first proof-of-life); path=microvm-resume emitted by the controller for a native Lambda MicroVM resume (regime=microvm,runtime=microvm; measured from resume-request accepted to the step-5 proof-of-life ack). Labels {regime,role,runtime,path,env}. The R4 (thaw-warm) p50<1s / p95<1.5s gate is the Phase 5 sub-second-resume success criterion (epic ka-0000002590). The bench-resume.sh harness writes the same samples per-run to CSV for overnight convergence analysis.",
			// 0.25s, 0.5s, 0.75s, 1s, 1.25s, 1.5s, 2s, 3s, 5s, 10s, 30s, 60s,
			// 120s, 300s — dense sub-second to resolve the <1s gate, coarse tail
			// for cold regimes. The microvm-resume path (whole-userspace warm-boot
			// + Ziti re-enroll + proof-of-life, default 90s budget) lands in the
			// mid-to-tail band; the existing buckets already resolve it.
			Buckets: []float64{0.25, 0.5, 0.75, 1, 1.25, 1.5, 2, 3, 5, 10, 30, 60, 120, 300},
		}, []string{"regime", "role", "runtime", "path", "env"}),
		microvmSuspends: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "microvm_suspends_total",
			Help:      "Count of MicroVM VMs put into native standby on a graceful stop (plan ka-0000004518, step 2 ka-0000007189; instrumented step 6 ka-0000002861). Labels {role,outcome,env}; outcome=suspended (VM left SUSPENDED as the resume target) | fallback-terminated (suspend attempt failed, teardown fell back to Terminate). The resume rate = rate(resume_latency_seconds_count{path=\"microvm-resume\"}) / rate(microvm_suspends_total{outcome=\"suspended\"}) is the headline 'does standby actually get resumed' signal for the cost model (docs/microvm-suspend-resume-cost-model.md).",
		}, []string{"role", "outcome", "env"}),
	}
}

// Register registers the resume-latency histogram and MicroVM suspend counter
// with reg. Registered in BOTH the controller (:9092, alongside the other event
// metrics) and the slack-bridge (default registry, served on its /metrics
// endpoint) — each process constructs its own ResumeMetrics and emits the paths
// it observes (controller: thaw-* + microvm-resume + suspends; bridge: cold).
// Both scrapes land in the same VictoriaMetrics, distinguished by the labels.
func (r *ResumeMetrics) Register(reg prometheus.Registerer) error {
	if r == nil {
		return nil
	}
	if err := reg.Register(r.resumeLatency); err != nil {
		return err
	}
	return reg.Register(r.microvmSuspends)
}

// RecordResumeLatency observes one resume in seconds. A negative duration (clock
// skew) or any out-of-enum label is DROPPED (returns false) so the histogram's
// cardinality and correctness stay bounded — a bad label never inflates series
// nor poisons a percentile. A nil receiver (metric not constructed) is a no-op.
func (r *ResumeMetrics) RecordResumeLatency(seconds float64, regime, role, runtime, path string) bool {
	if r == nil {
		return false
	}
	if seconds < 0 {
		return false
	}
	if !resumeRegimeValid(regime) || !resumePathValid(path) || !resumeRuntimeValid(runtime) {
		return false
	}
	if role == "" {
		role = "unknown"
	}
	r.resumeLatency.WithLabelValues(regime, role, runtime, path, r.env).Observe(seconds)
	return true
}

// RecordMicrovmSuspend counts one MicroVM standby-entry on a graceful stop
// (step 2 suspend-on-stop; instrumented step 6). outcome must be one of the
// MicrovmSuspendOutcome* enum (suspended | fallback-terminated); an out-of-enum
// outcome is DROPPED (returns false) to keep cardinality bounded, mirroring
// RecordResumeLatency. An empty role collapses to "unknown". A nil receiver
// (metric not constructed / MicroVM off) is a no-op so the teardown branch can
// call it unconditionally. Call ONCE per suspend decision (both the successful
// standby and the failed-then-terminated fallback are counted, so the ratio of
// the two quantifies how often standby entry itself fails).
func (r *ResumeMetrics) RecordMicrovmSuspend(role, outcome string) bool {
	if r == nil {
		return false
	}
	if !microvmSuspendOutcomeValid(outcome) {
		return false
	}
	if role == "" {
		role = "unknown"
	}
	r.microvmSuspends.WithLabelValues(role, outcome, r.env).Inc()
	return true
}
