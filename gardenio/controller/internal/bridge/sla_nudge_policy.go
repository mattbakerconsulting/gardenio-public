// Package bridge — SLA-by-priority nudge ladder for escalation and decision
// beads (Phase 2 of the event-gardeners design, gd-RxGnZ8ZUav).
//
// This file holds the PURE, clock-free policy: given a priority and an age,
// which rung of the 3-rung ladder (§3 of the spec) applies. Kept dependency-
// free (no BeadClient, no Slack) so ladder timing is unit-testable with plain
// durations, per the task's explicit acceptance criterion.
package bridge

import (
	"time"

	"gardenio/controller/internal/scheduler/conditions"
)

// SLARung identifies where a bead sits on the 3-rung nudge ladder relative to
// its priority's SLA.
type SLARung int

const (
	// SLARungNone means the bead has not yet crossed the rung-1 fraction of
	// its SLA — no nudge is due.
	SLARungNone SLARung = iota
	// SLARung1 — "reflag": priority bump + repost (spec §3).
	SLARung1
	// SLARung2 — "repeat on the operator channel" (spec §3).
	SLARung2
	// SLARung3 — "overdue": SLA fully elapsed, mark breached (spec §3).
	SLARung3
)

// String renders the rung as the label used in metrics and field values.
func (r SLARung) String() string {
	switch r {
	case SLARung1:
		return "rung1"
	case SLARung2:
		return "rung2"
	case SLARung3:
		return "overdue"
	default:
		return "none"
	}
}

// SLAPolicy is the (deliberately Go-struct, not policy-bead — see the design
// doc for why) configuration driving the ladder. Mirrors the shape §7 of the
// spec proposes for the `type:escalation_sla_policy` bead's per-priority
// fields, without the bead-type machinery.
type SLAPolicy struct {
	// SLAByPriority maps a bead's priority (0=P0 .. 3=P3) to its SLA window.
	// A priority with no entry, or a non-positive duration, never nudges.
	SLAByPriority map[int]time.Duration

	// Rung1Fraction / Rung2Fraction are the fractions of the SLA at which
	// rung 1 / rung 2 fire. Spec §2 default: 0.25 / 0.6.
	Rung1Fraction float64
	Rung2Fraction float64

	// QuietHours gates all rung fires (spec §4). nil disables quiet-hours
	// gating entirely. Only Timezone/Window/Days are read; Op is overridden
	// to "inside" by IsQuietHours regardless of what's set here.
	QuietHours *conditions.Leaf
}

// DefaultSLAPolicy returns the spec's §2 proposed defaults: P0=1h, P1=4h,
// P2=24h, P3=72h, rung fractions 0.25/0.6, and America/Los_Angeles
// 21:00-08:00 quiet hours (spec §4).
func DefaultSLAPolicy() SLAPolicy {
	return SLAPolicy{
		SLAByPriority: map[int]time.Duration{
			0: 1 * time.Hour,
			1: 4 * time.Hour,
			2: 24 * time.Hour,
			3: 72 * time.Hour,
		},
		Rung1Fraction: 0.25,
		Rung2Fraction: 0.6,
		QuietHours:    DefaultSLAQuietHours(),
	}
}

// DefaultSLAQuietHours returns the spec §4 default quiet-hours window:
// America/Los_Angeles, 21:00-08:00, every day.
func DefaultSLAQuietHours() *conditions.Leaf {
	return &conditions.Leaf{
		Timezone: "America/Los_Angeles",
		Window:   "21:00-08:00",
	}
}

// RungForAge returns the highest rung age has crossed for priority, per the
// SLA table in spec §2: rung1 at Rung1Fraction*SLA, rung2 at
// Rung2Fraction*SLA, rung3 (overdue) at 100% of SLA. Pure and clock-free —
// the caller supplies age.
func (p SLAPolicy) RungForAge(priority int, age time.Duration) SLARung {
	sla, ok := p.SLAByPriority[priority]
	if !ok || sla <= 0 {
		return SLARungNone
	}
	if age >= sla {
		return SLARung3
	}
	if age >= scaleDuration(sla, p.Rung2Fraction) {
		return SLARung2
	}
	if age >= scaleDuration(sla, p.Rung1Fraction) {
		return SLARung1
	}
	return SLARungNone
}

// scaleDuration returns d scaled by fraction (e.g. 24h * 0.25 = 6h).
func scaleDuration(d time.Duration, fraction float64) time.Duration {
	return time.Duration(float64(d) * fraction)
}

// IsQuietHours reports whether at falls inside the policy's quiet-hours
// window (spec §4). A nil QuietHours means no gating — always false. An
// unparseable window fails OPEN (returns false, with the error) so a
// timezone/window misconfiguration cannot silently disable the whole SLA
// nudger forever; the caller is expected to log the error.
func (p SLAPolicy) IsQuietHours(at time.Time) (bool, error) {
	if p.QuietHours == nil {
		return false, nil
	}
	leaf := *p.QuietHours
	leaf.Op = "inside"
	return conditions.EvalTimeWindowLeafAt(leaf, at)
}
