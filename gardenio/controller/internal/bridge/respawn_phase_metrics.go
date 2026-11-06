package bridge

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Slack-bridge cold-respawn PHASE-BREAKDOWN metrics (task ka-0000001457).
//
// The bridge already measures the end-to-end cold-respawn latency as a single
// number: gardenio_mention_cold_respawn_seconds is the wall-clock from a
// thread/DM follow-up arriving (top of respawnThreadAgent, t0) to the respawned
// agent's first proof-of-life (the SessionStart hook firing in
// HandleSessionStart). That single number told an operator a respawn took ~4
// minutes but NOT where the four minutes went — bridge-side dispatch (bead
// mint + daemon round-trips) vs pod-side boot (schedule + image pull + coop
// --resume transcript replay). Attributing that gap is the whole point of this
// task, so these series split the same t0->proof-of-life interval at the ONE
// boundary the bridge observes first-hand: the instant respawnThreadAgent
// finishes its synchronous dispatch work and arms the resume tracker
// (MarkColdResume / MarkColdRespawn). Two boundaries (t0, dispatch-done) plus
// the terminal (SessionStart) yield two disjoint intervals plus the total:
//
//   - phase=dispatch — t0 -> dispatch-done. Bridge-side "spawn latency": parse
//     the wake-up, resolve project/role, and mint (or revive) the agent bead,
//     including the transient-daemon retry loop. A slow dispatch means the
//     beads daemon is struggling (5xx/timeout/retry), NOT that the pod is slow.
//   - phase=boot — dispatch-done -> SessionStart. Pod-side "init + ready time":
//     the AgentSpawn is picked up by the controller, the pod is scheduled, the
//     image is pulled, coop replays the prior transcript (--resume), and Claude
//     boots to first proof-of-life. This is where a multi-minute image-pull or
//     an unschedulable-node stall lands.
//   - phase=total — t0 -> SessionStart. The end-to-end latency, identical to
//     gardenio_mention_cold_respawn_seconds. Emitted here too so an operator can
//     confirm dispatch + boot ≈ total on the same panel — a built-in
//     cross-check (advice ka-0000005924: measure a quantity two independent
//     ways so the two derivations must agree). A persistent total ≠
//     dispatch+boot gap is itself a bug in the instrumentation.
//
// Why the bridge cannot split "boot" finer (pod schedule vs image pull vs coop
// replay): the only intermediate signal it sees is the agent_state
// spawning->working transition, and that is written by the controller's SyncAll
// poll on a 60s cadence — laggy enough that HandleSessionStart deliberately
// self-heals a stale "spawning" the moment SessionStart proves life
// (ka-0000003479). Using that transition as a phase boundary would misattribute
// up to 60s of poll lag to coop replay, so "boot" is kept as one honest
// pod-side interval. Finer attribution needs pod-emitted spans, out of scope
// here.
//
// Registered globally against the controller's default registerer (promauto),
// matching the other bridge metrics (thread_reply_metrics.go,
// gardener_resume_metrics.go). The slack-bridge serves /metrics from
// prometheus.DefaultGatherer (cmd/slack-bridge), so these series are
// scrapeable; in the controller binary the bot never runs, so the families stay
// empty.
var (
	// gardenio_slackbridge_respawn_phase_seconds is the per-phase cold-respawn
	// latency histogram, labelled by phase (dispatch/boot/total) and role. The
	// bucket set resolves both a sub-second dispatch (bead mint) and a
	// multi-minute boot (image pull) in one family, and extends to 300s so the
	// ~4-minute respawn that motivated this task lands in a real bucket rather
	// than the +Inf overflow.
	respawnPhaseSeconds = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "gardenio_slackbridge_respawn_phase_seconds",
			Help:    "Cold-respawn latency broken down by phase, labelled by phase (dispatch=bridge enqueue->spawn decision/bead mint; boot=spawn decision->first proof-of-life, i.e. pod schedule+image pull+coop --resume replay+agent ready; total=end-to-end, == gardenio_mention_cold_respawn_seconds) and role. dispatch+boot ≈ total is a built-in cross-check (task ka-0000001457).",
			Buckets: []float64{0.5, 1, 2, 5, 10, 15, 20, 30, 45, 60, 90, 120, 150, 180, 240, 300},
		},
		[]string{"phase", "role"},
	)

	// gardenio_slackbridge_respawn_phase_slow_total counts respawns whose phase
	// exceeded its anomaly threshold (respawnPhaseSlowThreshold), labelled by
	// phase. Respawns are infrequent, so a histogram_quantile over the series
	// above is jumpy on a short window; this counter is the robust alert
	// primitive — increase(...[window]) > 0 means "a phase went anomalously
	// high recently", which reads cleanly for a rare event where a percentile
	// would not. Fulfils the task's "add Prometheus counters" + "alert if any
	// phase anomalously high".
	respawnPhaseSlowTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gardenio_slackbridge_respawn_phase_slow_total",
			Help: "Count of cold respawns whose phase latency exceeded its anomaly threshold (dispatch>15s / boot>150s / total>180s), labelled by phase. The SlackBridgeRespawnPhaseSlow alert fires on increase(...) > 0 (task ka-0000001457).",
		},
		[]string{"phase"},
	)
)

// Respawn phase-label enum — the only accepted `phase` values. recordRespawnPhase
// drops anything else so the histogram/counter cardinality stays bounded.
const (
	respawnPhaseDispatch = "dispatch" // t0 -> dispatch-done (bridge-side spawn latency)
	respawnPhaseBoot     = "boot"     // dispatch-done -> SessionStart (pod-side init + ready)
	respawnPhaseTotal    = "total"    // t0 -> SessionStart (end-to-end)
)

// respawnPhaseValid reports whether p is a member of the bounded phase enum.
func respawnPhaseValid(p string) bool {
	switch p {
	case respawnPhaseDispatch, respawnPhaseBoot, respawnPhaseTotal:
		return true
	default:
		return false
	}
}

// respawnPhaseSlowThreshold is the per-phase "anomalously high" line, in
// seconds. Crossing it bumps gardenio_slackbridge_respawn_phase_slow_total and,
// via the SlackBridgeRespawnPhaseSlow vmalert rule, pages. The values are set
// well above the healthy band so a normal respawn never trips them:
//   - dispatch: a healthy bead mint is sub-second to a couple of seconds; 15s
//     means the daemon is deep in the transient-retry loop (5xx/timeout).
//   - boot: a healthy cold boot (schedule + image pull + coop replay) runs
//     ~30-90s; 150s (2.5min) means an image-pull or unschedulable-node stall.
//   - total: 180s (3min) end-to-end — the ~4min respawn that motivated this
//     task (ka-0000001457) sits above this line, so the alert would have fired.
var respawnPhaseSlowThreshold = map[string]float64{
	respawnPhaseDispatch: 15,
	respawnPhaseBoot:     150,
	respawnPhaseTotal:    180,
}

// recordRespawnPhase observes one phase interval on
// gardenio_slackbridge_respawn_phase_seconds and, when the interval exceeds the
// phase's anomaly threshold, increments gardenio_slackbridge_respawn_phase_slow_total.
// An out-of-enum phase or a negative duration (clock skew) is DROPPED (returns
// false) so the series stay bounded and correct — mirroring
// metrics.RecordResumeLatency. An empty role collapses to "unknown".
func recordRespawnPhase(role, phase string, seconds float64) bool {
	if !respawnPhaseValid(phase) {
		return false
	}
	if seconds < 0 {
		return false
	}
	if role == "" {
		role = "unknown"
	}
	respawnPhaseSeconds.WithLabelValues(phase, role).Observe(seconds)
	if threshold, ok := respawnPhaseSlowThreshold[phase]; ok && seconds > threshold {
		respawnPhaseSlowTotal.WithLabelValues(phase).Inc()
	}
	return true
}
