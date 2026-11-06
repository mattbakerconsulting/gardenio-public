package main

// Schedule-mirror gardener detection (bug ka-0000002811).
//
// The gardenio → gardenio-next config mirror dual-writes schedule beads into the
// gardenio namespace as `type=trigger` + `role:gardener` artifacts so the foreign
// config tree stays in sync. Per advice ka-0000007027 the config mirror carries
// ONLY config (type definitions, prefixes, role contexts, decision templates) —
// it must NOT carry issues or schedules into a live operational surface. But
// because each mirror bead is a `role:gardener` trigger, loadGardenerRows swept
// them onto the /gardeners index, and handleGardenerDetail served full
// /gardener/{id} life-thread pages for them.
//
// Six such mirror beads (ka-0000007435, ka-0000006471, ka-0000005169,
// ka-0000005940, ka-0000003848, ka-0000002033) rendered as real, active gardenio
// gardeners with enormous embedded run counts (~100–440 spawned) and last
// activity 14–19d ago — dwarfing the real fleet and inflating the "active in
// last 7d" count. The embedded engine tally is a foreign-namespace snapshot, so
// the detail page also showed contradictory verdicts (verdict "0 outcomes"
// against a run_log of hundreds; index outcome "ok" vs verdict "stuck").
//
// isScheduleMirrorGardenerBead is the single chokepoint that decides whether a
// gardener bead is a foreign-namespace schedule-mirror artifact that must NOT
// appear on any live gardeners surface. It mirrors isFixtureGardenerBead
// (gardener_fixture.go) and is consulted at the same chokepoints — loadGardenerRows
// (so the mirror never lands on /gardeners or the shared /maps gardener picker)
// and handleGardenerDetail / handleMap (so a deep link 404s instead of rendering
// foreign run data).
//
// Signal robustness, strongest first. We match on ANY of three independent
// signals so a mirror can never slip through if one marker is missing, and —
// critically — so we never exclude a REAL gardener (verified against all six
// affected beads: each carries all three; real gardeners carry none):
//
//  1. The `schedule-mirror` LABEL. This is the intentional, stable marker the
//     config-mirror dual-writer stamps on every mirror bead. It is the one
//     signal the mirror author would never omit and never collides with an
//     operational gardener label.
//
//  2. A `source` FIELD of "schedule-mirror". The mirror dual-writer also sets
//     this provenance field; belt-and-suspenders in case a bead is mirrored
//     without the label.
//
//  3. A `created_by` of "static" combined with a `schedule:<foreign-id>` label.
//     Config-mirror rows are authored by the static seeder (created_by=static)
//     and carry a `schedule:` reference to their foreign source schedule. A real
//     gardener is authored by a thread/agent, never by "static". Requiring BOTH
//     the static author AND a schedule: reference keeps this signal from ever
//     matching a hand-seeded real gardener.

import (
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// scheduleMirrorLabel is the exact label the config-mirror dual-writer stamps on
// every mirrored schedule bead. The single strongest exclusion signal.
const scheduleMirrorLabel = "schedule-mirror"

// scheduleMirrorSource is the value of the `source` provenance field the mirror
// dual-writer sets ("source": "schedule-mirror").
const scheduleMirrorSource = "schedule-mirror"

// scheduleMirrorCreatedBy is the created_by handle the static config seeder uses
// for mirrored rows. On its own it is too broad, so it is only treated as a
// mirror signal in combination with a schedule: label (see signal 3).
const scheduleMirrorCreatedBy = "static"

// scheduleRefLabelPrefix is the namespaced label prefix carrying a mirror bead's
// foreign source schedule id (e.g. "schedule:ka-0000004416").
const scheduleRefLabelPrefix = "schedule:"

// isScheduleMirrorGardenerBead reports whether a gardener bead is a foreign-
// namespace config/schedule-mirror artifact that must be excluded from every
// live gardeners surface (index, detail, map). A nil bead is treated as
// not-a-mirror (callers handle nil separately). See the file header for the
// three-signal rationale.
func isScheduleMirrorGardenerBead(b *beadsapi.BeadDetail) bool {
	if b == nil {
		return false
	}
	hasScheduleRefLabel := false
	for _, l := range b.Labels {
		ll := strings.ToLower(strings.TrimSpace(l))
		if ll == scheduleMirrorLabel {
			return true
		}
		if strings.HasPrefix(ll, scheduleRefLabelPrefix) {
			hasScheduleRefLabel = true
		}
	}
	if b.Fields != nil && strings.EqualFold(strings.TrimSpace(b.Fields["source"]), scheduleMirrorSource) {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(b.CreatedBy), scheduleMirrorCreatedBy) && hasScheduleRefLabel {
		return true
	}
	return false
}
