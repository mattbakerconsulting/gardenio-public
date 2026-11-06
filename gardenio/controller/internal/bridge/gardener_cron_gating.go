// Package bridge — gardener CRON-PREFLIGHT-GATING verdict (ka-0000006215).
//
// The gardener effectiveness rubric (gardenio/docs/gardener-effectiveness-rubric.md)
// scores a gardener's compute economy (D3) and loop hygiene partly on whether its
// scheduled (cron) wake-ups are GATED by a world-state preflight — a gardener that
// fires an unconditional hourly cron into an empty epic burns runs on empty
// `gb stop` calls and should be tuned. But WHERE that gate lives depends on the
// trigger's SHAPE, and the meta-scorer kept reading the wrong field:
//
//   - LEGACY trigger:schedule gardeners carry a flat top-level `conditions` JSON
//     array (scheduler.Condition), evaluated by trigger_source_schedule.go before
//     the cron fires.
//   - trigger:multi gardeners carry a `triggers` JSON array; the cron floor lives
//     in ONE entry (kind=="cron") whose per-entry `preflight`
//     (conditions.Expression) is the engine-enforced fail-closed gate
//     (scheduler/triggers.go, cronPreflightHolds). On a multi trigger the
//     top-level `conditions` field is INERT — trigger_source_schedule.go is the
//     ONLY reader of it, and it never claims a trigger:multi bead.
//
// The bug this file fixes (ka-0000006215): the 2026-07-19 meta-scoring window
// graded a trigger:multi gardener (ka-0000001347) a C for "no preflight gate /
// fires every hour into empty gb stop calls" and filed a tune-task — TWICE more
// as duplicates in a later window — because it read the vestigial top-level
// `conditions=[]` (empty, because the gardener is multi-shaped) instead of the
// per-entry `triggers[].preflight`, which already carried the exact open-children
// bead_query gate the tune suggested. Both premises were wrong: the gate was
// present AND engine-enforced, and the fire audit showed runs_spawned=1 since
// 2026-06-11 (zero idle hourly spawns).
//
// The remedy mirrors D6 (gardener_d6.go, ka-0000000452): make the fact
// MACHINE-COMPUTABLE off the same bead the engine reads, so no two meta runs can
// disagree and the scorer never eyeballs the wrong field again. This function
// answers, deterministically and shape-aware: "are this gardener's cron wake-ups
// gated by a world-state preflight, and if not, is that a real gap or does it
// simply have no cron floor to gate?" `gb gardener status` renders it; the meta
// scorer reads `.cron_gating` from --json instead of hand-inspecting
// `conditions` / `triggers`.
//
// PURELY ADVISORY, like D6 and the self-block axes: nothing here changes the
// engine's fire-time behavior. The engine's own gate (cronPreflightHolds) is the
// authority; this is the scorer-facing readout of it.
package bridge

import (
	"strconv"
	"strings"

	"gardenio/controller/internal/scheduler"
)

// CronGatingVerdict is the shape-aware answer to "are this gardener's scheduled
// (cron) wakes gated by a world-state preflight?" It is computed off the trigger
// bead's own fields (the `triggers` array for multi, or flat `conditions`/`cron`
// for legacy schedule) so the meta-gardener scores gating from the SAME data the
// engine gates on, per whichever shape the gardener actually is.
type CronGatingVerdict struct {
	// Shape is the trigger shape the gating was evaluated under: "multi" (a
	// non-empty triggers[] array — the authoritative modern shape), "schedule"
	// (legacy flat cron + conditions), or "none" (no cron floor at all — a pure
	// event-wake gardener).
	Shape string `json:"shape"`

	// HasCron is whether the gardener has ANY scheduled (cron) wake at all. When
	// false there is no cron floor to gate — a pure event-wake gardener fires
	// only on real events, which is inherently gated, so Gated is true and this
	// is NOT a tuning gap.
	HasCron bool `json:"has_cron"`

	// Gated is the bottom line the scorer keys on: true when every cron wake is
	// covered by a world-state preflight (or there is no cron wake to gate),
	// false when at least one cron floor fires unconditionally. A false here is
	// the ONLY shape in which "no preflight gate / fires into empty gb stop
	// calls" is a legitimate D3/tuning finding.
	Gated bool `json:"gated"`

	// GatedCronEntries / TotalCronEntries count, for a multi gardener, how many of
	// its cron entries carry a per-entry preflight. For a legacy schedule gardener
	// with a cron they are 1/1 (gated) or 0/1 (ungated); for a pure event gardener
	// both are 0. Surfaced so a partially-gated multi gardener (rare) is visible
	// rather than collapsed to a single bool.
	GatedCronEntries int `json:"gated_cron_entries"`
	TotalCronEntries int `json:"total_cron_entries"`

	// Reason is a one-line human-readable explanation naming the field that was
	// read and the verdict — so a scorer (or operator) sees exactly why the
	// gardener is/ isn't considered gated and never re-inspects the wrong field.
	Reason string `json:"reason"`
}

// ScoreGardenerCronGating computes the cron-preflight-gating verdict for a gardener
// trigger bead from its raw field values, dispatching on the trigger's SHAPE.
//
//   - triggersRaw is the bead's `triggers` field (the multi array); when it
//     parses to a non-empty entry list the gardener is trigger:multi and gating is
//     read PER-ENTRY from triggers[].preflight — the top-level cron/conditions
//     fields are inert and are NOT consulted.
//   - cron / conditionsRaw are the legacy trigger:schedule flat fields, consulted
//     ONLY when triggers[] is empty.
//
// This mirrors the engine's own dispatch: the MultiTriggerSource claims any bead
// with a non-empty triggers[] and the ScheduleTriggerSource claims the rest, so
// scoring the same way guarantees the verdict matches which gate the engine
// actually enforces.
func ScoreGardenerCronGating(triggersRaw, cron, conditionsRaw string) CronGatingVerdict {
	if entries, err := scheduler.ParseTriggers(triggersRaw); err == nil && len(entries) > 0 {
		return cronGatingFromMultiEntries(entries)
	}
	return cronGatingFromLegacySchedule(cron, conditionsRaw)
}

// cronGatingFromMultiEntries scores gating for a trigger:multi gardener: it walks
// the triggers[] array, and for every cron entry checks whether that entry
// carries a per-entry preflight (the engine's fail-closed gate at
// scheduler/triggers.go). Non-cron entries (bead_event / decision_resolved /
// manual) are event-driven and carry no cron floor to gate, so they do not count
// against the verdict.
func cronGatingFromMultiEntries(entries []scheduler.TriggerEntry) CronGatingVerdict {
	v := CronGatingVerdict{Shape: "multi"}
	for i := range entries {
		if !isCronEntry(entries[i]) {
			continue
		}
		v.HasCron = true
		v.TotalCronEntries++
		if entries[i].Preflight != nil {
			v.GatedCronEntries++
		}
	}
	if !v.HasCron {
		// A multi gardener with no cron entry is pure event-wake — nothing fires
		// on a wall-clock floor, so there is no ungated cron to tune.
		v.Gated = true
		v.Reason = "trigger:multi with no cron entry — pure event-wake, no cron floor to gate"
		return v
	}
	v.Gated = v.GatedCronEntries == v.TotalCronEntries
	if v.Gated {
		v.Reason = cronGatingCountReason(
			"trigger:multi cron entries carry a per-entry preflight (triggers[].preflight, engine-enforced fail-closed)",
			v.GatedCronEntries, v.TotalCronEntries)
	} else {
		v.Reason = cronGatingCountReason(
			"trigger:multi cron entries carry a per-entry preflight; the rest fire unconditionally — score gating from triggers[].preflight, NOT the inert top-level conditions field",
			v.GatedCronEntries, v.TotalCronEntries)
	}
	return v
}

// cronGatingFromLegacySchedule scores gating for a legacy trigger:schedule
// gardener: the flat top-level `cron` field is the floor and the flat
// `conditions` JSON array (scheduler.Condition) is its preflight, exactly as
// trigger_source_schedule.go evaluates it.
func cronGatingFromLegacySchedule(cron, conditionsRaw string) CronGatingVerdict {
	v := CronGatingVerdict{Shape: "schedule"}
	if strings.TrimSpace(cron) == "" {
		// No cron floor at all — this legacy bead is either misconfigured or a
		// pure event-wake schedule. Either way there is no cron to gate.
		v.Shape = "none"
		v.Gated = true
		v.Reason = "no cron field and no triggers[] entries — no scheduled wake to gate"
		return v
	}
	v.HasCron = true
	v.TotalCronEntries = 1
	conds, err := scheduler.ParseConditions(conditionsRaw)
	if err == nil && len(conds) > 0 {
		v.GatedCronEntries = 1
		v.Gated = true
		v.Reason = "legacy trigger:schedule cron gated by a top-level conditions[] preflight"
		return v
	}
	v.Reason = "legacy trigger:schedule cron with no top-level conditions[] preflight — fires unconditionally on cron"
	return v
}

// isCronEntry reports whether a trigger entry is a cron floor. It matches the
// engine's own dispatch (scheduler/triggers.go switches on entry.Kind ==
// TriggerKindCron), and also treats an entry with a non-empty Cron expression as
// cron-shaped defensively, so an entry that carries a cron string but an
// unexpected/empty Kind is still counted rather than silently ignored.
func isCronEntry(e scheduler.TriggerEntry) bool {
	if e.Kind == scheduler.TriggerKindCron {
		return true
	}
	return e.Kind == "" && strings.TrimSpace(e.Cron) != ""
}

// cronGatingCountReason appends the "(N/M cron entries gated)" tally to a base
// reason so the readout names both the verdict and the exact count.
func cronGatingCountReason(base string, gated, total int) string {
	return base + " (" + strconv.Itoa(gated) + "/" + strconv.Itoa(total) + " cron entries gated)"
}
