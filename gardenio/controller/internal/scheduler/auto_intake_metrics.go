package scheduler

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Auto-intake sweep observability (ka-0000001368).
//
// Two metrics make both the sweep's action and the lane it drains graphable —
// the residual "surface needs-intake lane depth as a first-class throughput
// metric" ask carried on the feature bead. Registered globally against the
// controller's default registerer (promauto), matching the bridge metrics, so
// they are scraped by whichever binary imports the scheduler package and serves
// /metrics (trigger-engine, where the sweep runs). The scheduler package had no
// prometheus metrics before this; these are additive and label-free (bounded
// cardinality by construction).

// auto_intake_delaned_total counts beads the auto-intake sweep has promoted by
// stripping the intake label — the throughput this feature adds. A flat line
// while the lane is deep means the sweep-OUT half is not moving anything (the
// exact "groomer dark" failure this replaces); a rising line is the lane
// draining into the dispatch queue.
var autoIntakeDeLanedTotal = promauto.NewCounter(
	prometheus.CounterOpts{
		Name: "auto_intake_delaned_total",
		Help: "Total beads de-laned (needs-intake label stripped) by the auto-intake sweep so a code-ready plan child becomes dispatchable without a human intake pass.",
	},
)

// auto_intake_lane_depth gauges how many beads still carry the intake label
// after each sweep pass — the needs-intake lane's steady-state backlog. Set
// every sweep (even a no-op one) so a stuck / growing lane is visible
// independent of whether anything was de-laned. Read alongside
// auto_intake_delaned_total: depth flat + delane flat = lane wedged; depth
// falling = lane draining.
var autoIntakeLaneDepth = promauto.NewGauge(
	prometheus.GaugeOpts{
		Name: "auto_intake_lane_depth",
		Help: "Number of beads still carrying the needs-intake label after the most recent auto-intake sweep pass (the intake lane's residual backlog).",
	},
)
