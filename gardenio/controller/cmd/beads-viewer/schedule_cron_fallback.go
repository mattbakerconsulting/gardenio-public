package main

import (
	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/scheduler"
)

// cronFromTriggers returns the cron expression and timezone of the first
// kind:cron entry in a schedule/trigger bead's `triggers` JSON-array field, or
// ("","") when the field is empty, unparseable, or carries no cron entry.
//
// WHY THIS EXISTS (task ka-0000007640). The multi-source triggers migration
// (epic ka-0000002209) moved 12 gardeners onto a `triggers:` array as the
// canonical cadence source but PRESERVED the legacy top-level `cron`/`timezone`
// fields solely because the beads-viewer read them directly. That leaves the
// viewer unable to render cadence once the deprecation pass clears the legacy
// fields. This helper is the fallback: every viewer read site prefers the
// legacy field when present (no behavior change for un-migrated beads) and
// falls back to the first cron trigger when it is empty.
//
// ONLY a kind:cron entry is consulted. An event-mode gardener (trigger:multi +
// a bead_event entry) may carry a watchdog cron entry that is deliberately NOT
// its cadence authority; the callers already classify those as event-fired
// before reaching a cron read, and returning cron only from a genuine cron
// entry keeps this helper from re-surfacing a watchdog as a clock cadence.
func cronFromTriggers(raw string) (cron, timezone string) {
	entries, err := scheduler.ParseTriggers(raw)
	if err != nil {
		return "", ""
	}
	for _, e := range entries {
		if e.Kind == scheduler.TriggerKindCron && e.Cron != "" {
			return e.Cron, e.Timezone
		}
	}
	return "", ""
}

// scheduleCronTimezone resolves the cron + timezone a viewer surface should
// display for a schedule/trigger bead: the legacy top-level fields when set,
// otherwise the first kind:cron entry in the `triggers` array. The legacy
// field always wins when present so a bead that still carries both is
// unaffected (no regression); the triggers fallback fires only once the legacy
// field is cleared. Timezone falls back independently of cron so a bead whose
// legacy cron is set but timezone was cleared still resolves its zone.
func scheduleCronTimezone(bead *beadsapi.BeadDetail) (cron, timezone string) {
	if bead == nil {
		return "", ""
	}
	f := bead.Fields
	cron = f["cron"]
	timezone = f["timezone"]
	if cron != "" && timezone != "" {
		return cron, timezone
	}
	tCron, tTZ := cronFromTriggers(f["triggers"])
	if cron == "" {
		cron = tCron
	}
	if timezone == "" {
		timezone = tTZ
	}
	return cron, timezone
}
