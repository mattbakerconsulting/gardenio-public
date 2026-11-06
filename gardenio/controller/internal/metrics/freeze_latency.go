package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

// FreezeMetrics holds the freeze-latency histogram emitted from the controller
// freeze (checkpoint / put-to-sleep) path (Phase 6 fast+interruptible freeze,
// epic ka-0000008235, task ka-0000005147). It is the freeze-side twin of
// ResumeMetrics (resume_latency.go, ka-0000003577): the thaw proof measured the
// WAKE path; this measures the FREEZE path.
//
// freeze latency = t(checkpoint image durably persisted at snap.ImagePath +
// Snapshot recorded) − t(freeze requested). The controller freeze path stamps t0
// the moment it accepts a freeze request (FreezeThawManager.Freeze enter) and
// observes the elapsed wall-clock the moment the Snapshot is persisted — that
// persist is the first point at which a later spawn can thaw, so it IS the
// freeze's proof-of-durability (fast-interruptible-freeze.md §C1).
//
// PER-STEP ATTRIBUTION (the C1 reason-to-exist). The plan does NOT pre-commit to
// which step dominates — the ~3.0 G checkpoint image is the obvious suspect, but
// the Step-3 relocation copy must be attributed INDEPENDENTLY before C3 optimizes
// (fast-interruptible-freeze.md "Where the time goes today"). So this histogram
// is emitted once per step via the `step` label, plus a "total" roll-up:
//   - runsc-checkpoint: the `runsc checkpoint --leave-running --image-path=$IMG`
//     page-dump + 3.0 G image write (freeze node script [2/8]).
//   - relocation:       the Step-3 rootfs/bundle relocation copy that survives Pod
//     A teardown (freeze node script [3/8]) — the cp -a of each container's merged
//     rootfs into the controller-owned safe lowerdir. Attributed independently of
//     the checkpoint so C3 can decide whether eliminating the copy is worthwhile.
//   - persist:          persisting the Snapshot to the SnapshotStore (Go side).
//   - total:            the whole Freeze call, the headline number bench-freeze.sh
//     gates its p50/p95 convergence on.
//
// The per-step sub-timers sum to slightly less than total (control-plane glue —
// pod Get, sentinel parse, spec decode — is the small remainder); that gap is the
// "ms (control-plane)" Step 0/2/4 rows in the plan and is intentionally left
// un-bucketed rather than fabricated into a step.
//
// This is the dashboard-facing twin of bench-freeze.sh's per-run CSV log: the
// harness writes one row per freeze for offline convergence analysis; this
// histogram feeds the live Prometheus dashboard with the same samples bucketed.
//
// Labels (all HARD enums — RecordFreezeLatency drops anything else so series
// cardinality stays bounded):
//   - step:    runsc-checkpoint | relocation | persist | total (above).
//   - role:    the agent's functional role (crew/gardener/…); free-form is NOT
//     allowed — an empty role collapses to "unknown" via the caller.
//   - runtime: "runsc" (gVisor). The freeze path is runsc-only (shouldFreeze gates
//     on RuntimeClassRunsc), but the label is kept for parity with the resume
//     histogram and so a future runc checkpoint path can join the same series.
type FreezeMetrics struct {
	// env is baked in at construction (GARDENIO_ENV), the cross-estate join key
	// every gardenio series carries.
	env string

	freezeLatency *prometheus.HistogramVec
	// freezeInterrupts counts freezes aborted by an inbound-message interrupt
	// (C2, task ka-0000005834): an abort is NOT a freeze latency sample (the
	// freeze never completed), so it gets its own counter rather than polluting
	// the latency histogram. Labels {role,runtime,env}. The dashboard reads this
	// as "how often did the <10s safety net fire" — a high rate means freezes are
	// slow enough that messages routinely catch them mid-flight (a C3 signal).
	freezeInterrupts *prometheus.CounterVec

	// payloadIncomplete counts freezes that published a snapshot with a GENUINELY
	// INCOMPLETE workspace payload (ka-0000006933 finding #4) — a real ENOSPC/EIO/
	// short-write on a /work or /tmp warm copy, a genuine .gofer copytree failure,
	// or corrupt completeness telemetry. Labels {reason,role,runtime,env},
	// reason=work_copy|tmp_copy|gofer|gofer_malformed. This is the ALERT metric: a
	// non-zero rate() is a real wasted-freeze signal (the affected mount cold-starts
	// at thaw). It DELIBERATELY excludes the benign dangling-symlink noise (that is
	// payloadSymlinkSkip below), so a naive `rate(...) > 0` alert never pages on a
	// stale node_modules/.bin link. The freeze is published either way (WARN-not-
	// gate, invariant I1); this quantifies the wasted freeze.
	payloadIncomplete *prometheus.CounterVec

	// payloadSymlinkSkip counts freezes whose /work or /tmp warm copy tripped a
	// non-zero cp rc SOLELY because cp -aL skipped a benign DANGLING symlink
	// (ka-0000006933 finding #1) — the payload is COMPLETE. Labels {leg,role,
	// runtime,env}, leg=work|tmp. This is the quantified noise: pre-fix it was
	// indistinguishable from a real failure and drowned the alert signal. The
	// noise-vs-real ratio is sum(rate(payload_symlink_skip)) /
	// sum(rate(payload_incomplete{reason=~"work_copy|tmp_copy"})).
	payloadSymlinkSkip *prometheus.CounterVec
}

// Freeze payload-incomplete reason enum — the only accepted `reason` label values
// for the payload_incomplete counter (ka-0000006933). Bounded like freezeStep so a
// caller typo can never fork a series. Note "symlink" is DELIBERATELY not a value
// here — a benign dangling-symlink skip is a COMPLETE payload and goes to the
// separate payload_symlink_skip counter, so `rate(payload_incomplete) > 0` is
// always a real wasted-freeze signal.
const (
	// FreezePayloadReasonWorkCopy: a genuine /work warm-copy truncation.
	FreezePayloadReasonWorkCopy = "work_copy"
	// FreezePayloadReasonTmpCopy: a genuine /tmp warm-copy truncation.
	FreezePayloadReasonTmpCopy = "tmp_copy"
	// FreezePayloadReasonGofer: >=1 .gofer sweep mount's copytree failed.
	FreezePayloadReasonGofer = "gofer"
	// FreezePayloadReasonGoferMalformed: the FT_GOFER_FAILED sentinel was present
	// but unparseable (corrupt completeness telemetry).
	FreezePayloadReasonGoferMalformed = "gofer_malformed"
)

func freezePayloadReasonValid(r string) bool {
	switch r {
	case FreezePayloadReasonWorkCopy, FreezePayloadReasonTmpCopy,
		FreezePayloadReasonGofer, FreezePayloadReasonGoferMalformed:
		return true
	default:
		return false
	}
}

// Freeze payload symlink-skip leg enum — the only accepted `leg` label values for
// the payload_symlink_skip counter (ka-0000006933).
const (
	// FreezePayloadLegWork: the /work warm copy.
	FreezePayloadLegWork = "work"
	// FreezePayloadLegTmp: the /tmp warm copy.
	FreezePayloadLegTmp = "tmp"
)

func freezePayloadLegValid(l string) bool {
	switch l {
	case FreezePayloadLegWork, FreezePayloadLegTmp:
		return true
	default:
		return false
	}
}

// Freeze step enum — the per-step attribution buckets. Only these values are
// accepted as the `step` label.
const (
	// FreezeStepCheckpoint is the runsc page-dump + 3.0 G image write.
	FreezeStepCheckpoint = "runsc-checkpoint"
	// FreezeStepRelocation is the Step-3 rootfs/bundle relocation copy
	// (ka-0000003702), attributed independently of the checkpoint.
	FreezeStepRelocation = "relocation"
	// FreezeStepPersist is persisting the Snapshot to the SnapshotStore.
	FreezeStepPersist = "persist"
	// FreezeStepTotal is the whole Freeze call (the headline freeze latency).
	FreezeStepTotal = "total"
)

func freezeStepValid(s string) bool {
	switch s {
	case FreezeStepCheckpoint, FreezeStepRelocation, FreezeStepPersist, FreezeStepTotal:
		return true
	default:
		return false
	}
}

// Freeze runtime enum — the only accepted `runtime` label values. The freeze
// path is runsc-only today; runc is reserved for a future checkpoint path.
const (
	FreezeRuntimeRunsc = "runsc"
	FreezeRuntimeRunc  = "runc"
)

func freezeRuntimeValid(rt string) bool {
	switch rt {
	case FreezeRuntimeRunsc, FreezeRuntimeRunc:
		return true
	default:
		return false
	}
}

// NewFreezeMetrics constructs the freeze-latency histogram. env is attached to
// every series as the cross-estate join key. Register() must be called before
// the metric is served.
//
// Buckets are chosen for the freeze distribution, which is SECONDS-to-tens-of-
// seconds (a 3.0 G page-dump + image write), NOT the sub-second resume regime:
// dense 0.25..2s for a fast checkpoint, a 3..15s band where a real 3.0 G freeze
// is expected to land, then a coarse tail to 120s for a slow/contended node and
// the RUNSC_TIMEOUT (120s) ceiling. Values are SECONDS (Prometheus convention)
// so rate()/histogram_quantile() compose with the rest of the gardenio metrics —
// callers pass a duration in seconds. bench-freeze.sh's emit_prometheus builds
// the same buckets so harness-driven and production samples share buckets.
func NewFreezeMetrics(env string) *FreezeMetrics {
	return &FreezeMetrics{
		env: env,
		freezeLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "freeze_latency_seconds",
			Help:      "Agent freeze latency: seconds from freeze requested to checkpoint image durably persisted + Snapshot recorded. Labels {step,role,runtime,env}; step=runsc-checkpoint|relocation|persist|total breaks out per-step attribution (the ~3.0G image write vs the Step-3 relocation copy vs persist) so C3 optimization is data-driven (Phase 6 epic ka-0000008235, task ka-0000005147). bench-freeze.sh writes the same samples per-run to CSV for trailing-window convergence analysis.",
			// 0.25s..2s dense (fast checkpoint), 3..15s band (3.0G freeze lands
			// here), coarse tail to 120s (slow node + the RUNSC_TIMEOUT ceiling).
			Buckets: []float64{0.25, 0.5, 0.75, 1, 1.5, 2, 3, 5, 7.5, 10, 15, 20, 30, 60, 120},
		}, []string{"step", "role", "runtime", "env"}),
		freezeInterrupts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "freeze_interrupts_total",
			Help:      "Count of freezes aborted by an inbound-message interrupt (C2 interruptible freeze, task ka-0000005834): the agent was left live (`runsc checkpoint --leave-running`) to serve the message instead of being checkpointed+torn down. Labels {role,runtime,env}. A high rate means freezes are slow enough that user messages routinely catch them mid-flight — a C3 (freeze-speed) optimization signal.",
		}, []string{"role", "runtime", "env"}),
		payloadIncomplete: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "freeze_payload_incomplete_total",
			Help:      "Count of freezes that published a snapshot with a GENUINELY incomplete workspace payload (ka-0000006933): a real ENOSPC/EIO/short-write on the /work or /tmp warm copy, a genuine .gofer copytree failure, or corrupt completeness telemetry. Labels {reason,role,runtime,env}; reason=work_copy|tmp_copy|gofer|gofer_malformed. The affected mount cold-starts at thaw (WARN-not-gate; the freeze is still published). ALERT metric: a non-zero rate() is a real wasted-freeze signal. DELIBERATELY excludes benign dangling-symlink noise (see freeze_payload_symlink_skip_total), so `rate() > 0` never pages on a stale node_modules/.bin link. Noise-vs-real ratio: sum(rate(freeze_payload_symlink_skip_total)) / sum(rate(freeze_payload_incomplete_total{reason=~\"work_copy|tmp_copy\"})); true incomplete fraction: sum(rate(freeze_payload_incomplete_total)) / sum(rate(freeze_latency_seconds_count{step=\"total\"})).",
		}, []string{"reason", "role", "runtime", "env"}),
		payloadSymlinkSkip: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "freeze_payload_symlink_skip_total",
			Help:      "Count of freezes whose /work or /tmp warm copy tripped a non-zero cp -aL rc SOLELY because cp skipped a benign DANGLING symlink (ka-0000006933) — the payload is COMPLETE. Labels {leg,role,runtime,env}; leg=work|tmp. This is the quantified benign noise that, pre-fix, was indistinguishable from a real failure and drowned the freeze_payload_incomplete_total signal at fleet scale.",
		}, []string{"leg", "role", "runtime", "env"}),
	}
}

// Register registers the freeze-latency histogram with reg. Served alongside the
// other event metrics on the controller :9092 /metrics endpoint.
func (f *FreezeMetrics) Register(reg prometheus.Registerer) error {
	if f == nil {
		return nil
	}
	if err := reg.Register(f.freezeLatency); err != nil {
		return err
	}
	if err := reg.Register(f.freezeInterrupts); err != nil {
		return err
	}
	if err := reg.Register(f.payloadIncomplete); err != nil {
		return err
	}
	return reg.Register(f.payloadSymlinkSkip)
}

// RecordFreezeLatency observes one freeze step in seconds. A negative duration
// (clock skew) or any out-of-enum label is DROPPED (returns false) so the
// histogram's cardinality and correctness stay bounded — a bad label never
// inflates series nor poisons a percentile. A nil receiver (metric not
// constructed) is a no-op.
func (f *FreezeMetrics) RecordFreezeLatency(seconds float64, step, role, runtime string) bool {
	if f == nil {
		return false
	}
	if seconds < 0 {
		return false
	}
	if !freezeStepValid(step) || !freezeRuntimeValid(runtime) {
		return false
	}
	if role == "" {
		role = "unknown"
	}
	f.freezeLatency.WithLabelValues(step, role, runtime, f.env).Observe(seconds)
	return true
}

// RecordFreezeInterrupt counts one freeze aborted by an inbound-message
// interrupt (C2, task ka-0000005834). An out-of-enum runtime is DROPPED (returns
// false) to keep cardinality bounded; an empty role collapses to "unknown". A
// nil receiver is a no-op.
func (f *FreezeMetrics) RecordFreezeInterrupt(role, runtime string) bool {
	if f == nil {
		return false
	}
	if !freezeRuntimeValid(runtime) {
		return false
	}
	if role == "" {
		role = "unknown"
	}
	f.freezeInterrupts.WithLabelValues(role, runtime, f.env).Inc()
	return true
}

// RecordFreezePayloadIncomplete counts one freeze that published a genuinely
// incomplete workspace payload (ka-0000006933). reason must be one of the
// FreezePayloadReason* enum (work_copy|tmp_copy|gofer|gofer_malformed);
// an out-of-enum reason or runtime is DROPPED (returns false) to keep cardinality
// bounded, mirroring RecordFreezeLatency. An empty role collapses to "unknown". A
// nil receiver is a no-op. Call ONCE per genuine incomplete leg/reason on a
// successfully-persisted freeze (caller-side, so a discarded snapshot is never
// counted).
func (f *FreezeMetrics) RecordFreezePayloadIncomplete(reason, role, runtime string) bool {
	if f == nil {
		return false
	}
	if !freezePayloadReasonValid(reason) || !freezeRuntimeValid(runtime) {
		return false
	}
	if role == "" {
		role = "unknown"
	}
	f.payloadIncomplete.WithLabelValues(reason, role, runtime, f.env).Inc()
	return true
}

// RecordFreezePayloadSymlinkSkip counts one freeze whose /work or /tmp warm copy
// tripped a non-zero cp rc SOLELY due to a benign dangling symlink (ka-0000006933)
// — the payload is COMPLETE, this is the quantified noise. leg must be one of the
// FreezePayloadLeg* enum (work|tmp); an out-of-enum leg or runtime is DROPPED
// (returns false). An empty role collapses to "unknown". A nil receiver is a
// no-op.
func (f *FreezeMetrics) RecordFreezePayloadSymlinkSkip(leg, role, runtime string) bool {
	if f == nil {
		return false
	}
	if !freezePayloadLegValid(leg) || !freezeRuntimeValid(runtime) {
		return false
	}
	if role == "" {
		role = "unknown"
	}
	f.payloadSymlinkSkip.WithLabelValues(leg, role, runtime, f.env).Inc()
	return true
}
