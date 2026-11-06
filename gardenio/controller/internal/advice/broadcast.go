package advice

// broadcast.go implements the bare-project broadcast deny (Phase 3 of the
// context-crisis plan, ka-0000003733 / ka-0000007790). A project:-labelled
// advice bead with no role:, no agent:, and no global label reaches EVERY
// agent in that project — the "waste vector #2" broadcast leak that busted the
// 18k-token/archetype budget before the Part-A cut. To keep the cut from
// regressing silently, such beads are now dropped from delivery unless they
// carry the explicit BroadcastOptInLabel.
//
// The deny lives here as a per-bead delivery predicate (like IsFixture and
// IsHippoMirror), NOT inside MatchesSubscriptions: config beads resolve
// through the same matcher and are legitimately project-scoped without a
// role (see cmd/gb/config_resolve.go).

import (
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// BroadcastOptInLabel is the label that opts a bare-project advice bead into
// intentional project-wide broadcast. Add it ungrouped (no gN: prefix) so it
// cannot interfere with AND-group matching.
const BroadcastOptInLabel = "scope:project-broadcast"

// IsProjectBroadcast reports whether the labels target one or more whole
// projects with no narrowing: at least one real project: label (not the junk
// project:gd-… artifact), and no role:, agent:, or global label. Such
// targeting delivers to every agent of the project regardless of role.
func IsProjectBroadcast(labels []string) bool {
	hasProject := false
	for _, l := range labels {
		clean := StripGroupPrefix(l)
		switch {
		case clean == "global":
			return false
		case strings.HasPrefix(clean, "role:"):
			return false
		case strings.HasPrefix(clean, "agent:"):
			return false
		case strings.HasPrefix(clean, "project:"):
			name := strings.TrimPrefix(clean, "project:")
			if name != "" && !(strings.HasPrefix(name, "gd-") || strings.HasPrefix(name, "ka-")) {
				hasProject = true
			}
		}
	}
	return hasProject
}

// HasBroadcastOptIn reports whether the labels carry the explicit
// project-broadcast opt-in.
func HasBroadcastOptIn(labels []string) bool {
	for _, l := range labels {
		if StripGroupPrefix(l) == BroadcastOptInLabel {
			return true
		}
	}
	return false
}

// IsDeniedBroadcastLabels reports whether a label set is a bare-project
// broadcast without the explicit opt-in — the shape every advice delivery
// path drops. Exposed on raw labels so callers that only have a bead event
// (e.g. the bridge advice nudge) can apply the same predicate.
func IsDeniedBroadcastLabels(labels []string) bool {
	return IsProjectBroadcast(labels) && !HasBroadcastOptIn(labels)
}

// BroadcastProjects returns the deduplicated project names a broadcast's
// labels target (real names only — the junk project:gd-… artifact is
// skipped). Order follows first appearance in the labels.
func BroadcastProjects(labels []string) []string {
	var projects []string
	seen := map[string]bool{}
	for _, l := range labels {
		clean := StripGroupPrefix(l)
		name, ok := strings.CutPrefix(clean, "project:")
		if !ok || name == "" || (strings.HasPrefix(name, "gd-") || strings.HasPrefix(name, "ka-")) || seen[name] {
			continue
		}
		seen[name] = true
		projects = append(projects, name)
	}
	return projects
}

// IsDeniedBroadcast reports whether the advice bead is a bare-project
// broadcast without the BroadcastOptInLabel opt-in. Denied beads are dropped
// by ListAdviceForAgent, DiffAdviceForRole, AuditCatalog, the bridge advice
// nudge, and surfaced as NOT-DELIVERED by gb advice list / gb advice lint —
// the same single-predicate parity contract as IsHippoMirror.
func IsDeniedBroadcast(b *beadsapi.BeadDetail) bool {
	return IsDeniedBroadcastLabels(b.Labels)
}
