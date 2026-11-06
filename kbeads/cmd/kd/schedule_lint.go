package main

import (
	"errors"
	"fmt"
	"io"
	"sort"
)

// legacyScheduleFields are the deprecated wake-spec fields on type=schedule.
// The canonical replacement is the `triggers:` array on type=trigger.
var legacyScheduleFields = []string{
	"cron", "trigger_mode",
	"watch_bead_id", "watch_channel", "watch_from",
	"watch_labels", "watch_project", "watch_role", "watch_types",
}

// errScheduleCreateRejected is returned when a user attempts
// `kd create --type=schedule`. Phase 3b/D10 removed all firing support for
// type=schedule beads; new gardeners must be authored as type=trigger.
var errScheduleCreateRejected = errors.New(
	"type=schedule is no longer supported (Phase 3b/D10).\n" +
		"  Authoring path: kd create --type=trigger ... with gardener fields (stop / progress / terminal_goal / run_budget / max_runs)\n" +
		"  Wake spec:      use the `triggers:` array; legacy `cron` / `trigger_mode` / `watch_*` fields no longer fire\n" +
		"  Docs:           gardenio/docs/triggers.md, gardenio/docs/gardener-loop-runbook.md",
)

// rejectScheduleCreate returns errScheduleCreateRejected when beadType is
// "schedule". Called from `kd create` to hard-reject the deprecated surface
// at authoring time. Until D10, the create path emitted a deprecation lint;
// D10 made type=schedule silent in firing infra, so silently accepting it
// would create dead beads — hence the hard reject.
func rejectScheduleCreate(beadType string) error {
	if beadType == "schedule" {
		return errScheduleCreateRejected
	}
	return nil
}

// lintScheduleFields prints update-time deprecation warnings to w when an
// existing type=schedule bead is being touched with legacy wake-spec fields
// (`cron`, `trigger_mode`, `watch_*`) without a `triggers:` array in the
// same call. Create-time linting is replaced by rejectScheduleCreate.
// Returns the number of warnings emitted.
func lintScheduleFields(w io.Writer, beadType string, fieldPairs []string) int {
	if beadType != "schedule" {
		return 0
	}

	present := map[string]string{}
	for _, p := range fieldPairs {
		k, v, ok := splitField(p)
		if ok {
			present[k] = v
		}
	}

	var legacyFound []string
	for _, f := range legacyScheduleFields {
		if v, ok := present[f]; ok && v != "" {
			legacyFound = append(legacyFound, f)
		}
	}
	sort.Strings(legacyFound)
	if len(legacyFound) > 0 && present["triggers"] == "" {
		fmt.Fprintf(w,
			"lint: deprecated schedule fields %v set without a `triggers:` array.\n"+
				"      Phase 3b/D10 removed type=schedule firing; new gardeners must be authored as type=trigger.\n"+
				"      See gardenio/docs/triggers.md.\n",
			legacyFound)
		return 1
	}
	return 0
}
