package main

import (
	"fmt"
	"io"
	"sort"
)

// legacyTriggerSourceFields are the single-source dispatch fields on
// type=trigger that were the only way to describe a wake-up before the A4
// `triggers:` array landed (see ka-0000000736). When `triggers:` is set on the
// same bead, these fields become advisory only — the engine prefers the
// triggers[] array and ignores them. Setting both is legal but confusing, so
// kd emits a lint warning when it sees the dual-shape combination.
var legacyTriggerSourceFields = []string{
	"source",
	"cron",
	"jql",
	"jira_project",
	"project_path",
	"event_type",
	"state",
	"gitlab_labels",
	"author",
	"target_branch",
	"pipeline_ref",
	"pipeline_ref_pattern",
	"pipeline_status",
	"webhook_token",
	"webhook_secret",
	"conditions",
}

// lintTriggerFields prints an advisory warning to w when a type=trigger bead
// is being created or updated with BOTH the `triggers:` array AND one or
// more legacy single-source fields. The warning is purely advisory: both
// shapes parse, and the engine prefers triggers[] when present. The intent
// is to nudge operators toward picking one carrier.
//
// fieldPairs is the raw `-f key=value` list passed to kd create/update.
// Returns the number of warnings emitted (0 or 1).
func lintTriggerFields(w io.Writer, beadType string, fieldPairs []string) int {
	if beadType != "trigger" {
		return 0
	}
	present := map[string]string{}
	for _, p := range fieldPairs {
		k, v, ok := splitField(p)
		if ok {
			present[k] = v
		}
	}
	if present["triggers"] == "" {
		return 0
	}

	var legacyFound []string
	for _, f := range legacyTriggerSourceFields {
		if v, ok := present[f]; ok && v != "" {
			legacyFound = append(legacyFound, f)
		}
	}
	if len(legacyFound) == 0 {
		return 0
	}
	sort.Strings(legacyFound)

	fmt.Fprintf(w,
		"warning: type=trigger sets both `triggers:` array and legacy single-source fields %v.\n"+
			"         The engine prefers `triggers:` when present and ignores the legacy fields; pick one carrier\n"+
			"         to avoid drift. See gardenio/docs/triggers.md for the canonical triggers[] schema.\n",
		legacyFound)
	return 1
}
