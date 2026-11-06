package main

import (
	"os"
	"strings"
)

// defaultProject returns KD_PROJECT > BOAT_PROJECT > "".
func defaultProject() string {
	if p := os.Getenv("KD_PROJECT"); p != "" {
		return p
	}
	return os.Getenv("BOAT_PROJECT")
}

// issueTypeNames is the canonical set of issue-kind type names. Mirrors
// kbeads/internal/server/config.go (the issue-kind defaults). Used by
// `kd list` to decide when the implicit project-label filter is safe to
// apply: project labels exist on issue beads but not necessarily on
// infrastructure (project, config, agent) or data (formula, molecule)
// beads, so the filter must skip queries that could include them.
var issueTypeNames = map[string]struct{}{
	"task":    {},
	"bug":     {},
	"epic":    {},
	"feature": {},
	"chore":   {},
}

// isIssueOnlyQuery reports whether a `kd list` query constrained by the
// given beadType + kind filters will return only issue-kind beads.
// Returns true when:
//   - kind is exactly ["issue"] (or several "issue" entries), AND
//   - every entry in beadType is an issue-kind type (or beadType is empty)
//
// An unconstrained query (both empty) returns false because it would
// include infrastructure beads. The caller is expected to inject
// kind=["issue"] when in default mode before calling this.
func isIssueOnlyQuery(beadType, kind []string) bool {
	if len(kind) == 0 && len(beadType) == 0 {
		return false
	}
	for _, k := range kind {
		if k != "issue" {
			return false
		}
	}
	for _, t := range beadType {
		if _, ok := issueTypeNames[t]; !ok {
			return false
		}
	}
	return true
}

// projectScopeDescriptor returns the human-footer note describing whether the
// implicit project-label filter narrowed a `kd list` query. When applied it
// names the project and points at the opt-out flag; otherwise it states the
// query spanned all projects. Making this visible closes the silent-scope gap
// where `kd list --type=task` (scoped) and `kd list --type=config` (unscoped)
// printed identical footers over different corpora (ka-0000007254).
func projectScopeDescriptor(applied bool, project string) string {
	if applied {
		return "project=" + project + " — --all-projects to widen"
	}
	return "all projects"
}

// agentProject returns the agent's project name from KD_PROJECT, BOAT_PROJECT,
// or by parsing the first component of BEADS_AGENT_NAME (e.g., "gardenio/gb-zeta" → "gardenio").
func agentProject() string {
	if p := defaultProject(); p != "" {
		return p
	}
	if name := os.Getenv("BEADS_AGENT_NAME"); name != "" {
		parts := strings.SplitN(name, "/", 2)
		if len(parts) == 2 && parts[0] != "" {
			return parts[0]
		}
	}
	return ""
}
