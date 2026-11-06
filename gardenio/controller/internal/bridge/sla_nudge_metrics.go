package bridge

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// slaNudgesSentTotal counts every SLA-ladder action the sweep actually fired
// (dedupe-passed, quiet-hours-clear). Labels:
//   - bead_type: "escalation" | "decision" — the two types the sweep covers.
//   - rung: SLARung.String() — "rung1" | "rung2" | "overdue". Bounded 3-value
//     enum, safe as a label.
//
// This is the task's explicit "nudges_sent" acceptance-criteria metric.
var slaNudgesSentTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "sla_nudges_sent_total",
		Help: "SLA-ladder nudges actually fired by the SLA nudge sweep, labelled by bead_type (escalation/decision) and rung (rung1/rung2/overdue).",
	},
	[]string{"bead_type", "rung"},
)

// slaOverdueCount is a live gauge of open beads currently past 100% of their
// priority's SLA (SLARung3), recomputed every sweep regardless of whether a
// NEW rung-3 nudge fired this cycle — so it reflects the standing overdue
// population, not just this sweep's deltas. This is the task's explicit
// "overdue_count" acceptance-criteria metric.
var slaOverdueCount = promauto.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "sla_overdue_count",
		Help: "Current count of open escalation/decision beads past 100% of their priority's SLA, labelled by bead_type.",
	},
	[]string{"bead_type"},
)
